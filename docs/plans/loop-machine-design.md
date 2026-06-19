# Loop Machine Design

**Date:** 2026-06-17
**Status:** Draft

## Motivation

The loop currently builds conversation history directly inside `runTurn` while
streaming chunks are also accumulated independently by the TUI for display. That
creates two risks:

1. the UI can show an assistant message that is not the same semantic message the
   loop stores; and
2. journal/session-restore work has no explicit state hierarchy to record.

This design establishes the loop machine hierarchy first:

```text
session -> loop -> turn -> step -> block -> chunk
```

The session owns shared identity and loop routing. The stateful machine levels
below it own `msgs` buffers: step messages fold into turn messages, and
committed turn/step messages fold into one loop's durable history at explicit
loop-goroutine commit points.

## Scope

In:

- explicit session/loop/turn/step/block/chunk hierarchy;
- `AgentSession` as the session-level owner of shared session identity and loop
  handles;
- session-level event fan-in exposed as a publish/subscribe contract
  (`eventPublisher`/`eventSubscriber` + `EventFilter`), not a raw channel;
- event delivery classes (`Ephemeral`/`Enduring`) declared on the sealed `Event`
  interface, with the self-heal contract that makes Ephemeral events droppable;
- a new Enduring `StepDone` event carrying the step's finalized group (`AIMessage`
  + its `ToolResultMessage`s);
- one submit command (`command.UserInput`) with a typed `Disposition`
  (`TurnStarted`/`InputQueued`/`TurnRejected`) — the loop, not the session, decides
  whether input starts a turn, waits in the loop queue, or later folds into a
  tool-continuation request — plus
  `command.CancelQueuedInput` and the `event.TurnFoldedInto`/`InputCancelled` pair
  for the queued-message lifecycle;
- `command.SubagentResult` (the subagent hand-back) routed through the same submit
  path, plus `LoopIdle`/`SessionActive`/`SessionIdle`/`SessionStopped` and the
  single `sessionState` quiescence model (with session-owned `expectTurn`/
  `cancelExpectTurn`/`stopSession` operations serialized by the hub lock);
- producer-identity `Header` fields (`SessionID`/`LoopID`/`TurnID`/`StepID`)
  stamped on every event by the producer (loop for loop events, session for
  session events);
- `Header.CausationID` stamped on command-caused events; for `UserInput`/
  `SubagentResult` resolution events it is the submit command id;
- `turnState`, `stepState`, and `blockState` state ownership;
- rename `content.ToolMessage` to `content.ToolResultMessage`;
- shared `internal/content/streamaccumulator` helpers for chunk-to-block folding;
- TUI/CLI display alignment with the semantic messages produced by the loop.

Out:

- journal format and restore mechanics;
- retiring the sink `EventEnvelope` and re-homing redaction
  (`Redactable`/`SinkProjection`) as a redacting subscriber — redaction is accepted
  as a follow-on concern and stays unchanged here, including the risk that new
  content-bearing events may reach existing sinks unredacted;
- `Header.ID`, the parent `Header` fields (`ParentLoopID`/`ParentTurnID`/
  `ParentStepID`), and replacing the transport-only envelope — motivated by this
  spec, sequenced after redaction has a new home;
- changing how system prompts are sourced;
- changing tool permission/gate semantics.

## Hierarchy

### Session

The session is the top-level user-facing handle. Today `AgentSession` owns one
loop. In a multi-agent session it owns one shared session id and multiple loop
handles.

```go
type AgentSession struct {
    // SessionID is shared by every loop participating in this session.
    SessionID uuid.UUID

    // sessionCtx is the shared lifetime root for the session. Every loop gets a
    // loopCtx derived from it; Shutdown cancels sessionCancel as the final backstop
    // after routing Shutdown commands to loops.
    sessionCtx    context.Context
    sessionCancel context.CancelFunc

    // loopsMu protects loops and primaryLoopID. There is no session goroutine,
    // so session methods serialize access to this map with a normal mutex.
    loopsMu sync.RWMutex

    // loops are the loop handles participating in this session, keyed by loop id.
    // Each entry pairs the loop handle with the provenance of whatever spawned it
    // (zero for the primary loop), so the session can rebuild the turn tree for
    // Interrupt. Today this map has one entry; multi-agent orchestration adds
    // subagent loops with a non-zero parent, without changing the lower
    // loop/turn/step hierarchy.
    loops map[uuid.UUID]*loopHandle

    // primaryLoopID is the default target for Invoke/Stream.
    // Interrupt cancels the active turn tree; Shutdown stops every loop.
    primaryLoopID uuid.UUID

    // hub is the session-level event fan-in. It is NOT a raw channel: a bare
    // channel would demand a drainer the session has no goroutine to provide and
    // would deadlock a headless run with no consumer. The hub owns the
    // subscriber registry and the per-subscriber delivery policy; loops publish
    // through eventPublisher and consumers attach through eventSubscriber.
    // Loops publish directly here. A parent or primary loop must not forward
    // child-loop events, because a parent turn's live event stream can close
    // while a child loop is still running. Parent/child identity is metadata,
    // not the event transport path.
    hub eventHub

    // idGen mints command ids. It defaults to uuid.New; kept as a field only so
    // tests can inject failure and prove the session never sends zero-id commands.
    idGen idGenerator
}

// eventHub is the session's fan-in. It implements both halves so loops see only
// the publish side and consumers see only the subscribe side. See the Event
// Fan-In section for the publish/subscribe contract, classes, and delivery
// policy.
type eventHub interface {
    eventPublisher
    eventSubscriber
}

// PublishEvent is the narrow dependency passed to loop.New. It owns the send
// policy into the session fan-in (class-aware, non-blocking). With no
// subscribers, delivery is a no-op, but publish-path sessionState transitions
// still run so headless WaitIdle/quiescence is correct. Loops cannot close the
// fan-in and do not need to know whether it is buffered, filtered, or shutting
// down.
type eventPublisher interface {
    PublishEvent(context.Context, event.Event) error
}

// loopHandle is the session's registry entry: the loop's channel handle plus the
// provenance of the turn/step that spawned it. parent is the zero value for the
// primary loop. loop.Provenance is defined in the Loop section; it is the same
// (LoopID, TurnID, StepID) tuple the child stamps onto Parent* on its events.
type loopHandle struct {
    loop   *loop.Loop
    parent loop.Provenance
    cancel context.CancelFunc // cancels this loop's loopCtx; session-owned backstop
}

// NewLoop creates another loop inside this session. The new loop shares
// SessionID but receives its own loop id and loop goroutine. parent is the
// provenance of the spawning turn/step (zero for the primary loop); the session
// records it in the registry for tree reconstruction and passes it to loop.New
// so the child can stamp Parent* on the events it emits.
//
// The session stores the loop handle and returns only the loop id, because
// callers should route through session methods instead of writing directly to
// a loop command channel.
func (s *AgentSession) NewLoop(parent loop.Provenance, cfg loop.Config) (uuid.UUID, error) {
    loopID, err := s.idGen()
    if err != nil {
        return uuid.UUID{}, &SessionError{Kind: SessionLoopIDGenerationFailed, Cause: err}
    }

    loopCtx, cancel := context.WithCancel(s.sessionCtx)
    l, err := loop.New(loopCtx, s.SessionID, loopID, parent, s, cfg)
    if err != nil {
        cancel()
        return uuid.UUID{}, err
    }

    s.loopsMu.Lock()
    defer s.loopsMu.Unlock()
    s.loops[loopID] = &loopHandle{loop: l, parent: parent, cancel: cancel}
    return loopID, nil
}

// loopFor returns the loop's channel handle for command routing. The registry
// stores *loopHandle, so it derefs to the handle's loop; the parent provenance is
// read only by the Interrupt tree walk, which reads s.loops directly.
func (s *AgentSession) loopFor(loopID uuid.UUID) (*loop.Loop, bool) {
    s.loopsMu.RLock()
    defer s.loopsMu.RUnlock()
    h, ok := s.loops[loopID]
    if !ok {
        return nil, false
    }
    return h.loop, true
}
```

Current `NewAgent(ctx, cfg)` behavior stays the same from the caller's
perspective: derive `sessionCtx` from the construction context, mint one
`SessionID`, initialize session event fan-in, call
`s.NewLoop(loop.Provenance{}, cfg)` (zero parent) to create the primary loop with
a `loopCtx` derived from `sessionCtx`, store the returned loop id as
`primaryLoopID`, and use `idGen` to stamp command ids.

Session responsibilities:

- mint and own the shared session id;
- own `sessionCtx`/`sessionCancel`, the shared lifetime root for all loops;
- create loop handles with distinct loop ids under the same session id;
- protect the mutable loop registry with `loopsMu`, recording each loop's parent
  provenance (`loop.Provenance`) so it can rebuild the turn tree for Interrupt;
- mint command ids before sending commands to loops;
- expose user-facing methods such as `Invoke`, `Stream`, `Interrupt`, and
  `Shutdown`;
- route new user turns to `primaryLoopID` by default;
- optionally route explicit loop-targeted turns through future
  `InvokeLoop`/`StreamLoop` APIs;
- route permission/user-input answers to the correct loop in a multi-loop
  session;
- own the session-level event fan-in for all loops.

The session does not own loop conversation history. Each loop owns its committed
`content.AgenticMessages`; the session is the routing and composition boundary
above loops.

There is no session goroutine in this design. Session methods own command
routing and shared handles, but they do not need a long-running state-owner
goroutine unless future subscription/routing state requires one.

### Loop

The loop is session-initiated. It owns committed conversation history for one
agent loop:

```go
type eventPublisher interface {
    PublishEvent(context.Context, event.Event) error
}

// Loop is the public handle returned by New. It is not the loop machine state.
type Loop struct {
    Commands chan<- command.Command
    Done     <-chan struct{}

    // gateReg is the loop goroutine's gate-registration channel. It stays
    // unexported and is used only by in-package turn/tool paths and tests.
    gateReg chan<- gateRegistration
}

type loopConfig struct {
    loopCtx context.Context
    cfg     Config

    commands <-chan command.Command
    gateReg  chan gateRegistration
    internal chan turnResult
    done     chan struct{}

    // events publishes to the session-level event fan-in.
    //
    // The loop depends on the publisher interface instead of a raw channel so
    // only AgentSession owns buffering, shutdown, close, and sequence policy.
    // Parent or primary loops do not forward child events.
    events eventPublisher
}

// Provenance identifies the parent turn/step that spawned a loop. The zero value
// means "no parent" (the primary loop). It is the (LoopID, TurnID, StepID) tuple
// the loop stamps onto the Parent* fields of every event it emits. It lives in
// the loop package because both loopState and AgentSession's registry use it.
type Provenance struct {
    LoopID uuid.UUID // parent loop; zero for the primary loop
    TurnID uuid.UUID // the parent turn that spawned this loop
    StepID uuid.UUID // the parent step (optional finer grain)
}

type loopState struct {
    // id is the loop id. In multi-agent sessions, each subagent loop gets its
    // own loop id.
    id uuid.UUID

    // sessionID is shared by every loop participating in the same session.
    sessionID uuid.UUID

    // parent is the provenance of whatever spawned this loop (zero for the
    // primary loop). The loop knows its PARENT so it can stamp Parent* on the
    // events it emits; it never tracks its CHILDREN. The session owns the loop
    // registry and the turn tree, so child lifecycle and tree-Interrupt stay a
    // session concern (SRP): a loop goroutine mutates only its own state.
    parent Provenance

    // msgs is the committed session conversation:
    // optional SystemMessage values plus committed user messages and completed
    // step groups. The loop goroutine commits:
    //   - the initial UserMessage when it emits TurnStarted,
    //   - each folded UserMessage when it emits TurnFoldedInto, and
    //   - each completed step group when it emits StepDone.
    // A failed/interrupted turn discards only its in-flight step; queued input that
    // never started/folded was never committed.
    msgs content.AgenticMessages

    // inbox is the loop pending-input queue for accepted UserInput/SubagentResult,
    // actor-owned: only runLoop appends/removes/clears it — no locks. When idle the
    // actor pops one entry to start the next turn. While a turn runs, runTurn drains
    // it only at tool-continuation boundaries via turnConfig.drainPending (never
    // touching it directly), so queued input can ride along with a mandatory
    // tool-result follow-up request but cannot extend a turn after the model has
    // produced a final no-tool answer. Bounded by inboxCap; a full inbox rejects
    // with TurnRejected{QueueFull} (a length check, never a blocking send).
    // Loop-level (not per-turn) so input arriving between/before turns is queued
    // and cancellable. On going idle the actor re-checks inbox and starts a new turn
    // from the first queued entry if it is non-empty.
    inbox []queuedInput

    activeTurn *turn
    status     loopStatus

    shutdownAcks []chan<- error
}

func newLoopState(sessionID uuid.UUID, loopID uuid.UUID, parent Provenance) loopState {
    return loopState{
        id:        loopID,
        sessionID: sessionID,
        parent:    parent,
    }
}

// runLoop is the loop goroutine started by New. It is the only goroutine that
// mutates loopState, installs or clears activeTurn, closes activeTurn.events,
// commits or discards turn messages, emits TurnStarted/StepDone/TurnFoldedInto
// at the same points it mutates loopState.msgs, and resolves pending gates. The
// loop lifetime context is cfg.loopCtx (each turn ctx derives from it); state is
// passed as the second argument because it is what runLoop evolves, distinct from
// the immutable cfg.
func runLoop(cfg loopConfig, state loopState)
```

The public `Loop` remains a handle over channels. `runLoop` keeps `loopConfig`
and `loopState` as locals; no wrapper type is required unless the loop goroutine
later needs receiver methods. `loopConfig` holds dependencies and
construction-time wiring (including `loopCtx` and the internal/commits/drains
handshake channels), while `loopState` holds identity, status, and accumulated
messages. The session event publisher lives in `loopConfig.events`, NOT in
`loopState`: it is a dependency, and parking it in state would be an SRP smudge.

`New(ctx, sessionID, loopID, parent, events, cfg)` owns the loop-side split: it
validates `cfg`, validates the session event publisher, creates the loop
goroutine channels, returns the public `Loop` handle, starts `runLoop`, and
initializes `loopState` with `newLoopState(sessionID, loopID, parent)`.
`sessionID`, `loopID`, and `parent` do not need to live in `loopConfig`; they are
construction inputs that become `loopState` identity.

`ctx` passed to `loop.New` is the loop's `loopCtx`, derived from the owning
session's `sessionCtx`. A loop context is the loop lifetime, not a turn lifetime.
When `runLoop` starts a turn, it derives `turnCtx, cancel :=
context.WithCancel(cfg.loopCtx)`, stores `cancel` on `activeTurn`, and is the
only goroutine that calls it. Submit commands never carry or queue a context.

Do not route session fan-in through `cfg.Sinks`. Sinks are side-effect
observability outputs and may be redacted or best-effort. Session fan-in is the
full-fidelity runtime stream owned by `AgentSession`.

Runtime hierarchy structs such as `turn`, `step`, and `block` own state only.
Dependencies live in config values passed to runner functions such as `runTurn`
and `runStep`. `Loop` is different: it is the public channel handle returned by
`New`.

Goroutine hierarchy:

```text
AgentSession methods
  -> send commands to one or more loop goroutines

runLoop goroutine, one per loop
  -> owns loopState and active turn runtime handles
  -> starts at most one active turn goroutine for that loop
  -> receives turnResult from that turn goroutine

runTurn goroutine, one per active turn
  -> runs LLM/tools and accumulates turn/step/block state
  -> emits through cfg.emit
  -> returns turnResult to runLoop
```

`content.AgenticMessages` is the full conversation thread. It already supports
all sealed message types, including `SystemMessage`.

Important distinction: the type supports `SystemMessage`, but the current loop
does not inject `cfg.Model.System` into `loopState.msgs`. Today
`ModelSpec.System` is prepended by the provider encoder. Moving that system
prompt into committed conversation history is a separate behavior change.

`loopState.id` and `loopState.sessionID` are intentionally distinct. A future
multi-agent session can have one shared `sessionID` for the orchestrator and its
subagents, while each agent loop has its own `id`.

### Turn

A turn is user-message initiated. A turn starts with one `UserMessage`, can fold
additional queued `UserMessage`s only when a completed tool-using step already
requires another LLM request, and has zero or more steps. A final no-tool
assistant answer ends the turn; pending input after that starts a later turn
instead of being pulled backward into the completed one.

```go
const inboxCap = 64 // bound on the actor-owned loopState.inbox; full -> TurnRejected{QueueFull}

// queuedInput is an accepted-but-unresolved submit sitting in loopState.inbox.
// inputID is the submit command id (so CancelQueuedInput can remove it by id while
// still queued). triggeredBy is the producing subagent's loop id for a
// SubagentResult (zero for a UserInput); TurnStarted/TurnFoldedInto/InputCancelled
// stamp it as Header.TriggeredByLoopID, which releases the parent's {wake, …}
// token.
type queuedInput struct {
    inputID     uuid.UUID
    triggeredBy uuid.UUID
    msg         *content.UserMessage
}

type turn struct {
    state turnState

    // events is the caller's per-turn stream for this loop and turn. It is OPTIONAL:
    // nil for a fan-in-only submit (an AllowFold UserInput that starts a turn with
    // no per-turn StreamReader, or a SubagentResult). emit and deliverAndClose are
    // nil-safe — when events is nil they skip the per-turn stream entirely (session
    // fan-in and sinks still receive) and never send-on-nil or close-nil.
    //
    // When non-nil it is intentionally separate from session fan-in: closing or
    // abandoning a parent turn stream must not affect events from another loop.
    events    chan<- event.Event // optional; may be nil
    abandoned <-chan struct{}     // optional; paired with events
    cancel    context.CancelFunc

    // The accepted-but-unresolved queue is loopState.inbox (loop-level,
    // actor-owned), NOT a per-turn field — so input arriving between turns is queued
    // and cancellable too. runTurn drains it only at tool-continuation boundaries via
    // cfg.drainPending.

    // pendingGates is owned by the loop goroutine for this active turn. The
    // turn goroutine registers gates through gateReg; it never mutates this map.
    pendingGates map[uuid.UUID]gate
}

type turnConfig struct {
    // base is the committed loop history before this turn's initial UserMessage.
    // LLM requests use base + state.msgs. Do not build requests from live
    // loopState.msgs while the turn runs: runLoop commits this turn incrementally
    // for durability/events, so loopState.msgs + state.msgs would duplicate the
    // already committed parts of the active turn.
    //
    // runLoop passes base as a defensive clone with its OWN backing array, never a
    // reslice of loopState.msgs. runLoop keeps appending committed step groups to
    // loopState.msgs while runTurn concurrently reads base; an aliased backing
    // array would let a cap-preserving append write into memory runTurn is reading
    // (a data race today only avoided by appends landing past base's len — fragile
    // under any future cap/ordering change). The clone is cheap (a slice header and
    // element pointers) and removes the hazard unconditionally.
    base content.AgenticMessages

    model   llm.ModelSpec
    tools   ToolSet
    client  llm.LLM
    gateReg chan<- gateRegistration
    idGen   idGenerator

    // commit is the durability/event handshake back to runLoop. runTurn prepares
    // complete messages, but runLoop is the only goroutine that mutates
    // loopState.msgs; it appends commit.Messages and emits commit.Event at the same
    // actor-owned point. Used for StepDone and TurnFoldedInto. The initial
    // UserMessage/TurnStarted commit happens before runTurn starts.
    commit func(context.Context, turnCommit) error

    // drainPending is the tool-continuation handshake: after a completed step that
    // produced tool results, runTurn calls it before the mandatory next LLM request.
    // Backed by a request/reply to runLoop, it returns the accepted messages from
    // loopState.inbox and clears them. It is ctx-cancellable — it selects on the
    // reply AND turnCtx.Done, so an Interrupt/Shutdown during the handshake frees
    // runTurn instead of wedging (same escape as emit). The inbox has a single owner
    // (the actor); runTurn never reads it directly. runTurn does not call this after
    // a no-tool final answer.
    drainPending func(context.Context) ([]*content.UserMessage, error)

    // emit publishes this loop's events to its active turn stream and to the
    // session fan-in. It is not a callback into a parent loop; parent/child loop
    // relationships are correlation metadata, not event transport.
    emit func(event.Event)
}

type turnCommit struct {
    Messages content.AgenticMessages
    Event    event.Event
}

func newTurn(
    state turnState,
    events chan<- event.Event,
    abandoned <-chan struct{},
    cancel context.CancelFunc,
) turn {
    return turn{
        state:        state,
        events:       events,
        abandoned:    abandoned,
        cancel:       cancel,
        pendingGates: make(map[uuid.UUID]gate),
    }
}

// runTurn runs in the turn goroutine started by runLoop. It owns LLM/tool
// execution and staged turn/step/block state, then returns turnResult to
// runLoop. It never mutates loopState and never closes event channels.
func runTurn(ctx context.Context, cfg turnConfig, t turn) turnResult

type turnState struct {
    // sessionID is copied from loopState so turn/step records can be correlated
    // without reaching back into loopState.
    sessionID uuid.UUID

    // loopID is copied from loopState.
    loopID uuid.UUID

    // id is the turn id.
    id uuid.UUID

    // index is the loop-local turn index: each loop numbers its own turns from 0.
    // There is no session-global turn counter (that would need shared mutable state
    // the no-session-goroutine design avoids). In a multi-loop session turn indexes
    // are NOT unique across loops; a turn is globally identified by (loopID, turnID)
    // and displayed as (loopID, index).
    index event.TurnIndex

    // causationID is the submit command id (UserInput/SubagentResult) that
    // initiated this turn.
    causationID uuid.UUID

    // msgs is the staged turn conversation:
    // initial UserMessage, zero or more step message groups, and optional queued
    // UserMessages inserted only after a tool-result group and before the
    // mandatory continuation LLM request.
    // (The accepted-but-unresolved queue is loopState.inbox, owned by the
    // loop actor; runTurn pulls it only at tool-continuation boundaries — it is
    // not turnState.)
    msgs content.AgenticMessages

    toolIterations int
    toolCalls      int
    status         turnStatus
}

func newTurnState(
    sessionID uuid.UUID,
    loopID uuid.UUID,
    turnID uuid.UUID,
    index event.TurnIndex,
    causationID uuid.UUID,
    user *content.UserMessage,
) turnState {
    return turnState{
        sessionID:   sessionID,
        loopID:      loopID,
        id:          turnID,
        index:       index,
        causationID: causationID,
        msgs:        content.AgenticMessages{user},
    }
}
```

Commit is incremental and loop-owned. `runLoop` commits the initial `UserMessage`
and emits `TurnStarted` before it starts `runTurn`. As each step completes,
`runTurn` sends a commit request; `runLoop` appends that step's finalized group to
`loopState.msgs` **and** emits the Enduring `StepDone` at the same actor-owned
point (so `StepDone` is never a lie). When queued input folds into a mandatory
tool-continuation request, `runLoop` commits that `UserMessage` and emits
`TurnFoldedInto` the same way. The in-flight step is staged in `stepState` and is
only speculative until it completes; `turnState.msgs` is the running view used to
build the next LLM request, while committed history grows message group by
message group.

`TurnFailed`/`TurnInterrupted` therefore discard **only the in-flight incomplete
step** (which never emitted `StepDone`); completed steps stay committed. This is
the deliberate change from the old whole-turn rollback: a multi-step agentic turn
must not lose four completed tool-steps because step five's LLM call errored, and
the event stream (`StepDone` per completed step) must not contradict history. A
terminal then means "the turn stopped here," not "the turn never happened."

Continuation steering:

- A new user message accepted while a regular turn is active is not inserted
  immediately into `turnState.msgs` and is not assigned to that turn yet.
- `runLoop` (the actor) appends it to `loopState.inbox` (the actor-owned
  queue) and replies `InputQueued`; it also removes it on `CancelQueuedInput` —
  both race-free, since the actor is the sole owner.
- A step boundary is after the current LLM response is materialized and, if it
  requested tools, after that step's tool-result messages are appended.
- If the step requested tools, another LLM request is already required to send the
  tool results. Before that request, `runTurn` calls `cfg.drainPending(ctx)`, appends
  the returned messages to `turnState.msgs` in receive order, commits each through
  `cfg.commit`, emits `event.TurnFoldedInto` for each, and makes the required next
  LLM request.
- If the step did not request tools, the assistant answer is final for this turn.
  `runTurn` does **not** drain pending input into the completed turn; it returns
  `TurnDone`, and `runLoop` starts a later turn from the queue if input is waiting.

This allows controlled active-turn steering without splicing user messages into
the middle of one assistant tool-use batch, saves a round trip only when a tool
continuation was already required, and keeps the queue single-owner so retraction
and overflow are decided synchronously by the actor (no blocking push).

### Step

A step is one LLM request/response cycle inside a turn. Each step produces
exactly one `AIMessage`. That `AIMessage` may contain thinking, text, tool-use
blocks, or all of them at once.

If that `AIMessage` contains tool-use blocks, executing those tools produces zero
or more `ToolResultMessage`s. Those tool result messages belong to the same
step, after the assistant message.

Phase 10 (Open Items A) collapsed the placeholder `step{state stepState}`
wrapper: it was a one-field struct with no methods and no runtime role, so
`runStep` takes `stepState` directly. `stepState` owns the step's messages and
block state.

```go
type StepIndex uint64

type stepConfig struct {
    req    llm.Request
    client llm.LLM
    emit   func(event.Event)
}

type stepResult struct {
    state    stepState
    terminal event.Event // nil on success; non-nil when runTurn should stop
}

// runStep owns one LLM request/response cycle. Config/dependencies stay at this
// boundary; stepState owns one step's messages and block state.
func runStep(ctx context.Context, cfg stepConfig, st stepState) stepResult

type stepState struct {
    // sessionID is copied from turnState.
    sessionID uuid.UUID

    // loopID is copied from turnState.
    loopID uuid.UUID

    // turnID is copied from turnState.
    turnID uuid.UUID

    // id is the step id.
    id uuid.UUID

    index StepIndex

    // msgs is one step conversation:
    // exactly one AIMessage followed by zero or more ToolResultMessages.
    msgs   content.AgenticMessages
    blocks blockState
    status stepStatus
}

func newStepState(
    sessionID uuid.UUID,
    loopID uuid.UUID,
    turnID uuid.UUID,
    stepID uuid.UUID,
    index StepIndex,
) stepState {
    return stepState{
        sessionID: sessionID,
        loopID:    loopID,
        turnID:    turnID,
        id:        stepID,
        index:     index,
    }
}
```

The step owns the in-progress assistant blocks while streaming. When the stream
ends, `blockState` materializes the step's single `AIMessage`, and the step
stores it in `msgs`. Tool execution later appends `ToolResultMessage`s to the
same `msgs`.

### Block

The block layer owns assistant block accumulation for one step.

Phase 10 (Open Items A) collapsed the placeholder `block{state blockState}`
wrapper for the same reason as `step`: a one-field struct with no methods.
`blockState` carries the materialization methods (`AIMessage`/`ToolUses`)
directly, so callers use it without a wrapper.

```go
type blockState struct {
    // msgs is the assistant block state for one AIMessage:
    // thinking, text, and tool-use blocks accumulated from chunks.
    msgs blockMessages
}

type blockMessages struct {
    thinking streamaccumulator.Thinking
    text     streamaccumulator.Text
    toolUses streamaccumulator.ToolUses
}
```

The block layer materializes:

```go
func (b *blockState) AIMessage() *content.AIMessage
func (b *blockState) ToolUses() []content.ToolUseBlock
```

`ToolUses` returns the executable view of the `ToolUseBlock`s contained in the
assistant message. The `ToolUseBlock`s themselves remain child blocks of the
`AIMessage`; they are not separate messages.

### Chunk

Chunks are streaming deltas from the LLM. The chunk layer is responsible for two
things:

1. emit the live `TokenDelta` event for each chunk; and
2. pass the chunk to the block layer for accumulation.

The chunk layer lives in `loop`, because event emission is loop behavior.
`internal/content/streamaccumulator` stays pure and does not import loop events.

```text
LLM chunk
  -> loop chunk layer emits TokenDelta
  -> streamaccumulator folds chunk into block state
  -> blockState materializes AIMessage
```

```go
type chunkProcessor struct {
    emit  func(event.Event)
    state chunkState
}

type chunkState struct {
    blocks *blockState
}

func newChunkProcessor(emit func(event.Event), state chunkState) chunkProcessor {
    return chunkProcessor{emit: emit, state: state}
}
```

`chunkProcessor` owns the ordering of "emit then accumulate" for streamed chunks.
It does not own message finalization; `blockState` materializes the final
`AIMessage` after the stream reaches EOF.

## Content Message Types

Rename `content.ToolMessage` to `content.ToolResultMessage`.

The current name is too generic. The message specifically carries the result for
a prior assistant `ToolUseBlock`.

```go
type ToolResultMessage struct {
    Message
    ToolUseID string
}
```

The role remains `RoleTool`; only the Go type name becomes more precise.

The semantic hierarchy becomes:

```text
content.AgenticMessages
  1. SystemMessage       // supported by type; loop injection is separate
  2. UserMessage
  3. AIMessage(thinking, text, tool_use)
  4. ToolResultMessage
```

```text
turnState.msgs content.AgenticMessages
  1. UserMessage
  2. AIMessage(thinking, text, tool_use)
  3. ToolResultMessage
  4. UserMessage             // optional tool-continuation steering
  5. AIMessage(...)
  6. ToolResultMessage(...)
```

```text
stepState.msgs content.AgenticMessages
  1. AIMessage(thinking, text, tool_use)
  2. ToolResultMessage
```

## Message Containers

Use `content.AgenticMessages` as the message container at every state boundary:

```go
type AgenticMessages []Conversation
```

Do not add `content.TurnMessages` or `content.StepMessages` in this design. The
state types own the shape:

```go
func (t *turnState) Messages() content.AgenticMessages
func (s *stepState) Messages() content.AgenticMessages
```

Shape rules:

- `turnState.msgs` starts with exactly one initial `UserMessage`.
- `turnState` appends complete `stepState.msgs` groups.
- `turnState` may append queued `UserMessage`s only after a completed tool-using
  `stepState.msgs` group and before the mandatory continuation LLM request.
- `loopState.inbox` (actor-owned, not `turnState`) holds accepted queued user
  messages until they fold at a tool-continuation boundary, start a later turn, or
  are cancelled/returned.
- `stepState.msgs` starts with exactly one `AIMessage`.
- `stepState` only appends `ToolResultMessage`s after its `AIMessage`.
- `blockState` does not produce standalone messages. It produces the
  `AIMessage` that becomes the first message in `stepState.msgs`.

The states enforce those rules through narrow methods instead of exposing append
access to their `msgs` fields.

## ID Ownership

IDs belong to loop machine entities, not content messages or blocks:

| Entity | Field | Meaning |
|---|---|---|
| session | `AgentSession.SessionID` | shared session id |
| session | `AgentSession.primaryLoopID` | default loop id |
| loop | `loopState.id` | loop id |
| loop | `loopState.sessionID` | shared session id |
| turn | `turnState.sessionID` | shared session id |
| turn | `turnState.loopID` | parent loop id |
| turn | `turnState.id` | turn id |
| turn | `turnState.index` | loop-local turn index (each loop numbers its own turns from 0; not unique across loops) |
| turn | `turnState.causationID` | submit command id (`UserInput`/`SubagentResult`) |
| step | `stepState.sessionID` | shared session id |
| step | `stepState.loopID` | parent loop id |
| step | `stepState.turnID` | parent turn id |
| step | `stepState.id` | step id |
| step | `stepState.index` | turn-local step index |
| event | `event.Header.ID` | event id |
| event | `event.Header.CausationID` | command id that caused the event, when available |
| event | `event.Header.SessionID` | producing session id; set on every event |
| event | `event.Header.LoopID` | producing loop id; zero for session-scoped events |
| event | `event.Header.TurnID` | producing turn id; zero for session/loop-scoped events |
| event | `event.Header.StepID` | producing step id, when the event is step-scoped |
| event | `event.Header.ToolCallID` | tool call id, when the event is tool-call scoped |
| event | `event.Header.Parent*ID` | optional grouping ids for subagent events |
| command | `command.Header.ID` | command id |
| control route | `command.Route` / event route | session + loop + turn + step + tool call ids |
| model tool use | `content.ToolUseBlock.ID` | provider/model tool-use id string |
| model tool result | `content.ToolResultMessage.ToolUseID` | answered provider/model tool-use id |

Do not add loop UUIDs to `content.UserMessage`, `content.AIMessage`,
`content.ToolResultMessage`, `content.Block`, or `content.Chunk`. Those values
remain semantic content. Events and future journal records can refer to content
by entity id plus position, such as `turnState.id`, `stepState.id`, and block
index.

## Routing

New input routes through the session, which only **forwards** it to the target
loop's command channel (`primaryLoopID` for `Invoke`/`Stream`). The session does
**not** decide whether the input starts a turn, waits in the loop queue, or later
folds into a tool-continuation request — that depends on the loop's live
`status`/`activeTurn`, which only the loop goroutine owns. Deciding it at the
session would be a TOCTOU race (the same reason the session does not pull-query
loop state for quiescence). So one submit command carries the input and the loop
replies with what it did:

```go
// InputMode lets the caller say whether queueing behind a running turn is allowed.
type InputMode uint8
const (
    AllowFold InputMode = iota // interactive: queue while running; may fold only at tool continuation
    StartOnly                  // Invoke/Stream: must start a turn; TurnRejected if busy
)

// UserInput is interactive input. The loop decides its disposition; the caller
// never assumes a turn was created. Submit commands do not carry context: a
// queued input can start much later, fold, be cancelled, or be returned. The loop
// creates a turn context only when a turn actually starts.
// Events/Abandoned are the optional per-turn stream — set by StartOnly callers
// (Invoke/Stream), nil for a fan-in-only submit.
type UserInput struct {
    command.Header
    Blocks    []content.Block
    Mode      InputMode
    Events    chan<- event.Event
    Abandoned <-chan struct{}
    Ack       chan<- Disposition
}

// SubagentResult delivers a finished subagent's output to its parent loop (the
// hand-back). Same submit semantics as UserInput, always AllowFold, no per-turn
// stream (the parent's events go to the fan-in). FromLoopID is the producing
// subagent: the loop stamps it as TriggeredByLoopID on start/fold/return events.
// Those event outcomes release the parent's expectTurn token on the publish path;
// a TurnRejected reply is handled by the session via cancelExpectTurn.
type SubagentResult struct {
    command.Header
    FromLoopID uuid.UUID
    Blocks     []content.Block
    Ack        chan<- Disposition
}

// Disposition is the loop's answer. The caller branches on it rather than waiting
// for a TurnStarted that may never come. InputQueued is queue acceptance, NOT a
// turn assignment: the queued input may later resolve as TurnFoldedInto,
// TurnStarted, or InputCancelled.
type Disposition interface{ isDisposition() }
type TurnStarted  struct { TurnID, InputID uuid.UUID } // new turn; the event.TurnStarted follows
type InputQueued  struct { InputID uuid.UUID }         // accepted into loopState.inbox (not assigned to a turn yet)
type TurnRejected struct { Reason RejectReason }       // busy + non-queueable, full, or shutting down

type RejectReason uint8
const (
    RejectBusy RejectReason = iota // running and the submit is StartOnly/non-queueable
    RejectQueueFull
    RejectShuttingDown
)
```

The loop goroutine decides on its own state (race-free — it owns that state):

```text
On UserInput / SubagentResult the actor:
  if shutting down                -> Ack TurnRejected
  else if inbox at inboxCap       -> Ack TurnRejected{QueueFull}   (length check; never blocks)
  else if running & not queueable -> Ack TurnRejected              (StartOnly, compact/review/internal turn)
  else by state:
    idle    -> start a turn from this entry;
               commit the initial UserMessage;
               emit event.TurnStarted{InputID, Message, Header.CausationID=inputID};
               Ack TurnStarted{turnID, inputID}
    running -> append to loopState.inbox (ordered);
               Ack InputQueued{inputID}

When a turn ends normally and the actor goes idle, if inbox is non-empty it
immediately pops the first queued entry and starts a new turn from it (closes the
end-of-turn gap; no input is stranded). Additional queued entries remain queued;
they fold only if that new turn reaches a tool-continuation boundary, otherwise
they start later turns one at a time.
```

`UserInput` and `SubagentResult` share this one decision path; they differ only in
provenance (`SubagentResult.FromLoopID`) and in the `TriggeredByLoopID` the loop
stamps on any turn that results. For both submit kinds, any `TurnStarted`,
`TurnFoldedInto`, or `InputCancelled` caused by that submit stamps
`Header.CausationID` to the submit command id (`InputID`).

The session owns the `SubagentResult` round trip. It creates a buffered
`Ack` channel, routes the command to the parent loop, reads the returned
`Disposition`, and if the result is `TurnRejected` calls
`hub.cancelExpectTurn(subagentLoopID)` itself. The loop decides rejection, but it
does not mutate session quiescence state and does not depend on
`cancelExpectTurn`.

Acceptance, turn assignment, and commit are deliberately separated.
`InputQueued` (the reply) means "accepted into the loop queue," not "folded into
the active turn." A queued message later resolves through exactly one Enduring
event: `TurnFoldedInto` when it rides with a tool-continuation request,
`TurnStarted` when it becomes the initial message of a later turn, or
`InputCancelled` when it leaves the queue without committing. `TurnRejected` is
reply-only: nothing happened in the session, so there is no event.

`Invoke`/`Stream` return a per-turn `StreamReader`, which assumes a turn exists, so
they stay "start-or-reject" for programmatic single-shot callers (a fold has no
fresh per-turn stream). The interactive path submits `UserInput`, reads the
`Disposition`, and observes results on the session fan-in subscription.

The `ctx` passed to user-facing APIs (`Invoke`, `Stream`, `Interrupt`,
`Shutdown`) governs the API call: sending the command, waiting for an ack, or
waiting for a terminal event. It is not stored in `UserInput`/`SubagentResult` and
does not become the turn context. If `Invoke(ctx)` is cancelled after a turn has
started, the session should translate that boundary cancellation into an
`InterruptLoop` command and then return/drain according to the API contract.
`Stream(ctx)` uses `ctx` to start the stream; after a reader is returned,
reader close/abandon is represented by `Abandoned`, not by a queued context.

### Queued input: retract, fold, start, or return

An `InputQueued` message is not yet committed — it sits in
`loopState.inbox` until it either folds into a tool-continuation request or starts
a later turn, so the client can retract it, and the loop guarantees it is never
silently lost. The loop owns the queue, so it resolves every race (no session-side
TOCTOU):

```go
// CancelQueuedInput retracts a still-queued message. Routed to the loop like
// Approve/Deny; the loop resolves it against its own queue.
type CancelQueuedInput struct {
    command.Header
    Route   command.Route // selects the loop; queued input is resolved by InputID
    InputID uuid.UUID      // the UserInput command id returned in InputQueued
    Ack     chan<- CancelResult
}

type CancelResult interface{ isCancelResult() }
type Cancelled        struct{}                   // removed before it committed
type AlreadyCommitted struct{ TurnID uuid.UUID } // too late — already started or folded

```

All loop reply channels are non-nil, buffered with capacity 1, and receive at
most one reply. The buffer is the normal-path guarantee: a correct caller can
read late or abandon without wedging the loop goroutine. The loop still sends
through a non-blocking helper so a contract violation (nil/unbuffered/already
filled) cannot wedge the actor:

```go
// tryAck is the one non-blocking reply helper for every loop-originated reply
// channel: Disposition (UserInput/SubagentResult), CancelResult (CancelQueuedInput),
// and command.Command (gate.reply). The buffered cap-1 channel makes the send
// succeed on the normal path; the select/default keeps a contract violation
// (nil/unbuffered/already filled) from wedging the actor.
func tryAck[T any](ack chan<- T, v T) {
    select {
    case ack <- v:
    default:
        // contract violation; log loudly in production and assert in tests
    }
}
```

Session-created `Ack` channels must be `make(chan T, 1)`. The default branch is
not a normal drop policy; it exists only to protect the actor and should be noisy
so tests catch violations.

`InputCancelled` is defined in the Event API section. It carries the `InputID`,
`Reason`, and original `UserMessage`; its `Header.CausationID` is the submit
command id (`InputID`), and its `Header.TurnID` is the active turn that caused a
return, or zero for a pure client retract outside a turn.

A queued message ends in exactly one loop-decided outcome:

- **folded** — a tool-using step completed, so a next LLM request is already
  required; `runTurn` drains the queue, appends the message after the tool results,
  and commits `event.TurnFoldedInto`.
- **started** — the active turn completed normally before a tool-continuation
  drain consumed the message; `runLoop` pops it from the queue and starts a later
  turn, committing `event.TurnStarted`.
- **retracted** — `CancelQueuedInput` arrives while still queued → `Cancelled` +
  `InputCancelled{Reason: CancelClientRetracted}`. If it already committed →
  `AlreadyCommitted` (fail-secure: you can't un-commit what's in the transcript).
- **returned** — the active turn ends abnormally (`TurnInterrupted`/`TurnFailed`)
  before queued input starts or folds → `InputCancelled{Reason, Message}`, and the
  loop does **not** auto-start a new turn from those returned messages. The message
  goes back to the client, which decides (resend = a fresh `UserInput`, or drop).
  The prompt and the decision live in the client, not the loop. (A future
  unified loop→user question primitive could host such a decision at the session —
  see `docs/plans/2026-06-18-gates-package-design.md`; out of scope here.)

`Interrupt(ctx)` is not just a command to `primaryLoopID` in a multi-agent
session. It cancels the active root turn tree: the primary loop's active turn and
every subagent loop transitively spawned under it. The session reconstructs that
tree from the registry — each `loopHandle.parent` records the loop that spawned
it (`loop.Provenance`), so the target set is the transitive closure over
`parent.LoopID` from `primaryLoopID`:

```go
func (s *AgentSession) Interrupt(ctx context.Context) error {
    s.loopsMu.RLock()
    targets := descendantsAndSelf(s.loops, s.primaryLoopID) // []*loopHandle
    s.loopsMu.RUnlock()                                     // snapshot, send outside the lock
    for _, h := range targets {
        // Each loop goroutine cancels its own active turn and closes its own
        // per-turn stream. The send must not block on one wedged loop, so it
        // escapes on the loop's Done (already stopped — nothing to interrupt) and
        // on the caller's ctx. runLoop is an always-selecting actor, so in the
        // normal case the command is taken immediately and the loop is reached.
        select {
        case h.loop.Commands <- command.Interrupt{ /* Header */ }:
        case <-h.loop.Done:
        case <-ctx.Done():
            return &SessionError{Kind: SessionInterruptCanceled, Cause: ctx.Err()}
        }
    }
    return nil
}
```

The `parent.LoopID` closure may include **idle persistent** subagent loops (loops
are not torn down on completion — see below), but interrupting an idle loop is a
harmless no-op: its `runLoop` has no active turn to cancel. Loops with an active
turn are cancelled; idle ones are skipped. So the closure is safe to send to
wholesale, and the session need not track the primary's active turn id to compute
it. `parent.TurnID`/`parent.StepID` are retained for event correlation and a future
precise "interrupt only turn N's subtree".

Do not derive a persistent subagent loop's `loopCtx` from the parent turn or
spawning tool call. Subagent loops share the session lifetime: their `loopCtx` is
derived from `sessionCtx`, just like the primary loop. Parent-child cancellation
is expressed by routing `Interrupt` through the session registry; `sessionCtx`
cancellation is the shutdown backstop.

`InterruptLoop(ctx, loopID)` can exist as an explicit narrow API for cancelling
only one loop's active turn.

`Shutdown(ctx)` first calls `hub.stopSession()` so the session phase becomes
`SessionStopped` before any loop can publish a shutdown-induced `LoopIdle`.
Then it snapshots the loop registry under `loopsMu`, sends `Shutdown` to every
loop outside the lock — each send using the same `Done`/`ctx` escape as
`Interrupt`, so one wedged loop cannot stall shutting down the rest — and cancels
`sessionCancel` as the hard backstop for all loop contexts. It stops loop goroutines; it is not the same as interrupting
active turns. `SessionStopped` marks the phase transition, not the end of event
delivery: shutdown-induced `TurnInterrupted`/`LoopIdle` events may still arrive
after it, but they do not mutate `sessionState.active` or emit `SessionIdle`.

Loops are **persistent**, not one-shot. A subagent loop is **not** torn down when
it hands back a result — it stays **idle** (alive) in the registry and retains its
`loopState.msgs`, so an orchestrator can route a follow-up turn (a `SubagentResult`
or `UserInput`) to the **same** loop with its full prior conversation intact. This
is the conversational-agent-team model: agents that talk back and forth like
people, each keeping its own history. Teardown is **never** result hand-back; a
completed subagent stays in the registry in idle state. Whether (and how) a loop is
ever *retired* is intentionally **left open** — to be designed with subagents +
checkpoints; for now the only teardown is session `Shutdown`. Quiescence is
unaffected: an idle persistent loop holds no `{loop, …}` key, so it contributes
nothing to `sessionState.active`, and Interrupt skips it (no active turn).

Future multi-agent APIs can expose explicit loop targeting without changing the
loop/turn/step hierarchy:

```go
func (s *AgentSession) InvokeLoop(
    ctx context.Context,
    loopID uuid.UUID,
    input []content.Block,
) (...)

func (s *AgentSession) StreamLoop(
    ctx context.Context,
    loopID uuid.UUID,
    input []content.Block,
) (...)
```

For future multi-agent sessions, routing has two layers:

```text
session/router layer:
  loopID -> target loop command channel

loop goroutine layer:
  active turn + toolCallID -> parked gate reply channel
```

Permission and user-input gates are turn runtime state, not session history.
`turn.pendingGates` holds only the gates for that loop's active turn. It is still
owned by the loop goroutine: the turn goroutine registers gates through
`gateReg`, and `runLoop` installs/routes/deletes entries.

Control events that require a reply carry a route:

```text
sessionID
loopID
turnID
stepID
toolCallID
```

`stepID` is for UI/journal/correlation. The parked runtime gate is addressed by
`toolCallID` within the active turn.

Represent this as a small command/event routing value:

```go
type Route struct {
    SessionID  uuid.UUID
    LoopID     uuid.UUID
    TurnID     uuid.UUID
    StepID     uuid.UUID
    ToolCallID uuid.UUID
}
```

The session routes replies to the loop:

```go
func (s *AgentSession) ApproveToolCall(
    ctx context.Context,
    route command.Route,
    approval tool.Approval,
) error {
    l, ok := s.loopFor(route.LoopID)
    if !ok {
        return &SessionError{Kind: SessionLoopNotFound}
    }
    l.Commands <- command.ApproveToolCall{
        Route:    route,
        Approval: approval,
    }
    return nil
}
```

`DenyToolCall` and `ProvideUserInput` use the same routing shape. The session
uses `route.LoopID` to select the loop command channel. The loop goroutine
verifies the route against its current active turn before waking a parked gate:

```go
if state.activeTurn == nil {
    drop
}
if cmd.Route.SessionID != state.sessionID || cmd.Route.LoopID != state.id {
    drop
}
if cmd.Route.TurnID != state.activeTurn.state.id {
    drop
}
gate := state.activeTurn.pendingGates[cmd.Route.ToolCallID]
if gate kind does not match command kind {
    drop
}
tryAck(gate.reply, cmd)
delete(state.activeTurn.pendingGates, cmd.Route.ToolCallID)
```

This keeps cross-loop routing out of a shared mutable gate map. Each loop owns
the parked runner state for its own active turn; the session layer only routes
commands to the correct loop.

Gate reply channels follow the same actor-safety rule as command `Ack` channels:
they are buffered with capacity 1, and the loop replies through the same generic
`tryAck` helper (here `tryAck[command.Command]`). The buffer handles the normal
race where a parked runner is about to resume; the non-blocking default prevents a
cancelled/stopped runner or a malformed test from wedging the loop goroutine. As
with `Ack`, the default path is a contract violation and should be logged/asserted,
not silently treated as success.

## Event Fan-In

Event transport is not hierarchical through loops. Every loop emits directly to
the session-level fan-in; a parent or primary loop never forwards child-loop
events, because a parent turn's live stream can close while a child loop is still
running. Parent/child identity is correlation metadata, not the transport path.

```text
primary loop events  -> session fan-in
subagent loop events -> session fan-in
```

The existing per-turn event channel can remain the caller-facing stream for a
specific turn. It must not become the transport path for events from other loops.

The loop publish path stays explicit:

```text
loop event
  -> active turn stream     (full fidelity, per-turn; backpressure with
                             ctx/abandon escape, exactly as the loop does today)
  -> session fan-in         (via PublishEvent; class-aware, non-blocking)
  -> cfg.Sinks              (redacted, best-effort; unchanged this spec)
```

### Publisher / subscriber, not a raw channel

The session does **not** expose a raw `events chan event.Event`. A bare channel
demands a drainer the session (which has no goroutine) cannot provide, and would
deadlock a headless run with no consumer. The fan-in is a narrow publish/
subscribe contract instead:

```go
// Loops depend only on this. They cannot close the fan-in or observe its
// buffering, subscriber set, or shutdown state.
type eventPublisher interface {
    PublishEvent(context.Context, event.Event) error
}

// Consumers (TUI/CLI now, a durable journal later) attach here.
type eventSubscriber interface {
    SubscribeEvents(EventFilter) (*EventSubscription, error)
}

// EventSubscription is the consumer's handle. Events closes when the subscriber
// closes it or when the hub fails it for loss. Err returns nil for an intentional
// Close and a typed error for hub-forced termination. SessionStopped is an event,
// not a stream terminator; subscriptions end only on Close, loss, or hub teardown.
type EventSubscription struct { /* Events() <-chan event.Event; Close() error; Err() error */ }

type SubscriptionLossError struct {
    DroppedClass event.Class
    Cause        error
}
```

Delivery is **direct fan-out on the publishing goroutine** — the same push model
the sink path already uses (`publish` iterates `cfg.Sinks` and calls `OnEvent`
inline). No session goroutine is required for fan-out itself; a subscriber that
needs async behaviour (a batching journal) owns that goroutine inside its own
subscriber, where the need actually is.

Policy:

- **No subscribers → delivery is a no-op and `PublishEvent` returns nil.**
  The hub still applies publish-path `sessionState` transitions (`active`/`phase`,
  wake-token release, `WaitIdle` signalling) before it sees there is nobody to
  deliver to. A loop never blocks because nobody is listening (the headless case).
- **Each subscription owns one bounded egress channel.** One slow subscriber
  never blocks another, and never blocks a loop.
- **One lock; mutate-or-read by operation, deliver outside it.** A single
  `sync.RWMutex` guards the hub's mutable state — the subscriber set **and**
  `sessionState.active`/`phase`. Active/phase-mutating operations take the **write**
  lock: `PublishEvent` for `TurnStarted`/`LoopIdle`/wake-release events, plus the
  session methods `expectTurn`/`cancelExpectTurn`/`stopSession`. Non-mutating
  publishes (the `TokenDelta` firehose, `StepDone`, tool/gate events — they don't
  touch `active`) take the **read** lock. Either way the critical section only
  applies the `active`/`phase` change (if any) and **copies the subscriber slice**;
  delivery happens **outside** the lock. (The earlier "snapshot under RLock" was
  wrong — `active` mutations need the write lock, not a read lock.) A slow
  consumer can never stall `SubscribeEvents`/teardown or another loop, since
  nothing is delivered under the lock.
- **Slow-subscriber policy is class-aware, with a hard floor:** only Ephemeral
  events may be shed under backpressure. "Block with ctx" is *not* an option for
  the live stream — a blocking subscriber re-introduces the cross-loop coupling
  direct fan-in exists to avoid (a slow journal would throttle every publishing
  loop). Ephemeral overflow **drops** the event for that subscriber.
- **Enduring events are never *silently* dropped.** If a subscriber's buffer would
  overflow on an Enduring event, the hub **fails that subscription with a typed
  loss error** and closes it — the event is not delivered to *that* subscriber, but
  it is never silently lost: the subscriber learns it lost the stream and can
  re-subscribe and re-sync. (It is not delivered late or out of order either; the
  subscription simply ends with a typed terminal.)

### Event API: header, scope, class, and concrete events

Event shape is defined in one place. Every concrete event embeds a `Header`,
exactly one lifecycle mixin (`ephemeral`, `enduring`, or `terminal`), and exactly
one scope mixin (`sessionScoped` or `loopScoped`). The header gives
fan-in/filter/journal consumers producer identity without a transport-only
envelope; the lifecycle mixin gives the hub its delivery policy without a
concrete type switch; the scope mixin defines whether the event is session-global
or loop-produced. `LoopID == 0` is a validation invariant for session events, not
the definition of a session event.

The field names below are the loop-machine shape. Before implementation, apply
`docs/plans/2026-06-18-id-normalization-design.md` to normalize names such as
`InputID`, `CallID`, `ToolCallID`, and `Header.CausationID`.

```go
type Class uint8

const (
    Ephemeral Class = iota // reconstructable from a later authoritative event -> droppable
    Enduring               // authoritative transition/payload -> never silently dropped
)

type Scope uint8

const (
    ScopeSession Scope = iota
    ScopeLoop
)

type CancelReason uint8
const (
    CancelClientRetracted CancelReason = iota
    CancelTurnInterrupted
    CancelTurnFailed
)

type Header struct {
    // SessionID is set on every event.
    SessionID uuid.UUID

    // Producer identity. For session-scoped events, LoopID/TurnID/StepID are zero.
    // For loop-scoped events, LoopID is set. TurnID is set for turn events; StepID
    // is set for step/tool scoped events.
    LoopID uuid.UUID
    TurnID uuid.UUID
    StepID uuid.UUID

    // TriggeredByLoopID is set on a turn/input event caused by a SubagentResult
    // (= the producing subagent's loop id); zero otherwise. The publish path
    // releases {wake, TriggeredByLoopID} when it sees TurnStarted/TurnFoldedInto/
    // InputCancelled carrying it (see quiescence).
    TriggeredByLoopID uuid.UUID

    // CausationID is set when an event is directly caused by a command. For
    // UserInput/SubagentResult resolution events (TurnStarted, TurnFoldedInto,
    // InputCancelled), it is the submit command id and equals InputID.
    CausationID uuid.UUID

    // ToolCallID is set on gate/tool lifecycle events when CallID is available.
    ToolCallID uuid.UUID

    // Event identity and parent grouping are present on the type, but detailed
    // wiring and EventEnvelope replacement are sequenced after the
    // journal/redaction follow-on.
    ID           uuid.UUID
    ParentLoopID uuid.UUID
    ParentTurnID uuid.UUID
    ParentStepID uuid.UUID
}

func (h Header) EventHeader() Header { return h }

type Event interface {
    isEvent()
    Class() Class
    Scope() Scope
    EndsTurn() bool // turn-terminal: the last event this turn's per-turn stream carries
    EventHeader() Header
}

type ephemeral struct{} // streaming delta
func (ephemeral) Class() Class   { return Ephemeral }
func (ephemeral) EndsTurn() bool { return false }

type enduring struct{} // authoritative; mid-turn, or mid-loop (e.g. LoopIdle)
func (enduring) Class() Class    { return Enduring }
func (enduring) EndsTurn() bool  { return false }

type terminal struct{} // ends the turn; folds in Class()==Enduring
func (terminal) Class() Class    { return Enduring }
func (terminal) EndsTurn() bool  { return true }

type sessionScoped struct{}
func (sessionScoped) Scope() Scope { return ScopeSession }

type loopScoped struct{}
func (loopScoped) Scope() Scope { return ScopeLoop }

// Session-scoped events. Header.SessionID is set; LoopID/TurnID/StepID are zero.
type SessionStarted struct { enduring; sessionScoped; Header }
type SessionActive  struct { enduring; sessionScoped; Header } // Idle -> Active edge
type SessionIdle    struct { enduring; sessionScoped; Header } // Active -> Idle edge
type SessionStopped struct { enduring; sessionScoped; Header }

// Loop-scoped event. Header.SessionID + Header.LoopID are set; TurnID/StepID are zero.
type LoopIdle struct { enduring; loopScoped; Header }

// Turn/step-scoped events.
type TokenDelta      struct { ephemeral; loopScoped; Header; TurnIndex TurnIndex; Chunk content.Chunk }
type TurnStarted     struct { enduring;  loopScoped; Header; TurnIndex TurnIndex; InputID uuid.UUID; Message *content.UserMessage } // initial UserMessage committed by runLoop; Header.CausationID == InputID
type StepDone        struct { enduring;  loopScoped; Header; Messages content.AgenticMessages }       // step group: AIMessage + its ToolResultMessages
type TurnFoldedInto  struct { enduring;  loopScoped; Header; TurnIndex TurnIndex; InputID uuid.UUID; Message *content.UserMessage } // input folded into a tool-continuation request; Header.TriggeredByLoopID set for a hand-back
type InputCancelled  struct { enduring;  loopScoped; Header; TurnIndex TurnIndex; InputID uuid.UUID; Reason CancelReason; Message *content.UserMessage }
type TurnDone        struct { terminal;  loopScoped; Header; TurnIndex TurnIndex }
type TurnFailed      struct { terminal;  loopScoped; Header; TurnIndex TurnIndex; Err error }
type TurnInterrupted struct { terminal;  loopScoped; Header; TurnIndex TurnIndex }

// Gate/tool lifecycle events. Header.ToolCallID is set to CallID when available.
type PermissionRequested struct {
    enduring; loopScoped; Header
    CallID  uuid.UUID
    Request tool.PermissionRequest
}

type UserInputRequested struct {
    enduring; loopScoped; Header
    CallID   uuid.UUID
    Question string
    Choices  []string
}

type ToolCallStarted struct {
    enduring; loopScoped; Header
    CallID   uuid.UUID
    ToolName string
    Summary  string
}

type ToolCallCompleted struct {
    enduring; loopScoped; Header
    CallID        uuid.UUID
    IsError       bool
    ResultPreview string
}
```

All concrete event types live in package `event` and implement the unexported
`isEvent()` marker, preserving the sealed event union. The marker methods are
boilerplate and omitted from the struct block.

For submit-resolution events, `Header.CausationID` is the command id of the
`UserInput` or `SubagentResult` that supplied the message, and must match the
event's `InputID`. `TurnStarted.Message` is the exact `UserMessage` committed as
the first message of the turn; `TurnFoldedInto.Message` is the folded user
message; `InputCancelled.Message` is the returned/retracted user message.

The three lifecycle mixins are the complete lifecycle vocabulary; each event
embeds exactly one. The two scope mixins are the complete scope vocabulary; each
event embeds exactly one. The interface requires `Class()`, `Scope()`, and
`EndsTurn()` with no silent default; embedding two lifecycle mixins or two scope
mixins makes selectors ambiguous, so the type stops satisfying `Event` and fails
to compile. `terminal` folds in `Class() == Enduring`, so **terminal ⇒ Enduring**
holds by construction — a turn-ender can never be classified droppable.

Scope invariants:

- `ScopeSession`: `Header.SessionID` is set; `Header.LoopID`, `TurnID`, and
  `StepID` are zero.
- `ScopeLoop`: `Header.SessionID` and `Header.LoopID` are set; `TurnID`/`StepID`
  are set when the event is turn/step scoped.

Class is **semantic** (is this reconstructable later?), not a transport flag —
which is why it belongs on the event. The mapping is "deltas are ephemeral; state
transitions are enduring":

- **Ephemeral:** `TokenDelta` only. It carries any `content.Chunk` (`TextChunk` |
  `ThinkingChunk` | `ToolUseChunk`), so one declaration covers text, thinking,
  and tool-use deltas; there is no separate `ThinkingDelta`/`TextDelta` event.
- **Enduring:** `StepDone`, `LoopIdle`, `SessionActive`, `SessionIdle`,
  `SessionStopped`, `TurnStarted`,
  `TurnFoldedInto`, `InputCancelled`, `TurnDone`, `TurnFailed`, `TurnInterrupted`,
  `PermissionRequested`, `UserInputRequested`, `ToolCallStarted`,
  `ToolCallCompleted`, `SessionStarted`. Of these only the three turn-enders are
  `terminal` (`EndsTurn()`); the rest are non-terminal.

Dropping is a fan-out concern only; it never touches the loop's own accumulation.
The chunk layer always folds every chunk into `blockState` regardless of whether
the `TokenDelta` event was delivered, so the finalized message and the
provider-visible history are always complete — including thinking content and its
signature.

### Turn end, loop idle, session quiescence, and session stop

Different "ending" questions need different mechanisms — none derivable from another:

| Signal | Scope | Means | Consumer |
|---|---|---|---|
| `EndsTurn()` (`TurnDone`/`TurnFailed`/`TurnInterrupted`) | turn | this turn's stream is over | per-turn stream: `Invoke`/`Stream`/`deliverAndClose` |
| `LoopIdle` (Enduring event) | loop | the loop is parked (no active turn) | session quiescence |
| `SessionActive` (Enduring event) | session | work resumed (`Idle → Active`) | UI disables input |
| `SessionIdle` (Enduring event) | session | the whole interaction is at rest | UI re-enables input; `WaitIdle` |
| `SessionStopped` (Enduring event) | session | shutdown/stopping has begun | `WaitIdle` returns `ErrSessionStopped`; UI tears down |

`EndsTurn()` is **turn-terminal, per loop** — the last event a single turn's stream
carries. Intrinsic and local ("this turn is over"), correct in every model.
Consumers detect turn end via `ev.EndsTurn()` (Open/Closed, replacing today's
`TurnDone|TurnFailed|TurnInterrupted` type switch); `Invoke` returns on it and
`runLoop` closes the per-turn stream after delivering it. It is `Enduring` by
construction, so a reader sees exactly one per turn. Terminal is a refinement
*within* Enduring, not a fourth orthogonal axis.

These cannot be collapsed: a loop that ends turn N and immediately starts turn N+1
emits turn N's terminal but **not** `LoopIdle` (running→running), so idle is not
derivable from terminal; and `LoopIdle` is loop-scoped with no turn identity, so it
cannot close turn N's stream. `SessionStopped` is a terminal session phase, but not
a terminal event stream marker. Keep these separate.

#### Federated quiescence: loops report idle, the session aggregates

The loop owns its status, so it is the authority on idle: it emits the Enduring,
non-terminal `LoopIdle` on its running→idle transition (its "now busy" half is
already signaled by `TurnStarted`). The session does **not** poll loops — reading
`loopState` through a handle is a data race, and a global pull is TOCTOU-racy. It
derives quiescence from events it already receives (it owns the fan-in), as one
state object:

```go
type SessionPhase uint8
const (
    SessionIdle    SessionPhase = iota // quiescent — user may type again; zero value, so a freshly built session is idle until its first turn
    SessionActive                       // ≥1 loop busy, or a hand-back in flight
    SessionStopped                      // after Shutdown (distinct from Idle)
)

var ErrSessionStopped = errors.New("session stopped")

type sessionState struct {
    // active holds one entry per outstanding unit of work; empty ⇔ quiescent.
    // activityKey is {kind, id} so loop and wake entries with the same uuid coexist:
    //   {loop, LoopID}          — a busy loop
    //   {wake, subagentLoopID}  — a pending hand-back (its result will start or fold into a parent turn)
    active map[activityKey]struct{}
    phase  SessionPhase
}
```

Quiescence is the single predicate **`len(active) == 0`** while
`phase != SessionStopped`. All `active`/`phase` mutations run under the hub lock
(no session goroutine), but they have two owners:

1. `PublishEvent` applies event-derived transitions from loop/session events.
2. Session methods apply session-owned transitions:
   `expectTurn(subagentLoopID)`, `cancelExpectTurn(subagentLoopID)`, and
   `stopSession()`.

Loops never call `expectTurn`, `cancelExpectTurn`, or `stopSession`; their only
session dependency remains the narrow `eventPublisher`.

Every active-mutating operation — whether owned by `PublishEvent` or by a session
method — runs the same edge check under the hub lock after applying its mutation:
it compares `active` emptiness before and after and derives **at most one** phase
event. `SessionActive` fires on the `Idle → Active` edge (empty → non-empty);
`SessionIdle` fires on the `Active → Idle` edge (non-empty → empty) and also wakes
`WaitIdle`. This is why `expectTurn` can announce `SessionActive` (an async
hand-back token taken while the session was idle) and `cancelExpectTurn` can
announce `SessionIdle` (the rejected/discarded token was the last outstanding
work): neither is a loop or turn event, but both cross an emptiness edge. A single
operation crosses at most one edge — a hand-back `TurnStarted` removes `{wake, s}`
and adds `{loop, parent}` in the same step, so `active` never dips to empty and no
edge fires.

| Entry point | Effect on `active` / `phase` |
|---|---|
| `TurnStarted(loopID)` | add `{loop, loopID}` |
| `LoopIdle(loopID)` | remove `{loop, loopID}` |
| `expectTurn(subagentLoopID)` at spawn | add `{wake, subagentLoopID}` |
| `TurnStarted` carrying `TriggeredByLoopID == s` | remove `{wake, s}`; the parent loop key is added in the same step |
| `TurnFoldedInto` carrying `TriggeredByLoopID == s` | remove `{wake, s}`; the parent loop key is already present (it was busy) |
| `InputCancelled` carrying `TriggeredByLoopID == s` | remove `{wake, s}`; the queued hand-back was retracted/returned without starting or folding |
| `cancelExpectTurn(s)` — explicit discard | remove `{wake, s}` |
| `cancelExpectTurn(s)` after `TurnRejected` for `SubagentResult(s)` | remove `{wake, s}`; rejected, nothing was added (no event) |
| `stopSession()` | clear `active`; set `phase = SessionStopped`; signal `WaitIdle` waiters with `ErrSessionStopped`; derive `SessionStopped` |
| publish after `SessionStopped` | deliver if still subscribed (same filter + overflow policy), but do not mutate `active`/`phase` and never derive `SessionIdle`/`SessionActive` |

`active` is a **set of keyed ids, not a counter**: loop keys are idempotent (so
turn-chaining — `TurnStarted` again with no intervening `LoopIdle` — does not
double-count), and `delete` can't underflow. On the `Active → Idle` edge,
`PublishEvent` computes a local post event instead of writing to a queue (there is
no session goroutine to own one):

```text
// applyActivity runs under the hub lock for every active-mutating operation
// (event-derived in PublishEvent, and the session methods expectTurn/
// cancelExpectTurn). It applies the mutation, then derives the at-most-one edge.
applyActivity(mutate):
  if phase == SessionStopped:           // stopped is terminal; never mutate
    return nil
  wasEmpty = len(active) == 0
  mutate()                              // add/remove keys
  isEmpty = len(active) == 0
  if !wasEmpty && isEmpty:              // Active -> Idle
    phase = SessionIdle
    signal WaitIdle waiters
    return SessionIdle{Header.SessionID}
  if wasEmpty && !isEmpty:              // Idle -> Active
    phase = SessionActive
    return SessionActive{Header.SessionID}
  return nil                            // no edge crossed

PublishEvent(ev):
  take hub lock
  if phase == SessionStopped:
    subscribers = snapshot subscribers
    release hub lock
    deliver ev to subscribers           // same filter + overflow policy as below
    return
  post = applyActivity(apply ev's active mutation)
  subscribers = snapshot subscribers
  release hub lock

  deliver ev to subscribers             // shouldDeliver(filter, ev); Ephemeral drop / Enduring fail-close
  if post is set:
    deliver post to the same snapshot   // session-scoped: bypasses LoopScope; same overflow policy
```

The session methods reuse the same helper, so their emptiness edges emit the same
events with no session goroutine:

```text
expectTurn(s):
  take hub lock
  post = applyActivity(add {wake, s})       // SessionActive if the session was idle
  subscribers = snapshot subscribers
  release hub lock
  if post is set: deliver post to subscribers   // same filter + overflow policy

cancelExpectTurn(s):
  take hub lock
  post = applyActivity(remove {wake, s})    // SessionIdle if this emptied active
  subscribers = snapshot subscribers
  release hub lock
  if post is set: deliver post to subscribers   // same filter + overflow policy
```

This gives consumers deterministic ordering (`LoopIdle` before the derived
`SessionIdle`) without re-entering the hub lock and without requiring a session
event-draining goroutine. If there are no subscribers, both deliveries are skipped,
but the `active`/`phase` transition and `WaitIdle` signal still run.

`stopSession()` is the symmetric session-owned transition:

```text
stopSession():
  take hub lock
  if phase == SessionStopped:
    release hub lock
    return
  clear active
  phase = SessionStopped
  signal WaitIdle waiters with ErrSessionStopped
  post = SessionStopped{Header.SessionID}
  subscribers = snapshot subscribers
  release hub lock

  deliver post to subscribers             // same filter + overflow policy
```

`WaitIdle` checks `phase` under the hub lock on entry. If the session is already
stopped it returns `ErrSessionStopped` immediately; if the session stops while it
is waiting, `stopSession()` wakes it with the same error. Clearing `active` during
`stopSession()` never emits `SessionIdle` or `SessionActive` — it bypasses
`applyActivity` and forces `phase = SessionStopped` directly. `SessionStopped` is not guaranteed to
be the last delivered event: shutdown-induced turn terminals and `LoopIdle` may
arrive later, but post-stop publish never mutates `active`/`phase` and never
derives `SessionIdle`. Subscriptions close on explicit `Close`, loss, or hub
teardown, not merely because `SessionStopped` was delivered.

#### Subagent hand-back: the `expectTurn` token and its release key

The one thing no loop can know is that a finished subagent's result is in flight to
wake a parent — it lives *between* two loops. Without a guard, the instant a
subagent goes `LoopIdle` while the parent is already idle, `active` would empty and
fire a false `SessionIdle`. The guard is a `{wake, …}` entry taken at **spawn**.

The release key is the **subagent's loop id**, the only stable id known at *both*
ends: the hand-back `SubagentResult` command's id does not exist yet at
`expectTurn` time, so `Header.CausationID` cannot be the key. The subagent loop id
rides on the hand-back as `SubagentResult.FromLoopID`, and the loop stamps it as
`TriggeredByLoopID` on any turn it starts.

```text
spawn:     expectTurn(subagentLoopID)            -> add {wake, subagentLoopID}
running:   {loop, subagentLoopID} added/removed via the subagent's TurnStarted/LoopIdle
hand-back: result delivered to the parent as command.SubagentResult{FromLoopID: subagentLoopID}
           (enters loopState.inbox; queuedInput.triggeredBy = subagentLoopID)
release:   FIRST terminal outcome of that inbox entry:
             TurnStarted | TurnFoldedInto | InputCancelled carrying TriggeredByLoopID == s
               -> PublishEvent removes {wake, subagentLoopID}
             TurnRejected reply
               -> session reads Disposition and calls cancelExpectTurn(subagentLoopID)
discard:   cancelExpectTurn(subagentLoopID)                  -> remove {wake, subagentLoopID}
```

Release is on the **first terminal outcome** of the hand-back, **not** specifically
on `TurnStarted` — because the result may *fold* (no `TurnStarted`), or be *queued
then retracted/returned* before folding (`InputCancelled`, no `TurnFoldedInto`), or
be *rejected* (`TurnRejected`, no event). The event outcomes release on the
publish path; rejection releases on the session reply path via
`cancelExpectTurn`. Without the `InputCancelled`/`TurnRejected` cases the token
would leak when a queued hand-back is cancelled or rejected. `active` never dips
to empty on the fold/start cases: on `TurnStarted` the new loop key is added in
the same step; on `TurnFoldedInto` the parent was already busy. So
`TurnFoldedInto` and `InputCancelled` both carry `TriggeredByLoopID` (stamped
from `queuedInput.triggeredBy`), and the publish path keys those releases off it.

The two key *kinds* are why this is safe: while the subagent runs, `active` holds
both `{loop, subagentLoopID}` (busy) and `{wake, subagentLoopID}` (result pending) —
distinct entries. The subagent going idle removes only the `{loop, …}`; the
`{wake, …}` persists until its inbox entry reaches a terminal outcome. (The broader
cross-turn handle model — `wait`/`send`/`interrupt` — remains out of scope; the
hand-back delivery and its quiescence accounting are specified here.)

This assumes at most one in-flight hand-back per persistent subagent loop. A
second concurrent hand-back from the same loop would collide on the `{wake,
subagentLoopID}` set key; concurrent hand-backs are deferred to the cross-turn
handle model.

#### Synchronous reduction

A synchronous subagent runs *inside* its parent step and returns as the parent's
tool result — no `SubagentResult`, no `expectTurn`. So `{loop, parentLoopID}` is in
`active` the whole time it runs and there are no `{wake, …}` entries: `active`
empties exactly when the primary loop goes idle, and quiescence ≡ primary idle ≡
`Invoke` returning on the primary's terminal. The hand-back machinery is inert for
synchronous subagents; it engages only when a subagent's result is delivered
out-of-band via `SubagentResult`.

### StepDone and the self-heal contract

Ephemeral events are droppable **only because** a later Enduring event
re-establishes ground truth. `StepDone` is that anchor: emitted at step completion,
it carries the step's finalized **group** — the `AIMessage` (materialized
`blockState.AIMessage()`) plus its `ToolResultMessage`s — so every step has an
authoritative record independent of its streamed deltas. Tool results were never
streamed (only the redacted `ToolCallCompleted` preview), so `StepDone.Messages` is
their *only* authoritative carrier.

```text
ephemeral TokenDelta ... TokenDelta   (provisional; may drop under load)
enduring  StepDone{Messages}          (authoritative group: AIMessage + ToolResultMessages)
```

This makes three things hold at once:

- **Self-heal:** a consumer renders provisional text from `TokenDelta`, then snaps
  to `StepDone.Messages` at the step boundary; dropped deltas vanish on
  finalization. Without a per-*step* anchor, dropping an intermediate step's
  deltas would lose that step permanently (today's `TurnDone.Message` carries only
  the last step's message), so `StepDone` is a dependency of the drop policy, not
  optional.
- **Multi-step turns become representable:** per-step finalized groups flow as
  `StepDone` events; `TurnDone` becomes a lifecycle terminal (its `Message` field
  is no longer load-bearing and can be dropped).
- **UI message == stored message:** the TUI renders the loop's `StepDone.Messages`
  rather than its own independently accumulated text, so the displayed transcript
  equals the committed transcript by construction.

### EventFilter — subscribe by class x loop producer

`SubscribeEvents` takes a filter so a consumer can decline loop-produced events it
does not want (declared interest — deterministic, and distinct from backpressure
drop). Because producer identity rides on the event `Header` (`LoopID`), the
filter discriminates per class **and** per loop for loop-scoped events:

```go
type EventFilter struct {
    Ephemeral LoopScope // TokenDelta delivery
    Enduring  LoopScope // loop-produced StepDone, gates, tool lifecycle, terminals
}

type LoopScope struct {
    All   bool                   // deliver from every loop
    Loops map[uuid.UUID]struct{} // when !All, only these loop ids
}

func (s LoopScope) Matches(loopID uuid.UUID) bool {
    if s.All {
        return true
    }
    _, ok := s.Loops[loopID]
    return ok
}
```

A TUI that watches the primary loop stream live but wants only finalized output
from subagents:

```go
EventFilter{
    Ephemeral: LoopScope{Loops: {primaryLoopID}}, // live tokens: primary only
    Enduring:  LoopScope{All: true},              // results/gates: every loop
}
```

The filter is evaluated **at fan-out, before the bounded send**, so a subagent's
token firehose never even enters a subscriber's egress buffer. A useful UX falls
out for free: subagents appear collapsed-but-present (their `StepDone`/tool/gate
events are still delivered and attributed by `LoopID`) while only the chosen loop
streams live.

Session-scoped events (`ev.Scope() == ScopeSession`, currently
`SessionStarted`, `SessionActive`, `SessionIdle`, and `SessionStopped`) bypass the
`LoopScope` filter and are delivered to every subscriber. Their header still validates as
session-scoped (`SessionID` set, `LoopID`/`TurnID`/`StepID` zero), but consumers
should use `Scope()` rather than inferring session scope from zero ids.

```go
func shouldDeliver(filter EventFilter, ev event.Event) bool {
    if ev.Scope() == event.ScopeSession {
        return true
    }
    scope := filter.Enduring
    if ev.Class() == event.Ephemeral {
        scope = filter.Ephemeral
    }
    return scope.Matches(ev.EventHeader().LoopID)
}
```

### Producer identity on the event

The filter and direct fan-in both require each event to self-identify its
producer — with one shared fan-in there is no per-loop transport path to infer
identity from. The `Header` type is defined in the Event API section. Loop events
are stamped by the emitting loop from `loopState`/`turnState`/`stepState`; session
events are stamped by `AgentSession` with `SessionID` and zero loop/turn/step ids.
The fan-in never infers or repairs identity.

### Three orthogonal axes

Keep these separate; never express one through another:

| Axis | Question | Mechanism |
|---|---|---|
| Scope (`Session`/`Loop`) | Is this event session-global or loop-produced? | `Scope()` on the event |
| Class (`Ephemeral`/`Enduring`) | May the transport drop it under load? | `Class()` on the event |
| Redactable (`SinkProjection`) | Must a sink see a scrubbed copy? | existing `event.Redactable` |
| EventFilter | Does this subscriber even want it? | `SubscribeEvents(EventFilter)` |

`TokenDelta` is loop-scoped, ephemeral, redactable, and filterable, which is
exactly why they must stay independent.

### Redaction deferred and risk accepted

This spec folds in the event `Header`, concrete event shapes, and publish/
subscribe fan-in. It deliberately leaves the existing sink/redaction machinery
unchanged:

- Existing `Redactable`/`SinkProjection` implementations stay as-is for the events
  that already have them.
- `EventEnvelope` retirement and the full journal/redacting-subscriber design are
  deferred to the journal spec.
- New content-bearing events (`TurnStarted.Message`, `StepDone.Messages`,
  `TurnFoldedInto.Message`, `InputCancelled.Message`) are allowed to flow through
  the existing event paths without adding projections here, even if that means
  existing best-effort sinks may receive sensitive content.

That leakage risk is accepted for this design. Do not suppress these events, split
their payloads, or add new redaction projections in this spec just to avoid it; the
journal/redaction follow-on owns that problem.

## Stream Accumulator

Add `internal/content/streamaccumulator` for chunk-to-block folding. It is shared
by the loop and TUI/CLI live display path.

The package provides three focused helpers:

```go
type Thinking struct { ... }
func (a *Thinking) Add(chunk *content.ThinkingChunk)
func (a Thinking) Block() *content.ThinkingBlock
func (a Thinking) Empty() bool
```

```go
type Text struct { ... }
func (a *Text) Add(chunk *content.TextChunk)
func (a Text) Block() *content.TextBlock
func (a Text) Empty() bool
```

```go
type ToolUses struct { ... }
func (a *ToolUses) Add(chunk *content.ToolUseChunk)
func (a ToolUses) Blocks() []content.ToolUseBlock
func (a ToolUses) Empty() bool
```

`ToolUses` folds `ToolUseChunk`s by provider-supplied `Index`, concatenates
`InputJSON` fragments, and returns complete `ToolUseBlock`s in ascending index
order. It must use a map internally, not slice indexing, so negative or huge
provider indexes cannot panic or allocate an unbounded slice.

`Thinking.Block()` deliberately does not populate `ThinkingBlock.Signature`:
`ThinkingChunk` carries no signature, and the extended-thinking signature is
attached to the finalized block by the provider decode path, not reconstructed
from streamed deltas. This is a conscious omission (streaming does not populate it
today either); it is called out here so a future provider that streams signatures
has an obvious place to thread one through.

The accumulator does not send events, does not validate tool permissions, does
not decide turn failure, and does not know about the loop. It only converts:

```text
ThinkingChunk -> ThinkingBlock
TextChunk     -> TextBlock
ToolUseChunk  -> ToolUseBlock
```

The loop remains responsible for policy. For example, malformed tool-use input is
still handled as today: the stored assistant message must remain serializable,
while the raw executable tool use can still produce a model-visible tool-result
error.

## Turn Flow

The state boundaries become explicit:

```text
runLoop
  pop/start one UserInput/SubagentResult
  create turnState with one initial UserMessage
  commit initial UserMessage into loopState.msgs
  emit TurnStarted{InputID, Message, Header.CausationID=inputID}
  start runTurn with base = defensive clone of pre-turn loopState.msgs (own backing array)

runTurn
  loop:
    build LLM request from cfg.base + turnState.msgs
    run one step
      read chunks
      emit TokenDelta for each chunk
      fold chunks into blockState
      finalize one AIMessage into stepState.msgs

    if step has tool uses:
      check tool iteration/call limits
      run tool batch for the step's tool uses
      append ToolResultMessages to the same stepState.msgs

    // step complete -> commit it (per-step commit), then announce
    append complete stepState.msgs to turnState.msgs
    cfg.commit({Messages: stepState.msgs, Event: StepDone{Messages: stepState.msgs}})
      // runLoop appends to loopState.msgs and emits StepDone atomically

    if step has tool uses:
      batch := cfg.drainPending(ctx)   // pulls + clears loopState.inbox (ctx-cancellable)
      if batch is not empty:
        append batch to turnState.msgs in order
        cfg.commit({Messages: {msg}, Event: TurnFoldedInto{...}}) for each msg
      continue    // tool results already require the next LLM request

    if step has no tool uses:
      return TurnDone      // final answer: do not drain pending input into this turn

  on TurnFailed/TurnInterrupted:
    discard only the in-flight (incomplete) step; committed steps stay in loopState.msgs
    runLoop returns still-queued inbox entries via InputCancelled; no auto-start
```

The important invariant is that the LLM request for the next step uses:

```text
turnConfig.base + turnState.msgs
```

not live `loopState.msgs + turnState.msgs`. `loopState.msgs` is updated
incrementally by `runLoop` for durable history and events; using it as the active
turn request base would duplicate the initial user message, folded user messages,
and completed step groups already present in `turnState.msgs`.

## TUI/CLI Display

The UI should render the same semantic messages the loop stores.

While a step is streaming, the TUI/CLI may use
`internal/content/streamaccumulator` to render a provisional live `AIMessage`.
When the step or turn is finalized, display must align to the loop-produced
message structure:

```text
UserMessage
AIMessage(thinking, text, tool_use)
ToolResultMessage
UserMessage
AIMessage(...)
ToolResultMessage(...)
```

The UI must not collapse a multi-step turn into one assistant transcript entry.
Tool-use blocks are children of the `AIMessage` that requested them. Tool result
messages are separate messages immediately following the step's assistant
message. Queued user messages that fold into a tool-continuation request are also
separate transcript messages; they appear after that step's tool results and
before the next assistant message.

## Invariants

- A session has one or more loops.
- A loop has many turns.
- A turn starts with exactly one initial user message.
- A turn can contain additional queued user messages only when they fold into a
  tool-continuation request.
- Folded user messages are appended only after a completed tool-using step and
  before the mandatory continuation LLM request.
- A step boundary is after the assistant message and all tool results for that
  step are complete.
- A turn has zero or more steps.
- A step has exactly one assistant message.
- A step has zero or more tool result messages.
- An assistant message can contain thinking, text, tool use, or all of them.
- Tool-use blocks live inside the assistant message.
- Tool result messages live after the assistant message in the same step.
- Chunk-to-block folding is shared by loop and UI through
  `content/streamaccumulator`.
- Event emission remains loop-owned; content accumulation remains event-free.
- Every loop emits events directly to session fan-in.
- The session exposes publish/subscribe, not a raw fan-in channel; with no
  subscribers delivery is a no-op, but publish-path sessionState transitions
  still run and `PublishEvent` never blocks a loop.
- `PublishEvent` snapshots subscribers and delivers outside the lock; one slow
  subscriber never blocks a loop or another subscriber.
- Every event declares a `Scope` and `Class`. Ephemeral events (only `TokenDelta`)
  may be dropped under backpressure; Enduring events are never *silently* dropped
  — an Enduring overflow fails the subscription with `SubscriptionLossError`
  instead.
- `ScopeSession` is explicit, not inferred from `Header.LoopID == 0`.
  Session-scoped events (`SessionStarted`, `SessionActive`, `SessionIdle`,
  `SessionStopped`) have `SessionID` set, zero loop/turn/step ids, and are
  delivered to every subscriber.
- `ScopeLoop` events have `SessionID` and `LoopID` set and are matched by
  `EventFilter` using their class and producer `LoopID`.
- Submit-resolution events (`TurnStarted`, `TurnFoldedInto`, `InputCancelled`)
  stamp `Header.CausationID` to the submit command id and keep `InputID` equal to
  that value.
- Event payloads are immutable once emitted: producers do not retain-and-mutate a
  finalized `*content.AIMessage`/`*content.UserMessage`/`content.AgenticMessages`,
  and consumers treat them (and the slices) as read-only, copying before modifying.
- `turnConfig.base` is a defensive clone of the pre-turn `loopState.msgs` with its
  own backing array: `runLoop` keeps appending committed step groups to
  `loopState.msgs` while `runTurn` reads `base` concurrently, so the two must never
  share storage.
- The hub uses one `RWMutex` over subscribers + `active`/`phase`:
  active/phase-mutating operations take the write lock (`PublishEvent` for
  event-derived transitions, and session methods `expectTurn`/`cancelExpectTurn`/
  `stopSession`); non-mutating publishes take the read lock. Both paths copy the
  subscriber slice and deliver outside the lock.
- Every event declares `EndsTurn()`. Exactly `TurnDone`/`TurnFailed`/
  `TurnInterrupted` end the turn, and terminal ⇒ Enduring (the `terminal` mixin
  enforces it); `StepDone` and `LoopIdle` are Enduring but not terminal.
- Consumers detect turn end via `EndsTurn()`, not a type switch; the per-turn
  stream closes only after a turn-terminal event.
- A loop emits `LoopIdle` (Enduring, non-terminal) on its running→idle transition;
  idle is not derivable from a terminal (a chained turn ends without idling).
- Session quiescence is the single predicate `len(sessionState.active) == 0`
  while `phase != SessionStopped`, derived from event publish transitions and
  session methods, never from polling loops. `active` is a `{kind, id}` set
  (idempotent), not a counter. `SessionActive` (Enduring) fires on the
  `Idle → Active` edge and `SessionIdle` (Enduring) on the `Active → Idle` edge —
  both derived by the shared `applyActivity` check from `PublishEvent` and from
  `expectTurn`/`cancelExpectTurn`; neither fires during stop. `stopSession` clears
  `active`, sets `SessionStopped`, wakes `WaitIdle` with `ErrSessionStopped`, and
  delivers `SessionStopped`.
- Input is one submit command (`UserInput`/`SubagentResult`); the loop returns a
  `Disposition` (`TurnStarted`/`InputQueued`/`TurnRejected`). The session never
  decides start-vs-queue-vs-fold — the loop does, from its own state.
- `InputQueued` is queue acceptance, not commit and not turn assignment. A queued
  message ends folded (`TurnFoldedInto` at a tool-continuation boundary), started
  (`TurnStarted{Message}` as the first message of a later turn), retracted
  (`CancelQueuedInput` → `Cancelled`/`AlreadyCommitted`), or returned on an
  abnormal terminal (`InputCancelled{Reason, Message}`, no auto-turn) — never lost.
- A `{wake, subagentLoopID}` token is held from spawn until the **first terminal
  outcome** of its inbox entry carrying `TriggeredByLoopID == subagentLoopID`.
  Start/fold/return outcomes release on the `PublishEvent` path via
  `TurnStarted`/`TurnFoldedInto`/`InputCancelled`; `TurnRejected` is reply-only, so
  the session releases it after reading the `Disposition` by calling
  `cancelExpectTurn`. Releasing on any of these (not just `TurnStarted`) means a
  queued hand-back that is retracted/returned/rejected can't leak the token, while
  event-path release keeps `active` non-empty across a start/fold handoff.
- An Enduring `StepDone` carries each step's finalized group (`AIMessage` + its
  `ToolResultMessage`s); a dropped `TokenDelta` is reconciled against the next
  `StepDone`, never lost from history, and tool results have no other carrier.
- A subscriber whose Enduring buffer overflows has its subscription failed with
  `SubscriptionLossError` (channel closed; `Err()` returns the error), never
  silently stopped.
- `EventFilter` selects loop-scoped events by class and producer `LoopID`,
  evaluated before a subscriber's egress send; session-scoped events bypass the
  loop filter and go to every subscriber.
- Scope, Class, Redactable, and EventFilter are orthogonal axes.
- Dropping a `TokenDelta` affects only fan-out; the loop still folds every chunk
  into `blockState`, so finalized messages and provider history stay complete.
- Parent/child loop relationships are event metadata, not event transport.
- A per-turn event stream can close without affecting events from other loops.
- `turn.events`/`abandoned` are optional (nil for a fan-in-only submit); `emit` and
  `deliverAndClose` are nil-safe — they skip the per-turn stream when nil and never
  send-on-nil or close-nil. Fan-in and sinks still receive.
- Commit is loop-owned and incremental: the initial user message is appended to
  `loopState.msgs` when `TurnStarted` is emitted; a folded user message is appended
  when `TurnFoldedInto` is emitted; a completed step is appended when `StepDone` is
  emitted. A failed/interrupted turn discards only the in-flight incomplete step
  (which never emitted `StepDone`); completed steps stay committed — `StepDone`
  never contradicts history.
- `loopState.inbox` is actor-owned: only `runLoop` appends/removes/clears it (no
  locks). `runTurn` drains via `cfg.drainPending(ctx)` only at tool-continuation
  boundaries (a ctx-cancellable request/reply), never touching the inbox directly.
  On normal turn completion the actor starts a new turn from the first queued input
  if the inbox is non-empty (no late arrival stranded).

## Testing

- `streamaccumulator.Thinking` folds multiple thinking chunks into one
  `ThinkingBlock`.
- `streamaccumulator.Text` folds multiple text chunks into one `TextBlock`.
- `streamaccumulator.ToolUses` folds multi-fragment, multi-index tool chunks into
  stable ascending `ToolUseBlock`s and handles negative/huge indexes without
  panic or large allocation.
- `AgentSession` owns one `SessionID`, stores loop handles by loop id, and keeps
  `primaryLoopID` as the default target for existing single-agent methods.
- `AgentSession.NewLoop` creates a loop with the existing `SessionID`, a fresh
  loop id from `s.idGen`, the session event publisher, and stores it under
  `loopsMu`.
- `AgentSession.Invoke` and `AgentSession.Stream` route new turns to
  `primaryLoopID`.
- Session routing methods read `loops` through `loopFor`/`loopsMu`, not by
  directly indexing the map.
- `AgentSession.Interrupt` cancels every loop in the active root turn tree, not
  only `primaryLoopID`.
- `AgentSession.Shutdown` calls `hub.stopSession` before sending loop shutdown
  commands, then snapshots all loops under `loopsMu`, sends `Shutdown` to each
  loop outside the lock, and cancels `sessionCancel` as a backstop.
- `WaitIdle` returns `ErrSessionStopped` if the session is already stopped on
  entry or stops while the caller is waiting.
- Late shutdown-induced loop events (`TurnInterrupted`, `LoopIdle`) after
  `SessionStopped` are delivered to subscribers but do not mutate
  `sessionState.active`/`phase` and never emit `SessionIdle`.
- `AgentSession.ApproveToolCall`, `DenyToolCall`, and `ProvideUserInput` route
  replies to `route.LoopID`.
- `AgentSession` receives events from primary and subagent loops through the
  same session fan-in publisher.
- Closing or abandoning a parent turn event stream does not close the session
  fan-in and does not drop events from another loop.
- Event headers identify the producing `sessionID + loopID + turnID + stepID`;
  session-scoped events set only `sessionID`; optional parent ids are used only
  for display/correlation grouping.
- Every concrete event reports `Scope()`, `Class()`, and `EndsTurn()`; embedding
  any two lifecycle mixins (`ephemeral`/`enduring`/`terminal`) or any two scope
  mixins (`sessionScoped`/`loopScoped`) fails to compile (ambiguous selector).
- `TurnDone`/`TurnFailed`/`TurnInterrupted` report `EndsTurn() == true`; all other
  events (including `StepDone` and `LoopIdle`) report `false`.
- A consumer driven by `EndsTurn()` closes on exactly one terminal per turn;
  `StepDone` and `LoopIdle` do not end the stream.
- A loop chaining turn N → N+1 emits turn N's terminal but no `LoopIdle` between
  them; `sessionState.active` keeps the `{loop, …}` key across the chain (idempotent).
- `sessionState.active` empties iff every busy loop has emitted `LoopIdle`;
  `SessionIdle` fires once per `Active → Idle` edge. Synchronous model: that edge
  is the primary loop going idle.
- `SessionActive` fires once per `Idle → Active` edge — the first `TurnStarted` out
  of idle, or an `expectTurn` taken while idle — symmetric with `SessionIdle`;
  neither fires while `phase == SessionStopped`.
- A `UserInput` to an idle loop returns `TurnStarted{TurnID, InputID}` (+
  `event.TurnStarted{InputID, Message, Header.CausationID=InputID}`); to a
  running queueable loop returns
  `InputQueued{InputID}` with no `TurnID`; to a non-queueable/full/shutting-down
  loop returns `TurnRejected{Reason: RejectBusy|RejectQueueFull|RejectShuttingDown}`.
- A `CancelQueuedInput` while the message is still queued returns `Cancelled` (+
  `InputCancelled{Reason: CancelClientRetracted}`); after it starts or folds,
  `AlreadyCommitted`. A turn that interrupts/fails with a queued message emits
  `InputCancelled{Reason: CancelTurnInterrupted|CancelTurnFailed, Message}` and
  starts no new turn.
- With a `{wake, subagentLoopID}` token outstanding, `active` is non-empty even when
  every loop reports `LoopIdle`; the token releases on publish-path
  `TurnStarted`/`TurnFoldedInto`/`InputCancelled` carrying `TriggeredByLoopID`, or
  on the session reply path when a `TurnRejected` disposition calls
  `cancelExpectTurn`. `SessionIdle` fires only after that — including when the
  result was folded (no `TurnStarted`).
- `TokenDelta` is the only Ephemeral event; `StepDone`, terminals, gates, and
  tool-lifecycle events are Enduring.
- `PublishEvent` with no subscribers still applies `sessionState` transitions,
  skips delivery, returns nil, and never blocks.
- A slow Ephemeral subscriber drops `TokenDelta`s without blocking the loop or
  other subscribers; an Enduring overflow fails that subscription with a typed loss
  error returned by `EventSubscription.Err()` (closed), never silently dropping
  the event.
- `PublishEvent` delivers outside the lock (write lock for event-derived
  active/phase transitions, read lock otherwise): a subscriber that blocks on
  receive does not stall `SubscribeEvents` or another loop's publish.
- Submit/cancel/gate reply channels are buffered with capacity 1 and delivered
  through the single non-blocking helper `tryAck[T]`, so an abandoned caller or
  contract violation cannot wedge a loop goroutine.
- A queued `UserInput` is retractable: `CancelQueuedInput` while queued → removed
  (`Cancelled`); after it starts/folds → `AlreadyCommitted`. At `inboxCap`
  a further submit → `TurnRejected{QueueFull}` (a length check; the actor never
  blocks). The queue is `loopState.inbox`, owned by the actor.
- A turn that fails/interrupts at step N keeps steps 1..N-1 committed in
  `loopState.msgs` (with their `StepDone`s) and discards only step N's in-flight
  state.
- A `CancelQueuedInput` racing a tool-continuation drain resolves deterministically
  on the actor: `Cancelled` if still queued, `AlreadyCommitted` if already drained
  into the active turn.
- An input queued while a no-tool final answer is in flight is not pulled into that
  completed turn; after `TurnDone`, the actor starts a new turn from the first
  queued entry.
- An `Interrupt` while `runTurn` is parked in `drainPending` frees it (the handshake
  selects on `turnCtx.Done`), rather than wedging.
- A turn started from a fan-in-only `UserInput` (nil `Events`) emits to fan-in/sinks
  only; `emit` and `deliverAndClose` neither send-on-nil nor close-nil.
- An `EventFilter` of primary-only Ephemeral + all-loop Enduring delivers a
  subagent's `StepDone`, never its `TokenDelta`, and always delivers
  session-scoped `SessionStarted`/`SessionActive`/`SessionIdle`/`SessionStopped`.
- A dropped `TokenDelta` is reconciled by the subsequent `StepDone.Messages`; the
  rendered group equals the loop's stored `AIMessage` + its `ToolResultMessage`s.
- Each step emits exactly one `StepDone` carrying its finalized group (`AIMessage`
  + its `ToolResultMessage`s).
- `stepState` cannot finalize an empty assistant response, and its `msgs` starts
  with exactly one `AIMessage`.
- `stepState` appends only `ToolResultMessage`s after the assistant message.
- `turnState` starts with exactly one initial `UserMessage` and appends complete
  `stepState.msgs` groups.
- `loopState.inbox` (actor-owned) stores multiple queued messages in order until
  each message starts, folds, is cancelled, or is returned.
- `runTurn` calls `cfg.drainPending(ctx)` only after a complete tool-using step, never
  between tool results from the same assistant message and never after a no-tool
  final answer.
- Pending user messages do not force another LLM request after a no-tool final
  answer; they start later turns unless they are folded at a tool-continuation
  boundary first.
- Active-turn runtime handles (`events`, `abandoned`, `cancel`) and
  loop-goroutine-owned `pendingGates` live on `turn`, not directly on
  `loopState`.
- Gate replies carry `sessionID + loopID + turnID + stepID + toolCallID`: the
  session layer chooses the loop, and the loop goroutine resolves the active turn
  gate by `toolCallID`.
- `runTurn` rolls back at **step** granularity: a failed/interrupted turn discards
  only the in-flight incomplete step; steps already committed (and their `StepDone`s)
  remain in loop history. (This supersedes the old whole-turn rollback.)
- A single step with text plus tool use stores one `AIMessage` containing both a
  `TextBlock` and `ToolUseBlock`.
- A tool-using turn with multiple LLM responses produces one `UserMessage`,
  multiple `AIMessage`s, and matching `ToolResultMessage`s in order.
- A continuation-queued turn can produce `UserMessage, AIMessage, ToolResultMessage,
  UserMessage, AIMessage` within one `turnState.msgs`.
- TUI live accumulation and loop step accumulation use the same
  `streamaccumulator` helpers for text, thinking, and tool-use chunks.
- Run with `go test -race ./...`.

## Implementation Order

1. Define the complete event API first: `Header`, `Scope()` (`ScopeSession`/
   `ScopeLoop`), `Class()`, `EndsTurn()`, the `sessionScoped`/`loopScoped` mixins,
   the `ephemeral`/`enduring`/`terminal` mixins, and every concrete event shape
   (`SessionStarted`, `SessionActive`, `SessionIdle`, `SessionStopped`, `LoopIdle`,
   turn/step/input events, and gate/tool lifecycle events). Stamp
   `Header.SessionID` on every event; stamp
   `LoopID`/`TurnID`/`StepID` only for loop-scoped events; stamp
   `Header.CausationID` on submit-resolution events from the submit command id.
   Validate that session-scoped events set only `SessionID`.
2. Rename `content.ToolMessage` to `content.ToolResultMessage` and update JSON
   tests, provider encoding, loop code, and turn tests.
3. Update the session/loop construction boundary so `NewAgent` mints
   `sessionID`, initializes `loops`, calls `s.NewLoop(loop.Provenance{}, cfg)` for
   the primary loop, stores the returned loop id under `primaryLoopID`, and
   `NewLoop` calls `loop.New(loopCtx, sessionID, loopID, parent, s, cfg)` with
   `loopCtx` derived from `sessionCtx`.
4. Add the session event fan-in as a publish/subscribe contract: `AgentSession`
   implements `eventPublisher`/`eventSubscriber`; add `EventFilter` (class x
   producer `LoopID` for `ScopeLoop`, deliver all `ScopeSession` events to every
   subscriber). `PublishEvent` snapshots subscribers, delivers outside the lock,
   drops Ephemeral on overflow and fails the subscription (`SubscriptionLossError`
   from `EventSubscription.Err()`) on Enduring overflow; with no subscribers it
   still applies event-derived `sessionState` transitions and only skips delivery.
   Per-turn streams remain caller-facing for their own turn; the redacted
   `cfg.Sinks`/`EventEnvelope` path is unchanged. Add `LoopIdle`, `SessionActive`,
   `SessionIdle`, `SessionStopped`, `ErrSessionStopped`, and the single
   `sessionState` (`active` `{kind,id}` set + `SessionPhase`). The shared
   `applyActivity` edge check (used by `PublishEvent` and by session methods
   `expectTurn`/`cancelExpectTurn`) derives `SessionActive` on `Idle → Active` and
   `SessionIdle` on `Active → Idle`; `stopSession` forces `SessionStopped`. All
   mutate `active`/`phase` under the same hub lock. `WaitIdle` wakes on
   `Active → Idle` and returns `ErrSessionStopped` after stop; post-stop publishes
   deliver late events without mutating `active`/`phase`.
5. Add `internal/content/streamaccumulator` with `Thinking`, `Text`, and
   `ToolUses`; move the existing `toolAccumulator` logic out of `turn.go`.
6. Introduce `blockState` and make step streaming fold chunks through the shared
   accumulator helpers while preserving `TokenDelta` emission from the loop chunk
   layer.
7. Introduce `stepState` so one LLM response finalizes into
   `content.AgenticMessages` starting with one `AIMessage`, and emit the Enduring
   `StepDone{Messages}` at step completion carrying the finalized group (`AIMessage`
   + its `ToolResultMessage`s) — the self-heal anchor that makes dropped
   `TokenDelta`s recoverable and is the only authoritative carrier of tool results.
8. Introduce `turnState` so `runTurn` stages one user turn from
   `turnConfig.base + turnState.msgs`, while `runLoop` owns the commit handshake:
   initial `UserMessage` + `TurnStarted{Message, Header.CausationID}` before
   `runTurn`, folded `UserMessage` + `TurnFoldedInto` at tool-continuation
   boundaries, and completed step groups + `StepDone` at step completion.
9. Reframe input as one submit command: replace `StartTurn`/`SteerTurn` with
   `command.UserInput` (`Mode`/optional `Events`+`Abandoned`) and
   `command.SubagentResult`, returning a typed `Disposition`
   (`TurnStarted`/`InputQueued`/`TurnRejected`). `runLoop` decides on its own
   `status`: idle → start a turn (`event.TurnStarted{InputID, Message,
   Header.CausationID=InputID}`); running & queueable → append to the actor-owned
   `loopState.inbox`
   (`InputQueued{InputID}`), drained + folded only at tool-continuation boundaries
   via `cfg.drainPending` (`event.TurnFoldedInto`); normal no-tool completion
   leaves queued input for later turns; at `inboxCap` or non-queueable →
   `TurnRejected`. Drop the `steer` push channel. Add
   `command.CancelQueuedInput` (→ `Cancelled`/`AlreadyCommitted`) and
   `event.InputCancelled{Reason, Message}` for retract and interrupt/fail-return.
   Add buffered-cap-1 reply channel discipline and the single non-blocking
   `tryAck[T]` send helper for `UserInput`, `SubagentResult`, `CancelQueuedInput`,
   and `gate.reply`. Add
   `LoopIdle`/`SessionActive`/`SessionIdle`/`SessionStopped`, the `sessionState`
   quiescence aggregation (one `RWMutex`; write lock for active/phase-mutating
   operations),
   and the `expectTurn` token released on a hand-back turn's `TurnStarted`/
   `TurnFoldedInto`/`InputCancelled` via `Header.TriggeredByLoopID`, or on a
   `TurnRejected` disposition by session-owned `cancelExpectTurn`.
10. Rename `listen` to `runLoop` and reshape the loop goroutine around explicit
   `loopConfig` and `loopState` values so it owns loop id, session id, committed
   messages, status, and turn lifecycle dependencies without adding another
   wrapper type.
11. Update TUI/CLI live display to use `streamaccumulator` for provisional
   assistant messages, then render the Enduring `StepDone.Messages` group as the
   committed per-step messages (replacing locally accumulated text, so displayed ==
   stored). Subscribe via `SubscribeEvents(EventFilter)` for the whole session
   instead of opening a per-turn stream, so multi-loop events are attributable.
   The full display-layer change set is its own spec:
   `docs/plans/2026-06-18-tui-event-adoption-design.md`.

## Open Items & Follow-Ups

Tracking checklist for points raised in design review that are intentionally
**not** closed in the spec body. Group A must be verified before/during
implementation of this spec; Group B is owned by named follow-on specs but is
listed here so it is not lost. Check items off as they are resolved.

### A. Verify before/during implementation

- [ ] **Async-subagent spawn must take `expectTurn` before the child can finish.**
  `expectTurn(subagentLoopID)` is added "at spawn", but `NewLoop` does not call it
  today and the async-spawn path is deferred orchestration. When that path lands,
  the `{wake, s}` token must exist *before* the subagent could possibly go
  `LoopIdle` with a pending hand-back — otherwise `active` can momentarily empty
  and fire a false `SessionIdle`. (See *Subagent hand-back* + *Routing*.)

- [ ] **Specify concrete typed errors for the loop handshakes.** `commit`,
  `drainPending`, `PublishEvent`, and `SubscribeEvents` are shown returning bare
  `error`. Per the typed-error rule, define a concrete error struct (or documented
  sentinel) per distinct failure mode before implementing. (See *Turn*, *Event
  Fan-In*.)

- [ ] **Rename one of the two `TurnStarted` types.** The `Disposition` variant
  `TurnStarted{TurnID, InputID}` and the `event.TurnStarted{…Message}` share a name
  across packages — a readability/maintenance trap. Pick a distinct name for the
  disposition variant (e.g. `Started`). (See *Routing*, *Event API*.)

- [x] **Validate the thin wrapper types against real code.** Resolved in Phase 10.
  `block` (`block{state blockState}`) and `step` (`step{state stepState}`) were
  one-field structs with no methods and no runtime role beyond holding their state —
  every caller reached straight through to the inner state — so both were
  **collapsed** (YAGNI): `runStep` takes `stepState` directly and `feedBlock` builds
  `&blockState{}`. `chunkProcessor` is **kept**: it owns the per-chunk "emit
  TokenDelta THEN accumulate" ordering (a real behavior, not just state), so it earns
  its place. `turn` is **kept**: it is the real runtime type (events/abandoned/cancel/
  pendingGates), inlined in the actor-owned loopState. (See *Block*, *Chunk*, *Step*.)

- [ ] **Keep restated lists in sync (single-source drift).** The session-scoped
  event set, the Enduring set, and the quiescence transition rules are each
  restated across several sections (Scope, Event API, Invariants, Testing,
  Implementation Order). A change to one must be propagated to all; consider
  generating the invariant/testing lists from one canonical table.

- [x] **Add `SessionInterruptCanceled` to the `SessionError` kind set.** Reviewed in
  Phase 10. The implemented `Interrupt`'s ctx-done escape already returns the existing
  `SessionContextDone` kind (shared with every other session command send), and no
  code references `SessionInterruptCanceled`. Rather than add a second kind for the
  same failure mode, the error set is left as-is; `SessionInterruptCanceled` was a
  spec-example name, not a distinct implemented failure. (See *Routing*.)

### B. Owned by follow-on specs (tracked, not lost)

- [ ] **Concurrent hand-backs from one persistent subagent loop.** The
  `{wake, subagentLoopID}` set key assumes at most one in-flight hand-back per loop;
  a second concurrent hand-back collides on the key. Deferred to the cross-turn
  handle model (`wait`/`send`/`interrupt`). (See *Subagent hand-back*.)

- [ ] **Journal serialization of `error`/content payloads.** `TurnFailed.Err` is an
  `error` interface (no clean wire form) and content-bearing events
  (`TurnStarted.Message`, `StepDone.Messages`, …) need a durable shape. The journal
  spec must project these (e.g. a typed code/string for errors). (See *Redaction
  deferred and risk accepted*, *Scope → Out*.)

- [ ] **Redaction of new content-bearing events.** Accepted risk: new events may
  reach existing best-effort sinks unredacted. The journal/redacting-subscriber
  follow-on owns re-homing `Redactable`/`SinkProjection` and retiring
  `EventEnvelope`. (See *Redaction deferred and risk accepted*.)

- [ ] **Field-name normalization.** Apply
  `docs/plans/2026-06-18-id-normalization-design.md` to `InputID`, `CallID`,
  `ToolCallID`, and `Header.CausationID` before implementation. (Cross-referenced
  in *Event API*.)
