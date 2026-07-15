package serve

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
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
	mu       sync.Mutex
	hdr      http.Header
	status   int
	body     bytes.Buffer
	wrote    bool
	writeErr error // when non-nil, Write fails with it (simulates a broken client)
	flushes  chan struct{}
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
	if f.writeErr != nil {
		// Simulate a broken client: record the bytes (so a test can see a write was
		// attempted) but report the error so the stream loop takes its return branch.
		n, _ := f.body.Write(b)
		return n, f.writeErr
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
func runEvents(srv *server[*fakeSession, fakeSessionOption], w http.ResponseWriter, r *http.Request) <-chan struct{} {
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

// quietConfig builds a config whose SSE heartbeat is far longer than any test's
// bounded wait, so the keep-alive ticker never races the assertion in tests that are
// about frame content rather than heartbeats.
func quietConfig() *config {
	c := newConfig()
	c.heartbeat = time.Hour
	return c
}

// TestHandleEventsStreamsEnduring proves a single Enduring delivery is written as
// exactly one SSE frame of the new wire shape —
// event: enduring\nid: <JournalSeq>\ndata: {"v":1,"event":<MarshalEvent>}\n\n — 200
// with Content-Type text/event-stream plus the no-cache/no-buffer headers, and that
// closing the subscription channel ends the handler and tears down the subscription.
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
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
	srv.registry.put(sid, sess)

	rec := newFlushRecorder()
	done := runEvents(srv, rec, eventsRequest(t, context.Background(), eventsSIDStr))

	const seq = 42
	sub.ch <- event.Delivery{Event: ev, JournalSeq: seq}
	select {
	case <-rec.flushes:
	case <-time.After(streamDeadline):
		t.Fatalf("no frame flushed within %v", streamDeadline)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if xb := rec.Header().Get("X-Accel-Buffering"); xb != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", xb)
	}
	if got := rec.statusCode(); got != http.StatusOK {
		t.Errorf("status = %d, want %d", got, http.StatusOK)
	}
	wantFrame := "event: enduring\nid: 42\ndata: {\"v\":1,\"event\":" + string(want) + "}\n\n"
	if got := rec.snapshot(); got != wantFrame {
		t.Errorf("frame = %q, want %q", got, wantFrame)
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

// TestHandleEventsZeroSeqEnduring proves a zero-JournalSeq Enduring delivery still
// stamps an id: line — "id: 0" — rather than omitting it, so the frame shape is
// stable (the decided behavior; a real appender never emits seq 0).
func TestHandleEventsZeroSeqEnduring(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	ev := event.TurnDone{TurnIndex: 1}
	want, err := event.MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent(TurnDone) error = %v", err)
	}

	sub := &fakeSubscription{ch: make(chan event.Delivery, 4)}
	sess := &fakeSession{sub: sub}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
	srv.registry.put(sid, sess)

	rec := newFlushRecorder()
	done := runEvents(srv, rec, eventsRequest(t, context.Background(), eventsSIDStr))

	sub.ch <- event.Delivery{Event: ev, JournalSeq: 0}
	select {
	case <-rec.flushes:
	case <-time.After(streamDeadline):
		t.Fatalf("no frame flushed within %v", streamDeadline)
	}

	wantFrame := "event: enduring\nid: 0\ndata: {\"v\":1,\"event\":" + string(want) + "}\n\n"
	if got := rec.snapshot(); got != wantFrame {
		t.Errorf("frame = %q, want %q", got, wantFrame)
	}

	close(sub.ch)
	select {
	case <-done:
	case <-time.After(streamDeadline):
		t.Fatalf("handler did not return within %v after channel close", streamDeadline)
	}
}

// TestHandleEventsStreamsEphemeral proves each recognized Ephemeral kind is written
// as exactly one `event: ephemeral\ndata: {ephemeralFrame}\n\n` frame with the right
// kind, v:1, tagged delta, and NO id: line — and that a TokenDelta's content.Chunk is
// mapped to its tagged live DTO, never leaked as a raw Go-struct dump.
func TestHandleEventsStreamsEphemeral(t *testing.T) {
	t.Parallel()

	execID := parseTestUUID(t, "55555555-5555-5555-5555-555555555555")
	attemptID := event.CompactAttemptID(parseTestUUID(t, "66666666-6666-6666-6666-666666666666"))
	throughEventID := parseTestUUID(t, "77777777-7777-7777-7777-777777777777")
	progressEventID := parseTestUUID(t, "88888888-8888-8888-8888-888888888888")

	tests := []struct {
		name         string
		ev           event.Event
		wantKind     string
		wantInDelta  []string // substrings that MUST appear in the delta JSON
		wantHasDelta bool     // frame carries a "delta" key at all
	}{
		{
			name:         "token_delta text chunk",
			ev:           event.TokenDelta{TurnIndex: 1, Chunk: &content.TextChunk{Text: "hi"}},
			wantKind:     "token_delta",
			wantInDelta:  []string{`"chunk_type":"text"`, `"text":"hi"`},
			wantHasDelta: true,
		},
		{
			name:         "token_delta thinking chunk",
			ev:           event.TokenDelta{TurnIndex: 1, Chunk: &content.ThinkingChunk{Thinking: "hmm"}},
			wantKind:     "token_delta",
			wantInDelta:  []string{`"chunk_type":"thinking"`, `"thinking":"hmm"`},
			wantHasDelta: true,
		},
		{
			name:         "token_delta tool_use chunk",
			ev:           event.TokenDelta{TurnIndex: 1, Chunk: &content.ToolUseChunk{Index: 2, ID: "tu_1", Name: "Bash", InputJSON: `{"a":1}`}},
			wantKind:     "token_delta",
			wantInDelta:  []string{`"chunk_type":"tool_use"`, `"index":2`, `"id":"tu_1"`, `"name":"Bash"`, `"input_json":"{\"a\":1}"`},
			wantHasDelta: true,
		},
		{
			name:         "tool_call_started",
			ev:           event.ToolCallStarted{ToolExecutionID: execID, ToolName: "Bash", Summary: "ls -la"},
			wantKind:     "tool_call_started",
			wantInDelta:  []string{`"tool_name":"Bash"`, `"summary":"ls -la"`},
			wantHasDelta: true,
		},
		{
			name:         "tool_call_completed",
			ev:           event.ToolCallCompleted{ToolExecutionID: execID, IsError: true, ResultPreview: "boom"},
			wantKind:     "tool_call_completed",
			wantInDelta:  []string{`"is_error":true`, `"result_preview":"boom"`},
			wantHasDelta: true,
		},
		{
			name:         "input_queued has no delta",
			ev:           event.InputQueued{},
			wantKind:     "input_queued",
			wantInDelta:  nil,
			wantHasDelta: false,
		},
		{
			name: "compaction_started carries public progress identity",
			ev: event.CompactionStarted{
				Header:    event.Header{EventID: progressEventID},
				AttemptID: attemptID,
				Reason:    event.CompactionReasonManual,
				Basis:     event.ContextBasis{Revision: 3, ThroughEventID: throughEventID},
			},
			wantKind: "compaction_started",
			wantInDelta: []string{
				`"attempt_id":"66666666-6666-6666-6666-666666666666"`,
				`"reason":1`,
				`"revision":3`,
				`"through_event_id":"77777777-7777-7777-7777-777777777777"`,
				`"header":{"event_id":"88888888-8888-8888-8888-888888888888"}`,
			},
			wantHasDelta: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sid := parseTestUUID(t, eventsSIDStr)
			sub := &fakeSubscription{ch: make(chan event.Delivery, 4)}
			sess := &fakeSession{sub: sub}
			srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
			srv.registry.put(sid, sess)

			rec := newFlushRecorder()
			done := runEvents(srv, rec, eventsRequest(t, context.Background(), eventsSIDStr))

			// JournalSeq is 0 for Ephemeral deliveries; the frame must carry NO id:.
			sub.ch <- event.Delivery{Event: tt.ev}
			select {
			case <-rec.flushes:
			case <-time.After(streamDeadline):
				t.Fatalf("no frame flushed within %v", streamDeadline)
			}

			got := rec.snapshot()
			if !strings.HasPrefix(got, "event: ephemeral\ndata: ") {
				t.Errorf("frame = %q, want event: ephemeral prefix", got)
			}
			if !strings.HasSuffix(got, "\n\n") {
				t.Errorf("frame = %q, want trailing blank line", got)
			}
			if strings.Contains(got, "id:") {
				t.Errorf("frame = %q must NOT carry an id: line (ephemeral is never sequenced)", got)
			}
			if !strings.Contains(got, `"kind":"`+tt.wantKind+`"`) {
				t.Errorf("frame = %q, want kind %q", got, tt.wantKind)
			}
			if !strings.Contains(got, `"v":1`) {
				t.Errorf("frame = %q, want v:1", got)
			}
			for _, want := range tt.wantInDelta {
				if !strings.Contains(got, want) {
					t.Errorf("frame = %q, want delta substring %q", got, want)
				}
			}
			if tt.wantHasDelta && !strings.Contains(got, `"delta":`) {
				t.Errorf("frame = %q, want a delta key", got)
			}
			if !tt.wantHasDelta && strings.Contains(got, `"delta":`) {
				t.Errorf("frame = %q, want NO delta key for %s", got, tt.wantKind)
			}

			close(sub.ch)
			select {
			case <-done:
			case <-time.After(streamDeadline):
				t.Fatalf("handler did not return within %v after channel close", streamDeadline)
			}
		})
	}
}

// TestHandleEventsChunkNoLeak proves a token_delta frame carries the tagged chunk DTO
// and never leaks content.Chunk's Go-struct field names (e.g. a PascalCase "Text" key
// from a naive json.Marshal of the sealed type).
func TestHandleEventsChunkNoLeak(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	sub := &fakeSubscription{ch: make(chan event.Delivery, 4)}
	sess := &fakeSession{sub: sub}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
	srv.registry.put(sid, sess)

	rec := newFlushRecorder()
	done := runEvents(srv, rec, eventsRequest(t, context.Background(), eventsSIDStr))

	sub.ch <- event.Delivery{Event: event.TokenDelta{Chunk: &content.TextChunk{Text: "secret"}}}
	select {
	case <-rec.flushes:
	case <-time.After(streamDeadline):
		t.Fatalf("no frame flushed within %v", streamDeadline)
	}

	got := rec.snapshot()
	if !strings.Contains(got, `"chunk_type":"text"`) {
		t.Errorf("frame = %q, want tagged chunk_type", got)
	}
	if strings.Contains(got, `"Text"`) || strings.Contains(got, `"Chunk"`) {
		t.Errorf("frame = %q leaks a content.Chunk Go-struct field name", got)
	}

	close(sub.ch)
	select {
	case <-done:
	case <-time.After(streamDeadline):
		t.Fatalf("handler did not return within %v after channel close", streamDeadline)
	}
}

// TestHandleEventsSkipsUnrecognizedEphemeral proves an Ephemeral event this transport
// cannot represent (a TokenDelta with a nil chunk — no known variant) is SKIPPED
// without aborting the loop: a FOLLOWING Enduring delivery is still emitted, and it is
// the only frame written.
func TestHandleEventsSkipsUnrecognizedEphemeral(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	eph := event.TokenDelta{TurnIndex: 1} // nil Chunk => unrepresentable => skipped
	end := event.TurnDone{TurnIndex: 2}
	want, err := event.MarshalEvent(end)
	if err != nil {
		t.Fatalf("MarshalEvent(TurnDone) error = %v", err)
	}

	sub := &fakeSubscription{ch: make(chan event.Delivery, 4)}
	sess := &fakeSession{sub: sub}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
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

	wantFrame := "event: enduring\nid: 7\ndata: {\"v\":1,\"event\":" + string(want) + "}\n\n"
	if got := rec.snapshot(); got != wantFrame {
		t.Errorf("frame = %q, want exactly the enduring frame %q (ephemeral not skipped?)", got, wantFrame)
	}

	close(sub.ch)
	select {
	case <-done:
	case <-time.After(streamDeadline):
		t.Fatalf("handler did not return within %v after channel close", streamDeadline)
	}
}

// TestHandleEventsHeartbeat proves an idle stream emits a `: ping` SSE comment on the
// injected (tiny) heartbeat interval and that the cache/no-buffer headers are set. The
// wait is bounded by streamDeadline (a backstop), and the assertion is the ping bytes,
// not a fixed sleep.
func TestHandleEventsHeartbeat(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	sub := &fakeSubscription{ch: make(chan event.Delivery)} // never delivers => idle
	sess := &fakeSession{sub: sub}
	cfg := newConfig()
	cfg.heartbeat = 2 * time.Millisecond
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, cfg)
	srv.registry.put(sid, sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newFlushRecorder()
	done := runEvents(srv, rec, eventsRequest(t, ctx, eventsSIDStr))

	// Wait for the first heartbeat flush (bounded by the deadline backstop).
	select {
	case <-rec.flushes:
	case <-time.After(streamDeadline):
		t.Fatalf("no heartbeat flushed within %v", streamDeadline)
	}

	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if xb := rec.Header().Get("X-Accel-Buffering"); xb != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", xb)
	}
	if got := rec.snapshot(); !strings.Contains(got, ": ping\n\n") {
		t.Errorf("stream = %q, want a : ping comment frame", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(streamDeadline):
		t.Fatalf("handler did not return within %v after cancel", streamDeadline)
	}
}

// TestHandleEventsWriteError proves a write failure mid-stream (a broken client) ends
// the handler — it RETURNS rather than spinning — and the subscription is torn down.
// This exercises the return-on-write-error branch the plain flushRecorder never hits.
func TestHandleEventsWriteError(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	sub := &fakeSubscription{ch: make(chan event.Delivery, 4)}
	sess := &fakeSession{sub: sub}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
	srv.registry.put(sid, sess)

	rec := newFlushRecorder()
	rec.writeErr = errBoom
	done := runEvents(srv, rec, eventsRequest(t, context.Background(), eventsSIDStr))

	// A single enduring delivery drives one w.Write, which fails; the loop must return.
	sub.ch <- event.Delivery{Event: event.TurnDone{TurnIndex: 1}, JournalSeq: 1}
	select {
	case <-done:
	case <-time.After(streamDeadline):
		t.Fatalf("handler did not return within %v after a write error (spinning?)", streamDeadline)
	}
	if !sub.isClosed() {
		t.Error("sub.Close was not called after a write error")
	}
}

// TestHandleEventsClientCancel proves cancelling the request context ends the stream
// promptly and the subscription is closed (defer teardown).
func TestHandleEventsClientCancel(t *testing.T) {
	t.Parallel()

	sid := parseTestUUID(t, eventsSIDStr)
	sub := &fakeSubscription{ch: make(chan event.Delivery)}
	sess := &fakeSession{sub: sub}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, newConfig())
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
			srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, newConfig())
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
