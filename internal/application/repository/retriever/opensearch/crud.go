package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types"
)

// additionalParams contract (verified against
// elasticsearch/structs.go:ToDBVectorEmbedding and
// keywords_vector_hybrid_indexer.go:concurrentBatchSave on
// upstream/main):
//
//	additionalParams["embedding"]     map[string][]float32  // keyed by IndexInfo.SourceID
//	additionalParams["chunk_enabled"] map[string]bool       // keyed by IndexInfo.ChunkID
//
// Both keys may be absent. Missing "embedding" → keyword-only path
// (vector field omitted from the OpenSearch document, routed to the
// dim-less keywords index). Missing "chunk_enabled" → IndexInfo's own
// IsEnabled field is used as-is.

// Save indexes a single chunk. Idempotent: _id is set to chunk_id so
// re-indexing the same chunk overwrites instead of creating a duplicate.
func (r *Repository) Save(
	ctx context.Context,
	info *types.IndexInfo,
	params map[string]any,
) error {
	ctx, cancel := operationContext(ctx, r.client)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if info == nil {
		return ErrConfigInvalid
	}
	emb := lookupEmbedding(params, info.SourceID)
	enabled := lookupChunkEnabled(params, info.ChunkID, info.IsEnabled)
	dim := len(emb)

	var targetIndex string
	if dim > 0 {
		if err := r.ensureReady(ctx, dim); err != nil {
			return err
		}
		targetIndex = r.indexAlias(dim)
	} else {
		// Keyword-only path: no embedding supplied. Route to the
		// dim-less <base>_keywords index.
		if err := r.ensureKeywordsIndex(ctx); err != nil {
			return err
		}
		targetIndex = r.keywordsIndex()
	}

	doc, err := json.Marshal(toDoc(info, emb, enabled))
	if err != nil {
		return fmt.Errorf("opensearch: marshal doc: %w", err)
	}
	req := osapi.IndexReq{
		Index:      targetIndex,
		DocumentID: info.ChunkID, // _id = chunk_id (idempotent)
		Body:       bytes.NewReader(doc),
	}
	resp, err := r.client.Index(ctx, req)
	if err != nil {
		return wrapTransport(err)
	}
	if resp != nil {
		drainAndClose(resp.Inspect().Response.Body)
	}
	return nil
}

// BatchSave NDJSON-bulk-indexes chunks. Inspects per-item errors and
// returns a typed error if ANY item fails.
//
// Preserve the existing count/estimated-size rejection, then preflight exact
// encoded sizes before building a capped NDJSON buffer. Over-cap input returns
// ErrBatchTooLarge without sending any part of this batch.
func (r *Repository) BatchSave(
	ctx context.Context,
	infos []*types.IndexInfo,
	params map[string]any,
) (retErr error) {
	if len(infos) == 0 {
		return nil
	}
	ctx, cancel := operationContext(ctx, r.client)
	defer cancel()
	started := time.Now()
	trace := &requestTrace{}
	observation := WriteObservation{Total: len(infos)}
	defer func() {
		if retErr != nil {
			var be *BulkWriteError
			if !errors.As(retErr, &be) {
				be = bulkUnconfirmed(len(infos), trace.attempts.Load() > 0, retErr)
				retErr = be
			}
			be.Result.Bytes = observation.Bytes
			be.Result.Attempts = trace.attempts.Load()
			be.Result.Duration = time.Since(started)
			observation = be.Result
		} else {
			observation.Confirmed = len(infos)
			observation.Attempts = trace.attempts.Load()
			observation.Duration = time.Since(started)
		}
		if r.writeObserver != nil {
			r.writeObserver(observation)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(infos) > maxBulkItems {
		return ErrBatchTooLarge
	}
	ctx, release, admissionErr := admitWriteOperation(ctx, r.client)
	if admissionErr != nil {
		return admissionErr
	}
	defer release()
	for _, info := range infos {
		if info == nil {
			return ErrConfigInvalid
		}
	}
	emb, dim, err := extractBatchEmbeddings(params, infos)
	if err != nil {
		return err
	}

	// Pre-marshal size + count check.
	if estimated := estimateBulkBodyBytes(len(infos), dim); estimated > 10*1024*1024 {
		return fmt.Errorf(
			"opensearch: estimated bulk body %dB exceeds 10MB cap (n=%d, dim=%d): %w",
			estimated, len(infos), dim, ErrBatchTooLarge,
		)
	}

	// Route by dim: dim==0 means the entire batch is keyword-only
	// (concurrentBatchSaveNoEmbedding path), use the dim-less index.
	var alias string
	if dim == 0 {
		alias = r.keywordsIndex()
	} else {
		alias = r.indexAlias(dim)
	}

	body, err := buildBulkBodyContext(ctx, alias, infos, emb, params)
	if err != nil {
		return err
	}
	observation.Bytes = len(body)
	if dim == 0 {
		err = r.ensureKeywordsIndex(ctx)
	} else {
		err = r.ensureReady(ctx, dim)
	}
	if err != nil {
		return err
	}
	req := osapi.BulkReq{Body: bytes.NewReader(body)}
	resp, err := r.client.Bulk(context.WithValue(ctx, requestTraceKey{}, trace), req)
	if err != nil {
		// Non-runtime clients (legacy callers / old parser tests) cannot
		// prove whether bytes were sent. Conservatively report unknown.
		if _, managed := r.client.Client.Transport.(*runtimeTransport); !managed {
			trace.attempts.Add(1)
		}
		return wrapTransport(err)
	}
	if resp == nil || resp.Inspect().Response == nil {
		return bulkUnconfirmed(len(infos), true, ErrTransport)
	}
	defer drainAndClose(resp.Inspect().Response.Body)
	// Cap response body at 64MB (bulks of 1000 items are ~1-2MB success,
	// can balloon with full per-item errors).
	ids := make([]string, len(infos))
	for i, info := range infos {
		ids[i] = info.ChunkID
	}
	return inspectBulkResponse(io.LimitReader(resp.Inspect().Response.Body, 64<<20), ids...)
}

// estimateBulkBodyBytes preserves the legacy rough NDJSON estimate. It can
// underestimate long/escaped content and floats; buildBulkBodyContext supplies
// the exact hard limit. Per-item estimate:
//   - action header `{"index":{"_index":"...","_id":"..."}}\n` ≈ 100B
//   - doc body: dim*5 (float ASCII) + ~700B metadata + content ≈ 1024B
func estimateBulkBodyBytes(n, dim int) int {
	return n * (100 + dim*5 + 1024)
}

// toDoc projects IndexInfo + embedding to the OpenSearch document
// shape. source_type is `int` (not stringified) — mapping declared as
// `integer` field for stable cross-enum-extension behavior. Omits the
// "embedding" field entirely when emb is empty (keyword-only doc, no
// k-NN graph entry).
func toDoc(info *types.IndexInfo, emb []float32, enabled bool) map[string]any {
	doc := map[string]any{
		"chunk_id":          info.ChunkID,
		"knowledge_id":      info.KnowledgeID,
		"knowledge_base_id": info.KnowledgeBaseID,
		"source_id":         info.SourceID,
		"source_type":       int(info.SourceType),
		"tag_id":            info.TagID,
		"content":           info.Content,
		"is_enabled":        enabled,
		"is_recommended":    info.IsRecommended,
	}
	if len(emb) > 0 {
		doc["embedding"] = emb
	}
	return doc
}

// lookupEmbedding returns the embedding vector for info.SourceID.
// Empty slice (not nil) indicates "no embedding provided" — caller
// MUST check len(emb) before calling ensureReady.
//
// Defense in depth: tolerates the wrong shape rather than crash. The
// service layer is responsible for getting params right; we degrade
// to keyword-only for this doc and log a warning instead of panicking
// downstream.
func lookupEmbedding(params map[string]any, sourceID string) []float32 {
	if params == nil {
		return nil
	}
	raw, ok := params["embedding"]
	if !ok {
		return nil
	}
	embMap, ok := raw.(map[string][]float32)
	if !ok {
		logger.GetLogger(context.Background()).Warnf(
			"[OpenSearch] additionalParams[\"embedding\"] is %T, want map[string][]float32", raw)
		return nil
	}
	return embMap[sourceID] // nil/empty if SourceID not in map
}

// lookupChunkEnabled overlays the chunk_enabled map on top of
// info.IsEnabled. Returns the overlay value if present, else the
// default (info.IsEnabled).
func lookupChunkEnabled(params map[string]any, chunkID string, def bool) bool {
	if params == nil {
		return def
	}
	raw, ok := params["chunk_enabled"]
	if !ok {
		return def
	}
	enMap, ok := raw.(map[string]bool)
	if !ok {
		return def
	}
	if v, present := enMap[chunkID]; present {
		return v
	}
	return def
}

// extractBatchEmbeddings projects the params["embedding"] map onto the
// info list, returning per-info embeddings (some may be empty) and the
// dimension inferred from the first non-empty entry. Returns dim=0 if
// no info has an embedding — caller routes to the keywords-only path.
//
// Defense in depth: all non-empty embeddings MUST share the same
// dimension. Mixed-dim → ErrDimensionMismatch.
func extractBatchEmbeddings(params map[string]any, infos []*types.IndexInfo) ([][]float32, int, error) {
	out := make([][]float32, len(infos))
	dim := 0
	for i, info := range infos {
		emb := lookupEmbedding(params, info.SourceID)
		out[i] = emb
		if len(emb) > 0 {
			if dim == 0 {
				dim = len(emb)
			} else if len(emb) != dim {
				return nil, 0, fmt.Errorf(
					"opensearch: embedding[%s] dim=%d != first non-empty dim=%d: %w",
					info.SourceID, len(emb), dim, ErrDimensionMismatch,
				)
			}
		}
	}
	return out, dim, nil
}

// DeleteByChunkIDList runs _delete_by_query with a terms filter on
// chunk_id. Sync only — large batches (>1000) are rejected with
// ErrBatchTooLarge; the async task path that lifts the cap arrives in
// a later change.
//
// Refresh policy: preserve the existing by-query boolean Refresh=true.
// Bulk's string refresh parameter is a different API contract.
func (r *Repository) DeleteByChunkIDList(
	ctx context.Context, chunkIDs []string, dim int, _ string,
) error {
	return r.deleteByList(ctx, chunkIDs, dim, "chunk_id")
}

func (r *Repository) DeleteBySourceIDList(
	ctx context.Context, sourceIDs []string, dim int, _ string,
) error {
	return r.deleteByList(ctx, sourceIDs, dim, "source_id")
}

func (r *Repository) DeleteByKnowledgeIDList(
	ctx context.Context, knowledgeIDs []string, dim int, _ string,
) error {
	return r.deleteByList(ctx, knowledgeIDs, dim, "knowledge_id")
}

// deleteByList factors the common cap / empty / ensureReady / dispatch
// logic out of the three DeleteBy* methods. dim==0 routes to the
// dim-less keywords index.
func (r *Repository) deleteByList(
	ctx context.Context, ids []string, dim int, field string,
) error {
	ctx, cancel := operationContext(ctx, r.client)
	defer cancel()
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > 1000 {
		return fmt.Errorf("opensearch: %s-delete batch %d > 1000 cap: %w",
			field, len(ids), ErrBatchTooLarge)
	}
	var index string
	if dim == 0 {
		if err := r.ensureKeywordsIndex(ctx); err != nil {
			return err
		}
		index = r.keywordsIndex()
	} else {
		if err := r.ensureReady(ctx, dim); err != nil {
			return err
		}
		index = r.indexAlias(dim)
	}
	return r.deleteByTerms(ctx, index, field, ids)
}

func (r *Repository) deleteByTerms(
	ctx context.Context, index, field string, values []string,
) error {
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{
			"terms": map[string]any{field: values},
		},
	})
	if err != nil {
		return fmt.Errorf("opensearch: marshal delete body: %w", err)
	}
	if err := r.refreshBeforeByQuery(ctx, index); err != nil {
		return err
	}
	refresh := true
	req := osapi.DocumentDeleteByQueryReq{
		Indices: []string{index},
		Body:    bytes.NewReader(body),
		Params:  osapi.DocumentDeleteByQueryParams{Refresh: &refresh},
	}
	resp, err := r.client.Document.DeleteByQuery(ctx, req)
	if err != nil {
		return wrapTransport(err)
	}
	if resp == nil {
		return ErrTransport
	}
	defer drainAndClose(resp.Inspect().Response.Body)
	return inspectDeleteByQueryResponse(resp.Inspect().Response.Body)
}
