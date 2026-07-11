package loop

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

func TestDefineValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
		kind DefinitionErrorKind
	}{
		{name: "missing name", opts: []Option{WithInference(&fakeLLM{}, testModel())}, kind: DefinitionMissingName},
		{name: "missing client", opts: []Option{WithName("agent"), WithInference(nil, testModel())}, kind: DefinitionInvalidClient},
		{name: "typed nil client", opts: []Option{WithName("agent"), WithInference((*nilInferenceClient)(nil), testModel())}, kind: DefinitionInvalidClient},
		{name: "invalid model", opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, inference.Model{})}, kind: DefinitionInvalidModel},
		{name: "nil option", opts: []Option{WithName("agent"), nil, WithInference(&fakeLLM{}, testModel())}, kind: DefinitionNilOption},
		{name: "duplicate name", opts: []Option{WithName("a"), WithName("b"), WithInference(&fakeLLM{}, testModel())}, kind: DefinitionDuplicateOption},
		{name: "negative limits", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithToolLimits(ToolLimits{Calls: -1})}, kind: DefinitionInvalidToolLimits},
		{name: "negative drain", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithDrainTimeout(-time.Second)}, kind: DefinitionInvalidDrainTimeout},
		{name: "nil middleware", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithToolMiddlewares(nil)}, kind: DefinitionInvalidMiddleware},
		{name: "nil permission factory", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithPermissionFactory(nil)}, kind: DefinitionInvalidPermission},
		{name: "invalid engine", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithEngine(Engine(99))}, kind: DefinitionInvalidEngine},
		{name: "nil runtime context", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithRuntimeContext(nil)}, kind: DefinitionInvalidRuntimeContext},
		{name: "typed nil runtime context", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithRuntimeContext((*nilRuntimeContext)(nil))}, kind: DefinitionInvalidRuntimeContext},
		{name: "empty delegate", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithDelegates("")}, kind: DefinitionInvalidDelegate},
		{name: "invalid delegation", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithDelegation(Delegation{Style: DelegationStyle(99)})}, kind: DefinitionInvalidDelegation},
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

func TestDefinitionBindValidatesIDsBeforeFactories(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		bindings tool.Bindings
		kind     BindErrorKind
	}{
		{name: "missing session ID", bindings: tool.Bindings{LoopID: mustUUID(t)}, kind: BindInvalidSessionID},
		{name: "missing loop ID", bindings: tool.Bindings{SessionID: mustUUID(t)}, kind: BindInvalidLoopID},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var permissionCalls atomic.Int32
			d := mustDefinition(t, WithPermissionFactory(func(context.Context, tool.Bindings) (PermissionGate, error) {
				permissionCalls.Add(1)
				return permissionGateStub{}, nil
			}))
			_, err := d.Bind(context.Background(), tt.bindings)
			var bindErr *BindError
			if !errors.As(err, &bindErr) || bindErr.Kind != tt.kind {
				t.Fatalf("Bind error = %T %v, want kind %q", err, err, tt.kind)
			}
			var bindingsErr *tool.InvalidBindingsError
			if !errors.As(err, &bindingsErr) {
				t.Fatalf("Bind error cause = %T %v, want *tool.InvalidBindingsError", err, err)
			}
			if permissionCalls.Load() != 0 {
				t.Fatalf("permission factory called %d times before ID validation", permissionCalls.Load())
			}
		})
	}

	toolFree := mustDefinition(t)
	_, err := toolFree.Bind(context.Background(), tool.Bindings{})
	var bindErr *BindError
	if !errors.As(err, &bindErr) || bindErr.Kind != BindInvalidSessionID {
		t.Fatalf("tool-free Bind error = %T %v", err, err)
	}
}

func TestDefinitionDefaultsAndDefensiveCopies(t *testing.T) {
	t.Parallel()
	middleware := func(ctx context.Context, inv tool.InvokableTool, args string, next tool.ToolExecuteFunc) (*tool.ToolResult, error) {
		return next(ctx, args)
	}
	delegates := []identity.AgentName{"worker", "worker", "reviewer"}
	defs := []tool.Definition{testToolDefinition("base", nil, nil)}
	d, err := Define(
		WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithSystem("system"),
		WithTools(defs...), WithToolMiddlewares(middleware), WithDelegates(delegates...),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	delegates[0] = "changed"
	defs[0] = testToolDefinition("changed", nil, nil)

	b, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if got := b.Name(); got != "agent" {
		t.Fatalf("Name = %q", got)
	}
	if got := b.Delegates(); len(got) != 2 || got[0] != "worker" || got[1] != "reviewer" {
		t.Fatalf("Delegates = %v", got)
	}
	gotDelegates := b.Delegates()
	gotDelegates[0] = "mutated"
	if b.Delegates()[0] != "worker" {
		t.Fatal("Delegates aliases returned slice")
	}
	if got := b.ToolLimits(); got != (ToolLimits{Iterations: 25, Calls: 100, Parallel: 8}) {
		t.Fatalf("ToolLimits = %+v", got)
	}
	if got := b.DrainTimeout(); got != 5*time.Second {
		t.Fatalf("DrainTimeout = %v", got)
	}
	if got := b.InitialMode(); got != "" {
		t.Fatalf("InitialMode = %q, want base", got)
	}
	if got := b.Modes(); len(got) != 1 || got[0].Name != "" || len(got[0].Tools) != 1 {
		t.Fatalf("Modes = %+v", got)
	}
	gotModes := b.Modes()
	gotModes[0].Tools = nil
	if len(b.Modes()[0].Tools) != 1 {
		t.Fatal("Modes aliases returned tool slice")
	}
}

func TestDefinitionBindBuildsDistinctDefinitionsOncePerBinding(t *testing.T) {
	t.Parallel()
	var builds atomic.Int32
	shared := testToolDefinition("shared", &builds, nil)
	d := mustDefinition(t,
		WithTools(shared), WithTools(shared),
		WithModes(Mode{Name: "plan", Tools: []tool.Definition{shared}}, Mode{Name: "build", Tools: []tool.Definition{shared}}),
		WithInitialMode("plan"),
	)
	first, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("first Bind: %v", err)
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("builds after first Bind = %d, want 1", got)
	}
	base, _ := first.Mode("")
	plan, _ := first.Mode("plan")
	build, _ := first.Mode("build")
	if got := len(base.Tools); got != 1 {
		t.Fatalf("duplicate shared definition exposed %d tools, want 1", got)
	}
	if base.Tools[0] != plan.Tools[0] || plan.Tools[0] != build.Tools[0] {
		t.Fatal("shared definition did not reuse concrete tool in one binding")
	}
	second, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("second Bind: %v", err)
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("builds after second Bind = %d, want 2", got)
	}
	secondBase, _ := second.Mode("")
	if base.Tools[0] == secondBase.Tools[0] {
		t.Fatal("separate Bind reused concrete tool")
	}
}

func TestDefinitionBindFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		defs []tool.Definition
		kind BindErrorKind
	}{
		{name: "different definitions same name", defs: []tool.Definition{testToolDefinition("same", nil, nil), testToolDefinition("same", nil, nil)}, kind: BindDuplicateDefinitionName},
		{name: "duplicate concrete names", defs: []tool.Definition{testToolDefinition("a", nil, []string{"tool"}), testToolDefinition("b", nil, []string{"tool"})}, kind: BindDuplicateToolName},
		{name: "nil info", defs: []tool.Definition{testToolDefinitionWithTool("a", &definitionTestTool{nilInfo: true})}, kind: BindInvalidToolInfo},
		{name: "empty info name", defs: []tool.Definition{testToolDefinitionWithTool("a", &definitionTestTool{})}, kind: BindInvalidToolInfo},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := mustDefinition(t, WithTools(tt.defs...))
			_, err := d.Bind(context.Background(), validToolBindings(t))
			var bindErr *BindError
			if !errors.As(err, &bindErr) || bindErr.Kind != tt.kind {
				t.Fatalf("Bind error = %T %v, want *BindError kind %q", err, err, tt.kind)
			}
		})
	}
}

func TestDefinitionPermissionFactory(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	factory := func(_ context.Context, bindings tool.Bindings) (PermissionGate, error) {
		calls.Add(1)
		if bindings.Workspace != nil {
			bindings.Workspace.Root = "mutated"
		}
		return permissionGateStub{}, nil
	}
	d := mustDefinition(t, WithPermissionFactory(factory))
	bindings := validToolBindings(t)
	bindings.Workspace = &tool.WorkspaceBinding{Root: "original"}
	b, err := d.Bind(context.Background(), bindings)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if calls.Load() != 1 || b.Permission() == nil {
		t.Fatalf("factory calls = %d permission = %v", calls.Load(), b.Permission())
	}
	if bindings.Workspace.Root != "original" {
		t.Fatal("permission factory mutated caller bindings")
	}

	nilFactory := func(context.Context, tool.Bindings) (PermissionGate, error) { return (*nilPermissionGate)(nil), nil }
	d = mustDefinition(t, WithPermissionFactory(nilFactory))
	_, err = d.Bind(context.Background(), validToolBindings(t))
	var bindErr *BindError
	if !errors.As(err, &bindErr) || bindErr.Kind != BindInvalidPermission {
		t.Fatalf("typed nil permission error = %T %v", err, err)
	}
}

func mustDefinition(t *testing.T, opts ...Option) Definition {
	t.Helper()
	base := []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel())}
	d, err := Define(append(base, opts...)...)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	return d
}

func validToolBindings(t *testing.T) tool.Bindings {
	t.Helper()
	return tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t)}
}

func testToolDefinition(name string, builds *atomic.Int32, toolNames []string) tool.Definition {
	if toolNames == nil {
		toolNames = []string{name}
	}
	return tool.NewDefinition(name, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		if builds != nil {
			builds.Add(1)
		}
		result := make([]tool.InvokableTool, len(toolNames))
		for i, toolName := range toolNames {
			result[i] = &definitionTestTool{name: toolName}
		}
		return result, nil
	})
}

func testToolDefinitionWithTool(name string, inv tool.InvokableTool) tool.Definition {
	return tool.NewDefinition(name, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{inv}, nil
	})
}

type definitionTestTool struct {
	name    string
	nilInfo bool
}

func (t *definitionTestTool) Info(context.Context) (*tool.ToolInfo, error) {
	if t.nilInfo {
		return nil, nil
	}
	return &tool.ToolInfo{Name: t.name}, nil
}
func (*definitionTestTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}

type nilInferenceClient struct{}

func (*nilInferenceClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, nil
}
func (*nilInferenceClient) Stream(context.Context, inference.Request) (*inference.StreamReader[content.Chunk], error) {
	return nil, nil
}

type nilPermissionGate struct{}

func (*nilPermissionGate) Check(context.Context, tool.InvokableTool, string, string) Effect {
	return EffectAsk
}
func (*nilPermissionGate) Grant(context.Context, string, string, tool.ApprovalScope) error {
	return nil
}

type nilRuntimeContext struct{}

func (*nilRuntimeContext) Blocks(context.Context) []content.Block { return nil }
