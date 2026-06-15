# TUI Tool-Use Rendering — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Render an agentic turn's tool calls in the TUI transcript as children of the assistant text segment that triggered them — per-segment chronological nesting, status glyphs, a collapsed result preview with a `Ctrl+T` expand toggle — by reconstructing the structure from the event stream.

**Architecture:** Extend the existing flat `tui/` model: `DisplayMessage` gains nested `ToolCallView` children; `Screen`'s single `stream string` becomes a `live` segment (`{text, calls}`) built by a state machine in `handleEvent`; the renderer nests cards under each assistant segment. No new goroutines; pure Elm state.

**Tech Stack:** Go, Bubbletea/lipgloss (already in `tui/`), stdlib. No new dependencies.

**Design reference:** `docs/plans/2026-06-14-tui-tool-use-design.md` (cited as **§N**). Read it first — it is the authoritative spec; this plan is the execution order.

**Prerequisite:** the tools subsystem's loop events must exist —
`event.ToolCallStarted{CallID, ToolName, Summary}` and
`event.ToolCallCompleted{CallID, IsError, ResultPreview}`, emitted by the runner for
**every requested call** (tools-design §2d; `docs/plans/2026-06-14-tools-impl.md`
Phase 5). This TUI work is additive against those event types: it can be built and
unit-tested with **synthetic events** before the loop emits them live (the current TUI
already ignores unknown event types, so partial integration is safe). The
permission-prompt / AskUser-prompt rendering is a *separate* doc — **not** in scope here.

**Conventions (CLAUDE.md):** table-driven tests, `go test -race ./...`, frequent commits. The `tui/` package is already implemented and green — every task **modifies** existing files and must keep the existing tests passing (adjust them where a signature changes).

**TDD rhythm (every task):** write/adjust the failing test → run, confirm it fails → minimal implementation → run `-race`, confirm pass → commit.

---

## Phase 1 — Display model + `Screen.live` refactor

### Task 1.1: `ToolStatus` + `ToolCallView` + `DisplayMessage.ToolCalls`

**Files:** Modify `tui/message.go`; Test `tui/message_test.go`.

- Add per §1:
  ```go
  type ToolStatus uint8
  const ( ToolRunning ToolStatus = iota; ToolOK; ToolError; ToolCancelled )

  type ToolCallView struct {
      CallID   uuid.UUID
      ToolName string
      Summary  string
      Status   ToolStatus
      Result   []string // capped preview lines; nil while running
  }
  ```
  and a `ToolCalls []ToolCallView` field on `DisplayMessage` (populated only for `RoleAssistant`).
- Test: `ToolStatus` constant order; a `DisplayMessage` with `ToolCalls` round-trips; existing `DisplayMessage` tests still pass (the new field is zero by default).
- Commit.

### Task 1.2: replace `Screen.stream` with a `live` segment

**Files:** Modify `tui/screen.go` (struct + the `m.stream` sites at `:107,:117,:121,:124–131,:189,:445`); Modify `tui/render.go` (`renderMessages` signature); Test `tui/screen_test.go`, `tui/render_test.go`.

This is a **behavior-preserving refactor** — `live.calls` stays empty until Phase 2, so the transcript renders exactly as today.

- Add `type liveSegment struct { text string; calls []ToolCallView }` (in `screen.go` or `message.go`).
- Replace `Screen.stream string` with `Screen.live liveSegment`. Update each site: `m.stream += x` → `m.live.text += x`; `m.stream = ""` → `m.live = liveSegment{}`; `m.stream != ""` → `m.live.text != "" || len(m.live.calls) > 0`; the `TurnInterrupted` flush uses `m.live.text`.
- Change `renderMessages(msgs, stream string, queued, width)` → `renderMessages(msgs, live liveSegment, queued map[int]bool, expandTools bool, width int)`. For now it renders the live segment's `text` exactly as the old trailing `stream` (cards come in Phase 3); ignore `expandTools` for now. Update the call at `screen.go:445` (`refreshHistory`).
- Test: existing `screen_test.go`/`render_test.go` pass after mechanical signature updates; add nothing new behaviorally.
- Commit.

### Task 1.3: `Screen.expandTools`

**Files:** Modify `tui/screen.go` (struct field); Test later (Phase 4).

- Add `expandTools bool` to `Screen` (default false). No behavior yet.
- Commit (or fold into Task 1.2's commit).

**Phase 1 close:** `go test -race ./tui/...` green; transcript unchanged.

---

## Phase 2 — Event-reconstruction state machine (`handleEvent`)

> Rewrite the `handleEvent` cases (`tui/screen.go:101`) to build segments per §2. Drive every test with **synthetic `event.*` values** (no real loop needed). Add a small helper `commitLive()` that appends `DisplayMessage{Role:RoleAssistant, Blocks:[TextBlock(live.text)], ToolCalls:live.calls}` when `live` is non-empty and resets `live`.

### Task 2.1: `TokenDelta` commits the prior segment when new narration follows tools

**Files:** Modify `tui/screen.go` `handleEvent` `TokenDelta` case; Test `tui/screen_test.go`.

- Per §2: on `*content.TextChunk`, **if `len(live.calls) > 0`** call `commitLive()` first, then `live.text += chunk.Text`. (Other chunk types still skipped.)
- Test: `text→(simulate calls present)→text` produces a committed segment then a fresh live; plain `text→text` (no calls) just accumulates.
- Commit.

### Task 2.2: `ToolCallStarted` — append card, commit on back-to-back batches

**Files:** Modify `tui/screen.go` `handleEvent` (new `case event.ToolCallStarted`); Test `tui/screen_test.go`.

- Per §2: **if `live.calls` is non-empty and every existing call's `Status` is terminal** (`!= ToolRunning`), `commitLive()` first (a new batch with no narration between — §5 back-to-back). Then append `ToolCallView{CallID, ToolName, Summary, Status: ToolRunning}` to `live.calls`. `refreshHistory()`.
- Test: `text→tool→tool(no text)→text→done` yields **three** segments (the all-terminal rule splits the back-to-back batches); a parallel batch (two `ToolCallStarted` before any completion) attaches both to one segment.
- Commit.

### Task 2.3: `ToolCallCompleted` — update card by `CallID`

**Files:** Modify `tui/screen.go` `handleEvent` (new `case event.ToolCallCompleted`); Test `tui/screen_test.go`.

- Per §2: find the `ToolCallView` by `CallID` in `live.calls` (fallback: scan the most recent committed `RoleAssistant` message's `ToolCalls`); set `Status` (`ToolOK`/`ToolError` from `IsError`) and `Result` from `splitLines(ev.ResultPreview)`. `refreshHistory()`.
- Test: a `Completed{IsError:false}` flips the matching card to `ToolOK` with its `Result`; `IsError:true` → `ToolError`; a `Completed` for an unknown `CallID` is a no-op (no panic). **Failure card**: a `Started`+`Completed{IsError:true, ResultPreview:"error: …"}` with no execution renders a `✗` card (covers denied/invalid/unknown/WriteTarget — §5).
- Commit.

### Task 2.4: `TurnDone` — `live` authoritative, `Message` fallback (no duplication)

**Files:** Modify `tui/screen.go` `handleEvent` `TurnDone` case; Test `tui/screen_test.go`.

- Per §2 (the disambiguated rule): if `live` is non-empty → `commitLive()`. **Else** if `ev.Message != nil` and has content → append a `RoleAssistant{Blocks: ev.Message.Blocks}` (fallback for a non-streamed message). **Never both.** Clear `live`. Guard nil `Message`.
- Test (the no-duplication test, §6): streamed final text **plus** a `TurnDone.Message` carrying that same text → **exactly one** final assistant segment (assert the text appears once). Empty `live` + non-nil `Message` → one segment from `Message.Blocks`. Empty `live` + nil `Message` → no final segment.
- Commit.

### Task 2.5: `TurnFailed` / `TurnInterrupted` — commit live, keep tool work visible

**Files:** Modify `tui/screen.go` `handleEvent` (`TurnFailed`, `TurnInterrupted`); Test `tui/screen_test.go`.

- `TurnFailed`: if `live` non-empty → `commitLive()`; then `appendError(ev.Err)` (existing helper). `TurnInterrupted`: mark any still-`ToolRunning` card in `live.calls` as `ToolCancelled`, `commitLive()` (so partial text + tool cards stay visible), then append the `RoleInterrupted` tombstone. Clear `live`.
- Test: interrupt mid-tool → committed segment shows the running card as `ToolCancelled` + tombstone follows; `TurnFailed` after a tool batch → segment committed + `RoleError`; **queue indices survive** the extra segment commits (drive a queued input through and assert `queue[i].DisplayIndex` still points at the right `RoleUser` row).
- Commit.

**Phase 2 close:** `go test -race ./tui/...`. The model is now built correctly even though it renders flat (cards not yet drawn).

---

## Phase 3 — Nested rendering

### Task 3.1: tool-card styles

**Files:** Modify `tui/styles/styles.go`; Test `tui/styles/styles_test.go`.

- Add `ToolCallStyle` (e.g. `Faint(true)`) and `ToolResultStyle` (`Faint(true)`) alongside the existing styles.
- Test: styles render non-empty (matching the existing styles test pattern).
- Commit.

### Task 3.2: render a `ToolCallView` (cards + glyph + preview)

**Files:** Modify `tui/render.go` (new `renderToolCalls(calls []ToolCallView, expandTools, width int) string` + glyph helper); Test `tui/render_test.go`.

- Per §3: each card = `└ <ToolName>  <Summary>   <glyph>` then result lines indented 4. Glyph: `ToolRunning→⋯`, `ToolOK→✓`, `ToolError→✗`, `ToolCancelled→⊘`. Preview: `!expandTools` → first `K=6` lines + `… N more lines (Ctrl+T)` if more; `expandTools` → all `Result` lines (the runner already capped them — no extra TUI cap). Empty result → `(no output)`. Error result always shows. Width-aware wrap via existing helpers.
- Test (table-driven): each status glyph; collapsed vs expanded; truncation marker; `(no output)`; multi-card (parallel batch).
- Commit.

### Task 3.3: nest cards under assistant rows + the live segment

**Files:** Modify `tui/render.go` (`renderRow` `RoleAssistant`; `renderMessages` trailing live block); Test `tui/render_test.go`.

- `renderRow` for `RoleAssistant`: render the markdown text, then `renderToolCalls(m.ToolCalls, expandTools, width)` if any. A segment with empty text renders a bare `●` + its cards (§3).
- `renderMessages`: after the committed rows, render the live segment (its `text` then `renderToolCalls(live.calls, …)`) as the trailing in-progress block when non-empty. Thread `expandTools` through.
- Test: an assistant `DisplayMessage` with `ToolCalls` renders text + indented cards; a live segment with running cards renders as the trailing block; the full `text→tool→text` transcript renders nested correctly.
- Commit.

**Phase 3 close:** `go test -race ./tui/...`; tool cards now render.

---

## Phase 4 — `Ctrl+T` expand toggle

### Task 4.1: toggle `expandTools`

**Files:** Modify `tui/screen.go` `handleKey` (`:222`); Test `tui/screen_test.go`.

- Add `case "ctrl+t":` → `m.expandTools = !m.expandTools; m.refreshHistory(); return m, nil`. Works in any status (it only re-renders).
- Test: a `ctrl+t` `tea.KeyMsg` flips `expandTools`; a transcript with a long tool result renders collapsed (first K + marker) before and all lines after.
- Commit.

**Phase 4 close:** `go test -race ./tui/...`; `make secure`.

---

## Integration check (after the tools loop lands)

Once `docs/plans/2026-06-14-tools-impl.md` Phase 5 emits the events live, run the app
(`docs/plans` / `run` skill) against `agents/coding` on a multi-tool prompt and verify:
each assistant segment shows its tool cards nested beneath; a denied/failed call shows a
`✗` card (not vanished); `Ctrl+T` reveals full previews; interrupt mid-tool shows `⊘`.
No new automated test here — the unit tests (synthetic events) already cover the logic;
this is a manual smoke check of the wired stream.

---

## Out of scope (per design)

- Permission-prompt + AskUser-prompt rendering (separate doc; tools-design §5d).
- Per-card cursor + Enter-expand (needs a transcript-selection cursor; `Ctrl+T` global toggle is v1).
- Animated running spinner (static `⋯`).
- Rendering `ToolUseChunk` deltas live / thinking blocks (still skipped).
