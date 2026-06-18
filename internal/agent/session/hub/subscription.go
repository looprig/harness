package hub

import (
	"sync"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
)

// defaultEgressBuffer is the capacity of each subscription's bounded egress
// channel. One slow subscriber must never block a publisher or another
// subscriber, so delivery is a non-blocking send into this buffer; on overflow
// the class-aware policy (drop Ephemeral / fail-close Enduring) applies.
const defaultEgressBuffer = 256

// SubscriptionLossError is the typed terminal recorded on a subscription the hub
// fails because an Enduring event would have overflowed its egress buffer. The
// subscriber learns it lost the stream (so it can re-subscribe and re-sync)
// rather than silently missing an authoritative event. DroppedClass is the class
// of the event that triggered the loss; Cause is an optional underlying error.
type SubscriptionLossError struct {
	DroppedClass event.Class
	Cause        error
}

func (e *SubscriptionLossError) Error() string {
	msg := "hub: subscription lost (egress overflow on enduring event)"
	if e.Cause == nil {
		return msg
	}
	return msg + ": " + e.Cause.Error()
}

func (e *SubscriptionLossError) Unwrap() error { return e.Cause }

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

	// events is the single bounded egress channel. The hub is the sole sender
	// (non-blocking); the subscriber is the sole receiver.
	events chan event.Event

	// once guards the single close of events so neither a double Close nor a
	// Close racing a fail can panic on a closed channel.
	once sync.Once

	// errMu guards err. err is written exactly once (inside once.Do) and read by
	// Err from the subscriber goroutine, so it needs its own guard.
	errMu sync.Mutex
	err   error
}

// newSubscription builds a subscription with its bounded egress channel and the
// subscriber's filter. The hub calls this under its write lock when registering.
func newSubscription(filter event.EventFilter) *EventSubscription {
	return &EventSubscription{
		filter: filter,
		events: make(chan event.Event, defaultEgressBuffer),
	}
}

// Events is the receive end of the subscription's egress channel. It is closed on
// Close, loss, or hub teardown.
func (s *EventSubscription) Events() <-chan event.Event { return s.events }

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
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// fail is the hub-forced termination path: it records the typed loss error and
// closes the egress channel. Only the hub calls it. It is idempotent and only the
// first terminal cause is recorded.
func (s *EventSubscription) fail(err error) { s.terminate(err) }

// terminate closes the egress channel exactly once and records the first terminal
// cause (nil for Close, the loss error for fail). sync.Once makes the close
// panic-free under any interleaving of Close and fail.
func (s *EventSubscription) terminate(cause error) {
	s.once.Do(func() {
		s.errMu.Lock()
		s.err = cause
		s.errMu.Unlock()
		close(s.events)
	})
}
