package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// controlRequest builds a POST request to path with an optional body, stamping
// the {sid} path value the way the mux would.
func controlRequest(path, sid, body string, hasBody bool) *http.Request {
	var req *http.Request
	if hasBody {
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(http.MethodPost, path, http.NoBody)
	}
	req.SetPathValue("sid", sid)
	return req
}

func TestServerHandleInput(t *testing.T) {
	t.Parallel()

	const sidStr = "44444444-4444-4444-4444-444444444444"
	const cmdIDStr = "55555555-5555-5555-5555-555555555555"

	tests := []struct {
		name            string
		sid             string
		attach          bool // register the session under sidStr before the call
		hasBody         bool
		body            string
		submitErr       error
		wantStatus      int
		wantCmdID       bool
		wantSubmitCalls int
		wantBlocksLen   int
	}{
		{
			name:            "input happy path",
			sid:             sidStr,
			attach:          true,
			hasBody:         true,
			body:            validBlocksBody,
			wantStatus:      http.StatusOK,
			wantCmdID:       true,
			wantSubmitCalls: 1,
			wantBlocksLen:   1,
		},
		{
			name:            "malformed sid is 400 submit not called",
			sid:             "not-a-uuid",
			attach:          false,
			hasBody:         true,
			body:            validBlocksBody,
			wantStatus:      http.StatusBadRequest,
			wantSubmitCalls: 0,
		},
		{
			name:            "unknown session is 404 submit not called",
			sid:             sidStr,
			attach:          false,
			hasBody:         true,
			body:            validBlocksBody,
			wantStatus:      http.StatusNotFound,
			wantSubmitCalls: 0,
		},
		{
			name:            "empty blocks array is 400 submit not called",
			sid:             sidStr,
			attach:          true,
			hasBody:         true,
			body:            `{"blocks":[]}`,
			wantStatus:      http.StatusBadRequest,
			wantSubmitCalls: 0,
		},
		{
			name:            "empty object is 400 submit not called",
			sid:             sidStr,
			attach:          true,
			hasBody:         true,
			body:            `{}`,
			wantStatus:      http.StatusBadRequest,
			wantSubmitCalls: 0,
		},
		{
			name:            "no body is 400 submit not called",
			sid:             sidStr,
			attach:          true,
			hasBody:         false,
			wantStatus:      http.StatusBadRequest,
			wantSubmitCalls: 0,
		},
		{
			name:            "malformed json envelope is 400 submit not called",
			sid:             sidStr,
			attach:          true,
			hasBody:         true,
			body:            `{"blocks":`,
			wantStatus:      http.StatusBadRequest,
			wantSubmitCalls: 0,
		},
		{
			name:            "malformed blocks is 400 submit not called",
			sid:             sidStr,
			attach:          true,
			hasBody:         true,
			body:            `{"blocks":[{"type":"nope"}]}`,
			wantStatus:      http.StatusBadRequest,
			wantSubmitCalls: 0,
		},
		{
			name:            "submit error is 500",
			sid:             sidStr,
			attach:          true,
			hasBody:         true,
			body:            validBlocksBody,
			submitErr:       errBoom,
			wantStatus:      http.StatusInternalServerError,
			wantSubmitCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sid := parseTestUUID(t, sidStr)
			cmdID := parseTestUUID(t, cmdIDStr)
			sess := &fakeSession{submitID: cmdID, submitErr: tt.submitErr}
			rig := &fakeRig{}
			srv := newServer[*fakeSession](rig, nil, newConfig())
			if tt.attach {
				srv.registry.put(sid, sess)
			}

			req := controlRequest("/v1/sessions/"+tt.sid+"/input", tt.sid, tt.body, tt.hasBody)
			rec := httptest.NewRecorder()

			srv.handleInput(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if sess.submitCalls != tt.wantSubmitCalls {
				t.Errorf("submitCalls = %d, want %d", sess.submitCalls, tt.wantSubmitCalls)
			}

			if tt.wantStatus == http.StatusOK {
				var resp inputResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode 200 body: %v", err)
				}
				if tt.wantCmdID && resp.CommandID != cmdID {
					t.Errorf("command_id = %v, want %v", resp.CommandID, cmdID)
				}
				if len(sess.submitBlocks) != tt.wantBlocksLen {
					t.Errorf("submit blocks len = %d, want %d", len(sess.submitBlocks), tt.wantBlocksLen)
				}
			} else {
				assertErrorEnvelope(t, rec)
			}
		})
	}
}

func TestServerHandleInterrupt(t *testing.T) {
	t.Parallel()

	const sidStr = "66666666-6666-6666-6666-666666666666"

	tests := []struct {
		name               string
		sid                string
		attach             bool
		interruptResult    bool
		interruptErr       error
		wantStatus         int
		wantInterrupted    bool
		wantInterruptCalls int
	}{
		{
			name:               "interrupt happy interrupted true",
			sid:                sidStr,
			attach:             true,
			interruptResult:    true,
			wantStatus:         http.StatusOK,
			wantInterrupted:    true,
			wantInterruptCalls: 1,
		},
		{
			name:               "interrupt happy interrupted false",
			sid:                sidStr,
			attach:             true,
			interruptResult:    false,
			wantStatus:         http.StatusOK,
			wantInterrupted:    false,
			wantInterruptCalls: 1,
		},
		{
			name:               "malformed sid is 400 interrupt not called",
			sid:                "not-a-uuid",
			attach:             false,
			wantStatus:         http.StatusBadRequest,
			wantInterruptCalls: 0,
		},
		{
			name:               "unknown session is 404 interrupt not called",
			sid:                sidStr,
			attach:             false,
			wantStatus:         http.StatusNotFound,
			wantInterruptCalls: 0,
		},
		{
			name:               "interrupt error is 500",
			sid:                sidStr,
			attach:             true,
			interruptErr:       errBoom,
			wantStatus:         http.StatusInternalServerError,
			wantInterruptCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sid := parseTestUUID(t, sidStr)
			sess := &fakeSession{interruptResult: tt.interruptResult, interruptErr: tt.interruptErr}
			rig := &fakeRig{}
			srv := newServer[*fakeSession](rig, nil, newConfig())
			if tt.attach {
				srv.registry.put(sid, sess)
			}

			req := controlRequest("/v1/sessions/"+tt.sid+"/interrupt", tt.sid, "", false)
			rec := httptest.NewRecorder()

			srv.handleInterrupt(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if sess.interruptCalls != tt.wantInterruptCalls {
				t.Errorf("interruptCalls = %d, want %d", sess.interruptCalls, tt.wantInterruptCalls)
			}

			if tt.wantStatus == http.StatusOK {
				var resp interruptResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode 200 body: %v", err)
				}
				if resp.Interrupted != tt.wantInterrupted {
					t.Errorf("interrupted = %v, want %v", resp.Interrupted, tt.wantInterrupted)
				}
			} else {
				assertErrorEnvelope(t, rec)
			}
		})
	}
}
