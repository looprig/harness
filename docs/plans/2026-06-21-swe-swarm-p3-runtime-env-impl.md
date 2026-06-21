# SWE Swarm — P3 Implementation Plan (Runtime/env context injection)

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Inject volatile per-turn context (date, cwd, git branch/status) into the model **without
invalidating the cached system-prompt prefix** — by appending it at the turn tail, append-to-update.

**Architecture:** A narrow `RuntimeContextProvider` the loop calls **before each turn**; it returns
`content.Block`s appended to the turn input (the volatile tail). The cached prefix (system prompt =
identity+role) is **never** edited; updates are a fresh appended block (or a short diff). Engine
capability, orthogonal to the agent packages. Depends on P1.

**Tech Stack:** Go 1.26 stdlib (`time`, `os`, `os/exec` for git via argv — never a shell string).
Design: `docs/plans/2026-06-21-swe-swarm-roles-design.md` §5 (runtime row), §13 P3.

**Conventions:** as in `2026-06-21-swe-swarm-roles-impl.md`. **Security:** git via `exec.Command`
argv list (no shell); bounded output; context timeout; never log/inject secrets.

---

## Phase 1 — Provider

### Task 1.1: `RuntimeContextProvider` interface + a default impl

**Files:** Create `internal/agent/loop/runtime_context.go` (interface) + a concrete provider
(recommend `swarms/swe/runtime_context.go` at the composition root, so the engine stays generic);
Test: provider test.

- `type RuntimeContextProvider interface { Blocks(ctx context.Context) []content.Block }`.
- Default provider returns one block: current date (injected clock seam, not `time.Now()` directly
  for testability), cwd, and `git rev-parse --abbrev-ref HEAD` + a short `git status --porcelain`
  summary (argv exec, timeout, bounded). Failures degrade gracefully (omit git, never error the turn).

**Step 1 — failing tests (table):** date present; git branch present (fake exec seam); git failure →
block without git, no error; clock seam controls the date. **Step 2-4. Commit:**
`feat(loop): RuntimeContextProvider interface + default (date/cwd/git, argv exec)`.

---

## Phase 2 — Wire into the turn (append-to-update, cache-safe)

### Task 2.1: loop appends runtime blocks at the turn tail

**Files:** Modify `internal/agent/loop/` turn-building (where `base`/turn `msgs` are assembled,
`loop.go` `installActiveTurn`/`buildTurnConfig`); inject an optional provider via `loop.Config`
(`RuntimeContext RuntimeContextProvider`, nil = off). Test: `internal/agent/loop/loop_test.go`.

**Step 1 — failing tests:**
- with a provider set, the turn sent to the model has the runtime block **appended at the tail**
  (after the user message), and the **system prompt (cached prefix) is byte-identical** across two
  turns even when the provider's output changes (append-to-update, never edit-prefix).
- nil provider → no change (current behavior).
- a second turn appends a **fresh** block (does not mutate the prior one).

**Step 2-4 (`-race`). Commit:** `feat(loop): append runtime context at turn tail (cache-safe)`.

### Task 2.2: wire the provider in `swarms/swe`

**Files:** Modify `swarms/swe/swarm.go` to pass the default `RuntimeContextProvider` into the
orchestrator (and leaf) `loop.Config`s. Test: `swarms/swe` smoke. **Commit:**
`feat(swarms/swe): enable runtime context injection`.

**P3 gate:** `go test -race ./...` + `make secure` green; a turn carries date/cwd/git at the tail;
the system prompt prefix is unchanged turn-to-turn (assert prompt-cache friendliness).

**Persistence note:** runtime blocks are part of the turn input → already journaled as part of
`msgs`; no new codec. They are volatile *data*, not config — **not** part of the fingerprint.
