# Design: `pkg/serve` — an in-harness HTTP session API

**Date:** 2026-07-06
**Status:** Draft (design discussion in session; this doc records the outcome).
Revised 2026-07-06: `serve` is an **in-harness package (`pkg/serve`)**, not a separate module — see
§2 and Decision #1.
**Location:** `github.com/looprig/harness/pkg/serve` — a package in harness, replacing `pkg/api`.
**Compile-time deps:** harness `pkg/event` (event taxonomy + wire marshaling), `pkg/gate` (gate
response requests/responses), and `github.com/looprig/core` `content`/`uuid` — the types in the
driven-surface signatures.
`serve` depends on the `LiveSession` *interface* (§3a), **not** `pkg/session`: the concrete session
is passed in by the consumer, so `pkg/serve` never imports the engine (Decision #5).
**Depends on (read plane):** a narrow catalog/replay interface; today a small adapter can be
built from harness `pkg/sessionstore.Catalog` plus `Store.OpenEventReplayer`/`OpenRecordReplayer`
over `github.com/looprig/storage`.
**Supersedes:** `pkg/api` — its runner is reshaped into `pkg/serve` (session-first, not
factory-first). `Intent`/posture (`pkg/tools`) and the `Session` engine (`pkg/session`) are unchanged.
**Related:** [Open-Gate Posture](2026-07-01-open-gate-posture-design.md) (headless posture),
[Client web app](2026-07-02-client-web-app-design.md) (the *future, separate* front-end **module**
that will consume this package over HTTP — not in scope here).

## 1. Problem & goal

Harness already runs a session and, through `pkg/api`, exposes a per-session HTTP runner
(submit a turn, resolve gates, stream live events, export a transcript). The problem is its
**shape**, not its location — a small HTTP layer belongs in harness: it is stdlib-only (adds no
external dependency), and an agent that never serves HTTP simply doesn't import it. Two things about
the shape are wrong for where we are going:

1. **The runner is factory-first.** `pkg/api` owns the session lifecycle (an in-memory many-session
   map) and calls *down* into a consumer-supplied `Factory` to build each session on an incoming
   `POST /sessions`. Control points the wrong way: the runner constructs sessions instead of
   *wrapping* ones the consumer already built.
2. **The stream is live-only and lossy.** Its SSE stream drops every **Ephemeral** event (token
   deltas, tool-lifecycle) — `event.MarshalEvent` fails closed on them — so an HTTP consumer gets
   step-granularity, never token streaming; and there is no session-listing / cold-history surface.

**Goal.** Reshape the HTTP surface into **`pkg/serve`** (replacing `pkg/api`, staying in harness),
that:

- is a **thin, session-first** HTTP projection of a session's driven surface — you build the
  session the normal harness way and *wrap* it; `serve` never constructs agents;
- adds **no extra engine semantics** beyond what harness already does — it is the over-the-wire
  shape of normal in-process SDK use, not a second session runtime or state machine;
- is **plug-and-play**: it depends on neither your LLM nor any concrete storage backend — those are
  wired at the consumer's composition root and hidden behind the session/read interfaces;
- carries the **full event stream, including Ephemeral live-frames**, so an HTTP client gets the
  same token-by-token fidelity an in-process caller has;
- drives **headless and interactive** sessions through **one** posture-agnostic surface;
- is **routing-agnostic**: sessions are addressed by `session_id`; getting a request to the pod
  that owns a session is **infrastructure's responsibility** (future helm charts / ingress), not
  this package's.

## 2. Scope & package boundary

`github.com/looprig/harness/pkg/serve` — a new **package in harness**, replacing `pkg/api`. It is
*not* a separate module: `serve` is stdlib-only (`net/http`), so a module would shed no dependencies
from harness core — unlike the `looprig/cli` and `storage` extractions, which existed to remove the
charm.land and NATS stacks. An agent that never serves HTTP simply doesn't import the package
(unused packages aren't compiled), and keeping it in-repo holds it lockstep with the engine surface
it projects — no module version to align (Decision #1).

| Compiles against | For |
|---|---|
| harness `pkg/event` | the event taxonomy + wire marshaling (Enduring today; Ephemeral added here) |
| harness `pkg/gate`, `github.com/looprig/core` `content`/`uuid` | gate responses, input-block, and identifier types in the driven-surface signatures |
| the `LiveSession` interface (§3a) | the driven surface — **not** `pkg/session`; the concrete `*Session` is passed in by the consumer |
| catalog/replay interfaces | the stateless read plane and future sequenced replay-live join |
| stdlib `net/http` | the server + SSE |

**Does not import:** `pkg/session` (satisfied structurally by the `LiveSession` interface — keeps
handler tests fakeable, §11), any LLM provider, any storage backend, any concrete agent, `swe`, any
front-end/BFF, any TUI stack. `serve` is a transport over the session's driven surface plus a thin
projection of read-side interfaces; it does not own engine state.

**Explicitly out of scope (owned elsewhere):**

- **Routing / session affinity** → infrastructure (helm charts, ingress, service mesh) in a
  later iteration. §9 states the *contract* infra must satisfy and the harness primitives that
  make it safe, but `serve` implements no router.
- **The front-end / BFF** → the separate future `client` module. It consumes `serve`.
- **`cmd/` binaries** → the consumer (`swe` or the front-end module). `serve` ships `pkg/` only.
- **Browser rendering — the DTO/zod contract + rich UI** → the front-end module. `serve` emits
  wire JSON and the raw session list (§3d); folding it into components is the front-end's job.
  (Session **listing** and cold history *are* in scope here — but as a *separate* read-plane
  handler, §3d, not part of the live-session wrap.)

## 3. Core shape: session-first, not factory-first

### 3a. The driven surface

A session's whole control plane should stay a narrow interface. `serve` is a faithful **wrapper**:
every endpoint maps 1:1 to a session capability, so it exposes no behavior the session lacks. In
particular, `GET …/events` is exactly `SubscribeEvents` — a live, from-now, **lossy** stream
(subscribe late or drop the connection and you miss those events; the session keeps no backlog).
"From the beginning" is **not** a session capability — it is a store read, and lives on the read
plane (`GET …/history`, §3d). `serve` invents neither.

Phase 0 makes two signature cleanups at the session boundary: `Session.SubscribeEvents` should return
`event.Subscription` instead of the concrete `*hub.EventSubscription`, and the gate-specific
`Approve`/`Deny`/`ProvideUserInput` trio should normalize to one `RespondGate(gate.GateResponse)`.
That prevents every new gate kind from adding another session method and another HTTP branch.

```go
// serve.LiveSession — the driven surface serve projects onto HTTP.
// *session.Session satisfies this surface after the SubscribeEvents return type is widened.
type LiveSession interface {
    Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error)
    SubscribeEvents(filter event.EventFilter) (event.Subscription, error)
    RespondGate(ctx context.Context, response gate.GateResponse) error
    Interrupt(ctx context.Context) (bool, error)
}
```

The live wrapper takes the `session_id` separately (`serve.New(id, s)`) because the interface is
behavioral, not identity-bearing. That keeps the port minimal and avoids making harness grow a
method solely for HTTP routing. If the path `{sid}` does not match the mounted id, the handler
returns 404.

### 3b. `serve.New(id, s)` — the primitive

You build the session **exactly as you would without `serve`**, then wrap it. In-process and
HTTP share identical construction; only the last line differs:

```go
runner, _ := session.Compile(cfg, store)   // design-time: bind the agent def + deps (§3c)
id, s, _  := runner.Run(ctx)               // runtime: mint a session id, bring it up live

// in-process:      s.Submit(ctx, blocks)
// over HTTP:        h := serve.New(id, s); serve.Serve(ctx, serve.Config{Addr: "127.0.0.1:0"}, h)
```

Control flow points the right way: **the consumer owns construction; `serve` only exposes.**
There is no `Factory`, no `AgentRequest`, no `serve`-owned session lifecycle, and no background
gate supervisor. Session construction is the consumer's, via `session.Runner` (§3c): `Run` brings up
a new session and `Resume` *rebuilds one from its journal* (a construction / failover concern, §9) —
**not** an event-replay API; `serve` calls neither. `serve.New(id, s)` returns an `http.Handler`;
per-request SSE subscriptions are closed when the request ends.

### 3c. Multi-session per pod; create/resume via `session.Runner`

A pod routinely hosts **many** sessions. Agents/swarms that need no workspace or default tools are
cheap, and kernel-sandboxed agents (via `looprig/sandbox` — Seatbelt on macOS; namespaces +
Landlock + seccomp on Linux) are isolated *within* a shared pod, so isolation does **not** require
one pod per session. `serve.Registry` holds `map[session_id]LiveSession`, populated by the *consumer*
with sessions it built, and routes the `{sid}` path segment to the local session; it never
constructs them (`session.New`/`Restore` stay the consumer's). Whether a pod holds one session or
hundreds — and whether isolation is pod-per-session or kernel-sandbox-in-a-shared-pod — is the
**consumer's deployment choice**, invisible to `serve`.

**Create/resume is the consumer's, via `session.Runner` — a real API, not "deferred".** Building a
session needs the agent's standing config (`loop.Config`: LLM, tools, posture) plus the store — a
**design-time** concern `serve` neither has nor should. `session.Runner` (a `pkg/session` addition,
modeled on `flow.Runner`) makes the split explicit:

- **design time** — `session.Compile(cfg, store, opts…) (*Runner, error)` binds the agent definition
  (`loop.Config`) and its deps into an immutable, reusable Runner and computes the config fingerprint;
- **runtime** — `Runner.Run(ctx) (uuid.UUID, *Session, error)` mints a fresh `session_id` and brings
  up a live session (returns immediately); `Runner.Resume(ctx, sid) (*Session, error)` rebuilds one
  from its journal, refusing a fingerprint mismatch (fail-secure).

The reuse point is the **runner lifecycle contract**, not shared code. `flow.Runner[S]` is a
graph-specific concrete type (`GraphID`, `GraphVersion`, typed state `S`, `CheckpointStore`,
`Run(ctx, in S)`, `Resume(ctx, graphRunID, payload)`). A session runner has different inputs and
outputs (`loop.Config`, session journal/store/lease/workspace deps, `Run(ctx) -> (SessionID,
*Session)`, `Resume(ctx, SessionID) -> *Session`). Do not move a generic runner interface into
`core` yet: it would either be too abstract to help or would leak flow/session concepts into the
leaf identity/content module. Keep the session runner concrete/local to `pkg/session`; `pkg/serve`
may define the narrow interface it consumes. Extract only after two packages need the exact same
method set and semantics.

So the HTTP create/resume flow is a **consumer handler** — `runner.Run`/`Resume` →
`serve.Registry.Register(id, s)` — built from the `session.Runner` API plus the one `serve` seam
(`Register`). `serve` never constructs sessions. The only piece genuinely handed to infra is
**placement** (*which pod* runs a new session, §9); locally it is trivial — one pod builds and
registers. (`session.Runner` is specified separately; it is a Phase 0 prerequisite here, §12.)

### 3d. `serve.NewReader(reads)` — the read plane (listing + cold history)

Listing every session is neither a session's job (a `*Session` knows only itself) nor the live
plane's — it is the **read plane**. Today harness `pkg/sessionstore` owns the derived catalog over
the generic `storage` module's `KV`; if that catalog later moves to `github.com/looprig/storage`,
`serve`'s interface should not change. `sessionstore.Catalog.ListSessions(ctx) → []SessionMeta` is
a derived, event-sourced projection, one `SessionMeta` per session, updated by folding the event
stream. It needs **no live session**, so it lists ended sessions and sessions owned by other pods.
Cold history/export come from the store's replay openers, not from the catalog itself. `serve`
wraps those read-side capabilities as a **separate** handler:

```go
live := serve.New(id, s)              // live+control for ONE session (sticky)
read := serve.NewReader(reads)        // read plane: catalog + replay openers (stateless, any pod)
//   GET /sessions        → ListSessions() → []SessionMeta
//   GET /sessions/{sid}   → one SessionMeta
//   GET /sessions/{sid}/history → cold Enduring replay from the store (from Beginning/FromSeq)
//   GET /sessions/{sid}/export  → transcript render from replay
```

`serve.NewReader` depends on a narrow read interface (`ListSessions` + per-session metadata lookup
+ cold event/record replay). Today that can be satisfied by an adapter over
`sessionstore.Catalog.ListSessions` plus `sessionstore.Store.OpenEventReplayer`/
`OpenRecordReplayer`; `Catalog` alone is the listing projection. Different dependency (a read
source, not a live session), different affinity (stateless — any replica answers, no lease, no
routing), different lifecycle: two small handlers in one package (ISP/SRP), mounted on one mux with
**no path overlap** — the live handler owns `/events` (subscribe); the reader owns `/history`,
`/export`, and the listing routes. The listing intelligence stays in `sessionstore`/storage; `serve`
only projects it onto HTTP.

## 4. Plug-and-play: the consumer owns LLM, storage, tools, posture

`serve`'s only tie to the consumer's world is the `LiveSession` value it wraps. Everything the
session needs is wired at the composition root, captured before `serve` sees anything — so
`serve` **structurally cannot** couple to it:

```go
// YOUR composition root (swe, or your own service)
store, _ := fsstore.Open(dir)                 // ← swap: natsstore / pgstore / your own Store
llm, _   := auto.New(ctx, providerCfg)         // ← swap: your inference.Client (chutes / TEE / …)

gate := tools.Interactive.Wrap(myChecker)      // ← or tools.Unattended (headless), see §5
cfg  := loop.Config{
    Client: llm,                               // ← YOUR LLM
    Model:  myModel, System: mySystemPrompt,
    Tools:  loop.ToolSet{Permission: gate, Registry: myTools},   // ← YOUR tools + posture
}
runner, _ := session.Compile(cfg, store, session.WithWorkspace(ws, root)) // design-time: agent + deps
id, s, _  := runner.Run(ctx)                                              // runtime: new live session

_ = serve.Serve(ctx, serve.Config{Addr: addr}, serve.New(id, s)) // serve knows none of the above
```

Swapping the LLM, storage backend, tools, system prompt, model, or posture is a **local edit
inside your composition root** that `serve` never observes.

## 5. Headless vs interactive over one API

Posture (`Interactive` / `Unattended`) is **not** a `serve` concept. It lives in harness
`pkg/tools` as `Intent`, whose own doc comment is emphatic: *"NOT session state… the composition
root reads it to decide the permission wiring and then discards it."* The consumer's session
construction (§4) bakes the posture into the gate. **`serve` is posture-agnostic** — by the time
a session reaches its handler, it already *is* one posture or the other.

The **same endpoints** serve both; the only difference is whether gate-reply POSTs are commonly
needed:

- **Interactive** — permission gates surface as events on the SSE stream; a caller **must** POST a
  `ResponseRequest` for the event's opaque `gate_id` back to the session gate endpoint to unpark the
  blocker. Full drive.
- **Headless (`Unattended`)** — permission gates **auto-resolve** via the consumer's declared
  allowlist over the non-bypassable safety floor; no POST needed. A caller mostly reads
  `…/events` to watch and can POST `…/interrupt` to steer. The **`AskUser` user-input gate still
  parks by design** (autonomy must not sever the agent's line to the human): a caller answers it
  with the same `ResponseRequest` endpoint, or the harness session applies that gate's configured
  `ResponsePolicy` (for example suspend-for-restore or an explicit non-critical model/default
  decision).

This is the whole point of the extraction: one HTTP surface, both run modes, selected upstream at
construction.

## 6. HTTP surface

Live/control paths are `session_id`-scoped for addressing (infra routes on `{sid}` — §9) and
resolve to the wrapped session (or the registry's local session for that `{sid}`). Read-plane
paths resolve to the catalog/replayer and do not require a live session on this pod.

| Method + path | Plane | Backed by |
|---|---|---|
| `GET  /sessions` | read | `sessionstore.Catalog.ListSessions` → `[]SessionMeta` (stateless; any pod) |
| `GET  /sessions/{sid}` | read | catalog per-session `SessionMeta` (stateless; any pod) |
| `POST /sessions` `{agent}` | create† | *consumer handler*: `Runner.Run` → `Registry.Register` → `201 {session_id}` |
| `POST /sessions/{sid}/resume` | create† | *consumer handler*: `Runner.Resume(sid)` → `Registry.Register` → `200` |
| `POST /sessions/{sid}/input` | control | `LiveSession.Submit` (fire-and-forget → `CommandID`) |
| `GET  /sessions/{sid}/events` | live | `LiveSession.SubscribeEvents` — live SSE from the subscription point; **lossy, no backlog** (mirrors the in-proc API exactly) |
| `GET  /sessions/{sid}/history` | read | cold Enduring replay from the store (`OpenEventReplayer` from `Beginning()`/`FromSeq`, paged) — any pod, no live session |
| `POST /sessions/{sid}/gates/{gid}` | control | `LiveSession.RespondGate(gate.GateResponse)` from a `gate.ResponseRequest` body (`202 Accepted`; durably committed by harness) |
| `POST /sessions/{sid}/interrupt` | control | `LiveSession.Interrupt` |
| `GET  /sessions/{sid}/export` | read | read-plane transcript render from the session journal (optional shortcut / parity oracle) |

- **Create/resume (†) are consumer handlers, not `serve` core.** They call `session.Runner` (§3c)
  and `serve.Registry.Register`; `serve` provides the `Register` seam, never the agent config.
  Placement (*which pod*) is infra's (§9); locally the receiving pod builds and registers.
- **Validate at the boundary:** `{sid}`/`{gid}` parse as `uuid.UUID`; malformed IDs return
  400; an unknown live session or gate returns 404; `from`/`to` are bounded integers; bodies are
  size-limited.
- **Live and cold are different endpoints with different sources — `serve` invents nothing.**
  `GET …/events` is exactly `SubscribeEvents`: a live, from-now, **lossy** stream (the session has
  no "from the beginning" API — subscribe late or drop, and you miss those events; that is the
  in-proc contract). "From the beginning" is a *store* capability — `GET …/history` cold-replays the
  journal — a different source (the durable log, any pod, no live session). Fusing them into one
  lossless replay-then-follow is a **client** concern (§7) and/or waits on a future harness sequenced
  source; `serve` exposes the two primitives as-is and promises no join.
- **Gate replies mirror harness semantics.** For v1 loop gates, a successful gate POST returns
  `202 Accepted`: the `GateResponse` was accepted and durably committed by the session gate router,
  not proven consumed by the parked runner. The public HTTP body is only
  `gate.ResponseRequest{Action, Values}`; `{gid}` supplies the gate id, and harness sets/overwrites
  response provenance. `serve` must not add a shadow gate registry or branch per gate kind; stale,
  duplicate, wrong-kind, or wrong-action responses belong to harness' session-owned gate directory
  and resolver validation.
- **Gate response policies belong to harness.** `serve` does not run gate timers, auto-deny tools,
  decide non-critical questions, or suspend sessions. Those behaviors are session-owned
  `ResponsePolicy` actions and surface through the same gate events/history as in-process use.
- **No HTTP destroy in v1.** `Interrupt` only cancels in-flight work. Graceful session shutdown,
  lease release, and store deletion/retention are composition-root lifecycle and storage-GC
  concerns, not part of this thin serving layer.

## 7. Event model on the wire: Enduring + the new Ephemeral live-frame

This is the one real code addition. Today `streamEvents` subscribes to both classes but
`event.MarshalEvent` **fails closed on Ephemeral** (`EphemeralNotPersistableError`), so the SSE
stream carries only **Enduring** events. `serve` adds a **live-only wire frame** for Ephemeral
events, using named SSE event types without changing the durable event codec:

```
event: enduring                 event: ephemeral
data: {"v":1,"type":"StepDone",…} data: {"v":1,"kind":"token_delta",…}
```

- **`enduring`** — authoritative transitions (StepDone, gates, terminals `TurnDone`/`TurnFailed`/
  `TurnInterrupted`). Persisted by harness; replayable from the journal. Live delivery uses the
  same event payload harness already publishes. Gate delivery is the public pair only:
  `GateOpened` and `GateResolved`. `GatePrepared` is a private journal record used for restore and
  must not appear in SSE/history.
- **`ephemeral`** — `TokenDelta`, `ToolCallStarted/Completed`, `InputQueued`. **Live-only,
  unpersisted, best-effort, no `seq`.** Dropped on reconnect — it *self-heals* from the next
  authoritative `enduring` event. The client renders deltas live and reconciles to the
  authoritative `StepDone`.

Ephemeral frames **never** enter the journal and **never** carry a sequence — that invariant
(`MarshalEvent` failing closed) is preserved; the ephemeral encoder is a *separate*, transport-
only path.

**Sequenced reconnect is a harness/storage capability, not a `serve` invention.** `GET …/events`
is the session's live subscription (lossy, no backlog, no `seq`); `GET …/history` is the store's
cold replay cursor (yields `(event, seq)`). `serve` must not synthesize journal sequence numbers,
maintain a parallel event log, or fuse the two into one stream. A lossless replay-then-follow join
needs a harness/storage source that pairs live Enduring events with their journal sequence (a
sequenced subscription or a `Follow:true` journal tail); until that exists, a client can read
`…/history` and then attach `…/events`, accepting the race at the seam. `serve` exposes both
primitives and promises no join.

The DTO is versioned (`"v":1`) from day one; a `kind` discriminator lets the front-end fold both
classes with one renderer. (The *browser-facing* DTO/zod contract is the front-end module's
concern; `serve` emits the wire JSON.)

## 8. Connection & concurrency model

The load-bearing fact: **a session is long-lived server state; HTTP requests are short RPCs
against it.** `session.New` spawns the loop's goroutines, which live for the session's whole life
(many turns). `serve.New(id, s)` makes that already-living thing reachable. Go's goroutine-per-request
is fine: request goroutines are ephemeral front-doors that call the concurrency-safe session and
return.

Three request lifetimes:

| Request | Lifetime | Behavior |
|---|---|---|
| `POST …/input`, `…/gates/{gid}`, `…/interrupt` | milliseconds | delivers to the session, returns immediately |
| `GET …/events` (SSE) | the whole session | the outbound event stream, spanning many turns |
| `GET …/export` and read-plane listing/history | milliseconds | snapshot reads |

- **Submit is fire-and-forget.** `LiveSession.Submit` returns a `CommandID` *before the turn starts*;
  the outcome (`InputQueued` / `TurnStarted` / `TurnFoldedInto` / `TurnRejected` /
  `InputCancelled`) is observed on the event fan-in, correlated by `CommandID`. A turn completing
  emits a terminal `enduring` event on the (still-open) SSE stream — it closes no request.
- **Submit-during-a-running-turn** is just another `POST …/input`. The **session** owns that
  concurrency (queue / fold / reject), surfaced as events. The HTTP layer never coordinates.
- **Gate out, response in.** A gate is an `enduring` event *out* carrying an opaque `gate_id` plus
  display/correlation metadata; the decision comes *in* as a `ResponseRequest` POST to that
  `gate_id`. `serve` forwards the response to harness and returns `202 Accepted` when harness durably
  accepts the gate response; it does not prove runner consumption. It does not maintain open-gate
  state, run response policies, or invent gate-kind-specific APIs. Permission approval scope values
  are stable strings in `values.scope` (`"once"`, `"session"`, `"workspace"`), matching the prompt
  option values; numeric enum values are not part of the HTTP contract.
- **Reconnect is the client's to assemble.** `serve` offers two primitives — live `…/events`
  (subscribe, lossy) and cold `…/history` (store replay) — and does not fuse them. A client that
  wants continuity reads `…/history` to the tip, then attaches `…/events`, accepting the seam race
  until harness/storage exposes a sequenced source (§7). Dropped `ephemeral` frames self-heal from
  later authoritative `enduring` events.
- **SSE proxying** (when infra peer-forwards, §9) is a flush loop with an idle deadline, never an
  unbounded copy.

**Not WebSockets (decision, §Decision-log).** The shape is a trickle of control inbound + a
firehose of events outbound + durable replay — for which SSE-out/POST-in is simpler and keeps
every HTTP affordance (per-action auth, status codes, retries, idempotency, per-request tracing,
proxy tolerance). A socket would relocate routing to connect-time but add a permanent wrong-pod
relay, an in-band RPC protocol, reconnect storms on deploys, and head-of-line blocking. WebSocket
stays an **evidence-driven, opt-in transport** (true high-frequency duplex only), layered *on
top of* routing, never replacing it.

## 9. Distribution & session affinity — the consumer's business, not `serve`'s

A live session is **pinned to one pod** (its loop goroutines are in that pod's memory). Harness
already makes this a *correctness* invariant, not just an implementation detail: a session is
driven under a **single-writer fencing lease** (`sessionstore` `Lease`: `Epoch()` fencing token,
`Lost()` channel, `journal.LeaseHeldError{HolderEpoch}`). Two pods driving one session would
double-call the LLM and double-execute tools; the lease forbids it.

Therefore **all requests for a `session_id` must reach the pod that owns that session.** `serve`
does **not** implement this. It is the **consumer's** deployment decision, realized in
**infrastructure** (helm charts / ingress / service mesh, a later iteration). The contract that
deployment must satisfy:

1. **Route on `session_id`** (present in every `/sessions/{sid}/…` path) to the owning pod —
   *resource*-keyed routing, not client-keyed sticky cookies. Both SSE and POSTs for a given
   `{sid}` converge on one pod.
2. **A create/placement path** decides which pod hosts a new session and records the mapping
   (the ownership directory can be built on the `storage` module's `KV`, which the catalog already
   uses; the `Lease` is the authoritative single-writer token).
3. **Failover:** on pod loss, the lease's `Lost()`/expiry lets another pod acquire it and
   `session.Restore(sid)` from the shared journal, then update the mapping. The fencing epoch
   prevents a stale owner from writing. In-flight requests to the dead pod fail and re-route.

What `serve` provides so infra *can* do this cleanly:

- **`session_id` in every path** — the routing key is free.
- **Read-plane statelessness** — history/listing (the catalog + cold replay) read the shared
  store, so those requests need **no** affinity (any replica answers). Only the live+control
  plane is sticky. Infra can route the read plane freely and only pin live/control.
- **Honest liveness/durability split** — liveness is pod-pinned; durability is the shared
  journal, so death→`Restore` handoff loses no committed history.

**Ultimately the consumer's call, not just "infra's".** How sessions pack onto pods — one per pod,
or hundreds behind kernel sandboxing (§3c) — whether a deployment even *needs* sticky routing, and
at what granularity, are the consumer's decisions. `serve` is identical whether a pod owns one
session or a thousand; "the owning pod" is simply wherever the consumer placed it.

The self-routing-fleet option (pods peer-forward using a `KV` directory) is recorded as a
*possible* infra implementation, **not** a `serve` feature — infra may instead choose L7
`session_id` ingress routing or a mesh. `serve` is identical under all of them.

## 10. Auth & security (per CLAUDE.md)

- **Loopback default, fail-secure.** `serve.Config` binds `127.0.0.1` by default. Loopback may run
  unauthenticated for local development, matching today's `pkg/api` posture. A non-loopback bind
  requires both explicit public-bind opt-in and either an auth verifier or an explicitly named
  unsafe no-auth development flag; no credentials configured must never silently expose a public
  control surface.
- **Bearer auth is boundary-checked when configured.** `serve` accepts a token verifier for the
  driven surface; TLS is typically terminated at the infra ingress, but `serve` supports direct TLS
  (`MinVersion: tls.VersionTLS12`, never `InsecureSkipVerify`). Secrets come from the
  environment/secrets manager, never code, never logs.
- **Explicit `http.Server` timeouts** (Read/Write/Idle/`MaxHeaderBytes`); SSE clears the write
  deadline for its own stream only. Every session/journal call is `context`-bounded.
- **Typed errors** for each distinct failure (`SessionNotFoundError`, `LoopNotFoundError`,
  `StoreReadError`, …); `errors.As` at call sites; audit auth failures and denied gates without
  logging payloads/tokens/PII. Harness now reports gate response failures authoritatively, so `serve`
  projects them without inspecting gate state: stale/unknown gate → `404`, invalid action/kind/scope
  or grant values → `400`, not-ready races → `409`, persistence/session faults → `500`.

## 11. Testing (per CLAUDE.md)

- **Table-driven, `-race` always.** Handlers test against a **fake `serve.LiveSession`** (the narrow
  interface) — no real loop, no LLM, deterministic.
- **Gate forwarding:** gate POST validates `{sid,gid}` plus a `gate.ResponseRequest` body, calls
  `LiveSession.RespondGate` with a server-shaped `gate.GateResponse`, returns `202 Accepted` after the
  harness session durably accepts the response, and does not require or maintain a `serve` open-gate
  map. Client provenance is ignored/rejected. Handler tests assert the boundary mapping for
  authoritative gate errors (`404` stale/unknown, `400` invalid response, `409` not ready) while the
  deeper wrong-kind, response-policy, timeout/default/suspend, and grant-validation behavior remains
  covered by harness session gate-router tests. Permission-scope values are asserted as stable
  strings, not numeric enums.
- **Event-wire codec:** a fuzz target (external SSE frame → decode); tests that live `enduring`
  frames use the existing durable event payload, `ephemeral` frames never carry a `seq`, and
  Ephemeral never round-trips through the journal.
- **Future sequenced seam tests (not Phase 1):** once harness/storage exposes a sequenced source,
  add replay-to-`seq`-then-attach coverage proving every `enduring` event is delivered exactly once
  across the join (no loss, no duplication), including an event appended in the join window.
- **Concurrency:** fire-and-forget submit returns before the terminal event; a submit during a
  running turn surfaces `InputQueued`/`TurnFoldedInto`/`TurnRejected`, never blocks the handler.
- **Integration-tagged** (`//go:build integration`) SSE flush/teardown, upstream-down → typed
  error, loopback-vs-public gating.

## 12. Migration phases

- **Phase 0 (prerequisites, `pkg/session`):** add `session.Runner` (`Compile` → `Run`/`Resume`,
  modeled on `flow.Runner`) separating the agent definition + deps (design time) from instantiation
  (runtime); widen `Session.SubscribeEvents` to return `event.Subscription` while still returning the
  same concrete hub subscription; confirm the rest of the `LiveSession` port matches existing
  `*session.Session` methods; decide the exact narrow read-plane interface over catalog/replay.
- **Phase 1 — reshape & wrap:** move `pkg/api`'s live+control handlers into `pkg/serve` (same repo);
  introduce the session-first `serve.New(id, s)` primitive + narrow `LiveSession` interface; add the
  `serve.NewReader(reads)` read-plane handler (listing/history/export); delete the
  `Factory`/many-session-create model (the registry is the multi-session path — first-class, a pod
  hosts many; create/resume are consumer handlers over `session.Runner` + `Registry.Register`).
  `pkg/api` is replaced by `pkg/serve`. Gate replies use a
  normalized `ResponseRequest`/`GateResponse` path against opaque gate ids; no `serve` gate registry
  ships.
- **Phase 2 — ephemeral live-frame:** add the `event: ephemeral` SSE frame class over the existing
  live subscription. Keep exact `from=<seq>` replay-to-live as deferred until harness/storage exposes
  sequenced live Enduring delivery.
- **Phase 3 — deferred, consumer/infra-owned:** `session_id` routing (helm charts / ingress),
  multi-pod session **placement** (which pod runs a new session), and optional remote session
  shutdown land outside the thin wrapper.

## Decision log (session, 2026-07-02 → 2026-07-06)

1. **In-harness package `pkg/serve`, not a separate module.** `serve` is stdlib-only, so a module
   would shed no dependencies (unlike `looprig/cli`/`storage`, which removed charm.land/NATS);
   unused-package elimination already makes it optional for non-serving agents; and in-repo keeps it
   lockstep with the engine surface — no module version to align, and the serve↔harness compile edge
   is one atomically-versioned repo (the only real skew boundary is the HTTP wire to remote clients).
   Revisit a module split only if `serve` grows heavy external deps. It replaces `pkg/api`;
   `Intent`/posture (`pkg/tools`) and the `Session` engine (`pkg/session`) are unchanged.
2. **Name `pkg/serve`** (over keeping `api` or `host`): it is the serving layer; `api` reads generic
   and `host` blurs with the in-process path.
3. **Session-first, not factory-first.** `serve.New(id, s)` wraps an already-built session; the
   consumer owns create/resume via `session.Runner` (Decision #13). No `Factory`/`AgentRequest` in
   `serve`. HTTP create/resume are consumer handlers (`Runner.Run`/`Resume` → `Registry.Register`),
   not a `serve`-owned layer; only multi-pod *placement* is deferred to infra.
4. **`NewSession` is the in-process method; `serve` is its over-the-wire twin.** One driven
   surface (`*session.Session`/`serve.LiveSession`), two transports. `serve` must not add state or
   semantics beyond what harness already exposes. In particular `GET …/events` is exactly
   `SubscribeEvents` (live, lossy, no backlog); "from the beginning" is a store read (`…/history`),
   not a session capability — `serve` invents neither, and does not fuse them into a replay-to-live
   join (that is a client concern / a future sequenced-source capability). `Restore` rebuilds a
   session from its journal to resume it; it is the consumer's, and is not an event-replay API.
5. **Plug-and-play by construction; interface over the engine (Option B).** `serve` depends on the
   `LiveSession` interface, **not** `pkg/session` — `*session.Session` satisfies it structurally, so
   the consumer passes a real session while handler tests pass a fake (§11). LLM, storage backend,
   tools, and posture are wired at the composition root and hidden behind the `LiveSession` and
   read-plane interfaces; `serve` never imports them.
6. **Posture-agnostic.** `Intent` (Interactive/Unattended) stays in harness `pkg/tools`, chosen
   at construction, discarded. Same endpoints for both modes; `ResponseRequest` POSTs are required
   when harness emits a gate event (interactive permissions or `AskUser`); `AskUser` still parks.
7. **No `serve` open-gate registry.** Harness' session-owned gate directory is authoritative for
   open gates and stale/random/wrong-kind responses. `serve` forwards response requests through
   `GateResponse` values with durable-acceptance semantics and projects authoritative gate errors to
   status codes; it never mirrors open-gate state locally.
8. **Ephemeral live-frame added.** `event: ephemeral` SSE frames carry token/tool-lifecycle
   deltas (live-only, unpersisted, no `seq`). Live `enduring` frames use the existing durable event
   payload. Exact journal `seq` on live Enduring frames is deferred until harness/storage exposes a
   sequenced live source; `serve` must not synthesize it. The `MarshalEvent`-fails-closed-on-
   ephemeral invariant is preserved (separate transport encoder).
9. **SSE-out + POST-in, not WebSockets.** Asymmetric duplex fits an agent session and keeps HTTP
   affordances. WebSocket is an opt-in, evidence-driven transport layered on top of routing.
10. **`serve` does not own routing — the consumer does.** Sessions are addressed by `session_id`;
   routing to the owning pod is the **consumer's** deployment decision (realized in infra — future
   helm charts / ingress). A pod may host one session or hundreds (lightweight or kernel-sandboxed
   agents share a pod — §3c), so packing / isolation / stickiness granularity are the consumer's
   call. `serve` states the contract (§9), exposes the enablers (`session_id` in every path;
   read-plane statelessness), but implements no router. Affinity's *correctness* is backed by
   harness's single-writer fencing lease; failover is `Lost()`→`Restore` from the shared journal.
11. **The front-end/BFF is a separate future module** that consumes `serve`. BFF concerns (token
    custody, same-origin, `sid→pod` proxying) are *not* imposed on `serve`'s consumers.
12. **Listing/history is a read-plane handler in `serve`, backed by `sessionstore`/storage.**
    `serve.NewReader(reads)` exposes `GET /sessions` (+ per-session metadata + cold history) over a
    narrow adapter: `sessionstore.Catalog.ListSessions → []SessionMeta` for listing, plus
    `Store.OpenEventReplayer`/`OpenRecordReplayer` for cold history/export. It is a *separate*,
    stateless handler (any pod, no lease, no routing), distinct from the live `serve.New(id, s)`
    wrap. The listing intelligence lives in `sessionstore` today over the generic storage module;
    `serve` only projects it. The browser DTO / rich rendering stays the front-end's job.
13. **`session.Runner` separates design-time deps from runtime create/resume (Phase 0).** Modeled on
    `flow.Runner`: `session.Compile(cfg, store, opts…)` binds the agent definition (`loop.Config`:
    LLM, tools, posture) + deps into an immutable, reusable Runner and computes the config
    fingerprint (**design time**); `Runner.Run(ctx) (id, *Session, error)` mints a session id and
    brings up a live session, `Runner.Resume(ctx, sid) (*Session, error)` rebuilds from the journal
    and refuses a fingerprint mismatch (**runtime**). Per-run tuning is functional options into an
    unexported config — no exported `Deps`/`Config` grab-bag, matching flow's convention. Reuse
    flow's lifecycle invariants and naming discipline, not its concrete code: `flow.Runner[S]` and
    `session.Runner` do not share method signatures or state model, and a common `core.Runner`
    interface is YAGNI until an identical method set has at least two consumers. This is why `serve`
    needs no create layer: the consumer's handler calls `Runner.Run`/`Resume` and
    `serve.Registry.Register`.
