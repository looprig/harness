# Hustle — Harness-Owned Auxiliary Work

**Date:** 2026-07-11

**Last revised:** 2026-07-11 (adds the immutable hustle registry, typed adapters
over a serialized execution boundary, internal event visibility, loop kinds,
common audit lifecycle, blocking/background participation, near-term use cases,
and prompt-cache TODO)

**Status:** Draft

**Depends on:**

- `docs/plans/2026-07-10-rig-lifecycle-workspace-snapshots-design.md`
  (**Approved**) — rig composition, loop definitions, native lifecycle hooks,
  quiescence, restore, and workspace snapshots.
- `docs/plans/2026-06-19-event-persistence-checkpoint-design.md` — durable
  journal, event classes, replay, and catalog projection.
- `docs/plans/2026-07-11-token-usage-context-occupancy-design.md` — per-loop
  usage, loop kinds, and compaction as the first blocking loop-backed hustle.
- `docs/plans/2026-07-11-structured-output-design.md` — constrained output
  required by classifier hustles.

**New dependencies:** none.

---

## Motivation

Harness repeatedly needs to accomplish a small sidequest whose result informs a
larger decision without becoming a turn in the user/agent conversation:

- summarize active context before it exceeds a model window;
- classify whether a command or gate can be auto-approved;
- detect prompt injection or malicious instructions in fetched/tool content;
- derive a short session title;
- recap a stale session before resuming it; or
- produce a compact session summary for display/export.

These operations share lifecycle, attribution, isolation, model selection,
timeouts, audit, and resource controls. They do not share one domain input or
output type. A generic `Hustle[In, Out any]` would introduce untyped generic
constraints contrary to repository rules and would not itself solve lifecycle
or audit.

## Decision

Add a small public contract package plus an internal runtime:

```text
pkg/hustle
    Immutable definitions, names, execution envelopes, backend contract,
    typed errors, limits, and result metadata used at composition boundaries.

internal/hustleruntime
    Per-session registry, invocation lifecycle, loop-backed execution,
    audit events, timeouts, cancellation, quiescence participation,
    internal event visibility, concurrency limits, and cleanup.
```

The mechanism remains harness-controlled:

- consumers may register immutable definitions at `rig.Define`;
- only trusted harness subsystems invoke a registered hustle;
- there is no public arbitrary `Session.RunHustle` method in this version; and
- the caller consumes a typed result and owns the resulting policy/action.

Compaction is implemented now. The other named uses shape validation and
isolation contracts and are recorded as planned TODOs, not all implemented in
the first change.

## Vocabulary

- A **hustle definition** is an immutable design-time recipe for one auxiliary
  task.
- A **hustle invocation** is one session-scoped execution with a unique run ID
  and direct cause.
- A **hustle backend** performs the work. Initial backend kinds are loop, API,
  and function.
- A **loop-backed hustle** uses normal model/turn/step machinery but has
  `LoopKindHustle` and internal-only events.
- A **typed adapter** exposes a domain interface such as `Compactor` or
  `GateClassifier`, validates typed input/output, and invokes the generic
  runtime through its explicit serialization boundary.
- A **consumer** is the trusted harness subsystem that requested the hustle and
  decides what to do with its result.

## Core invariants

1. **Harness-owned.** Models, primer loops, delegates, and other hustles cannot
   enumerate, invoke, message, or await hustles.
2. **Not delegation.** Hustles have no parent-agent relationship, do not appear
   in `loop.WithDelegates`, and do not consume delegation depth/quota.
3. **Typed business logic.** Domain callers use focused typed interfaces. Raw
   bytes exist only at the generic serialization boundary and are immediately
   narrowed and validated.
4. **Caller-owned action.** A verdict is evidence, not authority. The caller
   checks policy/security ceilings before acting.
5. **Usage ownership.** A loop-backed hustle owns its request usage. No usage is
   rolled into the loop it assists.
6. **Internal visibility.** Hustle detail journals for audit but never fans out
   through the public event stream.
7. **No session hooks.** Hustle loops are structurally excluded from context
   measurement/compaction, workspace snapshot triggers, and future normal-loop
   hooks.
8. **No partial product.** A caller commits an enduring product event only after
   successful hustle completion and validation.
9. **Restore re-runs.** An interrupted hustle is never resumed as a loop. Its
   caller re-evaluates the trigger.
10. **Bounded execution.** Every invocation has a timeout, input/output limit,
    cancellation path, and per-session concurrency accounting.

---

## §1 · Immutable definitions and rig registry

Near-term uses justify including the registry now rather than deferring it:

```go
package hustle

type Name string

type Participation uint8

const (
	ParticipationUnknown Participation = iota
	ParticipationBlocking
	ParticipationBackground
)

type Definition struct {
	// fields unexported; built by Define
}

func Define(opts ...Option) (Definition, error)
```

Initial options:

```text
WithName(name)
WithBackend(backend)
WithTimeout(duration)
WithMaxInputBytes(bytes)
WithMaxOutputBytes(bytes)
WithParticipation(participation)
WithSecurityProfile(profile)
```

Definitions are immutable, defensively copied, and secret-free except for the
already-wired backend capability. Required validation:

- non-empty unique name;
- non-nil backend;
- positive resolved timeout;
- positive, bounded input/output limits;
- known participation mode;
- a security profile no broader than the rig/session ceiling; and
- backend-specific validation performed without session I/O.

The rig registers definitions:

```go
rig.WithHustles(compaction, gateSafety, fetchInjection, sessionTitle)
rig.WithHustleLimits(rig.HustleLimits{
	Concurrent: 4,
	Queued:     32,
})
```

`WithHustles` is additive and rejects duplicate names atomically. An unreferenced
definition is allowed because metadata/background consumers may invoke it after
session creation. Limits are independent of delegation limits.

The rig fingerprint includes each definition's stable name, backend kind,
security/limits revision, and participation mode, but never credentials or raw
prompts.

## §2 · Generic execution boundary without `any`

The generic runtime transports versioned JSON at an explicit serialization
boundary:

```go
package hustle

type RunID uuid.UUID

type Invocation struct {
	RunID     RunID
	SessionID uuid.UUID
	Name      Name
	Cause     identity.Cause
	Input     json.RawMessage
}

type Result struct {
	Output json.RawMessage
	Usage  *content.Usage
}

type Backend interface {
	Execute(context.Context, Invocation) (Result, error)
}
```

`Invocation.Input` and `Result.Output` are the only intentionally untyped data.
They are size-checked before decoding, use a versioned wire shape per hustle,
reject unknown fields, and are immediately converted into typed domain structs.
They are never passed deeper into policy/business logic.

Every domain consumer remains focused:

```go
type Compactor interface {
	Compact(context.Context, CompactionInput) (CompactionOutput, error)
}

type GateClassifier interface {
	Classify(context.Context, GateClassificationInput) (
		GateClassification,
		error,
	)
}

type SessionTitler interface {
	Title(context.Context, SessionTitleInput) (SessionTitle, error)
}
```

The adapter owns marshal → invoke → unmarshal → validate. Callers never switch
on generic payload maps.

## §3 · Backend kinds

```go
type BackendKind uint8

const (
	BackendUnknown BackendKind = iota
	BackendLoop
	BackendAPI
	BackendFunc
)
```

Every backend implements the same `Backend` contract and reports a stable kind
for definition validation/audit.

### Loop backend

A loop backend builds a restricted `LoopSpec`, invokes
`internal/hustleruntime.RunLoop`, and converts its terminal `AIMessage` into the
generic result. It reuses inference streaming, usage capture, structured output,
and event codecs.

```go
type LoopSpec struct {
	Model           inference.Model
	System          string
	Messages        content.AgenticMessages
	Tools           []inference.Tool
	Output          *inference.OutputSchema
	MaxOutputTokens content.TokenCount
	Security        SecurityProfile
}

type RunLoop interface {
	RunLoop(context.Context, LoopInvocation) (LoopResult, error)
}
```

The exact public placement of `LoopSpec` follows dependency direction: public
definition/building types live under `pkg/hustle`; concrete spawn/await state
lives under `internal/hustleruntime`.

### API backend

An API backend calls an injected narrow client with a bounded context. It does
not construct HTTP clients or load credentials inside business logic. The
backend validates response size and narrows wire errors into typed hustle
errors.

### Function backend

A function backend executes an injected, deterministic function. It still rides
the same timeout, concurrency, audit, and caller policy path. It receives no
ambient session/controller capability.

## §4 · Loop kind and internal visibility

Every durable loop start records a non-zero kind:

```go
type LoopKind uint8

const (
	LoopKindUnknown LoopKind = iota
	LoopKindPrimer
	LoopKindDelegate
	LoopKindHustle
)

type LoopStarted struct {
	// existing fields
	Kind LoopKind
}
```

Legacy records infer primer/delegate from root/parent provenance during the
schema migration; newly emitted `Unknown` is invalid.

Event visibility is independent of persistence class and producer scope:

```go
type Visibility uint8

const (
	VisibilityPublic Visibility = iota
	VisibilityInternal
)
```

Hustle lifecycle and loop-detail events are enduring/internal where audit needs
them. Internal visibility means:

```text
journal append and validation: yes
replay/audit fold:              yes
public subscriber fan-out:      no
normal-loop lifecycle hooks:    no
```

The runtime supplies hustle loops an internal publisher and no normal-loop hook
dispatcher. This is structural; a future hook cannot accidentally run merely
because its author forgot a `LoopKind` conditional.

## §5 · Common invocation lifecycle and audit

All backends emit the same internal lifecycle:

```go
type HustleStarted struct {
	internal
	enduring
	Header
	RunID   hustle.RunID
	Name    hustle.Name
	Backend hustle.BackendKind
}

type HustleCompleted struct {
	internal
	enduring
	Header
	RunID hustle.RunID
	Name  hustle.Name
}

type HustleFailed struct {
	internal
	enduring
	Header
	RunID hustle.RunID
	Name  hustle.Name
	Error RestoredError
}
```

Lifecycle events do not copy raw input, output, credentials, commands, fetched
content, or PII. The caller's existing intent/event and the loop-backed hustle's
normal internal journal supply detailed audit where authorized. A successful
caller may append its own sanitized enduring product event (`Compacted`, title
changed, gate decision, and so on).

Ordered execution:

```text
validate registered definition + typed request
→ enforce size, security, concurrency, and participation policy
→ mint RunID and acquire activity/concurrency token
→ append HustleStarted (internal)
→ execute backend with bounded context
→ validate typed output
→ append HustleCompleted or HustleFailed (internal)
→ release activity/concurrency token exactly once
→ return typed result/error directly to the caller
→ caller applies policy and, on success, commits any product event
```

The direct caller return is authoritative. Public pub/sub is not used as a
result hand-back.

## §6 · Quiescence participation

Participation is definition-time policy:

- **Blocking:** compaction and gate/tool classifiers hold a session activity
  token. `SessionIdle` cannot fire while the result is required for the current
  operation.
- **Background:** session title/recap metadata may run without delaying normal
  loop idleness. It is still session-owned, bounded, canceled on shutdown, and
  must commit its durable product atomically if it completes.

Background does not mean detached/unbounded. The session retains cancellation
and concurrency ownership until completion. Restore never resumes either mode.

Compaction's loop-backed hustle runs while the primary turn is already active,
so its blocking token normally does not introduce a new public active/idle edge;
it prevents premature idleness if other work finishes first.

## §7 · Model selection and resource posture

Each hustle selects a model independently:

```go
type ModelStrategy uint8

const (
	ModelStrategyUnknown ModelStrategy = iota
	ModelStrategyCurrentLoop
	ModelStrategyNamed
)
```

- Compaction uses the current loop model or another explicitly configured model
  whose context window can contain the source conversation. Cache reuse is only
  an optimization.
- Classifiers, titles, and short recaps normally use a named small/fast model.
- A model selection is resolved and validated at invocation; no hustle silently
  falls back to a broader/more expensive model.

Loop-backed defaults are least privilege:

```text
tools:             none
delegation:        none
normal-loop hooks: none
fan-out:           none
security ceiling: explicit narrow profile
max output:        required
timeout:           required
```

A future tool-using hustle must explicitly opt into a named restricted tool set
and rides the same gate/sandbox policy as normal tool execution.

## §8 · Caller-owned policy and safety authority

`Backend.Execute` and typed adapters return errors; the consumer owns failure
and action policy.

Typical policies:

- compaction: fail open only below the hard context admission limit;
- safety classifier: deny or escalate to a human on error/ambiguity;
- title: deterministic text fallback;
- recap: resume without recap or surface a non-blocking warning.

A classifier never directly approves a gate. It returns a constrained verdict:

```go
type Verdict uint8

const (
	VerdictUnknown Verdict = iota
	VerdictAllow
	VerdictDeny
	VerdictEscalate
)

type Classification struct {
	Verdict    Verdict
	Risk       RiskLevel
	ReasonCode ReasonCode
}
```

The gate/policy layer may auto-approve only when all conditions hold:

```text
caller is authenticated and authorized
gate kind is explicitly auto-approvable
session/rig security ceiling permits machine approval
classifier output validates and says Allow
configured risk threshold is satisfied
no higher-priority deny rule matched
```

Any error, unknown field/value, invalid schema, disagreement, non-auto-
approvable gate, or exceeded ceiling becomes deny/escalate, never allow.

Structured output constrains syntax but is not proof that the semantic verdict
is correct. Tests/evaluation and policy limits remain required.

## §9 · Isolation and prompt injection

Loop-backed guard hustles process untrusted content. Controls:

- dedicated system prompt stating that supplied content is data to analyze, not
  instructions to follow;
- versioned delimited input;
- no tools by default;
- constrained output schema;
- maximum input/output sizes;
- least-privilege model/security profile; and
- caller-owned deny/escalate behavior.

Compaction is also security-sensitive because its summary re-enters a tool-
capable loop. Its output is validated as one summary text message, tagged as
data-only context, and cannot introduce tools, system messages, or gate
decisions.

The OS sandbox is irrelevant to a tool-less inference call. If a future hustle
executes tools, the normal tool sandbox/gate controls apply.

## §10 · Usage and observability

A loop-backed hustle's usage lives on its own `AIMessage.Usage` and folds into
its own hustle-loop totals. It is not copied to the caller's message or usage.

Common `HustleCompleted` does not duplicate usage as a second accounting source.
Cross-session aggregate spend can derive from internal hustle loop records or a
future dedicated projection.

Operational views may expose sanitized aggregates by hustle name/model/status,
but never prompts, fetched content, commands, credentials, or raw classifier
reasoning.

## §11 · Restore and crash behavior

Hustles re-run; they never resume:

- restore reconstructs primers and delegates;
- `LoopStarted{Kind: LoopKindHustle}` is retained for audit but no live hustle
  loop is reconstructed;
- an unmatched `HustleStarted` is an interrupted attempt, not a product;
- if the caller's product event never committed, the restored caller state
  re-evaluates its trigger; and
- a completed product event (`Compacted`, title change, gate decision) folds
  normally and prevents duplicate application according to its own basis/id.

This makes completion atomic from the caller's perspective:

```text
hustle result returned but product event not appended before crash
→ result is not durable
→ restore re-evaluates/re-runs
```

## §12 · Planned hustles and TODOs

| Name | Backend | Participation | Output | Caller | Initial status |
|---|---|---|---|---|---|
| `context.compact` | loop | blocking | summary | loop context controller | implement now |
| `gate.command-safety` | loop/API | blocking | classification | gate policy | TODO |
| `gate.general-safety` | loop/API | blocking | classification | gate policy | TODO |
| `content.fetch-injection` | loop/API | blocking | verdict + spans/codes | Fetch policy | TODO |
| `content.tool-safety` | loop/API | blocking | classification | tool boundary | TODO |
| `session.title` | loop | background | short title | catalog/title controller | TODO |
| `session.stale-recap` | loop | background or blocking by caller | recap | restore/UI flow | TODO |
| `session.summary` | loop | background | summary | display/export flow | TODO |

Each TODO requires its own typed input/output, validation, caller policy,
evaluation corpus, and tests. Adding one must not add fields to unrelated hustle
types; it composes a new adapter/definition over the stable runtime.

### Prompt-cache TODO

Prompt caching is not currently a guaranteed Looprig capability. Add a separate
design that:

- represents provider-neutral cache intent and stable prefix identity in
  `inference`;
- translates intent into OpenAI/Anthropic/Gemini policy in `llm`;
- records cache read/write usage without changing context size semantics;
- defines privacy/retention expectations; and
- verifies byte-/block-stable prefixes.

No hustle correctness or safety decision may depend on a cache hit.

## §13 · Error model

Package/runtime APIs return typed errors:

- definition: missing/duplicate name, backend, limits, timeout,
  participation, security incompatibility;
- registry: unknown hustle, duplicate registration, fingerprint mismatch;
- invocation: invalid session/cause/run ID, input limit, encode/decode,
  unsupported version;
- admission: concurrency/queue limit, shutting down, security ceiling;
- execution: timeout, cancellation, backend kind/stage, loop terminal,
  API/function failure;
- output: size, malformed JSON, unknown fields, schema/typed validation; and
- persistence: lifecycle append/product append faults.

Every stage error includes `{Name, RunID, Stage}` and unwraps its cause. No error
path returns a partially validated output.

## §14 · Testing

All tests are table-driven and run with `-race`; network/process crossings have
integration tests under the `integration` tag; external-input parsers receive
fuzz targets.

- definition/registry: required fields, duplicate names, defensive copies,
  limits, fingerprinting, unknown references;
- serialization boundary: version, size limits, unknown fields, immediate typed
  narrowing, malformed input/output fuzzing;
- lifecycle: exact start/completed/failed order, append failures, timeout,
  cancel, release-on-every-path;
- concurrency: session cap, bounded queue, shutdown, blocking/background
  participation;
- loop backend: `LoopKindHustle`, internal visibility, direct terminal return,
  no public fan-out, no restore, own usage;
- hook isolation: hustle step/turn events cannot trigger snapshots, occupancy,
  compaction, or future normal-loop dispatcher tests;
- API/function backends: same lifecycle/audit semantics without loop events;
- caller policy: invalid/unknown/error classifier outputs never auto-allow;
- compaction: result commits only through `Compacted`; crash before product
  append re-runs; no usage leakage;
- metadata hustles: deterministic fallback and no unbounded background work; and
- legacy loop-kind migration.

## §15 · Module impact

- `core/content` — normalized usage consumed by loop-backed hustle results.
- `inference` — structured-output and future cache-intent dependencies; no
  hustle import.
- `llm` — provider clients/caching implementation only; no harness import.
- `harness/pkg/hustle` — public immutable contracts and backend boundary.
- `harness/internal/hustleruntime` — registry/runtime/loop spawn/audit.
- `harness/pkg/rig` — `WithHustles`, limits, graph validation, fingerprint.
- `harness/pkg/event` — loop kind, visibility, hustle lifecycle codecs.
- `harness/internal/sessionruntime` — per-session registry, activity tokens,
  shutdown, restore-skip, product integration.
- `swe` — definitions, prompts/models, typed adapters, policy/evaluations for
  product-specific hustles.

## Result

Hustles become a reusable harness-owned execution mechanism without becoming
agents, delegates, or an untyped business API. The runtime centralizes audit,
isolation, limits, model execution, and lifecycle; each consumer keeps a narrow
typed contract and sole authority over the resulting action. Compaction uses it
now, and the named classifier/title/recap TODOs can be added by composition
without redesigning session lifecycle.
