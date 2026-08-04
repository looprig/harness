# Cross-Provider Model Switching Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:test-driven-development for every task below.

**Goal:** Let a `pkg/loop` definition declare a set of admitted `ContextTransport`s (provider/API-format/base-URL → `InferenceCapability`) instead of one implicit transport, so a live `ChangeModel` can move between declared transports — including cross-provider — while preserving the trust-tier invariant `InferenceCapability` exists to protect, preserving conversation continuity across the switch, and restoring safely.

**Architecture:** `pkg/loop.Definition` gains an optional declarative `ContextTransport` set (default: synthesized single member, byte-identical to today). `pkg/loop.BoundDefinition` gains a lookup from a candidate model to its declared capability. `internal/loopruntime` moves `InferenceCapability` from a loop-lifetime-frozen `runtimeConfig`/`compactionExecutorConfig` value into the per-turn `effectiveConfig`, re-resolved on every mode change and inference change. `pkg/event.ModelRuntime` gains two additive fields (`APIFormat`, `BaseURL`) so restore can reconstruct which declared transport a durable model selection belongs to; both restore-fold sites graft them; a new typed error fails restore hard (no override) when a folded selection is no longer a declared transport.

**Tech Stack:** Go 1.26 (module `github.com/looprig/harness`), stdlib only, `github.com/looprig/inference/{model,contextcount}` (already a dependency), existing vendored tree (`-mod=vendor`), Go race detector, table-driven tests.

---

## Execution protocol

For every numbered task below:

1. Write one focused failing test first (`superpowers:test-driven-development`).
2. Run it and confirm the RED failure for the reason described (compile error or assertion failure, not an unrelated error).
3. Implement the minimum code to turn it GREEN.
4. Run the focused test, then the owning package's full test suite with `-race`.
5. `gofmt -l` the touched files (must be empty) before committing.
6. Commit with the message shown.

Use:

```bash
GOWORK=off GOFLAGS=-mod=vendor go test -race ./path/to/package -run '<Focused test>'
GOWORK=off GOFLAGS=-mod=vendor go test -race ./path/to/package
```

**`GOWORK=off` is required, not optional.** This worktree sits outside the
top-level `~/code/looprig/go.work` workspace's tracked module paths.
`GOFLAGS=-mod=vendor` alone resolves against the wrong (top-level looprig)
vendor tree and fails with "inconsistent vendoring" errors. Always set
`GOWORK=off` alongside `GOFLAGS=-mod=vendor` for every build/test/vet/lint
command in this plan.

At the end of each phase, run the full suite (`GOWORK=off GOFLAGS=-mod=vendor go test -race ./...`) and `make secure` before moving to the next phase. Do not start a phase with a red full suite or an open `make secure` finding.

Work in the existing worktree `~/code/looprig/harness/.worktrees/cross-provider-model-switching`, branch `feat/cross-provider-model-switching`. Do not touch vendored files except via `make vendor` (not needed here — no new external dependency).

---

# Phase 1 — Declarative `ContextTransport` type and build-time validation

## Task 1.1: Add `ContextTransport` type, key projection, and lookup helpers

**Files:**
- Create: `pkg/loop/context_transport.go`
- Create: `pkg/loop/context_transport_test.go`

**Step 1 — failing test.** Table-driven test `TestContextTransportKeyOf` proving `transportKeyOf(model.Model)` projects exactly `Provider`/`APIFormat`/`BaseURL` (two models differing only in `Name` or `Sampling.Effort` produce equal keys; differing in any of `Provider`/`APIFormat`/`BaseURL` produce different keys). Table-driven test `TestLookupTransport` proving `lookupTransport(set, model)` returns `(capability, true)` for a member and `(zero, false)` for a non-member, including the empty-set case.

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./pkg/loop -run 'TestContextTransportKeyOf|TestLookupTransport'
```
Expected: compile failure (`ContextTransport`, `transportKeyOf`, `lookupTransport` undefined).

**Step 3 — implement.**
```go
package loop

import (
    contextcount "github.com/looprig/inference/contextcount"
    model "github.com/looprig/inference/model"
)

// ContextTransport is one admitted (wire transport -> trust posture) pair a
// loop definition allows a live model switch to move to.
type ContextTransport struct {
    Provider   model.ProviderName
    APIFormat  model.APIFormat
    BaseURL    string
    Capability contextcount.InferenceCapability
}

type contextTransportKey struct {
    Provider  model.ProviderName
    APIFormat model.APIFormat
    BaseURL   string
}

func transportKeyOf(m model.Model) contextTransportKey {
    return contextTransportKey{Provider: m.Provider, APIFormat: m.APIFormat, BaseURL: m.BaseURL}
}

func lookupTransport(set []ContextTransport, m model.Model) (contextcount.InferenceCapability, bool) {
    key := transportKeyOf(m)
    for _, t := range set {
        if (contextTransportKey{Provider: t.Provider, APIFormat: t.APIFormat, BaseURL: t.BaseURL}) == key {
            return t.Capability, true
        }
    }
    return contextcount.InferenceCapability{}, false
}
```

**Step 4 — GREEN**, then:
```bash
GOFLAGS=-mod=vendor go test -race ./pkg/loop
```

**Step 5 — commit:**
```bash
git add pkg/loop/context_transport.go pkg/loop/context_transport_test.go
git commit -m "feat(loop): add ContextTransport type and set-lookup helpers"
```

## Task 1.2: Add `WithContextTransports` option and `ContextTransportNotDeclaredError`, remove `ContextTransportBindingError`

**Files:**
- Modify: `pkg/loop/context_transport.go`
- Modify: `pkg/loop/definition.go` (`definitionState`, `definitionOptions`)
- Modify: `pkg/loop/compaction_policy.go` (remove `ContextTransportBindingError`, `validateContextTransportBinding`)
- Modify: `pkg/loop/context_transport_test.go`
- Modify: `pkg/loop/compaction_policy_test.go` (drop/replace tests for the removed type — search first: `grep -n ContextTransportBindingError pkg/loop/compaction_policy_test.go`)

**Step 1 — failing test.** `TestWithContextTransports_Singleton` proves calling `WithContextTransports` twice on one `Define` call fails with `DefinitionDuplicateOption`. `TestWithContextTransports_RequiresBaseMember` proves `Define` fails with `DefinitionInvalidContextTransport` when the caller-supplied set omits a member matching the base model's transport key, or includes one whose `Capability` differs from the `WithInferenceCapability` value.

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./pkg/loop -run 'TestWithContextTransports'
```
Expected: compile failure (`WithContextTransports`, `DefinitionInvalidContextTransport` undefined).

**Step 3 — implement.** Add to `definitionState`/`definitionOptions`: `contextTransports []ContextTransport`. Add:
```go
func WithContextTransports(transports ...ContextTransport) Option {
    transports = append([]ContextTransport(nil), transports...)
    return func(o *definitionOptions) error {
        if err := o.singleton("context_transports"); err != nil {
            return err
        }
        o.contextTransports = transports
        return nil
    }
}
```
Add `DefinitionDuplicateContextTransport`, `DefinitionInvalidContextTransport` to `pkg/loop/definition_errors.go`. Add to `pkg/loop/context_transport.go`:
```go
// ContextTransportNotDeclaredError reports a candidate model whose transport
// is not a member of a loop definition's declared ContextTransport set.
type ContextTransportNotDeclaredError struct {
    Provider  model.ProviderName
    APIFormat model.APIFormat
    BaseURL   string
}

func (e *ContextTransportNotDeclaredError) Error() string {
    return "loop: context model transport is not a declared ContextTransport"
}
```
Remove `ContextTransportBindingError` and `validateContextTransportBinding` from `compaction_policy.go` (lines 110-133) — leave every other declaration in that file untouched. Do not wire the base-member/uniqueness/validate checks into `Define` yet — that is Task 1.3. This task only adds the option, the singleton guard, and the new error types so Task 1.3 has something to call.

**Step 4 — GREEN:**
```bash
GOFLAGS=-mod=vendor go test -race ./pkg/loop
```
This will fail to compile until `definition.go`'s mode-binding call site (line 237, `validateContextTransportBinding`) is updated — update it now to call a new (still-permissive, always-`nil`) placeholder `validateContextTransportMembership(transports []ContextTransport, m model.Model) error` defined in `context_transport.go` that returns `nil` when `len(transports) == 0` (preserves today's behavior exactly until Task 1.3 populates `transports`), else does the real lookup. This keeps every existing test green while unblocking compilation.

**Step 5 — commit:**
```bash
git add pkg/loop/context_transport.go pkg/loop/definition.go pkg/loop/definition_errors.go pkg/loop/compaction_policy.go pkg/loop/context_transport_test.go pkg/loop/compaction_policy_test.go
git commit -m "feat(loop): add WithContextTransports option and ContextTransportNotDeclaredError"
```

## Task 1.3: Wire full build-time validation into `validateContextDefinition`

**Files:**
- Modify: `pkg/loop/definition.go` (`validateContextDefinition`, lines 190-243)
- Modify: `pkg/loop/definition_test.go` (or the file that already tests `Define`'s context-policy validation — locate with `grep -rn DefinitionMissingContextCounter pkg/loop/*_test.go`)

**Step 1 — failing tests** (table-driven, added to the existing `Define` context-policy test table):
- Omitting `WithContextTransports` synthesizes a one-element set; a mode whose model matches the base transport still validates (regression case, must already pass).
- A declared transport with an invalid `Capability` (e.g. `Transport: InferenceTransportUnknown`) fails `DefinitionInvalidContextTransport`.
- Two declared transports with the same `(Provider, APIFormat, BaseURL)` fail `DefinitionDuplicateContextTransport`.
- A declared transport incompatible with the counter (construct a non-provider-neutral fake `CounterCapability` requiring a specific provider, then declare a transport with a different provider) fails `DefinitionIncompatibleContextCounter`.
- A mode's model on a SECOND declared transport (not the base) now validates successfully (this is the new capability the whole feature exists to unlock) — assert `Define` succeeds and the resulting bound definition's `ContextTransportCapability` resolves it.
- A mode's model on an UNDECLARED third transport still fails `DefinitionInvalidModeBinding` wrapping `*ContextTransportNotDeclaredError`.

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./pkg/loop -run TestDefine
```
Expected: the new-transport-succeeds and undeclared-transport-fails cases fail (validation not yet wired); the base-member/duplicate/incompatible cases fail to compile against not-yet-existing behavior or simply don't fail as expected.

**Step 3 — implement.** In `validateContextDefinition` (`definition.go:190-243`), after the existing `contextcount.CompatibleCounter` check (line 220) and before the mode-binding loop (line 233):
```go
transports := resolved.contextTransports
if len(transports) == 0 {
    transports = []ContextTransport{{
        Provider: resolved.model.Provider, APIFormat: resolved.model.APIFormat, BaseURL: resolved.model.BaseURL,
        Capability: resolved.inferenceCapability,
    }}
} else {
    baseKey := transportKeyOf(resolved.model)
    foundBase := false
    seen := make(map[contextTransportKey]struct{}, len(transports))
    for _, t := range transports {
        key := contextTransportKey{Provider: t.Provider, APIFormat: t.APIFormat, BaseURL: t.BaseURL}
        if _, dup := seen[key]; dup {
            return &DefinitionError{Kind: DefinitionDuplicateContextTransport, Field: "context_transports"}
        }
        seen[key] = struct{}{}
        if err := t.Capability.Validate(); err != nil {
            return &DefinitionError{Kind: DefinitionInvalidContextTransport, Field: "context_transports", Cause: err}
        }
        if err := contextcount.CompatibleCounter(t.Capability, capability); err != nil {
            return &DefinitionError{Kind: DefinitionIncompatibleContextCounter, Field: "context_transports", Cause: err}
        }
        if key == baseKey {
            foundBase = true
            if t.Capability != resolved.inferenceCapability {
                return &DefinitionError{Kind: DefinitionInvalidContextTransport, Field: "context_transports"}
            }
        }
    }
    if !foundBase {
        return &DefinitionError{Kind: DefinitionInvalidContextTransport, Field: "context_transports"}
    }
}
resolved.contextTransports = transports
```
Replace the mode-binding loop's call (line 237) from `validateContextTransportBinding(resolved.model, mode.Model)` to the real `validateContextTransportMembership(transports, mode.Model)` implemented in Task 1.2's placeholder — now given the real `transports` it performs the actual lookup and returns `*ContextTransportNotDeclaredError` on miss. Also validate the BASE model's own transport is `∈ transports` right after synthesizing/normalizing (it always is by construction in the synthesized case, and is enforced by `foundBase` in the caller-supplied case — add an explicit assertion-style check only if the synthesized branch could ever disagree, which it cannot; no extra code needed there).

**Step 4 — GREEN**, then full package:
```bash
GOFLAGS=-mod=vendor go test -race ./pkg/loop
```

**Step 5 — commit:**
```bash
git add pkg/loop/definition.go pkg/loop/definition_test.go
git commit -m "feat(loop): validate declared ContextTransport sets at Define time"
```

## Task 1.4: `BoundDefinition.ContextTransportCapability` and set-based `ValidateContextModel`

**Files:**
- Modify: `pkg/loop/definition.go` (`BoundDefinition` interface at 622-658, `boundDefinitionState`, `validateDefinitionContextModel` at 872-891, `definitionState` gains frozen `contextTransports`)
- Modify: `pkg/loop/definition_test.go`

**Step 1 — failing test.** `TestBoundDefinition_ContextTransportCapability` proves: (a) for a definition with no `WithContextTransports`, the synthesized base transport resolves via `ContextTransportCapability` and any other transport returns `(zero, false)`; (b) for a definition with two declared transports, both resolve to their own distinct `Capability`, and a third undeclared transport returns `(zero, false)`. `TestValidateContextModel_SetMembership` proves `Definition.ValidateContextModel`/`BoundDefinition.ValidateContextModel` accept every declared member and reject a non-member with `*ContextTransportNotDeclaredError`.

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./pkg/loop -run 'TestBoundDefinition_ContextTransportCapability|TestValidateContextModel_SetMembership'
```
Expected: compile failure (`ContextTransportCapability` not on the interface).

**Step 3 — implement.** Freeze `resolved.contextTransports` onto `state.contextTransports` in `Define` (the `state := resolved.definitionState` clone block, `definition.go:168-187`) — clone the slice (`append([]ContextTransport(nil), state.contextTransports...)`) like every other cloned slice field there. Add to the `BoundDefinition` interface:
```go
ContextTransportCapability(model.Model) (contextcount.InferenceCapability, bool)
```
Implement on `boundDefinitionState`:
```go
func (b *boundDefinitionState) ContextTransportCapability(m model.Model) (contextcount.InferenceCapability, bool) {
    return lookupTransport(b.definition.contextTransports, m)
}
```
Rewrite `validateDefinitionContextModel` (872-891) to use `lookupTransport` instead of `validateContextTransportBinding`:
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

**Step 4 — GREEN**, then full package plus a check that every other `BoundDefinition` implementation in the tree still compiles (fake/test doubles):
```bash
GOFLAGS=-mod=vendor go build ./...
GOFLAGS=-mod=vendor go test -race ./pkg/loop ./internal/loopruntime ./internal/sessionruntime ./pkg/rig
```
Fix any test double implementing `BoundDefinition` that now needs the new method (search `grep -rln "boundDefinition()" $(find . -name '*_test.go' -not -path './vendor/*' -not -path './.worktrees/*')`).

**Step 5 — commit:**
```bash
git add pkg/loop/definition.go pkg/loop/definition_test.go <any test-double files fixed>
git commit -m "feat(loop): add BoundDefinition.ContextTransportCapability, set-based ValidateContextModel"
```

## Task 1.5: `PolicyRevision` hashes the declared transport set

**Files:**
- Modify: `pkg/loop/definition.go` (`PolicyRevision`, 358-454)
- Modify: `pkg/loop/definition_test.go` (existing `PolicyRevision` stability/uniqueness tests — locate with `grep -n TestPolicyRevision pkg/loop/definition_test.go`)

**Step 1 — failing test.** Extend the existing `PolicyRevision` uniqueness table: two otherwise-identical definitions with different `WithContextTransports` sets produce different `PolicyRevision()` values; two definitions with the SAME set supplied in different slice order produce the SAME `PolicyRevision()` (order-independence); a definition with no `WithContextTransports` call produces the SAME `PolicyRevision()` before and after this change (regression — capture the revision on `main` behavior by asserting it only depends on existing fields when the set is synthesized... in practice: assert it stays stable across two `Define` calls with identical single-transport config, which already passes and stays passing).

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./pkg/loop -run TestPolicyRevision
```
Expected: the order-independence and different-set cases fail (both currently hash nothing about transports beyond the existing single `InferenceCapability`).

**Step 3 — implement.** In the `PolicyRevision` projection struct (`definition.go:404-421`), add `ContextTransports []ContextTransport `json:",omitempty"``. Before marshaling, when `d.state.contextCounter != nil`, sort a clone of `d.state.contextTransports` by `(Provider, APIFormat, BaseURL)` using `slices.SortFunc` (same helper already imported) and assign it to `projection.ContextTransports`.

**Step 4 — GREEN**, then:
```bash
GOFLAGS=-mod=vendor go test -race ./pkg/loop
```

**Step 5 — commit:**
```bash
git add pkg/loop/definition.go pkg/loop/definition_test.go
git commit -m "feat(loop): hash the declared ContextTransport set into PolicyRevision"
```

### Phase 1 checkpoint
```bash
GOFLAGS=-mod=vendor go test -race ./...
make secure
```
Fix any finding before Phase 2.

---

# Phase 2 — Per-turn effective `InferenceCapability` and live-change application

## Task 2.1: Add `inferenceCapability` to `effectiveConfig`, seed it at construction and restore

**Files:**
- Modify: `internal/loopruntime/loop.go` (`effectiveConfig` at 233-239, its two construction sites: `New`'s seed at 447-454, and `newLoopWithSeed`)
- Modify: `internal/loopruntime/loop_test.go` (or the closest existing effective-config test file — locate with `grep -rln "effectiveConfig{" internal/loopruntime/*_test.go`)

**Step 1 — failing test.** A focused test constructing a loop with `WithContextCounter`+`WithInferenceCapability` (the existing `context_loop_test.go` fixtures already do this) and asserting — via a new tiny accessor described below, or via triggering a context measurement and inspecting which capability was used — that the loop's initial effective capability equals `bound.InferenceCapability()`'s value. Since `effectiveConfig` is unexported actor state, drive this through the public/internal seam already used elsewhere in this package: add a minimal test-only actor query or, simpler, assert indirectly through an existing context-measurement test's `CounterCapability`/`InferenceCapability`-sensitive assertion (e.g. `context_loop_test.go` already exercises `measureRequestContext` inputs — extend one such existing test to assert on the NEW effective-state source once Task 2.2 makes it observable). For this task alone (pure plumbing, no behavior change yet), a compile-level test suffices: add the field and assert `effectiveConfig{}.inferenceCapability` zero-values correctly and that `New`'s constructed loop's (test-visible) effective state equals `cfg.InferenceCapability` immediately after construction, using whatever the package's existing internal test helper for peeking at actor state already is (search `grep -n "func.*peek\|internalState\|debugState" internal/loopruntime/*_test.go` first — reuse, do not invent a new export).

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime -run <chosen test name>
```
Expected: compile failure (`inferenceCapability` field does not exist) or assertion failure against the zero value.

**Step 3 — implement.** Add `inferenceCapability contextcount.InferenceCapability` to `effectiveConfig` (`loop.go:233-239`). Set it at both existing construction sites (`loop.go:447-454`'s `state.effective = effectiveConfig{...}` literal, and wherever `newLoopWithSeed` builds the restored equivalent) to `cfg.InferenceCapability` — the value `resolveMode` already resolved for the base/restored mode. No other behavior changes in this task.

**Step 4 — GREEN**, then:
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime
```

**Step 5 — commit:**
```bash
git add internal/loopruntime/loop.go internal/loopruntime/<test file>
git commit -m "feat(loopruntime): seed effectiveConfig.inferenceCapability from resolved mode"
```

## Task 2.2: Re-resolve effective capability on `SetMode` and `ChangeLoopInference`

**Files:**
- Modify: `internal/loopruntime/loop.go` (`applySetMode`, `applyChangeInference` at 2169-2220)
- Modify: `internal/loopruntime/context_loop_test.go` (or create `internal/loopruntime/context_transport_switch_test.go`)

**Step 1 — failing test.** Build a `loop.Definition` with `WithContextTransports` declaring two members (base local + a second remote-shaped transport with a distinct `InferenceCapability`, e.g. `Transport: InferenceTransportTLS, Provider: "chutes", SecurityIdentity: <non-zero>`), a mode on the second transport, and `WithCompaction`/`WithContextObservation` per the existing fixtures' pattern. Drive: (a) `SetMode` to the second-transport mode updates the effective capability (assert via a subsequent context measurement's `InferenceCapability` — the existing test fixtures already capture counter calls with the capability passed in, e.g. `context_loop_test.go`'s counter double records inputs); (b) a direct `ChangeLoopInference` (via `command.ChangeLoopInference`) to a model on the second transport (while staying in the base mode) likewise updates it; (c) a `ChangeLoopInference` to a model on an undeclared third transport is refused with `loop.ChangeInvalidModel` wrapping `*loop.ContextTransportNotDeclaredError` and the effective capability is UNCHANGED (refusal applies nothing, matching the existing all-or-nothing contract already documented on `applyChangeInference`).

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime -run TestContextTransportSwitch
```
Expected: assertion failures (capability still reads the base-transport value after switching) — the plumbing from Task 2.1 exists but nothing re-resolves it yet.

**Step 3 — implement.** In `applySetMode` (the mode-change arm around `loop.go:2093-2116`), where `resolved.Model` is already used to build `next`, add:
```go
nextCapability := state.effective.inferenceCapability
if capability, ok := cfg.bound.ContextTransportCapability(resolved.Model); ok {
    nextCapability = capability
}
next := effectiveConfig{mode: modeName, model: resolved.Model, effort: resolved.Model.Sampling.Effort, system: resolved.System, tools: nextTools, inferenceCapability: nextCapability}
```
In `applyChangeInference` (2169-2220), inside the `if c.SetModel` block right after the existing `ValidateContextModel` check (line 2197-2201), resolve and stage the new capability into a local, and only assign it to `state.effective.inferenceCapability` at the SAME point the function already commits `state.effective.model`/`state.effective.effort` (line 2216-2217) — preserving the function's existing all-or-nothing-until-committed structure:
```go
nextCapability := state.effective.inferenceCapability
if cfg.bound != nil {
    if capability, ok := cfg.bound.ContextTransportCapability(model); ok {
        nextCapability = capability
    }
}
// ... existing commitContextConfigurationChange(...) call, unchanged ...
state.effective.model = model
state.effective.effort = effort
state.effective.inferenceCapability = nextCapability
```

**Step 4 — GREEN**, then:
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime
```

**Step 5 — commit:**
```bash
git add internal/loopruntime/loop.go internal/loopruntime/<test file>
git commit -m "feat(loopruntime): re-resolve effective InferenceCapability on mode/inference change"
```

## Task 2.3: Thread effective capability into request measurement (replace frozen `config.InferenceCapability` reads)

**Files:**
- Modify: `internal/loopruntime/loop.go` (call sites at 1102, 1341, 1347, 2508-2509)
- Modify: `internal/loopruntime/context_loop_test.go`

**Step 1 — failing test.** Extend the Task 2.2 test (or add a new one) to prove that a context-admission measurement taken AFTER a transport-crossing `ChangeLoopInference` uses the NEW capability, not the one resolved at loop construction — assert on the counter double's recorded `InferenceCapability` argument for a turn started after the switch.

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime -run <test name>
```
Expected: assertion failure — these call sites still read `config.InferenceCapability` (the frozen `runtimeConfig` value).

**Step 3 — implement.** At each of `loop.go:1102`, `1341`, `1347`, `2508-2509`, replace `config.InferenceCapability` with `state.effective.inferenceCapability` (these all already execute on the actor goroutine with `state` in scope — confirm at each site before editing; if a given call site is inside a helper that does not currently receive `state`, thread it through as an explicit parameter rather than reaching for a package global). Leave `config.CounterCapability` reads unchanged (the counter itself never varies per transport — see design doc).

**Step 4 — GREEN**, then:
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime
```

**Step 5 — commit:**
```bash
git add internal/loopruntime/loop.go internal/loopruntime/<test file>
git commit -m "feat(loopruntime): read InferenceCapability from per-turn effective state, not frozen config"
```

## Task 2.4: Un-freeze `compactionExecutorConfig.InferenceCapability` into a per-candidate value

**Files:**
- Modify: `internal/loopruntime/compaction_executor.go` (`compactionExecutorConfig` 17-24, `installCompactionExecutor` 80-100, `compactionExecutionCandidate` 26-32, `prepare` 273-325)
- Modify: `internal/loopruntime/loop.go` (wherever a `compactionExecutionCandidate` is constructed — locate with `grep -n "compactionExecutionCandidate{" internal/loopruntime/*.go`)
- Modify: `internal/loopruntime/compaction_hooks_test.go`, `internal/loopruntime/safe_boundary_compaction_test.go` (constructors that set `InferenceCapability:` on `compactionExecutorConfig` — will need updating to the new shape; enumerate with `grep -rln "InferenceCapability:" internal/loopruntime/*_test.go`)

**Step 1 — failing test.** A compaction-path test (extend an existing one in `compaction_hooks_test.go` or `safe_boundary_compaction_test.go`) proving: a compaction triggered AFTER a transport-crossing switch produces a `compactionPreparedSuccess`/measurement whose fingerprint reflects the NEW `InferenceCapability`, not whatever was frozen at executor construction. Simplest reliable assertion: construct the executor once (as today), then invoke `CoordinateCompactionCandidate` twice with two different `InferenceCapability` values on the candidate and assert the two resulting `PostCount.Fingerprint`/measurements differ purely due to that field (holding everything else fixed).

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime -run <test name>
```
Expected: compile failure once `compactionExecutionCandidate` gains the new field but `prepare` still reads `e.config.InferenceCapability` (or, sequenced the other way, an assertion failure if the field is added but not yet threaded through — prefer the compile-failure ordering: write the test against the target shape first).

**Step 3 — implement.**
- Remove `InferenceCapability contextcount.InferenceCapability` from `compactionExecutorConfig` (line 21) and its `Validate()` call in `newCompactionExecutor` (lines 74-76) — capability is no longer known at executor-construction time. Move that validation to occur per-candidate instead (see below).
- Add `InferenceCapability contextcount.InferenceCapability` to `compactionExecutionCandidate` (26-32).
- In `CoordinateCompactionCandidate` (126-163), validate `candidate.InferenceCapability.Validate()` before starting the goroutine (mirrors the removed constructor-time check, now per-call); refuse with the existing `compactionExecutorError{Field: "inference_capability"}` shape on failure.
- In `prepare` (273-325), replace both `e.config.InferenceCapability` reads (296, 309) with `candidate.InferenceCapability`.
- In `installCompactionExecutor` (80-100), drop `InferenceCapability: config.InferenceCapability` from the `compactionExecutorConfig{...}` literal (line 92).
- At the `loop.go` call site(s) that build a `compactionExecutionCandidate`, add `InferenceCapability: state.effective.inferenceCapability` (the per-turn value from Task 2.1/2.2).
- Update every test that sets `InferenceCapability:` on `compactionExecutorConfig` in a struct literal to instead set it on the `compactionExecutionCandidate` (or on whichever helper builds one) — this is a mechanical shape change across several `_test.go` files found in Step 0 grep.

**Step 4 — GREEN**, then:
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime
```

**Step 5 — commit:**
```bash
git add internal/loopruntime/compaction_executor.go internal/loopruntime/loop.go internal/loopruntime/compaction_hooks_test.go internal/loopruntime/safe_boundary_compaction_test.go
git commit -m "refactor(loopruntime): thread InferenceCapability per compaction candidate instead of freezing it on the executor"
```

### Phase 2 checkpoint
```bash
GOFLAGS=-mod=vendor go test -race ./...
make secure
```

---

# Phase 3 — Event schema and restore

## Task 3.1: Extend `event.ModelRuntime` additively with `APIFormat`/`BaseURL`

**Files:**
- Modify: `pkg/event/turn.go` (`ModelRuntime`, 8-15)
- Modify: `pkg/event/quality_validation_test.go` or `pkg/event/usage_runtime_test.go` (existing `ModelRuntime` validation/round-trip tables — locate with `grep -n "ModelRuntime{" pkg/event/usage_runtime_test.go`)

**Step 1 — failing test.** Extend the existing `ModelRuntime` round-trip/JSON test table: a `ModelRuntime` with `APIFormat`/`BaseURL` set encodes and decodes byte-for-byte (round-trip); a `ModelRuntime` with both left zero encodes WITHOUT the new keys present at all (proves `omitzero` — assert on the raw JSON, not just the decoded struct, to actually exercise additivity for old-journal compatibility); `validateModelRuntime` still accepts a `ModelRuntime` with the new fields populated arbitrarily (no new required-field constraint).

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./pkg/event -run 'TestModelRuntime'
```
Expected: compile failure (fields don't exist) or the empty-JSON-omission assertion fails.

**Step 3 — implement.**
```go
type ModelRuntime struct {
    Key       model.ModelKey      `json:"key"`
    Limits    model.ContextLimits `json:"limits"`
    Effort    model.Effort        `json:"effort,omitzero"`
    APIFormat model.APIFormat     `json:"api_format,omitzero"`
    BaseURL   string              `json:"base_url,omitzero"`
}
```
No change needed to `validateModelRuntime` (`pkg/event/validate.go:788-799`) — the two new fields are unconstrained by design (see design doc). Add a doc-comment note on `ModelRuntime` recording that zero/absent means "use the definition's declared base transport."

**Step 4 — GREEN**, then:
```bash
GOFLAGS=-mod=vendor go test -race ./pkg/event
```

**Step 5 — commit:**
```bash
git add pkg/event/turn.go pkg/event/<test file>
git commit -m "feat(event): extend ModelRuntime additively with APIFormat and BaseURL"
```

## Task 3.2: Graft the new fields at both restore-fold sites

**Files:**
- Modify: `internal/loopruntime/restored.go` (`NewRestoredWithRuntime`, graft at 134-140)
- Modify: `internal/sessionruntime/loop_change.go` (`applyModelRuntime`, 131-138)
- Modify: `internal/loopruntime/restore_runtime_test.go` (or nearest existing restore-runtime-fold test)
- Modify: `internal/sessionruntime/loop_change_test.go`

**Step 1 — failing test.** In `internal/loopruntime`: a restore seeded with `RestoredState{Runtime: event.ModelRuntime{..., APIFormat: "openai", BaseURL: "https://example"}, HasRuntime: true}` against a bound definition whose base mode model has a DIFFERENT `APIFormat`/`BaseURL` produces a restored loop whose `cfg.Model` carries the SEEDED `APIFormat`/`BaseURL`, not the base mode's. In `internal/sessionruntime`: `applyModelRuntime` applied to a base model with a runtime carrying non-empty `APIFormat`/`BaseURL` overrides both; applied with both left zero leaves the base model's values untouched (regression, must already pass).

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime -run <test name>
GOFLAGS=-mod=vendor go test -race ./internal/sessionruntime -run <test name>
```
Expected: assertion failures — the graft currently ignores the new fields entirely (they don't exist on the struct before Task 3.1, and even after, both functions don't read them yet).

**Step 3 — implement.** In both `restored.go:134-140` and `loop_change.go:131-138`, add before the existing `Provider`/`Name` assignments:
```go
if seed.Runtime.APIFormat != "" {
    cfg.Model.APIFormat = seed.Runtime.APIFormat // restored.go naming; applyModelRuntime uses `model.APIFormat = runtime.APIFormat`
}
if seed.Runtime.BaseURL != "" {
    cfg.Model.BaseURL = seed.Runtime.BaseURL
}
```
(adapt variable names to each function's existing local names — `cfg.Model`/`seed.Runtime` in `restored.go`, `model`/`runtime` in `applyModelRuntime`).

**Step 4 — GREEN**, then:
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime ./internal/sessionruntime
```

**Step 5 — commit:**
```bash
git add internal/loopruntime/restored.go internal/sessionruntime/loop_change.go internal/loopruntime/<test file> internal/sessionruntime/<test file>
git commit -m "feat(restore): graft ModelRuntime.APIFormat/BaseURL at both restore-fold sites"
```

## Task 3.3: Restore-time transport-membership hard failure (`RestoreTransportMismatchError`)

**Files:**
- Modify: `internal/loopruntime/restored.go` (`NewRestoredWithRuntime`, after the graft)
- Create: `internal/loopruntime/restore_transport_error.go` (or add to an existing errors file in the package — check `grep -rln "type.*Error struct" internal/loopruntime/*.go | grep -v _test` for the package's existing error-file convention first)
- Modify: `internal/loopruntime/restore_runtime_test.go`
- Modify: `internal/sessionruntime/restore_constructor_test.go` (or nearest restore-integration test — proves the error surfaces as `RestoreLoopFailed`)

**Step 1 — failing test.** In `internal/loopruntime`: restoring with a folded `ModelRuntime` whose `(Provider, APIFormat, BaseURL)` is NOT a member of the bound definition's declared transport set returns a typed `*RestoreTransportMismatchError` and constructs no loop. A folded selection that IS a member (including the synthesized single-member default case) restores successfully — regression. In `internal/sessionruntime`: an end-to-end restore whose one loop hits this condition fails the whole `RestoreTopology` call with `*RestoreError{Kind: RestoreLoopFailed}` wrapping the cause, and — asserted explicitly to prove non-composition with the unrelated mechanism — succeeds/fails identically regardless of whether `WithAllowConfigMismatch`/`WithRestoreDecider` is configured (add both a with-decider and without-decider variant of the failing case).

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime -run TestRestoreTransportMismatch
GOFLAGS=-mod=vendor go test -race ./internal/sessionruntime -run TestRestoreTransportMismatch
```
Expected: compile failure (`RestoreTransportMismatchError` undefined), then assertion failure once it exists but isn't checked.

**Step 3 — implement.**
```go
// RestoreTransportMismatchError reports a durably-folded model runtime whose
// transport is no longer a member of the current bound definition's declared
// ContextTransport set. Restore fails unconditionally on this error — there
// is no coherent "resume anyway" answer for a resolved model whose trust
// tier can no longer be determined, so it does not route through
// RestoreDecider/WithAllowConfigMismatch (see design doc).
type RestoreTransportMismatchError struct {
    Provider  model.ProviderName
    APIFormat model.APIFormat
    BaseURL   string
}

func (e *RestoreTransportMismatchError) Error() string {
    return "loopruntime: restored model transport is not a declared ContextTransport"
}
```
In `NewRestoredWithRuntime`, immediately after the graft (post line 140) and before `installRuntimeDependencies`:
```go
if seed.HasRuntime && bound.ContextCounter() != nil {
    if _, ok := bound.ContextTransportCapability(cfg.Model); !ok {
        return nil, &RestoreTransportMismatchError{Provider: cfg.Model.Provider, APIFormat: cfg.Model.APIFormat, BaseURL: cfg.Model.BaseURL}
    }
}
```
No change needed in `attachRestoredLoop` (`restore_constructor.go:87-90`) — it already wraps any `NewRestoredWithRuntime` error as `&RestoreError{Kind: RestoreLoopFailed, Cause: err}` unconditionally, which is exactly the desired composition (confirm with the sessionruntime-level test, do not add a parallel check there).

**Step 4 — GREEN**, then:
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime ./internal/sessionruntime
```

**Step 5 — commit:**
```bash
git add internal/loopruntime/restore_transport_error.go internal/loopruntime/restored.go internal/loopruntime/<test file> internal/sessionruntime/<test file>
git commit -m "feat(restore): fail restore hard when a folded model's transport is no longer declared"
```

### Phase 3 checkpoint
```bash
GOFLAGS=-mod=vendor go test -race ./...
make secure
```

---

# Phase 4 — Cross-cutting regression and documentation

## Task 4.1: End-to-end cross-provider live-switch integration test

**Files:**
- Create: `internal/loopruntime/context_transport_switch_integration_test.go` (or extend `context_loop_test.go` if the package convention keeps everything in one file — check first)

**Step 1 — failing test.** One scenario test covering the full path this feature exists for: build a definition with two declared transports (local-shaped + remote-TLS-shaped, distinct `InferenceCapability`), start a turn on the base transport, run an automatic-compaction-triggering conversation up to the point a summary exists, live-switch via `ChangeLoopInference` to the second transport, run another turn, and assert: (a) the switch is accepted; (b) the pre-switch compaction summary is present in the next request's messages (conversation preserved, per the settled design decision); (c) the post-switch request's measurement/fingerprint reflects the NEW `InferenceCapability`; (d) a subsequent switch attempt to an undeclared third transport is refused and the loop's effective model/capability are unchanged.

**Step 2 — RED:**
```bash
GOFLAGS=-mod=vendor go test -race ./internal/loopruntime -run TestCrossProviderModelSwitch_Integration
```

**Step 3 — implement.** No new production code expected — this task is a pure regression/acceptance test over Phases 1-3's combined behavior. If it reveals a gap, fix it in the relevant Phase 1-3 file and note the correction in the commit message.

**Step 4 — GREEN**, then full suite:
```bash
GOFLAGS=-mod=vendor go test -race ./...
```

**Step 5 — commit:**
```bash
git add internal/loopruntime/context_transport_switch_integration_test.go
git commit -m "test(loopruntime): add end-to-end cross-provider live model switch regression"
```

## Task 4.2: Final verification gate

**Steps (no commit unless a fix is needed):**
```bash
GOFLAGS=-mod=vendor go test -race ./...
make secure
GOWORK=off go build ./...
gofmt -l $(go list -f '{{.Dir}}' ./...)
```
All must pass clean. If `make secure` or `gofmt` flags anything, fix it as a small follow-up commit scoped to exactly the flagged file(s):
```bash
git add <fixed files>
git commit -m "fix(loop): satisfy gofmt/staticcheck/gosec for cross-provider model switching"
```

---

## Critical Files for Implementation

- `pkg/loop/context_transport.go` (new)
- `pkg/loop/definition.go`
- `internal/loopruntime/loop.go`
- `internal/loopruntime/compaction_executor.go`
- `internal/loopruntime/restored.go`
- `pkg/event/turn.go`
- `internal/sessionruntime/loop_change.go`
