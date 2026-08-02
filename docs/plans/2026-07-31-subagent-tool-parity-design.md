# Subagent Tool Parity and Per-Loop Runtime Selection Design

**Date:** 2026-07-31

**Status:** Approved; revised 2026-08-01 — foreignloops ACP driver placement,
gateway-side effort binding, hard replacement of the old tool

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
- `start` uses `description`, `prompt`, `subagent_type`, and
  `run_in_background`; `send` uses `delegate_id`, `prompt`, and
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
  boundaries consumed by a new foreignloops ACP driver;
- `foreignloops` as the existing foreign `loop.Backend` implementation and
  driver seam the ACP driver plugs into;
- Harness's existing public foreign-backend builder boundary
  (`foreign.Builder`/`foreign.RestoredBuilder`);
- Claude Code's current subagent tool documentation.

The prepared-call boundary is already on Harness main: `PrepareCall` validates
once and returns a per-call `tool.PreparedArtifact`; execution must consume
that artifact through `loop.PreparedCallFromContext` and must not re-decode
mutable raw JSON. The current Subagent does not yet use it — it returns a nil
artifact and validates during execution — which this design changes.

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

**Effort binds gateway-side.** `model.Sampling.Effort` is part of the gateway
target's `model.Model`, so an admitted `(model, effort)` pair resolves to a
concrete registered gateway target — for example a distinct route carrying
`sonnet-5` at `high` — never to an ACP session option. The catalog owns the
mapping from the model-facing `(model, effort)` selectors to the concrete
harness-facing gateway alias; the ACP harness only ever sees that concrete
alias. No connector expresses effort today and none needs to. ACP-native
effort mechanisms (the protocol's thought-level config category, Codex's
reasoning-effort launch config) are a possible later refinement outside this
design. The model-facing `model` and `effort` fields and the concrete gateway
alias are therefore distinct namespaces and must never be conflated.

Gateway-side binding requires one named gateway change: today both ingress
codecs always build a per-request sampling override from whatever the foreign
harness sends, encoding gives that override wholesale precedence over
`Model.Sampling`, and the handler only swaps the request's model — so a
per-effort target's registered effort is currently dead configuration. The
gateway pipeline gains a target-authoritative effort step: when the resolved
target declares an effort, it replaces any ingress-supplied effort in the
per-request override (other sampling fields keep their normal precedence).
Without this step the design's effort semantics do not exist.

Per-effort aliases follow one canonical derivation: the harness-facing alias
for a non-default effort is `<model alias>@<effort>` (for example
`sonnet-5@high`), while the default effort uses the bare alias. The registry
derives these aliases itself; a configured alias colliding with a derived one
fails catalog construction. A deterministic scheme also keeps the catalog
digest stable across sessions.

This release keeps one shared gateway token for all children, with two
consequences that must be stated, not hidden:

- **Strict resolution is mandatory.** The gateway's default/fallback route
  resolution is not acceptable for child traffic: a request naming an alias
  that is not registered for its ingress must fail with an error, never
  silently resolve to a format-default or global-default target. The routes
  registered for Subagent children are exactly the cataloged aliases; silent
  defaulting would let a mistyped or fabricated model name run on an
  unintended target.
- **Pinning is config-time, not wire-time.** With a single shared token, any
  child can name any alias registered for its ingress format; the pinned
  tuple is enforced at ACP configuration time and by strict-resolution
  failure, but a foreign harness that deliberately names another registered
  alias will reach it. Per-child tokens carrying per-child route
  authorization — which would also make per-child token-usage attribution at
  the gateway possible — are a named follow-up, not part of this design.
  Until then, per-child usage attribution at the gateway is not claimed.

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
- connector configuration uses only validated absolute executables, a
  caller-supplied explicit child environment (the launch package inherits
  nothing ambient and rejects — never silently strips — forbidden
  security-sensitive variables), and the gateway's local token.

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
work and no ACP code is linked or initialized. The canonical ACP integration
is a new driver in the `foreignloops` module (`foreignloops/driver/acp`),
which adapts `acp/client` and `acp/launch` to the existing
`foreignloops/backend` implementation of Harness's public `loop.Backend`
seam. The `acp` packages the driver consumes stay harness/core/inference-free:
the enforced boundary is package-level — `acp/protocol`, `acp/transport`,
`acp/client`, and `acp/launch` must not import harness, core, or inference
(the editor-facing `acp/agent` package is the sanctioned exception and is not
consumed here) — which forbids a `loop.Backend` implementation among those
packages and is why the adapter lives in `foreignloops` beside the existing
CLI drivers.

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

The model-facing catalogue supports this closed effort vocabulary, exactly
Looprig's neutral `model.Effort` values:

```text
none, low, medium, high, max
```

`none` maps to Looprig's internal zero-value `model.EffortNone`; the model never
passes an empty string. Omitting `effort` means use the catalog default and is
therefore distinct from explicitly requesting `none`. `xhigh` and `ultra` are
deliberately unsupported: they are not valid `model.Effort` values, and
extending the neutral vocabulary is a cross-repo `inference/model` + codec
change outside this design. A catalogue entry advertises only the efforts the
target model can honor without clamping or loss; enforcement is the gateway's
target-authoritative effort step, and adapter-side effort expression is never
required.

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
- non-empty effort lists contain only valid Looprig effort values, serialize
  `model.EffortNone` as `none`, and never include `xhigh` or `ultra`;
- the catalog may be narrower for one parent than another;
- schema generation and controller validation use the same snapshot;
- the controller revalidates every selection even if schema validation was
  bypassed.

Every entry also carries a **credential mode**, because it determines which
combinations are physically reachable:

- `gateway-backed` — the product's own API credential serves the traffic
  through the gateway. Any provider the inference module has a client for
  qualifies (Anthropic, OpenAI, OpenRouter, Bedrock, Phala, a local server,
  …). Because the gateway translates dialects, these targets are reachable
  from any harness ingress, and all gateway guarantees apply.
- `native-auth` — the agent harness runs on its own subscription/login and
  talks to its vendor directly. No gateway is bound for that child; the
  selectable models are exactly that harness's own catalog; effort is
  advertised only where the connector expresses it.

The cross-harness matrix therefore exists only over gateway-backed entries.
A native-auth entry is single-harness by construction. Reaching one vendor's
subscription models from a different harness is not possible without
replaying that subscription credential or impersonating a first-party client
— both of which are out of scope and must never be implemented: they breach
the providers' terms, break on every upstream client release, and endanger
the user's account. A combination requiring either is simply not admitted.

The catalog is not an unrestricted Cartesian product. A composition root admits
a tuple only when it is truthful and safe. At minimum it checks:

- the selected ACP adapter exists and passed its required preflight;
- for a gateway-backed entry, the product holds a real credential/client for
  that target and the gateway has an ingress codec and route for the
  harness/alias pair; for a native-auth entry, the harness has a usable
  login and the alias belongs to that harness's own catalog;
- the target model supports tool use required by the role;
- the target's context and other required capabilities are compatible;
- non-default effort is supported by the target and a gateway target embodying
  that effort is registered for the selected harness ingress;
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
The matrix has a fixed row budget: each role always shows its default
harness/model/effort row; past the budget, non-default combinations are
elided with an explicit marker telling the model the advertised matrix is
narrower than the actual catalog. Preparation still validates against the
full catalog, so an elided combination remains selectable.
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

`delegate_id` and `prompt` are required. `description`, `agent_harness`,
`model`, `effort`, `mode`, and `subagent_type` are forbidden: a follow-up
cannot relabel the child or mutate its identity or inference runtime.

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

## Hard Replacement

The existing Subagent implementation is removed, not migrated: the new tool is
written fresh against this design and the old envelope ceases to exist. No
aliases, dual schema, or compatibility shims are retained. For orientation
only, the old vocabulary maps as:

| Removed | Replacement |
|---|---|
| `agent` | `subagent_type` |
| `message` | `prompt` |
| `wait: true` | `run_in_background: false` |
| `wait: false` | `run_in_background: true` |

Note the default flip hiding in that table: today an omitted `wait` means
foreground; under managed delegation an omitted `run_in_background` means
background. This is a deliberate behavioral change matching Claude Code, not
just a rename. `agent_harness`, `model`, and `effort` are new start-only
fields.

The internal `tool.DelegateRequest` may retain controller-domain names but gains
an optional runtime selection preserving field presence. The prepared adapter
performs the translation once.

## Boundary Validation and Limits

The new tool introduces fixed byte/time limits (the current implementation has
none beyond `timeout_seconds >= 0`):

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

This preparation boundary is new work, not preserved behavior: the old tool
validated inside execution and returned a nil prepared artifact. The new
implementation performs all decode/validation in `PrepareCall` and introduces
the first Subagent prepared artifact. What carries over is the strict envelope
discipline: unknown-field and trailing-JSON rejection, per-action forbidden
fields, typed UUID handling, and controller-side re-enforcement.

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

The existing foreign-backend seam is the function pair
`foreign.Builder`/`foreign.RestoredBuilder`; native Loops are constructed
directly by the session runtime and never pass through it. That two-function
shape does not scale to several harness profiles, so this design prefers a
profile-keyed builder registry over widening the pair, routing on the resolved
profile carried by `loop.BoundDefinition`. In either shape, routing on
`Engine` alone is insufficient because several distinct ACP harness profiles
can share one generic adapter engine. Routing on display name is also
forbidden. Use an explicit stable profile alias.

Two Harness work items must be named, because the current start machinery has
no path for per-start runtime selection:

- **Bind-time runtime override surface.** Today the delegate controller starts
  a child from one frozen `loop.Definition` per role whose engine, model, and
  effort are fixed at composition, and `loop.Engine` is a closed
  native/foreign-Claude/foreign-Codex enum with no generic adapter value and
  no profile field on `Definition`/`BoundDefinition`. This design adds a
  public `pkg/loop` seam that applies a validated runtime selection (profile
  alias, model, effort) at bind time, plus profile-alias routing for backend
  construction — with the definition fingerprint and restore re-derivation
  updated to match.
- **Catalog injection seam.** `Subagent` is auto-derived inside the session
  runtime from the delegate topology alone; the product-owned runtime catalog
  currently has no way to reach it. This design adds a composition option that
  carries the parent-scoped frozen catalog into the session runtime, where the
  same snapshot feeds both schema derivation and controller validation.

Schema expressibility is not a risk: the current tool already emits per-agent
conditional branches under `additionalProperties: false`, so nested
role/harness/model/effort branches are constructible; only the schema's size
needs watching as catalogs grow.

`foreignloops/driver/acp` owns the concrete ACP driver. It implements the
foreignloops `driver.Agent` contract over `acp/client` and `acp/launch`, and
the existing `foreignloops/backend` Loop plus the live/restore builder
contracts carry it to Harness unchanged. Both the Claude Code and Codex
connectors must be callable through this one driver; the resolved harness
profile alias selects which connector is constructed. Where the driver
contract is CLI-shaped (transcript-oriented history and turn fields), adjust
the contract rather than bypassing it. Pure ACP wire, client, transport, and
launch packages remain Harness-independent and never import harness, core,
inference, or foreignloops. The existing direct-CLI drivers remain supported
siblings; ACP is an additional driver, not a replacement.

Process lifecycle is the load-bearing composition detail. The driver contract
is Spawn-per-turn: the backend closes each turn's stream and cancels the
per-turn context after every turn, and `driver.Agent` has no close method. The
ACP client/process therefore lives outside any `driver.Stream`, owned by the
driver value and bound to the child Loop's lifetime through the context the
`foreign.Builder` receives; Spawn returns per-turn streams over that one
persistent session. The driver must not pass the per-turn context to
`Session.Prompt` — cancelling it would abort the JSON-RPC call instead of
performing the required `session/cancel` — so prompts run under their own
context while a watcher translates turn-context cancellation into
`Session.Cancel` and drains to `TurnInterrupted`. Getting this wrong either
kills the ACP session after the first turn or leaks one process per child.

For every ACP child:

1. resolve the selected profile and gateway alias;
2. create a fresh ACP connector/process/session for that child;
3. borrow the session-owned gateway binding;
4. apply the model selection before the first prompt (effort is already
   embodied by the resolved gateway target, not an ACP session option);
5. verify advertised adapter capabilities and fail closed on mismatch;
6. normalize ACP updates into Harness events;
7. close the ACP client/process when the child Loop closes or construction
   fails.

Interrupt maps through the existing foreignloops turn machinery: the backend
cancels the turn context, and the ACP driver must translate that cancellation
into `session/cancel`, then drain the ACP stream to a clean turn end
(`TurnInterrupted`) rather than abandoning the process mid-stream.

Claude Code applies its model selection through the advertised `model` config
option after `session/new`. Codex receives its model alias at process launch,
so a different model always means a fresh connector/process. Neither connector
expresses effort, and none needs to: effort is bound gateway-side. Any other
ACP adapter must declare an equally explicit model mechanism; a harness
profile is registrable only when a real ACP connector path exists for it —
the env-only Gemini adapter, for example, is not an ACP connector and is not
an admissible ACP harness profile in this release. If a requested tuple cannot
be realized by a registered gateway target and connector, it must not be
advertised.

Claude Code additionally requires a second, small/fast model
(`ClaudeModels.Small`) that the tuple's single `model` selector does not
express. The catalog entry for a Claude-Code-backed profile therefore carries
an explicit small-model alias, defaulting to the main alias when
unconfigured. It is validated like any other alias — registered for the
ingress format and inside the parent's authorized catalog — applied at
session configuration, pinned with the rest of the tuple, and recorded in the
durable runtime identity. It is never model-selectable; an unadvertised
auxiliary alias must not be a side door to a model the parent was not
authorized to use.

## Permission Posture Translation

An ACP child's permissions are governed, not inherited from the foreign
harness's defaults. Each admissible role/profile combination carries a fixed,
role-derived access posture resolved by the composition root into the runtime
profile; the model cannot select, widen, or omit it.

The translation lives in the foreignloops driver layer:

- the neutral posture vocabulary is declared on the foreignloops driver
  contract, so every driver (ACP and CLI alike) consumes the same secret-free
  value and Harness never learns connector-specific permission names;
- `foreignloops/driver/acp` maps that neutral posture per connector — the
  Claude Code permission mode via `session/set_mode`, the Codex
  sandbox/approval posture at launch — and applies it before the first prompt;
- the driver always registers a `session/request_permission` handler; leaving
  the handler nil (which unregisters the method and surrenders the decision to
  the foreign harness) is forbidden. The handler evaluates each request
  against the neutral posture and denies anything outside it.

This release is policy-only: there is no interactive approval path for ACP
children. Denial is the only answer for requests the posture does not already
allow — fail closed, never prompt, never allow-by-timeout. Routing an ACP
child's permission requests into Harness's interactive gate stack is a
separate, explicitly deferred design. The posture is catalog-derived and not
persisted separately; restore re-derives it from the role and the current
catalog like every other runtime property.

## Durability and Restore

`LoopStarted.Runtime` already persists target model identity, limits, and effort,
but ACP reconstruction additionally needs the harness/profile and the
harness-facing gateway alias. Add a secret-free durable runtime identity, for
example:

```go
type AgentRuntime struct {
	Harness         string `json:"harness"`
	CredentialMode  string `json:"credential_mode"` // native-auth | gateway-backed
	ModelAlias      string `json:"model_alias"`
	SmallModelAlias string `json:"small_model_alias,omitempty"`
	ACPSessionID    string `json:"acp_session_id,omitempty"`
}
```

`ACPSessionID` is the agent-assigned session identifier returned by
`session/new`; it is persisted (as an additive event once known) because it is
the resume key. ACP session identifiers are opaque, secret-free identifiers;
they map one-to-one to our child Loop ID but are chosen by the agent harness,
so the mapping must be journaled, not derived.

`ModelAlias` records the concrete harness-facing gateway alias actually used.
When the catalog realizes effort as a distinct per-effort gateway target, the
durable alias is that concrete alias; the model-facing `model`/`effort`
selectors are re-derived through the catalog at restore and must agree, or
restore fails with a typed mismatch.

The exact event shape may be additive fields on `LoopStarted` or a nested value,
but restore must not guess. Native legacy Loops may decode to the existing
definition defaults; a non-native Loop missing required ACP runtime identity
fails closed.

Restore:

- folds the child Loop's persisted harness/profile, alias, model key, limits,
  and effort;
- resolves the profile and alias through the current immutable session catalog;
- verifies the resolved target model key matches the journaled key;
- reconstructs a fresh ACP process and resumes the child's own agent-side
  session: `acp/client` already implements `LoadSession`/`ResumeSession`
  (`session/load`) with load timeouts and typed errors, and both Claude Code
  and Codex assign durable session identifiers, so the primary restore path is
  a fresh process + `session/load` with the journaled `ACPSessionID`, applied
  before any new prompt and only when the adapter advertises the load
  capability;
- otherwise reports a typed restore incompatibility instead of silently
  starting a different harness/model or replaying into a fresh session.

A failed or unavailable resume must not be session-fatal. Today
`attachAndActivate` fails the entire session restore on any child attach
failure; this design adds per-child degraded restore: a child whose runtime
cannot be reconstructed is restored as a closed tombstone Loop — its journal
folds normally, a typed restore-incompatibility event records why, no live
backend attaches, its pending requests resolve as failed, and it still counts
against historical (not active) spawn accounting — while the root and sibling
Loops restore normally. This is a named session-runtime work item, not
optional polish: without it, one dead ACP child bricks the whole session.

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

One acknowledged limitation: Harness depth and spawn quotas govern Harness
Loops only. Agents an ACP harness spawns internally — for example Claude
Code's own subagents — are invisible to Harness quota accounting. A profile
may disable harness-internal subagents where the foreign harness supports
such a setting, and containing the ACP process tree is the sandbox module's
territory; this design does not pretend the ceilings extend inside the
foreign harness.

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
5. driver tests proving the model selection is applied before the first
   prompt, the resolved gateway target embodies the requested effort, and
   unsupported options fail closed;
6. restore tests for exact profile/alias/model/effort recovery and drift
   rejection;
7. race tests for concurrent starts, waits, interrupts, and independent ACP
   teardown;
8. long-running-command compatibility tests proving prepared artifacts remain
   the sole execution boundary;
9. posture tests proving the neutral posture is applied before the first
   prompt, the permission handler is always registered, and out-of-posture
   `session/request_permission` calls are denied;
10. interrupt tests proving a Subagent interrupt drives `session/cancel` and
    ends in a clean `TurnInterrupted`.

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
- The canonical ACP adapter is a `foreignloops` driver callable for both
  Claude Code and Codex; the `acp` packages it consumes (`protocol`,
  `transport`, `client`, `launch`) remain harness/core/inference-free.
- Effort is realized by gateway target selection; no ACP connector change is
  required to honor an admitted effort.
- The old Subagent implementation is deleted; only the new tool exists.
- Every ACP child runs under a role-derived neutral posture translated by the
  foreignloops driver; the permission handler is always registered and
  out-of-posture requests are denied (no interactive path in this release).
- Workspace isolation behavior remains unchanged.
- Primary/root Loops remain native until the ACP control-tool exposure contract
  is designed separately.
