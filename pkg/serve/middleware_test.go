package serve

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// countingHandler records whether it was invoked and, on invocation, reads its
// request body fully (so body-cap read errors surface) and reports 200. It is the
// fake inner handler the middleware wraps.
type countingHandler struct {
	called   int
	readErr  error
	bodyRead []byte
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called++
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		h.bodyRead = b
		h.readErr = err
	}
	w.WriteHeader(http.StatusOK)
}

// TestAuthMiddleware exercises the auth seam: nil auth passes through; a failing
// authenticator returns 401 with the nested envelope WITHOUT calling the inner
// handler and WITHOUT leaking the authenticator's error text; a passing
// authenticator calls the inner handler.
func TestAuthMiddleware(t *testing.T) {
	t.Parallel()
	const secret = "super-secret-internal-cause-abc123"
	tests := []struct {
		name         string
		opts         []Option
		wantStatus   int
		wantInnerHit int
		wantCode     string // "" means no error envelope expected
	}{
		{
			name:         "nil auth passes through to inner",
			opts:         nil,
			wantStatus:   http.StatusOK,
			wantInnerHit: 1,
		},
		{
			name:         "authn nil error passes through to inner",
			opts:         []Option{WithAuth(func(*http.Request) error { return nil })},
			wantStatus:   http.StatusOK,
			wantInnerHit: 1,
		},
		{
			name:         "authn error rejects with 401 and does not call inner",
			opts:         []Option{WithAuth(func(*http.Request) error { return errors.New(secret) })},
			wantStatus:   http.StatusUnauthorized,
			wantInnerHit: 0,
			wantCode:     codeUnauthorized,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inner := &countingHandler{}
			c := newConfig(tt.opts...)
			h := c.wrap(inner)

			req := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if inner.called != tt.wantInnerHit {
				t.Errorf("inner handler called %d times, want %d", inner.called, tt.wantInnerHit)
			}
			body := rec.Body.String()
			if strings.Contains(body, secret) {
				t.Errorf("response body leaked authenticator error text: %q", body)
			}
			if tt.wantCode != "" {
				var env errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
					t.Fatalf("decode error envelope: %v (body=%q)", err, body)
				}
				if env.Error.Code != tt.wantCode {
					t.Errorf("error code = %q, want %q", env.Error.Code, tt.wantCode)
				}
				if env.Error.Retryable {
					t.Errorf("unauthorized envelope should not be retryable")
				}
				if env.Error.Message == "" {
					t.Errorf("error envelope message is empty")
				}
			}
		})
	}
}

// TestBodyCapMiddleware verifies http.MaxBytesReader is installed: a body under
// the cap reads cleanly and reaches the inner handler intact, while a body over
// the cap produces a read error inside the handler (the lazy, on-read mechanism).
func TestBodyCapMiddleware(t *testing.T) {
	t.Parallel()
	const cap int64 = 16
	tests := []struct {
		name       string
		body       string
		wantReadOK bool
	}{
		{name: "under cap reads fine", body: strings.Repeat("a", 8), wantReadOK: true},
		{name: "at cap reads fine", body: strings.Repeat("a", int(cap)), wantReadOK: true},
		{name: "over cap errors on read", body: strings.Repeat("a", int(cap)+1), wantReadOK: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inner := &countingHandler{}
			c := newConfig(WithMaxBodyBytes(cap))
			h := c.wrap(inner)

			req := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if inner.called != 1 {
				t.Fatalf("inner handler called %d times, want 1", inner.called)
			}
			gotReadOK := inner.readErr == nil
			if gotReadOK != tt.wantReadOK {
				t.Errorf("read ok = %v (err=%v), want %v", gotReadOK, inner.readErr, tt.wantReadOK)
			}
			if tt.wantReadOK && string(inner.bodyRead) != tt.body {
				t.Errorf("body read = %q, want %q", inner.bodyRead, tt.body)
			}
		})
	}
}
