# Subagent Tool Parity and Per-Loop Runtime Selection Implementation Plan

> **Execution note:** implement this plan with `superpowers:executing-plans`,
> test-first, one task at a time, preserving review checkpoints between module
> boundaries.

**Goal:** Upgrade Harness's injected `Subagent` tool to the Claude-style launch
vocabulary and add parent-scoped, per-child selection of agent harness, model
alias, and inference effort. Run ACP children through the inference gateway so
different sibling Loops can use different harness/model/effort combinations.

**Architecture:** Harness owns only secret-free selection, authorization,
durability, and generic backend-builder seams. The product composition root
owns the available runtime catalog and gateway. `inference/gateway` routes a
harness-facing alias from any supported ACP ingress dialect to an injected
model target. `acp/launch` owns safe ACP process/proxy setup. The new
`acp/loop` package directly adapts `acp/client` plus `acp/launch` into a Harness
`loop.Backend` and its live/restore builder contracts.

ACP remains optional: Harness exposes a protocol-neutral opaque
runtime-profile seam and does not import or name ACP types. A product without
ACP registers no ACP-backed profiles. Native delegation and `Subagent`
continue without linking or initializing ACP or the gateway.

**Tech stack:** Go 1.26, Harness prepared-call contracts,
`inference/gateway`, `acp/client`, `acp/launch`, `acp/loop`, and
the existing Looprig model/effort/event types.

---

## Preconditions and Scope

Implement on a branch containing the long-running-command prepared-call
contracts reviewed in `.worktrees/long-running-commands/harness`, or rebase the
paths and assertions onto their merged equivalents first.

The approved design is:

- `docs/plans/2026-07-31-subagent-tool-parity-design.md`

Preserve unrelated changes in every nested repository. In particular, do not
stage or rewrite existing Harness `go.mod`/`go.sum` changes or the current
Inference `transport/client.go` change unless the implementing task explicitly
requires and owns them.

Do not add `isolation`.
Do not convert the primary/root Loop to ACP in this iteration. ACP runtime
selection applies only to children launched through `Subagent`.

The model-facing hard cut is:

```text
agent   -> subagent_type
message -> prompt
wait    -> !run_in_background
```

The new optional, start-only fields are:

```text
agent_harness
model
effort
```

They are capability-derived schema properties, not unconditional parameters.
When a parent has no corresponding runtime choice, the property is absent and
an explicitly supplied value is rejected.

ACP, not MCP, is the agent protocol in this plan.

Run focused and module tests with `GOWORK=off` and `-race`. Refresh module
versions/vendor trees only at the release task, after the producing module has
committed and tagged the needed public contract.

## Delivery Order

The module delivery order is:

```text
inference gateway (already landed)    harness runtime-profile seams
                    \                 /
                     \               /
             acp/client + launch + acp/loop
                         |
          product composition and end-to-end tests
```

The diagram is delivery sequencing. `acp/launch` continues to consume the
gateway only through its structural `ModelProxy`/`ProxyBinding` contract.
`acp/loop` is a product-facing adapter and may import Harness public packages,
`acp/client`, and `acp/launch`. Pure ACP wire packages remain independent:
`acp/launch` must not import Inference or Harness, and Harness must not import
ACP, `acp/loop`, or `inference/gateway`.

### Task 1: Add stable runtime-selection value contracts

**Harness files:**

- Modify: `pkg/tool/definition.go`
- Modify: `pkg/tool/definition_test.go`
- Create: `pkg/loop/delegate_runtime.go`
- Create: `pkg/loop/delegate_runtime_test.go`

**Step 1: Write failing value-contract tests**

Define and test closed, secret-free selector types in `pkg/loop`:

```go
type AgentHarnessName string
type ModelAlias string

type DelegateRuntimeSelection struct {
	AgentHarness AgentHarnessName
	Model        ModelAlias
	Effort       model.Effort
	HarnessSet   bool
	ModelSet     bool
	EffortSet    bool
}

type DelegateRuntimeModel struct {
	Alias         ModelAlias
	Target        model.Model
	DefaultEffort model.Effort
	Efforts       []model.Effort
}

type DelegateRuntimeOption struct {
	AgentHarness AgentHarnessName
	Default      bool
	DefaultModel ModelAlias
	Models       []DelegateRuntimeModel
}
```

The precise representation may use private presence bits or pointers, but the
public behavior must distinguish omitted fields from explicit values.

Pin validation and cloning:

- blank/oversized aliases rejected;
- duplicate harness/model aliases rejected;
- exactly one default harness per non-empty role catalog;
- default model must exist;
- model descriptors validate and are deep-cloned;
- efforts are valid, unique, deterministically ordered, and compatible with
  the target's thinking capability;
- the model-facing token `none` round-trips to internal `model.EffortNone`
  without collapsing into omission; `xhigh` round-trips as
  `model.EffortXHigh`; `ultra` and an explicit empty string are rejected;
- no URL, token, executable, environment, or client is included in a
  model-facing/durable projection.

**Step 2: Extend the controller request/result contract**

Add plain, presence-preserving start fields to `tool.DelegateRequest`. Keep
`pkg/tool` independent of `pkg/loop` and Inference:

```go
AgentHarness string
Model        string
Effort       string
HarnessSet   bool
ModelSet     bool
EffortSet    bool
```

Add a secret-free resolved runtime to `DelegateResult` for `start`:

```go
type DelegateRuntimeResult struct {
	AgentHarness string
	Model        string
	Effort       string
}
```

Prove non-start operations reject/populate none of these fields in later
controller tests.

**Step 3: Implement minimal validation and clone helpers**

Keep implementation handles out of these values. A separate composition seam
will associate an authorized alias with a real native client or ACP profile.

**Step 4: Run focused tests**

```bash
GOWORK=off go test -race ./pkg/tool ./pkg/loop -run 'TestDelegateRuntime|TestDelegateRequestRuntime'
```

Expected: PASS.

**Step 5: Commit Harness value contracts**

Stage only the four owned files.

### Task 2: Add a parent-scoped runtime catalog and resolver seam

**Harness files:**

- Create: `pkg/loop/runtime_profile.go`
- Create: `pkg/loop/runtime_profile_test.go`
- Modify: `pkg/rig/options.go`
- Modify: `pkg/rig/options_test.go`
- Modify: `internal/sessionruntime/session.go`
- Modify: `internal/sessionruntime/command_journal.go`
- Modify: `internal/sessionruntime/lifecycle.go`
- Create: `internal/sessionruntime/delegate_runtime_test.go`

**Step 1: Specify the composition contract in tests**

Add a session-owned resolver/provider seam that can:

1. return the immutable runtime catalog visible to a specific parent/child
   role pair;
2. resolve an authorized selection to a bound child runtime;
3. re-resolve a persisted runtime identity during restore.

The resolved runtime must contain a selected `loop.BoundDefinition` whose
mode, model, effort, engine, and stable runtime profile are already fixed.
Implementation handles remain inside the injected resolver, not in the tool
catalog.

Introduce a stable profile alias on a bound definition, for example:

```go
type RuntimeProfileName string

type RuntimeIdentity struct {
	AgentHarness AgentHarnessName
	ModelAlias   ModelAlias
	Profile      RuntimeProfileName
}
```

`RuntimeProfileName` is secret-free and opaque. It is not an executable name
and is not model-selectable independently of `agent_harness`.

**Step 2: Pin parent scoping and determinism**

Tests must prove:

- two parents may expose different runtime catalogs for the same child role;
- catalogs are defensive snapshots and sort deterministically;
- a child role not in `parent.Delegates()` has no catalog;
- duplicate/ambiguous aliases fail session construction;
- missing resolver leaves legacy/default runtime behavior intact only when no
  runtime-selection catalog is advertised;
- a native-only catalog neither consults nor requires an adapter builder,
  gateway, ACP connector, or ACP availability check;
- requested ACP runtime without a resolver fails closed;
- aliases never route by display name or `Engine` alone.

**Step 3: Implement the narrow options**

Wire live and restore resolution through Session/Lifecycle options. Keep live
and restore construction paired, as the existing backend-builder option does,
so a consumer cannot configure a live-only catalog that restore cannot
interpret.

Do not put ACP, gateway, process, or executable types into Harness public APIs.

**Step 4: Run focused tests**

```bash
GOWORK=off go test -race ./pkg/loop ./pkg/rig ./internal/sessionruntime -run 'TestDelegateRuntime|TestRuntimeCatalog'
```

Expected: PASS.

### Task 3: Add bind-time engine/profile/model/effort overrides

**Harness files:**

- Modify: `pkg/loop/engine.go`
- Modify: `pkg/loop/definition.go`
- Modify: `pkg/loop/bound_overrides.go`
- Modify: `pkg/loop/definition_test.go`
- Modify: `pkg/loop/bound_overrides_test.go`
- Modify: `internal/sessionruntime/session.go`
- Create: `internal/sessionruntime/adapter_newloop_test.go`

**Step 1: Write failing bound-runtime tests**

Add a protocol-neutral generic adapter engine named `EngineAdapter`; do not add
an ACP-named engine to Harness. Keep the existing provider-specific engine
values only as compatibility paths; the new ACP composition must not select
them. Add a sealed
bind-time override that clones a bound definition and changes only:

- engine;
- stable runtime profile;
- full model descriptor;
- effort;
- optional native client when the selected runtime is native.

It must preserve role name, system prompt, selected mode instructions/tools,
access gate, tool limits, delegate permissions, context policy, and output
policy.

Tests must prove two bindings from one immutable role definition can resolve to:

```text
claude-code / sonnet-5 / high
codex       / luna     / max
```

without sharing mutable model sampling, tools, access, or profile state.

**Step 2: Define precedence and pinning**

Select role mode first, then resolve and apply the runtime tuple. Pin the entire
resolved agent-harness/model/effort tuple for every tool-spawned child, including
values chosen by defaults, so later `SetMode`/mode changes cannot replace it.
Omission changes initial resolution only; it does not make an ACP child's
runtime mutable after launch.

The model must pass `ValidateContextModel`; effort must be valid; an adapter
engine must have a non-empty runtime profile; a native engine must have no
adapter profile and must have a usable client.

**Step 3: Route a generic adapter profile through the backend seam**

Update the construction switch so `EngineAdapter` uses the injected backend
builder. The existing public builder types currently live in Harness's
`pkg/foreign` package; treat that package name as a compatibility API detail,
not as the name or ownership model for ACP Loops. The builder receives the
selected bound definition and reads its profile/model/effort. Missing
builder/profile fails closed.

**Step 4: Run focused tests**

```bash
GOWORK=off go test -race ./pkg/loop ./internal/sessionruntime -run 'Test(OverrideBoundRuntime|DelegateRuntimeBinding|AdapterProfile)'
```

Expected: PASS.

### Task 4: Pin the new Subagent schema and catalog projection

**Harness files:**

- Modify: `internal/delegationtool/subagent.go`
- Modify: `internal/delegationtool/subagent_test.go`
- Modify: `internal/sessionruntime/delegation.go`
- Modify: `internal/sessionruntime/delegation_test.go`

**Step 1: Replace schema tests first**

Decode and inspect the JSON Schema. Pin:

- exact name `Subagent`;
- closed top-level object;
- managed actions and sync-only subset;
- start requires `description`, `prompt`, `subagent_type`;
- optional start-only `mode`;
- `agent_harness` is absent and rejected for a native-only parent catalog;
- `agent_harness` appears only in role branches with a genuine runtime-harness
  choice and enumerates that parent's allowed aliases;
- `model` and `effort` are independently absent when only their defaults exist,
  and otherwise are bounded to the selected catalog branch;
- the description renders only compatible tuples and contains no ACP/runtime
  section for a plain native-only catalog;
- `send` forbids every runtime-selection field;
- absence of legacy `agent`, `message`, and `wait`;
- absence of `isolation`.

Keep deterministic concurrent-schema and defensive-copy tests. The catalog
description must never render target provider IDs, model descriptors, URLs,
paths, tokens, or internal profile aliases.

**Step 2: Replace the model-facing argument type**

```go
type SubagentArgs struct {
	Action          SubagentAction     `json:"action,omitempty"`
	Description     string             `json:"description,omitempty"`
	Prompt          string             `json:"prompt,omitempty"`
	SubagentType    identity.AgentName `json:"subagent_type,omitempty"`
	Mode            loop.ModeName      `json:"mode,omitempty"`
	AgentHarness    string             `json:"agent_harness,omitempty"`
	Model           string             `json:"model,omitempty"`
	Effort          string             `json:"effort,omitempty"`
	DelegateID      *uuid.UUID         `json:"delegate_id,omitempty"`
	RequestID       *uuid.UUID         `json:"request_id,omitempty"`
	RunInBackground *bool              `json:"run_in_background,omitempty"`
	TimeoutSeconds  *int               `json:"timeout_seconds,omitempty"`
}
```

Preserve presence independently from string zero values during decode.

**Step 3: Render the bounded availability matrix**

Use a deterministic block such as:

```text
<available_subagents>
- reviewer
  modes: default, review
  runtimes:
  - claude-code: sonnet-5 [low, medium, high]
  - codex: luna [high, max]
</available_subagents>
```

Descriptions are guidance only; they must come from the exact frozen catalog
the controller will enforce.

**Step 4: Run schema tests**

```bash
GOWORK=off go test -race ./internal/delegationtool ./internal/sessionruntime -run 'TestSubagentInfo|TestSubagentCatalog|TestDelegateCatalog'
```

Expected: PASS.

### Task 5: Move all Subagent validation into preparation

**Harness files:**

- Modify: `internal/delegationtool/subagent.go`
- Modify: `internal/delegationtool/subagent_test.go`

**Step 1: Write failing preparation tests**

Cover successful starts with:

- a native-only catalog whose schema and description contain no
  `agent_harness` vocabulary;
- all runtime fields omitted and defaults resolved;
- explicit harness only;
- explicit harness/model;
- explicit effort over default harness/model;
- Claude Code/`sonnet-5`/`high`;
- Codex/`luna`/`max`;
- foreground/background and sync-only behavior.

Cover failures:

- malformed, trailing, scalar, null, array, or oversized JSON;
- unknown/legacy fields and `isolation`;
- action-specific forbidden fields;
- blank/oversized description or prompt;
- unknown role/mode/harness/model/effort;
- `agent_harness`, `model`, or `effort` supplied when the parent catalog did
  not advertise that selector;
- valid individual values in an invalid combination;
- runtime fields on `send`, `wait`, `interrupt`, or `status`;
- invalid UUIDs/timeouts and timeout on a background call;
- attempted values containing paths, URLs, whitespace/control bytes, or
  internal profile identifiers.

Every rejection returns a typed preparation error, empty request, and nil
artifact. Error text is bounded and contains none of the untrusted values.

**Step 2: Implement one private artifact**

```go
type subagentArtifact struct {
	tool.TokenArtifact
	request     tool.DelegateRequest
	description string
}
```

Apply the fixed argument/description/prompt/timeout limits from the design.
Resolve omitted runtime fields through the frozen catalog during preparation,
but preserve which fields were explicit for runtime pinning and diagnostics.

**Step 3: Make execution artifact-only**

`InvokableRun` retrieves `loop.PreparedCallFromContext`, asserts the private
artifact type, copies its prepared request, adds the trusted tool-use ID, and
calls the controller. It never decodes or validates `argsJSON` again.

Add a regression test where raw args differ after preparation; execution must
use only the artifact. Missing/wrong artifacts fail closed.

**Step 4: Run focused tests**

```bash
GOWORK=off go test -race ./internal/delegationtool -run 'TestSubagent(Prepare|Artifact|Run)'
```

Expected: PASS.

### Task 6: Enforce runtime authorization in the scoped controller

**Harness files:**

- Modify: `internal/sessionruntime/delegation.go`
- Modify: `internal/sessionruntime/delegation_test.go`
- Modify: `internal/sessionruntime/limits.go`
- Modify: `pkg/tool/definition.go`
- Modify: `pkg/tool/definition_test.go`

**Step 1: Extend stable failure classification**

Add model-safe codes for:

```text
unknown_agent_harness
unknown_model
unsupported_effort
incompatible_runtime
runtime_unavailable
runtime_drift
```

Retain the existing action/ownership/request/quota classifications. Public
codes carry no selected value or internal cause.

**Step 2: Test bypass resistance**

Construct `tool.DelegateRequest` values directly, bypassing schema and
preparation. Prove the parent-scoped controller rejects unauthorized roles and
runtime tuples, including a harness/model pair where both aliases exist but not
together.

Prove one parent cannot use a tuple granted only to another parent. Existing
depth, cumulative spawn, ownership, interrupt, wait, and send rules remain.

**Step 3: Resolve before quota commit and child creation**

The controller validates and resolves the selection before any ACP process,
Loop registration, durable start event, or initial command is created. Ensure
all post-reservation failure paths release quotas and close partially created
runtime resources.

Return the exact resolved secret-free tuple in `DelegateResult.Runtime`.

**Step 4: Run focused tests**

```bash
GOWORK=off go test -race ./pkg/tool ./internal/sessionruntime -run 'TestDelegate(Failure|Runtime|Start|Quota|Ownership)'
```

Expected: PASS.

### Task 7: Persist runtime identity and make restore exact

**Harness files:**

- Modify: `pkg/event/event.go`
- Modify: `pkg/event/turn.go`
- Modify: `pkg/event/marshal.go`
- Modify: `pkg/event/validate.go`
- Modify: `pkg/event/*runtime*_test.go`
- Modify: `pkg/event/config_fingerprint.go`
- Modify: `pkg/event/config_manifest.go`
- Modify: `internal/sessionruntime/session.go`
- Modify: `internal/sessionruntime/restore.go`
- Modify: `internal/sessionruntime/restore_constructor.go`
- Modify: `internal/sessionruntime/loop_change.go`
- Modify: `internal/loopruntime/restored.go`
- Modify: restore/fingerprint tests under the same packages

**Step 1: Add an additive durable runtime identity**

Persist the stable `agent_harness`, harness-facing `model_alias`, and opaque
runtime profile identity needed to reconstruct the exact backend. Keep
`LoopStarted.Runtime` as the authoritative target model key/limits/effort.

The event must reject:

- ACP runtime without harness/profile/model alias;
- native runtime carrying an ACP profile;
- blank/invalid identifiers;
- invalid model runtime/effort.

Legacy native records remain decodable. Legacy provider-specific records
continue through their existing engine-specific compatibility path; never
reinterpret those compatibility records as a new generic ACP profile without
an explicit migration rule.

**Step 2: Digest the public catalog**

Add a secret-free delegate-runtime catalog digest/revision to the session
manifest/fingerprint. Deterministic reordering must not change it; changing a
harness/model/effort tuple must.

Do not hash executable paths, tokens, URLs, credentials, or raw function/client
identities.

**Step 3: Rehydrate full declared descriptors**

Current restore runtime data contains a model key, limits, and effort, not a
full capability/API descriptor. Resolve the journaled alias from the immutable
catalog, verify its target `ModelKey` equals the journaled key, then overlay the
journaled limits/effort. Reject missing or ambiguous matches.

Apply the same rule in native restored loops and model/effort change folds so a
restored Loop never accidentally uses the initial mode's unrelated descriptor.

**Step 4: Prove exact restore and drift refusal**

Tests cover:

- native and ACP round trips;
- two sibling ACP profiles/models/efforts;
- alias removed;
- alias retargeted to a different model key;
- profile changed or unavailable;
- catalog digest mismatch;
- no fallback to a default harness/model;
- model/effort remains pinned after restore and subsequent mode change.

**Step 5: Run focused tests**

```bash
GOWORK=off go test -race ./pkg/event ./internal/loopruntime ./internal/sessionruntime -run 'Test.*(Runtime|Restore|Fingerprint|Manifest)'
```

Expected: PASS.

### Task 8: Return structured JSON and sanitize all failures

**Harness files:**

- Modify: `internal/delegationtool/subagent.go`
- Modify: `internal/delegationtool/subagent_test.go`

**Step 1: Pin exact success objects**

Use `encoding/json` structs for every action. Start includes only selectors
advertised by the parent catalog. An ACP-enabled start may include:

```json
{
  "action": "start",
  "delegate_id": "...",
  "request_id": "...",
  "status": "queued",
  "runtime": {
    "agent_harness": "claude-code",
    "model": "sonnet-5",
    "effort": "high"
  }
}
```

Pin omission of the entire `runtime` object for a plain native catalog with no
runtime selectors, and omission of `agent_harness` for native-only
model/effort selection.

Send/wait/interrupt/status retain their bounded shapes. Test escaping with
quotes, backslashes, newlines, and Unicode in child output.

**Step 2: Map controller failures by stable code**

Never append `err.Error()` to a model result. Add canary causes containing ACP
argv, executable paths, gateway tokens, URLs, provider messages, prompts,
descriptions, and UUIDs; prove none reaches tool text.

Keep full wrapped causes available to trusted logging/hooks.

**Step 3: Run focused tests**

```bash
GOWORK=off go test -race ./internal/delegationtool -run 'TestSubagent(Result|Failure|Sanit)'
```

Expected: PASS.

### Task 9: Add effort configuration to ACP launch connectors

**ACP files:**

- Modify: `launch/contracts.go` as needed for a neutral connector capability
- Modify: `launch/claude_connector.go`
- Modify: `launch/claude_connector_test.go`
- Modify: `launch/codex_connector.go`
- Modify: `launch/codex.go`
- Modify: `launch/codex*_test.go`
- Modify: `docs/connectors/inference-gateway.md`

**Step 1: Pin adapter-specific behavior**

Claude Code:

- locate only an advertised select option whose category is
  `thought_level` (or the exact pinned ACP category name);
- resolve only configured Looprig effort aliases from the closed external set
  `none|low|medium|high|xhigh|max`, mapping `none` to the internal zero value;
- apply it with `session/set_config_option` after `session/new` and before the
  first prompt;
- fail with a typed capability/alias error when requested but unavailable.

Codex:

- add a validated `model_reasoning_effort=<level>` launch override;
- keep model and effort fixed for the connector lifetime;
- require a new connector/process/session for another tuple;
- never use speculative extension calls.

Neither connector may clamp `xhigh`/`max`, reinterpret explicit `none` as an
omitted default, or admit `ultra`. A catalogue tuple is executable only when
the adapter and gateway can preserve its exact selected effort.

Do not assume every adapter supports every effort. Connector capability must be
explicit and testable.

**Step 2: Preserve launch security**

Keep absolute executable paths, allowlisted environments, no ambient
credentials, no shell, local gateway token only, and existing teardown rules.
Effort/model values are validated catalog aliases, never raw config fragments.

**Step 3: Run ACP verification**

```bash
GOWORK=off go test -race ./launch
GOWORK=off go test -race ./client ./protocol ./transport/stdio
```

Then run the ACP module's required secure checks before committing.

### Task 10: Implement the Harness adapter in `acp/loop`

**ACP files:**

- Create: `loop/config.go`
- Create: `loop/builder.go`
- Create: `loop/backend.go`
- Create: `loop/session.go`
- Create: `loop/updates.go`
- Create: `loop/restore.go`
- Create matching unit, race, cancellation, restore, and fuzz tests
- Modify: `internal/boundary/deps_test.go`
- Modify: `CLAUDE.md`
- Modify: module/package documentation

**Step 1: Declare the product-facing package boundary**

`acp/loop` is the only new ACP package allowed to import Harness. It directly
adapts `acp/client` and `acp/launch` to Harness public contracts. Its `Backend`
implements `harness/pkg/loop.Backend`; its builder exposes live and restored
functions assignable to the existing Harness builder contracts (currently
exported from `harness/pkg/foreign`).

Update ACP dependency-boundary tests and documentation so pure wire packages
(`protocol`, `transport`, `client`, and `launch`) remain independent of
Harness/Inference while this explicit product adapter may import Harness.
There is no intermediate direct-CLI driver and no dependency on a separate
Loop-adapter module.

**Step 2: Define a profile-keyed ACP factory**

Create an immutable profile router/factory that receives the selected bound
definition's stable runtime profile, model alias, and effort and returns one
child-owned managed ACP client/session. It owns connector factories and the
shared gateway binding reference, while the product owns validated executable
and environment configuration.

Do not route on `Engine`, role name, display name, model provider, or raw
model-facing input. Unknown profiles fail closed before process launch.

**Step 3: Build one ACP client/session per child Loop**

Use `acp/launch.Dial` with the session's shared gateway `ProxyBinding`. The ACP
Loop adapter must not construct executables, arguments, URLs, or environment
values from model-facing strings.

For each new child:

1. resolve its approved profile/model/effort connector configuration;
2. create one `launch.ManagedClient` and initialize the ACP peer;
3. call `session/new` with the session workspace CWD;
4. apply adapter-specific model and effort controls before the first prompt;
5. receive Harness commands and submit them to that pinned ACP session;
6. convert ACP updates directly into Harness events and snapshots;
7. close the ACP session/client/process on Loop close, failure, or cancellation.

**Step 4: Preserve conversation and restore semantics**

Follow-up turns reuse only the same child ACP session. For restore, use
`session/load`/`session/resume` only when the selected adapter advertises and
the adapter proves that capability. Otherwise return a typed
restore-unavailable error; never start a blank session under an old Harness
Loop ID.

**Step 5: Prove independent siblings and cleanup**

Tests use fake ACP peers and a fake shared proxy to prove:

- Claude Code and Codex child sessions coexist;
- each receives its own model/effort before first prompt;
- prompts, updates, event publication, and snapshots do not cross sessions;
- interrupt cancels only the addressed child;
- closing one child does not close the shared gateway or sibling;
- partial construction unwinds every process/session/resource;
- malformed ACP updates fail safely and fuzz without panic/leak.

**Step 6: Run ACP Loop verification**

```bash
GOWORK=off go test -race ./loop ./client ./launch
```

Then run the ACP module's full secure and boundary checks.

### Task 10A: Keep Harness and ACP tool ownership separate

**Harness and ACP files:** add focused compatibility tests and contract
documentation beside the runtime-profile and `acp/loop` code.

Do not send `loop.BoundDefinition.Tools()` to an ACP child and do not claim the
ACP child executes them. Pin the two supported cases:

- a native runtime profile executes the role's bound Harness tools through the
  existing native runner;
- an ACP runtime profile executes its agent harness's own tool surface; Harness
  observes normalized tool events and enforces the explicitly mapped ACP
  permission posture.

Add role/profile compatibility requirements so a role that requires a
native-only Harness tool is not advertised for an ACP profile. For this
release, keep the portable role projection narrow: system/instructions,
runtime/model/effort, workspace CWD, and mapped access posture. Do not add a
Harness-to-ACP tool translator.

This is a boundary test/documentation task, not a new MCP server or tool bridge.
A later optional adapter may export selected Harness tools only through the same
prepared/gated/audited invocation boundary.

### Task 11: Compose the gateway, catalog, and ACP Loops in Carbon/product

**Consumer files:** discover the active product composition root with `rg`
before editing. Carbon is the intended consumer, but do not assume stale paths
or its current local replace targets.

**Step 1: Build one immutable source configuration**

From product config, construct:

- inference clients and full target model descriptors;
- an ACP harness profile declaring each harness's ingress API format;
- a `gateway.Mux` route for every admitted `(ACP ingress format, alias)`, whose
  resolved target carries its own, independently validated
  `model.Model.APIFormat` egress format;
- one loopback `gateway.Server` shared by the session;
- an `acp/loop` profile factory for each installed/preflighted agent harness;
- the parent-scoped Subagent runtime catalog derived from those same routes and
  profiles.

Register the `acp/loop` live and restore builders through Harness's existing
backend-builder option. Carbon must not import or register the direct Claude
or Codex CLI Loop integrations for this path.

Do not create the gateway catalog and Subagent catalog from independent lists.
A startup validator must reject route/profile/catalog drift.

For every model alias admitted under more than one harness, compile one route
per harness ingress. Tests must distinguish:

```text
Claude Code + sonnet-5: Anthropic ingress -> Anthropic target
Codex       + sonnet-5: Responses ingress -> Anthropic target
```

Both use the gateway. The first is same-dialect routing; the second exercises
translation. Never derive target/egress format from the selected harness.

**Step 2: Wire parent-specific policy**

Intersect session availability with each parent role's delegation policy. A
reviewer may be allowed fewer child roles/models/efforts than a primary
operator. The existing child access/tool/depth ceilings remain authoritative.

**Step 3: Add the two-runtime acceptance fixture**

Configure product aliases (fixture names are fine):

```text
reviewer -> claude-code ACP -> sonnet-5 -> high
worker   -> codex ACP       -> luna     -> max
```

Start both from one parent, run them concurrently through the same gateway, and
assert distinct Loop runtime events, ACP sessions, outputs, interrupts, and
teardown.

Also prove cross-dialect routing by targeting at least one model whose provider
dialect differs from its ACP harness's ingress dialect.

**Step 4: Startup and shutdown tests**

Reject missing binaries, failed version preflight, bad routes, unsupported
effort, incompatible model capabilities, duplicate aliases, or unavailable
profiles before advertising the tuple. On shutdown, close child ACP clients
before the shared gateway.

### Task 12: Update fixtures, fuzzing, docs, and compatibility

**Harness and consumer files:**

- Search all non-vendor Go/Markdown/JSON fixtures for old Subagent calls
- Modify: `internal/delegationtool/subagent_test.go` fuzz target
- Modify: relevant Harness/consumer docs and examples

**Step 1: Migrate all model-facing calls**

```bash
rg -n '"agent"|"message"|"wait"|Subagent' --glob '!vendor/**' --glob '!docs/plans/2026-07-31-subagent-tool-parity-*'
```

Review hits manually; do not blindly rewrite unrelated domain fields.

**Step 2: Fuzz the real preparation boundary**

Seed valid/invalid runtime tuples, legacy fields, malformed JSON, oversized
values, invalid efforts, runtime fields on non-start actions, path/URL-like
aliases, and canary secrets. Assert no panic, bounded failures, and no untrusted
echo.

```bash
GOWORK=off go test ./internal/delegationtool -run '^$' -fuzz FuzzSubagentPrepare -fuzztime=30s
```

**Step 3: Update public documentation**

Document:

- role vs agent-harness vs model vs effort;
- default resolution and start-only pinning;
- catalog ownership at the composition root;
- gateway is routing, not discovery;
- one ACP process/session per child Loop;
- ACP is optional and Harness sees only opaque runtime profile aliases;
- native children use bound Harness tools while ACP children use their agent
  harness's own tools in this iteration;
- ACP is unrelated to MCP;
- `isolation` remains deferred;
- ACP primary/root Loops are deferred because exporting Harness control tools
  into an ACP agent harness needs a separate prepared/gated tool bridge;

### Task 13: Full verification and release discipline

Run each module independently from its repository root with `GOWORK=off`.

**Harness:**

```bash
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
```

Run Harness's repository-required secure/lint commands, then repeat against the
long-running-command worktree or its merged result. Explicitly confirm:

- prepared artifact is the sole Subagent execution input;
- no long-running process/resource API was repurposed as delegation transport;

**ACP:**

```bash
GOWORK=off go test -race ./...
```

Run ACP's secure target and verify its boundary test still proves `launch`,
`client`, `protocol`, and `transport` do not import Harness/Inference. Verify
`acp/loop` is the sole new Harness-importing ACP adapter and run its
process-leak/cancellation integration tests on Darwin and Linux.

**Inference:**

No gateway change should be necessary. Run its full race suite to prove the
shared multi-route gateway remains correct. If implementation reveals a needed
gateway change, stop and revise this plan rather than smuggling catalog or ACP
policy into Inference.

**Consumer/product:**

Run the complete suite, including the two-runtime concurrent acceptance test,
restore, shutdown, and config-drift tests.

Inspect every nested repository status and diff. Stage only files owned by the
current task. Release/tag in dependency order; replace local module mappings
with tagged versions and refresh committed vendor trees only after each
producer release exists.

## Completion Evidence

Record in the implementation handoff:

- exact commits/tags for Harness, ACP, Inference, and the product;
- focused and full verification commands with exit status;
- the catalog shown to the test parent Loop;
- durable `LoopStarted` runtime identities for both example children;
- evidence that Claude Code/`sonnet-5`/`high` and Codex/`luna`/`max` ran as
  independent ACP child Loops through one gateway;
- restore and runtime-drift rejection evidence;
- confirmation that `isolation` behavior did not change;
- confirmation that the primary/root Loop composition remained unchanged;
- remaining known limitations, especially adapters without proven restore or
  effort controls.
