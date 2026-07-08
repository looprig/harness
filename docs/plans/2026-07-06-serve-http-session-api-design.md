# Design: `pkg/serve` — an in-harness HTTP session API

**Date:** 2026-07-06
**Status:** Draft (design discussion in session; this doc records the outcome).
Revised 2026-07-06: `serve` is an **in-harness package (`pkg/serve`)**, not a separate module — see
§2 and Decision #1.
Revised 2026-07-08 (review): sequenced live delivery pulled into Phase 0 (§7b, Decision #16); the
status projection moved into the catalog fold (§3d, Decision #17); `/v1` path prefix,
`Idempotency-Key`, SSE heartbeats, fail-secure public bind, and an explicit no-CORS stance added
(§6, §8, §10, Decision #18); runner/HTTP naming aligned to the session package (`Restore`,
Decision #19).
**Location:** `github.com/looprig/harness/pkg/serve` — a package in harness, replacing `pkg/api`.
**Compile-time deps:** harness `pkg/event` (event taxonomy + wire marshaling), `pkg/gate` (gate
response requests/responses), and `github.com/looprig/core` `content`/`uuid` — the types in the
driven-surface signatures.
`serve` depends on narrow `Runner`/`LiveSession` *interfaces* (§3a-§3c), **not** `pkg/session`: the
compiled runner is passed in by the consumer and returns live sessions structurally satisfying
`LiveSession`, so `pkg/serve` never imports the engine (Decision #5).
**Depends on (read plane):** a narrow catalog/replay interface; today a small adapter can be
built from harness `pkg/sessionstore.Catalog` plus `Store.OpenEventReplayer` over
`github.com/looprig/storage`. Status and journal reads come from this read plane, not from the live
session.
**Supersedes:** `pkg/api` — its runner is reshaped into `pkg/serve` (runner-supplied, not
factory-first). `Intent`/posture (`pkg/tools`) and the `Session` engine (`pkg/session`) are unchanged.
**Related:** [Open-Gate Posture](2026-07-01-open-gate-posture-design.md) (headless posture),
[Client web app](2026-07-02-client-web-app-design.md) (the *future, separate* front-end **module**
that will consume this package over HTTP — not in scope here),
[Flow run observability](2026-07-08-flow-run-observability-design.md) (the workflow analogue of
this surface, sharing its HTTP conventions).

## 1. Problem & goal

Harness already runs a session and, through `pkg/api`, exposes a per-session HTTP runner
(submit a turn, resolve gates, stream live events). The problem is its
**shape**, not its location — a small HTTP layer belongs in harness: it is stdlib-only (adds no
external dependency), and an agent that never serves HTTP simply doesn't import it. Two things about
the shape are wrong for where we are going:

1. **The runner is factory-first.** `pkg/api` owns the session lifecycle (an in-memory many-session
   map) and calls *down* into a consumer-supplied `Factory` to build each session on an incoming
   `POST /sessions`. Control points the wrong way: the HTTP layer knows about an agent factory instead
   of operating over a consumer-supplied compiled runner.
2. **The stream is live-only and lossy.** Its SSE stream drops every **Ephemeral** event (token
   deltas, tool-lifecycle) — `event.MarshalEvent` fails closed on them — so an HTTP consumer gets
   step-granularity, never token streaming; and there is no session-listing / cold-journal surface.

**Goal.** Reshape the HTTP surface into **`pkg/serve`** (replacing `pkg/api`, staying in harness),
that:

- is a **thin, runner-supplied** HTTP projection of a session's driven surface — you compile the
  runner the normal harness way and `serve` creates/restores runtime sessions through it; `serve`
  never constructs agents;
- adds **no extra engine semantics** beyond what harness already does — it is the over-the-wire
  shape of normal in-process SDK use, not a second session runtime or state machine;
- is **plug-and-play**: it depends on neither your LLM nor any concrete storage backend — those are
  wired at the consumer's composition root and hidden behind the session/read interfaces;
- carries the **full event stream, including Ephemeral live-frames**, so an HTTP client gets the
  same token-by-token fidelity an in-process caller has;
- drives **headless and interactive** sessions through **one** posture-agnostic surface;
- gives client SDKs a single framework-neutral protocol to target, so first-party Svelte and
  third-party React/Vue/Angular/Solid integrations do not require backend-specific branches;
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
| the `Runner`/`LiveSession` interfaces (§3a-§3c) | the runtime lifecycle and driven surface — **not** `pkg/session`; the compiled runner is passed in by the consumer |
| catalog/replay interfaces | the stateless read plane and future sequenced replay-live join |
| stdlib `net/http` | the server + SSE |

**Does not import:** `pkg/session` (satisfied structurally by the `Runner`/`LiveSession` interfaces —
keeps handler tests fakeable, §11), any LLM provider, any storage backend, any concrete agent, `swe`,
any front-end/BFF, any TUI stack. `serve` is a transport over the session runtime contract plus a
thin projection of read-side interfaces; it does not own engine state.

**Explicitly out of scope (owned elsewhere):**

- **Routing / session affinity** → infrastructure (helm charts, ingress, service mesh) in a
  later iteration. §9 states the *contract* infra must satisfy and the harness primitives that
  make it safe, but `serve` implements no infrastructure or cross-pod router.
- **The front-end / BFF** → the separate future `client` module. It consumes `serve`.
- **`cmd/` binaries** → the consumer (`swe` or the front-end module). `serve` ships `pkg/` only.
- **Browser rendering and framework adapters** → the front-end/client SDK module. `serve` owns the
  stable HTTP/SSE **wire contract**; folding that contract into React/Svelte/Vue/etc. state and
  components is the client module's job. (Session **listing** and cold journal *are* in scope here
  — but as a *separate* read-plane route group, §3d, not part of the live/control routes.)

## 3. Core shape: runner-supplied, not factory-first

### 3a. The driven surface

A session's whole control plane should stay a narrow interface. `serve` is a faithful **wrapper**:
every endpoint maps 1:1 to a session capability, so it exposes no behavior the session lacks. In
particular, `GET …/events` is exactly `SubscribeEvents` — a live, from-now, **lossy** stream
(subscribe late or drop the connection and you miss those events; the session keeps no backlog).
"From the beginning" is **not** a session capability — it is a store read, and lives on the read
plane (`GET …/journal`, §3d). `serve` invents neither.

Phase 0 makes three signature cleanups at the session boundary: `Session.SubscribeEvents` should
return `event.Subscription` instead of the concrete `*hub.EventSubscription`;
`Subscription.Events()` should yield `event.Delivery{Event, JournalSeq}` instead of bare
`event.Event` (§7b) so live Enduring events carry their journal sequence; and the legacy
gate-specific `Approve`/`Deny`/`ProvideUserInput` trio (which today coexists with the already-landed
`RespondGate(gate.GateResponse)`) should be deleted in favor of that one method. That prevents every
new gate kind from adding another session method and another HTTP branch.

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

The live surface is behavioral, not identity-bearing. `serve` keeps identity in its internal
live-session table, keyed by the `session_id` returned from the supplied runner. That keeps the port
minimal and avoids making harness grow an identity method solely for HTTP routing. If the path `{sid}`
does not resolve to a live session in this process, live/control handlers return 404.

### 3b. `serve.Handler(runner, reads, opts...)` — the runtime HTTP adapter

You compile the session runner **exactly as you would without `serve`**, then hand the compiled
runtime runner to the HTTP adapter. In-process and HTTP share the same design-time construction; only
the runtime front door differs:

```go
runner, _ := session.Compile(cfg, store)   // design-time: bind the agent def + deps (§3c)

// in-process:      id, s, _ := runner.Run(ctx); s.Submit(ctx, blocks)
// over HTTP:        hdlr := serve.Handler(runner, reads); srv, _ := serve.Server(addr, hdlr); srv.ListenAndServe()
```

Control flow points the right way: **the consumer owns compile/design time; `serve` owns the HTTP
runtime lifecycle over that compiled runner.** There is no `Factory`, no `AgentRequest`, and no
background gate supervisor. `POST /sessions` calls `Runner.Run` to mint a new session id and bring up
a live session; if the request body carries initial input blocks, the handler immediately submits
them through `LiveSession.Submit` and returns the resulting `command_id`. `POST
/sessions/{sid}/restore` calls `Runner.Restore` to rebuild one from its journal (a construction /
failover concern, §9) — **not** an event-replay API. `serve.Handler` does not call
`session.Compile`, `session.New`, or `session.Restore` directly; those remain behind the supplied
runner. Per-request SSE subscriptions are closed when the request ends.

### 3c. Multi-session per pod; create/restore via the supplied runner

A pod routinely hosts **many** sessions. Agents/swarms that need no workspace or default tools are
cheap, and kernel-sandboxed agents (via `looprig/sandbox` — Seatbelt on macOS; namespaces +
Landlock + seccomp on Linux) are isolated *within* a shared pod, so isolation does **not** require
one pod per session. `serve.Handler` owns a local, internal `map[session_id]LiveSession` and routes
the `{sid}` path segment to the local session. Whether a pod holds one session or hundreds — and
whether isolation is pod-per-session or kernel-sandbox-in-a-shared-pod — is the **consumer's
deployment choice**, invisible to `serve`.

The internal live-session table is not a public API and not infrastructure routing. It answers
"is `{sid}` live in this process, and if so which `LiveSession` handles it?" It is separate from the
read-plane catalog, which lists durable sessions whether or not they are live here.

**Create/restore is serve-owned at HTTP runtime, through the supplied runner.** Building a session
needs the agent's standing config (`loop.Config`: LLM, tools, posture) plus the store — a
**design-time** concern `serve` neither has nor should. `session.Runner` (a `pkg/session` addition,
modeled on `flow.Runner`) makes the split explicit:

- **design time** — `session.Compile(cfg, store, opts…) (*Runner, error)` binds the agent definition
  (`loop.Config`) and its deps into an immutable, reusable Runner and captures the config
  fingerprint inputs — reusing the existing `FingerprintFrom`/`ConfigFingerprintFields` machinery
  in `pkg/session/config_fingerprint.go`, already stamped by `New` and already enforced by
  `Restore`;
- **runtime** — `Runner.Run(ctx) (uuid.UUID, *Session, error)` mints a fresh `session_id` and brings
  up a live session (returns immediately); `Runner.Restore(ctx, sid) (*Session, error)` rebuilds one
  from its journal, refusing a fingerprint mismatch (fail-secure — the check already lives in
  `session.Restore`, with `WithAllowConfigMismatch` as the explicit opt-out).

The reuse point is the **runner lifecycle contract**, not shared code. `flow.Runner[S]` is a
graph-specific concrete type (`GraphID`, `GraphVersion`, typed state `S`, `CheckpointStore`,
`Run(ctx, in S)`, `Resume(ctx, graphRunID, payload)`). A session runner has different inputs and
outputs (`loop.Config`, session journal/store/lease/workspace deps, `Run(ctx) -> (SessionID,
*Session)`, `Restore(ctx, SessionID) -> *Session` — named for the underlying `session.Restore`,
since flow's `Resume(ctx, id, payload)` has different semantics, Decision #19). Do not move a generic runner interface into
`core` yet: it would either be too abstract to help or would leak flow/session concepts into the
leaf identity/content module. Keep the session runner concrete/local to `pkg/session`; `pkg/serve`
may define the narrow interface it consumes. Extract only after two packages need the exact same
method set and semantics.

`pkg/serve` consumes only the narrow runtime contract it needs:

```go
type Runner[S LiveSession] interface {
    Run(ctx context.Context) (uuid.UUID, S, error)
    Restore(ctx context.Context, id uuid.UUID) (S, error)
}
```

So the HTTP create/restore flow is a **serve handler** — `runner.Run`/`Restore` → internal
`session_id` attachment → optional initial `Submit` → `201/200`. `serve` never owns
compile/design-time config. The only piece genuinely handed to infra is **placement** (*which pod*
runs a new session, §9); locally it is trivial — the receiving pod runs/restores and records the
live session in memory. (`session.Runner` is specified by this section plus Decision #14 and lands
in Phase 0, §12; the one open decision — which `session.Option`s are Compile-time vs per-run — is
settled in the implementation plan.)

### 3d. `serve.NewReader(reads)` — the read plane (listing + status + cold journal)

Listing every session is neither a session's job (a `*Session` knows only itself) nor the live
plane's — it is the **read plane**. Today harness `pkg/sessionstore` owns the derived catalog over
the generic `storage` module's `KV`; if that catalog later moves to `github.com/looprig/storage`,
`serve`'s interface should not change. `sessionstore.Catalog.ListSessions(ctx) → []SessionMeta` is
a derived, event-sourced projection, one `SessionMeta` per session, updated by folding the event
stream. It needs **no live session**, so it lists ended sessions and sessions owned by other pods.
Cold journal reads and lightweight status come from the store's replay openers, not from the catalog
itself. `serve` wraps those read-side capabilities as a **separate** route group:

```go
read := serve.NewReader(reads)        // read plane: catalog + replay openers (stateless, any pod)

hdlr := serve.Handler(runner, read, serve.WithAuth(authn))
srv, _ := serve.Server(addr, hdlr)    // fail-secure: errors on a non-loopback bind with nil auth (§10)
err := srv.ListenAndServe()
```

`serve.Handler` mounts the live/control routes, HTTP create/restore routes, and read-plane routes on
one handler. The live-session table stays internal; callers do not register sessions by hand.

`serve.NewReader` depends on a narrow read interface (`ListSessions` + lightweight status read
+ cold public event-journal replay). Today that can be satisfied by an adapter over
`sessionstore.Catalog.ListSessions` plus `sessionstore.Store.OpenEventReplayer`; `Catalog` alone is
the listing projection. Different dependency (a read source, not a live session), different affinity
(stateless — any replica answers, no lease, no routing), different lifecycle: two route groups in one
package (ISP/SRP), mounted on one mux with **no path overlap** — the live routes own `/events`
(subscribe); the reader owns `/journal`, `GET /sessions/{sid}/status`, and the listing routes. The
listing intelligence stays in `sessionstore`/storage; `serve` only projects it onto HTTP.

```go
type Page struct {
    Skip  int // for list APIs
    Limit int // default 100, hard cap 1000
}

type JournalPage struct {
    From  uint64 // inclusive ledger sequence; 0 means beginning
    Limit int
}

type Reader interface {
    ListSessions(ctx context.Context, page Page) (SessionList, error)
    ReadStatus(ctx context.Context, id uuid.UUID) (SessionStatus, error)
    ReadJournal(ctx context.Context, id uuid.UUID, page JournalPage) (EventJournalPage, error)
}
```

`EventJournalPage` is the raw public Enduring event journal page with ledger sequence numbers.
`serve` intentionally does not expose a generic session document projection in v1; add a richer read
endpoint later only when a concrete client proves the shape it needs.

`SessionStatus` is the lightweight scheduler/headless read model. It is **maintained by the catalog
fold, not derived per request**: `sessionstore`'s catalog already folds the event stream into one
`SessionMeta` per session, so the status fields extend that projection (a Phase 0 `sessionstore`
prerequisite) and `ReadStatus` is a cheap catalog read — never an O(journal) replay under a
scheduler's polling loop. Today `SessionMeta` carries only `active`/`stopped`; the fold gains
`State` (running / waiting_on_gate / idle / failed / interrupted / stopped, folded from
TurnStarted / GateOpened / GateResolved / terminal events), `LastJournalSeq`, `ActiveTurnID`,
`WaitingGateID`, and the latest terminal turn / step summaries. This also gives the session *list*
its most important glanceable signal — sessions waiting on approval — without N journal replays.
It needs no live session and no connected client:

```go
type SessionStatus struct {
    SessionID      uuid.UUID
    State          string // running, waiting_on_gate, idle, failed, interrupted, stopped
    LastJournalSeq uint64
    ActiveTurnID   uuid.UUID // optional
    WaitingGateID  uuid.UUID // optional
    LastTurn       *StatusEvent // latest TurnDone/TurnFailed/TurnInterrupted-like event, optional
    LastStep       *StatusEvent // latest StepDone, optional
    UpdatedAt      time.Time
}

type StatusEvent struct {
    JournalSeq uint64
    Event      event.Event
}
```

## 4. Plug-and-play: the consumer owns LLM, storage, tools, posture

`serve`'s only tie to the consumer's world is the compiled runtime runner and read-plane adapter it
receives. Everything the session needs is wired at the composition root, captured before `serve` sees
anything — so `serve` **structurally cannot** couple to it:

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
read := serve.NewReader(reads)                                            // read plane: catalog + replay

hdlr := serve.Handler(runner, read, serve.WithAuth(authn))                 // runtime HTTP lifecycle
srv, _ := serve.Server(addr, hdlr)   // fail-secure: errors on a non-loopback bind with nil auth (§10)
err := srv.ListenAndServe()
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
resolve to `serve`'s internal local live-session table for that `{sid}`. Read-plane paths resolve to
the catalog/replayer and do not require a live session on this pod. All routes mount under a
**`/v1` path prefix** — the same convention as `flow/pkg/ingress` — so a wire-breaking change gets
`/v2` rather than in-place mutation; `GET /v1/capabilities` complements path versioning, it does not
replace it. (Prose elsewhere in this doc omits the prefix for brevity.)

| Method + path | Plane | Backed by |
|---|---|---|
| `GET  /v1/capabilities` | protocol | static `serve` feature/version descriptor for client SDK negotiation |
| `GET  /v1/sessions?skip=<n>&limit=<n>` | read | paged `sessionstore.Catalog.ListSessions` → `[]SessionMeta` (stateless; any pod) |
| `GET  /v1/sessions/{sid}/status` | read | catalog status projection (§3d): state, last journal seq, active/waiting ids, last turn/step summary (stateless; any pod) |
| `POST /v1/sessions` | create† | `serve` handler: supplied `Runner.Run` → internal live attach → optional initial `LiveSession.Submit` → `201 {session_id, command_id?}`; honors `Idempotency-Key` |
| `POST /v1/sessions/{sid}/restore` | create† | `serve` handler: supplied `Runner.Restore(sid)` → internal live attach → `200 {session_id}` |
| `POST /v1/sessions/{sid}/input` | control | `LiveSession.Submit` (fire-and-forget → `CommandID`) |
| `GET  /v1/sessions/{sid}/events` | live | `LiveSession.SubscribeEvents` — live SSE from the subscription point; **lossy, no backlog** (mirrors the in-proc API exactly); `enduring` frames stamped `id: <journal_seq>` (§7b) |
| `GET  /v1/sessions/{sid}/journal?from_journal_seq=<n>&limit=<n>` | read | cold public Enduring event journal from the store (`OpenEventReplayer` from `FromSeq`, bounded in the handler) — any pod, no live session |
| `POST /v1/sessions/{sid}/gates/{gid}` | control | `LiveSession.RespondGate(gate.GateResponse)` from a `gate.ResponseRequest` body (`202 Accepted`; durably committed by harness) |
| `POST /v1/sessions/{sid}/interrupt` | control | `LiveSession.Interrupt` |

- **Create/restore (†) are `serve` handlers over a supplied runner.** They call the runner (§3c) and
  attach the returned live session to the handler's internal live-session table. `serve` owns no agent
  config and never compiles. Placement (*which pod*) is infra's (§9); locally the receiving pod
  runs/restores and keeps the live session in memory.
- **Initial input on create:** `POST /sessions` accepts an optional JSON body
  `{"blocks":[...]}` using the same `content.Block` array as `POST /sessions/{sid}/input`. If
  `blocks` is present and non-empty, the handler calls `LiveSession.Submit` after attaching the new
  live session and returns `{"session_id":"...","command_id":"..."}`. If no blocks are supplied, it
  creates an idle live session and returns `{"session_id":"..."}`.
- **Create is idempotent under retry.** `POST /v1/sessions` honors an optional `Idempotency-Key`
  header, mirroring `flow/pkg/ingress` run-create: a retried create with the same key returns the
  original `{session_id, command_id?}` instead of minting a second session (a network retry from a
  headless scheduler must not double-run an agent). Keys are bounded strings with a bounded
  retention window; a reused key with a different body returns 409. v1 keeps the key→response map
  per-pod, in memory, with a TTL — cross-pod idempotency is meaningless before multi-pod placement
  exists and arrives with it (Phase 4).
- **Validate at the boundary:** `{sid}`/`{gid}` parse as `uuid.UUID`; malformed IDs return
  400; an unknown live session or gate returns 404; `skip`, `from_journal_seq`, and `limit` are
  bounded integers; bodies are size-limited.
- **Session listing paging contract:** `skip` is zero-based and defaults to 0; `limit` defaults to
  100 with a hard cap of 1000; values above the cap return 400. `GET /sessions` returns
  `{"sessions":[...],"skip":0,"limit":100,"next_skip":100,"done":false}`. `done=true` means fewer
  than `limit` entries were returned. The catalog may initially implement this by reading all
  `SessionMeta`, applying a stable sort (`last_active_at desc`, then `session_id asc`), and slicing;
  a storage-backed cursor can replace that later without changing the HTTP contract.
- **Journal paging contract:** `from_journal_seq` is an inclusive ledger sequence (`0`/absent means
  beginning), and `limit` defaults to 100 with a hard cap of 1000. The current store read API has only
  `ReplayRequest.FromSeq`; `serve` enforces `limit` by stopping after N yielded Enduring events.
  The response is `{"events":[{"journal_seq":123,"event":{...}}],"next_journal_seq":124,"done":false}`.
  `done=true` means the cold cursor drained before `limit`; `next_journal_seq` is the next ledger
  sequence a client should ask for when `done=false`. No upper-bound parameter ships until the storage
  API has a native bounded cursor.
- **Status read contract:** `GET /v1/sessions/{sid}/status` is the cheap polling API for headless
  schedulers and dashboards. It is served from the catalog's fold-maintained status projection
  (§3d) — a KV read exposing `state`, `last_journal_seq`, `active_turn_id`, optional
  `waiting_gate_id`, and the latest terminal turn / step summaries — never a per-request journal
  replay. It does not require a live session or an SSE subscriber. If a scheduler starts a run
  through `POST /v1/sessions`, it polls this endpoint until `state` leaves
  `running`/`waiting_on_gate`.
- **Live and cold journal are different endpoints with different sources — `serve` invents nothing.**
  `GET …/events` is exactly `SubscribeEvents`: a live, from-now, **lossy** stream (the session has
  no "from the beginning" API — subscribe late or drop, and you miss those events; that is the
  in-proc contract). "From the beginning" is a *store* capability — `GET …/journal` cold-replays the
  public Enduring event journal — a different source (the durable log, any pod, no live session).
  `serve` exposes the two primitives as-is and implements no server-side join; because live
  `enduring` frames are seq-stamped (§7b), a **client** fuses them losslessly itself
  (subscribe-and-buffer, replay to tip, drop `journal_seq <= tip`).
- **No plain `GET /sessions/{sid}` in v1.** The API has no proven product projection yet, so `serve`
  does not ship a vague session document. `GET /sessions/{sid}/status` is the lightweight durable
  summary for polling. `GET /sessions/{sid}/journal` is the lower-level public event journal cursor:
  raw Enduring events with ledger sequences for repair, debugging, replay pagination, and reconnect
  stitching. Add a richer read endpoint later only after a concrete consumer defines that view.
- **Gate replies mirror harness semantics.** For v1 loop gates, a successful gate POST returns
  `202 Accepted`: the `GateResponse` was accepted and durably committed by the session gate router,
  not proven consumed by the parked runner. The public HTTP body is only
  `gate.ResponseRequest{Action, Values}`; `{gid}` supplies the gate id, and harness sets/overwrites
  response provenance. `serve` must not add a shadow gate registry or branch per gate kind; stale,
  duplicate, wrong-kind, or wrong-action responses belong to harness' session-owned gate directory
  and resolver validation.
- **Gate response policies belong to harness.** `serve` does not run gate timers, auto-deny tools,
  decide non-critical questions, or suspend sessions. Those behaviors are session-owned
  `ResponsePolicy` actions and surface through the same gate events/journal as in-process use.
- **No HTTP destroy in v1.** `Interrupt` only cancels in-flight work. Graceful session shutdown,
  lease release, and store deletion/retention are composition-root lifecycle and storage-GC
  concerns, not part of this thin serving layer.
- **No transcript/export layer in `serve`.** `serve` exposes status and journal primitives only; it
  must not require `LiveSession.ExportSource`, `pkg/transcript/html`, `goldmark`, or any markdown
  renderer. A future product view belongs in a separate consumer/client package after its shape is
  proven, not in the thin serving layer.
- **Capability discovery is intentionally small.** `GET /v1/capabilities` returns a static JSON document
  such as `{"protocol":"looprig.serve","version":1,"features":["journal","live_sse","ephemeral_sse","gate_response"]}`.
  It is not health, auth introspection, tenancy, or routing metadata. Its job is to let generated
  clients and browser SDKs fail fast when they require a feature the server does not expose.

## 7. Event model on the wire: Enduring + the new Ephemeral live-frame

This is the one real code addition. Today `streamEvents` subscribes to both classes but
`event.MarshalEvent` **fails closed on Ephemeral** (`EphemeralNotPersistableError`), so the SSE
stream carries only **Enduring** events. `serve` adds a **live-only wire frame** for Ephemeral
events, using named SSE event types without changing the durable event codec:

```
event: enduring                   event: ephemeral
data: {"v":1,"event":{...}}        data: {"v":1,"kind":"token_delta","delta":{...},...}
```

- **`enduring`** — authoritative transitions (StepDone, gates, terminals `TurnDone`/`TurnFailed`/
  `TurnInterrupted`). Persisted by harness; replayable from the journal. Live delivery uses the
  same event payload harness already publishes. Gate delivery is the public pair only:
  `GateOpened` and `GateResolved`. `GatePrepared` is a private journal record used for restore and
  must not appear in SSE or the public `/journal` endpoint.
- **`ephemeral`** — `TokenDelta`, `ToolCallStarted/Completed`, `InputQueued`. **Live-only,
  unpersisted, best-effort, no `seq`.** Dropped on reconnect — it *self-heals* from the next
  authoritative `enduring` event. The client renders deltas live and reconciles to the
  authoritative `StepDone`.

Ephemeral frames **never** enter the journal and **never** carry a sequence — that invariant
(`MarshalEvent` failing closed) is preserved; the ephemeral encoder is a *separate*, transport-
only path.

The ephemeral encoder has its own DTO because `content.Chunk` is intentionally not JSON-serializable
in `core/content`:

```go
type EphemeralFrame struct {
    V     int             `json:"v"`              // 1
    Kind  string          `json:"kind"`           // token_delta, tool_call_started, tool_call_completed, input_queued
    Header event.Header    `json:"header,omitzero"`
    Delta json.RawMessage `json:"delta,omitempty"` // kind-specific, never a journal event
}
```

For `TokenDelta`, `delta` is a tagged live-only chunk:

```json
{"chunk_type":"text","text":"hello"}
{"chunk_type":"thinking","thinking":"reasoning"}
{"chunk_type":"tool_use","index":0,"id":"call_1","name":"Bash","input_json":"{\"cmd\":"}
```

`ToolCallStarted`, `ToolCallCompleted`, and `InputQueued` use their existing public fields in
`delta` (for example `tool_execution_id`, `tool_name`, `summary`, `is_error`, `result_preview`);
the shared `header` carries session/loop/turn/step/cause correlation. Unknown future Ephemeral
types are skipped with a debug log until the live DTO grows a new `kind` case; they are never sent
as lossy ad-hoc JSON.

**Sequenced reconnect is a harness capability `serve` projects — not a `serve` invention.**
`GET …/events` is the session's live subscription (lossy, no backlog); `GET …/journal` is the
store's cold public event-journal cursor (yields `(event, journal_seq)`). `serve` must not
synthesize journal sequence numbers, maintain a parallel event log, or fuse the two into one
stream. Harness supplies the sequence on live deliveries (§7b — a Phase 0 change), `serve` stamps
it as the SSE `id:` on `enduring` frames, and a **client** performs the exact join itself. `serve`
still exposes exactly the two primitives and implements no server-side join.

The DTO is versioned (`"v":1`) from day one; a `kind` discriminator lets the front-end fold both
classes with one renderer.

## 7a. Public protocol contract for client SDKs

`serve` is the backend stability point for framework flexibility. The client module may ship React,
Svelte, Vue, Angular, Solid, or plain TypeScript adapters, but all of them must target the same
versioned HTTP/SSE contract. That contract lives at the `serve` boundary, not inside a particular
SPA implementation.

Contract artifacts:

- **OpenAPI / JSON Schema** for request and response bodies: session list pages, status summaries,
  journal pages, submit requests, gate response requests, interrupt responses, capability documents,
  and typed error envelopes.
- **SSE frame schemas** for `event: enduring` and `event: ephemeral`, including the required `v`
  discriminator and the allowed `kind` values for live-only frames.
- **Golden wire fixtures** committed beside the schemas. Go tests in `pkg/serve` validate emitted
  JSON/SSE frames against the fixtures; TypeScript tests in the client SDK parse the same fixtures.
- **Versioning rules:** additive fields are allowed; removing or changing the meaning of a field
  requires a new protocol version. Unknown JSON fields are ignored by clients; unknown SSE event
  names or frame kinds are skipped with diagnostics, not treated as ad-hoc payloads.
- **Error envelope:** every non-2xx JSON response uses one shape, for example
  `{"error":{"code":"session_not_found","message":"session not found","retryable":false}}`.
  The stable part is `code` and `retryable`; `message` is diagnostic and not parsed for control
  flow.

This is the Looprig analogue of Vercel AI SDK's split: backend/core behavior is stable, while
framework packages adapt the stream and state model to each UI runtime. `serve` must not import or
know about those packages; it only preserves the protocol they consume.

**Lossless resume.** Live `enduring` SSE frames carry the durable `journal_seq` as the SSE `id:`
field from Phase 2, backed by the Phase 0 delivery envelope (§7b). Public SDKs may promise lossless
resume from that point using the client-side exact join (§7b); a server-side
`GET /v1/sessions/{sid}/events?from_journal_seq=<n>` replay-then-follow remains optional Phase 3
sugar, not a prerequisite. `ephemeral` frames stay live-only and unsequenced; SDKs reconcile them
against later authoritative `enduring` frames.

## 7b. Sequenced live delivery — the Phase 0 harness change

The sequence already exists at exactly the right moment: the hub's invariant is durable-append
**before** fan-out, and `SessionJournal.Append(ctx, rec) (uint64, error)` returns the strictly
monotonic sequence. Today it is deliberately discarded at the hub's appender seam
(`JournalEventAppender.AppendEvent` drops it — "the hub needs only the success/failure signal"), so
live subscribers get bare events while the replay side already yields `(event, seq)` pairs. Phase 0
closes that gap in harness:

- **`pkg/hub`'s appender seam widens:** `AppendEvent(ctx, ev) (uint64, error)` — the journal
  adapter returns the sequence it already receives from `Append`.
- **`pkg/event` gains a delivery envelope**, and `Subscription.Events()` yields it:

```go
type Delivery struct {
    Event      Event
    JournalSeq uint64 // 0 for Ephemeral deliveries — never persisted, never sequenced
}
```

- **`pkg/hub` threads the sequence** from append through `deliver`; Ephemeral events publish with
  `JournalSeq == 0`.

Invariants preserved: `event.Header` is untouched (no sequence field enters the durable codec),
`MarshalEvent` still fails closed on Ephemeral, and the journal remains the only sequence
authority. Changing the subscription element type is a breaking change; it lands together with the
Phase 0 `SubscribeEvents` widening so in-repo consumers (`pkg/api`, until deleted) and the sibling
`cli` module adjust once.

On the wire, `serve` stamps each live `enduring` SSE frame with `id: <journal_seq>`; `ephemeral`
frames never carry an `id`. `serve` still does not fuse replay and live — with both sides
sequenced, a **client** joins losslessly with no `Follow` support and no server-side state:

1. open `GET …/events` and buffer incoming `enduring` frames;
2. page `GET …/journal` to the tip `T`;
3. discard buffered frames with `journal_seq <= T`, apply the rest, then follow live.

A server-side `GET …/events?from_journal_seq=` replay-then-follow join remains optional sugar
(Phase 3); it is no longer required for lossless resume.

## 8. Connection & concurrency model

The load-bearing fact: **a session is long-lived server state; HTTP requests are short RPCs
against it.** `session.New` spawns the loop's goroutines, which live for the session's whole life
(many turns). `serve.Handler` starts or restores those sessions through the supplied runner and keeps
the live values in an internal `session_id` table. Go's goroutine-per-request is fine: request
goroutines are ephemeral front-doors that call the concurrency-safe session and return.

Three request lifetimes:

| Request | Lifetime | Behavior |
|---|---|---|
| `POST …/input`, `…/gates/{gid}`, `…/interrupt` | milliseconds | delivers to the session, returns immediately |
| `GET …/events` (SSE) | the whole session | the outbound event stream, spanning many turns |
| `GET /sessions`, `GET /sessions/{sid}/status`, and `GET …/journal` | milliseconds | read-plane snapshot/cursor reads |

- **Subscribers are observational.** A session runs without any `/events` subscriber. Enduring events
  are durably appended before fan-out; with no subscribers, fan-out is simply a no-op. This is the
  normal headless/scheduler path: `POST /sessions {"blocks":[...]}` starts work, and the scheduler
  observes later through `/status` or `/journal`.
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
- **Reconnect is the client's to assemble — and it is exact.** `serve` offers two primitives —
  live `…/events` (subscribe, lossy, seq-stamped per §7b) and cold `…/journal` (store replay) —
  and does not fuse them. A client joins losslessly: subscribe and buffer, page `…/journal` to tip
  `T`, drop buffered frames with `journal_seq <= T`, follow live. Dropped `ephemeral` frames still
  self-heal from later authoritative `enduring` events.
- **SSE disconnect is not interruption.** The `/events` handler owns one subscription for that HTTP
  request. When `r.Context()` is canceled or a write fails, it closes the subscription and returns;
  the hub removes that subscriber from fan-out. The session keeps running. The only HTTP endpoint that
  cancels work is `POST /sessions/{sid}/interrupt`.
- **SSE keepalive.** Streams emit a comment heartbeat (`: ping`) every 15–30 seconds when idle and
  set `Cache-Control: no-store` (plus `X-Accel-Buffering: no` for buffering proxies), so
  intermediaries do not silently kill a quiet session's stream. Heartbeats are part of the wire
  contract (§7a); clients treat a missing heartbeat past a grace window as a dead connection and
  reconnect.
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
   `Runner.Restore(ctx, sid)` from the shared journal, then update the mapping. The fencing epoch
   prevents a stale owner from writing. In-flight requests to the dead pod fail and re-route.

What `serve` provides so infra *can* do this cleanly:

- **`session_id` in every path** — the routing key is free.
- **Read-plane statelessness** — journal/listing (the catalog + cold replay) read the shared
  store, so those requests need **no** affinity (any replica answers). Only the live+control
  plane is sticky. Infra can route the read plane freely and only pin live/control.
- **Honest liveness/durability split** — liveness is pod-pinned; durability is the shared
  journal, so death→`Runner.Restore` handoff loses no committed journal entries.

**Ultimately the consumer's call, not just "infra's".** How sessions pack onto pods — one per pod,
or hundreds behind kernel sandboxing (§3c) — whether a deployment even *needs* sticky routing, and
at what granularity, are the consumer's decisions. `serve` is identical whether a pod owns one
session or a thousand; "the owning pod" is simply wherever the consumer placed it.

The self-routing-fleet option (pods peer-forward using a `KV` directory) is recorded as a
*possible* infra implementation, **not** a `serve` feature — infra may instead choose L7
`session_id` ingress routing or a mesh. `serve` is identical under all of them.

## 10. Auth & security (per CLAUDE.md)

Follow `flow/pkg/ingress`: auth is a caller-supplied seam on the HTTP handler, not a baked-in
scheme. That keeps `serve` cohesive with other looprig HTTP modules and lets a future auth module
export one adapter that returns `func(*http.Request) error` for both `flow/ingress.WithAuth` and
`serve.WithAuth`.

```go
type Option func(*config)

func WithAuth(authn func(*http.Request) error) Option
func WithMaxBodyBytes(n int64) Option

type ServerOption func(*serverConfig)

// Required to bind a non-loopback address when the handler has no authenticator.
func WithInsecurePublicBind() ServerOption

func Handler[S LiveSession](runner Runner[S], reads Reader, opts ...Option) http.Handler
func Server(addr string, h http.Handler, opts ...ServerOption) (*http.Server, error)
```

- **No scheme baked in.** `WithAuth` installs a caller-supplied authenticator on every `serve` route:
  a non-nil error returns generic `401 {"error":"unauthorized"}` before any live session or read
  interface is touched. `nil` auth means no auth, matching `flow/pkg/ingress`; production services
  wire auth at the composition root or behind an ingress/proxy. A future `auth` module can implement
  bearer tokens, mTLS claims, tenant checks, or capability checks while still presenting the same
  `func(*http.Request) error` seam to `flow` and `serve`.
- **Authorization model — no built-in tenancy.** Like `flow/pkg/ingress`, `serve` has no intrinsic
  session ownership model. A caller that passes `WithAuth` may still access any session id unless the
  authenticator rejects it. Deployments that need tenancy enforce it in the authenticator (for
  example by checking `{sid}` against request-derived claims) or in a fronting proxy.
- **Loopback/server defaults — and a fail-secure public bind.** The handler is auth-agnostic;
  transport hardening lives in the `Server` helper, mirroring `flow/pkg/ingress.Server`: explicit
  ReadHeader/Read/Write/Idle timeouts, `MaxHeaderBytes`, and `TLSConfig.MinVersion >=
  tls.VersionTLS12`. Loopback-only local development can run no-auth. A **non-loopback bind with a
  nil authenticator is refused**: `serve.Server` returns a typed `PublicBindWithoutAuthError`
  unless the caller passes the explicit `WithInsecurePublicBind()` opt-in. The gate-response POST
  is remote approval of tool execution — the most security-critical route in the system — so
  unauthenticated exposure must be a deliberate act, never a default (fail secure, per CLAUDE.md).
  `serve.Server(addr, hdlr, opts...)` returns a configured `*http.Server`; callers start it with
  the standard `srv.ListenAndServe()` / `srv.Serve(listener)` APIs.
- **No CORS, by design.** `serve` sets no CORS headers. Browser apps reach it through a
  same-origin BFF (the client module); the direct `ServeTransport` is for trusted/server-side
  callers only. If a deployment genuinely needs cross-origin browser access, that is a
  fronting-proxy concern, not a `serve` feature.
- **One looprig HTTP convention set.** `serve` and `flow/pkg/ingress` share the `/v1` path prefix,
  the `func(*http.Request) error` auth seam, the error-envelope shape (§7a), the `skip`/`limit`
  paging contract, and `Idempotency-Key` semantics — so a future auth module, generated clients,
  and operators see one protocol family, not two dialects.
- **Request hardening.** `WithMaxBodyBytes` bounds decoded request bodies. Every session/journal call
  is `context`-bounded. SSE uses no server-wide write timeout for the stream path, or explicitly
  clears the write deadline for that response.
- **Typed errors** for each distinct failure (`SessionNotFoundError`, `LoopNotFoundError`,
  `StoreReadError`, …); `errors.As` at call sites; audit auth failures and denied gates without
  logging payloads/tokens/PII. Harness now reports gate response failures authoritatively via
  typed `GateError` kinds, so `serve` projects them without inspecting gate state:
  `GateNotFound` → `404`, `GateActionInvalid`/`GateKindMismatch` → `400`, `GateNotReady` → `409`,
  `GateAppendFailed` → `500`, `GateCapacity` → `503`.

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
  Ephemeral never round-trips through the journal. Token-delta tests cover all three chunk DTOs
  (`text`, `thinking`, `tool_use`) so `content.Chunk` never leaks into JSON via reflection.
- **Protocol contract:** generated OpenAPI / JSON Schema plus golden JSON/SSE fixtures for every
  public response, request, SSE frame, and error envelope. Handler tests validate emitted payloads
  against the fixtures; fixture changes are reviewed as wire-contract changes, not incidental test
  churn.
- **Journal paging:** handler tests cover absent `from_journal_seq`, explicit `from_journal_seq`,
  default `limit`, `limit` above the hard cap rejected with 400, `next_journal_seq`, `done`,
  malformed query params, and replay errors mapped to typed HTTP failures.
- **Auth/public bind:** default no-auth matches `flow/pkg/ingress`; `WithAuth` rejects before
  touching the session/read interfaces; auth errors return sanitized 401s; a future auth module can
  supply one authenticator function to both `flow` and `serve`. `serve.Server` timeout/TLS defaults
  are tested separately from handler auth.
- **Session list/read plane:** list tests cover absent `skip`, explicit `skip`, default `limit`,
  `limit` above the hard cap rejected with 400, stable sort, `next_skip`, and `done`; no plain
  `GET /sessions/{sid}` handler ships in v1.
- **Status read plane:** `GET /v1/sessions/{sid}/status` tests cover running, waiting-on-gate,
  idle, failed/interrupted/stopped, `last_journal_seq`, latest terminal turn, latest `StepDone`,
  and no-live-session reads. The handler is a projection passthrough — state-transition coverage
  (fold correctness) lives in `sessionstore` catalog tests, not `serve`.
- **SSE teardown:** disconnecting an `/events` client closes that request's subscription and removes
  it from fan-out without calling `Interrupt`; a headless session with no subscribers continues to
  append Enduring events and reaches status/journal normally.
- **Create/restore lifecycle:** `POST /sessions` tests cover idle create (`session_id` only),
  create-with-initial-blocks (`session_id` + `command_id`, `Submit` called after attach), runner
  failures, submit failures, malformed bodies, and `POST /sessions/{sid}/restore` reattaching the
  restored live session.
- **Sequenced seam tests (Phase 2):** `enduring` SSE frames carry `id:` equal to the delivery's
  `JournalSeq`; `ephemeral` frames never carry `id`. Join coverage proves every `enduring` event is
  delivered exactly once across subscribe-buffer → replay-to-tip → drop-`<=`-tip (no loss, no
  duplication), including an event appended inside the join window. Hub/journal envelope tests
  (append returns the seq; deliveries carry it; Ephemeral deliveries carry zero) live with
  `pkg/hub`/`pkg/journal` in Phase 0.
- **Hardening tests:** `serve.Server` refuses a non-loopback bind with nil auth (typed
  `PublicBindWithoutAuthError`) and allows it with `WithInsecurePublicBind()`; idle SSE streams
  emit `: ping` heartbeats on schedule; `POST /v1/sessions` with a repeated `Idempotency-Key`
  returns the original ids, and a reused key with a different body returns 409.
- **Concurrency:** fire-and-forget submit returns before the terminal event; a submit during a
  running turn surfaces `InputQueued`/`TurnFoldedInto`/`TurnRejected`, never blocks the handler.
- **Integration-tagged** (`//go:build integration`) SSE flush/teardown, upstream-down → typed
  error, loopback-vs-public gating.

## 12. Migration phases

- **Phase 0 (prerequisites — `pkg/session`, `pkg/hub`/`pkg/journal`/`pkg/event`,
  `pkg/sessionstore`):** add `session.Runner` (`Compile` → `Run`/`Restore`, lifecycle modeled on
  `flow.Runner`) separating the agent definition + deps (design time) from instantiation (runtime);
  widen `Session.SubscribeEvents` to return `event.Subscription`; **sequenced live delivery** —
  widen the hub appender seam to return the journal seq and make `Subscription.Events()` yield
  `event.Delivery{Event, JournalSeq}` (§7b); **catalog status projection** — extend the
  `sessionstore` catalog fold with `State`/`LastJournalSeq`/`ActiveTurnID`/`WaitingGateID` and the
  terminal turn/step summaries (§3d); delete the legacy `Approve`/`Deny`/`ProvideUserInput` trio in
  favor of `RespondGate`; confirm the rest of the `LiveSession` port matches existing
  `*session.Session` methods; decide the exact narrow read-plane interface over catalog/replay.
- **Phase 1 — reshape & wrap:** move `pkg/api`'s live+control handlers into `pkg/serve` (same repo);
  introduce `serve.Handler(runner, reads, opts...)`, the narrow `LiveSession` and `Runner` interfaces,
  an internal live-session table, and `serve.NewReader(reads)` for the read-plane route group
  (listing/status/journal), all mounted under `/v1`; delete the `Factory`/`AgentRequest` model and
  public attach/register surface. A pod hosts many sessions; HTTP create/restore are serve handlers
  over the supplied compiled `session.Runner`. `pkg/api` is replaced by `pkg/serve`. Gate replies use
  a normalized `ResponseRequest`/`GateResponse` path against opaque gate ids; no `serve` gate
  registry ships.
- **Phase 2 — protocol contract + ephemeral live-frame + lossless resume:** add
  `GET /v1/capabilities`, stable error envelopes, generated schema/OpenAPI artifacts, golden wire
  fixtures, the `event: ephemeral` SSE frame class, `id: <journal_seq>` stamping on live `enduring`
  frames (§7b), SSE heartbeats, and `Idempotency-Key` on create. Client SDKs may promise lossless
  resume from this phase via the client-side exact join.
- **Phase 3 — optional server-side join sugar:** `GET …/events?from_journal_seq=`
  replay-then-follow as a convenience for thin clients; no longer required for lossless resume.
- **Phase 4 — deferred, consumer/infra-owned:** `session_id` routing (helm charts / ingress),
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
3. **Runner-supplied, not factory-first.** `serve.Handler(runner, reads, opts...)` receives a compiled
   runtime runner and owns HTTP create/restore by calling `Runner.Run`/`Restore`. No
   `Factory`/`AgentRequest` in `serve`, no public attach/register API, and no agent config in the
   serving layer; only multi-pod *placement* is deferred to infra.
4. **`session.Compile` is design time; `Runner.Run`/`Restore` are runtime.** One driven surface
   (`*session.Session`/`serve.LiveSession`), two transports. `serve.Handler` must not compile or own
   agent config; it starts/restores sessions only through the supplied runner and keeps returned live
   sessions in an internal table. In particular `GET …/events` is exactly `SubscribeEvents` (live,
   lossy, no
   backlog); "from the beginning" is a store read (`…/journal`), not a session capability — `serve`
   invents neither, and does not fuse them into a replay-to-live join (that is a client concern / a
   future sequenced-source capability). `Runner.Restore` rebuilds a session from its journal to bring it back live
   it; it is not an event-replay API.
5. **Plug-and-play by construction; interfaces over the engine (Option B).** `serve` depends on the
   `Runner` and `LiveSession` interfaces, **not** `pkg/session` — `*session.Runner` and
   `*session.Session` satisfy them structurally, so the consumer passes the real compiled runner while
   handler tests pass fakes (§11). LLM, storage backend, tools, and posture are wired at the
   composition root and hidden behind the runner, live-session, and read-plane interfaces; `serve`
   never imports them.
6. **Posture-agnostic.** `Intent` (Interactive/Unattended) stays in harness `pkg/tools`, chosen
   at construction, discarded. Same endpoints for both modes; `ResponseRequest` POSTs are required
   when harness emits a gate event (interactive permissions or `AskUser`); `AskUser` still parks.
7. **No `serve` open-gate registry.** Harness' session-owned gate directory is authoritative for
   open gates and stale/random/wrong-kind responses. `serve` forwards response requests through
   `GateResponse` values with durable-acceptance semantics and projects authoritative gate errors to
   status codes; it never mirrors open-gate state locally.
8. **Ephemeral live-frame added.** `event: ephemeral` SSE frames carry token/tool-lifecycle
   deltas (live-only, unpersisted, no `seq`) through a live-only DTO. `TokenDelta` does not serialize
   `content.Chunk` directly; the transport maps chunks to tagged `text` / `thinking` / `tool_use`
   deltas. Live `enduring` frames use the existing durable event payload and carry `journal_seq`
   via the Phase 0 delivery envelope (§7b, Decision #16); `serve` stamps but never synthesizes it.
   The `MarshalEvent`-fails-closed-on-ephemeral invariant is preserved (separate transport
   encoder).
9. **Stable protocol contract for framework SDKs.** `serve` owns the versioned HTTP/SSE wire
   contract, including capability discovery, schema/OpenAPI artifacts, golden fixtures, SSE frame
   schemas, and one error envelope. React/Svelte/Vue/Angular/Solid packages adapt this protocol into
   framework state; `serve` does not import or branch for them.
10. **SSE-out + POST-in, not WebSockets.** Asymmetric duplex fits an agent session and keeps HTTP
   affordances. WebSocket is an opt-in, evidence-driven transport layered on top of routing.
11. **`serve` does not own infrastructure routing — the consumer does.** Sessions are addressed by
   `session_id`; routing to the owning pod is the **consumer's** deployment decision (realized in
   infra — future helm charts / ingress). A pod may host one session or hundreds (lightweight or
   kernel-sandboxed agents share a pod — §3c), so packing / isolation / stickiness granularity are
   the consumer's call. `serve` provides only an internal local map from `{sid}` to `LiveSession`; it
   does not implement cross-pod affinity, placement, or peer-forwarding.
   Affinity's *correctness* is backed by harness's single-writer fencing lease; failover is
   `Lost()`→`Runner.Restore` from the shared journal.
12. **The front-end/BFF is a separate future module** that consumes `serve`. BFF concerns (token
    custody, same-origin, `sid→pod` proxying) are *not* imposed on `serve`'s consumers.
13. **Listing/status/journal is a read-plane route group in `serve`, backed by
    sessionstore/storage.** `serve.NewReader(reads)` plus `serve.Handler` exposes paged `GET
    /sessions`, lightweight `GET /sessions/{sid}/status`, and cold-journal
    `GET /sessions/{sid}/journal` over a narrow adapter:
    `sessionstore.Catalog.ListSessions → []SessionMeta` for listing plus `Store.OpenEventReplayer`
    for raw journal reads and status. It is a *separate*, stateless route group (any pod, no lease,
    no infrastructure routing), distinct from the live/control routes backed by the internal
    live-session table. There is no `/export` endpoint, no `LiveSession.ExportSource`, and no
    transcript/markdown rendering dependency in `serve`. If a richer product view is needed later,
    it should live in a separate consumer/client package over the journal/status primitives. The
    listing intelligence lives in `sessionstore` today over the generic storage module; `serve` only
    projects it. Browser rendering and framework state adapters stay the client module's job.
14. **`session.Runner` separates design-time deps from runtime create/restore (Phase 0).** Modeled on
    `flow.Runner`: `session.Compile(cfg, store, opts…)` binds the agent definition (`loop.Config`:
    LLM, tools, posture) + deps into an immutable, reusable Runner and computes the config
    fingerprint (**design time**); `Runner.Run(ctx) (id, *Session, error)` mints a session id and
    brings up a live session, `Runner.Restore(ctx, sid) (*Session, error)` rebuilds from the journal
    and refuses a fingerprint mismatch (**runtime**). Per-run tuning is functional options into an
    unexported config — no exported `Deps`/`Config` grab-bag, matching flow's convention. Reuse
    flow's lifecycle invariants and naming discipline, not its concrete code: `flow.Runner[S]` and
    `session.Runner` do not share method signatures or state model, and a common `core.Runner`
    interface is YAGNI until an identical method set has at least two consumers. `serve` owns the
    HTTP create/restore layer over that runner, while the consumer still owns compile/design time and
    deployment placement.
15. **Auth matches `flow/pkg/ingress`: caller-supplied request authenticator.** `serve.WithAuth`
    accepts `func(*http.Request) error`, gates every serve-owned route before any session/read call,
    and returns sanitized 401s. `serve` does not bake bearer tokens, principals, tenants, or
    ownership into the package. A future auth module should expose adapters that produce the same
    function for both `flow/ingress.WithAuth` and `serve.WithAuth`, so consumers can wire auth
    cohesively across modules.
16. **Sequenced live delivery is a Phase 0 harness change, not a deferred storage capability
    (2026-07-08).** The journal seq is known at durable-append time (append-before-fan-out) and was
    being deliberately discarded at the hub's appender seam. Phase 0 widens that seam to return the
    seq and makes `Subscription.Events()` yield `event.Delivery{Event, JournalSeq}` (zero for
    Ephemeral). `serve` stamps `id: <journal_seq>` on live `enduring` SSE frames from Phase 2;
    lossless resume becomes a client-side exact join (subscribe-buffer → replay-to-tip → drop
    `<= tip`) with no `Follow` support and no server-side fusion. Mid-run reconnect (tab refresh,
    network drop) is the single most user-visible flow for a web UI, and shipping racy "best
    effort" join semantics into every framework adapter first would have made this contract far
    harder to fix later. Supersedes the deferred-sequenced-source stance previously recorded in
    Decisions #4/#8 and the old Phase 3.
17. **Status is a catalog-fold projection, not a per-request journal replay (2026-07-08).**
    `SessionStatus` extends `sessionstore.SessionMeta`, maintained by the existing catalog fold:
    `State` beyond `active`/`stopped` (running / waiting_on_gate / idle / failed / interrupted /
    stopped), `LastJournalSeq`, `ActiveTurnID`, `WaitingGateID`, and terminal turn/step summaries.
    `GET /v1/sessions/{sid}/status` is a KV read that scales with scheduler polling, and the
    session list can surface waiting-on-approval without N replays. Fold correctness is
    `sessionstore`'s to test; `serve` only projects.
18. **Wire hardening + one looprig HTTP convention set (2026-07-08).** `/v1` path prefix (matching
    `flow/pkg/ingress`), `Idempotency-Key` on `POST /v1/sessions` (mirroring flow run-create), SSE
    comment heartbeats + `Cache-Control: no-store`/`X-Accel-Buffering: no`, fail-secure public bind
    (`serve.Server` returns a typed `PublicBindWithoutAuthError` on a non-loopback bind with nil
    auth unless `WithInsecurePublicBind()` is passed), and an explicit no-CORS stance (browsers
    reach `serve` through a same-origin BFF; `ServeTransport` is for trusted/server-side callers).
    `serve` and `flow/pkg/ingress` share one error-envelope/auth/versioning/paging convention set.
19. **API names stay true to the session package (2026-07-08).** The runner's journal-rebuild
    method is `Restore` — matching the underlying `session.Restore` — not flow's `Resume`:
    `flow.Runner.Resume(ctx, id, payload)` delivers a payload to a parked task, while a session
    restore rebuilds a live session from its journal with no payload; borrowing flow's name would
    misstate the semantics. The HTTP route follows: `POST /v1/sessions/{sid}/restore`. The runner
    lifecycle naming (`Compile`/`Run`) still mirrors flow where the semantics genuinely match.
