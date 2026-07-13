package event

import "github.com/looprig/harness/pkg/ceiling"

// SecurityCeilingChanged records that the session's SECURITY-CEILING ordinal changed to
// Level at this point in the event order (SPEC §8/§10.2). It is the durable, auditable,
// replayable effect of applying a command.SetSecurityCeiling: folding these events on
// restore reproduces the live ceiling — LAST WRITE WINS. It is session-scoped and
// Enduring (never silently dropped): Header.SessionID is set; LoopID/TurnID/StepID are
// zero. Harness treats Level as an ORDINAL ONLY (0 = most restrictive); the consumer maps
// it to a mode.
type SecurityCeilingChanged struct {
	enduring
	sessionScoped
	Header
	// Level is the EFFECTIVE ceiling ordinal after apply (post-clamp), 0 = most
	// restrictive. omitzero is deliberately NOT used: level 0 (the most-restrictive
	// clamp) is a meaningful, must-be-recorded value, so it always rides on the wire.
	Level ceiling.Level `json:"level"`
}

func (SecurityCeilingChanged) isEvent() {}
