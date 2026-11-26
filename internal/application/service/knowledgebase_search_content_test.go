package service

// BF-02: deferred matched content and truncated result sets, seen from the
// search layer.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	openSearchRepo "github.com/primind/primind/internal/application/repository/retriever/opensearch"
	"github.com/primind/primind/internal/application/service/retriever"
	"github.com/primind/primind/internal/types"
)

// contentEngine serves a fixed ranking. With deferContent it leaves Content
// empty and hands it out through FetchMatchedContent, like the OpenSearch
// driver with OPENSEARCH_DEFER_CONTENT=true.
type contentEngine struct {
	fakeRetrieveEngineService
	deferContent bool
	ranking      []*types.IndexWithScore

	mu      sync.Mutex
	fetched [][]string
}

func (e *contentEngine) Retrieve(_ context.Context, p types.RetrieveParams) ([]*types.RetrieveResult, error) {
	out := make([]*types.IndexWithScore, 0, len(e.ranking))
	for _, h := range e.ranking {
		if p.RetrieverType == types.KeywordsRetrieverType &&
			(h.SourceType == types.ImageSourceType || strings.HasPrefix(h.ChunkID, "long-")) {
			// Image rows have no keyword entry; the long rows are vector-only
			// so that, after RRF, the cut falls inside them.
			continue
		}
		c := *h
		if e.deferContent {
			c.Content = ""
		}
		if p.RetrieverType == types.KeywordsRetrieverType {
			c.Score = 10 * c.Score // BM25 scale; fusion is rank based
		}
		out = append(out, &c)
	}
	return []*types.RetrieveResult{{
		Results: out, RetrieverEngineType: types.OpenSearchRetrieverEngineType, RetrieverType: p.RetrieverType,
	}}, nil
}

func (e *contentEngine) DefersMatchedContent() bool { return e.deferContent }

func (e *contentEngine) FetchMatchedContent(_ context.Context, chunkIDs []string) (map[string]string, error) {
	e.mu.Lock()
	e.fetched = append(e.fetched, slices.Clone(chunkIDs))
	e.mu.Unlock()
	out := make(map[string]string, len(chunkIDs))
	for _, h := range e.ranking {
		if slices.Contains(chunkIDs, h.ChunkID) {
			out[h.ChunkID] = h.Content
		}
	}
	return out, nil
}

// bf02Ranking mixes the row kinds whose matched text differs from the chunk
// text in PG: a document chunk (title + text), a generated-question row, a
// FAQ row, an image row and long chunks.
func bf02Ranking() []*types.IndexWithScore {
	rows := []*types.IndexWithScore{
		{ChunkID: "text-1", SourceID: "text-1", KnowledgeBaseID: "kb-doc", Score: 0.92, Content: "制度汇编\n车辆管理规定"},
		{
			ChunkID: "gq-1", SourceID: types.GeneratedQuestionSourceID("gq-1", "q1"), KnowledgeBaseID: "kb-doc",
			Score: 0.90, Content: "制度汇编\n停车场如何申请车位？",
		},
		{ChunkID: "faq-1", SourceID: "faq-1", KnowledgeBaseID: "kb-faq", Score: 0.88, Content: "访客车辆怎么登记"},
		{
			ChunkID: "img-1", SourceID: "img-1", KnowledgeBaseID: "kb-doc", Score: 0.86,
			SourceType: types.ImageSourceType, Content: "停车场平面图",
		},
	}
	for i := range 8 {
		rows = append(rows, &types.IndexWithScore{
			ChunkID: fmt.Sprintf("long-%d", i), SourceID: fmt.Sprintf("long-%d", i), KnowledgeBaseID: "kb-doc",
			Score: 0.80 - float64(i)/100, Content: strings.Repeat("长正文", 4000),
		})
	}
	for _, r := range rows {
		r.IsEnabled = true
		r.KnowledgeID = "doc-" + r.ChunkID
	}
	return rows
}

// candidateRun is what HybridSearch hands processSearchResults: the fused,
// cut, filled candidates and the matched content buildChunkIndex derives.
type candidateRun struct {
	chunks  []*types.IndexWithScore
	matched map[string]string
	engine  *contentEngine
}

func runCandidates(t *testing.T, deferContent bool, matchCount int) candidateRun {
	t.Helper()
	ctx := context.Background()
	engine := &contentEngine{
		fakeRetrieveEngineService: fakeRetrieveEngineService{
			engineType: types.OpenSearchRetrieverEngineType,
			support:    []types.RetrieverType{types.VectorRetrieverType, types.KeywordsRetrieverType},
		},
		deferContent: deferContent,
		ranking:      bf02Ranking(),
	}
	g := &storeGroup{
		StoreID:         "store-os",
		KBIDs:           []string{"kb-doc", "kb-faq"},
		Engine:          buildBoundComposite(t, engine),
		BaseParams:      append(vectorParams("停车"), keywordsRetrieveParams("停车")...),
		TopK:            50,
		ImageKBIDs:      recalls("kb-doc"),
		VectorThreshold: 0.3,
	}
	svc := &knowledgeBaseService{}
	res, err := svc.retrieveFromStores(ctx, []*storeGroup{g}, retriever.EngineAwareNormalizer{})
	require.NoError(t, err)
	vectorLists, keywordLists := classifyRetrievalResults(ctx, res)
	chunks := fuseOrDeduplicate(ctx, vectorLists, keywordLists, nil)
	chunks = chunks[:min(len(chunks), matchCount)]
	newMatchedContentSource([]*storeGroup{g}).fill(ctx, chunks)
	return candidateRun{chunks: chunks, matched: svc.buildChunkIndex(chunks).matchedContents, engine: engine}
}

// With the switch on and off the kept candidates have the same IDs, order,
// scores and matched content, and the deferred run fetches the text only for
// the hits that survived the cut, in one lookup.
func TestDeferredMatchedContentMatchesInlineContent(t *testing.T) {
	const matchCount = 6
	inline := runCandidates(t, false, matchCount)
	deferred := runCandidates(t, true, matchCount)

	require.Len(t, inline.chunks, matchCount)
	require.Len(t, deferred.chunks, matchCount)
	for i := range inline.chunks {
		a, b := inline.chunks[i], deferred.chunks[i]
		assert.Equal(t, a.ChunkID, b.ChunkID, "order at %d", i)
		assert.Equal(t, a.Score, b.Score, "score of %s", a.ChunkID)
		assert.Equal(t, a.MatchType, b.MatchType, "match type of %s", a.ChunkID)
		assert.Equal(t, a.SourceType, b.SourceType, "source type of %s", a.ChunkID)
		assert.Equal(t, a.Content, b.Content, "matched content of %s", a.ChunkID)
	}
	assert.Equal(t, inline.matched, deferred.matched)
	for _, id := range []string{"gq-1", "faq-1", "img-1"} {
		assert.Contains(t, deferred.matched, id, "row kind %s must be among the kept hits", id)
	}

	assert.Empty(t, inline.engine.fetched, "no lookup when content is inline")
	require.Len(t, deferred.engine.fetched, 1, "one lookup for all kept hits")
	assert.ElementsMatch(t, ids(deferred.chunks), deferred.engine.fetched[0])
}

// A failed lookup leaves MatchedContent empty but keeps every hit.
func TestDeferredMatchedContentLookupFailureKeepsHits(t *testing.T) {
	logs := captureServiceLogs(t)
	g := &storeGroup{KBIDs: []string{"kb-doc"}, Engine: buildBoundComposite(t, &failingFetchEngine{})}
	chunks := []*types.IndexWithScore{{ChunkID: "c1", KnowledgeBaseID: "kb-doc", Score: 0.5}}
	newMatchedContentSource([]*storeGroup{g}).fill(context.Background(), chunks)
	require.Len(t, chunks, 1)
	assert.Empty(t, chunks[0].Content)
	assert.Contains(t, logs.String(), "deferred matched content lookup failed")
}

type failingFetchEngine struct{ fakeRetrieveEngineService }

func (*failingFetchEngine) DefersMatchedContent() bool { return true }

func (*failingFetchEngine) FetchMatchedContent(context.Context, []string) (map[string]string, error) {
	return nil, fmt.Errorf("lookup: %w", openSearchRepo.ErrTransport)
}

// Without a deferring store there is nothing to fill: nil source, no-op.
func TestMatchedContentSourceNilWithoutDeferringStore(t *testing.T) {
	g := &storeGroup{KBIDs: []string{"kb"}, Engine: buildBoundComposite(t, &fakeRetrieveEngineService{
		engineType: types.OpenSearchRetrieverEngineType,
		support:    []types.RetrieverType{types.KeywordsRetrieverType},
	})}
	src := newMatchedContentSource([]*storeGroup{g})
	assert.Nil(t, src)
	src.fill(context.Background(), []*types.IndexWithScore{{ChunkID: "c1"}}) // must not panic
}

// A chunk disabled between retrieval and the PG read stays out of the
// results, deferred content or not: assembleSearchResults re-checks the PG row.
func TestChunkDisabledAfterRetrievalIsDropped(t *testing.T) {
	svc := &knowledgeBaseService{}
	hits := []*types.IndexWithScore{
		{ChunkID: "kept", KnowledgeID: "doc", Score: 0.9, Content: "kept text", IsEnabled: true},
		{ChunkID: "disabled", KnowledgeID: "doc", Score: 0.8, Content: "disabled text", IsEnabled: true},
	}
	chunkMap := map[string]*types.Chunk{
		"kept":     {ID: "kept", KnowledgeID: "doc", ChunkType: types.ChunkTypeText, IsEnabled: true},
		"disabled": {ID: "disabled", KnowledgeID: "doc", ChunkType: types.ChunkTypeText, IsEnabled: false},
	}
	knowledgeMap := map[string]*types.Knowledge{"doc": {ID: "doc"}}
	results := svc.assembleSearchResults(context.Background(), hits, chunkMap, knowledgeMap,
		svc.buildChunkIndex(hits), true)
	require.Len(t, results, 1)
	assert.Equal(t, "kept", results[0].ID)
	assert.Equal(t, "kept text", results[0].MatchedContent)
}

// A result set the OpenSearch driver cut short to fit its byte cap reaches
// the search layer as a partial result: the hits are kept, the search does not
// fail, and the truncation is logged.
func TestRetrieveFromStores_KeepsOpenSearchTruncatedResults(t *testing.T) {
	logs := captureServiceLogs(t)
	fake := &fakeRetrieveEngineService{
		engineType: types.OpenSearchRetrieverEngineType,
		support:    []types.RetrieverType{types.KeywordsRetrieverType},
		canned: []*types.IndexWithScore{
			{ChunkID: "c0", Score: 0.9}, {ChunkID: "c1", Score: 0.8},
		},
		cannedResultErr: fmt.Errorf("%w: kept top 250 of 500 keywords hits", openSearchRepo.ErrResultsTruncated),
	}
	g := &storeGroup{
		StoreID:    "store-os",
		KBIDs:      []string{"kb-1"},
		Engine:     buildBoundComposite(t, fake),
		BaseParams: keywordsRetrieveParams("q"),
		TopK:       500,
	}

	res, err := (&knowledgeBaseService{}).retrieveFromStores(
		context.Background(), []*storeGroup{g}, retriever.EngineAwareNormalizer{})

	require.NoError(t, err, "a truncated result set must not fail the search")
	require.Len(t, res, 1)
	assert.Equal(t, []string{"c0", "c1"}, ids(res[0].Results))
	assert.ErrorIs(t, res[0].Error, openSearchRepo.ErrResultsTruncated)
	assert.Contains(t, logs.String(), "results truncated to fit response byte cap")
}
