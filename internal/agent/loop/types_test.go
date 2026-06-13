package loop

import (
	"context"
	"errors"
	"testing"
)

func TestErrorTypesAreTyped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"config missing client", &ConfigError{Kind: ConfigMissingClient}, "loop: config error: Config.Client is required"},
		{"turn busy running", &TurnBusyError{Reason: TurnAlreadyRunning}, "loop: turn already running"},
		{"turn busy shutdown", &TurnBusyError{Reason: SessionShuttingDown}, "loop: session shutting down"},
		{"empty response", &EmptyResponseError{}, "loop: empty response from provider"},
		{"turn panic", &TurnPanicError{Detail: "x"}, "loop: panic in turn goroutine: x"},
		{"invalid command", &InvalidCommandError{Command: CommandStartTurn, Field: StartTurnAbandoned}, "loop: invalid command: StartTurn.Abandoned is required"},
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

func TestConfigErrorUnwrap(t *testing.T) {
	t.Parallel()
	cause := errors.New("inner")
	err := &ConfigError{Kind: ConfigInvalidModel, Cause: cause}
	if !errors.Is(err, cause) {
		t.Error("ConfigError does not unwrap to its Cause")
	}
}

func TestValidateStartTurn(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	ev := make(chan Event)
	ab := make(chan struct{})
	ack := make(chan error)
	tests := []struct {
		name      string
		cmd       StartTurn
		wantField CommandField
		wantErr   bool
	}{
		{"valid", StartTurn{Ctx: ctx, Events: ev, Abandoned: ab, Ack: ack}, "", false},
		{"nil ctx", StartTurn{Events: ev, Abandoned: ab, Ack: ack}, StartTurnCtx, true},
		{"nil events", StartTurn{Ctx: ctx, Abandoned: ab, Ack: ack}, StartTurnEvents, true},
		{"nil abandoned", StartTurn{Ctx: ctx, Events: ev, Ack: ack}, StartTurnAbandoned, true},
		{"nil ack", StartTurn{Ctx: ctx, Events: ev, Abandoned: ab}, StartTurnAck, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateStartTurn(tt.cmd)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateStartTurn() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var ice *InvalidCommandError
			if !errors.As(err, &ice) {
				t.Fatalf("err = %T, want *InvalidCommandError", err)
			}
			if ice.Field != tt.wantField {
				t.Errorf("Field = %q, want %q", ice.Field, tt.wantField)
			}
		})
	}
}
