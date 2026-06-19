# TUI Event Adoption — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Finish migrating the TUI to consume the loop-machine event model — the two
owned loop/event amendments (ToolCall\* → Ephemeral; `Disposition` → `Reply` events) and
the remaining TUI display work (input-queue removal, working-word headline, per-loop
prompt scoping) — per `docs/plans/2026-06-18-tui-event-adoption-design.md`.

**Architecture:** The StepDone-committed transcript, provisional-live, interrupt handling,
live→committed tool cards (option c), and the `SubscribeEvents` fan-in have **already
landed** on `main`. This plan implements only what the design's *Current state → Still
pending* lists. Engine amendments land first; the TUI changes that consume them follow.

**Tech Stack:** Go (stdlib-first), `charm.land/bubbletea/v2` TUI, table-driven `-race`
tests (CLAUDE.md). Packages: `internal/agent/loop/event`, `internal/agent/loop`,
`internal/agent/loop/command`, `internal/agent/session`, `tui/`.

**Before you start:** Work in the `.worktrees/tui-event-adoption` worktree (branch
`design/tui-event-adoption`, off `main`, has both the spec and the code). Read the design
doc's §3.3, §6, and the two *Loop/event amendment* sections — they are the contract. Run
`go test -race ./...` once to confirm a green baseline.

**Dependency order:** Phase 0 (quick fix) → Phase 1 (Amendment 1) → Phase 2 (working-word)
are independent and safe first. Phase 3 (Amendment 2 engine) gates Phase 4 (TUI input).
Phase 5 and 6 are independent TUI work.

---

## Phase 0 — Fix the `stepToolCard` fallback status (design §3.3 "code gap")

The committed tool-card fallback (no live card streamed) hardcodes `ToolOK`, ignoring
`ToolResultMessage.IsError`, so a *failed* subagent/dropped tool renders `✓`.

### Task 0.1: Fallback status from `IsError`

**Files:**
- Modify: `tui/transcript.go` (`stepToolCard`)
- Test: `tui/transcript_test.go`

**Step 1: Write the failing test** — drive `stepDone` with a step whose `ToolUseBlock` has
**no** matching live card and whose `ToolResultMessage` has `IsError: true`; assert the
committed card's `Status == ToolError`.

```go
func TestStepToolCardFallbackErrorStatus(t *testing.T) {
	t.Parallel()
	m := transcriptModel{}
	uid := "tu-1"
	ai := &content.AIMessage{Message: content.Message{Blocks: []content.Block{
		&content.ToolUseBlock{ID: uid, Name: "Bash"},
	}}}
	res := &content.ToolResultMessage{Message: content.Message{Role: content.RoleTool},
		ToolUseID: uid, IsError: true}
	m.stepDone(event.StepDone{Messages: content.AgenticMessages{ai, res}})
	// find the committed kindTool entry; assert ToolError
	got := lastToolCard(t, m) // small helper: returns the last committed ToolCallView
	if got.Status != ToolError {
		t.Fatalf("fallback status = %v, want ToolError", got.Status)
	}
}
```

**Step 2: Run it, verify FAIL** — `go test -race ./tui/ -run TestStepToolCardFallbackErrorStatus -v` → fails (`ToolOK`).

**Step 3: Implement** — in `stepToolCard`, set the fallback status from the result:

```go
card := ToolCallView{ToolName: use.Name, Status: ToolOK}
if r, ok := results[use.ID]; ok {
	card.Result = splitLines(toolResultText(r))
	if r.IsError {
		card.Status = ToolError
	}
}
return card
```

**Step 4: Run, verify PASS.** Then `go test -race ./tui/`.

**Step 5: Commit** — `fix(tui): stepToolCard fallback reads ToolResultMessage.IsError`.

---

## Phase 1 — Amendment 1: `ToolCall*` → Ephemeral (`internal/agent/loop/event`)

Pure class flip. `ToolCallStarted`/`ToolCallCompleted` are fully reconstructable from
`StepDone`, so they self-heal on drop (design amendment 1). The TUI already consumes them
regardless of class, so this is low-risk.

### Task 1.1: Reclassify the two structs

**Files:**
- Modify: `internal/agent/loop/event/tool.go:64-83` (the two structs)
- Modify: `internal/agent/loop/event/turn.go:69-70` (TokenDelta comment "the only Ephemeral event")
- Modify: `internal/agent/loop/event/filter.go:12` (comment lists "tool lifecycle" under Enduring)
- Test: `internal/agent/loop/event/event_test.go` (or wherever `Class()` is asserted)

**Step 1: Write the failing test** — assert the class:

```go
func TestToolEventsAreEphemeral(t *testing.T) {
	t.Parallel()
	if got := (event.ToolCallStarted{}).Class(); got != event.Ephemeral {
		t.Errorf("ToolCallStarted.Class() = %v, want Ephemeral", got)
	}
	if got := (event.ToolCallCompleted{}).Class(); got != event.Ephemeral {
		t.Errorf("ToolCallCompleted.Class() = %v, want Ephemeral", got)
	}
}
```

**Step 2: Run, verify FAIL** (`Enduring`).

**Step 3: Implement** — swap the mixin on both structs in `tool.go`:

```go
type ToolCallStarted struct {
	ephemeral
	loopScoped
	Header
	CallID   uuid.UUID
	ToolName string
	Summary  string
}

type ToolCallCompleted struct {
	ephemeral
	loopScoped
	Header
	CallID        uuid.UUID
	IsError       bool
	ResultPreview string
}
```

Update the stale comments: `turn.go` "It is the only Ephemeral event" → "TokenDelta and
the ToolCall\* events are Ephemeral"; `filter.go:12` drop "tool lifecycle" from the
Enduring comment and note it under Ephemeral.

**Step 4: Run, verify PASS.** Then `go test -race ./internal/agent/...` — watch for any
existing test that asserts "TokenDelta is the only Ephemeral event" or that tool events go
to sinks via the Enduring path; update those to match (they are now Ephemeral).

> **Check:** `ToolCallCompleted` implements `Redactable` (`SinkProjection` drops
> `ResultPreview`). Confirm the fan-in still applies `SinkProjection` to Ephemeral events
> bound for sinks (or that Ephemeral events are simply not sink-bound). If a test encodes
> "Enduring → sink", reconcile per the design's "redaction shrinks to one content-bearing
> event (StepDone)" note. Do **not** change redaction behavior here beyond the class flip.

**Step 5: Commit** — `feat(event): reclassify ToolCall* as Ephemeral (TUI-event-adoption amendment 1)`.

---

## Phase 2 — §3 rule 4: working-word headline / `● Done` (`tui/`)

Empty-text-with-tools steps currently commit a **bare `●`** (`commitStepAssistant`). The
design wants a live "working" synonym that commits to `● Done`.

### Task 2.1: Working-word list

**Files:**
- Modify: `tui/anim.go` (beside `spinnerFrames`)
- Test: `tui/anim_test.go`

**Step 1: Failing test** — assert a non-empty, stable list and a deterministic-by-index
picker (no RNG; index by a frame counter so it can rotate without flicker concerns —
design says it need not be deterministic, but a pure index keeps it testable):

```go
func TestWorkingWords(t *testing.T) {
	t.Parallel()
	if len(workingWords) == 0 { t.Fatal("workingWords empty") }
	if workingWord(0) != workingWords[0] { t.Errorf("workingWord(0) mismatch") }
	if workingWord(len(workingWords)) != workingWords[0] { t.Errorf("not wrapping") }
}
```

**Step 2: Run, verify FAIL.**

**Step 3: Implement** in `tui/anim.go`:

```go
// workingWords are the live "doing work" synonyms for an empty-text tool step.
// Cosmetic and live-only: the committed headline is always "Done" (design §3 rule 4).
var workingWords = []string{"Working", "Crunching", "Churning", "Toiling", "Cooking", "Whirring"}

func workingWord(i int) string { return workingWords[i%len(workingWords)] }
```

**Step 4: Run, verify PASS.**

**Step 5: Commit** — `feat(tui): add working-word list for empty-text tool steps`.

### Task 2.2: Render working-word (live) / `● Done` (committed)

**Files:**
- Modify: `tui/render.go` (`renderAssistant`/`renderLiveAssistant` bare-bullet branch)
- Modify: `tui/transcript.go` (`commitStepAssistant` — the card-only bare-bullet case)
- Test: `tui/render_test.go`, `tui/transcript_test.go`

**Step 1: Failing tests** — (a) a committed card-only segment renders `● Done` (bold);
(b) a live card-only segment renders a bold working-word (assert it is one of
`workingWords`). Use the existing render-test harness; assert on the rendered string
containing the bold `Done` / a working word at the dot.

**Step 2: Run, verify FAIL** (currently a bare `●`).

**Step 3: Implement** — committed path: when an assistant segment has no thinking/text but
has cards, emit a bold `Done` headline beside the dot instead of the bare bullet (replace
the `body == "" && len(calls) > 0` bare-bullet branch in `renderAssistant`). Live path
(`renderLiveAssistant`): emit `liveDot + bold(workingWord(a.frame))`. Keep the
`commitStepAssistant` change minimal — it already commits "one bare kindAssistant entry"
for card-only; the *renderer* decides bare-`●` vs `Done`/working-word, so most of the
change is in `render.go`. Verify the renderer can tell "card-only assistant entry" from
"prose entry" (it can: empty markdown + non-empty `calls`).

**Step 4: Run, verify PASS.** Then `go test -race ./tui/`.

**Step 5: Commit** — `feat(tui): working-word live / Done committed for empty-text tool steps`.

> **Manual check:** use the `run` skill / launch the TUI and trigger a tool-only step
> (e.g. a prompt that makes the agent call a tool with no preamble); confirm the live
> headline animates a working word and the frozen entry reads `● Done`.

---

## Phase 3 — Amendment 2: `Disposition` → `Reply` events (engine) ⟵ gates Phase 4

The biggest change. **Read first:** `internal/agent/loop/command/submit.go`,
`internal/agent/loop/ack.go`, `internal/agent/loop/turn.go` (where `Disposition` is
constructed/acked), the session quiescence code in `internal/agent/session/` (the
`expectTurn`/`cancelExpectTurn`/wake-release model), and the design's second amendment +
"Why it is safe — quiescence is untouched". Do not start coding until you can name every
current `Disposition`/`Ack` call site.

### Task 3.1: New events `InputQueued` (Ephemeral) + `TurnRejected` (Enduring)

**Files:**
- Modify: `internal/agent/loop/event/turn.go` (add the two structs + `isEvent`)
- Test: `internal/agent/loop/event/turn_test.go`

**Step 1: Failing test** — assert both exist with the right class and carry `InputID`;
`TurnRejected` carries a `Reason`:

```go
func TestInputReplyEventClasses(t *testing.T) {
	t.Parallel()
	if (event.InputQueued{}).Class() != event.Ephemeral { t.Error("InputQueued not Ephemeral") }
	if (event.TurnRejected{}).Class() != event.Enduring { t.Error("TurnRejected not Enduring") }
}
```

**Step 2: Run, verify FAIL** (types don't exist).

**Step 3: Implement** in `turn.go`:

```go
// InputQueued is the Ephemeral Reply event for a UserInput accepted into the loop
// inbox but not yet assigned to a turn. Header.CausationID == the submit command id.
// Self-heals: a later TurnStarted/TurnFoldedInto/InputCancelled is authoritative.
type InputQueued struct {
	ephemeral
	loopScoped
	Header
	InputID uuid.UUID
}

// TurnRejected is the Enduring Reply event for a UserInput the loop refused
// (queue-full or shutting-down). Enduring: a rejected message must never silently
// vanish. Header.CausationID == the submit command id.
type TurnRejected struct {
	enduring
	loopScoped
	Header
	InputID uuid.UUID
	Reason  RejectReason
}

func (InputQueued) isEvent()  {}
func (TurnRejected) isEvent() {}
```

Reuse the existing `RejectReason` if `command` already defines one; otherwise add a typed
enum in `event` (named constants, CLAUDE.md — no magic ints).

**Step 4: Run, verify PASS.**

**Step 5: Commit** — `feat(event): add InputQueued (Ephemeral) and TurnRejected (Enduring) Reply events`.

### Task 3.2: `Reply` marker interface

**Files:**
- Modify: `internal/agent/loop/event/event.go` (add `Reply` interface) + the 5 events
- Test: `internal/agent/loop/event/event_test.go`

**Step 1: Failing test** — assert the 5 reply events satisfy `Reply` and `ReplyTo()`
returns `Header.CausationID`; assert a non-reply (e.g. `StepDone`) does **not**:

```go
func TestReplyInterface(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV4())
	var r event.Reply = event.TurnRejected{Header: event.Header{CausationID: id}}
	if r.ReplyTo() != id { t.Errorf("ReplyTo() = %v, want %v", r.ReplyTo(), id) }
	if _, ok := any(event.StepDone{}).(event.Reply); ok { t.Error("StepDone must not be a Reply") }
}
```

**Step 2: Run, verify FAIL.**

**Step 3: Implement** — add the interface and a shared `ReplyTo` (the `Header` already has
`CausationID`, so a method on `Header` covers all five):

```go
// Reply is an event that is the direct outcome of a command, delivered on the normal
// fan-in (classed Ephemeral/Enduring like any other event). Typed replacement for the
// command.Disposition reply channel.
type Reply interface {
	Event
	isReply()
	ReplyTo() uuid.UUID // == Header.CausationID
}

func (h Header) ReplyTo() uuid.UUID { return h.CausationID }
```

Add `func (TurnStarted) isReply() {}` … for `TurnStarted, InputQueued, TurnRejected,
TurnFoldedInto, InputCancelled`. (`Header.ReplyTo` is promoted, so only `isReply` is
per-type.)

**Step 4: Run, verify PASS.** Then `go test -race ./internal/agent/loop/event/`.

**Step 5: Commit** — `feat(event): add Reply marker interface over CausationID`.

### Task 3.3: Publish replies as events; drop the `Disposition`/`Ack` from submit

**Files:**
- Modify: `internal/agent/loop/command/submit.go` (drop `Ack chan<- Disposition`)
- Modify: `internal/agent/loop/ack.go`, `internal/agent/loop/turn.go` / `runLoop`
  (replace `tryAck(Disposition)` with `emit(event.InputQueued{…})` /
  `emit(event.TurnRejected{…})`; `TurnStarted`/`TurnFoldedInto`/`InputCancelled` already
  emit)
- Modify: `internal/agent/loop/errors.go` (the "TurnRejected disposition" path → event)
- Modify: any session caller that read the `Disposition` reply
- Test: `internal/agent/loop/*_test.go` (drive a submit; assert the published events)

**Step 1: Failing test** — submit a `UserInput` to a busy loop (queueable) via the loop's
public submit; assert an `event.InputQueued` with `CausationID == submit id` is published;
submit when shutting down → assert `event.TurnRejected{Reason: shutdown}`. (Use the
existing loop test harness / a fake `eventPublisher` that records.)

**Step 2: Run, verify FAIL.**

**Step 3: Implement** — at each former `tryAck(Disposition{...})` site, publish the
corresponding event instead, stamping `Header.CausationID` = submit command id. Remove the
`Ack chan<- Disposition` field from `command.UserInput` (and `SubagentResult`); make submit
return only a transport error (loop gone). Delete the now-unused `Disposition` type once no
caller references it (`go build` + `staticcheck` will surface stragglers).

**Step 4: Run, verify PASS.** Then `go test -race ./internal/agent/...`.

**Step 5: Commit** — `feat(loop): publish InputQueued/TurnRejected as events; drop UserInput Disposition ack`.

### Task 3.4: `SubagentResult` never rejected; drop `CancelResult` ack

**Files:**
- Modify: the inbox-cap check (`internal/agent/loop/…` `inboxCap`) so `SubagentResult`
  bypasses the cap (never `TurnRejected`)
- Modify: remove the `cancelExpectTurn`-after-`TurnRejected`-for-`SubagentResult` path in
  the session quiescence code
- Modify: `internal/agent/loop/command/…` `CancelQueuedInput` — drop `Ack chan<- CancelResult`;
  its outcome is the existing `event.InputCancelled`
- Test: loop + session quiescence tests

**Step 1: Failing tests** — (a) a `SubagentResult` to a loop whose inbox is at `inboxCap`
is still accepted (no `TurnRejected`); (b) after this change, `WaitIdle`/quiescence still
resolves correctly when a subagent hands back into a busy parent (the wake token releases
on the `TurnStarted`/`TurnFoldedInto` event, not on a disposition). Reuse existing
quiescence tests as the template.

**Step 2: Run, verify FAIL.**

**Step 3: Implement** — gate the cap check on command kind (UserInput only); route
`SubagentResult` past it. Remove the disposition-driven `cancelExpectTurn` branch (the
wake table now releases purely on events — design "adds zero rows"). Drop `CancelResult`.

**Step 4: Run, verify PASS.** Then `go test -race ./internal/agent/...` and
`make secure` (quiescence is concurrency-sensitive — the `-race` run is the real gate).

**Step 5: Commit** — `feat(loop): SubagentResult never rejected; drop CancelResult ack`.

---

## Phase 4 — §6: TUI input-queue removal + `Reply` consumption (depends on Phase 3)

**Read first:** `tui/screen.go` (`queue`, the self-drain at 187-201, `CommitUser`),
`tui/transcript.go` (`CommitUser`), `tui/agent.go` (how it submits + subscribes), design §6.

### Task 4.1: Submit fire-and-forget; delete `Screen.queue`

**Files:**
- Modify: `tui/screen.go` (remove `queue [][]content.Block` + its drain + `DisplayIndex` plumbing)
- Modify: `tui/agent.go` (submit `command.UserInput` fire-and-forget; no ack read)
- Test: `tui/screen_test.go`

**Step 1: Failing test** — submitting while a turn runs no longer appends to a TUI queue
and does not itself commit a user row (the row now comes from `event.TurnStarted`). Assert
`Screen` has no `queue` field path exercised (the test will fail to compile against the old
field, which is the point — rewrite it to the new behavior).

**Step 2–4:** Remove the field + drain; submit fire-and-forget; the user row is committed
only by the event path (Task 4.2). Build + `go test -race ./tui/`.

**Step 5: Commit** — `refactor(tui): remove Screen input queue; submit UserInput fire-and-forget`.

### Task 4.2: Consume `Reply` events for the input lifecycle

**Files:**
- Modify: `tui/transcript.go` / `tui/screen.go` (handle the 5 events) + a small
  `InputID`-keyed `queued` map for the pending affordance
- Test: `tui/transcript_test.go`

**Step 1: Failing tests** (drive `ApplyEvent`):
- `event.InputQueued{InputID}` → a queued affordance keyed by `InputID` renders below the live tail.
- `event.TurnStarted{InputID, Message}` → commits a normal user row from `Message`, removes the queued affordance, **exactly once**.
- `event.TurnFoldedInto{InputID, Message}` → commits a user row appended at the committed tail.
- `event.InputCancelled{InputID}` → drops the queued affordance.
- `event.TurnRejected{InputID}` → drops the affordance + surfaces a rejection notice; a terminal event does **not** drop a queued message.

**Step 2: Run, verify FAIL.**

**Step 3: Implement** — route each event; commit user rows from `*.Message` keyed by
`InputID`; the queued set is a `map[uuid.UUID]queuedMsg`. User rows become authoritative
from the event (design §6 "displayed == stored for user rows too").

**Step 4: Run, verify PASS.** Then `go test -race ./tui/`.

**Step 5: Commit** — `feat(tui): consume Reply events for the input lifecycle (§6)`.

---

## Phase 5 — §7: per-loop gate-prompt scoping (`tui/interaction.go`)

### Task 5.1: Scope `ClearPrompts` to the finishing `LoopID`

**Files:**
- Modify: `tui/interaction.go` (`ClearPrompts` → per-loop), `tui/screen.go` (terminal handling)
- Test: `tui/interaction_test.go`

**Step 1: Failing test** — two loops each with a pending gate prompt; a terminal event for
loop A clears A's prompt only, not B's.

**Step 2–4:** Key the prompt queue by `LoopID` (gate prompts already carry the route per
design §7); a terminal event clears only the matching loop's prompts. Build + test.

**Step 5: Commit** — `fix(tui): scope gate-prompt clearing per loop (§7)`.

---

## Phase 6 — §1/§5: verify lifetime subscription + `LoopID` attribution

Much of §1 (the `SubscribeEvents` fan-in) and §5 (LoopID tagging) is partly landed
(`tui/agent.go`). This phase **audits** rather than builds.

### Task 6.1: Audit and close gaps

**Step 1:** Read `tui/agent.go` + `tui/transcript.go`. Confirm: (a) the TUI holds **one**
session-lifetime subscription (not per-turn `Stream()`); (b) the live segment and committed
entries are keyed/tagged by `LoopID`; (c) the multi-loop default filter is
`{Ephemeral: primary-only, Enduring: all}`.

**Step 2:** For each gap, add a failing test (design Testing list: "subscribes once per
session"; "with `{Ephemeral: primary-only}` a subagent's `TokenDelta` never renders but its
`StepDone`/tool/gate events do, attributed by `LoopID`"), implement, commit. If all three
already hold, write the locking tests anyway and commit them.

---

## Final verification

1. `go test -race ./...` — green.
2. `make secure` — lint + vet + staticcheck + gosec + govulncheck (CLAUDE.md).
3. Re-read the design's **Testing** section; confirm every bullet has a test.
4. Manual: launch the TUI (the `run` skill), exercise a multi-step tool turn, a queued
   input while running, an interrupt mid-tool; confirm the design's behaviors.
5. Update the design's *Current state* (move the now-done items from *pending* to
   *landed*).

---

## Notes / sequencing

- **Phases 0–2 can land before Phase 3** — they don't depend on the engine amendments and
  de-risk early.
- **Phase 4 strictly follows Phase 3** (it consumes the new events).
- **Redaction is out of scope** (design + loop-machine "Open Items B"); do not add
  `Redactable` to `StepDone` or the new events here.
- **Names track loop-machine** (`CausationID`/`CallID`/`InputID`), not id-normalization;
  do not rename. The committed tool card needs no `ToolUseID` on the live events (it
  matches by position; design §3.3 + id-normalization note).
- Keep commits small and per-task; every test runs with `-race` (CLAUDE.md).
