package client

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsSSEStreamError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"typed", NewSSEStreamError("boom"), true},
		{"wrapped", fmt.Errorf("request failed: %w", NewSSEStreamError("boom")), true},
		{"legacy_string", fmt.Errorf("SSE stream error: boom"), true},
		{"http", fmt.Errorf("HTTP error 500: internal"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSSEStreamError(tc.err); got != tc.want {
				t.Errorf("IsSSEStreamError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestSSEStreamError_Unwrap(t *testing.T) {
	err := NewSSEStreamError("boom")
	if !errors.Is(err, ErrSSEStreamTerminal) {
		t.Fatal("expected ErrSSEStreamTerminal in chain")
	}
}

// BF-03: continue-stream's interrupted frame carries data.code so SDK callers
// can tell an abandoned turn from a model failure.
func TestIsTurnInterruptedError(t *testing.T) {
	code := streamErrorCode(map[string]interface{}{"code": "turn_interrupted"})
	interrupted := NewSSEStreamErrorWithCode("This answer was interrupted", code)
	if !IsTurnInterruptedError(interrupted) {
		t.Fatalf("data.code=turn_interrupted should be recognised: %v", interrupted)
	}
	if !IsTurnInterruptedError(fmt.Errorf("resume: %w", interrupted)) {
		t.Fatal("wrapped interrupted errors are still recognised")
	}
	if IsTurnInterruptedError(NewSSEStreamError("This answer was interrupted")) {
		t.Fatal("the text alone does not make it an interruption")
	}
	if IsTurnInterruptedError(NewSSEStreamErrorWithCode("boom", "model_error")) {
		t.Fatal("other codes are not interruptions")
	}
	if IsTurnInterruptedError(nil) {
		t.Fatal("nil is not an interruption")
	}
	if code := streamErrorCode(map[string]interface{}{"code": 42}); code != "" {
		t.Fatalf("non-string code read as %q", code)
	}
}

func TestMessageEffectiveStatus(t *testing.T) {
	cases := []struct {
		name string
		msg  *Message
		want string
	}{
		{"explicit wins", &Message{Status: TurnStatusFailed, IsCompleted: true}, TurnStatusFailed},
		{"legacy completed", &Message{IsCompleted: true}, TurnStatusCompleted},
		{"legacy generating", &Message{}, TurnStatusGenerating},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.msg.EffectiveStatus(); got != tc.want {
				t.Errorf("EffectiveStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}
