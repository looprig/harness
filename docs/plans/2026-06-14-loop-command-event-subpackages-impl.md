# loop/command + loop/event Subpackage Extraction — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Extract `loop/command` and `loop/event` as sibling subpackages of `internal/agent/loop`, one file per command type with its ack error co-located.

**Architecture:** `loop/event` is the DAG base (no loop deps); `loop/command` imports `loop/event`; `loop` imports both. Pure reorganisation — no logic changes anywhere.

**Tech Stack:** Go, `go test -race`, `go build`

**Note on pre-existing failures:** `loop/turn.go` and `loop/loop.go` currently fail to build due to in-progress content sealed-interface work (unrelated). After Task 3, those same failures will persist in the same files — our changes do not introduce new ones. Verify new subpackages independently with `go build ./internal/agent/loop/event/...` and `go build ./internal/agent/loop/command/...`.

---

### Task 1: Create `loop/event` package

**Files:**
- Create: `internal/agent/loop/event/event.go`
- Create: `internal/agent/loop/event/turn.go`
- Create: `internal/agent/loop/event/errors.go`
- Create: `internal/agent/loop/event/sink.go`
- Create: `internal/agent/loop/event/errors_test.go`

**Step 1: Create the directory and `event.go`**

```go
// internal/agent/loop/event/event.go
package event

import "github.com/inventivepotter/urvi/internal/uuid"

type Event interface{ isEvent() }

// TurnIndex identifies a turn within one session.
type TurnIndex int

// SessionStarted is published to sinks when the actor starts.
type SessionStarted struct{ SessionID uuid.UUID }

func (SessionStarted) isEvent() {}
```

**Step 2: Create `turn.go`**

Copy event types verbatim from current `internal/agent/loop/event.go`, adjusting package name:

```go
// internal/agent/loop/event/turn.go
package event

import "github.com/inventivepotter/urvi/internal/content"

// TurnStarted is the first event written to StartTurn.Events.
type TurnStarted struct{ TurnIndex TurnIndex }

// TokenDelta is emitted for each streaming chunk from the LLM.
type TokenDelta struct {
	TurnIndex TurnIndex
	Chunk     content.Chunk
}

// TurnDone is the terminal success event. Message is the complete AI response.
type TurnDone struct {
	TurnIndex TurnIndex
	Message   *content.AIMessage
}

// TurnFailed is the terminal event for non-cancellation LLM/provider errors.
// On failure the user message is rolled back from history. Err carries the
// typed cause; callers may errors.As it to inspect and retry.
type TurnFailed struct {
	TurnIndex TurnIndex
	Err       error
}

// TurnInterrupted is the terminal event when the turn context is cancelled.
// The user message for the cancelled turn is rolled back from history.
type TurnInterrupted struct{ TurnIndex TurnIndex }

func (TurnStarted) isEvent()     {}
func (TokenDelta) isEvent()      {}
func (TurnDone) isEvent()        {}
func (TurnFailed) isEvent()      {}
func (TurnInterrupted) isEvent() {}
```

**Step 3: Create `errors.go`**

```go
// internal/agent/loop/event/errors.go
package event

// EmptyResponseError is the TurnFailed.Err cause when a provider returns a
// successful stream that contains no text or thinking content.
type EmptyResponseError struct{}

func (e *EmptyResponseError) Error() string { return "loop: empty response from provider" }

// TurnPanicError is the TurnFailed.Err cause when the turn goroutine panics.
// Detail is the recovered value rendered as a string.
type TurnPanicError struct{ Detail string }

func (e *TurnPanicError) Error() string {
	return "loop: panic in turn goroutine: " + e.Detail
}
```

**Step 4: Create `sink.go`**

```go
// internal/agent/loop/event/sink.go
package event

import (
	"context"

	"github.com/inventivepotter/urvi/internal/uuid"
)

// EventEnvelope tags an event with session and turn identity for observability.
// TurnIndex is zero for session-level events such as SessionStarted.
type EventEnvelope struct {
	SessionID uuid.UUID
	TurnIndex TurnIndex
	Event     Event
}

// EventSink receives best-effort copies of session events.
// Implementations must not block; slow or durable sinks own their own buffering.
// Implementations must be safe for concurrent calls.
// Sink failures must not affect turn execution.
// The context passed to OnEvent may already be cancelled during a hard loop kill.
// Implementations must not use it for I/O; use an independently-managed context instead.
type EventSink interface {
	OnEvent(context.Context, EventEnvelope)
}
```

**Step 5: Create `errors_test.go`**

```go
// internal/agent/loop/event/errors_test.go
package event_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
)

func TestEventErrorMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"empty response", &event.EmptyResponseError{}, "loop: empty response from provider"},
		{"turn panic", &event.TurnPanicError{Detail: "x"}, "loop: panic in turn goroutine: x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

**Step 6: Build and test**

```bash
go build ./internal/agent/loop/event/...
go test -race ./internal/agent/loop/event/...
```

Expected: both pass with no errors.

**Step 7: Commit**

```bash
git add internal/agent/loop/event/
git commit -m "feat(loop/event): extract event subpackage from loop"
```

---

### Task 2: Create `loop/command` package

**Files:**
- Create: `internal/agent/loop/command/command.go`
- Create: `internal/agent/loop/command/start_turn.go`
- Create: `internal/agent/loop/command/interrupt.go`
- Create: `internal/agent/loop/command/shutdown.go`
- Create: `internal/agent/loop/command/start_turn_test.go`
- Create: `internal/agent/loop/command/shutdown_test.go`

**Step 1: Create `command.go`**

```go
// internal/agent/loop/command/command.go
package command

// Command is a sealed interface for all loop commands.
// Only types in this package can implement it.
type Command interface{ isCommand() }
```

**Step 2: Create `start_turn.go`**

```go
// internal/agent/loop/command/start_turn.go
package command

import (
	"context"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
)

// StartTurn begins a new LLM turn. Ack receives nil on acceptance, a
// *TurnBusyError if a turn is already running or the loop is shutting down,
// or an *InvalidCommandError if a required field is nil.
// Events, Abandoned, and Ack are required and must be non-nil.
type StartTurn struct {
	Ctx       context.Context
	Input     []content.Block
	Events    chan<- event.Event
	Abandoned <-chan struct{}
	Ack       chan<- error
}

func (StartTurn) isCommand() {}

// Validate checks that all required fields are non-nil.
func (c StartTurn) Validate() error {
	switch {
	case c.Ctx == nil:
		return &InvalidCommandError{Command: CommandStartTurn, Field: StartTurnCtx}
	case c.Events == nil:
		return &InvalidCommandError{Command: CommandStartTurn, Field: StartTurnEvents}
	case c.Abandoned == nil:
		return &InvalidCommandError{Command: CommandStartTurn, Field: StartTurnAbandoned}
	case c.Ack == nil:
		return &InvalidCommandError{Command: CommandStartTurn, Field: StartTurnAck}
	default:
		return nil
	}
}

type TurnBusyReason string

const (
	TurnAlreadyRunning  TurnBusyReason = "turn already running"
	SessionShuttingDown TurnBusyReason = "session shutting down"
)

// TurnBusyError is returned on StartTurn.Ack when the loop cannot accept a turn.
type TurnBusyError struct{ Reason TurnBusyReason }

func (e *TurnBusyError) Error() string { return "loop: " + string(e.Reason) }

type CommandName string
type CommandField string

const (
	CommandStartTurn CommandName = "StartTurn"

	StartTurnCtx       CommandField = "Ctx"
	StartTurnEvents    CommandField = "Events"
	StartTurnAbandoned CommandField = "Abandoned"
	StartTurnAck       CommandField = "Ack"
)

// InvalidCommandError is returned when an internal caller violates a command contract.
type InvalidCommandError struct {
	Command CommandName
	Field   CommandField
}

func (e *InvalidCommandError) Error() string {
	return "loop: invalid command: " + string(e.Command) + "." + string(e.Field) + " is required"
}
```

**Step 3: Create `interrupt.go`**

```go
// internal/agent/loop/command/interrupt.go
package command

// Interrupt cancels the running turn. Ack receives true if a turn was cancelled,
// false if idle or the session is already shutting down.
// Ack is required and must be non-nil.
type Interrupt struct {
	Ack chan<- bool
}

func (Interrupt) isCommand() {}
```

**Step 4: Create `shutdown.go`**

```go
// internal/agent/loop/command/shutdown.go
package command

// Shutdown cancels the running turn (if any), delivers its terminal event, and
// exits the actor. Ack receives nil after clean exit, or *LoopTerminatedError
// if the loop's root context was cancelled before cleanup completed.
// Ack is required and must be non-nil.
type Shutdown struct {
	Ack chan<- error
}

func (Shutdown) isCommand() {}

// LoopTerminatedError is sent on Shutdown.Ack when the loop's root context was
// cancelled before the actor finished cleanup.
type LoopTerminatedError struct{ Cause error }

func (e *LoopTerminatedError) Error() string {
	return "loop: terminated by context: " + e.Cause.Error()
}
func (e *LoopTerminatedError) Unwrap() error { return e.Cause }
```

**Step 5: Create `start_turn_test.go`**

```go
// internal/agent/loop/command/start_turn_test.go
package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	ctx := context.TODO()
	ev := make(chan event.Event)
	ab := make(chan struct{})
	ack := make(chan error)
	tests := []struct {
		name      string
		cmd       command.StartTurn
		wantField command.CommandField
		wantErr   bool
	}{
		{"valid", command.StartTurn{Ctx: ctx, Events: ev, Abandoned: ab, Ack: ack}, "", false},
		{"nil ctx", command.StartTurn{Events: ev, Abandoned: ab, Ack: ack}, command.StartTurnCtx, true},
		{"nil events", command.StartTurn{Ctx: ctx, Abandoned: ab, Ack: ack}, command.StartTurnEvents, true},
		{"nil abandoned", command.StartTurn{Ctx: ctx, Events: ev, Ack: ack}, command.StartTurnAbandoned, true},
		{"nil ack", command.StartTurn{Ctx: ctx, Events: ev, Abandoned: ab}, command.StartTurnAck, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cmd.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var ice *command.InvalidCommandError
			if !errors.As(err, &ice) {
				t.Fatalf("err = %T, want *InvalidCommandError", err)
			}
			if ice.Field != tt.wantField {
				t.Errorf("Field = %q, want %q", ice.Field, tt.wantField)
			}
		})
	}
}

func TestCommandErrorMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"turn busy running", &command.TurnBusyError{Reason: command.TurnAlreadyRunning}, "loop: turn already running"},
		{"turn busy shutdown", &command.TurnBusyError{Reason: command.SessionShuttingDown}, "loop: session shutting down"},
		{"invalid command", &command.InvalidCommandError{Command: command.CommandStartTurn, Field: command.StartTurnAbandoned}, "loop: invalid command: StartTurn.Abandoned is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

**Step 6: Create `shutdown_test.go`**

```go
// internal/agent/loop/command/shutdown_test.go
package command_test

import (
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
)

func TestLoopTerminatedError(t *testing.T) {
	t.Parallel()
	cause := errors.New("context canceled")
	err := &command.LoopTerminatedError{Cause: cause}

	t.Run("message", func(t *testing.T) {
		t.Parallel()
		want := "loop: terminated by context: context canceled"
		if got := err.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})
	t.Run("unwrap", func(t *testing.T) {
		t.Parallel()
		if !errors.Is(err, cause) {
			t.Error("LoopTerminatedError does not unwrap to its Cause")
		}
	})
}
```

**Step 7: Build and test**

```bash
go build ./internal/agent/loop/command/...
go test -race ./internal/agent/loop/command/...
```

Expected: both pass with no errors.

**Step 8: Commit**

```bash
git add internal/agent/loop/command/
git commit -m "feat(loop/command): extract command subpackage from loop"
```

---

### Task 3: Migrate `loop` package to use subpackages

**Files:**
- Modify: `internal/agent/loop/loop.go`
- Modify: `internal/agent/loop/turn.go`
- Modify: `internal/agent/loop/config.go`
- Modify: `internal/agent/loop/errors.go`
- Delete: `internal/agent/loop/event.go`
- Delete: `internal/agent/loop/command.go`
- Delete: `internal/agent/loop/sink.go`

**Step 1: Update `loop.go`**

Add imports for `loop/command` and `loop/event`. Replace all unqualified references:

| Old | New |
|---|---|
| `Command` (channel/switch type) | `command.Command` |
| `StartTurn` | `command.StartTurn` |
| `Interrupt` | `command.Interrupt` |
| `Shutdown` | `command.Shutdown` |
| `validateStartTurn(c)` | `c.Validate()` |
| `TurnBusyError{Reason: TurnAlreadyRunning}` | `command.TurnBusyError{Reason: command.TurnAlreadyRunning}` |
| `TurnBusyError{Reason: SessionShuttingDown}` | `command.TurnBusyError{Reason: command.SessionShuttingDown}` |
| `Event` (channel/func type) | `event.Event` |
| `EventEnvelope{...}` | `event.EventEnvelope{...}` |
| `SessionStarted{...}` | `event.SessionStarted{...}` |
| `TurnStarted{...}` | `event.TurnStarted{...}` |
| `TokenDelta{...}` | `event.TokenDelta{...}` |
| `TurnDone{...}` | `event.TurnDone{...}` |
| `TurnFailed{...}` | `event.TurnFailed{...}` |
| `TurnInterrupted{...}` | `event.TurnInterrupted{...}` |
| `TurnIndex` (field type) | `event.TurnIndex` |

`loopState` fields change:
```go
type loopState struct {
    turnIndex     event.TurnIndex
    // ...
    turnEvents    chan<- event.Event
    // ...
}
```

`Loop.Commands` field:
```go
type Loop struct {
    Commands chan<- command.Command
    Done     <-chan struct{}
}
```

`listen` signature — `commands` parameter:
```go
func listen(ctx context.Context, sessionID uuid.UUID, cfg Config, commands <-chan command.Command, done chan struct{})
```

**Step 2: Update `turn.go`**

Add `loop/event` import. Replace all unqualified event references:

| Old | New |
|---|---|
| `TurnIndex` (parameter type) | `event.TurnIndex` |
| `emit func(Event)` | `emit func(event.Event)` |
| `(content.AgenticMessages, Event)` return | `(content.AgenticMessages, event.Event)` |
| `TurnStarted{...}` | `event.TurnStarted{...}` |
| `TokenDelta{...}` | `event.TokenDelta{...}` |
| `TurnInterrupted{...}` | `event.TurnInterrupted{...}` |
| `TurnFailed{...}` | `event.TurnFailed{...}` |
| `EmptyResponseError{}` | `event.EmptyResponseError{}` |
| `TurnDone{...}` | `event.TurnDone{...}` |

**Step 3: Update `config.go`**

Add `loop/event` import. Change `Sinks` field type:

```go
import "github.com/inventivepotter/urvi/internal/agent/loop/event"

type Config struct {
    Client       llm.LLM
    Model        llm.ModelSpec
    Sinks        []event.EventSink
    DrainTimeout time.Duration
}
```

**Step 4: Shrink `errors.go`**

Remove `TurnBusyError`, `TurnBusyReason`, `TurnAlreadyRunning`, `SessionShuttingDown`, `EmptyResponseError`, `TurnPanicError`, `CommandName`, `CommandField`, `CommandStartTurn`, `StartTurnCtx/Events/Abandoned/Ack`, `LoopTerminatedError`, `InvalidCommandError`. Keep only `ConfigError` and its `ConfigErrorKind` constants.

**Step 5: Delete the three now-empty source files**

```bash
git rm internal/agent/loop/event.go internal/agent/loop/command.go internal/agent/loop/sink.go
```

**Step 6: Build**

```bash
go build ./internal/agent/loop/
```

Expected: same pre-existing failures in `turn.go` and `loop.go` due to in-progress content work — no new errors introduced by this task.

**Step 7: Commit**

```bash
git add internal/agent/loop/loop.go internal/agent/loop/turn.go internal/agent/loop/config.go internal/agent/loop/errors.go
git commit -m "refactor(loop): migrate loop package to use loop/command and loop/event subpackages"
```

---

### Task 4: Update loop tests

**Files:**
- Modify: `internal/agent/loop/loop_test.go`
- Modify: `internal/agent/loop/turn_test.go`
- Modify: `internal/agent/loop/types_test.go` → rename to `errors_test.go`

**Step 1: Update `loop_test.go`**

Add imports:
```go
"github.com/inventivepotter/urvi/internal/agent/loop/command"
"github.com/inventivepotter/urvi/internal/agent/loop/event"
```

Replace all unqualified references (the file is `package loop` so these were previously unqualified):

| Old | New |
|---|---|
| `EventEnvelope` | `event.EventEnvelope` |
| `EventSink` (in `Config.Sinks`, `newLoop` param) | `event.EventSink` |
| `Event` (channel types, function params, `make(chan Event, ...)`) | `event.Event` |
| `TurnDone`, `TurnFailed`, `TurnInterrupted` | `event.TurnDone`, `event.TurnFailed`, `event.TurnInterrupted` |
| `TurnStarted`, `TokenDelta` | `event.TurnStarted`, `event.TokenDelta` |
| `SessionStarted` | `event.SessionStarted` |
| `TurnPanicError` | `event.TurnPanicError` |
| `StartTurn{...}` | `command.StartTurn{...}` |
| `Interrupt{...}` | `command.Interrupt{...}` |
| `Shutdown{...}` | `command.Shutdown{...}` |
| `TurnBusyError` | `command.TurnBusyError` |
| `TurnAlreadyRunning` | `command.TurnAlreadyRunning` |
| `InvalidCommandError` | `command.InvalidCommandError` |
| `StartTurnAbandoned` | `command.StartTurnAbandoned` |

`captureSink` and `panicSink` implement `event.EventSink` — update their `OnEvent` signature accordingly. `newLoop`'s variadic sinks parameter becomes `...event.EventSink`.

**Step 2: Update `turn_test.go`**

Add `loop/event` import. Replace:

| Old | New |
|---|---|
| `Event` (slice type, func param) | `event.Event` |
| `TurnDone`, `TurnFailed`, `TurnInterrupted`, `TurnStarted`, `TokenDelta` | `event.*` |
| `EmptyResponseError` | `event.EmptyResponseError` |

**Step 3: Shrink `types_test.go` → `errors_test.go`**

```bash
git mv internal/agent/loop/types_test.go internal/agent/loop/errors_test.go
```

Delete `TestValidateStartTurn` (moved to `loop/command/start_turn_test.go` in Task 2). Keep only:

```go
package loop

import (
    "errors"
    "testing"
)

func TestConfigError(t *testing.T) {
    t.Parallel()
    tests := []struct {
        name string
        err  error
        want string
    }{
        {"missing client", &ConfigError{Kind: ConfigMissingClient}, "loop: config error: Config.Client is required"},
        {"invalid model", &ConfigError{Kind: ConfigInvalidModel}, "loop: config error: Config.Model invalid"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            if got := tt.err.Error(); got != tt.want {
                t.Errorf("Error() = %q, want %q", got, tt.want)
            }
        })
    }
}

func TestConfigErrorUnwrap(t *testing.T) {
    t.Parallel()
    cause := errors.New("inner")
    err := &ConfigError{Kind: ConfigInvalidModel, Cause: cause}
    if !errors.Is(err, cause) {
        t.Error("ConfigError does not unwrap to its Cause")
    }
}
```

**Step 4: Run subpackage tests (pre-existing failures expected in `loop` itself)**

```bash
go test -race ./internal/agent/loop/event/...
go test -race ./internal/agent/loop/command/...
```

Expected: both pass.

**Step 5: Commit**

```bash
git add internal/agent/loop/loop_test.go internal/agent/loop/turn_test.go internal/agent/loop/errors_test.go
git commit -m "refactor(loop): update loop tests to use loop/command and loop/event"
```

---

### Task 5: Update `internal/agent/session`

**Files:**
- Modify: `internal/agent/session/agent.go`
- Modify: `internal/agent/session/agent_test.go`

**Step 1: Update `agent.go` imports and type references**

Add imports:
```go
"github.com/inventivepotter/urvi/internal/agent/loop/command"
"github.com/inventivepotter/urvi/internal/agent/loop/event"
```

Replace:

| Old | New |
|---|---|
| `loop.StartTurn{...}` | `command.StartTurn{...}` |
| `loop.Interrupt{Ack: ack}` | `command.Interrupt{Ack: ack}` |
| `loop.Shutdown{Ack: ack}` | `command.Shutdown{Ack: ack}` |
| `loop.Event` (type param, type switch) | `event.Event` |
| `loop.TurnDone`, `loop.TurnFailed`, `loop.TurnInterrupted` | `event.TurnDone`, `event.TurnFailed`, `event.TurnInterrupted` |
| `loop.LoopTerminatedError` | `command.LoopTerminatedError` |

Method signatures that change:
```go
func (s *AgentSession) Invoke(ctx context.Context, input []content.Block) (event.Event, error)
func (s *AgentSession) Stream(ctx context.Context, input []content.Block) (*llm.StreamReader[event.Event], error)
```

Keep `loop` import — still needed for `*loop.Loop` and `loop.Config` in `NewAgent`.

**Step 2: Update `agent_test.go` imports and type references**

Add:
```go
"github.com/inventivepotter/urvi/internal/agent/loop/command"
"github.com/inventivepotter/urvi/internal/agent/loop/event"
```

Replace:

| Old | New |
|---|---|
| `loop.TurnDone`, `loop.TurnInterrupted` | `event.TurnDone`, `event.TurnInterrupted` |
| `loop.TurnBusyError` | `command.TurnBusyError` |
| `loop.LoopTerminatedError` | `command.LoopTerminatedError` |

**Step 3: Build and test**

```bash
go test -race ./internal/agent/session/...
```

Expected: passes (session tests do not touch `turn.go` — pre-existing failures do not block this package).

**Step 4: Commit**

```bash
git add internal/agent/session/agent.go internal/agent/session/agent_test.go
git commit -m "refactor(session): update imports to loop/command and loop/event"
```

---

### Task 6: Update `agents/personal-assistant`

**Files:**
- Modify: `agents/personal-assistant/agent.go`
- Modify: `agents/personal-assistant/agent_test.go`

**Step 1: Update `agent.go`**

Add `loop/event` import. Update return types and comments:

```go
import "github.com/inventivepotter/urvi/internal/agent/loop/event"

// Send ... returning it unchanged as one of the value types event.TurnDone,
// event.TurnFailed, or event.TurnInterrupted ...
func (a *Assistant) Send(ctx context.Context, text string) (event.Event, error)

// Stream ... sr.Close() ... may briefly observe *command.TurnBusyError ...
func (a *Assistant) Stream(ctx context.Context, text string) (*llm.StreamReader[event.Event], error)
```

Keep `loop` import — still needed for `loop.Config` in `newWithClient`.

**Step 2: Update `agent_test.go`**

Add imports:
```go
"github.com/inventivepotter/urvi/internal/agent/loop/command"
"github.com/inventivepotter/urvi/internal/agent/loop/event"
```

Replace:

| Old | New |
|---|---|
| `loop.TurnDone` | `event.TurnDone` |
| `loop.TurnFailed` | `event.TurnFailed` |
| `loop.TurnStarted`, `loop.TokenDelta` | `event.TurnStarted`, `event.TokenDelta` |
| `loop.TurnInterrupted` | `event.TurnInterrupted` |
| `loop.TurnBusyError` | `command.TurnBusyError` |

Remove `loop` import from `agent_test.go` if no longer referenced there (check: `loop.Event` type is gone; remaining `loop.*` references may drop to zero).

**Step 3: Build and test**

```bash
go test -race ./agents/personal-assistant/...
```

Expected: passes.

**Step 4: Commit**

```bash
git add agents/personal-assistant/agent.go agents/personal-assistant/agent_test.go
git commit -m "refactor(personal-assistant): update imports to loop/event and loop/command"
```

---

### Task 7: Update TUI design doc and final verification

**Files:**
- Modify: `docs/plans/2026-06-13-tui-design.md`

**Step 1: Update type references in the TUI design doc**

In `docs/plans/2026-06-13-tui-design.md`, find all references to `loop.Event`, `loop.TurnDone`, `loop.TurnFailed`, `loop.TurnInterrupted`, `loop.TurnStarted`, `loop.TokenDelta`, `loop.TurnBusyError` and update to `event.Event`, `event.TurnDone`, etc. / `command.TurnBusyError`. The `Agent` interface's `StreamBlocks` return type changes from `*llm.StreamReader[loop.Event]` to `*llm.StreamReader[event.Event]`.

**Step 2: Verify subpackage tests still pass**

```bash
go test -race ./internal/agent/loop/event/...
go test -race ./internal/agent/loop/command/...
go test -race ./internal/agent/session/...
go test -race ./agents/personal-assistant/...
```

**Step 3: Commit**

```bash
git add docs/plans/2026-06-13-tui-design.md
git commit -m "docs(tui): update loop type references to loop/event and loop/command"
```
