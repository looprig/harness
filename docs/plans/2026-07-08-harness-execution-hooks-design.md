# Design: Harness Execution Hooks

**Date:** 2026-07-08  
**Revised:** 2026-07-19
**Status:** Approved
**Scope:** Go API only. User-configured task hooks/YAML are explicitly deferred.

## 1. Goal

Add a typed, in-process hook API for harness execution phases so embedders can run
custom code at well-defined session, loop, turn, step, and tool boundaries.

Examples:

- record metrics before and after each turn;
- audit tool execution without parsing the event stream;
- attach tracing spans around loop inference calls, step commit, or tool execution;
- run cleanup logic when a loop or session stops;
- enforce local policy at a small number of before-hooks.

Hooks are not a replacement for events. Events remain the durable, externally
observable history. Hooks are in-process extension points that run near the code
performing the operation.

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

## 3. Package Shape

Add a small package:

```go
package hook

type Point string

// StepIndex is the turn-local index of a native loop step.
type StepIndex uint64

type Func func(context.Context, Event) error

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
`FailureAbort` on after/error points where aborting has no coherent meaning.
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
- `Coordinates.StepID` is set for step/tool hooks once a step id exists.
- `Err` on normal after-hooks is allowed only when the underlying domain result
  is itself an error outcome, for example `event.TurnFailed`. Infrastructure
  failures use `On*Error` hooks.
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

For `AfterTurnEnd`, `Terminal` is one of `event.TurnDone`,
`event.TurnFailed`, or `event.TurnInterrupted`. A model/provider failure that
lands as `event.TurnFailed` is still a valid turn terminal, so
`AfterTurnEnd` fires. `Err` is set to the terminal's underlying error for
convenience.

### 4d. StepData

```go
type StepData struct {
	Index        StepIndex
	ToolUseCount int
	Committed    bool
}
```

`AfterStepCommit` means the actor has appended the completed step group and
emitted `StepDone`. That is distinct from `AfterInferenceCall`: an inference
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

### 4f. ToolData

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
contain secrets or user data. The hook package does not redact them because this
is an in-process Go API, but docs and examples must steer logging toward the
redacted fields.

## 5. Hook Points

Use consistent naming:

- `Start` / `Stop` for lifecycle.
- `Active` / `Idle` for quiescence.
- `Start` / `End` for turn, step, and tool execution phases.
- Specific operation names for sub-phases such as `InferenceCall`, `Commit`,
  `PermissionCheck`, and `Exec`.

### 5a. Session Lifecycle and Quiescence

```go
const (
	BeforeSessionStart Point = "beforeSessionStart"
	AfterSessionStart  Point = "afterSessionStart"
	OnSessionStartError Point = "onSessionStartError"

	BeforeSessionActive Point = "beforeSessionActive"
	AfterSessionActive  Point = "afterSessionActive"

	BeforeSessionIdle Point = "beforeSessionIdle"
	AfterSessionIdle  Point = "afterSessionIdle"

	BeforeSessionStop Point = "beforeSessionStop"
	AfterSessionStop  Point = "afterSessionStop"
	OnSessionStopError Point = "onSessionStopError"

	OnSessionError Point = "onSessionError"
)
```

`Active` and `Idle` do not get dedicated error hooks. If publishing or applying a
quiescence transition fails, fire `OnSessionError` with `FailedAt` set to the
transition hook point.

### 5b. Loop Lifecycle and Quiescence

```go
const (
	BeforeLoopStart Point = "beforeLoopStart"
	AfterLoopStart  Point = "afterLoopStart"
	OnLoopStartError Point = "onLoopStartError"

	BeforeLoopIdle Point = "beforeLoopIdle"
	AfterLoopIdle  Point = "afterLoopIdle"

	BeforeLoopStop Point = "beforeLoopStop"
	AfterLoopStop  Point = "afterLoopStop"
	OnLoopStopError Point = "onLoopStopError"

	OnLoopError Point = "onLoopError"
)
```

Use `Stop`, not `End`, for loops. A loop stops or exits; a turn/step/tool
execution ends.

`BeforeLoopIdle` fires immediately before publishing `LoopIdle` after a terminal
turn if no chained turn starts. `AfterLoopIdle` fires after `LoopIdle` is
published. The loop is still alive and can accept future input.

### 5c. Turn Hooks

```go
const (
	BeforeTurnStart Point = "beforeTurnStart"
	AfterTurnStart  Point = "afterTurnStart"
	OnTurnStartError Point = "onTurnStartError"

	BeforeTurnEnd Point = "beforeTurnEnd"
	AfterTurnEnd  Point = "afterTurnEnd"

	BeforeTurnFold Point = "beforeTurnFold"
	AfterTurnFold  Point = "afterTurnFold"

	OnTurnError Point = "onTurnError"
)
```

Do not add `OnTurnEndError` in v1. If the turn reaches a terminal event, even
`TurnFailed`, the end operation succeeded and `AfterTurnEnd` fires. If the
harness fails while managing the turn, use `OnTurnError`.

### 5d. Step Hooks

```go
const (
	BeforeStepStart Point = "beforeStepStart"
	AfterStepStart  Point = "afterStepStart"

	BeforeStepCommit Point = "beforeStepCommit"
	AfterStepCommit  Point = "afterStepCommit"
	OnStepCommitError Point = "onStepCommitError"

	BeforeStepEnd Point = "beforeStepEnd"
	AfterStepEnd  Point = "afterStepEnd"

	OnStepError Point = "onStepError"
)
```

`OnStepCommitError` is specific because commit is a real fallible operation:
the turn goroutine sends a commit request and waits for the actor ack. If the
turn context is cancelled before the ack, `AfterStepCommit` must not fire.

### 5e. Inference Hooks

```go
const (
	BeforeInferenceCall  Point = "beforeInferenceCall"
	AfterInferenceCall   Point = "afterInferenceCall"
	OnInferenceCallError Point = "onInferenceCallError"
)
```

An inference call is one provider attempt, not a whole step or turn. In v1 each
native step has exactly one call. If retries are added later, every attempt gets
its own before/after-or-error sequence under the same step coordinates.

`BeforeInferenceCall` fires after the exact request is assembled and measured,
the step id is minted, and execution admission is acquired, immediately before
`inference.Client.Stream`. A `FailureAbort` error prevents the provider call and
fires `OnInferenceCallError` with `AbortedByHook: true`.

`AfterInferenceCall` fires exactly once after clean stream EOF, response assembly,
and capture of terminal stream metadata. It fires before harness output
validation, tool execution, or step commit. A response that later fails output
validation is therefore still a successfully completed inference call.

`OnInferenceCallError` fires exactly once if opening or consuming the provider
stream fails. It does not fire for empty, malformed, or policy-invalid model
output after clean EOF; those are step/output failures. Neither inference hook
fires for hustle `Client.Invoke` calls.

### 5f. Tool Hooks

```go
const (
	BeforeToolBatch Point = "beforeToolBatch"
	AfterToolBatch  Point = "afterToolBatch"

	BeforeToolResolve Point = "beforeToolResolve"
	AfterToolResolve  Point = "afterToolResolve"

	BeforeToolPermissionCheck Point = "beforeToolPermissionCheck"
	AfterToolPermissionCheck  Point = "afterToolPermissionCheck"

	BeforeToolExec Point = "beforeToolExec"
	AfterToolExec  Point = "afterToolExec"
	OnToolExecError Point = "onToolExecError"

	OnToolError Point = "onToolError"
)
```

Tool pre-execution failures such as unknown tool, invalid JSON, permission deny,
or `WriteTarget` error are model-visible tool results in the existing runner.
Those should still fire `AfterToolResolve` or `AfterToolPermissionCheck` with the
appropriate `ToolData`, but not `BeforeToolExec` because the tool is not executed.

`OnToolExecError` covers actual execution errors: `InvokableRun` returns a Go
error, the tool panics, or the execution middleware returns an error.

## 6. Error Hook Semantics

Use this rule:

```text
BeforeX     = operation is about to run.
AfterX      = operation reached that boundary successfully.
OnXError    = operation failed before reaching AfterX.
OnScopeError = generic error hook for a scope when no specific OnXError exists.
```

Specific error hooks are useful for construction, shutdown, inference, commit,
and tool execution:

```go
OnSessionStartError
OnSessionStopError
OnLoopStartError
OnLoopStopError
OnTurnStartError
OnInferenceCallError
OnStepCommitError
OnToolExecError
```

Generic hooks preserve precision with `FailedAt`:

```go
hook.Event{
	Point:    hook.OnLoopError,
	FailedAt: hook.BeforeLoopIdle,
	Err:      err,
}
```

Do not add artificial hooks such as `OnSessionActiveError` or
`OnLoopIdleError`. Active/idle are state transitions. If the transition's event
publish or quiescence application fails, use the generic scope error.

## 7. Ordering and Failure Policy

For a hook set, functions run in registration order.

Default behavior is `FailureLog`: log the hook error and continue execution.
This is the right default for metrics, tracing, and audit hooks.

`FailureAbort` lets a before-hook block the operation:

```go
rig.WithHooks("policy-v1", hook.Registry{
	hook.BeforeToolExec: hook.Set{
		FailurePolicy: hook.FailureAbort,
		Funcs:         []hook.Func{enforceLocalPolicy},
	},
})
```

Abort rules:

- Only before-hooks can abort the wrapped operation.
- After-hooks and error-hooks never change the already-computed result.
- If a before-hook aborts, the matching `AfterX` hook does not fire.
- The relevant `OnXError` or `OnScopeError` fires with `AbortedByHook: true`.

Hook execution must respect `ctx`. Long-running hooks should return when
`ctx.Done()` closes.

## 8. Events vs Hooks

Events remain authoritative for consumers and persistence. Hooks do not replace
or alter the event stream.

Examples:

- `AfterTurnStart` runs near the actor path that commits the initial user
  message and publishes `TurnStarted`.
- `AfterInferenceCall` runs after a native loop's provider stream reaches clean
  EOF and the response snapshot is assembled, before output validation and any
  durable step commit.
- `AfterStepCommit` runs after the actor appends the step group and publishes
  `StepDone`.
- `AfterToolExec` runs after `InvokableRun` and middleware complete and after
  the result is normalized into `ToolData`, but before the runner emits
  `ToolCallCompleted`.
- `AfterToolBatch` runs after every requested call in the batch has produced a
  model-visible result and every `ToolCallCompleted` for the batch has been
  emitted.

Prefer hook execution after the corresponding state mutation/event publish when
the hook name says `After*`, so a hook that observes the session sees the same
state implied by its name.

## 9. Examples

### 9a. Metrics

```go
hooks := hook.Registry{
	hook.AfterTurnEnd: hook.Set{
		Funcs: []hook.Func{
			func(ctx context.Context, ev hook.Event) error {
				turnsCompleted.Add(ctx, 1)
				if ev.Err != nil {
					turnFailures.Add(ctx, 1)
				}
				return nil
			},
		},
	},
}
```

### 9b. Tool Audit

```go
hooks := hook.Registry{
	hook.AfterToolExec: hook.Set{
		Funcs: []hook.Func{
			func(ctx context.Context, ev hook.Event) error {
				if ev.Tool == nil {
					return nil
				}
				return audit.Record(ctx, audit.ToolCall{
					SessionID: ev.Coordinates.SessionID,
					LoopID:    ev.Coordinates.LoopID,
					TurnID:    ev.Coordinates.TurnID,
					StepID:    ev.Coordinates.StepID,
					CallID:    ev.Tool.ToolExecutionID,
					Name:      ev.Tool.ToolName,
					Summary:   ev.Tool.Summary,
					IsError:   ev.Tool.IsError,
				})
			},
		},
	},
}
```

### 9c. Abort a Tool

```go
hooks := hook.Registry{
	hook.BeforeToolExec: hook.Set{
		FailurePolicy: hook.FailureAbort,
		Funcs: []hook.Func{
			func(ctx context.Context, ev hook.Event) error {
				if ev.Tool != nil && ev.Tool.ToolName == "Bash" {
					return errors.New("bash disabled by embedding application")
				}
				return nil
			},
		},
	},
}
```

The runner converts the abort into a model-visible tool-result error, consistent
with existing tool execution failures.

## 10. Implementation Notes

- Add a small internal runner helper in `pkg/hook` that invokes a registry point,
  applies failure policy, and returns a typed hook error.
- Add `rig.WithHooks` as a singleton definition option, include its stable policy
  revision in `event.ConfigManifest`, and thread its cloned registry through
  `internal/sessionruntime.Lifecycle`, the session runtime, native-loop
  `runtimeConfig`, `turnConfig`, `stepConfig`, and `RunBatch` only where needed.
  Do not thread it through hustle definitions, the hustle runtime, or foreign
  backend internals.
- Keep hook invocation out of the durable event codec.
- Add no imports from `pkg/session` into `pkg/hook`; hook data is plain values
  and existing leaf package types.
- Avoid holding `loopsMu`, `gatesMu`, or actor-owned mutable state while running
  hooks. Build the `hook.Event` snapshot, release locks, then invoke hooks.
- For parallel tool execution, per-tool hooks run in the goroutine executing that
  tool. The hook runner must be safe for concurrent invocation.

## 11. Tests

Add focused tests:

- hook point constants are unique;
- registry validation rejects unknown points/policies, nil functions, and abort
  policy on non-before points;
- hooks run in registration order;
- `FailureLog` logs and continues;
- `FailureAbort` on a before-hook prevents the operation and fires the relevant
  error hook;
- after-hooks do not run when the wrapped operation fails before the boundary;
- `AfterTurnEnd` fires for `TurnDone`, `TurnFailed`, and `TurnInterrupted`;
- `OnStepCommitError` fires when the commit handshake is cancelled;
- `BeforeInferenceCall` receives the exact cloned request and full coordinates;
- `AfterInferenceCall` fires only after clean EOF and receives independently
  owned response and terminal-metadata snapshots;
- `OnInferenceCallError` fires for stream-open and stream-consumption failures,
  while clean-EOF output validation failures do not fire it;
- inference-hook `FailureAbort` prevents `Client.Stream` and releases execution
  admission;
- hustle `Client.Invoke` calls never fire inference hooks;
- `AfterToolExec` receives redacted `Summary` and capped `ResultPreview`;
- hook registry use is race-clean across sessions, loops, and parallel tool calls;
- hook policy revision changes produce configuration drift and callback identity
  is absent from the manifest.

## 12. Future Work

Configured task hooks can be built later as an adapter that compiles YAML/JSON
task declarations into `hook.Func` values. That layer should own command
execution, sandbox posture, environment templating, stdin JSON, timeouts, and
redaction. It should not change the core hook API.
