package turn

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRegistryTracksGeneratingTurns(t *testing.T) {
	r := NewRegistry()
	assert.False(t, r.IsLive("s1", "a1"))

	var cancelled atomic.Bool
	_, cancel := context.WithCancel(context.Background())
	unregister := r.Register("s1", "a1", func(context.Context) {}, cancel)

	assert.True(t, r.IsLive("s1", "a1"))
	assert.False(t, r.IsLive("s1", "a2"), "the key is the (session, message) pair")
	assert.False(t, r.IsLive("s2", "a1"))
	assert.Equal(t, 1, r.Len())

	unregister()
	unregister() // idempotent
	assert.False(t, r.IsLive("s1", "a1"))
	assert.Zero(t, r.Len())
	assert.False(t, cancelled.Load(), "unregistering never cancels the turn")
	cancel()
}

func TestInterruptAllCancelsAndWritesEveryLiveTurn(t *testing.T) {
	r := NewRegistry()
	events := make(chan string, 6)
	contexts := make([]context.Context, 0, 3)
	for _, id := range []string{"a1", "a2", "a3"} {
		id := id
		ctx, cancel := context.WithCancel(context.Background())
		contexts = append(contexts, ctx)
		r.Register("s1", id, func(context.Context) {
			events <- "interrupt:" + id
		}, func() {
			cancel()
			events <- "cancel:" + id
		})
	}
	// A turn that registered no callbacks is still counted.
	r.Register("s2", "a4", nil, nil)

	n := r.InterruptAll(context.Background())
	assert.Equal(t, 4, n)

	seen := make(map[string]bool)
	for i := 0; i < 6; i++ {
		select {
		case e := <-events:
			seen[e] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d events arrived: %v", i, seen)
		}
	}
	for i, id := range []string{"a1", "a2", "a3"} {
		assert.True(t, seen["interrupt:"+id], id)
		assert.True(t, seen["cancel:"+id], id)
		assert.Error(t, contexts[i].Err(), "generation of %s is stopped", id)
	}
	assert.Len(t, r.Live(), 4, "interrupting does not unregister; the turn's own teardown does")
}

// InterruptAll must not hang on a turn whose interrupt blocks: the shutdown
// budget is finite, and the registry only reports how many it ran.
func TestInterruptAllReturnsWhenAnInterruptBlocks(t *testing.T) {
	r := NewRegistry()
	r.Register("s1", "a1", func(ctx context.Context) { <-ctx.Done() }, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan int, 1)
	go func() { done <- r.InterruptAll(ctx) }()
	select {
	case n := <-done:
		assert.Equal(t, 1, n)
	case <-time.After(2 * time.Second):
		t.Fatal("InterruptAll ignored the context deadline")
	}
}

func TestNilRegistryIsEmpty(t *testing.T) {
	var r *Registry
	assert.False(t, r.IsLive("s1", "a1"))
	assert.Empty(t, r.Live())
	assert.Zero(t, r.Len())
	assert.Zero(t, r.InterruptAll(context.Background()))
	r.Register("s1", "a1", nil, nil)()
}

func TestRegisterIgnoresIncompleteIds(t *testing.T) {
	r := NewRegistry()
	r.Register("", "a1", nil, nil)
	r.Register("s1", "", nil, nil)
	assert.Zero(t, r.Len())
}

// A quick-answer stream can lose its driver without ever unregistering. The
// registration stops vouching for the turn after MaxTurnAge so the reconciler
// may repair the row; shutdown still interrupts it.
func TestIsLiveStopsVouchingAfterMaxTurnAge(t *testing.T) {
	r := NewRegistry()
	now := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	var interrupted atomic.Int32
	r.Register("s1", "a1", func(context.Context) { interrupted.Add(1) }, nil)

	assert.True(t, r.IsLive("s1", "a1"))
	now = now.Add(MaxTurnAge - time.Second)
	assert.True(t, r.IsLive("s1", "a1"))
	now = now.Add(2 * time.Second)
	assert.False(t, r.IsLive("s1", "a1"), "an old registration no longer holds the reconciler back")

	assert.Equal(t, 1, r.Len(), "the registration itself is kept for shutdown")
	assert.Equal(t, 1, r.InterruptAll(context.Background()))
	assert.Equal(t, int32(1), interrupted.Load())
}

// Generation stops before any terminal write starts: a slow database must not
// keep every turn calling the model until the writes finish.
func TestInterruptAllCancelsEveryTurnBeforeAnyWrite(t *testing.T) {
	r := NewRegistry()
	var mu sync.Mutex
	cancelled := 0
	cancelledAtWrite := make([]int, 0, 3)
	for _, id := range []string{"a1", "a2", "a3"} {
		r.Register("s1", id, func(context.Context) {
			mu.Lock()
			cancelledAtWrite = append(cancelledAtWrite, cancelled)
			mu.Unlock()
		}, func() {
			mu.Lock()
			cancelled++
			mu.Unlock()
		})
	}

	assert.Equal(t, 3, r.InterruptAll(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{3, 3, 3}, cancelledAtWrite, "every turn was cancelled before the first write")
}
