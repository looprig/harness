package chutes

import "context"

// withAttestFn overrides the attestation step for tests. Real attestation binds
// to a live TEE quote and cannot succeed against a test-generated ML-KEM key, so
// hermetic tests substitute a no-op (or forced-failure) attestFn. This option is
// test-only and intentionally lives in export_test.go so it is not part of the
// package's public surface.
func withAttestFn(fn func(ctx context.Context, inst instance, chuteID string) error) Option {
	return func(c *Client) { c.attestFn = fn }
}

// withStreamDone installs a hook the streaming reader goroutine calls exactly
// once just before it returns. Tests use it to prove the goroutine exits (no
// leak) on EOF, error, or context cancellation. Test-only.
func withStreamDone(fn func()) Option {
	return func(c *Client) { c.streamDone = fn }
}
