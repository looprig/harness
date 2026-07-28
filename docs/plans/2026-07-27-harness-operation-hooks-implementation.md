# Harness Operation Hooks Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add deterministic, fail-closed guards and exactly-once around hooks for Harness operations without duplicating the durable event stream.

**Architecture:** A new immutable `pkg/hook.Runner` compiles typed registrations at rig definition time. The runner is threaded through the lifecycle, session, native loop, and journal wrapper; runtime boundaries call `Start`, execute guards, and finish in reverse order with one terminal snapshot. Only guard policy revision enters `ConfigManifest`; observation-only around hooks remain operational configuration.

**Tech Stack:** Go 1.26.4, existing Harness event/identity/loop/tool/gate packages, standard `context`, `sync`, `log/slog`, table-driven tests, `go test -race`.

---

## Working Rules

- Apply `@superpowers:test-driven-development` to every task.
- Do not alter durable event ordering or event classification.
- Do not add shell/HTTP/config-file hook support.
- Do not add request, argument, result, or message mutation.
- Keep the hook runner immutable after `rig.Define`.
- Run `git diff --check` before every commit.
- Apply `@superpowers:verification-before-completion` before the final commit.

### Task 1: Define the public hook contracts and validation

**Files:**

- Create: `pkg/hook/hook.go`
- Create: `pkg/hook/data.go`
- Create: `pkg/hook/errors.go`
- Create: `pkg/hook/clone.go`
- Create: `pkg/hook/hook_test.go`

**Step 1: Write failing contract and validation tests**

Cover:

- all eight `Operation` values are valid;
- only turn, inference, compaction, and tool-call operations are guardable;
- empty sets validate;
- nil callbacks and unknown operations fail;
- guards require a non-empty policy revision;
- a policy revision without guards fails;
- `Deny` validates bounded code/reason input;
- `Call` requires exactly one payload matching its operation;
- cloned calls do not alias JSON, message blocks, inference requests, compaction
  transcripts, or terminal results.

Use a table such as:

```go
func TestValidateSet(t *testing.T) {
	tests := []struct {
		name string
		set  hook.Set
		ok   bool
	}{
		{name: "empty", set: hook.Set{}, ok: true},
		{
			name: "guard needs revision",
			set: hook.Set{Guards: []hook.Guard{{
				Operation: hook.OperationToolCall,
				Check:     func(context.Context, hook.Call) error { return nil },
			}}},
		},
		{
			name: "around needs no revision",
			set: hook.Set{Around: []hook.Around{{
				Operation: hook.OperationInference,
				Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
					return ctx, nil
				},
			}}},
			ok: true,
		},
	}
	// Assert errors.As(err, *hook.ConfigError) on rejected cases.
}
```

**Step 2: Run tests and confirm the package does not yet exist**

Run:

```bash
GOWORK=off go test ./pkg/hook
```

Expected: FAIL because `pkg/hook` has not been implemented.

**Step 3: Implement the public contract**

Define:

```go
type Operation uint8

const (
	OperationTurn Operation = iota + 1
	OperationStep
	OperationInference
	OperationCompaction
	OperationToolCall
	OperationGateWait
	OperationToolExecution
	OperationJournalAppend
)

type Outcome uint8

const (
	OutcomeCompleted Outcome = iota + 1
	OutcomeDenied
	OutcomeFailed
	OutcomeCanceled
)

type StepIndex uint64
type RecordFamily string

const (
	RecordEvent        RecordFamily = "event"
	RecordCommand      RecordFamily = "command"
	RecordGatePrepared RecordFamily = "gate_prepared"
	RecordFence        RecordFamily = "fence"
)

type GuardFunc func(context.Context, Call) error
type BeginFunc func(context.Context, Call) (context.Context, FinishFunc)
type FinishFunc func(Result)

type Guard struct {
	Operation Operation
	Check     GuardFunc
}

type Around struct {
	Operation Operation
	Begin     BeginFunc
}

type Set struct {
	PolicyRevision string
	Guards         []Guard
	Around         []Around
}
```

Define `Call`, `Result`, and the eight typed payloads from the approved design.
Use `json.RawMessage` for tool arguments, existing public domain types for
inference/compaction/gate data, and bounded primitive fields for journal data.
Do not expose internal runner structs.

Implement:

```go
func ValidateSet(Set) error
func ValidateCall(Call) error
func CloneCall(Call) Call
func CloneResult(Result) Result
func Deny(code, reason string) error
```

Use typed `ConfigError`, `CallError`, and `Denial`. Set conservative denial
bounds in named constants and reject blank codes, whitespace-only reasons,
control characters, or values above the bounds.

Copy the ownership logic currently proven by
`internal/loopruntime/message_clone.go` into dependency-neutral helpers inside
`pkg/hook`; do not import `internal/loopruntime`.

**Step 4: Run focused tests**

Run:

```bash
GOWORK=off go test ./pkg/hook
```

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/hook
git commit -m "feat: define harness operation hook contracts"
```

### Task 2: Implement deterministic compilation and dispatch

**Files:**

- Create: `pkg/hook/runner.go`
- Create: `pkg/hook/runner_test.go`
- Modify: `pkg/hook/errors.go`

**Step 1: Write failing runner tests**

Prove:

- around begins run in registration order;
- each returned context reaches the next begin, guards, and operation;
- matching guards run sequentially;
- first guard error stops later guards;
- a typed denial and an internal error both block;
- a guard panic becomes a typed internal hook error and blocks;
- finishes run in reverse order;
- the aggregate finish function is `sync.Once` protected;
- a begin panic is logged/ignored and contributes no finish;
- a finish panic does not suppress remaining finishes;
- a nil begin context retains the previous valid context;
- every callback gets its own cloned call/result snapshot;
- unrelated dispatches can run concurrently under `go test -race`.

Representative ordering test:

```go
func TestRunnerOrder(t *testing.T) {
	var got []string
	set := hook.Set{
		PolicyRevision: "policy-v1",
		Around: []hook.Around{
			testAround(hook.OperationToolCall, "a", &got),
			testAround(hook.OperationToolCall, "b", &got),
		},
		Guards: []hook.Guard{
			testGuard(hook.OperationToolCall, "g1", &got, nil),
			testGuard(hook.OperationToolCall, "g2", &got, nil),
		},
	}
	runner, err := hook.Compile(set)
	if err != nil {
		t.Fatal(err)
	}
	_, finish, err := runner.Start(context.Background(), validToolCall())
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, "operation")
	finish(hook.Result{Call: validToolCall(), Outcome: hook.OutcomeCompleted})
	assertStrings(t, got, []string{"a.begin", "b.begin", "g1", "g2", "operation", "b.finish", "a.finish"})
}
```

**Step 2: Run tests and verify failure**

Run:

```bash
GOWORK=off go test ./pkg/hook -run 'TestRunner|TestCompile'
```

Expected: FAIL because `Compile` and `Runner.Start` do not exist.

**Step 3: Implement the immutable runner**

Expose:

```go
type Runner struct {
	guards []Guard
	around []Around
}

func Compile(Set) (*Runner, error)

func (r *Runner) Start(
	ctx context.Context,
	call Call,
) (context.Context, FinishFunc, error)
```

Implementation rules:

1. Validate and defensively clone the set in `Compile`.
2. Treat a nil runner as a no-op runner.
3. Validate and clone the call before invoking callbacks.
4. Run matching begins in slice order, recovering each panic separately.
5. Retain the prior context when a callback returns nil.
6. Run matching guards in slice order on the final derived context.
7. Recover a guard panic into `*GuardError`.
8. Stop on the first guard error.
9. Return an aggregate `FinishFunc` even when a guard blocks.
10. Protect the aggregate finish with `sync.Once`.
11. Clone the supplied result separately for every finish.
12. Invoke finishes in reverse order, recovering each panic separately.

Log observation failures through `slog.Default()` with operation and callback
index only. Never log the call payload or raw error-bearing data fields.

**Step 4: Run package tests with the race detector**

Run:

```bash
GOWORK=off go test -race ./pkg/hook
```

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/hook
git commit -m "feat: add deterministic operation hook runner"
```

### Task 3: Fingerprint guard policy and compile hooks in the rig

**Files:**

- Modify: `pkg/event/config_manifest.go`
- Modify: `pkg/event/config_manifest_test.go`
- Modify: `pkg/event/config_manifest_fuzz_test.go`
- Modify: `pkg/event/drift.go`
- Modify: `pkg/event/drift_test.go`
- Modify: `pkg/event/validate.go`
- Modify: `pkg/rig/options.go`
- Modify: `pkg/rig/definition.go`
- Modify: `pkg/rig/errors.go`
- Modify: `pkg/rig/fingerprint.go`
- Modify: `pkg/rig/fingerprint_test.go`
- Create: `pkg/rig/hooks_test.go`

**Step 1: Write failing manifest and rig tests**

Add tests proving:

- `HookPolicyRev` participates in canonical fingerprints;
- manifest schema version and encoding domain advance together;
- hook-policy drift is `Warn`;
- a legacy baseline upgrades safely;
- `rig.WithHooks` is a singleton option;
- invalid sets produce `DefinitionInvalidHooks`;
- the rig accepts around-only hooks without a revision;
- changing around callbacks does not change the manifest;
- changing guard revision changes the manifest fingerprint.

**Step 2: Run focused tests and verify failure**

Run:

```bash
GOWORK=off go test ./pkg/event ./pkg/rig -run 'Hook|Manifest|Drift'
```

Expected: FAIL because no hook-policy manifest field or rig option exists.

**Step 3: Update the manifest contract**

Make these exact changes:

```go
const ManifestSchemaVersion uint32 = 2
const manifestEncodingDomain = "looprig/config-manifest/v2"

type ConfigManifest struct {
	// existing fields...
	HookPolicyRev string `json:"hook_policy_rev,omitzero"`
}
```

Append `HookPolicyRev` in a fixed position in `canonical()`. Add a
`DriftHookPolicy` category and compare revisions at `DriftWarn`. Leave
`ManifestFromLegacy` with an empty hook revision. Update validation budgets and
all schema/golden tests.

**Step 4: Add rig compilation**

Add `keyHooks`, raw `hook.Set` storage, and compiled `*hook.Runner` storage to
`definitionState`. Implement:

```go
func WithHooks(set hook.Set) Option
```

The option defensively captures the set. `Define` calls `hook.Compile` after all
options resolve, wraps failures as `DefinitionInvalidHooks`, sets
`manifest.HookPolicyRev`, and passes the compiled runner into the lifecycle.
Do not add hook policy to legacy `ConfigFingerprint`.

**Step 5: Run focused tests**

Run:

```bash
GOWORK=off go test ./pkg/event ./pkg/rig
```

Expected: PASS.

**Step 6: Commit**

```bash
git add pkg/event pkg/rig
git commit -m "feat: compile and fingerprint hook guard policy"
```

### Task 4: Thread the immutable runner through lifecycle, session, and journal

**Files:**

- Modify: `internal/sessionruntime/lifecycle.go`
- Modify: `internal/sessionruntime/session.go`
- Modify: `internal/sessionruntime/restore_constructor.go`
- Create: `internal/sessionruntime/hooks.go`
- Modify: `internal/sessionruntime/composition_options_test.go`
- Create: `pkg/journal/hooked.go`
- Create: `pkg/journal/hooked_test.go`
- Modify: `internal/loopruntime/config.go`
- Modify: `internal/loopruntime/loop.go`
- Modify: `internal/loopruntime/restored.go`
- Create: `internal/loopruntime/hooks_test.go`

**Step 1: Write failing plumbing tests**

Prove:

- `WithLifecycleHooks` stores the same immutable compiled runner;
- both `NewSession` and `RestoreSession` receive it;
- every native loop receives it, including spawned and restored loops;
- foreign builders do not receive or execute native operation hooks;
- a journal wrapper emits one `OperationJournalAppend` around every underlying
  append and preserves sequence/error values;
- event, command, gate-prepared, and fence records report the right bounded
  `RecordFamily` and idempotency id;
- a journal around-hook failure cannot change append behavior.

**Step 2: Run focused tests and verify failure**

Run:

```bash
GOWORK=off go test ./pkg/journal ./internal/sessionruntime ./internal/loopruntime -run 'Hook|HookedJournal'
```

Expected: FAIL because the runner is not wired.

**Step 3: Add lifecycle/session options**

Add:

```go
func WithLifecycleHooks(runner *hook.Runner) LifecycleOption
func WithHooks(runner *hook.Runner) Option
```

Store the runner on `Lifecycle` and `Session`. Append the session option on both
new and restore construction paths. Keep the runner out of hustle configuration.

**Step 4: Add a single journal decorator**

Implement:

```go
func WithHooks(
	j SessionJournal,
	runner *hook.Runner,
	sessionID uuid.UUID,
) SessionJournal
```

The returned private wrapper calls `runner.Start` around exactly one delegated
`Append`. Derive record family through the sealed `JournalRecord` type switch,
use `IdempotencyID()` as bounded identity, preserve the underlying `(seq, err)`
exactly, and classify cancellation via `ctx.Err()`.

Install the wrapper after offload-GC admission wrapping and before building the
event/command/gate appenders in new and restore paths.

**Step 5: Add native-loop runtime dependencies**

Add an internal dependency value rather than growing every public constructor:

```go
type RuntimeDependencies struct {
	Compactor Compactor
	Hooks     *hook.Runner
}

func NewInModeWithRuntime(/* existing identity args */, deps RuntimeDependencies) (*Loop, error)
func NewRestoredWithRuntime(/* existing identity args */, deps RuntimeDependencies) (*Loop, error)
```

Keep existing constructors as compatibility wrappers. Store `Hooks` in
`runtimeConfig`; session runtime uses the new constructors for native loops.

**Step 6: Run focused tests**

Run:

```bash
GOWORK=off go test -race ./pkg/journal ./internal/sessionruntime ./internal/loopruntime -run 'Hook|HookedJournal'
```

Expected: PASS.

**Step 7: Commit**

```bash
git add pkg/journal internal/sessionruntime internal/loopruntime
git commit -m "feat: wire operation hooks through session runtime"
```

### Task 5: Instrument turn, step, and inference operations

**Files:**

- Modify: `internal/loopruntime/loop.go`
- Modify: `internal/loopruntime/turn.go`
- Modify: `internal/loopruntime/step.go`
- Create: `internal/loopruntime/turn_hooks_test.go`
- Create: `internal/loopruntime/step_hooks_test.go`
- Create: `internal/loopruntime/inference_hooks_test.go`

**Step 1: Write failing boundary tests**

Prove:

- a turn around hook begins after turn-id minting but before `TurnStarted`;
- a turn denial publishes no false `TurnStarted` and never calls inference;
- a turn finishes once after its durable terminal;
- a step begins after step-id minting and finishes after commit/discard;
- an inference hook sees the exact cloned provider request;
- an inference denial never calls `Client.Stream`;
- provider open, stream-consumption, cancellation, and panic paths finish once;
- the around-derived context reaches `Client.Stream`;
- turn → step → inference begin/finish nesting is correct.

**Step 2: Run focused tests and verify failure**

Run:

```bash
GOWORK=off go test ./internal/loopruntime -run 'TurnHooks|StepHooks|InferenceHooks'
```

Expected: FAIL because these boundaries do not dispatch hooks.

**Step 3: Add a small internal result helper**

Create package-private helpers in `internal/loopruntime`:

```go
func hookOutcome(ctx context.Context, err error) hook.Outcome
func finishHook(finish hook.FinishFunc, call hook.Call, outcome hook.Outcome, err error)
```

Use `OutcomeDenied` only for `*hook.Denial`, `OutcomeCanceled` for context
cancel/deadline, and `OutcomeFailed` for other errors.

**Step 4: Instrument turn admission and terminal completion**

In `startTurnWithIDAndAdmission`, construct the immutable turn call before the
opening commit, call `Hooks.Start`, and use the derived context for the turn.
When a guard blocks:

- release admission exactly once;
- emit the existing typed rejection/failure path selected by the actor;
- do not publish `TurnStarted`;
- finish the around scope once.

Carry the finish callback in the actor's active-turn state so the owner that
durably handles the terminal performs the finish. Do not finish merely when
`runTurn` returns if the durable terminal has not committed.

**Step 5: Instrument step and inference separately**

Wrap the complete step lifecycle in `runTurn`, including tool execution and
`StepDone` acknowledgement. Wrap only `cfg.client.Stream` plus stream
consumption/assembly in `runStep`. Add the hook runner to `turnConfig` and
`stepConfig` explicitly; do not use package globals or ambient context values.

Map inference guard failure into the existing `TurnFailed` path with a safe
typed wrapper; do not expose arbitrary guard errors to model-visible text.

**Step 6: Run focused and existing lifecycle tests**

Run:

```bash
GOWORK=off go test -race ./internal/loopruntime -run 'TurnHooks|StepHooks|InferenceHooks|Boundary|OutputLifecycle'
```

Expected: PASS.

**Step 7: Commit**

```bash
git add internal/loopruntime
git commit -m "feat: hook turn step and inference operations"
```

### Task 6: Instrument tool calls, gate waits, and tool execution

**Files:**

- Modify: `internal/loopruntime/runner.go`
- Modify: `internal/loopruntime/gate.go`
- Modify: `internal/loopruntime/turn.go`
- Create: `internal/loopruntime/tool_hooks_test.go`
- Create: `internal/loopruntime/gate_hooks_test.go`
- Modify: `internal/loopruntime/runner_test.go`
- Modify: `internal/loopruntime/runner_access_test.go`

**Step 1: Write failing tool boundary tests**

Prove:

- every requested tool block gets one semantic `ToolCall` scope;
- a tool guard runs before lookup, preparation, permission, or execution;
- denial produces a bounded model-visible error and the normal ephemeral audit
  pair without opening a gate;
- unknown tools and invalid arguments finish as normalized completed outcomes;
- `GateWait` measures only actual approval/user-input waiting;
- `ToolExecution` begins after permission and ends after middleware/tool return;
- tool execution duration excludes gate latency;
- serial and parallel tool calls preserve per-call nesting;
- panics become normalized results and finish both execution and tool-call scopes;
- cancellation finishes all begun scopes once.

**Step 2: Run focused tests and verify failure**

Run:

```bash
GOWORK=off go test ./internal/loopruntime -run 'ToolHooks|GateHooks'
```

Expected: FAIL because `RunBatch` has no runner or operation identity.

**Step 3: Pass a typed batch hook dependency**

Replace positional growth with:

```go
type BatchRuntime struct {
	GateRegistrations chan<- gateRegistration
	IDGen             func() (uuid.UUID, error)
	Emit              func(event.Event)
	Hooks             *hook.Runner
	Coordinates       identity.Coordinates
	AgentName         identity.AgentName
	Cause             identity.Cause
}

func RunBatch(ctx context.Context, calls []content.ToolUseBlock, tools ToolSet, runtime BatchRuntime) []result
```

Update tests and the single production call in `turn.go`.

**Step 4: Instrument semantic tool calls**

Start `OperationToolCall` before `newResolved`. A guard denial must create a
`resolved` failure without calling `lookupTool`, `PrepareCall`, access
authorization, or tool code. Store each tool call's aggregate finish function on
its private `resolved` value and finish it from the existing `complete`
chokepoint after `ToolCallCompleted`.

Do not classify normalized tool failures as hook infrastructure failures.
Expose raw arguments only in the trusted `ToolCallData.ArgsJSON`; continue using
existing redacted summary/preview fields for logs.

**Step 5: Instrument waits and execution**

Wrap only the blocking reply selects in `approvalRequesterFor` and
`RequestUserInput` with `OperationGateWait`. Wrap `runOne` immediately around
the middleware/tool invocation with `OperationToolExecution`.

Keep gate creation, durable activation, and response routing unchanged.

**Step 6: Run focused and existing runner tests**

Run:

```bash
GOWORK=off go test -race ./internal/loopruntime -run 'Runner|Access|ToolHooks|GateHooks|Prepared'
```

Expected: PASS.

**Step 7: Commit**

```bash
git add internal/loopruntime
git commit -m "feat: hook tool gate and execution operations"
```

### Task 7: Instrument compaction without changing its durable protocol

**Files:**

- Modify: `internal/loopruntime/compaction_control.go`
- Modify: `internal/loopruntime/compaction_executor.go`
- Modify: `internal/loopruntime/compaction_finalization.go`
- Modify: `internal/loopruntime/loop.go`
- Create: `internal/loopruntime/compaction_hooks_test.go`
- Modify: `internal/loopruntime/compaction_control_loop_test.go`
- Modify: `internal/loopruntime/compaction_finalization_test.go`

**Step 1: Write failing compaction tests**

Prove:

- the operation starts after attempt id and input basis are frozen;
- a guard denial never invokes the compactor;
- denial uses the existing `CompactionRejected` and waiter outcome protocol;
- successful compaction finishes only after the canonical terminal append;
- invalid output, counter failure, finalization failure, cancellation, and panic
  finish once with the correct outcome;
- retries/idempotent finalization do not double-finish;
- the derived context reaches `CompactAndFinalize`.

**Step 2: Run focused tests and verify failure**

Run:

```bash
GOWORK=off go test ./internal/loopruntime -run 'CompactionHooks'
```

Expected: FAIL because compaction does not dispatch hooks.

**Step 3: Carry one operation scope with the compaction attempt**

Add private context/finish ownership to the live executor obligation, not to the
durable `compactionAttempt` event projection. Start the hook after the candidate
input is frozen and before `CompactionStarted`/`CompactAndFinalize`.

On guard denial, build a typed internal rejection that maps to the existing
`CompactReject...` value without introducing a new durable event type.

Transfer finish ownership to the finalizer and call it only after canonical
terminal publication succeeds or after a terminal infrastructure failure makes
success impossible. Guard with `sync.Once` so callback retries are idempotent.

**Step 4: Run compaction suites**

Run:

```bash
GOWORK=off go test -race ./internal/loopruntime -run 'Compaction'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/loopruntime
git commit -m "feat: hook compaction operations"
```

### Task 8: Prove end-to-end behavior and document the API

**Files:**

- Create: `pkg/rig/hooks_integration_test.go`
- Modify: `pkg/session/README.md`
- Create: `pkg/hook/README.md`
- Modify: `docs/plans/2026-07-08-harness-execution-hooks-design.md` only if implementation discovered a necessary approved-design clarification

**Step 1: Write failing rig-level integration tests**

Build a real rig and prove:

- registration order and nesting across turn → step → inference → tool-call →
  gate-wait/tool-execution → journal-append;
- guard policy revision is stamped on `SessionStarted.Manifest`;
- around-only rigs stamp an empty policy revision;
- restore detects changed guard policy at `Warn`;
- restore with unchanged guard policy uses the newly supplied immutable runner;
- no hook replays for historical operations;
- native and foreign loop exclusions hold;
- hustles do not trigger inference hooks.

**Step 2: Run the integration test and verify failure**

Run:

```bash
GOWORK=off go test ./pkg/rig -run 'HooksIntegration'
```

Expected: FAIL until all construction and restore paths are correctly connected.

**Step 3: Fix only missing integration plumbing**

Do not expand the public API. Correct missed runner propagation, finish
ownership, or manifest stamping discovered by the test.

**Step 4: Add concise documentation**

`pkg/hook/README.md` must show:

```go
hooks := hook.Set{
	PolicyRevision: "tool-safety-v3",
	Guards: []hook.Guard{{
		Operation: hook.OperationToolCall,
		Check: func(ctx context.Context, call hook.Call) error {
			if blocked(call.ToolCall) {
				return hook.Deny("unsafe_tool", "tool call rejected by policy")
			}
			return nil
		},
	}},
	Around: []hook.Around{{
		Operation: hook.OperationInference,
		Begin: startInferenceSpan,
	}},
}

rig.Define(/* existing options */, rig.WithHooks(hooks))
```

Document concurrency, trusted-data, redaction, fail-closed guards, panic
isolation, exact finish pairing, and the event-vs-hook distinction.

**Step 5: Run package and integration tests**

Run:

```bash
GOWORK=off go test -race ./pkg/hook ./pkg/event ./pkg/journal ./pkg/rig ./internal/loopruntime ./internal/sessionruntime
```

Expected: PASS.

**Step 6: Commit**

```bash
git add pkg/hook pkg/session pkg/rig
git commit -m "docs: document harness operation hooks"
```

### Task 9: Full verification and handoff

**Files:**

- Modify only files required by verification failures attributable to this feature.

**Step 1: Format all changed Go files**

Run:

```bash
gofmt -w pkg/hook/*.go \
  pkg/event/config_manifest.go pkg/event/config_manifest_test.go pkg/event/config_manifest_fuzz_test.go pkg/event/drift.go pkg/event/drift_test.go pkg/event/validate.go \
  pkg/journal/hooked.go pkg/journal/hooked_test.go \
  pkg/rig/options.go pkg/rig/definition.go pkg/rig/errors.go pkg/rig/fingerprint.go pkg/rig/fingerprint_test.go pkg/rig/hooks_test.go pkg/rig/hooks_integration_test.go \
  internal/loopruntime/config.go internal/loopruntime/loop.go internal/loopruntime/restored.go internal/loopruntime/runner.go internal/loopruntime/gate.go internal/loopruntime/turn.go internal/loopruntime/step.go internal/loopruntime/compaction_control.go internal/loopruntime/compaction_executor.go internal/loopruntime/compaction_finalization.go internal/loopruntime/*_hooks_test.go \
  internal/sessionruntime/lifecycle.go internal/sessionruntime/session.go internal/sessionruntime/restore_constructor.go internal/sessionruntime/hooks.go internal/sessionruntime/composition_options_test.go
```

Expected: command exits 0.

**Step 2: Run repository tests with race detection**

Run:

```bash
GOWORK=off go test -race ./...
```

Expected: PASS.

**Step 3: Run static analysis**

Run:

```bash
GOWORK=off go vet ./...
```

Expected: PASS.

**Step 4: Run repository lint**

Run the repository's checked lint target:

```bash
GOWORK=off GOPRIVATE='github.com/ciram-co/*' GOSUMDB=off GOCACHE=/private/tmp/harness-hooks-gocache make lint
```

Expected: PASS.

**Step 5: Review the final diff**

Run:

```bash
git diff --check
git status --short
git log --oneline --decorate -12
```

Expected:

- no whitespace errors;
- only planned hook/manifest/runtime/docs changes;
- one focused commit per completed task.

**Step 6: Request code review**

Apply `@superpowers:requesting-code-review` to the complete branch. Address only
verified findings with `@superpowers:receiving-code-review`.

**Step 7: Commit final verification fixes, if any**

```bash
git add <verified-fix-files>
git commit -m "fix: address operation hooks verification"
```

Do not create an empty commit when no fixes were necessary.
