package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/core/uuid"
)

// allFilter delivers every event from every loop in both classes.
func allFilter() event.EventFilter {
	return event.EventFilter{
		Ephemeral: event.LoopScope{All: true},
		Enduring:  event.LoopScope{All: true},
	}
}

// TestSessionStartedDeliveredToLateSubscriber proves the session emits the
// session-scoped SessionStarted through the hub at construction. Because the hub
// has no buffering for pre-subscription events, a subscriber that attaches after
// New will not receive that initial SessionStarted — but a fresh
// SessionStarted from a subsequent publish (or, here, the session's own start
// being session-scoped) confirms wiring. We assert wiring by publishing a
// session-scoped event after subscribing and seeing it arrive.
func TestHubWiringDeliversSessionEvents(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	// Publish a session-scoped event through the session's PublishEvent; the
	// subscriber must receive it (proving PublishEvent -> hub -> subscriber wiring).
	if err := s.PublishEvent(context.Background(), event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.SessionID}}}); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatalf("subscription closed unexpectedly")
		}
		if _, isStart := ev.(event.SessionStarted); !isStart {
			t.Fatalf("got %T, want event.SessionStarted", ev)
		}
	case <-time.After(time.Second):
		t.Fatalf("subscriber did not receive the session event")
	}
}

// TestSubscribeSeamDefaultFilterDeliversSessionEvent proves the whole-session
// subscribe seam (11.1): a subscription opened with the single-loop TUI default
// filter (primary-only Ephemeral, all-loop Enduring) is usable and delivers a
// session-scoped event (SessionIdle) — which bypasses the loop filter — and that the
// concrete *hub.EventSubscription satisfies the consumer-facing event.Subscription
// contract the TUI depends on.
func TestSubscribeSeamDefaultFilterDeliversSessionEvent(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// The single-loop default: live tokens from the primary loop only, finalized
	// events from every loop. Session-scoped events ignore both scopes.
	filter := event.EventFilter{
		Ephemeral: event.LoopScope{Loops: map[uuid.UUID]struct{}{s.PrimaryLoopID(): {}}},
		Enduring:  event.LoopScope{All: true},
	}

	// The seam returns a concrete *hub.EventSubscription; it must satisfy the
	// consumer-facing event.Subscription the TUI's EventStream aliases.
	var sub event.Subscription
	sub, err = s.SubscribeEvents(filter)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	// A session-scoped event must arrive through the default filter.
	if err := s.PublishEvent(context.Background(), event.SessionIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.SessionID}}}); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatalf("subscription closed unexpectedly")
		}
		if _, isIdle := ev.(event.SessionIdle); !isIdle {
			t.Fatalf("got %T, want event.SessionIdle", ev)
		}
	case <-time.After(time.Second):
		t.Fatalf("subscriber did not receive the session-scoped event through the default filter")
	}
	if err := sub.Err(); err != nil {
		t.Errorf("sub.Err() = %v, want nil (live subscription)", err)
	}
}

// TestWaitIdleFreshSession proves a freshly built session is idle (no turn yet),
// so WaitIdle returns nil immediately.
func TestWaitIdleFreshSession(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle on a fresh session = %v, want nil", err)
	}
}

// TestShutdownStopsSessionAndWaitIdle proves Shutdown drives the session to its
// stopped phase: WaitIdle then returns hub.ErrSessionStopped, a subscriber sees
// SessionStopped, and a late loop event published after stop is delivered but does
// not flip the phase back to idle/active (WaitIdle keeps returning ErrSessionStopped).
func TestShutdownStopsSessionAndWaitIdle(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// WaitIdle returns ErrSessionStopped (stopped, not merely idle).
	if err := s.WaitIdle(context.Background()); !errors.Is(err, hub.ErrSessionStopped) {
		t.Fatalf("WaitIdle after Shutdown = %v, want hub.ErrSessionStopped", err)
	}

	// The subscriber received SessionStopped at some point.
	sawStopped := false
	drain := time.After(time.Second)
	for !sawStopped {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription closed before SessionStopped seen")
			}
			if _, ok := ev.(event.SessionStopped); ok {
				sawStopped = true
			}
		case <-drain:
			t.Fatalf("never saw SessionStopped after Shutdown")
		}
	}

	// A late loop event published after stop is delivered but does not flip phase.
	if err := s.PublishEvent(context.Background(), event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.SessionID}}}); err != nil {
		t.Fatalf("post-stop PublishEvent = %v", err)
	}
	if err := s.WaitIdle(context.Background()); !errors.Is(err, hub.ErrSessionStopped) {
		t.Fatalf("WaitIdle after post-stop publish = %v, want still hub.ErrSessionStopped", err)
	}
}

// TestExpectCancelExpectTurnSessionWiring proves the session-internal wake-token
// methods delegate to the hub and derive the right session-scoped edges. They are
// inert in production (no async subagents yet) but the wiring is exercised here.
func TestExpectCancelExpectTurnSessionWiring(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	subagent := mustUUID()
	s.expectTurn(context.Background(), subagent)

	// WaitIdle must now block (a wake token makes the session Active).
	blockCtx, blockCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer blockCancel()
	if err := s.WaitIdle(blockCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitIdle while a wake token is held = %v, want DeadlineExceeded (blocked)", err)
	}

	// SessionActive should have been delivered for the expectTurn edge.
	if !drainFor[event.SessionActive](t, sub) {
		t.Fatalf("expectTurn did not derive SessionActive")
	}

	s.cancelExpectTurn(context.Background(), subagent)
	if !drainFor[event.SessionIdle](t, sub) {
		t.Fatalf("cancelExpectTurn did not derive SessionIdle")
	}

	// Now idle again.
	if err := s.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after cancelExpectTurn = %v, want nil", err)
	}
}

// drainFor reads from the subscription until an event of type T arrives or a
// timeout elapses. It returns true if T was seen.
func drainFor[T event.Event](t *testing.T, sub event.Subscription) bool {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return false
			}
			if _, match := ev.(T); match {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// firstMatching reads from the subscription until an event of type T arrives or a
// timeout elapses, returning the matched event and true (zero value, false on
// miss). Unlike drainFor it hands back the concrete event so a test can inspect
// its Header/Cause.
func firstMatching[T event.Event](t *testing.T, sub event.Subscription) (T, bool) {
	t.Helper()
	var zero T
	deadline := time.After(time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return zero, false
			}
			if got, match := ev.(T); match {
				return got, true
			}
		case <-deadline:
			return zero, false
		}
	}
}

// TestLoopStartedPublishedOnNewLoop proves Session.NewLoop publishes exactly one
// Enduring LoopStarted to a subscriber active at creation time, carrying the NEW
// loop in Header.Coordinates and the spawning provenance in Header.Cause
// (Agency=AgencyMachine). It also proves there is no replay: a subscriber that
// attaches AFTER NewLoop never sees that LoopStarted.
func TestLoopStartedPublishedOnNewLoop(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Subscribe to all Enduring events BEFORE creating the second loop, so this
	// subscriber is active at creation time and must receive the LoopStarted.
	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	parent := loop.Provenance{LoopID: mustUUID(), TurnID: mustUUID(), StepID: mustUUID()}
	loopID, err := s.NewLoop(parent, cfg(&stubLLM{chunks: []content.Chunk{textChunk("y")}}))
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	ev, ok := firstMatching[event.LoopStarted](t, sub)
	if !ok {
		t.Fatal("subscriber active at NewLoop did not receive a LoopStarted")
	}
	// Header.Coordinates is the NEW loop: SessionID+LoopID set, Turn/Step zero.
	if ev.SessionID != s.SessionID {
		t.Errorf("LoopStarted SessionID = %v, want %v", ev.SessionID, s.SessionID)
	}
	if ev.LoopID != loopID {
		t.Errorf("LoopStarted LoopID = %v, want returned loop id %v", ev.LoopID, loopID)
	}
	if !ev.TurnID.IsZero() || !ev.StepID.IsZero() {
		t.Errorf("LoopStarted Coordinates Turn/Step = %v/%v, want both zero", ev.TurnID, ev.StepID)
	}
	if ev.EventID.IsZero() {
		t.Error("LoopStarted EventID is zero, want a freshly minted id")
	}
	// Header.Cause is the spawning loop/turn/step, machine-originated.
	if ev.Cause.LoopID != parent.LoopID {
		t.Errorf("LoopStarted Cause.LoopID = %v, want parent %v", ev.Cause.LoopID, parent.LoopID)
	}
	if ev.Cause.TurnID != parent.TurnID {
		t.Errorf("LoopStarted Cause.TurnID = %v, want parent %v", ev.Cause.TurnID, parent.TurnID)
	}
	if ev.Cause.StepID != parent.StepID {
		t.Errorf("LoopStarted Cause.StepID = %v, want parent %v", ev.Cause.StepID, parent.StepID)
	}
	if ev.Cause.Agency != identity.AgencyMachine {
		t.Errorf("LoopStarted Cause.Agency = %v, want AgencyMachine", ev.Cause.Agency)
	}
	// It is Enduring (durable loop-tree record).
	if ev.Class() != event.Enduring {
		t.Errorf("LoopStarted Class = %v, want Enduring", ev.Class())
	}

	// No replay: a subscriber attaching AFTER NewLoop must not see that LoopStarted.
	late, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("late SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = late.Close() })
	if _, ok := firstMatching[event.LoopStarted](t, late); ok {
		t.Fatal("late subscriber received a replayed LoopStarted, want none (no replay)")
	}
}
