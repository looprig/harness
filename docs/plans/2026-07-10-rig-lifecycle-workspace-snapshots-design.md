# Design: rig lifecycle and automatic workspace snapshots

**Date:** 2026-07-10  
**Status:** Approved  
**Consolidates:** `2026-07-11-workspace-placement-modes-design.md`, retained only as
decision history
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
	rig.WithSessionWorkspaces(workspaceStore, "/var/agents/workspaces"),
	rig.WithSnapshots(rig.SnapshotPolicy{
		Trigger:  rig.SnapshotOnIdle,
		Priority: rig.SnapshotBestEffort,
		Timeout:  60 * time.Second,
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
└── optional workspace placement, store, and snapshot policy
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

The model-facing `Subagent` tool is the one parent-to-child communication surface. It
accepts an optional initial mode for the delegated loop:

```json
{
	"action": "start",
  "agent": "builder",
  "mode": "review",
  "message": "Review the persistence changes",
  "wait": true
}
```

The tool uses one flat, strictly validated action envelope:

```go
type SubagentArgs struct {
	Action     SubagentAction    `json:"action,omitempty"`
	Agent      identity.AgentName `json:"agent,omitempty"`
	Mode       loop.ModeName      `json:"mode,omitempty"`
	DelegateID DelegateID         `json:"delegate_id,omitempty"`
	RequestID  *uuid.UUID         `json:"request_id,omitempty"`
	Message    string             `json:"message,omitempty"`
	Wait       *bool              `json:"wait,omitempty"`
	TimeoutSeconds *int           `json:"timeout_seconds,omitempty"`
}
```

`timeout_seconds` is a non-negative integer. Omitting it means an interruptible,
unbounded wait; a supplied timeout returns a typed timed-out request result. An absent
`request_id` means not supplied, while a supplied zero UUID is invalid.

The actions are:

| Action | Meaning |
|---|---|
| `start` | Create a child, submit its initial message, and optionally wait for its final response. |
| `send` | Enqueue a distinct follow-up turn on an owned child and optionally wait for its answer. |
| `wait` | Wait for one previously returned request ID without sending another message. |
| `interrupt` | Interrupt an owned child's current turn without destroying the child loop. |
| `status` | Return mechanical status for one owned child, or all owned children when `delegate_id` is omitted. |

Missing `action` means `start` and missing `wait` on `start` means `true`, preserving the
current synchronous Subagent behavior. There is no model-facing event cursor or raw child
event feed. A parent asks a meaningful progress question by using `send`; `status` reports
only bounded runtime facts such as running/idle/faulted/interrupted and pending request
count.

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

### Synchronous and managed delegation

A loop definition or predeclared mode chooses which Subagent actions its model can see:

```go
loop.WithDelegation(loop.Delegation{Style: loop.DelegationSyncOnly})
loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged})
```

| Style | Exposed actions |
|---|---|
| `DelegationSyncOnly` | `start`, with `wait` fixed to `true` |
| `DelegationManaged` | `start`, `send`, `wait`, `interrupt`, `status` |

Managed delegation includes synchronous use through `start` with `wait:true`. The rig
derives the model-facing JSON schema from the active definition/mode. The schema is not a
security boundary: the parent-scoped controller enforces the same action set if a caller
crafts unsupported JSON. A loop with no delegates receives no Subagent tool.

The session creates a separate controller bound to each live parent loop and injects it
into that loop's Subagent tool instance:

```text
session
└── parent loop
    └── Subagent tool
        └── DelegateController(parentLoopID)
```

The parent model never receives `SessionController` or `DelegateController` directly. A
scoped delegate controller can address only children owned by its bound parent; it rejects
siblings, ancestors, unrelated loop IDs, unavailable actions, and invalid modes. The
trusted `SessionController` remains able to intervene across the whole session.

### Follow-up request and answer semantics

`send` is a new Subagent tool call, but it uses the same `Subagent` tool with
`action:"send"`:

```json
{
  "action": "send",
  "delegate_id": "delegate_123",
  "message": "What have you completed and what remains?",
  "wait": true
}
```

For unambiguous question/answer correlation, delegate `send` does **not** use the normal
interactive fold-into-active-turn path. It enqueues a distinct child turn:

```text
child turn 10 is running
parent sends request 456
request 456 queues
child turn 10 finishes
child turn 11 starts with Cause.CommandID = request 456
child turn 11 reaches a terminal event
```

The session needs an internal non-folding enqueue primitive for this path; public
`Session.SubmitToLoop` retains its existing interactive queue/fold semantics.

With `wait:true`, the parent tool call waits for the exact turn correlated to the minted
request ID. Intermediate AI/tool `StepDone` messages are progress, not the answer. Only
that turn's terminal event resolves the request:

- `TurnDone.Message` is the answer returned in the parent's tool result;
- `TurnFailed` returns a typed failed request result; and
- `TurnInterrupted` returns a typed interrupted request result.

With `wait:false`, `send` returns immediately:

```json
{
  "delegate_id": "delegate_123",
  "request_id": "request_456",
  "status": "queued"
}
```

The parent later calls `wait` with both IDs. The request ID is required because one child
may have several queued turns. Child questions and answers become part of the child's own
committed history; the final answer crosses into the parent's history only as the
Subagent tool result. Histories are never implicitly merged.

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
| `WithExclusiveWorkspace(store, root, leaser)` | Use one fixed, exclusively leased workspace root. |
| `WithSessionWorkspaces(store, baseDir)` | Derive one isolated root per session. |
| `WithSharedWorkspace(store, root)` | Explicitly allow concurrent writers and fuzzy snapshots in one root. |
| `WithSnapshots(policy)` | Select explicit checkpoint scheduling and priority. |
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
- exactly one active primer resolves and belongs to the primer set; a single primer is
  the default, while multiple primers require `WithActivePrimer`;
- every delegate resolves to a registered definition;
- a non-nil session store is supplied;
- zero or one workspace placement option is supplied;
- placement and snapshot policy are either both absent or both present;
- any workspace root/base is canonical, non-empty, disjoint from discoverable
  persistence paths, and compatible with workspace-requiring tools;
- exclusive placement has a non-nil `storage.Leaser`;
- snapshot trigger, priority, and timeout resolve to valid values; and
- all existing fingerprint, gate, foreign-loop, ceiling, and limits invariants hold.

Validation performs no session I/O. Opening backend implementations remains the
consumer's composition responsibility; a rig receives already-open stores.

## Session creation and restoration

### `Rig.NewSession`

`NewSession(ctx, opts ...NewSessionOption)`:

1. Mints a cryptographically random session ID.
2. Acquires the session's single-writer lease.
3. Resolves the optional workspace root and acquires its exclusive lease when required.
4. Opens its journal and checked event, command, and gate appenders.
5. Materializes an optional `rig.WithSeedSnapshot(ref)`, then journals it as the first
   workspace checkpoint after `SessionStarted` and before any `LoopStarted` event.
6. Creates the session-scoped workspace coordinator and binds fresh tool instances for
   each loop.
7. Mints a fresh per-session security-ceiling state and constructs every primer as a
   root loop with zero parent provenance.
8. Sets the active loop to the resolved active primer and publishes the durable
   lifecycle events.
9. Starts the configured native checkpoint controller when a workspace is present.
10. Returns the live `session.SessionController` contract backed by an internal runtime.

Any failure after lease acquisition releases the lease on a bounded best-effort path.
The method returns a typed stage error chaining the underlying cause.

### `Rig.RestoreSession`

`RestoreSession(ctx, id)`:

1. Acquires the session lease and opens its journal.
2. Replays and validates the durable event stream.
3. Checks the rig/loop topology fingerprint according to the configured mismatch policy.
4. Reconstructs all primer and delegated loops.
5. Reapplies durable loop changes and the last active-loop selection.
6. Resolves the effective `CurrentWorkspace` pointer and materializes or attaches to it
   according to the configured placement mode.
7. Creates the session workspace coordinator, binds fresh ephemeral tools, and brings
   the reconstructed session up idle.
8. Starts the configured native checkpoint controller when a workspace is present.
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
	RestoreWorkspace(context.Context, workspacestore.Ref) error
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

A successful direct change emits an enduring `LoopInferenceChanged` event with the loop ID,
complete secret-free model identity, and effort. A successful named mode change emits
`LoopModeChanged`. Restore folds the latest changes per loop before accepting input.
Invalid modes/models/efforts, exited loops, changes during shutdown, and durable-append
failures return typed errors and do not partially apply.

## Optional workspace lifecycle

Workspace support is optional. With no placement option, the rig has no managed root,
snapshot controller, seeding, or rewind. `CheckpointWorkspace` and `RestoreWorkspace`
remain on the uniform controller but return `WorkspaceUnavailableError`. A
workspace-requiring tool definition makes a no-workspace rig invalid.

Exactly one of these placement options may accompany `WithSnapshots`:

```go
rig.WithExclusiveWorkspace(workspaceStore, "/home/user/project", rootLeaser)
rig.WithSessionWorkspaces(workspaceStore, "/var/agents/workspaces")
rig.WithSharedWorkspace(workspaceStore, "/home/user/project")
```

| Placement | Root and exclusion | Automatic restore | Checkpoint consistency |
|---|---|---|---|
| Exclusive | One canonical fixed root; one exclusive `storage.Leaser` lease | Materialize only into an empty root; otherwise attach | `quiescent` |
| Per-session | `baseDir/<sessionID>`; isolated by construction | Always replace with effective durable ref | `quiescent` |
| Shared | One canonical fixed root; deliberately no root lease | Never; rewind is explicit | `fuzzy` |

The public module and Go package are both `github.com/looprig/storage`, imported as
`storage`; public errors, comments, examples, and filenames use that name. The workspace
blob store and root leaser are separate arguments because their scopes/providers may
differ.

The session journal, catalog, leases, workspace blobs, and checkpoint controller state
must live outside the workspace root. Discoverable persistence paths equal to or beneath
the root fail `rig.Define` with a typed overlap error. Appending a boundary or checkpoint
event therefore cannot mutate the tree being captured.

### Placement details

Exclusive roots are canonicalized with `Abs`, `Clean`, and `EvalSymlinks` when present.
Their backend lease name is
`workspace-roots/<sha256(canonical-root)>`, so lexical/symlink aliases contend. Session
lease acquisition precedes root lease acquisition, which precedes any append,
materialization, checkpoint controller, or loop construction. Contention releases the
session lease and returns `WorkspaceRootBusyError`. Root lease loss faults the session,
closes admission, interrupts live loops, and cancels checkpoints. Before committing a
structured mutation, tools check that the lease remains healthy; this fences cooperative
writers, but cannot retroactively fence an external process or shell command that has
already escaped the harness.

Per-session placement proves the destination is exactly the non-symlinked injective
`baseDir/<sessionID>` path. Restore materializes into an empty sibling staging directory,
verifies it, renames the live root to a sibling backup, renames staging to the root, then
removes the backup. Failure of the second rename restores the backup. Startup removes
abandoned staging directories and restores an orphaned backup when the root is absent.
Generic `workspacestore.Materialize` keeps its fail-closed `DestNotEmptyError`; it never
gains recursive wipe behavior.

Shared placement admits concurrent harness sessions, humans, and external tools. Every
checkpoint is honestly stamped `fuzzy`; `SnapshotRequired` is invalid. Safety comes from
file-level optimistic concurrency and explicit fork/merge, not from pretending the tree
is isolated.

### Seeding

`Rig.NewSession` accepts `rig.WithSeedSnapshot(ref)`. It materializes the ref before
constructing loops and journals it as the first workspace checkpoint:

```text
acquire session/root leases
→ materialize seed
→ append SessionStarted
→ append WorkspaceCheckpointed{Ref: seed, Trigger: seed}
→ construct primers and append LoopStarted events
→ admit work
```

Seeding is valid for per-session roots, for an empty exclusive root, and never for a
shared root. The ref must resolve in the configured workspace store. Because the seed is
an ordinary checkpoint event, restore and manual GC need no seed-specific path.

### Snapshot triggers and priority

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

Unset resolves to `SnapshotOnIdle`; `SnapshotManual` remains explicit. Zero timeout
resolves to 60 seconds and negative timeout is invalid. Step/turn triggers choose the
boundary, not snapshot scope: every ref still covers the entire session workspace.

| Trigger | Boundary |
|---|---|
| Manual | Explicit idle `CheckpointWorkspace` call |
| OnIdle | `SessionActive → SessionIdle` |
| OnTurnDone | `TurnDone`, `TurnFailed`, or `TurnInterrupted` on any loop |
| OnStepDone | Every `StepDone` |

Best-effort means session progress wins. It permits one active automatic walk plus one
latest-wins pending trigger; coalesced edges get no checkpoint event. Activation cancels
an exclusive/per-session walk, while shared fuzzy walks may finish. Errors are observed
and the next eligible edge retries.

Required means persistence wins. Automatic boundaries are FIFO and never coalesced; the
triggering actor does not acknowledge its step/turn boundary, and session mutations stay
blocked, until the checkpoint commits or the session faults. Required idle/manual calls
close admission. Timeout or error latches a workspace-persistence fault and rejects
queued/new work. `SessionController.CheckpointWorkspace` remains permitted while this
specific fault is latched: a successful required checkpoint clears the fault and reopens
admission; another failure leaves it latched. `Shutdown` is always permitted. Shared
placement rejects required priority.

Manual calls are never coalesced, require idle, and use the configured priority. Caller
cancellation removes an unstarted request. Manual requests precede the one pending
best-effort automatic request.

### Native checkpoint boundary and workspace gate

Checkpoint scheduling is native session/loop control flow, not a public hook or an
asynchronous event subscriber. One session-scoped `WorkspaceCoordinator` is shared by
all primer and delegate loops. A loop has at most one active turn/step; parallel
workspace mutation comes from parallel tool calls and different loops.

| Operation | Coordinator permit |
|---|---|
| Structured known-path mutator | Shared mutation permit plus canonical path lock |
| Bash/unknown-path mutator | Exclusive whole-workspace mutation permit |
| Checkpoint walk | Exclusive snapshot permit |
| Restore/root replacement | Exclusive restore permit |

For an accepted automatic boundary, the responsible actor performs this deterministic
sequence before acknowledging the boundary:

```text
actual step/turn/session work reaches terminal boundary
→ acquire session exclusive snapshot permit
→ append triggering StepDone/TurnDone/SessionIdle durably
→ emit triggering event
→ snapshot the entire workspace
→ append WorkspaceCheckpointed durably with Header.Cause = triggering event
→ emit WorkspaceCheckpointed
→ release permit
→ acknowledge boundary and continue queued work
```

The trigger remains durable even when the snapshot fails; the checkpoint event exists
only after blob durability. An exclusive permit waits for active managed mutations and
blocks new ones, so recorded exclusive/per-session refs are quiescent. External writers
do not participate, which is why shared refs remain fuzzy.

### Checkpoint event and catalog fold

```go
type WorkspaceCheckpointed struct {
	enduring
	sessionScoped
	Header
	Ref         string              `json:"ref"`
	Consistency SnapshotConsistency `json:"consistency"`
	Trigger     SnapshotTriggerKind `json:"trigger"`
}

const (
	SnapshotConsistencyUnknown SnapshotConsistency = iota // legacy decode only
	SnapshotQuiescent
	SnapshotFuzzy
)

const (
	SnapshotTriggerKindUnknown SnapshotTriggerKind = iota // legacy decode only
	SnapshotTriggerManual
	SnapshotTriggerIdle
	SnapshotTriggerInterrupt
	SnapshotTriggerTurnDone
	SnapshotTriggerStepDone
	SnapshotTriggerSeed
)
```

Unknown consistency/trigger values decode old records but are never emitted. The event's
own coordinates remain session-only. `Header.Cause` identifies the firing edge for
idle/interrupt/turn/step and is zero for manual/seed; `Trigger` says why it fired.
`SnapshotQuiescent` means only that no harness-managed mutation overlapped the walk.

The catalog folds two different facts:

```go
type WorkspacePointer struct {
	Ref     workspacestore.Ref
	EventID uuid.UUID
	Seq     uint64
	Source  WorkspacePointerSource // checkpoint | restore
}

type CheckpointSummary struct {
	Ref         workspacestore.Ref
	EventID     uuid.UUID
	Seq         uint64
	Consistency SnapshotConsistency
}
```

`WorkspaceCheckpointed` updates both `LastCheckpoint` and `CurrentWorkspace`;
`WorkspaceRestored` updates only `CurrentWorkspace`. `CurrentWorkspace` means the
effective durable restore point, not a claim that the mutable live tree still equals its
ref. Thus checkpoint A → checkpoint B → restore A restarts from A, while backup/history
tools can still see B.

### Manual rewind

`RestoreWorkspace(ctx, ref)` is control-plane only and valid while idle. It takes the
exclusive restore permit, stages and verifies the ref, replaces the target safely, then
appends `WorkspaceRestored{Ref}` before reopening admission.

For per-session roots it uses the verified whole-root swap above. For fixed exclusive or
shared roots, it never recursively wipes or renames the configured root itself. Instead
it builds a manifest from the target ref, stages every replacement file as a contained
sibling temporary, obtains canonical path locks in sorted order, revalidates containment
and lease health, then commits file replacements/deletions deterministically. Before
commit it keeps rollback copies for every affected existing file; failure rolls back in
reverse order and returns a typed error. In shared mode this deliberately clobbers other
writers and is documented/authorized as an operator rewind. `WorkspaceRestored` is
appended only after the filesystem commit succeeds; append failure faults the session
because the live tree changed without advancing the durable pointer.

### File-tool optimistic concurrency and binding

`loop.Define` stores immutable `tool.Definition` blueprints, not live tool instances.
At new/restore time rig binds definitions with `SessionID`, `LoopID`, and an optional
`WorkspaceBinding{Root, Coordinator}`. Each primer/delegate receives fresh concrete
tools; modes on one loop reuse its bound instances. Restored loops start with fresh
ephemeral state. Subagent definitions receive only their parent-scoped delegate
controller, never the session controller.

`tools.Files(readGuard)` builds `ReadFile`, `WriteFile`, and `EditFile` around one private
per-loop observation map keyed by canonical contained path. A complete successful read
records raw-content SHA-256. Definitive not-found records absence; truncated, denied,
escaping, symlink, or ambiguous reads do not authorize mutation. No hash/version is
exposed to the model.

An existing-file overwrite/edit requires that loop's complete observation and an equal
current hash while holding the path critical section. Mismatch returns
`StaleFileError`, invalidates the entry, and asks the model to read again. Successful
mutation records the new hash. A write with no observation may create a currently absent
path using a sibling temporary and atomic no-replace publication; if the path exists or
another writer wins, it fails typed without clobbering. No failed read is required before
creating a genuinely new file.

Bash and other unknown-path mutators take the whole-workspace permit. When they finish,
the calling loop's entire file-observation map is invalidated because the harness cannot
know which paths changed. They do not gain file-level compare-and-swap guarantees;
approval/sandbox policy and workspace isolation remain the safety boundary for arbitrary
shell effects.

### Session-wide interrupt ordering

`Session.Interrupt` marks every live loop interrupt-pending before concurrently sending
commands; `loop.Controller.Interrupt` marks one loop and its delegate subtree; Subagent
`interrupt` affects only the owned child's current turn. User input remains queued,
machine-created delegate requests are flushed, and an interrupt admission barrier holds
until all targets are idle and `SessionIdle` is appended.

Barrier release is policy-specific: required idle holds through its checkpoint; required
turn/step holds through already accepted required boundaries and `SessionIdle`;
best-effort idle releases when the idle checkpoint is accepted; best-effort turn/step
releases after `SessionIdle` while any active/latest coalesced walk proceeds normally;
manual releases after `SessionIdle`; no-workspace releases immediately after it. An idle
trigger after an interrupt stamps `Trigger: interrupt`.

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

SWE and CLI are deliberately not migrated in this harness spec. Harness tests and
`pkg/serve` must compile and pass in the breaking harness commit itself. After harness
lands, the end-user documentation and runnable examples below must be completed before
consumer migration planning begins. Then write separate migration specs in this order:

1. CLI migration: replace its manual session factory, lifecycle calls, workspace
   wiring, checkpoint watcher, restore attachment, and old naming.
2. SWE migration: compose its primers, delegates, modes, tools, workspace policy, and
   session lifecycle on the documented harness surface.

The migration specs may rely on the completed guides rather than repeating the harness
architecture. Neither consumer migration is folded into the breaking harness change.

## Error model

Every package-level failure is typed and unwraps its cause where applicable:

- loop-definition errors: missing inference, invalid model, duplicate/empty name,
  invalid delegate;
- rig-definition errors: missing loops/primers/active primer/store, unknown references,
  duplicate singleton options, multiple/invalid placement, invalid workspace/snapshot
  pairing, persistence-path overlap, missing root leaser, and shared/required conflict;
- lifecycle errors: ID, lease, journal, appender, construction, restore, and workspace
  materialization stages;
- active-loop errors: unknown/exited target and durable active-loop change failure;
- loop-change errors: invalid change, invalid model/effort, wrong lifecycle phase, and
  durable append failure; and
- root lifecycle errors: busy/lost lease, unsafe destination, staging/root-swap/rollback,
  and unresolvable or invalid seed;
- checkpoint errors: unavailable/not idle, timeout, activation cancellation,
  snapshot/store failure, event append fault, required-fault latch, and shutdown
  cancellation classification; and
- tool errors: stale observation, atomic-create conflict, containment, and lease loss.

No validation path silently substitutes a missing required dependency. No partial rig,
session, topology, active-loop change, or loop model change is returned on failure.

## Concurrency and ordering

- A `Rig` is immutable after `Define` and safe to use concurrently to create or restore
  independent sessions.
- Every session receives its own lease, journal, appenders, ceiling, active-loop state,
  loop actors, tool bindings, observation maps, workspace coordinator, and checkpoint
  controller. Only explicit shared placement reuses a workspace root.
- Loop changes commit through the target actor and begin at the next turn boundary.
- Active-loop changes serialize through session state and become visible only after their
  enduring event commits.
- The session workspace coordinator orders managed mutation, native trigger append and
  emission, snapshot blob durability, and checkpoint append/emission. Restore excludes
  both mutation and checkpoint walks.
- Required boundaries serialize FIFO and withhold actor acknowledgement/admission;
  best-effort bounds pressure to one active plus one latest pending automatic trigger.
- Shutdown stops admission/work and checkpoint activity, releases the optional root
  lease, then releases the session lease.

## Fingerprints and durable events

The rig fingerprint replaces the single-loop configuration fingerprint. It includes:

- every loop definition's stable identity and immutable policy revision;
- the ordered primer set;
- the active primer;
- delegation edges;
- tool/security policy revisions;
- workspace placement policy `{mode, canonical root/base}` and runtime-skill mode fields;
  and
- other existing compatibility fields.

Runtime model/effort changes are durable events, not definition-fingerprint mutations.
They restore after the base definition passes compatibility checks.

New enduring events:

```text
ActiveLoopChanged
    session id, previous loop id, active loop id

LoopInferenceChanged
    session id, loop id, secret-free model descriptor, effort

LoopModeChanged
    session id, loop id, previous mode, selected mode

WorkspaceCheckpointed
    session id, ref, consistency, trigger, Header.Cause

WorkspaceRestored
    session id, ref
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
- restore folds of `LoopInferenceChanged` and `LoopModeChanged`.

### `pkg/tools`

- sync-only schema exposes only `start` with `wait:true`;
- managed schema exposes `start`, `send`, `wait`, `interrupt`, and `status`;
- unsupported actions are rejected by both schema validation and the scoped controller;
- missing action/wait preserve today's synchronous start behavior;
- Subagent JSON schema exposes optional start `mode`;
- omitted mode uses the target definition's initial mode;
- valid explicit mode constructs the child directly in that mode;
- unknown/unauthorized mode fails before quota reservation or loop creation;
- selected mode is carried on `LoopStarted` and restored; and
- requested mode cannot bypass parent/session permission clamps;
- `send` enqueues a distinct non-folding child turn and returns its request ID;
- `send(wait:true)` ignores intermediate steps and returns only the correlated
  `TurnDone.Message`;
- `send(wait:false)` plus `wait` resolves the same request after restore;
- `interrupt` affects only the owned child's current turn; and
- `status` returns bounded mechanical state without exposing an event cursor;
- each file-tool bundle shares one private observation set, while loops do not share it;
- complete read/not-found observation, stale hash invalidation, and successful mutation
  hash refresh;
- no-observation create succeeds only when atomic no-replace publication wins; an
  existing destination or concurrent winner returns typed without clobbering;
- existing-file write/edit without a fresh complete observation fails typed;
- Bash/unknown-path mutation takes the whole-workspace permit and invalidates that
  loop's observations; and
- tool definitions bind fresh instances per primer/delegate/restore while modes reuse
  one loop's bound instances.

### `pkg/rig`

- complete option validation matrix;
- multiple primers created as root loops;
- active primer selection;
- definition/delegate graph validation;
- delegation depth/quota are independent from per-loop tool iteration/call limits;
- no-workspace, exclusive, per-session, and shared placement validation matrices;
- two concurrent sessions share no session state or coordinator; exclusive aliases
  contend, per-session roots remain disjoint, and shared roots are explicit/fuzzy;
- failure at every new-session wiring stage releases the lease;
- create/shutdown/restore round trip with multiple primers and delegates;
- restored active loop and model/effort changes;
- seed ordering, validity, restore, and live-ref retention;
- unset trigger defaults to idle while manual remains distinct;
- idle/turn/step trigger selection, best-effort coalescing, required FIFO boundaries,
  fault recovery through manual checkpoint, and shared/required rejection;
- per-session staged swap/rollback/recovery and fixed-root manifest rewind/rollback;
- current-workspace versus last-checkpoint catalog semantics;
- unchanged snapshot performs no duplicate blob upload;
- timeout and both priority policies;
- shutdown joins the checkpoint controller; and
- no automatic workspace GC.

### `pkg/event` / `pkg/sessionstore`

- checkpoint codecs round-trip `Consistency`, `Trigger`, and `Header.Cause`;
- missing legacy trigger/consistency decode as unknown but producers never emit unknown;
- cause validation matches manual/seed/idle/interrupt/turn/step shapes while checkpoint
  coordinates remain session-scoped;
- checkpoint A, checkpoint B, restore A folds `LastCheckpoint=B` and
  `CurrentWorkspace=A`; and
- `WorkspaceRestored` append failures fault rather than misreport the live tree.

### `pkg/session`

- `SessionController` embeds `Session`, while `Session` exposes no policy-mutating
  methods;
- `Session.Loop` returns a read-only `loop.Handle` and
  `SessionController.LoopController` returns the trusted `loop.Controller`;
- `Submit` targets the current active loop;
- `SubmitToLoop` remains explicitly targeted;
- active-loop change durability and invalid-target atomicity;
- dynamically delegated loop can become active;
- parent-scoped delegate controllers reject sibling, ancestor, and unrelated loop IDs;
- non-folding delegate enqueue gives each follow-up request its own correlated turn;
- several queued child requests resolve independently by request ID;
- multi-root quiescence derives one correct `SessionIdle` edge;
- one session workspace coordinator excludes mutations from checkpoint/restore walks;
- native step/turn/idle ordering persists and emits the trigger before snapshot blob and
  checkpoint durability, then releases the gate and acknowledges the boundary;
- required failure leaves the trigger durable and faults before boundary acknowledgement;
- lease loss stops admission and cooperative commits;
- session/subtree/child interrupt scopes, queue preservation/flush, and policy-specific
  admission barrier release; and
- constructor visibility is enforced by dependency tests.

### `pkg/serve`

- `serve.Rig` contract and concrete structural assertion;
- create/restore routes call `NewSession`/`RestoreSession`;
- create reads `SessionID` from the returned session;
- idempotency behavior remains unchanged; and
- HTTP fixtures and error mapping remain wire-compatible.

### Integration

Integration tests over actual filesystem backends prove:

1. define a two-primer rig;
2. create a session and submit work to both loops;
3. change the active loop, model, and effort;
4. mutate the workspace;
5. reach session idle and checkpoint;
6. shut down;
7. restore a per-session workspace into a fresh base directory;
8. verify files, active loop, loop settings, and conversation state; and
9. continue work successfully;
10. seed two isolated sessions from one ref, diverge, checkpoint, and restore both to
    their own journal-authoritative trees;
11. contend for one exclusive root across two rig instances, release it cleanly, then
    exercise root-lease loss; and
12. keep filesystem persistence/snapshot blobs out of the workspace archive and reject
    overlapping persistence paths.

## Documentation deliverables

The implementation is not complete when only the APIs and tests pass. Its final task is
an end-user documentation and example pass that explains the composed system without
requiring readers to reconstruct it from package references.

Required documentation:

- a concepts page for `Rig → Session → Loop → Turn → Step`;
- package-oriented guidance showing how `rig`, `loop`, `session`, `storage`,
  `workspacestore`, and `tools` compose, including which package owns each lifecycle;
- a glossary and diagram for definitions, primers, active loop, modes, delegates, and
  session/loop controllers;
- a minimal single-loop rig quickstart;
- a multi-primer example with active-loop switching and explicit loop routing;
- a same-history plan/build mode example;
- synchronous Subagent (`wait:true`) and managed asynchronous Subagent examples covering
  `start`, `send`, `wait`, `interrupt`, and `status`;
- session creation, all optional workspace placements, seeding, manual/idle/turn/step
  snapshots, rewind, shutdown, and restore;
- capability and security guidance explaining data-plane versus control-plane APIs,
  delegation attenuation, permission gates, and session ceilings;
- `serve` integration and lifecycle examples;
- a breaking migration guide from `loop.Config`, `session.Compile`, and
  `session.Runner`; and
- runnable examples compiled in CI so documentation cannot silently drift from the API.

Documentation should lead with common workflows and progressively disclose the lower
level contracts. Package reference comments remain necessary but do not satisfy this
deliverable by themselves. Only after this documentation passes CI do the separate CLI
and then SWE migration specs begin.

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
delegates enable least-privilege dynamic spawning, and loop handles permit durable model
and effort changes. Workspace lifecycle is optional; when present, placement, seeding,
native checkpoint boundaries, optimistic file freshness, rewind, and persistence
priority are explicit harness policies rather than SWE-specific watcher code.
