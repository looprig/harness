# SWE Swarm v2 — P1 Implementation Plan (Bounded Agents on In-Session Loops)

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the single `coding` agent with a swarm of five bounded agents
(orchestrator, researcher, explorer, operator, reviewer), each a distinct `loop.Config`,
wired in `swarms/swe` and shipped as `cmd/swe` — re-using the in-session-loop substrate.

**Architecture:** An "agent" is a `loop.Config` (own `ToolSet` + `ModelSpec`). The swarm
builds a typed `AgentRegistry`, injects a shared identity prompt into each agent, runs the
orchestrator as the **primary** loop, and spawns the four leaves as in-session sub-loops via
the existing `Session.RunSubagent`. Re-adds depth/spawn-quota caps (dropped regression) via a
`WithLimits` session option with reservation/rollback in `NewLoop`. Stamps an immutable
`AgentName` onto `loop.Config` + `LoopStarted` for attribution + restore validation.

**Tech Stack:** Go 1.26; `internal/agent/{session,loop,loop/event}`, `tools/`, `agents/*`,
new `swarms/swe`, new `cmd/swe`, new `internal/cli`. Design:
`docs/plans/2026-06-21-swe-swarm-roles-design.md`.

**Scope:** P1 only. **Out of this plan (own follow-ons):** P2 embedded `Skill` tool, P2b
workspace `.skills/`, P3 runtime/env injection, the optional greeting (§5a) and per-agent TUI
labels (§6f, beyond the `AgentName` stamping this plan lands).

---

## Conventions (every task)

- **TDD:** write the failing test first, run it **red**, implement minimally, run it **green**, commit.
- **Build:** `CGO_ENABLED=0 go build -trimpath ./...`
- **Test (task):** `go test -race ./<pkg>/...`; **phase end:** `go test -race ./...`
- **Eval (phase end, where touched):** `go test -tags integration -race ./agents/operator/...`
- **Before each commit:** `make fmt` then `make secure` (gofmt + vet + staticcheck + gosec + vuln).
- **Commit style:** short, human-readable Conventional-Commits subject. **No co-author trailer.**
- **Green-keeping:** `agents/coding` and `cmd/cli` stay building until Phase 7 deletes them;
  every phase keeps `go build ./... && go test -race ./...` green.
- All work happens in this worktree (`feature/swe-swarm`), already rebased onto the
  persistence-merged `main`.

---

## Phase 0 — Engine: immutable `AgentName` (additive, coding unaffected)

Adds attribution metadata. Additive: `coding` keeps working (its loops get the zero name).

### Task 0.1: `identity.AgentName` type

**Files:**
- Modify: `internal/agent/loop/identity/` (the `identity` package — confirm path via `git grep -n "package identity"`)
- Test: same package `_test.go`

**Step 1 — failing test:** add `TestAgentNameZeroValue` asserting `identity.AgentName("")` is the zero/"unset" value and a non-empty name round-trips as a string.

**Step 2 — red:** `go test -race ./internal/agent/loop/identity/...` → FAIL (type undefined).

**Step 3 — implement:**
```go
// AgentName is the swarm-assigned name of the agent a loop runs (e.g. "orchestrator").
// Empty = unset (e.g. a plain coding loop, or a pre-AgentName persisted record).
type AgentName string
```

**Step 4 — green.** **Step 5 — commit:** `feat(identity): add AgentName type for loop attribution`.

### Task 0.2: carry `AgentName` on `loop.Config` and stamp `LoopStarted`

**Files:**
- Modify: `internal/agent/loop/config.go` (add field)
- Modify: `internal/agent/loop/event/event.go` (add `AgentName` to `event.Header` **or** to `LoopStarted`)
- Modify: `internal/agent/session/session.go` `NewLoop` (stamp it from `cfg`)
- Test: `internal/agent/loop/event/marshal_test.go`, `internal/agent/session/session_test.go`

**Step 1 — failing test:** in `event/marshal_test.go`, a `LoopStarted` carrying `AgentName:"operator"` **round-trips** through `MarshalEvent`/`UnmarshalEvent` (additive — `LoopStarted` already uses `marshalPlain`, so a new struct field serializes with no codec case). Add `TestLoopStartedAgentNameRoundTrip`. Also a session test: `NewLoop(parent, cfg{AgentName:"x"})` publishes a `LoopStarted` whose `AgentName == "x"`.

**Step 2 — red.**

**Step 3 — implement:**
- `config.go`: add exported field `AgentName identity.AgentName` to `loop.Config`.
- `event.go`: add `AgentName identity.AgentName \`json:"agent_name,omitzero"\`` to `event.Header` (preferred — one home, omitzero keeps old records empty).
- `loop.New`/loop construction: thread `cfg.AgentName` so it is available to the loop's emitted headers; for `LoopStarted` specifically, `session.NewLoop` sets `ev.Header.AgentName = cfg.AgentName` where it builds the `LoopStarted` (the `event.Factory`/stamp site — see `factory.go`).

**Step 4 — green** (`event` + `session`). **Step 5 — commit:** `feat(loop,event): stamp immutable AgentName on Config and LoopStarted`.

### Task 0.3: restore validates root-loop `AgentName`

**Files:**
- Modify: `internal/agent/session/restore_constructor.go` (root-loop validation)
- Test: `internal/agent/session/restore_test.go`

**Step 1 — failing test:** restoring a journal whose root `LoopStarted.AgentName` ≠ the configured primary name returns a typed mismatch error (reuse `ConfigMismatchError` or a sibling); an empty (pre-AgentName) name is routed to the **config-fingerprint** path, not silently accepted. Add `TestRestoreRootAgentNameMismatch` + `TestRestoreRootAgentNameEmptyLegacy`.

**Step 2 — red. Step 3 — implement** the root-loop check in the restore fold. **Step 4 — green. Step 5 — commit:** `feat(session): restore validates root-loop AgentName vs configured primary`.

**Phase 0 gate:** `go build ./... && go test -race ./...` green; `coding` still works.

---

## Phase 1 — Engine: resource caps (`WithLimits` + reservation/rollback)

Re-adds the dropped depth + spawn-quota caps. Additive option; default-off-safe.

### Task 1.1: `session.Limits` + `WithLimits` option + `spawned` counter field

**Files:**
- Modify: `internal/agent/session/session.go` (add `limits` + `spawned` fields to `Session`)
- Modify: `internal/agent/session/command_journal.go` (where `Option`s live) — add `WithLimits`
- Test: `internal/agent/session/composition_options_test.go`

**Step 1 — failing test:** `New(ctx, cfg, WithLimits(Limits{Depth:3, Quota:64}))` records the limits; default (no option) yields fail-secure defaults (`Depth:3, Quota:64` — pick the spec defaults). `TestWithLimitsDefaults` + `TestWithLimitsExplicit`.

**Step 2 — red. Step 3 — implement:**
```go
// Limits bounds in-session subagent loops (design §6d). Zero fields → defaults.
type Limits struct {
    Depth int // max ancestor chain length of a sub-loop (default 3)
    Quota int // max cumulative subagent loops per session lifetime (default 64)
}
func WithLimits(l Limits) Option { return func(s *Session) { s.limits = l.withDefaults() } }
```
Add `limits Limits` and `spawned int` (guarded by `loopsMu`) to `Session`; apply defaults in `newSession` if no option set.

**Step 4 — green. Step 5 — commit:** `feat(session): add Limits + WithLimits option (depth/quota caps)`.

### Task 1.2: typed cap errors

**Files:** `internal/agent/session/` errors file; Test: errors test.

**Step 1 — failing test:** `errors.As` recovers `*SessionError` with kinds `SessionLoopDepthExceeded` / `SessionLoopQuotaExceeded`.
**Step 2–4** implement the two kinds (mirror `SessionClosing`). **Step 5 — commit:** `feat(session): typed depth/quota cap errors`.

### Task 1.3: depth check (pure, pre-build) in `NewLoop`

**Files:** Modify `session.go` `NewLoop` (the early section, before `loop.New`); Test: `session_test.go`.

**Step 1 — failing test:** spawning past `Limits.Depth` (build a parent chain N deep via repeated `NewLoop` with chained `Provenance`) returns `SessionLoopDepthExceeded` and **creates no loop / emits no `LoopStarted`** (assert via a subscription). `TestNewLoopDepthCap`.

**Step 2 — red. Step 3 — implement:** under the early `loopsMu` section, walk `loop.Provenance.LoopID` up `s.loops[…].parent` counting depth; if `depth >= limits.Depth`, return `SessionLoopDepthExceeded` **before** minting IDs / `loop.New` (creates nothing).

**Step 4 — green. Step 5 — commit:** `feat(session): NewLoop depth cap (provenance walk, pre-build)`.

### Task 1.4: spawn-quota reservation + rollback in `NewLoop`

**Files:** Modify `session.go` `NewLoop`; Test: `session_test.go`.

**Step 1 — failing tests:**
- `TestNewLoopQuotaCap`: after `Quota` successful sub-loop spawns, the next `NewLoop` returns `SessionLoopQuotaExceeded` and emits no `LoopStarted`.
- `TestNewLoopQuotaConcurrent` (`-race`): N concurrent `NewLoop` calls never let `spawned` exceed `Quota`.
- `TestNewLoopQuotaRollback`: a forced failure after reservation (inject a `loop.New`/publish error via a test seam) **decrements** `spawned` (a later spawn still succeeds).
- Primary (built by `New`, not `NewLoop`) does **not** count.

**Step 2 — red. Step 3 — implement (reservation/rollback, design §6d):**
- Fold the cheap early-out into one authoritative `loopsMu.Lock`: check `closing` + depth + `spawned < Quota`; on success `spawned++` (reserve); unlock.
- Define `release := func(){ s.loopsMu.Lock(); s.spawned--; s.loopsMu.Unlock() }`.
- On every later failure path (`newID`, `loop.New`, the registration-time `closing` re-check, publish failure) call `release()` **alongside** the existing `cancel()`.
- Keep the existing closing re-check + publish-failure rollback structure intact (do not regress shutdown atomicity).

**Step 4 — green (`-race`). Step 5 — commit:** `feat(session): NewLoop spawn-quota reservation/rollback`.

### Task 1.5: quota survives restore (recount non-root `LoopStarted`)

**Files:**
- Modify: `internal/agent/session/restore_constructor.go`
- Add (if needed): an all-loop `LoopStarted` count helper in `internal/agent/session/journal/` (the v1 `EventReplayer` is primary-only)
- Test: `internal/agent/session/restore_test.go`

**Step 1 — failing test:** restoring a session whose journal has K non-root `LoopStarted` events initializes `spawned == K` (so a restart cannot grant a fresh `Quota`). `TestRestoreRecountsSpawnQuota`. Assert it does **not** read `SessionMeta.LoopCount`.

**Step 2 — red. Step 3 — implement:** before bringing the primary up, scan all `…loop.<lid>.event` subjects (or a metadata scan) counting `LoopStarted` with non-zero `Header.Cause` (= a spawned sub-loop); set `s.spawned`.

**Step 4 — green. Step 5 — commit:** `feat(session): restore recounts spawn quota from durable LoopStarted`.

**Phase 1 gate:** `go test -race ./internal/agent/...` green; `coding` still works (it never hits the caps in normal use, but its subagent spawns now count — verify the coding eval still passes).

---

## Phase 2 — Typed agent factory (`AgentRegistry`)

New package, no behavior change yet. Recommended home: `swarms/swe/registry.go` (or a small
`agents/registry` package if reuse across swarms is wanted — default to `swarms/swe`).

### Task 2.1: registry types

**Files:** Create `swarms/swe/registry.go`; Test: `swarms/swe/registry_test.go`.

**Step 1 — failing test:** `Registry.Lookup(name)` returns `(Agent, true)` for a registered agent and `(_, false)` for an unknown; `Catalog()` returns `{Name, Description}` in **deterministic order**. `TestRegistryLookup` + `TestRegistryCatalogOrder`.

**Step 2 — red. Step 3 — implement (design §6b):**
```go
type Agent struct {
    Name                identity.AgentName
    Description         string
    Role                string                          // role prompt; swarm prepends identity
    BuildTools          func(LeafToolDeps) loop.ToolSet // own allowlist + fresh PermissionChecker
    AllowsRuntimeSkills bool                            // P2b; false in P1
}
type LeafToolDeps struct { Root string; HTTPCl *http.Client }
type ModelFactory func(systemPrompt string) llm.ModelSpec
type AgentCatalogEntry struct { Name identity.AgentName; Description string }
type Registry struct { byName map[identity.AgentName]Agent; order []identity.AgentName }
func (r *Registry) Lookup(n identity.AgentName) (Agent, bool) { a, ok := r.byName[n]; return a, ok }
func (r *Registry) Catalog() []AgentCatalogEntry { /* in r.order */ }
```

**Step 4 — green. Step 5 — commit:** `feat(swarms/swe): typed AgentRegistry + Agent/LeafToolDeps/ModelFactory`.

---

## Phase 3 — The five agent packages

Each agent = `Role` prompt + `BuildTools` (its exact allowlist). Operator is salvaged from
`coding`; the four others are read/exec-scoped subsets.

### Task 3.1: shared identity + per-agent role prompts (XML)

**Files:** Create `swarms/swe/identity.go` (the `<identity product="SWE">` constant) and
`agents/<agent>/system.go` for each of the five (the `<role name="…">` constant). Operator's
role is salvaged from `agents/coding/prompts`. Test: `swarms/swe/identity_test.go` (asserts the
constant is non-empty, XML-well-formed, mentions persistence/security per design §5).

**Steps:** failing test → constants → green → commit `feat(swarms/swe,agents): identity + role XML prompts`.

### Task 3.2: `agents/operator` (salvage `coding`)

**Files:**
- Create `agents/operator/` by salvaging `agents/coding/`: rename `Coding`→`Operator`; keep
  lifecycle (`New`/`newWithClient`/`Close`/`Submit`/`Subscribe`/`Interrupt`/gate-trio),
  `model`, `newHTTPClient`, `errors.go`.
- `buildToolSet`: **operator allowlist** = `ReadFile, Glob, Grep, WriteFile, EditFile, Bash,
  Todo, AskUser` (drop `Fetch`, `WebSearch`, `Subagent`); `autoApprovedTools` drops `Subagent`.
- Migrate golden-set: `agents/coding/golden-set/`, `golden_set_test.go`,
  `eval_integration_test.go` → `agents/operator/` (`internal/eval` untouched).
- Test: `agents/operator/agent_test.go` (salvaged) + a new `TestBuildToolSetAllowlist`.

**Step 1 — failing test:** `TestBuildToolSetAllowlist` asserts operator's registry is **exactly**
the 8 tools above and `autoApprovedTools == {ReadFile,Glob,Grep,Todo,AskUser}`.
**Step 2 — red. Step 3 — implement** the salvage + allowlist. **Step 4 — green** (`go test -race ./agents/operator/...`; `-tags integration` for eval). **Step 5 — commit:** `feat(agents/operator): salvage coding as operator (write+exec allowlist)`.

> NOTE: `agents/coding` is **not deleted yet** (Phase 7). Both exist transiently.

### Task 3.3–3.6: leaf agents `researcher`, `explorer`, `reviewer` + orchestrator toolset

For each, create `agents/<name>/` exposing `Name`, `Description`, `Role`, and `BuildTools`
returning its allowlist (design §3):
- **researcher:** `Glob, Grep, ReadFile, WebSearch, Fetch, AskUser` — auto-approve `{ReadFile,Glob,Grep,AskUser}`; Ask `{WebSearch,Fetch}`.
- **explorer:** `Glob, Grep, ReadFile, AskUser` — all auto-approve (never gates).
- **reviewer:** `Glob, Grep, ReadFile, Bash, Todo, AskUser` — Ask `{Bash}`.
- **orchestrator toolset:** `Glob, Grep, ReadFile, Todo, AskUser, Subagent` — auto-approve `{ReadFile,Glob,Grep,Todo,AskUser,Subagent}`.

**Each task:** failing `TestBuildToolSetAllowlist` (exact set + auto-approve set) → implement
`BuildTools` (fresh `PermissionChecker` per call, mirror operator) → green → commit
`feat(agents/<name>): boundary + allowlist`.

**Phase 3 gate:** `go build ./... && go test -race ./agents/...` green.

---

## Phase 4 — `swarms/swe` wiring

Builds the registry from the five agents, injects identity, wires the spawner, runs the
orchestrator as the primary. **Does not yet flip the Subagent tool arg** (Phase 5).

### Task 4.1: `swarmSpawner` (agent-aware), behind the existing `tools.Spawner` shape for now

**Files:** Create `swarms/swe/spawner.go`; Test: `swarms/swe/spawner_test.go`.

**Step 1 — failing test:** `swarmSpawner.Spawn(ctx, parent, agent, message)` looks up `agent`
in the registry, builds a **fresh** `loop.Config` (`Model = modelFactory(identity+role)`,
`Tools = a.BuildTools(deps)`, `AgentName = a.Name`) and calls `session.RunSubagent`; unknown
agent → `UnknownAgentError`. Use a fake session capturing the `cfg` it receives; assert the
spawned cfg's tools == that agent's allowlist and `cfg.AgentName == agent`. `TestSwarmSpawnerResolvesAgent` + `TestSwarmSpawnerUnknownAgent`.

**Step 2 — red. Step 3 — implement** (`codingSpawner` late-bind pattern: `session` set once
post-construction). **Step 4 — green. Step 5 — commit:** `feat(swarms/swe): agent-aware swarmSpawner`.

### Task 4.2: `UnknownAgentError`

**Files:** `tools/errors.go` (or `swarms/swe`); Test: errors test. Failing `errors.As` test → implement → commit `feat(tools): UnknownAgentError`.

### Task 4.3: `swe.New(ctx) (tui.Agent, error)` — orchestrator as primary

**Files:** Create `swarms/swe/swarm.go`; Test: `swarms/swe/swarm_test.go`.

**Step 1 — failing test:** `swe.New(ctx)` returns a `tui.Agent` whose primary loop runs the
**orchestrator** (assert `PrimaryLoopID` exists, the primary `cfg.AgentName == "orchestrator"`,
and its toolset includes `Subagent`). Build the registry (4 leaves), `swarmSpawner`, the
orchestrator `loop.Config` (`Model = modelFactory(identity+orchestratorRole)`, toolset incl.
`Subagent` wired to the spawner), pass `WithLimits(...)` to `session.New`. `TestSwarmNewPrimaryIsOrchestrator`.

**Step 2 — red. Step 3 — implement** the composition root (the only place the five couple).
**Step 4 — green. Step 5 — commit:** `feat(swarms/swe): swe.New wires orchestrator-as-primary + leaf registry`.

**Phase 4 gate:** `go build ./... && go test -race ./swarms/...` green. `coding` still builds.

---

## Phase 5 — Agent-aware Subagent tool (coordinated flip)

This is the one breaking change to a shared file (`tools/subagent.go` + `tools.Spawner`). Do it
in a single commit so the tree stays green, updating both consumers.

### Task 5.1: `Spawner` interface + Subagent tool args → `{agent, message}`

**Files:**
- Modify: `tools/subagent.go` (`subagentArgs{Agent, Message}`, schema, `InvokableRun` validates
  `agent` via catalog, passes to `Spawn`)
- Modify: `tools.Spawner` → `Spawn(ctx, parent loop.Provenance, agent identity.AgentName, message string) (string, error)`
- Modify: `agents/coding/spawner.go` to satisfy the new interface **temporarily** (ignore/validate
  `agent`, single-agent) **OR** — cleaner — do this task **together with Phase 7** so `coding`
  is deleted in the same step and only `swarmSpawner` implements the new interface.
- Modify: `swarms/swe/spawner.go` to match the interface (already agent-aware).
- Add: `<available_subagents>` catalog into the Subagent tool's description from `registry.Catalog()`.
- Test: `tools/subagent_test.go` (`{agent,message}` decode; unknown agent → fail-secure string;
  catalog lists only permitted agents) + `FuzzSubagentArgs`.

**Decision:** to keep green without a throwaway coding shim, **merge Task 5.1 with Phase 7**
(flip the tool + delete coding + wire orchestrator's Subagent in one commit). Until then,
`swarms/swe` can wire the orchestrator's `Subagent` against an internal agent-aware spawner type
without changing the shared `tools.Spawner` (i.e. keep `tools/subagent.go` on `{message}` until
the flip). Pick one approach at execution time; the plan assumes the **merged flip** (cleaner).

**Steps:** failing tests (new `{agent,message}` behavior + fuzz) → implement the flip across
`tools/subagent.go`, `tools.Spawner`, `swarms/swe` → green → commit (in Phase 7's commit).

---

## Phase 6 — CLI: `internal/cli` + `cmd/swe` + Makefile

### Task 6.1: extract `internal/cli` from `cmd/cli/main.go`

**Files:** Create `internal/cli/run.go` (the runtime: `~/.urvi/urvi.log` + slog,
`signal.NotifyContext`, `ttylog` capture, `tea.Program` run/teardown/exit codes). Signature:
`Run(ctx context.Context, newAgent func(context.Context) (tui.Agent, error), banner Banner) int`.
Test: `internal/cli/run_test.go` (with a fake agent + fake program seam).

Failing test → extract (move logic out of `cmd/cli/main.go`, leave `cmd/cli` calling it so it
still builds) → green → commit `refactor(cli): extract shared runtime into internal/cli`.

### Task 6.2: `cmd/swe/main.go` (thin)

**Files:** Create `cmd/swe/main.go`:
```go
func main() { os.Exit(internal_cli.Run(context.Background(), swe.New, cli.Banner{Name: "SWE"})) }
```
Test: `cmd/swe/main_test.go` (smoke: `swe.New` wired). Failing → implement → green → commit
`feat(cmd/swe): thin SWE entrypoint`.

### Task 6.3: Makefile + run migration

**Files:** Modify `Makefile`:
- `build:` → `CGO_ENABLED=0 go build -trimpath -o bin/swe ./cmd/swe`
- `run:` → `go run ./cmd/swe` (drop `AGENT=`; keep `.env` load)
- Sweep installer/CI/docs for `cmd/cli`, `bin/urvi`, `AGENT=`.
(No unit test; verify `make build && ./bin/swe --help`-style smoke or `make run` boots.)
Commit `build: target cmd/swe → bin/swe; drop AGENT selection`.

**Decision flagged for the maintainer:** binary name `bin/swe` vs keeping `bin/urvi`. Plan
assumes `bin/swe`.

**Phase 6 gate:** `go build ./...` green; `bin/swe` boots the orchestrator.

---

## Phase 7 — Delete `coding` + `cmd/cli` (flip Subagent here)

Only after Phases 2–6 compile and tests pass.

### Task 7.1: flip the Subagent tool + delete coding + delete cmd/cli (one commit)

**Files:**
- Apply Task 5.1's `tools/subagent.go` + `tools.Spawner` flip to `{agent, message}`.
- Delete `agents/coding/` (operator + the four leaves now cover it; golden-set already migrated).
- Delete `cmd/cli/` (registry: `defaultAgent`/`agentName`/`buildRegistry`/`agentDescriptions`/
  `agentDisplayNames`).
- Remove any remaining importers (`git grep -n "agents/coding\|cmd/cli"`).

**Step 1 — failing/guard:** `git grep -n "agents/coding"` returns nothing after deletion;
`go build ./...` is the gate.
**Step 2–4:** delete, fix importers, `go build -trimpath ./... && go test -race ./...`,
`go test -tags integration -race ./agents/operator/...` (eval green).
**Step 5 — commit:** `feat(swarm): flip Subagent to agent-aware; delete coding + cmd/cli`.

**Phase 7 gate (the P1 milestone):** whole tree `go build -trimpath ./...` + `go test -race ./...`
+ eval integration green; `make secure` clean; `bin/swe` runs the orchestrator and can spawn the
four leaves (manual smoke or an acceptance test: orchestrator spawns `operator`, leaf cannot
spawn, depth/quota caps enforced).

### Task 7.2: cross-cutting acceptance test

**Files:** `swarms/swe/acceptance_test.go` (build-tagged integration as needed).
Assert: orchestrator can spawn only its four; a leaf has no `Subagent`; an unknown agent →
fail-secure; depth/quota caps reject with no `LoopStarted`; a spawned leaf's gates surface by
`LoopID` and route via `Approve(loopID,…)`. Commit `test(swarm): cross-cutting P1 acceptance`.

---

## Done-when (P1)

- Five agents wired in `swarms/swe`; orchestrator is the primary; leaves spawn as in-session
  loops with their own allowlists (per-child least privilege).
- Depth + spawn-quota caps enforced (reservation/rollback) and **survive restore**.
- `AgentName` stamped + restore-validated.
- `cmd/swe`/`bin/swe` ships; `coding`/`cmd/cli` deleted; eval green; `make secure` clean.

**Follow-on plans (not here):** P2 embedded `Skill` tool; P2b workspace `.skills/` (Prepare seam,
`os.Root`, `SkillLoadRequest`); P3 runtime/env injection; greeting (§5a) + per-agent TUI labels (§6f).
