package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types/interfaces"
)

// deferContentEnv switches retrieval searches to leave the indexed text out of
// their responses (BF-02). Off by default; when off retrieval behaves exactly
// as before.
const deferContentEnv = "OPENSEARCH_DEFER_CONTENT"

// matchedContentBatch bounds one FetchMatchedContent request. Callers ask for
// the hits they kept (about MatchCount, or the rerank pool), so one batch is
// the usual case.
const matchedContentBatch = 200

var _ interfaces.MatchedContentFetcher = (*Repository)(nil)

func deferContentFromEnv(ctx context.Context) bool {
	raw := strings.TrimSpace(os.Getenv(deferContentEnv))
	if raw == "" {
		return false
	}
	on, err := strconv.ParseBool(raw)
	if err != nil {
		logger.GetLogger(ctx).Warnf("[OpenSearch] ignoring invalid %s=%q; content stays in retrieval responses",
			deferContentEnv, raw)
		return false
	}
	return on
}

// WithDeferredContent overrides OPENSEARCH_DEFER_CONTENT for this repository.
func WithDeferredContent(on bool) Option {
	return func(r *Repository) { r.deferContent = on }
}

// DefersMatchedContent reports whether Retrieve leaves IndexWithScore.Content
// empty for the caller to fetch with FetchMatchedContent.
func (r *Repository) DefersMatchedContent() bool { return r.deferContent }

// FetchMatchedContent returns the indexed text (title + chunk text, FAQ or
// generated-question text — whatever the hit matched) of the given chunks,
// keyed by chunk ID, from every dimension index of this repository. It reads
// the same field a non-deferred retrieval would have returned, so the result
// is identical by construction. IDs not in the index are absent.
func (r *Repository) FetchMatchedContent(ctx context.Context, chunkIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(chunkIDs))
	if len(chunkIDs) == 0 {
		return out, nil
	}
	ctx, cancel := operationContext(ctx, r.client)
	defer cancel()
	for start := 0; start < len(chunkIDs); start += matchedContentBatch {
		batch := chunkIDs[start:min(start+matchedContentBatch, len(chunkIDs))]
		body, err := json.Marshal(map[string]any{
			"size":  len(batch),
			"query": map[string]any{"ids": map[string]any{"values": batch}},
		})
		if err != nil {
			return nil, fmt.Errorf("marshal content lookup: %w", err)
		}
		hits, n, err := r.search(ctx, r.baseIndex+"_*", body, []string{"chunk_id", "content"})
		if err != nil {
			return nil, err
		}
		logger.GetLogger(ctx).WithFields(logger.Fields{
			"requested": len(batch), "hits": len(hits), "response_bytes": n,
		}).Info("[OpenSearch] matched content lookup")
		for _, h := range hits {
			if _, seen := out[h.ID]; !seen {
				out[h.ID] = h.Source.Content
			}
		}
	}
	return out, nil
}
