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
	switch admitted.Kind {
	case runtimecommand.KindInput:
		if _, err := s.sendUserInput(ctx, pending.backend, pending.cmd); err != nil {
			return disposition, err
		}
	case runtimecommand.KindInterrupt:
		// The interrupt is the kind that is ALWAYS in the no-adjacency position rather
		// than occasionally — an idle interrupt is fail-quiet and appends no public
		// event at all — but the gap itself is general; see the method doc.
		interrupted, err := s.Interrupt(ctx)
		if err != nil {
			return disposition, err
		}
		disposition.Interrupted = interrupted
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
	if durable.CommandID != admitted.CommandID || durable.RuntimeCommandID != admitted.RuntimeCommandID {
		return runtimecommand.Disposition{}, &runtimecommand.MappingConflictError{
			CommandID:        admitted.CommandID,
			RuntimeCommandID: admitted.RuntimeCommandID,
			DurableRuntimeID: durable.RuntimeCommandID,
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
	_ leaseEpochSource        = (journal.Lease)(nil)
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
