package command

import (
	"context"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
)

// StartTurn begins a new LLM turn. Ack receives nil on acceptance, a
// *TurnBusyError if a turn is already running or the loop is shutting down,
// or an *InvalidCommandError if a required field is nil.
// Events, Abandoned, and Ack are required and must be non-nil.
type StartTurn struct {
	Header
	Ctx       context.Context
	Input     []content.Block
	Events    chan<- event.Event
	Abandoned <-chan struct{}
	Ack       chan<- error
}

func (StartTurn) isCommand() {}

// Validate checks that all required fields are non-nil.
func (c StartTurn) Validate() error {
	switch {
	case c.Ctx == nil:
		return &InvalidCommandError{Command: CommandStartTurn, Field: StartTurnCtx}
	case c.Events == nil:
		return &InvalidCommandError{Command: CommandStartTurn, Field: StartTurnEvents}
	case c.Abandoned == nil:
		return &InvalidCommandError{Command: CommandStartTurn, Field: StartTurnAbandoned}
	case c.Ack == nil:
		return &InvalidCommandError{Command: CommandStartTurn, Field: StartTurnAck}
	default:
		return nil
	}
}

type TurnBusyReason string

const (
	TurnAlreadyRunning  TurnBusyReason = "turn already running"
	SessionShuttingDown TurnBusyReason = "session shutting down"
)

// TurnBusyError is returned on StartTurn.Ack when the loop cannot accept a turn.
type TurnBusyError struct{ Reason TurnBusyReason }

func (e *TurnBusyError) Error() string { return "loop: " + string(e.Reason) }

const (
	CommandStartTurn CommandName = "StartTurn"

	StartTurnCtx       CommandField = "Ctx"
	StartTurnEvents    CommandField = "Events"
	StartTurnAbandoned CommandField = "Abandoned"
	StartTurnAck       CommandField = "Ack"
)
