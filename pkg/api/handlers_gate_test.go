package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/core/uuid"
)

// gatePrompt is the human-facing description the permission gate carries in these
// tests (BashRequest.Description() returns its Command verbatim).
const gatePrompt = "rm -rf /tmp/x"

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

// TestGateResolution is the core routing test: it proves POST
// /sessions/{sid}/gates/{tid} resolves an OPEN gate by pulling the producing
// LoopID from the supervisor registry (never the client body), validates the
// action against the gate Kind (fail-secure 409 on a mismatch), and dispatches
// Approve/Deny/ProvideAnswer with the tool-execution id. No SSE client is ever
// attached — the supervisor owns its own subscription.
func TestGateResolution(t *testing.T) {
	t.Parallel()

	const kindNone = ""

	tests := []struct {
		name        string
		gateKind    string // kindNone => feed no gate (tid stays unregistered)
		body        string
		approveErr  error
		sidOverride string
		tidOverride string
		wantStatus  int
		assert      func(t *testing.T, fa *fakeAgent, tid, lid uuid.UUID)
	}{
		{
			name: "approve permission gate routes registry LoopID", gateKind: kindPermission,
			body: `{"action":"approve","scope":0}`, wantStatus: http.StatusNoContent,
			assert: func(t *testing.T, fa *fakeAgent, tid, lid uuid.UUID) {
				called, loopID, callID, scope := fa.approveArgs()
				if !called {
					t.Fatal("Approve not called")
				}
				if loopID != lid {
					t.Errorf("Approve loopID = %v, want registry lid %v", loopID, lid)
				}
				if callID != tid {
					t.Errorf("Approve callID = %v, want tid %v", callID, tid)
				}
				if scope != tool.ScopeOnce {
					t.Errorf("Approve scope = %v, want ScopeOnce", scope)
				}
			},
		},
		{
			name: "approve default scope is ScopeOnce", gateKind: kindPermission,
			body: `{"action":"approve"}`, wantStatus: http.StatusNoContent,
			assert: func(t *testing.T, fa *fakeAgent, _, _ uuid.UUID) {
				called, _, _, scope := fa.approveArgs()
				if !called || scope != tool.ScopeOnce {
					t.Errorf("Approve called=%v scope=%v, want called=true scope=ScopeOnce", called, scope)
				}
			},
		},
		{
			name: "approve honors explicit session scope", gateKind: kindPermission,
			body: `{"action":"approve","scope":1}`, wantStatus: http.StatusNoContent,
			assert: func(t *testing.T, fa *fakeAgent, _, _ uuid.UUID) {
				_, _, _, scope := fa.approveArgs()
				if scope != tool.ScopeSession {
					t.Errorf("Approve scope = %v, want ScopeSession", scope)
				}
			},
		},
		{
			name: "deny permission gate routes registry LoopID", gateKind: kindPermission,
			body: `{"action":"deny"}`, wantStatus: http.StatusNoContent,
			assert: func(t *testing.T, fa *fakeAgent, tid, lid uuid.UUID) {
				called, loopID, callID := fa.denyArgs()
				if !called {
					t.Fatal("Deny not called")
				}
				if loopID != lid || callID != tid {
					t.Errorf("Deny (loopID,callID) = (%v,%v), want (%v,%v)", loopID, callID, lid, tid)
				}
			},
		},
		{
			name: "answer user-input gate routes registry LoopID", gateKind: kindUserInput,
			body: `{"action":"answer","answer":"blue"}`, wantStatus: http.StatusNoContent,
			assert: func(t *testing.T, fa *fakeAgent, tid, lid uuid.UUID) {
				called, loopID, callID, answer := fa.answerArgs()
				if !called {
					t.Fatal("ProvideAnswer not called")
				}
				if loopID != lid || callID != tid {
					t.Errorf("ProvideAnswer (loopID,callID) = (%v,%v), want (%v,%v)", loopID, callID, lid, tid)
				}
				if answer != "blue" {
					t.Errorf("ProvideAnswer answer = %q, want %q", answer, "blue")
				}
			},
		},
		{
			name: "unknown tool-execution id returns 404", gateKind: kindNone,
			body: `{"action":"approve"}`, wantStatus: http.StatusNotFound,
		},
		{
			name: "answer on a permission gate is a 409 mismatch", gateKind: kindPermission,
			body: `{"action":"answer","answer":"x"}`, wantStatus: http.StatusConflict,
			assert: func(t *testing.T, fa *fakeAgent, _, _ uuid.UUID) {
				ac, _, _, _ := fa.approveArgs()
				dc, _, _ := fa.denyArgs()
				an, _, _, _ := fa.answerArgs()
				if ac || dc || an {
					t.Error("a kind-mismatched action dispatched a control (want fail-secure: nothing dispatched)")
				}
			},
		},
		{
			name: "approve on a user-input gate is a 409 mismatch", gateKind: kindUserInput,
			body: `{"action":"approve"}`, wantStatus: http.StatusConflict,
		},
		{
			name: "unknown action is a 400", gateKind: kindPermission,
			body: `{"action":"frobnicate"}`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "out-of-range scope is a 400", gateKind: kindPermission,
			body: `{"action":"approve","scope":99}`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "negative scope is a 400", gateKind: kindPermission,
			body: `{"action":"approve","scope":-1}`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "malformed json is a 400", gateKind: kindPermission,
			body: `{"action":`, wantStatus: http.StatusBadRequest,
		},
		{
			name: "agent Approve error is a 500", gateKind: kindPermission,
			body: `{"action":"approve"}`, approveErr: errInterrupt, wantStatus: http.StatusInternalServerError,
		},
		{
			name: "unknown session returns 404", gateKind: kindNone,
			body: `{"action":"approve"}`, sidOverride: mkID(0xEE).String(), wantStatus: http.StatusNotFound,
		},
		{
			name: "malformed session id returns 400", gateKind: kindNone,
			body: `{"action":"approve"}`, sidOverride: "not-a-uuid", wantStatus: http.StatusBadRequest,
		},
		{
			name: "malformed tool-execution id returns 400", gateKind: kindNone,
			body: `{"action":"approve"}`, tidOverride: "not-a-uuid", wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newServer(Config{}, fakeFactory)
			ts := httptest.NewServer(s.handler())
			defer ts.Close()
			t.Cleanup(func() { stopAll(s) })

			fs := newFakeSub()
			fa := &fakeAgent{sub: fs, approveErr: tt.approveErr}
			sup := mustSupervisor(t, fa)
			sid, tid, lid := mkID(0xB1), mkID(0xC1), mkID(0xD1)
			s.putSession(sid, &sessionEntry{agent: fa, sup: sup})

			switch tt.gateKind {
			case kindPermission:
				fs.feed(event.PermissionRequested{Header: loopHeader(lid), ToolExecutionID: tid, Request: tool.BashRequest{Command: gatePrompt}})
			case kindUserInput:
				fs.feed(event.UserInputRequested{Header: loopHeader(lid), ToolExecutionID: tid, Question: "pick a color"})
			}
			if tt.gateKind != kindNone {
				if !pollUntil(t, func() bool { _, ok := sup.lookup(tid); return ok }) {
					t.Fatalf("gate %v not recorded within %v", tid, pollDeadline)
				}
			}

			sidPart := sid.String()
			if tt.sidOverride != "" {
				sidPart = tt.sidOverride
			}
			tidPart := tid.String()
			if tt.tidOverride != "" {
				tidPart = tt.tidOverride
			}
			resp := doReqBody(t, ts, http.MethodPost, "/sessions/"+sidPart+"/gates/"+tidPart, tt.body)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("POST gate status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.assert != nil {
				tt.assert(t, fa, tid, lid)
			}
		})
	}
}

// TestListGates proves GET /sessions/{sid}/gates returns the live open-gate set
// for a reconnecting client, emits an empty JSON array (never null) for a session
// with no open gates, fails secure with 503 when the supervisor's subscription
// has DIED (its registry is stale), 404s an unknown session, and 400s a malformed
// id.
func TestListGates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		setup          func(t *testing.T, s *server) (path string, tid, lid uuid.UUID)
		wantStatus     int
		wantGates      int
		wantEmptyArray bool
		wantMatch      bool
	}{
		{
			name: "one open gate is listed",
			setup: func(t *testing.T, s *server) (string, uuid.UUID, uuid.UUID) {
				fs := newFakeSub()
				fa := &fakeAgent{sub: fs}
				sup := mustSupervisor(t, fa)
				sid, tid, lid := mkID(0xB1), mkID(0xC1), mkID(0xD1)
				s.putSession(sid, &sessionEntry{agent: fa, sup: sup})
				fs.feed(event.PermissionRequested{Header: loopHeader(lid), ToolExecutionID: tid, Request: tool.BashRequest{Command: gatePrompt}})
				if !pollUntil(t, func() bool { _, ok := sup.lookup(tid); return ok }) {
					t.Fatalf("gate %v not recorded within %v", tid, pollDeadline)
				}
				return "/sessions/" + sid.String() + "/gates", tid, lid
			},
			wantStatus: http.StatusOK, wantGates: 1, wantMatch: true,
		},
		{
			name: "empty session yields an empty array",
			setup: func(t *testing.T, s *server) (string, uuid.UUID, uuid.UUID) {
				fa := &fakeAgent{sub: newFakeSub()}
				sup := mustSupervisor(t, fa)
				sid := mkID(0xB2)
				s.putSession(sid, &sessionEntry{agent: fa, sup: sup})
				return "/sessions/" + sid.String() + "/gates", uuid.UUID{}, uuid.UUID{}
			},
			wantStatus: http.StatusOK, wantGates: 0, wantEmptyArray: true,
		},
		{
			name: "dead supervisor fails secure with 503",
			setup: func(t *testing.T, s *server) (string, uuid.UUID, uuid.UUID) {
				fs := newFakeSub()
				fa := &fakeAgent{sub: fs}
				sup := mustSupervisor(t, fa)
				sid := mkID(0xB3)
				s.putSession(sid, &sessionEntry{agent: fa, sup: sup})
				fs.fail(errStreamLost)
				if !pollUntil(t, func() bool { return sup.exitError() != nil }) {
					t.Fatalf("supervisor did not record exit error within %v", pollDeadline)
				}
				return "/sessions/" + sid.String() + "/gates", uuid.UUID{}, uuid.UUID{}
			},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "unknown session returns 404",
			setup: func(_ *testing.T, _ *server) (string, uuid.UUID, uuid.UUID) {
				return "/sessions/" + mkID(0xEE).String() + "/gates", uuid.UUID{}, uuid.UUID{}
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "malformed session id returns 400",
			setup: func(_ *testing.T, _ *server) (string, uuid.UUID, uuid.UUID) {
				return "/sessions/not-a-uuid/gates", uuid.UUID{}, uuid.UUID{}
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

			path, tid, lid := tt.setup(t, s)
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
			var body gatesResponse
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode gatesResponse: %v", err)
			}
			if len(body.Gates) != tt.wantGates {
				t.Fatalf("gates len = %d, want %d", len(body.Gates), tt.wantGates)
			}
			if tt.wantEmptyArray && !strings.Contains(string(raw), `"gates":[]`) {
				t.Errorf("empty body = %q, want it to contain \"gates\":[] (not null)", raw)
			}
			if tt.wantMatch {
				want := gateView{ToolExecutionID: tid.String(), LoopID: lid.String(), Kind: kindPermission, Prompt: gatePrompt}
				if body.Gates[0] != want {
					t.Errorf("gate = %+v, want %+v", body.Gates[0], want)
				}
			}
		})
	}
}
