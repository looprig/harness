package tool

import (
	"context"
	"reflect"
	"strconv"
	"strings"

	"github.com/looprig/core/uuid"
)

// Requirements is the set of runtime capabilities a Definition needs before it
// can build concrete tools.
type Requirements uint8

const (
	// RequiresWorkspace marks definitions that build workspace-bound tools.
	RequiresWorkspace Requirements = 1 << iota
	// RequiresDelegateController marks definitions that build delegation tools.
	RequiresDelegateController
	knownRequirements = RequiresWorkspace | RequiresDelegateController
)

// WorkspaceOperation identifies the scope of a workspace mutation permit.
type WorkspaceOperation uint8

const (
	// WorkspaceOperationPathMutation permits mutation of canonicalPath.
	WorkspaceOperationPathMutation WorkspaceOperation = iota + 1
	// WorkspaceOperationWholeMutation permits a whole-workspace mutation.
	WorkspaceOperationWholeMutation
)

// WorkspacePermit is an acquired workspace mutation permit. Release is
// idempotent so callers may safely defer it immediately after acquisition.
type WorkspacePermit interface {
	Release()
}

// WorkspaceCoordinator is the narrow workspace-mutation coordination seam used
// by runtime-bound tools. Acquire expects WorkspaceOperationPathMutation with a
// non-empty canonical workspace-contained path, and
// WorkspaceOperationWholeMutation with an empty canonicalPath.
type WorkspaceCoordinator interface {
	Acquire(ctx context.Context, operation WorkspaceOperation, canonicalPath string) (WorkspacePermit, error)
	Healthy() error
}

// DelegateOperation identifies an operation on a parent-scoped delegate.
type DelegateOperation uint8

const (
	DelegateStart DelegateOperation = iota + 1
	DelegateSend
	DelegateWait
	DelegateInterrupt
	DelegateStatus
)

// DelegateStatusValue is the lifecycle state returned by a delegate controller.
type DelegateStatusValue uint8

const (
	DelegateStatusUnknown DelegateStatusValue = iota
	DelegateStatusRunning
	DelegateStatusCompleted
	DelegateStatusInterrupted
	DelegateStatusFailed
	DelegateStatusDone
	DelegateStatusTimedOut
	DelegateStatusQueued
)

// DelegateRequest is the typed command passed to a parent-scoped delegate
// controller. Fields not used by the selected Operation remain zero-valued.
type DelegateRequest struct {
	Operation       DelegateOperation
	DelegateID      uuid.UUID
	Agent           string
	Message         string
	Wait            bool
	ParentToolUseID string
}

// DelegateResult is the typed result of a delegate-controller operation.
type DelegateResult struct {
	DelegateID uuid.UUID
	Status     DelegateStatusValue
	Output     string
}

// DelegateController is the only delegation capability exposed to a built tool.
// The runtime binds it to the tool's parent; tools never receive a session
// controller.
type DelegateController interface {
	Execute(ctx context.Context, request DelegateRequest) (DelegateResult, error)
}

// WorkspaceBinding contains the workspace capabilities supplied at build time.
type WorkspaceBinding struct {
	Root        string
	Coordinator WorkspaceCoordinator
}

// Bindings contains session-specific runtime capabilities supplied to a
// Definition. SessionID and LoopID must be non-zero. Definitions retain no
// Bindings between Build calls.
type Bindings struct {
	SessionID uuid.UUID
	LoopID    uuid.UUID
	Workspace *WorkspaceBinding
	Delegate  DelegateController
}

// Definition is immutable tool metadata plus a factory that builds concrete,
// session-bound tool instances.
type Definition interface {
	Name() string
	Requirements() Requirements
	Build(context.Context, Bindings) ([]InvokableTool, error)
	definition()
}

// Factory builds concrete tools for one runtime binding. It is invoked once per
// Build call and must not return nil tools.
type Factory func(context.Context, Bindings) ([]InvokableTool, error)

type factoryDefinition struct {
	name         string
	requirements Requirements
	factory      Factory
}

// NewDefinition returns an immutable, factory-backed definition. Validation is
// performed by Build so the constructor remains composable in declarative rig
// configuration while still returning typed failures at the runtime boundary.
func NewDefinition(name string, requirements Requirements, factory Factory) Definition {
	return &factoryDefinition{name: name, requirements: requirements, factory: factory}
}

func (d *factoryDefinition) Name() string { return d.name }

func (d *factoryDefinition) Requirements() Requirements { return d.requirements }

func (*factoryDefinition) definition() {}

// Build validates the definition and its runtime bindings, invokes the factory,
// rejects nil tools, and returns a defensive copy of the built slice.
func (d *factoryDefinition) Build(ctx context.Context, bindings Bindings) ([]InvokableTool, error) {
	if strings.TrimSpace(d.name) == "" {
		return nil, &InvalidDefinitionError{Field: "name"}
	}
	if d.factory == nil {
		return nil, &InvalidDefinitionError{Field: "factory"}
	}
	if ctx == nil {
		return nil, &InvalidBindingsError{Field: "context"}
	}
	if unknown := d.requirements &^ knownRequirements; unknown != 0 {
		return nil, &InvalidRequirementsError{Unknown: unknown}
	}
	if err := validateBindings(d.requirements, bindings); err != nil {
		return nil, err
	}
	built, err := d.factory(ctx, attenuateBindings(d.requirements, bindings))
	if err != nil {
		return nil, err
	}
	if built == nil {
		return nil, &NilBuiltToolError{Index: -1}
	}
	for i, builtTool := range built {
		if nilInvokableTool(builtTool) {
			return nil, &NilBuiltToolError{Index: i}
		}
	}
	result := make([]InvokableTool, len(built))
	copy(result, built)
	return result, nil
}

func attenuateBindings(requirements Requirements, bindings Bindings) Bindings {
	attenuated := Bindings{SessionID: bindings.SessionID, LoopID: bindings.LoopID}
	if requirements&RequiresWorkspace != 0 {
		workspace := *bindings.Workspace
		attenuated.Workspace = &workspace
	}
	if requirements&RequiresDelegateController != 0 {
		attenuated.Delegate = bindings.Delegate
	}
	return attenuated
}

func validateBindings(requirements Requirements, bindings Bindings) error {
	if bindings.SessionID.IsZero() {
		return &InvalidBindingsError{Field: "session_id"}
	}
	if bindings.LoopID.IsZero() {
		return &InvalidBindingsError{Field: "loop_id"}
	}
	if requirements&RequiresWorkspace != 0 {
		if bindings.Workspace == nil {
			return &MissingBindingError{Requirement: RequiresWorkspace}
		}
		if strings.TrimSpace(bindings.Workspace.Root) == "" {
			return &InvalidBindingsError{Field: "workspace.root"}
		}
		if nilWorkspaceCoordinator(bindings.Workspace.Coordinator) {
			return &InvalidBindingsError{Field: "workspace.coordinator"}
		}
		if err := bindings.Workspace.Coordinator.Healthy(); err != nil {
			return &InvalidBindingsError{Field: "workspace.coordinator", Cause: err}
		}
	}
	if requirements&RequiresDelegateController != 0 && nilDelegateController(bindings.Delegate) {
		return &MissingBindingError{Requirement: RequiresDelegateController}
	}
	return nil
}

func nilInvokableTool(value InvokableTool) bool {
	return value == nil || nilReflectValue(reflect.ValueOf(value))
}

func nilWorkspaceCoordinator(value WorkspaceCoordinator) bool {
	return value == nil || nilReflectValue(reflect.ValueOf(value))
}

func nilDelegateController(value DelegateController) bool {
	return value == nil || nilReflectValue(reflect.ValueOf(value))
}

func nilReflectValue(rv reflect.Value) bool {
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

// InvalidRequirementsError reports requirement bits unknown to this package.
type InvalidRequirementsError struct{ Unknown Requirements }

func (e *InvalidRequirementsError) Error() string {
	return "tool: invalid definition requirements: unknown bits " + strconv.Itoa(int(e.Unknown))
}

// InvalidDefinitionError reports invalid immutable definition metadata.
type InvalidDefinitionError struct{ Field string }

func (e *InvalidDefinitionError) Error() string {
	return "tool: invalid definition: " + e.Field + " is required"
}

// InvalidBindingsError reports a present but invalid runtime binding.
type InvalidBindingsError struct {
	Field string
	Cause error
}

func (e *InvalidBindingsError) Error() string {
	message := "tool: invalid bindings: " + e.Field
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

func (e *InvalidBindingsError) Unwrap() error { return e.Cause }

// MissingBindingError reports a required runtime capability that was absent.
type MissingBindingError struct{ Requirement Requirements }

func (e *MissingBindingError) Error() string {
	var name string
	switch e.Requirement {
	case RequiresWorkspace:
		name = "workspace"
	case RequiresDelegateController:
		name = "delegate controller"
	default:
		name = strconv.Itoa(int(e.Requirement))
	}
	return "tool: missing required binding " + name
}

// NilBuiltToolError reports a nil factory result. Index is -1 when the returned
// slice itself is nil, otherwise it identifies the nil element.
type NilBuiltToolError struct{ Index int }

func (e *NilBuiltToolError) Error() string {
	if e.Index < 0 {
		return "tool: factory returned a nil tool slice"
	}
	return "tool: factory returned nil tool at index " + strconv.Itoa(e.Index)
}

var _ Definition = (*factoryDefinition)(nil)
