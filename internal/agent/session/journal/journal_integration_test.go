//go:build integration

package journal_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
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

// seedUUID builds a deterministic non-zero uuid from a single seed byte so the
// integration records carry stable, readable ids. (Mirrors the white-box test
// helper fixedUUID, redefined here because this file is package journal_test.)
func seedUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

// recordKind tags how a stored record's Data must be decoded for the round-trip
// assertion: the three codecs are distinct, so the table names which one applies.
type recordKind uint8

const (
	kindEvent recordKind = iota
	kindCommand
	kindFence
)

// appendCase is one table row: the record to append plus the subject, Msg-Id, and
// codec-keyed expected value the stored record must round-trip back to.
type appendCase struct {
	name        string
	rec         journal.JournalRecord
	kind        recordKind
	wantSubject string
	wantMsgID   string
	// wantEvent/wantCommand/wantFence is the value the stored Data must decode
	// deep-equal to; exactly one is set per row, keyed by kind.
	wantEvent   event.Event
	wantCommand command.Command
	wantFence   journal.LeaseFence
}

// TestSessionJournalAppend exercises the happy path of the single-writer
// serializer against a real embedded JetStream server: the journal's opening
// LeaseFence lands first (seq 1, carrying the lease's epoch), then several events and
// a command are appended in order, and each is asserted to land at a strictly
// monotonic sequence on the expected subject with the expected Nats-Msg-Id, and to
// decode back deep-equal to what was written.
func TestSessionJournalAppend(t *testing.T) {
	sid := seedUUID(0x10)
	lid := seedUUID(0x11)
	tid := seedUUID(0x12)
	stepID := seedUUID(0x13)

	sessionStarted := event.SessionStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid},
			EventID:     seedUUID(0x20),
		},
	}
	loopStarted := event.LoopStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
			EventID:     seedUUID(0x21),
		},
	}
	stepDone := event.StepDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID},
			EventID:     seedUUID(0x22),
		},
		Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: "done"}},
			}},
		},
	}
	userInput := command.UserInput{
		Header: command.Header{CommandID: seedUUID(0x30)},
		Blocks: []content.Block{&content.TextBlock{Text: "hello"}},
	}

	cases := []appendCase{
		{
			name:        "session started",
			rec:         journal.NewEventRecord(sessionStarted),
			kind:        kindEvent,
			wantSubject: journal.SessionEventSubject(sid),
			wantMsgID:   sessionStarted.EventID.String(),
			wantEvent:   sessionStarted,
		},
		{
			name:        "loop started",
			rec:         journal.NewEventRecord(loopStarted),
			kind:        kindEvent,
			wantSubject: journal.LoopEventSubject(sid, lid),
			wantMsgID:   loopStarted.EventID.String(),
			wantEvent:   loopStarted,
		},
		{
			name:        "step done with messages",
			rec:         journal.NewEventRecord(stepDone),
			kind:        kindEvent,
			wantSubject: journal.LoopEventSubject(sid, lid),
			wantMsgID:   stepDone.EventID.String(),
			wantEvent:   stepDone,
		},
		{
			name:        "user input command",
			rec:         journal.NewCommandRecord(sid, lid, userInput),
			kind:        kindCommand,
			wantSubject: journal.LoopCommandSubject(sid, lid),
			wantMsgID:   userInput.CommandID.String(),
			wantCommand: userInput,
		},
	}

	_, js := newEmbeddedJS(t)
	lease := mustAcquireLease(t, js, sid)
	j, err := journal.NewSessionJournal(js, sid, lease)
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The journal's FIRST append is its opening LeaseFence at seq 1, carrying the
	// lease's epoch on the session's fence subject. Assert it before the user records.
	fenceRaw, err := js.GetMsg(journal.StreamName(sid), 1)
	if err != nil {
		t.Fatalf("GetMsg(opening fence seq 1): %v", err)
	}
	if fenceRaw.Subject != journal.FenceSubject(sid) {
		t.Errorf("opening fence subject = %q, want %q", fenceRaw.Subject, journal.FenceSubject(sid))
	}
	gotFence, err := journal.UnmarshalLeaseFence(fenceRaw.Data)
	if err != nil {
		t.Fatalf("UnmarshalLeaseFence(opening fence): %v", err)
	}
	if gotFence.Epoch != lease.Epoch() {
		t.Errorf("opening fence epoch = %d, want lease epoch %d", gotFence.Epoch, lease.Epoch())
	}

	var lastSeq uint64
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq, err := j.Append(ctx, tc.rec)
			if err != nil {
				t.Fatalf("Append(%s): %v", tc.name, err)
			}
			// User records follow the opening LeaseFence (seq 1): strictly monotonic,
			// gap-free, starting at seq 2.
			wantSeq := uint64(i + 2)
			if seq != wantSeq {
				t.Fatalf("Append(%s) seq = %d, want %d (strictly monotonic)", tc.name, seq, wantSeq)
			}
			lastSeq = seq

			// Read the record back BY SEQUENCE and assert subject + Msg-Id header.
			raw, err := js.GetMsg(journal.StreamName(sid), seq)
			if err != nil {
				t.Fatalf("GetMsg(seq %d): %v", seq, err)
			}
			if raw.Subject != tc.wantSubject {
				t.Errorf("stored subject = %q, want %q", raw.Subject, tc.wantSubject)
			}
			if got := raw.Header.Get(nats.MsgIdHdr); got != tc.wantMsgID {
				t.Errorf("stored %s = %q, want %q", nats.MsgIdHdr, got, tc.wantMsgID)
			}

			// Decode the stored Data via the kind's codec and assert deep-equal.
			assertRoundTrip(t, tc, raw.Data)
		})
	}

	// The stream tip equals the last returned sequence: the journal and the stream
	// agree on the durable length.
	info, err := js.StreamInfo(journal.StreamName(sid))
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if info.State.LastSeq != lastSeq {
		t.Errorf("StreamInfo LastSeq = %d, want %d (last returned seq)", info.State.LastSeq, lastSeq)
	}
	// The opening LeaseFence plus every user record.
	if want := uint64(len(cases) + 1); info.State.Msgs != want {
		t.Errorf("StreamInfo Msgs = %d, want %d (opening LeaseFence + %d records)", info.State.Msgs, want, len(cases))
	}
}

// TestNewSessionJournalRejectsNilLease asserts the constructor fails closed when
// handed a nil lease against a LIVE server: the lease is a required dependency (DIP),
// so a nil one returns a *StreamSetupError unwrapping to a nil-lease cause before any
// stream is taken over. (The nil-jetstream guard, which runs first, is covered in the
// white-box suite.)
func TestNewSessionJournalRejectsNilLease(t *testing.T) {
	sid := seedUUID(0x58)
	_, js := newEmbeddedJS(t)

	j, err := journal.NewSessionJournal(js, sid, nil)
	if err == nil {
		t.Fatalf("NewSessionJournal(js, sid, nil) err = nil, want error")
	}
	if j != nil {
		t.Errorf("NewSessionJournal(js, sid, nil) journal = %v, want nil", j)
	}
	var setupErr *journal.StreamSetupError
	if !errors.As(err, &setupErr) {
		t.Fatalf("error %v is not *StreamSetupError", err)
	}
	if setupErr.Stream != journal.StreamName(sid) {
		t.Errorf("StreamSetupError.Stream = %q, want %q", setupErr.Stream, journal.StreamName(sid))
	}
}

// TestNewSessionJournalRejectsMismatchedStream asserts the constructor fails closed
// when an existing stream under the per-session name diverges from the durability
// contract. A stream pre-created with WorkQueue retention (or wrong subjects) is NOT
// silently bound — ensureStream must verify the existing config and return a typed
// *StreamSetupError (phase "verify"), protecting the keep-everything guarantee
// against a fail-open rebind onto a divergent stream.
func TestNewSessionJournalRejectsMismatchedStream(t *testing.T) {
	tests := []struct {
		name   string
		preCfg func(sid uuid.UUID) *nats.StreamConfig
	}{
		{
			name: "wrong retention (WorkQueue)",
			preCfg: func(sid uuid.UUID) *nats.StreamConfig {
				return &nats.StreamConfig{
					Name:      journal.StreamName(sid),
					Subjects:  []string{"urvi.session." + sid.String() + ".>"},
					Retention: nats.WorkQueuePolicy,
				}
			},
		},
		{
			name: "wrong subjects",
			preCfg: func(sid uuid.UUID) *nats.StreamConfig {
				return &nats.StreamConfig{
					Name:      journal.StreamName(sid),
					Subjects:  []string{"some.other.subject.>"},
					Retention: nats.LimitsPolicy,
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			sid := seedUUID(0x40)
			_, js := newEmbeddedJS(t)

			// Pre-create a divergent stream under the per-session name.
			if _, err := js.AddStream(tt.preCfg(sid)); err != nil {
				t.Fatalf("pre-create stream: %v", err)
			}

			// A valid lease is supplied, but construction fails at stream-verify before
			// the lease is ever used (the opening fence is never written onto a divergent
			// stream).
			j, err := journal.NewSessionJournal(js, sid, mustAcquireLease(t, js, sid))
			if err == nil {
				t.Fatalf("NewSessionJournal bound a divergent stream (j=%v); want a verify error", j)
			}
			if j != nil {
				t.Errorf("NewSessionJournal returned non-nil journal %v alongside error", j)
			}
			var setupErr *journal.StreamSetupError
			if !errors.As(err, &setupErr) {
				t.Fatalf("error %v is not *StreamSetupError", err)
			}
			if setupErr.Phase != journal.PhaseVerify {
				t.Errorf("StreamSetupError.Phase = %q, want %q", setupErr.Phase, journal.PhaseVerify)
			}
			if setupErr.Stream != journal.StreamName(sid) {
				t.Errorf("StreamSetupError.Stream = %q, want %q", setupErr.Stream, journal.StreamName(sid))
			}
		})
	}
}

// TestSessionJournalLeaseHandoverFencesStaleWriter is the Task 6.2 handover boundary
// proof. Owner A acquires a lease (epoch N), constructs a journal — whose FIRST append
// is its LeaseFence{N} (seq 1) — and appends a loop event (seq 2). A's TTL is then
// expired (injected clock), so a second Acquire yields epoch N+1; owner B constructs a
// journal whose FIRST append is LeaseFence{N+1}, which ADVANCES the stream (seq 3)
// before B does any loop work. The handover is now complete and:
//
//   - B's opening LeaseFence really is its first append and is acked (its journal
//     constructed) before B appends a loop record.
//   - A is stale: its lease is lost (B took a higher epoch), so A's Append fails fast
//     with a typed JournalLeaseLostError — and the HARD backstop holds regardless: a
//     publish on A's stale tip, even to a DIFFERENT loop subject than anyone wrote,
//     fails the stream's expected-last-sequence fence (the 4.4 stream-level assertion).
//   - B proceeds normally, appending after its fence.
func TestSessionJournalLeaseHandoverFencesStaleWriter(t *testing.T) {
	sid := seedUUID(0x50)
	loopA := seedUUID(0x51)
	loopB := seedUUID(0x52)
	loopC := seedUUID(0x55) // a third, never-written subject: proves the fence is stream-level

	_, js := newEmbeddedJS(t)

	// An injected clock so A's TTL expiry (and thus B's takeover at N+1) is
	// deterministic — no wall-clock waiting.
	var (
		mu  sync.Mutex
		clk = time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clk
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		clk = clk.Add(d)
	}
	lm, err := journal.NewLeaseManager(js, journal.WithLeaseTTL(200*time.Millisecond), journal.WithLeaseClock(now))
	if err != nil {
		t.Fatalf("NewLeaseManager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// --- Owner A: acquire (epoch N), construct (opening LeaseFence{N} at seq 1), append.
	leaseA, err := lm.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	epochN := leaseA.Epoch()
	a, err := journal.NewSessionJournal(js, sid, leaseA)
	if err != nil {
		t.Fatalf("NewSessionJournal(A): %v", err)
	}
	assertOpeningFence(t, js, sid, 1, epochN)

	aEvent := event.LoopStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: loopA}, EventID: seedUUID(0x53)}}
	seqA, err := a.Append(ctx, journal.NewEventRecord(aEvent))
	if err != nil {
		t.Fatalf("A.Append: %v", err)
	}
	if seqA != 2 {
		t.Fatalf("A.Append seq = %d, want 2 (after A's opening LeaseFence at seq 1)", seqA)
	}

	// --- Expire A's TTL so B can take over at a higher epoch.
	advance(time.Second) // >> the 200ms lease TTL
	leaseB, err := lm.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire(B) after A expiry: %v", err)
	}
	if leaseB.Epoch() <= epochN {
		t.Fatalf("B epoch = %d, want > A epoch %d (monotonic handover)", leaseB.Epoch(), epochN)
	}
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		_ = leaseB.Release(rctx)
	})

	// --- Owner B: construct. Its FIRST append is LeaseFence{N+1}, advancing the stream
	// to seq 3 — BEFORE B does any loop work. A successful construction is precisely the
	// "fence acked before loop start" guarantee: NewSessionJournal does not return until
	// the opening fence lands.
	b, err := journal.NewSessionJournal(js, sid, leaseB)
	if err != nil {
		t.Fatalf("NewSessionJournal(B): %v", err)
	}
	assertOpeningFence(t, js, sid, 3, leaseB.Epoch())

	// --- A is now stale. B took a higher epoch, so A's heartbeat renewal CAS-fails and
	// marks A's lease lost. The heartbeat ticks on real time (independent of the injected
	// clock), so wait (bounded) for A's loss to be observed before asserting the refusal.
	select {
	case <-leaseA.Lost():
	case <-time.After(5 * time.Second):
		t.Fatalf("A's lease never observed lost after B's takeover")
	}

	// A's Append now fails fast with the typed lease-lost refusal (the journal never
	// re-fetches/advances its expectedSeq).
	staleEvent := event.LoopStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: loopB}, EventID: seedUUID(0x54)}}
	seq, err := a.Append(ctx, journal.NewEventRecord(staleEvent))
	if err == nil {
		t.Fatalf("stale A.Append seq = %d, want a lease-lost refusal", seq)
	}
	if seq != 0 {
		t.Errorf("stale A.Append returned seq %d alongside error, want 0", seq)
	}
	var lostErr *journal.JournalLeaseLostError
	if !errors.As(err, &lostErr) {
		t.Fatalf("stale A.Append error %v is not *JournalLeaseLostError", err)
	}
	var leaseLost *journal.LeaseLostError
	if !errors.As(err, &leaseLost) {
		t.Errorf("stale A.Append error %v does not unwrap to *LeaseLostError", err)
	}

	// --- The HARD backstop (Task 4.4 stream-level fence): even bypassing the journal's
	// fast-path lease guard, a publish on A's stale tip (seq 2) — to loopC, a subject
	// NOBODY has written, so a subject-level fence could not catch it — is rejected by
	// the stream's expected-last-sequence check, because B's LeaseFence advanced the tip
	// to 3. This is the guarantee that backs the lease loss: a stale journal physically
	// cannot extend the stream once a higher-epoch owner has fenced.
	staleTip := uint64(2) // A's expectedSeq before the handover
	foreign := &nats.Msg{
		Subject: journal.LoopEventSubject(sid, loopC),
		Header:  nats.Header{nats.MsgIdHdr: []string{seedUUID(0x56).String()}},
		Data:    []byte("stale-A-on-an-unwritten-subject"),
	}
	_, perr := js.PublishMsg(foreign, nats.Context(ctx), nats.ExpectLastSequence(staleTip))
	if perr == nil {
		t.Fatalf("publish on A's stale tip succeeded; the stream fence must reject it")
	}
	var apiErr *nats.APIError
	if !errors.As(perr, &apiErr) {
		t.Fatalf("stale publish error %v is not *nats.APIError (the wrong-last-seq fence)", perr)
	}
	if apiErr.ErrorCode != nats.JSErrCodeStreamWrongLastSequence {
		t.Errorf("APIError.ErrorCode = %d, want %d (wrong last sequence)", apiErr.ErrorCode, nats.JSErrCodeStreamWrongLastSequence)
	}

	// --- B proceeds normally: an append after its opening fence lands at seq 4.
	bEvent := event.LoopStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: loopB}, EventID: seedUUID(0x57)}}
	seqB, err := b.Append(ctx, journal.NewEventRecord(bEvent))
	if err != nil {
		t.Fatalf("B.Append: %v", err)
	}
	if seqB != 4 {
		t.Errorf("B.Append seq = %d, want 4 (after B's opening LeaseFence at seq 3)", seqB)
	}

	// The durable log holds exactly: A's fence (1), A's event (2), B's fence (3), B's
	// event (4). The stale A publish never landed.
	info, err := js.StreamInfo(journal.StreamName(sid))
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if info.State.LastSeq != 4 || info.State.Msgs != 4 {
		t.Errorf("stream state LastSeq=%d Msgs=%d, want 4/4 (A.fence, A.evt, B.fence, B.evt)", info.State.LastSeq, info.State.Msgs)
	}
}

// assertOpeningFence reads the record at seq and asserts it is a LeaseFence on the
// session's fence subject carrying wantEpoch — the journal's ownership handshake.
func assertOpeningFence(t *testing.T, js nats.JetStreamContext, sid uuid.UUID, seq, wantEpoch uint64) {
	t.Helper()
	raw, err := js.GetMsg(journal.StreamName(sid), seq)
	if err != nil {
		t.Fatalf("GetMsg(opening fence seq %d): %v", seq, err)
	}
	if raw.Subject != journal.FenceSubject(sid) {
		t.Errorf("opening fence (seq %d) subject = %q, want %q", seq, raw.Subject, journal.FenceSubject(sid))
	}
	fence, err := journal.UnmarshalLeaseFence(raw.Data)
	if err != nil {
		t.Fatalf("UnmarshalLeaseFence(seq %d): %v", seq, err)
	}
	if fence.Epoch != wantEpoch {
		t.Errorf("opening fence (seq %d) epoch = %d, want %d", seq, fence.Epoch, wantEpoch)
	}
}

// assertRoundTrip decodes data via the codec named by tc.kind and asserts the
// decoded value is deep-equal to the value tc carried into the append.
func assertRoundTrip(t *testing.T, tc appendCase, data []byte) {
	t.Helper()
	switch tc.kind {
	case kindEvent:
		got, err := event.UnmarshalEvent(data)
		if err != nil {
			t.Fatalf("UnmarshalEvent: %v", err)
		}
		if !reflect.DeepEqual(got, tc.wantEvent) {
			t.Errorf("decoded event = %#v, want %#v", got, tc.wantEvent)
		}
	case kindCommand:
		got, err := command.UnmarshalCommand(data)
		if err != nil {
			t.Fatalf("UnmarshalCommand: %v", err)
		}
		if !reflect.DeepEqual(got, tc.wantCommand) {
			t.Errorf("decoded command = %#v, want %#v", got, tc.wantCommand)
		}
	case kindFence:
		got, err := journal.UnmarshalLeaseFence(data)
		if err != nil {
			t.Fatalf("UnmarshalLeaseFence: %v", err)
		}
		if !reflect.DeepEqual(got, tc.wantFence) {
			t.Errorf("decoded fence = %#v, want %#v", got, tc.wantFence)
		}
	default:
		t.Fatalf("unknown record kind %d", tc.kind)
	}
}
