# looprig/harness

`github.com/looprig/harness` is the agent runtime module of the looprig
ecosystem: a Go library that turns an `inference.Client` and a set of tools
into a durable, observable, permissioned agentic loop. It owns the
**contracts and the runtime** — composition, sessions, loops, turns, steps,
events, commands, gates, journal, restore — and deliberately leaves leaf
capabilities (LLM providers, storage backends, tool implementations, OS
confinement, foreign-loop subprocesses, the terminal presenter) to sibling
modules wired at the consumer's composition root.

It is a library, not a binary. A consumer — a TUI, a CLI, an HTTP service, a
test — assembles a `Rig`, brings up a `Session`, drives it through the
`Session` contract, and reads the event stream. Everything is typed; nothing
flows through `any` or `interface{}` past a serialization boundary.

## What is harness?

A **Rig** is a design-time assembly: one or more **Loop** definitions (an
inference client + model + tools + access gate + modes + delegation policy),
a session store, primers, and a workspace placement. A **Session** is one
live execution of a rig — durable from the moment it starts, restorable by
id. Inside a session, **Loops** are actors: each is one goroutine that owns
its mutable state and command ordering. A **Turn** is one user-input →
assistant-reply cycle through the model, and a **Step** is one
model-invocation → tool-batch round-trip inside a turn.

The runtime is built around four ideas:

1. **Actors, not locks.** Each loop is one goroutine; commands travel on a
   `chan command.Command`. State mutations happen only inside the actor.
2. **One event stream per session.** Loops publish events into a `Hub`; the
   hub fans them out to subscribers (TUI, journal, HTTP SSE) by filter. The
   hub also owns federated quiescence so a headless runner can `WaitIdle`
   without a session goroutine.
3. **Typed prepared requests.** Permission is never decided by parsing tool
   arguments. Each tool decodes, normalizes, and validates its own
   arguments and produces a `tool.Request`; the `gate.Evaluator` decides
   `Deny` / `Gated` / `Allow` on the typed request.
4. **Durable by construction.** Every command and enduring event flows
   through one `journal.SessionJournal` per session: a totally-ordered,
   gap-free append-only log. Restore replays it; foreign-loop backends
   recover their own session ids from it.

## How to use harness

Compose a `Rig`, bring up a `Session`, drive it, read the event stream,
answer gates.

```go
package main

import (
    "context"
    "log"

    "github.com/looprig/core/content"
    "github.com/looprig/harness/pkg/gate"
    "github.com/looprig/harness/pkg/loop"
    "github.com/looprig/harness/pkg/rig"
    "github.com/looprig/harness/pkg/sessionstore"
    "github.com/looprig/inference"
    "github.com/looprig/inference/model"
)

func main() {
    store, err := sessionstore.Open(/* a *storage.Composite from a backend module */)
    if err != nil { log.Fatal(err) }

    agent, err := loop.Define(
        loop.WithName("operator"),
        loop.WithClient(inferenceClient),
        loop.WithModel(model.Model{ /* provider, name, sampling */ }),
        loop.WithTools(/* ...tool.Definition values from looprig/tools */...),
        loop.WithAccessGate(/* a gate.Evaluator built from your sandbox */),
    )
    if err != nil { log.Fatal(err) }

    r, err := rig.Define(
        rig.WithLoops(agent),
        rig.WithSessionStore(store),
        rig.WithPrimers("operator"),
    )
    if err != nil { log.Fatal(err) }

    ctx := context.Background()
    session, err := r.NewSession(ctx)
    if err != nil { log.Fatal(err) }

    // Subscribe to the event stream before submitting so no event is missed.
    sub, err := session.SubscribeEvents(nil /* = all events */)
    if err != nil { log.Fatal(err) }
    go func() {
        for delivery := range sub.Events() {
            handle(delivery)
            if delivery.Event.EndsTurn() { /* ... */ }
        }
    }()

    // Submit a user turn. The outcome arrives on the event stream,
    // correlated by the returned input id.
    if _, err := session.Submit(ctx, []content.Block{
        &content.TextBlock{Text: "Read README.md and summarize it."},
    }); err != nil { log.Fatal(err) }

    // Answer a permission gate raised by a tool.
    //   session.RespondGate(ctx, gate.GateResponse{ GateID: id, Action: gate.ApproveActionApprove })

    _ = session /* call Shutdown when done */
}
```

For HTTP consumers, `pkg/serve` wraps a `Rig` and its sessions behind a
narrow HTTP surface (submit, subscribe via SSE, respond to a gate,
interrupt). For terminal consumers, the sibling `looprig/tui` module binds
against the same `Session` contract.

## Sibling modules

Harness is one module in a larger ecosystem. See
[`docs/ECOSYSTEM.md`](docs/ECOSYSTEM.md) for the full map; the short version:

- [`looprig/core`](https://github.com/looprig/core) — `content.Block`,
  `uuid`, the shared primitives.
- [`looprig/inference`](https://github.com/looprig/inference) —
  `inference.Client`, `model.Model`, streaming, structured output, context
  counters.
- [`looprig/storage`](https://github.com/looprig/storage) — `Ledger`,
  `Leaser`, `KV`, `Blobs` leaf contracts.
- [`looprig/eval`](https://github.com/looprig/eval) — the evaluation
  framework that runs under `go test`.
- [`looprig/foreignloops`](https://github.com/looprig/foreignloops) —
  `codex` and `claude` subprocess backends behind `pkg/foreign`.
- [`looprig/tools`](https://github.com/looprig/tools) — bash, web, and the
  other standard tool implementations.
- [`looprig/sandbox`](https://github.com/looprig/sandbox) — OS confinement
  that satisfies `gate.AccessSource` / `gate.GrantIssuer`.
- [`looprig/tui`](https://github.com/looprig/tui) — the terminal presenter.
- [`looprig/mcp`](https://github.com/looprig/mcp) — MCP client and the
  harness integration that publishes `IntegrationStatus`.
- [`looprig/fsstore`](https://github.com/looprig/fsstore) /
  [`looprig/natsstore`](https://github.com/looprig/natsstore) /
  [`looprig/rclonestore`](https://github.com/looprig/rclonestore) —
  `storage.Composite` backends.

## How harness is designed

### Layered packages

```
pkg/rig ──────► pkg/session ──────► pkg/loop ──────► pkg/tool
   │              │                   │
   │              │                   ├──► pkg/gate      (three-state access decision)
   │              │                   ├──► pkg/event     (sealed event union)
   │              │                   ├──► pkg/command   (sealed command union)
   │              │                   ├──► pkg/identity  (coordinates, cause, agency)
   │              │                   └──► pkg/foreign   (foreign-loop builder seam)
   │              │
   │              ├──► pkg/hub           (event fan-in, federated quiescence)
   │              ├──► pkg/hustle        (parallel background work)
   │              ├──► pkg/journal       (single-writer durable log contract)
   │              ├──► pkg/sessionstore  (session-scoped storage facade)
   │              └──► pkg/workspacestore (workspace snapshots)
   │
   └──► pkg/serve  (HTTP surface; depends only on narrow LiveSession + Rig seams)

internal/loopruntime    private loop actor, turn, step, runner
internal/sessionruntime private session coordinator (owns loops, hub, journal)
internal/hustleruntime  private hustle scheduler lanes
internal/delegationtool  the harness-owned delegation tool
internal/registry        generic name→constructor registry
internal/hashcache       SHA-256-keyed parse cache
internal/pathutil        canonical filesystem path normalization
internal/buildtest       shared build/lint test helpers
```

`pkg/*` is the public surface; `internal/*` is the private implementation.
Public packages depend only on contracts (interfaces and value types); the
concrete wiring lives at the composition root in `internal/sessionruntime`
and `internal/loopruntime`. The result is that a consumer can substitute any
leaf (a different inference client, a different storage backend, a different
sandbox) without harness importing it.

### The actor model

Each `Loop` is one goroutine owning its mutable state. A `Session` has **no
goroutine** — it is a coordinator that owns a `Hub`, a loop registry, and a
journal. Commands travel on channels; events travel back through the hub.

```
Consumer (TUI / CLI / HTTP / test)
  │
  │ Session.Submit / SubscribeEvents / RespondGate / Interrupt / Shutdown
  ▼
┌─────────────────────────────────────────────────────────────────────┐
│ Session (pkg/session) — coordinator, no goroutine                   │
│  • owns one Hub (pkg/hub)                                            │
│  • owns the loop registry + active loop                              │
│  • routes gate responses to loops by GateID/LoopID/ToolExecutionID   │
│  • serializes loop lifecycle (Interrupt/Shutdown on a priority lane) │
└───┬───────────────────────────────────────────────────────────┬─────┘
    │ publish                                                    │ spawn
    ▼                                                           ▼
┌──────────────┐                                      ┌────────────────────┐
│   Hub        │  ◀── publish event ─── from loop ──  │ Loop actor          │
│  (pkg/hub)   │                                       │ (internal/          │
│              │                                       │  loopruntime)       │
│  pub/sub     │                                       │  commands chan      │
│  filter      │                                       │  state machine:     │
│  WaitIdle    │                                       │   idle → running    │
│              │                                       │         → shuttingDown
└──────┬───────┘                                       │  priority lane for  │
       │                                                │  Interrupt/Shutdown │
       │ events                                         └──────┬─────────────┘
       ▼                                                      │ per turn
┌──────────────┐                                              ▼
│ Subscribers  │                                      ┌────────────────────┐
│  TUI / SSE / │                                      │ Turn runner        │
│  journal /   │                                      │ (goroutine)        │
│  tests       │                                      │  events chan       │
└──────────────┘                                      │  LLM stream        │
                                                       │  tool batch        │
                                                       └──────┬─────────────┘
                                                              │
                                                              ▼
                                              inference.Client  +  tools
                                              (looprig/inference)  (looprig/tools)
                                                                     │
                                                                     ▼
                                                          pkg/gate  →  sandbox
                                                          (D/G/A)      (looprig/sandbox)
```

The full turn-level picture — including how an `Interrupt` is delivered, how
shutdown drains, and why there is no turn queue in v1 — is in
[`docs/architecture/agent-loop.md`](docs/architecture/agent-loop.md).

### Commands, events, and the journal

- **Commands** (`pkg/command`) are a sealed interface: only types in that
  package can implement `Command`. Submit commands (`UserInput`,
  `SubagentResult`, `CancelQueuedInput`) are fire-and-forget — their outcome
  is **published as a typed event**, never replied on a per-command channel.
  Control commands (`Interrupt`, `Shutdown`) carry a buffered `Ack` channel
  so the actor's send never stalls.
- **Events** (`pkg/event`) are a sealed union: every concrete event satisfies
  `event.Event` and is asserted at compile time in `pkg/event/doc.go`. Each
  event embeds exactly one lifecycle mixin (`ephemeral` / `enduring` /
  `terminal`) and one scope mixin (`sessionScoped` / `loopScoped`).
  Ephemeral streaming events (`TokenDelta`, `ToolCallStarted`, …) are never
  persisted; enduring control and workspace events are the durable replay
  inputs.
- **Journal** (`pkg/journal`) is one serialized writer per session. Every
  command and every enduring event flows through `SessionJournal.Append` so
  the log stays totally-ordered and gap-free. Restore replays it; foreign
  loops recover their session ids from it.

### The gate (permission model)

A tool call is never evaluated by parsing arguments. Each tool owns a
`CallPreparer` that decodes, validates, and normalizes its arguments into a
typed `tool.Request` (a list of `tool.Requirement` values, each with a Kind,
Scope, Match, Description, and optional GrantClass/GrantTarget). The
`gate.Evaluator` then runs a strict order:

1. **Configured access first.** Every requirement is routed to its bound
   `AccessSource`. `Deny` short-circuits; `Allow` needs no grant token;
   `Gated` continues.
2. **Every stored deny before any allow.** Each gated requirement is checked
   against `RuleMatcher.MatchesDeny`; a match denies the call.
3. **Stored allows.** A gated requirement matched by `MatchesAllow` is met;
   the rest form **one combined unmet set** with the displayed reusable
   candidates.

The whole unmet set is resolved by **one combined approval** with exactly
three actions: `Approve` (once), `Approve always for this workspace`
(persists the displayed candidates atomically before any grant is minted),
or `Deny`. The runtime never opens a second prompt for the same call, never
invents a session/global scope, and never persists a partial approval
silently. See [`pkg/gate/README.md`](pkg/gate/README.md).

### Concurrency contracts in one place

- **One goroutine per loop** owns mutable state; everything else is
  lock-free or holds narrow locks for snapshot copy.
- **One goroutine per turn** owns the staged turn conversation; the actor
  never touches it.
- **One bounded egress channel per subscriber** (default 256). A slow
  subscriber never blocks a publisher or another subscriber; on overflow,
  ephemeral events are dropped and enduring events fail the subscription
  with a typed `SubscriptionLossError` so the subscriber can re-subscribe
  and re-sync.
- **One serialized writer per journal.** A failed append is a typed
  `*EvaluationError` (fail-closed); the session aborts construction rather
  than wedge the writer.
- **No unbounded blocking.** Every I/O call takes a `context.Context`; the
  hub, journal, gate, and tool runner all carry per-call deadlines
  independent of the caller's context.

## Project layout

```
harness/
├── pkg/                  public contracts and runtime surfaces
│   ├── command/          sealed command union (pkg/command/README.md)
│   ├── evalmigration/    build-tagged proof that legacy eval re-expresses against looprig/eval
│   ├── event/            sealed event union (pkg/event/README.md)
│   ├── foreign/          foreign-loop builder seams (pkg/foreign/README.md)
│   ├── gate/             three-state access decision (pkg/gate/README.md)
│   ├── hub/              session event fan-in (pkg/hub/README.md)
│   ├── hustle/           parallel background work definitions (pkg/hustle/README.md)
│   ├── identity/         coordinates, cause, agency (pkg/identity/README.md)
│   ├── journal/          single-writer durable log contract (pkg/journal/README.md)
│   ├── loop/             immutable loop recipes + live loop contracts (pkg/loop/README.md)
│   ├── rig/              composition root (pkg/rig/README.md)
│   ├── serve/            HTTP surface over a live session (pkg/serve/README.md)
│   ├── session/          live session data-plane + control-plane (pkg/session/README.md)
│   ├── sessionstore/     session-scoped storage facade (pkg/sessionstore/README.md)
│   ├── tool/             dependency-free tool contracts (pkg/tool/README.md)
│   └── workspacestore/   workspace snapshots over storage.Blobs (pkg/workspacestore/README.md)
├── internal/             private implementation
│   ├── loopruntime/       loop actor, turn, step, runner
│   ├── sessionruntime/    session coordinator (owns loops, hub, journal, restore)
│   ├── hustleruntime/     hustle scheduler lanes
│   ├── delegationtool/    harness-owned subagent delegation tool
│   ├── registry/         generic name→constructor registry
│   ├── hashcache/        SHA-256-keyed parse cache
│   ├── pathutil/         canonical path normalization
│   └── buildtest/        build/lint test helpers
├── docs/                 architecture, plans, releases, ecosystem
├── vendor/               audited vendored dependency tree
├── scripts/              build/lint helper scripts
├── Makefile              fmt, lint, secure, fuzz, vendor targets
├── go.mod / go.sum       module graph
├── CLAUDE.md / AGENTS.md development guidelines (AGENTS.md is a symlink)
├── CONTRIBUTING.md       how to contribute
├── SECURITY.md           how to report a vulnerability
└── LICENSE               Apache 2.0
```

## Building and verifying

This module vendors its dependencies and builds offline. The `Makefile`
encodes the rules; the short version:

```sh
make test      # go test -race ./...
make fmt       # gofmt the whole module in place
make lint      # fmt-check + vendor-check + vet + staticcheck + gosec
make vuln      # go mod verify + govulncheck
make secure    # lint + vuln — run before every commit
make vendor    # refresh the vendored tree, scrub local-replace VCS metadata
```

Build with `CGO_ENABLED=0 go build -trimpath` so binaries never leak local
paths. Run tests with `-race`; a test that only passes without `-race` is
not passing. Integration tests are tagged `//go:build integration` and run
with `go test -tags integration -race ./...`.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). The short version: read
[`CLAUDE.md`](CLAUDE.md) for the design and security rules the codebase
follows, run `make secure` before every commit, prefer stdlib, and ask
before adding any external dependency.

## License

Apache License 2.0. See [`LICENSE`](LICENSE).
