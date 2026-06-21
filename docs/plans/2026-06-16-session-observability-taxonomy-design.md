# Design A — Session Observability & Taxonomy Foundation

**Date:** 2026-06-16
**Status:** Approved design, pending implementation plan
**Companion:** [Design B — Invoke Control Semantics & Autonomous Headless Mode](2026-06-16-autonomous-headless-mode-design.md) builds on this.

## Motivation

A headless consumer driving the agent from custom Go code (no TUI/CLI) needs a
complete, correlatable session history and a principled way to reason about which
events demand a response. Two gaps block that today:

1. **No typed categorisation of commands/events.** The old core loop (see
   `docs/old/2.md`) split commands into `turnCommand` vs `controlCommand`; the
   current code degraded this to a single `Command` interface plus a structural
   `GateCallID()` marker (named only in a test, `command/control_test.go:13`).
   Events have no categorisation at all. Consumers cannot ask "is this event one I
   must answer?" without string/type matching.
2. **Asymmetric, incomplete observability.** Per-turn consumers
   (`session.Invoke`/`Stream`) receive bare events with no `SessionID`, event id,
   timestamp, or causation metadata; only `EventSink`s get `EventEnvelope`
   (`event/sink.go:16`). Sinks receive events only, never commands, so the journal
   records "what happened" but not "what was requested." Nothing carries a creation
   timestamp.

This design establishes the taxonomy and the command/event journal foundation. It
is valuable independent of autonomous mode (Design B), which depends on it.

## Scope

In: event taxonomy (Turn/Control/Notification + Terminal marker), command
taxonomy (Turn/Control), event metadata as an embedded `event.Header`,
`CreatedAt` on commands and events with injectable clock seams, the `CallID` to
`ToolCallID` rename, and a unified `loop.Journal` that records both commands and
events.

Out: Invoke semantics, autonomous mode, notification emission sites, TUI
rendering, durable on-disk journal format, replay/session-restore mechanics, and
redaction. This design records full semantic command/event data. Runtime-only Go
handles such as contexts and channels are not durable session data.

---

## A1. Event taxonomy — three categories + a terminal marker

Three sealed sub-interfaces of `event.Event` (`event/event.go`), classified by
what the consumer must do:

| Category | Marker | Requires a response? | Members |
|---|---|---|---|
| **Turn** | `TurnEvent` | no - work/progress | `TurnStarted`, `TokenDelta`, `ToolCallStarted`, `ToolCallCompleted`, `TurnDone`, `TurnFailed`, `TurnInterrupted` |
| **Control** | `ControlEvent` (`Event` + `GateCallID() uuid.UUID`) | yes - a control command back | `PermissionRequested`, `UserInputRequested` |
| **Notification** | `NotificationEvent` (`Event` + `Severity() NotificationSeverity` + `Message() string`) | no - diagnostic | `SessionStarted` (reclassified), plus emissions added in Design B |

- `TerminalEvent` is a refinement marker within Turn, implemented by exactly
  `TurnDone`/`TurnFailed`/`TurnInterrupted`, so `Invoke` (Design B) can write
  `case TerminalEvent: return ev` instead of enumerating three types.
- `NotificationSeverity` is a typed enum: `SeverityInfo` / `SeverityWarn` /
  `SeverityError`.
- `ControlEvent.GateCallID()` returns `EventHeader().ToolCallID`, pairing the
  event with the control command that answers it.

Symmetry with commands is intentionally partial: events have a third category
(Notification) that commands lack, because diagnostics flow outward only.

**Exhaustiveness invariant (test-pinned).** A table test asserts every concrete
`event.Event` implements exactly one of `TurnEvent`/`ControlEvent`/
`NotificationEvent`; exactly the three terminal events implement `TerminalEvent`.
This makes adding a new event a compile-or-test forcing function to classify it.

### Type identity vs category

In-process code uses the concrete Go type as the event/command kind
(`TokenDelta`, `PermissionRequested`, `StartTurn`, etc.) and marker interfaces as
the category (`TurnEvent`, `ControlEvent`, `NotificationEvent`,
`TurnCommand`, `ControlCommand`). Do not add a `Type` or `Kind` field to
`event.Header` or `command.Header`; a duplicated discriminator can drift from the
actual concrete type.

A durable journal format will need an explicit serialized kind string, but that
belongs in the journal codec/record format and is derived from the concrete type
at the persistence boundary. It is not part of the in-process domain event or
command structs in Design A.

### Journalability invariant

Every concrete `event.Event` is journalable. If the loop needs an internal signal
that must not appear in the journal, that signal should not implement
`event.Event`; model it as an internal loop type instead. `event.Event` means
"part of the externally observable session history."

## A2. Command taxonomy — two categories

Re-formalise the old split as named, sealed marker interfaces in
`internal/agent/loop/command`:

- `TurnCommand` - `StartTurn` (work enqueued to the runner).
- `ControlCommand` - `Shutdown`, `Interrupt`, `ApproveToolCall`, `DenyToolCall`,
  `ProvideUserInput` (handled out-of-band / route to a parked gate).

The gate-routing subset keeps its `GateCallID()` method. No behavioural change -
this only names what is currently structural.

## A3. Event header instead of `EventEnvelope`

Remove `event.EventEnvelope`. Metadata becomes part of each event, matching the
command model where every command embeds `command.Header`.

```go
package event

type Header struct {
    SessionID   uuid.UUID
    TurnID      uuid.UUID
    TurnIndex   TurnIndex
    ID          uuid.UUID // event id
    CreatedAt   time.Time
    CausationID uuid.UUID // causing command Header.ID; zero for root/session events
    ToolCallID  uuid.UUID // zero unless the event pertains to a tool call
    Sequence    uint64    // loop-local journal/delivery ordering
}

func (h Header) EventHeader() Header { return h }

type Event interface {
    isEvent()
    EventHeader() Header
}
```

Every concrete event embeds `event.Header`:

```go
type TokenDelta struct {
    Header
    Chunk content.Chunk
}

type PermissionRequested struct {
    Header
    Request tool.PermissionRequest
}

type TurnDone struct {
    Header
    Message *content.AIMessage
}
```

The header is the single source for event metadata:

- remove `TurnIndex` fields from `TurnStarted`, `TokenDelta`, `TurnDone`,
  `TurnFailed`, and `TurnInterrupted`;
- remove `CallID` fields from tool/control events;
- remove `SessionStarted.SessionID`; `SessionID` comes from `event.Header`;
- use `ev.EventHeader().TurnIndex` and `ev.EventHeader().ToolCallID`.

The per-turn channel remains `chan event.Event`; `session.Invoke` and
`session.Stream` continue to return/read `event.Event`. Existing type switches
remain shaped around the concrete event type; callers that need metadata read
`ev.EventHeader()`.

### Stamping model

The loop stamps event headers immediately before delivering or journaling an
event. Producers may set only the local metadata they know, primarily
`ToolCallID`; the loop fills the fields it owns:

- `SessionID`
- `TurnID`
- `TurnIndex`
- `ID`
- `CreatedAt`
- `CausationID`
- `Sequence`

`Sequence` is the loop-local journal record order. It is assigned by the
serialized `journalRecorder` when a command/event is recorded. It orders journal
records by the moment they pass through the recorder; it is not a stronger
causality claim between independent goroutines.

### Stamping helpers

Headers are embedded in value structs behind interfaces, so stamping must be
copy-based and exhaustive:

```go
func stampEventHeader(ev event.Event, h event.Header) event.Event
func stampCommandSequence(cmd command.Command, seq uint64) command.Command
```

Both helpers use type switches over every concrete event/command. They return a
copy with the updated embedded header. Exhaustiveness tests make adding a new
event or command a forcing function to update these helpers.

The recorder exposes these helpers at the delivery boundaries:

```go
func (r *journalRecorder) recordCommand(cmd command.Command) command.Command
func (r *journalRecorder) recordEvent(ev event.Event, h event.Header) event.Event
```

`recordCommand` returns the stamped command copy; the actor should switch on that
returned value. `recordEvent` returns the stamped event copy; the loop delivers
that same value to both `Journal.RecordEvent` and the per-turn channel.

### `CreatedAt` on both sides

- `event.Header.CreatedAt` is stamped by the loop when it stamps the event header.
- `command.Header` gains `CreatedAt time.Time`, stamped by the sender at command
  mint. The session already mints command ids in one place (`newCommandID`,
  `session/agent.go:67`); promote it to `newHeader()` returning
  `command.Header{ID, CreatedAt}`.
- `command.Header` also gains `Sequence uint64`, stamped by the loop on the
  command copy handed to `Journal.RecordCommand`. Senders leave it zero.

### Clock seams

Mirror the existing `idGen`/`newID` injection:

- `loop.Config` gains `now func() time.Time` (default
  `func() time.Time { return time.Now().UTC() }`) - stamps event `CreatedAt`.
- The session gains a `now func() time.Time` seam - stamps command `CreatedAt`.

Both default to UTC wall-clock and are overridden in tests for determinism.

### Resulting correlation surface

| | ID | Time | Causality | Order |
|---|---|---|---|---|
| **Command** | `command.Header.ID` | `command.Header.CreatedAt` | `command.Header.CausationID` | `command.Header.Sequence` |
| **Event** | `event.Header.ID` | `event.Header.CreatedAt` | `event.Header.CausationID` | `event.Header.Sequence` |

Command ids are authored by senders. Event ids and event headers are authored by
the loop. The journal sees both with enough metadata to correlate commands,
events, turns, tool calls, and total loop order.

## A4. `CallID` to `ToolCallID` rename

`CallID` is the loop's internally-minted tool-call identifier (`runner.go:88`
"mints a CallID per call"; routed for gates + event correlation). Rename the
field `CallID` to `ToolCallID` across its bounded context:

- event metadata: `event.Header.ToolCallID`;
- command fields: `ApproveToolCall.ToolCallID`, `DenyToolCall.ToolCallID`,
  `ProvideUserInput.ToolCallID`;
- runner/gate/loop/session consumers and TUI prompt/action/message models.

The `GateCallID()` method name stays as-is. The `Gate` prefix disambiguates the
routing method, and keeping the name avoids churn in the routing abstraction.

Exclude `internal/llm/openaiapi`'s `ToolCallID` (the provider wire
`tool_call_id`, a different model-level concept) and all vendor code. Use a
scope-aware symbol rename, not text replacement.

Three distinct ids exist:

| Identifier | Type | Layer |
|---|---|---|
| `content.ToolUseBlock.ID` / `ToolResultBlock.ToolUseID` | `string` | model message thread |
| `openaiapi` `ToolCallID` | `string` | provider wire adapter |
| loop `ToolCallID` (was `CallID`) | `uuid.UUID` | loop runtime (gate routing + correlation) |

## A5. Unified `Journal` — commands and events

`EventSink` is misleading once the attachment records both directions. Replace it
with `Journal` in the `loop` package, not the `event` package, because the
interface depends on both `command` and `event`. Keeping it in `event` would
create an import cycle (`event -> command -> event`) because `command.StartTurn`
already imports `event`.

```go
// internal/agent/loop/journal.go
package loop

type Journal interface {
    RecordCommand(command.Command)
    RecordEvent(event.Event)
}
```

`loop.Config` changes from:

```go
Sinks []event.EventSink
```

to:

```go
Journals []Journal
```

`Config.Journals` is construction-time dependency wiring. The loop runtime state
keeps the active recorder next to the conversation history:

```go
type loopState struct {
    msgs    content.AgenticMessages
    journal journalRecorder
}

type journalRecorder struct {
    mu       sync.Mutex
    journals []Journal
    nextSeq  uint64
}
```

Use a value field, not `*journalRecorder`: the recorder is always present, even
with zero configured journals, and is owned by exactly one `loopState`. Its
methods take `*journalRecorder` receivers to mutate `nextSeq`; Go will take the
address of `state.journal` automatically.

`journalRecorder` is the one loop-state component intentionally called from both
the actor goroutine and the turn goroutine. It serializes those calls with
`mu`, assigns `Sequence`, invokes journals, and recovers journal panics. Because
the recorder serializes calls, individual `Journal` implementations do not need
to be concurrency-safe for calls from one loop. They still must return quickly and
must not call back into the same loop, because recording is synchronous at
command/event delivery boundaries.

`newJournalRecorder(cfg.Journals)` copies the configured slice with
`append([]Journal(nil), journals...)` so caller-side mutation of `Config.Journals`
after `loop.New` cannot change the running loop's journal set.

The loop records every received command and every delivered event through this
recorder. Design A does not redact journal data and does not change an event into
a different journal-only type. The journal receives the same concrete event type
that per-turn consumers receive, now carrying its embedded `event.Header`.

The old sink-projection path is removed in this design: delete `event.Redactable`,
`SinkProjection` methods, `UserInputRequestedSink`, and `loop.projectForSink`.

The recorder stamps command/event copies even when no journals are configured, so
per-turn consumers still receive event headers with `ID`, `CreatedAt`, and
`Sequence`.

Commands are recorded immediately after the actor receives them from
`Loop.Commands`, before validation, routing, ack sends, rejection, cancellation,
or any other side effect. That includes invalid commands and rejected/busy
`StartTurn`s; the journal records what was requested, not only what was accepted.

Events are recorded at their natural delivery points:

- non-terminal events in `emit`, before sending to the per-turn channel;
- terminal events in `deliverAndClose`, before sending the terminal event and
  closing the channel;
- session-level notifications such as `SessionStarted` before the actor enters
  its command loop.

### Runtime handles vs durable data

`Journal` does not accept `context.Context`. A journal implementation that writes
to disk, a database, or a queue owns its own context and buffering.

Some command fields are runtime handles, not restorable session data:

```go
type StartTurn struct {
    Header
    Ctx       context.Context
    Input     []content.Block
    Events    chan<- event.Event
    Abandoned <-chan struct{}
    Ack       chan<- error
}
```

The in-process `Journal` hook sees the live command value. A durable journal
format, deferred to a restore design, serializes semantic data (`Header`, command
kind, `Input`, `ToolCallID`, approval scope, user answer) and excludes runtime
handles (`Ctx`, channels, ack channels). That is not redaction; those handles
cannot be serialized or replayed meaningfully.

---

## Error handling

- The taxonomy work introduces no new runtime errors.
- `CreatedAt` minting cannot fail (wall clock).
- Event id generation keeps the existing best-effort behavior: if event id
  generation fails after a turn has started, the event is still delivered and
  journaled with a zero `event.Header.ID`.
- Journal panics remain isolated like today's sink panics; a bad journal must not
  affect turn execution.

## Testing

- **Taxonomy exhaustiveness**: every concrete `event.Event` implements
  exactly one of Turn/Control/Notification; exactly the three terminal types
  implement `TerminalEvent`; the documented command sets implement
  `TurnCommand`/`ControlCommand`.
- **Event header stamping**: per-turn consumers receive concrete `event.Event`
  values with non-zero `SessionID`, event `ID`, `CreatedAt`, and `Sequence`; turn
  events share the same non-zero `TurnID` and causation id.
- **Journal parity**: the event delivered to `Journal.RecordEvent` has the same
  concrete type and header values as the event delivered to the per-turn channel.
- **Stamping helpers**: `stampEventHeader` and `stampCommandSequence` cover every
  concrete event/command type; adding a type fails the exhaustiveness test until
  the helper is updated.
- **Clock seams**: injected fixed clocks produce deterministic
  `command.Header.CreatedAt` and `event.Header.CreatedAt`.
- **Command journaling**: a fake `Journal` records `StartTurn` and gate commands;
  command headers carry ID/CreatedAt/CausationID, and semantic payloads such as
  `Input`, `ToolCallID`, approval scope, and `Answer` are present.
- **Command journal point**: invalid commands and rejected/busy `StartTurn`
  commands are still recorded before the actor replies or drops them.
- **Ordering**: command/event journal records receive monotonically increasing
  loop-local sequences, and fake journal callbacks are never concurrent for one
  loop instance.
- Run all with `-race`.

## Implementation order (for the plan)

1. Command taxonomy markers + `command.Header.CreatedAt` and session `newHeader`.
2. Event `Header`, event taxonomy markers, and event exhaustiveness tests.
3. Loop event-header stamping (`ID`, `CreatedAt`, `SessionID`, `TurnID`,
   `TurnIndex`, `CausationID`, `Sequence`) while keeping per-turn channels typed
   as `event.Event`.
4. `CallID` to `ToolCallID` symbol rename within the loop/runtime bounded context.
5. Replace `event.EventSink` with `loop.Journal`; record commands and events with
   no redaction/projection in Design A.
