package loopruntime

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/inference"
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

type gatedFirstInference struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

type panickingBoundaryInference struct{}

func (panickingBoundaryInference) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	panic("inference invoke panic")
}

func (panickingBoundaryInference) Stream(context.Context, inference.Request) (*inference.StreamReader[content.Chunk], error) {
	panic("inference stream panic")
}

type countingAdmission struct {
	recordingPublisher
	releases atomic.Int32
}

func (a *countingAdmission) EnterExecution(context.Context) (func(), error) {
	return func() { a.releases.Add(1) }, nil
}

func (g *gatedFirstInference) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("Invoke not used")
}

func (g *gatedFirstInference) Stream(ctx context.Context, _ inference.Request) (*inference.StreamReader[content.Chunk], error) {
	if g.calls.Add(1) == 1 {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	i := 0
	return inference.NewStreamReader(func() (content.Chunk, error) {
		if i == 0 {
			i++
			return textChunk("done"), nil
		}
		return nil, io.EOF
	}, nil), nil
}

type failingTerminalBoundary struct {
	recordingPublisher
	err error
}

func (b *failingTerminalBoundary) CommitBoundary(ctx context.Context, ev event.Event) error {
	if err := b.PublishEventChecked(ctx, ev); err != nil {
		return err
	}
	if ev.EndsTurn() {
		return b.err
	}
	return nil
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
	inputID := mustID(t)
	select {
	case l.Commands <- command.UserInput{Header: command.Header{CommandID: inputID, CreatedAt: time.Now()}}:
	case <-time.After(time.Second):
		t.Fatal("submit blocked")
	}

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

func TestPanickingInferenceReleasesTransferredAdmissionExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admission := &countingAdmission{}
	l, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, admission, runtimeConfig{
		Client: panickingBoundaryInference{}, Model: testModel(), DrainTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case l.Commands <- command.UserInput{Header: command.Header{CommandID: mustID(t), CreatedAt: time.Now()}}:
	case <-time.After(time.Second):
		t.Fatal("submit blocked")
	}
	deadline := time.Now().Add(time.Second)
	for {
		failed := false
		for _, ev := range admission.events() {
			if _, ok := ev.(event.TurnFailed); ok {
				failed = true
				break
			}
		}
		if failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("panic did not emit TurnFailed")
		}
		time.Sleep(time.Millisecond)
	}
	if got := admission.releases.Load(); got != 1 {
		t.Fatalf("admission releases = %d, want exactly 1", got)
	}
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
	inputID := mustID(t)
	select {
	case l.Commands <- command.UserInput{Header: command.Header{CommandID: inputID, CreatedAt: time.Now()}}:
	case <-time.After(time.Second):
		t.Fatal("submit blocked")
	}
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("loop did not consult session execution admission")
	}
	for _, ev := range b.events() {
		if _, ok := ev.(event.TurnStarted); ok {
			t.Fatal("TurnStarted published while first-step admission blocked")
		}
	}
	close(b.release)
	if _, ok := drainToTerminal(t, &b.recordingPublisher).(event.TurnDone); !ok {
		t.Fatal("turn did not complete after admission release")
	}
}

func TestFailedRequiredTurnBoundaryDoesNotChainQueuedInput(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &gatedFirstInference{entered: make(chan struct{}), release: make(chan struct{})}
	boundaryErr := errors.New("required checkpoint failed")
	b := &failingTerminalBoundary{err: boundaryErr}
	l, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, b, runtimeConfig{Client: client, Model: testModel(), DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	startTurn(t, l, &b.recordingPublisher, nil)
	<-client.entered
	queuedID := mustID(t)
	queued := command.UserInput{Header: command.Header{CommandID: queuedID, CreatedAt: time.Now()}}
	select {
	case l.Commands <- queued:
	case <-time.After(time.Second):
		t.Fatal("queue submit blocked")
	}
	blockUntilEvents(t, &b.recordingPublisher, func(events []event.Event) bool {
		for _, ev := range events {
			if q, ok := ev.(event.InputQueued); ok && q.Cause.CommandID == queuedID {
				return true
			}
		}
		return false
	})
	close(client.release)
	blockUntilEvents(t, &b.recordingPublisher, func(events []event.Event) bool {
		for _, ev := range events {
			if _, ok := ev.(event.LoopIdle); ok {
				return true
			}
		}
		return false
	})
	turnStarts := 0
	queuedCanceled := 0
	terminals := 0
	for _, ev := range b.events() {
		switch e := ev.(type) {
		case event.TurnStarted:
			turnStarts++
		case event.InputCancelled:
			if e.Cause.CommandID == queuedID {
				queuedCanceled++
			}
		}
		if ev.EndsTurn() {
			terminals++
		}
	}
	if turnStarts != 1 || queuedCanceled != 1 || terminals != 1 {
		t.Fatalf("starts=%d queuedCanceled=%d terminals=%d, want 1/1/1", turnStarts, queuedCanceled, terminals)
	}
}

func TestShutdownCancelsParkedFirstStepAdmissionWithoutTurnStarted(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &blockingStepAdmission{entered: make(chan struct{}), release: make(chan struct{})}
	l, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, b, runtimeConfig{Client: &fakeLLM{chunks: []content.Chunk{textChunk("unused")}}, Model: testModel(), DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	inputID := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: inputID, CreatedAt: time.Now()}}
	<-b.entered
	ack := make(chan error, 1)
	l.Commands <- command.Shutdown{Header: command.Header{CommandID: mustID(t), CreatedAt: time.Now()}, Ack: ack}
	if err := <-ack; err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.Done:
	case <-time.After(time.Second):
		t.Fatal("loop did not exit after canceling parked admission")
	}
	starts, canceled := 0, 0
	for _, ev := range b.events() {
		switch e := ev.(type) {
		case event.TurnStarted:
			starts++
		case event.InputCancelled:
			if e.Cause.CommandID == inputID {
				canceled++
			}
		}
	}
	if starts != 0 || canceled != 1 {
		t.Fatalf("TurnStarted=%d InputCancelled=%d, want 0/1", starts, canceled)
	}
}
