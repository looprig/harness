package tool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

func TestDefinitionInterfaceIsSealed(t *testing.T) {
	t.Parallel()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the test source path")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(filename), "definition.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse definition.go: %v", err)
	}

	var definition *ast.InterfaceType
	for _, declaration := range parsed.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range general.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "Definition" {
				continue
			}
			definition, _ = typeSpec.Type.(*ast.InterfaceType)
		}
	}
	if definition == nil {
		t.Fatal("Definition interface declaration not found")
	}
	for _, method := range definition.Methods.List {
		if len(method.Names) == 1 && !method.Names[0].IsExported() {
			return
		}
	}
	t.Fatal("Definition interface has no unexported sealing method; external packages can implement it")
}

func TestDelegateControllerUsesAgentLanguageWithoutWaitCollection(t *testing.T) {
	t.Parallel()

	wantRequestFields := []string{
		"Operation", "AgentID", "AgentType", "Name", "AgentMode", "Message",
		"WaitForResponse", "TimeoutSeconds", "ParentToolUseID", "Runtime",
	}
	requestType := reflect.TypeOf(tool.DelegateRequest{})
	if got := exportedFieldNames(requestType); !reflect.DeepEqual(got, wantRequestFields) {
		t.Fatalf("DelegateRequest fields = %v, want %v", got, wantRequestFields)
	}

	wantResultFields := []string{
		"AgentID", "Name", "State", "Response", "ResponseStatus",
		"CorrelationID", "PreviousState", "Agents", "Truncated",
	}
	resultType := reflect.TypeOf(tool.DelegateResult{})
	if got := exportedFieldNames(resultType); !reflect.DeepEqual(got, wantResultFields) {
		t.Fatalf("DelegateResult fields = %v, want %v", got, wantResultFields)
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the test source path")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(filename), "definition.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse definition.go: %v", err)
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && ident.Name == "DelegateWait" {
			t.Errorf("definition.go still declares model-facing DelegateWait")
		}
		return true
	})
}

func exportedFieldNames(typ reflect.Type) []string {
	fields := make([]string, 0, typ.NumField())
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if field.IsExported() {
			fields = append(fields, field.Name)
		}
	}
	return fields
}

func TestReadWorkspaceBindingIsStructurallyReadOnly(t *testing.T) {
	t.Parallel()

	bindingType := reflect.TypeOf(tool.ReadWorkspaceBinding{})
	if bindingType.NumField() != 1 {
		t.Fatalf("ReadWorkspaceBinding fields = %d, want exactly Root", bindingType.NumField())
	}
	field := bindingType.Field(0)
	if field.Name != "Root" || field.Type.Kind() != reflect.String || !field.IsExported() {
		t.Fatalf("ReadWorkspaceBinding field = %#v, want exported Root string", field)
	}
}

func TestEvidenceFactoryBindingsExposeOnlyInvocationOriginAndReadWorkspace(t *testing.T) {
	t.Parallel()

	bindingType := reflect.TypeOf(tool.EvidenceFactoryBindings{})
	want := []struct {
		name string
		typ  reflect.Type
	}{
		{name: "SessionID", typ: reflect.TypeOf(uuid.UUID{})},
		{name: "LoopID", typ: reflect.TypeOf(uuid.UUID{})},
		{name: "ReadWorkspace", typ: reflect.TypeOf((*tool.ReadWorkspaceBinding)(nil))},
	}
	if bindingType.NumField() != len(want) {
		t.Fatalf("EvidenceFactoryBindings fields = %d, want exactly %d", bindingType.NumField(), len(want))
	}
	for index, expected := range want {
		field := bindingType.Field(index)
		if field.Name != expected.name || field.Type != expected.typ || !field.IsExported() {
			t.Fatalf("EvidenceFactoryBindings field %d = %#v, want exported %s %v", index, field, expected.name, expected.typ)
		}
	}
}

func TestEvidenceDefinitionReceivesOnlyEvidenceFactoryBindings(t *testing.T) {
	t.Parallel()

	bindings := validBindings()
	bindings.ReadWorkspace = &tool.ReadWorkspaceBinding{Root: "/canonical/workspace"}
	var got tool.EvidenceFactoryBindings
	definition := tool.NewEvidenceDefinition(
		"workspace-status",
		tool.RequiresWorkspaceRead,
		[]tool.ToolInfo{{Name: "workspace-status", Desc: "read status", Schema: json.RawMessage(`{"type":"object"}`)}},
		func(_ context.Context, bound tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
			got = bound
			return []tool.InvokableTool{&reportedNameTool{info: &tool.ToolInfo{Name: "workspace-status"}}}, nil
		},
	)

	if _, err := definition.Build(context.Background(), bindings); err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got.SessionID != bindings.SessionID || got.LoopID != bindings.LoopID {
		t.Fatalf("factory coordinates = (%s, %s), want (%s, %s)", got.SessionID, got.LoopID, bindings.SessionID, bindings.LoopID)
	}
	if got.ReadWorkspace == nil || got.ReadWorkspace.Root != "/canonical/workspace" {
		t.Fatalf("factory ReadWorkspace = %#v, want canonical root", got.ReadWorkspace)
	}
	if got.ReadWorkspace == bindings.ReadWorkspace {
		t.Fatal("factory received caller's ReadWorkspaceBinding pointer")
	}
	got.ReadWorkspace.Root = "/mutated"
	if bindings.ReadWorkspace.Root != "/canonical/workspace" {
		t.Fatalf("factory mutation changed caller root to %q", bindings.ReadWorkspace.Root)
	}
}

func TestReadWorkspaceRequirementValidatesAndAttenuatesBindings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		requirement tool.Requirements
		change      func(*tool.Bindings)
		wantMissing tool.Requirements
		wantField   string
		wantRead    bool
	}{
		{name: "required binding", requirement: tool.RequiresWorkspaceRead, wantRead: true},
		{name: "missing binding", requirement: tool.RequiresWorkspaceRead, change: func(b *tool.Bindings) { b.ReadWorkspace = nil }, wantMissing: tool.RequiresWorkspaceRead},
		{name: "blank root", requirement: tool.RequiresWorkspaceRead, change: func(b *tool.Bindings) { b.ReadWorkspace.Root = " " }, wantField: "read_workspace.root"},
		{name: "relative root", requirement: tool.RequiresWorkspaceRead, change: func(b *tool.Bindings) { b.ReadWorkspace.Root = "workspace" }, wantField: "read_workspace.root"},
		{name: "unclean root", requirement: tool.RequiresWorkspaceRead, change: func(b *tool.Bindings) { b.ReadWorkspace.Root = "/workspace/../other" }, wantField: "read_workspace.root"},
		{name: "extra binding omitted", change: func(*tool.Bindings) {}, wantRead: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bindings := validBindings()
			bindings.ReadWorkspace = &tool.ReadWorkspaceBinding{Root: "/workspace"}
			if tt.change != nil {
				tt.change(&bindings)
			}
			var got tool.Bindings
			definition := tool.NewDefinition("custom", tt.requirement, func(_ context.Context, bound tool.Bindings) ([]tool.InvokableTool, error) {
				got = bound
				return []tool.InvokableTool{&definitionTool{}}, nil
			})
			_, err := definition.Build(context.Background(), bindings)
			if tt.wantMissing != 0 {
				var missing *tool.MissingBindingError
				if !errors.As(err, &missing) || missing.Requirement != tt.wantMissing {
					t.Fatalf("Build() error = %T %v, want missing %v", err, err, tt.wantMissing)
				}
				return
			}
			if tt.wantField != "" {
				var invalid *tool.InvalidBindingsError
				if !errors.As(err, &invalid) || invalid.Field != tt.wantField {
					t.Fatalf("Build() error = %T %v, want field %q", err, err, tt.wantField)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if (got.ReadWorkspace != nil) != tt.wantRead {
				t.Fatalf("factory ReadWorkspace present = %t, want %t", got.ReadWorkspace != nil, tt.wantRead)
			}
		})
	}
}

func TestEvidenceDefinitionFreezesToolInfos(t *testing.T) {
	t.Parallel()

	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	infos := []tool.ToolInfo{
		{Name: "status", Desc: "read status", Schema: schema},
		{Name: "diff", Desc: "read diff", Schema: json.RawMessage(`{"type":"object"}`)},
	}
	definition := tool.NewEvidenceDefinition("git-evidence", tool.RequiresWorkspaceRead, infos, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{
			&reportedNameTool{info: &tool.ToolInfo{Name: "diff"}},
			&reportedNameTool{info: &tool.ToolInfo{Name: "status"}},
		}, nil
	})
	infos[0].Name = "drift"
	infos[0].Desc = "drift"
	schema[0] = '['

	if got, want := definition.ProducedToolNames(), []string{"status", "diff"}; !equalStrings(got, want) {
		t.Fatalf("ProducedToolNames() = %q, want %q", got, want)
	}
	first := definition.ToolInfos()
	if len(first) != 2 || first[0].Name != "status" || first[0].Desc != "read status" ||
		!bytes.Equal(first[0].Schema, json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)) {
		t.Fatalf("ToolInfos() = %#v, want frozen metadata", first)
	}
	first[0].Name = "output-drift"
	first[0].Schema[0] = '['
	second := definition.ToolInfos()
	if second[0].Name != "status" || second[0].Schema[0] != '{' {
		t.Fatalf("ToolInfos() after caller mutation = %#v, want independent deep clone", second)
	}
}

func TestToolInfoCloneOwnsSchema(t *testing.T) {
	t.Parallel()

	original := tool.ToolInfo{Name: "status", Desc: "read status", Schema: json.RawMessage(`{"type":"object"}`)}
	clone := original.Clone()
	clone.Name = "changed"
	clone.Schema[0] = '['
	if original.Name != "status" || original.Schema[0] != '{' {
		t.Fatalf("Clone() shared state with original: %#v", original)
	}
}

func TestToolInfoClonePreservesNilAndEmptySchema(t *testing.T) {
	t.Parallel()

	if clone := (tool.ToolInfo{}).Clone(); clone.Schema != nil {
		t.Fatalf("nil schema cloned as %#v, want nil", clone.Schema)
	}
	clone := (tool.ToolInfo{Schema: json.RawMessage{}}).Clone()
	if clone.Schema == nil {
		t.Fatal("non-nil empty schema cloned as nil")
	}
}

func TestDefinitionStaticToolInfoValidation(t *testing.T) {
	t.Parallel()

	valid := tool.ToolInfo{Name: "status", Desc: "read status", Schema: json.RawMessage(`{"type":"object"}`)}
	tests := []struct {
		name  string
		infos []tool.ToolInfo
		field string
	}{
		{name: "empty catalog", field: "tool_infos"},
		{name: "blank name", infos: []tool.ToolInfo{{Name: " ", Desc: valid.Desc, Schema: valid.Schema}}, field: "tool_infos.name"},
		{name: "noncanonical name", infos: []tool.ToolInfo{{Name: " status ", Desc: valid.Desc, Schema: valid.Schema}}, field: "tool_infos.name"},
		{name: "duplicate name", infos: []tool.ToolInfo{valid, valid}, field: "tool_infos.name"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition := tool.NewEvidenceDefinition("evidence", 0, tt.infos, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{&reportedNameTool{info: &tool.ToolInfo{Name: "status"}}}, nil
			})
			_, err := definition.Build(context.Background(), validBindings())
			var invalid *tool.InvalidDefinitionError
			if !errors.As(err, &invalid) || invalid.Field != tt.field {
				t.Fatalf("Build() error = %T %v, want invalid field %q", err, err, tt.field)
			}
		})
	}
}

func TestEvidenceDefinitionRejectsMutationCapabilities(t *testing.T) {
	t.Parallel()

	tests := []tool.Requirements{
		tool.RequiresWorkspace,
		tool.RequiresDelegateController,
		tool.RequiresWorkspaceRead | tool.RequiresWorkspace,
	}
	for _, requirements := range tests {
		requirements := requirements
		t.Run(fmt.Sprintf("requirements_%d", requirements), func(t *testing.T) {
			t.Parallel()
			called := false
			definition := tool.NewEvidenceDefinition(
				"status",
				requirements,
				[]tool.ToolInfo{{Name: "status"}},
				func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
					called = true
					return []tool.InvokableTool{&reportedNameTool{info: &tool.ToolInfo{Name: "status"}}}, nil
				},
			)
			_, err := definition.Build(context.Background(), validBindings())
			var invalid *tool.InvalidDefinitionError
			if !errors.As(err, &invalid) || invalid.Field != "evidence_requirements" {
				t.Fatalf("Build() error = %T %v, want invalid evidence requirements", err, err)
			}
			if called {
				t.Fatal("factory called with mutation-capable evidence requirements")
			}
		})
	}
}

func TestNormalDefinitionsHaveNoStaticToolInfos(t *testing.T) {
	t.Parallel()

	definitions := []tool.Definition{
		tool.NewDefinition("custom", 0, nil),
		tool.NewBundleDefinition("bundle", []string{"a", "b"}, 0, nil),
	}
	for _, definition := range definitions {
		if infos := definition.ToolInfos(); infos != nil {
			t.Fatalf("%s ToolInfos() = %#v, want nil", definition.Name(), infos)
		}
	}
}

// Keep the fake non-zero-sized so independently built pointers have distinct
// identities; a blank field expresses that test requirement without dead state.
type definitionTool struct{ _ byte }

func (*definitionTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "custom"}, nil
}

func (*definitionTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}

type coordinatorStub struct{ healthErr error }

func (*coordinatorStub) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return permitStub{}, nil
}

func (c *coordinatorStub) Healthy() error { return c.healthErr }

type permitStub struct{}

func (permitStub) Release() {}

type delegateStub struct{}

func (*delegateStub) Execute(context.Context, tool.DelegateRequest) (tool.DelegateResult, error) {
	return tool.DelegateResult{}, nil
}

type sessionResourceStub struct{}

func (*sessionResourceStub) Activate(context.Context, tool.SessionResourceServices) error {
	return nil
}

func (*sessionResourceStub) Shutdown(context.Context) error { return nil }

type sessionResourceRegistryStub struct{}

func (*sessionResourceRegistryStub) GetOrCreate(
	_ context.Context,
	_ string,
	factory func(string) (tool.SessionResource, error),
) (tool.SessionResource, error) {
	return factory("/resource")
}

type asyncProcessRunnerStub struct{}

func (*asyncProcessRunnerStub) PrepareProcess(context.Context, tool.ProcessRequest) (tool.PreparedProcess, error) {
	return nil, errors.New("not implemented")
}

func TestDefinitionMetadataAndFreshBuilds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		requirements tool.Requirements
	}{
		{name: "stateless", requirements: 0},
		{name: "all runtime requirements", requirements: tool.RequiresWorkspace | tool.RequiresDelegateController},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			var factoryOutputs [][]tool.InvokableTool
			definition := tool.NewDefinition("custom", tt.requirements, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
				calls++
				output := []tool.InvokableTool{&definitionTool{}}
				factoryOutputs = append(factoryOutputs, output)
				return output, nil
			})

			if got := definition.Name(); got != "custom" {
				t.Fatalf("Name() = %q, want custom", got)
			}
			if got := definition.Requirements(); got != tt.requirements {
				t.Fatalf("Requirements() = %v, want %v", got, tt.requirements)
			}
			if got := definition.ProducedToolNames(); len(got) != 1 || got[0] != "custom" {
				t.Fatalf("ProducedToolNames() = %q, want [custom]", got)
			}

			bindings := validBindings()
			first, err := definition.Build(context.Background(), bindings)
			if err != nil {
				t.Fatalf("first Build() error = %v", err)
			}
			second, err := definition.Build(context.Background(), bindings)
			if err != nil {
				t.Fatalf("second Build() error = %v", err)
			}
			if calls != 2 {
				t.Fatalf("factory calls = %d, want 2", calls)
			}
			if first[0] == second[0] {
				t.Fatal("Build() reused a tool instance")
			}

			first[0] = nil
			if factoryOutputs[0][0] == nil || second[0] == nil {
				t.Fatal("Build() did not return a defensive slice")
			}
		})
	}
}

func TestBundleDefinitionProducedToolNamesAreImmutable(t *testing.T) {
	t.Parallel()

	names := []string{"ReadFile", "WriteFile", "EditFile"}
	definition := tool.NewBundleDefinition("Files", names, tool.RequiresWorkspace, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{&definitionTool{}}, nil
	})
	names[0] = "mutated-input"

	first := definition.ProducedToolNames()
	if got, want := first, []string{"ReadFile", "WriteFile", "EditFile"}; !equalStrings(got, want) {
		t.Fatalf("ProducedToolNames() = %q, want %q", got, want)
	}
	first[1] = "mutated-output"
	if got, want := definition.ProducedToolNames(), []string{"ReadFile", "WriteFile", "EditFile"}; !equalStrings(got, want) {
		t.Fatalf("ProducedToolNames() after caller mutation = %q, want %q", got, want)
	}
}

type reportedNameTool struct {
	info *tool.ToolInfo
	err  error
}

func (t *reportedNameTool) Info(context.Context) (*tool.ToolInfo, error) { return t.info, t.err }
func (*reportedNameTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}

func TestDefinitionBuildRejectsInvalidProducedToolMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		declared []string
		built    []*reportedNameTool
		kind     tool.ProducedToolNamesErrorKind
	}{
		{name: "no declarations", declared: nil, built: []*reportedNameTool{}, kind: tool.ProducedToolNameEmpty},
		{name: "empty declaration", declared: []string{" "}, built: []*reportedNameTool{{info: &tool.ToolInfo{Name: "Read"}}}, kind: tool.ProducedToolNameEmpty},
		{name: "duplicate declaration", declared: []string{"Read", " Read "}, built: []*reportedNameTool{{info: &tool.ToolInfo{Name: "Read"}}}, kind: tool.ProducedToolNameDuplicate},
		{name: "nil info", declared: []string{"Read"}, built: []*reportedNameTool{{}}, kind: tool.BuiltToolInfoInvalid},
		{name: "empty actual name", declared: []string{"Read"}, built: []*reportedNameTool{{info: &tool.ToolInfo{Name: " "}}}, kind: tool.BuiltToolNameEmpty},
		{name: "duplicate actual name", declared: []string{"Read"}, built: []*reportedNameTool{{info: &tool.ToolInfo{Name: "Read"}}, {info: &tool.ToolInfo{Name: " Read "}}}, kind: tool.BuiltToolNameDuplicate},
		{name: "stale declaration", declared: []string{"Read", "Write"}, built: []*reportedNameTool{{info: &tool.ToolInfo{Name: "Read"}}, {info: &tool.ToolInfo{Name: "Edit"}}}, kind: tool.ProducedToolNamesMismatch},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition := tool.NewBundleDefinition("bundle", tt.declared, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
				built := make([]tool.InvokableTool, len(tt.built))
				for i := range tt.built {
					built[i] = tt.built[i]
				}
				return built, nil
			})
			_, err := definition.Build(context.Background(), validBindings())
			var namesErr *tool.ProducedToolNamesError
			if !errors.As(err, &namesErr) {
				t.Fatalf("Build() error = %T %v, want *tool.ProducedToolNamesError", err, err)
			}
			if namesErr.Kind != tt.kind {
				t.Fatalf("ProducedToolNamesError.Kind = %q, want %q", namesErr.Kind, tt.kind)
			}
		})
	}
}

func TestDefinitionBuildWrapsToolInfoError(t *testing.T) {
	t.Parallel()

	infoErr := errors.New("describe failed")
	definition := tool.NewDefinition("Read", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{&reportedNameTool{err: infoErr}}, nil
	})
	_, err := definition.Build(context.Background(), validBindings())
	var namesErr *tool.ProducedToolNamesError
	if !errors.As(err, &namesErr) || namesErr.Kind != tool.BuiltToolInfoInvalid {
		t.Fatalf("Build() error = %T %v, want BuiltToolInfoInvalid", err, err)
	}
	if !errors.Is(err, infoErr) {
		t.Fatalf("Build() error does not wrap Info error %v", infoErr)
	}
}

func TestDefinitionBuildComparesNormalizedProducedNameSets(t *testing.T) {
	t.Parallel()

	definition := tool.NewBundleDefinition("bundle", []string{" Write ", "Read"}, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{
			&reportedNameTool{info: &tool.ToolInfo{Name: "Read"}},
			&reportedNameTool{info: &tool.ToolInfo{Name: " Write "}},
		}, nil
	})
	if _, err := definition.Build(context.Background(), validBindings()); err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestDefinitionValidation(t *testing.T) {
	t.Parallel()

	validFactory := func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{&definitionTool{}}, nil
	}
	tests := []struct {
		name       string
		definition tool.Definition
		wantField  string
	}{
		{name: "valid definition", definition: tool.NewDefinition("custom", 0, validFactory)},
		{name: "empty name", definition: tool.NewDefinition("", 0, validFactory), wantField: "name"},
		{name: "nil factory", definition: tool.NewDefinition("custom", 0, nil), wantField: "factory"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.definition.Build(context.Background(), validBindings())
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("Build() error = %v, want nil", err)
				}
				return
			}
			var validationErr *tool.InvalidDefinitionError
			if !errors.As(err, &validationErr) {
				t.Fatalf("Build() error = %T %v, want *tool.InvalidDefinitionError", err, err)
			}
			if validationErr.Field != tt.wantField {
				t.Fatalf("InvalidDefinitionError.Field = %q, want %q", validationErr.Field, tt.wantField)
			}
		})
	}
}

func TestBindingsValidation(t *testing.T) {
	t.Parallel()

	healthErr := errors.New("coordinator unhealthy")
	tests := []struct {
		name         string
		ctx          context.Context
		requirements tool.Requirements
		bindings     tool.Bindings
		wantField    string
		wantMissing  tool.Requirements
	}{
		{name: "valid bindings", ctx: context.Background(), bindings: validBindings()},
		{name: "nil context", bindings: validBindings(), wantField: "context"},
		{name: "zero session id", ctx: context.Background(), bindings: func() tool.Bindings { b := validBindings(); b.SessionID = uuid.UUID{}; return b }(), wantField: "session_id"},
		{name: "zero loop id", ctx: context.Background(), bindings: func() tool.Bindings { b := validBindings(); b.LoopID = uuid.UUID{}; return b }(), wantField: "loop_id"},
		{name: "missing workspace", ctx: context.Background(), requirements: tool.RequiresWorkspace, bindings: func() tool.Bindings { b := validBindings(); b.Workspace = nil; return b }(), wantMissing: tool.RequiresWorkspace},
		{name: "empty workspace root", ctx: context.Background(), requirements: tool.RequiresWorkspace, bindings: func() tool.Bindings { b := validBindings(); b.Workspace.Root = ""; return b }(), wantField: "workspace.root"},
		{name: "nil workspace coordinator", ctx: context.Background(), requirements: tool.RequiresWorkspace, bindings: func() tool.Bindings { b := validBindings(); b.Workspace.Coordinator = nil; return b }(), wantField: "workspace.coordinator"},
		{name: "typed nil workspace coordinator", ctx: context.Background(), requirements: tool.RequiresWorkspace, bindings: func() tool.Bindings {
			b := validBindings()
			b.Workspace.Coordinator = (*coordinatorStub)(nil)
			return b
		}(), wantField: "workspace.coordinator"},
		{name: "unhealthy workspace coordinator", ctx: context.Background(), requirements: tool.RequiresWorkspace, bindings: func() tool.Bindings {
			b := validBindings()
			b.Workspace.Coordinator = &coordinatorStub{healthErr: healthErr}
			return b
		}(), wantField: "workspace.coordinator"},
		{name: "missing delegate controller", ctx: context.Background(), requirements: tool.RequiresDelegateController, bindings: func() tool.Bindings { b := validBindings(); b.Delegate = nil; return b }(), wantMissing: tool.RequiresDelegateController},
		{name: "typed nil delegate controller", ctx: context.Background(), requirements: tool.RequiresDelegateController, bindings: func() tool.Bindings {
			b := validBindings()
			b.Delegate = (*delegateStub)(nil)
			return b
		}(), wantMissing: tool.RequiresDelegateController},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition := tool.NewDefinition("custom", tt.requirements, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{&definitionTool{}}, nil
			})
			_, err := definition.Build(tt.ctx, tt.bindings)
			if tt.wantField == "" && tt.wantMissing == 0 {
				if err != nil {
					t.Fatalf("Build() error = %v, want nil", err)
				}
				return
			}
			if tt.wantMissing != 0 {
				var missingErr *tool.MissingBindingError
				if !errors.As(err, &missingErr) {
					t.Fatalf("Build() error = %T %v, want *tool.MissingBindingError", err, err)
				}
				if missingErr.Requirement != tt.wantMissing {
					t.Fatalf("MissingBindingError.Requirement = %v, want %v", missingErr.Requirement, tt.wantMissing)
				}
				return
			}
			var bindingErr *tool.InvalidBindingsError
			if !errors.As(err, &bindingErr) {
				t.Fatalf("Build() error = %T %v, want *tool.InvalidBindingsError", err, err)
			}
			if bindingErr.Field != tt.wantField {
				t.Fatalf("InvalidBindingsError.Field = %q, want %q", bindingErr.Field, tt.wantField)
			}
		})
	}
}

func TestProcessBindingRequiresRegistry(t *testing.T) {
	t.Parallel()

	bindings := validBindings()
	bindings.Process = &tool.ProcessBinding{Runner: &asyncProcessRunnerStub{}}
	definition := tool.NewDefinition("custom", tool.RequiresProcessServices, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{&definitionTool{}}, nil
	})

	_, err := definition.Build(context.Background(), bindings)
	var bindingErr *tool.InvalidBindingsError
	if !errors.As(err, &bindingErr) {
		t.Fatalf("Build() error = %T %v, want *tool.InvalidBindingsError", err, err)
	}
	if bindingErr.Field != "process.registry" {
		t.Fatalf("InvalidBindingsError.Field = %q, want process.registry", bindingErr.Field)
	}
}

func TestProcessBindingRejectsTypedNilServices(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		process   *tool.ProcessBinding
		wantField string
	}{
		{
			name: "typed nil registry",
			process: &tool.ProcessBinding{
				Registry: (*sessionResourceRegistryStub)(nil),
				Runner:   &asyncProcessRunnerStub{},
			},
			wantField: "process.registry",
		},
		{
			name: "typed nil runner",
			process: &tool.ProcessBinding{
				Registry: &sessionResourceRegistryStub{},
				Runner:   (*asyncProcessRunnerStub)(nil),
			},
			wantField: "process.runner",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bindings := validBindings()
			bindings.Process = tt.process
			definition := tool.NewDefinition("custom", tool.RequiresProcessServices, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{&definitionTool{}}, nil
			})

			_, err := definition.Build(context.Background(), bindings)
			var bindingErr *tool.InvalidBindingsError
			if !errors.As(err, &bindingErr) {
				t.Fatalf("Build() error = %T %v, want *tool.InvalidBindingsError", err, err)
			}
			if bindingErr.Field != tt.wantField {
				t.Fatalf("InvalidBindingsError.Field = %q, want %q", bindingErr.Field, tt.wantField)
			}
		})
	}
}

func TestAttenuateBindingsPreservesOnlyRequiredProcessServices(t *testing.T) {
	t.Parallel()

	bindings := validBindings()
	originalProcess := bindings.Process
	definition := tool.NewDefinition("custom", tool.RequiresProcessServices, func(_ context.Context, got tool.Bindings) ([]tool.InvokableTool, error) {
		if got.Process == nil {
			t.Fatal("factory process binding = nil, want required process services")
		}
		if got.Process == originalProcess {
			t.Fatal("factory process binding aliases caller binding")
		}
		if got.Process.Registry != originalProcess.Registry || got.Process.Runner != originalProcess.Runner {
			t.Fatal("factory process services differ from caller services")
		}
		if got.Workspace != nil || got.Delegate != nil || got.ExtraTools != nil {
			t.Fatalf("factory received undeclared bindings: %+v", got)
		}
		got.Process.Registry = nil
		return []tool.InvokableTool{&definitionTool{}}, nil
	})

	if _, err := definition.Build(context.Background(), bindings); err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if bindings.Process != originalProcess || bindings.Process.Registry == nil {
		t.Fatal("factory mutation changed caller process binding")
	}
}

func TestDefinitionRejectsUnknownRequirementsBeforeFactory(t *testing.T) {
	t.Parallel()

	const unknown tool.Requirements = 1 << 7
	called := false
	definition := tool.NewDefinition("custom", unknown, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		called = true
		return []tool.InvokableTool{&definitionTool{}}, nil
	})

	_, err := definition.Build(context.Background(), validBindings())
	var requirementsErr *tool.InvalidRequirementsError
	if !errors.As(err, &requirementsErr) {
		t.Fatalf("Build() error = %T %v, want *tool.InvalidRequirementsError", err, err)
	}
	if requirementsErr.Unknown != unknown {
		t.Fatalf("InvalidRequirementsError.Unknown = %v, want %v", requirementsErr.Unknown, unknown)
	}
	if called {
		t.Fatal("factory called for unknown requirements")
	}
}

func TestDefinitionAttenuatesBindingsBeforeFactory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		requirements tool.Requirements
		check        func(*testing.T, tool.Bindings)
	}{
		{
			name: "undeclared capabilities are absent",
			check: func(t *testing.T, got tool.Bindings) {
				t.Helper()
				if got.Workspace != nil || got.Delegate != nil {
					t.Fatalf("factory bindings capabilities = (%v, %v), want both nil", got.Workspace, got.Delegate)
				}
			},
		},
		{
			name:         "workspace value is copied and delegate is absent",
			requirements: tool.RequiresWorkspace,
			check: func(t *testing.T, got tool.Bindings) {
				t.Helper()
				if got.Workspace == nil || got.Delegate != nil {
					t.Fatalf("factory bindings capabilities = (%v, %v), want copied workspace and nil delegate", got.Workspace, got.Delegate)
				}
				got.Workspace.Root = "/mutated"
			},
		},
		{
			name:         "delegate is present and workspace is absent",
			requirements: tool.RequiresDelegateController,
			check: func(t *testing.T, got tool.Bindings) {
				t.Helper()
				if got.Workspace != nil || got.Delegate == nil {
					t.Fatalf("factory bindings capabilities = (%v, %v), want nil workspace and declared delegate", got.Workspace, got.Delegate)
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bindings := validBindings()
			originalWorkspace := bindings.Workspace
			definition := tool.NewDefinition("custom", tt.requirements, func(_ context.Context, got tool.Bindings) ([]tool.InvokableTool, error) {
				tt.check(t, got)
				return []tool.InvokableTool{&definitionTool{}}, nil
			})
			if _, err := definition.Build(context.Background(), bindings); err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if bindings.Workspace != originalWorkspace || bindings.Workspace.Root != "/workspace" {
				t.Fatalf("caller workspace mutated: pointer=%p root=%q", bindings.Workspace, bindings.Workspace.Root)
			}
		})
	}
}

func TestDefinitionConcurrentBuildsAreFresh(t *testing.T) {
	t.Parallel()

	const builds = 32
	definition := tool.NewDefinition("custom", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{&definitionTool{}}, nil
	})
	results := make(chan tool.InvokableTool, builds)
	var wg sync.WaitGroup
	for range builds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			built, err := definition.Build(context.Background(), validBindings())
			if err != nil {
				t.Errorf("Build() error = %v", err)
				return
			}
			results <- built[0]
		}()
	}
	wg.Wait()
	close(results)
	seen := make(map[tool.InvokableTool]struct{}, builds)
	for built := range results {
		if _, exists := seen[built]; exists {
			t.Fatal("concurrent Build() reused a tool instance")
		}
		seen[built] = struct{}{}
	}
	if len(seen) != builds {
		t.Fatalf("fresh tool instances = %d, want %d", len(seen), builds)
	}
}

func TestDefinitionRejectsNilBuiltTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		factory tool.Factory
		index   int
		wantErr bool
	}{
		{name: "non-nil tool", factory: func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{&definitionTool{}}, nil
		}},
		{name: "nil result slice", factory: func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) { return nil, nil }, index: -1, wantErr: true},
		{name: "nil tool element", factory: func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{nil}, nil
		}, index: 0, wantErr: true},
		{name: "typed nil tool element", factory: func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			var builtTool *definitionTool
			return []tool.InvokableTool{builtTool}, nil
		}, index: 0, wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition := tool.NewDefinition("custom", 0, tt.factory)
			_, err := definition.Build(context.Background(), validBindings())
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Build() error = %v, want nil", err)
				}
				return
			}
			var nilToolErr *tool.NilBuiltToolError
			if !errors.As(err, &nilToolErr) {
				t.Fatalf("Build() error = %T %v, want *tool.NilBuiltToolError", err, err)
			}
			if nilToolErr.Index != tt.index {
				t.Fatalf("NilBuiltToolError.Index = %d, want %d", nilToolErr.Index, tt.index)
			}
		})
	}
}

func validBindings() tool.Bindings {
	return tool.Bindings{
		SessionID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		LoopID:    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		Workspace: &tool.WorkspaceBinding{Root: "/workspace", Coordinator: &coordinatorStub{}},
		Delegate:  &delegateStub{},
		Process: &tool.ProcessBinding{
			Registry: &sessionResourceRegistryStub{},
			Runner:   &asyncProcessRunnerStub{},
		},
	}
}
