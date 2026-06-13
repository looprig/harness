package llm

import (
	"context"
	"encoding/json"

	"github.com/inventivepotter/urvi/internal/content"
)

// LLM is the provider-neutral inference interface.
type LLM interface {
	Invoke(ctx context.Context, req Request) (*Response, error)
	Stream(ctx context.Context, req Request) (*StreamReader[content.Chunk], error)
}

// ReasoningEffort selects o-series inference intensity. Zero value = disabled.
// Silently ignored by providers that do not support it.
type ReasoningEffort string

const (
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
)

// ModelSpec identifies a model and its sampling configuration.
// Call Validate before encoding to catch self-contradictory combinations.
type ModelSpec struct {
	Model  string
	System string

	Temperature *float64
	TopP        *float64
	MaxTokens   *int
	Stop        []string

	// ThinkingBudget enables extended thinking (budget_tokens).
	// When >0, Temperature must be exactly 1.0; Validate enforces this.
	ThinkingBudget int

	ReasoningEffort ReasoningEffort
}

// Validate returns an error if the spec contains self-contradictory values.
func (s ModelSpec) Validate() error {
	if s.ThinkingBudget > 0 {
		if s.Temperature == nil {
			return &ValidationError{Field: "ThinkingBudget", Reason: "requires Temperature to be set to exactly 1.0"}
		}
		if *s.Temperature != 1.0 {
			return &ValidationError{Field: "ThinkingBudget", Reason: "requires Temperature == 1.0"}
		}
	}
	switch s.ReasoningEffort {
	case "", ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh:
		// valid
	default:
		return &ValidationError{Field: "ReasoningEffort", Reason: "must be low, medium, or high"}
	}
	return nil
}

// Request is the provider-neutral inference request.
type Request struct {
	Model    ModelSpec
	Messages content.AgenticMessages
	Tools    []Tool
}

// Response is the complete provider-neutral response.
type Response struct {
	Message *content.AIMessage
	Usage   *Usage
	Model   string
}

// Tool is a callable function definition exposed to the model.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Usage reports token consumption for the request.
type Usage struct {
	InputTokens  int
	OutputTokens int
}
