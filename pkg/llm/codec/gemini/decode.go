// pkg/llm/codec/gemini/decode.go
package gemini

import (
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/llm"
)

// DecodeResponse parses a Gemini generateContent JSON response body into a
// provider-neutral *llm.Response. It reads candidates[0]; a body with no
// candidates is an *llm.APIError (matching the sibling OpenAI codec), and
// malformed JSON is a *DecodeError.
func DecodeResponse(body []byte) (*llm.Response, error) {
	var wire GenerateContentResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, &DecodeError{Reason: "unmarshal response body", Err: err}
	}

	if len(wire.Candidates) == 0 {
		return nil, &llm.APIError{Status: 0, Message: "response contains no candidates", Body: body}
	}

	blocks := buildBlocks(wire.Candidates[0].Content.Parts)

	var usage *llm.Usage
	if wire.UsageMetadata != nil {
		usage = &llm.Usage{
			InputTokens:  wire.UsageMetadata.PromptTokenCount,
			OutputTokens: wire.UsageMetadata.CandidatesTokenCount,
		}
	}

	return &llm.Response{
		Message: &content.AIMessage{
			Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: blocks,
			},
		},
		Model: wire.ModelVersion,
		Usage: usage,
	}, nil
}

// buildBlocks maps candidate parts to content blocks, preserving Gemini's part
// order (which interleaves text, thoughts, and tool calls). A functionCall part
// becomes a ToolUseBlock; a thought-tagged text part becomes a ThinkingBlock; any
// other non-empty text part becomes a TextBlock. Empty text and unknown parts are
// skipped.
func buildBlocks(parts []geminiPart) []content.Block {
	var blocks []content.Block
	for _, p := range parts {
		switch {
		case p.FunctionCall != nil:
			blocks = append(blocks, &content.ToolUseBlock{
				ID:    p.FunctionCall.ID,
				Name:  p.FunctionCall.Name,
				Input: argsJSON(p.FunctionCall.Args),
			})
		case p.Thought && p.Text != "":
			blocks = append(blocks, &content.ThinkingBlock{Thinking: p.Text})
		case p.Text != "":
			blocks = append(blocks, &content.TextBlock{Text: p.Text})
		}
	}
	return blocks
}
