package loopruntime

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

func TestConfigFromBoundUsesEffectivePromptAndEffort(t *testing.T) {
	t.Parallel()
	d, err := loop.Define(
		loop.WithName("agent"), loop.WithInference(&fakeLLM{}, testModel()), loop.WithSystem("base system"),
		loop.WithModes(loop.Mode{Name: "build", Effort: inference.EffortHigh, Instructions: "build instructions"}),
		loop.WithInitialMode("build"),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	bound, err := d.Bind(context.Background(), tool.Bindings{SessionID: sessionID, LoopID: loopID})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	cfg, err := configFromBound(bound, "build")
	if err != nil {
		t.Fatalf("configFromBound: %v", err)
	}
	if cfg.System != bound.EffectiveSystem() {
		t.Errorf("System = %q, effective = %q", cfg.System, bound.EffectiveSystem())
	}
	if cfg.System != "base system\n\nbuild instructions" {
		t.Errorf("System = %q", cfg.System)
	}
	if cfg.Model.Sampling.Effort != inference.EffortHigh {
		t.Errorf("effort = %q, want high", cfg.Model.Sampling.Effort)
	}
}
