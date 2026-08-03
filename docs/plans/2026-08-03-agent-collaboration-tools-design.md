# Agent Collaboration Tools Design

**Date:** 2026-08-03

**Status:** Approved

**Supersedes:** The model-facing `Subagent` action-envelope portions of
`2026-07-31-subagent-tool-parity-design.md`. The earlier document's runtime
catalogue, gateway, ACP, durability, and parent-scoped security design remains
in force except where this document explicitly changes it.

## Goal

Replace the single model-facing `Subagent` action envelope with four tools that
present child agents as persistent collaborators:

- `StartAgent`
- `MessageAgent`
- `ListAgents`
- `StopAgent`

The vocabulary must remain distinct from Harness's todo tools (`TaskCreate`,
`TaskUpdate`, `TaskGet`, and `TaskList`). A task is a shared work record. An
agent is a persistent participant that may receive many messages and produce
many responses during one session.

Every configured child runtime, including an ordinary in-process Harness Loop,
may select an admitted model and effort independently at `StartAgent` time.
Model and effort selection is not an ACP feature.

## Non-goals

- This design does not introduce a global team or peer-to-peer mailbox.
- It does not let a child discover or control its parent or siblings.
- It does not make task-list records represent agent lifecycle.
- It does not expose credentials, provider IDs, endpoints, executable paths,
  gateway aliases, or connector configuration to the model.
- It does not change an existing agent's model or effort through
  `MessageAgent`; a different runtime selection requires a new agent.
- It does not retain the old `Subagent` tool as an alias or compatibility
  envelope.

## Naming and Semantics

The tool names are verb-first and describe an interaction with a persistent
agent:

| Tool | Meaning |
|---|---|
| `StartAgent` | Start a new agent and send its initial instructions. |
| `MessageAgent` | Send a message to an existing agent and receive or await its response. |
| `ListAgents` | List agents directly started by the calling Loop. |
| `StopAgent` | Stop an agent's current response without deleting the agent. |

`StopAgent` means "stop what you are doing." It interrupts the current response
and cancels messages queued for that response cycle. The agent remains owned,
listed, and available for a later `MessageAgent` call. It is not a close,
delete, unregister, or recursive shutdown operation.

## Tool Injection and Ownership

Agent tools are derived from the calling Loop's frozen definition and bound to
a controller scoped to that Loop. A Loop with no admitted child agent types
receives no agent tools. A Loop with admitted children receives the tools
allowed by its delegation style.

An agent tool may address only agents whose direct parent is the calling Loop.
For this hierarchy:

```text
Parent P
├── Agent A
│   └── Agent C
└── Agent B
```

- `ListAgents` called by P returns A and B;
- `ListAgents` called by A returns C;
- `ListAgents` called by B or C returns an empty list;
- A cannot list, message, or stop P or B;
- P cannot directly list, message, or stop C.

Results flow from a child to its direct parent through the response-delivery
rules below. A child does not need authority to address its parent for that
automatic hand-back.

## Common Response Policy

`StartAgent` and `MessageAgent` share two optional control fields:

- `wait_for_response` is optional and defaults to `true`;
- `timeout_seconds` is optional and has no elapsed-time default.

Omitting `timeout_seconds` means the response has no timer deadline. Parent
cancellation, `StopAgent`, session shutdown, and runtime failure can still end
it. When supplied, the timeout bounds the specific response in foreground or
background mode; a background timeout produces the same durable completion
delivery as any other terminal outcome.

Every admitted response has exactly one delivery path:

| `wait_for_response` | Admission result | Completion delivery |
|---|---|---|
| omitted or `true` | The tool call blocks. | The response is returned exactly once as that tool call's result. |
| `false` | The tool returns the agent identity immediately. | The response is delivered exactly once as a durable machine-originated input to the parent. |

A response must never be delivered through both paths. Background completion
does not require or permit a `WaitAgent` tool.

## `StartAgent`

### Input

```json
{
  "agent_type": "planner",
  "name": "api_planner",
  "instructions": "Inspect the authentication flow and propose an implementation plan.",
  "wait_for_response": false,
  "timeout_seconds": 900,
  "agent_harness": "codex",
  "agent_source": "gateway",
  "model": "gpt-5.6-sol",
  "effort": "high",
  "agent_mode": "plan"
}
```

| Field | Presence | Meaning |
|---|---|---|
| `agent_type` | Required | Parent-admitted role or specialization. |
| `instructions` | Required | Initial instructions given to the new agent. |
| `name` | Optional | Bounded friendly instance name. Harness generates a unique name when omitted. |
| `wait_for_response` | Optional | Defaults to `true`; selects foreground or background delivery. |
| `timeout_seconds` | Optional | No default; bounds this response when present. |
| `model` | Optional, always advertised | Model alias admitted for the selected runtime. Omission selects its default. |
| `effort` | Optional, always advertised | Effort admitted for the selected model. Omission selects that model's default. |
| `agent_harness` | Conditional | Stable execution-harness alias; advertised when a role has multiple harness choices. |
| `agent_source` | Conditional | Stable source alias such as `gateway` or `native`; advertised when the harness has multiple sources. |
| `agent_mode` | Conditional | Initial Loop mode; advertised when the agent type has multiple modes. |

`model` and `effort` are schema properties for every `StartAgent` tool,
irrespective of whether ACP support is compiled or enabled. Schema branches
constrain their values to the selected agent type, harness, and source. A
harness-managed runtime with no product-known model catalogue requires these
fields to be omitted, but their absence from that branch must not hide model
selection from ordinary native Loops or other explicit runtime branches.

The selected runtime tuple is immutable for the agent's lifetime. It is
recorded durably on `LoopStarted` and restored without re-resolving mutable
external configuration.

### Foreground Result

```json
{
  "agent_id": "55555555-5555-4555-8555-555555555555",
  "name": "api_planner",
  "state": "idle",
  "response": "Here is the proposed implementation plan..."
}
```

### Background Admission Result

```json
{
  "agent_id": "55555555-5555-4555-8555-555555555555",
  "name": "api_planner",
  "state": "working"
}
```

The internal response/command ID remains available for durable correlation but
is not a required model-facing input because there is no follow-up wait
operation.

## `MessageAgent`

### Input

```json
{
  "agent_id": "55555555-5555-4555-8555-555555555555",
  "message": "Now examine how restoration changes that proposal.",
  "wait_for_response": false,
  "timeout_seconds": 600
}
```

| Field | Presence | Meaning |
|---|---|---|
| `agent_id` | Required | Stable identity returned by `StartAgent`. |
| `message` | Required | Direction, context, or question sent to the agent. |
| `wait_for_response` | Optional | Defaults to `true`; selects foreground or background delivery. |
| `timeout_seconds` | Optional | No default; bounds this response when present. |

If the agent is idle, the message starts a response immediately. If it is
working, the message is admitted in order behind already accepted messages.
If its previous response was stopped, a new message starts it again. Unknown,
unowned, unavailable, or closed agents fail without revealing whether another
parent owns them.

The result shapes match `StartAgent`: foreground returns the response with the
agent identity and final state; background returns the identity and `working`,
then uses durable parent delivery.

## `ListAgents`

### Input

List every directly owned agent:

```json
{}
```

Filter to one directly owned agent:

```json
{"agent_id":"55555555-5555-4555-8555-555555555555"}
```

`agent_id` is optional. Supplying an unknown or unowned ID returns the same
bounded not-owned error. The unfiltered result is deterministically ordered and
bounded.

### Result

```json
{
  "agents": [
    {
      "agent_id": "55555555-5555-4555-8555-555555555555",
      "name": "api_planner",
      "agent_type": "planner",
      "state": "working",
      "queued_messages": 1,
      "agent_harness": "codex",
      "agent_source": "gateway",
      "model": "gpt-5.6-sol",
      "effort": "high",
      "agent_mode": "plan"
    }
  ],
  "truncated": false
}
```

Agent state uses only mechanical lifecycle terms:

- `starting`
- `working`
- `idle`
- `unavailable`

An agent is not `completed`; one of its responses completes and the persistent
agent returns to `idle`. `ListAgents` is observability only and never retrieves
response content.

## `StopAgent`

### Input

```json
{"agent_id":"55555555-5555-4555-8555-555555555555"}
```

### Result

```json
{
  "agent_id": "55555555-5555-4555-8555-555555555555",
  "previous_state": "working",
  "state": "idle"
}
```

The result deliberately has no `stopped` boolean. `previous_state` tells the
caller whether work was active, and `state` reports the resulting reusable
state. Calling `StopAgent` on an idle agent is an idempotent success with both
states equal to `idle`.

Stopping an agent terminates its active response and cancels its queued
messages. Each affected foreground caller or background parent delivery
receives exactly one interrupted terminal outcome. The agent's Loop, identity,
runtime selection, and ownership registration remain intact.

## Session Activity and Durable Background Hand-back

A running child Loop contributes ordinary Loop activity, so the session cannot
be idle while that response is running. A persistent but idle child does not by
itself keep the session active.

Background admission must also acquire a quiescence wake token before the child
can finish. That token bridges the gap between child completion and admission
of the completion input to the parent:

```text
background response admitted
    -> child runs
    -> child terminal is durably recorded
    -> completion input is durably admitted to parent
    -> parent starts, queues, folds, or cancels that input
    -> wake token is released on the corresponding enduring parent event
```

There must be no `SessionIdle` edge between the child terminal and the parent
hand-back. Harness's existing `ExpectTurn`/`SubagentResult` mechanism supplies
this invariant, but asynchronous agent admission must take the wake token in
production rather than leaving it as an unused hand-back path.

The completion input includes bounded structured metadata (`agent_id`, name,
terminal state, and internal correlation) plus the bounded response text. It is
machine-originated, never rejected, and starts the parent when idle or queues
when the parent is working. Restore reconstructs unresolved admitted responses
from durable events and either completes their hand-back exactly once or
records a bounded terminal failure.

## Runtime Selection Is General Harness Behavior

Model and effort freedom applies to every explicit child runtime:

- an in-process Harness Loop binds the selected injected inference client,
  `model.Model`, and effort directly;
- an ACP gateway-backed child resolves the same model-facing alias and effort
  to its concrete strict gateway route;
- an ACP native-auth child uses an explicitly catalogued native model when its
  profile supports product-managed selection;
- a harness-managed ACP profile may omit model identity only when the product
  genuinely cannot enumerate or set it.

CodeRig compiles all configured `uses: ["delegate"]` models into the eligible
ordinary-native and adapter-backed runtime branches permitted for each agent
type. ACP being absent disables only ACP branches; it must not remove
`model`/`effort` selection from ordinary children.

Harness remains product-neutral. It does not read `models.json`, discover ACP
connectors, inspect provider credentials, or decide which models a role may
use. It consumes a frozen, secret-free, parent-scoped runtime catalogue
injected by the product composition root.

## Model Descriptions in `models.json`

Each model row admitted for `delegate` use gains a non-empty `description`
field explaining when an agent should choose it. Example:

```json
{
  "alias": "gpt-5.6-sol",
  "description": "Use for difficult planning, implementation, and review that benefits from strong long-horizon reasoning; prefer a smaller model for narrow lookups.",
  "provider": "openai",
  "api_format": "openai-responses",
  "base_url": "",
  "model": "gpt-5.6-sol",
  "api_key": "REDACTED-EXAMPLE",
  "uses": ["primer", "delegate"],
  "capabilities": {"tools": true, "thinking": true},
  "efforts": ["low", "medium", "high", "max"],
  "default_effort": "high"
}
```

Rules:

- `description` is required and non-blank for every `delegate` model;
- it is valid UTF-8, single-line after normalization, and bounded to 256 bytes;
- it describes selection guidance, not provider wiring;
- it must contain no credentials, endpoint URLs, executable paths, account
  names, or other secrets;
- it is carried into the secret-free runtime model option, defensively copied,
  included in the runtime catalogue digest, and therefore included in the
  configuration fingerprint;
- descriptions from models not admitted to the caller's parent-scoped
  catalogue are never rendered.

Because making descriptions required changes validation of external
configuration, the product model-file schema increments to version 2 rather
than silently changing the meaning of version 1. A version-1-to-version-2
migration adds a description to each delegate-eligible model. CodeRig
continues to fail closed on unknown versions and never rewrites the file
itself.

## Runtime Capability Block

The parent chooses a runtime from a bounded capability block generated from
the same frozen parent-scoped runtime catalogue used to build and validate the
`StartAgent` schema. The block belongs in the `StartAgent` tool description,
not in the system prompt.

Reasons:

- availability varies by parent Loop, role policy, connector preflight,
  configured source, and admitted model;
- the tool exists only where delegation is authorized;
- one catalogue snapshot can drive schema branches, descriptions, preparation,
  controller revalidation, and the rendered block;
- a system-prompt copy would duplicate tokens, churn prompt caches, and risk
  drifting from the actual schema;
- runtime configuration is capability metadata, not permanent agent identity
  or behavioral policy.

The stable system prompt may explain when delegation is useful and instruct the
model to consult `StartAgent` for current choices. It must not enumerate
harnesses, models, efforts, defaults, or availability.

The tool description renders two bounded sections. For example:

```text
<available_agents>
- planner: Investigates and designs without modifying the workspace.
- reviewer: Reviews changes and reports concrete findings.
</available_agents>

<available_agent_runtimes>
- agent_type=planner default: harness=looprig source=native model=gpt-5.6-sol effort=high
  - harness=looprig source=native model=gpt-5.6-sol efforts=[low,medium,high,max]: Use for difficult planning and long-horizon reasoning.
  - harness=codex source=gateway model=gpt-5.6-sol efforts=[low,medium,high,max]: Use for difficult planning and long-horizon reasoning.
  - harness=claude-code source=gateway model=sonnet-5 efforts=[medium,high]: Use for broad codebase analysis and implementation with strong tool use.
</available_agent_runtimes>
```

Rendering rules:

- list only agent types directly admitted for this parent;
- list only harness profiles that passed construction/preflight and are usable;
- list a gateway model under an ACP harness only when the gateway compiler
  created the strict route for that harness's ingress format;
- list ordinary native models even when no ACP module is present;
- mark exactly one default runtime tuple per agent type;
- render each model description once per distinct visible runtime row without
  provider IDs or concrete target aliases;
- sort deterministically by agent type, default-first harness/source, model,
  and effort order;
- compact efforts into one list rather than expanding their Cartesian product;
- enforce per-description, per-row, row-count, and total-byte limits, with an
  explicit deterministic elision marker;
- treat the schema as authoritative if a rendered description and schema could
  ever disagree.

Harness descriptions (for example, what `codex`, `claude-code`, or an
in-process `looprig` harness means) are owned by the product's runtime-profile
registry, not by `models.json`. The frozen catalogue carries only the bounded,
secret-free presentation text needed by `StartAgent`.

## Validation and Security Boundary

The JSON schema guides the model but is not an authorization boundary.
`PrepareCall` strictly decodes one tool-specific payload, rejects unknown or
cross-tool fields, resolves `StartAgent` selectors against the frozen
parent-scoped catalogue, and produces a typed prepared artifact. Execution
consumes only that artifact plus trusted runner context.

The parent-scoped controller independently revalidates ownership, delegation
style, agent type, mode, runtime tuple, quota, depth, and session state. Errors
are bounded categories and never reveal whether another parent owns an ID.

Existing input bounds continue to apply, with names, descriptions, runtime
blocks, results, roster sizes, queued-message counts, and model-facing error
strings gaining explicit limits. No untrusted raw JSON is decoded again during
execution.

## Hard Replacement

The following model-facing fields and operations are removed:

- `Subagent`
- `action`
- `subagent_type`
- `description`
- `prompt`
- `run_in_background`
- model-supplied `request_id`
- `wait`
- `status`
- `interrupt`

The conceptual mapping is:

| Removed envelope operation | Replacement |
|---|---|
| `start` | `StartAgent` |
| `send` | `MessageAgent` |
| `wait` | Automatic foreground result or durable background hand-back |
| `status` | `ListAgents` |
| `interrupt` | `StopAgent` |

There is no alias, dual registration, action discriminator, or compatibility
shim. Internal controller operations and durable identifiers may retain neutral
names where they do not leak into the model-facing contract, but comments and
types must no longer imply that a wait collector is part of the public design.

## Required Verification

Implementation verification must cover at least:

- exactly four tools are injected for an authorized managed parent and none for
  a parent with no admitted children;
- each tool has a strict operation-specific schema with no `action` field;
- `wait_for_response` omission behaves exactly as `true`;
- `timeout_seconds` omission installs no timer deadline;
- foreground responses appear only as tool results;
- background responses appear only as durable parent inputs;
- background hand-back holds session activity across the child-to-parent gap;
- restore cannot lose or duplicate an admitted background response;
- `StopAgent` has no `stopped` result field, is idempotent for idle agents,
  cancels active/queued responses, and leaves the agent reusable;
- `ListAgents` exposes direct children only at every tree depth;
- ordinary native children can select every role-admitted model and effort when
  ACP is absent;
- ACP harness/source/model rows appear only for usable compiled profiles and
  strict gateway routes;
- model descriptions are validated, bounded, secret-free by contract,
  propagated into the runtime digest, and rendered deterministically;
- the `StartAgent` runtime block and schema are derived from one catalogue
  snapshot and never advertise incompatible combinations;
- fingerprints change when the visible agent/runtime/model-description
  capability changes;
- old `Subagent` names, schemas, preparation paths, tests, documentation, and
  fingerprint literals are removed.
