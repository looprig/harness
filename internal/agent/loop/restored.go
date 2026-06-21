package loop

import (
	"context"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// RestoredState is the pre-built committed state a restored loop comes up with: the
// folded message history and the turn count from the durable journal. It is the loop
// half of the Restore constructor's payoff — the session folds a loop's Enduring
// events (foldPrimaryLoop) into these two values and seeds a fresh actor with them so
// the resumed loop's history is byte-for-byte what it committed before teardown.
//
// Msgs is the committed conversation ONLY — it does NOT carry a SystemMessage. The
// loop never stores the system prompt in loopState.msgs; the prompt rides
// Config.Model.System and is sent on every request, so a restored loop "re-seeds" the
// system prompt simply by carrying the same Config. TurnIndex is the count of turns
// already started, so the next live turn numbers from TurnIndex+1 (installActiveTurn
// increments it), continuing the loop's numbering without a gap.
type RestoredState struct {
	Msgs      content.AgenticMessages
	TurnIndex event.TurnIndex
}

// NewRestored constructs a loop SEEDED with pre-built committed state and starts its
// actor goroutine IDLE — the restore counterpart to New. New spawns an empty loop that
// commits its first message at the first submit; NewRestored seeds loopState.msgs +
// turnIndex from the journal fold so the resumed loop already holds its prior history
// and numbers its next turn correctly. Everything else is identical to New: the same
// config validation/defaulting, the same actor goroutine, the same idle status — the
// ONLY difference is the seeded initial state.
//
// loopCtx, sessionID, loopID, events, and cfg mean exactly what they do in New. loopID
// MUST be the loop's ORIGINAL id (the session passes the primary loop's recovered id)
// so identity is stable across restore. seed is the folded committed state; a zero
// RestoredState (empty Msgs, zero TurnIndex) yields a loop indistinguishable from a
// freshly New'd one.
func NewRestored(loopCtx context.Context, sessionID, loopID uuid.UUID, events eventPublisher, cfg Config, seed RestoredState) (*Loop, error) {
	return newLoopWithSeed(loopCtx, sessionID, loopID, Provenance{}, events, cfg, &seed)
}

// snapshotRequest is the actor-served committed-state query handshake. The actor is the
// SOLE mutator of loopState.msgs/turnIndex (no locks), so a consistent read must go
// THROUGH the actor: a caller sends a request on cfg.snapshots and the actor replies a
// defensive clone on reply. reply is buffered(1) so the actor never blocks delivering
// it. This is the restore-verification + future-snapshot primitive (Snapshot below).
type snapshotRequest struct {
	reply chan<- loopSnapshot
}

// loopSnapshot is a consistent, defensively-cloned view of the loop's committed state
// returned by Snapshot: the conversation history and the turn count. Both are read by
// the actor and cloned before hand-off so the caller can never race the live state.
type loopSnapshot struct {
	msgs      content.AgenticMessages
	turnIndex event.TurnIndex
}

// Snapshot returns a consistent view of the loop's committed conversation and turn
// count by querying the actor (the sole owner of loopState), so the read never races a
// concurrent commit. It is the restore-verification primitive (the session proves a
// restored loop's history matches the original) and the hook a future dormant-snapshot
// writer reads from. It returns a typed *SnapshotError if the loop has exited (its
// actor is gone) or ctx is done before the actor replies — never a partial or zero
// view.
func (l *Loop) Snapshot(ctx context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	reply := make(chan loopSnapshot, 1)
	select {
	case l.snapshots <- snapshotRequest{reply: reply}:
	case <-l.Done:
		return nil, 0, &SnapshotError{Reason: SnapshotLoopExited}
	case <-ctx.Done():
		return nil, 0, &SnapshotError{Reason: SnapshotContextDone, Cause: ctx.Err()}
	}
	select {
	case snap := <-reply:
		return snap.msgs, snap.turnIndex, nil
	case <-l.Done:
		return nil, 0, &SnapshotError{Reason: SnapshotLoopExited}
	case <-ctx.Done():
		return nil, 0, &SnapshotError{Reason: SnapshotContextDone, Cause: ctx.Err()}
	}
}

// SnapshotErrorReason classifies why a Snapshot could not return a consistent view.
type SnapshotErrorReason string

const (
	// SnapshotLoopExited means the actor goroutine has exited (Loop.Done closed), so
	// there is no live state to read.
	SnapshotLoopExited SnapshotErrorReason = "loop_exited"
	// SnapshotContextDone means the caller's context was cancelled before the actor
	// replied.
	SnapshotContextDone SnapshotErrorReason = "context_done"
)

// SnapshotError is returned by Snapshot when it cannot obtain a consistent view of the
// loop's committed state. Cause chains the underlying ctx error when present.
type SnapshotError struct {
	Reason SnapshotErrorReason
	Cause  error
}

func (e *SnapshotError) Error() string {
	switch e.Reason {
	case SnapshotLoopExited:
		return "loop: snapshot failed: loop exited"
	case SnapshotContextDone:
		if e.Cause != nil {
			return "loop: snapshot failed: context done: " + e.Cause.Error()
		}
		return "loop: snapshot failed: context done"
	default:
		return "loop: snapshot failed"
	}
}

func (e *SnapshotError) Unwrap() error { return e.Cause }
