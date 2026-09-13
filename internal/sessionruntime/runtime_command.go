package sessionruntime

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/looprig/core/uuid"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/runtimecommand"
)

// runtimeCommandLog is the session's narrow durable seam for APPLICATION PREFIXES:
// make one correlation durable, and read one back. It is deliberately separate from
// commandAppender (command_journal.go), which is the AUDIT-ONLY intent log: that
// seam logs and swallows its failures so a lost audit record can never block a
// user's input, and treating it as an inbox would silently drop the very record the
// duplicate-delivery rule depends on. This seam has the opposite policy — an append
// failure REFUSES the command — because the prefix is the crash-safety barrier in
// front of a runtime-visible effect.
//
// The session depends on these two methods alone (Interface Segregation), never on
// the store, the journal, or the record codec.
type runtimeCommandLog interface {
	// AppendCommandApplication makes app durable. Appended=false reports that an
	// identical prefix was already durable, carrying the ORIGINAL sequence. A public
	// CommandID already durable under a different mapping fails closed with
	// *journal.IdempotencyCollisionError.
	AppendCommandApplication(ctx context.Context, app runtimecommand.Application) (journal.AppendResult, error)
	// ReadCommandApplicationAt returns the durable correlation at seq.
	ReadCommandApplicationAt(ctx context.Context, seq uint64) (runtimecommand.Application, error)
}

// dispositionLog is the OPTIONAL extension of runtimeCommandLog the attempt-aware
// paths need: write one disposition, and scan for a committed effect. It is separate
// from runtimeCommandLog rather than folded into it so an existing implementer — a
// composition root's log, a test double — keeps satisfying the base seam unchanged,
// and so a deployment whose log cannot record dispositions is DISCOVERED rather than
// nil-dereferenced.
type dispositionLog interface {
	AppendCommandDisposition(ctx context.Context, d runtimecommand.CommandDisposition) (journal.AppendResult, error)
	ScanCommandEffect(ctx context.Context, commandID runtimecommand.CommandID, runtimeID uuid.UUID) (runtimecommand.EffectScan, error)
}

// DispositionUnsupportedError reports that an attempt-bearing command reached a
// session whose durable log cannot record a disposition. It is raised BEFORE any
// durable write: a command applied with no evidence sits applying forever, which is
// strictly worse than a refusal Host can retry elsewhere.
type DispositionUnsupportedError struct {
	CommandID runtimecommand.CommandID
	AttemptID runtimecommand.AttemptID
}

func (e *DispositionUnsupportedError) Error() string {
	return "sessionruntime: this session's durable log cannot record a command disposition; refusing attempt " +
		strconv.Quote(string(e.AttemptID)) + " of command " + strconv.Quote(string(e.CommandID))
}

// recordDisposition appends the attempt's durable disposition, or does nothing at
// all when the admitted record carries no attempt id.
//
// THE EMPTY-ATTEMPT ARM IS THE COMPATIBILITY CONTRACT, not an optimization. A binary
// pinned below the sessionstore release that knows the disposition envelope kind
// REFUSES a journal containing one, by design — decoders fail closed on an unknown
// kind — so a legacy-admitted session's journal must stay exactly what it was.
//
// The grant it stamps is the one this applier HOLDS, not the one the admitted record
// claims. Nothing is store-stamped on this path: the frame is encoded here and the
// bytes are appended here, so the reader treats the epoch as a claim and cross-checks
// it against the nearest preceding opening fence. A wrong value does not degrade
// settlement, it stops it.
func (s *Session) recordDisposition(
	ctx context.Context,
	log dispositionLog,
	admitted runtimecommand.Admitted,
	kind runtimecommand.DispositionKind,
) error {
	if admitted.AttemptID == "" || log == nil {
		return nil
	}
	_, err := log.AppendCommandDisposition(ctx, admitted.DispositionFor(kind, s.runtimeCommandLease.Epoch()))
	return err
}

// CloseAttempt writes the not_applied recovery closure for an attempt a previous
// runtime never finished. See runtimecommand.AttemptCloser for the contract.
//
// FOUR REFUSALS, and the two that matter are the last two.
//
// The GRANT check is the protocol's: a closure is authored by a strictly later
// journal grant, and a runtime closing its own attempt would be writing a tombstone
// over work it is still doing. The MAPPING check keeps a closure from tombstoning a
// command whose durable prefix binds a different runtime identity.
//
// The IDEMPOTENCY guard is free and is not written here at all — it is the record's
// own key. A disposition's idempotency id derives from the ATTEMPT id, and Harness
// hydrates its index from the journal at open, so a successor's not_applied collides
// with a predecessor's durable applied and fails closed at the append. A successor
// cannot tombstone an applied command.
//
// The EFFECT guard is the one that is NOT free, and it is why this method pays for a
// whole-journal scan. If the predecessor's effect committed but its disposition
// append failed, there is no colliding record: the idempotency guard sees nothing,
// and a closure would convert a real effect into a tombstone every future grant must
// honour. The scan looks for an enduring event caused by that runtime command after
// its prefix and refuses when it finds one.
func (s *Session) CloseAttempt(ctx context.Context, c runtimecommand.Closure) (runtimecommand.ClosureResult, error) {
	log, lease := s.runtimeCommands, s.runtimeCommandLease
	if log == nil || lease == nil {
		return runtimecommand.ClosureResult{}, &runtimecommand.CapabilityUnavailableError{CommandID: c.CommandID}
	}
	if err := c.Validate(); err != nil {
		return runtimecommand.ClosureResult{}, err
	}
	dispositions, ok := log.(dispositionLog)
	if !ok {
		return runtimecommand.ClosureResult{}, &DispositionUnsupportedError{CommandID: c.CommandID, AttemptID: c.AttemptID}
	}
	if !lease.Valid() {
		return runtimecommand.ClosureResult{}, &runtimecommand.ClosureNotAuthorizedError{
			AttemptID: c.AttemptID, AttemptJournalEpoch: c.AttemptJournalEpoch, Current: lease.Epoch(),
		}
	}
	current := lease.Epoch()
	if current <= c.AttemptJournalEpoch {
		return runtimecommand.ClosureResult{}, &runtimecommand.ClosureNotAuthorizedError{
			AttemptID: c.AttemptID, AttemptJournalEpoch: c.AttemptJournalEpoch, Current: current, Held: true,
		}
	}
	scan, err := dispositions.ScanCommandEffect(ctx, c.CommandID, c.RuntimeCommandID)
	if err != nil {
		return runtimecommand.ClosureResult{}, err
	}
	if scan.PrefixSeq != 0 && (scan.DurableRuntimeID != c.RuntimeCommandID || scan.DurableKind != c.Kind) {
		return runtimecommand.ClosureResult{}, &runtimecommand.MappingConflictError{
			CommandID:        c.CommandID,
			RuntimeCommandID: c.RuntimeCommandID,
			DurableRuntimeID: scan.DurableRuntimeID,
			Kind:             c.Kind,
			DurableKind:      scan.DurableKind,
			Sequence:         scan.PrefixSeq,
		}
	}
	if scan.EffectFound {
		return runtimecommand.ClosureResult{}, &runtimecommand.EnduringEffectError{
			AttemptID:        c.AttemptID,
			CommandID:        c.CommandID,
			RuntimeCommandID: c.RuntimeCommandID,
			PrefixSeq:        scan.PrefixSeq,
			EffectSeq:        scan.EffectSeq,
		}
	}
	res, err := dispositions.AppendCommandDisposition(ctx, runtimecommand.CommandDisposition{
		CommandID:           c.CommandID,
		RuntimeCommandID:    c.RuntimeCommandID,
		Kind:                c.Kind,
		LeaseEpoch:          current,
		AttemptID:           c.AttemptID,
		AttemptJournalEpoch: c.AttemptJournalEpoch,
		Disposition:         runtimecommand.DispositionNotApplied,
	})
	if err != nil {
		return runtimecommand.ClosureResult{}, err
	}
	return runtimecommand.ClosureResult{Sequence: res.Sequence, Appended: res.Appended}, nil
}

// leaseEpochSource is the two-method view of the session's single-writer lease the
// runtime-command applier needs: which epoch it holds, and whether it still holds
// it. journal.Lease satisfies it structurally, so the applier never depends on the
// lease's lifecycle surface (Release, SessionID) it must not touch.
type leaseEpochSource interface {
	Epoch() uint64
	Valid() bool
}

// WithRuntimeCommands injects the durable application-prefix log and the session's
// lease, enabling the segregated runtime-command capability. Both are required: an
// applier without a lease cannot reject a superseded admission, and one without a
// deduplicating log cannot honor duplicate delivery. A nil argument leaves the
// capability unadvertised rather than half-wired.
func WithRuntimeCommands(log runtimeCommandLog, lease leaseEpochSource) Option {
	return func(s *Session) {
		if log == nil || lease == nil {
			return
		}
		s.runtimeCommands = log
		s.runtimeCommandLease = lease
	}
}

// RuntimeCommands reports whether this session can apply Host-admitted runtime
// commands, and hands back the applier when it can.
//
// This is a SEGREGATED capability, not a method on session.Session, for the same
// reason CommittedPublicEventSource is: almost every Session implementation — every
// test double, the TUI's view, an adapter that only reads — will never apply an
// admitted command, and widening the base contract would force all of them to grow
// a method they cannot honor. The two-result form is what lets a Host adapter learn
// the answer BEFORE it has acknowledged a command as accepted.
func (s *Session) RuntimeCommands() (runtimecommand.Applier, bool) {
	if s.runtimeCommands == nil || s.runtimeCommandLease == nil {
		return nil, false
	}
	return s, true
}

// ApplyRuntimeCommand applies one previously admitted command.
//
// The ordering is the contract. The private application prefix — public CommandID,
// the one RuntimeCommandID it maps to, and the lease epoch — is made durable BEFORE
// any runtime-visible effect. A delivery that finds the prefix already durable
// applied nothing and returns the ORIGINAL disposition; a delivery whose public id
// is durably bound to a DIFFERENT runtime id fails closed.
//
// Harness allocates no identity here. The dispatched command carries the admitted
// RuntimeCommandID verbatim, and the opaque public id is never parsed as a UUID.
//
// A NON-NIL ERROR DOES NOT MEAN THE COMMAND MAY BE RE-OFFERED, and the RETURNED
// DISPOSITION is how a caller tells the two apart:
//
//   - zero Disposition + error: nothing durable was written (validation, a lost or
//     stale lease, a mapping conflict, an unreadable prefix, a failed prefix append).
//     The command is untouched and may be re-offered.
//   - non-zero PrefixSequence + error: the prefix COMMITTED and the effect then
//     failed — an exited loop, a cancelled context. Every later delivery is a
//     duplicate that applies nothing.
//
// The second case is inherent to writing the correlation before the effect, and it
// is the safe direction: the alternative is applying the command twice. Returning
// the earned disposition alongside the error is what keeps it observable without
// forcing a redelivery to find out. See
// TestEffectFailureAfterTheDurablePrefixStrandsTheCommand.
//
// KNOWN GAP — A HOST ADAPTER MUST NOT BLOCK ON ANY APPLICATION SETTLING, of any
// kind. The released settlement correlation resolves a prefix by ADJACENCY: the
// record at prefix+1 must be the public event the command caused, and its resolve()
// switches on that record's envelope kind without ever inspecting the command's
// kind. This applier writes the prefix LAST before the effect, so adjacency holds
// when nothing else is writing — but nothing GUARANTEES it. A concurrent legacy
// Submit's audit intent record, another loop's event in a multi-loop session, or a
// checkpoint landing in that slot resolves an INPUT unresolved exactly as readily as
// an interrupt. The interrupt is merely the kind that is always in that position: it
// has no guaranteed public event at all, and an idle interrupt is fail-quiet.
//
// Unresolved never licenses a rejection, so this is liveness and not correctness —
// which is precisely why it is written down: nothing fails, the command simply never
// settles, and a caller that waits for it waits forever. Closing it needs either a
// guaranteed durable effect record per kind or the prefix-and-effect pair serialized
// against every other append; both are decisions about the public event vocabulary
// and the writer's admission rather than about this seam, so neither is made here.
// pkg/sessionstore's TestAdjacencyIsNotGuaranteedForAnyCommandKind measures the
// consequence for both kinds and holds the semantics still while it waits.
func (s *Session) ApplyRuntimeCommand(ctx context.Context, admitted runtimecommand.Admitted) (runtimecommand.Disposition, error) {
	log, lease := s.runtimeCommands, s.runtimeCommandLease
	if log == nil || lease == nil {
		return runtimecommand.Disposition{}, &runtimecommand.CapabilityUnavailableError{CommandID: admitted.CommandID}
	}
	if err := admitted.Validate(); err != nil {
		return runtimecommand.Disposition{}, err
	}
	// Fail-secure: a faulted session admits no new work, checked before the prefix so
	// a refused session leaves no durable trace of a command it never applied.
	if err := s.faultIfFaulted(); err != nil {
		return runtimecommand.Disposition{}, err
	}
	if !lease.Valid() {
		return runtimecommand.Disposition{}, &runtimecommand.LeaseLostError{CommandID: admitted.CommandID, Epoch: lease.Epoch()}
	}
	if current := lease.Epoch(); admitted.LeaseEpoch != current {
		return runtimecommand.Disposition{}, &runtimecommand.StaleLeaseEpochError{
			CommandID: admitted.CommandID, Admitted: admitted.LeaseEpoch, Current: current,
		}
	}
	// An attempt-bearing command needs a log that can record its disposition, and the
	// check is made BEFORE the prefix deliberately. A command applied with no evidence
	// is unsettleable by anybody — absence is not a terminal outcome, and not_applied
	// would be refused over a committed effect — so it would sit applying forever.
	// A refusal here is a state Host can act on.
	dispositions, hasDispositions := log.(dispositionLog)
	if admitted.AttemptID != "" && !hasDispositions {
		return runtimecommand.Disposition{}, &DispositionUnsupportedError{
			CommandID: admitted.CommandID, AttemptID: admitted.AttemptID,
		}
	}

	// For an input the audit intent record is appended FIRST, so that the application
	// prefix is the LAST append before the effect. The released settlement correlation
	// resolves a prefix by adjacency — the record at prefix+1 must be the public event
	// the command caused — and the intent record sitting between them resolved every
	// input command as UNRESOLVED. Auditing first costs nothing: the intent log is
	// audit-only and swallows its own failures, and a crash between the two leaves an
	// orphan audit record, which is a state it already tolerates.
	var pending *pendingInput
	if admitted.Kind == runtimecommand.KindInput {
		var prepErr error
		pending, prepErr = s.prepareAdmittedInput(ctx, admitted)
		if prepErr != nil {
			return runtimecommand.Disposition{}, prepErr
		}
	}

	res, err := log.AppendCommandApplication(ctx, admitted.Application())
	if err != nil {
		return s.resolveApplicationConflict(ctx, log, admitted, err)
	}
	if !res.Appended {
		// A byte-identical prefix is already durable: same public id, same runtime id,
		// same epoch. The command was applied; replay the original disposition.
		return runtimecommand.Disposition{
			CommandID:        admitted.CommandID,
			RuntimeCommandID: admitted.RuntimeCommandID,
			PrefixSequence:   res.Sequence,
			Duplicate:        true,
		}, nil
	}

	disposition := runtimecommand.Disposition{
		CommandID:        admitted.CommandID,
		RuntimeCommandID: admitted.RuntimeCommandID,
		PrefixSequence:   res.Sequence,
	}
	// Past this point the prefix IS durable, so every return carries the disposition
	// alongside whatever error the effect raises. That is what makes "the prefix
	// committed" observable without a redelivery: a zero Disposition with an error
	// means nothing was written and the command may be re-offered; a non-zero
	// PrefixSequence with an error means it was written and every later delivery will
	// deduplicate against it.
	// THE PER-KIND DISPOSITION TABLE. Each arm states what the runtime observed
	// SYNCHRONOUSLY, which is the only thing a separate post-effect frame can honestly
	// report: an append takes exactly one record, so no effect is ever inside the
	// disposition's frame.
	//
	//   input,     sendUserInput returned nil          -> applied
	//   interrupt, fan-out completed, any == true      -> applied
	//   interrupt, fan-out completed, any == false     -> no_op   (a SUCCESS)
	//   either,    the effect failed after the prefix  -> refused
	//
	// The refused arm is not symmetry. It is the only terminal answer available to a
	// command whose effect failed under a STILL-LIVE lease: not_applied requires a
	// strictly later grant, and a healthy Host never turns its lease over.
	switch admitted.Kind {
	case runtimecommand.KindInput:
		if _, err := s.sendUserInput(ctx, pending.backend, pending.cmd); err != nil {
			return disposition, errors.Join(err, s.recordDisposition(ctx, dispositions, admitted, runtimecommand.DispositionRefused))
		}
		if err := s.recordDisposition(ctx, dispositions, admitted, runtimecommand.DispositionApplied); err != nil {
			return disposition, err
		}
	case runtimecommand.KindInterrupt:
		// The interrupt is the kind that is ALWAYS in the no-adjacency position rather
		// than occasionally — an idle interrupt is fail-quiet and appends no public
		// event at all — but the gap itself is general; see the method doc.
		interrupted, err := s.Interrupt(ctx)
		if err != nil {
			return disposition, errors.Join(err, s.recordDisposition(ctx, dispositions, admitted, runtimecommand.DispositionRefused))
		}
		disposition.Interrupted = interrupted
		outcome := runtimecommand.DispositionNoOp
		if interrupted {
			outcome = runtimecommand.DispositionApplied
		}
		if err := s.recordDisposition(ctx, dispositions, admitted, outcome); err != nil {
			return disposition, err
		}
	default:
		// Unreachable: Validate rejects every other kind. The prefix is already
		// durable, so this reports it rather than pretending nothing happened.
		return disposition, &runtimecommand.ValidationError{Field: "Kind", Reason: "no runtime dispatch path"}
	}
	return disposition, nil
}

// resolveApplicationConflict classifies an append failure. Only an idempotency
// collision is interesting: it means the public CommandID is already durable under
// a DIFFERENT persisted mapping. Reading that durable prefix distinguishes the two
// cases the collision conflates — a redelivery of the SAME command under a new lease
// epoch (a duplicate, which must replay the original disposition) from the same
// public id bound to a DIFFERENT RuntimeCommandID (a conflict, which must fail
// closed). Every other error propagates unchanged, with no effect applied.
func (s *Session) resolveApplicationConflict(
	ctx context.Context,
	log runtimeCommandLog,
	admitted runtimecommand.Admitted,
	appendErr error,
) (runtimecommand.Disposition, error) {
	var collision *journal.IdempotencyCollisionError
	if !errors.As(appendErr, &collision) {
		return runtimecommand.Disposition{}, appendErr
	}
	durable, err := log.ReadCommandApplicationAt(ctx, collision.Seq)
	if err != nil {
		// The durable mapping cannot be read, so the command cannot be classified.
		// Refusing is the only safe answer: applying it could apply it twice.
		return runtimecommand.Disposition{}, &runtimecommand.MappingConflictError{
			CommandID:        admitted.CommandID,
			RuntimeCommandID: admitted.RuntimeCommandID,
			Sequence:         collision.Seq,
			Cause:            err,
		}
	}
	// Kind is part of the comparison because it is part of the CORRELATION the
	// released reader performs: it resolves a prefix whose kind disagrees with the
	// inbox record's as CONFLICTED. Omitting it here would leave Harness reporting
	// Duplicate=true — already applied — for exactly the shape the counterparty fails
	// closed on, so the two authorities would disagree about the same durable record.
	if durable.CommandID != admitted.CommandID ||
		durable.RuntimeCommandID != admitted.RuntimeCommandID ||
		durable.Kind != admitted.Kind {
		return runtimecommand.Disposition{}, &runtimecommand.MappingConflictError{
			CommandID:        admitted.CommandID,
			RuntimeCommandID: admitted.RuntimeCommandID,
			DurableRuntimeID: durable.RuntimeCommandID,
			Kind:             admitted.Kind,
			DurableKind:      durable.Kind,
			Sequence:         collision.Seq,
			Cause:            appendErr,
		}
	}
	// Same public id, same runtime id, a different lease epoch: this is the SAME
	// command redelivered after a lease handover. It was already applied.
	return runtimecommand.Disposition{
		CommandID:        admitted.CommandID,
		RuntimeCommandID: durable.RuntimeCommandID,
		PrefixSequence:   collision.Seq,
		Duplicate:        true,
	}, nil
}

var (
	_ runtimecommand.Provider = (*Session)(nil)
	_ runtimecommand.Applier  = (*Session)(nil)
	// The closer is SEGREGATED and discovered by assertion, following
	// session.LeaseEpochReporter: it is not a method on Applier, so none of that
	// interface's implementers grow a method they cannot honor.
	_ runtimecommand.AttemptCloser = (*Session)(nil)
	_ leaseEpochSource             = (journal.Lease)(nil)
)

// ZeroSuppliedCommandIDError reports that a supplied-id submit was handed the zero
// UUID. It is deliberately NOT SessionError{SessionIDGenerationFailed}: nothing was
// generated on that path, so that kind would send a reader looking for a crypto/rand
// failure that never happened. Reaching it at all is a caller bug — every production
// path validates the admitted record first — so it names the real condition.
type ZeroSuppliedCommandIDError struct{}

func (*ZeroSuppliedCommandIDError) Error() string {
	return "sessionruntime: a supplied-id submit requires a non-zero command id"
}

// logUnavailableRuntimeCommands records why a composition root could not advertise
// the runtime-command capability. The root cannot FAIL on this — a session whose
// journal is not idempotent is still a perfectly good session for every other
// consumer, and Host learns ok=false before it acknowledges anything — but it must
// not swallow it either.
//
// The two causes are categorically different and the log says which. A
// *journal.NonIdempotentJournalError is a genuine capability absence: this
// deployment's journal cannot deduplicate, so it declines the contract. Anything
// else — today, sessionName rejecting the session id — is a WIRING BUG that would
// otherwise present identically, as a deployment that silently applies no commands
// with no error anywhere to explain it.
func logUnavailableRuntimeCommands(ctx context.Context, sessionID uuid.UUID, err error) {
	var nonIdempotent *journal.NonIdempotentJournalError
	if errors.As(err, &nonIdempotent) {
		slog.InfoContext(ctx, "session: runtime-command capability unavailable (journal cannot deduplicate); admitted commands cannot be applied to this session",
			"session", sessionID, "err", err)
		return
	}
	slog.ErrorContext(ctx, "session: runtime-command capability unavailable for an unexpected reason (wiring bug); admitted commands cannot be applied to this session",
		"session", sessionID, "err", err)
}

// pendingInput is one admitted input built and audited, held until its application
// prefix is durable and it can be sent.
type pendingInput struct {
	backend loop.Backend
	cmd     command.UserInput
}

// prepareAdmittedInput resolves the target loop and builds the admitted input,
// appending its audit-only intent record. It performs NO runtime-visible effect: the
// command has not been handed to the loop when this returns, so a caller that then
// fails to persist the application prefix has still applied nothing.
//
// COST OF AUDITING FIRST, recorded because the reorder introduced it. Every refusal
// that happens AFTER this point — a mapping conflict, a lost or stale lease, a failed
// prefix append — leaves an intent record for a command that was never dispatched.
// Those orphans are unbounded under repeated refusal: a Host retrying a conflicting
// mapping appends one per attempt, and each carries a distinct runtime id, so they do
// not deduplicate with one another. Nothing reads them as evidence of application —
// the prefix is the evidence and none was written — so the cost is ledger volume, not
// correctness, and it sits inside the intent log's stated audit-only tolerance. It is
// bounded in practice only by Host's retry policy, which is worth knowing before that
// policy is written.
//
// The alternative is worse. Auditing after the prefix puts a runtime-control record
// in the slot the released settlement correlation reads as the effect, which resolved
// EVERY input command UNRESOLVED — an always-on liveness defect traded for a
// bounded-volume one that only a misbehaving caller triggers.
//
// The command carries the admitted RuntimeCommandID verbatim — the whole point of
// the seam — and the loop-exited and loop-missing refusals are the same ones the
// ordinary submit path raises, checked here so they are raised BEFORE the prefix is
// written rather than after.
func (s *Session) prepareAdmittedInput(ctx context.Context, admitted runtimecommand.Admitted) (*pendingInput, error) {
	if admitted.RuntimeCommandID.IsZero() {
		return nil, &ZeroSuppliedCommandIDError{}
	}
	s.loopsMu.RLock()
	active := s.activeLoopID
	s.loopsMu.RUnlock()
	l, ok := s.loopFor(active)
	if !ok {
		return nil, &SessionError{Kind: SessionLoopNotFound}
	}
	if l == nil {
		return nil, &SessionError{Kind: SessionLoopExited}
	}
	select {
	case <-l.DoneChan():
		// Refuse an exited loop BEFORE the prefix is durable. Discovering it after
		// would strand the command: the prefix would deduplicate every redelivery of a
		// command that was never applied.
		return nil, &SessionError{Kind: SessionLoopExited}
	default:
	}
	cmd := s.buildAndAuditUserInput(ctx, active, admitted.Blocks, identity.AgencyUser, false, admitted.RuntimeCommandID)
	return &pendingInput{backend: l, cmd: cmd}, nil
}
