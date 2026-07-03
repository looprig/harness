package hub

import (
	"context"

	"github.com/looprig/harness/pkg/event"
)

// eventAppender is the hub's narrow durable-write seam: append one Enduring event to
// the session's durable journal, returning a typed error if the append did not commit.
// The hub depends only on this one method (Interface Segregation) — never on the full
// SessionJournal, its stream management, or the record wrappers. The composition root
// (Phase 10) wires a real adapter over SessionJournal; the default is the nop appender
// so existing hub callers and headless/no-persistence mode are unchanged.
//
// AppendEvent must be append-BEFORE-apply: the hub calls it before applying the event
// to hub state, outside the hub lock (no I/O under the lock), and treats any non-nil
// error as fail-secure — the live event is not delivered and a SessionPersistenceFault
// is raised.
type eventAppender interface {
	AppendEvent(ctx context.Context, ev event.Event) error
}

// nopEventAppender is the default appender wired into a hub built without an injected
// one. It persists nothing and never fails, so the hub's fail-secure branch is never
// taken in no-persistence mode — every Enduring event is delivered exactly as before
// the durable tap landed. Headless runs and existing tests use this.
type nopEventAppender struct{}

func (nopEventAppender) AppendEvent(context.Context, event.Event) error { return nil }

// Option configures an optional hub dependency at construction. The bare
// New(sessionID) installs the nop appender, a real-clock/real-uuid Factory, and the
// nop fault reporter; an Option overrides one of them. This keeps existing callers
// (hub.New(id)) working while the composition root injects the durable trio.
type Option func(*Hub)

// WithAppender injects the durable event appender (the composition root's adapter over
// SessionJournal). A nil appender is ignored (the nop default stays installed) so a
// caller can never accidentally null out the field and skip the nil-safe publish path.
func WithAppender(a eventAppender) Option {
	return func(h *Hub) {
		if a != nil {
			h.appender = a
		}
	}
}

// WithFactory injects the event Factory the hub mints EventID+CreatedAt from for the
// session events it SYNTHESIZES (SessionActive/SessionIdle/SessionStopped). A nil
// factory is ignored (the default real-clock Factory stays). The session-scoped
// events the hub derives currently carry no header identity; this factory stamps them
// so the journal sees a stable idempotency key and creation time.
func WithFactory(f *event.Factory) Option {
	return func(h *Hub) {
		if f != nil {
			h.factory = f
		}
	}
}

// WithFaultReporter injects the fail-secure escalation seam invoked when a required
// durable append fails. A nil reporter is ignored (the nop default stays). The Session
// implements it to reject new Submit/NewLoop and wake WaitIdle waiters.
func WithFaultReporter(r FaultReporter) Option {
	return func(h *Hub) {
		if r != nil {
			h.reporter = r
		}
	}
}
