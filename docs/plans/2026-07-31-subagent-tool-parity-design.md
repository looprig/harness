# Subagent Tool Parity and Per-Loop Runtime Selection Design

**Date:** 2026-07-31

**Status:** Approved, revised for per-loop ACP/model/effort selection

## Goal

Upgrade Harness's model-facing `Subagent` tool toward Claude Code's current
`Agent` invocation model while retaining Looprig's parent-scoped delegation
security and managed-child lifecycle.

In addition to the Claude-style field vocabulary, a parent Loop may choose a
configured agent harness, model alias, and inference effort independently for
each child Loop. This permits, for example, one child to run Claude Code through
ACP with `sonnet-5`/`high` and another to run Codex through ACP with
`luna`/`max`, provided those combinations are in that parent's capability
catalog.

This is a semantic compatibility upgrade, not exact Claude wire compatibility:

- the model-facing name remains `Subagent`;
- `start`, `send`, `wait`, `interrupt`, and `status` remain one control surface;
- `start` and `send` use `description`, `prompt`, `subagent_type`, and
  `run_in_background`;
- `start` conditionally accepts `agent_harness`, `model`, and `effort` when
  those selectors are genuinely available to its parent Loop;
- preparation is the only untrusted decode and validation boundary;
- every success has a stable JSON result;
- runtime failures are mapped to bounded model-safe errors.

This design is scoped exclusively to Subagent delegation and the runtime seams
needed to launch child Loops.

## Reviewed Baseline

This revision was checked against:

- Harness main and `.worktrees/long-running-commands/harness`;
- Harness's existing per-Loop `model.Model` and `model.Effort` state;
- the session runtime's atomic model/effort change and restore paths;
- `inference/gateway` at the merged multi-model gateway change;
- `acp/launch` at the gateway-backed Claude Code, Codex, and Gemini adapter
  changes;
- `acp/client` and `acp/launch` as the protocol/session and safe process
  boundaries for a new `acp/loop` adapter;
- Harness's existing public backend-builder boundary;
- Claude Code's current subagent tool documentation.

The long-running-command branch does not change the current Subagent envelope,
but it makes the call boundary explicit: `PrepareCall` validates once and
returns a per-call `tool.PreparedArtifact`; execution must consume that artifact
through `loop.PreparedCallFromContext` and must not re-decode mutable raw JSON.

The current runtime already proves that model and effort are Loop-scoped:

- every `loop.Mode` and `loop.BoundMode` carries its own model and effort;
- every child definition is independently bound before its Loop is created;
- `LoopStarted.Runtime` records the child Loop's effective model key, context
  limits, and effort;
- model/effort changes apply atomically at a turn boundary;
- restore folds model/effort independently for each Loop.

Session workspace placement is unrelated to inference selection. The previous
design incorrectly grouped `model` with `isolation`; this revision corrects
that. Per-child worktree isolation remains deferred, while model and effort are
part of this design.

## Current Surface

Harness currently exposes one flat `Subagent` action envelope:

| Action | Required fields | Optional fields | Current default |
|---|---|---|---|
| `start` | `agent`, `message` | `mode`, `wait`, `timeout_seconds` | omitted `action` means `start`; omitted `wait` means `true` |
| `send` | `delegate_id`, `message` | `wait`, `timeout_seconds` | omitted `wait` means `true` |
| `wait` | `delegate_id`, `request_id` | `timeout_seconds` | waits |
| `interrupt` | `delegate_id` | none | immediate control result |
| `status` | none | `delegate_id` | all owned children when omitted |

`DelegationSyncOnly` advertises only synchronous `start`.
`DelegationManaged` advertises all five actions.

The controller already enforces parent-scoped delegate authorization, child
ownership, depth and spawn quotas, request correlation, follow-up, wait,
interrupt, status, and durable request reconstruction. These semantics remain.

## Gateway and ACP Ground Truth

`inference/gateway` is a loopback HTTP compatibility router. It resolves
`(ingress API format, harness-facing model alias)` to an injected
`inference.Client` and full `model.Model`. Because it decodes to Looprig's
neutral inference request and re-encodes for the target, an Anthropic-speaking
ACP harness can reach an OpenAI-, Gemini-, or Anthropic-backed target, and the
same is true for a Responses-speaking harness.

Every admitted selection therefore carries two distinct formats:

- the agent harness profile declares the **ingress format** it speaks to the
  gateway;
- the selected target `model.Model.APIFormat` declares the **target/egress
  format** its injected client speaks.

The gateway route key uses only `(ingress format, model alias)`; the resolved
target supplies the egress format. The same model alias is registered once for
each harness ingress that may use it. For example:

```text
(Anthropic,        sonnet-5) -> Sonnet target (Anthropic)
(OpenAI Responses, sonnet-5) -> Sonnet target (Anthropic)
```

The first route is same-dialect: the gateway still provides local
authentication, alias resolution, capability validation, and credential-safe
client injection, but no cross-dialect translation is needed. The second route
decodes OpenAI Responses from Codex and re-encodes Anthropic Messages for the
same Sonnet target. The product must compile both routes from one harness/model
registry; it must not infer egress format from the chosen harness.

The gateway deliberately does **not** discover ACP agents or maintain a model
catalog. The embedding product already owns that configuration. It must build a
frozen Subagent runtime catalog from the same inputs used to construct:

- gateway aliases and targets;
- available ACP adapter executables/connectors;
- native Loop inference bindings, when native is offered;
- model capability and allowed-effort policy;
- parent-to-child delegation policy.

`acp/launch` supplies the required process/proxy boundary:

- `launch.Dial` can lend one shared gateway binding to many independent ACP
  clients;
- Claude Code selects a harness-facing alias through the adapter's advertised
  `model` config option after `session/new`;
- Codex receives its alias at process launch and therefore requires a new ACP
  process/session for a different model;
- connector configuration uses only validated absolute executables, an
  allowlisted environment, and the gateway's local token.

The common rule is therefore one ACP process/session per child Loop. The
selected tuple is fixed before the child's first prompt and remains pinned for
that child. Two sibling children may use completely different tuples while
sharing the same session workspace and gateway server.

ACP and MCP are separate protocols. MCP is not part of this design.

## Optional Adapter and Tool Boundaries

ACP is an optional product adapter, not a Harness dependency. Harness must not
import ACP, name ACP protocol types, launch ACP processes, know a harness API
format, or translate a harness's tool language. Its generic runtime contract is:

```text
role + opaque agent-harness alias + opaque model alias + effort
    -> resolved native or adapter runtime profile
```

Harness carries only validated, secret-free aliases plus a generic runtime
profile key passed to its injected backend-builder boundary. A product with no
ACP module registers no ACP-backed profiles; native delegation continues to
work and no ACP code is linked or initialized. The canonical ACP integration is
`acp/loop`, which adapts `acp/client` and `acp/launch` to Harness's public
`loop.Backend` construction seam.

There are also two different tool surfaces:

- **Harness control tools** belong to a Looprig Loop. `Subagent` is injected
  into the parent Loop and executes through Harness's prepared, gated,
  parent-scoped controller path.
- **Child execution tools** belong to the selected child runtime. A native
  child receives its bound Harness tools. An ACP child uses the tools supplied
  by its agent harness, such as Claude Code's or Codex's own tools.

This iteration does not translate arbitrary Harness tools into an ACP harness
and does not claim that a role's native tool definitions automatically exist in
an ACP child. The runtime catalog admits a role/profile combination only if
the role's requirements can be honored by that profile. Portable role data in
this release is limited to system/instruction policy and explicitly mapped
access posture; native-only tool requirements make an ACP combination
incompatible.

A future optional tool-export adapter may bridge selected Harness tools to an
ACP runtime using a protocol that runtime supports. It must invoke the same
prepared/gated/audited Harness execution seam; no such bridge belongs in the
core `Subagent` or Loop packages.

## Runtime Selection Model

The child role and three runtime dimensions are independently selectable:

- `subagent_type` selects the child's role definition: system instructions,
  modes, limits, and runtime requirements; native Harness tools are available
  only when the selected profile implements the native tool surface;
- `agent_harness` selects the execution harness, such as Looprig native,
  Claude Code over ACP, or Codex over ACP;
- `model` selects a harness-facing gateway alias;
- `effort` selects Looprig's dialect-neutral effort intent.

`mode` remains a fourth, behavioral dimension. For a native runtime it selects
the role's declared instructions, tools, limits, and defaults. An ACP profile
receives only the mode data its compatibility contract explicitly
maps; this release requires instructions but does not claim to enforce native
Harness tool definitions or tool-iteration limits inside an ACP harness.
Resolution order for a new child is:

1. select `subagent_type`;
2. select `mode` or the role's initial mode;
3. select the default or requested `agent_harness`;
4. select the harness's default or requested `model` alias;
5. apply the model/harness default effort or the requested `effort`.

Start-time runtime resolution wins over mode defaults. The complete resolved
agent-harness/model/effort tuple is pinned for the child Loop's lifetime,
whether its values were explicit or defaulted. A later mode change may alter
tools, instructions, and limits, but must not silently replace that tuple.
Follow-up `send` calls use the same pinned runtime.

The initial version supports these effort values:

```text
low, medium, high, max
```

Omitting `effort` means use the catalog default; there is no model-facing empty
string or guessed vendor-specific level.

## Parent-Scoped Capability Catalog

Each parent Loop receives an immutable catalog containing only child runtime
combinations it may launch. Conceptually:

```go
type RuntimeCatalogEntry struct {
	SubagentType  identity.AgentName
	AgentHarness  AgentHarnessName
	Default       bool
	DefaultModel  ModelAlias
	Models        []RuntimeModelOption
}

type RuntimeModelOption struct {
	Alias         ModelAlias
	Target        model.Model
	DefaultEffort model.Effort
	Efforts       []model.Effort
}
```

The concrete implementation may normalize this differently, but it must
preserve these invariants:

- catalog entries are immutable, defensively copied, deterministically sorted,
  and secret-free;
- identifiers are stable aliases, never executable paths, provider secrets,
  proxy tokens, raw URLs, or arbitrary argv;
- every subagent type has exactly one deterministic default harness;
- every harness has exactly one deterministic default model;
- duplicate or ambiguous aliases fail catalog construction;
- non-empty effort lists contain only valid Looprig effort values;
- the catalog may be narrower for one parent than another;
- schema generation and controller validation use the same snapshot;
- the controller revalidates every selection even if schema validation was
  bypassed.

The catalog is not an unrestricted Cartesian product. A composition root admits
a tuple only when it is truthful and safe. At minimum it checks:

- the selected ACP adapter exists and passed its required preflight;
- the gateway has an ingress codec and route for the harness/alias pair;
- the target model supports tool use required by the role;
- the target's context and other required capabilities are compatible;
- non-default effort is supported by the target and can be expressed by the
  selected ACP harness/connector;
- the parent is authorized to launch that child role and runtime.

The model-facing schema is capability-derived rather than permanently widened:

- if a parent has no registered ACP agent-harness profiles,
  `agent_harness` is absent from its `Subagent` schema and description, and an
  explicit field is rejected during preparation;
- if ACP profiles are available, `agent_harness` is present only in the
  affected start branches and enumerates only the aliases allowed to that
  parent/role;
- `model` is advertised only when the resolved native or ACP runtime offers
  a model choice beyond its implicit default;
- `effort` is advertised only when at least one admitted model offers a choice
  beyond its default;
- a mixed catalog may expose `native` alongside configured ACP harness
  aliases, while omission continues to select the deterministic default.

There is no process-global `acp_enabled` flag. Registering an optional ACP
profile changes only the catalog/schema branches that can actually use it. A
plain Harness build with no ACP composition retains the native Subagent surface
and behavior.

For a partial model-facing selection, omission never triggers a global search:

- omitted `agent_harness` uses that child role's default harness;
- omitted `model` uses the selected harness's default alias;
- omitted `effort` uses the selected model/harness default;
- an explicit value incompatible with those resolved defaults is rejected;
- the controller never silently falls back to a different harness, model, or
  effort.

## Proposed Model-Facing API

The envelope remains flat and `additionalProperties` remains `false`. `action`
may be omitted only for `start`.

### Start

```json
{
  "description": "Review restore behavior",
  "prompt": "Inspect the restore path and identify correctness risks.",
  "subagent_type": "reviewer",
  "mode": "review",
  "agent_harness": "claude-code",
  "model": "sonnet-5",
  "effort": "high",
  "run_in_background": true
}
```

A sibling may independently start as:

```json
{
  "description": "Implement parser fix",
  "prompt": "Implement the approved parser change and run focused tests.",
  "subagent_type": "worker",
  "agent_harness": "codex",
  "model": "luna",
  "effort": "max",
  "run_in_background": true
}
```

`sonnet-5` and `luna` in these examples are product-configured gateway aliases,
not names hard-coded by Harness.

| Field | Required | Meaning |
|---|---:|---|
| `action` | no | `start`; omission also means `start`. |
| `description` | yes | Short, non-blank call label. |
| `prompt` | yes | Initial child user turn. |
| `subagent_type` | yes | One child role in the parent's frozen allowlist. |
| `mode` | no | One mode declared by that role; omission uses its initial mode. |
| `agent_harness` | no | Capability-derived. Present only when that role has a selectable ACP/native harness choice; omission uses its default. |
| `model` | no | Capability-derived. One gateway/native alias available under the resolved harness; omission uses its default. |
| `effort` | no | Capability-derived. One allowed neutral effort for the resolved harness/model; omission uses its default. |
| `run_in_background` | no | Managed defaults to `true`; sync-only defaults to and requires `false`. |
| `timeout_seconds` | no | Foreground wait bound; forbidden for background calls. |

The schema derives subagent, mode, and whichever runtime selector properties
are genuinely available from the same frozen catalog. The description renders a bounded
`<available_subagents>` matrix so the model can see compatible combinations.
The schema guides the model; preparation and the scoped controller remain the
security boundaries.

### Send

```json
{
  "action": "send",
  "delegate_id": "55555555-5555-4555-8555-555555555555",
  "prompt": "Now inspect the event fold.",
  "run_in_background": false,
  "timeout_seconds": 60
}
```

`delegate_id` and `prompt` are required. `agent_harness`, `model`, `effort`,
`mode`, and `subagent_type` are forbidden: a follow-up cannot mutate child
identity or inference runtime.

### Wait, Interrupt, and Status

The existing shapes remain, with strict action-specific fields:

```json
{
  "action": "wait",
  "delegate_id": "55555555-5555-4555-8555-555555555555",
  "request_id": "66666666-6666-4666-8666-666666666666",
  "timeout_seconds": 60
}
```

```json
{"action":"interrupt","delegate_id":"55555555-5555-4555-8555-555555555555"}
```

```json
{"action":"status"}
```

Interrupt stops the current turn, not the registered child. Status returns
bounded mechanical state and pending request counts, never transcript content,
runtime secrets, executable paths, or gateway bindings.

## Hard-Cut Migration

The model-facing rename remains a hard cut:

| Removed | Replacement |
|---|---|
| `agent` | `subagent_type` |
| `message` | `prompt` |
| `wait: true` | `run_in_background: false` |
| `wait: false` | `run_in_background: true` |

No aliases or dual schema are retained. `agent_harness`, `model`, and `effort`
are new start-only fields.

The internal `tool.DelegateRequest` may retain controller-domain names but gains
an optional runtime selection preserving field presence. The prepared adapter
performs the translation once.

## Boundary Validation and Limits

The Subagent tool keeps fixed byte/time limits:

```go
const (
	maxSubagentArgsBytes = 256 << 10
	maxDescriptionBytes  = 256
	maxPromptBytes       = 192 << 10
	maxTimeoutSeconds    = 24 * 60 * 60
)
```

Preparation:

1. rejects oversized input before decoding;
2. requires exactly one JSON object;
3. rejects unknown and trailing fields;
4. applies action-specific required/allowed fields;
5. validates bounds and canonical UUIDs;
6. resolves defaults and validates the complete runtime tuple against the
   frozen catalog;
7. translates to one typed `tool.DelegateRequest` artifact.

Preparation errors use fixed, bounded categories and never include prompts,
descriptions, identifiers, raw JSON, catalog internals, paths, URLs, tokens, or
decoder messages.

Execution retrieves the prepared artifact and adds only the trusted
`ParentToolUseID` from context. A missing or wrong artifact fails closed. It
never re-decodes `argsJSON`.

## Runtime Construction

Harness needs a narrow composition seam that resolves an authorized selection
to a child runtime before the first prompt. The resolved value contains:

- the selected role definition and mode;
- an engine kind (`native` or generic `adapter`);
- a stable secret-free agent-harness/profile alias;
- the full selected target `model.Model` and effort;
- for native, the already-configured `inference.Client`;
- for adapter execution, only an opaque profile key consumed by the injected
  backend builder.

Harness must not import ACP or the gateway. It stores and validates aliases and
calls injected resolver/builder seams. The product composition root owns real
clients, gateway routes, ACP connector factories, executable paths, and
environment policy.

The existing backend-builder pair can remain if it becomes a router over the
resolved profile carried by `loop.BoundDefinition`. A clearer alternative is a
profile-keyed builder registry. In either shape, routing on `Engine` alone is
insufficient because several distinct ACP harness profiles can share one
generic adapter engine. Routing on display name is also forbidden. Use an
explicit stable profile alias.

`acp/loop` owns the concrete ACP-to-Harness adapter. It may import
`acp/client`, `acp/launch`, and Harness public packages to implement
`loop.Backend` plus the existing live/restore builder contracts. Pure ACP wire,
client, transport, and launch packages remain Harness-independent. Harness
does not import `acp/loop`; the product composition root injects its builders.
Existing direct-CLI compatibility integrations are outside this design and are
not part of the canonical ACP path.

For every ACP child:

1. resolve the selected profile and gateway alias;
2. create a fresh ACP connector/process/session for that child;
3. borrow the session-owned gateway binding;
4. apply model and effort before the first prompt;
5. verify advertised adapter capabilities and fail closed on mismatch;
6. normalize ACP updates into Harness events;
7. close the ACP client/process when the child Loop closes or construction
   fails.

Claude Code may apply model/effort with advertised config options after
`session/new`. Codex applies them at process launch. Other ACP adapters must
declare and implement an equally explicit mechanism. If an adapter cannot
express a requested effort, that tuple must not be advertised.

## Durability and Restore

`LoopStarted.Runtime` already persists target model identity, limits, and effort,
but ACP reconstruction additionally needs the harness/profile and the
harness-facing gateway alias. Add a secret-free durable runtime identity, for
example:

```go
type AgentRuntime struct {
	Harness    string `json:"harness"`
	ModelAlias string `json:"model_alias"`
}
```

The exact event shape may be additive fields on `LoopStarted` or a nested value,
but restore must not guess. Native legacy Loops may decode to the existing
definition defaults; a non-native Loop missing required ACP runtime identity
fails closed.

Restore:

- folds the child Loop's persisted harness/profile, alias, model key, limits,
  and effort;
- resolves the profile and alias through the current immutable session catalog;
- verifies the resolved target model key matches the journaled key;
- reconstructs a fresh ACP process/session and resumes/loads only when the ACP
  adapter and backend provide a proven restore path;
- otherwise reports a typed restore incompatibility instead of silently
  starting a different harness/model.

The session configuration fingerprint/manifest includes a digest of the
secret-free delegate runtime catalog. Executable paths, gateway URLs/tokens,
credentials, and raw model descriptors do not enter events or fingerprints.

## Result and Error Contracts

Success remains JSON text with one stable shape per action. A start result adds
only the resolved secret-free runtime selectors that were advertised to that
parent so callers can verify what was created. A plain native catalog with no
runtime choice omits `runtime`; a native catalog with only model/effort choices
omits `agent_harness`; an ACP-enabled branch includes the resolved harness:

```json
{
  "action": "start",
  "delegate_id": "55555555-5555-4555-8555-555555555555",
  "request_id": "66666666-6666-4666-8666-666666666666",
  "status": "queued",
  "runtime": {
    "agent_harness": "claude-code",
    "model": "sonnet-5",
    "effort": "high"
  }
}
```

Structured encoding uses `encoding/json`, never string concatenation. Child
output remains model-visible by design; mechanical results remain bounded.

Add stable failure classifications for unknown or incompatible runtime
selection, unavailable ACP profile, adapter capability mismatch, and restore
runtime mismatch. Raw controller, ACP, gateway, subprocess, or provider errors
are retained only as trusted local causes/logs. They never reach a model-facing
tool result.

## Ownership and Security Invariants

- The schema is guidance, not authorization.
- Every controller is bound to exactly one parent Loop.
- A parent may start only declared child roles and cataloged runtime tuples.
- The model cannot provide executable paths, environment entries, gateway URLs,
  tokens, provider names, API keys, argv, or arbitrary ACP config identifiers.
- Runtime selection cannot raise the child's access, tool, depth, or spawn
  ceilings.
- Each child gets independent Loop state and, for ACP, an independent process
  and ACP session.
- Sharing the session workspace or gateway does not share model/effort state.
- Follow-up, wait, status, and interrupt remain owner-checked.
- Catalog ambiguity and runtime drift fail closed.

## Testing Strategy

Coverage must include:

1. schema/preparation tests for the hard-cut fields, optional runtime fields,
   compatible matrix, defaults, strict rejection, limits, and typed artifacts;
2. controller tests for parent-specific catalogs, default resolution, invalid
   tuples, bypassed schema, quotas, ownership, and sanitized failures;
3. Loop binding tests proving two sibling children can hold different
   harness/model/effort tuples without cross-talk;
4. gateway/ACP integration tests proving Claude-Code/alias-A/high and
   Codex/alias-B/max can run concurrently through one shared gateway;
5. adapter tests proving model and effort are applied before the first prompt
   and unsupported options fail closed;
6. restore tests for exact profile/alias/model/effort recovery and drift
   rejection;
7. race tests for concurrent starts, waits, interrupts, and independent ACP
   teardown;
8. long-running-command compatibility tests proving prepared artifacts remain
   the sole execution boundary.

## Deferred Follow-Up: Per-Child Worktree Isolation

`isolation` remains unadvertised. All child Loops currently operate over one
session workspace/coordinator with separate Loop observation sets. A later
design may add `isolation: "worktree"` only after defining placement,
checkpoint, lease, process CWD, cleanup, and restore semantics. This does not
block per-Loop runtime selection.

## Deferred Follow-Up: ACP Primary Loops

This iteration applies ACP runtime selection to child Loops launched through
`Subagent`. The primary/root Loop remains on its existing native composition.

Making the primary Loop an ACP harness is compatible with the runtime registry
in principle, but it introduces a separate tool-exposure problem: an ACP agent
harness does not automatically receive Harness's bound `Subagent` tool. The
standard ACP session surface accepts MCP server descriptors for external tools,
so a complete design would need a loop-scoped, prepared/gated/audited bridge for
exporting selected Harness control tools to the ACP primary. That bridge is not
added implicitly here and no Harness tool is invoked outside its existing
prepared-call boundary.

## Acceptance Criteria

- `Subagent` retains the five-action managed lifecycle and sync-only subset.
- Start requires `description`, `prompt`, and `subagent_type`.
- `agent_harness`, `model`, and `effort` are capability-derived, start-only,
  and resolved deterministically from a parent-scoped frozen catalog; selectors
  with no genuine choice are absent and rejected.
- A session can concurrently run sibling ACP children using different agent
  harnesses, model aliases, and efforts through one inference gateway.
- The complete resolved runtime tuple stays pinned for each child Loop.
- Preparation is the only JSON decode/validation boundary.
- Controller authorization and runtime compatibility are re-enforced beneath
  the schema.
- Runtime identity is durable and restore never guesses or silently falls back.
- No secret, path, binding, raw ACP/gateway/provider error, or untrusted field
  reaches model-visible results or durable events.
- Workspace isolation behavior remains unchanged.
- Primary/root Loops remain native until the ACP control-tool exposure contract
  is designed separately.
