# Design A — Session Observability & Taxonomy Foundation

**Date:** 2026-06-16
**Status:** Approved design, pending implementation plan
**Companion:** [Design B — Invoke Control Semantics & Autonomous Headless Mode](2026-06-16-autonomous-headless-mode-design.md) builds on this.

## Motivation

A headless consumer driving the agent from custom Go code (no TUI/CLI) needs a
complete, correlatable audit trail and a principled way to reason about which
events demand a response. Two gaps block that today:

1. **No typed categorisation of commands/events.** The old core loop (see
   `docs/old/2.md`) split commands into `turnCommand` vs `controlCommand`; the
   current code degraded this to a single `Command` interface plus a *structural*
   `GateCallID()` marker (named only in a test, `command/control_test.go:13`).
   Events have no categorisation at all. Consumers can't ask "is this event one I
   must answer?" without string/type matching.
2. **Asymmetric, incomplete observability.** Per-turn consumers
   (`session.Invoke`/`Stream`) receive *bare* events with no `SessionID`,
   `EventID`, or timestamp; only `EventSink`s get the `EventEnvelope`
   (`event/sink.go:16`). Sinks receive **events only** — never commands — so the
   journal records "what happened" but not "what was requested." Nothing carries
   a creation timestamp.

This design establishes the taxonomy and the observability/correlation
foundation. It is valuable independent of autonomous mode (Design B), which
depends on it.

## Scope

In: event taxonomy (Turn/Control/Notification + Terminal marker), command
taxonomy (Turn/Control), envelope-as-delivery on the per-turn channel,
`CreatedAt` on both sides with injectable clock seams, the `CallID` → `ToolCallID`
rename, and a unified `Sink` that journals **both** commands and events with
redaction.

Out: Invoke semantics, autonomous mode, notification *emission sites*, TUI
rendering — all in Design B. Attestation/fallback notification *sources* are
deferred (Design B documents why).

---

## A1. Event taxonomy — three categories + a terminal marker

Three sealed sub-interfaces of `event.Event` (`event/event.go`), classified by
**what the consumer must do**:

| Category | Marker | Requires a response? | Members |
|---|---|---|---|
| **Turn** | `TurnEvent` | no — work/progress | `TurnStarted`, `TokenDelta`, `ToolCallStarted`, `ToolCallCompleted`, `TurnDone`, `TurnFailed`, `TurnInterrupted` |
| **Control** | `ControlEvent` (`Event` + `GateCallID() uuid.UUID`) | **yes** — a control command back | `PermissionRequested`, `UserInputRequested` |
| **Notification** | `NotificationEvent` (`Event` + `Severity() NotificationSeverity` + `Message() string`) | no — diagnostic | `SessionStarted` (reclassified), plus emissions added in Design B |

- `TerminalEvent` is a **refinement marker within Turn**, implemented by exactly
  `TurnDone`/`TurnFailed`/`TurnInterrupted`, so `Invoke` (Design B) can write
  `case TerminalEvent: return ev` instead of enumerating three types.
- `NotificationSeverity` is a typed enum: `SeverityInfo` / `SeverityWarn` /
  `SeverityError` (no untyped magic numbers).
- `ControlEvent.GateCallID()` returns the tool-call id the event pertains to,
  pairing it with the control command that answers it (same id). It is the
  event-side mirror of the control command's `GateCallID()`.

**Symmetry with commands is intentionally partial:** events have a third
category (Notification) that commands lack, because diagnostics flow outward
only — you never *command* a notification inward.

**Exhaustiveness invariant (test-pinned).** A table test asserts every concrete
`event.Event` implements **exactly one** of `TurnEvent`/`ControlEvent`/
`NotificationEvent` (mirroring the existing `TestRedactableImplementations` in
`event/tool_test.go:353`). This makes adding a new event a compile-or-test
forcing function to classify it.

## A2. Command taxonomy — two categories

Re-formalise the old split as named, sealed marker interfaces in
`internal/agent/loop/command`:

- `TurnCommand` — `StartTurn` (work enqueued to the runner).
- `ControlCommand` — `Shutdown`, `Interrupt`, `ApproveToolCall`, `DenyToolCall`,
  `ProvideUserInput` (handled out-of-band / route to a parked gate).

The gate-routing subset keeps its `GateCallID()` method. No behavioural change —
this only names what is currently structural.

## A3. Envelope-as-delivery on the per-turn channel + `CreatedAt` + clock seams

**Change the per-turn channel element type from bare `event.Event` to
`event.EventEnvelope`.** Per-turn consumers then receive `SessionID`, `TurnID`,
`TurnIndex`, `EventID`, `CausationID`, `ToolCallID`, and a new `CreatedAt` —
uniform with sinks.

The payoff is **correlation**: the per-turn envelope and its sink/journal copy
share the **same `EventID` and `CreatedAt`**, so a headless caller can line up
"what my code saw (full fidelity)" with "what the audit log recorded (redacted)"
without the redacted log ever leaking a secret.

### Full-fidelity vs redacted — two envelopes, shared metadata

The loop already feeds two audiences from `listen()` (`loop.go:141-188`):

- **per-turn channel** → envelope wrapping the **full** event (trusted in-process
  caller needs real data),
- **sinks** → envelope wrapping the **redacted** event via `projectForSink`
  (`loop.go:147`; observability must never see secrets — CLAUDE.md "log security
  events, not secrets").

Both copies share one set of metadata (`EventID`, `CreatedAt`, `TurnID`,
`CausationID`, `ToolCallID`). **`CreatedAt` and `EventID` are captured by the
producer at the moment the event is constructed — its birth time — not at publish
or delivery.** Today `publish` mints `EventID` (`loop.go:152`) on the sink step,
which is too late and would make a `CreatedAt` added there a *delivery* time. The
clock/id seam is read where the event is *created*: for streaming events the
producer builds and emits on the same line (`runner.go:143`
`safeEmit(event.ToolCallStarted{...})`), so capture happens at construction; for
the **terminal** event — the one case where construction and delivery differ —
capture `CreatedAt`/`EventID` in `runTurn` when it is **finalised**, and have the
actor **reuse** those values when it delivers via `deliverAndClose` (it must never
re-read the clock at delivery). The loop then attaches the contextual fields it
owns (`SessionID`/`TurnID`/`TurnIndex`/`CausationID`, frozen by value at turn start
so reads from the turn goroutine are race-free), builds the full envelope for
`turnEvents`, and hands the **same** metadata with the event's `SinkProjection` to
sinks. `publish` only redacts and forwards — it never mints.

**Cost-reducer:** keep the loop's internal `emit func(event.Event)` signature
unchanged — enveloping happens *inside* the closure. So every event **producer**
(`turn.go`, `runner.go`, `gate.go`) is untouched; only the delivery type and
**consumers** change.

### `CreatedAt` on both sides

- `EventEnvelope` gains `CreatedAt time.Time` (UTC), stamped once at mint.
- Command `Header` (`command/header.go:8`) gains `CreatedAt time.Time` (UTC),
  stamped by the **sender** at mint. The session already mints headers in one
  place (`newCommandID`, `session/agent.go:67`) — promote it to a `newHeader()`
  returning `Header{ID, CreatedAt}`; all `command.Header{ID: id}` sites switch to
  it.

### Clock seams (testability)

Mirror the existing `idGen`/`newID` injection:

- `loop.Config` gains `now func() time.Time` (default
  `func() time.Time { return time.Now().UTC() }`) — stamps event `CreatedAt`.
- The session gains a `now func() time.Time` seam — stamps command `CreatedAt`.

Both default to UTC wall-clock and are overridden in tests for determinism.

### Resulting correlation surface

| | ID | Time | Causality |
|---|---|---|---|
| **Command** | `Header.ID` | `Header.CreatedAt` (new) | `Header.CausationID` |
| **Event** | `EventEnvelope.EventID` | `EventEnvelope.CreatedAt` (new) | `EventEnvelope.CausationID` (= causing command's `Header.ID`) |

A fully ordered, timestamped, causally-linked command+event trace.

### Blast radius (measured)

~10 non-test files; `loop.go` is the only concurrency-sensitive change (localised
to closures already present), the rest mechanical type-threading:

- Producer/core: `loop.go`, `command/start_turn.go` (channel field type).
- Session: `session/agent.go` (`Invoke` returns `EventEnvelope`; `Stream` →
  `StreamReader[event.EventEnvelope]`; terminal switch on `env.Event`).
- Consumers: `agents/{personal-assistant,coding}/agent.go`,
  `agents/coding/subagent_factory.go`, `tools/subagent.go`,
  `tui/{agent,commands,screen,transcript,interaction}.go` (signatures + `env.Event`
  for type switches; TUI gains `env.CreatedAt`/`SessionID`).
- ~13 test files (mechanical).

One shared `EventEnvelope` struct is reused for both projections (YAGNI — only the
loop builds them, correctly). A distinct `SinkEnvelope` type to make redaction
compile-enforced was considered and rejected for now.

## A4. `CallID` → `ToolCallID` rename

`CallID` is unambiguously the loop's internally-minted tool-call identifier
(`runner.go:88` "mints a CallID per call"; routed for gates + event correlation).
Rename the field `CallID` → `ToolCallID` across its bounded context (events,
commands, `EventEnvelope`, `runner.go`/`gate.go`/`loop.go`, `session/agent.go`,
and consumers `tools/askuser.go`, `tui/*`).

- **Plain field rename**, not a named type (decided; named type deferred).
- **Exclude** `internal/llm/openaiapi`'s `ToolCallID` (the OpenAI wire
  `tool_call_id` — a different, model-level concept) and all vendor code.
- **Mechanism:** an IDE / gopls **symbol** rename, which is scope-aware and will
  rename only references to *this* symbol — it cannot touch the unrelated
  `openaiapi` field, unlike a text find-replace. The `GateCallID()` *method* name
  stays as-is (the `Gate` prefix disambiguates; the symbol rename of the field
  does not alter it).

Three distinct ids exist; the rename sharpens the boundary:

| Identifier | Type | Layer |
|---|---|---|
| `content.ToolUseBlock.ID` / `ToolResultBlock.ToolUseID` | `string` | model message thread |
| `openaiapi` `ToolCallID` | `string` | provider wire adapter |
| loop `ToolCallID` (was `CallID`) | `uuid.UUID` | loop runtime (gate routing + correlation) |

## A5. Unified `Sink` — journal commands **and** events, with redaction

`EventSink` has **zero production implementers** — it is consumed only at
`loop.go:178`, with fakes in 3 test files. So the interface can be reshaped cheaply.

### Interface

Rename `EventSink` → `Sink` (it journals the whole session, both directions):

```go
type Sink interface {
    OnEvent(context.Context, EventEnvelope) // redacted event
    OnCommand(context.Context, Command)     // redacted command (Header embedded)
}
```

- The loop publishes each **received** command (redacted) to sinks at the actor's
  single serialization point, so the journal is one correctly **interleaved,
  ordered** command+event stream.
- **No `CommandEnvelope` wrapper.** A command **embeds** its metadata (`Header`:
  `ID`/`CausationID`/`CreatedAt`) because the **sender** authors it at construction.
  An event keeps its metadata in `EventEnvelope` (the bare event struct stays
  metadata-free) because one event needs **two projections** — full for the
  per-turn caller, redacted for sinks — that must **share** identical metadata. Both
  are stamped **at creation** (the producer captures `CreatedAt`/`EventID` when it
  builds the event — see A3), never at delivery; the embed-vs-wrap difference
  reflects *who authors the metadata* (sender vs loop), not *when*.

### Command redaction

Commands carry non-serializable and sensitive fields. `StartTurn`
(`command/start_turn.go:14`):

```go
type StartTurn struct {
    Header
    Ctx       context.Context    // strip (non-serializable)
    Input     []content.Block    // REDACT (user message content)
    Events    chan<- event.Event // strip
    Abandoned <-chan struct{}    // strip
    Ack       chan<- error       // strip
}
```

Plus `ProvideUserInput.Answer` (REDACT — user reply to AskUser). The redacted
command keeps its `Header` + shape, dropping channels/ctx and sensitive payload —
the same `Redactable`/`SinkProjection` pattern events use (`event/tool.go:16`).
`Approve`/`Deny`/`Interrupt`/`Shutdown` carry only ids/scope → no redaction needed.

---

## Error handling

- All new error modes are typed structs (CLAUDE.md). The taxonomy work introduces
  no new runtime errors; the clock/id seams already fail through existing typed
  paths.
- `CreatedAt` minting cannot fail (wall clock). Sink panics remain isolated as
  today (`loop.go:180-184` recover).

## Testing

- **Taxonomy exhaustiveness** table test: every `event.Event` implements exactly
  one of Turn/Control/Notification; exactly the three terminal types implement
  `TerminalEvent`; the documented control/turn command sets implement their
  markers.
- **Envelope delivery**: per-turn consumer receives `EventEnvelope` with non-zero
  `SessionID`/`EventID`/`CreatedAt`; per-turn and sink copies of the same event
  share `EventID` + `CreatedAt`; per-turn is full-fidelity while the sink copy is
  redacted (extend `sink_projection_test.go`).
- **Clock seams**: injected fixed clock → deterministic `CreatedAt` on both event
  envelopes and command headers.
- **Command journaling**: a fake `Sink` records `OnCommand` for `StartTurn`/gate
  commands; assert the recorded command is redacted (no `Input`/`Answer`, no
  channels) and carries its `Header` (ID/CreatedAt/CausationID).
- **Redaction security**: a `StartTurn` with secret `Input` and a
  `ProvideUserInput` with secret `Answer` never reach a `Sink` un-redacted
  (mirrors `TestSinkProjectionDropsSecrets`).
- Run all with `-race`.

## Implementation order (for the plan)

1. Taxonomy markers + exhaustiveness test (additive, no behaviour change).
2. `ToolCallID` symbol-rename (mechanical; do early to stabilise field names).
3. `CreatedAt` + clock seams on `EventEnvelope` and command `Header`.
4. Envelope-as-delivery on the per-turn channel (the `loop.go` emit refactor +
   consumer threading).
5. Unified `Sink` + command redaction + command journaling.
