package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/primind/primind/internal/types"
)

func managedTestRepo(t *testing.T, url, store string, opts ...ClientOption) *Repository {
	t.Helper()
	c, err := NewOpenSearchClient(&types.ConnectionConfig{Addr: url}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { CloseIdleConnections(c) })
	return &Repository{
		client:        c,
		baseIndex:     store,
		cfg:           internalCfg{shards: 1, replicas: 0, knnEngine: "lucene", hnswM: 16, hnswEFConstruction: 100},
		keywordsReady: true,
	}
}

func echoBulk(w http.ResponseWriter, r *http.Request) {
	var items []any
	dec := json.NewDecoder(r.Body)
	for {
		var action map[string]struct {
			ID string `json:"_id"`
		}
		if err := dec.Decode(&action); err == io.EOF {
			break
		} else if err != nil {
			w.WriteHeader(400)
			return
		}
		var doc any
		if err := dec.Decode(&doc); err != nil {
			w.WriteHeader(400)
			return
		}
		items = append(items, map[string]any{"index": map[string]any{"_id": action["index"].ID, "status": 201}})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": false, "items": items})
}

func awaitCondition(t *testing.T, f func() bool) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for !f() {
		if time.Now().After(until) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOS01_InitializationReturnsAttemptErrorAndRecovers(t *testing.T) {
	var healthy atomic.Bool
	var puts atomic.Int32
	r, ts := newTestRepo(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodHead {
			w.WriteHeader(404)
			return
		}
		if strings.HasSuffix(req.URL.Path, "_v1") {
			puts.Add(1)
			if !healthy.Load() {
				w.WriteHeader(503)
				_, _ = io.WriteString(w, `{"error":{"type":"server_error"},"status":503}`)
				return
			}
		}
		_, _ = io.WriteString(w, `{"acknowledged":true}`)
	})
	defer ts.Close()
	if err := r.ensureReady(context.Background(), 3); err == nil {
		t.Error("failed first initialization returned nil")
	}
	first := puts.Load()
	if first != 4 {
		t.Errorf("SDK baseline expected four physical attempts, got %d", first)
	}
	healthy.Store(true)
	if err := r.ensureReady(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if puts.Load() <= first {
		t.Fatal("recovery did not retry creation")
	}
}

func TestOS01_BulkActualBytesRejectedBeforeSend(t *testing.T) {
	var sent atomic.Int32
	r, ts := newTestRepo(t, func(w http.ResponseWriter, req *http.Request) {
		sent.Add(1)
		_, _ = io.Copy(io.Discard, req.Body)
		_, _ = io.WriteString(w, `{"errors":false,"items":[{"index":{"_id":"c1","status":201}}]}`)
	})
	defer ts.Close()
	r.keywordsReady = true
	// Estimate is 1124 bytes; JSON escaping expands this to over 12 MiB.
	err := r.BatchSave(
		context.Background(),
		[]*types.IndexInfo{{ChunkID: "c1", Content: strings.Repeat("\x00", 2<<20)}},
		nil,
	)
	if !errors.Is(err, ErrBatchTooLarge) {
		t.Errorf("want ErrBatchTooLarge, got %v", err)
	}
	if sent.Load() != 0 {
		t.Errorf("oversized bulk reached server: %d", sent.Load())
	}
}

func TestOS01_BulkRejectsUnconfirmedResponses(t *testing.T) {
	for name, body := range map[string]string{
		"missing":              `{"errors":false,"items":[]}`,
		"extra":                `{"errors":false,"items":[{"index":{"_id":"c1","status":201}},{"index":{"_id":"c2","status":201}}]}`, //nolint:lll // bulk response fixture
		"wrong_id":             `{"errors":false,"items":[{"index":{"_id":"other","status":201}}]}`,
		"false_with_failure":   `{"errors":false,"items":[{"index":{"_id":"c1","status":400,"error":{"type":"mapper_parsing_exception"}}}]}`, //nolint:lll // bulk response fixture
		"true_without_failure": `{"errors":true,"items":[{"index":{"_id":"c1","status":201}}]}`,
		"invalid_status":       `{"errors":false,"items":[{"index":{"_id":"c1","status":0}}]}`,
		"truncated":            `{"errors":false,"items":[`,
	} {
		t.Run(name, func(t *testing.T) {
			r, ts := newTestRepo(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			defer ts.Close()
			r.keywordsReady = true
			if err := r.BatchSave(context.Background(), []*types.IndexInfo{{ChunkID: "c1"}}, nil); err == nil {
				t.Fatal("unconfirmed response returned success")
			}
		})
	}
}

func TestOS01_DeleteRequiresCompletion(t *testing.T) {
	for name, body := range map[string]string{
		"timeout":       `{"total":1,"deleted":1,"timed_out":true,"failures":[]}`,
		"partial":       `{"total":2,"deleted":1,"failures":[{"cause":{"type":"mapper_parsing_exception"}}]}`,
		"missing_count": `{"failures":[]}`,
		"conflict":      `{"total":1,"deleted":0,"version_conflicts":1,"failures":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			r, ts := newTestRepo(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			defer ts.Close()
			if err := r.deleteByTerms(context.Background(), "test", "chunk_id", []string{"c1"}); err == nil {
				t.Fatal("incomplete delete returned success")
			}
		})
	}
}

func TestOS01_ProbeReleasesOwnedPool(t *testing.T) {
	var connections atomic.Int32
	ts := httptest.NewUnstartedServer(clusterHandler("elasticsearch", "8.10.4", nil))
	ts.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
		if s == http.StateClosed {
			connections.Add(-1)
		}
	}
	ts.Start()
	defer ts.Close()
	if err := TestConnection(context.Background(), &types.ConnectionConfig{Addr: ts.URL}); err == nil {
		t.Fatal("expected rejected cluster")
	}
	until := time.Now().Add(300 * time.Millisecond)
	for connections.Load() != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if connections.Load() != 0 {
		t.Fatalf("probe leaked %d idle connections", connections.Load())
	}
}

func TestOS01_TransportHasFiniteTotalConnections(t *testing.T) {
	tr := buildHTTPTransport(false)
	defer tr.CloseIdleConnections()
	if tr.MaxConnsPerHost <= 0 || tr.MaxIdleConns <= 0 {
		t.Fatal("total/idle connection limits are unbounded")
	}
}

func TestOS01_DefaultBudgetSharedAcrossClients(t *testing.T) {
	var active, peak, queries atomic.Int32
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "_search") {
			queries.Add(1)
			_, _ = io.WriteString(w, `{"hits":{"hits":[]}}`)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `{"_id":"c1","result":"created"}`)
	}))
	defer ts.Close()
	clients := make([]*osapi.Client, 2)
	for i := range clients {
		c, err := NewOpenSearchClient(&types.ConnectionConfig{Addr: ts.URL})
		if err != nil {
			t.Fatal(err)
		}
		clients[i] = c
	}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := clients[i%2].Index(
				ctx,
				osapi.IndexReq{Index: "test", DocumentID: "c1", Body: strings.NewReader(`{}`)},
			)
			if err != nil {
				t.Errorf("index: %v", err)
			}
		}(i)
	}
	time.Sleep(100 * time.Millisecond)
	for _, c := range clients {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := c.Search(ctx, &osapi.SearchReq{Indices: []string{"test"}})
		cancel()
		if err != nil {
			t.Errorf("query could not progress: %v", err)
		}
	}
	close(release)
	wg.Wait()
	t.Logf("two clients: write peak=%d queries=%d", peak.Load(), queries.Load())
	if peak.Load() > 12 {
		t.Errorf("combined writes exceeded default 12: %d", peak.Load())
	}
	if queries.Load() != 2 {
		t.Error("reserved query capacity unavailable")
	}
}

func TestOS01_BulkWireBoundariesAndRouting(t *testing.T) {
	cases := []struct {
		name    string
		n, dim  int
		content string
		reject  bool
	}{
		{"empty", 0, 0, "", false},
		{"1000", 1000, 0, "small", false},
		{"1001", 1001, 0, "small", true},
		{"unicode_escape", 2, 3, "中文\"\n<>&\u2028", false},
		{"long_floats", 1, 16000, "vector", false},
		{"single_oversize", 1, 0, strings.Repeat("\x00", 2<<20), true},
		{"aggregate_oversize", 2, 0, strings.Repeat("\x00", 1<<20), true},
		{"estimate_cap", 140, 16000, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			var captured []byte
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodHead {
					return
				}
				if req.URL.Path != "/_bulk" {
					t.Errorf("unexpected path %s", req.URL.Path)
					return
				}
				sends.Add(1)
				captured, _ = io.ReadAll(req.Body)
				if req.URL.Query().Get("refresh") != "" {
					t.Error("bulk forced refresh")
				}
				req.Body = io.NopCloser(bytes.NewReader(captured))
				echoBulk(w, req)
			}))
			defer ts.Close()
			repo := managedTestRepo(t, ts.URL, "store_a")
			infos := make([]*types.IndexInfo, tc.n)
			embs := map[string][]float32{}
			enabled := map[string]bool{}
			for i := range infos {
				src := fmt.Sprintf("src_%d", i)
				id := fmt.Sprintf("chunk_%d", i)
				infos[i] = &types.IndexInfo{
					ChunkID:         id,
					SourceID:        src,
					KnowledgeID:     "doc_os_local_a",
					KnowledgeBaseID: "kb_os_local_a",
					Content:         tc.content,
					IsEnabled:       true,
				}
				enabled[id] = false
				if tc.dim > 0 {
					v := make([]float32, tc.dim)
					for j := range v {
						v[j] = []float32{math.SmallestNonzeroFloat32, math.MaxFloat32, 0.123456789}[j%3]
					}
					embs[src] = v
				}
			}
			var observation WriteObservation
			repo.writeObserver = func(o WriteObservation) { observation = o }
			err := repo.BatchSave(
				context.Background(),
				infos,
				map[string]any{"embedding": embs, "chunk_enabled": enabled},
			)
			if tc.reject {
				if !errors.Is(err, ErrBatchTooLarge) {
					t.Fatalf("want batch cap, got %v", err)
				}
				if sends.Load() != 0 {
					t.Fatal("oversize sent")
				}
				if observation.NotSent != tc.n {
					t.Fatalf("not sent=%d", observation.NotSent)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.n == 0 {
				if sends.Load() != 0 {
					t.Fatal("empty sent")
				}
				return
			}
			if sends.Load() != 1 || len(captured) > maxBulkBytes || !bytes.HasSuffix(captured, []byte{'\n'}) {
				t.Fatal("invalid bulk shape")
			}
			lines := bytes.Split(bytes.TrimSuffix(captured, []byte{'\n'}), []byte{'\n'})
			if len(lines) != 2*tc.n {
				t.Fatalf("line count %d", len(lines))
			}
			for i := range infos {
				var action map[string]map[string]string
				var doc map[string]any
				if json.Unmarshal(lines[2*i], &action) != nil || json.Unmarshal(lines[2*i+1], &doc) != nil {
					t.Fatal("invalid NDJSON")
				}
				wantIndex := "store_a_keywords"
				if tc.dim > 0 {
					wantIndex = fmt.Sprintf("store_a_%d", tc.dim)
				}
				if action["index"]["_id"] != infos[i].ChunkID || action["index"]["_index"] != wantIndex ||
					doc["source_id"] != infos[i].SourceID ||
					doc["is_enabled"] != false ||
					doc["content"] != tc.content {
					t.Fatal("mapping changed")
				}
				if tc.dim > 0 && len(doc["embedding"].([]any)) != tc.dim {
					t.Fatal("vector truncated")
				}
			}
			if observation.Bytes != len(captured) || observation.Confirmed != tc.n || observation.Attempts != 1 {
				t.Fatalf("incorrect observation: %+v", observation)
			}
			t.Logf("items=%d bytes=%d attempts=%d", tc.n, len(captured), observation.Attempts)
		})
	}
}

func TestOS01_JSONSizeExactAndBounded(t *testing.T) {
	for _, s := range []string{"", "中文\"\n\r\t\b\f\\<>&\u2028\u2029", string([]byte{0xff, 0xc0, 0, 1, 0x7f})} {
		value := map[string]any{
			"text":  s,
			"count": 3,
			"on":    false,
			"v":     []float32{math.SmallestNonzeroFloat32, math.MaxFloat32},
		}
		actual, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		n, err := jsonSize(value, len(actual))
		if err != nil || n != len(actual) {
			unlimited, _ := jsonSize(value, maxBulkBytes)
			t.Fatalf("size got %d unlimited=%d want %d: %v encoded=%q", n, unlimited, len(actual), err, actual)
		}
		if _, err = jsonSize(value, len(actual)-1); !errors.Is(err, ErrBatchTooLarge) {
			t.Fatalf("missing byte limit: %v", err)
		}
	}
}

func TestOS01_BulkOutcomeAccounting(t *testing.T) {
	cases := []struct {
		name, body               string
		success, failed, unknown int
	}{
		{
			"all",
			`{"errors":false,"items":[{"index":{"_id":"a","status":201}},{"index":{"_id":"b","status":200}},{"index":{"_id":"c","status":201}}]}`, //nolint:lll // bulk response fixture
			3,
			0,
			0,
		},
		{
			"mixed",
			`{"errors":true,"items":[{"index":{"_id":"a","status":201}},{"index":{"_id":"b","status":400,"error":{"type":"mapper_parsing_exception","reason":"PRIVATE_BODY"}}},{"index":{"_id":"c","status":429,"error":{"type":"rejected"}}}]}`, //nolint:lll // bulk response fixture
			1,
			2,
			0,
		},
		{"missing", `{"errors":false,"items":[{"index":{"_id":"a","status":201}}]}`, 1, 0, 2},
		{
			"wrong_order",
			`{"errors":false,"items":[{"index":{"_id":"b","status":201}},{"index":{"_id":"a","status":201}},{"index":{"_id":"c","status":201}}]}`, //nolint:lll // bulk response fixture
			1,
			0,
			2,
		},
		{
			"status_error_conflict",
			`{"errors":true,"items":[{"index":{"_id":"a","status":201,"error":{"type":"bad"}}},{"index":{"_id":"b","status":0}},{"index":{"_id":"c","status":201}}]}`, //nolint:lll // bulk response fixture
			1,
			0,
			2,
		},
		{"truncated", `{"errors":false,"items":[`, 0, 0, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			ts := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, _ *http.Request) { sends.Add(1); _, _ = io.WriteString(w, tc.body) },
				),
			)
			defer ts.Close()
			repo := managedTestRepo(t, ts.URL, "store_a")
			infos := []*types.IndexInfo{{ChunkID: "a"}, {ChunkID: "b"}, {ChunkID: "c"}}
			var obs WriteObservation
			repo.writeObserver = func(o WriteObservation) { obs = o }
			err := repo.BatchSave(context.Background(), infos, nil)
			if tc.name == "all" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var be *BulkWriteError
				if !errors.As(err, &be) {
					t.Fatalf("missing outcomes: %v", err)
				}
				if strings.Contains(err.Error(), "PRIVATE_BODY") {
					t.Fatal("leaked document")
				}
			}
			if obs.Confirmed != tc.success || obs.Failed != tc.failed || obs.Unknown != tc.unknown ||
				obs.NotSent != 0 ||
				sends.Load() != 1 {
				t.Fatalf("unexpected counts %+v requests=%d", obs, sends.Load())
			}
			t.Logf(
				"confirmed=%d failed=%d unknown=%d attempts=%d",
				obs.Confirmed,
				obs.Failed,
				obs.Unknown,
				obs.Attempts,
			)
		})
	}
}

func TestOS01_BudgetWaitCancelSaturationAndQueryReserve(t *testing.T) {
	budget, err := NewRequestBudget(3, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var writes, queries atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "_search") {
			queries.Add(1)
			_, _ = io.WriteString(w, `{"hits":{"hits":[]}}`)
			return
		}
		writes.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		if strings.HasSuffix(r.URL.Path, "_bulk") {
			echoBulk(w, r)
		} else {
			_, _ = io.WriteString(w, `{"_id":"c1","result":"created"}`)
		}
	}))
	defer ts.Close()
	repos := []*Repository{
		managedTestRepo(t, ts.URL, "store_a", WithRequestBudget(budget)),
		managedTestRepo(t, ts.URL, "store_b", WithRequestBudget(budget)),
	}
	done := make(chan error, 4)
	for _, r := range repos {
		go func(r *Repository) {
			done <- r.BatchSave(context.Background(), []*types.IndexInfo{{ChunkID: "c1"}}, nil)
		}(r)
	}
	awaitCondition(t, func() bool { return writes.Load() == 2 })
	_, err = repos[0].Retrieve(
		context.Background(),
		types.RetrieveParams{RetrieverType: types.KeywordsRetrieverType, Query: "query"},
	)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	go func() { done <- repos[0].BatchSave(cancelCtx, []*types.IndexInfo{{ChunkID: "c2"}}, nil) }()
	go func() { done <- repos[1].Save(context.Background(), &types.IndexInfo{ChunkID: "c3"}, nil) }()
	awaitCondition(t, func() bool { return budget.Snapshot().Queued == 2 })
	err = repos[0].BatchSave(context.Background(), []*types.IndexInfo{{ChunkID: "rejected"}}, nil)
	var be *BulkWriteError
	if !errors.Is(err, ErrRequestBudgetFull) || !errors.As(err, &be) || be.Result.NotSent != 1 || be.Result.Bytes != 0 {
		close(release)
		t.Fatalf("queue overflow: %v", err)
	}
	cancel()
	err = <-done
	if !errors.Is(err, context.Canceled) || !errors.As(err, &be) || be.Result.NotSent != 1 {
		close(release)
		t.Fatalf("wait cancellation: %v", err)
	}
	close(release)
	for i := 0; i < 3; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	snap := budget.Snapshot()
	if snap.InFlight != 0 || snap.Queued != 0 || snap.Peak > 3 || snap.WritePeak > 2 || snap.QueuePeak > 2 ||
		snap.Rejected != 1 ||
		queries.Load() != 1 {
		t.Fatalf("budget invariant: %+v", snap)
	}
	t.Logf("two stores budget snapshot: %+v", snap)
}

func TestOS01_ManagedConnectionsReuseAndCap(t *testing.T) {
	var open, peak, total atomic.Int32
	release := make(chan struct{})
	var blocking atomic.Bool
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blocking.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, `{"version":{"distribution":"opensearch","number":"2.19.7"}}`)
	}))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			total.Add(1)
			n := open.Add(1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
		}
		if state == http.StateClosed {
			open.Add(-1)
		}
	}
	ts.Start()
	defer ts.Close()
	options := DefaultRuntimeOptions()
	options.MaxConnsPerHost = 2
	options.MaxIdleConnsPerHost = 2
	options.MaxIdleConns = 2
	repo := managedTestRepo(t, ts.URL, "store_a", WithRuntimeOptions(options))
	for i := 0; i < 3; i++ {
		reused := false
		ctx := httptrace.WithClientTrace(
			context.Background(),
			&httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }},
		)
		if err := probeVersion(ctx, repo.client); err != nil {
			t.Fatal(err)
		}
		if i > 0 && !reused {
			t.Fatal("connection was not reused")
		}
	}
	if total.Load() != 1 {
		t.Fatalf("sequential connections=%d", total.Load())
	}
	blocking.Store(true)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := probeVersion(context.Background(), repo.client); err != nil {
				t.Error(err)
			}
		}()
	}
	awaitCondition(t, func() bool { return open.Load() == 2 })
	close(release)
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("connection peak=%d", peak.Load())
	}
	CloseIdleConnections(repo.client)
	awaitCondition(t, func() bool { return open.Load() == 0 })
	t.Logf("sequential reuse=true connection peak=%d total created=%d", peak.Load(), total.Load())
}

func TestOS01_DeadlineCoversHeadersBodyAndUpload(t *testing.T) {
	for _, phase := range []string{"headers", "body", "upload"} {
		t.Run(phase, func(t *testing.T) {
			release := make(chan struct{})
			var requests atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if phase == "body" {
					w.Header().Set("Content-Length", "100000")
					_, _ = io.WriteString(w, `{"`)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer ts.Close()
			defer close(release)
			opts := DefaultRuntimeOptions()
			opts.RequestTimeout = 80 * time.Millisecond
			budget, _ := NewRequestBudget(2, 1, 2)
			repo := managedTestRepo(t, ts.URL, "store_a", WithRuntimeOptions(opts), WithRequestBudget(budget))
			started := time.Now()
			var err error
			if phase == "upload" {
				_, err = repo.client.Index(
					context.Background(),
					osapi.IndexReq{
						Index:      "test",
						DocumentID: "c1",
						Body:       strings.NewReader(strings.Repeat("x", 9<<20)),
					},
				)
			} else {
				_, err = repo.client.Info(context.Background(), nil)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline phase=%s: %v", phase, err)
			}
			if requests.Load() != 1 {
				t.Fatalf("scenario did not exercise a real HTTP attempt: %d", requests.Load())
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("deadline did not bound request")
			}
			if snap := budget.Snapshot(); snap.InFlight != 0 || snap.Queued != 0 {
				t.Fatalf("leaked permits: %+v", snap)
			}
		})
	}
}

func TestOS01_SDKRetryBoundsAndUnknown(t *testing.T) {
	for _, status := range []int{400, 403, 429, 500, 502, 503, 504, 0} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			var idsMu sync.Mutex
			var ids []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var action map[string]map[string]string
				_ = json.NewDecoder(r.Body).Decode(&action)
				idsMu.Lock()
				ids = append(ids, action["index"]["_id"])
				idsMu.Unlock()
				if status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"error":{"type":"test_error","reason":"PRIVATE_BODY"},"status":%d}`, status)
			}))
			defer ts.Close()
			budget, _ := NewRequestBudget(2, 1, 2)
			repo := managedTestRepo(t, ts.URL, "store_a", WithRequestBudget(budget))
			err := repo.BatchSave(context.Background(), []*types.IndexInfo{{ChunkID: "stable_id"}}, nil)
			var be *BulkWriteError
			if !errors.As(err, &be) || be.Result.Unknown != 1 {
				t.Fatalf("unknown write: %v", err)
			}
			want := int32(1)
			if status == 0 || status == 502 || status == 503 || status == 504 {
				want = 4
			}
			if calls.Load() != want || budget.Snapshot().Attempts != int64(want) {
				t.Fatalf("HTTP attempts=%d snapshot=%+v", calls.Load(), budget.Snapshot())
			}
			if strings.Contains(err.Error(), "PRIVATE_BODY") {
				t.Fatal("unsafe error summary")
			}
			idsMu.Lock()
			defer idsMu.Unlock()
			for _, id := range ids {
				if id != "stable_id" {
					t.Fatal("replay changed identity")
				}
			}
			if budget.Snapshot().InFlight != 0 {
				t.Fatal("permit leak")
			}
			t.Logf("status=%d HTTP attempts=%d unknown=%d", status, calls.Load(), be.Result.Unknown)
		})
	}
}

func TestOS01_CancelBackoffAndBoundResponse(t *testing.T) {
	t.Run("backoff", func(t *testing.T) {
		got := make(chan struct{}, 1)
		var requests atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `{"status":503,"error":{"type":"server_error"}}`)
			select {
			case got <- struct{}{}:
			default:
			}
		}))
		defer ts.Close()
		budget, _ := NewRequestBudget(2, 1, 2)
		repo := managedTestRepo(t, ts.URL, "store_a", WithRequestBudget(budget))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- repo.Save(ctx, &types.IndexInfo{ChunkID: "a"}, nil) }()
		<-got
		time.Sleep(20 * time.Millisecond)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		if requests.Load() != 1 || budget.Snapshot().InFlight != 0 {
			t.Fatalf("retried or leaked: %d %+v", requests.Load(), budget.Snapshot())
		}
	})
	t.Run("oversize", func(t *testing.T) {
		ts := httptest.NewServer(
			http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", 128<<10)) },
			),
		)
		defer ts.Close()
		opts := DefaultRuntimeOptions()
		opts.MaxResponseBytes = 1024
		budget, _ := NewRequestBudget(2, 1, 2)
		repo := managedTestRepo(t, ts.URL, "store_a", WithRuntimeOptions(opts), WithRequestBudget(budget))
		_, err := repo.client.Info(context.Background(), nil)
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("response cap: %v", err)
		}
		snap := budget.Snapshot()
		if snap.InFlight != 0 || snap.ResponseBytes != 1025 || snap.Attempts != 1 {
			t.Fatalf("unbounded response: %+v", snap)
		}
	})
	t.Run("partial_response", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n{}")
			_ = rw.Flush()
		}))
		defer ts.Close()
		repo := managedTestRepo(t, ts.URL, "store_a")
		err := repo.BatchSave(context.Background(), []*types.IndexInfo{{ChunkID: "a"}}, nil)
		var be *BulkWriteError
		if !errors.As(err, &be) || be.Result.Unknown != 1 || be.Result.Attempts != 1 {
			t.Fatalf("partial response outcome: %v", err)
		}
	})
}

func TestRetrieveFiltersLargeUnusedSourceFields(t *testing.T) {
	embedding := make([]float32, 4096)
	embedding[0] = 1
	want := types.IndexWithScore{
		ID:              "chunk-1",
		ChunkID:         "chunk-1",
		KnowledgeID:     "document-1",
		KnowledgeBaseID: "knowledge-base-1",
		SourceID:        "source-1",
		SourceType:      types.SummarySourceType,
		TagID:           "tag-1",
		Content:         "Policy text used by the answer and its citation.",
		IsEnabled:       true,
	}
	source := map[string]any{
		"chunk_id":          want.ChunkID,
		"knowledge_id":      want.KnowledgeID,
		"knowledge_base_id": want.KnowledgeBaseID,
		"source_id":         want.SourceID,
		"source_type":       want.SourceType,
		"tag_id":            want.TagID,
		"content":           want.Content,
		"is_enabled":        want.IsEnabled,
		"embedding":         embedding,
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		returnedSource := source
		if includes := r.URL.Query().Get("_source_includes"); includes != "" {
			fields := strings.Split(includes, ",")
			returnedSource = make(map[string]any, len(fields))
			for _, field := range fields {
				if value, ok := source[field]; ok {
					returnedSource[field] = value
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{
			"hits": []map[string]any{{"_id": want.ID, "_score": 0.75, "_source": returnedSource}},
		}})
	}))
	defer ts.Close()

	opts := DefaultRuntimeOptions()
	opts.MaxResponseBytes = 1024
	repo := managedTestRepo(t, ts.URL, "store_a", WithRuntimeOptions(opts))
	ready := make(chan struct{})
	close(ready)
	repo.initializations = map[int]*indexInitialization{len(embedding): {done: ready}}

	for _, tc := range []struct {
		retriever types.RetrieverType
		matchType types.MatchType
		score     float64
	}{
		{types.VectorRetrieverType, types.MatchTypeEmbedding, 0.5},
		{types.KeywordsRetrieverType, types.MatchTypeKeywords, 0.75},
	} {
		t.Run(string(tc.retriever), func(t *testing.T) {
			results, err := repo.Retrieve(context.Background(), types.RetrieveParams{
				Query:            "policy",
				Embedding:        embedding,
				KnowledgeBaseIDs: []string{want.KnowledgeBaseID},
				TopK:             5,
				RetrieverType:    tc.retriever,
			})
			if err != nil {
				t.Fatalf("retrieve: %v", err)
			}
			if len(results) != 1 || len(results[0].Results) != 1 {
				t.Fatalf("expected one result with one hit, got %+v", results)
			}
			expected := want
			expected.MatchType = tc.matchType
			expected.Score = tc.score
			if got := *results[0].Results[0]; got != expected {
				t.Fatalf("hit = %+v, want %+v", got, expected)
			}
		})
	}
}

func TestOS01_ReviewSafeErrorsAndZeroBudget(t *testing.T) {
	t.Run("bounded by-query errors", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"failures": []any{map[string]any{
			"id": "PRIVATE document identifier", "cause": map[string]any{
				"type":   strings.Repeat("PRIVATE", 1000),
				"reason": "PRIVATE content",
			},
		}}})
		err := inspectByQueryResponse(bytes.NewReader(body))
		if err == nil || len(err.Error()) > 256 || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("by-query error must contain only bounded safe failure types")
		}
	})
	t.Run("zero budget rejected", func(t *testing.T) {
		client, err := NewOpenSearchClient(
			&types.ConnectionConfig{Addr: "http://localhost:1"},
			WithRequestBudget(&RequestBudget{}),
		)
		if client != nil {
			CloseIdleConnections(client)
		}
		if !errors.Is(err, ErrConfigInvalid) {
			t.Fatal("uninitialized budget must be rejected at construction")
		}
	})
}

func TestOS01_ParametersVisibilityAndReplicaProjection(t *testing.T) {
	defaults := DefaultRuntimeOptions()
	for _, change := range []func(*RuntimeOptions){
		func(o *RuntimeOptions) { o.RequestTimeout = 0 },
		func(o *RuntimeOptions) { o.MaxConnsPerHost = -1 }, func(o *RuntimeOptions) { o.MaxResponseBytes = 0 },
		func(o *RuntimeOptions) { o.MaxIdleConns = 0 }, func(o *RuntimeOptions) { o.IdleConnTimeout = 0 },
	} {
		o := defaults
		change(&o)
		if _, err := NewOpenSearchClient(&types.ConnectionConfig{Addr: "http://localhost:1"},
			WithRuntimeOptions(o)); !errors.Is(
			err,
			ErrConfigInvalid,
		) {
			t.Fatal("invalid runtime accepted")
		}
	}
	for _, cfg := range []*types.IndexConfig{nil, {}, {NumberOfReplicas: 0}, {NumberOfReplicas: 2}} {
		got, err := buildInternalCfg(cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if cfg != nil && cfg.NumberOfReplicas == 2 {
			want = 2
		}
		if got.replicas != want {
			t.Fatalf("replica projection %d", got.replicas)
		}
		t.Logf("input=%+v replicas=%d", cfg, got.replicas)
	}
	empty, _ := json.Marshal(types.IndexConfig{})
	zero, _ := json.Marshal(types.IndexConfig{NumberOfReplicas: 0})
	if !bytes.Equal(empty, zero) {
		t.Fatal("existing omitted/zero semantics changed")
	}
	checks := 0
	err := WaitForVisibility(
		context.Background(),
		30*time.Millisecond,
		5*time.Millisecond,
		func(context.Context) (bool, error) { checks++; return false, nil },
	)
	if !errors.Is(err, context.DeadlineExceeded) || checks < 2 {
		t.Fatalf("polling timeout: %d %v", checks, err)
	}
	if err := WaitForVisibility(context.Background(), time.Second, time.Millisecond, func(context.Context) (bool,
		error,
	) {
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := WaitForVisibility(context.Background(), 0, time.Millisecond, func(context.Context) (bool,
		error,
	) {
		return true, nil
	}); !errors.Is(
		err,
		ErrConfigInvalid,
	) {
		t.Fatal("unlimited wait accepted")
	}
}

func TestOS01_AllRepositoryRoutesShareWriteBudget(t *testing.T) {
	budget, _ := NewRequestBudget(2, 1, 2)
	var block atomic.Bool
	var mutations atomic.Int32
	release := make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			return
		case strings.HasSuffix(r.URL.Path, "_search"):
			_, _ = io.WriteString(
				w,
				`{"hits":{"hits":[{"_source":{"chunk_id":"src","source_id":"src","knowledge_id":"doc","knowledge_base_id":"kb","content":"copy","embedding":[1,2,3]}}]}}`, //nolint:lll // bulk response fixture
			)
		case strings.HasSuffix(r.URL.Path, "/_refresh"):
			// Pre-by-query visibility refresh; not a data mutation.
			_, _ = io.WriteString(w, `{"_shards":{"total":1,"successful":1,"failed":0}}`)
		default:
			mutations.Add(1)
			if block.Load() {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			switch {
			case strings.HasSuffix(r.URL.Path, "_bulk"):
				echoBulk(w, r)
			case strings.HasSuffix(r.URL.Path, "_delete_by_query"):
				_, _ = io.WriteString(w, `{"total":1,"deleted":1,"failures":[]}`)
			case strings.HasSuffix(r.URL.Path, "_update_by_query"):
				_, _ = io.WriteString(w, `{"total":1,"updated":1,"failures":[]}`)
			default:
				if r.URL.Query().Get("refresh") != "" {
					t.Error("Save forced refresh")
				}
				_, _ = io.WriteString(w, `{"_id":"target","result":"created"}`)
			}
		}
	}))
	defer ts.Close()
	defer closeRelease()
	repo := managedTestRepo(t, ts.URL, "store_a", WithRequestBudget(budget))
	other := managedTestRepo(t, ts.URL, "store_b", WithRequestBudget(budget))
	operations := []struct {
		name string
		call func(context.Context) error
	}{
		{"save", func(ctx context.Context) error { return repo.Save(ctx, &types.IndexInfo{ChunkID: "target"}, nil) }},
		{"bulk", func(ctx context.Context) error {
			return repo.BatchSave(ctx, []*types.IndexInfo{{ChunkID: "target"}}, nil)
		}},
		{"enabled", func(ctx context.Context) error {
			return repo.BatchUpdateChunkEnabledStatus(ctx, map[string]bool{"target": false})
		}},
		{"tag", func(ctx context.Context) error {
			return repo.BatchUpdateChunkTagID(ctx, map[string]string{"target": "tag"})
		}},
		{"delete", func(ctx context.Context) error { return repo.DeleteByChunkIDList(ctx, []string{"target"}, 0, "") }},
		{
			"move",
			func(ctx context.Context) error { return repo.MoveKnowledgeIndices(ctx, "kb", "kb2", "doc", nil, 3, "") },
		},
		{"copy", func(ctx context.Context) error {
			return repo.CopyIndices(
				ctx,
				"kb",
				map[string]string{"doc": "doc2"},
				map[string]string{"src": "target"},
				"kb2",
				3,
				"",
			)
		}},
	}
	// Run every real route once, including Copy's nested scan -> BatchSave.
	for _, op := range operations {
		if err := op.call(context.Background()); err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
	}
	block.Store(true)
	held := make(chan error, 1)
	go func() { held <- other.Save(context.Background(), &types.IndexInfo{ChunkID: "held"}, nil) }()
	awaitCondition(t, func() bool { return mutations.Load() == int32(len(operations)+1) })
	before := mutations.Load()
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			if err := op.call(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("write route bypassed full budget: %v", err)
			}
		})
	}
	if mutations.Load() != before {
		t.Fatal("write reached server while write quota full")
	}
	block.Store(false)
	closeRelease()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if s := budget.Snapshot(); s.InFlight != 0 || s.Queued != 0 || s.WritePeak != 1 {
		t.Fatalf("budget leak %+v", s)
	}
}

func TestOS01_PermitHeldUntilBodyReadFinishes(t *testing.T) {
	budget, _ := NewRequestBudget(2, 1, 2)
	headers := make(chan struct{})
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(headers)
		select {
		case <-release:
			_, _ = io.WriteString(w, `{}`)
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	repo := managedTestRepo(t, ts.URL, "store_a", WithRequestBudget(budget))
	done := make(chan error, 1)
	go func() { _, err := repo.client.Info(context.Background(), nil); done <- err }()
	<-headers
	if snap := budget.Snapshot(); snap.InFlight != 1 {
		close(release)
		t.Fatalf("released before body completed: %+v", snap)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if budget.Snapshot().InFlight != 0 {
		t.Fatal("body completion leaked permit")
	}
}

func TestOS01_InitializationWaiterCanCancel(t *testing.T) {
	for _, dim := range []int{0, 3} {
		t.Run(fmt.Sprint(dim), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					close(entered)
					<-release
					return
				}
				_, _ = io.WriteString(w, `{"acknowledged":true}`)
			}))
			defer ts.Close()
			repo := managedTestRepo(t, ts.URL, "store_a")
			repo.keywordsReady = false
			ensure := func(ctx context.Context) error {
				if dim == 0 {
					return repo.ensureKeywordsIndex(ctx)
				}
				return repo.ensureReady(ctx, dim)
			}
			done := make(chan error, 1)
			go func() { done <- ensure(context.Background()) }()
			<-entered
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			err := ensure(ctx)
			close(release)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("waiter not cancellable: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
