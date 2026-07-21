# Design: Harness Execution Hooks

**Date:** 2026-07-08  
**Revised:** 2026-07-19
**Status:** Approved
**Scope:** Go API only. User-configured task hooks/YAML are explicitly deferred.

## 1. Goal

Add a typed, in-process hook API at meaningful harness extension boundaries so
embedders can observe execution and enforce policy without coupling themselves to
runtime internals.

Examples:

- record metrics when turns and lifecycle transitions complete;
- audit tool execution without parsing the event stream;
- attach tracing spans around inference, compaction, or tool operations;
- run cleanup logic when a loop or session stops;
- reject unsafe tool calls or inference requests at explicit interception hooks.

Hooks are not a replacement for events. Events remain the durable, externally
observable history. Hooks are in-process extension points that run near the code
performing the operation.

The API is semantic rather than mechanically symmetric. A lifecycle occurrence
such as `SessionStart` is one hook point. A fallible operation such as an inference
call has `BeforeInferenceCall`, `AfterInferenceCall`, and
`OnInferenceCallError` because those boundaries carry different data and control.
There is no `BeforeSessionStart`/`AfterSessionStart` pair and no public step-start
or step-end hook.

## 2. Non-Goals

- No YAML, JSON, shell command, or task-runner hook config in v1.
- No remote hook execution.
- No hook mutation of loop/session internals.
- No second event system.
- No hustle hooks. Hustles are separate, session-owned auxiliary operations and
  do not participate in loop inference, step, turn, or tool hook points.
- No hooks inside a foreign backend. Harness-owned session and loop lifecycle
  boundaries still fire for foreign loops, but turn, step, inference, and tool
  execution hooks are native-loop-only.
- No guarantee that hooks replay on restore. Restore may emit restore-specific
  hooks, but historical hooks are not re-run from the journal.
- No hook points added only for symmetry. Error hooks exist only where there is a
  meaningful fallible operation.
- No classifier-specific or OpenTelemetry-specific hook points. Those are
  consumers of the general policy and observation boundaries defined here.

## 3. Package Shape

Add a small package:

```go
package hook

type Point string

// StepIndex is the turn-local index of a native loop step.
type StepIndex uint64

type Func func(context.Context, Event) (context.Context, error)

type FailurePolicy uint8

const (
	FailureLog FailurePolicy = iota
	FailureAbort
)

type Set struct {
	FailurePolicy FailurePolicy
	Funcs         []Func
}

type Registry map[Point]Set
```

Functions return a context so a before-hook can attach tracing state, deadlines,
or request-scoped values to the wrapped operation. Functions within one set
receive the previous function's returned context. The runner uses the final
context for the operation and its matching after/error hook. A nil returned
context is invalid and is treated as a hook error. When a function returns an
error, its returned context is discarded: `FailureLog` continues with the input
context and `FailureAbort` stops. Returned contexts from occurrence, after, and
error hooks do not escape that hook point.

The rig is the current composition root. `rig.WithHooks` installs one registry
for sessions and loops created or restored by that immutable rig:

```go
func WithHooks(policyRevision string, h hook.Registry) Option
```

`rig.Define` validates and defensively clones the registry, including every
`Set.Funcs` slice. The lifecycle passes a concurrency-safe runner into each
session and native loop it constructs. The public `pkg/session` package remains
contracts-only; it does not regain the obsolete `session.Option` construction
surface. Loop definitions also remain immutable domain definitions and do not
carry runtime callbacks.

Validation rejects unknown points, unknown failure policies, nil functions, and
`FailureAbort` on occurrence, after, or error points where aborting has no coherent
meaning. The only intercepting points in v1 are `BeforeTurn`,
`BeforeInferenceCall`, `BeforeCompaction`, and `BeforeToolCall`.
Because an immutable rig may create multiple live sessions, callbacks can run
concurrently across sessions as well as across loops and parallel tool calls.
Hook implementations must therefore be concurrency-safe; the runner does not
serialize unrelated executions.

`policyRevision` is required and non-empty because `FailureAbort` hooks can
change execution behavior. The revision is stored as a dedicated, secret-free
hook policy field in `event.ConfigManifest` and participates in fingerprinting
and drift assessment at `Warn` severity. Adding the field requires a manifest
schema-version bump and canonical-encoding coverage. Callback pointers and
function names are never fingerprint material. A changed callback implementation
must ship with a changed revision; restore must not silently adopt changed
blocking policy.

```go
type ConfigManifest struct {
	// existing fields...
	HookPolicyRev string `json:"hook_policy_rev,omitzero"`
}
```

The registry does not flow into `internal/hustleruntime`, and a hustle's
`inference.Client.Invoke` never fires an inference hook. Session lifecycle and
quiescence hooks still reflect real session transitions regardless of what
caused them; there are no hustle-specific hook points.

## 4. Hook Event

`hook.Event` is an immutable execution snapshot. It gives hooks correlation IDs
and phase-specific detail without exposing mutable session or loop internals.

```go
type Event struct {
	Point    Point
	When     time.Time
	FailedAt Point // set only for generic On*Error hooks

	Coordinates identity.Coordinates
	AgentName   identity.AgentName
	Cause       identity.Cause

	Session   *SessionData
	Loop      *LoopData
	Turn      *TurnData
	Step      *StepData
	Inference *InferenceData
	Compaction *CompactionData
	Tool      *ToolData

	Err           error
	Recoverable   bool
	AbortedByHook bool
}
```

Rules:

- `Coordinates.SessionID` is set on every hook.
- `Coordinates.LoopID` is set for loop-scoped and lower hooks.
- `Coordinates.TurnID` is set for turn-scoped and lower hooks.
- `Coordinates.StepID` is set for step, inference, and tool hooks once a step id
  exists. Compaction may occur between steps and leaves it zero.
- `Err` on normal occurrence/after hooks is allowed only when the underlying
  domain result is itself an error outcome, for example `event.TurnFailed`.
  Infrastructure failures use `On*Error` hooks.
- Hooks must treat `Event` and nested data as read-only.
- Because some fields point at existing content/request/result values, the
  implementation should clone where practical. Hook functions must still treat
  pointed-to values as immutable by contract.

### 4a. SessionData

```go
type SessionData struct {
	ConfigManifest event.ConfigManifest
	Restored       bool
	WorkspaceRoot  string
}
```

Session data intentionally does not expose `*session.Session`, the loop map, hub,
gate directory, or appenders. `ConfigManifest`, including its `AppFields` map,
is defensively cloned.

### 4b. LoopData

```go
type LoopData struct {
	Parent          loop.Provenance
	ParentToolUseID string
	Engine          loop.Engine
	ForeignSID      string
	IsPrimer        bool
	IsActive        bool
}
```

`IsPrimer` reflects membership in the rig's primer set; `IsActive` reflects the
session's active-loop selection at snapshot time. The old single-primary model
is not part of the current topology.

### 4c. TurnData

```go
type TurnData struct {
	Index        event.TurnIndex
	InputMessage *content.UserMessage
	Terminal     event.Event
}
```

For `TurnEnd`, `Terminal` is one of `event.TurnDone`,
`event.TurnFailed`, or `event.TurnInterrupted`. A model/provider failure that
lands as `event.TurnFailed` is still a valid turn terminal, so
`TurnEnd` fires. `Err` is set to the terminal's underlying error for
convenience.

### 4d. StepData

```go
type StepData struct {
	Index        StepIndex
	ToolUseCount int
	Committed    bool
}
```

`StepCommit` means the actor has appended the completed step group and emitted
`StepDone`. It is an occurrence hook and cannot veto or roll back the commit.
That is distinct from `AfterInferenceCall`: an inference
call can complete successfully before output validation, tool execution, and
step commit. Interrupted or failed turns discard only the in-flight uncommitted
step.

### 4e. InferenceData

```go
type InferenceData struct {
	Request      *inference.Request
	AIMessage    *content.AIMessage
	StreamResult *stream.StreamResult
	StartedAt    time.Time
	EndedAt      time.Time
}
```

Inference hooks describe native loop streaming only. `Request` is the exact,
defensively cloned request passed to the provider. On `AfterInferenceCall`,
`AIMessage` is the assembled response, which may still fail later harness output
validation, and `StreamResult` carries an independently owned copy of terminal
provider metadata when the provider supplied it. `OnInferenceCallError` carries
the provider error in `Event.Err`; partial response blocks are not exposed as a
successful result.

### 4f. CompactionData

```go
type CompactionData struct {
	AttemptID event.CompactAttemptID
	Input     *loop.CompactionInput
	Output    *loop.CompactionOutput
	StartedAt time.Time
	EndedAt   time.Time
}
```

`BeforeCompaction` receives the frozen compaction input immediately before the
compactor is invoked. `AfterCompaction` receives an independently owned output
after it passes validation and the resulting `CompactionCommitted` is durable and
published. `OnCompactionError` carries execution, validation, or commit failures
in `Event.Err`. These hooks surround the harness-owned compaction operation; they
do not expose or instrument the hustle used to perform it.

### 4g. ToolData

```go
type ToolData struct {
	ToolExecutionID uuid.UUID
	ToolUseID       string
	ToolName        string
	Summary         string

	ArgsJSON string

	PermissionEffect loop.Effect
	PermissionReason string

	Result        *tool.ToolResult
	ResultPreview string
	IsError       bool

	StartedAt time.Time
	EndedAt   time.Time
}
```

`Summary` and `ResultPreview` are the safe logging fields. `ArgsJSON`,
`Result`, `Inference.Request`, `InputMessage`, and `Inference.AIMessage` may
contain secrets or user data, as may compaction input and output. The hook package
does not redact them because this is an in-process Go API, but docs and examples
must steer logging toward redacted fields.

## 5. Hook Points

Hook names describe what consumers can do:

- occurrence hooks (`SessionStart`, `TurnEnd`, `StepCommit`) report a committed
  lifecycle fact and cannot block it;
- `BeforeX` hooks run before a real operation and may block it;
- `AfterX` hooks receive the successful result of that operation;
- `OnXError` hooks receive an operation failure and cannot recover or replace it.

Do not manufacture `BeforeX`/`AfterX` pairs around lifecycle words such as
`Start`, `Idle`, or `Stop`.

### 5a. Session Lifecycle and Quiescence

```go
const (
	SessionStart   Point = "sessionStart"
	SessionActive  Point = "sessionActive"
	SessionIdle    Point = "sessionIdle"
	SessionStop    Point = "sessionStop"
	OnSessionError Point = "onSessionError"
)
```

Each occurrence hook fires after the matching durable event has been appended and
published live. `SessionStart` fires once for each usable runtime: after
`SessionStarted` for a fresh session, or after `RestoreDone` for a restored
session. It never replays the historical `SessionStarted` hook. If construction
interception is ever required, it must be designed separately as a rig-level
`SessionCreate` operation rather than disguised as `BeforeSessionStart`.

`SessionActive` and `SessionIdle` fire only on real quiescence edges, not on every
event while the session remains in that state. `SessionStop` fires after
`SessionStopped` commits. Hook failures at these occurrence points are logged and
never roll back the transition.

### 5b. Loop Lifecycle and Quiescence

```go
const (
	LoopStart   Point = "loopStart"
	LoopIdle    Point = "loopIdle"
	LoopStop    Point = "loopStop"
	OnLoopError Point = "onLoopError"
)
```

These are also post-commit occurrence hooks. `LoopStart` fires for a newly
committed `LoopStarted`; restoring an existing loop does not replay it. `LoopIdle`
fires after `LoopIdle` is durable and published when no chained turn starts. The
loop remains alive and can accept future input. A failure to construct a loop
cannot invoke `LoopStart`; a runtime failure associated with an established loop
invokes `OnLoopError` when no more specific operation error hook applies.

### 5c. Turn Hooks

```go
const (
	BeforeTurn  Point = "beforeTurn"
	TurnStart   Point = "turnStart"
	TurnFold    Point = "turnFold"
	TurnEnd     Point = "turnEnd"
	OnTurnError Point = "onTurnError"
)
```

`BeforeTurn` is the admission seam. It fires after a queued or submitted input is
selected to become a turn and a turn id is minted, but before the initial message
is committed and before `TurnStarted` is appended or published. `FailureAbort`
rejects that input without starting a turn. This is the semantic equivalent of a
user-prompt-submit hook; it is not half of a start pair.

`TurnStart`, `TurnFold`, and `TurnEnd` are post-commit occurrence hooks for
`TurnStarted`, `TurnFoldedInto`, and a terminal event respectively. `TurnEnd`
fires for `TurnDone`, `TurnFailed`, and `TurnInterrupted`; a model/provider failure
represented by `TurnFailed` is a completed terminal transition, not an
`OnTurnError`. `OnTurnError` is reserved for harness failures that prevent the
turn from reaching a durable terminal, and for `BeforeTurn` aborts.

The context returned by `BeforeTurn` becomes the turn execution context and is
passed to `TurnStart`, `TurnFold`, `TurnEnd`, or `OnTurnError`. This lets tracing
span the turn without adding mechanical before/after-start hooks.

### 5d. Step Commit

```go
const (
	StepCommit        Point = "stepCommit"
	OnStepCommitError Point = "onStepCommitError"
)
```

There is no public `StepStart` or `StepEnd`. A step is an internal grouping of an
inference result, possible tool results, and one atomic `StepDone` commit; its
useful external seams are the operations inside it. `StepCommit` fires only after
the actor appends and publishes `StepDone`. It cannot veto a durable commit.
`OnStepCommitError` fires when the commit handshake fails or is cancelled before
the acknowledgement.

### 5e. Inference Call

```go
const (
	BeforeInferenceCall  Point = "beforeInferenceCall"
	AfterInferenceCall   Point = "afterInferenceCall"
	OnInferenceCallError Point = "onInferenceCallError"
)
```

An inference call is one provider attempt, not a whole step or turn. If retries
are added later, every attempt gets its own before/after-or-error sequence under
the same step coordinates.

`BeforeInferenceCall` fires after the exact request is assembled and defensively
cloned, the step id is minted, and execution admission is acquired, immediately
before `inference.Client.Stream`. It runs before any future
`InferenceCallStarted` event is appended or published. A `FailureAbort` error
prevents the provider call and fires `OnInferenceCallError` with
`AbortedByHook: true`.

`AfterInferenceCall` fires exactly once after clean stream EOF, response assembly,
and capture of terminal stream metadata. It fires before output validation, tool
execution, or step commit. A response that later fails output validation is still
a successfully completed inference call.

`OnInferenceCallError` fires exactly once if the pre-hook aborts or opening or
consuming the provider stream fails. It does not fire for empty, malformed, or
policy-invalid model output after clean EOF; those are turn/step output failures.
No inference hook fires for a hustle's `Client.Invoke` call.

### 5f. Compaction

```go
const (
	BeforeCompaction  Point = "beforeCompaction"
	AfterCompaction   Point = "afterCompaction"
	OnCompactionError Point = "onCompactionError"
)
```

`BeforeCompaction` fires after a candidate is frozen and counted but before
`CompactionStarted` is committed and before the compactor is invoked. An abort is
recorded through the existing compaction rejection path. `AfterCompaction` fires
after successful execution, output validation, and durable publication of
`CompactionCommitted`; it is observational and cannot veto the commit.
`OnCompactionError` fires for pre-hook abort, compactor failure, invalid output,
or commit failure.

### 5g. Tool Call

```go
const (
	BeforeToolCall  Point = "beforeToolCall"
	AfterToolCall   Point = "afterToolCall"
	OnToolCallError Point = "onToolCallError"
)
```

One semantic tool-call boundary covers resolution, permission evaluation, and
execution. This matches what policy consumers care about and avoids exposing the
runner's internal phases as public API.

`BeforeToolCall` receives the model-supplied name and arguments before
`ToolCallStarted` is emitted, permission is checked, or tool code runs. A
`FailureAbort` prevents permission and execution. The runner converts the abort
into a model-visible error result and emits the normal started/completed audit
pair for the attempted call after the hook decision.

`AfterToolCall` fires after a model-visible result has been normalized and after
`ToolCallCompleted` is durable and published. It fires with `ToolData.IsError`
for unknown tools, invalid arguments, permission denial, `WriteTarget` failure,
tool-returned errors, and recovered panics. Those are tool outcomes, not
hook-pipeline failures.

`OnToolCallError` fires for a `BeforeToolCall` abort or an infrastructure failure
that prevents the runner from producing or committing a normalized result. A
pre-hook abort can still have a durable, model-visible rejection outcome without
becoming a successful hook sequence. The hook outcome is exclusive: after
`BeforeToolCall`, exactly one of `AfterToolCall` or `OnToolCallError` fires.

Batch scheduling is intentionally not a public hook point in v1. Each call has
stable coordinates and can execute concurrently; consumers that need batch
aggregation can correlate calls by step id.

## 6. Error Hook Semantics

Use this rule:

```text
X           = committed lifecycle occurrence; observation only.
BeforeX     = operation is about to run; the only potentially blocking phase.
AfterX      = operation produced a successful/normalized result.
OnXError    = operation failed before reaching AfterX.
OnScopeError = established scope failed where no specific operation hook applies.
```

The specific operation error hooks in v1 are:

```go
OnInferenceCallError
OnCompactionError
OnToolCallError
OnStepCommitError
```

`OnSessionError`, `OnLoopError`, and `OnTurnError` are fallbacks. They carry
`FailedAt` when a meaningful point is known:

```go
hook.Event{
	Point:    hook.OnTurnError,
	FailedAt: hook.BeforeTurn,
	Err:      err,
}
```

An error returned by an error hook is logged; it never recursively invokes
another error hook. Hook failures are not journal events in v1. Operational
failures continue through the existing durable domain event paths.

## 7. Ordering and Failure Policy

For a hook set, functions run in registration order. Separate points may run
concurrently across sessions, loops, and parallel tool calls.

Default behavior is `FailureLog`: log the hook error and continue execution. This
is the required policy for occurrence, after, and error hooks and the normal
policy for metrics, tracing, and audit hooks.

`FailureAbort` is valid only on `BeforeTurn`, `BeforeInferenceCall`,
`BeforeCompaction`, and `BeforeToolCall`:

```go
rig.WithHooks("policy-v1", hook.Registry{
	hook.BeforeToolCall: hook.Set{
		FailurePolicy: hook.FailureAbort,
		Funcs:         []hook.Func{enforceLocalPolicy},
	},
})
```

Abort rules:

- the wrapped operation does not start;
- the matching `AfterX` hook does not fire;
- the matching `OnXError`, or `OnTurnError` for `BeforeTurn`, fires with
  `AbortedByHook: true`;
- operation-specific code converts the abort into its existing public outcome
  (`TurnRejected`, compaction rejection, model-visible tool error, or turn
  failure) rather than inventing a second event protocol.

Hook execution must respect `ctx`. Long-running hooks must return when
`ctx.Done()` closes. A hook may call an external classifier, but it must use a
separate client or pipeline so it cannot recursively invoke the same Harness hook
path.

When a before-hook returns a derived context, the runner passes it to the wrapped
operation and then to the matching after/error point. This is how an
OpenTelemetry adapter starts a span before an operation, makes provider/tool
instrumentation its child, and ends the same span after success or failure.

## 8. Journal and Event Ordering

Events remain authoritative for persistence, replay, and out-of-process
observation. Hooks provide synchronous in-process control and rich snapshots; they
do not replace or alter the event stream.

| Hook kind | Journal/live-event ordering | May block? |
|---|---|---|
| Lifecycle occurrence (`SessionStart`, `TurnStart`, `StepCommit`) | After the matching event is durably appended and published; restored `SessionStart` follows `RestoreDone` | No |
| `BeforeTurn` | Before `TurnStarted` append and publish | Yes |
| `BeforeInferenceCall` | Before provider I/O and before any future inference-start event | Yes |
| `AfterInferenceCall` | After clean provider completion; before output validation and `StepDone` | No |
| `BeforeCompaction` | Before `CompactionStarted` and compactor invocation | Yes |
| `AfterCompaction` | After `CompactionCommitted` is durably appended and published | No |
| `BeforeToolCall` | Before `ToolCallStarted`, permission, and execution | Yes |
| `AfterToolCall` | After `ToolCallCompleted` append and publish | No |
| `OnXError` | After the failure is known; before or after a domain failure event as required by that operation | No |

The important invariant is that no hook intended to stop an operation runs after
a `*Started` event for that operation. Post-operation and lifecycle hooks cannot
undo state, journal records, provider calls, or tool side effects.

## 9. Integrations and Examples

### 9a. Safety and Prompt-Injection Classifiers

Classifiers are not a second Harness pipeline. A consumer may build a composite
classifier service internally, then invoke it from the hook that owns the relevant
decision:

- `BeforeToolCall` classifies command safety, arguments, target paths, and whether
  untrusted content is influencing a side effect;
- `BeforeInferenceCall` classifies the exact context about to leave the process,
  including newly admitted tool output, retrieved content, or shell output;
- `BeforeTurn` classifies input before a durable turn begins.

This keeps classifier implementation, caching, model choice, and policy versioning
outside Harness while giving it a synchronous enforcement seam. A classifier that
uses inference must use a separate client and must not recursively call the hooked
rig.

```go
hooks := hook.Registry{
	hook.BeforeToolCall: hook.Set{
		FailurePolicy: hook.FailureAbort,
		Funcs: []hook.Func{
			func(ctx context.Context, ev hook.Event) (context.Context, error) {
				if ev.Tool == nil {
					return ctx, nil
				}
				if err := classifier.CheckToolCall(ctx, ev.Tool.ToolName, ev.Tool.ArgsJSON); err != nil {
					return ctx, err
				}
				return ctx, nil
			},
		},
	},
}
```

### 9b. OpenTelemetry

OpenTelemetry is an adapter over hooks, not a separate set of integration points.
Before-hooks start spans and return the derived context; after/error hooks end and
annotate them. Lifecycle occurrence hooks add instantaneous events or metrics.
The durable event stream remains the source for offline reconstruction.

```go
func startInference(ctx context.Context, ev hook.Event) (context.Context, error) {
	ctx, _ = tracer.Start(ctx, "harness.inference")
	return ctx, nil
}

func endInference(ctx context.Context, ev hook.Event) (context.Context, error) {
	span := trace.SpanFromContext(ctx)
	if ev.Err != nil {
		span.RecordError(ev.Err)
	}
	span.End()
	return ctx, nil
}

hooks := hook.Registry{
	hook.BeforeInferenceCall:  {Funcs: []hook.Func{startInference}},
	hook.AfterInferenceCall:   {Funcs: []hook.Func{endInference}},
	hook.OnInferenceCallError: {Funcs: []hook.Func{endInference}},
}
```

The adapter should use OpenTelemetry semantic-convention attributes where they
exist and default to the redacted fields in `hook.Event`. Raw prompts, arguments,
and results are opt-in because they can contain secrets or user data.

### 9c. Tool Audit

```go
hooks := hook.Registry{
	hook.AfterToolCall: hook.Set{
		Funcs: []hook.Func{
			func(ctx context.Context, ev hook.Event) (context.Context, error) {
				if ev.Tool == nil {
					return ctx, nil
				}
				err := audit.Record(ctx, audit.ToolCall{
					SessionID: ev.Coordinates.SessionID,
					LoopID:    ev.Coordinates.LoopID,
					TurnID:    ev.Coordinates.TurnID,
					StepID:    ev.Coordinates.StepID,
					CallID:    ev.Tool.ToolExecutionID,
					Name:      ev.Tool.ToolName,
					Summary:   ev.Tool.Summary,
					IsError:   ev.Tool.IsError,
				})
				return ctx, err
			},
		},
	},
}
```

## 10. Implementation Notes

- Add a small internal runner helper in `pkg/hook` that invokes a registry point,
  chains returned contexts, applies failure policy, and returns a typed hook
  error.
- Add `rig.WithHooks` as a singleton definition option, include its stable policy
  revision in `event.ConfigManifest`, and thread its cloned registry through
  `internal/sessionruntime.Lifecycle`, the session runtime, native-loop
  `runtimeConfig`, `turnConfig`, `stepConfig`, the compaction coordinator, and
  `RunBatch` only where needed.
  Do not thread it through hustle definitions, the hustle runtime, or foreign
  backend internals.
- Keep hook invocation out of the durable event codec.
- Add no imports from `pkg/session` into `pkg/hook`; hook data is plain values
  and existing leaf package types.
- Avoid holding `loopsMu`, `gatesMu`, or actor-owned mutable state while running
  hooks. Build the `hook.Event` snapshot, release locks, then invoke hooks.
- Invoke lifecycle occurrence hooks only after the corresponding durable append
  and live publish succeed. Their latency may delay subsequent control flow but
  cannot change the committed outcome.
- For parallel tool execution, per-tool hooks run in the goroutine executing that
  tool. The hook runner must be safe for concurrent invocation.

## 11. Tests

Add focused tests:

- hook point constants are unique;
- registry validation rejects unknown points/policies, nil functions, and abort
  policy except on the four documented interception points;
- hooks run in registration order and a returned context reaches the next hook,
  the wrapped operation, and the matching after/error hook;
- a nil returned context follows the configured failure policy;
- `FailureLog` logs and continues;
- `FailureAbort` on a before-hook prevents the operation and fires the relevant
  error hook;
- after-hooks do not run when the wrapped operation fails before the boundary;
- lifecycle occurrence hooks fire after durable append and live publication, and
  their errors cannot roll back state;
- fresh `SessionStart` follows `SessionStarted`, restored `SessionStart` follows
  `RestoreDone`, and restore does not replay historical `LoopStart` hooks;
- `BeforeTurn` abort produces no `TurnStarted`, while `TurnStart` fires only after
  one commits;
- `TurnEnd` fires for `TurnDone`, `TurnFailed`, and `TurnInterrupted`;
- `OnStepCommitError` fires when the commit handshake is cancelled;
- no `StepCommit` fires before `StepDone` commits;
- `BeforeInferenceCall` receives the exact cloned request and full coordinates;
- `AfterInferenceCall` fires only after clean EOF and receives independently
  owned response and terminal-metadata snapshots;
- `OnInferenceCallError` fires for stream-open and stream-consumption failures,
  while clean-EOF output validation failures do not fire it;
- inference-hook `FailureAbort` prevents `Client.Stream` and releases execution
  admission;
- hustle `Client.Invoke` calls never fire inference hooks;
- `BeforeCompaction` abort prevents `CompactionStarted` and invokes the existing
  rejection path;
- `AfterCompaction` fires only after `CompactionCommitted`, while commit failure
  fires `OnCompactionError` instead;
- compaction hooks surround the harness compaction boundary but do not fire for
  the underlying hustle invocation;
- `BeforeToolCall` fires before `ToolCallStarted`, permission, and execution;
- a tool-hook abort becomes a model-visible error and preserves the normal tool
  audit pair;
- `AfterToolCall` receives redacted `Summary` and capped `ResultPreview` for both
  successful and normalized error outcomes;
- exactly one of `AfterToolCall` or `OnToolCallError` follows a non-aborting
  `BeforeToolCall`;
- hook registry use is race-clean across sessions, loops, and parallel tool calls;
- an OpenTelemetry test adapter proves that the span context returned by a before
  hook is visible to provider/tool instrumentation and the matching terminal hook;
- hook policy revision changes produce configuration drift and callback identity
  is absent from the manifest.

## 12. Future Work

Configured command hooks can be built later as an adapter that compiles YAML/JSON
declarations into `hook.Func` values. Consumers could then point a hooks file at
Bash scripts or other executables, but the core remains Go code and never executes
commands itself. The adapter must own command lookup, sandbox posture, environment
templating, stdin JSON, exit-code mapping, timeouts, output limits, and redaction.
It must preserve the same point names and blocking rules rather than creating a
parallel hook system.
