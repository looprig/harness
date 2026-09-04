package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

func TestModeValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		modes   []Mode
		initial ModeName
		kind    DefinitionErrorKind
	}{
		{name: "modes require initial", modes: []Mode{{Name: "plan"}}, kind: DefinitionMissingInitialMode},
		{name: "empty mode name", modes: []Mode{{Name: ""}}, initial: "plan", kind: DefinitionInvalidMode},
		{name: "duplicate mode", modes: []Mode{{Name: "plan"}, {Name: "plan"}}, initial: "plan", kind: DefinitionDuplicateMode},
		{name: "unknown initial", modes: []Mode{{Name: "plan"}}, initial: "build", kind: DefinitionInvalidInitialMode},
		{name: "invalid effort", modes: []Mode{{Name: "plan", Effort: model.Effort("huge")}}, initial: "plan", kind: DefinitionInvalidMode},
		{name: "invalid model sampling effort", modes: []Mode{{Name: "plan", Model: modelWithEffort(model.Effort("huge"))}}, initial: "plan", kind: DefinitionInvalidMode},
		{name: "invalid limits", modes: []Mode{{Name: "plan", ToolLimits: ToolLimits{Parallel: -1}}}, initial: "plan", kind: DefinitionInvalidMode},
		{name: "negative result bytes", modes: []Mode{{Name: "plan", ToolLimits: ToolLimits{ResultBytes: -1}}}, initial: "plan", kind: DefinitionInvalidMode},
		{name: "result bytes below minimum", modes: []Mode{{Name: "plan", ToolLimits: ToolLimits{ResultBytes: minToolResultBytes - 1}}}, initial: "plan", kind: DefinitionInvalidMode},
		{name: "initial without modes", initial: "plan", kind: DefinitionInvalidInitialMode},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithModes(tt.modes...)}
			if tt.initial != "" {
				opts = append(opts, WithInitialMode(tt.initial))
			}
			_, err := Define(opts...)
			var definitionErr *DefinitionError
			if !errors.As(err, &definitionErr) || definitionErr.Kind != tt.kind {
				t.Fatalf("Define error = %T %v, want %q", err, err, tt.kind)
			}
		})
	}
}

func TestDefinitionRejectsInvalidBaseSamplingEffort(t *testing.T) {
	t.Parallel()
	_, err := Define(WithName("agent"), WithInference(&fakeLLM{}, modelWithEffort(model.Effort("huge"))))
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionInvalidModel {
		t.Fatalf("Define error = %T %v, want invalid model", err, err)
	}
}

func TestModeResolutionAndCopy(t *testing.T) {
	t.Parallel()
	modeTools := []tool.Definition{testToolDefinition("mode", nil, nil)}
	modes := []Mode{{Name: "plan", Model: model.Model{}, Effort: model.EffortHigh, Tools: modeTools, ToolLimits: ToolLimits{Calls: 7, ResultBytes: 2048}, Instructions: "plan more"}}
	d := mustDefinition(t, WithToolLimits(ToolLimits{Iterations: 3, Parallel: 2, ResultBytes: 1024}), WithModes(modes...), WithInitialMode("plan"))
	modes[0].Name = "changed"
	modeTools[0] = testToolDefinition("changed", nil, nil)
	b, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	mode, ok := b.Mode("plan")
	if !ok {
		t.Fatal("plan mode missing")
	}
	if mode.Model.Name != testModel().Name || mode.Effort != model.EffortHigh || mode.Instructions != "plan more" {
		t.Fatalf("resolved mode = %+v", mode)
	}
	if mode.ToolLimits != (ToolLimits{Iterations: 3, Calls: 7, Parallel: 2, ResultBytes: 2048, CaptureBytes: DefaultToolResultCaptureBytes}) {
		t.Fatalf("resolved limits = %+v", mode.ToolLimits)
	}
}

func TestToolLimitsResultBytesResolution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		base     ToolLimits
		override ToolLimits
		want     ToolLimits
	}{
		{
			name:     "base inheritance",
			base:     ToolLimits{ResultBytes: 1024},
			override: ToolLimits{},
			want:     ToolLimits{ResultBytes: 1024},
		},
		{
			name:     "mode override",
			base:     ToolLimits{ResultBytes: 1024},
			override: ToolLimits{ResultBytes: 2048},
			want:     ToolLimits{ResultBytes: 2048},
		},
		{
			name:     "zero base stays off",
			base:     ToolLimits{},
			override: ToolLimits{},
			want:     ToolLimits{},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveLimits(tt.base, tt.override); got != tt.want {
				t.Fatalf("resolveLimits(%+v, %+v) = %+v, want %+v", tt.base, tt.override, got, tt.want)
			}
		})
	}
}

func TestModeEffectiveEffortIsStampedIntoModel(t *testing.T) {
	t.Parallel()
	baseModel := modelWithEffort(model.EffortLow)
	d, err := Define(
		WithName("agent"), WithInference(&fakeLLM{}, baseModel),
		WithModes(
			Mode{Name: "inherit", Model: modelWithEffort(model.EffortMax)},
			Mode{Name: "override", Effort: model.EffortHigh},
		),
		WithInitialMode("inherit"),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	b, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	base, _ := b.Mode("")
	inherit, _ := b.Mode("inherit")
	override, _ := b.Mode("override")
	for name, mode := range map[string]BoundMode{"base": base, "inherit": inherit} {
		if mode.Effort != model.EffortLow || mode.Model.Sampling.Effort != model.EffortLow {
			t.Errorf("%s effort = %q model effort = %q, want low", name, mode.Effort, mode.Model.Sampling.Effort)
		}
	}
	if override.Effort != model.EffortHigh || override.Model.Sampling.Effort != model.EffortHigh {
		t.Errorf("override effort = %q model effort = %q, want high", override.Effort, override.Model.Sampling.Effort)
	}
}

func modelWithEffort(effort model.Effort) model.Model {
	model := testModel()
	model.Sampling.Effort = effort
	return model
}

// TestToolLimitsCaptureBytesIsSeparateFromResultBytes pins the Step 3 contract:
// the retention ceiling and the model-text budget are independent knobs with
// different zero meanings. A single fixture could not show that — the pairs are
// enumerated so a mutant that derived either from the other disagrees on one.
func TestToolLimitsCaptureBytesIsSeparateFromResultBytes(t *testing.T) {
	t.Parallel()
	for _, resultBytes := range []int{0, 256, 4096} {
		for _, captureBytes := range []int{0, 256, 4096, 1 << 20} {
			limits := defaultLimits(ToolLimits{ResultBytes: resultBytes, CaptureBytes: captureBytes})
			// ResultBytes keeps its zero: zero means "do not bound the model text".
			if limits.ResultBytes != resultBytes {
				t.Errorf("result=%d capture=%d: ResultBytes = %d, want %d (zero must stay unbounded)",
					resultBytes, captureBytes, limits.ResultBytes, resultBytes)
			}
			wantCapture := captureBytes
			if wantCapture == 0 {
				wantCapture = DefaultToolResultCaptureBytes
			}
			if limits.CaptureBytes != wantCapture {
				t.Errorf("result=%d capture=%d: CaptureBytes = %d, want %d",
					resultBytes, captureBytes, limits.CaptureBytes, wantCapture)
			}
		}
	}
}

// TestToolLimitsCaptureBytesValidation walks the floor from both sides so the
// rejected and accepted values bracket it rather than sampling one of each far
// from the boundary.
func TestToolLimitsCaptureBytesValidation(t *testing.T) {
	t.Parallel()
	for _, value := range []int{-1, 1, minToolResultCaptureBytes - 1} {
		if !invalidLimits(ToolLimits{CaptureBytes: value}) {
			t.Errorf("CaptureBytes = %d was accepted, want rejected", value)
		}
	}
	for _, value := range []int{0, minToolResultCaptureBytes, minToolResultCaptureBytes + 1, DefaultToolResultCaptureBytes} {
		if invalidLimits(ToolLimits{CaptureBytes: value}) {
			t.Errorf("CaptureBytes = %d was rejected, want accepted", value)
		}
	}
}

// TestToolLimitsCaptureBytesOverrideResolution pins that a mode's declared
// ceiling overrides the base one and that leaving it zero inherits, matching
// every other ToolLimits field.
func TestToolLimitsCaptureBytesOverrideResolution(t *testing.T) {
	t.Parallel()
	base := ToolLimits{CaptureBytes: 4096}
	if got := resolveLimits(base, ToolLimits{}).CaptureBytes; got != 4096 {
		t.Errorf("unset override CaptureBytes = %d, want the base 4096", got)
	}
	if got := resolveLimits(base, ToolLimits{CaptureBytes: 8192}).CaptureBytes; got != 8192 {
		t.Errorf("override CaptureBytes = %d, want 8192", got)
	}
}

// TestPolicyRevisionMovesWithCaptureBytes is the Step 3 requirement that the
// ceiling ride the policy digest. It asserts the digest CHANGES between two
// declared ceilings and is stable for a repeated one, so a mutant that dropped
// ToolLimits from the projection cannot pass by returning a constant.
func TestPolicyRevisionMovesWithCaptureBytes(t *testing.T) {
	t.Parallel()
	revision := func(captureBytes int) string {
		return mustDefinition(t, WithToolLimits(ToolLimits{CaptureBytes: captureBytes})).PolicyRevision()
	}
	small, large := revision(4096), revision(8192)
	if small == large {
		t.Fatal("PolicyRevision is identical for two different capture ceilings; the ceiling is not in the digest")
	}
	if again := revision(4096); again != small {
		t.Fatalf("PolicyRevision is unstable for the same ceiling: %q then %q", small, again)
	}
}
