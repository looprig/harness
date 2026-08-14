package hook

import (
	"encoding/json"
	"fmt"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

// CloneCall gives a hook independent ownership of every reference-backed
// payload while preserving nil-versus-empty distinctions. It panics with
// *CloneError if an upstream sealed content union gains an unsupported variant;
// it never silently drops unknown content.
func CloneCall(call Call) Call {
	cloned := call
	cloned.Turn = cloneTurnData(call.Turn)
	cloned.Step = cloneStepData(call.Step)
	cloned.Inference = cloneInferenceData(call.Inference)
	cloned.Compaction = cloneCompactionData(call.Compaction)
	cloned.ToolCall = cloneToolCallData(call.ToolCall)
	cloned.GateWait = cloneGateWaitData(call.GateWait)
	cloned.ToolExecution = cloneToolExecutionData(call.ToolExecution)
	cloned.JournalAppend = cloneJournalAppendData(call.JournalAppend)
	return cloned
}

// CloneResult clones the embedded Call while intentionally retaining Err.
func CloneResult(result Result) Result {
	cloned := result
	cloned.Call = CloneCall(result.Call)
	return cloned
}

func cloneTurnData(data *TurnData) *TurnData {
	if data == nil {
		return nil
	}
	cloned := *data
	cloned.Input = cloneUserMessage(data.Input)
	return &cloned
}

func cloneStepData(data *StepData) *StepData {
	if data == nil {
		return nil
	}
	cloned := *data
	return &cloned
}

func cloneInferenceData(data *InferenceData) *InferenceData {
	if data == nil {
		return nil
	}
	cloned := *data
	cloned.Request = cloneInferenceRequest(data.Request)
	cloned.AIMessage = cloneAIMessage(data.AIMessage)
	cloned.StreamResult = cloneStreamResult(data.StreamResult)
	return &cloned
}

func cloneCompactionData(data *CompactionData) *CompactionData {
	if data == nil {
		return nil
	}
	cloned := *data
	if data.Input != nil {
		input := *data.Input
		input.Transcript = cloneMessages(data.Input.Transcript)
		cloned.Input = &input
	}
	if data.Output != nil {
		output := *data.Output
		output.Summary = cloneUserMessage(data.Output.Summary)
		cloned.Output = &output
	}
	return &cloned
}

func cloneToolCallData(data *ToolCallData) *ToolCallData {
	if data == nil {
		return nil
	}
	cloned := *data
	cloned.ArgsJSON = cloneRawMessage(data.ArgsJSON)
	cloned.Result = cloneToolResult(data.Result)
	return &cloned
}

func cloneGateWaitData(data *GateWaitData) *GateWaitData {
	if data == nil {
		return nil
	}
	cloned := *data
	cloned.Answer = cloneGateAnswer(data.Answer)
	return &cloned
}

func cloneToolExecutionData(data *ToolExecutionData) *ToolExecutionData {
	if data == nil {
		return nil
	}
	cloned := *data
	cloned.ArgsJSON = cloneRawMessage(data.ArgsJSON)
	cloned.Result = cloneToolResult(data.Result)
	return &cloned
}

func cloneJournalAppendData(data *JournalAppendData) *JournalAppendData {
	if data == nil {
		return nil
	}
	cloned := *data
	return &cloned
}

func cloneInferenceRequest(request *inference.Request) *inference.Request {
	if request == nil {
		return nil
	}
	cloned := *request
	cloned.Model = request.Model
	cloned.Model.Sampling = cloneSampling(request.Model.Sampling)
	cloned.Messages = cloneMessages(request.Messages)
	cloned.Tools = cloneInferenceTools(request.Tools)
	if request.Output != nil {
		output := request.Output.Clone()
		cloned.Output = &output
	}
	if request.Override != nil {
		override := cloneSampling(*request.Override)
		cloned.Override = &override
	}
	return &cloned
}

func cloneSampling(sampling model.Sampling) model.Sampling {
	cloned := sampling
	if sampling.Temperature != nil {
		value := *sampling.Temperature
		cloned.Temperature = &value
	}
	if sampling.TopP != nil {
		value := *sampling.TopP
		cloned.TopP = &value
	}
	if sampling.MaxTokens != nil {
		value := *sampling.MaxTokens
		cloned.MaxTokens = &value
	}
	if sampling.Stop != nil {
		cloned.Stop = make([]string, len(sampling.Stop))
		copy(cloned.Stop, sampling.Stop)
	}
	return cloned
}

func cloneInferenceTools(tools []inference.Tool) []inference.Tool {
	if tools == nil {
		return nil
	}
	cloned := make([]inference.Tool, len(tools))
	for index := range tools {
		cloned[index] = tools[index]
		cloned[index].Schema = cloneRawMessage(tools[index].Schema)
	}
	return cloned
}

func cloneStreamResult(result *stream.StreamResult) *stream.StreamResult {
	if result == nil {
		return nil
	}
	cloned := *result
	cloned.Usage = cloneUsage(result.Usage)
	return &cloned
}

func cloneGateAnswer(answer *gate.Answer) *gate.Answer {
	if answer == nil {
		return nil
	}
	cloned := *answer
	if answer.Values != nil {
		cloned.Values = make(map[string]string, len(answer.Values))
		for key, value := range answer.Values {
			cloned.Values[key] = value
		}
	}
	return &cloned
}

func cloneToolResult(result *tool.ToolResult) *tool.ToolResult {
	if result == nil {
		return nil
	}
	return &tool.ToolResult{Content: cloneBlocks(result.Content)}
}

func cloneMessages(messages content.AgenticMessages) content.AgenticMessages {
	if messages == nil {
		return nil
	}
	cloned := make(content.AgenticMessages, len(messages))
	for index, message := range messages {
		cloned[index] = cloneConversation(message)
	}
	return cloned
}

func cloneConversation(message content.Conversation) content.Conversation {
	switch typed := message.(type) {
	case *content.UserMessage:
		return cloneUserMessage(typed)
	case *content.AIMessage:
		return cloneAIMessage(typed)
	case *content.SystemMessage:
		return cloneSystemMessage(typed)
	case *content.ToolResultMessage:
		return cloneToolResultMessage(typed)
	default:
		// The content union is sealed and exhaustive today. Fail visibly if an
		// upstream release adds a variant before this ownership boundary does.
		panic(&CloneError{
			Kind: CloneUnknownConversation, ValueType: fmt.Sprintf("%T", message),
		})
	}
}

func cloneUserMessage(message *content.UserMessage) *content.UserMessage {
	if message == nil {
		return nil
	}
	return &content.UserMessage{Message: cloneMessage(message.Message)}
}

func cloneAIMessage(message *content.AIMessage) *content.AIMessage {
	if message == nil {
		return nil
	}
	return &content.AIMessage{
		Message: cloneMessage(message.Message),
		Usage:   cloneUsage(message.Usage),
	}
}

func cloneSystemMessage(message *content.SystemMessage) *content.SystemMessage {
	if message == nil {
		return nil
	}
	return &content.SystemMessage{Message: cloneMessage(message.Message)}
}

func cloneToolResultMessage(message *content.ToolResultMessage) *content.ToolResultMessage {
	if message == nil {
		return nil
	}
	return &content.ToolResultMessage{
		Message:   cloneMessage(message.Message),
		ToolUseID: message.ToolUseID,
		IsError:   message.IsError,
	}
}

func cloneMessage(message content.Message) content.Message {
	return content.Message{Role: message.Role, Blocks: cloneBlocks(message.Blocks)}
}

// cloneBlocks walks the slice itself only so each element can carry this
// package's escalation policy; the copy is content.CloneBlock's.
//
// The nil-versus-empty requirement this package used to satisfy by hand — build
// through core's constructor, then restore the raw provider-state pair verbatim
// because the constructor NORMALIZES a half-set or empty-but-non-nil pair away
// — is now the documented guarantee of content.CloneBlock itself: a clone is a
// copy, not a normalization. The hand-restore is gone because the behaviour it
// produced is the behaviour content promises. Nothing was dropped; the
// requirement was promoted to the type's owner, where the other four clones in
// the workspace get it too.
func cloneBlocks(blocks []content.Block) []content.Block {
	if blocks == nil {
		return nil
	}
	cloned := make([]content.Block, len(blocks))
	for index, block := range blocks {
		cloned[index] = cloneBlock(block)
	}
	return cloned
}

// cloneBlock copies one block and keeps this package's fail-secure policy: a
// value the sealed union does not cover panics with *CloneError rather than
// becoming a silent nil, which would erase content from the payload a hook
// inspects. content.CloneBlock returns a nil interface only for a nil block —
// a typed-nil payload comes back as the same typed nil, in a non-nil interface
// — so that is the exact condition to escalate.
func cloneBlock(block content.Block) content.Block {
	cloned := content.CloneBlock(block)
	if cloned == nil {
		panic(&CloneError{Kind: CloneUnknownBlock, ValueType: fmt.Sprintf("%T", block)})
	}
	return cloned
}

func cloneUsage(usage *content.Usage) *content.Usage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	return &cloned
}

func cloneRawMessage(value json.RawMessage) json.RawMessage {
	if value == nil {
		return nil
	}
	cloned := make(json.RawMessage, len(value))
	copy(cloned, value)
	return cloned
}
