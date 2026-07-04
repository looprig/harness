package foreignloop

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
)

// snapshotReq is the actor-served committed-state query handshake. The actor is the
// SOLE owner of msgs/turnIndex (no locks), so a consistent read must go THROUGH the
// actor: Snapshot sends a request on l.snapshots and the actor replies a defensive
// clone on reply. reply is buffered(1) so the actor never blocks delivering it.
type snapshotReq struct {
	reply chan snapshotResult
}

// snapshotResult is a consistent, defensively-cloned view of the loop's committed
// state: the conversation thread and the turn count.
type snapshotResult struct {
	msgs      content.AgenticMessages
	turnIndex event.TurnIndex
}

// Snapshot returns a consistent view of the loop's committed conversation and turn
// count by querying the actor (the sole owner of that state), so the read never
// races a concurrent commit. Its signature is exactly loop.Loop.Snapshot's (the
// Backend contract). It returns a typed *SnapshotError if the loop has exited (its
// actor is gone) or ctx is done before the actor replies — never a partial view.
func (l *Loop) Snapshot(ctx context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	reply := make(chan snapshotResult, 1)
	select {
	case l.snapshots <- snapshotReq{reply: reply}:
	case <-l.Done:
		return nil, 0, &SnapshotError{Reason: SnapshotLoopExited}
	case <-ctx.Done():
		return nil, 0, &SnapshotError{Reason: SnapshotContextDone, Cause: ctx.Err()}
	}
	select {
	case res := <-reply:
		return res.msgs, res.turnIndex, nil
	case <-l.Done:
		return nil, 0, &SnapshotError{Reason: SnapshotLoopExited}
	case <-ctx.Done():
		return nil, 0, &SnapshotError{Reason: SnapshotContextDone, Cause: ctx.Err()}
	}
}
