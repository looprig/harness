package journal

import (
	"testing"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/uuid"
)

// fixedUUID builds a deterministic non-zero uuid from a single seed byte so the table
// tests round-trip stable, readable ids.
func fixedUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

// TestEventRecordIDAndPayload proves an EventRecord carries the event's EventID as its
// idempotency id and returns the wrapped event unchanged for the serializer to marshal.
func TestEventRecordIDAndPayload(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x11)
	lid := fixedUUID(0x12)
	evID := fixedUUID(0x13)

	tests := []struct {
		name   string
		ev     event.Event
		wantID string
	}{
		{
			name:   "session-scoped event",
			ev:     event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: evID}},
			wantID: evID.String(),
		},
		{
			name:   "loop-scoped event",
			ev:     event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}, EventID: evID}},
			wantID: evID.String(),
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := NewEventRecord(tt.ev)
			if got := rec.IdempotencyID(); got != tt.wantID {
				t.Errorf("IdempotencyID() = %q, want %q", got, tt.wantID)
			}
			// The wrapped event is recoverable for the serializer's MarshalEvent.
			if rec.Event() != tt.ev {
				t.Errorf("Event() did not return the wrapped event")
			}
			var _ JournalRecord = rec // EventRecord satisfies the sealed sum.
		})
	}
}

// TestCommandRecordIDAndTarget proves a CommandRecord carries the command's CommandID as
// its idempotency id, exposes the writer-supplied dispatch target (session + loop), and
// returns the wrapped command unchanged.
func TestCommandRecordIDAndTarget(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x21)
	lid := fixedUUID(0x22)
	cmdID := fixedUUID(0x23)

	cmd := command.Interrupt{Header: command.Header{CommandID: cmdID}}
	rec := NewCommandRecord(sid, lid, cmd)
	if got := rec.IdempotencyID(); got != cmdID.String() {
		t.Errorf("IdempotencyID() = %q, want %q", got, cmdID.String())
	}
	if rec.SessionID() != sid {
		t.Errorf("SessionID() = %v, want %v", rec.SessionID(), sid)
	}
	if rec.LoopID() != lid {
		t.Errorf("LoopID() = %v, want %v", rec.LoopID(), lid)
	}
	if rec.Command() != cmd {
		t.Errorf("Command() did not return the wrapped command")
	}
	var _ JournalRecord = rec // CommandRecord satisfies the sealed sum.
}

// TestJournalRecordSumIsSealed asserts each record variant satisfies the sealed
// JournalRecord marker (so a serializer's switch over the sum stays exhaustive) and
// exposes a non-empty idempotency id.
func TestJournalRecordSumIsSealed(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x41)
	lid := fixedUUID(0x42)
	recs := []JournalRecord{
		NewEventRecord(event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}}}),
		NewCommandRecord(sid, lid, command.Interrupt{}),
		NewFenceRecord(sid, LeaseFence{Epoch: 7}),
	}
	for i, r := range recs {
		if r == nil {
			t.Fatalf("record %d is nil", i)
		}
		if r.IdempotencyID() == "" {
			t.Errorf("record %d IdempotencyID() is empty", i)
		}
	}
}
