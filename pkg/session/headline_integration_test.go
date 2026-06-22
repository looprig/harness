//go:build integration

package session

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/journal"
	"github.com/ciram-co/looprig/pkg/tui"
	"github.com/ciram-co/looprig/pkg/uuid"
	"github.com/nats-io/nats.go"
)

// allPrimaryEvents replays the FULL ordered Enduring stream scoped to the primary loop
// (the session-event subject + that loop's event subject, in stream-sequence order)
// into a materialized slice — exactly the read the cold-restore TUI repaint folds
// (Coding.ReplayBacklog → EventReplayer{From:Beginning, Follow:false}). Unlike
// restoreEventTail it keeps EVERY event, because the displayed projection is the fold
// over the whole backlog, not just the restore-lifecycle tail. It is the durable "what
// restore sees" sequence the headline property folds through the TUI reducers.
func allPrimaryEvents(t *testing.T, js nats.JetStreamContext, sessionID, primaryLoopID uuid.UUID) []event.Event {
	t.Helper()
	r := journal.NewEventReplayer(js, mustObjectStore(t, js, sessionID))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cursor, err := r.Open(ctx, journal.ReplayRequest{SessionID: sessionID, LoopID: primaryLoopID, From: journal.Beginning(), Follow: false})
	if err != nil {
		t.Fatalf("replay Open: %v", err)
	}
	defer func() { _ = cursor.Close() }()

	var out []event.Event
	for {
		ev, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("replay Next: %v", err)
		}
		out = append(out, ev)
	}
}

// TestHeadlineQuiescentDisplayedStoredRestored is Task 11.1 — the headline property for
// a CLEANLY-ENDED (quiescent) history: displayed == stored == restored, exactly.
//
// An original run persists a non-trivial clean history (a multi-message StepDone group —
// an AI tool-use reply followed by two tool results — plus a mid-turn TurnFoldedInto and
// a final step, all inside one cleanly-closed turn, via the buildComplexShapesRun
// direct-publish builder; the prompt's "ASK if" clause prefers direct-publish over
// scripting a real tool step through the loop). The lease hands over, the session is
// restored on the SAME stream, and the full chain is asserted:
//
//   - stored == restored: the restored primary loop's msgs (via the actor-served
//     Snapshot accessor restoredSnapshot) deep-equals the original committed msgs.
//   - displayed == restored: folding the restored ReplayBacklog through the TUI reducers
//     (tui.FoldDisplay → transcript.ApplyEvent + interaction.ApplyEvent) yields a
//     transcript byte-for-byte equal to the one from folding the ORIGINAL run's durable
//     Enduring sequence through the SAME reducers. The repainted TUI == the original TUI.
func TestHeadlineQuiescentDisplayedStoredRestored(t *testing.T) {
	js := newEmbeddedJS(t)
	fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

	orig := buildComplexShapesRun(t, js, fp)

	// The original run's durable Enduring sequence — what the live TUI displayed and
	// what the cold restore will re-read. Captured BEFORE handover/restore so it holds
	// no Restore* bracketing.
	primaryID := orig.primaryLoopID
	origEvents := allPrimaryEvents(t, js, orig.sessionID, primaryID)
	origDisplayed := tui.FoldDisplay(origEvents, primaryID)

	handOver(t, orig.lease)

	objStore := mustObjectStore(t, js, orig.sessionID)
	s, err := Restore(context.Background(), restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("after restore")}}, "model-x", "be helpful"),
		orig.sessionID, js, objStore, mustLeaseManager(t, js))
	if err != nil {
		t.Fatalf("Restore (quiescent): %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// stored == restored: the restored loop's committed msgs deep-equal the original's.
	msgs, idx := restoredSnapshot(t, s)
	if !reflect.DeepEqual(msgs, orig.committedMsgs) {
		t.Errorf("stored != restored: restored msgs =\n  %#v\nwant\n  %#v", msgs, orig.committedMsgs)
	}
	if idx != orig.committedTurn {
		t.Errorf("restored turnIndex = %d, want %d", idx, orig.committedTurn)
	}

	// displayed == restored: the restored backlog folds to the SAME displayed transcript
	// as the original live Enduring sequence. The restored backlog additionally carries
	// the RestoreStarted/RestoreDone lifecycle bracket, which the transcript reducers
	// treat as no-ops — so a clean restore repaints the IDENTICAL transcript.
	restoredEvents := allPrimaryEvents(t, js, orig.sessionID, primaryID)
	restoredDisplayed := tui.FoldDisplay(restoredEvents, primaryID)

	if !restoredDisplayed.EqualTranscript(origDisplayed) {
		t.Errorf("displayed != restored: repainted transcript (%d committed) does not deep-equal the original live transcript (%d committed)",
			restoredDisplayed.CommittedLen(), origDisplayed.CommittedLen())
	}
	// A quiescent restore comes up idle with NO pending gates on either side.
	if restoredDisplayed.PendingPrompts() != 0 || origDisplayed.PendingPrompts() != 0 {
		t.Errorf("quiescent displayed has pending prompts: restored=%d original=%d (want 0/0)",
			restoredDisplayed.PendingPrompts(), origDisplayed.PendingPrompts())
	}
	// Sanity: the clean history committed a non-trivial transcript (user + tool group +
	// folded user + final assistant), not an accidentally-empty fold.
	if restoredDisplayed.CommittedLen() == 0 {
		t.Fatal("displayed transcript is empty; the clean history should have repainted committed rows")
	}
}

// TestHeadlineCrashRestoredEqualsDurableProjection is Task 11.2 — the headline property
// for a MID-STEP KILL: the restored/repainted transcript equals the DURABLE Enduring
// projection (committed user + completed steps + the crash-seam interruption marker, NO
// partial assistant step, NO ephemeral content) and is explicitly NOT EQUAL to the
// pre-crash LIVE transcript (which held ephemeral TokenDeltas + a partial in-flight
// step the user saw).
//
// The crash history is built by direct-publish (buildCrashedRun): a complete turn, then
// an OPEN turn with a committed TurnStarted + one completed StepDone and NO terminal —
// the exact "crash after committing a step, before the next step finished" stored shape.
func TestHeadlineCrashRestoredEqualsDurableProjection(t *testing.T) {
	js := newEmbeddedJS(t)
	fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

	orig := buildCrashedRun(t, js, fp)
	primaryID := orig.primaryLoopID

	// The PRE-CRASH LIVE transcript: the durable committed sequence the user saw up to
	// the crash PLUS the ephemeral in-flight step they were watching stream — TokenDeltas
	// of a partial assistant step that NEVER committed a StepDone and NEVER saw a terminal
	// (the process died). The live path folds Ephemeral; restore does not. This is the
	// view the design says we must NOT compare the restored transcript to.
	durableBeforeCrash := allPrimaryEvents(t, js, orig.sessionID, primaryID)
	preCrashLive := tui.FoldDisplay(appendEphemeralPartialStep(durableBeforeCrash, primaryID), primaryID)

	handOver(t, orig.lease)

	objStore := mustObjectStore(t, js, orig.sessionID)
	s, err := Restore(context.Background(), restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("recovered")}}, "model-x", "be helpful"),
		orig.sessionID, js, objStore, mustLeaseManager(t, js))
	if err != nil {
		t.Fatalf("Restore (crash mid-turn): %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Restored msgs end at the LAST COMMITTED StepDone — no partial assistant step. This
	// is the stored==restored leg for the crash seam (the partial in-flight step that the
	// pre-crash live view streamed is absent).
	msgs, idx := restoredSnapshot(t, s)
	if !reflect.DeepEqual(msgs, orig.committedMsgs) {
		t.Errorf("restored (crash) msgs end past the last committed StepDone:\n  got  %#v\n  want %#v", msgs, orig.committedMsgs)
	}
	if idx != orig.committedTurn {
		t.Errorf("restored (crash) turnIndex = %d, want %d", idx, orig.committedTurn)
	}

	// The restored backlog folds to the DURABLE Enduring projection: the committed
	// user/step rows plus the crash-seam TurnInterrupted tombstone Restore appended — and
	// NO partial step, NO ephemeral. Build that durable projection independently from the
	// committed durable sequence plus the interruption marker Restore writes.
	restoredEvents := allPrimaryEvents(t, js, orig.sessionID, primaryID)
	restoredDisplayed := tui.FoldDisplay(restoredEvents, primaryID)

	durableProjection := tui.FoldDisplay(durableEnduringWithInterrupt(durableBeforeCrash, primaryID), primaryID)

	// restored == durable-projection (exact).
	if !restoredDisplayed.EqualTranscript(durableProjection) {
		t.Errorf("restored != durable-projection: repainted transcript (%d committed) does not deep-equal the durable Enduring projection (%d committed)",
			restoredDisplayed.CommittedLen(), durableProjection.CommittedLen())
	}

	// restored != pre-crash-live (deliberate inequality): the pre-crash live view held a
	// partial step + ephemeral token deltas and NO interruption marker; the restored view
	// has the interruption marker and NO partial/ephemeral. They MUST differ.
	if restoredDisplayed.EqualTranscript(preCrashLive) {
		t.Error("restored == pre-crash-live: the repaint wrongly reproduced the ephemeral/partial live view (it must equal the durable projection, not the live screen)")
	}

	// The restored session CONTINUES: a fresh Submit runs and numbers from the restored
	// turnIndex.
	submitAndDrain(t, s, []content.Block{&content.TextBlock{Text: "carry on"}})
	if _, idx2 := restoredSnapshot(t, s); idx2 != orig.committedTurn+1 {
		t.Errorf("post-crash-restore turnIndex = %d, want %d (the restored session must continue from the restored index)", idx2, orig.committedTurn+1)
	}
}

// appendEphemeralPartialStep returns durable followed by the EPHEMERAL events the user
// saw streaming for the in-flight (uncommitted) step before the crash: TokenDeltas
// accumulating a partial assistant narration on the open turn's primary loop, with NO
// StepDone (the step never committed) and NO terminal (the process died). Folding this
// through the live reducers leaves that partial prose in the live segment — the
// "pre-crash live screen" the restored view must NOT reproduce.
func appendEphemeralPartialStep(durable []event.Event, primaryLoopID uuid.UUID) []event.Event {
	hdr := event.Header{Coordinates: identity.Coordinates{LoopID: primaryLoopID}}
	out := append([]event.Event(nil), durable...)
	out = append(out,
		event.TokenDelta{Header: hdr, Chunk: &content.TextChunk{Text: "partial "}},
		event.TokenDelta{Header: hdr, Chunk: &content.TextChunk{Text: "in-flight "}},
		event.TokenDelta{Header: hdr, Chunk: &content.TextChunk{Text: "answer the user saw"}},
	)
	return out
}

// durableEnduringWithInterrupt returns durable (the committed Enduring sequence up to
// the crash) followed by the crash-seam TurnInterrupted marker Restore appends to close
// the open turn. Folding this through the reducers is the DURABLE projection: committed
// rows + the interruption tombstone, no partial step, no ephemeral — independent of the
// restored stream so the equality is a real property, not a self-comparison.
func durableEnduringWithInterrupt(durable []event.Event, primaryLoopID uuid.UUID) []event.Event {
	hdr := event.Header{Coordinates: identity.Coordinates{LoopID: primaryLoopID}}
	out := append([]event.Event(nil), durable...)
	out = append(out, event.TurnInterrupted{Header: hdr})
	return out
}
