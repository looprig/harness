package command_test

import (
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/command"
)

func TestShutdownValidate(t *testing.T) {
	t.Parallel()
	ack := make(chan error)
	tests := []struct {
		name      string
		cmd       command.Shutdown
		wantField command.CommandField
		wantErr   bool
	}{
		{"valid", command.Shutdown{Ack: ack}, "", false},
		{"nil ack", command.Shutdown{}, command.ShutdownAck, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cmd.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var ice *command.InvalidCommandError
			if !errors.As(err, &ice) {
				t.Fatalf("err = %T, want *InvalidCommandError", err)
			}
			if ice.Field != tt.wantField {
				t.Errorf("Field = %q, want %q", ice.Field, tt.wantField)
			}
		})
	}
}

func TestLoopTerminatedError(t *testing.T) {
	t.Parallel()
	cause := errors.New("context canceled")
	tests := []struct {
		name    string
		err     *command.LoopTerminatedError
		wantMsg string
		unwrap  bool
	}{
		{"message", &command.LoopTerminatedError{Cause: cause}, "loop: terminated by context: context canceled", false},
		{"unwrap", &command.LoopTerminatedError{Cause: cause}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.unwrap {
				if !errors.Is(tt.err, cause) {
					t.Error("LoopTerminatedError does not unwrap to its Cause")
				}
				return
			}
			if got := tt.err.Error(); got != tt.wantMsg {
				t.Errorf("Error() = %q, want %q", got, tt.wantMsg)
			}
		})
	}
}
