package journal

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strconv"
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
			if !reflect.DeepEqual(er.Event(), tt.ev) {
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
			if !reflect.DeepEqual(cat.events[0], ev) {
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
		if !reflect.DeepEqual(cat.events[0], ev) {
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

// idempotentRecordingJournal is a SessionJournal double that ALSO implements
// IdempotentJournal: it deduplicates by IdempotencyID exactly like the real backend
// (recording only the FIRST record seen for a given id and reporting Appended=false
// with the original sequence on a repeat), so AppendEvent's catalog-skip behavior can
// be unit-tested without a live backend.
type idempotentRecordingJournal struct {
	records []JournalRecord
	seq     uint64
	seen    map[string]uint64
}

func newIdempotentRecordingJournal() *idempotentRecordingJournal {
	return &idempotentRecordingJournal{seen: make(map[string]uint64)}
}

func (j *idempotentRecordingJournal) Append(ctx context.Context, rec JournalRecord) (uint64, error) {
	result, err := j.AppendIdempotent(ctx, rec)
	return result.Sequence, err
}

func (j *idempotentRecordingJournal) AppendIdempotent(_ context.Context, rec JournalRecord) (AppendResult, error) {
	id := rec.IdempotencyID()
	if seq, ok := j.seen[id]; ok {
		return AppendResult{Sequence: seq, Appended: false}, nil
	}
	j.records = append(j.records, rec)
	j.seq++
	j.seen[id] = j.seq
	return AppendResult{Sequence: j.seq, Appended: true}, nil
}

var _ IdempotentJournal = (*idempotentRecordingJournal)(nil)

// TestJournalAppenderDoesNotRepublishDuplicate proves AppendEvent, layered over a
// journal that implements the optional IdempotentJournal seam, does not write a
// second durable frame NOR re-notify the catalog for a redelivered event: the second
// call returns the ORIGINAL sequence, the underlying journal still holds exactly one
// record, and the catalog was notified exactly once.
func TestJournalAppenderDoesNotRepublishDuplicate(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0xA1)
	ev := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: fixedUUID(0xA2)}}

	j := newIdempotentRecordingJournal()
	cat := &recordingCatalog{}
	app := NewJournalEventAppender(j, WithCatalog(cat))

	seq1, err := app.AppendEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("first AppendEvent() err = %v", err)
	}
	seq2, err := app.AppendEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("second (duplicate) AppendEvent() err = %v", err)
	}
	if seq1 != seq2 {
		t.Errorf("duplicate AppendEvent returned seq %d, want the original seq %d", seq2, seq1)
	}
	if len(j.records) != 1 {
		t.Fatalf("underlying journal recorded %d records, want 1 (duplicate must not append a second frame)", len(j.records))
	}
	if len(cat.events) != 1 {
		t.Errorf("catalog notified %d times, want 1 (duplicate must not republish)", len(cat.events))
	}
}

func TestJournalEventAppenderAppendEventResultPreservesDedupOutcome(t *testing.T) {
	t.Parallel()

	sid := fixedUUID(0xB1)
	ev := event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid},
		EventID:     fixedUUID(0xB2),
	}}
	app := NewJournalEventAppender(newIdempotentRecordingJournal())

	seq, appended, err := app.AppendEventResult(context.Background(), ev)
	if err != nil {
		t.Fatalf("first AppendEventResult() error = %v", err)
	}
	if seq != 1 || !appended {
		t.Fatalf("first AppendEventResult() = seq:%d appended:%t, want 1/true", seq, appended)
	}
	duplicateSeq, duplicateAppended, err := app.AppendEventResult(context.Background(), ev)
	if err != nil {
		t.Fatalf("duplicate AppendEventResult() error = %v", err)
	}
	if duplicateSeq != seq || duplicateAppended {
		t.Fatalf("duplicate AppendEventResult() = seq:%d appended:%t, want %d/false", duplicateSeq, duplicateAppended, seq)
	}

	legacySeq, err := app.AppendEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("legacy AppendEvent() error = %v", err)
	}
	if legacySeq != seq {
		t.Fatalf("legacy AppendEvent() seq = %d, want original %d", legacySeq, seq)
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

// committedRecordingJournal is a SessionJournal double that implements the optional
// CommittedPublicJournal seam: it stores a per-record "canonical public body" for
// every PUBLIC event record and reports the exact stored bytes back to the appender.
// A private record (or a non-event record) stores no public body, exactly like the
// released sessionstore backend. It also deduplicates by idempotency id so the
// committed seam's dedup arm can be driven.
type committedRecordingJournal struct {
	records []JournalRecord
	stored  map[uint64][]byte // sequence -> exact stored canonical public body
	seen    map[string]uint64
	seq     uint64
}

func newCommittedRecordingJournal() *committedRecordingJournal {
	return &committedRecordingJournal{stored: make(map[uint64][]byte), seen: make(map[string]uint64)}
}

func (j *committedRecordingJournal) Append(ctx context.Context, rec JournalRecord) (uint64, error) {
	result, err := j.AppendCommitted(ctx, rec)
	return result.Sequence, err
}

func (j *committedRecordingJournal) AppendIdempotent(ctx context.Context, rec JournalRecord) (AppendResult, error) {
	result, err := j.AppendCommitted(ctx, rec)
	return result.AppendResult, err
}

func (j *committedRecordingJournal) AppendCommitted(_ context.Context, rec JournalRecord) (CommittedAppendResult, error) {
	id := rec.IdempotencyID()
	if seq, ok := j.seen[id]; ok {
		return CommittedAppendResult{AppendResult: AppendResult{Sequence: seq, Appended: false}}, nil
	}
	j.records = append(j.records, rec)
	j.seq++
	j.seen[id] = j.seq
	result := CommittedAppendResult{AppendResult: AppendResult{Sequence: j.seq, Appended: true}}
	eventRecord, isEvent := rec.(EventRecord)
	if !isEvent || eventRecord.Event().Visibility() != event.Public {
		return result, nil
	}
	body := []byte(`{"canonical":"` + id + `","seq":` + strconv.FormatUint(j.seq, 10) + `}`)
	j.stored[j.seq] = body
	result.Public = CommittedPublicBody{EventID: id, Body: body}
	return result, nil
}

var _ CommittedPublicJournal = (*committedRecordingJournal)(nil)

// TestJournalEventAppenderCommittedCarriesStoredPublicBody proves the committed seam
// hands back the EXACT bytes the journal stored — not a re-derived projection — along
// with the winning sequence, the appended state, and the committed public EventID.
func TestJournalEventAppenderCommittedCarriesStoredPublicBody(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0xC1)
	ev := event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid},
		EventID:     fixedUUID(0xC2),
	}}
	j := newCommittedRecordingJournal()
	app := NewJournalEventAppender(j)

	if !app.SupportsCommittedPublicBodies() {
		t.Fatalf("SupportsCommittedPublicBodies() = false over a CommittedPublicJournal, want true")
	}
	commit, err := app.AppendEventCommitted(context.Background(), ev)
	if err != nil {
		t.Fatalf("AppendEventCommitted() error = %v", err)
	}
	if commit.Sequence != 1 || !commit.Appended {
		t.Fatalf("AppendEventCommitted() = seq:%d appended:%t, want 1/true", commit.Sequence, commit.Appended)
	}
	if commit.EventID != ev.EventID.String() {
		t.Errorf("committed EventID = %q, want %q", commit.EventID, ev.EventID.String())
	}
	if !bytes.Equal(commit.PublicBody, j.stored[1]) {
		t.Errorf("committed PublicBody = %s, want the exact stored body %s", commit.PublicBody, j.stored[1])
	}
}

// TestJournalEventAppenderCommittedCoveredThroughEqualsCommittedSequence drives the
// watermark AT the threshold and on both sides. Two PRIVATE records occupy sequences
// 1 and 2; the public event commits at 3. CoveredThrough must be exactly 3: 2 would
// leave the private gap permanently unclosed for a public reader, and 4 would claim
// coverage of an append that has not happened.
func TestJournalEventAppenderCommittedCoveredThroughEqualsCommittedSequence(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0xC3)
	j := newCommittedRecordingJournal()
	app := NewJournalEventAppender(j)

	for i := range 2 {
		private := event.HustleStarted{Header: event.Header{
			Coordinates:     identity.Coordinates{SessionID: sid},
			EventID:         fixedUUID(byte(0xD0 + i)),
			EventVisibility: event.Internal,
		}}
		commit, err := app.AppendEventCommitted(context.Background(), private)
		if err != nil {
			t.Fatalf("private AppendEventCommitted() error = %v", err)
		}
		if commit.CoveredThrough != 0 || commit.PublicBody != nil || commit.EventID != "" {
			t.Fatalf("private commit = %+v, want no public coverage, body, or id", commit)
		}
	}

	public := event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid},
		EventID:     fixedUUID(0xD9),
	}}
	commit, err := app.AppendEventCommitted(context.Background(), public)
	if err != nil {
		t.Fatalf("public AppendEventCommitted() error = %v", err)
	}
	if commit.Sequence != 3 {
		t.Fatalf("public commit sequence = %d, want 3", commit.Sequence)
	}
	switch {
	case commit.CoveredThrough < commit.Sequence:
		t.Fatalf("CoveredThrough = %d, want %d: a lower watermark leaves the private gap at 1-2 permanently unclosed",
			commit.CoveredThrough, commit.Sequence)
	case commit.CoveredThrough > commit.Sequence:
		t.Fatalf("CoveredThrough = %d, want %d: a watermark past this append claims coverage of records that do not exist",
			commit.CoveredThrough, commit.Sequence)
	}
}

// TestJournalEventAppenderCommittedDeduplicatedRetryCarriesNoBody proves a
// deduplicated retry reports the ORIGINAL sequence with Appended=false and claims NO
// coverage and NO body: this call committed nothing, so it may not advertise a
// watermark its own append did not earn.
func TestJournalEventAppenderCommittedDeduplicatedRetryCarriesNoBody(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0xC5)
	ev := event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid},
		EventID:     fixedUUID(0xC6),
	}}
	j := newCommittedRecordingJournal()
	cat := &recordingCatalog{}
	app := NewJournalEventAppender(j, WithCatalog(cat))

	first, err := app.AppendEventCommitted(context.Background(), ev)
	if err != nil {
		t.Fatalf("first AppendEventCommitted() error = %v", err)
	}
	retry, err := app.AppendEventCommitted(context.Background(), ev)
	if err != nil {
		t.Fatalf("retry AppendEventCommitted() error = %v", err)
	}
	if retry.Sequence != first.Sequence {
		t.Errorf("retry sequence = %d, want the original %d", retry.Sequence, first.Sequence)
	}
	if retry.Appended {
		t.Errorf("retry Appended = true, want false")
	}
	if retry.PublicBody != nil || retry.CoveredThrough != 0 {
		t.Errorf("retry commit = %+v, want no body and no coverage claim", retry)
	}
	if len(j.records) != 1 {
		t.Errorf("journal recorded %d records, want 1", len(j.records))
	}
	if len(cat.events) != 1 {
		t.Errorf("catalog notified %d times, want 1", len(cat.events))
	}
}

// TestLegacyJournalAppenderDoesNotAdvertiseCommittedPublicBodies proves an appender
// over a journal WITHOUT the committed seam stays fully usable — same sequence, same
// dedup outcome — but never claims the committed-public-event capability and never
// invents bytes or a watermark it cannot source from the durable append.
func TestLegacyJournalAppenderDoesNotAdvertiseCommittedPublicBodies(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0xC7)
	ev := event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid},
		EventID:     fixedUUID(0xC8),
	}}
	for _, tt := range []struct {
		name    string
		journal SessionJournal
	}{
		{name: "plain", journal: &recordingJournal{}},
		{name: "idempotent", journal: newIdempotentRecordingJournal()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			app := NewJournalEventAppender(tt.journal)
			if app.SupportsCommittedPublicBodies() {
				t.Fatalf("SupportsCommittedPublicBodies() = true over a legacy journal, want false")
			}
			commit, err := app.AppendEventCommitted(context.Background(), ev)
			if err != nil {
				t.Fatalf("AppendEventCommitted() error = %v", err)
			}
			if commit.Sequence != 1 || !commit.Appended {
				t.Fatalf("commit = seq:%d appended:%t, want 1/true", commit.Sequence, commit.Appended)
			}
			if commit.EventID != "" || commit.PublicBody != nil || commit.CoveredThrough != 0 {
				t.Fatalf("legacy commit = %+v, want no committed public fields", commit)
			}
		})
	}
}

// misreportingJournal is a backend that reports a public body for EVERY record,
// including a private one and an ephemeral one. It is the shape a backend bug (or a
// future backend written against a looser reading of the seam) would take.
type misreportingJournal struct{ seq uint64 }

func (j *misreportingJournal) Append(ctx context.Context, rec JournalRecord) (uint64, error) {
	result, err := j.AppendCommitted(ctx, rec)
	return result.Sequence, err
}

func (j *misreportingJournal) AppendIdempotent(ctx context.Context, rec JournalRecord) (AppendResult, error) {
	result, err := j.AppendCommitted(ctx, rec)
	return result.AppendResult, err
}

func (j *misreportingJournal) AppendCommitted(_ context.Context, rec JournalRecord) (CommittedAppendResult, error) {
	j.seq++
	return CommittedAppendResult{
		AppendResult: AppendResult{Sequence: j.seq, Appended: true},
		Public:       CommittedPublicBody{EventID: rec.IdempotencyID(), Body: []byte(`{"leaked":true}`)},
	}, nil
}

// TestJournalEventAppenderRefusesPublicBytesForNonPublicEnduringEvents asserts the
// class/visibility guard AT the seam that publishes the bytes, not one level away in
// the backend. A private event and an ephemeral event have no public publication, so
// no reported body may ride onto one — a backend that offers bytes for either is not
// believed, and the delivery stays empty rather than leaking an internal record or
// giving an unpersisted event a committed identity.
func TestJournalEventAppenderRefusesPublicBytesForNonPublicEnduringEvents(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0xF1)
	tests := []struct {
		name  string
		event event.Event
	}{
		{
			name: "private enduring",
			event: event.HustleStarted{Header: event.Header{
				Coordinates:     identity.Coordinates{SessionID: sid},
				EventID:         fixedUUID(0xF2),
				EventVisibility: event.Internal,
			}},
		},
		{
			name: "public ephemeral",
			event: event.TokenDelta{Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: sid, LoopID: fixedUUID(0xF3), TurnID: fixedUUID(0xF4)},
				EventID:     fixedUUID(0xF5),
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := NewJournalEventAppender(&misreportingJournal{})
			commit, err := app.AppendEventCommitted(context.Background(), tt.event)
			if err != nil {
				t.Fatalf("AppendEventCommitted() error = %v", err)
			}
			if commit.EventID != "" || commit.PublicBody != nil || commit.CoveredThrough != 0 {
				t.Fatalf("commit = %+v, want no public id, body, or coverage for a %s event", commit, tt.name)
			}
			if commit.PublishesPublicBody() {
				t.Error("PublishesPublicBody() = true, want false")
			}
		})
	}
}
