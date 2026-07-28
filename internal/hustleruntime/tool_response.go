package hustleruntime

import (
	"bytes"
	"encoding/json"
	"io"
	"math"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

// classifiedToolResponse is a sealed internal union. A successful
// classification is exactly one terminal result or one non-empty ordered call
// list.
type classifiedToolResponse interface {
	isClassifiedToolResponse()
}

type terminalToolResponse struct {
	output json.RawMessage
}

func (terminalToolResponse) isClassifiedToolResponse() {}

type evidenceToolCall struct {
	id    string
	name  string
	input json.RawMessage
}

type evidenceToolResponse struct {
	calls []evidenceToolCall
}

func (evidenceToolResponse) isClassifiedToolResponse() {}

// classifyToolResponse exhaustively classifies one complete provider response.
// knownTools is the exact evidence catalog exposed for the invocation.
func classifyToolResponse(
	response *inference.Response,
	knownTools map[string]struct{},
	outputLimit int,
) (classifiedToolResponse, error) {
	if response == nil || response.Message == nil || response.Message.Role != content.RoleAssistant {
		return nil, toolResponseError(ToolResponseFailureInvalidShape)
	}

	shape, reason := inspectToolResponseBlocks(response.Message.Blocks)
	if reason.Valid() {
		return nil, toolResponseError(reason)
	}
	if reason := validateToolResponseFinish(response.FinishReason, shape); reason.Valid() {
		return nil, toolResponseError(reason)
	}

	if shape.textSeen || shape.terminalCount == 1 {
		rawBytes, overflow := terminalResponseBytes(shape)
		if outputLimit <= 0 || overflow || rawBytes > outputLimit {
			return nil, toolResponseError(ToolResponseFailureTooLarge)
		}
		output, err := inference.StructuredMessageResult(response.Message)
		if err != nil {
			return nil, toolResponseError(ToolResponseFailureInvalidTerminal)
		}
		return terminalToolResponse{output: append(json.RawMessage(nil), output...)}, nil
	}

	calls, reason := classifyEvidenceCalls(shape.ordinary, knownTools)
	if reason.Valid() {
		return nil, toolResponseError(reason)
	}
	return evidenceToolResponse{calls: calls}, nil
}

type inspectedToolResponse struct {
	textSeen      bool
	textBytes     int
	textOverflow  bool
	terminal      *content.ToolUseBlock
	terminalCount int
	ordinary      []*content.ToolUseBlock
}

func inspectToolResponseBlocks(blocks []content.Block) (inspectedToolResponse, ToolResponseFailureReason) {
	var shape inspectedToolResponse
	for _, block := range blocks {
		switch typed := block.(type) {
		case *content.TextBlock:
			if typed == nil {
				return inspectedToolResponse{}, ToolResponseFailureInvalidShape
			}
			shape.textSeen = true
			if len(typed.Text) > math.MaxInt-shape.textBytes {
				shape.textOverflow = true
			} else {
				shape.textBytes += len(typed.Text)
			}
		case *content.ThinkingBlock:
			if typed == nil {
				return inspectedToolResponse{}, ToolResponseFailureInvalidShape
			}
		case *content.ToolUseBlock:
			if typed == nil {
				return inspectedToolResponse{}, ToolResponseFailureInvalidShape
			}
			if typed.Name == inference.StructuredOutputToolName {
				shape.terminalCount++
				shape.terminal = typed
			} else {
				shape.ordinary = append(shape.ordinary, typed)
			}
		case *content.ImageBlock:
			return inspectedToolResponse{}, ToolResponseFailureInvalidShape
		case *content.AudioBlock:
			return inspectedToolResponse{}, ToolResponseFailureInvalidShape
		case *content.DocumentBlock:
			return inspectedToolResponse{}, ToolResponseFailureInvalidShape
		case *content.ToolResultBlock:
			return inspectedToolResponse{}, ToolResponseFailureInvalidShape
		default:
			return inspectedToolResponse{}, ToolResponseFailureInvalidShape
		}
	}

	if shape.terminalCount > 1 {
		return inspectedToolResponse{}, ToolResponseFailureDuplicateTerminal
	}
	if shape.textSeen && (shape.terminalCount > 0 || len(shape.ordinary) > 0) ||
		shape.terminalCount > 0 && len(shape.ordinary) > 0 {
		return inspectedToolResponse{}, ToolResponseFailureMixed
	}
	if !shape.textSeen && shape.terminalCount == 0 && len(shape.ordinary) == 0 {
		return inspectedToolResponse{}, ToolResponseFailureInvalidShape
	}
	return shape, ""
}

func validateToolResponseFinish(
	finish stream.FinishReason,
	shape inspectedToolResponse,
) ToolResponseFailureReason {
	switch finish {
	case stream.FinishReasonUnknown:
		return ""
	case stream.FinishReasonStop:
		if shape.textSeen {
			return ""
		}
	case stream.FinishReasonToolUse:
		if !shape.textSeen && (shape.terminalCount == 1 || len(shape.ordinary) > 0) {
			return ""
		}
	case stream.FinishReasonLength, stream.FinishReasonContentFilter:
	default:
	}
	return ToolResponseFailureFinishReason
}

func terminalResponseBytes(shape inspectedToolResponse) (int, bool) {
	if shape.textSeen {
		return shape.textBytes, shape.textOverflow
	}
	return len(shape.terminal.Input), false
}

func classifyEvidenceCalls(
	blocks []*content.ToolUseBlock,
	knownTools map[string]struct{},
) ([]evidenceToolCall, ToolResponseFailureReason) {
	seenIDs := make(map[string]struct{}, len(blocks))
	calls := make([]evidenceToolCall, 0, len(blocks))
	for _, block := range blocks {
		if block.ID == "" {
			return nil, ToolResponseFailureMissingCallID
		}
		if _, duplicate := seenIDs[block.ID]; duplicate {
			return nil, ToolResponseFailureDuplicateCallID
		}
		seenIDs[block.ID] = struct{}{}
		if _, known := knownTools[block.Name]; !known {
			return nil, ToolResponseFailureUnknownTool
		}
		if !validEvidenceArguments(block.Input) {
			return nil, ToolResponseFailureMalformedArguments
		}
		calls = append(calls, evidenceToolCall{
			id:    block.ID,
			name:  block.Name,
			input: append(json.RawMessage(nil), block.Input...),
		})
	}
	return calls, ""
}

func validEvidenceArguments(input json.RawMessage) bool {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') || !consumeJSONObject(decoder) {
		return false
	}
	_, err = decoder.Token()
	return err == io.EOF
}

func consumeJSONObject(decoder *json.Decoder) bool {
	members := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return false
		}
		if _, duplicate := members[name]; duplicate {
			return false
		}
		members[name] = struct{}{}
		if !consumeJSONValue(decoder) {
			return false
		}
	}
	token, err := decoder.Token()
	return err == nil && token == json.Delim('}')
}

func consumeJSONArray(decoder *json.Decoder) bool {
	for decoder.More() {
		if !consumeJSONValue(decoder) {
			return false
		}
	}
	token, err := decoder.Token()
	return err == nil && token == json.Delim(']')
}

func consumeJSONValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		return consumeJSONObject(decoder)
	case json.Delim('['):
		return consumeJSONArray(decoder)
	default:
		return true
	}
}

func toolResponseError(reason ToolResponseFailureReason) *ToolResponseError {
	return &ToolResponseError{Reason: reason}
}
