# Design: workspace placement, seeding, and snapshot policy

**Date:** 2026-07-11
**Status:** Folded into `2026-07-10-rig-lifecycle-workspace-snapshots-design.md`;
retained as decision history
**Amended:** `2026-07-10-rig-lifecycle-workspace-snapshots-design.md`
**Builds on:** `2026-07-02-workspacestore-design.md`, the implemented
`pkg/workspacestore`, `pkg/sessionstore`, and
`session.SessionController.CheckpointWorkspace`

## Problem

The approved rig design has two correctness flaws and several unspecified seams.

1. **Shared workspace root contradicts concurrent sessions.**
   `rig.WithWorkspaceStore(store, root)` fixes one root for every session, while the
   concurrency section promises a rig safely creates and restores concurrent sessions.
   Two live sessions would materialize into and checkpoint the same directory: session
   A's on-idle snapshot captures session B's half-written tool output and durably
   records it as A's checkpoint; `RestoreSession` clobbers a directory another live
   session is using. The rig doc's own concurrency test list ("two concurrent sessions
   share no lease, ceiling, loops, or checkpoint controller") omits the workspace root
   because the root *is* shared.

2. **Torn snapshots after re-activation.** The rig doc lets an in-flight checkpoint
   walk continue across a `SessionActive` edge. Tools then mutate the tree mid-walk and
   the archive interleaves states that never coexisted — durably recorded as the restore
   point. Harness's own tools are inside the consistency boundary; the external-writer
   argument does not cover them.

3. There is no way to express the two real deployments — a local agent whose workspace
   is the user's actual directory, and a cloud agent whose sessions each need a fresh
   directory, optionally seeded from a knowledgebase snapshot — nor the pattern every
   shipped coding agent converged on for concurrent writers (fork at a known base,
   isolate, merge explicitly).

4. `WorkspaceCheckpointed` carries only `Ref`. Under trigger-based snapshotting the
   journal cannot say *why* a checkpoint fired, *which* turn or step it followed, or
   whether the captured tree was quiescent.

This design resolves all four, plus the small review items listed at the end. It
replaces the rig doc's `WithWorkspaceStore` option, its `SnapshotPolicy.Failure`
semantics, and its "Workspace snapshot policy" section. Everything else in the rig doc
stands.

## Decision

Workspace support is optional. When present, placement becomes a declared mode.
Seeding becomes a journaled first checkpoint. Snapshot triggers become a frequency
ladder over existing journal edges. The failure field becomes a priority policy that
decides who yields — the session or the snapshot — closing the torn-snapshot window by
construction. The checkpoint event carries explicit cause and consistency.

## Placement modes

Zero or one workspace placement option may be supplied. With none, the rig is a valid
non-workspace rig: it has no harness-managed workspace root, workspace snapshotting,
seeding, or rewind. Loop definitions remain workspace-agnostic; consumers may still
provide tools backed by resources outside the rig's workspace lifecycle.
`WithSnapshots` without a placement fails at `rig.Define`, and `WithSeedSnapshot`
without one fails at `NewSession`. Each placement option still requires
`WithSnapshots` so workspace persistence policy is explicit.

```go
// Local agent: the workspace IS the user's real directory. Exclusive.
rig.WithExclusiveWorkspace(workspaceStore, "/home/user/project", leases)

// Cloud agent: every session gets baseDir/<sessionID>. Exclusive by construction.
rig.WithSessionWorkspaces(workspaceStore, "/var/agents/workspaces")

// Local multi-session (Claude-Code-style): many writers, optimistically guarded,
// snapshots admitted fuzzy. Explicit opt-in; never the default.
rig.WithSharedWorkspace(workspaceStore, "/home/user/project")
```

Here `leases` implements `storage.Leaser` from `github.com/looprig/storage`. The
public API, Go package identifier, errors, and documentation consistently use the name
`storage`.
`WithExclusiveWorkspace` accepts the leaser explicitly because the workspace blob
store and the lease backend may have different scopes or providers. The other modes
take no root lease and therefore take no leaser argument.

| Mode | Root | Exclusivity | Restore materializes | Ref consistency |
|---|---|---|---|---|
| `WithExclusiveWorkspace` | fixed directory | one exclusive root lease | only if root is empty | always `quiescent` |
| `WithSessionWorkspaces` | `baseDir/<sessionID>` | per session by construction; no root lease | always | always `quiescent` |
| `WithSharedWorkspace` | fixed directory | none | never — manual rewind only | always `fuzzy` |

### No workspace

Omitting all three options disables the workspace subsystem rather than selecting an
implicit mode. Session construction does not resolve a root, materialize a snapshot,
start a checkpoint controller, or acquire a root lease. The workspace methods retained
on the uniform `SessionController` (`CheckpointWorkspace` and `RestoreWorkspace`)
return a typed `WorkspaceUnavailableError`. This keeps one controller API without
manufacturing a workspace for chat-only, orchestration-only, or externally managed
rigs.

### Persistence is outside the workspace

The session journal, catalog, leases, and workspace snapshot blobs must not live under
the snapshotted workspace root. Persisting `StepDone` or `WorkspaceCheckpointed` inside
that root would itself mutate the tree being checkpointed and make a stable final ref
impossible. Filesystem-backed providers whose storage locations are discoverable fail
`rig.Define` with a typed overlap error when a persistence directory is equal to or
contained by the workspace root. A persistence directory that is an ancestor of the
workspace is not by itself recursive; only files beneath the workspace are captured.
Providers without a discoverable local path must document and honor the same
composition contract.

The persistence backend may use the same disk or host; it must simply be outside the
workspace tree. Workspace restore and root replacement therefore cannot overwrite the
session's own journal or snapshot blobs.

There is deliberately **no root-factory closure** (`func(sessionID) string`). A closure
cannot be validated at `Define`, cannot be fingerprinted, and cannot be reasoned about
for exclusivity. Three declarative modes cover the real deployments; a fourth deployment
gets a fourth mode.

### Root lease (`WithExclusiveWorkspace`)

- `WithExclusiveWorkspace(workspaceStore, root, leases storage.Leaser)` always acquires
  one exclusive lease. The existing `storage.Leaser` contract is exclusive, so the
  rig does not invent reader/writer semantics or try to infer whether every tool is
  read-only. Consumers that intentionally want concurrent attachment use
  `WithSharedWorkspace` and accept fuzzy snapshots.
- The root identity is canonicalized with `filepath.Abs`, `filepath.Clean`, and, for an
  existing root, `filepath.EvalSymlinks`. The valid backend name is
  `workspace-roots/<sha256(canonical-root)>`; hashing avoids leaking host paths and
  guarantees compliance with the `storage` name grammar. Lexical and symlink aliases
  therefore contend for the same lease.
- Lease scope equals the configured `storage.Leaser` backend's scope: a process-local
  backend guards one host, while a shared backend can guard a fleet. The rig does not
  claim a broader exclusion boundary than its backend provides.
- `NewSession`/`RestoreSession` mint or resolve the session ID, acquire the normal
  session lease, then acquire the root lease before opening the journal, materializing,
  or constructing loops. Contention returns
  `WorkspaceRootBusyError{Root, HolderEpoch}` and releases the session lease; no durable
  session record is appended. `HolderEpoch` is the identity exposed by
  `storage.LeaseHeldError`; the lease backend does not know the holder's session ID.
- The session watches `storage.Lease.Lost()`. Loss faults the session, rejects new work,
  interrupts live loops, and cancels in-flight checkpoints; continuing to mutate after
  exclusion is lost would violate this mode's defining guarantee.
- Restore with a non-empty root attaches without materializing — the local directory is
  authoritative and is never silently rolled back. Restore with an empty root
  materializes the effective `CurrentWorkspace` ref.
- Shutdown first stops admission and work, waits for or cancels checkpoint activity,
  then releases the root lease and finally the session lease. Release is best-effort
  and context-bounded, following the existing session-lease discipline.

### Per-session roots (`WithSessionWorkspaces`)

- Root is derived injectively from the session ID; the session lease is the only lock
  needed.
- Restore always rematerializes the effective `CurrentWorkspace` ref; any later residue
  in the per-session root is discarded. In this mode the journal is authoritative and
  per-session directories are disposable — which is what gives sessions mobility
  across machines.
- Replacement never asks the generic `workspacestore.Materialize` operation to wipe a
  non-empty destination. The placement layer creates an empty sibling staging
  directory under `baseDir`, materializes and verifies the ref there, renames the
  current derived root to a sibling backup, renames staging to the root, then removes
  the backup. If the second rename fails, it restores the backup before returning a
  typed restore error.
- This destructive path is valid only after proving that root is exactly the
  injectively derived `baseDir/<sessionID>`, neither an arbitrary caller path nor a
  symlink. Startup removes abandoned uncommitted staging directories and, if the root
  is absent but a backup remains, restores the backup before retrying from the durable
  `CurrentWorkspace` pointer. Generic materialization retains its existing fail-closed
  `DestNotEmptyError` behavior.

### Shared mutable root (`WithSharedWorkspace`)

The pattern local coding agents (Claude Code, Codex CLI, OpenCode) ship implicitly,
made explicit and typed:

- No root lease. Multiple writer sessions, plus the human, share the directory.
- Snapshots still fire per the configured trigger (default `SnapshotOnIdle`), capturing
  the directory as-is — other writers' edits included. Every ref is stamped `fuzzy`.
  Checkpoints are honest history/backup, not proof of session state.
- Restore never materializes automatically; the live directory is authoritative.
  Rolling back is a deliberate operator action via `RestoreWorkspace` (below).
- `SnapshotRequired` is invalid in this mode (typed error at `rig.Define`): a
  hard-guaranteed fuzzy backup is a contradiction.
- Safety at file granularity comes from the unconditional tool-level optimistic
  concurrency described below, and from the human as merge arbiter — exactly the
  shipped-product model, with the guarantees written down.

## Seeding

A seed is a checkpoint that predates the session. `Rig.NewSession` gains session
options:

```go
sess, err := r.NewSession(ctx, rig.WithSeedSnapshot(kbRef))
```

`NewSession` materializes `kbRef` into the fresh root before constructing loops. The
exact successful ordering is:

```text
acquire session/root leases
→ resolve and materialize the seed
→ append SessionStarted
→ append WorkspaceCheckpointed{Ref: kbRef, Trigger: seed}
→ construct primers and append LoopStarted events
→ admit work
```

The seed is the first workspace checkpoint and precedes every loop and turn, but it is
not claimed to be the journal's first record or event: backend fencing and
`SessionStarted` may precede it. If the checkpoint append fails, construction faults
and cleans up without constructing loops. Because the seed enters the journal as an
ordinary checkpoint event:

- restore needs no seed-specific code — the event initializes `CurrentWorkspace`, and
  later checkpoint or rewind events update that same fold;
- manual GC needs no new code — live-set computation already walks journals for refs,
  so a seed is live while any retained session references it; and
- content-addressed deduplication lets a thousand sessions seeded from one
  knowledgebase ref share the same stored blob.

Validity: the seed ref must resolve in the configured workspace store (cross-store
import is the consumer's composition job, like opening backends). Seeding is valid in
`WithSessionWorkspaces` always, in `WithExclusiveWorkspace` only when the root is
empty, and never in `WithSharedWorkspace` (it would clobber other writers). Violations
are typed errors before any session state is created.

### Concurrent writers: fork/merge, never shared mutable state

Seeding N per-session roots from one ref makes each session a branch with the seed as
merge-base. Sessions work isolated; their checkpoint refs are merged afterwards —
three-way against the seed, or by materializing refs as git branches and letting git
arbitrate. This is the pattern cloud agents (Codex, Devin, Cursor background agents)
converged on. Merge machinery itself is out of scope here; the rig provides the fork.

## Snapshot triggers

`SnapshotTrigger` becomes a frequency ladder over edges the journal already emits
(`TurnDone`, `TurnFailed`, `StepDone`, `SessionActive`/`SessionIdle`):

```go
const (
	SnapshotTriggerUnset SnapshotTrigger = iota
	SnapshotManual
	SnapshotOnIdle
	SnapshotOnTurnDone
	SnapshotOnStepDone
)
```

`SnapshotTriggerUnset` is configuration-only and is never emitted. `rig.Define`
resolves it to `SnapshotOnIdle`; making `Manual` nonzero preserves a real default while
keeping manual-only scheduling explicit.

| Trigger | Schedules on | Density | Tree consistency |
|---|---|---|---|
| `SnapshotManual` | explicit `CheckpointWorkspace` while idle | caller-controlled | `quiescent` in exclusive/per-session; `fuzzy` in shared |
| `SnapshotOnIdle` *(resolved default)* | `SessionActive → SessionIdle` edge | once per work burst | `quiescent` in exclusive/per-session; `fuzzy` in shared |
| `SnapshotOnTurnDone` | each turn-terminal event (`TurnDone`/`TurnFailed`/`TurnInterrupted`) on any loop | once per turn | `quiescent` in exclusive/per-session when captured; `fuzzy` in shared |
| `SnapshotOnStepDone` | every `StepDone` | many per turn | `quiescent` in exclusive/per-session when captured; `fuzzy` in shared |

- Content addressing avoids uploading an identical archive again. Every attempted
  snapshot still pays tree-walk and hashing I/O, and every unique archive consumes
  storage; these costs are the practical limit on `SnapshotOnStepDone` for large trees.
- Step/turn triggers choose a boundary, not a smaller snapshot scope: the ref still
  captures the entire session workspace and `Header.Cause` identifies the triggering
  loop/turn/step.
- No shutdown trigger: orderly shutdown from idle already has a fresh `OnIdle` ref, and
  an interrupt sweep ends in an idle edge that checkpoints too (see "Session-wide
  interrupt"), so there is no shutdown path left that silently skips a wanted snapshot —
  `Manual` covers anything else.
- `Manual` remains available on top of any configured trigger.

### Native checkpoint boundary and workspace gate

Checkpointing is native session-runtime control flow, not a public hook and not an
asynchronous subscriber reacting sometime after an event. Every session with a
workspace owns one `WorkspaceCoordinator` shared by all primer and delegate loops.
There is at most one active turn and one active step inside a loop; steps are
sequential. Parallelism comes from tool calls within a step and from different loops,
which is why the workspace gate is session-scoped rather than loop-scoped.

The coordinator grants these internal permits:

| Operation | Permit |
|---|---|
| `WriteFile` / `EditFile` / known-path mutator | shared workspace-mutation permit plus its path lock |
| `Bash` / unknown-path mutator | exclusive whole-workspace mutation permit |
| checkpoint walk | exclusive snapshot permit |
| workspace restore/root replacement | exclusive restore permit |

An exclusive snapshot permit waits for active managed mutations to finish and blocks
new managed mutations until the checkpoint boundary completes. Read-only inference and
event observation do not mutate the workspace; queued work may be accepted, but policy
decides when its execution or next mutation may proceed. External processes do not
participate, so shared placement remains `fuzzy`.

For an accepted automatic boundary, the step/turn/session actor executes this sequence
directly before acknowledging the boundary and allowing subsequent managed mutation:

```text
actual step/turn/session work reaches its terminal boundary
→ acquire the session's exclusive snapshot permit
→ append the triggering StepDone/TurnDone/SessionIdle event durably
→ emit that triggering event to the session fan-out
→ snapshot the entire session workspace
→ append WorkspaceCheckpointed durably with Header.Cause = triggering event
→ emit WorkspaceCheckpointed
→ release the snapshot permit
→ acknowledge the boundary and continue queued work
```

The trigger event comes before the snapshot commit event because it records an
execution fact that remains true even if snapshotting fails. `WorkspaceCheckpointed`
comes only after the snapshot blob is durable. A failure therefore never erases a
completed step/turn/idle transition, and a checkpoint event never points at a missing
blob. The journal and blob backends are outside the workspace, so appending either
event does not perturb the captured tree.

`LoopIdle` remains a native loop event but is not a snapshot trigger in this policy. It
feeds session quiescence; once every loop is idle, the session performs the same native
boundary around `SessionIdle`.

### Trigger backpressure and coalescing

Under `SnapshotBestEffort`, each session checkpoint controller permits one in-flight
checkpoint and one pending automatic trigger. When more automatic edges arrive while a
walk is running, the pending slot is latest-wins: replace it with the newest edge and
its `Header.Cause`. After the walk finishes or is cancelled, the controller processes
that latest pending edge if it remains eligible. Skipped or coalesced edges append no
checkpoint event. A delayed best-effort snapshot is quiescent while its walk runs, but
does not claim to reproduce the exact earlier trigger-time tree.

Under `SnapshotRequired`, automatic triggers are never coalesced. Each loop can have at
most one outstanding boundary because its steps and turns are sequential; required
boundary requests wait FIFO at the session checkpoint controller. The actor does not
acknowledge its step/turn boundary or advance that loop until its checkpoint commits or
the session faults. Required `OnStepDone` therefore deliberately trades throughput for
one durable checkpoint event per step.

Manual `CheckpointWorkspace` calls are never coalesced: each caller is serialized
through the same controller and receives its own ref or error. Caller context
cancellation removes a request that has not started. Manual callers are serviced before
the single pending best-effort automatic trigger so sustained step traffic cannot
starve control-plane work.

## Snapshot priority

`SnapshotPolicy` keeps its shape; the failure field is promoted from a failure handler
to a priority policy answering one question — **when snapshot and session progress
conflict, who yields?**

```go
type SnapshotPolicy struct {
	Trigger  SnapshotTrigger
	Priority SnapshotPriority
	Timeout  time.Duration // zero resolves to 60s; negative invalid
}

const (
	SnapshotBestEffort SnapshotPriority = iota // session wins, always
	SnapshotRequired                            // snapshot wins; session yields
)
```

| Conflict | `SnapshotBestEffort` (session-first) | `SnapshotRequired` (snapshot-first) |
|---|---|---|
| Input arrives mid-walk | Session wins: cancel an exclusive/per-session walk. An automatic trigger re-arms on its next eligible edge; a manual caller receives a retryable cancellation. Shared mode lets the walk finish and stamps `fuzzy`. | Snapshot wins: admission defers — input queues, no turn starts until the ref commits, bounded by `Timeout`. |
| Snapshot error | Record the typed failure through the observability seam; session continues. Automatic scheduling retries on its next eligible edge; manual returns the error. | Latch the workspace-persistence fault, wake idle waiters, reject queued and new input until an operator acts. |
| Snapshot timeout | Treated as error → move on. | Treated as error → fault. |
| Walk cancelled by clean shutdown | Not an error (unchanged from the rig doc). | Not a fault (unchanged). |

Consequences:

- **The managed torn-snapshot window disappears under `Required` by construction.** At
  idle/manual boundaries, admission stays closed. At step/turn boundaries, the
  triggering actor withholds its boundary acknowledgement and every loop's mutating
  tools wait behind the session workspace gate. Each exclusive/per-session ref is
  therefore `quiescent` even when sibling loops continue read-only inference or emit
  non-mutating events.
- `SnapshotRequired` is valid with `SnapshotManual`, `SnapshotOnIdle`,
  `SnapshotOnTurnDone`, and `SnapshotOnStepDone`. Required step/turn policies accept
  potentially high latency: every boundary waits for its serialized snapshot, capped
  by `Timeout`; failure leaves the trigger event durable and faults the session before
  that actor advances.
- Under `BestEffort`, a snapshot is recorded only while the coordinator holds the same
  exclusive permit, so recorded exclusive/per-session refs remain `quiescent`.
  Best-effort may cancel, skip, or coalesce a boundary instead of blocking progress.
- `WithSharedWorkspace` forces `SnapshotBestEffort`; `Required` there is a typed
  `rig.Define` error.

Manual calls use the same policy rather than bypassing it. `CheckpointWorkspace`
requires an idle session. Under `BestEffort`, activation cancels the walk and the call
returns a retryable error. Under `Required`, the controller closes admission before the
walk, queues incoming input until commit, and applies the same timeout and fault-latch
rules as a required idle checkpoint. Shared mode remains best-effort and `fuzzy`.

## Checkpoint event, correlation, and rewind

### `WorkspaceCheckpointed` uses `Header.Cause` and gains consistency

There is still exactly one journal event per checkpoint, and still no separate
"snapshot saved" event: snapshot-blob durability is guaranteed by the existing
snapshot-before-append ordering (`ws.Snapshot` returns before the event is appended), so
the journal can never hold a dangling ref, and a crash between the two steps leaks an
unreferenced blob for manual GC. The event is the commit record.

```go
type WorkspaceCheckpointed struct {
	enduring
	sessionScoped
	Header                                    // Coordinates are session-only; Cause identifies the firing edge
	Ref         string                `json:"ref"`
	Consistency SnapshotConsistency   `json:"consistency"`     // quiescent | fuzzy
	Trigger     SnapshotTriggerKind   `json:"trigger"`         // manual | idle | interrupt | turn_done | step_done | seed
}

const (
	SnapshotConsistencyUnknown SnapshotConsistency = iota // legacy decode only; never emitted
	SnapshotQuiescent
	SnapshotFuzzy
)

const (
	SnapshotTriggerKindUnknown SnapshotTriggerKind = iota // legacy decode only; never emitted
	SnapshotTriggerManual
	SnapshotTriggerIdle
	SnapshotTriggerInterrupt
	SnapshotTriggerTurnDone
	SnapshotTriggerStepDone
	SnapshotTriggerSeed
)
```

The existing `Header.Cause` is the direct causal edge and already embeds
`identity.Coordinates` plus `EventID`; no checkpoint-specific cause field is added.
`Header.Coordinates` still identifies the checkpoint itself and therefore contains only
`SessionID`. `Header.Cause` does not change the event's session scope:

| Trigger | `Header.Cause` |
|---|---|
| `turn_done` | terminal turn event's `EventID` and `SessionID+LoopID+TurnID` |
| `step_done` | `StepDone.EventID` and `SessionID+LoopID+TurnID+StepID` |
| `idle` | firing `SessionIdle.EventID` and `SessionID` |
| `interrupt` | post-sweep `SessionIdle.EventID` and `SessionID`; `Trigger` distinguishes it from ordinary idle |
| `manual` | zero; the direct API call has no journal event to reference |
| `seed` | zero; the session option has no preceding journal event to reference |

**`Trigger` says why the walk fired; `Header.Cause` identifies the firing edge; journal
order says what the snapshot covers.** One walk captures the whole tree — all loops'
work up to the checkpoint's `JournalSeq` — not just the triggering turn's edits.
"Rewind to turn X" resolves as the first `WorkspaceCheckpointed` with `JournalSeq`
greater than turn X's terminal-event seq, or directly via
`Header.Cause.Coordinates.TurnID` under the turn trigger.

`SnapshotQuiescent` has a deliberately bounded meaning: no harness-managed workspace
mutation overlapped the archive walk. It does not claim that a human, IDE, watcher, or
uncooperative external process left the filesystem untouched; the root lease only
coordinates participants using its `storage.Leaser`. Filesystem-wide transactional
proof would require cooperation the rig does not control.

Consistency is determined mechanically: `WithSharedWorkspace` always stamps `fuzzy`;
every recorded exclusive/per-session ref is `quiescent` because its walk holds the
session workspace's exclusive snapshot permit. Sibling loops may remain active, but
their managed mutations wait behind that permit. An idle edge produced by an interrupt
sweep stamps `Trigger: interrupt` — the session knows it just killed the work rather
than watched it complete — with consistency rules identical to `idle`. Tooling can
thereby distinguish "state after finished work" from "state after a panic stop"
without scanning for preceding `TurnInterrupted` events.

`Ref` remains content identity, not checkpoint identity: two checkpoints over an
unchanged tree carry the same `v1:sha256:<hex>` ref. The unique per-checkpoint key is
the event's `EventID` (plus `JournalSeq`); turn→ref is many-to-one.

### Catalog fold

The catalog distinguishes the latest snapshot produced from the workspace from the ref
that the live workspace is currently meant to represent:

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

// Fields folded into the session catalog:
LastCheckpoint   CheckpointSummary
CurrentWorkspace WorkspacePointer
```

The fold is deterministic:

| Event | `LastCheckpoint` | `CurrentWorkspace` |
|---|---|---|
| `WorkspaceCheckpointed{Ref:B}` | B | B, source `checkpoint` |
| `WorkspaceRestored{Ref:A}` | unchanged | A, source `restore` |

This distinction matters for `checkpoint A → checkpoint B → restore A → shutdown`:
`RestoreSession` must materialize A, not silently undo the deliberate rewind by loading
B. Restore uses `CurrentWorkspace`; backup/history tooling may use `LastCheckpoint`.
Manual GC still derives its complete live set from retained journals rather than one
catalog pointer.

### Manual rewind: `RestoreWorkspace`

"Restore never materializes" in fixed and shared modes requires the deliberate
counterpart to exist:

```go
type SessionController interface {
	// ...existing methods...
	RestoreWorkspace(ctx context.Context, ref workspacestore.Ref) error
}
```

Control-plane only, valid only while the session is idle, materializes the named ref
over the workspace root, and durably appends a new enduring event before returning:

```text
WorkspaceRestored
    session id, ref
```

The journal thereby records that the tree changed out from under the conversation
history. Appending the event advances `CurrentWorkspace` but not `LastCheckpoint`; the
event is the durable commit point for the new effective workspace ref. In
`WithSharedWorkspace` this is the operator-invoked rewind and is documented as
clobbering other writers — that is what rewinding a shared directory means.

## Session-wide interrupt

`Session.Interrupt(ctx) (bool, error)` is the data-plane panic button and is
**session-wide**: interrupting only the active loop while its delegates keep grinding
means the stop button didn't stop the machine. Three scopes, one mechanism:

| Surface | Scope | Queues |
|---|---|---|
| `Session.Interrupt` | every live loop | delegate request queues flushed; user input preserved |
| `loop.Controller.Interrupt` | that loop **and its delegate subtree** | same flush rule, within the subtree |
| Subagent `interrupt` action | one owned child, current turn only | untouched — the parent owns its queued requests |

**Select hierarchically, deliver flat.** The ownership tree decides who is in scope
(a loop's interrupt covers its delegate subtree, because interrupting the planner
should stop the builder it is waiting on); delivery never routes through parents. The
session owns the flat registry of live loops with parent provenance, so it sends the
interrupt command directly and concurrently into every targeted actor's mailbox — one
hop each, no dependence on a parent that is itself blocked mid-`wait`, no depth-N
cascade through busy mailboxes. Interrupting an idle loop is a no-op, so delivery is
idempotent and unordered.

One ordering guard: the session marks the entire target set interrupt-pending in
session state **before** fan-out. Otherwise a parent whose child's `TurnInterrupted`
resolves its pending `wait` could take another step — or issue a fresh `send` — in the
window before its own interrupt lands. Actors observe the mark at step boundaries.

**Queue semantics follow initiative.** User-submitted queued input survives an
interrupt — it is expressed user intent. Machine-initiated queued work is flushed:
pending delegate requests resolve as typed interrupted results (the existing
`TurnInterrupted` → interrupted-request mapping), because their initiators — parent
turns — are themselves being interrupted; a parent that still wants the work re-sends.
Without the flush, "stop everything" resumes itself the moment each child's current
turn dies and its next queued request starts.

Preservation does not mean immediate redispatch. The interrupt-pending mark also closes
an **interrupt admission barrier** before fan-out. User input remains queued behind that
barrier while the session waits for every targeted loop to acknowledge interruption
and become idle. The session then appends the normal `SessionIdle` edge and hands its
trigger to the checkpoint controller before it can dispatch preserved input. This
guarantees an observable idle boundary rather than letting queued work restart in the
last-interrupt-to-idle window.

Barrier release follows snapshot policy:

- required `OnIdle`: hold through the interrupt-stamped idle checkpoint commit or the
  latched fault;
- required `OnTurnDone`: hold until every required `TurnInterrupted` boundary accepted
  by the sweep commits or faults, and until `SessionIdle` is appended;
- required `OnStepDone`: hold until already-accepted required step boundaries drain and
  `SessionIdle` is appended; interruption itself does not manufacture a `StepDone`;
- best-effort `OnIdle`: release once the idle boundary checkpoint is accepted;
- best-effort `OnTurnDone`/`OnStepDone`: release after `SessionIdle` is appended; any
  active or latest coalesced terminal/step snapshot proceeds under normal best-effort
  rules;
- `SnapshotManual`: release after `SessionIdle`, with no automatic checkpoint; and
- no workspace: release immediately after `SessionIdle`.

The returned bool reports whether anything was actually running. Interrupting a fully
idle session returns false and creates neither a barrier transition nor an idle event.

### Snapshot on interrupt

The post-interrupt checkpoint uses the same native boundaries: the sweep ends in
quiescence and the session persists and emits `SessionIdle` under its admission
barrier. Under an idle trigger, the session then checkpoints before releasing that
barrier and stamps `Trigger: interrupt` so tooling can tell a panic-stop state from
completed work. Consequences per policy:

- Under required `OnIdle`, admission stays gated until the post-interrupt ref commits:
  a panic stop yields a durable workspace state before new work is admitted.
- Under best-effort `OnIdle`, the barrier releases after the idle checkpoint is
  accepted; preserved input may then cancel an exclusive/per-session walk. Shared mode
  lets the walk finish and stamps `fuzzy`.
- Under `OnTurnDone`, each `TurnInterrupted` is a native terminal boundary and uses
  `Trigger: turn_done`; required mode waits for every such checkpoint, while
  best-effort may coalesce them.
- Under `OnStepDone`, interruption creates no artificial step event or checkpoint; only
  step boundaries completed before the interruption participate.
- Under `SnapshotManual`, no automatic snapshot fires — the policy is respected;
  `CheckpointWorkspace` remains available to the composition root.

The journal stays coherent with the workspace: `TurnInterrupted` events record which
turns died; an idle-triggered interrupt checkpoint records the tree they collectively
left behind.

## Tool-level optimistic concurrency (unconditional)

Independent of placement mode, the file tools adopt the guards that make any sharing
survivable. The model-facing `ReadFile`, `WriteFile`, and `EditFile` schemas do not gain
a hash or version parameter. Freshness is private runtime state owned by the tools.

### Ownership and placement

The implementation lives in `pkg/tools/file_observations.go`, beside the file tools:

```go
type fileObservations struct {
	mu     sync.Mutex
	byPath map[string]*filePathState // canonical path; lookup only, never ordered
}

type filePathState struct {
	mu          sync.Mutex // held across read/record or check/write/record
	observed    bool
	observation fileObservation
}

type fileObservation struct {
	exists   bool
	hash     [sha256.Size]byte
	complete bool
}
```

`pkg/tools.NewFileTools(root string, guard loop.ReadGuard)` creates one private
observation set and injects it into that bundle's `ReadFile`, `WriteFile`, and
`EditFile`. Rig loop instantiation creates a fresh bundle per loop. The `loop` package
still receives an ordinary `ToolSet` and knows nothing about workspaces or file
observations.

The lifetime follows the model history that could know the file contents:

- modes on one loop share the same observations because they share the loop and tools;
- every primer and delegate loop gets an independent set — one loop's read never
  authorizes another loop's write;
- restored or rehydrated loops start empty and must read again; and
- observations are ephemeral runtime state: never journaled, checkpointed, folded into
  session state, or exposed to the model.

The map is deterministic by construction: keys are canonical contained paths and every
operation is a direct lookup. Correctness never depends on Go map iteration order.

### Observation and mutation rules

1. A successful, non-truncated `ReadFile` hashes the complete raw file bytes and records
   `{exists:true, hash, complete:true}` after the read. Selecting a line range does not
   weaken the observation when the tool read the complete underlying file.
2. A definitive not-found result, after containment and read-permission checks, records
   `{exists:false}`. Permission, containment, symlink, or ambiguous I/O failures record
   nothing.
3. A truncated read may record `{exists:true, complete:false}` for diagnostics but does
   not authorize a mutation.
4. Before overwriting or editing an existing path, the tool takes the observation set's
   per-path critical section, requires a complete present observation, and hashes the
   current raw bytes.
5. Missing, incomplete, existence-mismatched, or hash-mismatched observations on an
   existing path fail with typed `StaleFileError{Path}`. The tool removes the stale
   observation and returns only a model-safe instruction to read the file again; hashes
   are never exposed.
6. A `WriteFile` with no observation may create a path that is currently absent. It
   prepares the complete sibling temporary file and publishes it with atomic
   no-replace semantics. If the destination already exists or another writer wins the
   race, creation fails typed and never clobbers that file. A prior not-found read may
   still record absence but is not required for creation.
7. On an overwrite match, `WriteFile` performs its existing
   temp-file-plus-atomic-rename write.
   `EditFile` additionally requires its exact content anchor to match before performing
   the same atomic write.
8. A successful mutation replaces the observation with the hash of the new complete
   contents, which the loop now knows because it produced them. An ambiguous write
   failure invalidates the observation.

An existing-file `WriteFile` therefore cannot overwrite content that this loop has not
completely observed. New-file creation needs no failed `ReadFile` round trip, yet an
unobserved existing file can never be overwritten.

The per-path critical section makes read/check/write/record deterministic among the
three file tools sharing one bundle. Existing runner `WriteTarget` serialization
remains useful for parallel calls. An uncooperative external process can still change a
file in the tiny interval between the final hash check and atomic rename; portable
filesystems do not provide content-hash compare-and-swap. The design detects ordinary
stale reads and guarantees atomic file replacement, but does not claim transactional
isolation from arbitrary external writers or cross-file semantic isolation. Those needs
still route to workspace partitioning or fork/merge.

## Tool definitions bind per session loop

`loop.Define` cannot store already-running tool instances: workspace roots differ per
session, file observations belong to one loop, and delegate control is parent-scoped.
It stores immutable `tool.Definition` blueprints instead:

```go
builder := loop.Define(
	loop.WithTools(
		tools.Files(readGuard),
		tools.Bash(),
		tools.Subagent(),
	),
)
```

Conceptually, one definition may build a bundle of concrete tools that share private
runtime state:

```go
type Definition interface {
	Build(context.Context, Bindings) ([]InvokableTool, error)
}

type Bindings struct {
	SessionID uuid.UUID
	LoopID    uuid.UUID
	Workspace *WorkspaceBinding // nil for a rig without workspace lifecycle
}

type WorkspaceBinding struct {
	Root        string
	Coordinator WorkspaceCoordinator
}
```

At `NewSession`/`RestoreSession`, rig resolves the session workspace root and creates
one session `WorkspaceCoordinator`. For every primer or delegate loop, it binds the
loop's tool definitions with that loop's IDs and the session workspace binding.
`tools.Files` then creates fresh `ReadFile`/`WriteFile`/`EditFile` instances sharing one
new per-loop observation map; all loops share only the session coordinator. Subagent
definitions receive the separate parent-scoped `DelegateController` capability and do
not receive `SessionController`.

Modes reuse the bound instances because modes share one loop and history; a mode only
selects which prebound tool definitions are visible. A new delegate gets fresh tools.
A restored or rehydrated loop also gets fresh ephemeral tool state and an empty file
observation map. The loop runtime ultimately receives only the concrete `ToolSet`, so
`pkg/loop` remains workspace-agnostic.

Definitions declare required bindings. `rig.Define` rejects a workspace-requiring tool
definition when no workspace placement exists. The resolved root is canonicalized and
contained when the definition binds, which handles `WithSessionWorkspaces` roots that
do not exist at rig-definition time without introducing a root-factory closure.

## Fingerprints

The rig fingerprint records the placement **policy**, not resolved paths:
`{mode: none|exclusive|per_session|shared, base: <canonical root or baseDir>}`.
Per-session roots therefore do not churn fingerprints; relocating the base is covered
by the existing mismatch escape hatch. This replaces the single `WorkspaceRoot`
fingerprint field for rig-composed sessions. `none` records that workspace lifecycle
support was deliberately omitted.

## Additional amendments to the rig design

Small items from the same review, adopted here so the rig doc needs no second pass:

1. **Subagent envelope timeout**: `Timeout time.Duration` in the model-facing JSON is
   replaced by `timeout_seconds` (integer). `encoding/json` treats `time.Duration` as
   integer nanoseconds; a model will send `"60s"` or `60` and a strictly validated
   envelope must not misinterpret it. Absent-vs-zero handling for `request_id` is
   specified: absent means "not supplied"; a zero UUID is rejected.
2. **Synchronous wait default**: `start`/`send` with `wait:true` and no
   `timeout_seconds` waits unbounded but interruptibly — the parent's own turn
   interrupt/cancellation is the escape hatch. Suppliers of `timeout_seconds` get a
   typed timed-out request result.
3. **`Session.Interrupt` scope**: session-wide, with subtree and single-child
   variants — specified in "Session-wide interrupt" above.
4. **No child `stop` action — intentional.** An idle child costs no inference and is
   quiescent for `SessionIdle` purposes; what it holds — its committed history — is a
   reusable asset, so children are deliberately kept warm: a follow-up `send` to a child
   with full context beats respawning and re-briefing a cold one. Accumulation is
   bounded by `DelegationLimits.Quota`, which never replenishes precisely so a
   spawn→kill→spawn churn loop cannot manufacture unbounded total work. Every "stop"
   use case is covered by a narrower tool: runaway work → `interrupt` (kills the turn,
   keeps the loop); operator cleanup → the trusted `SessionController`; session over →
   `Shutdown`. Idle children stay in memory for now; parking them (evict history,
   rehydrate from the journal on next `send`) is a future internal optimization tracked
   in `docs/TODO.md`.
5. **Event rename**: `LoopChanged` → `LoopInferenceChanged`. It carries exactly the
   secret-free model descriptor and effort; the name should say so and stop the event
   becoming a grab-bag.
6. **Active-primer default**: with exactly one primer, `WithActivePrimer` may be
   omitted and defaults to it; with multiple primers it remains required. The invariant
   becomes "exactly one active primer is *resolved*."

## Error model additions

All typed, all unwrapping their cause where applicable, consistent with the rig doc:

- placement errors: multiple placement options, unknown mode, non-canonical root/base,
  missing/nil `storage.Leaser` for `WithExclusiveWorkspace`, snapshot policy without a
  placement, discoverable persistence/workspace path overlap, seed invalid for mode,
  seed ref unresolvable in the workspace store;
- lease errors: `WorkspaceRootBusyError{Root, HolderEpoch}` wrapping
  `storage.LeaseHeldError`, and `WorkspaceRootLeaseLostError{Root, Epoch}` wrapping or
  classifying the corresponding `storage` lease-loss cause;
- policy errors: `Required` with shared mode, unknown trigger/priority, negative
  timeout;
- checkpoint errors: unchanged (timeout, snapshot/store failure, append fault,
  shutdown-cancellation classification), plus not-idle, best-effort activation
  cancellation, and fault-latch rejection under `Required`;
- rewind errors: not idle, unknown ref, staging/materialization failure, root-swap
  failure, and rollback failure;
- tool errors: `StaleFileError{Path}` for freshness/CAS violations.

## Concurrency and ordering

- `WithExclusiveWorkspace` root lease acquisition orders after session lease
  acquisition but before any durable append, materialization, checkpoint controller,
  or loop actor. Root lease loss is a session fault, not merely an observability
  warning.
- Teardown stops admission/work and checkpoint activity before releasing the root
  lease, then releases the session lease. Failure between acquisitions releases every
  acquired lease best-effort, using the rig doc's staged-construction discipline.
- `WithSessionWorkspaces` and `WithSharedWorkspace` never call a root leaser.
- The session workspace coordinator orders managed mutation, native boundary event
  persistence/emission, snapshotting, and checkpoint event persistence/emission.
  Workspace persistence backends are outside this gate because they are outside the
  workspace tree.
- Under `Required`, idle/manual admission stays closed and step/turn actors withhold
  their boundary acknowledgement. Required automatic boundaries serialize FIFO and do
  not coalesce; a loop cannot enqueue a second boundary while its first is pending.
- Under `BestEffort`, walk cancellation is prompt, automatic retry arms on the next
  eligible edge, and one latest-wins pending trigger bounds pressure. A cancelled or
  skipped walk journals no `WorkspaceCheckpointed` event.
- A persisted trigger event is emitted before its checkpoint walk; a successful
  snapshot blob is durable before `WorkspaceCheckpointed` is persisted and emitted.
  Snapshot failure never rolls back or hides the trigger event.
- The interrupt admission barrier orders all targeted loops idle, `SessionIdle`, and
  checkpoint-trigger acceptance before preserved user input can be dispatched.
- `RestoreWorkspace` serializes with the checkpoint controller: never concurrent with a
  walk, never while non-idle.
- Per-session root replacement stages and verifies outside the live root, then swaps
  only the proven `baseDir/<sessionID>` path; generic materialization never gains a
  recursive-wipe mode.
- Everything else (per-session appenders, ceiling, actors, checkpoint serialization per
  session) is unchanged from the rig doc.

## Testing

All table-driven, all under `-race`, extending the rig doc's matrix:

### `pkg/rig`

- placement option matrix: zero is valid when neither snapshot policy nor seed is
  configured; one selects that mode; many fail; snapshot policy or seed without
  placement fails typed;
  no-workspace sessions construct without root resolution, root lease acquisition, or
  checkpoint controller, and their workspace methods return
  `WorkspaceUnavailableError`;
- discoverable filesystem journal/catalog/blob directories equal to or below the
  workspace root fail typed; ancestor and disjoint directories are accepted when their
  actual persistence files remain outside the workspace tree;
- `WithExclusiveWorkspace`: missing/nil leaser fails at `Define`; a second session —
  including one created by another rig instance using the same backend — fails typed
  while the first is live; it succeeds after clean shutdown; lexical and symlink
  aliases contend on the same hashed lease name; root lease loss faults the session,
  rejects new work, interrupts live loops, and cancels checkpoint activity;
  restore-into-non-empty attaches without materializing; restore-into-empty
  materializes;
- `WithSessionWorkspaces`: concurrent sessions get disjoint roots; restore stages and
  swaps the effective `CurrentWorkspace` ref over post-checkpoint residue; empty-root
  restore rehydrates; failed second rename rolls the backup back; abandoned
  staging/backup recovery is deterministic; arbitrary and symlinked destinations are
  never removed; no root lease acquired;
- `WithSharedWorkspace`: no root lease acquired; refs stamped `fuzzy`; `Required`
  rejected at `Define`; restore never materializes;
- seeding: seed materializes before `SessionStarted`, its checkpoint follows
  `SessionStarted` and precedes every `LoopStarted`; restore of a
  seeded-but-never-worked session rehydrates the seed; seed rejected for non-empty
  fixed root and for shared mode; unresolvable seed ref fails before durable session
  state;
- trigger ladder: `Unset` resolves to `OnIdle`, explicit `Manual` remains distinct,
  each trigger schedules on its edge and no other; best-effort permits one in-flight
  plus one latest pending trigger and coalesced edges append nothing; required
  step/turn boundaries serialize FIFO without coalescing and block only their own
  session; manual calls serialize without coalescing;
- priority: `BestEffort` cancels an exclusive/per-session walk on activation and
  retries automatic work at the next eligible edge; manual receives a retryable error;
  shared mode finishes and stamps `fuzzy`; `Required` is valid for all four triggers in
  exclusive/per-session modes and rejected in shared mode; required step/turn blocks
  boundary acknowledgement, required manual/idle closes admission, timeout/error leaves
  the trigger event durable and latches the fault, and shutdown cancellation is
  classified as neither error nor fault.

### `pkg/event` / `pkg/sessionstore`

- `WorkspaceCheckpointed` codec round-trips `Consistency`, `Trigger`, and
  `Header.Cause`; producers never emit `SnapshotConsistencyUnknown`, while legacy
  records missing the field decode as unknown; validation requires the cause shape
  specified for each trigger while keeping `Header.Coordinates` session-scoped;
  malformed values fail replay per existing discipline;
- `WorkspaceRestored` codec and validation; replay of checkpoint A, checkpoint B,
  restore A leaves `LastCheckpoint=B` and `CurrentWorkspace=A`;
- catalog checkpoint events update both pointers, restore events update only
  `CurrentWorkspace`, and `RestoreSession` materializes `CurrentWorkspace`;
- correlation: turn→checkpoint resolution by seq and by
  `Header.Cause.Coordinates.TurnID` agree under the turn trigger.

### `pkg/tools`

- `NewFileTools` gives its three tools one observation set; separate bundles do not
  share observations; no file-tool JSON schema exposes hashes or versions;
- successful complete read records the raw-content hash; ranged complete reads remain
  usable; truncated, denied, escaping, symlink, and ambiguous failed reads do not
  authorize mutation; definitive not-found records absence;
- existing-file write without that loop's complete observation fails typed; creation
  without an observation succeeds only for a currently absent path via atomic
  no-replace publication, while an existing path or concurrent winner fails typed;
- write/edit after external modification fails typed and invalidates the observation;
  re-read records the new state and permits a new decision;
- successful write/edit updates the observation to the new hash, so a subsequent
  mutation by the same loop does not require a redundant read;
- two concurrent same-path calls are race-free under the observation critical section;
  canonical aliases resolve to the same key; tests run under `-race`;
- exact-match edit against changed content fails closed in addition to the whole-file
  freshness check;
- Subagent `timeout_seconds` accepts integers, rejects strings/negatives; absent
  `request_id` versus zero UUID.

### `pkg/session`

- a loop never has two active steps; parallel tool calls and multiple loops exercise
  one session workspace coordinator under `-race`;
- native step/turn/idle boundaries order durable trigger append, trigger emission,
  snapshot blob durability, durable `WorkspaceCheckpointed` append, checkpoint
  emission, then gate release;
- structured mutators hold shared workspace-mutation permits, Bash/unknown-path
  mutators hold the whole-workspace permit, and snapshot/restore excludes both; a
  waiting required checkpoint prevents new managed mutations;
- snapshot failure leaves the already-persisted and emitted trigger event intact;
  best-effort releases progress without a checkpoint event, while required faults
  before acknowledging the boundary;
- `RestoreWorkspace` requires idle, appends `WorkspaceRestored`, serializes against
  the checkpoint controller;
- `Session.Interrupt` reaches every live loop concurrently, including a delegate whose
  parent is blocked in a synchronous `wait`;
- `loop.Controller.Interrupt` covers exactly the target's delegate subtree — siblings
  and ancestors untouched;
- mark-then-deliver: a parent whose `wait` resolves interrupted cannot take a further
  step or `send` before its own interrupt lands;
- queued user submissions survive an interrupt; pending delegate requests resolve as
  typed interrupted results and their queued turns never start; preserved user input
  remains held until every target is idle and `SessionIdle` is appended;
- interrupt barrier release occurs after required checkpoint commit, after best-effort
  idle-trigger acceptance, after `SessionIdle` for best-effort turn/step triggers, after
  the idle event for manual policy, and immediately after the idle event when no
  workspace exists;
- Subagent `interrupt` remains turn-only and leaves the child's queue untouched;
- interrupt of a fully idle session returns false and journals nothing; and
- the post-interrupt idle edge produces a checkpoint stamped `Trigger: interrupt`
  under `SnapshotOnIdle`, gates admission until commit under `SnapshotRequired`, and
  fires nothing under `SnapshotManual`.

### Integration

Extend the rig doc's filesystem-backend test: seed two per-session roots from one ref,
run divergent work in both, checkpoint both, restore both on an empty base directory,
and verify disjoint trees each matching their own journal. A second scenario drives
`WithExclusiveWorkspace` exclusion across two rig instances, clean release, and lease
loss end to end. A third places filesystem session persistence and snapshot blobs in a
disjoint sibling directory, verifies they are absent from the workspace archive, and
proves overlapping persistence/workspace roots are rejected.

## Documentation consolidation

The accepted API, invariants, errors, ordering, and tests in this amendment are folded
into `2026-07-10-rig-lifecycle-workspace-snapshots-design.md`; that consolidated file is
the implementation authority. This file remains only as the decision trail that led to
the consolidated result.

The pasted review that prompted the final fold ended with the incomplete fragment
“The tes…”, and no continuation exists in the repository or conversation record
available to the spec. The fragment is therefore not treated as a hidden normative
requirement. Every complete review item was incorporated, and the consolidated testing
matrix was independently checked against the accepted behavior.

## Consumer documentation and migration order

The harness change includes `pkg/serve` but not CLI or SWE migration. After the
consolidated harness implementation passes, complete end-user guides and CI-compiled
examples for composing `rig`, `loop`, `session`, `storage`, `workspacestore`, and
`tools`, including session construction/restore and every workspace placement,
snapshot, rewind, and file-freshness policy in this design.

Only then write separate consumer migration specs, in order:

1. CLI migration spec and implementation plan.
2. SWE migration spec and implementation plan.

This ordering makes both migrations consumers of a documented public contract rather
than additional authors of the harness lifecycle. Migration implementation remains out
of scope for this design.

## Non-goals

- Migrating CLI or SWE; each receives the ordered follow-up spec above.
- Merge machinery for forked refs (three-way merge, conflict resolution) — the rig
  provides the fork; merging is a consumer or future-design concern.
- A rig-level default seed; the per-call option covers composition.
- Cross-store seed import.
- Distributed root leasing beyond the configured `storage.Leaser` backend's scope.
- Automatic workspace GC (unchanged from the rig doc: still manual, still requires a
  provably complete live set).
- Cross-file semantic isolation between concurrent writers on a shared root; the design
  explicitly routes that need to partitioning or fork/merge.
- Filesystem watchers as snapshot truth (unchanged).

## Result

Workspace support is optional; when enabled, placement is a typed, declared choice
instead of one ambiguous root parameter. The rig's concurrent-session promise is true
in every mode: fixed roots are exclusive by an explicit `storage.Leaser`, per-session
roots are exclusive by construction without a root lease, and shared roots are an
explicit opt-in with honestly labeled fuzzy snapshots. Per-session restore uses a
verified root swap, and the catalog distinguishes the newest backup from the effective
workspace ref so deliberate rewinds survive restart. Seeding is a journaled first
workspace checkpoint, which makes knowledgebase-seeded cloud sessions and fork/merge
concurrency fall out of existing restore and GC machinery. Snapshot scheduling is a
native ladder at actor boundaries with one priority knob deciding who yields. A
session-scoped workspace gate surrounds trigger persistence/emission, blob durability,
and checkpoint persistence/emission; required step/turn boundaries serialize without
coalescing, while best-effort pressure stays latest-wins and bounded. Harness-managed
torn snapshots disappear, persistence remains outside the captured tree, and refs are
honestly labeled `quiescent` or `fuzzy`. Interrupt barriers preserve user intent without
skipping the idle boundary. The journal now answers why a checkpoint fired, what it
covers, which ref is effective, and whether harness-managed mutation overlapped it —
and the patterns Claude Code, Codex, and the cloud agents ship implicitly become
declared, enforceable configuration.
