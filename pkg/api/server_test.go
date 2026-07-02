package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serveReturnWait bounds how long a graceful-shutdown test waits for Serve to
// return after its ctx is cancelled. The assertion is the return value + the
// fact that it returns at all (does not hang) — this only caps the wait.
const serveReturnWait = 5 * time.Second

// fakeFactory is a Factory that hands back an inert fakeAgent (defined in
// supervisor_test.go). Task 12 never creates sessions (only POST /sessions in
// Task 13 does), so the factory is never invoked by these tests — it only has
// to be a well-typed Factory. The injected fakeSub keeps Subscribe valid should
// a future test exercise the session path.
func fakeFactory(_ context.Context, _ AgentRequest) (Agent, error) {
	return &fakeAgent{sub: newFakeSub()}, nil
}

// TestServe_HealthzOK proves Handler wires the one unauthenticated route:
// GET /healthz answers 200 with a data-free body, an unknown path is 404, and a
// wrong method on the health route is 405 (Go 1.22 method+pattern routing). It
// exercises the returned http.Handler directly via httptest.NewServer, so the
// body-cap wrapper is in the path too.
func TestServe_HealthzOK(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(Handler(Config{}, fakeFactory))
	defer ts.Close()

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantEmpty  bool
	}{
		{name: "healthz GET returns 200 empty body", method: http.MethodGet, path: "/healthz", wantStatus: http.StatusOK, wantEmpty: true},
		{name: "healthz wrong method returns 405", method: http.MethodPost, path: "/healthz", wantStatus: http.StatusMethodNotAllowed},
		{name: "unknown path returns 404", method: http.MethodGet, path: "/does-not-exist", wantStatus: http.StatusNotFound},
	}
	// The parallel subtests are nested in a synchronous group so ts stays open
	// until every one finishes: a paused t.Parallel() subtest runs AFTER the
	// enclosing function returns, so a top-level `defer ts.Close()` would race
	// ahead of them. t.Run("cases") blocks on its parallel children, and only
	// then does the parent's defer close the server.
	t.Run("cases", func(t *testing.T) {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				req, err := http.NewRequest(tt.method, ts.URL+tt.path, nil)
				if err != nil {
					t.Fatalf("NewRequest(%s %s) error = %v", tt.method, tt.path, err)
				}
				resp, err := ts.Client().Do(req)
				if err != nil {
					t.Fatalf("Do(%s %s) error = %v", tt.method, tt.path, err)
				}
				defer func() { _ = resp.Body.Close() }()

				if resp.StatusCode != tt.wantStatus {
					t.Errorf("Do(%s %s) status = %d, want %d", tt.method, tt.path, resp.StatusCode, tt.wantStatus)
				}
				if tt.wantEmpty {
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatalf("ReadAll(healthz body) error = %v", err)
					}
					if len(body) != 0 {
						t.Errorf("healthz body = %q, want empty (data-free)", body)
					}
				}
			})
		}
	})
}

// TestServe_RejectsPublicBind proves Serve enforces the loopback-default guard
// BEFORE binding and propagates the typed guard errors: a non-loopback Addr
// without AllowPublic returns *PublicBindError (and binds nothing); a malformed
// Addr returns *InvalidAddrError; the same public Addr WITH AllowPublic binds
// and then shuts down cleanly (nil) when the already-cancelled ctx is observed.
func TestServe_RejectsPublicBind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cfg         Config
		wantErr     bool
		wantPublic  bool
		wantInvalid bool
	}{
		{name: "public bind without opt-in rejected", cfg: Config{Addr: "0.0.0.0:0"}, wantErr: true, wantPublic: true},
		{name: "empty host binds all interfaces rejected", cfg: Config{Addr: ":0"}, wantErr: true, wantPublic: true},
		{name: "malformed addr missing port rejected", cfg: Config{Addr: "0.0.0.0"}, wantErr: true, wantInvalid: true},
		{name: "public bind with opt-in serves then shuts down nil", cfg: Config{Addr: "0.0.0.0:0", AllowPublic: true}, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Already-cancelled ctx: a guard failure returns before binding; the
			// opt-in case binds, immediately observes cancellation, and shuts down.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := Serve(ctx, tt.cfg, fakeFactory)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Serve(%+v) error = %v, wantErr %v", tt.cfg, err, tt.wantErr)
			}

			var pubErr PublicBindError
			if errors.As(err, &pubErr) != tt.wantPublic {
				t.Errorf("Serve(%+v) PublicBindError = %v, want %v (err=%v)", tt.cfg, errors.As(err, &pubErr), tt.wantPublic, err)
			}
			if tt.wantPublic && pubErr.Addr != tt.cfg.Addr {
				t.Errorf("PublicBindError.Addr = %q, want %q", pubErr.Addr, tt.cfg.Addr)
			}
			var invErr InvalidAddrError
			if errors.As(err, &invErr) != tt.wantInvalid {
				t.Errorf("Serve(%+v) InvalidAddrError = %v, want %v (err=%v)", tt.cfg, errors.As(err, &invErr), tt.wantInvalid, err)
			}
		})
	}
}

// TestServer_SessionRegistry exercises the mutex-guarded registry helpers Task
// 13 depends on: put/get round-trips an entry, get and delete miss on an absent
// id (fail-secure: nil, false), delete evicts and RETURNS the removed entry (so
// the caller can stop/close it outside the lock), a scoped delete leaves other
// sessions intact, and snapshotSessions copies the live set. Entries carry nil
// agent/sup because the registry helpers never touch either — distinct pointers
// are all the map semantics need.
func TestServer_SessionRegistry(t *testing.T) {
	t.Parallel()

	s := newServer(Config{}, fakeFactory)
	idA, idB := mkID(0x01), mkID(0x02)
	entryA, entryB := &sessionEntry{}, &sessionEntry{}

	// Empty registry: get misses, snapshot is empty.
	if got, ok := s.getSession(idA); ok || got != nil {
		t.Fatalf("getSession(idA) on empty = %v, %v; want nil, false", got, ok)
	}
	if got := s.snapshotSessions(); len(got) != 0 {
		t.Fatalf("snapshotSessions on empty = %d entries, want 0", len(got))
	}

	// Put then get: round-trips the exact entry; snapshot holds both.
	s.putSession(idA, entryA)
	s.putSession(idB, entryB)
	if got, ok := s.getSession(idA); !ok || got != entryA {
		t.Fatalf("getSession(idA) = %v, %v; want %p, true", got, ok, entryA)
	}
	if got := s.snapshotSessions(); len(got) != 2 {
		t.Fatalf("snapshotSessions = %d entries, want 2", len(got))
	}

	// Delete returns the removed entry and evicts only it.
	got, ok := s.deleteSession(idA)
	if !ok || got != entryA {
		t.Fatalf("deleteSession(idA) = %v, %v; want %p, true", got, ok, entryA)
	}
	if got, ok := s.getSession(idA); ok || got != nil {
		t.Fatalf("getSession(idA) after delete = %v, %v; want nil, false", got, ok)
	}
	if _, ok := s.getSession(idB); !ok {
		t.Fatal("deleteSession(idA) also evicted idB; delete is not scoped")
	}

	// Delete on an absent id fails secure: nil, false (no panic, no phantom).
	if got, ok := s.deleteSession(idA); ok || got != nil {
		t.Fatalf("deleteSession(idA) second time = %v, %v; want nil, false", got, ok)
	}
}

// TestServe_GracefulShutdown proves the ctx wiring: Serve bound to an ephemeral
// loopback address returns nil (NOT http.ErrServerClosed) promptly once its ctx
// is cancelled, and does not hang. That the return is nil is the proof of the
// graceful Shutdown path.
func TestServe_GracefulShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, Config{Addr: "127.0.0.1:0"}, fakeFactory) }()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after graceful shutdown, want nil", err)
		}
	case <-time.After(serveReturnWait):
		t.Fatalf("Serve did not return within %v after ctx cancel", serveReturnWait)
	}
}
