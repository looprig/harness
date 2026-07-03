// pkg/llm/codec/openaiapi/decode.go
package openaiapi

import (
	"encoding/json"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/llm"
)

// DecodeResponse parses an OpenAI chat completions JSON response body into
// a provider-neutral *llm.Response.
func DecodeResponse(body []byte) (*llm.Response, error) {
	var wire chatResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}

	if len(wire.Choices) == 0 {
		return nil, &llm.APIError{Status: 0, Message: "response contains no choices", Body: body}
	}

	msg := wire.Choices[0].Message
	blocks := buildBlocks(msg)

	var usage *llm.Usage
	if wire.Usage != nil {
		usage = &llm.Usage{
			InputTokens:  wire.Usage.PromptTokens,
			OutputTokens: wire.Usage.CompletionTokens,
		}
	}

	return &llm.Response{
		Message: &content.AIMessage{
			Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: blocks,
			},
		},
		Model: wire.Model,
		Usage: usage,
	}, nil
}

// buildBlocks constructs an ordered slice of content blocks from a decoded
// chatMessage. Reasoning comes first, then text, then tool calls.
func buildBlocks(msg chatMessage) []content.Block {
	var blocks []content.Block

	if msg.ReasoningContent != "" {
		blocks = append(blocks, &content.ThinkingBlock{Thinking: msg.ReasoningContent})
	}

	if s, ok := msg.Content.(string); ok && s != "" {
		blocks = append(blocks, &content.TextBlock{Text: s})
	}

	for _, tc := range msg.ToolCalls {
		blocks = append(blocks, &content.ToolUseBlock{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: tc.Function.Arguments,
		})
	}

	return blocks
}
