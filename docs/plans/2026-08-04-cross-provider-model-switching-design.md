# Cross-Provider Model Switching Design

**Date:** 2026-08-04

**Status:** Proposed

## Goal

Let a consumer (CodeRig's TUI `/model` command) switch a running loop's live
model between operator-configured candidates that may sit on **different**
providers, wire dialects, or endpoints (e.g. a local LM Studio model and a
remote Chutes-hosted model) — not just different model names on one fixed
backend. Today `pkg/loop` hard-rejects any such switch. This document designs
the harness-side mechanism that lifts that restriction while preserving the
security invariant the restriction was protecting.

This is a harness-only design. The CodeRig-side model-router composition
(building the `ContextTransport` set from `~/.looprig/models.json`, wiring its
existing per-request `{Provider, APIFormat, BaseURL, Name}` client router) is
out of scope and left to separate follow-up work in the `coderig` repo.

## Problem Statement

`pkg/loop.BoundDefinition.ValidateContextModel` and the live `ChangeModel`
path both reject a candidate model whose `Provider`, `APIFormat`, or `BaseURL`
differs from the loop's originally-bound model
(`pkg/loop/compaction_policy.go:122-133`, `validateContextTransportBinding`).
A consumer that wants `/model` to move a loop from a local unencrypted
endpoint to a remote TLS-protected one (or vice versa, or between two remote
providers) cannot do so on a live loop today — it can only rebuild a brand new
loop under a different definition, losing conversation continuity.

## Why The Binding Exists (Verified)

The binding is **not** a context-counting compatibility constraint. Verified
in `vendor/github.com/looprig/inference/contextcount/contracts.go`:

- `CounterCapability` (the counter's own trust/quality metadata) and
  `InferenceCapability` (the *inference path's* trust posture — transport
  encryption tier, provider identity, retention) are two separate types.
- `CompatibleCounter(inf InferenceCapability, counter CounterCapability) error`
  (`contracts.go:211-271`) is the actual counter-compatibility gate, and it
  has an explicit early-out: `providerNeutralCounter(counter)`
  (`contracts.go:273-278`) returns `true` — meaning "always compatible,
  regardless of `inf`" — whenever the counter is
  `Transport=Local, Retention=None, Provider="", SecurityIdentity=={}`.
  CodeRig's counter is exactly this shape (a provider-agnostic heuristic
  estimator: `CountQualityHeuristicEstimate`, no provider-specific tokenizer).
  So a real transport switch never invalidates counter compatibility for
  CodeRig's counter — `CompatibleCounter` would pass against *any* declared
  transport's `InferenceCapability` without change.
- What actually gets frozen and compared 1:1 against the bound model is
  `InferenceCapability` — `Provider`, `Transport` (Local / TLS / AttestedTLS /
  EndToEndEncrypted), `SecurityIdentity`, `Retention`
  (`contracts.go:168-207`). This is a security/trust-tier assertion made once
  at loop-definition time (`WithInferenceCapability`,
  `pkg/loop/definition.go:822-831`) and trusted for the loop's entire life:
  every measurement fingerprint, every compaction-executor validation, and
  every request's context-budget accounting assumes it never changes
  mid-session.

So `validateContextTransportBinding` is a trust-tier invariant check
masquerading as a transport-identity check: it works today only because one
loop definition can declare exactly one `(Provider, APIFormat, BaseURL)` →
exactly one `InferenceCapability`. The fix is to let a definition declare
**several** validated `(transport → capability)` pairs up front, and let a
live switch move between validated set members instead of being compared
against a single frozen value.

## What The Prior Consultation Got Right, and What This Corrects

The prior fable-model design was substantially accurate. Corrections/additions
found by reading the current tree:

1. **Two independent restore-graft sites, not one.** The prior write-up
   named only `internal/sessionruntime/loop_change.go`'s `applyModelRuntime`
   (lines 131-138, confirmed exact). There is a **second**, functionally
   duplicate graft inside `internal/loopruntime/restored.go`,
   `NewRestoredWithRuntime` (lines 134-140), which is the one that actually
   seeds the restored actor's `cfg.Model` — `applyModelRuntime` in
   `sessionruntime` only recomputes the lightweight `Handle` view
   (`liveViewFor`, used for `Handle.Mode()/Model()` reporting). **Both** must
   graft the new additive `ModelRuntime` fields identically or the live
   `Handle` view and the actor's real running model can diverge after
   restore.
2. **`InferenceCapability` is frozen in three places, not two.** Confirmed:
   `internal/loopruntime/config.go:132-134` (`runtimeConfig`, resolved once in
   `resolveMode`, called once at `New`/mode-change — but mode-change does
   *not* currently re-propagate it into the live actor state, see below);
   `internal/loopruntime/compaction_executor.go:17-24,90-94` frozen into
   `compactionExecutorConfig` at construction (`installCompactionExecutor`,
   called exactly once, from `New`, `loop.go:338`); and used at
   `internal/loopruntime/loop.go:1102,1341,1347,2508-2509` and
   `compaction_executor.go:296,309`. All of these currently read one
   loop-lifetime-constant value. None of them currently re-read it after a
   mode change or an inference change.
3. **The `AllowConfigMismatch` mechanism does not compose the way the "one
   parallel mechanism" framing suggested — it's the wrong layer.**
   `WithAllowConfigMismatch` / `RestoreDecider` / `DriftAssessment`
   (`pkg/session/decider.go`, `pkg/event/drift.go`,
   `pkg/event/config_fingerprint.go`) is a **session-level, pre-loop-construction**
   policy decision over an aggregate `ConfigFingerprint`/`ConfigManifest`
   comparison (step 2 of `RestoreTopology`,
   `internal/sessionruntime/restore_constructor.go:23-49`). It answers "has
   the whole session's config drifted enough that a human/policy should
   decide whether to resume at all" *before* any loop is bound. A specific
   loop's restored model no longer being a member of the *current*
   definition's declared transport set is a **different, later** failure
   mode: it happens during `attachRestoredLoop` →
   `loopruntime.NewRestoredWithRuntime` (step 6, "build all loops"), which is
   unconditionally fail-secure today — any error there is wrapped
   `RestoreLoopFailed` and fails the whole restore with no override, exactly
   like an unknown restored mode name or an invalid model would. So the new
   check **composes by fitting into that same already-unconditional
   fail-secure step**, not by piggybacking on `WithAllowConfigMismatch`
   (which only ever suppresses the earlier, coarser, aggregate check). A
   drifted transport set is a structural "this session cannot be safely
   resumed as bound" fact, not a policy trade-off `DriftAssessment` is built
   to arbitrate (that mechanism trades off Info/Warn severity for things like
   tool-set or prompt drift where "resume anyway" is a coherent choice; there
   is no coherent "resume anyway" for a resolved model whose trust tier is now
   unknown).
4. **Exact confirmed line numbers** (all read from the working tree at HEAD
   `3bbe44c4`, branch `feat/cross-provider-model-switching`):
   - `pkg/loop/compaction_policy.go:110-133` — `validateContextTransportBinding`.
   - `pkg/loop/compaction_policy.go:138-146` — `RequestFingerprintInput`.
   - `pkg/loop/definition.go:190-243` — `validateContextDefinition`, mode-binding
     loop at line 237.
   - `pkg/loop/definition.go:358-454` — `PolicyRevision` (not 404-435 as
     estimated; the anonymous projection struct itself is 404-429, but the
     whole method including the `contextCounter` conditional block that must
     grow is 358-454).
   - `pkg/loop/definition.go:872-891` — `Definition.ValidateContextModel` /
     `validateDefinitionContextModel`.
   - `pkg/loop/definition.go:622-658` — the `BoundDefinition` interface.
   - `internal/loopruntime/loop.go:2169-2220` — `applyChangeInference`; the
     transport-binding gate is at line 2197 (`cfg.bound.ValidateContextModel`),
     confirmed.
   - `internal/loopruntime/config.go:91-200` — `runtimeConfig`;
     `InferenceCapability` field at line 134; resolved at lines 65-69 inside
     `resolveMode` (config.go:50-79).
   - `internal/loopruntime/compaction_executor.go:17-24` —
     `compactionExecutorConfig`; frozen at construction lines 90-94; used at
     lines 296 and 309 inside `prepare`.
   - `pkg/event/turn.go:8-34` — `ModelRuntime`, `LoopInferenceChanged`,
     `LoopModeChanged`.
   - `pkg/event/event.go:453-460` — `LoopStarted.Runtime`.
   - `internal/sessionruntime/loop_change.go:116-138` — `liveViewFor`,
     `applyModelRuntime`.
   - `internal/loopruntime/restored.go:106-146` —
     `NewRestoredWithRuntime`, the graft at lines 134-140.
   - `internal/sessionruntime/restore_constructor.go:51-96` —
     `attachRestoredLoop`.

## Chosen Mechanism: Declarative `ContextTransport` Sets

### New type (`pkg/loop`, new file `context_transport.go`)

```go
// ContextTransport is one admitted (wire transport → trust posture) pair a
// loop definition allows a live model switch to move to. Provider/APIFormat/
// BaseURL together identify the transport; Capability is the trust posture
// InferenceCapability declares for it.
type ContextTransport struct {
    Provider   model.ProviderName
    APIFormat  model.APIFormat
    BaseURL    string
    Capability contextcount.InferenceCapability
}

// contextTransportKey is the wire-identity projection used for set membership,
// uniqueness, and lookup. It deliberately excludes Capability and Name/Effort:
// Effort is never part of transport/trust identity (see product decision #3),
// and Name varies freely within one transport.
type contextTransportKey struct {
    Provider  model.ProviderName
    APIFormat model.APIFormat
    BaseURL   string
}

func transportKeyOf(m model.Model) contextTransportKey {
    return contextTransportKey{Provider: m.Provider, APIFormat: m.APIFormat, BaseURL: m.BaseURL}
}
```

### `WithContextTransports` option

```go
// WithContextTransports declares the additional transports a live model
// switch may move this loop to, beyond the base transport WithInference +
// WithInferenceCapability already declare. Omitting it (existing callers)
// synthesizes the historical single-member set from the base model's
// transport and WithInferenceCapability's value — byte-identical behavior.
// When supplied, ONE member must exactly match the base model's transport key
// AND that member's Capability must equal the WithInferenceCapability value
// (a definition never contradicts itself about its own base transport's trust
// posture); every OTHER member is a genuinely additional admitted transport.
func WithContextTransports(transports ...ContextTransport) Option
```

Design choice: `WithInferenceCapability` remains the single source of truth
for the base transport's capability (no behavior change for the ~all existing
callers that never call `WithContextTransports`). `WithContextTransports`
purely *extends* the admitted set; it cannot redeclare or contradict the base
member. This keeps the singleton-option validation pattern the rest of
`definitionOptions` already uses (`o.singleton(name)`,
`pkg/loop/definition.go:80-86`) and avoids a second, competing owner of "what
is the base transport's capability."

### Build-time validation (replaces/extends `validateContextDefinition`)

In `pkg/loop/definition.go:190-243`, after the existing counter/capability
checks (`contextcount.CompatibleCounter`, line 220):

1. Resolve `transports := resolved.contextTransports`; if empty, synthesize
   the one-element set from `resolved.model` + `resolved.inferenceCapability`.
2. If non-empty and caller-supplied, require one member's key ==
   `transportKeyOf(resolved.model)` and that member's `Capability ==
   resolved.inferenceCapability` (`DefinitionInvalidContextTransport`
   otherwise).
3. For every member: `member.Capability.Validate()` must pass
   (`DefinitionInvalidContextTransport`); keys must be unique
   (`DefinitionDuplicateContextTransport`); and
   `contextcount.CompatibleCounter(member.Capability, capability)` must pass
   (`DefinitionIncompatibleContextCounter`, reusing the existing kind — it *is*
   the same check, just run once per declared transport instead of once for
   the sole transport). For CodeRig's provider-neutral counter this is a
   structural no-op per transport (see "Why The Binding Exists" above) but the
   check stays generic for a future non-neutral counter.
4. Replace the mode-binding loop at line 237
   (`validateContextTransportBinding(resolved.model, mode.Model)`) with a
   set-membership check: `mode.Model`'s transport key must be `∈ transports`
   (`DefinitionInvalidModeBinding` unchanged as the wrapping kind; the
   underlying cause becomes the new `ContextTransportNotDeclaredError` instead
   of the old `ContextTransportBindingError`).
5. Freeze the normalized `transports` slice (sorted by
   `(Provider, APIFormat, BaseURL)`) onto `definitionState.contextTransports`.

New/changed error types in `pkg/loop/definition_errors.go` and
`pkg/loop/compaction_policy.go`:

```go
const (
    DefinitionDuplicateContextTransport DefinitionErrorKind = "duplicate_context_transport"
    DefinitionInvalidContextTransport   DefinitionErrorKind = "invalid_context_transport"
)

// ContextTransportNotDeclaredError reports a candidate model whose transport
// is not a member of the loop definition's declared ContextTransport set.
// It replaces validateContextTransportBinding's single-value comparison
// (ContextTransportBindingError is removed — it is an unexported-surface,
// pre-adoption rename; no shipped consumer depends on the old shape).
type ContextTransportNotDeclaredError struct {
    Provider  model.ProviderName
    APIFormat model.APIFormat
    BaseURL   string
}
```

### `ValidateContextModel` becomes set membership

`pkg/loop/definition.go:872-891`:

```go
func validateDefinitionContextModel(state *definitionState, m model.Model) error {
    if err := m.Validate(); err != nil { return err }
    if err := m.Key().Validate(); err != nil { return err }
    if state.contextCounter == nil { return nil }
    if _, ok := lookupTransport(state.contextTransports, m); !ok {
        return &ContextTransportNotDeclaredError{Provider: m.Provider, APIFormat: m.APIFormat, BaseURL: m.BaseURL}
    }
    return nil
}
```

### New `BoundDefinition` accessor

`pkg/loop/definition.go:622-658` (the `BoundDefinition` interface) gains:

```go
// ContextTransportCapability resolves the declared InferenceCapability for
// model's transport, or (zero, false) if that transport is not a member of
// this definition's declared set.
ContextTransportCapability(model.Model) (contextcount.InferenceCapability, bool)
```

implemented on `boundDefinitionState` by delegating to the frozen
`definitionState.contextTransports` (mirrors the existing
`InferenceCapability()`/`CounterCapability()` accessor pattern at
`definition.go:717-722`).

### `PolicyRevision` hashes the declared set once

`pkg/loop/definition.go:358-454`. The anonymous `projection` struct gains a
`ContextTransports []ContextTransport `json:",omitempty"`` field (sorted by
key, same discipline `Modes`/`Delegates` already use — `slices.SortFunc`
before marshal, `definition.go:401,403`), populated whenever
`d.state.contextCounter != nil` (same guard as the existing
`CounterCapability`/`InferenceCapability` block, lines 430-435). This makes
the declared SET part of the definition's identity: adding, removing, or
re-capabilitying a transport changes `PolicyRevision` (and therefore
`TopologyRev` — `pkg/rig/fingerprint.go`'s `writeLoopTopology`, which folds in
`candidate.PolicyRevision()`), so an operator widening or narrowing the
admitted set between sessions is visible as ordinary topology drift (`DriftTopology`,
`Info` severity today — an explicit, intentional consequence: expanding which
transports a loop MAY switch to is not itself a live security event, only an
actual live switch is, and that is bounded by the runtime-membership check,
not by drift detection).

`PolicyRevision` itself stays computed once at `Define` and never changes
within a session: the declared SET is hashed once; a live switch between two
members of that already-hashed set is an ordinary runtime event inside the
admitted policy, not a config change. This matches the doc comment already on
`PolicyRevision` (`definition.go:358-361`) and requires no change to that
invariant.

## Live-Change Application

`internal/loopruntime/loop.go:2169-2220`, `applyChangeInference`. The gate at
line 2197 changes from a single-value transport-equality check
(`cfg.bound.ValidateContextModel`, still called, now set-based per the change
above) to also **resolving and carrying forward** the new model's capability:

```go
if cfg.bound != nil {
    if verr := cfg.bound.ValidateContextModel(model); verr != nil {
        c.Ack <- command.LoopChangeResult{Err: &loop.ChangeError{Kind: loop.ChangeInvalidModel, Cause: verr}}
        return
    }
    if capability, ok := cfg.bound.ContextTransportCapability(model); ok {
        nextCapability = capability
    }
}
```

`command.ChangeLoopInference` / `loop.ChangeModel` need **no signature
change** — confirmed: `model.Model` already carries `Provider`, `APIFormat`,
`BaseURL`, `Name` (`vendor/github.com/looprig/inference/model/model.go:13-22`),
and `command.ChangeLoopInference` (`pkg/command/loop_change.go:63-70`) already
carries a full `model.Model`. `loop.ChangeError.Kind` stays `ChangeInvalidModel`
for a not-declared transport — no new `ChangeErrorKind` needed, only a new
`Cause` type wrapped inside it.

## Un-Freezing `InferenceCapability`

Confirmed today: `InferenceCapability` is resolved exactly once
(`resolveMode`, `config.go:65-69`) into `runtimeConfig.InferenceCapability`
(a loop-lifetime constant) and copied exactly once more into
`compactionExecutorConfig.InferenceCapability`
(`installCompactionExecutor`, called once from `New`,
`compaction_executor.go:80-100`). Every read site
(`loop.go:1102,1341,1347,2508-2509`; `compaction_executor.go:296,309`) reads
one of these two frozen copies. Neither is touched by `applySetMode` or
`applyChangeInference` today, because until now they could never legitimately
differ from the value resolved at construction.

The fix moves capability resolution into the loop's existing **per-turn
effective state** (`effectiveConfig`, `loop.go:233-239`, the same struct that
already carries the live `model`/`effort`/`system`/`tools` and that both
`applySetMode` and `applyChangeInference` already mutate at a turn boundary):

1. Add `inferenceCapability contextcount.InferenceCapability` to
   `effectiveConfig`.
2. Seed it at construction (`loop.go:447-454`, where `state.effective` is
   first built) and at restore (`newLoopWithSeed`) from
   `cfg.InferenceCapability` (the base-transport value `resolveMode` already
   produces) — byte-identical to today for every loop that never switches.
3. `applySetMode` (`loop.go` mode-change arm) re-resolves it from
   `cfg.bound.ContextTransportCapability(resolved.Model)` alongside the model
   it already re-resolves — a mode declaring a model on a different
   transport already changes the effective model; it must also change the
   effective capability.
4. `applyChangeInference` sets it from the same
   `ContextTransportCapability` lookup described above.
5. Every read site that currently reads the frozen `config.InferenceCapability`
   (`loop.go:1102,1341,1347,2508-2509`) reads `state.effective.inferenceCapability`
   instead — these all execute on the actor goroutine at the point a turn
   is admitted/measured, so they already have race-free access to
   `state.effective`.
6. `compactionExecutorConfig` **drops** its frozen `InferenceCapability`
   field (`compaction_executor.go:21`); the executor's public entry points
   (`CoordinateCompactionCandidate`, `compactionExecutionCandidate`) instead
   carry the triggering turn's `InferenceCapability` as a field on
   `compactionExecutionCandidate` (`compaction_executor.go:26-32`), threaded
   in by the caller (`loop.go`, wherever it builds the candidate today,
   already has `state.effective` in scope) and read at
   `compaction_executor.go:296,309` (`prepare`) from `candidate.InferenceCapability`
   instead of `e.config.InferenceCapability`. `CounterCapability` is left
   frozen on `compactionExecutorConfig` as-is — the counter itself never
   changes on a transport switch (one `ContextCounter` per definition, and
   `CompatibleCounter`'s provider-neutral fast path means it is compatible
   with every declared transport by construction, verified above).

This is the one genuinely architectural change in the plan: `InferenceCapability`
moves from "resolved once, trusted forever" to "resolved once, re-resolved on
every mode/inference change, and read from the SAME per-turn effective state
the model and effort already live in." No other collaborator (context
admission, fingerprinting, restore) needs to change its *shape* — they already
take `InferenceCapability` as an explicit parameter; only where that parameter
value comes from changes.

## Restore / Replay

### Durable event shape

`pkg/event/turn.go:8-34`. `ModelRuntime` today:

```go
type ModelRuntime struct {
    Key    model.ModelKey      `json:"key"`
    Limits model.ContextLimits `json:"limits"`
    Effort model.Effort        `json:"effort,omitzero"`
}
```

confirmed to carry no `APIFormat`/`BaseURL`/capability, and confirmed embedded
(directly or via `HustleRunDescriptor`) in `LoopInferenceChanged`
(`turn.go:19-24`), `LoopModeChanged` (`turn.go:26-34`), and `LoopStarted`
(`event.go:453-460`, field `Runtime`). Extend it **additively**:

```go
type ModelRuntime struct {
    Key       model.ModelKey      `json:"key"`
    Limits    model.ContextLimits `json:"limits"`
    Effort    model.Effort        `json:"effort,omitzero"`
    APIFormat model.APIFormat     `json:"api_format,omitzero"`
    BaseURL   string              `json:"base_url,omitzero"`
}
```

Both new fields are `omitzero`; an old journal record decodes them as `""`,
which the fold below treats as "use the definition's base transport" —
today's exact semantics, preserved. `validateModelRuntime`
(`pkg/event/validate.go:788-799`) gets no NEW required-field checks: a
present `APIFormat`/`BaseURL` is carried, not independently re-validated,
because it always originates from a `model.Model` that already passed
`model.Validate()` at the point `ChangeLoopInference`/`SetLoopMode` accepted
it (`loop.go:2188-2201`).

### Both grafting sites, kept in lockstep

`internal/loopruntime/restored.go:134-140` (the one that actually seeds the
restored actor's model) and `internal/sessionruntime/loop_change.go:131-138`
(`applyModelRuntime`, used only for the reported `Handle` view) must both
graft the two new fields:

```go
if seed.HasRuntime {
    if seed.Runtime.APIFormat != "" { cfg.Model.APIFormat = seed.Runtime.APIFormat }
    if seed.Runtime.BaseURL != "" { cfg.Model.BaseURL = seed.Runtime.BaseURL }
    cfg.Model.Provider = seed.Runtime.Key.Provider
    cfg.Model.Name = seed.Runtime.Key.Model
    cfg.Model.Limits = seed.Runtime.Limits
    cfg.Model.Sampling = cfg.Model.Sampling.Clone()
    cfg.Model.Sampling.Effort = seed.Runtime.Effort
}
```

(mirrored in `applyModelRuntime`). Today these two functions are independent,
duplicated implementations of the same fold rule; this plan keeps them
duplicated (consistent with the existing structure) but requires both be
updated together, and calls this out explicitly as a drift risk worth a
shared-helper follow-up (out of scope here — not touching working,
already-tested call sites beyond what this feature requires).

### New restore-time hard validation

Today `NewRestoredWithRuntime` performs **no** validation of the folded model
against the bound definition at all (confirmed — the graft at lines 134-140
runs unconditionally and the result feeds straight into `newLoopWithSeed`).
That was safe by construction: a single-transport definition's folded
`Provider`/`Name` always originated from that same definition's one legal
transport. With multiple declared transports this is no longer guaranteed —
an operator can remove a transport from `~/.looprig/models.json` between
sessions, or a definition can change between builds. Add an explicit,
unconditional check right after the graft:

```go
if seed.HasRuntime && bound.ContextCounter() != nil {
    if _, ok := bound.ContextTransportCapability(cfg.Model); !ok {
        return nil, &RestoreTransportMismatchError{
            Provider: cfg.Model.Provider, APIFormat: cfg.Model.APIFormat, BaseURL: cfg.Model.BaseURL,
        }
    }
}
```

`RestoreTransportMismatchError` is a new typed error in `internal/loopruntime`
(or `pkg/loop` if a public surface is warranted — recommend `internal/loopruntime`
since `NewRestoredWithRuntime`'s errors are already internal-only). It
propagates exactly like today's other construction failures: returned from
`NewRestoredWithRuntime` → wrapped `&RestoreError{Kind: RestoreLoopFailed,
Cause: err}` in `attachRestoredLoop` (`restore_constructor.go:87-90`) → the
whole `RestoreTopology` fails, durably records `RestoreErrored`, and releases
the lease (per the documented restore contract,
`restore_constructor.go:39-45`). This is **not** routed through
`RestoreDecider`/`WithAllowConfigMismatch`: as established above, that
mechanism answers a *different* question (aggregate session-level drift,
decided *before* any loop is built) and there is no coherent "accept anyway"
answer for a resolved model whose trust tier can no longer be resolved. A
restored session can always still succeed via the normal path (an operator
who removed a transport but left the loop's last-selected model referencing
one that is still declared restores cleanly; only a genuinely dangling
selection fails, exactly like a dangling restored mode name would).

## Fingerprinting: Free Invalidation, Confirmed

`RequestFingerprintInput` (`pkg/loop/compaction_policy.go:138-146`) includes
both `model.Model` (full) and `InferenceCapability` (full), and `RequestFingerprint`
(`compaction_policy.go:164-196`) hashes the whole struct with SHA-256. Traced
the actual call path: `internal/loopruntime/context.go:319-341`
(`contextFingerprintTemplateForRequest`) builds this input from the live
request's `Model` and the CURRENT effective `InferenceCapability` on every
measurement, and `compaction_executor.go:286-289` (`prepare`) explicitly
rejects a summary whose `Model`/`RequestFingerprint` no longer match the
CURRENT `candidate.Measurement` before committing it. Separately,
`internal/loopruntime/context_replacement.go:39-46` re-checks a pending
context-replacement's stale fingerprint against the actor's live state before
applying it. So a pre-switch measurement's fingerprint structurally cannot
match a post-switch fingerprint (different `Model` field alone already
changes the hash; a different `InferenceCapability` changes it again) — a
switch invalidates every prior open measurement/compaction candidate for
free, through machinery that already exists and needs no new code.

## Design Decision: Compaction Summaries Are Preserved Across a Switch

**Decided, not open.** A transport-crossing live switch does **not** force a
compaction or context reset. The existing committed conversation (including
any prior compaction summaries) carries forward unchanged into the first
request built against the new transport.

**Rationale:** `InferenceCapability`'s trust boundary protects where **future**
request bytes travel — it says nothing about content already committed to the
durable conversation. A user-initiated `/model` switch is itself the
authorization event: choosing to point a loop at a different endpoint is
implicit, in-the-moment consent to send the existing conversation there next
turn (that is the entire point of offering the switch — an operator who
wanted to keep prior content off the new endpoint would not switch this loop
to it). Forcing a reset on every switch would make the feature nearly
unusable for its actual purpose (resuming a long-running task on a different
backend, e.g. because a local model ran out of budget) while providing no
security benefit the transport-membership check does not already provide:
`ValidateContextModel`/`ContextTransportCapability` are exactly the gate on
*whether a given endpoint is allowed at all*; once a target is admitted, its
own declared `InferenceCapability` (retention posture, encryption tier) is
the caller's already-made trust judgment about that endpoint, independent of
what triggered the request. Nothing in the fingerprinting or fold machinery
needs a "wipe" step — the free-invalidation property (above) is about
*measurements*, not the conversation content itself, and the conversation
content is deliberately never wiped.

## Product Decisions (Settled)

These were decided upstream of this document and are recorded here, not
re-litigated:

1. **No confirmation/consent prompt required for a local → remote switch.**
   Harness stays mechanism-only and neutral here: `ChangeModel` performs no
   UI-level confirmation of any kind for any transport pair. Any consent UX
   is a CodeRig TUI decision, not a harness concern.
2. **Compaction summaries are preserved, never force-reset, across a
   transport-crossing switch.** See the dedicated section above.
3. **Effort is never part of transport/capability identity.** `contextTransportKey`
   deliberately excludes `Sampling.Effort` (and all of `Sampling`); effort
   stays a per-request sampling parameter validated by each model's own
   declared effort set (`model.Effort.Valid()`, `Model.Sampling.Effort`),
   completely orthogonal to `ContextTransport`/`InferenceCapability`. No
   change to existing effort validation is needed or made.

## Backward Compatibility

Every existing caller that never calls `WithContextTransports` gets a
synthesized one-element set built from its existing `WithInference` model and
`WithInferenceCapability` value — structurally and behaviorally identical to
today: `ValidateContextModel` accepts exactly the one transport it always
accepted (a one-element membership check is exactly the old equality check);
`PolicyRevision` includes a one-element `ContextTransports` list, which is a
`PolicyRevision` byte-content change but not a *behavioral* one — however,
because `PolicyRevision` feeds `TopologyRev`, deploying this change will shift
every consumer's `TopologyRev`/`ConfigFingerprint` once, which surfaces as a
one-time `DriftTopology` (`Info` severity, auto-accepted by
`DefaultPolicyDecider`) on the first restore after upgrade — the same
category of one-time, harmless drift any additive `PolicyRevision` field
already causes (see e.g. `pkg/loop/definition.go:449` note about hashable
projection fields). This is called out explicitly rather than left implicit.

`ContextTransportBindingError` is removed and replaced by
`ContextTransportNotDeclaredError`. This is a breaking Go API rename of an
already-exported type. It is acceptable here because (a) the type is a
narrow, mechanical validation-failure descriptor with no known external
`errors.As` dependents yet (the feature that would create the first live
cross-provider switch does not exist until this plan ships), and (b) keeping
the old name with re-purposed multi-member semantics would be more confusing
than a clean rename. No wire-format/journal compatibility is affected — this
type never crosses the durable-event boundary.

## Out of Scope

- The CodeRig-side `~/.looprig/models.json` → `ContextTransport` set
  composition, and wiring the existing `RuntimeClient` per-request model
  router in place of a single fixed `PrimerClient`. Separate follow-up in the
  `coderig` repo.
- Any TUI-level consent/confirmation UX for a transport-crossing switch
  (settled: none required at the harness layer).
- Unifying the two independent `ModelRuntime`-fold implementations
  (`restored.go` vs. `loop_change.go`) into one shared helper. Both are
  updated identically by this plan; consolidating them is flagged as a
  worthwhile but separate cleanup.

## Critical Files for Implementation

- `pkg/loop/definition.go`
- `pkg/loop/compaction_policy.go`
- `pkg/loop/context_transport.go` (new)
- `internal/loopruntime/loop.go`
- `internal/loopruntime/config.go`
- `internal/loopruntime/compaction_executor.go`
- `internal/loopruntime/restored.go`
- `internal/sessionruntime/loop_change.go`
- `pkg/event/turn.go`
