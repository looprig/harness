package inference

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/inference/model"

	"github.com/looprig/inference/stream"
)

// Client is the provider-neutral inference interface.
type Client interface {
	Invoke(ctx context.Context, req Request) (*Response, error)
	Stream(ctx context.Context, req Request) (*stream.StreamReader[content.Chunk], error)
}

// Request is the provider-neutral inference request. It carries a secret-free
// Model descriptor for this turn, the per-agent System prompt, the message
// thread, the exposed tools, and an optional per-call sampling Override
// (nil means use Model.Sampling).
type Request struct {
	Model    model.Model
	System   string
	Messages content.AgenticMessages
	Tools    []Tool
	Override *model.Sampling
}

// Response is the complete provider-neutral response.
type Response struct {
	Message *content.AIMessage
	Usage   *content.Usage
	Model   string
}

// Tool is a callable function definition exposed to the model.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}
