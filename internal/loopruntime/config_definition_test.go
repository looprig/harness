package loopruntime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

func TestConfigFromBoundUsesEffectivePromptAndEffort(t *testing.T) {
	t.Parallel()
	d, err := loop.Define(
		loop.WithName("agent"), loop.WithInference(&fakeLLM{}, testModel()), loop.WithSystem("base system"),
		loop.WithModes(loop.Mode{Name: "build", Effort: testEffortHigh, Instructions: "build instructions"}),
		loop.WithInitialMode("build"),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("new session id: %v", err)
	}
	loopID, err := uuid.New()
	if err != nil {
		t.Fatalf("new loop id: %v", err)
	}
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
	if cfg.Model.Sampling.Effort != testEffortHigh {
		t.Errorf("effort = %q, want high", cfg.Model.Sampling.Effort)
	}
}

func TestConfigFromBoundClonesOutputPolicy(t *testing.T) {
	t.Parallel()
	output := testLoopOutput()
	d, err := loop.Define(
		loop.WithName("agent"), loop.WithInference(&fakeLLM{}, outputModel(model.Capabilities{StructuredOutput: true})),
		loop.WithOutputSchema(output),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("new session id: %v", err)
	}
	loopID, err := uuid.New()
	if err != nil {
		t.Fatalf("new loop id: %v", err)
	}
	bound, err := d.Bind(context.Background(), tool.Bindings{SessionID: sessionID, LoopID: loopID})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	cfg, err := configFromBound(bound, "")
	if err != nil {
		t.Fatalf("configFromBound: %v", err)
	}
	if cfg.Output == nil || !json.Valid(cfg.Output.Schema) {
		t.Fatalf("runtime output = %#v", cfg.Output)
	}
	first := cfg.Output.Clone()
	fromBound, configured := bound.OutputSchema()
	if !configured {
		t.Fatal("bound output is absent")
	}
	fromBound.Schema[0] = '['
	if !reflect.DeepEqual(*cfg.Output, first) {
		t.Fatal("runtime output aliases bound accessor")
	}
}
