# loop/command + loop/event Subpackage Extraction

Date: 2026-06-14

## Problem

All command types, event types, and their associated errors live flat in `package loop`.
`StartTurn.Events chan<- Event` couples the two concerns, making it impossible to move
commands to a subpackage without also moving events — otherwise a circular import forms.
The goal is one file per command type with its ack error co-located, and events in a
parallel sibling package.

## Approach

Extract two sibling subpackages from `internal/agent/loop/`:

- `loop/command/` — the Command interface and one file per command type
- `loop/event/` — the Event interface and all event/sink types

`loop` itself becomes the thin orchestrator that imports both.

---

## Package structure

```
internal/agent/loop/
├── loop.go          — Loop struct, New, actor
├── turn.go          — runTurn
├── config.go        — Config (gains loop/event import for EventSink)
├── errors.go        — ConfigError only
├── fake_test.go     — package loop (unchanged)
├── loop_test.go     — package loop, gains loop/command + loop/event imports
├── turn_test.go     — package loop, gains loop/event import
├── errors_test.go   — package loop, ConfigError tests only (renamed from types_test.go)
│
├── command/
│   ├── command.go     — Command interface
│   ├── start_turn.go  — StartTurn, validateStartTurn, TurnBusyError/Reason,
│   │                    CommandName/Field, InvalidCommandError
│   ├── interrupt.go   — Interrupt
│   └── shutdown.go    — Shutdown, LoopTerminatedError
│
└── event/
    ├── event.go   — Event interface, TurnIndex, SessionStarted
    ├── turn.go    — TurnStarted, TokenDelta, TurnDone, TurnFailed, TurnInterrupted
    ├── errors.go  — EmptyResponseError, TurnPanicError
    └── sink.go    — EventEnvelope, EventSink
```

---

## Import graph

```
loop/event   →  content, uuid
loop/command →  loop/event, content, context
loop         →  loop/event, loop/command, llm, uuid
session      →  loop, loop/event, loop/command
pa/agent_test→  loop/event, loop/command
```

No cycles. `loop/event` is the base of the DAG.

---

## Type placement

| Type | Destination |
|---|---|
| `Command` interface | `loop/command/command.go` |
| `StartTurn` | `loop/command/start_turn.go` |
| `validateStartTurn` (unexported) | `loop/command/start_turn.go` |
| `TurnBusyError`, `TurnBusyReason` | `loop/command/start_turn.go` |
| `CommandName`, `CommandField`, `InvalidCommandError` | `loop/command/start_turn.go` |
| `Interrupt` | `loop/command/interrupt.go` |
| `Shutdown` | `loop/command/shutdown.go` |
| `LoopTerminatedError` | `loop/command/shutdown.go` |
| `ConfigError` | `loop/errors.go` |
| `Event` interface, `TurnIndex`, `SessionStarted` | `loop/event/event.go` |
| `TurnStarted`, `TokenDelta`, `TurnDone`, `TurnFailed`, `TurnInterrupted` | `loop/event/turn.go` |
| `EmptyResponseError`, `TurnPanicError` | `loop/event/errors.go` |
| `EventEnvelope`, `EventSink` | `loop/event/sink.go` |
| `Config` | `loop/config.go` (unchanged, gains loop/event import) |
| `Loop`, `New`, actor | `loop/loop.go` (logic unchanged) |
| `runTurn` | `loop/turn.go` (logic unchanged) |

Ack errors live in the same file as the command that produces them (`TurnBusyError` with
`StartTurn`, `LoopTerminatedError` with `Shutdown`). `EmptyResponseError` and
`TurnPanicError` live in `loop/event` because they appear as `TurnFailed.Err` — they
are event payload errors, not command errors.

---

## Test placement

| Test | Destination | Package |
|---|---|---|
| `TestValidateStartTurn` | `loop/command/start_turn_test.go` | `package command` (white-box) |
| Command error message tests (`TurnBusyError`, `InvalidCommandError`) | `loop/command/start_turn_test.go` | `package command` |
| `LoopTerminatedError` message test | `loop/command/shutdown_test.go` | `package command` |
| Event error message tests (`EmptyResponseError`, `TurnPanicError`) | `loop/event/errors_test.go` | `package event` |
| `ConfigError` tests | `loop/errors_test.go` | `package loop` |
| Actor tests | `loop/loop_test.go` | `package loop` (white-box, gains subpackage imports) |
| Turn tests | `loop/turn_test.go` | `package loop` (white-box, gains loop/event import) |
| Fake LLM helpers | `loop/fake_test.go` | `package loop` (unchanged) |

White-box (`package loop`) is kept for actor and turn tests because they need access to
loop internals. White-box (`package command`) is kept for `validateStartTurn` which is
intentionally unexported. Event tests use `package event` for consistency.

---

## Consumer impact

| File | Change |
|---|---|
| `internal/agent/session/agent.go` | Add `loop/event`, `loop/command` imports; `loop.StartTurn` → `command.StartTurn`, `loop.TurnDone` → `event.TurnDone`, etc. |
| `agents/personal-assistant/agent_test.go` | `loop.TurnDone` → `event.TurnDone`, `loop.TurnBusyError` → `command.TurnBusyError`, etc. |
| `internal/agent/session/agent_test.go` | Same event/command import updates |

No logic changes in any consumer — only import paths and type qualifiers change.
