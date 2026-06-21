package hub

import (
	"context"
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
)

// orderingAppender records, on each AppendEvent call, a snapshot of how many events a
// watched subscription has already buffered. Because a publish is synchronous on the
// caller's goroutine, "0 buffered at append time" proves the append happened BEFORE any
// delivery to that subscriber (append-before-apply). It can also fail a chosen call.
type orderingAppender struct {
	sub        *EventSubscription
	appended   []event.Event
	bufferedAt []int // len(sub.events) observed at each append call
	failAt     int   // 1-based call index to fail; 0 = never
	calls      int
}

func (a *orderingAppender) AppendEvent(_ context.Context, ev event.Event) error {
	a.calls++
	// Snapshot the watched subscription's egress depth at append time.
	a.bufferedAt = append(a.bufferedAt, len(a.sub.events))
	if a.failAt != 0 && a.calls == a.failAt {
		return errAppend
	}
	a.appended = append(a.appended, ev)
	return nil
}

// TestEnduringAppendsBeforeDelivery proves an Enduring event is appended BEFORE it is
// delivered to any subscriber: at the append call the subscriber's egress buffer is
// still empty, and after the publish the event is both appended and delivered.
func TestEnduringAppendsBeforeDelivery(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}
	app := &orderingAppender{sub: sub}
	h.appender = app // single-goroutine test, set before any publish

	ev := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}
	if err := h.PublishEvent(context.Background(), ev); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}

	if len(app.appended) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.appended))
	}
	if app.bufferedAt[0] != 0 {
		t.Errorf("subscriber had %d buffered events at append time, want 0 (delivery before append)", app.bufferedAt[0])
	}
	if _, ok := recv(t, sub).(event.StepDone); !ok {
		t.Fatalf("StepDone was not delivered after a successful append")
	}
}

// TestEnduringAppendErrorNoDelivery proves a failed Enduring append delivers NOTHING
// and raises a *SessionPersistenceFault carrying the offending event and the cause.
func TestEnduringAppendErrorNoDelivery(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	rep := &recordingReporter{}
	h := New(session, WithFaultReporter(rep))
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}
	h.appender = &fakeAppender{failAll: true}

	ev := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}
	if err := h.PublishEvent(context.Background(), ev); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}

	// Nothing delivered.
	expectNone(t, sub)
	// Exactly one fault, of the right concrete type, naming the offending event.
	faults := rep.reported()
	if len(faults) != 1 {
		t.Fatalf("reported %d faults, want 1", len(faults))
	}
	var spf *SessionPersistenceFault
	if !errors.As(error(faults[0]), &spf) {
		t.Fatalf("fault is not a *SessionPersistenceFault: %v", faults[0])
	}
	if _, ok := spf.Event.(event.StepDone); !ok {
		t.Errorf("fault.Event = %T, want event.StepDone", spf.Event)
	}
	if !errors.Is(spf.Cause, errAppend) {
		t.Errorf("fault.Cause = %v, want errAppend", spf.Cause)
	}
}

// TestEphemeralNotAppended proves an Ephemeral event is NEVER appended (it is
// reconstructable from a later authoritative event) and is delivered normally.
func TestEphemeralNotAppended(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}
	app := &fakeAppender{}
	h.appender = app

	if err := h.PublishEvent(context.Background(), event.TokenDelta{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}

	if app.callCount() != 0 {
		t.Errorf("appender called %d times for an Ephemeral event, want 0", app.callCount())
	}
	if got := recv(t, sub); got.Class() != event.Ephemeral {
		t.Errorf("Ephemeral event not delivered: got %T", got)
	}
}

// TestDerivedSessionActiveAppendOrder proves an Idle->Active trigger appends the
// TRIGGER first, then MINTS (factory), APPENDS, and DELIVERS the derived
// SessionActive — in that causal order. The derived event carries a non-zero
// EventID + CreatedAt (it was stamped by the factory, not delivered raw).
func TestDerivedSessionActiveAppendOrder(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	app := &fakeAppender{}
	h := New(session, WithAppender(app), WithFactory(testFactory()))
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	trigger := event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}
	if err := h.PublishEvent(context.Background(), trigger); err != nil {
		t.Fatalf("PublishEvent(TurnStarted) = %v", err)
	}

	// Appended in causal order: trigger, then derived SessionActive.
	appended := app.events()
	if len(appended) != 2 {
		t.Fatalf("appended %d events, want 2 (trigger + derived)", len(appended))
	}
	if _, ok := appended[0].(event.TurnStarted); !ok {
		t.Errorf("first appended = %T, want TurnStarted", appended[0])
	}
	active, ok := appended[1].(event.SessionActive)
	if !ok {
		t.Fatalf("second appended = %T, want SessionActive", appended[1])
	}
	// The derived event was minted: non-zero EventID + CreatedAt.
	if active.EventID.IsZero() {
		t.Errorf("derived SessionActive has a zero EventID (not minted by the factory)")
	}
	if active.CreatedAt.IsZero() {
		t.Errorf("derived SessionActive has a zero CreatedAt (not minted by the factory)")
	}
	if active.SessionID != session {
		t.Errorf("derived SessionActive SessionID = %v, want %v", active.SessionID, session)
	}

	// Delivered in causal order: trigger, then derived SessionActive.
	if _, ok := recv(t, sub).(event.TurnStarted); !ok {
		t.Fatalf("first delivered not TurnStarted")
	}
	if _, ok := recv(t, sub).(event.SessionActive); !ok {
		t.Fatalf("second delivered not the derived SessionActive")
	}
}

// TestDerivedAppendFailureNeitherDelivered proves that when the DERIVED session
// event's append fails (trigger append succeeded), NEITHER the trigger nor the
// derived event is delivered live, and a fault naming the derived event is raised.
func TestDerivedAppendFailureNeitherDelivered(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	rep := &recordingReporter{}
	// failAt=2: the trigger (call 1) succeeds; the derived SessionActive (call 2) fails.
	app := &fakeAppender{failAt: 2}
	h := New(session, WithAppender(app), WithFactory(testFactory()), WithFaultReporter(rep))
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	trigger := event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}
	if err := h.PublishEvent(context.Background(), trigger); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}

	// Neither the trigger nor the derived event reached the subscriber.
	expectNone(t, sub)
	faults := rep.reported()
	if len(faults) != 1 {
		t.Fatalf("reported %d faults, want 1", len(faults))
	}
	if _, ok := faults[0].Event.(event.SessionActive); !ok {
		t.Errorf("fault.Event = %T, want SessionActive (the derived event whose append failed)", faults[0].Event)
	}
}

// TestStopSessionAppendsBeforeDeliver proves StopSession mints + appends the
// synthesized SessionStopped BEFORE delivering it, and that on an append failure it
// delivers nothing and raises a fault (fail-secure). The phase still flips and
// waiters still wake on the SUCCESS path (the durable record precedes the live edge).
func TestStopSessionAppendsBeforeDeliver(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		failAll     bool
		wantDeliver bool
		wantFault   bool
	}{
		{name: "append succeeds: minted, appended, delivered", failAll: false, wantDeliver: true, wantFault: false},
		{name: "append fails: nothing delivered, fault raised", failAll: true, wantDeliver: false, wantFault: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			session := mustID(t)
			app := &fakeAppender{failAll: tt.failAll}
			rep := &recordingReporter{}
			h := New(session, WithAppender(app), WithFactory(testFactory()), WithFaultReporter(rep))
			sub, err := h.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents = %v", err)
			}

			h.StopSession(context.Background())

			if tt.wantDeliver {
				stopped, ok := recv(t, sub).(event.SessionStopped)
				if !ok {
					t.Fatalf("did not deliver SessionStopped on the success path")
				}
				if stopped.EventID.IsZero() || stopped.CreatedAt.IsZero() {
					t.Errorf("delivered SessionStopped was not minted: %+v", stopped.Header)
				}
				if got := app.events(); len(got) != 1 {
					t.Errorf("appended %d events, want 1 (SessionStopped)", len(got))
				}
			} else {
				expectNone(t, sub)
			}

			if tt.wantFault {
				faults := rep.reported()
				if len(faults) != 1 {
					t.Fatalf("reported %d faults, want 1", len(faults))
				}
				if _, ok := faults[0].Event.(event.SessionStopped); !ok {
					t.Errorf("fault.Event = %T, want SessionStopped", faults[0].Event)
				}
			} else if got := len(rep.reported()); got != 0 {
				t.Errorf("reported %d faults on the success path, want 0", got)
			}
		})
	}
}

// TestPostStopNoAppendForNonMutating proves that after SessionStopped, a non-mutating
// Enduring publish is still appended (the durable tap is unconditional for Enduring),
// but derives no session event — so exactly one append (the event itself) occurs.
func TestPostStopNoDerivedAppend(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	app := &fakeAppender{}
	h := New(session, WithAppender(app), WithFactory(testFactory()))
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	h.StopSession(context.Background())
	_ = recv(t, sub) // SessionStopped (1st append)
	before := len(app.events())

	// A LoopIdle after stop: appended (Enduring), delivered, but derives no SessionIdle.
	if err := h.PublishEvent(context.Background(), event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("post-stop PublishEvent = %v", err)
	}
	if _, ok := recv(t, sub).(event.LoopIdle); !ok {
		t.Fatalf("post-stop did not deliver LoopIdle")
	}
	expectNoMore(t, sub) // no derived SessionIdle
	if got := len(app.events()) - before; got != 1 {
		t.Errorf("post-stop appended %d events, want 1 (the LoopIdle only, no derived)", got)
	}
}
