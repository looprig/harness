# Agent Loop Architecture

The loop is an actor: one goroutine owns mutable session state and all command
ordering. Each turn owns its own event stream, so callers never compete over a
shared `Events` channel and abandoned turns cannot leak stale events into the
next call.

```
Client / AgentSession
  |
  | StartTurn{
  |   Ctx,
  |   Input,
  |   Events chan<- Event,
  |   Abandoned <-chan struct{},
  |   Ack    chan<- error,
  | }
  |
  | Interrupt{Ack chan<- bool}
  | Shutdown{Ack chan<- error}
  v
──────────────────────────────────────────────────────────────────────────────
commands chan Command
──────────────────────────────────────────────────────────────────────────────
  |
  v
┌────────────────────────────────────────────────────────────────────────────┐
│ loop actor goroutine                                                       │
│                                                                            │
│ state                                                                      │
│   sessionID uuid.UUID                                                      │
│   turnIndex TurnIndex                                                      │
│   status: idle | running | shuttingDown                                    │
│   cancelTurn context.CancelFunc                                            │
│   turnEvents chan<- Event                                                  │
│   turnAbandoned <-chan struct{}                                            │
│   shutdownAcks []chan<- error                                              │
│   sinks []EventSink                                                        │
│                                                                            │
│ startup                                                                    │
│                                                                            │
│   -> publish SessionStarted{SessionID} to sinks                            │
│                                                                            │
│ command handling                                                           │
│                                                                            │
│   StartTurn                                                                │
│     invalid required field                                                 │
│       -> Ack <- *InvalidCommandError if Ack is non-nil                     │
│       -> close Events if non-nil                                           │
│                                                                            │
│     idle                                                                   │
│       -> status = running                                                  │
│       -> turnIndex++                                                       │
│       -> start turn runner with request-owned Events channel               │
│       -> Ack <- nil                                                        │
│                                                                            │
│     running                                                                │
│       -> Ack <- *TurnBusyError{Reason: TurnAlreadyRunning}                 │
│       -> close Events                                                      │
│                                                                            │
│     shuttingDown                                                           │
│       -> Ack <- *TurnBusyError{Reason: SessionShuttingDown}                │
│       -> close Events                                                      │
│                                                                            │
│   Interrupt                                                                │
│     invalid nil Ack                                                        │
│       -> log and continue                                                  │
│                                                                            │
│     running                                                                │
│       -> cancelTurn()                                                      │
│       -> Ack <- true                                                       │
│                                                                            │
│     otherwise                                                              │
│       -> Ack <- false                                                      │
│                                                                            │
│   Shutdown                                                                 │
│     invalid nil Ack                                                        │
│       -> log, continue shutdown path without adding a waiter                │
│                                                                            │
│     idle                                                                   │
│       -> Ack <- nil to all shutdown waiters                                │
│       -> return, closing Loop.Done                                         │
│                                                                            │
│     running                                                                │
│       -> cancelTurn()                                                      │
│       -> status = shuttingDown                                             │
│       -> wait for internal turn completion                                 │
│                                                                            │
│     shuttingDown                                                           │
│       -> remember Ack; wait on the already pending shutdown path            │
│                                                                            │
│ internal turn completion                                                   │
│   -> update history                                                        │
│   -> publish terminal event to sinks                                       │
│   -> send terminal to Events unless Abandoned/ctx done                     │
│   -> close Events                                                          │
│   -> status = idle, or Ack nil to all shutdown waiters and return           │
└────────────────────────────────────────────────────────────────────────────┘
  |
  | starts one runner for the accepted turn
  v
┌────────────────────────────────────────────────────────────────────────────┐
│ turn runner goroutine                                                      │
│                                                                            │
│ input                                                                      │
│   ctx = request ctx joined with actor cancellation                         │
│   messages = prior completed conversation                                  │
│   events = request-owned chan<- Event                                      │
│                                                                            │
│ flow                                                                       │
│   emit TurnStarted{TurnIndex}                                              │
│   client.Stream(ctx, llm.Request{Messages: messages + userMessage})        │
│       -> emit TokenDelta{TurnIndex, Chunk}                                 │
│       -> assemble assistant message                                        │
│                                                                            │
│ result                                                                     │
│   success      -> internal <- TurnDone{TurnIndex, Message}, updated msgs   │
│   LLM error    -> internal <- TurnFailed{TurnIndex, Err}, rolled-back msg  │
│   ctx cancel   -> internal <- TurnInterrupted{TurnIndex}, rolled back msg  │
│                                                                            │
│ cleanup                                                                    │
│   close provider stream                                                    │
└────────────────────────────────────────────────────────────────────────────┘
  |
  v
LLM
```

`EventSink` is the observability attachment point. The actor publishes
`EventEnvelope{SessionID, TurnIndex, Event}` to sinks for startup, non-terminal,
and terminal events. Sinks must return quickly; slow logging, tracing,
WebSocket, console, or journal adapters own their own queues.

## Why There Is No Turn Queue In V1

`AgentSession` is single-flight: only one `Invoke` or `Stream` may be active at
a time. A buffered turn queue would add shutdown, interrupt, cancellation, and
event-routing states that the public API does not need yet.

V1 rejects `StartTurn` while another turn is running. If FIFO multi-producer
turn submission becomes necessary later, add an actor-owned queue of full
`TurnRequest` values:

```go
type TurnRequest struct {
    Ctx       context.Context
    Input     []*content.Block
    Events    chan<- Event
    Abandoned <-chan struct{}
    Ack       chan<- error
}
```

The key rule is that queued turns must carry their own ownership channels. The
loop should not queue anonymous `StartTurn` commands that all report into one
global event stream.
