package loopruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

type selectiveCheckedFailurePublisher struct {
	*recordingPublisher
	fail func(event.Event) bool
	err  error
}

type committedThenFailingContextBoundary struct {
	recordingPublisher
	err error
}

func (b *committedThenFailingContextBoundary) CommitBoundary(ctx context.Context, value event.Event) error {
	if err := b.PublishEventChecked(ctx, value); err != nil {
		return err
	}
	if _, ok := value.(event.StepDone); ok {
		return b.err
	}
	return nil
}

func (b *committedThenFailingContextBoundary) CommitContextBoundary(ctx context.Context, value event.Event) (bool, error) {
	if err := b.PublishEventChecked(ctx, value); err != nil {
		return false, err
	}
	return true, b.err
}

func (p *selectiveCheckedFailurePublisher) PublishEventChecked(ctx context.Context, value event.Event) error {
	if p.fail(value) {
		return p.err
	}
	return p.recordingPublisher.PublishEventChecked(ctx, value)
}

func eventCount[T event.Event](events []event.Event) int {
	count := 0
	for _, value := range events {
		if _, ok := value.(T); ok {
			count++
		}
	}
	return count
}

func foldContextMutationMessages(events []event.Event) content.AgenticMessages {
	messages := make(content.AgenticMessages, 0)
	for _, value := range events {
		switch typed := value.(type) {
		case event.TurnStarted:
			messages = append(messages, typed.Message)
		case event.StepDone:
			messages = append(messages, typed.Messages...)
		case event.TurnFoldedInto:
			messages = append(messages, typed.Message)
		}
	}
	return messages
}

func assertContextMutationRestoreEquivalent(t *testing.T, events []event.Event, live content.AgenticMessages) {
	t.Helper()
	restored := foldContextMutationMessages(events)
	if !reflect.DeepEqual(live, restored) {
		t.Fatalf("live messages = %#v, durable-fold messages = %#v", live, restored)
	}
}

func newAtomicityLoop(t *testing.T, publisher eventPublisher, client inference.Client, basis event.ContextBasis) *Loop {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	seed := RestoredState{Basis: basis, HasBasis: true}
	instance, err := newLoopWithSeed(ctx, mustID(t), mustID(t), Provenance{}, publisher, runtimeConfig{
		Client: client, Model: testModel(), DrainTimeout: 200 * time.Millisecond,
	}, nil, "", &seed)
	if err != nil {
		t.Fatalf("newLoopWithSeed() error = %v", err)
	}
	return instance
}

func waitForTurnTerminal(t *testing.T, recorder *recordingPublisher) {
	t.Helper()
	blockUntilEvents(t, recorder, func(events []event.Event) bool {
		return eventCount[event.TurnDone](events)+eventCount[event.TurnFailed](events)+eventCount[event.TurnInterrupted](events)+eventCount[event.TurnRejected](events) > 0
	})
}

func runFoldAtomicityScenario(t *testing.T, publisher eventPublisher, recorder *recordingPublisher, basis event.ContextBasis) content.AgenticMessages {
	t.Helper()
	blocking := newBlockingTool()
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "fold-call", "Block", `{}`)},
		{textChunk("unused")},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	instance, err := newLoopWithSeed(ctx, mustID(t), mustID(t), Provenance{}, publisher, runtimeConfig{
		Client: client, Model: testModel(), Tools: agenticToolSet([]tool.InvokableTool{blocking}, 25, 100), DrainTimeout: 200 * time.Millisecond,
	}, nil, "", &RestoredState{Basis: basis, HasBasis: true})
	if err != nil {
		t.Fatalf("newLoopWithSeed() error = %v", err)
	}
	instance.Commands <- command.UserInput{Header: command.Header{CommandID: mustID(t)}, Blocks: []content.Block{&content.TextBlock{Text: "start"}}}
	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking tool did not start")
	}
	instance.Commands <- command.UserInput{Header: command.Header{CommandID: mustID(t)}, Blocks: []content.Block{&content.TextBlock{Text: "must not fold"}}}
	close(blocking.release)
	waitForTurnTerminal(t, recorder)
	messages, _, err := instance.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	return messages
}

func TestTurnFoldedIntoContextMutationIsAtomic(t *testing.T) {
	t.Parallel()
	publishErr := errors.New("checked fold publication failed")
	tests := []struct {
		name      string
		basis     event.ContextBasis
		publisher func(*recordingPublisher) eventPublisher
	}{
		{
			name:  "revision overflow",
			basis: event.ContextBasis{Revision: ^event.ContextRevision(0) - 2, ThroughEventID: uuid.UUID{1}},
			publisher: func(recorder *recordingPublisher) eventPublisher {
				return recorder
			},
		},
		{
			name:  "checked publication failure",
			basis: event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{1}},
			publisher: func(recorder *recordingPublisher) eventPublisher {
				return &selectiveCheckedFailurePublisher{
					recordingPublisher: recorder,
					err:                publishErr,
					fail: func(value event.Event) bool {
						_, ok := value.(event.TurnFoldedInto)
						return ok
					},
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &recordingPublisher{}
			messages := runFoldAtomicityScenario(t, tt.publisher(recorder), recorder, tt.basis)
			if got := eventCount[event.TurnFoldedInto](recorder.events()); got != 0 {
				t.Fatalf("TurnFoldedInto events = %d, want 0", got)
			}
			if len(messages) != 3 {
				t.Fatalf("live messages = %d, want 3 committed pre-fold messages", len(messages))
			}
			assertContextMutationRestoreEquivalent(t, recorder.events(), messages)
		})
	}
}

func TestContextMutationOverflowPublishesNoMutationEvent(t *testing.T) {
	t.Parallel()
	maxRevision := ^event.ContextRevision(0)
	tests := []struct {
		name string
		run  func(*testing.T) ([]event.Event, content.AgenticMessages, error)
		want func([]event.Event) int
	}{
		{
			name: "TurnStarted",
			run: func(t *testing.T) ([]event.Event, content.AgenticMessages, error) {
				recorder := &recordingPublisher{}
				instance := newAtomicityLoop(t, recorder, &recordingLLM{chunks: []content.Chunk{textChunk("unused")}}, event.ContextBasis{Revision: maxRevision, ThroughEventID: uuid.UUID{1}})
				instance.Commands <- command.UserInput{Header: command.Header{CommandID: mustID(t)}, Blocks: []content.Block{&content.TextBlock{Text: "must not start"}}}
				waitForTurnTerminal(t, recorder)
				messages, _, err := instance.Snapshot(context.Background())
				return recorder.events(), messages, err
			},
			want: eventCount[event.TurnStarted],
		},
		{
			name: "StepDone",
			run: func(t *testing.T) ([]event.Event, content.AgenticMessages, error) {
				recorder := &recordingPublisher{}
				instance := newAtomicityLoop(t, recorder, &recordingLLM{chunks: []content.Chunk{textChunk("must not commit")}}, event.ContextBasis{Revision: maxRevision - 1, ThroughEventID: uuid.UUID{1}})
				instance.Commands <- command.UserInput{Header: command.Header{CommandID: mustID(t)}, Blocks: []content.Block{&content.TextBlock{Text: "start"}}}
				waitForTurnTerminal(t, recorder)
				messages, _, err := instance.Snapshot(context.Background())
				return recorder.events(), messages, err
			},
			want: eventCount[event.StepDone],
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, messages, err := tt.run(t)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if got := tt.want(events); got != 0 {
				t.Fatalf("mutation event count = %d, want 0", got)
			}
			wantMessages := 0
			if tt.name == "StepDone" {
				wantMessages = 1
			}
			if len(messages) != wantMessages {
				t.Fatalf("live messages = %d, want %d", len(messages), wantMessages)
			}
			assertContextMutationRestoreEquivalent(t, events, messages)
		})
	}
}

func TestContextRequestShapeOverflowPublishesNoChange(t *testing.T) {
	t.Parallel()
	maxBasis := event.ContextBasis{Revision: ^event.ContextRevision(0), ThroughEventID: uuid.UUID{1}}
	tests := []struct {
		name string
		run  func(*testing.T, *Loop) command.LoopChangeResult
		want func([]event.Event) int
	}{
		{name: "LoopModeChanged", run: func(t *testing.T, instance *Loop) command.LoopChangeResult { return sendSetMode(t, instance, "build") }, want: eventCount[event.LoopModeChanged]},
		{name: "LoopInferenceChanged", run: func(t *testing.T, instance *Loop) command.LoopChangeResult {
			return sendChange(t, instance, command.ChangeLoopInference{Effort: testEffortHigh, SetEffort: true})
		}, want: eventCount[event.LoopInferenceChanged]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &recordingPublisher{}
			client := &recordingLLM{chunks: []content.Chunk{textChunk("unused")}}
			bound := modeDefinition(t, client)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			instance, err := NewRestored(ctx, mustID(t), mustID(t), Provenance{}, recorder, bound, RestoredState{Basis: maxBasis, HasBasis: true})
			if err != nil {
				t.Fatalf("NewRestored() error = %v", err)
			}
			result := tt.run(t, instance)
			var changeErr *loop.ChangeError
			if !errors.As(result.Err, &changeErr) || changeErr.Kind != loop.ChangeDurableAppendFailed {
				t.Fatalf("change error = %T %v, want ChangeDurableAppendFailed", result.Err, result.Err)
			}
			if got := tt.want(recorder.events()); got != 0 {
				t.Fatalf("change event count = %d, want 0", got)
			}
		})
	}
}

func TestContextMutationCheckedPublicationFailureIsAtomic(t *testing.T) {
	t.Parallel()
	publishErr := errors.New("checked mutation publication failed")
	tests := []struct {
		name string
		fail func(event.Event) bool
		run  func(*testing.T, *Loop) error
		want func([]event.Event) int
	}{
		{name: "TurnStarted", fail: func(value event.Event) bool { _, ok := value.(event.TurnStarted); return ok }, run: func(t *testing.T, instance *Loop) error {
			instance.Commands <- command.UserInput{Header: command.Header{CommandID: mustID(t)}, Blocks: []content.Block{&content.TextBlock{Text: "start"}}}
			return nil
		}, want: eventCount[event.TurnStarted]},
		{name: "StepDone", fail: func(value event.Event) bool { _, ok := value.(event.StepDone); return ok }, run: func(t *testing.T, instance *Loop) error {
			instance.Commands <- command.UserInput{Header: command.Header{CommandID: mustID(t)}, Blocks: []content.Block{&content.TextBlock{Text: "step"}}}
			return nil
		}, want: eventCount[event.StepDone]},
		{name: "LoopInferenceChanged", fail: func(value event.Event) bool { _, ok := value.(event.LoopInferenceChanged); return ok }, run: func(t *testing.T, instance *Loop) error {
			return sendChange(t, instance, command.ChangeLoopInference{Effort: testEffortHigh, SetEffort: true}).Err
		}, want: eventCount[event.LoopInferenceChanged]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &recordingPublisher{}
			publisher := &selectiveCheckedFailurePublisher{recordingPublisher: recorder, fail: tt.fail, err: publishErr}
			instance := newAtomicityLoop(t, publisher, &recordingLLM{chunks: []content.Chunk{textChunk("answer")}}, event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{1}})
			runErr := tt.run(t, instance)
			if tt.name == "LoopInferenceChanged" {
				var changeErr *loop.ChangeError
				if !errors.As(runErr, &changeErr) || !errors.Is(runErr, publishErr) {
					t.Fatalf("change error = %T %v, want wrapped checked publication error", runErr, runErr)
				}
			} else {
				waitForTurnTerminal(t, recorder)
			}
			if got := tt.want(recorder.events()); got != 0 {
				t.Fatalf("failed mutation event count = %d, want 0", got)
			}
			messages, _, err := instance.Snapshot(context.Background())
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			wantMessages := 0
			if tt.name == "StepDone" {
				wantMessages = 1
			}
			if len(messages) != wantMessages {
				t.Fatalf("live messages = %d, want %d", len(messages), wantMessages)
			}
			assertContextMutationRestoreEquivalent(t, recorder.events(), messages)
		})
	}
}

func TestContextBoundaryPostCommitFailurePreservesLiveRestoreEquivalence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "StepDone durable before checkpoint failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			boundary := &committedThenFailingContextBoundary{err: errors.New("checkpoint failed after event commit")}
			instance := newAtomicityLoop(t, boundary, &recordingLLM{chunks: []content.Chunk{textChunk("durable answer")}}, event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{1}})
			instance.Commands <- command.UserInput{Header: command.Header{CommandID: mustID(t)}, Blocks: []content.Block{&content.TextBlock{Text: "start"}}}
			waitForTurnTerminal(t, &boundary.recordingPublisher)
			messages, _, err := instance.Snapshot(context.Background())
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if eventCount[event.StepDone](boundary.events()) != 1 {
				t.Fatalf("StepDone count = %d, want 1 durable event", eventCount[event.StepDone](boundary.events()))
			}
			assertContextMutationRestoreEquivalent(t, boundary.events(), messages)
		})
	}
}

func TestContextRequestShapeCheckedPublicationFailureLeavesConfigUnchanged(t *testing.T) {
	t.Parallel()
	publishErr := errors.New("checked request-shape publication failed")
	tests := []struct {
		name string
		fail func(event.Event) bool
		run  func(*testing.T, *Loop) command.LoopChangeResult
		want func([]event.Event) int
	}{
		{
			name: "LoopModeChanged",
			fail: func(value event.Event) bool { _, ok := value.(event.LoopModeChanged); return ok },
			run:  func(t *testing.T, instance *Loop) command.LoopChangeResult { return sendSetMode(t, instance, "build") },
			want: eventCount[event.LoopModeChanged],
		},
		{
			name: "LoopInferenceChanged",
			fail: func(value event.Event) bool { _, ok := value.(event.LoopInferenceChanged); return ok },
			run: func(t *testing.T, instance *Loop) command.LoopChangeResult {
				return sendChange(t, instance, command.ChangeLoopInference{Effort: testEffortHigh, SetEffort: true})
			},
			want: eventCount[event.LoopInferenceChanged],
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &recordingPublisher{}
			publisher := &selectiveCheckedFailurePublisher{recordingPublisher: recorder, fail: tt.fail, err: publishErr}
			client := &recordingLLM{chunks: []content.Chunk{textChunk("answer")}}
			bound := modeDefinition(t, client)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			instance, err := NewRestored(ctx, mustID(t), mustID(t), Provenance{}, publisher, bound, RestoredState{
				Basis: event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{1}}, HasBasis: true,
			})
			if err != nil {
				t.Fatalf("NewRestored() error = %v", err)
			}
			result := tt.run(t, instance)
			var changeErr *loop.ChangeError
			if !errors.As(result.Err, &changeErr) || !errors.Is(result.Err, publishErr) {
				t.Fatalf("change error = %T %v, want wrapped checked publication error", result.Err, result.Err)
			}
			if got := tt.want(recorder.events()); got != 0 {
				t.Fatalf("failed request-shape events = %d, want 0", got)
			}
			runOneTurn(t, instance, recorder, "after failure")
			request := client.lastReq()
			if request.Model.Sampling.Effort != model.EffortLow || request.System != "base\n\nplan-i" {
				t.Fatalf("request config after failure = effort %q system %q, want low initial plan", request.Model.Sampling.Effort, request.System)
			}
		})
	}
}
