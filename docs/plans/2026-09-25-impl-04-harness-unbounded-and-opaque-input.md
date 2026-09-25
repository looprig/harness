# Harness: explicit unbounded execution, zero hustle timeout, opaque tool input — implementation plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Land items 1–3 of the approved design
`2026-09-25-unbounded-execution-and-opaque-tool-input-design.md` on harness `main`:
`loop.Unlimited` for per-turn tool caps and the session spawn quota, a zero hustle timeout
meaning "no execution deadline", and a duplicate-key check that treats a `tool_use`
block's `Input` as opaque. With these, Oxy can drop fork patches 1, 2 and 5.

**Architecture:** Each change is opt-in or a bug fix, so default behaviour does not change.
- `-1` (`loop.Unlimited`) becomes the one accepted negative value for
  `ToolLimits.Iterations`/`Calls` and `rig.DelegationLimits.Quota`. The runtime keeps the
  sentinel and skips only the matching comparison.
- A zero hustle timeout skips `context.WithTimeout`. Caller and session cancellation still
  apply.
- `pkg/event`'s duplicate-key walker tracks the JSON path. On a message-block path it
  holds a block's `input` back and skips the check only when the block's `type` is
  `tool_use`.

**Tech Stack:** Go 1.26.8, stdlib only, no new dependencies. Verify everything with
`GOWORK=off GOTOOLCHAIN=go1.26.8`.

**Reference implementation:** the Oxy fork
`/Users/ipotter/code/oxy/.worktrees/looprig-current/third_party/looprig-harness`
(`OXY-PATCHES.md`, patches 1, 2 and 5), rebased on harness v0.40.2. Its hunks were
extracted with `git -C harness archive v0.40.2`, re-applied to current `main`
(`b9797b96`, v0.40.2 + docs), and every test below was run red, then green, in a scratch
copy. Four deliberate differences from the fork:

1. **An omitted `hustle.WithTimeout` still fails `Define`.** The fork's `<= 0 → < 0`
   change alone would turn a *forgotten* option into an unbounded hustle. Only an
   explicit `WithTimeout(0)` means "no deadline". This keeps today's behaviour for
   omission.
2. The turn cap check is factored into `toolCapExceeded`, and the `pkg/loop` import sits
   in the harness import group, not the stdlib group.
3. The opaque-input walker computes `messageBlockPath(path)` once per object and only
   records `type` on a block path. It behaves the same as the fork.
4. The fork's tests are rewritten against public or runtime behaviour. They show that
   caps and quota are really lifted: 120 tool calls in one turn, and 65 spawns. They also
   show that the other cap still fires, and that depth stays bounded.

**Not in scope:** `session.HustleHost`, the thinking-tolerant legacy extractor (Oxy patches
3 and 4; Oxy drops them), inference's `WithoutExecutionTimeout` (impl-03), and anything in
the principal/metadata/presenter plan (impl-05).

**Release:** none in this plan. Do **not** tag or push a tag. These commits land on harness
`main` first, and **harness v0.41.0 is released by impl-05's final task**, which ships them
with the principal/metadata/presenter work. Tasks here have no dependency on impl-05,
core v0.12.0, sessionstore v0.14.0 or inference v0.14.0, and need no `go.mod` change.
Push `main` only with owner confirmation (master plan rules).

---

## Conventions

- Repository: `/Users/ipotter/code/looprig/harness`, local `main`. Preserve other
  branches and worktrees. All commands run from the repository root.
- Every test command uses `GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race`.
- Commits use conventional messages with **no `Co-Authored-By` trailer**. Commit only the
  files listed in the task.
- Hunks are unified diffs against `main` at `b9797b96`. Line numbers may drift; match on
  context.
- Run `gofmt -l` through the 1.26.8 toolchain (`GOTOOLCHAIN=go1.26.8 go fmt ./...` or
  `make fmt-check`). An older host `gofmt` flags `pkg/hub/durable_tap_test.go`
  spuriously.

## Task 0: Baseline

**Step 1:** Confirm the starting point.

```sh
git status --short        # only untracked docs/plans files expected
git log --oneline -1      # b9797b96 or a descendant; note it
GOWORK=off GOTOOLCHAIN=go1.26.8 go build ./...
```

Expected: the build succeeds. If `main` has moved past `b9797b96`, re-read the hunk
contexts before applying them.

---

## Task 1: `loop.Unlimited` in `pkg/loop` (definition and mode resolution)

**Files:**
- Modify: `pkg/loop/mode.go`
- Modify: `pkg/loop/definition_test.go` (the `-1` row moves to `-2`)
- Create: `pkg/loop/unlimited_test.go`

**Step 1: Write the failing test.** Create `pkg/loop/unlimited_test.go`:

```go
package loop

import (
	"context"
	"errors"
	"testing"
)

// TestUnlimitedToolLimitsSurviveDefine proves an explicit loop.Unlimited on the
// base definition survives Define's defaulting and Bind's resolution, while zero
// still means the package default and the other fields keep their semantics.
func TestUnlimitedToolLimitsSurviveDefine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   ToolLimits
		want ToolLimits
	}{
		{name: "both unlimited", in: ToolLimits{Iterations: Unlimited, Calls: Unlimited}, want: ToolLimits{Iterations: Unlimited, Calls: Unlimited, Parallel: 8, CaptureBytes: DefaultToolResultCaptureBytes}},
		{name: "iterations unlimited only", in: ToolLimits{Iterations: Unlimited}, want: ToolLimits{Iterations: Unlimited, Calls: 100, Parallel: 8, CaptureBytes: DefaultToolResultCaptureBytes}},
		{name: "calls unlimited only", in: ToolLimits{Calls: Unlimited, Iterations: 3}, want: ToolLimits{Iterations: 3, Calls: Unlimited, Parallel: 8, CaptureBytes: DefaultToolResultCaptureBytes}},
		{name: "zero keeps defaults", in: ToolLimits{}, want: ToolLimits{Iterations: 25, Calls: 100, Parallel: 8, CaptureBytes: DefaultToolResultCaptureBytes}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := mustDefinition(t, WithToolLimits(tt.in))
			b, err := d.Bind(context.Background(), validToolBindings(t))
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			if got := b.ToolLimits(); got != tt.want {
				t.Fatalf("ToolLimits = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestModeOverrideToUnlimited proves a mode may lift a finite base cap to
// Unlimited, and that a zero mode field still inherits the base value.
func TestModeOverrideToUnlimited(t *testing.T) {
	t.Parallel()
	modes := []Mode{{Name: "long", ToolLimits: ToolLimits{Iterations: Unlimited, Calls: Unlimited}}, {Name: "inherit"}}
	d := mustDefinition(t, WithToolLimits(ToolLimits{Iterations: 3, Calls: 7}), WithModes(modes...), WithInitialMode("long"))
	b, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	long, ok := b.Mode("long")
	if !ok {
		t.Fatal("long mode missing")
	}
	if long.ToolLimits.Iterations != Unlimited || long.ToolLimits.Calls != Unlimited {
		t.Fatalf("long mode limits = %+v, want Unlimited iterations and calls", long.ToolLimits)
	}
	inherit, ok := b.Mode("inherit")
	if !ok {
		t.Fatal("inherit mode missing")
	}
	if inherit.ToolLimits.Iterations != 3 || inherit.ToolLimits.Calls != 7 {
		t.Fatalf("inherit mode limits = %+v, want base 3/7", inherit.ToolLimits)
	}
}

// TestBelowUnlimitedRefused proves -1 is the only accepted negative value for
// Iterations and Calls, and that Parallel has no unlimited value.
func TestBelowUnlimitedRefused(t *testing.T) {
	t.Parallel()
	for _, limits := range []ToolLimits{{Iterations: -2}, {Calls: -2}, {Parallel: Unlimited}} {
		_, err := Define(WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithToolLimits(limits))
		var de *DefinitionError
		if !errors.As(err, &de) || de.Kind != DefinitionInvalidToolLimits {
			t.Fatalf("Define(%+v) error = %v, want DefinitionInvalidToolLimits", limits, err)
		}
		_, err = Define(WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithModes(Mode{Name: "m", ToolLimits: limits}), WithInitialMode("m"))
		if !errors.As(err, &de) || de.Kind != DefinitionInvalidMode {
			t.Fatalf("Define(mode %+v) error = %v, want DefinitionInvalidMode", limits, err)
		}
	}
}
```

**Step 2: Run it and watch it fail.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./pkg/loop/ -run 'TestUnlimitedToolLimitsSurviveDefine|TestModeOverrideToUnlimited|TestBelowUnlimitedRefused'
```

Expected: build failure, `undefined: Unlimited`.

**Step 3: Implement.** Apply to `pkg/loop/mode.go`:

```diff
--- a/pkg/loop/mode.go
+++ b/pkg/loop/mode.go
@@ -41,7 +41,18 @@
 // ModeName identifies a predeclared loop mode. The empty name identifies the base mode.
 type ModeName string
 
-// ToolLimits bounds tool activity during one turn.
+// Unlimited, set on ToolLimits.Iterations or ToolLimits.Calls, disables that
+// per-turn cap. Zero still means the package default (25 iterations, 100 calls)
+// and any value below Unlimited is invalid. Parallel, ResultBytes and
+// CaptureBytes have no unlimited value.
+//
+// An unlimited loop stops only when the model ends the turn, or on interrupt,
+// shutdown or context cancellation: nothing else bounds a model that keeps
+// calling tools. Choose it deliberately.
+const Unlimited = -1
+
+// ToolLimits bounds tool activity during one turn. Iterations and Calls accept
+// Unlimited; see its documentation.
 type ToolLimits struct {
 	Iterations  int
 	Calls       int
@@ -115,10 +126,10 @@
 
 func resolveLimits(base, override ToolLimits) ToolLimits {
 	result := base
-	if override.Iterations > 0 {
+	if override.Iterations > 0 || override.Iterations == Unlimited {
 		result.Iterations = override.Iterations
 	}
-	if override.Calls > 0 {
+	if override.Calls > 0 || override.Calls == Unlimited {
 		result.Calls = override.Calls
 	}
 	if override.Parallel > 0 {
@@ -150,7 +161,7 @@
 }
 
 func invalidLimits(limits ToolLimits) bool {
-	return limits.Iterations < 0 || limits.Calls < 0 || limits.Parallel < 0 ||
+	return limits.Iterations < Unlimited || limits.Calls < Unlimited || limits.Parallel < 0 ||
 		limits.ResultBytes < 0 || (limits.ResultBytes > 0 && limits.ResultBytes < minToolResultBytes) ||
 		limits.CaptureBytes < 0 || (limits.CaptureBytes > 0 && limits.CaptureBytes < minToolResultCaptureBytes)
 }
```

`defaultLimits` needs no change, because it fills only `== 0`. `Define` (`invalidLimits`
at `definition.go` ~115) and mode validation (~1215) already route through
`invalidLimits`, and `Bind` routes through `resolveLimits` (~738).

**Step 4: Run the new tests, then the package.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./pkg/loop/ -run 'TestUnlimitedToolLimitsSurviveDefine|TestModeOverrideToUnlimited|TestBelowUnlimitedRefused'
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./pkg/loop/
```

Expected: the new tests PASS. The package run FAILS exactly one existing row:
`TestDefineValidation/negative_limits: Define() error = <nil> <nil>, want *DefinitionError kind "invalid_tool_limits"`.
`Calls: -1` is now `Unlimited`.

**Step 5: Move the upstream row to `-2`.** In `pkg/loop/definition_test.go`:

```diff
--- a/pkg/loop/definition_test.go
+++ b/pkg/loop/definition_test.go
@@ -34,7 +34,7 @@
 		{name: "invalid model", opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, model.Model{})}, kind: DefinitionInvalidModel},
 		{name: "nil option", opts: []Option{WithName("agent"), nil, WithInference(&fakeLLM{}, testModel())}, kind: DefinitionNilOption},
 		{name: "duplicate name", opts: []Option{WithName("a"), WithName("b"), WithInference(&fakeLLM{}, testModel())}, kind: DefinitionDuplicateOption},
-		{name: "negative limits", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithToolLimits(ToolLimits{Calls: -1})}, kind: DefinitionInvalidToolLimits},
+		{name: "negative limits", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithToolLimits(ToolLimits{Calls: -2})}, kind: DefinitionInvalidToolLimits},
 		{name: "negative result bytes", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithToolLimits(ToolLimits{ResultBytes: -1})}, kind: DefinitionInvalidToolLimits},
 		{name: "result bytes below minimum", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithToolLimits(ToolLimits{ResultBytes: minToolResultBytes - 1})}, kind: DefinitionInvalidToolLimits},
 		{name: "negative drain", opts: []Option{WithName("a"), WithInference(&fakeLLM{}, testModel()), WithDrainTimeout(-time.Second)}, kind: DefinitionInvalidDrainTimeout},
```

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./pkg/loop/
```

Expected: `ok  github.com/looprig/harness/pkg/loop`.

**Step 6: Commit.**

```sh
git add pkg/loop/mode.go pkg/loop/definition_test.go pkg/loop/unlimited_test.go
git commit -m "feat(loop): add loop.Unlimited for per-turn tool iteration and call caps"
```

---

## Task 2: Loop runtime keeps `Unlimited` and skips only that comparison

**Files:**
- Modify: `internal/loopruntime/toolset.go`, `internal/loopruntime/turn.go`
- Modify: `internal/loopruntime/deps_test.go` (the three `-1` rows move to `-2`; unlimited rows are added)
- Create: `internal/loopruntime/unlimited_test.go`

**Step 1: Write the failing test.** Create `internal/loopruntime/unlimited_test.go`. It
reuses `agenticToolSet`, `scriptedLLM`, `echoTool`, `toolUseChunk`, `textChunk`,
`newTurnFixture` and `noGateReg` from `turn_test.go`/`fake_test.go`. `scriptedLLM` repeats
its last script, so the text-only final script ends the turn.

```go
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

// TestResolveCapsPreserveUnlimited proves loop.Unlimited survives the actor's cap
// resolution, while zero keeps the defaults and Parallel has no unlimited value.
func TestResolveCapsPreserveUnlimited(t *testing.T) {
	t.Parallel()
	got := resolveToolSetCaps(ToolSet{MaxToolIterations: loop.Unlimited, MaxToolCallsPerTurn: loop.Unlimited, MaxParallelToolCalls: loop.Unlimited})
	if got.MaxToolIterations != loop.Unlimited || got.MaxToolCallsPerTurn != loop.Unlimited {
		t.Fatalf("caps = %d/%d, want Unlimited/Unlimited", got.MaxToolIterations, got.MaxToolCallsPerTurn)
	}
	if got.MaxParallelToolCalls != defaultMaxParallelToolCalls {
		t.Fatalf("MaxParallelToolCalls = %d, want default %d (no unlimited parallelism)", got.MaxParallelToolCalls, defaultMaxParallelToolCalls)
	}
}

// unlimitedScripts returns iterations tool-call steps of callsPerStep calls each,
// with unique tool-use ids, followed by a text-only step that ends the turn.
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

// TestRunTurnUnlimitedRunsPastFormerCaps proves an Unlimited cap is never
// compared: a turn runs past the former default of 25 iterations and 100 calls
// and ends normally, while the other, finite cap still fires.
func TestRunTurnUnlimitedRunsPastFormerCaps(t *testing.T) {
	t.Parallel()
	input := []content.Block{&content.TextBlock{Text: "go"}}

	t.Run("both unlimited: 30 iterations x 4 calls ends in TurnDone", func(t *testing.T) {
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
			t.Fatalf("echo ran %d times, want 120 (past the former 100-call cap)", got)
		}
		if got := len(rec.commits); got != 31 {
			t.Fatalf("commits = %d, want 31 (30 tool steps + final text step)", got)
		}
	})

	t.Run("unlimited iterations, finite calls: call cap still fires", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, loop.Unlimited, 5)
		client := &scriptedLLM{scripts: unlimitedScripts(30, 1)}
		cfg, st, _ := newTurnFixture(input, nil, ts, client, noGateReg())
		failed, ok := runTurn(context.Background(), cfg, st).(event.TurnFailed)
		if !ok {
			t.Fatal("terminal is not TurnFailed")
		}
		var tle *event.ToolLimitError
		if !errors.As(failed.Err, &tle) || tle.Calls != 6 || tle.MaxCalls != 5 || tle.MaxIterations != loop.Unlimited {
			t.Fatalf("TurnFailed.Err = %#v, want call cap 6/5 with MaxIterations Unlimited", failed.Err)
		}
	})

	t.Run("unlimited calls, finite iterations: iteration cap still fires", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, 2, loop.Unlimited)
		client := &scriptedLLM{scripts: unlimitedScripts(30, 60)}
		cfg, st, _ := newTurnFixture(input, nil, ts, client, noGateReg())
		failed, ok := runTurn(context.Background(), cfg, st).(event.TurnFailed)
		if !ok {
			t.Fatal("terminal is not TurnFailed")
		}
		var tle *event.ToolLimitError
		if !errors.As(failed.Err, &tle) || tle.Iterations != 3 || tle.MaxIterations != 2 || tle.MaxCalls != loop.Unlimited {
			t.Fatalf("TurnFailed.Err = %#v, want iteration cap 3/2 with MaxCalls Unlimited", failed.Err)
		}
	})
}
```

**Step 2: Run it and watch it fail.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./internal/loopruntime/ -run 'TestResolveCapsPreserveUnlimited|TestRunTurnUnlimitedRunsPastFormerCaps'
```

Expected: FAIL.
- `caps = 25/100, want Unlimited/Unlimited`.
- The both-unlimited subtest ends in `TurnFailed` with
  `tool limit reached: 26/25 steps, 104/100 calls`.
- The two mixed subtests report the default 25/100 in place of `-1`.

**Step 3: Implement.** Apply:

```diff
--- a/internal/loopruntime/toolset.go
+++ b/internal/loopruntime/toolset.go
@@ -62,15 +62,17 @@
 // per definition.
 const defaultMaxMaterializedToolResultBytes = loop.DefaultMaterializedToolResultBytes
 
+// resolveMaxToolIterations and resolveMaxToolCallsPerTurn default a zero or
+// invalid negative cap but keep loop.Unlimited, which runTurn never compares.
 func resolveMaxToolIterations(n int) int {
-	if n <= 0 {
+	if n <= 0 && n != loop.Unlimited {
 		return defaultMaxToolIterations
 	}
 	return n
 }
 
 func resolveMaxToolCallsPerTurn(n int) int {
-	if n <= 0 {
+	if n <= 0 && n != loop.Unlimited {
 		return defaultMaxToolCallsPerTurn
 	}
 	return n
```

```diff
--- a/internal/loopruntime/turn.go
+++ b/internal/loopruntime/turn.go
@@ -13,6 +13,7 @@
 	"github.com/looprig/harness/pkg/event"
 	"github.com/looprig/harness/pkg/hook"
 	identitydomain "github.com/looprig/harness/pkg/identity"
+	"github.com/looprig/harness/pkg/loop"
 	"github.com/looprig/harness/pkg/tool"
 	"github.com/looprig/inference"
 	model "github.com/looprig/inference/model"
@@ -517,7 +518,7 @@
 
 		ts.toolIterations++
 		ts.toolCalls += len(toolUses)
-		if ts.toolIterations > cfg.tools.MaxToolIterations || ts.toolCalls > cfg.tools.MaxToolCallsPerTurn {
+		if toolCapExceeded(ts.toolIterations, cfg.tools.MaxToolIterations) || toolCapExceeded(ts.toolCalls, cfg.tools.MaxToolCallsPerTurn) {
 			// The runaway cap fires on this UNCOMPLETED tool step: it is never appended
 			// to ts.msgs and never committed, so no unpaired tool_use survives into
 			// loopState.msgs and no StepDone is emitted for it.
@@ -1213,3 +1214,9 @@
 	}
 	return defs
 }
+
+// toolCapExceeded reports whether count has passed a per-turn tool cap. A
+// loop.Unlimited cap is never exceeded.
+func toolCapExceeded(count, limit int) bool {
+	return limit != loop.Unlimited && count > limit
+}
```

`resolveToolSetCaps` is the only caller of both resolvers, at `loop.go:453` (New) and
`loop.go:2429` (SetMode). `config.go:60` copies `ToolLimits.Iterations`/`Calls` straight
into the `ToolSet`. So the sentinel reaches `runTurn` on construction and on a mode switch.
`MaxParallelToolCalls` keeps `n <= 0 → default`.

**Step 4: Run the package.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./internal/loopruntime/
```

Expected: the new tests PASS, and exactly these existing rows FAIL because `-1` is now
preserved:
- `TestResolveMaxToolIterations/negative_defaults`
  (`resolveMaxToolIterations(-1) = -1, want 25`)
- `TestResolveMaxToolCallsPerTurn/negative_defaults`
- `TestResolveToolSetCaps/negative_treated_as_unset`
  (`MaxToolIterations = -1, want 25`, and the same for calls)

**Step 5: Move those rows to `-2` and add unlimited rows.** In
`internal/loopruntime/deps_test.go`:

```diff
--- a/internal/loopruntime/deps_test.go
+++ b/internal/loopruntime/deps_test.go
@@ -8,6 +8,7 @@
 	"github.com/looprig/core/content"
 	"github.com/looprig/core/uuid"
 	"github.com/looprig/harness/pkg/event"
+	"github.com/looprig/harness/pkg/loop"
 	"github.com/looprig/harness/pkg/tool"
 )
 
@@ -22,7 +23,8 @@
 		want int
 	}{
 		{"zero defaults", 0, defaultMaxToolIterations},
-		{"negative defaults", -1, defaultMaxToolIterations},
+		{"invalid negative defaults", -2, defaultMaxToolIterations},
+		{"unlimited preserved", loop.Unlimited, loop.Unlimited},
 		{"positive preserved", 7, 7},
 	}
 	for _, tt := range tests {
@@ -44,7 +46,8 @@
 		want int
 	}{
 		{"zero defaults", 0, defaultMaxToolCallsPerTurn},
-		{"negative defaults", -1, defaultMaxToolCallsPerTurn},
+		{"invalid negative defaults", -2, defaultMaxToolCallsPerTurn},
+		{"unlimited preserved", loop.Unlimited, loop.Unlimited},
 		{"positive preserved", 42, 42},
 	}
 	for _, tt := range tests {
@@ -129,8 +132,9 @@
 		{
 			name: "negative treated as unset",
 			in: ToolSet{
-				MaxToolIterations:    -1,
-				MaxToolCallsPerTurn:  -1,
+				// -1 is loop.Unlimited for iterations and calls; -2 is the first invalid value.
+				MaxToolIterations:    -2,
+				MaxToolCallsPerTurn:  -2,
 				MaxParallelToolCalls: -1,
 			},
 			want: ToolSet{
```

The parallel-calls row keeps `-1`, because `Parallel` has no unlimited value.

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./internal/loopruntime/
```

Expected: `ok  github.com/looprig/harness/internal/loopruntime`. The package takes about
40 s, and its ERROR/WARN log lines are pre-existing test noise.

**Step 6: Commit.**

```sh
git add internal/loopruntime/toolset.go internal/loopruntime/turn.go internal/loopruntime/deps_test.go internal/loopruntime/unlimited_test.go
git commit -m "feat(loopruntime): honour loop.Unlimited tool iteration and call caps"
```

---

## Task 3: Unlimited cumulative spawn quota (`rig.DelegationLimits.Quota`)

**Files:**
- Modify: `internal/sessionruntime/limits.go`, `internal/sessionruntime/session.go`
- Modify: `pkg/rig/options.go`
- Modify: `pkg/rig/rig_test.go` (the negative-quota row moves to `-2`; a new test is added)
- Create: `internal/sessionruntime/unlimited_quota_test.go`

**Step 1: Write the failing tests.** Create
`internal/sessionruntime/unlimited_quota_test.go`. It reuses `newTestSession`, `cfg`,
`stubLLM`, `textChunk` and `readSpawned` from `quota_cap_test.go`. With `Depth: 2`, a child
of the primary is allowed and a grandchild is refused. The test drives this through
`depthUnderLock`.

```go
package sessionruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
)

// TestUnlimitedQuotaPreservedByDefaults proves loop.Unlimited survives
// withDefaults while Depth stays bounded and invalid negatives still default.
func TestUnlimitedQuotaPreservedByDefaults(t *testing.T) {
	t.Parallel()
	if got := (Limits{Depth: 2, Quota: loop.Unlimited}).withDefaults(); got != (Limits{Depth: 2, Quota: loop.Unlimited}) {
		t.Fatalf("withDefaults() = %+v, want Depth 2, Quota Unlimited", got)
	}
	if got := (Limits{Depth: loop.Unlimited, Quota: loop.Unlimited}).withDefaults(); got.Depth != defaultDepth {
		t.Fatalf("Depth = %d, want default %d (depth has no unlimited value)", got.Depth, defaultDepth)
	}
	if got := (Limits{Quota: -2}).withDefaults(); got.Quota != defaultQuota {
		t.Fatalf("Quota = %d, want default %d for an invalid negative", got.Quota, defaultQuota)
	}
}

// TestNewLoopUnlimitedQuotaExceedsFormerLifetimeQuota proves an Unlimited quota
// admits more spawns than the former default lifetime quota, while the depth cap
// still refuses a too-deep spawn.
func TestNewLoopUnlimitedQuotaExceedsFormerLifetimeQuota(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(),
		cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}),
		WithLimits(Limits{Depth: 2, Quota: loop.Unlimited}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	var child loop.Provenance
	for i := 0; i < defaultQuota+1; i++ {
		id, err := s.NewLoop(loop.Provenance{LoopID: s.ActiveLoopID()}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}))
		if err != nil {
			t.Fatalf("NewLoop #%d with unlimited quota: %v", i+1, err)
		}
		child = loop.Provenance{LoopID: id}
	}
	if got := readSpawned(t, s); got != defaultQuota+1 {
		t.Fatalf("spawned = %d, want %d", got, defaultQuota+1)
	}
	_, err = s.NewLoop(child, cfg(&stubLLM{chunks: []content.Chunk{textChunk("deep")}}))
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopDepthExceeded {
		t.Fatalf("grandchild NewLoop err = %v, want SessionLoopDepthExceeded (depth stays bounded)", err)
	}
}
```

Append to `pkg/rig/rig_test.go`. `errors`, `testing` and `loop` are already imported
there.

```go
// TestWithDelegationLimitsAcceptsUnlimitedQuota proves Quota accepts
// loop.Unlimited, the first invalid negative is -2, and Depth has no unlimited
// value.
func TestWithDelegationLimitsAcceptsUnlimitedQuota(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		limits  DelegationLimits
		wantErr bool
	}{
		{name: "unlimited quota", limits: DelegationLimits{Depth: 2, Quota: loop.Unlimited}},
		{name: "quota below unlimited", limits: DelegationLimits{Quota: -2}, wantErr: true},
		{name: "unlimited depth refused", limits: DelegationLimits{Depth: loop.Unlimited}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &definitionState{seen: make(map[singletonKey]bool)}
			err := WithDelegationLimits(tt.limits)(state)
			var target *DefinitionError
			if tt.wantErr {
				if !errors.As(err, &target) || target.Kind != DefinitionInvalidDelegationLimits {
					t.Fatalf("WithDelegationLimits(%+v) = %v, want DefinitionInvalidDelegationLimits", tt.limits, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("WithDelegationLimits(%+v) = %v, want nil", tt.limits, err)
			}
		})
	}
}
```

**Step 2: Run them and watch them fail.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./internal/sessionruntime/ -run 'Unlimited'
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./pkg/rig/ -run TestWithDelegationLimitsAcceptsUnlimitedQuota
```

Expected: FAIL.
- `withDefaults() = {Depth:2 Quota:64}, want Depth 2, Quota Unlimited`.
- The 65th `NewLoop` is refused `SessionLoopQuotaExceeded`.
- `WithDelegationLimits({Depth:2 Quota:-1}) = rig: invalid definition (invalid_delegation_limits), want nil`.

**Step 3: Implement.** Apply:

```diff
--- a/internal/sessionruntime/limits.go
+++ b/internal/sessionruntime/limits.go
@@ -1,5 +1,7 @@
 package sessionruntime
 
+import "github.com/looprig/harness/pkg/loop"
+
 // Child-agent spawn safety caps. They are the two independent backstops against a runaway
 // agent tree: Depth bounds how DEEP the spawn chain can nest (a sub-loop spawning a
 // sub-loop spawning a sub-loop…), and Quota bounds the TOTAL number of sub-loops a
@@ -23,26 +25,31 @@
 
 // Limits are the in-session agent-spawn safety caps applied by NewLoop: Depth bounds
 // the spawn-chain nesting and Quota bounds the total sub-loops a session may spawn. A zero
-// (or negative — a wiring slip) field adopts the package default via withDefaults, so a
-// caller can never accidentally disable a cap; an explicit positive value overrides it.
+// (or invalid negative — a wiring slip) field adopts the package default via withDefaults,
+// so a caller can never accidentally disable a cap; an explicit positive value overrides
+// it. The one way to disable the Quota is to name loop.Unlimited; Depth has no unlimited
+// value.
 type Limits struct {
 	// Depth is the maximum spawn-chain nesting depth (sub-loops below the primary). Zero
 	// → defaultDepth. A spawn whose parent chain is already this deep is refused.
 	Depth int
 
 	// Quota is the maximum total number of sub-loops the session may spawn over its
-	// lifetime. Zero → defaultQuota. A spawn once this many have been reserved is refused.
+	// lifetime. Zero → defaultQuota; loop.Unlimited disables the cap. A spawn once this
+	// many have been reserved is refused.
 	Quota int
 }
 
 // withDefaults returns a copy of l with any non-positive field replaced by its package
-// default. It is applied in newSession so the live limits are always positive caps — a
-// zero or negative configured value never silently disables the depth or quota backstop.
+// default, except an explicit loop.Unlimited Quota, which is kept. It is applied in
+// newSession so the live Depth is always a positive cap and the live Quota is a positive
+// cap or loop.Unlimited — a zero or invalid negative value never silently disables a
+// backstop.
 func (l Limits) withDefaults() Limits {
 	if l.Depth <= 0 {
 		l.Depth = defaultDepth
 	}
-	if l.Quota <= 0 {
+	if l.Quota <= 0 && l.Quota != loop.Unlimited {
 		l.Quota = defaultQuota
 	}
 	return l
```

```diff
--- a/internal/sessionruntime/session.go
+++ b/internal/sessionruntime/session.go
@@ -1586,7 +1586,7 @@
 		s.loopsMu.Unlock()
 		return uuid.UUID{}, &SessionError{Kind: SessionLoopDepthExceeded}
 	}
-	if counts && s.spawned >= limits.Quota {
+	if counts && limits.Quota != loop.Unlimited && s.spawned >= limits.Quota {
 		s.loopsMu.Unlock()
 		return uuid.UUID{}, &SessionError{Kind: SessionLoopQuotaExceeded}
 	}
```

`session.go` already imports `pkg/loop`. The reservation (`s.spawned++`) and rollback
still run under an unlimited quota, so `spawned` keeps counting.

```diff
--- a/pkg/rig/options.go
+++ b/pkg/rig/options.go
@@ -304,6 +304,11 @@
 // lane. The execution controller may allocate no queue larger than this bound.
 const MaxHustleQueued = 10_000
 
+// DelegationLimits caps sub-loop spawning for every session of the rig. Depth
+// bounds spawn-chain nesting and Quota bounds the total sub-loops one session may
+// spawn over its lifetime; zero means the package default for either. Quota also
+// accepts loop.Unlimited, which disables the lifetime cap; Depth has no unlimited
+// value. Any other negative value is refused by WithDelegationLimits.
 type DelegationLimits struct {
 	Depth int
 	Quota int
@@ -392,7 +397,7 @@
 
 func WithDelegationLimits(limits DelegationLimits) Option {
 	return func(state *definitionState) error {
-		if limits.Depth < 0 || limits.Quota < 0 {
+		if limits.Depth < 0 || limits.Quota < loop.Unlimited {
 			return &DefinitionError{Kind: DefinitionInvalidDelegationLimits}
 		}
 		return singletonCompile(keyDelegationLimits, sessionruntime.WithLifecycleLimits(sessionruntime.Limits{Depth: limits.Depth, Quota: limits.Quota}))(state)
```

Restore needs no change. `restore_constructor.go` (~1343) recounts `spawned` from
`LoopStarted` and never compares it with the quota.

**Step 4: Run the packages.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./internal/sessionruntime/ ./pkg/rig/
```

Expected:
- `internal/sessionruntime` is `ok`. `TestLimitsWithDefaults/negative_falls_back_to_default`
  uses `Quota: -9`, which still defaults, so it needs no edit.
- `pkg/rig` FAILS exactly one existing row: `TestDefineRejectsInvalidFinalLifecycleOptions/negative_delegation_quota`.

**Step 5: Move the rig row to `-2`.** In `pkg/rig/rig_test.go`:

```diff
-		{name: "negative delegation quota", opt: WithDelegationLimits(DelegationLimits{Quota: -1}), kind: DefinitionInvalidDelegationLimits},
+		{name: "negative delegation quota", opt: WithDelegationLimits(DelegationLimits{Quota: -2}), kind: DefinitionInvalidDelegationLimits}, // -1 is loop.Unlimited
```

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./internal/sessionruntime/ ./pkg/rig/
```

Expected: both `ok`.

**Step 6: Commit.**

```sh
git add internal/sessionruntime/limits.go internal/sessionruntime/session.go internal/sessionruntime/unlimited_quota_test.go pkg/rig/options.go pkg/rig/rig_test.go
git commit -m "feat(rig): accept loop.Unlimited as the delegation spawn quota"
```

---

## Task 4: Zero hustle timeout means no execution deadline

**Files:**
- Modify: `pkg/hustle/definition.go`, `internal/hustleruntime/execution.go`
- Modify: `pkg/hustle/definition_test.go`, `pkg/hustle/descriptor_test.go` (the zero-timeout rows flip to acceptance; a missing-timeout row is added)
- Create: `pkg/hustle/zero_timeout_test.go`, `internal/hustleruntime/zero_timeout_test.go`, `pkg/event/hustle_zero_timeout_test.go`

**Step 1: Write the failing tests.**

`pkg/hustle/zero_timeout_test.go` (`validCurrentOptions` index 2 is `WithTimeout`):

```go
package hustle

import (
	"testing"
	"time"
)

// TestZeroTimeoutDefinesNoDeadline proves WithTimeout(0) defines a hustle whose
// Timeout and durable descriptor both carry zero, and that the descriptor
// validates, so a journal holding TimeoutNanos 0 replays.
func TestZeroTimeoutDefinesNoDeadline(t *testing.T) {
	t.Parallel()
	d, err := Define(replaceOption(validCurrentOptions(), 2, WithTimeout(0))...)
	if err != nil {
		t.Fatalf("Define(WithTimeout(0)) = %v, want nil", err)
	}
	if got := d.Timeout(); got != 0 {
		t.Fatalf("Timeout() = %v, want 0", got)
	}
	descriptor := d.Descriptor()
	if descriptor.TimeoutNanos != 0 {
		t.Fatalf("TimeoutNanos = %d, want 0", descriptor.TimeoutNanos)
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("Descriptor().Validate() = %v, want nil", err)
	}
	if _, err := Define(replaceOption(validCurrentOptions(), 2, WithTimeout(-time.Nanosecond))...); err == nil {
		t.Fatal("Define(WithTimeout(-1ns)) = nil, want DefinitionInvalidTimeout")
	}
}
```

`internal/hustleruntime/zero_timeout_test.go`:

```go
package hustleruntime

import (
	"context"
	"testing"
	"time"
)

// TestExecutionContextZeroTimeoutHasNoDeadline proves a zero hustle timeout adds
// no deadline, while caller cancellation and session cancellation both still end
// the execution context, and a positive timeout still sets one.
func TestExecutionContextZeroTimeoutHasNoDeadline(t *testing.T) {
	t.Parallel()
	session, stopSession := context.WithCancel(context.Background())
	defer stopSession()
	r := &runtimeController{executionCtx: session}

	caller, cancelCaller := context.WithCancel(context.Background())
	ctx, done := r.executionContextWithTimeout(caller, 0)
	defer done()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("zero timeout execution context has a deadline")
	}
	cancelCaller()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not end a zero-timeout execution")
	}

	ctx, done = r.executionContextWithTimeout(context.Background(), 0)
	defer done()
	stopSession()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("session cancellation did not end a zero-timeout execution")
	}

	bounded := &runtimeController{executionCtx: context.Background()}
	ctx, done = bounded.executionContextWithTimeout(context.Background(), time.Hour)
	defer done()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("positive timeout execution context has no deadline")
	}
}
```

`pkg/event/hustle_zero_timeout_test.go` covers the durable replay path.
`DefinitionDescriptor.Validate` runs from `validate.go` (~672) on `UnmarshalEvent`.

```go
package event

import (
	"reflect"
	"strings"
	"testing"
)

// TestHustleStartedZeroTimeoutReplays proves a journal record whose hustle
// descriptor carries TimeoutNanos 0 (no execution deadline) round-trips through
// the durable codec, while a negative timeout is still refused on replay.
func TestHustleStartedZeroTimeoutReplays(t *testing.T) {
	t.Parallel()
	run := exhaustiveHustleRun(ModelRuntime{})
	run.Definition.TimeoutNanos = 0
	original := HustleStarted{Header: exhaustiveHustleHeader(), Run: run}
	raw, err := MarshalEvent(original)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	decoded, err := UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("round trip = %#v, want %#v", decoded, original)
	}
	bad := strings.Replace(string(raw), `"TimeoutNanos":0`, `"TimeoutNanos":-1`, 1)
	if bad == string(raw) {
		t.Fatalf("fixture has no \"TimeoutNanos\":0 member: %s", raw)
	}
	if _, err := UnmarshalEvent([]byte(bad)); err == nil {
		t.Fatal("UnmarshalEvent accepted a negative hustle timeout")
	}
}
```

Flip the upstream rows. Both files keep their negative-timeout rows. In
`pkg/hustle/definition_test.go`, the new `missing timeout` row guards difference 1 above.
It passes before and after the change.

```diff
--- a/pkg/hustle/definition_test.go
+++ b/pkg/hustle/definition_test.go
@@ -755,7 +755,8 @@
 		{name: "named nan top p", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, modelWithTopP(model, math.NaN()))), kind: DefinitionInvalidModel, field: "model.sampling.top_p"},
 		{name: "named positive infinity top p", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, modelWithTopP(model, math.Inf(1)))), kind: DefinitionInvalidModel, field: "model.sampling.top_p"},
 		{name: "named negative infinity top p", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, modelWithTopP(model, math.Inf(-1)))), kind: DefinitionInvalidModel, field: "model.sampling.top_p"},
-		{name: "zero timeout", opts: replaceOption(validNamedOptions(client, model), 2, WithTimeout(0)), kind: DefinitionInvalidTimeout},
+		{name: "zero timeout means no deadline", opts: replaceOption(validNamedOptions(client, model), 2, WithTimeout(0))},
+		{name: "missing timeout", opts: withoutOption(validNamedOptions(client, model), 2), kind: DefinitionInvalidTimeout, field: "timeout"},
 		{name: "negative timeout", opts: replaceOption(validNamedOptions(client, model), 2, WithTimeout(-time.Nanosecond)), kind: DefinitionInvalidTimeout},
 		{name: "long timeout accepted", opts: replaceOption(validNamedOptions(client, model), 2, WithTimeout(24*time.Hour+time.Nanosecond))},
 		{name: "zero input limit", opts: replaceOption(validNamedOptions(client, model), 3, WithLimits(Limits{InputBytes: 0, OutputBytes: 1})), kind: DefinitionInvalidLimits},
```

In `pkg/hustle/descriptor_test.go`:

```diff
--- a/pkg/hustle/descriptor_test.go
+++ b/pkg/hustle/descriptor_test.go
@@ -104,7 +104,7 @@
 		{name: "zero prompt hash", value: zeroPromptHash, wantErr: true},
 		{name: "blank prompt revision", value: withDescriptorPromptRevision(current, " "), wantErr: true},
 		{name: "blank policy revision", value: withDescriptorPolicyRevision(current, " "), wantErr: true},
-		{name: "zero timeout", value: withDescriptorTimeout(current, 0), wantErr: true},
+		{name: "zero timeout means no deadline", value: withDescriptorTimeout(current, 0)},
 		{name: "negative timeout", value: withDescriptorTimeout(current, -1), wantErr: true},
 		{name: "zero input limit", value: withDescriptorLimits(current, Limits{OutputBytes: 1}), wantErr: true},
 		{name: "zero output limit", value: withDescriptorLimits(current, Limits{InputBytes: 1}), wantErr: true},
```

**Step 2: Run them and watch them fail.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./pkg/hustle/ ./internal/hustleruntime/ ./pkg/event/ -run 'Timeout|TestDefineValidation|Descriptor'
```

Expected: FAIL.
- `Define(WithTimeout(0)) = hustle: invalid definition: invalid_timeout (timeout), want nil`.
- `TestDefineValidation/zero_timeout_means_no_deadline` fails.
- The descriptor row fails.
- `zero timeout execution context has a deadline`.
- `TestHustleStartedZeroTimeoutReplays` fails at Marshal/UnmarshalEvent.

**Step 3: Implement.** Apply:

```diff
--- a/pkg/hustle/definition.go
+++ b/pkg/hustle/definition.go
@@ -180,7 +180,7 @@
 	if d.ModelSource != ModelSourceCurrentLoop && d.ModelSource != ModelSourceNamed {
 		return &DefinitionError{Kind: DefinitionInvalidModelSource, Field: "model_source"}
 	}
-	if d.TimeoutNanos <= 0 {
+	if d.TimeoutNanos < 0 {
 		return &DefinitionError{Kind: DefinitionInvalidTimeout, Field: "timeout"}
 	}
 	if invalidLimits(d.Limits) {
@@ -360,7 +360,10 @@
 	}
 }
 
-// WithTimeout sets the exact invocation timeout.
+// WithTimeout sets the exact invocation timeout. It is required. Zero means no
+// execution deadline: the invocation still ends on caller or session
+// cancellation, and audit and finalization keep their own bounded timeouts. A
+// negative timeout is invalid.
 func WithTimeout(timeout time.Duration) Option {
 	return func(options *definitionOptions) error {
 		if err := options.singleton("timeout"); err != nil {
@@ -508,7 +511,9 @@
 			return err
 		}
 	}
-	if options.timeout <= 0 {
+	// Omitting WithTimeout stays invalid: no deadline must be asked for with
+	// WithTimeout(0), never reached by forgetting the option.
+	if _, set := options.seen["timeout"]; !set || options.timeout < 0 {
 		return &DefinitionError{Kind: DefinitionInvalidTimeout, Field: "timeout"}
 	}
 	if invalidLimits(options.limits) {
```

Also update the `Timeout` accessor doc at `definition.go` ~886:

```diff
-// Timeout returns the definition's exact invocation timeout.
+// Timeout returns the definition's exact invocation timeout. Zero means no
+// execution deadline (the zero Definition also reports zero).
```

`options.seen` is the singleton map `WithTimeout` records into. `DefinitionError{Kind:
DefinitionInvalidTimeout, Field: "timeout"}` is the error omission already produced, so
no new error kind is added.

```diff
--- a/internal/hustleruntime/execution.go
+++ b/internal/hustleruntime/execution.go
@@ -1076,7 +1076,12 @@
 	if r.executionCtx.Err() != nil {
 		cancelCombined()
 	}
-	execution, cancelTimeout := context.WithTimeout(combined, timeout)
+	// A zero timeout is the definition's explicit "no execution deadline"; the
+	// combined caller and session cancellation above still applies.
+	execution, cancelTimeout := combined, context.CancelFunc(func() {})
+	if timeout > 0 {
+		execution, cancelTimeout = context.WithTimeout(combined, timeout)
+	}
 	return execution, func() {
 		cancelTimeout()
 		stopSessionCancel()
```

`Definition.Timeout()` is consumed only through `newExecutionContext` (`execution.go`
~301 and ~368). `newAuditContext` and `newFinalizationContext` keep their bounded
timeouts.

**Step 4: Run the packages.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./pkg/hustle/ ./internal/hustleruntime/ ./pkg/event/
```

Expected: all three `ok`.

**Step 5: Commit.**

```sh
git add pkg/hustle/definition.go pkg/hustle/definition_test.go pkg/hustle/descriptor_test.go pkg/hustle/zero_timeout_test.go internal/hustleruntime/execution.go internal/hustleruntime/zero_timeout_test.go pkg/event/hustle_zero_timeout_test.go
git commit -m "feat(hustle): treat an explicit zero timeout as no execution deadline"
```

---

## Task 5: The duplicate-key check treats `tool_use` input as opaque

**Files:**
- Modify: `pkg/event/marshal.go` (`rejectDuplicateJSONKeys`, `inspectJSONValue`, new `messageBlockPath`)
- Create: `pkg/event/opaque_tool_input_test.go`

The bug: `UnmarshalEvent` → `rejectDuplicateJSONKeys` recurses into a `tool_use` block's
`Input`. That field holds the model's raw argument bytes, and the writer retains them
verbatim. A model that repeats a key therefore writes a journal that restore refuses. The
check lives in `pkg/event` alone. SessionStore's journal envelope is binary and carries
the event bytes opaquely, so no other module is involved.

The message-block paths were audited against every message-carrying event field:
- `StepDone.Messages`: `.messages[].blocks[]`
- `TurnStarted`/`TurnDone.Message`: `.message.blocks[]`
- compaction `Summary`/`Retained`: `.summary.blocks[]`, `.retained[].blocks[]`
- `GatePrepared.Resume.Message` (`gate.go:40-45`): `.resume.message.blocks[]`

Nested tool-result `.content[]` is stripped. Before editing, re-run the audit, since a
field may have been added since `b9797b96`:

```sh
grep -n 'content\.\(AgenticMessages\|AIMessage\|UserMessage\)' pkg/event/*.go | grep -v _test
```

Expected: exactly `compaction.go` (Summary, Retained), `gate.go:42`, and `turn.go`
(92, 172, 185, 199, 273). If a new field appears, add its path to `messageBlockPath` and to
the accepted table below.

**Step 1: Write the failing test.** Create `pkg/event/opaque_tool_input_test.go`. It reuses
`gateWireHeader` (`gate_wire_test.go`) and `resumeGate` (`gate_resume_test.go`).

```go
package event

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
)

// duplicateKeyToolInput is valid JSON that repeats a key, exactly as a model may
// emit it as tool-call arguments.
const duplicateKeyToolInput = `{"results":[],"results":[{"id":"example"}]}`

// TestStepDoneReplaysDuplicateKeysInToolInput proves a committed step whose
// tool_use Input repeats a key replays byte-for-byte, while a duplicate envelope
// key or a duplicate block key in the same record is still refused.
func TestStepDoneReplaysDuplicateKeysInToolInput(t *testing.T) {
	t.Parallel()
	newID := func() uuid.UUID {
		id, err := uuid.New()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	input := json.RawMessage(duplicateKeyToolInput)
	e := StepDone{
		Header: Header{Coordinates: identity.Coordinates{SessionID: newID(), LoopID: newID(), TurnID: newID(), StepID: newID()}, EventID: newID(), CreatedAt: time.Now()},
		Messages: content.AgenticMessages{&content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{
			&content.ToolUseBlock{ID: "call-1", Name: "example", Input: input},
		}}}},
	}
	raw, err := MarshalEvent(e)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	decoded, err := UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	got := decoded.(StepDone).Messages[0].(*content.AIMessage).Blocks[0].(*content.ToolUseBlock).Input
	if string(got) != duplicateKeyToolInput {
		t.Fatalf("tool input = %s, want original bytes %s", got, duplicateKeyToolInput)
	}
	for name, bad := range map[string]string{
		"duplicate envelope key": strings.Replace(string(raw), `"type":"StepDone"`, `"type":"StepDone","type":"StepDone"`, 1),
		"duplicate block key":    strings.Replace(string(raw), `"Input":`, `"Input":{},"Input":`, 1),
	} {
		if bad == string(raw) {
			t.Fatalf("%s: fixture did not change", name)
		}
		if _, err := UnmarshalEvent([]byte(bad)); err == nil {
			t.Fatalf("%s: UnmarshalEvent accepted it", name)
		}
	}
}

// TestGatePreparedResumeReplaysDuplicateKeysInToolInput covers the parked step
// GatePrepared.Resume carries, which restore decodes through UnmarshalEvent.
func TestGatePreparedResumeReplaysDuplicateKeysInToolInput(t *testing.T) {
	t.Parallel()
	input := json.RawMessage(duplicateKeyToolInput)
	message := &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{
		&content.ToolUseBlock{ID: "call-1", Name: "Ask", Input: input},
	}}}
	raw, err := MarshalEvent(GatePrepared{Header: gateWireHeader(), Gate: resumeGate(), Resume: &ToolStepResume{Message: message, ToolUseID: "call-1"}})
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	decoded, err := UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	got := decoded.(GatePrepared).Resume.Message.Blocks[0].(*content.ToolUseBlock).Input
	if string(got) != duplicateKeyToolInput {
		t.Fatalf("tool input = %s, want original bytes %s", got, duplicateKeyToolInput)
	}
	bad := strings.Replace(string(raw), `"Input":`, `"Input":{},"Input":`, 1)
	if bad == string(raw) {
		t.Fatal("fixture did not change")
	}
	if _, err := UnmarshalEvent([]byte(bad)); err == nil {
		t.Fatal("UnmarshalEvent accepted a duplicate block key inside resume")
	}
}

// TestRejectDuplicateJSONKeysTreatsOnlyToolUseInputAsOpaque pins the exact scope
// of the exemption: the Input of a tool_use block on a message-block path, in
// either key order, including blocks nested under content. Everything else,
// including a non-tool block's input and a top-level "input", stays strict.
func TestRejectDuplicateJSONKeysTreatsOnlyToolUseInputAsOpaque(t *testing.T) {
	t.Parallel()
	dup := `{"results":[],"results":[]}`
	accepted := map[string]string{
		"messages path":           `{"messages":[{"blocks":[{"type":"tool_use","Input":` + dup + `}]}]}`,
		"message path":            `{"message":{"blocks":[{"type":"tool_use","Input":` + dup + `}]}}`,
		"retained path":           `{"retained":[{"blocks":[{"Input":` + dup + `,"type":"tool_use"}]}]}`,
		"summary path":            `{"summary":{"blocks":[{"type":"tool_use","Input":` + dup + `}]}}`,
		"resume path":             `{"resume":{"message":{"blocks":[{"type":"tool_use","Input":` + dup + `}]}}}`,
		"nested under content":    `{"messages":[{"blocks":[{"type":"tool_result","content":[{"type":"tool_use","Input":` + dup + `}]}]}]}`,
		"input before type":       `{"messages":[{"blocks":[{"Input":` + dup + `,"type":"tool_use"}]}]}`,
		"lower-case input member": `{"messages":[{"blocks":[{"type":"tool_use","input":` + dup + `}]}]}`,
	}
	for name, raw := range accepted {
		if err := rejectDuplicateJSONKeys([]byte(raw)); err != nil {
			t.Errorf("%s: rejectDuplicateJSONKeys = %v, want nil", name, err)
		}
	}
	refused := map[string]string{
		"duplicate envelope key":        `{"type":"StepDone","TYPE":"StepDone"}`,
		"duplicate nested envelope key": `{"cause":{"command_id":"one","COMMAND_ID":"two"}}`,
		"top-level input":               `{"input":` + dup + `}`,
		"input off a block path":        `{"resume":{"input":` + dup + `}}`,
		"duplicate outside input":       `{"resume":{"results":[],"results":[]}}`,
		"text block input":              `{"messages":[{"blocks":[{"type":"text","Input":` + dup + `}]}]}`,
		"retained text block input":     `{"retained":[{"blocks":[{"Input":` + dup + `,"type":"text"}]}]}`,
		"untyped block input":           `{"messages":[{"blocks":[{"Input":` + dup + `}]}]}`,
		"duplicate Input member":        `{"messages":[{"blocks":[{"type":"tool_use","Input":{},"input":{}}]}]}`,
		"nested duplicate Input member": `{"messages":[{"blocks":[{"type":"tool_result","content":[{"type":"tool_use","Input":{},"input":{}}]}]}]}`,
		"duplicate block type":          `{"messages":[{"blocks":[{"type":"tool_use","Type":"tool_use","Input":{}}]}]}`,
	}
	for name, raw := range refused {
		if err := rejectDuplicateJSONKeys([]byte(raw)); err == nil {
			t.Errorf("%s: rejectDuplicateJSONKeys accepted %s", name, raw)
		}
	}
}
```

**Step 2: Run it and watch it fail.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./pkg/event/ -run 'ReplaysDuplicateKeysInToolInput|TestRejectDuplicateJSONKeysTreatsOnlyToolUseInputAsOpaque'
```

Expected: FAIL.
- Both round trips fail with `UnmarshalEvent: event: decode : duplicate field "results"`.
- All eight accepted rows fail with `duplicate field "results"`.
- The refused rows already pass.

**Step 3: Implement.** Apply to `pkg/event/marshal.go`. `strings` is already imported.

```diff
--- a/pkg/event/marshal.go
+++ b/pkg/event/marshal.go
@@ -437,16 +437,28 @@
 	return ev, nil
 }
 
+// rejectDuplicateJSONKeys refuses an event whose JSON repeats an object key
+// (compared case-insensitively, as encoding/json matches fields), because a
+// repeated key makes the decoded value depend on which copy wins.
+//
+// The one exemption is a tool_use block's Input on a message-block path
+// (see messageBlockPath): it holds the model's raw tool-call arguments, which
+// the writer retains verbatim as json.RawMessage, so a model that repeats a key
+// must not make the journal unreplayable. The block's own keys, including a
+// repeated "input" or "type", stay strictly checked, and an "input" member of any
+// other block type is checked as usual.
 func rejectDuplicateJSONKeys(data []byte) error {
 	decoder := json.NewDecoder(bytes.NewReader(data))
 	token, err := decoder.Token()
 	if err != nil {
 		return err
 	}
-	return inspectJSONValue(decoder, token)
+	return inspectJSONValue(decoder, token, "")
 }
 
-func inspectJSONValue(decoder *json.Decoder, token json.Token) error {
+// inspectJSONValue walks one JSON value. path is the lower-cased member path of
+// the value ("" at the root, ".key" per object member, "[]" per array element).
+func inspectJSONValue(decoder *json.Decoder, token json.Token, path string) error {
 	delim, ok := token.(json.Delim)
 	if !ok {
 		return nil
@@ -454,6 +466,9 @@
 	switch delim {
 	case '{':
 		seen := make(map[string]struct{})
+		var blockType string
+		var deferredInput json.RawMessage
+		onBlockPath := messageBlockPath(path)
 		for decoder.More() {
 			keyToken, err := decoder.Token()
 			if err != nil {
@@ -468,23 +483,39 @@
 				return fmt.Errorf("duplicate field %q", key)
 			}
 			seen[canonical] = struct{}{}
+			if onBlockPath && canonical == "input" {
+				// The block's type may follow its input, so hold the raw value
+				// until the object closes.
+				if err := decoder.Decode(&deferredInput); err != nil {
+					return err
+				}
+				continue
+			}
 			valueToken, err := decoder.Token()
 			if err != nil {
 				return err
 			}
-			if err := inspectJSONValue(decoder, valueToken); err != nil {
+			if onBlockPath && canonical == "type" {
+				blockType, _ = valueToken.(string)
+			}
+			if err := inspectJSONValue(decoder, valueToken, path+"."+canonical); err != nil {
 				return err
 			}
 		}
-		_, err := decoder.Token()
-		return err
+		if _, err := decoder.Token(); err != nil {
+			return err
+		}
+		if deferredInput != nil && blockType != "tool_use" {
+			return rejectDuplicateJSONKeys(deferredInput)
+		}
+		return nil
 	case '[':
 		for decoder.More() {
 			valueToken, err := decoder.Token()
 			if err != nil {
 				return err
 			}
-			if err := inspectJSONValue(decoder, valueToken); err != nil {
+			if err := inspectJSONValue(decoder, valueToken, path+"[]"); err != nil {
 				return err
 			}
 		}
@@ -492,7 +523,22 @@
 		return err
 	default:
 		return fmt.Errorf("unexpected delimiter %q", delim)
+	}
+}
+
+// messageBlockPath reports whether path addresses a content block of a message
+// an event carries: StepDone/TurnStarted/TurnDone messages, compaction's summary
+// and retained history, and GatePrepared's resume snapshot. Blocks nested under
+// a block's content (a tool result's content) count as the enclosing path.
+func messageBlockPath(path string) bool {
+	for strings.HasSuffix(path, ".content[]") {
+		path = strings.TrimSuffix(path, ".content[]")
+	}
+	switch path {
+	case ".messages[].blocks[]", ".message.blocks[]", ".retained[].blocks[]", ".summary.blocks[]", ".resume.message.blocks[]":
+		return true
 	}
+	return false
 }
 
 // validateDecodedEvent preserves the one additive compatibility exception in the
```

**Step 4: Run the package.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./pkg/event/
```

Expected: `ok  github.com/looprig/harness/pkg/event`. The existing fuzz seeds
(`*_fuzz_test.go`) run as ordinary tests and stay green.

**Step 5: Commit.**

```sh
git add pkg/event/marshal.go pkg/event/opaque_tool_input_test.go
git commit -m "fix(event): treat tool_use input as opaque in the duplicate-key check"
```

---

## Task 6: Documentation

**Files:**
- Modify: `pkg/loop/README.md`, `pkg/rig/README.md`, `pkg/hustle/README.md`, `pkg/event/README.md`, `pkg/event/errors.go` (doc comment only)
- Modify: `docs/plans/2026-09-25-unbounded-execution-and-opaque-tool-input-design.md` (Progress table)
- Modify: `docs/plans/2026-09-25-impl-00-master-plan.md` (the row for plan 04)

Harness has no CHANGELOG. Release notes live in the v0.41.0 tag annotation, which impl-05
writes. The material for it is in Step 5.

**Step 1: `pkg/loop/README.md`.** Replace the last sentence of "### Modes and tool limits"
(currently "Default tool limits are 25 iterations, 100 calls per turn, 8 parallel calls.")
with:

```markdown
Default tool limits are 25 iterations, 100 calls per turn, 8 parallel calls.
Zero in a `ToolLimits` field means that default. `loop.Unlimited` (-1) on
`Iterations` or `Calls` disables that cap, in the base limits or a mode override.
Any other negative value is refused. `Parallel`, `ResultBytes` and `CaptureBytes`
have no unlimited value. An unlimited loop stops only when the model ends the turn,
or on interrupt, shutdown or context cancellation.
```

**Step 2: `pkg/rig/README.md`.** Under "### Validation at the boundary", after the
"Hustle lane bounds …" bullet, add:

```markdown
- Delegation limits are non-negative, except that `Quota` may be
  `loop.Unlimited` to disable the per-session lifetime spawn cap. `Depth`
  is always bounded (zero means the default of 3).
```

**Step 3: `pkg/hustle/README.md`.** After the `hustle.Define(...)` example, in the
paragraph that starts "A loop invokes a hustle …", add before it:

```markdown
`WithTimeout` is required. `WithTimeout(0)` means no execution deadline: the run
still ends on caller or session cancellation, and `AuditTimeout` and
`FinalizationTimeout` still bound audit and finalization. A negative timeout is
refused. A descriptor with `TimeoutNanos: 0` is valid and replays.
```

**Step 4: `pkg/event/README.md` and `errors.go`.** At the end of "## How it is
designed" (before "### Why the mixins"), add:

```markdown
`UnmarshalEvent` refuses any record whose JSON repeats an object key
(case-insensitively). The one exemption is a `tool_use` block's `Input` inside
an event's message blocks: those bytes are the model's tool-call arguments,
kept verbatim, so a model that repeats a key cannot make a journal
unreplayable. The block's own keys stay strictly checked.
```

In `pkg/event/errors.go`, extend the `ToolLimitError` doc comment with one line:

```go
// MaxIterations or MaxCalls is loop.Unlimited (-1) when that cap is disabled;
// the error is then caused by the other, finite cap.
```

**Step 5: Progress tables.** In the design doc's Progress table, set items 1–3 to
`on main (unreleased; ships in v0.41.0 via impl-05)`. In the master plan, set row
`04 harness unbounded + opaque input` to `merged` with the last commit SHA. Record
this release-note material for impl-05 in the design doc under a new
`## Release-note material (for v0.41.0)` heading:

- `loop.Unlimited` (new exported const, minor). `ToolLimits.Iterations`/`Calls` and
  `rig.DelegationLimits.Quota` accept `-1` to disable the cap. **Behaviour change for
  a caller that passed `-1` by mistake:** it was refused before (`invalid_tool_limits`,
  `invalid_delegation_limits`) and is now unlimited. `-2` and below are still refused.
  At the `internal/loopruntime` and `sessionruntime` level, `-1` was silently defaulted
  before; it is now preserved.
- `hustle.WithTimeout(0)` and a descriptor `TimeoutNanos: 0` are accepted as "no
  execution deadline". Omitting `WithTimeout` is still refused. It is a relaxation, not
  one-way: an older harness refuses to replay a journal holding a zero-timeout
  descriptor, so do not roll a process back below v0.41.0 once it has run a zero-timeout
  hustle.
- Fix: a `tool_use` block's `Input` is opaque to the duplicate-key check. Journals whose
  tool arguments repeat a key now replay. Such a journal is unreadable by harness
  ≤ v0.40.x, as it already was.

**Step 6: Commit.**

```sh
git add pkg/loop/README.md pkg/rig/README.md pkg/hustle/README.md pkg/event/README.md pkg/event/errors.go docs/plans/2026-09-25-unbounded-execution-and-opaque-tool-input-design.md docs/plans/2026-09-25-impl-00-master-plan.md
git commit -m "docs: document loop.Unlimited, zero hustle timeout and opaque tool input"
```

If the design doc and master plan are still untracked at this point, `git add` starts
tracking them. That is intended, because both belong in harness `docs/plans/`. If impl-05
already committed them, the diff is just the table edits.

---

## Task 7: Full-suite verification (no release)

**Step 1: Standalone tests.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 go test -race ./...
```

Expected: every package `ok`, no `FAIL`. On this plan's scratch run, the only packages
touched were `pkg/loop`, `pkg/rig`, `pkg/hustle`, `pkg/event`, `internal/loopruntime`,
`internal/sessionruntime` and `internal/hustleruntime`.

**Step 2: Native checks.**

```sh
GOWORK=off GOTOOLCHAIN=go1.26.8 make check
```

This runs `fmt-check vet check-staticcheck check-gosec check-vuln test build`. Expected:
it exits 0. If `check-vuln` needs a network or database that is unavailable, record that
and run the rest: `make fmt-check vet check-staticcheck check-gosec build`.

**Step 3: API surface.** If `apidiff` is installed:

```sh
git stash -u  # only if needed; otherwise use a worktree
apidiff -m github.com/looprig/harness@<baseline-sha> github.com/looprig/harness  # or: gorelease -base=v0.40.2
```

Expected: one compatible addition, `pkg/loop.Unlimited`, and nothing incompatible. This
fits the minor that impl-05 releases.

**Step 4: Confirm the scope.**

```sh
git log --oneline <baseline-sha>..HEAD   # six commits from Tasks 1–6
git status --short                        # clean apart from files owned by other plans
```

Do **not** tag. Push `main` only with owner confirmation, and hand off to impl-05. Its
final task releases harness v0.41.0, including these commits. It then updates
`repositories.mk`, `go.work` and `AGENTS.md` in the outer workspace (not committed by the
agent). Oxy then drops fork patches 1, 2 and 5 (master plan 09).
