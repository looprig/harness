//go:build integration

package session

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/inventivepotter/urvi/tui"
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
