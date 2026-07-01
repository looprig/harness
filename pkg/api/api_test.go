package api

import (
	"errors"
	"testing"
)

// TestConfig_LoopbackDefault covers resolveListenAddr's loopback-accepting
// cases: an empty Addr defaults to loopback+ephemeral, and any host that is
// provably loopback (127.0.0.0/8, ::1, the literal "localhost") is returned
// unchanged with no error and no AllowPublic opt-in required.
func TestConfig_LoopbackDefault(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "empty addr defaults to loopback ephemeral", cfg: Config{Addr: ""}, want: "127.0.0.1:0"},
		{name: "loopback ephemeral unchanged", cfg: Config{Addr: "127.0.0.1:0"}, want: "127.0.0.1:0"},
		{name: "loopback fixed port unchanged", cfg: Config{Addr: "127.0.0.1:8080"}, want: "127.0.0.1:8080"},
		{name: "loopback in 127/8 unchanged", cfg: Config{Addr: "127.0.0.5:8080"}, want: "127.0.0.5:8080"},
		{name: "ipv6 loopback unchanged", cfg: Config{Addr: "[::1]:8080"}, want: "[::1]:8080"},
		{name: "localhost literal unchanged", cfg: Config{Addr: "localhost:8080"}, want: "localhost:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveListenAddr(tt.cfg)
			if err != nil {
				t.Fatalf("resolveListenAddr(%+v) unexpected error = %v", tt.cfg, err)
			}
			if got != tt.want {
				t.Errorf("resolveListenAddr(%+v) = %q, want %q", tt.cfg, got, tt.want)
			}
		})
	}
}

// TestConfig_PublicRequiresOptIn covers the fail-secure guard: any host not
// provably loopback (including the empty host that binds all interfaces) is
// rejected with a typed PublicBindError unless AllowPublic is set, in which case
// the address is returned unchanged. A malformed address (no port) fails secure
// with a typed InvalidAddrError and never binds.
func TestConfig_PublicRequiresOptIn(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		want       string
		wantErr    bool
		wantPublic bool // expect a PublicBindError
		wantInvln  bool // expect an InvalidAddrError
	}{
		{name: "all interfaces v4 without opt-in rejected", cfg: Config{Addr: "0.0.0.0:8080"}, wantErr: true, wantPublic: true},
		{name: "empty host binds all interfaces rejected", cfg: Config{Addr: ":8080"}, wantErr: true, wantPublic: true},
		{name: "public ip without opt-in rejected", cfg: Config{Addr: "192.168.1.5:8080"}, wantErr: true, wantPublic: true},
		{name: "all interfaces ephemeral without opt-in rejected", cfg: Config{Addr: "0.0.0.0:0"}, wantErr: true, wantPublic: true},
		{name: "non-ip hostname not provably loopback rejected", cfg: Config{Addr: "example.com:8080"}, wantErr: true, wantPublic: true},
		{name: "all interfaces v4 with opt-in allowed", cfg: Config{Addr: "0.0.0.0:8080", AllowPublic: true}, want: "0.0.0.0:8080"},
		{name: "public ip with opt-in allowed", cfg: Config{Addr: "192.168.1.5:8080", AllowPublic: true}, want: "192.168.1.5:8080"},
		{name: "empty host with opt-in allowed", cfg: Config{Addr: ":8080", AllowPublic: true}, want: ":8080"},
		{name: "malformed missing port rejected", cfg: Config{Addr: "127.0.0.1"}, wantErr: true, wantInvln: true},
		{name: "malformed missing port not saved by opt-in", cfg: Config{Addr: "127.0.0.1", AllowPublic: true}, wantErr: true, wantInvln: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveListenAddr(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveListenAddr(%+v) error = %v, wantErr %v", tt.cfg, err, tt.wantErr)
			}
			if tt.wantErr {
				if got != "" {
					t.Errorf("resolveListenAddr(%+v) returned addr %q on error; must fail secure with empty addr", tt.cfg, got)
				}
				var pubErr PublicBindError
				if errors.As(err, &pubErr) != tt.wantPublic {
					t.Errorf("resolveListenAddr(%+v) PublicBindError = %v, want %v (err=%v)", tt.cfg, errors.As(err, &pubErr), tt.wantPublic, err)
				}
				if tt.wantPublic && pubErr.Addr != tt.cfg.Addr {
					t.Errorf("PublicBindError.Addr = %q, want %q", pubErr.Addr, tt.cfg.Addr)
				}
				var invErr InvalidAddrError
				if errors.As(err, &invErr) != tt.wantInvln {
					t.Errorf("resolveListenAddr(%+v) InvalidAddrError = %v, want %v (err=%v)", tt.cfg, errors.As(err, &invErr), tt.wantInvln, err)
				}
				if tt.wantInvln {
					if invErr.Addr != tt.cfg.Addr {
						t.Errorf("InvalidAddrError.Addr = %q, want %q", invErr.Addr, tt.cfg.Addr)
					}
					if invErr.Cause == nil {
						t.Error("InvalidAddrError.Cause = nil, want the underlying net.SplitHostPort error")
					}
					if unwrapped := errors.Unwrap(invErr); unwrapped == nil {
						t.Error("InvalidAddrError.Unwrap() = nil, want the wrapped cause")
					}
				}
				return
			}
			if got != tt.want {
				t.Errorf("resolveListenAddr(%+v) = %q, want %q", tt.cfg, got, tt.want)
			}
		})
	}
}
