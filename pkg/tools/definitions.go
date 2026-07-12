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
		// The loop's observation map: the SHARED set from the binding when present
		// (so this loop's Bash tool can invalidate exactly these observations), else a
		// fresh private one. One map per binding (per primer/delegate/restored loop),
		// shared across this loop's ReadFile/WriteFile/EditFile so a complete read
		// authorizes a subsequent same-path mutation. A restored loop starts empty.
		obs, err := loopObservations(bindings.Workspace.Observations)
		if err != nil {
			return nil, err
		}
		// The structured mutators additionally run their commit under a session
		// PathMutation permit from the coordinator (serializing same-real-file writes
		// across loops, excluded by a Bash/checkpoint permit).
		coord := bindings.Workspace.Coordinator
		return []tool.InvokableTool{
			NewReadFile(root, readGuard, obs),
			NewWriteFile(root, obs, WithMutationCoordinator(coord)),
			NewEditFile(root, obs, WithMutationCoordinator(coord)),
		}, nil
	})
}

// loopObservations resolves the loop's observation map. A nil binding set yields a
// fresh private map (the standalone/bare path). A non-nil set MUST be the package's
// own concrete map — NewObservations is the only producer — so the file tools and Bash
// share exactly one set. A non-nil set of any OTHER concrete type is a fail-secure
// build error rather than a silent private fallback: silently building a divergent
// private map here would leave Bash calling InvalidateAll on the foreign object while
// the file tools recorded into a different map, defeating cross-Bash staleness
// protection without a signal.
func loopObservations(shared tool.WorkspaceObservations) (*fileObservations, error) {
	if nilInterface(shared) {
		return newFileObservations(), nil
	}
	fo, ok := shared.(*fileObservations)
	if !ok {
		return nil, &DefinitionBuildError{Definition: "Files", Dependency: "workspace.observations"}
	}
	return fo, nil
}

// Bash defines a workspace-bound Bash tool. Caller options are eagerly resolved into
// sealed values so later caller mutation and option closure state cannot change the
// immutable definition; the per-loop workspace coordinator and shared observation set
// come from the binding at Build (they cannot be known at definition time).
func Bash(opts ...BashOption) tool.Definition {
	config, initErr := resolveBashOptions(opts)
	return tool.NewDefinition(bashToolName, tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if initErr != nil {
			return nil, initErr
		}
		bound := config
		bound.coord = bindings.Workspace.Coordinator
		bound.obs = bindings.Workspace.Observations
		return []tool.InvokableTool{newBash(bindings.Workspace.Root, bound)}, nil
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
