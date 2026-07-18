// Package security holds the session-scoped security limit ORDINAL — the runtime clamp
// on how permissive auto-approval may be (SPEC §8/§10.2). Harness treats the limit as
// an ordinal ONLY (0 = most restrictive); the mapping ordinal -> mode/posture lives in
// the consumer, so this package stays mode-agnostic and depends on nothing but the
// standard library.
//
// Limit is the single mutable holder shared at the composition root: the session APPLIES
// a journaled command.SetSecurityLimit by calling Set (and emits an
// event.SecurityLimitChanged carrying the effective ordinal Set returns), while a
// permission checker READS the live ordinal via Current on every Check — the checker is
// wired over the SAME Limit so a change takes effect on the very next decision. Because
// those two sides run on different goroutines (a loop Checks while the session applies),
// Limit is safe for concurrent Current/Set.
package security

import "sync/atomic"

// Level is the session security limit ordinal. Its uint8 representation is stable on
// the wire; the name prevents policy APIs from accepting unrelated integers.
type Level uint8

// LimitSource is the READ side of the limit: the live ordinal, read once per Check. It
// is shared by the session controller and permission posture selector, so callers cannot
// accidentally pass an unrelated integer as a policy ordinal.
type LimitSource interface {
	// Current returns the live limit ordinal (0 = most restrictive). It is read on
	// every Check, so a Set is visible on the very next decision.
	Current() Level
}

// Limit is the session-scoped security limit ordinal — the mutable holder the session
// applies SetSecurityLimit to and the permission checker reads per Check. It is safe
// for concurrent Current/Set (a lock-free atomic load/store); the zero value is not
// usable — construct one with New or NewBounded so it starts at the fail-secure
// most-restrictive ordinal.
type Limit struct {
	// current is the live ordinal (0 = most restrictive), stored as a uint32 for
	// lock-free atomic access. The public domain is uint8; the high bytes stay zero.
	current atomic.Uint32
	// max is the OPTIONAL operator upper bound on the ordinal: when hasMax is set, Set
	// never stores a value above it — a journaled command can LOWER the limit or raise
	// it only up to this cap, never past it (fail-secure). hasMax distinguishes "no cap"
	// (store as-is) from a cap whose value happens to be 0 (pin to most restrictive).
	max    Level
	hasMax bool
}

// New returns a Limit at the most-restrictive ordinal (0) with NO upper cap: Set stores
// the requested ordinal as-is. It is the fail-secure default a fresh session starts under
// before any SetSecurityLimit command is applied.
func New() *Limit { return &Limit{} }

// NewBounded returns a Limit at the most-restrictive ordinal (0) whose Set clamps every
// requested ordinal to max — the operator's upper bound on permissiveness. A journaled
// command can then never raise the limit above max (a compromised or replayed command
// cannot exceed the operator's cap), only up to it or below.
func NewBounded(max Level) *Limit { return &Limit{max: max, hasMax: true} }

// Current returns the live limit ordinal (0 = most restrictive). It is safe to call
// concurrently with Set and is the read the permission checker performs on every Check,
// so a Set is visible on the very next Check (the clamp takes effect immediately, §8).
func (s *Limit) Current() Level {
	value := s.current.Load()
	if value > uint32(^Level(0)) {
		return 0
	}
	return Level(value)
}

// Clamp returns level reduced to the configured cap (when one is set), WITHOUT storing
// it — the PURE projection Set applies. The applier uses it to learn the EFFECTIVE
// ordinal (and compare it to Current to decide tighten vs loosen) BEFORE committing the
// change, so the apply/emit order can be chosen by direction. It is safe to call
// concurrently.
func (s *Limit) Clamp(level Level) Level {
	if s.hasMax && level > s.max {
		return s.max
	}
	return level
}

// Set applies a new limit ordinal, clamping to the configured cap when one is set, and
// returns the EFFECTIVE (stored) ordinal. Applying a command.SetSecurityLimit calls
// this; the returned effective ordinal is what event.SecurityLimitChanged carries, so
// folding the emitted events on replay reproduces the exact live ordinal — LAST WRITE
// WINS (a later Set overwrites an earlier one). It is safe to call concurrently with
// Current.
func (s *Limit) Set(level Level) Level {
	eff := s.Clamp(level)
	s.current.Store(uint32(eff))
	return eff
}
