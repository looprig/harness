# Subagent Tool Parity Upgrade Design

**Date:** 2026-07-31

**Status:** Approved

## Goal

Upgrade Harness's model-facing `Subagent` tool toward the current Claude Code
`Agent` invocation model without weakening Looprig's parent-scoped delegation
security or removing its richer managed-child lifecycle.

This is a compatibility-first semantic upgrade, not a claim of exact Claude
wire compatibility:

- the model-facing name remains `Subagent`;
- the existing `start`, `send`, `wait`, `interrupt`, and `status` actions remain
  one control surface;
- `start` and `send` adopt Claude-style `description`, `prompt`,
  `subagent_type`, and `run_in_background` vocabulary;
- argument preparation becomes the only decode and validation boundary;
- every success has a stable JSON result shape;
- controller failures are mapped to bounded model-safe errors and never expose
  raw internal error text.

The separate loop-scoped Task tool design remains unchanged in ownership and
lifetime: Task tools are optional utilities in `github.com/looprig/tools`, while
`Subagent` remains an automatically injected Harness control-plane capability.

## Reviewed Baseline

This design was checked against:

- Harness main at `c768c252`;
- `.worktrees/long-running-commands/harness` at `aaefa01c`;
- Claude Code `2.1.220` and the current official subagent documentation.

The long-running-command branch does not change
`internal/delegationtool/subagent.go`. It does, however, make the intended tool
execution boundary especially important:

- `tool.CallPreparer` validates a call once and returns a per-call
  `tool.PreparedArtifact`;
- the runner carries that artifact in `tool.PreparedCall`;
- execution reads it with `loop.PreparedCallFromContext`;
- tools must not re-decode mutable raw arguments after preparation;
- supervised process and session-resource contracts are separate capabilities,
  not a replacement transport for in-session child Loops.

The current Subagent implementation returns an empty prepared artifact and
performs all decoding in `InvokableRun`. That is the primary integration defect
this design corrects.

## Current Surface

Harness currently exposes one `Subagent` tool with this flat envelope:

| Action | Required fields | Optional fields | Current default |
|---|---|---|---|
| `start` | `agent`, `message` | `mode`, `wait`, `timeout_seconds` | omitted `action` means `start`; omitted `wait` means `true` |
| `send` | `delegate_id`, `message` | `wait`, `timeout_seconds` | omitted `wait` means `true` |
| `wait` | `delegate_id`, `request_id` | `timeout_seconds` | waits |
| `interrupt` | `delegate_id` | none | immediate control result |
| `status` | none | `delegate_id` | all owned children when omitted |

`DelegationSyncOnly` advertises only synchronous `start`.
`DelegationManaged` advertises all five actions.

The current controller already provides the important runtime behavior:

- parent-scoped delegate authorization;
- configured agent and mode validation;
- child ownership checks;
- depth and cumulative spawn quotas;
- synchronous and queued turns;
- request correlation;
- follow-up, wait, status, and interrupt;
- durable reconstruction of resolved requests after restore.

Those controller semantics are retained.

## Claude Code Reference and Deliberate Differences

Claude Code renamed its `Task` delegation tool to `Agent` in v2.1.63. In the
reviewed 2.1.220 tool schema, `Agent` requires `description` and `prompt`, and
accepts optional `subagent_type`, `run_in_background`, `model`, and `isolation`.
The official documentation also describes independent subagent context,
foreground/background execution, per-invocation model selection, and optional
worktree isolation.

Looprig adopts the invocation concepts that its current runtime can represent
truthfully:

| Claude concept | Looprig V1 decision |
|---|---|
| `Agent` tool name | Keep `Subagent` to avoid collision with the Agent domain and preserve existing consumers. |
| `description` | Adopt for a short model-authored call label. Required for `start`. |
| `prompt` | Adopt as the canonical child-turn input for `start` and `send`. |
| `subagent_type` | Adopt as the configured delegate name. Required because Looprig has no implicit unrestricted general-purpose delegate. |
| `run_in_background` | Adopt. It is the inverse of the controller's internal `Wait` field. |
| `model` | Do not expose in V1. Looprig models are provider-qualified descriptors, not portable Claude aliases. A later design must provide a bounded configured selector and preserve transport/context invariants. |
| `isolation` | Do not expose in V1. All Loops currently receive a fresh observation set over one session workspace/coordinator. Per-child worktrees require their own placement, checkpoint, lease, process-CWD, cleanup, and restore design. |
| resume/follow-up | Retain Looprig's `send` action and stable child UUID. |
| output/status/stop | Retain `wait`, `status`, and `interrupt`; they are more explicit than overloading a generic background-task API. |

Unknown `model` and `isolation` fields therefore fail strict decoding rather
than being accepted and ignored. That fail-closed behavior is intentional.

References:

- <https://code.claude.com/docs/en/sub-agents>
- <https://code.claude.com/docs/en/agents>

## Proposed Model-Facing API

The envelope stays flat and `additionalProperties` remains `false`. `action`
may be omitted only for `start`.

### Start

```json
{
  "description": "Map repository structure",
  "prompt": "Inspect the repository and summarize its package boundaries.",
  "subagent_type": "explorer",
  "mode": "review",
  "run_in_background": true
}
```

Fields:

| Field | Required | Meaning |
|---|---:|---|
| `action` | no | `start`; omission also means `start`. |
| `description` | yes | Short call label. Guidance asks for 3-5 words; runtime enforces non-whitespace and byte bounds, not English word counting. |
| `prompt` | yes | Initial child user turn. |
| `subagent_type` | yes | One delegate in the parent definition's frozen allowlist. |
| `mode` | no | One mode declared by that delegate definition. Omission uses its initial mode. This is a Looprig extension. |
| `run_in_background` | no | Managed style defaults to `true`; sync-only style defaults to and requires `false`. |
| `timeout_seconds` | no | Foreground wait bound. Forbidden when `run_in_background` resolves to `true`. |

The start schema derives `subagent_type` and `mode` choices from the immutable
delegate catalog exactly as the current `agent` and `mode` schema does.

### Send

```json
{
  "action": "send",
  "delegate_id": "55555555-5555-4555-8555-555555555555",
  "prompt": "Now inspect the restore path.",
  "run_in_background": false,
  "timeout_seconds": 60
}
```

`delegate_id` and `prompt` are required. Managed mode defaults
`run_in_background` to `true`. `timeout_seconds` is allowed only for a
foreground send.

### Wait

```json
{
  "action": "wait",
  "delegate_id": "55555555-5555-4555-8555-555555555555",
  "request_id": "66666666-6666-4666-8666-666666666666",
  "timeout_seconds": 60
}
```

The two IDs are required. The request ID identifies one queued start/send turn.

### Interrupt

```json
{
  "action": "interrupt",
  "delegate_id": "55555555-5555-4555-8555-555555555555"
}
```

The child Loop stays registered; only its current turn is interrupted.

### Status

```json
{"action":"status"}
```

An omitted `delegate_id` returns every child owned by this parent. A supplied
ID returns only that child. Status remains bounded mechanical state and pending
request counts, never transcript content or event cursors.

## Hard-Cut Migration

The model-facing field rename is a hard cut:

| Removed | Replacement |
|---|---|
| `agent` | `subagent_type` |
| `message` | `prompt` |
| `wait: true` | `run_in_background: false` |
| `wait: false` | `run_in_background: true` |

The implementation does not accept both spellings, hidden aliases, or a
dual-schema transition. Keeping aliases would enlarge the untrusted envelope,
create contradictory boolean combinations, and make examples nondeterministic.
All Harness and consumer fixtures change in the same release.

The internal `tool.DelegateRequest` retains `Agent`, `Message`, and `Wait`; these
are controller-domain names, not model-facing JSON. The prepared tool adapter
performs the canonical translation once.

## Defaults and Style Rules

For `DelegationManaged`:

- omitted `run_in_background` on `start` or `send` means `true`;
- background calls return a queued handle;
- foreground calls wait for a terminal result or their optional timeout.

For `DelegationSyncOnly`:

- only `start` is advertised and accepted;
- omitted `run_in_background` means `false`;
- an explicit `true` is rejected during preparation;
- the schema constrains the field to `false`.

The scoped controller re-enforces the style even if a caller bypasses the
schema or constructs a `tool.DelegateRequest` directly.

## Boundary Validation and Limits

The Subagent tool defines private V1 bounds:

```go
const (
	maxSubagentArgsBytes   = 256 << 10
	maxDescriptionBytes    = 256
	maxPromptBytes         = 192 << 10
	maxTimeoutSeconds      = 24 * 60 * 60
)
```

Preparation:

1. rejects an argument document over `maxSubagentArgsBytes` before decoding;
2. requires exactly one top-level JSON object;
3. uses `json.Decoder.DisallowUnknownFields`;
4. rejects trailing JSON;
5. applies action-specific allowed/required fields;
6. validates canonical UUIDs and configured catalog selections;
7. rejects blank or oversized descriptions/prompts;
8. rejects negative or over-limit timeouts;
9. rejects timeouts on background start/send calls;
10. translates the result into one `tool.DelegateRequest`.

Preparation returns a typed, private artifact:

```go
type subagentArtifact struct {
	tool.TokenArtifact
	request     tool.DelegateRequest
	description string
}
```

The embedded token is the call execution UUID string. `InvokableRun` requires a
runner-installed `tool.PreparedCall`, requires exactly this artifact type,
requires the token and prepared execution ID to match, copies its request, adds
the provider tool-use ID from context, and invokes the controller. It never
decodes `argsJSON`.

A missing, nil, wrong-type, or mismatched artifact fails closed as a stable
tool-result error and never reaches the controller. Mutating raw arguments
between preparation and execution cannot change the operation.

The prepared `tool.Request` is empty because spawning an authorized child Loop
does not itself request an OS capability. The child still uses its own access
gate for every effectful tool. `Subagent` remains `Auditable` and deliberately
does not implement `WriteTarget` or `Sequential`.

## Result Contract

Every success is one JSON object in one text block.

Queued start/send:

```json
{
  "delegate_id": "55555555-5555-4555-8555-555555555555",
  "request_id": "66666666-6666-4666-8666-666666666666",
  "status": "queued"
}
```

Completed foreground start/send or wait:

```json
{
  "delegate_id": "55555555-5555-4555-8555-555555555555",
  "request_id": "66666666-6666-4666-8666-666666666666",
  "status": "completed",
  "output": "bounded child response"
}
```

Timed out, interrupted, or failed terminal results use the same IDs and a
bounded `status`, omitting `output` unless the status is `completed`.

Single-child status:

```json
{
  "delegate_id": "55555555-5555-4555-8555-555555555555",
  "status": "running",
  "pending_requests": 2
}
```

All-child status:

```json
{
  "children": [
    {
      "delegate_id": "55555555-5555-4555-8555-555555555555",
      "status": "idle",
      "pending_requests": 0
    }
  ]
}
```

Results are marshaled from typed response structs with `encoding/json`, not
assembled through string concatenation. Child output remains model-visible by
design but is never included in audit summaries or internal diagnostic logs.

## Error Contract

Preparation failures are typed Go errors because they occur before execution.
Their messages are bounded and never echo the description, prompt, IDs, or raw
JSON. Examples include:

- `Subagent: invalid arguments`
- `Subagent: field is unavailable for action`
- `Subagent: prompt is required`
- `Subagent: timeout is out of range`

Runtime/controller failures are model-visible `error:` tool results with nil Go
errors. The adapter classifies only stable delegation failure codes. Unknown
controller errors become the generic `error: subagent operation failed`; the
implementation never appends `err.Error()`.

To avoid importing `internal/sessionruntime`, `pkg/tool` owns the small public
error-code vocabulary used by the controller seam. `sessionruntime.DelegateError`
implements that classification contract. Internal causes remain available to
logs and hooks through error wrapping, but not to the model result.

## Ownership and Security Invariants

- The parent definition's frozen delegate set remains the authorization source.
- The schema is model guidance, never the security boundary.
- The parent-scoped controller rechecks action availability, delegate
  authorization, selected mode, child ownership, depth, and quota.
- A child keeps its own tool set, access gate, permissions, and Loop-local Task
  store.
- Background draining uses the session lifetime, not the completed parent tool
  call context.
- A request ID always identifies one non-folding child turn.
- Status never exposes transcripts, prompts, event cursors, or unrelated loops.
- Audit summary remains the constant `Subagent`.
- Subagent is not a supervised process resource. The long-running-command
  process registry and delegation registry remain separate typed domains.

## Restore and Long-Running-Command Interaction

This change does not alter durable Loop identity or request correlation.
Existing restore logic still reconstructs child ownership and resolved request
terminals from durable events.

The in-memory pending-request map remains the live handle registry. A queued
delegate turn continues after the parent tool call returns because its drain is
rooted in the session context. This is analogous to, but intentionally separate
from, the long-running-command branch's supervised process lifecycle:

- child Loop work is controlled through `DelegateController`;
- OS processes are controlled through `ProcessBinding` and session resources;
- neither ID can be passed to the other's tools;
- neither registry is widened to a generic untyped task registry.

## Testing Strategy

The change is pinned at four boundaries:

1. `internal/delegationtool`: exact schema, strict preparation, artifact
   isolation, safe JSON formatting, safe error mapping, and fuzzing;
2. `pkg/tool`: delegation error-code contract and defensive value behavior;
3. `internal/sessionruntime`: style, authorization, ownership, queue/wait,
   timeout, interrupt, status, restore, and error classification;
4. consumer integration: updated start/send fixtures and proof that each child
   keeps independent optional Task state.

All Go tests run with `-race`. The parser fuzz target moves from
`InvokableRun` to `PrepareCall`, because preparation becomes the only untrusted
JSON boundary.

## Deferred Follow-Ups

### Per-invocation model selection

A later design may add optional `model` only after defining:

- a consumer-configured, bounded selector catalog;
- provider-qualified model resolution;
- validation against the child definition's inference transport and context
  counter capability;
- deterministic event/fingerprint/restore representation;
- behavior when `mode` and `model` are both supplied.

### Per-child worktree isolation

A later design may add optional `isolation: "worktree"` only after defining:

- child-specific workspace placement and coordinator ownership;
- checkpoint and restore semantics;
- process working-directory confinement;
- cleanup behavior for clean, dirty, interrupted, failed, and restored children;
- root lease behavior and mutation serialization across the parent and child.

`remote` isolation is outside the Harness in-session delegation scope.

## Acceptance Criteria

- The tool is still named `Subagent` and remains Harness-owned/injected.
- Managed style exposes all five actions; sync-only exposes only foreground
  start.
- Start uses `description`, `prompt`, `subagent_type`, and optional
  `run_in_background`, `mode`, and foreground `timeout_seconds`.
- Send uses `prompt` and optional `run_in_background`.
- Legacy `agent`, `message`, and `wait` fields are rejected.
- Managed start/send default to background; sync-only start defaults to
  foreground.
- `PrepareCall` is the only argument decode/validation boundary and returns a
  typed artifact.
- Execution ignores raw argument changes and fails closed without the right
  artifact.
- Every success is deterministic valid JSON.
- No raw controller error reaches the model.
- Current controller authorization, ownership, quota, restore, and lifecycle
  invariants continue to pass under `go test -race`.
- `model` and `isolation` remain unadvertised until their runtime contracts are
  designed and implemented.
