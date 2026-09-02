package sessionruntime

import (
	"context"
	"errors"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
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
	switch admitted.Kind {
	case runtimecommand.KindInput:
		s.loopsMu.RLock()
		active := s.activeLoopID
		s.loopsMu.RUnlock()
		if _, err := s.submitToLoopWithID(ctx, active, admitted.Blocks, identity.AgencyUser, false, admitted.RuntimeCommandID); err != nil {
			return runtimecommand.Disposition{}, err
		}
	case runtimecommand.KindInterrupt:
		interrupted, err := s.Interrupt(ctx)
		if err != nil {
			return runtimecommand.Disposition{}, err
		}
		disposition.Interrupted = interrupted
	default:
		// Unreachable: Validate rejects every other kind. Fail closed rather than
		// reporting an application that never happened.
		return runtimecommand.Disposition{}, &runtimecommand.ValidationError{Field: "Kind", Reason: "no runtime dispatch path"}
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
