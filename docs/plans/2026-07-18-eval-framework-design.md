# Reusable Evaluation Framework — Design Specification

**Date:** 2026-07-18
**Status:** Approved design
**Target module:** `github.com/looprig/eval`
**Companion:** [Model Power Quality Test](2026-07-18-model-power-quality-test-design.md)

## Summary

Create `github.com/looprig/eval` as an application-neutral Go evaluation module.
It uses Go tests for local execution, comparison, failure reporting, parallelism,
and CI integration, while adding the domain vocabulary Go's `testing` package
does not provide: conversations, expectations, operational evidence, evaluators,
rubrics, findings, measurements, reports, and sinks.

The module supports two evaluation lifecycles:

- **Qualification evaluation** runs before deployment against fixtures, golden
  sets, generated cases, models, agents, HTTP endpoints, or local processes.
- **Continuous evaluation** observes completed turns or sessions in production,
  asynchronously and without changing the active conversation.

Both lifecycles use the same observation and assessment contracts. Evals only
observe, score, report, and propose golden-set candidates. They do not authorize,
block, rewrite, retry, or otherwise act on a session. Future classifiers may
consume eval findings, but classification and action remain separate concerns.

This specification supersedes the narrow `harness/pkg/eval` design in
`2026-06-15-togo-prompt-eval-framework-design.md`.

## Goals

- Make evals ordinary Go code runnable with `go test`, including `t.Run`,
  `t.Parallel`, build tags, benchmarks, fuzzing, test caching controls, and CI.
- Represent an agent interaction as `content.AgenticMessages`, preserving text,
  multimodal blocks, tool requests, tool results, errors, and usage.
- Support deterministic, model-based, and composite evaluation.
- Evaluate semantic quality and operational behavior with one reporting model.
- Work independently of Harness while offering an optional read-only Harness
  adapter.
- Support active qualification tests against inference clients, HTTP services,
  and sandboxed processes.
- Emit portable reports to Go tests, JSON, OpenTelemetry, and user-supplied
  sinks without selecting a monitoring or alerting vendor.
- Make missing evidence and unavailable enforcement explicit as `unverified`,
  never as a passing score.
- Keep judge output typed and machine-validated through inference structured
  output.

## Non-goals

- Taking action in a live loop or session.
- Replacing permission gates, policy engines, classifiers, or guardrails.
- Writing eval output to the Harness journal.
- Owning alert thresholds or alert delivery.
- Selecting an OpenTelemetry SDK, exporter, collector, or backend.
- Persisting an eval database in the core module.
- Providing a hosted evaluation service in the first release.
- Claiming open-world truth from conversation text alone.

## Design principles

1. **Observation is not control.** An evaluator reports evidence and
   assessments. A different component decides what to do.
2. **Conversation is canonical.** Input, output, steps, and tool calls are not
   duplicated into eval-specific string fields.
3. **Evidence precedes judgment.** Use exact and programmatic checks whenever
   possible; reserve model judgment for semantic ambiguity.
4. **Unknown is not pass.** Missing context, unsupported sandbox guarantees,
   unavailable judges, and incomplete telemetry remain visible.
5. **Core is application-neutral.** Harness, sandbox, proxy, and inference
   integrations live in adapters.
6. **Reports are data.** Grafana, Datadog, CI, or an organization-specific
   system decides whether a measurement warrants an alert.
7. **Version everything that changes meaning.** Evaluators, rubrics, judge
   models, prompts, schemas, datasets, targets, and policies are report inputs.

## Module and package layout

```text
github.com/looprig/eval
├── eval.go                 # core observation/evaluator/report contracts
├── run.go                  # orchestration, cancellation, concurrency
├── rubric/                 # built-in rubric definitions
├── judge/                  # inference.Client-backed structured-output evaluator
├── evaltest/               # testing.TB integration and assertions
├── dataset/                # JSONL/JSON codecs and dataset validation
├── target/inference/       # active inference.Client target
├── target/http/            # active HTTP target
├── target/process/         # active process target contracts
├── adapter/harness/        # read-only Harness observation adapter
├── adapter/sandbox/        # sandbox guarantees and execution evidence
├── adapter/proxy/          # recording-proxy evidence mapping
└── evalotel/               # OTel instruments and report sink
```

Dependency direction:

```text
core/content  <-  eval core  <-  evaltest, dataset, adapters, targets
inference     <-  judge and target/inference
harness       <-  adapter/harness
sandbox       <-  adapter/sandbox
OTel API      <-  evalotel
```

The eval root imports `github.com/looprig/core/content` and the standard
library. It does not import Harness, sandbox, OpenTelemetry, or a provider SDK.
The judge package imports the provider-neutral `inference.Client` directly.
Hustles remain session-oriented Harness features and are not an eval dependency.

## Core domain model

### Observation

```go
type Observation struct {
    Conversation content.AgenticMessages
    Scope        Scope
    Subject      Subject
    Trace        Trace
    Expectation  *Expectation
}

type Scope uint8

const (
    ScopeCase Scope = iota
    ScopeTurn
    ScopeSession
    ScopeRun
)

type Subject struct {
    ID       string
    Kind     SubjectKind
    Name     string
    Revision string
}
```

`Conversation` is the semantic record. A new shared content type is added to
`core/content` only when it is generally true of agentic content, not merely
useful to eval. Eval-specific lifecycle, timing, provenance, and expected-result
types stay in eval.

`Trace` holds facts that are not already present in the conversation:

```go
type Trace struct {
    TraceID       string
    SessionID     string
    TurnID        string
    StartedAt     time.Time
    EndedAt       time.Time
    Model         Revision
    Prompt        Revision
    MessageRanges []MessageRange
    Operations    []Operation
    Evidence      []Evidence
}
```

An `Operation` describes an inference call, tool execution, network request,
process action, sandbox decision, or other timed step. It contains typed status,
timestamps, parent/child correlation, safe attributes, and an error
classification. It must not become a second transcript.

`Expectation` is optional qualification data, such as required facts, forbidden
actions, reference answers, expected tool calls, expected schemas, or a policy
reference. Production observations normally omit it.

### Scenario, target, and sample

An active qualification case separates the test description from execution:

```go
type Scenario struct {
    ID          string
    Name        string
    Revision    string
    Input       content.AgenticMessages
    Expectation *Expectation
    Labels      []Label
}

type Target interface {
    Name() string
    Observe(context.Context, Scenario) (Observation, error)
}

type Sample struct {
    Scenario    *Scenario
    Observation Observation
}
```

Continuous eval already has an observation, so it constructs a `Sample` without
executing a target. Targets may wrap an `inference.Client`, an agent entry point,
an HTTP endpoint, or a local process. Active execution is therefore available
without coupling the evaluator API to a particular runtime.

### Evaluators

```go
type Evaluator interface {
    Descriptor() Descriptor
    Evaluate(context.Context, Sample) (Assessment, error)
}

type Descriptor struct {
    Name        string
    Revision    string
    Method      Method
    Description string
    Requires    []EvidenceKind
}

type Method uint8

const (
    MethodProgrammatic Method = iota
    MethodModel
    MethodComposite
)
```

`Method` is descriptive metadata that supports filtering, cost accounting, and
reporting. The `Evaluator` interface remains small. Composite evaluators call
named component evaluators and disclose those components in their descriptor.

```go
type Assessment struct {
    Evaluator   string
    Revision    string
    Status      AssessmentStatus
    Measurements []Measurement
    Findings    []Finding
    Evidence    []Evidence
    Duration    time.Duration
}

type Measurement struct {
    Name  string
    Value float64
    Unit  Unit
}

type Finding struct {
    Code     string
    Severity Severity
    Message  string
    Evidence []EvidenceRef
}
```

Assessment status is one of `pass`, `fail`, `unverified`, `error`, or `skipped`.
An assessment can carry several measurements and findings; a forced scalar
score would lose important operational and security information.

`Evidence` is a tagged, typed union with stable kinds for conversation excerpts,
message indexes, tool calls/results, timings, model usage, HTTP/DNS activity,
process execution, file access, sandbox guarantees, structured-output errors,
and evaluator diagnostics. Sensitive payloads are represented by hashes,
classifications, byte counts, or redacted excerpts unless a secure detailed sink
is explicitly configured.

## Rubrics and judge schemas

A rubric defines **what good means**. A schema defines **the shape of the
judge's answer**. They are deliberately separate.

```go
type Rubric struct {
    Name       string
    Revision   string
    Scope      Scope
    Definition string
    Criteria   []Criterion
    Anchors    []Anchor
}
```

Built-in rubric families include:

- Response: answer relevance, groundedness, factual consistency, helpfulness,
  clarity, concision, civility, toxicity, and vulgarity.
- Agent turn: task progress, tool-choice quality, tool recovery, instruction
  adherence, and unsupported-action claims.
- Session: goal adherence, task completion, conversation consistency,
  unresolved commitments, intent preservation, and overall quality.

Truth-related names are precise:

- **Groundedness** asks whether claims are supported by supplied context or tool
  evidence.
- **Factual consistency** asks whether the answer contradicts known references.
- **Conversation consistency** asks whether turns contradict one another.
- **Open-world truthfulness** requires external verification and may be
  unverified when no authoritative evidence is available.

Most scalar rubrics share an internal versioned structured-output contract:

```go
type ScoreOutput struct {
    Score    float64
    Reason   string
    Evidence []QuotedEvidence
}

type QuotedEvidence struct {
    MessageIndex int
    Quote        string
}
```

`judge.ScoreSchemaV1` owns the JSON Schema and validates score range, evidence
indexes, bounded reason length, and quote provenance. Callers normally supply a
rubric, not a schema. Categorical classification, pairwise comparison, and issue
extraction use separate versioned schemas because they have different result
semantics.

The judge builds an `inference.Request` with structured output enabled and calls
`inference.Client` directly. A tool-call emulation may be a provider codec's
mechanism for structured output, but it is not the eval package contract. If a
model cannot satisfy the declared schema, the assessment is `error` or
`unverified`, never an inferred pass from free-form text.

## Choosing an evaluation method

Use programmatic evaluation for observable facts: exact output structure,
required strings or fields, tool names and arguments, tool order, error and
recovery sequences, latency, token use, process/file/network activity, canary
leakage, and sandbox guarantees.

Use model evaluation where meaning is genuinely ambiguous: helpfulness,
clarity, whether clarification was appropriate, open-ended goal progress,
proportional refusal, misleading claims, contextual hostility, and whether an
internet call was relevant to the task.

Use a composite evaluation when a semantic conclusion must be grounded in
operational facts. Instruction following, groundedness, tool quality, toxicity,
prompt-injection resistance, task completion, internet-use quality, and citation
quality are typical composites. A composite report retains every component
assessment so a judge score cannot conceal a deterministic failure.

## Execution engine

The engine processes:

```text
Scenario -> Target -> Observation -> Evaluators -> Report -> Sinks
```

It provides:

- bounded concurrency with context cancellation;
- configurable retries only for explicitly retryable target or judge failures;
- repeated trials for nondeterministic targets;
- stable input ordering and deterministic report ordering;
- per-target and per-evaluator timeouts;
- optional sampling for continuous eval;
- no default fail-fast behavior;
- descriptor and evidence requirement validation before execution;
- provenance for dataset, target, evaluator, rubric, judge, schema, and policy
  revisions.

Failures are separated by stage. A target error does not masquerade as a failed
quality score. One evaluator error does not discard successful sibling
assessments. Cancellation stops unstarted work and records partial completion.

### Suite matrices, baselines, and reproducibility

The runner can expand a suite across explicit target variables such as model,
prompt revision, tool set, sampling profile, or policy profile. Matrix expansion
produces ordinary scenarios with stable derived IDs; it is not a second runner.

A baseline report can be compared with a candidate report only when the report
declares compatible case and evaluator identities. Comparison retains added,
removed, changed, errored, and unverified cases rather than comparing averages
alone. Repeated trials expose mean, quantiles, variance, and per-case flakiness.

Caching is opt-in and content-addressed by the complete reproducibility input:
scenario, target, model/configuration, evaluator, rubric, judge, schema, and
relevant policy revisions. Live continuous observations are never served from
cache. Secrets are excluded from cache keys and cache values follow the same
redaction rules as reports.

Rate limiting and retry policy are injected at targets or judges because their
limits differ. The engine coordinates concurrency but does not guess provider
quotas. A retry remains visible in operational evidence and report cost.

### Relationship to existing evaluation frameworks

The first useful release should cover the facilities engineers expect from
DeepEval, Promptfoo, Braintrust, and similar tools without copying their Python
or YAML execution model:

- reusable metrics/evaluators and an LLM-as-judge;
- datasets, golden cases, generated cases, and versioned provenance;
- model/prompt/tool matrices and repeated trials;
- deterministic assertions, semantic rubrics, and composite metrics;
- concurrency, timeout, retry, rate-limit, and optional cache seams;
- baseline comparison, regressions, aggregate distributions, and reports;
- red-team and security packs built above the generic scenario/evidence API;
- CI exit status and readable per-case failure output.

Looprig's differentiators are native `go test` composition, a canonical typed
agentic conversation, first-class tool/process/network/sandbox evidence, and the
same assessment contracts for qualification and continuous production
observation. A hosted UI, shared experiment database, and large built-in metric
catalog can be added later without changing core contracts.

## Go test integration

The core package does not import `testing`. `evaltest` adapts reports to
`testing.TB` and makes each scenario and evaluator visible as a Go subtest.

```go
func TestSupportAgentQualification(t *testing.T) {
    t.Parallel()

    report := evaltest.Run(t,
        eval.Suite{Name: "support-agent", Scenarios: cases},
        inferenceeval.NewTarget(client, requestTemplate),
        exact.RequiredTool("lookup_account"),
        judge.New(rubric.AnswerRelevanceV1, judgeClient),
    )

    evaltest.RequireProfile(t, report, releaseProfile)
}
```

`evaltest.Run` owns subtest presentation but returns the complete report for
custom assertions. Expensive or networked cases use normal Go build tags such as
`integration` or `qualification`. Datasets can be embedded with `//go:embed`.
Benchmarks measure evaluator overhead or target latency. Fuzz tests exercise
parsers, codecs, and deterministic invariants rather than invoking a paid judge.

The package does not fight Go's test cache: documentation and helpers make
nondeterministic live runs use `-count=1`; deterministic golden tests remain
cacheable.

## Qualification and continuous examples

Qualification evaluation:

```go
func TestInvoiceAgentDoesNotInventRefund(t *testing.T) {
    scenario := eval.Scenario{
        ID:    "refund-policy-017",
        Input: conversation.User("Refund this non-refundable invoice"),
        Expectation: policy.Expect().ForbiddenAction("issue_refund"),
    }

    report := evaltest.RunScenario(t, scenario, agentTarget,
        exact.NoToolCall("issue_refund"),
        judge.New(rubric.InstructionAdherenceV1, judgeClient),
    )
    evaltest.RequirePass(t, report)
}
```

Continuous evaluation:

```go
observer := harnesseval.NewObserver(harnesseval.Config{
    Boundaries: []harnesseval.Boundary{
        harnesseval.TurnDone,
        harnesseval.SessionIdle,
    },
    Evaluators: []eval.Evaluator{
        judge.New(rubric.GoalAdherenceV1, judgeClient),
        exact.ToolErrorRate(),
    },
    Sampler: eval.SampleRate(0.05),
    Sink:    eval.MultiSink(jsonSink, otelSink),
})
```

The Harness adapter snapshots the public conversation and correlated operational
events at `TurnDone` or `SessionIdle`, queues evaluation outside the active loop,
and never writes to the session journal. Queue saturation, sampling, judge
failure, and sink failure are observable in eval's own telemetry and cannot
delay or fail the user turn.

## Harness telemetry boundary

Harness may add `pkg/telemetry` for first-party OpenTelemetry API
instrumentation around actual execution boundaries:

```text
session -> turn -> step/inference/tool -> optional eval/judge
```

It records duration histograms, request/error counters, token totals, active
operations, queue time, and tool failures. High-cardinality session, turn, and
tool-call IDs belong on traces or logs, not metric labels. Harness accepts
providers or uses OTel global no-op defaults; it does not configure exporters.

`adapter/harness` may consume these correlated facts, but it must not reconstruct
precise latency solely from asynchronous events when direct instrumentation is
available.

## Reports, storage, and alerting

```go
type Report struct {
    ID          string
    Suite       Revision
    Target      Revision
    StartedAt   time.Time
    EndedAt     time.Time
    Samples     []SampleReport
    Summary     Summary
    Provenance  Provenance
}

type Sink interface {
    WriteReport(context.Context, Report) error
}
```

Initial sinks are in-memory, JSON/JSONL, Go test, and OpenTelemetry. Consumers
can implement storage backends without a dependency from eval core. Runtime
reports should normally store redacted evidence and references to separately
controlled raw traces. Retention, encryption, regional placement, and access
control are deployment concerns.

The OTel sink emits low-cardinality measurements and failure counts. Detailed
findings are span events or logs. Eval does not create monitors or notification
channels. An operator can alert on signals such as a goal-adherence percentile,
tool-error rate, unverified-eval rate, or sudden evaluator-score distribution
change in Grafana, Datadog, or another backend.

## Golden-set extraction

Continuous eval may write a `Candidate` to an explicit candidate sink when a
sample is novel, low-scoring, operationally unusual, or selected by sampling.
Candidates are not automatically promoted to goldens. Promotion requires
review, redaction, stable expectations, provenance, and a durable case ID.

This creates the feedback loop missing from design-only frameworks:

```text
production observation -> candidate -> human review -> golden case
-> qualification suite -> future release comparison
```

## Validation and security

- Validate dataset and report JSON at the boundary with bounded sizes.
- Reject duplicate scenario, evaluator, finding, and measurement identities
  where ambiguity would corrupt comparison.
- Require finite numeric measurements and valid units.
- Bound judge prompts and outputs; validate every structured response.
- Treat conversation, tool output, proxy records, and judge explanations as
  untrusted data.
- Never place secrets, full prompts, raw message text, URLs with credentials, or
  PII in metric labels.
- Redact before external sinks and preserve evidence hashes for correlation.
- Require explicit opt-in for raw-content sinks.
- Keep evaluator errors and target errors typed and stage-specific.

## Migration from `harness/pkg/eval`

The current package provides useful seeds—case loading, a deterministic
contains metric, a judge seam, and a simple runner—but its string-only case
model, scalar scores, free-text judge parsing, sequential fail-fast execution,
and Harness ownership are intentionally replaced.

Migration steps:

1. Build the new module and core contracts without changing Harness.
2. Re-express `Contains` as a programmatic evaluator over conversation text.
3. Replace free-form judge parsing with structured inference output.
4. Move JSON cases to the versioned dataset codec.
5. Add `evaltest` and migrate existing eval tests to normal Go subtests.
6. Add optional Harness and OTel adapters.
7. Remove `harness/pkg/eval` only after all consumers migrate.

## Acceptance criteria

- A consumer with no Harness dependency can run a deterministic suite through
  `go test`.
- A consumer can judge an `AgenticMessages` conversation using an injected
  `inference.Client` and a versioned rubric.
- One suite can combine scalar measurements, findings, and evidence from
  programmatic and model evaluators.
- An active target can attach process, network, and sandbox evidence.
- Missing required evidence produces `unverified`.
- Harness continuous eval runs asynchronously at turn/session boundaries and
  never changes the journal or active conversation.
- Reports can be rendered in Go tests, serialized, and exported through OTel.
- Alert policy remains outside the eval module.
