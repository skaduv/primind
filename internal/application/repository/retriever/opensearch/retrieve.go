package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types"
)

// Retrieve dispatches to vectorRetrieve or keywordsRetrieve based on
// params.RetrieverType. Returns exactly one *RetrieveResult — the
// service-layer multi-store dispatcher depends on this one-bundle-per-
// driver protocol. Score in result is RAW [0, 1] passthrough (the
// OpenSearch k-NN plugin's SpaceType.COSINESIMIL.scoreTranslation has
// already mapped to (1 + cos) / 2 before the score reaches us).
//
// Dim resolution order:
//  1. params.AdditionalParams["dim"] if present and positive
//  2. len(params.Embedding) if positive
//  3. cross-dim multi-index search for keyword-only paths via
//     "<base>_*" wildcard pattern
//
// Vector retrieve always requires a positive dim (it's intrinsic to k-NN).
func (r *Repository) Retrieve(
	ctx context.Context,
	params types.RetrieveParams,
) ([]*types.RetrieveResult, error) {
	ctx, cancel := operationContext(ctx, r.client)
	defer cancel()
	dim, multiIndex := resolveDim(params)

	switch params.RetrieverType {
	case types.VectorRetrieverType:
		if dim == 0 {
			return nil, fmt.Errorf(
				"opensearch: vector retrieve requires embedding or AdditionalParams[\"dim\"]: %w",
				ErrDimensionMismatch)
		}
		if err := r.ensureReady(ctx, dim); err != nil {
			return nil, err
		}
		return r.vectorRetrieve(ctx, params, dim)
	case types.KeywordsRetrieverType:
		if multiIndex {
			// BM25-only path without dim — search across "<base>_*"
			// wildcard pattern. Slower than a single-index search but
			// correct for admin / keyword-only callers.
			return r.keywordsRetrieve(ctx, params, r.baseIndex+"_*")
		}
		if err := r.ensureReady(ctx, dim); err != nil {
			return nil, err
		}
		return r.keywordsRetrieve(ctx, params, r.indexAlias(dim))
	default:
		return nil, fmt.Errorf("opensearch: unsupported retriever type %q", params.RetrieverType)
	}
}

// resolveDim returns (dim, multiIndex). multiIndex=true means dim is
// unknown and the caller should use a wildcard alias pattern (keyword
// path only — vector retrieve cannot be dim-less).
func resolveDim(p types.RetrieveParams) (int, bool) {
	if p.AdditionalParams != nil {
		if v, ok := p.AdditionalParams["dim"].(int); ok && v > 0 {
			return v, false
		}
	}
	if len(p.Embedding) > 0 {
		return len(p.Embedding), false
	}
	return 0, true // unknown — multi-index fallback for keyword
}

func (r *Repository) vectorRetrieve(
	ctx context.Context, p types.RetrieveParams, dim int,
) ([]*types.RetrieveResult, error) {
	k, filters := effectiveTopK(ctx, p), fromParams(p)
	return r.retrieveShrinking(ctx, p, r.indexAlias(dim), types.VectorRetrieverType, types.MatchTypeEmbedding, k,
		func(size int) ([]byte, error) { return buildKNNQuery(p.Embedding, k, size, p.Threshold, filters) })
}

func (r *Repository) keywordsRetrieve(
	ctx context.Context, p types.RetrieveParams, indexPattern string,
) ([]*types.RetrieveResult, error) {
	topK, filters := effectiveTopK(ctx, p), fromParams(p)
	return r.retrieveShrinking(ctx, p, indexPattern, types.KeywordsRetrieverType, types.MatchTypeKeywords, topK,
		func(size int) ([]byte, error) { return buildKeywordQuery(p.Query, size, p.Threshold, filters) })
}

// maxShrinkRetries bounds the oversized-response fallback: 500 -> 250 -> 125 -> 62.
const maxShrinkRetries = 3

// retrieveShrinking runs a retrieval search and, when the response exceeds the
// client's byte cap, halves the requested hit count and retries, at most
// maxShrinkRetries times. build must return a query whose hits for a smaller
// size are a prefix of those for a larger one (the k-NN query keeps k and
// shrinks only size), so a shrunk result keeps the original order and scores.
//
// A shrunk result is returned with RetrieveResult.Error set to
// ErrResultsTruncated: CompositeRetrieveEngine and the service fan-out keep
// such partial results instead of failing the whole search. When even the
// smallest size is too large, the original error is returned unchanged.
func (r *Repository) retrieveShrinking(
	ctx context.Context,
	p types.RetrieveParams,
	index string,
	rt types.RetrieverType,
	mt types.MatchType,
	topK int,
	build func(size int) ([]byte, error),
) ([]*types.RetrieveResult, error) {
	started := time.Now()
	size, retries := topK, 0
	record := retrievalLog{index: index, retriever: rt, kbCount: len(p.KnowledgeBaseIDs), topK: topK}
	for {
		body, err := build(size)
		if err != nil {
			return nil, err
		}
		hits, n, err := r.search(ctx, index, body, r.retrievalSourceFields())
		record.size, record.bytes, record.retries = size, n, retries
		if err == nil {
			record.hits, record.truncated, record.elapsed = len(hits), size < topK, time.Since(started)
			r.logRetrieval(ctx, record)
			results := wrapResults(ctx, hits, rt, mt)
			if record.truncated {
				if stats := types.RetrievalStatsFromContext(ctx); stats != nil {
					stats.TruncatedSets.Add(1)
				}
				results[0].Error = fmt.Errorf("%w: kept top %d of %d %s hits",
					ErrResultsTruncated, size, topK, rt)
			}
			return results, nil
		}
		if !errors.Is(err, ErrResponseTooLarge) || retries >= maxShrinkRetries || size <= 1 {
			record.err, record.elapsed = err, time.Since(started)
			r.logRetrieval(ctx, record)
			return nil, err
		}
		retries++
		size = max(1, size/2)
		logger.GetLogger(ctx).WithFields(logger.Fields{
			"index": index, "retriever": rt, "next_size": size, "retry": retries,
			"response_cap": responseCap(r.client),
		}).Warn("[OpenSearch] retrieval response exceeded byte cap; retrying with fewer hits")
	}
}

// effectiveTopK returns the caller's TopK or 10 if unset. The 10 is a
// defensive default — every real caller sets TopK, so a TopK=10 in
// production logs indicates a caller bug. Caps at 10000 to stay below
// OpenSearch's index.max_result_window default.
func effectiveTopK(ctx context.Context, p types.RetrieveParams) int {
	if p.TopK <= 0 {
		logger.GetLogger(ctx).Warnf("[OpenSearch] Retrieve called with TopK<=0; defaulting to 10 (caller bug?)")
		return 10
	}
	if p.TopK > 10000 {
		return 10000
	}
	return p.TopK
}

// retrievalSourceFields lists the _source fields a retrieval search asks for:
// everything hit.Source decodes, minus content when it is deferred.
func (r *Repository) retrievalSourceFields() []string {
	if r.deferContent {
		return hitSourceFieldsWithoutContent
	}
	return hitSourceFields
}

// search runs one search and decodes its hits, fetching only includes from
// _source. It returns the response body size it read alongside the hits.
// Returning stored vectors (or unused fields) would spend the response byte
// cap before the hits reach the decoder.
func (r *Repository) search(
	ctx context.Context, indexPattern string, body []byte, includes []string,
) ([]hit, int64, error) {
	if stats := types.RetrievalStatsFromContext(ctx); stats != nil {
		stats.Searches.Add(1)
	}
	req := osapi.SearchReq{
		Indices: []string{indexPattern},
		Body:    bytes.NewReader(body),
		Params:  osapi.SearchParams{SourceIncludes: includes},
	}
	resp, err := r.client.Search(ctx, &req)
	if err != nil {
		if isNotFound(err) {
			return nil, 0, fmt.Errorf("opensearch: index %s missing: %w", indexPattern, ErrIndexNotFound)
		}
		return nil, 0, wrapTransport(err)
	}
	defer func() {
		if resp != nil {
			drainAndClose(resp.Inspect().Response.Body)
		}
	}()
	// The transport already read the whole body within the client's cap; the
	// limit here follows that cap instead of repeating a fixed size.
	counted := &countingReader{r: io.LimitReader(resp.Inspect().Response.Body, responseCap(r.client))}
	hits, err := parseSearchHits(counted)
	if stats := types.RetrievalStatsFromContext(ctx); stats != nil {
		stats.ResponseBytes.Add(counted.n)
	}
	return hits, counted.n, err
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// retrievalLog is the structured record written once per retrieval search.
type retrievalLog struct {
	index     string
	retriever types.RetrieverType
	kbCount   int
	topK      int
	size      int
	hits      int
	bytes     int64
	elapsed   time.Duration
	retries   int
	truncated bool
	err       error
}

// logRetrieval writes one structured line per retrieval search (request_id
// comes from the context logger), and a WARN once the response passes half
// of the client's byte cap so growth shows up before searches fail.
func (r *Repository) logRetrieval(ctx context.Context, l retrievalLog) {
	limit := responseCap(r.client)
	entry := logger.GetLogger(ctx).WithFields(logger.Fields{
		"index":          l.index,
		"retriever":      l.retriever,
		"kb_count":       l.kbCount,
		"top_k":          l.topK,
		"size":           l.size,
		"hits":           l.hits,
		"response_bytes": l.bytes,
		"response_cap":   limit,
		"elapsed_ms":     l.elapsed.Milliseconds(),
		"retries":        l.retries,
		"truncated":      l.truncated,
	})
	switch {
	case l.err != nil:
		entry.WithField("error", l.err.Error()).Warn("[OpenSearch] retrieval search failed")
	case l.bytes > limit/2:
		entry.Warn("[OpenSearch] retrieval response above half of the byte cap")
	default:
		entry.Info("[OpenSearch] retrieval search")
	}
}

// hit is the structural shape we pull from each OpenSearch search hit.
// Field-by-field decode (vs map[string]any) keeps the JSON shape
// pinned at compile time.
type hit struct {
	ID     string  `json:"_id"` // equals chunk_id per the indexing invariant
	Score  float64 `json:"_score"`
	Source struct {
		Content         string `json:"content"`
		ChunkID         string `json:"chunk_id"`
		KnowledgeID     string `json:"knowledge_id"`
		KnowledgeBaseID string `json:"knowledge_base_id"`
		SourceID        string `json:"source_id"`
		SourceType      int    `json:"source_type"` // integer, not stringified
		TagID           string `json:"tag_id"`
		IsEnabled       bool   `json:"is_enabled"`
	} `json:"_source"`
}

// hitSourceFields lists the _source fields hit.Source decodes, read from its
// json tags so a field added to the struct is also fetched.
var hitSourceFields = sourceFieldNames(reflect.TypeOf(hit{}.Source))

// hitSourceFieldsWithoutContent is hitSourceFields without the indexed text,
// used when content is deferred. It keeps every field the service layer
// filters on (source_type drives the image recall rules).
var hitSourceFieldsWithoutContent = slices.DeleteFunc(slices.Clone(hitSourceFields),
	func(f string) bool { return f == "content" })

func sourceFieldNames(t reflect.Type) []string {
	fields := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			fields = append(fields, name)
		}
	}
	return fields
}

func parseSearchHits(body io.Reader) ([]hit, error) {
	var resp struct {
		Hits struct {
			Hits []hit `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("opensearch: parse search response: %w", ErrTransport)
	}
	return resp.Hits.Hits, nil
}

// wrapResults converts OpenSearch hits to the upstream IndexWithScore
// shape. Always returns exactly one *RetrieveResult — the service-layer
// multi-store dispatcher depends on this "one engine result per driver"
// contract.
//
// Defensive: warns if hit._id != _source.chunk_id (we set _id=chunk_id
// on every write, so a mismatch means external tooling has been writing
// into the index) but does not fail the query — the result is still
// usable for ranking, just suspicious.
func wrapResults(ctx context.Context, hits []hit, rt types.RetrieverType, mt types.MatchType) []*types.RetrieveResult {
	log := logger.GetLogger(ctx)
	out := make([]*types.IndexWithScore, 0, len(hits))
	for _, h := range hits {
		score := h.Score
		if rt == types.VectorRetrieverType {
			score = knnScoreToCosine(score)
		}
		if h.ID != h.Source.ChunkID {
			log.Warnf("[OpenSearch] hit._id=%q != _source.chunk_id=%q (D12 invariant violation)",
				h.ID, h.Source.ChunkID)
		}
		out = append(out, &types.IndexWithScore{
			ID:              h.ID,
			ChunkID:         h.Source.ChunkID,
			KnowledgeID:     h.Source.KnowledgeID,
			KnowledgeBaseID: h.Source.KnowledgeBaseID,
			SourceID:        h.Source.SourceID,
			SourceType:      types.SourceType(h.Source.SourceType),
			TagID:           h.Source.TagID,
			Content:         h.Source.Content,
			Score:           score,
			MatchType:       mt,
			IsEnabled:       h.Source.IsEnabled,
		})
	}
	return []*types.RetrieveResult{{
		Results:             out,
		RetrieverEngineType: types.OpenSearchRetrieverEngineType,
		RetrieverType:       rt,
	}}
}
