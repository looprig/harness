# Design: workspace store (durable workspace snapshots for suspend/resume)

**Date:** 2026-07-02
**Status:** Approved (design discussion in session; this doc records the outcome)
**Depends on:** `2026-07-02-storekit-sessionstore-design.md` (uses `storekit.Blobs`)

## Problem

When the looprig harness runs in the cloud (container, microVM, scale-to-zero sandbox), an
agent's working files must survive the compute. Today nothing persists working files:
`pkg/tools` confines tools to a `WorkspaceRoot` path on the real filesystem, and that is
all. If the agent asks the user a question and the process dies, the files are gone; resume
on another host has no workspace to stand on.

The design must also draw a line that kept blurring in discussion: what is the difference
between storing a *session* and storing a *workspace*?

## The line: three concepts, not two

1. **Session store** persists *what happened* — small, ordered, fenced, append-only events.
   A log. (Previous spec.)
2. **Workspace store** persists *what the files were at named moments* — big, immutable,
   content-addressed snapshots. A blob tree. (This spec.)
3. **The live workspace** is neither — the real local directory tools run in (the
   `WorkspaceRoot` the permission layer already confines). Ephemeral scratch, always local
   disk, never abstracted. This spec deliberately gives it no interface.

They differ on every axis that matters for storage (size, mutability, ordering, access
pattern, consistency), so they are **two contracts** — but they **link through the
journal**: a snapshot ref is recorded as an enduring event, giving checkpoints a total
order and making the journal the complete resume token.

## Goals

- A minimal snapshot/materialize contract in looprig core; backends supply only bulk bytes
  (`storekit.Blobs`).
- Deterministic, content-addressed snapshots ordered by the session journal
  (upload-before-append; no dangling refs).
- Resume on any host: journal replay → last checkpoint ref → materialize → continue.
- Runs on the lowest common denominator of cloud runtimes: scratch disk + outbound network.
  No privileged mounts, no volume plugins.

## Non-goals

- Incremental / manifest-per-file snapshots (future optimization; the versioned ref format
  leaves room).
- Live/continuous sync of the working directory; mounted-filesystem abstractions (a mount
  may exist as a *cache*, never as truth — see profiles).
- Linking rclone as a library (exec only).
- VM/memory snapshotting (a platform concern, orthogonal to this design).

## Contract: pkg/workspacestore (looprig core)

The domain logic — walk, tar, hash, validate — lives in core, written once. A backend
provides only `storekit.Blobs`.

```go
package workspacestore

func Open(b storekit.Blobs, opts ...Option) (*Store, error)

// Ref names one immutable snapshot, content-addressed and versioned.
// Format v1: "v1:sha256:<hex>". Opaque to callers.
type Ref string

// Snapshot captures the tree rooted at root and returns its ref.
// Identical trees yield identical refs (deterministic archive), so an
// unchanged workspace re-snapshots into a no-op upload.
func (s *Store) Snapshot(ctx context.Context, root string) (Ref, error)

// Materialize restores ref into dest. An empty dest is the truth path (extract).
// A non-empty dest is never trusted and never wiped: Materialize re-archives it
// deterministically and compares digests — a match (a warm volume holding exactly
// ref) is a verified no-op; a mismatch returns *DestNotEmptyError and the caller
// decides whether to clear and retry. No marker files: reuse is proven by
// content, since markers go stale the moment a resumed turn mutates the tree.
func (s *Store) Materialize(ctx context.Context, ref Ref, dest string) error

// Delete removes a snapshot's blobs. Used only by GC (below).
func (s *Store) Delete(ctx context.Context, ref Ref) error
```

### Snapshot format v1

- One deterministic `tar.gz` per snapshot (stdlib `archive/tar` + `compress/gzip`):
  entries sorted by path; mtimes zeroed; uid/gid cleared; mode preserved (the executable
  bit matters); symlinks stored as symlinks.
- Streamed: tree → tar → gzip → sha256 tee → `Blobs.Put` under key
  `workspaces/<sha256-hex>` (spooled via a temp file so the digest names the key before
  upload completes; the working set never needs to fit in memory).
- `Ref = "v1:sha256:<hex>"`. The version prefix is the evolution seam — a future
  incremental format becomes `v2:` without breaking stored history.
- Determinism is also what makes the verified warm-volume fast path sound: re-archiving an
  unchanged tree reproduces the digest bit-for-bit, so reuse is proven by content — never
  by a marker or timestamp that can go stale after a crashed turn mutates the tree.

### Journal linkage (the resume token)

A new **enduring** event in looprig's event set:

```go
// WorkspaceCheckpointed records that the session's workspace was durably
// snapshotted as Ref at this point in the event order.
type WorkspaceCheckpointed struct{ Ref workspacestore.Ref }
```

Discipline (same as large-record offload): **snapshot before append**. The harness, at a
quiescence point (agent asked the user a question; turn done; about to suspend):

1. `ref := ws.Snapshot(ctx, root)` — bytes are durable first;
2. append `WorkspaceCheckpointed{ref}` to the session journal;
3. the process may now die.

Resume (any host): open session store → replay (folds state and yields the **last**
`WorkspaceCheckpointed`) → `ws.Materialize(ref, freshRoot)` → deliver the pending user
reply → continue. Crash between 1 and 2 leaks an unreferenced blob (GC's job), never a
dangling ref. The session store's single-writer fencing decides which of two concurrent
resumes may append; the loser has its own materialized copy and corrupts nothing.

### GC

Mark-and-sweep at the composition root, mirroring the journal's blob GC: refs reachable
from any live session's `WorkspaceCheckpointed` events are live; unreferenced snapshot
blobs older than a policy age are deleted via `Store.Delete`. Content-addressing makes
this safe against races with in-flight snapshots of identical trees (a re-Put of the same
key is a no-op).

## Runtime model: snapshots are truth, mounts are cache

The container/VM is a CPU with scratch disk; everything precious leaves through a store
interface before the compute can die. The **mount model** (workspace on NFS/EFS/FUSE-S3)
was considered and rejected as the primary:

1. Agents hammer the filesystem (builds, git, package installs) — network filesystems are
   slow and semantically leaky. Tools touch local disk only; this contract deliberately has
   no per-file I/O methods.
2. A mount cannot align with the journal: it is one mutable "whatever is there now," with
   no way back to the state at the moment the agent asked its question. A crash
   mid-`npm install` has no good restore point. Snapshot refs in the event order do.
3. Fencing extends to files with snapshots (each resume materializes its own copy); mounts
   let concurrent resumes stomp each other.

A mounted persistent volume remains useful as a **cache**: Materialize's verified fast
path (local re-archive + digest compare — CPU and local I/O, no network fetch) makes a
warm volume holding exactly the checkpointed tree a no-op resume, while a drifted tree —
mutated by a turn that crashed after the last checkpoint — fails closed with
`*DestNotEmptyError` instead of silently resurrecting unjournaled files. The design does
not know mounts exist.

## Deployment profiles

| | Session store | Workspace store | Global fencing |
|---|---|---|---|
| **Laptop** | fsstore | fsstore (Blobs) | flock (single host is the world) |
| **Fleet service** | natsstore / pgstore → shared infra | rclonestore → object store | the session store (cluster CAS) |
| **Scale-to-zero sandbox** (one microVM per session) | fsstore local, archived at quiescence | fsstore local, archived same step | the orchestrator (one live sandbox per session); flock inside |

- **Fleet** is the canonical cloud shape and this design's target: every append crosses the
  network as it happens; the container may be killed at any instant; the lost in-flight
  turn is recovered by replay. A pgstore-backed session store takes its offload Blobs from
  the same object-store backend used here, assembled via `storekit.Composite`.
- **Scale-to-zero** is the "everything local, sync outward" composition: legitimate,
  provided the platform kills only after the archive step (these platforms suspend between
  turns by construction) and with fencing honestly delegated to the control plane. The
  archive decorator that ships a local store dir to a remote at quiescence is named future
  work in the sessionstore spec.
- The two stores stay two interfaces in every profile; what varies is whether their
  *backings* coincide.

## Backends

| Backend | Provides workspace storage? | Notes |
|---|---|---|
| `storekit/memstore` | ✓ | tests / ephemeral only |
| `ciram-co/fsstore` | ✓ | blobs under `<root>/blobs/`; laptop default |
| `ciram-co/natsstore` | ✓ | JetStream ObjectStore; suits small/medium workspaces in NATS-only deployments |
| `ciram-co/rclonestore` | ✓ (Blobs **only**) | the cloud workhorse — any of rclone's ~70 remotes |
| `ciram-co/pgstore` | ✗ | bulk bytes do not belong in Postgres; a pgstore-backed deployment takes Blobs from an object-store backend via `storekit.Composite` (companion spec), covering both session offload and workspaces |

rclone cannot be a session store (no CAS → cannot pass ledger conformance); Postgres is a
poor workspace store. Each backend lands on the side of the line its semantics support —
this asymmetry is why the contracts stay separate.

### rclonestore (`ciram-co/rclonestore`)

Implements `storekit.Blobs` by **exec'ing the rclone binary** — never linking librclone
(a giant dependency tree would defeat the point of the extraction).

- `Put` → `rclone rcat <remote>:<prefix>/<key>` (streams stdin); `Get` → `rclone cat`
  (streams stdout); `Delete` → `rclone deletefile`; `List` → `rclone lsf`.
- `Options{Remote string, Prefix string, Binary string /* default "rclone", resolved via
  exec.LookPath */, ConfigPath string, Timeout time.Duration}`.
- Security (per CLAUDE.md): argv exec only, no shell; `--` before positional arguments;
  storekit's canonical segment grammar (single `/` separators, no empty/`.`/`..` segments,
  no leading or trailing `/`) plus a validated remote name means no argument can be
  mistaken for a flag; every call context-bounded; rclone config (credentials) is
  referenced by path, never parsed, logged, or copied.
- Startup check: binary present and executable, remote resolvable (`rclone lsf --max-depth 0`
  probe) — fail loudly at Open, per fail-secure.

## Security

- **Materialize is the trust boundary**: archive entries are untrusted input. Every entry
  name is `filepath.Clean`'d and verified to stay under dest (reject absolute paths and
  `..` — zip-slip); symlinks are restored with `os.Symlink` without following, and entry
  *names* are still containment-checked; entry count/total-size limits guard decompression
  bombs; device/fifo/hardlink entries are rejected. Fuzz target on entry validation.
- Snapshot refuses roots outside the caller-supplied path after `filepath.Clean` +
  symlink-escape checks (same guards `pkg/tools` uses for `WorkspaceRoot`).
- Refs are validated (`v1:sha256:<64 hex>`) before deriving blob keys.
- No credentials in errors or logs (rclone remotes may embed tokens in config — only the
  remote *name* ever appears in messages).

## Testing

- storetest Blobs conformance for every backend claiming Blobs (rclonestore's suite runs
  under `//go:build integration` against a local `rclone` + temp remote).
- workspacestore property tests: snapshot→materialize round-trip yields an identical tree
  (contents, modes, symlinks); identical trees yield identical refs; verified reuse no-ops
  on an exact warm tree and rejects a drifted one (`DestNotEmptyError`, tree untouched);
  hostile-archive corpus (traversal names, symlink escapes, bombs) all rejected —
  table-driven per CLAUDE.md, `-race` always.
- End-to-end (integration tag): snapshot → append `WorkspaceCheckpointed` → new store
  instance → replay → materialize → tree equality. The suspend/resume path in one test.

## Composes with flow (informative)

In a flow-orchestrated multi-agent system, a graph node runs a looprig session to
quiescence inside a `Task.Execute`. A flow interrupt checkpoints the graph with the node's
sessionID in its state; the session's journal carries its own `WorkspaceCheckpointed` ref.
Resume walks back in through flow → session replay → materialize. Composition is by
reference at every layer; no store is shared by contract, only (optionally) by backing.

## Migration phases (detail in the implementation plan)

- **A.** `pkg/workspacestore` in looprig core (against memstore + fsstore Blobs) +
  `WorkspaceCheckpointed` event + codec.
- **B.** Harness quiescence hook: snapshot-then-append on suspend; materialize on resume.
- **C.** `rclonestore` module + integration suite.
- **D.** swe wiring (laptop profile: fsstore for both stores).

## Decision log (from design discussion, 2026-07-02)

1. Session store vs workspace store vs live workspace: three named concepts; the live
   workspace gets no abstraction.
2. Two contracts, never merged (backend viability barely overlaps in both directions);
   they link through the journal by ref.
3. Snapshot model is truth; mounts only ever a cache (idempotent Materialize fast path).
4. rclone via exec in its own module; librclone rejected.
5. natsstore also provides workspace Blobs (JetStream ObjectStore).
6. Fleet profile is the design target; scale-to-zero is a documented composition with
   fencing delegated to the orchestrator.
7. Review fix (2026-07-02): the marker-file fast path was unsound — a warm volume drifts
   when a turn crashes after the last checkpoint, and a marker would trust it anyway.
   Materialize now proves reuse by deterministic re-archive + digest compare, requires an
   empty dest otherwise, and never wipes (`DestNotEmptyError` leaves the decision to the
   caller).
