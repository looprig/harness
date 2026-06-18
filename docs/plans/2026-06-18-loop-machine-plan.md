# Loop Machine Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the loop's ad-hoc turn/streaming code with an explicit
`session -> loop -> turn -> step -> block -> chunk` state hierarchy, a session-level
publish/subscribe event fan-in with quiescence tracking, and a single typed submit
command — so the UI renders exactly what the loop stores and headless runs have a
well-defined idle/stop model.

**Architecture:** Incremental, bottom-up. Build the new event vocabulary and the
shared stream accumulator first (no behavior change), then introduce the state
types (`blockState`/`stepState`/`turnState`) and the loop-owned incremental commit,
then reframe input as one submit command with a typed `Disposition`, then rewire the
actor (`listen` → `runLoop`) and finally point the TUI/CLI at the new session fan-in.
Each phase keeps `go test -race ./...` green; the actor stays functional throughout.

**Tech Stack:** Go 1.26.4, module `github.com/inventivepotter/urvi`. Stdlib only
(no new external deps). Internal `uuid` package (`internal/uuid`, `UUID [16]byte`,
`New() (UUID, error)`). Bubble Tea v2 TUI (unchanged by this plan except event
consumption).

**Design spec:** `docs/plans/loop-machine-design.md` (read it before starting; each
phase below cites the spec section it implements). Open risks are tracked in that
doc's **Open Items & Follow-Ups** checklist — keep it in view.

---

## Working Agreement (read once, applies to every task)

**TDD is mandatory (REQUIRED SUB-SKILL: superpowers:test-driven-development).**
Every task is: write the failing test → run it, watch it fail for the *right*
reason → write the minimal implementation → run it, watch it pass → commit.

**Commands:**
- Run one package's tests: `go test -race ./internal/agent/loop/... -run TestName -v`
- Run everything: `make test` (`go test -race ./...`)
- Before every commit at a **phase boundary**: `make secure` (`go vet`,
  `staticcheck`, `gosec`, `go mod verify`, `govulncheck`) and `make build`
  (`CGO_ENABLED=0 go build -trimpath`).
- Per-task commits during a phase only need the package's `-race` tests green.

**Code rules (from CLAUDE.md — non-negotiable):**
- Table-driven tests with `t.Parallel()` at the outer function and each subtest
  (matches existing style in `internal/content/block_test.go`,
  `internal/agent/loop/loop_test.go`).
- Every distinct failure mode is a concrete typed error struct (`errors.As`-able);
  sentinels only for context-free leaf errors (e.g. `ErrSessionStopped`).
- No `any`/`interface{}` except at serialization boundaries; narrow immediately.
- Cover happy path, boundary (zero/empty/max), error, and domain edge cases.
- Commit messages: conventional (`feat(loop): …`, `refactor(event): …`) and end with
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

**Package map (current):**
- `internal/content` — `message.go`, `block.go`, `chunk.go`, `block_json.go`
- `internal/agent/loop/event` — `event.go`, `turn.go`, `tool.go`, `sink.go`, `errors.go`
- `internal/agent/loop` — `loop.go` (`Loop`, `New`, `listen`, `loopState`), `turn.go`
  (`runTurn`, `streamOnce`, `toolAccumulator`), `runner.go` (`RunBatch`), `gate.go`,
  `config.go`, `deps.go`, `errors.go`
- `internal/agent/loop/command` — `command.go`, `header.go`, `start_turn.go`,
  `interrupt.go`, `shutdown.go`, `approve.go`, `deny.go`, `provide_user_input.go`
- `internal/agent/session` — `agent.go` (`AgentSession`, `NewAgent`, `Invoke`,
  `Stream`, `Interrupt`, `Shutdown`, `Approve`, `Deny`, `ProvideUserInput`)
- `internal/llm` — `stream.go` (`StreamReader[T]`, `NewStreamReader`)
- `tui/` — `agent.go` (`StreamBlocks`), `screen.go`, `commands.go`, `transcript.go`,
  `render.go`, `scrollback.go`
- `cmd/cli` — CLI entry point

**Phases map 1:1 to the spec's "Implementation Order" (steps 1–11).**

---

## Phase 1: Event API — Header, Scope, Class, mixins, concrete events

**Design ref:** *Event API: header, scope, class, and concrete events*; *Three
orthogonal axes*. **Depends on:** nothing. This is the foundation.

This rebuilds the `event` package from bare structs into `Header` + one lifecycle
mixin + one scope mixin per event. It is additive-then-migrating: introduce the new
shape, then make existing emit sites compile against it. `EventEnvelope` (in
`sink.go`) stays for now (spec defers its retirement).

### Task 1.1: Lifecycle + scope mixins and the `Event` interface

**Files:**
- Modify: `internal/agent/loop/event/event.go`
- Create: `internal/agent/loop/event/mixins_test.go`

**Step 1 — Write the failing test.** Assert each mixin reports the right
`Class()`/`Scope()`/`EndsTurn()`:

```go
package event_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
)

func TestLifecycleMixins(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		ev        event.Event
		class     event.Class
		endsTurn  bool
	}{
		{"ephemeral token", event.TokenDelta{}, event.Ephemeral, false},
		{"enduring stepdone", event.StepDone{}, event.Enduring, false},
		{"terminal done", event.TurnDone{}, event.Enduring, true},
		{"terminal failed", event.TurnFailed{}, event.Enduring, true},
		{"terminal interrupted", event.TurnInterrupted{}, event.Enduring, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.ev.Class(); got != tt.class {
				t.Errorf("Class() = %v, want %v", got, tt.class)
			}
			if got := tt.ev.EndsTurn(); got != tt.endsTurn {
				t.Errorf("EndsTurn() = %v, want %v", got, tt.endsTurn)
			}
		})
	}
}
```

**Step 2 — Run, expect FAIL** (`Class`/`StepDone`/`Enduring` undefined):
`go test ./internal/agent/loop/event/ -run TestLifecycleMixins`.

**Step 3 — Implement.** In `event.go`, replace the bare `Event` interface with the
spec's shape and add the mixins (copy verbatim from the design spec's *Event API*
block): `Class`/`Scope` enums, `ephemeral`/`enduring`/`terminal`,
`sessionScoped`/`loopScoped`, the `Header` struct, `EventHeader()`, and the
extended `Event` interface (`isEvent()`, `Class()`, `Scope()`, `EndsTurn()`,
`EventHeader()`). Keep `type TurnIndex int`.

**Step 4 — Run, expect PASS** (existing concrete events won't satisfy the new
interface yet — that's Task 1.2; if the package doesn't compile, comment out the
not-yet-migrated event files' `isEvent()` lines or do 1.2 in the same commit).

**Step 5 — Commit:** `refactor(event): add Header, Class/Scope, lifecycle+scope mixins`.

### Task 1.2: Migrate concrete events onto Header + mixins

**Files:**
- Modify: `internal/agent/loop/event/event.go` (`SessionStarted`; add `SessionActive`,
  `SessionIdle`, `SessionStopped`, `LoopIdle`)
- Modify: `internal/agent/loop/event/turn.go` (`TurnStarted`, `TokenDelta`,
  `TurnDone`, `TurnFailed`, `TurnInterrupted`; add `StepDone`, `TurnFoldedInto`,
  `InputCancelled`; add `CancelReason`)
- Modify: `internal/agent/loop/event/tool.go` (`PermissionRequested`,
  `UserInputRequested`, `ToolCallStarted`, `ToolCallCompleted` gain `Header` +
  `loopScoped` + `enduring`)
- Create: `internal/agent/loop/event/scope_test.go`

**Step 1 — Failing test:** for every concrete event, assert `Scope()` and that a
`var _ event.Event = T{}` compiles. Also assert session-scoped events report
`ScopeSession` and turn events `ScopeLoop`:

```go
func TestEventScopes(t *testing.T) {
	t.Parallel()
	session := []event.Event{event.SessionStarted{}, event.SessionActive{}, event.SessionIdle{}, event.SessionStopped{}}
	loop := []event.Event{event.LoopIdle{}, event.TokenDelta{}, event.StepDone{}, event.TurnDone{}}
	for _, ev := range session {
		if ev.Scope() != event.ScopeSession {
			t.Errorf("%T Scope() = %v, want ScopeSession", ev, ev.Scope())
		}
	}
	for _, ev := range loop {
		if ev.Scope() != event.ScopeLoop {
			t.Errorf("%T Scope() = %v, want ScopeLoop", ev, ev.Scope())
		}
	}
}
```

**Step 2 — Run, expect FAIL** (new events undefined).

**Step 3 — Implement.** Apply the spec's concrete event declarations verbatim
(*Event API* block): embed `Header` + exactly one lifecycle + one scope mixin in
each. `StepDone{Header; Messages content.AgenticMessages}`, the new
`SessionActive`/`SessionIdle`/`SessionStopped`/`LoopIdle`, `TurnFoldedInto`,
`InputCancelled`, plus `TurnStarted.Message`. Add `CancelReason` enum. Keep each
type's `isEvent()` marker (sealed union). Remove fields now covered by `Header`
(e.g. the events no longer carry their own ad-hoc ids).

**Step 4 — Run, expect PASS.** Then `go build ./...` will fail at every *emit site*
(loop/turn.go, session) — that's expected; those are migrated in later phases. To
keep the tree compiling, in this task only update the `event` package itself and add
`var _ event.Event = …` assertions in a `doc.go` for the full set. Defer emit-site
changes to the phase that owns each site (note it in the commit body).

**Step 5 — Commit:** `refactor(event): move concrete events onto Header + mixins; add StepDone/LoopIdle/Session* events`.

### Task 1.3: Keep `SinkProjection`/`Redactable` working

**Files:** Modify `internal/agent/loop/event/turn.go`, `tool.go` (the existing
`SinkProjection()` methods); Modify `internal/agent/loop/event/sink.go` if envelope
construction reads fields that moved into `Header`.

**Steps:** Run the existing `sink_projection_test.go` (`go test ./internal/agent/loop/event/`).
Fix `SinkProjection()` methods to construct the new event shapes (carry `Header`
through; redact payload as before). Add a table case for `StepDone` if it needs a
projection (spec defers new-event redaction — leave `StepDone` unredacted, with a
`// TODO(Open Items B): journal/redaction follow-on` comment). Commit:
`refactor(event): keep sink projections compiling against new event shapes`.

**Phase 1 gate:** `make secure && make build` (the `event` package and its tests
compile and pass; emit-site breakage is expected and tracked).

---

## Phase 2: Rename `content.ToolMessage` → `content.ToolResultMessage`

**Design ref:** *Content Message Types*. **Depends on:** nothing (mechanical, do
it early while the surface is small — 5 files).

### Task 2.1: Rename the type and its codec

**Files:**
- Modify: `internal/content/message.go:34-37,45-48,91-106` (type, `isMessage()`
  marker, `MarshalJSON`/`UnmarshalJSON`, `toolMessageJSON` → `toolResultMessageJSON`)
- Modify: `internal/content/message_test.go` (rename references; the role stays
  `RoleTool`)

**Step 1 — Failing test:** add/rename a round-trip JSON test asserting
`content.ToolResultMessage{Message: …, ToolUseID: "x"}` marshals with
`"tool_use_id":"x"` and `"role":"tool"` and unmarshals back equal.

**Step 2 — Run, expect FAIL** (type undefined).

**Step 3 — Implement:** rename `ToolMessage` → `ToolResultMessage` (type, methods,
JSON helper struct). Role constant unchanged.

**Step 4 — Run, expect PASS** for the content package.

**Step 5 — Commit:** `refactor(content): rename ToolMessage to ToolResultMessage`.

### Task 2.2: Update all references

**Files:**
- Modify: `internal/llm/openaiapi/encode.go:90-95` (type switch case)
- Modify: `internal/agent/loop/turn.go` (`toolResultMessage` constructor + comments
  at ~201-206, 26, 56, 199)
- Modify: `internal/agent/loop/runner.go:56` (comment)
- Grep guard: `grep -rn "ToolMessage" --include='*.go' .` must return **only**
  `ToolResultMessage` hits.

**Steps:** Update each site, `make test`, then `make secure`. Commit:
`refactor: update all ToolMessage references to ToolResultMessage`.

**Phase 2 gate:** `make test && make secure`.

---

## Phase 3: Session/loop construction boundary

**Design ref:** *Session* (the `AgentSession`/`NewLoop`/`loopHandle` block),
*Loop* (the `loopCtx` derivation). **Depends on:** Phase 1.

Introduce `sessionCtx`/`sessionCancel`, the `loops` registry keyed by loop id,
`primaryLoopID`, `loop.Provenance`, and `NewLoop`. The loop's `New` signature gains
`sessionID, loopID, parent, events`. Behavior for the single primary loop is
unchanged from the caller's view.

### Task 3.1: `loop.Provenance` and the new `loop.New` signature

**Files:**
- Modify: `internal/agent/loop/loop.go` (add `Provenance`; change
  `New(ctx, sessionID, cfg)` → `New(loopCtx, sessionID, loopID, parent, events, cfg)`;
  thread `loopID`/`parent` into `loopState`)
- Modify: `internal/agent/loop/loop_test.go` + `fake_test.go` helpers (`newLoop`
  must pass the new args)
- Modify call site: `internal/agent/session/agent.go` (temporary: pass
  `s.SessionID`, a fresh loop id, `loop.Provenance{}`, and a placeholder publisher)

**Step 1 — Failing test:** a `loopState` constructed via `newLoopState(sessionID,
loopID, parent)` carries all three; `New` rejects a nil `events` publisher with a
typed error.

**Step 2–4:** Implement `Provenance` (verbatim from spec), `newLoopState`, and the
new `New`. The `events eventPublisher` param is a new in-package interface
(`PublishEvent(context.Context, event.Event) error`) — define it in `loop.go`
(consumer-side interface, DIP). For now `New` can accept a no-op publisher so the
loop compiles; the real hub arrives in Phase 4.

**Step 5 — Commit:** `feat(loop): add Provenance and session/loop identity to New`.

### Task 3.2: `AgentSession` owns `sessionCtx`, `loops`, `primaryLoopID`, `NewLoop`

**Files:**
- Modify: `internal/agent/session/agent.go` (struct fields per spec: `SessionID`,
  `sessionCtx`/`sessionCancel`, `loopsMu`, `loops map[uuid.UUID]*loopHandle`,
  `primaryLoopID`, `hub`, `idGen`; add `loopHandle{loop, parent, cancel}`,
  `NewLoop`, `loopFor`); rewrite `NewAgent` to derive `sessionCtx`, mint
  `SessionID`, call `NewLoop(loop.Provenance{}, cfg)`, store `primaryLoopID`)
- Modify: `internal/agent/session/agent_test.go`

**Step 1 — Failing tests** (table-driven):
- `NewAgent` produces a session with exactly one loop in `loops`, and
  `primaryLoopID` indexes it.
- `NewLoop` mints a fresh loop id from `idGen`, derives `loopCtx` from `sessionCtx`,
  stores a `loopHandle` with the given `parent` and a non-nil `cancel`.
- `loopFor(primaryLoopID)` returns the loop; `loopFor(random)` returns `false`.
- `idGen` failure → `NewLoop` returns `*SessionError{Kind: SessionLoopIDGenerationFailed}`.

**Step 2–4:** Implement per the spec's `Session` block verbatim (adapt
`uuid.UUID` to the internal uuid package; `idGen` returns `(uuid.UUID, error)`).
`Invoke`/`Stream` route to `primaryLoopID` via `loopFor`.

**Step 5 — Commit:** `feat(session): own sessionCtx, loop registry, primaryLoopID, NewLoop`.

**Phase 3 gate:** `make test && make secure && make build`.

---

## Phase 4: Session event fan-in (publish/subscribe + quiescence)

**Design ref:** *Event Fan-In* (all subsections), *Federated quiescence*,
*EventFilter*. **Depends on:** Phases 1, 3. **This is the largest new subsystem.**

Build the hub: `eventPublisher`/`eventSubscriber`, `EventFilter`/`LoopScope`,
`EventSubscription`, `SubscriptionLossError`, `sessionState` + the shared
`applyActivity` edge helper, `PublishEvent`, `expectTurn`/`cancelExpectTurn`,
`stopSession`, and `WaitIdle`. `AgentSession` becomes the `eventHub`.

### Task 4.1: `EventFilter`, `LoopScope`, `shouldDeliver`

**Files:** Create `internal/agent/loop/event/filter.go`, `filter_test.go`.

**Steps (TDD):** Implement `EventFilter`, `LoopScope`, `LoopScope.Matches`, and
`shouldDeliver(filter, ev)` verbatim from the spec's *EventFilter* block. Tests
(table-driven): session-scoped events always deliver; loop-scoped match by class +
producer `LoopID`; `All` short-circuits; empty `Loops` map matches nothing. Commit:
`feat(event): add EventFilter/LoopScope and shouldDeliver`.

### Task 4.2: `EventSubscription` + bounded egress + `SubscriptionLossError`

**Files:** Create `internal/agent/loop/event/subscription.go`, `subscription_test.go`
(or under a new `internal/agent/session/hub` package — see note).

> **Placement note:** the hub coordinates subscribers + `sessionState` and is owned
> by `AgentSession`. Put the hub in a new `internal/agent/session/hub` package (so
> `event` stays a pure vocabulary package and the session depends on it). The
> `eventPublisher`/`eventSubscriber` interfaces live where consumed: define
> `eventPublisher` in `loop` (Task 3.1) and `eventSubscriber` in `session`.

**Steps (TDD):** `EventSubscription{ events chan event.Event; err; closeOnce }`
with `Events() <-chan event.Event`, `Close() error`, `Err() error`. Each
subscription owns one bounded channel (cap is a config constant, e.g. 256).
`SubscriptionLossError{DroppedClass, Cause}` (typed). Tests: `Close` is idempotent
and `Err()==nil` after intentional close; a forced-loss close makes `Err()` return
the typed error and `Events()` is closed. Commit:
`feat(hub): EventSubscription with bounded egress and typed loss error`.

### Task 4.3: `sessionState` + `applyActivity` edge helper

**Files:** Create `internal/agent/session/hub/state.go`, `state_test.go`.

**Step 1 — Failing tests** (this is the heart of quiescence — test it hard):
- `activityKey{kind, id}`: `{loop,X}` and `{wake,X}` coexist (distinct entries).
- `applyActivity(add {loop,X})` from empty → returns a `SessionActive` post and sets
  `phase=SessionActive`.
- `applyActivity(remove {loop,X})` to empty → returns `SessionIdle`, signals
  `WaitIdle`, sets `phase=SessionIdle`.
- Idempotent add (`{loop,X}` twice) crosses no second edge (no double post).
- A single op that removes `{wake,s}` and adds `{loop,p}` crosses **no** edge
  (`active` never empties).
- When `phase==SessionStopped`, `applyActivity` is a no-op returning nil.
- Zero-value `sessionState` is `SessionIdle` with empty `active` (enum zero value).

**Step 2 — Run, expect FAIL.**

**Step 3 — Implement** `applyActivity(mutate func()) event.Event` verbatim from the
spec's pseudo-code (compare emptiness before/after; derive at-most-one edge;
`SessionActive` on Idle→Active, `SessionIdle` + WaitIdle signal on Active→Idle).
Define `SessionPhase` with `SessionIdle` as the **zero value** (per spec enum order),
`ErrSessionStopped` sentinel, and the `WaitIdle` waiter registry.

**Step 4 — Run, expect PASS.**

**Step 5 — Commit:** `feat(hub): sessionState with applyActivity edge derivation`.

### Task 4.4: Hub `PublishEvent`, `SubscribeEvents`, lock discipline

**Files:** Create `internal/agent/session/hub/hub.go`, `hub_test.go`.

**Step 1 — Failing tests:**
- No subscribers → `PublishEvent` returns nil, still applies the `sessionState`
  transition (verify via a follow-up `WaitIdle`/phase check), never blocks.
- One subscriber receives a published loop event filtered by its `EventFilter`.
- A `TurnStarted` (Active edge) followed by `LoopIdle` (Idle edge) delivers, in
  order, the loop events **and** the derived `SessionActive`/`SessionIdle`.
- A slow Ephemeral subscriber drops `TokenDelta` (egress full) without blocking the
  publisher or other subscribers.
- An Enduring overflow **closes** that subscription with `SubscriptionLossError`
  (`Err()` returns it), does not block others.
- Delivery happens outside the lock: a subscriber that never reads does not stall a
  second subscriber's `SubscribeEvents`/another publish (use a barrier/timeout).

**Step 2–4:** Implement `PublishEvent` and the session-method entry points
(`expectTurn`/`cancelExpectTurn`/`stopSession`) **all routing active/phase mutation
through `applyActivity` under one `sync.RWMutex`**, copying the subscriber slice
inside the lock and delivering outside it (verbatim from the spec's *Publisher /
subscriber* policy + `PublishEvent`/`stopSession` pseudo-code). Apply
`shouldDeliver` + the class-aware overflow policy on **every** delivery path
(including post-stop and the derived posts). `WaitIdle(ctx)` checks `phase` on entry
(return `ErrSessionStopped` if stopped) and otherwise waits for the Idle signal or
ctx.

**Step 5 — Commit:** `feat(hub): PublishEvent/Subscribe with lock-out-of-delivery and quiescence`.

### Task 4.5: Wire `AgentSession` to the hub; add `SessionStarted`/`stopSession`

**Files:** Modify `internal/agent/session/agent.go` (`hub` field constructed in
`NewAgent`, emit `SessionStarted` at construction; loops get the hub as their
`events` publisher via `NewLoop` → `loop.New`); `Shutdown` calls `hub.stopSession()`
**first**, then sends `Shutdown` to loops (with the `Done`/`ctx` send escape), then
`sessionCancel()`. Add `WaitIdle` and `SubscribeEvents` as `AgentSession` methods.

**Steps (TDD):** `Shutdown` drives the session to `SessionStopped`, `WaitIdle`
returns `ErrSessionStopped`, and a late loop event after stop is delivered but does
not flip phase back. Commit: `feat(session): wire hub, SessionStarted, Shutdown→stopSession`.

**Phase 4 gate:** `make test && make secure && make build`. The `expectTurn` token
is defined but inert (no async subagents yet) — that's expected per spec step 4.

---

## Phase 5: `internal/content/streamaccumulator`

**Design ref:** *Stream Accumulator*. **Depends on:** nothing (pure). Extract the
existing `toolAccumulator` (loop/turn.go:234-295) plus add `Thinking`/`Text`.

### Task 5.1: `ToolUses` (port + harden)

**Files:** Create `internal/content/streamaccumulator/streamaccumulator.go`,
`streamaccumulator_test.go`.

**Step 1 — Failing tests** (table-driven): multi-fragment single index concatenates
`InputJSON`; multi-index returns ascending `ToolUseBlock`s; **negative and huge
indexes don't panic or over-allocate** (map-backed, per spec); `Empty()` true before
any add.

**Step 2 — Run, expect FAIL.**

**Step 3 — Implement** `ToolUses` with `Add(*content.ToolUseChunk)`,
`Blocks() []content.ToolUseBlock`, `Empty() bool`, backed by `map[int]*toolPart`
and a first-seen order slice (port `toolAccumulator`), returning blocks sorted by
index.

**Step 4 — PASS. Step 5 — Commit:** `feat(content): add streamaccumulator.ToolUses`.

### Task 5.2: `Text` and `Thinking`

**Files:** same package + test.

**Steps (TDD):** `Text.Add(*TextChunk)` concatenates into one `*TextBlock`;
`Thinking.Add(*ThinkingChunk)` into one `*ThinkingBlock` and **does not set
`Signature`** (assert empty — conscious omission per spec, leave the
`// TODO: future provider that streams signatures` comment). `Empty()` for both.
Commit: `feat(content): add streamaccumulator.Text and Thinking`.

**Phase 5 gate:** `make test && make secure`.

---

## Phase 6: `blockState` and chunk folding through the accumulator

**Design ref:** *Block*, *Chunk*. **Depends on:** Phase 5.

Introduce the block layer in `loop` and route streaming through the shared
accumulator while preserving `TokenDelta` emission. No external behavior change yet.

### Task 6.1: `blockState`/`blockMessages` + `AIMessage()`/`ToolUses()`

**Files:** Create `internal/agent/loop/block.go`, `block_test.go`.

**Steps (TDD):** `blockMessages{thinking, text, toolUses}` (the three accumulators);
`blockState.AIMessage() *content.AIMessage` materializes one assistant message from
the accumulated thinking+text+tool-use blocks (in block order); `blockState.ToolUses()
[]content.ToolUseBlock` returns the executable view. Test: feeding text+thinking+
tool-use chunks yields one `AIMessage` containing a `TextBlock`, `ThinkingBlock`,
and `ToolUseBlock`. Commit: `feat(loop): add blockState materializing one AIMessage`.

### Task 6.2: `chunkProcessor` — emit then accumulate

**Files:** Create `internal/agent/loop/chunk.go`, `chunk_test.go`; modify
`internal/agent/loop/turn.go` (`streamOnce` uses `chunkProcessor` instead of the
inline `toolAccumulator`; delete `toolAccumulator`/`toolPart`).

**Step 1 — Failing test:** `chunkProcessor.process(chunk)` calls `emit(TokenDelta{…})`
**then** folds into `blockState` — assert ordering and that a dropped/ignored emit
still folds (accumulation is independent of emission).

**Step 2–4:** Implement `chunkProcessor{emit, state}` per spec; rewire `streamOnce`
to construct a `blockState`, loop chunks through the processor, and materialize via
`blockState.AIMessage()`/`ToolUses()`. The malformed-tool-use handling stays as
today (stored message serializable; raw executable use can still error).

**Step 5 — Commit:** `refactor(loop): fold chunks through chunkProcessor + streamaccumulator`.

**Phase 6 gate:** `make test && make secure`. Existing loop/turn tests must stay
green — this is an internal refactor.

---

## Phase 7: `stepState` and the `StepDone` self-heal anchor

**Design ref:** *Step*, *StepDone and the self-heal contract*. **Depends on:** 6.

### Task 7.1: `stepState`/`step` + `runStep`

**Files:** Create `internal/agent/loop/step.go`, `step_test.go`; modify `turn.go`.

**Steps (TDD):** `stepState{sessionID, loopID, turnID, id, index, msgs, blocks,
status}`, `newStepState(...)`, `runStep(ctx, stepConfig, step) stepResult`. A step
finalizes exactly one `AIMessage` into `msgs[0]`; tool execution appends
`ToolResultMessage`s after it; **`stepState` cannot finalize an empty assistant
response** (typed error). Test the shape rules. Commit:
`feat(loop): add stepState and runStep (one LLM cycle → AIMessage + tool results)`.

### Task 7.2: Emit `StepDone{Messages}` at step completion

**Files:** modify `turn.go` (emit point), `event` already has `StepDone`.

**Steps (TDD):** each completed step emits exactly one `StepDone` carrying the
finalized group (`AIMessage` + its `ToolResultMessage`s). A step with text+tool-use
produces one `AIMessage` with both blocks. Use the loop test helpers (`newLoop`,
`startTurn`, `drainToTerminal`) and a `scriptedLLM`. Commit:
`feat(loop): emit StepDone with the finalized step group`.

**Phase 7 gate:** `make test && make secure`.

---

## Phase 8: `turnState` and loop-owned incremental commit

**Design ref:** *Turn*, *Message Containers*, *Turn Flow*. **Depends on:** 7.

This introduces the `base + turnState.msgs` request model, the `commit`/`drainPending`
handshakes back to the actor, and **step-granularity rollback**. The `commit`
handshake here only needs `StepDone` (fold/`TurnFoldedInto` arrives in Phase 9).

### Task 8.1: `turnState` + `turnConfig` + `runTurn` staging

**Files:** Create `internal/agent/loop/turnstate.go`, tests; heavily modify
`turn.go` (`runTurn` rewritten around `turnState`).

**Steps (TDD):**
- `turnState` starts with exactly one initial `UserMessage`; appends complete
  `stepState.msgs` groups; LLM request is built from `turnConfig.base + turnState.msgs`
  (never live `loopState.msgs`).
- **`turnConfig.base` is a defensive clone** of the pre-turn `loopState.msgs` (own
  backing array — test that appending to the source after constructing `base` does
  not mutate `base`). *(Open Items A / immutability invariant.)*
- A tool-using turn with multiple LLM responses produces one `UserMessage`, multiple
  `AIMessage`s, and matching `ToolResultMessage`s in order.

Implement `turnState`/`turnConfig`/`turnCommit` and `runTurn(ctx, cfg, turn)` per
spec. Define concrete typed errors for `commit`/`drainPending` failures *(Open
Items A)*. Commit: `feat(loop): stage turns via turnState with base+staged request`.

### Task 8.2: Loop-owned incremental commit + step-granularity rollback

**Files:** modify `loop.go` (`runLoop`/`listen` owns `loopState.msgs`; services the
`commit` handshake), `turn.go`.

**Steps (TDD):**
- `runLoop` commits the initial `UserMessage` + emits `TurnStarted{Message,
  Header.CausationID}` **before** `runTurn`; appends each step group + emits
  `StepDone` at the same actor-owned point.
- A turn that fails/interrupts at step N keeps steps 1..N-1 committed (with their
  `StepDone`s) and discards only step N's in-flight state. (Replaces whole-turn
  rollback — update/replace the old rollback test in `turn_test.go`.)

Commit: `feat(loop): incremental loop-owned commit with step-granularity rollback`.

**Phase 8 gate:** `make test && make secure && make build`.

---

## Phase 9: Input as one submit command + quiescence wiring

**Design ref:** *Routing*, *Queued input*, *Federated quiescence*, *Subagent
hand-back*. **Depends on:** 4, 8. **Largest control-flow change.**

Replace `StartTurn`/steering with `command.UserInput` + `command.SubagentResult`
returning a typed `Disposition`; add the actor-owned `inbox`, `drainPending`,
`CancelQueuedInput`, the `tryAck[T]` helper, and the `expectTurn`/`cancelExpectTurn`
quiescence wiring (`LoopIdle` emission, `SessionActive`/`SessionIdle` via the hub).

### Task 9.1: Submit/disposition/cancel command types + `tryAck[T]`

**Files:** Create `internal/agent/loop/command/user_input.go`,
`subagent_result.go`, `cancel_queued_input.go`; modify `command/approve.go`/
`deny.go`/`provide_user_input.go` to carry `command.Route`; create
`internal/agent/loop/command/route.go`. Create `internal/agent/loop/disposition.go`
(`Disposition`, `TurnStarted`/`InputQueued`/`TurnRejected`, `RejectReason`,
`InputMode`) and `internal/agent/loop/ack.go` (`tryAck[T any]`). Delete
`start_turn.go`.

**Steps (TDD):**
- `tryAck` on a buffered(1) channel delivers; on a nil/unbuffered/full channel it
  hits `default` and does **not** block (the actor-safety contract). Reuse it for
  `Disposition`, `CancelResult`, and `gate.reply` (`command.Command`).
- `Route` carries `SessionID/LoopID/TurnID/StepID/ToolCallID`.
- **Rename the disposition `TurnStarted` to avoid colliding with `event.TurnStarted`**
  *(Open Items A)* — e.g. `Started{TurnID, InputID}`.

Commit: `feat(loop): add UserInput/SubagentResult/CancelQueuedInput, Disposition, tryAck, Route`.

### Task 9.2: Actor decision path + `inbox`

**Files:** modify `loop.go` (`loopState.inbox []queuedInput`, `inboxCap`, the
decide-on-own-state branch), `loop_test.go`.

**Steps (TDD)** — mirror the spec's *Routing* decision table and *Testing* list:
- idle + `UserInput` → `Started{turnID,inputID}` + `event.TurnStarted`.
- running + queueable → `InputQueued{inputID}` (appended to `inbox`, ordered).
- non-queueable/`StartOnly` busy / `inboxCap` / shutting down → `TurnRejected{Reason}`
  (a length check; the actor never blocks).
- On normal turn completion with non-empty `inbox`, the actor pops the first entry
  and starts a new turn (no stranded input).

Commit: `feat(loop): actor decides start/queue/reject on its own state`.

### Task 9.3: `drainPending` + fold + `CancelQueuedInput`

**Files:** modify `loop.go`, `turn.go`.

**Steps (TDD):**
- `drainPending(ctx)` (request/reply to the actor, selects on reply **and**
  `turnCtx.Done`) returns + clears accepted inbox messages only at a tool-continuation
  boundary; folded messages append after tool results, commit + emit
  `TurnFoldedInto`. Never drains after a no-tool final answer.
- An `Interrupt` while parked in `drainPending` frees `runTurn` (selects on Done).
- `CancelQueuedInput`: while queued → `Cancelled` + `InputCancelled{CancelClientRetracted}`;
  after start/fold → `AlreadyCommitted`. A racing cancel vs. drain resolves
  deterministically on the actor.
- Abnormal terminal returns still-queued inbox via `InputCancelled{Reason, Message}`;
  **no** auto-start.

Commit: `feat(loop): tool-continuation drainPending, fold, and CancelQueuedInput`.

### Task 9.4: `LoopIdle` + quiescence + session ownership of `expectTurn`

**Files:** modify `loop.go` (emit `LoopIdle` on running→idle), `internal/agent/session/agent.go`
(session owns `expectTurn`/`cancelExpectTurn`; the `SubagentResult` round trip reads
the `Disposition` and on `TurnRejected` calls `hub.cancelExpectTurn`).

**Steps (TDD):**
- A loop emits `LoopIdle` (Enduring, non-terminal) on running→idle; a chained
  turn N→N+1 emits no `LoopIdle` between them (idempotent `{loop}` key).
- With a `{wake,s}` token outstanding (set via `expectTurn`), `active` is non-empty
  even when every loop is idle; `SessionIdle` fires only after the token releases
  (publish-path `TurnStarted`/`TurnFoldedInto`/`InputCancelled` carrying
  `TriggeredByLoopID`, **or** session-path `cancelExpectTurn` on `TurnRejected`).
- Synchronous reduction: no `expectTurn`; quiescence ≡ primary loop idle.

> **Open Items A reminder:** the async-spawn path that *calls* `expectTurn` is
> deferred orchestration. Add `expectTurn`/`cancelExpectTurn` as session methods and
> unit-test them against the hub, but leave the spawn caller as a documented TODO so
> the `{wake}` token is taken before a subagent could complete its first turn.

Commit: `feat(session): LoopIdle quiescence + session-owned expectTurn/cancelExpectTurn`.

**Phase 9 gate:** `make test && make secure && make build`.

---

## Phase 10: Rename `listen` → `runLoop`; reshape around `loopConfig`/`loopState`

**Design ref:** *Loop* (the `loopConfig`/`loopState`/`runLoop` block). **Depends
on:** all prior. Mostly mechanical consolidation now that the pieces exist.

### Task 10.1: Reshape the actor

**Files:** modify `internal/agent/loop/loop.go`.

**Steps:** Rename `listen` → `runLoop(ctx, cfg loopConfig)`. Collapse the actor's
locals into explicit `loopConfig` (deps/wiring: `loopCtx`, `cfg`, `commands`,
`gateReg`, `internal`, `done`, `events`) and `loopState` (identity/status/`msgs`/
`inbox`/`activeTurn`/`shutdownAcks`). No new wrapper type. Per-turn `cancel` derives
from `cfg.loopCtx` (`context.WithCancel`) and is stored on `activeTurn` — submit
commands carry no context (verify there is no `Ctx` field on `UserInput`/
`SubagentResult`). Update all loop tests to the new names. Commit:
`refactor(loop): rename listen→runLoop around loopConfig/loopState`.

**Phase 10 gate:** `make test && make secure && make build`. Add the
`SessionInterruptCanceled` kind to the `SessionError` set if not already present
*(Open Items A)*; confirm `Interrupt`/`Shutdown` sends use the `Done`/`ctx` escape.

---

## Phase 11: TUI/CLI — subscribe to the session fan-in, render `StepDone`

**Design ref:** *TUI/CLI Display*, *StepDone … UI message == stored message*.
**Depends on:** all prior. **Full change set is its own spec:**
`docs/plans/2026-06-18-tui-event-adoption-design.md` — this phase is the loop-side
seam + a minimal switch.

### Task 11.1: Session subscription entry point

**Files:** modify `internal/agent/session/agent.go` (ensure `SubscribeEvents(EventFilter)`
is the public entry), `tui/agent.go` (add a `Subscribe`-style method alongside or
replacing `StreamBlocks`).

**Steps (TDD):** A subscriber with primary-only Ephemeral + all-loop Enduring
receives a (future) subagent's `StepDone`, never its `TokenDelta`, and always the
session-scoped `SessionStarted`/`SessionActive`/`SessionIdle`/`SessionStopped`.
Commit: `feat(session): expose SubscribeEvents(EventFilter) for whole-session consumption`.

### Task 11.2: Render the stored group

**Files:** modify `tui/commands.go` (drain a subscription instead of/in addition to
the per-turn `StreamReader`), `tui/transcript.go`/`render.go` (on `StepDone`, render
`StepDone.Messages` as the committed per-step group; use `streamaccumulator` only
for the provisional live `AIMessage` between `TokenDelta`s).

**Steps:** Per the TUI-adoption spec. Acceptance: a multi-step turn renders as
separate `AIMessage`/`ToolResultMessage` entries (not collapsed); displayed
transcript == `StepDone.Messages` by construction; dropped `TokenDelta` reconciles
to the next `StepDone`. Manual check with `make run` (REQUIRED SUB-SKILL:
superpowers:verification-before-completion — run the app, observe, before claiming
done). Commit: `feat(tui): render StepDone groups; provisional live message via streamaccumulator`.

**Phase 11 gate:** `make test && make secure && make build && make run` (smoke).

---

## Definition of Done

- `make test` (`go test -race ./...`) green; `make secure` clean; `make build` ok.
- All invariants in the spec's **Invariants** section have a corresponding test
  (the spec's **Testing** section is the checklist — port each bullet).
- The spec's **Open Items & Follow-Ups → Group A** items are each either resolved or
  carried as an explicit `// TODO(Open Items A: …)` with a test guarding the seam.
- Group B items remain owned by their follow-on specs (journal/redaction,
  id-normalization, TUI-adoption, cross-turn handle model) — untouched here.

---

## Execution Handoff

This is a large, multi-phase plan that mutates shared loop/session code. Strongly
prefer running it in an isolated **git worktree** (REQUIRED SUB-SKILL:
superpowers:using-git-worktrees) so `main`/the current branch stays clean.

Two execution options:

1. **Subagent-Driven (this session)** — I dispatch a fresh subagent per task and
   code-review between tasks. Fast iteration, I stay in the loop.
   (REQUIRED SUB-SKILL: superpowers:subagent-driven-development.)
2. **Parallel Session (separate)** — open a new session in the worktree and execute
   with checkpoints. (REQUIRED SUB-SKILL: superpowers:executing-plans.)

Which approach do you want — and should I set up a worktree first?
