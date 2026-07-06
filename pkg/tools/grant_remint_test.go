package tools

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// grant_remint_test.go exercises the Task-17d session/workspace-scope re-mint seam:
// a repeat call whose CURRENTLY-computed escalation delta set equals a persisted
// delta-bearing ALLOW record auto-approves (recordMatcher), and ApprovedGrants
// re-mints FRESH single-mint tokens for the spawn. It pins the fail-secure edges
// (superset/subset/disjoint drift → Ask, no-planner → Ask, non-delta auto-approve →
// nil grants), the set-canonicalization contract (reordered/duplicated on-disk
// deltas still match), and the dir-consistency invariant (the checker plans grants
// for the SAME spawn dir Bash runs in).

// countingGrantRunner is a tool.CommandRunner plus the structural escalation-planner
// methods (PlanGrants/DescribeGrant) the checker probes — no sandbox import. It mints
// a FRESH, unique token for EACH capability description of a command on EVERY
// PlanGrants call (single-mint), so a test can assert multi-element delta sets AND
// that ApprovedGrants re-mints distinct tokens each invocation rather than replaying a
// spent one. capsForCmd maps a command → the capability descriptions PlanGrants mints
// one fresh token each for; minted records every issued (and pre-seeded) token → its
// description so DescribeGrant verifies it; lastDir records the dir PlanGrants last saw
// (for the dir-consistency assertion).
type countingGrantRunner struct {
	mu         sync.Mutex
	capsForCmd map[string][]string
	minted     map[string]string
	n          int
	lastDir    string
}

func newCountingGrantRunner(capsForCmd map[string][]string, seeded map[string]string) *countingGrantRunner {
	m := make(map[string]string, len(seeded))
	for k, v := range seeded {
		m[k] = v
	}
	return &countingGrantRunner{capsForCmd: capsForCmd, minted: m}
}

func (r *countingGrantRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}

func (r *countingGrantRunner) PlanGrants(dir, command string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastDir = dir
	descs, ok := r.capsForCmd[command]
	if !ok {
		return nil
	}
	toks := make([]string, 0, len(descs))
	for _, d := range descs {
		r.n++
		tok := fmt.Sprintf("mint-%d", r.n)
		r.minted[tok] = d
		toks = append(toks, tok)
	}
	return toks
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

// remintFixture builds a checker holding a planner runner and persists a
// delta-bearing session ALLOW record for Bash "git push" whose deltas are
// recordDescs (derived by Grant from one seeded token per description). The runner's
// PlanGrants yields computedDescs for "git push", so a test controls BOTH the
// record's set and the call's currently-computed set independently.
func remintFixture(t *testing.T, recordDescs, computedDescs []string) (*PermissionChecker, *countingGrantRunner) {
	t.Helper()
	ws := newWS(t)
	home := t.TempDir()
	seeded := make(map[string]string, len(recordDescs))
	var seedToks []string
	for i, d := range recordDescs {
		tok := fmt.Sprintf("seed-%d", i)
		seeded[tok] = d
		seedToks = append(seedToks, tok)
	}
	runner := newCountingGrantRunner(map[string][]string{"git push": computedDescs}, seeded)
	pc, err := NewPermissionChecker(
		PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
		WithHomeDir(func() (string, error) { return home, nil }),
		WithPosture(Posture{}, runner),
	)
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}
	ctx := tool.WithGrants(context.Background(), seedToks)
	if err := pc.Grant(ctx, "Bash", `{"command":"git push"}`, tool.ScopeSession); err != nil {
		t.Fatalf("Grant(ScopeSession): %v", err)
	}
	return pc, runner
}

// TestDeltaBearingRecordSetMatch is the core set-vs-slice matrix: a delta-bearing
// record auto-approves (and re-mints) ONLY when the call's computed delta set EQUALS
// the record's — covering multi-element equality and the security-critical superset
// (call needs MORE than approved), subset, and disjoint drift cases, all of which
// MUST fall to Ask with no re-minted tokens (a subset check here would be a
// privilege-escalation hole).
func TestDeltaBearingRecordSetMatch(t *testing.T) {
	t.Parallel()
	const call = `{"command":"git push"}`
	tests := []struct {
		name          string
		recordDescs   []string
		computedDescs []string
		wantEffect    loop.Effect
		wantGrants    int // number of re-minted tokens ApprovedGrants must return
	}{
		{name: "equal single", recordDescs: []string{"egress"}, computedDescs: []string{"egress"}, wantEffect: loop.EffectAutoApprove, wantGrants: 1},
		{name: "equal multi", recordDescs: []string{"egress", "fs-write"}, computedDescs: []string{"fs-write", "egress"}, wantEffect: loop.EffectAutoApprove, wantGrants: 2},
		{name: "superset needs more than approved", recordDescs: []string{"egress"}, computedDescs: []string{"egress", "fs-write"}, wantEffect: loop.EffectAsk, wantGrants: 0},
		{name: "subset needs less than approved", recordDescs: []string{"egress", "fs-write"}, computedDescs: []string{"egress"}, wantEffect: loop.EffectAsk, wantGrants: 0},
		{name: "disjoint drift", recordDescs: []string{"egress"}, computedDescs: []string{"fs-write"}, wantEffect: loop.EffectAsk, wantGrants: 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pc, runner := remintFixture(t, tt.recordDescs, tt.computedDescs)

			if got := pc.Check(context.Background(), plainTool{name: toolBash}, toolBash, call); got != tt.wantEffect {
				t.Fatalf("Check = %v, want %v", got, tt.wantEffect)
			}
			grants := pc.ApprovedGrants(context.Background(), toolBash, call)
			if len(grants) != tt.wantGrants {
				t.Fatalf("ApprovedGrants = %#v (len %d), want len %d", grants, len(grants), tt.wantGrants)
			}
			// Every returned token must be a freshly-minted one verifying to a record delta.
			recSet := map[string]struct{}{}
			for _, d := range tt.recordDescs {
				recSet[d] = struct{}{}
			}
			for _, tok := range grants {
				d, ok := runner.DescribeGrant(tok)
				if !ok {
					t.Errorf("returned token %q does not verify", tok)
				}
				if _, in := recSet[d]; !in {
					t.Errorf("returned token %q describes %q, outside the record's deltas %v", tok, d, tt.recordDescs)
				}
			}
		})
	}
}

// TestDeltaBearingRemintDirAndSingleMint proves (a) the checker plans grants for the
// SAME dir Bash spawns in (dir-consistency) and (b) ApprovedGrants re-mints DISTINCT
// fresh tokens on every call (single-mint), never replaying the original grant token.
func TestDeltaBearingRemintDirAndSingleMint(t *testing.T) {
	t.Parallel()
	const call = `{"command":"git push"}`
	pc, runner := remintFixture(t, []string{"egress"}, []string{"egress"})

	if got := pc.Check(context.Background(), plainTool{name: toolBash}, toolBash, call); got != loop.EffectAutoApprove {
		t.Fatalf("Check = %v, want EffectAutoApprove", got)
	}
	wantDir, _ := resolveSpawnDir(checkerWorkspaceRoot(t, pc), "")
	if got := runner.dir(); got != wantDir {
		t.Errorf("PlanGrants dir = %q, want spawn dir %q", got, wantDir)
	}

	grants1 := pc.ApprovedGrants(context.Background(), toolBash, call)
	grants2 := pc.ApprovedGrants(context.Background(), toolBash, call)
	if len(grants1) != 1 || len(grants2) != 1 {
		t.Fatalf("ApprovedGrants lens = %d,%d, want 1,1", len(grants1), len(grants2))
	}
	if grants1[0] == "seed-0" || grants2[0] == "seed-0" {
		t.Errorf("ApprovedGrants returned the ORIGINAL grant token; must re-mint fresh")
	}
	if grants1[0] == grants2[0] {
		t.Errorf("re-mint returned the SAME token %q twice; single-mint must re-mint distinct tokens", grants1[0])
	}
	if !runner.describes(grants1[0], "egress") || !runner.describes(grants2[0], "egress") {
		t.Errorf("re-minted tokens do not verify to the record delta %q", "egress")
	}
}

// checkerWorkspaceRoot returns the checker's WorkspaceRoot (test-only accessor under
// the lock) so a test can derive the expected spawn dir.
func checkerWorkspaceRoot(t *testing.T, pc *PermissionChecker) string {
	t.Helper()
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.policy.WorkspaceRoot
}

// TestDeltaBearingRecordOnDiskReload proves the seam works through an on-disk
// workspace record reloaded by a FRESH checker — both an exact-match record AND a
// hand-edited-journal record whose grant_deltas are REORDERED and DUPLICATED (which
// canonicalizeDeltas/equalDeltaSet must treat as the same set, end to end).
func TestDeltaBearingRecordOnDiskReload(t *testing.T) {
	t.Parallel()
	const call = `{"command":"git push"}`

	t.Run("exact match via Grant round-trip", func(t *testing.T) {
		t.Parallel()
		ws := newWS(t)
		home := t.TempDir()
		writer := newCountingGrantRunner(nil, map[string]string{"s": "egress"})
		w, err := NewPermissionChecker(
			PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			WithHomeDir(func() (string, error) { return home, nil }),
			WithPosture(Posture{}, writer),
		)
		if err != nil {
			t.Fatalf("NewPermissionChecker(writer): %v", err)
		}
		if err := w.Grant(tool.WithGrants(context.Background(), []string{"s"}), "Bash", `{"command":"git push"}`, tool.ScopeWorkspace); err != nil {
			t.Fatalf("Grant(ScopeWorkspace): %v", err)
		}

		readerRunner := newCountingGrantRunner(map[string][]string{"git push": {"egress"}}, nil)
		reader, err := NewPermissionChecker(
			PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			WithHomeDir(func() (string, error) { return home, nil }),
			WithPosture(Posture{}, readerRunner),
		)
		if err != nil {
			t.Fatalf("NewPermissionChecker(reader): %v", err)
		}
		if got := reader.Check(context.Background(), plainTool{name: toolBash}, toolBash, call); got != loop.EffectAutoApprove {
			t.Fatalf("Check(on-disk record) = %v, want EffectAutoApprove", got)
		}
		if g := reader.ApprovedGrants(context.Background(), toolBash, call); len(g) != 1 || !readerRunner.describes(g[0], "egress") {
			t.Errorf("ApprovedGrants = %#v, want one fresh token describing %q", g, "egress")
		}
	})

	t.Run("reordered+duplicated on-disk deltas still match", func(t *testing.T) {
		t.Parallel()
		ws := newWS(t)
		// Hand-edited-journal shape: same SET as the call, but reordered + duplicated.
		rec := ApprovalRecord{
			Tool:        toolBash,
			Match:       "git push",
			Effect:      loop.EffectAutoApprove,
			GrantDeltas: []string{"fs-write", "egress", "egress"},
		}
		_, homeFn := fakeHome(t, ws, writeApprovals(t, rec), nil)
		readerRunner := newCountingGrantRunner(map[string][]string{"git push": {"egress", "fs-write"}}, nil)
		reader, err := NewPermissionChecker(
			PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			WithHomeDir(homeFn),
			WithPosture(Posture{}, readerRunner),
		)
		if err != nil {
			t.Fatalf("NewPermissionChecker(reader): %v", err)
		}
		if got := reader.Check(context.Background(), plainTool{name: toolBash}, toolBash, call); got != loop.EffectAutoApprove {
			t.Fatalf("Check(reordered+dup on-disk deltas) = %v, want EffectAutoApprove (set canonicalization)", got)
		}
		if g := reader.ApprovedGrants(context.Background(), toolBash, call); len(g) != 2 {
			t.Errorf("ApprovedGrants = %#v, want 2 fresh tokens filtered to the record set", g)
		}
	})
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
		if g := pc.ApprovedGrants(context.Background(), toolBash, call); g != nil {
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
		if g := reader.ApprovedGrants(context.Background(), toolBash, call); g != nil {
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
	runner := newCountingGrantRunner(map[string][]string{"git push": {"egress"}}, nil)
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
	if g := pc.ApprovedGrants(context.Background(), toolBash, call); g != nil {
		t.Errorf("ApprovedGrants(deltaless auto-approve) = %#v, want nil (no delta record matched)", g)
	}
}
