package geminiapi

import (
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	failure "github.com/looprig/inference/failure"
	"github.com/looprig/inference/internal/usagenorm"
	usage "github.com/looprig/inference/usage"
)

// DecodeResponse parses a Gemini generateContent JSON response body into a
// provider-neutral *inference.Response. It reads candidates[0]; a body with no
// candidates is a *failure.APIError (matching the sibling OpenAI codec), and
// malformed JSON is a *DecodeError.
func DecodeResponse(body []byte) (*inference.Response, error) {
	var wire GenerateContentResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, &DecodeError{Reason: "unmarshal response body", Err: err}
	}

	if len(wire.Candidates) == 0 {
		return nil, &failure.APIError{Status: 0, Message: "response contains no candidates", Body: body}
	}

	blocks := buildBlocks(wire.Candidates[0].Content.Parts)

	usage, err := normalizeUsage(wire.UsageMetadata)
	if err != nil {
		return nil, err
	}
	var messageUsage *content.Usage
	if usage != nil {
		cloned := *usage
		messageUsage = &cloned
	}

	return &inference.Response{
		Message: &content.AIMessage{
			Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: blocks,
			},
			Usage: messageUsage,
		},
		Model: wire.ModelVersion,
		Usage: usage,
	}, nil
}

func normalizeUsage(wire *usageMetadata) (*usage.Usage, error) {
	if wire == nil {
		return nil, nil
	}
	input, cacheRead, err := normalizeInputUsage(*wire)
	if err != nil {
		return nil, err
	}
	output, reasoning, err := normalizeOutputUsage(*wire)
	if err != nil {
		return nil, err
	}
	if err := validateTotalUsage(*wire); err != nil {
		return nil, err
	}
	usage := usage.Usage{InputTokens: input, OutputTokens: output, CacheReadTokens: cacheRead, ReasoningTokens: reasoning}
	if err := usagenorm.ValidateUsage(usage); err != nil {
		return nil, err
	}
	return &usage, nil
}

func normalizeInputUsage(wire usageMetadata) (content.TokenCount, content.TokenCount, error) {
	prompt, err := wire.PromptTokenCount.TokenCount(usagenorm.FieldInputTokens)
	if err != nil {
		return 0, 0, err
	}
	cacheRead, err := wire.CachedContentTokenCount.TokenCount(usagenorm.FieldCacheReadTokens)
	if err != nil {
		return 0, 0, err
	}
	input, err := usagenorm.SubtractTokenCounts(usagenorm.FieldInputTokens, prompt, cacheRead, 0)
	if err != nil {
		return 0, 0, err
	}
	return input, cacheRead, nil
}

func normalizeOutputUsage(wire usageMetadata) (content.TokenCount, content.TokenCount, error) {
	candidates, err := wire.CandidatesTokenCount.TokenCount(usagenorm.FieldOutputTokens)
	if err != nil {
		return 0, 0, err
	}
	reasoning, err := wire.ThoughtsTokenCount.TokenCount(usagenorm.FieldReasoningTokens)
	if err != nil {
		return 0, 0, err
	}
	output, err := usagenorm.AddTokenCounts(usagenorm.FieldOutputTokens, candidates, reasoning)
	return output, reasoning, err
}

func validateTotalUsage(wire usageMetadata) error {
	if !wire.TotalTokenCount.Present() {
		return nil
	}
	reported, err := wire.TotalTokenCount.TokenCount(usagenorm.FieldTotalTokens)
	if err != nil {
		return err
	}
	calculated, err := totalComponents(wire)
	if err != nil {
		return err
	}
	return usagenorm.RequireEqual(usagenorm.FieldTotalTokens, reported, calculated)
}

func totalComponents(wire usageMetadata) (content.TokenCount, error) {
	prompt, err := wire.PromptTokenCount.TokenCount(usagenorm.FieldInputTokens)
	if err != nil {
		return 0, err
	}
	candidates, err := wire.CandidatesTokenCount.TokenCount(usagenorm.FieldOutputTokens)
	if err != nil {
		return 0, err
	}
	thoughts, err := wire.ThoughtsTokenCount.TokenCount(usagenorm.FieldReasoningTokens)
	if err != nil {
		return 0, err
	}
	calculated, err := usagenorm.AddTokenCounts(usagenorm.FieldTotalTokens, prompt, candidates)
	if err != nil {
		return 0, err
	}
	calculated, err = usagenorm.AddTokenCounts(usagenorm.FieldTotalTokens, calculated, thoughts)
	return calculated, err
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
