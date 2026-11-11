package container

import (
	"context"
	"errors"
	"testing"

	nebulaRepo "github.com/primind/primind/internal/application/repository/retriever/nebula"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

type countingCleaner struct {
	interfaces.ResourceCleaner
	names []string
}

func (c *countingCleaner) RegisterWithName(name string, _ types.CleanupFunc) {
	c.names = append(c.names, name)
}

func TestDisabledGraphRepositoryReportsDisabled(t *testing.T) {
	var repo interfaces.RetrieveGraphRepository = disabledGraphRepository{}
	ctx := context.Background()
	if err := repo.AddGraph(ctx, types.NameSpace{}, nil); !errors.Is(err, types.ErrGraphDisabled) {
		t.Fatalf("AddGraph = %v, want ErrGraphDisabled", err)
	}
	if _, err := repo.SearchNode(ctx, types.NameSpace{}, []string{"x"}); !errors.Is(err, types.ErrGraphDisabled) {
		t.Fatalf("SearchNode = %v, want ErrGraphDisabled", err)
	}
	if err := repo.DelGraph(ctx, []types.NameSpace{{KnowledgeBase: "kb"}}); err != nil {
		t.Fatalf("DelGraph = %v, want nil", err)
	}
}

func TestInitGraphRepositorySelectsOnlyChosenBackend(t *testing.T) {
	t.Cleanup(func() { types.SetGraphBackendForTest(types.GraphBackendSelection{Backend: types.GraphBackendDisabled}) })

	types.SetGraphBackendForTest(types.GraphBackendSelection{Backend: types.GraphBackendDisabled})
	cleaner := &countingCleaner{}
	repo, err := initGraphRepository(nil, cleaner)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := repo.(disabledGraphRepository); !ok || len(cleaner.names) != 0 {
		t.Fatalf("disabled: got %T, cleanups %v", repo, cleaner.names)
	}
	if d, err := initNeo4jClient(); d != nil || err != nil {
		t.Fatalf("disabled must not create a Neo4j driver: %v %v", d, err)
	}

	types.SetGraphBackendForTest(types.GraphBackendSelection{Backend: types.GraphBackendNebula})
	if d, err := initNeo4jClient(); d != nil || err != nil {
		t.Fatalf("nebulagraph must not create a Neo4j driver: %v %v", d, err)
	}
	// No NEBULA_* configured: the selected backend fails loudly, no fallback.
	t.Setenv("NEBULA_ADDRS", "")
	repo, err = initGraphRepository(nil, cleaner)
	if err == nil || repo != nil || !errors.Is(err, nebulaRepo.ErrInvalid) {
		t.Fatalf("nebulagraph without config: repo=%T err=%v", repo, err)
	}

	types.SetGraphBackendForTest(types.GraphBackendSelection{Backend: types.GraphBackendNeo4j})
	repo, err = initGraphRepository(nil, cleaner)
	if err != nil || repo == nil {
		t.Fatalf("neo4j: repo=%T err=%v", repo, err)
	}
	if _, ok := repo.(disabledGraphRepository); ok {
		t.Fatal("neo4j selection returned the disabled repository")
	}
}

func TestConnectNebulaDoesNotRetryPermanentErrors(t *testing.T) {
	t.Setenv("NEBULA_ADDRS", "127.0.0.1:9669")
	t.Setenv("NEBULA_SPACE", "primind_ng_unit")
	t.Setenv("NEBULA_USERNAME", "root")
	t.Setenv("NEBULA_PASSWORD", "pw")
	for _, perm := range []error{nebulaRepo.ErrSchema, nebulaRepo.ErrInvalid} {
		calls := 0
		open := func(context.Context, nebulaRepo.EnvConfig) (*nebulaRepo.Repository, error) {
			calls++
			return nil, perm
		}
		_, err := connectNebula(context.Background(), open)
		if !errors.Is(err, perm) || calls != 1 {
			t.Fatalf("%v: calls=%d err=%v", perm, calls, err)
		}
	}
}
