package loop

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

func workspaceWriteRequest(path string) tool.Request {
	return tool.Request{ToolName: "WriteFile", Requirements: []tool.Requirement{{
		Kind: "filesystem.write", Scope: path, Match: path, Description: "write " + path,
		Candidates: []tool.RuleCandidate{{Kind: "filesystem.write", Match: path, Description: "write " + path}},
	}}}
}

func defineWorkspace(t *testing.T, access WorkspaceAccess, opts ...Option) Definition {
	t.Helper()
	d, err := Define(append([]Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithWorkspaceAccess(access)}, opts...)...)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	return d
}

func bindAccess(t *testing.T, d Definition) AccessGate {
	t.Helper()
	bound, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if bound.Access() == nil {
		t.Fatal("bound.Access() = nil, want the workspace gate")
	}
	return bound.Access()
}

// alwaysApprover answers "Approve always" and counts prompts.
type alwaysApprover struct{ prompts atomic.Int32 }

func (a *alwaysApprover) RequestApproval(context.Context, gate.ApprovalPrompt) (gate.ApprovalAction, error) {
	a.prompts.Add(1)
	return gate.ApprovalApproveAlwaysWorkspace, nil
}

func TestWithWorkspaceAccessReadsRunWritesAskOthersDenied(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	approver := &alwaysApprover{}
	access := bindAccess(t, defineWorkspace(t, WorkspaceAccess{Roots: []string{root}, Approver: approver}))
	ctx := context.Background()

	read := tool.Request{ToolName: "ReadFile", Requirements: []tool.Requirement{{
		Kind: "filesystem.read", Scope: filepath.Join(root, "a.go"), Match: filepath.Join(root, "a.go"), Description: "read",
	}}}
	if resolution, err := access.Authorize(ctx, read); err != nil || !resolution.Approved || approver.prompts.Load() != 0 {
		t.Fatalf("Authorize(read) = %+v, %v; want approved without a prompt", resolution, err)
	}
	if resolution, err := access.Authorize(ctx, workspaceWriteRequest(filepath.Join(root, "a.go"))); err != nil || !resolution.Approved {
		t.Fatalf("Authorize(write) = %+v, %v; want approved by the approver", resolution, err)
	}
	if approver.prompts.Load() != 1 {
		t.Fatalf("prompts = %d, want 1", approver.prompts.Load())
	}
	if resolution, err := access.Authorize(ctx, workspaceWriteRequest(filepath.Dir(root))); err != nil || resolution.Approved || resolution.Denial != gate.DenialStructural {
		t.Fatalf("Authorize(write outside) = %+v, %v; want a structural deny", resolution, err)
	}
	if approver.prompts.Load() != 1 {
		t.Fatal("a write outside the roots prompted")
	}
}

// TestWithWorkspaceAccessDefaultRulesArePerBoundLoop pins the default rule
// scope: an "always" answer spares later prompts on the same bound loop
// (one session's loop) but never reaches a loop bound for another session.
func TestWithWorkspaceAccessDefaultRulesArePerBoundLoop(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	target := filepath.Join(root, "a.go")
	approver := &alwaysApprover{}
	d := defineWorkspace(t, WorkspaceAccess{Roots: []string{root}, Approver: approver})
	ctx := context.Background()

	first := bindAccess(t, d)
	for range 2 {
		if resolution, err := first.Authorize(ctx, workspaceWriteRequest(target)); err != nil || !resolution.Approved {
			t.Fatalf("Authorize(write) = %+v, %v; want approved", resolution, err)
		}
	}
	if got := approver.prompts.Load(); got != 1 {
		t.Fatalf("prompts on one bound loop = %d, want 1 (always remembered)", got)
	}
	second := bindAccess(t, d)
	if resolution, err := second.Authorize(ctx, workspaceWriteRequest(target)); err != nil || !resolution.Approved {
		t.Fatalf("Authorize(write, second bind) = %+v, %v; want approved", resolution, err)
	}
	if got := approver.prompts.Load(); got != 2 {
		t.Fatalf("prompts after a second bind = %d, want 2 (default rules never cross bound loops)", got)
	}
}

func TestWithWorkspaceAccessSuppliedRulesAreShared(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	target := filepath.Join(root, "a.go")
	approver := &alwaysApprover{}
	d := defineWorkspace(t, WorkspaceAccess{Roots: []string{root}, Approver: approver, Rules: gate.NewSessionRules()})
	ctx := context.Background()
	for range 2 {
		if resolution, err := bindAccess(t, d).Authorize(ctx, workspaceWriteRequest(target)); err != nil || !resolution.Approved {
			t.Fatalf("Authorize(write) = %+v, %v; want approved", resolution, err)
		}
	}
	if got := approver.prompts.Load(); got != 1 {
		t.Fatalf("prompts = %d, want 1: a supplied store is shared by every bind", got)
	}
}

// TestWithWorkspaceAccessDefaultApproverIsTheLoopGate proves a nil Approver
// selects GateApprover: outside a live loop call it fails closed, and inside
// one it routes to the loop's approval requester.
func TestWithWorkspaceAccessDefaultApproverIsTheLoopGate(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	access := bindAccess(t, defineWorkspace(t, WorkspaceAccess{Roots: []string{root}}))
	request := workspaceWriteRequest(filepath.Join(root, "a.go"))

	_, err := access.Authorize(context.Background(), request)
	var ctxErr *ApprovalContextError
	if !errors.As(err, &ctxErr) {
		t.Fatalf("Authorize(outside a loop) error = %T %v, want *ApprovalContextError", err, err)
	}

	var asked atomic.Int32
	ctx := WithApprovalRequester(context.Background(), func(context.Context, gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		asked.Add(1)
		return gate.ApprovalDeny, nil
	})
	resolution, err := access.Authorize(ctx, request)
	if err != nil || resolution.Approved || resolution.Denial != gate.DenialRefused || asked.Load() != 1 {
		t.Fatalf("Authorize(in a loop) = %+v, %v, asked %d; want the loop gate's deny", resolution, err, asked.Load())
	}
}

// revisionedRules is a rule store that participates in policy identity.
type revisionedRules struct {
	*gate.SessionRules
	revision string
}

func (r revisionedRules) PolicyRevision() string { return r.revision }

func TestWithWorkspaceAccessPolicyRevision(t *testing.T) {
	t.Parallel()
	first, second := canonicalTempDir(t), canonicalTempDir(t)
	define := func(opts ...Option) Definition {
		t.Helper()
		d, err := Define(append([]Option{WithName("agent"), WithInference(&fakeLLM{}, testModel())}, opts...)...)
		if err != nil {
			t.Fatalf("Define() error = %v", err)
		}
		return d
	}
	approver := gate.ApproverFunc(func(context.Context, gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		return gate.ApprovalApproveAlwaysWorkspace, nil
	})
	rules := gate.NewSessionRules()
	d := define(WithWorkspaceAccess(WorkspaceAccess{Roots: []string{first}, Rules: rules}))
	before := d.PolicyRevision()

	// An "always" answer lands in the rules and must not move the revision.
	resolution, err := bindAccess(t, d).Authorize(WithApprovalRequester(context.Background(), approver.RequestApproval),
		workspaceWriteRequest(filepath.Join(first, "a.go")))
	if err != nil || !resolution.Approved {
		t.Fatalf("Authorize(write) = %+v, %v", resolution, err)
	}
	if matched, _ := rules.MatchesAllow(context.Background(), tool.Requirement{Kind: "filesystem.write", Match: filepath.Join(first, "a.go")}); !matched {
		t.Fatal("the always answer was not remembered")
	}
	if after := d.PolicyRevision(); after != before {
		t.Fatal("an always answer changed PolicyRevision")
	}

	defaults := define(WithWorkspaceAccess(WorkspaceAccess{Roots: []string{first}})).PolicyRevision()
	withApprover := define(WithWorkspaceAccess(WorkspaceAccess{Roots: []string{first}, Approver: approver})).PolicyRevision()
	reordered := define(WithWorkspaceAccess(WorkspaceAccess{Roots: []string{second, first, first}})).PolicyRevision()
	both := define(WithWorkspaceAccess(WorkspaceAccess{Roots: []string{first, second}})).PolicyRevision()
	gateless := define().PolicyRevision()
	readOnly := define(WithReadOnlyAccess(first)).PolicyRevision()
	revisioned := define(WithWorkspaceAccess(WorkspaceAccess{Roots: []string{first}, Rules: revisionedRules{gate.NewSessionRules(), "r1"}})).PolicyRevision()
	revisionedAgain := define(WithWorkspaceAccess(WorkspaceAccess{Roots: []string{first}, Rules: revisionedRules{gate.NewSessionRules(), "r1"}})).PolicyRevision()
	revisionedOther := define(WithWorkspaceAccess(WorkspaceAccess{Roots: []string{first}, Rules: revisionedRules{gate.NewSessionRules(), "r2"}})).PolicyRevision()

	if defaults != before || withApprover != before {
		t.Fatal("PolicyRevision depends on the approver or on in-memory rules, want roots and shape only")
	}
	if reordered != both {
		t.Fatal("PolicyRevision depends on root order or duplicates")
	}
	if both == before {
		t.Fatal("adding a root did not change PolicyRevision")
	}
	if before == gateless || before == readOnly {
		t.Fatal("workspace access shares a PolicyRevision with a gateless or read-only loop")
	}
	if revisioned == before || revisioned != revisionedAgain || revisioned == revisionedOther {
		t.Fatal("a PolicyRevisioner rule store's revision does not participate in PolicyRevision")
	}
}

func TestWithWorkspaceAccessValidation(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	ws := WithWorkspaceAccess(WorkspaceAccess{Roots: []string{root}})
	var nilApprover gate.ApproverFunc
	var nilRules *gate.SessionRules
	tests := []struct {
		name string
		opts []Option
		kind DefinitionErrorKind
	}{
		{name: "no roots", opts: []Option{WithWorkspaceAccess(WorkspaceAccess{})}, kind: DefinitionInvalidAccessGate},
		{name: "missing root", opts: []Option{WithWorkspaceAccess(WorkspaceAccess{Roots: []string{filepath.Join(root, "missing")}})}, kind: DefinitionInvalidAccessGate},
		{name: "filesystem root", opts: []Option{WithWorkspaceAccess(WorkspaceAccess{Roots: []string{"/"}})}, kind: DefinitionInvalidAccessGate},
		{name: "typed nil approver", opts: []Option{WithWorkspaceAccess(WorkspaceAccess{Roots: []string{root}, Approver: nilApprover})}, kind: DefinitionInvalidAccessGate},
		{name: "typed nil rules", opts: []Option{WithWorkspaceAccess(WorkspaceAccess{Roots: []string{root}, Rules: nilRules})}, kind: DefinitionInvalidAccessGate},
		{name: "then access gate", opts: []Option{ws, WithAccessGate(&fakeAccessGate{}), WithPolicyRevision("rev-1")}, kind: DefinitionDuplicateOption},
		{name: "access gate then workspace", opts: []Option{WithAccessGate(&fakeAccessGate{}), ws, WithPolicyRevision("rev-1")}, kind: DefinitionDuplicateOption},
		{name: "then read-only", opts: []Option{ws, WithReadOnlyAccess(root)}, kind: DefinitionDuplicateOption},
		{name: "read-only then workspace", opts: []Option{WithReadOnlyAccess(root), ws}, kind: DefinitionDuplicateOption},
		{name: "twice", opts: []Option{ws, ws}, kind: DefinitionDuplicateOption},
		{name: "middlewares still need a revision", opts: []Option{ws, WithToolMiddlewares(func(ctx context.Context, t tool.InvokableTool, args string, next tool.ToolExecuteFunc) (*tool.ToolResult, error) {
			return next(ctx, args)
		})}, kind: DefinitionMissingPolicyRevision},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Define(append([]Option{WithName("agent"), WithInference(&fakeLLM{}, testModel())}, tt.opts...)...)
			var definitionErr *DefinitionError
			if !errors.As(err, &definitionErr) || definitionErr.Kind != tt.kind {
				t.Fatalf("Define() error = %T %v, want *DefinitionError kind %q", err, err, tt.kind)
			}
		})
	}
	// Alone it needs no WithPolicyRevision.
	if _, err := Define(WithName("agent"), WithInference(&fakeLLM{}, testModel()), ws); err != nil {
		t.Fatalf("Define(workspace access alone) error = %v", err)
	}
}

// mutableRevisionedRules is a revisioned store whose revision can move, as a
// durable rule file's digest does when its rules change.
type mutableRevisionedRules struct {
	*gate.SessionRules
	revision atomic.Value
}

func newMutableRevisionedRules(revision string) *mutableRevisionedRules {
	r := &mutableRevisionedRules{SessionRules: gate.NewSessionRules()}
	r.revision.Store(revision)
	return r
}

func (r *mutableRevisionedRules) PolicyRevision() string { return r.revision.Load().(string) }

// TestWithWorkspaceAccessRevisionedRulesFailClosedOnChange: a revisioned
// store that changes after Define must not be used under the stale identity
// — existing evaluators fail closed and a new Bind is refused, with a typed
// error, until the store reports the defined revision again.
func TestWithWorkspaceAccessRevisionedRulesFailClosedOnChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := canonicalTempDir(t)
	target := filepath.Join(root, "a.go")
	rules := newMutableRevisionedRules("r1")
	approver := &alwaysApprover{}
	d := defineWorkspace(t, WorkspaceAccess{Roots: []string{root}, Approver: approver, Rules: rules})
	defined := d.PolicyRevision()
	access := bindAccess(t, d)

	// The store's rules change: a write is newly covered and it reports r2.
	if err := rules.WriteRules(ctx, []tool.RuleCandidate{{Kind: "filesystem.write", Match: target}}); err != nil {
		t.Fatal(err)
	}
	rules.revision.Store("r2")

	resolution, err := access.Authorize(ctx, workspaceWriteRequest(target))
	var revisionErr *WorkspaceRulesRevisionError
	if !errors.As(err, &revisionErr) || resolution.Approved {
		t.Fatalf("Authorize(after the store changed) = %+v, %v; want a WorkspaceRulesRevisionError", resolution, err)
	}
	if revisionErr.Defined != "r1" || revisionErr.Current != "r2" {
		t.Fatalf("WorkspaceRulesRevisionError = %+v, want defined r1, current r2", revisionErr)
	}
	if approver.prompts.Load() != 0 {
		t.Fatal("a stale-identity evaluation reached the approver")
	}
	// Reads need no rule, so they are unaffected.
	read := tool.Request{ToolName: "ReadFile", Requirements: []tool.Requirement{{Kind: "filesystem.read", Scope: target, Match: target, Description: "read"}}}
	if resolution, err := access.Authorize(ctx, read); err != nil || !resolution.Approved {
		t.Fatalf("Authorize(read) = %+v, %v; want approved", resolution, err)
	}
	if _, err := d.Bind(ctx, validToolBindings(t)); !errors.As(err, &revisionErr) {
		t.Fatalf("Bind(after the store changed) error = %v, want a WorkspaceRulesRevisionError", err)
	}
	if d.PolicyRevision() != defined {
		t.Fatal("PolicyRevision moved after Define")
	}

	// Back at the defined revision, the store is usable again.
	rules.revision.Store("r1")
	if resolution, err := bindAccess(t, d).Authorize(ctx, workspaceWriteRequest(target)); err != nil || !resolution.Approved {
		t.Fatalf("Authorize(at the defined revision) = %+v, %v; want approved by the stored rule", resolution, err)
	}
	if approver.prompts.Load() != 0 {
		t.Fatal("the stored rule did not spare the prompt")
	}
	// A rebuilt definition adopts the new revision and a new identity.
	rules.revision.Store("r2")
	rebuilt := defineWorkspace(t, WorkspaceAccess{Roots: []string{root}, Approver: approver, Rules: rules})
	if rebuilt.PolicyRevision() == defined {
		t.Fatal("a rebuilt definition kept the old identity")
	}
	bindAccess(t, rebuilt)
}
