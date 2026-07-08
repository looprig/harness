package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
)

// fakeSession is a test double for LiveSession: it records Submit and Interrupt
// calls and returns configured results / errors. The subscribe/gate methods
// satisfy the interface but are unused by the lifecycle/control routes.
type fakeSession struct {
	submitID     uuid.UUID
	submitErr    error
	submitCalls  int
	submitBlocks []content.Block

	interruptResult bool
	interruptErr    error
	interruptCalls  int

	respondGateErr   error
	respondGateCalls int
	respondGateResp  gate.GateResponse
}

func (f *fakeSession) Submit(_ context.Context, blocks []content.Block) (uuid.UUID, error) {
	f.submitCalls++
	f.submitBlocks = blocks
	return f.submitID, f.submitErr
}

func (f *fakeSession) SubscribeEvents(event.EventFilter) (event.Subscription, error) {
	return nil, nil
}

func (f *fakeSession) RespondGate(_ context.Context, resp gate.GateResponse) error {
	f.respondGateCalls++
	f.respondGateResp = resp
	return f.respondGateErr
}

func (f *fakeSession) Interrupt(context.Context) (bool, error) {
	f.interruptCalls++
	return f.interruptResult, f.interruptErr
}

// fakeRunner is a test double for Runner[*fakeSession]: Run/Restore return the
// configured id/session/error and count their calls, and Restore records the id it
// was asked to rebuild.
type fakeRunner struct {
	runID    uuid.UUID
	runSess  *fakeSession
	runErr   error
	runCalls int

	restoreSess  *fakeSession
	restoreErr   error
	restoreCalls int
	restoreGotID uuid.UUID
}

func (f *fakeRunner) Run(context.Context) (uuid.UUID, *fakeSession, error) {
	f.runCalls++
	return f.runID, f.runSess, f.runErr
}

func (f *fakeRunner) Restore(_ context.Context, id uuid.UUID) (*fakeSession, error) {
	f.restoreCalls++
	f.restoreGotID = id
	return f.restoreSess, f.restoreErr
}

// parseTestUUID parses a canonical UUID for tests, failing the test on malformed input.
func parseTestUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := id.UnmarshalText([]byte(s)); err != nil {
		t.Fatalf("parseTestUUID(%q): %v", s, err)
	}
	return id
}

// validBlocksBody is a well-formed create body carrying a single text block.
const validBlocksBody = `{"blocks":[{"type":"text","Text":"hello"}]}`

// errBoom is a stand-in runner/session failure cause.
var errBoom = errors.New("boom")

func TestServerHandleCreate(t *testing.T) {
	t.Parallel()

	const runIDStr = "11111111-1111-1111-1111-111111111111"
	const cmdIDStr = "22222222-2222-2222-2222-222222222222"

	tests := []struct {
		name            string
		hasBody         bool
		body            string
		runErr          error
		submitErr       error
		wantStatus      int
		wantCmdID       bool
		wantAttached    bool
		wantRunCalls    int
		wantSubmitCalls int
	}{
		{
			name:            "idle create no body",
			hasBody:         false,
			wantStatus:      http.StatusCreated,
			wantCmdID:       false,
			wantAttached:    true,
			wantRunCalls:    1,
			wantSubmitCalls: 0,
		},
		{
			name:            "idle create empty object",
			hasBody:         true,
			body:            `{}`,
			wantStatus:      http.StatusCreated,
			wantCmdID:       false,
			wantAttached:    true,
			wantRunCalls:    1,
			wantSubmitCalls: 0,
		},
		{
			name:            "idle create empty blocks array",
			hasBody:         true,
			body:            `{"blocks":[]}`,
			wantStatus:      http.StatusCreated,
			wantCmdID:       false,
			wantAttached:    true,
			wantRunCalls:    1,
			wantSubmitCalls: 0,
		},
		{
			name:            "create with blocks",
			hasBody:         true,
			body:            validBlocksBody,
			wantStatus:      http.StatusCreated,
			wantCmdID:       true,
			wantAttached:    true,
			wantRunCalls:    1,
			wantSubmitCalls: 1,
		},
		{
			name:            "runner run error is 500 nothing attached",
			hasBody:         false,
			runErr:          errBoom,
			wantStatus:      http.StatusInternalServerError,
			wantAttached:    false,
			wantRunCalls:    1,
			wantSubmitCalls: 0,
		},
		{
			name:            "submit error is 500 session stays attached",
			hasBody:         true,
			body:            validBlocksBody,
			submitErr:       errBoom,
			wantStatus:      http.StatusInternalServerError,
			wantAttached:    true,
			wantRunCalls:    1,
			wantSubmitCalls: 1,
		},
		{
			name:            "malformed json envelope is 400 run not called",
			hasBody:         true,
			body:            `{"blocks":`,
			wantStatus:      http.StatusBadRequest,
			wantAttached:    false,
			wantRunCalls:    0,
			wantSubmitCalls: 0,
		},
		{
			name:            "malformed blocks is 400 run not called",
			hasBody:         true,
			body:            `{"blocks":[{"type":"nope"}]}`,
			wantStatus:      http.StatusBadRequest,
			wantAttached:    false,
			wantRunCalls:    0,
			wantSubmitCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runID := parseTestUUID(t, runIDStr)
			cmdID := parseTestUUID(t, cmdIDStr)
			sess := &fakeSession{submitID: cmdID, submitErr: tt.submitErr}
			runner := &fakeRunner{runID: runID, runSess: sess, runErr: tt.runErr}
			srv := newServer[*fakeSession](runner, newConfig())

			var req *http.Request
			if tt.hasBody {
				req = httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(tt.body))
			} else {
				req = httptest.NewRequest(http.MethodPost, "/v1/sessions", http.NoBody)
			}
			rec := httptest.NewRecorder()

			srv.handleCreate(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if runner.runCalls != tt.wantRunCalls {
				t.Errorf("runCalls = %d, want %d", runner.runCalls, tt.wantRunCalls)
			}
			if sess.submitCalls != tt.wantSubmitCalls {
				t.Errorf("submitCalls = %d, want %d", sess.submitCalls, tt.wantSubmitCalls)
			}

			// Attachment: the session is resolvable in the registry iff expected.
			got, ok := srv.registry.get(runID)
			if ok != tt.wantAttached {
				t.Errorf("registry attached = %v, want %v", ok, tt.wantAttached)
			}
			if tt.wantAttached && got != sess {
				t.Errorf("registry returned %v, want the fake session", got)
			}

			if tt.wantStatus == http.StatusCreated {
				var resp createResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode 201 body: %v", err)
				}
				if resp.SessionID != runID {
					t.Errorf("session_id = %v, want %v", resp.SessionID, runID)
				}
				if tt.wantCmdID {
					if resp.CommandID == nil || *resp.CommandID != cmdID {
						t.Errorf("command_id = %v, want %v", resp.CommandID, cmdID)
					}
					// Submit received the decoded blocks (submit-after-attach).
					if len(sess.submitBlocks) != 1 {
						t.Errorf("submit blocks len = %d, want 1", len(sess.submitBlocks))
					}
				} else if resp.CommandID != nil {
					t.Errorf("command_id = %v, want absent", *resp.CommandID)
				}
			} else {
				assertErrorEnvelope(t, rec)
			}
		})
	}
}

func TestServerHandleRestore(t *testing.T) {
	t.Parallel()

	const sidStr = "33333333-3333-3333-3333-333333333333"

	tests := []struct {
		name             string
		sid              string
		restoreErr       error
		wantStatus       int
		wantAttached     bool
		wantRestoreCalls int
	}{
		{
			name:             "restore happy path",
			sid:              sidStr,
			wantStatus:       http.StatusOK,
			wantAttached:     true,
			wantRestoreCalls: 1,
		},
		{
			name:             "malformed sid is 400 restore not called",
			sid:              "not-a-uuid",
			wantStatus:       http.StatusBadRequest,
			wantAttached:     false,
			wantRestoreCalls: 0,
		},
		{
			name:             "restore generic error is 500",
			sid:              sidStr,
			restoreErr:       errBoom,
			wantStatus:       http.StatusInternalServerError,
			wantAttached:     false,
			wantRestoreCalls: 1,
		},
		{
			name:             "restore not-found error is 404",
			sid:              sidStr,
			restoreErr:       SessionNotFoundError{SessionID: parseTestUUID(t, sidStr)},
			wantStatus:       http.StatusNotFound,
			wantAttached:     false,
			wantRestoreCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sess := &fakeSession{}
			runner := &fakeRunner{restoreSess: sess, restoreErr: tt.restoreErr}
			srv := newServer[*fakeSession](runner, newConfig())

			req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+tt.sid+"/restore", http.NoBody)
			req.SetPathValue("sid", tt.sid)
			rec := httptest.NewRecorder()

			srv.handleRestore(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if runner.restoreCalls != tt.wantRestoreCalls {
				t.Errorf("restoreCalls = %d, want %d", runner.restoreCalls, tt.wantRestoreCalls)
			}
			if tt.wantRestoreCalls > 0 && runner.restoreGotID != parseTestUUID(t, sidStr) {
				t.Errorf("restore got id = %v, want %v", runner.restoreGotID, sidStr)
			}

			sid := parseTestUUID(t, sidStr)
			got, ok := srv.registry.get(sid)
			if ok != tt.wantAttached {
				t.Errorf("registry attached = %v, want %v", ok, tt.wantAttached)
			}
			if tt.wantAttached {
				if got != sess {
					t.Errorf("registry returned %v, want the fake session", got)
				}
				var resp restoreResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode 200 body: %v", err)
				}
				if resp.SessionID != sid {
					t.Errorf("session_id = %v, want %v", resp.SessionID, sid)
				}
			} else if tt.wantStatus != http.StatusOK {
				assertErrorEnvelope(t, rec)
			}
		})
	}
}

// assertErrorEnvelope verifies the response body is the nested error envelope with a
// non-empty code and message (the generic, client-safe shape).
func assertErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var env errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, rec.Body.String())
	}
	if env.Error.Code == "" {
		t.Errorf("error code is empty (body %s)", rec.Body.String())
	}
	if env.Error.Message == "" {
		t.Errorf("error message is empty (body %s)", rec.Body.String())
	}
}
