package ttylog

import (
	"errors"
	"testing"
)

// TestRedirectError covers the typed error's message formatting and unwrapping.
func TestRedirectError(t *testing.T) {
	t.Parallel()

	cause := errors.New("boom")
	tests := []struct {
		name    string
		err     *RedirectError
		wantMsg string
	}{
		{name: "dup", err: &RedirectError{Op: "dup", Err: cause}, wantMsg: "ttylog: dup: boom"},
		{name: "dup2", err: &RedirectError{Op: "dup2", Err: cause}, wantMsg: "ttylog: dup2: boom"},
		{name: "dup3", err: &RedirectError{Op: "dup3", Err: cause}, wantMsg: "ttylog: dup3: boom"},
		{name: "redirect", err: &RedirectError{Op: "redirect", Err: cause}, wantMsg: "ttylog: redirect: boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.wantMsg {
				t.Errorf("Error() = %q, want %q", got, tt.wantMsg)
			}
			if !errors.Is(tt.err, cause) {
				t.Errorf("errors.Is(err, cause) = false, want true")
			}
		})
	}
}

// TestCaptureStdioNilDst verifies a nil destination is rejected with a
// *RedirectError and no Capture (fail secure: never silently leave stdio in a bad
// state).
func TestCaptureStdioNilDst(t *testing.T) {
	t.Parallel()

	cap, err := CaptureStdio(nil)
	var re *RedirectError
	if !errors.As(err, &re) {
		t.Fatalf("CaptureStdio(nil) err = %v, want *RedirectError", err)
	}
	if re.Op != "capture" {
		t.Errorf("RedirectError.Op = %q, want %q", re.Op, "capture")
	}
	if cap != nil {
		t.Errorf("CaptureStdio(nil) = %v, want nil", cap)
	}
}
