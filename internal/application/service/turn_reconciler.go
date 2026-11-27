package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/primind/primind/internal/common/redislock"
	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/runtime"
	"github.com/primind/primind/internal/turn"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/redis/go-redis/v9"
)

// TurnReconciler ends assistant turns that were left "generating" by a
// process that could not run its own teardown: kill -9, OOM, a rolling
// restart, a panic the handler never saw. Without it such a row keeps
// is_completed=false forever, every open of the session asks continue-stream
// for it, and the user sees a stream that no longer exists (BF-03 §1, §4.4).
//
// It is a backstop, not the primary path: the handler finalizes every turn it
// runs, continue-stream repairs the ones a user opens, and this sweeps the
// sessions nobody opened. "repaired" should therefore sit near zero; a run
// that keeps repairing means a new teardown hole (BF-03 §9).
type TurnReconciler struct {
	messages interfaces.MessageRepository
	streams  interfaces.StreamManager
	redis    redis.UniversalClient
	registry *turn.Registry

	lockKey   string
	lockLease time.Duration
	grace     time.Duration
	batchSize int
	interval  time.Duration
	now       func() time.Time

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
	started   atomic.Bool
}

// ReconcileResult is the summary of one pass.
type ReconcileResult struct {
	Scanned  int // candidate rows the pass looked at
	Repaired int // rows this pass moved to interrupted
	Skipped  int // rows left alone: live here, live in another replica, or unreadable
	// LockHeld is true when another replica held the reconciler lock, so
	// this pass did not run at all.
	LockHeld bool
}

const (
	// turnReconcileInterval is how often the sweep runs.
	turnReconcileInterval = 5 * time.Minute
	// turnReconcileStartupDelay holds the first sweep until shortly after
	// boot: migrations finish and the first requests land before it runs,
	// yet operators still see the first "repaired" number on the same boot.
	turnReconcileStartupDelay = 30 * time.Second
	// turnReconcileLockLease outlives one sweep (interval + margin) so a
	// replica that dies mid-sweep cannot fence the others out for long.
	turnReconcileLockLease = 4 * time.Minute
	// turnReconcileBatchSize caps one scan.
	turnReconcileBatchSize = 200
	// turnReconcileSweepTimeout bounds one sweep's database and Redis work.
	turnReconcileSweepTimeout = 2 * time.Minute
	// turnReconcileLockKeySuffix is appended to the deployment's Redis prefix.
	turnReconcileLockKeySuffix = ":turn-reconciler:lock"
	// defaultRedisPrefix mirrors internal/stream's key prefix default.
	defaultRedisPrefix = "stream:events"
)

// NewTurnReconciler wires the reconciler. redis == nil means a single-process
// deployment (Lite / memory streams): no cross-replica lock is taken. The
// registry, which may be nil, lets the sweep recognise turns this very
// process is still generating.
func NewTurnReconciler(
	messages interfaces.MessageRepository,
	streams interfaces.StreamManager,
	rdb *redis.Client,
	registry *turn.Registry,
) *TurnReconciler {
	prefix := strings.TrimSpace(os.Getenv("REDIS_PREFIX"))
	if prefix == "" {
		prefix = defaultRedisPrefix
	}
	var lockClient redis.UniversalClient
	if rdb != nil {
		lockClient = rdb
	}
	return newTurnReconciler(messages, streams, lockClient, prefix+turnReconcileLockKeySuffix, registry)
}

// newTurnReconciler is the testable constructor: every knob in one place.
func newTurnReconciler(
	messages interfaces.MessageRepository,
	streams interfaces.StreamManager,
	lockClient redis.UniversalClient,
	lockKey string,
	registry *turn.Registry,
) *TurnReconciler {
	return &TurnReconciler{
		messages:  messages,
		streams:   streams,
		redis:     lockClient,
		registry:  registry,
		lockKey:   lockKey,
		lockLease: turnReconcileLockLease,
		grace:     types.TurnReconcileGracePeriod,
		batchSize: turnReconcileBatchSize,
		interval:  turnReconcileInterval,
		now:       time.Now,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Start begins the periodic sweep. Calling it more than once is a no-op.
func (r *TurnReconciler) Start(ctx context.Context) {
	if r == nil || r.messages == nil {
		return
	}
	r.startOnce.Do(func() {
		r.started.Store(true)
		logger.Infof(ctx, "[turn-reconciler] started: interval=%s grace=%s batch=%d lock=%t",
			r.interval, r.grace, r.batchSize, r.redis != nil)
		go r.loop()
	})
}

// Stop signals the loop to exit and waits for it.
func (r *TurnReconciler) Stop() { r.StopWithin(0) }

// StopWithin is Stop with a bound; timeout <= 0 waits for the loop to return.
func (r *TurnReconciler) StopWithin(timeout time.Duration) {
	if r == nil || !r.started.Load() {
		return
	}
	r.stopOnce.Do(func() { close(r.stopCh) })
	if runtime.WaitFor(r.doneCh, timeout) {
		return
	}
	logger.Warnf(context.Background(), "[turn-reconciler] sweep still running after %s; continuing shutdown", timeout)
}

// loop runs one sweep shortly after boot, then on the interval. Each sweep
// gets its own Background context: the ctx Start received belongs to
// container init and is gone by the time the first sweep fires.
func (r *TurnReconciler) loop() {
	defer close(r.doneCh)

	startup := time.NewTimer(turnReconcileStartupDelay)
	defer startup.Stop()
	select {
	case <-startup.C:
	case <-r.stopCh:
		return
	}
	r.runOnce()

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.runOnce()
		case <-r.stopCh:
			return
		}
	}
}

func (r *TurnReconciler) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), turnReconcileSweepTimeout)
	defer cancel()
	if _, err := r.ReconcileOnce(ctx); err != nil {
		logger.Warnf(ctx, "[turn-reconciler] sweep failed: %v", err)
	}
}

// acquireLock takes the cross-replica lock when one is configured. It returns
// the release function (nil when there is no lock) and whether this caller
// owns the sweep: false means another replica holds the lock.
func (r *TurnReconciler) acquireLock(ctx context.Context) (release func(), owned bool, err error) {
	if r.redis == nil {
		return nil, true, nil
	}
	token, err := redislock.NewToken()
	if err != nil {
		return nil, false, err
	}
	acquired, err := redislock.TryAcquire(ctx, r.redis, r.lockKey, token, r.lockLease)
	if err != nil {
		return nil, false, err
	}
	if !acquired {
		return nil, false, nil
	}
	return func() {
		// Release on a fresh context: the sweep context may already be done.
		releaseCtx, cancel := context.WithTimeout(context.Background(), redislockReleaseTimeout)
		defer cancel()
		if _, err := redislock.Release(releaseCtx, r.redis, r.lockKey, token); err != nil {
			logger.Warnf(releaseCtx, "[turn-reconciler] lock release failed (it expires in %s): %v", r.lockLease, err)
		}
	}, true, nil
}

const redislockReleaseTimeout = 5 * time.Second

// ReconcileOnce runs a single sweep: every assistant row still marked
// generating and older than the grace period is finalized as interrupted,
// unless something is still driving it. Only the lock holder runs when a
// Redis lock is configured.
func (r *TurnReconciler) ReconcileOnce(ctx context.Context) (ReconcileResult, error) {
	var result ReconcileResult
	if r == nil || r.messages == nil {
		return result, errors.New("turn reconciler has no message repository")
	}
	release, owned, err := r.acquireLock(ctx)
	if err != nil {
		return result, err
	}
	if !owned {
		result.LockHeld = true
		logger.Debugf(ctx, "[turn-reconciler] another replica holds the lock; skipping sweep")
		return result, nil
	}
	if release != nil {
		defer release()
	}

	result, err = r.sweep(ctx, r.now().Add(-r.grace))
	if err != nil {
		return result, err
	}
	// One line per sweep: the health signal operators watch (BF-03 §9).
	logger.Infof(ctx, "[turn-reconciler] sweep complete: scanned=%d repaired=%d skipped=%d",
		result.Scanned, result.Repaired, result.Skipped)
	return result, nil
}

// Backfill is the one-off form used after a deployment to repair the rows
// that accumulated before the reconciler existed (spec §4.6). It pages until
// a pass makes no progress, so rows skipped as live do not loop forever.
// olderThan is applied from now; it must cover the stream TTL for the same
// reason the periodic grace does.
func (r *TurnReconciler) Backfill(ctx context.Context, olderThan time.Duration) (ReconcileResult, error) {
	var total ReconcileResult
	if r == nil || r.messages == nil {
		return total, errors.New("turn reconciler has no message repository")
	}
	if olderThan <= 0 {
		olderThan = types.TurnReconcileGracePeriod
	}
	// The one-off runs while the app is up and holds no lock by default
	// (it is invoked by an operator, once); the conditional write keeps it
	// safe against a concurrent periodic sweep.
	for {
		pass, err := r.sweep(ctx, r.now().Add(-olderThan))
		total.Scanned += pass.Scanned
		total.Repaired += pass.Repaired
		total.Skipped += pass.Skipped
		if err != nil {
			return total, err
		}
		if pass.Repaired == 0 || pass.Scanned < r.batchSize {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// CountStale is the number Backfill would consider, for the before/after
// record the spec asks for.
func (r *TurnReconciler) CountStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		olderThan = types.TurnReconcileGracePeriod
	}
	return r.messages.CountGeneratingAssistantMessagesBefore(ctx, r.now().Add(-olderThan))
}

// sweep finalizes one batch of candidates older than cutoff. Backfill pages
// with it, so every candidate must leave the candidate set: a skipped row is
// fine because Backfill stops on the first pass without progress.
func (r *TurnReconciler) sweep(ctx context.Context, cutoff time.Time) (ReconcileResult, error) {
	var result ReconcileResult
	candidates, err := r.messages.ListGeneratingAssistantMessagesBefore(ctx, cutoff, r.batchSize)
	if err != nil {
		return result, err
	}
	result.Scanned = len(candidates)
	interruptedMessage := types.TurnInterruptedMessage(types.DefaultLanguage())
	for _, m := range candidates {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if m == nil || m.ID == "" {
			result.Skipped++
			continue
		}
		// A turn this process is still generating is not abandoned, whatever
		// its row age says: quick-answer turns publish no live-run marker.
		if r.registry != nil && r.registry.IsLive(m.SessionID, m.ID) {
			result.Skipped++
			continue
		}
		// A marker that names this message means an engine is still driving
		// it (possibly on another replica); an unreadable marker is not proof
		// of absence, so that row is left for a later pass.
		liveID, _, err := r.streams.GetLiveRun(ctx, m.SessionID)
		if err != nil {
			result.Skipped++
			logger.Warnf(ctx, "[turn-reconciler] live-run lookup failed for session %s: %v", m.SessionID, err)
			continue
		}
		if liveID != "" && liveID == m.ID {
			result.Skipped++
			continue
		}
		applied, err := r.messages.FinalizeMessage(ctx, m.SessionID, m.ID,
			types.MessageStatusInterrupted, types.TurnErrorCodeProcessLost, interruptedMessage, nil)
		if err != nil {
			result.Skipped++
			logger.Warnf(ctx, "[turn-reconciler] finalize failed for message %s in session %s: %v",
				m.ID, m.SessionID, err)
			continue
		}
		if !applied {
			// Ended by someone else between the scan and the write.
			result.Skipped++
			continue
		}
		result.Repaired++
		logger.Infof(ctx, "[turn-reconciler] interrupted abandoned turn message=%s session=%s abandoned_for=%s",
			m.ID, m.SessionID, r.now().Sub(m.UpdatedAt).Round(time.Second))
	}
	return result, nil
}
