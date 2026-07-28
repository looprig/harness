package hook

import (
	"encoding/json"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

// Call is the immutable typed snapshot supplied when an operation begins.
// Exactly one operation-specific payload must be non-nil and match Operation.
type Call struct {
	Operation   Operation
	StartedAt   time.Time
	Coordinates identity.Coordinates
	AgentName   identity.AgentName
	Cause       identity.Cause

	Turn          *TurnData
	Step          *StepData
	Inference     *InferenceData
	Compaction    *CompactionData
	ToolCall      *ToolCallData
	GateWait      *GateWaitData
	ToolExecution *ToolExecutionData
	JournalAppend *JournalAppendData
}

// Result is the terminal snapshot supplied to an around hook.
type Result struct {
	Call
	EndedAt time.Time
	Outcome Outcome
	Err     error
}

type TurnData struct {
	Index event.TurnIndex
	Input *content.UserMessage
}

type StepData struct {
	Index StepIndex
}

type InferenceData struct {
	Request      *inference.Request
	AIMessage    *content.AIMessage
	StreamResult *stream.StreamResult
}

type CompactionData struct {
	AttemptID event.CompactAttemptID
	Input     *loop.CompactionInput
	Output    *loop.CompactionOutput
}

type ToolCallData struct {
	ToolExecutionID  uuid.UUID
	ToolUseID        string
	ToolName         string
	Summary          string
	ArgsJSON         json.RawMessage
	PermissionEffect event.PermissionDecisionEffect
	PermissionReason string
	ResultPreview    string
	IsError          bool
}

type GateWaitData struct {
	GateID   gate.ID
	Kind     gate.Kind
	Resolver gate.ResolverKind
	Blocks   gate.Blocks
	Effect   gate.Effect
	Answer   *gate.Answer
}

type ToolExecutionData struct {
	ToolExecutionID uuid.UUID
	ToolUseID       string
	ToolName        string
	ArgsJSON        json.RawMessage
	Result          *tool.ToolResult
	ResultPreview   string
	IsError         bool
}

type JournalAppendData struct {
	Family   RecordFamily
	RecordID string
}

// ValidateCall validates the closed operation-payload union.
func ValidateCall(call Call) error {
	if !call.Operation.Valid() {
		return &CallError{Kind: CallUnknownOperation, Operation: call.Operation}
	}

	payloads := 0
	payloads += boolInt(call.Turn != nil)
	payloads += boolInt(call.Step != nil)
	payloads += boolInt(call.Inference != nil)
	payloads += boolInt(call.Compaction != nil)
	payloads += boolInt(call.ToolCall != nil)
	payloads += boolInt(call.GateWait != nil)
	payloads += boolInt(call.ToolExecution != nil)
	payloads += boolInt(call.JournalAppend != nil)
	if payloads != 1 || !call.payloadMatchesOperation() {
		return &CallError{Kind: CallInvalidPayload, Operation: call.Operation}
	}
	return nil
}

func (call Call) payloadMatchesOperation() bool {
	switch call.Operation {
	case OperationTurn:
		return call.Turn != nil
	case OperationStep:
		return call.Step != nil
	case OperationInference:
		return call.Inference != nil
	case OperationCompaction:
		return call.Compaction != nil
	case OperationToolCall:
		return call.ToolCall != nil
	case OperationGateWait:
		return call.GateWait != nil
	case OperationToolExecution:
		return call.ToolExecution != nil
	case OperationJournalAppend:
		return call.JournalAppend != nil
	default:
		return false
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
