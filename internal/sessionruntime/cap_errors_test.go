package sessionruntime

import (
	"errors"
	"testing"
)

// TestSessionCapErrors proves the two new spawn-cap SessionError kinds mirror the
// SessionClosing pattern: each renders a distinct, descriptive message, chains an
// optional Cause, and is errors.As-recoverable to *SessionError with the right Kind so a
// caller can branch on a depth vs quota refusal.
func TestSessionCapErrors(t *testing.T) {
	t.Parallel()
	cause := errors.New("underlying")
	tests := []struct {
		name     string
		err      *SessionError
		wantKind SessionErrorKind
		wantMsg  string
	}{
		{
			name:     "depth exceeded, no cause",
			err:      &SessionError{Kind: SessionLoopDepthExceeded},
			wantKind: SessionLoopDepthExceeded,
			wantMsg:  "session: loop spawn depth limit exceeded",
		},
		{
			name:     "quota exceeded, no cause",
			err:      &SessionError{Kind: SessionLoopQuotaExceeded},
			wantKind: SessionLoopQuotaExceeded,
			wantMsg:  "session: loop spawn quota exceeded",
		},
		{
			name:     "depth exceeded with cause chains",
			err:      &SessionError{Kind: SessionLoopDepthExceeded, Cause: cause},
			wantKind: SessionLoopDepthExceeded,
			wantMsg:  "session: loop spawn depth limit exceeded: underlying",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.wantMsg {
				t.Errorf("Error() = %q, want %q", got, tt.wantMsg)
			}
			// errors.As recovers the concrete type and the caller branches on Kind.
			var se *SessionError
			if !errors.As(error(tt.err), &se) {
				t.Fatalf("errors.As failed to recover *SessionError from %v", tt.err)
			}
			if se.Kind != tt.wantKind {
				t.Errorf("recovered Kind = %q, want %q", se.Kind, tt.wantKind)
			}
		})
	}
}

// TestSessionCapErrorsUnwrap proves Unwrap exposes the chained Cause (so errors.Is/As
// reach it) and returns nil when there is none.
func TestSessionCapErrorsUnwrap(t *testing.T) {
	t.Parallel()
	cause := errors.New("boom")
	if got := (&SessionError{Kind: SessionLoopQuotaExceeded, Cause: cause}).Unwrap(); !errors.Is(got, cause) {
		t.Errorf("Unwrap() = %v, want %v", got, cause)
	}
	if got := (&SessionError{Kind: SessionLoopDepthExceeded}).Unwrap(); got != nil {
		t.Errorf("Unwrap() = %v, want nil", got)
	}
}
