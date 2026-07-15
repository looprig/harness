# Hustle Mechanism Implementation Plan

> **Status:** Superseded by
> `docs/plans/2026-07-12-hustle-token-compaction-implementation.md`. Do not
> execute this partial plan: it predates the required core/inference/LLM
> foundations, public manual compaction path, CLI restore reducer, and SWE wiring.

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add bounded, session-owned, one-shot inference hustles and integrate the first `context.compact` consumer without turning hustles into loops.

**Architecture:** `pkg/hustle` owns immutable definitions and typed wire contracts; `internal/hustleruntime` owns two session-global lanes, inference execution, audit, activity, callbacks, and drain. `pkg/rig` composes definitions, `pkg/hub` owns internal durable publication and blocking activity, and `internal/sessionruntime` binds current-loop models and orders controller teardown before hub/lease release.

**Tech Stack:** Go standard library, existing `github.com/looprig/core`, existing `github.com/looprig/inference`, harness event/journal/rig/session packages; no new external dependencies.

---

## Preconditions

- Execute in a fresh worktree based on the commit containing
  `feat/rig-lifecycle-workspace-snapshots`; the plan's file paths assume the
  landed `pkg/rig`, `internal/sessionruntime`, and `internal/loopruntime` split.
- Use `superpowers:test-driven-development` for every task.
- Preserve the repository's mandatory table-driven tests and run all Go tests
  with `-race`.
- Do not implement classifier definitions until the structured-output
  prerequisite lands. The compaction slice uses text output only.
- Do not add function, tool, arbitrary HTTP, streaming, foreign-agent, or
  multi-turn hustle backends.

### Task 1: Immutable hustle definitions

**Files:**

- Create: `pkg/hustle/definition.go`
- Create: `pkg/hustle/definition_errors.go`
- Create: `pkg/hustle/run.go`
- Create: `pkg/hustle/definition_test.go`
- Create: `pkg/hustle/deps_test.go`

**Step 1: Write failing table-driven definition tests**

Cover valid named/current-loop definitions; nil/duplicate options; blank name,
prompt revision, and policy revision; invalid model source/model/timeout/limits;
defensive prompt/model copies; exact nanosecond timeout identity; prompt digest;
named-model policy digest; and absence of secrets from descriptors.

```go
func TestDefine(t *testing.T) {
	tests := []struct {
		name    string
		opts    []hustle.Option
		wantErr hustle.DefinitionErrorKind
	}{
		{name: "named model", opts: validNamedOptions()},
		{name: "missing name", opts: withoutName(validNamedOptions()), wantErr: hustle.DefinitionMissingName},
		{name: "zero timeout", opts: replaceTimeout(validNamedOptions(), 0), wantErr: hustle.DefinitionInvalidTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := hustle.Define(tt.opts...)
			assertDefinitionErrorKind(t, err, tt.wantErr)
		})
	}
}
```

**Step 2: Run the focused test and confirm RED**

Run: `GOWORK=off go test -race ./pkg/hustle -run 'TestDefine|TestDefinitionDescriptor'`

Expected: FAIL because `pkg/hustle` does not exist.

**Step 3: Implement the minimal immutable contract**

Implement `Name`, `RunID`, `Participation`, `ModelSource`, input/output `Limits`,
`DefinitionDescriptor`, `Request`, `Result`, `Outcome`, `Definition`, options,
`Bind`, and sealed `BoundDefinition`. Copy model sampling, prompts, schemas, and
raw JSON defensively. Use SHA-256 and canonical typed structs for revisions; do
not use `map[string]any`.

```go
type BoundDefinition interface {
	Name() Name
	Participation() Participation
	Timeout() time.Duration
	Limits() Limits
	Descriptor() DefinitionDescriptor
	ResolveInference(context.Context, uuid.UUID) (InferenceBinding, error)
	SystemPrompt() string
	boundDefinition()
}
```

Return concrete typed errors from every exported failure path.

**Step 4: Run focused tests and dependency boundary**

Run: `GOWORK=off go test -race ./pkg/hustle`

Expected: PASS. `deps_test.go` must prove `pkg/hustle` does not import
`pkg/event`, `pkg/rig`, `pkg/session`, or an internal package.

**Step 5: Commit**

```bash
git add pkg/hustle
git commit -m "feat(hustle): define immutable inference work"
```

### Task 2: Rig registration and fingerprint ownership

**Files:**

- Modify: `pkg/rig/definition.go`
- Modify: `pkg/rig/options.go`
- Modify: `pkg/rig/fingerprint.go`
- Modify: `pkg/rig/errors.go`
- Modify: `pkg/rig/rig_test.go`
- Modify: `pkg/rig/fingerprint_test.go`
- Modify: `pkg/rig/fingerprint_ownership_test.go`
- Modify: `pkg/rig/deps_test.go`

**Step 1: Write failing registration/fingerprint tests**

Add table rows for additive `WithHustles`, duplicate names, nil/invalid
definitions, duplicate `WithHustleLimits`, invalid lane limits, deterministic
name sorting, and fingerprint changes for every definition/lane behavior field.
Assert client identity, credentials, raw prompt/schema, and current resolved
model do not enter the fingerprint.

**Step 2: Run focused tests and confirm RED**

Run: `GOWORK=off go test -race ./pkg/rig -run 'TestDefine.*Hustle|Test.*Hustle.*Fingerprint'`

Expected: FAIL because the rig has no hustle options.

**Step 3: Implement registration**

Add `hustles []hustle.Definition` and one `HustleLimits` singleton to
`definitionState`. Validate all definitions before constructing the lifecycle.
Fold sorted `(Name, PolicyRevision)` rows and all lane/cleanup limits into
`topologyRevision`. Forward defensive copies through a new
`sessionruntime.WithLifecycleHustles` option.

**Step 4: Run rig tests**

Run: `GOWORK=off go test -race ./pkg/rig`

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/rig
git commit -m "feat(rig): register and fingerprint hustles"
```

### Task 3: Visibility and hustle lifecycle wire format

**Files:**

- Modify: `pkg/event/event.go`
- Modify: `pkg/event/marshal.go`
- Modify: `pkg/event/validate.go`
- Modify: `pkg/event/filter.go`
- Modify: `pkg/event/marshal_test.go`
- Modify: `pkg/event/validate_test.go`
- Modify: `pkg/event/filter_test.go`
- Create: `pkg/event/hustle_test.go`
- Create: `pkg/event/hustle_fuzz_test.go`

**Step 1: Write failing event tests**

Cover zero/public legacy visibility, internal round-trip, public filter denial,
started with zero runtime, completed with non-zero runtime, failed before/after
resolution, session-scoped coordinate rules, bounded reason/stage enums, and
defensive `Usage` copies.

**Step 2: Run focused tests and confirm RED**

Run: `GOWORK=off go test -race ./pkg/event -run 'Test.*Visibility|TestHustle'`

Expected: FAIL because event visibility/lifecycle types are absent.

**Step 3: Add wire types and codecs**

Add `EventVisibility`, `Header.EventVisibility`, `Header.Visibility`, and
`Visibility()` to sealed `Event`. Add `HustleRunDescriptor`, `HustleStarted`,
`HustleCompleted`, and `HustleFailed`; update marshal/unmarshal and validation
switches exhaustively. `ShouldDeliver` must return false for internal events
before session-scope handling.

**Step 4: Run unit and fuzz smoke tests**

Run: `GOWORK=off go test -race ./pkg/event`

Run: `GOWORK=off go test ./pkg/event -run '^$' -fuzz=FuzzHustleEvent -fuzztime=5s`

Expected: PASS with no panic.

**Step 5: Commit**

```bash
git add pkg/event
git commit -m "feat(event): add private hustle lifecycle"
```

### Task 4: Hub internal audit and activity leases

**Files:**

- Modify: `pkg/hub/hub.go`
- Modify: `pkg/hub/state.go`
- Modify: `pkg/hub/fault.go`
- Modify: `pkg/hub/hub_test.go`
- Modify: `pkg/hub/state_test.go`
- Modify: `pkg/hub/durable_tap_test.go`
- Modify: `pkg/hub/deps.go`

**Step 1: Write failing boundary tests**

Table-drive:

- ordinary publication rejects internal visibility;
- internal publication rejects public/unrecognized/ephemeral/wrong-session events;
- accepted internal lifecycle appends but does not fan out or mutate activity;
- committed activity acquisition/release produces correct active/idle edges;
- partial acquisition after `SessionActive` append failure returns a lease whose
  release silently rolls back without `SessionIdle`;
- release is concurrent-safe, idempotent, and caches its result; and
- stopped/aborted hub rejects acquisition.

**Step 2: Run focused tests and confirm RED**

Run: `GOWORK=off go test -race ./pkg/hub -run 'Test.*Internal|Test.*HustleActivity'`

Expected: FAIL because the hub seams are absent.

**Step 3: Implement checked seams**

Add `kindHustle` activity keys. Implement
`PublishInternalEventChecked` as append-only checked ingress. Implement
`AcquireHustleActivity` returning an internal lease that remembers committed vs
partial acquisition and owns exact-once release. Keep all I/O outside hub locks;
reuse existing derived-edge/fault/waiter rules.

**Step 4: Run hub tests**

Run: `GOWORK=off go test -race ./pkg/hub`

Expected: PASS, including existing loop/wake quiescence tests.

**Step 5: Commit**

```bash
git add pkg/hub
git commit -m "feat(hub): own private hustle audit and activity"
```

### Task 5: Controller lanes and ownership state machine

**Files:**

- Create: `internal/hustleruntime/controller.go`
- Create: `internal/hustleruntime/lane.go`
- Create: `internal/hustleruntime/contracts.go`
- Create: `internal/hustleruntime/errors.go`
- Create: `internal/hustleruntime/controller_test.go`
- Create: `internal/hustleruntime/lane_test.go`
- Create: `internal/hustleruntime/deps_test.go`

**Step 1: Write failing state-machine tests**

Cover both shared lanes, FIFO by ownership commit, total owned cap of
`Concurrent + Queued`, no cross-lane borrowing, rejection before ownership,
queued cancellation with finalizer, scheduler disabled after close/poison,
finalizing runs retaining ownership capacity, and exact drain completion.

Use channels only as deterministic test barriers; do not use sleeps.

**Step 2: Run focused tests and confirm RED**

Run: `GOWORK=off go test -race ./internal/hustleruntime -run 'TestLane|TestControllerAdmission'`

Expected: FAIL because the runtime package is absent.

**Step 3: Implement minimal lane/controller state**

Implement one controller mutex/condition (or equivalent channel state machine)
that atomically owns admission, poison, FIFO queues, execution slots, total
owned counts, cancellation, and drained notification. Mint `RunID` before queue
insertion; ownership begins only on insertion. Do not start model resolution in
this task—inject a blocking execution seam in tests.

**Step 4: Run runtime tests and race detector**

Run: `GOWORK=off go test -race ./internal/hustleruntime`

Expected: PASS with no race or goroutine leak in test cleanup.

**Step 5: Commit**

```bash
git add internal/hustleruntime
git commit -m "feat(hustleruntime): add bounded ownership lanes"
```

### Task 6: Inference execution, lifecycle, callbacks, and poison

**Files:**

- Modify: `internal/hustleruntime/controller.go`
- Create: `internal/hustleruntime/execution.go`
- Create: `internal/hustleruntime/audit.go`
- Create: `internal/hustleruntime/execution_test.go`
- Create: `internal/hustleruntime/audit_test.go`
- Create: `internal/hustleruntime/panic_test.go`

**Step 1: Write failing execution matrix**

Table-drive named/current resolution, start-before-resolution, one tool-less
`Invoke`, text extraction, output bounds, validator failure, usage on
post-inference failure, audit failure, inference timeout, queued cancellation,
finalizer failure, combined errors, validator/finalizer panic recovery, and
activity held through finalization.

Add a context-ignoring fake client. Prove `WorkerDrainTimeout` atomically poisons
both lanes, cancels waiting nodes, prevents further scheduler grants, bounds
abandoned workers by current execution slots, records nil unavailable usage,
and discards late results without callbacks/publication.

**Step 2: Run focused tests and confirm RED**

Run: `GOWORK=off go test -race ./internal/hustleruntime -run 'TestExecute|TestRunAndFinalize|TestWorkerPoison|TestCallbackPanic'`

Expected: FAIL because execution is not wired.

**Step 3: Implement execution and checked audit**

Create the capacity-one capability-free worker result path. Use separate
session-derived contexts for audit, finalization, worker drain, and activity
release. Recover callback panics at the controller boundary, discard panic
values, report a typed fault, and never let a background panic escape. Copy
usage before constructing events.

**Step 4: Run all runtime tests**

Run: `GOWORK=off go test -race ./internal/hustleruntime`

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/hustleruntime
git commit -m "feat(hustleruntime): execute and audit inference work"
```

### Task 7: Session binding, model resolution, and teardown

**Files:**

- Modify: `internal/sessionruntime/lifecycle.go`
- Modify: `internal/sessionruntime/session.go`
- Modify: `internal/sessionruntime/restore_constructor.go`
- Modify: `internal/sessionruntime/errors.go`
- Create: `internal/sessionruntime/hustle.go`
- Create: `internal/sessionruntime/hustle_test.go`
- Modify: `internal/sessionruntime/lifecycle_test.go`
- Modify: `internal/sessionruntime/restore_test.go`
- Modify: `pkg/session/contracts_test.go`

**Step 1: Write failing integration-shaped unit tests**

Cover transactional bind failure, named/current model resolution, committed
live model changes, missing/exited origin loop, no runner on public
`session.Session`/`SessionController`, restore not resuming unmatched starts,
and construction abort draining internal publishers.

Add shutdown-order tests proving:

```text
closing → controller close/cancel → loop drain → hustle drain
→ checkpoint/controller stop → SessionStopped → lease releases → session cancel
```

The caller deadline must cancel work and appear in the eventual error without
allowing hub/lease release before bounded non-abandonable cleanup finishes.

**Step 2: Run focused tests and confirm RED**

Run: `GOWORK=off go test -race ./internal/sessionruntime -run 'Test.*Hustle|TestShutdown.*Hustle|TestRestore.*Hustle'`

Expected: FAIL because lifecycle wiring is absent.

**Step 3: Wire the controller**

Capture definitions/limits in `Lifecycle`, bind against a session-local
`hustle.ModelResolver`, and inject hub audit/activity, event factory, and fault
reporter through narrow interfaces. Resolve current client from
`loopHandle.bound.Client()` and model from `loopHandle.Model()` after checking
the loop exists and has not exited. Keep the raw runner internal.

**Step 4: Run session and rig suites**

Run: `GOWORK=off go test -race ./internal/sessionruntime ./pkg/session ./pkg/rig`

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/sessionruntime pkg/session
git commit -m "feat(session): own hustle lifecycle and teardown"
```

### Task 8: Typed compaction adapter

**Files:**

- Modify: `pkg/event/event.go`
- Modify: `pkg/event/marshal.go`
- Modify: `pkg/event/validate.go`
- Modify: `pkg/event/event_test.go`
- Modify: `pkg/event/marshal_test.go`
- Create: `pkg/loop/compaction.go`
- Modify: `pkg/loop/definition.go`
- Create: `pkg/loop/compaction_test.go`
- Create: `internal/loopruntime/compaction.go`
- Modify: `internal/loopruntime/config.go`
- Modify: `internal/loopruntime/loop.go`
- Create: `internal/loopruntime/compaction_test.go`
- Create: `internal/sessionruntime/compaction_adapter.go`
- Create: `internal/sessionruntime/compaction_adapter_test.go`

**Step 1: Write failing typed-adapter tests**

Follow `2026-07-11-token-usage-context-occupancy-design.md`. Cover concrete
versioned `CompactionInput`/`CompactionOutput`, unknown fields, empty/oversized
summary, exact basis, `DisallowUnknownFields`, validator-before-completed audit,
failure finalization, and no generic runner exposure to the actor. Also cover
one public ephemeral `CompactionStarted` per accepted attempt, no duplicate for
coalesced waiters, ordering before invocation/terminal, no persistence or replay,
`{LoopID, AttemptID}` correlation, and non-negative terminal `Duration` on both
success and rejection. Enumerate interrupt, shutdown, stale-basis/CAS rejection,
preflight failure, ID-generation failure, lane-full, lane-closed, and progress
event construction/validation/publication failure. Assert that every successful
start has exactly one canonical terminal, a progress-publication failure invokes
no compactor, and a direct pre-ownership return is converted to
`CompactionRejected` plus all waiter replies. Add panic-before-terminal-append
and panic-after-terminal-append cases proving the actor's idempotent attempt
transition neither strands the start nor appends a second outcome.

**Step 2: Run focused tests and confirm RED**

Run: `GOWORK=off go test -race ./pkg/event ./pkg/loop ./internal/loopruntime ./internal/sessionruntime -run 'Test.*Compact'`

Expected: FAIL because the compaction adapter/path is absent.

**Step 3: Implement the narrow adapter and injection**

Declare `Compactor` with `CompactAndFinalize` in the compaction domain. Build a
session adapter bound to one loop ID and fixed `context.compact` name. Inject it
through bound loop runtime config. The adapter owns input marshal, output decode,
domain validation, and call-local capture of validated output; the actor owns
the `CompactionStarted` emission, private monotonic start time, terminal duration,
`CompactionCommitted`/`CompactionRejected` finalizer, and CAS policy. Emit the
start only after the basis is frozen and the attempt is accepted, immediately
before invoking the compactor. Add one actor-owned
`finalizeCompaction(AttemptID, outcome)` transition used by both the adapter
callback and direct-error fallback. It checks actor state, durably appends at
most one canonical terminal, records the terminal only after append success,
and returns the existing terminal to later requests. If start publication fails,
do not invoke the adapter and reject through this transition. If
`CompactAndFinalize` returns a typed pre-ownership error or its callback is
recovered before a terminal commits, map the error and submit a rejection; a
terminal already committed makes that fallback idempotently return without a
second append. Compute successful duration after CAS validation and immediately
before terminal event construction; compute rejected duration after selecting
the reject reason.

**Step 4: Run compaction suites**

Run: `GOWORK=off go test -race ./pkg/event ./pkg/loop ./internal/loopruntime ./internal/sessionruntime`

Expected: PASS; hustle usage must not change loop cumulative usage.

**Step 5: Commit**

```bash
git add pkg/event pkg/loop internal/loopruntime internal/sessionruntime
git commit -m "feat(compaction): use typed hustle adapter"
```

### Task 9: CLI compaction progress and completion presentation

This task lands in the sibling `github.com/looprig/cli` module after Task 8's
harness event contract is available. Keep it as a separate CLI commit.

**Files:**

- Modify: `../cli/tui/statusline.go`
- Modify: `../cli/tui/statusline_test.go`
- Modify: `../cli/tui/screen.go`
- Modify: `../cli/tui/screen_test.go`

**Step 1: Write failing reducer and presentation tests**

Add table-driven cases proving:

- focused-loop `CompactionStarted` changes the status label to
  `compacting conversation…` both during an active turn and during idle manual
  compaction;
- another loop's compaction does not change the focused loop's status;
- a matching `CompactionCommitted` or `CompactionRejected` clears the activity;
- a mismatched or stale `AttemptID` cannot clear a newer activity;
- replay or a terminal without an observed ephemeral start does not leave the
  status busy; and
- `CompactionCommitted{Duration: 25 * time.Second}` appends
  `○ conversation compacted in 25s` through `CommitHarnessFor`, reusing
  `formatElapsed`, while rejection appends no success row. Restore reconstructs
  one such row per committed terminal, and duplicate delivery or live/replay
  overlap cannot append it twice because completion presentation is keyed by
  terminal `Header.EventID`.

**Step 2: Run focused CLI tests and confirm RED**

Run from `../cli`:
`GOWORK=off go test -race ./tui -run 'Test.*Compaction|TestStatusLabel'`

Expected: FAIL because the screen has no compaction activity projection.

**Step 3: Implement the focused-loop activity projection**

Track the active compaction attempt per loop, keyed by
`{LoopID, AttemptID}`. Fold `CompactionStarted` into that projection and clear
only on a matching canonical terminal. Add `compacting` to `statusInputs`; give
it precedence over idle, waiting/thinking/streaming, and prompt substates, while
session-global interrupting/clearing still win. Treat that derived label as
active for the filled dot and animated gradient even when the turn-lifecycle
`Status` is idle. On `CompactionCommitted`, append the loop-scoped faint harness
row with its authoritative `Duration`, deduplicated by terminal event ID. Do not
use local arrival timestamps or reconstruct ephemeral activity on restore;
enduring committed terminals do reconstruct their deduplicated historical rows.

**Step 4: Run the CLI suites**

Run from `../cli`: `GOWORK=off go test -race ./...`

Run from `../cli`: `CGO_ENABLED=0 GOWORK=off go build -trimpath ./...`

Run from `../cli`: `GOWORK=off make secure`

Expected: all three commands PASS.

**Step 5: Commit in the CLI module**

```bash
cd ../cli
git add tui/statusline.go tui/statusline_test.go tui/screen.go tui/screen_test.go
git commit -m "feat(tui): show conversation compaction progress"
```

### Task 10: Replay and bounded catalog aggregate

**Files:**

- Modify: `pkg/sessionstore/catalog.go`
- Modify: `pkg/sessionstore/catalog_test.go`
- Modify: `pkg/sessionstore/sessionstore.go`
- Modify: `pkg/sessionstore/sessionstore_test.go`
- Modify: `internal/sessionruntime/restore.go`
- Create: `internal/sessionruntime/hustle_restore_test.go`

**Step 1: Write failing replay/catalog tests**

Cover unmatched start ignored for execution, completed/failed usage aggregation,
named model rows, one zero/mixed current-loop row regardless of resolved model
cardinality/order, no started usage, checked addition overflow, duplicate EventID
dedup, and replay-order independence.

**Step 2: Run focused tests and confirm RED**

Run: `GOWORK=off go test -race ./pkg/sessionstore ./internal/sessionruntime -run 'Test.*Hustle.*(Catalog|Restore|Aggregate)'`

Expected: FAIL because the folds are absent.

**Step 3: Implement deterministic folds**

Key aggregates by `(name, model source, named model key, status)`. Store a fixed
runtime for named definitions and zero/mixed runtime for current-loop rows. Fold
only terminal event usage. Do not enumerate runs or reconstruct controller
state.

**Step 4: Run persistence suites**

Run: `GOWORK=off go test -race ./pkg/sessionstore ./internal/sessionruntime`

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/sessionstore internal/sessionruntime
git commit -m "feat(sessionstore): fold bounded hustle usage"
```

### Task 11: Full verification and documentation reconciliation

**Files:**

- Modify if required: `docs/plans/2026-07-11-hustle-mechanism-design.md`
- Modify if required: `docs/plans/2026-07-11-token-usage-context-occupancy-design.md`

**Step 1: Run formatting**

Run: `make fmt`

Expected: only implementation files from this plan change.

**Step 2: Run default and integration tests**

Run: `GOWORK=off go test -race ./...`

Run: `GOWORK=off go test -tags integration -race ./...`

Expected: PASS.

**Step 3: Run required fuzz targets**

Run: `GOWORK=off go test ./pkg/event -run '^$' -fuzz=FuzzHustleEvent -fuzztime=30s`

Run: `GOWORK=off go test ./pkg/loop -run '^$' -fuzz=FuzzCompactionWire -fuzztime=30s`

Expected: PASS with no crash.

**Step 4: Run build and security gates**

Run: `CGO_ENABLED=0 GOWORK=off go build -trimpath ./...`

Run: `GOWORK=off make secure`

Expected: PASS.

**Step 5: Review and commit reconciled design documentation**

Use `superpowers:requesting-code-review`, resolve every Critical/Important
finding, re-run Step 4, then:

```bash
git add docs/plans/2026-07-11-hustle-mechanism-design.md docs/plans/2026-07-11-token-usage-context-occupancy-design.md
git commit -m "docs: record hustle inference support"
```

Plan implementation is complete only when the worktree is clean and every
verification command above has fresh passing output.
