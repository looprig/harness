# Design: Harness Operation Hooks

**Date:** 2026-07-08
**Revised:** 2026-07-27
**Status:** Approved
**Scope:** Typed, in-process Go hooks only

This revision supersedes the earlier point-registry design in this document.

## 1. Decision

Harness will add a small synchronous interception API around real operations.
It will not add a second lifecycle event system or copy the user-script hook
platforms exposed by Claude Code, Codex, Grok, and OpenCode.

The design has two deliberately separate extension types:

- **guards** make a synchronous allow-or-block policy decision before a small
  set of operations;
- **around hooks** propagate context and observe the complete duration and
  outcome of an operation without changing its result.

Committed lifecycle facts remain on the existing `event.Event` stream. Session
start, loop stop, turn terminal, compaction commit, and similar facts do not get
duplicate hook points.

## 2. Why This Shape

The agent hook systems reviewed for this design solve a broader problem than
Harness:

- Claude Code and Codex provide layered configuration, matchers, shell commands,
  prompt/agent handlers, output protocols, input rewriting, and UI reporting.
- Grok provides command and HTTP hooks with sequential pre-tool decisions, but
  treats hook failures as allow.
- OpenCode provides typed callbacks in deterministic plugin order, but callbacks
  share mutable output objects and can rewrite broad runtime state.

Harness is an embeddable Go runtime with an existing durable event stream. It
does not need handler discovery, configuration merging, shell execution, remote
callbacks, regex matching, model-context injection, or shared mutation in core.

The useful ideas retained are:

- typed callbacks;
- deterministic registration order;
- an explicit pre-operation policy seam;
- complete operation outcomes;
- context propagation for tracing;
- first denial wins.

The complexity deliberately omitted is:

- YAML, JSON, TOML, or settings-file configuration;
- shell, HTTP, MCP, prompt, or agent handlers;
- matcher languages;
- async/background hooks;
- argument, result, message, or configuration rewriting;
- user trust and hook discovery;
- hook-specific UI lifecycle events;
- per-point failure-policy matrices.

Those features may be built by an application outside Harness using this Go API.

## 3. Goals

- Give embedders synchronous policy control at stable semantic boundaries.
- Support OpenTelemetry spans and context propagation without an
  OpenTelemetry-specific runtime API.
- Make ordering, failure, cancellation, panic, and pairing behavior exact.
- Preserve immutable rig/session/loop definitions.
- Keep observation-only integration changes out of configuration drift.
- Reuse durable events for committed facts.

## 4. Non-Goals

- Hooks do not replace, replay, or persist as events.
- Hooks do not mutate requests, tool arguments, results, messages, or runtime
  definitions.
- Hooks do not recover an operation or replace its result.
- Hooks are not a plugin loader or process boundary.
- Hooks do not run inside foreign agent backends. Harness-owned wrapper
  operations may still be observed.
- Hooks do not instrument hustles in v1.
- Hooks do not provide automatic timeouts for in-process callbacks.

## 5. Public Package

Add `pkg/hook`.

```go
package hook

type Operation uint8

const (
	OperationTurn Operation = iota + 1
	OperationStep
	OperationInference
	OperationCompaction
	OperationToolCall
	OperationGateWait
	OperationToolExecution
	OperationJournalAppend
)

type Outcome uint8

const (
	OutcomeCompleted Outcome = iota + 1
	OutcomeDenied
	OutcomeFailed
	OutcomeCanceled
)

type GuardFunc func(context.Context, Call) error
type BeginFunc func(context.Context, Call) (context.Context, FinishFunc)
type FinishFunc func(Result)

type Guard struct {
	Operation Operation
	Check     GuardFunc
}

type Around struct {
	Operation Operation
	Begin     BeginFunc
}

type Set struct {
	PolicyRevision string
	Guards         []Guard
	Around         []Around
}
```

Slices, rather than maps, define deterministic registration order. `rig.Define`
validates and defensively clones the set. One immutable rig installs one set for
the sessions and native loops it constructs or restores.

```go
func rig.WithHooks(hooks hook.Set) rig.Option
```

There is no public registry mutation after `rig.Define`.

### 5.1 Guardable Operations

Only these operations may have guards:

```text
Turn
Inference
Compaction
ToolCall
```

`Step`, `GateWait`, `ToolExecution`, and `JournalAppend` are observation-only.
Validation rejects a guard registered for an observation-only operation.

### 5.2 Denials

A nil guard error means allow. Any non-nil error blocks the operation. Guard
failures therefore fail closed by default.

An intentional policy denial uses a typed error:

```go
type Denial struct {
	Code   string
	Reason string
}

func Deny(code, reason string) error
func AsDenial(err error) (*Denial, bool)
```

`Code` is a 1–64 byte ASCII machine identifier matching
`[a-z][a-z0-9_.-]*`. `Reason` is a 1–1024 byte, valid UTF-8, trim-nonblank
message with no Unicode control characters. An operation may expose a validated
reason to the model or caller.

`hook.AsDenial` is the only supported intentional-denial classifier. It follows
wrapped errors, revalidates the exported fields, and returns an independently
owned copy. Runtime code must not classify with a raw
`errors.As` check against `*hook.Denial`: direct construction remains possible
because the approved fields are exported, and a malformed direct construction
is an internal guard failure, not an intentional denial. `OutcomeDenied` is
selected only when `hook.AsDenial` succeeds.

Any other guard error is an internal hook failure: it still blocks, but its text
is redacted before logs, telemetry, or another trust boundary.

Applications that explicitly want best-effort policy can catch their own
classifier or service failure and return nil. Harness does not offer a global
fail-open switch.

## 6. Immutable Call and Result Snapshots

`Call` identifies one operation and contains exactly one matching typed payload.

```go
type Call struct {
	Operation   Operation
	StartedAt   time.Time
	Coordinates identity.Coordinates
	AgentName   identity.AgentName
	Cause       identity.Cause

	Turn          *TurnData
	Step          *StepData
	Inference     *InferenceData
	Compaction    *CompactionData
	ToolCall      *ToolCallData
	GateWait      *GateWaitData
	ToolExecution *ToolExecutionData
	JournalAppend *JournalAppendData
}

type Result struct {
	Call
	EndedAt time.Time
	Outcome Outcome
	Err     error
}
```

Validation requires the payload matching `Operation` and rejects every other
payload. Snapshots are read-only by contract and clone mutable request, message,
argument, result, and manifest data before exposure.

Operation payloads carry only data meaningful at that boundary:

- `TurnData`: turn index and selected input message;
- `StepData`: turn-local step index;
- `InferenceData`: exact provider request and terminal response metadata;
- `CompactionData`: attempt id, frozen input, and terminal output when present;
- `ToolCallData`: execution id, model tool-use id, name, immutable arguments,
  permission decision, and normalized result when present;
- `GateWaitData`: gate id, kind, resolver, blocked operation, and terminal gate
  decision;
- `ToolExecutionData`: resolved tool identity, execution id, and normalized
  execution result;
- `JournalAppendData`: record family and bounded record identity, never the raw
  serialized record.

`Result.Err` is the trusted in-process error. Logging and telemetry adapters must
redact it before export. A normalized model-visible tool error may still be an
operationally completed `ToolCall`; adapters inspect `ToolCallData` to classify
that domain outcome.

## 7. Dispatch Semantics

For one operation, the runner performs:

```text
Begin around hooks in registration order
    ↓ derived context
Run matching guards in registration order
    ↓ first error stops
Execute the operation and any nested operations
    ↓
Finish around hooks in reverse order
```

Every around hook whose `Begin` completes gets exactly one `Finish`, including
when:

- a guard denies;
- a guard fails;
- the operation fails;
- the context is canceled;
- a nested operation fails;
- the operation panics and the runtime converts the panic to an error.

Beginning around hooks before guards makes policy latency and denial outcomes
observable. Reverse finish order gives normal nested middleware semantics.

The context returned by one begin callback is passed to the next callback, the
guards, the operation, and its nested operations. The runner retains the prior
context's cancellation and deadline even if a callback returns a detached
context; callbacks should normally add values only. A nil returned context is
ignored and reported as an observation failure; it never replaces the valid
input context.

Around hooks cannot return an error. A panic in `Begin` or `Finish` is recovered,
logged, and ignored:

- a panicking `Begin` contributes no finish callback;
- a panicking `Finish` does not prevent remaining finishes;
- observation failure never changes the operation result.

A guard panic is recovered as an internal guard error and blocks the operation.

Callbacks may run concurrently across sessions, loops, parallel tool calls, and
journal appends. Harness preserves order only within one operation dispatch.
Callbacks must be concurrency-safe.

The operation owner must invoke the aggregate finish function on every terminal
path, including guard rejection. Finish is both the exactly-once notification
and cleanup for any cancellation links installed while chaining contexts.
Actor boundaries pass the derived operation context, rather than the actor's
long-lived base context, into durable appends so journal observation inherits
the active operation.

## 8. Operation Boundaries

### 8.1 Turn

Begins after an input is selected and the turn id is minted, but before
`TurnStarted` is committed. It finishes after the durable terminal event or an
infrastructure failure that prevents a terminal commit. A guard denial rejects
the selected input without publishing a false `TurnStarted`.

### 8.2 Step

Begins when the step id is minted and finishes after `StepDone` commits or the
in-flight step is discarded. It is not guardable.

### 8.3 Inference

Begins after the exact request is assembled and before
`inference.Client.Stream`. It finishes after clean stream EOF and response
assembly, or with the provider/stream failure. Every future retry is a distinct
operation under the same step.

### 8.4 Compaction

Begins after the candidate input and attempt id are frozen. It finishes after
commit, rejection, cancellation, or infrastructure failure. A guard denial uses
the existing compaction rejection path.

### 8.5 Tool Call

The semantic tool call begins with the model-supplied name and arguments before
resolution or permission evaluation. It contains the nested gate-wait and
tool-execution operations and finishes after a normalized result and normal
tool-call audit events exist. A denial becomes a bounded, model-visible policy
result without executing permission or tool code.

### 8.6 Gate Wait

Begins when execution actually waits on a gate and finishes on its terminal
decision or cancellation. Immediate policy decisions that do not wait may
finish with zero or near-zero duration.

### 8.7 Tool Execution

Begins only after resolution, argument preparation, and permission succeed,
immediately before invoking tool code. It finishes when the tool returns or a
panic is normalized. This boundary excludes gate latency.

### 8.8 Journal Append

Begins immediately before one checked append and finishes after acknowledgement
or failure. Its payload is bounded and secret-free. It does not expose raw event,
command, gate, or record bytes. Each append is wrapped once at the journal
boundary. Lifecycle construction and restore own their fresh opening fence;
operation owners retain ownership of their terminal append and finish only
after that append resolves. Restore replay reads historical records without
dispatching hooks.

## 9. Events and OpenTelemetry

Durable events remain the source of truth for committed occurrences. Hook
consumers must not infer that a durable fact exists merely because an operation
finished; they use the corresponding event.

The OpenTelemetry adapter uses:

- around hooks for nested spans, context propagation, durations, cancellations,
  denials, and failures;
- the existing event publication path for lifecycle counters and durable
  outcomes;
- stable ids from `identity.Coordinates` for attributes and links.

Observation-only around-hook changes are operational configuration and do not
participate in restore drift.

## 10. Configuration Fingerprint

Guard callbacks can change behavior. Around hooks cannot intentionally do so.
Only guard policy is therefore fingerprinted.

Add a secret-free field to `event.ConfigManifest`:

```go
HookPolicyRev string `json:"hook_policy_rev,omitzero"`
```

Rules:

- `PolicyRevision` must be non-empty when at least one guard is registered.
- `PolicyRevision` must be empty when there are no guards.
- callback pointers, names, and around registrations are never fingerprinted;
- changing guard behavior requires a new policy revision;
- hook-policy drift is assessed at `Warn` severity;
- adding the field requires a manifest schema-version and encoding-domain bump.

## 11. Validation

`rig.Define` rejects:

- unknown operations;
- nil guard or around functions;
- guards on observation-only operations;
- missing policy revision with guards;
- policy revision without guards;
- malformed or oversized denial codes and reasons where constructed by helpers.

Runtime dispatch validates internal `Call` construction during tests and debug
paths. Invalid internal calls fail before user callbacks run.

## 12. Testing Strategy

### Package tests

- defensive cloning and validation;
- operation/payload invariants;
- deterministic guard order and first-error short circuit;
- fail-closed guard errors and panics;
- begin order, derived-context chaining, and reverse finish order;
- exactly-once finish on every outcome;
- observation panic and nil-context isolation;
- concurrent dispatch under `go test -race`;
- bounded denial validation.

### Manifest tests

- schema-version and canonical-encoding golden coverage;
- guard revision changes fingerprints;
- around-only changes do not;
- drift classifies hook-policy changes as `Warn`;
- legacy manifest upgrade remains safe.

### Runtime integration tests

- turn denial creates no false turn start;
- inference denial never calls the provider;
- compaction denial uses the rejection path;
- tool denial never resolves permission or invokes tool code;
- step, inference, tool, gate, execution, and append scopes pair exactly once;
- tool execution duration excludes gate waiting;
- context values propagate through nested operations;
- durable event ordering remains unchanged;
- restored sessions use the rig's validated immutable hook set;
- foreign loops and hustles respect the stated exclusions.

### Repository verification

Run focused package tests during each task, then:

```bash
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
```

The hook API is complete when all pairing, ordering, failure, fingerprint, and
runtime-boundary tests pass without changing existing event semantics.

## 13. Deferred Work

- Shell/HTTP/user-configured hook adapters.
- Matcher syntax.
- Hook-specific UI reporting.
- OpenTelemetry exporter configuration and semantic-convention mapping.
- Foreign-backend internal hooks.
- Hustle operation hooks.
