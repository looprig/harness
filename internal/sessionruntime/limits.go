package sessionruntime

import "github.com/looprig/harness/pkg/loop"

// Child-agent spawn safety caps. They are the two independent backstops against a runaway
// agent tree: Depth bounds how DEEP the spawn chain can nest (a sub-loop spawning a
// sub-loop spawning a sub-loop…), and Quota bounds the TOTAL number of sub-loops a
// single session may ever spawn (a fan-out bound across the whole tree). Both are
// in-session, per-session limits — they do not span sessions.
const (
	// defaultDepth caps a sub-loop's ANCESTOR CHAIN LENGTH at 3 (design §6d): NewLoop
	// refuses a spawn whose parent chain already has this many registered ancestors, so
	// the deepest spawnable loop has an ancestor chain of defaultDepth-1 (2 levels of
	// sub-loops below the primary). The designed tree is depth-1 (operator → leaves that
	// can't spawn), so this is a backstop against unbounded recursion, not the normal
	// shape. The primary loop itself has no ancestors (chain length 0).
	defaultDepth = 3

	// defaultQuota caps the TOTAL sub-loops one session may spawn over its whole lifetime
	// at 64 — generous for legitimate fan-out (many parallel workers) while still bounding
	// a pathological spawn loop. The primary loop does NOT count (it is built by New, not
	// the quota-counted NewLoop spawn path).
	defaultQuota = 64
)

// Limits are the in-session agent-spawn safety caps applied by NewLoop: Depth bounds
// the spawn-chain nesting and Quota bounds the total sub-loops a session may spawn. A zero
// (or invalid negative — a wiring slip) field adopts the package default via
// withDefaults. An explicit positive value overrides it. Only Quota accepts
// loop.Unlimited to disable that cap; Depth stays bounded.
type Limits struct {
	// Depth is the maximum spawn-chain nesting depth (sub-loops below the primary). Zero
	// → defaultDepth. A spawn whose parent chain is already this deep is refused.
	Depth int

	// Quota is the maximum total number of sub-loops the session may spawn over its
	// lifetime. Zero → defaultQuota; loop.Unlimited disables the cap. A spawn once
	// this many have been reserved is refused.
	Quota int
}

// withDefaults replaces zero or invalid negative fields with their defaults. It
// preserves an explicit loop.Unlimited quota. The live Depth is always positive;
// the live Quota is either positive or loop.Unlimited.
func (l Limits) withDefaults() Limits {
	if l.Depth <= 0 {
		l.Depth = defaultDepth
	}
	if l.Quota <= 0 && l.Quota != loop.Unlimited {
		l.Quota = defaultQuota
	}
	return l
}
