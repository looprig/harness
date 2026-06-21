# SWE Swarm — Multi-Agent Roles + Skill Tool (Design)

- **Date:** 2026-06-16
- **Branch:** feature/togo-eval-framework
- **Status:** Approved (brainstorming); pending implementation plan (writing-plans)

## 1. Goal

Replace the single `coding` agent with a **swarm of five role-bounded agents**
(orchestrator, researcher, explorer, operator, reviewer), add a **Skill**
capability system, and ship the swarm as its **own binary**. Delete the existing
`coding` and `personal-assistant` agents, salvaging their reusable parts.

## 2. Core model

- **Agent = a boundary** (tools + permissions + model), human-defined and reusable. Lives in `agents/<name>/`.
- **Skill = behavior/knowledge** (instructions/scripts), loaded on demand. It **cannot** change an agent's tools or model. Lives in `skills/<name>/SKILL.md`.
- **Swarm = a composition** that wires agents into a team and injects identity. Lives in `swarms/<name>/`.
- **Runtime/env context** (date/cwd/git) = a volatile layer owned by the **session engine**, appended per turn — never baked into the system prompt (caching).
- **Subagent = the spawn entity** (noun). **Task = the message** handed to it. A model issues tasks against *allowed* subagents; it can **never mint a new agent** (no self-authorization). A new boundary = a human adds an agent package.

## 3. Agents & tool allowlists

| Agent | Tools | Spawns |
|---|---|---|
| researcher | Glob, Grep, ReadFile, WebSearch, Fetch, AskUser, Skill | — |
| explorer | Glob, Grep, ReadFile, AskUser, Skill | — |
| operator | Glob, Grep, ReadFile, WriteFile, EditFile, Bash, Todo, AskUser, Skill | — |
| reviewer | Glob, Grep, ReadFile, Bash, Todo, AskUser, Skill | — |
| orchestrator | Glob, Grep, ReadFile, Todo, AskUser, **Subagent**, Skill | researcher, explorer, operator, reviewer |

Each allowlist is wired at construction — a tool the agent lacks is literally
absent from its registry (least privilege by construction). A **fresh
PermissionChecker per agent isolates only session-scope (in-memory) grants** —
persisted workspace/user approvals are global by `(tool, match)` with no agent
field (see §10), so e.g. a workspace-scoped `Bash` approval applies to any agent
holding `Bash`. **`Skill` is wired in P2** (see §13); P1 agents carry the same
allowlists minus `Skill`. Privilege lattice: explorer (read) ⊂ researcher
(+network); reviewer (+exec); operator (+write+exec); orchestrator (delegate-only).

**Every agent has AskUser.** A spawned subagent is NOT headless: it is driven via
the event stream, so its AskUser (and its permission gates) forward to the human
and the answer/approval is routed back to that specific subagent — it behaves
exactly like the root agent (see §6a).

**Auto-approve per agent.** Auto-approve is **Stage 4 of the permission `Check`, not
a bypass** — containment + hard-deny (Stages 1–2) are non-bypassable and run first,
so even an auto-approved `ReadFile` cannot read a denied secret path. Read/search
(ReadFile/Glob/Grep), Todo, Skill, Subagent, and AskUser auto-approve (they run
without a prompt — AskUser's "prompt" IS its purpose). WriteFile/EditFile/Bash/
Fetch/WebSearch stay **Ask**: the human approves each call (fail-secure). Every
Ask-gate tool lives in a leaf (operator write/exec, researcher network, reviewer
exec), so those gates are forwarded to the user by the bridge — see §6a. Explorer
holds only auto-approve tools, so it never gates.

## 4. Package layout

```
agents/                    reusable agent packages, each its own boundary
  researcher/  explorer/  operator/  reviewer/  orchestrator/
swarms/
  swe/swarm.go             imports the 5 agents, wires the orchestrator's roster,
                           injects identity, exposes swe.New(ctx) -> tui.Agent
cmd/
  swe/main.go              thin: internal/cli.Run(ctx, swe.New, banner{Name:"SWE"})
internal/
  cli/                     shared CLI runtime extracted from cmd/cli/main.go
tui/                       unchanged (UI layer)
tools/                     + the new Skill tool (shared)
internal/eval/             unchanged; golden-set migrates into agents/operator
deleted: agents/coding, agents/personal-assistant, cmd/cli
```

Future swarm = new `swarms/<name>/` + `cmd/<name>/`; the `swe` binary never
links it (compile-time isolation between swarms).

## 5. Prompt architecture (XML)

Three layers, by owner and volatility:

| Layer | Owner | Placement | Volatility |
|---|---|---|---|
| identity | swarm | system prompt | stable (cached) |
| role | agent | system prompt | stable (cached) |
| runtime/env | session engine | appended turn message | volatile (append/diff) |

- **identity** (swarm-injected, one block, `SWE`) carries the cross-cutting bits every agent shares (persona/tone, persistence, security/secrets, reversibility):
  ```xml
  <identity product="SWE">
    <summary>You are part of SWE, an interactive tool for software engineering tasks.</summary>
    <personality>Concise, direct, friendly. Actionable over verbose.</personality>
    <persistence>Keep going until the request is fully resolved. Never guess or fabricate.</persistence>
    <security>Never read, display, or transmit secrets/PII. Note presence, never values.</security>
    <reversibility>Local/reversible actions freely; check first for hard-to-reverse or shared-system actions.</reversibility>
  </identity>
  ```
- **role** (per agent, identity-free) carries the agent's job + role-specific guidelines, e.g.:
  ```xml
  <role name="operator">
    <objective>Implement tasks through tools: fix bugs, add features, refactor.</objective>
    <guidelines>
      <item>Fix at root cause; match existing style; read a file before editing; prefer editing to creating.</item>
      <item>WriteFile/EditFile/Bash require approval — state your plan before each change.</item>
      <item>Validate narrowest test first, then broaden; don't fix unrelated failures, mention them.</item>
    </guidelines>
  </role>
  ```
  researcher/explorer/reviewer/orchestrator follow the same shape (read-only +
  cite; read-only map; critique-don't-fix + may run tests; decompose + delegate).
- **Composition:** `agent.New(ctx, identity string, …)` builds `systemPrompt = identity + role`. Each agent's `<role>` XML lives in `agents/<agent>/system.go` (the operator's is salvaged from `coding/prompts/system.go`). Identity is a **trusted in-repo constant** (no external/user data in prompts — CLAUDE.md compliant). A future `cmd/<agent>` injects its own identity; the agent never changes.
- **Naming:** `product` (identity) vs `name` (role) — exactly one notion of "name" per scope, no collision.
- **Caching:** the system prompt (identity + role + skill catalog) is the stable, cached prefix. Runtime context is appended at the tail and **updated by appending a fresh block (or diff), never by editing the prior one** — editing the cached prefix invalidates everything after it. (Observed model: Codex appends env diffs; Claude Code appends a "date has changed" note rather than rewriting the original.)

## 6. Subagent mechanism

- Rewire `tools/subagent.go`: selector `skill` → `agent`; args become **`{agent, task}`** (untangles today's overload where "skill" selected the child persona; `task` = the work, per §2's naming).
- `orchestrator.New(ctx, identity, factory tools.SubagentFactory)` owns the `Subagent` tool but takes its spawnable set as an **injected dependency**.
- `swarms/swe/swarm.go` builds the factory (`agentName → leaf constructor`) from the four leaf packages and injects identity into each. It is the **only** place the five are coupled.
- `agents/orchestrator` must **not** import the leaf packages (reusability + no import cycles).
- **Subagent catalog.** Each agent package exposes a `Name` + `Description`. The factory's `Available() []{Name, Description}` feeds an `<available_subagents>` listing rendered into the `Subagent` tool's description, so the orchestrator knows which agents it may spawn and what each does (same pattern as the skill catalog; cf. OpenCode `describeTask`). Only the caller's permitted agents are listed.
- Recursion: leaves have no `Subagent` tool, so they can't spawn (natural depth-1); the existing hard depth cap (2) stays as a backstop.
- **Per-child least privilege:** each spawned child gets only its own package's allowlist (improvement over today's behavior, where children inherit the parent's full tool set).

### 6a. Subagent event bridge (required)

Today `childSubsession.Invoke` drives a child via the **blocking** `session.Invoke`,
which returns only on terminal events and **ignores permission-gate and AskUser
events** — and nothing calls `Approve`/`ProvideUserInput` on the child. So a child
that calls an Ask-tool (operator Write/Edit/Bash, researcher Fetch/WebSearch,
reviewer Bash) or AskUser currently **parks and hangs**. The swarm cannot do any
gated action until this is fixed.

P1 therefore drives children via **`session.Stream`**, so a subagent behaves exactly
like the root agent for permissions and AskUser:
- **Up:** forward each child's events (tool calls, token deltas, permission gates, AskUser) to the user-facing stream, tagged by originating subagent — this also makes subagent activity **visible/streamed on the CLI**.
- **Down:** route the user's `Approve`/`Deny`/`ProvideUserInput` to **that specific child** session, by (session, callID), via the child session's existing methods.

**Concurrent subagents.** Multiple subagents can run at once (parallel tool calls,
bounded by the loop's `MaxParallelToolCalls`) — fork-join within a turn. Each
independently raises its own gates/AskUser; the bridge multiplexes them to the
single human (queued/serialized in the UI) and routes each answer back to the right
child. Per-call human approval (fail-secure) is preserved. Explorer is gate-free
(read-only).

**Implication:** the `Subagent` tool is therefore **engine-integrated** — it emits
nested events into the parent stream and receives routed user responses — not a
plain leaf `InvokableTool`. The implementation plan must account for this.

### 6b. Bridge mechanism — event sink up, `sessionID` routing down

The bridge is two **decoupled** paths, and **neither needs per-`callID`
bookkeeping** — a `callID` never leaves its owning session's namespace.

- **Up (visibility + surfacing):** the `Subagent` tool drives the child via
  `child.Stream` and forwards each child event onto its own session's event
  stream, **tagged with the child's `sessionID`** (and name). Each level forwards
  to its parent, so everything recurses into the root stream the TUI already
  consumes — the human sees the child's tool calls, tokens, permission gates, and
  AskUser, attributed to the originating subagent.
- **Down (approvals/answers):** the bridge tags each surfaced gate/AskUser with its
  `sessionID` (see §6c — bare stream events do **not** carry it today), so the TUI
  can echo `(sessionID, callID)` back. The runtime looks the session up in a single
  flat **`sessionID → *AgentSession` registry** and calls that session's existing
  `Approve(callID)` / `Deny(callID)` / `ProvideUserInput(callID)`. The session's own
  loop resolves `callID` internally (unchanged) — there is **no** router-side
  `callID` map.

| Concern | Mechanism |
|---|---|
| Find the right subagent | `sessionID → *AgentSession` registry (= the live-children set) |
| Find the right gate within it | the session's own loop, by `callID` (unchanged) |
| Know which subagent a gate is from | event tagged with `sessionID` on the up path |

The `sessionID → session` registry is populated when a subagent is spawned and
removed at its teardown — it doubles as the live-children/lifecycle set, so it is
not extra bookkeeping.

**Interface:** responses are **addressed by `sessionID`** — e.g.
`Approve(sessionID, callID, scope)`. The root agent's own gates use the root's
`sessionID`, so the path is uniform for root and subagents alike.

**Fail-secure:** an unknown `sessionID` finds no session (no-op); an unknown
`callID` no-ops at the target session's loop. `callID`s need no global uniqueness
because they are only ever interpreted within their owning session.

**Engine hooks (for the plan):** (1) give the `Subagent` tool an emitter onto its
session's event stream (a plain `InvokableTool` only returns a `ToolResult`); (2) a
small **session-group runtime** holds the `sessionID → session` registry + the
merged stream, wired by the swarm into the factory (children publish + register on
spawn, deregister on teardown) and into the root agent (its Approve/Deny/
ProvideAnswer become `sessionID`-addressed). All engine-level (`internal/agent`),
not in the `agents/*` packages. writing-plans pins the exact types.

### 6c. Required engine/TUI changes (verified against current code)

The bridge needs three changes the current code does **not** yet support — flagged
so writing-plans treats them as first-class P1 tasks:

1. **`sessionID` on the stream path.** Bare turn-stream events carry only `CallID`
   (`event/tool.go`). `SessionID` lives **only** on the sink `EventEnvelope`
   (`event/sink.go`: "live exclusively on the envelope, never on the bare event
   structs"), which the TUI's `StreamReader[event.Event]` never sees
   (`session/agent.go` `Stream`). The up path must add `SessionID` to the stream —
   envelope the stream events, or carry it on the gate/AskUser events.
2. **Session-aware TUI surface.** `tui.Agent` is callID-only today (`tui/agent.go`:
   `Approve(callID, scope)` / `Deny(callID)` / `ProvideAnswer(callID, …)`). These
   become `sessionID`-addressed, and the agent wrapper routes to the owning child
   via the §6b registry.
3. **Session-scoped terminal handling.** The TUI clears **all** pending prompts on
   **any** terminal event (`tui/interaction.go` `ApplyEvent` → `ClearPrompts`). With
   merged child streams, one subagent finishing would drop a *sibling's* pending
   prompt. Terminal handling must be scoped to the finishing `sessionID`, and the
   prompt queue partitioned by session.

### 6d. TUI transcript & rendering (verified gap — bigger than routing)

Reading the `sessionID` is necessary but **not sufficient** to "roll" a child's
events under its subagent: the transcript reducer is built for a **single session**.
`transcriptModel` holds one `live liveSeg` and a flat `committed []entry` with **no
session field** (`transcript.go`); `TokenDelta` accumulates into the one `live.Text`,
tool cards match by `CallID` within the one `live.Calls`, and `TurnDone` resets that
shared segment (`commitLive`). `Screen` consumes a **single** stream reader and folds
every event into that one reducer (`screen.go` `handleEvent`). Merging concurrent
subagent streams into it would **interleave narration into one blob, cross-mix tool
cards, and let one child's `TurnDone` reset another child's in-progress segment** —
garbled output, not attribution.

So attribution needs more than `sessionID` on the event:

- **Option A (faithful, larger):** make the transcript session-keyed — a `live`
  segment per `sessionID` and a session tag on each committed `entry` — and teach
  the renderer to nest/label entries under their subagent. Faithful, but a big change
  and noisy when several subagents stream at once.
- **Option B (v1-pragmatic, chosen):** do **not** fold child token streams into the
  root transcript. The bridge surfaces only what needs the human or the record: child
  **permission gates + AskUser** as *attributed* prompt records (add a subagent tag to
  `promptContext`/`entry`), plus a compact attributed "▸ <subagent>: running/done"
  line (optionally its tool cards). The child's full token narration is
  summarized/collapsed, not interleaved. This bounds the transcript change to a
  session tag + one compact entry kind and avoids N-stream interleaving.

P1 takes **Option B**; Option A is a later enhancement. Either way `entry` /
`promptContext` gain a subagent-attribution field and `renderEntry` renders it.

## 7. Skill tool + loader

- `tools.Skill`: param `name`; resolves against *this agent's* allowed-skill set; reads the skill's `SKILL.md` body; returns it as the tool result. Fail-secure on unknown/unauthorized (tool-result error string). `Auditable` (logs the skill name only). AutoApprove (a scoped, side-effect-free read — same class as ReadFile/Subagent).
- `tools.SkillLoader` narrow interface (`Load(ctx, name) (string, error)`); concrete impl reads `skills/<name>/SKILL.md`, **scoped per-agent** by each package's allowed-skills list.
- **Catalog injection:** when an agent has ≥1 allowed skill, its system prompt carries an `<available_skills>` listing (name + description) — progressive-disclosure tier 1; the body loads only on the tool call (tier 2). An agent with zero skills gets no catalog (and no Skill tool wired).
- On disk: `skills/<name>/SKILL.md` — a `---`-fenced frontmatter block + markdown body (Claude Code-compatible *shape*). Parsing uses a **tiny stdlib parser** (split the `---` fences, read flat `key: value` lines for `name`/`description`) — **no external YAML dependency** (CLAUDE.md: stdlib-first; a YAML package would need explicit approval). Richer/nested frontmatter is out of scope until such approval.
- Per-agent allowed skills are declared in each agent package (part of its boundary).

## 8. Salvage & deletion

| From | → | How |
|---|---|---|
| `coding` wrapper + lifecycle (New/newWithClient/Close/Stream/gate-trio), `model`, `newHTTPClient`, `errors.go` | operator | rename `Coding`→`Operator`, near-verbatim |
| `coding` `buildToolSet` | operator | drop Fetch/WebSearch/Subagent, add Skill; `autoApprovedTools` drops Subagent |
| `coding/prompts/system.go` (Togo) | operator role XML + swarm identity | shared bits → `<identity>`; craft bits → operator `<role>` |
| `coding/subagent_factory.go` (+ recursion/depth/lifetime machinery) | swarm | generalize `personas` (skill→spec) into `agentName→leaf` |
| golden-set + `golden_set_test.go` + `eval_integration_test.go` | agents/operator | migrate; `internal/eval` untouched (keeps eval green) |
| `personal-assistant` read-only `buildToolSet` shape | template for researcher/explorer/reviewer | reference while building; then delete the package |
| `cmd/cli/main.go` | split | runtime → `internal/cli`; launch → `cmd/swe` |

Deletion order keeps the build green: old packages (`coding`,
`personal-assistant`, `cmd/cli`) are removed only after the new wiring compiles
and tests pass.

## 9. CLI

- `cmd/swe/main.go` is thin: `internal/cli.Run(ctx, swe.New, banner{Name:"SWE"})`.
- `internal/cli` is the extracted runtime: `~/.urvi/urvi.log` + slog, `signal.NotifyContext`, `ttylog` stdio capture, `tea.Program` run + teardown + exit codes.
- One orchestrator per binary → the CLI-level agent-name selection registry is removed (`defaultAgent`, `agentName`, `buildRegistry`, `agentDescriptions`, `agentDisplayNames`).

## 10. Security

- Per-agent tool allowlist (construction-time, runtime-enforced); per-binary isolation between swarms.
- Deny-by-default hard-deny rules retained; recursion cap retained.
- SkillLoader scoped per-agent + fail-secure on unknown/unauthorized.
- researcher is the least-privileged despite touching the most untrusted data (web): no write/exec, so it cannot itself be a confused deputy. **But its report is consumed by privileged agents**, so poisoned web content can still influence the orchestrator/operator indirectly (indirect prompt injection).
- **Untrusted-data contract:** researcher labels fetched web content as untrusted *data*; the orchestrator treats every subagent report as *data, not instructions*; operator and reviewer never execute or follow instructions sourced from research output. A privileged action always requires the human approval gate (§6a), regardless of what a report says.
- **Persisted-approval scope:** workspace/user approvals are keyed by `(tool, match)` with no agent dimension — intentionally a "this command is safe in this workspace" statement, not "this agent may run it." So a `ScopeWorkspace` grant crosses agents. Per-agent persisted scoping is a possible future enhancement, not v1.
- A model cannot self-authorize (no minting agents/boundaries); identity is a trusted in-repo constant.

## 11. Errors (typed)

- `registry.UnknownNameError` for an unknown subagent.
- A new typed `UnknownSkillError` for unknown/unauthorized skill.
- All tool failures → tool-result error strings (existing pattern; `InvokableRun` never returns a Go error).

## 12. Testing

- Table-driven, `-race`. Per-agent: `buildToolSet` yields exactly the intended allowlist (incl. Skill).
- Orchestrator can spawn only its four; leaves cannot spawn.
- SkillLoader denies unknown/unauthorized; catalog lists only allowed skills.
- Fakes for `SubagentFactory`, `SkillLoader`, and child sessions.
- Migrated golden-set + `eval_integration_test.go` (build tag `integration`).

## 13. Phasing

- **P1** — `agents/{5}` (allowlists **minus `Skill`**) + `swarms/swe` + `cmd/swe` + `internal/cli` + subagent rewire (`skill`→`agent`) + the **§6a–§6d bridge** (Stream-driven children, `sessionID` on the stream path, session-aware TUI + `Approve(sessionID,…)`, session-scoped terminal handling, subagent-attributed transcript via Option B) + coding→operator salvage + delete `coding`/`personal-assistant`/`cmd/cli`. Build green, eval green. XML identity/role prompts.
- **P2** — `Skill` tool + `SkillLoader` + catalog injection, **and wiring `Skill` into every agent's allowlist + tests** (the first point `Skill` appears in the build).
- **P3** — runtime/env context injection in the session engine (appended turn message, append-to-update). urvi injects none today; this is a new engine capability, orthogonal to the agent packages.

## 14. Out of scope (v1)

**Asynchronous (non-blocking, handle-based) subagents** and inter-subagent
messaging (Codex-style `wait_agent`/`send_message`/`interrupt`), skill
args/parameters, remote skills, and multiple primaries per binary. Also deferred:
a generic catch-all agent and file-defined agents — see §15. **Note:** streaming a
child's events to the UI is **in scope** (§6a). So is **concurrency** — multiple
subagents via parallel tool calls (fork-join within a turn). "Async" here means only
the **cross-turn handle model** (spawn returns immediately; the child runs across
turns and is steered later via wait/send/interrupt) — that alone is deferred.

## 15. Dynamic subagents — in scope vs deferred

"Dynamic subagent" means three distinct things in Claude Code / Codex /
OpenCode: a **generic catch-all agent** tasked with an arbitrary prompt
(`general-purpose` / `default` / `general`), a runtime **fork** of the caller
(`fork` / `fork_context`), and **file-defined agents** loaded without recompile
(`.claude/agents/*.md`, `$CODEX_HOME/agents/*.toml`, `.opencode/agent/*.md`).
None of the three lets the *model* define a new toolset/boundary at runtime — the
boundary is always pre-defined (in code, in config, or inherited via fork).

This design keeps the dynamism that matters and defers the rest:

- **Dynamic task** — IN. The `message` handed to any subagent is arbitrary / model-defined.
- **Dynamic behavior** — IN (P2). Skills are file-loaded `SKILL.md`; an agent's procedure changes at runtime without code changes.
- **Dynamic boundary by the model** — intentionally NOT supported (no self-authorization), matching all three tools.
- **Generic catch-all agent** — DEFERRED. The four roles already cover the privilege tiers (operator = broad mutating worker; explorer/researcher = read-only), tasked via custom prompts. Trivial to add a fifth `general` leaf if a real gap appears.
- **File-defined agent boundaries** — DEFERRED, gated behind a future security review. An editable `tools:` file is a privilege-escalation surface; boundaries stay compiled Go (reviewed code, fail-secure). Behavior-level dynamism is served by skills.
- **Fork** — DEFERRED (advanced; grouped with async/streaming subagents above).
