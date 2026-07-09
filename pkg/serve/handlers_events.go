package serve

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/looprig/harness/pkg/event"
)

const (
	// contentTypeSSE is the media type of the events stream body.
	contentTypeSSE = "text/event-stream"
	// msgSubscribeFailed is the generic, client-safe 500 message when a session's
	// event subscription cannot be opened; the concrete cause is logged, never sent.
	msgSubscribeFailed = "could not subscribe to session events"
)

// allEventsFilter is the whole-session subscription the SSE stream opens: both
// classes, every loop. It mirrors pkg/api's allEventsFilter — subscribing broadly is
// correct because the MARSHALER (event.MarshalEvent), not the filter, is what keeps
// Ephemeral events out of the Phase-1 durable SSE payload (it fails closed on them).
func allEventsFilter() event.EventFilter {
	return event.EventFilter{
		Ephemeral: event.LoopScope{All: true},
		Enduring:  event.LoopScope{All: true},
	}
}

// handleEvents serves GET /v1/sessions/{sid}/events as a Server-Sent Events stream.
// It resolves {sid} against the live registry (malformed => 400, unknown => 404),
// opens a whole-session subscription (a Subscribe failure => 500 BEFORE any SSE
// header is written, so the client gets a normal JSON error, not a half-open
// stream), then streams each Enduring event as a `data:` frame until the client
// disconnects or the subscription ends. The subscription is always closed on return.
func (s *server[S]) handleEvents(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r.PathValue("sid"))
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidSID, false, err)
		return
	}

	sess, ok := s.registry.get(sid)
	if !ok {
		writeErrorCause(w, http.StatusNotFound, codeNotFound, msgNotFound, false, SessionNotFoundError{SessionID: sid})
		return
	}

	// Subscribe BEFORE any SSE header: a failure here is a clean 500 with a JSON
	// body, never a truncated event stream.
	sub, err := sess.SubscribeEvents(allEventsFilter())
	if err != nil {
		writeErrorCause(w, http.StatusInternalServerError, codeInternal, msgSubscribeFailed, false, err)
		return
	}
	defer func() { _ = sub.Close() }()

	w.Header().Set("Content-Type", contentTypeSSE)
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// Clear any per-connection write deadline: a server-wide WriteTimeout would
	// otherwise truncate this long-lived stream. ErrNotSupported is benign — the
	// underlying writer simply has no deadline to clear.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Debug("serve: events set write deadline", "err", err)
	}

	streamEvents(r, w, rc, sub)
}

// streamEvents copies deliveries from sub onto w as SSE `data:` frames until the
// client disconnects (r.Context cancelled) or the subscription ends (channel
// closed). An event the marshaler rejects — every Ephemeral event, plus any unknown
// type — is SKIPPED (never aborting the stream): the durable envelope is the Phase-1
// SSE payload and Ephemeral deltas are a documented omission. MarshalEvent returns
// compact single-line JSON, so one `data:` line per event is well-formed.
func streamEvents(r *http.Request, w http.ResponseWriter, rc *http.ResponseController, sub event.Subscription) {
	for {
		select {
		case <-r.Context().Done():
			return
		case d, ok := <-sub.Events():
			if !ok {
				return
			}
			data, err := event.MarshalEvent(d.Event)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
