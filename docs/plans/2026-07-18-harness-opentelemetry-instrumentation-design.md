# Harness OpenTelemetry Instrumentation — Design Specification

**Date:** 2026-07-18
**Revised:** 2026-08-14 — GenAI semantic-convention repository split, opt-in
message-content tier, and LLM-observability backend compatibility.
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

Two consumer classes read this signal. Operational backends such as Grafana,
Datadog, or a bare OTLP collector need only the metadata-only default: span
shape, durations, token counts, and bounded error classifications. LLM-trace
viewers such as Arize Phoenix, LangSmith, and Langfuse are content-first; with
metadata only they render correctly shaped but empty spans. Harness therefore
also defines an explicit, redacted, off-by-default message-content tier and a
backend-compatibility contract, without making raw content easy to enable by
accident.

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
conventions remain under active development and were moved out of the core
convention repository in semconv v1.42.0 (June 2026), so this design pins their
revision and isolates their use behind Harness-owned helpers:

- <https://opentelemetry.io/docs/languages/go/instrumentation/>
- <https://opentelemetry.io/docs/specs/semconv/general/events/>
- <https://github.com/open-telemetry/semantic-conventions-genai> — the current
  home of the GenAI conventions; the `docs/gen-ai` tree in
  `open-telemetry/semantic-conventions` is deprecated as of v1.42.0.

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
- Make a Harness trace usable in an LLM-trace backend (Phoenix, LangSmith,
  Langfuse, Arize AX) once an application explicitly opts into redacted content,
  without teaching Harness any vendor's schema.
- Support native loops, foreign loops, Hustles, headless use, HTTP composition,
  persistence, and future eval adapters.
- Make telemetry failures incapable of failing or delaying Harness work.

## Non-goals

- Configuring an OTel SDK, OTLP exporter, collector, backend, or dashboard.
- Defining Grafana, Datadog, or other vendor alert policies.
- Emitting vendor attribute schemas — OpenInference, OpenLLMetry/Traceloop,
  Logfire, `langsmith.*`, `langfuse.*` — or offering a per-backend emission mode.
- Redacting content on the application's behalf, or shipping a default redactor.
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
  ├── content redactor (only if the content tier is enabled)
  ├── collector vendor mapping (only for a backend that needs one)
  └── telemetry.Config
          |
          v
  harness/pkg/telemetry
    ├── Tracer + span helpers
    ├── Meter + instruments
    ├── RecordEmitter (command/event occurrence records)
    ├── genai (pinned GenAI name binding)
    ├── attribute/cardinality policy + complex-value encoder
    └── content policy (none by default; redacted messages on opt-in)
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
    Redactor        ContentRedactor // required when Content == ContentMessages
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

OTel Go traces and metrics are stable while its logs API is still pre-stable —
`go.opentelemetry.io/otel/log` is v0.21.0 alongside API v1.45.0. To avoid
spreading an experimental API across Harness, the core instrumentation depends on a
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
separately. Tool arguments and results are not span attributes by default; under
`ContentMessages` they carry redactor-produced `gen_ai.tool.call.arguments` and
`gen_ai.tool.call.result`.

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

### Complex attribute encoding

`gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions`,
`gen_ai.tool.definitions`, `gen_ai.tool.call.arguments`, and
`gen_ai.tool.call.result` are declared as `any`-typed structured values in the
conventions, but the OTel Go attribute model carries only scalars and scalar
slices. There is no supported way to attach a nested structure through
`attribute.KeyValue`.

Harness therefore encodes these as a single JSON string attribute using the
convention's own message schema (`role` plus a `parts` array with typed part
objects). That is what every backend below already parses, and it keeps one
encoding across the whole content tier. `pkg/telemetry` owns the encoder,
enforces a configured byte budget per attribute, and marks truncation with a
bounded `looprig.content.truncated` flag rather than emitting a partial JSON
document. When the OTel Go API grows first-class complex attribute values, the
encoder is the single place that changes.

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
looprig.content.tier
looprig.content.truncated
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

### Content tiers

```go
type ContentPolicy uint8

const (
    ContentNone     ContentPolicy = iota // default
    ContentMetadata                      // counts, types, sizes, digests
    ContentMessages                      // redacted conversation content; opt-in only
)
```

`ContentMessages` exists because an LLM-trace backend is unusable without it: a
metadata-only trace shows correct span shape, timing, and token counts with an
empty body, which is precisely the part those tools are bought for. Refusing the
tier does not make deployments safer; it makes them export raw content through a
side channel Harness cannot police.

The tier is constrained rather than convenient:

- `New` returns an error when `Content == ContentMessages` and `Redactor` is
  nil. There is no default redactor and no "full" pass-through value.
- Harness never emits raw content directly. It projects the turn into the
  convention's message shape, hands it to the redactor, and emits only what the
  redactor returns.
- The redactor is application-owned:

  ```go
  type ContentRedactor interface {
      RedactMessages(context.Context, []Message) ([]Message, error)
      RedactToolCall(context.Context, ToolCall) (ToolCall, error)
  }
  ```

  `Message`/`ToolCall` are Harness-owned projections, not `core/content` blocks,
  so a redactor cannot accidentally reach provider state, credentials, or block
  internals it was never meant to see.
- A redactor error or panic drops the content for that span, increments the
  dropped-record counter, and leaves the span otherwise intact. It never fails
  the turn.
- Emitted attributes are `gen_ai.input.messages`, `gen_ai.output.messages`,
  `gen_ai.system_instructions`, `gen_ai.tool.call.arguments`, and
  `gen_ai.tool.call.result`, JSON-encoded per *Complex attribute encoding*, with
  a per-attribute byte budget.
- Spans carry `looprig.content.tier` so a backend can tell a metadata-only
  deployment from a redacted-content one instead of guessing from absence.
- Permission-gate question text, gate reason, raw error strings, URLs,
  environment values, shell commands, and file paths stay out of every tier,
  including `ContentMessages`. The tier covers conversation and tool payloads
  only.

The ecosystem gates this behavior through environment opt-ins
(`OTEL_SEMCONV_STABILITY_OPT_IN=gen_ai_latest_experimental` selects the current
convention generation; instrumentations additionally gate message capture behind
their own capture-content variable). Harness is a library and does not read
environment variables itself: the composition root decides, and the documented
example wires the same variables so a Harness application behaves like the rest
of a mixed fleet.

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
development and have changed across semantic-convention releases.

### The GenAI conventions left the core repository

Semantic conventions v1.42.0 (June 2026) deprecated every GenAI, OpenAI, and MCP
convention in `open-telemetry/semantic-conventions` and moved them to
`open-telemetry/semantic-conventions-genai`. The generated Go packages followed:
`go.opentelemetry.io/otel/semconv/v1.41.0` ships a `genaiconv` package and the
full `gen_ai.*` key set, while `v1.42.0` and `v1.43.0` contain none of it — the
v1.42.0 migration document lists 119 removed `GenAI*` declarations. The split is
organizational; the conventions remain pre-stable either way.

The consequence for this design is concrete: **`semconv/v1.41.0` is the last
otel-go semconv package that carries GenAI symbols, so there is no in-repo
upgrade path for them.** Harness must not stay pinned to v1.41.0 for *all*
semconv just to keep GenAI names, and must not silently lose them on the next
routine semconv bump.

### Rules

1. Split the pin. General semconv (HTTP, error, server, schema URL) tracks the
   current otel-go package and upgrades normally. GenAI names are pinned
   separately. Both are packages inside the one `go.opentelemetry.io/otel`
   module version, and otel-go retains historical semconv packages, so this is a
   package-selection decision rather than two conflicting module requirements.
2. Bind the GenAI names once, in `pkg/telemetry/genai`, as Harness-owned typed
   constants. The initial implementation derives them from `semconv/v1.41.0`
   (`genaiconv` metrics plus the `gen_ai.*` attribute keys) and records that
   provenance, so the package can later be re-sourced from the GenAI convention
   repository's own Go module — if one is published — without touching any
   call site.
3. Centralize every semconv key, metric, and span mapping in `pkg/telemetry`.
   Runtime packages never name a `gen_ai.*` string.
4. Record the schema URL and instrumentation scope revision where supported, and
   record the GenAI convention revision separately from the general one.
5. Never mix two GenAI convention revisions in one default emission path.
6. Treat a convention upgrade as a reviewed telemetry-schema migration with
   golden tests and release notes. A golden test pins the exact emitted attribute
   and metric names so a dependency bump that drops or renames a GenAI symbol
   fails the suite rather than silently changing the wire format.
7. Keep `looprig.*` custom names stable and version their breaking changes.

Current pinned versions: OTel Go API v1.45.0, general semconv v1.43.0, GenAI
names sourced from semconv v1.41.0. Harness has no `vendor/` tree; these are
`go.mod` pins verified by `go.sum`.

This design uses GenAI concepts such as `invoke_agent`, `execute_tool`, client
operation duration, token usage, and time to first chunk, but does not promise
that today's development names will never change.

## Backend compatibility

Harness emits OTel GenAI semantic conventions and `looprig.*`, and nothing else.
It does not emit OpenInference, OpenLLMetry/Traceloop, Logfire, or vendor-prefixed
attributes, and it does not grow a per-backend mode. Vendor schemas churn faster
than this module releases, a library that dual-emits doubles every span's
attribute cost for whichever consumer is not looking, and the non-goals above
already place exporters and backends in the composition root.

Translation is therefore a collector concern. The mapping below is what a
deployment needs to add; none of it is a Harness change.

| Backend | Ingest | Reads Harness output as-is | Deployment adds |
|---|---|---|---|
| Grafana / Datadog / any OTLP store | OTLP | Yes — spans, metrics, records | Dashboards and alerts only |
| Langfuse | OTLP **HTTP** only (protobuf or JSON; no gRPC), `/api/public/otel`, Basic auth from project keys | Yes — maps `gen_ai.*` onto its own model | `ContentMessages` for bodies; optional `langfuse.*` session/user attributes |
| LangSmith | OTLP at its `/otel` endpoint | Mostly — reads `gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.usage.*` | `langsmith.span.kind` for run type; `langsmith.trace.session_id` for threading — it does **not** thread on `gen_ai.conversation.id` |
| Arize AX | OTLP | Yes — normalizes any `gen_ai.*` span into OpenInference on ingest | Nothing |
| Arize Phoenix (OSS) | OTLP | **No** — Phoenix understands OpenInference only and does not recognize the OTel GenAI conventions | Full `gen_ai.*` → OpenInference transform in the collector |

Phoenix is the sharp edge: sending Harness spans straight at it produces spans
that arrive but do not populate its LLM views. The transform is mechanical —
`gen_ai.request.model` → `llm.model_name`, `gen_ai.usage.input_tokens` →
`llm.token_count.prompt`, `gen_ai.usage.output_tokens` →
`llm.token_count.completion`, `gen_ai.input.messages` → `input.value`,
`gen_ai.output.messages` → `output.value`, and the operation name to
`openinference.span.kind` (`chat` → `LLM`, `execute_tool` → `TOOL`,
`invoke_agent` → `AGENT`). Harness ships this as a reference collector config,
not as code:

```yaml
processors:
  transform/openinference:
    trace_statements:
      - context: span
        statements:
          - set(attributes["openinference.span.kind"], "LLM")
              where attributes["gen_ai.operation.name"] == "chat"
          - set(attributes["openinference.span.kind"], "TOOL")
              where attributes["gen_ai.operation.name"] == "execute_tool"
          - set(attributes["openinference.span.kind"], "AGENT")
              where attributes["gen_ai.operation.name"] == "invoke_agent"
          - set(attributes["llm.model_name"], attributes["gen_ai.request.model"])
          - set(attributes["llm.token_count.prompt"], attributes["gen_ai.usage.input_tokens"])
          - set(attributes["llm.token_count.completion"], attributes["gen_ai.usage.output_tokens"])
          - set(attributes["input.value"], attributes["gen_ai.input.messages"])
          - set(attributes["output.value"], attributes["gen_ai.output.messages"])
```

An application that cannot run a collector may register its own
`sdk/trace.SpanProcessor` to add vendor attributes on export. That is a
documented composition-root pattern, not a Harness feature, and it keeps the
vendor coupling in the binary that chose the vendor.

### Example composition root

The reference example lives in `docs/examples` and is exercised by a build-tagged
test so it cannot rot:

```go
exp, err := otlptracehttp.New(ctx) // endpoint/headers from OTEL_EXPORTER_OTLP_*
if err != nil {
    return err
}
tp := sdktrace.NewTracerProvider(
    sdktrace.WithBatcher(exp),
    sdktrace.WithResource(res),
)
defer tp.Shutdown(ctx) // the application owns shutdown; Harness never flushes

tel, err := telemetry.New(telemetry.Config{
    Providers: telemetry.Providers{Tracer: tp, Meter: mp},
    Content:   telemetry.ContentMessages, // opt-in; omit for metadata-only
    Redactor:  myRedactor,                // required by the line above
})
if err != nil {
    return err
}

r, err := rig.New(rig.WithTelemetry(tel))
```

The example documents the environment variables a mixed fleet already sets
(`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`,
`OTEL_SEMCONV_STABILITY_OPT_IN`) and shows the metadata-only default first, with
the content tier as a deliberate second step.

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
- `ContentMessages` without a redactor fails construction; content is emitted
  only as what the redactor returned, and a failing or panicking redactor drops
  the content without failing the span or the turn.
- Content JSON encoding matches the convention message schema, honors the byte
  budget, and sets `looprig.content.truncated` instead of emitting partial JSON.
- Gate text, raw error strings, URLs, paths, and environment values stay absent
  in every tier, including `ContentMessages`.
- A golden test pins every emitted GenAI attribute, metric, and span name so a
  semconv dependency bump that renames or drops a symbol fails the suite.
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
- no UUID or arbitrary tool/user string appears on a metric datapoint;
- with the content tier enabled, an exported turn carries exactly the five
  content attributes, each valid JSON in the convention message shape, each
  produced by the redactor, and the same run with the default policy carries
  none of them.

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

### Phase 1.5 — opt-in content tier and backend compatibility

- `ContentMessages`, the `ContentRedactor` contract, and the JSON attribute
  encoder with byte budget and truncation marking.
- Content emission on inference and tool spans only, under the redactor.
- `pkg/telemetry/genai` name binding plus the golden semconv-name test.
- Reference collector configs (pass-through, LangSmith, Phoenix/OpenInference)
  and the tested `docs/examples` composition root.

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
- `ContentMessages` cannot be enabled without an application redactor, emits only
  redactor output, and is visible on the span as `looprig.content.tier`.
- A turn exported with the content tier enabled renders complete input/output
  messages in Langfuse, LangSmith, and Arize AX with no Harness change, and in
  Phoenix through the documented collector transform alone.
- Harness emits no vendor attribute namespace, and a semconv bump that drops or
  renames a GenAI symbol fails the golden test rather than changing the wire
  format silently.
- Telemetry failures and emitter panics never alter command, turn, tool,
  persistence, or session outcomes.
- Foreign turns close at their terminal even while the known `SessionIdle` gap
  remains visible.
- The application can export the signals through its chosen OTel SDK/backend and
  define alerts without a Harness change.
