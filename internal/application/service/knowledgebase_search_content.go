package service

import (
	"context"
	"fmt"

	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types"
)

// matchedContentSource fills IndexWithScore.Content (the text a hit matched,
// later SearchResult.MatchedContent) for hits whose store left it out of the
// retrieval response. Deferring keeps a retrieval response's size independent
// of chunk length (BF-02); the text is fetched only for the few hits that
// survive the MatchCount (or rerank-pool) cut.
//
// A nil *matchedContentSource means no store in the search deferred content,
// and fill is a no-op, so callers need not check.
type matchedContentSource struct {
	// byKB maps a KB ID to the store group that retrieved it, for groups
	// whose engine defers content.
	byKB map[string]*storeGroup
}

// newMatchedContentSource returns nil when no group defers content.
func newMatchedContentSource(groups []*storeGroup) *matchedContentSource {
	var byKB map[string]*storeGroup
	for _, g := range groups {
		if g == nil || g.Engine == nil || !g.Engine.DefersMatchedContent() {
			continue
		}
		if byKB == nil {
			byKB = make(map[string]*storeGroup)
		}
		for _, id := range g.KBIDs {
			byKB[id] = g
		}
	}
	if byKB == nil {
		return nil
	}
	return &matchedContentSource{byKB: byKB}
}

// fill sets Content on the chunks that lack it, asking each deferring store
// once for all of its chunks. Order and scores are untouched. A failed lookup
// only leaves MatchedContent empty — the answer text itself is read from the
// database by processSearchResults — so it is logged, not returned.
func (m *matchedContentSource) fill(ctx context.Context, chunks []*types.IndexWithScore) {
	if m == nil || len(chunks) == 0 {
		return
	}
	pending := make(map[*storeGroup][]*types.IndexWithScore)
	var order []*storeGroup
	for _, c := range chunks {
		if c == nil || c.Content != "" {
			continue
		}
		g, ok := m.byKB[c.KnowledgeBaseID]
		if !ok {
			continue
		}
		if _, seen := pending[g]; !seen {
			order = append(order, g)
		}
		pending[g] = append(pending[g], c)
	}
	for _, g := range order {
		hits := pending[g]
		ids := make([]string, 0, len(hits))
		seen := make(map[string]bool, len(hits))
		for _, c := range hits {
			if !seen[c.ChunkID] {
				seen[c.ChunkID] = true
				ids = append(ids, c.ChunkID)
			}
		}
		content, err := g.Engine.FetchMatchedContent(ctx, ids)
		if err != nil {
			logger.WarnWithFields(ctx, logger.Fields{
				"store_kind": storeKindLabel(g.StoreID),
				"kb_count":   len(g.KBIDs),
				"chunks":     len(ids),
			}, fmt.Sprintf("deferred matched content lookup failed; matched content left empty: %v", err))
			continue
		}
		for _, c := range hits {
			c.Content = content[c.ChunkID]
		}
	}
}

// logRetrievalSummary writes one line per search request with the figures of
// all its store groups: how many groups were searched, how many candidates
// came back, and the response bytes the stores reported.
func logRetrievalSummary(
	ctx context.Context, groups []*storeGroup, stats *types.RetrievalStats, candidates int, err error,
) {
	if len(groups) == 0 || stats == nil {
		return
	}
	logger.GetLogger(ctx).WithFields(logger.Fields{
		"group_count":    len(groups),
		"failed":         err != nil,
		"candidates":     candidates,
		"searches":       stats.Searches.Load(),
		"response_bytes": stats.ResponseBytes.Load(),
		"truncated_sets": stats.TruncatedSets.Load(),
	}).Info("retrieval summary")
}
