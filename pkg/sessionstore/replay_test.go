package sessionstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/core/uuid"
	"github.com/looprig/storekit"
	"github.com/looprig/storekit/memstore"
)

// replayThreshold is a small offload threshold used across the replay tests so a
// modestly padded record is forced down the blob-offload path while the tiny
// header-only records stay inline.
const replayThreshold = 2048

// fixture is one built session ledger plus the records that were appended to it
// (the opening fence first), the session id, and the ledger sequence of the one
// over-threshold record that was offloaded to a blob.
type fixture struct {
	store      *Store
	id         uuid.UUID
	want       []journal.JournalRecord // in append order; want[0] is the opening fence
	offloadSeq uint64                  // ledger seq of the offloaded (blobptr) record
}

// buildFixture opens a memstore-backed Store over backend (which may wrap Blobs),
// acquires a real lease, opens the journal (writing the opening fence), and appends
// a fixed mix of records: two small inline events, a small inline command, one
// over-threshold event (offloaded to a blob), and a final small event. It returns
// the fixture with want holding every record in stream order — the opening fence at
// index 0 — so a replay can be compared against it.
//
// The command is appended with a ZERO dispatch loop id on purpose: the storekit
// envelope frames only {kind, id, payload} and does not persist a command's
// dispatch loop id (the NATS journal recovered it from the subject; there is no
// subject here). The RecordReplayer therefore reconstructs a CommandRecord with a
// zero loop id, so appending with a zero loop id makes the round-trip exact.
func buildFixture(t *testing.T, backend *storekit.Composite) fixture {
	t.Helper()
	st, err := Open(backend, WithOffloadThreshold(replayThreshold))
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	id := newTestUUID(t)
	loopID := newTestUUID(t)
	turnID := newTestUUID(t)
	stepID := newTestUUID(t)

	lease, err := st.AcquireLease(context.Background(), id)
	if err != nil {
		t.Fatalf("AcquireLease() err = %v", err)
	}
	epoch := lease.Epoch()

	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}

	sessionStarted := event.SessionStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: id},
			EventID:     newTestUUID(t),
		},
	}
	loopStarted := event.LoopStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: id, LoopID: loopID},
			EventID:     newTestUUID(t),
		},
	}
	interrupt := command.Interrupt{Header: command.Header{CommandID: newTestUUID(t)}}
	// > replayThreshold once marshaled: forced down the offload path on append.
	bigStep := event.StepDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: id, LoopID: loopID, TurnID: turnID, StepID: stepID},
			EventID:     newTestUUID(t),
		},
		Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: strings.Repeat("x", 4*1024)}},
			}},
		},
	}
	turnDone := event.TurnDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: id, LoopID: loopID, TurnID: turnID},
			EventID:     newTestUUID(t),
		},
		TurnIndex: 1,
		Message: &content.AIMessage{Message: content.Message{
			Role:   content.RoleAssistant,
			Blocks: []content.Block{&content.TextBlock{Text: "done"}},
		}},
	}

	want := []journal.JournalRecord{
		journal.NewFenceRecord(id, journal.LeaseFence{Epoch: epoch}), // seq 1 (opening fence)
		journal.NewEventRecord(sessionStarted),                       // seq 2
		journal.NewEventRecord(loopStarted),                          // seq 3
		journal.NewCommandRecord(id, uuid.UUID{}, interrupt),         // seq 4 (zero loop id, see doc)
		journal.NewEventRecord(bigStep),                              // seq 5 (offloaded)
		journal.NewEventRecord(turnDone),                             // seq 6
	}
	// Append everything after the opening fence (want[0]).
	for _, rec := range want[1:] {
		if _, err := j.Append(context.Background(), rec); err != nil {
			t.Fatalf("Append(%T) err = %v", rec, err)
		}
	}

	// Pin that the big event actually offloaded: its ledger record must be a blobptr,
	// so the replay tests genuinely exercise the rehydration path (not an inline read).
	if env := readEnvelope(t, st, id, 5); env.Kind != string(kindBlobPtr) {
		t.Fatalf("record 5 kind = %q, want %q (bigStep must offload at threshold %d)", env.Kind, kindBlobPtr, replayThreshold)
	}

	return fixture{store: st, id: id, want: want, offloadSeq: 5}
}

// drainRecords opens a RecordCursor at req and reads every record to io.EOF,
// returning the decoded records with their sequences in order. A non-EOF error is
// fatal.
func drainRecords(t *testing.T, r journal.RecordReplayer, req journal.ReplayRequest) ([]journal.JournalRecord, []uint64) {
	t.Helper()
	cur, err := r.Open(context.Background(), req)
	if err != nil {
		t.Fatalf("RecordReplayer.Open() err = %v", err)
	}
	t.Cleanup(func() {
		if err := cur.Close(); err != nil {
			t.Errorf("RecordCursor.Close() err = %v", err)
		}
	})
	var recs []journal.JournalRecord
	var seqs []uint64
	for {
		rec, seq, err := cur.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return recs, seqs
		}
		if err != nil {
			t.Fatalf("RecordCursor.Next() err = %v", err)
		}
		recs = append(recs, rec)
		seqs = append(seqs, seq)
	}
}

// drainEvents opens an EventCursor at req and reads every event to io.EOF,
// returning the events with their sequences in order. A non-EOF error is fatal.
func drainEvents(t *testing.T, r journal.EventReplayer, req journal.ReplayRequest) ([]event.Event, []uint64) {
	t.Helper()
	cur, err := r.Open(context.Background(), req)
	if err != nil {
		t.Fatalf("EventReplayer.Open() err = %v", err)
	}
	t.Cleanup(func() {
		if err := cur.Close(); err != nil {
			t.Errorf("EventCursor.Close() err = %v", err)
		}
	})
	var evs []event.Event
	var seqs []uint64
	for {
		ev, seq, err := cur.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return evs, seqs
		}
		if err != nil {
			t.Fatalf("EventCursor.Next() err = %v", err)
		}
		evs = append(evs, ev)
		seqs = append(seqs, seq)
	}
}

// eventsOf returns the wrapped events of every EventRecord in recs, in order —
// the events the EventReplayer is expected to surface (commands and fences dropped).
func eventsOf(recs []journal.JournalRecord) []event.Event {
	var out []event.Event
	for _, rec := range recs {
		if er, ok := rec.(journal.EventRecord); ok {
			out = append(out, er.Event())
		}
	}
	return out
}

// TestRecordReplayerReplaysAppended is the replay-equals-appended assertion for the
// full RecordReplayer: a session holding a fence, events, a command, and an
// offloaded event is drained from various inclusive start sequences and yields
// exactly the appended suffix — the offloaded record rehydrated transparently — in
// order, with strictly increasing sequences.
func TestRecordReplayerReplaysAppended(t *testing.T) {
	t.Parallel()
	fx := buildFixture(t, memstore.New())

	tests := []struct {
		name    string
		fromSeq uint64
		wantLo  int // index into fx.want of the first record expected (inclusive)
	}{
		{name: "from beginning (0 clamps to first)", fromSeq: 0, wantLo: 0},
		{name: "from seq 1 (opening fence)", fromSeq: 1, wantLo: 0},
		{name: "from interior seq 4 (command)", fromSeq: 4, wantLo: 3},
		{name: "from offloaded seq 5", fromSeq: 5, wantLo: 4},
		{name: "from tip seq 6", fromSeq: 6, wantLo: 5},
		{name: "past tip yields nothing", fromSeq: 99, wantLo: len(fx.want)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rr, err := fx.store.OpenRecordReplayer(fx.id, ReplayRequest{FromSeq: tt.fromSeq})
			if err != nil {
				t.Fatalf("OpenRecordReplayer() err = %v", err)
			}
			got, seqs := drainRecords(t, rr, journal.ReplayRequest{})

			want := fx.want[tt.wantLo:]
			if len(got) != len(want) {
				t.Fatalf("replayed %d records, want %d", len(got), len(want))
			}
			for i := range want {
				if !reflect.DeepEqual(got[i], want[i]) {
					t.Errorf("record[%d] =\n%#v\nwant\n%#v", i, got[i], want[i])
				}
			}
			// Sequences are strictly increasing and start inclusively at the request.
			for i := 1; i < len(seqs); i++ {
				if seqs[i] <= seqs[i-1] {
					t.Errorf("seq[%d]=%d not > seq[%d]=%d", i, seqs[i], i-1, seqs[i-1])
				}
			}
			if len(seqs) > 0 {
				wantFirst := uint64(tt.wantLo + 1) // 1-based ledger seq of want[wantLo]
				if tt.fromSeq > wantFirst {
					wantFirst = tt.fromSeq
				}
				if seqs[0] != wantFirst {
					t.Errorf("first replayed seq = %d, want %d (FromSeq inclusive)", seqs[0], wantFirst)
				}
			}
		})
	}
}

// TestRecordReplayerSurfacesFence pins that the RecordReplayer surfaces the opening
// LeaseFence as a FenceRecord (fences are NOT filtered out) — the property the
// EventReplayer lacks.
func TestRecordReplayerSurfacesFence(t *testing.T) {
	t.Parallel()
	fx := buildFixture(t, memstore.New())
	rr, err := fx.store.OpenRecordReplayer(fx.id, ReplayRequest{FromSeq: 1})
	if err != nil {
		t.Fatalf("OpenRecordReplayer() err = %v", err)
	}
	got, _ := drainRecords(t, rr, journal.ReplayRequest{})
	if len(got) == 0 {
		t.Fatal("no records replayed")
	}
	fence, ok := got[0].(journal.FenceRecord)
	if !ok {
		t.Fatalf("record[0] = %T, want journal.FenceRecord (opening fence)", got[0])
	}
	if want := fx.want[0].(journal.FenceRecord); fence.Fence().Epoch != want.Fence().Epoch {
		t.Errorf("opening fence epoch = %d, want %d", fence.Fence().Epoch, want.Fence().Epoch)
	}
}

// TestEventReplayerYieldsEventsOnly is the EventReplayer filtering assertion: over
// the SAME ledger the event-only replayer yields exactly the events — every command
// AND fence filtered out — matching pkg/journal's subject-filtered EventReplayer.
func TestEventReplayerYieldsEventsOnly(t *testing.T) {
	t.Parallel()
	fx := buildFixture(t, memstore.New())

	tests := []struct {
		name    string
		fromSeq uint64
		wantLo  int
	}{
		{name: "from beginning", fromSeq: 0, wantLo: 0},
		{name: "from seq 1 (fence skipped)", fromSeq: 1, wantLo: 0},
		{name: "from interior seq 4 (command skipped)", fromSeq: 4, wantLo: 3},
		{name: "from offloaded event seq 5", fromSeq: 5, wantLo: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			er, err := fx.store.OpenEventReplayer(fx.id, ReplayRequest{FromSeq: tt.fromSeq})
			if err != nil {
				t.Fatalf("OpenEventReplayer() err = %v", err)
			}
			got, _ := drainEvents(t, er, journal.ReplayRequest{})
			want := eventsOf(fx.want[tt.wantLo:])
			if !reflect.DeepEqual(got, want) {
				t.Errorf("events =\n%#v\nwant\n%#v", got, want)
			}
		})
	}
}

// TestEventReplayerNarrowsByLoop covers the loop-narrowing filter that makes the
// EventReplayer a faithful drop-in for restore's foldPrimaryLoop: a session holding
// events from a PRIMARY loop and a SUBAGENT loop (plus a session-scoped event) is
// replayed with req.LoopID = primary and yields ONLY the primary loop's events plus
// the session-scoped event (matching pkg/journal's session-event + this-loop subject
// filter) — the subagent loop's events are dropped so they cannot corrupt the primary
// thread. With LoopID unset every loop's events are yielded.
func TestEventReplayerNarrowsByLoop(t *testing.T) {
	t.Parallel()
	st, err := Open(memstore.New(), WithOffloadThreshold(replayThreshold))
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	id := newTestUUID(t)
	primary := newTestUUID(t)
	subagent := newTestUUID(t)
	pTurn := newTestUUID(t)
	sTurn := newTestUUID(t)

	lease, err := st.AcquireLease(context.Background(), id)
	if err != nil {
		t.Fatalf("AcquireLease() err = %v", err)
	}
	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}

	aiMsg := func() *content.AIMessage {
		return &content.AIMessage{Message: content.Message{
			Role:   content.RoleAssistant,
			Blocks: []content.Block{&content.TextBlock{Text: "done"}},
		}}
	}
	sessionStarted := event.SessionStarted{ // session-scoped: LoopID zero
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: id}, EventID: newTestUUID(t)},
	}
	pLoop := event.LoopStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: id, LoopID: primary}, EventID: newTestUUID(t)},
	}
	sLoop := event.LoopStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: id, LoopID: subagent}, EventID: newTestUUID(t)},
	}
	pTurnDone := event.TurnDone{
		Header:    event.Header{Coordinates: identity.Coordinates{SessionID: id, LoopID: primary, TurnID: pTurn}, EventID: newTestUUID(t)},
		TurnIndex: 1,
		Message:   aiMsg(),
	}
	sTurnDone := event.TurnDone{
		Header:    event.Header{Coordinates: identity.Coordinates{SessionID: id, LoopID: subagent, TurnID: sTurn}, EventID: newTestUUID(t)},
		TurnIndex: 1,
		Message:   aiMsg(),
	}

	// Interleave the two loops so narrowing (not luck of ordering) is what excludes
	// the subagent's events.
	appendOrder := []event.Event{sessionStarted, pLoop, sLoop, pTurnDone, sTurnDone}
	for _, ev := range appendOrder {
		if _, err := j.Append(context.Background(), journal.NewEventRecord(ev)); err != nil {
			t.Fatalf("Append(%T) err = %v", ev, err)
		}
	}

	tests := []struct {
		name string
		req  journal.ReplayRequest
		want []event.Event
	}{
		{
			name: "narrow to primary loop (session-scoped kept, subagent dropped)",
			req:  journal.ReplayRequest{LoopID: primary},
			want: []event.Event{sessionStarted, pLoop, pTurnDone},
		},
		{
			name: "unnarrowed yields every loop's events",
			req:  journal.ReplayRequest{},
			want: appendOrder,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			er, err := st.OpenEventReplayer(id, ReplayRequest{FromSeq: 1})
			if err != nil {
				t.Fatalf("OpenEventReplayer() err = %v", err)
			}
			got, _ := drainEvents(t, er, tt.req)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("events =\n%#v\nwant\n%#v", got, tt.want)
			}
		})
	}
}

// TestReplayBlobResolution covers the offloaded-record path end to end: the happy
// path rehydrates the original event transparently (the reader never sees a
// blobptr); a corrupted blob (bytes whose sha256 no longer matches the pointer)
// fails closed with *BlobIntegrityError; a deleted blob fails closed with
// *BlobUnavailableError wrapping *storekit.BlobNotFoundError. None yields a bogus
// record.
func TestReplayBlobResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		tamper     func(t *testing.T, fx fixture, backend *storekit.Composite)
		wantErrAs  func(err error) bool
		wantResolv bool // the offloaded record resolves cleanly to its original kind
	}{
		{
			name:       "happy path rehydrates transparently",
			tamper:     func(t *testing.T, fx fixture, backend *storekit.Composite) {},
			wantResolv: true,
		},
		{
			name: "corrupt blob fails closed with BlobIntegrityError",
			tamper: func(t *testing.T, fx fixture, backend *storekit.Composite) {
				// The backend's Blobs is a corruptGetBlobs wrapper; arm it.
				backend.Blobs.(*corruptGetBlobs).corrupt.Store(true)
			},
			wantErrAs: func(err error) bool {
				var ie *BlobIntegrityError
				return errors.As(err, &ie)
			},
		},
		{
			name: "oversized blob fails closed with BlobIntegrityError (Size+1 bound)",
			tamper: func(t *testing.T, fx fixture, backend *storekit.Composite) {
				// Get returns MORE bytes than the pointer's Size: the bounded read caps
				// at Size+1, so len(raw) != ptr.Size trips the guard without an
				// unbounded read — pinning the anti-OOM bound.
				backend.Blobs.(*corruptGetBlobs).oversize.Store(true)
			},
			wantErrAs: func(err error) bool {
				var ie *BlobIntegrityError
				return errors.As(err, &ie)
			},
		},
		{
			name: "missing blob fails closed with BlobUnavailableError",
			tamper: func(t *testing.T, fx fixture, backend *storekit.Composite) {
				keys, err := backend.Blobs.List(context.Background(), sessionsPrefix+fx.id.String()+blobsInfix)
				if err != nil {
					t.Fatalf("List() err = %v", err)
				}
				if len(keys) == 0 {
					t.Fatal("no offloaded blob to delete")
				}
				for _, k := range keys {
					if err := backend.Blobs.Delete(context.Background(), k); err != nil {
						t.Fatalf("Delete(%q) err = %v", k, err)
					}
				}
			},
			wantErrAs: func(err error) bool {
				var ue *BlobUnavailableError
				if !errors.As(err, &ue) {
					return false
				}
				var nf *storekit.BlobNotFoundError
				return errors.As(err, &nf) // fail-closed cause preserved
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mem := memstore.New()
			backend := &storekit.Composite{
				Ledger: mem.Ledger,
				Leaser: mem.Leaser,
				KV:     mem.KV,
				Blobs:  &corruptGetBlobs{inner: mem.Blobs},
			}
			fx := buildFixture(t, backend)
			tt.tamper(t, fx, backend)

			rr, err := fx.store.OpenRecordReplayer(fx.id, ReplayRequest{FromSeq: fx.offloadSeq})
			if err != nil {
				t.Fatalf("OpenRecordReplayer() err = %v", err)
			}
			cur, err := rr.Open(context.Background(), journal.ReplayRequest{})
			if err != nil {
				t.Fatalf("Open() err = %v", err)
			}
			defer cur.Close()

			rec, seq, err := cur.Next(context.Background())
			if tt.wantResolv {
				if err != nil {
					t.Fatalf("Next() err = %v, want the rehydrated record", err)
				}
				if seq != fx.offloadSeq {
					t.Errorf("resolved seq = %d, want %d", seq, fx.offloadSeq)
				}
				if !reflect.DeepEqual(rec, fx.want[fx.offloadSeq-1]) {
					t.Errorf("rehydrated record =\n%#v\nwant\n%#v", rec, fx.want[fx.offloadSeq-1])
				}
				return
			}
			if rec != nil {
				t.Errorf("Next() yielded a record %#v on a tampered blob; must fail closed", rec)
			}
			if !tt.wantErrAs(err) {
				t.Fatalf("Next() err = %v, want the fail-closed typed error", err)
			}
		})
	}
}

// TestReplayEmptySession covers the boundary case: replaying a session whose ledger
// was never written (absent == empty) yields no records and no error — an
// immediately-drained cursor, not a setup failure.
func TestReplayEmptySession(t *testing.T) {
	t.Parallel()
	st, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	id := newTestUUID(t)

	rr, err := st.OpenRecordReplayer(id, ReplayRequest{FromSeq: 1})
	if err != nil {
		t.Fatalf("OpenRecordReplayer() err = %v", err)
	}
	recs, _ := drainRecords(t, rr, journal.ReplayRequest{})
	if len(recs) != 0 {
		t.Errorf("replayed %d records from an empty session, want 0", len(recs))
	}

	er, err := st.OpenEventReplayer(id, ReplayRequest{FromSeq: 1})
	if err != nil {
		t.Fatalf("OpenEventReplayer() err = %v", err)
	}
	evs, _ := drainEvents(t, er, journal.ReplayRequest{})
	if len(evs) != 0 {
		t.Errorf("replayed %d events from an empty session, want 0", len(evs))
	}
}

// TestReplayFollowUnsupported covers the fail-closed follow guard: a Follow:true
// request returns a typed *journal.FollowUnsupportedError from Open rather than
// silently behaving as a cold cursor (matching pkg/journal).
func TestReplayFollowUnsupported(t *testing.T) {
	t.Parallel()
	fx := buildFixture(t, memstore.New())

	t.Run("record replayer", func(t *testing.T) {
		t.Parallel()
		rr, err := fx.store.OpenRecordReplayer(fx.id, ReplayRequest{FromSeq: 1})
		if err != nil {
			t.Fatalf("OpenRecordReplayer() err = %v", err)
		}
		_, err = rr.Open(context.Background(), journal.ReplayRequest{Follow: true})
		var fue *journal.FollowUnsupportedError
		if !errors.As(err, &fue) {
			t.Fatalf("Open(Follow:true) err = %v, want *journal.FollowUnsupportedError", err)
		}
	})
	t.Run("event replayer", func(t *testing.T) {
		t.Parallel()
		er, err := fx.store.OpenEventReplayer(fx.id, ReplayRequest{FromSeq: 1})
		if err != nil {
			t.Fatalf("OpenEventReplayer() err = %v", err)
		}
		_, err = er.Open(context.Background(), journal.ReplayRequest{Follow: true})
		var fue *journal.FollowUnsupportedError
		if !errors.As(err, &fue) {
			t.Fatalf("Open(Follow:true) err = %v, want *journal.FollowUnsupportedError", err)
		}
	})
}

// TestReplayCursorCloseIdempotent pins the cursor teardown contract for both cursor
// kinds: after a full drain to io.EOF, a first Close returns nil, a second Close is a
// no-op that also returns nil (idempotent), and any Next after Close returns io.EOF
// rather than panicking or erroring.
func TestReplayCursorCloseIdempotent(t *testing.T) {
	t.Parallel()
	fx := buildFixture(t, memstore.New())

	// drainToEOF reads until io.EOF via next, failing on any other error. next abstracts
	// over the two cursors' (differently-typed) Next methods.
	drainToEOF := func(t *testing.T, next func() error) {
		t.Helper()
		for {
			err := next()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				t.Fatalf("Next() err = %v", err)
			}
		}
	}
	// assertClosed runs the shared Close/Close/Next assertions given a drained cursor.
	assertClosed := func(t *testing.T, closer func() error, next func() error) {
		t.Helper()
		if err := closer(); err != nil {
			t.Errorf("first Close() = %v, want nil", err)
		}
		if err := closer(); err != nil {
			t.Errorf("second Close() = %v, want nil (idempotent)", err)
		}
		if err := next(); !errors.Is(err, io.EOF) {
			t.Errorf("Next() after Close = %v, want io.EOF", err)
		}
	}

	t.Run("record cursor", func(t *testing.T) {
		t.Parallel()
		rr, err := fx.store.OpenRecordReplayer(fx.id, ReplayRequest{FromSeq: 1})
		if err != nil {
			t.Fatalf("OpenRecordReplayer() err = %v", err)
		}
		cur, err := rr.Open(context.Background(), journal.ReplayRequest{})
		if err != nil {
			t.Fatalf("Open() err = %v", err)
		}
		next := func() error { _, _, err := cur.Next(context.Background()); return err }
		drainToEOF(t, next)
		assertClosed(t, cur.Close, next)
	})

	t.Run("event cursor", func(t *testing.T) {
		t.Parallel()
		er, err := fx.store.OpenEventReplayer(fx.id, ReplayRequest{FromSeq: 1})
		if err != nil {
			t.Fatalf("OpenEventReplayer() err = %v", err)
		}
		cur, err := er.Open(context.Background(), journal.ReplayRequest{})
		if err != nil {
			t.Fatalf("Open() err = %v", err)
		}
		next := func() error { _, _, err := cur.Next(context.Background()); return err }
		drainToEOF(t, next)
		assertClosed(t, cur.Close, next)
	})
}

// --- test doubles ---------------------------------------------------------

// corruptGetBlobs wraps a Blobs, delegating Put/Delete/List unchanged so the writer
// stores real bytes, but tampering with every Get response once armed — so an
// offloaded record's rehydration reads bytes that fail the integrity guard, driving
// the *BlobIntegrityError fail-closed path. Put is never tampered (the writer's blob
// must land intact); only replay-time Get is. Two independent tamper modes exercise
// the two halves of the guard: corrupt flips a byte (same length → the sha256 half),
// oversize appends bytes past the pointer's Size (→ the len(raw) != ptr.Size half,
// the anti-OOM Size+1 bound).
type corruptGetBlobs struct {
	inner    storekit.Blobs
	corrupt  atomic.Bool
	oversize atomic.Bool
}

func (b *corruptGetBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	return b.inner.Put(ctx, key, r)
}

func (b *corruptGetBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := b.inner.Get(ctx, key)
	if err != nil {
		return rc, err
	}
	corrupt, oversize := b.corrupt.Load(), b.oversize.Load()
	if !corrupt && !oversize {
		return rc, nil
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	switch {
	case oversize:
		// More bytes than the pointer declared: the bounded LimitReader(Size+1) read
		// yields Size+1 bytes, so len(raw) != ptr.Size trips the integrity guard before
		// an unbounded read could occur.
		data = append(data, 0xDE, 0xAD, 0xBE, 0xEF)
	case len(data) == 0:
		data = []byte{0x00} // never byte-identical to an empty original
	default:
		data[0] ^= 0xFF // flip the first byte: sha256 no longer matches the pointer
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *corruptGetBlobs) Delete(ctx context.Context, key string) error {
	return b.inner.Delete(ctx, key)
}

func (b *corruptGetBlobs) List(ctx context.Context, prefix string) ([]string, error) {
	return b.inner.List(ctx, prefix)
}

var _ storekit.Blobs = (*corruptGetBlobs)(nil)
