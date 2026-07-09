package serve

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/event"
)

// streamDeadline bounds every blocking wait in the SSE tests so a regression (a
// frame that never arrives, a stream that never ends on cancel/close) fails fast
// instead of hanging the suite. It is a backstop, never the assertion itself.
const streamDeadline = 2 * time.Second

// eventsSIDStr is a canonical session id used across the events-handler tests.
const eventsSIDStr = "44444444-4444-4444-4444-444444444444"

// fakeSubscription is a controllable event.Subscription test double: the test feeds
// deliveries onto ch and closes it to end the stream, and Close records that the
// handler tore the subscription down (asserted via the atomic closed flag).
type fakeSubscription struct {
	ch     chan event.Delivery
	err    error
	closed atomic.Bool
}

func (s *fakeSubscription) Events() <-chan event.Delivery { return s.ch }

func (s *fakeSubscription) Close() error {
	s.closed.Store(true)
	return nil
}

func (s *fakeSubscription) Err() error { return s.err }

func (s *fakeSubscription) isClosed() bool { return s.closed.Load() }

// flushRecorder is an http.ResponseWriter that also implements http.Flusher so the
// SSE handler's per-frame flush works, and http.NewResponseController can drive it.
// Every Flush signals flushes so a test can deterministically wait for a frame to
// be written (no sleeps). It does NOT implement a write-deadline setter, so the
// handler's SetWriteDeadline exercises the benign ErrNotSupported path.
type flushRecorder struct {
	mu      sync.Mutex
	hdr     http.Header
	status  int
	body    bytes.Buffer
	wrote   bool
	flushes chan struct{}
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{hdr: make(http.Header), flushes: make(chan struct{}, 64)}
}

func (f *flushRecorder) Header() http.Header { return f.hdr }

func (f *flushRecorder) WriteHeader(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.wrote {
		f.status = code
		f.wrote = true
	}
}

func (f *flushRecorder) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.wrote {
		f.status = http.StatusOK
		f.wrote = true
	}
	return f.body.Write(b)
}

func (f *flushRecorder) Flush() { f.flushes <- struct{}{} }

func (f *flushRecorder) snapshot() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body.String()
}

func (f *flushRecorder) statusCode() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// runEvents drives handleEvents in a goroutine and returns a channel closed when the
// handler returns, so a test can assert the stream ended (on cancel or channel close)
// without racing on the handler's own goroutine.
func runEvents(srv *server[*fakeSession], w http.ResponseWriter, r *http.Request) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		srv.handleEvents(w, r)
		close(done)
	}()
	return done
}

func eventsRequest(t *testing.T, ctx context.Context, sid string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/sessions/"+sid+"/events", http.NoBody)
	req.SetPathValue("sid", sid)
	return req
}

// TestHandleEventsStreamsEnduring proves a single Enduring delivery is written as
// exactly one SSE `data:` frame whose payload equals MarshalEvent's output, 200 with
// Content-Type text/event-stream, and that closing the subscription channel ends the
// handler.
func TestHandleEventsStreamsEnduring(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	ev := event.TurnDone{TurnIndex: 1}
	want, err := event.MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent(TurnDone) error = %v", err)
	}

	sub := &fakeSubscription{ch: make(chan event.Delivery, 4)}
	sess := &fakeSession{sub: sub}
	srv := newServer[*fakeSession](&fakeRunner{}, newConfig())
	srv.registry.put(sid, sess)

	rec := newFlushRecorder()
	done := runEvents(srv, rec, eventsRequest(t, context.Background(), eventsSIDStr))

	sub.ch <- event.Delivery{Event: ev, JournalSeq: 1}
	select {
	case <-rec.flushes:
	case <-time.After(streamDeadline):
		t.Fatalf("no frame flushed within %v", streamDeadline)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := rec.statusCode(); got != http.StatusOK {
		t.Errorf("status = %d, want %d", got, http.StatusOK)
	}
	if got, frame := rec.snapshot(), "data: "+string(want)+"\n\n"; got != frame {
		t.Errorf("frame = %q, want %q", got, frame)
	}

	close(sub.ch)
	select {
	case <-done:
	case <-time.After(streamDeadline):
		t.Fatalf("handler did not return within %v after channel close", streamDeadline)
	}
	if !sub.isClosed() {
		t.Error("sub.Close was not called on return")
	}
}

// TestHandleEventsSkipsEphemeral proves an Ephemeral delivery (which MarshalEvent
// rejects) is SKIPPED without aborting the loop: a FOLLOWING Enduring delivery is
// still emitted, and it is the only frame written.
func TestHandleEventsSkipsEphemeral(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	eph := event.TokenDelta{TurnIndex: 1}
	end := event.TurnDone{TurnIndex: 2}
	want, err := event.MarshalEvent(end)
	if err != nil {
		t.Fatalf("MarshalEvent(TurnDone) error = %v", err)
	}

	sub := &fakeSubscription{ch: make(chan event.Delivery, 4)}
	sess := &fakeSession{sub: sub}
	srv := newServer[*fakeSession](&fakeRunner{}, newConfig())
	srv.registry.put(sid, sess)

	rec := newFlushRecorder()
	done := runEvents(srv, rec, eventsRequest(t, context.Background(), eventsSIDStr))

	sub.ch <- event.Delivery{Event: eph}
	sub.ch <- event.Delivery{Event: end, JournalSeq: 7}
	select {
	case <-rec.flushes:
	case <-time.After(streamDeadline):
		t.Fatalf("no frame flushed within %v", streamDeadline)
	}

	if got, frame := rec.snapshot(), "data: "+string(want)+"\n\n"; got != frame {
		t.Errorf("frame = %q, want exactly the enduring frame %q (ephemeral not skipped?)", got, frame)
	}

	close(sub.ch)
	select {
	case <-done:
	case <-time.After(streamDeadline):
		t.Fatalf("handler did not return within %v after channel close", streamDeadline)
	}
}

// TestHandleEventsClientCancel proves cancelling the request context ends the stream
// promptly and the subscription is closed (defer teardown).
func TestHandleEventsClientCancel(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	sub := &fakeSubscription{ch: make(chan event.Delivery)}
	sess := &fakeSession{sub: sub}
	srv := newServer[*fakeSession](&fakeRunner{}, newConfig())
	srv.registry.put(sid, sess)

	ctx, cancel := context.WithCancel(context.Background())
	rec := newFlushRecorder()
	done := runEvents(srv, rec, eventsRequest(t, ctx, eventsSIDStr))

	cancel()
	select {
	case <-done:
	case <-time.After(streamDeadline):
		t.Fatalf("handler did not return within %v after context cancel", streamDeadline)
	}
	if !sub.isClosed() {
		t.Error("sub.Close was not called on client cancel")
	}
}

// TestHandleEventsErrors proves the events endpoint fails secure BEFORE any SSE
// header: malformed sid => 400, unknown session => 404 (SubscribeEvents not called),
// subscribe failure => 500. Each writes the nested JSON error envelope with
// Content-Type application/json, never text/event-stream.
func TestHandleEventsErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		sid          string
		register     bool
		subErr       error
		wantStatus   int
		wantSubCalls int
	}{
		{
			name:         "malformed sid is 400 subscribe not called",
			sid:          "not-a-uuid",
			register:     false,
			wantStatus:   http.StatusBadRequest,
			wantSubCalls: 0,
		},
		{
			name:         "unknown session is 404 subscribe not called",
			sid:          eventsSIDStr,
			register:     false,
			wantStatus:   http.StatusNotFound,
			wantSubCalls: 0,
		},
		{
			name:         "subscribe error is 500",
			sid:          eventsSIDStr,
			register:     true,
			subErr:       errBoom,
			wantStatus:   http.StatusInternalServerError,
			wantSubCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sess := &fakeSession{subErr: tt.subErr}
			srv := newServer[*fakeSession](&fakeRunner{}, newConfig())
			if tt.register {
				srv.registry.put(parseTestUUID(t, eventsSIDStr), sess)
			}

			rec := httptest.NewRecorder()
			srv.handleEvents(rec, eventsRequest(t, context.Background(), tt.sid))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != contentTypeJSON {
				t.Errorf("Content-Type = %q, want %q (no SSE header on error)", ct, contentTypeJSON)
			}
			if sess.subCalls != tt.wantSubCalls {
				t.Errorf("subCalls = %d, want %d", sess.subCalls, tt.wantSubCalls)
			}
			assertErrorEnvelope(t, rec)
		})
	}
}
