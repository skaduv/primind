package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/primind/primind/internal/application/repository"
	"github.com/primind/primind/internal/turn"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// reconcilerStream stubs the one StreamManager call the reconciler makes.
type reconcilerStream struct {
	interfaces.StreamManager
	mu     sync.Mutex
	live   map[string]string // session id -> generating assistant id
	lookup func(sessionID string) error
}

func (s *reconcilerStream) GetLiveRun(_ context.Context, sessionID string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lookup != nil {
		if err := s.lookup(sessionID); err != nil {
			return "", "", err
		}
	}
	return s.live[sessionID], "req-1", nil
}

func newTurnReconcilerTestRig(t *testing.T) (*TurnReconciler, *reconcilerStream, *turn.Registry, *gorm.DB, *miniredis.Miniredis) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Message{}))
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	streams := &reconcilerStream{live: map[string]string{}}
	registry := turn.NewRegistry()
	r := newTurnReconciler(repository.NewMessageRepository(db), streams, client, "test:turn-reconciler:lock", registry)
	return r, streams, registry, db, mini
}

// seedStaleTurn writes an assistant row left generating since `age` ago.
func seedStaleTurn(t *testing.T, db *gorm.DB, id, sessionID string, age time.Duration) {
	t.Helper()
	at := time.Now().Add(-age)
	require.NoError(t, db.Session(&gorm.Session{SkipHooks: true}).Create(&types.Message{
		ID: id, SessionID: sessionID, Role: "assistant", CreatedAt: at,
	}).Error)
	require.NoError(t, db.Model(&types.Message{}).Where("id = ?", id).UpdateColumn("updated_at", at).Error)
}

func loadTurn(t *testing.T, db *gorm.DB, id string) *types.Message {
	t.Helper()
	var row types.Message
	require.NoError(t, db.Where("id = ?", id).First(&row).Error)
	return &row
}

func TestTurnReconcilerInterruptsAbandonedTurns(t *testing.T) {
	r, streams, registry, db, _ := newTurnReconcilerTestRig(t)
	ctx := context.Background()
	seedStaleTurn(t, db, "abandoned", "s1", 2*time.Hour)
	seedStaleTurn(t, db, "too-recent", "s2", 10*time.Minute)
	seedStaleTurn(t, db, "live-elsewhere", "s3", 2*time.Hour) // agent turn still running on another replica
	streams.live["s3"] = "live-elsewhere"
	seedStaleTurn(t, db, "live-here", "s4", 2*time.Hour) // quick-answer turn in this process
	unregister := registry.Register("s4", "live-here", nil, nil)
	defer unregister()
	seedStaleTurn(t, db, "other-run-live", "s5", 2*time.Hour) // the session's live run is a different message
	streams.live["s5"] = "some-newer-run"

	got, err := r.ReconcileOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, ReconcileResult{Scanned: 4, Repaired: 2, Skipped: 2}, got)

	for _, id := range []string{"abandoned", "other-run-live"} {
		row := loadTurn(t, db, id)
		assert.Equal(t, types.MessageStatusInterrupted, row.Status, id)
		assert.Equal(t, types.TurnErrorCodeProcessLost, row.ErrorCode, id)
		assert.Equal(t, types.TurnInterruptedMessage(types.DefaultLanguage()), row.ErrorMessage, id)
		assert.True(t, row.IsCompleted, id)
	}
	for _, id := range []string{"too-recent", "live-elsewhere", "live-here"} {
		assert.Equal(t, types.MessageStatusGenerating, loadTurn(t, db, id).EffectiveStatus(), id)
		assert.False(t, loadTurn(t, db, id).IsCompleted, id)
	}

	// Only the two live turns are left as candidates on the next pass.
	got, err = r.ReconcileOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, ReconcileResult{Scanned: 2, Repaired: 0, Skipped: 2}, got)
}

// A replica that cannot read the live-run marker must not treat it as "no
// run is live": doing so would interrupt a turn that is still generating.
func TestTurnReconcilerSkipsWhenLiveRunLookupFails(t *testing.T) {
	r, streams, _, db, _ := newTurnReconcilerTestRig(t)
	seedStaleTurn(t, db, "abandoned", "s1", 2*time.Hour)
	streams.lookup = func(string) error { return context.DeadlineExceeded }

	got, err := r.ReconcileOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ReconcileResult{Scanned: 1, Skipped: 1}, got)
	assert.Equal(t, types.MessageStatusGenerating, loadTurn(t, db, "abandoned").EffectiveStatus())
}

// Two replicas share one Redis: whoever holds the lock sweeps, the other
// stands back instead of racing the same rows.
func TestTurnReconcilerOnlyTheLockHolderSweeps(t *testing.T) {
	leader, _, _, db, mini := newTurnReconcilerTestRig(t)
	follower, followerStreams, _, _, _ := newTurnReconcilerTestRig(t)
	// Point the follower at the leader's Redis and database so both see the
	// same lock and the same rows.
	followerClient := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = followerClient.Close() })
	follower.redis = followerClient
	follower.messages = leader.messages
	follower.streams = followerStreams

	seedStaleTurn(t, db, "abandoned", "s1", 2*time.Hour)
	ctx := context.Background()

	release, owned, err := leader.acquireLock(ctx)
	require.NoError(t, err)
	require.True(t, owned)

	got, err := follower.ReconcileOnce(ctx)
	require.NoError(t, err)
	assert.True(t, got.LockHeld)
	assert.Equal(t, ReconcileResult{LockHeld: true}, got, "the follower neither scans nor repairs")
	assert.Equal(t, types.MessageStatusGenerating, loadTurn(t, db, "abandoned").EffectiveStatus())

	release()

	got, err = follower.ReconcileOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, ReconcileResult{Scanned: 1, Repaired: 1}, got)
	assert.Equal(t, types.MessageStatusInterrupted, loadTurn(t, db, "abandoned").Status)
}

// Replicas that start a sweep at the same instant repair each abandoned turn
// exactly once between them.
func TestTurnReconcilerConcurrentSweepsRepairEachTurnOnce(t *testing.T) {
	leader, streams, _, db, mini := newTurnReconcilerTestRig(t)
	for i := 0; i < 5; i++ {
		seedStaleTurn(t, db, "stale-"+string(rune('a'+i)), "s"+string(rune('a'+i)), 3*time.Hour)
	}
	replicas := []*TurnReconciler{leader}
	for i := 0; i < 3; i++ {
		client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		replicas = append(replicas, newTurnReconciler(leader.messages, streams, client, leader.lockKey, nil))
	}

	var total ReconcileResult
	var mu sync.Mutex
	var wg sync.WaitGroup
	for round := 0; round < 4; round++ {
		for _, replica := range replicas {
			wg.Add(1)
			go func(replica *TurnReconciler) {
				defer wg.Done()
				got, err := replica.ReconcileOnce(context.Background())
				assert.NoError(t, err)
				mu.Lock()
				total.Scanned += got.Scanned
				total.Repaired += got.Repaired
				total.Skipped += got.Skipped
				mu.Unlock()
			}(replica)
		}
	}
	wg.Wait()

	assert.Equal(t, 5, total.Repaired, "each abandoned turn is repaired once, not once per replica")
	for i := 0; i < 5; i++ {
		assert.Equal(t, types.MessageStatusInterrupted, loadTurn(t, db, "stale-"+string(rune('a'+i))).Status)
	}
}

// The one-off backfill pages past the batch size and stops on rows it must
// leave alone, and CountStale reports what it will touch.
func TestTurnReconcilerBackfillPagesUntilDone(t *testing.T) {
	r, streams, _, db, _ := newTurnReconcilerTestRig(t)
	r.batchSize = 2
	for i := 0; i < 5; i++ {
		seedStaleTurn(t, db, "stale-"+string(rune('a'+i)), "s"+string(rune('a'+i)), 3*time.Hour)
	}
	seedStaleTurn(t, db, "live", "s-live", 3*time.Hour)
	streams.live["s-live"] = "live"

	ctx := context.Background()
	count, err := r.CountStale(ctx, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(6), count)

	got, err := r.Backfill(ctx, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 5, got.Repaired)
	assert.Equal(t, types.MessageStatusInterrupted, loadTurn(t, db, "stale-e").Status)
	assert.Equal(t, types.MessageStatusGenerating, loadTurn(t, db, "live").EffectiveStatus(), "the live turn is skipped on every page")

	count, err = r.CountStale(ctx, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "only the still-live turn remains a candidate")
}

// Without Redis there is no lock to take: a single-process deployment always
// owns the sweep.
func TestTurnReconcilerWithoutRedisSweepsUnlocked(t *testing.T) {
	r, _, _, db, _ := newTurnReconcilerTestRig(t)
	r.redis = nil
	seedStaleTurn(t, db, "abandoned", "s1", 2*time.Hour)

	got, err := r.ReconcileOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ReconcileResult{Scanned: 1, Repaired: 1}, got)
}

func TestTurnReconcilerStartIsIdempotentAndStops(t *testing.T) {
	r, _, _, _, _ := newTurnReconcilerTestRig(t)
	r.interval = 10 * time.Millisecond
	r.Start(context.Background())
	r.Start(context.Background()) // no-op
	r.StopWithin(5 * time.Second)
	select {
	case <-r.doneCh:
	default:
		t.Fatal("the loop did not exit on StopWithin")
	}
	// A second Stop is harmless.
	r.Stop()

	var never *TurnReconciler
	never.Start(context.Background())
	never.Stop()
	// A constructed-but-never-started reconciler does not block StopWithin.
	r2, _, _, _, _ := newTurnReconcilerTestRig(t)
	r2.StopWithin(time.Second)
}

func TestNewTurnReconcilerDerivesLockKeyFromRedisPrefix(t *testing.T) {
	t.Setenv("REDIS_PREFIX", "acme")
	r := NewTurnReconciler(nil, nil, nil, nil)
	assert.Equal(t, "acme:turn-reconciler:lock", r.lockKey)
	assert.Equal(t, types.TurnReconcileGracePeriod, r.grace)
	assert.Equal(t, turnReconcileBatchSize, r.batchSize)

	t.Setenv("REDIS_PREFIX", "")
	r = NewTurnReconciler(nil, nil, nil, nil)
	assert.Equal(t, defaultRedisPrefix+turnReconcileLockKeySuffix, r.lockKey)
	assert.Nil(t, r.redis, "no Redis client means no cross-replica lock")
}
