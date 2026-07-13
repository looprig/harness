package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// wantFeatures is the exact, ordered feature list the capabilities endpoint
// advertises (SPEC §6). The order is part of the contract.
var wantFeatures = []string{"journal", "live_sse", "ephemeral_sse", "gate_response"}

// TestHandleCapabilities drives the handler in isolation and asserts the static
// discovery document: 200, application/json, and every field of the typed
// response — protocol, version, and the exact ordered feature slice.
func TestHandleCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		wantProtocol string
		wantVersion  int
		wantFeatures []string
	}{
		{
			name:         "static discovery document",
			wantProtocol: "looprig.serve",
			wantVersion:  1,
			wantFeatures: wantFeatures,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := newServer[*fakeSession, fakeSessionOption](nil, nil, newConfig())
			req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", http.NoBody)
			rec := httptest.NewRecorder()

			srv.handleCapabilities(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != contentTypeJSON {
				t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSON)
			}

			var got capabilities
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode body: %v (body %s)", err, rec.Body.String())
			}
			if got.Protocol != tt.wantProtocol {
				t.Errorf("Protocol = %q, want %q", got.Protocol, tt.wantProtocol)
			}
			if got.Version != tt.wantVersion {
				t.Errorf("Version = %d, want %d", got.Version, tt.wantVersion)
			}
			if !reflect.DeepEqual(got.Features, tt.wantFeatures) {
				t.Errorf("Features = %v, want %v", got.Features, tt.wantFeatures)
			}
		})
	}
}

// TestHandleCapabilitiesExactJSON pins the wire bytes so a field rename, reorder,
// or value drift is caught at the serialization boundary, not just the struct.
func TestHandleCapabilitiesExactJSON(t *testing.T) {
	t.Parallel()

	srv := newServer[*fakeSession, fakeSessionOption](nil, nil, newConfig())
	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", http.NoBody)
	rec := httptest.NewRecorder()

	srv.handleCapabilities(rec, req)

	const want = `{"protocol":"looprig.serve","version":1,"features":["journal","live_sse","ephemeral_sse","gate_response"]}`
	if got := trimNewline(rec.Body.String()); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

// trimNewline drops the single trailing newline json.Encoder appends so the body
// can be compared to a canonical one-line document.
func trimNewline(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\n' {
		return s[:n-1]
	}
	return s
}

// TestHandleCapabilitiesWiredThroughMux proves the route is registered on the
// assembled Handler: GET reaches the handler (200 + exact body) and a wrong
// method on the same path is resolved to 405 by the ServeMux before any handler
// runs.
func TestHandleCapabilitiesWiredThroughMux(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		wantStatus int
		wantMuxErr bool // expect the plain ServeMux response, not the handler's JSON
	}{
		{name: "GET reaches handler", method: http.MethodGet, wantStatus: http.StatusOK},
		{name: "POST is 405 from mux", method: http.MethodPost, wantStatus: http.StatusMethodNotAllowed, wantMuxErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := muxFixtures(t)
			req := httptest.NewRequest(tt.method, "/v1/capabilities", http.NoBody)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantMuxErr {
				return
			}

			var got capabilities
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode body: %v (body %s)", err, rec.Body.String())
			}
			if got.Protocol != "looprig.serve" || got.Version != 1 || !reflect.DeepEqual(got.Features, wantFeatures) {
				t.Errorf("capabilities = %+v, want protocol=looprig.serve version=1 features=%v", got, wantFeatures)
			}
		})
	}
}
