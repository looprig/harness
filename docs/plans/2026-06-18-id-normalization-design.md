# ID Normalization Design

**Date:** 2026-06-18 (revised 2026-06-20)
**Status:** Draft
**Depends on:** `docs/plans/loop-machine-design.md`
**Supersedes:** the naming/correlation parts of
`docs/plans/2026-06-14-identity-correlation-design.md`

## Amendments (2026-06-20)

This revision changes the original draft as follows (rationale inline below):

1. **Tool identity is two ids, not three.** Drop `GateID`. Keep one internal
   `ToolExecutionID` (`uuid.UUID`, ours) as the master key for gate routing *and*
   the tool lifecycle, and keep the provider `ToolUseID` (`string`) as a
   content-only foreign key bound 1:1 to it. See *Tool identity*.
2. **`Coordinates` quartet** (`SessionID/LoopID/TurnID/StepID`) is a shared struct,
   embedded by the event `Header`, `GateRoute`, `Cause`, and the internal state
   structs. (Named `Coordinates`, not `Scope` — `event.Scope` is already the
   delivery-scope enum `ScopeSession`/`ScopeLoop`.)
3. **No per-message id.** Messages are addressed through the events/commands that
   carry them — a user message by its `TurnID`/`Cause.CommandID`, the AI message by
   its `StepID`, each tool result by its `ToolExecutionID`. `content` is unchanged
   (no id, no envelope). See *Messages*.
4. **`Agency` (user vs machine)** on the command `Header` and `Cause`, with
   **`AgencyMachine` as the default** — only user-origination points stamp
   `AgencyUser`. A per-action audit record. See *Agency*.
5. **`ToolExecutionID` lives on the four tool/gate event bodies**, not the shared
   event `Header` (it is meaningful on only 4 of ~17 events). The header keeps only
   the universal fields: `Coordinates`, `EventID`, `Cause`.
6. **`Route` → `GateRoute`, and it becomes the routing key.** The session
   dispatches a gate reply to `loops[GateRoute.LoopID]`; that loop matches the gate
   by `GateRoute.ToolExecutionID`. This replaces today's route-to-primary-loop
   (a latent bug once subagents exist) and makes the field load-bearing now — the
   registry is already keyed by `LoopID` and the TUI already tracks the producing
   loop. The gate API takes a `GateRoute`, not a bare `callID`.
7. **`Origin`/provenance is deferred, not added.** Nothing reads provenance today.
   The dead `Parent*` fields are *removed* from the header; the live subagent edge
   is the causal one (`TriggeredByLoopID` → `Cause.LoopID`). See *Coordinates and
   Cause*.
8. **Session rename.** `Sesssion` (a typo) → `Session`; `NewAgent` → `New`
   (so `session.New(...)`). Mint ladder corrected: `StepID` is minted by
   `runTurn`, not `runStep`.

> **Header naming.** The header structs keep the name `Header` (qualified
> `command.Header` / `event.Header`). They are deliberately **not** named
> `CommandHeader`/`EventHeader`, because those are the existing promoted accessor
> methods (`CommandHeader() Header`, `EventHeader() Header`). A struct sharing a
> method's name shadows that method when embedded — which would drop
> `EventHeader()` from the method set and break the sealed `Event` interface. So:
> **struct = `Header`, accessor = `CommandHeader()` / `EventHeader()`.**

## Motivation

The loop-machine design adds session fan-in, multi-loop routing, per-turn and
per-step state, gate routing, queued input, and future journal/restore. Those
features need a shared ID vocabulary. The pre-normalization names overlap:

- command `Header.ID` means command identity;
- event `Header.ID` is intended to mean event identity;
- `InputID` is actually the submit command id;
- `CallID`, `ToolCallID`, `ToolUseID`, and gate routing all describe different
  pieces of the tool path;
- `CausationID` is a single flat field, but the thing that caused an action can
  be a command, event, turn, step, or tool execution.

This spec normalizes the vocabulary so every field name answers one question and
every event/command type has a clear fill rule.

## Naming Rules

- Use Go initialism style: `ID`, not `Id`.
- Do not use a generic field named `ID` in shared headers. Use the domain name:
  `CommandID`, `EventID`, `SessionID`, etc.
- Do not add flat fields like `CausationSessionID`, `CausationTurnID`, and so on.
  Causation uses the same ID vocabulary under a nested `Cause` struct.
- Zero value means absent/not applicable. Non-zero required fields are validated
  before publishing/sending.
- Provider/model IDs stay strings when the provider owns the format, and they
  live only in `content`. Internal runtime IDs are `uuid.UUID` and never enter
  model-visible content.
- **JSON (journal only):** tag `uuid.UUID` fields `json:",omitzero"`, *not*
  `omitempty`. `omitempty` is a no-op on `uuid.UUID` (it is `[16]byte`, an array;
  `encoding/json` never treats arrays as empty). `omitzero` (Go 1.24+) honors the
  existing `UUID.IsZero()`. `uuid.UUID` also needs `MarshalText`/`UnmarshalText`
  (canonical string) for a readable journal — today it has only `String()`.
- **Payload (non-id) fields** also carry snake_case `json` tags so journal output
  is stable: `omitzero` for scalars/structs, `omitempty` for slices/strings.
- All these tags are inert for the in-memory channel flow; they matter only at
  journal/restore.

## Packaging

The shared identifier types — `Coordinates`, `Cause`, `Agency` — live in a
dedicated low-level package `internal/agent/loop/identity` (file
`identifier_types.go`, importing only `uuid`). They **cannot** live in `loop`
itself: `loop` imports both `command` and `event`, so those packages importing
`loop` back for `Coordinates` would form an import cycle. `identity` sits below
`event`, so `event`, `command`, and `loop` all import it freely.

| Package | Owns |
|---|---|
| `identity` (new, low-level) | `Coordinates`, `Cause`, `Agency`, ID aliases |
| `event` | `Header` (struct; accessor `EventHeader()`; embeds `identity.Coordinates`) + the event types |
| `command` | `Header` (struct; accessor `CommandHeader()`), `GateRoute` (embed `identity` types) |
| `content` | unchanged — model-domain only, no ids |

## Identity Vocabulary

| ID | Type | Minted by | Meaning |
|---|---|---|---|
| `SessionID` | `uuid.UUID` | `Session` (`session.New`) | User-visible session boundary shared by all loops in the session. |
| `LoopID` | `uuid.UUID` | `Session.NewLoop` | One loop goroutine/agent instance inside a session. |
| `TurnID` | `uuid.UUID` | `runLoop` | One user/subagent initiated turn in one loop. |
| `StepID` | `uuid.UUID` | `runTurn` | One LLM response step inside a turn. |
| `CommandID` | `uuid.UUID` | Command issuer | One command sent to a loop/session. |
| `EventID` | `uuid.UUID` | Event producer | One event published to streams/fan-in/sinks. |
| `ToolExecutionID` | `uuid.UUID` | `runner.RunBatch` | **Our** handle on one tool call: gate routing, lifecycle events, UI cards, journal/tracing. |
| `ToolUseID` | `string` | Model/provider | The **model's** tool-use id. Lives only in `content`; bound 1:1 to a `ToolExecutionID`; used solely to pair the tool result back to the model. |

There is deliberately **no `MessageID`** — see *Messages*.

**The mint ladder — the parent's runner mints the child's id, just before it
spawns the child:**

```
session.New      → SessionID
Session.NewLoop  → LoopID
runLoop          → TurnID
runTurn          → StepID
runner.RunBatch  → ToolExecutionID   (binds it 1:1 to the model's ToolUseID)
```

`CommandID` and `EventID` are stamped at the moment of send/emit by the issuer/
producer.

## Tool identity: `ToolExecutionID` vs `ToolUseID`

There is exactly one internal tool id and one foreign one. They are bound once,
at mint, and translated only at the model boundary.

- **`ToolExecutionID`** (`uuid.UUID`, ours) is the master correlation key for the
  whole tool call: the gate (`pendingGates` keyed by it + `gateKind`), the
  lifecycle events (`PermissionRequested`, `UserInputRequested`, `ToolCallStarted`,
  `ToolCallCompleted`), the TUI card, the journal, and tracing.
- **`ToolUseID`** (`string`, the provider's `toolu_…`/`call_…`) is the model's
  handle. It lives only in `content.ToolUseBlock.ID` and
  `content.ToolResultMessage.ToolUseID`. We never route or correlate internally
  on it; we keep it so the tool result can be paired back to the model.

`GateID` is **removed.** Gate routing keys on `ToolExecutionID + gateKind`
(today's mechanism); "auto-approved" is simply "no entry in `pendingGates`," not a
missing id. Every gate is born from a tool call (`PermissionRequested`/
`UserInputRequested` both come from tools), so a separate gate id buys nothing. If
a gate ever exists that is *not* tied to a tool call, revisit this.

Lifecycle (the only two boundary crossings of `ToolUseID` are in and out):

```
 MODEL                                   OUR MACHINE
 ToolUseID (string)                      ToolExecutionID (uuid)
 ─────────────────────────────────────────────────────────────────
 model emits tool_use ───────────────►  ToolUseBlock.ID = "toolu_7f3a"
                                         mint ToolExecutionID, BIND 1:1
                                         PermissionRequested{ToolExecutionID}  (gate)
                                         ToolCallStarted{ToolExecutionID}      (card)
                                         …run…
                                         ToolCallCompleted{ToolExecutionID}    (card)
 tool_result ◄───────────────────────  ToolResultMessage{ToolUseID:"toolu_7f3a"}
                                         ↑ looked up from ToolExecutionID
```

**Why ours-as-master:** it keeps the whole correlation/tracing graph homogeneous
(every id is a `uuid.UUID` we mint and control), restores type-safety, decouples
journal/restore keys from provider id formats, and is where the runner already is
(`result{CallID, ToolUseID: block.ID}`). The only cost: to correlate against
provider logs you join on the provider `ToolUseID`, so keep it queryable (store it
on the card/result and as a span attribute when tracing lands).

## Coordinates and Cause

`Coordinates` is the four nesting ids, declared once and reused. (Not `Scope` —
`event.Scope` is already the delivery-scope enum `ScopeSession`/`ScopeLoop`.)

```go
type Coordinates struct {
    SessionID uuid.UUID `json:"session_id,omitzero"`
    LoopID    uuid.UUID `json:"loop_id,omitzero"`
    TurnID    uuid.UUID `json:"turn_id,omitzero"`
    StepID    uuid.UUID `json:"step_id,omitzero"`
}
```

`Cause` is "the direct thing that caused this" — the same ID vocabulary, nested.
It changes per event/command:

```go
type Cause struct {
    Coordinates                       // the cause's coordinates (full quartet, mostly omitted)
    CommandID       uuid.UUID `json:"command_id,omitzero"`
    EventID         uuid.UUID `json:"event_id,omitzero"`
    ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
    Agency          Agency    `json:"agency,omitzero"` // the causing command's agency (machine by default)
}
```

### Agency (user vs machine)

`Agency` answers *"who performed this action?"* — **per action**, not per turn. It
is an audit/observability record (good for CLAUDE.md "log security events: who
approved"), *not* the gate decision (autonomous mode decides whether a human is
prompted; `Agency` records which actually happened).

```go
type Agency uint8

const (
    AgencyMachine Agency = iota // 0 — the DEFAULT: our code did it
    AgencyUser                  // a human did it
)
```

**`AgencyMachine` is the default** because almost everything is machine-driven:
the loop running, each step, every tool call, subagent hand-backs, the scheduler.
Only the discrete **user-origination points** stamp `AgencyUser`:

- the TUI stamping a typed `UserInput`;
- a human `ApproveToolCall` / `DenyToolCall` / `ProvideUserInput`;
- a manual `Interrupt`.

So a `StepDone` or `ToolCallStarted` reads `AgencyMachine` even inside a
user-started turn — correctly, because *the agent* chose to do it; the user only
authored the input. Per-action agency with a machine default gives this for free,
with no per-turn propagation needed.

**The two `Agency` fields answer different questions:**

- **command `Header.Agency`** — *"who performed **this** command?"* Stamped by the
  command's issuer. It is the authoritative source.
- **`Cause.Agency`** — *"who performed the thing that **caused** this?"* A copy of
  the causing command's `Header.Agency`, carried so an **event** can surface agency
  without chasing the command (e.g. `TurnStarted.Cause.Agency == AgencyUser` means
  "this turn was started by a human"). An event has no agency of its own; it reads
  agency through `Cause.Agency`, and most events (no cause) read machine by default.

(Not named `Actor` — "the actor" already means the `runLoop` goroutine.)
Default-machine is the **fail-secure** attribution: a missing/zero value reads as
machine, so we never falsely claim a human acted. With the machine default at zero,
`omitzero` drops it from the journal and only the rarer `AgencyUser` is recorded.

### Deferred: `Origin` / lineage

An earlier draft put a nested `Origin` (the parent loop/turn/step that spawned a
subagent loop) in the event `Header`. **It is deferred** — nothing reads
provenance yet:

- `loopState.parent` (`loop.Provenance`) is set at spawn but **never read**;
- the event `Header`'s `ParentLoopID/TurnID/StepID` are **declared but never set or
  read**, on both main and `feature/in-session-subagents`;
- the only subagent edge actually wired is the **causal** one,
  `TriggeredByLoopID` → `Cause.LoopID` (used for quiescence/wake on hand-back).

So the dead `Parent*` fields are **removed** from the header (not renamed), and
`loop.Provenance` stays on `loopState.parent` as the spawn-time record. Add a
nested `Origin Coordinates` to the event `Header` only when a real consumer exists
(TUI subagent lineage display, or journal-restore). Until then the header carries
only what is read: `Coordinates`, `EventID`, and `Cause`.

### Setting `Cause`

Most events have a zero `Cause`; their coordinate fields already say where they
happened. Set `Cause` only when a consumer needs the direct edge:

- submit-resolution events (`TurnStarted`, `TurnFoldedInto`, `InputCancelled`)
  set `Cause.CommandID` to the `UserInput`/`SubagentResult` command id, and
  `Cause.Agency` to that command's agency (so a user-started turn is visible);
- a gate reply command may set `Cause.EventID` to the `PermissionRequested` or
  `UserInputRequested` event it answers, if the UI has that event id;
- a `ToolCallStarted` after a human approval may set `Cause.CommandID` to the
  approve command id;
- a subagent hand-back command may set `Cause.EventID` to the child loop terminal
  event or `Cause.LoopID` to the child loop when there is no terminal event id.

Do not stamp the original user command onto every event in a long turn. A turn can
fold more user messages later, so "the command that caused this step" can become
ambiguous. The durable transcript is carried by `TurnStarted.Message`,
`TurnFoldedInto.Message`, and `StepDone.Messages`; causation only records direct
edges.

## Headers

Commands and events use different headers because they have different identities.
Gate addressing stays separate from both. Each struct is named `Header` in its own
package (`command.Header`, `event.Header`) so it does not shadow the promoted
`CommandHeader()` / `EventHeader()` accessor methods.

```go
// package command — accessor: func (h Header) CommandHeader() Header
type Header struct {
    CommandID uuid.UUID `json:"command_id,omitzero"`
    Cause     Cause     `json:"cause,omitzero"`
    Agency    Agency    `json:"agency,omitzero"` // who issued THIS command (machine by default)
}

// package event — accessor: func (h Header) EventHeader() Header
type Header struct {
    Coordinates                 // SessionID, LoopID, TurnID, StepID (embedded)

    EventID uuid.UUID `json:"event_id,omitzero"`
    Cause   Cause     `json:"cause,omitzero"` // Origin is deferred (see above)
}

// GateRoute addresses a gate reply to the right loop's pending gate. It IS the
// routing key: the session dispatches the command to loops[GateRoute.LoopID], and
// that loop matches the gate by GateRoute.ToolExecutionID (its pendingGates key).
// The TUI already tracks the producing LoopID (from the request event's
// Coordinates), so it stamps the route. LoopID dispatch is trivial today (one
// primary loop) but correct for subagents from day one.
type GateRoute struct {
    Coordinates                 // SessionID, LoopID, TurnID, StepID (embedded)
    ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
}
```

`ToolExecutionID` is **not** on the shared event `Header`. It is a field on the
four events that actually have one:

```go
type ToolCallStarted struct {
    ephemeral
    loopScoped
    Header                               // event.Header
    ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
    ToolName        string    `json:"tool_name,omitempty"`
    Summary         string    `json:"summary,omitempty"`
}
// likewise: PermissionRequested, UserInputRequested, ToolCallCompleted.
```

This single body `ToolExecutionID` replaces *both* today's per-event `CallID` and
the duplicate `event.Header.ToolCallID` — the tool id now lives in exactly one
place per event.

Gate commands embed `GateRoute`; `CancelQueuedInput` only needs to reach the loop,
so it carries plain `Coordinates` plus the target command id:

```go
type ApproveToolCall struct {
    Header                               // command.Header
    GateRoute                            // Coordinates + ToolExecutionID
    Scope tool.ApprovalScope `json:"scope,omitzero"`
}

type CancelQueuedInput struct {
    Header                               // command.Header
    Coordinates                          // addresses the loop holding the queued submit
    TargetCommandID uuid.UUID `json:"target_command_id,omitzero"` // the queued UserInput/SubagentResult
}

type SubagentResult struct {
    Header                               // command.Header; Cause.LoopID = CHILD loop; Agency = AgencyMachine
    Coordinates                          // addresses the PARENT loop (delivery target)
    Blocks []content.Block `json:"blocks,omitempty"`
}
```

The session's gate API takes the full `GateRoute` (`LoopID` + `ToolExecutionID`),
not a bare `callID`: it dispatches to `loops[GateRoute.LoopID]`, and the loop
matches the gate by `GateRoute.ToolExecutionID`. This replaces routing every gate
reply to the primary loop — a latent misroute the moment a subagent loop opens a
gate.

`SubagentResult` carries **two** loop ids with distinct jobs: `Coordinates.LoopID`
is the **parent** loop (delivery target — the session dispatches there), and
`Cause.LoopID` is the **child** loop that produced the result. When the parent
folds the result into a turn, that `Cause.LoopID` rides onto the resulting
`TurnStarted`/`TurnFoldedInto` (it is the old `TriggeredByLoopID`, which releases
the parent's quiescence wake token). E.g. child `C` finishes →
`SubagentResult{Coordinates{LoopID: P}, Header{Cause{LoopID: C}, Agency: AgencyMachine}, Blocks: R}`
→ session delivers to parent `P` → `P` emits `TurnStarted{… Cause.LoopID: C}`.

Command identity is `command.Header.CommandID`, not `Header.ID`. Event identity is
`event.Header.EventID`. Submit-resolution events do not need an `InputID` field —
the submit command is `Cause.CommandID`:

```go
type TurnStarted struct {
    enduring
    loopScoped
    Header                               // event.Header
    TurnIndex TurnIndex            `json:"turn_index,omitzero"`
    Message   *content.UserMessage `json:"message,omitzero"`
}
```

## Messages

There is **no per-message id.** Messages are payloads of the events and commands
that carry them, and each is already addressable through the existing id graph:

- a **user message** — by the `TurnID` it starts (or `Cause.CommandID` of its
  `TurnStarted`/`TurnFoldedInto`);
- the **AI message** of a step — by its `StepID` (one AI message per step);
- each **tool-result message** — by its `ToolExecutionID` (1:1 with the tool call).

So `content` stays exactly as-is — no envelope, no id, no generics. The journal is
event-sourced: it records events (each with `EventID` + `Coordinates`) and the
messages reconstruct from them, so nothing needs a standalone message identity. If
a future feature ever needs to address an arbitrary message *outside* its event
context, revisit — but commands and events are the correlation graph, and messages
ride inside them.

## Fill Rules

### Commands

Every command sets `command.Header.CommandID` before send (fail-secure: on mint
failure the sender sends no command). `Agency` defaults to `AgencyMachine`; the
user-origination paths stamp `AgencyUser` — a typed `UserInput`, a human
`ApproveToolCall`/`DenyToolCall`/`ProvideUserInput`, or a manual `Interrupt`.

| Command | Required identity | Required addressing | Cause |
|---|---|---|---|
| `UserInput` | `CommandID` | none; session chooses target loop | usually zero |
| `SubagentResult` | `CommandID` | embeds `Coordinates`; `LoopID` = parent loop | `Cause.LoopID` = child loop (was `FromLoopID`) |
| `CancelQueuedInput` | `CommandID`, `TargetCommandID` | `SessionID`, `LoopID` (`Coordinates`) | usually zero |
| `ApproveToolCall` | `CommandID` | `GateRoute` (`SessionID`, `LoopID`, `TurnID`, `StepID`, `ToolExecutionID`) | optional requested-event `EventID` |
| `DenyToolCall` | `CommandID` | `GateRoute` | optional requested-event `EventID` |
| `ProvideUserInput` | `CommandID` | `GateRoute` | optional requested-event `EventID` |
| `Interrupt` | `CommandID` | session-wide today; gains optional `Coordinates` loop-targeting when subagents land | usually zero |
| `Shutdown` | `CommandID` | session-wide today; gains optional `Coordinates` loop-targeting when subagents land | usually zero |

### Events

`ToolExecutionID` is a body field on the four tool/gate events only; other events
have no such field. "Must be zero" lists header coordinates that must stay unset.

| Event | Required identity | Coordinates that must be zero | Cause |
|---|---|---|---|
| `SessionStarted` | `SessionID`, `EventID` | `LoopID`, `TurnID`, `StepID` | zero |
| `SessionIdle` | `SessionID`, `EventID` | `LoopID`, `TurnID`, `StepID` | zero |
| `SessionStopped` | `SessionID`, `EventID` | `LoopID`, `TurnID`, `StepID` | zero |
| `LoopIdle` | `SessionID`, `LoopID`, `EventID` | `TurnID`, `StepID` | zero |
| `TurnStarted` | `SessionID`, `LoopID`, `TurnID`, `EventID` | `StepID` | `Cause.CommandID` = submit; `Cause.Agency` = submit's agency; `Cause.LoopID` = triggering loop if subagent-spawned |
| `TurnFoldedInto` | `SessionID`, `LoopID`, `TurnID`, `EventID` | `StepID` | `Cause.CommandID` = submit; `Cause.Agency` = submit's agency; `Cause.LoopID` = triggering loop if subagent-spawned |
| `InputCancelled` | `SessionID`, `LoopID`, `EventID` | `StepID` | `Cause.CommandID` = submit; `Cause.Agency` = submit's agency |
| `TokenDelta` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID` | none; tool-use chunks carry provider ids in the payload | zero |
| `StepDone` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID` | none | zero |
| `PermissionRequested` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID`, `ToolExecutionID` | none | zero |
| `UserInputRequested` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID`, `ToolExecutionID` | none | zero |
| `ToolCallStarted` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID`, `ToolExecutionID` | none | optional `Cause.CommandID` = approval command |
| `ToolCallCompleted` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID`, `ToolExecutionID` | none | optional `Cause.EventID` = started event |
| `TurnDone` | `SessionID`, `LoopID`, `TurnID`, `EventID` | `StepID` | zero |
| `TurnFailed` | `SessionID`, `LoopID`, `TurnID`, `EventID` | `StepID` | zero |
| `TurnInterrupted` | `SessionID`, `LoopID`, `TurnID`, `EventID` | `StepID` | `Cause.CommandID` = interrupt command when available |

For `InputCancelled` on client retraction outside an active turn, `TurnID` is
zero. For abnormal turn return, `TurnID` is the turn that returned the queued
message.

## Validation Invariants

- `EventID` is non-zero on every event.
- `CommandID` is non-zero on every command before send.
- `Agency` defaults to `AgencyMachine`; only the user-origination paths set
  `AgencyUser`. A missing value reads as machine (fail-secure attribution).
- `ScopeSession` events set only `SessionID` and `EventID` on identity fields.
- `ScopeLoop` events set `SessionID`, `LoopID`, and `EventID`.
- `StepID` requires `TurnID`.
- `ToolExecutionID` (on the four tool/gate events) requires `SessionID`, `LoopID`, `TurnID`, and `StepID`.
- Gate replies (approve/deny/provide-user-input) carry a `GateRoute` with non-zero `LoopID` and `ToolExecutionID`.
- A gate reply is dispatched by `GateRoute.LoopID` and matched to its gate by `GateRoute.ToolExecutionID`; a reply for one loop never reaches another. The provider `ToolUseID` is never used to route or match approvals.
- `ToolExecutionID` (our uuid) is never written into model-visible content; the provider `ToolUseID` (string) is the only tool id in `content`.
- Each `ToolExecutionID` binds 1:1 to exactly one originating `ToolUseBlock.ID`.

## Rename Map

| Current name | Normalized name |
|---|---|
| `Sesssion` (type) / `NewAgent` (ctor) | `Session` / `New` (`session.New`) |
| command `Header.ID` | `command.Header.CommandID` (struct stays `Header`; field `ID`→`CommandID`; accessor `CommandHeader()` kept) |
| event `Header.ID` | `event.Header.EventID` (struct stays `Header`; accessor `EventHeader()` kept) |
| event `Header.CausationID` | `event.Header.Cause.CommandID` or another `Cause` field |
| `InputID` on submit disposition/events | `TargetCommandID` for commands, `Cause.CommandID` for events |
| `CallID` (`uuid.UUID`) on gate/tool commands+events | `ToolExecutionID` (NOT split into a separate `GateID`) |
| event `Header.ToolCallID` | **removed** — it duplicated the per-event `CallID`; the single tool id is the body `ToolExecutionID` |
| `ToolCallID` (`uuid.UUID`) in `Route` | `GateRoute.ToolExecutionID` |
| `GateID` | **removed** — folded into `ToolExecutionID` |
| `Route` | `GateRoute` (gate commands) — the **routing key**: session dispatches by `GateRoute.LoopID`, loop matches by `GateRoute.ToolExecutionID`. `CancelQueuedInput` uses plain `Coordinates` |
| `content.ToolUseBlock.ID` / `content.ToolResultMessage.ToolUseID` | unchanged (the provider `ToolUseID`, content-only) |
| `TriggeredByLoopID` | `Cause.LoopID` on `SubagentResult`-caused events |
| `SubagentResult.FromLoopID` | `Cause.LoopID` (child); `SubagentResult` embeds `Coordinates` for parent-loop addressing |
| event `Header`'s `ParentLoopID`/`ParentTurnID`/`ParentStepID` | **removed** (dead — no reader). `loop.Provenance` retained on `loopState.parent`; `Origin` deferred until a consumer exists |
| (new) coordinate quartet | `Coordinates` (`SessionID/LoopID/TurnID/StepID`) — embedded by event `Header` / `GateRoute` / `Cause` |
| (new) actor of an action | `Agency` enum on command `Header` + `Cause` (default `AgencyMachine`) |

## Implementation Order

1. Rename `Sesssion` → `Session` and `NewAgent` → `New`; fix the stale
   "AgentSession" comments.
2. Create the `internal/agent/loop/identity` package with `Coordinates`, `Cause`
   (with `Agency`), and `Agency`. Reshape the command `Header` and event `Header`
   (embed the `identity` types; keep the struct named `Header` and the
   `CommandHeader()`/`EventHeader()` accessors) and add `GateRoute` (in `command`);
   add `uuid.MarshalText`/`UnmarshalText` and the `json` tags.
3. Rename command `Header.ID` → `CommandID`; rename event `Header.ID` → `EventID`;
   stamp `EventID` on all fan-in events. Stamp `AgencyUser` at the user-origination
   points (typed `UserInput`; human approve/deny/answer; manual `Interrupt`);
   everything else defaults `AgencyMachine`.
4. Replace `InputID` with `TargetCommandID` on commands and `Cause.CommandID` on
   submit-resolution events; copy `Cause.Agency` from the submit command.
5. Rename `CallID`/`ToolCallID` (`uuid.UUID`) → `ToolExecutionID` on the four
   tool/gate event bodies and `GateRoute`, and remove the now-duplicate
   `event.Header.ToolCallID`; delete `GateID`. Keep the provider
   `ToolUseID` (`string`) untouched in `content`; bind `ToolExecutionID` 1:1 to it
   in `runner.RunBatch`. Rename `Route` → `GateRoute`; give `CancelQueuedInput`
   plain `Coordinates`. Make the session gate API take a `GateRoute` and dispatch
   by `GateRoute.LoopID` (matched in-loop by `GateRoute.ToolExecutionID`),
   replacing route-to-primary-loop. Thread the producing `LoopID` from the pending
   prompt (`m.pending[].LoopID`) through `uiAction` into the gate command so the
   TUI can build the `GateRoute`.
6. Fold `TriggeredByLoopID`/`SubagentResult.FromLoopID` → `Cause.LoopID`. Remove
   the dead event `Header` `ParentLoopID/TurnID/StepID` fields; keep
   `loop.Provenance` on `loopState.parent` and defer `Origin` until a
   subagent-lineage consumer exists.
7. Add validation helpers/tests for the fill matrix before wiring journal/restore.

## Testing

- Table-driven validation tests for every event type: required IDs present,
  forbidden coordinates zero.
- Table-driven validation tests for every command type: `CommandID` present,
  addressing fields present when required.
- Agency tests: a command defaults to `AgencyMachine`; the user-origination paths
  (typed input, human approve/deny/answer, manual interrupt) produce `AgencyUser`;
  `Cause.Agency` on a submit-resolution event matches its submit command.
- Gate routing tests prove a reply is dispatched to the loop named by
  `GateRoute.LoopID` and matched to its gate by `GateRoute.ToolExecutionID` (never
  by the provider `ToolUseID`), and that a reply for loop A never reaches loop B.
- Binding test: each `ToolExecutionID` maps to exactly one originating
  `ToolUseBlock.ID`, and a `ToolResultMessage` looked up from a `ToolExecutionID`
  carries that same provider `ToolUseID`.
- Encoding tests prove model-visible pairing uses the provider `ToolUseID` and
  that no `ToolExecutionID` (`uuid.UUID`) is serialized to the provider.
- JSON tests prove zero `uuid.UUID` fields and the `AgencyMachine` default are
  dropped (`omitzero`) and non-zero ids round-trip via `MarshalText`/`UnmarshalText`.
- Submit lifecycle tests prove queued input cancellation targets
  `TargetCommandID`, while emitted events carry `Cause.CommandID`.
