package serve

import (
	"crypto/tls"
	"errors"
	"net/http"
	"testing"
	"time"
)

// plainHandler is an http.Handler that does NOT satisfy authAware — the arbitrary
// handler case Server must treat as unauthenticated (fail secure).
type plainHandler struct{}

func (plainHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

// authedHandler is built by Handler with an authenticator, so it satisfies
// authAware and reports auth installed.
func authedHandler(t *testing.T) http.Handler {
	t.Helper()
	rig := &fakeRig{}
	return Handler[*fakeSession](rig, &fakeReader{}, WithAuth(func(*http.Request) error { return nil }))
}

// noAuthBoundHandler is built by Handler with NO authenticator: it satisfies
// authAware but reports no auth installed (the has-auth bit is false).
func noAuthBoundHandler(t *testing.T) http.Handler {
	t.Helper()
	rig := &fakeRig{}
	return Handler[*fakeSession](rig, &fakeReader{})
}

func TestServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		addr        string
		handler     func(t *testing.T) http.Handler
		opts        []ServerOption
		wantErr     bool
		wantPublic  bool // expect *PublicBindWithoutAuthError
		wantInvalid bool // expect InvalidAddrError
		wantServer  bool
	}{
		{
			name:       "loopback ipv4 no auth is ok",
			addr:       "127.0.0.1:0",
			handler:    func(t *testing.T) http.Handler { return plainHandler{} },
			wantServer: true,
		},
		{
			name:       "loopback localhost no auth is ok",
			addr:       "localhost:0",
			handler:    func(t *testing.T) http.Handler { return plainHandler{} },
			wantServer: true,
		},
		{
			name:       "loopback ipv6 no auth is ok",
			addr:       "[::1]:0",
			handler:    func(t *testing.T) http.Handler { return plainHandler{} },
			wantServer: true,
		},
		{
			name:       "public wildcard no auth is refused",
			addr:       ":0",
			handler:    func(t *testing.T) http.Handler { return noAuthBoundHandler(t) },
			wantErr:    true,
			wantPublic: true,
		},
		{
			name:       "public zero-host no auth is refused",
			addr:       "0.0.0.0:0",
			handler:    func(t *testing.T) http.Handler { return noAuthBoundHandler(t) },
			wantErr:    true,
			wantPublic: true,
		},
		{
			name:       "public no auth with insecure opt is ok",
			addr:       ":0",
			handler:    func(t *testing.T) http.Handler { return noAuthBoundHandler(t) },
			opts:       []ServerOption{WithInsecurePublicBind()},
			wantServer: true,
		},
		{
			name:       "public with installed auth is ok",
			addr:       "0.0.0.0:0",
			handler:    authedHandler,
			wantServer: true,
		},
		{
			name:       "public plain handler no authAware is refused",
			addr:       ":0",
			handler:    func(t *testing.T) http.Handler { return plainHandler{} },
			wantErr:    true,
			wantPublic: true,
		},
		{
			name:        "malformed addr is invalid",
			addr:        "noport",
			handler:     func(t *testing.T) http.Handler { return plainHandler{} },
			wantErr:     true,
			wantInvalid: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, err := Server(tt.addr, tt.handler(t), tt.opts...)

			if (err != nil) != tt.wantErr {
				t.Fatalf("Server() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantPublic {
				var pubErr PublicBindWithoutAuthError
				if !errors.As(err, &pubErr) {
					t.Fatalf("err = %v, want PublicBindWithoutAuthError", err)
				}
				if pubErr.Addr != tt.addr {
					t.Errorf("PublicBindWithoutAuthError.Addr = %q, want %q", pubErr.Addr, tt.addr)
				}
			}
			if tt.wantInvalid {
				var invErr InvalidAddrError
				if !errors.As(err, &invErr) {
					t.Fatalf("err = %v, want InvalidAddrError", err)
				}
				if invErr.Addr != tt.addr {
					t.Errorf("InvalidAddrError.Addr = %q, want %q", invErr.Addr, tt.addr)
				}
				if invErr.Unwrap() == nil {
					t.Error("InvalidAddrError.Unwrap() = nil, want the wrapped cause")
				}
			}
			if tt.wantServer != (srv != nil) {
				t.Fatalf("server non-nil = %v, want %v", srv != nil, tt.wantServer)
			}
			if !tt.wantServer {
				return
			}

			// Hardened defaults on the returned server.
			if srv.Addr != tt.addr {
				t.Errorf("Addr = %q, want %q", srv.Addr, tt.addr)
			}
			if srv.ReadTimeout != 5*time.Second {
				t.Errorf("ReadTimeout = %v, want 5s", srv.ReadTimeout)
			}
			if srv.ReadHeaderTimeout != 5*time.Second {
				t.Errorf("ReadHeaderTimeout = %v, want 5s", srv.ReadHeaderTimeout)
			}
			if srv.IdleTimeout != 60*time.Second {
				t.Errorf("IdleTimeout = %v, want 60s", srv.IdleTimeout)
			}
			if srv.WriteTimeout != 0 {
				t.Errorf("WriteTimeout = %v, want 0 (SSE-safe)", srv.WriteTimeout)
			}
			if srv.MaxHeaderBytes != 1<<20 {
				t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, 1<<20)
			}
			if srv.TLSConfig == nil || srv.TLSConfig.MinVersion != tls.VersionTLS12 {
				t.Errorf("TLSConfig.MinVersion = %v, want TLS 1.2", srv.TLSConfig)
			}
		})
	}
}

// TestIsLoopbackHost pins the loopback classification, most importantly that an
// empty host (a wildcard bind) is treated as NON-loopback / public (fail secure).
func TestIsLoopbackHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
		want bool
	}{
		{name: "localhost literal", host: "localhost", want: true},
		{name: "ipv4 loopback", host: "127.0.0.1", want: true},
		{name: "ipv4 loopback range", host: "127.0.0.5", want: true},
		{name: "ipv6 loopback", host: "::1", want: true},
		{name: "empty host is public", host: "", want: false},
		{name: "wildcard ipv4 is public", host: "0.0.0.0", want: false},
		{name: "routable ipv4 is public", host: "10.0.0.1", want: false},
		{name: "non-ip host is public", host: "example.com", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isLoopbackHost(tt.host); got != tt.want {
				t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}
