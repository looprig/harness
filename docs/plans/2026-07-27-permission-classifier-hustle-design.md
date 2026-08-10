# Permission Auto-Review Classifier and Tool-Using Hustles

**Date:** 2026-07-27

**Status:** Approved

**Scope:** Harness, a new `github.com/looprig/classifiers` module, and
consumer composition such as Carbon.

**Reference implementation:** Codex Guardian in
`codeagents/codex/codex-rs/core/src/guardian`.

## Relationship to existing designs

This document is an additive amendment to:

- `2026-06-18-gates-package-design.md`;
- `2026-07-01-open-gate-posture-design.md`;
- `2026-07-11-hustle-mechanism-design.md`; and
- `2026-07-11-structured-output-design.md`.

It is authoritative for automatic review of permission gates.

The original gates design reserves `PolicyModelDecide` for a timeout response
on non-critical, non-side-effecting gates and explicitly rejects it for
permission gates. That rule remains correct for `ResponsePolicy`. Permission
auto-review is not a timeout response and does not use
`ResponsePolicy.ModelDecision`. It is a separate, immediate, classifier-owned
review that races the human response while preserving the ordinary gate claim,
validation, routing, grant, and audit paths.

The original Hustle design defines v1 as one tool-less inference invocation and
lists `gate.command-safety` and `gate.general-safety` as follow-ons. This
document supplies the missing classifier design and extends Hustles with an
optional bounded evidence-tool loop. Tool-less definitions, including context
compaction, retain their exact current behavior.

## 1. Problem

A prepared tool request can reach a permission gate even when the requested
action is safe and clearly follows from the user's instructions. Requiring a
human to answer every such gate creates latency and trains users to approve
without reading.

Harness already has the required lower-level pieces:

- tools convert untrusted arguments into typed `tool.Request` values;
- `gate.Evaluator` applies configured access, stored deny, stored allow, and
  one combined human approval;
- the session owns durable, exactly-once gate state;
- structured-output Hustles provide bounded auxiliary inference with private
  lifecycle audit; and
- the sandbox and tool preparation contracts enforce the action after approval.

What is missing is a trusted caller that can use a classifier result as
evidence, revalidate the exact subject, and submit only a one-shot approval.
The classifier must understand user authorization and intrinsic risk. It must
also be able to gather read-only evidence when risk depends on local state,
such as the identity of a deletion target or a repository's remote.

## 2. Decision

Build permission auto-review in two layers:

1. **Harness mechanism**
   - extends Hustles with optional, bounded, read-only evidence tools;
   - defines neutral permission-review contracts in `pkg/gate`;
   - captures the exact live review context in the loop runtime;
   - runs applicable classifiers through the session-owned Hustle controller;
   - validates basis, policy, evidence, and live gate state locally; and
   - races a classifier one-shot approval against the existing human gate.

2. **Classifier product**
   - lives in a separate `github.com/looprig/classifiers` module;
   - provides the initial `gate.command-safety` classifier;
   - owns prompts, schemas, typed wire adapters, risk policy, evaluation
     corpus, and versioning; and
   - is selected explicitly by consumers.

Harness never imports the classifiers module. The classifiers module imports
public Harness contracts. A consumer imports both and selects which classifiers
to register.

## 3. Goals

The complete implementation must:

1. Auto-approve a permission gate only when one or more explicitly enabled
   classifiers establish that the exact request is allowed.
2. Preserve the human gate for every non-allow result.
3. Let the human answer while automatic review is running.
4. Restrict automatic approval to `gate.ApprovalApprove`.
5. Never persist an allow rule, mint a grant, deny a tool call, close a gate,
   or widen the session security ceiling from classifier code.
6. Bind every verdict to the exact gate, tool execution, prepared request,
   context revision, classifier revision, gate policy, and security ceiling.
7. Support active, capability-constrained, read-only investigation.
8. Keep classifier inputs, evidence, model output, and reasoning out of durable
   product events.
9. Reuse Hustle admission, timeout, usage, audit, cancellation, and shutdown.
10. Match or exceed Codex Guardian's behavioral and safety coverage.
11. Support multiple independently selectable classifiers without allowing one
    permissive classifier to override a stricter applicable classifier.
12. Keep the new classifiers repository free of root-level Go source files.

## 4. Non-goals

The first release does not:

- auto-deny permission gates;
- add "approve for me" as a fourth human gate action;
- use `ResponsePolicy.ModelDecision`;
- auto-approve ask-user, form, open-URL, or extension gates;
- persist workspace allow rules;
- expose arbitrary shell execution to a classifier;
- let a classifier invoke mutating or approval-gated tools;
- restore or replay an interrupted classifier run;
- make provider prompt caching part of correctness;
- publish model reasoning or evidence contents;
- replace access profiles, stored rules, grant enforcement, or sandboxing; or
- make classifier output an authorization capability.

## 5. Codex Guardian compatibility bar

Codex Guardian is the minimum behavioral bar. The implementation must cover
the following Guardian properties:

| Guardian property | Required Harness behavior |
| --- | --- |
| Canonical planned action | Exact typed `tool.Request` plus subject digest |
| User-intent reconstruction | Bounded, labeled context snapshot retaining user intent |
| Read-only investigation | Capability-enforced evidence tools |
| Risk classification | Closed low/medium/high/critical enum |
| Authorization classification | Closed unknown/low/medium/high enum |
| Strict structured result | Native output schema plus local domain validation |
| Locked-down reviewer | No mutation, delegation, approval, grants, or session control |
| Timeout | Human gate remains open |
| Malformed output | One bounded retry when classified retryable, then human fallback |
| Review audit | Private Hustle lifecycle plus redacted assessment events |
| Human override | Human and classifier race the same gate claim |
| Repeated rejection breaker | Bounded per-turn/session circuit breaker |
| Stable policy prefix | Immutable prompt and policy revisions in identity |
| Parallel reviews | Bounded by the existing blocking Hustle lane |

Harness improves the authority boundary in four ways:

1. The classifier reviews the tool-owned prepared request rather than a
   display-oriented command string.
2. The model's recommendation is not the final policy decision; Harness
   independently evaluates the typed assessment.
3. The verdict repeats a cryptographic basis that is recomputed immediately
   before applying an allow.
4. Read-only tools must pass the same preparation and capability checks as
   ordinary tools, under a reviewer-specific headless policy.

Performance parity is desirable but not a security invariant. Implementations
may use stable request prefixes, provider prompt caching, or reusable immutable
reviewer state. A cache hit, prior review conversation, or previous assessment
must never be required to reach the correct result.

## 6. Repository and dependency boundaries

### 6.1 Harness

Harness owns:

```text
pkg/hustle
    Generic optional evidence-tool policy and descriptor fields.

pkg/gate
    Permission-review subject, basis, assessment, policy, and classifier
    registration contracts.

pkg/event
    Secret-free permission-review lifecycle events.

pkg/rig
    Consumer composition options and identity/fingerprint contribution.

internal/hustleruntime
    Bounded multi-round inference/tool execution and private audit.

internal/loopruntime
    Exact live context capture and permission-gate registration.

internal/sessionruntime
    Review scheduling, gate race, cancellation, final validation, circuit
    breaker, and event publication.
```

### 6.2 Classifiers

The separate module uses no root Go package:

```text
classifiers/
    go.mod
    go.sum
    LICENSE
    README.md
    CONTRIBUTING.md
    docs/
        plans/
        evaluations/
    pkg/
        commandsafety/
        catalog/
    internal/
        prompt/
        wire/
        policy/
        corpus/
        testmodel/
```

`pkg/commandsafety` is the public construction API for the initial classifier.
`pkg/catalog` is an optional convenience catalog and never performs implicit
global registration. Prompts, codecs, policy tables, and fixtures remain
internal.

### 6.3 Consumers

A consumer:

- selects zero or more classifiers;
- supplies each classifier's named inference binding;
- chooses the local risk/authorization policy within Harness's hard ceiling;
- supplies the reviewer evidence-tool catalog and workspace bindings;
- configures lane and circuit-breaker limits; and
- includes classifier descriptors in its durable configuration identity.

Zero registered classifiers preserves current behavior exactly.

## 7. Terminology

- **permission gate** — a `gate.KindPermission` gate opened for one unmet
  prepared `tool.Request`.
- **subject** — the complete immutable value classified by one review.
- **basis** — identifiers and revision digests that bind an assessment to its
  subject and live policy.
- **evidence tool** — a model-facing, read-only tool available only inside an
  eligible Hustle definition.
- **assessment** — the classifier's validated structured result.
- **decision** — Harness's local determination after applying policy to an
  assessment.
- **auto-approval** — a server-originated `ApprovalApprove` submitted through
  the ordinary gate response path.
- **human fallback** — leaving the ordinary permission gate open and unchanged.
- **applicable classifier** — an enabled classifier whose typed applicability
  predicate accepts the prepared request.

## 8. Public permission-review domain

### 8.1 Closed enums

`pkg/gate` adds closed, validated types:

```go
type ReviewRisk string

const (
    ReviewRiskLow      ReviewRisk = "low"
    ReviewRiskMedium   ReviewRisk = "medium"
    ReviewRiskHigh     ReviewRisk = "high"
    ReviewRiskCritical ReviewRisk = "critical"
)

type ReviewAuthorization string

const (
    ReviewAuthorizationUnknown ReviewAuthorization = "unknown"
    ReviewAuthorizationLow     ReviewAuthorization = "low"
    ReviewAuthorizationMedium  ReviewAuthorization = "medium"
    ReviewAuthorizationHigh    ReviewAuthorization = "high"
)

type ReviewRecommendation string

const (
    ReviewAllow      ReviewRecommendation = "allow"
    ReviewNeedsHuman ReviewRecommendation = "needs_human"
)
```

Unknown and zero values are invalid. Parse and validation functions are the
single source of truth for wire and local checks.

Risk categories are also closed values. The initial set includes:

- `data_exfiltration`;
- `credential_access`;
- `credential_probing`;
- `destructive_local`;
- `destructive_shared`;
- `persistent_security_weakening`;
- `production_mutation`;
- `protected_source_control`;
- `untrusted_code_execution`;
- `mutable_network`;
- `prompt_injection`;
- `authorization_conflict`;
- `target_ambiguity`; and
- `insufficient_evidence`.

Categories may be added only with policy, corpus, codec, and drift-guard
updates.

### 8.2 Basis

```go
type ReviewBasis struct {
    GateID             ID
    ToolExecutionID    ID
    SubjectDigest      [32]byte
    ContextRevision    string
    GatePolicyRevision string
    ClassifierRevision string
    SecurityCeiling    string
}
```

Validation requires every field. `SubjectDigest` is SHA-256 over canonical JSON
for:

- wire version;
- gate kind;
- gate ID;
- tool execution ID;
- the complete cloned `tool.Request`;
- context coordinates and revision;
- gate policy revision;
- classifier revision; and
- effective security ceiling.

Canonicalization rejects invalid JSON, duplicate object fields, unknown wire
versions, unsupported enum values, and non-canonical identifiers.
Every private-wire object has an exact canonical key set. Case variants are not
aliases: a canonical key plus `encoding/json`-equivalent case variant is
rejected rather than allowed to shadow the canonical value.
The strict v1 decoder also rejects JSON `null` for every scalar, object, and
array member—including optional strings whose canonical value may be empty—
before Go zero-value conversion can erase the distinction.

Subject construction validates UTF-8 for every string serialized into the
canonical request projection, including optional command and working-directory
fields that `tool.ValidateRequest` does not inspect for a grant-free request.
Canonical hashing never relies on `encoding/json`'s replacement of invalid
UTF-8, so distinct typed inputs cannot collapse to one digest.

Before cloning or constructing a wire projection, subject construction performs
an allocation-free checked preflight over the caller-owned basis, request, and
context. Classifier revision is capped by the same 128-byte registry/event
identity bound, and gate-policy revision by the same 128-byte local-policy
bound, so every constructible subject can correspond to registered/evaluable
state. These basis limits are enforced before any clone or JSON projection.
The request domain is independently bounded to 4,096 requirements, 4,096
candidates in aggregate, 1 MiB in any one request string, and 1 MiB of aggregate
request string text. Inputs outside those bounds fail closed before a second
large slice or string tree can be allocated. Context hard bounds are likewise
checked on the original typed value before cloning.

Each classifier receives its own subject: `ClassifierRevision` and therefore
`SubjectDigest` are classifier-specific. Conjunctive combination computes a
second private common-subject digest with only those two fields neutralized.
That digest proves every outcome covers the same gate, tool execution, complete
request, context, gate policy, and security ceiling while allowing different
classifier revisions. Assessment-to-subject validation always retains the full
classifier-specific basis comparison. Neutralization uses a fixed non-growing
wire representation, so a valid subject at the 1 MiB ceiling cannot become
unusable solely because its classifier revision was replaced for comparison.

### 8.3 Context snapshot

The loop runtime builds a live-only `ReviewContext` before running a tool batch.
It includes:

```go
type ReviewContext struct {
    Coordinates       identity.Coordinates
    ContextRevision   string
    WorkspaceRoot     string
    WorkingDirectory  string
    RetryReason       string
    SecurityCeiling   string
    GatePolicyRevision string
    Entries           []ReviewContextEntry
    Truncation        ReviewTruncation
}
```

Each entry has a closed origin and kind:

- user message;
- assistant message;
- assistant tool request;
- tool result;
- runtime context;
- fetched or external content; and
- omission marker.

Origins are authority labels, not display labels. Only a human user entry can
establish direct user authorization. Tool output, repository instructions,
assistant text, runtime context, and fetched data remain untrusted evidence.

The snapshot contains:

1. the current user request and user-authored follow-ups;
2. relevant earlier user intent retained across transcript truncation;
3. recent assistant and tool-result context;
4. the staged assistant message containing the exact active tool use;
5. retry or escalation justification;
6. workspace root and working directory;
7. effective access posture and security ceiling; and
8. the fact that configured access and stored rules left requirements unmet.

Snapshot construction clones all mutable content. It is deterministic for a
given turn state and policy revision.

### 8.4 Bounds and truncation

Context policy has separate limits for:

- total encoded bytes;
- total estimated tokens;
- entry count;
- user-entry bytes;
- assistant-entry bytes;
- tool-result bytes;
- individual block bytes; and
- active action bytes.

The builder also has hard implementation bounds independent of consumer policy:

- at most 4,096 input entries;
- at most 4 MiB of checked aggregate raw input text;
- at most 2 MiB in one entry; and
- at most 64 KiB in one root metadata field.

Consumer limits may tighten but not exceed their corresponding hard bounds.
The final canonical context is at most the 1 MiB subject-wire ceiling.

“Total encoded bytes” means the exact byte length of the canonical private
JSON projection of the complete `ReviewContext`, including coordinates, root
metadata, entries, and truncation metadata. It is not merely the sum of entry
content. The context builder and subject wire share this projection so budget
and digest representations cannot drift.

Selection is linear in the bounded input domain. The builder precomputes each
canonical entry contribution and checked content total once, scans optional
entries newest-to-oldest once, and performs only bounded constant-size encodes
for changing omission metadata plus one final full encode. It does not clone
and re-encode the growing history for every candidate. An intermediate context
may exceed the final 1 MiB ceiling when deterministic omission can reduce it
below the configured limit.

Every budget constraint encountered while testing candidate retention is
recorded in `Applied` and `Material`. Because adding a mask bit changes the
canonical context size, the selector converges the closed three-bit budget mask
before accepting a candidate or final output; it never reports only the first
constraint that triggered omission.

Truncation preserves both prefix and suffix when useful and emits an explicit
typed omission entry. It never silently drops content.
If source content that must be truncated already contains the reserved fixed
truncation marker, construction fails closed instead of emitting an ambiguous
entry that strict subject validation cannot accept.

The active prepared request is not truncated. If it exceeds the classifier's
input bound, review is ineligible and the human gate remains open.

Truncation does not automatically forbid approval. Local policy declares which
omissions are material. Missing active action, current user intent, security
posture, or required evidence is always material and prevents auto-approval.

The v1 omission marker carries bounded counts but no omitted-kind inventory.
At an untrusted subject-wire boundary, v1 therefore treats every omission as
material; a later revision may safely distinguish non-material omission only
with additional typed, validated provenance. The final active assistant tool
request is never truncatable. Every other truncated entry must carry the fixed
prefix/suffix marker and a material bit whenever its authority kind can contain
current intent or required evidence. Every applied non-budget mask bit must be
explained by an actual truncated entry; the active-action bit cannot appear in
a successfully built v1 context. Omission requires a positive entry count, but
its exact omitted-byte count may be zero when omitted entries were empty.
Strict provenance also requires omitted bytes to be distributable across the
reported omitted entry count under the 2 MiB per-input-entry hard bound.

### 8.5 Subject

```go
type PermissionReviewSubject struct {
    Basis   ReviewBasis
    Request tool.Request
    Context ReviewContext
}
```

The subject is cloned on construction and validation. It carries no grant
tokens, persisted rule authority, controller, inference client, or response
capability.

### 8.6 Assessment

```go
type PermissionAssessment struct {
    Basis          ReviewBasis
    Risk           ReviewRisk
    Authorization  ReviewAuthorization
    Categories     []ReviewRiskCategory
    Recommendation ReviewRecommendation
    Rationale      string
}
```

The output schema is strict:

- object root;
- no additional properties;
- all basis fields required;
- all decision fields required;
- enum values closed;
- categories unique and bounded;
- rationale non-empty for non-low risk and bounded for every result; and
- no reasoning trace field.

The adapter rejects duplicate categories, invalid combinations, oversized
rationale, unknown fields, duplicate JSON keys, nulls, trailing values, or a
basis mismatch.

Rationale is live diagnostic data. It is not copied into enduring events.

## 9. Classifier registration contract

`pkg/gate` defines a focused interface implemented by trusted classifier
packages:

```go
type PermissionClassifier interface {
    Name() hustle.Name
    Revision() string
    Definition() hustle.Definition
    Applies(PermissionReviewSubject) bool
    MarshalInput(PermissionReviewSubject) (json.RawMessage, error)
    ValidateResult(PermissionReviewSubject, hustle.Result) (PermissionAssessment, error)
}
```

The exact exported shape may be split into smaller interfaces during
implementation to keep capability surfaces narrow. The semantic contract is
fixed:

- `Definition` is immutable and must be blocking, named-model, structured
  output, and evidence-tool compatible.
- `Applies` is deterministic, side-effect free, and may inspect only the typed
  subject.
- `MarshalInput` emits one versioned JSON value and may not weaken the subject.
- `ValidateResult` performs strict decode and domain validation.
- none of the methods receives a gate responder, controller, grant issuer,
  rule writer, workspace mutator, or generic Hustle runner.

Construction rejects:

- duplicate names or revisions;
- duplicate applicability slots where consumer policy disallows conjunction;
- non-blocking definitions;
- current-loop model sources;
- missing structured output;
- missing tool-policy revisions;
- invalid limits;
- nil or typed-nil implementations; and
- definitions whose descriptors do not match the classifier metadata.

Permission-classifier names are additionally valid UTF-8, trimmed canonical
text without NUL, and at most 128 bytes. Registration reads name, revision, and
definition once, validates them, and stores an immutable wrapper containing
those frozen metadata values. Registry views expose the wrapper, never the
mutable implementation's metadata methods. Applicability/input/result behavior
delegates to the trusted implementation, but later metadata drift cannot
change the registered name, revision, or definition.

## 10. Local decision policy

The model recommendation is necessary but insufficient. Harness evaluates:

```go
type PermissionReviewPolicy struct {
    MaximumAutoRisk      ReviewRisk
    MinimumAuthorization map[ReviewRisk]ReviewAuthorization
    AbsoluteHuman        []ReviewRiskCategory
    MaterialTruncation   ReviewTruncationMask
}
```

Construction seals the complete canonical policy projection in unexported
state. Evaluation validates both the public fields and that seal, so any
post-construction mutation—including a different policy that would be valid if
newly constructed—fails closed under the original revision.

The hard Harness ceiling is:

- critical risk never auto-approves;
- an absolute-human category never auto-approves;
- missing or conflicting authorization never increases authority;
- an invalid or unknown policy fails construction;
- a consumer may tighten but not relax hard ceilings; and
- policy revisions participate in rig and subject identity.

The initial command-safety default is compatible with Codex Guardian:

| Risk | Minimum authorization | Default result |
| --- | --- | --- |
| low | unknown | eligible |
| medium | unknown | eligible unless a category tightens it |
| high | medium | eligible only when narrowly scoped and no absolute-human category applies |
| critical | any | human |

Consumers may choose a stricter default, such as low-only.

Harness recomputes the policy decision from the typed assessment. A model
`allow` that is inconsistent with the local matrix becomes `needs_human`.

## 11. Multiple classifiers

Consumers register an ordered set, but applicable classifiers may execute
concurrently subject to the blocking Hustle lane.

Harness constructs one classifier-specific `PermissionReviewSubject` per
registered classifier. An outcome carries that exact subject. Combination
validates each assessment against its own full basis, rejects duplicate
classifier revisions, and compares a private common-subject digest that
neutralizes only classifier revision and subject digest. Any difference in
gate, tool execution, prepared request, context, gate policy, or security
ceiling leaves the gate human.

Combination also receives the immutable registered classifier set. It requires
exactly one ordered outcome for every frozen classifier revision; missing,
extra, reordered, duplicate, or invented revisions leave the gate human.
This prevents a caller from manufacturing eligibility by omitting an enabled
classifier that failed or required human review.

Decision combination is conjunctive:

1. At least one enabled classifier must apply.
2. Every applicable classifier must produce a locally eligible allow.
3. A non-applicable classifier contributes nothing.
4. If no classifier applies, the gate remains human.
5. Any `needs_human`, error, timeout, cancellation, stale result, or policy
   mismatch leaves the gate human.
6. A late result cannot reverse a completed decision.

An allow result cannot widen the policy or evidence toolset used by another
classifier.

Frozen registry wrappers pass a deep clone of the subject to `Applies`,
`MarshalInput`, and `ValidateResult`. A buggy trusted classifier therefore
cannot mutate or race the caller's canonical subject through slice aliases.

## 12. Tool-using Hustle extension

### 12.1 Compatibility

Tool support is opt-in. A definition without evidence tools continues to issue
exactly one `inference.Client.Invoke`, validates the result, publishes one
terminal Hustle event, and finalizes exactly as today.

### 12.2 Definition policy

`pkg/hustle` adds:

```go
type ToolLoopLimits struct {
    MaxRounds        int
    MaxCalls         int
    MaxCallsPerRound int
    MaxResultBytes   int
    MaxEvidenceBytes int
}
```

and an option conceptually equivalent to:

```go
WithEvidenceTools(revision string, limits ToolLoopLimits, definitions ...tool.Definition)
```

The exact API may use a named policy value rather than positional arguments.
It must remain self-documenting.

The definition descriptor adds:

- evidence-tool policy revision;
- ordered produced tool names;
- tool definition metadata digest;
- tool-loop limits; and
- a marker that structured output with tools is required.

Evidence catalogs have hard construction bounds independent of loop execution:
at most 64 tool definitions, at most 128 produced concrete tools, at most 64
bytes per concrete tool name, at most 4 KiB per description, at most 1 MiB per
schema, and at most 4 MiB across all concrete names, descriptions, and compact
schemas. Evidence-policy revisions are canonical trimmed UTF-8 without NUL.

The descriptor never contains raw schemas or descriptions. Bound construction
freezes model-facing `ToolInfo` metadata declared by each evidence definition
and includes versioned, domain-separated catalog digests in descriptor and rig
identity. Tool argument schemas must satisfy the same bounded portable JSON
Schema subset used by the inference layer, including duplicate-key, keyword,
depth, property-count, and root-object validation.

Concrete tools are built for each review invocation using its originating
session and loop identity, never the session's construction-time active loop.
Every build validates the concrete `ToolInfo` against the frozen static
metadata before inference. Static metadata drift changes topology/restore
identity; factory drift under unchanged metadata fails that review closed.

Evidence definitions use a dedicated read-only workspace requirement. Their
distinct `EvidenceFactory` receives `EvidenceFactoryBindings`, whose exact
public fields are the invocation `SessionID`, invocation `LoopID`, and a
`ReadWorkspaceBinding` containing only the canonical root needed for read
evidence. Neither factory nor binding is the generic `tool.Factory` /
`tool.Bindings` seam: the evidence API has no mutation coordinator,
observations, delegate controller, extra tools, session controller, gate, rule,
grant, or loop-control capability. Generic mutation-capable
`RequiresWorkspace` definitions are invalid in evidence policies.

### 12.3 Model capability

A tool-using structured Hustle requires:

- `Model.Caps.Tools`;
- `Model.Caps.StructuredOutput`;
- `Model.Caps.StructuredOutputWithTools`; and
- codec/provider support for ordinary tool results plus terminal structured
  output.

Capability mismatch fails the review before inference and leaves the human gate
open.

### 12.4 Execution loop

The runtime:

1. builds a request from the immutable system prompt and versioned input;
2. exposes only the bound evidence tools and terminal output schema;
3. invokes the model;
4. accepts either:
   - one strict terminal structured result; or
   - one or more ordinary evidence-tool calls;
5. prepares each evidence call once;
6. applies reviewer access policy;
7. executes allowed evidence calls sequentially;
8. appends the assistant tool-use message and paired tool results to the
   private invocation transcript;
9. repeats within round, call, byte, and deadline bounds; and
10. validates and finalizes the terminal result.

Mixed terminal output and ordinary evidence calls are ambiguous and fail.
Text-only output is invalid for a structured classifier. Duplicate tool-use
IDs, unknown tools, malformed arguments, unpaired results, or a provider
finish-reason contradiction fail.

Sequential execution is deliberate. Evidence gathering is low volume, and
deterministic order simplifies byte accounting, cancellation, audit, and
reproduction.

Every complete provider response is preflighted read-only before any response
block or argument is cloned, parsed, prepared, or executed. The definition's
`OutputBytes` limit bounds terminal text or terminal arguments exactly and also
bounds both each ordinary evidence-call argument and their aggregate in one
round. `MaxCallsPerRound` is enforced during the first shallow block scan, so a
call-count violation wins before argument parsing.

Structural denial-of-service limits are fixed runtime contract values: at most
4,096 response blocks, 1 MiB aggregate thinking text and thinking signatures,
1,024 bytes per call ID, 64 bytes per tool name, and 20 MiB across all
provider-controlled block strings and argument bytes. Checked arithmetic is
used throughout. Evidence argument JSON is a strict object with at most 64
container levels, 65,536 object members across the document, and 262,144
decoder tokens. Validation is iterative, rejects duplicate keys at every
object level, and accepts neither a non-object root nor a trailing value.
Limit failures use closed reasons and never retain provider content.

### 12.5 Usage

Usage from every inference round is normalized and accumulated with checked
arithmetic. The terminal Hustle event carries the aggregate. Evidence tools do
not add usage. Partial usage from a failed later round is retained on the
failure event when available.

### 12.6 Retry

The classifier adapter may request at most one retry within the original
deadline for:

- a classified transient inference failure; or
- a recoverable malformed terminal output.

For adapter-owned terminal decoding and strict wire-shape validation,
`pkg/hustle` exposes `NewRecoverableTerminalValidationError`. Its concrete
type is private, it accepts no cause, and its error text is fixed and bounded.
The paired exact-type predicate neither follows wrappers nor matches by
substring, so arbitrary errors cannot emulate the marker through error text or
a custom `As` method. The runtime recognizes this marker only when
`ValidateResult` fails at `StageOutput` with `ReasonInvalidOutput` under
`RetryPolicyClassifiedOnce`. It normalizes the marker back to a fresh fixed
package-owned value before retaining the failure, so provider output and
adapter error text cannot escape through the exhausted second attempt.

The marker is exclusively a syntax/shape signal: duplicate, unknown, missing,
or otherwise invalid terminal wire fields may use it. A basis mismatch,
`needs_human`, deny/unsafe semantic result, arbitrary validator error, callback
panic, or any other domain or operational failure must not use it. A marker at
another stage or reason does not make that failure retryable. The zero retry
policy remains single-attempt even when a validator returns the marker.

It does not retry:

- context cancellation;
- deadline expiry;
- lane rejection;
- evidence-policy violation;
- an unknown or mutating tool;
- basis mismatch;
- domain `needs_human`;
- finalizer failure; or
- session shutdown.

Retry restarts from the immutable subject. It does not preserve partially
trusted evidence from the failed attempt. Both attempts share the original
deadline. Session/controller shutdown and finalizer failure cannot enter the
retry path. A second recoverable malformed result terminates with the fixed
bounded classification; there is no retry loop.

## 13. Evidence-tool security

### 13.1 Reviewer access policy

Evidence calls run through a dedicated headless evaluator. They never use
`loop.GateApprover`.

Every tool must implement `tool.CallPreparer`. The prepared request is accepted
only when:

- every requirement kind is on the classifier's read-only allowlist;
- configured access returns `AccessAllow`;
- no requirement is `AccessGated` or `AccessDeny`;
- no stored rule lookup or persistence is needed;
- no grant class or grant target is present;
- no reusable rule candidate is present;
- execution identity matches the evidence call;
- the request is within the review workspace/security ceiling; and
- the definition was selected by trusted consumer composition.

Unknown access state or any dependency error fails closed.

The runtime does not infer containment from the generic, tool-owned
`Requirement.Scope` or `Requirement.Match` strings. Consumer composition must
provide a narrow `EvidenceContainmentVerifier` together with the canonical read
root and effective security ceiling. The verifier receives only those two
policy values and a defensive clone of the normalized prepared request. It
must resolve every target (including symlinks), reject ambiguous requirement
semantics and root escape, and enforce the ceiling independently before
configured access is consulted. Nil/typed-nil verifiers, an invalid policy,
verifier error, or verifier panic fail closed; no session, gate, rule, grant,
mutation, delegation, or loop-control capability crosses this seam.

The runtime deep-clones the request immediately after `PrepareCall` returns.
Containment, access evaluation, and `PreparedCall` execution each receive
independently owned views, so a preparer or verifier retaining a slice cannot
rewrite another collaborator's authorization input. The opaque
`PreparedArtifact` remains the preparer's authoritative per-call value.

Before that first clone, the runtime performs an allocation-light structural
preflight over the borrowed request: at most 128 requirements, no reusable
candidates, at most 64 KiB in any request string, and at most 1 MiB across all
request strings. Before encoding or cloning a tool result, it accepts only
non-nil text blocks, at most 4,096 blocks, at most 1 MiB per text block, and at
most 20 MiB of text in aggregate. Checked arithmetic is used throughout. The
raw result content must also fit both the independent per-result limit and the
true remaining aggregate evidence budget before encoding; the encoded form is
then checked against both limits exactly. Unknown block types, typed nils, and
any one-over structure fail closed with a bounded error that contains no
request or result content.

Invocation-scoped evidence catalog binding and evidence execution both run in
controller-owned, buffered workers under the original single deadline. A
worker panic is redacted and fault-reported. A collaborator that ignores
cancellation beyond `WorkerDrainTimeout` poisons both admission lanes and no
new evidence worker is admitted.

All evidence-attempt callbacks run under a value-empty context rooted outside
the caller/session value tree. It preserves the original cancellation cause
and exact deadline through a bounded `context.AfterFunc` bridge that is stopped
when the attempt ends. Only the exact `InvokableRun` subcall receives a fresh
`PreparedCall` value; model resolution, inference, catalog factories,
preparation, containment, validation, and other callbacks cannot observe
ambient loop, gate, requester, provenance, or prior prepared-call values.

### 13.2 Initial evidence capabilities

The complete initial command-safety evidence pack includes:

- canonical path resolution;
- `lstat`-style metadata that does not follow the final symlink;
- resolved target metadata;
- bounded directory listing;
- bounded file reading;
- bounded glob;
- bounded grep;
- Git repository root and worktree state;
- Git status and diff metadata;
- configured Git remotes;
- local branch/upstream/default-branch evidence when available; and
- repository visibility evidence through an explicitly configured, read-only
  network identity source.

The pack reuses existing guarded ReadFile, Glob, and Grep behavior where their
contracts fit. It adds focused metadata and Git tools rather than exposing Bash.

### 13.3 Forbidden capabilities

Evidence tools may not:

- write, rename, delete, chmod, chown, or create files;
- execute arbitrary shell or user-provided programs;
- mutate Git state or contact a remote with write semantics;
- access denied secret paths;
- request a permission gate;
- persist a rule;
- mint or consume an execution grant;
- delegate work;
- alter a loop, mode, model, session, or security ceiling;
- open an interactive login or URL;
- access arbitrary MCP tools; or
- return an unbounded result.

### 13.4 TOCTOU

Classification is not enforcement. Tool preparation and the sandbox remain
responsible for execution-time safety.

Target-sensitive evidence records an observation token derived from canonical
identity and stable metadata. Immediately before auto-approval, the adapter
rechecks observations required by policy. A mismatch or unverifiable target
leaves the human gate open.

The eventual tool still consumes its original prepared artifact. Existing
symlink-swap, containment, grant-target, and sandbox checks remain mandatory.

## 14. Gate lifecycle

### 14.1 Context capture

At the tool-batch boundary, `internal/loopruntime` has:

- committed base history;
- staged messages for the active turn;
- the assistant message containing current tool calls;
- loop, turn, and step coordinates;
- model-facing runtime context; and
- the bound policy revision.

It builds one immutable review-context snapshot and installs it in the private
batch context. Every permission approval registration receives the same
snapshot plus its own exact prepared request and tool execution ID.

The snapshot is not exposed through public tool context APIs.

### 14.2 Open human gate first

The ordinary gate lifecycle remains:

1. append private `GatePreparedRecord`;
2. install the loop-local blocker;
3. append and announce `GateOpened`; and
4. acknowledge successful installation to the waiting runner.

Automatic review begins only after `GateOpened` commits. Therefore:

- the human can answer immediately;
- there is no invisible review-only wait;
- the same gate ID appears in classifier basis and human UI;
- activation failure cannot produce an auto-approval; and
- all answers race the existing exactly-once session claim.

### 14.3 Start review

After activation, the loop/session hands a live-only review request to a
focused session runtime adapter. The actor does not block on inference.

The adapter:

1. evaluates classifier applicability;
2. constructs a separate subject and basis for each applicable classifier;
3. publishes secret-free start events;
4. schedules classifier Hustles;
5. validates every result;
6. combines decisions;
7. re-reads live gate and security state; and
8. attempts a classifier-originated response only if all checks pass.

### 14.4 Race

Human and classifier use the same gate directory claim:

```text
human response first
    -> GateResolved commits
    -> classifier contexts cancel
    -> late classifier response is stale

classifier approval first
    -> GateResolved commits with classifier source
    -> ApproveToolCall routes to the parked evaluator
    -> human response is stale

no eligible classifier approval
    -> no GateResponse is submitted
    -> human gate remains open
```

A stale classifier response is an expected race, not a session fault.

### 14.5 Applying approval

Only a private session-runtime method may create classifier response
provenance. Public callers cannot select it.

The submitted response is semantically:

```go
gate.GateResponse{
    GateID: gateID,
    Action: string(gate.ApprovalApprove),
    Source: gate.ResponseSource{
        Kind: gate.ResponseFromClassifier,
        Reason: classifierNameAndRevision,
    },
}
```

The session validates the action against the permission gate, claims the gate,
appends `GateResolved`, and routes the ordinary `command.ApproveToolCall`.
`gate.Evaluator.Resolve` then mints exact execution-bound grants. The
classifier never receives them.

`ApprovalApproveAlwaysWorkspace`, `ApprovalDeny`, and arbitrary values are
structurally unavailable on this path.

## 15. Cancellation, shutdown, and restore

The session retains one cancellation group per active gate review. It cancels
when:

- the gate resolves;
- the owner closes the gate;
- the loop or turn is interrupted;
- the session shuts down;
- the review deadline expires; or
- a conjunction member makes auto-approval impossible and remaining results
  are not needed for configured audit.

Cancellation releases lane ownership only after bounded terminal audit and
finalization, following existing Hustle semantics.

Hustles are not restored. If the process exits during review:

- unmatched Hustle starts repair as interrupted;
- the permission gate restores normally;
- no classifier response is synthesized;
- the gate remains answerable by a human; and
- review is not rerun from guessed context.

An exact re-review after restore requires a future design proving equivalent
transcript, policy, active tool continuation, and security state.

## 16. Events, audit, and privacy

### 16.1 Private Hustle lifecycle

Existing `HustleStarted`, `HustleCompleted`, and `HustleFailed` retain:

- classifier definition descriptor;
- run ID;
- model runtime;
- aggregate usage;
- stage and bounded reason; and
- timing.

They never contain input, evidence, output, prompt, command, or rationale.

### 16.2 Permission-review events

Add enduring, secret-free events:

```go
type PermissionReviewStarted struct {
    Header
    GateID             gate.ID
    ToolExecutionID    uuid.UUID
    Classifier         hustle.Name
    ClassifierRevision string
}

type PermissionReviewCompleted struct {
    Header
    GateID             gate.ID
    ToolExecutionID    uuid.UUID
    Classifier         hustle.Name
    ClassifierRevision string
    Status             gate.ReviewStatus
    Risk               gate.ReviewRisk
    Authorization      gate.ReviewAuthorization
    Categories         []gate.ReviewRiskCategory
    AutoApproved       bool
}
```

Statuses are closed:

- `allowed`;
- `needs_human`;
- `not_applicable`;
- `timed_out`;
- `failed`;
- `cancelled`; and
- `stale`.

An `allowed` completion can never carry `critical` risk; durable validation
rejects that globally impossible audit state even if a producer is buggy.

Events exclude:

- conversation text;
- commands and raw arguments;
- file names when policy treats them as sensitive;
- file contents and evidence output;
- prompt text;
- model output;
- rationale or reasoning;
- credentials;
- rule data; and
- grant material.

An ephemeral diagnostic may carry a bounded, sanitized rationale to an
authorized UI. It is never journaled or replayed.

### 16.3 Provenance

Add `gate.ResponseFromClassifier`. The session, not a request caller, stamps
this source. `GateResolved` and permission audit distinguish human, timeout
policy, and classifier approval without claiming a human acted.

## 17. Error taxonomy

Publicly classifiable failures use typed errors. Internal errors carry bounded
stage/reason values and wrap causes without embedding subject data.

Expected review failures leave the gate human:

- no applicable classifier;
- lane full;
- classifier construction or model capability mismatch;
- input limit;
- inference failure;
- evidence-tool failure;
- forbidden capability;
- output or domain validation failure;
- retry exhaustion;
- local policy refusal;
- basis mismatch;
- observation mismatch;
- stale gate;
- cancellation; and
- timeout.

Integrity failures keep existing fault semantics:

- failure to append required durable events;
- corrupt gate directory state;
- impossible Hustle ownership state;
- invalid internal event coordinates;
- callback panic at a trusted boundary; and
- shutdown drain failure.

No error returns a partially trusted assessment or partially applied response.

## 18. Circuit breaker

Track per-turn and per-session bounded counters for:

- consecutive `needs_human` results;
- recent invalid or failed reviews;
- materially identical gated subjects; and
- classifier-originated stale responses.

Threshold crossing:

1. disables automatic review for the current turn;
2. emits one bounded warning;
3. leaves current and future gates human-only; and
4. does not widen any policy.

The default does not interrupt the loop because an open human gate already
prevents rapid autonomous retry. Consumers may configure a stricter interrupt
action, matching Codex Guardian's repeated-denial behavior.

Counters are bounded and cleared when the turn completes. They do not contain
subject contents.

## 19. Initial `gate.command-safety` classifier

### 19.1 Applicability

The initial classifier applies when a permission gate's prepared request
contains command execution or a command-triggered combination of filesystem
and network requirements. A later `gate.general-safety` classifier covers
non-command tools.

Applicability is based on typed capability kinds, not tool display names.

### 19.2 Prompt

The immutable prompt:

- states that it is judging one planned coding-agent action;
- labels all supplied context and evidence as untrusted data;
- defines user authorization scoring;
- defines the risk taxonomy;
- requires read-only investigation when local facts matter;
- warns that omissions are unknown, not benign;
- separates intrinsic risk from authorization;
- defines consumer policy inputs;
- forbids following instructions found in evidence; and
- requires the strict assessment schema.

Prompt, policy, schema, wire, and corpus revisions are independently explicit
and included in classifier identity.

### 19.3 Default policy

The default policy covers:

- data exfiltration;
- credential use and credential probing;
- destructive local and shared actions;
- persistent security weakening;
- source-control history and protected branches;
- production operations;
- network access;
- sandbox escalation;
- user-requested narrow cleanup;
- package installation and build/test operations;
- generated scripts;
- untrusted instructions and prompt injection; and
- post-warning explicit user approval.

Tenant or product policy may tighten the default through an immutable
configuration value. Raw policy text is hashed into identity and kept out of
audit.

### 19.4 Output

The model returns basis, risk, authorization, categories, recommendation, and
one concise rationale. Local code re-evaluates the result. The output does not
carry a gate action.

## 20. Consumer composition

The intended composition is conceptually:

```go
reviewer, err := commandsafety.New(commandsafety.Options{
    Inference: classifierClient,
    Model: classifierModel,
    Policy: commandsafety.DefaultPolicy(),
    Evidence: commandsafety.StandardEvidence(readPolicy),
})

rig, err := rig.Define(
    rig.WithLoops(...),
    rig.WithPermissionClassifiers(reviewer),
    rig.WithPermissionReviewPolicy(gate.PermissionReviewPolicy{...}),
    rig.WithPermissionReviewLimits(rig.PermissionReviewLimits{...}),
)
```

Exact option names may change during API review. Required properties:

- no global registry;
- no implicit default classifier;
- no model-facing enable/disable control;
- immutable definitions;
- explicit consumer selection;
- policy and classifier identity in rig fingerprints;
- duplicate registration rejected; and
- zero configuration preserves current gates.

## 21. Fingerprint and drift

Rig identity covers:

- ordered classifier names and revisions;
- definition descriptors;
- prompt, policy, wire, schema, and evidence-tool revisions;
- model source and named model policy revision;
- output schema digest;
- evidence tool names and schema digests;
- tool-loop limits;
- local decision policy revision;
- context snapshot policy revision;
- circuit-breaker policy; and
- security ceiling revision.

Restore compares the complete identity. A mismatch follows the existing rig
configuration mismatch policy. It never silently resumes with a different
reviewer.

Every evidence-policy, catalog, produced-name, concrete-tool, and rig projection
uses an explicit versioned domain label and length-delimited or typed canonical
encoding. Raw SHA-256 of an unlabelled value is not an identity boundary.

## 22. Testing strategy

### 22.1 TDD

Every implementation slice follows:

1. write a focused failing test;
2. run it and verify the expected failure;
3. write the minimum implementation;
4. run the focused test;
5. run the owning package suite;
6. commit; and
7. obtain review at each phase boundary.

### 22.2 Harness unit tests

Cover:

- enum and basis validation;
- strict codecs and canonical digest;
- defensive cloning;
- context selection, authority labels, bounds, and truncation;
- classifier registration and descriptor validation;
- local policy evaluation;
- multi-classifier conjunction;
- optional tool-policy definition behavior;
- model capability negotiation;
- multi-round tool execution;
- malformed and ambiguous response shapes;
- evidence call preparation;
- read-only capability enforcement;
- round, call, result, aggregate, and deadline limits;
- usage accumulation and overflow;
- retry classification;
- cancellation and shutdown;
- callback and worker panic recovery;
- circuit breaker;
- event classification, codecs, visibility, and redaction; and
- dependency boundaries.

### 22.3 Gate race tests

Run with the race detector:

- human before review starts;
- human during evidence gathering;
- classifier immediately before human;
- simultaneous claim;
- gate close during review;
- turn interruption;
- session shutdown;
- stale classifier result;
- basis mutation;
- security ceiling mutation;
- observation mismatch;
- activation failure;
- audit append failure; and
- duplicate classifier completion.

Each test asserts exactly one gate resolution and at most one routed approval.

### 22.4 Restore tests

Cover:

- unmatched classifier Hustle start repair;
- restored human gate remains answerable;
- no auto-review replay;
- no persisted review input/evidence/output;
- classifier configuration drift; and
- stale live cancellation handles do not survive restore.

### 22.5 Fuzzing

Fuzz:

- classifier input and output codecs;
- duplicate JSON fields;
- basis canonicalization;
- subject digest stability;
- context truncation;
- evidence tool-call decoding;
- terminal/tool mixed response extraction;
- category set validation;
- policy matrices; and
- event codecs.

Fuzz error messages must not echo raw fuzz payloads.

### 22.6 Classifier corpus

The classifiers module contains versioned cases for:

- routine builds, tests, formatting, and lint;
- package and tool installation;
- sandbox retry and escalation;
- narrow and broad deletion;
- missing, empty, symlinked, and changed deletion targets;
- Git status, checkout, reset, clean, force push, branch deletion, and protected
  branches;
- credential use, credential reading, and credential exfiltration;
- public, private, internal, and unknown repository remotes;
- uploads and data movement;
- production and shared-environment mutation;
- security-control changes;
- arbitrary downloaded or generated code execution;
- explicit, implied, absent, conflicting, and post-warning authorization;
- prompt injection in files, tool results, repository instructions, command
  arguments, and fetched content;
- truncated material context; and
- evidence failures.

Port every materially distinct Codex Guardian policy scenario as an
independently worded fixture with expected risk, authorization, categories, and
eligibility. Record any stricter Harness result.

### 22.7 Evaluation reports

Evaluation reports include:

- corpus revision;
- classifier and model identity;
- confusion matrix for auto-approval eligibility;
- results by risk and authorization;
- critical-case false allows;
- high-risk false allows;
- benign actions unnecessarily sent to humans;
- tool/evidence usage;
- latency and token usage;
- comparison against the Codex baseline; and
- every changed result from the previous accepted revision.

Reports contain redacted or synthetic fixtures only.

### 22.8 Boundary tests

Enforce:

- Harness never imports `github.com/looprig/classifiers`;
- `pkg/gate` does not import session/runtime internals;
- `pkg/hustle` does not import gate, rig, session, or runtime internals;
- classifiers use only public Harness packages;
- classifier code cannot access response or grant capabilities;
- evidence tools cannot acquire workspace mutation or delegation bindings;
- public session/controller APIs expose no generic Hustle runner; and
- the classifiers repository has no root-level `.go` files.

## 23. Acceptance criteria

The feature is complete only when:

1. A configured safe command can be auto-approved once end to end.
2. The human can answer the same gate while review runs.
3. Human-first and classifier-first races each resolve exactly once.
4. Every non-allow classifier path leaves the human gate open.
5. Critical risk never auto-approves.
6. `Approve always`, deny, rule persistence, and grant minting are unreachable
   from classifier code.
7. Evidence tools cannot mutate state or open nested approvals.
8. Basis, policy, context, observation, or security drift prevents approval.
9. Review data does not appear in durable public events or restored gates.
10. Interrupted reviews restore as human-only gates.
11. Multiple classifiers combine conjunctively.
12. The command-safety corpus has no critical-case false allow.
13. Every Codex Guardian scenario is matched or handled more strictly.
14. Harness, classifiers, and cross-module suites pass with `-race`.
15. Static analysis, vulnerability checks, formatting, vendoring, and boundary
    checks pass in every modified module.

## 24. Delivery phases

Implementation is staged:

1. public review domain and identity;
2. optional tool-using Hustle definitions;
3. multi-round Hustle runtime;
4. reviewer evidence enforcement;
5. events and gate review lifecycle;
6. context capture, race, cancellation, restore, and circuit breaker;
7. new classifiers module and command-safety implementation;
8. read-only evidence pack;
9. consumer integration;
10. cross-module evaluation, hardening, documentation, and release.

Each phase:

- is TDD-driven;
- lands in reviewable commits;
- receives an independent specification-compliance review;
- receives a code-quality/security review;
- fixes all findings before the next phase; and
- reruns the phase acceptance suite.

## 25. Security invariants

The following statements are non-negotiable:

1. A classifier result is evidence, never authority.
2. Only trusted Harness code can translate eligible evidence into an ordinary
   one-shot approval response.
3. A classifier can never persist or mint authority.
4. Every ambiguous, invalid, missing, stale, cancelled, or unknown condition
   preserves the human gate.
5. Human response and classifier approval share one exactly-once claim.
6. The prepared request and enforcement path remain authoritative.
7. Read-only investigation is capability constrained, not prompt constrained.
8. Untrusted context cannot redefine classifier or tenant policy.
9. Durable audit never stores classifier inputs, evidence, output, reasoning,
   prompts, credentials, or grants.
10. No cache, prior review, provider behavior, or model recommendation is
    required for a correct local decision.
