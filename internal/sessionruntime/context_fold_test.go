package sessionruntime

import (
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/inference"
)

func foldContextMeasurement(seed byte) event.ContextMeasurement {
	return event.ContextMeasurement{
		Basis: event.ContextBasis{Revision: event.ContextRevision(seed), ThroughEventID: uuid.UUID{seed}},
		Model: inference.ModelKey{Provider: "provider", Model: "model"}, RequestFingerprint: [32]byte{seed},
		InputTokens: content.TokenCount(seed), InputLimit: 100, Quality: inference.CountQualityExactLocal,
	}
}

func TestFoldLoopTracksAndInvalidatesContextMeasurement(t *testing.T) {
	t.Parallel()
	runtime := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}}
	first := foldContextMeasurement(1)
	second := foldContextMeasurement(2)
	mismatched := second
	mismatched.Model = inference.ModelKey{Provider: "other", Model: "model"}
	tests := []struct {
		name    string
		events  []event.Event
		want    event.ContextMeasurement
		has     bool
		wantErr bool
	}{
		{name: "latest measurement", events: []event.Event{event.ContextMeasured{Measurement: first}, event.ContextMeasured{Measurement: second}}, want: second, has: true},
		{name: "matching runtime measurement", events: []event.Event{event.LoopStarted{Runtime: runtime}, event.ContextMeasured{Measurement: first}}, want: first, has: true},
		{name: "mismatched runtime measurement rejected", events: []event.Event{event.LoopStarted{Runtime: runtime}, event.ContextMeasured{Measurement: mismatched}}, wantErr: true},
		{name: "measurement without runtime remains valid", events: []event.Event{event.ContextMeasured{Measurement: mismatched}}, want: mismatched, has: true},
		{name: "later runtime boundary clears prior no-runtime measurement", events: []event.Event{event.ContextMeasured{Measurement: mismatched}, event.LoopStarted{Runtime: runtime}}},
		{name: "runtime change invalidates", events: []event.Event{event.ContextMeasured{Measurement: first}, event.LoopInferenceChanged{Runtime: runtime}}},
		{name: "mode change invalidates", events: []event.Event{event.ContextMeasured{Measurement: first}, event.LoopModeChanged{Runtime: runtime}}},
		{name: "turn start invalidates", events: []event.Event{event.ContextMeasured{Measurement: first}, event.TurnStarted{}}},
		{name: "step commit invalidates", events: []event.Event{event.ContextMeasured{Measurement: first}, event.StepDone{}}},
		{name: "folded input invalidates", events: []event.Event{event.ContextMeasured{Measurement: first}, event.TurnFoldedInto{}}},
		{name: "new measurement after mutation wins", events: []event.Event{event.ContextMeasured{Measurement: first}, event.LoopInferenceChanged{Runtime: runtime}, event.ContextMeasured{Measurement: second}}, want: second, has: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := foldLoop(tt.events)
			var mismatch *RestoredContextModelMismatchError
			if errors.As(got.Err, &mismatch) != tt.wantErr {
				t.Fatalf("error = %T %v, wantErr=%v", got.Err, got.Err, tt.wantErr)
			}
			if got.HasContext != tt.has || got.Context != tt.want {
				t.Fatalf("context = %#v has=%v; want %#v has=%v", got.Context, got.HasContext, tt.want, tt.has)
			}
		})
	}
}

func TestFoldLoopForRestoreRejectsContextModelMismatch(t *testing.T) {
	t.Parallel()
	runtime := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}}
	measurement := foldContextMeasurement(1)
	mismatched := measurement
	mismatched.Model = inference.ModelKey{Provider: "other", Model: "model"}
	tests := []struct {
		name    string
		events  []event.Event
		wantErr bool
	}{
		{name: "matching", events: []event.Event{event.LoopStarted{Runtime: runtime}, event.ContextMeasured{Measurement: measurement}}},
		{name: "mismatched", events: []event.Event{event.LoopStarted{Runtime: runtime}, event.ContextMeasured{Measurement: mismatched}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := foldLoopForRestore(tt.events)
			if !tt.wantErr {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var restoreErr *RestoreError
			var mismatchErr *RestoredContextModelMismatchError
			if !errors.As(err, &restoreErr) || restoreErr.Kind != RestoreReplayFailed || !errors.As(err, &mismatchErr) {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}
