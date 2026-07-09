package serve

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// Server timeout and header-size defaults (mirroring pkg/api/server.go and
// flow/pkg/ingress). They are the slowloris and resource-exhaustion guards the
// CLAUDE.md HTTP-server rule mandates:
//
//   - readTimeout       — whole-request read budget (headers + body). An SSE GET
//     is read to completion well within it (the long-lived part is the WRITE),
//     so it is safe for the events stream while still bounding a stalled reader.
//   - readHeaderTimeout — header read budget, the primary slowloris guard.
//   - idleTimeout       — keep-alive idle budget between requests.
//   - maxHeaderBytes    — 1 MiB header cap (request bodies are capped separately
//     by the body-cap middleware).
const (
	readTimeout       = 5 * time.Second
	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 60 * time.Second
	maxHeaderBytes    = 1 << 20
)

// serverConfig is the assembled bind configuration: it holds only the fail-secure
// escape hatch. It is built from ServerOptions at the call to Server and read
// once during the bind decision.
type serverConfig struct {
	allowInsecurePublic bool
}

// ServerOption mutates a serverConfig during Server construction (the functional-
// options pattern). Every option is fail-safe: it can only relax the bind guard
// via an explicit opt-in, never silently.
type ServerOption func(*serverConfig)

// WithInsecurePublicBind opts INTO binding a public (non-loopback) address with
// no authentication installed. Without it, such a bind is refused with a
// PublicBindWithoutAuthError (fail secure — deny by default). It exists for
// deployments that terminate authentication in front of this server (a mesh
// sidecar, an authenticating proxy); naming it "insecure" makes the trade-off
// explicit at the call site.
func WithInsecurePublicBind() ServerOption {
	return func(c *serverConfig) { c.allowInsecurePublic = true }
}

// Server builds a hardened *http.Server bound to addr and serving h, refusing —
// fail secure — to bind a public address without authentication (SPEC §10,
// Decision #18). It does NOT listen or serve; the caller runs the returned
// server (ListenAndServe / Serve), so binding policy is decided here and the
// listen lifecycle stays the caller's.
//
// The has-auth bit is recovered from h WITHOUT re-parsing options: if h was built
// by Handler it satisfies authAware and reports whether an authenticator is
// installed; any other handler cannot prove auth and is treated as
// unauthenticated (fail secure). A public bind (empty or non-loopback host) with
// no auth and no WithInsecurePublicBind opt-in returns a PublicBindWithoutAuthError
// and no server. A malformed addr returns an InvalidAddrError and no server.
func Server(addr string, h http.Handler, opts ...ServerOption) (*http.Server, error) {
	cfg := &serverConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, InvalidAddrError{Addr: addr, Cause: err}
	}

	if !isLoopbackHost(host) && !hasInstalledAuth(h) && !cfg.allowInsecurePublic {
		return nil, PublicBindWithoutAuthError{Addr: addr}
	}

	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadTimeout:       readTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		// WriteTimeout is deliberately 0 (no server-wide write deadline): GET
		// /v1/sessions/{sid}/events is a long-lived SSE stream a server-wide
		// WriteTimeout would truncate. The READ/slowloris surface is covered by
		// ReadHeaderTimeout (header-read budget) + IdleTimeout (keep-alive budget)
		// + MaxHeaderBytes (header cap); the request body is capped separately by
		// the body-cap middleware. The consequence, accepted here, is that non-SSE
		// response WRITES are NOT deadline-bounded — a low-severity, response-side
		// slowloris — tolerated because these responses are small JSON envelopes.
		// The future hardening, if that residual risk needs closing, is a
		// per-request write-deadline middleware that /events would clear (exactly
		// as it already clears the per-connection deadline via ResponseController).
		WriteTimeout: 0,
		// Defensive per CLAUDE.md: TLS termination is the deployment's job, but a
		// pinned MinVersion is harmless on a plain server and correct if this
		// server is ever reused behind TLS.
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}, nil
}

// hasInstalledAuth reports whether h carries a proven authenticator. It is true
// ONLY when h satisfies authAware (i.e. it came from Handler) AND that handler
// reports an authenticator installed. Any handler that does not satisfy authAware
// is treated as unauthenticated — the fail-secure default: absent proof of auth,
// assume none.
//
// CONSEQUENCE: wrapping the Handler result in further middleware forfeits the
// authAware proof (the wrapper is a plain http.Handler), so Server fails secure
// and refuses a public bind AS IF unauthenticated even when auth is in fact
// installed. This is by design and only ever errs safe — a wrapper can cause a
// false-negative refusal, never a false-positive exposure. Pass the Handler
// result to Server directly, or have the wrapper also implement authAware.
func hasInstalledAuth(h http.Handler) bool {
	aware, ok := h.(authAware)
	return ok && aware.authInstalled()
}

// isLoopbackHost reports whether host is provably loopback. It treats the literal
// "localhost", 127.0.0.0/8, and ::1 as loopback. An empty host (as in ":8080")
// binds all interfaces and is NOT loopback; any host that does not parse as a
// loopback IP and is not the "localhost" literal is treated as non-loopback
// (fail-secure). Reimplemented here (not imported from pkg/api) to keep serve's
// dependency surface stdlib-plus-leaf only.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
