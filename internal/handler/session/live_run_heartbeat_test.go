package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

// touchCountingStreams records TouchLiveRun calls; everything else panics
// through the embedded nil interface, so the test proves the heartbeat only
// ever touches its own marker.
type touchCountingStreams struct {
	interfaces.StreamManager
	mu      sync.Mutex
	touches []string
}

func (s *touchCountingStreams) TouchLiveRun(_ context.Context, sessionID, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touches = append(s.touches, sessionID+"/"+messageID)
	return nil
}

func (s *touchCountingStreams) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.touches)
}

// BF-03: reads no longer extend the live-run marker, so the generating
// process must, for as long as the turn runs and not a moment longer.
func TestLiveRunHeartbeatTouchesOwnMarkerUntilTurnEnds(t *testing.T) {
	prev := liveRunHeartbeatInterval
	liveRunHeartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() { liveRunHeartbeatInterval = prev })

	streams := &touchCountingStreams{}
	h := &Handler{streamManager: streams}
	streamCtx := &sseStreamContext{}

	h.startLiveRunHeartbeat(context.Background(), streamCtx, "sess-1", "assist-1")
	require.Eventually(t, func() bool { return streams.count() >= 2 }, time.Second, 5*time.Millisecond)

	streamCtx.endTurn()
	streamCtx.endTurn() // every ending path calls it; a second call must not panic
	stoppedAt := streams.count()
	time.Sleep(60 * time.Millisecond)
	// One tick may already have been in flight when endTurn closed the channel.
	require.LessOrEqual(t, streams.count(), stoppedAt+1, "the heartbeat stops with the turn")

	streams.mu.Lock()
	defer streams.mu.Unlock()
	for _, touched := range streams.touches {
		require.Equal(t, "sess-1/assist-1", touched)
	}
}

// A second start for the same turn must not leak a second goroutine.
func TestLiveRunHeartbeatStartsOnce(t *testing.T) {
	streams := &touchCountingStreams{}
	h := &Handler{streamManager: streams}
	streamCtx := &sseStreamContext{}

	h.startLiveRunHeartbeat(context.Background(), streamCtx, "sess-1", "assist-1")
	first := streamCtx.stopLiveRunHeartbeat
	h.startLiveRunHeartbeat(context.Background(), streamCtx, "sess-1", "assist-1")
	require.Equal(t, first, streamCtx.stopLiveRunHeartbeat)
	streamCtx.endTurn()
}
