package loopruntime

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/tool"
)

// A call denied because the loop has NO access gate logs one warning that
// names the fix, once per process, while every such call still fails closed.
// Not parallel: it swaps the process-wide default logger and resets the
// process-wide once flag (parallel tests only start after this returns).
func TestRunBatch_NilAccessGateWarnsOnceNamingTheFix(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	accessGateMissingWarned.Store(false)

	tl := &fakeRunTool{name: "Glob", output: "ok"}
	ts := ToolSet{Registry: []tool.InvokableTool{tl}, MaxParallelToolCalls: 2}
	emit, _ := collectEmit()
	for range 2 {
		results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "Glob", `{}`), call(t, "Glob", `{}`)}, ts, emit)
		for _, r := range results {
			if got, want := resultText(r), "error: permission denied [unavailable]"; got != want {
				t.Fatalf("result = %q, want %q", got, want)
			}
		}
	}
	if tl.totalRuns != 0 {
		t.Fatalf("totalRuns = %d, want 0 without an access gate", tl.totalRuns)
	}

	out := logs.String()
	if got := strings.Count(out, "no access gate"); got != 1 {
		t.Fatalf("warning logged %d times, want exactly once:\n%s", got, out)
	}
	for _, want := range []string{"level=WARN", "tool=Glob", "loop.WithReadOnlyAccess", "loop.WithAccessGate"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning %q does not contain %q", out, want)
		}
	}
}
