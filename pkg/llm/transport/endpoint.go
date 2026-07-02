// Package transport is a generic, connection-bound HTTP client for the llm seam.
// It composes three orthogonal collaborators — an llm.Codec (wire dialect), an
// Endpoint (connection identity + routing), and an llm.Authenticator (credentials)
// — into an llm.LLM. It knows HTTP + SSE framing only; it depends on the Codec
// interface and never imports a concrete codec, so the composition root (auto)
// selects the dialect. It de-frames streaming responses with the shared codec/sse
// helper and hands the codec one event payload at a time.
package transport

import "github.com/ciram-co/looprig/pkg/llm"

// DefaultChatPath is the OpenAI-compatible chat completions request path, used
// when an Endpoint leaves ChatPath empty.
const DefaultChatPath = "/chat/completions"

// Endpoint is the connection a Client is bound to: the identity a request's Model
// must match (fail-closed via ModelMismatchError) plus the request routing.
//   - Provider and BaseURL together form the binding identity checked before any I/O.
//   - ChatPath is the dialect-specific request path appended to BaseURL; it is
//     injected because it varies by dialect, defaulting to DefaultChatPath.
//
// It is deliberately secret-free — credentials live on the Authenticator, never here.
type Endpoint struct {
	Provider llm.Provider
	BaseURL  string
	ChatPath string
}
