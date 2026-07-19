# Per-Loop Compaction Input Queue Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Reuse each loop's existing input inbox as a compaction barrier so only the compacting loop defers new messages until its durable terminal.

**Architecture:** `compactionControl.pending` remains the sole compaction lifecycle state and gains a read-only `blocksInput` query. The loop actor consults it at admission, folding, and next-turn boundaries, then wakes the existing FIFO only after finalization clears the compaction slot.

**Tech Stack:** Go standard library; Harness loop actor, compaction controller, event journal, and existing race-enabled tests.

---

### Task 1: Lock the idle-compaction regression

**Files:**
- Modify: `internal/loopruntime/safe_boundary_compaction_test.go`

**Step 1: Write the failing test**

Add a test that starts an idle manual compaction with a blocked executor, sends a
`UserInput`, observes `InputQueued`, and proves no `TurnStarted` occurs before the
compaction terminal.

**Step 2: Run the focused test to verify it fails**

Run: `go test -race ./internal/loopruntime -run TestLoopIdleCompactionQueuesInputUntilTerminal -count=1`

Expected: FAIL because the queued input starts while idle compaction is active.

### Task 2: Reuse the actor inbox as the compaction barrier

**Files:**
- Modify: `internal/loopruntime/compaction_control.go`
- Modify: `internal/loopruntime/loop.go`

**Step 1: Add the canonical query**

Add `blocksInput() bool` to `compactionControl`, returning whether its pending slot
is occupied.

**Step 2: Queue every input variant**

Include `blocksInput` in the busy branch shared by ordinary `UserInput`, managed
delegate input, and `SubagentResult`.

**Step 3: Guard every inbox exit**

Prevent execution admission and tool-continuation drain from removing inbox
entries while compaction blocks input.

**Step 4: Wake after durable finalization**

After every path that clears the compaction slot, request admission for the FIFO
head only when the loop is idle and the session is still healthy.

**Step 5: Run the focused test to verify it passes**

Run: `go test -race ./internal/loopruntime -run TestLoopIdleCompactionQueuesInputUntilTerminal -count=1`

Expected: PASS.

### Task 3: Cover input variants and lifecycle edges

**Files:**
- Modify: `internal/loopruntime/safe_boundary_compaction_test.go`
- Modify: `internal/loopruntime/compaction_control_loop_test.go`
- Modify: `internal/sessionruntime/subagent_result_test.go` if session-level coverage is needed

**Step 1: Add focused cases**

Cover `NoFold` input, `SubagentResult`, no folding at a step boundary, rejection
release, and shutdown cancellation. Assert compaction terminal ordering before any
queued state mutation.

**Step 2: Run focused packages**

Run: `go test -race ./internal/loopruntime ./internal/sessionruntime -count=1`

Expected: PASS.

### Task 4: Verify the CodeRig integration and full Harness module

**Files:**
- Modify: `../coderig/internal/app/compaction_acceptance_test.go` only if existing acceptance coverage cannot express the subagent target invariant

**Step 1: Run CodeRig compaction acceptance tests**

Run from `../coderig`: `go test -race ./internal/app -run Compaction -count=1`

Expected: PASS using the local Harness replacement.

**Step 2: Run the full Harness suite**

Run: `go test -race ./...`

Expected: PASS.

**Step 3: Verify formatting and diff**

Run: `gofmt -w internal/loopruntime/compaction_control.go internal/loopruntime/loop.go internal/loopruntime/*compaction*_test.go`

Run: `git diff --check`

Expected: both commands succeed with no formatting errors.
