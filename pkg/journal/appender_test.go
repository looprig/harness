package journal

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
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
// EventRecord that carries the event's EventID as the idempotency id, then calls the
// underlying journal's Append.
func TestJournalEventAppenderRoutes(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x21)
	lid := fixedUUID(0x22)
	evID := fixedUUID(0x23)

	tests := []struct {
		name string
		ev   event.Event
	}{
		{
			name: "session-scoped event",
			ev:   event.SessionActive{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: evID}},
		},
		{
			name: "loop-scoped event",
			ev:   event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}, EventID: evID}},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j := &recordingJournal{}
			app := NewJournalEventAppender(j)

			if _, err := app.AppendEvent(context.Background(), tt.ev); err != nil {
				t.Fatalf("AppendEvent = %v, want nil", err)
			}
			if len(j.records) != 1 {
				t.Fatalf("appended %d records, want 1", len(j.records))
			}
			rec := j.records[0]
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

// TestJournalEventAppenderReturnsSeq proves AppendEvent returns the durable sequence the
// journal assigned (so the hub can ride it on the live delivery), and notifies the catalog
// with that same (event, seq) pair. recordingJournal returns startSeq+1 on the first
// append, so a case primes startSeq to pin the returned sequence.
func TestJournalEventAppenderReturnsSeq(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x91)
	ev := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: fixedUUID(0x92)}}

	tests := []struct {
		name     string
		startSeq uint64
		wantSeq  uint64
	}{
		{name: "first append (boundary seq 1)", startSeq: 0, wantSeq: 1},
		{name: "mid-log append seq 7", startSeq: 6, wantSeq: 7},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j := &recordingJournal{seq: tt.startSeq}
			cat := &recordingCatalog{}
			app := NewJournalEventAppender(j, WithCatalog(cat))

			seq, err := app.AppendEvent(context.Background(), ev)
			if err != nil {
				t.Fatalf("AppendEvent = %v, want nil", err)
			}
			if seq != tt.wantSeq {
				t.Errorf("AppendEvent seq = %d, want %d", seq, tt.wantSeq)
			}
			if len(cat.events) != 1 || len(cat.seqs) != 1 {
				t.Fatalf("catalog saw %d events / %d seqs, want 1 / 1", len(cat.events), len(cat.seqs))
			}
			if cat.events[0] != ev {
				t.Errorf("catalog event = %v, want the appended event", cat.events[0])
			}
			if cat.seqs[0] != tt.wantSeq {
				t.Errorf("catalog seq = %d, want %d", cat.seqs[0], tt.wantSeq)
			}
		})
	}
}

func TestJournalGateAppenderRoutes(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x81)
	lid := fixedUUID(0x82)
	turnID := fixedUUID(0x83)
	stepID := fixedUUID(0x84)
	gateID := gate.ID(fixedUUID(0x85))
	evID := fixedUUID(0x86)
	coords := identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: turnID, StepID: stepID}
	g := gate.Gate{
		ID:       gateID,
		Kind:     gate.KindPermission,
		Resolver: gate.ResolverLoop,
		Subject:  gate.Subject{TurnID: gate.ID(turnID), StepID: gate.ID(stepID)},
	}
	prepared := event.GatePrepared{Header: event.Header{Coordinates: coords, EventID: evID}, Gate: g}
	openPayload := gate.OpenPayload{GateID: gateID, Payload: gate.PermissionPayload{}}
	preparedRecord := NewGatePreparedRecord(prepared, openPayload)
	opened := event.GateOpened{Header: event.Header{Coordinates: coords, EventID: fixedUUID(0x87)}, Gate: g}
	resolved := event.GateResolved{Header: event.Header{Coordinates: coords, EventID: fixedUUID(0x88)}, GateID: gateID}

	j := &recordingJournal{}
	app := NewJournalGateAppender(j)

	if err := app.AppendGatePrepared(context.Background(), preparedRecord); err != nil {
		t.Fatalf("AppendGatePrepared = %v, want nil", err)
	}
	if err := app.AppendGateOpened(context.Background(), opened); err != nil {
		t.Fatalf("AppendGateOpened = %v, want nil", err)
	}
	if err := app.AppendGateResolved(context.Background(), resolved); err != nil {
		t.Fatalf("AppendGateResolved = %v, want nil", err)
	}

	if len(j.records) != 3 {
		t.Fatalf("appended %d records, want 3", len(j.records))
	}
	if got, ok := j.records[0].(GatePreparedRecord); !ok || got.IdempotencyID() != preparedRecord.IdempotencyID() {
		t.Fatalf("record[0] = %T/%q, want GatePreparedRecord/%q", j.records[0], j.records[0].IdempotencyID(), preparedRecord.IdempotencyID())
	}
	openedRecord, ok := j.records[1].(EventRecord)
	if !ok {
		t.Fatalf("record[1] = %T, want EventRecord", j.records[1])
	}
	if !reflect.DeepEqual(openedRecord.Event(), opened) {
		t.Errorf("record[1] event = %#v, want %#v", openedRecord.Event(), opened)
	}
	resolvedRecord, ok := j.records[2].(EventRecord)
	if !ok {
		t.Fatalf("record[2] = %T, want EventRecord", j.records[2])
	}
	if !reflect.DeepEqual(resolvedRecord.Event(), resolved) {
		t.Errorf("record[2] event = %#v, want %#v", resolvedRecord.Event(), resolved)
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

	seq, err := app.AppendEvent(context.Background(), event.SessionStopped{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: fixedUUID(0x32)}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("AppendEvent error = %v, want %v", err, wantErr)
	}
	if seq != 0 {
		t.Errorf("AppendEvent seq on error = %d, want 0", seq)
	}
}

// recordingCatalog records each (event, seq) pair the appender hands it after a
// successful append. It satisfies the catalogUpdater seam and, per that contract, never
// returns an error.
type recordingCatalog struct {
	events []event.Event
	seqs   []uint64
}

func (c *recordingCatalog) UpdateOnEvent(_ context.Context, ev event.Event, seq uint64) error {
	c.events = append(c.events, ev)
	c.seqs = append(c.seqs, seq)
	return nil
}

// TestJournalEventAppenderCatalogHook proves the appender notifies the injected catalog
// AFTER a successful append, with the same event — and that the nop default (no catalog
// injected) leaves the append path unchanged.
func TestJournalEventAppenderCatalogHook(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x61)
	ev := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: fixedUUID(0x62)}}

	t.Run("catalog notified post-success", func(t *testing.T) {
		t.Parallel()
		j := &recordingJournal{}
		cat := &recordingCatalog{}
		app := NewJournalEventAppender(j, WithCatalog(cat))
		if _, err := app.AppendEvent(context.Background(), ev); err != nil {
			t.Fatalf("AppendEvent = %v, want nil", err)
		}
		if len(j.records) != 1 {
			t.Fatalf("appended %d records, want 1", len(j.records))
		}
		if len(cat.events) != 1 {
			t.Fatalf("catalog saw %d events, want 1", len(cat.events))
		}
		if cat.events[0] != ev {
			t.Errorf("catalog event = %v, want the appended event", cat.events[0])
		}
	})

	t.Run("nil catalog ignored (nop default keeps behavior)", func(t *testing.T) {
		t.Parallel()
		j := &recordingJournal{}
		app := NewJournalEventAppender(j, WithCatalog(nil))
		if _, err := app.AppendEvent(context.Background(), ev); err != nil {
			t.Fatalf("AppendEvent = %v, want nil", err)
		}
		if len(j.records) != 1 {
			t.Errorf("appended %d records, want 1", len(j.records))
		}
	})
}

// TestJournalEventAppenderCatalogSkippedOnFailure proves a failed durable append does
// NOT touch the catalog (the event did not land, so it must not be indexed).
func TestJournalEventAppenderCatalogSkippedOnFailure(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x71)
	wantErr := errors.New("stream rejected the write")
	j := &recordingJournal{err: wantErr}
	cat := &recordingCatalog{}
	app := NewJournalEventAppender(j, WithCatalog(cat))

	ev := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: fixedUUID(0x72)}}
	seq, err := app.AppendEvent(context.Background(), ev)
	if !errors.Is(err, wantErr) {
		t.Fatalf("AppendEvent error = %v, want %v", err, wantErr)
	}
	if seq != 0 {
		t.Errorf("AppendEvent seq on error = %d, want 0", seq)
	}
	if len(cat.events) != 0 {
		t.Errorf("catalog saw %d events on append failure, want 0", len(cat.events))
	}
}

// TestJournalCommandAppenderRoutes proves the command façade wraps a command in a
// CommandRecord targeting the given session+loop (exposed via SessionID/LoopID) and
// carries the command's CommandID as the idempotency id, then calls the underlying Append.
func TestJournalCommandAppenderRoutes(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x41)
	lid := fixedUUID(0x42)
	cmdID := fixedUUID(0x43)

	tests := []struct {
		name string
		cmd  command.Command
	}{
		{name: "UserInput routes to the loop cmd subject", cmd: command.UserInput{Header: command.Header{CommandID: cmdID}}},
		{name: "Interrupt routes to the loop cmd subject", cmd: command.Interrupt{Header: command.Header{CommandID: cmdID}}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j := &recordingJournal{}
			app := NewJournalCommandAppender(j)

			rec := NewCommandRecord(sid, lid, tt.cmd)
			if err := app.AppendCommand(context.Background(), rec); err != nil {
				t.Fatalf("AppendCommand = %v, want nil", err)
			}
			if len(j.records) != 1 {
				t.Fatalf("appended %d records, want 1", len(j.records))
			}
			got := j.records[0]
			if got.IdempotencyID() != cmdID.String() {
				t.Errorf("record idempotency id = %q, want %q", got.IdempotencyID(), cmdID.String())
			}
			cr, ok := got.(CommandRecord)
			if !ok {
				t.Fatalf("record type = %T, want CommandRecord", got)
			}
			if cr.SessionID() != sid || cr.LoopID() != lid {
				t.Errorf("record target = (%v, %v), want (%v, %v)", cr.SessionID(), cr.LoopID(), sid, lid)
			}
			if cr.Command().CommandHeader().CommandID != cmdID {
				t.Errorf("wrapped command id = %v, want %v", cr.Command().CommandHeader().CommandID, cmdID)
			}
		})
	}
}

// TestJournalCommandAppenderPropagatesError proves an Append failure is surfaced
// unchanged to the caller (the session logs+proceeds; the façade itself never swallows).
func TestJournalCommandAppenderPropagatesError(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x51)
	lid := fixedUUID(0x52)
	wantErr := errors.New("stream rejected the command")
	j := &recordingJournal{err: wantErr}
	app := NewJournalCommandAppender(j)

	rec := NewCommandRecord(sid, lid, command.UserInput{Header: command.Header{CommandID: fixedUUID(0x53)}})
	err := app.AppendCommand(context.Background(), rec)
	if !errors.Is(err, wantErr) {
		t.Fatalf("AppendCommand error = %v, want %v", err, wantErr)
	}
}

// TestJournalCommandAppenderNilJournal proves the checked constructor's nil guard
// mirrors the event appender's: a nil SessionJournal fails loud at the composition
// root rather than nil-deref at the first append.
func TestJournalCommandAppenderNilJournal(t *testing.T) {
	t.Parallel()
	app, err := NewJournalCommandAppenderChecked(nil)
	if err == nil {
		t.Fatalf("NewJournalCommandAppenderChecked(nil) err = nil, want error")
	}
	if app != nil {
		t.Errorf("NewJournalCommandAppenderChecked(nil) appender = %v, want nil", app)
	}
	var nje *NilJournalError
	if !errors.As(err, &nje) {
		t.Fatalf("error %v is not *NilJournalError", err)
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
