package serve

import (
	"net/http"
	"time"
)

// defaultMaxBodyBytes is the request-body cap applied when WithMaxBodyBytes is not
// given (or is given a non-positive value): 1 MiB. Session-control request bodies
// (input blocks, gate responses) are small, so a 1 MiB ceiling bounds per-request
// memory against an oversized-body DoS while comfortably admitting legitimate
// payloads. http.MaxBytesReader enforces the cap lazily, on read (see
// bodyCapMiddleware).
const defaultMaxBodyBytes int64 = 1 << 20

// defaultHeartbeatInterval is the SSE keep-alive cadence applied when the config is
// built with the secure default: every 20s an idle events stream emits a `: ping`
// comment so the connection (and any intermediary idle timeout) stays alive. Tests
// override the (unexported) config.heartbeat field directly with a few-millisecond
// value for deterministic, sleep-free heartbeat assertions.
const defaultHeartbeatInterval = 20 * time.Second

// config is serve's assembled server configuration: an optional request
// authenticator and the request-body cap. It is built once from the secure
// defaults plus Options (newConfig) at the composition root and is read-only on
// the request path thereafter. A nil authn means no authentication (every request
// passes the auth seam); hasAuth exposes that fact to P1-10's fail-secure bind.
type config struct {
	authn        func(*http.Request) error
	maxBodyBytes int64
	// heartbeat is the SSE keep-alive interval for the events stream. It is set to
	// defaultHeartbeatInterval by newConfig and is not exposed via an Option (no
	// caller need has surfaced); tests set it directly for deterministic assertions.
	heartbeat time.Duration
}

// Option mutates a config during construction (the functional-options pattern).
// Every Option is fail-safe: an invalid argument leaves the corresponding secure
// default in place rather than weakening the configuration.
type Option func(*config)

// newConfig builds a config from the secure defaults (no authenticator installed,
// 1 MiB body cap) and applies opts in order. A nil Option in the list is skipped.
// The returned config is ready for wrap.
func newConfig(opts ...Option) *config {
	c := &config{maxBodyBytes: defaultMaxBodyBytes, heartbeat: defaultHeartbeatInterval}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// WithAuth installs a caller-supplied authenticator applied to every request: a
// non-nil return from authn rejects the request with 401 before the wrapped
// handler runs (fail secure — authenticate before act). serve never bakes in a
// scheme; the default is no auth, which a caller opts out of by supplying an
// authenticator (least privilege — the auth policy is the caller's, wired at the
// composition root). A nil authn is ignored (stays no-auth), per the fail-safe
// option convention.
func WithAuth(authn func(*http.Request) error) Option {
	return func(c *config) {
		if authn != nil {
			c.authn = authn
		}
	}
}

// WithMaxBodyBytes sets the per-request body cap, in bytes. A non-positive n is
// ignored (the default cap stays in place), per the fail-safe option convention:
// an option may tighten or reset the bound but never disable it.
func WithMaxBodyBytes(n int64) Option {
	return func(c *config) {
		if n > 0 {
			c.maxBodyBytes = n
		}
	}
}

// hasAuth reports whether an authenticator is installed. P1-10's fail-secure bind
// uses it to refuse binding a non-loopback address without authentication, without
// re-deriving the auth policy.
func (c *config) hasAuth() bool { return c.authn != nil }
