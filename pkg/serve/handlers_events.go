package serve

import (
	"errors"
	"io"
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
	// ssePing is the SSE comment frame emitted on the heartbeat interval to keep an
	// idle connection (and any intermediary) alive. It begins with ':' so an
	// EventSource client ignores it — it carries no event.
	ssePing = ": ping\n\n"
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
	// Never cache a live event stream, and disable proxy buffering (nginx's
	// X-Accel-Buffering) so frames and heartbeats reach the client immediately
	// rather than being coalesced by an intermediary (spec §8).
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// Clear any per-connection write deadline: a server-wide WriteTimeout would
	// otherwise truncate this long-lived stream. ErrNotSupported is benign — the
	// underlying writer simply has no deadline to clear.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Debug("serve: events set write deadline", "err", err)
	}

	streamEvents(r, w, rc, sub, s.cfg.heartbeat)
}

// streamEvents copies deliveries from sub onto w as SSE frames until the client
// disconnects (r.Context cancelled) or the subscription ends (channel closed). Each
// delivery is rendered by encodeDelivery into either an `event: enduring` frame (with
// an id: line stamping d.JournalSeq) or an `event: ephemeral` frame (never sequenced);
// a delivery encodeDelivery rejects — an Enduring event outside the sealed union, or
// an unrecognized Ephemeral event — is SKIPPED, never aborting the stream.
//
// An independent ticker emits a `: ping` SSE comment every heartbeat interval so an
// idle stream (and any intermediary) stays alive; it fires on a fixed cadence
// regardless of event activity (simplest correct choice — a client ignores comment
// frames, so an occasional ping alongside real traffic is harmless).
func streamEvents(r *http.Request, w http.ResponseWriter, rc *http.ResponseController, sub event.Subscription, heartbeat time.Duration) {
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ssePing); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case d, ok := <-sub.Events():
			if !ok {
				return
			}
			frame, ok := encodeDelivery(d)
			if !ok {
				continue
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
