package sessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/uuid"
	"github.com/looprig/storekit"
)

// appendTimeout bounds a single Append's ledger round-trip (offload upload plus
// the AppendDefinite) independent of the caller's context. The writer holds its
// mutex across the whole operation, so an unbounded backend call would wedge every
// queued Append behind it; this per-append deadline fails one stuck call fast and
// keeps the serialized writer live. The value matches the journal's historical
// per-append publish deadline, carried over to the storekit-backed writer.
const appendTimeout = 5 * time.Second

// blobsInfix is the name segment separating a session's ledger prefix from its
// content-addressed offload blobs: a blob lands at "sessions/<uuid>/blobs/<sha>".
const blobsInfix = "/blobs/"

// NilLeaseError reports that a Store constructor (OpenJournal or OpenObjectGC) was
// handed a nil lease. The lease is a required dependency (DIP): the composition root
// acquires it via AcquireLease and passes it in. The constructor fails closed with
// this typed error rather than deferring a nil dereference to first use (stamping the
// epoch into the opening fence, or the GC lease guard).
type NilLeaseError struct {
	SessionID uuid.UUID
}

func (e *NilLeaseError) Error() string {
	return "sessionstore: session " + e.SessionID.String() + ": nil lease"
}

// sessionJournal is the concrete single-writer serializer over a storekit ledger:
// it frames each JournalRecord as a versioned envelope, offloads an over-threshold
// frame to Blobs before appending a small pointer in its place, and commits under
// CAS fencing on the tracked tip. It is the sessionstore port of the NATS
// journal's WRITE semantics onto storekit — storekit.AppendDefinite owns the
// ambiguous-ack / conflict resolution the old journal did by hand.
type sessionJournal struct {
	id        uuid.UUID       // the session this journal owns (for fence + error context)
	lease     journal.Lease   // single-writer ownership token (injected; never acquired here)
	ledger    storekit.Ledger // the append-only record log this journal is the sole writer of
	blobs     storekit.Blobs  // content-addressed offload store for over-threshold frames
	name      string          // the bound ledger name (ledgerName(id))
	threshold int             // frame size (bytes) above which a record is offloaded

	// mu serializes Append and guards ready + trackedTip. The serializer is
	// single-writer by contract; the mutex makes that safe even if a caller fans
	// Append across goroutines.
	mu sync.Mutex
	// ready is set true only after the opening fence has committed (the journal has
	// taken ownership of the tip). Append refuses with a typed JournalNotReadyError
	// until then so no record ever precedes the ownership fence.
	ready bool
	// trackedTip is the ledger sequence the next append must fence on: the last
	// sequence this writer committed (the tip observed at Open, then each append's
	// new seq). A stale writer whose trackedTip is behind the real tip is rejected
	// by storekit's CAS on append.
	trackedTip uint64
}

// Compile-time proof that *sessionJournal honors the journal.SessionJournal contract.
var _ journal.SessionJournal = (*sessionJournal)(nil)

// OpenJournal binds a single-writer journal to session id's ledger and takes
// ownership of the tip by writing the opening fence — a fence-kind envelope
// carrying the lease epoch — as an append fenced on the ledger's current tip. That
// fence advances the tip, so any stale prior writer's next CAS append conflicts;
// only once it commits is the journal ready to accept Appends. The lease is a
// required dependency (DIP): a nil lease fails closed with *NilLeaseError.
func (s *Store) OpenJournal(ctx context.Context, id uuid.UUID, lease journal.Lease) (journal.SessionJournal, error) {
	if lease == nil {
		return nil, &NilLeaseError{SessionID: id}
	}
	name, err := sessionName(id)
	if err != nil {
		return nil, err
	}
	// Bound the tip read on the same per-append budget so a wedged backend cannot
	// block Open indefinitely (every I/O call carries a deadline).
	tipCtx, cancel := context.WithTimeout(ctx, appendTimeout)
	defer cancel()
	tip, err := s.backend.Ledger.Tip(tipCtx, name)
	if err != nil {
		return nil, err
	}

	j := &sessionJournal{
		id:         id,
		lease:      lease,
		ledger:     s.backend.Ledger,
		blobs:      s.backend.Blobs,
		name:       name,
		threshold:  s.opts.OffloadThreshold,
		trackedTip: tip,
	}

	// Take ownership: the first append is the opening fence, stamping the lease
	// epoch and fenced on the current tip. A stale prior owner (or higher-epoch
	// successor) that advanced the ledger causes this CAS to conflict and Open to
	// fail closed. Only once it commits is the journal ready.
	fence := journal.NewFenceRecord(id, journal.LeaseFence{Epoch: lease.Epoch()})
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.writeLocked(ctx, fence); err != nil {
		return nil, err
	}
	j.ready = true
	return j, nil
}

// Append serializes rec behind mu, refuses if the journal is not ready or its lease
// is lost, then frames, offloads-if-large, and commits rec under CAS on the tracked
// tip. The whole operation holds mu so the guard, offload, append, and tip advance
// are one atomic step; the append carries its own per-append deadline so one stuck
// call cannot wedge the queued writers. On success it returns the assigned ledger
// sequence; sequences are strictly monotonic across calls.
func (b *sessionJournal) Append(ctx context.Context, rec journal.JournalRecord) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.ready {
		return 0, &journal.JournalNotReadyError{SessionID: b.id}
	}
	if !b.leaseHeld() {
		return 0, &journal.JournalLeaseLostError{SessionID: b.id, Epoch: b.lease.Epoch()}
	}
	return b.writeLocked(ctx, rec)
}

// leaseHeld reports whether the ownership lease is still held: both its validity
// flag and its loss channel must say so. It is the fast-path ownership guard; the
// ledger's CAS fence is the hard backstop that catches a loss this guard races.
func (b *sessionJournal) leaseHeld() bool {
	if !b.lease.Valid() {
		return false
	}
	select {
	case <-b.lease.Lost():
		return false
	default:
		return true
	}
}

// writeLocked is the serialized write core shared by Append (after its ready/lease
// guard) and OpenJournal (the opening fence, which is what SETS ready). The caller
// MUST hold mu. It derives a per-append child context, frames rec (offloading an
// over-threshold frame to Blobs first), commits the resulting bytes under CAS on
// the tracked tip via storekit.AppendDefinite, and on success advances the tip and
// returns the new sequence. On any failure the tip is left unadvanced (fail closed).
func (b *sessionJournal) writeLocked(ctx context.Context, rec journal.JournalRecord) (uint64, error) {
	childCtx, cancel := context.WithTimeout(ctx, appendTimeout)
	defer cancel()

	recordBytes, err := b.frame(childCtx, rec)
	if err != nil {
		return 0, err
	}
	if err := storekit.AppendDefinite(childCtx, b.ledger, b.name, b.trackedTip, recordBytes); err != nil {
		return 0, b.mapAppendErr(rec, err)
	}
	b.trackedTip++
	return b.trackedTip, nil
}

// frame encodes rec into the bytes to append: its codec body wrapped in a versioned
// envelope, and — if that envelope exceeds the offload threshold — replaced by a
// small blobptr envelope whose real bytes were first written to Blobs
// (blob-durable-before-pointer). It runs under mu (writeLocked holds it), so the
// upload is serialized with the append that references it.
func (b *sessionJournal) frame(ctx context.Context, rec journal.JournalRecord) ([]byte, error) {
	k, body, err := b.encodeRecordBody(rec)
	if err != nil {
		return nil, err
	}
	env, err := encodeEnvelope(envelope{V: envelopeVersion, Kind: string(k), ID: rec.IdempotencyID(), Body: body})
	if err != nil {
		return nil, err
	}
	if len(env) <= b.threshold {
		return env, nil
	}
	return b.offload(ctx, rec, env)
}

// offload writes an over-threshold frame's full bytes to Blobs under their
// content-addressed key BEFORE returning the small blobptr envelope that stands in
// for it in the ledger — so a pointer can never reference a blob that is not yet
// durable. On any Blobs failure it fails closed with a typed *journal.RecordTooLargeError
// rather than inlining an oversized record (which would breach the 1 MiB ledger floor).
func (b *sessionJournal) offload(ctx context.Context, rec journal.JournalRecord, env []byte) ([]byte, error) {
	sum := sha256.Sum256(env)
	shahex := hex.EncodeToString(sum[:])
	// Reuse the already-validated ledger name as the blob-key prefix so the key can
	// never diverge from the session's canonical name derivation.
	key := b.name + blobsInfix + shahex

	// Blob durable first: no dangling pointer.
	if err := b.blobs.Put(ctx, key, bytes.NewReader(env)); err != nil {
		return nil, &journal.RecordTooLargeError{
			Subject: b.name,
			MsgID:   rec.IdempotencyID(),
			Length:  len(env),
			Cause:   err,
		}
	}

	ptr, err := encodeBlobPointer(blobPointer{Key: key, Size: int64(len(env)), SHA256: shahex})
	if err != nil {
		return nil, err
	}
	return encodeEnvelope(envelope{V: envelopeVersion, Kind: string(kindBlobPtr), ID: rec.IdempotencyID(), Body: ptr})
}

// mapAppendErr translates a storekit append failure into the journal's error
// vocabulary so callers keep classifying at the journal level: a definite CAS
// conflict (a stale writer fenced out) becomes *journal.AppendError; a still-
// ambiguous outcome becomes *journal.AmbiguousAckError; any other error is
// surfaced unchanged (fail closed). The tip is already left unadvanced by the caller.
func (b *sessionJournal) mapAppendErr(rec journal.JournalRecord, err error) error {
	var conflict *storekit.ConflictError
	if errors.As(err, &conflict) {
		return &journal.AppendError{Subject: b.name, MsgID: rec.IdempotencyID(), Expected: b.trackedTip, Cause: err}
	}
	var ambiguous *storekit.AmbiguousError
	if errors.As(err, &ambiguous) {
		return &journal.AmbiguousAckError{Subject: b.name, MsgID: rec.IdempotencyID(), Expected: b.trackedTip, Cause: err}
	}
	// AppendDefinite's verifyAppend could not read the contested tip to resolve a
	// conflict, so the append outcome is genuinely UNKNOWN — the same unresolved,
	// decide-fail-or-retry case as a lingering ambiguous ack. Map it to the journal's
	// AmbiguousAckError (carrying the verify error) rather than leaking storekit's
	// type through the facade.
	var verify *storekit.AppendVerifyError
	if errors.As(err, &verify) {
		return &journal.AmbiguousAckError{Subject: b.name, MsgID: rec.IdempotencyID(), Expected: b.trackedTip, Cause: err}
	}
	return err
}

// encodeRecordBody encodes a record's payload via the codec for its concrete kind
// and names the envelope kind that carries it: an event via event.MarshalEvent, a
// command via command.MarshalCommand, a fence via journal.MarshalLeaseFence. The
// switch is over the sealed JournalRecord sum, so the default arm is unreachable
// for an in-package record; it fails closed with a typed *journal.RecordKindError
// rather than panicking if a foreign type ever satisfies the marker. It additionally
// returns the envelope kind (JournalRecord exposes no Kind(); the concrete type is the
// source of truth). Error context uses the ledger name (b.name), the record's
// backend-neutral destination.
func (b *sessionJournal) encodeRecordBody(rec journal.JournalRecord) (kind, []byte, error) {
	switch r := rec.(type) {
	case journal.EventRecord:
		body, err := event.MarshalEvent(r.Event())
		if err != nil {
			return "", nil, &journal.MarshalRecordError{Subject: b.name, Cause: err}
		}
		return kindEvent, body, nil
	case journal.CommandRecord:
		body, err := command.MarshalCommand(r.Command())
		if err != nil {
			return "", nil, &journal.MarshalRecordError{Subject: b.name, Cause: err}
		}
		return kindCommand, body, nil
	case journal.FenceRecord:
		body, err := journal.MarshalLeaseFence(r.Fence())
		if err != nil {
			return "", nil, &journal.MarshalRecordError{Subject: b.name, Cause: err}
		}
		return kindFence, body, nil
	default:
		return "", nil, &journal.RecordKindError{Subject: b.name}
	}
}
