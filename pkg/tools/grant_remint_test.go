package tools

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// grant_remint_test.go exercises the Task-17d session/workspace-scope re-mint seam:
// a repeat call whose CURRENTLY-computed escalation delta set equals a persisted
// delta-bearing ALLOW record auto-approves (recordMatcher), and ApprovedGrants
// re-mints FRESH single-mint tokens for the spawn. It also pins the fail-secure
// edges (drift → Ask, no-planner → Ask, non-delta auto-approve → nil grants) and the
// dir-consistency invariant (the checker plans grants for the SAME spawn dir Bash
// runs in).

// countingGrantRunner is a tool.CommandRunner plus the structural escalation-planner
// methods (PlanGrants/DescribeGrant) the checker probes — no sandbox import. It mints
// a FRESH, unique token on EVERY PlanGrants call (single-mint), so a test can prove
// ApprovedGrants re-mints distinct tokens each invocation rather than replaying a
// spent one. capForCmd maps a command → the single capability description PlanGrants
// mints one token for; minted records every issued (and pre-seeded) token → its
// description so DescribeGrant verifies it; lastDir records the dir PlanGrants last
// saw (for the dir-consistency assertion).
type countingGrantRunner struct {
	mu        sync.Mutex
	capForCmd map[string]string
	minted    map[string]string
	n         int
	lastDir   string
}

func newCountingGrantRunner(capForCmd, seeded map[string]string) *countingGrantRunner {
	m := make(map[string]string, len(seeded))
	for k, v := range seeded {
		m[k] = v
	}
	return &countingGrantRunner{capForCmd: capForCmd, minted: m}
}

func (r *countingGrantRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}

func (r *countingGrantRunner) PlanGrants(dir, command string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastDir = dir
	desc, ok := r.capForCmd[command]
	if !ok {
		return nil
	}
	r.n++
	tok := fmt.Sprintf("mint-%d", r.n)
	r.minted[tok] = desc
	return []string{tok}
}

func (r *countingGrantRunner) DescribeGrant(token string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.minted[token]
	return d, ok
}

func (r *countingGrantRunner) dir() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastDir
}

// describes reports whether a minted token verifies to the given description.
func (r *countingGrantRunner) describes(token, want string) bool {
	d, ok := r.DescribeGrant(token)
	return ok && d == want
}

var _ tool.CommandRunner = (*countingGrantRunner)(nil)

// grantDeltaChecker builds a checker holding runner and persists a delta-bearing
// ALLOW record for Bash "git push" at scope, deriving its delta from seedTok (which
// runner must DescribeGrant). It asserts the record actually carries a delta.
func grantDeltaChecker(t *testing.T, ws, home string, runner tool.CommandRunner, seedTok string, scope tool.ApprovalScope) *PermissionChecker {
	t.Helper()
	pc, err := NewPermissionChecker(
		PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
		WithHomeDir(func() (string, error) { return home, nil }),
		WithPosture(Posture{}, runner),
	)
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}
	ctx := tool.WithGrants(context.Background(), []string{seedTok})
	if err := pc.Grant(ctx, "Bash", `{"command":"git push"}`, scope); err != nil {
		t.Fatalf("Grant(%v): %v", scope, err)
	}
	return pc
}

// TestDeltaBearingRecordAutoApprovesAndRemints proves the OPEN seam: a persisted
// delta-bearing ALLOW record auto-approves a later call whose computed delta set
// equals it, and ApprovedGrants re-mints FRESH single-mint tokens filtered to the
// record's deltas — for BOTH an in-memory session record and an on-disk workspace
// record reloaded through a fresh checker.
func TestDeltaBearingRecordAutoApprovesAndRemints(t *testing.T) {
	t.Parallel()
	const call = `{"command":"git push"}`

	t.Run("session-projected record", func(t *testing.T) {
		t.Parallel()
		ws := newWS(t)
		home := t.TempDir()
		runner := newCountingGrantRunner(
			map[string]string{"git push": "egress"},
			map[string]string{"seed": "egress"},
		)
		pc := grantDeltaChecker(t, ws, home, runner, "seed", tool.ScopeSession)

		if got := pc.Check(context.Background(), plainTool{name: toolBash}, toolBash, call); got != loop.EffectAutoApprove {
			t.Fatalf("Check(repeat vs delta-bearing session record) = %v, want EffectAutoApprove (seam open)", got)
		}
		// Dir-consistency: the checker planned grants for the SAME dir Bash spawns in.
		wantDir, _ := resolveSpawnDir(ws, "")
		if got := runner.dir(); got != wantDir {
			t.Errorf("PlanGrants dir = %q, want spawn dir %q", got, wantDir)
		}

		grants1 := pc.ApprovedGrants(toolBash, call)
		if len(grants1) != 1 {
			t.Fatalf("ApprovedGrants = %#v, want exactly 1 re-minted token", grants1)
		}
		if grants1[0] == "seed" {
			t.Errorf("ApprovedGrants returned the ORIGINAL grant token %q; must re-mint fresh", grants1[0])
		}
		if !runner.describes(grants1[0], "egress") {
			t.Errorf("re-minted token %q does not verify to the record delta %q", grants1[0], "egress")
		}

		grants2 := pc.ApprovedGrants(toolBash, call)
		if len(grants2) != 1 || grants2[0] == grants1[0] {
			t.Errorf("second ApprovedGrants = %#v, want a DISTINCT freshly-minted token (single-mint), first was %#v", grants2, grants1)
		}
	})

	t.Run("on-disk workspace record reloaded through a fresh checker", func(t *testing.T) {
		t.Parallel()
		ws := newWS(t)
		home := t.TempDir()
		writerRunner := newCountingGrantRunner(
			map[string]string{"git push": "egress"},
			map[string]string{"seed": "egress"},
		)
		_ = grantDeltaChecker(t, ws, home, writerRunner, "seed", tool.ScopeWorkspace)
		af := readApprovalsFile(t, wsApprovalsPathFor(t, home, ws))
		if len(af.Approvals) != 1 || len(af.Approvals[0].GrantDeltas) == 0 {
			t.Fatalf("precondition: on-disk record missing GrantDeltas: %+v", af.Approvals)
		}

		// A FRESH checker (its own planner runner) reloads approvals.json and matches.
		readerRunner := newCountingGrantRunner(map[string]string{"git push": "egress"}, nil)
		reader, err := NewPermissionChecker(
			PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			WithHomeDir(func() (string, error) { return home, nil }),
			WithPosture(Posture{}, readerRunner),
		)
		if err != nil {
			t.Fatalf("NewPermissionChecker(reader): %v", err)
		}
		if got := reader.Check(context.Background(), plainTool{name: toolBash}, toolBash, call); got != loop.EffectAutoApprove {
			t.Fatalf("Check(repeat vs delta-bearing on-disk record) = %v, want EffectAutoApprove", got)
		}
		grants := reader.ApprovedGrants(toolBash, call)
		if len(grants) != 1 || !readerRunner.describes(grants[0], "egress") {
			t.Errorf("ApprovedGrants = %#v, want one fresh token describing %q", grants, "egress")
		}
	})
}

// TestDeltaBearingRecordDriftAsks proves policy drift is fail-secure: a record whose
// GrantDeltas do NOT equal the call's CURRENTLY-computed delta set (the runner's
// capabilities changed) does NOT auto-match — Check Asks and ApprovedGrants returns
// nil (no token is minted under a record that no longer describes the call).
func TestDeltaBearingRecordDriftAsks(t *testing.T) {
	t.Parallel()
	const call = `{"command":"git push"}`
	ws := newWS(t)
	home := t.TempDir()
	// Record delta derived from seed→"egress"; but PlanGrants now yields "fs-write".
	runner := newCountingGrantRunner(
		map[string]string{"git push": "fs-write"},
		map[string]string{"seed": "egress"},
	)
	pc := grantDeltaChecker(t, ws, home, runner, "seed", tool.ScopeSession)
	if d := lastSessionPolicy(t, pc).GrantDeltas; !slices.Equal(d, []string{"egress"}) {
		t.Fatalf("precondition: record deltas = %#v, want [egress]", d)
	}

	if got := pc.Check(context.Background(), plainTool{name: toolBash}, toolBash, call); got != loop.EffectAsk {
		t.Errorf("Check(drifted delta set) = %v, want EffectAsk (fail-secure)", got)
	}
	if g := pc.ApprovedGrants(toolBash, call); g != nil {
		t.Errorf("ApprovedGrants(drift) = %#v, want nil", g)
	}
}

// TestDeltaBearingRecordFailsClosedWithoutPlanner proves the fail-closed edge: a
// delta-bearing record with a runner that verifies (DescribeGrant) but CANNOT plan
// (no PlanGrants) can never compute the call's delta set, so it never auto-matches —
// Check Asks and ApprovedGrants returns nil. Covers BOTH the session-projected record
// and an on-disk record reloaded through a fresh (runnerless) checker.
func TestDeltaBearingRecordFailsClosedWithoutPlanner(t *testing.T) {
	t.Parallel()
	const call = `{"command":"git push","grants":["x"]}`

	t.Run("session-projected record, no PlanGrants", func(t *testing.T) {
		t.Parallel()
		ws := newWS(t)
		home := t.TempDir()
		runner := &fakeDescribeRunner{desc: map[string]string{"tok": "egress"}} // no PlanGrants.
		pc, err := NewPermissionChecker(
			PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			WithHomeDir(func() (string, error) { return home, nil }),
			WithPosture(Posture{}, runner),
		)
		if err != nil {
			t.Fatalf("NewPermissionChecker: %v", err)
		}
		ctx := tool.WithGrants(context.Background(), []string{"tok"})
		if err := pc.Grant(ctx, "Bash", `{"command":"git push"}`, tool.ScopeSession); err != nil {
			t.Fatalf("Grant: %v", err)
		}
		if d := lastSessionPolicy(t, pc).GrantDeltas; len(d) == 0 {
			t.Fatalf("precondition: session policy has no GrantDeltas")
		}
		if got := pc.Check(context.Background(), plainTool{name: toolBash}, toolBash, call); got != loop.EffectAsk {
			t.Errorf("Check(no-planner runner) = %v, want EffectAsk (fail-closed)", got)
		}
		if g := pc.ApprovedGrants(toolBash, call); g != nil {
			t.Errorf("ApprovedGrants(no-planner) = %#v, want nil", g)
		}
	})

	t.Run("on-disk record, runnerless reader", func(t *testing.T) {
		t.Parallel()
		ws := newWS(t)
		home := t.TempDir()
		writer := &fakeDescribeRunner{desc: map[string]string{"tok": "egress"}}
		w, err := NewPermissionChecker(
			PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			WithHomeDir(func() (string, error) { return home, nil }),
			WithPosture(Posture{}, writer),
		)
		if err != nil {
			t.Fatalf("NewPermissionChecker(writer): %v", err)
		}
		ctx := tool.WithGrants(context.Background(), []string{"tok"})
		if err := w.Grant(ctx, "Bash", `{"command":"git push"}`, tool.ScopeWorkspace); err != nil {
			t.Fatalf("Grant(ScopeWorkspace): %v", err)
		}
		// Runnerless reader can neither plan nor verify → fail-closed.
		reader, err := NewPermissionChecker(
			PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			WithHomeDir(func() (string, error) { return home, nil }),
		)
		if err != nil {
			t.Fatalf("NewPermissionChecker(reader): %v", err)
		}
		if got := reader.Check(context.Background(), plainTool{name: toolBash}, toolBash, call); got != loop.EffectAsk {
			t.Errorf("Check(runnerless reader vs on-disk delta record) = %v, want EffectAsk", got)
		}
		if g := reader.ApprovedGrants(toolBash, call); g != nil {
			t.Errorf("ApprovedGrants(runnerless) = %#v, want nil", g)
		}
	})
}

// TestApprovedGrantsNilForNonDeltaAutoApprove proves ApprovedGrants returns nil when
// the auto-approve did NOT come from a delta-bearing record: a DELTALESS allow record
// auto-approves a grant-free repeat call, but that call needs no re-minted tokens.
func TestApprovedGrantsNilForNonDeltaAutoApprove(t *testing.T) {
	t.Parallel()
	const call = `{"command":"git push"}`
	ws := newWS(t)
	home := t.TempDir()
	runner := newCountingGrantRunner(map[string]string{"git push": "egress"}, nil)
	pc, err := NewPermissionChecker(
		PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
		WithHomeDir(func() (string, error) { return home, nil }),
		WithPosture(Posture{}, runner),
	)
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}
	// A grant-FREE session grant persists a DELTALESS allow.
	if err := pc.Grant(context.Background(), "Bash", `{"command":"git push"}`, tool.ScopeSession); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if got := pc.Check(context.Background(), plainTool{name: toolBash}, toolBash, call); got != loop.EffectAutoApprove {
		t.Fatalf("Check(grant-free vs deltaless record) = %v, want EffectAutoApprove", got)
	}
	if g := pc.ApprovedGrants(toolBash, call); g != nil {
		t.Errorf("ApprovedGrants(deltaless auto-approve) = %#v, want nil (no delta record matched)", g)
	}
}
