# Rig Lifecycle and Workspace Snapshots Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace `session.Compile`/`session.Runner` with one immutable `rig.Define` composition path that creates and restores multi-loop sessions, supports modes and managed delegation, and owns optional, deterministic workspace placement, snapshot, rewind, and file-freshness policy.

**Architecture:** Add immutable tool and loop definitions first, then move concrete actors into `internal/loopruntime` and `internal/sessionruntime` while leaving `pkg/loop` and `pkg/session` as public contracts. `pkg/rig` becomes the sole lifecycle composition root. One session-scoped workspace coordinator serializes harness-managed mutations against native step/turn/idle checkpoint boundaries; placement strategies isolate or explicitly share roots, and durable events make restore deterministic.

**Tech Stack:** Go 1.26.4 and the standard library; existing `github.com/looprig/core`, `github.com/looprig/inference`, and `github.com/looprig/storage` contracts; existing `pkg/event`, `pkg/hub`, `pkg/journal`, `pkg/sessionstore`, and `pkg/workspacestore`. No new external dependencies. Tests are table-driven, use `-race`, and filesystem/process cases use the existing integration build tag.

**Authoritative spec:** `docs/plans/2026-07-10-rig-lifecycle-workspace-snapshots-design.md`. The folded `2026-07-11-workspace-placement-modes-design.md` is decision history only. When this plan and the spec disagree, the consolidated spec wins.

---

## Execution rules

- Work in a dedicated harness worktree. Use `@superpowers:test-driven-development` for every behavior change and `@superpowers:verification-before-completion` before every commit.
- Preserve the unrelated deletion of `docs/plans/2026-07-08-flow-run-observability-design.md`; never stage it as part of this plan.
- Keep every committed checkpoint green. The package-cutover task is intentionally atomic because the exact public names cannot coexist with the old concrete `session.Session` type.
- Use `GOWORK=off` throughout. Harness's module replacements point at sibling `../core`, `../inference`, and `../storage`; refresh `vendor/` whenever the storage contract changes.
- No public hooks implement snapshot ordering. The coordinator is an internal lifecycle collaborator invoked synchronously by loop/session boundary code.
- Workspace snapshot GC remains manual. Compaction, budgets, rewind of conversation history, merge machinery, CLI migration, and SWE migration implementation are out of scope.
- Before each harness commit run:

  ```bash
  GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache make secure
  ```

- Before each storage/fsstore commit run the module's leaf gate:

  ```bash
  GOWORK=off GOCACHE=/private/tmp/looprig-storage-gocache make check
  ```

## Current code landmarks

- `pkg/loop/config.go` — exported mutable `loop.Config`, to be replaced by immutable definitions.
- `pkg/loop/loop.go`, `turn.go`, `step.go`, `runner.go` — native actor runtime, to move under `internal/loopruntime`.
- `pkg/session/runner.go` — `Compile`, `Runner.Run`, and `Runner.Restore`, to be deleted after rig cutover.
- `pkg/session/session.go` — concrete session, single primary loop, submit/delegation/interrupt logic.
- `pkg/session/restore_constructor.go` — replay, root-loop discovery, checkpoint materialization.
- `pkg/hub/hub.go` — enduring append/fan-out and derived `SessionActive`/`SessionIdle` edges.
- `pkg/event/event.go`, `marshal.go`, `validate.go` — sealed event union and codec.
- `pkg/sessionstore/catalog.go` — durable catalog fold.
- `pkg/tools/{readfile,writefile,editfile,bash,subagent}.go` — tools requiring per-loop/session binding.
- `pkg/workspacestore` — existing deterministic snapshot/materialize/GC primitives; archive format remains unchanged.
- `pkg/serve/serve.go`, `handlers_lifecycle.go` — narrow `Runner` dependency and Run/Restore names.

## Target dependency direction

```text
pkg/rig ───────────────→ pkg/session (contracts/errors) ─→ pkg/loop (contracts/definitions)
   │                                  ▲                         ▲
   ├→ internal/sessionruntime ────────┘                         │
   │          └→ internal/loopruntime ─────────────────────────┘
   ├→ pkg/sessionstore
   └→ pkg/workspacestore

pkg/tools ─→ pkg/tool + pkg/loop policy contracts
pkg/serve ─→ its own narrow Rig/LiveSession interfaces only
```

---

## Phase A — Additive contracts and durable schema

### Task 1: Add discoverable local persistence paths

**Files:**
- Create: `../storage/paths.go`
- Create: `../storage/paths_test.go`
- Modify: `../fsstore/fsstore.go`
- Modify: `../fsstore/fsstore_test.go`
- Modify: `pkg/sessionstore/sessionstore.go`
- Modify: `pkg/sessionstore/sessionstore_test.go`
- Modify: `pkg/workspacestore/store.go`
- Modify: `pkg/workspacestore/store_test.go`
- Refresh: `vendor/github.com/looprig/storage/`

**Step 1: Write failing contract tests**

Add a small optional storage capability and test that only local providers report paths:

```go
type PathReporter interface {
	StoragePaths() []string
}
```

The storage test must prove callers receive a defensive copy. The fsstore test must prove its canonical root is reported once. Harness tests must prove `sessionstore.Store.PersistencePaths()` and `workspacestore.Store.PersistencePaths()` collect, canonicalize, sort, and deduplicate reporter paths while remote/non-reporting providers return none.

**Step 2: Run tests and verify failure**

```bash
(cd ../storage && GOWORK=off go test -race ./...)
(cd ../fsstore && GOWORK=off go test -race ./...)
GOWORK=off go test -race ./pkg/sessionstore ./pkg/workspacestore
```

Expected: compile failures for the missing interface and methods.

**Step 3: Implement the capability**

Keep `PathReporter` optional; do not widen `Ledger`, `Leaser`, `KV`, or `Blobs`. Have fsstore return its canonical provider root. Harness facades inspect each configured primitive structurally and expose only canonical local paths. Refresh vendor with:

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache go mod vendor
```

**Step 4: Run all affected tests**

Run all three commands from Step 2 and `git diff --check` in storage, fsstore, and harness. Expected: PASS.

**Step 5: Commit in dependency order**

```bash
(cd ../storage && git add paths.go paths_test.go && git commit -m "feat(storage): report discoverable local paths")
(cd ../fsstore && git add fsstore.go fsstore_test.go && git commit -m "feat(fsstore): report canonical storage root")
git add pkg/sessionstore pkg/workspacestore vendor/github.com/looprig/storage
git commit -m "feat(storage): expose persistence paths to rig validation"
```

### Task 2: Add immutable tool definitions and runtime bindings

**Files:**
- Create: `pkg/tool/definition.go`
- Create: `pkg/tool/definition_test.go`
- Create: `pkg/tools/definitions.go`
- Create: `pkg/tools/definitions_test.go`
- Modify: `pkg/tool/deps_test.go`

**Step 1: Write failing tests**

Test immutable definitions, requirement bits, fresh builds, nil binding failures, and no reuse of session-specific tools:

```go
type Definition interface {
	Name() string
	Requirements() Requirements
	Build(context.Context, Bindings) ([]InvokableTool, error)
}

type Bindings struct {
	SessionID uuid.UUID
	LoopID    uuid.UUID
	Workspace *WorkspaceBinding
	Delegate  DelegateController
}

type WorkspaceBinding struct {
	Root        string
	Coordinator WorkspaceCoordinator
}
```

Use typed bit flags `RequiresWorkspace` and `RequiresDelegateController`. Define a narrow coordinator permit contract (`Acquire(ctx, operation, canonicalPath) (WorkspacePermit, error)` plus `Healthy() error`) and one `DelegateController.Execute(ctx, DelegateRequest) (DelegateResult, error)` entrypoint so the model-facing tool never receives a session controller.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/tool ./pkg/tools -run 'TestDefinition|TestBindings'
```

Expected: missing types/functions.

**Step 3: Implement minimally**

Add immutable factory-backed definitions in `pkg/tool`. In `pkg/tools`, add blueprints matching the approved vocabulary:

```go
tools.Files(readGuard)
tools.Bash(opts...)
tools.Subagent()
```

Also provide definition wrappers for existing stateless tools so `loop.WithTools` has one input type. Keep current concrete constructors for package tests/custom low-level tool use, but rig composition accepts definitions only. Every `Build` must return fresh mutable instances.

**Step 4: Verify green**

Run the focused command, then `GOWORK=off go test -race ./pkg/tool ./pkg/tools`. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tool pkg/tools
git commit -m "feat(tool): add immutable runtime-bound definitions"
```

### Task 3: Replace `loop.Config` composition with immutable definitions

**Files:**
- Create: `pkg/loop/definition.go`
- Create: `pkg/loop/definition_test.go`
- Create: `pkg/loop/mode.go`
- Create: `pkg/loop/mode_test.go`
- Create: `pkg/loop/controller.go`
- Create: `pkg/loop/definition_errors.go`
- Modify: `pkg/loop/config.go` (temporary internal compatibility only)
- Modify: `pkg/loop/deps_test.go`

**Step 1: Write failing definition tests**

Cover missing name/inference/model, duplicate singleton options, duplicate modes, invalid initial mode, deep-copy behavior, tool limits, delegate names, delegation style, and exactly one resolved initial mode. The public surface is:

```go
func Define(opts ...Option) (Definition, error)

type ToolLimits struct {
	Iterations int
	Calls      int
	Parallel   int
}

type Mode struct {
	Name         ModeName
	Model        inference.Model
	Effort       inference.Effort
	Tools        []tool.Definition
	ToolLimits   ToolLimits
	Instructions string
}
```

Options include `WithName`, `WithInference`, `WithSystem`, `WithTools`, `WithToolLimits`, `WithEngine`, `WithDrainTimeout`, `WithRuntimeContext`, `WithDelegates`, `WithDelegation`, `WithModes`, and `WithInitialMode`. `Definition.Bind(ctx, tool.Bindings)` returns an immutable read-only `BoundDefinition` used by internal runtimes; no exported mutable config struct is introduced.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/loop -run 'TestDefine|TestMode|TestDefinition'
```

Expected: undefined definition API.

**Step 3: Implement and bridge temporarily**

Implement option resolution with typed errors and defensive copies. Add one temporary internal conversion from `BoundDefinition` to the existing actor `Config`; mark it for deletion in Task 7. Preserve existing foreign-builder, gate, parallel-tool, runtime-context, and fingerprint inputs rather than dropping them during the API rename.
Binding tests must prove modes on one loop reuse its concrete tool instances and private
observations, while every primer, delegate, and restored loop receives a fresh build.

**Step 4: Verify green**

Run the focused command, then all `pkg/loop` tests under `-race`. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/loop
git commit -m "feat(loop): define immutable loops and modes"
```

### Task 4: Add the durable rig, loop-control, and workspace events

**Files:**
- Modify: `pkg/event/event.go`
- Modify: `pkg/event/turn.go`
- Modify: `pkg/event/doc.go`
- Modify: `pkg/event/marshal.go`
- Modify: `pkg/event/validate.go`
- Modify: `pkg/event/header_test.go`
- Modify: `pkg/event/marshal_test.go`
- Modify: `pkg/event/validate_test.go`
- Create: `pkg/event/rig_test.go`
- Create: `pkg/event/rig_fuzz_test.go`

**Step 1: Extend failing event tables**

Add rows for:

```go
ActiveLoopChanged
LoopInferenceChanged
LoopModeChanged
WorkspaceRestored
WorkspaceCheckpointed // add Consistency and Trigger; keep Ref
```

Add `InitialMode`, the initial resolved secret-free model runtime (including context
limits), and a non-zero `LoopKind` to `LoopStarted`. Define
`LoopKindUnknown/Primer/Delegate/Hustle`; zero is legacy-decode-only. Existing legacy
records infer primer versus delegate from root/parent provenance, while current
producers may never emit unknown. `LoopInferenceChanged` and `LoopModeChanged` carry
the resolved runtime selected by the change so replay does not consult a mutable model
catalog.

Reserve event `VisibilityPublic/Internal` independently from class/scope. Zero is
public for legacy compatibility. This task needs no hustle event yet; it pins the
structural visibility seam that the downstream hustle/compaction plan will consume.

Define `SnapshotConsistencyUnknown/Quiescent/Fuzzy` and
`SnapshotTriggerKindUnknown/Manual/Idle/Interrupt/TurnDone/StepDone/Seed`; unknown is
legacy-decode-only and never valid for a newly emitted checkpoint.

Test exact coordinate/cause shapes: checkpoint/restored events are session-scoped; turn/step/idle/interrupt checkpoint causes identify the direct firing event; manual/seed causes are zero.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/event
```

Expected: compile failures/missing codec cases.

**Step 3: Implement the sealed union and codec**

Update the single classification switch, marshal/unmarshal tags, validation profiles, compile-time assertions, and fuzz corpus. Legacy JSON missing new fields decodes to unknown; current producers are validated against emitting unknown.

**Step 4: Verify green and fuzz parser**

```bash
GOWORK=off go test -race ./pkg/event
GOWORK=off go test ./pkg/event -run=^$ -fuzz=FuzzRigEvent -fuzztime=30s
```

Expected: PASS and no panic.

**Step 5: Commit**

```bash
git add pkg/event
git commit -m "feat(event): persist rig topology and workspace transitions"
```

### Task 5: Fold workspace pointers and live refs in session storage

**Files:**
- Modify: `pkg/sessionstore/catalog.go`
- Modify: `pkg/sessionstore/catalog_test.go`
- Modify: `pkg/sessionstore/gc.go`
- Modify: `pkg/sessionstore/gc_test.go`
- Modify: `pkg/session/restore.go` (temporary compatibility fold)
- Modify: `pkg/session/restore_test.go`

**Step 1: Write failing fold tests**

Add `LastCheckpoint CheckpointSummary` and `CurrentWorkspace WorkspacePointer` to catalog metadata. Prove:

```text
checkpoint A → checkpoint B → restore A
LastCheckpoint   = B
CurrentWorkspace = A (source restore)
```

Prove legacy checkpoint fields decode as unknown, and complete manual GC live-set discovery includes refs from both `WorkspaceCheckpointed` and `WorkspaceRestored` across every retained journal.
Under the turn trigger, prove turn-to-checkpoint lookup by journal sequence and by
`Header.Cause.Coordinates.TurnID` select the same checkpoint.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/sessionstore ./pkg/session -run 'Test.*Workspace|Test.*LiveRefs'
```

Expected: missing pointer fields/fold behavior.

**Step 3: Implement deterministic folds**

Use journal sequence and event ID as checkpoint identity; `Ref` remains content identity. Replace the old `lastWorkspaceCheckpoint` helper with an effective-current-workspace fold that honors restore events. Do not make GC automatic.

**Step 4: Verify green**

Run all `pkg/sessionstore` and `pkg/session` tests with `-race`. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/sessionstore pkg/session/restore.go pkg/session/restore_test.go
git commit -m "feat(sessionstore): fold effective workspace pointers"
```

---

## Phase B — Runtime ownership and the rig cutover

### Task 6: Move the native loop actor under `internal/loopruntime`

**Files:**
- Create/move: `internal/loopruntime/{loop,runner,turn,step,block,chunk,gate,restored,runtime_context}.go`
- Create: `internal/loopruntime/toolset.go`
- Move corresponding actor tests from `pkg/loop/` to `internal/loopruntime/`
- Keep/modify: `pkg/loop/{definition,mode,controller,deps,errors,backend,provenance_ctx}.go`
- Modify: `pkg/session/session.go`
- Modify: `pkg/session/restore_constructor.go`
- Modify: `pkg/foreignloop/*.go`
- Modify: `pkg/loop/deps_test.go`

**Step 1: Add failing boundary assertions**

Add dependency tests proving public `pkg/loop` contains definitions/contracts but no exported `New`, `NewRestored`, or concrete actor type, and `internal/loopruntime` imports `pkg/loop` rather than the reverse.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/loop ./internal/loopruntime ./pkg/session ./pkg/foreignloop
```

Expected: missing internal package and forbidden constructor findings.

**Step 3: Move the actor and adapt construction**

`internal/loopruntime.New` consumes `loop.BoundDefinition`, IDs, non-zero loop kind,
provenance, publisher, and restore seed. Move the mutable resolved `ToolSet` and runaway
counters into the internal runtime; keep only permission/read-policy contracts in
public `pkg/loop`. Keep `loop.Backend` as the common native/foreign runtime contract.
Update foreign builders to accept the immutable bound definition instead of
`loop.Config`. Delete the temporary compatibility conversion from Task 3. This plan
constructs only primer/delegate kinds; `LoopKindHustle` construction belongs to the
downstream hustle implementation plan.

**Step 4: Verify green**

Run the command from Step 2, then `GOWORK=off go test -race ./...`. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/loop internal/loopruntime pkg/session pkg/foreignloop
git commit -m "refactor(loop): move actor runtime under internal"
```

### Task 7: Atomically introduce rig and move session runtime internal

**Files:**
- Create: `pkg/rig/definition.go`
- Create: `pkg/rig/options.go`
- Create: `pkg/rig/errors.go`
- Create: `pkg/rig/lifecycle.go`
- Move: `pkg/session/config_fingerprint.go` to `pkg/rig/fingerprint.go`
- Move: `pkg/session/config_fingerprint_test.go` to `pkg/rig/fingerprint_test.go`
- Create: `pkg/rig/rig_test.go`
- Create/move: `internal/sessionruntime/*.go` and existing session-runtime tests
- Replace: `pkg/session/session.go` with public contracts
- Create: `pkg/session/errors.go`
- Create: `pkg/session/contracts_test.go`
- Modify: `pkg/ceiling/state.go`
- Modify: `pkg/ceiling/state_test.go`
- Modify: `pkg/command/security_ceiling.go`
- Modify: `pkg/event/security_ceiling.go`
- Delete: `pkg/session/runner.go`, `pkg/session/runner_test.go`
- Delete public constructors/options after internal equivalents are wired
- Modify: `pkg/serve/serve.go`
- Modify: `pkg/serve/handlers_lifecycle.go`
- Modify: `pkg/serve/handlers_lifecycle_test.go`
- Modify: `pkg/serve/deps_test.go`

**Step 1: Write the failing public-contract tests**

Pin the intended surface:

```go
type Session interface {
	SessionID() uuid.UUID
	ActiveLoop() loop.Handle
	Loop(uuid.UUID) (loop.Handle, bool)
	Submit(context.Context, []content.Block) (uuid.UUID, error)
	SubmitToLoop(context.Context, uuid.UUID, []content.Block) (uuid.UUID, error)
	SubscribeEvents(event.EventFilter) (event.Subscription, error)
	RespondGate(context.Context, gate.GateResponse) error
	Interrupt(context.Context) (bool, error)
}

type SessionController interface {
	Session
	SetActiveLoop(context.Context, uuid.UUID) error
	LoopController(uuid.UUID) (loop.Controller, bool)
	SetSecurityCeiling(context.Context, ceiling.Level) error
	CheckpointWorkspace(context.Context) (workspacestore.Ref, error)
	RestoreWorkspace(context.Context, workspacestore.Ref) error
	Shutdown(context.Context) error
}
```

Add a minimal rig test: one definition + one primer + session store → `Define`, `NewSession`, `Shutdown`, `RestoreSession`. Add negative compile guards for `session.New`, `session.Restore`, `session.Compile`, and `session.Runner`.

Introduce the named ordinal `ceiling.Level` and migrate the existing command, event,
state, and controller APIs from bare `uint8`; this keeps the final controller signature
strictly typed without changing its wire representation.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/rig ./pkg/session ./pkg/serve ./internal/sessionruntime
```

Expected: missing rig/contracts and old-surface guard failures.

**Step 3: Perform the atomic cutover**

Move concrete session construction, replay, gates, limits, quiescence, and shutdown into `internal/sessionruntime`. Keep public typed errors in `pkg/session`; internal code returns them. Add the minimal immutable `rig.Define` graph and lifecycle wiring over the existing single-root behavior, returning `session.SessionController`.

At the same time rename serve's narrow factory:

```go
type Rig[S LiveSession] interface {
	NewSession(context.Context) (S, error)
	RestoreSession(context.Context, uuid.UUID) (S, error)
}
```

Read the ID from `LiveSession.SessionID()`. Preserve HTTP routes, payloads, idempotency, and registry behavior. This is one commit because exposing both lifecycle roots is explicitly forbidden.

**Step 4: Verify the cutover**

```bash
GOWORK=off go test -race ./pkg/rig ./pkg/session ./pkg/serve ./internal/sessionruntime
GOWORK=off go test -race ./...
```

Expected: PASS; `rg -n 'session\.(Compile|Runner|New|Restore)|serve\.Runner' --glob '*.go' --glob '!vendor/**' .` returns only negative dependency tests or migration documentation.

**Step 5: Commit**

```bash
git add pkg/ceiling pkg/command pkg/event pkg/rig pkg/session pkg/serve internal/sessionruntime
git commit -m "feat(rig): replace session runner lifecycle"
```

### Task 8: Build multi-primer topology and active-loop routing

**Files:**
- Modify: `pkg/rig/definition.go`
- Modify: `pkg/rig/options.go`
- Modify: `pkg/rig/errors.go`
- Modify: `pkg/rig/rig_test.go`
- Modify: `internal/sessionruntime/session.go`
- Create: `internal/sessionruntime/topology.go`
- Create: `internal/sessionruntime/topology_test.go`
- Modify: `internal/sessionruntime/restore.go`
- Modify: `pkg/rig/fingerprint.go`

**Step 1: Write graph and routing tests**

Cover additive `WithLoops`, primers, single-primer active default, required active primer for multiple primers, unknown/duplicate/unreferenced definitions, primer root provenance, independent histories, `Submit` active routing, explicit `SubmitToLoop`, durable `ActiveLoopChanged`, invalid-target atomicity, restore of every live loop, and rig fingerprint order.

Pin the exact topology options:

```go
rig.WithLoops(definitions...)
rig.WithPrimers(names...)
rig.WithActivePrimer(name)
rig.WithDelegationLimits(rig.DelegationLimits{Depth: 2, Quota: 64})
```

Delegation depth/quota remain session-lifetime limits and never substitute for the
per-turn `loop.ToolLimits`.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/rig ./internal/sessionruntime -run 'Test.*Primer|Test.*ActiveLoop|Test.*Topology'
```

Expected: single-root assumptions fail.

**Step 3: Implement the topology registry**

Rig validates/freeze-copies definitions and graph edges. Session runtime owns a flat live-loop registry plus parent provenance, instantiates every primer, records each `LoopStarted`, and makes active-loop changes visible only after `ActiveLoopChanged` commits. Restore reconstructs the full graph; it never invents missing primers.

**Step 4: Verify green**

Run focused tests, then `go test -race ./pkg/rig ./internal/sessionruntime`. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/rig internal/sessionruntime
git commit -m "feat(rig): support primers and active-loop routing"
```

### Task 9: Add next-turn loop modes and inference changes

**Files:**
- Create: `pkg/command/loop_change.go`
- Create: `pkg/command/loop_change_test.go`
- Modify: `internal/loopruntime/loop.go`
- Modify: `internal/loopruntime/turn.go`
- Create: `internal/loopruntime/change_test.go`
- Modify: `internal/sessionruntime/topology.go`
- Modify: `internal/sessionruntime/restore.go`
- Modify: `pkg/loop/controller.go`

**Step 1: Write failing actor tests**

Prove mode and model/effort changes are validated and committed atomically, take effect only at the next turn boundary, preserve loop ID/history, emit `LoopModeChanged` or `LoopInferenceChanged`, reject exited/shutting-down loops, and restore last-write-wins. Modes can change only predeclared tools/instructions/models; direct `Change` can alter only model and effort.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/command ./pkg/loop ./internal/loopruntime ./internal/sessionruntime -run 'Test.*Mode|Test.*Inference|Test.*Change'
```

Expected: missing commands/controller behavior.

**Step 3: Implement actor-owned changes**

Capture one immutable effective mode/model/tool set when a turn starts. Commands validate against the bound definition, append the enduring event, then update actor state. `loop.Controller` is a session-owned capability wrapper; callers never mutate actor fields.

**Step 4: Verify green**

Run the focused command, then all affected package tests. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/command pkg/loop internal/loopruntime internal/sessionruntime
git commit -m "feat(loop): change modes and inference at turn boundaries"
```

### Task 10: Replace synchronous spawning with the scoped managed Subagent tool

**Files:**
- Modify: `pkg/tool/definition.go`
- Modify: `pkg/tools/subagent.go`
- Modify: `pkg/tools/subagent_test.go`
- Create: `internal/sessionruntime/delegation.go`
- Create: `internal/sessionruntime/delegation_test.go`
- Modify: `internal/sessionruntime/session.go`
- Modify: `internal/sessionruntime/restore.go`
- Modify: `pkg/rig/definition.go`
- Modify: `pkg/rig/rig_test.go`

**Step 1: Write failing schema/controller tests**

Test strict action envelopes for `start`, `send`, `wait`, `interrupt`, and `status`; `timeout_seconds` integer validation; omitted action/wait synchronous defaults; optional initial mode; sync-only versus managed schemas; one parent-scoped controller per loop; ownership rejection; depth/quota; and no tool when no delegates.

An omitted timeout waits without a timer but remains interruptible by the parent turn;
a supplied timeout returns a typed timed-out request result. `status` returns only bounded
mechanical state and pending-request counts—never a raw event cursor or child transcript.

Test request correlation: `send` always queues a distinct non-folding child turn with a minted request ID; `wait:true` resolves only that turn's `TurnDone.Message`/failed/interrupted terminal; `wait:false` followed by `wait` resolves the same request, including after restore. Histories remain separate.

Also reject absent-required/zero request IDs, unknown modes before quota reservation,
unauthorized agents/modes, and attempts to exceed the parent/session permission ceiling.
The selected start mode is carried by `LoopStarted` without a synthetic
`LoopModeChanged`; cumulative quota and request ownership survive restore. A live delegate
may become the session's active loop through the trusted controller.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/tools ./pkg/rig ./internal/sessionruntime -run 'Test.*Subagent|Test.*Delegate'
```

Expected: old two-field synchronous schema and spawner behavior fail.

**Step 3: Implement one action surface**

Use `tool.DelegateController.Execute` as the only tool binding. Rig derives the allowed catalog/actions/modes from the parent definition. Session runtime maintains child ownership and request maps, reserves quota before construction, and records selected initial mode on `LoopStarted`. Keep idle children warm; no model-facing stop action.

**Step 4: Verify green**

Run the focused command and all `pkg/tools`, `pkg/rig`, and `internal/sessionruntime` tests. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tool pkg/tools pkg/rig internal/sessionruntime
git commit -m "feat(subagent): add managed parent-scoped delegation"
```

### Task 11: Implement hierarchical interruption and queue policy

**Files:**
- Modify: `internal/sessionruntime/session.go`
- Create: `internal/sessionruntime/interrupt.go`
- Create: `internal/sessionruntime/interrupt_test.go`
- Modify: `internal/sessionruntime/delegation.go`
- Modify: `pkg/loop/controller.go`
- Modify: `pkg/tools/subagent.go`

**Step 1: Write failing concurrency tests**

Prove session interrupt reaches every live loop concurrently; loop-controller interrupt covers its delegate subtree; Subagent interrupt affects one owned child's current turn; mark-before-fanout prevents a parent from taking a new step after an interrupted wait resolves; user input survives; machine-created delegate requests flush; and fully idle interrupt returns false without events.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./internal/sessionruntime ./pkg/tools -run 'Test.*Interrupt'
```

Expected: current active-turn-only behavior/queue semantics fail.

**Step 3: Implement flat delivery over hierarchical selection**

Select target IDs from parent provenance under the session lock, mark the whole set interrupt-pending, then deliver one-hop commands concurrently. Close an admission barrier until every target acknowledges idle; leave its final release policy pluggable for the workspace controller added in Task 15.

**Step 4: Verify green**

Run the focused command plus all sessionruntime/tools race tests. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/loop pkg/tools internal/sessionruntime
git commit -m "feat(session): add hierarchical interruption"
```

---

## Phase C — Workspace safety, placement, and native checkpoints

### Task 12: Add per-loop file observations and atomic create-without-read

**Files:**
- Create: `pkg/tools/file_observations.go`
- Create: `pkg/tools/file_observations_test.go`
- Modify: `pkg/tools/readfile.go`
- Modify: `pkg/tools/readfile_test.go`
- Modify: `pkg/tools/writefile.go`
- Modify: `pkg/tools/writefile_test.go`
- Modify: `pkg/tools/editfile.go`
- Modify: `pkg/tools/editfile_test.go`
- Modify: `pkg/tools/definitions.go`
- Modify: `pkg/tools/definitions_test.go`

**Step 1: Write failing freshness tests**

Cover complete read hash recording, definitive absence, truncated/denied/escaping/ambiguous reads, existing-file mutation without observation, external modification, re-read recovery, successful-write hash refresh, independent loop bundles, canonical aliases, concurrent same-path calls, and edit anchor mismatch.

Pin new-file behavior separately:

- no observation + absent path → success through atomic no-replace publication;
- no observation + existing path → typed failure, unchanged bytes;
- concurrent creators → one success, one typed conflict;
- successful creator records the produced hash.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/tools -run 'Test.*Observation|Test.*Fresh|Test.*Create'
```

Expected: stale overwrites are currently allowed and no observation bundle exists.

**Step 3: Implement the private deterministic map**

Use `map[canonicalPath]*filePathState` guarded by a map mutex and per-path mutex. Hash complete raw bytes with SHA-256. Keep hashes private. Publish a new file by writing/syncing a sibling temp and atomically linking it into the absent destination (`os.Link` no-replace semantics on the same filesystem), then remove the temp. Existing overwrite keeps temp+atomic rename after hash validation.

**Step 4: Verify green and integration behavior**

```bash
GOWORK=off go test -race ./pkg/tools
GOWORK=off go test -tags integration -race ./pkg/tools -run 'Test.*File'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tools
git commit -m "feat(tools): guard file writes with per-loop observations"
```

### Task 13: Add the session-scoped workspace coordinator

**Files:**
- Create: `internal/sessionruntime/workspace_coordinator.go`
- Create: `internal/sessionruntime/workspace_coordinator_test.go`
- Modify: `pkg/tool/definition.go`
- Modify: `pkg/tools/{writefile,editfile,bash}.go`
- Modify: `pkg/tools/definitions.go`
- Modify: `internal/loopruntime/runner.go`

**Step 1: Write failing permit tests**

Under `-race`, prove structured path mutations can run concurrently on different paths, same-path mutations serialize, Bash/unknown mutators take the exclusive whole-workspace permit, snapshot and restore exclude every mutation, waiting exclusive work prevents starvation, cancellation removes waiters, and lease-health failure blocks commit.

Also prove Bash invalidates the calling loop's entire observation map after every attempted run because changed paths are unknowable.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./internal/sessionruntime ./pkg/tools -run 'Test.*Coordinator|Test.*Permit|TestBash.*Observation'
```

Expected: missing coordinator and overlapping mutations.

**Step 3: Implement permits and bindings**

The concrete coordinator stays internal and satisfies the narrow `tool.WorkspaceCoordinator`. Structured mutators hold shared session mutation + canonical path permits around final validation/write/observation update. Bash holds a whole-workspace mutation permit around command execution. Inference and read-only event observation need no permit.

**Step 4: Verify green**

Run focused tests and all tools/sessionruntime tests under `-race`. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tool pkg/tools internal/loopruntime internal/sessionruntime
git commit -m "feat(workspace): coordinate session-wide mutations"
```

### Task 14: Implement optional workspace placement, leases, seeding, and rewind

**Files:**
- Create: `pkg/rig/workspace.go`
- Create: `pkg/rig/session_options.go`
- Create: `pkg/rig/workspace_errors.go`
- Create: `pkg/rig/workspace_test.go`
- Create: `internal/sessionruntime/workspace_placement.go`
- Create: `internal/sessionruntime/workspace_placement_test.go`
- Create: `internal/sessionruntime/workspace_restore.go`
- Create: `internal/sessionruntime/workspace_restore_test.go`
- Modify: `pkg/rig/options.go`
- Modify: `pkg/rig/definition.go`
- Modify: `internal/sessionruntime/restore.go`

**Step 1: Write the placement matrix**

Test zero placement, exactly one placement, multiple-placement rejection, snapshots/seed without placement, workspace-requiring tools without placement, persistence-root overlap, canonical roots, and fingerprint `{mode, canonical root/base}`.

No-workspace sessions construct without root resolution, leasing, or a checkpoint
controller; both workspace controller methods return `WorkspaceUnavailableError`.
Discoverable persistence paths equal to or beneath the workspace fail, while disjoint
paths and an ancestor whose actual files stay outside the workspace are accepted.

For exclusive placement, test hashed root lease names, lexical/symlink alias contention across rig instances, acquire/release order, `WorkspaceRootBusyError{Root, HolderEpoch}`, lease loss fault (reject admission, interrupt loops, cancel checkpoints), empty-root materialization, and non-empty attach. Shutdown must stop work/checkpoints, release the root lease, then release the session lease. For per-session placement, test injective root derivation, staged verify/swap, backup rollback, abandoned-stage recovery, and refusal to remove arbitrary/symlink paths. For shared placement, test no lease, no automatic materialization, and required-priority rejection.

Test seed ordering and validity. Test `RestoreWorkspace`: idle/control-plane requirement, exclusive permit, safe per-session swap, fixed/shared manifest reconcile with sorted path locks and rollback copies, durable `WorkspaceRestored`, and append-failure fault.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/rig ./internal/sessionruntime -run 'Test.*Workspace|Test.*Seed|Test.*Restore'
```

Expected: missing placement APIs and old unconditional materialization behavior.

**Step 3: Implement declarative strategies**

Add:

```go
rig.WithExclusiveWorkspace(store, root, leaser)
rig.WithSessionWorkspaces(store, baseDir)
rig.WithSharedWorkspace(store, root)
rig.WithSeedSnapshot(ref) // NewSession option
```

Validate discoverable persistence paths at `rig.Define`. Acquire session lease then exclusive root lease before durable append. Keep persistence outside the captured tree. `CurrentWorkspace` is the effective durable restore point, not a live-tree equality claim.

**Step 4: Verify green**

Run focused tests, all rig/sessionruntime tests, then filesystem integration cases under `-tags integration -race`. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/rig internal/sessionruntime
git commit -m "feat(rig): place seed and rewind optional workspaces"
```

### Task 15: Implement native trigger boundaries and snapshot priority

**Files:**
- Create: `pkg/rig/snapshot_policy.go`
- Create: `pkg/rig/snapshot_policy_test.go`
- Create: `internal/sessionruntime/checkpoint_controller.go`
- Create: `internal/sessionruntime/checkpoint_controller_test.go`
- Modify: `internal/sessionruntime/workspace_coordinator.go`
- Modify: `internal/sessionruntime/session.go`
- Modify: `internal/loopruntime/loop.go`
- Modify: `internal/loopruntime/turn.go`
- Modify: `pkg/hub/deps.go`
- Modify: `pkg/hub/hub.go`
- Modify: `pkg/hub/hub_test.go`

**Step 1: Write failing ordering/backpressure tests**

Cover trigger default/manual/idle/turn/step selection, every turn terminal
(`TurnDone`/`TurnFailed`/`TurnInterrupted`), no synthetic shutdown trigger, one
whole-workspace ref per boundary, and this strict successful order:

```text
actual work complete
→ acquire exclusive snapshot permit
→ append trigger durably
→ emit trigger
→ make snapshot blob durable
→ append WorkspaceCheckpointed with Header.Cause
→ emit WorkspaceCheckpointed
→ release permit
→ acknowledge boundary
```

Test best-effort one-active/one-latest coalescing, cancellation on activation, shared fuzzy completion, manual non-coalescing, required FIFO/no coalescing, actor/admission blocking, timeout/fault latch, manual recovery clearing the latch, and shutdown cancellation classification. Failure must leave the trigger durable and must never append a checkpoint pointing at a missing blob.

Manual checkpoint calls require idle even when an automatic trigger is configured;
`SnapshotManual` is a valid explicit policy and installs no automatic scheduler.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/rig ./pkg/hub ./internal/loopruntime ./internal/sessionruntime -run 'Test.*Checkpoint|Test.*Snapshot|Test.*Boundary'
```

Expected: only manual checkpointing exists and hub-derived idle cannot participate in the native boundary.

**Step 3: Implement the internal boundary collaborator**

Add an internal session boundary interface, not a public hook. Loop actor step commits and turn terminals synchronously call it. Extend hub's derived-session-edge path with a narrow internal collaborator that acquires before appending/emitting `SessionIdle` and completes checkpoint acceptance/walk before `WaitIdle` acknowledgement as policy requires. Avoid generic subscriber/watch logic.

`SnapshotPolicy` is:

```go
type SnapshotPolicy struct {
	Trigger  SnapshotTrigger
	Priority SnapshotPriority
	Timeout  time.Duration
}

const (
	SnapshotTriggerUnset SnapshotTrigger = iota
	SnapshotManual
	SnapshotOnIdle
	SnapshotOnTurnDone
	SnapshotOnStepDone
)

const (
	SnapshotBestEffort SnapshotPriority = iota
	SnapshotRequired
)
```

Unset resolves to idle, zero priority resolves to best effort, and zero timeout resolves to 60 seconds. Recorded exclusive/per-session refs are quiescent; shared refs are fuzzy.
`rig.WithSnapshots(policy)` is required with every placement and invalid without one.

**Step 4: Verify green**

Run focused tests, all affected packages, and the whole harness under `-race`. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/rig pkg/hub internal/loopruntime internal/sessionruntime
git commit -m "feat(workspace): checkpoint at native execution boundaries"
```

### Task 16: Connect interrupt barriers to checkpoint policy

**Files:**
- Modify: `internal/sessionruntime/interrupt.go`
- Modify: `internal/sessionruntime/interrupt_test.go`
- Modify: `internal/sessionruntime/checkpoint_controller.go`
- Modify: `internal/sessionruntime/checkpoint_controller_test.go`

**Step 1: Extend failing interruption tests**

Pin barrier release for required idle, required turn, required step, best-effort idle, best-effort turn/step, manual, and no workspace. Prove preserved user input cannot dispatch before every target is idle and `SessionIdle` is appended. Under idle trigger, verify interrupt checkpoints carry `Trigger: interrupt`; turn interruption carries turn-done trigger; step policy manufactures no `StepDone`.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./internal/sessionruntime -run 'TestInterrupt.*Checkpoint|TestInterrupt.*Barrier'
```

Expected: Task 11's generic barrier releases too early.

**Step 3: Implement policy-specific release**

Have the checkpoint controller return an explicit accepted/committed/faulted outcome to the interrupt sweep. Release exactly as the consolidated spec's barrier table requires; no sleeps or event-stream polling.

**Step 4: Verify green**

Run all sessionruntime tests and the whole harness under `-race`. Expected: PASS.

**Step 5: Commit**

```bash
git add internal/sessionruntime
git commit -m "feat(session): order interrupts with workspace checkpoints"
```

---

## Phase D — Public integration, deletion guards, and end-to-end proof

### Task 17: Complete rig validation and staged lifecycle failures

**Files:**
- Modify: `pkg/rig/definition.go`
- Modify: `pkg/rig/lifecycle.go`
- Modify: `pkg/rig/errors.go`
- Modify: `pkg/rig/rig_test.go`
- Create: `pkg/rig/lifecycle_test.go`
- Modify: `internal/sessionruntime/restore.go`

**Step 1: Write the complete validation/failure matrix**

Cover every singleton/additive option, graph invariant, workspace pairing, delegation cap, foreign builder, gate cap, ceiling factory, fingerprint mismatch policy, and failure after each acquired resource. Prove cleanup order and no partial session escapes. Restore must rebuild primers, delegates, modes, direct inference changes, active loop, quota, gates, ceiling, current workspace, and checkpoint policy before admission.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/rig ./internal/sessionruntime -run 'TestDefine|TestNewSession|TestRestoreSession|TestLifecycle'
```

Expected: missing edge validation/failure cleanup cases.

**Step 3: Fill the remaining rig seams**

Add final options corresponding to existing supported seams (`WithForeignBuilders`, `WithGateCaps`, `WithFingerprintFields`, `WithAllowConfigMismatch`, `WithCeilingFactory`) without introducing alternate constructors. Freeze all resolved options; singleton duplicates fail instead of last-one-wins.

**Step 4: Verify green**

Run all rig/sessionruntime tests and the whole harness under `-race`. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/rig internal/sessionruntime
git commit -m "feat(rig): finalize lifecycle validation and restore"
```

### Task 18: Enforce the final public package boundaries and remove compatibility code

**Files:**
- Modify: `pkg/loop/deps_test.go`
- Modify: `pkg/session/contracts_test.go`
- Modify: `pkg/serve/deps_test.go`
- Create: `pkg/rig/deps_test.go`
- Delete remaining `loop.Config`, session construction options, compatibility adapters, and old naming
- Modify package comments: `pkg/loop`, `pkg/session`, `pkg/rig`, `internal/loopruntime`, `internal/sessionruntime`

**Step 1: Write failing AST/dependency guards**

Assert:

- `pkg/loop` exports `Define`, `Definition`, `Mode`, `Handle`, and `Controller`, but no actor constructor, mutable config, or resolved `ToolSet`;
- `pkg/session` exports contracts/errors but no constructor, restore function, runner, or compile option;
- only `pkg/rig` imports internal session runtime;
- `serve` imports neither rig nor session concrete packages in production;
- no active Go source uses `storekit`, `WithWorkspaceStore`, `WithCompile`, `session.Runner`, or `serve.Runner`.

**Step 2: Verify red**

```bash
GOWORK=off go test -race ./pkg/loop ./pkg/session ./pkg/rig ./pkg/serve
```

Expected: compatibility symbols are still found.

**Step 3: Delete the temporary bridge surface**

Remove it rather than deprecating it. Update package docs and internal tests to use only final contracts. Do not touch CLI or SWE yet; their migration is specified later.

**Step 4: Verify green and search**

```bash
GOWORK=off go test -race ./...
rg -n '\bstorekit\b|WithWorkspaceStore|WithCompile|session\.Runner|serve\.Runner' --glob '*.go' --glob '!vendor/**' pkg internal
rg -n '^type Config struct' pkg/loop
```

Expected: tests PASS; search returns no active legacy API declarations/usages.

**Step 5: Commit**

```bash
git add pkg internal
git commit -m "refactor: enforce rig-only session construction"
```

### Task 19: Add full filesystem-backed integration coverage

**Files:**
- Create: `pkg/rig/rig_integration_test.go`
- Create: `pkg/rig/workspace_integration_test.go`
- Modify: `pkg/serve/handlers_lifecycle_test.go`

**Step 1: Write end-to-end integration scenarios**

Use actual filesystem storage/workspace backends and test:

1. two primers, work on both, active-loop/model/effort changes, workspace mutation, idle checkpoint, shutdown, fresh-base restore, state verification, continued work;
2. two sessions seeded from one ref, divergent trees/checkpoints, fresh-base restore to distinct journal-authoritative roots;
3. exclusive-root contention across two rigs, clean handoff, and lease-loss fault;
4. persistence blobs/journals outside archives and overlap rejection;
5. serve create/restore wire behavior over the concrete rig.

**Step 2: Verify red**

```bash
GOWORK=off go test -tags integration -race ./pkg/rig ./pkg/serve
```

Expected: missing fixtures/helpers or uncovered integration defects.

**Step 3: Implement only integration fixes**

Add deterministic fake inference streams and filesystem fixtures. Fix behavior in the owning package; do not weaken assertions or add sleeps. Keep process/filesystem cases under `//go:build integration`.

**Step 4: Verify all gates**

```bash
GOWORK=off go test -race ./...
GOWORK=off go test -tags integration -race ./...
CGO_ENABLED=0 GOWORK=off go build -trimpath ./...
```

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/rig pkg/serve
git commit -m "test(rig): cover lifecycle and workspace integration"
```

---

## Phase E — Documentation and ordered consumer migration plans

### Task 20: Write CI-compiled rig documentation and examples

**Files:**
- Create: `docs/architecture/rig-session-loop.md`
- Create: `docs/guides/rig-quickstart.md`
- Create: `docs/guides/multi-loop-and-modes.md`
- Create: `docs/guides/delegation.md`
- Create: `docs/guides/workspaces.md`
- Create: `docs/guides/serve.md`
- Create: `docs/guides/rig-migration.md`
- Create: `pkg/rig/example_test.go`
- Create: `pkg/tools/example_test.go`
- Modify: `docs/architecture/agent-loop.md`

**Step 1: Add failing examples first**

Write executable Go examples for:

- minimal one-loop rig;
- multiple primers and active-loop routing;
- same-history plan/build modes;
- sync and managed Subagent actions;
- no-workspace, exclusive, per-session, and shared placement;
- manual/idle/turn/step snapshot policy, seed, rewind, shutdown, and restore;
- serve's narrow rig integration.

Expected outputs should avoid unstable UUIDs/timestamps.

**Step 2: Verify examples fail before docs settle**

```bash
GOWORK=off go test -race ./pkg/rig ./pkg/tools -run Example
```

Expected: compile/output failures identify drift.

**Step 3: Write workflow-first documentation**

Explain `Rig → Session → Loop → Turn → Step`, definitions/primers/active loop/modes/delegates, data-plane versus control-plane interfaces, package ownership, delegation attenuation, workspace consistency labels, file observation behavior, persistence placement, manual GC, and the breaking migration from old session APIs. Manual GC guidance must require a complete journal-derived live set and external serialization against every active snapshot writer, and distinguish it from session offload-blob GC. Link each guide to a compiled example.

**Step 4: Verify docs and examples**

```bash
GOWORK=off go test -race ./pkg/rig ./pkg/tools
GOWORK=off go test -race ./...
```

Expected: PASS.

**Step 5: Commit**

```bash
git add docs/architecture/agent-loop.md docs/architecture/rig-session-loop.md docs/guides pkg/rig/example_test.go pkg/tools/example_test.go
git commit -m "docs: explain rig sessions delegation and workspaces"
```

### Task 21: Write the CLI migration spec and implementation plan

**Files:**
- Create: `../cli/docs/plans/2026-07-11-harness-rig-migration-design.md`
- Create: `../cli/docs/plans/2026-07-11-harness-rig-migration-implementation.md`

**Step 1: Inventory the CLI consumer**

Run:

```bash
rg -n 'session\.|loop\.Config|Runner|Run\(|Restore\(|WithWorkspace|CheckpointWorkspace|SessionIdle' ../cli --glob '*.go'
```

Record every lifecycle, serve adapter, workspace, event, and tool construction site.

**Step 2: Write the design spec**

Map each old call to documented rig APIs, including active loop, mode/controller access, session ID retrieval, and serve's renamed narrow interface. Do not change CLI code.

**Step 3: Write the TDD implementation plan**

Use the same task structure as this plan, exact CLI files, exact test commands, and a harness version/vendor update step. CLI migration comes first and must not absorb SWE-specific composition.

**Step 4: Review against compiled harness examples**

Every proposed CLI call must appear in or be type-compatible with the final harness API/examples. Run `git diff --check` in CLI.

**Step 5: Commit in CLI only**

```bash
(cd ../cli && git add docs/plans/2026-07-11-harness-rig-migration-*.md && git commit -m "docs: plan harness rig migration")
```

### Task 22: Write the SWE migration spec and implementation plan

**Files:**
- Create: `../swe/docs/plans/2026-07-11-harness-rig-migration-design.md`
- Create: `../swe/docs/plans/2026-07-11-harness-rig-migration-implementation.md`

**Step 1: Inventory SWE after the CLI plan is complete**

Run:

```bash
rg -n 'session\.|loop\.Config|Runner|Run\(|Restore\(|WithWorkspace|CheckpointWorkspace|SessionIdle|Subagent|Spawner' ../swe --glob '*.go'
```

Inventory persistence/session factory, late-bound spawner cycle, workspace checkpoint watcher, tools, sandbox/read guard, primers/delegates/modes, and serve integration.

**Step 2: Write the SWE design spec**

Specify its rig composition using documented definitions, primer/delegate topology, mode selection, per-session tool binding, sandbox runner, session/workspace stores, placement, snapshot priority, and managed Subagent tool. Explicitly delete SWE's checkpoint watcher and manual lifecycle wiring rather than wrapping them.

**Step 3: Write the TDD implementation plan**

Include exact SWE files, migration ordering, harness version/vendor step, regression tests for restored sessions and async delegates, and removal checks. Migration implementation remains a separate execution.

**Step 4: Review against the CLI plan and harness docs**

Ensure shared concerns use the same documented API, while SWE-only topology/sandbox policy stays in SWE. Run `git diff --check` in SWE.

**Step 5: Commit in SWE only**

```bash
(cd ../swe && git add docs/plans/2026-07-11-harness-rig-migration-*.md && git commit -m "docs: plan harness rig migration")
```

### Task 23: Final verification and handoff

**Files:**
- Modify only files required by failures found during verification.

**Step 1: Run formatting and static/security gates**

```bash
GOWORK=off make fmt
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache make secure
```

Expected: PASS with no formatting diff after the gate.

**Step 2: Run all unit and integration tests**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache go test -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache go test -tags integration -race ./...
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache go build -trimpath ./...
```

Expected: all commands exit 0.

**Step 3: Run final architectural searches**

```bash
rg -n '\bstorekit\b|WithWorkspaceStore|WithCompile|session\.Runner|serve\.Runner' --glob '*.go' --glob '!vendor/**' pkg internal
rg -n 'internal/(loopruntime|sessionruntime)' pkg --glob '*.go'
git diff --check
git status --short
```

Expected: no active legacy APIs; only `pkg/rig` imports internal session runtime; no whitespace errors; only intentional changes.

**Step 4: Request code review**

Use `@superpowers:requesting-code-review` against the consolidated design and this plan. Fix findings with TDD and rerun Steps 1–3.

**Step 5: Commit verification fixes, if any**

```bash
git add <only-files-fixed-by-review>
git commit -m "fix(rig): address final implementation review"
```

Do not create an empty commit when no fixes were needed.

---

## Completion criteria

- `loop.Define(options...)` and `rig.Define(options...)` are the only public composition path.
- `Rig.NewSession` and `Rig.RestoreSession` return `session.SessionController`; public session/loop packages expose contracts, not constructors.
- Multiple primers, active-loop changes, same-history modes, dynamic inference changes, scoped managed delegation, and hierarchical interrupt all restore deterministically.
- Workspace support is optional and exactly one explicit placement is used when present.
- Harness-managed mutation cannot overlap a recorded quiescent snapshot; native boundaries preserve append/emit/blob/checkpoint ordering.
- File observations are per loop, hashes stay private, existing files require a fresh read, and genuinely new files use atomic no-replace creation without a failed-read round trip.
- `CurrentWorkspace` and `LastCheckpoint` fold independently; seed and rewind survive restore; workspace GC remains manual and sees every retained ref.
- Serve wire behavior is unchanged while its dependency vocabulary becomes `Rig.NewSession`/`RestoreSession`.
- End-user documentation and CI-compiled examples land before the separate CLI and SWE migration plans.
- Unit, integration, race, security, and trimpath build gates all pass.
