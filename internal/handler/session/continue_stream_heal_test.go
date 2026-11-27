package session

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/primind/primind/internal/middleware"
	"github.com/primind/primind/internal/turn"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// BF-03 T4: continue-stream self-heal. A generating message whose stream
// expired is ended as interrupted when nothing drives it, and is left alone
// when something might.

type healMessages struct {
	interfaces.MessageService
	message     *types.Message
	finalized   []finalizedCall
	finalizeErr error
	// endedAs, when set, is the state another path wrote between the
	// handler's read and its finalize: the write then does not apply.
	endedAs types.MessageStatus
}

func (m *healMessages) GetMessage(context.Context, string, string) (*types.Message, error) {
	row := *m.message
	return &row, nil
}

func (m *healMessages) FinalizeMessage(
	ctx context.Context, sessionID, messageID string, status types.MessageStatus,
	code, message string, content *string,
) (bool, error) {
	m.finalized = append(m.finalized, finalizedCall{
		sessionID: sessionID, messageID: messageID,
		status: status, code: code, message: message, content: content, ctxErr: ctx.Err(),
	})
	if m.finalizeErr != nil {
		return false, m.finalizeErr
	}
	if m.endedAs != "" {
		m.message.Status, m.message.IsCompleted = m.endedAs, true
		return false, nil
	}
	m.message.Status, m.message.ErrorCode, m.message.ErrorMessage, m.message.IsCompleted = status, code, message, true
	return true, nil
}

type healStream struct {
	stubStreamManager
	live    string
	liveErr error
}

func (s *healStream) GetLiveRun(context.Context, string) (string, string, error) {
	return s.live, "req", s.liveErr
}

func newHealRouter(t *testing.T, message *types.Message, streams *healStream) (*gin.Engine, *healMessages) {
	t.Helper()
	return newHealRouterWithTurns(t, message, streams, nil)
}

func newHealRouterWithTurns(
	t *testing.T, message *types.Message, streams *healStream, turns *turn.Registry,
) (*gin.Engine, *healMessages) {
	t.Helper()
	messages := &healMessages{message: message}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	h := &Handler{
		sessionService: &stubSessionService{},
		messageService: messages,
		streamManager:  streams,
		fileService:    &stubResourceFileService{},
		turns:          turns,
	}
	r.GET("/sessions/continue-stream/:session_id", h.ContinueStream)
	return r, messages
}

func getContinueStream(t *testing.T, r *gin.Engine) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sessions/continue-stream/sess1?message_id=msg1", nil))
	return recorder
}

func generatingMessage(idleFor time.Duration) *types.Message {
	return &types.Message{
		ID: "msg1", SessionID: "sess1", Role: "assistant", RequestID: "req-1",
		UpdatedAt: time.Now().Add(-idleFor),
	}
}

// A message that already ended has nothing to resume: the 404 says which
// state it is in, so the client renders it as is.
func TestContinueStreamTerminalMessageReportsStatus(t *testing.T) {
	for name, message := range map[string]*types.Message{
		"completed before status existed": {ID: "msg1", SessionID: "sess1", IsCompleted: true},
		"failed": {
			ID: "msg1", SessionID: "sess1", IsCompleted: true, Status: types.MessageStatusFailed,
		},
		"interrupted": {
			ID: "msg1", SessionID: "sess1", IsCompleted: true, Status: types.MessageStatusInterrupted,
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, messages := newHealRouter(t, message, &healStream{})

			w := getContinueStream(t, r)

			require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, false, body["success"])
			assert.Equal(t, "Incomplete message not found", body["error"])
			assert.Equal(t, string(message.EffectiveStatus()), body["status"])
			assert.Empty(t, messages.finalized, "a terminal message is never rewritten")
		})
	}
}

// Nothing is running for this message, its stream is gone and it is older
// than the grace period: end it here and tell the client once.
func TestContinueStreamHealsAbandonedTurn(t *testing.T) {
	for name, live := range map[string]string{"no live run": "", "live run names another message": "newer-run"} {
		t.Run(name, func(t *testing.T) {
			message := generatingMessage(3 * time.Minute)
			r, messages := newHealRouter(t, message, &healStream{live: live})

			w := getContinueStream(t, r)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Contains(t, w.Header().Get("Content-Type"), "text/event-stream")
			body := w.Body.String()
			assert.Equal(t, 1, strings.Count(body, "event:message"), "exactly one event: %s", body)
			assert.Contains(t, body, `"response_type":"error"`)
			assert.Contains(t, body, `"done":true`)
			assert.Contains(t, body, `"code":"`+types.TurnSSECodeInterrupted+`"`)
			assert.Contains(t, body, types.TurnInterruptedMessage("zh-CN"))

			require.Len(t, messages.finalized, 1)
			call := messages.finalized[0]
			assert.Equal(t, "sess1", call.sessionID)
			assert.Equal(t, "msg1", call.messageID)
			assert.Equal(t, types.MessageStatusInterrupted, call.status)
			assert.Equal(t, types.TurnErrorCodeProcessLost, call.code)
			assert.Nil(t, call.content, "the stored content is left alone")
			require.NoError(t, call.ctxErr)

			// The next open finds a terminal message: no second error, no
			// further attempt to resume (spec §4.3, AC6).
			w = getContinueStream(t, r)
			require.Equal(t, http.StatusNotFound, w.Code)
			assert.Contains(t, w.Body.String(), `"status":"interrupted"`)
			assert.Len(t, messages.finalized, 1)
		})
	}
}

// A marker that still names the message means an engine is generating it and
// has not emitted anything yet: the existing behaviour is kept.
func TestContinueStreamKeepsWaitingForLiveRunWithoutEvents(t *testing.T) {
	r, messages := newHealRouter(t, generatingMessage(3*time.Minute), &healStream{live: "msg1"})

	w := getContinueStream(t, r)

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "No stream events found")
	assert.Empty(t, messages.finalized)
}

// A quick answer publishes no marker; this process still generating it is
// the only witness, and it must win over the row's age.
func TestContinueStreamLeavesTurnGeneratingInThisProcessAlone(t *testing.T) {
	registry := turn.NewRegistry()
	unregister := registry.Register("sess1", "msg1", nil, nil)
	defer unregister()
	r, messages := newHealRouterWithTurns(t, generatingMessage(3*time.Minute), &healStream{}, registry)

	w := getContinueStream(t, r)

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "No stream events found")
	assert.Empty(t, messages.finalized)
}

// R1: a message younger than the grace period may simply be between its
// persist and its first event or marker.
func TestContinueStreamLeavesYoungMessageWithoutEventsAlone(t *testing.T) {
	r, messages := newHealRouter(t, generatingMessage(10*time.Second), &healStream{})

	w := getContinueStream(t, r)

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), `"status":"`+string(types.MessageStatusGenerating)+`"`)
	assert.Empty(t, messages.finalized)
}

// An unreadable marker is not proof that nothing runs.
func TestContinueStreamLiveRunLookupFailureIsNotProof(t *testing.T) {
	streams := &healStream{liveErr: stderrors.New("redis timeout")}
	r, messages := newHealRouter(t, generatingMessage(3*time.Minute), streams)

	w := getContinueStream(t, r)

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, messages.finalized)
}

// A failed repair must not claim the turn was interrupted.
func TestContinueStreamDoesNotClaimInterruptedWhenRepairFails(t *testing.T) {
	r, messages := newHealRouter(t, generatingMessage(3*time.Minute), &healStream{})
	messages.finalizeErr = stderrors.New("database unavailable")

	w := getContinueStream(t, r)

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.NotContains(t, w.Body.String(), types.TurnSSECodeInterrupted)
	assert.Equal(t, types.MessageStatusGenerating, messages.message.EffectiveStatus())
}

// With events to replay the turn is resumed, however old the row is: the
// self-heal only applies to a stream that is gone.
func TestContinueStreamReplaysOldGeneratingMessageWithEvents(t *testing.T) {
	message := generatingMessage(3 * time.Minute)
	streams := &healStream{}
	streams.events = []interfaces.StreamEvent{
		{ID: "answer-1", Type: types.ResponseTypeAnswer, Content: "kept"},
		{ID: "complete-1", Type: types.ResponseTypeComplete, Done: true},
	}
	r, messages := newHealRouter(t, message, streams)

	w := getContinueStream(t, r)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "kept")
	assert.Empty(t, messages.finalized)
}

// The turn ended between the read and the repair (its own process finished,
// a stop, another replica healed it): no interruption is claimed, and the
// client is told the state it actually ended in.
func TestContinueStreamReportsTurnThatEndedDuringRepair(t *testing.T) {
	r, messages := newHealRouter(t, generatingMessage(3*time.Minute), &healStream{})
	messages.endedAs = types.MessageStatusCompleted

	w := getContinueStream(t, r)

	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), types.TurnSSECodeInterrupted)
	assert.Contains(t, w.Body.String(), `"status":"completed"`)
	assert.Len(t, messages.finalized, 1)
}
