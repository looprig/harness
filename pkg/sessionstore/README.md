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
