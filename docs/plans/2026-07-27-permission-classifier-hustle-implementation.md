# Permission Auto-Review Classifier and Tool-Using Hustles Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** Add consumer-selectable, Codex-Guardian-parity permission auto-review using basis-bound command-safety classifiers, capability-constrained read-only evidence tools, and human/classifier gate races.

**Architecture:** Harness gains the neutral review domain, an optional bounded tool loop for structured Hustles, and the session-owned gate integration. A new `github.com/looprig/classifiers` module supplies the command-safety prompt, policy, codecs, evidence pack, and evaluation corpus; CodeRig explicitly composes it. Classifier output is evidence only: Harness revalidates the exact live basis and can submit only `ApprovalApprove`, while every other outcome leaves the ordinary human gate open.

**Tech Stack:** Go 1.26, existing `github.com/looprig/{core,harness,inference,tools}` modules, standard library JSON/crypto/filesystem/process packages, existing vendored dependencies, Go race detector, fuzz tests, and fake provider-neutral inference clients.

---

## Execution protocol

This plan is executed in the current session with
`superpowers:subagent-driven-development`.

For every numbered task:

1. Dispatch a fresh implementation subagent with the complete task text.
2. Require `superpowers:test-driven-development`.
3. The subagent writes one focused failing test and records the expected RED
   failure before production code.
4. The subagent implements the minimum GREEN behavior, refactors only while
   green, runs the owning package suite, self-reviews, and commits.
5. Record the commit and continue to the next task in the same phase without an
   independent task-level review.

At each phase boundary:

1. Dispatch one fresh specification-compliance reviewer for the entire phase
   diff against:
   - `docs/plans/2026-07-27-permission-classifier-hustle-design.md`;
   - every task requirement in the phase;
   - the phase acceptance list below; and
   - the Codex Guardian parity table in design §5.
2. Send every finding to an implementation subagent and repeat spec review
   until approved.
3. Dispatch one fresh code-quality/security reviewer using
   `superpowers:requesting-code-review` for the entire phase diff.
4. Fix every Critical and Important finding and re-review until approved.

Do not start the next phase with open findings.

Use these Harness test settings unless a task says otherwise:

```bash
GOCACHE=/private/tmp/looprig-harness-go-cache GOFLAGS=-mod=vendor go test -race ./path/to/package
```

The new classifiers module uses:

```bash
GOCACHE=/private/tmp/looprig-classifiers-go-cache GOFLAGS=-mod=vendor go test -race ./...
```

Never update module dependencies accidentally. Do not run bare `go mod
download` in a module with local sibling replacements. Refresh a vendored tree
only in the explicit vendoring task.

## Workspace and branch map

| Module | Working directory | Branch |
| --- | --- | --- |
| Harness | `/Users/ipotter/code/looprig/harness-permission-classifier` | `feat/permission-classifier` |
| Classifiers | `/Users/ipotter/code/looprig/classifiers` | new repository, `feat/command-safety` |
| CodeRig | adjacent isolated worktree created before Phase 6 | `feat/permission-classifier` |
| Integration tests | adjacent isolated worktree created before Phase 7 | `feat/permission-classifier` |

The Harness design commit is `ac99e57`.

---

# Phase 1 — Neutral review domain and audit

## Task 1: Add closed review enums, status, categories, and validation

**Files:**

- Create: `pkg/gate/review.go`
- Create: `pkg/gate/review_test.go`
- Create: `pkg/gate/deps_test.go`

**Step 1: Write the failing enum table test**

Add table-driven tests proving that:

- risk accepts exactly `low`, `medium`, `high`, and `critical`;
- authorization accepts exactly `unknown`, `low`, `medium`, and `high`;
- recommendation accepts exactly `allow` and `needs_human`;
- status accepts the design §16.2 set;
- every initial risk category is recognized;
- blank and unknown values fail; and
- category validation rejects duplicates and an over-limit slice.

Use complete-value comparisons. Do not test constants by merely comparing them
to their literal declarations; drive the public parse/validate behavior.

**Step 2: Run RED**

```bash
GOCACHE=/private/tmp/looprig-harness-go-cache GOFLAGS=-mod=vendor \
  go test -race ./pkg/gate -run 'TestReview(Enum|Categories)'
```

Expected: compile failure because review types and validators do not exist.

**Step 3: Implement the minimum closed domain**

Implement:

```go
type ReviewRisk string
type ReviewAuthorization string
type ReviewRecommendation string
type ReviewStatus string
type ReviewRiskCategory string

func ParseReviewRisk(string) (ReviewRisk, bool)
func ParseReviewAuthorization(string) (ReviewAuthorization, bool)
func ParseReviewRecommendation(string) (ReviewRecommendation, bool)
func ParseReviewStatus(string) (ReviewStatus, bool)
func ParseReviewRiskCategory(string) (ReviewRiskCategory, bool)
func ValidateReviewCategories([]ReviewRiskCategory) error
```

Use a bounded typed error with field and reason. Error strings must not include
untrusted raw values.

**Step 4: Run GREEN and package regression**

Run the focused command, then:

```bash
GOCACHE=/private/tmp/looprig-harness-go-cache GOFLAGS=-mod=vendor \
  go test -race ./pkg/gate
```

Expected: PASS.

**Step 5: Run dependency boundary test**

Ensure `pkg/gate` still has no forbidden runtime/session imports.

**Step 6: Commit**

```bash
git add pkg/gate/review.go pkg/gate/review_test.go pkg/gate/deps_test.go
git commit -m "feat(gate): add permission review taxonomy"
```

## Task 2: Add authority-labeled review context and deterministic truncation

**Files:**

- Create: `pkg/gate/review_context.go`
- Create: `pkg/gate/review_context_test.go`
- Create: `pkg/gate/review_context_fuzz_test.go`

**Step 1: Write failing construction tests**

Specify:

```go
type ReviewContextOrigin string
type ReviewContextKind string

type ReviewContextEntry struct {
    Origin    ReviewContextOrigin
    Kind      ReviewContextKind
    Content   string
    Truncated bool
}

type ReviewContext struct {
    Coordinates        identity.Coordinates
    ContextRevision    string
    WorkspaceRoot      string
    WorkingDirectory   string
    RetryReason        string
    SecurityCeiling    string
    GatePolicyRevision string
    Entries            []ReviewContextEntry
    Truncation         ReviewTruncation
}

type ReviewContextPolicy struct {
    Revision             string
    MaxBytes             int
    MaxEstimatedTokens   int
    MaxEntries           int
    MaxUserEntryBytes    int
    MaxAgentEntryBytes   int
    MaxToolEntryBytes    int
    MaxBlockBytes        int
    MaxActiveActionBytes int
}

type ReviewTruncationMask uint16

type ReviewTruncation struct {
    Applied        ReviewTruncationMask
    Material       ReviewTruncationMask
    OmittedEntries int
    OmittedBytes   int
}
```

Tests must cover user/assistant/tool/runtime/external/omission origins,
defensive cloning, UTF-8-safe prefix/suffix truncation, stable omission
markers, retention of current user intent, retention of the active assistant
tool request, exact limits, one-byte-over limits, and material-truncation
classification.

Close the wire values:

- origins: `user`, `assistant`, `tool`, `runtime`, `external`, `omission`;
- kinds: `user_message`, `assistant_message`, `assistant_tool_request`,
  `tool_result`, `runtime_context`, `external_content`, `omission`; and
- accept only the corresponding origin/kind pair for each kind.

Add complete-value parse tests for both enums. Unknown, blank, mismatched, or
invalid UTF-8 entries fail with the bounded non-echoing review validation
error from Task 1.

**Step 2: Verify RED**

Expected: compile failure.

**Step 3: Implement deterministic builder**

Keep raw `content.AgenticMessages` out of the public review domain. Accept
already-labeled builder entries and return an immutable value. The loop adapter
will own conversion from conversation types in Phase 4.

Implement:

```go
func BuildReviewContext(
    input ReviewContext,
    policy ReviewContextPolicy,
) (ReviewContext, error)

func (c ReviewContext) Clone() ReviewContext
```

`BuildReviewContext` validates the non-zero coordinate quartet, non-empty
context/policy revisions, workspace root, working directory, security ceiling,
and gate-policy revision. Workspace paths must already be absolute and clean,
and the working directory must remain within the workspace root. It rejects
invalid UTF-8 and zero/negative limits.
It treats the final user-message entry as current intent and the final
assistant-tool-request entry as the active action. Both must remain represented;
construction rejects input missing either one so the caller leaves review
ineligible. If the active action alone exceeds its explicit limit, construction
also fails.

Apply per-entry limits before total limits. Truncated text keeps a deterministic
UTF-8-safe prefix and suffix separated by a fixed marker. Entry-count or total
budget omission inserts one fixed typed omission entry containing bounded
counts, never source content. Estimate tokens with one documented deterministic
integer rule rather than a model tokenizer. `ReviewTruncation.Applied` records
every applied limit; `Material` includes loss of current intent, active action,
security posture, or required evidence. The returned value owns a cloned entry
slice. The builder must never silently omit or mutate an input entry.

`MaxBytes` measures the exact `encoding/json` byte length of one private
snake-case projection of the complete returned `ReviewContext`: coordinates,
all root metadata, entries, and truncation metadata. It is not a sum of entry
content. Define that projection with the context task and reuse it in the
subject v1 wire so the two representations cannot drift. Oversized immutable
root metadata fails closed when no valid omission can satisfy the bound.
The deterministic token estimate remains
`ceil(sum(entry content bytes)/4)` as specified above.

Because the v1 omission marker records counts but no trustworthy omitted-kind
inventory, every omission is conservatively material in v1. A future wire
revision may distinguish non-material omission only by adding a typed,
validated inventory. Strict subject validation requires the budget bits for an
omission to appear in both `Applied` and `Material`.

**Step 4: Add fuzz coverage**

Fuzz arbitrary UTF-8/invalid byte boundaries and limits. Assert output remains
valid UTF-8, bounded, and deterministic.

**Step 5: Run GREEN and commit**

```bash
git add pkg/gate/review_context.go pkg/gate/review_context_test.go \
  pkg/gate/review_context_fuzz_test.go
git commit -m "feat(gate): add bounded authority-labeled review context"
```

## Task 3: Add review basis, subject cloning, canonical wire, and digest

**Files:**

- Create: `pkg/gate/review_subject.go`
- Create: `pkg/gate/review_subject_test.go`
- Create: `pkg/gate/review_wire.go`
- Create: `pkg/gate/review_wire_test.go`
- Create: `pkg/gate/review_fuzz_test.go`

**Step 1: Write failing basis and clone tests**

Define wished-for tests for:

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

type PermissionReviewSubject struct {
    Basis   ReviewBasis
    Request tool.Request
    Context ReviewContext
}
```

Tests must prove:

- every basis field is required;
- subject construction validates `tool.Request`;
- the basis context revision, gate-policy revision, and security ceiling exactly
  match the built context;
- a non-empty request execution ID is canonical and equals the basis tool
  execution ID;
- `Clone` does not share requirement, candidate, context-entry, or byte slices;
- the digest is stable across map allocation/order and insignificant JSON
  whitespace;
- each basis/request/context/policy field changes the digest;
- duplicate JSON keys, trailing JSON, unknown fields, and unsupported versions
  fail; and
- errors do not echo request contents.

**Step 2: Run RED**

Expected: compile failure for missing types/functions.

**Step 3: Implement strict versioned canonical wire**

Use an unexported v1 wire struct with no maps in the digest path unless keys are
explicitly sorted. Reuse the gate package's strict JSON and duplicate-field
rejection helpers.

The exact wire version is `permission_review_subject.v1`; the encoded gate kind
is always `harness.permission`. Encode UUIDs as canonical lowercase strings and
the digest as fixed-length lowercase hex. Reject non-canonical UUID/digest text
even when a permissive parser could normalize it. Bound encoded input with:

```go
const MaxPermissionReviewSubjectWireBytes = 1 << 20
```

Keep the wire codec unexported because subjects cross the trusted classifier
interface as typed values, not as a public persistence format:

```go
func marshalPermissionReviewSubject(
    subject PermissionReviewSubject,
) ([]byte, error)

func unmarshalPermissionReviewSubject(
    data []byte,
) (PermissionReviewSubject, error)
```

The unmarshal path revalidates every closed context enum and pair, UUID,
request, truncation mask/counter, required entry, basis/context equality, and
stored digest. It wraps strict-JSON scanner/decoder failures in the bounded
non-echoing review validation error rather than returning attacker-controlled
JSON keys or contents.

Before typed conversion it also verifies that every required scalar and
container member has its exact JSON kind. JSON `null` is rejected for strings,
booleans, integers, objects, and arrays, including optional strings whose
canonical value may otherwise be empty. Add a null mutation test for every
field family and nested level.

Implement conceptually:

```go
func NewPermissionReviewSubject(
    basis ReviewBasis,
    request tool.Request,
    context ReviewContext,
) (PermissionReviewSubject, error)

func (s PermissionReviewSubject) Clone() PermissionReviewSubject
func SubjectDigest(s PermissionReviewSubject) ([32]byte, error)
```

Digest the subject with `SubjectDigest` zeroed, then install and validate the
computed value. Avoid a recursive digest. `NewPermissionReviewSubject` accepts
only a zero incoming digest, validates every other field, clones all mutable
input, computes the digest, and returns the stamped subject.
`SubjectDigest` recomputes from an otherwise valid subject while ignoring its
stored digest; the strict decoder separately requires the stored value to equal
that recomputation.

Strict built-context validation accepts only values the v1 builder can emit:

- the final assistant tool request is present and never truncated;
- every truncated entry contains exactly one fixed truncation marker with
  non-empty UTF-8 prefix and suffix;
- truncated current-user, tool-result, runtime, external, and earlier
  assistant-tool-request entries have every exercised compatible applied bit
  marked material;
- every non-budget applied bit is explained by at least one truncated entry,
  and the active-action bit is never valid in a successfully built subject;
- one omission marker has a positive omitted-entry count, an exact nonnegative
  omitted-byte count, and matching budget bits in both `Applied` and
  `Material`; and
- masks/counters/markers cannot underreport one another.

**Step 4: Add fuzz seeds and invariants**

Fuzz strict decoding and digest construction. An error must return a zero
subject/digest and bounded error.

**Step 5: Run GREEN**

Run focused tests, fuzz seed execution, then all `pkg/gate`.

**Step 6: Commit**

```bash
git add pkg/gate/review_subject.go pkg/gate/review_subject_test.go \
  pkg/gate/review_wire.go pkg/gate/review_wire_test.go \
  pkg/gate/review_fuzz_test.go
git commit -m "feat(gate): bind reviews to canonical subjects"
```

## Task 4: Add assessment validation, local policy, and conjunction

**Files:**

- Create: `pkg/gate/review_policy.go`
- Create: `pkg/gate/review_policy_test.go`
- Create: `pkg/gate/reviewer.go`
- Create: `pkg/gate/reviewer_test.go`

**Step 1: Write failing policy matrix tests**

Test:

- Codex-compatible default risk/authorization matrix;
- hard critical-risk refusal;
- absolute-human categories;
- consumer tightening;
- rejection of any attempted relaxation;
- model `allow` inconsistent with local policy becoming human;
- basis mismatch;
- material truncation;
- at least one applicable classifier required;
- all applicable classifiers must allow; and
- any error/stale/needs-human result prevents auto-approval.

**Step 2: Write failing interface/registration tests**

Use small real classifier stubs to specify the focused interface from design
§9. Construction must reject nil, typed nil, duplicates, current-loop models,
background definitions, missing structured output, and descriptor/revision
drift. Evidence-tool policy validation is added to this same registry in Task 7
after the Hustle descriptor can represent it; do not invent placeholder
metadata in `pkg/gate`.

**Step 3: Verify RED**

Expected: compile failure.

**Step 4: Implement minimum policy and registry**

Keep the policy evaluator pure:

```go
const MaxPermissionReviewRationaleBytes = 2048
const MaxPermissionReviewPolicyRevisionBytes = 128
const MaxPermissionClassifierRevisionBytes = 128

type PermissionAssessment struct {
    Basis          ReviewBasis
    Risk           ReviewRisk
    Authorization  ReviewAuthorization
    Categories     []ReviewRiskCategory
    Recommendation ReviewRecommendation
    Rationale      string
}

type PermissionReviewPolicy struct {
    Revision             string
    MaximumAutoRisk      ReviewRisk
    MinimumAuthorization map[ReviewRisk]ReviewAuthorization
    AbsoluteHuman        []ReviewRiskCategory
    MaterialTruncation   ReviewTruncationMask
}

func NewPermissionReviewPolicy(
    revision string,
    maximum ReviewRisk,
    minimum map[ReviewRisk]ReviewAuthorization,
    absoluteHuman []ReviewRiskCategory,
    material ReviewTruncationMask,
) (PermissionReviewPolicy, error)

func DefaultPermissionReviewPolicy(
    revision string,
) (PermissionReviewPolicy, error)

func EvaluatePermissionAssessment(
    policy PermissionReviewPolicy,
    subject PermissionReviewSubject,
    assessment PermissionAssessment,
) ReviewDecision

type PermissionAssessmentOutcome struct {
    Applicable bool
    Status     ReviewStatus
    Assessment PermissionAssessment
}

func CombinePermissionAssessments(
    policy PermissionReviewPolicy,
    subject PermissionReviewSubject,
    outcomes []PermissionAssessmentOutcome,
) ReviewDecision
```

`ReviewDecision` is not a `GateResponse` and exposes no action string. It can
only report `Eligible bool` plus a closed, bounded `ReviewDecisionReason`. Its
reason domain distinguishes eligible, invalid policy, invalid assessment,
basis mismatch, recommendation, risk ceiling, authorization, absolute-human
category, material truncation, no applicable classifier, and non-allow
classifier status. It never carries classifier errors or rationale.

Policy requirements:

- the default is the design §10 matrix: low→unknown, medium→unknown,
  high→medium, with maximum high; critical is never eligible;
- a custom policy is complete for low/medium/high, uses only supported enum
  keys/values and truncation bits, has unique categories, and owns cloned
  slices/maps;
- maximum risk may tighten to medium or low but may not relax to critical;
- high-risk minimum authorization may tighten above medium but never relax
  below medium;
- consumer absolute-human categories and material-truncation bits only add
  restrictions;
- any bit already present in `subject.Context.Truncation.Material` always
  blocks eligibility; `MaterialTruncation` selects additional applied bits and
  the default additional mask is zero;
- evaluation revalidates the policy so mutation of its exported map after
  construction fails closed;
- policy revision is non-empty, bounded, valid UTF-8, and exactly equals the
  subject's gate-policy revision.

Assessment requirements:

- basis exactly equals the subject basis, including digest;
- risk, authorization, recommendation, and categories use the closed domains;
- categories are unique and bounded;
- rationale is valid UTF-8, at most
  `MaxPermissionReviewRationaleBytes`, and non-empty after trimming for every
  non-low assessment;
- rationale is never returned in a decision; and
- a model `allow` that violates local policy becomes a non-eligible decision.

Conjunction requirements:

- an outcome is non-applicable only when `Applicable` is false and status is
  `not_applicable`;
- an applicable outcome is considered only when status is `allowed`;
- any failed/timed-out/cancelled/stale/needs-human/inconsistent status fails
  the entire conjunction;
- at least one classifier must be applicable; and
- every applicable assessment must be individually eligible.

Define and register:

```go
type PermissionClassifier interface {
    Name() hustle.Name
    Revision() string
    Definition() hustle.Definition
    Applies(PermissionReviewSubject) bool
    MarshalInput(PermissionReviewSubject) (json.RawMessage, error)
    ValidateResult(
        PermissionReviewSubject,
        hustle.Result,
    ) (PermissionAssessment, error)
}

type PermissionClassifierSet struct {
    // unexported immutable ordered storage
}

func NewPermissionClassifierSet(
    classifiers ...PermissionClassifier,
) (PermissionClassifierSet, error)

func (s PermissionClassifierSet) Classifiers() []PermissionClassifier
```

The constructor preserves order and clones the slice. It rejects zero
classifiers, nil/typed-nil implementations, invalid or duplicate names,
duplicate revisions, blank/oversized/non-UTF-8 revisions, zero definitions,
non-blocking participation, non-named model sources, missing structured output,
name mismatch, and classifier revision mismatch with the definition
descriptor's declared policy revision. It must not call `Applies`,
`MarshalInput`, or `ValidateResult` during registration.

**Step 5: Run GREEN and commit**

```bash
git add pkg/gate/review_policy.go pkg/gate/review_policy_test.go \
  pkg/gate/reviewer.go pkg/gate/reviewer_test.go
git commit -m "feat(gate): validate and combine classifier assessments"
```

## Task 5: Add secret-free permission-review events

**Files:**

- Create: `pkg/event/permission_review.go`
- Create: `pkg/event/permission_review_test.go`
- Create: `pkg/event/permission_review_fuzz_test.go`
- Modify: `pkg/event/validate.go`
- Modify: `pkg/event/validate_internal_test.go`
- Modify: `pkg/event/marshal.go`
- Modify: `pkg/event/marshal_test.go`
- Modify: `pkg/event/header_test.go`
- Modify: `pkg/event/doc.go`

**Step 1: Write failing event validation and codec tests**

Specify the two events from design §16.2:

```go
type PermissionReviewStarted struct {
    // enduring, loopScoped, Header
    GateID             gate.ID
    ToolExecutionID    uuid.UUID
    Classifier         hustle.Name
    ClassifierRevision string
}

type PermissionReviewCompleted struct {
    // enduring, loopScoped, Header
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

Assert:

- both are enduring, internal, loop-scoped, non-terminal events;
- both require the full session/loop/turn/step coordinate quartet;
- valid gate/tool/classifier/revision fields;
- status-dependent fields;
- bounded unique categories;
- exact JSON discriminator;
- strict round trip;
- event union drift guards updated; and
- reflection/JSON confirms there is no command, context, evidence, output,
  prompt, rationale, credential, rule, or grant field.

Validation is exact:

- visibility must be `Internal`;
- gate ID, tool execution ID, classifier name, and classifier revision are
  required;
- classifier name uses `hustle.Name.Validate`; revision is valid UTF-8,
  non-blank, and at most `gate.MaxPermissionClassifierRevisionBytes`;
- completed status must be one of the closed `gate.ReviewStatus` values;
- `allowed` requires valid risk and authorization, valid unique categories,
  and `AutoApproved=true`;
- `needs_human` requires valid risk and authorization, valid unique categories,
  and `AutoApproved=false`; and
- `not_applicable`, `timed_out`, `failed`, `cancelled`, and `stale` require
  zero risk/authorization, no categories, and `AutoApproved=false`.

The wire uses exact discriminators `PermissionReviewStarted` and
`PermissionReviewCompleted` and snake-case field names. Add both types to every
sealed-union/class/scope/terminal/visibility/identity/marshal/decode drift guard.
The existing event envelope's additive unknown-field compatibility remains
unchanged; do not make the global decoder stricter as part of this task.
Add fuzz seeds for both discriminators and assert decode never panics; every
successful decode validates and remarshal/redecode is a fixed point.

**Step 2: Verify RED**

Expected: unknown event types.

**Step 3: Implement and update the sealed event union**

Follow existing exhaustive switch style. Do not add generic reflection-based
event mutation.

**Step 4: Run GREEN**

Run focused permission-review tests and all `pkg/event`.

**Step 5: Commit**

```bash
git add pkg/event
git commit -m "feat(event): audit permission classifier reviews"
```

## Phase 1 review gate

Run:

```bash
GOCACHE=/private/tmp/looprig-harness-go-cache GOFLAGS=-mod=vendor \
  go test -race ./pkg/gate ./pkg/event
```

Review the complete Phase 1 diff for:

- no authority in assessment/decision types;
- exact basis binding;
- deterministic bounded context;
- closed enum drift guards;
- no sensitive event fields; and
- no import-boundary regression.

Commit review fixes separately with `fix(review): ...`.

---

# Phase 2 — Optional evidence-tool definitions in Hustles

## Task 6: Add tool-loop limits and immutable definition options

**Files:**

- Modify: `pkg/hustle/definition.go`
- Modify: `pkg/hustle/definition_errors.go`
- Modify: `pkg/hustle/definition_test.go`
- Modify: `pkg/hustle/descriptor_test.go`
- Modify: `pkg/hustle/deps_test.go`

**Step 1: Write failing option and validation tests**

Specify `ToolLoopLimits`, a self-documenting `EvidenceToolPolicy`, and
`WithEvidenceTools(policy)`.

Test:

- zero policy preserves the exact old descriptor;
- positive bounds are required together;
- definitions and produced names are non-empty and unique;
- structured output is required;
- policy revision is required;
- duplicate option rejected;
- background definitions rejected for evidence tools;
- descriptor clone and validation;
- hashes change for every behavioral field; and
- prompt-only/tool-less compaction fixtures remain byte-identical.

**Step 2: Verify RED**

Expected: compile failure.

**Step 3: Implement immutable policy**

Use named structs, not positional booleans. Store defensive copies of
`tool.Definition` interfaces and secret-free metadata. Add only digest/revision
data to `DefinitionDescriptor`.

**Step 4: Run GREEN and regression**

Run `pkg/hustle` and `pkg/rig` fingerprint tests.

**Step 5: Commit**

```bash
git add pkg/hustle
git commit -m "feat(hustle): define bounded evidence tool loops"
```

## Task 7: Bind evidence tools and verify concrete schema identity

**Files:**

- Modify: `pkg/hustle/definition.go`
- Create: `pkg/hustle/evidence.go`
- Create: `pkg/hustle/evidence_test.go`
- Modify: `pkg/gate/reviewer.go`
- Modify: `pkg/gate/reviewer_test.go`
- Modify: `internal/sessionruntime/hustle.go`
- Modify: `internal/sessionruntime/hustle_test.go`

**Step 1: Write failing bind tests**

Tests must prove:

- workspace-required definitions receive only workspace bindings;
- delegate bindings are absent;
- built tools match frozen produced names;
- `ToolInfo` is non-nil, valid, uniquely named, and has valid JSON schema;
- schema and description digests contribute to bound identity;
- build/schema drift fails session construction;
- typed nil tools fail; and
- tool-less definitions do not build a toolset;
- a registered permission classifier must declare a non-empty evidence-tool
  policy revision whose descriptor identity survives binding.

**Step 2: Verify RED**

**Step 3: Extend Hustle bindings narrowly**

Add only the workspace/read collaborators needed to build evidence tools.
Never pass the session, gate registrar, loop controller, rule writer, grant
issuer, or delegate controller.

**Step 4: Run GREEN**

Run `pkg/hustle` and focused `internal/sessionruntime` construction tests.

**Step 5: Commit**

```bash
git add pkg/hustle internal/sessionruntime/hustle.go \
  internal/sessionruntime/hustle_test.go pkg/gate/reviewer.go \
  pkg/gate/reviewer_test.go
git commit -m "feat(hustle): bind fingerprinted evidence tools"
```

## Task 8: Include classifier/evidence identity in rig fingerprints

**Files:**

- Modify: `pkg/rig/options.go`
- Modify: `pkg/rig/definition.go`
- Modify: `pkg/rig/fingerprint.go`
- Modify: `pkg/rig/hustle_fingerprint_test.go`
- Modify: `pkg/rig/hustle_test.go`
- Modify: `pkg/rig/README.md`

**Step 1: Write failing fingerprint drift tests**

Mutate one field at a time: tool policy revision, produced tool name, schema
digest, each loop bound, review policy revision, and classifier order. Every
mutation must alter topology/config identity.

**Step 2: Verify RED**

**Step 3: Implement minimal fingerprint projection**

Do not include prompts, raw schemas, tool descriptions, model secrets, or
workspace paths in the wrong identity layer.

**Step 4: Run GREEN and commit**

```bash
git add pkg/rig
git commit -m "feat(rig): fingerprint classifier evidence policy"
```

## Phase 2 review gate

Run:

```bash
GOCACHE=/private/tmp/looprig-harness-go-cache GOFLAGS=-mod=vendor \
  go test -race ./pkg/hustle ./pkg/rig ./internal/sessionruntime
```

Review capability attenuation, immutable identity, old-definition
compatibility, and absence of generic public Hustle execution.

---

# Phase 3 — Bounded multi-round Hustle runtime

## Task 9: Parse terminal output versus ordinary evidence calls

**Files:**

- Create: `internal/hustleruntime/tool_response.go`
- Create: `internal/hustleruntime/tool_response_test.go`
- Create: `internal/hustleruntime/tool_response_fuzz_test.go`
- Modify: `internal/hustleruntime/errors.go`

**Step 1: Write failing response-shape tests**

Cover:

- terminal structured output only;
- one and several ordinary calls;
- unknown tool;
- malformed args;
- duplicate IDs;
- missing IDs;
- mixed text/tool;
- mixed terminal/ordinary;
- duplicate terminal;
- finish-reason contradictions;
- nil/typed-nil blocks; and
- exact output-byte boundaries.

**Step 2: Verify RED**

**Step 3: Implement a pure exhaustive classifier**

Return a sealed internal variant: terminal or evidence calls, never both.
Errors contain only bounded shape/reason values.

**Step 4: Fuzz and run GREEN**

**Step 5: Commit**

```bash
git add internal/hustleruntime/tool_response*
git commit -m "feat(hustle): classify evidence and terminal responses"
```

## Task 10: Add reviewer evidence preparation and access enforcement

**Files:**

- Create: `internal/hustleruntime/evidence_runner.go`
- Create: `internal/hustleruntime/evidence_runner_test.go`
- Modify: `internal/hustleruntime/contracts.go`
- Modify: `internal/hustleruntime/execution.go`

**Step 1: Write failing security tests**

Use real tiny prepared tools to prove:

- `PrepareCall` runs exactly once;
- missing preparer refuses execution;
- execution ID mismatch refuses;
- only allowlisted read-only requirements run;
- gated, denied, unknown, and source-error states refuse;
- grants and reusable candidates refuse;
- write/delegate/session capabilities are unreachable;
- prepared artifact is delivered to the tool;
- result content is paired and byte bounded; and
- panic becomes a bounded internal failure.

**Step 2: Verify RED**

**Step 3: Implement dedicated sequential evidence runner**

Do not reuse the loop runner wholesale; it carries human gate, event, mutation,
and parallel semantics that Hustles must not gain. Reuse leaf validation and
prepared-call helpers where possible without widening visibility.

**Step 4: Run GREEN and commit**

```bash
git add internal/hustleruntime
git commit -m "feat(hustle): enforce read-only evidence calls"
```

## Task 11: Implement bounded multi-round execution and usage aggregation

**Files:**

- Modify: `internal/hustleruntime/execution.go`
- Create: `internal/hustleruntime/tool_execution_test.go`
- Modify: `internal/hustleruntime/execution_test.go`
- Modify: `internal/hustleruntime/advanced_test.go`
- Modify: `internal/hustleruntime/output_reason_test.go`

**Step 1: Write failing end-to-end runtime tests**

Script fake inference responses for:

- one evidence round then terminal;
- maximum rounds/calls/per-round;
- one over each bound;
- aggregate evidence byte exact/over;
- sequential call order;
- private message pairing;
- usage aggregation across rounds;
- checked usage overflow;
- model capability mismatch;
- timeout during inference and tool execution;
- cancellation;
- ignored cancellation/worker poisoning;
- validator failure; and
- tool-less single-invoke compatibility.

**Step 2: Verify RED**

**Step 3: Implement the loop**

Keep one execution deadline for the entire run. Clone every provider-owned
response. Validate request features before each call. Accumulate usage with
existing normalized arithmetic. Never publish intermediate evidence.

**Step 4: Run GREEN**

Run all `internal/hustleruntime`.

**Step 5: Commit**

```bash
git add internal/hustleruntime
git commit -m "feat(hustle): execute bounded evidence tool rounds"
```

## Task 12: Add one-shot classified retry without weakening deadlines

**Files:**

- Create: `internal/hustleruntime/retry.go`
- Create: `internal/hustleruntime/retry_test.go`
- Modify: `internal/hustleruntime/execution.go`
- Modify: `pkg/hustle/run.go`
- Modify: `pkg/hustle/run_test.go`

**Step 1: Write failing retry tests**

Verify exactly one retry for classified transient inference and recoverable
terminal parse errors. Verify no retry for every forbidden class in design
§12.6. Assert the second attempt starts from immutable input with no first
attempt evidence and remains inside the original deadline.

**Step 2: Verify RED**

**Step 3: Implement bounded policy**

Keep retry opt-in on the definition/reviewer policy so existing compaction does
not change behavior.

**Step 4: Run GREEN and commit**

```bash
git add pkg/hustle internal/hustleruntime
git commit -m "feat(hustle): retry transient classifier runs once"
```

## Phase 3 review gate

Run:

```bash
GOCACHE=/private/tmp/looprig-harness-go-cache GOFLAGS=-mod=vendor \
  go test -race ./internal/hustleruntime ./pkg/hustle
```

Review all runtime goroutine ownership, cancellation, evidence authority,
usage accounting, retry, redaction, and tool-less compatibility.

---

# Phase 4 — Live context capture and gate race

## Task 13: Capture exact live permission-review context at the batch boundary

**Files:**

- Create: `internal/loopruntime/review_context.go`
- Create: `internal/loopruntime/review_context_test.go`
- Modify: `internal/loopruntime/turn.go`
- Modify: `internal/loopruntime/runner.go`
- Modify: `internal/loopruntime/gate.go`
- Modify: `pkg/loop/tool_context.go`

**Step 1: Write failing turn-level tests**

Assert the snapshot contains:

- current and retained user intent;
- staged assistant tool use before step commit;
- recent tool results;
- correct authority labels;
- loop/turn/step coordinates;
- workspace/cwd/policy/security metadata;
- retry reason;
- stable revision; and
- explicit truncation markers.

Assert ordinary tools cannot retrieve the private snapshot through public
`pkg/loop` APIs.

**Step 2: Verify RED**

**Step 3: Implement private propagation**

Create once per batch, clone per approval registration, and attach the exact
prepared request/tool execution ID later. Do not add it to gate durable payload.

**Step 4: Run GREEN**

Run focused `internal/loopruntime` tests and its full package.

**Step 5: Commit**

```bash
git add internal/loopruntime pkg/loop/tool_context.go
git commit -m "feat(loop): capture permission review context"
```

## Task 14: Start asynchronous reviews only after GateOpened commits

**Files:**

- Modify: `internal/loopruntime/gate.go`
- Modify: `internal/loopruntime/loop.go`
- Create: `internal/loopruntime/gate_review_test.go`
- Modify: `internal/sessionruntime/gates.go`
- Create: `internal/sessionruntime/review_adapter.go`
- Create: `internal/sessionruntime/review_adapter_test.go`

**Step 1: Write failing lifecycle tests**

Prove ordering:

1. prepared record;
2. blocker install;
3. `GateOpened`;
4. runner ack;
5. review start.

Activation failure must start no review. Review inference must never block the
loop actor. A human response immediately after `GateOpened` must be accepted.

**Step 2: Verify RED**

**Step 3: Add a focused private registrar seam**

Do not expose a generic session runner. The loop passes a live-only request to
the session after successful activation.

**Step 4: Run GREEN and commit**

```bash
git add internal/loopruntime internal/sessionruntime
git commit -m "feat(session): start review after permission gate activation"
```

## Task 15: Implement local validation and exactly-once classifier approval

**Files:**

- Modify: `pkg/gate/response.go`
- Modify: `pkg/gate/response_test.go`
- Modify: `internal/sessionruntime/review_adapter.go`
- Create: `internal/sessionruntime/review_race_test.go`
- Modify: `internal/sessionruntime/gates.go`
- Modify: `internal/sessionruntime/gates_test.go`

**Step 1: Write failing race tests**

Test human-first, classifier-first, simultaneous, duplicate classifier,
gate-close, basis drift, policy drift, context drift, security drift,
observation drift, and stale result. Assert:

- exactly one `GateResolved`;
- at most one routed approve;
- classifier path only emits `ApprovalApprove`;
- public callers cannot forge classifier source;
- evaluator still mints grants; and
- every non-allow path leaves the gate open.

**Step 2: Verify RED**

**Step 3: Implement private classifier response**

Add `ResponseFromClassifier`, but stamp it only inside a private session method.
Re-read gate state and recompute the entire basis immediately before calling
the ordinary claim/response logic.

**Step 4: Run GREEN and commit**

```bash
git add pkg/gate internal/sessionruntime
git commit -m "feat(gate): race classifier approval with human response"
```

## Task 16: Add cancellation groups, restore behavior, and circuit breaker

**Files:**

- Create: `internal/sessionruntime/review_state.go`
- Create: `internal/sessionruntime/review_state_test.go`
- Modify: `internal/sessionruntime/restore_gates.go`
- Modify: `internal/sessionruntime/restore_gates_test.go`
- Modify: `internal/sessionruntime/hustle_shutdown_test.go`
- Modify: `internal/sessionruntime/session.go`

**Step 1: Write failing lifecycle tests**

Cover gate resolve/close, loop interrupt, timeout, shutdown, restore,
unmatched-Hustle repair, no replay, counters, identical subjects, threshold
warning once, per-turn reset, and optional interrupt policy.

**Step 2: Verify RED**

**Step 3: Implement bounded state**

Store cancellation handles and counters only in memory. Restored gates have no
review handle and remain human-only.

**Step 4: Run GREEN and commit**

```bash
git add internal/sessionruntime
git commit -m "feat(session): bound permission review lifecycle"
```

## Task 17: Publish review events without leaking review data

**Files:**

- Modify: `internal/sessionruntime/review_adapter.go`
- Create: `internal/sessionruntime/review_audit_test.go`
- Modify: `pkg/sessionstore/catalog_hustle_usage_test.go`
- Modify: `pkg/serve/catalogreader/*_test.go`

**Step 1: Write failing audit tests**

Exercise success, human, stale, failure, timeout, and cancellation. Search
marshaled journals/events for seeded secrets from context, command, evidence,
prompt, output, and rationale.

**Step 2: Verify RED**

**Step 3: Publish bounded events**

Use checked durable publication. Audit append failure follows existing session
fault semantics; expected classifier failure does not.

**Step 4: Run GREEN and commit**

```bash
git add internal/sessionruntime pkg/sessionstore pkg/serve
git commit -m "feat(session): audit permission reviews safely"
```

## Phase 4 review gate

Run:

```bash
GOCACHE=/private/tmp/looprig-harness-go-cache GOFLAGS=-mod=vendor \
  go test -race ./internal/loopruntime ./internal/sessionruntime \
  ./pkg/gate ./pkg/event ./pkg/sessionstore ./pkg/serve/...
```

Review exact ordering, actor non-blocking behavior, human availability,
single-claim semantics, private provenance, restore, cancellation, and leak
tests.

---

# Phase 5 — New classifiers repository and command-safety product

## Task 18: Scaffold `github.com/looprig/classifiers` with no root Go package

**Files:**

- Create under `/Users/ipotter/code/looprig/classifiers`:
  - `go.mod`
  - `LICENSE`
  - `README.md`
  - `CONTRIBUTING.md`
  - `Makefile`
  - `.gitignore`
  - `pkg/commandsafety/doc.go`
  - `pkg/catalog/doc.go`
  - `internal/buildtest/layout_test.go`

**Step 1: Initialize repository and branch**

Create the directory, initialize Git, and create `feat/command-safety`.
Do not create a root `.go` file.

**Step 2: Write failing layout/dependency tests**

Test:

- no root-level `.go` files;
- module path exact;
- public packages only under `pkg`;
- implementation packages under `internal`;
- no import cycle or Harness internal import; and
- no replacement directives in a simulated release modfile.

**Step 3: Verify RED**

Expected: missing package/docs or failing layout.

**Step 4: Add minimal scaffold**

Use Apache-2.0 to match the sibling ecosystem unless repository policy says
otherwise. Add local development replacements only where sibling modules
require them, with an explicit release guard.

**Step 5: Vendor and run GREEN**

Use the module's documented vendoring workflow. Run `go test -race ./...`.

**Step 6: Commit**

```bash
git add .
git commit -m "chore: scaffold classifier module"
```

## Task 19: Implement command-safety wire, schema, prompt, and definition

**Files:**

- Create: `pkg/commandsafety/commandsafety.go`
- Create: `pkg/commandsafety/commandsafety_test.go`
- Create: `internal/wire/input.go`
- Create: `internal/wire/input_test.go`
- Create: `internal/wire/output.go`
- Create: `internal/wire/output_test.go`
- Create: `internal/prompt/command_safety.md`
- Create: `internal/prompt/prompt.go`
- Create: `internal/prompt/prompt_test.go`
- Create: `internal/wire/schema.json`

**Step 1: Write failing public construction tests**

Specify a named `Options` API, immutable named model binding, default policy,
strict schema, revisions, and evidence definitions. Test invalid/typed-nil
inputs, capability mismatch, clone behavior, and descriptor identity.

**Step 2: Write failing strict wire tests**

Require all basis/risk/authorization/category/recommendation fields. Reject
unknown/duplicate/trailing/null/oversized values. Ensure rationale is bounded
and never appears in errors.

**Step 3: Write failing prompt invariant tests**

Assert the prompt contains the required policy headings and untrusted-evidence
instructions without snapshot-testing the entire prose. Hash the full prompt
through descriptor identity.

**Step 4: Verify RED**

**Step 5: Implement minimum classifier**

Return a public `gate.PermissionClassifier`. Keep prompt and codecs internal.

**Step 6: Run GREEN and commit**

```bash
git add pkg/commandsafety internal/prompt internal/wire
git commit -m "feat: define command safety classifier"
```

## Task 20: Implement deterministic policy and applicability

**Files:**

- Create: `internal/policy/policy.go`
- Create: `internal/policy/policy_test.go`
- Modify: `pkg/commandsafety/commandsafety.go`
- Modify: `pkg/commandsafety/commandsafety_test.go`

**Step 1: Write failing policy tests**

Port every Codex taxonomy branch in independently written tables: exfiltration,
credentials, security weakening, destructive actions, Git, low-risk actions,
production/shared state, prompt injection, authorization, and post-warning
approval.

**Step 2: Write failing applicability tests**

Use typed requirement kinds and combinations. Do not route by display tool
name.

**Step 3: Verify RED**

**Step 4: Implement immutable policy**

Model assessment plus Harness local policy must produce the expected
eligibility. Policy text/config revision changes definition identity.

**Step 5: Run GREEN and commit**

```bash
git add pkg/commandsafety internal/policy
git commit -m "feat: enforce command safety policy"
```

## Task 21: Add filesystem and repository evidence tools

**Files:**

- Create: `internal/evidence/path.go`
- Create: `internal/evidence/path_test.go`
- Create: `internal/evidence/path_integration_test.go`
- Create: `internal/evidence/git.go`
- Create: `internal/evidence/git_test.go`
- Create: `internal/evidence/git_integration_test.go`
- Create: `internal/evidence/visibility.go`
- Create: `internal/evidence/visibility_test.go`
- Create: `internal/evidence/catalog.go`
- Create: `internal/evidence/catalog_test.go`
- Modify: `pkg/commandsafety/commandsafety.go`

**Step 1: Write failing security tests first**

For every tool, test malformed arguments, containment, denied reads, symlink
behavior, exact byte limits, cancellation, and no mutation. Snapshot filesystem
and Git state before/after each invocation and deep-compare.

**Step 2: Write failing behavior tests**

Cover canonical metadata, missing/empty directories, bounded list/read/search,
Git root/status/diff/remotes/branch/default-branch, and injected read-only
visibility resolver.

**Step 3: Write failing integration tests**

Tag both integration files with `//go:build integration`. Exercise real
temporary filesystem trees and real `git` subprocesses, including symlink
escapes, cancellation, unusual but valid file names, detached HEAD, empty
repositories, and before/after snapshots proving no filesystem or repository
mutation.

**Step 4: Verify RED**

Run both:

```bash
GOCACHE=/private/tmp/looprig-classifiers-go-cache GOFLAGS=-mod=vendor \
  go test -race ./internal/evidence
GOCACHE=/private/tmp/looprig-classifiers-go-cache GOFLAGS=-mod=vendor \
  go test -tags integration -race ./internal/evidence
```

**Step 5: Implement focused prepared tools**

Use direct argv for Git with a fixed allowlisted subcommand set and sanitized
environment. Never expose arbitrary `git` arguments or shell.

**Step 6: Run GREEN and commit**

```bash
git add internal/evidence pkg/commandsafety
git commit -m "feat: gather bounded command safety evidence"
```

## Task 22: Build Codex-parity evaluation corpus and runner

**Files:**

- Create: `internal/corpus/case.go`
- Create: `internal/corpus/corpus_test.go`
- Create: `internal/corpus/testdata/*.json`
- Create: `pkg/commandsafety/evaluation.go`
- Create: `pkg/commandsafety/evaluation_test.go`
- Create: `docs/evaluations/README.md`
- Create: `docs/evaluations/baseline.md`

**Step 1: Write failing corpus completeness tests**

Require at least one case for every design §22.6 category, each risk level,
each authorization level, every absolute-human category, evidence failure,
truncation, injection, and basis mismatch.

**Step 2: Add Codex parity manifest**

Record the corresponding Guardian scenario/category, expected classifier
assessment, expected local eligibility, and whether Harness is equal or
stricter. Do not copy Codex source or proprietary fixture text verbatim.

**Step 3: Verify RED**

**Step 4: Implement deterministic evaluation runner**

The runner accepts a fake/client binding and emits aggregate confusion/risk
metrics without raw private content.

**Step 5: Run GREEN**

Require zero critical false allows and no undocumented looser result.

**Step 6: Commit**

```bash
git add internal/corpus pkg/commandsafety/evaluation.go \
  pkg/commandsafety/evaluation_test.go docs/evaluations
git commit -m "test: add command safety evaluation corpus"
```

## Phase 5 review gate

Run all classifiers tests with `-race`, layout checks, static analysis, and the
corpus. Review against Codex Guardian policy and prompt behavior. Require a
written parity table and zero Critical/Important findings.

---

# Phase 6 — Consumer composition in CodeRig

## Task 23: Create isolated CodeRig worktree and add explicit classifier config

**Files:**

- Create adjacent CodeRig worktree and feature branch.
- Modify exact config files discovered by `rg "roleGate|AccessProfile|rig.Define"`.
- Create focused config tests next to the owning package.
- Update CodeRig dependency and vendor metadata.

**Step 1: Verify clean baseline**

Run CodeRig's documented vendored race suite before edits.

**Step 2: Write failing config tests**

Specify:

- off by default;
- explicit enable;
- named model required;
- selectable strict/default policy;
- duplicate registration rejected;
- role/security ceiling inherited but not widened;
- headless/unattended behavior remains human/fail-closed as configured; and
- config fingerprint changes.

**Step 3: Verify RED**

**Step 4: Implement composition**

Construct `commandsafety` in the application composition root and pass it to
Harness rig options. Do not add classifier code to `roleGate` or duplicate
policy logic.

**Step 5: Vendor, run GREEN, and commit**

Commit module changes and vendor changes together.

## Task 24: Add CodeRig end-to-end permission review tests

**Files:**

- Create: `internal/app/permission_review_integration_test.go`
- Add fake classifier inference fixtures.

**Step 1: Write failing end-to-end tests**

Cover:

- safe command auto-approved once;
- evidence lookup;
- human answers while classifier blocks;
- classifier needs-human leaves gate;
- classifier timeout;
- critical risk;
- stale basis;
- no persistent rule;
- exact grant mint after auto-approval; and
- disabled config preserves old behavior.

**Step 2: Verify RED**

The file uses `//go:build integration`. Run:

```bash
GOCACHE=/private/tmp/looprig-coderig-go-cache GOFLAGS=-mod=vendor \
  go test -tags integration -race ./internal/app \
  -run '^TestPermissionReview'
```

**Step 3: Add only necessary test/application plumbing**

**Step 4: Run GREEN and commit**

Run the focused integration command, full integration-tagged application
package, default unit suite, and CodeRig race suite. Confirm the integration
test actually executes by checking the named test in verbose output.

## Phase 6 review gate

Review consumer boundaries, secure defaults, dependency/vendor changes, and
absence of duplicated classifier policy.

---

# Phase 7 — Cross-module integration, hardening, and release

## Task 25: Add cross-module integration tests

**Files:**

- Create adjacent `tests` worktree and feature branch.
- Create: `tests/permission_classifier_integration_test.go`
- Modify: `tests/go.mod`
- Modify: `tests/go.sum`
- Modify release-modfile and dependency-boundary fixtures as required.

**Step 1: Verify baseline**

Run existing integration-tagged tests with documented vendored/modfile flags.

**Step 2: Write failing real composition tests**

Import Harness, classifiers, CodeRig-facing seams where allowed, tools,
sandbox, and stores only in the integration module. Cover safe allow, human
fallback, race, restore, and no leaked review content.

The file uses the platform-aware tag:

```go
//go:build integration && (darwin || (linux && !android))
```

**Step 3: Verify RED**

Run:

```bash
LOOPRIG_LIVE_NETWORK=0 GOWORK=off \
  go test -tags integration -race -count=1 \
  -run '^TestPermissionClassifier' ./...
```

**Step 4: Add minimum integration plumbing**

No production policy belongs in `tests`.

**Step 5: Run GREEN and commit**

Run the focused command, then `make test` in the tests module. Use verbose
output once to prove the integration-tagged tests are selected rather than
silently excluded.

## Task 26: Add fuzz, race, leak, and dependency hardening

**Files:**

- Modify/add fuzz tests in Harness and classifiers from design §22.5.
- Modify dependency-boundary tests in each owning module.
- Add repository root-layout check to cross-module CI.

**Step 1: Add failing regression seeds for every bug found during phases**

Every fix discovered by review begins with a reproducing failing test.

**Step 2: Run fuzz smoke**

For each fuzz target:

```bash
go test -run '^$' -fuzz '<target>' -fuzztime=10s ./owning/package
```

**Step 3: Run race and leak sweeps**

Search journal/event JSON for seeded sensitive strings. Run repeated race tests
with `-count=50` for claim/cancel cases.

**Step 4: Fix via TDD and commit**

## Task 27: Refresh documentation, vendor trees, and release guards

**Files:**

- Modify: Harness `pkg/hustle/README.md`
- Modify: Harness `pkg/gate/README.md`
- Modify: Harness `docs/ECOSYSTEM.md`
- Modify: Harness `docs/TODO.md`
- Modify: classifiers `README.md`, `CONTRIBUTING.md`, evaluation docs
- Modify: CodeRig configuration/user docs
- Modify vendor trees and module release guards in every consumer.

**Step 1: Write/extend documentation tests first**

Examples must compile. Release guards must fail against local replacements and
pass against clean simulated release modfiles.

**Step 2: Update docs**

Document:

- enable/disable;
- model capability requirements;
- evidence boundaries;
- human fallback;
- audit/privacy;
- policy tuning;
- evaluation workflow; and
- restore behavior.

**Step 3: Refresh vendor trees intentionally**

Review every dependency/version diff. No unrelated upgrade is accepted.

**Step 4: Run GREEN and commit per module**

## Task 28: Full acceptance and final independent reviews

**Files:**

- Update implementation plan acceptance ledger at the bottom of this file.
- No production changes without a new failing test.

**Step 1: Run complete verification**

Harness:

```bash
GOCACHE=/private/tmp/looprig-harness-go-cache make test
GOCACHE=/private/tmp/looprig-harness-go-cache make lint
GOCACHE=/private/tmp/looprig-harness-go-cache make vuln
```

Run equivalent test/lint/vulnerability/vendor commands in classifiers,
CodeRig, tools if changed, and integration tests.

**Step 2: Run corpus acceptance**

Require:

- zero critical false allows;
- zero undocumented result looser than Guardian;
- every corpus category represented;
- deterministic local policy;
- bounded latency/usage report; and
- review evidence redaction.

**Step 3: Dispatch final spec reviewer**

Review the entire multi-repository diff against every design acceptance
criterion and security invariant.

**Step 4: Dispatch final security/code-quality reviewer**

Provide all repository base/head SHAs and test output. Fix and re-review every
Critical/Important issue.

**Step 5: Use verification-before-completion**

Rerun affected verification after final fixes. Do not claim success from stale
output.

**Step 6: Use finishing-a-development-branch**

Present merge/PR/cleanup choices for each repository. Do not merge or push
without explicit user authorization.

---

# Acceptance ledger

Update this table only from fresh command output:

| Criterion | Evidence |
| --- | --- |
| Safe request auto-approves once | pending |
| Human can answer during review | pending |
| Human/classifier races resolve exactly once | pending |
| Every non-allow leaves gate open | pending |
| Critical never auto-approves | pending |
| No persistent approval/rule/grant authority | pending |
| Evidence tools are read-only and bounded | pending |
| Basis/security/observation drift blocks allow | pending |
| Durable audit excludes sensitive review data | pending |
| Restore is human-only | pending |
| Classifiers combine conjunctively | pending |
| Codex scenarios equal or stricter | pending |
| Harness full race suite | pending |
| Classifiers full race suite | pending |
| CodeRig full race suite | pending |
| Cross-module integration suite | pending |
| Lint/vulnerability/vendor/release guards | pending |
