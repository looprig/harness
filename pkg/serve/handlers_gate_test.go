package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/session"
)

// gateRequest builds a POST to the gate-response route, stamping the {sid} and
// {gid} path values the mux would extract from the path template.
func gateRequest(sid, gid, body string, hasBody bool) *http.Request {
	path := "/v1/sessions/" + sid + "/gates/" + gid
	var req *http.Request
	if hasBody {
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(http.MethodPost, path, http.NoBody)
	}
	req.SetPathValue("sid", sid)
	req.SetPathValue("gid", gid)
	return req
}

// scopeValue extracts the "scope" field of a gate response's Values as its raw
// (unquoted) string, so a test can assert the permission scope round-trips as a
// STABLE STRING (once|session|workspace) and not a numeric enum.
func scopeValue(t *testing.T, resp gate.GateResponse) string {
	t.Helper()
	raw, ok := resp.Values["scope"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("scope is not a JSON string: %v (raw %s)", err, string(raw))
	}
	return s
}

func TestServerHandleGateResponse(t *testing.T) {
	t.Parallel()

	const sidStr = "77777777-7777-7777-7777-777777777777"
	const gidStr = "88888888-8888-8888-8888-888888888888"

	// approveBody carries an approve action with a "session" scope stable string
	// plus a BOGUS client-supplied source the server must ignore (stamp user).
	const approveBody = `{"action":"approve","values":{"scope":"session"},"source":{"kind":"model","reason":"spoofed"}}`
	const denyBody = `{"action":"deny","values":{"scope":"once"}}`
	const answerBody = `{"action":"answer","values":{"text":"blue"}}`

	tests := []struct {
		name       string
		sid        string
		gid        string
		attach     bool
		hasBody    bool
		body       string
		gateErr    error
		wantStatus int
		wantCalls  int
		// assertions applied only on a 202 (success) response.
		wantAction string
		wantScope  string
	}{
		{
			name:       "approve happy path stamps user source scope session",
			sid:        sidStr,
			gid:        gidStr,
			attach:     true,
			hasBody:    true,
			body:       approveBody,
			wantStatus: http.StatusAccepted,
			wantCalls:  1,
			wantAction: "approve",
			wantScope:  "session",
		},
		{
			name:       "deny happy path scope once",
			sid:        sidStr,
			gid:        gidStr,
			attach:     true,
			hasBody:    true,
			body:       denyBody,
			wantStatus: http.StatusAccepted,
			wantCalls:  1,
			wantAction: "deny",
			wantScope:  "once",
		},
		{
			name:       "answer happy path",
			sid:        sidStr,
			gid:        gidStr,
			attach:     true,
			hasBody:    true,
			body:       answerBody,
			wantStatus: http.StatusAccepted,
			wantCalls:  1,
			wantAction: "answer",
		},
		{
			name:       "gate not_found is 404",
			sid:        sidStr,
			gid:        gidStr,
			attach:     true,
			hasBody:    true,
			body:       approveBody,
			gateErr:    &session.GateError{Kind: session.GateNotFound},
			wantStatus: http.StatusNotFound,
			wantCalls:  1,
		},
		{
			name:       "gate action_invalid is 400",
			sid:        sidStr,
			gid:        gidStr,
			attach:     true,
			hasBody:    true,
			body:       approveBody,
			gateErr:    &session.GateError{Kind: session.GateActionInvalid},
			wantStatus: http.StatusBadRequest,
			wantCalls:  1,
		},
		{
			name:       "gate kind_mismatch is 400",
			sid:        sidStr,
			gid:        gidStr,
			attach:     true,
			hasBody:    true,
			body:       approveBody,
			gateErr:    &session.GateError{Kind: session.GateKindMismatch},
			wantStatus: http.StatusBadRequest,
			wantCalls:  1,
		},
		{
			name:       "gate not_ready is 409",
			sid:        sidStr,
			gid:        gidStr,
			attach:     true,
			hasBody:    true,
			body:       approveBody,
			gateErr:    &session.GateError{Kind: session.GateNotReady},
			wantStatus: http.StatusConflict,
			wantCalls:  1,
		},
		{
			name:       "gate capacity is 503",
			sid:        sidStr,
			gid:        gidStr,
			attach:     true,
			hasBody:    true,
			body:       approveBody,
			gateErr:    &session.GateError{Kind: session.GateCapacity},
			wantStatus: http.StatusServiceUnavailable,
			wantCalls:  1,
		},
		{
			name:       "gate append_failed is 500",
			sid:        sidStr,
			gid:        gidStr,
			attach:     true,
			hasBody:    true,
			body:       approveBody,
			gateErr:    &session.GateError{Kind: session.GateAppendFailed, Cause: errBoom},
			wantStatus: http.StatusInternalServerError,
			wantCalls:  1,
		},
		{
			name:       "non-gate error is 500",
			sid:        sidStr,
			gid:        gidStr,
			attach:     true,
			hasBody:    true,
			body:       approveBody,
			gateErr:    errBoom,
			wantStatus: http.StatusInternalServerError,
			wantCalls:  1,
		},
		{
			name:       "malformed sid is 400 respond not called",
			sid:        "not-a-uuid",
			gid:        gidStr,
			attach:     false,
			hasBody:    true,
			body:       approveBody,
			wantStatus: http.StatusBadRequest,
			wantCalls:  0,
		},
		{
			name:       "malformed gid is 400 respond not called",
			sid:        sidStr,
			gid:        "not-a-uuid",
			attach:     true,
			hasBody:    true,
			body:       approveBody,
			wantStatus: http.StatusBadRequest,
			wantCalls:  0,
		},
		{
			name:       "unknown session is 404 respond not called",
			sid:        sidStr,
			gid:        gidStr,
			attach:     false,
			hasBody:    true,
			body:       approveBody,
			wantStatus: http.StatusNotFound,
			wantCalls:  0,
		},
		{
			name:       "malformed body is 400 respond not called",
			sid:        sidStr,
			gid:        gidStr,
			attach:     true,
			hasBody:    true,
			body:       `{"action":`,
			wantStatus: http.StatusBadRequest,
			wantCalls:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sid := parseTestUUID(t, sidStr)
			gid := parseTestUUID(t, gidStr)
			sess := &fakeSession{respondGateErr: tt.gateErr}
			srv := newServer[*fakeSession](&fakeRunner{}, newConfig())
			if tt.attach {
				srv.registry.put(sid, sess)
			}

			req := gateRequest(tt.sid, tt.gid, tt.body, tt.hasBody)
			rec := httptest.NewRecorder()

			srv.handleGateResponse(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if sess.respondGateCalls != tt.wantCalls {
				t.Errorf("respondGateCalls = %d, want %d", sess.respondGateCalls, tt.wantCalls)
			}

			if tt.wantStatus == http.StatusAccepted {
				got := sess.respondGateResp
				if got.GateID != gid {
					t.Errorf("GateID = %v, want %v", got.GateID, gid)
				}
				if got.Action != tt.wantAction {
					t.Errorf("Action = %q, want %q", got.Action, tt.wantAction)
				}
				// The SERVER stamps user provenance; the client-supplied source
				// (a spoofed "model" in approveBody) must be ignored.
				if got.Source.Kind != gate.ResponseFromUser {
					t.Errorf("Source.Kind = %q, want %q (server must stamp user provenance)", got.Source.Kind, gate.ResponseFromUser)
				}
				if tt.wantScope != "" {
					if scope := scopeValue(t, got); scope != tt.wantScope {
						t.Errorf("Values[scope] = %q, want %q (stable string, not numeric)", scope, tt.wantScope)
					}
				}
				return
			}
			assertErrorEnvelope(t, rec)
		})
	}
}

// TestServerHandleGateResponseNoCauseLeak proves an internal cause never reaches
// the client body: an append_failed GateError wraps errBoom, yet the 500 body
// carries only the generic envelope message.
func TestServerHandleGateResponseNoCauseLeak(t *testing.T) {
	t.Parallel()

	const sidStr = "99999999-9999-9999-9999-999999999999"
	const gidStr = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	sid := parseTestUUID(t, sidStr)
	sess := &fakeSession{respondGateErr: &session.GateError{Kind: session.GateAppendFailed, Cause: errBoom}}
	srv := newServer[*fakeSession](&fakeRunner{}, newConfig())
	srv.registry.put(sid, sess)

	req := gateRequest(sidStr, gidStr, `{"action":"approve"}`, true)
	rec := httptest.NewRecorder()
	srv.handleGateResponse(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("response body leaked internal cause: %s", rec.Body.String())
	}
	assertErrorEnvelope(t, rec)
}
