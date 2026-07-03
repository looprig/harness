package bedrock

import "github.com/looprig/harness/pkg/llm/auth"

// NewWithEndpoint is a test-only constructor that overrides the region-derived
// Bedrock Runtime endpoint so Invoke can be exercised against an httptest.Server.
// It reuses the exact production wiring (signer + http.Client) via newClient; only
// the endpoint base differs.
func NewWithEndpoint(creds auth.SigV4Credentials, region, endpoint string) *Client {
	return newClient(creds, region, endpoint)
}
