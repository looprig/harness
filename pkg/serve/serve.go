// Package serve hosts the HTTP surface over a live session.
//
// It is the composition seam between the outside world (HTTP clients) and the
// in-process session machinery, and it obeys strict Dependency Inversion: the
// production package couples ONLY to the narrow interfaces declared here plus the
// leaf value types those interfaces mention (pkg/event, pkg/gate, core/content,
// core/uuid) and the standard library. It NEVER imports pkg/session, any LLM
// package, or any store package — those concrete types are wired in at the
// composition root and reach serve exclusively through LiveSession and Rig.
//
// LiveSession is the per-session control surface an HTTP handler drives (submit
// input, subscribe to the event stream, answer a gate, interrupt). Rig is the
// session factory the handler calls to bring a new session up (NewSession) or resume a
// prior one (RestoreSession). Both are satisfied structurally by the real session contracts
// (proven in the package's dependency-guard test), so serve depends on the
// behavior without depending on the implementation.
package serve

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
)

// LiveSession is the narrow, HTTP-facing view of a running session: the exact
// method set an HTTP handler needs to drive one session and nothing more
// (Interface Segregation). session.SessionController satisfies it structurally at the
// composition root; serve never imports or names that contract in production.
//
//   - Submit queues human-authored input to the session's primary loop and returns
//     the minted input id (fire-and-forget; the outcome is observed on the event
//     stream, correlated by that id).
//   - SubscribeEvents attaches a filtered consumer to the session fan-in; the caller
//     Closes the returned Subscription when done.
//   - RespondGate delivers a human's answer to an open approval gate.
//   - Interrupt cancels every in-flight turn in the session, reporting whether any
//     running turn was actually cancelled.
type LiveSession interface {
	SessionID() uuid.UUID
	Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error)
	SubscribeEvents(filter event.EventFilter) (event.Subscription, error)
	RespondGate(ctx context.Context, response gate.GateResponse) error
	Interrupt(ctx context.Context) (bool, error)
}

// SessionDone is the OPTIONAL liveness extension of LiveSession: a session that can
// report its own death exposes a channel closed once its shutdown has begun.
//
// It is deliberately separate from LiveSession rather than folded into it. A session
// is drivable whether or not it can report death, so requiring the method would force
// every implementor (tui, acp, consumer fakes) to grow one for a capability most of
// them do not have — an Interface Segregation violation, and a compile break across
// three repositories for one optional bit. Being structural, it also means
// session.SessionController need not widen: serve type-asserts the DYNAMIC type, and
// *sessionruntime.Session satisfies it today.
//
// Absence is not death. A session that does not satisfy SessionDone is treated as live
// forever, which is exactly today's behaviour; only a session that opts in can be
// evicted. A wrapper around a live session MUST forward Done, or it silently opts its
// wrapped session out and reintroduces the corpse-pinning leak this exists to fix.
type SessionDone interface {
	// Done returns a channel closed once the session has begun shutting down. It never
	// reopens, and a receive means "admits no new work", not "cleanup finished".
	Done() <-chan struct{}
}

// Rig is the narrow session-factory view serve depends on. It is generic over the
// concrete live-session type S (constrained to LiveSession) so a caller keeps the
// real type through NewSession/RestoreSession without serve importing it: the composition
// root instantiates Rig[session.SessionController, rig.SessionOption], while serve
// remains independent of both concrete packages.
//
//   - NewSession brings up a brand-new live session that exposes its minted ID.
//   - RestoreSession rebuilds a prior session from its durable history by id.
type Rig[S LiveSession, O any] interface {
	NewSession(ctx context.Context, opts ...O) (S, error)
	RestoreSession(ctx context.Context, id uuid.UUID) (S, error)
}
