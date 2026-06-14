# Identity & Correlation — Design

Date: 2026-06-14 · Status: approved (brainstorm)

## Scope

A uniform identity model for the agent loop: stable **entity IDs**, per-message
**identity IDs**, a **causal edge** between messages, and the **correlation**
grouping that ties a flow together. The purpose is twofold — a substrate for
*idempotency* and first-class *tracing*.

This is a cross-cutting concern that touches `internal/agent/loop`,
`internal/agent/loop/command`, and `internal/agent/loop/event` independent of
any single feature. It is specified here, separately, so it can be reused by
future work (gateway, persistence, multi-agent, subagents). The tools subsystem
(`docs/plans/2026-06-14-tools-design.md`) is its first real consumer and only
*uses* the model — chiefly `TurnID` and `CallID`.

It deliberately does **not** introduce a dedup/idempotency cache (see *Idempotency*).

---

## The ID taxonomy

There are four distinct roles. Conflating them is the mistake this design avoids
(it mirrors the standard correlation/causation pattern, and distributed-tracing's
trace → span → span-id → parent-id hierarchy).

| Role | Question it answers | Members |
|---|---|---|
| **Entity ID** | "Which *thing* is this?" | `SessionID`, `TurnID`, `CallID` |
| **Message ID** | "Which *message instance* is this?" | command `Header.ID`, `EventID` |
| **Causal edge** | "What *directly caused* this message?" | `CausationID` |
| **Correlation** | "Which *flow* does this belong to?" | `SessionID ⊃ TurnID ⊃ CallID` |

Entity IDs nest: `SessionID ⊃ TurnID ⊃ CallID`. A `CallID` groups *every* message
about one tool call; a `TurnID` groups everything in one turn; a `SessionID`
groups the session.

All IDs are `uuid.UUID` (`internal/uuid`, v4, crypto/rand). The zero value
(`uuid.UUID{}`) means "absent / root". Add a small helper:

```go
// in internal/uuid
func (u UUID) IsZero() bool { return u == UUID{} }
```

---

## Commands carry a `Header`

Every command embeds a `Header` and the sealed interface exposes it without a
type switch.

```go
package command

// Header is the correlation/idempotency metadata embedded in every command.
type Header struct {
    ID          uuid.UUID // fresh per command instance (uuid.New at construction)
    CausationID uuid.UUID // message-ID of the cause; zero = root (user-initiated)
}

// CommandHeader is promoted onto every command that embeds Header.
func (h Header) CommandHeader() Header { return h }

// Command is the sealed command interface.
type Command interface {
    isCommand()
    CommandHeader() Header
}
```

Each concrete command embeds `Header` (which promotes `CommandHeader()`) and
keeps its own `isCommand()`:

```go
type StartTurn struct {
    Header                       // embed → promotes CommandHeader()
    Ctx       context.Context
    Input     []content.Block
    Events    chan<- event.Event
    Abandoned <-chan struct{}
    Ack       chan<- error
}
func (StartTurn) isCommand() {}
```

`Interrupt`, `Shutdown`, and the three tool control commands
(`ApproveToolCall`, `DenyToolCall`, `ProvideUserInput` — defined in the tools
design) embed `Header` the same way. The channel type stays `chan command.Command`;
no envelope wrapper.

### `CausationID` semantics for commands

`CausationID` is the **message-ID of the cause, when the issuer holds it; zero
otherwise.** Commands are overwhelmingly **user-initiated roots** — a human
gesture has no upstream message — so this is usually zero:

| Command | Caused by | `CausationID` |
|---|---|---|
| `StartTurn` | a human typing a prompt + Enter | zero (root) |
| `Interrupt` | the user pressing Esc | zero (root) |
| `Shutdown` | Ctrl+C / app teardown | zero (root) |
| `ApproveToolCall` / `DenyToolCall` | answering a `PermissionRequested` | zero — link carried by `CallID` (below) |
| `ProvideUserInput` | answering a `UserInputRequested` | zero — link carried by `CallID` |

It is kept on the `Header` uniformly (rather than events-only) so it is ready for
the machine-issued commands that *will* set it — chiefly a subagent's child
`StartTurn`, and future scheduled/retried turns. Keeping it mostly-zero today is
intended, not a gap.

---

## Events stay bare on the stream; correlation rides in the envelope

The per-turn `StartTurn.Events` channel delivers **bare `event.Event` values** so
the in-flight TUI (`readNext` / `StreamReader[event.Event]`) is untouched.
Correlation metadata is added in the `EventEnvelope` the loop already builds for
sinks:

```go
package event

type EventEnvelope struct {
    SessionID   uuid.UUID
    TurnID      uuid.UUID  // NEW — entity id for the turn
    TurnIndex   TurnIndex  //       kept for ordering + history rollback
    EventID     uuid.UUID  // NEW — fresh per emitted event
    CausationID uuid.UUID  // NEW — the command that caused it (the active StartTurn.ID)
    CallID      uuid.UUID  // NEW — tool-call correlation when applicable; zero otherwise
    Event       Event
}
```

`EventID`/`CausationID` are pure tracing metadata and live **only** in the
envelope. For events, `CausationID` is *rich*: the loop holds the active
`StartTurn.ID`, so every event emitted during a turn gets
`CausationID = StartTurn.ID`. A sink (OTel exporter, audit log) can therefore
trace any event by `SessionID → TurnID → CallID` and walk causation by `EventID`/
`CausationID`, without a single tracing field polluting `TurnDone`/`TokenDelta`/etc.

---

## `CallID` — the tool-call entity ID (domain-meaningful, on bare events)

Unlike `EventID`, **`CallID` appears on the bare event structs themselves**
(`PermissionRequested`, `UserInputRequested`, `ToolCallStarted`,
`ToolCallCompleted`) and on the response commands. It is *domain-meaningful*, not
mere tracing metadata, for two reasons:

1. **The TUI must echo it.** The TUI receives bare events and never sees an
   `EventID` (envelope-only). It reads `CallID` off `PermissionRequested` and
   echoes it back in `ApproveToolCall` — that is the gate's request↔response join.
2. **It is the stable grouping id for the whole tool-call lifecycle.** The runner
   generates `CallID` once (`uuid.New`) *before* the permission check; the same
   value flows through the gate request (if any), `ToolCallStarted`, and
   `ToolCallCompleted`. "Show everything for call C" is a filter on `CallID`.

### Why `CallID` is **not** redundant with `CausationID`

They coincide in *value* only at the single `ApproveToolCall` message — and even
there they answer different questions (`CallID`: "which call does this belong
to?"; `CausationID`: "what message caused this?"). They diverge everywhere else:

- **Auto-approved calls have no gate message.** A session-policy-approved `Bash`
  emits no `PermissionRequested`, yet still emits `ToolCallStarted{CallID:C}` /
  `ToolCallCompleted{CallID:C}`. Without `CallID` the call has no stable id and
  grouping degrades to a causation-chain traversal.
- **`CausationID` differs per message;** `CallID` is constant across a call's
  messages.

So both are kept, each single-purpose: `CallID` = the tool-call "span";
`CausationID` = the parent-message edge. `ApproveToolCall.CausationID` stays
**zero** (the gate correlation is carried entirely by `CallID`).

---

## Where IDs are assigned

| ID | Assigned by | When |
|---|---|---|
| `SessionID` | `session.NewAgent` (already) | session construction |
| `TurnID` | the loop actor | when accepting a `StartTurn` (fresh per turn); kept on `loopState` alongside `turnIndex` |
| command `Header.ID` | the **session** (and any command issuer) | at command construction, `uuid.New` |
| `EventID` | the loop's `emit` path | when building each `EventEnvelope` |
| envelope `CausationID` | the loop's `emit` path | = active `StartTurn.ID` |
| `CallID` | the **tool runner** | per tool call, `uuid.New`, before the permission check |

`loopState` gains `turnID uuid.UUID` (set on turn start, cleared on turn end),
used to stamp the envelope.

---

## Idempotency

The IDs are the **substrate**; this design does **not** build a dedup cache.

In-process, channels deliver exactly once and `StartTurn` already has an `Ack`, so
there is no replay/retry to dedupe. The one place idempotency matters today —
duplicate/stale approve/deny — is already handled by **`CallID` validation in the
tool runner**: a second `ApproveToolCall` for an already-resolved `CallID` is
dropped (a harmless no-op). See the tools design, §2c.

A `seen-set` keyed on command `Header.ID` is the documented **seam** to add when a
network/persistence boundary appears (gateway, durable command log, retried
delivery). Not before — YAGNI.

---

## Worked example — causation chain

A turn that writes a file (gate) and then reads one (auto-approved):

```
StartTurn            Header{ID:S, CausationID:0}                 (user root)
  ├─ TurnStarted     envelope{EventID:E1, CausationID:S, TurnID:T}
  ├─ TokenDelta×N    envelope{EventID:Ek, CausationID:S, TurnID:T}
  ├─ PermissionReq   envelope{EventID:Ep, CausationID:S, CallID:C1}   (bare event carries CallID:C1)
ApproveToolCall      Header{ID:A, CausationID:0}, CallID:C1            (user, gate link via CallID)
  ├─ ToolCallStarted envelope{EventID:Es, CausationID:S, CallID:C1}
  ├─ ToolCallDone     envelope{EventID:Ed, CausationID:S, CallID:C1}
  ├─ ToolCallStarted envelope{EventID:..., CausationID:S, CallID:C2}  (ReadFile, auto-approved: no gate)
  ├─ ToolCallDone     envelope{EventID:..., CausationID:S, CallID:C2}
  └─ TurnDone        envelope{EventID:..., CausationID:S, TurnID:T}
```

"Everything for the write" = filter `CallID == C1`. "Everything for this turn" =
filter `TurnID == T`. Causation chains the messages within the flow.

---

## Blast radius

- `internal/uuid` — add `IsZero()`.
- `internal/agent/loop/command` — add `Header`, widen the `Command` interface with
  `CommandHeader()`, embed `Header` in every command (additive).
- `internal/agent/loop/event` — add the four envelope fields.
- `internal/agent/loop` — `loopState` gains `turnID`; assign `TurnID` on turn
  start; stamp `EventID`/`CausationID`/`TurnID`/`CallID` on the envelope at emit;
  may read `CommandHeader()` for logging.
- `internal/agent/session` — stamp `Header.ID` (`uuid.New`) when constructing the
  commands it sends.

The per-turn event **stream** and therefore the in-flight TUI are **unchanged** —
bare events gain no fields (except `CallID`, added by the tools design only to the
tool-call/gate events the TUI already needs).

---

## Testing

Table-driven, `-race`:

- `uuid.IsZero` — zero vs. non-zero.
- `command.Header` — `CommandHeader()` promotion on each command; `Command`
  interface satisfied.
- envelope stamping — `EventID` unique per event, `CausationID == StartTurn.ID`,
  `TurnID` constant within a turn and distinct across turns, `CallID` zero for
  non-tool events.
- causation chain — drive a scripted turn and assert the chain in the worked
  example holds.

---

## Out of scope

- Dedup/idempotency **cache** (substrate only; add at a replay boundary).
- Surfacing `EventID` on bare stream events (envelope-only by design).
- Network/persistence command log, durable correlation store.
- Distributed-trace export format (an `EventSink` concern, not this model).
