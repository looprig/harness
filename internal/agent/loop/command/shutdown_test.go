package command_test

import (
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
)

func TestLoopTerminatedError(t *testing.T) {
	t.Parallel()
	cause := errors.New("context canceled")
	err := &command.LoopTerminatedError{Cause: cause}

	t.Run("message", func(t *testing.T) {
		t.Parallel()
		want := "loop: terminated by context: context canceled"
		if got := err.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})
	t.Run("unwrap", func(t *testing.T) {
		t.Parallel()
		if !errors.Is(err, cause) {
			t.Error("LoopTerminatedError does not unwrap to its Cause")
		}
	})
}
