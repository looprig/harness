package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

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

// Provider names the concrete backend an internal/llm/auto factory dispatches on.
// Unknown values are rejected by auto.New, not Validate.
type Provider string

const (
	ProviderLMStudio Provider = "lmstudio"
	ProviderPhala    Provider = "phala"
	ProviderChutes   Provider = "chutes"
)

// ModelSpec identifies a model and its sampling configuration.
// Call Validate before encoding to catch self-contradictory combinations.
type ModelSpec struct {
	Provider Provider
	BaseURL  string
	APIKey   string

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

// redactKey returns "" for an empty key and "[REDACTED]" for any non-empty key,
// so the raw APIKey never reaches a log line or formatted string.
func redactKey(k string) string {
	if k == "" {
		return ""
	}
	return "[REDACTED]"
}

// LogValue implements slog.LogValuer so structured logging emits a compact,
// secret-free summary of the spec. The APIKey is never logged in the clear.
func (s ModelSpec) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("provider", string(s.Provider)),
		slog.String("baseURL", s.BaseURL),
		slog.String("model", s.Model),
		slog.String("apiKey", redactKey(s.APIKey)),
	)
}

// String implements fmt.Stringer so %v, %+v, and %s never expose the APIKey.
func (s ModelSpec) String() string {
	return fmt.Sprintf("ModelSpec{Provider:%s BaseURL:%s APIKey:%s Model:%s}",
		s.Provider, s.BaseURL, redactKey(s.APIKey), s.Model)
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
