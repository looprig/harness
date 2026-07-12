# Token Usage, Context Measurement, and Compaction Design

**Date:** 2026-07-11

**Last revised:** 2026-07-12 (aligns compaction with one-shot, non-loop hustles;
terminal-event usage ownership; and bounded model-source catalog buckets)

**Status:** Draft

**Depends on:**

- `docs/plans/2026-06-13-content-llm-design.md` — the `core/content`
  message/chunk vocabulary extended here.
- `docs/plans/2026-07-10-rig-lifecycle-workspace-snapshots-design.md`
  (**Approved**) — immutable loop definitions, mutable per-loop model/mode,
  native step/turn/session boundaries, per-loop restore, and the rig composition
  root.
- `docs/plans/2026-06-19-event-persistence-checkpoint-design.md` — append-only
  journal, event classes, replay folds, and catalog repair.
- `docs/plans/2026-07-11-hustle-mechanism-design.md` — compaction summarization
  runs as a harness-owned one-shot inference hustle.

**Cross-module:** `core/content` owns normalized usage; `inference` owns the
request-counting contract and streaming result trailer; `llm` implements direct
provider counters where available; `harness` owns per-loop measurement,
pressure, commands, compaction, persistence, and restore; `swe` selects models,
hustles, and policy.

**New dependencies:** none. Exact remote counters use provider APIs through the
existing HTTP clients. A future local tokenizer requires a separate design and
explicit dependency approval.

---

## Problem

Provider usage is currently produced but lost on the streaming path. Harness
therefore cannot answer three different questions:

1. **How much has this loop consumed over its lifetime?** — cumulative metering.
2. **How large is the prompt we would send now?** — current context measurement.
3. **Will the next request fit this model while reserving output space?** —
   admission against a model-specific input limit.

Those questions must remain distinct. Cumulative usage never shrinks;
compaction deliberately shrinks the active context; a hustle owns its own
usage; and a model change changes both the applicable limit and potentially the
tokenizer.

The current native turn runtime also has two conversation views:

```text
loopState.msgs                      actor-owned durable committed history
turnConfig.base + turnState.msgs    turn-goroutine request history
```

Replacing only `loopState.msgs` does not compact an in-flight turn. The turn
goroutine would continue sending its frozen `base + msgs`. Compaction therefore
needs an explicit actor/turn context-replacement protocol.

## Decision summary

- Persist normalized request usage on the resulting `AIMessage`; do not attach
  usage to `UserMessage`.
- Capture streaming usage in a terminal `StreamResult` trailer.
- Model current context separately as a request measurement tied to a model and
  exact context revision.
- Count a complete `inference.Request`, never an isolated string. Direct
  provider counters live in `llm`; custom/gateway counters use an injected
  `inference.ContextCounterFunc` (bare func + declared `CounterCapability`) and
  declare their quality and trust posture.
- Derive the per-loop input limit from the current model's resolved context
  limits, requested output reservation, and safety margin.
- Expose pressure as a percentage, represented deterministically as integer
  basis points (`10_000 == 100%`).
- Route `Compact` through a coalescing control lane selected by command type,
  not by caller-supplied agency.
- Compact by replacing the whole active conversation with one summary message.
  Apply the replacement only if its `ContextBasis` still matches.
- Reset both the actor's committed history and the in-flight turn's private
  request history before the next model call.
- Fail open on a soft compaction failure only while the next request remains
  below the hard input limit. Never knowingly send an oversized request.

## Scope

**In scope:**

- normalized per-request usage and streaming capture;
- explicit `AIMessage` usage codecs and durable event round-trips;
- cumulative per-turn and per-loop usage;
- a model-aware complete-request counting contract;
- current context measurements with model, revision, quality, and input limit;
- percentage pressure with integer arithmetic;
- manual and automatic compaction for native conversational loops;
- the actor/turn replacement handshake;
- durable compaction basis, summary, and post-compaction measurement;
- replay/catalog behavior; and
- observe-only usage/pressure for foreign loops.

**Out of scope / deferred:**

- live mid-stream usage gauges;
- monetary cost calculation;
- a universal local tokenizer implementation;
- provider-native compaction features;
- automatic compaction of foreign loops; and
- prompt-cache policy and explicit breakpoints. A named TODO is recorded in
  §10.

---

## §1 · Canonical token and usage types (`core/content`)

Token counts are named, non-negative domain values:

```go
package content

type TokenCount uint64

type Usage struct {
	InputTokens         TokenCount // uncached prompt tokens
	OutputTokens        TokenCount // all generated tokens, including reasoning
	CacheReadTokens     TokenCount // prompt tokens read from cache
	CacheCreationTokens TokenCount // prompt tokens written to cache
	ReasoningTokens     TokenCount // subset of OutputTokens
}

func (u Usage) ContextTokens() (TokenCount, error)
func (u Usage) TotalTokens() (TokenCount, error)
func (u Usage) Add(other Usage) (Usage, error)
func (u Usage) Validate() error
```

The helpers use checked arithmetic and return typed overflow errors. Provider
decoders reject negative wire values before converting them to `TokenCount`.
`Validate` requires `ReasoningTokens <= OutputTokens`.

`AIMessage` gains optional usage:

```go
type AIMessage struct {
	Message
	Usage *Usage
}
```

`nil` means the provider did not report usage or the stream did not terminate
with an authoritative trailer. A present all-zero value remains distinguishable
from unknown.

### Why only `AIMessage` carries usage

One provider request produces one `AIMessage`, and the response trailer reports
usage for that complete request: system prompt, tools, all input messages, and
generated output. A `UserMessage` does not independently cause a provider call
and cannot truthfully own a request count. Its tokens are already included in
the next AI response's input usage or in an explicit `ContextMeasurement`.

Usage is metadata, not prompt content. Every request encoder must ignore
`AIMessage.Usage` when serializing history back to a provider.

### Cross-provider normalization

Codecs normalize provider fields into disjoint input categories:

| Domain field | Anthropic | OpenAI-compatible | Gemini |
|---|---|---|---|
| `InputTokens` | `input_tokens` | `prompt_tokens - cached_tokens - cache_write_tokens` | `promptTokenCount - cachedContentTokenCount` |
| `CacheReadTokens` | `cache_read_input_tokens` | `cached_tokens` | `cachedContentTokenCount` |
| `CacheCreationTokens` | `cache_creation_input_tokens` | `cache_write_tokens` when reported | `0` unless reported separately |
| `OutputTokens` | `output_tokens` | `completion_tokens` | `candidatesTokenCount + thoughtsTokenCount` |
| `ReasoningTokens` | thinking detail when reported | `reasoning_tokens` | `thoughtsTokenCount` |

Subtractions validate or floor at zero according to a typed normalization
policy; malformed impossible combinations are covered by codec tests. The
invariant is:

```text
ContextTokens == provider's effective total prompt size
ReasoningTokens <= OutputTokens
```

`inference.Usage` becomes an alias of `content.Usage`.

## §2 · Streaming result trailer (`inference`)

Usage remains out of the content `Chunk` union. It is terminal request metadata:

```go
type StreamResult struct {
	Usage        *content.Usage
	Model        string
	FinishReason FinishReason
}

// Result is authoritative only after Next returns io.EOF. The bool is false
// before EOF and after a non-EOF terminal error.
func (r *StreamReader[T]) Result() (StreamResult, bool)
```

The existing generic `StreamReader` type remains; only its terminal result
contract changes.

The frame-to-chunk adapter must propagate terminal metadata rather than dropping
it. Codec collectors accumulate:

- Anthropic `message_start` plus cumulative `message_delta` usage;
- OpenAI's final `include_usage` chunk; request encoding sets
  `stream_options.include_usage=true` when the dialect supports it; and
- Gemini's final `usageMetadata`.

An interrupted stream may legitimately have no authoritative result. `Close`
remains mandatory and idempotent.

## §3 · Stitch usage onto the response message

At the point `internal/loopruntime` finalizes exactly one `AIMessage` for a
request:

```go
if result, ok := stream.Result(); ok {
	aiMessage.Usage = cloneUsage(result.Usage)
}
```

The loop clones pointer-bearing metadata before publishing/committing so a
later mutation cannot change an enduring event. Foreign adapters normalize
their usage at the equivalent terminal boundary. An adapter reporting
cumulative process/session usage must first derive a validated monotonic delta;
it must not present cumulative values as per-request usage.

## §4 · Codec and persistence requirements

`AIMessage` must define its own JSON codec. Its embedded `Message.MarshalJSON`
would otherwise be promoted and silently drop `Usage`:

```go
type aiMessageJSON struct {
	Role   Role            `json:"role"`
	Blocks json.RawMessage `json:"blocks,omitempty"`
	Usage  *Usage          `json:"usage,omitempty"`
}

func (m AIMessage) MarshalJSON() ([]byte, error)
func (m *AIMessage) UnmarshalJSON(data []byte) error
```

The event codec must prove both message-bearing paths:

1. `StepDone.Messages` through the tagged message-slice codec; and
2. `TurnDone.Message` through the plain event codec.

A persist → replay → persist fixed-point test asserts byte-identical usage.
Dedicated request-encoder tests assert that usage never appears on provider
requests.

## §5 · Cumulative usage is not current context

Each loop owns its own cumulative usage. No primer or delegate usage is rolled
into another loop. Hustles are not loops; their usage is accounted separately
from terminal hustle lifecycle events.

```go
type LoopTokenState struct {
	CumulativeUsage content.Usage
	CurrentContext  ContextMeasurement
}
```

- `TurnDone.Usage` is the checked sum of the turn's step-message usage and is a
  convenience projection.
- Per-loop cumulative usage folds only the authoritative per-step request usage;
  it must not add `TurnDone.Usage` a second time.
- A successful compaction resets `CurrentContext`, never
  `CumulativeUsage`.
- A compaction hustle's usage stays on its terminal `HustleCompleted` or
  `HustleFailed` event and in the bounded hustle catalog aggregate.

## §6 · Model-aware context counting and limits

### Complete-request counter

`inference` owns the model identity used throughout counting and limits. It is a
single canonical type; all counting, measurement, and limit structures reference
it and never redefine it:

```go
// Package inference. Stable, secret-free identity of a resolved model: provider
// namespace plus the provider's own model id. Not a display name, not a catalog
// pointer, so replay/catalog repair never depends on a mutable external catalog.
type ModelKey struct {
	Provider string
	Model    string
}
```

`inference` defines the narrow optional capability and function adapter:

```go
type CountQuality uint8

const (
	CountQualityUnknown CountQuality = iota
	CountQualityExactProvider
	CountQualityExactLocal
	CountQualityHeuristicEstimate // structural estimate; NOT a proven upper bound
)

type ContextCount struct {
	Model       ModelKey
	InputTokens content.TokenCount
	Quality     CountQuality
}

type ContextCounter interface {
	CountContext(context.Context, Request) (ContextCount, error)
	// CounterCapability describes the counter's trust posture. It is metadata
	// only and must not perform I/O. Named CounterCapability (not Capability) so
	// the func adapter below can carry a Capability field without a
	// field/method collision.
	CounterCapability() CounterCapability
}

// ContextCountFunc is the bare counting function.
type ContextCountFunc func(context.Context, Request) (ContextCount, error)

// ContextCounterFunc adapts a bare counting function plus its declared capability
// into a full ContextCounter. A bare func cannot satisfy the interface because a
// counter must also declare its trust posture.
type ContextCounterFunc struct {
	Count      ContextCountFunc
	Capability CounterCapability
}

func (c ContextCounterFunc) CountContext(
	ctx context.Context,
	req Request,
) (ContextCount, error) {
	return c.Count(ctx, req)
}

func (c ContextCounterFunc) CounterCapability() CounterCapability {
	return c.Capability
}
```

The input is the exact `inference.Request`, including system, messages, tools,
model, and sampling. A string-only counter is not sufficient.

### Counter trust boundary

A remote counting endpoint receives the complete request — system prompt, tools,
and full history — in plaintext. If that endpoint is weaker than the loop's own
inference transport (a separate host, no attestation, a different retention
policy), an "exact" count leaks the conversation outside the protected path. The
counter therefore declares its posture, and composition refuses a downgrade:

```go
type CounterTransport uint8

const (
	CounterTransportUnknown CounterTransport = iota
	CounterTransportLocal                    // in-process; no request egress
	CounterTransportSameEndpoint             // same attested/E2EE path as inference
	CounterTransportSeparateEndpoint         // distinct host/security identity
)

type RetentionPosture uint8

const (
	RetentionUnknown  RetentionPosture = iota // fail-secure: treated as worst case
	RetentionNone                             // input discarded after counting
	RetentionEphemeral                        // bounded, provider-declared window
	RetentionLogged                           // input may be persisted provider-side
)

type CounterCapability struct {
	Provider         ProviderID       // owning provider/gateway
	Transport        CounterTransport // where request bytes travel
	SecurityIdentity SecurityIdentity // endpoint/attestation identity, or zero for local
	Retention        RetentionPosture // provider-declared retention of counted input
	TokenizerRev     TokenizerRevision
}
```

The loop's inference path declares a **symmetric** posture, and compatibility is a
single deterministic function rather than scattered ad-hoc checks:

```go
type InferenceCapability struct {
	Provider         ProviderID
	Transport        InferenceTransport // e.g. plaintext / attested / E2EE
	SecurityIdentity SecurityIdentity
	Retention        RetentionPosture
}

// CompatibleCounter reports why a counter is unacceptable for an inference path,
// or nil if it is acceptable. Deterministic and I/O-free so rig.Define and replay
// agree. A counter is compatible only when it egresses no further than inference
// (transport no weaker), its security identity is acceptable to the inference
// path, and its retention is no broader than inference's.
func CompatibleCounter(inf InferenceCapability, counter CounterCapability) error
```

`rig.Define` rejects a loop whose counter fails `CompatibleCounter`: a
`CounterTransportSeparateEndpoint` counter is invalid under an attested/E2EE
inference transport, and a retention posture broader than inference's is rejected.
For a protected provider a local `CountQualityHeuristicEstimate` may be the
*safer* choice than a remote "exact" endpoint; the runtime never silently prefers
exactness over the trust boundary. The **entire** secret-free `CounterCapability`
— provider, transport, security identity, retention, and tokenizer revision — is
fingerprinted into the measurement, so any counter swap (including a retention or
security-identity change) is auditable and invalidates prior measurements.

`llm` implements exact counters for direct providers whose APIs support them.
OpenAI-compatible gateways that lack a count endpoint may receive an injected
model-aware `ContextCounterFunc` (bare func + declared capability). Such a counter
must label itself accurately; an estimate never masquerades as exact.

When a client has no provider counter, composition supplies an
`inference.ContextCounterFunc`. The standard fallback is a deterministic
complete-request heuristic estimator in the `inference/contextcount`
subpackage; that package can depend on the root contracts and codec-specific
request shapes without creating a root-package import cycle. It declares
`CountQualityHeuristicEstimate` and `CounterTransportLocal`. Provider/codec tests
calibrate its structural allowances. Because it is a heuristic and not a proven
ceiling, the hard-admission path applies the configured `SafetyMargin` on top of
it; it is useful for early pressure and deterministic policy, but it is not
described as provider-exact.

### Resolved model limits

Context capacity is model metadata, not something learned from a response:

```go
type ContextLimits struct {
	WindowTokens    content.TokenCount // shared input + output window; 0 unknown
	MaxInputTokens  content.TokenCount // independent cap; 0 means derive/unknown
	MaxOutputTokens content.TokenCount // provider/model cap; 0 unknown
}
```

For one candidate request the margin is subtracted **last**, after the minimum,
so it applies in every branch — including the known-`MaxInputTokens`,
unknown-window case:

```text
reservedOutput = configured output reservation clamped to known MaxOutputTokens
rawInputLimit  = min(non-zero MaxInputTokens, WindowTokens - reservedOutput)
inputLimit     = rawInputLimit - SafetyMargin
```

Resolution rules make each unknown explicit rather than guessed:

- **Unknown window** (`WindowTokens == 0`): the `WindowTokens - reservedOutput`
  operand drops out of the minimum. If `MaxInputTokens` is known,
  `rawInputLimit == MaxInputTokens` and the margin is still subtracted
  (`inputLimit = MaxInputTokens - SafetyMargin`); otherwise the limit is unknown.
  The runtime never substitutes a default window.
- **Independent input cap** (`MaxInputTokens != 0`): it always participates in
  the minimum, even when a window is known, because some models cap input below
  `WindowTokens - reservedOutput`.
- **Default output reservation:** when the policy leaves `ReservedOutput` zero,
  resolution uses `min(MaxOutputTokens, policy default)`; if both are zero the
  limit is unknown rather than assuming the whole window is available for input.
- **Safety margin:** `SafetyMargin` is subtracted from `rawInputLimit` in every
  branch, never only from a shared window. For a `CountQualityHeuristicEstimate`
  measurement the runtime additionally requires the margin be non-zero (a
  heuristic count has no proven ceiling).

Invalid subtraction (any operand exceeding the minuend) or `inputLimit == 0`
yields an unknown/unavailable limit; the runtime never invents a denominator and
a policy that `RequireExact` cannot admit against an unknown limit fails closed.

### Context identity and measurement

`ContextRevision`, `ContextBasis`, and `ContextMeasurement` are owned by
**`harness/pkg/event`**, not the internal per-loop runtime. They are **fields of
durable events** (`CompactionCommitted`, `ContextMeasured`), so they must live in
`pkg/event`: the internal runtime imports `pkg/event`, so `pkg/event` cannot in
turn import the runtime without a cycle. They reference `inference`'s `ModelKey`
and `content`'s `TokenCount` (both below `event`) but are never defined in those
lower layers. Every committed conversation mutation advances a loop-local
revision:

```go
type ContextRevision uint64

type ContextBasis struct {
	Revision       ContextRevision
	ThroughEventID uuid.UUID
}

type ContextMeasurement struct {
	Basis              ContextBasis
	Model              ModelKey
	RequestFingerprint [32]byte
	InputTokens        content.TokenCount
	InputLimit         content.TokenCount
	Quality            inference.CountQuality
}
```

`ThroughEventID` is the last enduring context-mutating event included in the
measurement. `Revision` supplies cheap actor-local compare-and-swap; the event ID
supplies durable audit identity. `RequestFingerprint` covers the secret-free
effective request shape that affects counting: system/tool policy revisions,
model/sampling identity, context basis, and runtime-context revision. It is a
hash, not serialized prompt content.

A measurement is valid only for the exact
`{Basis, Model, RequestFingerprint}` tuple. A new user message, committed step
group, folded input, compaction, model change, mode change, or runtime-request
change invalidates or supersedes it.

### Model changes

On `LoopInferenceChanged` or `LoopModeChanged`, the numerator measured for the
old model is not divided by the new model's limit. Before the first request on
the new model, the runtime rebuilds and recounts the complete request using the
new model's counter. Waiting for the first AI response is too late because that
request may already exceed the new limit.

## §7 · Percentage pressure with integer arithmetic

Pressure remains a percentage for callers and users. Internally it is basis
points, avoiding floating-point thresholds and replay rounding:

```go
type BasisPoints uint16

const FullScaleBasisPoints BasisPoints = 10_000 // 100.00%

func OccupancyBasisPoints(used, limit content.TokenCount) (
	BasisPoints,
	error,
)
```

The helper uses checked integer cross-multiplication and clamps display values
at `10_000`; the raw counts remain available when over limit.

```go
type PressureLevel uint8

const (
	PressureUnknown PressureLevel = iota
	PressureNormal
	PressureCompact
	PressureHardLimit
)
```

`ContextPressure` is an ephemeral loop-scoped state-change signal containing the
measurement, percentage, previous level, and new level. It fires on a level
change rather than on every step. Current state is queryable from the loop/session
view and reconstructable from enduring measurements/events.

Threshold defaults and the exact rearm example remain intentionally unresolved
until the follow-up calibration discussion. The representation and ordering are
fixed by this design.

## §8 · `Compact` command and control lane

Manual and automatic compaction use one command:

```go
type Compact struct {
	Header
	identity.Coordinates
}
```

`Header.Agency` records provenance (`AgencyUser` for `/compact`,
`AgencyMachine` for policy). It does not select priority. The trusted session
boundary stamps agency after authentication; untrusted callers cannot assert
machine privileges.

The loop routes `Compact` by concrete command type into a bounded control lane.
Coalescing shares one *attempt* but never drops a command's reply obligation:

```go
type CompactAttemptID uuid.UUID

type PendingCompaction struct {
	AttemptID CompactAttemptID // identifies the single shared summarization attempt
	Waiters   []uuid.UUID      // every coalesced command id, bounded by the lane
	Reason    CompactionReason // the reason that first opened this attempt
}

type loopControlState struct {
	PendingCompaction *PendingCompaction // one coalescing slot, many waiters
}
```

Rules:

- machine triggers coalesce to one pending attempt;
- a user request joins an in-progress/pending compaction and observes its result;
  its command id is appended to `Waiters`;
- queue fullness for `UserInput` cannot reject compaction;
- `Interrupt` and `Shutdown` outrank compaction;
- no unbounded express queue exists; the waiter slice is bounded by the lane; a
  command that cannot join a full `Waiters` slice is **immediately rejected** with
  `CompactWaiterRejected{Reason: CompactRejectControlLaneFull}` rather than dropped
  or blocked — a journaled command always receives a terminal reply;
- `Waiters` is kept in a **canonical order** — ascending command-`CreatedAt`, ties
  broken by command-UUID bytes — and admits each command id **at most once**; the
  ordering and uniqueness are what make reply regeneration deterministic; and
- a control request is consumed only at a safe step/turn boundary.

### Crash-consistent outcome

The product and the attempt outcome must be **one** durable event, not two.
Committing the summary product first and an outcome/membership record second is
not atomic — the journal appends one event at a time, so a crash in between would
apply the summary yet lose the waiter membership, and repair could never run. The
canonical event therefore carries the replacement **and** the full membership:

```go
// Success: the ONE canonical event. It is BOTH the context replacement (folded in
// §12) and the attempt outcome + waiter membership. There is no separate product
// event to desync from.
type CompactionCommitted struct {
	enduring
	loopScoped
	Header
	AttemptID        CompactAttemptID
	WaiterCommandIDs []uuid.UUID // full canonical membership
	Basis            ContextBasis
	Summary          *content.UserMessage
	PostContext      ContextMeasurement
}

// Failure: the canonical negative outcome, likewise carrying full membership.
type CompactionRejected struct {
	enduring
	loopScoped
	Header
	AttemptID        CompactAttemptID
	WaiterCommandIDs []uuid.UUID
	RejectReason     CompactRejectReason
}
```

Exactly one of these is written per attempt. Because it already contains the full
waiter set, the per-command terminal replies are an **idempotent projection** of
it — and each reply's event id is **derived deterministically** so replaying the
repair can never append a duplicate:

```go
// Deterministic, content-addressed reply id. Regenerating a reply after a crash
// yields the SAME id, so the append is a no-op if it already exists.
func waiterReplyID(attempt CompactAttemptID, cmd uuid.UUID, ok bool) uuid.UUID

type CompactWaiterResolved struct {
	enduring
	loopScoped
	Header                        // ID = waiterReplyID(AttemptID, CommandID, true)
	AttemptID           CompactAttemptID
	CommittedEventID    uuid.UUID // the CompactionCommitted this waiter observed
}

type CompactWaiterRejected struct {
	enduring
	loopScoped
	Header                    // ID = waiterReplyID(AttemptID, CommandID, false)
	AttemptID CompactAttemptID
	Reason    CompactRejectReason
}
```

On restore, each command in the canonical event's `WaiterCommandIDs` with no
matching reply is regenerated from the outcome (resolved cite the
`CompactionCommitted` id, rejected cite `RejectReason`). Because reply ids are a
pure function of `{AttemptID, CommandID, outcome}`, a crash *during* repair cannot
double-append: the same id is produced and the existing record wins. Every waiter
ends with exactly one terminal reply, and no command is left owed — even across a
crash between the canonical event and its replies.

## §9 · Full replacement and the actor/turn protocol

### Meaning of full replacement

If the active conversation is:

```text
[old history, current user, AI tool call, tool result, latest AI message]
```

successful compaction changes it to exactly:

```text
[summary]
```

It does **not** retain the latest AI message or tool result beside the summary.
Those messages were input to the summarizer and their salient facts must be in
the summary. Keeping them would reintroduce tool-pair boundary rules and make the
post-compaction state dependent on where the cut occurred.

### Summary form

The replacement is one synthetic `content.UserMessage` containing one non-empty
text block:

```xml
<conversation_summary>
  <goal>...</goal>
  <constraints>...</constraints>
  <decisions>...</decisions>
  <state>...</state>
  <open_items>...</open_items>
</conversation_summary>
```

The role is `user` so the model can produce the next assistant step. The loop's
trusted system prompt explains that this block is data-only remembered context,
not a new instruction or an authority grant.

### Internal `ContextReplacement`

`ContextReplacement` is a single-purpose internal actor command/handshake. It is
not a general public `UpdateAgenticMessages` API; arbitrary consumers must not be
able to rewrite agent history.

```go
type ContextReplacement struct {
	Basis   ContextBasis
	Model   inference.ModelKey // model the summarized measurement was taken under
	Fprint  [32]byte           // RequestFingerprint of that measurement
	Summary *content.UserMessage
}
```

The actor applies it as a compare-and-swap over the **full** measurement
identity, not `Basis` alone. A measurement is valid only for
`{Basis, Model, RequestFingerprint}` (§6), so a model or request-shape change
that left `Basis` numerically equal must still invalidate the replacement:

```go
func applyContextReplacement(
	state *loopState,
	replacement ContextReplacement,
) error {
	if state.contextBasis != replacement.Basis ||
		state.model != replacement.Model ||
		state.requestFingerprint != replacement.Fprint {
		return &StaleCompactionError{
			Expected: replacement.Basis,
			Actual:   state.contextBasis,
		}
	}
	state.msgs = content.AgenticMessages{replacement.Summary}
	state.contextBasis = nextBasis(/* CompactionCommitted event */)
	return nil
}
```

Equivalently, the actor may CAS on `Basis` alone and then **recompute
`PostContext`** against its post-accept state before minting `CompactionCommitted`;
the
two are interchangeable, but the design fixes the full-tuple CAS as the default
because it fails the stale attempt before any durable append.

### In-flight turn reset

At turn start, the runtime clones committed history into `turnConfig.base`; the
turn goroutine then owns `turnState.msgs`. Therefore a successful actor
replacement must be acknowledged back to that goroutine:

```go
// After the actor has durably appended CompactionCommitted and applied the replacement:
turnConfig.base = content.AgenticMessages{}
turnState.msgs = content.AgenticMessages{summary}
```

Turn identity, turn index, tool-iteration counters, and command causation remain
unchanged. Only the request context is reset. The next step of the same turn
sends the summary-based context.

### Deterministic boundary sequence

For a tool-using step:

```text
finalize AIMessage + tool results + usage
→ actor commits StepDone and advances ContextBasis
→ complete the configured native workspace boundary
→ build the exact candidate next request
→ count it and append ContextMeasured when the measurement changed
→ evaluate pressure from that durable measurement
→ return a compaction directive to the turn goroutine when required
→ run the compaction hustle while the turn is paused
   (the actor remains responsive to interrupt/shutdown)
→ validate output and mint the proposed CompactionCommitted/new ContextBasis
→ build and count the proposed summary-based next request
→ construct CompactionCommitted{AttemptID, Waiters, old Basis, Summary, PostContext}
→ construct ContextReplacement{old Basis, Model, Fprint of the measurement}
→ actor CAS-validates {Basis, Model, RequestFingerprint}
→ append CompactionCommitted durably
→ actor replaces loopState.msgs
→ acknowledge replacement to the turn goroutine
→ turn goroutine replaces base + turnState.msgs
→ continue the same turn (next model step uses the summary-based context)
```

Not every StepDone continues the turn. The trigger point is the same, but the
boundary sequence forks on whether the step that crossed the threshold is a tool
continuation or the turn's terminal AI response:

- **Tool continuation** (the AI message requested tools): the turn *will* make at
  least one more model call. Compact, reset both context views (actor + turn
  goroutine), and the next step sends the summary-based context — the sequence
  above.
- **Terminal AI response** (no tool calls; the turn is ending): there is **no
  next model call in this turn**, and the original terminal AI response is
  **preserved and returned unchanged** — compaction never rewrites a reply the
  user is about to see. But the turn must **not** be returned first: returning or
  emitting `TurnDone` before compaction opens a race where new input or idleness
  arrives and makes the compaction basis stale. Compaction runs while the turn is
  still active, and only then does the turn close:

  ```text
  commit StepDone (advances ContextBasis)
  → retain the original terminal AIMessage (never mutated)
  → perform or decline compaction while the turn is STILL ACTIVE
  → on success: append CompactionCommitted and apply ContextReplacement to committed state
  → emit TurnDone carrying the unchanged original AIMessage
  → return the response
  ```

  No additional model call is made. There is no in-flight `turnState.msgs` to
  reset because the turn closes only after compaction; `CompactionCommitted`
  conditions the *next* turn's request, never this response.

This preserves the invariant that a summary measured for continuation is only
ever spent on a *future* request, never retroactively on the response already
produced — and, for the terminal case, that the turn does not become returnable
until compaction has committed against a basis that cannot have gone stale.

Queued user/subagent input that has not yet committed is not part of the
compaction basis. It remains queued and folds after the replacement according to
normal input semantics.

### Durable event

The success case is the single `CompactionCommitted` event defined in §8 — it is
simultaneously the durable product (summary + basis + post-context) **and** the
attempt outcome (waiter membership). There is deliberately no separate `Compacted`
event to fall out of sync with it.

`CompactionCommitted.Basis` identifies what was summarized. `PostContext` measures
the primary loop's summary-based request context; it is not the hustle's usage.
The hustle's own usage remains on its terminal `HustleCompleted` or
`HustleFailed` event.

If future runtime context is not yet knowable (for example, idle manual
compaction before a future turn), `PostContext` measures the stable request base
and its quality reflects that boundary. The next admitted user message produces
a new measurement before inference.

## §10 · Compaction hustle and prompt caching

The compactor receives the exact active conversation at `ContextBasis`, the
current resolved model, and a maximum summary-output budget. It preserves goal,
constraints, decisions, rationale, concrete workspace/tool state, unresolved
threads, and next actions while dropping redundant deliberation and verbose raw
output.

The compactor uses no tools and treats the transcript as untrusted data. A
dedicated compaction system prompt is preferred when it is safer or more
reliable than reusing the agent's system prompt.

Provider prefix caching is an optimization, never a correctness assumption.
The current Looprig request model has no provider-neutral cache intent, the
Anthropic codec does not enable `cache_control`, and OpenAI/Gemini implicit cache
behavior is provider/model-specific.

**TODO — prompt-cache design:** define provider-neutral cache intent and stable
prefix identity in `inference`, translate it to provider policy in `llm`, add
usage normalization for cache writes/reads, and test exact request-prefix
stability. Do not block compaction implementation on this TODO.

## §11 · Soft compaction and hard admission

Compaction has two different decisions:

1. **Soft pressure:** attempt compaction early enough to preserve an output
   reserve.
2. **Hard admission:** never knowingly send a request whose measured input
   exceeds `InputLimit`.

If the compaction hustle fails below the hard limit, history is unchanged and
the turn may continue. If it fails at/above the hard limit, the next model call
is rejected with a typed context-limit error.

A failed automatic attempt is recorded against the current `ContextBasis`.
There is at most one automatic attempt per unchanged basis. A later context
mutation may retry; failure does not disarm automatic compaction forever.
Manual `/compact` may explicitly retry the same basis.

After a successful replacement, the summary-based request is counted. If fixed
system/tool/runtime context plus the summary still exceeds the hard limit, the
runtime returns `SummaryTooLargeError` and does not call the primary model.

## §12 · Restore and catalog

The per-loop replay fold treats `CompactionCommitted` as a complete context reset
— the same event that carries the outcome is the one that resets the context, so
product and outcome can never disagree:

```go
case event.CompactionCommitted:
	if err := validateBasis(foldedBasis, e.Basis); err != nil {
		return foldResult{}, err
	}
	msgs = content.AgenticMessages{cloneUserMessage(e.Summary)}
	basis = basisFromCommitted(e)
	contextMeasurement = e.PostContext
```

Multiple compactions compose in journal order. Raw superseded messages remain
in the append-only journal for audit; only the active-context fold resets.

Restore also repairs incomplete waiter replies: for each `CompactionCommitted` or
`CompactionRejected`, any command in `WaiterCommandIDs` lacking a matching
`CompactWaiterResolved`/`CompactWaiterRejected` is regenerated idempotently from
the recorded outcome, using the deterministic `waiterReplyID` so a crash during
repair cannot double-append. This closes the crash-between-outcome-and-replies gap
(§8) — every waiter deterministically ends with exactly one terminal reply after
replay.

The journal must contain the resolved runtime whenever the effective model
changes. `ModelRuntime` is **owned by `harness/pkg/event`** (exact owner
`event.ModelRuntime`, not the ambiguous "harness") because it is a durable event
payload, and it is the **single** resolved-runtime shape shared with the
rig-lifecycle events — so it carries not just the model and its limits but the
resolved reasoning **effort**, which the lifecycle design's mode resolution
requires. There is one runtime type, not a limits-only variant here and an
effort-carrying variant there:

```go
// Package event. The single resolved-runtime payload, shared by the token/limits
// machinery and the rig-lifecycle loop events.
type ModelRuntime struct {
	Key    inference.ModelKey
	Limits inference.ContextLimits
	Effort inference.Effort // resolved reasoning effort (mode resolution result)
}

type LoopInferenceChanged struct {
	// existing identity fields
	Runtime ModelRuntime
}

type LoopModeChanged struct {
	// existing mode fields
	Runtime ModelRuntime // resolved result of selecting the mode (model + limits + effort)
}
```

`LoopStarted` likewise carries the initial resolved runtime. This makes replay
and catalog repair independent of a mutable external model catalog, and gives the
rig-lifecycle runtime the effort it needs without a second type. `inference.Effort`
is the reasoning-effort type owned by `inference`; if the rig-lifecycle design
already names that type, `ModelRuntime` uses that exact type rather than
duplicating it.

`SessionMeta` stores a deterministically ordered per-loop projection for the
rig's bounded, user-visible primer/delegate topology. Hustles are not loops and
therefore never enter `Loops`. Their detailed runs live in the journal; the
catalog exposes only a bounded aggregate keyed by hustle name/model-source
bucket/status:

```go
type LoopUsageMeta struct {
	LoopID          uuid.UUID
	Runtime         ModelRuntime
	CumulativeUsage content.Usage
	CurrentContext  ContextMeasurement
}

type HustleUsageAggregate struct {
	Name            hustle.Name
	ModelSource      hustle.ModelSource
	Model            ModelRuntime          // named source only; zero means current-loop/mixed
	Status          hustle.TerminalStatus // completed / failed, bounded set
	Runs            uint64                // count folded from lifecycle events
	CumulativeUsage content.Usage         // summed terminal-event usage
}

type SessionMeta struct {
	// existing fields
	Loops   []LoopUsageMeta        // primers/delegates, sorted by LoopID bytes
	Hustles []HustleUsageAggregate // bounded: one row per definition model-source bucket/status
}
```

`SessionMeta` remains a repairable cache, not an authority. Carrying the durable
runtime/measurement allows the session picker to show pressure and repair without
importing the current rig/model catalog. The hustle aggregate is folded from
internal `HustleCompleted` and `HustleFailed` events; `HustleStarted` carries no
usage. Individual hustle runs are recoverable from the journal but are never
enumerated in `SessionMeta`.

The aggregate key is `(name, model source, named model key, status)`. A named
hustle has one fixed model row. Every current-loop resolution folds into one
row per `(name, current-loop, status)` with zero/mixed `Model`; terminal events
retain their actual resolved runtime for detailed audit. The fold is bounded and
replay-order-independent even if `ChangeLoopInference` installs arbitrarily many
model keys. `hustle.TerminalStatus` is a fixed two-value set (completed/failed).

## §13 · Configuration

The presence of `WithCompaction` installs manual compaction. Automatic behavior
is explicit:

```go
type CompactionPolicy struct {
	Automatic        bool
	CounterPolicy    CounterPolicy
	CompactAt        BasisPoints
	RearmBelow       BasisPoints
	ReservedOutput   content.TokenCount
	SafetyMargin     content.TokenCount
	MaxSummaryTokens content.TokenCount
	CountTimeout     time.Duration
	Hustle           hustle.Name
}

loop.WithCompaction(policy)
```

No option means compaction is unavailable. `Automatic: false` means manual only.
There is no `Threshold == 0` magic value.

```go
type CounterPolicy uint8

const (
	CounterPolicyUnknown CounterPolicy = iota
	CounterPolicyRequireExact
	CounterPolicyAllowConservative
)
```

Validation requires:

- `0 < RearmBelow < CompactAt < 10_000` when automatic;
- a non-zero counter policy when automatic;
- an exact counter for `RequireExact`, or an exact/heuristic counter for
  `AllowConservative`;
- non-zero summary/output budgets;
- positive timeout after default resolution;
- a registered hustle whose output satisfies the compactor contract; and
- a counter whose `CounterCapability` is compatible with the loop's inference
  transport per the §6 trust boundary and §14 counter policy.

Threshold percentages/defaults are deliberately left for the follow-up
soft/rearm example and calibration pass.

## §14 · Event, error, and counter policy

### Event / command summary

| Item | Kind | Class/visibility | Persisted | Purpose |
|---|---|---|---|---|
| `Compact` | command | intent log | yes | manual or machine compaction request |
| `ContextPressure` | event | Ephemeral/Public | no | percentage level change |
| `ContextMeasured` | event | Enduring/Public | yes | authoritative/replayable current measurement |
| `CompactionCommitted` | event | Enduring/Public | yes | canonical success: context replacement **and** outcome + waiter membership (folded as the reset) |
| `CompactionRejected` | event | Enduring/Public | yes | canonical failure: reject reason + full waiter membership |
| `CompactWaiterResolved` | event | Enduring/Public | yes | per-waiter success reply; deterministic id, idempotent projection |
| `CompactWaiterRejected` | event | Enduring/Public | yes | per-waiter reject reply (failure/cancel/shutdown/stale-basis/lane-full) |

Every successful measurement that changes the current measurement appends
`ContextMeasured` before policy acts on it. The sole exception is the
post-replacement measurement embedded in `CompactionCommitted`; the runtime does not emit
a duplicate `ContextMeasured` for the same
`{Basis, Model, RequestFingerprint}` tuple. Catalog/replay treat measurements as
latest-value state, never as cumulative usage.

### Typed errors

- `UsageValidationError` and `UsageOverflowError`;
- `ContextCountError{Model, Quality, Cause}`;
- `ContextLimitUnknownError{Model}`;
- `ContextLimitError{Measurement}`;
- `CompactionError{SessionID, LoopID, Basis, Cause}`;
- `StaleCompactionError{Expected, Actual}`;
- `InvalidSummaryError{Reason}`;
- `SummaryTooLargeError{Measurement}`; and
- command validation/routing errors for missing coordinates or invalid agency.

All errors unwrap their cause when applicable.

### Missing exact provider counters

Automatic compaction always requires a `ContextCounter`, but it need not be a
provider endpoint. A loop may use the deterministic inference fallback only when
its policy explicitly selects `CounterPolicyAllowConservative`.

`CounterPolicyRequireExact` rejects a loop at `rig.Define` when its resolved
counter cannot return `ExactProvider` or `ExactLocal`. `AllowConservative`
accepts the inference fallback, persists/exposes measurement quality, and uses
earlier safety margins; it never labels the hard admission result as a provider
fit guarantee. No silent fallback or runtime policy change is allowed. Counter
policy and estimator revision are fingerprinted and durable.

## §15 · Hustle isolation

Hustles are one-shot inference operations, not durable loops. They have no
`LoopStarted`, committed message history, context measurement, compaction
trigger, workspace boundary, or restoreable actor. Their usage comes only from
terminal internal hustle lifecycle events. See the hustle design for private
audit, explicit blocking activity, shutdown drain, and restore behavior.

## §16 · Testing plan

All unit tests are table-driven and run with `-race`; external provider count
endpoints receive integration tests under the `integration` build tag.

- `content.Usage`: zero, cache-only, reasoning subset, checked addition,
  overflow, negative wire input, invalid reasoning.
- `AIMessage` JSON: nil/present usage; embedded-codec regression; fixed-point.
- Provider codecs: stream and invoke normalization, cache fields, reasoning,
  missing/interrupted trailer, malformed totals.
- `StreamResult`: unavailable before EOF, available after EOF, unavailable after
  terminal error, propagated through frame-to-chunk adapter.
- Request encoders: usage is never sent back to any provider.
- Counters: complete request mapping includes system/messages/tools; model
  mismatch; exact/estimate quality; timeouts; gateway unsupported behavior.
- Limits: output reservation, independent input cap, unknown zero limits,
  checked subtraction, integer basis-point boundaries.
- Model changes: old measurement invalidated; new model counted before first
  request; smaller-window hard rejection.
- Express lane: queue-full independence, coalescing, user join, interrupt and
  shutdown priority, command type rather than agency.
- Per-waiter replies: every coalesced waiter gets exactly one terminal
  `CompactWaiterResolved`/`CompactWaiterRejected`; success replies cite one
  `CompactionCommitted` id; failure, cancellation, shutdown, and stale basis each
  reject every waiter; a command that cannot join a full `Waiters` slice is
  immediately rejected with `CompactRejectControlLaneFull`; `Waiters` is canonical
  ordered and de-duplicated.
- Crash consistency: the single `CompactionCommitted`/`CompactionRejected` carries
  full membership; a crash between it and the per-waiter replies is repaired on
  restore; reply ids are deterministic (`waiterReplyID`) so repair after a
  mid-repair crash cannot double-append; every waiter ends with exactly one reply.
- Counter trust boundary: `CompatibleCounter` rejects a separate-endpoint or
  broader-retention counter under an attested/E2EE loop; a local heuristic is
  admitted; the **entire** `CounterCapability` (incl. retention + security
  identity) is fingerprinted into the measurement.
- Replacement: actor-only reset is proven insufficient; successful handshake
  resets both actor and turn state; stale basis **or model/fingerprint change**
  cannot overwrite; queued input survives; same turn continues.
- Compaction step fork: a tool-continuation step resets the turn view and issues
  a next model call; a terminal AI-response step returns the original response
  unchanged, commits `CompactionCommitted` before `TurnDone`, and does **not**
  re-invoke the model.
- Compaction: summary-only active history, no retained last AI/tool result,
  hustle usage isolated, post-context belongs to primary loop, soft failure
  continues, hard failure blocks, successful summary still-too-large blocks.
- Restore: multiple compactions, basis validation, raw journal retained,
  resolved runtime restored, catalog deterministic.
- Hustles: usage owned independently by terminal lifecycle events; no recursive
  occupancy/compaction or workspace hooks.

### Foundation types land first

This design's first shippable unit is a small, self-contained **foundation** of
shared types with unambiguous owners. It has no dependency on any other type
introduced later in this document and must land before anything — in this design
or any consumer — references a resolved model runtime or context limits:

| Type | Owning package |
|---|---|
| `content.TokenCount` | `core/content` |
| `inference.ModelKey`, `inference.ContextLimits`, `inference.Effort` | `inference` |
| `hustle.DefinitionDescriptor`, `hustle.Name`, `hustle.ModelSource`, `hustle.Participation` | `harness/pkg/hustle` (leaf; must **not** import `pkg/event`) |
| `event.EventVisibility` (durable wire) | `harness/pkg/event` |
| `event.ModelRuntime` (`{ModelKey, ContextLimits, Effort}`) | `harness/pkg/event` |
| `event.ContextBasis`, `event.ContextMeasurement` | `harness/pkg/event` |
| `event.HustleRunDescriptor` (`{hustle.DefinitionDescriptor, hustle.RunID, event.ModelRuntime}`) | `harness/pkg/event` |

**Dependency direction is one-way and acyclic:** `pkg/hustle` is a leaf owning
only definition-time identity (`DefinitionDescriptor`, `Name`, `ModelSource`,
`Participation`) and
imports neither `pkg/event` nor the runtime. `pkg/event` imports `pkg/hustle`
(for `DefinitionDescriptor`) and `inference`/`content`, and owns every durable
**event field** — `ContextBasis`, `ContextMeasurement`, `ModelRuntime`, and
`HustleRunDescriptor` — because the internal runtime imports `pkg/event` and a
type used in a durable event therefore cannot live in the runtime. The import
chain is `internal/sessionruntime` → `internal/hustleruntime` → `pkg/event` →
`pkg/hustle` → `inference`/`content`, with `pkg/event` also importing the leaf
model/content packages directly. There is no back-edge from a public package to
either internal runtime. This design provides all foundation types before a
consumer references them.

### Release order

1. `core/content` — `TokenCount`, normalized `Usage`, explicit `AIMessage` JSON
   codec.
2. `inference` — `ModelKey`, `Effort`, `StreamResult`, usage trailer
   propagation, `ContextCounter`, `CounterCapability`, `ContextCounterFunc`,
   `ContextLimits`, codec normalization.
3. `llm` — exact provider counters where supported; typed unsupported behavior
   for gateways; recompile against inference/content.
4. `harness` — usage stitch, context basis/measurement, pressure, express-lane
   `Compact` + per-waiter replies, actor/turn replacement, events/codecs/folds/
   catalog and compaction hustle integration.
5. `swe` — model limits, counters or explicit missing-counter policy, hustle
   registry, percentage thresholds, `/compact`.

Release in dependency order: content → inference → llm → harness → swe. Update
vendored/replaced copies in lockstep.

## Result

The design produces two honest per-loop views:

```text
cumulative usage: historical and monotonic
current context:  model-specific, revision-specific, replaceable by compaction
```

Compaction replaces the complete active conversation with a summary, resets the
in-flight request view as well as durable actor state, and measures the resulting
primary request independently of separately accounted hustle usage. Percentage
policy stays human-readable while integer basis points, exact context bases,
typed limits, and durable model snapshots make replay deterministic.
