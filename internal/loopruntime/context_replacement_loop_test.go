package loopruntime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
)

type compactionCommitFailurePublisher struct {
	*recordingPublisher
	mu      sync.Mutex
	err     error
	enabled bool
	failed  bool
}

func (p *compactionCommitFailurePublisher) PublishEventChecked(ctx context.Context, value event.Event) error {
	p.mu.Lock()
	if _, committed := value.(event.CompactionCommitted); p.enabled && committed && !p.failed {
		p.failed = true
		err := p.err
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()
	return p.recordingPublisher.PublishEventChecked(ctx, value)
}

func (p *compactionCommitFailurePublisher) enable() {
	p.mu.Lock()
	p.enabled = true
	p.mu.Unlock()
}

func TestLoopCompactionOutcomeAppliesReplacementAfterDurableCommit(t *testing.T) {
	tests := []struct {
		name              string
		mutateSuccess     func(*compactionPreparedSuccess)
		failCommit        bool
		wantCommitted     bool
		wantReject        event.CompactRejectReason
		wantPrimaryCalls  int
		wantSummaryActive bool
		observeProjection bool
	}{
		{name: "success resets actor and turn before continuation directive", wantCommitted: true, wantSummaryActive: true, observeProjection: true},
		{name: "failed fingerprint CAS rejects stale without mutation", mutateSuccess: func(success *compactionPreparedSuccess) {
			success.RequestFingerprint = [32]byte{0xee}
		}, wantReject: event.CompactRejectStaleBasis, wantPrimaryCalls: 1},
		{name: "failed canonical append leaves actor unchanged", failCommit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			publisher := &compactionCommitFailurePublisher{recordingPublisher: recorder, err: errors.New("commit append failed")}
			client := &contextOrderClient{recorder: recorder}
			counter := &loopContextCounter{
				capability: contextTestCapability(inference.CountQualityExactLocal), counts: []content.TokenCount{65},
			}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			sink := newContextAwaitSink()
			config := runtimeConfig{
				Client: client, Model: model, System: "system", DrainTimeout: 200 * time.Millisecond,
				ContextCounter: counter, CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				Compaction: &loop.CompactionPolicy{
					Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000,
					ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
				},
				compactionSink: sink,
			}
			replacementApplied := make(chan struct{})
			replacementRelease := make(chan struct{})
			if tt.observeProjection {
				config.afterContextReplacement = func() {
					close(replacementApplied)
					<-replacementRelease
				}
			}
			actor, err := newWithConfig(ctx, uuid.UUID{81}, uuid.UUID{82}, Provenance{}, publisher, config)
			if err != nil {
				t.Fatalf("newWithConfig() error = %v", err)
			}
			original := replacementTestMessage("original input")
			startTurn(t, actor, recorder, original.Blocks)
			disposition := <-sink.started
			if disposition.Attempt == nil {
				t.Fatal("missing compaction attempt")
			}
			measured, _ := contextEvents(recorder.events())
			if measured == nil {
				t.Fatal("missing pre-compaction measurement")
			}
			success := &compactionPreparedSuccess{
				Model: measured.Measurement.Model, RequestFingerprint: measured.Measurement.RequestFingerprint,
				Summary: validFinalizationSummary(), PostContext: validFinalizationMeasurement(83),
			}
			success.PostContext.Model = measured.Measurement.Model
			success.PostContext.Basis = event.ContextBasis{}
			if tt.mutateSuccess != nil {
				tt.mutateSuccess(success)
				success.PostContext.Model = success.Model
			}
			if tt.failCommit {
				publisher.enable()
			}
			queuedID := uuid.UUID{84}
			if tt.observeProjection {
				actor.Commands <- command.UserInput{
					Header: command.Header{CommandID: queuedID}, Blocks: []content.Block{&content.TextBlock{Text: "queued after basis"}},
				}
				if _, ok := awaitReply(t, recorder, queuedID).(event.InputQueued); !ok {
					t.Fatal("uncommitted input did not enter actor queue")
				}
			}
			sink.release <- contextCompactionAwaitResult{
				Disposition: contextCompactionAwaitCommitted,
				Proposal:    compactionFinalizationProposal{Success: success},
			}
			if tt.observeProjection {
				select {
				case <-replacementApplied:
				case <-time.After(2 * time.Second):
					t.Fatal("turn did not apply replacement directive")
				}
				projected, _, snapshotErr := actor.Snapshot(context.Background())
				if snapshotErr != nil {
					t.Fatalf("Snapshot() during replacement pause error = %v", snapshotErr)
				}
				if len(projected) != 1 || !reflect.DeepEqual(projected[0], success.Summary) {
					t.Fatalf("next actor dispatch observed messages %#v, want only committed summary", projected)
				}
				close(replacementRelease)
			}

			terminal := drainToTerminal(t, recorder)
			if tt.wantSummaryActive {
				failed, ok := terminal.(event.TurnFailed)
				var directive *contextReplacementDirective
				if !ok || !errors.As(failed.Err, &directive) || directive.AttemptID != disposition.Attempt.AttemptID {
					t.Fatalf("terminal = %T %+v, want typed replacement continuation directive", terminal, terminal)
				}
			}
			if tt.failCommit {
				failed, ok := terminal.(event.TurnFailed)
				var finalizationErr *CompactionFinalizationError
				if !ok || !errors.As(failed.Err, &finalizationErr) || finalizationErr.Kind != CompactionFinalizationTerminalAppend {
					t.Fatalf("terminal = %T %+v, want terminal-append TurnFailed", terminal, terminal)
				}
			}
			var committedCount int
			var rejected *event.CompactionRejected
			for _, published := range recorder.events() {
				switch value := published.(type) {
				case event.CompactionCommitted:
					committedCount++
				case event.CompactionRejected:
					copyOfValue := value
					rejected = &copyOfValue
				}
			}
			if (committedCount == 1) != tt.wantCommitted {
				t.Fatalf("committed events = %d, wantCommitted %v", committedCount, tt.wantCommitted)
			}
			if tt.wantReject != event.CompactRejectUnspecified && (rejected == nil || rejected.RejectReason != tt.wantReject) {
				t.Fatalf("rejection = %+v, want reason %v", rejected, tt.wantReject)
			}
			requests := client.requestSnapshot()
			if len(requests) != tt.wantPrimaryCalls {
				t.Fatalf("primary requests = %d, want %d", len(requests), tt.wantPrimaryCalls)
			}
			messages, _, snapshotErr := actor.Snapshot(context.Background())
			if snapshotErr != nil {
				t.Fatalf("Snapshot() error = %v", snapshotErr)
			}
			if tt.wantSummaryActive {
				if len(messages) == 0 || !reflect.DeepEqual(messages[0], success.Summary) {
					t.Fatalf("actor messages = %#v, want summary as committed base", messages)
				}
			} else {
				for _, message := range messages {
					if reflect.DeepEqual(message, success.Summary) {
						t.Fatalf("failed replacement activated summary: %#v", messages)
					}
				}
			}
			if tt.observeProjection {
				blockUntilEvents(t, recorder, func(events []event.Event) bool {
					for _, published := range events {
						if canceled, ok := published.(event.InputCancelled); ok && canceled.Cause.CommandID == queuedID {
							return true
						}
					}
					return false
				})
			}
		})
	}
}
