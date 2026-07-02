package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ciram-co/looprig/pkg/uuid"
)

// errFactory is a leaf sentinel for the create 500 path: a Factory that refuses
// to build an agent.
var errFactory = errors.New("api_test: factory boom")

// recordingFactory is a Factory that records the AgentRequest it was handed (so a
// test can assert the create/resume path minted the right SessionID + Resume flag)
// and hands back a fresh fakeAgent with a live sub so newSupervisor can subscribe.
// makeErr forces the factory-failure (500) path. It is safe for concurrent use so
// -race stays clean across the server's handler goroutine and the test goroutine.
type recordingFactory struct {
	mu      sync.Mutex
	calls   int
	lastReq AgentRequest

	makeErr error
}

func (f *recordingFactory) build(_ context.Context, req AgentRequest) (Agent, error) {
	f.mu.Lock()
	f.calls++
	f.lastReq = req
	f.mu.Unlock()
	if f.makeErr != nil {
		return nil, f.makeErr
	}
	return &fakeAgent{sub: newFakeSub()}, nil
}

func (f *recordingFactory) snapshot() (int, AgentRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.lastReq
}

// stopAll tears down every supervisor a test left in the registry so no run
// goroutine leaks past the subtest (stop is idempotent, so already-stopped
// sessions are a harmless no-op).
func stopAll(s *server) {
	for _, e := range s.snapshotSessions() {
		_ = e.sup.stop()
	}
}

// doReq issues method+path against ts and returns the response; the caller closes
// the body. Fatals on a construction/transport error so each subtest stays terse.
func doReq(t *testing.T, ts *httptest.Server, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest(%s %s) error = %v", method, path, err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do(%s %s) error = %v", method, path, err)
	}
	return resp
}

// TestCreateSession proves POST /sessions mints a fresh session (201, non-zero
// sid, factory called with Resume:false), that ?resume=<sid> resumes that EXACT
// id with Resume:true, that a malformed ?resume= fails secure with 400 and never
// calls the factory, and that a Factory failure is a 500. The happy paths also
// prove the session is registered with a running supervisor.
func TestCreateSession(t *testing.T) {
	t.Parallel()

	resumeID := mkID(0x77)

	tests := []struct {
		name       string
		query      string
		makeErr    error // non-nil => factory refuses to build (500 path)
		wantStatus int
		wantCalled bool
		wantResume bool
		wantSID    uuid.UUID // zero => "any non-zero id" (create mints one)
	}{
		{name: "create mints a new session", query: "", wantStatus: http.StatusCreated, wantCalled: true, wantResume: false},
		{name: "resume uses the given id", query: "?resume=" + resumeID.String(), wantStatus: http.StatusCreated, wantCalled: true, wantResume: true, wantSID: resumeID},
		{name: "malformed resume rejected", query: "?resume=not-a-uuid", wantStatus: http.StatusBadRequest, wantCalled: false},
		{name: "factory failure returns 500", query: "", makeErr: errFactory, wantStatus: http.StatusInternalServerError, wantCalled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rf := &recordingFactory{makeErr: tt.makeErr}
			s := newServer(Config{}, rf.build)
			ts := httptest.NewServer(s.handler())
			defer ts.Close()
			t.Cleanup(func() { stopAll(s) })

			resp := doReq(t, ts, http.MethodPost, "/sessions"+tt.query)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("POST /sessions%s status = %d, want %d", tt.query, resp.StatusCode, tt.wantStatus)
			}

			calls, gotReq := rf.snapshot()
			if tt.wantCalled && calls != 1 {
				t.Fatalf("factory calls = %d, want 1", calls)
			}
			if !tt.wantCalled {
				if calls != 0 {
					t.Errorf("factory calls = %d on bad input, want 0 (never build on 4xx)", calls)
				}
				return
			}
			// A factory-failure 500 called the factory but produced no sid body and
			// registered no session — stop after the call-count assertion.
			if tt.wantStatus != http.StatusCreated {
				return
			}

			var body sidResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode sidResponse: %v", err)
			}
			var gotSID uuid.UUID
			if err := gotSID.UnmarshalText([]byte(body.SID)); err != nil {
				t.Fatalf("response sid %q not a UUID: %v", body.SID, err)
			}
			if gotSID.IsZero() {
				t.Error("response sid is the zero UUID, want a real id")
			}
			if gotReq.Resume != tt.wantResume {
				t.Errorf("factory req.Resume = %v, want %v", gotReq.Resume, tt.wantResume)
			}
			if gotReq.SessionID != gotSID {
				t.Errorf("factory req.SessionID = %v, want it to match the returned sid %v", gotReq.SessionID, gotSID)
			}
			if tt.wantResume && gotReq.SessionID != tt.wantSID {
				t.Errorf("factory req.SessionID = %v, want the resumed id %v", gotReq.SessionID, tt.wantSID)
			}

			// The session must be registered with a running supervisor.
			entry, ok := s.getSession(gotSID)
			if !ok {
				t.Fatalf("session %v not in registry after create", gotSID)
			}
			if entry.sup == nil {
				t.Fatal("session entry has nil supervisor")
			}
			select {
			case <-entry.sup.done:
				t.Error("supervisor already exited after create; want it running")
			default:
			}
		})
	}
}
