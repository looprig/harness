package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ciram-co/looprig/pkg/event"
)

// msgSubscribeFailed is the client-safe body when the events stream cannot open its
// subscription. Deliberately generic so a response never leaks internal state; the
// concrete cause is logged.
const msgSubscribeFailed = "could not subscribe to session events"

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
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			b, err := event.MarshalEvent(ev)
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
