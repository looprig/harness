package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/uuid"
)

// spawnChild spawns one sub-loop under parentLoopID, returning the new loop id. It
// builds the child's provenance from its parent loop id (Turn/Step are not part of the
// depth walk, which follows LoopID only) so a chain of these produces a real ancestor
// chain in the registry.
func spawnChild(t *testing.T, s *Session, parentLoopID uuid.UUID) uuid.UUID {
	t.Helper()
	id, err := s.NewLoop(loop.Provenance{LoopID: parentLoopID}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("NewLoop(parent=%v): %v", parentLoopID, err)
	}
	return id
}

// TestNewLoopDepthCap proves NewLoop rejects a spawn whose parent ancestor chain is
// already at the Depth limit with a typed *SessionError{SessionLoopDepthExceeded}, and
// that the rejection is PURE: it creates nothing (no registry entry) and publishes NO
// LoopStarted. The chain is built via chained Provenance so the depth walk has real
// ancestors to count.
func TestNewLoopDepthCap(t *testing.T) {
	t.Parallel()

	// Depth 2: a new loop whose ancestor chain length reaches 2 is refused. So the
	// primary may spawn A (chain [primary] = 1, allowed), A may spawn B (chain
	// [A, primary] = 2, refused).
	s, err := New(context.Background(),
		cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}),
		WithLimits(Limits{Depth: 2}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Subscribe BEFORE the rejected spawn so a (wrongly) published LoopStarted for the
	// refused loop would be observable. The hub has no replay, so attaching first is the
	// only way to catch it.
	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	// Chain: primary -> A (chain length 1, allowed).
	loopA := spawnChild(t, s, s.PrimaryLoopID())

	s.loopsMu.RLock()
	loopsBeforeReject := len(s.loops)
	s.loopsMu.RUnlock()

	// A -> B would be chain length 2 == Depth → rejected, pure (no loop, no LoopStarted).
	_, err = s.NewLoop(loop.Provenance{LoopID: loopA}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("never")}}))
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopDepthExceeded {
		t.Fatalf("NewLoop at depth limit err = %v, want *SessionError{SessionLoopDepthExceeded}", err)
	}

	// Pure rejection: the registry size is unchanged (nothing created).
	s.loopsMu.RLock()
	loopsAfterReject := len(s.loops)
	s.loopsMu.RUnlock()
	if loopsAfterReject != loopsBeforeReject {
		t.Errorf("registry size after rejected spawn = %d, want unchanged %d", loopsAfterReject, loopsBeforeReject)
	}

	// No LoopStarted was published for the refused loop. We saw exactly one LoopStarted
	// (loopA's); a second would be the refused spawn leaking an announcement.
	count := countLoopStarted(sub, 250*time.Millisecond)
	if count != 1 {
		t.Errorf("observed %d LoopStarted, want exactly 1 (only loopA; the refused spawn must publish none)", count)
	}
}

// TestNewLoopDepthCapDefault proves the DEFAULT depth (3) is enforced: a chain
// primary -> A -> B is allowed, and the spawn that would make a chain of length 3 (B's
// child) is refused.
func TestNewLoopDepthCapDefault(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	loopA := spawnChild(t, s, s.PrimaryLoopID()) // chain 1, allowed
	loopB := spawnChild(t, s, loopA)             // chain 2, allowed

	// B's child would be chain length 3 == default Depth → refused.
	_, err = s.NewLoop(loop.Provenance{LoopID: loopB}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("never")}}))
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopDepthExceeded {
		t.Fatalf("NewLoop at default depth limit err = %v, want *SessionError{SessionLoopDepthExceeded}", err)
	}
}

// countLoopStarted drains sub for d, returning how many LoopStarted events arrived. It
// is the no-replay observer: a LoopStarted published for a refused spawn would arrive
// here; none does.
func countLoopStarted(sub *hub.EventSubscription, d time.Duration) int {
	deadline := time.After(d)
	n := 0
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return n
			}
			if _, isLS := ev.(event.LoopStarted); isLS {
				n++
			}
		case <-deadline:
			return n
		}
	}
}
