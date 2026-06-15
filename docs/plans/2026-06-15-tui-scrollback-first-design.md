# TUI Scrollback-First Architecture — Design

Date: 2026-06-15 · Status: **authoritative spec for the follow-up impl plan**

## Goal

Make Urvi's default TUI behave like a coding-agent terminal app: styled output in the
terminal's **native scrollback**, native text selection/copy, native paste, and a single
active composer at the bottom. On the same foundation, close the functional gaps the
current TUI has: gated tools that **freeze the UI**, a composer hard-capped at two lines,
and a status line that is never shown.

This replaces the fullscreen-viewport approach in
`2026-06-15-tui-permission-prompt-design.md`. That design is retained only as the basis for
a possible future `urvi --fullscreen` opt-in mode; it is **not** the default architecture.

### Why scrollback-first (decision rationale)

The fullscreen/alt-screen route forces a choice the terminal will not let you have both
ways: mouse reporting ON gives in-app wheel scroll but breaks native click-drag copy
(needs a Shift/Option modifier); OFF gives native copy but no in-app scroll. OpenAI's Codex
CLI took the alt-screen route and has spent a year retrofitting back toward native
scrollback — `--no-alt-screen` alone did not fix it, because a full-screen *renderer* still
redraws the main buffer instead of appending; the real fix is an append-only renderer
(ratatui `insert_before`, the analogue of Bubble Tea's `tea.Println`). Choosing
scrollback-first as the **default** keeps both futures open (fullscreen can still be added
behind a flag); choosing fullscreen-first forecloses the native future or makes it
expensive to retrofit. Scroll and copy then work out of the box because the app never
competes with the terminal for the mouse.

## Decision

Default TUI mode is **scrollback-first**, rendered **commit-at-boundary** (Approach A):

- `cmd/cli` creates the Bubble Tea program **without** alt-screen and **without** mouse mode.
  Today it uses `tea.NewProgram(screen, tea.WithAltScreen())` (`cmd/cli/main.go:120`); the
  new call is `tea.NewProgram(screen)`.
- During a turn, live assistant output (thinking + text + in-flight tool cards) renders only
  in the **active surface** at the bottom. Nothing is written to scrollback mid-stream.
- Committed transcript entries are printed to native scrollback **exactly once** via
  `tea.Println`, at a **boundary**: `TurnDone` / `TurnFailed` / `TurnInterrupted`, when a
  permission/AskUser prompt opens, or (for the user's own message) on submit.

`tea.Println`/`tea.Printf` print unmanaged lines above the program and persist across
renders **only in normal-screen mode** — they are no-ops under alt-screen. Dropping
`tea.WithAltScreen()` is therefore a precondition, not a nicety. `styles.go` already pins
glamour to a static `dark` theme with no OSC-11 queries, so markdown rendering stays safe in
normal-screen mode.

A future fullscreen viewport mode may exist behind an explicit `urvi --fullscreen` flag, and
mouse reporting only inside that mode. Both are out of scope here.

## Scope

In scope (confirmed):

1. Scrollback-first rendering foundation (drop alt-screen; print to native scrollback).
2. **Permission + AskUser prompt** rendering and dispatch (the freeze fix).
3. **Auto-growing input box** (replace the fixed 2-line composer).
4. **Thinking + status-line** polish.

No loop, event, session, or agent changes. The producing events
(`event.PermissionRequested`, `event.UserInputRequested`), the gate plumbing, and the
`tui.Agent` trio (`Approve`/`Deny`/`ProvideAnswer`, `tui/agent.go:26/29/33`) all already
exist and are tested. Only the TUI half is missing.

Out of scope: a copy-on-hotkey affordance (native selection already copies in scrollback
mode), fullscreen mode as default, mouse reporting, retroactive collapse/expand of printed
scrollback, rich transcript cursor selection, and changing the `AskUser` contract to allow
custom free text alongside choices.

## Background — why the UI freezes today

`tui/screen.go handleEvent` has **no case** for `PermissionRequested` or
`UserInputRequested`; the `switch` falls through to `return readNext(m.reader)`, pulling the
next event — but none arrives, because the loop is parked on the gate awaiting a reply the
TUI never sends. Any gated tool (`agents/coding`'s `WriteFile`/`EditFile`/`Bash`/`Fetch`/
`WebSearch`, or any `AskUser`) freezes on a `⋯` card. The decision channel already exists
end-to-end; this design supplies the missing render-prompt → read-key → call-trio half.

## Architecture — split helpers inside `package tui`

The public boundary stays stable: `tui.Agent`, `tui.OpenAgent`, `tui.Screen`, `cmd/cli` as
composition root. `Screen` sheds the responsibilities it has accreted, and the
`viewport`-based history is retired in favour of native scrollback.

| File | Action | Owns |
|---|---|---|
| `screen.go` | slim down | Root orchestration only: agent/reader lifecycle, `Init`/`Update`/`View`, event routing, executing typed actions. |
| `transcript.go` | new | `transcriptModel`: `committed []entry` + `live` segment + `nextID displayID`. `ApplyEvent` does all reconstruction. |
| `scrollback.go` | new | `scrollbackModel`: committed entries → `printAction`s; records printed `displayID`s so each prints exactly once. |
| `interaction.go` | new | `interactionModel`: single editor, slash completion, interaction mode, prompt FIFO queue, draft preservation, active-surface key routing; returns typed `uiAction`s. |
| `prompt.go` | new | `prompt` view-models (permission + AskUser) and their render/key helpers. Returns actions; never calls the agent. |
| `surface.go` | new | Computes the **active surface only**: capped live tail + prompt/composer + slash + one status line. No transcript viewport. |
| `status.go` / `statusline.go` | keep + wire | Derive the status label from session status + interaction state; `RenderStatusLine` (today dead) is called by `surface.go`. |
| `render.go` | keep | ANSI/lipgloss for entries, segments, tool cards, thinking, prompts, status. Consumes snapshots, mutates nothing. |
| `commands.go` | extend | Add bounded `approveCmd`/`denyCmd`/`provideAnswerCmd` + `promptResultMsg`, and the `tea.Println`-based print-action command. Keep `readNext`/`interruptTurn`/`reopenAgent`/`closeAgent`. |
| `components/history.go` | retire (default) | The `viewport` history is unused in scrollback mode — the terminal owns history. Kept in tree only for a possible future `--fullscreen`. |
| `components/input.go` | modify | Auto-growing composer (below). Replaces fixed `inputHeight = 2`. |
| `cmd/cli/main.go` | modify | `tea.NewProgram(screen)` — drop `tea.WithAltScreen()`, no mouse option. Keep the `ttylog` stderr redirect (now *more* important: stray stderr would corrupt live scrollback). |

This is **not** a `tui/internal/...` package split; modularity comes from small types and
focused files inside `package tui`.

## Output model & active surface (commit-at-boundary)

Two models, one rule:

> The active surface is **mutable** — redrawn every frame, free to show `⋯`, a moving tail,
> a highlighted choice. Scrollback is **append-only** — an entry prints once, when it is
> *final*, and is never rewritten.

```go
// transcript.go
type displayID uint64

type transcriptModel struct {
    committed []entry      // finalized rows; scrollbackModel prints each once
    live      liveSegment  // in-progress assistant output — active surface ONLY
    nextID    displayID
}

type entry struct {
    ID   displayID
    Kind entryKind  // user | assistant | tool | promptRecord | system | error | interrupted
    view entryView  // rendered-ready data (blocks / toolCallView / prompt context)
}

type liveSegment struct {
    Thinking string
    Text     string
    Calls    []toolCallView  // shown as ⋯ in the active surface; NOT committed until terminal
    active   bool
}
```

**What commits when** — the whole of Approach A:

| Trigger | Commits to scrollback |
|---|---|
| User submits | the user message — immediately (one entry) |
| `ToolCallCompleted` / cancelled | that tool card, **once**, in its terminal state (`✓`/`✗`/`⊘`) |
| Prompt opens | the live **prose + thinking so far** as an entry, **plus** the prompt context as a `promptRecord` entry (copyable command/question + all choices) |
| `TurnDone` / `TurnFailed` / `TurnInterrupted` | any remaining live prose/thinking |

A tool call appears in scrollback **once**, at resolution — never as a mutating `⋯→✓` line.
The in-progress `⋯` lives only in the active surface. A single turn may yield several
committed prose entries (before-tool, after-tool); they read naturally in order.

```go
// scrollback.go
type printAction struct {
    EntryID displayID
    Lines   []string   // fully rendered, width-aware
}

type scrollbackModel struct {
    printed map[displayID]bool
    width   int
}

// Flush emits a print action for every committed entry not yet printed.
func (s scrollbackModel) Flush(committed []entry, render func(entry) []string) (scrollbackModel, []printAction)
```

The print is a bounded `tea.Cmd` in `commands.go` that calls `tea.Println` once per flush
(joining the pending entries' lines). `printed[displayID]` guarantees a resize, re-render, or
duplicate event never reprints a committed entry.

**Spacing.** Every transcript entry renders with **one trailing blank line**, so consecutive
entries (user message, assistant segment, each tool card, prompt record) are separated by
exactly one blank line in both scrollback and the live tail — messages breathe instead of
crowding. The blank line is part of the entry's rendered output (counted in its
`printAction.Lines`), so the print-once guarantee covers it too; within a single assistant
segment, thinking → text → tool cards stay grouped.

**Update flow** (replacing today's `handleEvent` + `commitLive`):

1. `tea.Msg` → `transcript.ApplyEvent(ev)` mutates `live` or appends a committed entry on a
   boundary / terminal tool state.
2. `interaction.ApplyEvent(ev)` enqueues prompt events.
3. `scrollback.Flush(...)` → `[]printAction` → one batched `tea.Println` command.
4. `View` re-renders the active surface only.

**Active surface (`surface.go` → `Screen.View`)** renders, top to bottom, only: the capped
live tail (most recent N lines of live thinking/text/tool `⋯`); a full-width **separator
rule**; the **bottom box** (composer, prompt control, or answer field by mode); slash
completion (when visible); one status line. No `viewport`, no `historyHeight()` — the
terminal owns history, so the active surface never needs an internal scroll region.

**Stable IDs fix a latent bug:** today's queued-input cleanup keys off `messages` slice
indexes (`DisplayIndex`), which is brittle. Queued inputs now reference `displayID`, so
inserting prompt records or thinking rows between messages cannot desync interrupt/rollback.

## Input box (auto-growing composer)

Today `components/input.go:12` locks the composer to two visible rows (`inputHeight = 2`),
then `Height(inputHeight).MaxHeight(inputHeight)` in `View`; multi-line text is accepted but
invisible past line 2. Scrollback-first makes the fix easy — there is no `viewport`
competing for rows.

```go
// components/input.go
const (
    minInputLines = 1
    maxInputLines = 10   // grow 1→10 with content, then scroll internally (cursor stays visible)
)

func (b InputBox) Height() int {
    return clamp(b.ta.LineCount(), minInputLines, maxInputLines)
}
```

On every key routed to the editor, recompute and `ta.SetHeight(b.Height())`. Past
`maxInputLines` the `textarea` scrolls internally and keeps the cursor visible.

**Active-surface budgeting** replaces `reservedLines`/`historyHeight()` with:

```
statusH      = 1
sepH         = 1                              // the separator rule above the box
boxBorderH   = 2                              // top + bottom border of the bottom box
slashH       = slashComplete.Visible ? panelHeight : 0
contentH     = input.Height()                 // or the prompt-control height in prompt mode
bottomH      = sepH + boxBorderH + contentH
liveTailCap  = max(0, termHeight − statusH − slashH − bottomH)
```

As the composer grows, the live tail shrinks; lines that scroll off the tail are already
committed to scrollback at the next boundary, so nothing is lost.

**Keys:** `Enter` submits. **`Shift+Enter` inserts a newline.** (`Alt+Enter`/`Ctrl+J` are
**not** bound.)

> Known limitation: distinguishing `Shift+Enter` from `Enter` requires the terminal to
> report it via an **enhanced / Kitty keyboard protocol**. The program must request that
> protocol (the impl plan verifies the exact Bubble Tea option and minimum version). On
> terminals that do not support it (e.g. Apple Terminal), `Shift+Enter` is byte-identical to
> `Enter` and submits. This is an accepted consequence of the binding choice.

**Composer affordance — a bordered box below a separator.** The composer renders as a
bordered box pinned to the bottom of the active surface, with an explicit full-width
**separator rule** above it (chosen over a `> ` prefix and over a borderless bar). The
bottom box is **always present**; its content switches by interaction mode — composer
(compose), prompt control (permission/choice), or answer field (free-text) — so there is one
consistent bottom affordance and never a second input box. The placeholder (`Type a
message…`) shows when empty.

**Committed transcript styling is unchanged.** User messages keep the `▌ ` accent bar
(`styles.AccentBarPrompt` / `AccentBarStyle`) + bold `UserStyle`; assistant narration keeps
the `● ` `Dot` prefix; thinking/tool cards keep `ThinkingStyle`/`ToolCallStyle`/
`ToolResultStyle`. (The earlier `> `-prefix idea is dropped, and there is no `InputBoxStyle`
— the current composer already uses the `▌ ` bar, which now moves inside the box only as the
text region.) New styles needed: a `BoxStyle` border for the composer/answer box and an
emphasised `PromptBoxStyle` border for prompt mode.

**Draft handling** stays via `Value()`/`Reset()`/`SetValue()`; the same editor is reused as
the AskUser free-text answer field with save/restore of the compose draft — there is never a
second input box.

## Permission & AskUser prompts

Prompts are a **FIFO queue keyed by `CallID`**, owned by `interactionModel` (not `Screen`).
A parallel tool batch can open several `AskUser` gates at once; permission gates resolve
sequentially. Routing every prompt by `CallID` through one queue is uniform and
collision-free.

```go
// prompt.go
type promptKind uint8
const ( promptPermission promptKind = iota; promptUserInput )

type prompt struct {
    CallID      uuid.UUID
    Kind        promptKind
    ToolName    string               // permission: Request.ToolName()
    Description string               // permission: Request.Description() (full cmd/path/URL)
    Scopes      []tool.ApprovalScope // permission: Request.AllowedScopes()
    Question    string               // userInput
    Choices     []string             // userInput
    selected    int                  // choice cursor
    freeText    bool                 // set at enqueue when len(Choices)==0
}

// interaction.go
type interactionMode uint8
const ( modeCompose interactionMode = iota; modePermissionPrompt; modeChoicePrompt; modeAnswerPrompt )
// interactionModel.pending []prompt  // FIFO keyed by CallID; head = active
```

**Enqueue** (in `interaction.ApplyEvent`):

```go
case event.PermissionRequested:
    enqueue(prompt{CallID: ev.CallID, Kind: promptPermission,
        ToolName: ev.Request.ToolName(), Description: ev.Request.Description(),
        Scopes: ev.Request.AllowedScopes()})
case event.UserInputRequested:
    enqueue(prompt{CallID: ev.CallID, Kind: promptUserInput,
        Question: ev.Question, Choices: ev.Choices, freeText: len(ev.Choices) == 0})
```

**Scrollback / active-surface split** (the scrollback-first treatment): on enqueue → commit
live prose/thinking, then commit a `promptRecord` entry carrying the *full* context to
scrollback (the command / file path / URL, or the question + **all** choices) — copyable and
permanent. The **bottom box** then becomes the prompt control (emphasised `PromptBoxStyle`
border), replacing the composer; the compact control for the head prompt is:

```
┌─ Approve Bash? ──────────────────────────────────────────┐
│ [y] once   [s] session   [w] workspace   [n] deny          │
└──────────────────────────────────────────────────────────┘   (+2 more pending)
```
```
┌─ Question · choice 3/12 ─────────────────────────────────┐
│ ▸ Use repo default                                         │
│   ↑/↓ select · enter answer · 1–9 quick · o other · esc    │
└──────────────────────────────────────────────────────────┘
```

Scope hints are **derived from `Scopes`** (`[y]` iff `ScopeOnce`, `[s]` iff `ScopeSession`,
`[w]` iff `ScopeWorkspace`); `[n]` always shows. `(+N more pending)` appears when the queue
is deeper than one.

> Implementation note: the mockups above draw the header embedded in the top border (`┌─
> Approve Bash? ─┐`), but the implementation renders it as a bold first content row inside
> the box (not border-embedded) — Lipgloss v2 exposes no border-title API.

**Key routing by mode:**

| Mode | Keys |
|---|---|
| `modePermissionPrompt` | `y`/`s`/`w` → `approveCmd(head, scope)` if that scope ∈ `Scopes`, else no-op; `n`/`esc` → `denyCmd(head)`. Pop after each. |
| `modeChoicePrompt` | `↑`/`↓` move `selected` (reaches choices past 9); `enter` → `provideAnswerCmd(head, Choices[selected])`; `1`–`9` accelerate the first nine; `o` → `provideAnswerCmd(head, "other")` (literal); `esc` → interrupt turn. |
| `modeAnswerPrompt` (free-text) | reuse the single composer; `enter` (non-empty) → `provideAnswerCmd(head, typed)`; empty `enter` ignored; `esc` → interrupt. |
| `modeCompose` | unchanged; prompt keys inert. |

**AskUser answer contract** (must match `tools/askuser.go validateAnswer`, the source of
truth): with choices → only a listed choice or the literal `"other"` is ever sent (an
unlisted typed string would fail validation and surface as a tool-result error); no choices →
any free text is valid. Hence choices use selection and only the no-choices case reuses the
editor for typed text.

**Dispatch** (`commands.go`, mirroring `interruptTurn`'s bounded shape so `Update` never
blocks on the send):

```go
const promptDispatchTimeout = 2 * time.Second   // mirrors interruptTimeout

func approveCmd(ctx context.Context, agent Agent, callID uuid.UUID, scope tool.ApprovalScope) tea.Cmd {
    return func() tea.Msg {
        c, cancel := context.WithTimeout(ctx, promptDispatchTimeout); defer cancel()
        return promptResultMsg{err: agent.Approve(c, callID, scope)}
    }
}
// denyCmd → agent.Deny(c, callID); provideAnswerCmd → agent.ProvideAnswer(c, callID, answer)
```

Pop is **optimistic** (fire-and-route): pop the head immediately, do not wait for an ack —
blocking would reintroduce the hang. `promptResultMsg{err}` only surfaces a faint error line
on failure; it is never fatal (a gone gate is also covered by the terminal-event clear).

**Invariants:**

- **Terminal events clear the queue.** `TurnDone`/`TurnFailed`/`TurnInterrupted` →
  `ClearPrompts()`, exit free-text mode, restore the compose draft. The loop tears down its
  gates at turn end, so a late keypress against a cleared queue is a no-op.
- **`Esc` precedence:** permission prompt active → `Esc` = deny head; choice/free-text active
  → `Esc` = interrupt turn; no prompt active → existing interrupt/clear behaviour.
- **Append-once:** ignore a `PermissionRequested`/`UserInputRequested` whose `CallID` is
  already pending.
- **Global keys stay global** even with a prompt open: `ctrl+c` (quit), `ctrl+t` (expand
  toggle) — they only re-render.
- **Auto-approved tools never enqueue** (the gate is not opened); only their cards render.

## Thinking & status line

**Thinking — renderer unchanged, lifecycle changed.** `render.go`'s `renderThinking` already
produces a dim `thinking` header + `│ `-prefixed lines (`ThinkingStyle`, faint+italic), with
tests. During a turn, `content.ThinkingChunk` accumulates into the live segment and shows in
the capped live tail; at the boundary it commits to scrollback as part of the assistant entry
(`content.ThinkingBlock`), above the answer text.

**Expand/collapse — a single toggle.** `ctrl+t` toggles one `expand` state controlling
**both** thinking and tool output. Default **expanded** (thinking → full `│ `-prefixed body;
tool result → full runner-capped preview); `ctrl+t` collapses both (thinking → compact dim
summary line; tool result → folded preview at `previewLineCap`), and toggles back. `ctrl+r`
is not used.

**Why default expanded (revised):** scrollback is **append-only** — an entry prints once and
can never be retroactively re-rendered, so a toggle cannot expand thinking/tool output that
has already been committed to native history. Defaulting to *collapsed* would therefore
permanently truncate that history with no way to recover it. The transcript instead shows
**full** thinking + tool output by default; `ctrl+t` *reduces* verbosity for the live tail
and **future** commits only. (Real-world testing confirmed collapsed-by-default is wrong for
scrollback-first for exactly this reason.)

**Append-only consequence (stated plainly):** the toggle affects the live tail and **future**
commits only — it cannot retroactively collapse or expand text already printed to scrollback.
This is the accepted cost of scrollback-first, and the reason the default is expanded.

**Status line — finally shown.** `statusline.go`'s `RenderStatusLine` exists but `View` never
calls it. `surface.go` renders it as the single bottom line of the active surface, with the
label derived from session `Status` + interaction state:

| Condition | Label |
|---|---|
| streaming text | `streaming…` |
| only thinking chunks so far | `thinking…` |
| permission prompt active | `awaiting approval` |
| AskUser prompt active | `awaiting input` |
| interrupting / clearing | `interrupting…` / `clearing…` |
| idle | empty (composer prompt is the cue) |

Prompts occur while `Status == Running`; the label reads `awaiting approval`/`awaiting input`
for clarity without changing the underlying status. This makes the non-streaming waits
(approval, interrupt, clear) legible.

## Testing

Table-driven, `go test -race ./...`, synthetic events/messages (no live loop), per the
existing `tui/` tests. Grouped by helper:

- **`transcriptModel`** — text/thinking chunks accumulate into the live segment;
  commit-at-boundary fires on `TurnDone`/`TurnFailed`/`TurnInterrupted` **and** on
  prompt-open; a tool card commits **exactly once** at its terminal state (not as `⋯`);
  stable `displayID`s; queued-input rollback keyed by `displayID`; terminal events reset live.
- **`scrollbackModel`** — each committed `displayID` yields **one** `printAction`; duplicate
  events and resize/re-render never reprint; `Flush` returns the right lines in order.
- **`interactionModel`** — enqueue → FIFO by `CallID`; mode transitions; per-mode key
  routing; slash completion suppressed while a prompt is open; free-text draft save/restore;
  append-once on duplicate `CallID`; `ClearPrompts` on terminal events; `Esc` precedence.
- **`prompt` rendering** — permission control shows only offered scopes + `[n]`; AskUser
  shows numbered choices + `[o]`; **>9 choices reachable via `↑`/`↓`** with `1`–`9` as
  accelerators; width-aware wrap; `(+N more pending)`.
- **AskUser contract conformance (regression guard)** — with choices, the fake `Agent` only
  ever receives a listed choice or the literal `"other"`, never arbitrary typed text.
  `tool.PermissionRequest` is a **sealed** interface (unexported marker) — a `tui` fake
  cannot implement it; use the concrete exported requests: `tool.BashRequest{Command: …}`
  (`AllowedScopes()` = Once/Session/Workspace) for the all-scopes box and
  `tool.UnknownRequest{Tool: …, Summary: …}` (`AllowedScopes()` = `ScopeOnce` only) for the
  once-only box. A companion `tools`-package test drives `AskUser.InvokableRun` via its
  `requestUserInput` seam (a listed choice, then `"other"`) → non-error result.
- **Dispatch (`commands.go`)** — `approve`/`deny`/`provideAnswer` call the trio with the
  right `CallID`/scope/answer; optimistic pop; a fake agent returning an error →
  `promptResultMsg` surfaces a faint line, no panic, no hang (bounded ctx).
- **Input box** — auto-grow height clamps to `[1,10]` then scrolls; `Enter` submits,
  `Shift+Enter` inserts a newline (on a backend reporting the protocol); draft preserved
  across an AskUser free-text detour.
- **`surface` / layout** — `liveTailCap = term − status − slash − input`, floored at 0;
  status-label derivation; never negative.
- **`cmd/cli`** — program options **do not** include `tea.WithAltScreen()` or any mouse
  option.
- **`ctrl+t`** — a fresh Screen renders thinking + tool output EXPANDED by default; the first
  `ctrl+t` collapses both, the second expands again; changes live/future rendering only.

**Manual smoke** (the integration unit tests cannot cover):

1. Verify the CLI registry resolves `agents/coding` (no name collision with the default
   assistant) so the gated-tool smoke is reachable.
2. Start `urvi`; confirm it does **not** enter alt-screen and output remains visible after
   exit.
3. Submit a prompt; confirm assistant output appears in terminal scrollback at turn end.
4. Mouse-wheel scrolls native terminal history; click-drag select + copy works **without**
   holding Shift/Option; paste into the composer works.
5. Run a prompt that writes a file / runs `Bash`; confirm the approval context prints to
   scrollback (copyable) while the active surface shows the compact control; `y`/`n` work and
   the tool card resolves to `✓`/`✗`.
6. Trigger `AskUser` with >9 choices; confirm all are printed and rows beyond 9 are reachable
   by keyboard.

## Suggested execution order (for the follow-up impl plan)

Each step is one TDD task (failing test → minimal impl → `-race` → commit), keeping existing
`tui/` tests green throughout.

1. Fix/verify the CLI registry precondition so `agents/coding` resolves during manual TUI
   smoke.
2. Switch `cmd/cli` to normal-screen mode (`tea.NewProgram(screen)`), keeping the `ttylog`
   stderr redirect; assert no `tea.WithAltScreen()`/mouse options.
3. Add `transcriptModel` (committed/live + stable `displayID`s) and move event
   reconstruction into it.
4. Add `scrollbackModel` + `printAction` + the `tea.Println` print command; commit entries
   to scrollback exactly once at boundaries.
5. Refactor `Screen.View()` to render only the active surface via `surface.go` (capped live
   tail + composer + slash + status); retire the `viewport` history in default mode.
6. Add the auto-growing composer (`Shift+Enter` newline; bordered box + separator rule;
   `▌`/`●` transcript styling unchanged) and the active-surface row budget.
7. Add `interactionModel` + `prompt.go`: permission, AskUser choices, AskUser free-text with
   draft preservation; per-mode key routing; AskUser answer-contract tests.
8. Add `commands.go` `approve`/`deny`/`provideAnswer` bounded cmds + `promptResultMsg`; map
   typed interaction actions to commands in `Screen`.
9. Wire the status line (`RenderStatusLine`) and the unified `ctrl+t` expand/collapse for
   thinking + tools.
10. Run race tests, build with `CGO_ENABLED=0 go build -trimpath`, and perform the manual
    smoke checks.

## Appendix — UX reference (approved screens)

The visual contract. Above the `────` separator rule is native scrollback (scrollable,
selectable, copyable); the rule + bottom box + status line are the active surface pinned to
the bottom. User messages render `▌ <bold>`; assistant narration `● …`; thinking/tool cards
dim/faint. The bottom box is always present; its content switches by mode.

**Idle**

```
 ● Welcome to urvi. Ask me to build, edit, or run things.

 ──────────────────────────────────────────────────────────
 ┌──────────────────────────────────────────────────────────┐
 │ Type a message…                                            │
 └──────────────────────────────────────────────────────────┘
 ready
```

**Composing (multi-line; box auto-grew via `Shift+Enter`)**

```
 ──────────────────────────────────────────────────────────
 ┌──────────────────────────────────────────────────────────┐
 │ Add a --version flag that prints the build version and     │
 │ exits. Wire it before the Bubble Tea program starts.▮       │
 └──────────────────────────────────────────────────────────┘
 ready
```

**Streaming** (committed `▌` user msg in scrollback; live `●` turn in the tail above the
separator; box stays for queued input):

```
 ▌ Add a --version flag that prints the build version and exits.

 ● I'll add a --version flag to the CLI entrypoint. Let me read
   the current flag setup first.
   thinking
   │ The CLI builds the program in main; I should add the flag
   │ parse before tea.NewProgram and short-circuit on --version.

   └ ReadFile  cmd/cli/main.go  ✓
     package main
     …                           (full result; ctrl+t collapses to a fold)
 ──────────────────────────────────────────────────────────
 ┌──────────────────────────────────────────────────────────┐
 │ Type a message…  (queues while the agent works)            │
 └──────────────────────────────────────────────────────────┘
 streaming…
```

**Permission prompt** (full diff committed to scrollback; bottom box = emphasised prompt
control):

```
 ● I'll register the flag and short-circuit before the program starts.

   Approve EditFile?
   cmd/cli/main.go  ·  +7 −0
     + version := flag.Bool("version", false, "print version and exit")
     + if *version { fmt.Println(buildversion.Version()); os.Exit(0) }
 ──────────────────────────────────────────────────────────
 ┌─ Approve EditFile? ──────────────────────────────────────┐
 │ [y] once   [s] session   [w] workspace   [n] deny          │
 └──────────────────────────────────────────────────────────┘
 awaiting approval
```

**AskUser free-text** (question committed to scrollback; the *same* box is reused as the
answer field — the compose draft is saved and restored):

```
 ● What should the --version output look like? (e.g. "urvi 1.4.2")
 ──────────────────────────────────────────────────────────
 ┌─ answer ─────────────────────────────────────────────────┐
 │ urvi v1.4.2 (commit abc1234)▮                              │
 └──────────────────────────────────────────────────────────┘
 awaiting input
```

**AskUser with >9 choices** (all choices committed to scrollback; bottom box shows the
cursor + a window that scrolls with `selected`, so rows 10+ stay reachable):

```
 ● Which version string source should I use?
     1. internal/version.Version()
     …
     12. ask me each build
 ──────────────────────────────────────────────────────────
 ┌─ Question · choice 10/12 ────────────────────────────────┐
 │   9. latest git tag at runtime                             │
 │ ▸ 10. CHANGELOG.md top entry                               │
 │   11. date-based (YYYY.MM.DD)                              │
 │   ↑/↓ select · enter answer · 1–9 quick · o other · esc    │
 └──────────────────────────────────────────────────────────┘
 awaiting input
```
