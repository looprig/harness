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

For existing HTTP consumers, `pkg/serve` retains a frozen compatibility
surface over a `Rig` (submit, subscribe via SSE, respond to a gate,
interrupt). Its public BFF role is deprecated in favor of Factory for new
composition; see [the compatibility policy](pkg/serve/README.md).
For terminal consumers, the sibling `looprig/tui` module binds
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

### Upgrade note: gate responses make a journal one-way (v0.35.0)

v0.35.0 adds the `gate_response` runtime command (`runtimecommand.KindGateResponse`),
which lets a Host-admitted gate answer settle through the durable disposition path.
**Once a session journal holds any `gate_response` application — applied, `no_op` or
`refused`; a `no_op` against a gate that never existed is enough — harness v0.34.0 and
older cannot reopen it.** Both `OpenJournal` and replay fail closed with
`journal: encode command application: runtimecommand: invalid Kind: unknown kind "gate_response"`
(the Marshal-side check in v0.34.0 `pkg/journal/record_json.go:191`, reached from
`pkg/sessionstore/replay.go:422`). No data is lost, but the session is stranded until
a v0.35.0+ runtime opens it. **Do not roll a Host back below harness v0.35.0 once it
has applied a `gate_response`.**

### Upgrade note: create and restore make a journal one-way (v0.36.0)

v0.36.0 adds the `create` and `restore` runtime commands
(`runtimecommand.KindCreate`, `runtimecommand.KindRestore`), which are the kinds
Factory admits for a session's first command and for resuming one that is not
resident. Before it, `runtimecommand.Kind` named three kinds while Factory admitted
five, so a create was refused **after** Host had durably begun its dispatch attempt:
no disposition frame was ever written, the store could never settle the record, the
consumer blocked at that command and never advanced its cursor, and
`Closure.Validate` refused the same kinds so no successor could close it either. The
session existed and the agent was resident and the user could never talk to it.

**Once a session journal holds any `create` or `restore` application prefix or
disposition frame, harness v0.35.0 and older cannot reopen it.** Both `OpenJournal`
and replay fail closed — measured against v0.35.0:

```
sessionstore: replay decode at seq 3: journal: encode command application: runtimecommand: invalid Kind: unknown kind "create"
```

The failing check is the Marshal-side validation (v0.35.0
`pkg/journal/record_json.go:191`), reached from the replay hydration that runs inside
`OpenJournal` and wrapped by `pkg/sessionstore/replay.go:169`; a disposition frame
fails the same way through `journal: encode command disposition:`. No data is lost,
but the session is stranded until a v0.36.0+ runtime opens it. **Do not roll a Host
back below harness v0.36.0 once it has applied a `create` or a `restore`.**

### Upgrade note: an applied input is a durable debt (v0.37.0, not one-way)

In v0.36.0 and older, an input a Host admitted under a disposition attempt was handed
to the loop's in-memory inbox and its `applied` disposition written at once; the turn
that carries it out was appended later. A crash or a graceful shutdown in between left
the command settled `applied` forever with the user's message never run (a shutdown
even appended an `InputCancelled` for it). v0.37.0 closes that window.

**What `applied` means for an input now.** The loop actor writes it itself, after
deciding to take the input and before the input can queue, fold or start, so it is
durable strictly before any effect. From then on the input is OWED until a durable
event it caused resolves it:

- If the runtime dies or shuts down first, the input is carried over rather than
  cancelled, and the next restore re-offers it under its original runtime command id.
- The debt is also discharged, **without the input running**, by the ordinary visible
  resolutions of any queued input: an idle turn-start failure (`TurnRejected`), a failed
  turn ahead of it or an execution-admission error (`InputCancelled` with
  `CancelTurnFailed`), or an explicit retraction. These are durable and visible, never
  silent, and they are what `applied` settled into in v0.36.0 too.
- A loop that declines before committing (shutting down, queue full) publishes nothing
  and the command settles `refused` — the user must resend. v0.36.0 settled these
  `applied` and then rejected the input.

**Late and out-of-order replay.** An owed input is replayed at the NEXT restore, after
any inputs that ran in between, and possibly long after it was sent: debt a crashed
v0.36.0 runtime left behind, a commit whose outcome could not be read back, or a replay
the loop declined (more than 64 owed inputs, or a loop that could not take it) are all
retried at each later restore. The agent may therefore act on an instruction that is
stale relative to what it has done since, including with tool side effects. A
composition that cannot tolerate that should gate what it replays; harness does not
apply a staleness rule.

**Successors settle instead of halting.** `CloseAttempt` on an attempt whose own
disposition is already durable (the predecessor recorded it and died before the store
settled it) writes nothing and succeeds with `ClosureResult.AlreadyDisposed`, so a
successor Host settles from that evidence. In v0.36.0 it refused (an idempotency
collision, or `EnduringEffectError` once the replayed input's effect landed).

**Only native loops take the handshake.** The `command.Admission` handshake is sent
only to a `loop.Backend` that declares `SupportsRuntimeAdmission() bool` (the native
loop does). Every other backend — a foreign loop above all — keeps the released
send-then-record path: the session hands it the input and writes `applied` only after
the backend accepted the send. A backend must not declare the capability unless it
honours the whole `command.Admission` contract.

What a foreign loop keeps and loses, precisely:

- **It keeps crash-debt replay at restore.** The restore plan does not ask which
  backend ran: any input whose `applied` is durable and which no durable event names in
  its `Cause.CommandID` is re-offered at the next restore — to a foreign loop as the
  plain input, without the handshake — under its original runtime command id. For a
  foreign loop that window runs from the `applied` record until its durable
  `TurnStarted` or `InputCancelled` for the input (its `InputQueued` is not durable).
  A replay the loop does not start before this runtime goes away is planned again.
- **It loses ordering before the effect.** `applied` is written after the send, not by
  the actor before the input can queue or start, so a crash between the send and that
  record leaves no disposition; a successor closes the attempt `not_applied`, and the
  recovery scan refuses that closure if the input already caused a durable event.
- **It loses carry-over on a graceful shutdown.** Only the native actor returns queued
  inputs as owed instead of cancelling them. A foreign loop that durably cancels its
  queue on shutdown (`InputCancelled`) has discharged the debt, and nothing is replayed.

The input's intent record is now load-bearing under an attempt: if it cannot be
appended the command is refused before its prefix and may be re-offered.

**No new record kind is written**, so this is not a one-way upgrade: v0.36.0 can still
open a v0.37.0 journal (it just will not replay an owed input, so rolling back strands
any owed input), and v0.37.0 recovers the owed inputs a crashed v0.36.0 runtime left
behind. Commands admitted without an attempt id keep the released path unchanged.

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
├── scripts/              build/lint helper scripts
├── Makefile              fmt, lint, secure, fuzz targets
├── go.mod / go.sum       module graph
├── CLAUDE.md / AGENTS.md development guidelines (AGENTS.md is a symlink)
├── CONTRIBUTING.md       how to contribute
├── SECURITY.md           how to report a vulnerability
└── LICENSE               Apache 2.0
```

## Building and verifying

Dependencies are pinned by `go.mod`/`go.sum`, not vendored. The `Makefile`
encodes the rules; the short version:

```sh
make test      # go test -race ./...
make fmt       # gofmt the whole module in place
make lint      # fmt-check + vet + staticcheck + gosec
make vuln      # go mod verify + govulncheck
make secure    # lint + vuln — run before every commit
```

Add `GOWORK=off` to check the module against its real pinned dependency
versions rather than the sibling working copies `../go.work` supplies:
`GOWORK=off go test ./...`.

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
