# TUI Input + Message Redesign — Design

Date: 2026-06-15

## Goal

Refresh the chat TUI's input and transcript presentation:

1. **User messages** render as a left **accent bar** (`▌`), left-aligned in the same
   column as the assistant `●` — a single left-aligned conversation column, not
   right-side chat bubbles.
2. **Thinking tokens** are shown (they are currently discarded) as an
   always-visible dim block above the assistant narration.
3. The **input box** is 2 lines tall, has no `> ` prompt, carries the same `▌`
   accent bar, and shows a dim placeholder.

This is purely a TUI rendering + input change. The loop/event layer already emits
thinking as `content.ThinkingChunk` (stream) and `content.ThinkingBlock` (final
message); today's TUI simply drops it.

## Changes

### User messages — `tui/render.go`
`renderRow` `RoleUser` renders each (width-wrapped) line of the user text prefixed
with a styled `▌ `, left-aligned. The existing `rowSep` ("\n\n") provides the
blank-line margin. Replaces the current bold-only `UserStyle`.

### Thinking — `tui/screen.go`, `tui/render.go`, block rendering
- `liveSegment` gains a `thinking string` accumulator.
- `handleEvent`'s `TokenDelta` case, which currently skips non-text chunks, appends
  `*content.ThinkingChunk` text to `live.thinking` and refreshes.
- `liveNonEmpty` and the live-render guard include `thinking != ""`.
- `commitLive` converts accumulated thinking into a leading
  `*content.ThinkingBlock` on the committed assistant row — one uniform
  representation for both the streamed path and the final-message fallback
  (`handleTurnDone` via `ev.Message.Blocks`).
- `renderAssistant(thinking, text, calls, …)` renders, in order: the thinking block
  (if any), the markdown narration, then the tool cards.
- `renderThinking(s, width)` renders a faint `thinking` header followed by
  `│ `-prefixed, width-wrapped, dim/italic lines.
- `assistantText` skips `ThinkingBlock` (so thinking is not also markdown-rendered);
  `thinkingText(blocks)` extracts it. Fixes the latent `[unsupported block]`
  rendering for `ThinkingBlock`.

### Input box — `tui/components/input.go`
- `inputHeight` 3 → 2.
- Prompt `"> "` → accent bar `"▌ "` (dim; brighter on focus).
- Placeholder `"Type a message…"`.
- `reservedLines` 4 → 3 (status 1 + input 2) in `tui/render.go` so the viewport
  height budget stays correct.

### Shared styles — `tui/styles/styles.go`
- Accent bar glyph `▌` + bar style (shared by input prompt and user messages).
- `ThinkingStyle` = faint + italic.

## Testing (test-first)

- `components/input_test.go`: height = 2; prompt is not `"> "`; placeholder set;
  prompt is the accent bar.
- `render_test.go`: user row contains `▌`; `renderThinking` emits `thinking` + `│`
  lines; an assistant row carrying a `ThinkingBlock` shows the thinking text and
  never `[unsupported block]`; existing user/assistant substring assertions still
  hold.
- `screen_test.go`: a `ThinkingChunk` `TokenDelta` accumulates into the live segment
  and renders; replace any assertion that thinking is skipped.

## Out of scope

Loop/event changes; enabling extended thinking (a model `ThinkingBudget` toggle);
collapsible/ephemeral thinking (chose always-visible); mouse support.

## Implementation deltas (emerged during build/debug)

- **Input frame:** kept the `▌` accent bar (an interim full-width "black box" was
  tried and reverted — it looked wrong while typing). Also cleared the bubbles
  textarea's default focused `CursorLine` background (`"0"`), which showed as a stray
  dark patch. `InputBox.View` clamps to exactly `inputHeight` rows.
- **Constant-height frame:** the turn status line ("thinking…") was removed and the
  history pinned to an exact line count so `Screen.View` is **always** the terminal
  height. A frame whose height fluctuated (status shown only while running) left stale
  rows that bubbletea did not clear — the "multiplying"/doubled-input artifact.
  Guards: `TestViewHeightIsConstant`, `TestViewNoLineExceedsWidth`.
- **Markdown alignment:** `NewMarkdownRenderer` uses glamour's `DarkStyleConfig` with
  the document margin zeroed; `renderMD` strips the framing and prefixes the `●` so
  narration sits flush after the bullet. One blank line separates thinking from the AI
  message; `renderThinking` trims the model's leading/trailing blank reasoning lines.
- **Library logging off-screen (separate but related):** `go-tdx-guest` calls
  `logger.Init(os.Stdout)` at init, so its warnings printed onto the TUI. Handled in
  `internal/ttylog` (capture stdout+stderr to `~/.urvi/urvi.log`, give bubbletea its
  own terminal handle) — not in this package.
