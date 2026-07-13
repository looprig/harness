package loopruntime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

type cancelBarrierLLM struct {
	started    chan struct{}
	cancelled  chan struct{}
	release    chan struct{}
	startOnce  sync.Once
	cancelOnce sync.Once
}

func (*cancelBarrierLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, nil
}
func (l *cancelBarrierLLM) Stream(ctx context.Context, _ inference.Request) (*inference.StreamReader[content.Chunk], error) {
	return inference.NewStreamReader(func() (content.Chunk, error) {
		l.startOnce.Do(func() { close(l.started) })
		<-ctx.Done()
		l.cancelOnce.Do(func() { close(l.cancelled) })
		<-l.release
		return nil, ctx.Err()
	}, nil), nil
}

func cancelDelegateRequest(t *testing.T, l *Loop, target uuid.UUID) command.DelegateCancelResult {
	t.Helper()
	ack := make(chan command.DelegateCancelResult, 1)
	l.Commands <- command.CancelDelegateRequest{
		Header:          command.Header{CommandID: mustID(t)},
		Coordinates:     identity.Coordinates{SessionID: mustID(t), LoopID: mustID(t)},
		TargetCommandID: target,
		Ack:             ack,
	}
	select {
	case result := <-ack:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("targeted cancel was not acknowledged")
		return command.DelegateCancelNoop
	}
}

func managedNativeInput(t *testing.T, l *Loop, id uuid.UUID, text string) {
	t.Helper()
	accepted := make(chan error, 1)
	l.Commands <- command.UserInput{
		Header:       command.Header{CommandID: id},
		Blocks:       []content.Block{&content.TextBlock{Text: text}},
		NoFold:       true,
		TargetLoopID: mustID(t),
		Accepted:     accepted,
	}
	if err := <-accepted; err != nil {
		t.Fatalf("managed input: %v", err)
	}
}

func TestCancelDelegateRequestRemovesOnlyQueuedNativeRequest(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	activeID := mustID(t)
	managedNativeInput(t, l, activeID, "A")
	queuedID := mustID(t)
	managedNativeInput(t, l, queuedID, "B")
	if got := cancelDelegateRequest(t, l, queuedID); got != command.DelegateCancelQueued {
		t.Fatalf("cancel result = %v, want queued", got)
	}
	cancelled := waitNativeInputCancelled(t, rec, queuedID)
	if cancelled.Reason != event.CancelClientRetracted {
		t.Fatalf("queued cancellation reason = %v, want client retracted", cancelled.Reason)
	}
	ack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Header: command.Header{CommandID: mustID(t)}, Ack: ack}
	if !<-ack {
		t.Fatal("active A was not left running after cancelling queued B")
	}
}

func TestCancelDelegateRequestInterruptsActiveNativeRequestButPreservesNext(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	activeID := mustID(t)
	managedNativeInput(t, l, activeID, "B")
	nextID := mustID(t)
	managedNativeInput(t, l, nextID, "C")
	if got := cancelDelegateRequest(t, l, activeID); got != command.DelegateCancelActive {
		t.Fatalf("cancel result = %v, want active", got)
	}
	waitNativeTurnStarted(t, rec, nextID)
	for _, ev := range rec.events() {
		if cancelled, ok := ev.(event.InputCancelled); ok && cancelled.Cause.CommandID == nextID {
			t.Fatalf("request-specific cancellation of B also cancelled C: %+v", cancelled)
		}
	}
	ack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Header: command.Header{CommandID: mustID(t)}, Ack: ack}
	<-ack
}

func TestOrdinaryInterruptOverridesPendingTargetedCancelAndFlushesQueue(t *testing.T) {
	t.Parallel()
	client := &cancelBarrierLLM{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	l, rec, _ := newLoop(t, client)
	activeID := mustID(t)
	managedNativeInput(t, l, activeID, "B")
	select {
	case <-client.started:
	case <-time.After(2 * time.Second):
		t.Fatal("B did not enter provider barrier")
	}
	nextID := mustID(t)
	managedNativeInput(t, l, nextID, "C")
	if got := cancelDelegateRequest(t, l, activeID); got != command.DelegateCancelActive {
		t.Fatalf("targeted cancel result = %v, want active", got)
	}
	select {
	case <-client.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("B context cancellation did not reach provider barrier")
	}
	ack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Header: command.Header{CommandID: mustID(t)}, Ack: ack}
	if got := <-ack; !got {
		t.Fatal("ordinary interrupt ack = false while targeted cancellation terminal was pending")
	}
	close(client.release)
	cancelled := waitNativeInputCancelled(t, rec, nextID)
	if cancelled.Reason != event.CancelTurnInterrupted {
		t.Fatalf("C cancellation reason = %v, want interrupted", cancelled.Reason)
	}
	for _, ev := range rec.events() {
		if started, ok := ev.(event.TurnStarted); ok && started.Cause.CommandID == nextID {
			t.Fatal("ordinary interrupt raced after targeted cancellation but C still started")
		}
	}
}

func waitNativeInputCancelled(t *testing.T, rec *recordingPublisher, id uuid.UUID) event.InputCancelled {
	t.Helper()
	blockUntilEvents(t, rec, func(events []event.Event) bool {
		for _, ev := range events {
			if cancelled, ok := ev.(event.InputCancelled); ok && cancelled.Cause.CommandID == id {
				return true
			}
		}
		return false
	})
	for _, ev := range rec.events() {
		if cancelled, ok := ev.(event.InputCancelled); ok && cancelled.Cause.CommandID == id {
			return cancelled
		}
	}
	t.Fatal("InputCancelled disappeared")
	return event.InputCancelled{}
}

func waitNativeTurnStarted(t *testing.T, rec *recordingPublisher, id uuid.UUID) {
	t.Helper()
	blockUntilEvents(t, rec, func(events []event.Event) bool {
		for _, ev := range events {
			if started, ok := ev.(event.TurnStarted); ok && started.Cause.CommandID == id {
				return true
			}
		}
		return false
	})
}
