package sessionruntime

import (
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
	tests := []struct {
		name   string
		events []event.Event
		want   event.ContextMeasurement
		has    bool
	}{
		{name: "latest measurement", events: []event.Event{event.ContextMeasured{Measurement: first}, event.ContextMeasured{Measurement: second}}, want: second, has: true},
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
			if got.HasContext != tt.has || got.Context != tt.want {
				t.Fatalf("context = %#v has=%v; want %#v has=%v", got.Context, got.HasContext, tt.want, tt.has)
			}
		})
	}
}
