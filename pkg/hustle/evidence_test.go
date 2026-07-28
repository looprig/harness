package hustle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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
	policy := validEvidenceToolPolicy()
	policy.Definitions = definitions
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
	definition := tool.NewDefinition(
		"workspace-status",
		tool.RequiresWorkspace,
		func(_ context.Context, got tool.Bindings) ([]tool.InvokableTool, error) {
			builds++
			if got.SessionID != sessionID || got.LoopID != loopID {
				t.Fatalf("tool binding IDs = %v/%v, want %v/%v", got.SessionID, got.LoopID, sessionID, loopID)
			}
			if got.Workspace == nil || got.Workspace.Root != workspace.Root {
				t.Fatalf("workspace binding = %#v, want root %q", got.Workspace, workspace.Root)
			}
			if got.Delegate != nil || got.ExtraTools != nil {
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
		definition := bindableEvidenceDefinition(t, tool.NewDefinition(
			"status", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{concrete}, nil
			},
		))
		bound, err := definition.Bind(context.Background(), Bindings{SessionID: sessionID, LoopID: loopID})
		if err != nil {
			t.Fatalf("Bind() error = %v", err)
		}
		return bound.EvidenceTools()[0]
	}
	base := bind("read status", json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`))
	whitespace := bind("read status", json.RawMessage("{\n \"type\":\"object\", \"properties\":{\"path\":{\"type\":\"string\"}}\n}"))
	if base.IdentitySHA256() != whitespace.IdentitySHA256() {
		t.Fatal("schema whitespace changed canonical identity")
	}
	if base.IdentitySHA256() == bind("different", json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)).IdentitySHA256() {
		t.Fatal("description did not contribute to identity")
	}
	if base.IdentitySHA256() == bind("read status", json.RawMessage(`{"type":"object","properties":{"path":{"type":"number"}}}`)).IdentitySHA256() {
		t.Fatal("schema did not contribute to identity")
	}
}

func TestBindEvidenceToolsRejectsInvalidConcreteTools(t *testing.T) {
	t.Parallel()
	sessionID, loopID := evidenceBindingIDs(t)
	var typedNil *evidenceToolStub
	valid := &tool.ToolInfo{Name: "status", Desc: "read status", Schema: json.RawMessage(`{"type":"object"}`)}
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
		{name: "oversized description", tools: []tool.InvokableTool{&evidenceToolStub{infos: []*tool.ToolInfo{{Name: valid.Name, Desc: strings.Repeat("x", maxEvidenceToolDescriptionBytes+1), Schema: valid.Schema}}}}},
		{name: "oversized schema", tools: []tool.InvokableTool{&evidenceToolStub{infos: []*tool.ToolInfo{{Name: valid.Name, Desc: valid.Desc, Schema: json.RawMessage(`{"padding":"` + strings.Repeat("x", maxEvidenceToolSchemaBytes) + `"}`)}}}}},
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
	drifting := &evidenceToolStub{infos: []*tool.ToolInfo{
		{Name: "status", Desc: "first", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "status", Desc: "first", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "status", Desc: "second", Schema: json.RawMessage(`{"type":"object"}`)},
	}}
	definition := bindableEvidenceDefinition(t, tool.NewDefinition(
		"status", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
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
