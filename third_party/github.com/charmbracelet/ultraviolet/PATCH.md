# Vendored fork: `github.com/charmbracelet/ultraviolet`

This is a **vendored, minimally patched** copy of Charm's `ultraviolet` terminal
renderer (a transitive dependency of Bubble Tea v2, `charm.land/bubbletea/v2`),
wired into the build via a `replace` directive in the repo root `go.mod`:

```
replace github.com/charmbracelet/ultraviolet => ./third_party/github.com/charmbracelet/ultraviolet
```

- **Upstream module:** `github.com/charmbracelet/ultraviolet`
- **Pinned version:** `v0.0.0-20260525132238-948f4557a654`
  (pseudo-version; commit `948f4557a654`. There are no semver tags — upstream
  ships pseudo-versions only.)
- **Patched file:** `terminal_renderer.go`, function `(*TerminalRenderer).Render`
- **Everything else is byte-for-byte identical to the pinned upstream copy.**

## The bug (Bubble Tea v2 inline / normal-screen mode resize scrollback leak)

In inline mode, on a terminal resize, the renderer keeps a "current buffer"
(`curbuf`) modeling what is on screen and diffs each new frame against it to emit
the minimal escape sequences. At the end of `Render`, when the buffer dimensions
changed, `curbuf` is resized to the new dimensions and the new frame's lines are
copied back into it so the **next** diff is correct.

The upstream sync loop only copied the **newly-grown tail** rows:

```go
// Sync new lines to old lines
for i := curHeight - 1; i < newHeight; i++ {
    copy(s.curbuf.Line(i), newbuf.Line(i))
}
```

This leaves the rest of `curbuf` holding **stale content from the previous,
different-width frame**:

- On a **width growth**, `RenderBuffer.Resize` extends each line with blank
  cells but preserves the old narrow content in columns `0..oldWidth-1`, so the
  prior narrow row survives.
- On a **height shrink** (`curHeight > newHeight`), the loop runs **zero
  times** (`curHeight-1 >= newHeight`) and syncs nothing at all.

The next (non-clear) `Render` then diffs the new frame against this stale
`curbuf`. For full-width lines whose content matches the stale buffer in the
leading columns — e.g. the composer box's `┌……┐` top and `└……┘` bottom borders,
which share their leading `┌───` / `└───` run across widths — `transformLine`
believes the leading columns are unchanged and emits only the trailing portion
at an **absolute column offset** (`CSI <col> C`, e.g. `\e[40C────…┐`). At the
same time the frame's terminating **relative cursor-up** count is computed
assuming one physical row per logical line, so after such a partial redraw it
under-/over-counts and the next frame's erase (`\e[J`) clears the wrong number
of rows. In inline mode this **strands the prior, narrower separator + box-top
rows in native scrollback** on every resize step — cumulative across a drag.

## The fix (minimal, one line)

Sync **all** rows from the new buffer into `curbuf` after a dimension change,
not just the grown tail:

```go
// VENDOR PATCH (urvi): sync ALL rows from newbuf into curbuf after a
// dimension change, not just the grown tail (curHeight-1..newHeight).
for i := 0; i < newHeight; i++ {
    copy(s.curbuf.Line(i), newbuf.Line(i))
}
```

A dimension change always goes through a `clearUpdate` full redraw (Bubble Tea's
inline renderer calls `Erase()` when the frame bounds change), which has already
put the correct frame on screen. Making `curbuf` a faithful copy of `newbuf` for
**every** row keeps the renderer's model in sync with the screen so the next
diff is correct and never partial-redraws a full-width border at a column
offset. The change is local to the post-render sync block; no public API or
behavior outside the resize path is affected. Upstream's full test suite
(`go test ./...` inside this directory) passes unchanged.

## Empirical verification

Confirmed by driving a real `tea.Program` (this renderer, end to end) with a
captured `io.Writer` through a resize drag of a full-width bordered-box composer
frame, deterministically (one message at a time, waiting for each frame to
flush):

- **Before** the patch: the drag emits `CSI <col> C` + horizontal-border runs
  (e.g. `\e[40C────…┐`) — the stranded-border leak.
- **After** the patch: zero `CSI <col> C`-offset border runs; every border line
  is emitted whole from column 0 (clean full redraw).

The leak and its absence are locked by `tui/resize_scrollback_leak_test.go` in
this repo, which fails against pristine upstream and passes against this patched
copy.

## Re-syncing on dependency updates

When Bubble Tea / `ultraviolet` is bumped (directly or transitively):

1. Check whether upstream has fixed this in `(*TerminalRenderer).Render`'s
   post-resize `curbuf` sync loop (look for the `for i := curHeight - 1; ...`
   loop, or a `Render`/`clearUpdate` rewrite). If fixed upstream, **drop this
   fork**: remove the `replace` directive and the `third_party/.../ultraviolet`
   tree, run `go mod tidy`, and delete the CLAUDE.md note.
2. If not fixed upstream, re-vendor: copy the new
   `$(go env GOMODCACHE)/github.com/charmbracelet/ultraviolet@<newversion>` into
   this directory (`chmod -R u+w` — cache files are read-only), then re-apply
   the one-line change above. Update the pinned version in this file and the
   `replace` directive.
3. Re-run `tui/resize_scrollback_leak_test.go` (must pass) and, to confirm the
   test still has discriminating power, temporarily revert the loop to
   `for i := curHeight - 1; i < newHeight` and confirm it fails.

## Related

This is a known class of Bubble Tea **v2 inline / normal-screen** render bug
(stale `curbuf` diff after a resize). See the Charm `bubbletea` and
`ultraviolet` repositories for the upstream renderer (`cursed_renderer.go`,
`terminal_renderer.go`).
