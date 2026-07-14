package hub

import (
	"context"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

type turnStartReservationState uint8

const (
	turnStartReservationPending turnStartReservationState = iota
	turnStartReservationPublishing
	turnStartReservationPublished
	turnStartReservationReleased
)

type turnStartPublicationMode uint8

const (
	turnStartPublicationUnchecked turnStartPublicationMode = iota
	turnStartPublicationChecked
)

// TurnStartReservation is an opaque one-shot publisher that owns the Hub activity
// transition from immediately before a loop acquires its first checkpoint reader
// through publication of that loop's exact opening TurnStarted.
type TurnStartReservation struct {
	hub    *Hub
	loopID uuid.UUID

	mu    sync.Mutex
	state turnStartReservationState
}

// ReserveTurnStart establishes the global activity-before-checkpoint lock order for
// one loop's opening TurnStarted. The returned capability must be released on every
// path that does not publish that exact event.
func (h *Hub) ReserveTurnStart(loopID uuid.UUID) (*TurnStartReservation, error) {
	if loopID.IsZero() {
		return nil, &TurnStartReservationError{Reason: TurnStartReservationInvalidLoop}
	}
	h.activityMu.Lock()
	h.mu.RLock()
	stopped := h.state.phase == SessionStopped
	h.mu.RUnlock()
	if stopped {
		h.activityMu.Unlock()
		return nil, &TurnStartReservationError{Reason: TurnStartReservationStopped, LoopID: loopID}
	}
	return &TurnStartReservation{hub: h, loopID: loopID}, nil
}

// Release cancels an unused capability. A publication already in progress owns the
// activity release; a published or previously released capability is a no-op.
func (r *TurnStartReservation) Release() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.state != turnStartReservationPending {
		r.mu.Unlock()
		return
	}
	r.state = turnStartReservationReleased
	r.mu.Unlock()
	r.hub.activityMu.Unlock()
}

// PublishTurnStarted consumes this capability for exactly one matching value event.
// Generic Hub publication cannot discover or claim it from event coordinates. This
// legacy form preserves unchecked Hub reporting semantics; construction paths that
// must not install live state without the event use PublishTurnStartedChecked.
func (r *TurnStartReservation) PublishTurnStarted(ctx context.Context, started event.TurnStarted) error {
	return r.publishTurnStarted(ctx, started, turnStartPublicationUnchecked)
}

// PublishTurnStartedChecked consumes the reservation with checked durable
// publication, returning any append fault to the actor before it installs the turn.
func (r *TurnStartReservation) PublishTurnStartedChecked(ctx context.Context, started event.TurnStarted) error {
	return r.publishTurnStarted(ctx, started, turnStartPublicationChecked)
}

func (r *TurnStartReservation) publishTurnStarted(ctx context.Context, started event.TurnStarted, mode turnStartPublicationMode) error {
	if started.SessionID != r.hub.sessionID || started.LoopID != r.loopID {
		return &TurnStartReservationError{Reason: TurnStartReservationMismatch, LoopID: started.LoopID}
	}
	r.mu.Lock()
	switch r.state {
	case turnStartReservationReleased:
		r.mu.Unlock()
		return &TurnStartReservationError{Reason: TurnStartReservationReleased, LoopID: r.loopID}
	case turnStartReservationPublishing, turnStartReservationPublished:
		r.mu.Unlock()
		return &TurnStartReservationError{Reason: TurnStartReservationReused, LoopID: r.loopID}
	}
	r.state = turnStartReservationPublishing
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.state = turnStartReservationPublished
		r.mu.Unlock()
		r.hub.activityMu.Unlock()
	}()
	return r.hub.publishEventWithActivity(ctx, started, mode == turnStartPublicationChecked, true)
}
