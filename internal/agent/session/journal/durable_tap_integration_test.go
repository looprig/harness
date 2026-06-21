//go:build integration

package journal_test

import (
	"context"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/agent/session/hub"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestHubDurableTapReplayable is the end-to-end Phase-7 assertion: publishing Enduring
// events through a hub whose appender wraps a REAL SessionJournal makes them
// replayable via the EventReplayer — the durable tap actually persists them. It also
// proves the hub-SYNTHESIZED SessionActive/SessionIdle (derived from the quiescence
// transition, minted by the hub's Factory) land durably, and that an Ephemeral event
// is NEVER persisted.
func TestHubDurableTapReplayable(t *testing.T) {
	sid := seedUUID(0xD0)
	lid := seedUUID(0xD1)
	tid := seedUUID(0xD2)

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid, mustAcquireLease(t, js, sid))
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}

	// Real durable appender + a deterministic Factory so the synthesized session
	// events are stamped with non-zero ids the journal can de-dup on.
	appender := journal.NewJournalEventAppender(j)
	h := hub.New(sid, hub.WithAppender(appender), hub.WithFactory(testFactory()))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Idle -> Active: TurnStarted appended, then a derived SessionActive minted +
	// appended. A TokenDelta (Ephemeral) is published but must NOT persist. Active ->
	// Idle: LoopIdle appended, then a derived SessionIdle.
	turnStarted := event.TurnStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid},
		EventID:     seedUUID(0xE0),
	}}
	tokenDelta := event.TokenDelta{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
		EventID:     seedUUID(0xE1),
	}}
	loopIdle := event.LoopIdle{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
		EventID:     seedUUID(0xE2),
	}}

	if err := h.PublishEvent(ctx, turnStarted); err != nil {
		t.Fatalf("PublishEvent(TurnStarted): %v", err)
	}
	if err := h.PublishEvent(ctx, tokenDelta); err != nil {
		t.Fatalf("PublishEvent(TokenDelta): %v", err)
	}
	if err := h.PublishEvent(ctx, loopIdle); err != nil {
		t.Fatalf("PublishEvent(LoopIdle): %v", err)
	}

	// A replay scoped to loop lid yields the session events (SessionActive/SessionIdle)
	// AND loop lid's events (TurnStarted/LoopIdle), in stream-sequence (= append, =
	// causal) order. The Ephemeral TokenDelta was never appended, so it is absent.
	r := journal.NewEventReplayer(js, mustObjectStore(t, js, sid))
	all, _ := drainAll(t, r, journal.ReplayRequest{SessionID: sid, LoopID: lid, From: journal.Beginning(), Follow: false})

	// Causal order: trigger then derived companion, for each edge.
	assertEventTypes(t, "durable tap", all, []event.Event{
		event.TurnStarted{},   // Idle->Active trigger
		event.SessionActive{}, // derived companion (minted + appended after the trigger)
		event.LoopIdle{},      // Active->Idle trigger
		event.SessionIdle{},   // derived companion
	})

	// The derived session events were minted (non-zero EventID + CreatedAt).
	for _, ev := range all {
		switch ev.(type) {
		case event.SessionActive, event.SessionIdle:
			if ev.EventHeader().EventID.IsZero() {
				t.Errorf("replayed derived %T has a zero EventID (not minted)", ev)
			}
			if ev.EventHeader().CreatedAt.IsZero() {
				t.Errorf("replayed derived %T has a zero CreatedAt (not minted)", ev)
			}
		case event.TokenDelta:
			t.Errorf("Ephemeral TokenDelta was durably persisted by the tap")
		}
	}
}

// assertEventTypes fails the test unless got's concrete types match want's, in order.
func assertEventTypes(t *testing.T, label string, got, want []event.Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: replayed %d events %T, want %d", label, len(got), got, len(want))
	}
	for i := range want {
		if eventType(got[i]) != eventType(want[i]) {
			t.Errorf("%s[%d] = %T, want %T", label, i, got[i], want[i])
		}
	}
}

func eventType(ev event.Event) string {
	switch ev.(type) {
	case event.TurnStarted:
		return "TurnStarted"
	case event.LoopIdle:
		return "LoopIdle"
	case event.SessionActive:
		return "SessionActive"
	case event.SessionIdle:
		return "SessionIdle"
	case event.SessionStopped:
		return "SessionStopped"
	case event.TokenDelta:
		return "TokenDelta"
	default:
		return "other"
	}
}

// testFactory mints deterministic, monotonically increasing EventIDs and a fixed
// CreatedAt so the hub's synthesized session events get stable, non-zero ids/times.
func testFactory() *event.Factory {
	var n byte = 0x90
	ts := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	return event.NewFactory(func() (uuid.UUID, error) {
		n++
		return uuid.UUID{n}, nil
	}, func() time.Time { return ts })
}
