# Design B — Invoke Control Semantics & Autonomous Headless Mode

> **SUPERSEDED (2026-07-01) by [Open Gate — Posture-Driven Unattended Tool Approval](2026-07-01-open-gate-posture-design.md).** This design's foundations are gone from `main`: `Session.Invoke`/`Session.Stream` were deleted, the Design-A event taxonomy + `Sink`/`NotificationEvent` were deleted or never built, and the declared agents it targeted never landed on `main`. Kept for history; see the successor for the current plan.

**Date:** 2026-06-16
**Status:** Approved design, pending implementation plan
**Depends on:** ⚠️ **stale** — the former [Design A — Session Observability & Taxonomy Foundation] has been superseded by [Cleanup — Remove Event Sink, Redaction & EventEnvelope Scaffolding](2026-06-19-remove-sink-redaction-envelope.md). The concepts this doc leans on (`ControlEvent`/`TerminalEvent` markers, envelope delivery, unified `Sink`, `NotificationEvent`) were either dropped ("add when needed"), built differently on main (`Class`×`Scope`+`Reply`+`EndsTurn`, `Header`-carried identity, hub `Subscription`), or deleted (the sink path). **This design needs its own reconciliation against main before implementation.**

## Motivation

Run the declared agents (`agents/personal-assistant`, `agents/coding`) **headless**
— from custom Go code, unattended, no TUI/CLI — driving a turn to completion with
tool calls auto-approved, while observability stays complete.

The existing `session.Invoke` (`session/agent.go:101`) blocks for a terminal event
and **silently discards** non-terminal events — including `PermissionRequested`
and `UserInputRequested`. Those gate events are produced by the runner, which then
**parks the turn** waiting for a control command reply (`runner.go:294`
`askPermission` "blocks for the user's decision"). The reply travels on a separate
channel (`Commands`), not the events channel `Invoke` reads. So an unanswered gate
makes `Invoke` hang until `ctx` is cancelled — a silent footgun.

The fix is principled: control events are first-class (Design A's `ControlEvent`),
`Invoke` must never silently swallow them, and "autonomous" means **never prompt** —
auto-approve what is safe (within the permission floor) and auto-resolve the rest,
so the turn always completes.

## Key invariant: the safety floor is non-bypassable

"Approve all tool calls" reuses the existing wildcard sentinel in the
permission checker (`tools/check.go:48` `wildcardTool = "*"`,
`stageHardApprove` `check.go:353`). Crucially, `HardApprove` is **Stage 4** —
after the two non-bypassable safety stages (`check.go:122` `Check`):

- **Stage 1 Containment** — a path arg escaping the workspace root is denied.
- **Stage 2 HardDeny** — `~/.ssh/**`, `**/.env`, `**/*.pem`, the `.urvi` store,
  `rm -rf /`, `sudo`, `curl | bash`, `dd if=` are denied (`permission.go:97-123`).

So even wildcard autonomous mode **cannot** read SSH keys/secrets, escape the
workspace, or run the denied commands. Autonomous = "auto-approve within the
floor," not "bypass safety." (User decision: keep the floor.)

---

## B1. `Invoke` control-event semantics

`Invoke` is, by definition, the API that does **not** send a control command back
(a blocking single-return call cannot surface-then-resume — that is `Stream`). So
`Invoke` is only coherent when no control event needs a human answer. Design A's
`ControlEvent`/`TerminalEvent` markers let `Invoke` reason about this precisely.

### Static capability + up-front validation (fail fast, not lazily)

- New static tool capability marker `tool.Interactive` — implemented by tools that
  raise a control event by their nature. `AskUser` implements it (it always raises
  `UserInputRequested`).
- The permission gate exposes a static query (e.g. `UnconditionallyApproves(name)
  bool`) so the session can ask "is tool `T` always approved (in `HardApprove` /
  wildcard)?" without args.

### Behaviour

- **Non-autonomous `Invoke`:** validate **before starting the turn** that the
  session is *non-interactive-safe* — no registered `Interactive` tool **and**
  every registered tool unconditionally hard-approved. If not, return a typed
  `InteractiveSessionError` immediately (never starts the turn, never hangs). This
  catches the AskUser case and the "tools need permission but you didn't opt into
  autonomous" case **up front**, not mid-turn. (Defensive backstop: if a
  `ControlEvent` still arrives at runtime, interrupt the turn and return the typed
  error — never discard-and-hang.)
- **Autonomous `Invoke`:** auto-resolve any residual `ControlEvent`
  **fail-secure** — deny residual permission asks, cancel `UserInputRequested` —
  and run straight to the terminal event, returning one blocking output. Residual
  asks are rare (malformed/missing-path args that `Check` refuses to auto-approve
  even under wildcard — `check.go:145`); denying them is correct fail-secure
  behaviour and the turn continues (the model receives a denied tool result).
- **Both modes:** every event still flows to `Sink`s via the independent publish
  fan-out (Design A), so an autonomous `Invoke` that returns only the final output
  still yields the **complete (redacted) journal** of commands + events.

`Stream` is unchanged — it is the interactive path: it surfaces `ControlEvent`s
and the caller answers with the existing `session.Approve`/`Deny`/
`ProvideUserInput`. Use `Stream` whenever a human must answer gates.

| | raises gates? | how caller responds | token deltas |
|---|---|---|---|
| `Stream` (interactive) | yes (yields them) | `Approve`/`Deny`/`ProvideUserInput` inline | yes |
| `Invoke` non-autonomous | fails fast up front if the session can gate | n/a (use `Stream`) | no (collapsed) |
| `Invoke` autonomous | gates auto-resolve → none require a human | n/a | no (collapsed) |

## B2. Autonomous mode on the declared agents

`WithAutonomous()` functional option on `personalassistant.New` / `coding.New`
(the functional-option idiom is already used in the codebase, e.g.
`internal/llm/openaiapi/chutes/client.go:77`). Backward-compatible (existing
`New(ctx)` unchanged); greppable for audit; emits a `slog.Warn` at construction
(CLAUDE.md: log security-relevant configuration).

`WithAutonomous()` configures the session so it is non-interactive-safe by
construction:

1. **Wildcard `HardApprove{Tools: []string{"*"}}`** — broad auto-approve at
   `Check`, floor preserved (Stages 1–2 still deny). Replaces each agent's fixed
   `autoApprovedTools` list (`personal-assistant/agent.go:115`,
   `coding/agent.go:125`).
2. **Drop `Interactive` tools** (AskUser) from the registry at construction —
   least privilege; a tool that needs a human is dead weight unattended.
3. **Auto-resolving `Invoke` path** (B1) for residual control events.

### Coding agent's `Subagent` under autonomy

The coding agent registers `Subagent` (`coding/agent.go:125`), which spawns child
agents via a `SubagentFactory`. If the parent is autonomous but children are not, a
child's tool call re-introduces the hang. Decision: **propagate autonomy to
children** — thread the flag into the `SubagentFactory` so child sessions are also
autonomous. (Keeps delegation working unattended; adds factory wiring. The
personal assistant has no Subagent and is unaffected.)

### Usage

```go
a, _ := coding.New(ctx, coding.WithAutonomous())
defer a.Close(context.Background())
ev, err := a.Send(ctx, "refactor X and run the tests")
// blocks, returns the final outcome; err != nil only on transport failure.
// Sinks hold the full (redacted) command+event trail.
```

## B3. Notification emissions + TUI unification

Design A defines the `NotificationEvent` category; this section emits concrete
ones and renders them.

### Emit (in-loop, no llm-layer change required)

| Event | Severity | Carries | When |
|---|---|---|---|
| `SessionStarted` | Info | SessionID | reclassified; loop already emits it |
| `ToolCallAutoApproved` | Info | ToolCallID, tool, **rule** (hard-approve / workspace-persisted / session-policy / wildcard) | `Check` returns auto-approve |
| `ToolCallAutoDenied` | Warn | ToolCallID, tool, reason (containment / hard-deny / malformed) | `Check` denies, or autonomous auto-denies a residual |

These fire in **interactive mode too** (free audit), and are `Redactable` if they
carry a path (sink copy keeps tool + rule + ToolCallID, drops the path).

### TUI — one source, existing renderer

The TUI already has a leveled notice system: `noticeLevel`
(`noticeInfo`/`noticeWarn`/`noticeError`, `tui/transcript.go:51-57`),
`CommitNotice` (`transcript.go:180`), level-coloured accent bars
(`entryrender.go:41-54`). The agent-level `NotificationEvent` becomes the single
**source**; the TUI just renders it. Add one case to `transcript.ApplyEvent`:

```go
case event.NotificationEvent:
    m = m.CommitNotice(mapSeverity(ev.Severity()), ev.Message())
```

Keep `event.NotificationSeverity` and `noticeLevel` as **separate typed enums**
with an explicit `mapSeverity` switch — no raw cross-package cast coupling their
integer values. `TurnFailed` keeps rendering as an error notice as today;
notifications just join the same UI path. No new TUI component.

### Deferred (own follow-up specs)

- `ModelFallbackEngaged` (Warn) — the fallback-model retry path is not built yet
  (`docs/old/2.md` chunk-08).
- `AttestationDegraded` (Warn/Error) — today TEE attestation is **fail-closed**:
  `llm.AttestationError` → `client.Stream` error → `turn.go:183 streamFailure` →
  `TurnFailed`. There is **no non-fatal warning channel** across the `llm.LLM`
  interface (`llm.go:13` returns only `(stream, error)`; `Response` has no
  `Warnings`; `StreamReader` has no side-channel), and no degraded-trust *mode*.
  Surfacing attestation as a non-fatal notification needs both a new diagnostic
  channel and a trust-mode decision — security-sensitive, its own design. The
  `NotificationEvent` category is built to receive it later; until then
  attestation stays a hard `TurnFailed`.

---

## Error handling

- `InteractiveSessionError` (typed) — non-autonomous `Invoke` on a session that
  can raise control events; returned **before** the turn starts, naming the
  offending tool/reason and pointing to `Stream`/`WithAutonomous`.
- Autonomous residual control events resolve to the existing fail-secure
  `DenyToolCall` path — no new error; the model sees a denied tool result.
- `WithAutonomous` logs (not errors) at construction.

## Testing

- **Up-front validation:** non-autonomous `Invoke` on a session with `AskUser`
  (or a non-hard-approved tool) returns `InteractiveSessionError` and **never
  starts a turn** (assert no `TurnStarted` reaches the sink).
- **Autonomous completion:** with wildcard + dropped AskUser, an `Invoke` whose
  model emits write/bash calls runs to `TurnDone` with no parked gate; a
  malformed-arg residual is auto-denied (fail-secure) and the turn still
  completes.
- **Floor preserved:** autonomous `Invoke` attempting a hard-denied path/command
  (`~/.ssh`, `sudo`, workspace escape) is denied by Stage 1/2 (table over the
  floor rules) — wildcard does not override it.
- **Sinks complete under autonomy:** an autonomous `Invoke` returning only the
  terminal output still produces the full redacted command+event journal,
  including `ToolCallAutoApproved`/`ToolCallAutoDenied` notifications with the
  deciding rule.
- **Subagent propagation:** an autonomous coding agent's child session is
  autonomous (a child tool call does not park).
- **TUI rendering:** a `NotificationEvent{Severity: Warn}` renders as a
  warn-level notice via the existing `CommitNotice` path (`mapSeverity` table).
- **Stream unchanged:** interactive `Stream` still yields `ControlEvent`s and
  resolves via `Approve`/`Deny`/`ProvideUserInput`.
- Run all with `-race`.

## Implementation order (for the plan)

1. `tool.Interactive` marker + gate `UnconditionallyApproves` query.
2. `Invoke` up-front validation + typed `InteractiveSessionError`.
3. Autonomous control-event auto-resolution on the `Invoke` path.
4. `WithAutonomous()` on the declared agents (wildcard, drop interactive tools,
   `slog.Warn`); Subagent autonomy propagation.
5. Notification emissions (`SessionStarted`/`ToolCallAutoApproved`/
   `ToolCallAutoDenied`).
6. TUI `NotificationEvent` rendering.
