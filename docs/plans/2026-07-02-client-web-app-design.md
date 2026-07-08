# Design: client — framework-neutral session client + reference web app

**Date:** 2026-07-02
**Status:** Approved (design discussion in session; this doc records the outcome)
Revised 2026-07-08: reconciled against `2026-07-06-serve-http-session-api-design.md` — paths,
params, and gate ids aligned; `DELETE` dropped (serve ships no v1 destroy); the BFF read plane now
**mounts `serve`'s reader** instead of reimplementing it; wire-DTO ownership moved to harness
`pkg/serve`; lossless resume moved up (sequenced live delivery is a harness Phase 0 capability,
serve §7b). See Decision #15.
**Depends on:** `2026-07-06-serve-http-session-api-design.md` (the wire contract this client
consumes), `2026-07-02-storekit-sessionstore-design.md` (reads through `pkg/sessionstore`),
`2026-07-02-workspacestore-design.md` (Phase 2 workspace views)

## Problem

looprig can run a session and, through `pkg/serve`, expose a per-session HTTP runner
(create/restore, submit a turn, resolve permission gates, stream live events over SSE, and read
history). What it has **no** surface for is the thing a human actually wants day to
day: *browse every session that ever ran, read its transcript and journal, and — for the ones
still alive — drive them.* Session **listing** and **journal history** exist only as Go
interfaces (`sessionstore.Store.Catalog()`, `EventReplayer`); the legacy `pkg/api` held only an
in-memory map of *live* sessions and has no list/history endpoint. Nothing renders any of it.

We want one artifact that fills that gap, that ships as a **single Go binary** for a laptop and
also runs as a **thin client against a session living in a sandbox or the cloud**, and that is
**small and fast** — a rich but lightweight dashboard, not an Electron slab.

We also want the same flexibility that makes Vercel AI SDK easy to adopt: the backend protocol is
stable, and framework-specific packages are only thin adapters over a shared client core. The
first-party UI remains a Svelte 5 app, but Svelte is the **reference implementation**, not the public
integration boundary.

## The shape: one client, two backends, two modes

The two things a user wants have *fundamentally different* sources, and separating them is the
whole design:

- **Read plane — history & listing.** List sessions; read a session's journal; reconstruct its
  transcript. This data lives in the **session store** and can be read with **no live agent
  running** — straight from the store. Locally the backend is `fsstore`; in the cloud it is the
  shared `natsstore` (or `pgstore`+object store). *This is "read the sessionstore directly" —
  served by harness `serve.NewReader` mounted in-process, so the BFF's read routes are the same
  code and wire contract as a remote `pkg/serve` (Decision #15).*
- **Live plane — a running session's tail.** A *currently running* session's transcript-as-it-
  happens plus gate/status. Source: looprig **session events** over `pkg/serve`'s SSE
  `GET /sessions/{sid}/events`. It needs the session's **host** to be up.
- **Control plane — driving a running session.** Submit a turn, approve/deny a gate, interrupt,
  create/restore. Source: `pkg/serve`'s POST endpoints on the **host**.

```
        ┌──────────────────── client (new module) — one Go binary ─────────────────────┐
        │  //go:embed dist  →  Svelte 5 reference SPA, built on @looprig/client          │
        │                                          SPA talks ONLY to this local BFF       │
        │                                          (same origin; no CORS; token stays    │
        │                                           server-side)                         │
        │                                                                                │
        │  READ PLANE   ── serve.NewReader over sessionstore, in-process ──┐             │
        │    local:  fsstore            (no NATS, one binary)              ├─► list       │
        │    cloud:  natsstore / pgstore+object store                     │   journal     │
        │    → Catalog().Keys/Get  ·  OpenEventReplayer → cold events     │   transcript  │
        │                                                                 ┘               │
        │  LIVE + CONTROL ── pkg/serve HTTP client, reverse-proxied ──► session HOST       │
        │    GET /sessions/{sid}/events (SSE tail) · POST input/gates/interrupt/restore    │
        │    host = local swe process  OR  remote sandbox/cloud api                        │
        │  TS CLIENT CORE ── generated DTO/zod + history/live/control transports         │
        │    framework adapters: Svelte first; React/Vue/Angular/Solid later             │
        └──────────────────────────────────────────────────────────────────────────────────┘
             depends on: looprig SDK  +  one storekit backend (chosen at composition)
             NEVER depends on: swe (no agent runner here — the client hosts nothing)
```

- **Read is direct-to-store; control is proxied.** The client is a **backend-for-frontend
  (BFF)**: it owns the store connection and the remote bearer token, and the SPA only ever
  speaks to it, same-origin.
- The client is **backend-agnostic** by Dependency Inversion (per CLAUDE.md): it depends on the
  `sessionstore` *facade*; which storekit backend is wired (`fsstore` vs `natsstore`) is a
  `main`-time decision the client is blind to. "Local without NATS" is just a different backend
  swapped in — zero client changes.
- The client **hosts no agent**, so it never imports swe. It cannot *itself* be the compute for
  a session; it can proxy create/restore/input/gates/interrupt to a host that has a compiled
  `session.Runner`, and it can browse all history with no host at all.

## Module & dependency boundary

A new standalone module — working name **`github.com/looprig/client`** (no `looprig-` prefix,
matching the storekit backend-repo convention; final name is the owner's call).

| Depends on | For |
|---|---|
| `github.com/looprig/harness` — `pkg/serve` | the mounted read-plane handler (`serve.NewReader`), the wire DTO / error-envelope types, and the protocol schema the SDK generates from |
| `github.com/looprig/harness` — `pkg/sessionstore` | the store-backed read adapter behind `serve.NewReader` (`Catalog`, `OpenEventReplayer`) |
| harness `pkg/event`/`pkg/journal` + `github.com/looprig/core` `content`/`uuid` | decode replayed records where the transcript shortcut needs them |
| harness `pkg/transcript` (optional) | HTML-export shortcut (below) |
| one storage backend — `github.com/looprig/fsstore` **or** `github.com/looprig/natsstore` | the read-plane store, chosen at composition |
| stdlib `net/http` | the BFF server + the `pkg/serve` reverse-proxy client |

**Not** depended on: `swe`, any agent implementation, any charm/TUI stack. The module is a
consumer of the looprig SDK exactly like swe, minus the agent.

Session **creation/restore** is a host job — the client proxies `POST /sessions` to a configured
host, which owns the compiled `session.Runner`. swe may *also* embed this same SPA for an all-in-one
local dev binary that both runs and shows sessions; that is a swe concern, out of scope here.

## The BFF surface (client protocol)

The Go binary serves the embedded SPA and a small same-origin JSON/SSE API. One **event DTO**
(a stable, versioned JSON projection of `pkg/event`) flows through both history and live, so every
frontend has a *single* renderer/state machine fed in two segments. This protocol mirrors
`pkg/serve`'s public wire contract, with BFF path prefixes and token custody added for browser
safety.

| Method + path | Plane | Backed by |
|---|---|---|
| `GET /api/v1/sessions?skip=&limit=` | read | mounted `serve` reader: paged `Catalog.ListSessions` → list DTO |
| `GET /api/v1/sessions/{sid}/status` | read | mounted `serve` reader: catalog status projection (state, last seq, waiting gate) |
| `GET /api/v1/sessions/{sid}/journal?from_journal_seq=&limit=` | read | mounted `serve` reader: cold Enduring journal page (seq-carrying) |
| `GET /api/v1/sessions/{sid}/transcript.html` | read | `pkg/transcript` + `html` (server-rendered shortcut; see below) |
| `GET /api/v1/sessions/{sid}/events` | live | **reverse-proxy** of host `GET /v1/sessions/{sid}/events` (SSE; `enduring` frames carry `id: <journal_seq>`) |
| `GET /api/v1/sessions/{sid}/gates` | read | open-gate snapshot: catalog `WaitingGateID` / journal fold (`GateOpened` without `GateResolved`; `GatePrepared` never appears) |
| `POST /api/v1/sessions/{sid}/input` | control | reverse-proxy of host `POST …/input` |
| `POST /api/v1/sessions/{sid}/gates/{gid}` | control | reverse-proxy of host `POST …/gates/{gid}` (opaque gate id) |
| `POST /api/v1/sessions/{sid}/interrupt` | control | reverse-proxy of host `POST …/interrupt` |
| `POST /api/v1/sessions` · `POST /api/v1/sessions/{sid}/restore` | control | reverse-proxy of host create/restore (`Idempotency-Key` forwarded) |

- **Same-origin only.** The SPA never holds the remote token or hits the host directly; the BFF
  injects `Authorization` on the proxied leg. No CORS surface.
- **Event DTO is versioned** (`{"v":1, …}`) from day one; it decodes both a replayed
  `journal` record and a live SSE `enduring`/`ephemeral` frame into one shape (message blocks,
  tool cards, gate prompts, subagent/step markers, status). The seam between "history" and "live"
  is the journal sequence, and the join is **exact**: the SDK subscribes to `…/events`
  (buffering), pages `…/journal` to tip `T`, drops buffered frames with `journal_seq <= T`, and
  follows live (details below).
- **No `DELETE` in v1** — `pkg/serve` ships no destroy endpoint; its only cancellation is
  `…/interrupt` (stops in-flight work). The UI's "stop" action maps to interrupt; a true remote
  session shutdown is future serve/host work, and store deletion stays retention/GC policy, out of
  scope here.

## Framework-neutral TypeScript client core

The reusable frontend boundary is a plain TypeScript package, working name **`@looprig/client`**.
It owns protocol parsing and session state; framework packages only adapt that state into their
reactivity model. This is the same architectural split as Vercel AI SDK UI: one stream/protocol
contract, many framework adapters.

`@looprig/client` provides:

- generated zod schemas and TypeScript types for the `serve` / BFF protocol;
- a `LooprigTransport` interface with two first-party implementations:
  - `BFFTransport` for same-origin browser apps (`/api/...`, token stays server-side);
  - `ServeTransport` for trusted/server-side/custom apps that call `pkg/serve` directly;
- cold-history loading (`listSessions`, `readSession`, `readHistory`) and live SSE attachment;
- a framework-neutral session state machine that folds history pages plus live `enduring` and
  `ephemeral` frames into messages, tool cards, gates, status, and diagnostics;
- control methods (`createSession`, `restoreSession`, `submit`, `respondGate`, `interrupt`) with
  typed errors and retry metadata from the stable error envelope;
- lossless resume via the exact sequence join (serve §7b) once serve Phase 2 stamps `id:` on live
  `enduring` frames; only `ephemeral` frames are best-effort.

Framework adapters are deliberately thin:

- **`@looprig/svelte`** wraps the core in Svelte stores/runes and powers the first-party app.
- **`@looprig/react`** later wraps the same core in hooks (`useSession`, `useSessionList`,
  `useGateResponse`, etc.).
- **`@looprig/vue`**, **`@looprig/angular`**, and **`@looprig/solid`** follow only if users need
  them; each package shares fixtures and conformance tests with the core.

The adapters must not parse raw SSE, know Looprig event internals, or implement their own history
join. They call the core.

### Why client-side rendering, not just the HTML export

The read plane can still expose a server-rendered HTML transcript via `pkg/transcript` +
`pkg/transcript/html`. That is the **fast shortcut** and a fine Phase-1a milestone. But the target
is the **event DTO folded by the SDK core and rendered by Svelte**, because the user wants *live*
streaming into *rich, interactive* chat/tool/code components — which static HTML can't do. The
state machine that folds the DTO is the client core; the HTML export stays available for a plain
read-only view and as a rendering-parity oracle in tests.

## Live tail comes from session events, not journal follow

`journal.EventReplayer` is **cold-replay only** today — `Follow:true` returns
`FollowUnsupportedError`. v1 does **not** need it: the live tail is looprig **session events**
over the host's SSE `/events`, exactly the source that already exists. So a running session
reads as *cold replay from the store up to the tip* (works even if the host is down) *plus the
SSE tail* (when a host is reachable). Implementing `EventReplayer.Follow` for a host-independent
live tail direct from the store is future work, not a v1 blocker.

**Seam integrity — resolved (2026-07-08).** Replay-to-tip-then-attach is exact: harness delivers
the journal sequence with every live Enduring event (`event.Delivery`, serve §7b — a Phase 0
harness change), and `serve` stamps live `enduring` SSE frames with `id: <journal_seq>`. The SDK
core joins losslessly — subscribe and buffer, page `…/journal` to tip `T`, drop buffered frames
with `journal_seq <= T`, follow live — with no `EventReplayer.Follow` and no server-side fusion
required. Public SDKs may promise lossless resume once serve Phase 2 (id-stamped frames) lands;
`ephemeral` frames remain best-effort and self-heal from the next authoritative `enduring` event.

## Reference frontend stack

**Svelte 5**, built as a **static client-side-rendered SPA** and embedded via `//go:embed`. This is
the first-party reference app, not the only supported frontend integration.

- **Build:** SvelteKit with `adapter-static` + `export const ssr = false` (root layout) →
  pure static assets, **no Node at serve time**. This is Tauri's documented path and embeds
  cleanly into `embed.FS`. (SvelteKit is used purely as the router/build/tooling host; none of
  its SSR/server features are in play.)
- **Components (all verified current, 2026-07-02):**
  - Dashboard/UI: **shadcn-svelte** (on Bits UI) — cards, data table, dialogs, command palette.
  - Chat/transcript: **Svelte AI Elements** (shadcn-svelte registry) — message list, streaming
    bubbles, reasoning/tool panels, autoscroll, composer. Used **presentationally**: they render
    state produced by `@looprig/client` / `@looprig/svelte`; we do **not** adopt
    `@ai-sdk/svelte`'s transport, which speaks the Vercel AI data-stream protocol, not looprig's
    event/journal model. The protocol/transport layer is ours regardless of framework. (It's a
    community port, ~180★ — but shadcn-registry installs
    vendor the components into our tree, so upstream abandonment risk is capped: we own the
    code the moment it's installed.)
  - Long transcripts: **virtua** (`Svelte >= 5`, stick-to-bottom/reverse-scroll) — virtualize
    thousands of streamed events.
  - Code blocks: **Shiki** (framework-agnostic; `codeToHtml` in the code renderer).
  - Markdown: **svelte-exmarkdown** (runtime-dynamic; fenced code routed to Shiki).
  - Charts (Phase 2+ dashboards): **LayerChart** / **uPlot** for time-series.
- **Why Svelte for the reference app (recorded):** the app's differentiators are rich prebuilt dashboard +
  chat components; Svelte's ecosystem leads there (shadcn-svelte breadth, an AI-Elements chat
  kit) while Solid would mean hand-building the chat presentation layer. The raw perf/footprint
  edge Solid holds is negligible here with virtua virtualization. Framework-agnostic pieces
  (the TypeScript client core, Shiki, virtua, uPlot, static-SPA build) are a wash. React/Vue/etc.
  adapters remain future packages over the same core.

## Deployment modes

| | Read backend | Live/control host | Auth | Binary |
|---|---|---|---|---|
| **Laptop** | `fsstore` (local dir; no NATS) | local swe on loopback | none (loopback) | one binary, `//go:embed` SPA |
| **Cloud client** | `natsstore` / `pgstore`+object store (shared) | remote sandbox/cloud `pkg/serve` | bearer + TLS | one binary; BFF holds the token |
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
  mirroring `pkg/serve`'s public-bind discipline. Fail secure: no host/credentials configured →
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
│   ├── bff/                   # BFF library: Handler(cfg, deps) http.Handler / Serve(ctx, …)
│   │   ├── read/              # mounts harness serve.NewReader + transcript shortcut (no reimplementation)
│   │   └── proxy/             # live SSE + control reverse-proxy to the host
│   └── webui/                 # //go:embed of the built SPA + hashed-asset/SPA-fallback handler
├── internal/                  # app-private helpers (config parsing, logging setup)
├── sdk/                       # npm workspace packages; no Go deps at runtime
│   ├── core/                  # @looprig/client: DTO/zod, transports, event folding, state machine
│   ├── svelte/                # @looprig/svelte: Svelte stores/runes over core
│   └── react/                 # future @looprig/react adapter placeholder / examples
├── web/                       # SvelteKit app (adapter-static, ssr=false)
│   └── src/lib/{routes,transcript,components}/  # imports @looprig/client + @looprig/svelte
├── contract/                  # serve/BFF protocol schemas + golden DTO/SSE/error fixtures
├── docs/plans/ · Makefile · CLAUDE.md · .github/workflows/
```

- **`pkg/` is exported deliberately**: the design anticipates swe embedding this SPA for an
  all-in-one binary; `bff.Handler` + `webui.FS` make that a one-liner, mirroring looprig's
  `serve.Handler` pattern. Everything without a reuse story stays `internal/`.
- **`sdk/core` is the browser/runtime seam.** The Svelte app imports the same core that future
  React/Vue/etc. adapters use; it does not own protocol parsing or history/live folding. This keeps
  framework churn out of the Go BFF and avoids duplicating stream semantics per UI package.
- **Import discipline (DIP):** only `cmd/` imports a storage backend; the read plane is harness
  `serve.NewReader` over `serve`'s narrow `Reader` interface (satisfied by the `sessionstore`
  adapter), and `pkg/bff` adds only its **own** `Host` consumer interface (live SSE +
  input/resolve/interrupt proxy) satisfied by one `net/http` host client. Interface segregation
  buys: tests run against memstore-backed adapters + an `httptest` stub host, and
  `Host == nil` ⇒ browse-only mode falls out of the type system (no control routes registered —
  fail secure, not a 403 fall-through).
- **Config** is a typed struct parsed from env at the composition root only, validated,
  fail-loud (`CLIENT_ADDR` loopback default, `CLIENT_STORE`, `CLIENT_HOST_URL`,
  `CLIENT_HOST_TOKEN` — secret, required iff host configured, never logged). No config type
  passes below `cmd/`; handlers receive narrow interfaces and pre-validated values.

### Go↔TS / protocol contract & type generation

Harness `pkg/serve`'s wire types are the **single source of truth** — plain structs, explicit JSON
tags, a `kind` discriminator, and one typed error envelope; there is no parallel client-side
`pkg/dto` (Decision #15). Generation never reads harness `pkg/event` directly: that sealed sum
type evolves with harness, and the serve wire layer is the seam that decouples frontends from it
(the event→wire mapping is hand-written Go in `pkg/serve` — one `switch` over event types — and
*is* that seam). Wire types→TypeScript is generated into the framework-neutral SDK core, pinned to
the imported harness version:

```
harness pkg/serve wire types ──go:generate──► contract/schema/v1.json ──npm run gen──► sdk/core/src/gen.ts
    (invopop/jsonschema,          (language-neutral                (json-schema-to-zod;
     reflection, dev tool)         contract artifact)               TS types via z.infer)
```

- zod is the single TS source of truth: runtime **parse** (not cast) at the SDK boundary —
  validate-at-every-boundary applies to browsers too — with static types inferred.
- The schema lives beside the **golden fixtures** in `contract/` (one JSON file per event
  shape, route response, SSE frame, and error envelope). Go tests validate fixtures against the
  schema; `sdk/core` vitest parses the same fixtures through the generated zod. Drift cannot merge
  from either direction.
- CI guard: `make contract` regenerates schema + zod; `git diff --exit-code` fails the build
  when a harness upgrade changes `pkg/serve` wire types without regeneration. Golden fixtures are
  sourced from harness `pkg/serve`'s own contract fixtures (serve §7a), so both repos test the
  same bytes.
- **Approved deps (2026-07-02):** `github.com/invopop/jsonschema` (Go, dev/tool only) and
  `json-schema-to-zod` (npm, dev only) — recorded in the client repo's own CLAUDE.md, which
  inherits looprig's rules and seeds its approved list (looprig, fsstore/natsstore; npm:
  svelte/vite/shadcn-svelte/AI Elements/virtua/shiki/svelte-exmarkdown/zod). Framework adapter
  packages add only their framework peer dependency.

### Build pipeline

- `make sdk` → `npm ci && npm run build -w sdk/core -w sdk/svelte` and contract tests.
- `make web` → `npm ci && vite build` → `pkg/webui/dist/` (**gitignored**; a committed
  one-line placeholder `index.html` keeps `go build`/`vet`/`test` green without Node).
- `make build` → depends on `sdk web`; `CGO_ENABLED=0 go build -trimpath ./cmd/...`.
- `make secure` → looprig's gauntlet (fmt-check, vet, staticcheck, gosec, govulncheck)
  **plus** SDK vitest/typecheck, `svelte-check`, and eslint over `web/`.
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
- Contract corpus: Go validates `contract/` fixtures against `schema/v1.json`; `sdk/core` vitest
  parses the same fixtures through the generated zod; CI fails on unregenerated harness
  `pkg/serve` wire changes (`make contract` + `git diff --exit-code`).
- SDK core: state-machine tests fold cold history pages plus live `enduring` and `ephemeral` frames
  into the same session view regardless of transport. Transport tests cover BFF path prefixes,
  direct `serve` paths, typed error envelopes, aborts, and the exact seam join
  (subscribe-buffer → replay-to-tip → drop `<= tip`), including an event that lands inside the
  join window.
- Framework adapter conformance: each adapter must pass the same fixture-driven behavior suite as
  `sdk/core`; framework-specific tests only cover reactivity lifecycle, cleanup, and ergonomic API
  shape.
- Reverse-proxy handlers: integration-tagged (`//go:build integration`) against a stub
  `pkg/serve` host — auth injection, path allowlisting, SSE flush/teardown, upstream-down → typed
  error.
- SPA: component tests for the transcript renderer folding a recorded DTO stream (history →
  live seam; virtualized long transcript; gate prompt round-trip).

## Migration phases (detail in the implementation plan)

- **Phase 0 (prerequisites):** the `pkg/sessionstore` read surface (`Catalog` listing +
  `OpenEventReplayer`) has **landed**; the remaining hard prerequisites are harness `pkg/serve`
  Phases 0–2 (the runner-supplied handler, read plane, wire contract, ephemeral frames, and
  `id`-stamped `enduring` frames per serve §12). The client consumes that contract only —
  implementation must not start against the legacy `pkg/api`.
- **Phase 1 — the client (v1):**
  - 1a. BFF mounts the serve read plane + transcript shortcut; `contract/` generated from harness
    serve wire types; `sdk/core` with cold-session listing/history and typed errors.
  - 1b. Svelte reference shell built on `@looprig/client` + `@looprig/svelte`; lists sessions and
    shows a cold transcript. The Svelte app must not parse raw protocol payloads directly.
  - 1c. Live plane: SSE reverse-proxy + the SDK's exact history→live seam join; Svelte renders the
    SDK session state.
  - 1d. Control plane: input / gates / interrupt / create-restore reverse-proxy; SDK control methods;
    the interactive chat composer + gate-approval UI.
  - 1e. Auth + TLS + loopback/public gating; BFF and direct-serve transports; the two deployment
    modes wired at composition.
- **Phase 2 — workspaces:** once `pkg/workspacestore` exists, add a workspaces/snapshots view
  (list `WorkspaceCheckpointed` refs from the journal; browse snapshot metadata).
- **Phase 3 — additional framework adapters:** add `@looprig/react` first if demand exists, then
  Vue/Angular/Solid as needed. Each adapter is a small wrapper over `sdk/core`, not a new transport
  implementation.
- **Phase 4 — retired (2026-07-08):** lossless resume needs no separate phase — the exact sequence
  join ships with 1c, gated only on serve Phase 2 stamping `id: <journal_seq>` on live `enduring`
  frames (serve §7b).
- **Phase 5 — desktop/mobile:** wrap the *same* static SPA in **Tauri v2** (desktop + iOS/
  Android); it points at a bundled-or-remote BFF. No SPA changes.

## Decision log (from design discussion, 2026-07-02)

1. **Pure client, looprig-only.** The module hosts no agent and never imports swe; it browses
   history from the store and drives running sessions by proxying to a host. Session
   creation/restore is a host job the client proxies.
2. **Read plane / live plane / control plane split.** History & listing come from
   `sessionstore` directly (any backend); a running session's tail comes from `pkg/serve` SSE
   session events; control comes from `pkg/serve` POST — three distinct sources, one client core.
3. **Session listing is a `sessionstore` concern**, not `pkg/serve`'s live-session table nor
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
8. **Framework-neutral client core, Svelte reference app.** Inspired by Vercel AI SDK's split
   between a stable stream/message protocol and thin framework adapters, Looprig's public frontend
   boundary is `@looprig/client` (`sdk/core`): generated DTO/zod, transports, event folding, typed
   errors, and the session state machine. Svelte is still the first-party reference UI because
   shadcn-svelte + Svelte AI Elements give the fastest high-quality dashboard/chat surface, but the
   Svelte app consumes the same core that future React/Vue/Angular/Solid adapters will consume.
   `@ai-sdk/svelte`'s *transport* is **not** adopted — looprig's event stream is its own protocol —
   only AI Elements' presentational components.
9. **Workspaces deferred to Phase 2**, gated on `pkg/workspacestore` (design-only today).
10. **Additional framework adapters are future work.** `@looprig/svelte` ships first with the
    reference app; `@looprig/react` is the likely next adapter if users ask for it. Each adapter must
    wrap `sdk/core` and pass the shared fixture/conformance suite.
11. **Desktop/mobile deferred to Phase 5** via Tauri v2 wrapping the same static SPA.
12. **Review fixes (2026-07-02):** gate-snapshot convenience view added (`GET …/gates`, derived
    from `ReadSession`/history so it does not require a `serve` open-gate registry); history→live
    **seam integrity** named a verification item (SSE must eventually be seq-resumable or
    seq-carrying before public SDKs promise lossless resume; a small `pkg/serve` / harness-storage
    addition is in scope); **v1 scoped to a single configured host** — session→host
    routing for sandbox fleets is explicit future work; per-mode thin `main`s/build tags noted
    so the laptop binary links no NATS (the storekit extraction — in implementation — removes
    NATS from looprig core, so Phase 0 is a hard prerequisite with **no legacy
    `journal.Catalog`/`persistence` fallback**); proxied `DELETE` clarified as
    stop-live-only, never store deletion.
13. **Repo architecture (2026-07-02, revised for SDK core):** standalone module with exported
    `pkg/{dto,bff,webui}` (so swe can embed the BFF + SPA), `sdk/core` for `@looprig/client`,
    `sdk/svelte` for the first adapter, `cmd/` as the only backend-importing layer, BFF-owned
    narrow consumer interfaces (`Catalog`/`Replayer`/`Host`; `Host == nil` ⇒ browse-only by
    construction), gitignored SPA build with committed placeholder for Node-free `go test`.
14. **Type generation (2026-07-02, revised for protocol contract):** JSON Schema pipeline chosen
    over direct Go→TS (tygo) and no-codegen. Source of truth is `pkg/dto` plus `pkg/serve` wire
    types — never looprig `pkg/event` directly. Approved: `github.com/invopop/jsonschema` (Go dev
    tool), `json-schema-to-zod` (npm dev). zod is the TS source of truth (runtime parse at the SDK
    boundary; types via `z.infer`); golden fixtures + schema validation + CI regen-diff make drift
    unmergeable. *(Superseded in part by #15: the source of truth is harness `pkg/serve` wire
    types alone — client `pkg/dto` no longer exists.)*
15. **Reconciliation to the 2026-07-06 serve contract (2026-07-08).** Paths/params/gate ids
    aligned (`/api/v1/…`, `from_journal_seq`, opaque `{gid}`; the cold read is `…/journal`, the
    live SSE is `…/events`); `GET /api/sessions/{id}` replaced by the serve status projection;
    `DELETE` dropped (serve ships no v1 destroy — UI "stop" = interrupt); the BFF **mounts
    `serve.NewReader` in-process** instead of reimplementing the read plane (one wire contract,
    one Go implementation; browse-only mode preserved because the reader runs over the locally
    wired store); client `pkg/dto` deleted — wire-DTO ownership lives in harness `pkg/serve`, and
    `contract/` generates from those types pinned per harness version; the gates snapshot derives
    from the catalog `WaitingGateID`/journal fold, not a `ReadSession` API; seam integrity is
    resolved by harness sequenced delivery (serve §7b), so the SDK's history→live join is exact
    and lossless resume ships with serve Phase 2 (client Phase 4 retired); module names corrected
    (`github.com/looprig/fsstore`/`natsstore`, harness/core packages).
