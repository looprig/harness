// Package serve hosts the HTTP surface over a live session.
//
// It is the composition seam between the outside world (HTTP clients) and the
// in-process session machinery, and it obeys strict Dependency Inversion: the
// production package couples ONLY to the narrow interfaces declared here plus the
// leaf value types those interfaces mention (pkg/event, pkg/gate, core/content,
// core/uuid) and the standard library. It NEVER imports pkg/session, any LLM
// package, or any store package — those concrete types are wired in at the
// composition root and reach serve exclusively through LiveSession and Runner.
//
// LiveSession is the per-session control surface an HTTP handler drives (submit
// input, subscribe to the event stream, answer a gate, interrupt). Runner is the
// session factory the handler calls to bring a new session up (Run) or resume a
// prior one (Restore). Both are satisfied structurally by the real session types
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
// (Interface Segregation). The concrete *session.Session satisfies it
// structurally; serve never names that type.
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
	Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error)
	SubscribeEvents(filter event.EventFilter) (event.Subscription, error)
	RespondGate(ctx context.Context, response gate.GateResponse) error
	Interrupt(ctx context.Context) (bool, error)
}

// Runner is the narrow session-factory view serve depends on. It is generic over the
// concrete live-session type S (constrained to LiveSession) so a caller keeps the
// real type through Run/Restore without serve importing it: the composition root
// instantiates Runner[*session.Session], and serve holds only Runner[S].
//
//   - Run mints a fresh session id and brings up a brand-new live session.
//   - Restore rebuilds a prior session from its durable history by id.
type Runner[S LiveSession] interface {
	Run(ctx context.Context) (uuid.UUID, S, error)
	Restore(ctx context.Context, id uuid.UUID) (S, error)
}
