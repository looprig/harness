# TUI Scrollback Strand Fix — Design

**Status:** Proposed (revised after codex review of the first draft)
**Date:** 2026-06-23
**Area:** `pkg/tui` (scrollback-first inline renderer)

## Goal

Stop the bubbletea v2 inline renderer from stranding copies of the managed
region (the live tail, the input/composer box, the permission/AskUser prompt,
the status line) into native terminal scrollback — the "multiple input boxes /
duplicated thinking+subagent blocks within one session" symptom — without
abandoning the scrollback-first architecture or the `StepDone` self-heal.

## How rendering works today

Scrollback-first mode runs the bubbletea v2 inline renderer with `AltScreen =
false` (`screen.go:419-436`). Two paint paths:

1. **Committed entries → native scrollback.** Finalized entries get a stable
   `displayID` (`transcript.go:93-97`); `scrollbackModel` is a print-once engine
   keyed on it (`scrollback.go:20-63`). `flush` renders new entries and
   `printToScrollback` emits them via **one** `tea.Println`
   (`commands.go:188-192`; `printPayload` joins the whole flush, `:176-183`).
   `tea.Println` is the renderer's `insertAbove`.
2. **Live surface → managed region.** The live tail + bottom chrome (composer /
   prompt / status / tip) is the bubbletea-managed region, repainted every frame
   (`screen.go:419-452`, `surface.go`).

## Root cause (verified against bubbletea v2.0.7 source)

`charm.land/bubbletea/v2@v2.0.7` renders with `cursedRenderer`. Its
`insertAbove` (cursed_renderer.go:706-733) does:

```go
lines  := strings.Split(str, "\n")
offset := len(lines)                       // every line counts as >=1, blanks included
for _, line := range lines {
    lineWidth := ansi.StringWidth(line)
    if w > 0 && lineWidth > w {
        offset += (lineWidth / w)          // over-width lines add extra rows
    }
}
// ... scroll up by offset, then CursorUp(offset+h-1) + InsertLine(offset)
```

There is **no guard for `offset + h > terminalRows`**. When the payload is tall
enough that `offset + h` exceeds the screen, the terminal's cursor-up clamps at
the top and `InsertLine` writes at the wrong row, leaving the previous managed
region behind in scrollback. This is the strand.

Two corrections to the first draft, both load-bearing:

- **Over-width lines are NOT undercounted.** bubbletea adds `lineWidth / w` for
  lines wider than the cell buffer, so a soft-wrapping committed line is
  accounted for. The first draft's "soft-wrap ⇒ undercount ⇒ strand" mechanism
  is wrong. The **only confirmed trigger is the height overflow** (`offset + h >
  term`), driven by a tall committed payload in one `Println`.
- **The managed `View` is clipped, not soft-wrapped** (v2 default
  `StyledString.Wrap == false`); its height is newline-count based. So the live
  surface does not strand via wrapping either. (`surface.go:73` comments
  misdescribe this; correcting those comments is a minor cleanup, not core.)

### What makes the payload tall

A whole step commits in one `Println` (`printPayload`). A tall step — long
thinking + narration + several tool cards, or a reconciled `Subagent` card whose
committed children are unbounded (`reconcileSubagent` copies the full
`acc.children`, `transcript.go:1008`, vs. the live card capped at `liveCallCap =
3`) — makes `offset` large. With any non-trivial managed region `h`, `offset + h`
clears the screen and strands.

The existing guards (`liveTailCap` halving, `clampSurfaceWidth/Height`,
`surface.go:41-201`) bound only the **live surface** `h`; they never bound the
**committed payload** `offset`, which is the actual overflow term.

## How Codex avoids this (reference, not adopted wholesale)

Codex (`../codeagents/codex/codex-rs/tui/insert_history.rs`) forked ratatui's
terminal and owns history insertion: it pre-wraps + counts physical rows, inserts
via a DECSTBM scroll region that physically protects the viewport, and streams
finalized lines incrementally so there is never a large blob. The
scroll-region/atomic-paging part requires owning the renderer. This design
adopts the achievable application-side subset and records the renderer-level fix
as the structural endgame.

## Design: Screen-owned serialized print queue

Never hand bubbletea a `Println` whose `offset + h` exceeds the screen. Replace
the single `tea.Println` per flush with **one Screen-owned FIFO print queue** that
emits at most one page per Update cycle, each page sized to the current capacity.

### Components

- **`printQueue`** (new, owned by `Screen`): a FIFO of rendered scrollback lines
  awaiting emission. `flush` renders newly-committed entries (via the unchanged
  print-once `scrollbackModel`) and **enqueues** the lines instead of printing
  them, then kicks the drain.
- **Drain loop (message-driven, serialized).** A `drainPrintMsg` handler:
  1. If the queue is empty → no-op.
  2. Compute `h` = the exact height of the upcoming managed surface (the same
     `surfaceView` string `View` will render; **empty surface measures as 0**).
  3. Compute `capacity = max(0, termRows - h - reserve)` with `reserve = 1`.
     - If `capacity == 0` (a tall prompt on a short terminal): **defer** — emit
       nothing this cycle, re-arm `drainPrintMsg` via a short `tea.Tick` so it
       retries when the surface shrinks. Never force a one-row print.
  4. Otherwise take lines from the front, accumulating `offset` with bubbletea's
     **exact** formula (`+1` per line including blanks; `+ lineWidth/w` when
     `ansi.StringWidth(line) > w`), stopping before a line would push `offset`
     past `capacity`. Emit that page as one `tea.Println`.
  5. Return `tea.Batch(tea.Println(page), drainCmd())` where `drainCmd` emits the
     next `drainPrintMsg`. Each page is a separate message, so bubbletea repaints
     between pages and `h` is re-measured every page.
- **Oversized single line.** A single rendered line whose own `offset`
  (`1 + lineWidth/w`) exceeds `capacity` cannot be placed by line boundaries.
  Hard-wrap *that line only* (ANSI-aware, to the cell-buffer width) before paging
  so it becomes lines that fit. This is the sole re-wrapping step and a rare edge
  case (e.g. an unwrapped giant header on a very short terminal).

### Why this is correct

Because every emitted `Println` satisfies `offset + h ≤ termRows - reserve <
termRows`, bubbletea's `insertAbove` always operates within the screen — the
cursor-up never clamps, `InsertLine` lands on the right row, and the managed
region is never stranded. Ordering and isolation hold because a **single** queue
is drained **one page per message** by the Screen; concurrent `flush` calls just
append to the same FIFO, so pages never interleave (unlike per-flush
`tea.Sequence`, which spawns independent ordered runs that the runtime can
interleave with each other and with repaints/resizes).

### Self-heal preserved

The committed entry set is unchanged — `flush` still renders exactly the
`StepDone` group, and the print-once engine still keys on `displayID` so a
re-flush never re-enqueues. Only the transport (queue + paging) changes.

### Lifecycle

- **`/clear` (reopen):** reset the queue alongside `scrollbackModel` in
  `handleReopenResult` (`screen.go:350-369`) so stale pages from the old session
  never print into the new transcript.
- **Resize:** capacity is recomputed every page, so a resize between pages is
  absorbed. Already-enqueued lines were rendered at the old width; bubbletea's
  `offset` accounting measures their *actual* width, so they still cannot strand
  (worst case is a cosmetic wrap). Re-rendering queued lines on resize is out of
  scope.

### Width margin (demoted from the first draft)

Render the surface clamp (and committed rendering) at `safeWidth = max(1, width -
1)`. With over-width lines now correctly accounted by bubbletea, this is **no
longer load-bearing** — it is defense-in-depth against the one residual
(`ansi.StringWidth` vs. real-terminal disagreement at the right edge). Keep it
small and floored at 1; do not let it reach 0 (which disables wrapping helpers,
`render.go:104` fallback and friends).

## Divergence from the review's recommendation (with reasoning)

The review recommended "finalize **every** committed line into guaranteed-safe
physical rows (ANSI-aware hard-wrap/truncate all output incl. headers)." We do
**not** hard-wrap every line, because bubbletea already accounts for an
over-width line's rows in `offset` — an unwrapped tool/subagent header is a
cosmetic wrap, not a strand. We only (a) **measure** with bubbletea's exact
formula for paging and (b) hard-wrap the single-line-exceeds-capacity edge case.
This is the minimal set that makes the paging math sound; wrapping all headers is
a separate cosmetic change.

"Strip untrusted terminal control sequences from committed output" is a real,
related hardening (raw cursor/scroll escapes in tool output could desync the
cursor independently of paging) but is **out of scope** for the strand fix and
recorded as a residual below.

## Testing strategy

The existing `teaharness_test.go` is a `syncBuf` (mutex-wrapped `bytes.Buffer`,
`:1-30`) — a byte stream, **not** a terminal grid; it cannot prove a region was
stranded. Two layers:

1. **Unit tests for the paginator (pure function).** Given rendered lines,
   `termRows`, and `h`, assert: every page's bubbletea-`offset` ≤ `capacity`;
   blank separator lines count as one row; exact-width-multiple lines match
   bubbletea's `1 + lineWidth/w`; pages concatenate back to the input in order;
   an over-wide single line is split (never emitted whole over capacity);
   `h == termRows` yields capacity 0 and defers (emits nothing).
2. **Grid integration test.** Render the program's output bytes into a real cell
   grid using `charmbracelet/ultraviolet` (already a transitive dep — bubbletea
   v2's own cell buffer) or a PTY + emulator, then assert on the final grid:
   (a) the input/composer box appears exactly once; (b) committed lines appear
   above it in order; (c) no duplicated thinking/tool/subagent rows. Cover: a
   step taller than the viewport; a reconciled `Subagent` with `> liveCallCap`
   children; `h == term`; a resize between pages; `/clear` mid-queue.
3. **No-regression.** Existing `scrollback_test.go` print-once/spacing tests and a
   short single-step turn still emit a clean single page.

## Residual risks (documented, not closed here)

- **`ansi.StringWidth` vs. real terminal disagreement** — the one width residual.
  bubbletea measures inside `insertAbove`; an app-side fix measures the same way,
  so it cannot close this. Only owning the renderer does. `safeWidth` mitigates
  the right-edge case. Rare for normal content.
- **Untrusted control sequences in committed tool output** — could reposition the
  cursor and desync independently of paging. Separate hardening (SGR-allowlist
  sanitization); not addressed here.
- **Resize re-wrapping of already-queued lines** — cosmetic only; bubbletea's
  offset accounting prevents a strand.

## Structural endgame (future, not this change)

Fork/patch `cursedRenderer.insertAbove` to either page atomically under the
renderer lock or insert via a protected DECSTBM scroll region (the Codex
approach). That is the only fix that also closes the width-measurement residual
and makes the guarantee unconditional. Tracked as follow-up.

## Files touched (anticipated)

- `pkg/tui/commands.go` — replace single `Println`; the paginator helper (pure,
  unit-testable) lives here.
- `pkg/tui/screen.go` — `printQueue` field; `flush` enqueues; `drainPrintMsg`
  handler; measure `h`; reset queue in `handleReopenResult`.
- `pkg/tui/surface.go` — `safeWidth` clamp; fix the misdescribing comments.
- Tests: `pkg/tui/commands_test.go` (paginator units), a new grid integration
  test, existing `scrollback_test.go` unchanged-behavior checks.

## Open questions for the plan stage

1. `reserve = 1` vs. `0` — `offset + h ≤ term` is the exact invariant; `reserve =
   1` is conservative insurance against off-by-one in bubbletea's own position
   math. Confirm at implementation with a grid test.
2. Where to source `h`: factor the `surfaceView` height out of `View` into a
   shared measure so flush and View agree exactly.
3. Defer re-arm cadence (the `tea.Tick` interval) when `capacity == 0`.
