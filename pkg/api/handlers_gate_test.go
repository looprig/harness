package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
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
