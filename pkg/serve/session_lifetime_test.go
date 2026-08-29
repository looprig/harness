package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
)

// lifetimeSession models the ONE property of a real harness session these tests are
// about: its whole lifetime descends from the context it was constructed with.
// internal/sessionruntime's newSessionTopology and restore constructor both do
// `sessionCtx, sessionCancel := context.WithCancel(ctx)` and every loop context
// descends from that, so a cancelled construction context is a dead session whose
// Submit reports the loop as exited. This double reproduces exactly that rule and
// nothing else.
type lifetimeSession struct {
	id       uuid.UUID
	submitID uuid.UUID
	// ctx is the session's lifetime, derived from the construction context the same
	// way the real runtime derives it.
	ctx    context.Context
	cancel context.CancelFunc
}

func newLifetimeSession(ctx context.Context, id, submitID uuid.UUID) *lifetimeSession {
	sctx, cancel := context.WithCancel(ctx)
	return &lifetimeSession{id: id, submitID: submitID, ctx: sctx, cancel: cancel}
}

func (s *lifetimeSession) SessionID() uuid.UUID { return s.id }

// Submit fails once the session's lifetime is over, mirroring the real runtime's
// "session: loop exited" once its loops have been torn down.
func (s *lifetimeSession) Submit(context.Context, []content.Block) (uuid.UUID, error) {
	if err := s.ctx.Err(); err != nil {
		return uuid.UUID{}, fmt.Errorf("session: loop exited: %w", err)
	}
	return s.submitID, nil
}

func (s *lifetimeSession) SubscribeEvents(event.EventFilter) (event.Subscription, error) {
	return &fakeSubscription{ch: make(chan event.Delivery)}, nil
}

func (s *lifetimeSession) RespondGate(context.Context, gate.GateResponse) error { return nil }

func (s *lifetimeSession) Interrupt(context.Context) (bool, error) { return false, nil }

// lifetimeRig hands out lifetimeSessions and keeps the last one it built so the test
// can inspect the lifetime the handler handed it.
// traceKeyType is the private key type for the request-scoped value the test threads
// through the handler, so the test can assert the detach kept VALUES while dropping
// cancellation (a plain context.Background() would pass the lifetime assertion and lose
// every trace/auth value a caller attached).
type traceKeyType struct{}

// traceValue is the request-scoped value expected to survive into the rig.
const traceValue = "trace-42"

type lifetimeRig struct {
	sid      uuid.UUID
	submitID uuid.UUID

	mu    sync.Mutex
	last  *lifetimeSession
	trace string
}

func (r *lifetimeRig) NewSession(ctx context.Context, _ ...fakeSessionOption) (*lifetimeSession, error) {
	return r.record(ctx, newLifetimeSession(ctx, r.sid, r.submitID)), nil
}

func (r *lifetimeRig) RestoreSession(ctx context.Context, id uuid.UUID) (*lifetimeSession, error) {
	return r.record(ctx, newLifetimeSession(ctx, id, r.submitID)), nil
}

func (r *lifetimeRig) record(ctx context.Context, s *lifetimeSession) *lifetimeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = s
	trace, _ := ctx.Value(traceKeyType{}).(string)
	r.trace = trace
	return s
}

func (r *lifetimeRig) seenTrace() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.trace
}

func (r *lifetimeRig) session(t *testing.T) *lifetimeSession {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		t.Fatal("rig was never asked for a session")
	}
	return r.last
}

// TestSessionOutlivesCreatingRequest is the regression test for the lifetime bug: a
// session brought up over HTTP must still be drivable after the request that created
// it has completed.
//
// It runs over a REAL net/http server, because the bug lives in a property only a real
// server has: net/http cancels r.Context() the moment the handler returns
// (conn.serve calls cancelCtx immediately after ServeHTTP), which an httptest.Recorder
// never does. The client is pinned to a single connection, so the server reads the
// second request off that same connection only AFTER the first request's context has
// been cancelled — the sequencing is structural, not timed.
//
// If the handler passes r.Context() into the rig, the session's lifetime is cancelled
// with the request and the follow-up POST /input answers 500 "session: loop exited",
// which is exactly the failure a browser saw against the unfixed code.
func TestSessionOutlivesCreatingRequest(t *testing.T) {
	t.Parallel()

	const (
		lifetimeSIDStr = "77777777-7777-7777-7777-777777777777"
		lifetimeCmdStr = "99999999-9999-9999-9999-999999999999"
	)

	tests := []struct {
		name string
		// open brings a session up over HTTP and returns the session id now live.
		open func(t *testing.T, c *http.Client, base string, sid uuid.UUID) uuid.UUID
	}{
		{
			name: "create",
			open: func(t *testing.T, c *http.Client, base string, _ uuid.UUID) uuid.UUID {
				t.Helper()
				body := doJSON(t, c, http.MethodPost, base+"/v1/sessions", "", http.StatusCreated)
				var resp createResponse
				if err := json.Unmarshal(body, &resp); err != nil {
					t.Fatalf("decode create response %q: %v", body, err)
				}
				return resp.SessionID
			},
		},
		{
			name: "restore",
			open: func(t *testing.T, c *http.Client, base string, sid uuid.UUID) uuid.UUID {
				t.Helper()
				body := doJSON(t, c, http.MethodPost, base+"/v1/sessions/"+sid.String()+"/restore", "", http.StatusOK)
				var resp restoreResponse
				if err := json.Unmarshal(body, &resp); err != nil {
					t.Fatalf("decode restore response %q: %v", body, err)
				}
				if !resp.Restored {
					t.Fatalf("restore response = %+v, want restored=true", resp)
				}
				return resp.SessionID
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rig := &lifetimeRig{sid: parseTestUUID(t, lifetimeSIDStr), submitID: parseTestUUID(t, lifetimeCmdStr)}
			h := Handler[*lifetimeSession, fakeSessionOption](rig, nil)
			// Stand in for the trace/auth values a real deployment puts on the request
			// context ahead of serve.
			traced := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), traceKeyType{}, traceValue)))
			})
			srv := httptest.NewServer(traced)
			defer srv.Close()

			// One connection for both requests, so the server handles the second only
			// after it has cancelled the first request's context.
			client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1}}
			defer client.CloseIdleConnections()

			sid := tt.open(t, client, srv.URL, parseTestUUID(t, lifetimeSIDStr))

			sess := rig.session(t)
			if err := sess.ctx.Err(); err != nil {
				t.Errorf("session lifetime ended with the request that created it: %v", err)
			}
			// Cancellation is dropped, values are not.
			if got := rig.seenTrace(); got != traceValue {
				t.Errorf("request-scoped value seen by the rig = %q, want %q", got, traceValue)
			}

			// The observable consequence: the session is still drivable.
			doJSON(t, client, http.MethodPost, srv.URL+"/v1/sessions/"+sid.String()+"/input", validBlocksBody, http.StatusOK)
		})
	}
}

// doJSON performs one request, asserts the status, and returns the fully-drained body.
// Draining and closing is what returns the connection to the pool, which is what keeps
// the caller's requests on a single connection.
func doJSON(t *testing.T, c *http.Client, method, url, body string, wantStatus int) []byte {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, url, err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s %s: %v", method, url, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s status = %d, want %d (body %s)", method, url, resp.StatusCode, wantStatus, got)
	}
	return got
}
