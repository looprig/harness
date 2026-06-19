package command

import "github.com/inventivepotter/urvi/internal/uuid"

// CancelQueuedInput retracts a still-queued submit (a UserInput/SubagentResult
// that received an InputQueued disposition). It is routed to the loop like the
// gate commands; the loop resolves it against its OWN inbox (race-free, since the
// actor is the sole owner of the queue), so there is no session-side TOCTOU.
//
// Route selects the target loop; InputID names the queued submit to remove (it is
// the submit command's Header.ID, returned in InputQueued.InputID). Ack is
// required and must be buffered(1): the loop replies a CancelResult exactly once
// via tryAck — Cancelled if the input was still queued and removed,
// AlreadyCommitted if it had already started or folded.
type CancelQueuedInput struct {
	Header
	Route   Route
	InputID uuid.UUID
	Ack     chan<- CancelResult
}

func (CancelQueuedInput) isCommand() {}
