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
// Generic Hub publication cannot discover or claim it from event coordinates.
func (r *TurnStartReservation) PublishTurnStarted(ctx context.Context, started event.TurnStarted) error {
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
	return r.hub.publishEventWithActivity(ctx, started, false, true)
}
