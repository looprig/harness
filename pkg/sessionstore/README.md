# pkg/sessionstore

`pkg/sessionstore` is the **session-scoped facade** over a
`storage.Composite` backend. It is the in-tree `SessionJournal`
implementation that a [`pkg/rig`](../rig/README.md) is configured with;
it owns the durable event/command log, the replay-free session catalog,
and the workspace ref → blob offload threshold. Neutral journal, object, and
keyspace persistence delegates to released `github.com/looprig/sessionstore`
v0.1.0; Harness retains its codecs, catalog fold and direct catalog KV
ownership, workspace GC, hustle, and identity semantics.

The storage primitives themselves live in the sibling
[`looprig/storage`](https://github.com/looprig/storage) module; the
concrete backend lives in a sibling storage-provider module. This
package is the **session-shaped adapter** between those generic
primitives and the `pkg/journal` contract.

## What is sessionstore?

- **`Open(b *storage.Composite, opts...) (*Store, error)`** — the
  constructor. Validates the composite (rejects a nil composite or any
  nil primitive — `Ledger`, `Leaser`, `KV`, `OrderedIndex`, `Blobs` —
  fail-closed). `Blobs` must also implement Storage's
  `BlobReaderLifecycle` with a positive close bound; raw `fsstore` deliberately
  does not and is rejected before provider I/O. Open snapshots all five
  interfaces so later caller mutation of the composite cannot split providers.
- **`Store`** — the facade. Holds the assembled `*storage.Composite`
  plus resolved `Options`. Construct it only via `Open`.
- **`SessionJournal`** — `Store` satisfies `pkg/journal.SessionJournal`
  for the session's serialized writer: `Append(ctx, rec) (seq, err)`
  encodes the record, offloads large payloads to `Blobs`, and appends
  the envelope to the session's `Ledger`.
- **`Catalog`** — the replay-free session index. Projected from the
  event stream as events are appended; keyed by session id. Records
  `SessionMeta` (id, title, status, loop count, model, hustle usage,
  timestamps) and per-session derived state. A picker reads it without
  replaying any journal.
- **`Lease`** — the single-writer epoch lease a session acquires on
  open so two processes can't own the same session at once.
- **`Options`** — `WithOffloadThreshold(n)` is the only knob today: the
  payload size (bytes) above which a record is stored as an out-of-line
  blob instead of inline in the ledger. Default 512 KiB; non-positive
  values are ignored. Larger configured values never override the released
  codec's 512 KiB per-body or 1 MiB total-envelope ceilings.

## How to use

A consumer wires a `*sessionstore.Store` into a rig:

```go
import (
    "github.com/looprig/harness/pkg/sessionstore"
    "github.com/looprig/storage"
	// import a backend that implements BlobReaderLifecycle
)

backend, err := productionBackend() // *storage.Composite with all five primitives
if err != nil { return err }

store, err := sessionstore.Open(backend,
    sessionstore.WithOffloadThreshold(1<<20), // requested; effective inline ceiling remains 512 KiB
)
if err != nil { return err }

r, err := rig.Define(
    rig.WithSessionStore(store),
    /* ... */
)
```

`pkg/rig` and `pkg/session` reach the store through the rig; you don't
call `Append` yourself. The `Catalog` is reachable through the same
store for a session picker (a "recent sessions" list, a restore UI).

## Sibling packages

- [`pkg/journal`](../journal/README.md) — the `SessionJournal`
  contract this package implements.
- [`pkg/event`](../event/README.md) — the events the catalog projects.
- [`pkg/hustle`](../hustle/README.md) — hustle usage the catalog
  aggregates.
- [`pkg/workspacestore`](../workspacestore/README.md) — the workspace
  snapshot store wired alongside this one in a rig with a workspace
  placement.
- [`pkg/rig`](../rig/README.md) — `rig.WithSessionStore` takes a
  `*sessionstore.Store`.
- `github.com/looprig/sessionstore` — released neutral session persistence.
- `github.com/looprig/storage` — `Ledger`, `Leaser`, `KV`, `OrderedIndex`,
  `Blobs`, and the optional `BlobReaderLifecycle` capability required here.

## How it is designed

```
       *storage.Composite (looprig/storage)
            │
            │  sessionstore.Open (validates + wraps)
            ▼
       *sessionstore.Store
            │
   ┌────────┬──────────┼──────────────┬──────────────┐
   ▼        ▼          ▼              ▼              ▼
 Ledger   Leaser       KV        OrderedIndex      Blobs
   └────────┴──────────┬──────────────┴──────────────┘
                      ▼
       released sessionstore + Harness facade
```

### Layout

Every session's records share a leading name segment:

- ledger name: `sessions/<uuid>`
- blobs live under: `sessions/<uuid>/blobs/...`
- catalog key: `sessions/<uuid>/catalog`

The layout is the contract between `Open`, `Append`, `Replay`, and the
`Catalog`; it is enforced by the named constants in this package
(`sessionsPrefix`), not by string surgery at the call sites.

### Large-record offload

Each public append can carry two independent bodies: native Harness runtime
bytes and the canonical Core public projection. A private append carries only
the native runtime body. The effective policy offloads a body above the smaller
of the configured threshold and released `MaxInlineBodyBytes` (512 KiB). When
two individually valid public bodies would exceed released `MaxEnvelopeBytes`
(1 MiB including framing), it offloads the larger body, preferring runtime on a
tie. Exact codec limits come from the released API rather than duplicated local
constants. Object publication is verified before the small reference envelope
is appended; failures retain the legacy `*journal.RecordTooLargeError`
classification with a redacted durable cause.

A runtime body larger than replay's 16 MiB declared-size ceiling is refused at
append time with the same `*journal.RecordTooLargeError`, before any object is
published. That guard is load-bearing: only `event.MarshalEvent` caps its own
output — `command.MarshalCommand` and `journal.MarshalGatePreparedRecord` do not
(a gate-prepared body is an event-capped half plus an uncapped payload half plus
JSON framing) — so without it an over-ceiling record could be offloaded and
appended successfully and then be permanently unreadable, bricking restore for
that session. On the read side an over-ceiling DECLARED size is reported as
`*DurableBodyTooLargeError`, never as `*BlobIntegrityError`: it is a size
refusal, and nothing on that path has demonstrated corruption or substitution.

Admission past that 16 MiB ceiling does **not** imply decodability. A nested
8 MiB per-serialized-block cap in Core's `content.UnmarshalBlock` — which
`content.MarshalBlock` does not enforce — bites first: a single text block
serializing to between 8388609 and 16777126 bytes rides inside a command body at
or below the ceiling, so it marshals, passes the append guard, is offloaded,
appends, and is then permanently unreadable on replay with content's
`*BlockLimitError`. That residual is pre-existing, lives in a Core codec rather
than here, and is tracked in [`docs/TODO.md`](../../docs/TODO.md).

### Catalog is derivable

The `Catalog` is a **replay-free** projection. It is best-effort by
construction: `UpdateOnEvent` never returns a non-nil error (the catalog
is derivable, so a failed index is logged and swallowed inside it). A
failed CAS — `storage.KV` has no unconditional Put, every Put is a
revision compare-and-swap — is retried up to `catalogMaxCASRetries`; a
pathologically contended key surfaces a typed `*CatalogConflictError`
rather than spinning forever. `RepairCatalog` rebuilds a catalog from
the journal under its own scan timeout.

### Fail-closed validation

`Open` rejects incomplete composites and delegates the complete five-primitive
and `BlobReaderLifecycle` validation to the released store. Replay and GC use
released envelope magic as an ownership boundary: once that magic is present,
a released decode error is authoritative and cannot fall through to legacy JSON.
Legacy fallback remains only for records without released magic.

### Object reclamation boundary

The compatibility `ObjectGC` reclaims only the old Harness offload shape: a
lowercase SHA-256 leaf directly below `sessions/<uuid>/blobs/`. Released
SessionStore v0.1.0 deliberately exposes no public object enumeration/deletion
API, so Harness does not reconstruct its private physical layout. Released
journal objects and unrelated artifacts, tool results, and checkpoints are
retained until a released retention/reaping contract exists.

`GCResult.Unreclaimable` reports how many keys under the session prefix were
retained for that reason. Read it: a pass over a session holding nothing but
released objects returns `Scanned == Referenced == Deleted == 0` exactly like a
pass over an empty session, and the documented `Scanned == Referenced + Deleted`
identity is vacuously true at `0 == 0 + 0` in both. `Unreclaimable` is what
separates "nothing to reclaim" from "a class this pass cannot see".

**Harness now CREATES unreachable objects on a routine failure path.** This is
not only a deferred retention question. `Append` publishes an over-threshold body
through released `PutObject` BEFORE `storage.AppendDefinite` commits the envelope
that references it, and released `PutObject` mints a random 128-bit generation per
call: the physical key is `<digest>/<generation>`, content-addressed *plus* a
nonce. So retrying the same record after an append failure — a `*storage.ConflictError`
from a fenced-out writer or an ownership handoff, or an ambiguous ack the caller
retries — publishes a NEW object every attempt. Three retries of one record leave
three objects under one digest with a ledger tip that never advanced. Nothing
reclaims them: `ObjectGC` retains every non-legacy key, and released `PutObject`'s
own documentation assigns orphan reclamation to "the store operator", over exactly
this prefix, while noting that its only enumeration path is unexported so "no
caller-facing GC exists yet". Harness is that prefix's sweeper and defers back.

This cannot be fixed here. The generation is drawn from an unexported `Store`
field with no `Option` to supply one, so the retry cannot be made idempotent by
deriving the generation from content, and there is no exported deletion or
enumeration API to reap with. It is tracked in [`docs/TODO.md`](../../docs/TODO.md)
against the released SessionStore APIs it waits on. The pre-refactor legacy
offload keyed on the content SHA alone and so was both idempotent on retry and
reapable by `ObjectGC`; both properties were lost together.

### Durable-append validation is narrower than `event.ValidateEvent`

Every public event is projected through `sessionwire.Project` on the way to the
durable append, and `Project` imposes one rule that `event.ValidateEvent` does
not: an `event.Reply` whose `ReplyTo()` (its `Header.Cause.CommandID`) is zero is
rejected as malformed. The Reply set is not enumerated here: it is sealed by
`event.Reply`'s `isReply` method, `Project` matches on the interface, and
`sessionwire`'s `TestReplyProjectionCasesMatchSealedReplyUnion` derives the union
from `pkg/event` source. Read the members off `event.Reply` (listed in
[`pkg/event/README.md`](../event/README.md)); a hand-maintained copy in this file
could only drift out of the interface, and one did — it named five members while
seven implement `isReply`.

The consequence is worth stating plainly: an event Harness's own validator calls
VALID can be refused at the durable append with a `*journal.MarshalRecordError`.
Every production construction site of these events sets the causing command id,
so no live path emits a zero today — but a new emitter that leaves
`Header.Cause.CommandID` unset will pass `ValidateEvent` and fail the append.
Correlate a reply with the command it answers, or relax `Project`.

### On-disk object durability is not covered by a test

`Open` requires `Blobs` to implement Storage's `BlobReaderLifecycle`, and raw
`fsstore` deliberately does not implement it, so raw `fsstore` can no longer back
a session store at all — the rejection is required and has its own regression.
Harness ships no in-tree provider that satisfies `BlobReaderLifecycle` other than
`memstore`, so `internal/sessionruntime`'s process-services integration test now
composes an on-disk `fsstore` ledger/leaser/KV/index with a `memstore` `Blobs`.
Its "restart" therefore reuses the same in-process blob store.

**Nothing currently watches durability of offloaded session objects across a real
process restart.** A consumer wiring a durable object store (carbon, or any
deployment on rclonestore or another provider) is on its own for that property
until an in-tree provider implements `BlobReaderLifecycle` on disk.
