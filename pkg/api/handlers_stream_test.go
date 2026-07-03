package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/transcript"
	"github.com/looprig/harness/pkg/transcript/journalsource"
	"github.com/looprig/harness/pkg/uuid"
)

// export-path leaf sentinels: errExportSource stands in for a non-unavailable
// ExportSource failure (a 500), errReconstruct for a journal read failure surfaced
// by RecordSource.Next (Reconstruct returns a *ReconstructError -> 500).
var (
	errExportSource = errors.New("api_test: export source boom")
	errReconstruct  = errors.New("api_test: reconstruct read boom")
)

// eofSource is a transcript.RecordSource for an empty journal: Next yields io.EOF
// immediately, which Reconstruct folds into an empty-but-renderable Session.
type eofSource struct{}

func (eofSource) Next(_ context.Context) (transcript.Record, error) { return nil, io.EOF }

// errSource is a transcript.RecordSource whose Next fails with a non-EOF error, so
// Reconstruct aborts with a *ReconstructError the export handler maps to 500.
type errSource struct{}

func (errSource) Next(_ context.Context) (transcript.Record, error) { return nil, errReconstruct }

// noPrompts resolves no system prompt for any loop (ok == false) — the empty-source
// happy path needs a well-typed resolver even though it is never consulted.
type noPrompts struct{}

func (noPrompts) SystemPrompt(_ uuid.UUID) (string, bool) { return "", false }

// streamReadDeadline bounds every blocking read/end-of-stream assertion in the SSE
// test so a regression (a frame that never arrives, a stream that never ends on
// cancel) fails fast instead of hanging the suite. It is a backstop, never the
// assertion itself.
const streamReadDeadline = 2 * time.Second

// registerAgent puts agent into s's registry under id with an INERT supervisor
// (built from a throwaway agent's own subscription) so the supervisor never
// competes with the events handler for the events fed to agent's sub, and so the
// cleanup tears the run goroutine down without leaking.
func registerAgent(t *testing.T, s *server, id uuid.UUID, agent *fakeAgent) {
	t.Helper()
	sup, err := newSupervisor(&fakeAgent{sub: newFakeSub()})
	if err != nil {
		t.Fatalf("newSupervisor() error = %v", err)
	}
	t.Cleanup(func() { _ = sup.stop() })
	s.putSession(id, &sessionEntry{agent: agent, sup: sup})
}

// TestEvents_StreamsEnduringSkipsEphemeral proves GET /sessions/{sid}/events opens
// an SSE stream that: serves 200 text/event-stream; emits an Enduring event as a
// `data:` frame carrying the durable envelope (its "type" discriminator + the
// correlating command id); SKIPS an Ephemeral event (MarshalEvent fails closed on
// it) without aborting the stream; and ends when the client cancels the request.
func TestEvents_StreamsEnduringSkipsEphemeral(t *testing.T) {
	t.Parallel()

	s := newServer(Config{}, fakeFactory)
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	fs := newFakeSub()
	lid := mkID(0xB1)
	cmdID := mkID(0xC1)
	sid := mkID(0x51)
	registerAgent(t, s, sid, &fakeAgent{sub: fs})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/sessions/"+sid.String()+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do(GET events) error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET events status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Read the FIRST `data:` frame, then surface any subsequent read error (the
	// stream ending). One goroutine, both channels buffered so it never blocks even
	// after the test has moved on.
	datac := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		sent := false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				errc <- err
				return
			}
			if d, ok := strings.CutPrefix(line, "data: "); ok && !sent {
				sent = true
				datac <- strings.TrimRight(d, "\n")
			}
		}
	}()

	// Feed an EPHEMERAL event first — MarshalEvent rejects it, so it must be SKIPPED
	// (produce no frame) — then an ENDURING terminal event that must arrive framed.
	fs.feed(event.TokenDelta{Header: loopHeader(lid), TurnIndex: 1})
	fs.feed(event.TurnDone{
		Header:    event.Header{Coordinates: identity.Coordinates{LoopID: lid}, Cause: identity.Cause{CommandID: cmdID}},
		TurnIndex: 1,
	})

	select {
	case d := <-datac:
		// The FIRST frame is the TurnDone: if the ephemeral TokenDelta had leaked a
		// frame it would arrive first and would NOT carry the TurnDone discriminator.
		if !strings.Contains(d, `"TurnDone"`) {
			t.Errorf("first SSE frame = %q, want it to carry the TurnDone type discriminator (ephemeral event not skipped?)", d)
		}
		if !strings.Contains(d, cmdID.String()) {
			t.Errorf("first SSE frame = %q, want it to correlate command id %s", d, cmdID)
		}
	case err := <-errc:
		t.Fatalf("stream read errored before any frame: %v", err)
	case <-time.After(streamReadDeadline):
		t.Fatalf("no SSE data frame within %v", streamReadDeadline)
	}

	// Cancelling the request context must end the stream: the handler observes
	// r.Context().Done(), returns, and the client's next read errors.
	cancel()
	select {
	case <-errc:
		// read errored => handler returned => stream ended. Good.
	case <-time.After(streamReadDeadline):
		t.Fatalf("stream did not end within %v after context cancel", streamReadDeadline)
	}
}

// TestEvents_Errors proves the events endpoint fails secure BEFORE any SSE header:
// a malformed sid is 400, an unknown session 404, and a Subscribe failure a 500.
func TestEvents_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(t *testing.T, s *server) string // returns the request path
		wantStatus int
	}{
		{
			name: "subscribe error returns 500",
			setup: func(t *testing.T, s *server) string {
				id := mkID(0x52)
				registerAgent(t, s, id, &fakeAgent{subErr: errSubscribe})
				return "/sessions/" + id.String() + "/events"
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "unknown session returns 404",
			setup: func(_ *testing.T, _ *server) string {
				return "/sessions/" + mkID(0xE1).String() + "/events"
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "malformed id returns 400",
			setup: func(_ *testing.T, _ *server) string {
				return "/sessions/not-a-uuid/events"
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

			path := tt.setup(t, s)
			resp := doReq(t, ts, http.MethodGet, path)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("GET %s status = %d, want %d", path, resp.StatusCode, tt.wantStatus)
			}
		})
	}
}

// TestExport proves GET /sessions/{sid}/export renders a journal-backed transcript
// to HTML (200, text/html, non-empty body) over a real Reconstruct+Render of an
// empty source; maps an ExportUnavailableError to 409 and any other ExportSource or
// Reconstruct failure to 500; and fails secure on a bad/unknown id (400/404).
func TestExport(t *testing.T) {
	t.Parallel()

	const sid = 0x60

	tests := []struct {
		name       string
		agent      *fakeAgent // nil => not registered (unknown-session path)
		rawPath    string     // non-empty => used verbatim (malformed-id path)
		wantStatus int
		wantHTML   bool
	}{
		{
			name:       "happy path renders html from empty source",
			agent:      &fakeAgent{exportSrc: eofSource{}, exportPrompts: noPrompts{}},
			wantStatus: http.StatusOK,
			wantHTML:   true,
		},
		{
			name:       "export unavailable returns 409",
			agent:      &fakeAgent{exportErr: &journalsource.ExportUnavailableError{Reason: "in-memory session"}},
			wantStatus: http.StatusConflict,
		},
		{
			name:       "export source other error returns 500",
			agent:      &fakeAgent{exportErr: errExportSource},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "reconstruct read error returns 500",
			agent:      &fakeAgent{exportSrc: errSource{}, exportPrompts: noPrompts{}},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "unknown session returns 404",
			agent:      nil,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "malformed id returns 400",
			rawPath:    "/sessions/not-a-uuid/export",
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newServer(Config{}, fakeFactory)
			ts := httptest.NewServer(s.handler())
			defer ts.Close()

			path := tt.rawPath
			if path == "" {
				id := mkID(sid)
				if tt.agent != nil {
					registerAgent(t, s, id, tt.agent)
				}
				path = "/sessions/" + id.String() + "/export"
			}

			resp := doReq(t, ts, http.MethodGet, path)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("GET %s status = %d, want %d", path, resp.StatusCode, tt.wantStatus)
			}
			if !tt.wantHTML {
				return
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q, want it to start with text/html", ct)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll(export body) error = %v", err)
			}
			if len(body) == 0 {
				t.Error("export body is empty, want rendered HTML")
			}
		})
	}
}
