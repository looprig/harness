package hub

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// allFilter delivers every event from every loop in both classes.
func allFilter() event.EventFilter {
	return event.EventFilter{
		Ephemeral: event.LoopScope{All: true},
		Enduring:  event.LoopScope{All: true},
	}
}

// recv reads one event from the subscription within a short timeout, failing the
// test if none arrives.
func recv(t *testing.T, sub *EventSubscription) event.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatalf("Events() closed unexpectedly")
		}
		return ev
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for an event")
		return nil
	}
}

// expectNone asserts no event arrives within a brief window.
func expectNone(t *testing.T, sub *EventSubscription) {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if ok {
			t.Fatalf("unexpected event delivered: %T", ev)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPublishNoSubscribers proves a publish with no subscribers returns nil,
// never blocks, and still applies the sessionState transition (verified via a
// follow-up WaitIdle that must block until the loop goes idle).
func TestPublishNoSubscribers(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)

	// TurnStarted with no subscribers: no error, no block, phase becomes Active.
	if err := h.PublishEvent(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("PublishEvent(TurnStarted) = %v, want nil", err)
	}

	// WaitIdle must block now (session is Active) and unblock after LoopIdle.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	idleErr := make(chan error, 1)
	go func() { idleErr <- h.WaitIdle(ctx) }()

	select {
	case err := <-idleErr:
		t.Fatalf("WaitIdle returned %v before LoopIdle; session was not Active", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := h.PublishEvent(context.Background(), event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("PublishEvent(LoopIdle) = %v, want nil", err)
	}
	if err := <-idleErr; err != nil {
		t.Fatalf("WaitIdle after LoopIdle = %v, want nil", err)
	}
}

// TestPublishFilteredDelivery proves a loop event is delivered to a matching
// subscriber and not to one whose filter excludes that loop. Session events
// always reach both.
func TestPublishFilteredDelivery(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	primary := mustID(t)
	subagent := mustID(t)
	h := New(session)

	// onlyPrimary takes enduring events from the primary loop only.
	onlyPrimary, err := h.SubscribeEvents(event.EventFilter{
		Enduring: event.LoopScope{Loops: map[uuid.UUID]struct{}{primary: {}}},
	})
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}
	all, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	// A StepDone from the subagent: 'all' gets it, 'onlyPrimary' does not.
	if err := h.PublishEvent(context.Background(), event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: subagent}}}); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}
	if got := recv(t, all); got.EventHeader().LoopID != subagent {
		t.Errorf("all subscriber got LoopID %v, want %v", got.EventHeader().LoopID, subagent)
	}
	expectNone(t, onlyPrimary)

	// A StepDone from the primary: both get it.
	if err := h.PublishEvent(context.Background(), event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: primary}}}); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}
	if got := recv(t, onlyPrimary); got.EventHeader().LoopID != primary {
		t.Errorf("onlyPrimary got LoopID %v, want %v", got.EventHeader().LoopID, primary)
	}
}

// TestPublishOrderingWithDerivedPosts proves TurnStarted then LoopIdle deliver
// the loop events AND, in order, the derived SessionActive then SessionIdle.
func TestPublishOrderingWithDerivedPosts(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	if err := h.PublishEvent(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("PublishEvent(TurnStarted) = %v", err)
	}
	if _, ok := recv(t, sub).(event.TurnStarted); !ok {
		t.Fatalf("first event not TurnStarted")
	}
	if _, ok := recv(t, sub).(event.SessionActive); !ok {
		t.Fatalf("second event not derived SessionActive")
	}

	if err := h.PublishEvent(context.Background(), event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("PublishEvent(LoopIdle) = %v", err)
	}
	if _, ok := recv(t, sub).(event.LoopIdle); !ok {
		t.Fatalf("third event not LoopIdle")
	}
	if _, ok := recv(t, sub).(event.SessionIdle); !ok {
		t.Fatalf("fourth event not derived SessionIdle")
	}
}

// TestEphemeralOverflowDrops proves a slow Ephemeral subscriber (full buffer)
// drops TokenDelta without blocking the publisher or other subscribers and
// without failing the subscription.
func TestEphemeralOverflowDrops(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)

	slow, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}
	fast, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	// Flood far past the buffer; slow never reads. Publisher must never block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < defaultEgressBuffer*3; i++ {
			_ = h.PublishEvent(context.Background(), event.TokenDelta{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("publisher blocked on a slow Ephemeral subscriber")
	}

	// slow dropped silently — still live, no error.
	if err := slow.Err(); err != nil {
		t.Errorf("slow Ephemeral subscriber Err() = %v, want nil (dropped, not failed)", err)
	}
	// fast received at least buffer-worth (drain a few to confirm it is not failed).
	if got := recv(t, fast); got.Class() != event.Ephemeral {
		t.Errorf("fast got %T, want a TokenDelta", got)
	}
}

// TestEnduringOverflowFailsSubscription proves an Enduring overflow closes the
// offending subscription with a SubscriptionLossError, does not block others, and
// the publisher still returns nil.
func TestEnduringOverflowFailsSubscription(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)

	// slow never reads, so its egress buffer fills and the (defaultEgressBuffer+1)th
	// Enduring event overflows and fails it. fast is drained in LOCKSTEP with
	// publishing, so its buffer never accumulates and it is never failed — proving a
	// slow subscriber's loss is isolated to that subscriber.
	slow, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}
	fast, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	// PublishEvent fans out synchronously, so on return fast already has the event
	// buffered; draining it each iteration keeps fast's buffer from filling regardless
	// of goroutine scheduling. (A prior version drained fast on a separate goroutine and
	// was flaky under load / GOMAXPROCS=1: the drainer could be starved through the
	// non-blocking publish burst, spuriously overflowing fast.)
	for i := 0; i < defaultEgressBuffer+1; i++ {
		if err := h.PublishEvent(context.Background(), event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
			t.Fatalf("PublishEvent #%d = %v", i, err)
		}
		select {
		case got, ok := <-fast.Events():
			if !ok {
				t.Fatalf("fast.Events() closed at publish #%d — fast was wrongly failed", i)
			}
			if got.Class() != event.Enduring {
				t.Errorf("fast got %T (class %v), want an Enduring StepDone", got, got.Class())
			}
		default:
			t.Fatalf("fast had no buffered event after synchronous publish #%d", i)
		}
	}

	// slow is failed with the typed loss error. Delivery is synchronous, so the
	// overflowing publish above already failed it and closed its channel.
	var lerr *SubscriptionLossError
	if !errors.As(slow.Err(), &lerr) {
		t.Fatalf("slow.Err() = %v, want *SubscriptionLossError", slow.Err())
	}
	if lerr.DroppedClass != event.Enduring {
		t.Errorf("DroppedClass = %v, want Enduring", lerr.DroppedClass)
	}
	// slow's egress channel is closed; draining its buffered events reaches the close.
	for {
		if _, ok := <-slow.Events(); !ok {
			break
		}
	}

	// fast is unaffected: it received every event and is still live.
	if err := fast.Err(); err != nil {
		t.Errorf("fast subscriber Err() = %v, want nil", err)
	}
}

// TestDeliveryOutsideLock proves a subscriber that never reads does not stall a
// second subscriber's SubscribeEvents or another PublishEvent. We fill a slow
// subscriber's buffer with Ephemeral events (so it never fails, just back-pressures
// at the channel), then prove SubscribeEvents and PublishEvent both complete
// promptly while the slow subscriber is parked.
func TestDeliveryOutsideLock(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)

	slow, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}
	_ = slow // never read

	// Saturate slow's egress buffer with Ephemeral events (dropped past cap).
	for i := 0; i < defaultEgressBuffer*2; i++ {
		_ = h.PublishEvent(context.Background(), event.TokenDelta{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}})
	}

	// Now a second SubscribeEvents and a PublishEvent must complete promptly: if
	// delivery happened under the lock, the saturated slow subscriber would have
	// already returned (drops are non-blocking), so to truly exercise out-of-lock
	// we run them concurrently and require quick completion.
	subDone := make(chan struct{})
	go func() {
		defer close(subDone)
		_, _ = h.SubscribeEvents(allFilter())
	}()
	pubDone := make(chan struct{})
	go func() {
		defer close(pubDone)
		_ = h.PublishEvent(context.Background(), event.TokenDelta{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}})
	}()
	for _, c := range []chan struct{}{subDone, pubDone} {
		select {
		case <-c:
		case <-time.After(2 * time.Second):
			t.Fatalf("operation stalled behind a non-reading subscriber (delivery under lock?)")
		}
	}
}

// TestDeliveryOutsideLockBlockingSubscriber is the stronger out-of-lock proof: a
// subscriber whose receive we control blocks delivery for one in-flight publish,
// and we require that a concurrent SubscribeEvents/WaitIdle on the hub does not
// stall. Because egress is non-blocking we instead prove the lock is released by
// confirming a concurrent operation acquires it during a flood.
func TestDeliveryOutsideLockConcurrentOps(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)

	_, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	// One goroutine floods publishes; concurrently we Subscribe many times. None
	// must deadlock under -race.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = h.PublishEvent(context.Background(), event.TokenDelta{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}})
			}
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if _, err := h.SubscribeEvents(allFilter()); err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(stop)
		t.Fatalf("concurrent SubscribeEvents stalled behind publish flood")
	}
	close(stop)
}

// TestStopSession proves stopSession drives the phase to Stopped, wakes a waiting
// WaitIdle with ErrSessionStopped, delivers SessionStopped, makes a fresh WaitIdle
// return ErrSessionStopped immediately, and that a post-stop publish delivers but
// does not mutate active/phase nor derive SessionIdle.
func TestStopSession(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	// Make the session Active so a WaitIdle blocks.
	if err := h.PublishEvent(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}
	_ = recv(t, sub) // TurnStarted
	_ = recv(t, sub) // SessionActive

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waitErr := make(chan error, 1)
	go func() { waitErr <- h.WaitIdle(ctx) }()
	// Ensure the waiter is registered and blocked.
	select {
	case err := <-waitErr:
		t.Fatalf("WaitIdle returned %v before stop; should be blocked (Active)", err)
	case <-time.After(50 * time.Millisecond):
	}

	h.StopSession(context.Background())

	// The blocked WaitIdle wakes with ErrSessionStopped.
	if err := <-waitErr; !errors.Is(err, ErrSessionStopped) {
		t.Fatalf("blocked WaitIdle woke with %v, want ErrSessionStopped", err)
	}
	// SessionStopped event was delivered.
	if _, ok := recv(t, sub).(event.SessionStopped); !ok {
		t.Fatalf("did not deliver SessionStopped")
	}
	// A fresh WaitIdle returns ErrSessionStopped immediately.
	if err := h.WaitIdle(context.Background()); !errors.Is(err, ErrSessionStopped) {
		t.Fatalf("post-stop WaitIdle = %v, want ErrSessionStopped", err)
	}

	// A post-stop publish delivers (filtered) but does not mutate phase nor derive
	// SessionIdle: deliver a LoopIdle (which would normally remove a {loop} key).
	if err := h.PublishEvent(context.Background(), event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("post-stop PublishEvent = %v", err)
	}
	got := recv(t, sub)
	if _, ok := got.(event.LoopIdle); !ok {
		t.Fatalf("post-stop delivered %T, want LoopIdle", got)
	}
	// No derived SessionIdle follows.
	expectNoMore(t, sub)
	// Idempotent stop.
	h.StopSession(context.Background())
}

// expectNoMore asserts no further event arrives briefly (channel stays open).
func expectNoMore(t *testing.T, sub *EventSubscription) {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if ok {
			t.Fatalf("unexpected extra event: %T", ev)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

// TestExpectTurnCancelExpectTurn proves the session-owned wake-token operations
// derive the right edges: expectTurn while idle -> SessionActive; a
// cancelExpectTurn that empties active -> SessionIdle.
func TestExpectTurnCancelExpectTurn(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	subagent := mustID(t)
	h := New(session)
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	h.ExpectTurn(context.Background(), subagent)
	if _, ok := recv(t, sub).(event.SessionActive); !ok {
		t.Fatalf("expectTurn while idle did not derive SessionActive")
	}

	h.CancelExpectTurn(context.Background(), subagent)
	if _, ok := recv(t, sub).(event.SessionIdle); !ok {
		t.Fatalf("cancelExpectTurn emptying active did not derive SessionIdle")
	}
}

// TestExpectTurnCancelExpectTurnDerivedAppendFailure proves the wake-token operations
// are fail-secure when the derived session event's durable append fails: nothing is
// delivered to subscribers, a *SessionPersistenceFault naming the derived event is
// raised, and no blocked WaitIdle waiter is woken with a false nil. Two cases:
//   - ExpectTurn while idle derives SessionActive; its append fails.
//   - CancelExpectTurn emptying active derives SessionIdle; its append fails (the prior
//     ExpectTurn that took the wake token uses a passing appender path).
func TestExpectTurnCancelExpectTurnDerivedAppendFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		op      string // "expectTurn" or "cancelExpectTurn"
		wantEv  event.Event
		blockWI bool // register a blocked WaitIdle that must NOT wake with nil
	}{
		{name: "expectTurn idle->active append fails", op: "expectTurn", wantEv: event.SessionActive{}, blockWI: false},
		{name: "cancelExpectTurn active->idle append fails", op: "cancelExpectTurn", wantEv: event.SessionIdle{}, blockWI: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			session := mustID(t)
			subagent := mustID(t)
			// Fail ONLY the derived event for this case; any earlier append (the
			// wake-take SessionActive in the cancel case) succeeds.
			app := failOnType(tt.wantEv)
			rep := &recordingReporter{}
			h := New(session, WithAppender(app), WithFactory(testFactory()), WithFaultReporter(rep))
			sub, err := h.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents = %v", err)
			}

			var waitErr chan error
			if tt.op == "cancelExpectTurn" {
				// Take the wake token (idle->active) with a passing append, drain the
				// delivered SessionActive, then block a WaitIdle before the failing cancel.
				h.ExpectTurn(context.Background(), subagent)
				if _, ok := recv(t, sub).(event.SessionActive); !ok {
					t.Fatalf("setup expectTurn did not derive SessionActive")
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				waitErr = make(chan error, 1)
				go func() { waitErr <- h.WaitIdle(ctx) }()
				select {
				case err := <-waitErr:
					t.Fatalf("WaitIdle returned %v before cancel; should be blocked (Active)", err)
				case <-time.After(50 * time.Millisecond):
				}
				h.CancelExpectTurn(context.Background(), subagent)
			} else {
				h.ExpectTurn(context.Background(), subagent)
			}

			// Nothing delivered for the failed derived event.
			expectNone(t, sub)

			// The derived event was NOT recorded as appended (its append failed).
			for _, ev := range app.events() {
				if eventTypeName(ev) == eventTypeName(tt.wantEv) {
					t.Errorf("a %T was recorded as appended; its append was supposed to fail", tt.wantEv)
				}
			}

			// Exactly one fault, naming the derived event.
			faults := rep.reported()
			if len(faults) != 1 {
				t.Fatalf("reported %d faults, want 1", len(faults))
			}
			if eventTypeName(faults[0].Event) != eventTypeName(tt.wantEv) {
				t.Errorf("fault.Event = %T, want %T (the derived event whose append failed)", faults[0].Event, tt.wantEv)
			}
			if !errors.Is(faults[0].Cause, errAppend) {
				t.Errorf("fault.Cause = %v, want errAppend", faults[0].Cause)
			}

			// A blocked WaitIdle was NOT woken with a false nil by the failed SessionIdle
			// append: it must still be blocked (the hub's nop reporter does not release
			// it, and signalIdleIfEdge ran after — never before — the failed append).
			if tt.blockWI {
				select {
				case err := <-waitErr:
					t.Fatalf("WaitIdle was woken with %v after a failed SessionIdle append; must stay blocked (no false idle)", err)
				case <-time.After(50 * time.Millisecond):
				}
			}
		})
	}
}

// eventTypeName returns the %T name of an event for type-equality assertions in tables.
func eventTypeName(ev event.Event) string { return reflect.TypeOf(ev).String() }

// TestHandbackReleaseNoEdge proves a TurnStarted carrying Cause.LoopID
// removes the {wake} and adds the {loop} key in one step, crossing no emptiness
// edge: while a wake token is outstanding, the parent's TurnStarted does not
// re-fire SessionActive.
func TestHandbackReleaseNoEdge(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	parent := mustID(t)
	subagent := mustID(t)
	h := New(session)
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	// Spawn: expectTurn takes {wake, subagent}; session goes Active.
	h.ExpectTurn(context.Background(), subagent)
	if _, ok := recv(t, sub).(event.SessionActive); !ok {
		t.Fatalf("expectTurn did not derive SessionActive")
	}

	// Hand-back TurnStarted on the parent: removes {wake, subagent}, adds
	// {loop, parent} in the same step. No edge -> only the TurnStarted is delivered.
	if err := h.PublishEvent(context.Background(), event.TurnStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: session, LoopID: parent},
		Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: subagent}},
	}}); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}
	if _, ok := recv(t, sub).(event.TurnStarted); !ok {
		t.Fatalf("did not deliver TurnStarted")
	}
	expectNoMore(t, sub) // no derived SessionActive/SessionIdle

	// Parent goes idle: {loop, parent} removed -> empties -> SessionIdle.
	if err := h.PublishEvent(context.Background(), event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: parent}}}); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}
	if _, ok := recv(t, sub).(event.LoopIdle); !ok {
		t.Fatalf("did not deliver LoopIdle")
	}
	if _, ok := recv(t, sub).(event.SessionIdle); !ok {
		t.Fatalf("did not derive SessionIdle after parent idle")
	}
}

// TestWaitIdleAlreadyIdle proves WaitIdle returns nil immediately when the
// session is already idle, and respects ctx cancellation while waiting.
func TestWaitIdleAlreadyIdle(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)

	if err := h.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle on a fresh (idle) hub = %v, want nil", err)
	}

	// Make Active, then prove ctx cancellation unblocks a waiter.
	if err := h.PublishEvent(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waitErr := make(chan error, 1)
	go func() { waitErr <- h.WaitIdle(ctx) }()
	select {
	case err := <-waitErr:
		t.Fatalf("WaitIdle returned %v before cancel; should block", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-waitErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitIdle after ctx cancel = %v, want context.Canceled", err)
	}
}

// TestCloseDetachesFromHub proves Close (and a forced loss) removes the
// subscription from the hub's fan-out set, so it does not linger and a subsequent
// publish does not even attempt delivery to it.
func TestCloseDetachesFromHub(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	h.mu.RLock()
	before := len(h.subs)
	h.mu.RUnlock()
	if before != 1 {
		t.Fatalf("subscriber count after Subscribe = %d, want 1", before)
	}

	if err := sub.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	// onClose detaches synchronously.
	h.mu.RLock()
	after := len(h.subs)
	h.mu.RUnlock()
	if after != 0 {
		t.Fatalf("subscriber count after Close = %d, want 0 (detached)", after)
	}

	// A publish after detach is a clean no-op delivery (and never panics).
	if err := h.PublishEvent(context.Background(), event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("PublishEvent after detach = %v", err)
	}
}

// TestConcurrentCloseDuringDelivery proves a subscriber Closing concurrently with
// a publish flood never panics (no send on a closed channel) and never blocks the
// publisher. The race detector + the closed-channel-send guard are what this
// exercises.
func TestConcurrentCloseDuringDelivery(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)

	// Many subscribers; each is closed at a staggered moment while a publisher
	// floods enduring + ephemeral events through the snapshot.
	const subCount = 8
	subs := make([]*EventSubscription, subCount)
	for i := range subs {
		s, err := h.SubscribeEvents(allFilter())
		if err != nil {
			t.Fatalf("SubscribeEvents = %v", err)
		}
		subs[i] = s
		// Drain so enduring events don't fail these before we close them.
		go func(sub *EventSubscription) {
			for range sub.Events() {
			}
		}(s)
	}

	stop := make(chan struct{})
	pubDone := make(chan struct{})
	go func() {
		defer close(pubDone)
		for {
			select {
			case <-stop:
				return
			default:
				_ = h.PublishEvent(context.Background(), event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}})
				_ = h.PublishEvent(context.Background(), event.TokenDelta{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}})
			}
		}
	}()

	// Close every subscription while the flood runs.
	for _, s := range subs {
		if err := s.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}
	}
	close(stop)
	select {
	case <-pubDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("publisher stalled during concurrent close")
	}
}

// TestTurnFoldedAndInputCancelledReleaseWake covers the other two wake-release
// publish paths: TurnFoldedInto and InputCancelled carrying Cause.LoopID
// remove {wake, s}. With the parent already busy, folding empties nothing; an
// InputCancelled when the wake is the only entry empties active and fires Idle.
func TestTurnFoldedAndInputCancelledReleaseWake(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	subagent := mustID(t)
	h := New(session)
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	// Only a wake token outstanding -> Active.
	h.ExpectTurn(context.Background(), subagent)
	if _, ok := recv(t, sub).(event.SessionActive); !ok {
		t.Fatalf("expectTurn did not derive SessionActive")
	}

	// InputCancelled carrying Cause.LoopID removes {wake, subagent}; that was
	// the only entry -> SessionIdle.
	if err := h.PublishEvent(context.Background(), event.InputCancelled{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: session, LoopID: subagent},
		Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: subagent}},
	}}); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}
	if _, ok := recv(t, sub).(event.InputCancelled); !ok {
		t.Fatalf("did not deliver InputCancelled")
	}
	if _, ok := recv(t, sub).(event.SessionIdle); !ok {
		t.Fatalf("did not derive SessionIdle after wake release emptied active")
	}
}
