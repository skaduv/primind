// Package turn tracks the assistant turns that are generating inside this
// process.
//
// The stream manager's live-run marker says which turn owns a session across
// replicas, but only agent turns publish it, and it dies with Redis. This
// registry is the in-process truth: it lets shutdown finalize the turns that
// are still running here, and lets the reconciler tell an abandoned row from
// one this process is actively generating (BF-03 §4.4, §4.5).
package turn

import (
	"context"
	"sync"
	"time"
)

// MaxTurnAge bounds how long a registration vouches for a turn when the
// reconciler asks. A quick-answer stream whose driver goroutine dies without
// emitting anything never unregisters; without this bound the reconciler
// would skip that row for the life of the process. Two hours matches the
// handler's own stop watcher, which stops vouching for a stream after that
// long. Shutdown still sees every registration, however old.
const MaxTurnAge = 2 * time.Hour

// Turn is one generating assistant turn registered in this process.
type Turn struct {
	SessionID string
	MessageID string
	// registeredAt is when the turn started generating here.
	registeredAt time.Time
	// interrupt writes the turn's terminal state. It must be safe to call
	// more than once and with a context that is never cancelled.
	interrupt func(ctx context.Context)
	// cancel, when set, stops the generation after it was interrupted.
	cancel context.CancelFunc
}

// Registry is safe for concurrent use. A nil Registry is a valid empty
// registry so callers that were built without one need no nil checks.
type Registry struct {
	mu   sync.RWMutex
	live map[string]Turn
	now  func() time.Time
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{live: make(map[string]Turn), now: time.Now}
}

func key(sessionID, messageID string) string { return sessionID + "/" + messageID }

// Register records a generating turn and returns the function that ends the
// registration. Calling it more than once is harmless.
func (r *Registry) Register(
	sessionID, messageID string,
	interrupt func(ctx context.Context),
	cancel context.CancelFunc,
) (unregister func()) {
	if r == nil || sessionID == "" || messageID == "" {
		return func() {}
	}
	k := key(sessionID, messageID)
	r.mu.Lock()
	r.live[k] = Turn{SessionID: sessionID, MessageID: messageID, registeredAt: r.now(), interrupt: interrupt, cancel: cancel}
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.live, k)
			r.mu.Unlock()
		})
	}
}

// IsLive reports whether this process is vouching for messageID of sessionID
// as still generating: registered, unregistered no, and not older than
// MaxTurnAge.
func (r *Registry) IsLive(sessionID, messageID string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.live[key(sessionID, messageID)]
	return ok && r.now().Sub(t.registeredAt) < MaxTurnAge
}

// Live returns a snapshot of the registered turns, however old.
func (r *Registry) Live() []Turn {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Turn, 0, len(r.live))
	for _, t := range r.live {
		out = append(out, t)
	}
	return out
}

// Len returns how many turns are registered in this process.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.live)
}

// InterruptAll writes the terminal state of every turn still registered in
// this process, then stops their generation. It is the graceful-shutdown
// counterpart of a kill -9: nothing else gets a chance to run after it, so it
// waits for every interrupt and returns how many it ran. A turn whose
// interrupt is nil is only cancelled.
func (r *Registry) InterruptAll(ctx context.Context) int {
	if r == nil {
		return 0
	}
	live := r.Live()
	var wg sync.WaitGroup
	for _, t := range live {
		wg.Add(1)
		go func(t Turn) {
			defer wg.Done()
			if t.interrupt != nil {
				t.interrupt(ctx)
			}
			if t.cancel != nil {
				t.cancel()
			}
		}(t)
	}
	wg.Wait()
	return len(live)
}
