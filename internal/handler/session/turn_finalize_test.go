package session

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"
	"time"

	"github.com/primind/primind/internal/event"
	"github.com/primind/primind/internal/turn"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// BF-03 T3/T6: every way an assistant turn can end, checked against a tiny
// in-memory message store that has the same conditional semantics as
// repository.FinalizeMessage, so the endings are tested without a database.

type finalizedCall struct {
	sessionID, messageID string
	status               types.MessageStatus
	code, message        string
	content              *string
	ctxErr               error
}

type turnMemory struct {
	interfaces.MessageService
	mu          sync.Mutex
	rows        map[string]*types.Message
	updates     []types.Message
	finalized   []finalizedCall
	updateErr   error
	finalizeErr error
}

func newTurnMemory(message *types.Message) *turnMemory {
	row := *message
	return &turnMemory{rows: map[string]*types.Message{message.ID: &row}}
}

func (m *turnMemory) UpdateMessage(_ context.Context, message *types.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateErr != nil {
		return m.updateErr
	}
	row := *message
	m.rows[message.ID] = &row
	m.updates = append(m.updates, row)
	return nil
}

// PersistTurnMessage mirrors repository.PersistTurnMessage: the full write
// lands while the row is generating or already in the same terminal state.
func (m *turnMemory) PersistTurnMessage(_ context.Context, message *types.Message) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateErr != nil {
		return false, m.updateErr
	}
	if current, ok := m.rows[message.ID]; ok && current.IsTerminal() && current.Status != message.Status {
		return false, nil
	}
	row := *message
	m.rows[message.ID] = &row
	m.updates = append(m.updates, row)
	return true, nil
}

func (m *turnMemory) GetMessage(_ context.Context, _ string, id string) (*types.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[id]
	if !ok {
		return nil, stderrors.New("message not found")
	}
	copied := *row
	return &copied, nil
}

func (m *turnMemory) FinalizeMessage(
	ctx context.Context, sessionID, messageID string, status types.MessageStatus,
	code, message string, content *string,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	call := finalizedCall{
		sessionID: sessionID, messageID: messageID, status: status, code: code,
		message: message, ctxErr: ctx.Err(),
	}
	if content != nil {
		copied := *content
		call.content = &copied
	}
	m.finalized = append(m.finalized, call)
	if m.finalizeErr != nil {
		return false, m.finalizeErr
	}
	row, ok := m.rows[messageID]
	if !ok || row.IsTerminal() {
		return false, nil
	}
	row.Status, row.ErrorCode, row.ErrorMessage, row.IsCompleted = status, code, message, true
	if content != nil {
		row.Content = *content
	}
	return true, nil
}

func (m *turnMemory) IndexMessageToKB(context.Context, string, string, string, string) {}

func (m *turnMemory) row(id string) types.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.rows[id]
}

func (m *turnMemory) finalizeCalls() []finalizedCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]finalizedCall(nil), m.finalized...)
}

// turnStream records the stream events and the live-run bookkeeping.
type turnStream struct {
	stubStreamManager
	mu       sync.Mutex
	appended []interfaces.StreamEvent
	cleared  []string
	live     string
	liveErr  error
	// onAppend, when set, sees each event as it is written to the stream.
	onAppend func(interfaces.StreamEvent)
}

func (s *turnStream) AppendEvent(_ context.Context, _, _ string, e interfaces.StreamEvent) error {
	s.mu.Lock()
	s.appended = append(s.appended, e)
	hook := s.onAppend
	s.mu.Unlock()
	if hook != nil {
		hook(e)
	}
	return nil
}

func (s *turnStream) eventsOfType(kind types.ResponseType) []interfaces.StreamEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []interfaces.StreamEvent
	for _, e := range s.appended {
		if e.Type == kind {
			out = append(out, e)
		}
	}
	return out
}

func (s *turnStream) GetLiveRun(context.Context, string) (string, string, error) {
	return s.live, "req", s.liveErr
}

func (s *turnStream) ClearLiveRun(_ context.Context, _, assistantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleared = append(s.cleared, assistantID)
	return nil
}

type turnSessionService struct {
	interfaces.SessionService
	knowledgeQA func(context.Context, *types.QARequest, *event.EventBus) error
	agentQA     func(context.Context, *types.QARequest, *event.EventBus) error
}

func (s *turnSessionService) KnowledgeQA(ctx context.Context, req *types.QARequest, bus *event.EventBus) error {
	if s.knowledgeQA == nil {
		return nil
	}
	return s.knowledgeQA(ctx, req, bus)
}

func (s *turnSessionService) AgentQA(ctx context.Context, req *types.QARequest, bus *event.EventBus) error {
	if s.agentQA == nil {
		return nil
	}
	return s.agentQA(ctx, req, bus)
}

// errorSeen remembers the last error event the client was shown.
type errorSeen struct {
	mu   sync.Mutex
	last *event.ErrorData
}

func (s *errorSeen) record() func(context.Context, event.Event) error {
	return func(_ context.Context, evt event.Event) error {
		if data, ok := evt.Data.(event.ErrorData); ok {
			s.mu.Lock()
			s.last = &data
			s.mu.Unlock()
		}
		return nil
	}
}

type turnRig struct {
	h         *Handler
	memory    *turnMemory
	streams   *turnStream
	sessions  *turnSessionService
	bus       *event.EventBus
	streamCtx *sseStreamContext
	reqCtx    *qaRequestContext
	registry  *turn.Registry
	cancel    context.CancelFunc
	seen      *errorSeen
}

func (r *turnRig) lastError() *event.ErrorData {
	r.seen.mu.Lock()
	defer r.seen.mu.Unlock()
	return r.seen.last
}

// newTurnRig wires a turn the way executeQA does, minus gin and the HTTP
// request: the same stream handler, the same event hooks for the mode, and
// the same in-process tracking.
func newTurnRig(t *testing.T, mode qaMode) *turnRig {
	t.Helper()
	message := &types.Message{ID: "a1", SessionID: "s1", Role: "assistant", RequestID: "req"}
	memory := newTurnMemory(message)
	streams := &turnStream{}
	registry := turn.NewRegistry()
	h := &Handler{messageService: memory, streamManager: streams, turns: registry}
	bus := event.NewEventBus()
	seen := &errorSeen{}
	bus.On(event.EventError, seen.record())

	base := types.WithExecutionTenant(context.Background(), uint64(1))
	asyncCtx, cancel := context.WithCancel(base)
	streamCtx := &sseStreamContext{
		eventBus:         bus,
		asyncCtx:         asyncCtx,
		cancel:           cancel,
		assistantMessage: message,
	}
	// Same order as setupSSEStream: the error listener stores the failure
	// before the stream handler forwards the frame.
	h.watchTurnErrors(streamCtx)
	streamCtx.streamHandler = h.setupStreamHandler(asyncCtx, "s1", "a1", "req", 1, time.Now(), message, bus)
	t.Cleanup(cancel)

	rig := &turnRig{
		h: h, memory: memory, streams: streams, sessions: &turnSessionService{}, bus: bus,
		streamCtx: streamCtx, registry: registry, cancel: cancel, seen: seen,
	}
	h.sessionService = rig.sessions
	rig.reqCtx = &qaRequestContext{
		ctx:              asyncCtx,
		sessionID:        "s1",
		requestID:        "req",
		query:            "what?",
		session:          &types.Session{ID: "s1", TenantID: 1},
		assistantMessage: message,
	}
	h.trackTurn(rig.reqCtx, streamCtx)
	// setupSSEStream registers the stop handler for both modes.
	h.setupStopEventHandler(bus, 1, streamCtx)
	if mode == qaModeNormal {
		h.watchQuickAnswerTurn(streamCtx, rig.reqCtx)
	}
	return rig
}

func TestQuickAnswerRetrievalFailureEndsTurnAsFailed(t *testing.T) {
	rig := newTurnRig(t, qaModeNormal)
	rig.sessions.knowledgeQA = func(context.Context, *types.QARequest, *event.EventBus) error {
		return &types.QAStageError{
			Stage: string(types.CHUNK_SEARCH_PARALLEL), Kind: "search_failed",
			Err: stderrors.New("opensearch: response too large"),
		}
	}

	rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeNormal)

	row := rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusFailed, row.Status)
	assert.Equal(t, types.TurnErrorCodeKBSearchFailed, row.ErrorCode)
	assert.Equal(t, types.TurnFailedMessage(types.TurnErrorCodeKBSearchFailed, "zh-CN"), row.ErrorMessage)
	assert.True(t, row.IsCompleted, "is_completed and status are written together (C1)")
	assert.Empty(t, row.Content)

	calls := rig.memory.finalizeCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, types.MessageStatusFailed, calls[0].status)
	// The terminal write must not be cancelled by the request ending.
	require.NoError(t, calls[0].ctxErr)

	// The client is told about the failure with the same code, after the
	// terminal state was written.
	errors := rig.streams.eventsOfType(types.ResponseTypeError)
	require.Len(t, errors, 1)
	assert.Equal(t, types.TurnErrorCodeKBSearchFailed, errors[0].Data["code"])
	assert.Equal(t, stageKnowledgeQA, errors[0].Data["stage"])
	assert.True(t, errors[0].Done)
	assert.Equal(t, types.TurnErrorCodeKBSearchFailed, rig.lastError().ErrorCode)

	assert.False(t, rig.registry.IsLive("s1", "a1"), "an ended turn leaves the in-process registry")
}

func TestQuickAnswerModelFailuresUseTheModelCode(t *testing.T) {
	for name, knowledgeQA := range map[string]func(context.Context, *types.QARequest, *event.EventBus) error{
		"synchronous model call failure": func(context.Context, *types.QARequest, *event.EventBus) error {
			return &types.QAStageError{
				Stage: string(types.CHAT_COMPLETION_STREAM), Kind: "model_call_failed",
				Err: stderrors.New("upstream 503"),
			}
		},
		// The stream goroutine reports a mid-stream failure after KnowledgeQA
		// returned nil: only the bus hook can see it.
		"mid-stream model failure": func(ctx context.Context, _ *types.QARequest, bus *event.EventBus) error {
			go func() {
				require.NoError(t, bus.Emit(ctx, event.Event{Type: event.EventError, Data: event.ErrorData{
					Stage: "chat_completion_stream", Error: "the model's response ended before it finished",
				}}))
			}()
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newTurnRig(t, qaModeNormal)
			rig.sessions.knowledgeQA = knowledgeQA
			// The stream may have produced part of an answer before failing.
			rig.streamCtx.assistantMessage.Content = "partial answer"

			rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeNormal)
			require.Eventually(t, func() bool { row := rig.memory.row("a1"); return row.IsTerminal() },
				2*time.Second, 10*time.Millisecond)

			row := rig.memory.row("a1")
			assert.Equal(t, types.MessageStatusFailed, row.Status)
			assert.Equal(t, types.TurnErrorCodeModelError, row.ErrorCode)
			assert.True(t, row.IsCompleted)
			assert.Equal(t, "partial answer", row.Content, "what was generated is kept")
		})
	}
}

func TestQuickAnswerSuccessEndsTurnAsCompletedAndBackstopChangesNothing(t *testing.T) {
	rig := newTurnRig(t, qaModeNormal)
	rig.sessions.knowledgeQA = func(ctx context.Context, _ *types.QARequest, bus *event.EventBus) error {
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventAgentFinalAnswer,
			Data: event.AgentFinalAnswerData{Content: "the full "},
		}))
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventAgentFinalAnswer,
			Data: event.AgentFinalAnswerData{Content: "answer", Done: true},
		}))
		return nil
	}

	rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeNormal)

	row := rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusCompleted, row.Status)
	assert.Equal(t, "the full answer", row.Content)
	assert.True(t, row.IsCompleted)
	assert.Empty(t, row.ErrorCode)
	require.Len(t, rig.streams.eventsOfType(types.ResponseTypeComplete), 1, "the client gets its completion event")

	// The backstop that may run after any turn: the conditional write must
	// leave a finished answer alone.
	rig.h.finalizeTurn(context.Background(), rig.streamCtx, interruptedOutcome(context.Background()))
	row = rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusCompleted, row.Status, "a success is never relabelled by the backstop")
	assert.Empty(t, row.ErrorCode)
	assert.Empty(t, rig.memory.finalizeCalls(),
		"this process already ended the turn, so the backstop does not even write")
}

func TestUserStopKeepsPartialContentAsStopped(t *testing.T) {
	rig := newTurnRig(t, qaModeNormal)
	rig.sessions.knowledgeQA = func(ctx context.Context, _ *types.QARequest, bus *event.EventBus) error {
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventAgentFinalAnswer,
			Data: event.AgentFinalAnswerData{Content: "half an "},
		}))
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventStop,
			Data: event.StopData{SessionID: "s1", MessageID: "a1", Reason: "user_requested"},
		}))
		return ctx.Err()
	}

	rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeNormal)

	row := rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusStopped, row.Status)
	assert.True(t, row.IsCompleted)
	assert.Equal(t, "half an ", row.Content, "a stop keeps whatever was generated")
	assert.Empty(t, row.ErrorCode, "a stop is not an error")
	assert.Empty(t, rig.memory.finalizeCalls(), "the stop is a full persist, not a conditional finalize")
	// A requested stop is not reported as a failure.
	assert.Empty(t, rig.streams.eventsOfType(types.ResponseTypeError))
}

func TestAgentTurnSuccessThenBackstopStaysCompleted(t *testing.T) {
	rig := newTurnRig(t, qaModeAgent)
	rig.sessions.agentQA = func(ctx context.Context, _ *types.QARequest, bus *event.EventBus) error {
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventAgentComplete,
			Data: event.AgentCompleteData{MessageID: "a1", FinalAnswer: "done"},
		}))
		return nil
	}

	rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeAgent)

	row := rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusCompleted, row.Status)
	assert.Equal(t, "done", row.Content)
	assert.True(t, row.IsCompleted)
	assert.Contains(t, rig.streams.cleared, "a1",
		"the finished turn releases its live-run marker (the backstop repeats the idempotent clear)")
	assert.Empty(t, rig.memory.finalizeCalls(),
		"the backstop sees the turn already ended in this process and writes nothing")
}

func TestAgentTurnServiceErrorEndsAsFailed(t *testing.T) {
	rig := newTurnRig(t, qaModeAgent)
	rig.sessions.agentQA = func(context.Context, *types.QARequest, *event.EventBus) error {
		return stderrors.New("engine: model refused the tool schema")
	}

	rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeAgent)

	row := rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusFailed, row.Status)
	assert.Equal(t, types.TurnErrorCodeAgentError, row.ErrorCode)
	assert.True(t, row.IsCompleted)
	assert.Equal(t, types.TurnErrorCodeAgentError, rig.lastError().ErrorCode)
}

func TestAgentTurnUserStopEndsAsStopped(t *testing.T) {
	rig := newTurnRig(t, qaModeAgent)
	rig.sessions.agentQA = func(ctx context.Context, _ *types.QARequest, bus *event.EventBus) error {
		// The engine still announces what it had when a stop cancels it.
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventAgentComplete,
			Data: event.AgentCompleteData{MessageID: "a1", FinalAnswer: "so far"},
		}))
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventStop,
			Data: event.StopData{SessionID: "s1", MessageID: "a1", Reason: "user_requested"},
		}))
		return ctx.Err()
	}

	rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeAgent)

	row := rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusStopped, row.Status)
	assert.Equal(t, "so far", row.Content)
	assert.True(t, row.IsCompleted)
	assert.Empty(t, rig.streams.eventsOfType(types.ResponseTypeError), "a stop is not reported as an error")
}

func TestPanicInsideTurnEndsAsInterrupted(t *testing.T) {
	for _, mode := range []qaMode{qaModeNormal, qaModeAgent} {
		mode := mode
		t.Run(map[qaMode]string{qaModeNormal: "quick answer", qaModeAgent: "agent"}[mode], func(t *testing.T) {
			rig := newTurnRig(t, mode)
			boom := func(context.Context, *types.QARequest, *event.EventBus) error {
				panic("nil pointer in the pipeline")
			}
			rig.sessions.knowledgeQA = boom
			rig.sessions.agentQA = boom

			rig.h.runTurn(rig.reqCtx, rig.streamCtx, mode) // the deferred recover keeps this from escaping

			row := rig.memory.row("a1")
			assert.Equal(t, types.MessageStatusInterrupted, row.Status)
			assert.Equal(t, types.TurnErrorCodeProcessLost, row.ErrorCode)
			assert.True(t, row.IsCompleted)
		})
	}
}

// The persistence failure path: the client is told, and the turn is ended as
// a failed save so it is not left generating for the reconciler.
func TestAgentPersistenceFailureEndsTurnAsFailedSave(t *testing.T) {
	rig := newTurnRig(t, qaModeAgent)
	rig.memory.updateErr = stderrors.New("database unavailable")

	rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeAgent)

	row := rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusFailed, row.Status)
	assert.Equal(t, types.TurnErrorCodePersistFailed, row.ErrorCode)
	assert.True(t, row.IsCompleted)
	streamErrors := rig.streams.eventsOfType(types.ResponseTypeError)
	require.Len(t, streamErrors, 1)
	assert.Equal(t, "message_persistence", streamErrors[0].Data["stage"])
	assert.Equal(t, types.TurnErrorCodePersistFailed, streamErrors[0].Data["code"])
}

// When the database is really gone the conditional write fails too: nothing
// is finalized here and the reconciler repairs the row later (spec §6.1).
func TestPersistenceFailureWithDatabaseDownLeavesRowForReconciler(t *testing.T) {
	rig := newTurnRig(t, qaModeAgent)
	rig.memory.updateErr = stderrors.New("database unavailable")
	rig.memory.finalizeErr = stderrors.New("database unavailable")

	rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeAgent)

	rowAfterFailure := rig.memory.row("a1")
	assert.False(t, rowAfterFailure.IsTerminal())
	streamErrors := rig.streams.eventsOfType(types.ResponseTypeError)
	require.Len(t, streamErrors, 1)
	assert.Equal(t, "message_persistence", streamErrors[0].Data["stage"])
}

// Graceful shutdown (T6): every turn still registered as generating here is
// ended as interrupted through the handler's own finalizeTurn.
func TestShutdownInterruptsLiveTurns(t *testing.T) {
	rig := newTurnRig(t, qaModeNormal)
	require.True(t, rig.registry.IsLive("s1", "a1"))
	rig.streamCtx.assistantMessage.Content = "streamed so far"

	n := rig.registry.InterruptAll(context.Background())

	assert.Equal(t, 1, n)
	row := rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusInterrupted, row.Status)
	assert.Equal(t, types.TurnErrorCodeProcessLost, row.ErrorCode)
	assert.Equal(t, "streamed so far", row.Content)
	assert.Error(t, rig.streamCtx.asyncCtx.Err(), "generation is stopped after the terminal write")
	assert.False(t, rig.registry.IsLive("s1", "a1"), "the ended turn no longer counts as live")

	// Interrupting again finds nothing live: no second terminal write.
	rig.registry.InterruptAll(context.Background())
	assert.Len(t, rig.memory.finalizeCalls(), 1)
}

func TestFailureOutcomeMapsStagesToCodes(t *testing.T) {
	ctx := context.Background()
	stageErr := func(stage types.EventType, kind string) error {
		return &types.QAStageError{Stage: string(stage), Kind: kind, Err: stderrors.New("x")}
	}
	for name, tc := range map[string]struct {
		stage string
		err   error
		code  string
	}{
		"retrieval stage": {
			stageKnowledgeQA, stageErr(types.CHUNK_RERANK, "rerank_failed"), types.TurnErrorCodeKBSearchFailed,
		},
		"model lookup": {
			stageKnowledgeQA, stageErr(types.LOAD_HISTORY, "get_chat_model_failed"), types.TurnErrorCodeModelError,
		},
		"history": {
			stageKnowledgeQA, stageErr(types.LOAD_HISTORY, "get_history_failed"), types.TurnErrorCodeQAFailed,
		},
		"untyped service err": {stageKnowledgeQA, stderrors.New("resolve kb: 500"), types.TurnErrorCodeQAFailed},
		"stream event":        {"chat_completion_stream", stderrors.New("broke"), types.TurnErrorCodeModelError},
		"agent":               {stageAgentExecution, stageErr("x", ""), types.TurnErrorCodeAgentError},
	} {
		outcome := failureOutcome(ctx, tc.stage, tc.err)
		assert.Equal(t, types.MessageStatusFailed, outcome.status, name)
		assert.Equal(t, tc.code, outcome.code, name)
		assert.Equal(t, types.TurnFailedMessage(tc.code, "zh-CN"), outcome.message, name)
	}
	// errors.Is / errors.As still see through the pipeline's typed error.
	sentinel := stderrors.New("sentinel")
	wrapped := &types.QAStageError{Stage: "chunk_merge", Kind: "search_failed", Err: sentinel}
	assert.True(t, stderrors.Is(wrapped, sentinel))
	assert.Equal(t, "sentinel", wrapped.Error())
}

// An agent engine failure is reported by the engine's own error event, which
// carries no code. The failure is stored before the client sees the frame, the
// frame carries the stored code, it is sent once, and the turn is not
// announced as complete.
func TestAgentEngineErrorIsStoredBeforeTheClientSeesIt(t *testing.T) {
	rig := newTurnRig(t, qaModeAgent)
	var statusAtFrame types.MessageStatus
	rig.streams.onAppend = func(e interfaces.StreamEvent) {
		if e.Type == types.ResponseTypeError {
			statusAtFrame = rig.memory.row("a1").Status
		}
	}
	engineErr := stderrors.New("engine: model returned 500")
	rig.sessions.agentQA = func(ctx context.Context, _ *types.QARequest, bus *event.EventBus) error {
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventError,
			Data: event.ErrorData{Error: engineErr.Error(), Stage: stageAgentExecution, SessionID: "s1"},
		}))
		return engineErr
	}

	rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeAgent)

	assert.Equal(t, types.MessageStatusFailed, statusAtFrame, "a reload right after the frame shows the failure")
	row := rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusFailed, row.Status)
	assert.Equal(t, types.TurnErrorCodeAgentError, row.ErrorCode)
	assert.True(t, row.IsCompleted)
	frames := rig.streams.eventsOfType(types.ResponseTypeError)
	require.Len(t, frames, 1, "the failure is reported once")
	assert.Equal(t, types.TurnErrorCodeAgentError, frames[0].Data["code"])
	assert.Empty(t, rig.streams.eventsOfType(types.ResponseTypeComplete), "a failed turn is not announced as complete")
}

// A turn another path already ended (a shutdown, the reconciler, another
// replica) is not relabelled by a late success, and this process takes the
// stored state instead of announcing a completion.
func TestAgentSuccessDoesNotRelabelATurnEndedElsewhere(t *testing.T) {
	rig := newTurnRig(t, qaModeAgent)
	rig.sessions.agentQA = func(ctx context.Context, _ *types.QARequest, bus *event.EventBus) error {
		rig.memory.mu.Lock()
		row := rig.memory.rows["a1"]
		row.Status, row.ErrorCode = types.MessageStatusInterrupted, types.TurnErrorCodeProcessLost
		row.IsCompleted = true
		rig.memory.mu.Unlock()
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventAgentComplete,
			Data: event.AgentCompleteData{MessageID: "a1", FinalAnswer: "late answer"},
		}))
		return nil
	}

	rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeAgent)

	row := rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusInterrupted, row.Status)
	assert.Equal(t, types.TurnErrorCodeProcessLost, row.ErrorCode)
	assert.Equal(t, types.MessageStatusInterrupted, rig.streamCtx.assistantMessage.Status,
		"memory follows the stored state")
	assert.Empty(t, rig.streams.eventsOfType(types.ResponseTypeComplete))
}

// Graceful shutdown cancels a running agent turn before it writes the
// terminal state. The run's own teardown sees a cancelled context, as after a
// user stop, but must record the shutdown: interrupted, not stopped.
func TestShutdownCancelledAgentTurnEndsAsInterruptedNotStopped(t *testing.T) {
	rig := newTurnRig(t, qaModeAgent)
	rig.sessions.agentQA = func(ctx context.Context, _ *types.QARequest, bus *event.EventBus) error {
		require.NoError(t, bus.Emit(ctx, event.Event{
			Type: event.EventAgentComplete,
			Data: event.AgentCompleteData{MessageID: "a1", FinalAnswer: "half an answer"},
		}))
		// SIGTERM arrives while the engine is still running.
		rig.registry.InterruptAll(context.Background())
		return ctx.Err()
	}

	rig.h.runTurn(rig.reqCtx, rig.streamCtx, qaModeAgent)

	row := rig.memory.row("a1")
	assert.Equal(t, types.MessageStatusInterrupted, row.Status)
	assert.Equal(t, types.TurnErrorCodeProcessLost, row.ErrorCode)
	assert.Equal(t, "half an answer", row.Content, "what was generated is kept")
	assert.Empty(t, rig.streams.eventsOfType(types.ResponseTypeComplete))
}
