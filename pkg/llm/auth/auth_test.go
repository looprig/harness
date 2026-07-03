package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/llm"
)

func TestKeySetsBearer(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest(http.MethodPost, "https://x.test", nil)
	if err := Key("sekret").Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sekret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sekret")
	}
}

func TestHeaderSetsCustom(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest(http.MethodPost, "https://x.test", nil)
	if err := Header("sekret", "x-api-key").Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "sekret" {
		t.Errorf("x-api-key = %q, want %q", got, "sekret")
	}
	if req.Header.Get("Authorization") != "" {
		t.Errorf("Header must not set Authorization")
	}
}

func TestNoneIsNoop(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest(http.MethodPost, "https://x.test", nil)
	if err := None().Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if len(req.Header) != 0 {
		t.Errorf("None must not mutate headers, got %v", req.Header)
	}
}

func TestAuthenticatorRedactsSecret(t *testing.T) {
	t.Parallel()
	const secret = "supersecret-token"
	auths := []struct {
		name string
		a    llm.Authenticator
	}{
		{name: "Key", a: Key(secret)},
		{name: "Header", a: Header(secret, "x-api-key")},
	}
	for _, tt := range auths {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, s := range []string{
				fmt.Sprintf("%v", tt.a),
				fmt.Sprintf("%+v", tt.a),
				fmt.Sprintf("%s", tt.a),
				fmt.Sprintf("%#v", tt.a),
			} {
				if strings.Contains(s, secret) {
					t.Errorf("formatted authenticator leaked secret: %q", s)
				}
			}
		})
	}
}
