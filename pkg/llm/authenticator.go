package llm

import (
	"context"
	"net/http"
)

// Authenticator mutates an outbound request to carry credentials. Orthogonal to dialect.
// Implementations live in pkg/llm/auth (Key/Header/None; SigV4 lands with Bedrock).
type Authenticator interface {
	Authorize(ctx context.Context, r *http.Request) error
}
