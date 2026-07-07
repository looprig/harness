// Package gate defines the durable domain envelope for human and policy gates.
package gate

import "github.com/looprig/core/uuid"

// ID is the shared gate identifier type. It aliases uuid.UUID so existing
// text and JSON codecs are preserved exactly.
type ID = uuid.UUID

type Kind string
type ResolverKind string
type Blocks string
type Effect string
type CloseReason string
type Criticality string

const (
	KindPermission Kind = "harness.permission"
	KindAskUser    Kind = "harness.ask_user"
)

const (
	ResolverLoop    ResolverKind = "loop"
	ResolverSession ResolverKind = "session"
)

const (
	BlocksToolCall Blocks = "tool_call"
	BlocksSession  Blocks = "session"
)

const (
	EffectResume   Effect = "resume"
	EffectInitiate Effect = "initiate"
	EffectControl  Effect = "control"
)

const (
	CloseAnswered           CloseReason = "answered"
	ClosePolicyResponse     CloseReason = "policy_response"
	CloseAbandoned          CloseReason = "abandoned"
	CloseOwnerClosed        CloseReason = "owner_closed"
	CloseRestoreUnavailable CloseReason = "restore_unavailable"
)

const (
	GateCritical    Criticality = "critical"
	GateNonCritical Criticality = "non_critical"
)

type Subject struct {
	ToolExecutionID ID     `json:"tool_execution_id,omitzero"`
	ToolUseID       string `json:"tool_use_id,omitempty"`
	TurnID          ID     `json:"turn_id,omitzero"`
	StepID          ID     `json:"step_id,omitzero"`
	InputID         ID     `json:"input_id,omitzero"`
}

type Route struct {
	GateID          ID `json:"gate_id,omitzero"`
	LoopID          ID `json:"loop_id,omitzero"`
	ToolExecutionID ID `json:"tool_execution_id,omitzero"`
}

type Gate struct {
	ID             ID             `json:"id,omitzero"`
	Kind           Kind           `json:"kind,omitempty"`
	Resolver       ResolverKind   `json:"resolver,omitempty"`
	Blocks         Blocks         `json:"blocks,omitempty"`
	Effect         Effect         `json:"effect,omitempty"`
	Criticality    Criticality    `json:"criticality,omitempty"`
	Subject        Subject        `json:"subject,omitzero"`
	Prompt         Prompt         `json:"prompt,omitzero"`
	ResponsePolicy ResponsePolicy `json:"response_policy,omitzero"`
	Restorable     bool           `json:"restorable,omitzero"`
}
