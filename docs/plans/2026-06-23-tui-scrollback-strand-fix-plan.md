# TUI Scrollback Strand Fix — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (or executing-plans) to implement this plan task-by-task.

**Goal:** Stop the bubbletea v2 inline renderer from stranding the managed region into native scrollback when a committed step plus the bottom UI exceed the terminal height, by paging `cursedRenderer.insertAbove` inside a vendored fork of bubbletea v2.0.7.

**Architecture:** Patch `insertAbove` to split its input into chunks each satisfying `chunkOffset + h ≤ s.height` (renderer's own coherent geometry, under the mutex it already holds), running the existing scroll+insert sequence per chunk. App side reserves one surface row, keeps the single `tea.Println`, and guards a pure-blank payload. Mitigation scope: stable-frame only; resize-window / exotic-width / `h==termRows` are documented residuals.

**Tech Stack:** Go 1.26, `charm.land/bubbletea/v2@v2.0.7` (vendored + forked via `replace`), `charm.land/ultraviolet`, go workspace (`/Users/ipotter/code/go.work`) spanning `looprig` + `swe`.

**Design doc:** `docs/plans/2026-06-23-tui-scrollback-strand-fix-design.md`

**Working branch:** `fix/tui-scrollback-strand` in `looprig` (already created). `swe` changes (the `replace`) committed on a matching branch in `swe`.

---

### Task 0: Fork + wiring (verified before any logic)

The riskiest part. Establish a patched bubbletea that BOTH build contexts compile, proven with a temporary marker.

**Files:**
- Create: `/Users/ipotter/code/bubbletea-fork/` (copy of the cached module)
- Modify: `/Users/ipotter/code/go.work` (workspace replace)
- Modify: `/Users/ipotter/code/looprig/go.mod` (replace), `looprig/vendor/**`, `looprig/vendor/modules.txt`
- Modify: `/Users/ipotter/code/swe/go.mod` (replace — needed for non-workspace builds / CI; harmless in workspace mode)

**Step 1: Create the fork from the module cache (it has the go.mod that vendor/ strips).**
```bash
cp -R /Users/ipotter/go/pkg/mod/charm.land/bubbletea/v2@v2.0.7 /Users/ipotter/code/bubbletea-fork
chmod -R u+w /Users/ipotter/code/bubbletea-fork
cd /Users/ipotter/code/bubbletea-fork && git init -q && git add -A && git commit -qm "vendor: bubbletea v2.0.7 baseline (fork)"
```

**Step 2: Add a temporary build marker** to `bubbletea-fork/cursed_renderer.go` `insertAbove` (e.g. `if s.logger != nil { s.logger.Printf("FORK-MARKER insertAbove") }` at the top, or a package-level `const ForkMarker = "strand-fix"` referenced by a test).

**Step 3: Wire the workspace replace.**
Add to `/Users/ipotter/code/go.work`:
```
replace charm.land/bubbletea/v2 => ../bubbletea-fork
```
Verify the workspace build resolves it:
```bash
cd /Users/ipotter/code/swe && go build ./... && go list -m charm.land/bubbletea/v2
```
Expected: `go list -m` shows the replacement path `=> ../bubbletea-fork`.

**Step 4: Wire the `-mod=vendor` path for looprig.**
Add `replace charm.land/bubbletea/v2 => ../bubbletea-fork` to `looprig/go.mod`, then re-vendor with the workspace disabled so vendor/ reflects the replace:
```bash
cd /Users/ipotter/code/looprig && GOWORK=off go mod vendor && GOWORK=off go build -mod=vendor ./...
```
Expected: builds; `looprig/vendor/charm.land/bubbletea/v2/cursed_renderer.go` now contains the marker.

**Step 5: Prove both contexts compile the fork.**
- Workspace: `cd /Users/ipotter/code/swe && go run ./cmd/swe` — confirm it starts (or a smoke build `go build ./cmd/swe`).
- Vendor: `cd /Users/ipotter/code/looprig && make test` (uses `-mod=vendor`) compiles.
Record in the task notes which mode each path uses.

**Step 6: Remove the marker; commit each repo.**
```bash
# bubbletea-fork: remove marker, commit
# looprig: git add go.mod vendor/ ; commit "build: vendor bubbletea fork via replace"
# swe: git switch -c fix/tui-scrollback-strand ; git add go.mod ; commit "build: replace bubbletea with strand-fix fork"
# go.work: commit if tracked, else note it is untracked local config
```

**DONE when:** both `go run ./cmd/swe` (workspace) and `looprig` `-mod=vendor` build compile the fork, verified by the marker, then marker removed.

---

### Task 1: `pageByOffset` pure helper + unit tests (in the fork)

**Files:**
- Create: `bubbletea-fork/insert_above_paging.go`
- Test: `bubbletea-fork/insert_above_paging_test.go`

**Step 1: Write the failing test.** Use bubbletea's own offset accounting as the oracle.
```go
// offset(lines, w) mirrors insertAbove: len(lines) + sum(lw/w for lw>w).
func TestPageByOffset(t *testing.T) {
    w := 10
    // each line ≤ cap; blanks survive; exact-w costs 1; 2w costs 3 (1+2); order preserved.
    lines := []string{"", "abcdefghij", strings.Repeat("x", 20), "tail"}
    pages := pageByOffset(lines, w, /*cap*/ 3)
    // assert: every page's offset(page,w) <= 3
    // assert: concat(pages) == lines (order, blanks intact)
    // assert: the 20-wide line (offset 3) is its own page
}
```
Run: `cd bubbletea-fork && go test ./ -run TestPageByOffset -v` → FAIL (undefined `pageByOffset`).

**Step 2: Implement `pageByOffset(lines []string, w, cap int) [][]string`.** Greedy: accumulate `offset += 1 + (lw>w ? lw/w : 0)`; cut before exceeding `cap`; a single line with own offset > cap is its own page. Pure, no renderer state.

**Step 3:** Run the test → PASS.

**Step 4: Commit** (`feat: pageByOffset for insertAbove paging`).

---

### Task 2: Extract `insertAboveChunk` (no behavior change)

**Files:** Modify `bubbletea-fork/cursed_renderer.go`; Test `bubbletea-fork/insert_above_chunk_test.go`

**Step 1: Characterization test.** Capture the exact bytes `insertAbove` writes today for a small payload (offset+h ≤ rows) into a buffer-backed `cursedRenderer`; snapshot them.
Run → PASS against current code (it documents current behavior).

**Step 2: Refactor.** Move the body (after the `len==0` guard and `strings.Split`) into `insertAboveChunk(lines []string, w, h int) error`; have `insertAbove` call it once with the whole `lines`. No logic change.

**Step 3:** Re-run the characterization test → PASS (identical bytes).

**Step 4: Commit** (`refactor: extract insertAboveChunk (no behavior change)`).

---

### Task 3: Page `insertAbove` + renderer strand test

**Files:** Modify `bubbletea-fork/cursed_renderer.go`; Create `bubbletea-fork/insert_above_strand_test.go` and a minimal VT grid helper `bubbletea-fork/vtgrid_test.go`.

**Step 1: Write the failing strand test.** Build a tiny test VT grid (interprets only what `insertAbove` emits: text, `\r`, `\n`(scroll at bottom), CUU `CSI nA` with top-clamp, CUD `CSI nB`, IL `CSI nL`, EL `CSI 0K`; SGR ignored; maintains a scrollback list). Seed a managed region of `h` rows (a recognizable "INPUT BOX" line). Call `insertAbove` with a payload of `2*rows` lines. Assert: the "INPUT BOX" marker appears **exactly once** across grid+scrollback; payload lines appear above it in order.
Run → FAIL (current single-shot strands → marker appears twice).

**Step 2: Implement paging in `insertAbove`:** `cap := s.height - s.cellbuf.Height(); if cap < 1 { cap = 1 }`; `for _, chunk := range pageByOffset(strings.Split(str,"\n"), s.cellbuf.Width(), cap) { insertAboveChunk(chunk, w, h) }`. Keep the `len(str)==0` early return.

**Step 3:** Run the strand test → PASS. Add cases: `h == rows` (1-row transient tolerated — assert no permanent duplicate), blank-only chunk preserved.

**Step 4:** Re-run Task 2's characterization test (small payload still single chunk, bytes unchanged) → PASS.

**Step 5: Commit** (`fix: page insertAbove so offset+h never exceeds the screen`).

---

### Task 4: App-side complement (`looprig/pkg/tui`)

**Files:** Modify `pkg/tui/surface.go`, `pkg/tui/commands.go`; Tests `surface_test.go`, `commands_test.go`.

**Step 1 (height reserve): failing test** in `surface_test.go`: a surface of exactly `term` rows clamps to `term-1`. Run → FAIL.
**Step 2:** change `clampSurfaceHeight` to keep `height-1` lines (`surface.go:163-172`); adjust the height budget call sites if needed. Run → PASS.

**Step 3 (blank guard): failing test** in `commands_test.go`: `printToScrollback` of actions whose joined payload is `""` does NOT emit a `tea.Println("")` (assert nil cmd or a non-empty sentinel). Run → FAIL.
**Step 4:** guard in `printToScrollback`/`printPayload` (`commands.go:176-192`). Run → PASS.

**Step 5:** fix the inaccurate `surface.go:73-88` comments (no test; doc only).

**Step 6: Commit** (`fix(tui): reserve a surface row, guard blank payloads`).

---

### Task 5: Integration — PTY grid (looprig)

**Files:** Create `pkg/tui/strand_integration_test.go` (build tag `//go:build integration` if it needs a PTY).

**Step 1: Write the failing/▶ test.** Drive the `Screen` program through a PTY (or the existing program-driver harness writing to a captured writer) and the same minimal VT grid; commit (a) a step taller than the viewport and (b) a reconciled `Subagent` with `> liveCallCap` children. Assert the input box appears exactly once.
Run → FAIL on unpatched build (or skip if PTY unavailable in CI; document).

**Step 2:** With Tasks 0–4 in place, run → PASS.

**Step 3:** run existing `scrollback_test.go` print-once / spacing tests → PASS (no regression).

**Step 4: Commit** (`test(tui): PTY strand regression for tall step + busy subagent`).

---

### Task 6: Full verification + cleanup

**Step 1:** `cd /Users/ipotter/code/looprig && make test` (vendor path) → all green.
**Step 2:** `cd /Users/ipotter/code/swe && go build ./... && go run ./cmd/swe` (workspace path) → starts; manually reproduce the prior scenario (busy subagent) and confirm a single input box, no blank gap.
**Step 3:** `cd /Users/ipotter/code/bubbletea-fork && go test ./...` → fork tests green.
**Step 4:** Confirm no stray marker; design-doc residuals still accurate.
**Step 5: Final commit / PR prep** on `fix/tui-scrollback-strand` (looprig + swe). Open the upstream bubbletea PR separately (not blocking).

**DONE when:** all three test contexts green and the live app shows no strand for the reproduction.

---

## Notes for executors

- **DRY/YAGNI:** do not add the resize-coherence gate, emulator-validated row model, or hard-wrap of oversized lines — those are the deferred "full guarantee" (design doc Residuals). Only the paging loop + app complement.
- **The fork is the single source of truth**; never hand-edit `looprig/vendor/...` directly — change `bubbletea-fork/` then `GOWORK=off go mod vendor` in looprig (Task 0 step 4).
- **Emulator fidelity:** the minimal VT grid models only the sequences `insertAbove` emits; it proves the strand/no-strand transition, not full terminal conformance. That is sufficient for this mitigation; full conformance is the deferred guarantee.
