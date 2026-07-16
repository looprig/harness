package loopruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

type runtimeContextCounter struct {
	capability contextcount.CounterCapability
}

func (*runtimeContextCounter) CountContext(context.Context, inference.Request) (contextcount.ContextCount, error) {
	panic("unused")
}
func (c *runtimeContextCounter) CounterCapability() contextcount.CounterCapability {
	return c.capability
}

func contextBoundDefinition(t *testing.T, client inference.Client) loop.BoundDefinition {
	t.Helper()
	counter := &runtimeContextCounter{capability: contextcount.CounterCapability{Transport: contextcount.CounterTransportLocal, Retention: contextcount.RetentionNone, TokenizerRev: "v1", Quality: contextcount.CountQualityExactLocal}}
	definition, err := loop.Define(
		loop.WithName("agent"), loop.WithInference(client, testModel()), loop.WithContextCounter(counter),
		loop.WithInferenceCapability(contextcount.InferenceCapability{Transport: contextcount.InferenceTransportLocal, Retention: contextcount.RetentionNone}),
		loop.WithCompaction(loop.CompactionPolicy{ReservedOutput: 10, MaxSummaryTokens: 5, CountTimeout: 37*time.Millisecond + time.Nanosecond, Hustle: "context.compact"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: mustID(t), LoopID: mustID(t)})
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func TestConfigFromBoundCopiesContextConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "configured context collaborators"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bound := contextBoundDefinition(t, &fakeLLM{})
			first, err := configFromBound(bound, "")
			if err != nil {
				t.Fatal(err)
			}
			if first.ContextCounter == nil || first.CounterCapability.TokenizerRev != "v1" || first.Compaction == nil {
				t.Fatalf("context config = %#v", first)
			}
			if first.Compaction.CountTimeout != 37*time.Millisecond+time.Nanosecond {
				t.Fatalf("timeout = %v", first.Compaction.CountTimeout)
			}
			first.Compaction.ReservedOutput = 99
			second, err := configFromBound(bound, "")
			if err != nil {
				t.Fatal(err)
			}
			if second.Compaction.ReservedOutput != 10 {
				t.Fatalf("policy aliased prior config: %d", second.Compaction.ReservedOutput)
			}
		})
	}
}

func TestConfigFromBoundCopiesObservationConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "exact observation timeout and independent policy copy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			counter := &runtimeContextCounter{capability: contextcount.CounterCapability{Transport: contextcount.CounterTransportLocal, Retention: contextcount.RetentionNone, TokenizerRev: "v1", Quality: contextcount.CountQualityExactLocal}}
			policy := loop.ContextObservationPolicy{ReservedOutput: 10, CountTimeout: 41*time.Millisecond + time.Nanosecond}
			definition, err := loop.Define(
				loop.WithName("agent"), loop.WithInference(&fakeLLM{}, testModel()), loop.WithContextCounter(counter),
				loop.WithInferenceCapability(contextcount.InferenceCapability{Transport: contextcount.InferenceTransportLocal, Retention: contextcount.RetentionNone}),
				loop.WithContextObservation(policy),
			)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: mustID(t), LoopID: mustID(t)})
			if err != nil {
				t.Fatal(err)
			}
			first, err := configFromBound(bound, "")
			if err != nil {
				t.Fatal(err)
			}
			if first.ContextObservation == nil || *first.ContextObservation != policy || first.Compaction != nil {
				t.Fatalf("observation config = %#v, want exact policy and no compaction", first.ContextObservation)
			}
			first.ContextObservation.ReservedOutput = 99
			second, err := configFromBound(bound, "")
			if err != nil {
				t.Fatal(err)
			}
			if second.ContextObservation == nil || *second.ContextObservation != policy {
				t.Fatalf("observation policy aliased prior config: %#v", second.ContextObservation)
			}
		})
	}
}

func TestChangeInferenceRejectsContextTransportSwap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*model.Model)
	}{
		{name: "provider", mutate: func(model *model.Model) { model.Provider = "other" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
			bound := contextBoundDefinition(t, llm)
			l, rec := newBoundLoop(t, llm, bound)
			candidate := testModel()
			tt.mutate(&candidate)
			res := sendChange(t, l, command.ChangeLoopInference{Model: candidate, SetModel: true})
			var changeErr *loop.ChangeError
			var bindingErr *loop.ContextTransportBindingError
			if !errors.As(res.Err, &changeErr) || changeErr.Kind != loop.ChangeInvalidModel || !errors.As(res.Err, &bindingErr) {
				t.Fatalf("error = %T %v", res.Err, res.Err)
			}
			if countInferenceChanged(rec.events()) != 0 {
				t.Fatal("rejected transport swap emitted lifecycle event")
			}
		})
	}
}
