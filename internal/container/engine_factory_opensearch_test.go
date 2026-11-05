package container

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/primind/primind/internal/config"
	"github.com/primind/primind/internal/types"
)

func TestCreateOpenSearchEngine_FailureClosesOwnedPool(t *testing.T) {
	var live atomic.Int32
	ts := httptest.NewUnstartedServer(osClusterHandler("elasticsearch", "8.10.4", true))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			live.Add(1)
		}
		if state == http.StateClosed {
			live.Add(-1)
		}
	}
	ts.Start()
	defer ts.Close()
	store := types.VectorStore{ConnectionConfig: types.ConnectionConfig{Addr: ts.URL}}
	_, err := createOpenSearchEngine(context.Background(), store, nil)
	if err == nil {
		t.Fatal("expected construction failure")
	}
	until := time.Now().Add(time.Second)
	for live.Load() != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if live.Load() != 0 {
		t.Fatalf("failed construction retained %d connections", live.Load())
	}
}

// osClusterHandler simulates an OpenSearch cluster for the driver's
// construction probe: GET / (version) + /_cat/plugins (k-NN on every node).
func osClusterHandler(distribution, number string, knnInstalled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/_cat/plugins" {
			if knnInstalled {
				_, _ = w.Write([]byte(`[{"name":"node-1","component":"opensearch-knn"}]`))
			} else {
				_, _ = w.Write([]byte(`[{"name":"node-1","component":"opensearch-sql"}]`))
			}
			return
		}
		_, _ = w.Write([]byte(`{"version":{"distribution":"` + distribution + `","number":"` + number + `"}}`))
	}
}

func TestCreateOpenSearchEngine_WiresClientAndRepo(t *testing.T) {
	ts := httptest.NewServer(osClusterHandler("opensearch", "3.3.2", true))
	defer ts.Close()

	store := types.VectorStore{
		ID:               "", // env-style (no index prefix)
		EngineType:       types.OpenSearchRetrieverEngineType,
		ConnectionConfig: types.ConnectionConfig{Addr: ts.URL},
	}
	svc, err := createOpenSearchEngine(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("createOpenSearchEngine: %v", err)
	}
	if svc == nil {
		t.Fatal("want non-nil engine service")
	}
	if svc.EngineType() != types.OpenSearchRetrieverEngineType {
		t.Errorf("engine type: want opensearch, got %s", svc.EngineType())
	}
}

func TestCreateOpenSearchEngine_RejectsBadCluster(t *testing.T) {
	// Elasticsearch distribution must be rejected at construction.
	ts := httptest.NewServer(osClusterHandler("elasticsearch", "8.10.4", true))
	defer ts.Close()
	_, err := createOpenSearchEngine(context.Background(),
		types.VectorStore{EngineType: types.OpenSearchRetrieverEngineType,
			ConnectionConfig: types.ConnectionConfig{Addr: ts.URL}}, nil)
	if err == nil {
		t.Error("elasticsearch cluster should be rejected at engine creation")
	}
}

func TestCreateEngineServiceFromStore_OpenSearchCaseReached(t *testing.T) {
	ts := httptest.NewServer(osClusterHandler("opensearch", "2.11.0", true))
	defer ts.Close()
	svc, err := createEngineServiceFromStore(context.Background(),
		types.VectorStore{EngineType: types.OpenSearchRetrieverEngineType,
			ConnectionConfig: types.ConnectionConfig{Addr: ts.URL}},
		nil, &config.Config{}, nil)
	if err != nil {
		t.Fatalf("createEngineServiceFromStore (opensearch case): %v", err)
	}
	if svc == nil || svc.EngineType() != types.OpenSearchRetrieverEngineType {
		t.Errorf("opensearch case not wired correctly: %v", svc)
	}
}

// TestInitRetrieveEngineRegistry_OpenSearchEnvPath exercises the
// RETRIEVE_DRIVER=opensearch env-path registration block end to end.
func TestInitRetrieveEngineRegistry_OpenSearchEnvPath(t *testing.T) {
	ts := httptest.NewServer(osClusterHandler("opensearch", "3.3.2", true))
	defer ts.Close()

	// In-memory DB: the vector_stores table is absent, so loadDBStores logs
	// and returns (non-fatal) — only the env-path block matters here.
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-mem db: %v", err)
	}

	t.Setenv("RETRIEVE_DRIVER", "opensearch")
	t.Setenv("OPENSEARCH_ADDR", ts.URL)

	// nil store repository and engine factory: this exercises the env-driver
	// path, which never rebuilds a database-backed store.
	registry, err := initRetrieveEngineRegistry(db, &config.Config{}, &fakeAuditSvc{}, nil, nil)
	if err != nil {
		t.Fatalf("initRetrieveEngineRegistry: %v", err)
	}
	if _, err := registry.GetRetrieveEngineService(types.OpenSearchRetrieverEngineType); err != nil {
		t.Errorf("opensearch engine not registered via env path: %v", err)
	}
}

// Shutdown releases the connection pool of every OpenSearch engine the
// factory built: the "RetrieveEngines" cleanup registered by NewEngineFactory
// closes engines still published in the registry, not only retired ones.
func TestEngineFactoryShutdownClosesOwnedOpenSearchPools(t *testing.T) {
	var live atomic.Int32
	ts := httptest.NewUnstartedServer(osClusterHandler("opensearch", "2.19.3", true))
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			live.Add(1)
		case http.StateClosed, http.StateHijacked:
			live.Add(-1)
		}
	}
	ts.Start()
	defer ts.Close()

	cleaner := NewResourceCleaner()
	factory := NewEngineFactory(nil, &config.Config{}, &fakeAuditSvc{}, cleaner)
	svc, err := factory(context.Background(), types.VectorStore{
		ID: "0123456789abcdef0123", EngineType: types.OpenSearchRetrieverEngineType,
		ConnectionConfig: types.ConnectionConfig{Addr: ts.URL},
	})
	if err != nil || svc == nil {
		t.Fatalf("factory: %v", err)
	}
	if live.Load() == 0 {
		t.Fatal("expected an idle pooled connection after construction")
	}
	if errs := cleaner.Cleanup(context.Background()); len(errs) != 0 {
		t.Fatalf("cleanup errors: %v", errs)
	}
	until := time.Now().Add(time.Second)
	for live.Load() != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if n := live.Load(); n != 0 {
		t.Fatalf("shutdown left %d OpenSearch connections open", n)
	}
}
