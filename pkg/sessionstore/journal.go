package sessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	harnesssessionwire "github.com/looprig/harness/pkg/sessionwire"
	durablestore "github.com/looprig/sessionstore"
	"github.com/looprig/storage"
)

// appendTimeout bounds a single Append's ledger round-trip (offload upload plus
// the AppendDefinite) independent of the caller's context. The writer holds its
// mutex across the whole operation, so an unbounded backend call would wedge every
// queued Append behind it; this per-append deadline fails one stuck call fast and
// keeps the serialized writer live. The value matches the journal's historical
// per-append publish deadline, carried over to the storage-backed writer.
const appendTimeout = 5 * time.Second

// hydrateTimeout bounds OpenJournal's full-ledger walk to hydrate the idempotency
// index, independent of the caller's context — a wedged backend must not hang Open
// forever. It is more generous than appendTimeout because it may need to read and
// (for every offloaded record) fetch a whole session's history, not one record.
const hydrateTimeout = 30 * time.Second

// blobsInfix is the name segment separating a session's ledger prefix from its
// content-addressed offload blobs: a blob lands at "sessions/<uuid>/blobs/<sha>".
const blobsInfix = "/blobs/"

var errOpeningAppendMiddleware = errors.New("sessionstore: opening append middleware must delegate exactly once")

// errRuntimeBodyAboveReplayCeiling is the cause carried by the append-time
// refusal of a runtime body larger than replay's maxRuntimeBodyBytes ceiling.
var errRuntimeBodyAboveReplayCeiling = errors.New("sessionstore: runtime body exceeds the replay ceiling")

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

// OpeningFenceConflictError reports that a journal's OPENING fence lost the
// ownership race: the ledger tip moved between this Open's tip read and its fence
// CAS, so some other writer legitimately owns the stream now. It is deliberately
// NOT a transient failure. A lease grant is never rebased: this Open does not
// refresh the tip and try again under the same epoch, because a fence planted at a
// refreshed tip can land AFTER a higher-epoch owner's fence, and every record the
// rebased writer then appends reads, to any later replayer, as though it belonged
// to that higher epoch — the ledger's epoch high-water would fall and a fenced-out
// writer's state would be folded in as the current owner's. The grant that hit this
// error is spent and has been released; the caller must acquire a FRESH lease
// (which yields a strictly higher epoch) and construct a new writer.
type OpeningFenceConflictError struct {
	SessionID uuid.UUID
	Epoch     uint64 // the spent grant's epoch, already released
	Cause     error  // the underlying *journal.AppendError
}

func (e *OpeningFenceConflictError) Error() string {
	return "sessionstore: session " + e.SessionID.String() + ": opening fence at epoch " +
		strconv.FormatUint(e.Epoch, 10) + " lost the ownership race; acquire a fresh lease and open a new journal: " +
		e.Cause.Error()
}

func (e *OpeningFenceConflictError) Unwrap() error { return e.Cause }

// sessionJournal is the concrete single-writer serializer over a storage ledger:
// it frames each JournalRecord as a versioned envelope, offloads an over-threshold
// frame to Blobs before appending a small pointer in its place, and commits under
// CAS fencing on the tracked tip. It is the sessionstore port of the NATS
// journal's WRITE semantics onto storage — storage.AppendDefinite owns the
// ambiguous-ack / conflict resolution the old journal did by hand.
type sessionJournal struct {
	id        uuid.UUID                // the session this journal owns (for fence + error context)
	tenant    coresessionwire.TenantID // the Store's tenant; half of the durable identity every object and projection is filed under
	lease     journal.Lease            // single-writer ownership token (injected; never acquired here)
	ledger    storage.Ledger           // the append-only record log this journal is the sole writer of
	durable   *durablestore.Store
	project   func(coresessionwire.TenantID, coresessionwire.SessionID, any) (harnesssessionwire.Projection, error)
	name      string // the bound ledger name (ledgerName(id))
	threshold int    // configured body threshold, bounded by released envelope limits

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
	// by storage's CAS on append.
	trackedTip uint64

	// idx tracks every idempotency id already durable in this session's log —
	// hydrated from the full ledger AFTER the opening fence has claimed ownership
	// (see OpenJournalWithOpeningAppend and hydrateJournalIndexes) — so a
	// redelivered Append/AppendIdempotent can be detected and deduplicated instead
	// of writing a second frame. It is guarded by mu exactly like ready/trackedTip:
	// only appendChecked reads or updates it once the journal is open, and Open
	// itself holds mu across both the fence commit and the hydration that follows
	// it, so no external caller can observe or mutate it before hydration finishes.
	idx *journal.IdempotencyIndex
	// deliveryTransitions indexes the logical request state for phased delegate
	// commands. It is guarded by mu together with idx and is updated only after
	// the corresponding physical frame commits.
	deliveryTransitions map[uuid.UUID]deliveryTransition
}

type deliveryTransition struct {
	fingerprint journal.Fingerprint
	intentSeq   uint64
	fallbackSeq uint64
}

// Compile-time proofs that *sessionJournal honors the plain journal.SessionJournal
// contract and both of its optional extensions: idempotent dedup reporting, and
// committed canonical public bytes.
var (
	_ journal.SessionJournal         = (*sessionJournal)(nil)
	_ journal.IdempotentJournal      = (*sessionJournal)(nil)
	_ journal.CommittedPublicJournal = (*sessionJournal)(nil)
)

// OpenJournal binds a single-writer journal to session id's ledger and takes
// ownership of the tip by writing the opening fence — a fence-kind envelope
// carrying the lease epoch — as an append fenced on the ledger's current tip. That
// fence advances the tip, so any stale prior writer's next CAS append conflicts;
// only once it commits is the journal ready to accept Appends. The lease is a
// required dependency (DIP): a nil lease fails closed with *NilLeaseError, and a
// lease whose grant has already ended fails closed with
// *journal.JournalLeaseLostError before any tip is read.
//
// The fence is attempted EXACTLY ONCE. If it loses the CAS the grant is released
// and *OpeningFenceConflictError is returned; the caller acquires a fresh lease —
// a strictly higher epoch — and constructs a new writer. A grant is never rebased
// onto a refreshed tip.
func (s *Store) OpenJournal(ctx context.Context, id uuid.UUID, lease journal.Lease) (journal.SessionJournal, error) {
	return s.OpenJournalWithOpeningAppend(ctx, id, lease, nil)
}

// OpenJournalWithOpeningAppend is OpenJournal with middleware around the
// ownership fence append. The middleware sees the fence while it is still part
// of journal construction; the journal is returned only after that append
// commits and ready is set. Later appends are not decorated by this seam.
//
// The middleware runs INSIDE the tip-read-to-CAS window (see the claim comment
// below), so its latency is added to the window this Open is racing to close. It is
// invoked once per Open — and since a lost fence now costs the whole grant, a caller
// re-claiming under fresh grants invokes it once per grant. A slow middleware
// therefore makes contention worse, not merely observable.
func (s *Store) OpenJournalWithOpeningAppend(
	ctx context.Context,
	id uuid.UUID,
	lease journal.Lease,
	middleware journal.AppendMiddleware,
) (journal.SessionJournal, error) {
	if lease == nil {
		return nil, &NilLeaseError{SessionID: id}
	}
	// Refuse a grant that has already ended before touching the ledger. writeLocked
	// has no lease guard of its own — it is the raw CAS core — so without this an
	// ended grant (including one this very function released after losing an earlier
	// opening race) could be replayed into a second Open and, finding the ledger
	// quiet by then, plant its now-stale epoch's fence AFTER a higher-epoch owner's.
	// That is the same rebase the removed retry loop performed in-line, spread over
	// two calls. The ledger CAS is the hard backstop; this is the fast-path refusal.
	if !leaseGrantHeld(lease) {
		return nil, &journal.JournalLeaseLostError{SessionID: id, Epoch: lease.Epoch()}
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
		id:                  id,
		tenant:              s.opts.TenantID,
		lease:               lease,
		ledger:              s.backend.Ledger,
		durable:             s.durable,
		project:             s.project,
		name:                name,
		threshold:           s.opts.OffloadThreshold,
		trackedTip:          tip,
		idx:                 journal.NewIdempotencyIndex(),
		deliveryTransitions: make(map[uuid.UUID]deliveryTransition),
	}

	// Take ownership FIRST, immediately after the tip read: on the middleware-free
	// path there is no intervening I/O at all — the same tight tip-then-CAS
	// coupling every later Append already has via trackedTip (writeLocked never
	// re-reads the tip; it CASes on whatever is already tracked in memory). A
	// caller-supplied AppendMiddleware (as both of internal/sessionruntime's own
	// callers — Lifecycle.NewSession and restoreTopologySession — install via
	// journal.HookMiddleware whenever a hook handles OperationJournalAppend) still
	// runs between the tip read and the CAS below and could itself do I/O — that
	// gap is real but bounded by whatever the hook does, not by a full-ledger
	// walk.
	// Claiming the fence this early, BEFORE the idempotency-index hydration below,
	// closes the window in which a still-live predecessor writer (a crash-path
	// teardown append, or a parked goroutine unblocked by context cancellation —
	// see handBackRequest) could land a write on this ledger and advance the tip
	// out from under a tip value cached before a slow walk. The first append is
	// the opening fence, stamping the lease epoch and fenced on the tip read just
	// above — the only tip this grant will ever fence on. If the ledger moved in
	// between, this grant lost the ownership race and Open fails closed below; it
	// does NOT refresh the tip and try again. Any other append failure also fails
	// closed. Only once the fence commits is the journal ready.
	fence := journal.NewFenceRecord(id, journal.LeaseFence{Epoch: lease.Epoch()})
	j.mu.Lock()
	defer j.mu.Unlock()
	delegations := 0
	var fenceSeq uint64
	var fenceErr error
	appendOpening := journal.AppendFunc(func(appendCtx context.Context, _ journal.JournalRecord) (uint64, error) {
		delegations++
		if delegations != 1 {
			return 0, errOpeningAppendMiddleware
		}
		// The middleware may derive context but cannot substitute the ownership
		// record: construction always commits this exact fence THROUGH THE RAW
		// writeLocked path — never through appendChecked's idempotency gate — so
		// even a repeated lease epoch (whose id would otherwise look like a prior
		// duplicate) still physically advances the tip and fences out a stale
		// writer. One attempt, on the tracked tip as read above.
		fenceSeq, fenceErr = j.writeLocked(appendCtx, fence)
		return fenceSeq, fenceErr
	})
	if middleware != nil {
		appendOpening = middleware(appendOpening)
	}
	if appendOpening == nil {
		return nil, errOpeningAppendMiddleware
	}
	// The middleware's returned pair is deliberately ignored. Open derives its
	// result only from the guarded real append, so a decorator cannot suppress
	// an error or fabricate a successful fence sequence.
	_, _ = appendOpening(ctx, fence)
	if delegations != 1 {
		return nil, errOpeningAppendMiddleware
	}
	if fenceErr != nil {
		var conflict *storage.ConflictError
		if errors.As(fenceErr, &conflict) {
			// The ledger moved between this Open's tip read and its fence CAS, so some
			// other writer owns the stream. Retrying under this grant is exactly the
			// rebase the fence exists to prevent: a fence planted at a refreshed tip
			// can land after a HIGHER-epoch owner's fence, dropping the ledger's epoch
			// high-water, and every record this writer then appended would read to a
			// later replayer as the higher epoch's own — a fenced-out writer's state
			// folded in as the current owner's. Surrender the grant instead. The
			// release runs on its own background deadline, not ctx: it must happen even
			// when Open is failing because ctx is already done, and a stale release can
			// never free a later holder. A second release by the caller's own failure
			// path is a no-op.
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), appendTimeout)
			_ = lease.Release(releaseCtx)
			releaseCancel()
			return nil, &OpeningFenceConflictError{SessionID: id, Epoch: lease.Epoch(), Cause: fenceErr}
		}
		return nil, fenceErr
	}

	// Now that ownership is claimed, hydrate the idempotency index from whatever
	// is durable — including the fence just committed above, and anything else
	// that lands on the ledger before this walk observes it, since a fresh
	// full-ledger read has no dependency on the pre-fence tip snapshot. Deferring
	// this SLOW walk until after the fence commits is what closes the race: no
	// predecessor writer can land another append once the fence has fenced it out
	// (its next CAS conflicts against the tip this fence already advanced), so
	// hydration can safely take as long as it needs without widening the window
	// the opening fence itself is exposed to. j.ready stays false and j is not
	// yet returned to any caller, so nothing can call Append/AppendIdempotent
	// through this instance and race the walk. A ledger that had nothing durable
	// before this fence (tip 0, the common fresh-session case) has nothing more to
	// hydrate beyond what the fence's own hygiene Observe below already records,
	// so the walk is skipped entirely rather than performed for no reason.
	if tip > 0 {
		hydrateCtx, hydrateCancel := context.WithTimeout(ctx, hydrateTimeout)
		idx, transitions, hydrateErr := hydrateJournalIndexes(hydrateCtx, s, id, name)
		hydrateCancel()
		if hydrateErr != nil {
			// Accepted trade-off of hydrating after the fence: the fence above has
			// already durably committed (the tip is advanced) even though Open now
			// fails closed and returns this journal to no caller. That leaves an
			// orphaned fence stamped with this lease's epoch sitting in the ledger —
			// impossible in the pre-fix ordering, where nothing could fail once the
			// fence's writeLocked succeeded. It is not a correctness problem: the
			// caller's failure path releases the lease as usual, and the next Open
			// simply reads a fresh tip past this stray fence and claims its own —
			// the same self-healing the epoch/fencing design already relies on for
			// any crash between a fence commit and full ownership. Surfacing it here
			// so a future reader chasing an orphaned fence in production logs has a
			// documented, expected cause rather than a mystery.
			return nil, hydrateErr
		}
		j.idx = idx
		j.deliveryTransitions = transitions
	}
	// Keep the index authoritative for the fence itself too. This is hygiene, not
	// load-bearing: the fence always commits through the raw writeLocked path above
	// regardless of what the index holds. It is also not redundant with the
	// hydration walk above, which — for a fresh session (tip 0 before this fence)
	// — is skipped and so never observes the fence on its own. A MarshalLeaseFence
	// failure here is unreachable in practice (a LeaseFence is one uint64) and is
	// simply not observed rather than failing a fence that has already durably
	// committed.
	if body, marshalErr := journal.MarshalLeaseFence(fence.Fence()); marshalErr == nil {
		j.idx.Observe(fence.IdempotencyID(), fenceSeq, journal.NewFingerprint(string(kindFence), body))
	}
	j.ready = true
	return j, nil
}

func hydrateJournalIndexes(ctx context.Context, store *Store, id uuid.UUID, name string) (*journal.IdempotencyIndex, map[uuid.UUID]deliveryTransition, error) {
	idx := journal.NewIdempotencyIndex()
	transitions := make(map[uuid.UUID]deliveryTransition)
	cur, err := store.backend.Ledger.Read(ctx, name, 1)
	if err != nil {
		return nil, nil, &ReplayReadError{Name: name, Cause: err}
	}
	base := &baseCursor{name: name, blobs: store.backend.Blobs, durable: store.durable, tenant: store.opts.TenantID, sessionID: id, cur: cur}
	defer func() { _ = base.close() }()
	for {
		r, nextErr := base.next(ctx)
		if errors.Is(nextErr, io.EOF) {
			return idx, transitions, nil
		}
		if nextErr != nil {
			return nil, nil, nextErr
		}
		idx.Observe(r.id, r.seq, journal.NewFingerprint(string(r.kind), r.body))
		if err := observeHydratedDeliveryTransition(transitions, r); err != nil {
			return nil, nil, err
		}
	}
}

// observeHydratedDeliveryTransition rebuilds the logical request index from
// one durable command frame. A fallback is accepted only after its intent has
// appeared earlier in ledger order with the same phase-normalized payload.
func observeHydratedDeliveryTransition(transitions map[uuid.UUID]deliveryTransition, r resolved) error {
	if r.kind != kindCommand {
		return nil
	}
	decoded, err := command.UnmarshalCommand(r.body)
	if err != nil {
		return &ReplayDecodeError{Seq: r.seq, Cause: err}
	}
	input, ok := decoded.(command.UserInput)
	if !ok || !input.DelegateDeliveryPhase.Valid() {
		return nil
	}
	record := journal.NewCommandRecord(uuid.UUID{}, uuid.UUID{}, input)
	if record.IdempotencyID() != r.id {
		return &journal.DeliveryTransitionError{
			CommandID: input.CommandID,
			Phase:     input.DelegateDeliveryPhase,
			Reason:    "physical id does not match command phase",
		}
	}
	fingerprint, err := record.NormalizedDeliveryFingerprint()
	if err != nil {
		return err
	}
	logicalID := input.CommandID
	prior, exists := transitions[logicalID]
	switch input.DelegateDeliveryPhase {
	case command.DelegateDeliveryPhaseIntent:
		if exists {
			if prior.fingerprint != fingerprint {
				return &journal.DeliveryTransitionError{CommandID: logicalID, Phase: input.DelegateDeliveryPhase, Reason: "intent payload changed"}
			}
			if prior.intentSeq != 0 {
				return &journal.DeliveryTransitionError{CommandID: logicalID, Phase: input.DelegateDeliveryPhase, Reason: "duplicate intent frame"}
			}
		}
		transitions[logicalID] = deliveryTransition{fingerprint: fingerprint, intentSeq: r.seq}
	case command.DelegateDeliveryPhaseFallbackQueued:
		if !exists || prior.intentSeq == 0 {
			return &journal.DeliveryTransitionError{CommandID: logicalID, Phase: input.DelegateDeliveryPhase, Reason: "fallback precedes intent"}
		}
		if prior.fingerprint != fingerprint {
			return &journal.DeliveryTransitionError{CommandID: logicalID, Phase: input.DelegateDeliveryPhase, Reason: "fallback payload differs from intent"}
		}
		if prior.fallbackSeq != 0 {
			return &journal.DeliveryTransitionError{CommandID: logicalID, Phase: input.DelegateDeliveryPhase, Reason: "duplicate fallback frame"}
		}
		prior.fallbackSeq = r.seq
		transitions[logicalID] = prior
	}
	return nil
}

// Append serializes rec behind mu, refuses if the journal is not ready or its lease
// is lost, then deduplicates by idempotency id, and — for a genuinely new record —
// frames, offloads-if-large, and commits rec under CAS on the tracked tip. The whole
// operation holds mu so the guard, dedup check, offload, append, tip advance, and
// index update are one atomic step; the append carries its own per-append deadline so
// one stuck call cannot wedge the queued writers. On success it returns the assigned
// (or, for a deduplicated retry, the ORIGINAL) ledger sequence. Append,
// AppendIdempotent, and AppendCommitted share the same core (appendChecked) and each
// discards the part of its result it does not need: Append keeps only the
// sequence/error — see AppendIdempotent (journal.IdempotentJournal) for callers that
// need to distinguish a fresh append from a deduplicated retry, and AppendCommitted
// (journal.CommittedPublicJournal) for callers that additionally need the exact
// canonical public bytes this writer stored.
func (b *sessionJournal) Append(ctx context.Context, rec journal.JournalRecord) (uint64, error) {
	result, err := b.appendChecked(ctx, rec)
	return result.Sequence, err
}

// AppendIdempotent is Append's richer counterpart (journal.IdempotentJournal): see
// Append's doc for the shared mechanics. It exists so a caller that must react
// differently to a fresh append versus a deduplicated retry (e.g. skip a live
// broadcast for a duplicate) can observe that distinction via AppendResult.Appended.
func (b *sessionJournal) AppendIdempotent(ctx context.Context, rec journal.JournalRecord) (journal.AppendResult, error) {
	result, err := b.appendChecked(ctx, rec)
	return result.AppendResult, err
}

// AppendCommitted is Append's richest counterpart (journal.CommittedPublicJournal):
// same mechanics, and it additionally reports the canonical public event id and the
// EXACT canonical public body this writer stored for a newly appended PUBLIC event.
// Those are the bytes the frame path projected ONCE and wrote — not a second
// projection of the same event — because a consumer joining the durable tail to a
// live publication compares the two, and a re-derived body that merely happens to
// agree today is not the same guarantee.
//
// A deduplicated retry reports Appended=false with a zero Public: this call wrote
// nothing, so it has no stored bytes of its own to report. The ORIGINAL append's
// bytes remain durable at the returned sequence; whether the public READ serves them
// back depends on size (see the caveat on event.Delivery.PublicBody).
func (b *sessionJournal) AppendCommitted(ctx context.Context, rec journal.JournalRecord) (journal.CommittedAppendResult, error) {
	return b.appendChecked(ctx, rec)
}

// appendChecked is the shared serialized core behind Append and AppendIdempotent. It
// guards readiness/lease exactly as the plain Append always has, then — under the
// SAME lock — fingerprints rec's persisted (kind, body) via encodeRecordBody (the
// same codec path writeLocked/frame already use to encode for the wire; it never
// encodes a record's transient routing, e.g. CommandRecord's session/loop dispatch
// target, so that routing can never enter the fingerprint) and consults the hydrated
// IdempotencyIndex:
//   - an id never seen before is written as a new record (writeLocked) and then
//     observed into the index;
//   - an id seen before with an IDENTICAL fingerprint is deduplicated: no second
//     frame is written, and the ORIGINAL sequence is returned with Appended=false;
//   - an id seen before with a DIFFERENT fingerprint fails closed with a typed
//     *journal.IdempotencyCollisionError.
func (b *sessionJournal) appendChecked(ctx context.Context, rec journal.JournalRecord) (journal.CommittedAppendResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.ready {
		return journal.CommittedAppendResult{}, &journal.JournalNotReadyError{SessionID: b.id}
	}
	if !b.leaseHeld() {
		return journal.CommittedAppendResult{}, &journal.JournalLeaseLostError{SessionID: b.id, Epoch: b.lease.Epoch()}
	}
	var commandRecord journal.CommandRecord
	var hasPhasedCommand bool
	if candidate, ok := rec.(journal.CommandRecord); ok && candidate.DeliveryPhase() != "" {
		if err := journal.ValidateCommandRecordRoute(candidate); err != nil {
			return journal.CommittedAppendResult{}, err
		}
		commandRecord = candidate
		hasPhasedCommand = true
	}

	k, body, err := b.encodeRecordBody(rec)
	if err != nil {
		return journal.CommittedAppendResult{}, err
	}
	id := rec.IdempotencyID()
	fp := journal.NewFingerprint(string(k), body)
	if seq, duplicate, checkErr := b.idx.Check(id, fp); checkErr != nil {
		return journal.CommittedAppendResult{}, checkErr
	} else if duplicate {
		return journal.CommittedAppendResult{AppendResult: journal.AppendResult{Sequence: seq, Appended: false}}, nil
	}

	var transitionRecord *journal.CommandRecord
	var pendingTransition deliveryTransition
	if hasPhasedCommand && commandRecord.DeliveryPhase().Valid() {
		pending, err := b.prepareDeliveryTransition(commandRecord)
		if err != nil {
			return journal.CommittedAppendResult{}, err
		}
		transitionRecord = &commandRecord
		pendingTransition = pending
	}

	seq, public, err := b.writeEncodedLocked(ctx, rec, k, body)
	if err != nil {
		return journal.CommittedAppendResult{}, err
	}
	b.idx.Observe(id, seq, fp)
	if transitionRecord != nil {
		if b.deliveryTransitions == nil {
			b.deliveryTransitions = make(map[uuid.UUID]deliveryTransition)
		}
		if transitionRecord.DeliveryPhase() == command.DelegateDeliveryPhaseIntent {
			pendingTransition.intentSeq = seq
		} else {
			pendingTransition.fallbackSeq = seq
		}
		b.deliveryTransitions[transitionRecord.LogicalCommandID()] = pendingTransition
	}
	return journal.CommittedAppendResult{
		AppendResult: journal.AppendResult{Sequence: seq, Appended: true},
		Public:       public,
	}, nil
}

func (b *sessionJournal) prepareDeliveryTransition(record journal.CommandRecord) (deliveryTransition, error) {
	fingerprint, err := record.NormalizedDeliveryFingerprint()
	if err != nil {
		return deliveryTransition{}, err
	}
	logicalID := record.LogicalCommandID()
	prior, exists := b.deliveryTransitions[logicalID]
	switch record.DeliveryPhase() {
	case command.DelegateDeliveryPhaseIntent:
		if exists {
			if prior.fingerprint != fingerprint {
				return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "intent payload changed"}
			}
			return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "intent already durable"}
		}
		return deliveryTransition{fingerprint: fingerprint}, nil
	case command.DelegateDeliveryPhaseFallbackQueued:
		if !exists || prior.intentSeq == 0 {
			return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "fallback precedes intent"}
		}
		if prior.fingerprint != fingerprint {
			return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "fallback payload differs from intent"}
		}
		if prior.fallbackSeq != 0 {
			return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "fallback already durable"}
		}
		return prior, nil
	default:
		return deliveryTransition{}, &journal.DeliveryTransitionError{CommandID: logicalID, Phase: record.DeliveryPhase(), Reason: "unsupported delivery phase"}
	}
}

// leaseHeld reports whether the ownership lease is still held: both its validity
// flag and its loss channel must say so. It is the fast-path ownership guard; the
// ledger's CAS fence is the hard backstop that catches a loss this guard races.
func (b *sessionJournal) leaseHeld() bool { return leaseGrantHeld(b.lease) }

// leaseGrantHeld is leaseHeld over a bare grant, for the ownership check
// OpenJournal makes before a journal exists to ask.
func leaseGrantHeld(lease journal.Lease) bool {
	if !lease.Valid() {
		return false
	}
	select {
	case <-lease.Lost():
		return false
	default:
		return true
	}
}

// writeLocked is the serialized write core shared by Append (after its ready/lease
// guard) and OpenJournal (the opening fence, which is what SETS ready). The caller
// MUST hold mu. It derives a per-append child context, frames rec (offloading an
// over-threshold frame to Blobs first), commits the resulting bytes under CAS on
// the tracked tip via storage.AppendDefinite, and on success advances the tip and
// returns the new sequence. On any failure the tip is left unadvanced (fail closed).
func (b *sessionJournal) writeLocked(ctx context.Context, rec journal.JournalRecord) (uint64, error) {
	k, body, err := b.encodeRecordBody(rec)
	if err != nil {
		return 0, err
	}
	seq, _, err := b.writeEncodedLocked(ctx, rec, k, body)
	return seq, err
}

// writeEncodedLocked additionally returns the canonical public identity and body the
// framing step stored for a public event record (zero for every other record). It is
// the ONLY place those bytes are produced, so returning them here is what lets a
// caller publish the stored bytes rather than a second projection of the same event.
func (b *sessionJournal) writeEncodedLocked(ctx context.Context, rec journal.JournalRecord, k kind, body []byte) (uint64, journal.CommittedPublicBody, error) {
	childCtx, cancel := context.WithTimeout(ctx, appendTimeout)
	defer cancel()

	recordBytes, public, err := b.frame(childCtx, rec, k, body)
	if err != nil {
		return 0, journal.CommittedPublicBody{}, err
	}
	if err := storage.AppendDefinite(childCtx, b.ledger, b.name, b.trackedTip, recordBytes); err != nil {
		return 0, journal.CommittedPublicBody{}, b.mapAppendErr(rec, err)
	}
	b.trackedTip++
	return b.trackedTip, public, nil
}

// frame maps one already-encoded Harness record into SessionStore's released
// envelope. Public events carry the native replay body and one independently
// projected canonical public body; private records carry only their native body.
// Each over-threshold body is persisted through SessionStore's verified immutable
// object API before the envelope reference is appended. It runs under mu, so object
// publication remains serialized with the append that makes it reachable.
//
// It returns that canonical public identity and body alongside the framed bytes. The
// projection runs exactly ONCE, here, and the caller publishes what was stored rather
// than projecting a second time — a second projection is a second answer.
func (b *sessionJournal) frame(ctx context.Context, rec journal.JournalRecord, k kind, body []byte) ([]byte, journal.CommittedPublicBody, error) {
	env := durablestore.Envelope{}
	var publicBody []byte
	switch k {
	case kindFence:
		env.Kind = durablestore.EnvelopeKindOpeningFence
		env.LeaseEpoch = rec.(journal.FenceRecord).Fence().Epoch
	case kindEvent:
		eventRecord := rec.(journal.EventRecord)
		if eventRecord.Event().Visibility() == event.Public {
			projection, projectErr := b.project(b.tenant, harnessSessionID(b.id), eventRecord.Event())
			if projectErr != nil {
				return nil, journal.CommittedPublicBody{}, &journal.MarshalRecordError{Subject: b.name, Cause: projectErr}
			}
			env.Kind = durablestore.EnvelopeKindPublicEvent
			env.EventID = projection.EventID
			publicBody = projection.Body
		} else {
			env.Kind = durablestore.EnvelopeKindRuntimeControl
			env.RecordID = durableRecordID(k, rec.IdempotencyID())
		}
	case kindCommandApplication:
		// The application prefix has a FIRST-CLASS released envelope kind, and using
		// it is not a tidiness choice. The generic RuntimeControl slot carries a
		// derived RecordID, and the released reader's settlement correlation
		// (FindCommandApplication) matches on `Kind == EnvelopeKindApplicationPrefix
		// && CommandID == record.CommandID`. A prefix framed as RuntimeControl can
		// never match it, so every command Harness applies would read as ABSENT to
		// the counterparty — and ABSENT is the single outcome that licenses the
		// deadline reconciler to settle `rejected`. That is settling rejected over a
		// durable effect: a writer withholding the evidence the reader exists to read.
		//
		// The identity field carries the RAW public id, validated by Core's own
		// sessionwire.CommandID.Validate, so Harness's acceptance and the durable
		// boundary's acceptance are one rule rather than two that can drift.
		//
		// The kind's record-shape rule forbids a body: these envelope fields ARE the
		// record. body is still computed by encodeRecordBody, but only to fingerprint
		// the record for idempotency; it is never persisted, and the read side
		// reconstructs the identical bytes from these fields.
		app := rec.(journal.CommandApplicationRecord).Application()
		env.Kind = durablestore.EnvelopeKindApplicationPrefix
		env.CommandID = coresessionwire.CommandID(app.CommandID)
		env.RuntimeCommandID = app.RuntimeCommandID
		env.LeaseEpoch = app.LeaseEpoch
		env.CommandKind = string(app.Kind)
	default:
		env.Kind = durablestore.EnvelopeKindRuntimeControl
		env.RecordID = durableRecordID(k, rec.IdempotencyID())
	}
	// Bodiless kinds: the fence and the application prefix are wholly described by
	// their envelope fields, so neither publishes an object nor occupies a body slot.
	bodiless := k == kindFence || k == kindCommandApplication
	// Refuse a runtime body the READ side could never admit, before any object is
	// published. Only event.MarshalEvent caps its own output;
	// command.MarshalCommand and journal.MarshalGatePreparedRecord do not, so
	// without this guard an over-ceiling body would be offloaded and appended
	// successfully and then be permanently unreadable on replay — a fail-closed
	// but UNRECOVERABLE restore for that session. Fail the append instead, with
	// the same legacy *journal.RecordTooLargeError classification an oversized
	// record has always carried.
	if !bodiless && len(body) > maxRuntimeBodyBytes {
		return nil, journal.CommittedPublicBody{}, &journal.RecordTooLargeError{
			Subject: b.name, MsgID: rec.IdempotencyID(), Length: len(body),
			Cause: errRuntimeBodyAboveReplayCeiling,
		}
	}
	publicOffload, runtimeOffload, err := b.effectiveOffloadPlan(env, publicBody, body, !bodiless)
	if err != nil {
		return nil, journal.CommittedPublicBody{}, err
	}
	if publicBody != nil {
		env.Public, err = b.durableBodySlot(ctx, durablestore.ObjectKindJournalPublic, publicBody, publicOffload)
		if err != nil {
			return nil, journal.CommittedPublicBody{}, b.mapOffloadErr(rec, len(publicBody), err)
		}
	}
	if !bodiless {
		env.Runtime, err = b.durableBodySlot(ctx, durablestore.ObjectKindJournalRuntime, body, runtimeOffload)
		if err != nil {
			return nil, journal.CommittedPublicBody{}, b.mapOffloadErr(rec, len(body), err)
		}
	}
	frameBytes, err := durablestore.EncodeEnvelope(env)
	if err != nil {
		return nil, journal.CommittedPublicBody{}, err
	}
	if publicBody == nil {
		return frameBytes, journal.CommittedPublicBody{}, nil
	}
	// The bytes handed back are the ones just written — inline in this frame or
	// uploaded as this frame's public object — cloned so no caller can reach back
	// into the projector's buffer. The CONTENT is identical either way, which is the
	// whole point; note that the read side is not symmetric, because it refuses a
	// public reference above the released inline ceiling (see the caveat on
	// event.Delivery.PublicBody). That asymmetry is a boundary condition of the
	// released reader, not of these bytes: a live delivery carries them regardless.
	return frameBytes, journal.CommittedPublicBody{
		EventID: string(env.EventID),
		Body:    bytes.Clone(publicBody),
	}, nil
}

func durableRecordID(k kind, id string) string { return string(k) + "|" + id }

// effectiveOffloadPlan centralizes the compatibility policy between Harness's
// configurable threshold and the released envelope codec. Each inline body must
// satisfy the released per-body ceiling. A public event whose two individually
// valid bodies would exceed the released total frame ceiling offloads the larger
// body (runtime on a tie), keeping the object count minimal and the public body
// directly readable when either choice is equivalent.
func (b *sessionJournal) effectiveOffloadPlan(env durablestore.Envelope, publicBody, runtimeBody []byte, hasRuntime bool) (public, runtime bool, err error) {
	effectiveThreshold := b.threshold
	if effectiveThreshold > durablestore.MaxInlineBodyBytes {
		effectiveThreshold = durablestore.MaxInlineBodyBytes
	}
	public = publicBody != nil && len(publicBody) > effectiveThreshold
	runtime = hasRuntime && len(runtimeBody) > effectiveThreshold
	if publicBody == nil || public || runtime {
		return public, runtime, nil
	}
	candidate := env
	candidate.Public = durablestore.BodySlot{Inline: publicBody}
	candidate.Runtime = durablestore.BodySlot{Inline: runtimeBody}
	if _, encodeErr := durablestore.EncodeEnvelope(candidate); encodeErr == nil {
		return false, false, nil
	} else {
		var envelopeErr *durablestore.EnvelopeError
		if !errors.As(encodeErr, &envelopeErr) || envelopeErr.Code != durablestore.EnvelopeErrorTooLarge || envelopeErr.Field != "frame" {
			return false, false, encodeErr
		}
	}
	if len(runtimeBody) >= len(publicBody) {
		return false, true, nil
	}
	return true, false, nil
}

func (b *sessionJournal) durableBodySlot(ctx context.Context, objectKind durablestore.ObjectKind, body []byte, offload bool) (durablestore.BodySlot, error) {
	if !offload {
		return durablestore.BodySlot{Inline: bytes.Clone(body)}, nil
	}
	digest := sha256.Sum256(body)
	metadata, err := b.durable.PutObject(ctx, durablestore.PutObjectRequest{
		TenantID:  b.tenant,
		SessionID: harnessSessionID(b.id),
		Kind:      objectKind,
		SizeBytes: uint64(len(body)),
		SHA256:    digest,
		Body:      bytes.NewReader(body),
	})
	if err != nil {
		return durablestore.BodySlot{}, err
	}
	reference, err := durablestore.BodyReferenceFromObjectMetadata(metadata)
	if err != nil {
		return durablestore.BodySlot{}, err
	}
	return durablestore.BodySlot{Reference: &reference}, nil
}

func (b *sessionJournal) mapOffloadErr(rec journal.JournalRecord, length int, err error) error {
	return &journal.RecordTooLargeError{Subject: b.name, MsgID: rec.IdempotencyID(), Length: length, Cause: err}
}

// mapAppendErr translates a storage append failure into the journal's error
// vocabulary so callers keep classifying at the journal level: a definite CAS
// conflict (a stale writer fenced out) becomes *journal.AppendError; a still-
// ambiguous outcome becomes *journal.AmbiguousAckError; any other error is
// surfaced unchanged (fail closed). The tip is already left unadvanced by the caller.
func (b *sessionJournal) mapAppendErr(rec journal.JournalRecord, err error) error {
	var conflict *storage.ConflictError
	if errors.As(err, &conflict) {
		return &journal.AppendError{Subject: b.name, MsgID: rec.IdempotencyID(), Expected: b.trackedTip, Cause: err}
	}
	var ambiguous *storage.AmbiguousError
	if errors.As(err, &ambiguous) {
		return &journal.AmbiguousAckError{Subject: b.name, MsgID: rec.IdempotencyID(), Expected: b.trackedTip, Cause: err}
	}
	// AppendDefinite's verifyAppend could not read the contested tip to resolve a
	// conflict, so the append outcome is genuinely UNKNOWN — the same unresolved,
	// decide-fail-or-retry case as a lingering ambiguous ack. Map it to the journal's
	// AmbiguousAckError (carrying the verify error) rather than leaking storage's
	// type through the facade.
	var verify *storage.AppendVerifyError
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
		if _, private := r.Event().(event.GatePrepared); private {
			return "", nil, &journal.MarshalRecordError{Subject: b.name, Cause: errors.New("GatePrepared is private; append journal.GatePreparedRecord")}
		}
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
	case journal.GatePreparedRecord:
		body, err := journal.MarshalGatePreparedRecord(r)
		if err != nil {
			return "", nil, &journal.MarshalRecordError{Subject: b.name, Cause: err}
		}
		return kindGatePrepared, body, nil
	case journal.CommandApplicationRecord:
		body, err := journal.MarshalCommandApplicationRecord(r)
		if err != nil {
			return "", nil, &journal.MarshalRecordError{Subject: b.name, Cause: err}
		}
		return kindCommandApplication, body, nil
	default:
		return "", nil, &journal.RecordKindError{Subject: b.name}
	}
}
