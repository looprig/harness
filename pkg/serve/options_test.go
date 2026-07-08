package serve

import (
	"net/http"
	"testing"
)

// TestNewConfigDefaults pins the secure defaults newConfig applies with no
// options: no authenticator installed (hasAuth false) and the 1 MiB body cap.
func TestNewConfigDefaults(t *testing.T) {
	t.Parallel()
	c := newConfig()
	if c.hasAuth() {
		t.Errorf("newConfig() hasAuth = true, want false (no auth by default)")
	}
	if c.maxBodyBytes != defaultMaxBodyBytes {
		t.Errorf("newConfig() maxBodyBytes = %d, want %d", c.maxBodyBytes, defaultMaxBodyBytes)
	}
}

// TestWithAuth verifies WithAuth installs an authenticator and that hasAuth
// reflects its presence — including the fail-safe rule that a nil authn is
// ignored (stays no-auth).
func TestWithAuth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		authn       func(*http.Request) error
		wantHasAuth bool
	}{
		{name: "non-nil authn installs auth", authn: func(*http.Request) error { return nil }, wantHasAuth: true},
		{name: "nil authn ignored, stays no-auth", authn: nil, wantHasAuth: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newConfig(WithAuth(tt.authn))
			if got := c.hasAuth(); got != tt.wantHasAuth {
				t.Errorf("hasAuth() = %v, want %v", got, tt.wantHasAuth)
			}
		})
	}
}

// TestWithMaxBodyBytes verifies WithMaxBodyBytes sets a positive cap and, per the
// fail-safe convention, ignores a non-positive n (the default cap stays).
func TestWithMaxBodyBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		n    int64
		want int64
	}{
		{name: "positive sets cap", n: 4096, want: 4096},
		{name: "zero keeps default", n: 0, want: defaultMaxBodyBytes},
		{name: "negative keeps default", n: -1, want: defaultMaxBodyBytes},
		{name: "one byte", n: 1, want: 1},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newConfig(WithMaxBodyBytes(tt.n))
			if c.maxBodyBytes != tt.want {
				t.Errorf("maxBodyBytes = %d, want %d", c.maxBodyBytes, tt.want)
			}
		})
	}
}

// TestNewConfigNilOptionIgnored verifies newConfig tolerates a nil Option in the
// variadic list without panicking (defensive composition-root wiring).
func TestNewConfigNilOptionIgnored(t *testing.T) {
	t.Parallel()
	c := newConfig(nil, WithMaxBodyBytes(2048), nil)
	if c.maxBodyBytes != 2048 {
		t.Errorf("maxBodyBytes = %d, want 2048", c.maxBodyBytes)
	}
}
