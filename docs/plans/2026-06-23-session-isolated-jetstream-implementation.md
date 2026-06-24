# Session-Isolated Embedded JetStream Implementation Plan

> **Status:** Implemented (2026-06-23) on `feature/session-isolated-jetstream` in
> both `looprig` and `../swe`. All ten tasks landed; full verification
> (`go test -race ./...`, `-tags integration -race ./...`, `build -trimpath`,
> `make fmt-check`, and `make secure` in looprig) passes in each repository.
>
> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Give each local SWE session an isolated embedded JetStream StoreDir, fast filesystem-based list/resume metadata, optional low-cost generated titles, and an explicit legacy-store purge command.

**Architecture:** `looprig/pkg/persistence` will own typed, path-confined session storage, manifests, Unix file locks, and session-engine lifecycle. `../swe` will replace its process-global journal context with a per-session factory: each new or resumed session opens one isolated engine, and the agent’s teardown closes it. Filesystem manifests replace the shared JetStream catalog for listing; the journal remains authoritative for runtime events and restore.

**Tech Stack:** Go standard library (`os`, `filepath`, `encoding/json`, `syscall` on Unix), embedded NATS JetStream, existing `llm`/`auto` packages, Bubble Tea v2, Go race detector and integration-test tags.

---

## Preconditions and Working Rules

- Work in the isolated `looprig` worktree at `.worktrees/session-isolated-jetstream` and create a matched feature worktree/branch in `../swe` before modifying SWE files.
- Run Go commands with `GOWORK=off`, because the parent `/Users/ipotter/code/go.work` does not include this worktree.
- Use TDD for every production change: add a table-driven test, run it and observe the expected failure, implement the minimum behavior, then rerun the test under `-race`.
- Do not add dependencies. Unix locking uses build-tagged `syscall.Flock`; unsupported platforms fail with a typed error.
- Never run a destructive command against the real user data root in tests. Purge tests use `t.TempDir()` under a test XDG root.
- Preserve the user’s unrelated `go.mod`, `go.sum`, and `vendor` changes in the original workspace.

### Task 1: Establish the session-directory domain in `looprig`

**Files:**
- Create: `pkg/persistence/session_store.go`
- Create: `pkg/persistence/session_store_test.go`
- Modify: `pkg/persistence/embedded.go`
- Modify: `pkg/persistence/embedded_test.go`

**Step 1: Write failing table-driven unit tests**

Cover `SessionStoreRoot` construction and path resolution for XDG, home fallback, a canonical UUID, zero/invalid IDs, traversal attempts, root escape, a symlinked session directory, and owner-only directory modes. Define the desired public surface first:

```go
type SessionStoreRoot struct { /* unexported fields */ }

func OpenSessionStoreRoot() (*SessionStoreRoot, error)
func (r *SessionStoreRoot) SessionDir(id uuid.UUID) (string, error)
func (r *SessionStoreRoot) CreateSessionDir(id uuid.UUID) (string, error)
```

**Step 2: Verify RED**

Run: `GOWORK=off go test -race ./pkg/persistence -run 'TestSessionStoreRoot|TestSessionDir'`

Expected: compilation failure because the new store-root API does not exist.

**Step 3: Implement the minimal confined path API**

Use the existing XDG/home resolution convention, `filepath.Clean`, `filepath.Rel`, `os.Mkdir`/`os.MkdirAll` with `0700`, and `os.Lstat` to reject symlinks at every path component controlled by the feature. Return typed errors containing operation and safe path context.

**Step 4: Verify GREEN and format**

Run: `gofmt -w pkg/persistence/session_store.go pkg/persistence/session_store_test.go`

Run: `GOWORK=off go test -race ./pkg/persistence -run 'TestSessionStoreRoot|TestSessionDir'`

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/persistence/session_store.go pkg/persistence/session_store_test.go pkg/persistence/embedded.go pkg/persistence/embedded_test.go
git commit -m "feat(persistence): add confined session store root"
```

### Task 2: Add lock ownership and one-engine-per-session lifecycle

**Files:**
- Create: `pkg/persistence/lock_unix.go`
- Create: `pkg/persistence/lock_unsupported.go`
- Create: `pkg/persistence/session_engine.go`
- Create: `pkg/persistence/session_engine_test.go`
- Create: `pkg/persistence/session_engine_integration_test.go`
- Modify: `pkg/persistence/embedded.go`

**Step 1: Write failing tests for ownership behavior**

Use a unique temporary session directory per case. Test a first open, a second open returning `*SessionLockedError` before server startup, close releasing the lock even if drain returns an error, unsupported platform behavior through a platform-local test seam, and two distinct IDs opening independently.

**Step 2: Verify RED**

Run: `GOWORK=off go test -race ./pkg/persistence -run TestSessionEngine`

Expected: compilation failure for `OpenSessionEngine` and `SessionLockedError`.

**Step 3: Implement narrow lock and engine abstractions**

Keep the responsibilities separate:

```go
type SessionEngine struct {
	engine *Engine
	lock   sessionLock
}

func (r *SessionStoreRoot) OpenSessionEngine(id uuid.UUID) (*SessionEngine, error)
func (e *SessionEngine) JetStream() nats.JetStreamContext
func (e *SessionEngine) Close() error
```

Acquire `<session-dir>/session.lock` before `Engine.Open`, configure its `DataDir` as `<session-dir>/nats`, and close the lock on every failure and every `Close` path. Keep `Engine` general; do not make it know session IDs.

**Step 4: Verify GREEN, including process-boundary coverage**

Run: `GOWORK=off go test -race ./pkg/persistence -run TestSessionEngine`

Run: `GOWORK=off go test -tags integration -race ./pkg/persistence -run TestSessionEngine`

Expected: PASS, including lock release after close.

**Step 5: Commit**

```bash
git add pkg/persistence/lock_unix.go pkg/persistence/lock_unsupported.go pkg/persistence/session_engine.go pkg/persistence/session_engine_test.go pkg/persistence/session_engine_integration_test.go pkg/persistence/embedded.go
git commit -m "feat(persistence): isolate embedded engines by session"
```

### Task 3: Implement atomic, non-secret session metadata

**Files:**
- Create: `pkg/persistence/session_meta.go`
- Create: `pkg/persistence/session_meta_test.go`
- Create: `pkg/persistence/session_meta_integration_test.go`
- Modify: `pkg/persistence/session_store.go`

**Step 1: Write failing table-driven tests**

Define typed manifest fields and a serialized writer. Test initial `none` title, generated and first-user-message title sources, illegal title/control-character rejection, bounded title truncation, atomic update preservation under concurrent callers, missing/corrupt metadata list entries, and no API-key field in JSON.

```go
type TitleSource string
const (
	TitleSourceNone TitleSource = "none"
	TitleSourceGenerated TitleSource = "generated"
	TitleSourceFirstUserMessage TitleSource = "first_user_message"
)

type SessionMeta struct { /* ID, title, timestamps, status only */ }
```

**Step 2: Verify RED**

Run: `GOWORK=off go test -race ./pkg/persistence -run 'TestSessionMeta|TestListSessionMeta'`

Expected: compilation failure for the manifest API.

**Step 3: Implement the manifest repository**

Use a mutex-private writer, JSON encode into a `0600` temporary file in the same directory, `Sync`, atomic rename, and parent-directory `Sync`. Initial create/restore writes return typed errors; later refresh errors return to the caller for warning-level handling. Never serialize a model spec, API key, request, response, or transcript.

**Step 4: Verify GREEN**

Run: `GOWORK=off go test -race ./pkg/persistence -run 'TestSessionMeta|TestListSessionMeta'`

Run: `GOWORK=off go test -tags integration -race ./pkg/persistence -run TestSessionMeta`

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/persistence/session_meta.go pkg/persistence/session_meta_test.go pkg/persistence/session_meta_integration_test.go pkg/persistence/session_store.go
git commit -m "feat(persistence): add atomic session manifests"
```

### Task 4: Add explicit safe purge of the legacy shared StoreDir

**Files:**
- Modify: `pkg/persistence/session_store.go`
- Modify: `pkg/persistence/session_store_test.go`

**Step 1: Write failing purge tests**

Using only `t.TempDir()` plus `XDG_DATA_HOME`, test: legacy directory with files is removed, absent legacy directory is a no-op, a symlink is rejected, the sessions root and log sibling survive, and an escaping configured path is refused.

**Step 2: Verify RED**

Run: `GOWORK=off go test -race ./pkg/persistence -run TestPurgeLegacy`

Expected: compilation failure for `PurgeLegacyStore`.

**Step 3: Implement confined purge**

Implement a root method that derives the exact former `looprig/jetstream` path internally, verifies containment and non-symlink status, and removes only that directory. Return a typed result/error suitable for CLI output; never accept an arbitrary deletion path.

**Step 4: Verify GREEN**

Run: `GOWORK=off go test -race ./pkg/persistence -run TestPurgeLegacy`

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/persistence/session_store.go pkg/persistence/session_store_test.go
git commit -m "feat(persistence): add explicit legacy store purge"
```

### Task 5: Create the matching SWE worktree and replace global persistence composition

**Repositories and files:**
- Create a sibling `../swe` feature worktree on `feature/session-isolated-jetstream`.
- Modify: `../swe/cmd/swe/main.go`
- Modify: `../swe/cmd/swe/main_test.go`
- Modify: `../swe/swarms/swe/persistence.go`
- Modify: `../swe/swarms/swe/persistence_integration_test.go`
- Modify: `../swe/swarms/swe/agent.go`

**Step 1: Write failing SWE composition tests**

Test that a new open mints an ID before engine construction, uses that ID’s directory, `--resume` opens only the requested directory, and an agent close also closes only its own session engine. Keep a fake session-engine factory at the composition boundary so unit tests do not start NATS.

**Step 2: Verify RED**

Run from the SWE worktree: `GOWORK=off go test -race ./cmd/swe ./swarms/swe -run 'Test.*SessionStore|Test.*Engine'`

Expected: failure because the current process-global `startPersistence` API remains.

**Step 3: Implement a session-scoped factory**

Replace `startPersistence` plus the global `*swe.Persistence` open thunk with a narrow factory that owns a `*persistence.SessionStoreRoot`. Each `OpenAgent` call creates/resumes one `SessionEngine`, builds journal dependencies from that engine’s JetStream context, and installs engine close as the persisted agent teardown. Keep the old session alive until the TUI has successfully opened the replacement.

**Step 4: Verify GREEN and isolated-engine integration**

Run: `GOWORK=off go test -race ./cmd/swe ./swarms/swe`

Run: `GOWORK=off go test -tags integration -race ./swarms/swe -run Test.*Persistent`

Expected: PASS. Confirm two distinct temporary sessions can be simultaneously active.

**Step 5: Commit in the SWE repository**

```bash
git add cmd/swe/main.go cmd/swe/main_test.go swarms/swe/persistence.go swarms/swe/persistence_integration_test.go swarms/swe/agent.go
git commit -m "feat(swe): scope persistence to each session"
```

### Task 6: Move list and purge into the CLI boundary

**Files:**
- Modify: `../swe/cmd/swe/main.go`
- Modify: `../swe/cmd/swe/main_test.go`
- Modify: `../swe/swarms/swe/persistence.go`
- Modify: `../swe/swarms/swe/persistence_integration_test.go`

**Step 1: Write failing table-driven CLI tests**

Cover `--list` without engine startup, metadata sorting and `metadata-invalid` rows, `--purge-legacy-sessions` success/no-op/symlink rejection, and invalid combinations with `--resume` or normal TUI launch.

**Step 2: Verify RED**

Run: `GOWORK=off go test -race ./cmd/swe -run 'TestParseFlags|TestList|TestPurge'`

Expected: tests fail because the new flag and filesystem listing do not exist.

**Step 3: Implement minimal flag and output behavior**

Add a mutually exclusive `--purge-legacy-sessions` flag. Route `--list` directly to the manifest repository, print the exact purge path only after success, and never start an engine for either command. Remove the shared JetStream catalog from this local CLI path.

**Step 4: Verify GREEN**

Run: `GOWORK=off go test -race ./cmd/swe ./swarms/swe`

Expected: PASS.

**Step 5: Commit in the SWE repository**

```bash
git add cmd/swe/main.go cmd/swe/main_test.go swarms/swe/persistence.go swarms/swe/persistence_integration_test.go
git commit -m "feat(swe): list isolated sessions from manifests"
```

### Task 7: Remove the shutdown lease-loss false alarm

**Files:**
- Modify: `pkg/session/command_journal.go`
- Modify: `pkg/session/command_journal_test.go`
- Modify: `pkg/session/session.go`
- Modify: `pkg/session/session_test.go`

**Step 1: Write failing tests**

Use a command appender that returns `*journal.JournalLeaseLostError`. Verify ordinary commands remain audit-only but log an error, while shutdown after a legitimate lease release does not produce an error-level audit record and still completes loop shutdown. Include the multi-loop fan-out case shown by the incident.

**Step 2: Verify RED**

Run: `GOWORK=off go test -race ./pkg/session -run 'Test.*LeaseLost|TestShutdown'`

Expected: the expected shutdown log-level assertion fails.

**Step 3: Implement the smallest lifecycle-aware log policy**

Keep command appends before dispatch and lease release after drain. Distinguish a typed lease-lost audit failure during shutdown from all other append failures; downgrade only that expected path. Do not hide an append failure during normal operation or change dispatch semantics.

**Step 4: Verify GREEN**

Run: `GOWORK=off go test -race ./pkg/session -run 'Test.*LeaseLost|TestShutdown'`

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/session/command_journal.go pkg/session/command_journal_test.go pkg/session/session.go pkg/session/session_test.go
git commit -m "fix(session): suppress expected shutdown lease audit error"
```

### Task 8: Add optional model tiers and a testable resolver in SWE

**Files:**
- Modify: `../swe/swarms/swe/registry.go`
- Modify: `../swe/swarms/swe/model.go`
- Modify: `../swe/swarms/swe/model_test.go`
- Create: `../swe/swarms/swe/model_catalog.go`
- Create: `../swe/swarms/swe/model_catalog_test.go`

**Step 1: Write failing resolver tests**

Test empty catalog preserves the existing model, Standard chooses its first model, Economy resolves lazily, invalid/unknown supplied specs return typed configuration errors, Premium has no implicit selection, and no returned/loggable value exposes an API key.

**Step 2: Verify RED**

Run: `GOWORK=off go test -race ./swarms/swe -run 'TestModelCatalog|TestModelFactory'`

Expected: compilation failure for `ModelCatalog` and its resolver.

**Step 3: Implement typed catalog resolution**

Add `ModelCatalog` to `swe.Config`. Depend on a narrow resolver interface rather than passing all configuration through session logic. Validate explicit models at construction; resolve Standard for normal loops and Economy only when title generation starts. Keep Premium stored but unselected.

**Step 4: Verify GREEN**

Run: `GOWORK=off go test -race ./swarms/swe -run 'TestModelCatalog|TestModelFactory'`

Expected: PASS.

**Step 5: Commit in the SWE repository**

```bash
git add swarms/swe/registry.go swarms/swe/model.go swarms/swe/model_test.go swarms/swe/model_catalog.go swarms/swe/model_catalog_test.go
git commit -m "feat(swe): add optional model tiers"
```

### Task 9: Generate and persist a best-effort session title

**Files:**
- Create: `../swe/swarms/swe/session_title.go`
- Create: `../swe/swarms/swe/session_title_test.go`
- Modify: `../swe/swarms/swe/agent.go`
- Modify: `../swe/swarms/swe/persistence.go`
- Modify: `../swe/swarms/swe/persistence_integration_test.go`

**Step 1: Write failing tests against real metadata and fake LLMs**

Cover first non-empty text fallback on accepted input, image-only input retaining `TitleSourceNone`, successful Economy generated title replacing fallback after the first terminal turn, timeout/error/invalid output retaining fallback, one generation maximum, no tool definitions in the request, and close/clear not waiting for the title worker.

**Step 2: Verify RED**

Run: `GOWORK=off go test -race ./swarms/swe -run 'TestSessionTitle|TestPersistentTitle'`

Expected: compilation failure because no title coordinator exists.

**Step 3: Implement a narrow title coordinator**

Subscribe at the persisted-agent boundary, not in core session business logic. It receives only the first accepted user text and first terminal primary-loop response, stores the immediate fallback, then invokes Economy with a fixed system instruction, bounded prompt excerpts, no tools, and a timeout. Sanitize its one-line output and update the manifest through the metadata writer. Use an independent bounded context so shutdown never waits indefinitely.

**Step 4: Verify GREEN and integration behavior**

Run: `GOWORK=off go test -race ./swarms/swe -run 'TestSessionTitle|TestPersistentTitle'`

Run: `GOWORK=off go test -tags integration -race ./swarms/swe -run Test.*Title`

Expected: PASS.

**Step 5: Commit in the SWE repository**

```bash
git add swarms/swe/session_title.go swarms/swe/session_title_test.go swarms/swe/agent.go swarms/swe/persistence.go swarms/swe/persistence_integration_test.go
git commit -m "feat(swe): persist best-effort session titles"
```

### Task 10: Full verification and documentation reconciliation

**Files:**
- Modify: `docs/plans/2026-06-23-session-isolated-jetstream-design.md`
- Modify: `docs/plans/2026-06-23-session-isolated-jetstream-implementation.md`
- Modify as needed: `../swe/README.md` or existing CLI help documentation

**Step 1: Write any missing regression tests first**

Add a table case for each acceptance criterion still unproven by Tasks 1–9, especially a repeated clear cycle, list after legacy purge, and resumed session manifest repair.

**Step 2: Verify RED where coverage is missing**

Run only the new targeted tests and confirm each fails before its minimal implementation/test wiring change.

**Step 3: Complete documentation and release checks**

Update the design/plan status to implemented only after the checks below pass. Document the Unix-only local lock boundary and destructive purge semantics in CLI help.

**Step 4: Run final verification**

Run in each repository:

```bash
GOWORK=off go test -race ./...
GOWORK=off go test -tags integration -race ./...
CGO_ENABLED=0 GOWORK=off go build -trimpath ./...
make fmt-check
make secure
```

Expected: every command succeeds. Investigate any failure with `@superpowers:systematic-debugging`; do not attribute it to this feature without evidence.

**Step 5: Commit documentation separately in each repository**

```bash
git add docs/plans
git commit -m "docs: finalize isolated session persistence"
```
