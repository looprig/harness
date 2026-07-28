package hustle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

type evidenceToolStub struct {
	infos []*tool.ToolInfo
	calls int
	err   error
}

func (t *evidenceToolStub) Info(context.Context) (*tool.ToolInfo, error) {
	if t.err != nil {
		return nil, t.err
	}
	if len(t.infos) == 0 {
		return nil, nil
	}
	index := t.calls
	if index >= len(t.infos) {
		index = len(t.infos) - 1
	}
	t.calls++
	info := t.infos[index]
	if info == nil {
		return nil, nil
	}
	clone := *info
	clone.Schema = append(json.RawMessage(nil), info.Schema...)
	return &clone, nil
}

func (*evidenceToolStub) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}

type evidenceCoordinator struct{}

func (evidenceCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return evidencePermit{}, nil
}
func (evidenceCoordinator) Healthy() error { return nil }

type evidencePermit struct{}

func (evidencePermit) Release() {}

func evidenceBindingIDs(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	loopID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	return sessionID, loopID
}

func bindableEvidenceDefinition(
	t *testing.T,
	definitions ...tool.Definition,
) Definition {
	t.Helper()
	static := make([]tool.Definition, len(definitions))
	for i, definition := range definitions {
		if len(definition.ToolInfos()) > 0 {
			static[i] = definition
			continue
		}
		names := definition.ProducedToolNames()
		infos := make([]tool.ToolInfo, len(names))
		for j, name := range names {
			infos[j] = tool.ToolInfo{
				Name: name, Desc: "read " + name,
				Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			}
		}
		static[i] = tool.NewEvidenceDefinition(
			definition.Name(), 0, infos,
			func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
				return definition.Build(ctx, bindings)
			},
		)
	}
	policy := validEvidenceToolPolicy()
	policy.Definitions = static
	options := validEvidenceOptionsWithoutPolicy()
	definition, err := Define(append(options, WithEvidenceTools(policy))...)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	return definition
}

func TestBindEvidenceToolsAttenuatesBindingsAndFreezesIdentity(t *testing.T) {
	t.Parallel()
	sessionID, loopID := evidenceBindingIDs(t)
	workspace := &tool.WorkspaceBinding{
		Root: "/workspace", Coordinator: evidenceCoordinator{},
	}
	concrete := &evidenceToolStub{infos: []*tool.ToolInfo{{
		Name: "workspace-status", Desc: "read workspace status",
		Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}}}
	builds := 0
	staticInfo := tool.ToolInfo{
		Name: "workspace-status", Desc: "read workspace status",
		Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}
	definition := tool.NewEvidenceDefinition(
		"workspace-status",
		tool.RequiresWorkspaceRead,
		[]tool.ToolInfo{staticInfo},
		func(_ context.Context, got tool.Bindings) ([]tool.InvokableTool, error) {
			builds++
			if got.SessionID != sessionID || got.LoopID != loopID {
				t.Fatalf("tool binding IDs = %v/%v, want %v/%v", got.SessionID, got.LoopID, sessionID, loopID)
			}
			if got.ReadWorkspace == nil || got.ReadWorkspace.Root != workspace.Root {
				t.Fatalf("read workspace binding = %#v, want root %q", got.ReadWorkspace, workspace.Root)
			}
			if got.Workspace != nil || got.Delegate != nil || got.ExtraTools != nil {
				t.Fatalf("evidence tool received broader bindings: %#v", got)
			}
			return []tool.InvokableTool{concrete}, nil
		},
	)
	hustleDefinition := bindableEvidenceDefinition(t, definition)
	bound, err := hustleDefinition.Bind(context.Background(), Bindings{
		SessionID: sessionID, LoopID: loopID, Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if builds != 1 {
		t.Fatalf("build calls = %d, want 1", builds)
	}
	evidence := bound.EvidenceTools()
	if len(evidence) != 1 || evidence[0].Name() != "workspace-status" {
		t.Fatalf("EvidenceTools() = %#v", evidence)
	}
	if evidence[0].Tool() != concrete {
		t.Fatal("bound evidence tool did not preserve the concrete capability")
	}
	if evidence[0].SchemaSHA256() == ([sha256.Size]byte{}) ||
		evidence[0].DescriptionSHA256() == ([sha256.Size]byte{}) ||
		evidence[0].IdentitySHA256() == ([sha256.Size]byte{}) {
		t.Fatalf("incomplete evidence identity: %#v", evidence[0])
	}
	evidence[0] = BoundEvidenceTool{}
	if again := bound.EvidenceTools(); len(again) != 1 || again[0].Name() != "workspace-status" {
		t.Fatal("EvidenceTools accessor exposed its slice")
	}
	info := bound.EvidenceTools()[0].Info()
	if info == nil {
		t.Fatalf("frozen tool Info() = %#v", info)
	}
	info.Desc = "mutated"
	info.Schema[0] = '['
	again := bound.EvidenceTools()[0].Info()
	if again.Desc != "read workspace status" ||
		!bytes.Equal(again.Schema, json.RawMessage(`{"type":"object","additionalProperties":false}`)) {
		t.Fatalf("frozen Info() changed: %#v", again)
	}
}

func TestBindEvidenceToolIdentityCoversDescriptionAndCanonicalSchema(t *testing.T) {
	t.Parallel()
	sessionID, loopID := evidenceBindingIDs(t)
	bind := func(desc string, schema json.RawMessage) BoundEvidenceTool {
		t.Helper()
		concrete := &evidenceToolStub{infos: []*tool.ToolInfo{{
			Name: "status", Desc: desc, Schema: schema,
		}}}
		definition := bindableEvidenceDefinition(t, tool.NewEvidenceDefinition(
			"status", 0, []tool.ToolInfo{{Name: "status", Desc: desc, Schema: schema}},
			func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{concrete}, nil
			},
		))
		bound, err := definition.Bind(context.Background(), Bindings{SessionID: sessionID, LoopID: loopID})
		if err != nil {
			t.Fatalf("Bind() error = %v", err)
		}
		return bound.EvidenceTools()[0]
	}
	base := bind("read status", json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`))
	whitespace := bind("read status", json.RawMessage("{\n \"type\":\"object\", \"properties\":{\"path\":{\"type\":\"string\"}}, \"required\":[\"path\"], \"additionalProperties\":false\n}"))
	if base.IdentitySHA256() != whitespace.IdentitySHA256() {
		t.Fatal("schema whitespace changed canonical identity")
	}
	if base.IdentitySHA256() == bind("different", json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)).IdentitySHA256() {
		t.Fatal("description did not contribute to identity")
	}
	if base.IdentitySHA256() == bind("read status", json.RawMessage(`{"type":"object","properties":{"path":{"type":"number"}},"required":["path"],"additionalProperties":false}`)).IdentitySHA256() {
		t.Fatal("schema did not contribute to identity")
	}
}

func TestBindEvidenceToolsRejectsInvalidConcreteTools(t *testing.T) {
	t.Parallel()
	sessionID, loopID := evidenceBindingIDs(t)
	var typedNil *evidenceToolStub
	validSchema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	valid := &tool.ToolInfo{Name: "status", Desc: "read status", Schema: validSchema}
	tests := []struct {
		name  string
		tools []tool.InvokableTool
	}{
		{name: "nil tool", tools: []tool.InvokableTool{nil}},
		{name: "typed nil tool", tools: []tool.InvokableTool{typedNil}},
		{name: "nil info", tools: []tool.InvokableTool{&evidenceToolStub{}}},
		{name: "info error", tools: []tool.InvokableTool{&evidenceToolStub{err: errors.New("info failed")}}},
		{name: "noncanonical name", tools: []tool.InvokableTool{&evidenceToolStub{infos: []*tool.ToolInfo{{Name: " status", Desc: valid.Desc, Schema: valid.Schema}}}}},
		{name: "invalid schema", tools: []tool.InvokableTool{&evidenceToolStub{infos: []*tool.ToolInfo{{Name: valid.Name, Desc: valid.Desc, Schema: json.RawMessage(`{`)}}}}},
		{name: "nonobject schema", tools: []tool.InvokableTool{&evidenceToolStub{infos: []*tool.ToolInfo{{Name: valid.Name, Desc: valid.Desc, Schema: json.RawMessage(`[]`)}}}}},
		{name: "blank description", tools: []tool.InvokableTool{&evidenceToolStub{infos: []*tool.ToolInfo{{Name: valid.Name, Desc: " ", Schema: valid.Schema}}}}},
		{name: "oversized description", tools: []tool.InvokableTool{&evidenceToolStub{infos: []*tool.ToolInfo{{Name: valid.Name, Desc: strings.Repeat("x", MaxEvidenceToolDescriptionBytes+1), Schema: valid.Schema}}}}},
		{name: "oversized schema", tools: []tool.InvokableTool{&evidenceToolStub{infos: []*tool.ToolInfo{{Name: valid.Name, Desc: valid.Desc, Schema: append(append(json.RawMessage(nil), valid.Schema...), bytes.Repeat([]byte(" "), MaxEvidenceToolSchemaBytes-len(valid.Schema)+1)...)}}}}},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			definition := bindableEvidenceDefinition(t, tool.NewDefinition(
				"status", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
					return testCase.tools, nil
				},
			))
			_, err := definition.Bind(context.Background(), Bindings{SessionID: sessionID, LoopID: loopID})
			var bindErr *BindError
			if !errors.As(err, &bindErr) || bindErr.Kind != BindInvalidEvidenceTools {
				t.Fatalf("Bind() error = %T %v, want invalid evidence tools", err, err)
			}
		})
	}
}

func TestBindEvidenceToolMetadataFieldBoundaries(t *testing.T) {
	t.Parallel()
	validSchema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	exactSchema := append(append(json.RawMessage(nil), validSchema...),
		bytes.Repeat([]byte(" "), MaxEvidenceToolSchemaBytes-len(validSchema))...)
	tests := []struct {
		name    string
		desc    string
		schema  json.RawMessage
		wantErr bool
	}{
		{name: "exact description", desc: strings.Repeat("d", MaxEvidenceToolDescriptionBytes), schema: validSchema},
		{name: "description one over", desc: strings.Repeat("d", MaxEvidenceToolDescriptionBytes+1), schema: validSchema, wantErr: true},
		{name: "exact raw schema", desc: "d", schema: exactSchema},
		{name: "raw schema one over", desc: "d", schema: append(append(json.RawMessage(nil), exactSchema...), ' '), wantErr: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := bindSingleEvidenceTool(t, &tool.ToolInfo{
				Name: "status", Desc: testCase.desc, Schema: testCase.schema,
			})
			if (err != nil) != testCase.wantErr {
				t.Fatalf("Bind() error = %v, wantErr %v", err, testCase.wantErr)
			}
		})
	}
}

func TestBindEvidenceToolsRejectsNonPortableSchemas(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		schema json.RawMessage
	}{
		{name: "duplicate keyword", schema: json.RawMessage(`{"type":"object","type":"object","properties":{},"additionalProperties":false}`)},
		{name: "duplicate property", schema: json.RawMessage(`{"type":"object","properties":{"p":{"type":"string"},"p":{"type":"string"}},"required":["p"],"additionalProperties":false}`)},
		{name: "unknown keyword", schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false,"secret":true}`)},
		{name: "unsupported type", schema: json.RawMessage(`{"type":"object","properties":{"p":{"type":"null"}},"required":["p"],"additionalProperties":false}`)},
		{name: "root scalar", schema: json.RawMessage(`{"type":"string"}`)},
		{name: "excessive depth", schema: nestedEvidenceArraySchema(64)},
		{name: "excessive properties", schema: evidenceObjectSchemaWithProperties(1025)},
		{name: "invalid UTF-8", schema: json.RawMessage{'{', '"', 't', 'y', 'p', 'e', '"', ':', '"', 0xff, '"', '}'}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := bindSingleEvidenceTool(t, &tool.ToolInfo{
				Name: "status", Desc: "read status", Schema: testCase.schema,
			})
			var definitionErr *DefinitionError
			var bindErr *BindError
			staticRejected := errors.As(err, &definitionErr) && definitionErr.Kind == DefinitionInvalidEvidenceTools
			bindRejected := errors.As(err, &bindErr) && bindErr.Kind == BindInvalidEvidenceTools
			if !staticRejected && !bindRejected {
				t.Fatalf("evidence schema error = %T %v, want bounded invalid evidence tools", err, err)
			}
			if len(err.Error()) > 128 || strings.Contains(err.Error(), string(testCase.schema)) {
				t.Fatalf("Bind() error exposed schema or was unbounded: %q", err)
			}
		})
	}
}

func TestBindEvidenceToolCatalogAggregateMetadataBoundary(t *testing.T) {
	t.Parallel()
	for _, delta := range []int{0, 1} {
		delta := delta
		name := "exact aggregate"
		if delta != 0 {
			name = "aggregate one over"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := bindEvidenceCatalogOfSize(t, MaxEvidenceToolMetadataBytes+delta)
			if (err != nil) != (delta != 0) {
				t.Fatalf("Bind() error = %v, wantErr %v", err, delta != 0)
			}
		})
	}
}

func TestBindEvidenceToolsRejectsBuildError(t *testing.T) {
	t.Parallel()
	sessionID, loopID := evidenceBindingIDs(t)
	definition := bindableEvidenceDefinition(t, tool.NewDefinition(
		"status", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return nil, errors.New("build failed")
		},
	))
	_, err := definition.Bind(context.Background(), Bindings{SessionID: sessionID, LoopID: loopID})
	var bindErr *BindError
	if !errors.As(err, &bindErr) || bindErr.Kind != BindInvalidEvidenceTools {
		t.Fatalf("Bind() error = %T %v, want invalid evidence tools", err, err)
	}
}

func TestBindEvidenceToolsRejectsInfoDriftAndPreservesToollessBind(t *testing.T) {
	t.Parallel()
	sessionID, loopID := evidenceBindingIDs(t)
	// tool.Definition.Build performs the first Info read. The binder must then
	// compare its own two independently validated reads.
	drifting := &evidenceToolStub{infos: []*tool.ToolInfo{
		{Name: "status", Desc: "first", Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{Name: "status", Desc: "first", Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{Name: "status", Desc: "second", Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	}}
	definition := bindableEvidenceDefinition(t, tool.NewEvidenceDefinition(
		"status", 0, []tool.ToolInfo{{
			Name: "status", Desc: "first",
			Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		}}, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{drifting}, nil
		},
	))
	if _, err := definition.Bind(context.Background(), Bindings{SessionID: sessionID, LoopID: loopID}); err == nil {
		t.Fatal("Bind() accepted ToolInfo drift")
	}

	toolLess, err := Define(validNamedOptions(&testClient{}, validModel("tool-less"))...)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := toolLess.Bind(context.Background(), Bindings{})
	if err != nil {
		t.Fatalf("tool-less Bind() error = %v", err)
	}
	if got := bound.EvidenceTools(); got != nil {
		t.Fatalf("tool-less EvidenceTools() = %#v, want nil", got)
	}
}

func TestBindEvidenceToolsRejectsConcreteMetadataDriftFromStaticCatalog(t *testing.T) {
	t.Parallel()
	sessionID, loopID := evidenceBindingIDs(t)
	validSchema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	tests := []struct {
		name     string
		static   tool.ToolInfo
		concrete tool.ToolInfo
	}{
		{
			name:     "description",
			static:   tool.ToolInfo{Name: "status", Desc: "static description", Schema: validSchema},
			concrete: tool.ToolInfo{Name: "status", Desc: "concrete description", Schema: validSchema},
		},
		{
			name:   "schema",
			static: tool.ToolInfo{Name: "status", Desc: "read status", Schema: validSchema},
			concrete: tool.ToolInfo{
				Name: "status", Desc: "read status",
				Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
			},
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			concrete := &evidenceToolStub{infos: []*tool.ToolInfo{&testCase.concrete}}
			definition := bindableEvidenceDefinition(t, tool.NewEvidenceDefinition(
				"status", 0, []tool.ToolInfo{testCase.static},
				func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
					return []tool.InvokableTool{concrete}, nil
				},
			))
			_, err := definition.Bind(context.Background(), Bindings{SessionID: sessionID, LoopID: loopID})
			var bindErr *BindError
			if !errors.As(err, &bindErr) || bindErr.Kind != BindInvalidEvidenceTools {
				t.Fatalf("Bind() error = %T %v, want static metadata mismatch", err, err)
			}
		})
	}
}

func bindSingleEvidenceTool(t *testing.T, info *tool.ToolInfo) (BoundDefinition, error) {
	t.Helper()
	sessionID, loopID := evidenceBindingIDs(t)
	concrete := &evidenceToolStub{infos: []*tool.ToolInfo{info}}
	policy := validEvidenceToolPolicy()
	policy.Definitions = []tool.Definition{tool.NewEvidenceDefinition(
		info.Name, 0, []tool.ToolInfo{info.Clone()}, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{concrete}, nil
		},
	)}
	definition, err := Define(append(validEvidenceOptionsWithoutPolicy(), WithEvidenceTools(policy))...)
	if err != nil {
		return nil, err
	}
	return definition.Bind(context.Background(), Bindings{SessionID: sessionID, LoopID: loopID})
}

func nestedEvidenceArraySchema(arrayCount int) json.RawMessage {
	node := `{"type":"string"}`
	for range arrayCount {
		node = `{"type":"array","items":` + node + `}`
	}
	return json.RawMessage(`{"type":"object","properties":{"p":` + node + `},"required":["p"],"additionalProperties":false}`)
}

func evidenceObjectSchemaWithProperties(count int) json.RawMessage {
	var builder strings.Builder
	builder.WriteString(`{"type":"object","properties":{`)
	for index := range count {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `"p%d":{"type":"string"}`, index)
	}
	builder.WriteString(`},"required":[`)
	for index := range count {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `"p%d"`, index)
	}
	builder.WriteString(`],"additionalProperties":false}`)
	return json.RawMessage(builder.String())
}

func evidenceSchemaOfCompactSize(t *testing.T, size int) json.RawMessage {
	t.Helper()
	const prefix = `{"type":"object","properties":{"p":{"type":"string","enum":["`
	const suffix = `"]}},"required":["p"],"additionalProperties":false}`
	if size < len(prefix)+len(suffix) {
		t.Fatalf("requested schema size %d is below minimum %d", size, len(prefix)+len(suffix))
	}
	return json.RawMessage(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}

func bindEvidenceCatalogOfSize(t *testing.T, aggregateSize int) (BoundDefinition, error) {
	t.Helper()
	const toolCount = MaxEvidenceProducedToolNames
	names := make([]string, toolCount)
	infos := make([]*tool.ToolInfo, toolCount)
	staticInfos := make([]tool.ToolInfo, toolCount)
	baseMetadata := 0
	for index := range toolCount {
		names[index] = fmt.Sprintf("tool-%03d", index)
		baseMetadata += len(names[index]) + 1
	}
	schemaTotal := aggregateSize - baseMetadata
	if schemaTotal <= 0 {
		t.Fatalf("aggregate size %d is too small", aggregateSize)
	}
	for index := range toolCount {
		schemaSize := schemaTotal / (toolCount - index)
		schemaTotal -= schemaSize
		infos[index] = &tool.ToolInfo{
			Name: names[index], Desc: "d", Schema: evidenceSchemaOfCompactSize(t, schemaSize),
		}
		staticInfos[index] = infos[index].Clone()
	}
	tools := make([]tool.InvokableTool, toolCount)
	for index := range toolCount {
		tools[index] = &evidenceToolStub{infos: []*tool.ToolInfo{infos[index]}}
	}
	policy := validEvidenceToolPolicy()
	policy.Definitions = []tool.Definition{tool.NewEvidenceDefinition(
		"aggregate", 0, staticInfos, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return tools, nil
		},
	)}
	options := validEvidenceOptionsWithoutPolicy()
	definition, err := Define(append(options, WithEvidenceTools(policy))...)
	if err != nil {
		return nil, err
	}
	sessionID, loopID := evidenceBindingIDs(t)
	return definition.Bind(context.Background(), Bindings{SessionID: sessionID, LoopID: loopID})
}
