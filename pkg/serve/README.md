# pkg/serve

`pkg/serve` hosts the **HTTP surface** over a live session. It is the
composition seam between the outside world (HTTP clients) and the
in-process session machinery, and it obeys strict Dependency Inversion:
the production package couples **only** to the narrow interfaces
declared here (`LiveSession`, `Rig`) plus the leaf value types those
interfaces mention (`pkg/event`, `pkg/gate`, `core/content`,
`core/uuid`) and the standard library. It **never imports** `pkg/session`,
any LLM package, or any store package — those concrete types are wired
in at the composition root and reach `serve` exclusively through
`LiveSession` and `Rig`.

## What is serve?

- **`LiveSession`** — the narrow, HTTP-facing view of a running session:
  `SessionID`, `Submit`, `SubscribeEvents`, `RespondGate`,
  `Interrupt`. `session.SessionController` satisfies it structurally at
  the composition root; `serve` never imports or names that contract in
  production.
- **`Rig[S, O]`** — the narrow session-factory view: `NewSession` and
  `RestoreSession`. Generic over the concrete live-session type `S`
  (constrained to `LiveSession`) so a caller keeps the real type
  through `NewSession`/`RestoreSession` without `serve` importing it.
- **`Handler`** — builds the complete session HTTP surface: assembles
  the config from `Options`, installs the middleware (auth, body cap,
  request id, recovery, idempotency), wraps a `ServeMux`, and returns
  an `http.Handler`. The route table is fixed and disjoint (see below).
- **`Server`** — builds a hardened `*http.Server` bound to an address,
  refusing — **fail secure** — to bind a public address without
  authentication installed (`PublicBindWithoutAuthError`). It does
  **not** `Listen` or `Serve`; the caller runs the returned server so
  the listen lifecycle stays the caller's.
- **`Options`** — `WithAuthenticator(a)`, `WithBodyCap(n)`,
  `WithVisibility(v)`, etc. The authenticator is required for a public
  bind; `WithInsecurePublicBind` is the explicit opt-in for deployments
  that terminate authentication in front (a mesh sidecar, an
  authenticating proxy).

## How to use

```go
import (
    "github.com/looprig/harness/pkg/serve"
    "github.com/looprig/harness/pkg/rig"
    "github.com/looprig/harness/pkg/session"
)

r, _ := rig.Define(/* ... */)

handler, err := serve.Handler(
    serve.WithRig(serve.Rig[session.SessionController, rig.SessionOption](r)),
    serve.WithAuthenticator(myAuth),
    serve.WithBodyCap(1 << 20),  // 1 MiB request body cap
    serve.WithVisibility(serve.VisibilitySessionScoped),
)
if err != nil { return err }

srv, err := serve.Server(":8080", handler)
if err != nil { return err }  // PublicBindWithoutAuthError if public + no auth
_ = srv.ListenAndServe()
```

A client drives a session over HTTP:

- `POST /v1/sessions` — bring up a new session; returns the session id.
- `POST /v1/sessions/{sid}/restore` — restore a prior session by id.
- `POST /v1/sessions/{sid}/input` — submit a user turn (fire-and-forget;
  the outcome arrives on the event stream, correlated by the returned
  input id).
- `POST /v1/sessions/{sid}/interrupt` — interrupt every in-flight turn.
- `POST /v1/sessions/{sid}/gates/{gid}` — answer an open permission gate.
- `GET /v1/sessions/{sid}/events` — Server-Sent Events stream filtered
  to the caller's visibility.
- `GET /v1/sessions/{sid}/status` — read-only session status.
- `GET /v1/sessions/{sid}/journal` — read-only durable journal view.
- `GET /v1/sessions` — list sessions (read plane).
- `GET /v1/capabilities` — server capabilities advertisement.

## Sibling packages

- [`pkg/rig`](../rig/README.md) — the composition root `serve.Rig` wraps.
- [`pkg/session`](../session/README.md) — the `SessionController` that
  satisfies `LiveSession` structurally at the composition root.
- [`pkg/event`](../event/README.md) — `EventFilter`, `Subscription`,
  `Delivery` for the events SSE stream.
- [`pkg/gate`](../gate/README.md) — `GateResponse` for the gate-answer
  endpoint.
- [`pkg/hub`](../hub/README.md) — the fan-in `SubscribeEvents` reads.

## How it is designed

```
   HTTP client
       │
       │  POST /v1/sessions/{sid}/input  ·  GET /v1/sessions/{sid}/events  ·  ...
       ▼
   ┌────────────────────────────────────────────────┐
   │ serve.Handler (this package)                    │
   │  middleware:  auth → body-cap → request-id →     │
   │               recovery → idempotency             │
   │  mux:  method+path patterns (Go 1.22 syntax)     │
   └────────────────────────────────────────────────┘
       │
       │  LiveSession / Rig (narrow interfaces; the
       │  concrete session.SessionController satisfies
       │  LiveSession structurally at the composition root)
       ▼
   pkg/session (SessionController) ─► pkg/rig (Rig)
       │
       ▼
   the live session machinery (internal/sessionruntime)
```

### Strict Dependency Inversion

`serve` production code imports only `pkg/event`, `pkg/gate`,
`core/content`, `core/uuid`, and stdlib. It never names `pkg/session`,
`pkg/rig`, an LLM package, or a store package. The composition root
instantiates `serve.Rig[session.SessionController, rig.SessionOption]`
and hands it to `Handler`; `serve` depends on the **behavior** without
depending on the **implementation**. The package's dependency-guard
test proves it.

### Disjoint route table

The route patterns are disjoint by design (Go 1.22 method+path syntax):

- the **live plane** owns `/events` (a running in-process session);
- the **read plane** owns `/status`, `/journal`, and the bare
  `/v1/sessions` listing (pure reads over durable history).

`{sid}` and `{gid}` are wildcard path segments the handlers recover
via `r.PathValue`. The method is part of the pattern, so a matching
path with the wrong method yields 405 (not 404) and an unmatched path
yields 404 — both from the mux, before any handler runs.

### Hardened server

`Server` sets the slowloris and resource-exhaustion guards the
`CLAUDE.md` HTTP-server rule mandates: `ReadTimeout` 5 s,
`ReadHeaderTimeout` 5 s, `IdleTimeout` 60 s, `MaxHeaderBytes` 1 MiB.
An SSE GET is read to completion well within `ReadTimeout` (the
long-lived part is the write); it is safe for the events stream while
still bounding a stalled reader. Request bodies are capped separately
by the body-cap middleware.

### Fail-secure public bind

A public bind (empty or non-loopback host) with no authenticator
installed and no `WithInsecurePublicBind` opt-in returns a
`PublicBindWithoutAuthError` and no server. The has-auth bit is
recovered from the handler **without re-parsing options**: if it was
built by `Handler` it satisfies `authAware` and reports whether an
authenticator is installed; any other handler cannot prove auth and
is treated as unauthenticated.

### Idempotency

A `POST /v1/sessions/{sid}/input` is idempotent by the input id: a
redelivered submit with the same id de-duplicates rather than
double-submitting. The idempotency layer is the wire-side counterpart
of the journal's `IdempotencyID` de-dup on the durable side.
