# TUI Event Adoption Design

**Date:** 2026-06-18
**Status:** Draft
**Depends on:** `docs/plans/loop-machine-design.md` — event classes
(`Ephemeral`/`Enduring`), the per-step `StepDone` event carrying the finalized step
group (`AIMessage` + its `ToolResultMessage`s), the queued-input lifecycle
(`command.UserInput` and the `TurnStarted`/`TurnFoldedInto`/`InputCancelled` events),
producer-identity `Header` fields, and the publish/subscribe fan-in with `EventFilter`.
This spec **amends** that doc in two owned ways — see the *Loop/event amendment* sections
below — without editing it: it reclassifies the `ToolCall*` events as Ephemeral, and
replaces the `command.UserInput` `Disposition` reply with `Reply` events. Both apply when
this work lands.

> **Revision 2026-06-18 (loop-machine alignment).** Pins down what the original
> draft deferred ("nesting/styling is a follow-on") and folds in these decisions:
> (1) **rendering is now specified per `AIMessage`** — one `StepDone` renders as one
> dotted assistant entry, with explicit rules for thinking / text / tool-use / the
> empty-text-with-tools case; (2) **the TUI no longer owns an input queue** — the
> loop/session owns queueing, and the TUI surfaces queued messages and promotes them
> to committed user rows off the Enduring lifecycle events, keyed by `InputID`.
> `StepDone` stays a single atomic event (it is the freeze point — see Out of scope).
> (3) **`ToolCallStarted`/`ToolCallCompleted` are reclassified Ephemeral** — a cross-doc
> amendment this spec owns (see *Loop/event amendment*) — so live progress is uniformly
> Ephemeral and committed content is uniformly the Enduring `StepDone`.
> (4) **command replies become `Reply` events** — the `Disposition`/ack channel is
> dropped; the TUI submits fire-and-forget and reads outcomes (`InputQueued` Ephemeral,
> `TurnRejected` Enduring) from the one event stream (second amendment).

## Motivation

The loop-machine spec changes what the loop emits and guarantees: events are
classed `Ephemeral`/`Enduring`; each completed step emits an Enduring `StepDone`
carrying the step's finalized `*content.AIMessage` plus its `ToolResultMessage`s;
events carry producer identity (`LoopID`); the session exposes a publish/subscribe
fan-in with `EventFilter` instead of a per-turn stream; and the loop — not the TUI —
owns the queue of pending user input.

The current TUI predates all of that. The `StepDone`-committed transcript and the session
fan-in have since **landed** (see Current state), but the TUI still owns a FIFO of user
submissions it drains to start turns, still reads command replies as acks, and still treats
tool events as Enduring live signals. To finish becoming a **projection of the loop's
Enduring events** — user rows from `TurnStarted`/`TurnFoldedInto`, assistant rows from
`StepDone` — it must shed input queueing and the remaining ownership. This spec is the
display-layer counterpart to the loop-machine spec; it is kept separate because it lands
after the engine changes and has its own risk surface (rendering, scrollback, prompt
routing).

## Current state (verified against `main`)

The loop-machine merge already landed the **core** of this spec; the rest is what remains.

*Landed:*

- **StepDone-committed transcript.** `stepDone` snaps the transcript to the loop's
  finalized `StepDone.Messages` — one per-step group, multi-step turns never merged
  (`tui/transcript.go`). The "displayed != stored" problem is fixed: committed text is the
  loop's stored `AIMessage`, not the TUI's accumulation.
- **Provisional live + self-heal.** Chunks accumulate into a live segment; `StepDone`
  resets it, so dropped `TokenDelta`s vanish at the step boundary (`tui/transcript.go`).
- **Interrupt/partial commit.** A turn that ends with no `StepDone` for its in-flight step
  commits the provisional partial (`tui/transcript.go`).
- **Tool cards live→committed.** Live cards resolve in place; `StepDone` reuses the
  resolved card and falls back to the stored block (`tui/transcript.go:400-447`) — option
  (c), §3.3.
- **Session fan-in + the full transport switch (§1).** `AgentSession.SubscribeEvents(EventFilter)`
  + per-loop scoping (`session/agent.go`, `tui/agent.go`); `EventFilter{Ephemeral, Enduring
  LoopScope}` is the real shape. The TUI now holds **one session-lifetime subscription**
  (`subscribeCmd` once at `Init`, the continuous `subNext` reader) — the per-turn
  `StreamBlocks` reader and `Screen.queue` are gone (`tui/screen.go`, `tui/commands.go`).
- **`IsError` end-to-end (§3.3).** `content.ToolResultMessage` carries `IsError`; the loop
  threads it (`turn.go`) and the committed tool-card fallback reads it (`stepToolCard`), so
  ✓/✗ is authoritative from the `StepDone` group on both the reuse and fallback paths.
- **Tool events Ephemeral (Amendment 1).** `ToolCallStarted`/`ToolCallCompleted` embed
  `ephemeral` (`event/tool.go`); a subagent's tool chatter is muted under the default
  `{Ephemeral: primary-only}` filter alongside its tokens, surfacing only via `StepDone`.
- **Working-word / `● Done` (§3 rule 4).** An empty-text tool step renders a live
  working-word headline that commits to a bold `● Done` (`tui/anim.go`, `tui/render.go`,
  `commitStepAssistant`'s `doneHeadline`).
- **Replies are events (Amendment 2).** The loop publishes `event.InputQueued` (Ephemeral) /
  `event.TurnRejected` (Enduring) and a `Reply` marker interface replaces
  `command.Disposition`/`CancelResult`; submit/cancel are fire-and-forget; `SubagentResult`
  is never rejected (`loop.go`, `session/agent.go`, `command/`).
- **Input lifecycle from events (§6).** The TUI submits fire-and-forget (`agent.Submit`);
  user rows commit from `TurnStarted`/`TurnFoldedInto` `Message` (primary loop only); a
  queued affordance shows on `InputQueued`; `InputCancelled`/`TurnRejected` drop it
  (`tui/transcript.go`, `tui/screen.go`).
- **Per-loop gate-prompt clearing (§7).** A terminal event clears only the finishing loop's
  pending prompts (`tui/interaction.go` `ClearPromptsForLoop`).

*Still pending (out of scope for this spec):*

- **`TurnDone` still carries `Message`** (`event/turn.go`) — removed in a later loop
  phase; harmless, the TUI ignores it.
- **Per-turn channels for `Invoke`/`Stream`** — redundant with the fan-in but retained for
  the programmatic single-shot APIs; collapsing them onto the subscription is a tracked
  follow-on (not a behavioural dependency of this spec).

## Changes

### 1. Session-long subscription, not per-turn stream
Replace per-turn `Stream()` with one `SubscribeEvents(EventFilter)` held for the
TUI's lifetime. The TUI reads the merged session stream and routes by `LoopID`.
`EventFilter` selects per class × producer loop — `EventFilter{Ephemeral, Enduring
LoopScope}`, where `LoopScope` is `{All: true}` or `{Loops: {…}}`. For a single primary
loop: `{Ephemeral: LoopScope{All:true}, Enduring: LoopScope{All:true}}`; the multi-loop
default: `{Ephemeral: LoopScope{Loops:{primaryLoopID}}, Enduring: LoopScope{All:true}}` —
watch the primary live, see subagents' results without their token/tool firehose.
(Shorthand used below: `{Ephemeral: primary-only}`, `{Ephemeral: all}`, etc.)

### 2. Provisional via accumulator, committed via `StepDone`
While a step streams, fold `TokenDelta` chunks through
`internal/content/streamaccumulator` (`Thinking` + `Text`) into a *provisional* live
`AIMessage` and render it. On the Enduring `StepDone`, **discard the provisional
segment and commit `StepDone.Messages`** — the loop's authoritative finalized group
(the `AIMessage` plus its `ToolResultMessage`s) — as one immutable dotted entry,
rendered by the rules in §3. Consequences:

- The committed transcript is the loop's stored message, byte-for-byte (closes
  "displayed != stored").
- Dropped Ephemeral events self-heal at the step boundary — `TokenDelta` and, per the
  amendment below, `ToolCallStarted`/`ToolCallCompleted` (the fan-in Ephemeral-drop
  policy depends on this).
- The accumulator is genuinely shared: the loop uses it for step folding; the TUI
  uses `Thinking`/`Text` for the provisional live `AIMessage`. A `TokenDelta` carrying a
  `ToolUseChunk` is **skipped** on the live path (the TUI does **not** fold `ToolUses` —
  live tool cards come from `ToolCallStarted`/`Completed`, §4), so this is honest shared
  use of the text/thinking folders, not a claim that the TUI reassembles tool-use deltas.

**`StepDone` is the atomic freeze point.** A `StepDone` fires only after the step's
tools have run, so it carries the *complete* group; the TUI freezes it into immutable
scrollback in one append. The window between the `AIMessage` materializing (stream
EOF) and `StepDone` — the tool-execution phase — is covered live by the
`ToolCallStarted`→`ToolCallCompleted` signals (now Ephemeral — see the amendment
below) (§4). We deliberately do **not** split `StepDone` into request/result events
(see Out of scope).

### 3. Per-`AIMessage` rendering rules
**One `StepDone` = one assistant entry.** A multi-step turn emits multiple `StepDone`s
and therefore multiple entries — no collapse. The boundary-detection state machine
(the old `TokenDelta`-after-batch / `ToolCallStarted` all-terminal rules) is **gone**
for committed content: the loop already split the steps. The same rules render the
*provisional* live `AIMessage` so the live tail and the committed entry look identical.

Rendering one `AIMessage` (its blocks: `ThinkingBlock` / `TextBlock` / `ToolUseBlock`):

1. **Thinking present** → the existing dim thinking style (`│ ` rail expanded;
   `thinking · N lines · ctrl+t` collapsed). Unchanged (`renderThinking`).
2. **Text present** → dot bullet, markdown: `● <text>` (`renderMD`). Unchanged.
3. **Tool-use blocks present** → render each as an indented child card under the entry,
   in block order (**built** — `stepDone`/`stepToolCard`, `tui/transcript.go`; the fallback
   status now reads `ToolResultMessage.IsError`, below).
   One card per `AIMessage` `ToolUseBlock`. The header is `ToolName` (`ToolUseBlock.Name`)
   + a redacted one-line args summary sourced by **reusing the resolved live card**: at
   `StepDone` the i-th `ToolUseBlock` is matched to the i-th in-flight live card (guarded
   by tool name), and that card's already-redacted `ToolCallStarted.Summary` + capped
   preview commit. The TUI can't recompute the summary itself (it's the tool's per-tool
   `tool.Auditable.AuditSummary`, owned by the runner) and the raw `ToolUseBlock.Input` is
   unsuitable to show (a `WriteFile` arg *is* the whole file). **Fallback** when no live
   card streamed (a dropped `ToolCallStarted`, or a subagent-loop step the TUI only sees
   finalized): commit `ToolName` + the result, **no summary line** — body from the
   `StepDone` group's `ToolResultMessage` (correlated `ToolUseBlock.ID ==
   ToolResultMessage.ToolUseID`), ✓/✗ from `ToolResultMessage.IsError` (now carried on the
   message and read by the fallback — `ToolResultMessage` gained an `IsError` field, so the
   fallback no longer hardcodes `✓`). So the essentials (name, result, status) are always
   from the Enduring group; only
   the cosmetic summary depends on the live card. **Live source:** `ToolCallStarted`/`Completed` (§4).
4. **Text empty but tool-use present** → instead of a bare `●`, render a **bold
   headline** beside the dot, with the tool cards as its children. The headline depends
   on phase:
   - **Live (provisional, pre-`StepDone`):** a **"working" synonym** (e.g.
     `● **Crunching**`) from a small list beside `spinnerFrames` in `tui/anim.go` — a
     live activity indicator. Because it never has to survive into the committed entry,
     it need not be deterministic (it may even rotate while the step runs).
   - **Committed (`StepDone`):** a static **`● Done`**. The work is finished; each tool
     card's ✓/✗ glyph carries the real per-call outcome.

   So the synonym is purely a live affordance and the live→committed swap always lands on
   "Done" — no step-id-pinned word and no cross-swap flicker to reason about. This
   replaces the bare-bullet branch in `renderAssistant`/`renderLiveAssistant`
   (`tui/render.go`).

Empty parts are omitted; ordering is thinking → narration/working-word → cards, as in
the current `renderAssistant`. A thinking-only `AIMessage` (no text, no tool-use) renders
just the thinking rail with no bullet — intentional, not a missing-bullet bug.

### 4. Tool cards — live vs committed
The split is uniform with the text path: **live = Ephemeral, committed = Enduring.**
`ToolCallStarted`/`ToolCallCompleted` (reclassified Ephemeral by the amendment below)
are the **live** running/done signal on the provisional tail only — spinner glyph and,
on completion, the runner's capped `ResultPreview` (full-fidelity on the TUI stream;
redaction is the sink projection's concern, not the display's). The **committed** card
I/O comes from the finalized `AIMessage`'s `ToolUseBlock`s and the following
`ToolResultMessage`s in the same Enduring `StepDone` group — **not** from the live preview.
The lone exception is the cosmetic one-line **summary**, reused from the resolved live
`ToolCallStarted` (and dropped to name + result on the dropped/subagent fallback — §3.3).
A long-running tool still shows a spinner while it runs, and the frozen entry shows the
authoritative name + result; the curated summary stands in for the raw args, which are
never dumped. A dropped live signal costs the transient spinner and **only** the committed
card's one-line summary — the card self-heals to name + result + status (✓/✗ from
`ToolResultMessage.IsError`) at `StepDone`, so only the cosmetic summary is lost. Note the committed body is the
**full** `ToolResultMessage` content where the live body was the runner's **capped**
`ResultPreview`, so `ctrl+t` expand on a committed card can reveal more than the live one
did; the collapsed K-line fold still applies.

### 5. Loop-attributed transcript
Add a `LoopID` tag to the live segment and committed entries and key the live segment
by `LoopID` (a provisional `AIMessage` per loop). `renderEntry` labels/nests entries
under their loop. For a single primary loop this is a no-op tag; for subagents it is
what prevents interleaving one loop's narration into another's. Deep subagent nesting
is a follow-on; v1 renders the primary loop inline and subagents as compact attributed
lines.

### 6. Input handling — fire-and-forget submit, outcomes are `Reply` events
Remove `Screen.queue [][]content.Block` and the TUI's self-drain turn-start. The TUI
**submits `command.UserInput` fire-and-forget** — a plain enqueue whose only failure is a
transport error (the loop is **already gone**, so no event follows) — and learns every
outcome **from the event stream**, not from a `Disposition` ack (which this spec drops;
see the amendment below). Each outcome is a `Reply` event correlated by
`Header.CausationID == InputID` (the submit's id):

- `event.InputQueued{InputID}` (**Ephemeral**) → render the message in a **queued** state
  in a pending area below the live tail, keyed by `InputID` (accepted into the loop's
  inbox, not yet a turn). Ephemeral: if dropped, the resolution event below still arrives.
- `event.TurnRejected{InputID, Reason}` (**Enduring**) → surface the rejection and drop
  any queued affordance. Interactive input is *queueable*, so it is rejected only for
  queue-full or for a loop that is reachable-but-stopping (shutdown) — never "busy."
  Enduring so a rejected message never silently vanishes. (A loop already fully gone is
  the transport-error case above, not a `TurnRejected`.)
- `event.TurnStarted{InputID, Message}` (Enduring) → **commit a normal user row** from
  `Message`, keyed by `InputID`, at the end of the transcript (turn start) — replacing
  the queued affordance if one was shown, else appended directly.
- `event.TurnFoldedInto{InputID, Message}` (Enduring) → commit a normal user row from
  `Message`, keyed by `InputID`, **appended at the current committed tail** — the fold
  happens between steps (after the last `StepDone`, before the continuation), so this is a
  plain append, not an insert into already-flushed scrollback.
- `event.InputCancelled{InputID}` (Enduring) → drop the queued affordance (user
  `CancelQueuedInput`, or shutdown).

**User rows are authoritative from the event's `Message`**, keyed by `InputID` — so
displayed == stored for user rows too, mirroring the assistant side. The TUI holds only a
small `InputID`-keyed set of *queued* messages for display — no FIFO it drains, no
`DisplayIndex` bookkeeping — and reads **one** stream for both acceptance and resolution.

### 7. Per-loop terminal handling + gate-prompt routing
Scope terminal handling (`TurnDone`/`TurnFailed`/`TurnInterrupted`, all Enduring) to the
finishing `LoopID`, and **never** let it clear queued input (only
`TurnStarted`/`TurnFoldedInto`/`InputCancelled` resolve queued `InputID`s, §6).

- **`TurnDone` (clean end)** is lifecycle-only: the last `StepDone` already committed and
  reset the provisional, so there is nothing to commit — just clear the loop's live
  state.
- **`TurnFailed` / `TurnInterrupted`** can arrive mid-step, where the loop discards the
  in-progress step (no `StepDone` for it). Here the TUI keeps what the user was watching,
  exactly as today: **commit the provisional as a display-only partial** (any still-running
  card → `ToolCancelled`), then append the tombstone — a `RoleInterrupted` marker, or a
  `RoleError` row carrying `TurnFailed.Err`. This is the *sole* case where a committed
  entry comes from the accumulator instead of `StepDone`, and it is justified by *Display
  ≠ model context* (tui-tool-use-design §5): the loop drops the partial step from context,
  but the transcript still shows what streamed.

Partition the **gate-prompt** queue (permission / AskUser prompts in
`tui/interaction.go`, keyed by `CallID` — a *different* queue from the removed input
queue) by loop, so one loop's terminal event no longer `ClearPrompts` for a sibling.
Gate prompts already carry their route (`SessionID`/`LoopID`/`TurnID`/`ToolCallID`); the
TUI echoes that route back on approve/deny/answer.

## Loop/event amendment — reclassify `ToolCall*` as Ephemeral (owned here)

This spec **owns and drives** one cross-doc change to the loop-machine event taxonomy
(loop-machine-design.md, *"Class is semantic"*): **reclassify `ToolCallStarted` and
`ToolCallCompleted` from Enduring to Ephemeral.** The loop-machine doc is not edited;
the amendment lives here and is applied when this work lands (the `ephemeral` mixin
replaces `enduring` on those two event structs).

**Why.** The loop-machine criterion for the class is its own test — *"is this
reconstructable later?"* — deltas Ephemeral, state transitions Enduring. Both tool
events are **fully reconstructable from the Enduring `StepDone` group**: name/args from
the `AIMessage`'s `ToolUseBlock`s, `IsError` and result body from the
`ToolResultMessage`s, correlation by `ToolUseBlock.ID ↔ ToolResultMessage.ToolUseID`.
(The one thing NOT reconstructable from `StepDone` is the runner's redacted display
`Summary`; the committed card reuses it from the live event and degrades to name + result
on a drop — cosmetic only, §3.3. Everything semantic — name, args, result, status — is in
the group.)
They are non-blocking, within-step **live progress** — the same category as
`TokenDelta` — so by the doc's own test they are Ephemeral; a dropped one self-heals at
the step boundary.

**The line that stays Enduring is the gate, not "tool-ness."**
`PermissionRequested`/`UserInputRequested` remain Enduring because the loop **blocks on
their response** and they are **not reconstructable** — no later event replays a
permission request, so dropping one would deadlock the step and hide the prompt. So the
boundary is: **Enduring** = authoritative transitions/payloads (`StepDone`, turn/session
lifecycle) **plus gate events** (block + irreconstructable); **Ephemeral** = non-blocking
live progress reconstructable from `StepDone` (`TokenDelta`, `ToolCallStarted`,
`ToolCallCompleted`).

**One delivery class, not three.** `Class` stays the binary contract (Ephemeral →
drop on overflow; Enduring → fail the subscription). We do **not** add a "control"
class — it would carry no distinct *delivery* semantic. The content-delta vs
tool-progress distinction is real but is carried by the event *type*, not the class.

**Consequences (all favourable for this spec):**
- §4's live/committed split becomes uniform: **live = Ephemeral (`TokenDelta` +
  `ToolCall*`), committed = Enduring (`StepDone`)**.
- The default subagent filter `{Ephemeral: primary-only}` now mutes subagents' tool
  chatter as well as their tokens — fully honouring "results, not firehose."
- Redaction shrinks to one content-bearing event (`StepDone`): the second copy of tool
  output (`ToolCallCompleted.ResultPreview`) is no longer on an Enduring, sink-bound
  path.
- Nothing's correctness depends on tool-event delivery: once the TUI commits from
  `StepDone`, the old reconstruction state machine — and the runner's "all
  `ToolCallStarted` before any execute" ordering guarantee (tui-tool-use-design §4) —
  is gone, so the tool events are pure live decoration and safe to drop.

**Trade-off accepted.** A live cross-loop "who's running which tool" view can no longer
be had cheaply via `{Ephemeral: none, Enduring: all}`; off-firehose watchers see a
loop's tools only at `StepDone` granularity. That live multi-agent tool view is the
deferred deep-subagent concern (see Out of scope); `StepDone`-granularity is accepted as
the default.

## Loop/event amendment — replies are events; drop `Disposition` (owned here)

This spec **owns and drives** a second cross-doc change to the loop-machine command/event
model: **replace the `command.UserInput` reply (`Ack chan<- Disposition`) with published
`Reply` events.** As before, loop-machine-design.md is not edited; the amendment applies
when this work lands.

**The change.**
- **Drop the `Disposition` sealed interface and the `Ack chan<- Disposition`.** Submit
  becomes fire-and-forget (enqueue the command; a transport error only if the loop is
  gone).
- **Publish two new events**, each with `Header.CausationID` set to the submit's
  `InputID`:
  - `event.InputQueued{InputID}` — **Ephemeral** (self-heals: the authoritative
    resolution event still follows).
  - `event.TurnRejected{InputID, Reason}` — **Enduring** (irreconstructable, and a
    rejected user message must never silently vanish — the same gate-rule as the first
    amendment). It gains an `InputID` it did not carry as a point-to-point reply.

  `event.TurnStarted`/`TurnFoldedInto`/`InputCancelled` already exist as Enduring events
  and are now the *only* form of those outcomes.
- **Add a `Reply` marker interface** over the existing `CausationID` convention:

  ```go
  // Reply is an event that is the direct outcome of a command, delivered on the
  // normal fan-in (classed Ephemeral/Enduring like any other event — NOT a
  // point-to-point channel). It is the typed replacement for Disposition.
  type Reply interface {
      Event
      isReply()            // seals the set
      ReplyTo() uuid.UUID  // == Header.CausationID: the command this answers
  }
  ```

  Implemented by `TurnStarted`, `InputQueued`, `TurnRejected`, `TurnFoldedInto`,
  `InputCancelled`. It changes no delivery semantics; it lets an issuer recognise "the
  answer to *my* command" (`ev.ReplyTo() == myCmdID`) and lets others ignore it.
- **`CancelQueuedInput` loses its `CancelResult` ack too**, for the same reason: its
  success outcome is the Enduring `event.InputCancelled` (keyed by the cancelled
  `InputID`), and "already started" is observable because the TUI has already seen
  `event.TurnStarted`/`TurnFoldedInto` for that `InputID`. So no command in the
  submit/cancel family (`UserInput`, `SubagentResult`, `CancelQueuedInput`) carries an
  ack — every outcome is a `Reply` event. (`Shutdown`'s error ack is a separate lifecycle
  concern, unchanged.)

**`SubagentResult` is never rejected.** A subagent hand-back must reliably start or fold a
turn — losing it loses the subagent's work — so it **bypasses the user-input queue cap**
and is never answered with `TurnRejected`. (Safe because subagent fan-out is bounded, so
hand-backs can't grow the inbox without bound the way unthrottled user input could.) This
removes the existing `cancelExpectTurn`-after-`TurnRejected`-for-`SubagentResult` case
from the loop-machine design.

**Why it is safe — quiescence is untouched.** For user input, quiescence is already
event-driven (`TurnStarted` adds `{loop}`, `LoopIdle` removes it); the disposition never
fed it. With `SubagentResult` never rejected, **`TurnRejected` only ever answers a user
`UserInput`**, where there is **no `{wake}` token** to release — so making it an event
adds **zero** rows to the wake-release table. The session's `expectTurn`-at-spawn stays a
synchronous session method (it was never a disposition).

**Consequences (favourable):**
- §6 needs no session API to "surface a return value" — the TUI *and* the session's own
  quiescence read one stream.
- The confusing dual `TurnStarted` (a `Disposition.TurnStarted` *and* an
  `event.TurnStarted`) collapses to the single `event.TurnStarted`, which *is* the reply.

**Trade-off accepted.** `Reply` events are broadcast — but only loops that take *user*
input emit `InputQueued`/`TurnRejected` (effectively just the primary loop; subagents take
`SubagentResult`, never user input), so the added fan-out is small. `ReplyTo()`/
`CausationID` still let any subscriber pick out its own command's outcome, and Ephemeral
`InputQueued` is dropped under the primary-only filter anyway.

## What this resolves

- **Displayed != stored (assistant):** committed transcript is `StepDone.Messages`.
- **Displayed != stored (user):** committed user rows are `TurnStarted`/
  `TurnFoldedInto` `Message`, matched by `InputID`.
- **Shared-accumulator, honestly:** the TUI folds `Thinking`/`Text` via
  `streamaccumulator` for the provisional `AIMessage`; tool cards are sourced as in §4.
- **Uniform delivery model:** live = Ephemeral (`TokenDelta` + `ToolCall*`), committed =
  Enduring (`StepDone`) — no Enduring/Ephemeral seam between narration and tool spinners.
- **Multi-step turns:** each step's `StepDone` renders as its own assistant entry; no
  collapse, and no TUI-side boundary state machine.
- **Empty-text tool steps:** a live bold working-word headline that commits to `● Done`
  — instead of a bare `●`.
- **Multi-loop attribution:** `LoopID` on events + loop-keyed reducer.
- **Queue ownership:** the loop owns queueing; the TUI surfaces it without owning a
  FIFO it drains.
- **Single consumption channel:** submit is fire-and-forget; acceptance (`InputQueued`),
  rejection (`TurnRejected`), and resolution (`TurnStarted`/`TurnFoldedInto`/
  `InputCancelled`) all arrive as `Reply` events on the one subscription — no
  `Disposition` ack to plumb.

## Out of scope / sequencing

- Lands after the loop-machine spec ships `StepDone`, the classes, the fan-in, the
  filter, the queued-input lifecycle, and producer-identity headers.
- **id-normalization sequencing — names track loop-machine for now.** This spec uses the current
  loop-machine vocabulary (`Header.CausationID`, gate `CallID`/`ToolCallID`, `InputID`).
  The separate, unstarted `docs/plans/2026-06-18-id-normalization-design.md` will later
  rename these (`CausationID` to `Cause.CommandID`, gate `CallID` to `GateID`, `InputID`
  to `Cause.CommandID`). When those renames land they are **purely cosmetic for this
  spec** — there is no behavioural dependency on them. In particular the committed tool
  card needs no id bridge to the live events: it reuses the live card **by position**
  (i-th block ↔ i-th live card, guarded by tool name) and correlates the fallback result
  by the content-level `ToolUseID` (`ToolUseBlock.ID == ToolResultMessage.ToolUseID`, which
  already exists). So it does **not** need `ToolUseID` added to the live tool events. (The
  id-normalization doc also still shows `CancelQueuedInput` keeping a `CancelResult` ack —
  superseded by amendment 2's "no ack.")
- **Deferred: splitting `StepDone`** into a request event (`StepResponded{AIMessage}`
  at stream EOF) + a result event (`StepResolved{ToolResultMessages}` after tools).
  With `ToolCall*` now Ephemeral (amendment above), a subscriber watching a subagent
  *off the Ephemeral firehose* (`{Ephemeral: primary-only}`) sees **no** live progress
  for it — neither tokens nor tool spinners — only its completed `StepDone`s. The split
  would give such a watcher an Enduring "replied, tools running" checkpoint without the
  firehose. But it does not move the TUI's freeze point (still post-tools), and the
  primary loop already has the gap covered live. It is a loop-machine-spec change (event
  taxonomy + emit timing + tests) motivated only by the deferred deep-subagent live
  view; revisit there if/when that ships. `StepDone`-granularity is the accepted default
  until then.
- Deep subagent transcript nesting/styling is a follow-on; v1 targets a single primary
  loop inline + compact attributed subagent lines.
- Native-scrollback flushing of committed entries now happens on `StepDone` commit
  (landed); the remaining check is no double-flush across the provisional → committed swap.
- The working-word list is small and fixed; localization/configurability is out of
  scope.

## Testing

- A streamed step renders provisional text, then the committed entry equals
  `StepDone.Messages` (not the accumulated chunks); a deliberately dropped `TokenDelta`
  does not change the committed entry.
- One `StepDone` → exactly one assistant entry; a multi-step turn renders
  `UserMessage, AIMessage, ToolResultMessage, AIMessage` as separate entries with no
  collapse.
- The committed tool card's body equals its `ToolResultMessage` (✓/✗ from
  `ToolResultMessage.IsError`), correlated by `ToolUseBlock.ID ==
  ToolResultMessage.ToolUseID`; its header summary is **reused from the resolved live card**
  (matched by position + tool name) and **falls back to `ToolName` + result, no summary**
  when no live card streamed (dropped/subagent). A `WriteFile`-style tool shows its summary
  (path) when live, never the raw file-body args; the live card body comes from
  `ToolCallCompleted`.
- An empty-text + tool-use step renders a bold **working-word** headline + child cards
  while live (provisional), and commits to a **`● Done`** headline at `StepDone`; the
  per-card ✓/✗ reflects each tool's outcome.
- The TUI subscribes once per session; multiple turns reuse the one subscription.
- With `{Ephemeral: primary-only}`, a subagent's `TokenDelta` **and** its tool-lifecycle
  events (`ToolCallStarted`/`ToolCallCompleted`, now Ephemeral per amendment 1) never
  render, but its Enduring `StepDone`/gate events do, attributed by `LoopID` — so the
  subagent's tools surface at `StepDone` granularity, not as a live per-call view.
- **Input lifecycle (events, no ack):** `event.InputQueued` (Ephemeral) shows the message
  as queued keyed by `InputID`; a following `event.TurnStarted`/`TurnFoldedInto` with the
  same `CausationID`/`InputID` promotes it to a committed user row **exactly once** (at
  end-of-transcript vs the fold point respectively); `event.InputCancelled` drops it; an
  `event.TurnRejected` (Enduring) surfaces a rejection without losing the message; a
  terminal event does **not** drop a queued message. All are read from the one
  subscription — there is no `Disposition` ack.
- One loop's terminal event does not clear another loop's pending gate prompt.
- **Interrupt / failure mid-step:** a `TurnInterrupted` (or `TurnFailed`) with a
  non-empty provisional and no `StepDone` for the in-progress step commits the partial as
  a display-only entry (running cards → `ToolCancelled`) + a tombstone / `RoleError`;
  completed earlier steps stay committed from their `StepDone`s.
- Run with `go test -race ./...`.
