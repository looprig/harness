# Harness roadmap

This is the cross-cutting product backlog for making harness a complete coding-agent
runtime. Each item should receive a focused design before implementation unless an
approved design is already linked.

## Context and knowledge

- [ ] **Automatic context compaction** — design per-loop token accounting, trigger
  thresholds, summary construction, durable compaction boundaries, prompt-cache behavior,
  and restore semantics. Write this as a separate, thoughtful design spec.
- [ ] **Scoped repository instructions (`AGENTS.md`)** — define discovery from workspace
  root to working directory, precedence, size limits, trust classification, fingerprinting,
  and the exact context supplied to primers and delegates.
- [ ] **Persistent memory** — define user, project, and local scopes; ownership per loop
  definition; read/write permissions; curation limits; restore behavior; and separation
  from conversation history and workspace snapshots.

## Tools and extensibility

- [ ] **MCP support** — add loop-scoped MCP server configuration, tool discovery,
  authentication boundaries, approval policy, elicitation, cancellation, result-size
  limits, and durable audit events.
- [ ] **Execution hooks** — implement the approved
  [harness execution hooks design](plans/2026-07-08-harness-execution-hooks-design.md),
  including lifecycle ordering, timeout/failure policy, and tool/turn coverage.
- [ ] **Long-running commands** — design supervised background processes, PTY and
  non-PTY execution, bounded output, polling/streaming, cancellation, process-group
  teardown, session shutdown, and restore behavior.
- [ ] **Structured outputs** — support schema-bound loop and delegate results with strict
  validation, typed errors, provider capability negotiation, and a text fallback policy.
- [ ] **Artifacts** — define durable named outputs (files, reports, diffs, images, logs),
  content-addressed storage, event references, retention, access control, and handoff
  between loops and clients.

## Inference resilience

- [ ] **Retries and rate limits** — classify retryable provider failures, honor provider
  retry hints, use bounded backoff/jitter, preserve request identity, and prevent duplicate
  tool or message commits.
- [ ] **Model/provider fallback** — define ordered fallback policy, capability
  compatibility, context-window constraints, tool/schema compatibility, cost/security
  boundaries, observable selection events, and restore behavior.

## Sessions and orchestration

- [ ] **Conversation and loop forking** — define whether a fork copies committed history,
  mode/model state, permissions, workspace view, and prompt-cache prefix; give the fork a
  new identity; and specify steering, delegate restrictions, persistence, and result
  handoff. Forking is not rewind and must not mutate the source history.
- [ ] **Control-plane authorization** — define which callers may change the active loop,
  loop mode, model, effort, security ceiling, or other runtime policy; distinguish trusted
  host/user actions from model/tool actions; enforce capability attenuation for delegates;
  and journal every accepted change.
- [ ] **Park idle delegate loops** — idle children stay in memory for now: their
  committed history is a reusable asset (a warm `send` beats respawning and re-briefing),
  `DelegationLimits.Quota` bounds accumulation, and there is deliberately no model-facing
  `stop` (see the
  [workspace placement amendments](plans/2026-07-11-workspace-placement-modes-design.md)).
  When memory profiles demand it, evict an idle delegate's in-memory history and actor
  state and rehydrate from the session journal on the next `send` — identity, semantics,
  and events unchanged; a purely internal resource optimization.

## Safety and observability

- [ ] **Untrusted-content classification** — classify file contents, command output, web
  results, MCP data, artifacts, and delegate reports as data rather than instructions;
  propagate provenance labels; add prompt-injection detection/policy hooks; and ensure a
  classifier cannot silently grant authority.
- [ ] **Tracing and usage telemetry** — emit correlated session/loop/turn/step/tool spans,
  model usage and latency, retry/fallback decisions, gate waits, snapshot activity, and
  delegate relationships; define redaction and export interfaces without persisting
  secrets or private reasoning.

## Documentation integration

- [ ] **End-user guides and runnable examples** — after the rig lifecycle and workspace
  implementation lands, document how `rig`, `loop`, `session`, `storage`,
  `workspacestore`, and `tools` compose; the complete Rig/Session/Loop model; primers,
  modes, delegates, and controllers; synchronous and managed Subagent communication;
  workspace placement, snapshots, rewind, and file freshness; security boundaries;
  and migration from the legacy harness APIs. Compile and test example programs in CI
  to prevent drift. Extend these guides as MCP/hooks/memory/artifacts/tracing land.
- [ ] **CLI migration spec and implementation plan** — only after the end-user guides
  and examples pass CI, design the CLI move to the documented rig/session/workspace
  lifecycle. Keep it separate from the harness implementation spec.
- [ ] **SWE migration spec and implementation plan** — only after the CLI migration
  plan, design SWE primers, delegates, modes, tool construction, workspace policy, and
  session lifecycle on the documented public surface. Keep it separate from both the
  harness and CLI migration specs.

## Explicitly deferred

- Token/cost budgets beyond the existing delegation and per-turn tool limits.
- Rewind/undo semantics. Workspace restore and conversation forking remain separate
  concepts.
- Additional per-loop workspace isolation. The rig owns one workspace; tools retain their
  existing atomic file-edit and permission/confinement responsibilities.
