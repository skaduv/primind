package retriever

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closableEngine counts Close calls so tests can assert ownership hand-off.
type closableEngine struct {
	mockEngineService
	closed atomic.Int32
}

func (c *closableEngine) Close() error {
	c.closed.Add(1)
	return nil
}

func newClosable() *closableEngine {
	return &closableEngine{mockEngineService: mockEngineService{engineType: types.OpenSearchRetrieverEngineType}}
}

func TestRegistry_UnregisterClosesRetiredEngine(t *testing.T) {
	r := newRehydratingRegistry(nil)
	e := newClosable()
	r.RegisterWithStoreID("store-1", e)

	r.UnregisterByStoreID("store-1")
	r.UnregisterByStoreID("store-1") // idempotent: nothing left to close
	assert.EqualValues(t, 1, e.closed.Load())
}

func TestRegistry_ReplaceClosesOnlyThePreviousEngine(t *testing.T) {
	r := newRehydratingRegistry(nil)
	oldEngine, newEngine := newClosable(), newClosable()
	r.RegisterWithStoreID("store-1", oldEngine)
	r.RegisterWithStoreID("store-1", oldEngine) // same engine re-registered: keep it open
	assert.Zero(t, oldEngine.closed.Load())

	r.RegisterWithStoreID("store-1", newEngine)
	assert.EqualValues(t, 1, oldEngine.closed.Load())
	assert.Zero(t, newEngine.closed.Load())
	live, err := r.GetByStoreID("store-1")
	require.NoError(t, err)
	assert.Same(t, newEngine, live)
}

func TestRegistry_NonClosableEnginesAreUntouched(t *testing.T) {
	r := newRehydratingRegistry(nil)
	r.RegisterWithStoreID("store-1", newMock(types.PostgresRetrieverEngineType))
	r.RegisterWithStoreID("store-1", newMock(types.PostgresRetrieverEngineType))
	r.UnregisterByStoreID("store-1")
}

func TestGetOrLoadByStoreID_LosingBuildClosesItsEngine(t *testing.T) {
	built := newClosable()
	factory := newBlockingFactory()
	registry := newRehydratingRegistry(func(ctx context.Context, s types.VectorStore) (interfaces.RetrieveEngineService, error) {
		if _, err := factory.build(ctx, s); err != nil {
			return nil, err
		}
		return built, nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = registry.GetOrLoadByStoreID(context.Background(), 1, rehydrateStoreID)
	}()
	<-factory.entered
	registered := newClosable()
	registry.RegisterWithStoreID(rehydrateStoreID, registered)
	close(factory.release)
	<-done

	assert.EqualValues(t, 1, built.closed.Load(), "the stale build is owned by nobody and must be closed")
	assert.Zero(t, registered.closed.Load())
}
