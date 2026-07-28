package loopruntime

import (
	"context"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
)

func TestStepHooksWrapInferenceAndDurableCommit(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var order []string
	appendOrder := func(value string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, value)
	}
	runner, err := hook.Compile(hook.Set{Around: []hook.Around{
		{Operation: hook.OperationStep, Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
			if call.Step.Index != 0 || call.Coordinates.StepID.IsZero() {
				t.Errorf("step call = %#v, want frozen index and id", call)
			}
			appendOrder("step.begin")
			return ctx, func(result hook.Result) {
				appendOrder("step.finish")
				if result.Outcome != hook.OutcomeCompleted {
					t.Errorf("step outcome = %v, want completed", result.Outcome)
				}
			}
		}},
		{Operation: hook.OperationInference, Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			appendOrder("inference.begin")
			return ctx, func(hook.Result) { appendOrder("inference.finish") }
		}},
	}})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	cfg, state, recorder := newTurnFixture(
		[]content.Block{&content.TextBlock{Text: "go"}}, nil, ToolSet{},
		&fakeLLM{chunks: []content.Chunk{textChunk("done")}}, nil,
	)
	cfg.hooks = runner
	originalCommit := recorder.commit
	cfg.commit = func(ctx context.Context, commit turnCommit) error {
		err := originalCommit(ctx, commit)
		appendOrder("step.commit")
		return err
	}

	terminal := runTurn(context.Background(), cfg, state)

	if _, ok := terminal.(event.TurnDone); !ok {
		t.Fatalf("terminal = %T, want TurnDone", terminal)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"step.begin", "inference.begin", "inference.finish", "step.commit", "step.finish"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestStepHooksDiscardedInferenceFinishesFailed(t *testing.T) {
	t.Parallel()
	var finishes []hook.Result
	runner, err := hook.Compile(hook.Set{Around: []hook.Around{{
		Operation: hook.OperationStep,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, func(result hook.Result) { finishes = append(finishes, result) }
		},
	}}})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	cfg, state, _ := newTurnFixture(
		[]content.Block{&content.TextBlock{Text: "go"}}, nil, ToolSet{}, &fakeLLM{}, nil,
	)
	cfg.hooks = runner

	terminal := runTurn(context.Background(), cfg, state)

	if _, ok := terminal.(event.TurnFailed); !ok {
		t.Fatalf("terminal = %T, want TurnFailed", terminal)
	}
	if len(finishes) != 1 || finishes[0].Outcome != hook.OutcomeFailed {
		t.Fatalf("finishes = %#v, want one failed step", finishes)
	}
}
