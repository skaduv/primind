package session

import (
	"context"
	stderrors "errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/primind/primind/internal/event"
	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types"
)

// Stage names the error events carry. They also decide the turn error code
// when the service error carries no stage of its own.
const (
	stageKnowledgeQA    = "knowledge_qa_execution"
	stageAgentExecution = "agent_execution"
)

// turnOutcome is the terminal state an assistant turn ends with, together
// with the machine-readable reason and the user-facing explanation stored
// on the message.
type turnOutcome struct {
	status  types.MessageStatus
	code    string
	message string
}

// interruptedOutcome is the state of a turn whose process could not run its
// own teardown: a panic, a shutdown, or a row the reconciler found abandoned.
func interruptedOutcome(ctx context.Context) turnOutcome {
	return turnOutcome{
		status:  types.MessageStatusInterrupted,
		code:    types.TurnErrorCodeProcessLost,
		message: types.TurnInterruptedMessage(types.LanguageFromContextOrDefault(ctx)),
	}
}

// failureOutcome maps a service or stream error to the failed outcome. The
// quick-answer pipeline reports the stage that failed; everything else is an
// agent engine error.
func failureOutcome(ctx context.Context, stage string, err error) turnOutcome {
	code := types.TurnErrorCodeAgentError
	if stage != stageAgentExecution {
		code = types.TurnErrorCodeQAFailed
		var stageErr *types.QAStageError
		if stderrors.As(err, &stageErr) {
			code = types.TurnErrorCodeForStage(stageErr.Stage, stageErr.Kind)
		} else if stage != "" {
			code = types.TurnErrorCodeForStage(stage, "")
		}
	}
	return turnOutcome{
		status:  types.MessageStatusFailed,
		code:    code,
		message: types.TurnFailedMessage(code, types.LanguageFromContextOrDefault(ctx)),
	}
}

// turnWriteTimeout bounds a terminal-state write. The write must outlive a
// cancelled request, but not hang a shutdown or a goroutine on a stuck
// database: the reconciler ends a row whose write never landed.
var turnWriteTimeout = 10 * time.Second

// turnWriteContext detaches ctx from its cancellation (the turn is over
// whatever happens to the request) and gives the write its own deadline.
func turnWriteContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// finalizeTurn is how an assistant turn ends in any state other than a
// successful answer or a user stop, and the last resort for both of those.
//
// Every new generation path MUST end here or in completeAssistantMessage
// (BF-03 §9): a turn that ends anywhere else leaves is_completed=false
// forever, and every open of its session then asks continue-stream for a
// stream that no longer exists.
//
// The write is conditional (see repository.FinalizeMessage): a turn that
// already reached a terminal state is not touched, so calling this after a
// success, a stop, an earlier failure or from the deferred backstop is always
// safe — the first ending wins. The in-memory message follows the stored
// result: it takes the outcome when this call ended the turn, and also when
// the write failed, so the later full persist carries this decision instead
// of a default (if that write fails too, the reconciler ends the row). A call
// that lost the race leaves memory alone; the full persist then reads back
// the state that won. It returns whether this call ended the turn.
func (h *Handler) finalizeTurn(ctx context.Context, streamCtx *sseStreamContext, outcome turnOutcome) bool {
	if streamCtx == nil || streamCtx.assistantMessage == nil || h.messageService == nil {
		return false
	}
	msg := streamCtx.assistantMessage
	streamCtx.endTurn()

	ctx, cancel := turnWriteContext(ctx, turnWriteTimeout)
	defer cancel()

	streamCtx.turnMu.Lock()
	defer streamCtx.turnMu.Unlock()
	if turnStatusDecided(msg) {
		// This process already ended the turn; the stored row says so or a
		// full persist is about to write it.
		return false
	}
	var content *string
	if strings.TrimSpace(msg.Content) != "" {
		partial := msg.Content
		content = &partial
	}

	applied, err := h.messageService.FinalizeMessage(
		ctx, msg.SessionID, msg.ID, outcome.status, outcome.code, outcome.message, content,
	)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": msg.SessionID,
			"message_id": msg.ID,
			"status":     outcome.status,
			"code":       outcome.code,
		})
	} else {
		logger.Warnf(ctx, "Turn %s in session %s ended as %s (code=%q applied=%t)",
			msg.ID, msg.SessionID, outcome.status, outcome.code, applied)
	}

	if err != nil || applied {
		msg.Status, msg.ErrorCode, msg.ErrorMessage = outcome.status, outcome.code, outcome.message
		msg.IsCompleted = true
	}

	// A finished turn owns no live-run marker any more. ClearLiveRun is a
	// compare-and-set on the message id, so a follow-up run that already
	// claimed the session keeps its marker.
	if err == nil && h.streamManager != nil {
		if clearErr := h.streamManager.ClearLiveRun(ctx, msg.SessionID, msg.ID); clearErr != nil {
			logger.Warnf(ctx, "live run cleanup failed for session %s: %v", msg.SessionID, clearErr)
		}
	}
	return err == nil && applied
}

// turnEndedWithoutAnswer reports whether the turn already ended as failed or
// interrupted, i.e. it has no answer to complete. Read under turnMu: a
// shutdown interrupt may write it from another goroutine.
func (sc *sseStreamContext) turnEndedWithoutAnswer() bool {
	if sc == nil || sc.assistantMessage == nil {
		return false
	}
	sc.turnMu.Lock()
	defer sc.turnMu.Unlock()
	switch sc.assistantMessage.Status {
	case types.MessageStatusFailed, types.MessageStatusInterrupted:
		return true
	default:
		return false
	}
}

// turnStatusDecided reports whether this process already decided how the
// in-flight turn ends. Only Status counts: the stream handler sets
// IsCompleted on the in-memory message as soon as the answer is complete,
// before anything is stored, so EffectiveStatus would misread a turn that is
// still being persisted (or about to be stopped) as completed.
func turnStatusDecided(msg *types.Message) bool {
	return msg.Status != "" && msg.Status != types.MessageStatusGenerating
}

// reportTurnEndedWithoutAnswer sends the live client the one error frame that
// ends a turn stored as failed or interrupted, unless it already got one. An
// interrupted turn uses the same quiet code as continue-stream's self-heal.
func (h *Handler) reportTurnEndedWithoutAnswer(ctx context.Context, streamCtx *sseStreamContext) {
	if streamCtx == nil || streamCtx.streamHandler == nil || streamCtx.streamHandler.reportedError() {
		return
	}
	streamCtx.turnMu.Lock()
	msg := *streamCtx.assistantMessage
	streamCtx.turnMu.Unlock()

	data := event.ErrorData{
		Error:     msg.ErrorMessage,
		ErrorCode: msg.ErrorCode,
		Stage:     "turn_ended",
		SessionID: msg.SessionID,
	}
	if msg.Status == types.MessageStatusInterrupted {
		data.ErrorCode = types.TurnSSECodeInterrupted
		data.Stage = turnInterruptedStage
		if data.Error == "" {
			data.Error = types.TurnInterruptedMessage(types.LanguageFromContextOrDefault(ctx))
		}
	}
	_ = streamCtx.streamHandler.handleError(ctx, event.Event{
		ID: uuid.New().String(), Type: event.EventError, SessionID: msg.SessionID, Data: data,
	})
}
