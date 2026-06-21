package journal

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
)

func TestEventRecordSubjectAndID(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x11)
	lid := fixedUUID(0x12)
	evID := fixedUUID(0x13)

	tests := []struct {
		name        string
		ev          event.Event
		wantSubject string
		wantID      string
	}{
		{
			name: "session-scoped event maps to session subject",
			ev: event.SessionStarted{
				Header: event.Header{
					Coordinates: identity.Coordinates{SessionID: sid},
					EventID:     evID,
				},
			},
			wantSubject: SessionEventSubject(sid),
			wantID:      evID.String(),
		},
		{
			name: "loop-scoped event maps to loop event subject",
			ev: event.LoopStarted{
				Header: event.Header{
					Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
					EventID:     evID,
				},
			},
			wantSubject: LoopEventSubject(sid, lid),
			wantID:      evID.String(),
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := NewEventRecord(tt.ev)
			if got := rec.Subject(); got != tt.wantSubject {
				t.Errorf("Subject() = %q, want %q", got, tt.wantSubject)
			}
			if got := rec.IdempotencyID(); got != tt.wantID {
				t.Errorf("IdempotencyID() = %q, want %q", got, tt.wantID)
			}
			// The wrapped event is recoverable for the serializer's MarshalEvent.
			if rec.Event() != tt.ev {
				t.Errorf("Event() did not return the wrapped event")
			}
			// Round-trip the derived subject back through the parser.
			kind, gotSID, _, err := ParseSubject(rec.Subject())
			if err != nil {
				t.Fatalf("ParseSubject(%q) err = %v", rec.Subject(), err)
			}
			if !kind.IsEvent() {
				t.Errorf("kind %v is not an event subject", kind)
			}
			if gotSID != sid {
				t.Errorf("parsed sid = %s, want %s", gotSID, sid)
			}
		})
	}
}

func TestCommandRecordSubjectAndID(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x21)
	lid := fixedUUID(0x22)
	cmdID := fixedUUID(0x23)

	cmd := command.Interrupt{Header: command.Header{CommandID: cmdID}}
	rec := NewCommandRecord(sid, lid, cmd)
	if got := rec.Subject(); got != LoopCommandSubject(sid, lid) {
		t.Errorf("Subject() = %q, want %q", got, LoopCommandSubject(sid, lid))
	}
	if got := rec.IdempotencyID(); got != cmdID.String() {
		t.Errorf("IdempotencyID() = %q, want %q", got, cmdID.String())
	}
	if rec.Command() != cmd {
		t.Errorf("Command() did not return the wrapped command")
	}
	if IsEventSubject(rec.Subject()) {
		t.Errorf("command subject %q classified as an event subject", rec.Subject())
	}
}

// TestJournalRecordSumIsSealed asserts the event and command records satisfy the
// sealed JournalRecord marker so the serializer in 4.3 can switch over the sum and
// read a uniform Subject()/IdempotencyID() off any record. (The fence record joins
// the sum in 4.2, covered by its own test.)
func TestJournalRecordSumIsSealed(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x41)
	lid := fixedUUID(0x42)
	recs := []JournalRecord{
		NewEventRecord(event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}}}),
		NewCommandRecord(sid, lid, command.Interrupt{}),
	}
	for i, r := range recs {
		if r == nil {
			t.Fatalf("record %d is nil", i)
		}
		if r.Subject() == "" {
			t.Errorf("record %d Subject() is empty", i)
		}
		if r.IdempotencyID() == "" {
			t.Errorf("record %d IdempotencyID() is empty", i)
		}
	}
}
