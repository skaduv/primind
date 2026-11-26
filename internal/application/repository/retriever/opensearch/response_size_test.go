package opensearch

// BF-02: retrieval response size. These tests pin the field list, the parse
// limit following the client cap, the oversized-response fallback, the
// per-search log and the deferred-content lookup.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/primind/primind/internal/types"
)

// searchRequest is what the fake cluster saw for one _search call.
type searchRequest struct {
	path     string
	includes []string
	size     int
	k        int // k-NN k; 0 for keyword queries
	ids      []string
}

// fakeSearchCluster answers _search calls from a ranked list of hits. Every
// hit carries the same content, so the body size grows with the hit count.
type fakeSearchCluster struct {
	mu       sync.Mutex
	requests []searchRequest
	ranked   int    // number of hits the full ranking holds
	content  string // content of every hit
}

func (c *fakeSearchCluster) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Size  int `json:"size"`
		Query struct {
			KNN struct {
				Embedding struct {
					K int `json:"k"`
				} `json:"embedding"`
			} `json:"knn"`
			IDs struct {
				Values []string `json:"values"`
			} `json:"ids"`
		} `json:"query"`
	}
	_ = json.Unmarshal(raw, &body)
	req := searchRequest{path: r.URL.Path, size: body.Size, k: body.Query.KNN.Embedding.K, ids: body.Query.IDs.Values}
	if inc := r.URL.Query().Get("_source_includes"); inc != "" {
		req.includes = strings.Split(inc, ",")
	}
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()

	source := func(id string) map[string]any {
		full := map[string]any{
			"chunk_id": id, "knowledge_id": "doc-" + id, "knowledge_base_id": "kb-1",
			"source_id": id, "source_type": 0, "tag_id": "", "is_enabled": true,
			"content": c.content, "embedding": []float32{0.1, 0.2, 0.3, 0.4},
		}
		if req.includes == nil {
			return full
		}
		out := map[string]any{}
		for _, f := range req.includes {
			if v, ok := full[f]; ok {
				out[f] = v
			}
		}
		return out
	}
	var hits []map[string]any
	if len(req.ids) > 0 {
		for _, id := range req.ids {
			if strings.HasPrefix(id, "c") {
				hits = append(hits, map[string]any{"_id": id, "_score": 1.0, "_source": source(id)})
			}
		}
	} else {
		for i := range min(req.size, c.ranked) {
			id := fmt.Sprintf("c%04d", i)
			hits = append(hits, map[string]any{
				"_id": id, "_score": 1.0 - float64(i)/10000, "_source": source(id),
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": hits}})
}

func (c *fakeSearchCluster) seen() []searchRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.requests)
}

// newSizedRepo builds a repository against the fake cluster with the given
// response cap and a private request budget (so attempts can be counted).
func newSizedRepo(t *testing.T, cluster *fakeSearchCluster, capBytes int64) (*Repository, *RequestBudget) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(cluster.handler))
	t.Cleanup(ts.Close)
	opts := DefaultRuntimeOptions()
	opts.MaxResponseBytes = capBytes
	budget, err := NewRequestBudget(4, 2, 8)
	if err != nil {
		t.Fatal(err)
	}
	repo := managedTestRepo(t, ts.URL, "store_a", WithRuntimeOptions(opts), WithRequestBudget(budget))
	ready := make(chan struct{})
	close(ready)
	repo.initializations = map[int]*indexInitialization{4: {done: ready}}
	return repo, budget
}

func retrieveParams(rt types.RetrieverType, topK int) types.RetrieveParams {
	return types.RetrieveParams{
		Query:            "policy",
		Embedding:        []float32{1, 0, 0, 0},
		KnowledgeBaseIDs: []string{"kb-1"},
		TopK:             topK,
		RetrieverType:    rt,
	}
}

// The field list comes from hit.Source's json tags and must hold every field
// the service layer reads (BF-02 C5). Deleting one of them from the struct, or
// tagging it differently, fails here.
func TestHitSourceFieldsCoverServiceFields(t *testing.T) {
	want := []string{
		"content", "chunk_id", "knowledge_id", "knowledge_base_id",
		"source_id", "source_type", "tag_id", "is_enabled",
	}
	if got := slices.Sorted(slices.Values(hitSourceFields)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("hitSourceFields = %v, want the set %v", hitSourceFields, want)
	}
	deferred := slices.DeleteFunc(slices.Clone(want), func(f string) bool { return f == "content" })
	got := slices.Sorted(slices.Values(hitSourceFieldsWithoutContent))
	if !slices.Equal(got, slices.Sorted(slices.Values(deferred))) {
		t.Fatalf("hitSourceFieldsWithoutContent = %v, want the set %v", hitSourceFieldsWithoutContent, deferred)
	}
}

func TestRetrieveRequestsOnlyDecodedFields(t *testing.T) {
	for _, deferContent := range []bool{false, true} {
		t.Run(fmt.Sprintf("defer=%t", deferContent), func(t *testing.T) {
			cluster := &fakeSearchCluster{ranked: 3, content: "text"}
			repo, _ := newSizedRepo(t, cluster, 1<<20)
			repo.deferContent = deferContent
			for _, rt := range []types.RetrieverType{types.VectorRetrieverType, types.KeywordsRetrieverType} {
				res, err := repo.Retrieve(context.Background(), retrieveParams(rt, 3))
				if err != nil {
					t.Fatalf("%s: %v", rt, err)
				}
				for _, h := range res[0].Results {
					if h.KnowledgeBaseID != "kb-1" || h.ChunkID == "" || !h.IsEnabled {
						t.Fatalf("%s: service fields missing from hit %+v", rt, h)
					}
					if (h.Content == "") != deferContent {
						t.Fatalf("%s: content=%q with defer=%t", rt, h.Content, deferContent)
					}
				}
			}
			for _, req := range cluster.seen() {
				if slices.Contains(req.includes, "embedding") {
					t.Fatalf("request asked for the vector: %v", req.includes)
				}
				if slices.Contains(req.includes, "content") == deferContent {
					t.Fatalf("includes=%v with defer=%t", req.includes, deferContent)
				}
				if !slices.Contains(req.includes, "source_type") {
					t.Fatalf("source_type (image recall filter) missing: %v", req.includes)
				}
			}
		})
	}
}

// The parse layer follows the client's cap instead of a fixed 16 MiB: with a
// 32 MiB cap a 20 MiB response decodes. (Production has no setting for the
// cap; this only proves the decoder no longer cuts at 16 MiB.)
func TestParseFollowsClientResponseCap(t *testing.T) {
	cluster := &fakeSearchCluster{ranked: 1, content: strings.Repeat("a", 20<<20)}
	repo, _ := newSizedRepo(t, cluster, 32<<20)
	res, err := repo.Retrieve(context.Background(), retrieveParams(types.KeywordsRetrieverType, 1))
	if err != nil {
		t.Fatalf("20 MiB response under a 32 MiB cap: %v", err)
	}
	if got := len(res[0].Results[0].Content); got != 20<<20 {
		t.Fatalf("content length = %d, want %d", got, 20<<20)
	}
}

// First attempt over the cap, a smaller one fits: the driver returns the top
// of the original ranking, marks the set truncated and does not fail. The
// vector query keeps k and only shrinks size.
func TestRetrieveShrinksSizeOnOversizedResponse(t *testing.T) {
	for _, rt := range []types.RetrieverType{types.VectorRetrieverType, types.KeywordsRetrieverType} {
		t.Run(string(rt), func(t *testing.T) {
			// ~1.2 KiB per hit; a 200 KiB cap fits 125 hits but not 250.
			cluster := &fakeSearchCluster{ranked: 500, content: strings.Repeat("x", 1000)}
			repo, budget := newSizedRepo(t, cluster, 200<<10)

			res, err := repo.Retrieve(context.Background(), retrieveParams(rt, 500))
			if err != nil {
				t.Fatalf("retrieve: %v", err)
			}
			reqs := cluster.seen()
			var sizes []int
			for _, r := range reqs {
				sizes = append(sizes, r.size)
				if rt == types.VectorRetrieverType && r.k != 500 {
					t.Fatalf("k-NN k changed to %d; it must stay 500", r.k)
				}
			}
			if !slices.Equal(sizes, []int{500, 250, 125}) {
				t.Fatalf("sizes = %v, want [500 250 125]", sizes)
			}
			if got := budget.Snapshot().Attempts; got != int64(len(reqs)) {
				t.Fatalf("transport attempts = %d for %d requests; the SDK must not re-download an oversized response",
					got, len(reqs))
			}
			hits := res[0].Results
			if len(hits) != 125 {
				t.Fatalf("hits = %d, want 125", len(hits))
			}
			for i, h := range hits {
				if want := fmt.Sprintf("c%04d", i); h.ChunkID != want {
					t.Fatalf("hit %d = %s, want %s: result is not a prefix of the ranking", i, h.ChunkID, want)
				}
			}
			if !errors.Is(res[0].Error, ErrResultsTruncated) {
				t.Fatalf("result set error = %v, want ErrResultsTruncated", res[0].Error)
			}
		})
	}
}

// Still over the cap at the smallest size: the original error comes back,
// after 1 + maxShrinkRetries requests and one transport attempt each.
func TestRetrieveShrinkGivesUpWithOriginalError(t *testing.T) {
	cluster := &fakeSearchCluster{ranked: 500, content: strings.Repeat("x", 4000)}
	repo, budget := newSizedRepo(t, cluster, 1<<10)

	_, err := repo.Retrieve(context.Background(), retrieveParams(types.KeywordsRetrieverType, 500))
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	var sizes []int
	for _, r := range cluster.seen() {
		sizes = append(sizes, r.size)
	}
	if !slices.Equal(sizes, []int{500, 250, 125, 62}) {
		t.Fatalf("sizes = %v, want [500 250 125 62]", sizes)
	}
	if got := budget.Snapshot().Attempts; got != 4 {
		t.Fatalf("transport attempts = %d, want 4 (one per request)", got)
	}
}

// Errors other than an oversized response are not retried.
func TestRetrieveDoesNotShrinkOnOtherErrors(t *testing.T) {
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"security_exception"},"status":401}`))
	}))
	defer ts.Close()
	repo := managedTestRepo(t, ts.URL, "store_a")
	_, err := repo.Retrieve(context.Background(), retrieveParams(types.KeywordsRetrieverType, 500))
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func loggerContext() (context.Context, *logtest.Hook) {
	l, hook := logtest.NewNullLogger()
	l.SetLevel(logrus.DebugLevel)
	entry := logrus.NewEntry(l).WithField("request_id", "req-bf02")
	return context.WithValue(context.Background(), types.LoggerContextKey, entry), hook
}

func findLog(hook *logtest.Hook, msg string) *logrus.Entry {
	for _, e := range hook.AllEntries() {
		if e.Message == msg {
			return e
		}
	}
	return nil
}

func TestRetrieveLogsResponseSize(t *testing.T) {
	cluster := &fakeSearchCluster{ranked: 10, content: "text"}
	repo, _ := newSizedRepo(t, cluster, 1<<20)
	ctx, hook := loggerContext()
	if _, err := repo.Retrieve(ctx, retrieveParams(types.VectorRetrieverType, 10)); err != nil {
		t.Fatal(err)
	}
	e := findLog(hook, "[OpenSearch] retrieval search")
	if e == nil {
		t.Fatalf("no retrieval log; got %v", hook.AllEntries())
	}
	if e.Level != logrus.InfoLevel {
		t.Fatalf("level = %s, want info", e.Level)
	}
	for _, f := range []string{
		"request_id", "index", "retriever", "kb_count", "top_k", "size", "hits",
		"response_bytes", "response_cap", "elapsed_ms", "retries", "truncated",
	} {
		if _, ok := e.Data[f]; !ok {
			t.Fatalf("log field %q missing: %v", f, e.Data)
		}
	}
	if e.Data["request_id"] != "req-bf02" || e.Data["hits"] != 10 || e.Data["kb_count"] != 1 ||
		e.Data["retries"] != 0 || e.Data["truncated"] != false {
		t.Fatalf("unexpected log fields: %v", e.Data)
	}
	if n, _ := e.Data["response_bytes"].(int64); n <= 0 {
		t.Fatalf("response_bytes = %v, want > 0", e.Data["response_bytes"])
	}
}

func TestRetrieveWarnsAboveHalfTheCap(t *testing.T) {
	// ~1.2 KiB per hit, 100 hits ~ 120 KiB: above half of a 200 KiB cap.
	cluster := &fakeSearchCluster{ranked: 100, content: strings.Repeat("x", 1000)}
	repo, _ := newSizedRepo(t, cluster, 200<<10)
	ctx, hook := loggerContext()
	if _, err := repo.Retrieve(ctx, retrieveParams(types.KeywordsRetrieverType, 100)); err != nil {
		t.Fatal(err)
	}
	e := findLog(hook, "[OpenSearch] retrieval response above half of the byte cap")
	if e == nil || e.Level != logrus.WarnLevel {
		t.Fatalf("no WARN above half the cap; got %v", hook.AllEntries())
	}
}

func TestRetrieveLogsShrinkRetries(t *testing.T) {
	cluster := &fakeSearchCluster{ranked: 500, content: strings.Repeat("x", 1000)}
	repo, _ := newSizedRepo(t, cluster, 200<<10)
	ctx, hook := loggerContext()
	if _, err := repo.Retrieve(ctx, retrieveParams(types.KeywordsRetrieverType, 500)); err != nil {
		t.Fatal(err)
	}
	var final *logrus.Entry
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "[OpenSearch] retrieval") && e.Data["retries"] != nil {
			final = e
		}
	}
	if final == nil || final.Data["retries"] != 2 || final.Data["truncated"] != true || final.Data["size"] != 125 {
		t.Fatalf("final retrieval log = %v", final)
	}
}

func TestFetchMatchedContentReadsByChunkID(t *testing.T) {
	cluster := &fakeSearchCluster{content: "Title\nPolicy text"}
	repo, _ := newSizedRepo(t, cluster, 1<<20)
	repo.deferContent = true
	got, err := repo.FetchMatchedContent(context.Background(), []string{"c0001", "c0002", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["c0001"] != "Title\nPolicy text" || got["c0002"] != "Title\nPolicy text" {
		t.Fatalf("content = %v", got)
	}
	reqs := cluster.seen()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if !strings.Contains(reqs[0].path, "store_a_*") {
		t.Fatalf("lookup path = %s, want every index of the repository", reqs[0].path)
	}
	if !slices.Equal(slices.Sorted(slices.Values(reqs[0].includes)), []string{"chunk_id", "content"}) {
		t.Fatalf("includes = %v", reqs[0].includes)
	}
	if !slices.Equal(reqs[0].ids, []string{"c0001", "c0002", "missing"}) {
		t.Fatalf("ids = %v", reqs[0].ids)
	}
}

func TestDeferContentFromEnv(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "false": false, "true": true, "1": true, "bogus": false} {
		t.Setenv(deferContentEnv, raw)
		if got := deferContentFromEnv(context.Background()); got != want {
			t.Fatalf("%s=%q -> %t, want %t", deferContentEnv, raw, got, want)
		}
	}
}
