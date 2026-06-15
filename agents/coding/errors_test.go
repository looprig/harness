package coding

import (
	"errors"
	"strings"
	"testing"
)

// errSentinel is a leaf cause the wrapping-error tests assert on via errors.Is.
var errSentinel = errors.New("underlying cause")

// TestMissingEnvError proves Error() names the offending variable and that the
// concrete type (with its Var field) is recoverable via errors.As. MissingEnvError
// carries no cause, so it has no Unwrap.
func TestMissingEnvError(t *testing.T) {
	t.Parallel()
	err := error(&MissingEnvError{Var: "LLM_API_KEY"})

	for _, want := range []string{"coding", "LLM_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to contain %q", err.Error(), want)
		}
	}

	var me *MissingEnvError
	if !errors.As(err, &me) {
		t.Fatalf("errors.As(*MissingEnvError) failed")
	}
	if me.Var != "LLM_API_KEY" {
		t.Errorf("Var = %q, want LLM_API_KEY", me.Var)
	}
}

// TestWorkspaceRootError proves Error() renders a sensible message with and
// without a cause, and that Unwrap returns the wrapped cause (nil when there is
// none).
func TestWorkspaceRootError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cause     error
		wantSubs  []string
		wantNoSub string
	}{
		{
			name:      "no cause",
			cause:     nil,
			wantSubs:  []string{"coding", "workspace root"},
			wantNoSub: errSentinel.Error(),
		},
		{
			name:     "with cause",
			cause:    errSentinel,
			wantSubs: []string{"coding", "workspace root", errSentinel.Error()},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := error(&WorkspaceRootError{Cause: tt.cause})

			msg := err.Error()
			if msg == "" {
				t.Fatalf("Error() is empty")
			}
			for _, sub := range tt.wantSubs {
				if !strings.Contains(msg, sub) {
					t.Errorf("Error() = %q, want it to contain %q", msg, sub)
				}
			}
			if tt.wantNoSub != "" && strings.Contains(msg, tt.wantNoSub) {
				t.Errorf("Error() = %q, want it NOT to contain %q", msg, tt.wantNoSub)
			}

			var we *WorkspaceRootError
			if !errors.As(err, &we) {
				t.Fatalf("errors.As(*WorkspaceRootError) failed")
			}
			if got := errors.Unwrap(err); got != tt.cause {
				t.Errorf("Unwrap() = %v, want %v", got, tt.cause)
			}
			if tt.cause != nil && !errors.Is(err, tt.cause) {
				t.Errorf("errors.Is(err, cause) = false, want true")
			}
		})
	}
}

// TestSubagentTurnError proves Error() renders a sensible message with and
// without a cause, and that Unwrap returns the wrapped cause (nil when there is
// none) — recoverable via errors.Is/errors.As.
func TestSubagentTurnError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cause     error
		wantSubs  []string
		wantNoSub string
	}{
		{
			name:      "no cause",
			cause:     nil,
			wantSubs:  []string{"coding", "subagent"},
			wantNoSub: errSentinel.Error(),
		},
		{
			name:     "with cause",
			cause:    errSentinel,
			wantSubs: []string{"coding", "subagent", errSentinel.Error()},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := error(&SubagentTurnError{Cause: tt.cause})

			msg := err.Error()
			if msg == "" {
				t.Fatalf("Error() is empty")
			}
			for _, sub := range tt.wantSubs {
				if !strings.Contains(msg, sub) {
					t.Errorf("Error() = %q, want it to contain %q", msg, sub)
				}
			}
			if tt.wantNoSub != "" && strings.Contains(msg, tt.wantNoSub) {
				t.Errorf("Error() = %q, want it NOT to contain %q", msg, tt.wantNoSub)
			}

			var ste *SubagentTurnError
			if !errors.As(err, &ste) {
				t.Fatalf("errors.As(*SubagentTurnError) failed")
			}
			if got := errors.Unwrap(err); got != tt.cause {
				t.Errorf("Unwrap() = %v, want %v", got, tt.cause)
			}
			if tt.cause != nil && !errors.Is(err, tt.cause) {
				t.Errorf("errors.Is(err, cause) = false, want true")
			}
		})
	}
}

// TestSubagentInterruptedError proves Error() returns a non-empty interrupt
// message and that the concrete type is recoverable via errors.As. It carries no
// cause, so it has no Unwrap.
func TestSubagentInterruptedError(t *testing.T) {
	t.Parallel()
	err := error(&SubagentInterruptedError{})

	for _, want := range []string{"coding", "interrupted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to contain %q", err.Error(), want)
		}
	}

	var sie *SubagentInterruptedError
	if !errors.As(err, &sie) {
		t.Fatalf("errors.As(*SubagentInterruptedError) failed")
	}
}
