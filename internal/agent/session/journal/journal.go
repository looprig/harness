package journal

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/nats-io/nats.go"
)

// SessionJournal is the single serialized writer for one session's durable
// stream. Append encodes a JournalRecord's payload, publishes it to the record's
// JetStream subject under stream-level expected-sequence fencing, and returns the
// assigned stream sequence. It is the only thing that writes a session's stream;
// callers funnel every event, command, and fence through it so the stream stays a
// totally-ordered, gap-free log.
//
// The interface is intentionally narrow (one method): a caller that only needs to
// persist a record must not depend on stream-management surface. The concrete
// *streamJournal in this package satisfies it; NewSessionJournal is the
// composition-time constructor.
type SessionJournal interface {
	// Append serializes rec, publishes it under the next expected sequence, and
	// returns the assigned stream sequence. ctx bounds the caller's willingness to
	// wait; the publish additionally carries a per-append deadline independent of
	// ctx so one stuck call cannot wedge the serialized writer forever. Appends are
	// totally ordered: the returned sequences are strictly monotonic across calls.
	Append(ctx context.Context, rec JournalRecord) (seq uint64, err error)
}

// defaultAppendTimeout bounds a single Append's publish round-trip independent of
// the caller's context. The journal holds its mutex across the publish, so an
// unbounded publish would wedge every queued Append behind it; this per-append
// deadline fails the one stuck call fast and keeps the serialized writer live.
const defaultAppendTimeout = 5 * time.Second

// publishFunc is the journal's publish seam: it takes the per-append context and the
// fully-formed message and returns the JetStream ack. The default closure (set in the
// constructor) carries the stream-level expected-last-sequence fence and the
// Nats-Msg-Id wiring; tests swap it to drive the ambiguous-ack resolve branches
// deterministically. It is unexported and never appears in the public API.
type publishFunc func(ctx context.Context, msg *nats.Msg) (*nats.PubAck, error)

// streamJournal is the concrete single-writer serializer. It binds one session's
// stream and serializes every Append behind mu, advancing expectedSeq from each
// publish ack so the next publish fences on the exact prior sequence. A foreign
// goroutine cannot race the sequence: mu guards the whole publish-and-advance.
type streamJournal struct {
	js     nats.JetStreamContext
	stream string // the bound stream name (StreamName(sessionID))

	// appendTimeout is the per-append publish deadline (defaultAppendTimeout unless
	// overridden via WithAppendTimeout).
	appendTimeout time.Duration

	// publish is the publish seam. It defaults to a closure over js.PublishMsg that
	// applies the expected-last-sequence fence (read from expectedSeq at call time)
	// and the message's Nats-Msg-Id; tests override it via SwapPublish to inject
	// ambiguous/conflict outcomes. Called only under mu, so its read of expectedSeq
	// is serialized with the advance.
	publish publishFunc

	// mu serializes Append and guards expectedSeq. The serializer is single-writer
	// by contract; the mutex makes that safe even if a caller fans Append across
	// goroutines.
	mu sync.Mutex
	// expectedSeq is the stream sequence the next publish must fence on: the last
	// successfully published sequence (0 for a fresh stream). Publishing with
	// Nats-Expected-Last-Sequence == expectedSeq rejects any write that does not
	// extend exactly this tip, so a stale writer (Task 4.4) cannot interleave.
	expectedSeq uint64
}

// defaultPublish is the production publish seam: it publishes msg under the journal's
// current expected-last-sequence fence so a stale writer is rejected by the stream,
// not merely by subject. expectedSeq is read at call time (under mu), so a resolve
// retry — which does not advance expectedSeq on the failed first attempt — re-fences
// on the same tip. The Nats-Msg-Id is already on msg.Header (set by Append), so a
// republish of the same record dedups within the stream's dedup window.
func (j *streamJournal) defaultPublish(ctx context.Context, msg *nats.Msg) (*nats.PubAck, error) {
	return j.js.PublishMsg(msg,
		nats.Context(ctx),
		nats.ExpectLastSequence(j.expectedSeq),
	)
}

// Append marshals rec via its payload codec, publishes it to rec.Subject() with the
// record's IdempotencyID() as the Nats-Msg-Id and expectedSeq as the
// stream-level Nats-Expected-Last-Sequence fence, then advances expectedSeq to the
// assigned sequence on success. The whole operation holds mu so the publish and the
// expectedSeq advance are one atomic step; the publish carries its own deadline.
func (j *streamJournal) Append(ctx context.Context, rec JournalRecord) (uint64, error) {
	payload, err := marshalRecord(rec)
	if err != nil {
		return 0, err
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	// Per-append deadline independent of the session context: a derived child of
	// ctx so the caller can still cancel earlier, but capped so one stuck publish
	// cannot hold mu (and thus every queued Append) indefinitely.
	pubCtx, cancel := context.WithTimeout(ctx, j.appendTimeout)
	defer cancel()

	msg := &nats.Msg{
		Subject: rec.Subject(),
		Header:  nats.Header{nats.MsgIdHdr: []string{rec.IdempotencyID()}},
		Data:    payload,
	}

	// expected is the tip the publish (and any resolve retry) fences on. Capture it
	// before publishing: the seam reads j.expectedSeq, which stays N on a failed
	// attempt, so retry and verification both reason about the SAME N.
	expected := j.expectedSeq
	ack, err := j.publish(pubCtx, msg)
	if err == nil {
		// Definite success. A Duplicate ack on a FIRST publish means a same-Msg-Id
		// record already sits in the dedup window (e.g. a prior process's append we
		// are unaware of); reconcile it against the tip rather than trusting the ack's
		// sequence blindly — the same verification the resolve path uses.
		if ack.Duplicate {
			return j.reconcileTip(pubCtx, rec, payload, expected)
		}
		j.expectedSeq = ack.Sequence
		return ack.Sequence, nil
	}

	// Definite wrong-last-sequence: the stream advanced past expected, so the fence
	// rejected this writer (Task 4.4 — the stale/fenced case). This is NOT ambiguous:
	// the record did not land. Return the typed AppendError, leave expectedSeq
	// unadvanced, do not retry.
	if isWrongLastSequence(err) {
		return 0, &AppendError{Subject: rec.Subject(), MsgID: rec.IdempotencyID(), Expected: expected, Cause: err}
	}

	// Ambiguous: a timeout / deadline / no-response. The ack was lost; the record may
	// or may not be stored. Resolve by retry-then-verify (design "Bounded append &
	// ambiguous acks"). expectedSeq stays unadvanced unless resolve commits.
	if isAmbiguous(err) {
		return j.resolveAmbiguous(pubCtx, rec, msg, payload, expected, err)
	}

	// Any other publish error (e.g. a non-fence API error, a transport failure that
	// is not classed ambiguous) is a definite failure: fail closed, unadvanced.
	return 0, &AppendError{Subject: rec.Subject(), MsgID: rec.IdempotencyID(), Expected: expected, Cause: err}
}

// resolveAmbiguous resolves a lost-ack append whose original outcome is unknown. It
// runs under mu (Append holds it). With the original expected sequence N, the record
// — if it landed — sits at N+1. The algorithm (design): retry the SAME record (same
// Nats-Msg-Id, same ExpectLastSequence(N)) and branch on the retry's response:
//
//   - retry success, Duplicate==true  → the original landed (dedup hit); verify the
//     tip at N+1 is ours and advance to N+1.
//   - retry success, Duplicate==false → the retry is what landed; advance to its seq.
//   - retry wrong-last-seq conflict   → something already sits at N+1; verify the tip
//     at N+1: ours → the original landed, advance to N+1; foreign →
//     *FenceViolationError (single-writer broken).
//   - retry ambiguous again           → unresolved; return *AmbiguousAckError. Bounded
//     to a single retry so resolve never loops forever.
//
// On every path that does not commit, expectedSeq is left unadvanced.
func (j *streamJournal) resolveAmbiguous(ctx context.Context, rec JournalRecord, msg *nats.Msg, payload []byte, expected uint64, cause error) (uint64, error) {
	ack, err := j.publish(ctx, msg)
	if err == nil {
		if ack.Duplicate {
			// Original landed (dedup). Verify the tip is ours, then advance.
			return j.reconcileTip(ctx, rec, payload, expected)
		}
		// The retry itself landed. Advance to whatever sequence it acked.
		j.expectedSeq = ack.Sequence
		return ack.Sequence, nil
	}

	if isWrongLastSequence(err) {
		// Something is already at N+1. Compare it to ours: ours → the original landed
		// (commit it); foreign → the single-writer invariant is broken.
		return j.reconcileTip(ctx, rec, payload, expected)
	}

	if isAmbiguous(err) {
		// A second ambiguous outcome during resolve. Bounded — do not loop: surface a
		// typed unresolved error and leave expectedSeq unadvanced.
		return 0, &AmbiguousAckError{Subject: rec.Subject(), MsgID: rec.IdempotencyID(), Expected: expected, Cause: cause}
	}

	// A definite non-fence, non-ambiguous error on the retry: fail closed.
	return 0, &AppendError{Subject: rec.Subject(), MsgID: rec.IdempotencyID(), Expected: expected, Cause: err}
}

// reconcileTip reads the stream record at the contested sequence N+1 and decides
// whether it is the record we were publishing. It is the shared verification for the
// "duplicate / already-there" branches: a same-Msg-Id record at N+1 means our append
// committed (advance expectedSeq := N+1, return N+1); a foreign Nats-Msg-Id there is a
// single-writer violation (*FenceViolationError). A read failure other than
// not-found fails closed (*ResolveReadError); a not-found at N+1 (nothing landed where
// we expected) is itself a fence violation — we believed the tip was N+1 but the slot
// is empty, so the durable picture is inconsistent with our expectation.
func (j *streamJournal) reconcileTip(ctx context.Context, rec JournalRecord, payload []byte, expected uint64) (uint64, error) {
	tip := expected + 1
	wantMsgID := rec.IdempotencyID()

	raw, err := j.js.GetMsg(j.stream, tip, nats.Context(ctx))
	if err != nil {
		if errors.Is(err, nats.ErrMsgNotFound) {
			// Nothing at N+1: our record is not where it must be. Treat as a fence
			// violation rather than silently re-appending (which could duplicate).
			return 0, &FenceViolationError{Subject: rec.Subject(), Sequence: tip, WantMsgID: wantMsgID, GotMsgID: ""}
		}
		return 0, &ResolveReadError{Subject: rec.Subject(), Sequence: tip, Cause: err}
	}

	gotMsgID := raw.Header.Get(nats.MsgIdHdr)
	if gotMsgID != wantMsgID || !bytes.Equal(raw.Data, payload) {
		// Foreign record (or our id with a different body) at the slot we fenced on:
		// some other writer advanced the stream. Single-writer invariant broken.
		return 0, &FenceViolationError{Subject: rec.Subject(), Sequence: tip, WantMsgID: wantMsgID, GotMsgID: gotMsgID}
	}

	// Ours. The append committed at N+1; advance the fence to it.
	j.expectedSeq = tip
	return tip, nil
}

// isWrongLastSequence reports whether err is the JetStream stream-level
// expected-last-sequence rejection (a definite fence: the stream advanced past the
// expected tip, so the record did NOT land). It is a *nats.APIError carrying the
// wrong-last-sequence error code; matching on the typed code (not the message string)
// keeps the classification robust against description changes.
func isWrongLastSequence(err error) bool {
	var apiErr *nats.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == nats.JSErrCodeStreamWrongLastSequence
}

// isAmbiguous reports whether err means the publish ack was LOST — a deadline, a NATS
// timeout, or no response from the stream — so the record may or may not be stored.
// These are the only outcomes that warrant retry-then-verify; a wrong-last-sequence
// (definite reject) is explicitly NOT ambiguous and is classified first.
func isAmbiguous(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrNoStreamResponse)
}

// marshalRecord encodes a record's payload via the codec for its concrete kind:
// an event via event.MarshalEvent (fails closed on an Ephemeral event), a command
// via command.MarshalCommand, a fence via MarshalLeaseFence. The switch is over the
// sealed JournalRecord sum, so the default arm is unreachable for an in-package
// record; it fails closed with a typed RecordKindError rather than panicking if a
// foreign type ever satisfies the marker.
func marshalRecord(rec JournalRecord) ([]byte, error) {
	switch r := rec.(type) {
	case EventRecord:
		payload, err := event.MarshalEvent(r.Event())
		if err != nil {
			return nil, &MarshalRecordError{Subject: r.Subject(), Cause: err}
		}
		return payload, nil
	case CommandRecord:
		payload, err := command.MarshalCommand(r.Command())
		if err != nil {
			return nil, &MarshalRecordError{Subject: r.Subject(), Cause: err}
		}
		return payload, nil
	case FenceRecord:
		payload, err := MarshalLeaseFence(r.Fence())
		if err != nil {
			return nil, &MarshalRecordError{Subject: r.Subject(), Cause: err}
		}
		return payload, nil
	default:
		return nil, &RecordKindError{Subject: rec.Subject()}
	}
}
