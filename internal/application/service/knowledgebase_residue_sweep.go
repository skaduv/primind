package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hibiken/asynq"

	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types"
)

// kbResidueSweepMinAge leaves a just-deleted knowledge base to the delete
// task enqueued by DeleteKnowledgeBase. Older ones with leftovers are those
// whose task failed for good (retries exhausted, lost enqueue) and need a
// fresh cleanup.
const kbResidueSweepMinAge = 30 * time.Minute

// kbResidueSweepBatch bounds one sweep; the next tick picks up the rest.
const kbResidueSweepBatch = 50

// kbResidueUniqueTTL keeps one re-enqueued cleanup per knowledge base for a
// task's full timeout plus margin, so repeated sweeps never stack duplicates.
const kbResidueUniqueTTL = 3 * time.Hour

// kbDeleteQueueProbe is implemented by the asynq task inspector. It is an
// optional interface so other TaskInspector implementations need no change.
type kbDeleteQueueProbe interface {
	HasQueuedKBDeleteTasks(ctx context.Context, kbID string) (bool, error)
}

type deletedKBResidue struct {
	ID            string
	TenantID      uint64
	VectorStoreID *string
}

// sweepDeletedKBResidue re-enqueues cleanup for soft-deleted knowledge bases
// that still have live knowledge rows or chunks. ProcessKBDelete keeps
// knowledge rows until their vectors are gone, so such rows mean the cleanup
// did not finish. A KB that still has a kb:delete task pending, retrying or
// running is skipped, so the same KB is never cleaned concurrently.
func (h *HousekeepingService) sweepDeletedKBResidue(ctx context.Context) {
	if h.db == nil || h.task == nil {
		return
	}
	var rows []deletedKBResidue
	err := h.db.WithContext(ctx).Raw(`
SELECT kb.id, kb.tenant_id, kb.vector_store_id
FROM knowledge_bases kb
WHERE kb.deleted_at IS NOT NULL AND kb.deleted_at < ?
  AND (EXISTS (SELECT 1 FROM knowledges k WHERE k.knowledge_base_id = kb.id AND k.deleted_at IS NULL)
    OR EXISTS (SELECT 1 FROM chunks c WHERE c.knowledge_base_id = kb.id AND c.deleted_at IS NULL))
ORDER BY kb.deleted_at
LIMIT ?`, time.Now().Add(-kbResidueSweepMinAge), kbResidueSweepBatch).Scan(&rows).Error
	if err != nil {
		logger.Warnf(ctx, "[Housekeeping] KB residue sweep query failed: %v", err)
		return
	}
	probe, _ := h.inspector.(kbDeleteQueueProbe)
	for _, kb := range rows {
		if probe != nil {
			queued, err := probe.HasQueuedKBDeleteTasks(ctx, kb.ID)
			if err != nil {
				logger.Warnf(ctx, "[Housekeeping] KB %s residue: queue probe failed, skipping: %v", kb.ID, err)
				continue
			}
			if queued {
				continue
			}
		}
		h.enqueueKBResidueCleanup(ctx, kb)
	}
}

func (h *HousekeepingService) enqueueKBResidueCleanup(ctx context.Context, kb deletedKBResidue) {
	payload := types.KBDeletePayload{
		TenantID:        kb.TenantID,
		KnowledgeBaseID: kb.ID,
		VectorStoreID:   kb.VectorStoreID,
	}
	var tenant types.Tenant
	if err := h.db.WithContext(ctx).Where("id = ?", kb.TenantID).First(&tenant).Error; err == nil {
		payload.EffectiveEngines = tenant.GetEffectiveEngines()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Warnf(ctx, "[Housekeeping] KB %s residue: marshal payload: %v", kb.ID, err)
		return
	}
	_, err = h.task.Enqueue(asynq.NewTask(types.TypeKBDelete, body),
		asynq.Queue(types.QueueMaintenance), asynq.MaxRetry(3), asynq.Timeout(2*time.Hour),
		asynq.Unique(kbResidueUniqueTTL))
	switch {
	case errors.Is(err, asynq.ErrDuplicateTask):
		return
	case err != nil:
		logger.Warnf(ctx, "[Housekeeping] KB %s residue: enqueue cleanup failed: %v", kb.ID, err)
	default:
		logger.Infof(ctx, "[Housekeeping] re-enqueued cleanup for deleted knowledge base %s with leftover data", kb.ID)
	}
}
