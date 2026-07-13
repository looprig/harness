package tools

import (
	"encoding/json"

	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// posture.go implements the posture-driven auto-approve stage (Stage 6.5) and the
// guarantee interlock (SPEC §10.2/§10.3) — the LOAD-BEARING safety gate that lets
// a security mode auto-approve file edits and Bash, but ONLY when the injected OS
// sandbox actually enforces the guarantees the posture requires.
//
// harness stays MODE-AGNOSTIC: it never sees a mode name, only a Posture (what a
// mode implies for the gate) and, for dynamic mode, a stdlib ORDINAL ceiling (§8).
// harness NEVER imports github.com/looprig/sandbox: the runner is held as a
// stdlib-typed tool.CommandRunner and probed STRUCTURALLY for its optional
// GuaranteeBits()/Level() capabilities via type assertion.
//
// FAIL-CLOSED INVARIANT (same family as EffectAsk == 0): no runner, a runner
// without GuaranteeBits, a missing required bit, a Level below the floor, an
// out-of-range ceiling ordinal, or a grant-carrying call all resolve to the SAFE
// direction — the stage declines to auto-approve and the call falls to Stage 7
// (Ask). The stage can ONLY ever auto-approve or decline; it never denies and can
// never override the non-bypassable Stage 1/2 (containment + hard-deny) denies,
// the Stage 3 EffectChecker veto, or a Stage 5/6 persisted/session deny — those
// all run FIRST (Check calls this stage last, just before the default Ask).

// Posture expresses what a security mode implies for the permission gate. It is
// built by the CONSUMER (which knows the mode names) and handed to harness, which
// treats it opaquely. The zero Posture auto-approves nothing (fail-closed default,
// used as the most-restrictive table[0] and when no posture is configured).
type Posture struct {
	// AutoApproveEdits auto-approves file-edit/write tools (WriteFile/EditFile) — but,
	// like Bash, ONLY when the guarantee interlock passes: the held runner must enforce
	// RequiredGuaranteesEdits (and any RequiredLevel floor). Edits are ALSO confined by
	// the non-bypassable write-containment + hard-deny stages and the in-process
	// ReadGuard, but in-process containment alone is not an OS write-boundary — so on a
	// non-enforcing backend (nil / null-backend runner) the interlock fails and edits
	// degrade to Ask, uniform with Bash. A zero RequiredGuaranteesEdits requires nothing
	// (no interlock — the backward-compatible / unconfined choice).
	AutoApproveEdits bool

	// AutoApproveBash auto-approves Bash — but ONLY when the guarantee interlock
	// passes (RequiredGuarantees satisfied by the held runner, and any RequiredLevel
	// floor met). Without an enforcing runner this degrades to Ask.
	AutoApproveBash bool

	// RequiredGuarantees is the guarantee bitmask the runner must enforce for a
	// Bash auto-approve to fire. The interlock passes only when
	// runnerBits & RequiredGuarantees == RequiredGuarantees. A zero mask requires
	// nothing (the consumer's explicit "no interlock" choice, e.g. an unconfined
	// mode); any non-zero mask fails closed against a nil / non-probing runner.
	RequiredGuarantees uint64

	// RequiredGuaranteesEdits is the guarantee bitmask the runner must enforce for an
	// AutoApproveEdits auto-approve to fire — the edit counterpart of RequiredGuarantees
	// (kept SEPARATE because an edit typically needs only the OS write-boundary, NOT the
	// network boundary a Bash auto-approve requires). The interlock passes only when
	// runnerBits & RequiredGuaranteesEdits == RequiredGuaranteesEdits. A zero mask
	// requires nothing — an AutoApproveEdits posture with a zero mask auto-approves edits
	// with no interlock (backward-compatible; the unconfined "no interlock" choice) — but
	// any non-zero mask fails closed against a nil / non-probing runner, degrading edits
	// to Ask exactly as Bash does.
	RequiredGuaranteesEdits uint64

	// RequiredLevel is an OPTIONAL coarse secondary floor on the runner's Level()
	// (SPEC §10.3): the runner's Level() must be >= RequiredLevel. Zero means no
	// floor. Fail-closed: a positive floor with a nil runner, a runner that does not
	// expose Level(), or a Level() below the floor blocks the auto-approve.
	RequiredLevel uint8

	// TrivialBash is the "trivial auto, rest ask" slot (write mode): when
	// AutoApproveBash is false and TrivialBash is non-nil, only commands the
	// classifier deems trivial auto-approve (and only when the interlock passes);
	// every other command falls to Ask. nil means the slot is unused.
	//
	// CONCURRENCY CONTRACT: TrivialBash — like CeilingSource.Current and the
	// runner's GuaranteeBits()/Level() probes — is invoked while the checker's mutex
	// is held during Check. It must be cheap and pure and MUST NOT call back into
	// Check (the mutex is non-reentrant → re-entry deadlocks; mirrors the Stage-3
	// EffectChecker contract).
	TrivialBash func(command string) bool

	// GrantCarryingAlwaysAsk maps a grant-carrying call (a non-empty top-level
	// "grants" array in argsJSON) to Ask: an escalation is never auto-approved by
	// posture — it must be human-reviewed (SPEC §9.3/§10.7). Composes with the
	// Task-17 grant plumbing.
	GrantCarryingAlwaysAsk bool
}

// fieldGrants is the optional top-level Bash arg field carrying escalation grant
// tokens (SPEC §10.7 item 1). Its mere non-empty presence forces human review.
const fieldGrants = "grants"

// CeilingSource is the named ORDINAL source the dynamic posture option reads
// per Check (§8). harness treats the ceiling as an ordinal ONLY (0 = most
// restrictive); the consumer maps ordinal -> Posture via the registered table.
// A session ceiling change (journaled command -> event) simply moves Current().
type CeilingSource interface {
	// Current returns the live ceiling ordinal (0 = most restrictive). It is read
	// on every Check, so a downgrade takes effect on the very next decision.
	//
	// CONCURRENCY CONTRACT: Current is invoked while the checker's mutex is held
	// during Check — it must be cheap and MUST NOT call back into Check (the mutex
	// is non-reentrant → re-entry deadlocks; mirrors the Stage-3 EffectChecker
	// contract).
	Current() ceiling.Level
}

// postureSelector produces the Posture in force for a single Check. A nil selector
// on the checker means NO posture is configured (the pre-existing behavior). It is
// unexported so only the two constructors below can supply one.
type postureSelector interface {
	current() Posture
}

// fixedPosture is the static-mode selector: one Posture for the checker's life.
type fixedPosture struct{ p Posture }

func (f fixedPosture) current() Posture { return f.p }

// ceilingPostures is the dynamic-mode selector: read the live ordinal and select
// table[ordinal]. It is fail-closed — an out-of-range ordinal, a nil source, or an
// empty table resolves to the MOST RESTRICTIVE posture (table[0], or the zero
// Posture when the table is empty), never the most permissive entry.
type ceilingPostures struct {
	src   CeilingSource
	table []Posture
}

func (cp ceilingPostures) current() Posture {
	if len(cp.table) == 0 {
		return Posture{} // no registered postures => auto-approve nothing.
	}
	if cp.src == nil {
		return cp.table[0] // no source => clamp to the most restrictive posture.
	}
	ord := cp.src.Current()
	if int(ord) >= len(cp.table) {
		// Out-of-range ordinal: fail closed to the most restrictive posture. NEVER
		// clamp UP to table[len-1] — that would be the most permissive entry.
		return cp.table[0]
	}
	return cp.table[int(ord)]
}

// WithPosture configures a FIXED-mode posture and the ONE confined runner the
// checker holds (SPEC §10.2). The runner is stored as a stdlib tool.CommandRunner
// and probed STRUCTURALLY for its optional GuaranteeBits()/Level() capabilities —
// harness never imports sandbox. It is the same single reference the Task-17 grant
// plumbing will reuse. A nil runner is valid: the interlock then fails closed for
// any non-zero RequiredGuarantees, so a guarantee-requiring Bash auto-approve
// degrades to Ask.
func WithPosture(p Posture, runner tool.CommandRunner) Option {
	return func(c *checkerConfig) {
		c.posture = fixedPosture{p: p}
		c.runner = runner
	}
}

// WithCeilingPostures configures DYNAMIC-mode postures (SPEC §10.2/§8): per Check
// the checker reads src.Current() and selects table[ordinal] (clamped fail-closed
// to table[0] when out of range). This per-Check selection IS the ceiling clamp —
// downgrading the ceiling immediately makes the next Check use a lower posture.
// src must be the SAME source the sandbox Executor was built with so posture and
// enforcement never disagree. runner is the one held reference (see WithPosture).
// The table is copied so a later external mutation cannot alter the gate.
func WithCeilingPostures(src CeilingSource, table []Posture, runner tool.CommandRunner) Option {
	cloned := append([]Posture(nil), table...)
	return func(c *checkerConfig) {
		c.posture = ceilingPostures{src: src, table: cloned}
		c.runner = runner
	}
}

// stagePosture is the posture-driven auto-approve stage (Stage 6.5). It runs AFTER
// every stage that can DENY (1-2 containment/hard-deny, 3 EffectChecker veto, 5-6
// persisted/session deny) and BEFORE the Stage 7 default Ask, so it can only ever
// UPGRADE a still-undecided call to AutoApprove — never override an earlier deny.
// It returns (effect, decided): decided=true is always EffectAutoApprove; a
// decided=false result falls through to the default Ask (the fail-closed path for
// no posture, a failed interlock, a grant-carrying call, or a non-trivial command).
func (c *PermissionChecker) stagePosture(toolName string, class toolClass, argsJSON string) (loop.Effect, bool) {
	if c.posture == nil {
		return loop.EffectAsk, false // no posture configured => today's behavior.
	}
	p := c.posture.current()

	// A grant-carrying call is an escalation: never auto-approved by posture. It
	// falls through to Ask (== human review) regardless of class or interlock.
	if p.GrantCarryingAlwaysAsk && hasGrants(argsJSON) {
		return loop.EffectAsk, false
	}

	switch class {
	case classWrite:
		// Edits are interlock-gated too (SPEC §10.3): auto-approve only when the held
		// runner ACTUALLY enforces RequiredGuaranteesEdits (and any RequiredLevel floor).
		// A nil / non-probing runner against a non-zero edit mask fails closed → Ask; a
		// zero edit mask requires nothing (backward-compatible auto-approve).
		if p.AutoApproveEdits && c.interlockPassesMask(p, p.RequiredGuaranteesEdits) {
			return loop.EffectAutoApprove, true
		}
	case classBash:
		return c.postureBash(p, argsJSON)
	}
	return loop.EffectAsk, false
}

// postureBash applies the Bash auto-approve rules under the guarantee interlock.
// The interlock is checked FIRST for both the full-auto and the trivial-only path:
// a failed interlock (no runner, no GuaranteeBits, a missing required bit, or a
// Level below the floor) always declines to Ask.
func (c *PermissionChecker) postureBash(p Posture, argsJSON string) (loop.Effect, bool) {
	if !c.interlockPasses(p) {
		return loop.EffectAsk, false // fail-closed: guarantees not enforced.
	}
	if p.AutoApproveBash {
		return loop.EffectAutoApprove, true // full bash auto (interlock passed).
	}
	if p.TrivialBash != nil {
		cmd, ok, err := extractStringField(argsJSON, fieldCommand)
		if err != nil || !ok {
			return loop.EffectAsk, false // no classifiable command => Ask (fail-closed).
		}
		if p.TrivialBash(cmd) {
			return loop.EffectAutoApprove, true
		}
	}
	return loop.EffectAsk, false // non-trivial, or no bash auto-approve configured.
}

// interlockPasses reports whether the held runner ACTUALLY enforces the posture's
// Bash-required guarantees (RequiredGuarantees, SPEC §10.3). It is the safety gate
// for Bash auto-approve — a thin alias over interlockPassesMask with the Bash mask.
func (c *PermissionChecker) interlockPasses(p Posture) bool {
	return c.interlockPassesMask(p, p.RequiredGuarantees)
}

// interlockPassesMask reports whether the held runner ACTUALLY enforces the given
// guarantee mask AND meets the posture's optional Level floor (SPEC §10.3). It is the
// SHARED fail-closed gate for BOTH Bash auto-approve (mask = RequiredGuarantees) and
// edit auto-approve (mask = RequiredGuaranteesEdits), so the two paths degrade
// identically on a non-enforcing runner. Both checks are fail-closed:
//   - guarantee bits: pass only when runnerBits & mask == mask. A nil runner or one
//     without GuaranteeBits contributes 0 bits, so any non-zero mask fails.
//   - Level floor (optional): when RequiredLevel > 0 the runner's Level() must be
//     >= it. A nil runner or one without Level() contributes 0, so a positive floor
//     fails closed.
func (c *PermissionChecker) interlockPassesMask(p Posture, mask uint64) bool {
	if c.runnerGuaranteeBits()&mask != mask {
		return false
	}
	if p.RequiredLevel > 0 && c.runnerLevel() < p.RequiredLevel {
		return false
	}
	return true
}

// runnerGuaranteeBits probes the held runner STRUCTURALLY for its guarantee
// bitmask. It returns 0 (fail-closed) when there is no runner or the runner does
// not implement GuaranteeBits() — harness never imports sandbox to learn the type.
func (c *PermissionChecker) runnerGuaranteeBits() uint64 {
	if c.runner == nil {
		return 0
	}
	gb, ok := c.runner.(interface{ GuaranteeBits() uint64 })
	if !ok {
		return 0
	}
	return gb.GuaranteeBits()
}

// runnerLevel probes the held runner STRUCTURALLY for its coarse isolation Level.
// It returns 0 (LevelNone, fail-closed) when there is no runner or the runner does
// not implement Level().
func (c *PermissionChecker) runnerLevel() uint8 {
	if c.runner == nil {
		return 0
	}
	lv, ok := c.runner.(interface{ Level() uint8 })
	if !ok {
		return 0
	}
	return lv.Level()
}

// hasGrants reports whether argsJSON carries a NON-EMPTY top-level "grants" array
// (SPEC §10.7). It is fail-closed toward "carrying" (which forces Ask): a present
// but non-array "grants" value is treated as carrying. An absent field, an
// explicit null, or an empty array is NOT carrying. An unparseable document is not
// treated as carrying because the malformed-args gate has already Ask'd it before
// this stage runs.
func hasGrants(argsJSON string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &obj); err != nil {
		return false
	}
	raw, present := obj[fieldGrants]
	if !present || string(raw) == "null" {
		return false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return true // present but not an array => fail-closed: treat as grant-carrying.
	}
	return len(arr) > 0
}
