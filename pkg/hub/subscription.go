package hub

import (
	"sync"

	"github.com/looprig/harness/pkg/event"
)

// defaultEgressBuffer is the capacity of each subscription's bounded egress
// channel. One slow subscriber must never block a publisher or another
// subscriber, so delivery is a non-blocking send into this buffer; on overflow
// the class-aware policy (drop Ephemeral / fail-close Enduring) applies.
const defaultEgressBuffer = 256

// SubscriptionLossError is the typed terminal recorded on a subscription the hub
// fails rather than let it silently miss an authoritative event. DroppedClass is the
// class of the event that triggered the loss; Cause names the reason when it is not
// the default one.
//
// The reasons call for OPPOSITE responses, which is why Cause exists rather than one
// undifferentiated loss. A nil Cause is egress overflow: the subscriber fell behind,
// and re-subscribing to re-sync is the right answer. ErrCommittedBodyMissing and
// ErrCommitEventMismatch are both broken invariants — an enduring public event that
// arrived without its committed bytes, and a delivery whose committed append belongs to
// a different event — where re-subscribing loops forever against a hub that cannot
// satisfy the contract.
type SubscriptionLossError struct {
	DroppedClass event.Class
	Cause        error
}

func (e *SubscriptionLossError) Error() string {
	// Only the causeless form may name egress overflow: that is the loss the hub
	// raises on its own, with nothing further to say. A loss WITH a cause has a
	// different reason (see ErrCommittedBodyMissing), and repeating "egress
	// overflow" in front of it would state a cause the hub knows to be wrong.
	if e.Cause == nil {
		return "hub: subscription lost (egress overflow on enduring event)"
	}
	return "hub: subscription lost: " + e.Cause.Error()
}

func (e *SubscriptionLossError) Unwrap() error { return e.Cause }

// sendResult reports the outcome of a non-blocking egress send.
type sendResult uint8

const (
	sendDelivered sendResult = iota // the event entered the egress buffer
	sendFull                        // the buffer was full (overflow policy applies)
	sendClosed                      // the subscription was already torn down (skip it)
)

// EventSubscription is a consumer's handle to the session fan-in. It owns exactly
// one bounded egress channel. Events closes when the subscriber Closes it or when
// the hub fails it for loss. Err returns nil for an intentional Close and the
// typed SubscriptionLossError for a hub-forced termination. SessionStopped is an
// event, not a stream terminator: a subscription ends only on Close, loss, or hub
// teardown.
type EventSubscription struct {
	// filter is the subscriber's declared interest, evaluated by the hub at
	// fan-out before the bounded send.
	filter event.EventFilter

	// committedOnly marks a subscription obtained through the segregated
	// committed-public-event capability. Such a subscription carries a stronger
	// contract than the compatibility stream: EVERY delivery on it carries the
	// committed canonical public body of a public enduring event. The hub honors
	// that by skipping ephemeral events (reconstructable, never persisted, so they
	// are not a coverage gap) and by FAILING the subscription rather than passing
	// an enduring event with no committed bytes — a silent skip there would leave
	// the consumer an invisible hole in the sequence coverage it is about to
	// persist as a cursor.
	committedOnly bool

	// events is the single bounded egress channel. The hub is the sole sender
	// (non-blocking, via trySend); the subscriber is the sole receiver. It carries
	// event.Delivery (the event plus its durable journal sequence), so a live SSE
	// consumer can stamp id:<journal_seq> without the seq ever entering the codec.
	events chan event.Delivery

	// mu serializes a hub-side send against teardown so a non-blocking send can
	// never race the close of events (which would panic). It also guards closed
	// and err. It is a per-subscription lock, taken outside the hub lock, so it
	// never serializes unrelated subscribers.
	mu     sync.Mutex
	closed bool
	err    error

	// onClose detaches this subscription from the hub's subscriber set on the
	// first terminal (Close or fail), so a closed subscription does not linger in
	// the set forever. It is a callback (not a hub back-reference) to keep the
	// subscription decoupled from the concrete hub. Invoked exactly once, outside
	// the per-subscription lock to avoid a lock-order inversion with the hub lock.
	onClose func(*EventSubscription)
}

// newSubscription builds a subscription with its bounded egress channel, the
// subscriber's filter, and the detach callback. The hub calls this under its
// write lock when registering. onClose may be nil in unit tests that exercise the
// subscription in isolation.
func newSubscription(filter event.EventFilter, onClose func(*EventSubscription)) *EventSubscription {
	return &EventSubscription{
		filter:  filter,
		events:  make(chan event.Delivery, defaultEgressBuffer),
		onClose: onClose,
	}
}

// newCommittedSubscription builds a subscription for the segregated
// committed-public-event capability. It differs from newSubscription only in the
// committedOnly flag; the egress channel, filter, and teardown are identical, so a
// Host consumer and a TUI consumer are the same kind of subscriber to the hub.
func newCommittedSubscription(filter event.EventFilter, onClose func(*EventSubscription)) *EventSubscription {
	sub := newSubscription(filter, onClose)
	sub.committedOnly = true
	return sub
}

// Events is the receive end of the subscription's egress channel. It is closed on
// Close, loss, or hub teardown.
func (s *EventSubscription) Events() <-chan event.Delivery { return s.events }

// Close is the subscriber's intentional teardown. It is idempotent and records no
// error (Err stays nil). The first terminal — Close or a hub fail — wins, so a
// Close after a loss does not clobber the recorded loss error.
func (s *EventSubscription) Close() error {
	s.terminate(nil)
	return nil
}

// Err returns nil for an intentional Close and the typed SubscriptionLossError for
// a hub-forced loss. It returns nil while the subscription is still live.
func (s *EventSubscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// trySend performs the hub's non-blocking egress send under the per-subscription
// lock, so it can never race teardown. If the subscription is already torn down it
// returns sendClosed (the hub skips it); if the buffer is full it returns sendFull
// (the hub applies the overflow policy); otherwise the event is buffered and it
// returns sendDelivered. Only the hub calls this.
func (s *EventSubscription) trySend(d event.Delivery) sendResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sendClosed
	}
	select {
	case s.events <- d:
		return sendDelivered
	default:
		return sendFull
	}
}

// fail is the hub-forced termination path: it records the typed loss error and
// closes the egress channel. Only the hub calls it. It is idempotent and only the
// first terminal cause is recorded.
func (s *EventSubscription) fail(err error) { s.terminate(err) }

// terminate closes the egress channel exactly once and records the first terminal
// cause (nil for Close, the loss error for fail). The per-subscription lock makes
// the close mutually exclusive with trySend, so no send can ever fire on a closed
// channel. The onClose detach runs AFTER releasing the per-subscription lock, so
// it can take the hub lock without a lock-order inversion against trySend/deliver.
func (s *EventSubscription) terminate(cause error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return // first terminal wins; idempotent
	}
	s.closed = true
	s.err = cause
	close(s.events)
	s.mu.Unlock()

	if s.onClose != nil {
		s.onClose(s)
	}
}
