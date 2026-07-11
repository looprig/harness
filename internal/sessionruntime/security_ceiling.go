package sessionruntime

import (
	"context"

	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// SetSecurityCeiling clamps the session's SECURITY CEILING to level — the ordinal upper
// bound on how permissive auto-approval may be (SPEC §8/§10.2). It is the JOURNALED,
// replayable path (never a bare setter): it appends a command.SetSecurityCeiling to the
// audit intent log, APPLIES it to the session's live ceiling ordinal, and emits a durable
// event.SecurityCeilingChanged. Harness treats level as an ORDINAL ONLY (0 = most
// restrictive); the consumer maps it to a mode.
//
// The clamp takes effect on the NEXT permission Check: the checker (wired via
// tools.WithCeilingPostures over the SAME source this session holds — see WithCeiling /
// CeilingSource) reads the live ordinal per Check, so a change is visible immediately. An
// already-spawned tool call keeps its spawn-time policy — the executor was built per-spawn
// and the ceiling is read only at Check time, never re-read mid-spawn — so tightening the
// ceiling never retro-confines an in-flight call.
//
// Ordering is DIRECTION-DEPENDENT to close the un-persisted-loosening gap:
//
//   - TIGHTENING or no-change (effective <= current): APPLY first, THEN emit. §8 requires
//     a tightening to be live at once; if the emit later faults, a more-restrictive-in-
//     memory ceiling is the SAFE direction, so applying ahead of the durable record is
//     acceptable.
//   - LOOSENING (effective > current, a raise): EMIT first, then apply the raise ONLY if
//     the emit did NOT fault the session. PublishEvent returns nil on a required-append
//     fault (the hub latches it via ReportFault inline), so the fault-latch is checked
//     AFTER the emit. A raise is thus NEVER live in-memory ahead of its durable record —
//     no live window more permissive than the journal, even for the in-flight turn's
//     checker sharing this same source.
//
// It is operator-originated (Agency=AgencyUser). Deterministic on replay: folding the
// emitted SecurityCeilingChanged events reproduces the exact ordinal (last write wins). A
// faulted session (a required durable append already failed) refuses it up front, fail-
// secure, like every other new-work entry point. Returning nil after a faulting emit is
// consistent with the hub's PublishEvent contract (the fault is surfaced out-of-band).
func (s *Session) SetSecurityCeiling(ctx context.Context, level ceiling.Level) error {
	if err := s.faultIfFaulted(); err != nil {
		return err
	}
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	cmd := command.SetSecurityCeiling{
		Header: command.Header{CommandID: id, Agency: identity.AgencyUser, CreatedAt: s.stampNow()},
		Level:  level,
	}
	// Intent log (audit-only): append BEFORE apply/emit; a failure is logged and the change
	// proceeds (a lost record must never block an operator's security action). The ceiling
	// is session-global, so the record is anchored to the primary loop purely as audit
	// metadata — replay reconstructs the ceiling from the emitted events, not this record.
	s.appendCommand(ctx, s.PrimaryLoopID(), cmd)

	// Compute the EFFECTIVE (post-clamp) target and the current ordinal WITHOUT mutating
	// state yet, so the direction decides the apply/emit order. Set(effective) below is a
	// fixed point (effective is already clamped), so re-clamping cannot change it.
	cs := s.ceilingState()
	current := cs.Current()
	effective := cs.Clamp(level)

	stamped, err := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: s.sessionID}})
	if err != nil {
		return &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}
	changed := event.SecurityCeilingChanged{Header: stamped, Level: effective}

	if effective <= current {
		// Tighten / no-change: apply FIRST so the clamp is live on the very next Check (§8),
		// even if the emit later faults (more-restrictive-in-memory is the safe direction).
		cs.Set(effective)
		if err := s.PublishEvent(ctx, changed); err != nil {
			return &SessionError{Kind: SessionContextDone, Cause: err}
		}
		return nil
	}

	// Loosen (a raise): emit FIRST, then apply the raise ONLY if the durable emit did not
	// fault the session — so a raise is never live in-memory ahead of its durable record.
	if err := s.PublishEvent(ctx, changed); err != nil {
		return &SessionError{Kind: SessionContextDone, Cause: err}
	}
	if s.faultIfFaulted() == nil {
		cs.Set(effective)
	}
	return nil
}

// ceilingState returns the session's live ceiling state, default-minting it when nil. The
// state is always non-nil in a constructed session (New/Restore default-mint it); the
// nil-guard only covers a struct-literal test session (single goroutine, so no race).
func (s *Session) ceilingState() *ceiling.State {
	if s.ceiling == nil {
		s.ceiling = ceiling.New()
	}
	return s.ceiling
}

// CeilingSource returns the session's live security-ceiling source (READ-only): the same
// ordinal SetSecurityCeiling mutates. The composition root wires this into a permission
// checker via tools.WithCeilingPostures (structural — *ceiling.State satisfies both
// ceiling.Source and tools.CeilingSource), so posture selection and this session's ceiling
// never disagree. It is always non-nil for a constructed session. When the composition
// root injected its own source via WithCeiling, this returns that SAME instance (the
// checker and the session already share it).
func (s *Session) CeilingSource() ceiling.Source {
	return s.ceilingState()
}
