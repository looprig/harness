package tool

import (
	"context"
	"reflect"
	"strconv"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
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
	// WorkspaceOperationPathMutation permits mutation of canonicalPath. It is
	// SHARED across DIFFERENT canonical paths (many run concurrently) but
	// EXCLUSIVE on the SAME canonical path (per-path serialization), and is
	// wholly excluded by a WholeMutation or a Checkpoint permit.
	WorkspaceOperationPathMutation WorkspaceOperation = iota + 1
	// WorkspaceOperationWholeMutation permits a whole-workspace mutation (Bash and
	// other unknown-path mutators). It is EXCLUSIVE against every PathMutation and
	// against other whole/checkpoint permits.
	WorkspaceOperationWholeMutation
	// WorkspaceOperationCheckpoint is the exclusive snapshot/restore gate. Like a
	// WholeMutation it is EXCLUSIVE against every mutation and every other exclusive
	// permit, and it likewise requires an empty canonicalPath; it is a DISTINCT
	// operation so a checkpoint/restore actor names its intent (the checkpoint
	// boundary Task 15 consumes) rather than masquerading as a Bash mutation.
	WorkspaceOperationCheckpoint
)

// WorkspacePermit is an acquired workspace mutation permit. Release is
// idempotent so callers may safely defer it immediately after acquisition.
type WorkspacePermit interface {
	Release()
}

// WorkspaceCoordinator is the narrow workspace-mutation coordination seam used
// by runtime-bound tools. Acquire expects WorkspaceOperationPathMutation with a
// non-empty canonical workspace-contained path, and WorkspaceOperationWholeMutation
// or WorkspaceOperationCheckpoint with an empty canonicalPath. Acquire blocks until
// the permit is granted or ctx is done (a done ctx returns a typed error and removes
// the waiter). Healthy reports whether the underlying workspace lease is healthy; a
// structured mutator MUST NOT commit when it returns an error (fail-secure).
type WorkspaceCoordinator interface {
	Acquire(ctx context.Context, operation WorkspaceOperation, canonicalPath string) (WorkspacePermit, error)
	Healthy() error
}

// WorkspaceObservations is the loop-scoped file-observation set shared between one
// loop's file tools and its Bash tool. The file tools record/compare per-path hashes
// through it; Bash calls InvalidateAll after an opaque whole-workspace mutation
// because the changed paths are unknowable. Its concrete implementation lives in the
// tools package; the runtime treats it opaquely and only ever needs InvalidateAll.
type WorkspaceObservations interface {
	InvalidateAll()
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
	DelegateStatusTimedOut
	DelegateStatusQueued
	// DelegateStatusIdle is the mechanical "warm but not running a turn" state a
	// status operation reports for an owned child with no in-flight turn.
	DelegateStatusIdle
	// DelegateStatusDone is a compatibility alias for the canonical completed
	// state. It deliberately has no distinct wire or persisted value.
	DelegateStatusDone = DelegateStatusCompleted
)

// DelegateRequest is the typed command passed to a parent-scoped delegate
// controller. Fields not used by the selected Operation remain zero-valued.
//
// Mode is the requested initial mode for a DelegateStart; the empty string means
// "use the target definition's initial mode". Agent and Mode carry the untrusted
// model selection as plain strings (this package does not import the loop/identity
// domain types); the controller resolves and validates them.
//
// RequestID is meaningful only for DelegateWait, where it names one previously
// returned request: nil means "not supplied" (rejected for a wait), a non-nil
// pointer to the zero UUID means "supplied but invalid" (also rejected). For
// DelegateStart/DelegateSend the controller MINTS the request id and returns it in
// the DelegateResult.
//
// TimeoutSeconds bounds a waiting operation. nil means an interruptible, unbounded
// wait (only the parent turn's own cancellation can end it); a non-nil value is a
// non-negative second count after which the controller returns a typed timed-out
// result. A negative value is invalid and rejected by the envelope boundary.
type DelegateRequest struct {
	Operation       DelegateOperation
	DelegateID      uuid.UUID
	Agent           string
	Mode            string
	Message         string
	Wait            bool
	RequestID       *uuid.UUID
	TimeoutSeconds  *int
	ParentToolUseID string
}

// DelegateChildStatus is the bounded mechanical status of one owned child returned
// by a DelegateStatus that omitted DelegateID (report every owned child). It never
// carries a raw event cursor or child transcript.
type DelegateChildStatus struct {
	DelegateID      uuid.UUID
	Status          DelegateStatusValue
	PendingRequests int
}

// DelegateResult is the typed result of a delegate-controller operation.
//
// RequestID is the minted (DelegateStart/DelegateSend) or echoed (DelegateWait)
// request id that correlates a follow-up wait to its turn. PendingRequests and
// Children carry bounded mechanical status only: PendingRequests is the count of
// unresolved requests for a single addressed child, and Children is the per-child
// status list for a DelegateStatus that omitted DelegateID.
type DelegateResult struct {
	DelegateID      uuid.UUID
	RequestID       uuid.UUID
	Status          DelegateStatusValue
	Output          string
	PendingRequests int
	Children        []DelegateChildStatus
}

// DelegateController is the only delegation capability exposed to a built tool.
// The runtime binds it to the tool's parent; tools never receive a session
// controller.
type DelegateController interface {
	Execute(ctx context.Context, request DelegateRequest) (DelegateResult, error)
}

// WorkspaceBinding contains the workspace capabilities supplied at build time.
//
// Observations is the loop-scoped file-observation set shared by every workspace
// tool bound to the SAME loop (the file toolset and Bash). It is OPTIONAL: a nil
// value means "no shared set", in which case the file toolset builds its own private
// observation map and Bash performs no invalidation (the standalone/bare path). When
// present it is created once per loop binding so a Bash run can invalidate exactly
// the observations the loop's file tools recorded.
type WorkspaceBinding struct {
	Root         string
	Coordinator  WorkspaceCoordinator
	Observations WorkspaceObservations
}

// Bindings contains session-specific runtime capabilities supplied to a
// Definition. SessionID and LoopID must be non-zero. Definitions retain no
// Bindings between Build calls.
type Bindings struct {
	SessionID uuid.UUID
	LoopID    uuid.UUID
	// Ceiling is the exact live session-scoped ordinal source. Permission factories
	// read it on every check through consumer-defined posture tables.
	Ceiling   ceiling.Source
	Workspace *WorkspaceBinding
	Delegate  DelegateController
	// ExtraTools are additional tool definitions the LOOP appends to every mode's
	// toolset at Bind, beyond the definition's own WithTools. The composition root uses
	// it to inject a derived, definition-scoped tool (the delegation Subagent tool) into
	// a loop WITHOUT mutating the immutable loop definition. Per-tool factories never see
	// it (attenuateBindings drops it); only loop.Bind consumes it.
	ExtraTools []Definition
}

// Definition is immutable tool metadata plus a factory that builds concrete,
// session-bound tool instances.
type Definition interface {
	Name() string
	Requirements() Requirements
	Build(context.Context, Bindings) ([]InvokableTool, error)
	definition()
}

// Factory builds concrete tools for one runtime binding. It may be invoked
// concurrently by separate Build calls and must synchronize any shared captured
// state. Per-build mutable state belongs inside each invocation. A Factory must
// not return nil tools.
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
