# Hustle — Harness-Owned Auxiliary Inference

**Date:** 2026-07-11

**Last revised:** 2026-07-12

**Status:** Approved

**Depends on:**

- `docs/plans/2026-07-10-rig-lifecycle-workspace-snapshots-design.md`
  (**Approved**) — immutable rig composition, bound loop definitions, session
  ownership, quiescence, shutdown, restore, and workspace boundaries.
- `docs/plans/2026-06-19-event-persistence-checkpoint-design.md` — durable
  events, replay, and catalog repair.
- `docs/plans/2026-07-11-token-usage-context-occupancy-design.md` — normalized
  usage and compaction, the first hustle consumer.
- `docs/plans/2026-07-11-structured-output-design.md` — constrained results for
  later classifier hustles. Compaction does not depend on structured output.

**New external dependencies:** none.

---

## Problem

Harness needs small model calls whose results inform a larger operation without
becoming conversational turns:

- summarize context before a model limit is reached;
- classify a command or gate;
- scan fetched content for prompt injection;
- derive a session title; and
- recap or summarize a session.

These calls need common definition, attribution, timeout, cancellation, audit,
usage, concurrency, and shutdown behavior. Their domain inputs and outputs do
not belong in one generic business API.

The landed rig architecture makes a loop a durable topology actor. A loop is
registered by `pkg/rig`, routed by `internal/sessionruntime`, restored from
`LoopStarted`, participates in workspace boundaries, and remains addressable
through the session. A hustle has none of those semantics. Modeling each hustle
run as a loop would require exceptions in topology validation, restore, routing,
quiescence, hooks, catalog projection, and shutdown.

Therefore a hustle is **not a loop**. Version 1 is a session-owned, one-shot
inference operation with its own run identity and lifecycle.

## Decision

Add:

```text
pkg/hustle
    Immutable definition, model-source policy, wire envelopes, result metadata,
    limits, descriptors, and typed errors.

internal/hustleruntime
    Per-session controller, admission lanes, execution, audit, cancellation,
    quiescence participation, finalization, and drain.
```

`pkg/rig` registers definitions and includes their immutable policy revisions in
the rig fingerprint. `internal/sessionruntime` binds definitions to a live
session, supplies current-loop model resolution, owns the controller, and
orders it before hub/session teardown.

Version 1 has one execution kind: a tool-less call through the existing
`inference.Client.Invoke` contract. Function execution, arbitrary HTTP
backends, tools, multi-turn actors, streaming, and foreign-agent sessions are
out of scope. They require their own isolation and lifecycle designs rather
than more branches in this mechanism.

Compaction is implemented first. Classifiers, titles, recaps, and summaries are
named follow-ons that reuse the mechanism but still require their own typed
adapters, prompts, validation, policy, and evaluations.

## Core invariants

1. **Harness-owned.** Only trusted harness subsystems invoke hustles. Models,
   tools, delegates, and other hustles cannot enumerate or invoke them.
2. **Not a loop or delegation.** A hustle has no `LoopStarted`, loop handle,
   inbox, tool set, delegate depth, workspace hook, or restoreable actor.
3. **Typed business boundaries.** Callers use focused domain interfaces. Raw
   JSON exists only at the explicit runtime serialization boundary and is
   immediately narrowed and validated.
4. **Caller-owned action.** A result is evidence, not authority. The caller
   alone commits compaction, approves a gate, or changes product state.
5. **One usage owner.** Hustle usage is recorded on its terminal lifecycle
   event and is never added to the originating loop's cumulative usage.
6. **Private audit.** Lifecycle detail is durable but does not enter ordinary
   live subscriptions or native step/turn/workspace boundaries.
7. **No partial product.** A successful result has no durable product effect
   until the caller's finalizer commits its own enduring event.
8. **Re-run after interruption.** Restore never resumes a hustle. The caller
   re-evaluates its trigger from durable product state.
9. **Bounded ownership.** Every run has input/output bounds, a timeout,
   cancellation, lane admission, and session drain ownership.
10. **Fail secure.** Unknown definitions, invalid output, audit failures,
    ambiguous classification, model mismatch, or security mismatch never
    silently degrade to a more permissive behavior.

---

## §1 · Immutable definitions

The public package follows the landed `loop.Definition` pattern: construction
validates and freezes behavior; runtime code consumes a read-only bound view.

```go
package hustle

type Name string

type Participation uint8

const (
	ParticipationUnknown Participation = iota
	ParticipationBlocking
	ParticipationBackground
)

type ModelSource uint8

const (
	ModelSourceUnknown ModelSource = iota
	ModelSourceCurrentLoop
	ModelSourceNamed
)

type Limits struct {
	InputBytes  int
	OutputBytes int
}

type Definition struct {
	// immutable private state
}

func Define(opts ...Option) (Definition, error)
```

Initial options:

```text
WithName(name)
WithParticipation(participation)
WithTimeout(timeout)
WithLimits(limits)
WithCurrentLoopModel()
WithNamedInference(client, model)
WithSystemPrompt(prompt, promptRevision)
WithOutputSchema(schema, schemaRevision) // optional; classifiers only
WithPolicyRevision(revision)             // opaque request/parser behavior
```

The definition owns the hustle's prompt and inference request policy. The input
is a versioned JSON document carried as a single data-only user message. A
classifier definition may additionally require structured output. Tools and
delegation are not representable in v1.

`Define` rejects:

- a nil option, blank or reserved name, unknown participation, or unknown model
  source;
- a missing client/model for `ModelSourceNamed`;
- a non-positive timeout or non-positive/out-of-range byte limit;
- a blank prompt revision, schema revision, or required opaque policy revision;
- an invalid model or output schema;
- a schema on a named-model definition that does not support structured output
  (current-loop capability is checked when that model is resolved); and
- any prompt, schema, or opaque request/parser behavior without a stable
  revision included in the policy identity.

Secrets never enter descriptors or fingerprints. A definition may retain an
already-wired `inference.Client`, exactly as `loop.Definition` does, but its
identity contains only secret-free model and policy data.

### Model sources

`ModelSourceNamed` freezes a client/model pair in the definition.

`ModelSourceCurrentLoop` resolves at each invocation from the originating
`LoopID`. The landed session registry already retains:

- the loop's bound `inference.Client`; and
- a synchronized live model view updated only after a mode/model change is
  durably committed by the actor.

The resolver returns that client and current model. The hustle still uses its
own system prompt, no tools, and its own output/timeout limits. Resolution fails
if the cause has no loop, the loop is absent/exited, or the current model cannot
satisfy the definition's required capabilities. There is no fallback to the
active primer or a named model.

Binding is explicit and follows `loop.Definition.Bind`:

```go
type InferenceBinding struct {
	Client inference.Client
	Model  inference.Model
}

type ModelResolver interface {
	ResolveHustleModel(context.Context, uuid.UUID) (InferenceBinding, error)
}

type Bindings struct {
	Models ModelResolver
}

type BoundDefinition interface {
	Name() Name
	Participation() Participation
	Timeout() time.Duration
	Limits() Limits
	Descriptor() DefinitionDescriptor
	ResolveInference(context.Context, uuid.UUID) (InferenceBinding, error)
	SystemPrompt() string
	OutputSchema() *inference.OutputSchema
	boundDefinition()
}

func (d Definition) Bind(context.Context, Bindings) (BoundDefinition, error)
```

`ModelSourceNamed` resolves from its frozen pair. `ModelSourceCurrentLoop`
requires a non-nil resolver and delegates each invocation to it. Returned
models and output schemas are defensively copied. Until structured output lands,
the compaction implementation omits `OutputSchema`; the method is added with
that prerequisite before the first classifier definition.

### Stable descriptor and fingerprint

```go
type DefinitionDescriptor struct {
	Name             Name
	Participation    Participation
	ModelSource      ModelSource
	NamedModelKey    inference.ModelKey // empty for CurrentLoop
	NamedModelPolicyRevision string      // canonical secret-free model/sampling digest
	PromptRevision   string
	PromptSHA256     [32]byte
	SchemaRevision   string
	SchemaSHA256     [32]byte
	PolicyRevision   string
	TimeoutNanos     int64
	Limits           Limits
}
```

`TimeoutNanos` is the exact `time.Duration` nanosecond count. Positive durations
are not rounded, so any behavior-affecting timeout change changes the canonical
descriptor and fingerprint.

`Definition.Descriptor()` returns a defensive, secret-free value.
`Definition.PolicyRevision()` hashes its canonical encoding.

`rig.WithHustles` is additive and copies its arguments. `rig.Define` validates
all hustle definitions and rejects duplicate names atomically. The rig's
topology revision includes definitions sorted by `Name`, each definition's
policy revision, and the lane limits. It does not include clients, credentials,
raw prompts, raw schemas, or the model resolved by `ModelSourceCurrentLoop`.

Prompt/schema digests are computed from defensive copies during `Define`; raw
values never enter the fingerprint. The named-model policy revision covers the
full secret-free model/sampling descriptor, not only its routing key. Changing a
prompt, schema, model source, named model policy, participation, timeout, limit,
or parser/request policy therefore changes the resulting definition policy
revision and is covered by tests.

Structured definitions also fold `inference.StructuredOutputRevision` into
their policy revision, so provider-projection behavior cannot change across
restore under an unchanged rig fingerprint.

---

## §2 · Typed adapters and the serialization boundary

The shared runtime transports versioned JSON because hustle domains do not
share one Go input/output type:

```go
type RunID uuid.UUID

type Request struct {
	Name  Name
	Cause identity.Cause
	Input json.RawMessage
}

type Result struct {
	Output json.RawMessage
	Usage  *content.Usage
}

type Outcome struct {
	Result *Result
	Err    error
}
```

Exactly one of `Outcome.Result` and `Outcome.Err` is set. These envelopes are
serialization-layer types, not domain types. Input and output are size-checked
and must contain one JSON value. Concrete adapters own wire-version checks,
unknown-field rejection, decoding, and domain validation before business logic
sees the value.

Each consumer exposes a focused interface. For example:

```go
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
```

There is no public `Hustle[In, Out]`, no `map[string]any`, and no arbitrary
`Session.RunHustle`. The concrete adapter owns:

```text
validate typed input
→ marshal versioned wire input
→ invoke the named runtime definition
→ in the runner's validation callback, decode with DisallowUnknownFields
→ validate domain values before terminal lifecycle audit
→ convert to a concrete domain outcome
→ call the caller's finalizer
```

The finalizer shape is required, not optional. A bare returned value could
escape session ownership before the caller commits or rejects the corresponding
product. The finalizer receives success or failure while drain ownership—and,
for blocking work, quiescence activity—is still held.

---

## §3 · Binding and package ownership

`pkg/rig` owns design-time registration. `internal/sessionruntime.Lifecycle`
captures the frozen definitions and lane limits and passes them to both new and
restored sessions.

At session construction, `internal/sessionruntime` binds each definition with a
narrow current-loop model resolver, then creates one
`internal/hustleruntime.Controller` with the bound definitions and narrow
collaborators:

```go
type AuditPublisher interface {
	PublishInternalEventChecked(context.Context, event.Event) error
}

type HeaderStamper interface {
	Stamp(event.Header) (event.Header, error)
}

type FaultReporter interface {
	ReportFault(context.Context, error)
}

type ActivityTracker interface {
	AcquireHustleActivity(context.Context, hustle.RunID) (ActivityLease, error)
}

type ActivityLease interface {
	Release(context.Context) error
}
```

`*hub.Hub` is not expected to satisfy `ActivityTracker` directly: Go return
types are invariant even when two lease values expose the same methods.
`internal/sessionruntime` owns a small adapter that calls the concrete hub
activity method and wraps its returned lease as the runtime's `ActivityLease`.
This keeps `internal/hustleruntime` dependent on its consumer-owned interfaces
without moving hub types into a leaf package or creating a cycle.

The interfaces are defined where consumed. `event.Factory` satisfies
`HeaderStamper`; the controller uses a small exhaustive switch over the three
hustle lifecycle types to write the stamped header back. The controller receives
no session controller, loop registry, workspace coordinator, gate directory,
tool registry, or security-ceiling mutator.

If acquisition inserts hub activity but its derived `SessionActive` append then
fails, it returns both a non-nil **partial** lease and the error. The controller
always defers release for a non-nil lease. The lease remembers whether
acquisition committed: releasing a partial lease silently removes the in-memory
entry without deriving/appending `SessionIdle`, because no corresponding active
edge became durable; the already-latched session fault owns waiter failure.

The session's `hustle.ModelResolver` implementation reads the originating loop
handle under the existing registry/live-view locks and returns only
`inference.Client` plus the current `inference.Model`. It does not expose the
bound loop definition.

Definitions are bound before the session becomes reachable. A bind failure
aborts construction transactionally, like a loop/tool bind failure. Restore
uses the current rig definitions after the frozen rig fingerprint check; it does
not reconstruct old in-flight runs.

The raw `Runner` remains internal. For compaction, the loop actor depends only
on the focused `Compactor` contract declared with the compaction domain. During
loop construction, `internal/sessionruntime` injects an adapter bound to that
loop's ID; the adapter stamps the cause and delegates to the session controller.
The actor never receives the registry or a runner capable of naming another
hustle. Background consumers are wired to their own named adapters in
`internal/sessionruntime` by the same pattern.

---

## §4 · Invocation lifecycle

The controller boundary is:

```go
type Runner interface {
	RunAndFinalize(
		context.Context,
		hustle.Request,
		func(context.Context, hustle.Result) error,
		func(context.Context, hustle.Outcome) error,
	) error
}
```

The first callback performs concrete output decoding/domain validation. The
adapter captures the validated concrete value in call-local state; its finalizer
uses that value only when `Outcome.Err == nil`. This two-callback shape keeps
the low-level runtime non-generic while ensuring `HustleCompleted` can never
precede a domain-validation failure.

The adapter supplies only `Name`, `Cause`, and encoded `Input`. The controller
owns `SessionID` and mints `RunID` after queue capacity is reserved; neither
field exists on the caller request, so attribution cannot be spoofed.

Each session has exactly two shared FIFO lanes: blocking and background. A
definition selects one through `Participation`; there is no per-definition
lane. A run has the following state machine:

```text
rejected (not owned, no RunID/audit/finalizer)
  → queued-owned (RunID + drain ownership; blocking activity; Started committed)
  → executing (lane slot + resolved runtime + execution timeout)
  → finalizing (terminal audit committed; lane slot released)
  → done (activity/drain released)
```

Preflight validates name, cause, JSON shape, and input bounds, then mints a
candidate `RunID`. Under the chosen lane lock, the controller either rejects a
full/closed lane or inserts a node at the FIFO tail. That insertion is the
ownership commit point. It reserves drain ownership immediately. A blocking
node acquires hub activity next. `HustleStarted` is then appended with the
definition and a zero unresolved runtime; a node is not scheduler-eligible until
that append commits. A failure in activity/start audit removes the node and runs
its finalizer with the typed failure, even if persistence is too faulted to
record a terminal event.

The lane grants execution slots strictly by ownership-commit sequence, skipping
nodes already canceled. On grant, the controller resolves inference and derives
an execution-timeout context from session lifetime plus caller context. It then
issues the request, validates generic output bounds, invokes the adapter's domain
validation callback, and appends `HustleCompleted` or `HustleFailed` with the
resolved runtime when available. It releases the lane slot before finalization.

Caller/session cancellation while queued produces
`HustleFailed{Stage: StageQueue}` and invokes the finalizer exactly once. Thus a
successfully enqueued request is always an owned run with lifecycle/finalizer;
only preflight, ID-generation, lane-full, and lane-closed rejection have none.
Queue time does not consume the definition's execution timeout.

Execution timeout does not govern finalization: a timed-out inference still
needs a live, separately bounded context with which to commit
`CompactionRejected` or another failure product. Shutdown keeps the session
context and hub open while these finalizers drain. Audit and activity failures
fault/reject the run; inference does not begin after `HustleStarted` fails to
commit.

The terminal audit event is appended before the finalizer. The finalizer then
commits the caller-owned product event (`CompactionCommitted`, a gate decision,
title change, and so on). Thus lifecycle completion means “validated inference
result exists,” while the product event remains the sole durable statement that
the result was applied.

A domain consumer may emit its own public ephemeral progress event before
invocation when live presentation needs it. Compaction emits
`CompactionStarted` after accepting and freezing one attempt, then correlates it
with the caller-owned `CompactionCommitted` or `CompactionRejected` terminal.
This does not expose the generic internal `HustleStarted` audit or make hustle
lifecycle events subscriber-visible.

Because `RunAndFinalize` returns pre-ownership rejection without invoking the
finalizer, compaction uses one actor-owned idempotent terminal transition keyed
by `AttemptID`. The adapter finalizer submits its success/failure proposal to
that transition; after a direct return, the caller submits a rejection proposal
to the same transition. The actor durably appends at most one canonical terminal
and records it before acknowledging the request. A recovered callback panic
before the actor commits leaves no terminal and the fallback rejection may
commit; a panic/error after the actor commits finds the existing terminal and
is a no-op rather than a second outcome. If publishing the progress event itself
fails, compaction inference is never invoked and the actor rejects the attempt
through that same transition.

The finalizer runs exactly once for every queued-owned run:
success, model-resolution failure, inference failure, malformed output, domain
validation failure, timeout, or cancellation. Rejection before ownership returns
directly because no run exists.

Every validation/finalization callback runs behind a controller recovery
boundary. A panic is converted to `CallbackPanicError{Stage}` without retaining
the panic value, reported as a session fault, and releases activity/drain exactly
once. A validation panic records `HustleFailed` when persistence remains
available. A finalizer panic occurs after the terminal lifecycle event and is
reported as a session fault rather than manufacturing a second terminal event.
No background callback panic can escape a goroutine and terminate the process.

If both execution and finalization fail, the returned typed error retains the
execution error as the primary run failure and the finalizer error as a separate
field. Errors are never silently discarded.

---

## §5 · Concurrency, quiescence, and shutdown

Rig configuration supplies separate lanes:

```go
type HustleLimits struct {
	BlockingConcurrent   int
	BlockingQueued       int
	BackgroundConcurrent int
	BackgroundQueued     int
	AuditTimeout         time.Duration
	FinalizationTimeout  time.Duration
	WorkerDrainTimeout   time.Duration
}
```

Both concurrent counts and all operation timeouts must be positive; queue
counts are non-negative and bounded. Each lane is FIFO by admission sequence.
Background work cannot borrow blocking capacity, so titles and summaries cannot
starve compaction or safety classification. Blocking work cannot consume
background capacity either.

For each lane, `Concurrent + Queued` is the maximum total queued-owned runs,
including waiting, executing, and finalizing states. The ownership-admission
token is held through finalization even though the narrower execution slot is
released first. Slow finalizers therefore cannot accumulate without bound or
allow an unbounded stream of new inference calls.

Three resources have distinct lifetimes:

- a **lane slot** bounds inference calls and is released before finalization;
- **drain ownership** covers every queued-owned run through finalization and is what
  shutdown joins; and
- a hub **activity entry** covers blocking runs through finalization and prevents
  a false `SessionIdle`. Background runs never acquire one.

The hub already owns the federated quiescence set. Add a private activity kind
for hustle `RunID`, using the same edge derivation and durable
`SessionActive`/`SessionIdle` rules as loop and delegate-wake activity. The
controller never edits hub state directly.

`Client.Invoke` runs in a capability-free worker that receives only a copied
client reference, immutable request, and a capacity-one result channel. The
controller selects between that result and execution-context cancellation. On
cancellation it gives the worker `WorkerDrainTimeout` to honor the canceled
context. If the worker still does not return, the controller atomically marks
itself poisoned, closes both lanes to new admission, disables further scheduler
grants, and removes/cancels every waiting queued-owned node for failed
audit/finalization. It then reports a typed session fault before abandoning the
channel. Only nodes executing at the poison transition can become abandoned
workers; no waiting or later run can invoke. A broken client therefore cannot
cause an unbounded goroutine leak. A late worker can only place its
result in the buffered channel and exit. It has no publisher, activity tracker,
finalizer, session controller, or lease capability, so it cannot act on a
stopped session. Usage returned only after abandonment is necessarily
unavailable to the terminal event and is recorded as nil.

Compaction normally begins while its loop already owns activity, so it does not
usually create a new public active edge. The separate entry still prevents the
session from becoming idle if the originating turn finishes while compaction's
finalizer is pending.

Shutdown order extends the landed session sequence:

```text
1. latch Session.closing and snapshot loops
2. close hustle admission
3. cancel queued and running hustle contexts
4. gracefully stop loops while the hub and checkpoint controller remain open
5. wait for hustle terminal audits and finalizers to drain
6. stop/join checkpoint and other session-owned controllers
7. append SessionStopped and stop the hub
8. release workspace and session leases
9. cancel the session context as the final backstop
```

Already-owned finalizers may commit before step 5 completes; no new run can
start after step 2. `SessionStopped` therefore cannot precede an owned
hustle's terminal audit or race its product finalizer.

Lifecycle appends use a controller-owned context derived from the still-live
session and bounded by `AuditTimeout`, not the inference execution context. A
provider timeout/cancellation therefore cannot prevent the runtime from trying
to record `HustleFailed`. Activity-edge publication uses the hub's existing
checked/fault-reporting semantics. Finalizers similarly use
`FinalizationTimeout`.

Activity release runs after finalization with a fresh session-derived context
bounded by `AuditTimeout`, never the expired caller/execution/finalization
context. `ActivityLease.Release` is concurrency-safe and idempotent. For a
committed lease, its first call removes the exact `RunID`, attempts the derived
activity-edge commit, and caches the result. For a partial lease, it performs
the silent rollback described in §3 and caches the acquisition error. Later
calls return the cached result. A commit failure reports the session fault but
cannot leak or re-add the in-memory activity entry.

Once shutdown latches closing, controller cleanup is non-abandonable: the
method does not stop the hub, release either lease, cancel the session context,
or return while a queued-owned run can still audit or finalize. The caller's
deadline cancels queue/inference work and is reported in the eventual typed
shutdown error, but it does not authorize unsafe resource release. Shutdown may
therefore outlive that deadline by the finite internal cleanup bound determined
by owned-run count plus `WorkerDrainTimeout`/`AuditTimeout`/
`FinalizationTimeout` and existing checkpoint bounds. Internal audit/finalizer
implementations are required and tested to honor their contexts; violating that
trusted contract is a bug, not a detached teardown mode. The session context is
canceled only after controller drain, hub stop, and lease-release ordering is
complete.

---

## §6 · Private durable audit

Hustle lifecycle is durable but absent from ordinary event subscriptions.

```go
type EventVisibility uint8

const (
	VisibilityPublic EventVisibility = iota
	VisibilityInternal
)

type Header struct {
	// existing fields
	EventVisibility EventVisibility `json:"visibility,omitzero"`
}

func (h Header) Visibility() EventVisibility
```

Zero remains public for journal compatibility. Visibility is metadata, not an
authorization mechanism.

`event.Event` gains `Visibility() EventVisibility`; embedding `Header` satisfies
it for every concrete event without per-type methods. The differently named
`EventVisibility` field avoids a field/method collision.

The controller creates lifecycle events with `VisibilityInternal`, stamps them
with the session event factory, then calls a checked internal publication path.
That path requires all of the following:

- `VisibilityInternal`;
- `event.Enduring`;
- the controller's session ID;
- a recognized hustle lifecycle type; and
- valid event coordinates/body.

It appends through the hub's existing durable appender and fault reporter but
does **not** apply hub activity mutations, run workspace boundaries, or deliver
to subscribers. Blocking activity is represented only by the explicit activity
entry in §5.

The ordinary `PublishEvent`/`PublishEventChecked` path rejects internal events,
and `event.ShouldDeliver` returns false for them as defense in depth. Only the
recognized checked internal path may append them. Conversely, that path rejects
public events. Replay-facing product streams apply the same default-deny filter.

No generic `Event.WithHeader` method is added. The landed event producers use
small exhaustive write-back switches because an embedded `Header` cannot clone
an arbitrary concrete event. Hustle lifecycle events are constructed and
stamped directly, so generic restamping is unnecessary.

Ordinary live and replay-facing product APIs exclude internal events before
delivery. A future privileged audit API must authenticate and authorize before
reading them and must redact sensitive fields. Raw hustle inputs/outputs,
prompts, credentials, commands, fetched content, and classifier reasoning are
never stored in lifecycle events.

Restore itself uses an unfiltered internal replay seam because it must repair
hustle aggregates. Public journal readers, serve catalog readers, and consumer
backlogs use a separate visibility-filtered replay seam. They fail closed on an
unknown visibility value and never depend on callers remembering to filter.

### Lifecycle events

`DefinitionDescriptor` stays in leaf package `pkg/hustle`. Run-time audit types
live in `pkg/event`, which may import `pkg/hustle` without a cycle:

```go
type HustleRunDescriptor struct {
	Definition hustle.DefinitionDescriptor
	RunID      hustle.RunID
	Runtime    ModelRuntime
}

type HustleStarted struct {
	enduring
	sessionScoped
	Header
	Run HustleRunDescriptor
}

type HustleCompleted struct {
	enduring
	sessionScoped
	Header
	Run      HustleRunDescriptor
	Duration time.Duration
	Usage    *content.Usage
}

type HustleFailed struct {
	enduring
	sessionScoped
	Header
	Run        HustleRunDescriptor
	Duration   time.Duration
	Stage      hustle.Stage
	ReasonCode hustle.ReasonCode
	Usage      *content.Usage
}
```

`Header.Cause` is the single direct-cause location. `HustleStarted` records the
owned request before it is scheduler-eligible, so its `Runtime` is always zero.
The terminal event records the resolved runtime when resolution succeeded; a
resolution failure retains a zero runtime and uses
`HustleFailed{Stage: StageModelResolution}`. No model resolution or inference
begins until the started append commits.

Failures store bounded stage/reason enums, not raw provider errors. Detailed
typed errors return to the in-process caller and normal security-safe logs.

---

## §7 · Inference request and output

Every run issues exactly one non-streaming request:

```text
client:       named definition client or originating loop's bound client
model:        named definition model or originating loop's committed live model
system:       hustle definition's versioned system prompt
messages:     one data-only user message containing the versioned JSON input
tools:        nil
output:       optional structured schema from the definition
sampling:     definition/model policy, fingerprinted
```

The backend calls `inference.Client.Invoke`, not loop runtime machinery. It does
not create committed conversation history, stream tokens, request permissions,
or execute tools.

Free-text definitions require exactly one assistant text result. Structured
definitions use `inference.DecodeOutput` from the structured-output design.
Mixed prose/tool output, empty output, unexpected tools, multiple candidate
values, unknown JSON fields, invalid enums, or an oversized result fail closed.

The adapter performs the final domain validation. For compaction, the output is
one bounded summary string. It is later stored as data-only conversation context
by `CompactionCommitted`; it cannot introduce a system message, tool call, gate
decision, or instruction block.

---

## §8 · Security and caller authority

A definition's prompt treats supplied content as untrusted data. Classifier
definitions additionally require constrained output. All definitions have no
tools and no ambient session/controller capability.

A classifier returns a verdict bound to the exact subject it judged:

```go
type ClassificationBasis struct {
	GateID             uuid.UUID
	ToolExecutionID    uuid.UUID
	SubjectDigest      [32]byte
	PolicyRevision     PolicyRevision
	ClassifierRevision ClassifierRevision
}
```

The typed output repeats the basis. Immediately before applying an allow result,
the caller recomputes the subject digest and checks gate ID, tool execution ID,
policy revision, classifier revision, authorization, gate kind, risk threshold,
and the live session security ceiling. Any mismatch, unknown value, invalid
schema, error, or ambiguity denies or escalates; it never auto-allows.

The runtime does not approve gates, mutate loop context, write titles, or choose
fallback policy. Those are separate caller responsibilities.

---

## §9 · Usage and catalog projection

```go
type TerminalStatus uint8

const (
	TerminalStatusUnknown TerminalStatus = iota
	TerminalStatusCompleted
	TerminalStatusFailed
)
```

The `inference.Response` is the only usage source. On success,
`HustleCompleted.Usage` carries it. If inference consumed tokens but extraction
or validation failed, `HustleFailed.Usage` carries it. Pre-inference failures
have nil usage. The controller validates and defensively copies usage before it
enters an enduring event; no event retains a provider-owned mutable pointer.

Usage is not copied onto an originating loop message and does not change that
loop's cumulative usage or current-context measurement.

`SessionMeta` may expose a bounded aggregate keyed by definition identity rather
than invocation-time resolution:

```text
(hustle name, model source, named model key when fixed, terminal status)
```

It folds only terminal lifecycle events. Individual runs remain in the journal
and are not enumerated in the catalog. A named definition has one fixed model
bucket. Every current-loop resolution for a definition folds into that
definition's single `ModelSourceCurrentLoop` bucket; its aggregate runtime is
zero/mixed while each terminal event retains the actual resolved runtime. This
is bounded and replay-order-independent even when `ChangeLoopInference`
installs arbitrarily many model keys.

---

## §10 · Restore and crash consistency

Restore folds hustle lifecycle only for audit and aggregate repair:

- `HustleStarted` without a terminal is an interrupted attempt;
- no goroutine, queue entry, inference request, or finalizer is reconstructed;
- terminal lifecycle without a caller product event means the result was not
  applied; and
- the caller re-evaluates its durable trigger and may create a new `RunID`.

For compaction:

```text
HustleCompleted committed
→ crash before CompactionCommitted
→ restored context remains uncompacted
→ pressure logic may run a new hustle
```

Product events own idempotency through their domain basis/CAS rules. Hustle
lifecycle never substitutes for a product event.

---

## §11 · Errors

All package/runtime failures have concrete typed errors suitable for
`errors.As`. Distinct kinds cover:

- definition and rig registration;
- binding and current-loop model resolution;
- input/version/size validation;
- lane full, cancellation, timeout, session closing, and session fault;
- internal audit append;
- inference invoke and capability mismatch;
- output extraction, size, JSON shape, and domain validation; and
- finalization, including combined execution/finalization failure.

Run-stage errors carry `{Name, RunID, Stage}` when a run exists and unwrap their
cause. Error strings and durable reason codes never include raw input/output or
credentials. No error returns a partially validated result.

---

## §12 · Planned definitions

| Name | Participation | Model source | Output | Initial status |
|---|---|---|---|---|
| `context.compact` | blocking | current loop or named | bounded text summary | implement first |
| `gate.command-safety` | blocking | named | structured classification | TODO |
| `gate.general-safety` | blocking | named | structured classification | TODO |
| `content.fetch-injection` | blocking | named | structured verdict | TODO |
| `content.tool-safety` | blocking | named | structured classification | TODO |
| `session.title` | background | named | bounded text | TODO |
| `session.stale-recap` | caller-selected definition | named | bounded text | TODO |
| `session.summary` | background | named | bounded text | TODO |

Participation is definition-time, not selected per invocation. If stale recap
needs both blocking and background semantics, it is registered as two names or
two distinct definitions; one invocation cannot widen its definition's policy.

Each TODO needs a concrete wire type, adapter, validation, caller policy,
prompt/schema revision, evaluation corpus, and tests. Adding one must not add
fields to unrelated domain types.

Prompt caching is a separate inference/provider design. No hustle correctness,
safety, fingerprint, or timeout behavior may depend on a cache hit.

---

## §13 · Testing

All tests are table-driven and run with `-race`. External provider/process
crossings use integration build tags. External JSON parsers receive fuzz tests.

Required coverage:

- immutable definition validation, defensive copies, duplicate options/names,
  canonical policy revisions, and secret exclusion;
- rig fingerprint ordering and changes for every behavior-affecting field;
- named and current-loop resolution, including committed live model changes,
  missing/exited loop, missing cause, and capability mismatch;
- wire version, empty/max/oversized input/output, unknown fields, malformed JSON,
  and domain validation;
- tool-less request construction and exact one-call `Invoke` behavior;
- checked internal audit: only recognized internal enduring lifecycle events,
  ordinary publish rejects internal visibility, `ShouldDeliver` denies it, no
  ordinary subscriber delivery, no workspace boundary, and append failure
  faults the run/session;
- lifecycle order and sanitized completed/failed events, including usage on
  post-inference validation failure;
- blocking/background FIFO lanes, queue bounds, reserved capacity, fairness,
  cancellation, shared-lane (not per-definition) ordering, ownership commit,
  and no cross-lane borrowing;
- activity begins before `HustleStarted`, remains through finalization, and ends
  exactly once on success/error/panic; background never changes quiescence;
- finalizer exactly once for every owned terminal path and never for validation
  or queue rejection before ownership; queued cancellation receives failure
  audit/finalization;
- ignored provider cancellation: controller abandons the capability-free worker,
  records nil late usage, faults/closes admission after `WorkerDrainTimeout`,
  bounds abandoned workers by existing execution slots, drains safely, and a
  late result cannot publish or finalize;
- validation/finalizer panic recovery faults the session, redacts panic values,
  releases ownership exactly once, and never escapes a background goroutine;
- shutdown closes admission, cancels, drains terminal audit/finalizers before
  `SessionStopped`; caller deadline cancels work but cleanup may outlive it and
  resources remain owned until bounded drain completes;
- restore treats unmatched starts as interrupted and repairs bounded aggregates
  from terminal events only;
- compaction commits only through `CompactionCommitted` and never changes loop
  usage with hustle usage; and
- classifier basis mismatch/error/unknown output never auto-allows.

Boundary tests must enforce:

- `pkg/hustle` does not import `pkg/event`, `pkg/rig`, `pkg/session`, or internal
  runtime packages;
- `internal/hustleruntime` does not import concrete hub/session implementations;
- models/tools cannot reach a hustle runner through public session, loop, or tool
  contracts; and
- hustle code cannot acquire workspace, gate, tool, or security-ceiling mutation
  capabilities.

---

## §14 · Module impact and sequence

- `core/content` — normalized usage only; no hustle types.
- `inference` — structured output as a later classifier prerequisite; no hustle
  import.
- `harness/pkg/hustle` — immutable definitions and serialization contracts.
- `harness/pkg/rig` — definitions, lane limits, validation, fingerprint.
- `harness/pkg/event` — visibility and three lifecycle events/codecs.
- `harness/pkg/hub` — checked append-only internal audit path and private hustle
  activity entries.
- `harness/internal/hustleruntime` — controller and inference execution.
- `harness/internal/sessionruntime` — model resolver, binding, lifecycle,
  shutdown, and restore fold integration.
- `swe` — product definitions, prompts, adapters, and evaluation corpora.

Implementation sequence:

1. normalized usage prerequisites;
2. text-only `pkg/hustle` definitions and rig fingerprinting (no output schema
   field until the structured-output prerequisite lands);
3. event visibility/lifecycle codecs and hub internal audit/activity seams;
4. per-session controller, binding, lanes, cancellation, and shutdown;
5. typed compaction adapter and product integration;
6. bounded catalog aggregate;
7. structured output; then classifier definitions individually.

## Result

Hustles are a small, reusable, session-owned inference mechanism rather than a
second class of agent. The design reuses the landed rig's immutable composition,
live model view, durable hub, quiescence set, and controller shutdown pattern
without weakening loop topology or adding restore exceptions. Each caller keeps
a narrow typed contract and sole authority over the result it applies.
