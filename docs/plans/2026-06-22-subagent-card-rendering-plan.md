# Subagent Card Rendering Implementation Plan (v3)

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Render each `Subagent` tool call as a `Subagent(<agent>)  "<task>"` card with the subagent's tool calls + final result nested as `⎿` children, committed as one block — replacing the out-of-order `▸ <agent>: done` lines.

**Architecture:** Paint the card entirely from **Enduring** child events (already delivered to the TUI from all loops; replayed on restore): `LoopStarted` (agent), first child `TurnStarted` (task), child `StepDone` (tool cards via `splitStepGroup`), child terminal (done/failed/interrupted). Correlate child loop → parent card by the **durable provider tool-use id** (`content.ToolUseBlock.ID`), threaded through ctx → `Subagent` tool → `Spawner` → `Session.RunSubagent` → one new event field `event.LoopStarted.ParentToolUseID`. No identity/`Provenance` struct changes. `ToolExecutionID` (runner UUID) is kept for gate routing only — it is not persisted/replayed.

**Tech Stack:** Go 1.26; looprig `pkg/loop`, `pkg/event`, `pkg/session`, `pkg/tools`, `pkg/tui`; swe `swarms/swe`; bubbletea v2. Design: `docs/plans/2026-06-22-subagent-card-rendering-design.md`.

**Scope:** synchronous single + multiple-concurrent. Async/background = documented hook only. Red-symbols + big-gap bugs are separate.

**Conventions:** TDD (failing test first); `go test ./pkg/<pkg>/ -run <Name> -v`; commit per green task. Tasks 3–5 thread one parameter across `looprig` AND `swe`; keep both module builds green within each (update all call sites). Final: `go build ./... && go test ./...` in BOTH repos.

---

### Task 1: Runner injects provider tool-use id into ctx; export `loop.ToolUseIDFrom`

**Files:**
- Modify: `pkg/loop/gate.go` (add `withToolUseID` + private `toolUseIDFromContext`, mirroring `withCallID`/`callIDFromContext`)
- Create exported accessor in: `pkg/loop/provenance_ctx.go` (`ToolUseIDFrom`, and `WithToolUseID` for tests)
- Modify: `pkg/loop/runner.go:485` (`runOne`) — inject `r.block.ID` alongside `withCallID`
- Test: `pkg/loop/provenance_ctx_test.go`

**Step 1 (failing test):**
```go
func TestToolUseIDFrom(t *testing.T) {
	ctx := WithToolUseID(context.Background(), "toolu_123")
	if got, ok := ToolUseIDFrom(ctx); !ok || got != "toolu_123" {
		t.Fatalf("ToolUseIDFrom = (%q,%v), want (toolu_123,true)", got, ok)
	}
	if _, ok := ToolUseIDFrom(context.Background()); ok {
		t.Error("ToolUseIDFrom on bare ctx = ok, want !ok")
	}
}
```
**Step 2:** `go test ./pkg/loop/ -run TestToolUseIDFrom -v` → FAIL.
**Step 3 (implement):** add the unexported `withToolUseID(ctx, id string)` + `toolUseIDFromContext` (private, gate.go), exported `func ToolUseIDFrom(ctx) (string, bool)` + `func WithToolUseID(ctx, id string) context.Context` (provenance_ctx.go), and in `runOne` wrap the per-call ctx:
```go
ctx2 := WithPrepared(withGateReg(withEmit(withToolUseID(withCallID(ctx, r.callID), r.block.ID), emit), gateReg), r.prepared)
```
**Step 4:** test PASS. **Step 5:** `git commit -m "feat(loop): expose per-call provider ToolUseID via ctx"`.

---

### Task 2: `event.LoopStarted.ParentToolUseID`

**Files:**
- Modify: `pkg/event/event.go:243` (struct + field)
- Check/Modify: `pkg/event/marshal.go` (round-trip the field if LoopStarted has a custom JSON form — it is Enduring/persisted)
- Test: `pkg/event/marshal_test.go`

**Step 1 (failing test):** marshal→unmarshal a `LoopStarted{... ParentToolUseID:"toolu_9"}`; assert the field survives. (Adapt to the package's `Marshal`/`Unmarshal`.)
**Step 2:** FAIL. **Step 3:** add `ParentToolUseID string \`json:"parent_tool_use_id,omitempty"\`` to `LoopStarted`; thread it in `marshal.go` if a wire struct exists. **Step 4:** PASS. **Step 5:** `git commit -m "feat(event): LoopStarted.ParentToolUseID"`.

---

### Task 3: Session stamps `ParentToolUseID`; `RunSubagent` gains the param (private loop-creation path)

**Files:**
- Modify: `pkg/session/session.go` — extract a private `newLoop(parent, cfg, parentToolUseID string)`; public `NewLoop(parent, cfg)` delegates with `""`; build site `:560` uses `ParentToolUseID: parentToolUseID`. `RunSubagent(... , parentToolUseID string)` passes it to `newLoop`.
- Test: `pkg/session/session_test.go`

**Step 1 (failing test):** call `RunSubagent(ctx, parent, cfg, blocks, "toolu_7")` (fake/stub loop), subscribe, assert the child `LoopStarted.ParentToolUseID == "toolu_7"`; and that `NewLoop`/primary yields `""`.
**Step 2:** FAIL (signature/field). **Step 3:** implement the private path + signature. Update the in-package call sites. **Step 4:** PASS. **Step 5:** `git commit -m "feat(session): RunSubagent threads ParentToolUseID onto LoopStarted"`.

> Note: this changes `Session.RunSubagent`'s signature, which swe calls (Task 5). Keep the swe build green by completing Tasks 4–5 before the cross-repo `go build`.

---

### Task 4: `Spawner.Spawn` gains `parentToolUseID`; `Subagent` reads it from ctx

**Files:**
- Modify: `pkg/tools/subagent.go` — `Spawner.Spawn(ctx, parent, agent, message, parentToolUseID string)`; in `InvokableRun`, `tuid, _ := loop.ToolUseIDFrom(ctx)` and pass `tuid`.
- Modify: `pkg/tools/subagent_test.go` (`fakeSpawner.Spawn` signature + assertion)

**Step 1 (failing test):**
```go
func TestSubagentForwardsToolUseID(t *testing.T) {
	ctx := loop.WithToolUseID(context.Background(), "toolu_55")
	fs := &fakeSpawner{}
	_, _ = NewSubagent(fs, catalog).InvokableRun(ctx, `{"agent":"explorer","message":"map repo"}`)
	if fs.gotToolUseID != "toolu_55" {
		t.Fatalf("Spawn parentToolUseID = %q, want toolu_55", fs.gotToolUseID)
	}
}
```
**Step 2:** FAIL. **Step 3:** add the param + forward; absent id → `""` (graceful). **Step 4:** PASS. **Step 5:** `git commit -m "feat(tools): Subagent forwards provider ToolUseID to Spawner"`.

---

### Task 5: swe spawner pass-through

**Files:**
- Modify: `swarms/swe/spawner.go` — `swarmSpawner.Spawn(... , parentToolUseID string)`; its internal `RunSubagent` interface gains the param; forward to `session.RunSubagent(..., parentToolUseID)`.
- Test: `swarms/swe/spawner_test.go`

**Step 1 (failing test):** assert `swarmSpawner.Spawn(ctx, parent, "explorer", "msg", "toolu_3")` forwards `"toolu_3"` to a fake `RunSubagent` seam. **Step 2:** FAIL. **Step 3:** thread it. **Step 4:** PASS + `cd looprig && go build ./...` and `cd swe && go build ./...` both green. **Step 5:** `git commit -m "feat(swe): thread ParentToolUseID through swarm spawner"`.

---

### Task 6: TUI reducer — detached accumulator from Enduring child events

**Files:**
- Modify: `pkg/tui/message.go:35` (`ToolCallView` gains `Children []ToolCallView`, `Steps int`, `Agent string`, `Task string`, `SubStatus subStatus`, `Nested int`)
- Modify: `pkg/tui/transcript.go` (accumulator + reconciliation; retain `kindSubagent`/`commitSubagentLine` ONLY as the empty-`ParentToolUseID` fallback)
- Test: `pkg/tui/transcript_test.go`

**Step 1 (failing tests):**
1. *Enduring-only paint + ID-namespace:* feed `LoopStarted{LoopID:sub, AgentName:"explorer", Cause:{LoopID:primary,TurnID:t,StepID:s}, ParentToolUseID:"toolu_X"}`; child `TurnStarted{LoopID:sub, Message:"map repo"}`; child `StepDone{LoopID:sub, Messages:[AIMessage with a Grep tool-use + its ToolResult]}`; child `TurnDone{LoopID:sub}`; then the ORCHESTRATOR `StepDone{LoopID:primary, TurnID:t, StepID:s, Messages:[AIMessage with a Subagent ToolUseBlock{ID:"toolu_X"} + its ToolResult "result text"]}`. Assert: one committed `kindTool` card; `Agent=="explorer"`, `Task=="map repo"`, `Children` has the Grep row, `Steps==1`, done summary == "result text". Use a `ToolExecutionID` value DIFFERENT from `"toolu_X"` anywhere it appears, proving the match is by provider id.
2. *Concurrent:* two children, `ParentToolUseID` "toolu_A"/"toolu_B" under one orchestrator step with two Subagent blocks — each card gets only its own child rows.
3. *Empty ParentToolUseID:* child `LoopStarted` with `ParentToolUseID:""` → no accumulator; a fallback `▸ explorer: done` line commits on its `StepDone`.
4. *Mixed-batch same-index isolation (review-mandated):* parent batch `Bash`(idx0)+`Subagent`(idx1); the child's FIRST tool is also `Bash` with a DIFFERENT durable result. While the parent's live `Bash` card exists in `m.live.Calls[0]`, feed the child `StepDone` and assert the nested child `Bash` card carries the CHILD's result, NOT the parent live card.

**Step 2:** FAIL. **Step 3 (implement):**
- `ToolCallView` fields above.
- **Extract `storedStepToolCard(use, results)`** — the PURE card builder (stored tool-use block + `ToolResultMessage` paired by `use.ID`; NO `m.live.Calls` access). Refactor the existing `stepToolCard` fallback to call it (DRY); `stepToolCard` keeps the live-preference branch for the PRIMARY loop only. Child reconstruction uses `storedStepToolCard` EXCLUSIVELY.
- `transcriptModel`: `loopParent map[uuid.UUID]spawnKey`, `subagentAccum map[spawnKey]*subagentAccum` (clone-on-write). `spawnKey{parentLoopID,parentTurnID,parentStepID uuid.UUID; toolUseID string}`.
- `ApplyEvent`: for `LoopStarted` with non-empty `ParentToolUseID`, record `loopParent[LoopID]=spawnKey{Cause.LoopID,Cause.TurnID,Cause.StepID,ParentToolUseID}` and init the accum (agent from `AgentName`). For child `TurnStarted` (LoopID in loopParent, first one) set `task`. For child `StepDone` (LoopID in loopParent): `splitStepGroup` then `storedStepToolCard` per use to append `children`, `steps++`. For child terminal: set status. DELETE the old `commitSubagentLine` call for loops WITH a parent; KEEP it only when `LoopID` has NO `loopParent` entry (empty/absent `ParentToolUseID`).
- Reconcile in `stepDone` (primary branch): for each `Subagent` `ToolUseBlock`, look up `subagentAccum[spawnKey{ev.LoopID, ev.TurnID, ev.StepID, block.ID}]`; attach `children/steps/agent/task/status`; set done summary from the block's `ToolResultMessage` text (truncated); mark the card to SUPPRESS its normal result body.

**Step 4:** PASS. **Step 5:** `git commit -m "feat(tui): nest subagent activity from enduring events keyed by provider ToolUseID"`.

---

### Task 7: TUI render — `●` Subagent cards, `⎿` children, suppress doubling, drop umbrella, nested counter

**Files:**
- Modify: `pkg/tui/render.go` / `pkg/tui/entryrender.go` (render `Subagent(<agent>) "<task>"` header + indented `⎿` children + `done · N steps — summary`; suppress the card's own result body when flagged)
- Modify: `pkg/tui/transcript.go` `stepDone`/`commitStepAssistant` (promote every `Subagent` use to a `●` card; no "Multiple actions" umbrella when all uses are `Subagent`; depth-2 `Nested` counter via `Cause.LoopID` ancestry walk)
- Test: `pkg/tui/render_test.go`, `pkg/tui/transcript_test.go`

**Step 1 (failing tests):** render snapshot of a populated Subagent `ToolCallView` → header `Subagent(explorer)`, two `⎿` child lines, `done · 6 steps`; result body NOT duplicated; an all-Subagent two-call step has no `Multiple actions`; a depth-2 `StepDone` bumps the depth-1 card's `Nested` (ancestry walk), not a new card.
**Step 2:** FAIL. **Step 3:** implement. **Step 4:** PASS. **Step 5:** `git commit -m "feat(tui): render nested Subagent cards; drop umbrella; nested counter"`.

---

### Task 8: Restore equivalence + terminal/gate cases + integration + manual verify

**Files:** `pkg/tui/restore_test.go` (or transcript_test.go), `pkg/tui/screen_test.go`

**Tests (each: failing → implement-if-needed → pass):**
- **Restore equivalence:** build the live nested card from the Enduring sequence, then `FoldDisplay` the same Enduring slice and assert `EqualTranscript`.
- **Failure before child loop:** orchestrator `StepDone` with a `Subagent` block whose `ToolResult` is `"error: subagent failed: …"` and NO child `LoopStarted` → card shows the error body, no children.
- **Interruption:** child `TurnInterrupted` → `interrupted` child.
- **Mixed batch:** narration + a normal `Bash` `⎿` card + a `Subagent` `●` card in one step → topology holds.
- **Child gate routable:** child `PermissionRequested` (LoopID=sub) still enqueues a prompt for the sub loop (interaction model unchanged).

**Manual:** `cd swe && make run`; ask a repo question that triggers an `explorer` spawn; confirm `● Subagent(explorer) "<task>"` + `⎿` children + single `done` line, in order, no stray `▸` lines. Use @superpowers:verification-before-completion before claiming done.

**Final:** `cd looprig && go build ./... && go test ./...`; `cd swe && go build ./... && go test ./...` → all green. `git commit -m "test: subagent card rendering — restore, terminal, gate, integration"`.

---

## Notes / risks

- **Marshal:** confirm `LoopStarted` wire form round-trips `ParentToolUseID`; an old persisted event unmarshals to `""` → that subagent won't nest on restore of an old session (acceptable; falls to no-card).
- **`kindSubagent` retained** strictly as the empty-`ParentToolUseID` fallback line; don't delete the type.
- **Live-tail capacity:** nested children inflate the tail; existing `cappedTail` drops oldest rows — eyeball during Task 8.
- **Gate routing:** `ToolExecutionID` remains the permission/AskUser key (Ephemeral/live only); the new provider-id link is for display correlation only — keep the two separate.
