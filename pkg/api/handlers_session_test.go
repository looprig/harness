package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/uuid"
)

// errFactory / errInterrupt are leaf sentinels for the 500 paths: a Factory that
// refuses to build an agent, and an agent whose Interrupt fails.
var (
	errFactory   = errors.New("api_test: factory boom")
	errInterrupt = errors.New("api_test: interrupt boom")
)

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

// TestDeleteSession proves DELETE /sessions/{sid} evicts the session and tears it
// down OFF-lock (204, gone from the registry, supervisor stopped, agent closed);
// an unknown id fails secure with 404; a malformed id fails secure with 400.
func TestDeleteSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		setup        func(t *testing.T, s *server) (path string, sid uuid.UUID, fa *fakeAgent, sup *supervisor)
		wantStatus   int
		wantTornDown bool
	}{
		{
			name: "delete existing tears down and returns 204",
			setup: func(t *testing.T, s *server) (string, uuid.UUID, *fakeAgent, *supervisor) {
				t.Helper()
				id := mkID(0x21)
				fa := &fakeAgent{sub: newFakeSub()}
				sup, err := newSupervisor(fa)
				if err != nil {
					t.Fatalf("newSupervisor() error = %v", err)
				}
				s.putSession(id, &sessionEntry{agent: fa, sup: sup})
				return "/sessions/" + id.String(), id, fa, sup
			},
			wantStatus:   http.StatusNoContent,
			wantTornDown: true,
		},
		{
			name: "delete unknown returns 404",
			setup: func(_ *testing.T, _ *server) (string, uuid.UUID, *fakeAgent, *supervisor) {
				return "/sessions/" + mkID(0x99).String(), uuid.UUID{}, nil, nil
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "delete malformed id returns 400",
			setup: func(_ *testing.T, _ *server) (string, uuid.UUID, *fakeAgent, *supervisor) {
				return "/sessions/not-a-uuid", uuid.UUID{}, nil, nil
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

			path, sid, fa, sup := tt.setup(t, s)
			if sup != nil {
				t.Cleanup(func() { _ = sup.stop() })
			}

			resp := doReq(t, ts, http.MethodDelete, path)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("DELETE %s status = %d, want %d", path, resp.StatusCode, tt.wantStatus)
			}

			if !tt.wantTornDown {
				return
			}

			// 204 must carry an empty body.
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll(delete body) error = %v", err)
			}
			if len(b) != 0 {
				t.Errorf("delete body = %q, want empty", b)
			}
			if _, ok := s.getSession(sid); ok {
				t.Error("session still in registry after DELETE")
			}
			if !fa.wasClosed() {
				t.Error("agent Close not called on DELETE")
			}
			select {
			case <-sup.done:
			default:
				t.Error("supervisor not stopped on DELETE")
			}
		})
	}
}

// TestInterruptSession proves POST /sessions/{sid}/interrupt surfaces the agent's
// interrupted bool (200), 404s an unknown session, 400s a malformed id, and maps
// an Interrupt error to a 500 that leaks no internal detail.
func TestInterruptSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		setup           func(t *testing.T, s *server) string
		wantStatus      int
		wantDecode      bool
		wantInterrupted bool
	}{
		{
			name: "interrupt reports true",
			setup: func(t *testing.T, s *server) string {
				return putInterruptSession(t, s, mkID(0x31), &fakeAgent{sub: newFakeSub(), interruptResult: true})
			},
			wantStatus:      http.StatusOK,
			wantDecode:      true,
			wantInterrupted: true,
		},
		{
			name: "interrupt reports false",
			setup: func(t *testing.T, s *server) string {
				return putInterruptSession(t, s, mkID(0x32), &fakeAgent{sub: newFakeSub(), interruptResult: false})
			},
			wantStatus:      http.StatusOK,
			wantDecode:      true,
			wantInterrupted: false,
		},
		{
			name: "interrupt error returns 500",
			setup: func(t *testing.T, s *server) string {
				return putInterruptSession(t, s, mkID(0x33), &fakeAgent{sub: newFakeSub(), interruptErr: errInterrupt})
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "unknown session returns 404",
			setup: func(_ *testing.T, _ *server) string {
				return "/sessions/" + mkID(0xEE).String() + "/interrupt"
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "malformed id returns 400",
			setup: func(_ *testing.T, _ *server) string {
				return "/sessions/not-a-uuid/interrupt"
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

			path := tt.setup(t, s)
			resp := doReq(t, ts, http.MethodPost, path)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("POST %s status = %d, want %d", path, resp.StatusCode, tt.wantStatus)
			}
			if !tt.wantDecode {
				return
			}
			var body interruptResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode interruptResponse: %v", err)
			}
			if body.Interrupted != tt.wantInterrupted {
				t.Errorf("interrupted = %v, want %v", body.Interrupted, tt.wantInterrupted)
			}
		})
	}
}

// putInterruptSession registers fa under id with a live supervisor and returns the
// interrupt path, cleaning the supervisor up when the subtest ends.
func putInterruptSession(t *testing.T, s *server, id uuid.UUID, fa *fakeAgent) string {
	t.Helper()
	sup, err := newSupervisor(fa)
	if err != nil {
		t.Fatalf("newSupervisor() error = %v", err)
	}
	t.Cleanup(func() { _ = sup.stop() })
	s.putSession(id, &sessionEntry{agent: fa, sup: sup})
	return "/sessions/" + id.String() + "/interrupt"
}

// TestCreateSession_ResumeAlreadyLive_409 proves the fail-secure guard against a
// client-controlled ?resume=<sid> that collides with a LIVE session: create must
// refuse with 409 WITHOUT overwriting (and thus orphaning) the existing agent+
// supervisor, and it must tear down the resources it just built so they do not
// leak. It registers an original live session, then resumes its id and asserts the
// original is untouched while the just-built agent was Closed.
func TestCreateSession_ResumeAlreadyLive_409(t *testing.T) {
	t.Parallel()

	sid := mkID(0x5A)

	// The original, live session that must survive the colliding resume untouched.
	fa1 := &fakeAgent{sub: newFakeSub()}
	sup1, err := newSupervisor(fa1)
	if err != nil {
		t.Fatalf("newSupervisor(original) error = %v", err)
	}

	// The factory hands back a DISTINCT agent the create handler builds on the
	// resume path; on the collision it must be torn down, never registered.
	fa2 := &fakeAgent{sub: newFakeSub()}
	factory := func(_ context.Context, _ AgentRequest) (Agent, error) { return fa2, nil }

	s := newServer(Config{}, factory)
	if !s.putSessionIfAbsent(sid, &sessionEntry{agent: fa1, sup: sup1}) {
		t.Fatalf("putSessionIfAbsent(original) = false, want true")
	}
	ts := httptest.NewServer(s.handler())
	defer ts.Close()
	t.Cleanup(func() { stopAll(s) })

	resp := doReq(t, ts, http.MethodPost, "/sessions?resume="+sid.String())
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("resume of a live session status = %d, want 409", resp.StatusCode)
	}

	// The live session was NOT overwritten: the registry still holds the original.
	entry, ok := s.getSession(sid)
	if !ok {
		t.Fatal("original session evicted by a colliding resume; want it preserved")
	}
	if entry.agent != fa1 {
		t.Error("registry entry replaced by the colliding resume; want the original agent")
	}
	// The original is untouched: not closed, supervisor still running.
	if fa1.wasClosed() {
		t.Error("original agent was closed by a colliding resume; want it untouched")
	}
	select {
	case <-sup1.done:
		t.Error("original supervisor stopped by a colliding resume; want it running")
	default:
	}
	// The just-built resources were torn down (not leaked). The handler joins the
	// supervisor stop + agent Close synchronously before writing 409, so this is
	// settled by the time the response arrives.
	if !fa2.wasClosed() {
		t.Error("the just-built agent was not closed on collision; it would leak")
	}
}
