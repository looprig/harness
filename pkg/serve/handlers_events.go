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
// classes, every loop. Subscribing broadly is correct because the stream encodes both
// classes — Enduring deliveries become "enduring" frames and Ephemeral deliveries
// become "ephemeral" frames; the frame class, not the filter, decides the encoding.
func allEventsFilter() event.EventFilter {
	return event.EventFilter{
		Ephemeral: event.LoopScope{All: true},
		Enduring:  event.LoopScope{All: true},
	}
}

// handleEvents serves GET /v1/sessions/{sid}/events as a Server-Sent Events stream.
// It resolves {sid} against the LIVE plane (malformed => 400; unknown or already
// shutting down => the same 404), opens a whole-session subscription (a Subscribe
// failure => 500 BEFORE any SSE header is written, so the client gets a normal JSON
// error, not a half-open stream), then streams each event — both Enduring and
// Ephemeral classes — as its SSE frame until the client disconnects, the session dies,
// or the subscription ends. The subscription is always closed on return.
func (s *server[S, O]) handleEvents(w http.ResponseWriter, r *http.Request) {
	sid, err := parseSessionID(r.PathValue("sid"))
	if err != nil {
		writeErrorCause(w, http.StatusBadRequest, codeInvalidParam, msgInvalidSID, false, err)
		return
	}

	sess, ok := s.liveSession(sid)
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

	// done reports the session's death, when the session can report it. It stays NIL
	// otherwise, and that is load-bearing rather than a fallback: a receive on a nil
	// channel blocks forever, so the liveness arm of the stream select is inert and a
	// session that does not satisfy SessionDone streams exactly as it did before this
	// arm existed.
	var done <-chan struct{}
	if reporter, reportsDeath := sess.(SessionDone); reportsDeath {
		done = reporter.Done()
	}

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

	streamEvents(r, w, rc, sub, done, s.cfg.heartbeat)
}

// streamEvents copies deliveries from sub onto w as SSE frames until the client
// disconnects (r.Context cancelled), the session dies (done closed), or the
// subscription ends (channel closed). Each delivery is rendered by encodeDelivery into
// either an `event: enduring` frame (with an id: line stamping d.JournalSeq) or an
// `event: ephemeral` frame (never sequenced); a delivery encodeDelivery rejects — an
// Enduring event outside the sealed union, or an unrecognized Ephemeral event — is
// SKIPPED, never aborting the stream.
//
// An independent ticker emits a `: ping` SSE comment every heartbeat interval so an
// idle stream (and any intermediary) stays alive; it fires on a fixed cadence
// regardless of event activity (simplest correct choice — a client ignores comment
// frames, so an occasional ping alongside real traffic is harmless).
// done ends the stream when the session it belongs to begins shutting down. It is the
// ONLY exit a dead session offers: hub.SubscribeEvents registers unconditionally and
// returns nil even after StopSession, and the hub never closes subscriptions on stop
// (only a consumer Close or an overflow-fail does), so a stopped session delivers
// nothing and closes nothing — without this arm the stream heartbeats forever, pinning
// this goroutine, the hub subscription and the whole dead session until the process
// exits. A NIL done (a session that cannot report death) blocks forever on receive, so
// the arm is simply inert and the loop behaves exactly as it did before.
func streamEvents(r *http.Request, w http.ResponseWriter, rc *http.ResponseController, sub event.Subscription, done <-chan struct{}, heartbeat time.Duration) {
	// Clamp at the point of use: time.NewTicker panics on a non-positive interval, so
	// a future zero-valued config{} literal (or an Option that zeroed heartbeat) can
	// never panic deep in the request path — it falls back to the secure default,
	// matching the fail-safe convention the other options follow.
	hb := heartbeat
	if hb <= 0 {
		hb = defaultHeartbeatInterval
	}
	ticker := time.NewTicker(hb)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-done:
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
