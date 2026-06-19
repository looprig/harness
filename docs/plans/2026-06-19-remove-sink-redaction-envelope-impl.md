# Remove Event Sink, Redaction & EventEnvelope Scaffolding — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Delete the observability sink path (`EventSink`, `EventEnvelope`, `Redactable`/`SinkProjection` redaction) and the loop's duplicate sink-only `SessionStarted`, with no observable change to the two live delivery paths (hub fan-in + per-turn stream).

**Architecture:** The sink path has zero production implementers — its only consumers are test fakes (`captureSink`/`recordingSink`) that observe loop events. So the order is **migrate-then-delete**: first re-point those tests at a recording `eventPublisher` (the same narrow interface production uses for the hub fan-in), then delete the sink/redaction/envelope code once nothing references it. Each commit leaves `go build` and `go test -race` green.

**Tech Stack:** Go (stdlib only here), `go test -race`, `make secure` (vet + staticcheck + gosec + govulncheck).

**Spec:** `docs/plans/2026-06-19-remove-sink-redaction-envelope.md` — read it first; this plan executes it.

---

## Conventions for every task

- Build: `CGO_ENABLED=0 go build -trimpath ./...`
- Test (package): `go test -race ./internal/agent/loop/... ./internal/agent/session/...`
- Each task ends green and is committed. Commit message trailer:
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`
- This is a deletion/migration: the existing suite is the safety net. Do **not** change *what* a test asserts except the three intentional drops: (1) `SessionStarted` observation, (2) redacted-shape assertions, (3) `EventID` assertions. See the field map.

### Field map (apply throughout the test migration)

| Was (`EventEnvelope`) | Now (`event.Event`) |
|---|---|
| `env.Event` | the `event.Event` value itself |
| `env.TurnID` / `env.CausationID` / `env.CallID` | `ev.EventHeader().TurnID` / `.CausationID` / `.ToolCallID` |
| `env.TurnIndex` | the event's own `TurnIndex` field (type-assert the concrete event) |
| `env.EventID` | **no equivalent** — `Header.ID` is unwired; delete the assertion |
| a captured `SessionStarted` | **gone** — delete the assertion (loop no longer emits it) |
| a captured *redacted* shape (`UserInputRequestedSink`, `UnknownRequest`, empty `ResultPreview`, dropped `InputJSON`) | the **full** value (`UserInputRequested`, real `Request`, full preview/chunk) — the recorder is full-fidelity |

---

## Pre-flight

### Task 0: Baseline green

**Step 1:** Run the suite to confirm a clean starting point.

Run: `CGO_ENABLED=0 go build -trimpath ./... && go test -race ./internal/agent/...`
Expected: PASS.

**Step 2:** (Optional but recommended) work in a worktree — see superpowers:using-git-worktrees. Otherwise branch off `main`:

```bash
git checkout -b remove-sink-scaffolding
```

---

## Phase 1 — Recording test infrastructure (additive, green)

### Task 1: Add a recording `eventPublisher` and event-based helpers

**Files:**
- Modify: `internal/agent/loop/loop_test.go` (add helpers near `noopPublisher`, ~line 50)

**Step 1: Add the recorder and a constructor variant.** Append to `loop_test.go`:

```go
// recordingPublisher is an eventPublisher that records every full-fidelity event
// the loop publishes to the session fan-in. It replaces the old captureSink for
// loop tests: the loop already depends on eventPublisher (loop.go), so this
// observes exactly what production subscribers see — no envelope, no redaction.
type recordingPublisher struct {
	mu  sync.Mutex
	got []event.Event
}

func (r *recordingPublisher) PublishEvent(_ context.Context, ev event.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, ev)
	return nil
}

func (r *recordingPublisher) events() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.got...)
}

// blockUntilEvents waits until pred sees the recorded events, or fails.
func blockUntilEvents(t *testing.T, rec *recordingPublisher, pred func([]event.Event) bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if pred(rec.events()) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("event condition not met within deadline")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// newLoopRec is newLoop wired to a recordingPublisher, returned for assertions.
func newLoopRec(t *testing.T, client llm.LLM) (*Loop, *recordingPublisher, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recordingPublisher{}
	l, err := New(ctx, mustID(t), mustID(t), Provenance{}, rec,
		Config{Client: client, Model: llm.ModelSpec{Model: "m"}, DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cancel)
	return l, rec, cancel
}
```

**Step 2: Write a sanity test** (proves the recorder observes loop events) in `loop_test.go`:

```go
func TestRecordingPublisherObservesTurnStarted(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoopRec(t, scriptedClient(t, "hello")) // use the package's existing fake client helper
	_, _ = startTurn(t, l, context.Background(), []content.Block{content.TextBlock{Text: "hi"}})
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, ev := range evs {
			if _, ok := ev.(event.TurnStarted); ok {
				return true
			}
		}
		return false
	})
}
```

> Replace `scriptedClient(t, "hello")` with whatever fake `llm.LLM` constructor the existing tests use (grep `loop_test.go` for how `newLoop` callers build a client).

**Step 3: Run.** `go test -race ./internal/agent/loop/ -run TestRecordingPublisher -v` → PASS. Full package still PASS.

**Step 4: Commit.** `test(loop): add recordingPublisher event-observation harness`

---

## Phase 2 — Migrate observation off the sink (one file per task, green each)

For every task below: replace sink construction + `blockUntilSink`/`[]event.EventEnvelope` with `recordingPublisher` + `blockUntilEvents`/`[]event.Event`, applying the field map. Drop `SessionStarted`/`EventID`/redacted-shape assertions. Keep the sink production code alive (deleted in Phase 4–6).

Two construction shapes you will encounter:
- **Via `newLoop(t, client, sink)`** → switch to `newLoopRec(t, client)` and read `rec.events()`.
- **Via inline `New(ctx, …, noopPublisher{}, Config{… Sinks: []event.EventSink{sink} …})`** → make a local `rec := &recordingPublisher{}`, pass `rec` instead of `noopPublisher{}`, delete the `Sinks:` field, read `rec.events()`.

### Task 2: Migrate `loop_test.go`

**Files:** Modify `internal/agent/loop/loop_test.go`.

- Convert the `captureSink` test table (`var turnEnvs []event.EventEnvelope`, the `assert func(t, envs []event.EventEnvelope)` cases ~309–369, `newLoopWithIDGen(..., sinks)` ~410) to `recordingPublisher` + `[]event.Event`. Add a `newLoopWithIDGenRec` mirroring `newLoopRec` if `newLoopWithIDGen` is used for observation.
- Convert `hasTerminal(evs []event.EventEnvelope)` → `hasTerminal(evs []event.Event)` using `ev.EndsTurn()`:

  ```go
  func hasTerminal(evs []event.Event) bool {
      for _, ev := range evs {
          if ev.EndsTurn() {
              return true
          }
      }
      return false
  }
  ```
- Leave `captureSink`/`panicSink`/`TestEventSinkPanicRecovered` in place for now (removed in Task 8).

**Verify:** `go test -race ./internal/agent/loop/ -run TestLoop -v` (or the affected test names) → PASS. **Commit:** `test(loop): observe via recordingPublisher in loop_test`

### Task 3: Migrate `submit_decision_test.go`

**Files:** Modify `internal/agent/loop/submit_decision_test.go`.

- Replace the inline `New(…, noopPublisher{}, Config{… Sinks: …})` (~389) with a `recordingPublisher`.
- Replace `blockUntilSink(t, sink, func(evs []event.EventEnvelope) bool {…})` (~415) with `blockUntilEvents(t, rec, func(evs []event.Event) bool {…})`, reading event fields directly.
- The shared `blockUntilSink` helper lives here — leave its definition for now (other not-yet-migrated files use it); it's removed in Task 8.

**Verify:** `go test -race ./internal/agent/loop/ -run TestSubmit -v` → PASS. **Commit:** `test(loop): observe via recordingPublisher in submit_decision_test`

### Task 4: Migrate `fold_test.go`

**Files:** Modify `internal/agent/loop/fold_test.go`.
- Two inline `New(…, Sinks: …)` sites (~32, ~59) → `recordingPublisher`.
- Six `blockUntilSink(... []event.EventEnvelope ...)` sites → `blockUntilEvents(... []event.Event ...)`.

**Verify:** `go test -race ./internal/agent/loop/ -run TestFold -v` → PASS. **Commit:** `test(loop): observe via recordingPublisher in fold_test`

### Task 5: Migrate `cancel_queued_test.go`

**Files:** Modify `internal/agent/loop/cancel_queued_test.go`.
- Convert `hasInputCancelled(evs []event.EventEnvelope, inputID, reason)` (~29) to `[]event.Event` — type-assert `event.InputCancelled` and read its fields / `EventHeader()`.
- Inline `New(…, Sinks: …)` (~217) → `recordingPublisher`; `blockUntilSink` sites → `blockUntilEvents`.

**Verify:** `go test -race ./internal/agent/loop/ -run TestCancel -v` → PASS. **Commit:** `test(loop): observe via recordingPublisher in cancel_queued_test`

### Task 6: Migrate `inbox_pop_idgen_test.go`

**Files:** Modify `internal/agent/loop/inbox_pop_idgen_test.go`.
- `Sinks:` wiring (~77) → `recordingPublisher`; `blockUntilSink` (~109) → `blockUntilEvents`.
- If it asserted a non-zero `env.EventID`, delete that assertion (no equivalent).

**Verify:** `go test -race ./internal/agent/loop/ -run TestInbox -v` → PASS. **Commit:** `test(loop): observe via recordingPublisher in inbox_pop_idgen_test`

### Task 7: Migrate `session_test.go` to the hub Subscription

**Files:** Modify `internal/agent/session/session_test.go`.
- Delete `recordingSink` (~56) and `cfgWithSink` (~99).
- Subscribe to the session's event fan-in instead (the same entry the TUI uses — `hub.SubscribeEvents` / the session's exported subscribe method; grep the session for its subscription API). Collect `sub.Events()` (`[]event.Event`) and assert with the field map.
- A `Subscription` created after `NewAgent` will not see `SessionStarted` (emitted at construction, pre-subscribe) — drop any such assertion (matches the spec's follow-on note).

**Verify:** `go test -race ./internal/agent/session/ -v` → PASS. **Commit:** `test(session): observe via hub Subscription instead of a sink`

---

## Phase 3 — Delete the sink/redaction tests (green: only removes test code)

### Task 8: Remove obsolete sink/redaction test code

**Files:**
- Delete: `internal/agent/loop/event/sink_test.go` (`TestEventEnvelopeFields`)
- Delete: `internal/agent/loop/sink_projection_test.go`
- Modify: `internal/agent/loop/event/tool_test.go` — delete `TestSinkProjectionDropsSecrets`, `TestSinkProjectionPreservesCallID`, the `TokenDelta`/`TurnDone` projection tests, `TestRedactableImplementations`, the no-mutation test, and every `UserInputRequestedSink`/`Redactable`/`SinkProjection` reference. Keep any non-redaction coverage.
- Modify: `internal/agent/loop/loop_test.go` — delete `captureSink`, `panicSink`, `TestEventSinkPanicRecovered`.
- Modify: `internal/agent/loop/submit_decision_test.go` — delete the now-unused `blockUntilSink`.

**Verify:** `go build -trimpath ./... && go test -race ./internal/agent/...` → PASS. Confirm no test references remain:
`grep -rn "EventEnvelope\|EventSink\|Redactable\|SinkProjection\|UserInputRequestedSink\|captureSink\|blockUntilSink\|\.Sinks\|Sinks:" --include="*_test.go" internal/agent` → only the production-facing names should be gone; expect **no hits**.

**Commit:** `test: drop sink/redaction/envelope tests (scaffolding being removed)`

---

## Phase 4 — Delete sink usage from loop/config/session (green)

### Task 9: Collapse the loop publish path and remove the loop `SessionStarted`

**Files:**
- Modify: `internal/agent/loop/config.go` — delete the `Sinks []event.EventSink` field (13).
- Modify: `internal/agent/loop/loop.go`:
  - Delete `projectForSink` (175–200).
  - In `publish` (398–448): remove the `projectForSink` call, the `event.EventEnvelope` build, the `config.idGen()` EventID mint (408–415) and the `TurnIndex` switch (424–437), and the `for _, sink := range config.Sinks` fanout (438–447). The body becomes just `publishHub(ev)` — keep the `publish` name as a one-line wrapper to avoid touching ~10 call sites. **Keep `idGen`** (still used at ~683 for `turnID` and at ~667 in the runner config).
  - Delete the loop's startup `publish(event.SessionStarted{…})` (450).
  - In `publishHub` (389–396): delete the `if _, ok := ev.(event.SessionStarted); ok { return }` skip-guard (390–392) — now dead.
  - Rewrite the "to sinks"/"redacted envelope"/"sink-only"/"already in sinks" prose in the comments on `publishHub`, `publish`, `emitTurn`, `deliverAndClose`, `emitLoopIdle`.
- Modify: `internal/agent/session/session.go` — rewrite the `NewAgent` comment (301–306): sinks are gone, so the session's `NewAgent` emission is the sole `SessionStarted` (reliable subscriber delivery is the follow-on).

**Verify:** `go build -trimpath ./... && go test -race ./internal/agent/...` → PASS. (`event.EventEnvelope`/`event.EventSink` still exist but are now referenced by nothing outside `event/sink.go`.)

**Commit:** `refactor(loop): collapse publish to hub-only; drop loop SessionStarted duplicate`

---

## Phase 5 — Delete redaction (green)

### Task 10: Remove `Redactable`, `SinkProjection`, `UserInputRequestedSink`

**Files:**
- Modify: `internal/agent/loop/event/tool.go` — delete the `Redactable` interface (8–21), `UserInputRequestedSink` (47–59) + its `isEvent()` (87), and the three `SinkProjection()` methods (96–106, 111–117, 122–128). Trim the SINK/redaction prose in the surviving events' doc comments. Keep the events + their `CallID`/payload fields (per-turn stream/TUI still use them).
- Modify: `internal/agent/loop/event/turn.go` — delete `SinkProjection()` on `TokenDelta` (190–205) and `TurnDone` (216–243); **remove the now-unused `encoding/json` import**. Trim the redaction/`TODO(Open Items B)` comments on `StepDone`, `TurnFailed`, and the `TurnDone.Message` note.
- Modify: `internal/agent/loop/event/event.go` — remove the `Event`-interface SECURITY note about `Redactable`/the sink path (15–18); update the `Header` comment that defers "EventEnvelope replacement" (97–99).
- Modify: `internal/agent/loop/event/doc.go` — delete `_ Event = UserInputRequestedSink{}` (41) and the five `_ Redactable = …` assertions + comment (46–54).

**Verify:** `go build -trimpath ./... && go test -race ./internal/agent/...` → PASS.

**Commit:** `refactor(event): remove Redactable/SinkProjection redaction`

---

## Phase 6 — Delete the sink transport (green)

### Task 11: Delete `event/sink.go`

**Files:** Delete `internal/agent/loop/event/sink.go` (`EventEnvelope`, `EventSink`).

**Verify:** `go build -trimpath ./... && go test -race ./internal/agent/...` → PASS. Final closure check (whole repo, excl. vendor/worktrees):
`grep -rn "EventEnvelope\|EventSink\|Redactable\|SinkProjection\|UserInputRequestedSink\|projectForSink" --include="*.go" . | grep -v vendor/ | grep -v "/.worktrees/"` → **no hits**.

**Commit:** `refactor(event): delete EventSink and EventEnvelope`

---

## Phase 7 — Final verification

### Task 12: Full build, race suite, security gate

**Step 1:** `CGO_ENABLED=0 go build -trimpath ./...` → success.
**Step 2:** `go test -race ./...` → PASS.
**Step 3:** `make secure` (lint = vet + staticcheck + gosec; vuln = go mod verify + govulncheck) → PASS.
**Step 4:** Sanity-check the diff is deletion-dominant and touches only the files in this plan: `git diff --stat main`.

**Commit (if anything adjusted):** `chore: verify sink/redaction/envelope removal`

---

## Notes / known follow-ons (NOT in this plan)

- **Reliable `SessionStarted` observability** (snapshot-on-subscribe in the hub) — separate feature; owned elsewhere.
- **Headless mode / Design B** — being reconciled separately by the maintainer.
- Re-homing redaction as a hub subscriber — owned by the loop-machine journal follow-on if a real journal lands.
- Line numbers above are indicative (captured pre-change); re-grep the symbol if a range has drifted.
