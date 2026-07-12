# Hustle — Harness-Owned Auxiliary Work

**Date:** 2026-07-11

**Last revised:** 2026-07-11 (adds the immutable hustle registry, typed adapters
over a serialized execution boundary, internal event visibility, loop kinds,
common audit lifecycle, blocking/background participation, near-term use cases,
and prompt-cache TODO; folds two review rounds — visibility in the durable
`Header` (`EventVisibility` field / `Visibility()` accessor, no collision) stamped
by the trusted publisher via a `WithHeader` rewrite contract, runtime-owned
drain + activity tokens via `RunAndFinalize` (finalizer runs on every terminal
state) with a deterministic shutdown sequence, `pkg/hustle`-leaf
`DefinitionDescriptor` vs `pkg/event.HustleRunDescriptor` (acyclic imports; only
definition-time identity in the rig fingerprint), `Backend.Identity()`,
finalizer-shaped typed adapters, `sessionScoped` lifecycle events with loop
correlation and per-backend accounting incl. `HustleFailed.Usage`,
`ClassificationBasis` subject binding, and separate blocking/background lanes)

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
	// Blocking lane: compaction, safety/tool classifiers. Guaranteed capacity so a
	// flood of background work can never starve a hard-limit compaction or a gate
	// safety check.
	BlockingConcurrent:   3,
	BlockingQueued:       16,
	// Background lane: titles, recaps, summaries. Strictly lower priority; capped
	// so it cannot consume blocking capacity.
	BackgroundConcurrent: 1,
	BackgroundQueued:     16,
})
```

Blocking and background hustles run in **separate lanes**, not one shared pool.
The blocking lane has reserved, guaranteed concurrency; the background lane is
strictly lower priority and cannot borrow blocking capacity. Scheduling within a
lane is deterministic (FIFO by admission order) so replay and fairness are
predictable, and a single background `session.title` run can never compete with a
hard-limit `context.compact` or a `gate.command-safety` classification.
`WithHustles` is additive and rejects duplicate names atomically. An unreferenced
definition is allowed because metadata/background consumers may invoke it after
session creation. Limits are independent of delegation limits.

The rig fingerprint covers only the **definition-time** identity — a
`DefinitionDescriptor` (§5) — never invocation-time resolution. It includes the
stable name, backend kind, **model strategy** and **model selector**,
**system-prompt revision/hash**, **structured-output schema revision**,
**backend implementation revision**, and the security/limits revision and
participation mode. It never includes credentials, raw prompts, or a **resolved**
model key: `ModelStrategyCurrentLoop` resolves the concrete model at invocation
and legitimately varies without the rig definition changing, so the resolved key
belongs to the per-run `event.HustleRunDescriptor`, not the rig fingerprint. A change to any
definition-time field is an observable definition change; a prompt or schema edit
that leaves the fingerprint unchanged is a bug in revision bumping, not an
acceptable silent mutation.

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

// A backend both executes and reports its stable identity. Identity is on the
// interface (not just prose) because definition validation and the rig
// fingerprint need the backend kind and its immutable implementation revision.
type BackendIdentity struct {
	Kind    BackendKind
	ImplRev BackendImplRevision
}

type Backend interface {
	Execute(context.Context, Invocation) (Result, error)
	Identity() BackendIdentity
}
```

`Definition` construction reads `backend.Identity()` to populate the
`DefinitionDescriptor` (§5) — the kind and implementation revision it fingerprints
come from the backend itself, not a hand-maintained duplicate.

`Invocation.Input` and `Result.Output` are the only intentionally untyped data.
They are size-checked before decoding, use a versioned wire shape per hustle,
reject unknown fields, and are immediately converted into typed domain structs.
They are never passed deeper into policy/business logic.

Every domain consumer remains focused, and every one is **finalizer-shaped**: it
does not *return* a typed value (which would release the operation token before
the caller's policy/product commit, §5), it invokes a caller-supplied finalizer
**inside** the held-token scope. The finalizer receives a domain outcome that
carries either a validated value or the terminal error, so the caller can commit a
product on success **and** durably reject waiters on failure while the token is
still held:

```go
// One concrete outcome type per domain — NOT a generic Outcome[T any], which the
// repo's strict-typing rule disallows outside serialization boundaries. Exactly
// one of Value / Err is set.
type CompactionOutcome struct {
	Value *CompactionOutput
	Err   error
}

type Compactor interface {
	CompactAndFinalize(
		context.Context,
		CompactionInput,
		func(context.Context, CompactionOutcome) error,
	) error
}

type GateClassifierOutcome struct {
	Value *GateClassification
	Err   error
}

type GateClassifier interface {
	ClassifyAndFinalize(
		context.Context,
		GateClassificationInput,
		func(context.Context, GateClassifierOutcome) error,
	) error
}
```

The finalizer runs for **every** terminal state — success, execution failure,
output-validation failure, cancellation, and timeout — never only the happy path.
On failure the finalizer's outcome carries `Err` and no value, and the runtime
still holds the token, so (for example) compaction durably appends
`CompactionRejected` for its waiters before the token releases. The adapter owns
marshal → invoke → unmarshal → validate and then calls the finalizer; callers
never switch on generic payload maps and never receive a bare return value that
would escape the token scope.

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

Cancellation is **cooperative**: an in-process function is bounded by the
`context.Context` it is passed, but a function that ignores that context cannot be
given a hard timeout — the runtime can stop *waiting* on it and release the
caller, but it cannot forcibly kill a running goroutine. Function backends must
therefore honor `ctx.Done()` at their I/O and loop boundaries; the definition
contract states this, and the timeout is documented as a wait bound, not a
guaranteed-kill bound. (Loop and API backends cancel real I/O, so their timeout is
effective.)

## §4 · Loop kind and internal visibility

Every durable loop start records a non-zero kind. `LoopKind` is owned by
`harness/pkg/event` as its **single definition**; the token-usage design
references the same `event.LoopKind` and neither redefines it:

```go
// Package event — the one definition, referenced as event.LoopKind everywhere.
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
	VisibilityPublic   Visibility = iota // zero value: public, back-compatible default
	VisibilityInternal
)
```

**Where visibility is carried.** Visibility is a field on the durable event
`Header`, not a property of the event *type*. A type-level marker is
insufficient: the same shared type — `StepDone`, `TurnDone`, `LoopStarted` — is
**public** when produced by a primer/delegate loop and **internal** when produced
by a hustle loop. Only a per-instance field can express that. The field is named
`EventVisibility` and the accessor `Visibility()` — Go forbids a field and method
sharing one identifier, so they must differ. The zero value is `VisibilityPublic`
so every existing event stays public without a migration.

```go
type Header struct {
	// existing fields
	EventVisibility Visibility // zero == public
}

// Event gains two methods: an accessor (so the fan-out layer never inspects
// concrete types) and a sealed clone that returns a copy with a replaced Header.
type Event interface {
	// existing methods
	Visibility() Visibility
	EventHeader() Header       // returns a copy, cannot mutate in place
	WithHeader(Header) Event   // sealed clone: same concrete type, new Header
}

func (h Header) Visibility() Visibility { return h.EventVisibility }
```

**Producers are not trusted to self-classify — and stamping needs a real rewrite
contract.** `EventHeader()` returns a *copy*, so the publisher cannot mutate
visibility in place, and a hub type-switch that only knows a few concrete events
cannot restamp arbitrary shared events (`StepDone`, `TurnDone`, `LoopStarted`).
`WithHeader` closes this: it is a **sealed clone** every event type implements
(mechanically, via the embedded `Header` and a generated/boilerplate method), so
the trusted publisher restamps **generically** — take the event, read its header,
set `EventVisibility`, and re-emit `event.WithHeader(stamped)` — without a
per-type switch. A `LoopKindHustle` loop's runtime is constructed with an
internal-stamping publisher that runs exactly this rewrite on every event it
emits, forcing `VisibilityInternal` regardless of what the producing code set. A
hustle loop is never handed a publisher that can emit public events.

(Alternatively the publisher could accept unstamped event *builders* and
construct the final event itself; this design chooses `WithHeader` because it
keeps events as plain values and needs no builder type per event.)

Internal visibility means:

```text
journal append and validation: yes
replay/audit fold:              yes
public subscriber fan-out:      no  (centrally suppressed by Visibility(), not per-type)
normal-loop lifecycle hooks:    no
```

The fan-out and hook dispatchers filter on `event.Visibility()` centrally, so a
future hook or subscriber cannot accidentally observe an internal event merely
because its author forgot a `LoopKind` conditional. This is the structural
mechanism that makes core invariant 6 (internal visibility) enforceable rather
than aspirational.

## §5 · Common invocation lifecycle and audit

All backends emit the same internal lifecycle. Each carries a **secret-free
immutable descriptor** that fully identifies *which* recipe ran, so audit and the
rig fingerprint agree without ever storing prompts or credentials:

Identity is split so definition-time facts and invocation-time resolution never
mix (a `ModelStrategyCurrentLoop` hustle resolves a different model per run
without any definition change), **and the split follows the import boundary**:
definition-time identity is a `pkg/hustle` leaf type; the per-run wrapper is a
`pkg/event` type because it embeds `event.ModelRuntime` and rides on
`pkg/event` lifecycle events. `pkg/hustle` must not import `pkg/event`, so the
wrapper cannot live in `pkg/hustle`:

```go
// Package hustle (leaf; imports neither pkg/event nor the runtime). Definition-
// time identity — secret-free, immutable, the ONLY thing in the rig fingerprint
// (§1). Fields are named types, not bare strings, per the strict-typing rule.
type ModelSelector string        // named model id or the "current-loop" sentinel — NOT a resolved key
type PromptRevision string
type SchemaRevision string
type SecurityRevision string
type BackendImplRevision string

type DefinitionDescriptor struct {
	Name          Name
	Backend       BackendKind
	ModelStrategy ModelStrategy
	ModelSelector ModelSelector
	PromptRev     PromptRevision
	SchemaRev     SchemaRevision
	SecurityRev   SecurityRevision
	BackendImpl   BackendImplRevision
}
```

```go
// Package event (imports pkg/hustle and inference; the runtime imports event).
// Per-run identity — definition plus what invocation actually resolved. Lives on
// pkg/event lifecycle events, never in the rig fingerprint.
type HustleRunDescriptor struct {
	Definition hustle.DefinitionDescriptor
	Runtime    ModelRuntime // resolved model + limits + effort; zero for BackendFunc
}
```

All lifecycle events live in `pkg/event`, are `sessionScoped` (the run is a
session-owned operation, not a loop-scoped event), and carry
`event.HustleRunDescriptor`:

```go
type HustleStarted struct {
	internal // stamps Header.EventVisibility = VisibilityInternal at trusted publish
	enduring
	sessionScoped
	Header
	RunID  hustle.RunID
	Run    HustleRunDescriptor
	LoopID *uuid.UUID // set for BackendLoop: correlates to that loop's LoopStarted
}

type HustleCompleted struct {
	internal
	enduring
	sessionScoped
	Header
	RunID hustle.RunID
	Run   HustleRunDescriptor
	// Usage is the AUTHORITATIVE accounting source for BackendAPI, which has no
	// loop AIMessage. It is nil for BackendLoop (usage lives on its own AIMessage
	// and hustle-loop totals) and for BackendFunc (no model call). Exactly one
	// source per backend, no double counting.
	Usage *content.Usage
}

type HustleFailed struct {
	internal
	enduring
	sessionScoped
	Header
	RunID hustle.RunID
	Run   HustleRunDescriptor
	// Usage may be non-nil for BackendAPI even on failure: an API request can
	// consume tokens and THEN fail output decode/validation. Recording it here
	// keeps spend from being lost on the failure path. Nil for loop/func.
	Usage *content.Usage
	Error RestoredError
}
```

The `internal` embed is the producer-side declaration that the trusted
hustleruntime publisher honors when stamping `Header.EventVisibility`; these
events are internal for **every** backend, including API/function backends that
run no loop. `HustleStarted.LoopID` closes the correlation gap: a loop-backed
run's `HustleStarted` and the loop's own `LoopStarted{Kind: LoopKindHustle}` are
joined by `RunID`↔`LoopID`, so audit can walk from a hustle run to its loop
detail and back.

Lifecycle events do not copy raw input, output, credentials, commands, fetched
content, or PII. The caller's existing intent/event and the loop-backed hustle's
normal internal journal supply detailed audit where authorized. A successful
caller may append its own sanitized enduring product event (`CompactionCommitted`,
title changed, gate decision, and so on).

**Three distinct tokens.** Ownership/drain and quiescence are *different*
concerns and must not be one token — a background hustle is owned by the session
(shutdown must wait for it) yet must **not** delay `SessionIdle` (§6). So:

- a **concurrency** token bounds how many backends execute at once, released as
  soon as the backend finishes;
- an **ownership/drain** token is held by **every** hustle (blocking *and*
  background) from validation until its finalizer returns; shutdown waits on it so
  no run is torn down mid-flight; it does **not** gate `SessionIdle`;
- an **activity/quiescence** token is held by **blocking** hustles only, over the
  same span; it *does* gate `SessionIdle`, covering the window between "result
  ready" and "product committed". Background hustles never acquire it.

Neither the drain nor the activity token is a data struct the caller passes around
and must remember to release — that leaks the token if a caller forgets. The
runtime **owns** both across a caller-supplied finalizer, so release is structural
and exactly-once. The finalizer is the finalizer-shaped typed adapter from §2; it
runs on **every** terminal state, so failures reach the caller *inside* the held
scope:

```go
// The caller-facing runtime boundary. The runtime holds the drain token (always)
// and the activity token (blocking only) across the WHOLE call — validation,
// backend execution, AND the finalizer — releasing each exactly once when
// RunAndFinalize returns, on every path. The caller never sees or releases a
// token; the finalizer only decides what to commit.
//
// finalize receives the terminal Outcome (success OR error) so the caller can
// commit a product on success AND durably reject/record on failure. It runs for
// success, execution failure, output-validation failure, cancellation, and
// timeout — never only the happy path.
type RunAndFinalize interface {
	RunAndFinalize(
		ctx context.Context,
		invocation Invocation,
		finalize func(ctx context.Context, outcome Outcome) error,
	) error
}

// Outcome is the runtime-boundary (serialization-layer) envelope: exactly one of
// Result / Err is set. Typed adapters (§2) narrow it to a per-domain outcome
// (CompactionOutcome, …) before the caller sees it.
type Outcome struct {
	Result *Result
	Err    error
}
```

Ordered execution (all driven by the runtime, not the caller):

```text
validate registered definition + typed request
→ enforce size, security, concurrency, and participation policy
→ mint RunID; acquire drain token (always) + activity token (blocking only) + concurrency token
→ append HustleStarted (internal)
→ execute backend with bounded context
→ validate typed output
→ append HustleCompleted or HustleFailed (internal)
→ release the CONCURRENCY token (backend slot free)
→ invoke finalize(ctx, outcome) — drain (+ activity, if blocking) STILL HELD
→ finalize commits the product on success, or durably records/rejects on failure
→ release drain + activity tokens exactly once, on every return/panic/cancel path
→ return finalize's error (or the execution error) to the caller
```

The activity token is what a blocking hustle contributes to quiescence (§6):
`SessionIdle` cannot fire while it is held, so the result-ready→product-committed
window is covered by construction — the caller cannot observe the outcome outside
the token scope. The drain token is what **shutdown** waits on for *every* hustle.
All three tokens release on exactly one path each, including every error, panic,
and cancellation path. The finalizer return is authoritative; public pub/sub is
not used as a result hand-back.

## §6 · Quiescence participation

Participation is definition-time policy, and it maps directly onto which of the
two session-owned tokens (§5) a run acquires:

- **Blocking** (compaction, gate/tool classifiers): holds **both** the drain token
  and the **activity** token. `SessionIdle` cannot fire while the result is
  required for the current operation.
- **Background** (session title/recap/summary metadata): holds the **drain** token
  only, **never** the activity token — so it runs without delaying loop idleness.
  It is still session-owned, bounded, canceled on shutdown, and must commit its
  durable product atomically if it completes.

This is exactly the drain-vs-activity split: ownership (shutdown waits) applies to
every hustle via the drain token; quiescence (idle is delayed) applies to blocking
hustles only via the activity token. Background does not mean detached/unbounded —
the session retains cancellation and concurrency ownership until completion.
Restore never resumes either mode.

Compaction's loop-backed hustle runs while the primary turn is already active,
so its activity token normally does not introduce a new public active/idle edge;
it prevents premature idleness if other work finishes first.

### Shutdown ordering

Shutdown waits on the **drain** token — held by every hustle, blocking or
background — before it declares the session stopped. The session performs a fixed
sequence:

```text
1. close hustle admission (no new invocation accepted)
2. cancel queued and running hustles (bounded-context cancellation)
3. prevent new product commits (callers past this point cannot append products)
4. await every terminal lifecycle record (HustleCompleted/HustleFailed) and
   drain-token release (activity token too, for blocking runs)
5. emit SessionStopped
6. release the session lease
```

Steps 4→5 guarantee `SessionStopped` never races an in-flight product commit: a
run still holding its drain token keeps the session out of the stopped state until
its finalizer has returned (product committed or abandoned). A background run is
cancelled at step 2 and, if it had not yet committed, is re-evaluated on the next
start rather than committed during teardown.

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

A classifier never directly approves a gate. It returns a constrained verdict,
and the verdict is **bound to the exact subject it judged** so it can never be
applied to a different or mutated gate/command/policy:

```go
// Binds a classification to what was classified. Carried on classifier input, on
// the returned Classification, and on the product event. Every field is an id,
// digest, or revision — never raw command/content bytes.
type ClassificationBasis struct {
	GateID             uuid.UUID
	ToolExecutionID    uuid.UUID
	SubjectDigest      [32]byte       // digest of the exact classified subject
	PolicyRevision     PolicyRevision
	ClassifierRevision ClassifierRevision
}

type Verdict uint8

const (
	VerdictUnknown Verdict = iota
	VerdictAllow
	VerdictDeny
	VerdictEscalate
)

type Classification struct {
	Basis      ClassificationBasis
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
the Classification.Basis STILL MATCHES the live gate immediately before approval:
  same GateID + ToolExecutionID, recomputed SubjectDigest equal,
  PolicyRevision and ClassifierRevision unchanged
```

The basis recheck happens **immediately before** auto-approval, closing the
time-of-check/time-of-use gap: if the command, gate, policy, or classifier
revision changed between classification and application, the digests/revisions
disagree and the decision escalates or denies. Any error, unknown field/value,
invalid schema, disagreement, basis mismatch, non-auto-approvable gate, or
exceeded ceiling becomes deny/escalate, never allow.

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

Usage has **exactly one accounting source per backend**, so aggregation never
double-counts and never drops a backend:

| Backend | Authoritative usage source |
|---|---|
| `BackendLoop` | the hustle loop's `AIMessage.Usage`, folded into hustle-loop totals; the lifecycle `Usage` fields are nil |
| `BackendAPI` | `Usage` on the terminal lifecycle event — `HustleCompleted.Usage` on success, **`HustleFailed.Usage`** when the request consumed tokens but output decode/validation then failed |
| `BackendFunc` | none — no model call, so no usage (lifecycle `Usage` nil) |

Recording API usage on **both** terminal events is what prevents spend from being
lost: an API hustle can burn tokens and still fail validation, and that spend is
still real. A loop-backed hustle's usage is never copied to the caller's message
or usage. The token-usage design's `HustleUsageAggregate` (bounded, keyed by
name/model/status) folds from these single sources — loop totals for
`BackendLoop`, the terminal event's `Usage` (completed *or* failed) for
`BackendAPI` — and from nothing for `BackendFunc`.

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
- a completed product event (`CompactionCommitted`, title change, gate decision) folds
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
  limits, fingerprinting covers `DefinitionDescriptor` only (model **selector**,
  prompt/schema/security/impl revisions) and **excludes** the resolved model key,
  unknown references;
- serialization boundary: version, size limits, unknown fields, immediate typed
  narrowing, malformed input/output fuzzing;
- visibility: `Header.EventVisibility` zero == public and `Header.Visibility()`
  reads it (no field/method collision); the trusted publisher restamps generically
  via `WithHeader` (not a per-type switch); `EventHeader()` returns a copy so it
  cannot mutate in place; a `LoopKindHustle` loop's publisher forces internal on
  shared types (`StepDone`, `TurnDone`, `LoopStarted`) while the same type stays
  public from a primer/delegate; central `Visibility()` filter suppresses fan-out
  and hooks;
- lifecycle: exact start/completed/failed order, `sessionScoped`,
  `event.HustleRunDescriptor` present (definition + resolved runtime),
  `HustleStarted.LoopID`↔`LoopStarted` correlation, append failures, timeout,
  cancel, release-on-every-path for all three tokens;
- tokens via `RunAndFinalize`: drain token held by every hustle (shutdown waits),
  activity token held by blocking only (gates `SessionIdle`), background never
  acquires activity; the finalizer runs for success, execution failure,
  validation failure, cancellation, and timeout; drain + activity release exactly
  once on every path incl. panic; a caller cannot observe the outcome outside the
  token scope;
- shutdown ordering: admission closed → running cancelled → product commits
  blocked → terminal records + drain-token release awaited → `SessionStopped` →
  lease released; no `SessionStopped`/product-commit race;
- concurrency: separate blocking/background lanes, reserved blocking capacity,
  background cannot starve blocking, deterministic FIFO within a lane, bounded
  queues, shutdown;
- loop backend: `LoopKindHustle`, internal visibility, finalizer-delivered
  outcome (not a bare return), no public fan-out, no restore, own usage;
- backend identity: `Backend.Identity()` supplies kind + `BackendImplRevision`;
  `Definition` construction reads it into `DefinitionDescriptor`; a stale impl
  revision is a definition-fingerprint change;
- finalizer-shaped adapters: typed `CompactAndFinalize`/`ClassifyAndFinalize`
  invoke the finalizer for every terminal state and never return a bare value that
  escapes the token scope;
- import boundary: `pkg/hustle` (leaf: `DefinitionDescriptor`, `Name`,
  `BackendKind`) does not import `pkg/event`; `event.HustleRunDescriptor` embeds
  `hustle.DefinitionDescriptor`; no `event`↔`hustle` cycle;
- hook isolation: hustle step/turn events cannot trigger snapshots, occupancy,
  compaction, or future normal-loop dispatcher tests;
- API/function backends: same lifecycle/audit semantics without loop events; API
  usage authoritative on the terminal event — `HustleCompleted.Usage` on success
  **and `HustleFailed.Usage` when tokens were spent before a validation failure**;
  nil for loop/func; function backend cooperative cancellation (context-ignoring
  function bounds wait only);
- caller policy: invalid/unknown/error classifier outputs never auto-allow;
  `ClassificationBasis` mismatch (changed subject digest, gate id, policy or
  classifier revision) at the pre-approval recheck escalates/denies;
- compaction: result commits only through `CompactionCommitted`; crash before product
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
