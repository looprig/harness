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
	modes := []Mode{{Name: "plan", Model: model.Model{}, Effort: model.EffortHigh, Tools: modeTools, ToolLimits: ToolLimits{Calls: 7}, Instructions: "plan more"}}
	d := mustDefinition(t, WithToolLimits(ToolLimits{Iterations: 3, Parallel: 2}), WithModes(modes...), WithInitialMode("plan"))
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
	if mode.ToolLimits != (ToolLimits{Iterations: 3, Calls: 7, Parallel: 2}) {
		t.Fatalf("resolved limits = %+v", mode.ToolLimits)
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
