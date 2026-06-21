# In-Session Subagents Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (or
> superpowers:subagent-driven-development) to implement this plan task-by-task.

**Goal:** Replace the child-session Subagent with subagents that run as in-session loops on
the shared hub, collapse the turn-driving surface to `Submit` + `SubscribeEvents`, and make
shutdown/interrupt multi-loop.

**Architecture:** Subagents become loops created via `Session.NewLoop` in the *current*
session; their events flow to the same hub (visible to the TUI), attributed by `LoopID`.
Provenance rides `identity.Agency` (no new command type); the loop tree rides a new Enduring
`LoopStarted` event via `Header.Cause`. A blocked synchronous Subagent tool drives its sub-loop
through a narrow injected capability (loop-targeted submit + interrupt) and collects the result
with a shared `drainToFinalText` helper. `Shutdown` and `Session.Interrupt` are distributed
across all loops.

**Tech Stack:** Go; `internal/agent/{session,loop,loop/event,loop/command,loop/identity}`;
`tools/`; `agents/coding/`. Design: `docs/plans/2026-06-19-in-session-subagents-design.md`.

**Conventions (every task):**
- TDD: write the failing test first, run it red, implement minimally, run it green, commit.
- Build: `CGO_ENABLED=0 go build -trimpath ./...`
- Test: `go test -race ./<pkg>/...` for the task; `go test -race ./...` at phase end.
- Before each commit: `make fmt` then `make secure` (gofmt + vet + staticcheck + gosec + vuln).
- Commit messages: Conventional Commits; end with the Co-Authored-By trailer.
- Each phase keeps the tree building and green (ordering is forced by the `Invoke`↔subagent
  coupling — see design §"Phasing").

---

## Phase 1 — Surface trim

> Delete the `personal-assistant` agent and `StreamBlocks`/`session.Stream`. `Invoke`, the
> `StartOnly` machinery, and per-turn channels **stay** — the subagent still uses `Invoke`
> until Phase 2. The coding agent already exposes `Submit`/`Subscribe`/`Interrupt`, so no
> main-path migration is needed beyond dropping `StreamBlocks`.

### Task 1: Delete the personal-assistant agent

**Files:**
- Delete: `agents/personal-assistant/` (entire package)
- Inspect/Modify: any importer — run `git grep -n "agents/personal-assistant"` and remove each
  reference (cmd entrypoints, tests, docs wiring).

**Step 1 — Find references.** Run: `git grep -n "personal-assistant\|personalassistant\|personal_assistant"` — record every hit outside the package itself.

**Step 2 — Delete the package.** Run: `git rm -r agents/personal-assistant`.

**Step 3 — Remove importers.** Edit each file from Step 1 to drop the import/usage. If a cmd
entrypoint instantiates the assistant, remove that path (the coding agent is the only shipped
agent).

**Step 4 — Build green.** Run: `CGO_ENABLED=0 go build -trimpath ./...` — Expected: builds (no
unresolved imports). Then `go test -race ./... 2>&1 | tail -20`.

**Step 5 — Commit.** `make fmt && make secure`, then:
```bash
git add -A
git commit -m "refactor(agents): delete personal-assistant agent (single coding agent)"
```

### Task 2: Remove `StreamBlocks` from the `tui.Agent` interface and the coding agent

**Files:**
- Modify: `tui/agent.go` (the `Agent` interface — drop the `StreamBlocks` method, line ~24)
- Modify: `agents/coding/agent.go:186-191` (delete the `Coding.StreamBlocks` method)
- Modify: `tui/screen_test.go` (the `fakeAgent.StreamBlocks` stub) + any other `tui` fakes
- Modify: `agents/coding/eval_integration_test.go:31` (migrate the eval harness off
  `StreamBlocks` → `Submit` + a `Subscribe` drain, or delete if redundant)

**Step 1 — Failing check (compile-driven).** Remove `StreamBlocks` from the `tui.Agent`
interface in `tui/agent.go`. Run: `go build ./tui/...` — Expected: FAIL where fakes still
declare it / nothing requires it. (This is the red.)

**Step 2 — Delete the method + fakes.** Delete `Coding.StreamBlocks`; delete the
`fakeAgent.StreamBlocks` stub(s) in `tui/screen_test.go`.

**Step 3 — Migrate the eval harness.** In `agents/coding/eval_integration_test.go`, replace the
`StreamBlocks` call with `Submit` + a `Subscribe`-based drain (see `drainToFinalText`, Task 11
— if Phase 2 not yet landed, drain inline against the primary loop). Keep the integration tag.

**Step 4 — Green.** Run: `go build -trimpath ./...` then `go test -race ./tui/... ./agents/...`
— Expected: PASS.

**Step 5 — Commit.** `make fmt && make secure`, then:
```bash
git add -A
git commit -m "refactor(tui,coding): drop StreamBlocks; TUI/eval use Submit + Subscribe"
```

### Task 3: Delete `session.Stream` (now unused)

**Files:**
- Modify: `internal/agent/session/session.go` (delete the `Stream` method, ~line 458)
- Modify: `internal/agent/session/agent_test.go` (delete `Stream` table rows / cases)

**Step 1 — Confirm no callers.** Run: `git grep -n "\.Stream(" -- '*.go' ':!vendor/*' ':!internal/llm/*'` — Expected: only `llm` client `.Stream` (unrelated) remains. If a session caller remains, fix it first.

**Step 2 — Delete + red.** Delete `Session.Stream`. Run: `go build ./internal/agent/session/...` — Expected: FAIL in `agent_test.go` referencing `Stream`.

**Step 3 — Remove its tests.** Delete the `Stream` cases in `agent_test.go` (leave `Invoke`
cases — `Invoke` stays until Phase 3).

**Step 4 — Green.** Run: `go test -race ./internal/agent/session/...` — Expected: PASS.

**Step 5 — Commit.** `make fmt && make secure`, then:
```bash
git add -A
git commit -m "refactor(session): delete unused Stream method"
```

### Phase 1 gate
Run: `go test -race ./...` and `make secure` — both green. `Invoke`/`StartOnly`/per-turn
channels intentionally still present.

---

## Phase 2 — Subagent rewrite

> The substantive phase. Add `LoopStarted`, the `closing` flag, multi-loop shutdown,
> distributed interrupt, the loop-targeted submit/interrupt capability, `drainToFinalText`,
> parent-`Provenance` plumbing, then rewrite the Subagent tool + its coding adapter onto
> in-session loops. This removes the last `session.Invoke` caller.

### Task 4: Add the `LoopStarted` event

**Files:**
- Modify: `internal/agent/loop/event/event.go` (new type + `isEvent`)
- Modify: `internal/agent/loop/event/validate.go` (fill rule)
- Test: `internal/agent/loop/event/validate_test.go` (or `event_test.go`)

**Step 1 — Failing test.** Add to the event validation table a `LoopStarted` case: requires
`SessionID`, `LoopID`, `EventID`; `Cause.Coordinates` carries the parent (may be zero for the
primary); `TurnID`/`StepID` zero on the producer coordinates.
```go
{name: "LoopStarted requires LoopID+EventID", ev: event.LoopStarted{
    Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
        EventID: eid, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: plid, TurnID: ptid, StepID: psid}, Agency: identity.AgencyMachine}}},
    wantErr: false},
{name: "LoopStarted missing LoopID errors", ev: event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: eid}}, wantErr: true},
```

**Step 2 — Run red.** `go test -race ./internal/agent/loop/event/ -run Validate` — Expected: FAIL (`LoopStarted` undefined).

**Step 3 — Implement.** In `event.go` (mirror `LoopIdle`):
```go
// LoopStarted is published by Session.NewLoop when a loop is registered. Header.Coordinates
// is the NEW loop (SessionID+LoopID set; TurnID/StepID zero). Header.Cause.Coordinates is the
// spawning loop/turn/step (zero for the primary = root); Cause.Agency = AgencyMachine.
// It is the durable loop-tree record for subscribers active at creation time.
type LoopStarted struct {
    enduring
    loopScoped
    Header
}

func (LoopStarted) isEvent() {}
```
Add the validation fill rule in `validate.go` alongside `LoopIdle` (require SessionID, LoopID,
EventID; TurnID/StepID/ToolUseID/GateID zero on the producer header).

**Step 4 — Green.** `go test -race ./internal/agent/loop/event/` — Expected: PASS.

**Step 5 — Commit.**
```bash
git add -A && git commit -m "feat(event): add Enduring LoopStarted carrying the loop tree via Cause"
```

### Task 5: Publish `LoopStarted` from `Session.NewLoop`

**Files:**
- Modify: `internal/agent/session/session.go` (`NewLoop`, ~line 263)
- Test: `internal/agent/session/agent_hub_test.go` (or `agency_test.go`)

**Step 1 — Failing test.** Subscribe `{Enduring: {All:true}}` BEFORE creating a sub-loop, then
`NewLoop(parent, cfg)`, and assert exactly one `LoopStarted` arrives with
`Header.LoopID == subLoopID` and `Header.Cause.Coordinates.LoopID == parent.LoopID`. Also
assert a subscriber created AFTER `NewLoop` does NOT receive it (no replay).

**Step 2 — Run red.** `go test -race ./internal/agent/session/ -run LoopStarted` — FAIL.

**Step 3 — Implement.** In `NewLoop`, after the loop is registered under `loopsMu` and before
returning, mint an `EventID` and `PublishEvent` a `LoopStarted` with the new loop's
coordinates and `Cause.Coordinates` from `parent loop.Provenance`, `Cause.Agency =
AgencyMachine`. Publish AFTER releasing `loopsMu` (do not publish under the lock). Handle the
id-gen error on the session's typed error path.

**Step 4 — Green.** `go test -race ./internal/agent/session/ -run LoopStarted` — PASS.

**Step 5 — Commit.**
```bash
git add -A && git commit -m "feat(session): publish LoopStarted on NewLoop"
```

### Task 6: `closing` flag + fail-secure `NewLoop`

**Files:**
- Modify: `internal/agent/session/session.go` (`Session` struct field `closing bool` guarded by
  `loopsMu`; `NewLoop` checks it; a typed `SessionClosing` error kind in the `SessionError` set)
- Test: `internal/agent/session/agent_test.go`

**Step 1 — Failing test.** After a helper sets `closing` (via `Shutdown`, or a test seam),
`NewLoop` returns a `*SessionError{Kind: SessionClosing}` and registers nothing
(`len(s.loops)` unchanged).

**Step 2 — Run red.** `go test -race ./internal/agent/session/ -run Closing` — FAIL.

**Step 3 — Implement.** Add `SessionClosing SessionErrorKind`. In `NewLoop`, take `loopsMu`
first; if `s.closing`, return the typed error before minting/constructing.

**Step 4 — Green.** PASS.

**Step 5 — Commit.**
```bash
git add -A && git commit -m "feat(session): fail-secure NewLoop once session is closing"
```

### Task 7: Multi-loop `Shutdown`

**Files:**
- Modify: `internal/agent/session/session.go` (`Shutdown`, ~line 643)
- Test: `internal/agent/session/agent_test.go`

**Step 1 — Failing test.** Build a session, `NewLoop` a second (idle) loop, subscribe, call
`Shutdown(ctx)`. Assert: every loop's `Done` closes, `WaitIdle` returns `ErrSessionStopped`,
and a `NewLoop` after `Shutdown` returns `SessionClosing`.

**Step 2 — Run red.** `go test -race ./internal/agent/session/ -run Shutdown` — FAIL (only
primary stops today).

**Step 3 — Implement.** Rewrite `Shutdown` per design §9:
1. under `loopsMu`: set `s.closing = true` and snapshot `[]*loopHandle`;
2. `s.hub.StopSession(ctx)`;
3. for each handle, send `command.Shutdown{Header:{CommandID}, Ack}` with `Done`/`ctx` escapes;
4. wait each `Ack` or `ctx.Done()`;
5. `s.sessionCancel()`.
Keep the existing typed-error wrapping.

**Step 4 — Green.** PASS, `-race` clean.

**Step 5 — Commit.**
```bash
git add -A && git commit -m "feat(session): multi-loop Shutdown (snapshot+closing, shutdown each, backstop)"
```

### Task 8: Distributed `Session.Interrupt`

**Files:**
- Modify: `internal/agent/session/session.go` (`Interrupt`, ~line 592)
- Test: `internal/agent/session/agency_test.go` + `agent_test.go`

**Step 1 — Failing test.** Two loops, both running a turn; `Interrupt(ctx)` cancels BOTH
(each subsequent terminal is `TurnInterrupted`), returns `true`; an idle loop in the set
no-ops (no panic, Ack false path); the command carries `Agency: AgencyUser`.

**Step 2 — Run red.** `go test -race ./internal/agent/session/ -run Interrupt` — FAIL.

**Step 3 — Implement.** Rewrite `Interrupt`: snapshot loops under `loopsMu`; for each, send
`command.Interrupt{Header:{CommandID, Agency: AgencyUser}, Ack}` (buffered(1)); collect acks
(bounded by `ctx`); return `true` if any ack was `true`. Reuse a small per-loop send helper.

**Step 4 — Green.** PASS, `-race` clean.

**Step 5 — Commit.**
```bash
git add -A && git commit -m "feat(session): distributed Interrupt across all loops (AgencyUser)"
```

### Task 9: Internal loop-targeted submit

**Files:**
- Modify: `internal/agent/session/session.go` (new unexported `submitToLoop(ctx, loopID, blocks, agency) (uuid.UUID, error)`)
- Test: `internal/agent/session/submit_test.go`

**Step 1 — Failing test.** `submitToLoop(ctx, subLoopID, blocks, AgencyMachine)` returns a
non-zero `CommandID`; a `{Enduring:{subLoopID}}` subscriber then sees a `TurnStarted` on
`subLoopID` whose `Cause.CommandID` equals the returned id and `Cause.Agency == AgencyMachine`.

**Step 2 — Run red.** `go test -race ./internal/agent/session/ -run SubmitToLoop` — FAIL.

**Step 3 — Implement.** Mirror `Submit` but resolve `loops[loopID]` (typed `SessionLoopNotFound`
if absent) and stamp `Header.Agency = agency`, `Mode: AllowFold`, nil per-turn channels.

**Step 4 — Green.** PASS.

**Step 5 — Commit.**
```bash
git add -A && git commit -m "feat(session): internal loop-targeted submit (Agency-stamped)"
```

### Task 10: Internal loop-targeted interrupt

**Files:**
- Modify: `internal/agent/session/session.go` (new unexported `interruptLoopID(ctx, loopID) (bool, error)` reusing the existing `interruptLoop` send, stamped `AgencyMachine`)
- Test: `internal/agent/session/agent_test.go`

**Step 1 — Failing test.** With a sub-loop running a turn, `interruptLoopID(ctx, subLoopID)`
cancels it (terminal `TurnInterrupted`); an unknown loop id returns `SessionLoopNotFound`.

**Step 2 — Run red.** FAIL.

**Step 3 — Implement.** `loops[loopID]` → send `command.Interrupt{Header:{CommandID, Agency:
AgencyMachine}, Ack}`; return the ack (best-effort) with `Done`/`ctx` escapes.

**Step 4 — Green.** PASS.

**Step 5 — Commit.**
```bash
git add -A && git commit -m "feat(session): internal loop-targeted interrupt (AgencyMachine)"
```

### Task 11: `drainToFinalText` helper

**Files:**
- Create: `internal/agent/session/drain.go` (or `tools/subagentdrain.go` — keep it where the
  `Subscription` type is importable without a cycle; prefer `internal/agent/session`)
- Test: same package `_test.go`

**Step 1 — Failing tests (table).** Cover: clean (`TurnDone.Message` → text), failed
(`TurnFailed.Err` → typed error), interrupted (on `ctx.Done()` the passed `interrupt` is called
**once** and the helper drains to `TurnInterrupted` → typed error), rejected
(`TurnRejected` → typed error), subscription-loss (`sub.Err()` set → typed error), and
two-phase correlation (opening `TurnStarted.Cause.CommandID == commandID` → capture `TurnID`;
later `StepDone`/terminal matched by `TurnID`). Drive with a fake `Subscription` feeding events.

**Step 2 — Run red.** `go test -race ./internal/agent/session/ -run DrainToFinalText` — FAIL.

**Step 3 — Implement.**
```go
func drainToFinalText(ctx context.Context, sub Subscription, commandID uuid.UUID, interrupt func()) (string, error)
```
Loop over `sub.Events()`: phase 1 — match `TurnStarted` with `Cause.CommandID == commandID`,
capture `TurnID`; phase 2 — track latest `StepDone` (same `TurnID`), stop on the terminal for
that `TurnID`. On `ctx.Done()` call `interrupt()` once (guard with a bool) and keep draining.
Map terminals per design §5 to concrete typed errors; if the channel closes or `sub.Err()` is
non-nil before a terminal, return a typed error.

**Step 4 — Green.** PASS, `-race` clean.

**Step 5 — Commit.**
```bash
git add -A && git commit -m "feat(session): drainToFinalText collect helper (Cause.CommandID correlation, interrupt fail-safe)"
```

### Task 12: Parent `Provenance` plumbing to tools

**Files:**
- Modify: `internal/agent/loop/loop.go` and/or `internal/agent/loop/runner.go` (inject the
  current `{LoopID,TurnID,StepID}` into the tool-execution ctx via an unexported context key
  in a small new `internal/agent/loop` accessor, e.g. `loop.WithProvenance`/`loop.ProvenanceFrom`)
- Test: `internal/agent/loop/runner_test.go`

**Step 1 — Failing test.** A fake tool reads `loop.ProvenanceFrom(ctx)` during `InvokableRun`
and the test asserts it equals the running loop/turn/step.

**Step 2 — Run red.** FAIL (accessor undefined).

**Step 3 — Implement.** Add `loop.WithProvenance(ctx, Provenance)` / `loop.ProvenanceFrom(ctx)
(Provenance, bool)` (unexported key type). In the runner, wrap the tool ctx with the current
provenance before calling `InvokableRun`. Absent key → zero provenance (fail-safe).

**Step 4 — Green.** PASS.

**Step 5 — Commit.**
```bash
git add -A && git commit -m "feat(loop): expose current Provenance to tools via ctx"
```

### Task 13: Rewrite the Subagent tool onto an in-session capability

**Files:**
- Modify: `tools/subagent.go` (replace `SubagentFactory`/`Subsession`/`maxSubagentDepth`/
  `subagentDepthKey` with a narrow `Spawner` capability interface)
- Test: `tools/subagent_test.go` (fake `Spawner`)

**Step 1 — Define the capability (interface first).**
```go
// Spawner is the in-session capability the Subagent tool needs (DIP). The coding
// composition root wires a concrete adapter over *session.Session.
type Spawner interface {
    Spawn(ctx context.Context, parent loop.Provenance, task []content.Block) (string, error)
}
```
(The adapter internally does `NewLoop` → `SubscribeEvents({Enduring:{id}})` → `submitToLoop`
→ `drainToFinalText` with the loop-targeted interrupt bound; the tool stays oblivious.)

**Step 2 — Failing test.** With a fake `Spawner` returning `"done"`, `InvokableRun` returns a
`TextResult("done")`; a fake returning an error yields a tool-result **error string** (never a
Go error); the tool reads parent provenance from ctx (`loop.ProvenanceFrom`) and passes it to
`Spawn`. Keep the existing arg-validation and `AuditSummary` tests (skill arg removed — args are
`{message}` only now; see design §"Out of scope: skills").

**Step 3 — Run red.** `go test -race ./tools/ -run Subagent` — FAIL.

**Step 4 — Implement.** Drop depth-cap code and the child-session interfaces. `InvokableRun`:
parse `{message}`, read `loop.ProvenanceFrom(ctx)`, call `spawner.Spawn(ctx, prov, blocks)`,
map result/error to a tool-result string. Update schema/desc to a single `message` field.

**Step 5 — Green.** PASS.

**Step 6 — Commit.**
```bash
git add -A && git commit -m "refactor(tools): Subagent spawns an in-session loop via Spawner (drop child session + depth cap)"
```

### Task 14: Wire the concrete `Spawner` in coding; delete the child-session factory

**Files:**
- Delete: `agents/coding/subagent_factory.go` (child-session factory) + its test
- Create: `agents/coding/spawner.go` (a `Spawner` over `*session.Session` doing
  `NewLoop`+`submitToLoop`+`Subscribe`+`drainToFinalText`+loop-targeted interrupt)
- Modify: `agents/coding/agent.go` (`buildToolSet` wires `tools.NewSubagent(spawner)`; the
  spawner needs the live `*session.Session`, so construct it after `session.New` and inject)

**Step 1 — Failing/integration test.** `agents/coding/*_test.go`: a Subagent call runs as an
in-session loop — assert via a `Subscribe({Enduring:{All}})` that a `LoopStarted` + the
sub-loop's `StepDone`/terminal appear attributed by `LoopID`, and the tool returns the child's
final text. (Use the fake llm client seam `newWithClient`.)

**Step 2 — Run red.** FAIL (no spawner; factory gone).

**Step 3 — Implement.** Build `codingSpawner{session *session.Session}` exposing `Spawn` per
§2. Since `buildToolSet` runs before `session.New`, restructure: build the toolset with a
late-bound spawner (a pointer the manifest sets after `session.New`), or add the Subagent tool
after session construction. Remove `session.New` child usage and `aiMessageText`/child adapter.

**Step 4 — Green.** `go test -race ./agents/coding/...` — PASS.

**Step 5 — Commit.**
```bash
git add -A && git commit -m "feat(coding): Subagent runs as in-session loop (delete child-session factory)"
```

### Phase 2 gate
Run: `go test -race ./...` and `make secure` — green. `session.Invoke` now has **no production
caller** (verify: `git grep -n "\.Invoke(" -- '*.go' ':!vendor/*' ':!internal/llm/*'` shows
only tests).

---

## Phase 3 — Machinery removal

> Nothing uses `Invoke` or the per-turn machinery now; delete it.

### Task 15: Delete `session.Invoke` + its tests

**Files:** `internal/agent/session/session.go` (`Invoke`, ~line 361), `agent_test.go`,
`quiescence_test.go`, `subagent_result_test.go` (migrate any `Invoke` test driver to
`Submit` + a `drainToFinalText`/subscribe helper).

**Steps:** confirm no non-test callers → delete `Invoke` (red in tests) → migrate/remove the
`Invoke` test cases → `go test -race ./internal/agent/session/...` green → `make secure` →
commit `refactor(session): delete Invoke (no callers after in-session subagents)`.

### Task 16: Remove `StartOnly` / per-turn channels / dual-delivery

**Files:** `internal/agent/loop/command/submit.go` (drop `StartOnly`; `InputMode` collapses to
a single mode — remove the type and the `Mode` field, or keep a no-op if churn is large),
`UserInput.Events`/`.Abandoned`; `internal/agent/loop/loop.go` (`emit`/`emitTurn` lose the
per-turn channel send → publish-only; `decideSubmit` loses the busy-reject branch and the
`events`/`abandoned` params); `internal/agent/loop/turn.go`; plus all touched `_test.go`.

**Steps (incremental, keep green between sub-steps):**
1. Remove the per-turn channel send from `emit`/`emitTurn`; run loop tests.
2. Remove `Events`/`Abandoned` from `UserInput` + `decideSubmit` signature; fix callers/tests.
3. Remove `StartOnly` + collapse `InputMode`; fix the `RejectBusy` branch (now unreachable).
4. `go test -race ./...`; `make secure`.
5. Commit `refactor(loop): drop StartOnly + per-turn channels + emit dual-delivery`.

### Phase 3 gate
Run: `go test -race ./...`, `make secure`, and `CGO_ENABLED=0 go build -trimpath ./...` — all
green. The external surface is now exactly `Submit` + `SubscribeEvents` (+ `WaitIdle`,
`Interrupt`, `Shutdown`, gate trio).

---

## Cross-cutting acceptance (design §"Testing")
- Subagent integration: sub-loop events appear on the parent subscription by `LoopID`;
  token/tool muted under the default filter; final text (or typed error) returned.
- Audit: machine turn (`Cause.Agency == AgencyMachine`) distinguishable from human; no subagent
  turn commits a human user row in the TUI.
- Distributed interrupt + multi-loop shutdown stop every active loop; `NewLoop` fails secure
  once closing; `-race` clean throughout.
- **Known deferred (TODO, not in this plan):** per-session loop cap + depth-from-`Provenance`
  (design §8 — acknowledged resource-bound regression).
