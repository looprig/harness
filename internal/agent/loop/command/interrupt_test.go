package command_test

import (
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
)

func TestInterruptValidate(t *testing.T) {
	t.Parallel()
	ack := make(chan bool)
	tests := []struct {
		name      string
		cmd       command.Interrupt
		wantField command.CommandField
		wantErr   bool
	}{
		{"valid", command.Interrupt{Ack: ack}, "", false},
		{"nil ack", command.Interrupt{}, command.InterruptAck, true},
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
