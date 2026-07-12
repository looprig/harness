package tools

import (
	"context"
	"reflect"

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
		// One private observation map per binding (per primer/delegate/restored
		// loop), shared across this loop's ReadFile/WriteFile/EditFile so a complete
		// read authorizes a subsequent same-path mutation. A fresh Build gets a fresh
		// map: two loops never share observations, and a restored loop starts empty.
		obs := newFileObservations()
		return []tool.InvokableTool{
			NewReadFile(root, readGuard, obs),
			NewWriteFile(root, obs),
			NewEditFile(root, obs),
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

// Subagent defines the model-facing delegation tool: one action envelope bound to the
// parent-scoped controller supplied at Build time. The delegation style and delegate
// catalog are fixed at the composition root (the rig derives them from the parent
// definition) and drive the tool's model-facing schema and description; the injected
// controller re-enforces the action set, ownership, mode, and ceiling.
func Subagent(style loop.DelegationStyle, catalog []SubagentCatalogEntry) tool.Definition {
	catalog = cloneSubagentCatalog(catalog)
	return tool.NewDefinition(subagentToolName, tool.RequiresDelegateController, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{NewSubagent(bindings.Delegate, style, catalog)}, nil
	})
}

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
