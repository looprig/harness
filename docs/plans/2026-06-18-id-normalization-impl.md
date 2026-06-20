# ID Normalization Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Apply the normalized ID vocabulary from
`docs/plans/2026-06-18-id-normalization-design.md` to the loop/command/event/
session/TUI code — one internal `ToolExecutionID`, a shared `identity` package
(`Coordinates`/`Cause`/`Agency`), `GateRoute` as the gate routing key, `Agency`
audit, and the `Session`/`New` rename — with the build and `go test -race ./...`
green after every task.

**Architecture:** A new low-level `internal/agent/loop/identity` package holds the
shared correlation types (no import cycle — it imports only `uuid`). The header
structs keep the name `Header` per package (the `CommandHeader()`/`EventHeader()`
accessor methods are unchanged). `content` is never touched.

**Tech Stack:** Go 1.26, stdlib only (`crypto/rand` via `internal/uuid`). No new
dependencies. Tests: table-driven, `-race`, `make secure` before merge.

**Design reference:** `docs/plans/2026-06-18-id-normalization-design.md` — read it
first. The *Rename Map*, *Fill Rules*, and *Validation Invariants* are the source
of truth for every field.

---

## Collaboration model (read this first)

This plan is executed **together**, on a real feature branch in the main checkout
(`/Users/ipotter/code/urvi`) — **not** a worktree — so Krishna can make the
symbol renames in the IDE while Claude does the structural work.

- **🧑 IDE (you):** pure, scope-aware **symbol renames**. The IDE's rename gets
  every call site and won't touch unrelated symbols (notably it leaves
  `openaiapi.ToolCallID string` alone). After each, Claude verifies build + tests.
- **🤖 Claude (me):** new types, struct reshaping (add/remove/embed fields), new
  logic (GateRoute dispatch, Agency stamping, binding), and all new tests.

Each task says who drives. **Hard rule: every task ends with `go test -race ./...`
green and a commit.** A rename and its dependent reshape are split into adjacent
tasks so the tree compiles between them.

**Verification commands (used throughout):**
- Build: `CGO_ENABLED=0 go build -trimpath ./...`
- Tests: `go test -race ./...`
- Pre-merge: `make secure`

---

## Task 0: Branch setup (🤖 + 🧑, together)

**Goal:** a clean `feature/id-normalization` branch in the main checkout, carrying
the design + this plan.

**Steps:**
1. Confirm the main checkout is clean (Krishna commits/stashes any in-flight TUI
   work first): `git -C /Users/ipotter/code/urvi status --porcelain` → empty.
2. Land the docs on `main` (this branch is `main` + the two doc commits):
   `git branch -f main docs/identity-correlation` (fast-forward; no main worktree
   has `main` checked out, so this is safe), OR merge `docs/identity-correlation`.
3. In the main checkout, branch and switch:
   `git -C /Users/ipotter/code/urvi switch -c feature/id-normalization main`
4. Baseline: `go build ./...` and `go test -race ./...` → **all green** (record the
   pass; this is the clean baseline every later task must preserve).

**Commit:** none (branch creation only). If baseline tests fail, **stop** and
report — do not start on a red tree.

---

## Phase 1 — Independent groundwork (🤖, additive, TDD)

These add new code without touching existing call sites, so the tree stays green.

### Task 1: `uuid.MarshalText` / `UnmarshalText`

**Files:**
- Modify: `internal/uuid/uuid.go`
- Test: `internal/uuid/uuid_test.go`

**Why:** the journal needs readable canonical-string ids, and `omitzero` is only
useful with a real text encoding (today `uuid.UUID` would marshal as a 16-int
array).

**Step 1 — failing test** (table-driven: round-trip a known uuid, the zero uuid, and reject a malformed text):
```go
func TestUUIDTextRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      UUID
		wantErr bool
	}{
		{name: "zero", in: UUID{}},
		{name: "v4", in: MustParse("…canonical uuid…")}, // use an existing fixture/helper
	}
	for _, tt := range tests { /* MarshalText → UnmarshalText → DeepEqual */ }
}
```
**Step 2 — run, expect FAIL** (`MarshalText` undefined): `go test -race ./internal/uuid/ -run TestUUIDText`
**Step 3 — implement** `MarshalText`/`UnmarshalText` on `UUID` (reuse `String()` and the existing parse path).
**Step 4 — run, expect PASS.**
**Step 5 — commit:** `feat(uuid): add MarshalText/UnmarshalText for canonical-string encoding`

### Task 2: `internal/agent/loop/identity` package

**Files:**
- Create: `internal/agent/loop/identity/identifier_types.go`
- Test: `internal/agent/loop/identity/identifier_types_test.go`

**Contents** (per design *Coordinates and Cause* + *Agency*): `Coordinates`,
`Agency` (`AgencyMachine` = iota default, `AgencyUser`) with a `String()` for
logs, and `Cause` (embeds `Coordinates`; `CommandID`/`EventID`/`ToolExecutionID`/
`Agency`). All `uuid` fields tagged `json:",omitzero"`.

**Step 1 — failing tests** (table-driven):
- `Agency` zero value is `AgencyMachine`; `String()` for each.
- JSON round-trip: a zero `Cause` marshals to `{}` (all `omitzero`); a `Cause`
  with only `CommandID`+`Agency:AgencyUser` omits the rest and round-trips.
**Step 2 — run, expect FAIL** (package missing).
**Step 3 — implement** the three types.
**Step 4 — run, expect PASS.**
**Step 5 — commit:** `feat(identity): add Coordinates, Cause, Agency correlation types`

---

## Phase 2 — Session rename (🧑 IDE)

### Task 3: `Sesssion` → `Session`, `NewAgent` → `New`

**Files:** `internal/agent/session/*.go` (+ any callers, e.g. `cmd/…`, tests).

**🧑 Step 1 — IDE renames:**
- type `Sesssion` → `Session`
- func `NewAgent` → `New` (call sites become `session.New(...)`)
- fix stale "AgentSession" **comments** (text search; `loop.go:112` and others).

**🤖 Step 2 — verify:** `go build ./...` && `go test -race ./...` → green.
**🤖 Step 3 — commit:** `refactor(session): rename Sesssion→Session, NewAgent→New`

---

## Phase 3 — Command header (🧑 rename → 🤖 reshape)

### Task 4 (🧑 IDE): command `Header.ID` → `CommandID`

**Files:** `internal/agent/loop/command/*.go` + all command construction sites
(`internal/agent/session/session.go` `command.Header{ID: …}`, tests).

**🧑** IDE-rename the field `ID` → `CommandID` on `command.Header` (struct stays
`Header`). **🤖 verify** build+tests green. **Commit:**
`refactor(command): rename Header.ID → CommandID`

### Task 5 (🤖): reshape `command.Header` — `Cause` + `Agency`

**Files:**
- Modify: `internal/agent/loop/command/header.go`
- Modify: construction sites in `internal/agent/session/session.go`
- Test: `internal/agent/loop/command/header_test.go`

**Change** `command.Header` to:
```go
type Header struct {
	CommandID uuid.UUID       `json:"command_id,omitzero"`
	Cause     identity.Cause  `json:"cause,omitzero"`
	Agency    identity.Agency `json:"agency,omitzero"`
}
```
- Replace the old `CausationID` with `Cause` (callers that set `CausationID` now
  set `Cause.CommandID`/`Cause.EventID` per design *Setting Cause*; most set none).
- `CommandHeader()` accessor unchanged.

**TDD:** extend `header_test.go` — `CommandHeader()` returns the header;
zero-`Cause`/`AgencyMachine` JSON omits cleanly.
**🤖 verify + commit:** `feat(command): Header carries Cause + Agency`

---

## Phase 4 — Event header (🧑 rename → 🤖 reshape)

### Task 6 (🧑 IDE): event `Header.ID` → `EventID`

**Files:** `internal/agent/loop/event/*.go` + all event construction sites
(`loop.go`, `turn.go`, `step.go`, `session.go`, tests).

**🧑** rename field `ID` → `EventID` on `event.Header`. **🤖 verify + commit:**
`refactor(event): rename Header.ID → EventID`

### Task 7 (🤖): reshape `event.Header` — embed `Coordinates`, add `Cause`, drop dead fields

**Files:**
- Modify: `internal/agent/loop/event/event.go`
- Modify: every event construction site (`loop.go`, `turn.go`, `step.go`, `session.go`)
- Test: `internal/agent/loop/event/header_test.go`

**Change** `event.Header` to:
```go
type Header struct {
	identity.Coordinates              // SessionID, LoopID, TurnID, StepID (was flat)
	EventID uuid.UUID      `json:"event_id,omitzero"`
	Cause   identity.Cause `json:"cause,omitzero"`
}
```
- Embed `Coordinates` (the flat `SessionID/LoopID/TurnID/StepID` become the
  embed — promoted reads like `h.LoopID` keep working).
- Replace `CausationID` → `Cause.CommandID`; replace `TriggeredByLoopID` →
  `Cause.LoopID` (per *Rename Map*).
- **Remove** the dead `ParentLoopID/TurnID/StepID` and `ToolCallID` fields.
- Update construction sites: `Header{SessionID: …}` → `Header{Coordinates: identity.Coordinates{SessionID: …}}`.

**Watch:** `EventHeader()` accessor unchanged; quiescence code that read
`TriggeredByLoopID` now reads `Cause.LoopID` (grep `TriggeredByLoopID` →
`session/hub`, `loop.go`, `turn.go`).
**TDD:** header_test — embedded `Coordinates` promotes; `EventHeader().LoopID`
works; zero `Cause` omitted.
**🤖 verify + commit:** `feat(event): Header embeds Coordinates + Cause; drop dead Parent*/ToolCallID`

### Task 8 (🤖): submit events — drop `InputID`, use `Cause.CommandID`

**Files:** `internal/agent/loop/event/turn.go` (`TurnStarted`, `TurnFoldedInto`,
`InputCancelled`, `InputQueued`, `TurnRejected`), construction sites in `loop.go`/`turn.go`.

- Remove the `InputID` field from those events; producers set `Cause.CommandID`
  to the submit command id (and `Cause.Agency` from the submit command).
- Consumers that read `ev.InputID` now read `ev.Cause.CommandID`.

**TDD:** a `TurnStarted` built from a `UserInput` carries `Cause.CommandID ==`
the submit's `CommandID`.
**🤖 verify + commit:** `refactor(event): submit events use Cause.CommandID, drop InputID`

---

## Phase 5 — ToolExecutionID (🧑 rename → 🤖 bind)

### Task 9 (🧑 IDE): `CallID`/loop-`ToolCallID` (uuid) → `ToolExecutionID`

**Files:** `internal/agent/loop/{runner,gate}.go`,
`internal/agent/loop/event/tool.go`, `internal/agent/loop/command/{approve,deny,provide_user_input,route}.go`,
`tui/*.go` — wherever the **uuid** `CallID`/`ToolCallID` lives.

**🧑** IDE-rename the `uuid.UUID` `CallID` → `ToolExecutionID` (runner `result`/
`resolved`, gate `pendingGates` key vars, the 4 events in `tool.go`, gate commands,
`GateCallID()` → `GateToolExecutionID()`, TUI). Also `Route.ToolCallID` (uuid) →
`ToolExecutionID`. **Do NOT touch `openaiapi.ToolCallID string`** (different
symbol — IDE skips it; don't hand-edit).
**🤖 verify + commit:** `refactor: rename CallID/ToolCallID(uuid) → ToolExecutionID`

### Task 10 (🤖): remove duplicate `event.Header.ToolCallID`; lock the 1:1 binding

**Files:** `internal/agent/loop/event/event.go` (already dropped in Task 7 —
confirm), `internal/agent/loop/runner.go`, test `runner_test.go`.

- Confirm the tool id now lives **only** on the 4 event bodies as
  `ToolExecutionID` (the header field was removed in Task 7).
- `runner.RunBatch` already builds `result{ToolExecutionID: r.toolExecutionID, ToolUseID: r.block.ID}` — add the binding test.

**TDD:** each minted `ToolExecutionID` maps to exactly one `ToolUseBlock.ID`; a
`ToolResultMessage` built from a `ToolExecutionID` carries that provider `ToolUseID`.
**🤖 verify + commit:** `test(runner): lock ToolExecutionID ↔ provider ToolUseID 1:1 binding`

---

## Phase 6 — GateRoute (🧑 rename → 🤖 reshape + dispatch)

### Task 11 (🧑 IDE): `Route` → `GateRoute`

**Files:** `internal/agent/loop/command/route.go` + embedders.
**🧑** rename type `Route` → `GateRoute`. **🤖 verify + commit:**
`refactor(command): rename Route → GateRoute`

### Task 12 (🤖): reshape `GateRoute` + gate commands + `CancelQueuedInput`

**Files:** `command/route.go`, `command/{approve,deny,provide_user_input}.go`,
`command/cancel_queued_input.go`, construction sites.

- `GateRoute` = embed `identity.Coordinates` + `ToolExecutionID` (drop the old flat
  fields).
- Gate commands embed `GateRoute` (collapse the old separate `GateRoute` +
  `ToolExecutionID`/`CallID` field into the one embed); `GateToolExecutionID()`
  reads `GateRoute.ToolExecutionID`.
- `CancelQueuedInput`: embed plain `identity.Coordinates` (not `GateRoute`); rename
  `InputID` → `TargetCommandID`.

**TDD:** gate command exposes its `ToolExecutionID`; `CancelQueuedInput` carries
`TargetCommandID`.
**🤖 verify + commit:** `feat(command): GateRoute = Coordinates+ToolExecutionID; CancelQueuedInput uses Coordinates+TargetCommandID`

### Task 13 (🤖): make `GateRoute` the routing key (session dispatch + TUI threading)

**Files:** `internal/agent/session/session.go` (`Approve`/`Deny`/`ProvideUserInput`),
`internal/agent/loop/{loop,gate}.go`, `tui/{action,screen,interaction}.go`.

- Session gate API takes a `GateRoute` (or `LoopID`+`ToolExecutionID`) instead of a
  bare `callID`; dispatch to `loops[GateRoute.LoopID]` (the registry is already
  keyed by `LoopID`).
- Loop matches the gate by `GateRoute.ToolExecutionID` (`pendingGates`) as today.
- TUI: thread the pending prompt's `LoopID` (`m.pending[].LoopID`, set by
  `enqueueForLoop`) through `uiAction` into the gate command so it can build the
  `GateRoute`.

**TDD (the key new test):** a reply addressed to loop A is delivered to loop A's
gate and **never** reaches loop B; matching is by `ToolExecutionID`, never the
provider `ToolUseID`.
**🤖 verify + commit:** `feat(session): route gate replies by GateRoute.LoopID`

---

## Phase 7 — Agency stamping (🤖, TDD)

### Task 14 (🤖): stamp `AgencyUser` at user-origination points

**Files:** `tui/*.go` (typed `UserInput`; approve/deny/answer), the manual
`Interrupt` path, `internal/agent/session/session.go` (command minting),
construction of submit-resolution events.

- Default is `AgencyMachine` (zero) — nothing to do for machine paths.
- Set `Agency = AgencyUser` on: the TUI's typed `UserInput`; human
  `ApproveToolCall`/`DenyToolCall`/`ProvideUserInput`; a manual `Interrupt`.
- `TurnStarted`/`TurnFoldedInto`/`InputCancelled` copy `Cause.Agency` from the
  submit command.

**TDD:** machine paths default `AgencyMachine`; the user paths produce `AgencyUser`;
`TurnStarted.Cause.Agency` matches its submit command.
**🤖 verify + commit:** `feat: stamp Agency (user at origination, machine default)`

---

## Phase 8 — Validation + close-out (🤖, TDD)

### Task 15 (🤖): fill-matrix validation helpers + tests

**Files:** Create `internal/agent/loop/event/validate.go` (+ command side),
tests `validate_test.go`.

- Table-driven validation per the design *Fill Rules* / *Validation Invariants*:
  `EventID` non-zero; scope-zero rules per event; `StepID` requires `TurnID`;
  `ToolExecutionID` requires the full coordinate chain; gate replies carry a
  `GateRoute` with non-zero `LoopID`+`ToolExecutionID`.
**TDD:** one table row per event/command type (valid + each forbidden-field case).
**🤖 verify + commit:** `feat(event): validation helpers for the ID fill matrix`

### Task 16 (🤖): full sweep + pre-merge gate

- `go test -race ./...` (incl. `-tags integration` where relevant).
- `make secure` (vet + staticcheck + gosec + govulncheck).
- Update `CLAUDE.md`'s canonical struct hierarchy if it names any renamed field.
**Commit:** `chore: id-normalization sweep — make secure green`

---

## Notes / gotchas (carried from the design + code review)

- **`Header` struct names stay `Header`** (qualified `command.Header`/`event.Header`)
  — renaming to `CommandHeader`/`EventHeader` shadows the accessor methods and
  breaks the `Event` interface. (Verified: `command.go:7`, `event.go:19`/`102`.)
- **`content` is never touched.** The provider `ToolUseID` (`string`) stays in
  `ToolUseBlock.ID` / `ToolResultMessage.ToolUseID`.
- **`openaiapi.ToolCallID string`** is the provider wire field — leave it.
- **`Origin`/provenance is deferred** — do NOT add it; just remove the dead
  `Parent*` fields. `loop.Provenance` on `loopState.parent` stays untouched.
- **No `MessageID`** — do not add one; `content` is unchanged.
- Each rename task that the IDE drives should still leave **test files** renamed by
  the IDE; if a test references an old name, that's a missed rename — fix before
  committing.
