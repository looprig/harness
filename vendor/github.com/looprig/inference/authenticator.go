package inference

import (
	"context"
	"net/http"
)

// Authenticator mutates an outbound request to carry credentials. Orthogonal to dialect.
// Generic implementations (Key/Header/None) live in inference/auth; provider-specific
// authenticators (e.g. request-signing schemes) live in the llm module.
type Authenticator interface {
	Authorize(ctx context.Context, r *http.Request) error
}
