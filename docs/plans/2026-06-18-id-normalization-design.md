# ID Normalization Design

**Date:** 2026-06-18
**Status:** Draft
**Depends on:** `docs/plans/loop-machine-design.md`
**Supersedes:** the naming/correlation parts of
`docs/plans/2026-06-14-identity-correlation-design.md`

## Motivation

The loop-machine design adds session fan-in, multi-loop routing, per-turn and
per-step state, gate routing, queued input, and future journal/restore. Those
features need a shared ID vocabulary. Today names overlap:

- command `Header.ID` means command identity;
- event `Header.ID` is intended to mean event identity;
- `InputID` is actually the submit command id;
- `CallID`, `ToolCallID`, `ToolUseID`, and gate routing all describe different
  pieces of the tool path;
- `CausationID` is a single flat field, but the thing that caused an action can
  be a command, event, turn, step, tool use, or gate.

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
- Provider/model IDs stay strings when the provider owns the format. Internal
  runtime IDs are `uuid.UUID`.

## Identity Vocabulary

| ID | Type | Minted by | Meaning |
|---|---|---|---|
| `SessionID` | `uuid.UUID` | `AgentSession` | User-visible session boundary shared by all loops in the session. |
| `LoopID` | `uuid.UUID` | `AgentSession.NewLoop` | One loop goroutine/agent instance inside a session. |
| `TurnID` | `uuid.UUID` | `runLoop` | One user/subagent initiated turn in one loop. |
| `StepID` | `uuid.UUID` | `runTurn`/`runStep` | One LLM response step inside a turn. |
| `CommandID` | `uuid.UUID` | Command issuer | One command sent to a loop/session. |
| `EventID` | `uuid.UUID` | Event producer | One event published to streams/fan-in/sinks. |
| `ToolUseID` | `string` | Model/provider | Model tool-use id paired with a model-visible tool result. |
| `GateID` | `uuid.UUID` | Loop/tool runtime | Runtime gate used to route approve/deny/user-input replies. |

`ToolUseID` and `GateID` are intentionally separate. `ToolUseID` is the
provider/model pairing id and belongs in `content.ToolUseBlock` and
`content.ToolResultMessage`. `GateID` is our runtime reply-routing id for a
permission/user-input gate. A gated tool call has both. An auto-approved tool
call has a `ToolUseID` and no `GateID`.

## Cause

Causation is not another naming scheme. It is the same ID vocabulary nested under
`Cause`:

```go
type Cause struct {
    SessionID uuid.UUID
    LoopID    uuid.UUID
    TurnID    uuid.UUID
    StepID    uuid.UUID

    CommandID uuid.UUID
    EventID   uuid.UUID

    ToolUseID string
    GateID    uuid.UUID
}
```

`Cause` means "the direct thing that caused this", not "the whole ancestry". Most
events have a zero `Cause`; their scope fields already say where they happened.
Set `Cause` only when a consumer needs the direct edge:

- submit-resolution events (`TurnStarted`, `TurnFoldedInto`, `InputCancelled`)
  set `Cause.CommandID` to the `UserInput`/`SubagentResult` command id;
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
Routes stay separate from both.

```go
type CommandHeader struct {
    CommandID uuid.UUID
    Cause     Cause
}

type EventHeader struct {
    SessionID uuid.UUID
    LoopID    uuid.UUID
    TurnID    uuid.UUID
    StepID    uuid.UUID

    EventID uuid.UUID
    Cause   Cause

    ToolUseID string
    GateID    uuid.UUID
}

type Route struct {
    SessionID uuid.UUID
    LoopID    uuid.UUID
    TurnID    uuid.UUID
    StepID    uuid.UUID
    GateID    uuid.UUID
}
```

Command identity is `CommandHeader.CommandID`, not `Header.ID`. Event identity is
`EventHeader.EventID`, not `Header.ID`. A command that targets a queued input uses
the target command id explicitly:

```go
type CancelQueuedInput struct {
    CommandHeader
    Route           Route
    TargetCommandID uuid.UUID // the queued UserInput/SubagentResult command
    Ack             chan<- CancelResult
}
```

Submit-resolution events do not need an `InputID` field after normalization:

```go
type TurnStarted struct {
    enduring
    loopScoped
    EventHeader
    TurnIndex TurnIndex
    Message   *content.UserMessage
}
```

The submit command is available as `EventHeader.Cause.CommandID`.

## Fill Rules

### Commands

| Command | Required identity | Required route | Cause |
|---|---|---|---|
| `UserInput` | `CommandID` | none; session chooses target loop | usually zero |
| `SubagentResult` | `CommandID` | parent `LoopID` if routed by session | child loop/event cause when available |
| `CancelQueuedInput` | `CommandID` and `TargetCommandID` | `SessionID`, `LoopID` | usually zero |
| `ApproveToolCall` | `CommandID` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `GateID` | optional requested-event `EventID` |
| `DenyToolCall` | `CommandID` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `GateID` | optional requested-event `EventID` |
| `ProvideUserInput` | `CommandID` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `GateID` | optional requested-event `EventID` |
| `Interrupt` | `CommandID` | session or loop target, depending API | usually zero |
| `Shutdown` | `CommandID` | session or loop target, depending API | usually zero |

Command senders mint `CommandID` before sending. If minting fails, they fail
securely and send no command.

### Events

| Event | Required identity | Must be zero | Cause |
|---|---|---|---|
| `SessionStarted` | `SessionID`, `EventID` | `LoopID`, `TurnID`, `StepID`, `ToolUseID`, `GateID` | zero |
| `SessionIdle` | `SessionID`, `EventID` | `LoopID`, `TurnID`, `StepID`, `ToolUseID`, `GateID` | zero |
| `SessionStopped` | `SessionID`, `EventID` | `LoopID`, `TurnID`, `StepID`, `ToolUseID`, `GateID` | zero |
| `LoopIdle` | `SessionID`, `LoopID`, `EventID` | `TurnID`, `StepID`, `ToolUseID`, `GateID` | zero |
| `TurnStarted` | `SessionID`, `LoopID`, `TurnID`, `EventID` | `StepID`, `ToolUseID`, `GateID` | `Cause.CommandID` = submit command |
| `TurnFoldedInto` | `SessionID`, `LoopID`, `TurnID`, `EventID` | `StepID`, `ToolUseID`, `GateID` | `Cause.CommandID` = submit command |
| `InputCancelled` | `SessionID`, `LoopID`, `EventID` | `StepID`, `ToolUseID`, `GateID` | `Cause.CommandID` = submit command |
| `TokenDelta` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID` | `ToolUseID`, `GateID` on the header; tool-use chunks carry provider ids in the payload | zero |
| `StepDone` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID` | `GateID` | zero |
| `PermissionRequested` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID`, `ToolUseID`, `GateID` | none | zero |
| `UserInputRequested` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID`, `GateID` | `ToolUseID` unless tied to a tool use | zero |
| `ToolCallStarted` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID`, `ToolUseID` | `GateID` if auto-approved | optional approval command |
| `ToolCallCompleted` | `SessionID`, `LoopID`, `TurnID`, `StepID`, `EventID`, `ToolUseID` | `GateID` if auto-approved | optional started event |
| `TurnDone` | `SessionID`, `LoopID`, `TurnID`, `EventID` | `StepID`, `ToolUseID`, `GateID` | zero |
| `TurnFailed` | `SessionID`, `LoopID`, `TurnID`, `EventID` | `StepID`, `ToolUseID`, `GateID` | zero |
| `TurnInterrupted` | `SessionID`, `LoopID`, `TurnID`, `EventID` | `StepID`, `ToolUseID`, `GateID` | interrupt command when available |

For `InputCancelled` on client retraction outside an active turn, `TurnID` is
zero. For abnormal turn return, `TurnID` is the turn that returned the queued
message.

## Validation Invariants

- `EventID` is non-zero on every event.
- `CommandID` is non-zero on every command before send.
- `ScopeSession` events set only `SessionID` and `EventID` on identity fields.
- `ScopeLoop` events set `SessionID`, `LoopID`, and `EventID`.
- `StepID` requires `TurnID`.
- `GateID` requires `SessionID`, `LoopID`, `TurnID`, and `StepID`.
- `ToolUseID` requires `SessionID`, `LoopID`, `TurnID`, and `StepID`.
- `Route.GateID` is required for gate replies.
- `ToolUseID` is never used to route user approvals; `GateID` is.
- `GateID` is never written into model-visible content; `ToolUseID` is.

## Rename Map

| Current name | Normalized name |
|---|---|
| command `Header.ID` | `CommandHeader.CommandID` |
| event `Header.ID` | `EventHeader.EventID` |
| event `Header.CausationID` | `EventHeader.Cause.CommandID` or another `Cause` field |
| `InputID` on submit disposition/events | `TargetCommandID` for commands, `Cause.CommandID` for events |
| `CallID` on gate commands/events | `GateID` |
| `ToolCallID` in routes/headers | `GateID` for routing, `ToolUseID` for model tool-use pairing |
| `TriggeredByLoopID` | `Cause.LoopID` on `SubagentResult`-caused events |
| `ParentLoopID`/`ParentTurnID`/`ParentStepID` | display provenance; keep separate from `Cause` unless it is the direct cause |

## Implementation Order

1. Add `Cause`, `CommandHeader`, `EventHeader`, and normalized `Route` shapes in
   the command/event specs.
2. Rename command `Header.ID` to `CommandID`; update session command minting.
3. Rename event `Header.ID` to `EventID`; stamp it on all fan-in events.
4. Replace `InputID` with `TargetCommandID` on commands and `Cause.CommandID` on
   submit-resolution events.
5. Split current `CallID`/`ToolCallID` usages into `GateID` for gate routing and
   `ToolUseID` for model/tool-result pairing.
6. Add validation helpers/tests for the fill matrix before wiring journal/restore.

## Testing

- Table-driven validation tests for every event type: required IDs present,
  forbidden IDs zero.
- Table-driven validation tests for every command type: `CommandID` present,
  route fields present when required.
- Gate routing tests prove approvals route by `GateID`, not `ToolUseID`.
- Tool-result encoding tests prove model-visible pairing still uses `ToolUseID`.
- Submit lifecycle tests prove queued input cancellation targets
  `TargetCommandID`, while emitted events carry `Cause.CommandID`.
