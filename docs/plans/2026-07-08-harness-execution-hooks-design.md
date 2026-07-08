# Design: Harness Execution Hooks

**Date:** 2026-07-08  
**Status:** Draft  
**Scope:** Go API only. User-configured task hooks/YAML are explicitly deferred.

## 1. Goal

Add a typed, in-process hook API for harness execution phases so embedders can run
custom code at well-defined session, loop, turn, step, and tool boundaries.

Examples:

- record metrics before and after each turn;
- audit tool execution without parsing the event stream;
- attach tracing spans around model streaming, step commit, or tool execution;
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
- No guarantee that hooks replay on restore. Restore may emit restore-specific
  hooks, but historical hooks are not re-run from the journal.
- No hook points added only for symmetry. Error hooks exist only where there is a
  meaningful fallible operation.

## 3. Package Shape

Add a small package:

```go
package hook

type Point string

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

`loop.Config` carries loop/turn/step/tool hooks:

```go
type Config struct {
	// existing fields...
	Hooks hook.Registry
}
```

`session.Option` wires session-level hooks:

```go
func WithHooks(h hook.Registry) Option
```

The session passes the relevant hook registry down when constructing loops. If a
future composition root wants separate session and loop hook sets, it can merge
them explicitly before construction.

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

	Session *SessionData
	Loop    *LoopData
	Turn    *TurnData
	Step    *StepData
	Tool    *ToolData

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
	ConfigFingerprint event.ConfigFingerprint
	Restored          bool
	WorkspaceRoot     string
}
```

Session data intentionally does not expose `*session.Session`, the loop map, hub,
gate directory, or appenders.

### 4b. LoopData

```go
type LoopData struct {
	Parent          loop.Provenance
	ParentToolUseID string
	Engine          loop.Engine
	ForeignSID      string
	IsPrimary       bool
}
```

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
	Index        loop.StepIndex
	Request      *inference.Request
	AIMessage    *content.AIMessage
	ToolUseCount int
	Committed    bool
}
```

`AfterStepStream` means the model response has been assembled into an
`AIMessage`. `AfterStepCommit` means the actor has appended the completed step
group and emitted `StepDone`. That distinction matters because interrupted or
failed turns discard only the in-flight uncommitted step.

### 4e. ToolData

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
`Result`, `Request`, `InputMessage`, and `AIMessage` may contain secrets or user
data. The hook package does not redact them because this is an in-process Go API,
but docs and examples must steer logging toward the redacted fields.

## 5. Hook Points

Use consistent naming:

- `Start` / `Stop` for lifecycle.
- `Active` / `Idle` for quiescence.
- `Start` / `End` for turn, step, and tool execution phases.
- Specific operation names for sub-phases such as `Stream`, `Commit`,
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

	BeforeStepStream Point = "beforeStepStream"
	AfterStepStream  Point = "afterStepStream"

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

### 5e. Tool Hooks

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

Specific error hooks are useful for construction, shutdown, commit, and tool
execution:

```go
OnSessionStartError
OnSessionStopError
OnLoopStartError
OnLoopStopError
OnTurnStartError
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
loop.Config{
	Hooks: hook.Registry{
		hook.BeforeToolExec: hook.Set{
			FailurePolicy: hook.FailureAbort,
			Funcs: []hook.Func{enforceLocalPolicy},
		},
	},
}
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
- Thread a hook runner through `session.Session`, `loop.Config`, `loopConfig`,
  `turnConfig`, and `RunBatch` only where needed.
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
- hooks run in registration order;
- `FailureLog` logs and continues;
- `FailureAbort` on a before-hook prevents the operation and fires the relevant
  error hook;
- after-hooks do not run when the wrapped operation fails before the boundary;
- `AfterTurnEnd` fires for `TurnDone`, `TurnFailed`, and `TurnInterrupted`;
- `OnStepCommitError` fires when the commit handshake is cancelled;
- `AfterToolExec` receives redacted `Summary` and capped `ResultPreview`;
- hook registry use is race-clean under parallel tool calls.

## 12. Future Work

Configured task hooks can be built later as an adapter that compiles YAML/JSON
task declarations into `hook.Func` values. That layer should own command
execution, sandbox posture, environment templating, stdin JSON, timeouts, and
redaction. It should not change the core hook API.
