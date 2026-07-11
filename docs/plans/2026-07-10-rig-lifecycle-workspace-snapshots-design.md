# Design: rig lifecycle and automatic workspace snapshots

**Date:** 2026-07-10  
**Status:** Approved  
**Supersedes:** the public `session.Compile` / `session.Runner` lifecycle surface  
**Builds on:** `2026-07-02-workspacestore-design.md` and the implemented
`pkg/workspacestore`, `pkg/sessionstore`, and `session.CheckpointWorkspace`

## Problem

Harness already implements the mechanisms needed to persist an agent session:

- `sessionstore` durably journals every enduring event and provides leases, replay,
  catalogs, and offload-blob collection;
- `workspacestore` deterministically archives, hashes, stores, and materializes a
  workspace;
- `SessionController.CheckpointWorkspace` stores a snapshot before appending the enduring
  `WorkspaceCheckpointed` reference; and
- `session.Restore` reconstructs the session and materializes its latest workspace
  checkpoint.

The public composition API does not express that architecture cleanly. Today a consumer
calls `session.Compile(loop.Config, store, CompileOption...)`, receives a
`session.Runner`, and calls `Run` or `Restore`. This has four problems:

1. `Run` creates an empty live session; it does not run a task.
2. `Compile` and exported configuration structs provide several overlapping ways to
   assemble the same runtime.
3. A session package constructs the object that owns many sessions, reversing the real
   hierarchy.
4. Automatic workspace checkpoint scheduling remains consumer code. SWE watches
   `SessionIdle` itself even though the operation and restore path already live in
   harness.

The current model also assumes one immutable primary loop. That makes static fan-out,
changing the default input target, named plan/build modes, and changing a live loop's
model or effort unnecessarily awkward.

## Decision

Add `pkg/rig` as the single public composition and session-lifecycle API. A rig is an
immutable design-time assembly that creates and restores live sessions.

Use option-built definitions instead of exported configuration structs:

```go
planner, err := loop.Define(
	loop.WithName("planner"),
	loop.WithInference(client, planningModel),
	loop.WithSystem(planningPrompt),
	loop.WithTools(planningTools),
	loop.WithToolLimits(loop.ToolLimits{Iterations: 25, Calls: 100}),
	loop.WithDelegates("builder", "reviewer"),
)

builder, err := loop.Define(
	loop.WithName("builder"),
	loop.WithInference(client, buildingModel),
	loop.WithSystem(buildingPrompt),
	loop.WithTools(buildingTools),
)

r, err := rig.Define(
	rig.WithLoops(planner, builder),
	rig.WithPrimers("planner", "builder"),
	rig.WithActivePrimer("planner"),
	rig.WithSessionStore(sessionStore),
	rig.WithWorkspaceStore(workspaceStore, workspaceRoot),
	rig.WithSnapshots(rig.SnapshotPolicy{
		Trigger: rig.SnapshotOnIdle,
		Failure: rig.SnapshotBestEffort,
		Timeout: 60 * time.Second,
	}),
	rig.WithDelegationLimits(rig.DelegationLimits{Depth: 2, Quota: 64}),
)

freshController, err := r.NewSession(ctx)
restoredController, err := r.RestoreSession(ctx, sessionID)
```

This is an intentional breaking replacement. Remove `session.Compile`,
`session.Runner`, their compile options, and public session constructors in the same
change. Update `pkg/serve` in the same change. SWE and CLI receive a separate migration
spec after the harness API lands.

## Vocabulary and hierarchy

### Design time

```text
Rig
├── loop definitions
├── primers
├── active primer
├── session store and lifecycle policy
└── workspace store and snapshot policy
```

- A **loop definition** is an immutable recipe for constructing a loop.
- A **primer** is a named loop definition instantiated as a root loop when a session is
  created. A rig may have one or many primers.
- The **active primer** is the primer that becomes a new session's initial active loop.
- A **delegate** is a loop definition one loop is authorized to spawn dynamically.

### Runtime

```text
Rig
└── Session(s)
    ├── active loop
    ├── other primer loops
    └── dynamically delegated loops
        └── Turn
            └── Step
                ├── model chunks
                └── tool calls
```

A session owns and coordinates its loops. It also owns their shared event fan-in,
permission gates, spawn limits, security ceiling, persistence, active-loop selection,
and workspace checkpoint history.

## Package boundaries

```text
pkg/rig
    Public definition and session-lifecycle API.

pkg/loop
    Public immutable Definition, Mode, ToolLimits, and live Handle contracts.

pkg/session
    Public live Session contract and errors; no public constructors.

internal/loopruntime
    Concrete loop actor, turn/step machinery, and restoration implementation.

internal/sessionruntime
    Concrete session coordinator, construction, and restoration used by pkg/rig.

pkg/sessionstore
    Durable event ledger, leases, replay, catalog, and offload-blob GC.

pkg/workspacestore
    Immutable workspace snapshots, materialization, and snapshot GC primitive.
```

Dependency direction is downward:

```text
rig → session (contract) → loop (contract)
 │          ▲                  ▲
 │          │                  │
 ├→ internal/sessionruntime → internal/loopruntime
 ├──────────────────────────→ sessionstore
 └──────────────────────────→ workspacestore
```

`session` never imports `rig`. `serve` continues depending on narrow local interfaces
and does not import either concrete package in production.

`loop` and `session` remain public because consumers must define loops, hold loop handles,
submit work, route input, answer gates, subscribe to events, and shut sessions down. They
are public **contract packages**, not alternative composition roots. Their live concrete
implementations and constructors move under `internal`, so an external consumer cannot
bypass `rig.Define`, `Rig.NewSession`, or `Rig.RestoreSession`.

## Loop definitions

Replace the exported `loop.Config` construction surface with an immutable
`loop.Definition` built by `loop.Define`:

```go
func Define(opts ...Option) (Definition, error)
```

The initial option set covers the fields currently exposed by `loop.Config`:

- `WithName`
- `WithInference`
- `WithSystem`
- `WithTools`
- `WithToolLimits`
- `WithEngine`
- `WithDrainTimeout`
- `WithRuntimeContext`
- `WithDelegates`
- `WithModes`
- `WithInitialMode`

`loop.Define` validates required dependencies and returns typed definition errors. The
definition contains no session-specific mutable state. Internal constructors may still
use an unexported resolved structure, but callers see no public config struct.

### Delegation

Delegation topology belongs to loop definitions and the rig, not to the model-facing
tool:

```go
planner, err := loop.Define(
	loop.WithName("planner"),
	loop.WithDelegates("builder", "reviewer"),
)
```

At `rig.Define`, every delegate name must resolve to exactly one registered definition.
The rig derives the catalog and spawner capability for that parent. A loop without
delegates receives no `Subagent` capability and cannot spawn. This preserves structural
least privilege without a redundant global `WithSpawnableLoops` list.

Definitions, primers, and delegates are separate sets:

- every primer must name a definition;
- a delegate may name a primer definition or a non-primer definition;
- a definition may be both a primer and a delegate;
- an unreferenced definition is rejected because it can never be instantiated; and
- duplicate definition names fail definition atomically.

### Subagent tool mode selection

The model-facing `Subagent` tool accepts an optional initial mode for the delegated loop:

```json
{
  "agent": "builder",
  "mode": "review",
  "message": "Review the persistence changes"
}
```

Its typed arguments become:

```go
type SubagentArgs struct {
	Agent   identity.AgentName `json:"agent"`
	Mode    loop.ModeName      `json:"mode,omitempty"`
	Message string             `json:"message"`
}
```

`mode` is a construction parameter on the tool call, not a second tool and not a global
rig setting. When omitted, the child uses its definition's initial mode. When supplied,
the spawner resolves and validates the mode against the target definition before
reserving quota or creating a loop. An unknown or unauthorized mode fails without
spawning anything.

The child starts directly in the selected mode; it does not start in one mode and emit a
synthetic `LoopModeChanged`. `LoopStarted` carries the selected initial mode so replay and
restore reconstruct the child deterministically. The child's effective tools and
permissions remain clamped by the parent and session security ceiling regardless of the
requested mode.

## Rig definition

`rig.Define(opts ...Option) (*Rig, error)` is the only public constructor for the
complete runtime assembly.

Initial options:

| Option | Meaning |
|---|---|
| `WithLoops(definitions...)` | Register immutable loop definitions. Additive. |
| `WithPrimers(names...)` | Select root loops instantiated with every session. |
| `WithActivePrimer(name)` | Select the initial default input target. |
| `WithSessionStore(store)` | Supply durable leases, journal, replay, and catalog. |
| `WithWorkspaceStore(store, root)` | Enable workspace checkpoint and restore capability. |
| `WithSnapshots(policy)` | Select explicit checkpoint scheduling and failure behavior. |
| `WithDelegationLimits(limits)` | Set session-wide dynamic delegation depth and quota. |

`DelegationLimits.Depth` is the maximum nested delegate chain.
`DelegationLimits.Quota` is the maximum number of dynamically spawned loops over one
session's lifetime. Neither field limits model/tool steps.

Per-turn execution limits belong to each loop definition:

```go
loop.WithToolLimits(loop.ToolLimits{
	Iterations: 25, // maximum model↔tool round trips in one turn
	Calls:      100, // maximum total tool executions in one turn
})
```

Additional existing construction seams, including foreign-loop builders, gate caps,
fingerprint fields, config-mismatch policy, and a per-session ceiling factory, move to
equivalent `rig` options. They are not removed merely because the primary API is renamed.

Options resolve into an unexported definition structure. `Define` applies all options,
validates the completed graph once, and freezes it. Singleton options reject duplicates
instead of silently applying last-one-wins semantics. Additive options document whether
repetition merges or rejects duplicates.

### Required invariants

`rig.Define` fails with typed errors unless:

- at least one valid loop definition exists;
- at least one primer exists;
- every primer resolves to a registered definition;
- exactly one active primer is named and belongs to the primer set;
- every delegate resolves to a registered definition;
- a non-nil session store is supplied;
- workspace store and snapshot policy are either both absent or both present;
- the workspace root is canonical, non-empty, and compatible with the loop tool roots;
- snapshot trigger, failure behavior, and timeout resolve to valid values; and
- all existing fingerprint, gate, foreign-loop, ceiling, and limits invariants hold.

Validation performs no session I/O. Opening backend implementations remains the
consumer's composition responsibility; a rig receives already-open stores.

## Session creation and restoration

### `Rig.NewSession`

`NewSession(ctx)`:

1. Mints a cryptographically random session ID.
2. Acquires the session's single-writer lease.
3. Opens its journal and checked event, command, and gate appenders.
4. Mints a fresh per-session security-ceiling state.
5. Constructs every primer as a root loop with zero parent provenance.
6. Sets the active loop to the loop created from the active primer.
7. Publishes durable session, loop, and active-loop lifecycle events.
8. Starts automatic snapshot watching only for `SnapshotOnIdle`.
9. Returns the live `session.SessionController` contract backed by an internal runtime.

Any failure after lease acquisition releases the lease on a bounded best-effort path.
The method returns a typed stage error chaining the underlying cause.

### `Rig.RestoreSession`

`RestoreSession(ctx, id)`:

1. Acquires the session lease and opens its journal.
2. Replays and validates the durable event stream.
3. Checks the rig/loop topology fingerprint according to the configured mismatch policy.
4. Reconstructs all primer and delegated loops.
5. Reapplies durable loop changes and the last active-loop selection.
6. Materializes the latest workspace checkpoint, when configured.
7. Brings the reconstructed session up idle.
8. Starts the configured automatic snapshot watcher.
9. Returns the live session.

Restore does not create missing primer loops silently. A persisted topology incompatible
with the current rig fails with a typed mismatch error unless the caller explicitly
enabled the existing mismatch escape hatch.

The late-bound session/spawner cycle currently handled by SWE becomes internal rig
wiring. A session is fully bound before either lifecycle method returns, and no turn can
observe an unbound delegate capability.

## Active loop

There is no immutable primary loop. Every session has one mutable active loop: the
default target for `Session.Submit`.

```go
type Session interface {
	SessionID() uuid.UUID
	ActiveLoop() loop.Handle
	Loop(id uuid.UUID) (loop.Handle, bool)
	Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error)
	SubmitToLoop(ctx context.Context, id uuid.UUID, blocks []content.Block) (uuid.UUID, error)
	SubscribeEvents(event.EventFilter) (event.Subscription, error)
	RespondGate(context.Context, gate.GateResponse) error
	Interrupt(context.Context) (bool, error)
}

type SessionController interface {
	Session
	SetActiveLoop(ctx context.Context, id uuid.UUID) error
	LoopController(id uuid.UUID) (loop.Controller, bool)
	SetSecurityCeiling(context.Context, ceiling.Level) error
	CheckpointWorkspace(context.Context) (workspacestore.Ref, error)
	Shutdown(context.Context) error
}
```

`Session` is the ordinary data-plane contract for submitting work, observing it, and
answering gates. `SessionController` embeds it and adds the trusted control-plane
operations that change runtime policy or lifecycle. `Rig.NewSession` and
`Rig.RestoreSession` return `SessionController`; consumers pass it as the narrower
`Session` wherever control is unnecessary. Models receive neither interface directly —
only explicitly wired tools.

`SetActiveLoop` accepts any live loop, including a dynamically delegated loop. It emits
an enduring `ActiveLoopChanged` event before the new selection becomes observable.
Restore folds the last such event. An unknown, exited, or non-live target fails with a
typed error and leaves the prior active loop unchanged.

`SubmitToLoop` remains the explicit routing primitive and keeps its existing loop-ID
semantics. `Submit` resolves the active loop at dispatch time. Active-loop switching is
for navigation and routing among **independent loop histories**; it does not merge or
transfer context. Named plan/build behavior that must retain one conversation uses loop
modes instead, described below.

## Loop modes

A mode is a named, immutable, definition-time bundle of loop policy that may vary while
the loop retains its identity and committed message history. It supports plan/build,
explore/implement, and ordinary/high-effort workflows without pretending two independent
loops share context.

```go
operator, err := loop.Define(
	loop.WithName("operator"),
	loop.WithInference(client, defaultModel),
	loop.WithSystem(baseInstructions),
	loop.WithModes(
		loop.Mode{
			Name:         "plan",
			Model:        planningModel,
			Effort:       inference.EffortHigh,
			Tools:        planningTools,
			Instructions: planningInstructions,
		},
		loop.Mode{
			Name:         "build",
			Model:        buildingModel,
			Effort:       inference.EffortMedium,
			Tools:        buildingTools,
			Instructions: buildingInstructions,
		},
	),
	loop.WithInitialMode("plan"),
)
```

The base definition supplies identity, inference client, base system instructions,
engine, runtime context, and delegate policy. A mode may select a prevalidated model,
effort, tool set, tool limits, and additional mode instructions. Arbitrary runtime tools
or prompt text are never accepted; every selectable mode is fingerprinted as part of the
immutable definition.

At runtime:

```go
controller, ok := sess.LoopController(loopID)
err := controller.SetMode(ctx, "build")
```

`SetMode` validates the name against the loop definition and sends a command through the
loop actor. It takes effect at the next turn boundary. The current turn finishes entirely
under the mode captured when it began. A successful change durably appends
`LoopModeChanged`; restore reapplies the last selected mode before admitting new work.

The loop ID, attribution, turn sequence, and committed history do not change:

```text
operator loop
├── shared committed history
├── turn 1 (plan mode)
├── LoopModeChanged: plan → build
└── turn 2 (build mode, same history)
```

Use separate primer/delegate loops when histories should be isolated, such as independent
researchers, reviewers, parallel fan-out workers, or focused delegated agents. Use modes
when one continuing agent changes how it works over the same context.

## Dynamic loop handle

A session exposes a capability-limited handle for each live loop. Named mode changes are
the preferred way to change several coordinated properties; direct model/effort changes
support dynamic model routing that is not a predefined workflow mode:

```go
type Handle interface {
	ID() uuid.UUID
	Mode() ModeName
	Model() inference.Model
}

type Controller interface {
	Handle
	SetMode(context.Context, ModeName) error
	Change(context.Context, ...Change) error
}

handle, ok := sess.Loop(loopID)                    // read-only loop.Handle
controller, ok := sess.LoopController(loopID)      // trusted loop.Controller

err := controller.Change(
	ctx,
	loop.ChangeModel(newModel),
	loop.ChangeEffort(inference.EffortHigh),
)
```

`Change` sends a command through the loop actor; it never mutates loop fields from the
caller goroutine. All changes in one call validate and commit atomically. The first
version permits changing only the secret-free model descriptor and inference effort.
Changing the inference client, arbitrary tools/prompt text, engine, identity, delegates,
or security posture at runtime is out of scope. A predeclared mode may change its
definition-time tool set and mode instructions because those alternatives were validated
and fingerprinted by `loop.Define`.

Changes take effect at the **next turn boundary**. A running turn, including all of its
model/tool steps, keeps the model and effort selected when that turn started. This gives
deterministic request attribution and avoids changing behavior halfway through a tool
continuation.

A successful direct change emits an enduring `LoopChanged` event with the loop ID,
complete secret-free model identity, and effort. A successful named mode change emits
`LoopModeChanged`. Restore folds the latest changes per loop before accepting input.
Invalid modes/models/efforts, exited loops, changes during shutdown, and durable-append
failures return typed errors and do not partially apply.

## Workspace snapshot policy

The existing `workspacestore` snapshot format and `SessionController.CheckpointWorkspace`
snapshot-before-append ordering do not change. This design adds scheduling and failure
policy at the rig layer.

```go
type SnapshotPolicy struct {
	Trigger SnapshotTrigger
	Failure SnapshotFailure
	Timeout time.Duration
}

const (
	SnapshotManual SnapshotTrigger = iota
	SnapshotOnIdle
)

const (
	SnapshotBestEffort SnapshotFailure = iota
	SnapshotFaultSession
)
```

- `WithWorkspaceStore` requires an explicit `WithSnapshots` option.
- `WithSnapshots` without a workspace store is invalid.
- `SnapshotManual` is explicit and valid; it installs no watcher.
- `SnapshotOnIdle` reacts to the session-wide `SessionIdle` edge, after every primer and
  delegated loop is quiescent.
- A zero timeout resolves to 60 seconds. Negative timeouts are invalid.
- A zero failure mode resolves to `SnapshotBestEffort`.

### On-idle flow

For every `SessionActive → SessionIdle` transition:

1. The rig's per-session checkpoint controller observes `SessionIdle`.
2. It serializes against any checkpoint already running for that session.
3. It creates a timeout context derived from the session lifetime.
4. It calls `SessionController.CheckpointWorkspace`.
5. `workspacestore` deterministically archives and hashes the complete tree.
6. If the content-addressed blob is absent, it uploads it; otherwise upload is a no-op.
7. The session durably appends `WorkspaceCheckpointed{Ref}`.

The full archive walk and hash remain authoritative. Filesystem watchers or metadata may
later provide a dirty-check optimization, but never become correctness evidence because
watch events can be lost. Repeating an unchanged snapshot performs no blob upload. A
future optimization may also suppress a redundant checkpoint event when its ref equals
the last durable ref; that optimization is not required by this design.

The watcher is session-owned through the rig lifecycle: it begins before the lifecycle
method returns, stops on session shutdown, and is joined during teardown. It never leaks
a goroutine or runs after the workspace/store lifetime ends.

### Failure behavior

`SnapshotBestEffort` records/logs the typed checkpoint failure through the rig's
observability seam and leaves the session usable. The next idle edge retries.

`SnapshotFaultSession` latches a typed workspace-persistence fault on the session, wakes
idle waiters, and rejects subsequent input or loop creation. It does not destroy the
session; an operator may shut it down and restore from the previous durable checkpoint.

Cancellation caused by ordinary session shutdown is not reported as an operational
snapshot failure.

## Workspace garbage collection

Workspace snapshot GC remains **manual** in this design. The existing
`workspacestore.Store.GC(ctx, liveRefs)` primitive remains unchanged.

An administrator must compute a complete live set from all retained session journals and
must serialize collection against every active snapshot writer sharing the workspace blob
store. A process-local rig cannot prove that no other process is snapshotting, so it must
not pretend automatic periodic GC is safe. This design adds no automatic workspace GC
ticker.

The existing per-session offload-blob GC is a different collector. Its lifecycle may
move under `Rig`, but it must retain its lease/idle serialization and must not be described
as workspace snapshot GC.

## `serve` migration

Rename the narrow `serve.Runner` dependency to match the new lifecycle without importing
`pkg/rig`:

```go
type Rig[S LiveSession] interface {
	NewSession(ctx context.Context) (S, error)
	RestoreSession(ctx context.Context, id uuid.UUID) (S, error)
}
```

The created session exposes its ID, so `NewSession` need not return it separately. The
concrete rig returns `session.SessionController`; `serve` uses only its narrower
`LiveSession` method set:

```go
type LiveSession interface {
	SessionID() uuid.UUID
	Submit(context.Context, []content.Block) (uuid.UUID, error)
	// existing event, gate, and interrupt methods
}
```

HTTP routes and wire semantics do not change: create still returns 201 with the minted
session ID, and restore still addresses the existing ID. Only the injected dependency and
method names change. Handler tests use a `fakeRig` rather than `fakeRunner`, and the
dependency guard proves `*rig.Rig` satisfies
`serve.Rig[session.SessionController]`.

Because `serve` manages several attached live sessions, its own in-memory attachment map
remains in `serve`; it is not duplicated in `pkg/rig`.

## Breaking API removal

The harness change removes rather than deprecates:

- `loop.Config` as a public construction surface;
- `session.Compile`;
- `session.Runner`;
- all `session.WithCompile...` options;
- `Runner.Run` and `Runner.Restore`;
- public `session.New` and `session.Restore`; and
- `serve.Runner`.

The live `session.Session` and `session.SessionController` contracts remain public. Their
concrete implementation, construction, and restore code move behind `pkg/rig`, under
`internal/sessionruntime`.

SWE and CLI are deliberately not migrated in this harness spec. Once harness lands, a
separate cross-module migration spec will replace their manual session factory,
checkpoint watcher, restore/replay attachment, and old naming. Harness tests and
`pkg/serve` must compile and pass in the breaking harness commit itself.

## Error model

Every package-level failure is typed and unwraps its cause where applicable:

- loop-definition errors: missing inference, invalid model, duplicate/empty name,
  invalid delegate;
- rig-definition errors: missing loops/primers/active primer/store, unknown references,
  duplicate singleton options, invalid workspace/snapshot pairing;
- lifecycle errors: ID, lease, journal, appender, construction, restore, and workspace
  materialization stages;
- active-loop errors: unknown/exited target and durable active-loop change failure;
- loop-change errors: invalid change, invalid model/effort, wrong lifecycle phase, and
  durable append failure; and
- checkpoint errors: timeout, snapshot/store failure, event append fault, and shutdown
  cancellation classification.

No validation path silently substitutes a missing required dependency. No partial rig,
session, topology, active-loop change, or loop model change is returned on failure.

## Concurrency and ordering

- A `Rig` is immutable after `Define` and safe to use concurrently to create or restore
  independent sessions.
- Every session receives its own lease, journal, appenders, ceiling, active-loop state,
  loop actors, and checkpoint controller.
- Loop changes commit through the target actor and begin at the next turn boundary.
- Active-loop changes serialize through session state and become visible only after their
  enduring event commits.
- Checkpoints serialize per session and occur only after `SessionIdle`; a subsequent
  `SessionActive` does not cancel an already-running checkpoint, because the resulting ref
  names the filesystem observed during the snapshot walk. Tools cannot run while the
  session is idle, but external filesystem writers remain outside the harness consistency
  boundary, as they are today.
- Shutdown stops admission, waits/cancels lifecycle work according to existing session
  semantics, stops and joins the checkpoint controller, releases the lease, and then
  returns.

## Fingerprints and durable events

The rig fingerprint replaces the single-loop configuration fingerprint. It includes:

- every loop definition's stable identity and immutable policy revision;
- the ordered primer set;
- the active primer;
- delegation edges;
- tool/security policy revisions;
- workspace root identity and runtime-skill mode fields already supplied by consumers;
  and
- other existing compatibility fields.

Runtime model/effort changes are durable events, not definition-fingerprint mutations.
They restore after the base definition passes compatibility checks.

New enduring events:

```text
ActiveLoopChanged
    session id, previous loop id, active loop id

LoopChanged
    session id, loop id, secret-free model descriptor, effort

LoopModeChanged
    session id, loop id, previous mode, selected mode
```

Their codecs, validation profiles, replay folds, catalog implications, and malformed-event
failure behavior follow the existing event discipline. Neither event carries secrets.

## Testing

All tests are table-driven and run under `-race`.

### `pkg/loop`

- definition validation and duplicate options;
- delegate validation deferred to rig graph resolution;
- named mode validation, unique names, and required initial mode;
- plan→build mode change preserves loop ID and committed history;
- mode changes atomically select the declared model, effort, tools, limits, and
  instructions at the next turn boundary;
- immutable/deep-copied model and tool metadata;
- atomic runtime model/effort change;
- next-turn-only application, including a change requested during an active turn; and
- restore folds of `LoopChanged` and `LoopModeChanged`.

### `pkg/tools`

- Subagent JSON schema exposes optional `mode`;
- omitted mode uses the target definition's initial mode;
- valid explicit mode constructs the child directly in that mode;
- unknown/unauthorized mode fails before quota reservation or loop creation;
- selected mode is carried on `LoopStarted` and restored; and
- requested mode cannot bypass parent/session permission clamps.

### `pkg/rig`

- complete option validation matrix;
- multiple primers created as root loops;
- active primer selection;
- definition/delegate graph validation;
- delegation depth/quota are independent from per-loop tool iteration/call limits;
- two concurrent sessions share no lease, ceiling, loops, or checkpoint controller;
- failure at every new-session wiring stage releases the lease;
- create/shutdown/restore round trip with multiple primers and delegates;
- restored active loop and model/effort changes;
- manual policy installs no watcher;
- on-idle policy checkpoints exactly on session quiescence, not individual loop idle;
- unchanged snapshot performs no duplicate blob upload;
- timeout and both failure policies;
- shutdown joins the watcher; and
- no automatic workspace GC.

### `pkg/session`

- `SessionController` embeds `Session`, while `Session` exposes no policy-mutating
  methods;
- `Session.Loop` returns a read-only `loop.Handle` and
  `SessionController.LoopController` returns the trusted `loop.Controller`;
- `Submit` targets the current active loop;
- `SubmitToLoop` remains explicitly targeted;
- active-loop change durability and invalid-target atomicity;
- dynamically delegated loop can become active;
- multi-root quiescence derives one correct `SessionIdle` edge; and
- constructor visibility is enforced by dependency tests.

### `pkg/serve`

- `serve.Rig` contract and concrete structural assertion;
- create/restore routes call `NewSession`/`RestoreSession`;
- create reads `SessionID` from the returned session;
- idempotency behavior remains unchanged; and
- HTTP fixtures and error mapping remain wire-compatible.

### Integration

An integration test over an actual filesystem backend proves:

1. define a two-primer rig;
2. create a session and submit work to both loops;
3. change the active loop, model, and effort;
4. mutate the workspace;
5. reach session idle and checkpoint;
6. shut down;
7. restore into a fresh workspace;
8. verify files, active loop, loop settings, and conversation state; and
9. continue work successfully.

## Non-goals

- Migrating SWE or CLI in the harness change.
- Changing the workspace archive format or content-addressing algorithm.
- Incremental/per-file snapshots.
- Treating filesystem watchers as snapshot truth.
- Automatic workspace snapshot GC.
- Arbitrary runtime changes to inference clients, prompts, tools, delegates, engine,
  identity, or security posture. Predeclared, fingerprinted mode alternatives are allowed.
- Automatic fan-out or result aggregation semantics. Multiple primers are available and
  explicitly addressable; a higher-level flow may decide how to distribute and combine
  work.
- Replacing loop IDs with names in runtime routing.

## Result

Harness has one coherent public construction path:

```text
loop.Define(options...) → loop.Definition
rig.Define(options...)  → Rig
Rig.NewSession()        → live Session
Rig.RestoreSession(id)  → restored live Session
```

The rig owns topology and lifecycle policy. The session owns runtime loops and durable
state. The active loop is mutable, primers enable static multi-loop architectures,
delegates enable least-privilege dynamic spawning, loop handles permit durable model and
effort changes, and workspace snapshots become an explicit, reusable rig policy instead
of SWE-specific watcher code.
