# In-Session Subagents + Unified Submit Surface

**Date:** 2026-06-19 (revised 2026-06-20 for id-normalization + `Agency`)
**Status:** Draft — plan-ready, no blocking open questions. Revised after id-normalization
landed: provenance now rides `identity.Agency` (no new `AgentInput` type), loop addressing
uses `identity.Coordinates`, correlation is `Cause.CommandID`.
**Depends on (all landed in `main`):** `docs/plans/loop-machine-design.md` (multi-loop
sessions, federated quiescence, the publish/subscribe hub, `NewLoop`),
`docs/plans/2026-06-18-tui-event-adoption-design.md` (the TUI is already a projection of
the session event stream: one lifetime `SubscribeEvents` + fire-and-forget `Submit`), and
`docs/plans/2026-06-18-id-normalization-design.md` (`CommandID`/`EventID`, nested `Cause`,
`identity.Coordinates`, `identity.Agency`).

## Motivation

Two redundancies remain after the TUI event-adoption work landed:

1. **Two ways to drive a turn.** `session.Submit` (fire-and-forget, AllowFold, outcome on
   the hub) is what the TUI uses. But `session.Stream` and `session.Invoke` still exist —
   `StartOnly`, blocking, each owning a **per-turn `events` channel** — a second delivery
   path that duplicates the hub. `emit` double-delivers (hub **and** the per-turn channel).
2. **Subagents are child *sessions*.** `tools/subagent.go` builds a separate child session
   (`factory.New`) and runs it via `Invoke`. That session has its own hub, so its events
   are **invisible** to the parent session's subscribers — the TUI never sees a subagent's
   progress, and the multi-loop machinery built for exactly this (LoopID attribution, the
   `{Ephemeral: primary-only, Enduring: all}` filter, federated quiescence, `NewLoop`) goes
   unused.

This design collapses both: **one external surface (`Submit` + `SubscribeEvents`)**, and
**subagents as in-session loops** whose events flow to the shared hub, attributed by
`LoopID`. It is the architectural endpoint the loop-machine + event-adoption work was built
toward, not new machinery.

## Design

### 1. The external surface is `SubscribeEvents` + `Submit` (+ `WaitIdle`)

Every consumer is uniform: subscribe (own your subscription, with your filter and lifetime)
+ submit fire-and-forget + read your own stream, correlating by the submit's **`CommandID`**
(returned by `Submit`; resolution events carry it as `Cause.CommandID`). This is already how
the TUI works. There is **no** bundled `Stream()`/`Invoke()` method that subscribes on the
caller's behalf — subscribing is the client's job.

**Deleted / migrated:**
- `session.Stream` and `session.Invoke` (the `StartOnly`, per-turn-channel programmatic
  path). Their per-turn machinery goes with them: `command.UserInput.Events` + `.Abandoned`
  and the `StartOnly` `InputMode` (so `InputMode` collapses — `Submit` is always
  `AllowFold`/queueable). This simplifies `decideSubmit` (no busy-reject branch, no per-turn
  channel to feed/close) and the loop's `emit` (just `publish` to the hub — no dual delivery).
- **The `personal-assistant` agent is deleted entirely** — its `Send`/`Stream`/`StreamBlocks`
  were the only non-coding consumers of the deleted session methods. The **coding agent
  migrates** to `Submit` + `SubscribeEvents` (drop its `Stream`/`StreamBlocks`).
- `StreamBlocks` from the `tui.Agent` interface and the agent wrapper (the TUI `Screen`
  already drives `Submit` + subscription; `StreamBlocks` survives only in the wrapper + the
  coding eval harness, which migrate or drop with the coding agent).

### 2. Subagents are in-session loops, not child sessions

The Subagent tool spawns a **loop in the current session** and runs it. The loop publishes
into the **same hub**, so its events reach every session subscriber (the TUI) attributed by
`Header.LoopID`, with its token/tool firehose muted under the default filter
(`tui.DefaultEventFilter` = `{Ephemeral: primary-only, Enduring: all}`) and its `StepDone`/gate
events delivered (Enduring, all loops). Federated quiescence already counts
`{kindLoop, subLoopID}`, so the
session is not idle while a subagent runs.

The tool **composes existing session APIs** (loop creation stays in the session's purview —
`NewLoop` is already a session method, purpose-built with `parent loop.Provenance` for the
parent linkage and `cfg loop.Config` for the sub-loop's model/tools):

1. `subLoopID := NewLoop(parentProvenance, cfg)` — register the sub-loop, wired to the hub +
   quiescence, and publish an Enduring `LoopStarted` carrying the parent linkage (§3). **No
   skill resolution** (decision: the coding agent does not support skills in this cut): `cfg`
   is the coding agent's own `loop.Config` (model/tools inherited from the parent), supplied by
   the injected capability — there is no skill→Config registry lookup.
2. `SubscribeEvents({Enduring: {subLoopID}})` — subscribe **before** submitting.
3. submit the task to `subLoopID` as a `command.UserInput{Header.Agency: AgencyMachine}` via
   the loop-targeted submit (see §3 for why no new command type, §6 for addressing).
4. `drainToFinalText(ctx, sub, commandID, interrupt)` — drain that loop's events to its
   terminal → final text (`commandID` is the submit's `CommandID`).

The tool depends on a **narrow injected interface** (`NewLoop` + loop-targeted submit +
`SubscribeEvents` + loop-targeted `Interrupt`, see §6), not the whole `Session` — mirroring
today's injected `factory` (Dependency Inversion). `factory.New` (child session) → that
capability (in-session loop). Because skills are out, the capability hands the tool a fixed
coding `loop.Config`; it does **not** take a skill name.

### 3. Audit-honest provenance via `Agency` (no new command type)

**Already landed in id-normalization.** Every `command.Header` carries `identity.Agency`
(`AgencyMachine` = 0, the fail-secure default — "our code did it"; `AgencyUser` — "a human
did it"), and the loop copies the originating command's agency onto the events it causes as
`Cause.Agency`. `Session.Submit` stamps `AgencyUser` (human-typed); `Stream`/`Invoke` and all
machine-issued commands stay `AgencyMachine`. This is tested end-to-end
(`internal/agent/session/agency_test.go`): a human `Submit` yields
`TurnStarted.Cause.Agency == AgencyUser`; a machine submit yields `AgencyMachine`.

So the audit-honest "human intent vs machine intent" distinction the earlier draft proposed a
new `command.AgentInput` type for **already exists at the field level** — and it is strictly
better (on *every* command, not just submits; propagated to events via `Cause.Agency`).
Therefore:

- **No new command type, no `queuedInput` origin field.** A subagent's task is a plain
  `command.UserInput` with `Header.Agency = AgencyMachine` (the zero default), submitted to
  the sub-loop via the loop-targeted submit (§6). `Agency` carries the origin; there is no
  `AgentInput` type and nothing to thread through `queuedInput`.
- **Same Reply-event family — no new turn-event types.** The submit funnels through the
  existing `decideSubmit`, producing the exact same Reply events as a human submit
  (`InputQueued`/`TurnStarted`/`TurnFoldedInto`/`InputCancelled`/`TurnRejected`), correlated
  by `Cause.CommandID`, differing only in `Cause.Agency` (machine vs user). An audit subscriber
  reads machine-vs-human directly from `Cause.Agency` — never a fake "user said…".
- **`LoopStarted` event (new in this cut) — the loop tree, via `Cause`.** `loop.Provenance`
  (the parent linkage) lives in the session registry (`loopHandle.parent`), **not** on the
  event stream — so subscribers cannot today reconstruct "loop X was spawned by loop Y / turn /
  step" (the earlier "recoverable from `Provenance`" audit claim overstated this). Fix: on
  success `NewLoop` publishes a small **Enduring** `LoopStarted` that reuses the existing
  `Header.Cause` model (no bespoke parent-id fields):
  `LoopStarted{Header: {Coordinates: newLoopCoords, Cause: {Coordinates: parentCoords, Agency:
  AgencyMachine}}}` — the new loop in `Header.Coordinates`, the spawning loop/turn/step in
  `Header.Cause.Coordinates`. It fires for every loop (the primary's `Cause` is zero = root).
  **Delivery caveat:** the hub has no replay, so `LoopStarted` reaches only subscribers
  **already active at loop-creation time** — the TUI's lifetime subscription and any journal
  consumer get the full tree; the per-subagent drain subscription (created *after* `NewLoop`,
  §2 step 2) does not see its own `LoopStarted` and does not need to (it correlates by
  `Cause.CommandID`).
- Entry points: human → `Session.Submit(blocks)` → `UserInput{Agency: AgencyUser}` → primary
  loop. Subagent → `NewLoop` + loop-targeted submit `UserInput{Agency: AgencyMachine}` →
  sub-loop.
- **TUI committed-user-row rule — already correct, no change required.** The current
  `loopID == primary && Cause.LoopID.IsZero()` rule already excludes a sub-loop's first turn
  (its `loopID != primary`), so no subagent turn commits a human row. *Optionally* it could be
  re-keyed on `Cause.Agency == AgencyUser` for explicitness (needs the TUI handler to read
  `TurnStarted.Cause.Agency`) — a nicety, not in-scope for this cut.

`command.SubagentResult` is unrelated and unchanged: it is the subagent's result handed
*back* to a parent loop (the async fold pattern — already `AgencyMachine`, already embedding
`identity.Coordinates` to address the parent), the opposite direction from a forward task
submit. There is no forward "task-to-subagent" command, and none is needed.

### 4. `drainToFinalText` — the shared collect helper

A free function over a caller-provided subscription (the client owns the sub; the helper
does the correlation/drain), reused by the Subagent tool and any future whole-session
caller:

```
drainToFinalText(ctx context.Context, sub Subscription, commandID uuid.UUID, interrupt func()) (string, error)
```
`commandID` is what `Submit`/the loop-targeted submit returns. The helper reads
`sub.Events()` and correlates in **two phases**: the opening resolution event (`TurnStarted`)
carries `Cause.CommandID == commandID` → capture its `TurnID`; thereafter `StepDone`/terminal
events are matched by that `TurnID` (they do not carry `Cause.CommandID`). The final assistant
text is taken from the `TurnDone.Message` terminal (falling back to the last `StepDone` if
`Message` is nil); the helper stops at that terminal (§5). The subscribe-**before**-submit
ordering (so the opening `TurnStarted` cannot be missed) is the caller's responsibility and
the one subtlety the helper documents.

`ctx` is the calling turn's context and `interrupt` is the loop-targeted `Interrupt` bound
to the sub-loop (§6). On `ctx.Done()` (the caller went away — HTTP close / CLI Esc) the
helper calls `interrupt()` **once** and keeps draining to the sub-loop's `TurnInterrupted`
terminal, then returns the §5 interrupted error. This is exactly `session.Invoke`'s existing
boundary-cancel → `interruptLoop` translation: submits carry no ctx, so cancelling `ctx`
cannot reach the sub-loop's turn — only an explicit `Interrupt` can. This is a **fail-safe**: a
distributed `Session.Interrupt`/`Shutdown` (§6, §9) already reaches the sub-loop directly, but
firing `interrupt()` here keeps the drain correct for *any* ctx-cancel source, so the sub-loop
can never orphan (§8 never reaps it).

### 5. Failure contract

Mapped in `drainToFinalText` and surfaced by the Subagent tool (every exit path returns a
typed error, never a bare string):
- `TurnDone` → final assistant text (its `Message`, falling back to the last `StepDone`), nil
  error.
- `TurnFailed` → typed error wrapping `TurnFailed.Err`.
- `TurnInterrupted` (the caller went away: HTTP request closed / CLI Esc) → typed
  "interrupted" error, no partial. The sub-loop is stopped by the loop-targeted `Interrupt`
  the helper drives on `ctx.Done()` (§4, §6), not by ctx propagation.
- `TurnRejected` (should not occur for a fresh sub-loop with a single submitter, but
  fail-secure) → typed error.
- Subscription loss (`EventSubscription.Err()` set when the hub force-closes) or sub-loop exit
  before any terminal → typed error.

### 6. Loop-targeted submit and interrupt

Both halves of driving a sub-loop must address a specific loop, because the sub-loop's turn
ctx derives from `sessionCtx`, not from the parent tool call — so the parent's ctx can
neither submit to nor cancel the sub-loop implicitly. Addressing follows the landed pattern:
the session routes a command to `loops[loopID]` (today `Submit`/`Interrupt` route to
`primaryLoopID`; generalize to a `loopID`), and where a command must carry its own target it
embeds `identity.Coordinates` — exactly as `SubagentResult` already addresses the parent loop.

**Submit.** Public `Submit` stays primary-only (human path, `AgencyUser`). The subagent's
forward submit is an **internal** loop-targeted method on the injected capability that writes
`command.UserInput{Header.Agency: AgencyMachine}` (Mode `AllowFold`) to `loops[subLoopID]` —
no new command type (§3). (Embed `Coordinates` on the command vs. session-picks-channel is a
plan-time detail, like `Submit`'s existing primary routing — not a design fork.)

**Interrupt — two complementary forms, both `command.Interrupt{Header, Ack}` (no new
command).** The command already cancels whatever loop it is routed to (the actor's dispatch
fires `state.cancelTurn()`); an idle loop no-ops (`cancelTurn == nil` → Ack `false`).

1. **`Session.Interrupt` (human "stop") is distributed**, like §9 shutdown: snapshot loops
   under `loopsMu`, send `Interrupt` (`Agency: AgencyUser`) to **every** loop, return true if
   any turn was cancelled. Simpler and more robust than relying on a transitive ctx cascade,
   and it matches "stop everything." (Idle retained loops no-op; in this cut the only active
   loops are the primary + its synchronous subtree, so distributed == the set that should
   stop.) No `closing` flag is needed — interrupt does not tear loops down, so a loop created
   during it is simply not interrupted, which is fine.
2. **A loop-targeted form** (route `Interrupt` to `loops[subLoopID]`, `Agency: AgencyMachine`)
   on the injected capability, used by `drainToFinalText` as a **fail-safe** on `ctx.Done()`
   (§4): it guarantees the sub-loop can't orphan even if the parent's ctx was cancelled by a
   path that did not distribute — so the drain is correct on its own. Idempotent with form 1.

Either way it is `Interrupt`, **not** shutdown: the turn cancels, the loop goes idle, and it
stays retained (§8).

**Granularity.** A loop has exactly one active turn (the actor serializes turns), so
"interrupt loop X" already means "interrupt its active turn" — there is no separate
turn-addressed path. A step has no independent cancel (it lives and dies with the turn ctx),
so step-level interrupt has no coherent target. An optional turn guard on `Interrupt` (no-op
if the active turn ≠ target, defending against a stale interrupt landing on a *successor*
turn) is unnecessary for this synchronous cut — the sub-loop runs exactly one turn per tool
call — and is a follow-on for the agent-teams reuse case (§8).

### 7. Quiescence and async-cancel ownership

Unchanged. A synchronous Subagent tool keeps the **parent** loop active (its turn is blocked
on the tool) for the whole subagent run, so the session cannot go idle prematurely without
any wake token. The sub-loop's own `{kindLoop, subLoopID}` activity is also in the active
set. (The `expectTurn`/`SubagentResult` wake-token path remains for the *async* hand-back
pattern, which this synchronous cut does not use.)

**Who cancels an async subagent (deferred).** The `expectTurn`/`{wake, subLoopID}` token and
the `SubagentResult` hand-back own *liveness accounting* and *result delivery* for an async
subagent (loop-machine §"Federated quiescence", §"Subagent hand-back") — but **not**
cancellation. Cancel/interrupt of an in-flight async child is the deferred **cross-turn
handle model** (`wait`/`send`/`interrupt`), explicitly out of scope in loop-machine
(`loop-machine-design.md` lines 1757-1759; Open Items B). The **global** stops already reach
every loop — `Session.Interrupt` (§6) and `Shutdown` (§9) are both distributed — so what is
deferred is a **scoped** cancel of *one* in-flight async child (stop just it, not everything),
plus its liveness accounting. For *this* synchronous cut the owner is unambiguous: the blocked
tool call
(§4). The loop-targeted `Interrupt` primitive (§6) is **shared** — the sync tool calls it
now; the future async handle/supervisor will call the same lever — so building it now is
forward-compatible, not throwaway.

## 8. Loop lifecycle — loops are never deleted (intentional)

There is **no loop teardown** in the session, by design. A subagent call therefore:
spawn the loop (`NewLoop`) → run the task (a `UserInput{Agency: AgencyMachine}` turn) → the
result drains back to **whoever called the tool** (the parent turn's tool execution receives
the final text and
continues) → the loop then **sits idle** in the session's loop registry, retained with its
history. Nothing deletes it. So there is no "one-shot vs persistent (teardown)" fork — the
loop always persists idle.

This cut **spawns a fresh loop per subagent invocation** (each call gets a new idle loop).
Because idle loops are retained, **routing a follow-up subagent call back to the same loop
("agent teams" — reuse an existing idle loop by identity, replaying its history)** is a
purely **additive follow-on**: it needs loop addressing/identity at the tool boundary, but
no teardown and no change to §1–§7. Not required for this cut, not a blocker.

**Resource bound — KNOWN REGRESSION, deferred (TODO).** This cut **drops the recursion-depth
cap** today's `tools/subagent.go` enforces (`maxSubagentDepth`, `tools/subagent.go:60`,
enforced at `:184`). That cap worked only because the old child ran under the parent tool
call's ctx; in the in-session model the sub-loop's turn ctx derives from `sessionCtx`, not the
parent tool call, so the ctx-key no longer reaches the child's tool calls. Combined with
never-delete retention **and the fact that Subagent is auto-approved**
(`agents/coding/agent.go:125`), this cut bounds **neither depth nor breadth** — a runaway agent
could spawn unbounded idle-retained loops. **This is a deliberate, acknowledged regression to
be addressed in a follow-up** (not a permanent state): re-add a **per-session loop cap**
(fail-secure: reject `NewLoop` past N) and **derive depth from `loop.Provenance` at `NewLoop`**
(walk the parent chain) rather than via ctx. Tracked as a TODO; deferred here only because the
resource model is being reworked, not because it is safe.

## 9. Multi-loop shutdown (in this cut)

Because sub-loops are retained (§8) and each runs its own goroutine/`loopCtx`, session teardown
must reach **every** loop, not just the primary. Today `Session.Shutdown`
(`internal/agent/session/session.go`) routes `command.Shutdown` to `primaryLoopID` only and
leans on `sessionCancel` to hard-cancel the rest via `sessionCtx` — leaving idle sub-loops
stopped by ctx cancellation alone, never a graceful `Shutdown`. This cut makes `Shutdown`
multi-loop:

1. under `loopsMu`, set a `closing` flag **and** snapshot all loops in one critical section (so
   no loop can be inserted between the flag and the snapshot);
2. `hub.StopSession` (flip to `SessionStopped`, wake `WaitIdle`);
3. send `command.Shutdown` to **every** loop in the snapshot (keep the `Done`/`ctx` send
   escapes so an unbuffered send never wedges);
4. wait for all acks or `ctx.Done()`;
5. `sessionCancel()` as the final backstop (releases every `loopCtx` derived from `sessionCtx`).

`NewLoop` must also take `loopsMu` and **fail secure** when `closing` is set — return a typed
"session closing" error and create nothing — otherwise a `NewLoop` racing the snapshot would
register a loop shutdown never reaps. This makes retained idle sub-loops safe to leave in the
registry: shutdown stops them gracefully, the close-flag blocks late spawns, and the backstop
guarantees release even if a loop never acks.

## Testing

- `drainToFinalText` unit tests: clean / failed / interrupted / rejected / subscription-loss,
  and the subscribe-before-submit ordering (no missed opening event). The interrupted case
  asserts the helper calls the loop-targeted `interrupt` **once** on `ctx.Done()` and still
  drains to the `TurnInterrupted` terminal (no orphaned sub-loop). Correlation is by
  `Cause.CommandID` → `TurnID`.
- `decideSubmit`: a human submit (`Agency: AgencyUser`) and a machine submit
  (`Agency: AgencyMachine`) both start/queue a turn via the shared path and emit the **same**
  Reply events (`InputQueued`/`TurnStarted`/`TurnFoldedInto`/`InputCancelled`/`TurnRejected`,
  correlated by `Cause.CommandID`), differing only in `Cause.Agency`; quiescence `-race` stays
  green after dropping `StartOnly`/`Events`/`Abandoned` and the `Stream`/`Invoke` deletion.
- Subagent integration: a subagent runs as an in-session loop; its `StepDone`/gate events
  appear on the parent session's subscription attributed by `LoopID`; its token/tool events
  are muted under the default filter; it returns the final text (or the typed error).
- Audit: a machine-originated turn (`Cause.Agency == AgencyMachine`) is distinguishable from a
  human turn (`AgencyUser`) at the command and event layers; no subagent turn commits a human
  user row in the TUI.
- `LoopStarted`: `NewLoop` publishes exactly one Enduring `LoopStarted` per loop with
  `Header.Coordinates` = the new loop and `Header.Cause.Coordinates` = the parent (zero for the
  primary = root); a subscriber **already active before** `NewLoop` receives it under
  `{Enduring: all}` (a later subscriber does not — no replay).
- Multi-loop shutdown (§9): a session with an idle sub-loop, on `Shutdown`, sends
  `command.Shutdown` to **every** loop (each `Done` closes) and `WaitIdle` returns
  `ErrSessionStopped`; a `NewLoop` after shutdown starts fails secure (typed "closing" error,
  no loop created); `-race` clean.
- Distributed interrupt (§6): `Session.Interrupt` with an active sub-loop cancels **every**
  active turn (primary + sub-loop); idle loops no-op (Ack `false`); the drain's loop-targeted
  fail-safe is idempotent with it.
- Whole-tree `go test -race ./...`, plus `make secure` (incl. the new `gofmt` check).

## Out of scope / sequencing

- **Skills / persona selection** — the coding agent does not support skills in this cut; the
  Subagent tool spawns a sub-loop with the coding agent's own `loop.Config` (no skill→Config
  registry, no `skill` arg). A skill catalog is a follow-on.
- **Persistent agent teams** (§8) — follow-on layer, incl. the optional turn guard for
  reusing an idle loop across turns (§6).
- **Async `SubagentResult` fold-back orchestration** — the wake-token machinery stays for it,
  but this cut's Subagent tool is synchronous (block + drain), not fold-back.
- **Async subagent cancel/interrupt ownership** — the cross-turn handle model
  (`wait`/`send`/`interrupt`), deferred by loop-machine (§7); this cut's loop-targeted
  `Interrupt` is the shared primitive it will reuse.
- **Resource bounds** — depth and breadth caps for subagent spawning (§8); this cut enforces
  neither (depth dropped intentionally; to be added back later).

### Phasing (one branch, in phases — no separate PRs)

The `Invoke` ↔ subagent coupling sets the order: the current subagent still calls
`session.Invoke` (`agents/coding/subagent_factory.go`), and both `Stream` and `Invoke` share
the `StartOnly`/per-turn-channel machinery, so that machinery cannot be removed until the
subagent stops using `Invoke`. Each phase keeps the tree building and green.

1. **Surface trim** — delete the `personal-assistant` agent, migrate the coding agent's main
   path to `Submit` + `SubscribeEvents`, delete `session.Stream` + `StreamBlocks`. (`Invoke`
   and the per-turn machinery stay — the subagent still needs them.)
2. **Subagent rewrite** — add the loop-targeted submit/interrupt + `drainToFinalText` + parent
   `Provenance` plumbing; replace the child-session subagent (`subagent_factory.go`) with the
   in-session loop (§2–§6). This removes the last `Invoke` caller.
3. **Machinery removal** — now that nothing uses them, delete `session.Invoke`, the `StartOnly`
   `InputMode`, `UserInput.Events`/`.Abandoned`, the per-turn channel, the `emit` dual-delivery,
   and the `decideSubmit` busy-reject branch (§1's end state).
