package tools

import (
	"context"
	"reflect"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// Files defines the workspace-bound ReadFile, WriteFile, and EditFile bundle.
// Every Build receives fresh concrete tools for that binding.
func Files(readGuard loop.ReadGuard) tool.Definition {
	return tool.NewDefinition("Files", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if nilInterface(readGuard) {
			return nil, &DefinitionBuildError{Definition: "Files", Dependency: "read_guard"}
		}
		root := bindings.Workspace.Root
		return []tool.InvokableTool{
			NewReadFile(root, readGuard),
			NewWriteFile(root),
			NewEditFile(root),
		}, nil
	})
}

// Bash defines a workspace-bound Bash tool. Options are eagerly resolved into
// sealed values so later caller mutation and option closure state cannot change
// the immutable definition.
func Bash(opts ...BashOption) tool.Definition {
	config, initErr := resolveBashOptions(opts)
	return tool.NewDefinition(bashToolName, tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if initErr != nil {
			return nil, initErr
		}
		return []tool.InvokableTool{newBash(bindings.Workspace.Root, config)}, nil
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
	if result.DelegateID.IsZero() {
		return "", &delegateResultError{Status: result.Status, reason: "zero delegate id"}
	}
	switch result.Status {
	case tool.DelegateStatusCompleted:
		return result.Output, nil
	case tool.DelegateStatusFailed:
		return "", &delegateResultError{Status: result.Status, reason: "delegate failed"}
	case tool.DelegateStatusInterrupted:
		return "", &delegateResultError{Status: result.Status, reason: "delegate interrupted"}
	case tool.DelegateStatusTimedOut:
		return "", &delegateResultError{Status: result.Status, reason: "delegate timed out"}
	default:
		return "", &delegateResultError{Status: result.Status, reason: "invalid synchronous status"}
	}
}

type delegateResultError struct {
	Status tool.DelegateStatusValue
	reason string
}

func (e *delegateResultError) Error() string {
	return "tools: invalid delegate result: " + e.reason
}

var _ Spawner = delegateSpawner{}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

// DefinitionBuildError reports an invalid dependency captured by a built-in
// definition. It is returned from Definition.Build before any tool is exposed.
type DefinitionBuildError struct {
	Definition string
	Dependency string
	Cause      error
}

func (e *DefinitionBuildError) Error() string {
	message := "tools: " + e.Definition + " definition has invalid " + e.Dependency
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

func (e *DefinitionBuildError) Unwrap() error { return e.Cause }
