# Design: flow run observability — the workflow UI surface

**Date:** 2026-07-08
**Status:** Draft (design discussion in session; this doc records the outcome).
**Spans:** `github.com/looprig/flow` (store + ingress additions) and the future
`github.com/looprig/client` module (UI). Recorded here beside its siblings; may move to
`flow/docs/plans/` when implementation starts there.
**Related:** [Serve HTTP session API](2026-07-06-serve-http-session-api-design.md) (the session
analogue whose HTTP conventions this shares), [Client web app](2026-07-02-client-web-app-design.md)
(the reference UI this extends), flow engine design (`flow/docs/plans/2026-06-24-flow-engine-design.md`).

## 1. Problem & goal

Flows already record rich, durable run data: every run has an append-only, revision-ordered
checkpoint history (`CheckpointStore.History`) carrying per-vertex states
(Pending/Running/Done/Interrupted/Failed with attempts, timestamps, errors), routing records,
phases, frontier, full state snapshots, and interrupts — the engine itself replays it on resume.
Human-in-the-loop is first-class (`flow.Interrupt` / `StatefulInterrupt`, resumed via
`Runner.Resume` or `POST /v1/runs/{id}/resume`).

What is missing is any way to *see* it:

- **No run catalog.** `CheckpointStore` is keyed by `GraphRunID` only; nothing can list runs. You
  must already know a run id to observe it.
- **HTTP exposes only the latest snapshot.** `pkg/ingress`'s `GET /v1/runs/{id}` reads `Latest`;
  the checkpoint history — the actual timeline — is unreachable over the wire.
- **No topology export.** Graph structure (vertices, edges, conditional routes) exists internally
  (the version-hash `canonicalForm` is built from it) but has no exported accessor, so a UI cannot
  render the DAG.
- **Nothing renders any of it.**

**Goal.** Give workflows the same observe-and-drive experience sessions get from
`pkg/serve` + the client module: list runs, read a run's step-by-step timeline, see the graph with
live vertex status, and answer pending interrupts — reusing the session client's SDK core,
transports, and HTTP conventions rather than growing a second stack.

## 2. Scope split: observability now, authoring later

Two very different problems hide in "UI for workflows"; this spec deliberately carries only the
first.

**Run observability (this spec).** The data model already exists; the work is exposure: a run
catalog, a history endpoint, a topology endpoint, and client rendering. No engine semantics change.

**Visual authoring (explicitly out of scope — a future spec).** Graphs are Go code binding
arbitrary closures (`Task`/`Selector`/`Reducer`/`Condition.Pick`); closures are inherently
non-serializable, and a browser cannot author them. Real visual authoring means a declarative
graph schema plus a registry of *named, registered* building blocks the schema binds by reference —
a fundamental shift in how flows are defined, not a UI feature. It must not leak requirements into
this spec. (Read-only DAG *rendering* is in scope here and does not depend on it.)

## 3. Flow module additions (`github.com/looprig/flow`)

### 3a. Run catalog

Mirror harness `sessionstore`'s catalog split: listing is a **store-side** concern, not the
engine's and not ingress's. Add a narrow listing surface beside `CheckpointStore`:

```go
type RunMeta struct {
    GraphRunID   GraphRunID
    GraphID      GraphID
    GraphVersion GraphVersion
    Status       RunStatus // running, interrupted, completed, cancelled
    Step         int
    Revision     uint64
    CreatedAt    time.Time
    UpdatedAt    time.Time
    CompletedAt  time.Time // zero unless terminal
    Interrupts   int       // pending interruption count (0 unless status == interrupted)
}

type RunCatalog interface {
    ListRuns(ctx context.Context, page Page) ([]RunMeta, error) // paged; stable sort UpdatedAt desc, GraphRunID asc
}
```

`RunMeta` is derivable from the checkpoint the store already receives on every `Append` — backends
maintain it as a fold/index at append time (no scan-all). `MemStore` and the `pkg/nats` store both
implement it; the store conformance suite covers it. An optional `graph_id` filter is the only
query in v1.

### 3b. History endpoint (the run journal)

`GET /v1/runs/{id}/history?from_revision=<n>&limit=<n>` — a paged, revision-cursor read over
`CheckpointStore.History`. Same paging contract as serve's journal endpoint: `from_revision`
inclusive (0/absent = beginning), `limit` default 100 / hard cap 1000, response
`{"checkpoints":[…],"next_revision":<n>,"done":<bool>}`.

The checkpoint DTO carries run state, phase, frontier, vertex states, route records, interrupts,
and halt. **The user state `S` is excluded by default** (least privilege — `S` is arbitrary caller
data and may hold sensitive values); `?include_state=true` opts in explicitly, and deployments can
disable it in ingress options. Revisions are exact and append-only, so there is **no live/cold
seam problem at all** — a client pages history from its last revision and never misses or doubles
a step.

### 3c. Run listing endpoint

`GET /v1/runs?skip=<n>&limit=<n>[&graph_id=<uuid>]` over the run catalog — same `skip`/`limit`/
`done` paging contract as serve's session list.

### 3d. Topology export

Add read-only introspection to the compiled runner — the data already exists internally
(`canonicalForm` proves it); this exports it without touching closures:

```go
type Topology struct {
    GraphID      GraphID
    GraphVersion GraphVersion
    Entry        VertexID
    Finish       VertexID
    Vertices     []TopologyVertex          // {ID, Label} — label optional
    Edges        []TopologyEdge            // {From, To}
    Conditional  []TopologyConditionalEdge // {From, Targets []VertexID}
}

func (r *Runner[S]) Topology() Topology
```

Ingress: `GET /v1/graphs/{graphID}/topology` (per served version). Vertex `Label` comes from a new
additive vertex option (`flow.WithVertexLabel("score-candidates")`); absent labels fall back to
short ids in the UI. Output ordering is canonical (sorted, like `canonicalForm`) so golden
fixtures are stable. This is JSON topology for rendering — DOT export stays a non-goal.

### 3e. Live progress: polling, deliberately

Flow progress is **checkpoint-granularity** — there is no token firehose, so serve's
ephemeral-frame machinery has no business here. v1 progress is polling: `GET /v1/runs/{id}` for
cheap status, `GET /v1/runs/{id}/history?from_revision=` for the incremental timeline (the
revision cursor makes polling exact and cheap). A `Hooks`→SSE bridge (`event: checkpoint` frames,
same SSE conventions as serve) is recorded as **evidence-driven future work** — build it only if
polling proves insufficient for a real consumer, not before.

### 3f. Human-in-the-loop — nothing new needed

`Interruption` (kind `Awaiting` vs `Errored`, user-facing info) already surfaces through
`GET /v1/runs/{id}` and the history DTO; `POST /v1/runs/{id}/resume` already accepts the payload.
The UI renders `Awaiting` interruptions as approval/input cards — the same UX pattern as session
gates — and resumes with the payload. The engine changes not at all.

## 4. Shared HTTP conventions with `serve`

One looprig protocol family, not two dialects (serve Decision #18). Flow ingress already matches
serve on `/v1`, the `func(*http.Request) error` auth seam, secure `Server` defaults, and
`Idempotency-Key`; this spec adds the rest:

- **Error envelope:** ingress adopts serve's `{"error":{"code","message","retryable"}}` shape for
  all new routes (and migrates existing ones as a compatible superset where possible).
- **Paging:** `skip`/`limit`/`done` for lists; inclusive `from_revision` cursor for history —
  the exact shapes serve uses.
- **Capabilities:** `GET /v1/capabilities` on ingress, mirroring serve's
  (`{"protocol":"looprig.flow","version":1,"features":["runs_list","history","topology","resume"]}`),
  so one client SDK negotiates both surfaces uniformly.
- **Fail-secure public bind:** ingress `Server` adopts the same refusal serve specs — non-loopback
  bind with nil auth returns a typed error absent an explicit insecure opt-in.

## 5. Client side (`github.com/looprig/client`)

- **`flows` namespace in the same SDK core** (`@looprig/client`) — not a separate package: zod
  types generated from flow ingress wire types through the same schema pipeline, the same typed
  error envelope, and a run state machine that folds `RunMeta` + history pages into a
  timeline/DAG-status view model. Framework adapters stay thin, exactly as for sessions.
- **BFF: proxy-only for flows.** The BFF reverse-proxies `/api/v1/flows/…` → the configured flow
  ingress host with the same token custody rules. Unlike sessions, there is no direct-store mount:
  sessions needed it for browse-with-no-host-up; a flow deployment always has its ingress next to
  its store, and polling reads are cheap. The asymmetry is deliberate — revisit only if a real
  "browse flow history with no flow service" need appears.
- **Reference app:** a Runs list view (status, graph, updated-at, pending-interrupt badge) and a
  run detail view — DAG (topology + live vertex-status overlay, polled), step timeline (history),
  and interrupt cards (resume form). DAG layout needs a client-side layout library (elkjs or
  dagre) — an npm dependency requiring approval in the client repo's CLAUDE.md when Phase F2
  starts.

## 6. Security (per CLAUDE.md)

- `WithAuth` gates all new ingress routes; validate at the boundary (`{id}`/`graph_id` parse as
  ids, paging params bounded, bodies size-limited via the existing `WithMaxBodyBytes`).
- **State exclusion by default** (§3b): checkpoint `S` and `StepBase` are omitted from history
  DTOs unless explicitly requested *and* the deployment enables it — arbitrary user state is
  treated as sensitive (least privilege, fail secure).
- Typed errors for each failure (`RunNotFoundError`, `GraphNotServedError`, `HistoryReadError`, …);
  audit resume/cancel actions without logging payloads.

## 7. Testing (per CLAUDE.md)

- Table-driven, `-race` always. Run-catalog conformance tests run against `MemStore` and the NATS
  store through the shared store suite (fold correctness: status transitions, interrupt counts,
  stable sort, paging).
- History handler: absent/explicit `from_revision`, default and capped `limit`,
  `next_revision`/`done`, state excluded by default / included on opt-in / hard-disabled by
  option, malformed params, store errors → typed HTTP failures.
- Topology: golden JSON fixtures (canonical ordering); label fallback; conditional edges;
  round-trip against `canonicalForm`'s vertex/edge sets so the export can never drift from the
  version hash's view of the graph.
- Contract corpus: flow wire types feed the same schema→zod pipeline; `sdk/core` vitest parses the
  same fixtures. CI regen-diff, as for serve.
- Integration-tagged: ingress + real store, resume-from-interrupt round trip driven through the
  new endpoints.

## 8. Explicitly out of scope

- **Visual authoring** (declarative graph schema, registered building blocks, codegen/interpreter)
  — future spec, per §2.
- **Streaming task output** and the `Hooks`→SSE bridge (evidence-driven future work, §3e).
- **DOT export** (JSON topology only).
- **Cross-run analytics dashboards** (charts land with the client doc's Phase 2+ tooling if
  wanted).

## 9. Phases

- **F0 — flow store:** `RunCatalog` (`ListRuns` + `RunMeta` fold/index) with conformance tests;
  topology introspection (`Runner.Topology()`) + `WithVertexLabel`.
- **F1 — ingress:** `GET /v1/runs`, `GET /v1/runs/{id}/history`, `GET /v1/graphs/{graphID}/topology`,
  `GET /v1/capabilities`, error-envelope alignment, fail-secure bind.
- **F2 — client:** `flows` namespace in `sdk/core`, BFF proxy routes, Runs list + run detail
  (DAG/timeline/interrupt cards), poll-based.
- **F3 — evidence-driven:** `Hooks`→SSE checkpoint tail, only if polling proves insufficient.
- **Future spec:** visual authoring.

F0/F1 are independent of the serve/client session work and can proceed in parallel; F2 lands after
the client module's session Phase 1 exists to extend.

## Decision log (2026-07-08)

1. **Observability-first; authoring out of scope.** Closures are non-serializable; visual
   authoring needs a declarative schema + registered building blocks — its own future spec. This
   spec only exposes what already exists durably.
2. **Run catalog is a store concern**, mirroring harness `sessionstore`'s catalog: fold/index
   maintained at `Append` time, conformance-tested across backends, never a scan.
3. **History over HTTP is a paged revision cursor.** Append-only revisions make run timelines
   exact by construction — no live/cold seam, no sequenced-delivery work needed (unlike sessions).
4. **Polling over streaming for v1.** Checkpoints are step-granularity; serve's ephemeral
   machinery is not replicated. SSE bridge only on evidence (F3).
5. **Topology export via new read-only introspection** (`Runner.Topology()` +
   `GET /v1/graphs/{graphID}/topology`) with optional `WithVertexLabel`; canonical ordering keeps
   fixtures stable; DOT stays a non-goal.
6. **One HTTP convention set with serve** (serve Decision #18): `/v1`, auth seam, error envelope,
   paging shapes, `Idempotency-Key`, capabilities, fail-secure public bind.
7. **Client: `flows` namespace in the existing SDK core; BFF proxy-only for flows.** No
   direct-store mount — the browse-without-host requirement that justified it for sessions does
   not exist here; the asymmetry is recorded and deliberate.
8. **Checkpoint state `S` excluded from wire DTOs by default** (least privilege); explicit opt-in
   per request and per deployment.
