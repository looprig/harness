package tool

import "context"

// grants.go is the dependency-free carrier for escalation grant tokens on the
// per-call ctx. Grant tokens are OPAQUE strings the sandbox mints and verifies —
// harness NEVER inspects, mints, or validates them; it only carries them from the
// runner's pre-ask approval to the tool's execution. Living in pkg/tool (not
// pkg/loop) lets a tool like Bash read grants back WITHOUT importing loop (SPEC
// §9.3, §10.7). The write side (WithGrants) is called by the runner in a later
// slice; the read side (GrantsFromContext) is called by the tool now.

// grantsCtxKey is the unexported context-key type for the grant-token carrier. A
// distinct zero-size struct so the value never collides with another package's
// key and cannot be constructed from outside this package (the idiomatic Go
// ctx-key pattern).
type grantsCtxKey struct{}

// WithGrants returns a ctx carrying grant tokens the runner obtained from a
// pre-ask approval, to be merged with any grants in the tool's own args. The
// tokens are opaque and stored verbatim.
func WithGrants(ctx context.Context, grants []string) context.Context {
	return context.WithValue(ctx, grantsCtxKey{}, grants)
}

// GrantsFromContext returns grant tokens placed on ctx by the runner, or nil when
// none are present (including the background ctx). The returned slice is the one
// stored by WithGrants — callers must not mutate it.
func GrantsFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(grantsCtxKey{}).([]string)
	return v
}
