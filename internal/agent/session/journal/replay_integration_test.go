//go:build integration

package journal_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/nats-io/nats.go"
)

// drainAll opens a cold (Follow:false) cursor for req and reads every event until
// io.EOF, returning the events paired with their stream sequences in delivery order.
// Any non-EOF error fails the test fatally so a fail-secure case is asserted by the
// caller's own Next loop, not swallowed here.
func drainAll(t *testing.T, r journal.EventReplayer, req journal.ReplayRequest) ([]event.Event, []uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cur, err := r.Open(ctx, req)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := cur.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	var evs []event.Event
	var seqs []uint64
	for {
		ev, seq, err := cur.Next(ctx)
		if errors.Is(err, io.EOF) {
			return evs, seqs
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		evs = append(evs, ev)
		seqs = append(seqs, seq)
	}
}

// appendEvent marshals nothing itself — it wraps ev in an EventRecord and appends it
// through the real journal, returning the assigned sequence.
func appendEvent(t *testing.T, j journal.SessionJournal, ctx context.Context, ev event.Event) uint64 {
	t.Helper()
	seq, err := j.Append(ctx, journal.NewEventRecord(ev))
	if err != nil {
		t.Fatalf("Append(%T): %v", ev, err)
	}
	return seq
}

// TestEventReplayerColdBacklogInOrder is the core Task 5.3 assertion. A mix of inline
// events (SessionStarted, LoopStarted, a small StepDone, a TurnDone) plus one offloaded
// event (a StepDone with a > 512 KiB block) is appended, and a cold cursor drains them
// back: in stream-sequence order, deep-equal to what was appended, with the offloaded
// record rehydrated byte-for-byte. A command and a fence are also appended and must NOT
// appear in the replay (event subjects only).
func TestEventReplayerColdBacklogInOrder(t *testing.T) {
	sid := seedUUID(0xA0)
	lid := seedUUID(0xA1)
	tid := seedUUID(0xA2)
	stepID := seedUUID(0xA3)

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid)
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sessionStarted := event.SessionStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid},
			EventID:     seedUUID(0xB0),
		},
	}
	loopStarted := event.LoopStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
			EventID:     seedUUID(0xB1),
		},
	}
	smallStep := event.StepDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID},
			EventID:     seedUUID(0xB2),
		},
		Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: "done"}},
			}},
		},
	}
	// > 512 KiB once marshaled: forced down the offload path on append, must rehydrate.
	const blockChars = 700 * 1024
	bigStep := event.StepDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: seedUUID(0xB4)},
			EventID:     seedUUID(0xB3),
		},
		Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: strings.Repeat("x", blockChars)}},
			}},
		},
	}
	turnDone := event.TurnDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid},
			EventID:     seedUUID(0xB5),
		},
		TurnIndex: 1,
		Message: &content.AIMessage{Message: content.Message{
			Role:   content.RoleAssistant,
			Blocks: []content.Block{&content.TextBlock{Text: "complete"}},
		}},
	}

	want := []event.Event{sessionStarted, loopStarted, smallStep, bigStep, turnDone}
	for _, ev := range want {
		appendEvent(t, j, ctx, ev)
	}

	// A command and a fence on non-event subjects — must NOT be replayed.
	cmd := command.UserInput{
		Header: command.Header{CommandID: seedUUID(0xC0)},
		Blocks: []content.Block{&content.TextBlock{Text: "hi"}},
	}
	if _, err := j.Append(ctx, journal.NewCommandRecord(sid, lid, cmd)); err != nil {
		t.Fatalf("Append(command): %v", err)
	}
	if _, err := j.Append(ctx, journal.NewFenceRecord(sid, journal.LeaseFence{Epoch: 7})); err != nil {
		t.Fatalf("Append(fence): %v", err)
	}

	r := journal.NewEventReplayer(js, mustObjectStore(t, js, sid))
	got, seqs := drainAll(t, r, journal.ReplayRequest{SessionID: sid, LoopID: lid, From: journal.Beginning(), Follow: false})

	if len(got) != len(want) {
		t.Fatalf("replayed %d events, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("event[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
	// Sequences strictly increase (stream-sequence order); command+fence interleave the
	// stream but are filtered out, so the replayed seqs are a strictly-increasing subset.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("seq[%d]=%d not strictly greater than seq[%d]=%d", i, seqs[i], i-1, seqs[i-1])
		}
	}
}

// TestEventReplayerLoopFiltering asserts the subject filter: LoopID:0 yields the session
// event plus every loop's events; a specific LoopID yields the session event plus only
// that loop's events. Two loops are appended interleaved.
func TestEventReplayerLoopFiltering(t *testing.T) {
	sid := seedUUID(0xD0)
	loopA := seedUUID(0xD1)
	loopB := seedUUID(0xD2)

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid)
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid}, EventID: seedUUID(0xE0)}}
	aStart := event.LoopStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: loopA}, EventID: seedUUID(0xE1)}}
	bStart := event.LoopStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: loopB}, EventID: seedUUID(0xE2)}}
	aIdle := event.LoopIdle{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: loopA}, EventID: seedUUID(0xE3)}}
	bIdle := event.LoopIdle{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: loopB}, EventID: seedUUID(0xE4)}}

	// Interleave the two loops' events in the stream.
	for _, ev := range []event.Event{sess, aStart, bStart, aIdle, bIdle} {
		appendEvent(t, j, ctx, ev)
	}

	r := journal.NewEventReplayer(js, mustObjectStore(t, js, sid))

	t.Run("all loops (LoopID zero)", func(t *testing.T) {
		got, _ := drainAll(t, r, journal.ReplayRequest{SessionID: sid, From: journal.Beginning()})
		want := []event.Event{sess, aStart, bStart, aIdle, bIdle}
		assertEventsEqual(t, got, want)
	})

	t.Run("only loop A", func(t *testing.T) {
		got, _ := drainAll(t, r, journal.ReplayRequest{SessionID: sid, LoopID: loopA, From: journal.Beginning()})
		want := []event.Event{sess, aStart, aIdle}
		assertEventsEqual(t, got, want)
	})

	t.Run("only loop B", func(t *testing.T) {
		got, _ := drainAll(t, r, journal.ReplayRequest{SessionID: sid, LoopID: loopB, From: journal.Beginning()})
		want := []event.Event{sess, bStart, bIdle}
		assertEventsEqual(t, got, want)
	})
}

// TestEventReplayerFromSeq asserts the dormant-snapshot start hook: opening at FromSeq(n)
// begins delivery at stream sequence n (inclusive), skipping everything before it.
func TestEventReplayerFromSeq(t *testing.T) {
	sid := seedUUID(0xF0)
	lid := seedUUID(0xF1)

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid)
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid}, EventID: seedUUID(0x01)}}
	loop := event.LoopStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}, EventID: seedUUID(0x02)}}
	idle := event.LoopIdle{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}, EventID: seedUUID(0x03)}}

	appendEvent(t, j, ctx, sess)            // seq 1
	loopSeq := appendEvent(t, j, ctx, loop) // seq 2
	appendEvent(t, j, ctx, idle)            // seq 3

	r := journal.NewEventReplayer(js, mustObjectStore(t, js, sid))
	got, seqs := drainAll(t, r, journal.ReplayRequest{SessionID: sid, LoopID: lid, From: journal.FromSeq(loopSeq)})

	want := []event.Event{loop, idle}
	assertEventsEqual(t, got, want)
	if len(seqs) == 0 || seqs[0] != loopSeq {
		t.Errorf("first replayed seq = %v, want %d (FromSeq start is inclusive)", seqs, loopSeq)
	}
}

// TestEventReplayerEmptyStreamEOF asserts a cold cursor over a stream with no matching
// events returns io.EOF immediately on the first Next.
func TestEventReplayerEmptyStreamEOF(t *testing.T) {
	sid := seedUUID(0x10)
	lid := seedUUID(0x11)

	_, js := newEmbeddedJS(t)
	if _, err := journal.NewSessionJournal(js, sid); err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	r := journal.NewEventReplayer(js, mustObjectStore(t, js, sid))
	got, _ := drainAll(t, r, journal.ReplayRequest{SessionID: sid, LoopID: lid, From: journal.Beginning()})
	if len(got) != 0 {
		t.Errorf("empty stream replayed %d events, want 0", len(got))
	}
}

// TestEventReplayerFailSecureMissingObject deletes an offloaded record's backing object
// from the bucket and asserts the cursor surfaces a typed *ObjectMissingError (fail
// secure: a dangling pointer is never silently skipped or zero-valued).
func TestEventReplayerFailSecureMissingObject(t *testing.T) {
	sid := seedUUID(0x20)
	lid := seedUUID(0x21)

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid)
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bigStep := largeStepDone(sid, lid, 0x22, 700*1024)
	appendEvent(t, j, ctx, bigStep)

	store := mustObjectStore(t, js, sid)
	objID := onlyObjectID(t, store)
	if err := store.Delete(objID); err != nil {
		t.Fatalf("Delete(%s): %v", objID, err)
	}

	r := journal.NewEventReplayer(js, store)
	cur, err := r.Open(ctx, journal.ReplayRequest{SessionID: sid, LoopID: lid, From: journal.Beginning()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cur.Close()

	_, _, err = cur.Next(ctx)
	var missing *journal.ObjectMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("Next error = %v, want *ObjectMissingError", err)
	}
	if missing.ObjectID != objID {
		t.Errorf("ObjectMissingError.ObjectID = %q, want %q", missing.ObjectID, objID)
	}
}

// TestEventReplayerFailSecureCorruptObject overwrites an offloaded record's backing
// object with wrong bytes and asserts the cursor surfaces a typed *ObjectCorruptError
// (the sha256 re-hash of the fetched bytes no longer matches the pointer's object id).
func TestEventReplayerFailSecureCorruptObject(t *testing.T) {
	sid := seedUUID(0x30)
	lid := seedUUID(0x31)

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid)
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bigStep := largeStepDone(sid, lid, 0x32, 700*1024)
	appendEvent(t, j, ctx, bigStep)

	store := mustObjectStore(t, js, sid)
	objID := onlyObjectID(t, store)
	// Overwrite the object's bytes IN PLACE (same name) with content that does not hash
	// to objID — a corruption the replayer must detect on re-hash.
	if _, err := store.PutBytes(objID, []byte("corrupted-not-the-original-bytes"), nats.Context(ctx)); err != nil {
		t.Fatalf("PutBytes(corrupt): %v", err)
	}

	r := journal.NewEventReplayer(js, store)
	cur, err := r.Open(ctx, journal.ReplayRequest{SessionID: sid, LoopID: lid, From: journal.Beginning()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cur.Close()

	_, _, err = cur.Next(ctx)
	var corrupt *journal.ObjectCorruptError
	if !errors.As(err, &corrupt) {
		t.Fatalf("Next error = %v, want *ObjectCorruptError", err)
	}
	if corrupt.ObjectID != objID {
		t.Errorf("ObjectCorruptError.ObjectID = %q, want %q", corrupt.ObjectID, objID)
	}
}

// TestEventReplayerCallerDeadlineMidDrainNoSilentEOF is the data-loss regression for the
// Task 5.3 review finding: a cold drain whose CALLER context deadline expires mid-backlog
// must NOT return io.EOF (a silent truncation that a Phase-8 restore would persist as a
// complete-but-short history). It must fail closed with a typed *ReplayReadError while
// undelivered backlog remains (delivered < lastSeq and no NumPending==0 yet seen).
//
// Construction: append a sizable backlog (200 events), open with a generous context (so
// Open's StreamInfo succeeds and the tip is captured), then drain with a caller context
// carrying a deadline short enough to fire partway through the drain. The cursor's
// per-fetch child context is derived from this caller context, so once the caller
// deadline expires every subsequent Fetch returns context.DeadlineExceeded — which the
// UNFIXED code mistakes for the benign caught-up signal and turns into io.EOF.
func TestEventReplayerCallerDeadlineMidDrainNoSilentEOF(t *testing.T) {
	sid := seedUUID(0x40)
	lid := seedUUID(0x41)

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid)
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer setupCancel()

	const backlog = 200
	for i := 0; i < backlog; i++ {
		// Each event needs a unique, non-zero EventID: the journal dedups on a msg-id derived
		// from it (a repeated id trips the single-writer fence), and a zero id fails event
		// validation on decode. Encode i+1 across the trailing bytes.
		var eid uuid.UUID
		n := i + 1
		eid[13] = 0xEE
		eid[14] = byte(n >> 8)
		eid[15] = byte(n)
		ev := event.LoopIdle{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
			EventID:     eid,
		}}
		appendEvent(t, j, setupCtx, ev)
	}

	r := journal.NewEventReplayer(js, mustObjectStore(t, js, sid))

	// Open with a generous context so StreamInfo succeeds and lastSeq (the tip) is captured.
	cur, err := r.Open(setupCtx, journal.ReplayRequest{SessionID: sid, LoopID: lid, From: journal.Beginning()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := cur.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// Drain with a caller context whose deadline expires mid-drain. A few milliseconds is
	// long enough to deliver the first records (a local embedded store is fast) but fires
	// well before the 200th, so the drain is interrupted with backlog still pending.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 8*time.Millisecond)
	defer drainCancel()

	var delivered int
	for {
		_, _, err := cur.Next(drainCtx)
		if err == nil {
			delivered++
			continue
		}
		// The contract: while the caller deadline killed the drain mid-backlog, Next must
		// surface a typed *ReplayReadError (fail closed), NEVER io.EOF (silent truncation).
		if errors.Is(err, io.EOF) {
			t.Fatalf("Next returned io.EOF after delivering %d/%d events with caller deadline "+
				"expired mid-drain — SILENT TRUNCATION (lost %d events)", delivered, backlog, backlog-delivered)
		}
		var readErr *journal.ReplayReadError
		if !errors.As(err, &readErr) {
			t.Fatalf("Next error = %v (%T), want *ReplayReadError", err, err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("ReplayReadError should unwrap to context.DeadlineExceeded, got %v", err)
		}
		// Backlog was interrupted, not drained: we must have stopped short of the full set.
		if delivered >= backlog {
			t.Fatalf("delivered all %d events before the deadline could interrupt the drain; "+
				"tighten the drain deadline so the mid-drain case is actually exercised", backlog)
		}
		return
	}
}

// mustObjectStore binds the per-session object store bucket or fails the test.
func mustObjectStore(t *testing.T, js nats.JetStreamContext, sid uuid.UUID) nats.ObjectStore {
	t.Helper()
	store, err := js.ObjectStore(journal.SessionObjectBucket(sid))
	if err != nil {
		t.Fatalf("ObjectStore: %v", err)
	}
	return store
}

// onlyObjectID returns the single object id present in store, failing if there is not
// exactly one (the fail-secure tests append exactly one offloaded record).
func onlyObjectID(t *testing.T, store nats.ObjectStore) string {
	t.Helper()
	objs, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("object bucket has %d objects, want exactly 1", len(objs))
	}
	return objs[0].Name
}

// assertEventsEqual asserts got is element-wise deep-equal to want.
func assertEventsEqual(t *testing.T, got, want []event.Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("event[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}
