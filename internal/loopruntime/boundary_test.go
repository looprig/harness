package loopruntime

import (
	"context"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
)

type blockingExecutionBoundary struct {
	recordingPublisher
	entered chan event.Event
	release chan struct{}
}

type blockingStepAdmission struct {
	recordingPublisher
	entered chan struct{}
	release chan struct{}
}

func (b *blockingStepAdmission) EnterExecution(ctx context.Context) (func(), error) {
	close(b.entered)
	select {
	case <-b.release:
		return func() {}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *blockingExecutionBoundary) CommitBoundary(ctx context.Context, ev event.Event) error {
	if err := b.PublishEventChecked(ctx, ev); err != nil {
		return err
	}
	b.entered <- ev
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestNativeCheckpointBoundaryBlocksStepAndTurnAcknowledgement(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sid, lid := mustID(t), mustID(t)
	b := &blockingExecutionBoundary{entered: make(chan event.Event, 2), release: make(chan struct{}, 2)}
	l, err := newWithConfig(ctx, sid, lid, Provenance{}, b, runtimeConfig{Client: &fakeLLM{chunks: []content.Chunk{textChunk("done")}}, Model: testModel(), DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	startTurn(t, l, &b.recordingPublisher, nil)

	var step event.Event
	select {
	case step = <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("step boundary not entered")
	}
	if _, ok := step.(event.StepDone); !ok {
		t.Fatalf("first boundary = %T, want StepDone", step)
	}
	for _, ev := range b.events() {
		if ev.EndsTurn() {
			t.Fatalf("terminal %T published before step boundary released", ev)
		}
	}
	b.release <- struct{}{}

	var terminal event.Event
	select {
	case terminal = <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("turn boundary not entered")
	}
	if _, ok := terminal.(event.TurnDone); !ok {
		t.Fatalf("second boundary = %T, want TurnDone", terminal)
	}
	b.release <- struct{}{}
}

func TestNativeCheckpointAdmissionBlocksNextInferenceStep(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &blockingStepAdmission{entered: make(chan struct{}), release: make(chan struct{})}
	l, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, b, runtimeConfig{Client: &fakeLLM{chunks: []content.Chunk{textChunk("done")}}, Model: testModel(), DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	startTurn(t, l, &b.recordingPublisher, nil)
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("loop did not consult session execution admission")
	}
	for _, ev := range b.events() {
		if _, ok := ev.(event.StepDone); ok {
			t.Fatal("StepDone published while inference admission blocked")
		}
	}
	close(b.release)
	if _, ok := drainToTerminal(t, &b.recordingPublisher).(event.TurnDone); !ok {
		t.Fatal("turn did not complete after admission release")
	}
}
