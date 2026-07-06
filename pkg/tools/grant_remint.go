package tools

import (
	"context"
	"slices"

	"github.com/looprig/harness/pkg/loop"
)

// grant_remint.go opens the Task-17d session/workspace-scope re-mint seam. A repeat
// call whose CURRENTLY-computed escalation delta set equals a persisted delta-bearing
// ALLOW record auto-approves (the MATCH half is recordMatcher's delta-bearing branch in
// check.go, which calls computeCallDeltas/equalDeltaSet here); the runner then re-mints
// FRESH single-mint tokens for the spawn via ApprovedGrants. Tokens stay single-mint
// and short-lived even under a session-scope approval: only the DESCRIPTIONS are ever
// persisted, and live tokens are re-minted on every auto-approved call. harness never
// imports sandbox — PlanGrants/DescribeGrant are probed STRUCTURALLY on the held runner
// (SPEC §9.3, §10.7), the same shape Bash.planGrants probes.

// describeGrantFn returns the held runner's DescribeGrant closure, or ok=false
// (fail-closed) when there is no DescribeGrant-capable runner — a nil runner or one
// without the method. The nil runner is guarded BEFORE the type assertion so a
// typed-nil runner cannot pass the assertion into a nil-receiver call (mirrors
// runnerGuaranteeBits/runnerLevel in posture.go).
func (c *PermissionChecker) describeGrantFn() (func(token string) (string, bool), bool) {
	if c.runner == nil {
		return nil, false
	}
	dg, ok := c.runner.(interface {
		DescribeGrant(token string) (string, bool)
	})
	if !ok {
		return nil, false
	}
	return dg.DescribeGrant, true
}

// describeGrants maps grant TOKENS to their sorted, deduped, MAC-verified delta
// DESCRIPTIONS via the held runner's DescribeGrant; an unverifiable token
// (fabricated/tampered/expired → ok==false) is SKIPPED, never described. It returns
// nil when there is no DescribeGrant-capable runner or nothing verifies. It is the
// SINGLE definition of "a token set's delta description set", shared by the WRITE side
// (deriveGrantDeltas persists it, grant.go) and the READ side (computeCallDeltas
// compares the live call's set to a record's), so the two can never derive it
// differently.
func (c *PermissionChecker) describeGrants(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}
	describe, ok := c.describeGrantFn()
	if !ok {
		return nil
	}
	seen := make(map[string]struct{}, len(tokens))
	var deltas []string
	for _, tok := range tokens {
		desc, ok := describe(tok)
		if !ok {
			continue // unverifiable token → never described.
		}
		if _, dup := seen[desc]; dup {
			continue
		}
		seen[desc] = struct{}{}
		deltas = append(deltas, desc)
	}
	slices.Sort(deltas)
	return deltas
}

// planCallTokens re-mints FRESH candidate escalation tokens for a Bash call. It probes
// the held runner for the structural escalation-planner capability (BOTH PlanGrants and
// DescribeGrant — a planner must also verify, mirroring Bash.planGrants) and asks
// PlanGrants for the SAME spawn dir + command the Bash tool will run: the dir is derived
// via resolveSpawnDir over the checker's WorkspaceRoot, which (by the composition-root
// invariant WorkspaceRoot == Bash.root) equals the dir the spawn uses, so the executor's
// hashCommand(dir, command) token binding verifies at spawn.
//
// RUNNER-IDENTITY INVARIANT: re-minted tokens verify at spawn ONLY because the checker
// holds the SAME confined runner INSTANCE the Bash tool runs through (same HMAC key) — a
// composition-root invariant honored in the swe consumer. It is fail-SECURE if violated:
// a token minted under a different runner's key simply fails the spawn's verification, so
// the command runs WITHOUT the escalation rather than with an unauthorized one.
//
// PURITY CONTRACT: PlanGrants is invoked SPECULATIVELY here and its result discarded (the
// seam reads only the descriptions for the delta-set match), so it MUST be pure and cheap
// — an HMAC over (dir, command, capability), no I/O and no side effects. The speculative
// 2–3× minting per decision (Check's two stages + ApprovedGrants) relies on that.
//
// ok=false (fail-closed) when: there is no planner runner, the command cannot be
// extracted (a non-Bash call has none — grant deltas only arise from Bash), or the
// workdir escapes the workspace (no confined dir to plan for). The returned tokens are
// single-mint — a fresh set on EVERY call.
func (c *PermissionChecker) planCallTokens(argsJSON string) ([]string, bool) {
	if c.runner == nil {
		return nil, false
	}
	pg, ok := c.runner.(interface {
		PlanGrants(dir, command string) []string
	})
	if !ok {
		return nil, false
	}
	if _, ok := c.describeGrantFn(); !ok {
		return nil, false // a planner MUST also verify (mirrors Bash.planGrants).
	}
	command, present, err := extractStringField(argsJSON, fieldCommand)
	if err != nil || !present || command == "" {
		return nil, false
	}
	workdir, _, wErr := extractStringField(argsJSON, fieldWorkdir)
	if wErr != nil {
		return nil, false
	}
	dir, err := resolveSpawnDir(c.policy.WorkspaceRoot, workdir)
	if err != nil {
		return nil, false // escaping workdir → no confined dir to plan for.
	}
	return pg.PlanGrants(dir, command), true
}

// computeCallDeltas returns the live call's CURRENTLY-required escalation delta set
// (sorted, deduped descriptions), re-minting via PlanGrants and verifying via
// DescribeGrant. ok=false (fail-closed) is the hard "cannot compute" signal (no planner
// runner, no command, or an escaping workdir); ok=true with an empty set means the call
// needs no escalation now. It is the match input for recordMatcher's delta-bearing
// branch: a delta-bearing ALLOW matches only when equalDeltaSet(computed, record) holds.
func (c *PermissionChecker) computeCallDeltas(argsJSON string) ([]string, bool) {
	tokens, ok := c.planCallTokens(argsJSON)
	if !ok {
		return nil, false
	}
	return c.describeGrants(tokens), true
}

// equalDeltaSet reports whether two delta-description sets are equal AS SETS (order- and
// duplicate-insensitive). Both the record's persisted deltas and the live call's
// computed deltas are canonicalized (sorted + deduped) first, so a hand-edited on-disk
// record with reordered or duplicated entries still compares correctly.
func equalDeltaSet(a, b []string) bool {
	return slices.Equal(canonicalizeDeltas(a), canonicalizeDeltas(b))
}

// canonicalizeDeltas returns a sorted, deduped clone (nil for an empty input) so
// equalDeltaSet compares sets, not slices. It never mutates its argument.
func canonicalizeDeltas(d []string) []string {
	if len(d) == 0 {
		return nil
	}
	out := slices.Clone(d)
	slices.Sort(out)
	return slices.Compact(out)
}

// ApprovedGrants re-mints FRESH single-mint escalation grant tokens for a call that a
// delta-bearing ALLOW record auto-approves (Stage 5 persisted or Stage 6 session), so
// the runner can place LIVE tokens on the spawn ctx AFTER Check returned
// EffectAutoApprove (Check yields only an Effect, and the pre-ask tokens are single-mint
// and already spent). It is an OPTIONAL method the runner probes by type assertion — no
// new shared-package interface — see loop.applyApprovedGrants.
//
// It returns nil (no grants) when the auto-approve did NOT come from a delta-bearing
// record: no planner runner (fail-closed), the call needs no escalation, or no
// persisted/session delta-bearing ALLOW record's delta set equals the call's current set
// (drift). When one DOES match it re-mints via PlanGrants and returns the freshly-minted
// tokens whose MAC-verified description is in the record's delta set — bound (by the
// executor) to hashCommand(spawnDir, command), so they verify at spawn (see
// planCallTokens' runner-identity invariant). It RE-MINTS on every call (single-mint).
//
// ctx is threaded from the runner so the Stage-5 record load keeps the call's trace
// context on this security path.
func (c *PermissionChecker) ApprovedGrants(ctx context.Context, toolName, argsJSON string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	tokens, ok := c.planCallTokens(argsJSON)
	if !ok || len(tokens) == 0 {
		return nil
	}
	callDeltas := c.describeGrants(tokens)
	if len(callDeltas) == 0 {
		return nil // nothing verifiable to grant.
	}
	if !c.hasMatchingDeltaRecord(ctx, toolName, argsJSON, callDeltas) {
		return nil // auto-approve came from a non-delta path, or the set drifted.
	}
	return c.filterTokensToDeltas(tokens, callDeltas)
}

// hasMatchingDeltaRecord reports whether ANY persisted (workspace→user, unless the
// Unattended posture skips Stage 5) or in-memory session ALLOW record with a non-empty
// delta set base-matches the call AND has a delta set equal to callDeltas — the SAME
// condition recordMatcher's delta-bearing branch uses to auto-approve. Gathering the
// records the way Check's Stage 5/6 do keeps the re-mint decision from ever disagreeing
// with the auto-approve.
func (c *PermissionChecker) hasMatchingDeltaRecord(ctx context.Context, toolName, argsJSON string, callDeltas []string) bool {
	class := classifyTool(toolName)
	base := c.baseRecordMatcher(toolName, class, argsJSON)
	for _, rec := range c.collectDeltaRecords(ctx) {
		if rec.Effect != loop.EffectAutoApprove || len(rec.GrantDeltas) == 0 {
			continue
		}
		if base(rec) && equalDeltaSet(callDeltas, rec.GrantDeltas) {
			return true
		}
	}
	return false
}

// collectDeltaRecords gathers the persisted (workspace→user) and in-memory session
// approval records the same way Check's Stage 5/6 do: the persisted set comes from the
// SHARED loadPersistedRecords gatherer (nil under the Unattended posture or when home is
// "" — fail-secure), then the session policies. It is called under c.mu (held by
// ApprovedGrants), so it drives the approval caches exactly as Check does.
func (c *PermissionChecker) collectDeltaRecords(ctx context.Context) []ApprovalRecord {
	all := c.loadPersistedRecords(ctx)
	return append(all, sessionPoliciesToRecords(c.policy.Policies)...)
}

// filterTokensToDeltas keeps each re-minted token whose MAC-verified DescribeGrant
// description is in the allowed delta set (the matched record's deltas), dropping any
// token that fails verification or describes to a capability outside the record. It is
// the fail-secure filter guaranteeing ApprovedGrants never returns a token the
// operator's persisted record did not authorize.
func (c *PermissionChecker) filterTokensToDeltas(tokens, deltas []string) []string {
	describe, ok := c.describeGrantFn()
	if !ok {
		return nil
	}
	allowed := make(map[string]struct{}, len(deltas))
	for _, d := range deltas {
		allowed[d] = struct{}{}
	}
	var out []string
	for _, tok := range tokens {
		desc, ok := describe(tok)
		if !ok {
			continue
		}
		if _, in := allowed[desc]; in {
			out = append(out, tok)
		}
	}
	return out
}
