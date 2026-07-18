# Harness OpenTelemetry Instrumentation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add first-party, privacy-safe OpenTelemetry traces, metrics, and occurrence records to Harness without changing runtime outcomes or requiring an SDK/exporter.

**Architecture:** A cohesive `pkg/telemetry` package owns every signal name, attribute, error classification, content/cardinality policy, and OTel helper. `rig.WithTelemetry` carries one immutable instance through session composition into the real runtime boundaries. Production code imports only OTel APIs and pinned semantic-convention constants; applications own SDKs, exporters, resources, sampling, shutdown, dashboards, and alerts.

**Tech Stack:** Go 1.26, OpenTelemetry Go API v1.44, pinned `semconv/v1.41.0`, Go `testing`, and the OTel SDK only in `_test.go` support.

---

Read [the design specification](2026-07-18-harness-opentelemetry-instrumentation-design.md)
before implementation. All commands below run from
`/Users/ipotter/code/looprig/harness` with `GOWORK=off`. Preserve the user's
existing `docs/TODO.md` changes and any other unrelated worktree edits.

The plan deliberately does not install an exporter or create alerts. The
composing application owns those concerns. Use `telemetry.Noop()` for every
omitted internal dependency; runtime code must never branch on a nil telemetry
pointer.

### Task 1: Approve and normalize the OTel dependency surface

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `vendor/modules.txt`
- Modify: `CLAUDE.md`
- Test: `pkg/telemetry/dependencies_test.go`

**Step 1: Write the dependency-boundary test**

Add an AST/import test that walks non-test Go files under `pkg/telemetry` and
rejects imports of `go.opentelemetry.io/otel/sdk`, exporter packages, and
`go.opentelemetry.io/otel/log`. Permit the logs API only in the future
`pkg/telemetry/otellog` adapter.

**Step 2: Run the focused test and verify failure**

Run: `GOWORK=off go test -race ./pkg/telemetry`

Expected: FAIL because `pkg/telemetry` does not exist.

**Step 3: Make the approved API dependencies direct**

Move these existing indirect modules to direct requirements without changing
their current v1.44 versions:

- `go.opentelemetry.io/otel`
- `go.opentelemetry.io/otel/metric`
- `go.opentelemetry.io/otel/trace`

Use the already-vendored `go.opentelemetry.io/otel/semconv/v1.41.0` package and
do not mix it with `semconv/v1.37.0`. Do not add an exporter. Keep
`go.opentelemetry.io/otel/sdk` and `/sdk/metric` test-only in usage even though
Go records test dependencies in the same module graph.

Add the approved packages to `CLAUDE.md`, identifying APIs/semconv as production
dependencies and the SDK as test support. This conversation is the explicit
dependency approval required by that file.

**Step 4: Create the minimal package and vendor consistently**

Create `pkg/telemetry/doc.go` with the package contract, then run:

`GOWORK=off go mod tidy`

`GOWORK=off go mod vendor`

Do not hand-edit vendored source.

**Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./pkg/telemetry`

Expected: PASS.

Commit: `chore: establish harness otel dependency boundary`

### Task 2: Build the no-op-safe telemetry façade

**Files:**
- Create: `pkg/telemetry/config.go`
- Create: `pkg/telemetry/telemetry.go`
- Create: `pkg/telemetry/record.go`
- Create: `pkg/telemetry/errors.go`
- Test: `pkg/telemetry/telemetry_test.go`

**Step 1: Write construction and no-op tests**

Cover valid zero configuration, explicit providers, nil record emitter,
duplicate construction safety, invalid policy values, instrument-construction
failure, and a panicking custom emitter. Assert the panic is recovered and is
not returned to the caller.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./pkg/telemetry -run 'Test(New|Noop|Emit)'`

Expected: FAIL with undefined contracts.

**Step 3: Implement the public contracts**

Add `Providers`, `Config`, `Telemetry`, `RecordEmitter`, `Record`, `Severity`,
`EventDetail`, `ContentPolicy`, and `MetricLabelPolicy`. `New` uses explicit
providers when supplied and OTel globals otherwise. It creates instruments once
and returns a typed construction error. `Noop` returns a reusable, immutable
instance.

Keep `RecordEmitter.Emit(context.Context, Record)` return-free. The wrapper must
copy bounded attribute storage before handing it to an emitter that may retain
the record, call it outside runtime locks, and recover a panic at the telemetry
boundary.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./pkg/telemetry`

Expected: PASS.

Commit: `feat: add harness telemetry facade`

### Task 3: Centralize attributes, privacy, cardinality, and outcomes

**Files:**
- Create: `pkg/telemetry/attributes.go`
- Create: `pkg/telemetry/content.go`
- Create: `pkg/telemetry/cardinality.go`
- Create: `pkg/telemetry/outcome.go`
- Test: `pkg/telemetry/attributes_test.go`
- Test: `pkg/telemetry/content_test.go`
- Test: `pkg/telemetry/outcome_test.go`

**Step 1: Write table-driven policy tests**

Cover every `looprig.*` key from the design, the pinned GenAI keys, metric-safe
closed enums, allowlisted model/tool names, collapse-to-`other`, UUID/path/URL
rejection, and invalid UTF-8. Assert `ContentNone` emits no prompt, message,
tool argument/result, permission text, URL, path, environment value, or raw
error string. Assert `ContentMetadata` emits only counts, kinds, sizes, and
SHA-256 digests.

**Step 2: Write error/outcome mapping tests**

Use the existing typed provider, tool, journal, checkpoint, validation,
cancellation, denial, and timeout errors. Assert each maps to a bounded
`error.type` and `looprig.outcome`, and that no mapped attribute contains
`err.Error()`.

**Step 3: Run and verify failure**

Run: `GOWORK=off go test -race ./pkg/telemetry -run 'Test(Attribute|Content|Outcome|ErrorType)'`

Expected: FAIL.

**Step 4: Implement closed builders**

Runtime packages must call typed attribute builders instead of constructing
ad-hoc `attribute.KeyValue` slices. Separate span/record attributes from metric
attributes so IDs cannot accidentally cross into datapoints. Keep user-defined
strings denied unless the configured allowlist admits them.

**Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./pkg/telemetry`

Expected: PASS.

Commit: `feat: enforce telemetry privacy and cardinality policy`

### Task 4: Create metrics and span-operation helpers

**Files:**
- Create: `pkg/telemetry/metrics.go`
- Create: `pkg/telemetry/operations.go`
- Test: `pkg/telemetry/metrics_test.go`
- Test: `pkg/telemetry/operations_test.go`

**Step 1: Write metric descriptor tests**

Assert every metric in the design has the correct name, type, UCUM unit, and
approved attribute set. Include the four GenAI client metrics and all
`looprig.harness.*` metrics. Ensure histogram count is not duplicated by a
meaningless success counter.

**Step 2: Write operation-lifecycle tests**

Using an in-memory SDK in test files, verify helpers start the required span
name/kind, attach identifiers only to spans, record duration exactly once,
classify completion, set error status only for actual failures, and tolerate
double-finish without duplicate metric observations.

**Step 3: Run and verify failure**

Run: `GOWORK=off go test -race ./pkg/telemetry -run 'Test(Metric|Operation)'`

Expected: FAIL.

**Step 4: Implement typed operation handles**

Provide focused starters for session lifecycle, agent turn, step, inference,
tool, gate wait, command dispatch, journal append, checkpoint, workspace
restore, compaction, and Hustle run. Return the child context plus an idempotent
typed completion handle. Do not expose the raw tracer/meter to runtime packages.

Inference helpers must support authoritative usage, time-to-first-chunk, and
optional inter-chunk timing without estimating missing usage.

**Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./pkg/telemetry`

Expected: PASS.

Commit: `feat: add typed telemetry operations and metrics`

### Task 5: Exhaustively map commands and events to occurrence records

**Files:**
- Create: `pkg/telemetry/commands.go`
- Create: `pkg/telemetry/events.go`
- Test: `pkg/telemetry/commands_test.go`
- Test: `pkg/telemetry/events_test.go`
- Modify: `pkg/command/command_test.go`
- Modify: `pkg/event/event_test.go`

**Step 1: Inventory all concrete domain variants**

Create test fixtures listing every concrete command and event supported by the
domain packages. The tests must fail if a variant has no stable occurrence name
or explicit suppression decision. Avoid reflection over arbitrary external
values in production; exhaustiveness belongs in tests.

**Step 2: Specify detail behavior**

Test `EventsEnduring`, `EventsLifecycle`, and `EventsAll`. All modes increment
the event counter. `TokenDelta` produces no record by default, produces one only
under `EventsAll`, and never carries chunk content. Domain `CreatedAt` is the
record occurrence time.

**Step 3: Run and verify failure**

Run: `GOWORK=off go test -race ./pkg/telemetry -run 'Test(Command|Event)'`

Expected: FAIL.

**Step 4: Implement closed type switches**

Map names, class, scope, agency, correlation coordinates, outcome, and bounded
error type. Unknown variants increment `telemetry.record.dropped` with reason
`unknown_type` and do not leak `%T` into metric labels. Keep full content out of
records regardless of detail level.

**Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./pkg/telemetry ./pkg/command ./pkg/event`

Expected: PASS.

Commit: `feat: map harness command and event telemetry`

### Task 6: Wire one telemetry instance through Rig and session composition

**Files:**
- Modify: `pkg/rig/options.go`
- Modify: `pkg/rig/definition.go`
- Modify: `pkg/rig/options_test.go`
- Modify: `internal/sessionruntime/lifecycle.go`
- Modify: `internal/sessionruntime/command_journal.go`
- Modify: `internal/sessionruntime/session.go`
- Modify: `internal/sessionruntime/lifecycle_test.go`
- Modify: `internal/sessionruntime/composition_options_test.go`

**Step 1: Write wiring tests**

Assert `rig.Define` without telemetry installs `telemetry.Noop()`. Assert
`rig.WithTelemetry(nil)` and duplicate use are typed definition failures. Assert
one supplied instance reaches new and restored sessions, their native loops,
their Hustle controller, hub, and appenders without being copied or replaced.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./pkg/rig ./internal/sessionruntime -run 'Test.*Telemetry'`

Expected: FAIL.

**Step 3: Add the singleton option**

Add `keyTelemetry`, `rig.WithTelemetry`,
`sessionruntime.WithLifecycleTelemetry`, and an internal session option. Store
`*telemetry.Telemetry` as immutable wiring. Append the lifecycle option before
constructing `NewTopologyLifecycle` and forward it through both new and restore
paths.

Do not include telemetry configuration in the durable config fingerprint: SDK
and export policy changes are operational deployment changes, not replay
semantics.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./pkg/rig ./internal/sessionruntime`

Expected: PASS.

Commit: `feat: wire telemetry through rig lifecycle`

### Task 7: Instrument the native turn, step, and inference hot path

**Files:**
- Modify: `internal/loopruntime/config.go`
- Modify: `internal/loopruntime/turn.go`
- Modify: `internal/loopruntime/step.go`
- Modify: `internal/loopruntime/chunk.go`
- Modify: `internal/loopruntime/loop.go`
- Test: `internal/loopruntime/telemetry_test.go`
- Test: `internal/loopruntime/telemetry_race_test.go`

**Step 1: Write trace-shape tests**

Drive a text-only turn through the in-memory SDK and assert:

```text
invoke_agent {agent}
└── harness.step
    └── chat {model}
```

Assert the child context reaches `inference.Client.Stream`, the turn span begins
only after committed `TurnStarted`, terminal outcomes close it once, and no
long-lived session parent exists. Cover success, provider error, cancellation,
interrupt, malformed structured output, no chunks, and multiple chunks.

**Step 2: Write metric tests**

Verify turn/step/inference duration, time to first chunk, inter-chunk timing when
enabled, and authoritative input/output tokens. Assert no token datapoint when
the provider supplies no usage.

**Step 3: Run and verify failure**

Run: `GOWORK=off go test -race ./internal/loopruntime -run 'TestTelemetry'`

Expected: FAIL.

**Step 4: Add the narrow runtime hooks**

Carry the telemetry pointer in `runtimeConfig`, `turnConfig`, and `stepConfig`.
Start/finish spans at `runTurn` and `runStep`; wrap only the logical
`cfg.client.Stream` operation and pass its child context to the client. Do not
create a duplicate HTTP/provider-attempt span. Accumulate streaming timing and
usage without emitting one span per chunk.

**Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./internal/loopruntime`

Expected: PASS.

Commit: `feat: instrument native turn and inference path`

### Task 8: Instrument gate waiting and tool execution

**Files:**
- Modify: `internal/loopruntime/runner.go`
- Modify: `internal/loopruntime/gate.go`
- Modify: `internal/loopruntime/contracts.go`
- Test: `internal/loopruntime/runner_telemetry_test.go`
- Test: `internal/loopruntime/gate_telemetry_test.go`

**Step 1: Write boundary tests**

Cover no-gate tools, approved, denied, canceled, timed-out, tool failure,
middleware failure, and parallel tools. Assert gate wait is not included in the
tool span: `harness.gate.wait` ends at resolution and `execute_tool {tool}`
starts only after approval.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./internal/loopruntime -run 'Test(Tool|Gate)Telemetry'`

Expected: FAIL.

**Step 3: Instrument `RunBatch` and `execute`**

Thread the parent step context into every parallel tool operation. Record tool
class and allowlisted name on metrics, tool call/execution IDs on spans only,
and typed outcomes. Do not attach arguments, results, gate question text, or
denial reasons.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./internal/loopruntime`

Expected: PASS.

Commit: `feat: instrument tool and gate operations`

### Task 9: Add asynchronous command correlation and dispatch telemetry

**Files:**
- Create: `pkg/telemetry/correlation.go`
- Test: `pkg/telemetry/correlation_test.go`
- Modify: `internal/sessionruntime/session.go`
- Modify: `internal/sessionruntime/command_journal.go`
- Test: `internal/sessionruntime/command_telemetry_test.go`

**Step 1: Test bounded correlation ownership**

Verify capture, consume-once, explicit delete, expiry, capacity rejection,
session shutdown cleanup, and concurrent access. Store only a valid remote or
local `trace.SpanContext`; never retain context cancellation, values, baggage,
or the span object.

**Step 2: Test submit propagation**

Start a caller span, invoke fire-and-forget `Session.Submit`, let the caller
span end, and assert the eventual turn is parented from the captured span
context. Cover dispatch rejection, journal audit failure, queued cancellation,
missing correlation, machine agency, and shutdown before start.

**Step 3: Run and verify failure**

Run: `GOWORK=off go test -race ./pkg/telemetry ./internal/sessionruntime -run 'Test(Correlation|CommandTelemetry|SubmitTelemetry)'`

Expected: FAIL.

**Step 4: Implement command telemetry**

Capture context after command ID minting, then instrument validation/dispatch.
Emit a command occurrence after dispatch success or with `dispatch_error` after
failure. Measure queue duration from command `CreatedAt` to committed
`TurnStarted` when that relation exists. Delete correlation on every terminal
path; a missing entry starts a root turn and is not an error.

Keep trace IDs out of commands, events, and journal records.

**Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./pkg/telemetry ./internal/sessionruntime`

Expected: PASS with no race report.

Commit: `feat: correlate async commands with turn traces`

### Task 10: Instrument event publication and journal appends

**Files:**
- Modify: `pkg/hub/deps.go`
- Modify: `pkg/hub/hub.go`
- Test: `pkg/hub/telemetry_test.go`
- Modify: `pkg/journal/appender.go`
- Test: `pkg/journal/telemetry_test.go`
- Modify: `internal/sessionruntime/command_journal.go`
- Modify: `internal/sessionruntime/gates.go`

**Step 1: Write publication and append tests**

Cover ephemeral and enduring events, successful checked publication, append
failure, subscriber failure, command audit-only append failure, required gate
append failure, and a panicking occurrence emitter. Assert event records happen
only after successful publication; failures emit
`looprig.harness.event.publish_failed` with the intended bounded event name.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./pkg/hub ./pkg/journal -run 'TestTelemetry'`

Expected: FAIL.

**Step 3: Add constructor-compatible options**

Inject telemetry into Hub and checked appenders at their composition roots while
preserving existing constructors by defaulting to no-op. Wrap exactly one
underlying append attempt in `harness.journal.append`; include record kind,
durability policy, and outcome. Do not infer append latency from event times.

Event publication duration must cover the actual publish boundary once. Avoid
double-counting when `Session.PublishEvent` delegates to Hub.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./pkg/hub ./pkg/journal ./internal/sessionruntime`

Expected: PASS.

Commit: `feat: instrument event and journal boundaries`

### Task 11: Instrument session lifecycle, checkpointing, restore, and compaction

**Files:**
- Modify: `pkg/rig/lifecycle.go`
- Modify: `internal/sessionruntime/lifecycle.go`
- Modify: `internal/sessionruntime/session.go`
- Modify: `internal/sessionruntime/checkpoint_controller.go`
- Modify: `internal/sessionruntime/workspace_restore.go`
- Modify: `internal/loopruntime/compaction_executor.go`
- Test: `pkg/rig/lifecycle_telemetry_test.go`
- Test: `internal/sessionruntime/lifecycle_telemetry_test.go`
- Test: `internal/sessionruntime/checkpoint_telemetry_test.go`
- Test: `internal/loopruntime/compaction_telemetry_test.go`

**Step 1: Write lifecycle tests**

Cover create/restore/shutdown success and every typed failure stage, including
lease, journal, appender, security-limit, restore drift, workspace, and drain
failures. Assert the spans are bounded operations and restored sessions start
new traces while retaining semantic session IDs.

**Step 2: Write checkpoint and compaction tests**

Cover manual and automatic triggers, required versus best-effort checkpointing,
workspace restore, compaction success/failure/cancellation, and the case where
work fails before an event can be published.

**Step 3: Run and verify failure**

Run: `GOWORK=off go test -race ./pkg/rig ./internal/sessionruntime ./internal/loopruntime -run 'Test.*Telemetry'`

Expected: FAIL.

**Step 4: Instrument the actual operation boundaries**

Start lifecycle operations at public Rig/Lifecycle entry and end them after the
ready/durable/final cleanup boundary. Ensure only one layer owns each span.
Record active-operation changes with balanced increments/decrements, including
panic-safe deferred completion.

**Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./pkg/rig ./internal/sessionruntime ./internal/loopruntime`

Expected: PASS.

Commit: `feat: instrument harness lifecycle and persistence work`

### Task 12: Instrument Hustle execution without changing its session model

**Files:**
- Modify: `internal/hustleruntime/contracts.go`
- Modify: `internal/hustleruntime/execution.go`
- Test: `internal/hustleruntime/telemetry_test.go`
- Modify: `internal/sessionruntime/hustle.go`
- Test: `internal/sessionruntime/lifecycle_hustle_test.go`

**Step 1: Write Hustle trace tests**

Cover blocking/background participation, queue rejection, eligibility wait,
successful `Invoke`, provider failure, validation failure, terminal audit
failure, finalizer failure, cancellation, and shutdown drain. Assert:

```text
harness.hustle.run
└── chat {model}
```

The logical inference child must receive the child context. A Hustle remains a
session-oriented Harness operation; this does not make it an eval runner.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./internal/hustleruntime ./internal/sessionruntime -run 'TestHustleTelemetry'`

Expected: FAIL.

**Step 3: Wire and record the operation**

Add telemetry to `RuntimeConfig`, start the run span after ownership admission,
and end it after the required terminal audit/finalization boundary. Record queue
and execution separately, use participation/outcome as bounded metric labels,
and keep Hustle name/results out of metrics and default records.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./internal/hustleruntime ./internal/sessionruntime`

Expected: PASS.

Commit: `feat: instrument hustle execution`

### Task 13: Define the foreign-loop telemetry adapter contract

**Files:**
- Modify: `pkg/foreign/builder.go`
- Modify: `pkg/foreign/restored.go`
- Create: `pkg/foreign/telemetry.go`
- Test: `pkg/foreign/telemetry_test.go`
- Modify: `internal/sessionruntime/foreign_newloop_test.go`
- Modify: `internal/sessionruntime/foreign_restore_test.go`
- Create: `docs/FOREIGN_LOOPS.md`

**Step 1: Test the Harness-owned contract**

Create a fake foreign backend that opens and closes a foreign turn through the
adapter. Assert it emits one `invoke_agent` span, uses semantic IDs for
correlation, closes at `TurnDone`/failure/interruption, and does not require
`LoopIdle` or `SessionIdle` to close.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./pkg/foreign ./internal/sessionruntime -run 'TestForeign.*Telemetry'`

Expected: FAIL.

**Step 3: Add a narrow optional capability**

Do not import the extracted concrete foreign-loop module back into Harness.
Expose only the smallest context/operation decorator needed by a foreign
builder, and pass it through new and restored builder composition. Process,
driver, HTTP, and provider-attempt spans remain owned by the foreign module.

Document the known quiescence limitation: a foreign primary currently has no
`LoopIdle`, so `SessionIdle` does not fire. Emit bounded lifecycle telemetry for
the unavailable idle boundary; do not synthesize a false idle event.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./pkg/foreign ./internal/sessionruntime`

Expected: PASS.

Commit: `feat: add foreign loop telemetry adapter`

### Task 14: Add the isolated OTel Logs occurrence adapter

**Files:**
- Create: `pkg/telemetry/otellog/doc.go`
- Create: `pkg/telemetry/otellog/emitter.go`
- Test: `pkg/telemetry/otellog/emitter_test.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `vendor/modules.txt`
- Modify: `CLAUDE.md`

**Step 1: Write adapter tests**

Using the pinned beta Logs API test provider, assert occurrence timestamp,
severity, record name/body, attributes, trace correlation from context, and
panic/failure isolation. Assert no log API import appears elsewhere in Harness.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./pkg/telemetry/otellog`

Expected: FAIL because the adapter does not exist.

**Step 3: Implement the isolated adapter**

Translate `telemetry.Record` into one OTel log record. Keep the core
`RecordEmitter` contract stable if the beta OTel API changes. Add the exact logs
module/API to `CLAUDE.md` as an approved isolated dependency, then tidy/vendor.
If the pinned logs API cannot satisfy the contract cleanly, stop this task and
retain the core emitter seam; do not leak beta types into `pkg/telemetry`.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./pkg/telemetry/...`

Expected: PASS.

Commit: `feat: add otel log occurrence adapter`

### Task 15: Prove end-to-end behavior, safety, and overhead

**Files:**
- Create: `integration/telemetry/trace_integration_test.go`
- Create: `integration/telemetry/metrics_integration_test.go`
- Create: `integration/telemetry/privacy_integration_test.go`
- Create: `pkg/telemetry/benchmark_test.go`
- Create: `docs/TELEMETRY.md`
- Modify: `README.md`

**Step 1: Write representative integration tests**

Build an instrumented Rig with in-memory storage and test providers. Exercise:

- create → submit → turn → inference → tool → durable terminal → shutdown;
- provider error, tool error, denial, interrupt, and append failure;
- caller-span propagation through fire-and-forget submit;
- concurrent sessions and balanced active-operation counts;
- native, Hustle, and fake foreign turns.

Assert exact parent/child shape without assuming exporter ordering.

**Step 2: Add privacy and cardinality regression scans**

Seed prompts, tool arguments/results, permission text, URLs, paths, secrets, raw
errors, and UUIDs with unique canaries. Search all exported span attributes,
records, and metric datapoints. Under defaults, no content canary may appear;
no metric may contain any ID or arbitrary consumer-controlled string.

**Step 3: Benchmark the hot paths**

Benchmark no-op, sampled-out, and recording paths for inference chunks and tool
calls. Include allocations/op. Do not add a hard threshold until a stable CI
baseline exists; document and investigate regressions in token-delta handling.

**Step 4: Document application composition**

Show an application—not Harness—constructing an SDK, resource, sampler,
exporters, `telemetry.New`, `rig.WithTelemetry`, and shutdown. Explain signal
ownership, content defaults, allowlists, the beta log adapter, semantic-convention
pinning, foreign `SessionIdle` limitation, and backend-owned alerts.

**Step 5: Run full verification**

Run:

`GOWORK=off go test -race ./...`

`GOWORK=off go test -tags integration -race ./...`

`CGO_ENABLED=0 GOWORK=off go build -trimpath ./...`

`GOWORK=off make secure`

Expected: all commands PASS with no race reports, privacy canary leaks, metric
cardinality violations, or unformatted files.

**Step 6: Commit**

Commit: `test: verify harness telemetry end to end`

### Task 16: Review semantic schema and release readiness

**Files:**
- Modify: `docs/TELEMETRY.md`
- Create or modify: `CHANGELOG.md`
- Test: `pkg/telemetry/schema_test.go`

**Step 1: Freeze golden schema assertions**

Snapshot all public instrument names, span names, record names, units, and
allowed attributes. Record the exact OTel/GenAI semconv revision. A future
revision change must intentionally update this golden and release notes.

**Step 2: Audit package ownership**

Verify production code imports no SDK/exporter, Harness imports no eval module,
no trace ID enters durable state, no session-lifetime span exists, and every
runtime hook delegates signal construction to `pkg/telemetry`.

**Step 3: Re-run the complete gate**

Run:

`GOWORK=off go test -race ./...`

`GOWORK=off go test -tags integration -race ./...`

`CGO_ENABLED=0 GOWORK=off go build -trimpath ./...`

`GOWORK=off make secure`

Expected: PASS.

**Step 4: Commit**

Commit: `docs: publish harness telemetry contract`

## Execution checkpoints

Request review after Tasks 4, 8, 11, and 15. Those boundaries respectively
freeze the telemetry façade, native critical path, lifecycle/persistence
coverage, and end-to-end operational contract. Do not begin the next phase if a
review finds content leakage, unbounded metric labels, duplicate spans, or an
instrumentation path that changes a domain outcome.
