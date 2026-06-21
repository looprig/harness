# SWE Swarm — P4 Implementation Plan (Polish: startup greeting + per-agent TUI labels)

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Two optional UX niceties that gate nothing: (a) a deterministic UI-only startup greeting
listing the swarm's agents/skills; (b) per-agent attribution labels in the TUI transcript.

**Architecture:** The greeting is rendered from the `AgentRegistry` as the opening transcript entry
— **not** a turn/command, **not** in the model context. Labels map `LoopID → agent` via the
`AgentName` already stamped on `LoopStarted` (P1) and render `▸ <agent>: running/done`. Depends on
P1 (registry + `AgentName`); the greeting is richer once P2 exists (skills in the listing).

**Tech Stack:** Go 1.26; `tui/`. Design: `docs/plans/2026-06-21-swe-swarm-roles-design.md` §5a, §6f.

**Conventions:** as in `2026-06-21-swe-swarm-roles-impl.md`.

---

## Phase 1 — Startup greeting (§5a, UI-only, optional)

### Task 1.1: render a deterministic opening transcript entry

**Files:** Modify `tui/` opening render + `swarms/swe` (pass the registry catalog to the TUI); add a
`--greeting`/config toggle (default off or on — pick one; plan assumes **off by default**). Test:
`tui/` greeting test + `swarms/swe`.

**Step 1 — failing tests:**
- greeting text is **derived from `registry.Catalog()`** (agents; skills when P2 present) —
  deterministic, no LLM call.
- it emits **no command and no turn**, and the primary loop's `state.msgs` stays **empty** until the
  first real user message (lifecycle untouched).
- toggle off → no greeting.

**Step 2-4. Commit:** `feat(tui): deterministic UI-only startup greeting from the agent registry`.

---

## Phase 2 — Per-agent transcript labels (§6f, optional)

### Task 2.1: map `LoopID → agent` and render a labeled line

**Files:** Modify `tui/transcript.go` (+ wherever `LoopStarted` is consumed) to record
`LoopID → AgentName` from `LoopStarted.AgentName` (P1), and render subagent activity as
`▸ <agent>: running/done` on the collapsed-but-present line. Test: `tui/transcript_test.go`.

**Step 1 — failing tests:** a subagent loop's `StepDone`/terminal renders attributed to its agent
name; the primary (orchestrator) is not double-labeled; an unknown/empty `AgentName` falls back to
the loopID short form. **Step 2-4. Commit:** `feat(tui): per-agent transcript attribution labels`.

**P4 gate:** `go test -race ./tui/... && make secure` green; greeting renders deterministically and
does not touch the model context; subagent lines are agent-labeled.
