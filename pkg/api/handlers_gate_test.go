package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
)

// doReqBody issues method+path against ts with a JSON body and returns the
// response; the caller closes the body. It fatals on a construction/transport
// error so each subtest stays terse.
func doReqBody(t *testing.T, ts *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest(%s %s) error = %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do(%s %s) error = %v", method, path, err)
	}
	return resp
}

// mustSupervisor starts a supervisor over fa's sub, failing the test on error and
// registering an idempotent stop() cleanup so no run goroutine leaks the subtest.
func mustSupervisor(t *testing.T, fa *fakeAgent) *supervisor {
	t.Helper()
	sup, err := newSupervisor(fa)
	if err != nil {
		t.Fatalf("newSupervisor() error = %v", err)
	}
	t.Cleanup(func() { _ = sup.stop() })
	return sup
}

// TestInput proves POST /sessions/{sid}/input decodes the wire blocks via
// content.UnmarshalBlocks, submits them, and echoes the agent's command id as
// input_id — and that every decode/empty failure fails secure with 400, a Submit
// error is a 500, an unknown session is 404, and a malformed id is 400.
func TestInput(t *testing.T) {
	t.Parallel()

	submitID := mkID(0xAB)

	tests := []struct {
		name        string
		body        string
		submitErr   error
		sidOverride string // raw sid in the URL when set (unknown/malformed paths)
		wantStatus  int
		wantSubmit  bool // assert the decoded block reached Submit and input_id echoes submitID
	}{
		{name: "valid text block submits", body: `{"blocks":[{"type":"text","Text":"hi"}]}`, wantStatus: http.StatusOK, wantSubmit: true},
		{name: "malformed json rejected", body: `{"blocks":`, wantStatus: http.StatusBadRequest},
		{name: "empty blocks rejected", body: `{"blocks":[]}`, wantStatus: http.StatusBadRequest},
		{name: "missing blocks key rejected", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "unknown block type rejected", body: `{"blocks":[{"type":"bogus"}]}`, wantStatus: http.StatusBadRequest},
		{name: "submit error returns 500", body: `{"blocks":[{"type":"text","Text":"hi"}]}`, submitErr: errInterrupt, wantStatus: http.StatusInternalServerError},
		{name: "unknown session returns 404", body: `{"blocks":[{"type":"text","Text":"hi"}]}`, sidOverride: mkID(0xEE).String(), wantStatus: http.StatusNotFound},
		{name: "malformed session id returns 400", body: `{"blocks":[{"type":"text","Text":"hi"}]}`, sidOverride: "not-a-uuid", wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newServer(Config{}, fakeFactory)
			ts := httptest.NewServer(s.handler())
			defer ts.Close()
			t.Cleanup(func() { stopAll(s) })

			fa := &fakeAgent{sub: newFakeSub(), submitID: submitID, submitErr: tt.submitErr}
			sid := mkID(0xB0)
			s.putSession(sid, &sessionEntry{agent: fa, sup: mustSupervisor(t, fa)})

			sidPart := sid.String()
			if tt.sidOverride != "" {
				sidPart = tt.sidOverride
			}
			resp := doReqBody(t, ts, http.MethodPost, "/sessions/"+sidPart+"/input", tt.body)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("POST input status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if !tt.wantSubmit {
				return
			}

			var body inputResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode inputResponse: %v", err)
			}
			if body.InputID != submitID.String() {
				t.Errorf("input_id = %q, want %q", body.InputID, submitID.String())
			}
			blocks := fa.submittedBlocksSnapshot()
			if len(blocks) != 1 {
				t.Fatalf("Submit got %d blocks, want 1", len(blocks))
			}
			tb, ok := blocks[0].(*content.TextBlock)
			if !ok {
				t.Fatalf("Submit block type = %T, want *content.TextBlock", blocks[0])
			}
			if tb.Text != "hi" {
				t.Errorf("Submit block text = %q, want %q", tb.Text, "hi")
			}
		})
	}
}

// TestGateResolution proves POST /sessions/{sid}/gates/{gid} treats {gid} as the
// opaque gate.ID, decodes the generic gate.ResponseRequest body, stamps a user
// source, and delegates the complete response to Agent.RespondGate. The API does
// not inspect gate kind and does not consult an open-gate registry.
func TestGateResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		respondErr  error
		sidOverride string
		gidOverride string
		wantStatus  int
		assert      func(t *testing.T, fa *fakeAgent, gid gate.ID)
	}{
		{
			name: "response request is delegated with gate id and user source",
			body: `{"action":"approve","values":{"scope":1,"accepted_grants":["grant-a"]}}`, wantStatus: http.StatusAccepted,
			assert: func(t *testing.T, fa *fakeAgent, gid gate.ID) {
				called, got := fa.respondGateArgs()
				if !called {
					t.Fatal("RespondGate not called")
				}
				if got.GateID != gid {
					t.Errorf("RespondGate GateID = %v, want %v", got.GateID, gid)
				}
				if got.Action != "approve" {
					t.Errorf("RespondGate Action = %q, want approve", got.Action)
				}
				if got.Source.Kind != gate.ResponseFromUser {
					t.Errorf("RespondGate Source.Kind = %q, want %q", got.Source.Kind, gate.ResponseFromUser)
				}
				if got.Source.Reason != "" {
					t.Errorf("RespondGate Source.Reason = %q, want empty", got.Source.Reason)
				}
				if got := string(got.Values["scope"]); got != "1" {
					t.Errorf("RespondGate Values[scope] = %s, want 1", got)
				}
				if got := string(got.Values["accepted_grants"]); got != `["grant-a"]` {
					t.Errorf("RespondGate Values[accepted_grants] = %s, want [\"grant-a\"]", got)
				}
			},
		},
		{
			name: "malformed json is a 400",
			body: `{"action":`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "agent RespondGate error is a 500",
			body: `{"action":"approve"}`, respondErr: errInterrupt, wantStatus: http.StatusInternalServerError,
		},
		{
			name: "unknown session returns 404",
			body: `{"action":"approve"}`, sidOverride: mkID(0xEE).String(), wantStatus: http.StatusNotFound,
		},
		{
			name: "malformed session id returns 400",
			body: `{"action":"approve"}`, sidOverride: "not-a-uuid", wantStatus: http.StatusBadRequest,
		},
		{
			name: "malformed gate id returns 400",
			body: `{"action":"approve"}`, gidOverride: "not-a-uuid", wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newServer(Config{}, fakeFactory)
			ts := httptest.NewServer(s.handler())
			defer ts.Close()
			t.Cleanup(func() { stopAll(s) })

			fa := &fakeAgent{sub: newFakeSub(), respondGateErr: tt.respondErr}
			sid, gid := mkID(0xB1), gate.ID(mkID(0xC1))
			s.putSession(sid, &sessionEntry{agent: fa, sup: mustSupervisor(t, fa)})

			sidPart := sid.String()
			if tt.sidOverride != "" {
				sidPart = tt.sidOverride
			}
			gidPart := gid.String()
			if tt.gidOverride != "" {
				gidPart = tt.gidOverride
			}
			resp := doReqBody(t, ts, http.MethodPost, "/sessions/"+sidPart+"/gates/"+gidPart, tt.body)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("POST gate status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.assert != nil {
				tt.assert(t, fa, gid)
			}
		})
	}
}

// TestListGates proves GET /sessions/{sid}/gates returns Agent.ListGates(ctx)
// rather than an API-maintained shadow registry, emits an empty JSON array (never
// null), 404s an unknown session, and 400s a malformed id.
func TestListGates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		setup          func(t *testing.T, s *server) (path string, want []gate.Gate)
		wantStatus     int
		wantEmptyArray bool
	}{
		{
			name: "agent open gates are listed",
			setup: func(t *testing.T, s *server) (string, []gate.Gate) {
				want := []gate.Gate{{
					ID:   gate.ID(mkID(0xC1)),
					Kind: gate.KindPermission,
					Prompt: gate.Prompt{
						Body: "approve command?",
					},
				}}
				fa := &fakeAgent{sub: newFakeSub(), listGates: want}
				sid := mkID(0xB1)
				s.putSession(sid, &sessionEntry{agent: fa, sup: mustSupervisor(t, fa)})
				return "/sessions/" + sid.String() + "/gates", want
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "empty session yields an empty array",
			setup: func(t *testing.T, s *server) (string, []gate.Gate) {
				fa := &fakeAgent{sub: newFakeSub(), listGates: nil}
				sup := mustSupervisor(t, fa)
				sid := mkID(0xB2)
				s.putSession(sid, &sessionEntry{agent: fa, sup: sup})
				return "/sessions/" + sid.String() + "/gates", nil
			},
			wantStatus: http.StatusOK, wantEmptyArray: true,
		},
		{
			name: "unknown session returns 404",
			setup: func(_ *testing.T, _ *server) (string, []gate.Gate) {
				return "/sessions/" + mkID(0xEE).String() + "/gates", nil
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "malformed session id returns 400",
			setup: func(_ *testing.T, _ *server) (string, []gate.Gate) {
				return "/sessions/not-a-uuid/gates", nil
			},
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newServer(Config{}, fakeFactory)
			ts := httptest.NewServer(s.handler())
			defer ts.Close()
			t.Cleanup(func() { stopAll(s) })

			path, want := tt.setup(t, s)
			resp := doReq(t, ts, http.MethodGet, path)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("GET gates status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantStatus != http.StatusOK {
				return
			}

			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll(gates body) error = %v", err)
			}
			var body struct {
				Gates []gate.Gate `json:"gates"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode gatesResponse: %v", err)
			}
			if len(body.Gates) != len(want) {
				t.Fatalf("gates len = %d, want %d", len(body.Gates), len(want))
			}
			if tt.wantEmptyArray && !strings.Contains(string(raw), `"gates":[]`) {
				t.Errorf("empty body = %q, want it to contain \"gates\":[] (not null)", raw)
			}
			if len(want) > 0 && body.Gates[0].ID != want[0].ID {
				t.Errorf("gate id = %v, want %v", body.Gates[0].ID, want[0].ID)
			}
		})
	}
}
