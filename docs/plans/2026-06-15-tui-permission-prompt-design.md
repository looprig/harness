# TUI Permission-Prompt & AskUser-Prompt Rendering — Design

Date: 2026-06-15 · Status: design (authoritative spec for the follow-up impl plan)

**Goal:** Render the loop's interactive prompts — `event.PermissionRequested` (approve a
gated tool call) and `event.UserInputRequested` (answer an `AskUser` question) — in the
TUI, and dispatch the user's decision back through the already-wired
`agent.Approve / agent.Deny / agent.ProvideAnswer` trio. This closes the one gap left by
the tools subsystem: today the loop **blocks on a permission/AskUser gate** while the TUI
silently drops the event, so any gated tool (`agents/coding`'s `WriteFile`/`EditFile`/
`Bash`/`Fetch`/`WebSearch`, or any `AskUser`) **freezes the UI** on a `⋯` card with no way
forward.

**Scope:** This is the "separate follow-up doc" deferred by `tools-design.md` §5d and
`tui-tool-use-design.md` ("permission-prompt / AskUser-prompt rendering is a separate
doc — not in scope here"). It is purely a `tui/` extension: a CallID-keyed prompt queue,
its modal rendering and key handling, and the bounded dispatch commands. **No loop, event,
session, or agent changes** — the producing events (`event/tool.go`), the gate plumbing
(`loop/gate.go`, `runner.go`, `listen`), and the `tui.Agent` trio (`tui/agent.go`,
delegating to `session.Approve/Deny/ProvideUserInput`) all already exist and are tested.

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

## §1 — Display/queue model

Prompts are a **FIFO queue keyed by `CallID`, not a single slot** (§5d). Several
`AskUser` gates can be open simultaneously during a parallel tool batch (§2c), so a single
pending slot would drop one. Permission gates resolve sequentially and won't pile up, but
routing both kinds by `CallID` through one queue makes them uniform and collision-free.

`tui/screen.go` `Screen` gains one field:

```go
pending []prompt   // FIFO; head (index 0) is the active prompt. nil/empty = none.
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
    freeText bool   // set at ENQUEUE when len(Choices)==0: the input box captures the answer
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

Construction happens in `handleEvent` (below). The `prompt` carries the **already-safe,
full-fidelity** strings the stream delivered (`Description()` is what the user must read to
approve — it legitimately shows the command/path/URL; it is the *sink* projection that
drops it, not the stream). The queue holds no loop handle — answers are dispatched by
`CallID` through the `agent` trio.

`tui/agent.go`'s `Agent` interface is unchanged (it already has the trio).

---

## §2 — Event handling (enqueue)

`handleEvent` (`tui/screen.go:115`) gains two cases; both enqueue and re-render, then
`return readNext(m.reader)` as every case does (so the stream keeps draining — the loop is
*not* blocked on the stream, it is blocked on the gate, which the user's keypress will
release):

```go
case event.PermissionRequested:
    m.pending = append(m.pending, prompt{
        CallID: ev.CallID, Kind: promptPermission,
        ToolName: ev.Request.ToolName(), Description: ev.Request.Description(),
        Scopes: ev.Request.AllowedScopes(),
    })
    m.refreshHistory()

case event.UserInputRequested:
    m.pending = append(m.pending, prompt{
        CallID: ev.CallID, Kind: promptUserInput,
        Question: ev.Question, Choices: ev.Choices,
        freeText: len(ev.Choices) == 0, // no choices → free-text mode (matches the tool contract)
    })
    m.refreshHistory()
    m.resizeHistory() // a prompt opening changes the height budget (§3)
```

(`PermissionRequested` likewise calls `m.resizeHistory()` after enqueue, and the
terminal-event clear calls it after emptying `pending`, so the history viewport always
reflects the current prompt's height — see §3.)

**The queue is cleared on every terminal event.** `TurnDone`/`TurnFailed`/
`TurnInterrupted` set `m.pending = nil` (alongside the existing `m.live = liveSegment{}`
reset). The loop tears down all gates at turn end (`listen` clears `pendingGates`), so a
stale prompt for a finished turn must never linger or be answerable. Order: clear `pending`
in the same handlers that already reset `live`.

**Defensive:** ignore a `PermissionRequested`/`UserInputRequested` whose `CallID` is
already in `pending` (duplicate) — append-once. A `CallID` collision across kinds cannot
happen (the runner mints a fresh `CallID` per call).

---

## §3 — Rendering (the prompt box)

A prompt is **modal**: while `len(m.pending) > 0`, the **head** (`m.pending[0]`) renders as
a box occupying the input region (between the transcript viewport and the status line),
and the normal input box is hidden/disabled — except the `promptUserInput` "other…"
free-text sub-state, which re-enables the input box for typing (§4). This keeps the
transcript (and its in-progress tool cards from `tui-tool-use-design.md`) visible above
while the decision is pending. Use the existing width-aware render helpers; styling via a
new `styles.PromptStyle` (bordered/emphasised, distinct from the faint tool cards).

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
- If `len(m.pending) > 1`, append a faint `(+N more pending)` line so the user knows more
  prompts follow.

**AskUser box** (`promptUserInput`):

```
┌─ Question ─────────────────────────────────────────┐
│ <Question>                     (wrapped, width-aware)
│                                                    │
│ [1] <choice0>   [2] <choice1>   …   [o] other…     │
└────────────────────────────────────────────────────┘
```

- **With choices**: a numbered key list (`[1]`…`[9]`) plus `[o] other`. Selecting `[o]`
  sends the literal `"other"` (the contract escape hatch — §1); there is no custom-text
  capture in the choices case.
- **No choices (`freeText == true`)**: render the question above the (re-enabled) input box;
  the user types the answer and `Enter` submits it. No choice list, no `[o]`. This is the
  only path that sends typed free text, and the tool accepts it (no-choices → any answer).
- Only ≤9 choices get number keys; if a tool ever supplies >9 choices, the impl plan should
  decide (paginate or letter-key) — `AskUser` callers today supply small lists.

`renderMessages`/the screen's `View` composition: when `len(pending) > 0`, render
`renderPrompt(m.pending[0], width)` in place of the idle input box. The status line
(`StatusRunning`) can show `awaiting approval` / `awaiting input` so the state is
unambiguous.

### §3a — Layout & height budgeting (variable prompt height)

The current layout reserves a FIXED 4 lines below the history viewport — `reservedLines =
1 (status) + 3 (input box)` — and `historyHeight() = height − reservedLines −
panelHeight()` (`tui/screen.go:16,624`; input box fixed at 3, `tui/components/input.go:8`).
A bordered prompt with a wrapped `Description`/`Question` + a key-hint line is **taller than
3 and variable**, so it cannot reuse the fixed `reservedLines` budget — without accounting,
the prompt would overflow into / be clipped against the history viewport.

The prompt is rendered **in place of** the input box (the input is hidden while a non-free-
text prompt is active; in free-text mode the input box IS the prompt's entry field — see
§4), so the budget swaps the input's 3 lines for the prompt's measured height. Mirror the
existing `panelHeight()` pattern (`screen.go:615`, which already measures the slash-complete
panel via `lipgloss.Height`):

```go
// promptHeight returns the rendered height of the active prompt box, or 0 when none.
func (m Screen) promptHeight() int {
    if len(m.pending) == 0 { return 0 }
    return lipgloss.Height(renderPrompt(m.pending[0], m.width))
}

// historyHeight: status (1) is always reserved; the input region is EITHER the 3-line
// input box (no prompt) OR the measured prompt box (prompt active). panelHeight unchanged.
func (m Screen) historyHeight() int {
    inputRegion := inputBoxLines // 3
    if ph := m.promptHeight(); ph > 0 {
        inputRegion = ph // prompt replaces the input box
    }
    return max(0, m.height-statusLines-inputRegion-m.panelHeight())
}
```

- `resizeHistory()` is called whenever `pending` changes (enqueue in §2, pop in §4, clear in
  §7) and on `WindowSizeMsg`, so the viewport always reflects the current prompt height.
- **Cap the prompt height** so a pathologically long `Description`/`Question` cannot eat the
  screen: the prompt box wraps to `width` and caps its body at `min(measured, height/2)` (or
  a fixed max), truncating the body with a `… (truncated)` marker beyond the cap. The full
  text remains in `Description`/`Question` (and, for a tool call, is what the human approves
  — if it is genuinely huge that is itself a signal). A scrollable prompt body is out of
  scope (§9).
- Free-text mode: the prompt renders the question line(s) AND keeps the input box as its
  entry field; `promptHeight()` then measures both, so the budget still holds.
- Extract `statusLines = 1` and `inputBoxLines = 3` as named constants (replacing the lumped
  `reservedLines = 4`) so the two regions are budgeted independently. Existing call sites
  that used `reservedLines` (no prompt) compute the same `1 + 3 = 4` and stay behavior-
  identical when no prompt is open.

---

## §4 — Input handling (modal key routing)

When `len(m.pending) > 0`, `handleKey` (`tui/screen.go:373`) routes keys to the **head
prompt first**, before the normal bindings — except `ctrl+c` (quit) and `ctrl+t` (toggle
tool previews) stay global. This is the modal switch; it sits at the top of `handleKey`:

```go
if len(m.pending) > 0 && key != "ctrl+c" && key != "ctrl+t" {
    return m.handlePromptKey(key)
}
```

`handlePromptKey(key)` on the head:

**`promptPermission`:**
- `y` → if `ScopeOnce` ∈ Scopes: dispatch `approveCmd(headCallID, ScopeOnce)`, pop head.
- `s` → if `ScopeSession` ∈ Scopes: `approveCmd(headCallID, ScopeSession)`, pop.
- `w` → if `ScopeWorkspace` ∈ Scopes: `approveCmd(headCallID, ScopeWorkspace)`, pop.
- `n` (and `esc`) → `denyCmd(headCallID)`, pop.
- a key not offered by this prompt's `Scopes` → ignored (no-op, re-render).

**`promptUserInput` — with choices (`!freeText`):**
- `1`…`9` → if the index is within `Choices`: `provideAnswerCmd(headCallID, Choices[i])`, pop.
- `o` → `provideAnswerCmd(headCallID, "other")` — the literal contract escape hatch (§1).
  **No free-text capture here** (an unlisted typed string would fail the tool's
  `validateAnswer` and surface as a tool-result error).
- `esc` → interrupt the turn (`AskUser` has no "deny"; the gate releases via ctx-cancel →
  `TurnInterrupted` → queue cleared, §7).
- any other key → no-op.

**`promptUserInput` — free-text (`freeText`, no choices):**
- the input box is the prompt's entry field; printable keys type into it (route them to the
  input box from `handlePromptKey`). `enter` → if the typed text is non-empty,
  `provideAnswerCmd(headCallID, typed)`, clear the box, pop; an empty `enter` is ignored
  (re-prompt). `esc` → interrupt the turn (as above). The tool accepts free text in the
  no-choices case, so the typed answer is always valid.

**Pop = reveal next.** Popping the head (`m.pending = m.pending[1:]`, then `resizeHistory()`
since the height budget changed, §3a) re-renders; if another prompt remains its box renders
next. Answers are **fire-and-route** (no ack — see §5), so the pop is immediate/optimistic;
the bounded command reports only transport failure.

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
(distinct `CallID`, §2c) and each emit a `UserInputRequested` — so `pending` holds both.
The TUI renders the head, the user answers it (dispatched with the head's `CallID`), pops,
and the second renders. Because routing is by `CallID` (not arrival order at the loop), the
answers reach the correct gates even though the user answers them in queue order. Permission
gates are sequential so at most one permission prompt is open at a time, but it shares the
same queue uniformly. The `(+N more pending)` hint (§3) tells the user when siblings wait.

---

## §7 — Edge cases & invariants

- **Terminal clears the queue.** `TurnDone`/`TurnFailed`/`TurnInterrupted` → `pending = nil`
  (and exit free-text mode, restore the input box). The loop has torn down its gates, so a
  late keypress against a cleared queue is a no-op.
- **Interrupt during a prompt.** `Ctrl+C`/`Esc`-interrupt while a prompt is open: the
  existing interrupt path cancels the turn → the loop's gate waits unblock via `ctx.Done()`
  → `TurnInterrupted` → queue cleared. (Deny-on-interrupt is not required; the gate's
  ctx-cancel already releases the runner, which rolls the turn back.) `Esc` semantics:
  with a permission prompt active, `Esc` = deny the head (explicit); the existing
  `Esc`-clears-queue / interrupt behavior applies only when no prompt is active. Specify
  this precedence in the impl.
- **`ctrl+t`** stays global (toggle tool-result expansion) even with a prompt open — it
  only re-renders.
- **`ctrl+c`** stays global (quit).
- **Status.** Prompts occur while `StatusRunning` (a turn is in flight). They do not change
  `Status`; the status line *label* may read `awaiting approval`/`awaiting input` for
  clarity, derived from `len(pending) > 0`.
- **Queue indices.** `pending` is independent of `messages`/`queue` (the *input* queue) —
  no interaction with the existing queued-input `DisplayIndex` bookkeeping.
- **No prompt for an auto-approved tool.** AutoApprove tools never emit `PermissionRequested`
  (the gate isn't opened), so they never enqueue — only the cards (`tui-tool-use-design.md`)
  render. (`personal-assistant` only gates Fetch/WebSearch; `coding` gates the write/exec
  tools.)

---

## §8 — Testing (table-driven, `-race`, synthetic events)

The producing events already exist, so — like the tool-card work — this is unit-tested with
**synthetic `event.PermissionRequested`/`event.UserInputRequested`** fed through `Update`
(no live loop), plus a **fake `tui.Agent`** recording the trio calls.

- **Enqueue:** a synthetic `PermissionRequested` enqueues a `promptPermission` with the
  right `ToolName`/`Description`/`Scopes` (built via a fake `tool.PermissionRequest` whose
  `AllowedScopes()` varies — all-three vs once-only); `UserInputRequested` → `promptUserInput`
  with the choices. Two `UserInputRequested` (distinct `CallID`) → two pending, head first.
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
  typed text — so the answer always passes `tools/askuser.go validateAnswer`. (A test that
  feeds the sent answer through the real `validateAnswer` with the prompt's choices and
  asserts it returns "" is the tightest guard.)
- **Height budgeting (§3a):** with a prompt active, `historyHeight()` shrinks by the
  prompt's measured height (not the input's 3 lines); `resizeHistory` runs on enqueue / pop
  / terminal-clear; a prompt taller than the cap is truncated and `historyHeight()` never
  goes negative (floored at 0). Drive a `WindowSizeMsg` + a synthetic prompt and assert the
  resulting viewport height.
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

A final manual smoke check (the one deferred in `tui-tool-use-impl.md`): run `agents/coding`
on a prompt that writes a file / runs `Bash`, confirm the approval box renders, `y`/`n`
work, the tool then proceeds and its card resolves, and an `AskUser` tool shows the question
box. This is the integration the tool-card doc could not exercise without this work.

---

## §9 — Out of scope (this iteration)

- **Per-prompt cursor / scrolling within a prompt**, mouse selection (keyboard-only, head-of-
  queue is the focus — matches the `Ctrl+T`-global-toggle stance of the tool-card doc).
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
- **A scrollable prompt body.** A prompt whose `Description`/`Question` exceeds the height
  cap (§3a) is truncated with a marker, not scrolled.

---

## Suggested execution order (for the follow-up impl plan)

1. `prompt` model + `Screen.pending` field (§1) — additive, no behavior.
2. `handleEvent` enqueue cases (set `freeText` for no-choices) + terminal-clear (§2) —
   synthetic-event tests.
3. `tui/commands.go` `approve/deny/provideAnswer` bounded cmds + `promptResultMsg` (§5).
4. `handlePromptKey` modal routing — permission (`y/s/w/n`), AskUser-with-choices (number /
   literal `o` / `esc`-interrupt), AskUser-free-text (input box / `enter` / `esc`) (§4),
   with the contract-conformance test.
5. Layout split: `statusLines`/`inputBoxLines` consts + `promptHeight()` + `historyHeight()`
   accounting + `resizeHistory()` on prompt open/close, with the height cap (§3a).
6. `renderPrompt` (permission box + AskUser choices box + free-text box) + `styles.PromptStyle`
   + `View` composition (§3).
7. Manual smoke check against `agents/coding` (§8).

Each step is one TDD task (failing test → minimal impl → `-race` → commit), keeping the
existing `tui/` tests green throughout (the modal switch changes key routing only while a
prompt is open).
