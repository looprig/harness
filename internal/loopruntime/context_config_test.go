package loopruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

type runtimeContextCounter struct{ capability inference.CounterCapability }

func (*runtimeContextCounter) CountContext(context.Context, inference.Request) (inference.ContextCount, error) {
	panic("unused")
}
func (c *runtimeContextCounter) CounterCapability() inference.CounterCapability { return c.capability }

func contextBoundDefinition(t *testing.T, client inference.Client) loop.BoundDefinition {
	t.Helper()
	counter := &runtimeContextCounter{capability: inference.CounterCapability{Transport: inference.CounterTransportLocal, Retention: inference.RetentionNone, TokenizerRev: "v1", Quality: inference.CountQualityExactLocal}}
	definition, err := loop.Define(
		loop.WithName("agent"), loop.WithInference(client, testModel()), loop.WithContextCounter(counter),
		loop.WithInferenceCapability(inference.InferenceCapability{Transport: inference.InferenceTransportLocal, Retention: inference.RetentionNone}),
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

func TestChangeInferenceRejectsContextTransportSwap(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := contextBoundDefinition(t, llm)
	l, rec := newBoundLoop(t, llm, bound)
	candidate := testModel()
	candidate.Provider = "other"
	res := sendChange(t, l, command.ChangeLoopInference{Model: candidate, SetModel: true})
	var changeErr *loop.ChangeError
	var bindingErr *loop.ContextTransportBindingError
	if !errors.As(res.Err, &changeErr) || changeErr.Kind != loop.ChangeInvalidModel || !errors.As(res.Err, &bindingErr) {
		t.Fatalf("error = %T %v", res.Err, res.Err)
	}
	if countInferenceChanged(rec.events()) != 0 {
		t.Fatal("rejected transport swap emitted lifecycle event")
	}
}

var _ event.Event
