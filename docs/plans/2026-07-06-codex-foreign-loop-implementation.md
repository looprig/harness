# Codex Foreign Loop Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add Codex CLI as a second foreign-loop backend that can run as a primary loop or subagent, resume by Codex thread id, and publish normalized looprig events.

**Architecture:** Extend the existing `pkg/foreignloop` actor instead of creating a new session model. Add a durable late-bound foreign-session event for adapters like Codex whose session id is learned from JSONL, then add `pkg/foreignloop/codex` as a subprocess adapter over `codex exec --json` and `codex exec resume --json <id>`.

**Tech Stack:** Go stdlib (`os/exec`, `bufio`, `encoding/json`, `context`, `syscall`, `time`), existing looprig packages (`pkg/event`, `pkg/loop`, `pkg/session`, `pkg/foreignloop`, `core/content`, `core/uuid`). No new third-party dependencies.

**Design Reference:** `docs/plans/2026-07-06-codex-foreign-loop-design.md`.

---

## Phase 1: Durable Late-Bound Foreign SID

### Task 1: Add `event.ForeignSessionBound`

**Files:**
- Modify: `pkg/event/event.go`
- Modify: `pkg/event/validate.go`
- Modify: `pkg/event/marshal_test.go`
- Create: `pkg/event/foreign_session_bound_test.go`

**Step 1: Write the failing test**

Create `pkg/event/foreign_session_bound_test.go`:

```go
package event

import (
	"encoding/json"
	"testing"
)

func TestForeignSessionBoundRoundTrip(t *testing.T) {
	t.Parallel()
	in := ForeignSessionBound{ForeignSID: "0199a213-81c0-7800-8aa1-bbab2a035a53"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ForeignSessionBound
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ForeignSID != in.ForeignSID {
		t.Fatalf("ForeignSID = %q, want %q", out.ForeignSID, in.ForeignSID)
	}
	if in.Class() != Enduring || in.Scope() != ScopeLoop || in.EndsTurn() {
		t.Fatalf("ForeignSessionBound class/scope/terminal mismatch")
	}
}
```

Also add a `ForeignSessionBound{ForeignSID: "sid"}` row to the enduring round-trip table in `pkg/event/marshal_test.go`.

**Step 2: Run the test and confirm it fails**

Run:

```bash
go test ./pkg/event -run 'ForeignSessionBound|MarshalEventRoundTripEnduring' -count=1
```

Expected: compile failure for undefined `ForeignSessionBound`.

**Step 3: Implement the event**

Add to `pkg/event/event.go` near `LoopStarted`:

```go
// ForeignSessionBound records the foreign agent session id for adapters that
// cannot accept a pre-minted id at LoopStarted time. It is loop-scoped and
// Enduring because restore needs it to resume the foreign session.
type ForeignSessionBound struct {
	enduring
	loopScoped
	Header
	ForeignSID string `json:"foreign_sid"`
}
```

Add `func (ForeignSessionBound) isEvent() {}` and add it to the `classify` switch in `pkg/event/validate.go`, using the existing naming/profile pattern.

**Step 4: Run and commit**

Run:

```bash
go test -race ./pkg/event -run 'ForeignSessionBound|MarshalEventRoundTripEnduring' -count=1
```

Expected: PASS.

Commit:

```bash
git add pkg/event/event.go pkg/event/validate.go pkg/event/marshal_test.go pkg/event/foreign_session_bound_test.go
git commit -m "feat(event): add durable ForeignSessionBound"
```

### Task 2: Teach restore discovery to recover late-bound sid

**Files:**
- Modify: `pkg/session/restore.go`
- Modify: `pkg/session/restore_constructor.go`
- Test: `pkg/session/foreign_restore_test.go`

**Step 1: Write the failing tests**

Add tests in `pkg/session/foreign_restore_test.go`:

```go
func TestFindForeignSIDPrefersLoopStarted(t *testing.T) {
	t.Parallel()
	got := findForeignSID([]event.Event{
		event.LoopStarted{ForeignSID: "from-loop-started"},
		event.ForeignSessionBound{ForeignSID: "from-bound"},
	})
	if got != "from-loop-started" {
		t.Fatalf("sid = %q, want LoopStarted sid", got)
	}
}

func TestFindForeignSIDFallsBackToForeignSessionBound(t *testing.T) {
	t.Parallel()
	got := findForeignSID([]event.Event{
		event.LoopStarted{},
		event.ForeignSessionBound{ForeignSID: "from-bound"},
	})
	if got != "from-bound" {
		t.Fatalf("sid = %q, want bound sid", got)
	}
}
```

**Step 2: Run and confirm failure**

Run:

```bash
go test ./pkg/session -run 'TestFindForeignSID' -count=1
```

Expected: undefined `findForeignSID`.

**Step 3: Implement discovery**

Add to `pkg/session/restore.go`:

```go
func findForeignSID(events []event.Event) string {
	for _, ev := range events {
		if ls, ok := ev.(event.LoopStarted); ok && ls.ForeignSID != "" {
			return ls.ForeignSID
		}
	}
	for _, ev := range events {
		if fb, ok := ev.(event.ForeignSessionBound); ok && fb.ForeignSID != "" {
			return fb.ForeignSID
		}
	}
	return ""
}
```

In `restore_constructor.go`, after `primaryEvents` are drained and folded, pass `findForeignSID(primaryEvents)` into `buildRestoredSession` instead of `rootLoop.ForeignSID`. Preserve existing behavior for Claude because `primaryEvents` includes the root `LoopStarted`.

**Step 4: Run and commit**

Run:

```bash
go test -race ./pkg/session -run 'TestFindForeignSID|ForeignRestore' -count=1
```

Expected: PASS.

Commit:

```bash
git add pkg/session/restore.go pkg/session/restore_constructor.go pkg/session/foreign_restore_test.go
git commit -m "feat(session): restore late-bound foreign session ids"
```

## Phase 2: Shared Foreign Actor Supports Prebound and Late-Bound IDs

### Task 3: Add sid binding mode to `foreignloop.Spec`

**Files:**
- Modify: `pkg/foreignloop/foreignloop.go`
- Modify: `pkg/foreignloop/loop.go`
- Test: `pkg/foreignloop/loop_test.go`

**Step 1: Write the failing test**

Add a unit test that constructs `foreignloop.New` with a fake agent and a new `Spec` mode for late-bound sid, then asserts the returned sid is empty. Name it:

```go
func TestNewLateBoundSpecReturnsEmptyInitialSID(t *testing.T)
```

Expected setup:

- `Spec{Agent: fake, Cwd: t.TempDir(), SIDMode: SIDLateBound}`
- valid `cfg.System`
- deterministic id generator
- returned `sid == ""`

**Step 2: Run and confirm failure**

Run:

```bash
go test ./pkg/foreignloop -run TestNewLateBoundSpecReturnsEmptyInitialSID -count=1
```

Expected: compile failure for `SIDMode`.

**Step 3: Implement**

Add:

```go
type SIDMode uint8

const (
	SIDPrebound SIDMode = iota
	SIDLateBound
)
```

Add `SIDMode SIDMode` to `foreignloop.Spec`. Treat zero value as `SIDPrebound` so Claude is unchanged.

In `New`, mint `sid` only for `SIDPrebound`. For `SIDLateBound`, leave `sid == ""`; the actor will record it from the first `ForeignInit`.

**Step 4: Run and commit**

Run:

```bash
go test -race ./pkg/foreignloop -run TestNewLateBoundSpecReturnsEmptyInitialSID -count=1
```

Expected: PASS.

Commit:

```bash
git add pkg/foreignloop/foreignloop.go pkg/foreignloop/loop.go pkg/foreignloop/loop_test.go
git commit -m "feat(foreignloop): support late-bound session ids"
```

### Task 4: Publish `ForeignSessionBound` on first late-bound init

**Files:**
- Modify: `pkg/foreignloop/loop.go`
- Modify: `pkg/foreignloop/turn.go`
- Modify: `pkg/foreignloop/header.go`
- Test: `pkg/foreignloop/foreignloop_test.go`

**Step 1: Write the failing test**

Add a fake stream script:

```go
[]foreignloop.ForeignEvent{
	{Kind: foreignloop.ForeignInit, SessionID: "codex-thread-1"},
	{Kind: foreignloop.ForeignStepComplete, Message: aiMsg("ok")},
	{Kind: foreignloop.ForeignTerminalOK},
}
```

Assert the published enduring event sequence contains:

1. `TurnStarted`
2. `ForeignSessionBound{ForeignSID: "codex-thread-1"}`
3. `StepDone`
4. `TurnDone`

Also assert the loop's next turn sees `ForeignTurn{StartNew:false, ForeignSID:"codex-thread-1"}`.

**Step 2: Run and confirm failure**

Run:

```bash
go test ./pkg/foreignloop -run TestLateBoundSessionPublishesForeignSessionBound -count=1
```

Expected: no `ForeignSessionBound` is published and the second turn has empty sid.

**Step 3: Implement**

Add actor-owned fields:

```go
sidBound bool
```

Initialize `sidBound` to true for prebound loops and restored loops. For late-bound new loops, false.

In `drainStream`, when `ForeignInit` carries a non-empty `SessionID`:

- If `l.sid == ""`, set `l.sid = fe.SessionID`, set `sidBound = true`, publish `event.ForeignSessionBound{ForeignSID: fe.SessionID}`.
- If `l.sid != "" && fe.SessionID != l.sid`, keep the current mismatch warning behavior.

Add `ForeignSessionBound` to `fillForeignHeader` and `withForeignHeader`.

**Step 4: Run and commit**

Run:

```bash
go test -race ./pkg/foreignloop -run 'LateBound|ForeignSessionBound' -count=1
```

Expected: PASS.

Commit:

```bash
git add pkg/foreignloop/loop.go pkg/foreignloop/turn.go pkg/foreignloop/header.go pkg/foreignloop/foreignloop_test.go
git commit -m "feat(foreignloop): bind late foreign session ids from init"
```

## Phase 3: Codex Engine Selector

### Task 5: Add `loop.EngineForeignCodex`

**Files:**
- Modify: `pkg/loop/config.go`
- Modify: `pkg/session/foreign_newloop_test.go`
- Modify: `pkg/session/foreign_restore_test.go`

**Step 1: Write failing tests**

Add table rows anywhere `EngineForeignClaude` is tested for missing builder:

```go
{
	name: "codex foreign engine without a builder fails closed",
	engine: loop.EngineForeignCodex,
	wantErr: true,
	wantKind: SessionForeignBuilderMissing,
}
```

Add the matching restore missing-builder row.

**Step 2: Run and confirm failure**

Run:

```bash
go test ./pkg/loop ./pkg/session -run 'EngineForeignCodex|ForeignNewLoop|ForeignRestore' -count=1
```

Expected: compile failure for `EngineForeignCodex`.

**Step 3: Implement**

Add `EngineForeignCodex` after `EngineForeignClaude`. Do not change the `session.newLoop` switch yet; the default foreign branch should handle it.

**Step 4: Run and commit**

Run:

```bash
go test -race ./pkg/loop ./pkg/session -run 'EngineForeignCodex|ForeignNewLoop|ForeignRestore' -count=1
```

Expected: PASS.

Commit:

```bash
git add pkg/loop/config.go pkg/session/foreign_newloop_test.go pkg/session/foreign_restore_test.go
git commit -m "feat(loop): add Codex foreign engine selector"
```

## Phase 4: Pure Codex Adapter

### Task 6: Add Codex config, env, and argv builder

**Files:**
- Create: `pkg/foreignloop/codex/doc.go`
- Create: `pkg/foreignloop/codex/spec.go`
- Create: `pkg/foreignloop/codex/env.go`
- Create: `pkg/foreignloop/codex/args.go`
- Test: `pkg/foreignloop/codex/spec_test.go`
- Test: `pkg/foreignloop/codex/env_test.go`
- Test: `pkg/foreignloop/codex/args_test.go`

**Step 1: Write failing tests**

Test cases:

- `NewSpec` rejects empty `ExecPath`.
- `NewSpec` rejects empty `Cwd`.
- env whitelist includes only allowlisted parent keys plus sorted credentials.
- first-turn args include `exec`, `--json`, `--cd`, `--sandbox`, `-c`,
  `approval_policy="<policy>"`, and prompt.
- resume args include `exec`, `resume`, `--json`, sid, and prompt.
- enum mappings fail closed to least privilege (`read-only` / `on-request`) for unknown values.

**Step 2: Run and confirm failure**

Run:

```bash
go test ./pkg/foreignloop/codex -run . -count=1
```

Expected: package does not exist.

**Step 3: Implement**

Use the design's `SpecConfig`, `SandboxMode`, and `ApprovalPolicy` types. `NewSpec` returns:

```go
foreignloop.Spec{
	Agent: &Agent{ExecPath: cfg.ExecPath, Model: cfg.Model, Profile: cfg.Profile, Env: env},
	Cwd: cfg.Cwd,
	Env: env,
	SIDMode: foreignloop.SIDLateBound,
}
```

Keep argv construction in pure functions:

```go
func buildStartArgs(t foreignloop.ForeignTurn, c runConfig, prompt string) []string
func buildResumeArgs(t foreignloop.ForeignTurn, c runConfig, prompt string) []string
```

Do not spawn a process in this task.

**Step 4: Run and commit**

Run:

```bash
go test -race ./pkg/foreignloop/codex -run . -count=1
```

Expected: PASS.

Commit:

```bash
git add pkg/foreignloop/codex
git commit -m "feat(foreignloop): add Codex adapter config and argv"
```

### Task 7: Add Codex JSONL decoder

**Files:**
- Create: `pkg/foreignloop/codex/decode.go`
- Test: `pkg/foreignloop/codex/decode_test.go`
- Test: `pkg/foreignloop/codex/decode_fuzz_test.go`

**Step 1: Write failing decoder tests**

Fixtures:

```jsonl
{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}
```

Expected events:

- `ForeignInit{SessionID:"0199..."}`
- `ForeignStepComplete` with text `done`
- `ForeignTerminalOK`

Add rows for `turn.failed`, `error`, unknown item type, and malformed JSON.

**Step 2: Run and confirm failure**

Run:

```bash
go test ./pkg/foreignloop/codex -run 'Decode|FuzzDecode' -count=1
```

Expected: undefined decoder.

**Step 3: Implement**

Use a top-level typed envelope:

```go
type eventLine struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     json.RawMessage `json:"item"`
	Error    json.RawMessage `json:"error"`
}
```

Use per-item structs for the allowed fields. Return shared `foreignloop.ForeignEvent`s. Unknown event or item types return nil events and nil error. Malformed JSON returns `*foreignloop.DecodeError`.

**Step 4: Run and commit**

Run:

```bash
go test -race ./pkg/foreignloop/codex -run 'Decode|FuzzDecode' -count=1
```

Expected: PASS.

Commit:

```bash
git add pkg/foreignloop/codex/decode.go pkg/foreignloop/codex/decode_test.go pkg/foreignloop/codex/decode_fuzz_test.go
git commit -m "feat(foreignloop): decode Codex exec JSONL"
```

## Phase 5: Codex Subprocess Adapter

### Task 8: Implement `codex.Agent.Spawn`

**Files:**
- Create: `pkg/foreignloop/codex/codex.go`
- Test: `pkg/foreignloop/codex/codex_test.go`

**Step 1: Write tests with a fake executable**

Create a test helper shell script in `t.TempDir()` that prints JSONL to stdout and records argv/env to files. Tests:

- first turn calls `codex exec --json ...`
- resume turn calls `codex exec resume --json <sid> ...`
- stdout JSONL is decoded into foreign events.
- stderr is drained and does not block.
- `Close` is idempotent.

**Step 2: Run and confirm failure**

Run:

```bash
go test ./pkg/foreignloop/codex -run 'Agent|Spawn' -count=1
```

Expected: undefined `Agent.Spawn`.

**Step 3: Implement**

Model it on `pkg/foreignloop/claude/claude.go`:

- `exec.Command(a.ExecPath, args...)`
- no shell
- `cmd.Dir = t.Cwd`
- `cmd.Env = a.Env`
- stdin empty unless using prompt over stdin fallback
- stdout decoded with the Codex decoder
- stderr drained to a bounded buffer or `io.Discard`
- process group shutdown on Unix

`TranscriptPath()` returns empty for v1 because Codex JSONL is the committed source.

**Step 4: Run and commit**

Run:

```bash
go test -race ./pkg/foreignloop/codex -run . -count=1
```

Expected: PASS.

Commit:

```bash
git add pkg/foreignloop/codex/codex.go pkg/foreignloop/codex/codex_test.go
git commit -m "feat(foreignloop): spawn Codex exec adapter"
```

## Phase 6: Session-Level Proofs

### Task 9: Add fake Codex primary/subagent/restore tests

**Files:**
- Modify: `pkg/session/foreign_e2e_test.go`
- Modify: `pkg/session/foreign_restore_test.go`

**Step 1: Write tests**

Add fake `ForeignAgent` scripts with `Spec{SIDMode: foreignloop.SIDLateBound}`.

Tests:

- Codex-like primary loop publishes `ForeignSessionBound` and `TurnDone`.
- Codex-like subagent returns final text from `RunSubagent`.
- restored Codex-like loop recovers sid from `ForeignSessionBound`.
- restore fails closed when `EngineForeignCodex` has neither `LoopStarted.ForeignSID` nor `ForeignSessionBound`.

**Step 2: Run and confirm failure**

Run:

```bash
go test ./pkg/session -run 'Codex|LateBound|ForeignRestore' -count=1
```

Expected: one or more failures until shared actor and restore are fully wired.

**Step 3: Implement missing wiring**

Fix any session code paths that still assume foreign sid is present on `LoopStarted`.

**Step 4: Run and commit**

Run:

```bash
go test -race ./pkg/session -run 'Codex|LateBound|ForeignRestore|ForeignPrimaryE2E|ForeignSubagentE2E' -count=1
```

Expected: PASS.

Commit:

```bash
git add pkg/session/foreign_e2e_test.go pkg/session/foreign_restore_test.go
git commit -m "test(session): prove Codex-style late-bound foreign loops"
```

## Phase 7: Real CLI Contract Tests

### Task 10: Add opt-in Codex CLI contract tests

**Files:**
- Create: `pkg/foreignloop/codex/codex_integration_test.go`

**Step 1: Write skipped tests**

Tests should skip unless `LOOPRIG_CODEX_INTEGRATION=1`.

Contract checks:

- `codex exec --json --sandbox read-only -c approval_policy="never"` emits
  `thread.started` when the credentialed live contract is run.
- live `codex exec resume <thread_id> --json <prompt>` resumes the same id or
  clearly confirms continuation; a separate exact argv unit test covers the
  adapter's `codex exec resume --json <foreign_sid> <prompt>` invocation.
- parser-probe `--cd`, `--sandbox`, and `--add-dir` before and after `resume`
  with `--help`; if a flag has no valid placement, stop before live commands.
- independently pass the exact production start argv, including
  `-c approval_policy="never"`, to `codex exec --help` under temporary `HOME` and
  `CODEX_HOME` directories with a minimal sanitized environment. This parser-only
  check must not inherit credentials or user config and must require help output,
  guaranteeing no model or network invocation.

Historical note: the original draft used `--ask-for-approval`. Codex CLI 0.144.0
rejects that legacy flag, so production start argv uses the supported config
override instead.

**Step 2: Run normal tests**

Run:

```bash
go test -race ./pkg/foreignloop/codex -run Integration -count=1
```

Expected: SKIP without env var.

**Step 3: Run opt-in tests locally**

The isolated parser check requires the opt-in gate but no login or API key:

```bash
LOOPRIG_CODEX_INTEGRATION=1 go test -race ./pkg/foreignloop/codex -run '^TestIntegrationCodexProductionStartArgsParse$' -count=1 -v
```

Run the credentialed live start/resume contract only when a Codex login/API key
is available:

```bash
LOOPRIG_CODEX_INTEGRATION=1 go test -race ./pkg/foreignloop/codex -run Integration -count=1 -v
```

Expected: the parser-only check passes without model/network access; the
credentialed live check either passes or reports an actionable CLI contract
mismatch when explicitly run.

**Step 4: Commit**

```bash
git add pkg/foreignloop/codex/codex_integration_test.go
git commit -m "test(foreignloop): add opt-in Codex CLI contract tests"
```

## Final Verification

Run:

```bash
make test
make secure
```

These are the plan's prescribed final commands, not evidence that both were run.

If integration credentials are available, also run:

```bash
LOOPRIG_CODEX_INTEGRATION=1 go test -race ./pkg/foreignloop/codex -run Integration -count=1 -v
```

Expected when explicitly run with credentials: PASS.

Reviewed implementation verification notes:

- process exit, decode failure, and premature EOF publish `TurnFailed`, never
  `TurnDone`;
- a late-bound terminal before non-empty `ForeignInit` publishes `TurnFailed`;
  terminal-error-before-init retains both typed `ForeignResultError` and
  `ForeignProtocolError`, and combined close failures retain their typed causes
  through `errors.As`;
- late-bound resume begins only after SID binding; failures, interruption, and
  EOF before binding leave `hasSpawned` false so the next submit uses `StartNew`
  with an empty SID; prebound and successfully bound loops resume;
- focused race repetitions, the full foreign-loop race suite, `go vet`, and diff
  checks were run during review;
- the isolated exact production-start parser check ran under sanitized temporary
  config with `--help`; no credentialed live start/resume integration or
  successful `make secure` run is claimed here.

Then commit any final doc adjustments:

```bash
git status --short
git add docs/plans/2026-07-06-codex-foreign-loop-design.md docs/plans/2026-07-06-codex-foreign-loop-implementation.md
git commit -m "docs: update Codex foreign loop implementation notes"
```
