package llm

import (
	"context"
	"encoding/json"

	"github.com/ciram-co/looprig/pkg/content"
)

// LLM is the provider-neutral inference interface.
type LLM interface {
	Invoke(ctx context.Context, req Request) (*Response, error)
	Stream(ctx context.Context, req Request) (*StreamReader[content.Chunk], error)
}

// Provider names the concrete backend an internal/llm/auto factory dispatches on.
// Unknown values are rejected by Model.Validate; auto.New additionally enforces
// each provider's auth requirement.
type Provider string

const (
	ProviderLMStudio   Provider = "lmstudio"
	ProviderPhala      Provider = "phala"
	ProviderChutes     Provider = "chutes"
	ProviderOpenRouter Provider = "openrouter"
	ProviderBedrock    Provider = "bedrock"
	// ProviderGoogle is Google's Gemini generateContent backend. The provider (the
	// backend "google") and the dialect (APIFormatGemini) are distinct axes: google
	// speaks only the Gemini wire format, authenticated with an x-goog-api-key header
	// (RequiredAuth → AuthAPIKey), and is served by the bespoke providers/gemini
	// client (the generic transport assumes a static /chat/completions path).
	ProviderGoogle Provider = "google"
)

// Request is the provider-neutral inference request. It carries a secret-free
// Model descriptor for this turn, the per-agent System prompt, the message
// thread, the exposed tools, and an optional per-call sampling Override
// (nil means use Model.Sampling).
type Request struct {
	Model    Model
	System   string
	Messages content.AgenticMessages
	Tools    []Tool
	Override *Sampling
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
