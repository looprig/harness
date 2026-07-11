package serve

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// muxFixtures builds a Handler over fakes wired for routing tests: Run/Restore
// succeed and the reader returns non-error DTOs, so a request that reaches the
// intended handler produces a 2xx (proving routing) rather than the handler's
// own error path.
func muxFixtures(t *testing.T, opts ...Option) http.Handler {
	t.Helper()
	runID := parseTestUUID(t, "11111111-1111-1111-1111-111111111111")
	sess := &fakeSession{submitID: parseTestUUID(t, "22222222-2222-2222-2222-222222222222")}
	rig := &fakeRig{runID: runID, runSess: sess, restoreSess: sess}
	reader := &fakeReader{}
	return Handler[*fakeSession](rig, reader, opts...)
}

// TestHandlerRouting drives requests through the assembled mux and asserts each of
// the nine routes reaches the intended handler (2xx or the handler's own typed
// 404), while method/path mismatches are resolved by the ServeMux (405 / plain
// 404) BEFORE any handler runs.
func TestHandlerRouting(t *testing.T) {
	t.Parallel()

	const sid = "33333333-3333-3333-3333-333333333333"
	const gid = "44444444-4444-4444-4444-444444444444"

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantCode   string // non-empty => expect the JSON error envelope with this code
		muxMiss    bool   // expect the ServeMux plain-text 404/405, not a serve handler
	}{
		{name: "create reaches handleCreate", method: http.MethodPost, path: "/v1/sessions", body: validBlocksBody, wantStatus: http.StatusCreated},
		{name: "list reaches handleListSessions", method: http.MethodGet, path: "/v1/sessions", wantStatus: http.StatusOK},
		{name: "restore reaches handleRestore", method: http.MethodPost, path: "/v1/sessions/" + sid + "/restore", wantStatus: http.StatusOK},
		{name: "input reaches handleInput", method: http.MethodPost, path: "/v1/sessions/" + sid + "/input", body: validBlocksBody, wantStatus: http.StatusNotFound, wantCode: codeNotFound},
		{name: "interrupt reaches handleInterrupt", method: http.MethodPost, path: "/v1/sessions/" + sid + "/interrupt", wantStatus: http.StatusNotFound, wantCode: codeNotFound},
		{name: "gate reaches handleGateResponse", method: http.MethodPost, path: "/v1/sessions/" + sid + "/gates/" + gid, wantStatus: http.StatusNotFound, wantCode: codeNotFound},
		{name: "events reaches handleEvents", method: http.MethodGet, path: "/v1/sessions/" + sid + "/events", wantStatus: http.StatusNotFound, wantCode: codeNotFound},
		{name: "status reaches handleStatus", method: http.MethodGet, path: "/v1/sessions/" + sid + "/status", wantStatus: http.StatusOK},
		{name: "journal reaches handleJournal", method: http.MethodGet, path: "/v1/sessions/" + sid + "/journal", wantStatus: http.StatusOK},
		{name: "wrong method on collection is 405", method: http.MethodDelete, path: "/v1/sessions", wantStatus: http.StatusMethodNotAllowed, muxMiss: true},
		{name: "wrong method on events is 405", method: http.MethodPost, path: "/v1/sessions/" + sid + "/events", wantStatus: http.StatusMethodNotAllowed, muxMiss: true},
		{name: "unknown path is 404", method: http.MethodGet, path: "/v1/unknown", wantStatus: http.StatusNotFound, muxMiss: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := muxFixtures(t)
			var body *strings.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(tt.method, tt.path, body)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" {
				var env errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
					t.Fatalf("expected JSON error envelope, decode failed: %v (body %s)", err, rec.Body.String())
				}
				if env.Error.Code != tt.wantCode {
					t.Errorf("error code = %q, want %q", env.Error.Code, tt.wantCode)
				}
			}
			if tt.muxMiss {
				// The ServeMux default responses are plain text, never the JSON
				// envelope — confirm no serve handler produced this response.
				if strings.Contains(rec.Header().Get("Content-Type"), contentTypeJSON) {
					t.Errorf("mux-miss produced JSON body, want plain ServeMux response (body %s)", rec.Body.String())
				}
			}
		})
	}
}

// TestHandlerAuthApplied proves the wrap middleware is installed by Handler: an
// authenticator that rejects yields 401 before any route handler runs, while one
// that accepts lets the request through to the handler.
func TestHandlerAuthApplied(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		authn      func(*http.Request) error
		wantStatus int
	}{
		{name: "no auth passes through", authn: nil, wantStatus: http.StatusOK},
		{name: "failing auth is 401", authn: func(*http.Request) error { return errors.New("denied") }, wantStatus: http.StatusUnauthorized},
		{name: "passing auth reaches handler", authn: func(*http.Request) error { return nil }, wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var opts []Option
			if tt.authn != nil {
				opts = append(opts, WithAuth(tt.authn))
			}
			h := muxFixtures(t, opts...)

			req := httptest.NewRequest(http.MethodGet, "/v1/sessions", http.NoBody)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestHandlerCarriesAuthBit pins the has-auth carrier: the concrete handler
// Handler returns satisfies authAware and reports whether an authenticator was
// installed, so Server's fail-secure bind reads the bit without re-parsing opts.
func TestHandlerCarriesAuthBit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []Option
		want bool
	}{
		{name: "no auth reports false", opts: nil, want: false},
		{name: "with auth reports true", opts: []Option{WithAuth(func(*http.Request) error { return nil })}, want: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := muxFixtures(t, tt.opts...)
			aware, ok := h.(authAware)
			if !ok {
				t.Fatalf("Handler result does not satisfy authAware")
			}
			if got := aware.authInstalled(); got != tt.want {
				t.Errorf("authInstalled() = %v, want %v", got, tt.want)
			}
		})
	}
}
