# TUI Interaction Architecture & Permission-Prompt Rendering — Design

Date: 2026-06-15 · Status: design (authoritative spec for the follow-up impl plan)

**Goal:** Bring the TUI under a modular in-package architecture that can correctly handle
input placement, prompt modality, mouse scrolling, tool cards, and thinking-token display,
then render the loop's interactive prompts — `event.PermissionRequested` (approve a gated
tool call) and `event.UserInputRequested` (answer an `AskUser` question) — and dispatch
the user's decision back through the already-wired
`agent.Approve / agent.Deny / agent.ProvideAnswer` trio.

This closes the one gap left by the tools subsystem: today the loop **blocks on a
permission/AskUser gate** while the TUI silently drops the event, so any gated tool
(`agents/coding`'s `WriteFile`/`EditFile`/`Bash`/`Fetch`/`WebSearch`, or any `AskUser`)
**freezes the UI** on a `⋯` card with no way forward. It also corrects the architectural
drift that caused the current TUI to treat transcript reconstruction, status, layout,
input editing, slash completion, tool expansion, and prompt routing as one large
`Screen` responsibility.

**Scope:** This is the "separate follow-up doc" deferred by `tools-design.md` §5d and
`tui-tool-use-design.md` ("permission-prompt / AskUser-prompt rendering is a separate
doc — not in scope here"), expanded into the TUI interaction architecture that prompt
support needs. The chosen architecture is **option 2: split focused helpers inside the
existing `tui` package**. Do not create `tui/internal/...` packages for this iteration.

The change remains a `tui/` extension: transcript display helpers, a bottom interaction
controller, layout measurement/routing helpers, a CallID-keyed prompt queue, modal
rendering and key handling, thinking-token rendering, mouse routing, and bounded dispatch
commands. **No loop, event, session, or agent changes** — the producing events
(`event/tool.go`), the gate plumbing (`loop/gate.go`, `runner.go`, `listen`), and the
`tui.Agent` trio (`tui/agent.go`, delegating to
`session.Approve/Deny/ProvideUserInput`) all already exist and are tested.

**Tech stack:** Go, Bubbletea/lipgloss (already in `tui/`), stdlib. No new dependencies.

**References:** `tools-design.md` §5d (the prompt-queue contract, copied/expanded below),
§2c (why several `AskUser` gates can be open at once), §5c (the session/agent trio);
`tui-tool-use-design.md` (the tool-card rendering this composes with). Cited as **§N**.

---

## Background — why the UI hangs today

A gated tool call drives this sequence (verified against the shipped code):

1. The runner resolves permission **sequentially** (`runner.go`); on `EffectAsk` it opens a
   `gatePermission` gate (`loop/gate.go`), emits `event.PermissionRequested{CallID,
   Request}` on the per-turn stream, and **blocks** on the gate's reply channel until an
   `ApproveToolCall`/`DenyToolCall` arrives (or `ctx` cancels). `AskUser` is analogous via
   `loop.RequestUserInput` → `event.UserInputRequested{CallID, Question, Choices}` →
   `gateUserInput`.
2. The events ride the **full-fidelity per-turn stream** the TUI already drains
   (`readNext` → `eventMsg` → `handleEvent`). (`SinkProjection` redacts only the sink
   path; the stream the TUI reads is un-redacted — confirmed in `loop.go`'s `emit`.)
3. `tui/screen.go handleEvent` (`:115`) has **no case** for `PermissionRequested` or
   `UserInputRequested`. The `switch` falls through to `return readNext(m.reader)`, which
   pulls the *next* event — but none arrives, because the loop is parked on the gate
   awaiting a reply the TUI never sends. Result: a frozen `⋯` card.

The decision channel already exists end-to-end: `tui.Agent` exposes
`Approve(ctx, callID, scope) error` / `Deny(ctx, callID) error` /
`ProvideAnswer(ctx, callID, answer) error` (`tui/agent.go`), both manifest wrappers
delegate to `session.Approve/Deny/ProvideUserInput`, and the session routes the command
to the parked gate by `CallID`+kind (`listen`). **Only the TUI half — render the prompt,
read a keypress, call the trio — is missing.** This doc specifies that half.

---

## Architecture decision — split helpers inside `package tui`

The public TUI boundary is already the right shape and should stay stable:

- `tui.Agent` is the narrow dependency the UI needs.
- `tui.OpenAgent` keeps process/session setup outside the UI.
- `tui.Screen` remains the Bubble Tea root model.
- `cmd/cli` remains the composition root that wires the agent registry and Bubble Tea
  program options.

The problem is internal: `Screen` currently has too many reasons to change. It owns agent
streaming, transcript reconstruction, live tool-card state, queued user messages, slash
completion, text editing, status labels, layout math, and rendering coordination. The
follow-up implementation should split these responsibilities into small helpers in the
same `tui` package. This keeps churn low, avoids premature package boundaries, and still
brings the code back toward the AGENTS.md SOLID requirements.

Recommended file/model split:

- `screen.go` — root orchestration only. Owns the agent/open-agent lifecycle, active
  stream reader, high-level status, and Bubble Tea `Init`/`Update`/`View`. It routes events
  and executes typed UI actions; it should not contain prompt-specific, transcript-specific,
  or layout-specific branching beyond delegation.
- `transcript.go` — display transcript model. Applies loop events to display state:
  user/assistant messages, live assistant segments, tool calls, queued-message markers,
  terminal turn events, and thinking chunks/blocks. This replaces ad hoc transcript updates
  scattered through `handleEvent`.
- `interaction.go` — bottom interaction controller. Owns the single textarea-backed editor,
  slash completion, prompt modality, prompt queue, draft preservation, and key handling for
  the bottom surface. It returns typed actions (`submit`, `runSlash`, `approve`, `deny`,
  `answer`, `interrupt`, `noop`) for `Screen` to execute.
- `prompt.go` — prompt view-models plus prompt-specific rendering/key helpers. Permission,
  AskUser-with-choices, and AskUser-free-text live here. This file should not call the
  agent directly; it only returns actions.
- `layout.go` — pure frame calculation and mouse-region routing. Given terminal dimensions
  and measured bottom surfaces, it returns transcript/status/prompt/input/slash regions and
  viewport heights. It replaces `reservedLines` with measured regions.
- `status.go` — status-label derivation. Session status remains owned by `Screen`; the
  displayed label is derived from session status plus interaction state (`streaming`,
  `thinking`, `awaiting approval`, `awaiting input`, `interrupted`, `failed`).
- `render.go` — rendering primitives for transcript items, assistant segments, thinking
  blocks, tool cards, and shared styles. Rendering should consume view-model snapshots, not
  mutate state.
- `commands.go` — bounded Bubble Tea command factories for stream reads, submit,
  interrupt, approve, deny, and provide-answer.

This is deliberately **not** a `tui/internal/...` package split in this iteration. The
desired modularity is achieved by small types, focused files, and narrow method surfaces
inside `package tui`. If the package later becomes too large, these helpers will already
define the extraction boundaries.

The root update loop should follow one flow:

1. Receive `tea.Msg`.
2. Apply global commands first (`ctrl+c`, tool expansion toggle, thinking toggle).
3. Route `tea.MouseMsg` through `layout` to either the transcript viewport or active prompt
   body.
4. Route key input to `interaction.Update`.
5. Execute the typed action returned by `interaction`.
6. Apply stream events to `transcript` and enqueue prompt events into `interaction`.
7. Call one layout synchronization step that measures the rendered bottom surfaces,
   resizes the transcript viewport, and refreshes viewport content.

That final synchronization point is important: layout should not be fixed in several
places (`handleEvent`, `handleKey`, `View`, and resize handlers). A single post-update
layout pass prevents prompt boxes, slash completion, and the input editor from fighting
for the same rows.

---

## §1 — Display/queue model

Prompts are a **FIFO queue keyed by `CallID`, not a single slot** (§5d). Several
`AskUser` gates can be open simultaneously during a parallel tool batch (§2c), so a single
pending slot would drop one. Permission gates resolve sequentially and won't pile up, but
routing both kinds by `CallID` through one queue makes them uniform and collision-free.

The prompt queue belongs to the bottom interaction controller, not directly to the root
screen. `Screen` holds the controller as a helper field:

```go
type Screen struct {
    // existing agent/session/reader lifecycle fields
    transcript  transcriptModel
    interaction interactionModel
    layout      layoutModel
}
```

`interactionModel` owns the editor, slash completion, active mode, and prompt queue:

```go
type interactionMode uint8
const (
    modeCompose interactionMode = iota
    modePermissionPrompt
    modeChoicePrompt
    modeAnswerPrompt
)

type interactionModel struct {
    mode    interactionMode
    pending []prompt // FIFO; head (index 0) is active. nil/empty = no prompt.

    input         components.Input
    slashComplete components.SlashComplete
    composeDraft  string // restored after a free-text AskUser answer.
}
```

The `prompt` view-model (new `tui/prompt.go`):

```go
type promptKind uint8
const ( promptPermission promptKind = iota; promptUserInput )

type prompt struct {
    CallID uuid.UUID
    Kind   promptKind

    // promptPermission (built from event.PermissionRequested.Request — a tool.PermissionRequest):
    ToolName    string            // Request.ToolName()    — box header
    Description string            // Request.Description()  — box body (the Bash command / file path / URL the user approves)
    Scopes      []tool.ApprovalScope // Request.AllowedScopes() — which of y/s/w to offer

    // promptUserInput (built from event.UserInputRequested):
    Question string
    Choices  []string
    selected int    // highlighted choice index (cursor); ↑/↓ move it, Enter picks it
    freeText bool   // set at ENQUEUE when len(Choices)==0: the input box captures the answer
    scrollTop int    // first visible choice/body row when the prompt is taller than its region
}
```

**AskUser answer contract (must match `tools/askuser.go validateAnswer`).** The TUI is
constrained by the *existing* tool contract, which is the source of truth:
`validateAnswer(answer, choices)` accepts — **with choices**: exactly one listed choice OR
the literal string `"other"`; **with no choices**: any free text (including empty). So the
TUI's two modes map 1:1 onto the contract:
- **No choices (`freeText`)** → a free-text answer is valid → the input box captures it and
  `ProvideAnswer(typed)` is sent. (The TUI requires non-empty for usability — a UI guard,
  not a contract requirement; the tool would accept empty.)
- **With choices** → the answer must be a listed choice or the literal `"other"`. Number
  keys send the selected choice; `[o]` sends the **literal `"other"`** (the contract's
  escape hatch — "none of these"). The TUI does **not** capture custom free text in this
  case, because an unlisted typed string would fail `validateAnswer` and surface as a
  tool-result error. (See §9 for the optional AskUser amendment that would lift this.)

Construction happens from stream events routed by `Screen` into `interaction.EnqueuePrompt`
(below). The `prompt` carries the **already-safe, full-fidelity** strings the stream
delivered (`Description()` is what the user must read to approve — it legitimately shows
the command/path/URL; it is the *sink* projection that drops it, not the stream). The queue
holds no loop handle — answers are dispatched by `CallID` through typed actions that the
root screen maps to the `agent` trio.

`tui/agent.go`'s `Agent` interface is unchanged (it already has the trio).

The transcript model should stop depending on raw display indexes for mutable state such
as queued user messages. Use stable display IDs generated by `transcriptModel` instead:

```go
type displayID uint64

type transcriptItem struct {
    ID displayID
    // user/assistant/tool/thinking view data
}
```

Queued input should record the display ID it created, not a slice index. That keeps queue
rollback and interrupt cleanup correct after prompts, thinking blocks, or future transcript
rows are inserted above or between existing messages.

---

## §2 — Event handling (enqueue + transcript updates)

`Screen.handleEvent` should become a router, not the owner of every event-specific state
transition. It should always keep returning `readNext(m.reader)` after processing stream
events, as it does today, so the per-turn stream keeps draining. The loop is *not* blocked
on the stream; it is blocked on the gate, which the user's keypress will release.

Event routing:

```go
func (m Screen) handleEvent(ev event.Event) (tea.Model, tea.Cmd) {
    var actions []uiAction

    var transcriptActions []uiAction
    m.transcript, transcriptActions = m.transcript.ApplyEvent(ev)
    actions = append(actions, transcriptActions...)

    var interactionActions []uiAction
    m.interaction, interactionActions = m.interaction.ApplyEvent(ev)
    actions = append(actions, interactionActions...)

    m.status = m.status.ApplyEvent(ev, m.interaction.State())

    m.syncLayout()
    return m, tea.Batch(readNext(m.reader), m.commandsFor(actions)...)
}
```

The exact method signatures can differ, but the dependency direction should not: helpers
return view state and typed UI actions; helpers do not call the agent/session directly.

`interaction.ApplyEvent` handles the two prompt events:

```go
case event.PermissionRequested:
    m.enqueue(prompt{
        CallID: ev.CallID, Kind: promptPermission,
        ToolName: ev.Request.ToolName(), Description: ev.Request.Description(),
        Scopes: ev.Request.AllowedScopes(),
    })

case event.UserInputRequested:
    m.enqueue(prompt{
        CallID: ev.CallID, Kind: promptUserInput,
        Question: ev.Question, Choices: ev.Choices,
        freeText: len(ev.Choices) == 0, // no choices → free-text mode (matches the tool contract)
    })
```

Enqueue switches `interaction.mode` to the prompt type represented by the head of the
queue. If the head is a free-text AskUser prompt, the controller preserves the current
compose draft and reuses the single input editor as the answer editor.

`transcript.ApplyEvent` owns display reconstruction:

- `TurnStarted` starts a new assistant live segment when appropriate.
- `TokenDelta` applies `content.TextChunk` to the live text segment.
- `TokenDelta` applies `content.ThinkingChunk` to the live thinking segment; it is no
  longer skipped.
- `ToolCallStarted`/`ToolCallCompleted` update tool-call view state.
- `TurnDone` commits final assistant blocks, including `content.ThinkingBlock`.
- `TurnFailed`/`TurnInterrupted` mark the live segment terminal and clear queued display
  markers for the interrupted turn.

**The prompt queue is cleared on every terminal event.** `TurnDone`/`TurnFailed`/
`TurnInterrupted` call `interaction.ClearPrompts()` and restore compose mode. The loop
tears down all gates at turn end (`listen` clears `pendingGates`), so a stale prompt for a
finished turn must never linger or be answerable.

**Defensive:** ignore a `PermissionRequested`/`UserInputRequested` whose `CallID` is
already pending — append-once. A `CallID` collision across kinds cannot happen (the runner
mints a fresh `CallID` per call).

---

## §3 — Rendering (the prompt box)

A prompt is **modal**: while `interaction.ActivePrompt() != nil`, the **head** prompt
renders in the bottom interaction region between the transcript viewport and the status
line. The normal compose input is hidden/disabled for permission prompts and
AskUser-with-choices prompts. For an AskUser **free-text** prompt (no choices), the same
single input editor is reused as the prompt's entry field (§4).

This keeps the transcript and in-progress tool cards visible above while the decision is
pending, without creating multiple input boxes. Use width-aware render helpers; styling via
a new `styles.PromptStyle` (bordered/emphasised, distinct from the faint tool cards).

**Permission box** (`promptPermission`):

```
┌─ Approve tool call ───────────────────────────────┐
│ <ToolName>                                         │
│ <Description>                  (wrapped, width-aware)
│                                                    │
│ [y] allow once   [s] session   [w] workspace   [n] deny │
└────────────────────────────────────────────────────┘
```

- The key hints are **derived from `Scopes`** (`Request.AllowedScopes()`): show `[y] allow
  once` iff `ScopeOnce` ∈ Scopes, `[s] session` iff `ScopeSession`, `[w] workspace` iff
  `ScopeWorkspace`. `[n] deny` is **always** shown. (Per §3a, persistable tools offer all
  three; `UnknownRequest` offers only `ScopeOnce` → only `[y]`+`[n]`.)
- `Description` is the full command/path/URL — that is the point of the prompt; it is
  shown to the human to read before approving (it is the *sink* that redacts it, never the
  stream/TUI).
- If `interaction.PendingCount() > 1`, append a faint `(+N more pending)` line so the user
  knows more prompts follow.

**AskUser box** (`promptUserInput`):

```
┌─ Question ─────────────────────────────────────────┐
│ <Question>                     (wrapped, width-aware)
│                                                    │
│ [1] <choice0>   [2] <choice1>   …   [o] other…     │
└────────────────────────────────────────────────────┘
```

- **With choices**: a vertical, **cursor-navigable** list (the highlighted row is
  `prompt.selected`) plus `[o] other`. `↑`/`↓` move the highlight and `Enter` picks it, so
  **any number of choices is reachable** — `AskUser` imposes no length limit
  (`tools/askuser.go`), so a `[1]`–`[9]`-only scheme would strand a 10th choice. `[1]`…`[9]`
  remain as **accelerators** for the first nine rows. Selecting `[o]` sends the literal
  `"other"` (the contract escape hatch — §1); no custom-text capture in the choices case.
  If the list exceeds the prompt's height budget (§3a), the rendered window **scrolls with
  the cursor** (a viewport over the choices only — not a transcript cursor, §9), so every
  choice stays reachable.
- **No choices (`freeText == true`)**: render the question above the (re-enabled) input box;
  the user types the answer and `Enter` submits it. No choice list, no `[o]`. This is the
  only path that sends typed free text, and the tool accepts it (no-choices → any answer).

`Screen.View` composition should ask helpers for view fragments:

```go
func (m Screen) View() string {
    transcript := m.transcript.View(m.layout.TranscriptRegion())
    bottom := m.interaction.View(m.layout.BottomRegion())
    status := m.status.View(m.interaction.State())
    return m.layout.Join(transcript, bottom, status)
}
```

The code does not need to use these exact method names, but `View` should compose already
prepared helper views rather than reaching into prompt/editor internals. The status line
can show `awaiting approval` / `awaiting input` while the session status remains running.

### §3a — Layout & height budgeting (variable prompt height)

The current layout reserves a fixed 4 lines below the history viewport — `reservedLines =
1 (status) + 3 (input box)` — and `historyHeight() = height - reservedLines -
panelHeight()` (`tui/screen.go:16,624`; input box fixed at 3, `tui/components/input.go:8`).
A bordered prompt with a wrapped `Description`/`Question`, a choice list, and key hints is
taller than 3 and variable, so it cannot reuse the fixed `reservedLines` budget.

Replace fixed budgeting with `layoutModel`, a pure calculator. It takes terminal size and
measured bottom surfaces, then returns regions:

```go
type layoutSurfaces struct {
    StatusHeight int
    SlashHeight  int
    InputHeight  int
    PromptHeight int
}

type layoutFrame struct {
    Transcript region
    Prompt     region
    Input      region
    Slash      region
    Status     region
}
```

Rules:

- Status is always reserved.
- Slash completion is reserved only while visible.
- In compose mode, the bottom interaction region is the normal input editor.
- In permission/choice prompt mode, the prompt replaces the normal input editor.
- In free-text AskUser mode, the prompt includes the question body plus the same single
  input editor as the answer field.
- The transcript viewport receives all remaining rows, floored at 0.

`Screen` should call a single `syncLayout()` after any state transition that can affect
height: `WindowSizeMsg`, prompt enqueue/pop/clear, slash-completion visibility, input
height changes, tool/thinking expansion toggles, and terminal turn events. `syncLayout()`
measures the rendered bottom fragments, computes a frame, resizes the transcript viewport,
and refreshes viewport content.

Prompt height must be capped so a long command, URL, diff summary, or question cannot
consume the whole screen. Unlike the earlier narrow prompt design, this cap should not make
content unreachable. The active prompt owns a small viewport or scroll window over its body
or choice list:

- Permission prompt: header/actions stay visible; the description body scrolls when it
  exceeds the prompt body region.
- AskUser with choices: the choice list scrolls with `selected`, so rows 10+ remain
  reachable by keyboard.
- AskUser free-text: the question body scrolls above the answer editor if needed.

Mouse wheel events are routed by `layout.HitTest(y)`:

- wheel over transcript region → transcript viewport scrolls;
- wheel over active prompt body/list → prompt body/list scrolls;
- wheel over status/input/action rows → no scroll unless the active interaction mode
  explicitly handles it.

`cmd/cli` must enable Bubble Tea mouse reporting, using the existing Bubble Tea dependency
only. The implementation should use `tea.WithMouseCellMotion()` unless testing shows
another Bubble Tea option is required for wheel events on the supported terminal targets.

Extract `statusLines = 1` and `inputBoxLines = 3` as named constants (replacing the lumped
`reservedLines = 4`) for compatibility during the refactor. New layout code should prefer
measured surfaces over fixed constants, but these constants document the no-prompt baseline
and keep existing tests understandable.

### §3b — Thinking-token rendering

Thinking data already exists in the loop stream and final assistant message blocks. The TUI
should stop treating it as out of scope.

Display model:

```go
type assistantSegment struct {
    Thinking string
    Text     string
    Tools    []toolCallView
}
```

`transcriptModel.ApplyEvent` appends `content.ThinkingChunk` to the live assistant
segment's `Thinking` field and commits final `content.ThinkingBlock` values on `TurnDone`.
`render.go` renders thinking as a dim assistant sub-block above the answer text or tool
calls. The exact visual treatment can be restrained, but it must be visible when enabled
and it must not appear as `[unsupported block]`.

Controls:

- Keep `ctrl+t` for tool-result expansion.
- Add a separate thinking visibility toggle, for example `ctrl+r` or another key chosen in
  the implementation plan after checking current bindings.
- Status may say `thinking` while only thinking chunks have arrived, but status is not a
  substitute for rendering the thinking content itself.

Layout impact: expanding/collapsing thinking changes transcript content height, not the
bottom interaction height. It should trigger the same transcript refresh path as tool-card
expansion.

---

## §4 — Input handling (modal key routing)

`Screen.handleKey` should keep only global bindings and delegate the bottom-surface keys
to `interaction.Update`. This prevents prompt-specific behavior from spreading through the
root model.

```go
func (m Screen) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
    switch msg.String() {
    case "ctrl+c":
        return m, tea.Quit
    case "ctrl+t":
        m.transcript.ToggleToolExpansion()
        m.syncLayout()
        return m, nil
    case thinkingToggleKey:
        m.transcript.ToggleThinking()
        m.syncLayout()
        return m, nil
    }

    var action uiAction
    m.interaction, action = m.interaction.Update(msg)
    m.syncLayout()
    return m, m.commandFor(action)
}
```

`uiAction` should be a typed union-like struct or small interface local to `tui`, with
variants for submit, slash command, approve, deny, answer, interrupt, and no-op. It must
not use `any` for payloads.

Interaction modes:

**`modeCompose`:**
- printable keys update the single input editor;
- `enter` submits the composed user message;
- slash-completion keys update/select slash entries;
- normal history scrolling keys continue to target the transcript viewport;
- prompt-specific keys are ignored because no prompt is active.

**`modePermissionPrompt`:**

- `y` → if `ScopeOnce` ∈ Scopes: dispatch `approveCmd(headCallID, ScopeOnce)`, pop head.
- `s` → if `ScopeSession` ∈ Scopes: `approveCmd(headCallID, ScopeSession)`, pop.
- `w` → if `ScopeWorkspace` ∈ Scopes: `approveCmd(headCallID, ScopeWorkspace)`, pop.
- `n` (and `esc`) → `denyCmd(headCallID)`, pop.
- a key not offered by this prompt's `Scopes` → ignored (no-op, re-render).

**`modeChoicePrompt` (`promptUserInput` with choices):**

- `↑`/`↓` → move `prompt.selected` (clamp to `[0, len(Choices))`), re-render; no dispatch.
- `enter` → `provideAnswerCmd(headCallID, Choices[selected])`, pop.
- `1`…`9` → accelerator: if the index is within `Choices`, `provideAnswerCmd(headCallID,
  Choices[i])`, pop. (Rows 10+ are reached via `↑`/`↓`+`enter`.)
- `o` → `provideAnswerCmd(headCallID, "other")` — the literal contract escape hatch (§1).
  **No free-text capture here** (an unlisted typed string would fail the tool's
  `validateAnswer` and surface as a tool-result error).
- `esc` → interrupt the turn (`AskUser` has no "deny"; the gate releases via ctx-cancel →
  `TurnInterrupted` → queue cleared, §7).
- any other key → no-op.

**`modeAnswerPrompt` (`promptUserInput` free-text, no choices):**

- the input box is the prompt's entry field; printable keys type into it (route them to the
  same editor used by compose mode);
- `enter` → if the typed text is non-empty, `provideAnswerCmd(headCallID, typed)`, clear
  the answer text, pop; an empty `enter` is ignored (re-prompt);
- `esc` → interrupt the turn (as above);
- the tool accepts free text in the no-choices case, so the typed answer is always valid.

When entering `modeAnswerPrompt`, save the compose draft before repurposing the editor. On
answer, interrupt, or terminal clear, restore the saved compose draft. This is what
prevents multiple input boxes while also avoiding accidental loss of the user's partially
typed normal message.

**Pop = reveal next.** Popping the head updates `interaction.mode` from the next prompt, or
back to `modeCompose` if the queue is empty, then `Screen.syncLayout()` re-renders. Answers
are **fire-and-route** (no ack — see §5), so the pop is immediate/optimistic; the bounded
command reports only transport failure.

While a *choices/permission* prompt is active the input box is hidden and `Enter` does not
submit it; in *free-text* mode the input box IS the prompt entry. Normal `Enter`/queue
behavior resumes once `pending` is empty.

---

## §5 — Dispatch commands (bounded `tea.Cmd`)

Three new bounded commands in `tui/commands.go`, mirroring `interruptTurn` (`:43`) exactly
— a bounded ctx, the agent call, a result msg — so the Update loop **never blocks** on the
send (`loop.Commands` is unbuffered; the session selects on `ctx.Done()`/loop `Done`):

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

`promptResultMsg{err}` is handled in `Update`: on `err == nil` nothing more to do (the gate
was released; the runner proceeds and the next events — `ToolCallStarted`/`Completed` or
the tool's result — arrive on the stream). On `err != nil` (the loop exited / ctx done —
e.g. the turn was interrupted between enqueue and keypress) surface a faint error line; the
prompt was already popped, and a terminal event will clear any siblings. **A dispatch error
is not fatal** — it means the gate is gone, which the terminal-event queue-clear also
covers.

Rationale for pop-before-confirm (optimistic): the command is fire-and-route with no
meaningful success payload; blocking the UI on the round-trip (or re-rendering the box
until an ack) would reintroduce the very hang this doc removes. The bounded timeout + the
terminal-event clear make a lost/late dispatch self-healing.

---

## §6 — Parallel AskUser & queue dynamics

Two (or more) `AskUser` calls in one parallel batch each open their own `gateUserInput`
(distinct `CallID`, §2c) and each emit a `UserInputRequested` — so
`interaction.pending` holds both.
The interaction controller renders the head, the user answers it (dispatched with the
head's `CallID`), pops, and the second renders. Because routing is by `CallID` (not arrival
order at the loop), the answers reach the correct gates even though the user answers them
in queue order. Permission gates are sequential so at most one permission prompt is open at
a time, but it shares the same queue uniformly. The `(+N more pending)` hint (§3) tells the
user when siblings wait.

---

## §7 — Edge cases & invariants

- **Terminal clears the queue.** `TurnDone`/`TurnFailed`/`TurnInterrupted` →
  `interaction.ClearPrompts()` (clears `interaction.pending`, exits free-text mode, and
  restores the compose draft/input box). The loop has torn down its gates, so a late
  keypress against a cleared queue is a no-op.
- **Interrupt during a prompt.** `Ctrl+C`/`Esc`-interrupt while a prompt is open: the
  existing interrupt path cancels the turn → the loop's gate waits unblock via `ctx.Done()`
  → `TurnInterrupted` → queue cleared. (Deny-on-interrupt is not required; the gate's
  ctx-cancel already releases the runner, which rolls the turn back.) `Esc` semantics:
  with a permission prompt active, `Esc` = deny the head (explicit); the existing
  `Esc`-clears-queue / interrupt behavior applies only when no prompt is active. Specify
  this precedence in the impl.
- **`ctrl+t`** stays global (toggle tool-result expansion) even with a prompt open — it
  only re-renders.
- **Thinking toggle** stays global, but separate from `ctrl+t`.
- **`ctrl+c`** stays global (quit).
- **Status.** Prompts occur while `StatusRunning` (a turn is in flight). They do not change
  `Status`; the status line *label* may read `awaiting approval`/`awaiting input` for
  clarity, derived from `interaction.State()`.
- **Queue indices.** Prompt queue state is independent of transcript display state. Queued
  user messages should reference stable `displayID` values from `transcriptModel`, not
  slice indexes, so transcript insertions for thinking/tool/prompt-adjacent display do not
  corrupt rollback.
- **Mouse routing.** A prompt opening must not steal transcript scroll permanently. Mouse
  wheel events are routed by the current `layoutFrame`: transcript rows scroll transcript;
  prompt body/list rows scroll the prompt; action/status/input rows do not scroll unless
  explicitly handled.
- **No prompt for an auto-approved tool.** AutoApprove tools never emit `PermissionRequested`
  (the gate isn't opened), so they never enqueue — only the cards (`tui-tool-use-design.md`)
  render. (`personal-assistant` only gates Fetch/WebSearch; `coding` gates the write/exec
  tools.)

---

## §8 — Testing (table-driven, `-race`, synthetic events)

The producing events already exist, so — like the tool-card work — this is unit-tested with
synthetic events and Bubble Tea messages, not a live loop. Prefer helper-level tests first,
then a smaller set of `Screen.Update` orchestration tests:

- `interactionModel` tests cover prompt queueing, mode transitions, key routing, slash
  suppression during prompts, free-text draft preservation, and typed actions.
- `transcriptModel` tests cover text chunks, thinking chunks/blocks, tool-call updates,
  stable display IDs, queued-message rollback, and terminal events.
- `layoutModel` tests cover measured bottom surfaces, prompt/input/slash/status regions,
  viewport heights, and mouse hit testing.
- `Screen.Update` tests cover wiring: synthetic stream events reach the right helpers, typed
  actions become bounded commands, and global keys stay global.

Use a fake `tui.Agent` recording the trio calls for dispatch tests.

- **Enqueue:** a synthetic `PermissionRequested` enqueues a `promptPermission` with the
  right `ToolName`/`Description`/`Scopes`. **`tool.PermissionRequest` is a SEALED interface
  (unexported `permissionRequest()` marker) — a `tui` fake CANNOT implement it.** Use the
  concrete exported requests: `tool.BashRequest{Command: …}` (its `AllowedScopes()` is all
  three: Once/Session/Workspace) for the all-scopes box, and `tool.UnknownRequest{Tool: …,
  Summary: …}` (`AllowedScopes()` is `ScopeOnce` only) for the once-only box — construct the
  event as `event.PermissionRequested{CallID: …, Request: tool.BashRequest{…}}`.
  `UserInputRequested` → `promptUserInput` with the choices. Two `UserInputRequested`
  (distinct `CallID`) → two pending, head first.
- **Key dispatch (permission):** `y`/`s`/`w` → `agent.Approve(headCallID, ScopeOnce/Session/
  Workspace)`; a scope key NOT in `Scopes` is a no-op; `n`/`esc` → `agent.Deny(headCallID)`;
  the head pops after each.
- **Key dispatch (AskUser, with choices):** a number key → `agent.ProvideAnswer(headCallID,
  Choices[i])`, pop; `o` → `agent.ProvideAnswer(headCallID, "other")` — assert it sends the
  **literal `"other"`, not typed text**; `esc` → interrupts the turn (no `ProvideAnswer`).
- **Key dispatch (AskUser, no choices / free-text):** the input box captures text; `enter`
  → `agent.ProvideAnswer(headCallID, typed)`, pop; an empty `enter` is ignored (no call);
  `esc` → interrupt.
- **Contract conformance (the §2-finding regression):** assert that for a with-choices
  prompt the TUI only ever sends a listed choice or the literal `"other"` — never arbitrary
  typed text. `validateAnswer` is **unexported** in package `tools`, so the `tui` test
  asserts the PUBLIC invariant directly (the answer the fake `Agent` received is a member of
  `Choices` or equals `"other"`); it does NOT call `validateAnswer` cross-package. The
  end-to-end "AskUser actually accepts this" guard lives in a `tools` test (same package, so
  `validateAnswer`/`InvokableRun` are reachable): drive `AskUser.InvokableRun` via its
  `requestUserInput` seam returning a TUI-style answer (a listed choice, then `"other"`) and
  assert a non-error tool-result.
- **>9 choices reachable (the §4-finding regression):** a `UserInputRequested` with 12
  choices → `↑`/`↓`+`enter` selects the 10th–12th (which have no number key); `[1]`…`[9]`
  still accelerate the first nine; `selected` clamps at both ends; the rendered window
  scrolls so a highlighted row past the height budget stays visible.
- **Height budgeting (§3a):** with a prompt active, the transcript region shrinks by the
  measured prompt height (not the input's 3 lines); `syncLayout()` runs on enqueue / pop /
  terminal-clear; a prompt taller than the cap gets a scrollable body/list and transcript
  height never goes negative. Drive a `WindowSizeMsg` + a synthetic prompt and assert the
  resulting frame.
- **Mouse routing (§3a):** with mouse enabled, a wheel event over the transcript region
  scrolls transcript; a wheel event over a prompt body/list scrolls that prompt; a wheel
  event over status/action/input rows does not mutate transcript scroll.
- **Thinking rendering (§3b):** a `content.ThinkingChunk` appears in the live assistant
  segment; a final `content.ThinkingBlock` renders as thinking content rather than
  `[unsupported block]`; the thinking visibility toggle changes transcript rendering but
  does not affect tool expansion.
- **Single editor / draft preservation:** entering free-text AskUser mode saves the compose
  draft, reuses the same editor for the answer, then restores the draft after answer,
  interrupt, or terminal clear. With permission/choice prompts active, no second editor is
  rendered.
- **Modal routing:** with a prompt open, normal bindings (plain text, `enter`-submits-input,
  `up`/`down` history) are suppressed; `ctrl+c`/`ctrl+t` still act.
- **Pop reveals next:** answer head → second prompt renders; `(+N more pending)` count is
  correct.
- **Terminal clears:** `TurnDone`/`TurnFailed`/`TurnInterrupted` empties `pending` and exits
  free-text mode; a key after clear is a no-op.
- **Dispatch error path:** a fake agent returning an error from `Approve` → `promptResultMsg`
  surfaces a faint error line; the prompt is still popped; no panic, no hang (bounded ctx).
- **Render:** golden-ish assertions that the permission box shows only the offered scope keys
  + `[n]`; the AskUser box shows the numbered choices + `[o]`; width-aware wrap of a long
  `Description`/`Question`.

A final manual smoke check (the one deferred in `tui-tool-use-impl.md`): first verify the
CLI registry does not register the default assistant under the same name as the coding
agent. Then run `agents/coding` on a prompt that writes a file / runs `Bash`, confirm the
approval box renders, `y`/`n` work, the tool then proceeds and its card resolves, mouse
scroll works in transcript and prompt regions, thinking appears when enabled, and an
`AskUser` tool shows the question box. This is the integration the tool-card doc could not
exercise without this work.

---

## §9 — Out of scope (this iteration)

- **A transcript / per-card cursor** (selecting past tool cards or scrollback messages to
  act on them) and **mouse selection/click actions**. Mouse wheel routing is in scope
  (§3a), but clicking transcript cards or prompt buttons is not. The AskUser *choice list*
  IS cursor-navigable (§3/§4), but that highlight + its viewport live INSIDE the active
  prompt box only; there is no cursor over the scrollback transcript. Only the
  head-of-queue prompt is interactive.
- **A "remember for all tools" / batch-approve** affordance (each call is approved
  individually; that is the security posture — a human reads each `Description`).
- **Editing/replaying** a denied call from the transcript (deny is terminal for that call;
  the model sees the tool-result error and may retry).
- **Rich diff rendering inside the WriteFile/EditFile approval box** beyond the
  `Description()` the request already carries (the tool's `BuildRequest` decides what the
  prompt shows; richer previews are a tool-side change, not a TUI one).
- **Custom free text *alongside* choices (OPTIONAL follow-up — a tool change).** With
  choices present, `[o]` sends the literal `"other"`; the user cannot type a custom answer,
  because `tools/askuser.go validateAnswer` rejects unlisted text. Supporting "pick a choice
  OR type your own" requires a small, deliberate `AskUser` contract amendment — e.g.
  `validateAnswer` accepting the typed text as the free-text "other" value, or an
  `allow_other_text` arg. That is out of this tui-only doc (it changes a tool's validation
  contract); call it out so the choice is conscious. Free text already works for a
  no-choices question.

---

## Suggested execution order (for the follow-up impl plan)

1. Fix/verify the CLI registry precondition so `agents/coding` resolves to the coding
   agent during manual TUI smoke tests.
2. Add helper skeletons inside `package tui`: `transcriptModel`, `interactionModel`,
   `layoutModel`, status-label helper, typed `uiAction`, and focused tests. Keep behavior
   unchanged initially.
3. Move transcript reconstruction into `transcriptModel`, including stable display IDs,
   tool-call state, text chunks, thinking chunks, and final `ThinkingBlock` rendering.
4. Move composer/slash handling into `interactionModel` in `modeCompose`, still behavior-
   compatible with the current input flow.
5. Add prompt queueing and modal interaction modes: permission, AskUser choices, AskUser
   free-text with draft preservation. Cover key routing and AskUser answer contract tests.
6. Add `tui/commands.go` `approve/deny/provideAnswer` bounded cmds + `promptResultMsg`
   (§5), and map typed interaction actions to commands in `Screen`.
7. Replace fixed `reservedLines` layout with `layoutModel` + `syncLayout()`, including
   prompt height caps, prompt body/list scrolling, and mouse hit testing. Enable Bubble Tea
   mouse reporting in `cmd/cli`.
8. Update `Screen.View` to compose transcript, interaction, slash, prompt, and status
   fragments from helper snapshots.
9. Run the manual smoke check against `agents/coding` (§8).

Each step is one TDD task (failing test → minimal impl → `-race` → commit), keeping the
existing `tui/` tests green throughout (the modal switch changes key routing only while a
prompt is open).
