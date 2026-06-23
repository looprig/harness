# TUI Scrollback Strand Fix — Design

**Status:** Approved (scope: mitigation; strategy: paging fork of `insertAbove`)
**Date:** 2026-06-23
**Area:** `pkg/tui` + a vendored patch of `charm.land/bubbletea/v2@v2.0.7`

## Goal

Stop the bubbletea v2 inline renderer from stranding copies of the managed
region (live tail, input/composer box, permission/AskUser prompt, status line)
into native scrollback — the "multiple input boxes / blank gap / duplicated
thinking+subagent blocks within one session" symptom — for the cases that occur
in normal use, while keeping the scrollback-first architecture and the
`StepDone` self-heal.

## Root cause (verified against vendored bubbletea v2.0.7)

`cursedRenderer.insertAbove(str)` (cursed_renderer.go:706-763) pushes committed
output above the pinned managed region with:

```go
w, h := s.cellbuf.Width(), s.cellbuf.Height()   // h = managed-region height
lines  := strings.Split(str, "\n")
offset := len(lines)                              // every line ≥1, blanks count
for _, line := range lines {
    if lw := ansi.StringWidth(line); w > 0 && lw > w { offset += lw / w }
}
WriteString(strings.Repeat("\n", offset))         // scroll up by offset
CursorUp(offset + h - 1); InsertLine(offset)      // ← clamps when offset+h > rows
for _, line := range lines { line; EraseLineRight; "\r\n" }
s.scr.SetPosition(0, 0)
```

The strand: when `offset + h > terminalRows`, `CursorUp(offset+h-1)` clamps at
the top of the screen, `InsertLine` lands on the wrong row, and the previous
managed region is left behind in scrollback. There is **no guard**.

**Exact trigger:** `committed_payload_rows (offset) + managed_region_rows (h) >
terminalRows`. This fires both when the committed step alone exceeds the screen
**and** when a moderate step commits while a tall bottom UI (permission box +
subagent card + thinking) is up so the *sum* exceeds the screen — the original
incident. Both the duplicate-render and the blank-gap symptoms are this one
overflow.

The tall payload comes from committing a whole step in one `tea.Println`
(`printPayload` joins the flush, `commands.go:176-192`), worst case a reconciled
`Subagent` card with unbounded committed children (`transcript.go:1008` vs. the
live card capped at `liveCallCap = 3`).

## Why this must be a renderer change (decision record)

Three earlier approaches were reviewed (codex, verified against source) and
rejected as unsound or insufficient; this is recorded so the boundary is
deliberate:

- **App-side paginated `tea.Println`** cannot guarantee correctness: the `h` the
  renderer uses is `cellbuf.Height()` = the *last-flushed* View height, updated on
  a **ticker-driven** flush, not synchronously. At `StepDone` the upcoming surface
  shrinks but `cellbuf` can still be the larger pre-collapse height, so the app's
  page sizing is wrong at insert time. Plus `tea.Batch` reorders pages and `/clear`
  can't cancel an in-flight `Println`.
- **Erase-and-reprint at the tail** is sound only as a *transactional* renderer
  redesign (forced full redraw — the 60fps flush early-returns when the View is
  unchanged, cursed_renderer.go:287; atomic single write; alt-screen suppression;
  per-commit flicker). It also still has a resize-window clamp (on the cursor-up to
  the tail top), though it degrades to a transient blank rather than a permanent
  ghost. More surgery than paging, for no benefit within the chosen scope.
- **Full strand-free guarantee** additionally requires a coherent-geometry
  deferral gate (defer commits until a flush establishes `0 < h < termRows`) and an
  emulator-validated physical-row model. Out of scope (see Residuals → future).

The renderer is the only place with the authoritative, race-free `cellbuf.Width()`
/`cellbuf.Height()`/`s.height` **under the lock it already holds**. On a stable,
post-flush frame these are coherent, which is exactly the case the mitigation
targets.

## Scope: mitigation (stable-frame paging)

Patch `insertAbove` to never emit a `CursorUp(offset+h-1)` that clamps **when the
frame geometry is coherent** (the normal case). Page the input into chunks each
satisfying `chunkOffset + h ≤ s.height`, run the existing scroll+insert sequence
per chunk. Explicitly **not** handled (documented residuals):

- resize during the exact commit window (cellbuf vs s.height briefly incoherent);
- glyphs where `ansi.StringWidth` disagrees with the terminal's rendered width;
- `h == s.height` (managed region fills the screen) beyond the one-row reserve.

These are narrow timing/Unicode cases; the mitigation removes the duplication and
blank-gap for all normal usage (tall steps, busy subagents, tall-prompt commits).

## Design: page inside `insertAbove`

```go
func (s *cursedRenderer) insertAbove(str string) error {
    s.mu.Lock(); defer s.mu.Unlock()
    if len(str) == 0 { return nil }
    w, h, termRows := s.cellbuf.Width(), s.cellbuf.Height(), s.height
    cap := termRows - h
    if cap < 1 { cap = 1 }                 // degenerate h>=termRows: 1-row transient
    lines := strings.Split(str, "\n")
    for _, chunk := range pageByOffset(lines, w, cap) {
        if err := s.insertAboveChunk(chunk, w, h); err != nil { return err }
    }
    return nil
}
```

- `pageByOffset(lines, w, cap)` walks `lines`, accumulating bubbletea's **own**
  offset formula (`+1` per line incl. blanks; `+ lw/w` when `lw > w`), and cuts a
  chunk before the next line would push the chunk's offset past `cap`. A single
  line whose own offset exceeds `cap` becomes its own chunk (we rely on bubbletea's
  over-counting being conservative; a hard-wrap of that rare line is a possible
  refinement, deferred). It operates on `[]string`, so a blank line is a real
  one-row chunk member and is never collapsed to `""`.
- `insertAboveChunk(chunk, w, h)` is the **existing** `insertAbove` body minus the
  lock and the split, operating on a `[]string` whose offset is known `≤ cap`.
- **Why the loop composes** (verified, codex round 3 claim A): after a chunk,
  `CursorUp(offset+h-1)` → `InsertLine(offset)` → writing `offset` lines of
  `…\r\n` lands the cursor back at row `s.height-h`, col 0 — the managed-region
  origin — and `SetPosition(0,0)` records it, so the next chunk re-enters with
  `y=0`, `down=h-1`, identical to a fresh call. Order is preserved: each chunk's
  scroll-up pushes prior chunks further up into native scrollback.
- **Why bubbletea's own formula is safe for paging:** the patched insert and
  bubbletea's accounting use the same `ansi.StringWidth`, so paging is
  self-consistent; where the formula over-counts (e.g. an exact-multiple-width
  line) it reserves an extra blank row — wasteful, never a strand. Under-count only
  arises from `StringWidth`-vs-terminal disagreement, the documented residual.

## App-side complement (`pkg/tui`)

1. **`clampSurfaceHeight` → `height - 1`** (`surface.go:163-172`) so the managed
   View is never the full terminal height; with `h ≤ termRows-1`, `cap ≥ 1` in the
   common case and the degenerate path is normally unreachable.
2. **Guard a pure-blank payload** at the print boundary: if `printPayload`
   produces `""` (all-blank actions joined), don't emit a `tea.Println("")` (it
   early-returns and the blank is dropped); emit a single `" "`-bearing line or
   skip. (`commands.go:176-192`.)
3. **Fix the misdescribing comments** in `surface.go:73-88` (the width-wrap /
   resize-cascade explanation is inaccurate per verified renderer behavior).
4. Keep the single `tea.Println` per flush — the patched renderer pages it. No
   Screen print queue, no `tea.Sequence`.

## Fork / vendoring mechanics

looprig builds with `-mod=vendor` (Makefile:12) and vendors deps; there is a
`go.work` spanning `looprig` + `swe` (`/Users/ipotter/code/go.work`). The patch
must reach the binary in both standalone-looprig and `go run ./cmd/swe` builds.
Plan **Task 0** establishes and *verifies* the wiring before any patch logic
(add a build marker, confirm the patched code is compiled). Candidate approach:

- A patched copy of bubbletea at a sibling path (git fork or `third_party/`).
- `replace charm.land/bubbletea/v2 => <fork>` in **both** `looprig/go.mod` and
  `swe/go.mod` (replace applies per main module); re-run `go mod vendor` in looprig
  so `vendor/` (and `vendor/modules.txt`) carry the patched source for
  `-mod=vendor`. Verify `go build ./...` in both modules and `go run ./cmd/swe`
  pick up the patch. (The `go.work`↔vendor interaction is verified empirically in
  Task 0, not assumed.)

## Testing strategy

`teaharness_test.go` is a `syncBuf` (`bytes.Buffer`) — a byte stream, not a grid.

1. **Unit (fork) — `pageByOffset`.** Given lines, `w`, `cap`: every chunk's
   bubbletea offset ≤ `cap`; blanks count as one and survive; exact-`w` lines cost
   1; `2w`/`3w` lines match bubbletea's `1 + lw/w`; an over-`cap` single line is its
   own chunk; chunks concatenate back to the input in order.
2. **Renderer (fork) — emulator/golden.** Drive `insertAbove` with a payload
   taller than the terminal against a terminal-grid emulator (PTY + emulator, or a
   small parser for the emitted CUU/CUD/IL/EL/CRLF sequences) and assert: the
   managed region appears exactly once, content lands above it in order, nothing
   stranded; cover `h==termRows` (1-row transient acceptable), blank-only chunks.
3. **Integration (looprig) — PTY grid.** Drive the `Screen` program through a PTY
   into a grid and assert a tall step and a `> liveCallCap` reconciled `Subagent`
   commit leave a single input box. Existing `scrollback_test.go` print-once /
   spacing tests unchanged.

## Residual risks (documented; future "full guarantee")

- **Resize during the commit window** — `resize` updates `s.height` but not
  `cellbuf` until the next flush (cursed_renderer.go:618-630), so a commit racing a
  resize can mis-page. Closing it needs a coherent-geometry deferral gate (defer
  until a flush establishes `0 < h < termRows`).
- **`ansi.StringWidth` vs. terminal width** for exotic glyphs — self-consistent
  here but the terminal may render differently. Needs an emulator-validated row
  model.
- **`h == termRows`** beyond the one-row reserve — 1-row transient.

## Files touched (anticipated)

- Fork: `cursed_renderer.go` — `insertAbove` paging + `pageByOffset`,
  `insertAboveChunk`; their unit tests.
- `looprig/go.mod`, `swe/go.mod`, `looprig/vendor/**` — `replace` + re-vendor.
- `pkg/tui/surface.go` — `clampSurfaceHeight` → `height-1`; fix comments.
- `pkg/tui/commands.go` — pure-blank-payload guard; keep single `Println`.
- Tests: fork unit + emulator tests; looprig PTY integration; existing
  `scrollback_test.go` unchanged.
