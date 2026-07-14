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
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
)

type compactionTerminalFailurePublisher struct {
	*recordingPublisher
	mu           sync.Mutex
	err          error
	failRejected bool
	enabled      bool
	failed       bool
}

func (p *compactionTerminalFailurePublisher) PublishEventChecked(ctx context.Context, value event.Event) error {
	p.mu.Lock()
	_, committed := value.(event.CompactionCommitted)
	_, rejected := value.(event.CompactionRejected)
	if p.enabled && (committed || p.failRejected && rejected) && !p.failed {
		p.failed = true
		err := p.err
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()
	return p.recordingPublisher.PublishEventChecked(ctx, value)
}

func (p *compactionTerminalFailurePublisher) enable() {
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
		{name: "success resets actor and turn before continuation", wantCommitted: true, wantPrimaryCalls: 1, wantSummaryActive: true, observeProjection: true},
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
			publisher := &compactionTerminalFailurePublisher{recordingPublisher: recorder, err: errors.New("commit append failed")}
			client := &contextOrderClient{recorder: recorder}
			counter := &loopContextCounter{
				capability: contextTestCapability(inference.CountQualityExactLocal), counts: []content.TokenCount{65, 20},
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
				Summary: validFinalizationSummary(), PostCount: testCompactionPostCount(validFinalizationMeasurement(83)),
			}
			success.PostCount.Model = measured.Measurement.Model
			if tt.mutateSuccess != nil {
				tt.mutateSuccess(success)
				success.PostCount.Model = success.Model
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
				actor.Commands <- command.CancelQueuedInput{Header: command.Header{CommandID: uuid.UUID{85}}, TargetCommandID: queuedID}
				blockUntilEvents(t, recorder, func(events []event.Event) bool {
					for _, published := range events {
						if canceled, ok := published.(event.InputCancelled); ok && canceled.Cause.CommandID == queuedID {
							return true
						}
					}
					return false
				})
				close(replacementRelease)
			}

			terminal := drainToTerminal(t, recorder)
			if tt.wantSummaryActive {
				if _, ok := terminal.(event.TurnDone); !ok {
					t.Fatalf("terminal = %T %+v, want TurnDone after replacement continuation", terminal, terminal)
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

func TestLoopMalformedStartedCompactionFinalizesInternalRejection(t *testing.T) {
	tests := []struct {
		name             string
		mutate           func(contextCompactionAwaitResult) contextCompactionAwaitResult
		failRejectAppend bool
	}{
		{
			name: "zero success model",
			mutate: func(result contextCompactionAwaitResult) contextCompactionAwaitResult {
				result.Proposal.Success.Model = inference.ModelKey{}
				return result
			},
		},
		{
			name: "zero success fingerprint",
			mutate: func(result contextCompactionAwaitResult) contextCompactionAwaitResult {
				result.Proposal.Success.RequestFingerprint = [32]byte{}
				return result
			},
		},
		{
			name: "post context model differs from success model",
			mutate: func(result contextCompactionAwaitResult) contextCompactionAwaitResult {
				result.Proposal.Success.PostCount.Model = inference.ModelKey{Provider: "test", Model: "different"}
				return result
			},
		},
		{
			name: "committed disposition carries rejection",
			mutate: func(contextCompactionAwaitResult) contextCompactionAwaitResult {
				return contextCompactionAwaitResult{
					Disposition: contextCompactionAwaitCommitted,
					Proposal: compactionFinalizationProposal{
						RejectReason: event.CompactRejectExecutionFailed,
					},
				}
			},
		},
		{
			name: "rejected disposition carries success",
			mutate: func(result contextCompactionAwaitResult) contextCompactionAwaitResult {
				result.Disposition = contextCompactionAwaitRejected
				return result
			},
		},
		{
			name: "unknown disposition carries rejection",
			mutate: func(contextCompactionAwaitResult) contextCompactionAwaitResult {
				return contextCompactionAwaitResult{
					Disposition: contextCompactionAwaitUnknown,
					Proposal: compactionFinalizationProposal{
						RejectReason: event.CompactRejectExecutionFailed,
					},
				}
			},
		},
		{
			name:             "internal rejection append failure remains infrastructure failure",
			failRejectAppend: true,
			mutate: func(result contextCompactionAwaitResult) contextCompactionAwaitResult {
				result.Proposal.Success.RequestFingerprint = [32]byte{}
				return result
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			appendErr := errors.New("internal rejection append failed")
			publisher := &compactionTerminalFailurePublisher{
				recordingPublisher: recorder, err: appendErr, failRejected: tt.failRejectAppend,
			}
			client := &contextOrderClient{recorder: recorder}
			counter := &loopContextCounter{
				capability: contextTestCapability(inference.CountQualityExactLocal), counts: []content.TokenCount{65, 20},
			}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			sink := newContextAwaitSink()
			sessionID, loopID := uuid.UUID{91}, uuid.UUID{92}
			actor, err := newWithConfig(ctx, sessionID, loopID, Provenance{}, publisher, runtimeConfig{
				Client: client, Model: model, System: "system", DrainTimeout: 200 * time.Millisecond,
				ContextCounter: counter, CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				Compaction: &loop.CompactionPolicy{
					Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000,
					ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
				},
				compactionSink: sink,
			})
			if err != nil {
				t.Fatalf("newWithConfig() error = %v", err)
			}

			original := replacementTestMessage("original input")
			startTurn(t, actor, recorder, original.Blocks)
			first := <-sink.started
			if first.Attempt == nil || len(first.Attempt.WaiterCommandIDs) != 1 {
				t.Fatalf("first compaction disposition = %+v, want one-waiter attempt", first)
			}
			measured, _ := contextEvents(recorder.events())
			if measured == nil {
				t.Fatal("missing pre-compaction measurement")
			}
			success := &compactionPreparedSuccess{
				Model: measured.Measurement.Model, RequestFingerprint: measured.Measurement.RequestFingerprint,
				Summary: replacementTestMessage("malformed summary must not activate"), PostCount: testCompactionPostCount(validFinalizationMeasurement(93)),
			}
			success.PostCount.Model = success.Model
			result := tt.mutate(contextCompactionAwaitResult{
				Disposition: contextCompactionAwaitCommitted,
				Proposal:    compactionFinalizationProposal{Success: success},
			})
			if tt.failRejectAppend {
				publisher.enable()
			}
			sink.release <- result

			terminal := drainToTerminal(t, recorder)
			if tt.failRejectAppend {
				failed, ok := terminal.(event.TurnFailed)
				var finalizationErr *CompactionFinalizationError
				if !ok || !errors.As(failed.Err, &finalizationErr) || finalizationErr.Kind != CompactionFinalizationTerminalAppend || !errors.Is(failed.Err, appendErr) {
					t.Fatalf("turn terminal = %T %+v, want typed rejection terminal-append failure", terminal, terminal)
				}
			} else if _, ok := terminal.(event.TurnDone); !ok {
				t.Fatalf("turn terminal = %T %+v, want TurnDone after internal compaction rejection", terminal, terminal)
			}
			var terminalCount, waiterReplyCount int
			for _, published := range recorder.events() {
				switch value := published.(type) {
				case event.CompactionRejected:
					if value.AttemptID == first.Attempt.AttemptID {
						terminalCount++
						if value.RejectReason != event.CompactRejectInternal {
							t.Fatalf("rejection reason = %v, want %v", value.RejectReason, event.CompactRejectInternal)
						}
					}
				case event.CompactWaiterRejected:
					if value.AttemptID == first.Attempt.AttemptID {
						waiterReplyCount++
						if value.Reason != event.CompactRejectInternal || value.Cause.CommandID != first.Attempt.WaiterCommandIDs[0] {
							t.Fatalf("waiter rejection = %+v, want canonical internal reply", value)
						}
					}
				case event.CompactionCommitted, event.CompactWaiterResolved:
					t.Fatalf("malformed proposal published false success %T", published)
				}
			}
			wantTerminalCount, wantWaiterReplyCount := 1, 1
			if tt.failRejectAppend {
				wantTerminalCount, wantWaiterReplyCount = 0, 0
			}
			if terminalCount != wantTerminalCount || waiterReplyCount != wantWaiterReplyCount {
				t.Fatalf("compaction terminal/replies = %d/%d, want %d/%d", terminalCount, waiterReplyCount, wantTerminalCount, wantWaiterReplyCount)
			}
			requests := client.requestSnapshot()
			if tt.failRejectAppend {
				if len(requests) != 0 {
					t.Fatalf("primary requests = %#v, want none after fatal finalization failure", requests)
				}
			} else if len(requests) != 1 || len(requests[0].Messages) != 1 || !reflect.DeepEqual(requests[0].Messages[0], original) {
				t.Fatalf("primary requests = %#v, want original uncompacted turn context", requests)
			}
			messages, _, snapshotErr := actor.Snapshot(context.Background())
			if snapshotErr != nil {
				t.Fatalf("Snapshot() error = %v", snapshotErr)
			}
			for _, message := range messages {
				if reflect.DeepEqual(message, success.Summary) {
					t.Fatalf("malformed summary activated in actor state: %#v", messages)
				}
			}

			if tt.failRejectAppend {
				return
			}
			secondCommandID := uuid.UUID{94}
			sendCompact(t, actor, sessionID, loopID, secondCommandID, identity.AgencyUser)
			select {
			case second := <-sink.started:
				if second.Attempt == nil || second.Attempt.AttemptID == first.Attempt.AttemptID ||
					!equalUUIDs(second.Attempt.WaiterCommandIDs, []uuid.UUID{secondCommandID}) {
					t.Fatalf("second compaction disposition = %+v, want fresh one-waiter lane", second)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("later compaction did not reuse cleared control lane")
			}
		})
	}
}
