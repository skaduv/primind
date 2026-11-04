package opensearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/primind/primind/internal/types"
)

// waitBudgetState observes a required state, rather than assuming that a sleep
// was long enough to order goroutines. The timer only bounds a failed fixture.
func waitBudgetState(t *testing.T, budget *RequestBudget, ready func(BudgetSnapshot) bool) BudgetSnapshot {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		snapshot := budget.Snapshot()
		if ready(snapshot) {
			return snapshot
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("required budget state not reached: %+v", snapshot)
		}
	}
}

func TestOS01_InitializationAdmissionCancelRecheckAndCacheReads(t *testing.T) {
	budget, err := NewRequestBudget(2, 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	writeEntered, releaseWrite := make(chan struct{}), make(chan struct{})
	unblockWrite := sync.OnceFunc(func() { close(releaseWrite) })
	var heads, queries atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodHead:
			heads.Add(1) // index already exists; only one HEAD is needed per cached dimension
		case strings.HasSuffix(r.URL.Path, "/_search"):
			queries.Add(1)
			io.WriteString(w, `{"hits":{"hits":[]}}`)
		default:
			close(writeEntered)
			select {
			case <-releaseWrite:
				io.WriteString(w, `{"_id":"blocker","result":"created"}`)
			case <-r.Context().Done():
			}
		}
	}))
	defer ts.Close()
	defer unblockWrite()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo := managedTestRepo(t, ts.URL, "store_a", WithRequestBudget(budget))
	repo.keywordsReady = false
	if err := repo.ensureReady(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if err := repo.ensureKeywordsIndex(ctx); err != nil {
		t.Fatal(err)
	}
	blocker := make(chan error, 1)
	go func() {
		_, err := repo.client.Index(ctx, osapi.IndexReq{Index: "other", DocumentID: "blocker", Body: strings.NewReader(`{}`)})
		blocker <- err
	}()
	select {
	case <-writeEntered:
	case <-ctx.Done():
		t.Fatal("blocking write did not start")
	}
	cancelCtx, cancelWaiter := context.WithCancel(ctx)
	defer cancelWaiter()
	canceled := make(chan error, 1)
	completed := make(chan error, 2)
	go func() { canceled <- repo.ensureReady(cancelCtx, 4) }()
	go func() { completed <- repo.ensureReady(ctx, 4) }()
	waitBudgetState(t, budget, func(s BudgetSnapshot) bool { return s.Queued == 2 })
	if !repo.initMu.TryLock() {
		t.Fatal("initializer held initMu while waiting for admission")
	}
	_, publishedWithoutPermit := repo.initializations[4]
	repo.initMu.Unlock()
	if publishedWithoutPermit {
		t.Fatal("initializer published an attempt without a write lease")
	}
	cancelWaiter()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued initialization cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued initializer ignored cancellation")
	}
	waitBudgetState(t, budget, func(s BudgetSnapshot) bool { return s.Queued == 1 })
	readCtx, cancelRead := context.WithTimeout(ctx, time.Second)
	defer cancelRead()
	if err := repo.ensureKeywordsIndex(readCtx); err != nil {
		t.Fatalf("keyword cache hit needed write admission: %v", err)
	}
	_, err = repo.Retrieve(readCtx, types.RetrieveParams{RetrieverType: types.VectorRetrieverType, Embedding: []float32{1, 2, 3}, TopK: 1})
	if err != nil || queries.Load() != 1 || heads.Load() != 2 {
		t.Fatalf("cached vector read blocked or initialized again: err=%v queries=%d heads=%d", err, queries.Load(), heads.Load())
	}
	go func() { completed <- repo.ensureReady(ctx, 4) }() // retry after cancellation
	waitBudgetState(t, budget, func(s BudgetSnapshot) bool { return s.Queued == 2 })
	unblockWrite()
	for i := 0; i < 2; i++ {
		select {
		case err := <-completed:
			if err != nil {
				t.Errorf("initializer after admission: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("initialization did not finish after write release")
		}
	}
	if err := <-blocker; err != nil {
		t.Fatal(err)
	}
	final := waitBudgetState(t, budget, func(s BudgetSnapshot) bool { return s.InFlight == 0 && s.Writing == 0 && s.Queued == 0 })
	if heads.Load() != 3 || final.Peak > 2 || final.WritePeak != 1 {
		t.Fatalf("admission recheck duplicated initialization or leaked permit: heads=%d final=%+v", heads.Load(), final)
	}
	t.Logf("cache reads progressed with write slot occupied; canceled admission retried; one new HEAD; final=%+v", final)
}

func TestOS01_SaveAndBatchInitializationShareBudget(t *testing.T) {
	for _, dim := range []int{0, 3} {
		t.Run(fmt.Sprintf("dim_%d", dim), func(t *testing.T) {
			budget, err := NewRequestBudget(3, 2, 4)
			if err != nil {
				t.Fatal(err)
			}
			headEntered, releaseHead := make(chan struct{}), make(chan struct{})
			markHead, unblockHead := sync.OnceFunc(func() { close(headEntered) }), sync.OnceFunc(func() { close(releaseHead) })
			var heads, creates, saves, bulks, live atomic.Int32
			ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodHead:
					heads.Add(1)
					markHead()
					select {
					case <-releaseHead:
						w.WriteHeader(http.StatusNotFound)
					case <-r.Context().Done():
					}
				case r.URL.Path == "/":
					io.WriteString(w, `{"version":{"distribution":"opensearch","number":"2.19.7"}}`)
				case r.URL.Path == "/_cat/plugins":
					io.WriteString(w, `[{"name":"node","component":"opensearch-knn"}]`)
				case strings.HasSuffix(r.URL.Path, "/_bulk"):
					bulks.Add(1)
					echoBulk(w, r)
				case strings.Contains(r.URL.Path, "/_doc/"):
					saves.Add(1)
					io.WriteString(w, `{"_id":"save","result":"created"}`)
				case r.Method == http.MethodPut:
					if !strings.Contains(r.URL.Path, "/_alias/") {
						creates.Add(1)
					}
					io.WriteString(w, `{"acknowledged":true}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					live.Add(1)
				} else if state == http.StateClosed {
					live.Add(-1)
				}
			}
			ts.Start()
			defer ts.Close()
			defer unblockHead()
			options := DefaultRuntimeOptions()
			options.RequestTimeout = 10 * time.Second
			client, err := NewOpenSearchClient(&types.ConnectionConfig{Addr: ts.URL}, WithRequestBudget(budget), WithRuntimeOptions(options))
			if err != nil {
				t.Fatal(err)
			}
			defer CloseIdleConnections(client)
			engine, err := NewRepository(context.Background(), client, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			repo := engine.(*Repository)
			ctx, cancel := context.WithTimeout(context.Background(), options.RequestTimeout)
			defer cancel()
			var params map[string]any
			if dim > 0 {
				params = map[string]any{"embedding": map[string][]float32{"source": {1, 2, 3}}}
			}
			results := make(chan error, 3)
			go func() { results <- repo.Save(ctx, &types.IndexInfo{ChunkID: "save", SourceID: "source"}, params) }()
			select {
			case <-headEntered:
			case <-ctx.Done():
				t.Fatal("Save did not reach initialization HEAD")
			}
			initial := budget.Snapshot()
			for i := 0; i < 2; i++ {
				go func(i int) {
					results <- repo.BatchSave(ctx, []*types.IndexInfo{{ChunkID: fmt.Sprintf("batch-%d", i), SourceID: "source"}}, params)
				}(i)
			}
			// Both batch callers must have either acquired a write permit or
			// entered its queue before HEAD is allowed to return. The old path
			// has 2 writers + 0 queued; a leased initializer has 2 + 1 instead.
			gated := waitBudgetState(t, budget, func(s BudgetSnapshot) bool {
				return s.Writing+s.Queued == initial.Writing+2
			})
			if creates.Load() != 0 || saves.Load() != 0 || bulks.Load() != 0 {
				t.Fatal("fixture allowed a write before releasing HEAD")
			}
			started := time.Now()
			unblockHead()
			completion := time.NewTimer(time.Second) // well below the 10s operation timeout
			defer completion.Stop()
			deadline := completion.C
			for i := 0; i < 3; i++ {
				select {
				case err := <-results:
					if err != nil {
						t.Errorf("healthy mixed operation failed: %v", err)
					}
				case <-deadline:
					t.Errorf("healthy operations stalled after HEAD release: initial=%+v gated=%+v now=%+v", initial, gated, budget.Snapshot())
					cancel() // terminate the red case without waiting for the 10s operation timeout
					deadline = nil
					i--
				case <-ctx.Done():
					// Every goroutine must still publish its result after cancellation.
					select {
					case err := <-results:
						if err != nil {
							t.Errorf("mixed operation after cancellation: %v", err)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("mixed operation did not exit after cancellation")
					}
				}
			}
			final := waitBudgetState(t, budget, func(s BudgetSnapshot) bool { return s.InFlight == 0 && s.Writing == 0 && s.Queued == 0 })
			if heads.Load() != 1 || creates.Load() != 1 || saves.Load() != 1 || bulks.Load() != 2 {
				t.Errorf("no false success: head=%d create=%d save=%d bulk=%d", heads.Load(), creates.Load(), saves.Load(), bulks.Load())
			}
			if final.Peak > 3 || final.WritePeak > 2 {
				t.Errorf("shared budget exceeded: %+v", final)
			}
			CloseIdleConnections(client)
			awaitCondition(t, func() bool { return live.Load() == 0 })
			t.Logf("HEAD release to completion=%s; gated=%+v final=%+v; connections=%d", time.Since(started), gated, final, live.Load())
		})
	}
}
