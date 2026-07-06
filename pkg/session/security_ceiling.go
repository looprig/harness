package session

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
// Order is apply-then-emit so the clamp is live before the durable record is written (§8's
// "takes effect immediately"). It is operator-originated (Agency=AgencyUser). Deterministic
// on replay: folding the emitted SecurityCeilingChanged events reproduces the exact ordinal
// (last write wins). A faulted session (a required durable append already failed) refuses
// it, fail-secure, like every other new-work entry point.
func (s *Session) SetSecurityCeiling(ctx context.Context, level uint8) error {
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

	// Apply FIRST so the clamp is live on the very next Check (§8), then emit the durable
	// event that makes the change auditable and replayable. Set clamps to any configured
	// maximum and returns the EFFECTIVE ordinal the event carries, so a replay fold
	// reproduces the exact live value.
	effective := s.applyCeiling(level)

	stamped, err := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: s.SessionID}})
	if err != nil {
		return &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}
	if err := s.PublishEvent(ctx, event.SecurityCeilingChanged{Header: stamped, Level: effective}); err != nil {
		return &SessionError{Kind: SessionContextDone, Cause: err}
	}
	return nil
}

// applyCeiling sets the session's live ceiling ordinal and returns the effective
// (post-clamp) value. The state is always non-nil in a constructed session (New/Restore
// default-mint it); the nil-guard only covers a struct-literal test session (single
// goroutine, so no race).
func (s *Session) applyCeiling(level uint8) uint8 {
	if s.ceiling == nil {
		s.ceiling = ceiling.New()
	}
	return s.ceiling.Set(level)
}

// CeilingSource returns the session's live security-ceiling source (READ-only): the same
// ordinal SetSecurityCeiling mutates. The composition root wires this into a permission
// checker via tools.WithCeilingPostures (structural — *ceiling.State satisfies both
// ceiling.Source and tools.CeilingSource), so posture selection and this session's ceiling
// never disagree. It is always non-nil for a constructed session. When the composition
// root injected its own source via WithCeiling, this returns that SAME instance (the
// checker and the session already share it).
func (s *Session) CeilingSource() ceiling.Source {
	if s.ceiling == nil {
		s.ceiling = ceiling.New()
	}
	return s.ceiling
}
