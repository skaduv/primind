package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
	nebulaRepo "github.com/primind/primind/internal/application/repository/retriever/nebula"
	neo4jRepo "github.com/primind/primind/internal/application/repository/retriever/neo4j"
	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

// graphConnectRetries matches the Neo4j start-up retry budget: the graph
// service may come up after the application.
const (
	graphConnectRetries  = 30
	graphConnectInterval = 2 * time.Second
	// graphCloseTimeout stays inside the cleaner's per-step budget.
	graphCloseTimeout = 4 * time.Second
)

// resolveGraphBackend returns the process-wide GRAPH_BACKEND selection and
// logs a conflict warning once. Errors abort start-up; there is no fallback.
func resolveGraphBackend(ctx context.Context) (types.GraphBackendSelection, error) {
	sel, err := types.CurrentGraphBackend()
	if err != nil {
		return sel, err
	}
	if sel.Warning != "" {
		logger.Warnf(ctx, "[Graph] %s", sel.Warning)
	}
	return sel, nil
}

// initGraphRepository constructs only the selected knowledge-graph backend.
// The Neo4j driver is provided separately (nil unless neo4j is selected).
func initGraphRepository(
	driver neo4j.Driver, cleaner interfaces.ResourceCleaner,
) (interfaces.RetrieveGraphRepository, error) {
	ctx := context.Background()
	sel, err := resolveGraphBackend(ctx)
	if err != nil {
		return nil, err
	}
	logger.Infof(ctx, "[Graph] backend=%s", sel.Backend)
	switch sel.Backend {
	case types.GraphBackendNeo4j:
		return neo4jRepo.NewNeo4jRepository(driver), nil
	case types.GraphBackendNebula:
		repo, err := connectNebula(ctx, newNebulaRepository)
		if err != nil {
			return nil, err
		}
		cleaner.RegisterWithName("NebulaGraph", func() error {
			closeCtx, cancel := context.WithTimeout(context.Background(), graphCloseTimeout)
			defer cancel()
			return repo.Close(closeCtx)
		})
		return repo, nil
	default:
		return disabledGraphRepository{}, nil
	}
}

type nebulaOpener func(context.Context, nebulaRepo.EnvConfig) (*nebulaRepo.Repository, error)

// newNebulaRepository opens the adapter; with AutoSchema an existing but empty
// dedicated Space gets its schema created first. InitializeSchema refuses a
// non-empty Space (ErrSchema), in which case NewRepository validates what is
// there and rejects anything incompatible.
func newNebulaRepository(ctx context.Context, cfg nebulaRepo.EnvConfig) (*nebulaRepo.Repository, error) {
	if cfg.AutoSchema {
		err := nebulaRepo.InitializeSchema(ctx, cfg.Config, nebulaRepo.SchemaAuthorization{
			Space: cfg.Space, CreateInEmptyDedicatedSpace: true,
		})
		switch {
		case err == nil:
			logger.Infof(ctx, "[Graph] created NebulaGraph schema in empty space %s", cfg.Space)
		case !errors.Is(err, nebulaRepo.ErrSchema):
			return nil, fmt.Errorf("initialize NebulaGraph schema: %w", err)
		}
	}
	return nebulaRepo.NewRepository(ctx, cfg.Config)
}

func connectNebula(ctx context.Context, open nebulaOpener) (*nebulaRepo.Repository, error) {
	cfg, err := nebulaRepo.ConfigFromEnv(os.LookupEnv)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		repo, err := open(ctx, cfg)
		if err == nil {
			logger.Infof(ctx, "[Graph] connected to NebulaGraph (%s)", cfg.Summary())
			return repo, nil
		}
		// Invalid configuration or an incompatible schema will not heal by
		// waiting; only connectivity errors are retried.
		if errors.Is(err, nebulaRepo.ErrInvalid) || errors.Is(err, nebulaRepo.ErrSchema) ||
			attempt >= graphConnectRetries {
			return nil, fmt.Errorf("connect NebulaGraph (attempt %d/%d): %w",
				attempt, graphConnectRetries, err)
		}
		logger.Warnf(ctx, "[Graph] NebulaGraph not ready (attempt %d/%d): %v",
			attempt, graphConnectRetries, err)
		time.Sleep(graphConnectInterval)
	}
}

// disabledGraphRepository backs GRAPH_BACKEND=disabled. Writes and searches
// report ErrGraphDisabled instead of pretending to succeed; deletes are no-ops
// because there is nothing this process could have written.
type disabledGraphRepository struct{}

func (disabledGraphRepository) AddGraph(context.Context, types.NameSpace, []*types.GraphData) error {
	return types.ErrGraphDisabled
}

func (disabledGraphRepository) DelGraph(context.Context, []types.NameSpace) error { return nil }

func (disabledGraphRepository) SearchNode(
	context.Context, types.NameSpace, []string,
) (*types.GraphData, error) {
	return nil, types.ErrGraphDisabled
}
