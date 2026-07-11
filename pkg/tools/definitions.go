package tools

import (
	"context"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// Files defines the workspace-bound ReadFile, WriteFile, and EditFile bundle.
// Every Build receives fresh concrete tools for that binding.
func Files(readGuard loop.ReadGuard) tool.Definition {
	return tool.NewDefinition("Files", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		root := bindings.Workspace.Root
		return []tool.InvokableTool{
			NewReadFile(root, readGuard),
			NewWriteFile(root),
			NewEditFile(root),
		}, nil
	})
}

// Bash defines a workspace-bound Bash tool. The option slice is copied so later
// caller mutation cannot change the immutable definition.
func Bash(opts ...BashOption) tool.Definition {
	copiedOpts := append([]BashOption(nil), opts...)
	return tool.NewDefinition(bashToolName, tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{NewBash(bindings.Workspace.Root, copiedOpts...)}, nil
	})
}

// Subagent defines a delegation tool bound only to the parent-scoped controller
// supplied at Build time.
func Subagent() tool.Definition {
	return tool.NewDefinition(subagentToolName, tool.RequiresDelegateController, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{NewSubagent(delegateSpawner{controller: bindings.Delegate}, nil)}, nil
	})
}

type delegateSpawner struct{ controller tool.DelegateController }

func (s delegateSpawner) Spawn(
	ctx context.Context,
	_ loop.Provenance,
	agent identity.AgentName,
	message string,
	parentToolUseID string,
) (string, error) {
	result, err := s.controller.Execute(ctx, tool.DelegateRequest{
		Operation:       tool.DelegateStart,
		Agent:           string(agent),
		Message:         message,
		Wait:            true,
		ParentToolUseID: parentToolUseID,
	})
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

var _ Spawner = delegateSpawner{}
