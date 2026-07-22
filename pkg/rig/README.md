# pkg/rig

`pkg/rig` is the **composition root** for an agent runtime. A consumer
assembles a `*Rig` from loop definitions, hustle definitions, a session
store, primers, and a workspace placement; the `Rig` then creates and
restores live sessions.

It owns **design-time topology and lifecycle policy** — what loops exist,
what they're called, which one starts active, where the session is
persisted, where the workspace lives — while the live runtime behavior
lives behind the `pkg/session` contracts the `Rig` returns. Construction and
restoration of a session are owned exclusively here; nothing else in the
module can mint or resume one.

## What is rig?

A `Rig` is an immutable design-time assembly. `rig.Define(opts...)`
validates the options, freezes the assembly, and returns a `*Rig`. The
two lifecycle methods are the whole public surface:

- `rig.NewSession(ctx, opts...)` — bring up a brand-new live session.
  Optionally seed its workspace from a snapshot via `WithSeedSnapshot`.
- `rig.RestoreSession(ctx, id)` — rebuild a prior session from its
  durable journal by id.

Both return a `session.SessionController`, which embeds `session.Session`
(the data plane) and adds the trusted policy/lifecycle methods
(`SetActiveLoop`, `LoopController`, `CheckpointWorkspace`,
`RestoreWorkspace`, `Shutdown`).

## How to use

```go
r, err := rig.Define(
    rig.WithLoops(operatorLoop, reviewerLoop),
    rig.WithPrimers("operator"),
    rig.WithSessionStore(sessionStore),
    rig.WithExclusiveWorkspace(workspaceStore, "/repo", leaser),
    rig.WithHustles(/* ...optional hustle.Definition values... */...),
    rig.WithHustleLimits(rig.HustleLimits{ /* ... */ }),
    rig.WithForeignBuilders(/* ...foreign.Builder for codex/claude... */...),
    rig.WithGateCaps(rig.GateCaps{MaxOpen: 16, MaxTimeout: 30*time.Second}),
    rig.WithDelegationLimits(rig.DelegationLimits{Depth: 4, Quota: 32}),
    rig.WithRestoreDecider(session.DefaultPolicyDecider{}),
)
if err != nil { /* ... */ }

ctx := context.Background()
session, err := r.NewSession(ctx)
if err != nil { /* ... */ }
defer session.Shutdown(ctx)
```

Restore is the same shape with the session id:

```go
session, err := r.RestoreSession(ctx, priorSessionID)
```

## Sibling packages

- [`pkg/loop`](../loop/README.md) — `loop.Definition` values you pass to
  `rig.WithLoops`.
- [`pkg/session`](../session/README.md) — the `Session` /
  `SessionController` interfaces the lifecycle returns.
- [`pkg/sessionstore`](../sessionstore/README.md) — the
  `*sessionstore.Store` you pass to `rig.WithSessionStore`.
- [`pkg/workspacestore`](../workspacestore/README.md) — the
  `*workspacestore.Store` you pass to a workspace placement.
- [`pkg/hustle`](../hustle/README.md) — `hustle.Definition` values you
  pass to `rig.WithHustles`.
- [`pkg/foreign`](../foreign/README.md) — `foreign.Builder` values you
  pass to `rig.WithForeignBuilders` for codex/claude backends.
- [`pkg/gate`](../gate/README.md) — the `gate.Evaluator` you bind into
  each `loop.Definition` via `loop.WithAccessGate`.

## How it is designed

`pkg/rig` is intentionally thin. It validates options, freezes the
assembly, and delegates to the private `internal/sessionruntime`
coordinator, which owns the live loops, hub, journal, and workspace
lifecycle.

```
        rig.Define(opts...)
                │
                │ validate + freeze
                ▼
        *rig.Rig ──► internal/sessionruntime.Lifecycle
                            │
                            │ NewSession / RestoreSession
                            ▼
                     internal/sessionruntime.Session
                       │  │  │  │
                       │  │  │  └──► pkg/workspacestore (workspace snapshots)
                       │  │  └──────► pkg/sessionstore  (journal + catalog)
                       │  └─────────► pkg/hub           (event fan-in)
                       └────────────► internal/loopruntime (loop actors)
                                            │
                                            ▼
                                       pkg/session contracts returned to caller
```

### Validation at the boundary

`Define` enforces the invariants of a valid rig before any session is
created:

- At least one loop and at least one primer; the active primer must be a
  registered loop name.
- Loop names are unique; the active primer is the only one if exactly one
  loop is supplied.
- Every delegate a loop declares is itself a registered loop.
- A `*sessionstore.Store` is required; workspace placement is optional
  but at most one placement may be configured.
- Hustle lane bounds are within `MaxHustleQueued`; gate caps are positive.
- Foreign builders and restore decider are optional with fail-secure
  defaults.

A bad configuration fails closed at `Define` rather than at session
construction.

### Configuration fingerprint

`Define` computes an immutable `InitialFingerprint` for each loop (model,
effective system, tool names) so the rig can stamp and compare
compatibility before any runtime factories execute. At restore time the
rig runs the configured `RestoreDecider` against a `DriftAssessment` and
records the decision as a durable `ConfigurationAdopted`; the default
`DefaultPolicyDecider` accepts only when every change is `Info` and
rejects when any is `Warn`.
