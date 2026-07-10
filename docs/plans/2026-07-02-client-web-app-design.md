# Design: client — framework-neutral session client + reference web app

**Date:** 2026-07-02
**Status:** Approved (design discussion in session; this doc records the outcome)
Revised 2026-07-08: reconciled against `2026-07-06-serve-http-session-api-design.md` — paths,
params, and gate ids aligned; `DELETE` dropped (serve ships no v1 destroy); the BFF read plane now
**mounts `serve`'s reader** instead of reimplementing it; wire-DTO ownership moved to harness
`pkg/serve`; lossless resume moved up (sequenced live delivery is a harness Phase 0 capability,
serve §7b). See Decision #15.
Revised 2026-07-09: reconciled against the **as-built** `pkg/serve` and the 2026-07-09 harness change
that archived `pkg/transcript` out of harness. Five corrections: (1) the read plane mounts a new
`serve.ReadHandler(reads)` — a harness Phase 0 prerequisite carving the stateless read routes out of
the runner-coupled `serve.Handler` (the BFF hosts no agent, so it has no runner to pass); (2) the
`transcript.html` shortcut and its render-parity oracle are dropped (`pkg/transcript` left harness for
`looprig/cli`, which the client must not depend on); (3) the wire schema is **generated from
`pkg/serve`'s Go structs** (`invopop/jsonschema` `go:generate`), not hand-maintained testdata; (4) the
SDK consumes `GET /v1/capabilities` for feature negotiation; (5) the `/gates` snapshot is explicitly
BFF-synthesized, not a `serve` route. See Decision #16.
Revised 2026-07-09 (independent review, Decision #17): read is now a **composition-time seam** —
mounted `serve.ReadHandler` in-proc (laptop, links a store) OR reverse-proxied to a remote `serve`
read plane (cloud/thin, links **no** storage backend); TS runtime validation uses **ajv over serve's
shipped JSON Schema** (no `json-schema-to-zod`, and no ask for serve to adopt `invopop` — serve
hand-authored its schema to stay stdlib-only, by design); a DNS-rebind/`Host`/`Origin` guard + CSRF
move to Phase 1a; the `sdk/react` placeholder is dropped and `pkg/bff` kept internal (export
`pkg/webui` only); `id:`-stamped resume is **already shipped**, so lossless resume lands in 1c;
`web/` → `app/`.
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
  running**. The read routes are a **composition-time seam** (Decision #17): either harness
  `serve.ReadHandler` **mounted in-process** over a locally-wired store (laptop: `fsstore`, no
  NATS), or a **reverse-proxy to a remote `serve`'s stateless read routes** (cloud/thin client:
  links **no** storage backend at all). Both speak the identical `serve` wire contract, so the BFF
  and SDK are blind to which is wired.
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
        │  READ PLANE   ── composition-time seam (Decision #17) ───────────┐             │
        │    laptop:  serve.ReadHandler mounted in-proc over fsstore        ├─► list       │
        │             → Catalog().Keys/Get · OpenEventReplayer (links store)│   journal     │
        │    cloud:   reverse-proxy to remote serve read routes (no store)  ┘   transcript  │
        │  LIVE + CONTROL ── pkg/serve HTTP client, reverse-proxied ──► session HOST       │
        │    GET /sessions/{sid}/events (SSE tail) · POST input/gates/interrupt/restore    │
        │    host = local swe process  OR  remote sandbox/cloud api                        │
        │  TS CLIENT CORE ── DTO types + ajv(schema) + history/live/control transports    │
        │    framework adapters: Svelte first; React/Vue/Angular/Solid later             │
        └──────────────────────────────────────────────────────────────────────────────────┘
             depends on: looprig SDK  (+ one storekit backend ONLY when read is mounted in-proc)
             NEVER depends on: swe (no agent runner here — the client hosts nothing)
```

- **Read is mounted-or-proxied; control is always proxied.** The client is a **backend-for-frontend
  (BFF)**: it owns the store connection (only when read is mounted in-proc) and the remote bearer
  token, and the SPA only ever speaks to it, same-origin.
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
| `github.com/looprig/harness` — `pkg/serve` | the mounted read-plane handler (`serve.ReadHandler`), the wire DTO / error-envelope types, and the protocol schema the SDK generates from |
| `github.com/looprig/harness` — `pkg/serve/catalogreader` + `pkg/sessionstore` | **mounted-read mode only** — the store-backed read adapter behind `serve.ReadHandler` (`catalogreader.New` over `Catalog`, `OpenEventReplayer`) |
| harness `pkg/event`/`pkg/journal` + `github.com/looprig/core` `content`/`uuid` | **mounted-read mode only** — decode replayed records inside the mounted reader |
| one storage backend — `github.com/looprig/fsstore` **or** `github.com/looprig/natsstore` | **mounted-read mode only** (laptop); the cloud/thin client proxies read and links **no** backend (Decision #17) |
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
| `GET /api/v1/capabilities` | protocol | **BFF-synthesized** (not proxied verbatim): the BFF's *own* feature set — reflects whether a control host is wired (browse-only advertises `journal` only; no `live_sse`/`gate_response`) |
| `GET /api/v1/sessions?skip=&limit=` | read | mounted `serve.ReadHandler`: paged `Catalog.ListSessions` → list DTO |
| `GET /api/v1/sessions/{sid}/status` | read | mounted `serve.ReadHandler`: catalog status projection (state, last seq, waiting gate) |
| `GET /api/v1/sessions/{sid}/journal?from_journal_seq=&limit=` | read | mounted `serve.ReadHandler`: cold Enduring journal page (seq-carrying) |
| `GET /api/v1/sessions/{sid}/events` | live | **reverse-proxy** of host `GET /v1/sessions/{sid}/events` (SSE; `enduring` frames carry `id: <journal_seq>`) |
| `GET /api/v1/sessions/{sid}/gates` | read | **BFF-synthesized** (not a `serve` route): the single open gate from status `WaitingGateID`. Serve's status projection carries **one** `WaitingGateID`, so v1 surfaces at most one open gate; multiple concurrent gates (parallel tools/subagents) would need an O(journal) fold or a serve open-gates list — out of scope (Decision #17) |
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

- TypeScript types + a runtime validator (**ajv over serve's shipped JSON Schema**, Decision #17) for the `serve` / BFF protocol;
- a `LooprigTransport` interface with two first-party implementations:
  - `BFFTransport` for same-origin browser apps (`/api/...`, token stays server-side);
  - `ServeTransport` for trusted/server-side/custom apps that call `pkg/serve` directly;
- cold-history loading (`listSessions`, `readSession`, `readHistory`) and live SSE attachment;
- a framework-neutral session state machine that folds history pages plus live `enduring` and
  `ephemeral` frames into messages, tool cards, gates, status, and diagnostics;
- control methods (`createSession`, `restoreSession`, `submit`, `respondGate`, `interrupt`) with
  typed errors and retry metadata from the stable error envelope;
- lossless resume via the exact sequence join (serve §7b) — serve **already** stamps `id:
  <journal_seq>` on live `enduring` frames, so this ships in Phase 1c; only `ephemeral` frames are
  best-effort.

Framework adapters are deliberately thin:

- **`@looprig/svelte`** wraps the core in Svelte stores/runes and powers the first-party app.
- **`@looprig/react`** later wraps the same core in hooks (`useSession`, `useSessionList`,
  `useGateResponse`, etc.).
- **`@looprig/vue`**, **`@looprig/angular`**, and **`@looprig/solid`** follow only if users need
  them; each package shares fixtures and conformance tests with the core.

The adapters must not parse raw SSE, know Looprig event internals, or implement their own history
join. They call the core.

### Why client-side rendering (no HTML-export shortcut)

Rendering is the **event DTO folded by the SDK core and rendered by Svelte**: the user wants *live*
streaming into *rich, interactive* chat/tool/code components, which static server-rendered HTML
can't do. An earlier revision kept a server-rendered `pkg/transcript/html` transcript as a Phase-1a
shortcut and a render-parity test oracle, but `pkg/transcript` was archived out of harness
(2026-07-09, headed to `looprig/cli`) and the client must not depend on `looprig/cli` (it drags the
charm/TUI stack, forbidden above). So there is no HTML shortcut: Phase 1a renders a cold transcript
by folding the journal DTO through the SDK core, and the contract fixtures (not an HTML oracle)
guard that folding.

## Live tail comes from session events, not journal follow

`journal.EventReplayer` is **cold-replay only** today — `Follow:true` returns
`FollowUnsupportedError`. v1 does **not** need it: the live tail is looprig **session events**
over the host's SSE `/events`, exactly the source that already exists. So a running session
reads as *cold replay from the store up to the tip* (works even if the host is down) *plus the
SSE tail* (when a host is reachable). Implementing `EventReplayer.Follow` for a host-independent
live tail direct from the store is future work, not a v1 blocker.

**Seam integrity — shipped (verified 2026-07-09).** Replay-to-tip-then-attach is exact: harness
delivers the journal sequence with every live Enduring event (`event.Delivery`, serve §7b), and
`serve` **already** stamps live `enduring` SSE frames with `id: <journal_seq>` (verified in the
as-built `pkg/serve/handlers_events.go`). The SDK core joins losslessly — subscribe and buffer,
page `…/journal` to tip `T`, drop buffered frames with `journal_seq <= T`, follow live — with no
`EventReplayer.Follow` and no server-side fusion required. Public SDKs can promise lossless resume
**now** (no pending serve phase); `ephemeral` frames remain best-effort and self-heal from the next
authoritative `enduring` event.

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

| | Read plane | Live/control host | Auth | Binary |
|---|---|---|---|---|
| **Laptop** | mounted `serve.ReadHandler` over `fsstore` (no NATS) | local swe on loopback | none (loopback) | one binary, `//go:embed` SPA, links `fsstore` |
| **Cloud client** | **proxied** to remote `serve` read routes (no store linked) | remote sandbox/cloud `pkg/serve` | bearer + TLS | one binary; BFF holds the token; **no storage backend** |
| **Browse-only** | mounted over a store, or proxied to a runner-less `serve.ReadHandler` | — (no host) | read-token only | history + transcripts, no control |

The client code is identical across all three; only composition-root wiring (backend choice,
host URL, credentials) differs.

- **v1 is single-host.** The BFF proxies live/control to *one* configured host URL. A fleet of
  scale-to-zero sandboxes (one host per session) needs a session→host routing map, which
  nothing owns yet — recorded here as future work, not silently assumed.
- **Binary composition is honest about deps.** The read seam (Decision #17) means the cloud/thin
  binary **proxies** read and links **no** storage backend at all — no NATS "riding along unused,"
  which the earlier direct-to-store design couldn't avoid. The laptop binary mounts read in-proc and
  links `fsstore` only (no NATS). A dual-mode convenience binary links whatever backend its mounted
  mode needs; single-purpose `main`s stay minimal.

## Auth & security (per CLAUDE.md)

- **Loopback default.** The BFF binds `127.0.0.1` by default; a public bind is opt-in and gated,
  mirroring `pkg/serve`'s public-bind discipline. Fail secure: no host/credentials configured →
  read-only, never a fall-through to control.
- **Host/Origin validation + anti-DNS-rebinding (Phase 1a).** Loopback binding alone does **not**
  stop DNS rebinding — a malicious page can rebind a name to `127.0.0.1` and drive the BFF (no CORS
  preflight fires for simple/same-origin-shaped requests). So the BFF **validates `Host` and
  `Origin` on every request** — reject any `Host` not in `{127.0.0.1, localhost, [::1]}` (or the
  configured public host) and any cross-origin `Origin`, before auth — and control POSTs require a
  per-page-load **CSRF token** the BFF mints into the SPA and checks on submit. This lands with the
  bind (Phase 1a), not last.
- **Token stays server-side.** The remote host's bearer token lives only in the BFF process
  (env var / secrets manager — never in code, never shipped to the browser, never logged). The
  BFF injects it on the proxied leg; the SPA is same-origin and unauthenticated to the host. The
  reverse proxy **strips any inbound `Authorization` from the SPA** and injects only the
  server-side token, so a compromised SPA cannot smuggle credentials upstream.
- **SSE proxy preserves the resume seam.** The live reverse-proxy **forwards `Last-Event-ID`** on
  reconnect (and passes the downstream `id:` stamps through) so the client-side lossless join works
  *through* the BFF exactly as direct-to-serve; it uses a flush loop with an idle deadline, never an
  unbounded copy.
- **TLS** to any remote host: `MinVersion: tls.VersionTLS12`, never `InsecureSkipVerify`.
- **Explicit `http.Server` timeouts** (Read/Write/Idle, `MaxHeaderBytes`) on the BFF; every
  proxied/store call is `context`-bounded.
- **Validate at the boundary.** Session IDs are parsed as `uuid.UUID`; `from_journal_seq` and
  `limit` are bounded integers (serve exposes no `to` bound); the reverse proxy forwards only an
  allowlisted set of paths/methods (never an arbitrary upstream path). Any served file path is
  `filepath.Clean`'d and confined to the embedded FS.
- **Typed errors** for every distinct BFF failure (`UpstreamUnavailableError`,
  `SessionNotFoundError`, `StoreReadError`, …); `errors.As` at call sites; audit auth failures and
  denied gates by **route + sid + decision**, never the body (gate values / input blocks are
  PII-ish), matching serve's `writeErrorCause` discipline.

## Repo architecture

```
github.com/looprig/client
├── cmd/
│   ├── looprig-client/        # dual-mode main (links both backends; convenience)
│   └── looprig-client-local/  # fsstore-only main — the no-NATS laptop binary
├── pkg/
│   └── webui/                 # EXPORTED //go:embed of the built SPA + SPA-fallback handler (swe reuse)
├── internal/                  # app-private (config, logging) — and the BFF itself:
│   └── bff/                   # read (ReadSource: mounted serve.ReadHandler OR proxy) + live/control proxy
├── sdk/                       # npm workspace packages; no Go deps at runtime
│   ├── core/                  # @looprig/client: DTO types, ajv(schema), transports, folding, state machine
│   └── svelte/                # @looprig/svelte: Svelte stores/runes over core
├── app/                       # SvelteKit reference app (adapter-static, ssr=false)
│   └── src/lib/{routes,transcript,components}/  # imports @looprig/client + @looprig/svelte
├── contract/                  # serve JSON Schema (copied per harness version) + golden DTO/SSE/error fixtures
├── docs/plans/ · Makefile · CLAUDE.md · .github/workflows/
```

- **`pkg/webui` is the only exported package** (Decision #17): swe's all-in-one binary **is** the
  host (it has a runner), so it mounts full `serve.Handler` directly and reuses only the **SPA
  embed** — not the proxying BFF. `webui.FS` is the reuse surface; the BFF (token custody + proxy)
  stays `internal/bff` until a real second consumer needs it, then it's promoted.
- **`sdk/core` is the browser/runtime seam.** The Svelte app imports the same core that future
  React/Vue/etc. adapters use; it does not own protocol parsing or history/live folding. This keeps
  framework churn out of the Go BFF and avoids duplicating stream semantics per UI package.
- **Import discipline (DIP):** only `cmd/` imports a storage backend, and **only** when read is
  mounted in-proc; the read plane is a `ReadSource` (an `http.Handler`) chosen at composition —
  either harness `serve.ReadHandler` over the `serve/catalogreader` adapter (mounted) or a reverse
  proxy to a remote serve (proxied). `internal/bff` adds only its **own** `Host` consumer interface
  (live SSE + input/resolve/interrupt proxy) satisfied by one `net/http` host client. Interface
  segregation buys: tests run against memstore-backed adapters + an `httptest` stub host, and
  `Host == nil` ⇒ browse-only mode falls out of the type system (no control routes registered —
  fail secure, not a 403 fall-through).
- **Config** is a typed struct parsed from env at the composition root only, validated,
  fail-loud (`CLIENT_ADDR` loopback default, `CLIENT_STORE`, `CLIENT_HOST_URL`,
  `CLIENT_HOST_TOKEN` — secret, required iff host configured, never logged). No config type
  passes below `cmd/`; handlers receive narrow interfaces and pre-validated values.

### Go↔TS / protocol contract & type generation

Harness `pkg/serve` is the **single source of truth** for the wire contract, and it already ships
that contract as an artifact: a **hand-authored JSON Schema + OpenAPI** under `pkg/serve/testdata/`
plus **golden JSON/SSE fixtures**, cross-checked in serve's own tests (`schema_test.go`). serve
authored the schema by hand *deliberately* to stay stdlib-only (CLAUDE.md); the client does **not**
ask serve to adopt a JSON-Schema generator (Decision #17) — it **consumes serve's shipped schema +
fixtures**, pinned per harness version:

```
serve testdata/schema/*   ──copy per harness version──► contract/schema/
serve testdata/fixtures/* ──copy──────────────────────► contract/fixtures/   (shared golden bytes, both repos)
        │                                          │
        ├─► TS types:  json-schema-to-ts (type-level FromSchema<…>, no generated file)
        └─► TS runtime validation:  ajv compiles the schema; the SDK boundary calls validate()
```

- **ajv over the shipped schema — the schema *is* the validator.** Runtime **parse** (not cast) at
  the SDK boundary — validate-at-every-boundary applies to browsers too — using `ajv` against
  serve's exact JSON Schema. No `json-schema-to-zod` transpile hop (its `oneOf`/`$ref`/int64-as-
  string handling is uneven and would need hand-patching); no second hand-written schema. Static
  types come from `json-schema-to-ts` (`FromSchema<typeof schema>`) — type-level, nothing to rot.
- **Drift is caught by shared golden fixtures, not a regen guard.** The client copies serve's tagged
  `testdata/{schema,fixtures}`. Go (in serve) already validates the fixtures against the schema; TS
  vitest parses the **same** fixture bytes through ajv. A harness wire change breaks a golden fixture
  → **both** repos fail. This is more robust than a `git diff --exit-code` on generated output, and
  needs no generator in either repo.
- **Versioning:** schema + fixtures are pinned to the imported harness version; bumping harness
  re-copies them and any wire change surfaces as a fixture diff reviewed as a contract change.
- **Approved deps (client repo):** `ajv` and `json-schema-to-ts` (npm) — recorded in the client
  repo's own CLAUDE.md, which inherits looprig's rules and seeds its approved list (looprig,
  fsstore/natsstore; npm: svelte/vite/shadcn-svelte/AI Elements/virtua/shiki/svelte-exmarkdown). No
  `invopop/jsonschema` (serve owns the schema) and no `json-schema-to-zod`. Framework adapter
  packages add only their framework peer dependency.

### Build pipeline

- `make contract` → copy serve's tagged `testdata/{schema,fixtures}` into `contract/`; run the
  fixture-parse conformance (TS ajv + Go). No schema-generation step.
- `make sdk` → `npm ci && npm run build -w sdk/core -w sdk/svelte` and contract tests.
- `make app` → `npm ci && vite build` → `pkg/webui/dist/` (**gitignored**; a committed
  one-line placeholder `index.html` keeps `go build`/`vet`/`test` green without Node).
- `make build` → depends on `sdk app`; `CGO_ENABLED=0 go build -trimpath ./cmd/...`.
- `make secure` → looprig's gauntlet (fmt-check, vet, staticcheck, gosec, govulncheck)
  **plus** SDK vitest/typecheck, `svelte-check`, and eslint over `app/`.
- **Dev loop:** `vite dev` proxying `/api → 127.0.0.1:<bff>`; same-origin in prod, proxied in
  dev — CORS never exists anywhere. Local multi-repo dev via an uncommitted `go.work`.
- Release tooling (goreleaser vs plain make) is an **open question** — a new tool dep
  requiring approval when it comes up.

## Testing (per CLAUDE.md)

- Table-driven, `-race` always. The BFF's read handlers test against `storekit/memstore` behind
  `sessionstore` (fast, deterministic, no NATS).
- Event-DTO codec: a fuzz target (external input → decode) and round-trip tests that the DTO
  from cold replay and from a live SSE frame are byte-identical for the same event. (The former
  `pkg/transcript/html` render-parity oracle is dropped — that package left harness; the contract
  fixtures guard the DTO folding instead.)
- Contract corpus: `sdk/core` vitest parses serve's golden fixtures through **ajv over the shipped
  schema**; serve's own Go tests already validate those fixtures against the schema. A harness wire
  change breaks a shared golden fixture → both repos fail (no `git diff --exit-code` regen guard, no
  generated schema to drift).
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

- **Phase 0 (prerequisites):** the `pkg/sessionstore` read surface has **landed**, and harness
  `pkg/serve` (runner-supplied handler, read plane, wire contract, ephemeral frames, **already**
  `id`-stamped `enduring` frames, shipped hand-authored schema + golden fixtures) is **built**. The
  one remaining serve ask is a **`serve.ReadHandler(reads Reader, opts...) http.Handler`** =
  `newServer[LiveSession](nil, reads, cfg)` registering only `list/status/journal` (plus a reduced
  `/capabilities` advertising `journal` only) — the existing `serve.Handler[S]` requires a runner
  the BFF does not have. No schema-generation ask (serve owns the hand-authored schema by design).
  The client consumes that contract only.
- **Phase 1 — the client (v1):**
  - 1a. BFF binds loopback with the **`Host`/`Origin` + DNS-rebind guard and CSRF from the start**;
    read plane wired as a `ReadSource` (mounted `serve.ReadHandler` or proxy); `contract/` copied
    from serve's shipped schema + fixtures; `sdk/core` with cold-session listing/history, ajv
    validation, and typed errors. (No transcript-HTML shortcut — cold transcript renders via SDK DTO
    folding.)
  - 1b. Svelte reference shell built on `@looprig/client` + `@looprig/svelte`; lists sessions and
    shows a cold transcript. The Svelte app must not parse raw protocol payloads directly.
  - 1c. Live plane: SSE reverse-proxy (**forwarding `Last-Event-ID`**) + the SDK's exact history→live
    seam join over the already-shipped `id:` stamps — **lossless resume ships here**; Svelte renders
    the SDK session state.
  - 1d. Control plane: input / gates / interrupt / create-restore reverse-proxy with **token custody
    (inject server-side token, strip inbound `Authorization`) and fail-secure `Host == nil`
    browse-only**; TLS to any remote host; SDK control methods; the interactive chat composer +
    gate-approval UI. The two deployment modes wired at composition.
- **Phase 2 — workspaces:** once `pkg/workspacestore` exists, add a workspaces/snapshots view
  (list `WorkspaceCheckpointed` refs from the journal; browse snapshot metadata).
- **Phase 3 — additional framework adapters:** add `@looprig/react` first if demand exists, then
  Vue/Angular/Solid as needed. Each adapter is a small wrapper over `sdk/core`, not a new transport
  implementation.
- **Phase 4 — retired:** lossless resume needs no separate phase and no pending serve work — the
  exact sequence join ships with 1c against the **already-shipped** `id:`-stamped `enduring` frames.
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
7. **Client-side rich rendering via a versioned event DTO.** (The former `pkg/transcript` HTML-export
   shortcut and its rendering-parity oracle are dropped — that package left harness on 2026-07-09;
   see Decision #16.)
8. **Framework-neutral client core, Svelte reference app.** Inspired by Vercel AI SDK's split
   between a stable stream/message protocol and thin framework adapters, Looprig's public frontend
   boundary is `@looprig/client` (`sdk/core`): DTO types + ajv validation (Decision #17), transports,
   event folding, typed errors, and the session state machine. Svelte is still the first-party reference UI because
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
    types alone — client `pkg/dto` no longer exists. Superseded further by #17: no generator at all —
    the client consumes serve's shipped hand-authored schema + fixtures and validates with `ajv`;
    `invopop/jsonschema` and `json-schema-to-zod` are dropped.)*
15. **Reconciliation to the 2026-07-06 serve contract (2026-07-08).** Paths/params/gate ids
    aligned (`/api/v1/…`, `from_journal_seq`, opaque `{gid}`; the cold read is `…/journal`, the
    live SSE is `…/events`); `GET /api/sessions/{id}` replaced by the serve status projection;
    `DELETE` dropped (serve ships no v1 destroy — UI "stop" = interrupt); the BFF **mounts
    `serve.ReadHandler` in-process** instead of reimplementing the read plane (one wire contract,
    one Go implementation; browse-only mode preserved because the reader runs over the locally
    wired store); client `pkg/dto` deleted — wire-DTO ownership lives in harness `pkg/serve`, and
    `contract/` generates from those types pinned per harness version; the gates snapshot derives
    from the catalog `WaitingGateID`/journal fold, not a `ReadSession` API; seam integrity is
    resolved by harness sequenced delivery (serve §7b), so the SDK's history→live join is exact
    and lossless resume ships with serve Phase 2 (client Phase 4 retired); module names corrected
    (`github.com/looprig/fsstore`/`natsstore`, harness/core packages).
16. **Reconciliation to the as-built `pkg/serve` + transcript archival (2026-07-09).** Five
    corrections after `pkg/serve` shipped and `pkg/transcript` was archived out of harness:
    (1) **Read plane needs `serve.ReadHandler`.** As built, `serve.Handler[S](runner Runner[S], reads,
    …)` welds the read routes onto a mux that *requires* a runner; there is no `serve.NewReader` and
    no runner-free read handler. The BFF hosts no agent, so it has no runner — it therefore needs a
    new harness `serve.ReadHandler(reads Reader, opts...) http.Handler` mounting only the stateless
    read group (list/status/journal). This preserves "one wire contract, one Go implementation" and
    is honest to serve's own "separate read route group" framing. The concrete read adapter is
    `serve/catalogreader.New(catalog, store)` behind serve's narrow `Reader` interface.
    (2) **`transcript.html` shortcut dropped.** `pkg/transcript`/`.../html`/`journalsource` left
    harness (headed to `looprig/cli`, whose charm/TUI stack the client forbids). The Phase-1a HTML
    milestone and the render-parity test oracle are removed; cold transcripts render through the
    SDK's DTO folding, guarded by the contract fixtures.
    (3) **Schema as code — SUPERSEDED by Decision #17.** (This point had recommended serve
    `go:generate` its schema with `invopop`. That contradicts serve's deliberate stdlib-only,
    hand-authored schema; #17 instead consumes serve's shipped schema + fixtures and validates with
    `ajv` — no generator, no `invopop`, no `json-schema-to-zod`.)
    (4) **Capabilities negotiation.** The BFF proxies/caches `GET /v1/capabilities`; the SDK reads it
    to fail fast when the server lacks a feature it requires.
    (5) **`/gates` is BFF-synthesized.** Serve ships no `/gates` route; the BFF derives the open-gate
    snapshot from the mounted reader's status `WaitingGateID` + a journal fold — BFF-owned logic on
    top of `/status` + `/journal`, not a mounted serve route.
17. **Independent review reconciliation (2026-07-09).** A model-driven review against the as-built
    `pkg/serve` produced six accepted changes:
    (1) **Read is a composition-time `ReadSource` seam, not hardcoded direct-to-store.** The read
    plane is an `http.Handler` chosen at `cmd/`: mounted `serve.ReadHandler` over `catalogreader`
    (laptop — links a store) **or** a reverse-proxy to a remote serve's stateless read routes
    (cloud/thin — links **no** storage backend). This structurally deletes the "unused NATS rides
    along" wart the direct-to-store design couldn't avoid, and makes the cloud client a genuinely
    thin proxy + SPA host. Browse-only points at a store-backed mounted reader or a runner-less
    remote `serve.ReadHandler`.
    (2) **Runtime validation is `ajv` over serve's shipped JSON Schema; drop `json-schema-to-zod`
    and the "make serve `go:generate`" ask.** serve hand-authored its schema *deliberately* to stay
    stdlib-only (`schema_test.go` says so) — a consumer must not push it to reverse that. The client
    consumes serve's shipped `testdata/{schema,fixtures}`, validates with `ajv`, and types via
    `json-schema-to-ts`. Drift is caught by both repos parsing the same golden fixtures — more robust
    than a regen `git diff` guard. Supersedes #16(3) and the invopop/zod parts of #14.
    (3) **Security moves forward.** `Host`/`Origin` validation + DNS-rebind defense + CSRF land in
    Phase 1a (loopback binding alone does not stop DNS rebinding of a token-holding BFF); token
    custody + fail-secure browse-only land in 1d; the trailing "1e" is deleted. The SSE proxy
    forwards `Last-Event-ID` (or resume breaks through the BFF) and strips inbound `Authorization`
    from the SPA. Audit logs record route + sid + decision, never the body.
    (4) **De-scope the surface.** Export `pkg/webui` only; keep the BFF `internal/bff` (swe reuses
    the SPA embed, not the proxy — swe is itself the host). Keep the `sdk/core` ↔ `sdk/svelte` split
    (it dogfoods the adapter boundary) but drop the empty `sdk/react` placeholder until React is
    real. `web/` → `app/` (avoid the marketing-site naming collision).
    (5) **Stale as-built fixes.** `/capabilities` is BFF-**synthesized** (reflects whether a host is
    wired), not proxied verbatim; `/gates` honestly surfaces the **single** `WaitingGateID` (multiple
    concurrent gates are out of scope); drop the non-existent `to` journal param; drop all
    "once serve Phase 2 stamps `id:`" conditional framing — `id:` stamping and the client join are
    **already shipped**, so lossless resume lands in 1c.
    (6) **ReadHandler ask stays minimal:** `newServer[LiveSession](nil, reads, cfg)` registering only
    the three read routes (+ reduced capabilities) — reuses the shipped unexported handlers verbatim,
    zero new logic, trivially reviewable as a harness PR.
