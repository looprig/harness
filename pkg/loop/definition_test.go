package loop

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/security"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
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
		{name: "invalid model", opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, model.Model{})}, kind: DefinitionInvalidModel},
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
		{name: "empty policy revision", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithPolicyRevision("")}, kind: DefinitionInvalidPolicyRevision},
		{name: "opaque permission lacks revision", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithPermissionFactory(func(context.Context, tool.Bindings) (PermissionGate, error) { return permissionGateStub{}, nil })}, kind: DefinitionMissingPolicyRevision},
		{name: "opaque middleware lacks revision", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithToolMiddlewares(func(ctx context.Context, inv tool.InvokableTool, args string, next tool.ToolExecuteFunc) (*tool.ToolResult, error) {
			return next(ctx, args)
		})}, kind: DefinitionMissingPolicyRevision},
		{name: "opaque runtime context lacks revision", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithRuntimeContext(&fakeRuntimeContextProvider{})}, kind: DefinitionMissingPolicyRevision},
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

func TestDefineRequiresDurableModelKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		configure func(model.Model) []Option
		wantKind  DefinitionErrorKind
		wantField model.ModelKeyField
	}{
		{
			name: "base model requires provider",
			configure: func(model model.Model) []Option {
				return []Option{WithName("agent"), WithInference(&fakeLLM{}, model)}
			},
			wantKind:  DefinitionInvalidModel,
			wantField: model.ModelKeyFieldProvider,
		},
		{
			name: "mode model requires provider",
			configure: func(model model.Model) []Option {
				return []Option{
					WithName("agent"), WithInference(&fakeLLM{}, testModel()),
					WithModes(Mode{Name: "alternate", Model: model}), WithInitialMode("alternate"),
				}
			},
			wantKind:  DefinitionInvalidMode,
			wantField: model.ModelKeyFieldProvider,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			candidate := testModel()
			candidate.Provider = ""
			_, err := Define(tt.configure(candidate)...)
			var definitionErr *DefinitionError
			if !errors.As(err, &definitionErr) || definitionErr.Kind != tt.wantKind {
				t.Fatalf("Define error = %T %v, want *DefinitionError kind %q", err, err, tt.wantKind)
			}
			var keyErr *model.ModelKeyValidationError
			if !errors.As(err, &keyErr) || keyErr.Field != tt.wantField {
				t.Fatalf("Define cause = %T %v, want *ModelKeyValidationError field %q", err, err, tt.wantField)
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
			d := mustDefinition(t, WithPolicyRevision("test"), WithPermissionFactory(func(context.Context, tool.Bindings) (PermissionGate, error) {
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
		WithTools(defs...), WithPolicyRevision("test"), WithToolMiddlewares(middleware), WithDelegates(delegates...),
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
	d := mustDefinition(t, WithPolicyRevision("test"), WithPermissionFactory(factory))
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
	d = mustDefinition(t, WithPolicyRevision("test"), WithPermissionFactory(nilFactory))
	_, err = d.Bind(context.Background(), validToolBindings(t))
	var bindErr *BindError
	if !errors.As(err, &bindErr) || bindErr.Kind != BindInvalidPermission {
		t.Fatalf("typed nil permission error = %T %v", err, err)
	}
}

func TestDefinitionPermissionFactoryRequiresAndReceivesSecurityLimit(t *testing.T) {
	t.Parallel()
	source := security.New()
	var received security.LimitSource
	d := mustDefinition(t, WithPolicyRevision("securityLimit"), WithPermissionFactory(func(_ context.Context, bindings tool.Bindings) (PermissionGate, error) {
		received = bindings.SecurityLimit
		return permissionGateStub{}, nil
	}))
	bindings := validToolBindings(t)
	bindings.SecurityLimit = source
	if _, err := d.Bind(context.Background(), bindings); err != nil {
		t.Fatal(err)
	}
	if received != source {
		t.Fatalf("factory securityLimit = %p, want exact source %p", received, source)
	}
	bindings.SecurityLimit = nil
	_, err := d.Bind(context.Background(), bindings)
	var bindErr *BindError
	if !errors.As(err, &bindErr) || bindErr.Kind != BindInvalidSecurityLimit {
		t.Fatalf("missing securityLimit error = %v, want BindInvalidSecurityLimit", err)
	}
	var typedNil *security.Limit
	bindings.SecurityLimit = typedNil
	_, err = d.Bind(context.Background(), bindings)
	if !errors.As(err, &bindErr) || bindErr.Kind != BindInvalidSecurityLimit {
		t.Fatalf("typed-nil securityLimit error = %v, want BindInvalidSecurityLimit", err)
	}
}

type fixedPermissionGate struct {
	effect   Effect
	grantErr error
}

func (g *fixedPermissionGate) Check(context.Context, tool.InvokableTool, string, string) Effect {
	return g.effect
}
func (g *fixedPermissionGate) Grant(context.Context, string, string, tool.ApprovalScope) error {
	return g.grantErr
}

func TestAttenuateBoundPermissionMostRestrictiveAndLive(t *testing.T) {
	t.Parallel()
	child := &fixedPermissionGate{effect: EffectAutoApprove}
	parent := &fixedPermissionGate{effect: EffectAsk}
	d := mustDefinition(t, WithPolicyRevision("permissions"), WithPermissionFactory(func(context.Context, tool.Bindings) (PermissionGate, error) {
		return child, nil
	}))
	bound, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatal(err)
	}
	attenuated := AttenuateBoundPermission(bound, parent)
	if got := attenuated.Permission().Check(context.Background(), nil, "Bash", `{}`); got != EffectAsk {
		t.Fatalf("AutoApprove child + Ask parent = %v, want Ask", got)
	}
	parent.effect = EffectDeny
	if got := attenuated.Permission().Check(context.Background(), nil, "Bash", `{}`); got != EffectDeny {
		t.Fatalf("live Deny parent = %v, want Deny", got)
	}
	parent.effect = EffectAutoApprove
	child.effect = EffectAsk
	if got := attenuated.Permission().Check(context.Background(), nil, "Bash", `{}`); got != EffectAsk {
		t.Fatalf("Ask child + AutoApprove parent = %v, want Ask", got)
	}
	child.effect = Effect(255)
	if got := attenuated.Permission().Check(context.Background(), nil, "Bash", `{}`); got != EffectDeny {
		t.Fatalf("invalid child effect = %v, want fail-secure Deny", got)
	}
	child.effect = EffectAsk

	sentinel := errors.New("parent grant")
	parent.grantErr = sentinel
	if err := attenuated.Permission().Grant(context.Background(), "Bash", `{}`, tool.ScopeSession); !errors.Is(err, sentinel) {
		t.Fatalf("Grant error = %v, want original parent error", err)
	}
	if got := AttenuateBoundPermission(bound, nil).Permission(); got != nil {
		t.Fatalf("nil parent permission = %T, want fail-secure nil", got)
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

// TestPolicyRevisionDigest asserts PolicyRevision produces a stable, non-empty digest for a
// normal (total, marshalable) projection — the invariant the panic-on-marshal-failure guard
// protects. Equal definitions hash equal; a policy-affecting difference (the system prompt)
// changes the digest, so it can never silently collapse to a constant (e.g. sha256(nil)).
func TestPolicyRevisionDigest(t *testing.T) {
	t.Parallel()
	base := mustDefinition(t, WithSystem("be helpful"))
	same := mustDefinition(t, WithSystem("be helpful"))
	different := mustDefinition(t, WithSystem("be terse"))

	got := base.PolicyRevision()
	if got == "" {
		t.Fatal("PolicyRevision() = empty, want a non-empty digest")
	}
	if got != base.PolicyRevision() {
		t.Error("PolicyRevision() is not stable across calls on the same definition")
	}
	if got != same.PolicyRevision() {
		t.Error("PolicyRevision() differs for identical definitions, want equal digests")
	}
	if got == different.PolicyRevision() {
		t.Error("PolicyRevision() did not change for a differing system prompt (digest collapsed?)")
	}
}

func TestPolicyRevisionIncludesNormalizedProducedToolNames(t *testing.T) {
	t.Parallel()

	bundle := func(names ...string) tool.Definition {
		return tool.NewBundleDefinition("bundle", names, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return nil, nil
		})
	}
	baseA := mustDefinition(t, WithTools(bundle("Write", " Read ")))
	baseSameSet := mustDefinition(t, WithTools(bundle("Read", "Write")))
	baseDrift := mustDefinition(t, WithTools(bundle("Read", "Edit")))
	if baseA.PolicyRevision() != baseSameSet.PolicyRevision() {
		t.Fatal("PolicyRevision() changed for reordered/whitespace-equivalent produced names")
	}
	if baseA.PolicyRevision() == baseDrift.PolicyRevision() {
		t.Fatal("PolicyRevision() ignored base produced-name drift")
	}

	modeA := mustDefinition(t,
		WithModes(Mode{Name: "plan"}, Mode{Name: "review", Tools: []tool.Definition{bundle("Read")}}),
		WithInitialMode("plan"),
	)
	modeDrift := mustDefinition(t,
		WithModes(Mode{Name: "plan"}, Mode{Name: "review", Tools: []tool.Definition{bundle("Inspect")}}),
		WithInitialMode("plan"),
	)
	if modeA.PolicyRevision() == modeDrift.PolicyRevision() {
		t.Fatal("PolicyRevision() ignored noninitial-mode produced-name drift")
	}
}

func TestFingerprintInitialNormalizesProducedToolNames(t *testing.T) {
	t.Parallel()

	bundle := tool.NewBundleDefinition("bundle", []string{" Write ", "Read"}, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return nil, nil
	})
	fingerprint := mustDefinition(t, WithTools(bundle)).FingerprintInitial()
	if got, want := fingerprint.ToolNames, []string{"Write", "Read"}; !slices.Equal(got, want) {
		t.Fatalf("FingerprintInitial().ToolNames = %q, want %q", got, want)
	}
}

func validToolBindings(t *testing.T) tool.Bindings {
	t.Helper()
	return tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t), SecurityLimit: security.New()}
}

func testToolDefinition(name string, builds *atomic.Int32, toolNames []string) tool.Definition {
	if toolNames == nil {
		toolNames = []string{name}
	}
	return tool.NewBundleDefinition(name, toolNames, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
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
func (*nilInferenceClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
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

func TestEffectiveSystemComposition(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, system, instructions, want string }{
		{name: "both empty"},
		{name: "system only", system: "base", want: "base"},
		{name: "instructions only", instructions: "mode", want: "mode"},
		{name: "both", system: "base", instructions: "mode", want: "base\n\nmode"},
	}
	for _, tt := range tests {
		tc := tt
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := EffectiveSystem(tc.system, tc.instructions); got != tc.want {
				t.Errorf("EffectiveSystem(%q, %q) = %q, want %q", tc.system, tc.instructions, got, tc.want)
			}
		})
	}
}
