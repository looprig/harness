# Agent Loop and Session Design

**Date:** 2026-06-13

---

## Goal

Define `internal/agent/loop`, `internal/session`, and `internal/uuid` — the execution engine and runtime identity layer for the Urvi agent platform.

Builds on the established foundation:
- `internal/content` — unified content vocabulary (`Block`, `Chunk`, `AgenticMessages`)
- `internal/llm` — provider-neutral inference interface (`LLM`, `ModelSpec`, `Request`, `StreamReader`)
- `internal/llm/auto` — provider factory (`New(ModelSpec) (LLM, error)`)

---

## Scope

- `internal/uuid` — typed UUID v4 using `crypto/rand`
- `internal/agent/loop` — execution engine: actor goroutine, request/reply commands, per-turn event streams, event sinks
- `internal/session/agent.go` — runtime identity: `AgentSession` with `Invoke`, `Stream`, `Interrupt`, `Shutdown`
- `internal/llm/auto` — new package: provider factory (additive change to `internal/llm`)
- `internal/llm/llm.go` — additive: `Provider` type + `Provider`, `BaseURL`, `APIKey` fields on `ModelSpec`

**Out of scope:** graph `WorkflowSession`, registry, client wiring, tools, journal/WAL, checkpoint/resume, `agents/coding` implementation.

---

## Package layout

```
internal/uuid/
  uuid.go
  uuid_test.go

internal/llm/
  llm.go           — ModelSpec gains Provider, BaseURL, APIKey fields
  auto/
    auto.go        — New(ModelSpec) (LLM, error) — provider factory

internal/agent/
  loop/
    loop.go        — Loop struct, New, listen actor goroutine, loopState
    config.go      — Config{Client llm.LLM, Model llm.ModelSpec}
    command.go     — request/reply command types
    event.go       — Event interface + all event types
    sink.go        — EventEnvelope, EventSink
    turn.go        — runTurn streaming assembler

internal/session/
  agent.go         — AgentSession, NewAgent, Invoke, Stream, Interrupt, Shutdown

agents/            — application layer (wiring point, not implemented here)
  coding/
    agent.go       — reads env vars, constructs loop.Config, defines system prompt
```

---

## `internal/uuid`

```go
package uuid

import (
    "crypto/rand"
    "fmt"
    "io"
)

type UUID [16]byte

// GenerateError wraps failures from the randomness source.
type GenerateError struct{ Cause error }

func (e *GenerateError) Error() string {
    if e.Cause == nil {
        return "uuid: generate"
    }
    return "uuid: generate: " + e.Cause.Error()
}
func (e *GenerateError) Unwrap() error { return e.Cause }

func New() (UUID, error) {
    var u UUID
    if _, err := io.ReadFull(rand.Reader, u[:]); err != nil {
        return UUID{}, &GenerateError{Cause: err}
    }
    u[6] = (u[6] & 0x0f) | 0x40 // version 4
    u[8] = (u[8] & 0x3f) | 0x80 // variant 10
    return u, nil
}

func (u UUID) String() string {
    return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
        u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
```

---

## `internal/llm` — additive changes

### `ModelSpec` additions

```go
type Provider string

const (
    ProviderLMStudio Provider = "lmstudio"
    ProviderPhala    Provider = "phala"
    ProviderChutes   Provider = "chutes"
)

type ModelSpec struct {
    Provider Provider
    BaseURL  string
    APIKey   string

    Model           string
    System          string
    Temperature     *float64
    TopP            *float64
    MaxTokens       *int
    Stop            []string
    ThinkingBudget  int
    ReasoningEffort ReasoningEffort
}
```

### `internal/llm/auto/auto.go`

```go
package auto

func New(spec llm.ModelSpec) (llm.LLM, error) {
    if err := spec.Validate(); err != nil {
        return nil, err
    }
    switch spec.Provider {
    case llm.ProviderLMStudio:
        return lmstudio.New(spec.BaseURL), nil
    case llm.ProviderPhala:
        return phala.New(spec.BaseURL, spec.APIKey), nil
    case llm.ProviderChutes:
        return chutes.New(spec.BaseURL, spec.APIKey), nil
    default:
        return nil, &llm.ValidationError{Field: "Provider", Reason: "unknown or empty"}
    }
}
```

---

## `internal/agent/loop`

### Architecture

One actor goroutine (`listen`) owns all session state. Every command is **request/reply**: the caller sends a command struct carrying reply channels; the actor responds synchronously before moving on. There is no shared public event channel and no internal turn queue.

Each `StartTurn` spawns a short-lived goroutine that streams non-terminal events (`TurnStarted`, `TokenDelta`) to the per-turn `Events` channel. The goroutine returns the terminal event and updated history to the actor via an internal channel. **The actor owns terminal delivery**: it updates state, sends the terminal event on `Events`, and closes `Events` — in that order — so callers never observe a terminal event before the actor has transitioned to idle.

Observability is separate from turn ownership. The same events are also published to optional `EventSink`s as tagged `EventEnvelope`s. Sinks are side effects for logging, tracing, live consoles, WebSocket fan-out, or journals; they must not participate in turn control flow and must not block the actor.

Three runtime contracts are required for this architecture:

- `Invoke` callers must not abandon the `events` channel mid-turn; the method always drains until terminal and signals `Abandoned` via `defer` so the actor always has an escape from `deliverAndClose`. `Stream` callers must either read until EOF or call `Close` which closes `Abandoned` and cancels the turn. A caller that violates this (leaked reader, panic before `Close`) no longer wedges the actor permanently: `deliverAndClose` also escapes on the loop's root context, so a root-ctx cancel always frees it.
- `llm.LLM` providers should respect context cancellation in `Stream`; loop shutdown and interruption rely on the provider unblocking promptly when the turn context is cancelled. A provider that ignores ctx no longer pins the actor on a hard kill — the `ctx.Done` drain is bounded by `cfg.DrainTimeout`, after which the goroutine is detached so `Loop.Done` can close. (The goroutine itself still leaks until the provider returns; that leak is unavoidable while the provider is hung.)
- `EventSink` implementations must return quickly. Durable or slow sinks own their own buffering, goroutines, retries, and backpressure policy.

State machine (three states):

```
idle
  StartTurn  → launch turn goroutine, store cancelTurn and turnEvents → running
  Interrupt  → Ack(false)
  Shutdown   → Ack(nil), return
  (other commands: rejected with TurnBusyError)

running
  StartTurn  → Ack(&TurnBusyError{Reason: TurnAlreadyRunning}), close Events
  Interrupt  → cancelTurn(), Ack(true)
  Shutdown   → cancelTurn(), status=shuttingDown; wait for internal result
  internal   → update msgs, deliver terminal, close turnEvents → idle

shuttingDown
  StartTurn  → Ack(&TurnBusyError{Reason: SessionShuttingDown}), close Events
  Interrupt  → Ack(false) (cancel already issued)
  Shutdown   → remember Ack; wait for same internal result
  internal   → update msgs, deliver terminal, close turnEvents, Ack(nil) → return

(ctx.Done in any state → cancelTurn if any, force-abandon turnEvents, wait for internal if running/shuttingDown but only up to cfg.DrainTimeout — on timeout detach the goroutine without closing turnEvents, Ack(&LoopTerminatedError{Cause: ctx.Err()}) for pending shutdowns, return)
```

### `config.go`

```go
package loop

import (
    "time"

    "github.com/inventivepotter/urvi/internal/llm"
)

type Config struct {
    Client       llm.LLM       // required — caller constructs via auto.New at composition root
    Model        llm.ModelSpec // model name, system prompt, sampling params — sent every turn
    Sinks        []EventSink   // optional side-effect sinks for observability/journaling
    DrainTimeout time.Duration // optional — bounds the hard-kill wait for a cancelled turn to drain; New defaults it to 5s
}
```

### `command.go`

```go
package loop

import (
    "context"
    "github.com/inventivepotter/urvi/internal/content"
)

type Command interface{ isCommand() }

// StartTurn begins a new LLM turn. Ack receives nil on acceptance, a
// *TurnBusyError if a turn is already running or the loop is shutting down, or
// an *InvalidCommandError if a required field is nil.
// Events receives non-terminal events then one terminal event; the actor
// closes it after the terminal event is sent.
// Ctx is the parent for the turn: cancelling it cancels the turn.
// Events, Abandoned, and Ack are required and must be non-nil.
type StartTurn struct {
    Ctx       context.Context
    Input     []*content.Block
    Events    chan<- Event
    Abandoned <-chan struct{} // required; closed when caller no longer reads Events
    Ack       chan<- error
}
func (StartTurn) isCommand() {}

// Interrupt cancels the running turn. Ack receives true if a turn was cancelled,
// false if idle or the session is already shutting down (cancel already issued).
// Ack is required and must be non-nil.
type Interrupt struct {
    Ack chan<- bool
}
func (Interrupt) isCommand() {}

// Shutdown cancels the running turn (if any), delivers its terminal event, and
// exits the actor. Ack receives nil after clean exit, or *LoopTerminatedError
// if the loop's root context was cancelled before cleanup completed.
// Ack is required and must be non-nil.
type Shutdown struct {
    Ack chan<- error
}
func (Shutdown) isCommand() {}
```

### `event.go`

```go
package loop

import (
    "github.com/inventivepotter/urvi/internal/content"
    "github.com/inventivepotter/urvi/internal/uuid"
)

type Event interface{ isEvent() }

// TurnIndex identifies a turn within one session.
type TurnIndex int

// SessionStarted is published to sinks when the actor starts.
type SessionStarted struct{ SessionID uuid.UUID }

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
// On failure the user message is rolled back from history, so the thread holds
// only completed user/assistant pairs. Err carries the typed cause (the
// provider error, *EmptyResponseError, or *TurnPanicError); callers may
// errors.As it to inspect the failure and retry by re-invoking the same input.
type TurnFailed struct {
    TurnIndex TurnIndex
    Err       error
}

// TurnInterrupted is the terminal event when the turn context is cancelled.
// The user message for the cancelled turn is rolled back from history.
type TurnInterrupted struct{ TurnIndex TurnIndex }

func (SessionStarted) isEvent()  {}
func (TurnStarted) isEvent()     {}
func (TokenDelta) isEvent()      {}
func (TurnDone) isEvent()        {}
func (TurnFailed) isEvent()      {}
func (TurnInterrupted) isEvent() {}
```

Rejection is a synchronous error on `StartTurn.Ack` — there is no `TurnRejected` event.
Non-terminal events (`TurnStarted`, `TokenDelta`) are delivered to the per-turn channel with backpressure, not dropped: if `Events` is full the turn goroutine blocks until the consumer reads, the caller closes `Abandoned`, or the turn context is cancelled. A slow `Stream` consumer therefore slows only its own turn's token production — never the actor — and the streamed deltas stay faithful to the assembled `TurnDone.Message`. (Sinks remain strictly best-effort and own their own buffering; backpressure applies only to the per-turn channel.) Terminal events are delivered by the actor after state is updated. The actor may skip sending the terminal event to `Events` when `StartTurn.Abandoned` is closed or the loop's root context is done, but it still publishes the terminal event to sinks and always closes `Events`.

### `sink.go`

```go
package loop

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
// The context passed to OnEvent may already be cancelled during a hard loop kill
// (root context cancellation). Implementations must not use it for I/O; use an
// independently-managed context instead.
type EventSink interface {
    OnEvent(context.Context, EventEnvelope)
}
```

`EventSink` is the extension point for logging, tracing, metrics, live console/WebSocket fan-out, and future journal/WAL persistence. A live console can implement a sink that writes to its own subscriber bus. A restore/replay system should persist events through a sink or journal, not by reading per-turn Go channels.

The loop calls sinks synchronously as a best-effort side effect. A sink that writes to disk, network, or a subscriber list must enqueue internally and return; it must never block the actor on I/O or slow consumers.

### Errors

```go
type ConfigErrorKind string

const (
    ConfigMissingClient ConfigErrorKind = "missing_client"
    ConfigInvalidModel  ConfigErrorKind = "invalid_model"
)

// ConfigError is returned by New when the supplied Config is invalid.
type ConfigError struct {
    Kind  ConfigErrorKind
    Cause error
}
func (e *ConfigError) Error() string {
    switch e.Kind {
    case ConfigMissingClient:
        return "loop: config error: Config.Client is required"
    case ConfigInvalidModel:
        return "loop: config error: Config.Model invalid"
    default:
        return "loop: config error"
    }
}
func (e *ConfigError) Unwrap() error { return e.Cause }

type TurnBusyReason string

const (
    TurnAlreadyRunning  TurnBusyReason = "turn already running"
    SessionShuttingDown TurnBusyReason = "session shutting down"
)

// TurnBusyError is returned on StartTurn.Ack when the loop cannot accept a turn.
type TurnBusyError struct{ Reason TurnBusyReason }
func (e *TurnBusyError) Error() string { return "loop: " + string(e.Reason) }

// EmptyResponseError is the TurnFailed.Err cause when a provider returns a
// successful stream that contains no text or thinking content.
type EmptyResponseError struct{}
func (e *EmptyResponseError) Error() string { return "loop: empty response from provider" }

// TurnPanicError is the TurnFailed.Err cause when the turn goroutine panics.
// Detail is the recovered value rendered as a string; the raw value is not
// retained so no untyped `any` escapes the recovery site.
type TurnPanicError struct{ Detail string }
func (e *TurnPanicError) Error() string { return "loop: panic in turn goroutine: " + e.Detail }

type CommandName string
type CommandField string

const (
    CommandStartTurn CommandName = "StartTurn"

    StartTurnCtx       CommandField = "Ctx"
    StartTurnEvents    CommandField = "Events"
    StartTurnAbandoned CommandField = "Abandoned"
    StartTurnAck       CommandField = "Ack"
)

// LoopTerminatedError is sent on Shutdown.Ack when the loop's root context was
// cancelled before the actor finished cleanup. It wraps the root context error
// so internal callers can errors.As to this type rather than receiving a raw
// context.Canceled or context.DeadlineExceeded.
type LoopTerminatedError struct{ Cause error }
func (e *LoopTerminatedError) Error() string {
    return "loop: terminated by context: " + e.Cause.Error()
}
func (e *LoopTerminatedError) Unwrap() error { return e.Cause }

// InvalidCommandError is returned when an internal caller violates a command contract.
type InvalidCommandError struct {
    Command CommandName
    Field   CommandField
}
func (e *InvalidCommandError) Error() string {
    return "loop: invalid command: " + string(e.Command) + "." + string(e.Field) + " is required"
}
```

### `loop.go`

```go
package loop

// Loop is the handle to a running agent loop for internal packages.
// Commands is unbuffered — sends block until the actor is ready. Callers must
// never close Commands; stop the actor with Shutdown. (Closing it would exit the
// actor through the `!ok` path, skipping terminal delivery and shutdown acks.)
// Done is closed when the actor has fully exited.
// Direct callers must honor the command contracts, including non-nil reply
// channels and non-nil Abandoned channels for StartTurn.
type Loop struct {
    Commands chan<- Command
    Done     <-chan struct{}
}

const defaultDrainTimeout = 5 * time.Second

func New(ctx context.Context, sessionID uuid.UUID, cfg Config) (*Loop, error) {
    if cfg.Client == nil {
        return nil, &ConfigError{Kind: ConfigMissingClient}
    }
    if err := cfg.Model.Validate(); err != nil {
        return nil, &ConfigError{Kind: ConfigInvalidModel, Cause: err}
    }
    if cfg.DrainTimeout <= 0 {
        cfg.DrainTimeout = defaultDrainTimeout
    }
    commands := make(chan Command)
    done     := make(chan struct{})
    go listen(ctx, sessionID, cfg, commands, done)
    return &Loop{Commands: commands, Done: done}, nil
}
```

**Actor-local state:**

```go
type loopStatus int
const (
    loopIdle loopStatus = iota
    loopRunning
    loopShuttingDown
)

type loopState struct {
    turnIndex     TurnIndex
    status        loopStatus
    cancelTurn    context.CancelFunc
    turnEvents    chan<- Event            // current turn's channel; actor closes it
    turnAbandoned <-chan struct{}         // always non-nil; closed when caller stops reading
    msgs          content.AgenticMessages // conversation history across turns
    shutdownAcks  []chan<- error
}
```

**Internal message from turn goroutine to actor:**

```go
type turnResult struct {
    msgs     content.AgenticMessages
    terminal Event // TurnDone, TurnFailed, or TurnInterrupted
}
```

**`listen`:**

```go
func listen(ctx context.Context, sessionID uuid.UUID, cfg Config, commands <-chan Command, done chan struct{}) {
    defer close(done)

    internal := make(chan turnResult, 1)
    var state loopState

    publish := func(ev Event) {
        env := EventEnvelope{SessionID: sessionID, Event: ev} // sessionID is uuid.UUID
        switch e := ev.(type) {
        case TurnStarted:
            env.TurnIndex = e.TurnIndex
        case TokenDelta:
            env.TurnIndex = e.TurnIndex
        case TurnDone:
            env.TurnIndex = e.TurnIndex
        case TurnFailed:
            env.TurnIndex = e.TurnIndex
        case TurnInterrupted:
            env.TurnIndex = e.TurnIndex
        }
        for _, sink := range cfg.Sinks {
            func() {
                defer func() {
                    if r := recover(); r != nil {
                        slog.Warn("event sink panicked", "panic", r)
                    }
                }()
                sink.OnEvent(ctx, env)
            }()
        }
    }

    publish(SessionStarted{SessionID: sessionID})

    // deliverAndClose publishes the terminal event, sends it to the per-turn
    // channel unless the caller abandoned the stream, and closes the channel.
    // Always called by the actor, never by the turn goroutine, and only after the
    // turn goroutine has sent its result on `internal` (so closing turnEvents can
    // never race a concurrent emit).
    //
    // Three escapes, so the actor can never wedge here:
    //   - turnEvents <- terminal: the normal path, caller reads the terminal.
    //   - turnAbandoned: Invoke closes it via defer after receiving the terminal;
    //     Stream.Close closes it explicitly.
    //   - ctx.Done: a buggy caller that never reads and never closes Abandoned
    //     (e.g. a leaked Stream reader) must not pin the actor forever. A root-ctx
    //     cancel always frees it. Without this case such a caller would wedge the
    //     actor outside its select loop, where neither Shutdown nor root-ctx
    //     cancel could reach it.
    deliverAndClose := func(terminal Event) {
        publish(terminal)
        select {
        case state.turnEvents <- terminal:
        case <-state.turnAbandoned: // caller abandoned; terminal already in sinks
        case <-ctx.Done(): // hard loop kill; terminal already in sinks
        }
        close(state.turnEvents)
        state.turnEvents = nil
        state.turnAbandoned = nil
    }

    forceAbandon := func() {
        abandoned := make(chan struct{})
        close(abandoned)
        state.turnAbandoned = abandoned
    }

    ackShutdowns := func(err error) {
        for _, ack := range state.shutdownAcks {
            ack <- err
        }
        state.shutdownAcks = nil
    }

    for {
        select {
        case cmd, ok := <-commands:
            if !ok {
                return
            }
            switch c := cmd.(type) {

            case StartTurn:
                if err := validateStartTurn(c); err != nil {
                    slog.Warn("invalid StartTurn command", "error", err)
                    if c.Ack != nil {
                        c.Ack <- err
                    }
                    if c.Events != nil {
                        close(c.Events)
                    }
                    continue
                }
                if state.status != loopIdle {
                    reason := TurnAlreadyRunning
                    if state.status == loopShuttingDown {
                        reason = SessionShuttingDown
                    }
                    c.Ack <- &TurnBusyError{Reason: reason}
                    close(c.Events)
                    continue
                }
                state.turnIndex++
                state.status = loopRunning
                state.turnEvents = c.Events
                state.turnAbandoned = c.Abandoned
                turnCtx, cancel := context.WithCancel(c.Ctx)
                state.cancelTurn = cancel
                idx, preMsgs := state.turnIndex, state.msgs
                go func() {
                    defer cancel()
                    defer func() {
                        if r := recover(); r != nil {
                            slog.Error("turn goroutine panicked", "panic", r)
                            // preMsgs excludes the user message (runTurn appends it
                            // internally), so a panic rolls back exactly like a
                            // normal failure: history holds only completed pairs.
                            internal <- turnResult{
                                msgs:     preMsgs,
                                terminal: TurnFailed{TurnIndex: idx, Err: &TurnPanicError{Detail: fmt.Sprintf("%v", r)}},
                            }
                        }
                    }()
                    // Non-terminal events apply backpressure rather than drop:
                    // a slow Stream consumer slows token production for its own
                    // turn (never the actor). Escapes on Abandoned (caller gone)
                    // and turnCtx.Done (interrupt/shutdown) keep emit from pinning
                    // the turn goroutine when the consumer stops reading.
                    emit := func(ev Event) {
                        publish(ev)
                        select {
                        case c.Events <- ev:
                        case <-c.Abandoned:
                        case <-turnCtx.Done():
                        }
                    }
                    updated, terminal := runTurn(turnCtx, c.Input, idx, preMsgs, cfg, cfg.Client, emit)
                    internal <- turnResult{msgs: updated, terminal: terminal}
                }()
                c.Ack <- nil

            case Interrupt:
                if c.Ack == nil {
                    slog.Warn("invalid interrupt command: Ack is required")
                    continue
                }
                if state.cancelTurn != nil {
                    state.cancelTurn()
                    state.cancelTurn = nil
                    c.Ack <- true
                } else {
                    c.Ack <- false
                }

            case Shutdown:
                if c.Ack == nil {
                    slog.Warn("invalid shutdown command: Ack is required")
                } else {
                    state.shutdownAcks = append(state.shutdownAcks, c.Ack)
                }
                if state.status == loopShuttingDown {
                    continue
                }
                wasRunning := state.status == loopRunning
                state.status = loopShuttingDown
                if state.cancelTurn != nil {
                    state.cancelTurn()
                    state.cancelTurn = nil
                }
                if !wasRunning {
                    ackShutdowns(nil)
                    return
                }
                // Turn goroutine is winding down; wait for internal below.
            }

        case result := <-internal:
            state.msgs = result.msgs
            state.cancelTurn = nil
            shuttingDown := state.status == loopShuttingDown
            if !shuttingDown {
                state.status = loopIdle
            }
            deliverAndClose(result.terminal)
            if shuttingDown {
                ackShutdowns(nil)
                return
            }

        case <-ctx.Done():
            if state.cancelTurn != nil {
                state.cancelTurn()
                state.cancelTurn = nil
            }
            if state.status == loopRunning || state.status == loopShuttingDown {
                // Hard loop kill. Wait for the cancelled turn goroutine to drain
                // and deliver its terminal, but bound the wait: a provider that
                // ignores ctx must not hold the actor (and Loop.Done) hostage.
                // forceAbandon lets deliverAndClose skip a caller that is already
                // gone; the timeout detaches a goroutine still blocked in the
                // provider. We do NOT close turnEvents on the timeout path — the
                // detached goroutine may still hold it and would panic on a send
                // to a closed channel; it is wedged in the provider and would
                // never have produced a terminal anyway.
                forceAbandon()
                select {
                case result := <-internal:
                    deliverAndClose(result.terminal)
                case <-time.After(cfg.DrainTimeout):
                    slog.Error("turn goroutine did not drain after ctx cancel; detaching",
                        "timeout", cfg.DrainTimeout)
                    state.turnEvents = nil
                    state.turnAbandoned = nil
                }
            }
            ackShutdowns(&LoopTerminatedError{Cause: ctx.Err()})
            return
        }
    }
}
```

```go
func validateStartTurn(c StartTurn) error {
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
```

### `turn.go`

`runTurn` emits non-terminal events (`TurnStarted`, `TokenDelta`) via `emit` and **returns** the terminal event. The actor is responsible for delivering it to the caller.

```go
package loop

// runTurn streams one LLM turn. Returns updated history and the terminal event.
// History only ever advances by complete user/assistant pairs: a successful turn
// appends both messages; any failure or cancellation rolls the user message back
// out. This keeps the thread free of trailing or doubled user messages that
// strict providers (alternating-role APIs) reject on the next turn. The caller
// still holds the original input and the TurnFailed.Err cause, and retries by
// re-invoking with the same input.
func runTurn(
    ctx       context.Context,
    input     []*content.Block,
    turnIndex TurnIndex,
    msgs      content.AgenticMessages,
    cfg       Config,
    client    llm.LLM,
    emit      func(Event),
) (content.AgenticMessages, Event) {
    userMsg := &content.UserMessage{
        Message: content.Message{Role: content.RoleUser, Blocks: input},
    }
    msgs = append(msgs, userMsg)
    emit(TurnStarted{TurnIndex: turnIndex})

    req := llm.Request{Model: cfg.Model, Messages: msgs}
    sr, err := client.Stream(ctx, req)
    if err != nil {
        if ctx.Err() != nil {
            return msgs[:len(msgs)-1], TurnInterrupted{TurnIndex: turnIndex}
        }
        return msgs[:len(msgs)-1], TurnFailed{TurnIndex: turnIndex, Err: err}
    }
    defer sr.Close()

    var textBuf, thinkBuf strings.Builder
    for {
        chunk, err := sr.Next()
        if errors.Is(err, io.EOF) {
            break
        }
        if err != nil {
            if ctx.Err() != nil {
                return msgs[:len(msgs)-1], TurnInterrupted{TurnIndex: turnIndex}
            }
            return msgs[:len(msgs)-1], TurnFailed{TurnIndex: turnIndex, Err: err}
        }
        emit(TokenDelta{TurnIndex: turnIndex, Chunk: chunk})
        switch chunk.Type {
        case content.ChunkTypeText:
            if chunk.Text != nil {
                textBuf.WriteString(chunk.Text.Text)
            }
        case content.ChunkTypeThinking:
            if chunk.Thinking != nil {
                thinkBuf.WriteString(chunk.Thinking.Thinking)
            }
        }
    }

    var blocks []*content.Block
    if thinkBuf.Len() > 0 {
        blocks = append(blocks, &content.Block{
            Type:     content.TypeThinking,
            Thinking: &content.ThinkingBlock{Thinking: thinkBuf.String()},
        })
    }
    if textBuf.Len() > 0 {
        blocks = append(blocks, &content.Block{
            Type: content.TypeText,
            Text: &content.TextBlock{Text: textBuf.String()},
        })
    }
    if len(blocks) == 0 {
        // Provider sent a successful stream with no content — treat as a failure
        // and roll the user message back out, so callers are left with neither an
        // empty assistant message nor a dangling user message in history.
        return msgs[:len(msgs)-1], TurnFailed{TurnIndex: turnIndex, Err: &EmptyResponseError{}}
    }
    aiMsg := &content.AIMessage{
        Message: content.Message{Role: content.RoleAssistant, Blocks: blocks},
    }
    return append(msgs, aiMsg), TurnDone{TurnIndex: turnIndex, Message: aiMsg}
}
```

---

## `internal/session/agent.go`

`AgentSession` wraps `Loop` with a UUID identity. No `active` guard — the loop rejects concurrent turns via `Ack`. Command sends select on both the caller context and `s.loop.Done`, so methods do not block forever when the caller times out or the actor exits.

```go
package session

type SessionErrorKind string

const (
    SessionIDGenerationFailed SessionErrorKind = "id_generation_failed"
    SessionLoopExited         SessionErrorKind = "loop_exited"
    SessionEventChannelClosed SessionErrorKind = "event_channel_closed"
    SessionContextDone        SessionErrorKind = "context_done"
)

// SessionError is returned when a session method cannot complete.
// Cause is non-nil when there is an underlying error to chain.
type SessionError struct {
    Kind  SessionErrorKind
    Cause error
}
func (e *SessionError) Error() string {
    var msg string
    switch e.Kind {
    case SessionIDGenerationFailed:
        msg = "session: id generation failed"
    case SessionLoopExited:
        msg = "session: loop exited"
    case SessionEventChannelClosed:
        msg = "session: event channel closed without terminal event"
    case SessionContextDone:
        msg = "session: context done"
    default:
        msg = "session: error"
    }
    if e.Cause == nil {
        return msg
    }
    return msg + ": " + e.Cause.Error()
}
func (e *SessionError) Unwrap() error { return e.Cause }

type AgentSession struct {
    SessionID uuid.UUID
    loop      *loop.Loop
}

// NewAgent constructs an AgentSession and starts its actor goroutine.
// The actor publishes SessionStarted to sinks before entering its command loop.
// Because Commands is an unbuffered channel, the first call to Invoke, Stream,
// Interrupt, or Shutdown is guaranteed to observe SessionStarted in sinks — the
// unbuffered send cannot complete until the actor is in its select loop, which
// is entered only after SessionStarted is published.
func NewAgent(ctx context.Context, cfg loop.Config) (*AgentSession, error) {
    select {
    case <-ctx.Done():
        return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
    default:
    }

    id, err := uuid.New()
    if err != nil {
        return nil, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
    }
    l, err := loop.New(ctx, id, cfg)
    if err != nil {
        return nil, err
    }
    return &AgentSession{SessionID: id, loop: l}, nil
}

// Invoke sends input and blocks until a terminal event.
// Cancelling ctx cancels the running turn; Invoke returns the TurnInterrupted event.
func (s *AgentSession) Invoke(ctx context.Context, input []*content.Block) (loop.Event, error) {
    events   := make(chan loop.Event, 64)
    ack      := make(chan error, 1)
    abandoned := make(chan struct{})
    defer close(abandoned) // ensures deliverAndClose always has an escape if Invoke exits early

    select {
    case s.loop.Commands <- loop.StartTurn{Ctx: ctx, Input: input, Events: events, Abandoned: abandoned, Ack: ack}:
    case <-ctx.Done():
        return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
    case <-s.loop.Done:
        return nil, &SessionError{Kind: SessionLoopExited}
    }

    if err := <-ack; err != nil {
        return nil, err
    }

    for ev := range events {
        switch ev.(type) {
        case loop.TurnDone, loop.TurnFailed, loop.TurnInterrupted:
            return ev, nil
        }
    }
    return nil, &SessionError{Kind: SessionEventChannelClosed}
}

// Stream sends input and returns a StreamReader[loop.Event] that yields
// TurnStarted, TokenDelta×N, then one terminal event, then EOF while the caller
// keeps reading. Calling sr.Close() abandons the event stream and cancels the turn.
// Callers must either read until EOF or call Close.
func (s *AgentSession) Stream(ctx context.Context, input []*content.Block) (*llm.StreamReader[loop.Event], error) {
    streamCtx, streamCancel := context.WithCancel(ctx)
    abandoned := make(chan struct{})
    var abandonOnce sync.Once
    events := make(chan loop.Event, 64)
    ack    := make(chan error, 1)

    select {
    case s.loop.Commands <- loop.StartTurn{
        Ctx:       streamCtx,
        Input:     input,
        Events:    events,
        Abandoned: abandoned,
        Ack:       ack,
    }:
    case <-ctx.Done():
        streamCancel()
        abandonOnce.Do(func() { close(abandoned) })
        return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
    case <-s.loop.Done:
        streamCancel()
        abandonOnce.Do(func() { close(abandoned) })
        return nil, &SessionError{Kind: SessionLoopExited}
    }

    if err := <-ack; err != nil {
        streamCancel()
        abandonOnce.Do(func() { close(abandoned) })
        return nil, err
    }

    return llm.NewStreamReader(
        func() (loop.Event, error) {
            ev, ok := <-events
            if !ok {
                return nil, io.EOF
            }
            return ev, nil
        },
        func() error {
            streamCancel()
            abandonOnce.Do(func() { close(abandoned) })
            return nil
        },
    ), nil
}

// Interrupt cancels the running turn. Returns true if a turn was cancelled.
// ctx allows the caller to time out the cancel attempt if the actor is slow.
func (s *AgentSession) Interrupt(ctx context.Context) (bool, error) {
    ack := make(chan bool, 1)
    select {
    case s.loop.Commands <- loop.Interrupt{Ack: ack}:
    case <-s.loop.Done:
        return false, &SessionError{Kind: SessionLoopExited}
    case <-ctx.Done():
        return false, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
    }

    select {
    case cancelled := <-ack:
        return cancelled, nil
    case <-s.loop.Done:
        return false, &SessionError{Kind: SessionLoopExited}
    case <-ctx.Done():
        return false, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
    }
}

// Shutdown cancels any running turn and blocks until the actor exits.
// Calling Shutdown after the actor has exited is a no-op.
func (s *AgentSession) Shutdown(ctx context.Context) error {
    ack := make(chan error, 1)
    select {
    case s.loop.Commands <- loop.Shutdown{Ack: ack}:
    case <-s.loop.Done:
        return nil
    case <-ctx.Done():
        return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
    }

    select {
    case err := <-ack:
        // err is non-nil when the loop's root context was cancelled before
        // the actor finished cleanup. Wrap it so callers always receive a
        // typed *SessionError rather than a raw context error.
        if err != nil {
            return &SessionError{Kind: SessionContextDone, Cause: err}
        }
        return nil
    case <-s.loop.Done:
        return nil
    case <-ctx.Done():
        return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
    }
}
```

---

## Application layer wiring (out of scope — reference only)

```go
// agents/coding/agent.go — composition root
func Config() (loop.Config, error) {
    spec := llm.ModelSpec{
        Provider: llm.Provider(os.Getenv("LLM_PROVIDER")),
        BaseURL:  os.Getenv("LLM_BASE_URL"),
        APIKey:   os.Getenv("LLM_API_KEY"),
        Model:    os.Getenv("LLM_MODEL"),
        System:   "You are a skilled software engineer...",
    }
    client, err := auto.New(spec)
    if err != nil {
        return loop.Config{}, err
    }
    return loop.Config{Client: client, Model: spec}, nil
}
```

---

## Tests

### `internal/llm/auto/auto_test.go`

| Case | Assert |
|---|---|
| unknown provider | returns `*llm.ValidationError` |
| empty provider | returns `*llm.ValidationError` |
| invalid ModelSpec (ThinkingBudget>0, Temperature≠1.0) | returns `*llm.ValidationError` before provider switch |
| `ProviderLMStudio` | returns non-nil `llm.LLM`, no error |
| `ProviderPhala` | returns non-nil `llm.LLM`, no error |
| `ProviderChutes` | returns non-nil `llm.LLM`, no error |

### `internal/uuid/uuid_test.go`

| Case | Assert |
|---|---|
| `New` non-zero | result != `[16]byte{}` |
| `String` RFC 4122 format | matches `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$` |
| two calls differ | `uuid1 != uuid2` |
| random source failure | returns `*uuid.GenerateError` |

### `internal/agent/loop/loop_test.go`

Fake `llm.LLM` injected via `Config.Client`. Each test creates its own events channel.

| Case | Assert |
|---|---|
| `New` missing client | returns `*ConfigError{Kind: ConfigMissingClient}` |
| `New` invalid model | calls `ModelSpec.Validate`; returns `*ConfigError{Kind: ConfigInvalidModel}` wrapping the validation error |
| startup event | sink observes `SessionStarted{SessionID}` when actor starts |
| single turn | events: `TurnStarted` → ≥1 `TokenDelta` → `TurnDone`; actor is idle after |
| two turns serial | second `StartTurn` accepted only after first terminal event received |
| start while running | second `StartTurn.Ack` receives `*TurnBusyError{Reason: TurnAlreadyRunning}` |
| start while shutting down | `StartTurn.Ack` receives `*TurnBusyError{Reason: SessionShuttingDown}` and events channel closes |
| invalid start missing abandoned | `StartTurn.Ack` receives `*InvalidCommandError{Field: StartTurnAbandoned}` and actor remains usable |
| interrupt mid-turn | `Interrupt.Ack` true; events: `TurnInterrupted`; actor idle after |
| interrupt idle | `Interrupt.Ack` false |
| interrupt missing ack | actor logs and remains usable; no deadlock |
| ctx cancel mid-turn | turn ctx cancelled; events: `TurnInterrupted`; actor idle after |
| shutdown idle | `Shutdown.Ack` receives nil; `Loop.Done` closes |
| shutdown mid-turn | turn cancelled; terminal event delivered; `Shutdown.Ack` receives nil |
| shutdown while already shutting down | second `Shutdown.Ack` receives nil after same terminal cleanup |
| shutdown missing ack | actor exits without blocking |
| shutdown with context-aware provider | fake LLM unblocks on ctx cancellation; loop exits promptly |
| loop ctx cancel while running | fake LLM unblocks on ctx cancellation; `Loop.Done` closes; no goroutine leak |
| loop ctx cancel while shuttingDown | Shutdown sent, then root ctx cancelled; turn goroutine finishes; `turnEvents` closes; `Loop.Done` closes; no goroutine leak |
| loop ctx cancel Shutdown.Ack receives LoopTerminatedError | direct Loop caller's Shutdown.Ack receives `*LoopTerminatedError` (not raw ctx error); `errors.As` succeeds |
| turn goroutine panic | actor receives panic result; delivers `TurnFailed` with `Err` an `*TurnPanicError` (`errors.As` succeeds); user message rolled back; transitions to idle |
| `TurnFailed.Err` is typed | provider error surfaces on `TurnFailed.Err`; `errors.As` to the provider's concrete error type succeeds (not a flattened string) |
| failed turn rolls back history | after `TurnFailed`, next accepted turn's request shows no trailing/doubled user message; history holds only completed pairs |
| send after exit | select on `Loop.Done` returns immediately; no deadlock |
| event sink receives envelopes | sink observes `SessionID` (uuid.UUID), `TurnIndex`, non-terminal and terminal events |
| event sink panic | sink panic is recovered; turn still completes |
| empty provider response | stream completes with no text/thinking chunks; actor delivers `TurnFailed{Err: *EmptyResponseError}`; user message rolled back; transitions to idle |
| slow Stream consumer keeps all deltas | consumer reads slowly; no `TokenDelta` dropped; assembled `TurnDone.Message` equals concatenated deltas |
| leaked reader + root-ctx cancel | caller never reads and never closes `Abandoned`; root ctx cancel makes `deliverAndClose` escape via `ctx.Done`; `Loop.Done` closes; no actor wedge |
| ctx-ignoring provider on hard kill | fake LLM ignores ctx; after `cfg.DrainTimeout` the actor detaches the turn goroutine; `Loop.Done` closes; actor not pinned |
| non-blocking sink contract | slow sink test implementation enqueues internally; loop does not wait on slow consumer |

### `internal/session/agent_test.go`

| Case | Assert |
|---|---|
| `NewAgent` non-zero `SessionID` | not zero `uuid.UUID` |
| `NewAgent` ctx cancelled | returns `*SessionError{Kind: SessionContextDone}` |
| `Invoke` returns `TurnDone` | terminal event, no error |
| `Invoke` ctx cancel returns `TurnInterrupted` | event returned, not Go error |
| `Stream` yields ordered events | `TurnStarted` → `TokenDelta`s → `TurnDone` |
| `Stream` `sr.Close()` cancels turn | stream is abandoned; sink observes `TurnInterrupted`; session usable again |
| `Stream` drain contract | reading until EOF releases the session; closing early releases the session |
| `Interrupt(ctx)` during `Invoke` | returns `(true, nil)`; `Invoke` returns `TurnInterrupted` |
| `Interrupt(ctx)` ctx cancelled before send | returns `(false, *SessionError{Kind: SessionContextDone})` |
| `Interrupt(ctx)` ctx times out after send, before ack | command reached actor; returns `(false, *SessionError{Kind: SessionContextDone})`; no deadlock |
| concurrent `Invoke` | second returns `*loop.TurnBusyError` |
| `Shutdown(ctx)` waits for exit | returns nil only after actor done |
| `Shutdown(ctx)` ctx cancelled before send | returns `*SessionError{Kind: SessionContextDone}` |
| `Shutdown(ctx)` loop root ctx cancelled during shutdown | ack receives `*LoopTerminatedError`; session wraps to `*SessionError{Kind: SessionContextDone}`; `errors.As` to `*loop.LoopTerminatedError` via Cause succeeds |
| `Shutdown(ctx)` after shutdown | returns nil immediately |
| methods after shutdown | return `*SessionError{Kind: SessionLoopExited}` without deadlock |

---

## Import layering

```
internal/uuid                     (no internal imports)
internal/content                  (no internal imports)
internal/llm                   →  internal/content
internal/llm/openaiapi         →  internal/llm, internal/content
internal/llm/openaiapi/*       →  internal/llm/openaiapi, internal/llm, internal/content
internal/llm/auto              →  internal/llm, internal/llm/openaiapi/*
internal/agent/loop            →  internal/uuid, internal/content, internal/llm
internal/session               →  internal/uuid, internal/agent/loop, internal/llm
agents/coding                  →  internal/llm, internal/llm/auto, internal/agent/loop
cmd/urvi                       →  agents/coding, internal/session
```

---

## Explicitly deferred

- **Multi-step tool loop.** A v1 "turn" is exactly one `client.Stream` call → one
  terminal event, and `runTurn` assembles only thinking + text blocks
  (`content.Chunk` has no tool-use variant yet). When tools land a turn becomes a
  loop of stream → tool_use → execute → tool_result → stream, which introduces new
  event types, intermediate assistant/tool messages, and a different terminal
  contract. **Treat `TurnDone`-after-one-stream as provisional**: the event
  vocabulary and the terminal-delivery contract will change. Tools / `runToolBatch`.
- **Conversation history management.** `state.msgs` grows unbounded across turns;
  a long session will eventually exceed the model's context window and fail every
  request. Summarization / truncation / token-budgeting hooks in at the actor's
  `state.msgs` assignment.
- Journal / WAL
- Checkpoint / resume
- Turn queuing (v1 rejects concurrent `StartTurn`; add when there is a real multi-producer use case)
- `agents/coding` implementation
- `console` binary
- Registry and `client.Client[I,O]`
