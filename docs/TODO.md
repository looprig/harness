# Harness roadmap

This is the cross-cutting product backlog for making harness a complete coding-agent
runtime. Each item should receive a focused design before implementation unless an
approved design is already linked.

## Context and knowledge

- [x] **Automatic context compaction** — design per-loop token accounting, trigger
  thresholds, summary construction, durable compaction boundaries, prompt-cache behavior,
  and restore semantics. Write this as a separate, thoughtful design spec.
- [ ] **Scoped repository instructions (`AGENTS.md`)** — define discovery from workspace
  root to working directory, precedence, size limits, trust classification, fingerprinting,
  and the exact context supplied to primers and delegates.
- [ ] **Persistent memory** — define user, project, and local scopes; ownership per loop
  definition; read/write permissions; curation limits; restore behavior; and separation
  from conversation history and workspace snapshots.

## Tools and extensibility

- [x] **MCP support** — add loop-scoped MCP server configuration, tool discovery,
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
- [ ] **Reclaim orphaned session objects (blocked on released SessionStore APIs)** —
  `pkg/sessionstore` publishes an over-threshold body through released
  `github.com/looprig/sessionstore` `Store.PutObject` BEFORE `storage.AppendDefinite`
  commits the envelope that references it. `PutObject` mints a random 128-bit
  generation per call, so the physical key is `<digest>/<generation>` and RETRYING
  the same record after an append failure (`*storage.ConflictError` from a fenced-out
  writer or ownership handoff, or an ambiguous ack the caller retries) publishes a new,
  permanently unreferenced object every attempt. Harness's compatibility `ObjectGC`
  retains every non-legacy key, and `PutObject`'s own documentation assigns orphan
  reclamation to "the store operator" over this same prefix while noting that its only
  enumeration path (`listObjectReferences`) is unexported, so "no caller-facing GC
  exists yet". Neither side reclaims. The pre-refactor legacy offload keyed on the
  content SHA alone, which made a retry idempotent AND made the object reapable; both
  properties were lost together.

  This is not fixable inside Harness at sessionstore v0.1.0. It needs EITHER of two
  released additions, and Harness should consume whichever lands first:
  1. a caller-supplied or content-derived object generation (an `Option` for the
     unexported `Store.objectGeneration` field, or a documented deterministic
     generation), which restores retry idempotency at one key; or
  2. an exported enumeration + deletion contract over a tenant/session object prefix
     (a public form of `listObjectReferences` plus a guarded delete), which lets
     `ObjectGC` reap the class it currently only counts.

  Until then `GCResult.Unreclaimable` reports the size of the class each pass could
  not see, so an accumulating leak is at least visible to a caller that logs it. See
  `pkg/sessionstore/README.md`, "Object reclamation boundary".

- [ ] **Cap a content block on marshal, not only on unmarshal (Core `content`)** —
  `content.UnmarshalBlock` refuses a serialized block above `maxBlockBytes`
  (8 MiB) with a `*BlockLimitError`; `content.MarshalBlock` has no matching cap.
  A `UserInput` carrying one text block that serializes to between 8388609 and
  16777126 bytes therefore marshals, keeps the whole command body at or below
  `pkg/sessionstore`'s 16 MiB `maxRuntimeBodyBytes` ceiling, passes
  `sessionJournal.frame`'s write-side refusal, is offloaded, and appends — and
  then fails replay permanently with
  `command: decode UserInput: content: block input exceeds block_bytes cap`.
  Measured: text length 8388584 yields a 8388609-byte block in an 8388699-byte
  body (one byte shorter still decodes); text length 16777101 yields a
  16777126-byte block in a body of exactly 16777216 bytes, the largest the append
  guard admits.

  This is the same fail-closed-but-unrecoverable hazard the runtime-body ceiling
  closed, one level down and at a lower threshold. It is **pre-existing** — the
  identical command failed identically on replay before that guard existed — and
  it is not fixable inside Harness: the missing cap is in Core's codec, and a
  Harness-side block-size check would duplicate a constant Harness does not own.
  Fix is a marshal-side cap in `github.com/looprig/core/content` (symmetric with
  `UnmarshalBlock`, same `*BlockLimitError`), consumed here by a Core version
  bump. Until then, admission past `maxRuntimeBodyBytes` does not imply the body
  can be decoded; see `pkg/sessionstore/replay.go`'s `maxRuntimeBodyBytes` doc and
  `pkg/sessionstore/README.md`.

## Safety and observability

- [x] **Permission auto-review classifier and tool-using Hustles** — a bounded,
  evidence-gathering classifier can auto-approve a low-risk permission gate once,
  racing (never blocking) the ordinary human response; every non-eligible outcome
  leaves the human gate exactly as open as it always was. `pkg/gate` owns the
  neutral review domain and local decision policy; `pkg/hustle` owns the bounded
  tool-use loop; the actual classifiers (prompts, wire codecs, evidence-tool
  catalogs, evaluation corpus) live in the separate `looprig/classifiers` module,
  which harness never imports. Off by default: zero registered classifiers
  preserves prior gate behavior byte-for-byte. See
  [`2026-07-27-permission-classifier-hustle-design.md`](plans/2026-07-27-permission-classifier-hustle-design.md)
  and [`pkg/gate/README.md`](../pkg/gate/README.md#permission-review) for
  enable/disable, model capability requirements, evidence boundaries, human
  fallback, audit/privacy, policy tuning, evaluation workflow, and restore
  behavior. This directly informs and partially overlaps the
  "Untrusted-content classification" item below (provenance/classification for
  tool-triggered risk) without completing it — that item's broader scope
  (file/web/MCP/delegate-report content classification, independent of
  permission review) remains open.
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
  modes, delegates, and controllers; foreground and background agent collaboration
  through StartAgent, MessageAgent, ListAgents, and StopAgent;
  workspace placement, snapshots, rewind, and file freshness; security boundaries;
  and migration from the legacy harness APIs. Compile and test example programs in CI
  to prevent drift. Extend these guides as MCP/hooks/memory/artifacts/tracing land.
## Explicitly deferred

- Token/cost budgets beyond the existing delegation and per-turn tool limits.
- Rewind/undo semantics. Workspace restore and conversation forking remain separate
  concepts.
- Additional per-loop workspace isolation. The rig owns one workspace; tools retain their
  existing atomic file-edit and permission/confinement responsibilities.
