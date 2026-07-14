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
	bound := bindCfg(modeCfg(&stubLLM{}), uuid.UUID{1}, uuid.UUID{2})
	measurementFor := func(model inference.Model) event.ContextMeasurement {
		measurement := foldContextMeasurement(1)
		measurement.Model = model.Key()
		return measurement
	}
	base := validModel("base")
	swapped := validModel("swapped")
	routed := validModel("routed")
	tests := []struct {
		name    string
		events  []event.Event
		wantErr bool
	}{
		{
			name:   "absent runtime matches initial mode fallback",
			events: []event.Event{event.LoopStarted{}, event.ContextMeasured{Measurement: measurementFor(base)}},
		},
		{
			name:    "absent runtime mismatches initial mode fallback",
			events:  []event.Event{event.LoopStarted{}, event.ContextMeasured{Measurement: measurementFor(routed)}},
			wantErr: true,
		},
		{
			name:   "absent runtime matches selected mode fallback",
			events: []event.Event{event.LoopStarted{InitialMode: "swap"}, event.ContextMeasured{Measurement: measurementFor(swapped)}},
		},
		{
			name:    "absent runtime mismatches selected mode fallback",
			events:  []event.Event{event.LoopStarted{InitialMode: "swap"}, event.ContextMeasured{Measurement: measurementFor(base)}},
			wantErr: true,
		},
		{
			name:   "absent runtime matches changed mode fallback",
			events: []event.Event{event.LoopModeChanged{Mode: "swap"}, event.ContextMeasured{Measurement: measurementFor(swapped)}},
		},
		{
			name:    "absent runtime mismatches changed mode fallback",
			events:  []event.Event{event.LoopModeChanged{Mode: "swap"}, event.ContextMeasured{Measurement: measurementFor(base)}},
			wantErr: true,
		},
		{
			name:   "durable runtime matches measurement",
			events: []event.Event{event.LoopStarted{Runtime: runtimeForModel(routed)}, event.ContextMeasured{Measurement: measurementFor(routed)}},
		},
		{
			name:    "durable runtime mismatches measurement",
			events:  []event.Event{event.LoopStarted{Runtime: runtimeForModel(routed)}, event.ContextMeasured{Measurement: measurementFor(base)}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := foldLoopForRestore(bound, tt.events, false)
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

func TestRestoredContextConfigMismatchDisposition(t *testing.T) {
	t.Parallel()
	bound := bindCfg(modeCfg(&stubLLM{}), uuid.UUID{3}, uuid.UUID{4})
	measurementFor := func(model inference.Model) event.ContextMeasurement {
		measurement := foldContextMeasurement(3)
		measurement.Model = model.Key()
		return measurement
	}
	base := validModel("base")
	legacy := validModel("legacy")
	matching := event.ConfigFingerprint{ModelID: "base", SystemPromptRev: "same"}
	tests := []struct {
		name           string
		persisted      event.ConfigFingerprint
		live           event.ConfigFingerprint
		allowMismatch  bool
		events         []event.Event
		wantContext    bool
		wantConfigErr  bool
		wantRestoreErr bool
	}{
		{
			name:          "overridden changed model discards legacy fallback context",
			persisted:     event.ConfigFingerprint{ModelID: "legacy"},
			live:          event.ConfigFingerprint{ModelID: "base"},
			allowMismatch: true,
			events:        []event.Event{event.LoopStarted{}, event.ContextMeasured{Measurement: measurementFor(legacy)}},
		},
		{
			name:          "overridden request shape change discards same model context",
			persisted:     event.ConfigFingerprint{ModelID: "base", SystemPromptRev: "old"},
			live:          event.ConfigFingerprint{ModelID: "base", SystemPromptRev: "new"},
			allowMismatch: true,
			events:        []event.Event{event.LoopStarted{}, event.ContextMeasured{Measurement: measurementFor(base)}},
		},
		{
			name:           "override does not suppress corrupt durable runtime context",
			persisted:      event.ConfigFingerprint{ModelID: "legacy"},
			live:           event.ConfigFingerprint{ModelID: "base"},
			allowMismatch:  true,
			events:         []event.Event{event.LoopStarted{Runtime: runtimeForModel(legacy)}, event.ContextMeasured{Measurement: measurementFor(base)}},
			wantRestoreErr: true,
		},
		{
			name:          "actual mismatch without override rejects",
			persisted:     event.ConfigFingerprint{ModelID: "legacy"},
			live:          event.ConfigFingerprint{ModelID: "base"},
			events:        []event.Event{event.LoopStarted{}, event.ContextMeasured{Measurement: measurementFor(legacy)}},
			wantConfigErr: true,
		},
		{
			name:          "override with no actual mismatch preserves valid context",
			persisted:     matching,
			live:          matching,
			allowMismatch: true,
			events:        []event.Event{event.LoopStarted{}, event.ContextMeasured{Measurement: measurementFor(base)}},
			wantContext:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			discardContext, err := restoredContextDisposition(tt.persisted, tt.live, tt.allowMismatch)
			var configErr *ConfigMismatchError
			if errors.As(err, &configErr) != tt.wantConfigErr {
				t.Fatalf("disposition error = %T %v, wantConfigErr=%v", err, err, tt.wantConfigErr)
			}
			if tt.wantConfigErr {
				return
			}
			folded, err := foldLoopForRestore(bound, tt.events, discardContext)
			var restoreErr *RestoreError
			var mismatchErr *RestoredContextModelMismatchError
			gotRestoreErr := errors.As(err, &restoreErr) && restoreErr.Kind == RestoreReplayFailed && errors.As(err, &mismatchErr)
			if gotRestoreErr != tt.wantRestoreErr {
				t.Fatalf("restore error = %T %v, wantRestoreErr=%v", err, err, tt.wantRestoreErr)
			}
			if tt.wantRestoreErr {
				return
			}
			if folded.HasContext != tt.wantContext {
				t.Fatalf("HasContext = %v, want %v", folded.HasContext, tt.wantContext)
			}
		})
	}
}
