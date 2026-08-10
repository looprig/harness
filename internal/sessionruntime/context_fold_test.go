package sessionruntime

import (
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

func foldContextMeasurement(seed byte) event.ContextMeasurement {
	return event.ContextMeasurement{
		Basis: event.ContextBasis{Revision: event.ContextRevision(seed), ThroughEventID: uuid.UUID{seed}},
		Model: model.ModelKey{Provider: "provider", Model: "model"}, RequestFingerprint: [32]byte{seed},
		InputTokens: content.TokenCount(seed), InputLimit: 100, Quality: contextcount.CountQualityExactLocal,
	}
}

func TestFoldLoopTracksAndInvalidatesContextMeasurement(t *testing.T) {
	t.Parallel()
	runtime := event.ModelRuntime{Key: model.ModelKey{Provider: "provider", Model: "model"}, Limits: testContextLimits{WindowTokens: 100}}
	first := foldContextMeasurement(1)
	second := foldContextMeasurement(2)
	mismatched := second
	mismatched.Model = model.ModelKey{Provider: "other", Model: "model"}
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

func TestFoldLoopCarriesContextBasisWithoutMeasurement(t *testing.T) {
	t.Parallel()
	measurement := foldContextMeasurement(10)
	runtime := event.ModelRuntime{Key: measurement.Model, Limits: testContextLimits{WindowTokens: 100}}
	mutationID := uuid.UUID{11}
	tests := []struct {
		name     string
		mutation event.Event
	}{
		{name: "turn start", mutation: event.TurnStarted{Header: event.Header{EventID: mutationID}}},
		{name: "step done", mutation: event.StepDone{Header: event.Header{EventID: mutationID}}},
		{name: "folded input", mutation: event.TurnFoldedInto{Header: event.Header{EventID: mutationID}}},
		{name: "inference change", mutation: event.LoopInferenceChanged{Header: event.Header{EventID: mutationID}, Runtime: runtime}},
		{name: "mode change", mutation: event.LoopModeChanged{Header: event.Header{EventID: mutationID}, Runtime: runtime}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			folded := foldLoop([]event.Event{event.ContextMeasured{Measurement: measurement}, tt.mutation})
			wantBasis := event.ContextBasis{Revision: 11, ThroughEventID: mutationID}
			if folded.HasContext || !folded.HasBasis || folded.Basis != wantBasis {
				t.Fatalf("folded context=%v basis=%+v hasBasis=%v, want absent context and %+v", folded.HasContext, folded.Basis, folded.HasBasis, wantBasis)
			}
			seed := restoredStateFrom(folded, restoredInference{}, nil)
			if !seed.HasBasis || seed.Basis != wantBasis {
				t.Fatalf("restored basis=%+v has=%v, want %+v true", seed.Basis, seed.HasBasis, wantBasis)
			}
		})
	}
}

func TestFoldLoopIgnoresWorkflowActivityForModelContext(t *testing.T) {
	t.Parallel()

	activity := event.WorkflowActivity{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: uuid.UUID{20}}, EventID: uuid.UUID{21}},
		RunID:  uuid.UUID{22}, WorkflowName: "source_document_extract", WorkflowVersion: "v1",
		Kind: event.WorkflowActivityRunCompleted, Status: event.WorkflowRunStatusCompleted,
		Message: "bounded workflow diagnostic", OccurredAt: time.Date(2026, time.August, 9, 17, 0, 0, 0, time.UTC),
	}
	if err := event.ValidateEvent(activity); err != nil {
		t.Fatalf("WorkflowActivity validation = %v", err)
	}
	folded := foldLoop([]event.Event{activity})
	if folded.Err != nil {
		t.Fatalf("foldLoop() error = %v", folded.Err)
	}
	if len(folded.Msgs) != 0 || folded.HasContext || folded.HasBasis || folded.OpenTurn {
		t.Fatalf("folded workflow activity into model state: msgs=%d context=%v basis=%v openTurn=%v", len(folded.Msgs), folded.HasContext, folded.HasBasis, folded.OpenTurn)
	}
}

// TestFoldLoopRecordsDerivedPrefixAfterCompactionCommitted proves the
// restore-path half of the compaction-summary-authorization-inflation fix:
// a CompactionCommitted event replaces msgs with a model-generated summary
// (never something a human typed), so the fold must record that the leading
// message of the resulting Msgs is derived, not genuine conversation — and
// restoredStateFrom must carry that through to loopruntime.RestoredState,
// which seeds loopState.msgsDerivedPrefix on restore. Without this, a
// restored loop's next turn would clone the summary into its base with no
// marker at all, and capturePermissionReviewContext would credit it with
// gate.ReviewContextOriginUser (genuine human authority) — design §8.3's
// central defense, defeated by a restore.
func TestFoldLoopRecordsDerivedPrefixAfterCompactionCommitted(t *testing.T) {
	t.Parallel()
	turnID := uuid.UUID{1}
	committed := restoredCommitted(2)
	events := []event.Event{
		event.TurnStarted{Header: event.Header{EventID: turnID}, Message: restoredCompactionSummary("earlier human ask")},
		committed,
	}
	folded := foldLoop(events)
	if folded.Err != nil {
		t.Fatalf("foldLoop() error = %v", folded.Err)
	}
	if folded.DerivedPrefix != 1 {
		t.Fatalf("folded.DerivedPrefix = %d, want 1 (msgs was just replaced by a compaction summary)", folded.DerivedPrefix)
	}
	if len(folded.Msgs) != 1 || folded.Msgs[0] != committed.Summary {
		t.Fatalf("folded.Msgs = %+v, want exactly [committed.Summary] (CompactionCommitted fully replaces msgs)", folded.Msgs)
	}

	seed := restoredStateFrom(folded, restoredInference{}, nil)
	if seed.DerivedPrefix != 1 {
		t.Fatalf("restoredStateFrom().DerivedPrefix = %d, want 1", seed.DerivedPrefix)
	}

	// A loop that never compacted must seed DerivedPrefix at 0 — no false
	// positives that would treat genuine committed conversation as derived.
	uncompacted := foldLoop([]event.Event{
		event.TurnStarted{Header: event.Header{EventID: turnID}, Message: restoredCompactionSummary("genuine human ask")},
	})
	if uncompacted.DerivedPrefix != 0 {
		t.Fatalf("uncompacted foldLoop().DerivedPrefix = %d, want 0", uncompacted.DerivedPrefix)
	}
	if seed := restoredStateFrom(uncompacted, restoredInference{}, nil); seed.DerivedPrefix != 0 {
		t.Fatalf("uncompacted restoredStateFrom().DerivedPrefix = %d, want 0", seed.DerivedPrefix)
	}
}

func TestFoldLoopRestoresOnlyAutomaticAttemptLatch(t *testing.T) {
	t.Parallel()
	basis := foldContextMeasurement(7).Basis
	mutationID := uuid.UUID{8}
	canonical := func(reason event.CompactionReason) event.CompactionRejected {
		return event.CompactionRejected{Reason: reason, Basis: basis}
	}
	tests := []struct {
		name          string
		events        []event.Event
		wantAutomatic bool
		wantBasis     event.ContextBasis
	}{
		{name: "automatic rejection consumes unchanged basis", events: []event.Event{canonical(event.CompactionReasonAutomatic)}, wantAutomatic: true, wantBasis: basis},
		{name: "manual rejection does not consume latch", events: []event.Event{canonical(event.CompactionReasonManual)}},
		{name: "pre-start interrupt waiter rejection does not consume latch", events: []event.Event{event.ContextMeasured{Measurement: foldContextMeasurement(7)}, event.CompactWaiterRejected{Reason: event.CompactRejectInterrupted}}},
		{name: "pre-start shutdown waiter rejection does not consume latch", events: []event.Event{event.ContextMeasured{Measurement: foldContextMeasurement(7)}, event.CompactWaiterRejected{Reason: event.CompactRejectShuttingDown}}},
		{name: "later mutation retains only bounded old latch", events: []event.Event{canonical(event.CompactionReasonAutomatic), event.TurnStarted{Header: event.Header{EventID: mutationID}}}, wantAutomatic: true, wantBasis: basis},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			folded := foldLoop(tt.events)
			if folded.HasAutomaticBasis != tt.wantAutomatic || folded.AutomaticBasis != tt.wantBasis {
				t.Fatalf("automatic basis=%+v has=%v, want %+v has=%v", folded.AutomaticBasis, folded.HasAutomaticBasis, tt.wantBasis, tt.wantAutomatic)
			}
			seed := restoredStateFrom(folded, restoredInference{}, nil)
			if seed.HasAutomaticBasis != tt.wantAutomatic || seed.AutomaticBasis != tt.wantBasis {
				t.Fatalf("seed automatic basis=%+v has=%v, want %+v has=%v", seed.AutomaticBasis, seed.HasAutomaticBasis, tt.wantBasis, tt.wantAutomatic)
			}
		})
	}
}

func TestFoldLoopForRestoreRejectsContextModelMismatch(t *testing.T) {
	t.Parallel()
	bound := bindCfg(modeCfg(&stubLLM{}), uuid.UUID{1}, uuid.UUID{2})
	measurementFor := func(model model.Model) event.ContextMeasurement {
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
	measurementFor := func(model model.Model) event.ContextMeasurement {
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
