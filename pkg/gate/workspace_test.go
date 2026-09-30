package gate_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

func TestWorkspaceAccessDecisions(t *testing.T) {
	t.Parallel()
	root := canonicalDir(t)
	other := canonicalDir(t)
	access, err := gate.NewWorkspaceAccess(root)
	if err != nil {
		t.Fatalf("NewWorkspaceAccess() error = %v", err)
	}
	if got := access.AccessVersion(); got != gate.CurrentAccessVersion {
		t.Fatalf("AccessVersion() = %d, want %d", got, gate.CurrentAccessVersion)
	}
	tests := []struct {
		name  string
		kind  string
		scope string
		want  uint8
	}{
		{name: "read at root", kind: "filesystem.read", scope: root, want: gate.AccessAllow},
		{name: "read under root", kind: "filesystem.read", scope: filepath.Join(root, "src", "main.go"), want: gate.AccessAllow},
		{name: "read tree scope under root", kind: "filesystem.read", scope: "tree:" + filepath.Join(root, "src"), want: gate.AccessAllow},
		{name: "read outside", kind: "filesystem.read", scope: filepath.Join(other, "secret"), want: gate.AccessDeny},
		{name: "read sibling with shared prefix", kind: "filesystem.read", scope: root + "-evil", want: gate.AccessDeny},
		{name: "read parent of root", kind: "filesystem.read", scope: filepath.Dir(root), want: gate.AccessDeny},
		{name: "read whole host", kind: "filesystem.read", scope: "host:*", want: gate.AccessDeny},
		{name: "write at root", kind: "filesystem.write", scope: root, want: gate.AccessGated},
		{name: "write under root", kind: "filesystem.write", scope: filepath.Join(root, "main.go"), want: gate.AccessGated},
		{name: "write tree scope under root", kind: "filesystem.write", scope: "tree:" + filepath.Join(root, "src"), want: gate.AccessGated},
		{name: "write outside", kind: "filesystem.write", scope: filepath.Join(other, "main.go"), want: gate.AccessDeny},
		{name: "write sibling with shared prefix", kind: "filesystem.write", scope: root + "-evil/main.go", want: gate.AccessDeny},
		{name: "write parent of root", kind: "filesystem.write", scope: filepath.Dir(root), want: gate.AccessDeny},
		{name: "write whole host", kind: "filesystem.write", scope: "host:*", want: gate.AccessDeny},
		{name: "command", kind: "command.execute", scope: root, want: gate.AccessDeny},
		{name: "network", kind: "network", scope: "", want: gate.AccessDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := access.AccessFor(tt.kind, tt.scope)
			if err != nil {
				t.Fatalf("AccessFor(%q, %q) error = %v", tt.kind, tt.scope, err)
			}
			if got != tt.want {
				t.Fatalf("AccessFor(%q, %q) = %d, want %d", tt.kind, tt.scope, got, tt.want)
			}
		})
	}
}

func TestWorkspaceAccessFailsClosedOnMalformedInput(t *testing.T) {
	t.Parallel()
	root := canonicalDir(t)
	access, err := gate.NewWorkspaceAccess(root)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, kind, scope string }{
		{name: "unknown kind", kind: "mcp.call", scope: ""},
		{name: "relative write scope", kind: "filesystem.write", scope: "src/main.go"},
		{name: "traversal write scope", kind: "filesystem.write", scope: root + "/../etc/passwd"},
		{name: "unclean write scope", kind: "filesystem.write", scope: root + "/./main.go"},
		{name: "empty write scope", kind: "filesystem.write", scope: ""},
		{name: "empty tree write scope", kind: "filesystem.write", scope: "tree:"},
		{name: "relative read scope", kind: "filesystem.read", scope: "src/main.go"},
		{name: "traversal read scope", kind: "filesystem.read", scope: root + "/../etc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := access.AccessFor(tt.kind, tt.scope)
			if err == nil {
				t.Fatalf("AccessFor(%q, %q) = %d, nil; want an error", tt.kind, tt.scope, got)
			}
			if got != gate.AccessDeny {
				t.Fatalf("AccessFor(%q, %q) = %d on error, want AccessDeny", tt.kind, tt.scope, got)
			}
		})
	}

	var zero *gate.WorkspaceAccess
	if zero.AccessVersion() != 0 || zero.Roots() != nil {
		t.Fatal("nil WorkspaceAccess must report an unsupported access version and no roots")
	}
	if got, err := zero.AccessFor("filesystem.write", root); err == nil || got != gate.AccessDeny {
		t.Fatalf("nil WorkspaceAccess AccessFor = %d, %v; want deny with an error", got, err)
	}
	if (&gate.WorkspaceAccess{}).AccessVersion() != 0 {
		t.Fatal("unconstructed WorkspaceAccess must report an unsupported access version")
	}
}

func TestNewWorkspaceAccessRejectsUnsafeRoots(t *testing.T) {
	t.Parallel()
	dir := canonicalDir(t)
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		roots []string
	}{
		{name: "no roots", roots: nil},
		{name: "empty root", roots: []string{""}},
		{name: "whitespace root", roots: []string{dir + " "}},
		{name: "missing root", roots: []string{filepath.Join(dir, "missing")}},
		{name: "file root", roots: []string{file}},
		{name: "filesystem root", roots: []string{string(filepath.Separator)}},
		{name: "one bad among good", roots: []string{dir, filepath.Join(dir, "missing")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			access, err := gate.NewWorkspaceAccess(tt.roots...)
			if err == nil {
				t.Fatalf("NewWorkspaceAccess(%q) = %v, nil; want an error", tt.roots, access.Roots())
			}
			var accessErr *gate.AccessError
			if !errors.As(err, &accessErr) || accessErr.Kind != gate.AccessRootInvalid {
				t.Fatalf("NewWorkspaceAccess(%q) error = %v, want AccessError{Kind: %q}", tt.roots, err, gate.AccessRootInvalid)
			}
		})
	}
}

func TestNewWorkspaceAccessCanonicalizesRoots(t *testing.T) {
	t.Parallel()
	real := canonicalDir(t)
	links := canonicalDir(t)
	link := filepath.Join(links, "workspace")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	access, err := gate.NewWorkspaceAccess(link, real, real+string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := access.Roots(), []string{real}; !slices.Equal(got, want) {
		t.Fatalf("Roots() = %q, want %q (symlink resolved, duplicates compacted)", got, want)
	}
	if got, err := access.AccessFor("filesystem.write", filepath.Join(real, "a.txt")); err != nil || got != gate.AccessGated {
		t.Fatalf("AccessFor(write resolved) = %d, %v, want gated", got, err)
	}
	// Lexical contract: a spelling through a symlink that lives outside every
	// root is outside, so a write through it is denied, never gated.
	if got, _ := access.AccessFor("filesystem.write", filepath.Join(link, "a.txt")); got != gate.AccessDeny {
		t.Fatalf("AccessFor(write via outside symlink) = %d, want deny", got)
	}
	roots := access.Roots()
	roots[0] = "/"
	if access.Roots()[0] != real {
		t.Fatal("Roots() must return a copy")
	}
}

func TestWorkspaceAccessBindingsCoverStandardKinds(t *testing.T) {
	t.Parallel()
	access, err := gate.NewWorkspaceAccess(canonicalDir(t))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, binding := range access.Bindings() {
		if binding.Source != access {
			t.Fatalf("binding %q routes to %v, want the workspace source", binding.Kind, binding.Source)
		}
		kinds = append(kinds, binding.Kind)
	}
	slices.Sort(kinds)
	if want := []string{"command.execute", "filesystem.read", "filesystem.write", "network"}; !slices.Equal(kinds, want) {
		t.Fatalf("Bindings() kinds = %q, want %q", kinds, want)
	}
}

func TestSessionRulesRemembersExactCandidatesAllowOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rules := gate.NewSessionRules()
	target := tool.Requirement{Kind: "filesystem.write", Scope: "/w/a.go", Match: "/w/a.go"}
	if matched, err := rules.MatchesAllow(ctx, target); err != nil || matched {
		t.Fatalf("MatchesAllow(before write) = %v, %v; want false", matched, err)
	}
	if err := rules.WriteRules(ctx, []tool.RuleCandidate{{Kind: "filesystem.write", Match: "/w/a.go", Description: "write /w/a.go"}}); err != nil {
		t.Fatalf("WriteRules() error = %v", err)
	}
	tests := []struct {
		name string
		req  tool.Requirement
		want bool
	}{
		{name: "exact kind and match", req: target, want: true},
		{name: "same match other kind", req: tool.Requirement{Kind: "filesystem.read", Match: "/w/a.go"}},
		{name: "same kind other match", req: tool.Requirement{Kind: "filesystem.write", Match: "/w/b.go"}},
		{name: "match is not a prefix rule", req: tool.Requirement{Kind: "filesystem.write", Match: "/w/a.go/x"}},
	}
	for _, tt := range tests {
		matched, err := rules.MatchesAllow(ctx, tt.req)
		if err != nil || matched != tt.want {
			t.Fatalf("%s: MatchesAllow() = %v, %v; want %v", tt.name, matched, err, tt.want)
		}
		if denied, err := rules.MatchesDeny(ctx, tt.req); err != nil || denied {
			t.Fatalf("%s: MatchesDeny() = %v, %v; want false (allow-only)", tt.name, denied, err)
		}
	}

	// A batch with an invalid candidate is refused whole: nothing is remembered.
	err := rules.WriteRules(ctx, []tool.RuleCandidate{
		{Kind: "filesystem.write", Match: "/w/c.go"},
		{Kind: "", Match: "/w/d.go"},
	})
	if err == nil {
		t.Fatal("WriteRules(batch with an empty kind) error = nil")
	}
	if matched, _ := rules.MatchesAllow(ctx, tool.Requirement{Kind: "filesystem.write", Match: "/w/c.go"}); matched {
		t.Fatal("a refused batch persisted part of itself")
	}
	if err := rules.WriteRules(ctx, []tool.RuleCandidate{{Kind: "filesystem.write", Match: ""}}); err == nil {
		t.Fatal("WriteRules(empty match) error = nil")
	}

	var zero *gate.SessionRules
	if err := zero.WriteRules(ctx, []tool.RuleCandidate{{Kind: "k", Match: "m"}}); err == nil {
		t.Fatal("nil SessionRules WriteRules error = nil, want fail closed")
	}
	if matched, err := zero.MatchesAllow(ctx, target); err == nil || matched {
		t.Fatalf("nil SessionRules MatchesAllow = %v, %v; want fail closed", matched, err)
	}
}

func TestSessionRulesConcurrentWriters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rules := gate.NewSessionRules()
	const writers = 32
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			match := fmt.Sprintf("/w/%d.go", i)
			if err := rules.WriteRules(ctx, []tool.RuleCandidate{{Kind: "filesystem.write", Match: match}}); err != nil {
				t.Errorf("WriteRules(%s) error = %v", match, err)
			}
			if _, err := rules.MatchesAllow(ctx, tool.Requirement{Kind: "filesystem.write", Match: match}); err != nil {
				t.Errorf("MatchesAllow(%s) error = %v", match, err)
			}
		}()
	}
	wg.Wait()
	for i := range writers {
		match := fmt.Sprintf("/w/%d.go", i)
		if matched, err := rules.MatchesAllow(ctx, tool.Requirement{Kind: "filesystem.write", Match: match}); err != nil || !matched {
			t.Fatalf("MatchesAllow(%s) = %v, %v; want true", match, matched, err)
		}
	}
}

func TestApproverFunc(t *testing.T) {
	t.Parallel()
	var got gate.ApprovalPrompt
	approver := gate.ApproverFunc(func(_ context.Context, prompt gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		got = prompt
		return gate.ApprovalApprove, nil
	})
	action, err := approver.RequestApproval(context.Background(), gate.ApprovalPrompt{Request: tool.Request{ToolName: "W"}})
	if err != nil || action != gate.ApprovalApprove || got.Request.ToolName != "W" {
		t.Fatalf("RequestApproval() = %q, %v (prompt %+v); want approve with the prompt passed through", action, err, got)
	}
	var zero gate.ApproverFunc
	if action, err := zero.RequestApproval(context.Background(), gate.ApprovalPrompt{}); err == nil {
		t.Fatalf("nil ApproverFunc = %q, nil; want fail closed", action)
	}
}

// countingRules wraps SessionRules to count persistence calls.
type countingRules struct {
	*gate.SessionRules
	mu     sync.Mutex
	writes [][]tool.RuleCandidate
}

func (r *countingRules) WriteRules(ctx context.Context, candidates []tool.RuleCandidate) error {
	r.mu.Lock()
	r.writes = append(r.writes, candidates)
	r.mu.Unlock()
	return r.SessionRules.WriteRules(ctx, candidates)
}

func (r *countingRules) writeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.writes)
}

// scriptedApprover answers each prompt with the next scripted action.
type scriptedApprover struct {
	mu      sync.Mutex
	actions []gate.ApprovalAction
	err     error
	prompts []gate.ApprovalPrompt
}

func (a *scriptedApprover) RequestApproval(_ context.Context, prompt gate.ApprovalPrompt) (gate.ApprovalAction, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prompts = append(a.prompts, prompt)
	if a.err != nil {
		return "", a.err
	}
	if len(a.actions) == 0 {
		return "", errors.New("unexpected prompt")
	}
	action := a.actions[0]
	a.actions = a.actions[1:]
	return action, nil
}

func (a *scriptedApprover) promptCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.prompts)
}

func workspaceWrite(path string) tool.Request {
	return tool.Request{ToolName: "WriteFile", Requirements: []tool.Requirement{{
		Kind: "filesystem.write", Scope: path, Match: path, Description: "write " + path,
		Candidates: []tool.RuleCandidate{{Kind: "filesystem.write", Match: path, Description: "write " + path}},
	}}}
}

func TestWorkspaceEvaluatorApprovalFlows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := canonicalDir(t)
	other := canonicalDir(t)
	access, err := gate.NewWorkspaceAccess(root)
	if err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(root, "a.go"), filepath.Join(root, "b.go")
	newEvaluator := func(t *testing.T, approver gate.Approver) (*gate.Evaluator, *countingRules) {
		t.Helper()
		rules := &countingRules{SessionRules: gate.NewSessionRules()}
		evaluator, err := gate.NewWorkspaceEvaluator(access, approver, rules)
		if err != nil {
			t.Fatalf("NewWorkspaceEvaluator() error = %v", err)
		}
		if !evaluator.Interactive() {
			t.Fatal("workspace evaluator must be interactive")
		}
		return evaluator, rules
	}

	t.Run("read inside needs no prompt", func(t *testing.T) {
		approver := &scriptedApprover{}
		evaluator, _ := newEvaluator(t, approver)
		resolution, err := evaluator.Authorize(ctx, tool.Request{ToolName: "ReadFile", Requirements: []tool.Requirement{{
			Kind: "filesystem.read", Scope: a, Match: a, Description: "read",
		}}})
		if err != nil || !resolution.Approved || approver.promptCount() != 0 {
			t.Fatalf("Authorize(read) = %+v, %v, prompts %d; want approved without a prompt", resolution, err, approver.promptCount())
		}
	})

	t.Run("approve once writes no rule", func(t *testing.T) {
		approver := &scriptedApprover{actions: []gate.ApprovalAction{gate.ApprovalApprove, gate.ApprovalApprove}}
		evaluator, rules := newEvaluator(t, approver)
		for i := range 2 {
			resolution, err := evaluator.Authorize(ctx, workspaceWrite(a))
			if err != nil || !resolution.Approved {
				t.Fatalf("Authorize(write #%d) = %+v, %v; want approved", i, resolution, err)
			}
		}
		if approver.promptCount() != 2 || rules.writeCount() != 0 {
			t.Fatalf("prompts = %d, rule writes = %d; want 2 prompts and no rule", approver.promptCount(), rules.writeCount())
		}
	})

	t.Run("approve always skips the next prompt for the same target only", func(t *testing.T) {
		approver := &scriptedApprover{actions: []gate.ApprovalAction{gate.ApprovalApproveAlwaysWorkspace, gate.ApprovalDeny}}
		evaluator, rules := newEvaluator(t, approver)
		if resolution, err := evaluator.Authorize(ctx, workspaceWrite(a)); err != nil || !resolution.Approved {
			t.Fatalf("Authorize(first write) = %+v, %v; want approved", resolution, err)
		}
		if rules.writeCount() != 1 {
			t.Fatalf("rule writes = %d, want 1", rules.writeCount())
		}
		if resolution, err := evaluator.Authorize(ctx, workspaceWrite(a)); err != nil || !resolution.Approved {
			t.Fatalf("Authorize(second write) = %+v, %v; want approved from the rule", resolution, err)
		}
		if approver.promptCount() != 1 {
			t.Fatalf("prompts = %d after a remembered write, want 1", approver.promptCount())
		}
		// A different file still asks.
		if resolution, err := evaluator.Authorize(ctx, workspaceWrite(b)); err != nil || resolution.Approved {
			t.Fatalf("Authorize(other write) = %+v, %v; want the scripted deny", resolution, err)
		}
		if approver.promptCount() != 2 {
			t.Fatalf("prompts = %d, want the other file to prompt", approver.promptCount())
		}
	})

	t.Run("a remembered rule never widens past the roots", func(t *testing.T) {
		approver := &scriptedApprover{}
		rules := gate.NewSessionRules()
		outside := filepath.Join(other, "x.go")
		if err := rules.WriteRules(ctx, []tool.RuleCandidate{{Kind: "filesystem.write", Match: outside}}); err != nil {
			t.Fatal(err)
		}
		evaluator, err := gate.NewWorkspaceEvaluator(access, approver, rules)
		if err != nil {
			t.Fatal(err)
		}
		resolution, err := evaluator.Authorize(ctx, workspaceWrite(outside))
		if err != nil || resolution.Approved || resolution.Denial != gate.DenialStructural {
			t.Fatalf("Authorize(outside write with a rule) = %+v, %v; want a structural deny", resolution, err)
		}
		if approver.promptCount() != 0 {
			t.Fatal("an out-of-root write must be denied without a prompt")
		}
	})

	t.Run("deny writes nothing", func(t *testing.T) {
		approver := &scriptedApprover{actions: []gate.ApprovalAction{gate.ApprovalDeny}}
		evaluator, rules := newEvaluator(t, approver)
		resolution, err := evaluator.Authorize(ctx, workspaceWrite(a))
		if err != nil || resolution.Approved || resolution.Denial != gate.DenialRefused {
			t.Fatalf("Authorize(denied write) = %+v, %v; want a refused denial", resolution, err)
		}
		if rules.writeCount() != 0 {
			t.Fatalf("rule writes = %d, want none", rules.writeCount())
		}
	})

	t.Run("approver error fails closed", func(t *testing.T) {
		approver := &scriptedApprover{err: errors.New("approver offline")}
		evaluator, rules := newEvaluator(t, approver)
		resolution, err := evaluator.Authorize(ctx, workspaceWrite(a))
		var evalErr *gate.EvaluationError
		if !errors.As(err, &evalErr) || evalErr.Kind != gate.EvaluationApprovalFailed || resolution.Approved {
			t.Fatalf("Authorize() = %+v, %v; want EvaluationApprovalFailed", resolution, err)
		}
		if rules.writeCount() != 0 {
			t.Fatal("a failed approval wrote a rule")
		}
	})

	t.Run("write outside and network are denied without a prompt", func(t *testing.T) {
		approver := &scriptedApprover{}
		evaluator, _ := newEvaluator(t, approver)
		for _, request := range []tool.Request{
			workspaceWrite(filepath.Join(other, "x.go")),
			{ToolName: "Fetch", Requirements: []tool.Requirement{{Kind: "network", Match: "example.com", Description: "fetch"}}},
		} {
			resolution, err := evaluator.Authorize(ctx, request)
			if err != nil || resolution.Approved || resolution.Denial != gate.DenialStructural {
				t.Fatalf("Authorize(%s) = %+v, %v; want a structural deny", request.ToolName, resolution, err)
			}
		}
		if approver.promptCount() != 0 {
			t.Fatal("a structural deny prompted")
		}
	})
}

func TestNewWorkspaceEvaluatorValidation(t *testing.T) {
	t.Parallel()
	access, err := gate.NewWorkspaceAccess(canonicalDir(t))
	if err != nil {
		t.Fatal(err)
	}
	approver := gate.ApproverFunc(func(context.Context, gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		return gate.ApprovalApprove, nil
	})
	if _, err := gate.NewWorkspaceEvaluator(nil, approver, gate.NewSessionRules()); err == nil {
		t.Fatal("NewWorkspaceEvaluator(nil access) error = nil")
	}
	if _, err := gate.NewWorkspaceEvaluator(&gate.WorkspaceAccess{}, approver, gate.NewSessionRules()); err == nil {
		t.Fatal("NewWorkspaceEvaluator(unconstructed access) error = nil")
	}
	var evalErr *gate.EvaluationError
	if _, err := gate.NewWorkspaceEvaluator(access, nil, gate.NewSessionRules()); !errors.As(err, &evalErr) || evalErr.Kind != gate.EvaluationApproverMissing {
		t.Fatalf("NewWorkspaceEvaluator(nil approver) error = %v, want approver_missing", err)
	}
	if _, err := gate.NewWorkspaceEvaluator(access, approver, nil); !errors.As(err, &evalErr) || evalErr.Kind != gate.EvaluationWriterMissing {
		t.Fatalf("NewWorkspaceEvaluator(nil rules) error = %v, want writer_missing", err)
	}
}
