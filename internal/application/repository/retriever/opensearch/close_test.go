package opensearch

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/primind/primind/internal/types"
)

// openConnsAfterClose builds a repository (which leaves an idle pooled
// connection from its probes), calls Close, and reports open connections.
func openConnsAfterClose(t *testing.T, opts ...Option) int32 {
	t.Helper()
	var open atomic.Int32
	ts := httptest.NewUnstartedServer(clusterHandler("opensearch", "2.19.3", []osapi.CatPluginResp{
		{Name: "node-1", Component: "opensearch-knn"},
	}))
	ts.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)

	client, err := NewOpenSearchClient(&types.ConnectionConfig{Addr: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := NewRepository(context.Background(), client, "", nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if open.Load() == 0 {
		t.Fatal("expected an idle pooled connection after construction")
	}
	if err := repo.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(300 * time.Millisecond)
	for open.Load() != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	return open.Load()
}

func TestClose_OwnedClientReleasesPool(t *testing.T) {
	if n := openConnsAfterClose(t, WithOwnedClient()); n != 0 {
		t.Fatalf("owner Close left %d connections open", n)
	}
}

func TestClose_BorrowedClientIsLeftOpen(t *testing.T) {
	if n := openConnsAfterClose(t); n == 0 {
		t.Fatal("borrower Close must not release a client it does not own")
	}
}
