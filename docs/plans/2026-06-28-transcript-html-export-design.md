# Transcript HTML export — design

Date: 2026-06-28
Status: implementation-ready
Repos: `looprig` (primary — entire feature), `swe` (interface pass-through only)

## Problem

There is no way to get a durable, human-readable artifact of a session out of the agent. A
person chatting with an agent (e.g. swe) can see the live scrollback in the TUI, but once the
process exits that view is gone. We want a `/export` slash command that writes a single,
self-contained `~/Downloads/<session-id>.html` capturing the **whole session from the
beginning** — system prompt, every turn and step, tool calls, nested subagent activity, and the
**user's own actions** (gate approvals/denials, AskUser answers) — each stamped with a
timestamp, styled to echo the TUI, and readable as a long document with collapsible internals.

## Goal & ownership

- The **entire feature lives in looprig**. looprig is the source of the session data (journal),
  the loop tree (who spawned whom), the loop configs (system-prompt text), and the user-command
  intent log (gate decisions). It reconstructs the session and crafts the HTML from its own
  data. **swe contributes nothing but one interface method's pass-through** and inherits
  `/export` for free; any other consumer (future modes) does too.
- **Two packages, single-responsibility each:**
  - `pkg/transcript` — reconstructs a typed, **format-agnostic session model** from a record
    stream. No HTML, no NATS. One reason to change: the model's shape.
  - `pkg/transcript/html` — renders a `transcript.Session` to one self-contained HTML document.
    One reason to change: the HTML format/style.
- Reuse is the point: "other modes later" reuse the **same model** behind a markdown / JSON /
  live-web renderer with zero rework.

## Fidelity stance (decided)

**Full audit, readable by default.** Everything is captured — system prompt, every turn/step,
thinking blocks, raw tool inputs/outputs, gate decisions, nested subagents — but the page opens
clean: AI messages expanded but collapsible; thinking blocks and raw tool I/O collapsed by
default and expandable; a sticky toolbar to collapse/expand in bulk.

---

## Decision 1 — the data seam reads the FULL journal stream (events **and** user commands)

The user's gate decisions are the load-bearing reason the seam is not "replay events." Per the
journal layer:

- `pkg/journal/record.go` — a session's journal is a sealed sum type `JournalRecord` =
  `EventRecord` (an enduring `event.Event`) | `CommandRecord` (a `command.Command`) |
  `FenceRecord` (lease handover). All three are `Append`-ed to **one per-session stream**
  (`streamJournal.stream = StreamName(sessionID)`, `pkg/journal/journal.go`). They differ only
  by **subject**, so reading the whole stream in sequence yields events and commands
  **interleaved in causal/append order** — exactly the merged stream we need.
- `pkg/journal/replay.go`'s existing `EventReplayer` **subject-filters to enduring events only**
  (`ReplayRequest{SessionID, LoopID, From, Follow}`), so it drops `CommandRecord`s — i.e. it
  drops every user gate decision. We must NOT reuse it as-is.

**Add a `journal.RecordReplayer`** alongside `EventReplayer`: same construction (`js`, objects),
same `ReplayRequest`, but it does **not** subject-filter — its cursor yields the full
`JournalRecord` sequence from `Beginning()` to the stream's current end (`io.EOF` at the last
existing message; `Follow:false`, a cold snapshot). It reuses `EventReplayer`'s object-store
rehydration path for over-threshold records. Fences are surfaced but the transcript builder
ignores them.

This is a snapshot at call time: if `/export` fires mid-turn, records not yet journaled are
simply absent. We allow export in any status (unlike `/clear`, which gates on `StatusIdle`).

## Decision 2 — `pkg/transcript` stays pure: depends on data packages, never on storage

`transcript` depends only on the **pure data packages** (`event`, `command`, `content`,
`identity`) — never on `journal`/NATS. The seam it consumes is its own narrow type, and a thin
journal-backed adapter (Decision 8) converts `journal.JournalRecord` → `transcript.Record`:

```go
package transcript

// Record is one journaled item the builder folds in: an enduring Event or a Command.
// (Fences are dropped by the adapter and never reach the builder.)
type Record interface{ isRecord() }
type EventRecord   struct{ Event   event.Event }     // implements Record
type CommandRecord struct{ Command command.Command } // implements Record

// RecordSource yields Records in journal-sequence order from the beginning; io.EOF at end.
type RecordSource interface {
    Next(ctx context.Context) (Record, error)
}

// SystemPromptResolver supplies live system-prompt text per loop (see Decision 4).
type SystemPromptResolver interface {
    SystemPrompt(loopID uuid.UUID) (text string, ok bool)
}

// Reconstruct folds the record stream into the session model. Best-effort: malformed/unpaired
// records become Warnings, not errors; only a source read error aborts.
func Reconstruct(ctx context.Context, src RecordSource, prompts SystemPromptResolver) (*Session, []Warning, error)
```

## Decision 3 — the session model mirrors how looprig records reality

A tree built from the events documented in `pkg/event/{event,turn,tool}.go` and
`pkg/content/{message,block}.go`. Names provisional; shapes load-bearing.

```go
package transcript

type Session struct {
    SessionID uuid.UUID
    Title     string          // journal catalog SessionMeta.Title, else first user msg, truncated
    Config    Config          // from SessionStarted.Config (event.ConfigFingerprint)
    StartedAt time.Time       // SessionStarted.Header.CreatedAt
    EndedAt   time.Time       // last record's CreatedAt (snapshot edge)
    ExportedAt time.Time      // stamped by the caller (clocks are not available in pure code)
    Root      *Loop           // the primary loop (ParentToolUseID == "")
    Notices   []Notice        // session-lifecycle + out-of-band, in order
    Warnings  []Warning       // reconstruction anomalies, surfaced into the doc
}

type Config struct { ModelID, AgentKind, PermissionPosture string; SystemPromptRev string }

type Loop struct {
    LoopID          uuid.UUID
    AgentName       string     // LoopStarted.AgentName
    ParentToolUseID string     // "" for primary; else the Subagent ToolUseBlock.ID that spawned it
    SystemPrompt    string     // resolved live text; "" + a Warning if unavailable (restored session)
    StartedAt       time.Time
    Turns           []*Turn
}

type Turn struct {
    Index     event.TurnIndex
    StartedAt time.Time        // TurnStarted.Header.CreatedAt
    EndedAt   time.Time
    User      *Message         // TurnStarted.Message (UserMessage)
    Steps     []*Step
    Outcome   Outcome          // done | failed | interrupted | running
    Err       string           // TurnFailed error text (errors aren't serialized; see note)
}

type Step struct {
    StepID uuid.UUID
    AI     *Message            // the AIMessage from StepDone.Messages
    Tools  []*ToolCall         // ToolUseBlocks paired to their ToolResultMessages
    Gates  []*GateAction       // every gate raised during this step, in order (Decision 5);
                               // a bound gate is ALSO referenced by its ToolCall.Gate (same pointer)
}

type ToolCall struct {
    ToolUseID string            // content.ToolUseBlock.ID — the durable correlation key
    Name      string
    Input     json.RawMessage   // ToolUseBlock.Input, pretty-printed at render time
    Result    []content.Block   // from the paired ToolResultMessage (ToolUseID match)
    IsError   bool
    At        time.Time
    Gate      *GateAction       // non-nil iff a Step.Gate was bound to this call (same pointer); Decision 5
    Child     *Loop             // non-nil iff this call spawned a subagent (inline nesting)
}

type GateAction struct {
    Kind     GateKind           // permission | askUser
    Decision Decision           // approved | denied | answered | pending
    Scope    tool.ApprovalScope // once | session | workspace (meaningful iff Kind==permission)
    ToolName    string          // permission: Request.ToolName() (durable via the wire form, Decision 5)
    Description string          // permission: Request.Description() — redacted prompt body, never raw args
    Question string; Choices []string; Answer string   // askUser only (Question/Choices durable on the event)
    ToolUseID string            // the tool this gate bound to ("" if unbound → renders as a bare notification)
    OpenedAt  time.Time         // PermissionRequested/UserInputRequested timestamp
    DecidedAt time.Time         // resolving command timestamp (zero while pending)
    // Agency is always User for a resolved gate (the only thing that resolves it).
}

type Message struct { Role content.Role; Blocks []content.Block; At time.Time }
type Notice  struct { Kind NoticeKind; Text string; At time.Time }   // restore start/done, stopped, …
type Warning struct { Text string; At time.Time }
```

**Fold rules (the heart of `Reconstruct`):**

| Record | Action |
|--------|--------|
| `SessionStarted` | set `Config` (from `ConfigFingerprint`: `ModelID`, `AgentKind`, `PermissionPosture`, `SystemPromptRev`), `StartedAt`. |
| `LoopStarted` | new `Loop`; resolve `SystemPrompt` via the resolver. If `ParentToolUseID == ""` it is `Root`; else **buffer** the child keyed by `{Cause.LoopID, ParentToolUseID}` — the parent `ToolCall` does not exist yet (Decision 6). Its later turn/step events route by `Header.LoopID`. |
| `TurnStarted` / `TurnFoldedInto` | open a `Turn` on the owning loop (`Header.Coordinates.LoopID`), set `User` from `.Message`. |
| `PermissionRequested` / `UserInputRequested` | **buffer** a `GateAction{Decision: pending, …}` for the current (not-yet-emitted) step, keyed by `ToolExecutionID` — capturing `ToolName`/`Description` (permission) or `Question`/`Choices` (askUser) from the durable event, plus `OpenedAt` (Decision 5). |
| `ApproveToolCall` / `DenyToolCall` / `ProvideUserInput` **(commands, `Agency==User`)** | resolve the buffered gate by `ToolExecutionID` → `approved`(+`Scope`) / `denied` / `answered`(+`Answer`) + `DecidedAt`; an unmatched command → `Warning`. |
| `StepDone` | split `.Messages` (`content.AgenticMessages`) into the leading `AIMessage` + trailing `ToolResultMessage`s; create a `Step`; pair each `ToolUseBlock` to its `ToolResultMessage` by `ToolUseID`. Then **flush** the step's buffered gates onto `Step.Gates`, binding each to a `ToolCall` by tool **name** (exact when unique in the step, else positional among same-named; sets `ToolCall.Gate` + `GateAction.ToolUseID`); clear the buffer. Also **attach** any buffered child loop whose `{loopID, ToolUseID}` matches a `ToolCall` → `ToolCall.Child` (Decision 6). |
| `TurnDone` / `TurnFailed` / `TurnInterrupted` | close the open turn with `Outcome` (+ error text for failed). |
| `SessionActive`/`Idle`/`Stopped`, `RestoreStarted`/`Done`/`Errored` | append a `Notice`. |
| `FenceRecord`, ephemeral events | never reach the builder / not journaled. |

## Decision 4 — system-prompt text comes from the live loop config, never the journal

`SessionStarted.Config` (`event.ConfigFingerprint`, `pkg/event/config_fingerprint.go`) stores
only `SystemPromptRev` — a **sha256 digest**, not the text. The text rides on
`llm.ModelSpec.System` (`pkg/llm/llm.go`), held by the **live loop config**. So:

- For the **current session** (the `/export` case), the running session supplies the text per
  loop via `SystemPromptResolver` → the export shows the full prompt.
- For a **restored/foreign** session whose live config differs or is absent, the resolver
  returns `ok == false`; the loop renders `system prompt unavailable (rev <digest>)` and a
  `Warning` is recorded. Honest degradation, no fabrication.

## Decision 5 — gate↔tool correlation: buffer-and-flush, bind by tool name

**The durable reality (verified against the runner/event/command code):**

- **Ordering.** For a gated call the journal order is `PermissionRequested` → resolving command
  (`ApproveToolCall`/`DenyToolCall`/`ProvideUserInput`) → *(ephemeral `ToolCallStarted`/
  `Completed`, dropped)* → `StepDone`. So the gate and its resolution land **before** the
  `StepDone` that carries the tool's `ToolUseBlock` + result — the `ToolCall` model object does
  **not exist yet** when the gate arrives.
- **What's durable.** `PermissionRequested` *does* persist its `Request` (a
  `permissionRequestedWire` in `pkg/event/marshal.go` round-trips it via
  `tool.MarshalPermissionRequest`), so on replay we recover `Request.ToolName()` (e.g. `Bash`) and
  `Request.Description()` (a **redacted** prompt body — never raw args), plus `ToolExecutionID`.
  `UserInputRequested` persists `Question`+`Choices`+`ToolExecutionID`. The commands persist
  `ToolExecutionID` (in `GateRoute`), `Agency==User`, `CreatedAt`, plus `Scope` (Approve) /
  `Answer` (ProvideUserInput).
- **No durable `ToolExecutionID → ToolUseBlock.ID` link** exists; the live TUI bridges it only via
  the *ephemeral* `ToolCallStarted` (keyed by `ToolExecutionID`), which a cold replay never sees.

**Therefore the builder buffers and flushes** (it cannot bind on gate arrival):

1. On `PermissionRequested`/`UserInputRequested`: build a `GateAction{Decision: pending}` capturing
   `ToolName`/`Description` (permission) or `Question`/`Choices` (askUser) and `OpenedAt`; index it
   by `ToolExecutionID` and append to the **current step's gate buffer**.
2. On the resolving command: look up by `ToolExecutionID`, set `Decision` (+`Scope`/`Answer`) and
   `DecidedAt`. An unmatched command → `Warning` (fail-secure, never panic).
3. On `StepDone`: after the tools are built, move the buffer onto `Step.Gates` and **bind each gate
   to a `ToolCall` by tool name** — exact when the step has a single tool of that name (the common
   case), else **positional** among same-named tools (best-effort, documented). Binding sets
   `ToolCall.Gate` and `GateAction.ToolUseID`. A gate that matches no tool stays unbound on
   `Step.Gates` — still fully renderable as a notification from its own `ToolName`/`Description`.

This satisfies the product requirement (every user gate action is shown as a timestamped
notification, from the gate's own durable data) and adds best-effort per-card decision verbs.
Edge case (gate left pending at a turn terminal with no `StepDone`) is handled in Decision-5
territory by Task 5: such buffered gates flush to the turn's last step (or a `Warning`).

## Decision 6 — subagents render inline, nested under the spawning tool call

`LoopStarted.ParentToolUseID` (the field added in the subagent-card design) is the link: a child
loop attaches as `ToolCall.Child` on the `Subagent` call whose `ToolUseID == ParentToolUseID`.
**Ordering:** the child's `LoopStarted` and entire lifecycle are journaled BEFORE the parent's
`StepDone` (the `Subagent` tool runs the child to completion, then the parent step commits — the
same "child precedes parent `StepDone`" reality the subagent-card design relies on). So the parent
`ToolCall` does not exist when the child appears; the builder **buffers** each child keyed by
`{Cause.LoopID, ParentToolUseID}` and attaches it when the parent `StepDone` creates the matching
`ToolCall`. A child whose parent tool-use never materialises → `Warning`. The renderer draws the
child's whole transcript indented beneath that card, collapsible, with a per-depth left-border
color. looprig owns this entirely from `ParentToolUseID` + `Cause`
ancestry; **swe has no say in the nesting**. Depth is whatever the journal records (swe is
structurally depth-1, but the model and renderer are depth-general).

## Decision 7 — `pkg/transcript/html`: one self-contained file, TUI-inspired, web-native

- **Stdlib `html/template` + `embed`** for inline `<style>`/`<script>`. **No external assets, no
  network** — one portable file that opens offline. `html/template` contextual auto-escaping is
  the XSS boundary (Decision 11).
- **Markdown** in user/AI text → HTML via **`github.com/yuin/goldmark`** (approved 2026-06-28;
  Decision 12). Rendered with `goldmark`'s safe HTML (no raw-HTML passthrough), then placed via
  `template.HTML` only after goldmark has produced it — goldmark, not us, owns escaping inside
  rendered markdown.
- **Visual identity** lifted from `pkg/tui/styles/styles.go`: AI bullet lime `DotColor`
  `#D4F84D`; headings/inline-code blue `MarkdownHeadingColor` `#A2D2FF`; user accent bar `▌`
  `InputAccent` `#737373`; faint tool/notice cards; dark theme. **Web-native typography**:
  proportional font for prose, monospace only for code and tool output. Palette/markers live in
  one place in the template so they track the TUI.
- **Layout:** header (session id, title, model, agent kind, started/ended/exported, counts:
  turns / tools / gates); system prompt in a collapsed block; per loop → turns → steps. User
  message = accent bar + timestamp; AI message = lime bullet + agent name + timestamp,
  **collapsible, expanded by default**; thinking block = faint/italic, **collapsed by default**;
  tool call = faint card (`name · summary · Approved ✓ / Denied ✗`), expand for pretty-printed
  input JSON + full result (monospace, scrollable; output above a byte cap is truncated with a
  "… N bytes elided" note — the full text remains in the journal); **subagent loop nested
  inline**; **user actions as notification chips** (`You approved · session · 10:42:03`,
  `You denied · 10:43`, `You answered: …`) visually distinct from agent content. Sticky toolbar:
  collapse/expand-all AI, collapse-all thinking, jump-to-top, turn index.
- `Render(w io.Writer, s *transcript.Session) error`. **No file I/O in the renderer** — the
  caller owns the file (Decision 9).

## Decision 8 — wiring: `/export` action → seam on `tui.Agent` → swe forwards

All in looprig (`pkg/tui`), mirroring the existing `/clear` path:

1. `pkg/tui/components/slashcomplete.go`: append `{"/export", "export session transcript to HTML"}`
   to `SlashCommands` (feeds completion, `isSlashCommand`, and `helpText`).
2. `pkg/tui/action.go`: new `uiExport` kind on `uiAction`.
3. `pkg/tui/interaction.go`: `/export` dispatches to `uiAction{Kind: uiExport}` (alongside the
   existing `uiRunSlash` handling).
4. `pkg/tui/screen.go` `runSlash`: `case "/export":` returns an **async `tea.Cmd`** (a big
   session must not freeze the UI) that: obtains the seam, `transcript.Reconstruct`,
   `html.Render` to a buffer, resolves `~/Downloads`, atomic-writes the file, and posts a result
   message. On completion the handler commits a **system notice** to the transcript —
   `Exported → ~/Downloads/<session-id>.html (N turns · M tools)` — or an **error notice** on
   failure (the styles already exist: `NoticeInfoStyle` / `NoticeErrorStyle`). Not auto-opened.
5. `pkg/tui/agent.go` — extend the small `Agent` interface with **one** method:

   ```go
   // ExportSource returns a snapshot RecordSource over this session's full journal
   // (enduring events + user commands) from the beginning, plus a live system-prompt resolver.
   // Returns *journalsource.ExportUnavailableError for a non-persisted (in-memory) session.
   ExportSource(ctx context.Context) (transcript.RecordSource, transcript.SystemPromptResolver, error)
   ```

   The journal→transcript adapter lives in a small looprig **bridge** package
   `pkg/transcript/journalsource` (it imports BOTH `journal` and `transcript`, so `pkg/transcript`
   stays storage-pure): `journalsource.Open(rr, req)` wraps a `journal.RecordReplayer`, maps
   `JournalRecord` → `transcript.Record`, and **drops fences**.

6. **Placement reality (verified):** looprig's `session.Session` does NOT hold `js`/objects, and the
   analogous `ReplayBacklog` already lives on **swe's `*sessionAgent`** (using a `journal.EventReplayer`
   swe builds in `persistence.go`). So there is **no `session.Session.ExportSource`**. swe's
   `sessionAgent.ExportSource` **builds** a `journal.RecordReplayer` from the `js`+object-store it has
   in `persistence.go` (`openNew` AND `openResume`) — exactly as it builds the EventReplayer — calls
   `journalsource.Open(...)` with `LoopID` zero (all loops), and supplies a **primary-only**
   `SystemPromptResolver` (the primary loop's `Config.Model.System`, captured at construction). This is
   thin wiring, not business logic — the model reconstruction + HTML rendering remain entirely in
   looprig (`pkg/transcript` + `pkg/transcript/html`). **Subagent system prompts are not retained**
   (built transiently at spawn), so they degrade to the Decision-4 "unavailable" warning; capturing
   them is a documented future enhancement. A non-persisted/headless session → `ExportUnavailableError`.

## Decision 9 — file output & the permission exception

- Path: `filepath.Join(homeDir, "Downloads", sessionID.String()+".html")` via `os.UserHomeDir`,
  then `filepath.Clean`. The session id is a UUID — no traversal surface. `os.MkdirAll` the
  Downloads dir if absent. **Atomic write** (temp + `rename`, `0644`), reusing the
  `atomicWriteFile` pattern at `pkg/tools/writefile.go:191`.
- Filename = exactly `<session-id>.html` (caller's spec; collision-free, scriptable).
- **Deliberate permission exception:** this write is a **direct human-invoked action** (the user
  typed `/export`), not an agent tool call. It therefore does **not** route through the agent's
  Ask gate and intentionally writes **outside the workspace** (`~/Downloads`). It is the TUI
  saving its own output on the user's behalf — not the agent reaching the filesystem. This is the
  same boundary logic that keeps `Bash` gated: the gate guards *agent* actions, and this is not
  one. Documented here so it is not mistaken for a hole.
- The file contains whatever the conversation contained, **including the system prompt** — that
  is the purpose of an audit export. Only the **path** is ever logged, never content.

## Decision 10 — typed errors, fail-secure

Per repo rules, concrete `errors.As`-able types (no bare `errors.New`/`fmt.Errorf` at package
APIs):

- `transcript`: reconstruction is best-effort and returns `[]Warning` rather than failing; a
  source read failure returns `*ReconstructError{Stage, Cause}`.
- `html`: `*RenderError{Cause}`.
- tui export cmd: `*ExportWriteError{Path, Cause}` for the file write; surfaced as an error
  notice. Home-dir-unresolvable / mkdir / write failures all deny the export (no partial file —
  atomic temp+rename guarantees all-or-nothing) and report the typed error.

## Decision 11 — security

- **XSS:** every dynamic value flows through `html/template` contextual escaping; markdown is
  rendered by goldmark with **raw-HTML passthrough disabled**. Explicit test: a user message of
  `<script>alert(1)</script>` and a tool result containing `</script><img onerror>` render inert.
- **No traversal:** UUID filename + `filepath.Clean`; write confined to `~/Downloads`.
- **No secret logging:** only the output path is logged.
- **Input boundary:** the journal record stream is treated as untrusted input — every unpaired /
  malformed / unexpected record degrades to a `Warning`, never a panic (fail-secure).

## Decision 12 — dependency: goldmark (approved)

`github.com/yuin/goldmark` — pure-Go, CommonMark-compliant, no cgo, widely used, safe HTML
output. **Approved by the user on 2026-06-28** for markdown→HTML in the renderer. Tasks: add to
looprig `go.mod`; record the sanction in looprig's dependency notes; note the transitive add in
swe's `CLAUDE.md` dependency list. The stdlib has no markdown renderer, which is why a dep is
warranted; goldmark is the single new direct dependency this feature introduces.

## Definitions

- **"N turns / M tools"** — counts over the reconstructed model (all loops).
- **Truncation** (titles, tool-output cap): single-line, newline-stripped, trimmed, capped with
  an ellipsis / byte-count note; full text always remains in the journal.
- **Snapshot** — the export reflects records journaled at call time; an in-flight turn may be
  partially present.

## Testing (explicit)

`pkg/transcript` (table-driven, `-race`, pure — synthetic record streams built from
`event`/`command` constructors):

1. Happy path: SessionStarted → LoopStarted → TurnStarted → StepDone(AI + tools) → TurnDone →
   full model; tool-use ↔ tool-result pairing by `ToolUseID` is correct.
2. Empty session (SessionStarted only) → valid model, no turns, no panic.
3. Turn with no steps; turn still `running` at snapshot edge (no terminal) → `Outcome: running`.
4. `TurnFailed` / `TurnInterrupted` → outcome + error text.
5. Permission gate **approved at session scope** (event + ApproveToolCall command) → resolved
   `GateAction{approved, session}` on the right tool call.
6. Gate **denied**; AskUser **answered** (UserInputRequested + ProvideUserInput) → answer captured.
7. Gate event with **no resolving command** (snapshot mid-prompt) → `Decision: pending`, no warning;
   resolving command with **no matching gate** → `Warning`.
8. **Subagent nesting**: child `LoopStarted{ParentToolUseID: X}` attaches under the `Subagent`
   call whose `ToolUseID == X`; two concurrent children with distinct ids → no cross-attachment.
9. **System prompt**: resolver returns text → on the loop; resolver `ok=false` → empty +
   `Warning` (restored-session path).
10. Orphan tool result (no matching tool-use); unknown event type → `Warning`, never panic
    (fail-secure / untrusted-input boundary).
11. `TurnFoldedInto` user input folds onto the turn; lifecycle events → `Notice`s in order.

`pkg/transcript/html`:

12. **Golden-file** render of a known model (timestamps normalized) — stable structure.
13. **Self-contained**: output has no external `src`/`href` (no network deps); inline style+script present.
14. **Collapsible markers** present for AI messages + thinking; **gate notification chips** present
    with scope/timestamp; **nested subagent** block present and indented.
15. **XSS** (security): `<script>`/`onerror` in user message, AI text, and tool result all render
    inert; goldmark raw-HTML passthrough is off.
16. **Fuzz** (`FuzzRenderText`): arbitrary bytes through the text/markdown→HTML path never produce
    unescaped `<script>`/attribute-injection; always terminates.

Integration (`//go:build integration`, `-tags integration -race`):

17. End-to-end against a real journal: drive a small session (turns, a gated tool, a subagent),
    `RecordReplayer` snapshot → `Reconstruct` → `Render` → atomic write → assert file exists,
    is valid UTF-8/HTML, and contains the system prompt, a gate chip, and the nested subagent.
18. `journal.RecordReplayer` surfaces **both** EventRecords and CommandRecords in stream-sequence
    order (the property `EventReplayer` lacks), and rehydrates object-store-offloaded records.

## Out of scope (v1)

- Non-HTML renderers (markdown/JSON/live-web) — the model is built to enable them; none ships now.
- Exporting an **arbitrary past** session by id from a fresh process (system-prompt text would be
  unavailable; only the current/live session is the `/export` target). The `RecordReplayer` makes
  this a later, small addition.
- Auto-opening the file in a browser; a configurable output directory; a friendlier filename.
- Streaming/incremental rendering of huge sessions (v1 buffers; tool-output byte cap bounds size).
- Embedding image/audio/document block bytes inline (v1 renders a typed placeholder with metadata).
