package tools

import (
	"context"
	"encoding/json"
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
				if write, ok := built[1].(*WriteFile); !ok || write.root != "/workspace" {
					t.Fatalf("second tool = %#v, want workspace-bound *WriteFile", built[1])
				}
				if edit, ok := built[2].(*EditFile); !ok || edit.root != "/workspace" {
					t.Fatalf("third tool = %#v, want workspace-bound *EditFile", built[2])
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
			definition:    Subagent(),
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
			controller := &recordingDelegate{result: tool.DelegateResult{Output: "done"}}
			built, err := Subagent().Build(context.Background(), blueprintBindings(controller))
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
