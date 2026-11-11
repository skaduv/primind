package container

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	nebulaRepo "github.com/primind/primind/internal/application/repository/retriever/nebula"
	"github.com/primind/primind/internal/types"
)

// Real-service check that the application's cleanup closes the NebulaGraph
// pool it registered. Runs only with NEBULA_IT_ADDRS and a dedicated
// primind_ng_* NEBULA_IT_SPACE. NEBULA_IT_HOLD keeps the pool open for that
// long before cleanup so an external observer can count graphd sessions.
func TestRealNebulaPoolClosedByResourceCleaner(t *testing.T) {
	addrs, space := os.Getenv("NEBULA_IT_ADDRS"), os.Getenv("NEBULA_IT_SPACE")
	if addrs == "" {
		t.Skip("NEBULA_IT_ADDRS not set")
	}
	if !strings.HasPrefix(space, "primind_ng_") {
		t.Fatal("NEBULA_IT_SPACE must be a dedicated primind_ng_* Space")
	}
	t.Setenv("NEBULA_ADDRS", addrs)
	t.Setenv("NEBULA_SPACE", space)
	t.Setenv("NEBULA_USERNAME", os.Getenv("NEBULA_IT_USERNAME"))
	t.Setenv("NEBULA_PASSWORD", os.Getenv("NEBULA_IT_PASSWORD"))
	types.SetGraphBackendForTest(types.GraphBackendSelection{Backend: types.GraphBackendNebula})
	t.Cleanup(func() { types.SetGraphBackendForTest(types.GraphBackendSelection{Backend: types.GraphBackendDisabled}) })

	cleaner := NewResourceCleaner()
	repo, err := initGraphRepository(nil, cleaner)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := types.NameSpace{KnowledgeBase: "it-pool-close"}
	// Use more than one session so the pool holds several open connections.
	for i := 0; i < 4; i++ {
		if _, err := repo.SearchNode(ctx, ns, []string{"x"}); err != nil {
			t.Fatal(err)
		}
	}
	if hold, err := time.ParseDuration(os.Getenv("NEBULA_IT_HOLD")); err == nil {
		t.Logf("pool open, holding %v", hold)
		time.Sleep(hold)
	}

	if errs := cleaner.Cleanup(ctx); len(errs) > 0 {
		t.Fatalf("cleanup errors: %v", errs)
	}
	if _, err := repo.SearchNode(ctx, ns, []string{"x"}); !errors.Is(err, nebulaRepo.ErrClosed) {
		t.Fatalf("SearchNode after cleanup = %v, want ErrClosed", err)
	}
	t.Log("pool closed by ResourceCleaner")
}
