# `pkg/serve` HTTP Session API Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Reshape harness's HTTP session surface into a thin, runner-supplied `pkg/serve` package that projects a compiled `session.Runner` and a stateless read plane onto a versioned HTTP/SSE contract, carrying the full event stream (Enduring + a new live-only Ephemeral frame) with sequenced live delivery.

**Architecture:** Three independent Phase 0 harness changes land first (a `session.Runner` compile/run/restore lifecycle; a sequenced `event.Delivery{Event,JournalSeq}` delivery envelope threaded through `event`/`hub`/`journal`; and a `sessionstore` catalog-fold status projection). Phase 1 then builds `pkg/serve` over narrow `LiveSession`/`Runner`/`Reader` interfaces — an internal `{sid}→LiveSession` table for live/control routes and a stateless read-plane route group — and deletes `pkg/api`. Phase 2 adds the protocol contract: capability discovery, a typed error envelope, the `event: ephemeral` SSE frame class, `id: <journal_seq>` stamping, SSE heartbeats, `Idempotency-Key`, plus OpenAPI/JSON-Schema and golden wire fixtures.

**Tech Stack:** Go stdlib only (`net/http`, `crypto/tls`, `encoding/json`), plus harness `pkg/event`, `pkg/gate`, `pkg/session`, `pkg/hub`, `pkg/journal`, `pkg/sessionstore`, and `github.com/looprig/core` (`content`/`uuid`). No new external dependencies. Tests are table-driven and run with `go test -race`; `make secure` before every commit.

**Authoritative spec:** `docs/plans/2026-07-06-serve-http-session-api-design.md` (§§ referenced throughout). This plan implements Phases 0–2 of §12 only; Phases 3–4 are out of scope. The spec is authoritative — implement it, do not redesign it.

---

## Scope

- **Phase 0** — three parallelizable, test-first workstreams: `session.Runner` (WS-A), the `Delivery` envelope (WS-B), the catalog status fold (WS-C).
- **Phase 1** — create `pkg/serve`; move + reshape the live/control handlers; add the runner-driven create/restore handlers, the internal live-session table, the read-plane route group, `Server` fail-secure bind; delete `pkg/api`.
- **Phase 2** — protocol contract: capabilities, error envelope, ephemeral SSE frame, `id:` stamping, heartbeats, `Idempotency-Key`, OpenAPI/JSON-Schema + golden fixtures + fuzz.

Out of scope (do not implement): Phase 3 server-side `?from_journal_seq=` replay-then-follow join; Phase 4 `session_id` routing / multi-pod placement / remote destroy; any front-end/BFF; any transcript/export layer in `serve`.

---

## Current Code Landmarks (verified 2026-07-08)

- `pkg/session/session.go:745` — `func New(ctx, cfg loop.Config, opts ...Option) (*Session, error)`.
- `pkg/session/session.go:393` — `func (s *Session) SubscribeEvents(filter event.EventFilter) (*hub.EventSubscription, error)` (concrete return, to widen); interface decl at `session.go:334`.
- `pkg/session/session.go:947` — `Submit`; `:1107` — `Interrupt(ctx) (bool, error)`.
- `pkg/session/session.go:1304/1322/1341` — legacy `Approve` / `Deny` / `ProvideUserInput` (to delete); helper `resolveGate` at `:1362`, shared `routeGate` at `:1387` (routeGate is still used by `gates.go` dispatch — keep it).
- `pkg/session/gates.go:404` — `func (s *Session) RespondGate(ctx, gate.GateResponse) error` (the replacement); `GateError`/`GateErrorKind` at `gates.go:75-126` (`GateNotFound`, `GateNotReady`, `GateKindMismatch`, `GateActionInvalid`, `GateCapacity`, `GateAppendFailed`); `(*GateError).GateErrorKind() string` at `:126` (stable string for package boundaries).
- `pkg/session/restore_constructor.go:114` — `func Restore(ctx, cfg loop.Config, sessionID uuid.UUID, store *sessionstore.Store, opts ...Option) (*Session, error)`; fingerprint check in `restoreSession` via `checkFingerprint` (`restore.go:90`); `WithAllowConfigMismatch` opt-out.
- `pkg/session/config_fingerprint.go:66` — `FingerprintFrom(cfg)`; `:79` — `fingerprintWith(cfg, fields)` (the single merge point New stamps and Restore compares); `ConfigFingerprintFields` at `:30`.
- **All 12 `session.Option`s**: `command_journal.go:47` (`type Option func(*Session)`), `:53` `WithCommandAppender`, `:70` `WithCeiling`, `:85` `WithSessionID`, `:100` `WithEventAppender`, `:115` `WithLeaseRelease`, `:129` `WithLimits`, `:141` `WithAllowConfigMismatch`, `:154` `WithConfigFingerprintFields`, `:166` `WithForeignBuilder`, `:180` `WithWorkspaceStore`; `gates.go:131` `WithGateAppender`, `:141` `WithGateCaps`.
- `pkg/hub/deps.go:20-22` — `eventAppender.AppendEvent(ctx, ev) error` (seam to widen to `(uint64, error)`); `nopEventAppender` at `:28`.
- `pkg/hub/hub.go:118` — `PublishEvent`; `:160` `applyAndSnapshot`; `:239` `deliver`; `:295` `appendAndDeliverDerived`; `:362` `StopSession`. All call `appender.AppendEvent` / `deliver`.
- `pkg/hub/subscription.go:57` — `events chan event.Event` (to become `chan event.Delivery`); `:89` `Events() <-chan event.Event`; `:112` `trySend(ev event.Event) sendResult`; `:239`(hub) `deliver`.
- `pkg/journal/appender.go:105` — `JournalEventAppender.AppendEvent` discards the seq (`if _, err := a.journal.Append(...)`); `catalogUpdater.UpdateOnEvent(ctx, ev)` seam at `:28`; `pkg/journal/journal.go:21` — `SessionJournal.Append(ctx, rec) (seq uint64, err error)` (already returns seq).
- `pkg/event/event.go:121-125` — `Subscription` interface (`Events() <-chan Event`); `:19` `Event`; `marshal.go:93` `MarshalEvent` fails closed on Ephemeral (`EphemeralNotPersistableError` at `:34`).
- `pkg/sessionstore/catalog.go:57` — `SessionMeta` (has only `Status` active/stopped today at `:69`; `SessionStatus` enum at `:41`); `:201` `applyEvent` fold; `:375` `UpdateOnEvent`; `:460` `ListSessions`; `:495` `RepairCatalog`; `foldSession` at `:522` discards seq (`ev, _, nerr := cursor.Next(ctx)`).
- `pkg/sessionstore/replay.go:107` — `OpenEventReplayer(id, ReplayRequest{FromSeq})`; `ReplayRequest.FromSeq` at `:26`; `EventCursor.Next` returns `(event.Event, uint64, error)`.
- `pkg/api/` — `api.go` (`Agent`/`AgentRequest`/`Factory`/`Config`), `server.go` (`server`, `sessions map`, `Handler`/`Serve`), `supervisor.go` (per-session subscription drain), `handlers_stream.go` (`streamEvents` SSE loop at `:91`, `handleExport`), `handlers_session.go`, `handlers_gate.go`. **No repo imports `pkg/api`** — it is replaced wholesale, not migrated in place.
- `flow/pkg/flow/compile.go:60` (`Compile`), `runner.go:30/80/154`, `resume.go:39` — the `CompileOption`/`RunOption` split + Compile-captured version fingerprint checked at Resume (the lifecycle model WS-A mirrors, not the code).
- `flow/pkg/ingress/ingress.go:73` (`WithAuth func(*http.Request) error`), `:113` (`/v1` `http.ServeMux` routes), `:526` (typed-error→status map), `:593` (error envelope), `:189` (`Idempotency-Key`), `:612` (`Server`) — the HTTP conventions `serve` mirrors, with the deltas noted below.

### Where `serve` deliberately diverges from `flow/pkg/ingress`

The spec (§6, §7a, §10, Decisions #18) requires three things flow does **not** have today. Follow the **spec**, not flow, for these:
- **Error envelope** is nested `{"error":{"code":...,"message":...,"retryable":...}}` (spec §7a). Flow's is flat `{"error":"..."}`. `serve` is richer by design.
- **Paging** (`skip`/`limit`/`next_skip`/`done`; `from_journal_seq`/`next_journal_seq`) is net-new (spec §6). Flow has none.
- **`Idempotency-Key`** gains a **TTL** and a **409-on-body-mismatch** (spec §6). Flow's is an unbounded in-process map with neither.
- **`Server(addr, h, opts) (*http.Server, error)`** returns an **error** and refuses a non-loopback bind with nil auth unless `WithInsecurePublicBind()` (spec §10, Decision #18). Flow's `Server` returns `*http.Server` with no bind guard.

---

## SETTLED DESIGN DECISION: `session.Option` Compile-time vs per-run split

The one open question (spec §3c): classify every `session.Option` as **Compile-time** (captured into the Runner, identical for every session) or **per-run**. Mirroring flow's `CompileOption`/`RunOption` split (`flow/pkg/flow/{compile,runner}.go`), and constrained by the fact that the narrow `serve.Runner[S]` interface exposes only `Run(ctx)` and `Restore(ctx, id)` **with no per-call options**, every caller-facing option is captured at `Compile`. The options fall into three buckets:

**Bucket 1 — Compile-time (`session.CompileOption`, caller-supplied, captured once, define the agent + policy; identical across every `Run`/`Restore`):**

| Option | Why compile-time |
|---|---|
| `cfg loop.Config` (positional) | The agent definition (LLM/model/system/tools/posture). Fingerprint source (ModelID/SystemPromptRev/ToolPolicyRev). |
| `store *sessionstore.Store` (positional) | The durable backend the Runner opens per-session journals/leases from. Fixed once, like flow's Compile-fixed `CheckpointStore`. |
| `WithLimits` | Subagent depth/quota caps — agent policy, same every run. |
| `WithConfigFingerprintFields` | **The only fingerprinted option.** MUST be identical on create and restore or restore spuriously mismatches (`fingerprintWith` is the shared merge point). |
| `WithWorkspaceStore(ws, root)` | Workspace snapshot store + root; the root feeds the fingerprint (via `ConfigFingerprintFields.WorkspaceRoot`). Agent-level. |
| `WithForeignBuilder(b, rb)` | Foreign-engine build seams — an agent capability, same every run. |
| `WithGateCaps` | Gate-directory bounds — policy, same every run. |
| `WithAllowConfigMismatch` | **Decision: Compile-time policy.** Restore-only, but the serve `Runner.Restore(ctx, id)` interface takes no options, and a failover restore must apply the same fingerprint-drift policy every time. Passed to `Compile` and applied only on the restore path (New ignores it, as today). *(Judgment call — see Ambiguity A2.)* |
| `WithCeiling` | Security-ceiling seam. **See Ambiguity A1** — captured at Compile, but the Runner mints a fresh `*ceiling.State` per `Run` rather than sharing one mutable state across concurrent sessions. |

**Bucket 2 — Runner-derived per-run deps (the Runner builds these itself for each `Run`/`Restore` from `store` + the minted/received `sessionID`; NOT caller options — encapsulating exactly the journal chicken-and-egg that `Compile` exists to hide, replacing what `swe/swarms/swe/persistence.go:225-252` does by hand today):**

| Option | Runner builds it per-run from |
|---|---|
| `WithSessionID` | `Run` mints a fresh `uuid.New()`; `Restore` uses the positional `sid`. |
| `WithEventAppender` | `journal.NewJournalEventAppenderChecked(j, journal.WithCatalog(catalog))` over the per-session journal `j := store.OpenJournal(ctx, sid, lease)`. |
| `WithCommandAppender` | `journal.NewJournalCommandAppenderChecked(j)`. |
| `WithGateAppender` | `journal.NewJournalGateAppenderChecked(j)`. |
| `WithLeaseRelease` | `lease.Release` from `lease := store.AcquireLease(ctx, sid)`. |

**Bucket 3 — none.** The narrow `serve.Runner[S]` interface exposes no per-call knobs; `Run(ctx)`/`Restore(ctx, id)` take only ctx (+ sid). The concrete `*session.Runner` MAY expose richer methods later, but `serve` never uses them.

**Ambiguities surfaced (flag to the reviewer; do not silently resolve differently):**
- **A1 (ceiling state):** `WithCeiling` injects a `*ceiling.State` **shared** with the permission checker inside `cfg.Tools.Permission`. A single `Compile`-captured `cfg` reused across many concurrent `Run`s would share one mutable ceiling — wrong for multi-session pods (spec §3c). The Runner must mint a fresh `ceiling.State` per `Run` and the composition root must supply a `cfg` whose checker binds to a per-run ceiling (or `Compile` must accept a ceiling-state factory). **This is a composition-root (swe) concern the spec defers; the Phase 0 Runner should mint-per-run and document the contract, leaving the checker rebind to swe.** Confirm the exact wiring with the swe composition root before finalizing `Compile`'s signature.
- **A2 (`WithAllowConfigMismatch` placement):** classified Compile-time above for interface minimalism. If a reviewer wants per-restore control, the concrete `*session.Runner.Restore(ctx, id, ...RestoreOption)` can grow a variadic that `serve` ignores. Note it; default to Compile-time.
- **A3 (catalog for the event appender):** `Compile` needs a `*sessionstore.Catalog` to wire into each per-run event appender (`journal.WithCatalog`) so the fold stays live. `Compile` builds it from `store` via `store.OpenCatalog(sessionstore.WithCatalogReplayer(store))` (cheap, no I/O). The same logical catalog (same KV) backs `serve.NewReader`.

---

# PHASE 0 — Prerequisites (three parallel, test-first workstreams)

WS-A, WS-B, WS-C touch mostly disjoint packages and can run in parallel. The one coupling: WS-C's `LastJournalSeq` field needs the seq the appender receives; WS-B owns `pkg/journal/appender.go` and widens `catalogUpdater.UpdateOnEvent(ctx, ev)` → `(ctx, ev, seq)` there, WS-C consumes the new `seq` in the fold. **Coordinate that one interface-vs-implementation edge** (see Task B4 / Task C1). Land WS-B before WS-C's LastJournalSeq step, or stub the seam.

---

## WS-A — `session.Runner` + surface cleanups

### Task A1: Widen `Session.SubscribeEvents` to return `event.Subscription`

**Files:**
- Modify: `pkg/session/session.go:334` (interface decl), `:393` (method).
- Test: `pkg/session/session_test.go` (add/adjust a case asserting the return type satisfies `event.Subscription`).

**Step 1 — failing test.** Add a table-driven test asserting the widened contract:

```go
func TestSubscribeEventsReturnsInterface(t *testing.T) {
    t.Parallel()
    // compile-time assertion the method returns event.Subscription
    var s *session.Session
    var _ func(event.EventFilter) (event.Subscription, error) = s.SubscribeEvents
}
```

This will not compile while the method returns `*hub.EventSubscription`.

**Step 2 — run, verify fail.** `go test -race ./pkg/session -run TestSubscribeEventsReturnsInterface` → build error (return type mismatch).

**Step 3 — implement.** Change both the interface member (`session.go:334`) and the method (`:393`) from `(*hub.EventSubscription, error)` to `(event.Subscription, error)`. Body unchanged — `*hub.EventSubscription` structurally satisfies `event.Subscription` (`event.go:121`). Verified non-breaking at the one production caller: `swe/swarms/swe/agent.go:143` already narrows the result into `event.Subscription`.

**Step 4 — run, verify pass.** `go test -race ./pkg/session`.

**Step 5 — commit.** `git add pkg/session && git commit -m "refactor(session): widen SubscribeEvents to event.Subscription"`

---

### Task A2: Delete the legacy `Approve`/`Deny`/`ProvideUserInput` trio

**Files:**
- Modify: `pkg/session/session.go` (delete `:1304-1360`; delete `resolveGate` at `:1362` **only if** no remaining caller — `routeGate` at `:1387` stays, it is called by `gates.go:516`).
- Modify: `pkg/session/session_test.go`, `agency_test.go`, `command_journal_test.go` — replace every `Approve`/`Deny`/`ProvideUserInput` test call with the `RespondGate(gate.GateResponse{...})` equivalent (see the coordination note below for the exact `GateResponse` shapes).

**Step 1 — failing test.** Convert the existing trio tests (`session_test.go:508-1559`, `agency_test.go:60-70`, `command_journal_test.go:363-518`) to drive `RespondGate` with a `gate.GateResponse` (`Action:"approve"` + `Values{"scope":..., "accepted_grants":...}`, `Action:"deny"`, `Action:"answer"` + `Values{"answer":...}`). These reference the deleted methods and must be updated first; they fail to compile against the still-present trio only after deletion.

**Step 2 — run, verify fail.** `go test -race ./pkg/session` → build error referencing removed methods (proves the tests now depend on `RespondGate`).

**Step 3 — implement.** Delete the three methods. Remove `resolveGate` if unused after deletion (grep: `rg -n "resolveGate\(" pkg/session`). Keep `routeGate`.

**Step 4 — run, verify pass.** `go test -race ./pkg/session`.

**Step 5 — commit.** `git add pkg/session && git commit -m "refactor(session): remove legacy gate trio in favor of RespondGate"`

> **Cross-repo coordination (do NOT do here — schedule with WS-B's coordination task B6):** deleting the trio breaks `swe/swarms/swe/agent.go:231/238/245` (which delegate to it) and, transitively, the cli's `tui.Agent.Approve/Deny/ProvideAnswer` (`cli/tui/agent.go:61-70`, called from `cli/tui/commands.go:122-144`). swe and cli must route through `RespondGate`/`gate.GateResponse`. Because harness and swe/cli are separate modules (cli uses `replace => ../harness`), this is a coordinated bump. Land it together with the WS-B breaking change (Task B6) so the sibling modules absorb both at once.

---

### Task A3: Add `session.Runner` — `Compile` (design-time) + `Run`/`Restore` (runtime)

**Files:**
- Create: `pkg/session/runner.go`
- Create: `pkg/session/runner_test.go`
- (Reference only, do not edit) `swe/swarms/swe/persistence.go:225-252` — the by-hand wiring the Runner extracts.

**Step 1 — failing test.** Table-driven tests over an in-memory store (`sessionstore.Open` over a `memstore`/`fsstore` temp backend, following `pkg/sessionstore` test helpers):

```go
func TestRunnerRun(t *testing.T) {
    t.Parallel()
    tests := []struct {
        name    string
        opts    []session.CompileOption
        wantErr bool
    }{
        {name: "happy path mints id and live session"},
        {name: "nil store rejected at Compile", wantErr: true},         // boundary
        {name: "run twice mints two distinct session ids"},             // edge
    }
    // ...Compile → Run(ctx) → assert non-zero uuid, non-nil *Session, SubscribeEvents works
}

func TestRunnerRestore(t *testing.T) {
    t.Parallel()
    // happy: Run→append events→Shutdown→Restore(sid) rebuilds a live session
    // error: Restore unknown sid → typed *RestoreDiscoveryError
    // error: config fingerprint mismatch → *ConfigMismatchError (unless WithAllowConfigMismatch)
    // edge: Restore before Run (no journal) → typed discovery error, not panic
}
```

**Step 2 — run, verify fail.** `go test -race ./pkg/session -run 'TestRunner'` → FAIL (`session.Compile` undefined).

**Step 3 — implement.** In `runner.go`:

```go
// CompileOption configures the immutable Runner at design time. Mirrors flow's
// CompileOption; distinct from the runtime session.Option set (which the Runner
// derives per-run). Applied over an unexported compileConfig.
type CompileOption func(*compileConfig)

type compileConfig struct {
    limits              Limits
    fingerprintFields   ConfigFingerprintFields
    ws                  *workspacestore.Store
    wsRoot              string
    foreign             foreignloop.Builder
    foreignRestored     foreignloop.RestoredBuilder
    gateCaps            GateCaps
    allowConfigMismatch bool
    newCeiling          func() *ceiling.State // A1: fresh per-run ceiling
    // ...
}

// Compile binds the agent definition (cfg) and durable backend (store) into an
// immutable, reusable Runner and captures the config-fingerprint inputs (reusing
// fingerprintWith). Design-time. A nil store is rejected with a typed error.
func Compile(cfg loop.Config, store *sessionstore.Store, opts ...CompileOption) (*Runner, error)

type Runner struct { /* cfg, store, catalog, compiled opts (immutable) */ }

// Run mints a fresh session id, builds the per-run durable deps (lease, journal,
// event/command/gate appenders, lease-release) from store+id, and brings up a live
// session via session.New. Runtime.
func (r *Runner) Run(ctx context.Context) (uuid.UUID, *Session, error)

// Restore rebuilds a live session from its journal via session.Restore, refusing a
// fingerprint mismatch (unless WithAllowConfigMismatch was compiled in). Runtime.
func (r *Runner) Restore(ctx context.Context, id uuid.UUID) (*Session, error)
```

`Run` replicates `persistence.go:225-252`: `lease := store.AcquireLease(ctx, id)` → `j := store.OpenJournal(ctx, id, lease)` → `evAp := journal.NewJournalEventAppenderChecked(j, journal.WithCatalog(r.catalog))` → `cmdAp := journal.NewJournalCommandAppenderChecked(j)` → `gateAp := journal.NewJournalGateAppenderChecked(j)` → `session.New(ctx, r.cfg, WithSessionID(id), WithEventAppender(evAp), WithCommandAppender(cmdAp), WithGateAppender(gateAp), WithLeaseRelease(lease.Release), + captured Compile opts)`. Release the lease best-effort on any construction failure (mirror `releaseLeaseBestEffort`).

`Restore` delegates to `session.Restore(ctx, r.cfg, id, r.store, + captured Compile opts including WithConfigFingerprintFields and, if compiled, WithAllowConfigMismatch)`. **Confirm** whether `session.Restore` wires the event/command/gate appenders internally (it holds the store) or expects them as options; supply whichever it requires — this is the spec's Phase 0 "confirm the rest of the port" step.

Define typed errors: `NilStoreError`, and reuse `RunError`/existing session errors. Every package-level API returns a typed error (CLAUDE.md).

Add the `CompileOption` constructors matching Bucket 1: `WithCompileLimits`, `WithCompileConfigFingerprintFields`, `WithCompileWorkspaceStore`, `WithCompileForeignBuilder`, `WithCompileGateCaps`, `WithCompileAllowConfigMismatch`, `WithCompileCeilingFactory` (A1). Numeric/nil-arg guards follow flow's fail-safe convention (nil/zero ignored, keep default).

**Step 4 — run, verify pass.** `go test -race ./pkg/session -run 'TestRunner'` then `go test -race ./pkg/session`.

**Step 5 — commit.** `git add pkg/session && git commit -m "feat(session): add Runner (Compile/Run/Restore) lifecycle"`

---

## WS-B — sequenced live delivery: the `event.Delivery` envelope

### Task B1: Add `event.Delivery` and widen the `Subscription` interface

**Files:**
- Modify: `pkg/event/event.go` (add `Delivery`; change `Subscription.Events()` at `:122`).
- Test: `pkg/event/event_test.go` (new/existing) — assert `Delivery` zero value and field wiring.

**Step 1 — failing test.**

```go
func TestDeliveryZeroValue(t *testing.T) {
    t.Parallel()
    tests := []struct {
        name string
        d    event.Delivery
        want uint64
    }{
        {name: "zero delivery has zero seq", d: event.Delivery{}, want: 0},
        {name: "ephemeral carries zero seq", d: event.Delivery{Event: fakeEphemeral{}, JournalSeq: 0}, want: 0},
        {name: "enduring carries seq", d: event.Delivery{Event: fakeEnduring{}, JournalSeq: 42}, want: 42},
    }
    // assert d.JournalSeq == want
}
```

**Step 2 — run, verify fail.** `go test -race ./pkg/event` → FAIL (`event.Delivery` undefined).

**Step 3 — implement.**

```go
// Delivery is one fan-in delivery: the event plus its durable journal sequence.
// JournalSeq is 0 for Ephemeral deliveries (never persisted, never sequenced) and
// the strictly-monotonic append sequence for Enduring deliveries.
type Delivery struct {
    Event      Event
    JournalSeq uint64
}

type Subscription interface {
    Events() <-chan Delivery   // was: <-chan Event
    Close() error
    Err() error
}
```

Do NOT touch `Header`, `MarshalEvent`, or the durable codec — the sequence never enters the persisted envelope (spec §7b invariant).

**Step 4 — run, verify fail-to-compile downstream is expected.** `go test -race ./pkg/event` PASSES; `go build ./...` now fails in `hub`/`session`/`api` — those are Tasks B2–B5.

**Step 5 — commit** (with B2 as one logical breaking change, or commit `event` alone and let the module stay red until B5 — prefer committing B1–B5 as a chain, each package green in isolation is impossible mid-flight; commit at B5 when `go build ./...` is green). Recommended: hold the commit until Task B5.

---

### Task B2: Widen the hub appender seam to return the seq

**Files:**
- Modify: `pkg/hub/deps.go:20-30` (`eventAppender` interface + `nopEventAppender`).
- Test: `pkg/hub/deps_test.go`.

**Step 1 — failing test.** Assert the nop appender and any fake appender return `(uint64, error)`:

```go
func TestNopAppenderReturnsZeroSeq(t *testing.T) {
    t.Parallel()
    seq, err := nopEventAppender{}.AppendEvent(context.Background(), fakeEnduring{})
    if err != nil || seq != 0 { t.Fatalf("got (%d,%v)", seq, err) }
}
```

**Step 2 — run, verify fail.** `go test -race ./pkg/hub -run TestNopAppenderReturnsZeroSeq` → build error.

**Step 3 — implement.** `AppendEvent(ctx, ev) (uint64, error)`; `nopEventAppender` returns `(0, nil)`.

**Step 4/5 — hold** for the chain (green at B5).

---

### Task B3: Thread the seq through the hub egress + delivery path

**Files:**
- Modify: `pkg/hub/subscription.go` (egress channel `chan event.Delivery`; `Events() <-chan event.Delivery` at `:89`; `trySend(d event.Delivery)` at `:112`).
- Modify: `pkg/hub/hub.go` (`PublishEvent:118`, `applyAndSnapshot:160`, `deliver:239`, `appendAndDeliverDerived:295`, `StopSession:362`).
- Test: `pkg/hub/hub_test.go`, `durable_tap_test.go`, `subscription_test.go`.

**Step 1 — failing tests.** Table-driven, `-race`:
- Enduring publish delivers `Delivery{Event, JournalSeq=<appender seq>}`.
- Ephemeral publish delivers `Delivery{Event, JournalSeq=0}` (no append).
- Derived `SessionActive`/`SessionIdle` deliveries carry their own append seq.
- `StopSession` delivery carries the `SessionStopped` append seq.
- Overflow policy unchanged: Enduring overflow still fails the sub with `SubscriptionLossError`; Ephemeral overflow still drops.

**Step 2 — run, verify fail.** `go test -race ./pkg/hub` → build errors + assertion failures.

**Step 3 — implement.**
- `subscription.go`: `events chan event.Delivery`; `Events() <-chan event.Delivery`; `trySend(d event.Delivery) sendResult` (select on `s.events <- d`).
- `hub.go`: capture `seq` from `appender.AppendEvent` in `PublishEvent` (Enduring path; Ephemeral uses `seq=0`). Thread `seq` into `deliver(subs, ev, seq)` → `sub.trySend(event.Delivery{Event: ev, JournalSeq: seq})`. `applyAndSnapshot` unchanged in shape, but the caller now has both the triggering event's seq and the derived event's own seq (from its own `AppendEvent`). `appendAndDeliverDerived` and `StopSession` capture their derived event's seq from their `AppendEvent` call and deliver with it. `ShouldDeliver` filter still runs on `ev` (unwrap from the Delivery at the call boundary).

**Step 4/5 — hold** for the chain (green at B5).

---

### Task B4: Return the seq from the journal appender; widen the catalog seam

**Files:**
- Modify: `pkg/journal/appender.go:105` (`JournalEventAppender.AppendEvent` returns the seq from `journal.Append`); `catalogUpdater` interface `:28` → `UpdateOnEvent(ctx, ev, seq)`; `nopCatalogUpdater` `:35`.
- Test: `pkg/journal/appender_test.go` (or the existing journal appender test).

**Step 1 — failing test.**

```go
func TestJournalEventAppenderReturnsSeq(t *testing.T) {
    t.Parallel()
    // fake SessionJournal.Append returns seq=7; assert AppendEvent returns (7, nil)
    // fake catalog records the (ev, seq) it was notified with
}
```

**Step 2 — run, verify fail.** `go test -race ./pkg/journal` → build error.

**Step 3 — implement.** `func (a *JournalEventAppender) AppendEvent(ctx, ev) (uint64, error)` — capture `seq, err := a.journal.Append(ctx, NewEventRecord(ev))`, return `err` on failure, else `a.catalog.UpdateOnEvent(ctx, ev, seq)` (best-effort, ignored) and `return seq, nil`. Widen `catalogUpdater.UpdateOnEvent` to `(ctx, ev, seq)`; `nopCatalogUpdater` ignores all three. **This interface change is the one edge shared with WS-C** — `*sessionstore.Catalog.UpdateOnEvent` (Task C1) must match the new signature.

**Step 4/5 — hold** for the chain.

---

### Task B5: Absorb the breaking change in harness-internal `.Events()` consumers; make `go build ./...` green; commit the chain

**Files:**
- Modify: `pkg/session/drain.go:102,113` (`ev, ok := <-sub.Events()` → `d, ok := <-sub.Events(); ev := d.Event`).
- Modify: `pkg/api/handlers_stream.go:96` (`ev, ok := <-sub.Events()` → unwrap `d.Event` before `MarshalEvent`); `pkg/api/supervisor.go:47` (`for range s.sub.Events()` — value already discarded, only the element type shifts, no change needed).
- Modify: every harness test that reads `<-sub.Events()` or `range sub.Events()`: `pkg/session/{session_hub,session,submit,subagent_run,subagent_result,quiescence,restore_roundtrip,depth_cap,security_ceiling,foreign_e2e}_test.go`, `pkg/hub/{hub,subscription}_test.go` — unwrap `.Event`.

**Step 1 — run, observe the full break.** `go build ./... && go test -race ./...` → the compile errors enumerate every site.

**Step 2 — implement** the mechanical `.Event` unwrap at each site.

**Step 3 — verify.** `go build ./... && go test -race ./...` green; `make secure`.

**Step 4 — commit the whole B1–B5 chain as one breaking change.**
```bash
git add pkg/event pkg/hub pkg/journal pkg/session pkg/api
git commit -m "feat(event/hub/journal): sequenced live delivery via event.Delivery envelope"
```

---

### Task B6: Sibling-module coordination (`swe`, `cli`) — the one cross-repo bump

**Not a harness commit.** This is the coordinated absorption of the WS-B envelope change **and** the WS-A trio deletion (Task A2) by the two sibling modules that build against harness. Do it as a paired follow-up after the harness chain lands; it belongs in the swe/cli repos, not harness.

- **swe** (`swe/swarms/swe/`): `persistence.go:349` `for ev := range sub.Events()` → unwrap `.Event`. `agent.go:231/238/245` — replace `session.Approve/Deny/ProvideUserInput` delegations with `session.RespondGate(ctx, gate.GateResponse{...})`. swe tests `acceptance_test.go`, `persistence_integration_test.go`, `operator_eval_integration_test.go`, `agent_test.go`, `runtime_skills_integration_test.go`.
- **cli** (`cli/tui/`): `commands.go:72` `ev, ok := <-sub.Events()` → unwrap `.Event`; `messages.go:12` `eventMsg{ev event.Event}` keeps `event.Event` (unwrap at the boundary) — `agent.go:19` `type EventStream = event.Subscription` propagates automatically. `commands.go:122-144` gate replies (`Approve/Deny/ProvideAnswer`) route through the swe agent's new `RespondGate`-backed methods. cli builds harness via `replace => ../harness` **and** carries a `vendor/` tree: after the harness bump, run `go mod tidy && go mod vendor` in `cli` so the vendored `harness/pkg/{event,hub,...}` match, then fix the consumers. cli tests: `screen_test.go`, `run_test.go`, `restore_test.go`.

Deliverable of this task: a note/checklist in the swe and cli repos' own change, verified with `go build ./... && go test -race ./...` in each. Record completion; do not block the harness Phase 1 work on it (harness stays green independently).

---

## WS-C — `sessionstore` catalog status fold + read

### Task C1: Match the widened `UpdateOnEvent` seam; extend `SessionMeta` + the fold

**Files:**
- Modify: `pkg/sessionstore/catalog.go` (`SessionMeta:57`; `applyEvent:201`; `UpdateOnEvent:375` signature; `foldSession:522` to pass seq; add `ReadMeta`).
- Test: `pkg/sessionstore/catalog_test.go`.

**Step 1 — failing tests.** Table-driven fold assertions (fold correctness is `sessionstore`'s responsibility per spec §11):

```go
func TestApplyEventStatusFold(t *testing.T) {
    t.Parallel()
    tests := []struct {
        name    string
        events  []seqEvent   // (event, seq) in order
        wantState        SessionState
        wantLastSeq      uint64
        wantActiveTurn   uuid.UUID
        wantWaitingGate  uuid.UUID
    }{
        {name: "SessionStarted → idle"},                                  // happy
        {name: "TurnStarted → running, ActiveTurnID set"},                // happy
        {name: "GateOpened → waiting_on_gate, WaitingGateID set"},        // edge
        {name: "GateResolved clears WaitingGateID, back to running"},     // edge
        {name: "TurnDone → idle, LastTurn summary set, ActiveTurnID cleared"},
        {name: "TurnFailed → failed, LastTurn summary set"},
        {name: "TurnInterrupted → interrupted"},
        {name: "SessionStopped → stopped (terminal wins)"},               // boundary
        {name: "StepDone → LastStep summary set, LastJournalSeq bumped"},
        {name: "unrelated event → no change"},                            // no-op
        {name: "zero seq event → LastJournalSeq unchanged if lower"},     // boundary
    }
    // fold each; assert projected fields
}
```

**Step 2 — run, verify fail.** `go test -race ./pkg/sessionstore -run TestApplyEventStatusFold` → build/assert failures.

**Step 3 — implement.**
- Add a typed `SessionState string` enum: `StateRunning`, `StateWaitingOnGate`, `StateIdle`, `StateFailed`, `StateInterrupted`, `StateStopped` (spec §3d). Keep the existing `Status` (`active`/`stopped`) field for back-compat, OR supersede it — **decide and note**; additive is safest (`decodeSessionMeta` uses `DisallowUnknownFields`, so absent new fields on old entries decode fine; the catalog is rebuildable via `RepairCatalog`).
- Add fields to `SessionMeta`: `State SessionState`, `LastJournalSeq uint64`, `ActiveTurnID uuid.UUID (omitzero)`, `WaitingGateID uuid.UUID (omitzero)`, `LastTurn *terminalSummary (omitempty)`, `LastStep *stepSummary (omitempty)`, all `json` snake_case.
- **`LastTurn`/`LastStep` must NOT embed a bare `event.Event`** (an interface field breaks `json.Marshal`/`DisallowUnknownFields` round-trip). Store a codec-safe summary: `{journal_seq uint64, event json.RawMessage}` where `event` is `event.MarshalEvent(ev)` bytes, OR a projected scalar summary (turn index, error kind, step id). **Ambiguity A4** — pick the RawMessage-of-MarshalEvent form so `serve` can reconstruct the spec's `StatusEvent{JournalSeq, Event}` DTO (§3d) losslessly; confirm with the reviewer.
- Widen `applyEvent(meta, ev, now)` → `applyEvent(meta, ev, seq, now)`; fold: `TurnStarted`→`StateRunning`+`ActiveTurnID`; `GateOpened`→`StateWaitingOnGate`+`WaitingGateID`; `GateResolved`→clear `WaitingGateID`, back to `StateRunning`/`StateIdle`; `TurnDone`→`StateIdle`+`LastTurn`+clear `ActiveTurnID`; `TurnFailed`→`StateFailed`+`LastTurn`; `TurnInterrupted`→`StateInterrupted`; `StepDone`→`LastStep`; `SessionStopped`→`StateStopped`. Every relevant event bumps `LastJournalSeq = max(LastJournalSeq, seq)`. **Do not** surface `GatePrepared` (private) — it never reaches the event replayer (`replay.go:361` filters `kindGatePrepared`), so the fold never sees it.
- Widen `UpdateOnEvent(ctx, ev, seq)` (matching Task B4's `catalogUpdater` seam) and thread `seq` into `upsert`→`applyEvent`.
- `foldSession:532` — capture the seq: `ev, seq, nerr := cursor.Next(ctx)` and pass to `applyEvent`.
- Add `func (c *Catalog) ReadMeta(ctx, id uuid.UUID) (SessionMeta, bool, error)` — a single-key KV `load` (reuse `load:415`), returning `(meta, found, err)`, never a journal replay (spec §3d, §6 status-read contract).

**Step 4 — run, verify pass.** `go test -race ./pkg/sessionstore`.

**Step 5 — commit.** `git add pkg/sessionstore && git commit -m "feat(sessionstore): catalog status projection (state/seq/turn/gate summaries)"`

> **Coordination:** Task C1's `UpdateOnEvent(ctx, ev, seq)` signature must land with Task B4's widened `catalogUpdater` seam. If WS-C runs ahead of WS-B, temporarily keep the old `UpdateOnEvent(ctx, ev)` and add a seq via a follow-up; prefer sequencing B4 before C1's `LastJournalSeq` step.

### Task C2: Paged listing helper (optional Phase 0 seam, else Phase 1)

Spec §6 requires paged `ListSessions` (`skip`/`limit`, stable sort `last_active_at desc, session_id asc`, `next_skip`/`done`). The catalog's `ListSessions:460` returns all metas sorted by id. **Decision:** keep the paging/sort/slice logic in `serve.NewReader`'s adapter (Phase 1, Task P1-8), reading the full `[]SessionMeta` and slicing — the spec explicitly allows "read all, stable sort, slice" initially. No `sessionstore` change needed for paging in Phase 0. (A storage-backed cursor can replace it later without changing the HTTP contract.)

---

# PHASE 1 — reshape & wrap into `pkg/serve`

Phase 1 depends on all of Phase 0. Build `pkg/serve` from scratch (do not `git mv` `pkg/api` — the shape changes enough that a fresh package is cleaner; delete `pkg/api` at the end, Task P1-11). Handler tests use a **fake `serve.LiveSession`** and a **fake `serve.Reader`** (spec §11) — no real loop/LLM/store.

### Task P1-1: Package skeleton + narrow interfaces

**Files:**
- Create: `pkg/serve/serve.go` (package doc + the three interfaces + the generic `Handler` signature stub).
- Create: `pkg/serve/deps_test.go` (compile-time assertions that `*session.Session` / `*session.Runner` satisfy the interfaces — but via a local test, NOT by importing `pkg/session` into non-test code; keep production `serve` free of `pkg/session`, spec §2).

**Step 1 — failing test.** In `deps_test.go`:

```go
var _ serve.LiveSession = (*session.Session)(nil)          // structural satisfaction
var _ serve.Runner[*session.Session] = (*session.Runner)(nil)
```

**Step 2 — run, verify fail.** `go test -race ./pkg/serve` → build error (interfaces undefined).

**Step 3 — implement** the interfaces exactly as spec §3a/§3c:

```go
type LiveSession interface {
    Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error)
    SubscribeEvents(filter event.EventFilter) (event.Subscription, error)
    RespondGate(ctx context.Context, response gate.GateResponse) error
    Interrupt(ctx context.Context) (bool, error)
}

type Runner[S LiveSession] interface {
    Run(ctx context.Context) (uuid.UUID, S, error)
    Restore(ctx context.Context, id uuid.UUID) (S, error)
}
```

`serve` imports only `pkg/event`, `pkg/gate`, `core/content`, `core/uuid` (+ Reader deps). It must NOT import `pkg/session`, any LLM, or any store (verified by a deps guard test, mirroring `pkg/tool/deps_test.go`).

**Step 4/5 — run + commit** `git add pkg/serve && git commit -m "feat(serve): package skeleton and narrow interfaces"`

### Task P1-2: Internal live-session table

**Files:** Create `pkg/serve/registry.go` + `registry_test.go`.

**Steps (TDD):** a mutex-guarded `map[uuid.UUID]LiveSession` with `get`/`put`/`putIfAbsent`/`delete`, modeled on `pkg/api/server.go:62-136` but holding a bare `LiveSession` (no supervisor — subscriptions are per-request now, spec §8). Table tests: put+get, putIfAbsent collision returns false, delete returns the entry, get-missing returns `(nil,false)`. Never call a `LiveSession` method under the lock.

### Task P1-3: Error envelope + typed errors + ID/query parsing

**Files:** Create `pkg/serve/errors.go`, `parse.go` + tests.

**Steps (TDD):** the **spec §7a nested envelope** `errorResponse{Error errorBody{Code, Message, Retryable}}`; a `writeError(w, status, code, message, retryable)` helper (message generic, cause logged via slog, never returned). Typed errors: `SessionNotFoundError`, `LoopNotFoundError`, `StoreReadError`, `PublicBindWithoutAuthError` (Task P1-10), plus reused `gate.GateError` mapping. `parseSessionID`/`parseGateID` (via `uuid.UnmarshalText`, 400 on malformed — mirror `handlers_session.go:64`, `handlers_gate.go:62`); bounded int query parse for `skip`/`limit`/`from_journal_seq`. Table tests: valid/empty/wrong-length/non-hex ids; limit above cap → 400; negative skip → 400.

### Task P1-4: Auth seam + body cap options

**Files:** Create `pkg/serve/options.go` + `middleware.go` + tests.

**Steps (TDD):** `Option func(*config)`, `WithAuth(authn func(*http.Request) error)`, `WithMaxBodyBytes(n int64)` (spec §10). Middleware: a non-nil authenticator error returns generic `401 {"error":{"code":"unauthorized",...}}` before any session/read call (mirror `ingress.go:140`); nil auth = no auth. Body cap via `http.MaxBytesReader`. Table tests: nil auth allows; authenticator returns error → 401 with no session/reader touched (assert the fake reader/live-session got zero calls); auth error string never echoed.

### Task P1-5: Create/restore handlers over the supplied Runner

**Files:** Create `pkg/serve/handlers_lifecycle.go` + test.

**Steps (TDD):**
- `POST /v1/sessions` — `runner.Run(ctx)` → attach to registry → if body `{"blocks":[...]}` present+non-empty, `LiveSession.Submit` → `201 {"session_id":..., "command_id":...}`; if absent, `201 {"session_id":...}`. Decode blocks via `content.UnmarshalBlocks` (400 on malformed). (Idempotency-Key handling is Phase 2, Task P2-6.)
- `POST /v1/sessions/{sid}/restore` — `runner.Restore(ctx, sid)` → attach → `200 {"session_id":...}`.
- Table tests: idle create (session_id only); create-with-blocks (session_id + command_id, Submit called after attach); runner.Run error → 500 (generic); Submit error after attach → 500; malformed body → 400; restore happy → 200; restore unknown/rebuild error → mapped typed status; restore reattaches so subsequent live routes resolve.

### Task P1-6: Input + interrupt handlers

**Files:** Create `pkg/serve/handlers_control.go` + test.

**Steps (TDD):** `POST /v1/sessions/{sid}/input` → `LiveSession.Submit` → `200 {"command_id":...}` (fire-and-forget); `POST /v1/sessions/{sid}/interrupt` → `LiveSession.Interrupt` → `200 {"interrupted":bool}`. 400 malformed sid; 404 unknown live session; 400 empty/undecodable blocks; 500 on method error. Reshape from `handlers_session.go:96-217`/`handlers_gate.go:96-140` but drop the DELETE route (no HTTP destroy in v1, spec §6).

### Task P1-7: Gate response handler with authoritative error mapping

**Files:** Create `pkg/serve/handlers_gate.go` + test.

**Steps (TDD):** `POST /v1/sessions/{sid}/gates/{gid}` — decode `gate.ResponseRequest{Action,Values}`, build `gate.GateResponse{GateID:gid, Action, Values, Source:{Kind:ResponseFromUser}}` (server stamps provenance; client provenance ignored), call `LiveSession.RespondGate` → `202 Accepted`. Map `gate.GateError` via `(*GateError).GateErrorKind()` (do NOT import `pkg/session`): `not_found`→404, `action_invalid`/`kind_mismatch`→400, `not_ready`→409, `append_failed`→500, `capacity`→503 (spec §10). Table tests: happy 202; 404 stale/unknown; 400 invalid action; 400 kind mismatch; 409 not ready; 503 capacity; 500 append failed; malformed sid/gid → 400; malformed body → 400. Permission scope asserted as stable string `values.scope` ∈ {`once`,`session`,`workspace`} (spec §8), not numeric enum. No `serve` open-gate registry (spec Decision #7) — there is NO `GET .../gates` list route in serve (that was api-only; the read plane exposes status/journal instead).

### Task P1-8: SSE events handler (Enduring-only for Phase 1)

**Files:** Create `pkg/serve/handlers_events.go` + test.

**Steps (TDD):** `GET /v1/sessions/{sid}/events` — resolve live session (400/404), `SubscribeEvents(allEventsFilter)`, then a stream loop reshaped from `handlers_stream.go:41-113`: read `d, ok := <-sub.Events()` (now `event.Delivery`), `event.MarshalEvent(d.Event)`; **skip** what the marshaler rejects (every Ephemeral + unknown) — Phase 1 carries Enduring only, exactly as today. Close the subscription on `r.Context().Done()` or channel close (no `Interrupt`). Set `Content-Type: text/event-stream`, clear the write deadline via `http.NewResponseController`. (Ephemeral frames, `id:` stamping, and heartbeats are Phase 2, Tasks P2-2/P2-3/P2-4.) Table tests (with a fake subscription feeding `Delivery`s): Enduring event → one `data:` frame; Ephemeral delivery → skipped; client-cancel → returns and closes sub; subscribe error → 500 before any SSE header.

### Task P1-9: Read plane — `NewReader`, `Reader` interface, list/status/journal handlers

**Files:** Create `pkg/serve/reader.go`, `handlers_read.go` + tests.

**Steps (TDD):** define the narrow `Reader` (spec §3d):

```go
type Page struct { Skip, Limit int }
type JournalPage struct { From uint64; Limit int }
type Reader interface {
    ListSessions(ctx context.Context, page Page) (SessionList, error)
    ReadStatus(ctx context.Context, id uuid.UUID) (SessionStatus, error)
    ReadJournal(ctx context.Context, id uuid.UUID, page JournalPage) (EventJournalPage, error)
}
```

`serve.SessionStatus` DTO per spec §3d (`SessionID`, `State string`, `LastJournalSeq`, `ActiveTurnID`, `WaitingGateID`, `LastTurn *StatusEvent`, `LastStep *StatusEvent`, `UpdatedAt`); `StatusEvent{JournalSeq, Event event.Event}`. `NewReader(reads)` wraps a concrete adapter; the default adapter (a separate `serve/catalogreader` or a small adapter type) is built over `sessionstore.Catalog` (`ListSessions`, `ReadMeta`) + `Store.OpenEventReplayer`. Handlers:
- `GET /v1/sessions?skip&limit` — paging contract §6: `skip` default 0, `limit` default 100 hard-cap 1000 (>1000 → 400), stable sort `last_active_at desc, session_id asc`, response `{"sessions":[...],"skip","limit","next_skip","done"}`, `done` = fewer than `limit` returned.
- `GET /v1/sessions/{sid}/status` — `ReadStatus` (catalog `ReadMeta` projection, no replay); 404 if absent.
- `GET /v1/sessions/{sid}/journal?from_journal_seq&limit` — `OpenEventReplayer(FromSeq)`, stop after `limit` yielded Enduring events; response `{"events":[{"journal_seq","event"}],"next_journal_seq","done"}`; `from_journal_seq` 0/absent = beginning; `limit` default 100 cap 1000; `GatePrepared` never appears (replayer filters it); reconstruct `StatusEvent`/journal events from the catalog's codec-safe form (Task C1 A4).

Table tests per spec §11: list absent/explicit skip, default limit, limit>cap→400, stable sort, `next_skip`, `done`; status running/waiting/idle/failed/interrupted/stopped/no-live-session; journal absent/explicit `from_journal_seq`, default limit, limit>cap→400, `next_journal_seq`, `done`, malformed params, replay error → typed HTTP failure. These route to the **catalog/replay** (any pod, no live session) — assert a status/journal read succeeds with NO live session in the registry.

### Task P1-10: `Handler` mux + `Server` fail-secure bind

**Files:** Create `pkg/serve/mux.go`, `server.go` + tests.

**Steps (TDD):**
- `Handler[S LiveSession](runner Runner[S], reads Reader, opts ...Option) http.Handler` — one `http.ServeMux`, all routes under `/v1` (spec §6 table), live/control + create/restore + read-plane mounted together with **no path overlap** (live owns `/events`; reader owns `/journal`, `/status`, listing). Wrap in auth + body-cap middleware.
- `Server(addr string, h http.Handler, opts ...ServerOption) (*http.Server, error)` — hardened timeouts + `TLSConfig.MinVersion tls.VersionTLS12` (mirror `ingress.go:612` + `api/server.go:224`). **Fail-secure:** a non-loopback `addr` with a nil authenticator returns typed `*PublicBindWithoutAuthError` unless `WithInsecurePublicBind()` was passed (spec §10, Decision #18). Reuse the loopback detection from `api.go:113` (`isLoopbackHost`). The handler must expose whether an authenticator is installed to `Server` (e.g. `Handler` returns a type that carries the flag, or `Server` takes it via a `ServerOption`/the handler interface) — **design note:** thread the "has auth" bit cleanly; do not re-parse.

Table tests: loopback + nil auth → ok; non-loopback + nil auth → `*PublicBindWithoutAuthError`; non-loopback + nil auth + `WithInsecurePublicBind()` → ok; non-loopback + auth → ok; malformed addr → typed error; timeout/TLS defaults set. (These may be split into `server_test.go` unit tests; SSE flush/teardown + loopback-vs-public live behavior are integration-tagged, spec §11.)

### Task P1-11: Delete `pkg/api`

**Files:** Delete the whole `pkg/api/` directory.

**Steps:** confirm zero importers (`rg -l "looprig/harness/pkg/api" -g '*.go'` → only the dir itself), then `git rm -r pkg/api`. This drops harness's only HTTP use of `pkg/transcript`/`pkg/transcript/html`/`goldmark` — verify nothing else imports those transitively for the HTTP path (they remain used by CLI/export elsewhere; do NOT remove the transcript packages). `go build ./... && go test -race ./... && make secure`. Commit: `git commit -m "feat(serve): replace pkg/api with runner-supplied pkg/serve; delete pkg/api"`.

---

# PHASE 2 — protocol contract + ephemeral frame + lossless resume + hardening

Phase 2 depends on Phase 1. Each task test-first.

### Task P2-1: `GET /v1/capabilities`

**Files:** `pkg/serve/handlers_capabilities.go` + test.

**Steps (TDD):** static JSON `{"protocol":"looprig.serve","version":1,"features":["journal","live_sse","ephemeral_sse","gate_response"]}` (spec §6). Table tests: 200, exact body, content-type. Not health/auth/tenancy.

### Task P2-2: Ephemeral SSE frame class + `EphemeralFrame` DTO + chunk DTOs

**Files:** `pkg/serve/ephemeral.go` + `ephemeral_test.go`; extend `handlers_events.go`.

**Steps (TDD):** the transport-only encoder (spec §7):

```go
type EphemeralFrame struct {
    V      int             `json:"v"`   // 1
    Kind   string          `json:"kind"` // token_delta|tool_call_started|tool_call_completed|input_queued
    Header event.Header    `json:"header,omitzero"`
    Delta  json.RawMessage `json:"delta,omitempty"`
}
```

For `TokenDelta`, map `content.Chunk` to a tagged live DTO (`{"chunk_type":"text","text":...}` / `"thinking"` / `{"chunk_type":"tool_use","index","id","name","input_json"}`) — **never** serialize `content.Chunk` directly (it has no JSON codec). `ToolCallStarted`/`ToolCallCompleted`/`InputQueued` use their existing public fields in `delta`. Unknown future Ephemeral types are skipped with a debug log (never lossy ad-hoc JSON). The stream loop now emits `event: enduring\ndata: {"v":1,"event":{...}}` for Enduring and `event: ephemeral\ndata: {...EphemeralFrame...}` for Ephemeral. `MarshalEvent`-fails-closed-on-Ephemeral invariant preserved (this is a separate encoder). Table tests: all three chunk DTOs (`text`/`thinking`/`tool_use`); each Ephemeral kind; unknown kind skipped; assert Ephemeral frame carries no `seq`/`id`; assert `content.Chunk` never leaks via reflection.

### Task P2-3: `id: <journal_seq>` stamping on live `enduring` frames

**Files:** extend `handlers_events.go` + test.

**Steps (TDD):** the SSE loop now has `d.JournalSeq` (from Phase 0 WS-B). Stamp `id: <d.JournalSeq>\n` on `enduring` frames; `ephemeral` frames never carry an `id`. Table tests: enduring frame includes `id:` equal to `JournalSeq`; ephemeral frame has none; zero-seq enduring (shouldn't happen, but assert graceful — emit `id: 0` or omit per decision).

### Task P2-4: SSE heartbeats + cache headers

**Files:** extend `handlers_events.go` + test.

**Steps (TDD):** emit a `: ping` comment every 15–30s when idle (a `time.Ticker`, select alongside the events channel and `r.Context().Done()`); set `Cache-Control: no-store` + `X-Accel-Buffering: no` (spec §8). Table tests (deterministic clock/short interval): idle stream emits `: ping` on schedule; a real event resets/does not duplicate; headers present. (Flush timing is integration-tagged.)

### Task P2-5: Client-side join coverage (no server-side fusion)

**Files:** `pkg/serve/join_test.go` (or integration test).

**Steps (TDD):** prove the client-side exact join (spec §7b) over the two primitives serve exposes — subscribe+buffer `enduring` frames, page `/journal` to tip `T`, drop buffered `journal_seq <= T`, follow live — delivers **every** Enduring event exactly once (no loss, no duplication), **including an event appended inside the join window**. `serve` implements no server-side join; this test documents/guards the contract the two endpoints must satisfy. (Marked integration if it needs a real store/live session; otherwise drive with fakes emitting sequenced deliveries + a fake journal.)

### Task P2-6: `Idempotency-Key` on `POST /v1/sessions` (per-pod, TTL, 409 on mismatch)

**Files:** `pkg/serve/idempotency.go` + test; extend `handlers_lifecycle.go`.

**Steps (TDD):** a per-pod in-memory `map[key]{response, bodyHash, expiresAt}` with a TTL (spec §6, Decision #18) — richer than flow's (which has neither TTL nor 409). A repeated key + same body → return the original `{session_id, command_id?}` (do not re-run `Run`); a reused key + different body → `409`. Bounded key length. Table tests: no key → normal create; repeated key same body → same ids, `Run` called once (assert fake runner call count); reused key different body → 409; expired key → new create; oversized key → 400.

### Task P2-7: OpenAPI / JSON-Schema + golden wire fixtures

**Files:** Create `pkg/serve/testdata/openapi.yaml` (or `.json`) + `pkg/serve/testdata/fixtures/*.json` + `pkg/serve/schema_test.go`, `fixtures_test.go`.

**Steps (TDD):** hand-write (stdlib only — no generator dep) the OpenAPI/JSON-Schema for every request/response body (session list page, status, journal page, submit, gate response request, interrupt response, capabilities, error envelope) and the SSE frame schemas (`enduring`/`ephemeral`, required `v`, allowed `kind`s) — spec §7a. Commit **golden JSON/SSE fixtures** beside them. Go tests validate emitted payloads against the fixtures (assert every handler's emitted body byte-equals its golden, modulo volatile ids). Fixture changes are reviewed as wire-contract changes. (TS-side parsing lives in the future client module — out of scope.) Table tests per response type; a `-update` flag pattern to regenerate goldens deliberately.

### Task P2-8: Event-wire fuzz target

**Files:** `pkg/serve/ephemeral_fuzz_test.go` (and/or reuse `pkg/event` fuzz).

**Steps (TDD):** `FuzzEphemeralFrameDecode` — feed arbitrary bytes to the SSE-frame decode path; assert no panic, typed error on malformed, and that a decoded frame never reconstructs a persistable event (Ephemeral never round-trips through the journal). Run `go test -fuzz=FuzzEphemeralFrameDecode ./pkg/serve -fuzztime=30s` in CI-lite; keep the seed corpus in `testdata`.

### Task P2-9: Phase 2 verification + commit

**Steps:** `go test -race ./...`; `go test -tags integration -race ./...` (SSE flush/teardown, loopback-vs-public); `make secure`; commit `feat(serve): protocol contract, ephemeral frames, sequenced resume, hardening`.

---

## Cross-cutting verification (run before every commit)

```bash
CGO_ENABLED=0 go build -trimpath ./...
go test -race ./...
make fmt-check
make secure          # lint (gofmt+vet+staticcheck+gosec) + vuln (go mod verify + govulncheck)
```
Integration-tagged suites explicitly: `go test -tags integration -race ./pkg/serve`. Fuzz smoke: `go test -fuzz=Fuzz... -fuzztime=30s ./pkg/serve`.

## Coordination checklist (spec Decision #16, §7b)

1. Land WS-A + WS-B + WS-C in harness (each green in isolation where possible; the WS-B envelope commit is one atomic breaking change, Task B5).
2. Immediately follow with the sibling-module bump (Task B6): `swe` (`persistence.go` `.Events()` unwrap + `agent.go` `RespondGate`), then `cli` (`go mod tidy && go mod vendor`, `tui/commands.go` `.Events()` unwrap + gate replies). Verify each with `go build ./... && go test -race ./...`.
3. Only then start Phase 1 (`serve` needs the widened `SubscribeEvents`, the `Delivery` envelope, and the catalog status fold).

## Execution notes

- Use a dedicated worktree (the docs workspace is dirty on `main`).
- Keep `pkg/serve` dependency-inverted: production code imports only `pkg/event`, `pkg/gate`, `core/content`, `core/uuid`, and the Reader deps — **never** `pkg/session`, an LLM, or a store. Enforce with a `deps_test.go` guard.
- No new external dependencies (CLAUDE.md). All errors typed; no bare `fmt.Errorf`/`errors.New` from package APIs (sentinels only for context-free leaf errors). Validate every id/query/body at the boundary. `202 Accepted` on gate POST means durable session acceptance, not proven runner consumption.
- Surface (do not silently resolve) the four flagged ambiguities: **A1** per-run ceiling state, **A2** `WithAllowConfigMismatch` placement, **A3** catalog-for-appender wiring, **A4** codec-safe terminal-summary storage in `SessionMeta`.
