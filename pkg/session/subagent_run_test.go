package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
)

// TestRunSubagentReturnsFinalText drives the exported composition end-to-end on a
// REAL sub-loop: RunSubagent creates the sub-loop, runs one machine-originated turn
// against a stub LLM that emits a single no-tool final message, and returns that
// message's text with a nil error. It also asserts, via a SEPARATE whole-session
// subscription set up BEFORE the call, that the sub-loop announced itself
// (LoopStarted on a fresh, non-primary loop id) and that the turn it ran is
// attributed to the sub-loop (TurnStarted on the SAME loop id) with machine agency
// (Cause.Agency == AgencyMachine) — proving the submit was AgencyMachine, never a
// human's.
func TestRunSubagentReturnsFinalText(t *testing.T) {
	t.Parallel()

	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Whole-session observer attached BEFORE RunSubagent, so the sub-loop's opening
	// LoopStarted + TurnStarted (the hub has no replay) cannot be missed.
	obs, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents(observer): %v", err)
	}
	t.Cleanup(func() { _ = obs.Close() })

	// The sub-loop's FRESH cfg (its own client/ToolSet/PermissionChecker per the
	// contract) — here a stub that yields one no-tool final message.
	subCfg := cfg(&stubLLM{chunks: []content.Chunk{textChunk("subagent final")}})

	// Parent provenance is the spawning loop (the primary, here) — non-zero LoopID so
	// the spawn is attributed to a real parent.
	parent := loop.Provenance{LoopID: s.PrimaryLoopID()}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := s.RunSubagent(ctx, parent, subCfg, textBlocks("do the thing"), "")
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if got != "subagent final" {
		t.Errorf("RunSubagent text = %q, want %q", got, "subagent final")
	}

	// On the observer: find the sub-loop's LoopStarted (a non-primary loop id) and the
	// TurnStarted attributed to that SAME loop id with machine agency.
	subLS, ok := waitLoopStartedNonPrimaryEvent(t, obs, s.PrimaryLoopID())
	if !ok {
		t.Fatal("never observed a LoopStarted for a fresh (non-primary) sub-loop")
	}
	subLoopID := subLS.Coordinates.LoopID
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

// TestRunSubagentStampsParentToolUseID proves the durable correlation carrier:
// RunSubagent's parentToolUseID rides through the private loop-creation path onto
// the child sub-loop's LoopStarted (ParentToolUseID == the supplied id), while a
// plain loop not spawned by a tool call (the primary) carries the empty string.
// It asserts against the REAL LoopStarted observed via a whole-session
// subscription (never a struct copy), so the field is proven end-to-end through
// PublishEvent.
func TestRunSubagentStampsParentToolUseID(t *testing.T) {
	t.Parallel()

	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Observer attached BEFORE RunSubagent so the sub-loop's opening LoopStarted
	// (no hub replay) cannot be missed.
	obs, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents(observer): %v", err)
	}
	t.Cleanup(func() { _ = obs.Close() })

	subCfg := cfg(&stubLLM{chunks: []content.Chunk{textChunk("subagent final")}})
	parent := loop.Provenance{LoopID: s.PrimaryLoopID()}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := s.RunSubagent(ctx, parent, subCfg, textBlocks("do the thing"), "toolu_7"); err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}

	// The sub-loop's LoopStarted carries the supplied tool-use id.
	subLS, ok := waitLoopStartedNonPrimaryEvent(t, obs, s.PrimaryLoopID())
	if !ok {
		t.Fatal("never observed a LoopStarted for a fresh (non-primary) sub-loop")
	}
	if subLS.ParentToolUseID != "toolu_7" {
		t.Errorf("sub-loop LoopStarted.ParentToolUseID = %q, want %q", subLS.ParentToolUseID, "toolu_7")
	}

	// A plain NewLoop (the primary, not spawned by a tool call) carries the empty id.
	primLoopID, err := s.NewLoop(loop.Provenance{LoopID: s.PrimaryLoopID()}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("child")}}))
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	plainLS, ok := waitLoopStartedOnLoop(t, obs, primLoopID)
	if !ok {
		t.Fatalf("never observed a LoopStarted for plain loop %v", primLoopID)
	}
	if plainLS.ParentToolUseID != "" {
		t.Errorf("plain NewLoop LoopStarted.ParentToolUseID = %q, want empty", plainLS.ParentToolUseID)
	}
}

// TestRunSubagentPropagatesSessionClosing proves error propagation from the first
// building block: when the session is closing, NewLoop refuses, so RunSubagent must
// surface that *SessionError{SessionClosing} and never create a sub-loop or block.
func TestRunSubagentPropagatesSessionClosing(t *testing.T) {
	t.Parallel()

	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// White-box latch the closing gate (the same latch Shutdown sets) so NewLoop's
	// authoritative fail-secure check refuses.
	s.loopsMu.Lock()
	s.closing = true
	s.loopsMu.Unlock()

	subCfg := cfg(&stubLLM{chunks: []content.Chunk{textChunk("never runs")}})
	got, err := s.RunSubagent(context.Background(), loop.Provenance{LoopID: s.PrimaryLoopID()}, subCfg, textBlocks("go"), "")
	if got != "" {
		t.Errorf("RunSubagent text = %q, want empty on closing", got)
	}
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionClosing {
		t.Fatalf("RunSubagent err = %v, want *SessionError{SessionClosing}", err)
	}
}

// waitLoopStartedNonPrimaryEvent reads the observer until a LoopStarted for a loop
// id other than primary arrives, returning that whole event so the caller can inspect
// ParentToolUseID. The session emits a LoopStarted for every NewLoop; the sub-loop is
// the only non-primary one when this is called right after one RunSubagent.
func waitLoopStartedNonPrimaryEvent(t *testing.T, sub interface {
	Events() <-chan event.Event
}, primary [16]byte) (event.LoopStarted, bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return event.LoopStarted{}, false
			}
			if ls, ok := ev.(event.LoopStarted); ok && ls.Coordinates.LoopID != primary {
				return ls, true
			}
		case <-deadline:
			return event.LoopStarted{}, false
		}
	}
}

// waitLoopStartedOnLoop reads the observer until a LoopStarted whose Coordinates.LoopID
// equals loopID arrives, returning that whole event so the caller can inspect
// ParentToolUseID.
func waitLoopStartedOnLoop(t *testing.T, sub interface {
	Events() <-chan event.Event
}, loopID [16]byte) (event.LoopStarted, bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return event.LoopStarted{}, false
			}
			if ls, ok := ev.(event.LoopStarted); ok && ls.Coordinates.LoopID == loopID {
				return ls, true
			}
		case <-deadline:
			return event.LoopStarted{}, false
		}
	}
}

// waitTurnStartedOnLoop reads the observer until a TurnStarted whose Coordinates.LoopID
// equals loopID arrives, returning that event so the caller can inspect its Cause.
func waitTurnStartedOnLoop(t *testing.T, sub interface {
	Events() <-chan event.Event
}, loopID [16]byte) (event.TurnStarted, bool) {
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
