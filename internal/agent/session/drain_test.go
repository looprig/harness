package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// drainUUID builds a deterministic non-zero UUID from a seed for correlation
// fixtures. Mirrors identity.fixedUUID but local to this package's tests.
func drainUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

// fakeSubscription is a test double for event.Subscription: a buffered channel
// the test scripts events onto, a settable termination error, and a no-op Close.
// It is the seam that lets drainToFinalText be driven with a deterministic event
// sequence without a live hub/loop.
type fakeSubscription struct {
	events chan event.Event
	err    error
}

func newFakeSubscription(buf int) *fakeSubscription {
	return &fakeSubscription{events: make(chan event.Event, buf)}
}

func (f *fakeSubscription) Events() <-chan event.Event { return f.events }
func (f *fakeSubscription) Close() error               { return nil }
func (f *fakeSubscription) Err() error                 { return f.err }

// turnStarted builds a TurnStarted whose Cause.CommandID is cmd and whose
// Coordinates.TurnID is turn — the opening resolution event drainToFinalText
// correlates on in phase 1. LoopID is left zero (matched by the also-zero LoopID
// on the zero-loop phase-2 helpers); use turnStartedOnLoop to pin a LoopID.
func turnStarted(cmd, turn uuid.UUID) event.TurnStarted {
	return turnStartedOnLoop(cmd, turn, uuid.UUID{})
}

// turnStartedOnLoop is turnStarted with an explicit Coordinates.LoopID, so phase 2
// can be driven with matching/mismatching loop ids for the fail-secure cross-check.
func turnStartedOnLoop(cmd, turn, loop uuid.UUID) event.TurnStarted {
	return event.TurnStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{LoopID: loop, TurnID: turn},
			Cause:       identity.Cause{CommandID: cmd},
		},
	}
}

// stepDone builds a StepDone for turn carrying a single AIMessage with text.
func stepDone(turn uuid.UUID, text string) event.StepDone {
	return event.StepDone{
		Header:   event.Header{Coordinates: identity.Coordinates{TurnID: turn}},
		Messages: content.AgenticMessages{aiMessage(text)},
	}
}

// aiMessage builds an *content.AIMessage with a single TextBlock.
func aiMessage(text string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

func turnDone(turn uuid.UUID, msg *content.AIMessage) event.TurnDone {
	return event.TurnDone{
		Header:  event.Header{Coordinates: identity.Coordinates{TurnID: turn}},
		Message: msg,
	}
}

// turnDoneOnLoop is turnDone with an explicit Coordinates.LoopID, for exercising
// the phase-2 LoopID cross-check (right TurnID on a wrong loop must be ignored).
func turnDoneOnLoop(turn, loop uuid.UUID, msg *content.AIMessage) event.TurnDone {
	return event.TurnDone{
		Header:  event.Header{Coordinates: identity.Coordinates{LoopID: loop, TurnID: turn}},
		Message: msg,
	}
}

func turnFailed(turn uuid.UUID, err error) event.TurnFailed {
	return event.TurnFailed{
		Header: event.Header{Coordinates: identity.Coordinates{TurnID: turn}},
		Err:    err,
	}
}

func turnInterrupted(turn uuid.UUID) event.TurnInterrupted {
	return event.TurnInterrupted{Header: event.Header{Coordinates: identity.Coordinates{TurnID: turn}}}
}

func turnRejected(cmd uuid.UUID, reason event.RejectReason) event.TurnRejected {
	return event.TurnRejected{
		Header: event.Header{Cause: identity.Cause{CommandID: cmd}},
		Reason: reason,
	}
}

// errProvider is a sentinel leaf error used to assert TurnFailed.Err and
// subscription-loss wrapping reach the caller via errors.Is.
var errProvider = errors.New("provider exploded")

// TestDrainToFinalText drives the shared collect helper with scripted event
// sequences over a fake subscription, covering every §5 exit and the noise-
// rejection (phase-1 correlation) and ctx-interrupt fail-safe paths.
func TestDrainToFinalText(t *testing.T) {
	t.Parallel()

	cmd := drainUUID(0x01)
	turn := drainUUID(0x02)
	otherCmd := drainUUID(0x03)
	otherTurn := drainUUID(0x04)
	loop := drainUUID(0x05)
	otherLoop := drainUUID(0x06)

	tests := []struct {
		name string
		// script is fed onto the fake's buffered channel before draining (the
		// subscribe-before-submit ordering means every event is already buffered).
		script []event.Event
		// closeAfter closes the channel after the script (subscription-loss path).
		closeAfter bool
		// subErr is set on the fake before draining (the hub-forced loss cause).
		subErr error

		wantText         string
		wantErr          bool
		wantFailed       bool // *drainFailedError wrapping errProvider
		wantTurnRejected bool // *TurnRejectedError (the package's existing typed reject)
		wantLost         bool // *drainLostError
		wantInterrupts   int32
	}{
		{
			name:     "clean TurnDone returns its message text",
			script:   []event.Event{turnStarted(cmd, turn), stepDone(turn, "partial"), turnDone(turn, aiMessage("final"))},
			wantText: "final",
		},
		{
			name:     "fallback to last StepDone when TurnDone.Message is nil",
			script:   []event.Event{turnStarted(cmd, turn), stepDone(turn, "step text"), turnDone(turn, nil)},
			wantText: "step text",
		},
		{
			name:       "TurnFailed yields a typed failed error wrapping the cause",
			script:     []event.Event{turnStarted(cmd, turn), turnFailed(turn, errProvider)},
			wantErr:    true,
			wantFailed: true,
		},
		{
			name:             "TurnRejected before any turn yields a typed TurnRejectedError",
			script:           []event.Event{turnRejected(cmd, event.RejectShuttingDown)},
			wantErr:          true,
			wantTurnRejected: true,
		},
		{
			name:       "subscription loss before terminal wraps Err()",
			script:     []event.Event{turnStarted(cmd, turn), stepDone(turn, "partial")},
			closeAfter: true,
			subErr:     errProvider,
			wantErr:    true,
			wantLost:   true,
		},
		{
			name:       "subscription loss with no Err yields a no-terminal lost error",
			script:     []event.Event{turnStarted(cmd, turn)},
			closeAfter: true,
			wantErr:    true,
			wantLost:   true,
		},
		{
			name: "noise for other command/turn is ignored, real terminal resolves",
			script: []event.Event{
				turnStarted(otherCmd, otherTurn),
				stepDone(otherTurn, "OTHER step"),
				turnStarted(cmd, turn),
				stepDone(turn, "partial"),
				turnDone(otherTurn, aiMessage("OTHER final")),
				turnDone(turn, aiMessage("final")),
			},
			wantText: "final",
		},
		{
			// Fail-secure LoopID cross-check: a phase-2 TurnDone carrying the right
			// TurnID but a WRONG LoopID must be ignored; only the matching-loop
			// terminal resolves.
			name: "phase-2 event with right TurnID but wrong LoopID is ignored",
			script: []event.Event{
				turnStartedOnLoop(cmd, turn, loop),
				turnDoneOnLoop(turn, otherLoop, aiMessage("WRONG loop final")),
				turnDoneOnLoop(turn, loop, aiMessage("final")),
			},
			wantText: "final",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sub := newFakeSubscription(len(tt.script) + 1)
			sub.err = tt.subErr
			for _, ev := range tt.script {
				sub.events <- ev
			}
			if tt.closeAfter {
				close(sub.events)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var interrupts atomic.Int32
			interrupt := func() { interrupts.Add(1) }

			got, err := drainToFinalText(ctx, sub, cmd, interrupt)

			if (err != nil) != tt.wantErr {
				t.Fatalf("drainToFinalText() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if got != tt.wantText {
					t.Errorf("drainToFinalText() text = %q, want %q", got, tt.wantText)
				}
			}
			if tt.wantFailed {
				var fe *drainFailedError
				if !errors.As(err, &fe) {
					t.Fatalf("err = %v, want *drainFailedError", err)
				}
				if !errors.Is(err, errProvider) {
					t.Errorf("err = %v, want it to wrap errProvider", err)
				}
			}
			if tt.wantTurnRejected {
				var re *TurnRejectedError
				if !errors.As(err, &re) {
					t.Fatalf("err = %v, want *TurnRejectedError", err)
				}
				if re.Reason != event.RejectShuttingDown {
					t.Errorf("rejected reason = %v, want RejectShuttingDown", re.Reason)
				}
			}
			if tt.wantLost {
				var le *drainLostError
				if !errors.As(err, &le) {
					t.Fatalf("err = %v, want *drainLostError", err)
				}
				if tt.subErr != nil && !errors.Is(err, tt.subErr) {
					t.Errorf("err = %v, want it to wrap subErr %v", err, tt.subErr)
				}
			}
			if got := interrupts.Load(); got != tt.wantInterrupts {
				t.Errorf("interrupt called %d times, want %d", got, tt.wantInterrupts)
			}
		})
	}
}

// TestDrainToFinalTextInterruptOnCtxCancel asserts the ctx-cancel fail-safe in
// isolation (it needs interleaved goroutine timing the table cannot express):
// after the opening TurnStarted, cancelling ctx calls interrupt() exactly once,
// the helper keeps draining, and the sub-loop's TurnInterrupted terminal yields a
// typed interrupted error. The helper must not return on ctx.Done() alone, and
// must not busy-loop calling interrupt repeatedly.
func TestDrainToFinalTextInterruptOnCtxCancel(t *testing.T) {
	t.Parallel()

	cmd := drainUUID(0x01)
	turn := drainUUID(0x02)

	sub := newFakeSubscription(4)
	sub.events <- turnStarted(cmd, turn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var interrupts atomic.Int32
	interrupt := func() { interrupts.Add(1) }

	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err := drainToFinalText(ctx, sub, cmd, interrupt)
		done <- result{text, err}
	}()

	// Give the drain time to consume TurnStarted and block in its select, then
	// cancel ctx to trip the fail-safe.
	deadline := time.After(2 * time.Second)
	for interrupts.Load() == 0 {
		cancel()
		select {
		case <-deadline:
			t.Fatal("interrupt() was never called after ctx cancel")
		case <-time.After(time.Millisecond):
		}
	}

	// Feed the terminal the sub-loop produces once interrupted.
	sub.events <- turnInterrupted(turn)

	var res result
	select {
	case res = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainToFinalText did not return after TurnInterrupted")
	}

	if got := interrupts.Load(); got != 1 {
		t.Fatalf("interrupt called %d times, want exactly 1", got)
	}
	var ie *drainInterruptedError
	if !errors.As(res.err, &ie) {
		t.Fatalf("err = %v, want *drainInterruptedError", res.err)
	}
	if res.text != "" {
		t.Errorf("text = %q, want empty (no partial on interrupt)", res.text)
	}
}
