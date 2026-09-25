# pkg/session

`pkg/session` exposes the live **data-plane and control-plane contracts**
for one running rig. It is the surface a consumer (TUI, CLI, HTTP, test)
holds and drives; session construction and restoration are owned
exclusively by [`pkg/rig`](../rig/README.md).

Operation hooks are also installed at the rig boundary, not on a live session.
Pass a [`hook.Set`](../hook/README.md) with `rig.WithHooks` when defining the
rig. Every new or restored session then uses that immutable compiled policy for
new native-loop work and checked journal appends; sessions expose no mutable
hook registry.

It is a contracts package — it defines interfaces, not implementations.
The concrete `Session` lives in `internal/sessionruntime` and is returned
to callers as `session.SessionController`.

## What is session?

Two session views live here, layered by trust, plus a set of segregated
capabilities a consumer discovers by type assertion:

- **`Session`** — the ordinary data plane: identity, the active loop, the
  loop registry, submit, compact, subscribe, respond to a gate,
  interrupt.
- **`SessionController`** — the trusted policy and lifecycle view. It
  embeds `Session` and adds `SetActiveLoop`, `LoopController`,
  `CheckpointWorkspace`, `RestoreWorkspace`, `Shutdown`.
- **`GateHost`** — the capability to raise a **host-owned** gate (a form
  or an out-of-band URL) and wait on its answer. A separate contract from
  `SessionController`, because opening a gate is not part of running a
  session and the two roles have different holders (an MCP binding
  servicing an elicitation is a `GateHost`; the client that answers is a
  `Session`).

### Segregated capabilities

None of these is a method on `Session` or `SessionController`. Each is
obtained by asserting on the value `rig` returns, and a `false` result
means "this session cannot do that", never an error. The argument for
keeping each one out of the two views is the same one spelled out for
`GateHost` below.

- **`CommittedPublicEventSource`** / **`CommittedPublicEventProvider`** —
  a live event stream whose every delivery carries the exact canonical
  bytes the durable append stored. Discovered through the *provider*
  rather than a bare assertion, because the capability is a property of
  the session's persistence, not of its Go type.
- **`IdleWaiter`** — `WaitIdle`, whole-session quiescence. Most
  controller consumers submit work; a supervisor that waits usually does
  not submit.
- **`Liveness`** — `Done() <-chan struct{}`, closed when teardown
  *begins*. A broadcast a drain supervisor can `select` on, deliberately
  not an `Alive(ctx) error` poll. It is the same interface as
  `pkg/serve.SessionDone`, duplicated because `serve` does not import
  this package.
- **`Releaser`** — `ReleaseResidency`, giving up this process's resident
  runtime while leaving the logical session restorable. Named in full
  because it is **nonterminal**: unlike `Shutdown` it appends no
  `SessionStopped`. **The live runtime does not implement it yet**, so
  the assertion returns `false` today; see task H4.2.
- **`InputSubmitter`** — `SubmitInput(ctx, session.Input{Blocks, Principal,
  Metadata})` submits attributed user input to the active loop. The composition
  root verifies the principal and owns its truth; Harness validates its shape.
  An empty message or invalid attribution is refused before any send. An
  installed Message Presenter may frame it; `Submit` remains unchanged and
  presents with a nil principal.

Everything else exported here is an error type. `pkg/session` is
contracts plus errors, and a test enforces exactly that.

## How to use

```go
// From pkg/rig:
session, err := r.NewSession(ctx)
if err != nil { return err }
defer session.Shutdown(ctx)

// Submit user input to the primary loop.
inputID, err := session.Submit(ctx, []content.Block{
    &content.TextBlock{Text: "Summarize README.md."},
})
if err != nil { return err }

// Or target a specific loop (e.g. a delegate).
_, err = session.SubmitToLoop(ctx, reviewerLoopID, blocks)

// Subscribe to the event stream before submitting so no event is missed.
sub, err := session.SubscribeEvents(nil)
go func() {
    for delivery := range sub.Events() {
        handleEvent(delivery.Event)
        if delivery.Event.EndsTurn() { /* ... */ }
    }
}()

// Answer a permission gate raised by a tool.
session.RespondGate(ctx, gate.GateResponse{
    GateID: openGateID,
    Action: gate.ApproveActionApprove,
})

// Trust a host-owned gate (a form an MCP integration raised, e.g.):
host, ok := session.(session.GateHost)
if ok {
    id, _ := host.OpenHostGate(ctx, loopID, gate.KindForm, payload)
    answer, _ := host.AwaitGateAnswer(ctx, id)
    _ = answer
}
```

## Sibling packages

- [`pkg/rig`](../rig/README.md) — the composition root that returns a
  `SessionController`.
- [`pkg/loop`](../loop/README.md) — the `loop.Handle` / `loop.Controller`
  values the session exposes.
- [`pkg/event`](../event/README.md) — `event.EventFilter` and
  `event.Subscription` for `SubscribeEvents`.
- [`pkg/gate`](../gate/README.md) — `gate.GateResponse` for `RespondGate`
  and `gate.Gate`/`gate.Payload` for `OpenHostGate`.
- [`pkg/hook`](../hook/README.md) — rig-installed guards and around observers
  for native runtime operations and checked journal appends.
- [`pkg/workspacestore`](../workspacestore/README.md) — `workspacestore.Ref`
  for `CheckpointWorkspace` / `RestoreWorkspace`.

## How it is designed

The session is a **coordinator with no goroutine**. It owns a `*hub.Hub`,
a loop registry, the journal, and the workspace placement; methods
serialize access with normal RWMutexes. The loops are the actors; the
session dispatches commands to them and fans their events back in through
the hub.

```
                       Consumer (TUI / CLI / HTTP / test)
                                  │
                                  │  Submit / SubmitToLoop / SubscribeEvents
                                  │  RespondGate / Interrupt / Shutdown
                                  ▼
                ┌─────────────────────────────────────────────────┐
                │ SessionController (this package — contracts)     │
                │  • Submit → command.UserInput → active loop       │
                │  • SubmitToLoop → command.UserInput → target loop │
                │  • RespondGate → command.ApproveToolCall / Deny…  │
                │  • Interrupt → priority Interrupt to every loop   │
                │  • Shutdown → priority Shutdown, drain, close     │
                └──────┬────────────────────────┬───────────────────┘
                       │                        │
            publish    │                        │  dispatch
                       ▼                        ▼
                ┌──────────────┐         ┌────────────────────┐
                │  pkg/hub     │ ◀────  │ Loop actor          │
                │  (fan-in)    │  publish │ (internal/         │
                │              │         │  loopruntime)       │
                └──────┬───────┘         └────────────────────┘
                       │
                       ▼
                Subscribers (TUI / SSE / journal / tests)
```

### Why `GateHost` is separate from `SessionController`

Two independent reasons, both from the interface-segregation rule:

1. **Coupling.** Almost every `SessionController` consumer — TUI, CLI,
   test — submits work, watches events, and answers gates. Almost none
   *raise* one. Widening `SessionController` would force every
   implementation to grow three methods only an integration host calls.
2. **Holder.** A `SessionController` is the session's *operator*; a
   `GateHost` is whatever opened a particular gate and is blocked on its
   answer — an MCP binding servicing an elicitation, say. The two ends of
   the same gate should not collapse into one god-interface.

A live session implements `GateHost`, so a host obtains one by asserting:
`host, ok := controller.(session.GateHost)`. The contract is host-owned
gates only (`gate.KindForm`, `gate.KindOpenURL` with
`gate.ResolverSession`); there is deliberately no way to mint a permission
or ask-user gate through it, because a host that could mint one could
park — or forge an approval against — a loop that is not its own.
