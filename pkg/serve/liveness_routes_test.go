package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// doneSession is a fully drivable *fakeSession that ALSO satisfies the optional
// SessionDone extension, standing in for *sessionruntime.Session on the routes. It is
// distinct from liveness_test.go's doneFakeSession, which wraps the inert
// fakeLiveSession: the route tests need a session whose Submit/Interrupt/RespondGate/
// SubscribeEvents actually record and answer, so the "session was never driven"
// assertions mean something.
type doneSession struct {
	*fakeSession
	done chan struct{}
}

func newDoneSession(inner *fakeSession) *doneSession {
	return &doneSession{fakeSession: inner, done: make(chan struct{})}
}

func (d *doneSession) Done() <-chan struct{} { return d.done }

// shutdown publishes death the way the real session does at the START of teardown.
func (d *doneSession) shutdown() { close(d.done) }

// doneRig is a Rig whose concrete session type reports its own death, so the
// lifecycle routes' registration can be observed end-to-end: create/restore, then kill
// the session and watch the entry go. fakeRig cannot serve here — its *fakeSession does
// not satisfy SessionDone, so nothing would ever be evicted.
type doneRig struct {
	newSess     *doneSession
	restoreSess *doneSession
	// restoreCalls counts RestoreSession calls, so a test can assert the rig was not
	// consulted at all (restore's attach path) rather than inferring it from a status.
	restoreCalls int
}

func (r *doneRig) NewSession(context.Context, ...fakeSessionOption) (*doneSession, error) {
	return r.newSess, nil
}

func (r *doneRig) RestoreSession(context.Context, uuid.UUID) (*doneSession, error) {
	r.restoreCalls++
	return r.restoreSess, nil
}

// newDrivableSession builds a *fakeSession every live-plane route can drive to a
// success. Submit and Interrupt already answer with zero-value success; the
// subscription's delivery channel is closed up front so an events stream over it ends
// the instant it starts — the route's success here is that it STREAMED at all, not
// that it stayed open.
func newDrivableSession() *fakeSession {
	ch := make(chan event.Delivery)
	close(ch)
	return &fakeSession{sub: &fakeSubscription{ch: ch}}
}

// liveRoute is one route of the live plane: a handler that resolves {sid} against the
// registry before it does anything else. Every one of them must answer a shut-down sid
// exactly as it answers a sid it has never seen.
type liveRoute struct {
	name string
	// wantOK is the status the route answers for a live, drivable session.
	wantOK int
	call   func(srv *server[*fakeSession, fakeSessionOption], w http.ResponseWriter, sid string)
}

// livenessGateID is a canonical gate id for the gate route's request.
const livenessGateID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

func liveRoutes() []liveRoute {
	return []liveRoute{
		{
			name:   "input",
			wantOK: http.StatusOK,
			call: func(srv *server[*fakeSession, fakeSessionOption], w http.ResponseWriter, sid string) {
				srv.handleInput(w, controlRequest("/v1/sessions/"+sid+"/input", sid, validBlocksBody, true))
			},
		},
		{
			name:   "interrupt",
			wantOK: http.StatusOK,
			call: func(srv *server[*fakeSession, fakeSessionOption], w http.ResponseWriter, sid string) {
				srv.handleInterrupt(w, controlRequest("/v1/sessions/"+sid+"/interrupt", sid, "", false))
			},
		},
		{
			name:   "gate response",
			wantOK: http.StatusAccepted,
			call: func(srv *server[*fakeSession, fakeSessionOption], w http.ResponseWriter, sid string) {
				srv.handleGateResponse(w, gateRequest(sid, livenessGateID, `{"action":"approve"}`, true))
			},
		},
		{
			name:   "events",
			wantOK: http.StatusOK,
			call: func(srv *server[*fakeSession, fakeSessionOption], w http.ResponseWriter, sid string) {
				req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sid+"/events", http.NoBody)
				req.SetPathValue("sid", sid)
				srv.handleEvents(w, req)
			},
		},
	}
}

// TestLivePlaneRoutes404AfterSessionShutdown is the whole point of resolving through
// liveSession rather than registry.get: a session that has begun shutting down is gone
// from the live plane, and the route must not drive it.
//
// The entry is seeded with put, NOT register, so no watcher exists — only the handler's
// own probe can find the corpse. That is the deterministic form of the window a real
// request can land in (between close(done) and the watcher being scheduled), and it is
// what makes the assertion about the HANDLER rather than about goroutine timing.
func TestLivePlaneRoutes404AfterSessionShutdown(t *testing.T) {
	t.Parallel()

	const sidStr = "5c5c5c5c-5c5c-5c5c-5c5c-5c5c5c5c5c5c"

	for _, rt := range liveRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			t.Parallel()

			sid := parseTestUUID(t, sidStr)
			inner := newDrivableSession()
			dead := newDoneSession(inner)
			srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
			srv.registry.put(sid, dead)
			dead.shutdown()

			rec := httptest.NewRecorder()
			rt.call(srv, rec, sidStr)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 for a shut-down session (body %s)", rec.Code, rec.Body.String())
			}
			assertErrorEnvelope(t, rec)
			var env errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if env.Error.Code != codeNotFound {
				t.Errorf("error code = %q, want %q", env.Error.Code, codeNotFound)
			}

			// The corpse must never be driven: a 404 that still called Submit (or
			// opened a subscription) would be a 404 in name only.
			if n := inner.submitCalls + inner.interruptCalls + inner.respondGateCalls + inner.subCalls; n != 0 {
				t.Errorf("the shut-down session was driven %d times; the route must not touch it", n)
			}

			// And the dead entry must be evicted by the probe, not left to be
			// re-resolved by the next request.
			if got, ok := srv.registry.get(sid); ok {
				t.Errorf("registry still holds %v after the probe missed; the corpse was not evicted", got)
			}
		})
	}
}

// TestLivePlaneRoutesAreNotALivenessOracle pins the security half. "This session was
// alive and died" and "this session never existed here" must be indistinguishable on
// the wire, or an unauthorized prober can enumerate which sids ran on this pod. The
// comparison uses the SAME sid against two servers, so any difference is attributable
// to liveness alone.
func TestLivePlaneRoutesAreNotALivenessOracle(t *testing.T) {
	t.Parallel()

	const sidStr = "6d6d6d6d-6d6d-6d6d-6d6d-6d6d6d6d6d6d"

	for _, rt := range liveRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			t.Parallel()

			sid := parseTestUUID(t, sidStr)
			dead := newDoneSession(newDrivableSession())
			shutDownSrv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
			shutDownSrv.registry.put(sid, dead)
			dead.shutdown()

			neverSeenSrv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())

			shutDown := httptest.NewRecorder()
			rt.call(shutDownSrv, shutDown, sidStr)
			neverSeen := httptest.NewRecorder()
			rt.call(neverSeenSrv, neverSeen, sidStr)

			if shutDown.Code != neverSeen.Code {
				t.Errorf("status: shut-down = %d, never-seen = %d; the route is a liveness oracle", shutDown.Code, neverSeen.Code)
			}
			if got, want := shutDown.Body.String(), neverSeen.Body.String(); got != want {
				t.Errorf("body: shut-down = %q, never-seen = %q; the route is a liveness oracle", got, want)
			}
			if got, want := shutDown.Header().Get("Content-Type"), neverSeen.Header().Get("Content-Type"); got != want {
				t.Errorf("Content-Type: shut-down = %q, never-seen = %q", got, want)
			}
		})
	}
}

// TestLivePlaneRoutesTolerateSessionsWithoutDone pins the optionality end-to-end.
// SessionDone is structural and optional, so tui, acp and consumer fakes that implement
// only the five required LiveSession methods must be served exactly as they are today:
// registered, resolvable, driven, on every live-plane route.
func TestLivePlaneRoutesTolerateSessionsWithoutDone(t *testing.T) {
	t.Parallel()

	const sidStr = "7e7e7e7e-7e7e-7e7e-7e7e-7e7e7e7e7e7e"

	for _, rt := range liveRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			t.Parallel()

			sid := parseTestUUID(t, sidStr)
			sess := newDrivableSession()
			if _, isDone := LiveSession(sess).(SessionDone); isDone {
				t.Fatal("*fakeSession satisfies SessionDone; this test needs a session that does not")
			}
			srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
			srv.register(sid, sess)

			rec := httptest.NewRecorder()
			rt.call(srv, rec, sidStr)

			if rec.Code != rt.wantOK {
				t.Fatalf("status = %d, want %d for a session that cannot report death (body %s)", rec.Code, rt.wantOK, rec.Body.String())
			}
			assertRegistered(t, srv.registry, sid, sess)
		})
	}
}

// waitStatus blocks until rec has committed a status code, failing the test if none
// arrives inside streamDeadline. It is how a test knows the SSE handler is PAST its
// registry lookup and inside the stream loop, without racing on unsynchronized handler
// state.
func waitStatus(t *testing.T, rec *flushRecorder, want int) {
	t.Helper()
	deadline := time.Now().Add(streamDeadline)
	for time.Now().Before(deadline) {
		if got := rec.statusCode(); got != 0 {
			if got != want {
				t.Fatalf("stream status = %d, want %d", got, want)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no status written within %v; the handler never reached the stream", streamDeadline)
}

// TestHandleEventsEndsStreamWhenSessionShutsDown is the leak this task exists to fix.
// Hub.SubscribeEvents registers unconditionally and returns nil even after StopSession,
// and the hub never closes subscriptions on stop — so the subscription here NEVER
// closes and no delivery ever arrives, exactly like a real dead session's. With no
// liveness arm the stream heartbeats forever, pinning a handler goroutine, a hub
// subscription and the whole dead *Session.
//
// The wait is bounded by streamDeadline: a regression does not hang the suite, it fails
// after two seconds with the reason. The heartbeat is an hour (quietConfig), so a ping
// cannot end the stream and the request context is never cancelled — the session's own
// death is the ONLY exit.
func TestHandleEventsEndsStreamWhenSessionShutsDown(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	// A subscription that never delivers and never closes: the real post-stop hub.
	inner := &fakeSession{sub: &fakeSubscription{ch: make(chan event.Delivery)}}
	sess := newDoneSession(inner)
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
	srv.register(sid, sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newFlushRecorder()
	handlerDone := runEvents(srv, rec, eventsRequest(t, ctx, eventsSIDStr))

	// Only shut down once the stream is open, so a pass cannot come from the lookup
	// 404-ing before the stream ever started.
	waitStatus(t, rec, http.StatusOK)

	sess.shutdown()

	select {
	case <-handlerDone:
	case <-time.After(streamDeadline):
		t.Fatalf("the SSE stream was still running %v after the session shut down; a dead session pins its handler goroutine and hub subscription forever", streamDeadline)
	}

	if !inner.sub.isClosed() {
		t.Error("the subscription was not closed when the stream ended; the hub entry leaks")
	}
}

// TestHandleEventsKeepsStreamingWhileTheSessionLives is the negative control for the
// liveness arm: an OPEN Done channel must not end the stream. A nil-channel bug or an
// arm that fires on the wrong condition would show up here as a stream that ends on
// its own.
func TestHandleEventsKeepsStreamingWhileTheSessionLives(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	inner := &fakeSession{sub: &fakeSubscription{ch: make(chan event.Delivery)}}
	sess := newDoneSession(inner)
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
	srv.register(sid, sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newFlushRecorder()
	handlerDone := runEvents(srv, rec, eventsRequest(t, ctx, eventsSIDStr))
	waitStatus(t, rec, http.StatusOK)

	select {
	case <-handlerDone:
		t.Fatal("the stream ended while the session was still live")
	case <-time.After(50 * time.Millisecond):
	}

	// Cancelling the request must still end it, exactly as today.
	cancel()
	select {
	case <-handlerDone:
	case <-time.After(streamDeadline):
		t.Fatal("the stream did not end on client disconnect")
	}
}

// TestHandleEventsStreamsForASessionWithoutDone pins the nil-channel contract on the
// hot path. A session that cannot report death yields a NIL liveness channel, and a
// receive on a nil channel blocks forever — so the select behaves exactly as it did
// before the arm existed. If the arm were instead written over a CLOSED channel (or the
// assertion's zero value were mishandled), this stream would end instantly.
func TestHandleEventsStreamsForASessionWithoutDone(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	sess := &fakeSession{sub: &fakeSubscription{ch: make(chan event.Delivery)}}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
	srv.register(sid, sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newFlushRecorder()
	handlerDone := runEvents(srv, rec, eventsRequest(t, ctx, eventsSIDStr))
	waitStatus(t, rec, http.StatusOK)

	select {
	case <-handlerDone:
		t.Fatal("the stream ended on its own for a session that never reports death; the liveness arm must be a nil channel, which blocks forever")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-handlerDone:
	case <-time.After(streamDeadline):
		t.Fatal("the stream did not end on client disconnect")
	}
}

// TestHandleCreateStartsTheWatcher proves the create route registers through register,
// not a bare put. A bare put leaves the entry to outlive its session, and the client
// that created it is exactly the client likely to be holding an open stream — the one
// case on-next-use probing can never free.
func TestHandleCreateStartsTheWatcher(t *testing.T) {
	t.Parallel()

	id := mustUUID(t)
	sess := newDoneSession(&fakeSession{id: id})
	srv := newServer[*doneSession, fakeSessionOption](&doneRig{newSess: sess}, nil, newConfig())

	rec := httptest.NewRecorder()
	srv.handleCreate(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions", http.NoBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	assertRegistered(t, srv.registry, id, sess)

	sess.shutdown()
	waitAbsent(t, srv.registry, id)
}

// TestHandleRestoreStartsTheWatcher is the same guarantee on the restore route. It says
// nothing about restore's rebuild-vs-attach behaviour, only that whatever restore
// registers is watched.
func TestHandleRestoreStartsTheWatcher(t *testing.T) {
	t.Parallel()

	const sidStr = "8f8f8f8f-8f8f-8f8f-8f8f-8f8f8f8f8f8f"
	sid := parseTestUUID(t, sidStr)
	sess := newDoneSession(&fakeSession{id: sid})
	srv := newServer[*doneSession, fakeSessionOption](&doneRig{restoreSess: sess}, nil, newConfig())

	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sidStr+"/restore", http.NoBody)
	req.SetPathValue("sid", sidStr)
	rec := httptest.NewRecorder()
	srv.handleRestore(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	assertRegistered(t, srv.registry, sid, sess)

	sess.shutdown()
	waitAbsent(t, srv.registry, sid)
}
