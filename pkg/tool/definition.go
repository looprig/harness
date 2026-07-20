package tool

import (
	"context"
	"reflect"
	"sort"
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

// FileObservation is the private concurrency token standard file tools keep for one
// canonical path. It never appears in model output, events, or audit summaries.
type FileObservation struct {
	Observed bool
	Present  bool
	Hash     [32]byte
}

// WorkspaceObservations is the loop-scoped file-observation set shared between one
// loop's file tools and opaque workspace mutators such as Bash. WithPath holds the
// path's critical section for the callback, allowing a tool to compare filesystem
// state and commit atomically relative to other operations in the same Loop.
type WorkspaceObservations interface {
	WithPath(canonicalPath string, fn func(*FileObservation) error) error
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
// present it is created once per loop binding so independent file definitions and
// Bash share exactly the same state.
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
	ProducedToolNames() []string
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
	name          string
	producedNames []string
	requirements  Requirements
	factory       Factory
}

// NewDefinition returns an immutable, factory-backed definition. Validation is
// performed by Build so the constructor remains composable in declarative rig
// configuration while still returning typed failures at the runtime boundary.
func NewDefinition(name string, requirements Requirements, factory Factory) Definition {
	return NewBundleDefinition(name, []string{name}, requirements, factory)
}

// NewBundleDefinition returns an immutable factory-backed definition whose one
// build produces the declared concrete model-facing tool names. The metadata lets
// composition fingerprint a bundle without invoking its runtime-bound factory.
func NewBundleDefinition(name string, producedToolNames []string, requirements Requirements, factory Factory) Definition {
	return &factoryDefinition{
		name:          name,
		producedNames: append([]string(nil), producedToolNames...),
		requirements:  requirements,
		factory:       factory,
	}
}

func (d *factoryDefinition) Name() string { return d.name }

// ProducedToolNames returns a defensive copy of the stable concrete tool names
// this definition's factory produces.
func (d *factoryDefinition) ProducedToolNames() []string {
	return append([]string(nil), d.producedNames...)
}

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
	declared, err := normalizeDeclaredToolNames(d.producedNames)
	if err != nil {
		return nil, err
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
	actual, err := normalizedBuiltToolNames(ctx, built)
	if err != nil {
		return nil, err
	}
	if !equalToolNameSets(declared, actual) {
		return nil, &ProducedToolNamesError{Kind: ProducedToolNamesMismatch, Index: -1, Declared: declared, Actual: actual}
	}
	result := make([]InvokableTool, len(built))
	copy(result, built)
	return result, nil
}

func normalizeDeclaredToolNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, &ProducedToolNamesError{Kind: ProducedToolNameEmpty, Index: -1}
	}
	normalized := make([]string, len(names))
	seen := make(map[string]struct{}, len(names))
	for i, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, &ProducedToolNamesError{Kind: ProducedToolNameEmpty, Index: i}
		}
		if _, exists := seen[name]; exists {
			return nil, &ProducedToolNamesError{Kind: ProducedToolNameDuplicate, Index: i, Name: name}
		}
		seen[name] = struct{}{}
		normalized[i] = name
	}
	sort.Strings(normalized)
	return normalized, nil
}

func normalizedBuiltToolNames(ctx context.Context, built []InvokableTool) ([]string, error) {
	normalized := make([]string, len(built))
	seen := make(map[string]struct{}, len(built))
	for i, builtTool := range built {
		info, err := builtTool.Info(ctx)
		if err != nil {
			return nil, &ProducedToolNamesError{Kind: BuiltToolInfoInvalid, Index: i, Cause: err}
		}
		if info == nil {
			return nil, &ProducedToolNamesError{Kind: BuiltToolInfoInvalid, Index: i}
		}
		name := strings.TrimSpace(info.Name)
		if name == "" {
			return nil, &ProducedToolNamesError{Kind: BuiltToolNameEmpty, Index: i}
		}
		if _, exists := seen[name]; exists {
			return nil, &ProducedToolNamesError{Kind: BuiltToolNameDuplicate, Index: i, Name: name}
		}
		seen[name] = struct{}{}
		normalized[i] = name
	}
	sort.Strings(normalized)
	return normalized, nil
}

func equalToolNameSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
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

// ProducedToolNamesErrorKind identifies a fail-closed produced-name metadata
// violation discovered while binding a definition.
type ProducedToolNamesErrorKind string

const (
	ProducedToolNameEmpty     ProducedToolNamesErrorKind = "declared_name_empty"
	ProducedToolNameDuplicate ProducedToolNamesErrorKind = "declared_name_duplicate"
	BuiltToolInfoInvalid      ProducedToolNamesErrorKind = "built_tool_info_invalid"
	BuiltToolNameEmpty        ProducedToolNamesErrorKind = "built_tool_name_empty"
	BuiltToolNameDuplicate    ProducedToolNamesErrorKind = "built_tool_name_duplicate"
	ProducedToolNamesMismatch ProducedToolNamesErrorKind = "produced_names_mismatch"
)

// ProducedToolNamesError reports that immutable produced-name metadata is
// invalid or does not exactly describe the concrete tools returned by Build.
// Declared and Actual contain normalized, sorted names for a set mismatch.
type ProducedToolNamesError struct {
	Kind     ProducedToolNamesErrorKind
	Index    int
	Name     string
	Declared []string
	Actual   []string
	Cause    error
}

func (e *ProducedToolNamesError) Error() string {
	message := "tool: invalid produced tool names: " + string(e.Kind)
	if e.Index >= 0 {
		message += " at index " + strconv.Itoa(e.Index)
	}
	if e.Name != "" {
		message += " (" + e.Name + ")"
	}
	if e.Kind == ProducedToolNamesMismatch {
		message += ": declared [" + strings.Join(e.Declared, ", ") + "] actual [" + strings.Join(e.Actual, ", ") + "]"
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *ProducedToolNamesError) Unwrap() error { return e.Cause }

var _ Definition = (*factoryDefinition)(nil)
