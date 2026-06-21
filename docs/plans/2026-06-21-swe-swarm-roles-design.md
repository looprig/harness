# SWE Swarm v2 — Bounded Agents on In-Session Loops (Design)

- **Date:** 2026-06-21
- **Status:** Draft — supersedes `docs/plans/2026-06-16-swe-swarm-agents-design.md`
- **Supersedes / why:** the 06-16 draft was verified against the code *of that day*.
  Since then the subagent substrate was rebuilt by **in-session-subagents**
  (`2026-06-19`/`2026-06-20`, landed) on top of **loop-machine**,
  **tui-event-adoption**, **id-normalization**, and **session-observability**. The
  06-16 draft's largest section (§6, the subagent event *bridge*) describes plumbing
  that **no longer exists** and is now solved more simply. This rewrite keeps the
  draft's thesis (agents are boundaries, least privilege by construction, skills for
  behavior) and rebases the mechanism onto what is actually in `main`.
- **Terminology:** **agent** = the boundary (a `loop.Config`), identified by a typed
  `identity.AgentName`; used consistently in code, the registry, the `Subagent` arg,
  and errors. "role" survives only as the system-prompt section tag `<role name="…">`
  (§5) — the prompt's notion, not the code's.
- **Depends on (all landed in `main`):** in-session subagents
  (`session.RunSubagent` / `NewLoop` / `Submit` / `SubscribeEvents`, the shared hub,
  `LoopStarted`, `drainToFinalText`), id-normalization (`identity.Coordinates`,
  `identity.Agency`, `loop.Provenance`), tui-event-adoption (the TUI is a projection
  of the session stream; `DefaultEventFilter`).

---

## 0. What changed since the 06-16 draft (verified against code)

| 06-16 draft assumed | Reality in `main` (2026-06-21) | Consequence |
|---|---|---|
| Subagents are **child sessions** driven via `session.Stream`, needing a bespoke bridge (§6a–6d) | Subagents are **in-session loops** on one shared hub, attributed by `Header.LoopID`; `session.Stream`/`Invoke` are **deleted** | **§6a–6d is obsolete** — delete, don't port |
| Responses addressed by `sessionID`: `Approve(sessionID, callID, scope)` (§6b) | The gate trio is already **loopID-addressed**: `Approve(ctx, loopID, callID, scope)`, `Deny`, `ProvideAnswer` (`agents/coding/agent.go:220-237`) | No `sessionID→session` registry needed; routing is done |
| Add `SessionID` to the stream path; session-keyed transcript (§6c/6d) | Events carry `identity.Coordinates{SessionID, LoopID, …}` in `event.Header`; TUI filters by loop (`tui/agent.go` `DefaultEventFilter`), subagent firehose muted, `StepDone`/terminals/gates delivered all-loop | Attribution exists; per-agent *labels* need new metadata (§6f) |
| Rewire `tools/subagent.go` selector `skill`→`agent`; args `{agent, task}` | The selector is **gone**; args are `{message}` (`tools/subagent.go:56-66`); the tool depends on a narrow `Spawner` (DIP) | Add an **`agent`** arg + agent-aware Spawner — a small change, not a rewrite |
| Delete `coding` **and** `personal-assistant`; salvage `coding/subagent_factory.go` | `personal-assistant` **already deleted**; `subagent_factory.go` **already replaced** by `agents/coding/spawner.go` | Salvage table shrinks (§8) |
| Recursion bounded by depth cap (2) as a backstop | Depth cap **intentionally dropped**; recursion **unbounded** in breadth and depth, and `Subagent` is auto-approved (`agents/coding/spawner.go:20-23`, `autoApprovedTools`) | The swarm must **re-add resource caps** (§6d, §10) |
| Remove the CLI multi-agent registry (§9) | Registry **still present**: `buildRegistry`/`defaultAgent`/`agentName` (`cmd/cli/main.go`); the **build/run contract** still targets `cmd/cli` → `bin/urvi` with `AGENT=` selection (`Makefile:14,20`) | §9 still applies — incl. the Makefile |

**Net:** the hard, risky part of the 06-16 draft (the bridge) is now *free*. The
valuable part (agent boundaries + per-child least privilege + skills) is still
**unbuilt**, and the in-session cut explicitly deferred it ("skills/persona
selection — follow-on"; subagents currently **clone the coding agent's full
toolset** — `buildToolSet` is reused verbatim per sub-loop, `agents/coding/agent.go:123-164`).

## 1. Goal

Turn the single `coding` agent into a **swarm of bounded agents** (orchestrator,
researcher, explorer, operator, reviewer), each a distinct **boundary** (tools +
model + role prompt). Add a **Skill** capability (P2). Ship the swarm as **its own
binary** (`cmd/swe`). Rename `coding`→`operator` (salvage near-verbatim); the
substrate (in-session loops, the hub, the gate trio) is reused, not rebuilt.

## 2. Core model (rebased)

- **Agent = a boundary**, and in this codebase a boundary is exactly a
  **`loop.Config`**: its `Tools loop.ToolSet` (own `Registry` + own fail-secure
  `PermissionChecker`) and its `Model llm.ModelSpec` (which carries the **system
  prompt** — `agents/coding/agent.go:69`). An agent is identified by a typed
  `identity.AgentName` and is *a function that builds a fresh `loop.Config`* (§6b).
  There is **no per-agent session** — agents are loops within one session.
- **Skill = behavior/knowledge** loaded on demand; cannot change tools/model. P2.
- **Swarm = a composition** that builds the **agent registry** (§6b), wires the
  orchestrator as the **primary loop**, and injects the shared **identity** prompt
  into every agent (§5). Lives in `swarms/<name>/`. The single place the agents are
  coupled.
- **Subagent = the spawn entity**: an **in-session loop** created by
  `Session.RunSubagent(ctx, parent, cfg, blocks)` with the *target agent's* `cfg`.
  **Task = the message**. A model issues tasks against *allowed agents*; it can never
  mint a new boundary (no self-authorization) — a new agent is a human adding a
  package + registry entry.
- **Runtime/env context** (date/cwd/git) = a volatile per-turn layer; **still not
  injected anywhere today** — kept as P3 (a new engine capability).

## 3. Agents & tool allowlists

| Agent | Tools | Spawns |
|---|---|---|
| researcher | Glob, Grep, ReadFile, WebSearch, Fetch, AskUser, (Skill P2) | — |
| explorer | Glob, Grep, ReadFile, AskUser, (Skill P2) | — |
| operator | Glob, Grep, ReadFile, WriteFile, EditFile, Bash, Todo, AskUser, (Skill P2) | — |
| reviewer | Glob, Grep, ReadFile, Bash, Todo, AskUser, (Skill P2) | — |
| orchestrator | Glob, Grep, ReadFile, Todo, AskUser, **Subagent**, (Skill P2) | researcher, explorer, operator, reviewer |

- Each agent's allowlist is its own `buildToolSet` — a tool it lacks is **literally
  absent from its Registry** (least privilege by construction). This is the real fix:
  today every sub-loop reuses coding's *full eleven-tool* `buildToolSet`.
- **Auto-approve is per-agent and unchanged in mechanism.** It rides the existing
  `PermissionPolicy.HardApprove` list, evaluated as a stage of `Check` *after*
  non-bypassable containment + hard-deny (so an auto-approved `ReadFile` still cannot
  read a denied path — `tools/subagent.go:42-44`, `autoApprovedTools`). Read/search
  (ReadFile/Glob/Grep), Todo, AskUser, Subagent, and (P2) Skill auto-approve.
  WriteFile/EditFile/Bash/Fetch/WebSearch stay **Ask** (fail-secure, per-call human
  approval *until* a scope is granted — §10). Explorer holds only auto-approve tools
  → never gates.
- **Per-loop approval isolation already exists:** `buildToolSet` mints a **fresh
  `PermissionChecker` per call** (`agents/coding/agent.go:136-146`), so a sub-loop's
  **session-scope** grants never leak into a sibling/parent. Per-agent `buildToolSet`
  inherits this for free. **Persisted** workspace/user approvals are a *different
  store* (`ApprovalRecord{Tool, Match, Prefix, Effect}`, `tools/permission.go:59`)
  keyed by `(tool, match)` with **no agent dimension** — they cross agents by design
  (§10).
- **Capability matrix (not a lattice).** Tool-set inclusion is only a *partial* order:
  explorer (read-only) is the floor and is contained by all; **researcher (+network)
  and reviewer (+exec) are incomparable** (neither's tools contain the other's);
  operator (+write+exec) is the broadest mutating agent; orchestrator (delegate-only,
  no write/exec/net) is disjoint from the workers. Treat it as a capability matrix —
  there is no unique meet/join, so "lattice" is the wrong word.

## 4. Package layout

```
agents/                each agent = a loop.Config builder (buildToolSet + ModelSpec + Name/Description)
  researcher/ explorer/ operator/ reviewer/ orchestrator/
swarms/
  swe/swarm.go         builds the agent registry, wires the orchestrator as primary,
                       injects identity, returns a tui.Agent (swe.New(ctx))
  swe/skills/          (P2) embedded SKILL.md files (embed.FS), trusted in-repo (§7)
cmd/
  swe/main.go          thin: internal/cli.Run(ctx, swe.New, banner{Name:"SWE"})
internal/
  cli/                 shared CLI runtime extracted from cmd/cli/main.go
  agent/session/       + WithLimits option (resource caps, §6d) on New/Restore
tools/                 Subagent gains an `agent` arg (§6); + Skill tool (P2)
tui/                   already multi-loop aware (§6); + optional opening greeting (§5a)
deleted: cmd/cli (registry); agents/coding (renamed → operator)
already gone: agents/personal-assistant, agents/coding/subagent_factory.go
```

**One binary per swarm — kept, but it is *deployment/composition* isolation, not the
primary security boundary.** `cmd/swe` links only `swarms/swe` + the five `agents/*`
it names, so another swarm's tools can't be *accidentally* linked — useful
defense-in-depth and a clean composition root. **The runtime-enforced boundary is the
per-loop `ToolSet` registry + the permission gate** (a tool an agent lacks is absent
from its registry; every Ask-tool is gated). Do not rely on the binary split as the
security control; rely on the per-loop ToolSet + gate. *Lighter alternative (not
chosen):* one binary + a runtime swarm registry — fewer binaries, but it links every
swarm together; flip if the per-swarm binary overhead outweighs the composition
benefit.

## 5. Prompt architecture (identity + role)

Three layers by owner/volatility — **unchanged intent, corrected placement**:

| Layer | Owner | Placement | Volatility |
|---|---|---|---|
| identity | swarm | each agent's `ModelSpec.System` (shared prefix) | stable (cached) |
| role | agent | each agent's `ModelSpec.System` (agent-specific suffix) | stable (cached) |
| runtime/env | session engine | appended turn message | volatile — **P3, not built** |

- **Identity is injected per-agent, once, at swarm wiring time.** The swarm
  (`swarms/swe/swarm.go`) is the *only* place identity exists, and it injects it into
  **each** agent as it builds that agent's `loop.Config`:
  `systemPrompt = identity + role`, passed into the agent's `ModelSpec` (the same seam
  coding uses today: `model.Spec(apiKey, prompts.SystemPrompt)`). Because this happens
  at composition (not per turn), identity stays in the **cached prefix**. Identity is
  a **trusted in-repo constant**; the role XML lives in `agents/<agent>/system.go`
  (operator's is salvaged from `coding/prompts`). No external/user data in prompts.
- `<identity product="SWE">` carries the cross-cutting bits (persona, persistence,
  security/secrets, reversibility); `<role name="operator">` carries the agent's job +
  specific guidelines. (XML shapes as in the 06-16 draft §5 — unchanged.)
- **Caching:** the system prompt is the stable cached prefix; runtime/env is appended
  at the tail and updated by **appending** a fresh block, never editing the prefix
  (P3, when the engine grows runtime injection).

## 5a. Startup greeting (optional, not P1)

Instead of the user opening with "Hi", the swarm *may* show an opening assistant
entry describing what it does and which agents (and, P2, skills) are available.
**Decision: option A only — UI-only, deterministic, lifecycle-neutral — and it is
OPTIONAL** (off by default / behind a flag); no phase depends on it.

- **Deterministic, no LLM call.** Rendered from the *same* agent registry (§6b) that
  feeds the Subagent catalog (§6c), so the greeting can never drift from the actual
  wiring and costs nothing.
- **Lifecycle untouched.** It is **not** a turn and **not** a command — the TUI
  renders it as the opening transcript entry. The normal flow (user-msg → cmd → loop
  → turn) begins unchanged at the first *real* user message. No fake user "Hi".
- **Not in the model's context.** The primary loop's `state.msgs` stays empty until
  the first turn; agents already learn their capabilities from their system prompts,
  so seeding history would be redundant token cost.

*Recorded, not chosen:* (B) seeding the greeting into the primary loop's history (a
canned `content.AIMessage` at construction via a new `loop.Config` seed field) —
feasible and lifecycle-safe (it is pre-turn history, not a command/turn), but only
worth it if the model must "know" it greeted; (C) a machine-initiated greeting turn —
most lifecycle-pure but costs a call and is non-deterministic. Neither is in scope.

## 6. Subagent mechanism (the rebase)

The substrate is already built; the swarm is an **agent-aware wiring** of it.

**6a. The Subagent tool gains an `agent`.** Args become `{agent, message}`
(`tools/subagent.go` `subagentArgs`/`subagentSchema`). The `Spawner` interface
grows the agent: `Spawn(ctx, parent loop.Provenance, agent identity.AgentName, message string) (string, error)`.
The tool validates `agent` against its catalog (fail-secure tool-result string on
unknown/unauthorized — `tools.UnknownAgentError`), reads its own `parent` from ctx
(`loop.ProvenanceFrom`, unchanged), and calls `Spawn`. The arg decoder is **fuzzed**
(§12) — it parses untrusted model output.

**6b. Typed agent factory + composition flow (single source of truth).**
`map[string]func() loop.Config` is too loose, and one builder cannot both "own its
role" *and* have "identity already injected." Split ownership: **the agent package
owns its role + toolset; the swarm owns identity + the model and assembles the
`loop.Config`.**

```go
// identity.AgentName — a domain-meaningful name, not a bare string.
type AgentName string

// Agent is what an agent PACKAGE exposes. It owns its role prompt and its toolset
// builder; it does NOT own identity, the model, or the full loop.Config.
type Agent struct {
    Name                AgentName
    Description         string                           // Subagent catalog + greeting
    Role               string                           // role prompt; the swarm prepends identity
    BuildTools         func(LeafToolDeps) loop.ToolSet  // the agent's OWN allowlist (fresh PermissionChecker)
    AllowsRuntimeSkills bool                            // §7a: may load untrusted workspace .skills/ (explorer + researcher only)
}

// LeafToolDeps are the deps a leaf agent's toolset needs. NO Spawner — a leaf cannot
// spawn (least privilege; §6d depth-1). The orchestrator is assembled separately (below).
type LeafToolDeps struct {
    Root   string
    HTTPCl *http.Client
}

// ModelFactory turns a finished system prompt into a ModelSpec. The swarm owns the
// provider/model/sampling (e.g. wraps llm.ChutesKimiK2().Spec); agents never see it.
type ModelFactory func(systemPrompt string) llm.ModelSpec

// AgentRegistry is THE single source of truth — tool validation (§6a), the prompt
// catalog (§6c), the greeting (§5a), AgentName stamping (§6f).
type AgentRegistry interface {
    Lookup(AgentName) (Agent, bool)          // fail-secure: ok=false → UnknownAgentError
    Catalog() []AgentCatalogEntry            // {Name, Description}, deterministic order
}
```

**Composition flow (in `swarms/swe`, the only place agents couple):**
1. Construct the four leaf `Agent`s (each contributes `Role` + `BuildTools`).
2. Build the `AgentRegistry` from them; build `swarmSpawner{registry, session
   (late-bound)}` (the Subagent capability).
3. **Per spawn** (`swarmSpawner.Spawn(ctx, parent, agent, message)`):
   `a, ok := registry.Lookup(agent)` (fail-secure) → `sys := identity + a.Role` →
   `cfg := loop.Config{Client: client, Model: modelFactory(sys), Tools:
   a.BuildTools(deps), AgentName: a.Name}` → `session.RunSubagent(ctx, parent, cfg,
   blocks)`. Identity is injected **here**, by the swarm; the agent never sees identity,
   the swarm never sees the agent's role internals. `AgentName` rides on `cfg` →
   stamped on `LoopStarted` (§6f). `RunSubagent` is otherwise unchanged (`NewLoop` →
   subscribe-before-submit → machine-stamped submit → `drainToFinalText`,
   `internal/agent/session/session.go:555`).
4. **Orchestrator (the primary) is assembled separately** — it is not a leaf and is the
   one agent wired with the `Spawner`: `swarms/swe.New` builds its ToolSet (incl. the
   `Subagent` tool wired to `swarmSpawner`), its `Model = modelFactory(identity +
   orchestratorRole)`, and passes that `loop.Config` to `session.New` as the primary.
   **No leaf ever receives a `Spawner`** (matches the least-privilege boundary).

**6c. Catalog injection.** The orchestrator's `Subagent` tool renders
`registry.Catalog()` into its description as an `<available_subagents>` listing (only
the caller's permitted agents). Same registry feeds the (P2) skill catalog and the
optional greeting (§5a) — one source, three consumers.

**6d. Resource caps — fully specified (closes the dropped-cap regression).** Loops are
**never deleted** (`in-session-subagents` §8), so a naive "live-loop count" is a
session-lifetime quota. Caps are therefore defined explicitly, and they ride a **new
`WithLimits(session.Limits{Depth, Quota})` construction option** — **not** a positional
config struct: the persistence branch already builds sessions via functional options
(`New(ctx, cfg loop.Config, opts ...Option)` and `Restore(ctx, cfg, sessionID, js,
objectStore, leases, opts ...Option)`), so the caps join that `opts ...Option` chain on
**both** seams (§16.2). The session owns loop lifetime, so it is the correct owner — not
the swarm, not agent business logic. The swarm *chooses* the limits via the option; the
session *enforces* them; an absent option gets fail-secure defaults (mirrors `loop.New`).

| Cap | Definition | Derivation | Default | Lifetime semantics |
|---|---|---|---|---|
| **Depth** | max ancestor chain length of a subagent loop | walk `loop.Provenance.LoopID` up `loops[…].parent` at `NewLoop` | **N = 3** | structural — independent of session lifetime (the designed tree is depth-1; leaves can't spawn, so this is a backstop) |
| **Spawn quota** | max **cumulative** subagent loops created per session (primary **excluded**) | a monotonic counter incremented under `loopsMu` at `NewLoop` | **M = 64** | **a session-lifetime budget** (counter never decrements because loops are retained) |

- **Enforcement — reservation/rollback (verified against `NewLoop` on both branches).**
  `NewLoop` builds and **starts** the loop (`loop.New`) *before* its registration
  `loopsMu.Lock`, and already rolls back with `cancel()` on the closing re-check and on
  publish failure (`session.go:299/305/314/340` on `main`; the same structure at
  `:437/443/452-462` on the persistence branch) — so literal "create nothing" is not
  achievable; the honest contract is reserve-then-rollback:
  - **Depth** is a *pure* check (a function of the fixed parent chain): walk
    `loop.Provenance.LoopID` over `s.loops[…].parent` and reject **before** minting IDs
    or `loop.New` — creates nothing, no rollback needed.
  - **Quota** is a *reservation*: fold an authoritative **`loopsMu.Lock`** at the top
    (replacing the cheap RLock early-out) that checks `closing` + depth + `spawned < M`
    and, on success, **increments `spawned` (reserve)** before unlocking. Any later
    failure — ID mint, `loop.New`, the registration-time `closing` re-check, or publish
    failure — **decrements `spawned` (release)** alongside the existing `cancel()`.
  - A successful spawn leaves `spawned` incremented permanently (loops are retained →
    the cumulative budget never decrements on idle; it decrements **only** on a
    rolled-back spawn). The primary is built by `session.New`, not `NewLoop`, so it
    never touches `spawned`.
  - **Shutdown atomicity preserved:** the `closing` checks (early + at registration)
    and the Shutdown snapshot are unchanged; `spawned` is a separate counter under the
    same `loopsMu`, so it composes with them. A rejected spawn still emits no
    `LoopStarted` (rejection precedes the publish).
- **Exhaustion UX.** `NewLoop` returns a typed `SessionError`
  (`SessionLoopDepthExceeded` / `SessionLoopQuotaExceeded`); `RunSubagent` propagates
  it; the `Subagent` tool surfaces it as a tool-result error string the orchestrator
  reads and adapts to (e.g. does the work itself, or tells the user) — **not** a
  crash. Optionally the TUI shows a one-line "subagent capacity reached" note.
- **Upgrade path (documented, not v1).** When loop teardown or reuse ("agent teams")
  lands, replace the cumulative quota with a **concurrent-active gauge** (count loops
  with a running turn, decremented on idle) — a truer breadth metric. v1 uses the
  cumulative budget because it is unambiguous and fail-secure under never-delete.
- **Persistence — the quota is durable.** `spawned` is derived from the durable
  `LoopStarted` log, so it must **survive restart** (else a restart grants a fresh M): on
  restore, recount it from non-root `LoopStarted` events, and pass the `WithLimits` option
  through both `New` and `Restore`'s `opts ...Option`. See **§16**.

**6e. The bridge is free (delete 06-16 §6a–6d).** Subagent loops publish to the
**shared hub**; the TUI's `DefaultEventFilter(primaryLoopID)` already mutes their
token firehose (Ephemeral = primary only) while delivering their `StepDone` /
terminals / **permission gates / AskUser** (Enduring = all loops), attributed by
`Header.LoopID` (`tui/agent.go`, `tui/agent_test.go`). Gates route back via the
**loopID-addressed** trio `Approve(ctx, loopID, callID, scope)` /`Deny`/`ProvideAnswer`
(`agents/coding/agent.go:220-237`). `LoopStarted` (Enduring, carrying parent linkage
in `Header.Cause`) gives the loop tree. **Nothing in §6a–6d is needed.**

**6f. Per-agent attribution — requires new metadata (corrected).** `LoopStarted`
today carries loop *identity* + parent *provenance* (`Header.{Coordinates, Cause}`),
**not the selected agent name** — so a `LoopID→agent` map is **not** derivable from
the event + registry alone (the earlier draft was wrong). To attribute a loop to its
agent: carry an **immutable `AgentName`** on the spawn path (`loop.Config` /
`NewLoop`) and **stamp it onto `LoopStarted`** (the primary's is `"orchestrator"`).
This small metadata addition is the enabler for (a) audit ("which agent did this"),
and (b) the *optional* TUI label `▸ researcher: running/done`. The label *rendering*
stays optional (§6e routing already works without it); the `AgentName` stamping is the
prerequisite and is cheap. *(Alternative if AgentName is deferred: correlate the
spawning `Subagent` tool-call — which knows the agent — to the child loop by
provenance; fragile, not preferred.)* **Durability:** `AgentName` rides on the
**Enduring** `LoopStarted`, so it is journaled — the codec adds it as an **additive**
field and restore **validates the root loop's `AgentName` == the configured primary**
(§16).

## 7. Skill tool + loader (P2)

- `tools.Skill` (param `name`, resolves against *this agent's* allowed-skill set, reads
  the skill body, returns it; fail-secure; `Auditable`; auto-approve).
  `<available_skills>` catalog injected into an agent's system prompt only when it has
  ≥1 skill (else no catalog and **no `Skill` tool wired**). First point `Skill`
  appears in the build (P2).
- **Embedded skills are the DEFAULT, trusted source (`embed.FS`).**
  `swarms/swe/skills/*/SKILL.md` is embedded into the binary (`//go:embed`), so default
  skills are reviewed code shipped with the swarm — no filesystem surface, no
  traversal/symlinks to defend against, and load is **auto-approve**. An **opt-in,
  untrusted workspace source** (`.skills/`) is added by **§7a**; **remote** skills stay
  out of scope (§14).
- **Static allowed-name→path map**, built at composition from each agent's
  allowed-skills list. The `name` arg only **selects** from this closed set; it is
  **never interpolated** into a path.
- **Size limit (named, required):** `const maxSkillBytes = 64 * 1024` — a skill body
  over the limit is a fail-secure error (bounds context injection), enforced even
  though the file is embedded and reviewed.
- **Frontmatter parsing:** a **tiny stdlib parser** (split `---` fences, flat
  `key: value`) — no YAML dependency (CLAUDE.md). **Malformed or duplicate frontmatter
  → fail-secure** (typed `MalformedSkillError`, never a partial/ambiguous load). The
  parser is **fuzzed** (§12).
- `tools.SkillLoader` narrow interface (`Load(ctx, AgentName, name) (string, error)`);
  the concrete impl owns the `embed.FS` + the per-agent allow-map (and, when §7a is on,
  the workspace source behind it).

### 7a. Runtime (workspace) skills — opt-in, untrusted, Ask-gated

A human can enable a **second** skill source so the swarm also reads
`<workspaceRoot>/.skills/<name>/SKILL.md` — for project-local skills authored without a
rebuild. **Off by default** (embed-only). Workspace files are **untrusted** (anyone with
repo write controls them, and a loaded skill body becomes instructions in an agent's
context), so the controls below are *enforced*, not prompt-policy.

**Enablement & eligible agents.**
- **Human-enabled only (no self-authorization):** a launch flag / config
  (`--runtime-skills`, or `RuntimeSkills bool` on the swarm config). The model can never
  enable it (§2, §15).
- **`AllowsRuntimeSkills bool` on the `Agent` definition** (§6b) gates which agents may
  reach the source at all. **Only the read-only agents get it — `explorer` and
  `researcher`.** `operator`/`reviewer` (Bash-capable) and `orchestrator` do **not**: a
  Bash-capable agent can already inspect any file through its **human Bash gate**, so it
  needs no auto path to untrusted skills, and the orchestrator must stay delegate-only.

**Close the read-tool bypass (critical).** Auto-approved `ReadFile`/`Glob`/`Grep` would
otherwise let a runtime-capable agent read `.skills/<name>/SKILL.md` *directly*,
bypassing the Skill gate. So the SWE policy **hard-denies `.skills/**` for the generic
read/search tools and for generic writes** (`DefaultHardDeny` + SWE additions, §10): the
**only path for the generic *auto-approved* file tools** is the gated `Skill` tool, and no
agent can mint a skill by writing there via those tools (`Glob`/`Grep` results exclude
`.skills/**`). **Precise caveat (§10):** a Bash-capable agent can still touch `.skills/`
through its *human-gated* `Bash` — but operator/reviewer lack `AllowsRuntimeSkills`, so they
can never *load* a workspace skill into context via `Skill`; the Bash gate is the boundary
there.

**Load is Ask-gated via an `EffectChecker` (the enforced control).**
- The `Skill` tool installs an **`EffectChecker`** (`tools/permission.go:18`) that returns
  **Ask** for a *workspace* candidate, evaluated **before** the `HardApprove("Skill")`
  stage that auto-approves the embedded source. Embedded load = auto-approve; workspace
  load = a human gate.
- The request is a new **`tools.SkillLoadRequest`** whose `AllowedScopes()` is
  **`{ScopeOnce}` only** (like `UnknownRequest`, `permission_request.go:102`) — never
  session- or workspace-persisted; **every load re-prompts**.

**Bind the approved bytes — TOCTOU-safe (critical).** A workspace writer can swap
`SKILL.md` *or retarget a symlink on an intermediate dir* (`.skills/<name>`) between the
prompt and execution; `Clean`/`EvalSymlinks` at prompt time does not bind what runs, and
raw `O_NOFOLLOW` protects only the *last* component. So:
1. **Safe-root, descriptor-relative open of every component.** Open the workspace root
   once as an **`os.Root`** (the module is `go 1.26`; `os.Root` is 1.24+) and open
   `.skills/<name>/SKILL.md` *through it* — it resolves each component confined to the
   root, refusing symlink escapes and `..` on **intermediate dirs**, not just the final
   file. Then **validate a regular file** (`Mode().IsRegular()` — reject dir/device/fifo).
2. **Snapshot + hash once:** read a **bounded** (`maxSkillBytes`) snapshot from that
   descriptor and compute its **SHA-256**.
3. `SkillLoadRequest` carries **{relative path, agent, size, SHA-256}** (metadata only —
   never the body) for the human to approve.
4. **After approval, execute the snapshot — never re-open the path.**

**Prepared-call runner API (engine addition, P2b).** Binding the snapshot to the call
needs a new runner seam: today the runner mints `ToolExecutionID` into the per-call
`resolved` struct (`runner.go:newResolved`), but `EffectChecker`/`BuildRequest` get only
`argsJSON` — nowhere to stash an artifact both permission *and* execution read. Add an
optional **`Preparer`** tool interface `Prepare(ctx, callID, argsJSON) (PreparedArtifact,
error)`, invoked by the runner right after `newResolved` and stored on `resolved.prepared`.
Then: the **`EffectChecker`** returns **Ask** from the *name alone* (a workspace candidate
= a `name` absent from the embedded allow-map — derivable from `argsJSON`, matching today's
`CheckEffect(argsJSON)` signature, `tools/permission.go:23`); **`BuildRequest`** needs the
prepared **SHA-256**, so the runner threads `resolved.prepared` into the request-building
call site (today `buildRequest` passes only `argsJSON`, `runner.go`) to emit the
`SkillLoadRequest`; and **execution consumes the same `resolved.prepared`** (the snapshot
bytes). One artifact, keyed by `ToolExecutionID`, never a re-read. Embedded skills need no
`Prepare` (trusted, auto-approve).

**Name rules (strict).** Reject empty, `.`, `..`, either separator (`/` `\`), and control
characters; accept **only a bounded ASCII slug** (`[a-z0-9][a-z0-9_-]*`, length-capped).
Resolve through the `os.Root` safe-open above (descriptor-relative, not string `Clean`).
Path/name violations → typed `SkillContainmentError`.

**Embedded wins on name collision.** A workspace `.skills/<name>` can **never shadow** an
embedded skill of the same name (fail-secure — a workspace cannot hijack a trusted name).

**Discovery (sequencing).** Workspace skill *descriptions* are untrusted, so they are
**never** injected into the system-prompt `<available_skills>` catalog (the cached trusted
prefix lists embedded skills only). **P2b loads workspace skills by a name the model
already knows** (user-supplied or a known convention) — no auto-discovery of workspace
names in P2b. Surfacing workspace **names** (only, labeled untrusted) via the volatile
appended runtime layer is a **P3** follow-on (P2b precedes P3), or a later read-only
`ListSkills` *tool result* (never the system prompt).

**Persistence/fingerprint.** The runtime-skills mode **and a canonical workspace-root id**
are part of the config fingerprint, and `SkillLoadRequest` joins the sealed, persisted
`PermissionRequest` codec — see §16.

## 8. Salvage & deletion (updated to current reality)

| From | → | How |
|---|---|---|
| `agents/coding` wrapper/lifecycle (New/newWithClient/Close/Submit/Subscribe/Interrupt/gate-trio), `model`, `newHTTPClient`, `errors.go` | `agents/operator` | rename `Coding`→`Operator`, near-verbatim |
| `coding` `buildToolSet` | per-agent `buildToolSet`s | split by allowlist (§3): operator drops Fetch/WebSearch/Subagent; researcher = read+web; explorer = read-only; reviewer = read+Bash; orchestrator = read+Todo+Subagent |
| `coding/prompts` (Togo) | operator `<role>` + swarm `<identity>` | shared bits → identity; craft bits → operator role |
| `coding/spawner.go` (`codingSpawner`) | `swarms/swe` (`swarmSpawner`) | generalize to the typed `AgentRegistry`; **agent-aware `Spawn`** (§6b) |
| golden-set + `golden_set_test.go` + `eval_integration_test.go` | `agents/operator` | migrate; `internal/eval` untouched (keeps eval green) |
| `cmd/cli/main.go` + `Makefile`/`run` | split + migrate | runtime → `internal/cli`; launch → `cmd/swe`; **drop the registry + `AGENT=`** (§9) |
| ~~`agents/personal-assistant`~~ | — | **already deleted** (no action) |
| ~~`coding/subagent_factory.go`~~ | — | **already replaced** by `spawner.go` (no action) |

Deletion order keeps the build green: new wiring compiles + tests pass *before*
removing `agents/coding` and `cmd/cli`.

## 9. CLI + build/run contract

- `cmd/swe/main.go` thin: `internal/cli.Run(ctx, swe.New, banner{Name:"SWE"})`.
- `internal/cli` = extracted runtime (`~/.urvi/urvi.log` + slog, `signal.NotifyContext`,
  `ttylog` stdio capture, `tea.Program` run/teardown/exit codes).
- One swarm (one primary = orchestrator) per binary → **remove** the still-present
  CLI agent-selection registry: `defaultAgent`, `agentName`, `buildRegistry`,
  `agentDescriptions`, `agentDisplayNames` (`cmd/cli/main.go`).
- **Build/run migration (not just source moves):**
  - `Makefile:14` `build:` → `CGO_ENABLED=0 go build -trimpath -o bin/swe ./cmd/swe`
    (binary `bin/urvi`→`bin/swe`; *name is the maintainer's call — flag for review*).
  - `Makefile:20` `run:` → `go run ./cmd/swe` (**drop `AGENT=`** — one primary now;
    keep the `.env` load).
  - Sweep any installer/CI/docs referencing `cmd/cli`, `bin/urvi`, or `AGENT=`.

## 10. Security

- **Per-agent tool allowlist (the primary, runtime-enforced boundary):**
  construction-time ToolSet registry + the permission gate. Binary-per-swarm (§4) is
  *secondary* composition isolation, not the control to rely on.
- Deny-by-default hard-deny retained; **resource caps re-added** (depth + cumulative
  spawn quota, §6d) — closes the unbounded-recursion/idle-loop regression, made
  sharper by `Subagent` being auto-approved.
- **Approval scopes — corrected claim.** A privileged action (Write/Edit/Bash/Fetch/
  WebSearch) is **initially gated, then subject to the granted scope** — *not* "always
  per-call." Two stores, two behaviors:
  - **Session-scope** grants live on the per-loop `PermissionChecker` (fresh per
    spawn) → **per-agent isolated**: a grant in one agent never applies to another.
  - **Persisted** workspace/user grants (`ApprovalRecord{Tool, Match, …}`,
    `tools/permission.go:59`) have **no agent dimension** → **cross-agent by design**:
    a `ScopeWorkspace` `Bash` grant for a specific `(tool, match)` is honored by *any*
    agent holding `Bash`. **Decision: accepted for v1** — it means "this exact command/
    path is safe in this workspace," not "this agent may run it." Residual risk: an
    agent acting on poisoned research output (§ untrusted-data) could re-invoke an
    already-approved `(tool, match)`; mitigated by the match being specific and
    human-approved. Per-agent persisted scoping is a future enhancement. **Pinned by a
    test** (§12).
- (P2) Skills, two sources: **embedded = trusted** (`embed.FS`, auto-approve,
  static name-map, `maxSkillBytes`, fail-secure parse, §7); **workspace `.skills/` =
  untrusted, opt-in, Ask-gated per load** (§7a) — the gate is the *enforced* control for
  untrusted skill content (`SkillLoadRequest`, `ScopeOnce` only, SHA-256-bound snapshot),
  embedded names always win collisions, and only agents with `AllowsRuntimeSkills`
  (explorer, researcher) can reach the workspace source at all.
- **`.skills/**` is hard-denied for the generic *auto-approved* file tools and writes
  (§7a).** The SWE policy adds `.skills/**` to `DefaultHardDeny`'s read+write sets
  (`tools/permission.go:125`), so auto-approved `ReadFile`/`Glob`/`Grep` cannot bypass the
  `Skill` gate and no agent can mint a skill by writing there via those tools — the gated
  `Skill` tool is the only path **for the generic file tools**. **Precise caveat:** a
  Bash-capable agent (operator/reviewer) can still `cat`/create `.skills/` files **through
  its human-gated `Bash`**, but those agents lack `AllowsRuntimeSkills` so they can never
  *load* a workspace skill into context via `Skill`; the human Bash gate is the boundary.
  Acceptable, stated precisely.
- **Untrusted-data handling — a model-behavior mitigation, NOT an enforced guarantee.**
  researcher is least-privileged (read+net, no write/exec) so it cannot itself be a
  confused deputy, but its report is consumed by privileged agents, so poisoned web
  content can influence them (indirect prompt injection). The operational contract
  (researcher labels fetched content as *data*; the orchestrator treats subagent
  reports as *data, not instructions*; operator/reviewer don't follow instructions
  sourced from research) is **prompt policy the model is asked to follow — it cannot be
  architecturally guaranteed.** The only *enforced* controls are (a) each agent's
  ToolSet boundary and (b) the human approval gate. **Residual risk persists:** a
  previously **scoped** persisted approval (`(tool, match)`, above) can be re-invoked
  without a fresh prompt — including by an agent acting on poisoned input — bounded only
  by the specificity of that approved match.
- No self-authorization: a model cannot mint an agent/boundary; identity is a trusted
  in-repo constant.

## 11. Errors (typed)

- `tools.UnknownAgentError` — unknown/unauthorized `agent` (fail-secure tool-result string).
- `session.SessionError` kinds `SessionLoopDepthExceeded` / `SessionLoopQuotaExceeded`
  for the §6d caps — typed, created-nothing, like `SessionClosing`.
- (P2) `tools.UnknownSkillError` / `tools.MalformedSkillError` — unknown/unauthorized
  or malformed/duplicate-frontmatter skill.
- (P2, §7a) `tools.SkillContainmentError` — a workspace-skill path escaping `.skills/`
  (traversal/symlink) or violating the ASCII-slug name rule.
- (P2, §7a) `tools.SkillLoadRequest` — the `ScopeOnce`-only `PermissionRequest` for a
  workspace-skill load (carries relative path / agent / size / SHA-256; never the body);
  joins the sealed, persisted codec (§16).
- All tool failures → tool-result error strings (`InvokableRun` never returns a Go
  error — existing pattern).

## 12. Testing

- Table-driven, `-race`. Per-agent: `buildToolSet` yields **exactly** the intended
  allowlist (incl. auto-approve set; incl. Skill in P2).
- Orchestrator can spawn only its four; leaves have no `Subagent` tool (can't spawn).
- Agent-aware `Spawn`: unknown/unauthorized agent → fail-secure; permitted agent →
  fresh cfg with the agent's ToolSet (assert the spawned loop's tools).
- **Caps (reservation/rollback, §6d):** depth-N reject precedes construction (creates
  nothing); quota-M reject emits **no `LoopStarted`**; **concurrent** spawns never push
  `spawned` over M (`-race`); a **rolled-back** spawn (forced `loop.New`/publish
  failure) **releases** the reservation (`spawned` is restored).
- **Persisted approval across agent boundaries:** a `ScopeWorkspace` `(tool, match)`
  grant made under agent A **is** honored when agent B (also holding the tool) issues
  the same `(tool, match)`; a **session-scope** grant under A is **not** seen by B
  (pins the §10 decision).
- Bridge (reuse landed coverage): a subagent loop's gates/AskUser surface attributed
  by `LoopID` and route back via `Approve(loopID, …)`; token firehose muted.
- **AgentName attribution:** `LoopStarted` carries the spawned agent's `AgentName`
  (primary = `"orchestrator"`).
- (Optional, §5a) the opening greeting renders deterministically from the registry,
  emits no command/turn, and leaves `state.msgs` empty.
- **Fuzz (CLAUDE.md — parsers of external input):** the `Subagent` argument decoder
  (`FuzzSubagentArgs`) and (P2) the skill frontmatter parser (`FuzzSkillFrontmatter`).
- (P2) SkillLoader (embed.FS): embedded load succeeds; unknown/unauthorized →
  fail-secure; body over `maxSkillBytes` and malformed/duplicate frontmatter → typed
  error; catalog lists only allowed skills.
- (P2b, §7a) Workspace skills: **read-tool bypass blocked** — `ReadFile`/`Glob`/`Grep`
  and writes on `.skills/**` are hard-denied; only the `Skill` tool reaches it. **TOCTOU /
  safe traversal** — a file swap **or an intermediate-dir symlink retarget** between prompt
  and exec yields the **approved SHA-256 snapshot** (mismatch → fail-secure); `os.Root`
  rejects symlink/`..` escape on any component and a non-regular target; the `Preparer`
  binds **one** artifact so the permission-time hash == the executed bytes (keyed by
  `ToolExecutionID`). **Name table** — empty/`.`/`..`/`/`/`\`/control/over-length →
  `SkillContainmentError`. `SkillLoadRequest.AllowedScopes() == {ScopeOnce}`. Only
  `explorer`/`researcher` may load; embedded wins a name collision. Tagged FS integration
  for the on-disk path.
- Migrated golden-set + `eval_integration_test.go` (`//go:build integration`).
- Whole-tree `go test -race ./...` + `make secure` (+ `make fuzz`).

## 13. Phasing

- **P1** — `agents/{5 agents}` (allowlists **minus Skill**) + per-agent `buildToolSet` +
  typed `AgentRegistry`/`AgentName` (§6b) + `swarms/swe` (identity injection,
  orchestrator-as-primary) + agent-aware `Subagent` arg + `swarmSpawner` +
  **`WithLimits` option + depth/quota caps** (§6d) + **`AgentName` on `loop.Config`/
  `LoopStarted`** (§6f) + `cmd/swe` + `internal/cli` + **Makefile/run migration** (§9) +
  coding→operator salvage + delete `agents/coding` & `cmd/cli`. XML identity/role
  prompts. Build green, eval green. *(No bridge work — it's done.)* Optional greeting
  (§5a) and TUI labels (§6f) may land here or be deferred — they gate nothing.
- **P2** — `Skill` tool + `SkillLoader` (embedded default + per-agent allow-map +
  `maxSkillBytes`, §7) + catalog injection + wiring `Skill` into each agent's allowlist
  + fuzz + unit tests.
- **P2b (opt-in, §7a)** — runtime/workspace `.skills/` source: `.skills/**` HardDeny for
  generic read/search/write, **`Preparer` runner seam** (`resolved.prepared`) +
  **`os.Root` safe traversal** + regular-file check, `EffectChecker`-driven Ask gate,
  `SkillLoadRequest` (`ScopeOnce`, SHA-256 snapshot, `ToolExecutionID`-keyed prepared call),
  strict ASCII-slug names, per-agent `AllowsRuntimeSkills` (explorer + researcher),
  embedded-wins collision, fingerprint additions (§16) + **tagged FS integration tests**
  (this source *does* reintroduce an FS surface).
- **P3** — runtime/env context injection in the session engine (appended turn message,
  append-to-update). New engine capability; orthogonal to the agent packages.

## 14. Out of scope (v1)

Asynchronous (cross-turn handle) subagents and inter-subagent messaging
(`wait`/`send`/`interrupt`) — the loop-targeted `Interrupt` primitive exists and is
the shared lever they will reuse, but the handle model is deferred (loop-machine §7,
in-session-subagents §7). Skill args/parameters, **remote** skill sources, multiple
primaries per binary, per-agent persisted-approval scoping (§10), loop teardown/reuse
and the concurrent-active breadth gauge (§6d). *(Workspace `.skills/` is now **in scope**
as an opt-in, Ask-gated source — §7a; embedded stays the default.)* **In scope:**
streaming subagent events to the
UI (free), and **concurrency** — multiple subagents via parallel tool calls (fork-join
within a turn), bounded by the loop's `MaxParallelToolCalls` and the §6d caps.

## 15. Dynamic subagents — in scope vs deferred

- **Dynamic task** — IN. The `message` handed to any subagent is model-defined.
- **Dynamic agent selection** — IN. The model picks the `agent` from its catalog per spawn.
- **Dynamic behavior** — IN (P2). Skills are embedded `SKILL.md` content; an **opt-in
  workspace `.skills/`** source adds runtime-authored skills (untrusted, Ask-gated, §7a).
- **Dynamic boundary by the model** — NOT supported (no self-authorization); an
  agent's toolset is reviewed, compiled Go.
- **Generic catch-all agent** — DEFERRED. The four leaves cover the privilege tiers;
  trivial to add a fifth `general` leaf if a real gap appears.
- **File-defined agent boundaries** — DEFERRED behind a future security review (an
  editable `tools:` file is a privilege-escalation surface).
- **Fork / async / persistent agent teams** — DEFERRED. Note: persistent teams are
  *close* — in-session loops are **never deleted** and persist idle with history
  (in-session-subagents §8), so "route a follow-up back to the same idle agent loop by
  identity" is an additive follow-on (loop addressing at the tool boundary), not a
  rebuild.

## 16. Persistence integration (event-persistence-checkpoint)

Coordinates with `docs/plans/2026-06-19-event-persistence-checkpoint-design.md` — now
**implemented** on worktree `design/event-persistence-checkpoint` (v1 = local CLI,
**primary-loop** restore). **Verified against that code (2026-06-21):** the four core
fingerprint fields, the primary-only `EventReplayer`, the derived `SessionMeta.LoopCount`,
the additive event codec (`schemaVersion = 1`), and the build-before-lock `NewLoop`
rollback all exist as the spec assumes. The five swarm-specific deltas:

1. **`AgentName` is durable on `LoopStarted` (additive codec field).** `LoopStarted` is
   Enduring and marshaled via `marshalPlain` (`event/marshal.go`), so adding `AgentName`
   to `event.Header`/`LoopStarted` serializes **additively** under the existing
   `schemaVersion = 1` — no codec case to write. On **restore**, validate the **root**
   loop's `AgentName == the configured primary` (orchestrator); mismatch is fail-secure.
   **Pre-`AgentName` records** decode with an **empty** name (Header already uses
   `omitzero`) and are routed through the existing **config-fingerprint compatibility
   path** (`ConfigMismatchError` → reject / confirm) — never silently treated as
   orchestrator.
2. **The caps option flows through both construction seams.** §6d's `WithLimits` is an
   `Option`, so it joins the existing `opts ...Option` chain on **both**
   `New(ctx, cfg, opts...)` *and* `Restore(ctx, cfg, sessionID, js, objectStore, leases,
   opts...)` (the real signatures — there is no positional session-config). Both startup
   and restore pass the swarm's limits; an absent option = fail-secure defaults.
3. **The cumulative spawn quota survives restore (the load-bearing one).** The quota is
   per-`SessionID`, and restore **reuses the original `SessionID`** — so initializing
   `spawned = 0` on restore would grant a fresh **M** children **after every restart** (a
   trivial cap bypass). Fix: **recompute `spawned` by counting durable non-root
   `LoopStarted` events** (non-zero `Header.Cause` = a subagent spawn) **before bringing
   the primary up**. That equals the live `spawned` because `LoopStarted` is emitted
   **only on a successful spawn** (rejected/rolled-back spawns emit none, §6d). **Do not**
   use `SessionMeta.LoopCount` — the catalog is a *derived, rebuildable cache, explicitly
   stale-able* (persistence §Catalog), not a source of truth. Since the v1
   `EventReplayer` selects only the **primary** loop's subject, add a small **all-loop
   `LoopStarted` metadata scan** (or restore-state reader) that counts across loop
   subjects.
4. **The config fingerprint distinguishes the swarm.** The real struct is
   `event.ConfigFingerprint{AgentKind, ModelID, SystemPromptRev, ToolPolicyRev}` computed
   by `session.FingerprintFrom(cfg loop.Config)` — and **`AgentKind` is currently left
   empty** ("until the agent threads it through"), so step one is to **populate it** with
   the swarm + primary-agent identity (orchestrator). Then extend the struct +
   `FingerprintFrom` with the **resource-cap policy (N, M), the runtime-skills mode, and a
   canonical workspace-root identifier (§7a)** — the runtime-skills flag alone does **not**
   distinguish two repositories' `.skills/` dirs; the canonical root id does. So a `coding`
   session cannot silently resume as SWE/orchestrator (different prompt + delegation), nor
   under different caps, skill-trust mode, or a *different repo's* skills; mismatch hits the
   existing `ConfigMismatchError` reject-or-confirm path (`WithAllowConfigMismatch()` to
   override).
5. **`SkillLoadRequest` joins the sealed `PermissionRequest` codec (§7a).** `PermissionRequested`
   is Enduring → persisted, so the persistence design must add `SkillLoadRequest` to its
   sealed request codec, serializing the **safe metadata + SHA-256, never the skill body**.
   Per-load `ScopeOnce` gating means the workspace skill **contents** need no
   config-fingerprint hash of their own (every load re-prompts regardless).

**Already aligned (no change):** subagent loops are **not** restored in v1 (primary-only;
their results are already folded into the parent via `SubagentResult` →
`TurnFoldedInto`/`StepDone`), and **session-scope permission approvals are not persisted**
→ restore re-prompts (fail-secure) — both consistent with §6e (bridge) and §10 (per-loop
session-scope isolation).
