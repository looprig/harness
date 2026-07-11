package tool_test

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

type definitionTool struct{ marker byte }

func (*definitionTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "definition-test"}, nil
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
	}
}
