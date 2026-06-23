# TUI Scrollback Strand Fix — Design

**Status:** Proposed (revised twice after codex review)
**Date:** 2026-06-23
**Area:** `pkg/tui` + a patched fork of `charm.land/bubbletea/v2`

## Goal

Stop the bubbletea v2 inline renderer from stranding copies of the managed
region (live tail, input/composer box, permission/AskUser prompt, status line)
into native terminal scrollback — the "multiple input boxes / duplicated
thinking+subagent blocks within one session" symptom — with a **strand-free
guarantee**, while keeping the scrollback-first architecture and the `StepDone`
self-heal.

## Decision history (why this is a renderer patch, not app-side)

The first two drafts tried to fix this in `pkg/tui` (width clamp + paginated
`tea.Println`, then a Screen-owned print queue). Codex review, verified against
source, killed the app-side approach:

- The `h` that the renderer uses is `s.cellbuf.Height()` = `len(cellbuf.Lines)`
  (ultraviolet buffer.go:302), set to the **last-flushed View's** height during
  a **ticker-driven** flush — not synchronously per message. At `StepDone` the
  upcoming surface shrinks (e.g. 12→4 rows) but the renderer's `cellbuf` can
  still be 12 when our queued page actually prints. The app sizes a page for
  `h=4`; `insertAbove` runs with `h=12` → `offset + 12 > term` → strand. **The
  application cannot observe the renderer's live geometry, so it cannot
  guarantee `offset + h ≤ term`.**
- Secondary app-side hazards: `tea.Batch(Println, drainCmd)` runs concurrently
  (page reorder); a single blank-line page serializes to `""` and `insertAbove`
  early-returns (blank dropped); `/clear` can't cancel an already-dispatched
  `Println`; a `tea.Tick` defer loop spins.

The renderer is the only place with the authoritative `w`, `h` (`cellbuf`), and
`s.height` (terminal rows) **under the lock it already holds**. Fixing it there
removes all of the above at once.

## Root cause (verified against `cursed_renderer.go` v2.0.7)

`cursedRenderer.insertAbove(str)` (cursed_renderer.go:706-763):

```go
w, h := s.cellbuf.Width(), s.cellbuf.Height()  // h = managed-region height
_, y := s.scr.Position()                        // cursor row in the region
sb.WriteByte('\r')
if down := h - y - 1; down > 0 { CursorDown(down) }   // → bottom of region
lines  := strings.Split(str, "\n")
offset := len(lines)                             // every line ≥1, blanks count
for _, line := range lines {
    if lw := ansi.StringWidth(line); w > 0 && lw > w { offset += lw / w }
}
WriteString(strings.Repeat("\n", offset))        // scroll up by offset
up := offset + h - 1
CursorUp(up); InsertLine(offset)                 // ← clamps when offset+h > rows
for _, line := range lines { line; EraseLineRight; "\r\n" }
s.scr.SetPosition(0, 0)
```

The strand: `CursorUp(offset + h - 1)` can only move within the screen. When
`offset + h > s.height`, it clamps at row 0, `InsertLine` writes at the wrong
row, and the previous managed region is left behind in scrollback. There is **no
guard**. Over-width lines are already accounted for (`offset += lw/w`), so the
**only confirmed trigger is the height overflow** `offset + h > s.height`; the
managed View is clipped, not soft-wrapped (ultraviolet `StyledString.Wrap`
false), so it does not strand via wrapping either.

The tall payload comes from committing a whole step in one `Println`
(`printPayload`), especially a reconciled `Subagent` card with unbounded
committed children (`transcript.go:1008` vs. live `liveCallCap = 3`).

## Design: page inside `insertAbove`

Patch `insertAbove` to split its input into chunks each of which satisfies
`chunkOffset + h ≤ s.height`, and run the existing scroll+insert sequence once
per chunk. Everything stays under the single `s.mu` it already takes.

### Why a loop composes correctly (verified from the cursor math)

At the end of one chunk the physical cursor is at the **top-left of the managed
region**: after `CursorUp(offset+h-1)` (to row `s.height-offset-h`),
`InsertLine(offset)`, then writing `offset` lines of `…\r\n` advances back down
to row `s.height-h`, column 0 — exactly the region's origin. `SetPosition(0,0)`
already records that. So the next chunk re-enters with `y = 0`, `down = h-1`
(move to region bottom) — identical to a fresh call. No extra cursor restore is
needed. Order is preserved: each chunk's scroll-up pushes the prior chunk further
up (into native scrollback), so chunks land top-to-bottom in input order.

### The patch (shape)

```go
func (s *cursedRenderer) insertAbove(str string) error {
    s.mu.Lock(); defer s.mu.Unlock()
    if len(str) == 0 { return nil }
    w, hh, termRows := s.cellbuf.Width(), s.cellbuf.Height(), s.height
    lines := strings.Split(str, "\n")
    // hard-wrap any single line whose own offset (1 + lw/w) cannot fit a chunk
    lines = wrapOversized(lines, w, capRows(termRows, hh))
    for _, chunk := range pageByOffset(lines, w, capRows(termRows, hh)) {
        if err := s.insertAboveChunk(chunk, w, hh); err != nil { return err }
    }
    return nil
}
```

- `capRows(termRows, h) = max(1, termRows - h)` — the max `offset` a chunk may
  reach (`offset + h ≤ termRows`). The exact bound is `termRows - h` (no extra
  reserve needed; the renderer's own values are exact and race-free). `max(1, …)`
  keeps progress in the degenerate `h ≥ termRows` case (see below).
- `pageByOffset` walks `lines`, accumulating bubbletea's **exact** offset
  (`+1` per line including blanks; `+ lw/w` only when `lw > w`), emitting a chunk
  before the next line would exceed `cap`. It operates on `[]string`, so a blank
  line is a real one-row chunk member and is never collapsed to `""` (fixes the
  blank-drop bug).
- `wrapOversized` hard-wraps (ANSI-aware) only a line whose own offset exceeds
  `cap` — the rare "one line taller than the room" case.
- `insertAboveChunk` is the existing body, minus the lock and the split,
  operating on a `[]string` whose offset is known to fit.

### Degenerate case `h ≥ termRows`

`cellbuf.Height()` is clipped to the terminal height during flush, so `h ≤
termRows`. If `h == termRows` (managed region fills the screen) `cap` floors at 1
and one row of the region's top is briefly overwritten then repainted next frame
— a 1-row transient, not a permanent strand. The **app-side complement** below
keeps `h ≤ termRows - 1` so this case is normally unreachable.

## App-side changes (`pkg/tui`)

With the renderer paging internally, the application simplifies:

1. **Revert to a single `tea.Println` per flush.** `printToScrollback`
   (`commands.go:188-192`) hands the whole payload to the patched renderer, which
   pages it safely. No Screen print queue, no `tea.Sequence`, no `h` measurement,
   no `/clear` cancellation logic.
2. **Reserve one row in the surface height clamp.** Change `clampSurfaceHeight`
   (`surface.go:163-172`) to clamp at `height - 1` so the managed View is never
   the full terminal height — guaranteeing `cap ≥ 1` in the renderer. (The
   `liveTailCap` halving can be relaxed/removed later as a separate cleanup; it is
   no longer load-bearing, but leave it for now to minimize change.)
3. **Fix the misdescribing comments** in `surface.go:73-88` (the width-wrap /
   resize-cascade explanation is inaccurate per the verified renderer behavior).

`safeWidth = width - 1` from the earlier draft is **dropped** — it was only ever
mitigation for a mechanism that does not exist (bubbletea accounts for over-width
lines), so it adds nothing here.

## Fork mechanics

- Fork `charm.land/bubbletea` at the pinned `v2.0.7`, apply the `insertAbove`
  patch (+ `pageByOffset`/`wrapOversized` helpers), keep the diff to that one
  function plus helpers.
- Wire via `replace charm.land/bubbletea/v2 => <fork>` (a local vendored copy
  under e.g. `third_party/bubbletea`, or a git fork). **Both** module roots that
  build the TUI need the replace: `looprig/go.mod` (for standalone build/test)
  **and** the consuming app `swe/go.mod` (replace applies only to the main
  module). Document this in both repos.
- Open an upstream PR to charmbracelet/bubbletea adding the overflow guard; drop
  the `replace` once merged + released.

## Testing strategy

`teaharness_test.go` is a `syncBuf` (`bytes.Buffer`, `:1-30`) — a byte stream,
not a grid; it cannot prove a strand. Three layers:

1. **Unit (pure) — `pageByOffset`/`wrapOversized` in the fork.** Given lines,
   `w`, `cap`: every chunk's exact bubbletea offset ≤ `cap`; blanks count as one
   and survive; exact-width lines cost 1 (not `1+lw/w`); over-wide single line is
   wrapped; chunks concatenate back to the input in order.
2. **Renderer (fork) — golden escape-sequence / emulator test.** Drive
   `insertAbove` with a payload taller than the terminal against a real terminal
   emulator (PTY + emulator, or a small VT100 grid parser for the emitted
   sequences) and assert: the managed region appears exactly once, content lands
   above it in order, nothing is stranded. Cover `h == termRows`,
   blank-only chunks, exact-width multiples, an over-wide unsplittable line.
3. **Integration (looprig) — PTY grid.** Drive the `Screen` program through a PTY
   into an emulator grid (not `ultraviolet`, which emits rather than parses; pick
   a minimal emulator, gated by the repo's dependency policy / test-only build
   tag) and assert a tall step and a `> liveCallCap` reconciled `Subagent` commit
   leave a single input box. Plus existing `scrollback_test.go` print-once /
   spacing checks unchanged.

## Residual risk (irreducible without terminal cooperation)

`ansi.StringWidth` vs. the real terminal's width opinion can still differ for
exotic glyphs (ambiguous-width CJK/emoji, font quirks). After this patch the
renderer pages and bubbletea counts using the **same** `StringWidth`, so they are
self-consistent; the only residual is the terminal physically rendering a width
neither agrees with. This is the same floor Codex's own implementation lives
with — unfixable without the terminal reporting widths — and is rare for normal
content.

## Files touched (anticipated)

- Fork: `cursed_renderer.go` — `insertAbove` paging + `pageByOffset`,
  `wrapOversized` helpers; their unit tests.
- `looprig/go.mod`, `swe/go.mod` — `replace` to the fork.
- `pkg/tui/commands.go` — keep/restore the single `Println` (no queue).
- `pkg/tui/surface.go` — `clampSurfaceHeight` → `height - 1`; fix comments.
- Tests: fork renderer test, `looprig` PTY integration test, existing
  `scrollback_test.go` unchanged.

## Open questions for the plan stage

1. Fork carrying: vendored `third_party/` copy vs. a git fork module — which is
   lower-friction given the dual `replace` (looprig + swe)?
2. `wrapOversized` reuse: can it call the same ANSI-aware wrap helper bubbletea
   ships, to match its width semantics exactly?
3. Emulator choice for the renderer/integration tests under the dependency
   policy (test-only).
4. Should the upstream PR gate this work, or do we ship the local `replace` first
   and upstream in parallel?
