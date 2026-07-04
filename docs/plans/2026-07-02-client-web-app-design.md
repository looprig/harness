# Design: client — a web app for browsing and driving looprig sessions

**Date:** 2026-07-02
**Status:** Approved (design discussion in session; this doc records the outcome)
**Depends on:** `2026-07-02-storekit-sessionstore-design.md` (reads through `pkg/sessionstore`),
`2026-07-02-workspacestore-design.md` (Phase 2 workspace views)

## Problem

looprig can run a session and, through `pkg/api`, expose a per-session HTTP runner
(create/resume, submit a turn, resolve permission gates, stream live events over SSE, export
an HTML transcript). What it has **no** surface for is the thing a human actually wants day to
day: *browse every session that ever ran, read its transcript and journal, and — for the ones
still alive — drive them.* Session **listing** and **journal history** exist only as Go
interfaces (`sessionstore.Store.Catalog()`, `EventReplayer`); `pkg/api` holds only an
in-memory map of *live* sessions and has no list/history endpoint. Nothing renders any of it.

We want one artifact that fills that gap, that ships as a **single Go binary** for a laptop and
also runs as a **thin client against a session living in a sandbox or the cloud**, and that is
**small and fast** — a rich but lightweight dashboard, not an Electron slab.

## The shape: one client, two backends, two modes

The two things a user wants have *fundamentally different* sources, and separating them is the
whole design:

- **Read plane — history & listing.** List sessions; read a session's journal; reconstruct its
  transcript. This data lives in the **session store** and can be read with **no live agent
  running** — straight from the store. Locally the backend is `fsstore`; in the cloud it is the
  shared `natsstore` (or `pgstore`+object store). *This is "read the sessionstore directly."*
- **Live plane — a running session's tail.** A *currently running* session's transcript-as-it-
  happens plus gate/status. Source: looprig **session events** over `pkg/api`'s SSE
  `GET /sessions/{sid}/events`. It needs the session's **host** to be up.
- **Control plane — driving a running session.** Submit a turn, approve/deny a gate, interrupt,
  create/resume. Source: `pkg/api`'s POST endpoints on the **host**.

```
        ┌──────────────────── client (new module) — one Go binary ─────────────────────┐
        │  //go:embed dist  →  Svelte 5 SPA        SPA talks ONLY to this local BFF      │
        │                                          (same origin; no CORS; token stays    │
        │                                           server-side)                         │
        │                                                                                │
        │  READ PLANE   ── sessionstore.Open(backend) DIRECT ──────────────┐             │
        │    local:  fsstore            (no NATS, one binary)              ├─► list       │
        │    cloud:  natsstore / pgstore+object store                     │   journal     │
        │    → Catalog().Keys/Get  ·  OpenEventReplayer → cold events     │   transcript  │
        │                                                                 ┘               │
        │  LIVE + CONTROL ── pkg/api HTTP client, reverse-proxied ──► session HOST         │
        │    GET /sessions/{sid}/events (SSE tail) · POST input/gates/interrupt/resume     │
        │    host = local swe process  OR  remote sandbox/cloud api                        │
        └──────────────────────────────────────────────────────────────────────────────────┘
             depends on: looprig SDK  +  one storekit backend (chosen at composition)
             NEVER depends on: swe (no agent Factory here — the client hosts nothing)
```

- **Read is direct-to-store; control is proxied.** The client is a **backend-for-frontend
  (BFF)**: it owns the store connection and the remote bearer token, and the SPA only ever
  speaks to it, same-origin.
- The client is **backend-agnostic** by Dependency Inversion (per CLAUDE.md): it depends on the
  `sessionstore` *facade*; which storekit backend is wired (`fsstore` vs `natsstore`) is a
  `main`-time decision the client is blind to. "Local without NATS" is just a different backend
  swapped in — zero client changes.
- The client **hosts no agent**, so it never imports swe. It cannot *itself* be the compute for
  a session; it can proxy create/resume/input/gates/interrupt to a host that has the `Factory`,
  and it can browse all history with no host at all.

## Module & dependency boundary

A new standalone module — working name **`github.com/looprig/client`** (no `looprig-` prefix,
matching the storekit backend-repo convention; final name is the owner's call).

| Depends on | For |
|---|---|
| `github.com/looprig/harness` — `pkg/sessionstore` | list (`Catalog`), cold replay (`OpenEventReplayer`/`OpenRecordReplayer`) |
| looprig — `pkg/journal`, `pkg/event`, `pkg/content`, `pkg/tool` | decode replayed/streamed records into the event DTO |
| looprig — `pkg/transcript` (optional) | HTML-export shortcut (below) |
| one storekit backend — `ciram-co/fsstore` **or** `ciram-co/natsstore` | the read-plane store, chosen at composition |
| stdlib `net/http` | the BFF server + the `pkg/api` reverse-proxy client |

**Not** depended on: `swe`, any agent implementation, any charm/TUI stack. The module is a
consumer of the looprig SDK exactly like swe, minus the agent.

Session **creation/resume** is a host (swe) job — the client proxies `POST /sessions` to a
configured host, which owns the `Factory`. swe may *also* embed this same SPA for an all-in-one
local dev binary that both runs and shows sessions; that is a swe concern, out of scope here.

## The BFF surface (client → SPA contract)

The Go binary serves the embedded SPA and a small same-origin JSON/SSE API. One **event DTO**
(a stable, versioned JSON projection of `pkg/event`) flows through both history and live, so the
SPA has a *single* renderer fed in two segments.

| Method + path | Plane | Backed by |
|---|---|---|
| `GET /api/sessions` | read | `sessionstore.Store.Catalog().Keys/Get` → list DTO |
| `GET /api/sessions/{id}` | read | `Catalog().Get` → one session's metadata |
| `GET /api/sessions/{id}/events?from=&to=` | read | `OpenEventReplayer` cold replay → event DTO (paged) |
| `GET /api/sessions/{id}/transcript.html` | read | `pkg/transcript` + `html` (server-rendered shortcut; see below) |
| `GET /api/sessions/{id}/live` | live | **reverse-proxy** of host `GET /sessions/{id}/events` (SSE) |
| `GET /api/sessions/{id}/gates` | live | reverse-proxy of host `GET …/gates` (open-gate snapshot on reconnect) |
| `POST /api/sessions/{id}/input` | control | reverse-proxy of host `POST …/input` |
| `POST /api/sessions/{id}/gates/{tid}` | control | reverse-proxy of host `POST …/gates/{tid}` |
| `POST /api/sessions/{id}/interrupt` | control | reverse-proxy of host `POST …/interrupt` |
| `POST /api/sessions` `DELETE /api/sessions/{id}` | control | reverse-proxy of host create/resume/delete |

- **Same-origin only.** The SPA never holds the remote token or hits the host directly; the BFF
  injects `Authorization` on the proxied leg. No CORS surface.
- **Event DTO is versioned** (`{"v":1, …}`) from day one; it decodes both a replayed
  `journal` record and a live SSE `event` into one shape (message blocks, tool cards, gate
  prompts, subagent/step markers, status). The seam between "history" and "live" is a single
  sequence number: the SPA replays `…/events` up to the tip, then attaches `…/live` (seam
  integrity requirements below).
- **`DELETE` stops the live session only** (`pkg/api` semantics); it never removes history
  from the store. The UI presents it as *stop*, not *remove from list* — store deletion is
  retention/GC policy, out of scope here.

### Why client-side rendering, not just the HTML export

`pkg/api` already serves a rendered HTML transcript (`GET …/export`, via `pkg/transcript` +
`pkg/transcript/html`). That is the **fast shortcut** and a fine Phase-1a milestone. But the
target is the **event DTO rendered by Svelte**, because the user wants *live* streaming into
*rich, interactive* chat/tool/code components — which static HTML can't do. The renderer that
folds the DTO is the app's core; the HTML export stays available for a plain read-only view and
as a rendering-parity oracle in tests.

## Live tail comes from session events, not journal follow

`journal.EventReplayer` is **cold-replay only** today — `Follow:true` returns
`FollowUnsupportedError`. v1 does **not** need it: the live tail is looprig **session events**
over the host's SSE `/events`, exactly the source that already exists. So a running session
reads as *cold replay from the store up to the tip* (works even if the host is down) *plus the
SSE tail* (when a host is reachable). Implementing `EventReplayer.Follow` for a host-independent
live tail direct from the store is future work, not a v1 blocker.

**Seam integrity (Phase-1b verification item).** Replay-to-tip-then-attach is only sound if
the live stream can be joined without loss or duplication: either the SSE endpoint accepts a
resume position (e.g. `?from_seq=`) or every SSE frame carries its journal sequence so the SPA
can drop the overlap window. `pkg/api`'s `/events` is fed by the durable event tap, so
sequences are expected to be available — but this must be **verified before 1b is built**, and
a small `pkg/api` addition (resume position or seq-in-frame) is in scope for Phase 1b if it
falls short. Attaching to a running session is exactly when users look hardest; dropped or
doubled events at the seam are not acceptable.

## Frontend stack

**Svelte 5**, built as a **static client-side-rendered SPA** and embedded via `//go:embed`.

- **Build:** SvelteKit with `adapter-static` + `export const ssr = false` (root layout) →
  pure static assets, **no Node at serve time**. This is Tauri's documented path and embeds
  cleanly into `embed.FS`. (SvelteKit is used purely as the router/build/tooling host; none of
  its SSR/server features are in play.)
- **Components (all verified current, 2026-07-02):**
  - Dashboard/UI: **shadcn-svelte** (on Bits UI) — cards, data table, dialogs, command palette.
  - Chat/transcript: **Svelte AI Elements** (shadcn-svelte registry) — message list, streaming
    bubbles, reasoning/tool panels, autoscroll, composer. Used **presentationally**: they render
    our event DTO; we do **not** adopt `@ai-sdk/svelte`'s transport, which speaks the Vercel AI
    data-stream protocol, not looprig's event/journal model. The protocol/transport layer is
    ours regardless of framework. (It's a community port, ~180★ — but shadcn-registry installs
    vendor the components into our tree, so upstream abandonment risk is capped: we own the
    code the moment it's installed.)
  - Long transcripts: **virtua** (`Svelte >= 5`, stick-to-bottom/reverse-scroll) — virtualize
    thousands of streamed events.
  - Code blocks: **Shiki** (framework-agnostic; `codeToHtml` in the code renderer).
  - Markdown: **svelte-exmarkdown** (runtime-dynamic; fenced code routed to Shiki).
  - Charts (Phase 2+ dashboards): **LayerChart** / **uPlot** for time-series.
- **Why Svelte over Solid (recorded):** the app's differentiators are rich prebuilt dashboard +
  chat components; Svelte's ecosystem leads there (shadcn-svelte breadth, an AI-Elements chat
  kit) while Solid would mean hand-building the chat presentation layer. The raw perf/footprint
  edge Solid holds is negligible here with virtua virtualization. Framework-agnostic pieces
  (Shiki, virtua, uPlot, static-SPA build) are a wash. Full head-to-head in the decision log.

## Deployment modes

| | Read backend | Live/control host | Auth | Binary |
|---|---|---|---|---|
| **Laptop** | `fsstore` (local dir; no NATS) | local swe on loopback | none (loopback) | one binary, `//go:embed` SPA |
| **Cloud client** | `natsstore` / `pgstore`+object store (shared) | remote sandbox/cloud `pkg/api` | bearer + TLS | one binary; BFF holds the token |
| **Browse-only** | any read backend | — (no host) | read-token only | history + transcripts, no control |

The client code is identical across all three; only composition-root wiring (backend choice,
host URL, credentials) differs.

- **v1 is single-host.** The BFF proxies live/control to *one* configured host URL. A fleet of
  scale-to-zero sandboxes (one host per session) needs a session→host routing map, which
  nothing owns yet — recorded here as future work, not silently assumed.
- **Binary composition is honest about deps.** One binary supporting both modes by config
  links *both* backends — the NATS client rides along unused in local mode. looprig core
  itself has zero NATS dependency (storekit extraction, in implementation), so if minimal
  size is a hard goal, build two thin `main`s (or build tags) over the same client package:
  the laptop binary then links `fsstore` only and contains no NATS at all.

## Auth & security (per CLAUDE.md)

- **Loopback default.** The BFF binds `127.0.0.1` by default; a public bind is opt-in and gated,
  mirroring `pkg/api`'s `AllowPublic` discipline. Fail secure: no host/credentials configured →
  read-only, never a fall-through to control.
- **Token stays server-side.** The remote host's bearer token lives only in the BFF process
  (env var / secrets manager — never in code, never shipped to the browser, never logged). The
  BFF injects it on the proxied leg; the SPA is same-origin and unauthenticated to the host.
- **TLS** to any remote host: `MinVersion: tls.VersionTLS12`, never `InsecureSkipVerify`.
- **Explicit `http.Server` timeouts** (Read/Write/Idle, `MaxHeaderBytes`) on the BFF; every
  proxied/store call is `context`-bounded. SSE proxying uses a flush loop with an idle deadline,
  not an unbounded copy.
- **Validate at the boundary.** Session IDs are parsed as `uuid.UUID`; `from`/`to` are bounded
  integers; the reverse proxy forwards only an allowlisted set of paths/methods (never an
  arbitrary upstream path). Any served file path is `filepath.Clean`'d and confined to the
  embedded FS.
- **Typed errors** for every distinct BFF failure (`UpstreamUnavailableError`,
  `SessionNotFoundError`, `StoreReadError`, …); `errors.As` at call sites; audit auth failures
  and denied gates without logging payloads/tokens/PII.

## Repo architecture

```
github.com/looprig/client
├── cmd/
│   ├── looprig-client/        # dual-mode main (links both backends; convenience)
│   └── looprig-client-local/  # fsstore-only main — the no-NATS laptop binary
├── pkg/                       # exported: swe may embed the BFF + SPA (all-in-one dev binary)
│   ├── dto/                   # versioned event DTO v1: looprig event/record → JSON projection
│   ├── bff/                   # BFF library: Handler(cfg, deps) http.Handler / Serve(ctx, …)
│   │   ├── read/              # read-plane handlers (list, cold events, transcript)
│   │   └── proxy/             # live SSE + control reverse-proxy to the host
│   └── webui/                 # //go:embed of the built SPA + hashed-asset/SPA-fallback handler
├── internal/                  # app-private helpers (config parsing, logging setup)
├── web/                       # SvelteKit app (adapter-static, ssr=false)
│   └── src/lib/{api,dto,transcript,components}/
├── contract/                  # the Go↔TS contract: schema/v1.json + golden DTO fixtures
├── docs/plans/ · Makefile · CLAUDE.md · .github/workflows/
```

- **`pkg/` is exported deliberately**: the design anticipates swe embedding this SPA for an
  all-in-one binary; `bff.Handler` + `webui.FS` make that a one-liner, mirroring looprig's
  `api.Handler` pattern. Everything without a reuse story stays `internal/`.
- **Import discipline (DIP):** only `cmd/` imports a storekit backend; `pkg/bff` depends on
  its **own narrow consumer interfaces** — `Catalog` (list/get metadata), `Replayer`
  (cold event cursor), `Host` (live SSE + gates snapshot + input/resolve/interrupt) — satisfied
  by thin adapters over `sessionstore` and one `net/http` host client. Interface segregation
  buys: tests run against memstore-backed adapters + an `httptest` stub host, and
  `Host == nil` ⇒ browse-only mode falls out of the type system (no control routes registered —
  fail secure, not a 403 fall-through).
- **Config** is a typed struct parsed from env at the composition root only, validated,
  fail-loud (`CLIENT_ADDR` loopback default, `CLIENT_STORE`, `CLIENT_HOST_URL`,
  `CLIENT_HOST_TOKEN` — secret, required iff host configured, never logged). No config type
  passes below `cmd/`; handlers receive narrow interfaces and pre-validated values.

### Go↔TS contract & type generation

`pkg/dto` is the **single source of truth** — plain structs, explicit JSON tags, a `kind`
discriminator. Generation never reads looprig's `pkg/event` directly: that sealed sum type
evolves with looprig, and the DTO seam exists precisely to decouple the SPA from it. The
looprig→DTO mapping stays hand-written Go (one `switch` over event types — it *is* the seam);
DTO→TypeScript is generated:

```
pkg/dto ──go:generate──► contract/schema/v1.json ──npm run gen──► web/src/lib/dto/gen.ts
    (invopop/jsonschema,       (language-neutral                (json-schema-to-zod;
     reflection, dev tool)      contract artifact)               TS types via z.infer)
```

- zod is the single TS source of truth: runtime **parse** (not cast) at the SPA boundary —
  validate-at-every-boundary applies to the browser too — with static types inferred.
- The schema lives beside the **golden fixtures** in `contract/` (one JSON file per event
  shape). Go tests validate fixtures against the schema; vitest parses the same fixtures
  through the generated zod. Drift cannot merge from either direction.
- CI guard: `make contract` regenerates schema + zod; `git diff --exit-code` fails the build
  if `pkg/dto` changed without regeneration.
- **Approved deps (2026-07-02):** `github.com/invopop/jsonschema` (Go, dev/tool only) and
  `json-schema-to-zod` (npm, dev only) — recorded in the client repo's own CLAUDE.md, which
  inherits looprig's rules and seeds its approved list (looprig, fsstore/natsstore; npm:
  svelte/vite/shadcn-svelte/AI Elements/virtua/shiki/svelte-exmarkdown/zod).

### Build pipeline

- `make web` → `npm ci && vite build` → `pkg/webui/dist/` (**gitignored**; a committed
  one-line placeholder `index.html` keeps `go build`/`vet`/`test` green without Node).
- `make build` → depends on `web`; `CGO_ENABLED=0 go build -trimpath ./cmd/...`.
- `make secure` → looprig's gauntlet (fmt-check, vet, staticcheck, gosec, govulncheck)
  **plus** `svelte-check` + eslint over `web/`.
- **Dev loop:** `vite dev` proxying `/api → 127.0.0.1:<bff>`; same-origin in prod, proxied in
  dev — CORS never exists anywhere. Local multi-repo dev via an uncommitted `go.work`.
- Release tooling (goreleaser vs plain make) is an **open question** — a new tool dep
  requiring approval when it comes up.

## Testing (per CLAUDE.md)

- Table-driven, `-race` always. The BFF's read handlers test against `storekit/memstore` behind
  `sessionstore` (fast, deterministic, no NATS).
- Event-DTO codec: a fuzz target (external input → decode) and round-trip tests that the DTO
  from cold replay and from a live SSE frame are byte-identical for the same event; **parity
  test** that the DTO renderer and the `pkg/transcript/html` export agree on a corpus.
- Contract corpus: Go validates `contract/` fixtures against `schema/v1.json`; vitest parses
  the same fixtures through the generated zod; CI fails on unregenerated `pkg/dto` changes
  (`make contract` + `git diff --exit-code`).
- Reverse-proxy handlers: integration-tagged (`//go:build integration`) against a stub
  `pkg/api` host — auth injection, path allowlisting, SSE flush/teardown, upstream-down → typed
  error.
- SPA: component tests for the transcript renderer folding a recorded DTO stream (history →
  live seam; virtualized long transcript; gate prompt round-trip).

## Migration phases (detail in the implementation plan)

- **Phase 0 (prerequisite, in implementation):** `pkg/sessionstore` read surface lands
  (`Catalog` listing + `OpenEventReplayer`) per the storekit/sessionstore spec. This is a
  **hard** prerequisite with no fallback: the legacy listing paths
  (`journal.Catalog.ListSessions`, `pkg/persistence`) are deleted by the storekit extraction
  along with looprig's NATS dependency, so the client is written against `sessionstore` only.
- **Phase 1 — the client (v1):**
  - 1a. BFF read plane + event DTO + `GET /api/sessions`, `…/events` (cold), HTML-export
    shortcut; Svelte shell that lists sessions and shows a cold transcript.
  - 1b. Live plane: SSE reverse-proxy + the history→live seam in the renderer.
  - 1c. Control plane: input / gates / interrupt / create-resume reverse-proxy; the interactive
    chat composer + gate-approval UI.
  - 1d. Auth + TLS + loopback/public gating; the two deployment modes wired at composition.
- **Phase 2 — workspaces:** once `pkg/workspacestore` exists, add a workspaces/snapshots view
  (list `WorkspaceCheckpointed` refs from the journal; browse snapshot metadata).
- **Phase 3 — desktop/mobile:** wrap the *same* static SPA in **Tauri v2** (desktop + iOS/
  Android); it points at a bundled-or-remote BFF. No SPA changes.

## Decision log (from design discussion, 2026-07-02)

1. **Pure client, looprig-only.** The module hosts no agent and never imports swe; it browses
   history from the store and drives running sessions by proxying to a host. Session
   creation/resume is a host job the client proxies.
2. **Read plane / live plane / control plane split.** History & listing come from
   `sessionstore` directly (any backend); a running session's tail comes from `pkg/api` SSE
   session events; control comes from `pkg/api` POST — three distinct sources, one SPA.
3. **Session listing is a `sessionstore` concern**, not `pkg/api` (a live-only runner) nor
   `pkg/session` (the runtime engine). The client depends on the catalog facade.
4. **Backend-agnostic via DIP.** The client depends on the `sessionstore` facade; `fsstore`
   (local, no NATS) vs `natsstore` (cloud) is a composition-root swap. "Local without NATS" is
   just a different backend.
5. **BFF pattern.** One Go binary = embedded SPA + same-origin JSON/SSE API; it owns the store
   connection and the remote token; the SPA never holds credentials or hits the host directly.
6. **Live tail from session events, not `EventReplayer.Follow`** (unimplemented). Cold replay
   for history + SSE for the tail; host-independent store-follow is future work.
7. **Client-side rich rendering via a versioned event DTO**, with `pkg/transcript` HTML export
   kept as a shortcut and a rendering-parity oracle.
8. **Svelte 5 over SolidJS** (both web-verified 2026-07-02): chosen for prebuilt dashboard +
   chat component breadth (shadcn-svelte, Svelte AI Elements) — the app's differentiators —
   over Solid's marginal perf/footprint edge, which virtua virtualization neutralizes.
   SvelteKit is used only in `adapter-static`/`ssr=false` mode (pure static output for
   `embed.FS` + Tauri); its SSR/server features are out of scope. `@ai-sdk/svelte`'s
   *transport* is **not** adopted — looprig's event stream is its own protocol — only AI
   Elements' presentational components.
9. **Workspaces deferred to Phase 2**, gated on `pkg/workspacestore` (design-only today).
10. **Desktop/mobile deferred to Phase 3** via Tauri v2 wrapping the same static SPA.
11. **Review fixes (2026-07-02):** gate-snapshot proxy added (`GET …/gates` — without it a
    reloaded SPA can't reconstruct pending approvals); history→live **seam integrity** named a
    Phase-1b verification item (SSE must be seq-resumable or seq-carrying; a small `pkg/api`
    addition is in scope if not); **v1 scoped to a single configured host** — session→host
    routing for sandbox fleets is explicit future work; per-mode thin `main`s/build tags noted
    so the laptop binary links no NATS (the storekit extraction — in implementation — removes
    NATS from looprig core, so Phase 0 is a hard prerequisite with **no legacy
    `journal.Catalog`/`persistence` fallback**); proxied `DELETE` clarified as
    stop-live-only, never store deletion.
12. **Repo architecture (2026-07-02):** standalone module with exported `pkg/{dto,bff,webui}`
    (so swe can embed the BFF + SPA), `cmd/` as the only backend-importing layer, BFF-owned
    narrow consumer interfaces (`Catalog`/`Replayer`/`Host`; `Host == nil` ⇒ browse-only by
    construction), gitignored SPA build with committed placeholder for Node-free `go test`.
13. **Type generation (2026-07-02):** JSON Schema pipeline chosen over direct Go→TS (tygo)
    and no-codegen. Source of truth is `pkg/dto` — never looprig `pkg/event` directly.
    Approved: `github.com/invopop/jsonschema` (Go dev tool), `json-schema-to-zod` (npm dev).
    zod is the TS source of truth (runtime parse at the SPA boundary; types via `z.infer`);
    golden fixtures + schema validation + CI regen-diff make drift unmergeable.
