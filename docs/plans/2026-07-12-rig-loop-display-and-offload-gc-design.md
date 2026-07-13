# Rig loop display metadata and session offload GC — design amendment (2026-07-12)

This amendment pins two contracts that the SWE-harness rig migration relies on. Both
are owned by the harness runtime (rig + sessionruntime); the SWE consumer configures
them but implements neither.

1. **Per-loop display metadata** — already implemented (tip commit
   `feat(loop): carry per-loop display metadata on LoopStarted`). Documented here so the
   contract is fixed; no code changes accompany this section.
2. **Session offload GC policy** — the new work this amendment introduces.

---

## 1. Per-loop display metadata (contract, already implemented)

This section documents the display-metadata contract **as it actually exists in the
committed code** (tip commit `feat(loop): carry per-loop display metadata on
LoopStarted`). Where the migration plan's prose described behavior that is **not yet
present**, that is called out explicitly under "Scope of what is committed" below —
documented, not silently assumed.

### Definition-time options

- `loop.WithDisplayName(name string) loop.Option` and
  `loop.WithDescription(desc string) loop.Option` attach human-facing presentation
  metadata to a loop definition. Each is an **at-most-once** definition option
  (`DefinitionDuplicateOption` on a second set).
- The values are **presentation-only strings** carried on the immutable resolved
  `Definition` and exposed through the bound-definition accessors `DisplayName()` and
  `Description()`; the definition is value-copied at `Define`, so a caller cannot perturb
  a bound loop after the fact.
- **They are deliberately EXCLUDED from `PolicyRevision`** (and from the config
  fingerprint): two definitions differing only in display name/description produce the
  **same** `PolicyRevision`, so relabeling a loop is **restore-compatible** and never
  trips the fingerprint mismatch guard. (Invariant pinned by
  `TestDisplayMetadataDoesNotAffectPolicyRevision`.) This is the intended design — display
  metadata is a label, not part of a session's behavioral identity.

### Event carrier

- `event.LoopStarted` carries `DisplayName` and `Description` (both `json:",omitzero"`)
  alongside the existing `AgentName`.
- They are stamped from the bound definition's accessors at the single loop-creation
  chokepoint (`session.newLoopWithAdmission`), so both freshly spawned and restored loops
  (which re-bind their durable definition) present the same display metadata.
- The **agent name remains the topology key** — the identity used to map a durable
  `LoopStarted` back to its definition, to enforce delegate-target allow-lists, and to
  drive restore discovery. `DisplayName` is presentation only and never a lookup key.

### Scope of what is committed (and what is NOT)

The migration plan described a managed-`Subagent`-catalog contract on top of display
metadata — rendering delegate targets by display name/description, resolving a selected
**display name** back to the allowed definition key, display-alias uniqueness among one
parent's allowed delegate targets, and a primer/`operator`-leaf exemption. **As of this
amendment none of that catalog-level behavior exists in the committed code.** The
committed display-metadata surface stops at: the two definition options, the bound
accessors, and the two `LoopStarted` fields. The `Subagent` tool's catalog today keys and
renders delegate targets by **agent name** (plus its own descriptor `Description`), and
selection resolves an **agent name**, not a display alias. Wiring display names/aliases
into the catalog (and any alias-uniqueness rule) is **future work, not part of the
committed contract** this amendment pins.

---

## 2. Session offload GC policy (new)

### Rig-facing API

```go
rig.WithOffloadGC(rig.OffloadGCPolicy{
    Interval: 5 * time.Minute, // cadence between GC passes
    Timeout:  time.Minute,     // per-pass deadline
})
```

- `rig.OffloadGCPolicy` is a typed struct (`Interval`, `Timeout time.Duration`).
- `rig.WithOffloadGC` is an at-most-once rig `Option`. It **validates** both fields:
  a non-positive `Interval` fails with `*rig.InvalidOffloadGCIntervalError`; a
  non-positive `Timeout` fails with `*rig.InvalidOffloadGCTimeoutError`. No bare
  `errors.New` crosses the package API.
- A validated policy compiles to a `sessionruntime.WithLifecycleOffloadGC` lifecycle
  option carrying the equivalent `sessionruntime.OffloadGCPolicy`.

### What it wires

`sessionstore.OpenObjectGC(id, lease)` reaps **orphaned offload blobs** for one session
— content-addressed blobs left durable with no in-ledger `blobptr` pointer (the
crash gap of the writer's blob-durable-before-pointer discipline). See
`pkg/sessionstore/gc.go`. This is **session offload GC only** — never workspace-snapshot
GC.

Both **new** and **restored** rig sessions run a GC pass periodically, **only while the
session lease is held**, wired at the composition root:

- **New:** `Lifecycle.NewSession` (`internal/sessionruntime/lifecycle.go`) after it
  acquires the lease and opens the journal.
- **Restored:** `restoreTopologySession`
  (`internal/sessionruntime/restore_constructor.go`) after it acquires the lease and
  opens the journal.

The GC runner (`internal/sessionruntime/offload_gc.go`,
`offloadGCRunner`) depends only on the narrowest seams (Dependency Inversion / Interface
Segregation), all injected at the composition root:

- an `offloadScanner` (`GC(ctx) (sessionstore.GCResult, error)`), satisfied by
  `*sessionstore.ObjectGC`;
- the reader/writer **journal-admission gate** (see below), whose **writer** the runner
  acquires around each pass;
- an `idle func() bool` — native `SessionIdle` (from `hub.IsIdle`);
- the lease-loss channel (`lease.Lost()`) — **lease loss cancels the GC**;
- a **manual tick seam** (`offloadGCTicker`) so tests drive ticks deterministically with
  no wall-clock timers and no `time.Sleep`. Production wraps `time.NewTicker(Interval)`;
  tests inject a manual ticker.

### The journal-admission gate (serialization — load-bearing)

`ObjectGC.GC` MUST NOT scan/delete while an offload's pointer append is in flight: a blob
whose `blobptr` append has not yet landed would be observed as unreferenced and wrongly
reaped. `pkg/sessionstore/gc.go` documents that this serialization — not any grace
window — is GC's entire concurrency safety (storage's `Blobs.List` surfaces no ModTime).

Every durable append for a session funnels through the **single**
`journal.SessionJournal.Append` on the one `j` returned by `store.OpenJournal`: the hub's
event tap, the command intent log (`session.appendCommand*`), the gate-record appends
(`GatePreparedRecord` / `GateOpened` / `GateResolved`), the lease fence, and the restore
lifecycle events are all appenders built over that same `j`. The over-threshold **offload**
(blob `Put` then the `blobptr` `AppendDefinite`) happens entirely inside that one call
(`pkg/sessionstore/journal.go`).

So the gate is placed as a **single decorator over `journal.SessionJournal`**
(`gatedJournal`) wrapping `j` at the composition root **before any appender is built over
it**:

- every `Append` acquires the gate as a **reader** for the whole delegated call — which
  spans the blob `Put` **and** the pointer `AppendDefinite`, so no GC can interleave
  between them;
- the GC runner acquires the gate as the **writer** around each pass, and only **after**
  the session has reached native `SessionIdle`.

Reader/writer exclusion (a `sync.RWMutex`) is the barrier: a pending or held writer (GC)
blocks while any reader (append/offload) is in flight, and vice versa. This is proven by
two forced-barrier tests: (a) an offload blocked **before** its pointer append proves GC
cannot scan/delete until that append completes; (b) a forced **gate-record** offload
proves the same barrier holds for gate records (the decorator is record-kind agnostic, so
one gate covers event, command, and gate-record appends alike).

### Shutdown ordering and lease loss

- On clean `Shutdown`, the runner goroutine is **stopped and joined BEFORE**
  `SessionStopped` is appended (`hub.StopSession`) and **BEFORE** the lease is released —
  wired as the first action of Shutdown's innermost (last-registered, first-run) deferred
  teardown, ahead of the root-lease and session-lease release defers.
- **Lease loss cancels the GC:** the runner selects on `lease.Lost()` and returns,
  cancelling any in-flight pass; `ObjectGC` additionally re-checks the lease and fails
  closed, so a lost writer never deletes.

A session configured without `WithOffloadGC` wires no gate and no runner — the journal is
used undecorated and behavior is unchanged.
