# Headless / Non-Interactive Permission Mode — Declared Allowlist + Fail-Secure (native + foreign)

**Date:** 2026-07-01
**Status:** Draft
**Supersedes:** [Design B — Invoke Control Semantics & Autonomous Headless Mode](2026-06-16-autonomous-headless-mode-design.md). That spec's three foundations are gone from `main`: `Session.Invoke`/`Session.Stream` were deleted (`9d0867e`, `adf1d53`); the Design-A event taxonomy plus the `Sink`/`EventEnvelope`/`NotificationEvent` scaffolding it leaned on was deleted or never built (`4a5faaf`); and the declared agents (`agents/personal-assistant`, `agents/coding`) it wired never landed on `main`.
**Related:** [Foreign Loop Backend Design](2026-06-25-foreign-loop-backend-design.md), [Gates Package](2026-06-18-gates-package-design.md).

> **Note on framing (superseded).** This document was originally drafted as an "open gate" design — a *flood* posture that enforced only the safety floor and auto-approved **everything** above it. That framing is **superseded**: an external code review showed "clearing the floor" is necessary but not sufficient for safety (`Bash`, network, `Skill`, unknown tools would all be blanket-approved). The design below replaces the flood with a **consumer-declared auto-approve allowlist** (least privilege, a calculated risk owned by the agent/swarm definer) plus a small **fail-secure decorator** so an unattended turn never parks. The blanket wildcard remains only as an explicit, discouraged escape hatch (§3, §5).

## 1. Motivation

Run a looprig agent **unattended** — no interactive approver present — so the turn
**never parks on a *permission* prompt** and its permission path runs to completion.
The claim is scoped precisely: headless mode removes **permission** parking, not
**all** parking. The `AskUser` **user-input** gate still parks by design (§3c) —
autonomy must not sever the agent's channel to the human. So a no-human deployment
MUST have its driver supply a `UserInputRequested` policy — a responder, a timeout, or
a cancellation — or the turn will legitimately block on that user-input gate (Q2; §3c,
§5). At the same time, **keep the agent's ability to reach the user**: the `AskUser`
tool stays, and its question is delivered over whatever transport the *driver* provides
(the TUI in the CLI, an HTTP responder in the cloud, an automated policy in a batch
harness).

**The mechanism is a calculated allowlist, not a flood.** The permissions a headless
agent may auto-approve are **declared by the agent/swarm definer** (in the swe
consumer, §1a), not opened wholesale by the SDK. The definer already has the vocabulary
for this in the existing `PermissionPolicy` (§3b): a tool-level allowlist plus a
tool-scoped "workspace permissions list" (§3b), layered in the allow hierarchy,
over the non-bypassable safety floor. Anything the definer did **not** allow is
**denied fail-secure** in headless mode (so the turn continues with a denied tool
result) — never flooded-open, never parked.

The key framing: "how autonomous is tool approval" is a **posture axis** shared with
the *foreign* loop (the `--permission-mode` it hands `claude -p`). This design gives the
*native* loop the same axis and unifies the vocabulary (§3a) so there is one concept
spanning both engines — while the actual auto-approvals stay a least-privilege,
definer-owned choice.

## 1a. Scope & module boundary

This design lives in **looprig** (`github.com/looprig/harness`), a reusable
**SDK/module**. looprig provides **agent-agnostic capability** — the headless
permission posture (§3) and a reusable HTTP API runner, `pkg/api` (§3f) — but hosts
**no concrete agents and no `cmd/` entrypoint**, and it **declares no auto-approve
policy of its own**. Those live in the consumer.

- **looprig (this repo, the SDK)** owns: the `NonInteractiveGate` decorator, the
  composition-root `Intent` selector (§3a — a wiring choice, **not** session state), the
  transport-agnostic gate/event model (§3c), and the reusable `pkg/api` runner (§3f). All
  agent-agnostic; all policy-free.
- **swe (`github.com/looprig/swe`, the consumer)** `require`s looprig and owns
  `agents/` (reviewer, operator, coding, personal-assistant, …), `swarms/`, and
  `cmd/swe`. It **wires its agents** — choosing `Interactive` vs the `Unattended` intent
  per agent, **and declaring each agent's auto-approve `PermissionPolicy` allowlist** —
  into looprig's runners, and owns **deployment + credentials**.

**Precedent — `pkg/api` mirrors `pkg/cli`.** looprig already ships
`pkg/cli.Run(ctx, newAgent func(ctx) (tui.Agent, error), banner)`; `swe/cmd/swe`
injects its agent constructor and looprig runs the TUI. `pkg/api` (§3f) is the exact
analogue for a headless cloud service: the same injected-factory shape, an HTTP surface
instead of a terminal. looprig defines the runner; swe supplies the agent (with its
declared allowlist) and the `cmd/swe-serve` that deploys it.

This is why the 2026-06-16 spec's B2 (a `WithAutonomous()` on `personalassistant.New` /
`coding.New`) was the wrong layer: those agents legitimately live in the swe consumer,
not looprig. The headless posture is an **SDK-layer, agent-agnostic** mechanism the
consumer opts into per agent — and the *allowlist that defines the calculated risk* is
the consumer's to declare, not a method on any concrete agent and not a policy baked
into the SDK.

## 1b. Clients & runners — one session, many drivers

The `Session`/loop is the **single source of truth** and is engine- and transport-agnostic.
The TUI and the HTTP API are **not different systems** — they are two **drivers** of the
*same* narrow driven surface: `Submit` / `Subscribe` / `Approve` / `Deny` / `ProvideAnswer`
/ `Interrupt` / `Close` (these delegate to the session's `SubscribeEvents` /
`ProvideUserInput` / `Shutdown`). The TUI drives the existing `tui.Agent`; the HTTP runner
drives a **narrower** `pkg/api.Agent` (§3f) — a subset that drops the TUI-only methods
(`AcceptsImages`, `ReplayBacklog`), per Interface Segregation. (`ExportSource` is **kept** —
the API exposes an export endpoint, §3f.)
`pkg/cli.Run` (terminal) and `pkg/api.Serve` (HTTP, §3f) are **sibling runners**, each with
its **own factory shape** — cli: one-shot `func(ctx) (Agent, error)`; api: a request-carrying
`Factory` that takes create/resume + the target `sid` (§3f) — and neither knows the agent's
composition or policy.

**Headless vs in-process is purely a composition-root choice of `(Intent, runner)` over the
*same* agent definition** — the agent, its toolset, model, and system prompt are identical:

- `Interactive` intent → plain `PermissionChecker`; gates fire and a human answers.
  Typically driven by `pkg/cli` (in-process TUI).
- `Unattended` intent → `NonInteractiveGate{PermissionChecker}` (declared allowlist,
  auto-resolve, never park on permission); only `AskUser` needs a remote responder.
  Typically driven by `pkg/api` (cloud service).

What differs between the two clients — everything else is identical:

| Aspect | `pkg/cli` (TUI, in-process) | `pkg/api` (HTTP, cloud) |
|---|---|---|
| Transport | terminal render of the event stream | `event.Event` → **SSE** frames |
| Gate answer | keyboard input | `POST /sessions/{sid}/gates/{toolExecID}`, `LoopID` from the server-side pending-gate registry (§3f) |
| Typical intent | `Interactive` (human present) | `Unattended` (only `AskUser` needs a responder) |
| Lifecycle | one in-process session for the process | a server managing **many** sessions keyed by `sid` |
| Completion | interactive; or `WaitIdle` (native) | drain-to-terminal on the SSE stream (§3d) |

Everything below the driver — the loop, the `event.Event` stream, and `Submit` / gate /
`Interrupt` / `Shutdown` **semantics** — is the **same** across both clients. The `Session`
carries **no `Intent`**: it holds only a `loop.PermissionGate`, and the composition root
(the consumer's agent factory) decides whether to build a plain `PermissionChecker` or a
`NonInteractiveGate`-wrapped one; the runner just drives whatever session the factory
produced. The design adds one composition-root posture *selector* (`Intent`, §3a) and one
new runner (`pkg/api`, §3f); it adds no session state and does not fork the session model.

## 2. What changed vs the 2026-06-16 spec (reconciliation)

The prior design ("Design B") was written against an architecture that has since been
dismantled. Verified against `main`:

| Prior section | Premise (2026-06-16) | Reality on `main` | Verdict |
|---|---|---|---|
| **B1** — `Invoke` control-event semantics | Fix `Session.Invoke` (blocks, silently discards control events); contrast with `Session.Stream` | Both methods **deleted** — `Invoke` in `9d0867e`, `Stream` in `adf1d53`. Programmatic turn path is now `Submit()` + subscribe + `drainToFinalText` | **Obsolete** — targets deleted methods |
| **B2** — `WithAutonomous()` on declared agents | Add option to `personalassistant.New`/`coding.New`; replace their `autoApprovedTools`; propagate to `SubagentFactory` | `agents/` **does not exist in looprig** — concrete agents legitimately live in the **swe consumer** (`github.com/looprig/swe`), not the SDK (§1a) | **Wrong layer** — retarget as an agent-agnostic SDK posture; the allowlist is the consumer's |
| **B3** — `NotificationEvent` + Sink journal | Emit `NotificationEvent`s; "Sinks hold the full redacted journal"; render in TUI | `Sink`/`EventEnvelope`/`Redactable` **deleted** (`4a5faaf`, "zero implementers"); `NotificationEvent` **never built**. Delivery is `Hub` + full-fidelity `Subscription` + durable journal | **Dropped** — no event to render |
| Event taxonomy (Design A) | `ControlEvent`/`TerminalEvent` markers, `NotificationEvent`, unified `Sink` | Replaced by `Class`×`Scope` + `EndsTurn` + `Header` + `Reply`; delivery via `Hub` + `Subscription` | **Replaced wholesale** |
| Safety floor + auto-approve | Wildcard `HardApprove` over non-bypassable Containment + HardDeny | **Holds** — a typed `loop.Effect` pipeline in `pkg/tools`; the allowlist mechanism (`HardApprove` + `Policies`) already exists; only the `.urvi` → `~/.looprig` store rename + new deny-write rules | **Reused** — the allowlist is existing machinery (§3b) |

**What survives and is reused:** the non-bypassable floor invariant; the
**allowlist pipeline** the consumer declares — `HardApprove.Tools` + `ToolPolicy`
`Policies`; and the *mechanism idea* — auto-approve within the floor — now expressed via a
definer-declared policy plus a `NonInteractiveGate` decorator rather than a mutation of
`Invoke`. (The checker's **persisted approvals** and self-approving `EffectChecker` are
part of the *interactive* pipeline only — both are **disabled/suppressed under
`Unattended`** so headless approves *strictly* the definer's declared allowlist; see §3b.)

**The concept that reshaped the design:** the foreign loop
([Foreign Loop Backend Design](2026-06-25-foreign-loop-backend-design.md)) already runs
a coding agent headless and unattended — but for *external* agents (Claude Code) via
`--permission-mode`, observe-only, as a `loop.Backend` behind an `Engine` switch. It
does **not** cover the native declared agents. So this design is still needed, but it
lives beside a sibling mechanism, and the two share the posture axis unified here.

## 3. Design

### Trust model, preconditions & residual risk (declared allowlist)

**Decision.** Headless mode does **not** flood the gate. The auto-approvals are a
**consumer-declared allowlist** (§3b) — a **calculated, least-privilege choice owned by
the agent/swarm definer**. looprig supplies only the mechanism: the existing
`PermissionPolicy` pipeline plus the `NonInteractiveGate` decorator that turns any
would-be prompt into a fail-secure **deny** so an unattended turn never parks. The
non-bypassable floor (Stage 1 Containment + Stage 2 HardDeny) is enforced underneath
regardless, exactly as in interactive mode.

- **The blast radius is what the definer allowed — nothing more.** If a definer keeps
  the allowlist to read-only + workspace edits, that is the whole risk surface. If a
  definer allowlists `Bash` or the network tools, **that is their explicit, auditable
  calculated risk**, taken deliberately for that agent — not a default the SDK imposed.
- **Precondition — explicit opt-in only.** `Unattended` intent is **NEVER a default**
  (zero-value `Intent` is `Interactive`). It is enabled only via an explicit
  construction `Option` now, and a clearly-named flag when the CLI lands (§7).
  Constructing a `NonInteractiveGate` fires a loud `slog.Warn` security event with the
  data available *at construction* — the workspace root, an effective-policy summary, and
  "unattended posture selected" (CLAUDE.md: log security-relevant configuration). Note the
  checker/gate is built **before** `session.New` mints the loop id (in swe, toolsets and
  checkers are constructed first), so the construction-time audit carries **no loop id**; a
  loop-id-correlated audit line, if wanted, fires at session/loop creation.
- **Isolation boundary — recommended, not enforced (v1).** Running an unattended agent
  in an isolated / sandboxed cwd (a dedicated git worktree, container, or throwaway
  checkout) is **strongly recommended**, *especially when the allowlist includes `Bash`
  or the network tools*. For **v1 this is honor-system, not enforced** (§7, resolved
  2026-07-01); it becomes enforced later together with OS-level sandboxing.

**The wildcard flood is an escape hatch, not a posture.** The blanket
`HardApprove{Tools: ["*"]}` — auto-approve everything the floor **and any tool
`EffectChecker` gate** clear — **remains available** because it is just an existing
`PermissionPolicy` value. It is **not** a posture, **not** a default, and is
**discouraged**: it re-incurs most of the risk the code review flagged (`Bash` `sh -c`,
`Fetch` egress, unknown/no-path tools all auto-approved). Two mitigations survive even
the wildcard, because `EffectChecker` (Stage 3) runs **before** `HardApprove` (Stage 4):
a tool that pins its own effect there is honored ahead of the wildcard — e.g. `Skill`'s
workspace-load gate returns `EffectAsk` at Stage 3, so the wildcard does **not**
auto-approve it; and under headless, `NonInteractiveGate` maps that `EffectAsk` to
`EffectDeny`, so `Skill` workspace loads (and any other `EffectChecker` gate) are
**denied regardless**, even under the flood. Use the wildcard only for a fully-trusted,
disposable workload **under the (still-deferred) OS sandbox**. Documented so a definer
who reaches for it knows exactly what they are accepting.

**Trust model (stated plainly).** An unattended native loop can do **whatever its
declared allowlist permits, inside the floor, with the user's own privileges** — it is
contained by the floor and by the allowlist, **not sandboxed**. The floor bounds
*where* a tool may act and blocks a fixed denylist; the allowlist bounds *which* tools
may act without a prompt. Neither is an OS sandbox.

**Residual risk (stated honestly).** A git-worktree cwd is **NOT a security sandbox**.
If a definer allowlists `Bash`, that `Bash` still runs `sh -c` with full privileges;
Containment + HardDeny stop workspace-escape paths and secret reads (`~/.ssh`,
`**/.env`, `**/*.pem`, `~/.looprig`, `**/id_rsa`, `**/.skills/**`) but do **NOT** stop
network egress (a `Fetch` POST, or `curl … -o x && sh x`) or reads of anything outside
cwd that is not hard-denied. CLAUDE.md's real prerequisite for auto-approving `Bash` is
**OS-level sandboxing (seccomp/landlock/nsjail/container)**, which is **OUT OF SCOPE**
here; a worktree only reduces *blast radius*, not *capability*. **OS-level sandboxing is
planned as a later addition** (§7); until it lands, an allowlist that includes
`Bash`/network carries this risk in full, so such an agent should be pointed **only at
workloads and environments the definer already trusts**.

**CLAUDE.md reconciliation.** CLAUDE.md documents `Bash`'s per-call human-gate as the
security boundary "until OS sandboxing exists … the prerequisite for ever auto-approving
`Bash` broadly." This design does **not** auto-approve `Bash` by default — the
interactive `PermissionChecker` still human-gates it exactly as today, and a headless
agent only auto-approves `Bash` **if its definer explicitly allowlists it**. That is a
deliberate, per-agent, opt-in exception rather than a change to default behavior; when a
definer takes it, the residual-risk note above (and the OS-sandbox recommendation)
applies. Landing this may warrant a short exception note in CLAUDE.md (mirroring the
existing `Bash`-tool exception), added at implementation time.

### 3a. Intent — one shared axis, per-engine posture

The shared concept is an **intent** — a **composition-root selector**, not session state
and not a single cross-engine posture value: "interactive" (a human/driver answers gates)
vs "unattended" (run to completion without a permission prompt). It is a lightweight enum
the composition root and runners read; it is **never** stored on `loop.Config` or the
`Session` (both carry only a `loop.PermissionGate`). Zero value `Interactive`:

```go
type Intent uint8
const (
    Interactive Intent = iota // full permission pipeline; gates fire; a driver answers
    Unattended                 // no permission prompt; declared allowlist approves, rest deny
)
```

The composition root reads the intent and maps it **per engine to that engine's own
posture** — the mapping is adapter-owned; the intent is *not* forced onto either
engine's internal vocabulary:

- **Native loop** → the composition root (the consumer's agent factory) uses the intent to
  choose which `loop.PermissionGate` it wires into `ToolSet.Permission` at session
  construction: `Interactive → PermissionChecker` (with the consumer's `PermissionPolicy`);
  `Unattended → NonInteractiveGate{ PermissionChecker }` (the **same** checker + the same
  declared allowlist, wrapped so a would-be prompt becomes a fail-secure deny). The runner
  (`pkg/cli`/`pkg/api`) is agnostic — it drives whatever session the factory built. The
  intent is consumed *at wiring time* and discarded; neither the loop nor the session
  stores it — they see only the resulting `PermissionGate`.
- **Foreign loop** → intent maps to the adapter's **existing**
  `foreignloop.PermissionPosture` (`PostureDefault` / `PostureAcceptEdits`, verified in
  `pkg/foreignloop/foreignloop.go`), which `pkg/foreignloop/claude/args.go` turns into
  `--permission-mode default|acceptEdits`. **Preserve that enum as-is.** Foreign carries
  *two* distinct values, so the native 2-value `{Interactive, Unattended}` vocabulary
  must **not** be collapsed onto it — the adapter owns the intent→posture mapping (an
  unattended intent may select `PostureDefault` or `PostureAcceptEdits` per adapter
  policy).

**Honest caveat — per-engine semantics differ.** The intent is a shared *axis*, not
identical behavior. For the native loop, `Unattended` enforces **our** floor and
approves the **definer's declared allowlist**, denying the rest. For the foreign loop
the boundary is **observe-only** — the external agent owns its own permission model; we
only choose its non-interactive `--permission-mode`. The unification prevents duplicate
concepts; it does not claim byte-identical enforcement, nor a shared value set, across
engines.

### 3b. Native headless = the declared allowlist + a fail-secure decorator (Open/Closed)

The native loop depends only on the `loop.PermissionGate` interface
(`pkg/loop/deps.go:119`):

```go
type PermissionGate interface {
    Check(ctx context.Context, t tool.InvokableTool, toolName, argsJSON string) Effect
    Grant(ctx context.Context, toolName, argsJSON string, scope tool.ApprovalScope) error
}
```

`Effect` is `EffectAsk` (zero value) / `EffectDeny` / `EffectAutoApprove`, and the
contract is **fail-secure**: on any ambiguity or internal error, return `EffectAsk` or
`EffectDeny`, never `EffectAutoApprove`. Because the loop is coded to the interface, the
headless posture is added **at the composition root** — no change to the loop, the
runner, or the existing checker.

**The allowlist is existing machinery — the consumer declares it.** The auto-approvals
are the definer's `PermissionPolicy` (`pkg/tools/permission.go`), which the interactive
`PermissionChecker` already evaluates. Two fields express the calculated risk:

- **`HardApprove.Tools`** — the **tool-level allowlist** (Stage 4): tools always
  approved once the floor clears (e.g. `ReadFile`, `Glob`, `Grep`, `WriteFile`,
  `EditFile`). A specific list is the calculated choice; the wildcard `"*"` is the
  discouraged flood escape hatch (§3).
- **`Policies []loop.ToolPolicy{Tool, Effect, Match}`** — the **workspace permissions
  list** (Stage 6): per-tool effects, layered in the allow hierarchy. Note `Match` today
  is **tool-scoped**, not a general per-arg matcher — e.g. it keys on **hosts for
  `Fetch`**; `WebSearch` grants are **tool-level only**; and there is **no** `Bash`
  command-prefix or write-path-glob matching (session policies flatten to
  `ApprovalRecord` with no prefix concept, `pkg/tools/check.go`). So a definer can today
  allow a tool outright or scope it by that tool's existing match key (Fetch hosts);
  **finer-grained per-arg scoping** — "auto-approve `Bash` only for `go test …`" or
  "auto-approve writes only under `./src/**`" — is **future work** (§7), not current
  capability.

The full pipeline stays: **Stage 1 Containment → Stage 2 HardDeny (non-bypassable
floor) → malformed-args gate → Stage 3 `EffectChecker` → Stage 4 `HardApprove` → Stage 5
persisted → Stage 6 session `Policies` → Stage 7 default `EffectAsk`.** In interactive
mode the Stage-7 default is a prompt. In headless mode that default prompt is exactly the
thing we must eliminate.

**Persisted approvals (Stage 5) are disabled under `Unattended`.** Stage 5
(`stagePersistedApprovals`, `pkg/tools/check.go`) loads the workspace + user `~/.looprig`
approval files, and an ALLOW record there returns `EffectAutoApprove` — so a grant
written by a *previous* session could auto-approve a call the definer never declared,
breaking "only the declared allowlist approves." The `NonInteractiveGate` decorator
**cannot** fix this: Stage 5 returns `EffectAutoApprove`, indistinguishable from a
declared approval, so it passes through. The fix is at **checker construction** — for the
`Unattended` intent the composition root builds the `PermissionChecker` with
**persisted-approval loading disabled** (a `PermissionPolicy` flag / `NewPermissionChecker`
option). Then the only approval sources are the definer's `HardApprove.Tools` (Stage 4)
and declared `Policies` (Stage 6 — which under headless is *only* the declared policies,
since `Grant` is never called and no runtime session grant is ever added). This also
matches reality: a headless / isolated run has no legitimate persisted approvals to honor
anyway.

**`EffectChecker` auto-approvals (Stage 3) are suppressed under `Unattended`.** Stage 3
runs *before* the allowlist stages, and the `EffectChecker` interface permits a tool to
return `EffectAutoApprove` from `CheckEffect` — which `PermissionChecker` honors
immediately (`pkg/tools/check.go`). No production tool does this today (`Skill` returns
only `EffectAsk`/`EffectDeny`), but the interface allows it, so a tool could self-approve
ahead of the declared allowlist, again breaking "only the declared allowlist approves."
Like persisted approvals, the decorator can't fix this (Stage 3 returns `EffectAutoApprove`,
indistinguishable from a declared allow). So the **same `Unattended` checker option**
covers a second suppression: under `Unattended`, an `EffectChecker` that returns
`EffectAutoApprove` is **not** honored as an immediate approve — the call falls through to
the allowlist stages (Stage 4/6), so the tool must still be declared. `EffectChecker`
`EffectDeny` is **still honored** (it is a safety veto), and `EffectChecker` `EffectAsk`
still becomes `EffectDeny` via the decorator. Net: the `Unattended` checker option is the
pair **{persisted-approvals-disabled + effectchecker-autoapprove-suppressed}**, so the
only approval sources are the definer's `HardApprove.Tools` and declared `Policies`.

**The one new SDK piece — `NonInteractiveGate` (a fail-secure decorator).** It wraps
**any** `loop.PermissionGate` and translates the "would prompt" outcome into a
fail-secure deny:

```go
// NonInteractiveGate makes a PermissionGate safe to run with no human present:
// a would-be prompt becomes a deny (the turn continues with a denied tool result),
// never a park and never a silent approve. Approvals and denials pass through
// unchanged, so the inner gate's declared allowlist and the safety floor are fully
// honored. Wrap only for the Unattended intent.
type NonInteractiveGate struct { inner loop.PermissionGate }

func (g NonInteractiveGate) Check(ctx context.Context, t tool.InvokableTool, name, argsJSON string) loop.Effect {
    switch e := g.inner.Check(ctx, t, name, argsJSON); e {
    case loop.EffectAsk:
        return loop.EffectDeny // fail-secure: no one to ask
    default:
        return e               // EffectAutoApprove / EffectDeny pass through
    }
}

func (g NonInteractiveGate) Grant(ctx context.Context, name, argsJSON string, scope tool.ApprovalScope) error {
    return g.inner.Grant(ctx, name, argsJSON, scope) // pass-through; no gate is ever opened, so it is unused
}
```

Resulting behavior with an inner `PermissionChecker(policy)` under the `Unattended`
checker option (persisted approvals disabled + `EffectChecker` auto-approve suppressed,
per above):

- floor / policy **deny** (Stage 1–2, an `EffectChecker` `EffectDeny`, or a `Policies`
  `EffectDeny`) → `EffectDeny`;
- **allowlisted** (Stage 4 `HardApprove` or a Stage-6 declared `Policies`
  `EffectAutoApprove` match) → `EffectAutoApprove`;
- **everything else** — the Stage-7 `EffectAsk` default, plus (with the option) anything a
  stale persisted grant (Stage 5) or a self-approving `EffectChecker` (Stage 3) would
  otherwise have approved — → `EffectDeny` **fail-secure**: the turn continues with a
  denied tool result and **never parks**;
- it **never floods**: only what the definer declared is approved.

This is Open/Closed: `NonInteractiveGate` **wraps**, it does not modify, the checker.
There is **no** `Floor` extraction and **no** flood `OpenGate` type — the interactive
pipeline is reused verbatim, and headless is a decorator over it.

**`ReadGuard` is preserved — with one fix.** `PermissionChecker` satisfies both
`loop.PermissionGate` and `loop.ReadGuard` (`DeniedRead`/`MaxReadBytes`, verified in
`pkg/tools/permission.go`); the read tools (`ReadFile`/`Glob`/`Grep`) take a
`loop.ReadGuard` **at construction** and self-enforce deny-read during traversal. Because
`NonInteractiveGate` wraps only the `Check`/`Grant` path, the composition root passes the
**inner `PermissionChecker` itself as the `ReadGuard`** to the read tools (unchanged);
the decorator only affects the permission decision. Deny-read (`~/.ssh/**`, `**/.env`,
`**/*.pem`, `**/id_rsa`, `~/.looprig/**`, `**/.skills/**`) stays enforced in-tool,
independent of any prompt.

**Home-resolution must fail *closed at construction*, not silently *open at runtime* (fix).**
Today both the Check path (`containAndHardDeny`, `pkg/tools/check.go`) and the read filter
(`DeniedRead`, `pkg/tools/permission.go`) resolve home per-call via `resolveHomeOrEmpty`,
which returns `""` on error — making every `~/...` glob (`~/.ssh/**`, `~/.looprig/**`) a
**no-match**, i.e. **fail-open** (the `DeniedRead` code comment even mislabels this
"fail-secure"; for `~` patterns it is not). Under an **allowlist** (Stage 4 `HardApprove`),
a cleared-but-home-unresolvable path could reach Stage 4 and be **auto-approved → FAIL-OPEN**
(a `~/.ssh/**` write approved purely because home was unresolvable). A *runtime* deny can't
fix `DeniedRead` cleanly: it receives **only an absolute path**, so once home is unresolvable
it cannot tell which paths *would* be `~/...`-relative — the only safe runtime options are
deny-**all** reads (cripples the agent) or fail-open (the bug). **FIX — resolve home once, at
`PermissionChecker` construction, and fail fast:** `NewPermissionChecker` resolves home when
built; if resolution fails **while any `~/...` pattern is configured** (in the hard-deny set
*or* the read-deny set — the `DefaultHardDeny` includes `~/.ssh` and `~/.looprig`), it returns
a **typed construction error** (e.g. `HomeUnresolvableError`; CLAUDE.md: fail loudly on
missing/unresolvable required config). Then at runtime home is **always** resolved, so neither
the Check path nor `DeniedRead` ever faces an empty home — no fail-open and no deny-all; a
**defensive runtime deny** remains as a backstop should home ever read empty post-construction.
Construction still **succeeds** with an unresolvable home when **no** `~/...` pattern is
configured. Realistic implication: a headless container MUST set `HOME` (or explicitly drop the
`~/...` rules from its policy) — surfaced at startup, never as a silent security downgrade.

**The constructor becomes fallible (breaking change) — `NewPermissionChecker(policy, opts ...Option) (*PermissionChecker, error)`.**
Construction-time home resolution — and construction-time home *injection*, so a test can force
a pre-construction failure — need both a fallible return and an options seam; the current
non-fallible `NewPermissionChecker(policy) *PermissionChecker` with post-construction
`SetHomeDir` (`pkg/tools/permission.go`) cannot fail fast. New shape:

```go
func NewPermissionChecker(policy PermissionPolicy, opts ...Option) (*PermissionChecker, error)
// Options:
//   WithHomeDir(fn homeDirFunc) — inject/override the home seam AT CONSTRUCTION (default
//                                 os.UserHomeDir); REPLACES SetHomeDir, whose post-construction
//                                 injection defeats fail-fast and can't force a pre-construction
//                                 home failure in a test.
//   WithUnattended()            — the Unattended checker option pair
//                                 {persisted-approvals-disabled + EffectChecker-autoapprove-suppressed}.
```

The constructor resolves home via the (possibly injected) seam and returns `HomeUnresolvableError`
on failure while any `~/...` pattern is configured (above). This is a **breaking change**
(`*PermissionChecker` → `(*PermissionChecker, error)`; `SetHomeDir` removed in favor of
`WithHomeDir`), but the blast radius is narrow: there are **no non-test looprig callers** of
`NewPermissionChecker`, so looprig migrates only its own tests; the swe consumer's call site
(`swarms/swe/swarm.go`) migrates on its next looprig upgrade (Phase 3 / consumer).

### 3c. AskUser stays; gates are transport-agnostic

Headless mode governs **permission** gates only. It does **not** touch **user-input**
gates. `AskUser` continues to emit `UserInputRequested` (`pkg/event`), and the turn
parks on that gate until it is answered — exactly as today.

Delivery is already a **publish/subscribe** concern the session owns and the driver
serves. The session publishes gate events; a driver subscribes via
`Session.SubscribeEvents` and answers through the existing
`Session.Approve` / `Session.Deny` / `Session.ProvideUserInput`
(`pkg/session/session.go`). The transport is the driver's business.

**Replies are loop-scoped — route by the gate event's `LoopID`, not a bare tool id.**
`Session.Approve` / `Deny` / `ProvideUserInput` each take a `loopID` (verified at
`pkg/session/session.go`, `resolveGate`): the reply is dispatched to the loop that opened
the gate, a zero `loopID` falls back to the primary loop, and an unknown non-zero
`loopID` fails secure with `SessionLoopNotFound`. Every gate event carries its origin in
a loop-scoped `Header` (`identity.Coordinates.LoopID`, `pkg/event`). Drivers MUST extract
that `LoopID` from the gate event's header and pass it back on the reply — a **subagent**
loop's `AskUser` must be answered on the subagent's loop, not misrouted to the primary.

- **CLI:** the TUI renders `UserInputRequested` and answers from keyboard input.
- **Cloud / headless service:** `pkg/api` (§3f) turns `UserInputRequested` into an SSE
  frame and feeds the reply back through `ProvideUserInput`.
- **Batch harness:** an automated policy answers (or cancels) programmatically.

These transports are **frontends** — this design *enables* them (the event model is
already transport-agnostic) but does not *build* them.

**Net effect.** Under headless mode the **permission** path can never park — the footgun
the old B1 fretted over (`Submit` + `drainToFinalText` hanging on an unanswered
permission gate) is eliminated: a non-allowlisted call resolves to `EffectDeny` (via the
decorator), so no permission prompt is ever emitted. The **user-input** path still
requires a driver to answer — *by design*, so the agent keeps a channel to the human. A
deployment with no responder must have its driver answer or cancel `UserInputRequested`
(see §5, Q2).

### 3d. Foreign loop accounted for

The foreign loop already runs headless and unattended via its posture, so this design
**unifies the vocabulary rather than duplicating it** (§3a): the shared `Intent` is the
single source of "how autonomous is approval," mapped per engine.

One adjacent gap is **flagged but kept separate**: a foreign *primary* loop does not emit
`LoopIdle` — its turn completion publishes `TurnDone`/`TurnFailed` WITHOUT a `LoopIdle`
(verified at `pkg/foreignloop/turn.go`), whereas the native loop emits `LoopIdle` after a
terminal turn (`pkg/loop/loop.go`). The hub's quiescence removes a loop's activity key
**only** on `LoopIdle` (`pkg/hub/hub.go`), so a foreign primary never drives the session
to `SessionIdle` (subagents are unaffected — they drain to `TurnDone` via
`drainToFinalText`, `pkg/session/drain.go`). The `LoopIdle` fix is a foreign-loop fix,
not a headless-permission concern, and gets its **own spec** (§5, Q3).

**Completion primitive under headless — drain-to-terminal, NOT `WaitIdle`.** Because that
gap exists today, the unattended completion signal a headless driver relies on is
**drain-to-terminal**: subscribe and drain to `TurnDone` (via `drainToFinalText`,
`pkg/session/drain.go`) — the same path subagents already use. It is **not**
`WaitIdle`/`SessionIdle`. A foreign **primary** loop MUST NOT use `WaitIdle` as its
completion signal until the separate `LoopIdle` fix lands, or it will wait forever on a
quiescence event that is never emitted. (A native primary does emit `LoopIdle`, so
`WaitIdle` is safe there — but the portable, engine-agnostic completion primitive is
drain-to-terminal.)

### 3e. What we drop from the old spec

- **B1 (Invoke/Stream control-event semantics)** — the methods are deleted; the concern
  is now answered by posture (no permission parking) plus the `Submit` +
  `drainToFinalText` turn path.
- **B3 (`NotificationEvent` + Sink redacted journal)** — the `Hub` + `Subscription` +
  durable journal already carry the full-fidelity event trail; **no new event type is
  added**, matching the `4a5faaf` precedent of removing speculative scaffolding with no
  implementers.
- **"Drop `AskUser` under autonomy"** — **reversed** (§3c): autonomy must not make the
  agent unable to reach the user.
- **The flood `OpenGate` / `Floor`-extraction** — **dropped** in favor of the
  consumer-declared allowlist + `NonInteractiveGate` decorator (§3b); the wildcard flood
  survives only as an explicit escape hatch (§3).

### 3f. Cloud API (`pkg/api`) — reusable, agent-agnostic HTTP runner

`pkg/api` is the headless analogue of `pkg/cli.Run`: a reusable runner that exposes **an
injected agent/session factory** over HTTP so a consumer (swe) can run its agents
unattended in the cloud (§1a). looprig defines **no** concrete agent and **no** allowlist
— the consumer supplies the agent (its toolset, model, system prompt, its declared
`PermissionPolicy` allowlist, and whether the `Unattended` intent is selected). Stdlib
`net/http` only; no new dependency.

**Injected factory (request-carrying — differs from `pkg/cli.Run`).** `pkg/cli.Run` takes a
one-shot `func(ctx) (tui.Agent, error)` because it drives exactly **one** in-process session.
`pkg/api` manages **many** sessions keyed by `sid` and must tell the factory *which* session to
build and *whether* to create or resume it — so its factory is **request-carrying**. `pkg/api`
owns the HTTP surface, streaming, the many-session map, and gate routing; the factory owns the
agent's composition, policy, and **persistence** (the module boundary keeps looprig
policy/credential-free):

```go
// pkg/api — the agent surface is the narrow pkg/api.Agent (below), NOT tui.Agent.
type AgentRequest struct {
    SessionID uuid.UUID // the sid to create-with (new) or resume (existing)
    Resume    bool      // false = create (session.New with WithSessionID); true = resume (Restore)
}
type Factory func(ctx context.Context, req AgentRequest) (Agent, error)

func Serve(ctx context.Context, cfg Config, f Factory) error   // binds + runs the handler
func Handler(cfg Config, f Factory) http.Handler               // the router, for consumer-owned wrapping (auth, §3f)
```

On `POST /sessions` the runner mints a fresh `sid` (or, for `?resume=<sid>`, uses that sid with
`Resume: true`) and calls the `Factory`; the factory builds `session.New(…, WithSessionID(sid))`
for create or `Restore(sid, js, objectStore, leases, …)` for resume — **persistence stays
factory-owned** (the consumer chooses nop vs the durable journal). The runner registers the
returned `Agent` under `sid` in its session map and opens the per-session supervisor (below).

**The narrow `pkg/api.Agent` interface (Interface Segregation).** The runner drives a subset of
`tui.Agent` — it does **not** need the TUI-only `AcceptsImages`/`ReplayBacklog`, but it **does**
need `ExportSource` (for the export endpoint below). Existing agents (`*coding.Coding`, …)
satisfy it structurally:

```go
// pkg/api.Agent — exactly what the HTTP runner drives; delegates to the session.
type Agent interface {
    Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error)
    PrimaryLoopID() uuid.UUID
    Subscribe(filter event.EventFilter) (event.Subscription, error)     // → session.SubscribeEvents
    Approve(ctx context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error
    Deny(ctx context.Context, loopID, callID uuid.UUID) error
    ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error // → session.ProvideUserInput
    Interrupt(ctx context.Context) (bool, error)
    Close(ctx context.Context) error                                    // → session.Shutdown
    // ExportSource backs GET /sessions/{sid}/export (transcript.Reconstruct + html.Render).
    ExportSource(ctx context.Context) (transcript.RecordSource, transcript.SystemPromptResolver, error)
}
```

**Endpoints (v1 = plain / unauthenticated — front with consumer auth before exposure, see
Auth below; all with context deadlines):**

| Method + path | Purpose | Session mapping |
|---|---|---|
| `POST /sessions` | create (or `?resume=<sid>`) a session | `session.New` / `Restore` via the injected factory |
| `POST /sessions/{sid}/input` | submit a user turn; returns the `InputID` | `Submit(ctx, blocks) → uuid` |
| `GET  /sessions/{sid}/events` | **SSE** event stream | `SubscribeEvents(filter)`; each `event.Event` → one SSE frame |
| `POST /sessions/{sid}/gates/{toolExecutionID}` | answer a gate (approve/deny/answer) | `Agent.Approve`/`Deny`/`ProvideAnswer` (→ session methods); `LoopID` from the supervisor pending-gate registry |
| `GET  /sessions/{sid}/gates` | list currently-open gates (reconnect discovery) | from the supervisor's pending-gate registry (`{toolExecutionID, kind, prompt, loopID}`) |
| `POST /sessions/{sid}/interrupt` | stop running turns | `Agent.Interrupt(ctx)` |
| `GET  /sessions/{sid}/export` | export the transcript as a self-contained HTML file | `Agent.ExportSource` → `transcript.Reconstruct` + `html.Render` (same pipeline as the TUI `/export`); requires a **durable** session |
| `DELETE /sessions/{sid}` | shut the session down | `Agent.Close(ctx)` (→ `session.Shutdown`) |
| `GET  /healthz` | liveness/readiness probe (data-free) | none — the one **explicitly unauthenticated** carve-out (see Auth) |

**Event streaming (SSE).** The client `GET`s the events endpoint; `pkg/api` opens a
`Session.SubscribeEvents` and writes each `event.Event` as an SSE frame. Clients
correlate a submitted turn by matching `Header.Cause.CommandID` to the `InputID` returned
by submit, and drain to the turn's terminal event (`TurnDone`/`TurnFailed`/
`TurnInterrupted`) — the drain-to-terminal completion primitive of §3d (**NOT**
`WaitIdle`, which a foreign primary never reaches).

**Transcript export (`GET /sessions/{sid}/export`).** The handler calls
`Agent.ExportSource(ctx)` → `(transcript.RecordSource, transcript.SystemPromptResolver, error)`
and folds them through `transcript.Reconstruct` + `html.Render` (already in looprig from the
transcript-export work), writing `Content-Type: text/html` — a self-contained transcript file
(optionally `Content-Disposition: attachment`). **Persistence caveat:** the headless
**nop-persistence default has no journal**, so `ExportSource` returns a
`*journalsource.ExportUnavailableError` for an in-memory session; the handler `errors.As`-maps
that to a typed **4xx** ("export unavailable for a non-persisted session"). Export therefore
requires a **durable** session (journal wired via the factory, §3f/§4).

**Optional/future endpoints (not v1).** A **session-status** (`GET /sessions/{sid}`) and a
**session-list** (`GET /sessions`) are reasonable later additions for a management UI, but are
**not** v1 — a client can derive turn/idle status from the SSE stream — so the surface stays
narrow.

**AskUser over HTTP (the cloud transport of §3c).** `UserInputRequested` arrives on the
SSE stream like any other event; the client answers by `POST`ing to the gate endpoint with
the `ToolExecutionID`. The client's POST does **not** carry the gate event, so `pkg/api`
maintains a **server-side pending-gate registry** (`ToolExecutionID → {LoopID, Kind
(permission|user-input), prompt/question}`), and the gate-answer handler resolves the required
`LoopID` from it (recall `session.ProvideUserInput`/`Approve`/`Deny` all *require* the loop id).
The registry also backs `GET /sessions/{sid}/gates` (below), so a reconnecting client can
**discover** which gates are open and answer them.

**The index MUST be maintained by a per-session server-side *supervisor*, not by per-client
SSE handlers.** When the runner creates/resumes a session it opens its **own** whole-session
`Agent.Subscribe` — a supervisor goroutine that consumes *all* gate events for that session
and records the pending-gate **registry** `ToolExecutionID → {LoopID, Kind, prompt/question}`
(from the gate event and its `Header`'s `identity.Coordinates.LoopID`), independent of any
client connection; an entry is dropped once its gate resolves. Client SSE streams are
**separate** subscribers (fan-out). This is load-bearing: if the registry lived only inside a
client's SSE handler, a disconnected / late / reconnecting client would miss the gate event
and its later `POST /gates/{id}` could not recover the `LoopID`. With a supervisor, the answer
routes regardless of SSE connection state, and `GET /sessions/{sid}/gates` lets a reconnecting
client **rediscover** the open set (kind + prompt) so it knows what to answer. (An alternative
— the client sends `loopID` in the body — is rejected so the client needn't know loop
topology.) The gates-list endpoint closes the "reconnecting client doesn't know what to answer"
gap; full SSE event **scrollback** replay (missed non-gate events, via the durable journal / a
`Last-Event-ID` cursor) remains a **post-v1** design consideration. Under headless mode
`PermissionRequested` should essentially never appear (a non-allowlisted call denies rather
than asks), so the permission path never parks; only the user-input path needs a responder —
in the cloud that responder is the HTTP client.

**Auth — deferred to a later phase / consumer-owned.** v1 ships **no auth in `pkg/api`** —
plain endpoints. The design *enables* auth without building it: `pkg/api` exposes a standard
**`http.Handler`** (e.g. `Handler(cfg, newAgent) http.Handler` returning the router /
`*http.ServeMux`), so a consumer can wrap it with **any** middleware — OAuth/SSO, a reverse
proxy, an API gateway — at the agent/deployment level, or own auth **entirely** outside this
package. `Serve(...)` is a convenience that binds and runs that handler. `Config` carries an
**optional** middleware hook (`nil` in v1) as the seam to add pkg-level auth in a later
phase. This deferral mirrors the OS-sandbox deferral: a documented, explicit later-phase
concern — see the loopback-default safety stance in §4.

**Server hardening (CLAUDE.md HTTP checklist).** The `http.Server` sets explicit
`ReadTimeout` / `WriteTimeout` / `IdleTimeout` (the SSE handler uses its own flushing
deadline instead of `WriteTimeout`) and `MaxHeaderBytes`; TLS defaults to `MinVersion:
tls.VersionTLS12` and never `InsecureSkipVerify`; every request body is size-limited
(`http.MaxBytesReader`) and validated at the boundary before it becomes domain input; all
session I/O carries a context deadline. Errors are typed.

**Persistence.** The default is **nop** (bare `session.New` persists nothing — ideal for
a stateless cloud tier). A resumable deployment injects the durable journal via the
existing session `Option`s (`WithSessionID` / `WithEventAppender` / `WithCommandAppender`)
and `Restore`, unchanged by this design.

**Posture is per-session and audited.** The `Unattended` intent is selected **per
session** by the consumer's factory (the explicit opt-in of §3), never globally by
`pkg/api`; the construction-time `slog.Warn` posture audit (§4) fires when the gate is
built (workspace + effective policy, no loop id yet), and `pkg/api` additionally logs the
`sid` at session creation so every unattended session is traceable to its declared policy.

## 4. Security

Headless mode is **fail-secure by construction**, honoring the CLAUDE.md security rules:

- **Floor first, always — but it bounds only path-aware tools.** The inner
  `PermissionChecker` runs Containment + HardDeny before it approves anything, and
  `NonInteractiveGate` never turns a deny into an approve. **For path-aware tools**
  (`ReadFile`/`WriteFile`/`EditFile`/`Glob`/`Grep`, whose path args pass through Stage-1
  Containment), a headless agent cannot escape the workspace root or read/write the secret
  paths (`~/.ssh/**`, `**/.env`, `**/*.pem`, `~/.looprig/**`) — blocked identically to
  interactive mode. **This containment does NOT extend to allowlisted `Bash` or the
  network tools** (`Fetch`/`WebSearch`): `Bash` runs `sh -c` and is bounded only by the
  *advisory*, trivially-bypassable command-prefix denylist (`rm -rf /`, `sudo`,
  `curl | bash`, `dd if=`) plus honor-system isolation, and the network tools can reach
  arbitrary hosts. If a definer allowlists them, that blast radius is real (§3, residual
  risk) — the floor contains *paths*, not `Bash` or egress.
- **Least privilege — only the declared allowlist approves.** A would-be prompt (a call
  the definer did **not** allowlist) becomes `EffectDeny`, never `EffectAutoApprove`.
  There is no flood: the risk surface is exactly what the definer declared.
- **Never silent-approve on ambiguity.** Malformed / non-object args resolve to a deny
  through the checker (never auto-approve), and the decorator maps the remaining
  would-be-`EffectAsk` to `EffectDeny`, preserving the `PermissionGate` fail-secure
  contract while guaranteeing no prompt.
- **Deny by default on error.** Any internal error in the checker resolves to
  ask-or-deny; the decorator forces the ask branch to deny, so headless never falls
  through to approve.
- **Audit the posture.** Constructing a `NonInteractiveGate` emits a `slog.Warn`
  (security-relevant configuration is logged, per CLAUDE.md) with **construction-time data
  only** — the workspace root and effective-policy summary — since the loop id is not yet
  minted at checker/gate construction; a loop-id-correlated audit line, if wanted, fires at
  session/loop creation.
- **Deny-read survives headless (floor, not prompt).** The read tools keep the
  floor-backed `loop.ReadGuard` (§3b): `~/.ssh/**`, `**/.env`, `**/*.pem`, `**/id_rsa`,
  `~/.looprig/**`, and `**/.skills/**` stay unreadable. Deny-read is enforced in-tool
  during traversal, not by the (now absent) prompt.
- **Home-resolution fails *closed at construction*, never *open at runtime*.** Home is
  resolved once when the `PermissionChecker` is built; if it can't resolve while any `~/...`
  pattern is configured (hard-deny *or* read-deny), construction returns a typed error and the
  agent never starts (§3b). So an unresolvable home can never make the `~/` secret globs "not
  apply" at runtime and let `HardApprove` auto-approve them, and `DeniedRead` (which sees only
  an absolute path) never has to guess — a defensive runtime deny remains only as a backstop.
- **Wildcard flood is opt-in and loud.** The escape-hatch `HardApprove{["*"]}` is a
  deliberate definer choice; when used it should be paired with the OS sandbox (deferred)
  and is always covered by the `slog.Warn` posture audit.
- **`pkg/api` v1 is unauthenticated — loopback by default (auth deferred).** Plain endpoints
  expose **autonomous (unattended) execution with no gate**, so v1 **binds to `127.0.0.1`
  by default**; binding to a non-loopback/public interface requires an **explicit opt-in**
  in `Config`. It MUST NOT be exposed to an untrusted network without a consumer-supplied
  auth boundary (OAuth/SSO/reverse-proxy middleware wrapping the exposed `http.Handler`,
  §3f) or a trusted network. Non-auth hardening still applies (explicit `http.Server`
  timeouts, `MaxHeaderBytes`, `http.MaxBytesReader` body limits, boundary validation,
  context deadlines; TLS `MinVersion: tls.VersionTLS12` when served over a network —
  loopback dev may be plain HTTP). This deferral mirrors the OS-sandbox deferral: documented
  risk, explicit later phase.
- **The effective permission config is fingerprinted; restore fails closed on a change.**
  There is no session "intent" to fingerprint (the session holds only a `PermissionGate`);
  instead fingerprint the **effective permission/gate config** so a *durable* session
  restored under a different posture or allowlist fails closed. Today
  `event.ConfigFingerprint` fingerprints only the **foreign** `PermissionPosture` (its field
  is documented "empty for a native session") and `ToolPolicyRev` covers only the sorted
  tool *names* (`pkg/session/config_fingerprint.go`) — the native allowlist + gate-wrapping
  is NOT captured, so an allowlist change or an Interactive↔Unattended re-wire would restore
  silently. **This is moot for the stateless (nop-persistence) headless default, which never
  restores; it matters only for durable/resumable sessions**, where adding it makes restore
  compare fail-closed (`checkFingerprint`, `pkg/session/restore_constructor.go`). **Where it
  lives:** a NEW `omitzero` field `NativePermissionPolicyRev string` on `event.ConfigFingerprint`
  — distinct from `ToolPolicyRev` (which stays tool-*names*-only) and from the foreign
  `PermissionPosture` — compared in `ConfigFingerprint.Equal`, computed at the composition root
  and injected via the existing `ConfigFingerprintFields` + `WithConfigFingerprintFields` seam (a
  new corresponding field). Prefer this additive field over redefining `ToolPolicyRev` (preserves
  existing semantics; `omitzero` leaves foreign/legacy records unaffected). **Exact
  digest inputs (define before implementation), serialized canonically (deterministic +
  sorted) so the digest is reproducible:** (1) whether the gate is
  `NonInteractiveGate`-**wrapped** (the effective interactive-vs-unattended bit); (2) the
  `Unattended` checker-option flags — persisted-approvals-disabled,
  effectchecker-autoapprove-suppressed; (3) sorted `HardApprove.Tools`; (4) canonicalized
  `Policies` — each entry `Tool` + `Effect` + **sorted** `Match`, and the entry **list itself
  sorted**; (5) hard-deny **read** list; (6) hard-deny **write** list; (7) hard-deny
  **bash-prefix** list; (8) `MaxReadBytes`; (9) a `policySchemaVersion` (bump when the policy
  shape changes, so old digests are detectably stale). Hash the canonical form into the
  fingerprint; any change flips the digest and restore fails closed.

The trust statement, plainly: an unattended native loop can do anything **its declared
allowlist permits, inside the floor**, with the user's own privileges — it is contained
by the floor and the allowlist, not sandboxed. The floor + allowlist bound blast radius;
they are not an OS sandbox.

## 5. Decisions (settled)

- **Q1 — floor scope:** the floor is **both** non-bypassable stages (Containment **and**
  HardDeny), enforced under every posture. For **path-aware** tools headless never escapes
  the workspace or touches secrets; allowlisted `Bash`/network tools are **not**
  path-contained (§3, §4).
- **Q1a — how are auto-approvals chosen? (supersedes the earlier "literal flood-gate"
  decision).** **RESOLVED as a consumer-declared allowlist + fail-secure decorator, not
  a flood.** The agent/swarm definer declares auto-approvals via the existing
  `PermissionPolicy` (`HardApprove.Tools` + `Policies` `ToolPolicy{Tool,Effect,Match}`,
  the granular workspace permissions list); `NonInteractiveGate` maps any would-be prompt
  to `EffectDeny` so a non-allowlisted call **denies fail-secure and the turn continues,
  never parks**. If a definer allowlists `Bash`/network, that is their explicit
  calculated risk. The blanket wildcard `HardApprove{["*"]}` remains only as an
  **explicit, discouraged escape hatch** (§3), not a posture or default. *(User decision,
  2026-07-01, reversing the prior flood decision after an external code review flagged the
  flood's blast radius.)*
- **Q1b — persisted approvals under `Unattended`:** **DISABLED.** The composition root
  builds the `PermissionChecker` with Stage-5 persisted-approval loading off for the
  `Unattended` intent, so a stale `~/.looprig`/workspace grant can never auto-approve a
  call outside the definer's declared allowlist (the `NonInteractiveGate` decorator cannot
  do this — Stage 5 returns `EffectAutoApprove`, indistinguishable from a declared allow).
  *(User decision, 2026-07-01.)*
- **Q1c — home-unresolvable:** **FAIL FAST AT CONSTRUCTION** (both modes). Home is resolved
  once when the `PermissionChecker` is built; if it can't resolve while any `~/...` pattern is
  configured (hard-deny *or* read-deny), `NewPermissionChecker` returns a typed error and the
  agent never starts — the safe, loud result, and the only clean one since `DeniedRead` sees
  only an absolute path and cannot selectively match `~/...` at runtime (deny-all or fail-open
  would be the alternatives). A defensive runtime deny remains as a backstop; construction
  still succeeds with an unresolvable home if **no** `~/...` rule is configured. Implication:
  a headless container must set `HOME` (or drop the `~/...` rules). *(User decision,
  2026-07-01; refined from runtime-deny to construction-time fail-fast.)*
- **Q1d — `EffectChecker` auto-approve under `Unattended`:** **SUPPRESSED.** The same
  `Unattended` checker option that disables persisted approvals also stops a Stage-3
  `EffectChecker` `EffectAutoApprove` from being honored as an immediate approve — the call
  falls through to the allowlist stages, so a self-approving tool must still be declared. An
  `EffectChecker` `EffectDeny` is still honored (safety veto), and `EffectAsk` still becomes
  `EffectDeny` via the decorator. So the `Unattended` checker option =
  **{persisted-disabled + effectchecker-autoapprove-suppressed}**, guaranteeing "only the
  declared allowlist approves" by construction. *(User decision, 2026-07-01.)*
- **Q1e — `Intent` is a composition-root selector, not session state.** The `Session`/loop
  carries **no** `Intent` field — only a `loop.PermissionGate`. The composition root (the
  consumer's agent factory) uses the intent to choose the gate wiring (native: plain vs
  `NonInteractiveGate`-wrapped; foreign: its `--permission-mode`), then discards it; the
  runner (`pkg/cli`/`pkg/api`) is agnostic and drives whatever session the factory built.
  The fingerprint captures the **effective permission/gate config**, not an abstract intent
  (§4). *(User decision, 2026-07-01: neutral session + composition-applied wrapping.)*
- **Q2 — no-human deployments:** headless removes only **permission** parking; the
  **user-input** (`AskUser`) gate still parks (§3c). So the **driver MUST answer
  `AskUser`** over its transport — supplying at least one of a responder, a timeout, or a
  cancellation for every `UserInputRequested`, and routing the reply by the gate event's
  `LoopID` (§3c). "Never parks on a permission prompt" ≠ "never parks." A **session-level**
  auto-answer / timeout policy is **deferred (YAGNI)** — the responsibility lives
  driver-side, added when a concrete need appears.
- **Q3 — foreign-primary `LoopIdle`:** a **separate spec**, referenced here, not folded
  into this one.
- **Review fixes folded (2026-07-01):** read-side `DeniedRead` fail-closed on
  home-unresolvable (§3b); §4 containment claims scoped to path-aware tools (allowlisted
  `Bash`/network are not contained); `pkg/api` gate routing via a server-side pending-gate
  index (§3f); wildcard wording corrected for `EffectChecker` precedence (§3); and the
  `ToolPolicy.Match` examples corrected — finer-grained per-arg scoping is future work
  (§3b, §7).
- **Concrete API shapes folded (2026-07-01):** (a) `NewPermissionChecker` becomes **fallible
  with options** — `NewPermissionChecker(policy, opts...) (*PermissionChecker, error)` +
  `WithHomeDir` (replaces `SetHomeDir`) + `WithUnattended()`; a breaking change with a narrow
  blast radius (no non-test looprig callers → migrate looprig tests + the swe consumer on
  upgrade) (§3b, §8). (b) The effective-permission fingerprint lives in a new
  `event.ConfigFingerprint.NativePermissionPolicyRev` field, distinct from `ToolPolicyRev`
  (§4, §6). (c) The `pkg/api` supervisor keeps a pending-gate **registry**
  (`ToolExecutionID → {LoopID, Kind, prompt}`) exposed via `GET /sessions/{sid}/gates`, so a
  reconnecting client can discover what to answer; full SSE scrollback replay stays post-v1
  (§3f, §6).

## 6. Testing

Table-driven, `-race`, per CLAUDE.md:

- **`NonInteractiveGate` decorator (unit)** — over a stub `loop.PermissionGate`:
  `EffectAsk → EffectDeny`; `EffectAutoApprove` and `EffectDeny` pass through unchanged;
  `Grant` passes through to the inner gate. Compile-time assert that
  `NonInteractiveGate` satisfies `loop.PermissionGate`.
- **Allowlist behavior (decorator over `PermissionChecker`)** —
  - a tool on `HardApprove.Tools` → `EffectAutoApprove`;
  - a call matched by a `Policies` `ToolPolicy{Effect: EffectAutoApprove, Match}` glob →
    `EffectAutoApprove`; a `ToolPolicy{Effect: EffectDeny}` match → `EffectDeny`;
  - a **non-allowlisted** tool (would be Stage-7 `EffectAsk` in interactive) → `EffectDeny`
    through the decorator (fail-secure, turn continues, never parks);
  - the wildcard escape hatch `HardApprove{["*"]}` → `EffectAutoApprove` for a
    floor-cleared call (documented-caveat row).
- **Floor still denies under headless** — `~/.ssh/**`, `**/.env`, `**/*.pem`,
  `~/.looprig/**`, `**/id_rsa`, `**/.skills/**`, a workspace-escape path, and the denied
  Bash prefixes `sudo` / `rm -rf /` / `curl | bash` / `dd if=` → `EffectDeny`, even if the
  tool is otherwise allowlisted (the floor is non-bypassable); malformed / non-object
  args → `EffectDeny`.
- **Home-unresolvable → construction fails fast (both modes)** — with the home seam forced
  to error, `NewPermissionChecker` returns the typed `HomeUnresolvableError` when a `~/...`
  pattern is configured — asserted for **both** a hard-deny `~/...` rule **and** a read-deny
  `~/...` rule (the `DefaultHardDeny` case). The error is the same in interactive and headless
  (a shared-checker fix). **Construction SUCCEEDS** with an unresolvable home when **no**
  `~/...` rule is configured (a policy with only non-home globs like `**/.env`). Defensive
  backstop: if home is somehow empty post-construction, the containment/hard-deny stage and
  `DeniedRead` **deny** (never auto-approve / never fall open).
- **Persisted approvals ignored under `Unattended` (#1)** — a workspace/user `~/.looprig`
  approval file with an ALLOW record for a tool NOT in the declared allowlist does **not**
  auto-approve it under `Unattended` (Stage 5 disabled): the call resolves to `EffectDeny`
  via the decorator. The same ALLOW record *does* auto-approve under an interactive checker
  with Stage 5 active, proving the option is intent-scoped.
- **`EffectChecker` auto-approve suppressed under `Unattended` (Q1d)** — a stub tool whose
  `CheckEffect` returns `EffectAutoApprove` is **not** auto-approved under the `Unattended`
  checker option unless the tool is *also* on `HardApprove.Tools` (the call falls through to
  the allowlist stages, then denies via the decorator if undeclared); a stub whose
  `CheckEffect` returns `EffectDeny` **still denies** (the safety veto is preserved). Under
  an interactive checker the same self-approving stub *is* auto-approved, proving the
  suppression is intent-scoped.
- **`EffectChecker` beats wildcard; denies under headless (#6)** — with the escape-hatch
  `HardApprove{["*"]}`, a `Skill` workspace load still returns `EffectAsk` at Stage 3 (the
  wildcard does not auto-approve it); wrapped in `NonInteractiveGate` it resolves to
  `EffectDeny` — denied regardless, even under the flood.
- **Not the default + construction is audited** — the zero-value `Intent` / no `Option`
  wires the plain `PermissionChecker` (no decorator); wrapping with `NonInteractiveGate`
  (via its explicit `Option`) emits the `slog.Warn` security event naming the posture,
  workspace, and effective policy — and **no loop id** (not yet minted at construction);
  capture via a `slog` handler and assert the record.
- **`PermissionGate` substitution (loop/runner)** — the loop drives a turn to `TurnDone`
  with `NonInteractiveGate` behind the interface; a non-allowlisted tool call is denied
  and the turn completes, and **no** `PermissionRequested` event is ever published (assert
  against the subscription).
- **`AskUser` under headless** — an `AskUser` call still emits `UserInputRequested` and
  resolves via `ProvideUserInput`; the turn completes after the answer (proves headless
  governs permission gates only).
- **Intent mapping (§3a)** — native `Interactive` wires `PermissionChecker`, native
  `Unattended` wires `NonInteractiveGate{PermissionChecker}` with the same policy; foreign
  intent maps to its **existing** `PostureDefault`/`PostureAcceptEdits` → the pinned
  `--permission-mode` (the foreign enum is preserved, not collapsed).
- **Deny-write floor rows (WriteFile/EditFile e2e)** — one table row plus an end-to-end
  `WriteFile` *and* `EditFile` test per default deny-write glob: `**/.git/config`,
  `**/go.sum`, `**/.looprig/**`, `~/.looprig/**` (`pkg/tools/permission.go`), each
  asserting `EffectDeny` under headless (even when the write tool is allowlisted) and that
  the target file is never written. The write-deny set is a superset of the read-deny set,
  so also assert deny-write for `~/.ssh/**`, `**/.env`, `**/*.pem`, `**/id_rsa`,
  `**/.skills/**`.
- **ReadGuard preserved under headless** — with `NonInteractiveGate` as
  `ToolSet.Permission` and the inner checker passed as the read tools' `ReadGuard`, a
  `ReadFile`/`Glob`/`Grep` over a deny-read path (`~/.ssh/id_rsa`, `**/.env`) is still
  filtered/denied (deny-read is in-tool, not a prompt).
- **Subagent `AskUser` routing** — a subagent loop's `UserInputRequested` is answered by
  `ProvideUserInput` using the **subagent's** `LoopID` (from the event header) and resolves
  that loop's gate; a reply carrying the wrong/zero `LoopID` does not unpark the subagent
  (routes to primary / `SessionLoopNotFound`), proving loop-scoped routing.
- **Restore fingerprint mismatch on posture change** — a session persisted under
  `Interactive` fails closed on restore under `Unattended` (and vice-versa): the two produce a
  different `NativePermissionPolicyRev`. Assert the same fail-closed for an allowlist edit
  (`HardApprove.Tools`/`Policies`), an `Unattended`-flag flip, and the wrapped-bit flip — each
  changes `NativePermissionPolicyRev` and is caught by `checkFingerprint`
  (`pkg/session/restore_constructor.go`).
- **`pkg/api` pending-gate registry is supervisor-maintained (Phase 2, `httptest`)** — with the
  per-session supervisor running but **no active client SSE stream**, an `AskUser` gate is still
  in the registry, so a `POST /sessions/{sid}/gates/{toolExecutionID}` resolves the correct
  `LoopID` and unparks the turn (the registry does not depend on a client connection).
- **`pkg/api` client disconnect → reconnect → answer (Phase 2, `httptest`)** — a client opens
  the SSE stream, disconnects while a gate is pending, then answers via `POST /gates/{id}`
  (with or without reconnecting); the supervisor-held registry still routes the reply and the
  gate resolves — no lost `LoopID`.
- **`pkg/api` gate discovery on reconnect (Phase 2, `httptest`)** — with an `AskUser` gate
  pending and **no active SSE stream**, `GET /sessions/{sid}/gates` lists the open gate (its
  `toolExecutionID`, kind, and prompt) from the supervisor registry; the client then answers via
  `POST /gates/{id}`, resolving the gate — proving a reconnecting client can rediscover what to
  answer without replaying the whole stream.
- **`pkg/api` factory create/resume (Phase 2, `httptest`)** — `POST /sessions` calls the
  `Factory` with `AgentRequest{Resume:false}` and a fresh `sid`; `POST /sessions?resume=<sid>`
  calls it with `Resume:true` and that `sid`; a fake `Factory` asserts it received the right
  request, and the returned narrow `Agent` is registered under `sid` in the many-session map.
- **`pkg/api` transcript export (Phase 2, `httptest`)** — `GET /sessions/{sid}/export` on a
  **durable** session returns `Content-Type: text/html` with non-empty, self-contained HTML
  (assert `Agent.ExportSource` → `transcript.Reconstruct` + `html.Render` is exercised); on a
  **nop-persistence** session the endpoint returns the typed **4xx** that `errors.As`-maps to
  `*journalsource.ExportUnavailableError`.

## 7. Open questions for the plan

- **`Intent` placement** — since the native loop/session never *store* an intent (§3a, §5
  Q1e), no heavy shared enum needs threading through `loop`. `Intent` is a lightweight
  **composition-root selector** the runners read; put it wherever the composition root is
  convenient (a small looprig helper pkg, or even a consumer-side constant) — it need not be
  a leaf both engines import, and `loop`/`session` never reference it. The foreign
  `PermissionPosture` stays foreign-owned (§3a); do **not** lift/rename it or collapse it
  onto the 2-value native vocabulary.
- **Map, don't collapse (settled by §3a)** — the shared `Intent` maps to foreign's
  **existing** `PostureDefault`/`PostureAcceptEdits` at the composition root; do **not**
  replace foreign's enum with the 2-value native vocabulary. Keep the mapping explicit and
  adapter-owned.
- **Composition-root selection** — a CLI flag is deferred (`ba37178`, "turn-key Spec
  resolver for composition-root wiring (CLI flag deferred)", established the resolver seam
  with the flag left for later); for now the intent is a wiring `Option` at the
  composition root, consistent with the foreign builder.
- **Enforce the isolation precondition, or honor-system it? — Resolved (2026-07-01):** v1
  is **explicit-opt-in + honor-system**. Headless ships gated only on the explicit opt-in;
  it does **not** fail closed on a missing isolation boundary in v1. OS-level sandboxing
  and fail-closed isolation-gating (gate the flag on a *detectable* isolation boundary — a
  git worktree / container — and deny if none is detected) are **deferred to a later
  iteration** as documented future work, not a v1 blocker. Especially relevant for
  allowlists that include `Bash`/network.
- **Auth strategy (deferred)** — v1 `pkg/api` is plain + `http.Handler`-exposed so a
  consumer can front it with OAuth/SSO/gateway middleware or own auth entirely at the
  agent/deployment level. Whether looprig later adds an **optional** pkg-level auth
  middleware (via the `Config` hook) or leaves auth **fully consumer-owned** is a
  later-phase decision. v1 safety stance: **loopback-bind by default + explicit opt-in to
  expose publicly** (§4).

## 8. Implementation phasing

- **Phase 1 — Headless posture (looprig `pkg/tools`).** Add the `NonInteractiveGate`
  decorator (`EffectAsk → EffectDeny`, pass-through otherwise); the **construction-time
  home-resolution fail-fast** — `NewPermissionChecker` resolves home once and returns a typed
  `HomeUnresolvableError` if it can't while any `~/...` pattern (hard-deny or read-deny) is
  configured, with a defensive runtime deny as backstop (§3b/§5 Q1c) — this makes
  `NewPermissionChecker` **fallible with options**: `NewPermissionChecker(policy, opts ...Option)
  (*PermissionChecker, error)` + `WithHomeDir` (replaces `SetHomeDir`) + `WithUnattended()` (a
  breaking change; migrate looprig's test-only call sites). `WithUnattended()` **disables
  Stage-5 persisted approvals + suppresses Stage-3 `EffectChecker` auto-approve**. No `Floor` extraction, no flood `OpenGate`. Expose the
  `Intent` **selector** (§3a) — a composition-root/runner concept, **not** a
  `loop.Config`/`Session` field — so the composition root selects `PermissionChecker`
  (interactive) vs `NonInteractiveGate{PermissionChecker}` with the `Unattended` checker
  option (persisted approvals disabled + `EffectChecker` auto-approve suppressed) at session
  construction. Fully unit-testable, **no HTTP/network API** — but it *does* add public Go
  API: the `NonInteractiveGate` type, the `Intent` selector enum, and the `Unattended`
  `PermissionChecker` construction option (no session `Intent` field is added). The
  `NativePermissionPolicyRev` fingerprint wiring (§4) lands here or in Phase 2, wherever the
  composition seam is first touched.
- **Phase 2 — `pkg/api` (looprig).** The reusable, agent-agnostic HTTP runner (§3f): the
  **request-carrying `Factory`** (`AgentRequest{SessionID, Resume}` → create/resume, persistence
  factory-owned) and the many-session map; the **narrow `pkg/api.Agent`** interface (a subset of
  `tui.Agent`, no TUI-only methods — structurally satisfied by existing agents); session / input
  / SSE-events / gate / interrupt / shutdown endpoints; a **per-session supervisor** that holds
  its own whole-session `Subscribe` and maintains the pending-gate **registry** (`ToolExecutionID
  → {LoopID, Kind, prompt}`) independent of client SSE streams, so gate answers route across
  disconnect/reconnect and `GET /sessions/{sid}/gates` lets a reconnecting client rediscover the
  open set;
  the exposed **`http.Handler`** seam + `Serve` convenience; **loopback-default binding**
  (explicit opt-in to expose publicly); and the non-auth CLAUDE.md server hardening. Also the
  **transcript export** endpoint (`GET /sessions/{sid}/export` → `Agent.ExportSource` →
  `transcript.Reconstruct` + `html.Render`, with the `ExportUnavailableError` 4xx for
  nop-persistence sessions), a `GET /healthz` probe, and `ExportSource` in the narrow
  `pkg/api.Agent`. **Auth is explicitly NOT in Phase 2 scope** — plain endpoints,
  consumer-fronted (§3f, §4); an optional pkg-level auth middleware is a later phase.
  `httptest`-driven.
- **Phase 3 — swe wiring (consumer; OUT of looprig scope).** A `cmd/swe-serve` (or an
  extension of `cmd/swe`) that builds swe's agents with the `Unattended` intent **and each
  agent's declared `PermissionPolicy` allowlist**, injects them into `pkg/api`, wires
  credentials + the authenticator, and owns deployment. This is the **swe** repo's
  responsibility, not looprig's.
