# TUI Tool-Use Rendering — Design

Date: 2026-06-14 · Status: approved (brainstorm)

> **Revision 2026-06-14 (review pass 1)** resolves a review: (1) **back-to-back tool
> batches no longer collapse** — `ToolCallStarted` commits the live segment when its
> existing calls are all terminal, and the runner now **emits all `ToolCallStarted` for
> a batch before executing** (so the all-terminal rule is race-free; tools-design §2d
> amended); (2) **`/verbose` removed in favour of a `Ctrl+T` key**, and expansion
> redefined as "all lines of the runner-capped preview" (no fictional 100-line/"full"
> view the ~20-line event can't supply) — the runner marks truncation.
>
> **Revision 2026-06-14 (review pass 2)** resolves a review: (1) **`TurnDone` is now
> unambiguous** — `live` is authoritative, `ev.Message` is a fallback only when `live`
> is empty; never both (no duplicated final text). Replaces the current
> `tui/screen.go:111` handler; added a "no duplication" test (§2, §6); (2) the runner
> obligations the TUI depends on (`ResultPreview` cap/redaction; all `ToolCallStarted`
> before any `ToolCallCompleted`) are now in **tools-design §5e** + the impl plan, not
> only implied here.

## Scope

Render an agentic turn's tool calls in the TUI transcript as **children of the
assistant message that triggered them**. An agentic turn is `text → tool batch →
text → tool batch → … → final text`; each assistant text segment becomes a row with
its tool calls nested directly beneath, in execution order, each showing a status
glyph and a collapsed result preview.

This is the rendering work deferred by the tools design
(`docs/plans/2026-06-14-tools-design.md` §5d, "contract only"). It builds on the
in-flight TUI (`docs/plans/2026-06-13-tui-design.md`, now implemented in `tui/`).
**Permission-prompt and AskUser-prompt rendering are out of scope** (a separate doc).

### Decisions locked in (this brainstorm)

| Decision | Choice |
|---|---|
| Nesting structure | **Per-segment, chronological** — each assistant text segment is a row; its tool calls nest beneath it in order |
| Data model | Event-reconstruction: nested `ToolCallView` on the assistant `DisplayMessage`, built by a live-segment state machine from the event stream (not by delivering intermediate `AIMessage`s) |
| Result display | **Collapsed preview** — capped lines + a global expand toggle |
| Expand UX | **Global `Ctrl+T` toggle** — collapsed (first K lines) ⇄ all lines of the (runner-capped) preview; per-card cursor + Enter deferred |
| Result data | Amend `event.ToolCallCompleted` to carry a capped `ResultPreview` (with a truncation marker when capped); redacted on the sink path |

---

## Key facts (why reconstruction)

- The TUI is **flat**: `DisplayMessage{Role, Blocks}` rendered one row each, with text
  streamed into a single buffer and one `TurnDone` per turn carrying the **final**
  message (`tui/message.go`, `tui/render.go`, `tui/screen.go:101` `handleEvent`).
- The agentic loop emits **one `TurnDone` at turn end** (final assistant message only).
  Intermediate tool-bearing assistant messages are **not** delivered as messages — the
  TUI sees them only via the event stream: `TokenDelta` (narration text; `ToolUseChunk`
  is skipped), `ToolCallStarted{CallID, ToolName, Summary}`, `ToolCallCompleted{CallID,
  IsError, …}` (tools design §5b).
- So the TUI **reconstructs** the nesting from the ordered stream, correlating tool
  start↔completion by **`CallID`**. It never needs the provider `ToolUseBlock.ID`.

---

## §1 — Display model

`tui/message.go` — the assistant `DisplayMessage` gains nested children:

```go
type ToolStatus uint8
const (
	ToolRunning ToolStatus = iota
	ToolOK
	ToolError
	ToolCancelled // turn interrupted while the call was still running
)

// ToolCallView is one tool call rendered as a child of its assistant segment.
type ToolCallView struct {
	CallID   uuid.UUID
	ToolName string   // ToolCallStarted.ToolName
	Summary  string   // ToolCallStarted.Summary (already redacted, one line)
	Status   ToolStatus
	Result   []string // capped preview lines from ToolCallCompleted; nil while running
}

type DisplayMessage struct {
	Role      DisplayRole
	Blocks    []content.Block
	ToolCalls []ToolCallView // children; populated only for RoleAssistant segments
}
```

`Screen` (`tui/screen.go`) replaces the `stream string` field with a live segment and
adds the global expand flag:

```go
type liveSegment struct {
	text  string
	calls []ToolCallView
}
// Screen.live        liveSegment // replaces Screen.stream
// Screen.expandTools bool        // Ctrl+T toggle; false = collapsed previews
```

No per-card `Expanded` field — the global `expandTools` covers v1 (YAGNI).

---

## §2 — Event-reconstruction state machine

`handleEvent` (`tui/screen.go:101`) drives `live`:

| Event | Action |
|---|---|
| `TokenDelta` (`*content.TextChunk`) | If `live.calls` is non-empty (a batch ran and new narration is starting), **commit** `live` as `RoleAssistant{Blocks:[TextBlock(live.text)], ToolCalls:live.calls}` and reset `live`. Then `live.text += chunk.Text`. (Other chunk types still skipped.) |
| `ToolCallStarted` | **If `live.calls` is non-empty and every existing call is terminal** (a batch finished and a new one starts with *no narration between* — `tool batch → tool batch`), **commit** `live` and reset first. Then append `ToolCallView{CallID, ToolName, Summary, Status:ToolRunning}` to `live.calls`. The text streamed so far is this batch's narration. |
| `ToolCallCompleted` | Find the view by `CallID` in `live.calls` (fallback: most recent committed segment); set `Status` (`ToolOK`/`ToolError` from `IsError`) and `Result` from the capped preview. |
| `TurnDone` | **`live` is authoritative** — commit `live` (its streamed `text` + any `calls`) as the final segment; **do not also append `ev.Message.Blocks`** (that would duplicate the streamed text). `ev.Message` is a **fallback only**: if `live` is empty (`text==""` and no `calls` — e.g. a non-streaming provider), commit `ev.Message.Blocks` instead (guard nil `Message`). Either way exactly **one** final assistant segment is produced. Clear `live`. **This replaces the current `tui/screen.go:111` handler** (which appends `ev.Message.Blocks` + clears `stream`). |
| `TurnFailed` | Commit `live` if it has text or calls (keep completed tool work visible), then append `RoleError` (`ev.Err`). |
| `TurnInterrupted` | Commit `live` (partial text + calls; any still-`ToolRunning` card → `ToolCancelled`), then the `RoleInterrupted` tombstone. |

**Two boundary signals, both deterministic:** a new segment starts either when
narration text arrives after a batch (the `TokenDelta` rule) **or** when a new batch
starts with no narration (the `ToolCallStarted` all-terminal rule). Together they
correctly split `text→tool→tool→text→done` *and* `text→tool→tool(no text)→text→done`.

**Runner-ordering requirement (tools-design amendment):** the all-terminal rule is
only race-free if, within one batch, every `ToolCallStarted` precedes every
`ToolCallCompleted`. With parallel goroutines each emitting their own start, a fast
call could otherwise complete before a sibling's start and falsely trip "all
terminal" mid-batch. So **the runner emits all `ToolCallStarted` for the approved
batch *before* executing any call** (tools-design §2d). This also future-proofs serial
(`Sequential`) tools and shows all pending cards at once. Given that, within a batch
no prior call is ever terminal when a new start arrives, so "all terminal" fires only
across batches.

**`ToolCallCompleted` correlation:** a batch fully completes before the next
iteration streams (the loop re-streams only after `RunBatch` returns), so every
`Completed` lands while its view is still in `live.calls` — the `CallID` match is
reliable; the committed-segment fallback is defensive only.

**Commit = append** to `messages`, so the existing queue/`DisplayIndex` bookkeeping
(`tui/messages.go`, `tui/screen.go`) is unaffected: queued `RoleUser` rows keep their
indices; segment commits interleave by chronological append order.

---

## §3 — Rendering

`tui/render.go` — `renderMessages(msgs, live, queued, expandTools, width)` (the `live`
segment replaces the old `stream` param) renders committed messages, then the live
segment as the trailing in-progress block. `renderRow` for `RoleAssistant` renders the
markdown text, then each `ToolCallView` as an indented child:

```
● Let me read the config first.
  └ ReadFile  config.yaml                 ✓
    port: 8080
    host: 0.0.0.0
    … 14 more lines  (Ctrl+T)

● Now I'll fix the port.
  └ EditFile  config.yaml                 ✓
```

- **Glyph by `Status`:** `ToolRunning`→`⋯`, `ToolOK`→`✓`, `ToolError`→`✗`,
  `ToolCancelled`→`⊘`. (A tick-driven spinner for running is an optional enhancement;
  v1 uses the static `⋯`.)
- **Preview:** the renderer's `Result` is the runner-capped preview (≤ `ResultPreview`
  cap; the runner appends a truncation marker line if it capped). When `!expandTools`,
  show the first `K` lines (default 6) + `… N more lines (Ctrl+T)` if there are more;
  when `expandTools`, show **all lines of the preview** (no separate TUI line cap — the
  runner already bounds it). The two caps are aligned: the TUI never promises more than
  the event carries; output beyond the runner cap shows the runner's truncation marker.
  Error results always show (short + important); empty result → `(no output)`.
- **Layout:** cards indent 2, result lines 4; tree connector `└`/`├`; new
  `styles.ToolCallStyle` and `styles.ToolResultStyle` (dim). Width-aware wrap via the
  existing helpers. A segment with empty narration renders a bare `●` + its cards.

**`Ctrl+T` toggle:** a `tea.KeyMsg` for `ctrl+t` flips `Screen.expandTools` and
refreshes history (`tui/screen.go` `handleKey`). No slash command (`/verbose` removed —
not needed). Works in any status. (Per-card cursor + Enter-expand is deferred — it
needs a transcript-selection cursor the TUI doesn't have yet.)

---

## §4 — Loop/event amendment (cross-doc, enables the preview)

The collapsed preview needs tool output to reach the TUI; `ToolCallCompleted` doesn't
carry it today. This design **amends `2026-06-14-tools-design.md`** (kept consistent):

- **§5b** `event.ToolCallCompleted{CallID uuid.UUID; IsError bool; ResultPreview string}`
  — the runner (§2d) fills `ResultPreview` from `flattenToText(result.Content)`,
  **capped** (~2 KiB / 20 lines); when it caps it appends a truncation marker line
  (e.g. `… output truncated`). Full-fidelity on the TUI stream.
- **§2d runner ordering** — the runner **emits all `ToolCallStarted` for the approved
  batch before executing any call**, so within a batch every start precedes every
  completion (makes the TUI's all-terminal segment-boundary rule race-free; §2).
- **§5b sink table** — `ToolCallCompleted` joins the `Redactable.SinkProjection`:
  the sink copy **drops `ResultPreview`** (keeps `CallID, IsError`). `ResultPreview` is
  tool *output* (file contents, Bash output, possible secrets/PII), so this extends the
  rule from "tool *arguments* never reach a sink" to "tool *output* never reaches a
  sink." The TUI stream keeps the preview.

The single runner cap bounds the event size, the TUI's `Result` slice, and the
expanded view — there is no separate, larger TUI cap, so `Ctrl+T` never promises lines
the event doesn't carry.

---

## §5 — Edge cases

- **Tool call with no narration** — `live.text` empty → render a bare `●` + the cards.
- **Parallel batch** — multiple `ToolCallStarted` before any `Completed` (the runner
  emits all starts up front, §4); all attach as children of the same segment; each
  `Completed` updates its card by `CallID`.
- **Back-to-back tool batches with no narration** (`tool→tool`) — the new batch's first
  `ToolCallStarted` finds `live.calls` all-terminal → commits the prior segment first,
  so each batch becomes its own (possibly bare-`●`) segment (§2). Without the runner's
  emit-all-starts-up-front guarantee this would race; with it, it's deterministic.
- **Interrupt mid-tool** — committed segment shows the running card as `ToolCancelled`;
  tombstone follows.
- **Empty / truncated result** — `(no output)` / `… N more lines (Ctrl+T)` / the
  runner's `… output truncated` marker.
- **`TurnFailed{ToolLimitError}`** — the loop rolls back its context (tools design §2a),
  but the TUI keeps its display: the committed segments + a `RoleError` show what ran
  before the cap. (Display ≠ model context, per the TUI design.)

---

## §6 — Testing (table-driven, `-race` — CLAUDE.md)

- **render** — `renderMessages`/`renderRow` with nested cards in each `ToolStatus`;
  collapsed vs `expandTools`; truncation marker; empty/`(no output)`; bare-`●` segment;
  width wrap.
- **state machine** (drive `handleEvent` with synthetic events) — `text→tool→text→done`
  produces two committed segments with the right children; **`text→tool→tool(no
  text)→text→done` produces three segments** (the all-terminal rule splits the
  back-to-back batches, not one collapsed segment); parallel batch (two
  `ToolCallStarted` then two `ToolCallCompleted` by `CallID`); `Completed` updates the
  right card; interrupt mid-tool → `ToolCancelled` + tombstone; `TurnFailed` commits
  live + `RoleError`; queue indices survive segment commits.
- **TurnDone (no duplication)** — streamed final text **plus** a `TurnDone` whose
  `Message` carries that same text → **exactly one** final assistant segment (assert the
  text appears once, not twice). And the fallback: an empty `live` + a non-nil
  `TurnDone.Message` → one segment from `Message.Blocks`; empty `live` + nil `Message` →
  no final segment.
- **Ctrl+T** — toggles `expandTools` (collapsed first-K ⇄ all preview lines); works in
  any status.
- **redaction** (in the tools impl) — a fake `EventSink` receives `ToolCallCompleted`
  with **no** `ResultPreview`; the stream keeps it; runner caps the preview.

---

## Out of scope

- Per-card cursor + Enter-expand (needs a transcript-selection cursor — follow-up).
- Permission-prompt and AskUser-prompt rendering (tools design §5d — separate doc).
- Animated running spinner (static `⋯` for v1).
- Rendering `ToolUseChunk` deltas live as a "building call…" indicator (the
  `ToolCallStarted` card is enough; `ToolUseChunk` stays skipped).
- Thinking-block rendering (unchanged; still skipped).
