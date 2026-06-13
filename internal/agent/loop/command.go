package loop

import (
	"context"

	"github.com/inventivepotter/urvi/internal/content"
)

// Command is a request to the loop actor. Every command's Ack channel must be
// buffered (capacity >= 1) or always read: the actor sends acks synchronously, so
// a blocked ack send would stall the actor.
type Command interface{ isCommand() }

// StartTurn begins a new LLM turn. Ack receives nil on acceptance, a
// *TurnBusyError if a turn is already running or the loop is shutting down, or
// an *InvalidCommandError if a required field is nil.
// Events receives non-terminal events then one terminal event; the actor
// closes it after the terminal event is sent.
// Ctx is the parent for the turn: cancelling it cancels the turn.
// Events, Abandoned, and Ack are required and must be non-nil.
type StartTurn struct {
	Ctx       context.Context
	Input     []*content.Block
	Events    chan<- Event
	Abandoned <-chan struct{} // required; closed when caller no longer reads Events
	Ack       chan<- error
}

func (StartTurn) isCommand() {}

// Interrupt cancels the running turn. Ack receives true if a turn was cancelled,
// false if idle or the session is already shutting down (cancel already issued).
// Ack is required and must be non-nil.
type Interrupt struct {
	Ack chan<- bool
}

func (Interrupt) isCommand() {}

// Shutdown cancels the running turn (if any), delivers its terminal event, and
// exits the actor. Ack receives nil after clean exit, or *LoopTerminatedError
// if the loop's root context was cancelled before cleanup completed.
// Ack is required and must be non-nil.
type Shutdown struct {
	Ack chan<- error
}

func (Shutdown) isCommand() {}

func validateStartTurn(c StartTurn) error {
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
