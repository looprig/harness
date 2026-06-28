# Transcript HTML Export Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a `/export` slash command that writes a single self-contained
`~/Downloads/<session-id>.html` of the whole session — system prompt, turns/steps, tool calls,
inline-nested subagents, and the user's own gate actions — with TUI-inspired styling and
collapsible internals.

**Architecture:** Two new looprig packages. `pkg/transcript` folds a session's journal record
stream (events **and** user commands) into a format-agnostic, tree-structured session **model**;
it depends only on the pure data packages (`event`, `command`, `content`, `identity`), never on
NATS. `pkg/transcript/html` renders that model to one self-contained HTML file via `html/template`
+ embedded CSS/JS, with markdown rendered by goldmark. A new `journal.RecordReplayer` is the data
seam (the existing `EventReplayer` decodes events only and would drop every gate decision). The
TUI's `/export` action chains `Reconstruct → Render → atomic write`. The only swe change is a
one-method pass-through to keep `*sessionAgent` satisfying `tui.Agent`.

**Tech Stack:** Go 1.26; looprig `pkg/transcript` (new), `pkg/transcript/html` (new),
`pkg/journal`, `pkg/tui`, `pkg/session`, `pkg/event`, `pkg/command`, `pkg/content`, `pkg/identity`;
swe `swarms/swe`; **goldmark** (`github.com/yuin/goldmark`, approved 2026-06-28); bubbletea v2.
Design: `docs/plans/2026-06-28-transcript-html-export-design.md` (decisions referenced below as
"D1"…"D12").

**Scope:** Current/live persisted session export only. Non-HTML renderers, arbitrary past-session
export, auto-open, configurable path, and inline media bytes are **out of scope** (design "Out of
scope (v1)").

**Conventions:**
- **Worktree isolation:** prefix every Go command with
  `GOWORK=off GOPRIVATE='github.com/ciram-co/*' GOSUMDB=off` so the worktree's own module is
  exercised, not the main checkout (the parent `go.work` masks it otherwise). Abbreviated as `go …`
  below.
- TDD: write the failing test first, watch it fail, implement the minimum, watch it pass, commit.
- Test fixtures build events/commands as **struct literals** with a stamped `event.Header`
  (`event.Factory` only stamps headers; `content` has no `New*` constructors — use
  `content.Message{Role, Blocks}` wrapped in `content.UserMessage{}`/`AIMessage{}` etc.).
- All errors **typed** and `errors.As`-able (repo rule). Tests **table-driven** + `-race`.
- Tasks 11–14 touch the `tui.Agent` interface across **looprig AND swe**; keep both module builds
  green within the task that changes the signature.
- Final gate: looprig `make lint && make test`; swe `make lint && make test`; both green.

---

## Phase A — `pkg/transcript`: the session model + reconstruction (pure, no deps)

### Task 1: Package skeleton — model types + seam interfaces compile

**Files:**
- Create: `pkg/transcript/model.go` (the model structs + enums, per D3)
- Create: `pkg/transcript/source.go` (`Record`, `RecordSource`, `SystemPromptResolver`, per D2)
- Create: `pkg/transcript/errors.go` (`ReconstructError`, per D10)
- Test: `pkg/transcript/model_test.go`

**Step 1 (failing test):** a compile-guard + zero-value test:
```go
package transcript

import "testing"

func TestModelZeroValues(t *testing.T) {
	var s Session
	if s.Root != nil || len(s.Notices) != 0 || len(s.Warnings) != 0 {
		t.Fatalf("zero Session not empty: %+v", s)
	}
	// sum-type guard: EventRecord and CommandRecord both satisfy Record.
	var _ Record = EventRecord{}
	var _ Record = CommandRecord{}
}
```
**Step 2:** `go test ./pkg/transcript/ -run TestModelZeroValues -v` → FAIL (package/types absent).
**Step 3 (implement):** create the three files. `model.go` declares exactly the structs in D3
(`Session, Config, Loop, Turn, Step, ToolCall, GateAction, Message, Notice, Warning`) plus the
enums (`Outcome{Running,Done,Failed,Interrupted}`, `GateKind{Permission,AskUser}`,
`Decision{Pending,Approved,Denied,Answered}`, `NoticeKind`). Reuse `content.Block`/`content.Role`,
`event.TurnIndex`, `tool.ApprovalScope`, `uuid.UUID`, `time.Time` — do not redefine them.
`source.go`:
```go
// Record is one journaled item the builder folds in: an enduring Event or a Command.
type Record interface{ isRecord() }

type EventRecord   struct{ Event   event.Event }
type CommandRecord struct{ Command command.Command }

func (EventRecord) isRecord()   {}
func (CommandRecord) isRecord() {}

// RecordSource yields Records in journal-sequence order from the beginning; io.EOF at end.
type RecordSource interface {
	Next(ctx context.Context) (Record, error)
}

// SystemPromptResolver supplies live system-prompt text per loop (D4); ok=false when absent.
type SystemPromptResolver interface {
	SystemPrompt(loopID uuid.UUID) (text string, ok bool)
}
```
`errors.go`: `ReconstructError{Stage string; Cause error}` with `Error()`/`Unwrap()`.
**Step 4:** test PASS. **Step 5:** `git commit -m "feat(transcript): model types + record-source seam"`.

---

### Task 2: `Reconstruct` happy path — session → loop → turn → step (AI + tool pairing)

**Files:**
- Create: `pkg/transcript/reconstruct.go` (`Reconstruct` + an internal `builder`)
- Test: `pkg/transcript/reconstruct_test.go`

**Step 1 (failing test):** a table whose first row feeds an in-memory `RecordSource` (a slice-backed
fake) with: `SessionStarted{Config: ConfigFingerprint{ModelID:"claude-opus-4-8", AgentKind:"operator"}}`
→ `LoopStarted{}` (primary, `ParentToolUseID:""`) → `TurnStarted{TurnIndex:1, Message: user "hi"}` →
`StepDone{Messages: [AIMessage{Blocks:[TextBlock{"hello"}, ToolUseBlock{ID:"tu1", Name:"Bash", Input:…}]},
ToolResultMessage{ToolUseID:"tu1", Blocks:[TextBlock{"ok"}]}]}` → `TurnDone{TurnIndex:1}`. Assert:
one `Root` loop, one `Turn` (Outcome `Done`), `User` text "hi", one `Step` whose `AI` text is
"hello" and whose `Tools[0]` has `Name=="Bash"`, `ToolUseID=="tu1"`, `Result` text "ok", `IsError`
false. Use a tiny `sliceSource` helper that returns `io.EOF` past the end and stamps each event's
`Header.Coordinates.LoopID` to the primary loop id, `CreatedAt` increasing.
**Step 2:** `go test ./pkg/transcript/ -run TestReconstruct -v` → FAIL.
**Step 3 (implement):** `Reconstruct(ctx, src, prompts) (*Session, []Warning, error)` loops
`src.Next` until `io.EOF`; a non-EOF read error returns `nil,nil,&ReconstructError{Stage:"read",Cause:err}`.
A `builder` holds `loops map[uuid.UUID]*Loop`, the current open turn per loop, and a
`toolByExecID`/`toolByUseID` index. Implement the fold rows from D3 needed here:
`SessionStarted` (set Config/StartedAt), `LoopStarted` (root when `ParentToolUseID==""`),
`TurnStarted` (open turn, set `User`), `StepDone` (split `AgenticMessages`: leading `AIMessage`
→ `Step.AI`; each trailing `ToolResultMessage` paired to its `AIMessage` `ToolUseBlock` by
`ToolUseID`), `TurnDone` (close turn, `Outcome=Done`). Stamp `EndedAt` from the last record seen.
**Step 4:** test PASS. **Step 5:** `git commit -m "feat(transcript): reconstruct happy-path turn/step/tool"`.

---

### Task 3: Gate resolution — permission + AskUser, events ⨉ user commands (D5)

**Files:** Modify `pkg/transcript/reconstruct.go`; Test `pkg/transcript/reconstruct_test.go` (rows).

**Step 1 (failing tests, new table rows):**
1. *Approved at session scope:* after the tool's `StepDone`, feed
   `PermissionRequested{ToolExecutionID:e1}` (its `Cause`/coordinates locate the step's tool-use),
   then the user command `ApproveToolCall{Cause:{ToolExecutionID:e1}, Scope: session}` (Agency User).
   Assert the matching `ToolCall.Gate` = `{Kind:Permission, Decision:Approved, Scope:session,
   DecidedAt: cmd time}`.
2. *Denied:* `DenyToolCall` → `Decision:Denied`.
3. *AskUser answered:* `UserInputRequested{ToolExecutionID:e2, Question:"which?", Choices:[a,b]}`
   then `ProvideUserInput{Cause:{ToolExecutionID:e2}, …answer "a"}` → `Gate{Kind:AskUser,
   Decision:Answered, Question, Choices, Answer:"a"}`.
4. *Unresolved gate (snapshot mid-prompt):* `PermissionRequested` with no resolving command →
   `Decision:Pending`, **no Warning**.
5. *Resolving command with no gate:* an `ApproveToolCall` whose `ToolExecutionID` matches nothing →
   one `Warning`, no panic.
**Step 2:** FAIL. **Step 3 (implement):** maintain `pendingGates map[uuid.UUID]*ToolCall` keyed by
`ToolExecutionID`. On `PermissionRequested`/`UserInputRequested`, find the owning tool-use within the
just-closed step (via the event's `Cause`/coordinates → loop/turn/step → the tool whose
`ToolExecutionID` the runner assigned; correlate by the same provider-id precedent as
`2026-06-22-subagent-card-rendering-design.md` D2) and set `Gate{Decision:Pending}` + index it. On
the three **commands** (read `Cause.ToolExecutionID`, `Agency==User`), resolve the indexed gate;
unmatched → append `Warning`. Keep a `CommandRecord` switch alongside the `EventRecord` switch.
**Step 4:** PASS. **Step 5:** `git commit -m "feat(transcript): resolve permission/askuser gates from user commands"`.

---

### Task 4: Subagent nesting — child loop attaches under its spawning tool call (D6)

**Files:** Modify `pkg/transcript/reconstruct.go`; Test rows.

**Step 1 (failing tests):**
1. Primary `StepDone` contains `ToolUseBlock{ID:"sub1", Name:"Subagent"}`; later a
   `LoopStarted{LoopID:child, AgentName:"reviewer", ParentToolUseID:"sub1"}` + the child's own
   `TurnStarted`/`StepDone`/`TurnDone`. Assert the primary `ToolCall` with `ToolUseID=="sub1"` has a
   non-nil `Child *Loop` whose `AgentName=="reviewer"` and whose `Turns` hold the child's step; the
   child is **not** in `Root`.
2. Two concurrent children (`sub1`,`sub2`) attach to their respective tool calls — no cross-attach.
3. `LoopStarted` whose `ParentToolUseID` matches no tool-use → child becomes a top-level `Warning`
   (orphan loop), never a panic.
**Step 2:** FAIL. **Step 3 (implement):** keep `toolByUseID map[string]*ToolCall` populated as steps
fold. On `LoopStarted` with non-empty `ParentToolUseID`, look up the tool call and set its `Child`;
route that loop's later `TurnStarted`/`StepDone`/terminal events to the child `Loop` (loops are keyed
by `Header.Coordinates.LoopID`). Resolve the system prompt for the child via the resolver (Task 6).
**Step 4:** PASS. **Step 5:** `git commit -m "feat(transcript): nest subagent loops under spawning tool call"`.

---

### Task 5: Outcomes, folded input, notices, fail-secure warnings

**Files:** Modify `pkg/transcript/reconstruct.go`; Test rows.

**Step 1 (failing tests):**
1. `TurnFailed{Err}` → `Outcome:Failed`, `Err` text captured (note: `event.TurnFailed.Err` is
   `json:"-"` and absent on replayed records — capture it when present, else leave `""`).
2. `TurnInterrupted` → `Outcome:Interrupted`. A turn with no terminal at stream end → `Outcome:Running`.
3. `TurnFoldedInto{Message}` folds the user input onto the open turn (append/replace per its
   semantics).
4. Lifecycle events (`SessionIdle/Active/Stopped`, `RestoreStarted/Done/Errored`) → ordered
   `Notice`s on `Session.Notices`.
5. *Fail-secure:* an orphan `ToolResultMessage` (no matching tool-use) and an unknown event type →
   `Warning`, never panic. (Untrusted-input boundary, D11.)
**Step 2:** FAIL. **Step 3 (implement):** add the terminal/notice/fold fold-rows and a default
branch that records a `Warning` rather than failing. **Step 4:** PASS.
**Step 5:** `git commit -m "feat(transcript): outcomes, folded input, notices, fail-secure warnings"`.

---

### Task 6: System-prompt resolution + restored-session degradation (D4)

**Files:** Modify `pkg/transcript/reconstruct.go`; Test rows.

**Step 1 (failing tests):** (1) resolver returns `("SYSTEM TEXT", true)` for the primary loop id →
`Root.SystemPrompt=="SYSTEM TEXT"`; (2) resolver returns `("", false)` → `Root.SystemPrompt==""` **and**
one `Warning` ("system prompt unavailable (rev <SystemPromptRev>)"). Use a fake resolver keyed by
loop id. **Step 2:** FAIL. **Step 3 (implement):** on `LoopStarted`, call
`prompts.SystemPrompt(loopID)`; set `Loop.SystemPrompt` or record the degradation `Warning`,
carrying `Config.SystemPromptRev` from `SessionStarted`. **Step 4:** PASS.
**Step 5:** `git commit -m "feat(transcript): resolve live system prompt with restored-session fallback"`.

---

## Phase B — `pkg/transcript/html`: the renderer

### Task 7: Add goldmark; renderer skeleton + golden minimal render (D7, D12)

**Files:**
- Modify: `go.mod`/`go.sum` (`go get github.com/yuin/goldmark@latest`)
- Modify: `CLAUDE.md` (record goldmark under approved dependencies, per repo rule)
- Create: `pkg/transcript/html/render.go` (`Render(w io.Writer, s *transcript.Session) error`)
- Create: `pkg/transcript/html/template.gohtml` + `embed.go` (`//go:embed`)
- Create: `pkg/transcript/html/errors.go` (`RenderError{Cause}`)
- Test: `pkg/transcript/html/render_test.go` + `testdata/minimal.golden.html`

**Step 1 (failing test):** render a tiny `*transcript.Session` (one user turn, one AI step, no tools),
normalize timestamps (regex `\d{2}:\d{2}:\d{2}` → `HH:MM:SS`), compare to the golden file (write the
golden via a `-update` flag guard). Assert byte-equality after normalization. **Step 2:**
`go test ./pkg/transcript/html/ -run TestRenderMinimal -v` → FAIL. **Step 3 (implement):**
`go get` goldmark; add the approval line to `CLAUDE.md`. Build `Render` on `html/template` parsed
from the embedded `template.gohtml`; pass a view-model derived from `*transcript.Session`. Inline
`<style>`/`<script>` via embedded files (no external `src`/`href`). On template execute error return
`&RenderError{Cause:err}`. Generate the golden with `-update`. **Step 4:** PASS.
**Step 5:** `git commit -m "feat(transcript/html): renderer skeleton + embedded template; approve goldmark"`.

---

### Task 8: Markdown via goldmark + XSS hardening + fuzz (D7, D11)

**Files:** Modify `pkg/transcript/html/render.go` (a `renderMarkdown(string) template.HTML`);
Test `pkg/transcript/html/render_test.go` + `fuzz_test.go`.

**Step 1 (failing tests):**
1. AI text ```"# Title\n\n- a\n- b\n\n`code`"``` renders `<h1>`, `<ul><li>`, `<code>` in the output.
2. **XSS:** a user message `"<script>alert(1)</script>"`, an AI text ``"</script><img onerror=x>"``,
   and a tool result containing `"<svg onload=alert(1)>"` all appear **escaped/inert** — assert the
   output contains no live `<script>`/`onerror=`/`onload=` from user data. goldmark configured with
   **raw-HTML passthrough OFF** (no `html.WithUnsafe()`); the result is placed as `template.HTML`
   only because goldmark already escaped it.
3. `FuzzRenderMarkdown`: arbitrary bytes → never yields an unescaped `<script` or ` on\w+=` attribute
   from the input; always terminates. Run `go test ./pkg/transcript/html/ -run x -fuzz FuzzRenderMarkdown -fuzztime=30s`.
**Step 2:** FAIL. **Step 3 (implement):** configure goldmark (`goldmark.New(goldmark.WithExtensions(extension.GFM))`,
**without** the unsafe HTML renderer option) and render each markdown block through it; everything
else flows through `html/template` auto-escaping. **Step 4:** PASS (incl. 30s fuzz clean).
**Step 5:** `git commit -m "feat(transcript/html): goldmark markdown with XSS-safe escaping + fuzz"`.

---

### Task 9: Full layout — collapsible AI/thinking, tool cards, gate chips, nested subagents, toolbar (D7)

**Files:** Modify `template.gohtml`, `styles.css`, `app.js`, `render.go` view-model;
Test `pkg/transcript/html/render_test.go` (+ `testdata/full.golden.html`).

**Step 1 (failing tests):** render a model exercising every feature and assert structure (parse with
`golang.org/x/net/html`? No — keep stdlib: assert on substrings/markers):
- header shows session id, model, agent kind, counts (`N turns`, `M tools`);
- system prompt in a `<details>` collapsed block;
- user message has the accent-bar marker; AI message is a `<details open>` (collapsible, expanded);
- thinking block is a `<details>` (collapsed);
- tool card shows `name`, decision verb `Approved ✓` / `Denied ✗`, and an expandable result;
- a **user-action chip** with scope+timestamp (`You approved · session · HH:MM:SS`);
- a nested subagent loop block (indented, `data-depth="1"`);
- toolbar controls present (`collapse-all`, `expand-all`);
- still self-contained (no external `src=`/`href=`); golden byte-equal after timestamp normalization.
**Step 2:** FAIL. **Step 3 (implement):** flesh `template.gohtml` per D7 layout; CSS uses the TUI
palette (lime `#D4F84D` AI bullet, blue `#A2D2FF` headings, gray `#737373` user bar, faint tool
cards, dark theme) with web-native typography (proportional prose, monospace code/tool output);
`app.js` wires collapse/expand-all + jump-to-top (vanilla JS, no deps). Pretty-print tool `Input`
JSON; cap oversized tool `Result` with a "… N bytes elided" note. **Step 4:** PASS.
**Step 5:** `git commit -m "feat(transcript/html): full TUI-styled layout with collapsibles, gate chips, nested subagents"`.

---

## Phase C — the data seam in the journal

### Task 10: `journal.RecordReplayer` — full stream (events **and** commands), in sequence order

**Files:**
- Create: `pkg/journal/record_replay.go` (mirror `EventReplayer`/`streamReplayer` in `replay.go`,
  but **do not** subject-filter to events: include session-event, all loop-event, **and command**
  subjects; decode each `JournalRecord` variant by subject; fences surfaced or skipped)
- Test: `pkg/journal/record_replay_integration_test.go` (`//go:build integration`)

**Step 1 (failing test):** integration test (real embedded JetStream, as the other
`*_integration_test.go` do): append a known sequence — `SessionStarted`, `LoopStarted`,
`TurnStarted`, `StepDone`, a `PermissionRequested` (event) **and** an `ApproveToolCall` (command),
`TurnDone` — then `Open` a `RecordReplayer` at `Beginning()` and drain. Assert the cursor yields
**both** `EventRecord`s and the `CommandRecord` (the property `EventReplayer` lacks) in
stream-sequence order, and that an object-store-offloaded oversized record rehydrates. **Step 2:**
`go test -tags integration ./pkg/journal/ -run TestRecordReplayer -race -v` → FAIL. **Step 3
(implement):** add `RecordReplayer` interface (`Open(ctx, ReplayRequest) (RecordCursor, error)`),
`RecordCursor.Next(ctx) (JournalRecord, uint64, error)` (io.EOF at end), and `NewRecordReplayer(js,
objects)`. Reuse `replay.go`'s consumer-binding, bounded-fetch, fail-secure decode, and
object-rehydration; the only delta is the subject set (all subjects) and a by-subject decode
dispatch to `EventRecord`/`CommandRecord`/`FenceRecord`. Reuse/clone its typed errors
(`ReplaySetupError`, `ReplayReadError`). **Step 4:** PASS. **Step 5:**
`git commit -m "feat(journal): RecordReplayer yields events and commands in sequence"`.

---

## Phase D — wiring: session seam → tui action → swe pass-through → integration

### Task 11: `session.Session.ExportSource` — journal-backed `RecordSource` + `SystemPromptResolver`

**Files:**
- Modify: `pkg/session/session.go` (add `ExportSource`; an unexported adapter mapping
  `journal.JournalRecord` → `transcript.Record`, dropping fences; a resolver over the session's
  loop configs)
- Test: `pkg/session/export_test.go`

**Step 1 (failing test):** over a persisted test session with one turn, call
`ExportSource(ctx)`; drain the returned `transcript.RecordSource` and assert it yields the expected
`transcript.Record`s in order, and that `SystemPrompt(primaryLoopID)` returns the loop config's
`Model.System` text. For a **non-persisted** session, assert `ExportSource` returns a typed
`*ExportUnavailableError` (no journal stream to replay — see Notes/risks). **Step 2:** FAIL.
**Step 3 (implement):** `ExportSource(ctx) (transcript.RecordSource, transcript.SystemPromptResolver,
error)`: construct a `journal.RecordReplayer` over the session's stream (`js`+objects the session
already holds), open at `Beginning()` with `Follow:false`, wrap its `RecordCursor` in an adapter
implementing `transcript.RecordSource.Next` (map `EventRecord`/`CommandRecord`, skip `FenceRecord`);
build a `SystemPromptResolver` from the per-loop `loop.Config.Model.System`. Non-persisted →
`&ExportUnavailableError{}`. **Step 4:** PASS. **Step 5:**
`git commit -m "feat(session): ExportSource exposes journal record stream + system prompts"`.

---

### Task 12: Extend `tui.Agent` + the `/export` action, file write, and notices (D8, D9, D10)

**Files:**
- Modify: `pkg/tui/agent.go` (add `ExportSource(ctx) (transcript.RecordSource, transcript.SystemPromptResolver, error)` to `Agent`)
- Modify: `pkg/tui/components/slashcomplete.go` (append `{"/export", "export session transcript to HTML"}`)
- Modify: `pkg/tui/action.go` (new `uiExport` kind)
- Modify: `pkg/tui/interaction.go` (`/export` → `uiAction{Kind: uiExport}`; add to `isSlashCommand`)
- Modify: `pkg/tui/screen.go` (`runSlash` `case "/export"`; an async `exportCmd` + result message)
- Create: `pkg/tui/export.go` (the `tea.Cmd`: reconstruct → render → resolve `~/Downloads` → atomic
  write; `ExportWriteError{Path, Cause}`)
- Test: `pkg/tui/export_test.go`

**Step 1 (failing tests):**
1. `slashcomplete` lists `/export`; `isSlashCommand("/export")` true; `helpText` includes it.
2. `exportCmd` over a fake `Agent` (its `ExportSource` returns a canned record stream) writes a file
   under a temp `HOME`, returns a success message carrying the path + counts; the file exists, is
   valid UTF-8, contains the system prompt and a gate chip.
3. write failure (unwritable dir) → an `*ExportWriteError` surfaced as an **error** notice; success →
   an **info** notice `Exported → <path> (N turns · M tools)` (reuse `NoticeInfoStyle`/`NoticeErrorStyle`).
**Step 2:** `go test ./pkg/tui/ -run TestExport -v` → FAIL. **Step 3 (implement):** add the interface
method; register the command; add `uiExport` + dispatch; `runSlash` returns `m.exportCmd()` (allowed
in any status — snapshot semantics, D1). `export.go`: `transcript.Reconstruct` → `html.Render` to a
`bytes.Buffer` → path `filepath.Join(home, "Downloads", sessionID.String()+".html")` (`os.UserHomeDir`,
`filepath.Clean`, `os.MkdirAll`) → **atomic** temp+rename `0644` (mirror `pkg/tools/writefile.go`'s
`atomicWriteFile`). All failures → typed errors → notices; **only the path is logged, never content**
(D9). **Step 4:** PASS. **Step 5:** `git commit -m "feat(tui): /export command writes self-contained HTML transcript to ~/Downloads"`.

> Note: adding `ExportSource` to `tui.Agent` breaks swe's compile-time `var _ tui.Agent =
> (*sessionAgent)(nil)` until Task 13 — complete Task 13 before any cross-repo `go build`.

---

### Task 13: swe `*sessionAgent.ExportSource` pass-through (no logic)

**Files:**
- Modify: `swarms/swe/agent.go` (forward to `a.session.ExportSource(ctx)`)
- Test: `swarms/swe/agent_test.go` (the existing `var _ tui.Agent = (*sessionAgent)(nil)` now also
  proves the new method; add a focused forward test)

**Step 1 (failing test):** assert `(*sessionAgent).ExportSource` forwards to the session seam (over a
fake/closed session it returns the session's result/error unchanged); the interface-satisfaction var
compiles. **Step 2:** `cd /Users/ipotter/code/swe && go test ./swarms/swe/ -run TestExportSource -v`
→ FAIL (method missing). **Step 3 (implement):** one method:
```go
func (a *sessionAgent) ExportSource(ctx context.Context) (transcript.RecordSource, transcript.SystemPromptResolver, error) {
	return a.session.ExportSource(ctx)
}
```
**Step 4:** PASS; `cd looprig && go build ./...` and `cd swe && go build ./...` both green.
**Step 5:** `git commit -m "feat(swe): forward ExportSource to the looprig session"`.

---

### Task 14: End-to-end integration — drive a session, export, assert the file

**Files:** Create `pkg/tui/export_integration_test.go` (`//go:build integration`) **or**
`swarms/swe/export_integration_test.go` (whichever owns a real persisted session harness).

**Step 1 (failing test):** spin a real persisted session; submit a turn that (a) gates a tool the test
approves, and (b) spawns a subagent; then invoke the export path → assert the written
`~/Downloads/<id>.html` (temp HOME) exists, is valid HTML, and contains: the system prompt text, a
`You approved` gate chip, the nested subagent loop block, and both turns' timestamps. **Step 2:**
`go test -tags integration ./... -run TestExportEndToEnd -race -v` → FAIL. **Step 3:** wire the
harness; fix any integration gaps surfaced. **Step 4:** PASS. **Step 5:**
`git commit -m "test(transcript): end-to-end export integration"`.

---

## Phase E — finalize

### Task 15: Lint, full suites, docs, manual verify

**Steps:**
1. looprig: `make fmt && make lint && make test` (race) → green; `make build`.
2. swe: `make fmt && make lint && make test` → green; `make build`.
3. Integration suites: `go test -tags integration -race ./...` in both (where wired).
4. **Docs:** confirm goldmark recorded in looprig `CLAUDE.md`; add the transitive goldmark note to
   **swe** `CLAUDE.md` dependency list (it now ships in swe's binary via looprig).
5. **Manual verify** (use @superpowers:verification-before-completion): `cd swe && make run`, hold a
   short multi-turn conversation that approves a gate and spawns a subagent, type `/export`, confirm
   the notice and open `~/Downloads/<session-id>.html`: system prompt collapsible, AI messages
   collapsible (expanded) and bulk collapse works, thinking/tool I/O collapsed, gate chip present,
   subagent nested, timestamps throughout, TUI-like styling, opens offline.
6. `git commit -m "chore(transcript): finalize export — lint/tests/docs/manual verify"`.

When the branch is green and verified, use @superpowers:finishing-a-development-branch to integrate.

---

## Notes / risks

- **Persisted-session assumption (Task 11):** export replays the journal, so it requires a
  journal-backed session. A purely in-memory session has no stream → `ExportUnavailableError` +
  TUI notice. Confirm swe's default run path is persisted (`newPersistentSessionAgent`); if the
  default is in-memory, either make `/export` persist-or-degrade or add an in-memory record buffer
  (a larger change — raise before implementing).
- **Gate↔tool correlation (Task 3):** reuses the two-namespace precedent (`ToolExecutionID` UUID vs
  `content.ToolUseBlock.ID`) from `2026-06-22-subagent-card-rendering-design.md` D2. A gate whose
  tool-use never materializes degrades to a `Warning`, never a panic.
- **`TurnFailed.Err` is `json:"-"`** — absent on replayed records; failed turns may render without
  error text (acceptable; the Outcome still shows `failed`).
- **Snapshot semantics (D1):** `/export` mid-turn captures only journaled-so-far records; an
  in-flight turn may be partially present. Allowed in any status (unlike `/clear`).
- **Permission exception (D9):** the file write is a direct user action, not an agent tool call —
  it bypasses the Ask gate and writes outside the workspace by design. Documented in the design doc;
  do not "fix" it by routing through the gate.
- **Worktree builds:** always `GOWORK=off …` (the parent `go.work` points at the main checkout and
  would mask the worktree module otherwise).
- **goldmark unsafe option:** never pass `html.WithUnsafe()` / raw-HTML passthrough — that is the
  XSS boundary (Task 8).
