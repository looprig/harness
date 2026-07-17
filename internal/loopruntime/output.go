package loopruntime

import (
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

type outputStrategy uint8

const (
	outputStrategyNone outputStrategy = iota
	outputStrategyNative
	outputStrategyTerminalTool
)

// turnOutputPlan is the immutable request shape selected once at turn start.
// apply returns fresh schema/tool storage so neither provider clients nor
// measurement hooks can mutate a later continuation request.
type turnOutputPlan struct {
	strategy   outputStrategy
	output     *inference.OutputSchema
	tools      []inference.Tool
	toolChoice inference.ToolChoice
}

func resolveTurnOutput(current model.Model, configured *inference.OutputSchema, ordinary []inference.Tool) (turnOutputPlan, error) {
	plan := turnOutputPlan{strategy: outputStrategyNone, tools: cloneInferenceTools(ordinary)}
	if configured == nil {
		return plan, nil
	}
	if err := current.Validate(); err != nil {
		return turnOutputPlan{}, err
	}
	if err := inference.ValidateOutputSchema(*configured); err != nil {
		return turnOutputPlan{}, err
	}
	if err := validateOrdinaryOutputTools(ordinary); err != nil {
		return turnOutputPlan{}, err
	}

	output := configured.Clone()
	if current.Caps.StructuredOutput && (len(ordinary) == 0 || current.Caps.StructuredOutputWithTools) {
		plan.strategy = outputStrategyNative
		plan.output = &output
		return plan, nil
	}
	if !current.Caps.Tools {
		return turnOutputPlan{}, &inference.StructuredOutputUnsupportedError{Model: structuredOutputModelDiagnostic(current.Name)}
	}

	plan.strategy = outputStrategyTerminalTool
	plan.toolChoice = inference.ToolChoiceRequired
	plan.tools = append(plan.tools, inference.Tool{
		Name:        inference.StructuredOutputToolName,
		Description: output.Description,
		Schema:      append([]byte(nil), output.Schema...),
	})
	return plan, nil
}

func structuredOutputModelDiagnostic(name string) string {
	if !utf8.ValidString(name) {
		return "invalid-utf8"
	}
	if len(name) <= inference.MaxStructuredOutputDiagnosticBytes {
		return name
	}
	end := inference.MaxStructuredOutputDiagnosticBytes
	for end > 0 && !utf8.RuneStart(name[end]) {
		end--
	}
	return name[:end]
}

func validateOrdinaryOutputTools(tools []inference.Tool) error {
	seen := make(map[string]struct{}, len(tools))
	for _, definition := range tools {
		if definition.Name == inference.StructuredOutputToolName {
			return &inference.StructuredOutputConflictError{Feature: "reserved_structured_output_tool"}
		}
		if _, exists := seen[definition.Name]; exists {
			return &inference.StructuredOutputConflictError{Feature: "duplicate_tool_name"}
		}
		seen[definition.Name] = struct{}{}
	}
	return nil
}

func (p turnOutputPlan) apply(request inference.Request) inference.Request {
	request.Tools = cloneInferenceTools(p.tools)
	request.ToolChoice = p.toolChoice
	request.Output = cloneOutputSchema(p.output)
	return request
}

func cloneOutputSchema(output *inference.OutputSchema) *inference.OutputSchema {
	if output == nil {
		return nil
	}
	clone := output.Clone()
	return &clone
}

func cloneInferenceTools(tools []inference.Tool) []inference.Tool {
	if tools == nil {
		return nil
	}
	clone := make([]inference.Tool, len(tools))
	for i := range tools {
		clone[i] = tools[i]
		clone[i].Schema = append([]byte(nil), tools[i].Schema...)
	}
	return clone
}

// validateNativeStep applies the native-output finish contract before the turn
// actor observes any part of the current step. A complete ordinary tool batch
// is a continuation; a no-tool response must be a valid text representation and
// is canonicalized to one compact JSON TextBlock. Reserved terminal-tool frames
// belong only to the fallback strategy and fail closed here.
func validateNativeStep(
	message *content.AIMessage,
	calls []content.ToolUseBlock,
	result *stream.StreamResult,
) (*content.AIMessage, bool, error) {
	if result == nil {
		return nil, false, &inference.StructuredOutputFinishError{Reason: inference.StructuredOutputFinishReasonOther}
	}

	response := &inference.Response{Message: message, FinishReason: result.FinishReason}
	if len(calls) > 0 {
		if result.FinishReason != stream.FinishReasonToolUse {
			_, err := inference.StructuredResult(response)
			if err != nil {
				return nil, false, err
			}
			return nil, false, &inference.StructuredOutputFinishError{Reason: inference.StructuredOutputFinishReasonOther}
		}
		for _, call := range calls {
			if call.Name == inference.StructuredOutputToolName {
				return nil, false, &inference.StructuredOutputConflictError{Feature: "native_terminal_tool"}
			}
			if !validToolCall(call) {
				return nil, false, &inference.StructuredOutputConflictError{Feature: "incomplete_tool_call"}
			}
		}
		return nil, false, nil
	}

	raw, err := inference.StructuredResult(response)
	if err != nil {
		return nil, false, err
	}
	canonical := &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: string(raw)}},
	}}
	canonical.Usage = cloneUsage(message.Usage)
	return canonical, true, nil
}
