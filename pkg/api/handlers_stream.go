package api

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/transcript"
	"github.com/looprig/harness/pkg/transcript/html"
	"github.com/looprig/harness/pkg/transcript/journalsource"
)

// Client-safe messages for the stream + export endpoints. Deliberately generic so a
// response never leaks the runner's internal state; the concrete cause is logged.
const (
	msgSubscribeFailed   = "could not subscribe to session events"
	msgExportUnavailable = "transcript export is not available for this session"
	msgExportFailed      = "could not export session transcript"
)

// allEventsFilter is the whole-session subscription the SSE stream opens: both
// classes, every loop. It mirrors the supervisor's subscription — the marshaler
// (not the filter) is what keeps Ephemeral events out of the durable SSE payload,
// so subscribing broadly here is correct.
func allEventsFilter() event.EventFilter {
	return event.EventFilter{
		Ephemeral: event.LoopScope{All: true},
		Enduring:  event.LoopScope{All: true},
	}
}

// handleEvents serves GET /sessions/{sid}/events as a Server-Sent Events stream. It
// looks the session up (malformed id => 400, unknown => 404), opens a whole-session
// subscription (a Subscribe failure => 500 BEFORE any SSE header is written), then
// streams each event as a `data:` frame until the client disconnects or the
// subscription ends. The subscription is always closed on return (no leak).
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r)
	if err != nil {
		slog.Warn("api: events rejected invalid id", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidSessionID)
		return
	}

	entry, ok := s.getSession(sid)
	if !ok {
		writeError(w, http.StatusNotFound, msgSessionNotFound)
		return
	}

	// OFF-lock: getSession released s.mu, so Subscribe never runs under the registry
	// lock. A failure here is a clean 500 — no SSE header has been written yet.
	sub, err := entry.agent.Subscribe(allEventsFilter())
	if err != nil {
		slog.Error("api: events subscribe failed", "err", err)
		writeError(w, http.StatusInternalServerError, msgSubscribeFailed)
		return
	}
	defer func() { _ = sub.Close() }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// Disable any per-connection write deadline for this long-lived stream (a
	// server-wide WriteTimeout would truncate it). ErrNotSupported is benign — the
	// writer simply has no deadline to clear.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Debug("api: events set write deadline", "err", err)
	}
	if err := rc.Flush(); err != nil {
		slog.Debug("api: events flush headers", "err", err)
		return
	}

	streamEvents(r, w, rc, sub)
}

// streamEvents copies events from sub onto w as SSE `data:` frames until the client
// disconnects (r.Context cancelled) or the subscription ends (channel closed). An
// event the marshaler rejects — every Ephemeral event, plus any unknown type — is
// SKIPPED (logged at debug), never aborting the stream: the durable envelope is the
// v1 SSE payload and ephemeral deltas are a documented omission. MarshalEvent
// returns compact single-line JSON, so one `data:` line per event is well-formed.
func streamEvents(r *http.Request, w http.ResponseWriter, rc *http.ResponseController, sub event.Subscription) {
	for {
		select {
		case <-r.Context().Done():
			return
		case d, ok := <-sub.Events():
			if !ok {
				return
			}
			b, err := event.MarshalEvent(d.Event)
			if err != nil {
				slog.Debug("api: events skip unmarshalable event", "err", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// handleExport serves GET /sessions/{sid}/export: it reconstructs the session's
// journal into a transcript and renders it to a self-contained HTML document. It
// looks the session up (malformed id => 400, unknown => 404). A non-journal-backed
// session has no exportable transcript (ExportUnavailableError => 409); any other
// ExportSource or Reconstruct failure is a 500. The document is rendered into a
// buffer FIRST so a Render failure is a clean 500 with no partial 200 body.
func (s *server) handleExport(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r)
	if err != nil {
		slog.Warn("api: export rejected invalid id", "err", err)
		writeError(w, http.StatusBadRequest, msgInvalidSessionID)
		return
	}

	entry, ok := s.getSession(sid)
	if !ok {
		writeError(w, http.StatusNotFound, msgSessionNotFound)
		return
	}

	// OFF-lock: getSession released s.mu, so no agent call runs under the registry
	// lock.
	src, prompts, err := entry.agent.ExportSource(r.Context())
	if err != nil {
		var unavailable *journalsource.ExportUnavailableError
		if errors.As(err, &unavailable) {
			slog.Warn("api: export unavailable", "err", err)
			writeError(w, http.StatusConflict, msgExportUnavailable)
			return
		}
		slog.Error("api: export source failed", "err", err)
		writeError(w, http.StatusInternalServerError, msgExportFailed)
		return
	}

	sess, _, err := transcript.Reconstruct(r.Context(), src, prompts)
	if err != nil {
		slog.Error("api: export reconstruct failed", "err", err)
		writeError(w, http.StatusInternalServerError, msgExportFailed)
		return
	}

	// Render into a buffer first: only a successful render commits a 200 + body, so
	// a Render failure is a clean 500 with nothing partial written.
	var buf bytes.Buffer
	if err := html.Render(&buf, sess); err != nil {
		slog.Error("api: export render failed", "err", err)
		writeError(w, http.StatusInternalServerError, msgExportFailed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(buf.Bytes()); err != nil {
		slog.Error("api: export write body", "err", err)
	}
}
