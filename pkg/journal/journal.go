package journal

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/uuid"
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
	js        nats.JetStreamContext
	stream    string    // the bound stream name (StreamName(sessionID))
	sessionID uuid.UUID // the session this journal owns (for fence record + error context)

	// lease is the single-writer ownership token (DIP: injected, never acquired here).
	// Its Epoch is stamped into the opening LeaseFence; Append refuses once the lease
	// is lost (a higher-epoch owner took over). The journal depends only on the narrow
	// ownershipToken view — never Release — so it cannot tamper with the lifecycle.
	lease ownershipToken

	// objects is the per-session object store an over-threshold record's marshaled
	// bytes are offloaded to (content-addressed) before its pointer is appended. It is
	// bound once at construction (ensureObjectStore) and used only inside the serialized
	// publish path, so the upload-before-append ordering holds under mu.
	objects nats.ObjectStore

	// appendTimeout is the per-append publish deadline (defaultAppendTimeout unless
	// overridden via WithAppendTimeout).
	appendTimeout time.Duration

	// publish is the publish seam. It defaults to a closure over js.PublishMsg that
	// applies the expected-last-sequence fence (read from expectedSeq at call time)
	// and the message's Nats-Msg-Id; tests override it via SwapPublish to inject
	// ambiguous/conflict outcomes. Called only under mu, so its read of expectedSeq
	// is serialized with the advance.
	publish publishFunc

	// mu serializes Append and guards expectedSeq and ready. The serializer is
	// single-writer by contract; the mutex makes that safe even if a caller fans
	// Append across goroutines.
	mu sync.Mutex
	// ready is set true only after the opening LeaseFence has been acknowledged (the
	// journal has taken ownership of the stream tip). Append refuses with a typed
	// JournalNotReadyError until then so no record ever precedes the ownership fence.
	ready bool
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

// writeOpeningFence is the journal's ownership handshake: it appends a
// LeaseFence{Epoch: lease.Epoch()} on the session's fence subject as the journal's
// very first record, fenced on the current tip like any append. It runs once at
// construction, bypassing the ready gate (it is what SETS ready) but otherwise using
// the identical serialized publish path — so a stale prior owner (or a higher-epoch
// successor) that advanced the stream causes the expected-sequence fence to reject
// this fence and construction to fail closed. On success the journal is marked ready.
func (j *streamJournal) writeOpeningFence(ctx context.Context) error {
	fence := NewFenceRecord(j.sessionID, LeaseFence{Epoch: j.lease.Epoch()})

	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.appendLocked(ctx, fence); err != nil {
		return err
	}
	j.ready = true
	return nil
}

// Append marshals rec via its payload codec, publishes it to rec.Subject() with the
// record's IdempotencyID() as the Nats-Msg-Id and expectedSeq as the
// stream-level Nats-Expected-Last-Sequence fence, then advances expectedSeq to the
// assigned sequence on success. It first guards two ownership invariants under mu:
// the journal must be ready (its opening LeaseFence acknowledged) and its lease must
// still be held. Once the lease is lost it refuses every append fast and never
// re-fetches or advances expectedSeq — the stream fence is the hard backstop, this is
// the fast-path guard. The whole operation holds mu so the guard, publish, and
// expectedSeq advance are one atomic step; the publish carries its own deadline.
func (j *streamJournal) Append(ctx context.Context, rec JournalRecord) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if !j.ready {
		return 0, &JournalNotReadyError{SessionID: j.sessionID}
	}
	if !j.leaseHeld() {
		return 0, &JournalLeaseLostError{SessionID: j.sessionID, Epoch: j.lease.Epoch()}
	}
	return j.appendLocked(ctx, rec)
}

// leaseHeld reports whether the ownership lease is still held: both the validity flag
// and the loss channel must say so. It is the fast-path ownership guard; the stream's
// expected-sequence fence is the hard backstop that catches a loss this guard races.
func (j *streamJournal) leaseHeld() bool {
	if !j.lease.Valid() {
		return false
	}
	select {
	case <-j.lease.Lost():
		return false
	default:
		return true
	}
}

// appendLocked is the serialized publish core, factored out so both the public Append
// (after its ready/lease guard) and the construction-time writeOpeningFence share one
// code path. The caller MUST hold mu.
func (j *streamJournal) appendLocked(ctx context.Context, rec JournalRecord) (uint64, error) {
	payload, err := marshalRecord(rec)
	if err != nil {
		return 0, err
	}

	// Per-append deadline independent of the session context: a derived child of
	// ctx so the caller can still cancel earlier, but capped so one stuck publish
	// cannot hold mu (and thus every queued Append) indefinitely.
	pubCtx, cancel := context.WithTimeout(ctx, j.appendTimeout)
	defer cancel()

	// Build the message to publish. A record at or below inlineThreshold is published
	// inline exactly as before. An over-threshold record is content-addressed and its
	// marshaled bytes are offloaded to the object store BEFORE the pointer is appended
	// (upload-before-append: no dangling reference), and the published message becomes
	// the small pointer carrying the SAME Subject and SAME Nats-Msg-Id — so the fence,
	// dedup, and ambiguous-ack reconciliation below are byte-identical to the inline
	// path; only the body the server stores differs. The upload runs under mu (this
	// whole method holds it), preserving the single-writer ordering. The pointer body
	// (not the original payload) is what later reconcileTip compares against the tip.
	msg, payload, err := j.buildMessage(pubCtx, rec, payload)
	if err != nil {
		return 0, err
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

// buildMessage decides inline-vs-offload for a marshaled record and returns the message
// to publish together with the bytes that will actually be STORED in the stream (so the
// caller's ambiguous-ack reconcileTip compares the tip against the right body). Called
// only under mu, so the over-threshold upload is serialized with the publish that
// references it.
//
//   - payload <= inlineThreshold: the message body is payload itself (the unchanged
//     inline path); the stored bytes are payload.
//   - payload > inlineThreshold: the marshaled bytes are uploaded to the object store
//     under their content-addressed id BEFORE returning, and the message body becomes
//     the small pointer record (same Subject, same Nats-Msg-Id); the stored bytes are
//     that pointer body. A failed upload returns *RecordTooLargeError — the record is
//     never silently inlined.
func (j *streamJournal) buildMessage(ctx context.Context, rec JournalRecord, payload []byte) (*nats.Msg, []byte, error) {
	if len(payload) <= inlineThreshold {
		msg := &nats.Msg{
			Subject: rec.Subject(),
			Header:  nats.Header{nats.MsgIdHdr: []string{rec.IdempotencyID()}},
			Data:    payload,
		}
		return msg, payload, nil
	}
	msg, err := buildOffloadMessage(ctx, j.objects, rec, payload)
	if err != nil {
		return nil, nil, err
	}
	return msg, msg.Data, nil
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
