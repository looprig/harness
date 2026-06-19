package command

import "github.com/inventivepotter/urvi/internal/uuid"

// CancelResult is the loop's answer to a CancelQueuedInput. It is a sealed
// interface: Cancelled when the queued input was removed before it committed,
// AlreadyCommitted when it had already started or folded (fail-secure: you cannot
// un-commit what is in the transcript).
type CancelResult interface{ isCancelResult() }

// Cancelled reports the queued input was found still queued and removed. It carries
// no InputID echo: the caller already holds the InputID it passed to
// CancelQueuedInput, so the empty marker suffices.
type Cancelled struct{}

// AlreadyCommitted reports the queued input had already started or folded into a
// turn (identified by TurnID) and so could not be retracted. It is ALSO the answer
// for an unknown / never-queued InputID: there is nothing to retract, so TurnID is
// simply the currently-active turn (zero if idle), not necessarily related to the
// cancelled id.
type AlreadyCommitted struct {
	TurnID uuid.UUID
}

func (Cancelled) isCancelResult()        {}
func (AlreadyCommitted) isCancelResult() {}
