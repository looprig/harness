# Foreign-loop Module Extraction Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development`
> to execute this plan task-by-task in the isolated workspace. Every implementer
> must use `superpowers:test-driven-development`. Batch spec-compliance and
> code-quality reviews only at the review gates defined below.

**Goal:** Extract Harness's concrete foreign-loop backend and Claude/Codex process
drivers into `github.com/looprig/foreignloop`, leaving only the public builder and
restore seam in Harness and moving all real cross-module integration coverage to
`github.com/looprig/tests`.

**Architecture:** Work bottom-up across three repositories. First publish the
provider-neutral Harness seam and queue contract without removing the old package;
then build the new driver and backend packages against that seam; finally port the
public integration suite and delete the old concrete implementation. Preserve the
existing actor, event, transcript, locking, cancellation, and restore behavior,
with the single approved API refinement from `TranscriptPath()` to `History()`.

**Tech Stack:** Go 1.26.4, three git repositories, stdlib process/JSON/filesystem
APIs, `github.com/looprig/core`, public `github.com/looprig/harness` contracts,
vendoring, race tests, fuzz tests, `staticcheck`, `gosec`, and `govulncheck`.

**Design:** `docs/plans/2026-07-17-foreignloop-module-extraction-design.md`

---

## Execution protocol

### Isolated workspace

Use the existing ignored multi-repository worktree convention:

```text
/Users/ipotter/code/looprig/harness/.worktrees/foreignloop-extraction/
├── harness/      # branch feature/foreignloop-extraction
├── foreignloop/  # new repository, branch main
└── tests/        # branch test/foreignloop-extraction; create in Phase 4
```

The primary Harness and tests checkouts contain unrelated user work. Never stage,
modify, or clean those changes. All implementation commands below run inside the
isolated paths.

During development, `foreignloop/go.mod` and `tests/go.mod` may use local replaces
to sibling directories in this workspace. No release commit or tag may retain a
local replace. Release/tag operations are a manual authorization boundary and are
not performed by this plan.

### TDD and commits

For every behavior or boundary change:

1. Write the smallest test or dependency guard that expresses the new contract.
2. Run the focused command and observe the expected failure.
3. Implement the minimum change.
4. Run the focused race test and the affected package suite.
5. Commit only that task's files in the repository that owns them.

Pure file moves still need a red boundary test first: a compile test for the new
import path, a dependency guard, `root-check`, or a golden/parity assertion.

### Review cadence

Do not launch per-task reviewers. A fresh implementer subagent owns each task and
self-reviews its diff. After every phase, or after five tasks since the last gate
(whichever comes first), run exactly this batched review loop:

1. Dispatch one spec-compliance reviewer with the design, this plan, and all
   commits since the previous gate.
2. Fix every spec gap with the implementer responsible for the affected task;
   repeat the spec review until approved.
3. Dispatch one code-quality reviewer over the same commit range.
4. Fix every important issue and repeat quality review until approved.
5. Run the phase verification commands and record the reviewed commit range.

Never start code-quality review before spec review is approved. Never cross a
review gate with open findings.

### Cross-repository commit rule

Each repository gets its own focused commits. A task that changes two repositories
ends with two commits and records both SHAs in the phase notes. Never use a single
repository's passing tests as evidence for another repository.

---

## Ground truth to re-check before Task 1

- Harness currently owns `pkg/foreignloop`, including the backend, driver-neutral
  contracts, decoder, locks, snapshots, and `claude/` and `codex/` subpackages.
- Harness has no `pkg/foreign` directory yet.
- `internal/runtimecontract.ManagedInputQueueCapacity` is `64` and is consumed by
  native and foreign actors.
- `ForeignStream.TranscriptPath()` is called only by the concrete backend. The
  current transcript decoder ignores its `sinceTurn` parameter and reads the full
  file.
- `decodeTranscriptTail` already returns `[]content.AgenticMessages`; `commitTurn`
  publishes one `StepDone` per group and appends every group to the snapshot.
- `pkg/foreignloop/export.go` exists only to expose `DecodeStream` to the current
  nested Claude package.
- `github.com/looprig/tests` exists and is the only permitted owner of tests that
  import both Harness and the extracted module.
- The repository root `/Users/ipotter/code/looprig` is an umbrella directory, not a
  git repository. `foreignloop/` must be initialized as its own git repository.

If any item has drifted, stop and amend the design/plan before moving code.

---

## Phase 0 — Baseline and migration fixtures

### Task 1: Record clean baselines and the ownership manifest

**Files:**
- Create: `/private/tmp/foreignloop-extraction-baseline.txt` (not committed)
- Create: `docs/plans/2026-07-17-foreignloop-extraction-coverage.md`

**Step 1: Verify the isolated Harness checkout.**

Run:

```bash
git status --short
GOWORK=off go test -race ./pkg/foreignloop/... ./pkg/rig ./internal/sessionruntime
```

Expected: clean status and all focused tests pass. If the baseline is red, stop;
do not attribute a pre-existing failure to the extraction.

**Step 2: Write the coverage manifest.**

List every production/test file under `pkg/foreignloop`, its target owner
(`driver`, `driver/claude`, `driver/codex`, `backend`, `harness/pkg/foreign`,
`tests`, or delete), and all Harness files outside that tree importing
`pkg/foreignloop`. Include the eight old-to-new e2e test names from the design.

**Step 3: Add a checked completeness test.**

The committed check belongs in `pkg/rig/optional_dependencies_test.go`: locate the
module root, parse the manifest's source column, and fail when a tracked
`pkg/foreignloop/*.go` file is absent. Do not create a Go package under `docs/`.
Remove this manifest check in Task 18 when the old directory is deleted.

**Step 4: Verify and commit.**

```bash
go test -race ./pkg/rig -run 'Foreignloop|Optional'
git add docs/plans/2026-07-17-foreignloop-extraction-coverage.md \
  pkg/rig/optional_dependencies_test.go
git commit -m "test: inventory foreignloop extraction coverage"
```

---

## Phase 1 — Publish the Harness seam

### Task 2: Add `pkg/foreign` without changing runtime behavior

**Files:**
- Create: `pkg/foreign/builder.go`
- Create: `pkg/foreign/restored.go`
- Create: `pkg/foreign/builder_test.go`
- Modify: `pkg/foreignloop/foreignloop.go`
- Modify: `pkg/foreignloop/restored.go`

**Step 1: Write the failing seam compile test.**

The test constructs typed `foreign.Builder`, `foreign.RestoredBuilder`, and
`foreign.RestoredForeign` values and asserts that both nil builder zero values stay
nil and all restore fields retain their exact values.

Run:

```bash
go test ./pkg/foreign -run TestBuilderContracts
```

Expected: fail because `pkg/foreign` does not exist.

**Step 2: Move exactly the Harness-owned types.**

`builder.go` owns:

```go
type EventPublisher interface {
	PublishEvent(context.Context, event.Event) error
	PublishEventChecked(context.Context, event.Event) error
}

type Builder func(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
) (loop.Backend, string, error)
```

`restored.go` owns the unchanged `RestoredForeign` value and `RestoredBuilder`
signature. Keep temporary aliases in `pkg/foreignloop` so its existing backend and
tests remain green during overlap:

```go
type EventPublisher = foreign.EventPublisher
type Builder = foreign.Builder
type RestoredForeign = foreign.RestoredForeign
type RestoredBuilder = foreign.RestoredBuilder
```

**Step 3: Verify.**

```bash
go test -race ./pkg/foreign ./pkg/foreignloop/...
```

**Step 4: Commit.**

```bash
git add pkg/foreign pkg/foreignloop/foreignloop.go pkg/foreignloop/restored.go
git commit -m "feat(foreign): publish foreign backend builder seam"
```

### Task 3: Promote the managed-input capacity to `pkg/loop`

**Files:**
- Create: `pkg/loop/managed_queue.go`
- Create: `pkg/loop/managed_queue_test.go`
- Modify: `internal/loopruntime/loop.go`
- Modify: `internal/loopruntime/*_test.go`
- Modify: `pkg/foreignloop/turn.go`
- Modify: `pkg/foreignloop/turn_test.go`
- Delete: `internal/runtimecontract/managed_queue.go`
- Delete: `internal/runtimecontract/managed_queue_test.go`

**Step 1: Write the failing public-contract test.**

```go
func TestManagedInputQueueCapacityCompatibility(t *testing.T) {
	if ManagedInputQueueCapacity != 64 {
		t.Fatalf("ManagedInputQueueCapacity = %d, want 64", ManagedInputQueueCapacity)
	}
}
```

Run `go test ./pkg/loop -run ManagedInputQueueCapacity`; expect undefined symbol.

**Step 2: Add the public constant and migrate every caller.**

```go
const ManagedInputQueueCapacity = 64
```

Replace `runtimecontract.ManagedInputQueueCapacity` with
`loop.ManagedInputQueueCapacity`. Delete the internal constant only after:

```bash
rg -n 'runtimecontract.ManagedInputQueueCapacity|internal/runtimecontract' \
  internal/loopruntime pkg/foreignloop
```

returns no capacity caller.

**Step 3: Verify the rejection boundary.**

```bash
go test -race ./pkg/loop ./internal/loopruntime ./pkg/foreignloop \
  -run 'Managed|Queue|Capacity|SixtyFive'
```

Expected: native and foreign tests still reject item 65 before acceptance.

**Step 4: Commit.**

```bash
git add pkg/loop internal/loopruntime internal/runtimecontract pkg/foreignloop
git commit -m "refactor(loop): publish managed input queue capacity"
```

### Task 4: Repoint Harness composition at `pkg/foreign`

**Files:**
- Modify: `pkg/rig/options.go`
- Modify: `pkg/rig/rig_test.go`
- Modify: `internal/sessionruntime/command_journal.go`
- Modify: `internal/sessionruntime/lifecycle.go`
- Modify: `internal/sessionruntime/session.go`
- Modify: `internal/sessionruntime/restore_constructor.go`
- Modify: `internal/sessionruntime/foreign_newloop_test.go`
- Modify: `internal/sessionruntime/foreign_restore_test.go`
- Modify: other seam-only tests listed by the coverage manifest

**Step 1: Make the import-boundary test fail.**

Extend `pkg/rig/optional_dependencies_test.go` to reject production imports of
`github.com/looprig/harness/pkg/foreignloop` outside that package. Run the focused
test and observe existing rig/session imports fail.

**Step 2: Repoint only seam consumers.**

Change imports and type names from `foreignloop.{Builder,RestoredBuilder,
RestoredForeign}` to `foreign.{Builder,RestoredBuilder,RestoredForeign}`. Do not
move the concrete e2e tests yet; they remain as overlap evidence until Phase 4.

**Step 3: Verify Harness construction/restore behavior.**

```bash
go test -race ./pkg/rig ./internal/sessionruntime \
  -run 'Foreign|Builder|Restore|Journal'
```

**Step 4: Commit.**

```bash
git add pkg/rig internal/sessionruntime
git commit -m "refactor(harness): consume public foreign builder seam"
```

### Task 5: Lock the Harness dependency direction

**Files:**
- Create: `pkg/foreign/deps_test.go`
- Modify: `pkg/rig/optional_dependencies_test.go`

**Step 1: Add failing guards.**

Using `go list -deps -json` plus Go's parser, assert:

- Harness has no import whose path is `github.com/looprig/foreignloop` or starts
  with that prefix.
- `pkg/foreign` imports only stdlib, Core values, and public Harness contracts.
- no Harness production package except the temporary `pkg/foreignloop` imports
  `pkg/foreignloop`.

Seed the parser test with a synthetic forbidden import to prove it fails for the
right reason, then remove the seed.

**Step 2: Verify.**

```bash
go test -race ./pkg/foreign ./pkg/rig -run 'Depend|Optional|Import'
go test -race ./pkg/foreignloop/... ./internal/loopruntime ./internal/sessionruntime
```

**Step 3: Commit.**

```bash
git add pkg/foreign/deps_test.go pkg/rig/optional_dependencies_test.go
git commit -m "test(harness): enforce foreign module dependency direction"
```

### Review gate 1

Review Tasks 1-5 as one batch. Verify:

```bash
GOWORK=off go test -race ./pkg/foreign ./pkg/loop ./pkg/foreignloop/... \
  ./internal/loopruntime ./internal/sessionruntime ./pkg/rig
git status --short
```

Expected: tests pass and the Harness worktree is clean.

---

## Phase 2 — Create the external module and drivers

### Task 6: Scaffold `github.com/looprig/foreignloop`

**Files in the new repository:**
- Create: `.gitignore`
- Create: `AGENTS.md`
- Create: `CLAUDE.md`
- Create: `Makefile`
- Create: `go.mod`, `go.sum`
- Create: `internal/boundary/doc.go`
- Create: `scripts/check-root.sh` only if Make syntax alone is insufficient
- Create: `vendor/`

**Step 1: Initialize the repository and write a failing root check.**

Initialize `foreignloop/` as `github.com/looprig/foreignloop`. The `root-check`
target must fail when a temporary root `forbidden.go` exists and pass after it is
removed. Do not commit any root Go file.

**Step 2: Add focused policy and secure tooling.**

`CLAUDE.md` includes only applicable process, path, interface, validation,
dependency, test, vendoring, and secure-build rules. Approved dependencies are
Core, Harness, and the three dev tools. `AGENTS.md` points to it.

The Makefile provides `build`, `test`, `fmt`, `fmt-check`, `root-check`, `vendor`,
`vendor-check`, `lint`, `vuln`, `secure`, and `fuzz`. `lint` runs `go vet`,
`staticcheck`, and `gosec` over `GO_DIRS` derived from `go list`.

**Step 3: Wire the temporary development dependency.**

Require the intended Harness release version and temporarily replace it with
`../harness`. Require Core at its current tag. Vendor dependencies and scrub only
declared local `.git` metadata.

**Step 4: Verify and commit.**

```bash
make root-check
CGO_ENABLED=0 go build -trimpath ./...
make secure
git add -A
git commit -m "build: scaffold foreignloop module"
```

### Task 7: Define provider-neutral driver contracts

**Files:**
- Create: `driver/driver.go`
- Create: `driver/history.go`
- Create: `driver/errors.go`
- Create: `driver/driver_test.go`
- Create: `driver/deps_test.go`

**Step 1: Write the failing contract tests.**

Tests assert zero values and the exact public vocabulary `Agent`, `Turn`, `Stream`,
`Event`, `Kind`, `PermissionPosture`, and `History`. `Stream` must compile only with:

```go
type Stream interface {
	Events() <-chan Event
	History() (History, error)
	Close() error
}
```

No path or provider wire type may appear in the package API.

**Step 2: Port/rename the existing neutral values.**

Map `ForeignAgent` -> `Agent`, `ForeignTurn` -> `Turn`, `ForeignEvent` -> `Event`,
and `ForeignKind` -> `Kind`. Keep enum order and field semantics unchanged.

```go
type History struct {
	Available bool
	Steps     []content.AgenticMessages
}
```

Move spawn, exit, result, protocol, decode, and history error types to `driver`;
backend configuration, lock, and snapshot errors do not belong here.

**Step 3: Add dependency guards.**

Fail if `driver` imports `backend`, `harness/pkg/event`, any Harness session
package, or `harness/internal`. Allow Core content and stdlib only.

**Step 4: Verify and commit.**

```bash
go test -race ./driver
git add driver
git commit -m "feat(driver): define foreign agent contracts"
```

### Task 8: Move Claude decoding and prove history parity

**Files in Harness (temporary migration evidence):**
- Modify: `pkg/foreignloop/decode_transcript_test.go`
- Create: `pkg/foreignloop/migration_export.go` with `//go:build migration`
- Create: `pkg/foreignloop/testdata/transcript/*.golden.json`

**Files in foreignloop:**
- Create: `driver/claude/decode_stream.go`
- Create: `driver/claude/decode_transcript.go`
- Create: `driver/claude/history.go`
- Create: `driver/claude/decode_stream_test.go`
- Create: `driver/claude/decode_transcript_test.go`
- Create: `driver/claude/decode_fuzz_test.go`
- Copy: `driver/claude/testdata/{stream,transcript}/...`
- Create: `driver/claude/migration_export.go` with `//go:build migration`

**Step 1: Freeze the old golden projection.**

For each existing transcript fixture, commit canonical JSON containing:

- grouped `[]content.AgenticMessages`;
- ordered `StepDone.Messages` bodies;
- flattened committed snapshot; and
- error classification for malformed, truncated, missing, and empty inputs.

Run the old test once with an intentionally changed golden and observe failure,
then restore the generated golden.

**Step 2: Port the decoders into `driver/claude`.**

Keep JSON structures private. `History()` derives and reads the Claude path after
`Close()`, returns the entire decoded history, and wraps failures in a typed driver
history error. It has no `sinceTurn` parameter.

**Step 3: Add migration-tag parity exports and comparison.**

The migration-only exports expose the two private decoder functions only under
`//go:build migration`. They must be ordinary `.go` files (not `_test.go`) so the
tests module can import them under that build tag. A temporary comparison in the
tests module (Task 15) will
run old and new functions on identical bytes. These exports are deleted in Task 18.

**Step 4: Verify and commit in each changed repository.**

```bash
# Harness
go test -race ./pkg/foreignloop -run 'Transcript|Golden'

# foreignloop
go test -race ./driver/claude -run 'Decode|History|Golden'
go test ./driver/claude -run '^$' -fuzz FuzzDecode -fuzztime=10s
```

Commit Harness golden evidence separately from the external decoder move.

### Task 9: Move the Claude process driver and agent constructor

**Files:**
- Create/port: `driver/claude/args.go`, `args_test.go`
- Create/port: `driver/claude/env.go`, `env_test.go`
- Create/port: `driver/claude/claude.go`, `claude_test.go`
- Create/port: `driver/claude/transcript.go`, `transcript_test.go`
- Create/port: `driver/claude/wrap_test.go`
- Create/port: `driver/claude/claude_integration_test.go`
- Create: `driver/claude/config.go`, `config_test.go`, `doc.go`

**Step 1: Replace the old constructor test with a failing agent-only test.**

`NewAgent(parentEnv, Config)` returns `driver.Agent`, not a backend `Spec`. Test
required executable/workspace fields, copied environment ownership, credential
whitelisting, posture mapping, and typed config errors.

**Step 2: Port process behavior unchanged.**

Preserve argument order, session start/resume selection, process groups, wrapping,
context cancellation, close/drain order, environment whitelist, and the justified
`#nosec` annotation for an operator-selected executable.

**Step 3: Verify.**

```bash
go test -race ./driver/claude
go test -tags integration -race ./driver/claude
```

If the real CLI is unavailable, the integration suite must skip explicitly; fake
process-boundary tests still run and pass.

**Step 4: Commit.**

```bash
git add driver/claude
git commit -m "feat(claude): move process driver into foreignloop"
```

### Task 10: Move the Codex driver and return unavailable history

**Files:**
- Create/port: `driver/codex/args.go`, `args_test.go`
- Create/port: `driver/codex/env.go`, `env_test.go`
- Create/port: `driver/codex/decode.go`, `decode_test.go`, `decode_fuzz_test.go`
- Create/port: `driver/codex/codex.go`, `codex_test.go`
- Create/port: `driver/codex/codex_integration_test.go`
- Create: `driver/codex/config.go`, `config_test.go`, `doc.go`

**Step 1: Write failing agent/history tests.**

`NewAgent(parentEnv, Config)` returns `driver.Agent`. A spawned Codex stream returns
`driver.History{Available:false}` with no error after close. No transcript path is
exposed.

**Step 2: Port behavior unchanged.**

Preserve approval/sandbox enum validation, start/resume argv, resume-flag probing,
JSONL decoding, event order, terminal and joined close errors, cancellation, and
environment whitelisting.

**Step 3: Verify and commit.**

```bash
go test -race ./driver/codex
go test ./driver/codex -run '^$' -fuzz FuzzDecodeLine -fuzztime=10s
go test -tags integration -race ./driver/codex
git add driver/codex
git commit -m "feat(codex): move process driver into foreignloop"
```

### Review gate 2

Review Tasks 6-10. Verify:

```bash
make root-check
CGO_ENABLED=0 go build -trimpath ./...
go test -race ./driver/...
make secure
git status --short
```

The foreignloop repository must have no root Go files and no driver import of
backend, Harness event/session, or Harness internals.

---

## Phase 3 — Move the concrete backend

### Task 11: Add backend configuration, builders, and typed errors

**Files:**
- Create: `backend/config.go`, `config_test.go`
- Create: `backend/errors.go`, `errors_test.go`
- Create: `backend/builder.go`, `builder_test.go`
- Create: `backend/restored.go`, `restored_test.go`

**Step 1: Write failing public API tests.**

The exported API is:

```go
type Config struct {
	Agent   driver.Agent
	Cwd     string
	Posture driver.PermissionPosture
	SIDMode SIDMode
}

func BuildWith(cfg Config) foreign.Builder
func BuildRestoredWith(cfg Config) foreign.RestoredBuilder
```

Test nil agent, empty workspace, unknown posture/SID mode, eager typed validation,
and fail-closed builder invocation. Assert `Config` has no executable or environment
field.

**Step 2: Move backend-owned types.**

Move `SIDMode`, `ConfigError`, `ForeignSessionBusyError`, lock errors, and snapshot
errors into `backend`. Keep driver-originated error identity via `errors.Is/As`.

**Step 3: Implement adapters to the Harness seam.**

Port `BuildWith` and `BuildRestoredWith` to close over `backend.Config`. Preserve
the nil-interface-on-error guarantee.

**Step 4: Verify and commit.**

```bash
go test -race ./backend -run 'Config|Build|Restore|Error'
git add backend
git commit -m "feat(backend): add foreign loop composition API"
```

### Task 12: Move mapper, locks, snapshots, and restore construction

**Files:**
- Create/port: `backend/mapper.go`, `mapper_test.go`
- Create/port: `backend/lock.go`, `lock_test.go`
- Create/port: `backend/snapshot.go`
- Modify: `backend/restored.go`, `restored_test.go`
- Create: `backend/fake_test.go`

**Step 1: Port tests first and observe missing symbols.**

Copy the existing mapper, lock, snapshot, and restore tests, rewrite only package
names/types, and run them before copying production code.

**Step 2: Port production code mechanically.**

Preserve event mapping/correlation, lock path derivation, temporary late-bound lock
namespace, stale-PID behavior, idempotent release, defensive snapshot cloning, and
restored `hasSpawned/sidBound/turnIndex/messages` state.

**Step 3: Verify and commit.**

```bash
go test -race ./backend -run 'Mapper|Lock|Snapshot|Restore'
git add backend
git commit -m "feat(backend): move mapping locking and restore state"
```

### Task 13: Move the actor and authoritative-history commit path

**Files:**
- Create/port: `backend/loop.go`, `loop_test.go`
- Create/port: `backend/turn.go`, `turn_test.go`
- Create/port: `backend/header.go`
- Modify: `backend/fake_test.go`

**Step 1: Port actor tests before production code.**

Rewrite fake streams to implement `History()`. Keep tests for queue FIFO/capacity,
cancel, interrupt, shutdown, late binding, lock transitions, spawn/protocol/close
errors, event ordering, and transcript loss fallback.

Add explicit tests that:

- `Close()` happens before `History()`;
- available history emits exactly one `StepDone` per group and commits all groups;
- unavailable history silently uses complete streamed assistant messages;
- typed history failure uses the same fallback and warning/event behavior as the old
  transcript failure; and
- history grouping/order matches the committed Claude goldens.

Run focused tests and observe missing actor/history implementation.

**Step 2: Port the actor with the approved seam refinement.**

Replace the old call:

```go
commitTurn(stream.TranscriptPath(), cur, drained.assistant, pub)
```

with a post-close `stream.History()` call. Do not introduce a turn offset or change
fallback/event/snapshot behavior. Use `loop.ManagedInputQueueCapacity`.

**Step 3: Verify and commit.**

```bash
go test -race ./backend -run 'Turn|History|Transcript|Queue|Interrupt|Shutdown|LateBound'
git add backend
git commit -m "feat(backend): move foreign actor with neutral history"
```

### Task 14: Complete backend parity and module dependency guards

**Files:**
- Move/port remaining backend tests from the coverage manifest
- Create: `backend/deps_test.go`
- Create: `internal/boundary/deps_test.go`
- Modify: `Makefile`

**Step 1: Add guards and prove their failure mode.**

Reject:

- any `github.com/looprig/harness/internal/...` import;
- driver imports of backend or Harness event/session packages;
- backend imports of provider packages or transcript wire decoders; and
- root-level `*.go` files.

Use a synthetic import/root file in the test setup to demonstrate each guard fails,
then remove it.

**Step 2: Port all unclassified tests.**

The coverage manifest must show every old backend test as moved, retained in
Harness, assigned to tests, or intentionally deleted with design rationale. No
behavior test disappears merely because its package moved.

**Step 3: Run the external module gate and commit.**

```bash
CGO_ENABLED=0 go build -trimpath ./...
go test -race ./...
go test -tags integration -race ./...
make secure
git add -A
git commit -m "test: prove extracted backend parity and boundaries"
```

### Review gate 3

Review Tasks 11-14. In addition to the external checks, compare public symbol sets
and error classifications against the coverage manifest. No backend behavior may
remain dependent on a Claude path.

---

## Phase 4 — Cross-module tests and Harness removal

### Task 15: Add the tests-module worktree and development/release wiring

**Files in `github.com/looprig/tests`:**
- Modify: `go.mod`, `go.sum`
- Modify: `Makefile`
- Create: `foreignloop_migration_test.go` with `//go:build migration`

**Step 1: Create the tests worktree in the shared feature directory.**

Branch from tests `main` as `test/foreignloop-extraction`. Confirm its baseline
with `make check` before modifying module metadata.

**Step 2: Add local development wiring.**

Require `github.com/looprig/foreignloop v0.0.0` and replace it with
`../foreignloop`; point Harness at `../harness`. Add a release verification target
that consumes `go.release.mod` once real tags exist. Do not invent an unpublished
version or create unverifiable release sums during development.

**Step 3: Add temporary decoder parity.**

Under the `migration` tag, import the old Harness package and new Claude driver
test exports, run both over identical fixtures, and compare complete Go values and
canonical JSON. Observe a deliberate mismatch fail, then restore parity.

**Step 4: Verify and commit.**

```bash
GOWORK=off go test -tags 'integration migration' -race ./... \
  -run 'Foreignloop.*Parity'
git add go.mod go.sum Makefile foreignloop_migration_test.go
git commit -m "test: wire foreignloop migration parity suite"
```

The release-mode target must report that `go.release.mod` is not prepared until
Task 21 supplies real tags; it must never silently use local source.

### Task 16: Port the primary, Codex, and subagent integration scenarios

**Files:**
- Create: `foreignloop_integration_test.go`
- Create: `foreignloop_fixtures_test.go`
- Modify: `fixtures_test.go` only for shared public helpers

**Step 1: Port four scenarios through public APIs.**

Implement and initially fail these mappings:

- `TestForeignPrimaryE2E` -> `TestForeignloopPrimary`
- `TestCodexForeignPrimaryLateBoundPublishesBoundAndTurnDone` ->
  `TestForeignloopCodexPrimaryLateBound`
- `TestForeignSubagentE2E` -> `TestForeignloopSubagent`
- `TestCodexForeignSubagentLateBoundReturnsFinalText` ->
  `TestForeignloopCodexSubagentLateBound`

Construct agents with `claude.NewAgent`/`codex.NewAgent`, compose
`backend.Config`, and wire `backend.BuildWith`/`BuildRestoredWith` into the public
Harness rig/session APIs. Do not import Harness internals.

**Step 2: Verify and commit.**

```bash
GOWORK=off go test -tags integration -race ./... \
  -run 'TestForeignloop(Primary|CodexPrimaryLateBound|Subagent|CodexSubagentLateBound)'
git add foreignloop_integration_test.go foreignloop_fixtures_test.go fixtures_test.go
git commit -m "test: port foreignloop primary and subagent integration"
```

### Task 17: Port queue, failure, and quota integration scenarios

**Files:**
- Modify: `foreignloop_integration_test.go`
- Modify: `foreignloop_fixtures_test.go`

**Step 1: Port the remaining four mappings.**

- `TestForeignQueuedDelegateInterruptResolvesWithoutWaitTimeout` ->
  `TestForeignloopQueuedDelegateInterrupt`
- `TestForeignQueuedDelegateTimeoutCancelsOnlyThatRequest` ->
  `TestForeignloopQueuedDelegateTimeout`
- `TestForeignProviderFailureResolvesQueuedDelegatesFailedLive` ->
  `TestForeignloopProviderFailureWithQueuedDelegates`
- `TestForeignSubagentQuotaCap` -> `TestForeignloopSubagentQuota`

Use bounded contexts and public observation APIs. Preserve exact terminal status,
event order, cancellation scope, and quota assertions.

**Step 2: Verify all eight and commit.**

```bash
GOWORK=off go test -tags integration -race ./... -run TestForeignloop
git add foreignloop_integration_test.go foreignloop_fixtures_test.go
git commit -m "test: port foreignloop queue failure and quota integration"
```

### Task 18: Remove Harness's concrete package after parity passes

**Files in Harness:**
- Modify: `internal/sessionruntime/foreign_e2e_test.go`
- Modify: `internal/sessionruntime/loop_tools_test.go`
- Modify: every remaining importer listed in the coverage manifest
- Delete: `pkg/foreignloop/**`
- Modify: `docs/plans/2026-07-17-foreignloop-extraction-coverage.md`
- Modify: `pkg/rig/optional_dependencies_test.go`

**Step 1: Strengthen the guard before deletion.**

Make the Harness boundary test reject every production or test import of
`harness/pkg/foreignloop`. Run it and observe the remaining tests fail.

**Step 2: Replace only Harness-owned behavior with seam fakes.**

Keep builder selection, missing-builder, restore folding, engine policy, and
`TestReplaceExternalToolsRefusedOnForeignLoop` in Harness using fake
`foreign.Builder`/`RestoredBuilder` values. The eight real backend/session tests now
belong only to tests and are deleted from Harness after their public replacements
pass.

**Step 3: Delete the old implementation and migration exports.**

Delete all of `pkg/foreignloop`, including root `export.go`. Delete temporary
`migration` exports/tests in both Harness and foreignloop once Task 15 parity and
Tasks 16-17 integrations pass. Update the coverage manifest to a final checked
mapping.

**Step 4: Prove Harness is independent.**

```bash
! rg -n 'harness/pkg/foreignloop|github.com/looprig/foreignloop' \
  --glob '*.go' --glob 'go.mod' --glob 'go.sum'
GOWORK=off go test -race ./...
CGO_ENABLED=0 GOWORK=off go build -trimpath ./...
```

**Step 5: Commit in Harness, then foreignloop if migration files changed.**

```bash
git add -A
git commit -m "refactor: remove concrete foreign loop from harness"
```

### Task 19: Finalize cross-module guards and consumer migration examples

**Files:**
- Modify: `tests/Makefile`
- Create: `tests/dependency_boundary_test.go`
- Create: `foreignloop/backend/example_test.go`
- Modify: driver package docs
- Modify: Harness and foreignloop READMEs if present

**Step 1: Add checked cross-module ownership guards.**

The tests module enumerates sibling module manifests and fails if a module other
than tests imports both Harness and foreignloop as integration subjects. The
foreignloop guard rejects Harness internals. Harness guard rejects foreignloop.

**Step 2: Add a compiling composition example.**

Show:

```go
agent, err := claude.NewAgent(parentEnv, claude.Config{/* required fields */})
if err != nil {
	return err
}
cfg := backend.Config{
	Agent: agent, Cwd: workspace,
	Posture: driver.PostureAcceptEdits,
	SIDMode: backend.SIDPrebound,
}
rig.WithForeignBuilders(backend.BuildWith(cfg), backend.BuildRestoredWith(cfg))
```

The example must compile without obsolete `Spec`, `NewSpec`, `TranscriptPath`, or
`DecodeStream` names.

**Step 3: Verify and commit per repository.**

```bash
# tests
make check

# foreignloop
go test -race ./...

# Harness
go test -race ./pkg/foreign ./pkg/rig ./internal/sessionruntime
```

### Review gate 4

Review Tasks 15-19. This is both the phase boundary and the five-task boundary.
Review all three repository ranges together for spec compliance, then code quality.
The old Harness directory may be removed only if every mapped public integration
test passes.

---

## Phase 5 — Full verification and release readiness

### Task 20: Run the complete verification matrix

**Files:**
- Create: `/private/tmp/foreignloop-extraction-verification.txt` (not committed)
- Update docs only if commands or supported platforms differ from the design

**Step 1: Harness.**

```bash
GOWORK=off go test -race ./...
GOWORK=off go test -tags integration -race ./...
CGO_ENABLED=0 GOWORK=off go build -trimpath ./...
make secure
```

**Step 2: foreignloop.**

```bash
make root-check
make build
make test
go test -tags integration -race ./...
go test ./driver/claude -run '^$' -fuzz FuzzDecode -fuzztime=30s
go test ./driver/codex -run '^$' -fuzz FuzzDecodeLine -fuzztime=30s
make secure
```

**Step 3: tests.**

```bash
make check
```

Run the release-modfile target only when the required Harness and foreignloop tags
exist. Record it as blocked by unpublished tags rather than substituting local
replaces.

**Step 4: Verify repository state.**

Each worktree must be clean. `git diff --check` and `git status --short` must show
no uncommitted implementation changes.

### Task 21: Prepare the release handoff without tagging

**Files:**
- Create: `foreignloop/RELEASE.md` or update an existing release document
- Create: `tests/go.release.mod`, `tests/go.release.sum` only after real tags exist

**Step 1: Document the ordered release commands.**

1. Tag/push the Harness release containing `pkg/foreign` and no concrete backend.
2. Replace foreignloop's temporary Harness replace with that tag, tidy/vendor,
   re-run build/race/integration/secure, then tag/push foreignloop.
3. Pin both real tags in `tests/go.release.mod`, run the release suite with no local
   replaces, and commit the sums.
4. Migrate product composition roots to `backend.Config` and agent constructors.

**Step 2: Add a no-local-replace release check.**

The release Make target fails if `go.mod` or `go.release.mod` contains a local
Harness/foreignloop replace. Test it against a temporary modfile containing a local
replace, then against the intended release files.

**Step 3: Stop at the authorization boundary.**

Do not create or push tags, remove temporary replaces from a build that still needs
them, or modify product repositories without explicit user authorization.

### Review gate 5

Run one final whole-implementation spec review followed by one final code-quality
review across all repositories. Re-run Task 20 after every fix. Then use
`superpowers:finishing-a-development-branch` to present merge/PR/cleanup options;
do not merge or delete worktrees automatically.

---

## Completion checklist

- Harness exposes only `pkg/foreign`; no `pkg/foreignloop` or external module
  dependency remains.
- `loop.ManagedInputQueueCapacity == 64` drives both native and foreign queues.
- foreignloop has no root Go files and no Harness-internal imports.
- drivers expose `History()`, not a transcript path; Claude goldens and migration
  parity prove grouping/event/snapshot equivalence.
- Codex returns unavailable history without error and preserves live fallback.
- backend builders accept `backend.Config`; obsolete `Spec`, `NewSpec`, and
  `DecodeStream` APIs are gone.
- all eight real Harness/backend scenarios run through public APIs in tests.
- race, integration, fuzz, build, `gosec`, `staticcheck`, and `govulncheck` gates
  pass in their owning modules.
- release files contain no local replaces once real tags are created.
- all review gates are approved with no open spec or quality findings.
