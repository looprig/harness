package command

// SetSecurityCeiling is the journaled, replayable command that clamps the session's
// SECURITY CEILING — the ordinal upper bound on how permissive auto-approval may be
// (SPEC §8/§10.2). It is a plain-JSON command (every field round-trips through
// encoding/json), following the ApproveToolCall pattern. Harness treats Level as an
// ORDINAL ONLY (0 = most restrictive); the consumer maps the ordinal to a mode/posture,
// so harness stays mode-agnostic.
//
// Applying it is NEVER a bare setter: the session updates its live ceiling ordinal AND
// emits an event.SecurityCeilingChanged, so the change is durable, auditable, and
// deterministically replayable (fold the emitted events, last write wins). The clamp
// takes effect on the NEXT permission Check — the checker reads the live ordinal per
// Check — while an already-spawned tool call keeps its spawn-time policy (the ceiling is
// read at Check time, never mid-spawn).
type SetSecurityCeiling struct {
	Header
	// Level is the requested ceiling ordinal (0 = most restrictive). The session clamps
	// it to any operator-configured maximum on apply; the emitted SecurityCeilingChanged
	// carries the effective (post-clamp) ordinal. This is the operator's intent as
	// recorded in the audit intent log.
	Level uint8 `json:"level"`
}

func (SetSecurityCeiling) isCommand() {}
