# Permission Request Details Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Display concrete permission request and tool-call details in active prompts, live cards, committed parent cards, and subagent nested cards.

**Architecture:** Keep the behavior in `pkg/tui`. Active prompts read `prompt.Description`; transcript history stores permission gate metadata by `ToolExecutionID` and copies it onto `ToolCallView` when the matching tool call starts. Tool cards render through a shared detail helper that prefers permission descriptions, then normalized live summaries, then summaries reconstructed from stored `ToolUseBlock.Input`.

**Tech Stack:** Go, existing Bubble Tea/lipgloss TUI, stdlib only.

---

### Task 1: Active Prompt Details

**Files:**
- Modify: `pkg/tui/prompt_render_test.go`
- Modify: `pkg/tui/prompt.go`
- Test: `pkg/tui/prompt_render_test.go`

**Step 1: Write the failing test**

Add cases proving `renderPermissionBox` includes `BashRequest.Description()` and `FetchRequest.Description()`.

**Step 2: Run test to verify it fails**

Run: `go test -race ./pkg/tui -run TestRenderPermissionBox`
Expected: FAIL because the description is absent.

**Step 3: Implement**

Render the wrapped description above the scope-key legend when non-empty.

**Step 4: Run test to verify it passes**

Run: `go test -race ./pkg/tui -run TestRenderPermissionBox`
Expected: PASS.

### Task 2: Transcript Permission Details

**Files:**
- Modify: `pkg/tui/message.go`
- Modify: `pkg/tui/transcript.go`
- Modify: `pkg/tui/render.go`
- Create: `pkg/tui/toolsummary.go`
- Modify: `pkg/tui/transcript_test.go`
- Modify: `pkg/tui/render_test.go`
- Test: `pkg/tui/transcript_test.go`

**Step 1: Write the failing test**

Extend `TestGateDecisionFlow` to assert the committed card renders the permission description.

**Step 2: Run test to verify it fails**

Run: `go test -race ./pkg/tui -run TestGateDecisionFlow`
Expected: FAIL because committed cards render the audit summary but not the permission description.

**Step 3: Implement**

Store permission metadata from `PermissionRequested`, carry it onto `ToolCallView`, and render it in the card header when present. Normalize live audit summaries so the header does not duplicate the tool name. Reconstruct stored summaries from known `ToolUseBlock.Input` fields for parent fallback cards and subagent nested cards.

**Step 4: Run focused tests**

Run:

```bash
go test -race ./pkg/tui -run 'TestRenderPermissionBox|TestGateDecisionFlow|TestToolHeaderTextNormalizesAuditableSummaries|TestStoredStepToolCardSummarizesInput|TestSubagentMixedBatchSameIndexIsolation|TestSurfaceViewPermissionPrompt'
```

Expected: PASS.

### Task 3: Final Verification

Run:

```bash
make fmt
go test -race ./pkg/tui ./pkg/tool ./pkg/tools
```

Expected: PASS.
