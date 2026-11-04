package service

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/primind/primind/internal/types"
)

const kbResidueDDL = `
CREATE TABLE IF NOT EXISTS knowledge_bases (
    id VARCHAR(64) PRIMARY KEY,
    tenant_id INTEGER NOT NULL DEFAULT 0,
    vector_store_id VARCHAR(36),
    deleted_at DATETIME
);
CREATE TABLE IF NOT EXISTS chunks (
    id VARCHAR(64) PRIMARY KEY,
    knowledge_base_id VARCHAR(64),
    deleted_at DATETIME
);
`

// recordingKBEnqueuer captures kb:delete enqueues; dup marks KB IDs whose
// enqueue reports an existing unique task.
type recordingKBEnqueuer struct {
	kbIDs []string
	opts  [][]asynq.Option
	dup   map[string]bool
}

func (r *recordingKBEnqueuer) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	var p types.KBDeletePayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil || task.Type() != types.TypeKBDelete {
		return nil, err
	}
	if r.dup[p.KnowledgeBaseID] {
		return nil, asynq.ErrDuplicateTask
	}
	r.kbIDs = append(r.kbIDs, p.KnowledgeBaseID)
	r.opts = append(r.opts, opts)
	return &asynq.TaskInfo{}, nil
}

// kbQueueInspector reports a kb:delete task still queued for the KBs in busy.
type kbQueueInspector struct {
	fakeTaskInspector
	busy map[string]bool
}

func (k kbQueueInspector) HasQueuedKBDeleteTasks(_ context.Context, kbID string) (bool, error) {
	return k.busy[kbID], nil
}

func insertDeletedKB(t *testing.T, db *gorm.DB, id string, deletedAt *time.Time, knowledge, chunks int) {
	t.Helper()
	storeID := "store-" + id
	require.NoError(t, db.Exec(`INSERT INTO knowledge_bases (id, tenant_id, vector_store_id, deleted_at) VALUES (?, 1, ?, ?)`,
		id, storeID, deletedAt).Error)
	for i := 0; i < knowledge; i++ {
		require.NoError(t, db.Exec(`INSERT INTO knowledges (id, knowledge_base_id) VALUES (?, ?)`,
			id+"-k"+string(rune('0'+i)), id).Error)
	}
	for i := 0; i < chunks; i++ {
		require.NoError(t, db.Exec(`INSERT INTO chunks (id, knowledge_base_id) VALUES (?, ?)`,
			id+"-c"+string(rune('0'+i)), id).Error)
	}
}

// A deleted KB whose cleanup gave up (leftover knowledge rows or chunks, old
// enough, no delete task queued) is re-enqueued exactly once; recent, clean,
// live and still-being-cleaned KBs are left alone.
func TestSweepDeletedKBResidueReenqueuesOnlyAbandonedCleanups(t *testing.T) {
	db := setupHousekeepingDB(t)
	require.NoError(t, db.Exec(kbResidueDDL).Error)
	old := time.Now().Add(-time.Hour)
	recent := time.Now().Add(-5 * time.Minute)
	insertDeletedKB(t, db, "kb-stuck", &old, 2, 4)
	insertDeletedKB(t, db, "kb-chunks-only", &old, 0, 1)
	insertDeletedKB(t, db, "kb-recent", &recent, 1, 1)
	insertDeletedKB(t, db, "kb-clean", &old, 0, 0)
	insertDeletedKB(t, db, "kb-live", nil, 1, 1)
	insertDeletedKB(t, db, "kb-busy", &old, 1, 1)

	enq := &recordingKBEnqueuer{}
	svc := NewHousekeepingService(db, nil, kbQueueInspector{busy: map[string]bool{"kb-busy": true}}, enq)
	svc.sweepDeletedKBResidue(context.Background())

	got := append([]string(nil), enq.kbIDs...)
	sort.Strings(got)
	assert.Equal(t, []string{"kb-chunks-only", "kb-stuck"}, got)
	for _, opts := range enq.opts {
		var unique bool
		for _, o := range opts {
			if o.Type() == asynq.UniqueOpt {
				unique = true
			}
		}
		assert.True(t, unique, "re-enqueued cleanup must be unique per KB")
	}
}

// A unique task already held for the KB (an earlier sweep's cleanup still
// pending) is not an error and does not enqueue a second one.
func TestSweepDeletedKBResidueSkipsDuplicate(t *testing.T) {
	db := setupHousekeepingDB(t)
	require.NoError(t, db.Exec(kbResidueDDL).Error)
	old := time.Now().Add(-time.Hour)
	insertDeletedKB(t, db, "kb-stuck", &old, 1, 1)

	enq := &recordingKBEnqueuer{dup: map[string]bool{"kb-stuck": true}}
	svc := NewHousekeepingService(db, nil, fakeTaskInspector{}, enq)
	svc.sweepDeletedKBResidue(context.Background())
	assert.Empty(t, enq.kbIDs)
}
