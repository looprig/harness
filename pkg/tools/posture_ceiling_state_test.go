package tools

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/loop"
)

// *ceiling.State (the Task-18 journaled ceiling holder) satisfies the Task-16
// CeilingSource contract structurally — harness never shares a concrete type between
// the checker and the ceiling package.
var _ CeilingSource = (*ceiling.State)(nil)

// TestCeilingStateClampsNextCheck proves the journaled security-ceiling state (Task 18)
// drives the Task-16 posture selection end to end: a checker wired via
// WithCeilingPostures over a REAL *ceiling.State auto-approves at the trusted ordinal,
// and a Set that LOWERS the ordinal flips the very next Check to Ask — the clamp takes
// effect immediately (SPEC §8). An already-decided call (the effect captured before the
// change) is NOT revisited: the ceiling is read per Check, never mid-spawn, so an
// in-flight call keeps its spawn-time policy.
func TestCeilingStateClampsNextCheck(t *testing.T) {
	t.Parallel()
	restrictive := Posture{}                                          // ordinal 0: auto-approves nothing
	trusted := Posture{AutoApproveBash: true, RequiredGuarantees: gA} // ordinal 1: full bash auto
	table := []Posture{restrictive, trusted}

	state := ceiling.NewClamped(1) // operator cap = ordinal 1 (trusted)
	state.Set(1)                   // start trusted (what a prior SetSecurityCeiling applied)
	runner := &fakeGuaranteeRunner{bits: gA}
	c := newPostureChecker(t, PermissionPolicy{}, WithCeilingPostures(state, table, runner))
	bash := plainTool{name: toolBash}
	args := `{"command":"echo hi"}`

	// Trusted ordinal + satisfied interlock -> the call would be auto-approved (spawned).
	inFlight := c.Check(context.Background(), bash, toolBash, args)
	if inFlight != loop.EffectAutoApprove {
		t.Fatalf("ceiling=1 Check() = %v, want EffectAutoApprove", inFlight)
	}

	// Tighten the ceiling via the SAME Set the journaled SetSecurityCeiling applies.
	state.Set(0)

	// The NEXT Check sees the lowered ceiling and declines to auto-approve.
	if got := c.Check(context.Background(), bash, toolBash, args); got != loop.EffectAsk {
		t.Fatalf("ceiling=0 Check() = %v, want EffectAsk (clamped by lowered ceiling)", got)
	}

	// In-flight: the earlier decision is untouched — a spawned call keeps its spawn-time
	// policy (the checker never re-runs a completed Check when the ceiling moves).
	if inFlight != loop.EffectAutoApprove {
		t.Fatalf("in-flight effect mutated to %v; a spawned call must keep its spawn-time policy", inFlight)
	}

	// Raising back within the operator cap re-enables auto-approve; a Set ABOVE the cap
	// clamps to the cap (fail-closed) rather than selecting an out-of-range ordinal.
	state.Set(9)
	if got := c.Check(context.Background(), bash, toolBash, args); got != loop.EffectAutoApprove {
		t.Fatalf("ceiling clamped-to-cap Check() = %v, want EffectAutoApprove", got)
	}
}
