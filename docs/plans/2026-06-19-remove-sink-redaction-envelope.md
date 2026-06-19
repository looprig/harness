# Cleanup — Remove Event Sink, Redaction & EventEnvelope Scaffolding

**Date:** 2026-06-19
**Status:** Deletion spec — pending implementation plan
**Supersedes:** the prior "Design A — Session Observability & Taxonomy Foundation"
content (this file). The old A1–A5 design is recorded under Background; only the
deletion below is live.

## Background — why this flipped from a design to a deletion

The original Design A proposed an event/command taxonomy (Turn / Control /
Notification + Terminal markers), envelope-as-delivery on the per-turn channel,
`CreatedAt` + clock seams, the `CallID → ToolCallID` rename, and a unified `Sink`
that journals commands **and** events with redaction. Main has since implemented a
leaner model that subsumes or supersedes nearly all of it:

- **Identity moved onto the event.** Every event embeds a `Header` carrying
  `SessionID`/`LoopID`/`TurnID`/`StepID`/`CausationID`/`ToolCallID` — *"without a
  transport-only envelope."* `EventEnvelope` is now redundant. (Supersedes A3.)
- **Delivery moved to a hub.** Full-fidelity events flow to a session fan-in via
  `eventPublisher.PublishEvent`; consumers attach through `Subscription`
  (`event/event.go`). The per-turn stream (`Invoke`/`Stream`) is a separate
  bare-`event.Event` channel. (Supersedes A3's envelope-as-delivery.)
- **Classification moved to mixins.** `Class` (Ephemeral/Enduring) × `Scope`
  (Session/Loop) + the `Reply` marker + `EndsTurn()` replaced the
  Turn/Control/Notification/Terminal markers. (A1/A2 **dropped** — add when needed.)
- **`CallID → ToolCallID`** landed on `Header`; the broader rename is superseded by
  the `Route` addressing migration (`command/route.go`). (A4 **done/superseded**.)
- **`CreatedAt` / clock seams** were never built. (A3's timestamp piece **deferred**
  — add when needed.)

That leaves exactly one piece of A5 still in the tree and now unwanted: the
**observability sink path** — `EventSink`, the `EventEnvelope` it transports, and
the **redaction** (`Redactable` / `SinkProjection`) that exists *only* to scrub
events before they reach a sink. It has **zero production implementers** (only test
fakes), so nothing consumes its output. (Redaction still *runs* on every published
event via `projectForSink`, but the result is handed only to the empty sink list —
so removing it is unobservable, and it drops `TurnDone`'s per-event deep copy from
the hot path.) This spec removes it wholesale.

**Security:** the sink path is the *only* surface redaction ever guarded, so deleting
it removes the leak surface entirely — dropping redaction introduces no new exposure,
and the loop-machine "redaction deferred / risk accepted" note becomes moot. If a
real journal consumer ever lands, the loop-machine follow-on re-introduces redaction
as a hub *subscriber*.

## Goal

Delete the sink / redaction / envelope scaffolding with **no observable change** to
event delivery. After this, delivery is exactly two paths, both already in place:

- producer → `publishHub` → **session fan-in** (`eventPublisher.PublishEvent`,
  full fidelity), and
- the **per-turn `turnEvents`** channel (`Invoke`/`Stream`, bare `event.Event`).

The loop's sink-only `SessionStarted` is deleted (no subscriber ever saw it, so this
is unobservable). Making `SessionStarted` *reliably observable* to subscribers is a
deliberate **follow-on feature**, not part of this deletion — see Out of scope.

## Invariants to preserve (must NOT regress)

- The hub fan-in and the per-turn stream keep delivering the **same events, full
  fidelity** (the sink path was the only redacted/enveloped consumer).
- Quiescence still works: `LoopIdle` / `TurnStarted` / `TurnFoldedInto` /
  `InputCancelled` still reach the hub for the activity model.
- Sink-panic isolation becomes irrelevant (no sinks). The hub already tolerates a
  publish failure by logging and continuing (`PublishEvent` error path).

## Deletion surface

### Production code

1. **`event/sink.go` — delete the file.** Removes `EventEnvelope` and `EventSink`.
2. **`event/tool.go`** — delete:
   - the `Redactable` interface (8–21),
   - the `UserInputRequestedSink` struct (47–59) + its `isEvent()` (87),
   - the three `SinkProjection()` methods: `PermissionRequested` (96–106),
     `UserInputRequested` (111–117), `ToolCallCompleted` (122–128).
   - Trim the SINK / redaction prose in the doc comments on the surviving events
     (`PermissionRequested` / `UserInputRequested` / `ToolCallStarted` /
     `ToolCallCompleted`). **Keep the events and their `CallID`/payload fields** —
     the per-turn stream / TUI still use them. (`tool` and `uuid` imports stay used.)
3. **`event/turn.go`** — delete `SinkProjection()` on `TokenDelta` (190–205) and
   `TurnDone` (216–243); **drop the now-unused `encoding/json` import**. Trim the
   redaction / `TODO(Open Items B)` SECURITY comments on `StepDone` (29–34),
   `TurnFailed` (144–150), and the `TurnDone.Message` "retained for the current sink
   projection" note (135–137).
4. **`event/event.go`** — remove the `Event`-interface SECURITY note about
   `Redactable` / the sink path (15–18); update the `Header` comment that defers
   "EventEnvelope replacement" (97–99) — the envelope is now gone.
5. **`event/doc.go`** — delete the `_ Event = UserInputRequestedSink{}` assertion
   (41) and the five `_ Redactable = …` compile-time assertions + their comment
   (46–54).
6. **`loop/config.go`** — delete the `Sinks []event.EventSink` field (13).
7. **`loop/loop.go`** — the only concurrency-sensitive edit (a removal inside the
   actor-goroutine closure; no new races, but `-race` must stay green):
   - Delete `projectForSink` (175–200).
   - Collapse `publish` (398–448) to the hub path: remove the projection call, the
     `EventEnvelope` build, the per-event `EventID` mint (`config.idGen()` at 412 —
     **keep `idGen` itself**; still used for `turnID` at 683 and the runner config at
     667), the `TurnIndex` switch (424–437), and the
     `for _, sink := range config.Sinks` fanout (438–447). `publish` then reduces to
     `publishHub` — fold call sites onto `publishHub` (or keep `publish` as a thin
     alias; the plan decides).
   - **Delete** the loop's startup `publish(event.SessionStarted{…})` (450): it is
     **sink-only** (`publishHub` *skips* `SessionStarted`, 390–392), so with sinks
     gone it would deliver nowhere — and no subscriber ever saw it regardless. It is
     also **per-loop**: every loop actor emits its own copy, so a multi-loop session
     produces N redundant sink-only copies of one session-scoped fact. Remove the
     now-dead `SessionStarted` skip-guard in `publishHub` (390–392) too. The session's
     own `SessionStarted` (`session.go:332`) is the **sole authoritative** one and is
     untouched; making it reliably reach subscribers is the **follow-on** (see Out of
     scope), not this pass.
   - Rewrite the "to sinks" / "redacted envelope" / "sink-only" / "already in sinks"
     prose in the doc comments on `publishHub`, `publish`, `emitTurn`,
     `deliverAndClose`, `emitLoopIdle`.
8. **`session/session.go`** — rewrite the `NewAgent` comment (301–306) that contrasts
   the loop's SINKS with the session's subscribers: sinks no longer exist, so the
   session's `NewAgent` emission is now the sole `SessionStarted` (its reliable
   delivery to late subscribers is the follow-on).

### Tests — a migration, not just a deletion

The fake `EventSink` (`captureSink`) collecting `[]EventEnvelope` is the **primary
event-observation mechanism** across the loop and session tests. The migration is:
*observe via a recording `eventPublisher`* (the same interface production uses via
the hub) *collecting `[]event.Event`* — replacing the fake-sink/envelope pattern.
The recorder captures exactly the set the hub sees in production (`SessionStarted` is
*not* on this path — it was sink-only and is deleted), so it is a faithful swap.

9. **Delete outright:**
   - `event/sink_test.go` (`TestEventEnvelopeFields` — envelope gone).
   - `loop/sink_projection_test.go` (redaction tests — gone).
10. **`event/tool_test.go`** — delete the redaction tests: `TestSinkProjectionDropsSecrets`,
    `TestSinkProjectionPreservesCallID`, the `TokenDelta` / `TurnDone` projection
    tests, `TestRedactableImplementations`, the no-mutation test, and every
    `UserInputRequestedSink` / `Redactable` reference. Keep any non-redaction coverage.
11. **Migrate loop tests** from `captureSink` / `blockUntilSink` / `[]EventEnvelope`
    to a recording publisher / `[]event.Event`:
    - `loop_test.go` — `captureSink`, `panicSink`, the `sinks ...event.EventSink`
      params on `newLoop` / `newLoopWithIDGen`, `hasTerminal`, and the
      `[]EventEnvelope` assertion tables. **`TestEventSinkPanicRecovered` becomes
      obsolete → delete** (no sink to panic; hub publish-error handling is separate
      and already logs).
    - `submit_decision_test.go` — `blockUntilSink` → a block-until-published helper.
    - `fold_test.go`, `cancel_queued_test.go` (`hasInputCancelled([]EventEnvelope)`
      → `[]event.Event`), `inbox_pop_idgen_test.go` (`Sinks:` wiring).
12. **`session/session_test.go`** — delete `recordingSink` / `cfgWithSink`; migrate
    those tests to the session `Subscription`.

### Field mapping for the test migration

When a test read an envelope field, read it from the event instead:

| Was (`EventEnvelope`) | Now |
|---|---|
| `env.Event` | the `event.Event` itself |
| `env.TurnID` / `env.CausationID` / `env.CallID` | `ev.EventHeader().TurnID` / `.CausationID` / `.ToolCallID` |
| `env.TurnIndex` | the event's own `TurnIndex` field |
| `env.EventID` | no equivalent — `Header.ID` wiring is deferred; drop the assertion |
| loop-emitted `SessionStarted` | gone — was sink-only and is deleted; reliable subscriber delivery is a follow-on |

**Projection trap.** Captured sink events were the *redacted projection*, so a test
that received `UserInputRequestedSink`, an `UnknownRequest`, an empty `ResultPreview`,
or a dropped `InputJSON` will now get the *full* `UserInputRequested` / real request /
populated preview / full chunk from the hub recorder. Flip any such assertion to the
full-fidelity value. (Today only the deleted redaction tests match projected shapes,
so the live blast radius is small — but verify during the migration.)

## Out of scope (explicitly not in this pass)

- **Reliable `SessionStarted` observability (follow-on feature).** The loop-machine
  design intends session-scoped events to be **delivered to every subscriber** — they
  bypass the per-loop `EventFilter` (`2026-06-16-loop-machine-design.md`, fan-out
  filter) — but the session emits `SessionStarted` at construction *before any
  subscriber attaches*, and the hub has no replay (`SubscribeEvents` only adds to the
  set — `hub.go:53`), so no subscriber sees it today. Recommended mechanism:
  **snapshot-on-subscribe** — the hub already owns session-lifecycle state
  (`h.state`, phase/active via `applyActivity`); have `SubscribeEvents` deliver the
  current session-lifecycle snapshot (a `SessionStarted`, plus the live phase if
  already `Active`/`Idle`) into the new subscription's egress buffer before live
  events flow. The session's `NewAgent` emission (`session.go:332`) becomes the
  latch-setter. This closes the design's "delivered to every subscriber" intent.
  Tracked separately.
- Re-homing redaction as a hub subscriber (the loop-machine journal follow-on owns it).
- Adding event/command taxonomy markers or `CreatedAt` (deferred — "add when needed").
- The `Route` addressing migration (separate, already in flight).

## Risks

- **`loop.go` publish collapse** is the only concurrency-sensitive change; it is a
  removal within a closure already confined to the actor goroutine, so it introduces
  no new races — but the `-race` suite must stay green.
- **Test migration is broad** (~7 loop/session test files) and is where a
  behavioural regression would hide. The recording-publisher swap must capture the
  same event set the hub sees; assert that the migrated tests still observe every
  event the old sink did, minus the deleted loop `SessionStarted`. Watch the
  **projection trap** above — full-fidelity events replace redacted shapes.

## Suggested sequence (the plan will refine)

1. Delete `event/sink.go`, the `Redactable`/`SinkProjection` methods, and
   `UserInputRequestedSink` (`tool.go`/`turn.go`/`doc.go`); fix comments in
   `event.go`. Package `event` compiles with no sink/redaction surface.
2. Delete `Config.Sinks` and collapse the `loop.go` publish path (incl. deleting the
   loop's sink-only `SessionStarted` emission + dead skip-guard); fix the
   `session.go:301–306` comment.
3. Migrate the loop/session tests to a recording publisher; delete the obsolete
   sink/redaction/envelope tests.
4. `CGO_ENABLED=0 go build -trimpath ./...`, `go test -race ./...`, `make secure`.
