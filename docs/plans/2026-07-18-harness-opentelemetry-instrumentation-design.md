# Harness OpenTelemetry Instrumentation — Design Specification

**Date:** 2026-07-18
**Status:** Approved design extracted from the eval/continuous-observation discussion
**Scope:** `github.com/looprig/harness`
**Companion:** [Reusable Evaluation Framework](2026-07-18-eval-framework-design.md)

## Summary

Add first-party OpenTelemetry instrumentation to Harness through a cohesive
`pkg/telemetry` package and narrow hooks at the runtime's real execution
boundaries. Harness emits traces for duration-bearing operations, metrics for
aggregate health and latency, and structured occurrence records for commands and
events. It does not reconstruct timing from the journal and does not make every
domain event a span.

Harness is a library, so it depends only on OpenTelemetry APIs and pinned
semantic-convention constants. The composing application owns SDKs, resources,
sampling, exporters, collectors, shutdown, storage, dashboards, and alerts. With
no configured SDK, the instrumentation is a no-op and runtime behavior is
unchanged.

The durable journal remains the authoritative audit and replay record.
Telemetry is operational, lossy, sampled, and never written back into the
journal or conversation.

## Context

Harness already has a strong correlation vocabulary:

- `identity.Coordinates`: session, loop, turn, and step IDs;
- command ID and creation time;
- event ID, creation time, direct cause, agency, scope, and durability class;
- tool execution ID;
- turn, step, tool, gate, checkpoint, and journal lifecycle boundaries;
- typed provider, tool, persistence, restore, and cancellation failures.

It currently contains no Harness-owned OTel instrumentation. OTel modules are
present only as indirect dependencies. The event stream records semantic state
transitions, but it cannot by itself measure the complete duration of inference,
tool execution, blocked gates, command queueing, failed journal appends, or work
that ends before an event is published.

OpenTelemetry's current guidance matches this split: operations with meaningful
duration are spans, while point-in-time state changes are events/records. The Go
project also distinguishes library instrumentation—which should use OTel APIs
only—from application setup, which owns the SDK and exporters. GenAI semantic
conventions remain under active development, so this design pins their revision
and isolates their use behind Harness-owned helpers:

- <https://opentelemetry.io/docs/languages/go/instrumentation/>
- <https://opentelemetry.io/docs/specs/semconv/general/events/>
- <https://github.com/open-telemetry/semantic-conventions/tree/main/docs/gen-ai>

## Goals

- Explain where time is spent across session, turn, step, inference, tool,
  permission, journal, checkpoint, compaction, and Hustle operations.
- Expose tool, provider, structured-output, persistence, and lifecycle failures
  without waiting for an operator to reproduce them.
- Correlate telemetry with Harness session/loop/turn/step/command/event/tool IDs.
- Emit every command and domain event as an exhaustively mapped occurrence or an
  explicit high-volume suppression.
- Reuse OTel GenAI names for model inference, agent invocation, tool execution,
  and token usage where their semantics fit.
- Keep metrics safe from unbounded cardinality.
- Keep prompts, messages, tool arguments/results, permission text, URLs,
  credentials, and PII out of telemetry by default.
- Support native loops, foreign loops, Hustles, headless use, HTTP composition,
  persistence, and future eval adapters.
- Make telemetry failures incapable of failing or delaying Harness work.

## Non-goals

- Configuring an OTel SDK, OTLP exporter, collector, backend, or dashboard.
- Defining Grafana, Datadog, or other vendor alert policies.
- Replacing the Harness journal, event subscriptions, `slog`, or audit records.
- Exporting full conversations, system prompts, tool schemas, arguments, or
  results by default.
- Persisting trace/span IDs in commands, events, or the journal.
- Holding one trace span open for an entire potentially long-lived session.
- Instrumenting provider HTTP internals that belong in `inference`, a provider
  SDK, or `otelhttp`.
- Coupling Harness to `github.com/looprig/eval`.

## Approaches considered

### 1. Instrument actual execution boundaries — selected

Start and stop instrumentation where the runtime performs work. Domain events
remain correlation and outcome facts. This captures failures before publication,
provider streaming, gate wait time, journal latency, and tool duration accurately.

Trade-off: it adds small, explicit hooks to several runtime packages.

### 2. Reconstruct everything from commands and events — rejected

This would keep instrumentation outside the runtime, but timestamps do not cover
all operation boundaries. Ephemeral events may be dropped, foreign loops have
different lifecycle gaps, failed appends may have no successful event, and an
event observer cannot reliably separate queue, provider, tool, commit, or
checkpoint time.

This remains useful for coarse external analytics, not first-party latency
instrumentation.

### 3. Turn every command and event into a span — rejected

This produces very large traces, treats point occurrences as operations, and is
especially unsuitable for `TokenDelta`. Commands and events instead produce
structured occurrence records and a bounded event counter. Only work with a
meaningful start/end boundary gets a span.

## Architecture

```text
application composition root
  ├── OTel SDK / resource / sampler / exporters / shutdown
  └── telemetry.Config
          |
          v
  harness/pkg/telemetry
    ├── Tracer + span helpers
    ├── Meter + instruments
    ├── RecordEmitter (command/event occurrence records)
    ├── attribute/cardinality policy
    └── content policy (safe by default)
          |
          v
  real Harness boundaries
    ├── pkg/rig + internal/sessionruntime
    ├── internal/loopruntime
    ├── internal/hustleruntime
    ├── pkg/hub + pkg/journal
    └── checkpoint/compaction/tool/gate execution
```

`pkg/telemetry` is the single owner of instrument names, span names, attribute
keys, semantic-convention revision, error classification, occurrence mapping,
and cardinality policy. Runtime packages do not assemble arbitrary OTel
attributes themselves.

### Public construction

Illustrative API:

```go
type Providers struct {
    Tracer trace.TracerProvider
    Meter  metric.MeterProvider
}

type Config struct {
    Providers       Providers
    Records         RecordEmitter
    EventDetail     EventDetail
    Content         ContentPolicy
    MetricLabels    MetricLabelPolicy
    ScopeVersion    string
}

func New(Config) (*Telemetry, error)
func Noop() *Telemetry
```

`rig.WithTelemetry(*telemetry.Telemetry)` installs one immutable, concurrency-safe
instance for every session, loop, and Hustle created by that Rig. Bare Rig
construction installs `telemetry.Noop()`; internal code never branches on nil.

If providers are omitted, `New` uses the OTel global providers. When those are
the default no-op providers, Harness performs no export. `New` validates and
creates instruments once. Instrument-construction errors fail composition, not a
running turn.

Harness never calls provider `ForceFlush` or `Shutdown`; provider ownership stays
with the application.

### Occurrence records

OTel Go traces and metrics are stable while its logs API is currently beta. To
avoid spreading a beta API across Harness, the core instrumentation depends on a
narrow interface:

```go
type RecordEmitter interface {
    Emit(context.Context, Record)
}

type Record struct {
    Name       string
    OccurredAt time.Time
    Severity   Severity
    Attributes []attribute.KeyValue
}
```

`pkg/telemetry/otellog` supplies an adapter to the pinned OTel Logs API. A
composition root may instead bridge records to `slog` or another OTel-compatible
pipeline. The default emitter is no-op. Record emission is best effort and must
never return an error into Harness execution.

## Trace model

### No long-lived session parent span

A Harness session can live for minutes, days, or across process restoration.
Keeping one span open for its lifetime produces awkward sampling, buffering, and
cross-process semantics. Instead:

- create, restore, checkpoint, and shutdown are bounded lifecycle spans;
- each turn is an `invoke_agent` span and normally a trace root or child of the
  caller's active span;
- `gen_ai.conversation.id` and Harness IDs correlate turns across traces;
- restoration begins new traces and preserves semantic IDs, not trace identity.

### Normal turn topology

```text
incoming application/HTTP span (when present)
└── invoke_agent {agent}                 one Harness turn
    ├── harness.step                     inference + resulting tools + commit
    │   ├── chat {model}                 logical inference.Client stream
    │   │   └── HTTP/provider span       optional lower-layer instrumentation
    │   ├── harness.gate.wait            only when permission/input blocks
    │   ├── execute_tool {tool}
    │   │   └── HTTP/process/storage     tool-owned child instrumentation
    │   └── harness.journal.append       StepDone durable boundary
    ├── harness.compaction               when context compaction runs
    └── harness.journal.append           terminal durable boundary
```

Span names contain only bounded operation names plus model/agent/tool names where
the applicable GenAI convention requires them. IDs never appear in span names.

### Span inventory

| Span | Kind | Boundary | Required outcome |
|---|---|---|---|
| `harness.session.create` | INTERNAL | Rig/session creation entry to ready or error | success/error |
| `harness.session.restore` | INTERNAL | restore entry through `RestoreDone` or failure | success/error |
| `harness.session.shutdown` | INTERNAL | shutdown entry through drain/lease release | success/error |
| `invoke_agent {agent}` | INTERNAL | committed `TurnStarted` through committed terminal | done/failed/interrupted |
| `harness.step` | INTERNAL | step allocation through `StepDone` commit or terminal | success/error/interrupted |
| `chat {model}` | CLIENT | immediately around `inference.Client.Stream` or `Invoke` | success/error/canceled |
| `execute_tool {tool}` | INTERNAL | approved tool execution start through result | success/error/canceled |
| `harness.gate.wait` | INTERNAL | gate open/prepared through resolution/cancel | approved/denied/canceled |
| `harness.command.dispatch` | INTERNAL | validated command dispatch through send/failure | dispatched/error |
| `harness.journal.append` | INTERNAL | one underlying append attempt | success/error |
| `harness.workspace.checkpoint` | INTERNAL | snapshot/checkpoint request through committed result | success/error |
| `harness.workspace.restore` | INTERNAL | workspace replacement attempt | success/error |
| `harness.compaction` | INTERNAL | context compaction attempt | success/error/canceled |
| `harness.hustle.run` | INTERNAL | admitted Hustle run through terminal audit result | success/error/canceled |

The inference span is the logical client operation observed by Harness. If the
inference transport or provider SDK emits physical HTTP/request-attempt spans,
those appear beneath it because Harness passes the child context into
`Client.Stream`/`Invoke`. Harness must not create a duplicate physical HTTP span.

The tool span starts only after approval; gate waiting is therefore measured
separately. Tool arguments and results are not span attributes by default.

### Parentage across asynchronous boundaries

`Session.Submit` is fire-and-forget, so the submit context may end before the
turn starts. Harness captures only the caller's `trace.SpanContext`, keyed by the
minted command ID in a bounded session-local correlation table. It does not
retain the caller's cancellation, values, or baggage.

When `TurnStarted.Cause.CommandID` resolves the command, loop runtime consumes
that span context and creates the turn span. Entries are deleted on start,
rejection, cancellation, dispatch failure, timeout, or session stop. A missing
entry creates a root turn span and is not an error.

Machine-issued subagent commands use the same mechanism. When a child operation
cannot safely remain a child—for example it outlives the initiating span—it
starts a new trace with an OTel Span Link when the causal span context is still
available. Harness never reconstructs a parent from UUIDs.

## Metrics

Units follow OTel/UCUM conventions: seconds use `s`, tokens use `{token}`, and
counts use `{operation}`, `{event}`, or `{item}` as applicable. Duration
histograms always include count, so separate success counters are added only
where they answer a different operational question.

### OTel GenAI metrics

Use the pinned GenAI conventions when the required data is supplied by the
provider:

| Metric | Instrument | Notes |
|---|---|---|
| `gen_ai.client.operation.duration` | Float64Histogram, `s` | Logical `Stream`/`Invoke` duration. |
| `gen_ai.client.token.usage` | Int64Histogram, `{token}` | Input/output token type; provider-normalized usage only. |
| `gen_ai.client.operation.time_to_first_chunk` | Float64Histogram, `s` | Streaming calls only; no value when no chunk arrives. |
| `gen_ai.client.operation.time_per_output_chunk` | Float64Histogram, `s` | Streaming inter-chunk distribution when enabled. |

Never estimate or fabricate provider usage in this layer. Cache-read,
cache-creation, and reasoning usage remain span attributes or additional
measurements only when `content.Usage` supplies them with defined semantics.

### Harness metrics

| Metric | Instrument | Low-cardinality attributes |
|---|---|---|
| `looprig.harness.turn.duration` | Float64Histogram, `s` | engine, outcome |
| `looprig.harness.step.duration` | Float64Histogram, `s` | outcome |
| `looprig.harness.tool.duration` | Float64Histogram, `s` | tool class, outcome; allowlisted name optional |
| `looprig.harness.gate.wait.duration` | Float64Histogram, `s` | gate kind, outcome |
| `looprig.harness.command.queue.duration` | Float64Histogram, `s` | command name, outcome |
| `looprig.harness.command.dispatch.duration` | Float64Histogram, `s` | command name, outcome |
| `looprig.harness.event.publish.duration` | Float64Histogram, `s` | event name, class, outcome |
| `looprig.harness.journal.append.duration` | Float64Histogram, `s` | record kind, durability policy, outcome |
| `looprig.harness.workspace.checkpoint.duration` | Float64Histogram, `s` | trigger, consistency, outcome |
| `looprig.harness.compaction.duration` | Float64Histogram, `s` | trigger, outcome |
| `looprig.harness.hustle.duration` | Float64Histogram, `s` | participation, outcome |
| `looprig.harness.command.count` | Int64Counter, `{command}` | command name, agency, outcome |
| `looprig.harness.event.count` | Int64Counter, `{event}` | event name, class, scope, outcome |
| `looprig.harness.operation.active` | Int64UpDownCounter, `{operation}` | operation kind |
| `looprig.harness.queue.depth` | Int64ObservableGauge, `{item}` | queue kind |
| `looprig.harness.telemetry.record.dropped` | Int64Counter, `{record}` | reason |

`event.count` includes the number of `TokenDelta` events without creating one
record per chunk. Queue gauges are added only where a race-free owner already
knows the current value; instrumentation must not traverse mutable queues merely
to observe them.

Alerts remain outside Harness. A backend can alert on these measurements—for
example tool-error rate, p95 inference duration, checkpoint failures, stuck gate
count, or command queue latency—without Harness encoding organizational policy.

## Command and event occurrence records

Every concrete command and event must be present in an exhaustive mapping test.
The mapping yields a stable, low-cardinality record name such as:

```text
looprig.harness.command.user_input
looprig.harness.command.interrupt
looprig.harness.event.turn_started
looprig.harness.event.turn_done
looprig.harness.event.tool_call_completed
looprig.harness.event.session_idle
```

Command records are emitted once after dispatch succeeds or with a
`dispatch_error` outcome if it fails. Event records are emitted after successful
publication; an append/publication failure emits a separate
`looprig.harness.event.publish_failed` record with the intended event name and a
low-cardinality `error.type`.

`EventDetail` controls volume:

```go
type EventDetail uint8

const (
    EventsEnduring EventDetail = iota // default
    EventsLifecycle                  // enduring + tool/gate lifecycle
    EventsAll                         // includes TokenDelta records; diagnostics only
)
```

All modes update aggregate counters. `EventsAll` is explicit and unsuitable for
normal production because streaming chunks can be numerous. Even in
`EventsAll`, chunk content is never attached.

Records carry the occurrence timestamp from the domain header when present, not
the later export time. Record names never contain dynamic values.

## Attribute model

### Reused OTel attributes

Where semantics match, use the pinned GenAI/general attributes:

- `gen_ai.operation.name`
- `gen_ai.conversation.id` for Harness session ID on spans/records only
- `gen_ai.agent.name`
- `gen_ai.provider.name`
- `gen_ai.request.model` and `gen_ai.response.model`
- `gen_ai.request.stream`
- `gen_ai.output.type`
- `gen_ai.tool.name` and `gen_ai.tool.call.id` on spans/records only
- `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, cache, and reasoning
  attributes when authoritative
- `server.address` and `server.port` when already available and policy permits
- `error.type` for a bounded typed error classification

### Looprig attributes

Harness-specific attributes use the `looprig.*` namespace:

```text
looprig.loop.id
looprig.loop.engine
looprig.turn.id
looprig.turn.index
looprig.step.id
looprig.step.index
looprig.command.id
looprig.command.name
looprig.event.id
looprig.event.name
looprig.event.class
looprig.event.scope
looprig.event.journal_sequence
looprig.tool.execution.id
looprig.agency
looprig.outcome
looprig.checkpoint.trigger
looprig.checkpoint.consistency
looprig.structured_output.enabled
```

UUIDs, command/event/tool IDs, turn indexes, and journal sequences appear only on
spans or occurrence records. They are forbidden metric attributes.

### Cardinality policy

`MetricLabelPolicy` is deny-by-default for consumer-controlled strings:

- command/event names, enum outcomes, engine, gate kind, and event class/scope
  are closed bounded sets and safe;
- provider and model labels are admitted only through the bounded runtime model
  configuration and can be collapsed to `other` by policy;
- arbitrary external tool names, agent names, Hustle names, URLs, workspace
  paths, file names, error messages, IDs, and user labels are not metric
  attributes;
- an application may explicitly allowlist known tool/model/agent names, with all
  other values mapped to `other`.

The telemetry package tests every metric instrument against an attribute
allowlist. Runtime code cannot add one-off metric labels.

## Content and privacy policy

The default is `ContentNone`:

- no system instructions or prompt text;
- no input/output messages or thinking blocks;
- no tool schemas, arguments, summaries, result previews, or results;
- no permission question/choices, gate reason, or user input;
- no URLs, headers, request/response bodies, file paths, shell commands, or
  environment values;
- no raw error message or stack trace.

`ContentMetadata` may add bounded counts, block/media types, sizes, and SHA-256
digests. It still exports no raw semantic content.

Raw GenAI content attributes such as `gen_ai.input.messages`,
`gen_ai.output.messages`, `gen_ai.system_instructions`, tool arguments, and tool
results remain unsupported by built-in policy in the first release. A future
explicit content hook may return already-redacted attributes, but Harness must
not offer a convenient `ContentFull` switch that silently exports secrets or
PII.

Errors are classified with typed, low-cardinality codes. Raw error strings stay
in local `slog` diagnostics under the application's logging policy, not OTel
metrics or default records.

## Error and outcome semantics

- Unexpected provider, tool, persistence, checkpoint, restore, and validation
  failures set span status `Error` and `error.type`.
- User denial is `looprig.outcome=denied` with unset span status.
- User interruption/cancellation is `interrupted`/`canceled`; it is not an OTel
  error unless a typed infrastructure failure caused it.
- Deadline exceeded is `error.type=timeout` and status `Error` when the deadline
  represents an operation failure.
- A structured-output validation failure is an inference/step error with a
  bounded structured-output reason, never the malformed output.
- Command journal append is audit-only for ordinary commands: its append span can
  fail while command dispatch succeeds. The record must preserve both outcomes.
- Required event/gate/checkpoint append failures are errors and retain their
  fail-secure runtime behavior.
- Telemetry API/emitter failure is swallowed after incrementing a locally
  available dropped-record counter when possible. It never changes these domain
  outcomes.

## Native loops, foreign loops, and Hustles

Native turns receive full turn/step/inference/tool instrumentation.

Foreign primary turns receive the same `invoke_agent` turn span around the
foreign backend's turn execution. Driver or process/network instrumentation is a
child concern of the foreign-loop module. The known foreign-loop absence of
`LoopIdle` means a foreign primary session does not currently emit
`SessionIdle`; this does not prevent its turn span from ending at `TurnDone` or
another terminal. Session-idle pending age and abandonment remain visible as
lifecycle telemetry rather than silently appearing successful.

Hustles remain session-oriented Harness operations. A Hustle run gets a
`harness.hustle.run` span and a child logical inference span around
`inference.Client.Invoke`. This instrumentation does not make Hustles a generic
eval mechanism.

## Semantic-convention versioning

The current OTel GenAI spans, metrics, tools, and content attributes are marked
development and have changed across semantic-convention releases. Harness must:

1. Pin one OTel semconv Go package revision. The initial implementation uses
   the already-vendored `semconv/v1.41.0` with OTel Go API v1.44 and must not
   mix it with the also-vendored `semconv/v1.37.0`.
2. Centralize every semconv key, metric, and span mapping in `pkg/telemetry`.
3. Record the schema URL/instrumentation scope revision where supported.
4. Never mix two GenAI convention revisions in one default emission path.
5. Treat a convention upgrade as a reviewed telemetry-schema migration with
   golden tests and release notes.
6. Keep `looprig.*` custom names stable and version their breaking changes.

This design uses GenAI concepts such as `invoke_agent`, `execute_tool`, client
operation duration, token usage, and time to first chunk, but does not promise
that today's development names will never change.

## Integration with eval and alerting

Harness does not import eval. A future read-only Harness eval adapter consumes
public conversation/events and may export its own evaluation telemetry through
`evalotel`.

Synchronous evaluation can inherit the current trace context. Continuous eval
usually runs after a turn span has ended, so it starts a separate trace and
correlates using session/turn IDs; Span Links can be added later when an
in-memory causal context is available. Eval scores are not Harness metrics and
Harness does not write them to the journal.

Grafana, Datadog, or another telemetry backend owns alert expressions and
notification routing. Harness and eval provide measurements and findings only.

## Runtime overhead and failure isolation

- Instruments are constructed once per Rig telemetry instance.
- No exporter, batching queue, retry loop, or background worker lives in Harness.
- No-op instrumentation performs no content projection and no JSON encoding.
- Attribute slices use bounded, reusable construction paths where safe.
- Token deltas update only aggregate inference state unless `EventsAll` is
  explicitly enabled.
- Telemetry never holds runtime locks while calling a user-provided record
  emitter.
- Async emitters must own/copy the bounded record passed to them.
- A panicking custom record emitter is recovered at the telemetry boundary,
  classified locally, and cannot poison a loop actor.

## Testing strategy

### Unit tests

- No-op behavior produces no spans, records, metrics, allocations from content
  projection, or goroutines.
- Every command and event concrete type maps to exactly one stable record name or
  explicit suppression policy.
- Unknown future command/event types fail the exhaustive mapping test.
- Span helpers set required names, kinds, attributes, outcomes, status, and end
  exactly once.
- Metric helpers use only approved low-cardinality attributes and correct units.
- Typed errors map to bounded `error.type` without including error strings.
- Default content policy rejects all raw conversation/tool/permission fields.
- Correlation-table entries resolve and clean up on every terminal path.
- Panicking/failing record emitters do not affect runtime results.

### Integration tests

Use OTel in-memory test SDK exporters under build-independent tests to verify:

- create → submit → turn → step → inference → tool → terminal trace shape;
- parent propagation from caller through fire-and-forget submit;
- lower-layer HTTP/provider spans become inference children without duplicates;
- provider error, tool error, denial, interrupt, and persistence failure status;
- journal append and command audit-only failure semantics;
- native and foreign turn completion;
- Hustle run/inference trace shape;
- metrics agree with emitted operations under concurrent sessions;
- no UUID or arbitrary tool/user string appears on a metric datapoint.

### Race and performance tests

- Run all instrumentation tests with `go test -race`.
- Benchmark disabled, sampled-out, and recording paths separately.
- Assert bounded correlation-table cleanup under canceled/queued submissions.
- Compare a representative turn with no-op telemetry against the pre-instrumented
  baseline; investigate added allocations in token-delta and tool hot paths.

## Delivery phases

### Phase 1 — foundation and critical path

- `pkg/telemetry` construction, no-op, attributes, error/outcome mapping.
- Turn, step, logical streaming inference, and tool spans.
- GenAI duration/token/first-chunk metrics.
- Core Harness turn/step/tool duration metrics.
- Default-safe content/cardinality policies.

### Phase 2 — lifecycle, commands, events, and persistence

- Session create/restore/shutdown, command dispatch, gate, journal,
  checkpoint/restore, and compaction spans/metrics.
- Exhaustive command/event occurrence mapping and OTel log adapter.
- Active-operation and queue-depth instruments.
- Fire-and-forget causal-context propagation.

### Phase 3 — Hustles, foreign loops, and integration hardening

- Hustle instrumentation.
- Foreign-loop turn spans and lifecycle-gap telemetry.
- Cross-module parent/child verification with inference HTTP instrumentation.
- Semconv migration tests and operational dashboards/examples.

### Phase 4 — eval composition

- Read-only continuous-eval correlation guidance.
- Optional Span Links for asynchronous evals if real consumers require them.
- `evalotel` score/finding export remains in the eval module.

## Acceptance criteria

- Harness runs unchanged with no OTel SDK configured.
- Harness imports OTel APIs and pinned semantic conventions, not SDK/exporter
  packages in production code.
- A completed native turn shows an agent span with step, logical inference, tool,
  and required persistence children.
- Time to first chunk, inference duration, tokens, tool duration, gate wait,
  journal latency, and turn duration are independently observable.
- Every command/event type is counted and mapped; high-volume `TokenDelta`
  records are suppressed by default but counted.
- No metric contains session, loop, turn, step, command, event, tool execution,
  URL, path, or arbitrary user-defined IDs/strings.
- Default traces and records contain no prompt, conversation, tool arguments,
  tool results, permission text, URLs, environment, secrets, or raw error text.
- Telemetry failures and emitter panics never alter command, turn, tool,
  persistence, or session outcomes.
- Foreign turns close at their terminal even while the known `SessionIdle` gap
  remains visible.
- The application can export the signals through its chosen OTel SDK/backend and
  define alerts without a Harness change.
