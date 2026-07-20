package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/identity"
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
		{name: "invalid engine", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithEngine(Engine(99))}, kind: DefinitionInvalidEngine},
		{name: "nil runtime context", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithRuntimeContext(nil)}, kind: DefinitionInvalidRuntimeContext},
		{name: "typed nil runtime context", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithRuntimeContext((*nilRuntimeContext)(nil))}, kind: DefinitionInvalidRuntimeContext},
		{name: "empty delegate", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithDelegates("")}, kind: DefinitionInvalidDelegate},
		{name: "invalid delegation", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithDelegation(Delegation{Style: DelegationStyle(99)})}, kind: DefinitionInvalidDelegation},
		{name: "empty policy revision", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithPolicyRevision("")}, kind: DefinitionInvalidPolicyRevision},
		{name: "opaque access gate lacks revision (validation table)", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithAccessGate(&fakeAccessGate{})}, kind: DefinitionMissingPolicyRevision},
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
			d := mustDefinition(t, WithPolicyRevision("test"), WithAccessGate(&fakeAccessGate{}))
			_, err := d.Bind(context.Background(), tt.bindings)
			var bindErr *BindError
			if !errors.As(err, &bindErr) || bindErr.Kind != tt.kind {
				t.Fatalf("Bind error = %T %v, want kind %q", err, err, tt.kind)
			}
			var bindingsErr *tool.InvalidBindingsError
			if !errors.As(err, &bindingsErr) {
				t.Fatalf("Bind error cause = %T %v, want *tool.InvalidBindingsError", err, err)
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

func TestOutputSchemaOptionIsImmutableAcrossDefinitionLifecycle(t *testing.T) {
	t.Parallel()
	input := testOutputSchema()
	want := input.Clone()
	option := WithOutputSchema(input)
	input.Name = "mutated_before_define"
	input.Description = "mutated before define"
	input.Schema[0] = '['
	input.Strict = false

	definition := mustDefinition(t,
		option,
		WithModes(Mode{Name: "plan"}, Mode{Name: "build", Instructions: "build"}),
		WithInitialMode("plan"),
	)
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	assertOutputSchemaEqual(t, bound, want)

	first, _ := bound.OutputSchema()
	first.Name = "mutated_accessor"
	first.Description = "mutated accessor"
	first.Schema[0] = '['
	first.Strict = false
	assertOutputSchemaEqual(t, bound, want)

	selected, err := SelectBoundMode(bound, "build")
	if err != nil {
		t.Fatalf("SelectBoundMode() error = %v", err)
	}
	assertOutputSchemaEqual(t, selected, want)

	reused := mustDefinition(t, option)
	reusedBound, err := reused.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind(reused option) error = %v", err)
	}
	assertOutputSchemaEqual(t, reusedBound, want)
}

func TestOutputSchemaDefinitionValidation(t *testing.T) {
	t.Parallel()
	valid := testOutputSchema()
	tests := []struct {
		name       string
		opts       []Option
		kind       DefinitionErrorKind
		wantSchema bool
	}{
		{
			name: "duplicate option",
			opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()),
				WithOutputSchema(valid), WithOutputSchema(valid)},
			kind: DefinitionDuplicateOption,
		},
		{
			name: "invalid portable schema",
			opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithOutputSchema(inference.OutputSchema{
				Name: "result", Schema: json.RawMessage(`{"type":"array"}`),
			})},
			kind:       DefinitionInvalidOutputSchema,
			wantSchema: true,
		},
		{
			name: "reserved schema name",
			opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithOutputSchema(inference.OutputSchema{
				Name: inference.StructuredOutputToolName, Schema: valid.Schema,
			})},
			kind:       DefinitionInvalidOutputSchema,
			wantSchema: true,
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
			if tt.wantSchema {
				var schemaErr *inference.SchemaValidationError
				if definitionErr.Field != "output_schema" || !errors.As(err, &schemaErr) {
					t.Fatalf("Define() error = %#v, want output_schema wrapping *SchemaValidationError", definitionErr)
				}
			}
		})
	}
}

func TestOutputSchemaValidationErrorDoesNotExposeRawSchema(t *testing.T) {
	t.Parallel()
	const secret = "loop-output-schema-secret"
	output := inference.OutputSchema{
		Name:   "result",
		Schema: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false,"` + secret + `":true}`),
	}
	_, err := Define(WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithOutputSchema(output))
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionInvalidOutputSchema {
		t.Fatalf("Define() error = %T %v, want invalid output schema", err, err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(fmt.Sprint(definitionErr.Cause), secret) {
		t.Fatalf("validation error exposed raw schema: %v / %v", err, definitionErr.Cause)
	}
}

func TestDefinitionRejectsReservedProducedToolName(t *testing.T) {
	t.Parallel()
	reserved := func(definitionName string, names ...string) tool.Definition {
		return tool.NewBundleDefinition(definitionName, names, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return nil, nil
		})
	}
	tests := []struct {
		name string
		opts []Option
	}{
		{name: "base first", opts: []Option{WithTools(reserved("base", inference.StructuredOutputToolName, "Read"))}},
		{name: "base after ordinary definition", opts: []Option{WithTools(
			reserved("ordinary", "Read"), reserved("reserved", "Write", inference.StructuredOutputToolName),
		)}},
		{name: "noninitial mode", opts: []Option{
			WithModes(Mode{Name: "plan"}, Mode{Name: "build", Tools: []tool.Definition{reserved("mode", inference.StructuredOutputToolName)}}),
			WithInitialMode("plan"),
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Define(append([]Option{WithName("agent"), WithInference(&fakeLLM{}, testModel())}, tt.opts...)...)
			assertReservedToolDefinitionError(t, err)
		})
	}
}

func TestIsReservedToolNameMatchesOnlyControlName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{name: inference.StructuredOutputToolName, want: true},
		{name: " " + inference.StructuredOutputToolName},
		{name: inference.StructuredOutputToolName + " "},
		{name: "ordinary"},
		{name: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsReservedToolName(tt.name); got != tt.want {
				t.Fatalf("IsReservedToolName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestBindRejectsReservedInjectedProducedToolName(t *testing.T) {
	t.Parallel()
	definition := mustDefinition(t, WithDelegates("worker"))
	bindings := validToolBindings(t)
	builds := 0
	bindings.ExtraTools = []tool.Definition{tool.NewBundleDefinition(
		"injected", []string{"Subagent", inference.StructuredOutputToolName}, 0,
		func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			builds++
			return nil, nil
		},
	)}
	_, err := definition.Bind(context.Background(), bindings)
	var bindErr *BindError
	if !errors.As(err, &bindErr) || bindErr.Kind != BindInvalidDefinition {
		t.Fatalf("Bind() error = %T %v, want BindInvalidDefinition", err, err)
	}
	assertReservedToolDefinitionError(t, err)
	if builds != 0 {
		t.Fatalf("injected factory ran %d times, want validation before binding mutation", builds)
	}
}

func TestOutputSchemaDoesNotAddBoundToolsOrRequirements(t *testing.T) {
	t.Parallel()
	definition := mustDefinition(t, WithOutputSchema(testOutputSchema()))
	if got := definition.ToolRequirements(); got != 0 {
		t.Fatalf("ToolRequirements() = %v, want no terminal-tool permission requirements", got)
	}
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	base, ok := bound.Mode("")
	if !ok {
		t.Fatal("base mode missing")
	}
	if len(base.Tools) != 0 {
		t.Fatalf("bound executable tools = %#v, want none for output policy", base.Tools)
	}
}

func TestOutputSchemaPolicyIdentity(t *testing.T) {
	t.Parallel()
	baseOutput := testOutputSchema()
	define := func(output inference.OutputSchema) Definition {
		return mustDefinition(t, WithOutputSchema(output))
	}
	base := define(baseOutput)
	sameWhitespace := baseOutput.Clone()
	sameWhitespace.Schema = json.RawMessage(`{
		"type":"object", "properties":{"answer":{"type":"string"}},
		"required":["answer"], "additionalProperties":false
	}`)
	if base.PolicyRevision() != define(sameWhitespace).PolicyRevision() {
		t.Fatal("insignificant schema whitespace changed PolicyRevision")
	}
	if base.state.outputPolicy == nil || base.state.outputPolicy.Name != baseOutput.Name ||
		base.state.outputPolicy.Revision != inference.StructuredOutputRevision ||
		base.state.outputPolicy.SHA256 == ([32]byte{}) {
		t.Fatalf("output identity = %#v, want bounded name/digest/current revision", base.state.outputPolicy)
	}
	revisedState := *base.state
	revisedIdentity := *base.state.outputPolicy
	revisedIdentity.Revision = "structured-output/v2"
	revisedState.outputPolicy = &revisedIdentity
	if revised := (Definition{state: &revisedState}).PolicyRevision(); revised == base.PolicyRevision() {
		t.Fatalf("PolicyRevision() ignored structured-output revision drift: %q", revised)
	}

	changedName := baseOutput.Clone()
	changedName.Name = "other_result"
	changedDescription := baseOutput.Clone()
	changedDescription.Description = "other guidance"
	changedSchema := baseOutput.Clone()
	changedSchema.Schema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"boolean"}},"required":["answer"],"additionalProperties":false}`)
	changedStrict := baseOutput.Clone()
	changedStrict.Strict = false
	for _, tt := range []struct {
		name   string
		output inference.OutputSchema
	}{
		{name: "name", output: changedName},
		{name: "description", output: changedDescription},
		{name: "schema", output: changedSchema},
		{name: "strict", output: changedStrict},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if got := define(tt.output).PolicyRevision(); got == base.PolicyRevision() {
				t.Fatalf("PolicyRevision() unchanged: %q", got)
			}
		})
	}

	withoutOutput := mustDefinition(t)
	withoutOutputAgain := mustDefinition(t)
	if withoutOutput.PolicyRevision() != withoutOutputAgain.PolicyRevision() || withoutOutput.state.outputPolicy != nil {
		t.Fatal("absent output changed legacy policy behavior")
	}
}

func testOutputSchema() inference.OutputSchema {
	return inference.OutputSchema{
		Name:        "loop_result",
		Description: "final result guidance",
		Schema:      json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`),
		Strict:      true,
	}
}

func assertOutputSchemaEqual(t *testing.T, bound BoundDefinition, want inference.OutputSchema) {
	t.Helper()
	got, ok := bound.OutputSchema()
	if !ok || got == nil {
		t.Fatal("OutputSchema() = absent, want configured output")
	}
	if got.Name != want.Name || got.Description != want.Description || got.Strict != want.Strict || !bytes.Equal(got.Schema, want.Schema) {
		t.Fatalf("OutputSchema() = %#v, want %#v", got, want)
	}
}

func assertReservedToolDefinitionError(t *testing.T, err error) {
	t.Helper()
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionReservedToolName {
		t.Fatalf("error = %T %v, want DefinitionReservedToolName", err, err)
	}
	if definitionErr.Value != "" {
		t.Fatalf("reserved-name error retained a value: %#v", definitionErr)
	}
}

func validToolBindings(t *testing.T) tool.Bindings {
	t.Helper()
	return tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t)}
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
