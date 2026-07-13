package hub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

func TestTurnStartReservationLifecycle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		cancel bool
	}{
		{name: "matching TurnStarted consumes reservation"},
		{name: "unused reservation releases idempotently", cancel: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessionID := mustID(t)
			loopID := mustID(t)
			h := New(sessionID)
			reservation, err := h.ReserveTurnStart(loopID)
			if err != nil {
				t.Fatal(err)
			}
			if tt.cancel {
				reservation.Release()
				reservation.Release()
			} else {
				err := h.PublishEventChecked(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{
					SessionID: sessionID,
					LoopID:    loopID,
				}}})
				if err != nil {
					t.Fatal(err)
				}
				reservation.Release()
			}

			next := make(chan error, 1)
			nextLoopID := mustID(t)
			go func() {
				next <- h.PublishEventChecked(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{
					SessionID: sessionID,
					LoopID:    nextLoopID,
				}}})
			}()
			select {
			case err := <-next:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("activity transition remained locked after reservation completion")
			}
		})
	}
}

func TestTurnStartReservationRejectsInvalidUse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		invalidID  bool
		stop       bool
		mismatch   bool
		wantReason TurnStartReservationReason
	}{
		{name: "zero loop id", invalidID: true, wantReason: TurnStartReservationInvalidLoop},
		{name: "stopped session", stop: true, wantReason: TurnStartReservationStopped},
		{name: "mismatched TurnStarted", mismatch: true, wantReason: TurnStartReservationMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessionID := mustID(t)
			loopID := mustID(t)
			h := New(sessionID)
			if tt.stop {
				h.StopSession(context.Background())
			}
			requestedID := loopID
			if tt.invalidID {
				requestedID = identity.Coordinates{}.LoopID
			}
			reservation, err := h.ReserveTurnStart(requestedID)
			if tt.mismatch {
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(reservation.Release)
				err = h.PublishEventChecked(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{
					SessionID: sessionID,
					LoopID:    mustID(t),
				}}})
			}
			var reservationErr *TurnStartReservationError
			if !errors.As(err, &reservationErr) || reservationErr.Reason != tt.wantReason {
				t.Fatalf("error = %T %v, want TurnStartReservationError reason %q", err, err, tt.wantReason)
			}
		})
	}
}
