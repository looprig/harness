package loop

import "fmt"

// DefinitionErrorKind identifies a declarative loop-definition failure.
type DefinitionErrorKind string

const (
	DefinitionMissingName           DefinitionErrorKind = "missing_name"
	DefinitionInvalidClient         DefinitionErrorKind = "invalid_client"
	DefinitionInvalidModel          DefinitionErrorKind = "invalid_model"
	DefinitionNilOption             DefinitionErrorKind = "nil_option"
	DefinitionDuplicateOption       DefinitionErrorKind = "duplicate_option"
	DefinitionInvalidTool           DefinitionErrorKind = "invalid_tool"
	DefinitionInvalidToolLimits     DefinitionErrorKind = "invalid_tool_limits"
	DefinitionInvalidDrainTimeout   DefinitionErrorKind = "invalid_drain_timeout"
	DefinitionInvalidMiddleware     DefinitionErrorKind = "invalid_middleware"
	DefinitionInvalidPermission     DefinitionErrorKind = "invalid_permission_factory"
	DefinitionInvalidEngine         DefinitionErrorKind = "invalid_engine"
	DefinitionInvalidRuntimeContext DefinitionErrorKind = "invalid_runtime_context"
	DefinitionInvalidDelegate       DefinitionErrorKind = "invalid_delegate"
	DefinitionInvalidDelegation     DefinitionErrorKind = "invalid_delegation"
	DefinitionInvalidMode           DefinitionErrorKind = "invalid_mode"
	DefinitionDuplicateMode         DefinitionErrorKind = "duplicate_mode"
	DefinitionMissingInitialMode    DefinitionErrorKind = "missing_initial_mode"
	DefinitionInvalidInitialMode    DefinitionErrorKind = "invalid_initial_mode"
	DefinitionMissingPolicyRevision DefinitionErrorKind = "missing_policy_revision"
	DefinitionInvalidPolicyRevision DefinitionErrorKind = "invalid_policy_revision"
)

// DefinitionError reports an invalid immutable definition.
type DefinitionError struct {
	Kind  DefinitionErrorKind
	Field string
	Value string
	Cause error
}

func (e *DefinitionError) Error() string {
	message := "loop: invalid definition: " + string(e.Kind)
	if e.Field != "" {
		message += " (" + e.Field + ")"
	}
	if e.Value != "" {
		message += ": " + e.Value
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *DefinitionError) Unwrap() error { return e.Cause }

// BindErrorKind identifies a runtime-binding failure.
type BindErrorKind string

const (
	BindInvalidDefinition       BindErrorKind = "invalid_definition"
	BindInvalidContext          BindErrorKind = "invalid_context"
	BindDuplicateDefinitionName BindErrorKind = "duplicate_definition_name"
	BindDuplicateToolName       BindErrorKind = "duplicate_tool_name"
	BindInvalidToolInfo         BindErrorKind = "invalid_tool_info"
	BindInvalidPermission       BindErrorKind = "invalid_permission"
	BindInvalidSessionID        BindErrorKind = "invalid_session_id"
	BindInvalidLoopID           BindErrorKind = "invalid_loop_id"
)

// BindError reports a failure while creating one loop's runtime collaborators.
type BindError struct {
	Kind  BindErrorKind
	Name  string
	Index int
	Cause error
}

func (e *BindError) Error() string {
	message := "loop: bind: " + string(e.Kind)
	if e.Name != "" {
		message += ": " + e.Name
	}
	if e.Index >= 0 {
		message += fmt.Sprintf(" at index %d", e.Index)
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *BindError) Unwrap() error { return e.Cause }
