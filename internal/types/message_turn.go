package types

import (
	"strings"
	"time"
)

// MessageStatus is the lifecycle state of an assistant turn (BF-03).
//
// Every assistant message ends in exactly one of the terminal states. The
// empty value means the row predates the column; EffectiveStatus infers it
// from IsCompleted so readers never have to special-case old data.
type MessageStatus string

const (
	// MessageStatusGenerating means the turn is still producing output.
	MessageStatusGenerating MessageStatus = "generating"
	// MessageStatusCompleted means the answer finished normally.
	MessageStatusCompleted MessageStatus = "completed"
	// MessageStatusFailed means the turn ended with a service error
	// (retrieval, model, persistence, ...).
	MessageStatusFailed MessageStatus = "failed"
	// MessageStatusStopped means the user stopped the turn; whatever was
	// generated is kept.
	MessageStatusStopped MessageStatus = "stopped"
	// MessageStatusInterrupted means the process that was generating the
	// answer went away before it could write any other state.
	MessageStatusInterrupted MessageStatus = "interrupted"
)

// Turn error codes (Message.ErrorCode). They are machine-readable and stable;
// clients map them to localized text.
const (
	// TurnErrorCodeKBSearchFailed: a retrieval stage failed.
	TurnErrorCodeKBSearchFailed = "kb_search_failed"
	// TurnErrorCodeModelError: the chat model could not be loaded, called or
	// its stream broke.
	TurnErrorCodeModelError = "model_error"
	// TurnErrorCodeQAFailed: the pipeline failed outside retrieval and the
	// model call (history, memory recall, prompt assembly, ...).
	TurnErrorCodeQAFailed = "qa_failed"
	// TurnErrorCodeAgentError: the agent engine returned an error.
	TurnErrorCodeAgentError = "agent_error"
	// TurnErrorCodePersistFailed: the finished answer could not be saved.
	TurnErrorCodePersistFailed = "persist_failed"
	// TurnErrorCodeProcessLost: the generating process exited or the turn
	// was found abandoned with nothing still driving it.
	TurnErrorCodeProcessLost = "process_lost"
)

// TurnSSECodeInterrupted is the `data.code` of the single error event that
// continue-stream returns after it marked an abandoned turn interrupted. The
// client uses it to end the "replying" state quietly instead of raising an
// error toast.
const TurnSSECodeInterrupted = "turn_interrupted"

// ContinueStreamGracePeriod is how long a generating message with no stream
// events is given the benefit of the doubt before continue-stream decides
// nothing is driving it. It covers the window between persisting the row and
// the first event (or live-run marker) appearing (BF-03 R1).
const ContinueStreamGracePeriod = 2 * time.Minute

// TurnReconcileGracePeriod is how long the background reconciler waits before
// it finalizes a generating message that nothing is driving. It matches the
// stream TTL: before that, continue-stream can still replay the turn's events;
// after it, a row nobody repaired is by definition abandoned.
const TurnReconcileGracePeriod = time.Hour

// EffectiveStatus is the one way to read a message's turn state. A non-empty
// Status wins; otherwise the row predates the column and IsCompleted decides
// ("no longer generating" is exactly what IsCompleted always meant).
func (m *Message) EffectiveStatus() MessageStatus {
	if m == nil {
		return ""
	}
	if m.Status != "" {
		return m.Status
	}
	if m.IsCompleted {
		return MessageStatusCompleted
	}
	return MessageStatusGenerating
}

// IsTerminal reports whether the turn has stopped generating. A nil message is
// not a turn and reads as terminal so callers never wait on it.
func (m *Message) IsTerminal() bool {
	return m == nil || m.EffectiveStatus() != MessageStatusGenerating
}

// TurnInterruptedMessage is the user-facing text stored with, and streamed
// for, an interrupted turn. locale follows LanguageFromContextOrDefault.
func TurnInterruptedMessage(locale string) string {
	if strings.HasPrefix(locale, "zh") {
		return "本次回答已中断，可以重新提问"
	}
	return "This answer was interrupted. You can ask again."
}

// TurnFailedMessage is the user-facing text stored with a failed turn. It
// names the failing stage class only; the raw error stays in the logs.
func TurnFailedMessage(code, locale string) string {
	zh := strings.HasPrefix(locale, "zh")
	switch code {
	case TurnErrorCodeKBSearchFailed:
		if zh {
			return "知识库检索失败，回答未能生成"
		}
		return "Knowledge retrieval failed, so no answer was generated"
	case TurnErrorCodeModelError:
		if zh {
			return "模型调用失败，回答未能生成"
		}
		return "The model call failed, so no answer was generated"
	case TurnErrorCodeAgentError:
		if zh {
			return "智能体执行出错，回答未能完成"
		}
		return "The agent run failed, so the answer is incomplete"
	case TurnErrorCodePersistFailed:
		if zh {
			return "回答已生成，但保存失败"
		}
		return "The answer was generated but could not be saved"
	default:
		if zh {
			return "回答生成失败"
		}
		return "The answer could not be generated"
	}
}

// QAStageError is returned by the quick-answer pipeline when a stage fails.
// It names the stage and the plugin error type so the handler can record a
// precise turn error code; Error() and Unwrap keep the original error visible
// to every existing caller.
type QAStageError struct {
	Stage string // Pipeline EventType that failed
	Kind  string // PluginError.ErrorType, may be empty
	Err   error
}

func (e *QAStageError) Error() string { return e.Err.Error() }

func (e *QAStageError) Unwrap() error { return e.Err }

// TurnErrorCodeForStage maps a failed quick-answer stage to a turn error
// code. The model call is recognised by stage or by plugin error type since
// the model can fail before the stage runs (model lookup) or inside it.
func TurnErrorCodeForStage(stage, kind string) string {
	switch kind {
	case "get_chat_model_failed", "model_call_failed":
		return TurnErrorCodeModelError
	}
	switch EventType(stage) {
	case CHAT_COMPLETION_STREAM, CHAT_COMPLETION:
		return TurnErrorCodeModelError
	case QUERY_UNDERSTAND, CHUNK_SEARCH, CHUNK_SEARCH_PARALLEL, CHUNK_RERANK,
		WEB_FETCH, CHUNK_MERGE, FILTER_TOP_K, DATA_ANALYSIS, INTO_CHAT_MESSAGE:
		return TurnErrorCodeKBSearchFailed
	default:
		return TurnErrorCodeQAFailed
	}
}
