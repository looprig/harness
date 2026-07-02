package api

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ciram-co/looprig/pkg/uuid"
)

// Hardened-server and body-cap defaults. Named so no magic numbers are scattered
// through Serve/Handler. Durations are the fallback when the corresponding
// Config field is unset (<= 0); byte caps likewise.
const (
	defaultReadTimeout       = 5 * time.Second  // whole-request read budget (headers + body)
	defaultReadHeaderTimeout = 5 * time.Second  // header read budget — the slowloris guard
	defaultIdleTimeout       = 60 * time.Second // keep-alive idle budget
	defaultMaxHeaderBytes    = 1 << 20          // 1 MiB header cap
	defaultMaxBodyBytes      = 1 << 20          // 1 MiB request-body cap (MaxBytesReader)

	// shutdownTimeout bounds the graceful srv.Shutdown so a wedged connection
	// cannot block teardown forever. agentCloseTimeout bounds each per-session
	// agent Close during shutdown for the same reason.
	shutdownTimeout   = 10 * time.Second
	agentCloseTimeout = 5 * time.Second
)

// ListenError reports that the server could not bind its resolved TCP address.
// The address has already passed the loopback-default guard; this is the lower-
// level net.Listen failure (e.g. port in use, permission denied). Cause is the
// underlying error, exposed via Unwrap so callers can inspect it.
type ListenError struct {
	Addr  string
	Cause error
}

func (e ListenError) Error() string {
	return "api: listen on " + e.Addr + ": " + e.Cause.Error()
}

func (e ListenError) Unwrap() error { return e.Cause }

// sessionEntry is one live session: its driven agent plus the supervisor that
// maintains that session's pending-gate registry. The two are torn down together
// (supervisor stopped, agent closed) when the session ends or the server shuts
// down.
type sessionEntry struct {
	agent Agent
	sup   *supervisor
}

// server holds the runner state shared by all HTTP handlers: the consumer's
// Factory, the Config, and the many-session registry (mutex-guarded). Its single
// responsibility is to own that shared state and route it to the handlers; it
// never embeds agent policy, composition, or credentials — those live behind the
// Factory. Agent/supervisor methods are NEVER called while holding s.mu.
type server struct {
	factory  Factory
	cfg      Config
	mu       sync.Mutex
	sessions map[uuid.UUID]*sessionEntry
}

// newServer builds the shared runner state. Both Handler and Serve construct one
// so the HTTP surface and the graceful-shutdown teardown operate on the SAME
// session registry.
func newServer(cfg Config, f Factory) *server {
	return &server{
		factory:  f,
		cfg:      cfg,
		sessions: make(map[uuid.UUID]*sessionEntry),
	}
}

// getSession returns the live entry for id, if any. Mutex-guarded; the caller
// must not call agent/supervisor methods while it (or any registry helper) holds
// s.mu — this returns the entry so those calls happen after the lock is released.
func (s *server) getSession(id uuid.UUID) (*sessionEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	return e, ok
}

// putSession registers e under id (overwriting any prior entry). Mutex-guarded.
func (s *server) putSession(id uuid.UUID, e *sessionEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = e
}

// deleteSession removes and returns the entry for id so the caller can stop the
// supervisor and close the agent OUTSIDE the lock. Mutex-guarded; it performs no
// teardown itself, precisely so no agent/supervisor call happens under s.mu.
func (s *server) deleteSession(id uuid.UUID) (*sessionEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	return e, ok
}

// snapshotSessions returns a fresh slice of every live entry for shutdown. The
// copy lets the caller tear each session down without holding s.mu (and without
// mutating the map mid-iteration). Order is unspecified (map iteration).
func (s *server) snapshotSessions() []*sessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*sessionEntry, 0, len(s.sessions))
	for _, e := range s.sessions {
		out = append(out, e)
	}
	return out
}

// effectiveMaxBody is the per-request body cap: the configured value when set,
// else the 1 MiB default. This cap is part of the api's hardening and is applied
// by the body-limit wrapper regardless of any consumer middleware.
func (s *server) effectiveMaxBody() int64 {
	if s.cfg.MaxBodyBytes > 0 {
		return s.cfg.MaxBodyBytes
	}
	return defaultMaxBodyBytes
}

// handler builds the routed mux and wraps it so every request body is capped.
// Handler and Serve both call it; the wrap is inside the api so the body cap
// holds even if a consumer stacks its own middleware around the returned handler.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	return s.limitBody(mux)
}

// limitBody caps each request's body via http.MaxBytesReader before delegating
// to next. It is the api's request-body hardening and applies to every route.
func (s *server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, s.effectiveMaxBody())
		next.ServeHTTP(w, r)
	})
}

// routes registers the HTTP routes on mux using Go 1.22 method+pattern routing.
// Task 12 wires only GET /healthz — the single explicitly-unauthenticated route.
func (s *server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Task 13 registers the session + gate-routing endpoints here, e.g.:
	//   mux.HandleFunc("POST /sessions", s.handleCreateSession)
	//   mux.HandleFunc("POST /sessions/{sid}/input", s.handleInput)
	//   mux.HandleFunc("GET /sessions/{sid}/events", s.handleEvents)   // SSE
	//   mux.HandleFunc("POST /sessions/{sid}/gates/{tid}/approve", ...)
	//   mux.HandleFunc("POST /sessions/{sid}/gates/{tid}/deny", ...)
	//   mux.HandleFunc("POST /sessions/{sid}/gates/{tid}/answer", ...)
	//   mux.HandleFunc("GET /sessions/{sid}/gates", s.handleListGates)
}

// handleHealthz answers 200 with an intentionally data-free body: it is the one
// unauthenticated route and must leak nothing about the runner's state.
func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// Handler builds the routed, body-capped http.Handler for cfg + f and returns it
// so a consumer can wrap it with their own middleware. The api's body cap is
// applied inside this handler and therefore holds regardless of that middleware.
func Handler(cfg Config, f Factory) http.Handler {
	return newServer(cfg, f).handler()
}

// Serve binds a hardened HTTP server per cfg (loopback-default via
// resolveListenAddr) and serves until ctx is cancelled, then shuts down
// gracefully and tears down every live session. It returns the guard error
// (PublicBindError/InvalidAddrError) WITHOUT binding on a guard failure, a
// typed ListenError if the bind itself fails, and nil on a clean graceful
// shutdown (http.ErrServerClosed is treated as success).
func Serve(ctx context.Context, cfg Config, f Factory) error {
	addr, err := resolveListenAddr(cfg)
	if err != nil {
		return err // loopback-default guard failed — bind nothing.
	}

	// net.Listen (not srv.ListenAndServe) so an ephemeral ":0" resolves to a real
	// port we can log the bound address of.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return ListenError{Addr: addr, Cause: err}
	}

	s := newServer(cfg, f)
	srv := &http.Server{
		Handler:           s.handler(),
		ReadTimeout:       dfltDuration(cfg.ReadTimeout, defaultReadTimeout),
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		// WriteTimeout defaults to 0 (no server-wide write deadline) on purpose:
		// GET /sessions/{sid}/events is a long-lived SSE stream that a server-wide
		// WriteTimeout would truncate. Request/response endpoints are instead
		// bounded by ReadTimeout + ReadHeaderTimeout + the per-request context +
		// the MaxBytesReader body cap. A consumer that sets cfg.WriteTimeout gets
		// their explicit value honored.
		WriteTimeout:   cfg.WriteTimeout,
		IdleTimeout:    dfltDuration(cfg.IdleTimeout, defaultIdleTimeout),
		MaxHeaderBytes: dfltInt(cfg.MaxHeaderBytes, defaultMaxHeaderBytes),
		// Defensive per CLAUDE.md even though v1 serves plain HTTP over loopback:
		// TLS termination is the consumer's job, and MinVersion is harmless on a
		// plain server yet correct if this server is ever reused behind TLS.
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}

	slog.Info("api: serving", "addr", ln.Addr().String())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		// Serve returned on its own (e.g. an accept-loop failure) before ctx was
		// cancelled. There is nothing to shut down; surface the cause.
		return ignoreServerClosed(err)
	case <-ctx.Done():
		return s.shutdown(srv, serveErr)
	}
}

// shutdown performs the graceful teardown once ctx is cancelled: it Shutdown()s
// the server on a BOUNDED context, tears down every live session (best-effort),
// then returns the settled serve error (nil on the expected http.ErrServerClosed).
func (s *server) shutdown(srv *http.Server, serveErr <-chan error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("api: graceful shutdown failed", "err", err)
	}
	s.teardownSessions()
	return ignoreServerClosed(<-serveErr)
}

// teardownSessions stops each live session's supervisor and closes its agent. It
// snapshots the registry first so no agent/supervisor call happens under s.mu,
// and is best-effort: an error on one session is logged, never propagated, so a
// single wedged agent cannot block the server's shutdown.
func (s *server) teardownSessions() {
	for _, e := range s.snapshotSessions() {
		if err := e.sup.stop(); err != nil {
			slog.Error("api: supervisor stop during shutdown", "err", err)
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), agentCloseTimeout)
		if err := e.agent.Close(closeCtx); err != nil {
			slog.Error("api: agent close during shutdown", "err", err)
		}
		cancel()
	}
}

// ignoreServerClosed maps http.ErrServerClosed (the expected result of a
// graceful Shutdown or a Shutdown-before-Serve race) to nil, leaving any other
// error unchanged.
func ignoreServerClosed(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// dfltDuration returns v when it is positive, else d — the fallback for an unset
// Config duration.
func dfltDuration(v, d time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return d
}

// dfltInt returns v when it is positive, else d — the fallback for an unset
// Config byte count.
func dfltInt(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}
