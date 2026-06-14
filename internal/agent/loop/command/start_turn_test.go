package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	ev := make(chan event.Event)
	ab := make(chan struct{})
	ack := make(chan error)
	tests := []struct {
		name      string
		cmd       command.StartTurn
		wantField command.CommandField
		wantErr   bool
	}{
		{"valid", command.StartTurn{Ctx: ctx, Events: ev, Abandoned: ab, Ack: ack}, "", false},
		{"nil ctx", command.StartTurn{Events: ev, Abandoned: ab, Ack: ack}, command.StartTurnCtx, true},
		{"nil events", command.StartTurn{Ctx: ctx, Abandoned: ab, Ack: ack}, command.StartTurnEvents, true},
		{"nil abandoned", command.StartTurn{Ctx: ctx, Events: ev, Ack: ack}, command.StartTurnAbandoned, true},
		{"nil ack", command.StartTurn{Ctx: ctx, Events: ev, Abandoned: ab}, command.StartTurnAck, true},
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

func TestCommandErrorMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"turn busy running", &command.TurnBusyError{Reason: command.TurnAlreadyRunning}, "loop: turn already running"},
		{"turn busy shutdown", &command.TurnBusyError{Reason: command.SessionShuttingDown}, "loop: session shutting down"},
		{"invalid command", &command.InvalidCommandError{Command: command.CommandStartTurn, Field: command.StartTurnAbandoned}, "loop: invalid command: StartTurn.Abandoned is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}
