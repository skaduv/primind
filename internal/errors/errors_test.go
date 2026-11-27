package errors

import (
	"net/http"
	"testing"
)

// Both invite failures must be indistinguishable from the untyped 404 they
// replace except for the code: clients released before the typed codes read
// the status and the message verbatim.
func TestInviteCodeErrorsKeepLegacyStatusAndMessage(t *testing.T) {
	cases := []struct {
		name string
		err  *AppError
		code ErrorCode
	}{
		{name: "not found", err: NewInviteCodeNotFoundError(), code: ErrInviteCodeNotFound},
		{name: "expired", err: NewInviteCodeExpiredError(), code: ErrInviteCodeExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.HTTPCode != http.StatusNotFound {
				t.Fatalf("HTTPCode = %d, want %d", tc.err.HTTPCode, http.StatusNotFound)
			}
			if tc.err.Code != tc.code {
				t.Fatalf("Code = %d, want %d", tc.err.Code, tc.code)
			}
			if tc.err.Message != "Invalid invite code" {
				t.Fatalf("Message = %q, want the legacy English text", tc.err.Message)
			}
			if tc.err.Details != nil {
				t.Fatalf("Details = %v, want none: it would leak which case occurred", tc.err.Details)
			}
		})
	}
}

func TestInviteCodeErrorsAreDistinctCodes(t *testing.T) {
	if ErrInviteCodeNotFound == ErrInviteCodeExpired {
		t.Fatal("not-found and expired must carry different codes")
	}
	// The 2400 block is reserved for organization invite errors.
	if ErrInviteCodeNotFound != 2400 || ErrInviteCodeExpired != 2401 {
		t.Fatalf("invite codes moved: got %d/%d, want 2400/2401", ErrInviteCodeNotFound, ErrInviteCodeExpired)
	}
}
