package loopruntime

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func TestResolveCapsPreserveUnlimited(t *testing.T) {
	t.Parallel()
	got := resolveToolSetCaps(ToolSet{MaxToolIterations: loop.Unlimited, MaxToolCallsPerTurn: loop.Unlimited, MaxParallelToolCalls: loop.Unlimited})
	if got.MaxToolIterations != loop.Unlimited || got.MaxToolCallsPerTurn != loop.Unlimited {
		t.Fatalf("caps = %d/%d, want Unlimited/Unlimited", got.MaxToolIterations, got.MaxToolCallsPerTurn)
	}
	if got.MaxParallelToolCalls != defaultMaxParallelToolCalls {
		t.Fatalf("MaxParallelToolCalls = %d, want default %d", got.MaxParallelToolCalls, defaultMaxParallelToolCalls)
	}
}

func unlimitedScripts(iterations, callsPerStep int) [][]content.Chunk {
	scripts := make([][]content.Chunk, 0, iterations+1)
	for i := 0; i < iterations; i++ {
		step := make([]content.Chunk, 0, callsPerStep)
		for c := 0; c < callsPerStep; c++ {
			step = append(step, toolUseChunk(c, fmt.Sprintf("id-%d-%d", i, c), "Echo", `{}`))
		}
		scripts = append(scripts, step)
	}
	return append(scripts, []content.Chunk{textChunk("done")})
}

func TestRunTurnUnlimitedRunsPastFormerCaps(t *testing.T) {
	t.Parallel()
	input := []content.Block{&content.TextBlock{Text: "go"}}

	t.Run("both unlimited runs 120 calls", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, loop.Unlimited, loop.Unlimited)
		client := &scriptedLLM{scripts: unlimitedScripts(30, 4)}
		cfg, st, rec := newTurnFixture(input, nil, ts, client, noGateReg())
		terminal := runTurn(context.Background(), cfg, st)
		if _, ok := terminal.(event.TurnDone); !ok {
			t.Fatalf("terminal = %T (%+v), want TurnDone", terminal, terminal)
		}
		if got := echo.runCount(); got != 120 {
			t.Fatalf("echo ran %d times, want 120", got)
		}
		if got := len(rec.commits); got != 31 {
			t.Fatalf("commits = %d, want 31", got)
		}
	})

	t.Run("unlimited iterations still checks calls", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, loop.Unlimited, 5)
		client := &scriptedLLM{scripts: unlimitedScripts(30, 1)}
		cfg, st, _ := newTurnFixture(input, nil, ts, client, noGateReg())
		failed, ok := runTurn(context.Background(), cfg, st).(event.TurnFailed)
		if !ok {
			t.Fatal("terminal is not TurnFailed")
		}
		var limit *event.ToolLimitError
		if !errors.As(failed.Err, &limit) || limit.Calls != 6 || limit.MaxCalls != 5 || limit.MaxIterations != loop.Unlimited {
			t.Fatalf("TurnFailed.Err = %#v, want call cap 6/5 and unlimited iterations", failed.Err)
		}
	})

	t.Run("unlimited calls still checks iterations", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, 2, loop.Unlimited)
		client := &scriptedLLM{scripts: unlimitedScripts(30, 60)}
		cfg, st, _ := newTurnFixture(input, nil, ts, client, noGateReg())
		failed, ok := runTurn(context.Background(), cfg, st).(event.TurnFailed)
		if !ok {
			t.Fatal("terminal is not TurnFailed")
		}
		var limit *event.ToolLimitError
		if !errors.As(failed.Err, &limit) || limit.Iterations != 3 || limit.MaxIterations != 2 || limit.MaxCalls != loop.Unlimited {
			t.Fatalf("TurnFailed.Err = %#v, want iteration cap 3/2 and unlimited calls", failed.Err)
		}
	})
}
