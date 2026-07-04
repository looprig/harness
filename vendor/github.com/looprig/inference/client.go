package inference

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/content"
)

// Client is the provider-neutral inference interface.
type Client interface {
	Invoke(ctx context.Context, req Request) (*Response, error)
	Stream(ctx context.Context, req Request) (*StreamReader[content.Chunk], error)
}

// ProviderName is an opaque label identifying the backend a model/endpoint belongs to.
// It carries no policy: inference does not define provider constants, provider auth
// requirements, allowed wire formats, or default endpoints. Those belong to the llm module
// or a consumer composition root. An empty ProviderName is a wildcard, not a claim.
type ProviderName string

// Endpoint is explicit client-binding metadata: the base URL the client is bound to plus
// optional opaque provider/API-format labels. It carries no chat path — route shape belongs
// to the injected Router. Empty label fields are wildcards, not claims.
type Endpoint struct {
	BaseURL   string
	Provider  ProviderName
	APIFormat APIFormat
}

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
