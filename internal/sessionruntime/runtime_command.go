package sessionruntime

import (
	"context"
	"errors"
	"log/slog"

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
// honour. The scan looks for an enduring event caused by that runtime command
// ANYWHERE in the journal and refuses when it finds one — see the AttemptCloser
// contract for why position is not part of the predicate — and it FAILS CLOSED on a
// frame it cannot decode, because a record the walk could not read is not evidence
// that no effect exists.
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
		return runtimecommand.ClosureResult{}, &runtimecommand.DispositionUnsupportedError{CommandID: c.CommandID, AttemptID: c.AttemptID}
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
		return runtimecommand.Disposition{}, &runtimecommand.DispositionUnsupportedError{
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
	//
	// A CREATE carrying a first message takes the same path, and the gate is the
	// PAYLOAD rather than the kind: a create with no blocks and a restore send
	// nothing, so they must not borrow the input path's refusals — a resident
	// session's resume refused because some loop had exited would be an
	// unsettleable command for no reason.
	var pending *pendingInput
	if admitted.Kind == runtimecommand.KindInput ||
		(admitted.Kind == runtimecommand.KindCreate && len(admitted.Blocks) > 0) {
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
	//   input,         the loop took it and committed `applied`   -> applied
	//                  BEFORE queueing it (applyAdmittedInput)
	//   input,         the loop declined it before the commit     -> refused
	//   interrupt,     fan-out completed, any == true            -> applied
	//   interrupt,     fan-out completed, any == false           -> no_op   (a SUCCESS)
	//   gate_response, the answer's GateResolved is durable      -> applied
	//   gate_response, no such gate / gate not open              -> no_op   (a SUCCESS)
	//   gate_response, any other refusal (action, source, append) -> refused
	//   create,        the first message was taken, as an input  -> applied
	//   create,        it carried no first message               -> applied
	//   restore,       nothing to send                           -> applied
	//   input/create/interrupt, the effect failed after the prefix -> refused
	//
	// WHY CREATE AND RESTORE ARE applied AND NOT no_op even when they send nothing.
	// no_op is "the runtime accepted this and it had no effect" — an interrupt of an
	// idle session. These two are the opposite: their effect is RESIDENCY, and by the
	// time either reaches this seam Host has already made the session resident, which
	// is why the command was dispatched at all. Recording no_op would say the runtime
	// found nothing to do about a session that exists because of this command. Both
	// spellings settle the record as applied, so the choice does not change the
	// settlement — it changes what the durable record says, which is the only account
	// an operator reading a journal has.
	//
	// The refused arm is not symmetry. It is the only terminal answer available to a
	// command whose effect failed under a STILL-LIVE lease: not_applied requires a
	// strictly later grant, and a healthy Host never turns its lease over.
	switch admitted.Kind {
	case runtimecommand.KindInput:
		if err := s.applyAdmittedInput(ctx, dispositions, admitted, pending); err != nil {
			return disposition, err
		}
	case runtimecommand.KindCreate:
		// The session is already resident; what is left to apply is the first message,
		// if the create carried one. It goes through the SAME send an input uses, under
		// the same audit-intent-first ordering (prepared above), so its events correlate
		// to the admitted runtime id and the prefix stays the last record before the
		// effect. A create with no first message applies nothing and still settles
		// applied — see the table above for why that is not a no_op.
		if pending != nil {
			if err := s.applyAdmittedInput(ctx, dispositions, admitted, pending); err != nil {
				return disposition, err
			}
			break
		}
		if err := s.recordDisposition(ctx, dispositions, admitted, runtimecommand.DispositionApplied); err != nil {
			return disposition, err
		}
	case runtimecommand.KindRestore:
		// A restore carries no payload — Admitted.Validate refuses one — and the
		// session it resumes already holds its conversation, so there is no effect to
		// perform here beyond recording that this runtime took the command.
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
	case runtimecommand.KindGateResponse:
		// The caller-facing gate path, not the core: RespondGate's refusals — the
		// classifier-provenance one above all — apply to an admitted answer exactly
		// as they apply to a direct call. The answer's GateResolved is appended
		// durably BEFORE this frame, and carries the runtime id in its Cause; see
		// gateResponseDisposition for the ordering argument.
		outcome, err := gateResponseDisposition(
			s.respondGateAsCaller(ctx, *admitted.GateResponse, admitted.RuntimeCommandID))
		if err != nil {
			return disposition, errors.Join(err, s.recordDisposition(ctx, dispositions, admitted, outcome))
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

// applyAdmittedInput hands an admitted input (an input, or a create's first
// message) to its loop and records its disposition.
//
// UNDER A DISPOSITION ATTEMPT THE `applied` RECORD IS WRITTEN BY THE LOOP ACTOR, not
// after the send. The input carries a command.Admission whose Commit appends the
// applied disposition; the actor decides on its own live state, calls Commit, and
// only then queues or starts the input. That ordering is what the disposition means:
//
//   - `applied` is durable strictly BEFORE any effect the input can cause, so a
//     successor's recovery scan never finds an effect with no disposition behind it
//     for this kind, and never has to refuse a closure over one;
//   - an input `applied` names is one the runtime durably OWES: if the runtime dies
//     before the input's turn is durable, restore replays it (see
//     replayAppliedAdmittedInputs), and a loop going away carries it over rather than
//     cancelling it. v0.36.0 wrote `applied` after handing the input to an in-memory
//     inbox and lost it on exactly that crash.
//
// A loop that declines before Commit (shutting down, queue full, an admission fault)
// leaves no effect, so the command is refused under the live grant. A Commit that
// fails leaves no effect either, and nothing else is written: the append may have
// landed ambiguously, and a successor settles it from what is durable — a landed
// `applied` is replayed, and an absent one is closed not_applied.
//
// A legacy admitted record (no attempt id) keeps its released path exactly: it has
// no disposition to order and nothing to replay from, so it is sent and left.
func (s *Session) applyAdmittedInput(
	ctx context.Context,
	dispositions dispositionLog,
	admitted runtimecommand.Admitted,
	pending *pendingInput,
) error {
	if admitted.AttemptID == "" || dispositions == nil {
		_, err := s.sendUserInput(ctx, pending.backend, pending.cmd)
		return err
	}
	var commitErr error
	var committed bool
	result := make(chan error, 1)
	cmd := pending.cmd
	cmd.Admission = &command.Admission{
		Commit: func() error {
			committed = true
			commitErr = s.recordDisposition(ctx, dispositions, admitted, runtimecommand.DispositionApplied)
			return commitErr
		},
		Result: result,
	}
	if _, err := s.sendUserInput(ctx, pending.backend, cmd); err != nil {
		return errors.Join(err, s.recordDisposition(ctx, dispositions, admitted, runtimecommand.DispositionRefused))
	}
	// The actor answers in the same step that received the command, so once the send
	// succeeded the answer is already on its way. The loop's exit is watched only
	// against a defect; the caller's ctx is deliberately NOT, because abandoning the
	// wait would report a failure for an input the actor may have just made durable.
	var answer error
	select {
	case answer = <-result:
	case <-pending.backend.DoneChan():
		select {
		case answer = <-result:
		default:
			return &SessionError{Kind: SessionLoopExited}
		}
	}
	switch {
	case answer == nil:
		return nil
	case committed:
		// Commit ran and failed: the actor dropped the input. See the doc above.
		return commitErr
	default:
		return errors.Join(answer, s.recordDisposition(ctx, dispositions, admitted, runtimecommand.DispositionRefused))
	}
}

// gateResponseDisposition maps the gate path's answer onto the disposition
// vocabulary, returning the error the applier must still report (nil for the two
// successful outcomes).
//
//   - nil: the GateResolved append COMMITTED — respondGateCore returns nil only
//     after it — so the answer landed. applied.
//
//   - GateNotFound / GateNotReady: there is no open gate to answer. The gate was
//     already resolved (by this or another answer, a timeout, or its owner), never
//     existed, or is not yet or no longer answerable. Nothing was appended, and a
//     retry cannot change that, so this is the successful no-effect outcome, exactly
//     as an idle interrupt is. no_op.
//
//     CAVEAT, bounded: NotReady includes "another answer holds the claim". If that
//     answer's GateResolved append then fails, it reverts the gate to open, and this
//     command has already settled no_op — the gate is answerable again only under a
//     new command. The bound is that the failing append went through the checked hub
//     path, which faults the session (SessionPersistenceFault), so a faulted session
//     admits no further runtime command and the reopened gate is not answerable here.
//
//   - anything else — an invalid action or values, a classifier-provenance source,
//     a gate kind with no answer path, a GateResolved append failure: this runtime
//     did not accept the answer. refused.
//
// WHY THE APPEND-FAILURE ARM CANNOT CONTRADICT A LANDED ANSWER. A refused frame
// after a GateResolved append error is a lie only if the GateResolved actually
// landed. The store-backed journal appends under compare-and-swap on the writer's
// tracked tip and does not advance that tip on ANY error, ambiguous ones included
// (sessionstore.writeEncodedLocked). If the GateResolved landed despite the error,
// its frame occupies the slot the refused frame's CAS names, so the refused append
// fails and nothing contradictory is written; the command is then left for a
// successor, whose recovery scan finds the landed answer by its Cause and refuses to
// tombstone it. If it did not land, refused is the truth.
//
// The in-memory gate DOES reopen in the landed case (revertClaiming), so the
// directory briefly disagrees with the journal. That is bounded the same way: the
// failed append faulted the session, the stale tracked tip makes every further
// GateResolved append fail at its CAS, and a runtime command is refused before its
// prefix. TestAmbiguousGateResolvedAppendNeverContradictsTheJournal pins both modes
// at the storage ledger.
func gateResponseDisposition(err error) (runtimecommand.DispositionKind, error) {
	if err == nil {
		return runtimecommand.DispositionApplied, nil
	}
	var gateErr *GateError
	if errors.As(err, &gateErr) && (gateErr.Kind == GateNotFound || gateErr.Kind == GateNotReady) {
		return runtimecommand.DispositionNoOp, nil
	}
	return runtimecommand.DispositionRefused, err
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
	if admitted.AttemptID == "" {
		cmd := s.buildAndAuditUserInput(ctx, active, admitted.Blocks, identity.AgencyUser, false, admitted.RuntimeCommandID)
		return &pendingInput{backend: l, cmd: cmd}, nil
	}
	cmd := command.UserInput{
		Header: command.Header{CommandID: admitted.RuntimeCommandID, Agency: identity.AgencyUser, CreatedAt: s.stampNow()},
		Blocks: admitted.Blocks,
	}
	if err := s.appendAdmittedIntent(ctx, active, cmd); err != nil {
		return nil, err
	}
	return &pendingInput{backend: l, cmd: cmd}, nil
}

// appendAdmittedIntent is the LOAD-BEARING intent append for an input admitted under
// a disposition attempt. The ordinary intent log is audit-only and swallows its
// failures; this one cannot, because the record is the only durable copy of the
// input's blocks in the session journal, and it is what restore re-offers when the
// input was settled `applied` but its turn never became durable. A failure here
// refuses the command BEFORE the prefix, so nothing durable names it and it may be
// re-offered.
//
// A collision on the record's id is NOT a failure. The id is the store-issued
// runtime command id, so a record already durable under it is this same input from
// an earlier delivery — only its CreatedAt differs — and that record is the one
// restore will read. The delivery proceeds to the prefix, which deduplicates it.
func (s *Session) appendAdmittedIntent(ctx context.Context, loopID uuid.UUID, cmd command.UserInput) error {
	if s.cmdAppender == nil {
		return nil
	}
	err := s.cmdAppender.AppendCommand(ctx, journal.NewCommandRecord(s.sessionID, loopID, cmd))
	var collision *journal.IdempotencyCollisionError
	if err == nil || errors.As(err, &collision) {
		return nil
	}
	return &AdmittedIntentAppendError{CommandID: cmd.CommandID, Cause: err}
}

// AdmittedIntentAppendError reports that the durable intent record of an input
// admitted under a disposition attempt could not be written. Nothing durable names
// the command, so it may be re-offered.
type AdmittedIntentAppendError struct {
	CommandID uuid.UUID
	Cause     error
}

func (e *AdmittedIntentAppendError) Error() string {
	return "sessionruntime: admitted input intent append failed for " + e.CommandID.String() + ": " + e.Cause.Error()
}

func (e *AdmittedIntentAppendError) Unwrap() error { return e.Cause }
