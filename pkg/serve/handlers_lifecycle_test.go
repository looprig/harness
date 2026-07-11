package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
)

// fakeSession is a test double for LiveSession: it records Submit and Interrupt
// calls and returns configured results / errors. The subscribe/gate methods
// satisfy the interface but are unused by the lifecycle/control routes.
type fakeSession struct {
	id           uuid.UUID
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

	subErr   error
	sub      *fakeSubscription
	subCalls int
}

func (f *fakeSession) SessionID() uuid.UUID { return f.id }

func (f *fakeSession) Submit(_ context.Context, blocks []content.Block) (uuid.UUID, error) {
	f.submitCalls++
	f.submitBlocks = blocks
	return f.submitID, f.submitErr
}

func (f *fakeSession) SubscribeEvents(event.EventFilter) (event.Subscription, error) {
	f.subCalls++
	if f.subErr != nil {
		return nil, f.subErr
	}
	return f.sub, nil
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

// fakeRig is a test double for Rig[*fakeSession]: Run/Restore return the
// configured id/session/error and count their calls, and Restore records the id it
// was asked to rebuild.
type fakeRig struct {
	runID    uuid.UUID
	runSess  *fakeSession
	runErr   error
	runCalls int

	restoreSess  *fakeSession
	restoreErr   error
	restoreCalls int
	restoreGotID uuid.UUID
}

func (f *fakeRig) NewSession(context.Context) (*fakeSession, error) {
	f.runCalls++
	if f.runSess != nil {
		f.runSess.id = f.runID
	}
	return f.runSess, f.runErr
}

func (f *fakeRig) RestoreSession(_ context.Context, id uuid.UUID) (*fakeSession, error) {
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

// errBoom is a stand-in rig/session failure cause.
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
			name:            "rig run error is 500 nothing attached",
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
			rig := &fakeRig{runID: runID, runSess: sess, runErr: tt.runErr}
			srv := newServer[*fakeSession](rig, nil, newConfig())

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
			if rig.runCalls != tt.wantRunCalls {
				t.Errorf("runCalls = %d, want %d", rig.runCalls, tt.wantRunCalls)
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
			rig := &fakeRig{restoreSess: sess, restoreErr: tt.restoreErr}
			srv := newServer[*fakeSession](rig, nil, newConfig())

			req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+tt.sid+"/restore", http.NoBody)
			req.SetPathValue("sid", tt.sid)
			rec := httptest.NewRecorder()

			srv.handleRestore(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if rig.restoreCalls != tt.wantRestoreCalls {
				t.Errorf("restoreCalls = %d, want %d", rig.restoreCalls, tt.wantRestoreCalls)
			}
			if tt.wantRestoreCalls > 0 && rig.restoreGotID != parseTestUUID(t, sidStr) {
				t.Errorf("restore got id = %v, want %v", rig.restoreGotID, sidStr)
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

// idemRunID / idemCmdID are the fixed ids the fake rig/session mint in the
// idempotency handler tests.
const (
	idemRunID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	idemCmdID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

// doCreate drives handleCreate with an optional Idempotency-Key and body ("" body =>
// no body), returning the recorder for assertions.
func doCreate(t *testing.T, srv *server[*fakeSession], key, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, "/v1/sessions", http.NoBody)
	} else {
		req = httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(body))
	}
	if key != "" {
		req.Header.Set(headerIdempotencyKey, key)
	}
	rec := httptest.NewRecorder()
	srv.handleCreate(rec, req)
	return rec
}

// decodeCreate201 decodes a 201 create body, failing the test on a non-201 or a bad
// body.
func decodeCreate201(t *testing.T, rec *httptest.ResponseRecorder) createResponse {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var resp createResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 201 body: %v", err)
	}
	return resp
}

// newIdemTestServer builds a server whose idempotency clock is controllable via the
// returned pointer (advance it to cross the TTL), with a 1h TTL.
func newIdemTestServer(t *testing.T) (*server[*fakeSession], *fakeRig, *time.Time) {
	t.Helper()
	sess := &fakeSession{submitID: parseTestUUID(t, idemCmdID)}
	rig := &fakeRig{runID: parseTestUUID(t, idemRunID), runSess: sess}
	srv := newServer[*fakeSession](rig, nil, newConfig())
	clock := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	srv.idem.ttl = time.Hour
	srv.idem.now = func() time.Time { return clock }
	return srv, rig, &clock
}

// TestServerHandleCreateNoKeyDoesNotTouchStore proves an absent key is a normal
// create that never records an idempotency entry.
func TestServerHandleCreateNoKeyDoesNotTouchStore(t *testing.T) {
	t.Parallel()
	srv, rig, _ := newIdemTestServer(t)

	rec := doCreate(t, srv, "", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if rig.runCalls != 1 {
		t.Errorf("runCalls = %d, want 1", rig.runCalls)
	}
	srv.idem.mu.Lock()
	n := len(srv.idem.entries)
	srv.idem.mu.Unlock()
	if n != 0 {
		t.Errorf("store entries = %d, want 0 (untouched)", n)
	}
}

// TestServerHandleCreateIdempotentSequences covers the two-call sequential
// guarantees: same-key/same-body replay (no re-run), same-key/different-body 409,
// and expired-key re-create.
func TestServerHandleCreateIdempotentSequences(t *testing.T) {
	t.Parallel()

	const key = "client-key-1"

	tests := []struct {
		name             string
		firstBody        string
		secondBody       string
		advancePastTTL   bool
		wantSecondStatus int
		wantRunCalls     int
		wantReplay       bool // second 201 must replay first's ids
	}{
		{
			name:             "same key same body replays without re-running",
			firstBody:        validBlocksBody,
			secondBody:       validBlocksBody,
			wantSecondStatus: http.StatusCreated,
			wantRunCalls:     1,
			wantReplay:       true,
		},
		{
			name:             "same key same idle body replays",
			firstBody:        "",
			secondBody:       "",
			wantSecondStatus: http.StatusCreated,
			wantRunCalls:     1,
			wantReplay:       true,
		},
		{
			name:             "same key different body is 409 and does not re-run",
			firstBody:        `{}`,
			secondBody:       validBlocksBody,
			wantSecondStatus: http.StatusConflict,
			wantRunCalls:     1,
			wantReplay:       false,
		},
		{
			name:             "expired key mints a fresh create",
			firstBody:        `{}`,
			secondBody:       `{}`,
			advancePastTTL:   true,
			wantSecondStatus: http.StatusCreated,
			wantRunCalls:     2,
			wantReplay:       false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, rig, clock := newIdemTestServer(t)

			first := doCreate(t, srv, key, tt.firstBody)
			firstResp := decodeCreate201(t, first)

			if tt.advancePastTTL {
				*clock = clock.Add(2 * time.Hour)
			}

			second := doCreate(t, srv, key, tt.secondBody)
			if second.Code != tt.wantSecondStatus {
				t.Fatalf("second status = %d, want %d (body %s)", second.Code, tt.wantSecondStatus, second.Body.String())
			}
			if rig.runCalls != tt.wantRunCalls {
				t.Errorf("runCalls = %d, want %d", rig.runCalls, tt.wantRunCalls)
			}

			switch tt.wantSecondStatus {
			case http.StatusCreated:
				secondResp := decodeCreate201(t, second)
				if tt.wantReplay {
					if secondResp.SessionID != firstResp.SessionID {
						t.Errorf("replay session_id = %v, want %v", secondResp.SessionID, firstResp.SessionID)
					}
					if (secondResp.CommandID == nil) != (firstResp.CommandID == nil) {
						t.Errorf("replay command_id presence mismatch: %v vs %v", secondResp.CommandID, firstResp.CommandID)
					}
					if secondResp.CommandID != nil && firstResp.CommandID != nil &&
						*secondResp.CommandID != *firstResp.CommandID {
						t.Errorf("replay command_id = %v, want %v", *secondResp.CommandID, *firstResp.CommandID)
					}
				}
			case http.StatusConflict:
				assertErrorEnvelope(t, second)
			}
		})
	}
}

// TestServerHandleCreateIdempotentReplayCarriesCommandID proves the create-with-input
// replay returns the SAME command_id (not just session_id).
func TestServerHandleCreateIdempotentReplayCarriesCommandID(t *testing.T) {
	t.Parallel()
	srv, rig, _ := newIdemTestServer(t)

	first := decodeCreate201(t, doCreate(t, srv, "k", validBlocksBody))
	second := decodeCreate201(t, doCreate(t, srv, "k", validBlocksBody))

	if rig.runCalls != 1 {
		t.Fatalf("runCalls = %d, want 1", rig.runCalls)
	}
	if first.CommandID == nil {
		t.Fatal("first command_id absent, want present")
	}
	if second.CommandID == nil || *second.CommandID != *first.CommandID {
		t.Errorf("replay command_id = %v, want %v", second.CommandID, *first.CommandID)
	}
	// Submit ran exactly once (the replay did not re-submit).
	if srv.rig.(*fakeRig).runSess.submitCalls != 1 {
		t.Errorf("submitCalls = %d, want 1", srv.rig.(*fakeRig).runSess.submitCalls)
	}
}

// TestServerHandleCreateOversizedKey proves a key over the bound is 400 before the
// rig or store is touched.
func TestServerHandleCreateOversizedKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		keyLen     int
		wantStatus int
		wantRun    int
	}{
		{name: "max length key is accepted", keyLen: maxIdempotencyKeyLen, wantStatus: http.StatusCreated, wantRun: 1},
		{name: "one over max is rejected", keyLen: maxIdempotencyKeyLen + 1, wantStatus: http.StatusBadRequest, wantRun: 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, rig, _ := newIdemTestServer(t)
			key := strings.Repeat("a", tt.keyLen)

			rec := doCreate(t, srv, key, "")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if rig.runCalls != tt.wantRun {
				t.Errorf("runCalls = %d, want %d", rig.runCalls, tt.wantRun)
			}
			if tt.wantStatus == http.StatusBadRequest {
				assertErrorEnvelope(t, rec)
				srv.idem.mu.Lock()
				n := len(srv.idem.entries)
				srv.idem.mu.Unlock()
				if n != 0 {
					t.Errorf("store entries = %d, want 0 (untouched on oversized key)", n)
				}
			}
		})
	}
}

// concurrentRig is a race-safe Rig for the concurrency smoke test (the shared
// fakeRig increments an unguarded counter, which -race would flag under concurrent
// Run).
type concurrentRig struct {
	mu    sync.Mutex
	id    uuid.UUID
	sess  *fakeSession
	calls int
}

func (r *concurrentRig) NewSession(context.Context) (*fakeSession, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return r.sess, nil
}

func (r *concurrentRig) RestoreSession(context.Context, uuid.UUID) (*fakeSession, error) {
	return r.sess, nil
}

func (r *concurrentRig) runCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestServerHandleCreateIdempotentConcurrent is the -race smoke test for the per-pod
// double-run window: concurrent same-key idle creates must be race-free, and a
// SUBSEQUENT sequential same-key create must replay (not re-run) — the documented
// sequential guarantee.
func TestServerHandleCreateIdempotentConcurrent(t *testing.T) {
	t.Parallel()

	id := parseTestUUID(t, idemRunID)
	rig := &concurrentRig{id: id, sess: &fakeSession{id: id}}
	srv := newServer[*fakeSession](rig, nil, newConfig())

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := doCreate(t, srv, "shared-key", "")
			if rec.Code != http.StatusCreated {
				t.Errorf("concurrent create status = %d, want 201", rec.Code)
			}
		}()
	}
	wg.Wait()

	before := rig.runCount()
	if before < 1 || before > n {
		t.Fatalf("concurrent runCalls = %d, want in [1,%d]", before, n)
	}

	// After the concurrent burst settled, a repeat of the key replays the cached
	// outcome and does NOT run again — the required sequential guarantee.
	rec := doCreate(t, srv, "shared-key", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("sequential replay status = %d, want 201", rec.Code)
	}
	if after := rig.runCount(); after != before {
		t.Errorf("sequential replay re-ran: runCalls %d -> %d", before, after)
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
