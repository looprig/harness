package hub

import (
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

type turnStartReservationState uint8

const (
	turnStartReservationPending turnStartReservationState = iota
	turnStartReservationClaimed
	turnStartReservationReleased
)

// TurnStartReservation owns the Hub activity-transition lock from immediately
// before a loop acquires its first checkpoint reader until that loop publishes its
// matching TurnStarted. Release cancels an unused reservation and is idempotent.
type TurnStartReservation struct {
	hub    *Hub
	loopID uuid.UUID

	mu    sync.Mutex
	state turnStartReservationState
}

// ReserveTurnStart establishes the global activity-before-checkpoint lock order for
// one loop's opening TurnStarted. The returned reservation must be released on every
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

	reservation := &TurnStartReservation{hub: h, loopID: loopID}
	h.turnStartMu.Lock()
	h.turnStartReservation = reservation
	h.turnStartMu.Unlock()
	return reservation, nil
}

// Release cancels an unused reservation. Once the Hub has claimed it for the
// matching TurnStarted, the publication path owns the final release instead.
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
	r.releaseActivity()
}

func (r *TurnStartReservation) claim() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != turnStartReservationPending {
		return false
	}
	r.state = turnStartReservationClaimed
	return true
}

func (r *TurnStartReservation) finish() {
	r.mu.Lock()
	if r.state != turnStartReservationClaimed {
		r.mu.Unlock()
		return
	}
	r.state = turnStartReservationReleased
	r.mu.Unlock()
	r.releaseActivity()
}

func (r *TurnStartReservation) releaseActivity() {
	r.hub.turnStartMu.Lock()
	if r.hub.turnStartReservation == r {
		r.hub.turnStartReservation = nil
	}
	r.hub.turnStartMu.Unlock()
	r.hub.activityMu.Unlock()
}

func (h *Hub) claimTurnStartReservation(ev event.Event) (*TurnStartReservation, error) {
	started, isTurnStarted := ev.(event.TurnStarted)
	h.turnStartMu.Lock()
	reservation := h.turnStartReservation
	if reservation == nil || !isTurnStarted {
		h.turnStartMu.Unlock()
		return nil, nil
	}
	if started.SessionID != h.sessionID || started.LoopID != reservation.loopID {
		h.turnStartMu.Unlock()
		return nil, &TurnStartReservationError{
			Reason: TurnStartReservationMismatch,
			LoopID: started.LoopID,
		}
	}
	claimed := reservation.claim()
	h.turnStartMu.Unlock()
	if !claimed {
		return nil, nil
	}
	return reservation, nil
}
