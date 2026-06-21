//go:build integration

package journal_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/nats-io/nats.go"
)

// ambiguousResolveCase drives one resolve-branch of Append's ambiguous-ack handling
// through the injected publish seam (SwapPublish). The seam closure decides, per
// call, whether to actually store the record (via the real seam it captured) and
// what error to surface, so each of the four resolve branches is reproduced
// deterministically against a real embedded JetStream server.
//
// The four branches (design "Bounded append & ambiguous acks"):
//
//	(a) ambiguous, then the original DID land  → retry sees Duplicate==true, GetMsg(N+1)
//	    is ours              → advance, no duplicate stored.
//	(b) ambiguous, then the original DID NOT land → retry stores it (Duplicate==false)
//	                         → advance, stored once.
//	(c) ambiguous → retry hits a wrong-last-seq conflict, GetMsg(N+1) is OURS (the
//	    original landed via the dedup/edge path) → advance.
//	(d) ambiguous → retry hits a wrong-last-seq conflict, GetMsg(N+1) is FOREIGN (a
//	    different writer's record) → *FenceViolationError, expectedSeq NOT advanced.
func TestSessionJournalResolvesAmbiguousAck(t *testing.T) {
	type branch uint8
	const (
		branchOriginalLanded branch = iota
		branchOriginalLost
		branchConflictOurs
		branchConflictForeign
	)

	tests := []struct {
		name   string
		branch branch
	}{
		{name: "ambiguous then original already landed (dedup hit)", branch: branchOriginalLanded},
		{name: "ambiguous then original did not land (retry stores)", branch: branchOriginalLost},
		{name: "ambiguous then conflict and N+1 is ours", branch: branchConflictOurs},
		{name: "ambiguous then conflict and N+1 is foreign", branch: branchConflictForeign},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			sid := seedUUID(0x60)
			lid := seedUUID(0x61)
			_, js := newEmbeddedJS(t)

			j, err := journal.NewSessionJournal(js, sid, mustAcquireLease(t, js, sid))
			if err != nil {
				t.Fatalf("NewSessionJournal: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// The opening LeaseFence already occupies seq 1, so the record under test
			// lands at seq 2 (expectedSeq 1 → N+1 == 2).
			rec := journal.NewEventRecord(event.LoopStarted{
				Header: event.Header{
					Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
					EventID:     seedUUID(0x62),
				},
			})
			wantSubject := journal.LoopEventSubject(sid, lid)
			wantMsgID := seedUUID(0x62).String()

			// real is the genuine fence-and-store seam captured before swapping; the
			// override calls it to land records out-of-band exactly as production would.
			real := journal.SwapPublish(j, nil)

			switch tt.branch {
			case branchOriginalLanded:
				// First call: actually store the record (the original landed) THEN report
				// ambiguity. The retry reports a Duplicate ack — the server saw the
				// Nats-Msg-Id already (a dedup hit) — WITHOUT a fence rejection. (We
				// synthesize the Duplicate ack directly: with the real fenced seam, a
				// republish whose original already advanced the tip is rejected by the
				// expected-last-sequence check BEFORE dedup, so this exact arm is reached
				// only when the retry is a dedup hit rather than a sequence conflict — e.g.
				// the no-fence / first-publish edge. This drives that arm deterministically.)
				// resolve must then GetMsg(N+1), confirm it is ours, and advance. No
				// duplicate is ever stored.
				calls := 0
				journal.SwapPublish(j, func(ctx context.Context, msg *nats.Msg) (*nats.PubAck, error) {
					calls++
					switch calls {
					case 1: // original: store it, then report ambiguity
						if _, perr := real(ctx, msg); perr != nil {
							t.Fatalf("seam store (call 1): %v", perr)
						}
						return nil, nats.ErrTimeout
					case 2: // retry: server reports a dedup hit (Duplicate ack)
						return &nats.PubAck{Sequence: 2, Duplicate: true}, nil
					default: // the follow-up append uses the real fenced seam
						return real(ctx, msg)
					}
				})

			case branchOriginalLost:
				// First call: DO NOT store; just report ambiguity. The retry actually
				// stores (Duplicate==false) → advance.
				calls := 0
				journal.SwapPublish(j, func(ctx context.Context, msg *nats.Msg) (*nats.PubAck, error) {
					calls++
					if calls == 1 {
						return nil, nats.ErrTimeout
					}
					return real(ctx, msg) // retry actually lands the record
				})

			case branchConflictOurs:
				// First call: store the record via the REAL fenced seam (the original
				// landed at N+1), then report ambiguity. The retry runs the real seam
				// again under expected-last-seq N — but the tip already moved to N+1 with
				// OUR record, and JetStream checks the sequence fence BEFORE dedup, so the
				// retry gets a genuine wrong-last-seq conflict (not a Duplicate ack).
				// resolve GetMsg(N+1) sees it is ours → advance. This is the path the
				// "original landed" outcome ACTUALLY takes in production with a fenced
				// retry.
				calls := 0
				journal.SwapPublish(j, func(ctx context.Context, msg *nats.Msg) (*nats.PubAck, error) {
					calls++
					if calls == 1 {
						if _, perr := real(ctx, msg); perr != nil {
							t.Fatalf("seam store (call 1): %v", perr)
						}
						return nil, nats.ErrTimeout
					}
					// Retry under expected-last-seq N: tip is already N+1 (ours) →
					// genuine wrong-last-seq conflict from the real seam.
					return real(ctx, msg)
				})

			case branchConflictForeign:
				// First call: report ambiguity WITHOUT storing our record, but land a
				// FOREIGN record at N+1 first (a different writer broke single-writer).
				// The retry (expected last seq N) then conflicts; resolve GetMsg(N+1)
				// finds a foreign Nats-Msg-Id → *FenceViolationError.
				foreign := &nats.Msg{
					Subject: journal.LoopEventSubject(sid, seedUUID(0x6F)),
					Header:  nats.Header{nats.MsgIdHdr: []string{seedUUID(0x6E).String()}},
					Data:    []byte("foreign"),
				}
				calls := 0
				journal.SwapPublish(j, func(ctx context.Context, msg *nats.Msg) (*nats.PubAck, error) {
					calls++
					if calls == 1 {
						// Land a foreign record at seq 2 with expected-last-seq 1 (the tip is
						// the opening LeaseFence at seq 1).
						if _, perr := js.PublishMsg(foreign, nats.Context(ctx), nats.ExpectLastSequence(1)); perr != nil {
							t.Fatalf("seam foreign store (call 1): %v", perr)
						}
						return nil, nats.ErrTimeout
					}
					// Retry under expected-last-seq 1: tip is now 2 (foreign) → conflict.
					return real(ctx, msg)
				})
			}

			seq, err := j.Append(ctx, rec)

			if tt.branch == branchConflictForeign {
				if err == nil {
					t.Fatalf("Append seq = %d, want *FenceViolationError (foreign N+1)", seq)
				}
				if seq != 0 {
					t.Errorf("Append returned seq %d alongside error, want 0", seq)
				}
				var fve *journal.FenceViolationError
				if !errors.As(err, &fve) {
					t.Fatalf("Append error %v is not *FenceViolationError", err)
				}
				if fve.Sequence != 2 {
					t.Errorf("FenceViolationError.Sequence = %d, want 2 (the contested N+1)", fve.Sequence)
				}
				if fve.WantMsgID != wantMsgID {
					t.Errorf("FenceViolationError.WantMsgID = %q, want %q (our record)", fve.WantMsgID, wantMsgID)
				}
				if fve.GotMsgID == wantMsgID || fve.GotMsgID == "" {
					t.Errorf("FenceViolationError.GotMsgID = %q, want a foreign non-empty id", fve.GotMsgID)
				}
				// expectedSeq must NOT have advanced: a follow-up append still fences on 0.
				_, again := j.Append(ctx, rec)
				if again == nil {
					t.Fatalf("after a fence violation a follow-up Append succeeded; expectedSeq must not advance")
				}
				return
			}

			// Branches (a)/(b)/(c) all resolve to: the record committed exactly once at
			// seq 2 (after the opening LeaseFence at seq 1), and the journal advanced to it.
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			if seq != 2 {
				t.Fatalf("Append seq = %d, want 2", seq)
			}

			info, err := js.StreamInfo(journal.StreamName(sid))
			if err != nil {
				t.Fatalf("StreamInfo: %v", err)
			}
			if info.State.LastSeq != 2 {
				t.Errorf("StreamInfo LastSeq = %d, want 2 (LeaseFence + record committed once)", info.State.LastSeq)
			}
			if info.State.Msgs != 2 {
				t.Errorf("StreamInfo Msgs = %d, want 2 (LeaseFence + record; no duplicate under the dedup window)", info.State.Msgs)
			}

			raw, err := js.GetMsg(journal.StreamName(sid), 2)
			if err != nil {
				t.Fatalf("GetMsg(2): %v", err)
			}
			if raw.Subject != wantSubject {
				t.Errorf("stored subject = %q, want %q", raw.Subject, wantSubject)
			}
			if got := raw.Header.Get(nats.MsgIdHdr); got != wantMsgID {
				t.Errorf("stored Nats-Msg-Id = %q, want %q", got, wantMsgID)
			}

			// The journal advanced exactly to seq 2: the next append fences on 2 and
			// lands at 3 (no double-advance, no stuck fence).
			next := journal.NewEventRecord(event.LoopIdle{
				Header: event.Header{
					Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
					EventID:     seedUUID(0x63),
				},
			})
			nseq, err := j.Append(ctx, next)
			if err != nil {
				t.Fatalf("follow-up Append: %v", err)
			}
			if nseq != 3 {
				t.Errorf("follow-up Append seq = %d, want 3 (journal advanced to 2)", nseq)
			}
		})
	}
}

// TestSessionJournalAmbiguousTwiceIsUnresolved asserts the resolve is BOUNDED: if the
// retry is itself ambiguous (a second timeout), Append does not loop forever — it
// returns a typed unresolved error and leaves expectedSeq unadvanced.
func TestSessionJournalAmbiguousTwiceIsUnresolved(t *testing.T) {
	sid := seedUUID(0x70)
	lid := seedUUID(0x71)
	_, js := newEmbeddedJS(t)

	j, err := journal.NewSessionJournal(js, sid, mustAcquireLease(t, js, sid))
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rec := journal.NewEventRecord(event.LoopStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
			EventID:     seedUUID(0x72),
		},
	})

	// Capture the real seam so it can be restored after the unresolved attempt.
	real := journal.SwapPublish(j, nil)

	// Every call is ambiguous: the original publish AND its retry both time out.
	calls := 0
	journal.SwapPublish(j, func(ctx context.Context, msg *nats.Msg) (*nats.PubAck, error) {
		calls++
		return nil, nats.ErrTimeout
	})

	seq, err := j.Append(ctx, rec)
	if err == nil {
		t.Fatalf("Append seq = %d, want an unresolved ambiguous error", seq)
	}
	if seq != 0 {
		t.Errorf("Append returned seq %d alongside error, want 0", seq)
	}
	var ambErr *journal.AmbiguousAckError
	if !errors.As(err, &ambErr) {
		t.Fatalf("Append error %v is not *AmbiguousAckError", err)
	}
	// Bounded: exactly the original attempt plus one retry (no infinite loop).
	if calls != 2 {
		t.Errorf("publish seam called %d times, want 2 (original + one bounded retry)", calls)
	}

	// expectedSeq unadvanced: a follow-up append (now with the real seam) lands at 2
	// (the tip is the opening LeaseFence at seq 1; the unresolved attempt did not advance).
	journal.SwapPublish(j, real)
	nseq, err := j.Append(ctx, rec)
	if err != nil {
		t.Fatalf("follow-up Append after unresolved: %v", err)
	}
	if nseq != 2 {
		t.Errorf("follow-up Append seq = %d, want 2 (expectedSeq was not advanced past the LeaseFence)", nseq)
	}
}
