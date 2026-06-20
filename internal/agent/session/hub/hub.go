// Package hub implements the session-level event fan-in: a publish/subscribe hub
// with a federated-quiescence model. Loops publish events through the narrow
// eventPublisher contract; consumers (TUI/CLI now, a durable journal later)
// subscribe with an EventFilter. The hub aggregates loop activity into a single
// sessionState so a headless run can WaitIdle without any session goroutine.
//
// Concurrency contract: ONE sync.RWMutex guards the subscriber set, the
// sessionState (active/phase), and the WaitIdle waiter registry. Active/phase
// mutations take the write lock; non-mutating publishes take the read lock. In
// every case the critical section only applies the state change (if any) and
// copies the subscriber slice — delivery happens OUTSIDE the lock, so a slow
// consumer can never stall SubscribeEvents, another publisher, or teardown.
package hub

import (
	"context"
	"sync"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// Hub is the session's event fan-in. It is owned by Session; loops see only
// its PublishEvent method (the narrow eventPublisher), and consumers see only
// SubscribeEvents. ExpectTurn/CancelExpectTurn/StopSession are session-owned (the
// session calls them; loops depend on the eventPublisher interface, which excludes
// them).
type Hub struct {
	sessionID uuid.UUID

	// mu guards subs, state, and waiters together. One lock keeps the
	// subscriber-set snapshot consistent with the active/phase transition.
	mu      sync.RWMutex
	subs    map[*EventSubscription]struct{}
	state   sessionState
	waiters map[chan error]struct{}
}

// New builds an idle hub for sessionID. The returned hub has no subscribers and a
// zero-value (idle, empty) sessionState.
func New(sessionID uuid.UUID) *Hub {
	return &Hub{
		sessionID: sessionID,
		subs:      make(map[*EventSubscription]struct{}),
		state:     newSessionState(),
		waiters:   make(map[chan error]struct{}),
	}
}

// SubscribeEvents registers a new subscription with the given filter and returns
// its handle. The subscriber reads ev from sub.Events(); it must Close the
// subscription when done.
func (h *Hub) SubscribeEvents(filter event.EventFilter) (*EventSubscription, error) {
	sub := newSubscription(filter, h.unsubscribe)
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub, nil
}

// unsubscribe removes a subscription from the set under the write lock. It is the
// subscription's onClose callback, fired on the first terminal (Close or fail), so
// a torn-down subscription does not linger in the fan-out set. Idempotent: a
// second delete of an absent key is a no-op.
func (h *Hub) unsubscribe(sub *EventSubscription) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

// PublishEvent fans ev out to every matching subscriber and applies any
// quiescence transition the event implies. It never blocks a publisher: delivery
// is a non-blocking send into each subscription's bounded egress channel, and all
// delivery happens outside the hub lock. It returns nil even with no subscribers
// (the headless case) — the sessionState transition still runs.
//
// After SessionStopped, the event is still delivered (filtered) but no longer
// mutates active/phase and never derives SessionIdle/SessionActive.
func (h *Hub) PublishEvent(_ context.Context, ev event.Event) error {
	subs, post := h.applyAndSnapshot(ev)
	h.deliver(subs, ev)
	if post != nil {
		h.deliver(subs, post)
	}
	return nil
}

// applyAndSnapshot is the locked critical section of a publish: it applies the
// event's active/phase mutation (if any) and copies the subscriber slice. It
// returns the snapshot and the at-most-one derived post event to deliver after
// ev. Delivery is the caller's job, outside the lock.
func (h *Hub) applyAndSnapshot(ev event.Event) ([]*EventSubscription, event.Event) {
	mutate, mutates := activeMutation(ev)
	if !mutates {
		// Non-mutating publish: read lock, no applyActivity.
		h.mu.RLock()
		defer h.mu.RUnlock()
		return h.snapshotSubsLocked(), nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// applyActivity is a no-op (returns nil, never mutates) once SessionStopped, so
	// a post-stop mutating event delivers without flipping phase. signalIdle wakes
	// any blocked WaitIdle on the Active->Idle edge.
	post := h.state.applyActivity(h.sessionID, func() { mutate(&h.state) }, h.signalIdleLocked)
	return h.snapshotSubsLocked(), post
}

// snapshotSubsLocked copies the subscriber set into a slice. The caller must hold
// mu (read or write). Delivery iterates the copy outside the lock.
func (h *Hub) snapshotSubsLocked() []*EventSubscription {
	if len(h.subs) == 0 {
		return nil
	}
	out := make([]*EventSubscription, 0, len(h.subs))
	for sub := range h.subs {
		out = append(out, sub)
	}
	return out
}

// activeMutation returns the sessionState mutation an event implies and whether it
// mutates at all. The mutating events:
//   - TurnStarted: add {loop, LoopID}; if Cause.LoopID != 0, also remove its
//     {wake} (the hand-back release; the loop key is added in the same step).
//   - LoopIdle: remove {loop, LoopID}.
//   - TurnFoldedInto / InputCancelled with Cause.LoopID != 0: remove {wake}.
//
// Every other event is non-mutating (token firehose, StepDone, tool/gate, the
// session events themselves, and terminals).
func activeMutation(ev event.Event) (func(*sessionState), bool) {
	switch e := ev.(type) {
	case event.TurnStarted:
		loopID := e.LoopID
		wake := e.Cause.LoopID
		return func(s *sessionState) {
			if !wake.IsZero() {
				s.remove(activityKey{kind: kindWake, id: wake})
			}
			s.add(activityKey{kind: kindLoop, id: loopID})
		}, true
	case event.LoopIdle:
		loopID := e.LoopID
		return func(s *sessionState) {
			s.remove(activityKey{kind: kindLoop, id: loopID})
		}, true
	case event.TurnFoldedInto:
		if e.Cause.LoopID.IsZero() {
			return nil, false
		}
		wake := e.Cause.LoopID
		return func(s *sessionState) { s.remove(activityKey{kind: kindWake, id: wake}) }, true
	case event.InputCancelled:
		if e.Cause.LoopID.IsZero() {
			return nil, false
		}
		wake := e.Cause.LoopID
		return func(s *sessionState) { s.remove(activityKey{kind: kindWake, id: wake}) }, true
	default:
		return nil, false
	}
}

// deliver fans one event out to a snapshot of subscribers, OUTSIDE the lock. Per
// subscriber it applies the declared-interest filter (ShouldDeliver), then a
// non-blocking send into the bounded egress channel. On overflow the class-aware
// policy applies: an Ephemeral event is dropped for that subscriber; an Enduring
// event fails that subscription with a typed loss error (never silently dropped),
// and delivery continues to other subscribers. It never blocks.
func (h *Hub) deliver(subs []*EventSubscription, ev event.Event) {
	for _, sub := range subs {
		if !event.ShouldDeliver(sub.filter, ev) {
			continue
		}
		switch sub.trySend(ev) {
		case sendDelivered, sendClosed:
			// Delivered, or the subscription is already torn down (a Close/fail
			// racing this snapshot) — skip it either way.
			continue
		case sendFull:
			if ev.Class() == event.Ephemeral {
				continue // droppable: reconstructable from a later authoritative event
			}
			// Enduring overflow: fail this subscription, keep delivering to others.
			sub.fail(&SubscriptionLossError{DroppedClass: ev.Class()})
		}
	}
}

// ExpectTurn takes a {wake, subagentLoopID} token at subagent spawn so a finished
// subagent's in-flight hand-back cannot empty active and fire a false SessionIdle.
// It derives SessionActive if the session was idle. It is exported for the session
// (its sole caller); loops depend only on the narrow eventPublisher interface,
// which excludes it, so a loop can never reach it.
func (h *Hub) ExpectTurn(_ context.Context, subagentLoopID uuid.UUID) {
	h.mu.Lock()
	post := h.state.applyActivity(h.sessionID,
		func() { h.state.add(activityKey{kind: kindWake, id: subagentLoopID}) },
		h.signalIdleLocked)
	subs := h.snapshotSubsLocked()
	h.mu.Unlock()
	if post != nil {
		h.deliver(subs, post)
	}
}

// CancelExpectTurn releases a {wake, subagentLoopID} token when its hand-back is
// rejected or explicitly discarded. It derives SessionIdle if this emptied active.
// Exported for the session only (see ExpectTurn).
func (h *Hub) CancelExpectTurn(_ context.Context, subagentLoopID uuid.UUID) {
	h.mu.Lock()
	post := h.state.applyActivity(h.sessionID,
		func() { h.state.remove(activityKey{kind: kindWake, id: subagentLoopID}) },
		h.signalIdleLocked)
	subs := h.snapshotSubsLocked()
	h.mu.Unlock()
	if post != nil {
		h.deliver(subs, post)
	}
}

// StopSession is the session-owned teardown transition. It is idempotent: if
// already SessionStopped it returns without effect. Otherwise it clears active,
// forces phase=SessionStopped (bypassing applyActivity so no SessionIdle is
// derived), wakes every WaitIdle waiter with ErrSessionStopped, and delivers a
// SessionStopped event. Exported for the session only (see ExpectTurn).
func (h *Hub) StopSession(_ context.Context) {
	h.mu.Lock()
	if h.state.phase == SessionStopped {
		h.mu.Unlock()
		return
	}
	h.state.active = make(map[activityKey]struct{})
	h.state.phase = SessionStopped
	h.wakeWaitersLocked(ErrSessionStopped)
	subs := h.snapshotSubsLocked()
	h.mu.Unlock()

	h.deliver(subs, event.SessionStopped{Header: event.Header{Coordinates: identity.Coordinates{SessionID: h.sessionID}}})
}

// WaitIdle blocks until the session is quiescent (active empty), ctx is done, or
// the session stops. It returns nil on idle, ctx.Err() on cancellation, and
// ErrSessionStopped if the session is or becomes stopped. With no session
// goroutine, waiters are woken by applyActivity (Active->Idle) and StopSession.
func (h *Hub) WaitIdle(ctx context.Context) error {
	h.mu.Lock()
	switch {
	case h.state.phase == SessionStopped:
		h.mu.Unlock()
		return ErrSessionStopped
	case len(h.state.active) == 0:
		h.mu.Unlock()
		return nil
	}
	// Buffered(1) so the waker never blocks holding the lock and a late wake after
	// a ctx-loss is harmless (it lands in the buffer, no goroutine reads it).
	ch := make(chan error, 1)
	h.waiters[ch] = struct{}{}
	h.mu.Unlock()

	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.waiters, ch)
		h.mu.Unlock()
		return ctx.Err()
	}
}

// signalIdleLocked wakes every WaitIdle waiter with a nil error (the Active->Idle
// edge). The caller holds mu. Each waiter channel is buffered(1) and cleared, so
// the send never blocks.
func (h *Hub) signalIdleLocked() { h.wakeWaitersLocked(nil) }

// wakeWaitersLocked sends err to every registered waiter and clears the registry.
// The caller holds mu; each waiter channel is buffered(1) so no send blocks.
func (h *Hub) wakeWaitersLocked(err error) {
	for ch := range h.waiters {
		ch <- err
		delete(h.waiters, ch)
	}
}
