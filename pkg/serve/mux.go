package serve

import "net/http"

// Route patterns (Go 1.22 method+path syntax) for serve's session HTTP surface
// (SPEC §6). Each pattern is registered on ONE ServeMux; the method is part of
// the pattern, so a matching path with the wrong method yields 405 (not 404) and
// an unmatched path yields 404 — both from the mux, before any handler runs.
//
// The patterns are disjoint by design: no two overlap. The live plane owns
// /events (a running in-process session); the read plane owns /status, /journal,
// and the bare /v1/sessions listing (pure reads over durable history). {sid} and
// {gid} are wildcard path segments the handlers recover via r.PathValue.
const (
	routeCapabilities = "GET /v1/capabilities"
	routeCreate       = "POST /v1/sessions"
	routeList         = "GET /v1/sessions"
	routeRestore      = "POST /v1/sessions/{sid}/restore"
	routeInput        = "POST /v1/sessions/{sid}/input"
	routeInterrupt    = "POST /v1/sessions/{sid}/interrupt"
	routeGate         = "POST /v1/sessions/{sid}/gates/{gid}"
	routeEvents       = "GET /v1/sessions/{sid}/events"
	routeStatus       = "GET /v1/sessions/{sid}/status"
	routeJournal      = "GET /v1/sessions/{sid}/journal"
)

// authAware is the narrow, unexported view Server uses to learn — WITHOUT
// re-parsing options — whether the handler it was handed was built by Handler
// with an authenticator installed. A handler that does NOT satisfy this
// interface is treated as unauthenticated by the fail-secure bind (it cannot
// prove auth, so it assumes none). Interface Segregation: it exposes exactly the
// one bit the bind decision needs.
type authAware interface {
	authInstalled() bool
}

// boundHandler is the concrete carrier Handler returns: it IS an http.Handler
// (via the embedded field) and additionally carries whether an authenticator was
// installed. This threads the has-auth bit from the config to Server's
// fail-secure bind without re-deriving the auth policy from the options (SPEC §10
// design note). It is deliberately unexported — callers hold it only as an
// http.Handler; Server recovers the bit through the authAware interface.
//
// The embedded http.Handler is an invariant-non-nil field: the sole constructor
// (Handler) always sets it to cfg.wrap(mux), so it is never nil in practice; a
// nil embedded handler would panic in the promoted ServeHTTP.
//
// NOTE: wrapping this value in further middleware forfeits the authAware has-auth
// proof, so Server fails secure and refuses a public bind as if unauthenticated
// (see hasInstalledAuth) — pass the Handler result to Server directly.
type boundHandler struct {
	http.Handler
	hasAuth bool
}

// authInstalled reports whether the handler was built with an authenticator,
// satisfying authAware so Server can read the bit.
func (b *boundHandler) authInstalled() bool { return b.hasAuth }

// Handler builds the complete session HTTP surface: it assembles the config from
// opts, mints a server over rig and reads, registers every route (SPEC §6) on
// a single ServeMux using Go 1.22 method+path patterns, and wraps the mux with
// the request-path middleware (authentication then body-cap). It returns a
// concrete *boundHandler that carries the has-auth bit so a downstream Server
// bind can fail secure without re-parsing the options.
//
// rig and reads are wired at the composition root: rig drives the live
// plane (create/restore/input/interrupt/gate/events) and reads backs the stateless
// read plane (list/status/journal). All routes are disjoint, so registration order
// is irrelevant and no pattern conflicts.
func Handler[S LiveSession, O any](rig Rig[S, O], reads Reader, opts ...Option) http.Handler {
	cfg := newConfig(opts...)
	srv := newServer(rig, reads, cfg)

	mux := http.NewServeMux()
	mux.HandleFunc(routeCapabilities, srv.handleCapabilities)
	mux.HandleFunc(routeCreate, srv.handleCreate)
	mux.HandleFunc(routeList, srv.handleListSessions)
	mux.HandleFunc(routeRestore, srv.handleRestore)
	mux.HandleFunc(routeInput, srv.handleInput)
	mux.HandleFunc(routeInterrupt, srv.handleInterrupt)
	mux.HandleFunc(routeGate, srv.handleGateResponse)
	mux.HandleFunc(routeEvents, srv.handleEvents)
	mux.HandleFunc(routeStatus, srv.handleStatus)
	mux.HandleFunc(routeJournal, srv.handleJournal)

	return &boundHandler{Handler: cfg.wrap(mux), hasAuth: cfg.hasAuth()}
}
