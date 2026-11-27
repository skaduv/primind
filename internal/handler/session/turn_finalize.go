package session

import (
	"context"
	stderrors "errors"
	"strings"

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
// safe — the first ending wins. The outcome is still remembered on the
// in-memory message so a later full persist of the same message carries this
// decision instead of a default. It returns whether this call ended the turn.
func (h *Handler) finalizeTurn(ctx context.Context, streamCtx *sseStreamContext, outcome turnOutcome) bool {
	if streamCtx == nil || streamCtx.assistantMessage == nil || h.messageService == nil {
		return false
	}
	msg := streamCtx.assistantMessage
	streamCtx.endTurn()

	// The turn is over whatever happens to the request: a cancelled or
	// closed request must not cancel the terminal write.
	ctx = context.WithoutCancel(ctx)
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

	if msg.Status == "" {
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
