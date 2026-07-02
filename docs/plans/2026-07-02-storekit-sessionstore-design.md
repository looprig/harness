# Design: storekit primitive contracts + session store extraction

**Date:** 2026-07-02
**Status:** Approved (design discussion in session; this doc records the outcome)
**Companion spec:** `2026-07-02-workspacestore-design.md` (workspace store; builds on storekit defined here)

## Problem

looprig's session persistence is welded to NATS. The consumer-facing contracts
(`SessionJournal`, `EventReplayer`/`RecordReplayer`, `JournalRecord`, `Lease`) are already
storage-neutral, but every constructor takes `nats.JetStreamContext`/`nats.ObjectStore`,
`session.Restore` leaks those handles in its signature, and `pkg/persistence` embeds a full
`nats-server` per session directory. Consumers pay the embedded-JetStream dependency tree to
append a few dozen enduring events per session to local disk.

Separately, the sibling project `github.com/ciram-co/flow` (durable graph engine) has its own
append-only store (`CheckpointStore`) with its own NATS module — the same storage primitive,
implemented twice.

## Goals

- looprig core has **zero NATS dependency**. Backends are pluggable modules chosen at the
  composition root (swe).
- One **primitive storage contract** serves both looprig sessions and flow checkpoints, so
  each backend (fs, NATS, Postgres, …) is written once.
- Backend modules import **neither looprig nor flow** — only a tiny leaf contracts module.
- Contracts are **normalized to our invariants**, not to what JetStream happened to impose.
- Full contract, **scoped**: every backend provides ordering, gap-free append, retry-safe
  idempotency, and single-writer fencing — within its stated scope (fsstore fences per host,
  natsstore per cluster). Enforced by a shared conformance suite.

## Non-goals

- Merging flow into looprig (rejected: different layers, different reasons to change).
- Merging the looprig domain facades (sessionstore / workspacestore) into one interface
  (rejected: the honest backend sets barely overlap; see workspace spec).
- OS-level sandboxing, live workspace persistence (see workspace spec).
- Changing looprig's event model, codecs, or replay semantics.

## Architecture: three layers

```
┌─────────────────────────────────────────────────────────────────┐
│ Engines (domain logic, written once per engine)                 │
│  looprig: pkg/sessionstore, pkg/workspacestore                  │
│  flow:    CheckpointStore + future OverLog adapter (flow repo)  │
├─────────────────────────────────────────────────────────────────┤
│ storekit (leaf module: contracts + errors + resolver +          │
│           memstore + storetest) — stdlib only                   │
├─────────────────────────────────────────────────────────────────┤
│ Backends (pure storage engines; import storekit ONLY)           │
│  ciram-co/fsstore · ciram-co/natsstore · ciram-co/pgstore · …   │
└─────────────────────────────────────────────────────────────────┘
```

Why a leaf module instead of pure structural typing: Go interfaces whose methods return other
interfaces (`Read → Cursor`, `Acquire → Lease`) cannot be satisfied structurally across
packages — signatures must match exactly. `storekit` is the one shared vocabulary; it is
stdlib-only, a few hundred lines, and changes rarely. Backends never see an engine type;
engine releases never touch backends. looprig and flow still never import each other.

### Module map

| Module | Contents | Imports |
|---|---|---|
| `github.com/ciram-co/storekit` | contracts (`Ledger`, `Leaser`, `KV`, `Blobs`), typed errors, `AppendDefinite`, `memstore`, `storetest` | stdlib only |
| `github.com/ciram-co/looprig` | `pkg/sessionstore` (domain facade), existing `pkg/journal` record types/codecs/appenders/interfaces | + storekit |
| `github.com/ciram-co/fsstore` | all four primitives on local disk | stdlib + storekit |
| `github.com/ciram-co/natsstore` | all four primitives on JetStream; embedded-server option (absorbs `pkg/persistence`) | storekit + nats.go + nats-server |
| `github.com/ciram-co/pgstore` (future) | `Ledger`/`Leaser`/`KV` on Postgres | storekit + pg driver |
| `github.com/ciram-co/rclonestore` (workspace spec) | `Blobs` via exec'd rclone binary | stdlib + storekit |

Backend-first packaging: one module per technology, implementing whichever primitives it can
honestly pass conformance for. Repo names carry no `looprig-` prefix and no contract prefix.

## storekit contracts

### Ledger — the shared primitive

A **ledger** is a named, totally-ordered, gap-free, append-only sequence of records.
looprig sessions and flow checkpoints are both ledgers. (Name follows Apache BookKeeper,
whose fenced append-only primitive is called exactly this; `Log` was rejected for its
collision with `log`/`slog`.)

```go
package storekit

// Ledger addresses many ledgers by name. Append commits payload as the record
// immediately after sequence `expected` (CAS on the tip; expected == 0 means the
// ledger must be empty). The committed record's seq is expected+1 by definition,
// so Append returns no sequence. Sequences are 1-based, contiguous, immutable.
type Ledger interface {
	Append(ctx context.Context, name string, expected uint64, payload []byte) error
	Read(ctx context.Context, name string, from uint64) (Cursor, error)
	Tip(ctx context.Context, name string) (uint64, error)
	Delete(ctx context.Context, name string) error
}

type Record struct {
	Seq     uint64
	Payload []byte
}

type Cursor interface {
	Next(ctx context.Context) (Record, error) // io.EOF when drained
	Close() error
}
```

Append outcomes are a tri-state:

- `nil` — committed, definitely.
- `*ConflictError` — something already occupies expected+1. Definite: the record did not land.
- `*AmbiguousError` — the outcome is unknown (lost ack / lost COMMIT response). Only
  networked backends may return this; fs and memory never do.

Any other error is a definite failure (fail closed, tip unadvanced).

### AppendDefinite — ambiguity resolution, written once

The retry-then-verify algorithm currently buried in looprig's NATS journal
(`resolveAmbiguous`/`reconcileTip`) is hoisted into one generic function:

```go
// AppendDefinite turns any Append into a definite outcome. On AmbiguousError it
// retries the identical append once; on conflict (from either attempt) it reads
// the record at expected+1 and byte-compares: equal payload means the original
// landed (success); a foreign payload means this writer has been fenced
// (ConflictError). A second ambiguous outcome surfaces AmbiguousError unresolved.
func AppendDefinite(ctx context.Context, l Ledger, name string, expected uint64, payload []byte) error
```

This is correct for every networked backend with the same failure shape (JetStream lost ack,
Postgres lost COMMIT). Backends stay dumb; only they know which of their errors are
ambiguous, so their sole obligation is honest classification.

### Leaser — ownership with liveness

```go
// Leaser grants exclusive, epoch-fenced ownership of a name. Acquire fails with
// *LeaseHeldError while a live holder exists; a dead holder's lease is reclaimed
// by the backend's native mechanism (flock released by the OS, KV TTL expiry,
// PG advisory-lock session end). Epochs are strictly increasing across grants
// of the same name.
type Leaser interface {
	Acquire(ctx context.Context, name string) (Lease, error)
}

type Lease interface {
	Epoch() uint64
	Lost() <-chan struct{} // closed when ownership is lost (expiry, takeover)
	Release() error
}
```

The lease is the **fast-path** guard. The **hard** guard is the opening-fence discipline in
the engine layer (below): a new owner appends a fence record before anything else, advancing
the tip so every stale writer's CAS append conflicts. This preserves today's design exactly.

### KV and Blobs

```go
// KV holds small CAS'd metadata (the session catalog). Revisions are per-key,
// strictly increasing; Put with expectedRev 0 requires the key to be absent.
type KV interface {
	Get(ctx context.Context, key string) (val []byte, rev uint64, err error)
	Put(ctx context.Context, key string, expectedRev uint64, val []byte) (rev uint64, err error)
	Keys(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// Blobs holds bulk immutable bytes (large-record offload; workspace snapshots).
// Put streams; keys are content-addressed by callers.
type Blobs interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}
```

### Name and key grammar (normalization)

Ledger names, KV keys, and blob keys match `[a-z0-9][a-z0-9/_.-]*`, max 512 bytes, no `..`
segment. This is *our* grammar; backends escape into their native namespaces (JetStream
subject tokens, file paths) internally. NATS subject rules never constrain the contract.
The restricted charset also removes argv/path-injection surface in exec- and fs-backed
implementations.

### Payload floor and offload policy (normalization)

Every backend must accept ledger payloads and KV values up to **1 MiB**. Larger payloads are
the engine's responsibility to offload to Blobs. The *threshold* at which sessionstore
offloads (default **256 KiB**, configurable) is engine policy, not contract — JetStream's
message cap stops being an architectural constant.

### Typed errors

`ConflictError{Name, Expected}`, `AmbiguousError{Name, Expected, Cause}`,
`LedgerNotFoundError{Name}`, `RecordNotFoundError{Name, Seq}`, `KeyNotFoundError{Key}`,
`BlobNotFoundError{Key}`, `LeaseHeldError{Name, HolderEpoch}`, `LeaseLostError{Name, Epoch}`,
`InvalidNameError{Name, Rule}`. All concrete structs in storekit; backends return them
(wrapping causes). Engines classify with `errors.As` only — never by string.

### memstore and storetest (in storekit)

- `storekit/memstore`: a **real** implementation of all four primitives (CAS, epochs,
  ordering, conflict semantics — not a toy map). It is the reference implementation, the
  test double for both engines' consumers, and a legitimate choice for deliberately
  ephemeral runs. It persists nothing across restarts, by design and by name.
- `storekit/storetest`: the conformance suite, driven by a factory
  `func(t *testing.T) (backend, cleanup)`. Every backend must pass the suites for the
  primitives it claims. Coverage includes: append/read round-trip equality; contiguity and
  1-based sequencing; CAS conflict on wrong expected; expected==0 semantics; concurrent
  appender linearization; stale-writer-fenced-after-opening-fence; lease epoch monotonicity;
  LeaseHeldError while held; reclaim after holder death (where testable); KV rev CAS; blob
  round-trip and absence errors; name-grammar rejection; payload floor acceptance.

## looprig: pkg/sessionstore (domain facade)

What stays in looprig core, written once over any backend: record envelope + codecs,
opening-fence discipline, lease policy, dedup discipline, offload policy, catalog schema,
replay decode.

```go
package sessionstore

// Backend is what sessions require of a storage backend. Satisfied by
// fsstore.Store, natsstore.Store, memstore.Store, ...
type Backend interface {
	storekit.Ledger
	storekit.Leaser
	storekit.KV
	storekit.Blobs
}

func Open(b Backend, opts ...Option) (*Store, error)

// Store is the typed facade the harness and composition root use.
// It returns the existing journal interfaces — consumer code is unchanged.
func (s *Store) AcquireLease(ctx context.Context, id uuid.UUID) (journal.Lease, error)
func (s *Store) OpenJournal(ctx context.Context, id uuid.UUID, l journal.Lease) (journal.SessionJournal, error)
func (s *Store) OpenEventReplayer(id uuid.UUID) (journal.EventReplayer, error)
func (s *Store) OpenRecordReplayer(id uuid.UUID) (journal.RecordReplayer, error)
func (s *Store) Catalog() Catalog
```

Lifecycle: `Store` has no `Close`; the composition root owns the backend's lifecycle
(backends expose `Close` on their concrete types, outside the storekit contracts).

### Record envelope (normalization)

Today a session's records are routed to per-kind JetStream **subjects** (event / command /
fence) within one stream. Subjects were NATS routing; the normalized design uses **one
ledger per session** (`sessions/<uuid>`) and moves kind into a versioned envelope:

```
envelope v1 (JSON): {"v":1, "kind":"event"|"command"|"fence"|"blobptr", "id":"<idempotency-id>", "payload":<codec bytes>}
```

- `id` is the record's domain idempotency ID (formerly the `Nats-Msg-Id`). It is domain
  data now, not a transport header: replay/restore logic uses it to recognize
  already-appended records; the primitive layer never sees it.
- `blobptr` carries `{key, size, sha256}` for offloaded records: bytes are Put to Blobs
  (content-addressed) **before** the pointer is appended — upload-before-append, no dangling
  references, exactly today's discipline.
- The exact wire format (JSON vs length-prefixed) is pinned in the implementation plan; it
  must be versioned from day one.

### Write path

`OpenJournal` (holding a valid lease) appends the opening fence record
(`fence{epoch}`) via `AppendDefinite` before returning — taking ownership of the tip.
`Append` then, under the journal's single-writer mutex: fast-path lease check
(`Valid()` + `Lost()`), marshal, offload-if-over-threshold, `AppendDefinite` at the tracked
tip, advance the tip. Per-append deadline independent of the session context is preserved.
All of this is today's semantics with `nats.PublishMsg` swapped for `storekit`.

### Read path, catalog

Replay opens a `Cursor` from a position, decodes envelopes, resolves `blobptr` records via
`Blobs.Get`, and surfaces the existing `EventReplayer`/`RecordReplayer` semantics unchanged.
The catalog lives in KV under `sessions/<uuid>` (metadata JSON, CAS-updated); `Keys` +
`Get` provide listing. GC of offloaded blobs mirrors today's ObjectGC: blobs referenced by
live ledger records are reachable; sweep the rest.

### API change outside storage

Exactly one: `session.Restore(ctx, cfg, id, js, objectStore, leases)` becomes
`session.Restore(ctx, cfg, id, store *sessionstore.Store)`. No other non-test package
imports NATS today.

## flow (informative, not in scope)

flow keeps its `CheckpointStore` contract and its `MemStore`. After normalization, flow's
checkpoint persistence over `storekit.Ledger` is a ~100-line adapter (`Append` → marshal +
CAS append at revision; `Latest` → `Read(Tip)`; `History` → `Read(1)`). Recommended path:
an optional `OverLog`-style adapter package in the flow repo importing storekit; flow's
existing `pkg/nats` module keeps working until flow chooses to adopt shared backends. No
forced churn; direction of dependency is flow → storekit, never flow ↔ looprig.

## Backends

### fsstore (`ciram-co/fsstore`) — stdlib + storekit only

```
<root>/
  streams/<name>.log     ledger: length-prefixed records [len u32][crc32c u32][seq u64][payload]
  leases/<name>.lock     flock + epoch counter file
  kv/<key>               write-temp-then-rename; rev in a small header
  blobs/<key>            content-addressed files (write-temp-then-rename)
```

- Append: under the ledger's flock'd writer, validate expected == tip, write frame, fsync
  (per-append by default; cadence configurable). Never returns `AmbiguousError`.
- Open: scan; a torn tail frame (bad length/CRC) is truncated — crash consistency.
- Lease: flock (`syscall.Flock`, build-tagged as `pkg/persistence` does today) + epoch file.
  Scope: **per host**. A dead process's flock is released by the OS.
- `Options{Root string}` — required; the importer decides the location. No default path
  in any library (the current `~/.looprig/jetstream` default moves to consumers).
- Fuzz targets: frame parser and torn-tail recovery.

### natsstore (`ciram-co/natsstore`)

Receives the current NATS machinery as an implementation of the four primitives:

- Ledger → one JetStream stream per ledger; `Append` maps to publish with
  `ExpectLastSequence`; wrong-last-sequence → `ConflictError`; timeout/no-response →
  `AmbiguousError` (resolution is `AppendDefinite`'s job now — the in-journal
  resolve/reconcile code is deleted, not moved).
- Leaser → KV bucket with TTL + heartbeat + watch (today's LeaseManager, ported).
- KV → JetStream KV; Blobs → JetStream ObjectStore.
- Ledger/key names escaped into subject-safe tokens internally.
- `pkg/persistence` (embedded no-TCP server, `EngineOptions`, flock'd session dirs) moves
  here as the embedded deployment option: `natsstore.Open(natsstore.Options{URL: ...})` or
  `{EmbeddedDir: ...}`. Scope: **per cluster** (embedded: per host).
- The `nats-io` dependency approvals move from looprig's CLAUDE.md to this module's.

### pgstore (future, named for completeness)

Transactions give CAS trivially; advisory locks are native leases. No Blobs (bulk bytes do
not belong in Postgres; pair with rclonestore for workspaces).

## Composition (swe)

```go
fs, err := fsstore.Open(fsstore.Options{Root: filepath.Join(sweData, "store")})
// swap: natsstore.Open(natsstore.Options{URL: os.Getenv("NATS_URL")}) — nothing below changes

sessions, err := sessionstore.Open(fs)
lease, err := sessions.AcquireLease(ctx, id)
jrnl,  err := sessions.OpenJournal(ctx, id, lease)
sess,  err := session.Restore(ctx, cfg, id, sessions)
```

Harness options take the stores as separate fields (`Sessions`, `Workspaces`) — no bundle
type (YAGNI).

## Error handling

- Backends return storekit typed errors wrapping their causes; engines classify via
  `errors.As` exclusively.
- Fail closed everywhere: unresolved ambiguity, lease loss, torn frames, envelope-version
  mismatch all stop the writer with typed errors; nothing is silently skipped or re-inlined.
- looprig's existing journal error types (`JournalNotReadyError`, `JournalLeaseLostError`,
  `AppendError`, …) remain the domain-level surface, now caused-by storekit errors.

## Testing

- storetest conformance (above) — memstore, fsstore, natsstore (natsstore's runs under
  `//go:build integration` with the embedded server).
- looprig's existing journal/replay/restore correctness tests are retained and re-pointed at
  memstore — they are what makes rewriting the write path safe.
- Table-driven throughout per CLAUDE.md; `-race` always; fuzz targets for fsstore framing
  and the envelope decoder.

## Migration phases (detail in the implementation plan)

- **A. storekit**: contracts, errors, `AppendDefinite`, memstore, storetest. Ships first;
  everything else depends on it.
- **B. looprig**: add `pkg/sessionstore`; rewrite journal write/read internals over storekit;
  swap `session.Restore` signature; delete `pkg/journal`'s NATS files and `pkg/persistence`;
  drop both `nats-io` deps from go.mod; amend CLAUDE.md's approved-deps list.
- **C. fsstore**: fresh, conformance-driven. Parallel with B after A.
- **D. natsstore**: port machinery + embedded engine; conformance + integration tests.
- **E. swe**: wire fsstore as its default at the composition root.

## Future work

- `pgstore`; a `sessionstore-s3`-style backend on native S3 conditional puts (never via
  rclone — it lacks CAS and would fail conformance).
- flow `OverLog` adapter in the flow repo; eventual retirement of `flow/pkg/nats` in favor
  of natsstore (flow's call).
- An archive decorator (wrap a local backend, ship the store dir to a remote at
  quiescence/close) for scale-to-zero sandboxes — see the workspace spec's deployment
  profiles.

## Decision log (from design discussion, 2026-07-02)

1. NATS optional → NATS is a backend; fs is the practical default; memory ships in storekit.
2. Full contract, scoped (fencing per host vs per cluster), enforced by conformance tests.
3. Backend-first module packaging; no `looprig-` prefix; repos `ciram-co/<backend>store`.
4. Backends import only storekit (leaf module) — never looprig or flow.
5. Contracts normalized away from NATS: no MsgID/dedup-window, no assigned-seq return,
   neutral name grammar, payload floor + engine offload policy, explicit Delete, tri-state
   append outcomes with a shared resolver.
6. sessionstore and flow's checkpoint store unify **at the primitive** (`Ledger`), not at
   the domain contract; looprig keeps two domain facades (sessions, workspaces).
7. Primitive named `Ledger` (BookKeeper precedent), not `Log`.
