package client

import (
	"errors"
	"fmt"
	"strings"
)

// ErrSSEStreamTerminal marks a terminal response_type=error, done=true frame
// from an SSE stream. The stream delivered the frame to the callback before
// returning the error.
var ErrSSEStreamTerminal = errors.New("SSE stream terminal error")

// SSEStreamError is returned when the server emits a terminal error frame on
// an SSE stream.
type SSEStreamError struct {
	Content string
	// Code is the frame's machine-readable `data.code` when it carried one,
	// otherwise empty. See TurnSSECodeInterrupted.
	Code string
}

func (e *SSEStreamError) Error() string {
	return fmt.Sprintf("SSE stream error: %s", e.Content)
}

func (e *SSEStreamError) Unwrap() error {
	return ErrSSEStreamTerminal
}

// TurnSSECodeInterrupted is the `data.code` of the single error event that
// continue-stream returns after it marked an abandoned turn interrupted
// (BF-03 §4.3): the turn had stopped generating without the server noticing
// and was just closed as `interrupted`. The message is not resumable; ask the
// question again.
const TurnSSECodeInterrupted = "turn_interrupted"

// NewSSEStreamError builds the terminal SSE stream error returned by the
// streaming readers after delivering the error frame to the callback.
func NewSSEStreamError(content string) error {
	return &SSEStreamError{Content: content}
}

// NewSSEStreamErrorWithCode is NewSSEStreamError keeping the frame's
// `data.code`, so callers can tell an interrupted turn from a model failure.
func NewSSEStreamErrorWithCode(content, code string) error {
	return &SSEStreamError{Content: content, Code: code}
}

// streamErrorCode reads `data.code` from a terminal error frame.
func streamErrorCode(data map[string]interface{}) string {
	if code, ok := data["code"].(string); ok {
		return code
	}
	return ""
}

// IsTurnInterruptedError reports whether err is continue-stream's
// "turn_interrupted" terminal frame (see TurnSSECodeInterrupted).
func IsTurnInterruptedError(err error) bool {
	var sse *SSEStreamError
	return errors.As(err, &sse) && sse.Code == TurnSSECodeInterrupted
}

// IsSSEStreamError reports whether err is a terminal SSE stream error from the
// SDK readers, including wrapped *SSEStreamError values and legacy
// fmt.Errorf("SSE stream error: ...") chains.
func IsSSEStreamError(err error) bool {
	var sse *SSEStreamError
	if errors.As(err, &sse) {
		return true
	}
	for err != nil {
		if strings.HasPrefix(err.Error(), "SSE stream error: ") {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}
