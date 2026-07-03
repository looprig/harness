package gemini

import "github.com/looprig/harness/pkg/llm/auth"

// NewWithEndpoint is a test-only constructor that overrides the Gemini endpoint
// base so Invoke and Stream can be exercised against an httptest.Server. It reuses
// the exact production wiring (authenticator + http.Client) via newClient; only the
// endpoint base differs.
func NewWithEndpoint(key auth.APIKey, endpoint string) *Client {
	return newClient(key, endpoint)
}
