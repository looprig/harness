package coding

import (
	"context"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// newTestSpawner builds a codingSpawner over a real session.New and a fake client,
// mirroring the production wiring in newWithClient (build the spawner, build the
// tool set with it, construct the session, then late-bind spawner.session). The
// session is the live engine RunSubagent drives; t.Cleanup shuts it down so the
// actor goroutine never leaks. The returned session is also handed back so the
// test can attach a whole-session observer before calling Spawn.
func newTestSpawner(t *testing.T, client *fakeLLM) (*codingSpawner, *session.Session) {
	t.Helper()
	rootCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sp := &codingSpawner{root: "/tmp/workspace-root", httpCl: newHTTPClient(), client: client, spec: testSpec()}
	toolSet := buildToolSet(sp.root, sp.httpCl, sp)
	sess, err := session.New(rootCtx, loop.Config{Client: client, Model: testSpec(), Tools: toolSet})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })
	sp.session = sess // late-bind, exactly as newWithClient does
	return sp, sess
}

// TestSpawnRunsInSessionLoop proves the coding Spawner runs a subagent as an
// IN-SESSION loop via session.RunSubagent: Spawn drives a fresh sub-loop against
// the fake client (one no-tool final message) and returns that message's text. A
// whole-session observer attached BEFORE the call (the hub has no replay) proves
// the sub-loop announced itself (LoopStarted on a fresh, non-primary loop id) and
// that the turn it ran is attributed to that SAME sub-loop with MACHINE agency
// (Cause.Agency == AgencyMachine) — i.e. the submit was our code's, never a human's.
func TestSpawnRunsInSessionLoop(t *testing.T) {
	t.Parallel()

	sp, sess := newTestSpawner(t, &fakeLLM{chunks: []content.Chunk{textChunk("subagent "), textChunk("final")}})

	// Observer attached BEFORE Spawn so the sub-loop's opening LoopStarted +
	// TurnStarted cannot be missed.
	obs, err := sess.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents(observer): %v", err)
	}
	t.Cleanup(func() { _ = obs.Close() })

	// Parent provenance is the spawning loop (the primary, here) — non-zero LoopID so
	// the spawn is attributed to a real parent.
	parent := loop.Provenance{LoopID: sess.PrimaryLoopID()}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := sp.Spawn(ctx, parent, "do the thing")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got != "subagent final" {
		t.Errorf("Spawn text = %q, want %q", got, "subagent final")
	}

	subLoopID, ok := waitLoopStartedNonPrimary(t, obs, sess.PrimaryLoopID())
	if !ok {
		t.Fatal("never observed a LoopStarted for a fresh (non-primary) sub-loop")
	}
	ts, ok := waitTurnStartedOnLoop(t, obs, subLoopID)
	if !ok {
		t.Fatalf("never observed a TurnStarted attributed to sub-loop %v", subLoopID)
	}
	if ts.Cause.Agency != identity.AgencyMachine {
		t.Errorf("sub-loop TurnStarted Cause.Agency = %v, want AgencyMachine", ts.Cause.Agency)
	}
	if ts.Coordinates.LoopID != subLoopID {
		t.Errorf("sub-loop TurnStarted LoopID = %v, want sub-loop %v", ts.Coordinates.LoopID, subLoopID)
	}
}

// TestSpawnBuildsFreshToolSetPerCall proves each Spawn (and indeed each buildToolSet)
// builds an INDEPENDENT PermissionChecker, so a sub-loop's session-scope approval
// state can never leak into the parent's policy or a sibling's — the per-loop
// approval-isolation guarantee. buildToolSet is the per-call seam Spawn uses, so
// proving two calls yield distinct checkers proves the property without driving a
// recursive spawn.
func TestSpawnBuildsFreshToolSetPerCall(t *testing.T) {
	t.Parallel()

	sp := &codingSpawner{root: "/tmp/workspace-root", httpCl: newHTTPClient(), client: &fakeLLM{}, spec: testSpec()}

	first := buildToolSet(sp.root, sp.httpCl, sp)
	second := buildToolSet(sp.root, sp.httpCl, sp)

	if first.Permission == nil || second.Permission == nil {
		t.Fatal("buildToolSet returned a nil PermissionChecker")
	}
	if first.Permission == second.Permission {
		t.Error("buildToolSet returned the SAME PermissionChecker across calls; want a fresh one per call (per-loop approval isolation)")
	}
}

// waitLoopStartedNonPrimary reads the observer until a LoopStarted for a loop id
// other than primary arrives, returning that sub-loop id.
func waitLoopStartedNonPrimary(t *testing.T, sub event.Subscription, primary uuid.UUID) (uuid.UUID, bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return uuid.UUID{}, false
			}
			if ls, ok := ev.(event.LoopStarted); ok && ls.Coordinates.LoopID != primary {
				return ls.Coordinates.LoopID, true
			}
		case <-deadline:
			return uuid.UUID{}, false
		}
	}
}

// waitTurnStartedOnLoop reads the observer until a TurnStarted whose
// Coordinates.LoopID equals loopID arrives, returning it so the caller can inspect
// its Cause.
func waitTurnStartedOnLoop(t *testing.T, sub event.Subscription, loopID uuid.UUID) (event.TurnStarted, bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return event.TurnStarted{}, false
			}
			if ts, ok := ev.(event.TurnStarted); ok && ts.Coordinates.LoopID == loopID {
				return ts, true
			}
		case <-deadline:
			return event.TurnStarted{}, false
		}
	}
}
