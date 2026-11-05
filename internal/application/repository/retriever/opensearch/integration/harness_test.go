package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/primind/primind/internal/application/repository/retriever/opensearch"
	"github.com/primind/primind/internal/types"
	secutils "github.com/primind/primind/internal/utils"
)

func scope() Scope {
	return Scope{
		StoreID: "abcdef0123456789", Index: types.IndexConfig{
			IndexName: "os02local", NumberOfShards: 1,
			NumberOfReplicas: 1, HNSWM: 16, HNSWEFConstruction: 100, HNSWEFSearch: 100, KNNEngine: "lucene",
		},
		Alias: "os02local_abcdef012345_3", Physical: "os02local_abcdef012345_3_v1", Dimension: 3,
		KnowledgeBaseID: "kb-test", KnowledgeID: "doc-test", ChunkID: "chunk-test", RefreshInterval: "1s",
	}
}

func config() Config {
	s := scope()
	return Config{
		Mode: "replay", RunID: "local", AuthorizationRef: "LOCAL_MOCK_ONLY", Expires: time.Now().Add(time.Hour),
		Endpoint: "http://127.0.0.1:1", ClusterUUID: "mock-cluster", Version: "2.19.7", AnonymousAuthorized: true,
		QuiescentScope: true, MaxSeconds: 20, VisibilitySeconds: 1, PollMilliseconds: 10, PageSize: 2,
		MaxRecords: 1000, TotalRequests: 4, WriteRequests: 2, Waiters: 8, Replay: &s,
	}
}

func original(s Scope) Document {
	return Document{
		ChunkID:         s.ChunkID,
		SourceID:        "different-source-id",
		SourceType:      2,
		KnowledgeID:     s.KnowledgeID,
		KnowledgeBaseID: s.KnowledgeBaseID,
		TagID:           "tag-original",
		Content:         "合成重放记录 original",
		Enabled:         false,
		Recommended:     true,
		Embedding:       []float32{.25, -.5, .75},
	}
}

type mockIndex struct {
	scope         Scope
	exists, alias bool
	docs          map[string]Document
	revisions     map[string]Revision
}

func (idx *mockIndex) currentRevision(id string) Revision {
	if v, ok := idx.revisions[id]; ok {
		return v
	}
	if _, ok := idx.docs[id]; ok {
		return Revision{1, 0, 1}
	}
	return Revision{0, -1, 1}
}

func (idx *mockIndex) persist(d Document) {
	v := idx.currentRevision(d.ChunkID)
	v.Version++
	v.SeqNo++
	if idx.revisions == nil {
		idx.revisions = map[string]Revision{}
	}
	idx.revisions[d.ChunkID] = v
	idx.docs[d.ChunkID] = d
}

type mockCluster struct {
	mu                      sync.Mutex
	server                  *httptest.Server
	indexes                 map[string]*mockIndex
	Requests, Writes, Bulks int
	Fault                   string
	BadAt                   int
	BulkDelay               time.Duration
	active, peak            atomic.Int32
}

func newMock(t *testing.T, scopes []Scope, existing bool) *mockCluster {
	t.Helper()
	secutils.SetSSRFWhitelistFromRaw("127.0.0.1,::1,localhost")
	t.Cleanup(func() { secutils.SetSSRFWhitelistFromRaw("") })
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "")
	}
	m := &mockCluster{indexes: map[string]*mockIndex{}}
	for _, s := range scopes {
		m.indexes[s.Alias] = &mockIndex{scope: s, exists: existing, alias: existing, docs: map[string]Document{}}
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.server.Close)
	return m
}

func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (m *mockCluster) serve(w http.ResponseWriter, r *http.Request) {
	active := m.active.Add(1)
	defer m.active.Add(-1)
	for {
		if p := m.peak.Load(); active <= p || m.peak.CompareAndSwap(p, active) {
			break
		}
	}
	if r.URL.Path == "/_bulk" && m.BulkDelay > 0 {
		if sleepContext(r.Context(), m.BulkDelay) != nil {
			return
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requests++
	path := r.URL.Path
	if path == "/" {
		respond(
			w,
			map[string]any{
				"cluster_uuid": "mock-cluster",
				"version":      map[string]any{"number": "2.19.7", "distribution": "opensearch"},
			},
		)
		return
	}
	if path == "/_cat/plugins" {
		respond(w, []any{map[string]any{"name": "mock-node", "component": "opensearch-knn", "version": "2.19.7"}})
		return
	}
	if path == "/_bulk" {
		m.Bulks++
		scan := bufio.NewScanner(r.Body)
		scan.Buffer(make([]byte, 1024), 11<<20)
		items := []any{}
		for scan.Scan() {
			var meta struct {
				Index struct {
					Index string `json:"_index"`
					ID    string `json:"_id"`
				} `json:"index"`
			}
			if json.Unmarshal(scan.Bytes(), &meta) != nil || !scan.Scan() {
				http.Error(w, "bad bulk", 400)
				return
			}
			idx, ok := m.indexes[meta.Index.Index]
			if !ok || !idx.exists {
				http.Error(w, "wrong scope", http.StatusForbidden)
				return
			}
			d, e := decodeDocument(scan.Bytes(), idx.scope.Dimension)
			if e != nil || d.ChunkID != meta.Index.ID || d.KnowledgeBaseID != idx.scope.KnowledgeBaseID ||
				d.KnowledgeID != idx.scope.KnowledgeID {
				http.Error(w, "invalid document", 400)
				return
			}
			m.Writes++
			if m.Fault == "bulk429" {
				items = append(
					items,
					map[string]any{
						"index": map[string]any{
							"_id":    d.ChunkID,
							"status": 429,
							"error":  map[string]any{"type": "es_rejected_execution_exception"},
						},
					},
				)
				continue
			}
			idx.persist(d)
			items = append(items, map[string]any{"index": map[string]any{"_id": d.ChunkID, "status": 200}})
		}
		respond(w, map[string]any{"errors": m.Fault == "bulk429", "items": items})
		return
	}
	for alias, idx := range m.indexes {
		s := idx.scope
		if path == "/_alias/"+alias {
			if !idx.alias {
				http.NotFound(w, r)
				return
			}
			if r.Method == "HEAD" {
				return
			}
			physical := s.Physical
			if m.Fault == "alias-drift" && m.Writes >= m.BadAt {
				physical += "_other"
			}
			respond(w, map[string]any{physical: map[string]any{"aliases": map[string]any{alias: map[string]any{}}}})
			return
		}
		if path == "/"+s.Physical+"/_alias/"+alias && r.Method == "PUT" {
			idx.alias = true
			respond(w, map[string]any{"acknowledged": true})
			return
		}
		if path == "/"+s.Physical || path == "/"+alias {
			if r.Method == "HEAD" {
				if !idx.exists {
					http.NotFound(w, r)
				}
				return
			}
			if r.Method == "PUT" {
				idx.exists = true
				if s.Dimension == 0 {
					idx.alias = true
				}
				respond(w, map[string]any{"acknowledged": true})
				return
			}
		}
		if path == "/"+s.Physical+"/_settings" {
			respond(
				w,
				map[string]any{
					s.Physical: map[string]any{
						"settings": map[string]string{
							"index.number_of_shards":         fmt.Sprint(s.Index.NumberOfShards),
							"index.number_of_replicas":       fmt.Sprint(s.Index.NumberOfReplicas),
							"index.refresh_interval":         s.RefreshInterval,
							"index.knn":                      "true",
							"index.knn.algo_param.ef_search": fmt.Sprint(s.Index.HNSWEFSearch),
						},
					},
				},
			)
			return
		}
		if path == "/"+s.Physical+"/_mapping" {
			respond(
				w,
				map[string]any{
					s.Physical: map[string]any{
						"mappings": map[string]any{
							"properties": map[string]any{
								"embedding": map[string]any{
									"type":      "knn_vector",
									"dimension": s.Dimension,
									"method": map[string]any{
										"name":       "hnsw",
										"engine":     s.Index.KNNEngine,
										"space_type": "cosinesimil",
										"parameters": map[string]any{
											"m":               s.Index.HNSWM,
											"ef_construction": s.Index.HNSWEFConstruction,
										},
									},
								},
							},
						},
					},
				},
			)
			return
		}
		if strings.HasPrefix(path, "/"+alias+"/_doc/") && r.Method == "GET" {
			id := strings.TrimPrefix(path, "/"+alias+"/_doc/")
			d, ok := idx.docs[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			v := idx.currentRevision(id)
			respond(
				w,
				map[string]any{
					"_id":           id,
					"_index":        s.Physical,
					"found":         true,
					"_version":      v.Version,
					"_seq_no":       v.SeqNo,
					"_primary_term": v.PrimaryTerm,
					"_source":       d,
				},
			)
			return
		}
		if strings.HasPrefix(path, "/"+alias+"/_doc/") && r.Method == "PUT" {
			var d Document
			if json.NewDecoder(r.Body).Decode(&d) != nil {
				http.Error(w, "bad", 400)
				return
			}
			if d.ChunkID != strings.TrimPrefix(path, "/"+alias+"/_doc/") || d.KnowledgeBaseID != s.KnowledgeBaseID ||
				d.KnowledgeID != s.KnowledgeID {
				http.Error(w, "scope", http.StatusForbidden)
				return
			}
			m.Writes++
			if m.Fault == "save403" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if m.Fault == "corruption" && m.Writes >= m.BadAt {
				d.TagID = "corrupt"
			}
			idx.persist(d)
			respond(w, map[string]any{"_id": d.ChunkID, "_index": s.Physical, "result": "updated"})
			return
		}
		if path == "/"+alias+"/_search" {
			if !idx.exists {
				http.NotFound(w, r)
				return
			}
			var req map[string]any
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				http.Error(w, "bad", 400)
				return
			}
			ids := []string{}
			for id := range idx.docs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			size := int(req["size"].(float64))
			after := ""
			if values, ok := req["search_after"].([]any); ok {
				after = values[0].(string)
			}
			hits := []any{}
			for _, id := range ids {
				if id <= after {
					continue
				}
				d := idx.docs[id]
				if m.Fault == "wrong-source-id" {
					d.SourceID = "wrong"
				}
				v := idx.currentRevision(id)
				hits = append(
					hits,
					map[string]any{
						"_id":           id,
						"_index":        s.Physical,
						"_score":        1,
						"_source":       d,
						"sort":          []string{id},
						"_version":      v.Version,
						"_seq_no":       v.SeqNo,
						"_primary_term": v.PrimaryTerm,
					},
				)
				if len(hits) == size {
					break
				}
			}
			total := len(ids)
			relation := "eq"
			failed := 0
			timedout := false
			switch m.Fault {
			case "truncated":
				if after != "" {
					hits = []any{}
				}
			case "gte":
				relation = "gte"
			case "partial":
				failed = 1
			case "timeout":
				timedout = true
			case "repeated-page":
				if after != "" {
					id := ids[0]
					hits = []any{
						map[string]any{"_id": id, "_index": s.Physical, "_source": idx.docs[id], "sort": []string{id}},
					}
				}
			}
			respond(
				w,
				map[string]any{
					"timed_out": timedout,
					"_shards":   map[string]int{"total": 1, "successful": 1 - failed, "failed": failed},
					"hits": map[string]any{
						"total": map[string]any{"value": total, "relation": relation},
						"hits":  hits,
					},
				},
			)
			return
		}
	}
	http.NotFound(w, r)
}

func TestOptInDisabledBeforeConfigOrSecretRead(t *testing.T) {
	for _, env := range []map[string]string{
		{},
		{"PRIMIND_OS02_OPT_IN": "OS-02:replay"},
		{"PRIMIND_OS02_OPT_IN": "true", "PRIMIND_OS02_CONFIG": "unreadable"},
	} {
		var names []string
		_, e := loadConfig("replay", func(k string) string { names = append(names, k); return env[k] })
		if !errors.Is(e, errNotRun) {
			t.Fatal(e)
		}
		for _, k := range names {
			if k != "PRIMIND_OS02_OPT_IN" && k != "PRIMIND_OS02_CONFIG" {
				t.Fatalf("read unrelated environment %s", k)
			}
		}
	}
}

func TestConfigRejectsUnsafeScopeBeforeNetwork(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"expired":          func(c *Config) { c.Expires = time.Now().Add(-time.Second) },
		"wildcard":         func(c *Config) { c.Replay.Alias = "*" },
		"empty-config":     func(c *Config) { c.Replay.Index = types.IndexConfig{} },
		"wrong-physical":   func(c *Config) { c.Replay.Physical += "_other" },
		"zero-dimension":   func(c *Config) { c.Replay.Dimension = 0 },
		"no-authorization": func(c *Config) { c.AuthorizationRef = "" },
		"no-quiescence":    func(c *Config) { c.QuiescentScope = false },
		"embedded-secret":  func(c *Config) { c.Endpoint = "http://u:p@127.0.0.1" },
		"endpoint-query":   func(c *Config) { c.Endpoint += "/?other=1" },
		"unbounded":        func(c *Config) { c.MaxSeconds = 0 },
		"zero-replicas":    func(c *Config) { c.Replay.Index.NumberOfReplicas = 0 },
		"new-mode":         func(c *Config) { c.Mode = "load-production" },
	} {
		t.Run(name, func(t *testing.T) {
			c := config()
			m := newMock(t, []Scope{*c.Replay}, true)
			c.Endpoint = m.server.URL
			mutate(&c)
			_, e := runReplay(context.Background(), c)
			if e == nil || m.Requests != 0 {
				t.Fatalf("error=%v requests=%d", e, m.Requests)
			}
		})
	}
}

func TestConfigDedicatedStrictJSON(t *testing.T) {
	c := config()
	b, _ := json.Marshal(c)
	path := filepath.Join(t.TempDir(), "config.json")
	if e := os.WriteFile(path, b, 0o600); e != nil {
		t.Fatal(e)
	}
	get := func(k string) string {
		if k == "PRIMIND_OS02_OPT_IN" {
			return "OS-02:replay"
		}
		if k == "PRIMIND_OS02_CONFIG" {
			return path
		}
		t.Fatalf("unexpected env %s", k)
		return ""
	}
	if _, e := loadConfig("replay", get); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, append(b, []byte(" {}")...), 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e := loadConfig("replay", get); e == nil {
		t.Fatal("trailing config accepted")
	}
}

func TestReplayFormalAdapterPreservesOriginalAndStops(t *testing.T) {
	for _, fault := range []string{"", "save403", "bulk429", "corruption", "alias-drift", "partial"} {
		t.Run(fault, func(t *testing.T) {
			c := config()
			s := *c.Replay
			m := newMock(t, []Scope{s}, true)
			d := original(s)
			m.indexes[s.Alias].docs[d.ChunkID] = d
			m.Fault = fault
			m.BadAt = 1
			c.Endpoint = m.server.URL
			r, e := runReplay(context.Background(), c)
			m.mu.Lock()
			defer m.mu.Unlock()
			if fault == "" {
				if e != nil || r.Status != "PASS" || len(r.Steps) != 5 || m.Writes != 4 || m.Bulks != 2 {
					t.Fatalf("result=%+v error=%v writes=%d", r, e, m.Writes)
				}
				if !reflect.DeepEqual(m.indexes[s.Alias].docs[d.ChunkID], d) {
					t.Fatal("original fields/vector changed")
				}
			} else {
				if e == nil || r.Status == "PASS" {
					t.Fatal("failure accepted")
				}
				limit := 1
				if fault == "bulk429" {
					limit = 3
				}
				if fault == "partial" {
					limit = 0
				}
				if m.Writes != limit {
					t.Fatalf("continued after failure: writes %d expected %d", m.Writes, limit)
				}
			}
			if r.Budget.InFlight != 0 || r.Budget.Writing != 0 || r.Budget.Queued != 0 {
				t.Fatalf("leaked budget %+v", r.Budget)
			}
		})
	}
}

func TestReplayMissingVectorNeverFallsBackToKeyword(t *testing.T) {
	c := config()
	s := *c.Replay
	m := newMock(t, []Scope{s}, true)
	d := original(s)
	d.Embedding = nil
	m.indexes[s.Alias].docs[d.ChunkID] = d
	c.Endpoint = m.server.URL
	if _, e := runReplay(context.Background(), c); e == nil || m.Writes != 0 {
		t.Fatalf("error=%v writes=%d", e, m.Writes)
	}
}

func TestFullPaginationNotTopKOrCountOnly(t *testing.T) {
	for _, fault := range []string{"", "truncated", "gte", "partial", "timeout", "repeated-page", "wrong-source-id"} {
		t.Run(fault, func(t *testing.T) {
			c := config()
			s := *c.Replay
			s.ChunkID = ""
			s.Namespace = "os02perf_test"
			m := newMock(t, []Scope{s}, true)
			want := map[string]Document{}
			for n := 0; n < 7; n++ {
				d := synthetic(s, "golden-seed", n)
				want[d.ChunkID] = d
				m.indexes[s.Alias].docs[d.ChunkID] = d
			}
			m.Fault = fault
			c.Endpoint = m.server.URL
			budget, _ := adapter.NewRequestBudget(4, 2, 8)
			sess, e := openSession(context.Background(), c, s, budget, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer adapter.CloseIdleConnections(sess.client)
			got, e := sess.scan(context.Background(), s, s.Alias, s.Dimension, false, false)
			if e == nil {
				e = reconcile(want, got)
			}
			if (fault == "") != (e == nil) {
				t.Fatalf("fault=%s count=%d err=%v", fault, len(got), e)
			}
		})
	}
}

func TestReconcileIndependentFieldsAndDuplicateIdentity(t *testing.T) {
	s := scope()
	d := original(s)
	for name, mutate := range map[string]func(*Document){
		"tag":         func(x *Document) { x.TagID = "x" },
		"enabled":     func(x *Document) { x.Enabled = true },
		"recommended": func(x *Document) { x.Recommended = false },
		"source":      func(x *Document) { x.SourceID = x.ChunkID },
		"vector":      func(x *Document) { x.Embedding = []float32{.2, -.5, .75} },
		"content":     func(x *Document) { x.Content += "x" },
	} {
		t.Run(name, func(t *testing.T) {
			x := d
			mutate(&x)
			if reconcile(map[string]Document{d.ChunkID: d}, map[string]Document{d.ChunkID: x}) == nil {
				t.Fatal("same-count corruption accepted")
			}
		})
	}
	if reconcile(map[string]Document{d.ChunkID: d}, map[string]Document{"other": d}) == nil {
		t.Fatal("same-count replaced identity accepted")
	}
	raw, _ := json.Marshal(d)
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	obj["future_field"] = "must not be silently dropped"
	raw, _ = json.Marshal(obj)
	if _, e := decodeDocument(raw, s.Dimension); e == nil {
		t.Fatal("unknown source field accepted")
	}
}

func TestDispatchCancellationStopsNewWritesAndJoins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	jobs := make([]job, 100)
	for i := range jobs {
		jobs[i].at = time.Now()
	}
	var calls, active, peak atomic.Int32
	e := dispatch(ctx, cancel, 2, jobs, func(c context.Context, _ job) error {
		call := calls.Add(1)
		a := active.Add(1)
		defer active.Add(-1)
		for {
			if p := peak.Load(); a <= p || peak.CompareAndSwap(p, a) {
				break
			}
		}
		if call == 1 {
			return errors.New("simulated 429")
		}
		<-c.Done()
		return c.Err()
	})
	if e == nil || calls.Load() > 2 || active.Load() != 0 || peak.Load() > 2 {
		t.Fatalf("error=%v calls=%d active=%d peak=%d", e, calls.Load(), active.Load(), peak.Load())
	}
}

func TestApplicationOracleAcceptsContextExpansionRejectsLeaks(t *testing.T) {
	a := Application{KnowledgeBaseID: "kb", KnowledgeAID: "a", EvidenceAChunkIDs: []string{"child"}}
	good := &types.SearchResult{ID: "parent", KnowledgeBaseID: "kb", KnowledgeID: "a", SubChunkID: []string{"child"}}
	if _, _, e := oracleRetrieval(a, []*types.SearchResult{good}); e != nil {
		t.Fatal(e)
	}
	for _, results := range [][]*types.SearchResult{{good, good}, {{
		ID: "other", KnowledgeBaseID: "other-kb",
		KnowledgeID: "a",
	}}, {{ID: "child", KnowledgeBaseID: "kb", KnowledgeID: "b"}}, nil} {
		if _, _, e := oracleRetrieval(a, results); e == nil {
			t.Fatal("invalid oracle accepted")
		}
	}
}

func TestApplicationRequestsFreezeThreeModes(t *testing.T) {
	seen := []types.SearchParams{}
	a := Application{
		KnowledgeBaseID:   "kb",
		KnowledgeAID:      "a",
		EvidenceAChunkIDs: []string{"child"},
		MatchCount:        5,
		VectorThreshold:   .4,
		KeywordThreshold:  0,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/knowledge-bases/kb/hybrid-search" ||
			r.Header.Get("Authorization") != "Bearer local-test" {
			http.Error(w, "scope", http.StatusForbidden)
			return
		}
		var p types.SearchParams
		_ = json.NewDecoder(r.Body).Decode(&p)
		seen = append(seen, p)
		respond(
			w,
			map[string]any{
				"success": true,
				"data":    []any{map[string]any{"id": "child", "knowledge_id": "a", "knowledge_base_id": "kb"}},
			},
		)
	}))
	defer srv.Close()
	a.Endpoint = srv.URL
	p := appProbe{client: srv.Client(), config: a, token: "local-test"}
	for _, q := range fixedQueries[:3] {
		if _, e := p.run(context.Background(), q); e != nil {
			t.Fatal(e)
		}
	}
	for i, x := range seen {
		q := fixedQueries[i]
		if x.QueryText != q.Text || x.DisableVectorMatch != q.DisableVector ||
			x.DisableKeywordsMatch != q.DisableKeywords ||
			len(x.QueryEmbedding) != 0 ||
			!reflect.DeepEqual(x.KnowledgeBaseIDs, []string{"kb"}) ||
			x.Rerank != nil {
			t.Fatal("frozen request drift")
		}
	}
}

func TestApplicationProbeOwnsAndClosesPool(t *testing.T) {
	secutils.SetSSRFWhitelistFromRaw("127.0.0.1")
	defer secutils.SetSSRFWhitelistFromRaw("")
	t.Setenv("OS02_SECRET_APP", "local-token")
	var live, opened atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(
			w,
			map[string]any{
				"success": true,
				"data":    []any{map[string]any{"id": "a-chunk", "knowledge_id": "a", "knowledge_base_id": "kb"}},
			},
		)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			live.Add(1)
			opened.Add(1)
		}
		if s == http.StateClosed || s == http.StateHijacked {
			live.Add(-1)
		}
	}
	srv.Start()
	defer srv.Close()
	p, e := newAppProbe(
		Application{
			Endpoint:          srv.URL,
			TokenEnv:          "OS02_SECRET_APP",
			KnowledgeBaseID:   "kb",
			KnowledgeAID:      "a",
			EvidenceAChunkIDs: []string{"a-chunk"},
			MatchCount:        5,
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	defer p.close()
	for i := 0; i < 2; i++ {
		if _, e = p.run(context.Background(), fixedQueries[0]); e != nil {
			t.Fatal(e)
		}
	}
	if opened.Load() != 1 {
		t.Fatalf("pool not reused: %d connections", opened.Load())
	}
	p.close()
	deadline := time.Now().Add(time.Second)
	for live.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if live.Load() != 0 {
		t.Fatal("owned pool leaked")
	}
}

func TestReplayRejectsExistingKeywordCopyWithoutWriting(t *testing.T) {
	c := config()
	s := *c.Replay
	k := s
	k.Dimension = 0
	k.Alias = s.keywordIndex()
	k.Physical = k.Alias
	m := newMock(t, []Scope{s, k}, true)
	d := original(s)
	m.indexes[s.Alias].docs[d.ChunkID] = d
	d.Embedding = nil
	m.indexes[k.Alias].docs[d.ChunkID] = d
	c.Endpoint = m.server.URL
	if _, e := runReplay(context.Background(), c); e == nil || m.Writes != 0 {
		t.Fatalf("keyword copy accepted error=%v", e)
	}
}
