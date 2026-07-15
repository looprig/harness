package command

import "github.com/looprig/harness/pkg/security"

// SetSecurityLimit is the journaled, replayable command that clamps the session's
// security limit, the ordinal upper bound on how permissive auto-approval may be
// (SPEC §8/§10.2). It is a plain-JSON command (every field round-trips through
// encoding/json), following the ApproveToolCall pattern. Harness treats Level as an
// ORDINAL ONLY (0 = most restrictive); the consumer maps the ordinal to a mode/posture,
// so harness stays mode-agnostic.
//
// Applying it is NEVER a bare setter: the session updates its live security limit ordinal AND
// emits an event.SecurityLimitChanged, so the change is durable, auditable, and
// deterministically replayable (fold the emitted events, last write wins). The clamp
// takes effect on the NEXT permission Check — the checker reads the live ordinal per
// Check — while an already-spawned tool call keeps its spawn-time policy (the security limit is
// read at Check time, never mid-spawn).
type SetSecurityLimit struct {
	Header
	// Level is the requested security limit ordinal (0 = most restrictive). The session clamps
	// it to any operator-configured maximum on apply; the emitted SecurityLimitChanged
	// carries the effective (post-clamp) ordinal. This is the operator's intent as
	// recorded in the audit intent log.
	Level security.Level `json:"level"`
}

func (SetSecurityLimit) isCommand() {}
