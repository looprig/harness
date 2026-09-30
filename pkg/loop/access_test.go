package loop

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// fakeAccessGate is a minimal AccessGate for wiring tests.
type fakeAccessGate struct{ calls int }

func (g *fakeAccessGate) Authorize(context.Context, tool.Request) (gate.Resolution, error) {
	g.calls++
	return gate.Resolution{Approved: true}, nil
}

func TestWithAccessGateBinds(t *testing.T) {
	t.Parallel()
	access := &fakeAccessGate{}
	d, err := Define(
		WithName("agent"),
		WithInference(&fakeLLM{}, testModel()),
		WithAccessGate(access),
		WithPolicyRevision("rev-1"),
	)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	bound, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if bound.Access() != AccessGate(access) {
		t.Fatalf("bound.Access() = %v, want the configured gate", bound.Access())
	}
}

func TestDefineAccessGateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
		kind DefinitionErrorKind
	}{
		{
			name: "nil access gate",
			opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithAccessGate(nil), WithPolicyRevision("rev-1")},
			kind: DefinitionInvalidAccessGate,
		},
		{
			name: "opaque access gate lacks revision",
			opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithAccessGate(&fakeAccessGate{})},
			kind: DefinitionMissingPolicyRevision,
		},
		{
			name: "duplicate access gate",
			opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithAccessGate(&fakeAccessGate{}), WithAccessGate(&fakeAccessGate{}), WithPolicyRevision("rev-1")},
			kind: DefinitionDuplicateOption,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Define(tt.opts...)
			var definitionErr *DefinitionError
			if !errors.As(err, &definitionErr) || definitionErr.Kind != tt.kind {
				t.Fatalf("Define() error = %T %v, want *DefinitionError kind %q", err, err, tt.kind)
			}
		})
	}
}

func TestPreparedCallContextRoundTrip(t *testing.T) {
	t.Parallel()
	id := mustUUID(t)
	prepared := tool.PreparedCall{
		ExecutionID: id,
		Request:     tool.Request{ToolName: "Bash", Summary: "run git status"},
		Artifact:    tool.TokenArtifact{Token: "artifact"},
		Grants:      []string{"grant-token"},
	}

	if _, ok := PreparedCallFromContext(context.Background()); ok {
		t.Fatal("PreparedCallFromContext(background) ok = true, want false")
	}
	ctx := WithPreparedCall(context.Background(), prepared)
	got, ok := PreparedCallFromContext(ctx)
	if !ok {
		t.Fatal("PreparedCallFromContext ok = false, want true")
	}
	if got.ExecutionID != id || got.Request.ToolName != "Bash" || len(got.Grants) != 1 || got.Grants[0] != "grant-token" {
		t.Fatalf("PreparedCallFromContext = %+v, want the stored contract", got)
	}
	if artifact, ok := got.Artifact.(tool.TokenArtifact); !ok || artifact.Token != "artifact" {
		t.Fatalf("Artifact = %#v, want the stored TokenArtifact", got.Artifact)
	}
}

func TestGateApproverFailsClosedWithoutRequester(t *testing.T) {
	t.Parallel()
	_, err := GateApprover().RequestApproval(context.Background(), gate.ApprovalPrompt{})
	var ctxErr *ApprovalContextError
	if !errors.As(err, &ctxErr) {
		t.Fatalf("RequestApproval() error = %T %v, want *ApprovalContextError", err, err)
	}
}

func TestGateApproverRoutesToInstalledRequester(t *testing.T) {
	t.Parallel()
	var got gate.ApprovalPrompt
	requester := func(_ context.Context, prompt gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		got = prompt
		return gate.ApprovalApproveAlwaysWorkspace, nil
	}
	ctx := WithApprovalRequester(context.Background(), requester)
	prompt := gate.ApprovalPrompt{Request: tool.Request{ToolName: "Bash", ExecutionID: uuid.UUID{}.String()}}
	action, err := GateApprover().RequestApproval(ctx, prompt)
	if err != nil {
		t.Fatalf("RequestApproval() error = %v", err)
	}
	if action != gate.ApprovalApproveAlwaysWorkspace {
		t.Fatalf("action = %q, want %q", action, gate.ApprovalApproveAlwaysWorkspace)
	}
	if got.Request.ToolName != "Bash" {
		t.Fatalf("routed prompt = %+v, want the caller's prompt", got)
	}
}

// TestOverrideBoundAccessReplacesGate proves the binding-time per-loop gate
// override: the overridden view resolves the injected gate, the original bound
// view keeps its own definition gate, and a nil override is rejected.
func TestOverrideBoundAccessReplacesGate(t *testing.T) {
	t.Parallel()
	own := &fakeAccessGate{}
	other := &fakeAccessGate{}
	d, err := Define(
		WithName("agent"),
		WithInference(&fakeLLM{}, testModel()),
		WithAccessGate(own),
		WithPolicyRevision("rev-1"),
	)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	bound, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	overridden, err := OverrideBoundAccess(bound, other)
	if err != nil {
		t.Fatalf("OverrideBoundAccess() error = %v", err)
	}
	if overridden.Access() != AccessGate(other) {
		t.Fatalf("overridden Access() = %v, want the override gate", overridden.Access())
	}
	if bound.Access() != AccessGate(own) {
		t.Fatalf("original Access() = %v, want the definition's own gate (override must not leak back)", bound.Access())
	}

	if _, err := OverrideBoundAccess(bound, nil); err == nil {
		t.Fatal("OverrideBoundAccess(nil) error = nil, want rejection")
	}
}

// TestBoundAccessInheritsOwnDefinitionGate pins the subagent inheritance rule:
// binding a definition WITHOUT an override resolves that definition's OWN gate
// — two definitions bound in the same session never share or escalate to each
// other's gates, and a definition without a gate stays gateless (the runner
// fails closed on it).
func TestBoundAccessInheritsOwnDefinitionGate(t *testing.T) {
	t.Parallel()
	parentGate := &fakeAccessGate{}
	childGate := &fakeAccessGate{}
	parent, err := Define(WithName("parent"), WithInference(&fakeLLM{}, testModel()), WithAccessGate(parentGate), WithPolicyRevision("rev-1"))
	if err != nil {
		t.Fatalf("Define(parent) error = %v", err)
	}
	child, err := Define(WithName("child"), WithInference(&fakeLLM{}, testModel()), WithAccessGate(childGate), WithPolicyRevision("rev-1"))
	if err != nil {
		t.Fatalf("Define(child) error = %v", err)
	}
	gateless, err := Define(WithName("gateless"), WithInference(&fakeLLM{}, testModel()))
	if err != nil {
		t.Fatalf("Define(gateless) error = %v", err)
	}

	parentBound, err := parent.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind(parent) error = %v", err)
	}
	childBound, err := child.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind(child) error = %v", err)
	}
	gatelessBound, err := gateless.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind(gateless) error = %v", err)
	}

	if childBound.Access() != AccessGate(childGate) {
		t.Fatalf("child Access() = %v, want the child's own gate", childBound.Access())
	}
	if parentBound.Access() != AccessGate(parentGate) {
		t.Fatalf("parent Access() = %v, want the parent's own gate", parentBound.Access())
	}
	if gatelessBound.Access() != nil {
		t.Fatalf("gateless Access() = %v, want nil (runner fails closed)", gatelessBound.Access())
	}
}

// TestWithReadOnlyAccessInstallsReadOnlyGate proves the one-line read-only
// gate: it binds a headless gate that approves reads within the root and
// denies everything else, and it needs no WithPolicyRevision because its whole
// identity (the canonical roots) is hashed into PolicyRevision.
func TestWithReadOnlyAccessInstallsReadOnlyGate(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	d, err := Define(
		WithName("agent"),
		WithInference(&fakeLLM{}, testModel()),
		WithReadOnlyAccess(root),
	)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	bound, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	access := bound.Access()
	if access == nil {
		t.Fatal("bound.Access() = nil, want the read-only gate")
	}
	authorize := func(kind, scope string) gate.Resolution {
		t.Helper()
		resolution, err := access.Authorize(context.Background(), tool.Request{ToolName: "T", Requirements: []tool.Requirement{{
			Kind: kind, Scope: scope, Match: scope, Description: kind + " " + scope,
		}}})
		if err != nil {
			t.Fatalf("Authorize(%s %s) error = %v", kind, scope, err)
		}
		return resolution
	}
	if !authorize("filesystem.read", filepath.Join(root, "README.md")).Approved {
		t.Fatal("read inside the root was not approved")
	}
	if authorize("filesystem.read", filepath.Dir(root)).Approved {
		t.Fatal("read outside the root was approved")
	}
	if authorize("filesystem.write", filepath.Join(root, "README.md")).Approved {
		t.Fatal("write inside the root was approved")
	}
}

func TestWithReadOnlyAccessPolicyRevisionTracksRoots(t *testing.T) {
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
	gateless := define().PolicyRevision()
	one := define(WithReadOnlyAccess(first)).PolicyRevision()
	oneAgain := define(WithReadOnlyAccess(first, first)).PolicyRevision()
	both := define(WithReadOnlyAccess(first, second)).PolicyRevision()
	bothReordered := define(WithReadOnlyAccess(second, first)).PolicyRevision()
	withRevision := define(WithReadOnlyAccess(first), WithPolicyRevision("rev-1")).PolicyRevision()

	if one == gateless {
		t.Fatal("adding read-only access did not change PolicyRevision")
	}
	if one != oneAgain || both != bothReordered {
		t.Fatal("PolicyRevision depends on root order or duplicates, want the canonical root set only")
	}
	if one == both {
		t.Fatal("adding a read-only root did not change PolicyRevision")
	}
	if withRevision == one {
		t.Fatal("an explicit WithPolicyRevision must still contribute to PolicyRevision")
	}
}

func TestWithReadOnlyAccessValidation(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	tests := []struct {
		name string
		opts []Option
		kind DefinitionErrorKind
	}{
		{name: "no roots", opts: []Option{WithReadOnlyAccess()}, kind: DefinitionInvalidAccessGate},
		{name: "missing root", opts: []Option{WithReadOnlyAccess(filepath.Join(root, "missing"))}, kind: DefinitionInvalidAccessGate},
		{name: "combined with an access gate", opts: []Option{WithReadOnlyAccess(root), WithAccessGate(&fakeAccessGate{}), WithPolicyRevision("rev-1")}, kind: DefinitionDuplicateOption},
		{name: "access gate then read-only", opts: []Option{WithAccessGate(&fakeAccessGate{}), WithReadOnlyAccess(root), WithPolicyRevision("rev-1")}, kind: DefinitionDuplicateOption},
		{name: "twice", opts: []Option{WithReadOnlyAccess(root), WithReadOnlyAccess(root)}, kind: DefinitionDuplicateOption},
		{name: "middlewares still need a revision", opts: []Option{WithReadOnlyAccess(root), WithToolMiddlewares(func(ctx context.Context, t tool.InvokableTool, args string, next tool.ToolExecuteFunc) (*tool.ToolResult, error) {
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
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
