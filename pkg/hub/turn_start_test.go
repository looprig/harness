package hub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

type testTurnStartPublisher interface {
	PublishTurnStarted(context.Context, event.TurnStarted) error
}

func TestGenericPublicationCannotClaimTurnStartReservation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "same coordinates do not confer reservation authority"},
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
			published := make(chan error, 1)
			go func() {
				published <- h.PublishEventChecked(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{
					SessionID: sessionID,
					LoopID:    loopID,
				}}})
			}()
			select {
			case err := <-published:
				t.Fatalf("generic same-loop publisher stole reservation: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			reservation.Release()
			select {
			case err := <-published:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("generic publisher did not continue after reservation release")
			}
		})
	}
}

func TestTurnStartCapabilityRejectsInvalidLifetimeUse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		prepare    func(*TurnStartReservation, testTurnStartPublisher, event.TurnStarted) error
		wantReason TurnStartReservationReason
	}{
		{
			name: "released capability is stale",
			prepare: func(reservation *TurnStartReservation, publisher testTurnStartPublisher, started event.TurnStarted) error {
				reservation.Release()
				return publisher.PublishTurnStarted(context.Background(), started)
			},
			wantReason: TurnStartReservationReleased,
		},
		{
			name: "published capability cannot be reused",
			prepare: func(_ *TurnStartReservation, publisher testTurnStartPublisher, started event.TurnStarted) error {
				if err := publisher.PublishTurnStarted(context.Background(), started); err != nil {
					return err
				}
				return publisher.PublishTurnStarted(context.Background(), started)
			},
			wantReason: TurnStartReservationReused,
		},
		{
			name: "loop-mismatched capability publication is denied",
			prepare: func(_ *TurnStartReservation, publisher testTurnStartPublisher, started event.TurnStarted) error {
				started.LoopID = identity.Coordinates{}.LoopID
				return publisher.PublishTurnStarted(context.Background(), started)
			},
			wantReason: TurnStartReservationMismatch,
		},
		{
			name: "session-mismatched capability publication is denied",
			prepare: func(_ *TurnStartReservation, publisher testTurnStartPublisher, started event.TurnStarted) error {
				started.SessionID = identity.Coordinates{}.SessionID
				return publisher.PublishTurnStarted(context.Background(), started)
			},
			wantReason: TurnStartReservationMismatch,
		},
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
			t.Cleanup(reservation.Release)
			var publisher testTurnStartPublisher = reservation
			started := event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{
				SessionID: sessionID,
				LoopID:    loopID,
			}}}
			err = tt.prepare(reservation, publisher, started)
			var reservationErr *TurnStartReservationError
			if !errors.As(err, &reservationErr) || reservationErr.Reason != tt.wantReason {
				t.Fatalf("error = %T %v, want TurnStartReservationError reason %q", err, err, tt.wantReason)
			}
			if tt.wantReason == TurnStartReservationMismatch {
				if err := publisher.PublishTurnStarted(context.Background(), started); err != nil {
					t.Fatalf("matching publication after denied mismatch: %v", err)
				}
			}
			nextLoopID := mustID(t)
			next := make(chan error, 1)
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
				t.Fatal("capability lifetime failure leaked activity transition ownership")
			}
		})
	}
}

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
				err := reservation.PublishTurnStarted(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{
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
		wantReason TurnStartReservationReason
	}{
		{name: "zero loop id", invalidID: true, wantReason: TurnStartReservationInvalidLoop},
		{name: "stopped session", stop: true, wantReason: TurnStartReservationStopped},
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
			if reservation != nil {
				t.Cleanup(reservation.Release)
			}
			var reservationErr *TurnStartReservationError
			if !errors.As(err, &reservationErr) || reservationErr.Reason != tt.wantReason {
				t.Fatalf("error = %T %v, want TurnStartReservationError reason %q", err, err, tt.wantReason)
			}
		})
	}
}
