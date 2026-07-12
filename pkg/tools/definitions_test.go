package tools

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

type definitionCoordinator struct{}

func (*definitionCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return definitionPermit{}, nil
}

func (*definitionCoordinator) Healthy() error { return nil }

type definitionPermit struct{}

func (definitionPermit) Release() {}

type recordingDelegate struct {
	requests []tool.DelegateRequest
	result   tool.DelegateResult
}

func (d *recordingDelegate) Execute(_ context.Context, request tool.DelegateRequest) (tool.DelegateResult, error) {
	d.requests = append(d.requests, request)
	return d.result, nil
}

type definitionRunner struct{}

func (*definitionRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}

func TestDefinitionBlueprints(t *testing.T) {
	t.Parallel()

	guard := &fakeReadGuard{maxBytes: 1024}
	runner := &definitionRunner{}
	tests := []struct {
		name          string
		definition    tool.Definition
		wantName      string
		wantReqs      tool.Requirements
		wantToolCount int
		check         func(*testing.T, []tool.InvokableTool)
	}{
		{
			name:          "files",
			definition:    Files(guard),
			wantName:      "Files",
			wantReqs:      tool.RequiresWorkspace,
			wantToolCount: 3,
			check: func(t *testing.T, built []tool.InvokableTool) {
				t.Helper()
				read, ok := built[0].(*ReadFile)
				if !ok || read.guard != guard || read.root != "/workspace" {
					t.Fatalf("first tool = %#v, want workspace-bound *ReadFile", built[0])
				}
				write, ok := built[1].(*WriteFile)
				if !ok || write.root != "/workspace" {
					t.Fatalf("second tool = %#v, want workspace-bound *WriteFile", built[1])
				}
				edit, ok := built[2].(*EditFile)
				if !ok || edit.root != "/workspace" {
					t.Fatalf("third tool = %#v, want workspace-bound *EditFile", built[2])
				}
				// All three tools of ONE binding share ONE observation map, so a
				// ReadFile authorizes the same loop's WriteFile/EditFile.
				if read.obs == nil || read.obs != write.obs || read.obs != edit.obs {
					t.Fatalf("Files tools do not share one observation map: read=%p write=%p edit=%p", read.obs, write.obs, edit.obs)
				}
			},
		},
		{
			name:          "bash",
			definition:    Bash(WithRunner(runner)),
			wantName:      "Bash",
			wantReqs:      tool.RequiresWorkspace,
			wantToolCount: 1,
			check: func(t *testing.T, built []tool.InvokableTool) {
				t.Helper()
				bash, ok := built[0].(*BashTool)
				if !ok || bash.root != "/workspace" || bash.runner != runner {
					t.Fatalf("tool = %#v, want configured workspace-bound *BashTool", built[0])
				}
			},
		},
		{
			name:          "subagent",
			definition:    Subagent(loop.DelegationManaged, nil),
			wantName:      "Subagent",
			wantReqs:      tool.RequiresDelegateController,
			wantToolCount: 1,
			check: func(t *testing.T, built []tool.InvokableTool) {
				t.Helper()
				if _, ok := built[0].(*SubagentTool); !ok {
					t.Fatalf("tool = %T, want *SubagentTool", built[0])
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.definition.Name(); got != tt.wantName {
				t.Fatalf("Name() = %q, want %q", got, tt.wantName)
			}
			if got := tt.definition.Requirements(); got != tt.wantReqs {
				t.Fatalf("Requirements() = %v, want %v", got, tt.wantReqs)
			}
			first, err := tt.definition.Build(context.Background(), blueprintBindings(&recordingDelegate{}))
			if err != nil {
				t.Fatalf("first Build() error = %v", err)
			}
			second, err := tt.definition.Build(context.Background(), blueprintBindings(&recordingDelegate{}))
			if err != nil {
				t.Fatalf("second Build() error = %v", err)
			}
			if len(first) != tt.wantToolCount || len(second) != tt.wantToolCount {
				t.Fatalf("Build() lengths = %d, %d; want %d", len(first), len(second), tt.wantToolCount)
			}
			for i := range first {
				if first[i] == second[i] {
					t.Fatalf("Build() reused tool %d", i)
				}
			}
			tt.check(t, first)
		})
	}
}

func TestDefinitionBashCopiesOptions(t *testing.T) {
	t.Parallel()

	runner := &definitionRunner{}
	opts := []BashOption{WithRunner(runner)}
	definition := Bash(opts...)
	opts[0] = WithRunner(nil)

	tests := []struct {
		name string
	}{
		{name: "caller mutation does not change definition"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			built, err := definition.Build(context.Background(), blueprintBindings(&recordingDelegate{}))
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			bash := built[0].(*BashTool)
			if bash.runner != runner {
				t.Fatalf("runner = %T, want original %T", bash.runner, runner)
			}
		})
	}
}

func TestDefinitionBlueprintDependencyValidation(t *testing.T) {
	t.Parallel()

	var typedNilGuard *fakeReadGuard
	var typedNilRunner *definitionRunner
	tests := []struct {
		name           string
		definition     tool.Definition
		wantDefinition string
		wantDependency string
	}{
		{name: "files nil read guard", definition: Files(nil), wantDefinition: "Files", wantDependency: "read_guard"},
		{name: "files typed nil read guard", definition: Files(typedNilGuard), wantDefinition: "Files", wantDependency: "read_guard"},
		{name: "bash nil option", definition: Bash(nil), wantDefinition: "Bash", wantDependency: "option"},
		{name: "bash typed nil runner", definition: Bash(WithRunner(typedNilRunner)), wantDefinition: "Bash", wantDependency: "runner"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.definition.Build(context.Background(), blueprintBindings(&recordingDelegate{}))
			var buildErr *DefinitionBuildError
			if !errors.As(err, &buildErr) {
				t.Fatalf("Build() error = %T %v, want *DefinitionBuildError", err, err)
			}
			if buildErr.Definition != tt.wantDefinition || buildErr.Dependency != tt.wantDependency {
				t.Fatalf("DefinitionBuildError = %+v, want definition=%q dependency=%q", buildErr, tt.wantDefinition, tt.wantDependency)
			}
		})
	}
}

func TestDefinitionBashEagerlyResolvesOptions(t *testing.T) {
	t.Parallel()

	runner := &definitionRunner{}
	applications := 0
	definition := Bash(func(b *BashTool) {
		applications++
		b.runner = runner
	})
	if applications != 1 {
		t.Fatalf("option applications after Bash() = %d, want 1", applications)
	}
	for range 2 {
		built, err := definition.Build(context.Background(), blueprintBindings(&recordingDelegate{}))
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		if built[0].(*BashTool).runner != runner {
			t.Fatalf("built runner = %T, want %T", built[0].(*BashTool).runner, runner)
		}
	}
	if applications != 1 {
		t.Fatalf("option applications after Build() = %d, want 1", applications)
	}
}

func TestDefinitionConcurrentBuildsAreFresh(t *testing.T) {
	t.Parallel()

	const builds = 32
	definitions := []tool.Definition{Files(&fakeReadGuard{maxBytes: 1024}), Bash(WithRunner(&definitionRunner{})), Subagent(loop.DelegationManaged, nil)}
	for _, definition := range definitions {
		definition := definition
		t.Run(definition.Name(), func(t *testing.T) {
			t.Parallel()
			results := make(chan tool.InvokableTool, builds)
			var wg sync.WaitGroup
			for range builds {
				wg.Add(1)
				go func() {
					defer wg.Done()
					built, err := definition.Build(context.Background(), blueprintBindings(&recordingDelegate{}))
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
		})
	}
}

func TestDefinitionsFileAllowedImports(t *testing.T) {
	t.Parallel()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the test source path")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(filename), "definitions.go"), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse definitions.go: %v", err)
	}
	allowed := map[string]bool{
		`"context"`: true,
		`"reflect"`: true,
		`"github.com/looprig/harness/pkg/identity"`: true,
		`"github.com/looprig/harness/pkg/loop"`:     true,
		`"github.com/looprig/harness/pkg/tool"`:     true,
	}
	var unexpected []string
	for _, imported := range parsed.Imports {
		if !allowed[imported.Path.Value] {
			unexpected = append(unexpected, imported.Path.Value)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) != 0 {
		t.Fatalf("definitions.go imports outside the explicit dependency boundary: %v", unexpected)
	}
}

func TestDefinitionSubagentBindsController(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    map[string]string
		wantReq tool.DelegateRequest
	}{
		{
			name:    "start and synchronously wait",
			args:    map[string]string{"agent": "explorer", "message": "map repo"},
			wantReq: tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "explorer", Message: "map repo", Wait: true},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			controller := &recordingDelegate{result: tool.DelegateResult{
				DelegateID: uuid.MustParse("55555555-5555-4555-8555-555555555555"),
				Status:     tool.DelegateStatusCompleted,
				Output:     "done",
			}}
			built, err := Subagent(loop.DelegationManaged, nil).Build(context.Background(), blueprintBindings(controller))
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			args, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			result, err := built[0].InvokableRun(context.Background(), string(args))
			if err != nil {
				t.Fatalf("InvokableRun() error = %v", err)
			}
			if len(controller.requests) != 1 || controller.requests[0] != tt.wantReq {
				t.Fatalf("requests = %#v, want %#v", controller.requests, []tool.DelegateRequest{tt.wantReq})
			}
			if got := result.Content[0].(*content.TextBlock).Text; got != "done" {
				t.Fatalf("result text = %q, want done", got)
			}
		})
	}
}

func blueprintBindings(delegate tool.DelegateController) tool.Bindings {
	return tool.Bindings{
		SessionID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		LoopID:    uuid.MustParse("44444444-4444-4444-8444-444444444444"),
		Workspace: &tool.WorkspaceBinding{Root: "/workspace", Coordinator: &definitionCoordinator{}},
		Delegate:  delegate,
	}
}

var _ loop.ReadGuard = (*fakeReadGuard)(nil)
