# Hustle, Token Usage, and Conversation Compaction Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Every implementation subagent must use superpowers:test-driven-development. After each task, run a separate spec-compliance review and then a separate code-quality review before continuing.

**Goal:** Ship normalized token usage, model-aware context measurement, bounded one-shot hustles, crash-consistent conversation compaction, focused-loop CLI presentation, and production SWE wiring across the Looprig modules.

**Architecture:** Work proceeds in dependency order: `core` owns normalized usage; `inference` owns model limits, streaming trailers, counter contracts, and the deterministic estimator; `llm` preserves provider usage and adds supported exact counters; `harness` owns hustle execution and compaction state; `cli` owns the focused-loop command and presentation reducers; `swe` owns the product prompt, calibration, inference posture, and composition. Feature development uses adjacent worktrees and existing sibling `replace ../...` directives; planned version pins are reconciled before merge, while tag/push decisions are deferred to branch finishing.

**Tech Stack:** Go 1.26.4, standard library only for new implementation (`encoding/json`, `encoding/xml`, `crypto/sha256`, checked integer arithmetic), existing Looprig modules and provider transports; no new external dependency.

---

## Workspaces and global rules

```text
ROOT=/Users/ipotter/code/looprig/harness/.worktrees/hustle-token-compaction
CORE=$ROOT/core
INFERENCE=$ROOT/inference
LLM=$ROOT/llm
HARNESS=$ROOT/harness
CLI=$ROOT/cli
SWE=$ROOT/swe
```

- All worktrees start from clean `main` baselines. Fresh `go test -race ./...`
  passed in all six modules; inference/LLM/harness tests that open `httptest`
  listeners require local socket permission.
- Follow each repository's `CLAUDE.md`: table-driven tests, typed errors, no
  `any` beyond explicit serialization boundaries, no new dependency, and
  `CGO_ENABLED=0 go build -trimpath` before completion.
- TDD is mandatory: write one failing test, run and inspect the expected RED,
  write the minimum implementation, run GREEN, then refactor without adding
  behavior.
- Every Go or Make invocation must set a worktree-specific writable cache in the
  same command, for example
  `GOCACHE=/private/tmp/hustle-token-compaction-core-gocache go test ...`.
  Commands below that omit the prefix for readability are shorthand for this
  mandatory form. Listener-based tests additionally use the approved local
  socket escalation.
- One implementation subagent works at a time. After its self-review and commit,
  dispatch a fresh spec reviewer; only after approval dispatch a fresh quality
  reviewer. The same implementer fixes findings and each reviewer re-reviews.
- Do not tag or push releases during implementation. Planned pins are
  `core v0.2.0`, `inference v0.2.0`, `llm v0.2.0`, `harness v0.11.0`, and
  `cli v0.6.0`; local replaces make them build before tags exist. Final release
  actions require the finishing workflow and user choice.

## Phase A — Core and inference foundations

### Task 1: Core normalized usage arithmetic

**Repository:** `$CORE`

**Files:**

- Create: `content/usage.go`
- Create: `content/usage_test.go`

**RED:** Add table-driven cases for zero, cache-only, present-zero, reasoning
boundary, invalid reasoning, every overflow site, and additive identity.

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/core-hustle-gocache \
  go test -race ./content -run 'TestUsage|TestTokenCount'
```

Expected: compile failure because `TokenCount`, `Usage`, and typed usage errors
do not exist.

**GREEN:** Implement `TokenCount uint64`; the five-field `Usage`; checked
`ContextTokens`, `TotalTokens`, and `Add`; `Validate`; and typed
`UsageValidationError`/`UsageOverflowError`. `ReasoningTokens` is a subset of
`OutputTokens`; context includes uncached, cache-read, and cache-creation input.

Run the focused test, then:

```bash
GOWORK=off GOCACHE=/private/tmp/core-hustle-gocache go test -race ./content
```

Commit: `feat(content): add normalized token usage`.

### Task 2: Core usage-bearing AI message codec

**Repository:** `$CORE`

**Files:**

- Modify: `content/message.go`
- Modify: `content/message_json_test.go`
- Modify: `content/message_test.go`

**RED:** Prove that adding only `AIMessage.Usage` would be dropped by promoted
`Message.MarshalJSON`. Cover nil versus present-zero usage, full usage, tagged
blocks, reused-destination clearing, and marshal/unmarshal fixed point.

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/core-hustle-gocache \
  go test -race ./content -run 'TestAIMessage|TestMessageJSONRoundTrip'
```

**GREEN:** Add `Usage *Usage` plus explicit `AIMessage.MarshalJSON` and
`UnmarshalJSON`, reusing block codecs and allocating fresh decoded state.

Verify:

```bash
GOWORK=off GOCACHE=/private/tmp/core-hustle-gocache go test -race ./...
GOWORK=off GOCACHE=/private/tmp/core-hustle-gocache make check
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/core-hustle-gocache go build -trimpath ./...
```

Commit: `feat(content): persist AI message usage`.

### Task 3: Inference core pin, model keys, and canonical context limits

**Repository:** `$INFERENCE`

**Files:**

- Modify: `go.mod`, `go.sum`
- Modify: `model.go`, `model_test.go`, `capabilities.go`
- Modify: every file returned by `rg -l 'WithMaxContext|MaxContext' --glob '*.go'`
- Create: `modelkey.go`, `modelkey_test.go`
- Create: `contextlimits.go`, `contextlimits_test.go`

Set the planned core requirement to `v0.2.0` while retaining `replace ../core`.

**RED:** Table-test `ModelKey`, `Model.Key`, zero/known `ContextLimits`, invalid
relationships, and defensive model cloning.

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/inference-hustle-gocache \
  go test -race . -run 'TestModelKey|TestContextLimits|TestModel'
```

**GREEN:** Add canonical `Model.Limits`; use the existing typed `ProviderName`
for `ModelKey.Provider`; migrate and remove
`Capabilities.MaxContext`/`WithMaxContext`; add `WithContextLimits`. Key identity
uses provider namespace plus provider model id only. Zero limit fields remain
explicitly unknown.

Commit: `feat(inference): add model keys and context limits`.

### Task 4: Inference counter and transport capability contracts

**Repository:** `$INFERENCE`

**Files:**

- Create: `contextcounter.go`, `contextcounter_test.go`
- Modify: `errors.go`, `errors_test.go`

**RED:** Cover every enum, structural validation, nil function adapter, local
provider-neutral compatibility, same-endpoint identity, separate-endpoint
downgrade rejection, retention ordering, and unknown fail-closed behavior.

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/inference-hustle-gocache \
  go test -race . -run 'TestContextCounter|TestCompatibleCounter|TestCapability'
```

**GREEN:** Implement the approved typed contracts, including
`CounterCapability.Quality`, and typed `ContextCountError`,
`CapabilityValidationError`, and `CounterCompatibilityError`. Compatibility is
an exhaustive switch, not ordinal transport comparison.

Commit: `feat(inference): define context counter capabilities`.

### Task 5: Deterministic complete-request estimator

**Repository:** `$INFERENCE`

**Files:**

- Create: `contextcount/estimator.go`
- Create: `contextcount/estimator_test.go`
- Create: `contextcount/errors.go`

**RED:** Pin deterministic golden counts for OpenAI, Anthropic, and Gemini
requests. Prove system, messages, tools, images, and sampling affect the result;
historical `AIMessage.Usage` does not; unsupported formats fail typed.

Run: `GOWORK=off go test -race ./contextcount`.

**GREEN:** Encode the complete request with the existing codec, estimate
`ceil(encodedBytes/4)`, return heuristic quality and `req.Model.Key()`, and expose
a local/no-retention/provider-neutral capability with a named estimator revision.

Commit: `feat(inference): add deterministic request estimator`.

### Task 6: Non-streaming usage normalization

**Repository:** `$INFERENCE`

**Files:**

- Modify: `client.go`, `errors.go`
- Create: `usage.go`, `usage_test.go`
- Modify: `codec/{openaiapi,anthropicapi,geminiapi}/{types.go,decode.go,decode_test.go,encode_test.go}`

**RED:** Test negative wire values, impossible cached totals, cache reads/writes,
reasoning subsets, present-zero usage, response/message defensive copies, and
request encoders excluding historical usage. Explicit JSON `null`, fractional,
and out-of-range count scalars fail typed; an optional Gemini total is
presence-aware and must equal the checked component sum when reported.

Run:

```bash
GOWORK=off go test -race ./codec/openaiapi ./codec/anthropicapi ./codec/geminiapi \
  -run 'Test.*Usage|Test.*IgnoresUsage|TestDecodeResponse'
```

**GREEN:** Alias `inference.Usage` to `content.Usage`; normalize disjoint provider
fields with checked conversions in an internal helper; preserve exact typed core
validation causes without mislabeling future invariants; attach cloned usage to
both `Response.Usage` and `Response.Message.Usage`.

Commit: `feat(inference): normalize provider token usage`.

### Task 7: Generic streaming terminal result

**Repository:** `$INFERENCE`

**Files:**

- Modify: `stream.go`, `stream_test.go`, `chunkstream.go`, `chunkstream_test.go`
- Create: `streamresult.go`, `finishreason.go`

**RED:** Result unavailable before EOF and after non-EOF error; available once
after clean EOF; usage defensively copied; close idempotent; frame-to-chunk
adapter preserves terminal metadata and pending chunks.

Run:

```bash
GOWORK=off go test -race . -run 'TestStreamReader.*Result|TestFramesToChunks.*Result'
```

**GREEN:** Add `StreamResult` and a result-aware reader/adapter seam without
breaking simple existing framers. `Close` never manufactures authority.

Commit: `feat(inference): expose streaming result trailers`.

### Task 8: Provider streaming usage collectors

**Repository:** `$INFERENCE`

**Files:**

- Modify: OpenAI, Anthropic, and Gemini `types.go`, `stream.go`, and stream tests
- Modify: `codec/openaiapi/encode.go`, `encode_test.go`

**RED:** Test OpenAI `include_usage`, usage-only frames, Anthropic start/delta
combination, Gemini latest cumulative metadata, finish reason/model, missing
trailers, and malformed/negative terminal usage.

Run:

```bash
GOWORK=off go test -race ./codec/openaiapi ./codec/anthropicapi ./codec/geminiapi \
  -run 'Test.*Stream.*Result|Test.*IncludeUsage'
```

**GREEN:** Collect terminal metadata without emitting content chunks for
usage-only frames. A clean stream may have a result with nil usage; terminal
decode errors make `Result` unavailable.

Verify full inference race tests, secure gate, and trimpath build. Commit:
`feat(inference): collect provider stream trailers`.

## Phase B — LLM provider integration

### Task 9: LLM foundation pins and vendor establishment

**Repository:** `$LLM`

**Files:** `go.mod`, `go.sum`, new `vendor/` tree.

Set planned core/inference requirements to `v0.2.0`, retain sibling replaces,
update every LLM occurrence returned by
`rg -l 'WithMaxContext|MaxContext' --glob '*.go'` to canonical
`ContextLimits`/`WithContextLimits`, run `go mod tidy`, then `go mod vendor`.
Verify `vendor/modules.txt` names both planned versions. This module's Makefile
forces `-mod=vendor`; absence or drift is a failure.

Run: `GOWORK=off make test`.

Commit: `chore(deps): pin token foundation modules`.

### Task 10: Exact Gemini context counter

**Repository:** `$LLM`

**Files:**

- Create: `providers/gemini/counter.go`, `counter_test.go`
- Modify shared HTTP/auth helpers in `providers/gemini/client.go` as required

**RED:** Cover exact request body, escaped count route, auth, model mismatch,
non-negative response, malformed/negative response, non-2xx, cancellation, and
capability metadata.

Run: `GOWORK=off go test -race ./providers/gemini -run 'TestCounter'`.

**GREEN:** A separately constructed counter calls Gemini `countTokens`, returns
exact-provider quality, and never appears through an optional client assertion.

Commit: `feat(gemini): add exact context counter`.

### Task 11: Exact Bedrock context counter

**Repository:** `$LLM`

**Files:**

- Create: `providers/bedrock/counter.go`, `counter_test.go`
- Modify: `providers/bedrock/client.go` shared request/signing helpers
- Reuse: `providers/bedrock/body.go`

**RED:** Prove body equivalence with invoke, count route/SigV4, non-negative
response, provider mismatch, unsupported-model typed API errors, cancellation,
and capability metadata.

Run: `GOWORK=off go test -race ./providers/bedrock -run 'TestCounter'`.

**GREEN:** Implement the separate exact counter without silent estimator fallback.

Commit: `feat(bedrock): add exact context counter`.

### Task 12.1: Explicit LLM counter support

**Repository:** `$LLM`

**Files:**

- Modify: `errors.go`, `errors_test.go`
- Create: `auto/counter.go`, `auto/counter_test.go`

**RED:** Exhaust every known provider through `auto.NewCounter`; unsupported
gateways return typed errors and no estimator is silently substituted.

**GREEN:** Wire only supported exact counters and explicit typed unsupported
results. Commit: `feat(llm): make context counter support explicit`.

### Task 12.2: Preserve normalized usage through verified/provider wrappers

**Repository:** `$LLM`

**Files:** modify/test `aci/client.go`, Chutes, Gemini, and Bedrock provider
wrappers.

**RED:** Independently prove ACI verified replay, Chutes SSE, Gemini, and Bedrock
preserve normalized terminal usage and never expose it before verification or
after error.

**GREEN:** Make verified replay readers carry `StreamResult` and preserve the
codec result through each wrapper. Commit: `feat(llm): preserve normalized provider usage`.

### Task 12.3: LLM full verification and vendor reconciliation

**Repository:** `$LLM`

Run full LLM race tests, `go mod vendor`, secure gate, and trimpath build. Commit
vendor reconciliation as `chore(vendor): refresh token foundation dependencies`
when non-empty.

## Phase C — Harness foundation and hustle runtime

### Task 12A: Reconcile merged harness formatting baseline

**Repository:** `$HARNESS`

The clean main baseline passes the full race suite but its mandatory secure gate
reports pre-existing `gofmt` drift in merged files under `pkg/command`,
`pkg/event`, `pkg/hub`, and `pkg/tools`. Before feature edits overlap those files,
run `make fmt`, inspect that the diff is formatting-only, run the full race suite
and trimpath build, then commit `style: format merged harness sources`. This is a
mechanical baseline reconciliation, not a behavior change; no production
behavior is added without a RED test.

### Task 13: Harness dependency pins and usage/event foundation

**Repository:** `$HARNESS`

**Files:**

- Modify: `go.mod`, `go.sum`
- Modify: `pkg/event/event.go`, `validate.go`, `marshal.go`, relevant tests
- Modify: rig-lifecycle model events to use the single
  `event.ModelRuntime{Key, Limits, Effort}` payload
- Modify: `LoopStarted`, `LoopInferenceChanged`, and `LoopModeChanged` codecs,
  validation, runtime emission, replay, and catalog tests
- Modify: `pkg/event/turn.go`, `internal/loopruntime/step.go`, stream tests
- Modify: `pkg/sessionstore/catalog.go` and loop metadata codec/repair tests
- Modify: every harness occurrence returned by
  `rg -l 'WithMaxContext|MaxContext' --glob '*.go'`

Set planned core and inference requirements to `v0.2.0` while retaining sibling
replaces. Harness must not import or require `llm`.

**RED:** Usage survives `StepDone` and `TurnDone` durable round trips; stream
terminal usage attaches to the one finalized AI message; turn usage is checked;
per-loop cumulative usage adds steps exactly once. Also cover `ModelRuntime`
codec fidelity/validation, all three lifecycle events carrying key/limits/effort,
runtime emission after model/mode changes, replay, and catalog repair.

**GREEN:** Add `event.ModelRuntime`, migrate the three rig-lifecycle events and
their folds, implement cumulative `LoopUsageMeta`, and add the usage stitch
without any hustle behavior. Commit: `feat(harness): account for loop token usage`.

### Task 14: Immutable text-only hustle definitions

**Repository:** `$HARNESS`

**Files:** create `pkg/hustle/{definition.go,definition_errors.go,run.go,definition_test.go,deps_test.go}`.

Use the detailed contracts in the approved hustle design. RED covers
named/current-loop definitions, duplicate options, revisions, timeouts, byte
limits, defensive copies, secret-free descriptors, and dependency boundaries.
Do not add output schema support.

Run: `GOWORK=off go test -race ./pkg/hustle`.

Commit: `feat(hustle): define immutable inference work`.

### Task 15: Rig hustle registration and lifecycle storage seam

**Repository:** `$HARNESS`

**Files:**

- Modify: `pkg/rig/{definition.go,options.go,fingerprint.go,errors.go}` and tests
- Modify: `internal/sessionruntime/lifecycle.go`, composition tests

**RED:** Additive registration, duplicate names, singleton lane limits,
deterministic sorting, fingerprint sensitivity, and secret exclusion. Create the
session lifecycle storage seam in this same task so rig never calls a nonexistent
future option and sessionruntime never imports `pkg/rig`.

Commit: `feat(rig): register and fingerprint hustles`.

### Task 16: Event visibility, hustle lifecycle, and replay privacy

**Repository:** `$HARNESS`

**Files:**

- Modify: `pkg/event/{event.go,marshal.go,validate.go,filter.go}` and tests
- Create: `pkg/event/hustle_test.go`, `hustle_fuzz_test.go`
- Modify: `pkg/sessionstore/replay.go` and tests
- Modify: `pkg/serve/catalogreader/reader.go` and tests

**RED:** Internal lifecycle round trip, public filter denial, bounded stages and
reasons, session scope, visibility legacy zero, defensive usage, and public
replay/serve exclusion of internal events.

**GREEN:** Keep a separate unfiltered restore seam and default-deny public replay.

Commit: `feat(event): add private hustle lifecycle`.

### Task 17: Hub private audit and activity adapter

**Repository:** `$HARNESS`

**Files:**

- Modify: `pkg/hub/{hub.go,state.go,errors.go}` and tests
- Create: `internal/sessionruntime/hustle_activity.go` and tests

**RED:** Checked internal publication allowlist, ordinary publication denial,
wrong session/class/type rejection, no subscriber delivery, explicit blocking
activity edges, partial lease cleanup, and return-type adapter compatibility.

Commit: `feat(hub): own private hustle audit and activity`.

### Task 18: Bounded hustle lanes and admission

**Repository:** `$HARNESS`

**Files:** create `internal/hustleruntime/{controller.go,lane.go,contracts.go,errors.go,controller_test.go,lane_test.go,deps_test.go}`.

**RED:** FIFO ownership, separate blocking/background capacity, full/closed
rejection, queued cancellation, zero limits, one finalizer per owned run, no
sleeps, and import boundaries.

Commit: `feat(hustleruntime): add bounded ownership lanes`.

### Task 19: Hustle inference, audit, and panic-safe finalization

**Repository:** `$HARNESS`

**Files:** create/modify `internal/hustleruntime/{execution.go,audit.go,controller.go}` and tests.

**RED:** One `Invoke`, nil tools, exact prompt/data request, model resolution,
queue versus execution timeout, output bounds, validation before completed
audit, failure reason mapping, audit-before-finalizer, callback panics, ignored
cancellation, late results, and drain ownership.

Commit: `feat(hustleruntime): execute and audit inference work`.

### Task 20.1: Session hustle binding and construction

**Repository:** `$HARNESS`

**Files:**

- Create: `internal/sessionruntime/hustle.go`, tests
- Modify: `internal/sessionruntime/{session.go,lifecycle.go}` and construction tests
- Modify: `pkg/session/session.go`, `contracts_test.go` only as needed for narrow
  internal composition; do not expose a generic runner

**RED:** Bind once before reachability, current-loop resolution follows allowed
model changes, missing/exited loop, transactional construction abort, and no
public generic runner.

Commit: `feat(session): bind hustle runtime`.

### Task 20.2: Hustle shutdown and drain ordering

**Repository:** `$HARNESS`

**Files:** modify `internal/sessionruntime/{session.go,drain.go,lifecycle.go}` and
shutdown/fault tests.

**RED:** Close admission, cancel queue/execution, keep hub/session context alive,
drain audit/finalizers, then stop hub/release leases. Caller cancellation stops
queue/inference work but cleanup remains joined and may outlive that deadline;
internal audit/finalization deadlines bound trusted cleanup. No owned worker is
detached or abandoned.

Commit: `feat(session): drain hustle lifecycle safely`.

### Task 20.3: Hustle restore and bounded aggregate

**Repository:** `$HARNESS`

**Files:** modify `internal/sessionruntime/restore.go`,
`pkg/sessionstore/{catalog.go,sessionstore.go}` and tests.

**RED:** Unmatched starts become interrupted audit state; aggregate uses terminal
events only, is bounded/deterministic, and public replay excludes lifecycle audit.

Commit: `feat(sessionstore): fold bounded hustle usage`.

## Phase D — Harness context measurement and compaction

### Task 21: Context basis, measurement, limits, and loop definition policy

**Repository:** `$HARNESS`

**Files:**

- Add approved types/events in `pkg/event`
- Create: `pkg/loop/context.go`, `context_test.go`, `compaction_policy.go`, tests
- Modify: `pkg/loop/definition.go`, fingerprints/tests
- Modify: `pkg/rig/definition.go`, `errors.go`, and tests for cross-definition
  validation after loops and hustles are frozen
- Modify: `internal/loopruntime/config.go`, state/fold tests
- Modify: `pkg/sessionstore/catalog.go` and tests for latest runtime/context

**RED:** checked limit resolution, explicit unknowns, basis points boundaries,
counter/capability validation, fixed provider/API/base URL across live changes,
registered compatible hustle validation at `rig.Define`, fingerprint sensitivity,
request fingerprint inputs, catalog projection, positive arbitrary count
timeouts, exact duration preservation, and zero/negative rejection. Harness has
no two-second default; SWE pins `2s` in Task 35.

Commit: `feat(loop): define context measurement policy`.

### Task 22: Compact command, events, codecs, and public transport

**Repository:** `$HARNESS`

**Files:**

- Create: `pkg/command/compact.go`, tests; modify command classifier/codec tests
- Add compaction events/typed errors to `pkg/event` codecs/validation/tests
- Modify: `pkg/serve/ephemeral.go`, DTO/schema tests
- Modify: `internal/loopruntime/header.go`, `header_test.go`, `loop.go`, and
  checked-public publication tests

**RED:** agency validation, coordinates, stamped low-volume ephemeral start,
enduring terminal fidelity/duration, ephemeral persistence rejection, serve
delivery, waiter deterministic IDs, checked stamped publication, publication
failure preventing inference, and malformed input fuzzing.

The public/durable reason fields use named `uint8` enums encoded as ordinary JSON
numbers. Zero and unknown values are invalid. `CompactionReason` is exactly
`Unspecified(0)`, `Manual(1)`, and `Automatic(2)`. `CompactRejectReason` is
exactly `Unspecified(0)`, `ControlLaneFull(1)`, `ShuttingDown(2)`,
`Interrupted(3)`, `Canceled(4)`, `StaleBasis(5)`,
`ProgressPublication(6)`, `Unavailable(7)`, `ExecutionFailed(8)`,
`InvalidSummary(9)`, `ContextCountFailed(10)`, `SummaryTooLarge(11)`, and
`Internal(12)`, and `ContextLimitUnknown(13)`. Use the canonical mapping
documented in the token design §8. `ProgressPublication` applies to start
construction/validation/checked-publication/EventID-stamp failure only when a
valid durable rejection remains constructible and appendable. `Internal` is
only a recoverable unclassified failure after a valid AttemptID exists and a
valid terminal remains constructible/journalable. AttemptID or durable-terminal
EventID mint failure, structurally impossible canonical terminals, and fatal
hub/session/persistence failures fault/stop and complete in-process waiters with
typed infrastructure errors; none is journaled as a false rejection.

Both canonical terminal events durably carry the attempt identity:
`CompactionCommitted` and `CompactionRejected` require a non-zero valid
`CompactionReason` plus the exact valid attempted `ContextBasis` (committed keeps
its existing basis; rejected gains one). Codecs and validators reject zero or
unknown reasons and invalid bases. Immediate pre-start/control-lane-full
`CompactWaiterRejected` remains per-command after a valid coordination AttemptID
exists, lane-full cites the existing pending AttemptID, and neither fabricates a
canonical basis. Lane-full overflow rejects only the joining command and leaves
the existing owning slot plus accepted waiters untouched. Failure before any
AttemptID is minted emits no durable waiter reply and returns only the typed
infrastructure failure.

Commit: `feat(compaction): add commands and event contracts`.

### Task 23: Control-lane coalescing and waiter outcomes

**Repository:** `$HARNESS`

**Files:** create/modify `internal/loopruntime/compaction_control.go`, loop/runner contracts and tests.

**RED:** safe boundary consumption, one attempt/many canonical waiters,
dedup/order, lane-full immediate reply using the existing pending AttemptID, user
join with overflow leaving the owning slot and accepted waiters untouched,
machine coalescing, immutable first-opener reason across mixed-origin joins,
interrupt/shutdown priority, transient pending-slot suppression without durable
exhaustion, pre-start interrupt/shutdown clearing the slot without a canonical
automatic rejection, and journal-writable versus fatal-infrastructure outcomes.

Commit: `feat(loopruntime): coordinate compaction attempts`.

### Task 24: Counting, pressure, rearm, and hard admission

**Repository:** `$HARNESS`

**Files:** create `internal/loopruntime/context.go`, `context_test.go`; add the
public `pkg/loop.ContextObservationPolicy`, `WithContextObservation`, and
`ContextLimitError`; modify definition/fingerprint, step/turn/model-change paths,
and tests.

**RED:** complete candidate request counting before inference; changed-only
`ContextMeasured`; pressure state changes; 80/60 policy supplied by consumer;
one auto-attempt per basis; manual bypass; smaller-model recount; hard limit
blocks; timeout and cancellation. A counter must have exactly one explicit
observation or compaction policy. Observation validates a non-zero reservation,
positive exactly-preserved timeout, and heuristic margin; it is mutually exclusive
with compaction, fingerprinted, emits only Normal/HardLimit pressure transitions,
and never schedules compaction. Pre-primary count failure never reuses an older
measurement and ends the turn with typed `inference.ContextCountError`; unknown
limit uses `ContextLimitUnknownError`; measured hard admission uses
`ContextLimitError`. Measurement/pressure publication precedes policy; an eligible
automatic basis schedules/coalesces one real machine `Compact` and pauses primary
inference before hard-limit failure, including at/above the limit. A narrow await
disposition seam may land now; Task 26 owns execution/final replacement. No
`ContextLimitError` is emitted while that attempt is pending/in progress. None
fabricates a compaction attempt or rejection. Pin the public observation fields
`ReservedOutput`, `SafetyMargin`, and `CountTimeout`; typed
`ContextObservationPolicyError{Field}`; definition kinds
`DefinitionInvalidContextObservation`, `DefinitionConflictingContextPolicy`, and
`DefinitionMissingContextPolicy`; existing missing-counter and duplicate-option
kinds remain canonical. Soft compaction failure below a known hard limit and
compact-reject count/unknown mappings remain Task 26 behavior once a real attempt
exists. Restore RED additionally pins basis independent from measurement through
TurnStarted/StepDone/TurnFoldedInto/model/mode invalidation, stale in-flight count
CAS across mode/inference request-shape changes, and the bounded durable automatic
latch: restored automatic rejection suppresses only the unchanged basis, manual
rejection does not, and a later basis mutation re-enables automatic admission.
Live and restore mixed-origin RED pins opener semantics: a machine join of a
manual-opened attempt does not consume the latch and may open one later automatic
attempt at the same basis, while a manual join of an automatic-opened attempt
leaves its canonical reason Automatic and consumes the latch.
Task24's await/result seam marks exhausted `automaticBasis` only after receiving
the identity of a successfully appended canonical Automatic rejection. Mere
admission/open/start and per-waiter-only pre-start interrupt/shutdown/control
owner rejection use/clear only Task23's transient slot; a lane-full overflow
joiner is rejected without clearing that slot. RED covers Automatic-opened
pre-start Interrupt and Shutdown both live and after equivalent restore: the
unchanged basis remains eligible. Fatal no-terminal infrastructure creates no
durable latch; committed replacement advances basis instead.

Commit: `feat(loopruntime): measure and admit request context`.

### Task 25: Typed compaction adapter and strict XML validation

**Repository:** `$HARNESS`

**Files:**

- Create: `pkg/loop/compaction.go`, tests
- Create: `internal/sessionruntime/compaction_adapter.go`, tests
- Create: `internal/loopruntime/compaction.go`, tests
- Modify: `internal/hustleruntime/errors.go`, `execution.go`, and focused tests

**RED:** Define public `CompactionInput`/`CompactionOutput`, typed input fields and
errors, `CompactionWireVersion` with only v1, and the closed
`InvalidSummaryReason`/error set from the token design. The strict adapter-owned
lower-snake JSON input carries basis, model, canonical lowercase-hex request
fingerprint, typed transcript, and summary budget; output echoes the exact
identity and carries XML in `summary`. Reject unknown/missing/wrongly typed
fields, wrong version, noncanonical fingerprints, trailing JSON, invalid input
domain values, and output identity drift.

First pin the generic runtime's closed `OutputFailureReason`: `invalid_shape`,
`empty_text`, `too_large`, and `invalid_json`, evaluated in that order and never
carrying raw output. Prove zero/multiple/non-text/wrong-role results map to shape,
empty text maps independently, the registered descriptor `OutputBytes` bounds
the full JSON envelope, and invalid JSON remains distinct. The adapter maps
shape/empty to summary `output_shape`, too-large to `byte_limit`, and invalid JSON
to `wire` from the typed run outcome before caller product finalization.
`OutputError{Reason,Cause}` is exclusive: extraction sets one valid reason and nil
cause; normalized-usage or adapter-callback failure sets zero reason and one typed
cause; reject both-set and both-empty values and never retain raw output.

Before any `HustleCompleted` or product finalizer success, require normalized
non-nil usage with non-zero `OutputTokens`, conservatively enforce the whole JSON
envelope against `MaxSummaryTokens`, then validate the strict XML
root/order/children grammar.
Reject attributes/comments/directives/wrappers/trailing/nested/empty required
sections and every duplicate/unknown/missing/out-of-order child with the bounded
reason mapping. Preserve escaped character data. Define
`SummaryTooLargeError{Measurement}` now but reserve it for Task 26's
post-replacement complete-request count. Expose only a typed compactor to
loopruntime; no generic runner or public arbitrary hustle execution surface.
The adapter callback only receives generically valid bounded JSON; a descriptor
byte-cap check there is defensive and must not imply oversized raw output crosses
the runtime boundary.

Fuzz both external JSON and XML parsers for 30 seconds. Validator/domain checks
must precede completed audit and product commit. The adapter preserves the
current-loop hustle inference boundary and exact attempt basis/model/fingerprint
identity; Task 26 owns actor finalization and context replacement.

Commit: `feat(compaction): add typed hustle adapter`.

### Task 26.1: Canonical actor-owned compaction finalization

**Repository:** `$HARNESS`

**Files:** modify loop actor/control files and add focused finalization tests.

**RED:** `CompactionStarted` before invoke; direct pre-ownership errors;
actor-owned idempotent terminal transition; panic before/after durable append;
duration cut point; fatal journal exception; and exactly one waiter outcome while
writable.

Commit: `feat(loopruntime): finalize compaction attempts safely`.

### Task 26.2: Actor and in-flight turn context replacement

**Repository:** `$HARNESS`

**Files:** modify actor/turn state, handshake contracts, and focused tests.

**RED:** stale basis/model/fingerprint; actor and turn reset; queued input
survives; no public arbitrary rewrite; next request uses only validated summary.

Commit: `feat(loopruntime): replace compacted turn context`.

### Task 26.3: Safe step-boundary compaction semantics

**Repository:** `$HARNESS`

**Files:** modify step/turn runner paths and tests.

**RED:** tool continuation pauses and then invokes primary with summary; terminal
AI response remains byte-identical and `TurnDone` waits; post-summary recount;
summary-too-large blocks; soft/hard failure behavior.

Commit: `feat(loopruntime): compact at safe inference boundaries`.

### Task 27: Restore, catalog, and public Compact APIs

**Repository:** `$HARNESS`

**Files:**

- Modify: `internal/sessionruntime/restore.go` and tests
- Modify: `internal/loopruntime/restored.go` and tests
- Modify: `internal/sessionruntime/restore_constructor.go` and tests
- Modify: `pkg/session/session.go`, `contracts_test.go`,
  `internal/sessionruntime/session.go`, and tests. Add `Compact` and
  `CompactToLoop` to the public `Session` contract; do not create a separate
  generic controller surface.
- Modify: `pkg/sessionstore/catalog.go` and catalog/replay tests

**RED:** latest committed summary plus later events, multiple compactions, raw
journal retention, deterministic waiter repair, fatal append fault semantics,
`Compact` active convenience, `CompactToLoop` exact target, human agency stamping,
unknown/exited/foreign loop rejection, and no generic context rewrite API. Seed
restored messages, `ContextBasis`, current `ContextMeasurement`, and automatic
rearm state into the live actor; prove the first post-restore request uses them.

Commit: `feat(session): expose crash-safe conversation compaction`.

### Task 28: Harness full verification and planned v0.11.0 reconciliation

Run `make fmt`, default and integration race tests, required event/compaction
fuzz targets for 30 seconds, trimpath build, and `make secure`. Verify public
schema/serve drift guards and a clean worktree. Update `go.mod` planned version
pins without removing sibling replaces. Commit only non-empty reconciliation as
`docs: record hustle compaction support`.

## Phase E — CLI command and presentation

### Task 29: CLI harness pin and vendor refresh

**Repository:** `$CLI`

Modify `go.mod`, `go.sum`, `vendor/modules.txt`, vendored harness files, and
`tui/agent_test.go`. RED compile-tests public start/terminal events through the
all-loops filter. Set planned harness requirement `v0.11.0`, retain replace,
tidy/vendor, verify modules/source, then GREEN.

Commit: `chore(deps): vendor harness compaction contract`.

### Task 30: Bounded Agent compaction dispatch and focused slash command

**Repository:** `$CLI`

**Files:** modify `tui/agent.go`, `commands.go/test`, `messages.go`, fixtures,
slash completion/help, interaction/sessioncore/screen and tests.

**RED:** `Agent.CompactToLoop`, deadline at most 2 seconds without sleeps,
error preservation, `/compact` present, idle/running dispatch, focused—not
active—target, success silent, immediate failure visible.

**GREEN:** Add the bounded command; do not gate on turn status and do not show an
optimistic spinner.

Commit: `feat(tui): route compact command to focused loop`.

### Task 31: Pure compaction activity reducer and restore tombstones

**Repository:** `$CLI`

**Files:** create `tui/compaction.go`, tests; modify sessioncore/restore/display
projection/screen and tests.

**RED:** independent loops, matching/mismatched terminals, terminal-before-start
tombstone, duplicate idempotency, replay-terminal plus buffered-start race, and
`/clear` reset.

**GREEN:** Clone-on-write projection with active attempt per loop and terminal
attempt tombstones shared by live and restore folding.

Commit: `feat(tui): restore compaction activity safely`.

### Task 32: Replayable completion row and active status styling

**Repository:** `$CLI`

**Files:** modify transcript/statusline/screen/restore tests and implementations.

**RED:** committed duration row once per non-zero `EventID`; rejection no success
row; loop isolation; restore/live overlap; exact text without duplicate glyph;
compaction precedence; interrupt/clear precedence; idle compaction filled and
animated; elapsed suffix rules.

**GREEN:** Transcript reducer owns completion dedup and passes
`"conversation compacted in "+formatElapsed(d)` to `CommitHarnessFor`. A pure
`statusActive` derives styling from status plus compaction activity.

Commit: `feat(tui): show conversation compaction progress`.

### Task 33: CLI full verification and planned v0.6.0 reconciliation

Run formatting, default/integration race tests, trimpath build, secure gate,
vendor reproducibility (`go mod vendor` then no diff), and clean status. Commit
only non-empty reconciliation.

## Phase F — SWE product policy and composition

### Task 34: SWE dependency surface and model inference policy

**Repository:** `$SWE`

**Files:** modify `go.mod`, `go.sum`, dependency tests; create
`swarms/swe/inference_policy.go/test`; modify catalog/model tests and fakes.

Set planned requirements for core/inference/LLM/harness/CLI while retaining
replaces. SWE has no vendor tree.

**RED:** compile the entire new surface; Kimi/Phala 128K windows with unknown
independent caps; LM Studio unknown unless configured; fixed secret-free
inference capability per provider; unknown provider typed failure; local
heuristic metadata; standard model must resolve an input limit; live changes
cannot change provider/API/base URL.

Commit: `feat(swe): declare context inference policy`.

### Task 35: SWE compaction definition, prompt, and calibrated policy

**Repository:** `$SWE`

**Files:** create `swarms/swe/compaction.go`, `compaction_test.go`.

Pin the approved literal prompt, summary-consumption fragment, revisions, and
all values: current-loop blocking `context.compact`, 90s, 2MiB/64KiB, automatic,
AllowConservative, 8000/6000, 16384 reserve, 8192 margin, 4096 summary, 2s count.
Harness owns XML parsing; SWE pins parser revision and prompt digest.

Commit: `feat(swe): define conversation compaction`.

### Task 36: Register one hustle and enable every native loop

**Repository:** `$SWE`

**Files:** modify swarm/persistence/fingerprint/fake/error files and tests.

**RED:** three loops, one hustle, equivalent policy, summary fragment once in
every system, primer/operator prompt relation preserved, reviewer read-only,
same wiring for headless/new/restore/clear, fingerprint sensitivity and secret
exclusion, fail construction before session open.

Commit: `feat(swe): compose context compaction`.

### Task 37: SWE manual passthrough and private replay filtering

**Repository:** `$SWE`

**Files:** modify `swarms/swe/agent.go/test`, persistence integration tests,
dependency tests, and `cmd/swe/main_test.go` fakes.

**RED:** exact `CompactToLoop` forwarding, id/error preservation, no active-loop
fallback, internal hustle audit removed before gate/backlog folds, public terminal
retained, unknown visibility fail closed.

Commit passthrough as `feat(swe): expose loop compaction` and replay protection
as `fix(swe): hide internal hustle audit` if separable.

### Task 38: Scripted Invoke acceptance and restore integration

**Repository:** `$SWE`

**Files:** modify fake/acceptance tests; create
`swarms/swe/compaction_restore_integration_test.go`.

Add independent Stream and Invoke scripts/capture/barriers. Acceptance covers
exact one-shot request/no tools/current model/prompt/JSON data, valid XML reset,
next request context, usage isolation, all XML rejection cases, soft/hard
failure, model change, private audit, terminal response preservation, manual idle
compaction, and automatic threshold pause. Integration restores latest summary
plus subsequent context while raw journal retains superseded/internal records and
public backlog filters them.

Commit: `test(swe): cover compaction composition`.

### Task 39: SWE and cross-module final verification

Run SWE formatting, default/integration race tests, trimpath build, and lint.
Then from each of the six repositories run its full race suite, integration suite
where present, trimpath build, security/lint target, `git diff --check`, and
`git status --short`. Run required fuzz targets in core/inference/harness for 30
seconds. Confirm every dependency/vendor tree is reproducible and no internal
hustle event appears in a public API.

Dispatch one final cross-module spec reviewer and one final code-quality reviewer.
Resolve every Critical/Important finding and repeat the affected verification.
Then use `superpowers:finishing-a-development-branch` to present merge/release
options; do not tag, push, merge, or remove worktrees before the user chooses.
