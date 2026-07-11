package loop

import (
	"context"
	"testing"

	"github.com/looprig/inference"
)

func TestConfigFromBoundCombinesModeInstructionsAndEffort(t *testing.T) {
	t.Parallel()
	d, err := Define(
		WithName("agent"), WithInference(&fakeLLM{}, modelWithEffort(inference.EffortLow)), WithSystem("base system"),
		WithModes(Mode{Name: "build", Effort: inference.EffortHigh, Instructions: "build instructions"}),
		WithInitialMode("build"),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	bound, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	cfg, err := configFromBound(bound, "build")
	if err != nil {
		t.Fatalf("configFromBound: %v", err)
	}
	if cfg.System != "base system\n\nbuild instructions" {
		t.Errorf("System = %q", cfg.System)
	}
	if cfg.Model.Sampling.Effort != inference.EffortHigh {
		t.Errorf("model effort = %q, want high", cfg.Model.Sampling.Effort)
	}
}
