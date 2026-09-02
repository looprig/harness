package hub

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

type blockingAppender struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (a *blockingAppender) AppendEvent(context.Context, event.Event) (uint64, error) {
	if a.calls.Add(1) == 1 {
		close(a.entered)
	}
	<-a.release
	return 1, nil
}

func TestAbortSessionSealsPublicationAndReportsInflightDrain(t *testing.T) {
	t.Parallel()

	app := &blockingAppender{entered: make(chan struct{}), release: make(chan struct{})}
	h := New(mustID(t), WithAppender(app))
	inflightDone := make(chan error, 1)
	go func() {
		inflightDone <- h.PublishEventChecked(context.Background(), event.StepDone{})
	}()
	<-app.entered

	drained := h.AbortSession(errors.New("construction failed"))
	select {
	case <-drained:
		t.Fatal("abort drain closed while an admitted publish still used the appender")
	default:
	}
	err := h.PublishEventChecked(context.Background(), event.StepDone{})
	var aborted *SessionAbortedError
	if !errors.As(err, &aborted) {
		t.Fatalf("late checked publish error = %T %v, want *SessionAbortedError", err, err)
	}
	if got := app.calls.Load(); got != 1 {
		t.Fatalf("appender calls after late publish = %d, want 1", got)
	}

	close(app.release)
	if err := <-inflightDone; err != nil {
		t.Fatalf("admitted PublishEventChecked error = %v", err)
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("abort drain did not close after admitted publish completed")
	}
}

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

func (a *orderingAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	a.calls++
	// Snapshot the watched subscription's egress depth at append time.
	a.bufferedAt = append(a.bufferedAt, len(a.sub.events))
	if a.failAt != 0 && a.calls == a.failAt {
		return 0, errAppend
	}
	a.appended = append(a.appended, ev)
	return uint64(len(a.appended)), nil
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

func TestPublishEventCheckedReturnsPersistenceFault(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	h := New(session, WithAppender(&fakeAppender{failAll: true}))
	ev := event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: mustID(t)}}}
	err := h.PublishEventChecked(context.Background(), ev)
	var fault *SessionPersistenceFault
	if !errors.As(err, &fault) || !errors.Is(err, errAppend) {
		t.Fatalf("PublishEventChecked error = %T %v, want persistence fault wrapping append error", err, err)
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

// faultFailingReporter mirrors the Session's real FaultReporter: on a fault it wakes
// every blocked WaitIdle waiter with that fault (via Hub.FailWaiters), and records the
// fault. It lets a hub unit test prove the fail-secure escalation a SessionIdle
// append failure triggers: a blocked WaitIdle returns the FAULT, never a false nil.
type faultFailingReporter struct {
	h *Hub
	recordingReporter
}

func (r *faultFailingReporter) ReportFault(ctx context.Context, f *SessionPersistenceFault) {
	r.recordingReporter.ReportFault(ctx, f)
	r.h.FailWaiters(f)
}

// TestActiveToIdleDerivedAppendFailureDoesNotFalselyWakeWaitIdle is the dangerous
// window: in-memory state went Active->Idle but the DERIVED SessionIdle's durable
// append failed. The property under test is that signalIdleIfEdge runs only AFTER that
// append, so a blocked WaitIdle is NEVER woken with a false nil "idle"; instead the
// FaultReporter (wired here like the Session's) releases it with the fault. We also
// assert a *SessionPersistenceFault naming SessionIdle was raised and that nothing was
// delivered for the failed derived event.
//
// Guard proof: temporarily moving signalIdleIfEdge BEFORE the append-failure return in
// PublishEvent makes this test FAIL — the blocked WaitIdle would wake with nil before
// the fault.
func TestActiveToIdleDerivedAppendFailureDoesNotFalselyWakeWaitIdle(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	// Fail ONLY the derived SessionIdle append; the TurnStarted, derived SessionActive,
	// and the triggering LoopIdle all persist. (Ordering-robust: fail by type, not index.)
	app := failOnType(event.SessionIdle{})
	rep := &faultFailingReporter{}
	h := New(session, WithAppender(app), WithFactory(testFactory()), WithFaultReporter(rep))
	rep.h = h
	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	// Drive the session Active with a passing appender path: TurnStarted -> SessionActive.
	if err := h.PublishEvent(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("PublishEvent(TurnStarted) = %v", err)
	}
	if _, ok := recv(t, sub).(event.TurnStarted); !ok {
		t.Fatalf("first delivered not TurnStarted")
	}
	if _, ok := recv(t, sub).(event.SessionActive); !ok {
		t.Fatalf("second delivered not the derived SessionActive")
	}

	// Block a WaitIdle (session is Active). It MUST NOT wake with nil.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waitErr := make(chan error, 1)
	go func() { waitErr <- h.WaitIdle(ctx) }()
	select {
	case err := <-waitErr:
		t.Fatalf("WaitIdle returned %v before the Active->Idle edge; should be blocked (Active)", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Publish the Active->Idle trigger. The derived SessionIdle append fails: in-memory
	// state is now idle, but the durable record did not commit -> fail-secure.
	if err := h.PublishEvent(context.Background(), event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("PublishEvent(LoopIdle) = %v", err)
	}

	// The blocked WaitIdle wakes with the FAULT, NOT a false nil. (If signalIdleIfEdge
	// ran before the append-failure return, this would receive nil.)
	gotErr := <-waitErr
	if gotErr == nil {
		t.Fatalf("WaitIdle was falsely woken with nil after a failed SessionIdle append (must report the fault, not idle)")
	}
	var spf *SessionPersistenceFault
	if !errors.As(gotErr, &spf) {
		t.Fatalf("WaitIdle woke with %v, want a *SessionPersistenceFault", gotErr)
	}
	if _, ok := spf.Event.(event.SessionIdle); !ok {
		t.Errorf("WaitIdle fault.Event = %T, want SessionIdle", spf.Event)
	}

	// Exactly one fault, naming the derived SessionIdle whose append failed.
	faults := rep.reported()
	if len(faults) != 1 {
		t.Fatalf("reported %d faults, want 1", len(faults))
	}
	if _, ok := faults[0].Event.(event.SessionIdle); !ok {
		t.Errorf("fault.Event = %T, want SessionIdle (the derived event whose append failed)", faults[0].Event)
	}
	if !errors.Is(faults[0].Cause, errAppend) {
		t.Errorf("fault.Cause = %v, want errAppend", faults[0].Cause)
	}

	// The triggering LoopIdle was appended (it persisted) but the derived SessionIdle
	// was NOT, and neither the LoopIdle nor the SessionIdle was delivered live after
	// the failed derived append.
	appended := app.events()
	for _, ev := range appended {
		if _, ok := ev.(event.SessionIdle); ok {
			t.Errorf("a SessionIdle was recorded as appended; its append was supposed to fail")
		}
	}
	expectNone(t, sub)
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

// idleTrigger builds the two publishes that drive a hub from idle to active and back:
// a TurnStarted that adds loopID to the active set, then the LoopIdle that removes it
// and so derives the SessionIdle edge.
func idleTrigger(t *testing.T, sessionID, loopID uuid.UUID) (event.TurnStarted, event.LoopIdle) {
	t.Helper()
	coords := identity.Coordinates{SessionID: sessionID, LoopID: loopID}
	turnCoords := coords
	turnCoords.TurnID = mustID(t)
	return event.TurnStarted{
			Header: event.Header{
				Coordinates: turnCoords,
				EventID:     mustID(t),
				Cause:       identity.Cause{CommandID: mustID(t)},
			},
			TurnIndex: 1,
		}, event.LoopIdle{
			Header: event.Header{Coordinates: coords, EventID: mustID(t)},
		}
}

// TestDerivedSessionIdleCarriesItsOwnCommittedBody covers the SessionIdle edge derived
// INSIDE a publish — the hub's most common derived transition, and the one committed
// under the idle boundary rather than on the ordinary derived path. It must carry its
// OWN append result: its own sequence, its own committed EventID, its own stored body,
// and a watermark equal to its own sequence.
//
// Two distinct defects hide here and each is asserted separately. Delivering the
// TRIGGERING event's commit alongside the idle event gives two deliveries the same
// (sequence, EventID), so a consumer keying on that pair either drops the idle edge as
// a duplicate or renders the trigger twice. Delivering the idle event with its bytes
// stripped makes it carry no committed body at all, which — on the committed stream,
// whose contract is that every delivery carries them — FAILS the subscription: every
// Host runtime subscription would die the first time a turn went idle.
func TestDerivedSessionIdleCarriesItsOwnCommittedBody(t *testing.T) {
	t.Parallel()
	sessionID, loopID := mustID(t), mustID(t)
	app := newCommittedAppender()
	h := New(sessionID, WithAppender(app))
	committed, err := h.SubscribeCommittedPublicEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents() error = %v", err)
	}

	start, idle := idleTrigger(t, sessionID, loopID)
	if err := h.PublishEvent(context.Background(), start); err != nil {
		t.Fatalf("PublishEvent(TurnStarted) error = %v", err)
	}
	if err := h.PublishEvent(context.Background(), idle); err != nil {
		t.Fatalf("PublishEvent(LoopIdle) error = %v", err)
	}
	// Publication is synchronous on this goroutine, so the subscription's terminal
	// state is already decided. Checking it HERE names the failure precisely: a
	// SessionIdle delivered without its committed bytes fails the committed stream,
	// and read from the channel alone that surfaces only as an unexplained close.
	if err := committed.Err(); err != nil {
		t.Fatalf("committed subscription failed during publication: %v"+
			" (an enduring delivery was not describable from its own committed append)", err)
	}

	wants := []struct {
		name string
		is   func(event.Event) bool
		seq  uint64
	}{
		{name: "TurnStarted", is: func(e event.Event) bool { _, ok := e.(event.TurnStarted); return ok }, seq: 1},
		{name: "SessionActive", is: func(e event.Event) bool { _, ok := e.(event.SessionActive); return ok }, seq: 2},
		{name: "LoopIdle", is: func(e event.Event) bool { _, ok := e.(event.LoopIdle); return ok }, seq: 3},
		{name: "SessionIdle", is: func(e event.Event) bool { _, ok := e.(event.SessionIdle); return ok }, seq: 4},
	}
	bodies := make(map[string][]byte, len(wants))
	for _, want := range wants {
		d := recvDelivery(t, committed)
		if !want.is(d.Event) {
			t.Fatalf("delivery = %T, want %s", d.Event, want.name)
		}
		if d.JournalSeq != want.seq {
			t.Errorf("%s JournalSeq = %d, want %d", want.name, d.JournalSeq, want.seq)
		}
		if d.EventID != d.Event.EventHeader().EventID.String() {
			t.Errorf("%s EventID = %q, want its own header id %q",
				want.name, d.EventID, d.Event.EventHeader().EventID)
		}
		if !bytes.Equal(d.PublicBody, app.storedBody(want.seq)) {
			t.Errorf("%s PublicBody = %s, want the stored body %s", want.name, d.PublicBody, app.storedBody(want.seq))
		}
		if d.CoveredThrough != d.JournalSeq {
			t.Errorf("%s CoveredThrough = %d, want %d", want.name, d.CoveredThrough, d.JournalSeq)
		}
		if !d.Committed() {
			t.Errorf("%s Committed() = false, want true", want.name)
		}
		bodies[want.name] = d.PublicBody
	}
	if bytes.Equal(bodies["SessionIdle"], bodies["LoopIdle"]) {
		t.Errorf("SessionIdle carried the triggering LoopIdle's body %s; each append must carry its own",
			bodies["LoopIdle"])
	}
	if err := committed.Err(); err != nil {
		t.Fatalf("committed subscription failed: %v", err)
	}
}

// TestCancelExpectTurnDerivedSessionIdleCarriesItsOwnCommittedBody covers the OTHER
// SessionIdle site: the edge derived with no triggering event of its own, committed
// through appendAndDeliverDerivedChecked. It is a separate closure from the in-publish
// idle commit and fails independently, so it needs its own driver.
func TestCancelExpectTurnDerivedSessionIdleCarriesItsOwnCommittedBody(t *testing.T) {
	t.Parallel()
	sessionID, subagentLoopID := mustID(t), mustID(t)
	app := newCommittedAppender()
	h := New(sessionID, WithAppender(app))
	committed, err := h.SubscribeCommittedPublicEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents() error = %v", err)
	}

	h.ExpectTurn(context.Background(), subagentLoopID)
	h.CancelExpectTurn(context.Background(), subagentLoopID)
	// Both calls are synchronous, so a failed committed stream is already visible.
	if err := committed.Err(); err != nil {
		t.Fatalf("committed subscription failed during the derived edges: %v"+
			" (an enduring delivery was not describable from its own committed append)", err)
	}

	active := recvDelivery(t, committed)
	if _, ok := active.Event.(event.SessionActive); !ok {
		t.Fatalf("first delivery = %T, want event.SessionActive", active.Event)
	}
	idle := recvDelivery(t, committed)
	if _, ok := idle.Event.(event.SessionIdle); !ok {
		t.Fatalf("second delivery = %T, want event.SessionIdle", idle.Event)
	}
	if idle.JournalSeq != 2 {
		t.Errorf("SessionIdle seq = %d, want 2", idle.JournalSeq)
	}
	if idle.EventID != idle.Event.EventHeader().EventID.String() {
		t.Errorf("SessionIdle EventID = %q, want its own header id %q",
			idle.EventID, idle.Event.EventHeader().EventID)
	}
	if idle.EventID == active.EventID {
		t.Errorf("SessionIdle carried SessionActive's committed id %q", active.EventID)
	}
	if !bytes.Equal(idle.PublicBody, app.storedBody(2)) {
		t.Errorf("SessionIdle PublicBody = %s, want the stored body %s", idle.PublicBody, app.storedBody(2))
	}
	if idle.CoveredThrough != idle.JournalSeq {
		t.Errorf("SessionIdle CoveredThrough = %d, want %d", idle.CoveredThrough, idle.JournalSeq)
	}
	if !idle.Committed() {
		t.Error("SessionIdle Committed() = false, want true")
	}
	if bytes.Equal(idle.PublicBody, active.PublicBody) {
		t.Errorf("SessionIdle carried SessionActive's body %s; each append must carry its own", active.PublicBody)
	}
	if err := committed.Err(); err != nil {
		t.Fatalf("committed subscription failed: %v", err)
	}
}
