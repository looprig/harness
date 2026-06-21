package journal

import (
	"context"
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
)

// recordingJournal is a SessionJournal double that records each appended record and
// returns a chosen sequence/error. It lets the appender façade be unit-tested without
// a live JetStream server (the end-to-end durable path is covered by the integration
// suite).
type recordingJournal struct {
	records []JournalRecord
	seq     uint64
	err     error
}

func (j *recordingJournal) Append(_ context.Context, rec JournalRecord) (uint64, error) {
	j.records = append(j.records, rec)
	if j.err != nil {
		return 0, j.err
	}
	j.seq++
	return j.seq, nil
}

// TestJournalEventAppenderRoutes proves the façade wraps an event.Event in an
// EventRecord that self-routes to the right subject (session-scoped → session subject;
// loop-scoped → loop event subject) and carries the event's EventID as the idempotency
// id, then calls the underlying journal's Append.
func TestJournalEventAppenderRoutes(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x21)
	lid := fixedUUID(0x22)
	evID := fixedUUID(0x23)

	tests := []struct {
		name        string
		ev          event.Event
		wantSubject string
	}{
		{
			name:        "session-scoped event routes to the session subject",
			ev:          event.SessionActive{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: evID}},
			wantSubject: SessionEventSubject(sid),
		},
		{
			name:        "loop-scoped event routes to the loop event subject",
			ev:          event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}, EventID: evID}},
			wantSubject: LoopEventSubject(sid, lid),
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j := &recordingJournal{}
			app := NewJournalEventAppender(j)

			if err := app.AppendEvent(context.Background(), tt.ev); err != nil {
				t.Fatalf("AppendEvent = %v, want nil", err)
			}
			if len(j.records) != 1 {
				t.Fatalf("appended %d records, want 1", len(j.records))
			}
			rec := j.records[0]
			if rec.Subject() != tt.wantSubject {
				t.Errorf("record subject = %q, want %q", rec.Subject(), tt.wantSubject)
			}
			if rec.IdempotencyID() != evID.String() {
				t.Errorf("record idempotency id = %q, want %q", rec.IdempotencyID(), evID.String())
			}
			er, ok := rec.(EventRecord)
			if !ok {
				t.Fatalf("record type = %T, want EventRecord", rec)
			}
			if er.Event() != tt.ev {
				t.Errorf("wrapped event = %v, want the appended event", er.Event())
			}
		})
	}
}

// TestJournalEventAppenderPropagatesError proves an Append failure is surfaced
// unchanged (the hub maps it onto a SessionPersistenceFault) — never swallowed.
func TestJournalEventAppenderPropagatesError(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x31)
	wantErr := errors.New("stream rejected the write")
	j := &recordingJournal{err: wantErr}
	app := NewJournalEventAppender(j)

	err := app.AppendEvent(context.Background(), event.SessionStopped{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: fixedUUID(0x32)}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("AppendEvent error = %v, want %v", err, wantErr)
	}
}

// TestJournalEventAppenderNilJournal proves the constructor's nil guard: a nil
// SessionJournal is a programming error caught at construction (fail loud) rather than
// a nil-deref at the first append.
func TestJournalEventAppenderNilJournal(t *testing.T) {
	t.Parallel()
	app, err := NewJournalEventAppenderChecked(nil)
	if err == nil {
		t.Fatalf("NewJournalEventAppenderChecked(nil) err = nil, want error")
	}
	if app != nil {
		t.Errorf("NewJournalEventAppenderChecked(nil) appender = %v, want nil", app)
	}
	var nje *NilJournalError
	if !errors.As(err, &nje) {
		t.Fatalf("error %v is not *NilJournalError", err)
	}
}
