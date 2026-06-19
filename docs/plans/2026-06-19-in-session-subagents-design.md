# In-Session Subagents + Unified Submit Surface

**Date:** 2026-06-19
**Status:** Draft — no blocking open questions (loop lifecycle resolved, §8)
**Depends on:** `docs/plans/loop-machine-design.md` (multi-loop sessions, federated
quiescence, the publish/subscribe hub, `NewLoop`, producer-identity `Header`s) and
`docs/plans/2026-06-18-tui-event-adoption-design.md` (the TUI is already a projection of
the session event stream: one lifetime `SubscribeEvents` + fire-and-forget `Submit`).

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
+ submit fire-and-forget + read your own stream, correlating by `Header.CausationID ==`
the submit's `InputID`. This is already how the TUI works. There is **no** bundled
`Stream()`/`Invoke()` method that subscribes on the caller's behalf — subscribing is the
client's job.

**Deleted:**
- `session.Stream`, `session.Invoke`, and the dead `Assistant.Invoke` wrapper (no caller).
- The per-turn machinery they needed: `command.UserInput.Events` + `.Abandoned`, and the
  `StartOnly` input mode. `Submit` is always the queueable mode. This simplifies
  `decideSubmit` (no busy-reject branch, no per-turn channel to feed/close) and the loop's
  `emit` (just `publish` to the hub — no dual delivery).
- `StreamBlocks` from the `tui.Agent` / agent-wrapper interfaces (already unused after the
  TUI switched to the subscription).

### 2. Subagents are in-session loops, not child sessions

The Subagent tool spawns a **loop in the current session** and runs it. The loop publishes
into the **same hub**, so its events reach every session subscriber (the TUI) attributed by
`Header.LoopID`, with its token/tool firehose muted under the default
`{Ephemeral: primary-only}` filter (amendment 1) and its `StepDone`/gate events delivered
(Enduring, all loops). Federated quiescence already counts `{kindLoop, subLoopID}`, so the
session is not idle while a subagent runs.

The tool **composes existing session APIs** (loop creation stays in the session's purview —
`NewLoop` is already a session method, purpose-built with `parent loop.Provenance` for the
parent linkage and `cfg loop.Config` for the skill's model/tools/prompt):

1. `subLoopID := NewLoop(parentProvenance, skillCfg)` — register the sub-loop, wired to the
   hub + quiescence.
2. `SubscribeEvents({Enduring: {subLoopID}})` — subscribe **before** submitting.
3. submit the task to `subLoopID` as a `command.AgentInput` (see §3).
4. `drainToFinalText(sub, inputID)` — drain that loop's events to its terminal → final text.

The tool depends on a **narrow injected interface** (`NewLoop` + loop-targeted submit +
`SubscribeEvents`), not the whole `AgentSession` — mirroring today's injected `factory`
(Dependency Inversion). `factory.New` (child session) → that capability (in-session loop).

### 3. `command.AgentInput` — audit-honest input provenance

Reusing `command.UserInput` for a subagent's task **conflates human intent with
machine-generated intent**, which is wrong for audits (a reader must distinguish "the human
asked X" from "the parent agent told a subagent X") and is the source of recurring
"is this really a user?" special-casing.

Add `command.AgentInput` — a structural sibling of `UserInput` (`{Header, Blocks}`,
queueable, starts/queues a turn) but a **distinct type**, so provenance is honest at the
command layer and propagates into the turn's events. No new turn-start logic:

- The loop's command dispatch maps **both** `UserInput` and `AgentInput` into the same
  `queuedInput` → `decideSubmit`. The only difference is an **origin marker** on
  `queuedInput` (`OriginHuman` / `OriginAgent`) set from which command arrived; all
  queue/start/fold logic stays single-sourced.
- **Same Reply-event family — no new event types.** Because both funnel into `decideSubmit`,
  an `AgentInput` produces the *exact* same `Reply` events as a `UserInput` — `InputQueued`,
  `TurnStarted`, `TurnFoldedInto`, `InputCancelled`, `TurnRejected` — correlated by
  `Header.CausationID`, uniform with the way `SubagentResult` already emits its lifecycle
  events. The subagent's input lifecycle is therefore fully observable on the one event
  stream exactly like user input; the only difference is the carried provenance.
- The reply/turn events carry that origin (or it is recoverable from the sub-loop's
  `Provenance`, which names the parent). An audit subscriber sees "agent-originated turn on
  loop X spawned by Y", never a fake "user said…".
- Entry points: human → `Submit(blocks)` → `UserInput` → primary loop. Subagent →
  `NewLoop` + submit `AgentInput{task}` → sub-loop.

This also tidies the TUI's committed-user-row rule: instead of inferring "not human" from
`LoopID == primary && TriggeredByLoopID == 0`, a committed user row keys on the turn being
**`UserInput`/`OriginHuman`-originated** — explicit, not inferred.

`command.SubagentResult` is unchanged and unrelated: it is the subagent's result handed
*back* to a parent loop (the async fold pattern), the opposite direction from `AgentInput`.

### 4. `drainToFinalText` — the shared collect helper

A free function over a caller-provided subscription (the client owns the sub; the helper
does the correlation/drain), reused by the Subagent tool and any future whole-session
caller:

```
drainToFinalText(sub Subscription, inputID uuid.UUID) (string, error)
```
It reads `sub.Events()`, keeping events correlated to `inputID`/its turn, captures the
loop's latest `StepDone` final assistant text, and stops at the loop's terminal. The
subscribe-**before**-submit ordering (so the opening `TurnStarted` cannot be missed) is the
caller's responsibility and the one subtlety the helper documents.

### 5. Failure contract

Mapped in `drainToFinalText` and surfaced by the Subagent tool:
- `TurnDone` → final assistant text, nil error.
- `TurnFailed` → typed error wrapping `TurnFailed.Err`.
- `TurnInterrupted` (ctx-cancel — the caller went away: HTTP request closed / CLI Esc) →
  typed "interrupted" error, no partial.

### 6. Loop-targeted submit

`Submit` is primary-only today. A subagent submits to its sub-loop, so submit must address
a specific loop: either `Submit(ctx, loopID, blocks)` (uniform — the TUI passes the primary
id) or a sibling `SubmitTo(loopID, …)`. The `AgentInput` submit is the loop-targeted form;
public `Submit` may stay primary-only with sub-loop submission internal to the injected
capability. (Decide at plan time — implementation detail, not a design fork.)

### 7. Quiescence

Unchanged. A synchronous Subagent tool keeps the **parent** loop active (its turn is blocked
on the tool) for the whole subagent run, so the session cannot go idle prematurely without
any wake token. The sub-loop's own `{kindLoop, subLoopID}` activity is also in the active
set. (The `expectTurn`/`SubagentResult` wake-token path remains for the *async* hand-back
pattern, which this synchronous cut does not use.)

## 8. Loop lifecycle — loops are never deleted (intentional)

There is **no loop teardown** in the session, by design. A subagent call therefore:
spawn the loop (`NewLoop`) → run the task (`AgentInput` turn) → the result drains back to
**whoever called the tool** (the parent turn's tool execution receives the final text and
continues) → the loop then **sits idle** in the session's loop registry, retained with its
history. Nothing deletes it. So there is no "one-shot vs persistent (teardown)" fork — the
loop always persists idle.

This cut **spawns a fresh loop per subagent invocation** (each call gets a new idle loop).
Because idle loops are retained, **routing a follow-up subagent call back to the same loop
("agent teams" — reuse an existing idle loop by identity, replaying its history)** is a
purely **additive follow-on**: it needs loop addressing/identity at the tool boundary, but
no teardown and no change to §1–§7. Not required for this cut, not a blocker.

## Testing

- `drainToFinalText` unit tests: clean / failed / interrupted, and the subscribe-before-
  submit ordering (no missed opening event).
- `decideSubmit`: `UserInput` and `AgentInput` both start/queue a turn via the shared path
  and emit the **same** `Reply` events (`InputQueued`/`TurnStarted`/`TurnFoldedInto`/
  `InputCancelled`/`TurnRejected`, correlated by `CausationID`), differing only in the
  carried origin; the `queuedInput` origin marker is set correctly; quiescence `-race`
  stays green after dropping `StartOnly`/`Events`/`Abandoned`.
- Subagent integration: a subagent runs as an in-session loop; its `StepDone`/gate events
  appear on the parent session's subscription attributed by `LoopID`; its token/tool events
  are muted under the default filter; it returns the final text (or the typed error).
- Audit: an `AgentInput`-originated turn is distinguishable from a `UserInput`-originated
  turn at the command and event layers; no subagent turn commits a human user row in the
  TUI.
- Whole-tree `go test -race ./...`.

## Out of scope / sequencing

- **Persistent agent teams** (§8) — follow-on layer.
- **Async `SubagentResult` fold-back orchestration** — the wake-token machinery stays for it,
  but this cut's Subagent tool is synchronous (block + drain), not fold-back.
- Removing the now-redundant per-turn channels is part of §1 (deleting `Stream`/`Invoke`).
