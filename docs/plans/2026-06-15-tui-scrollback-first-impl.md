# TUI Scrollback-First — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Each task is TDD: write the failing test, run it red, implement the minimum, run it green, commit. Use @superpowers:test-driven-development for every task.

**Goal:** Re-render Urvi's default TUI in native terminal scrollback (commit-at-boundary), fix the permission/AskUser freeze, grow the composer, and add a status line — per `docs/plans/2026-06-15-tui-scrollback-first-design.md` (the authoritative spec; cited as **§Section**).

**Architecture:** Drop alt-screen; print finalized transcript entries to native scrollback exactly once via `tea.Println` at boundaries (TurnDone / prompt-open / submit). `Screen` becomes a thin router over focused helpers in `package tui`: `transcriptModel` (reconstruct display state), `scrollbackModel` (print-once), `interactionModel` (composer + prompt FIFO + key routing), `prompt.go` (prompt view-models), `surface.go` (active-surface layout). The composer is a bordered box below a separator rule; committed user/assistant styling (`▌`/`●`) is unchanged; every entry carries one trailing blank line.

**Tech Stack:** Go, Bubble Tea / Bubbles / Lipgloss / Glamour (already deps), stdlib. No new dependencies. Build `CGO_ENABLED=0 go build -trimpath`; test `go test -race ./...`.

**Conventions for every task:** table-driven tests with `t.Parallel()`; typed errors (no bare `errors.New` from package APIs); no `any` outside serialization; functions ≤ ~30 lines. Run `go test -race ./tui/...` after each implementation step. Commit messages end with the `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>` trailer.

---

## Task 0: Establish a clean baseline

**Files:** working tree (no code yet).

**Step 1:** Decide the fate of the pre-existing uncommitted changes to `cmd/cli/main.go`, `tui/screen.go`, `tui/render.go`, `tui/components/input.go`, `tui/styles/styles.go`, `tui/screen_test.go`, `tui/render_test.go`, `tui/components/input_test.go`, `Makefile`. Run `git status` and `git diff --stat`. Either commit them (if they belong to this feature), stash them (`git stash push -m "pre-scrollback wip"`), or discard. **Do not start Task 1 with these files dirty** — the plan rewrites them and a dirty base will corrupt every diff.

**Step 2:** Confirm green baseline: `go test -race ./...` and `CGO_ENABLED=0 go build -trimpath ./...`. Record the pass.

**Step 3:** Verify the smoke precondition (§Background, §Suggested order #1): confirm the CLI registry resolves `agents/coding` distinctly from the default assistant (grep the registry wiring in `cmd/cli/`). If it collides, note it; the manual smoke in Task 15 needs `agents/coding`.

No commit (baseline only).

---

## Task 1: Normal-screen program (drop alt-screen)

**Files:**
- Modify: `cmd/cli/main.go` (the `tea.NewProgram(screen, tea.WithAltScreen())` call, currently `:120`)
- Test: `cmd/cli/main_test.go` (create if absent)

**Step 1: Write the failing test.** Extract program-option construction into a testable, typed helper and assert the default disables alt-screen and mouse.

```go
// main_test.go
func TestDefaultTUIOptions(t *testing.T) {
    t.Parallel()
    got := defaultTUIOptions()
    if got.AltScreen {
        t.Errorf("AltScreen = true, want false (scrollback-first requires normal screen for tea.Println)")
    }
    if got.Mouse {
        t.Errorf("Mouse = true, want false (no mouse capture in scrollback-first)")
    }
}
```

**Step 2: Run red.** `go test ./cmd/cli/ -run TestDefaultTUIOptions -v` → FAIL (undefined `defaultTUIOptions`).

**Step 3: Implement.** Add a typed config and a builder; use it where the program is created.

```go
type tuiOptions struct{ AltScreen, Mouse bool }

func defaultTUIOptions() tuiOptions { return tuiOptions{AltScreen: false, Mouse: false} }

func teaProgramOptions(o tuiOptions) []tea.ProgramOption {
    var opts []tea.ProgramOption
    if o.AltScreen { opts = append(opts, tea.WithAltScreen()) }
    if o.Mouse { opts = append(opts, tea.WithMouseCellMotion()) }
    return opts
}
// at the call site:
prog := tea.NewProgram(screen, teaProgramOptions(defaultTUIOptions())...)
```

Keep the existing `ttylog` stderr redirect (it matters more now — stray stderr corrupts live scrollback).

**Step 4: Run green.** `go test -race ./cmd/cli/ -v` → PASS. Manually run `go run ./cmd/cli` briefly: confirm it does **not** switch to alt-screen and the shell prompt stays visible after `ctrl+c`.

**Step 5: Commit.** `feat(tui): normal-screen Bubble Tea program (drop alt-screen)`

---

## Task 2: `displayID` + `transcriptModel` skeleton

**Files:**
- Create: `tui/transcript.go`
- Test: `tui/transcript_test.go`

**Step 1: Write the failing test.** New transcript reconstructs a user message and assistant text from synthetic events, assigning stable IDs.

```go
func TestTranscriptApplyEvent(t *testing.T) {
    tests := []struct {
        name   string
        events []event.Event
        want   func(t *testing.T, m transcriptModel)
    }{
        {name: "text chunk accumulates into live", events: []event.Event{
            event.TurnStarted{}, textDelta("Hello "), textDelta("world"),
        }, want: func(t *testing.T, m transcriptModel) {
            if m.live.Text != "Hello world" { t.Fatalf("live text = %q", m.live.Text) }
        }},
        {name: "thinking chunk accumulates into live", events: []event.Event{
            event.TurnStarted{}, thinkingDelta("step 1"),
        }, want: func(t *testing.T, m transcriptModel) {
            if m.live.Thinking != "step 1" { t.Fatalf("live thinking = %q", m.live.Thinking) }
        }},
        {name: "TurnDone commits live to one entry with stable ID", events: []event.Event{
            event.TurnStarted{}, textDelta("done"), event.TurnDone{},
        }, want: func(t *testing.T, m transcriptModel) {
            if len(m.committed) != 1 { t.Fatalf("committed = %d, want 1", len(m.committed)) }
            if m.committed[0].ID == 0 { t.Fatal("entry has zero ID") }
            if !m.live.empty() { t.Fatal("live not reset after commit") }
        }},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            var m transcriptModel
            for _, ev := range tt.events { m, _ = m.ApplyEvent(ev) }
            tt.want(t, m)
        })
    }
}
```

Add small helpers `textDelta`/`thinkingDelta` in the test that build `event.TokenDelta` with the real `content.TextChunk`/`content.ThinkingChunk` types (check `event/` and `content/` for exact constructors — mirror today's `screen.go handleEvent`).

**Step 2: Run red.** `go test ./tui/ -run TestTranscriptApplyEvent -v` → FAIL (undefined `transcriptModel`).

**Step 3: Implement** `transcript.go` per **§Output model**: `displayID`, `entryKind`, `entry`, `liveSegment` (`Thinking`/`Text`/`Calls`/`active`, with `empty()`), `transcriptModel{committed, live, nextID}`, and `ApplyEvent(ev) (transcriptModel, []uiAction)` handling `TurnStarted`/`TokenDelta`(text+thinking)/`TurnDone`. Port the accumulation logic from the current `screen.go` `handleEvent` + `commitLive`. IDs come from `nextID++`.

**Step 4: Run green.** `go test -race ./tui/ -run TestTranscriptApplyEvent -v` → PASS.

**Step 5: Commit.** `feat(tui): transcriptModel with stable displayIDs (text/thinking/commit)`

---

## Task 3: Tool-call state + terminal events in `transcriptModel`

**Files:** Modify `tui/transcript.go`; extend `tui/transcript_test.go`.

**Step 1: Write the failing tests.** Cover: `ToolCallStarted` adds a live `⋯` card; `ToolCallCompleted` resolves it to a **committed** tool entry exactly once (terminal state, §"What commits when"); `TurnInterrupted` marks running calls cancelled and resets live; `TurnFailed` commits partial live + error entry. Use the real `event.ToolCallStarted/Completed` and `ToolStatus` enum from `tui/message.go`.

**Step 2: Run red** → FAIL.

**Step 3: Implement.** Add tool-call handling: live cards mutate in `live.Calls`; on `ToolCallCompleted`/cancel, append a committed `entry{Kind: tool}` once and remove from live. Terminal events (`TurnDone`/`TurnFailed`/`TurnInterrupted`) commit remaining live prose/thinking and reset. Return a `clearPrompts` `uiAction` on terminal events (Task 7 consumes it).

**Step 4: Run green** → PASS (`-race`).

**Step 5: Commit.** `feat(tui): tool-call + terminal-event reconstruction in transcriptModel`

---

## Task 4: `scrollbackModel` print-once + entry spacing

**Files:**
- Create: `tui/scrollback.go`
- Test: `tui/scrollback_test.go`

**Step 1: Write the failing test.**

```go
func TestScrollbackFlushPrintsEachEntryOnce(t *testing.T) {
    t.Parallel()
    entries := []entry{{ID: 1}, {ID: 2}}
    render := func(e entry) []string { return []string{"line-" + e.ID.String()} }

    s := newScrollbackModel(80)
    s, actions := s.Flush(entries, render)
    if len(actions) != 2 { t.Fatalf("first flush actions = %d, want 2", len(actions)) }
    // each entry ends with exactly one trailing blank line (§Spacing)
    if last := actions[0].Lines[len(actions[0].Lines)-1]; last != "" {
        t.Errorf("entry not blank-line terminated: %q", last)
    }
    // re-flush the same entries → nothing reprinted
    _, again := s.Flush(entries, render)
    if len(again) != 0 { t.Fatalf("second flush actions = %d, want 0 (print-once)", len(again)) }
}
```

**Step 2: Run red** → FAIL (undefined `scrollbackModel`).

**Step 3: Implement** per **§Output model** + **§Spacing**: `printAction{EntryID, Lines}`, `scrollbackModel{printed map[displayID]bool, width int}`, `newScrollbackModel(width)`, and `Flush(committed, render)` that emits one action per unprinted entry, appends one trailing `""` line, and records `printed`. Add `displayID.String()`.

**Step 4: Run green** → PASS (`-race`).

**Step 5: Commit.** `feat(tui): scrollbackModel print-once flush with one-line entry spacing`

---

## Task 5: Print command (`tea.Println`)

**Files:** Modify `tui/commands.go`; test `tui/commands_test.go`.

**Step 1: Write the failing test.** `tea.Println` output isn't unit-observable, so test the **pure line-assembly** seam: a function that turns `[]printAction` into the single string handed to `tea.Println`, joined with newlines in order.

```go
func TestPrintPayload(t *testing.T) {
    t.Parallel()
    actions := []printAction{{Lines: []string{"a", ""}}, {Lines: []string{"b", ""}}}
    got := printPayload(actions)
    want := "a\n\nb\n"
    if got != want { t.Errorf("printPayload = %q, want %q", got, want) }
}
```

**Step 2: Run red** → FAIL.

**Step 3: Implement** `printPayload([]printAction) string` (pure) and `printToScrollback(actions []printAction) tea.Cmd` returning `func() tea.Msg { return tea.Println(printPayload(actions))() }` (or `tea.Printf`). Keep `readNext`/`interruptTurn`/`reopenAgent`/`closeAgent` untouched.

**Step 4: Run green** → PASS.

**Step 5: Commit.** `feat(tui): scrollback print command via tea.Println`

---

## Task 6: Auto-growing composer box (`Shift+Enter` newline)

**Files:** Modify `tui/components/input.go`; update `tui/components/input_test.go`; add `BoxStyle`/`PromptBoxStyle`/separator helpers to `tui/styles/styles.go`.

**Step 1: Write the failing tests.** Replace the fixed-height assertions with growth:

```go
func TestInputBoxGrows(t *testing.T) {
    tests := []struct{ name, value string; wantHeight int }{
        {"empty is min", "", 1},
        {"two lines", "a\nb", 2},
        {"caps at max", strings.Repeat("x\n", 20), 10},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            b := NewInputBox(); b.Resize(60); b.SetValue(tt.value)
            if got := b.Height(); got != tt.wantHeight {
                t.Errorf("Height() = %d, want %d", got, tt.wantHeight)
            }
        })
    }
}
```

**Step 2: Run red** → FAIL (`Height` undefined / old fixed height).

**Step 3: Implement** per **§Input box**: `minInputLines=1`, `maxInputLines=10`; `Height() int` = `clamp(ta.LineCount(), min, max)`; `View()` renders the textarea inside a `styles.BoxStyle` border at `Height()` rows; bind **`Shift+Enter` → insert newline** (request the Kitty/enhanced keyboard protocol in `cmd/cli` — verify the Bubble Tea option/version; document that bare terminals fall back to submit). Keep the `▌ ` bar as the in-box text prompt. Remove the fixed-2 clamp.

**Step 4: Run green** → PASS (`-race`). Update any input tests that asserted exactly 2 lines.

**Step 5: Commit.** `feat(tui): auto-growing composer box + Shift+Enter newline`

---

## Task 7: `interactionModel` — compose mode + prompt queue

**Files:**
- Create: `tui/interaction.go`, `tui/prompt.go`, `tui/action.go` (typed `uiAction`)
- Test: `tui/interaction_test.go`, `tui/prompt_test.go`

**Step 1: Write the failing tests.** (a) compose mode: a printable key updates the editor; `enter` returns a `submit` action. (b) enqueue: a synthetic `event.PermissionRequested{CallID, Request: tool.BashRequest{Command:"go build"}}` enqueues one `promptPermission` with `Scopes == {Once,Session,Workspace}`; `event.UserInputRequested{Choices: …}` enqueues `promptUserInput`; two distinct-`CallID` user-input events → two pending, head first; duplicate `CallID` ignored (append-once). (c) clear: a terminal `uiAction` from transcript clears `pending` and restores compose draft.

Use the concrete **sealed** request types (`tool.BashRequest` → all scopes; `tool.UnknownRequest` → `ScopeOnce` only) — a fake cannot implement `tool.PermissionRequest` (§Testing).

**Step 2: Run red** → FAIL.

**Step 3: Implement** per **§Display/queue** + **§Input handling**: `interactionMode` enum, `interactionModel{mode, pending []prompt, input, slash, composeDraft}`, `prompt` struct, typed `uiAction` (submit/runSlash/approve/deny/answer/interrupt/noop — **no `any` payloads**), `ApplyEvent` (enqueue, append-once), `enqueue`/`pop`/`ClearPrompts`, and `Update(msg) (interactionModel, uiAction)` for `modeCompose`.

**Step 4: Run green** → PASS (`-race`).

**Step 5: Commit.** `feat(tui): interactionModel compose mode + CallID prompt queue`

---

## Task 8: Modal key routing (permission / choices / free-text)

**Files:** Modify `tui/interaction.go`, `tui/prompt.go`; extend `tui/interaction_test.go`.

**Step 1: Write the failing tests** (table-driven, one row per key per mode), per **§Input handling** routing table:
- permission: `y`/`s`/`w` → `approve` action with the right scope iff offered, else `noop`; `n`/`esc` → `deny`; head pops.
- choices: `↑/↓` move `selected` (reaches index 10–11 of a 12-choice prompt); `enter` → `answer(Choices[selected])`; `1`–`9` accelerate first nine; `o` → `answer("other")` (assert **literal**, not typed text); `esc` → `interrupt`.
- free-text: printable keys type into the editor; non-empty `enter` → `answer(typed)`; empty `enter` → `noop`; `esc` → `interrupt`; entering the mode saves the compose draft and restores it on answer/interrupt/clear.

**Step 2: Run red** → FAIL.

**Step 3: Implement** the per-mode branches in `interactionModel.Update`, the `selected` clamp + accelerator logic, and draft save/restore. `Esc` precedence: deny in permission mode, interrupt otherwise (§Edge cases).

**Step 4: Run green** → PASS (`-race`).

**Step 5: Commit.** `feat(tui): modal key routing for permission/choice/free-text prompts`

---

## Task 9: Dispatch commands + `promptResultMsg`

**Files:** Modify `tui/commands.go`; test `tui/commands_test.go` with a fake `Agent`.

**Step 1: Write the failing test.** A fake `Agent` records trio calls.

```go
func TestApproveCmdCallsAgent(t *testing.T) {
    t.Parallel()
    fake := &fakeAgent{}
    cmd := approveCmd(context.Background(), fake, callID, tool.ScopeSession)
    msg := cmd().(promptResultMsg)
    if msg.err != nil { t.Fatalf("err = %v", msg.err) }
    if fake.approvedScope != tool.ScopeSession || fake.approvedID != callID {
        t.Errorf("Approve(%v,%v), want (%v,%v)", fake.approvedID, fake.approvedScope, callID, tool.ScopeSession)
    }
}
// + denyCmd, provideAnswerCmd, and an error-returning fake → promptResultMsg{err!=nil}, no panic.
```

**Step 2: Run red** → FAIL.

**Step 3: Implement** per **§Dispatch**: `promptDispatchTimeout = 2*time.Second`, `approveCmd`/`denyCmd`/`provideAnswerCmd` (bounded ctx, mirror `interruptTurn`), and `promptResultMsg{err error}`.

**Step 4: Run green** → PASS (`-race`).

**Step 5: Commit.** `feat(tui): bounded approve/deny/provideAnswer dispatch commands`

---

## Task 10: Prompt + status rendering (`render.go` / `prompt.go` / `surface.go`)

**Files:** Modify `tui/render.go`, `tui/prompt.go`, `tui/statusline.go`; create `tui/surface.go`; tests `tui/prompt_test.go`, `tui/surface_test.go`.

**Step 1: Write the failing tests.** Per **§Rendering** + **§Appendix**:
- permission box shows only offered scope keys + `[n]`; `tool.UnknownRequest` → only `[y]`+`[n]`; `(+N more pending)` when queue > 1.
- AskUser box shows numbered choices + `[o]`; cursor `▸` on `selected`; window scrolls so a highlighted row past the budget stays visible.
- width-aware wrap of a long description/question.
- status label derivation table (`streaming…`/`thinking…`/`awaiting approval`/`awaiting input`/`interrupting…`/`clearing…`/idle).
- `surface` composition: capped live tail + separator rule + bottom box + status; `liveTailCap = max(0, term − statusH − slashH − (sepH+boxBorderH+contentH))`, floored at 0.

**Step 2: Run red** → FAIL.

**Step 3: Implement** the prompt-box renderers (using `styles.PromptBoxStyle`), wire `RenderStatusLine` into `surface.go`, add the layout calculator, and the bottom-box mode switch (composer/control/answer). Keep `▌`/`●`/thinking styling unchanged.

**Step 4: Run green** → PASS (`-race`).

**Step 5: Commit.** `feat(tui): prompt-box + status-line rendering and active-surface layout`

---

## Task 11: AskUser contract conformance (regression guard)

**Files:** Test `tui/interaction_test.go` (public-invariant) + `tools/askuser_test.go` (end-to-end seam).

**Step 1: Write the failing tests.** (a) In `tui`: for a with-choices prompt, assert the fake `Agent` only ever receives a listed choice or the literal `"other"` — never arbitrary typed text (§Testing; `validateAnswer` is unexported, so assert the public invariant). (b) In `tools` (same package, so `validateAnswer`/`InvokableRun` reachable): drive `AskUser.InvokableRun` via its `requestUserInput` seam returning a listed choice, then `"other"`, and assert a **non-error** tool result.

**Step 2: Run red** → FAIL (or already green for (a) if Task 8 enforced it — then this locks it).

**Step 3: Implement** only if a gap exists (the contract should already hold from Task 8). Otherwise these are pure regression locks.

**Step 4: Run green** → PASS (`-race`).

**Step 5: Commit.** `test(tui,tools): AskUser answer-contract conformance guards`

---

## Task 12: Unified `ctrl+t` expand/collapse (thinking + tools)

**Files:** Modify `tui/transcript.go` (or wherever expand state lives), `tui/render.go`; extend tests.

**Step 1: Write the failing test.** `ctrl+t` toggles one `expand` flag controlling **both** thinking and tool-result rendering; default collapsed (thinking → compact `thinking · N lines · ctrl+t` summary; tool result folded at `previewLineCap`); expanded → full both. Assert the toggle changes live/future rendering only (no reprint of committed scrollback). Remove any `ctrl+r` path.

**Step 2: Run red** → FAIL.

**Step 3: Implement** the single `expand` state + collapsed-summary renderer for thinking; route both off `ctrl+t`.

**Step 4: Run green** → PASS (`-race`).

**Step 5: Commit.** `feat(tui): unified ctrl+t expand/collapse for thinking and tools`

---

## Task 13: Rewire `Screen` (router) + `View` composition

**Files:** Modify `tui/screen.go`; update `tui/screen_test.go`; retire `tui/components/history.go` usage in default mode.

**Step 1: Write the failing tests.** `Screen.Update` orchestration (§Event handling): a synthetic stream event reaches `transcript.ApplyEvent` **and** `interaction.ApplyEvent`, the queue clears on terminal events, typed interaction actions become the right bounded commands, `readNext` keeps draining, and global keys (`ctrl+c`, `ctrl+t`) stay global. `View` composes `surface` fragments (no `viewport`).

**Step 2: Run red** → FAIL.

**Step 3: Implement.** Slim `screen.go`: remove transcript reconstruction, `commitLive`, `reservedLines`/`historyHeight`, viewport. `handleEvent` → router calling `transcript.ApplyEvent` + `interaction.ApplyEvent`, then `scrollback.Flush` → `printToScrollback`, then `syncLayout()`, returning `tea.Batch(readNext, commandsFor(actions)…)`. `handleKey` → global keys + `interaction.Update` → `commandFor(action)`. `View` → `surface.View(...)`. Map `uiAction`s to Task 9 commands.

**Step 4: Run green** → PASS (`-race`). Update/delete obsolete `screen_test.go` cases (alt-screen, viewport, index-based queue) — note each removal in the commit body.

**Step 5: Commit.** `refactor(tui): Screen as router over transcript/interaction/surface helpers`

---

## Task 14: Full suite + lint + vuln

**Files:** none (verification).

**Step 1:** `go test -race ./...` → all PASS. **Step 2:** `make secure` (vet + staticcheck + gosec + govulncheck) → clean; fix findings. **Step 3:** `CGO_ENABLED=0 go build -trimpath ./...` → builds. Commit any fixes: `chore(tui): satisfy race/lint/vuln for scrollback-first`.

---

## Task 15: Manual smoke (the integration units can't cover)

**Files:** none.

Per **§Testing → Manual smoke** and **§Appendix**:
1. `go run ./cmd/cli` — confirm **no** alt-screen; output persists after `ctrl+c`.
2. Submit a prompt → assistant output lands in native scrollback at turn end, with one blank line between messages and the `> ` -less `▌`/`●` styling.
3. Mouse-wheel scrolls native history; click-drag select + copy **without** Shift/Option; paste into the box.
4. Run `agents/coding` on a write/`Bash` prompt → approval box renders (full context copyable in scrollback), `y`/`n` work, the tool card resolves to `✓`/`✗`.
5. `AskUser` with >9 choices → all printed; rows 10+ reachable by `↑/↓`.
6. Composer grows with `Shift+Enter` (on a protocol-capable terminal) and caps at 10 lines.

Record results in the PR description. No code commit.

---

## Notes for the executor

- **Order matters:** Tasks 2–12 build helpers with their own tests *before* Task 13 rewires `Screen`, so the suite stays green until the switch. Task 13 is the one big cutover.
- **Don't fabricate APIs:** match the real `event.*`, `content.*`, `tool.*`, and `tui` types by reading the files named in each task; the code blocks above are intent, not verbatim signatures.
- **Each task is independently committable and revertable.** If a task balloons past ~30-line functions, split it (SRP).
