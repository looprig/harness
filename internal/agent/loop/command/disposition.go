package command

import "github.com/inventivepotter/urvi/internal/uuid"

// Disposition is the loop's answer to a submit (UserInput/SubagentResult). The
// caller branches on the concrete type rather than waiting for an event.TurnStarted
// that may never come. It is a sealed interface — only the variants below
// implement it — so a caller's type switch is exhaustive.
//
// The reply variants are deliberately separate from the events they may precede:
// Started means a turn was created (the event.TurnStarted follows on the stream);
// InputQueued is queue acceptance only, NOT a turn assignment — the queued input
// may later resolve as event.TurnStarted, event.TurnFoldedInto, or
// event.InputCancelled; TurnRejected is reply-only (nothing happened, so there is
// no event).
type Disposition interface{ isDisposition() }

// Started reports that the submit created a new turn. The event.TurnStarted for
// this turn follows on the per-turn stream / session fan-in. Named Started (not
// TurnStarted) to avoid colliding with the event.TurnStarted type.
type Started struct {
	TurnID  uuid.UUID
	InputID uuid.UUID
}

// InputQueued reports that the submit was accepted into the loop's pending-input
// queue (loopState.inbox) behind a running turn. It is NOT a turn assignment: the
// queued input later resolves to a turn start, a fold, or a cancellation.
type InputQueued struct {
	InputID uuid.UUID
}

// TurnRejected reports that the submit was refused without anything happening in
// the session (no event). Reason explains why.
type TurnRejected struct {
	Reason RejectReason
}

func (Started) isDisposition()      {}
func (InputQueued) isDisposition()  {}
func (TurnRejected) isDisposition() {}

// RejectReason explains a TurnRejected disposition.
type RejectReason uint8

const (
	// RejectBusy: the loop is running and the submit is not queueable
	// (StartOnly, or a non-queueable internal turn).
	RejectBusy RejectReason = iota
	// RejectQueueFull: the loop's inbox is at inboxCap (a length check; the actor
	// never blocks on a queue push).
	RejectQueueFull
	// RejectShuttingDown: the loop is shutting down and accepts no new input.
	RejectShuttingDown
)

// CancelResult is the loop's answer to a CancelQueuedInput. It is a sealed
// interface: Cancelled when the queued input was removed before it committed,
// AlreadyCommitted when it had already started or folded (fail-secure: you cannot
// un-commit what is in the transcript).
type CancelResult interface{ isCancelResult() }

// Cancelled reports the queued input was found still queued and removed.
type Cancelled struct{}

// AlreadyCommitted reports the queued input had already started or folded into a
// turn (identified by TurnID) and so could not be retracted.
type AlreadyCommitted struct {
	TurnID uuid.UUID
}

func (Cancelled) isCancelResult()        {}
func (AlreadyCommitted) isCancelResult() {}
