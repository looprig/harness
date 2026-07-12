package sessionruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hub"
)

// TestWatchRootLeaseFaultsSession proves the exclusive root-lease-loss path end to end: when
// the lease's Lost channel closes, watchRootLease faults the session (latches
// *WorkspaceRootLeaseLostError, closing admission) and cancels the session context
// (interrupting live loops + checkpoints). It is deterministic — no sleeps: closing the
// channel wakes the watcher, and the test blocks on sessionCtx.Done() (which the watcher
// cancels) before asserting.
func TestWatchRootLeaseFaultsSession(t *testing.T) {
	t.Parallel()
	id := mustUUID()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lost := make(chan struct{})
	s := &Session{
		sessionID:     id,
		hub:           hub.New(id),
		sessionCtx:    ctx,
		sessionCancel: cancel,
		wsLeaseLost:   lost,
		loops:         map[uuid.UUID]*loopHandle{},
	}

	s.watchRootLease()

	// Simulate the exclusive root lease being lost (expiry / hostile takeover).
	close(lost)

	// The watcher cancels sessionCtx after faulting — block on it (deterministic).
	<-s.sessionCtx.Done()

	// Admission is now closed: faultIfFaulted returns a SessionFaulted whose cause is the
	// typed lease-loss error.
	err := s.faultIfFaulted()
	if err == nil {
		t.Fatalf("faultIfFaulted() = nil, want a latched fault")
	}
	var lease *WorkspaceRootLeaseLostError
	if !errors.As(err, &lease) {
		t.Fatalf("faultIfFaulted() = %v, want cause *WorkspaceRootLeaseLostError", err)
	}
}
