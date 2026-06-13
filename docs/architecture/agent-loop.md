# Loop Architecture

```
           ───────────────────────────────
Client ──▶ Commands (chan Command, cap 64) ┐
           ─────────────────────────────── │
                                           │
         listen goroutine                  │      turn runner goroutine
         ┌─────────────────────────────────▼┐     ┌──────────────────────────────────┐
         │  listen()                        │     │                                  │
         │                                  │     │  var messages                    │
         │  loopState{                      │     │    []llm.AgenticMessage          │
         │    turnIndex int                 │     │                                  │
         │    status    loopStatus          │     │  for turn := range turnQueue:    │
         │  }                               │     │                                  │
         │                                  │     │  1. internal <-                  │
         │  cmd.(type)                      │     │     turnRunnerStatusStarted      │
         │  ├── controlCommand              │     │                                  │
         │  │   └── handleCommand           │     │  2. messages = runTurn(...)      │
         │  │       Shutdown →              │     │     ├── emit TurnStarted         │
         │  │         status=ShuttingDown   │     │     ├── stream LLM  ─────────────│───▶ LLM
         │  │         close(turnQueue)      │     │     │   emit TokenDelta (×N)     │
         │  │                               │     │     └── emit TurnDone/Failed     │
         │  └── turnCommand                 │     │                                  │
         │      └── enqueueTurn             │     │  3. internal <-                  │
         │          StartTurn →             │     │     turnRunnerStatusStopped      │
         │            turnIndex++  ───┐     │     │                                  │
         │            → turnQueue     │     │     │  4. loop to next turn            │
         │                            │     │     │                                  │
         │  turnQueue (chan Turn,64)◀─┘     │     │  defer:                          │
         │  ┌────┬────┬────┬────┐           │     │    internal <-                   │
         │  │ t3 │ t2 │ t1 │    │───────────│────▶│    turnRunnerStatusExited        │
         │  └────┴────┴────┴────┘           │     │                                  │
         │                                  │     │                                  │
         └───────────────────▲──────────────┘     └──────────────────────────────────┘
                          │  │                                            │    │
                          │  │                                            │    │ 
                          │  │    ──────────────────────────────────────  │    │
                          │  └─── internal (chan internalMsg)       ◀─────┘    │
                          │         loopStatusRunning  ◀ StatusStarted         │
                          │         loopStatusIdle     ◀ StatusStopped         │
                          │         loopStatusExited   ◀ StatusExited          │
                          │       ──────────────────────────────────────       │
                          │                                                    │
           ───────────────▼────────────────────────────────────────────────────▼───────
Client ◀── Events (chan Event, cap 256)
           ───────────────────────────────────────────────────────────────────────────
         ```
