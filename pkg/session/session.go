// Package session exposes the live session data-plane and control-plane contracts.
// Session construction and restoration are owned exclusively by package rig.
package session

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/workspacestore"
)

// Session is the ordinary data-plane view of one live rig execution.
type Session interface {
	SessionID() uuid.UUID
	ActiveLoop() loop.Handle
	Loop(uuid.UUID) (loop.Handle, bool)
	Submit(context.Context, []content.Block) (uuid.UUID, error)
	SubmitToLoop(context.Context, uuid.UUID, []content.Block) (uuid.UUID, error)
	Compact(context.Context) (uuid.UUID, error)
	CompactToLoop(context.Context, uuid.UUID) (uuid.UUID, error)
	SubscribeEvents(event.EventFilter) (event.Subscription, error)
	RespondGate(context.Context, gate.GateResponse) error
	Interrupt(context.Context) (bool, error)
}

// GateHost is the capability to raise a HOST-OWNED gate: to put a structured
// question or an out-of-band action to a human and receive the answer directly.
//
// It is a SEPARATE contract rather than three more methods on SessionController,
// for two independent reasons.
//
// The first is segregation. Opening a gate is not part of running a session:
// almost every consumer of a SessionController — the TUI, the CLI, a test —
// submits work, watches events, and answers gates, and none of them raise one.
// Widening SessionController would force every implementation to grow three
// methods that only an integration host calls, which is the exact coupling the
// interface rules forbid.
//
// The second is that the two contracts have different holders. A SessionController
// is the session's operator. A GateHost is whatever opened a particular gate and
// is blocked on its answer — an MCP binding servicing an elicitation, say. Those
// are the two ends of the same gate, and RespondGate (on Session) is the other
// end: a client answers, the host receives. Keeping them separate keeps that
// asymmetry visible instead of collapsing both roles into one god-interface.
//
// A live session implements it, so a host obtains one by asserting on the
// controller rig returns:
//
//	host, ok := controller.(session.GateHost)
//
// The contract is host-owned gates ONLY (gate.KindForm and gate.KindOpenURL with
// gate.ResolverSession). There is deliberately no way to open a permission or
// ask-user gate through it: those are answered by resuming a parked loop, and a
// host that could mint one could park — or forge an approval against — a loop
// that is not its own. An implementation MUST refuse anything else at open time
// rather than at answer time, so a caller learns its request was invalid before a
// human is shown a prompt that can never be delivered.
type GateHost interface {
	// OpenHostGate opens g and returns its id. The gate is public and answerable
	// when it returns. The caller MUST then either AwaitGateAnswer or CloseGate;
	// abandoning it without either leaks the answer slot for the session's life.
	OpenHostGate(context.Context, uuid.UUID, gate.Gate, gate.Payload) (gate.ID, error)
	// AwaitGateAnswer blocks until the gate is answered and returns the validated
	// answer, including the form values that are absent from every durable record.
	// An answer is delivered exactly once. Cancelling the context abandons the
	// wait and frees the slot but does NOT close the gate — the gate is durable
	// state and the context is the caller's, so an opener that gives up must
	// CloseGate.
	AwaitGateAnswer(context.Context, gate.ID) (gate.Answer, error)
	// CloseGate withdraws a gate without answering it, waking any awaiter with a
	// *GateError{GateNotFound}. It is how an opener cleans up after a cancelled or
	// timed-out request.
	CloseGate(context.Context, gate.ID, gate.CloseReason) error
}

// SessionController is the trusted policy and lifecycle view of a Session.
type SessionController interface {
	Session
	SetActiveLoop(context.Context, uuid.UUID) error
	LoopController(uuid.UUID) (loop.Controller, bool)
	CheckpointWorkspace(context.Context) (workspacestore.Ref, error)
	RestoreWorkspace(context.Context, workspacestore.Ref) error
	Shutdown(context.Context) error
}

// CommittedPublicEventSource is the segregated committed-public-event capability: a
// live event stream on which EVERY delivery carries the exact canonical public body
// the durable append stored, the public EventID it committed under, and a
// CoveredThrough watermark equal to that append's own sequence.
//
// It is a SEPARATE contract from Session.SubscribeEvents, and the difference is not
// cosmetic. SubscribeEvents is the compatibility stream: it serves a TUI/CLI on a
// headless session with no persistence at all, it carries ephemeral events, and it
// promises nothing about bytes. This one promises committed bytes on every delivery,
// which a session whose persistence cannot report the stored bytes is unable to keep.
// Folding it into Session would force every implementation to advertise a guarantee
// only some of them can honor — and a consumer that joins a durable tail to a live
// stream would have no way to learn, before it starts persisting cursors, that this
// session was not one of them.
//
// A consumer obtains one through CommittedPublicEventProvider rather than a bare type
// assertion, because the capability is a property of the session's persistence, not
// of its Go type.
// Two notes for a consumer building tail-join logic on this stream.
//
// First, the live delivery is the more available of the two sources. It always
// carries the committed bytes; the durable public read does not, for a body large
// enough to be offloaded above the released reader's inline ceiling (see the caveat
// on event.Delivery.PublicBody, which states the exact condition). Join by taking the
// live bytes as authoritative for any sequence you already hold, and never discard a
// held body because a read of the same sequence failed.
//
// Second, a subscription on this stream can terminate with *hub.SubscriptionLossError
// for two DIFFERENT reasons, and they call for opposite responses. Egress overflow is
// congestion: resubscribe and resync. A loss wrapping hub.ErrCommittedBodyMissing is a
// broken invariant — the hub delivered an enduring public event with no committed
// body — and resubscribing loops forever against a hub that cannot satisfy the
// contract. Check errors.Is before retrying.
type CommittedPublicEventSource interface {
	// SubscribeCommittedPublicEvents attaches a consumer to the committed public
	// stream with the given filter. The caller must Close the returned subscription.
	SubscribeCommittedPublicEvents(event.EventFilter) (event.Subscription, error)
}

// CommittedPublicEventProvider is implemented by a session that MAY be able to serve
// committed public events. CommittedPublicEvents reports the capability: ok is false
// — with a nil source — when this session's persistence cannot report the exact
// canonical bytes it stored, which includes a headless/no-persistence session and one
// over a journal that predates the committed-bytes seam.
//
// The two-result form is the point. A single-result form would hand back a source
// that fails only once the consumer is already subscribed and already advancing a
// cursor, which is exactly the shape of failure this capability exists to prevent.
type CommittedPublicEventProvider interface {
	CommittedPublicEvents() (CommittedPublicEventSource, bool)
}
