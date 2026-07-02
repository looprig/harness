# workspacestore Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Durable, journal-ordered workspace snapshots so a cloud-run agent's working files survive suspend/resume on any host.

**Architecture:** `pkg/workspacestore` in looprig core owns all domain logic (deterministic tar.gz archiving, content addressing, hostile-archive-safe extraction, verified warm-volume reuse) over any `storekit.Blobs` backend. A new enduring `WorkspaceCheckpointed{Ref}` event links snapshots into the session journal (snapshot-before-append). `rclonestore` is a new backend module implementing `storekit.Blobs` by exec'ing the rclone binary.

**Tech Stack:** Go stdlib (`archive/tar`, `compress/gzip`, `crypto/sha256`, `os/exec`); storekit contracts; external `rclone` binary (never linked).

**Design spec:** `docs/plans/2026-07-02-workspacestore-design.md`.
**Prerequisite:** Phase A+B of `2026-07-02-storekit-sessionstore-plan.md` (storekit exists; looprig depends on it; `pkg/sessionstore` works). Phase C of this plan additionally has no looprig dependency at all.

---

## Execution notes

- Same conventions as the companion plan: `GOWORK=off` everywhere, table-driven
  `t.Parallel()` tests, typed errors, `-race`, `make secure` before looprig commits,
  `replace` directives during development.
- looprig work happens on the `feat/storekit-extraction` branch (or a follow-up
  `feat/workspacestore` branch off it, if the extraction already merged).
- **GC constraint (records a deliberate v1 narrowing of the spec):** `storekit.Blobs` has
  no Stat/age surface, so the spec's "older than a policy age" sweep cannot be implemented
  contract-purely. v1 GC is live-set-only (`GC(ctx, live)` deletes unreferenced snapshot
  blobs) and documents that it must not run concurrently with active snapshotting.
  Age-based safety needs a `Stat` addition to Blobs — future work, noted in the doc
  comment and the spec's future-work section (Task A6 amends the spec).

---

## Phase A — pkg/workspacestore (looprig core)

### Task A1: Ref type + typed errors

**Files:**
- Create: `pkg/workspacestore/ref.go`, `pkg/workspacestore/ref_test.go`

**Step 1 (failing tests):** `ParseRef` table: valid `v1:sha256:<64 lowercase hex>`;
invalid: empty, `v2:...` (unknown version), bad algo, 63/65 hex chars, uppercase hex,
missing colons. `Ref.blobKey()` = `workspaces/<hex>`.

**Step 2:** Implement:

```go
// Ref names one immutable snapshot: "v1:sha256:<hex>". Opaque to callers.
type Ref string

func ParseRef(s string) (Ref, error) // *InvalidRefError on violation

type InvalidRefError struct{ Value, Reason string }
type DestNotEmptyError struct{ Dest string; Want Ref; GotDigest string }
type SnapshotError struct{ Root string; Cause error }    // Unwrap
type MaterializeError struct{ Ref Ref; Dest string; Cause error } // Unwrap
type ArchiveEntryError struct{ Name, Reason string }     // hostile-entry rejection
```

**Step 3–4:** Pass; commit `feat(workspacestore): Ref + error taxonomy`.

### Task A2: Deterministic archive writer

**Files:**
- Create: `pkg/workspacestore/archive.go`, `pkg/workspacestore/archive_test.go`

**Step 1 (failing tests):**
- determinism: build a tree (files, nested dirs, symlink, 0755 executable), archive twice
  → byte-identical output; rebuild the same tree in a different order at a different
  tmpdir → identical digest.
- mtime/uid/gid independence: `touch` a file to a different mtime → digest unchanged.
- mode preservation: 0755 stays 0755 in headers.
- symlinks stored as symlinks (header type, target preserved, never followed).
- irregular files (sockets, fifos — create with `net.Listen("unix", ...)`) → typed
  `*ArchiveEntryError`, fail closed.
- root symlink-escape: a symlinked subdir pointing outside root is archived as the
  symlink entry itself (not followed) — no content outside root ever read.

**Step 2:** Implement `writeArchive(w io.Writer, root string) error`:
`filepath.WalkDir`, collect relative paths, sort, emit `tar.Header`s with
`ModTime: time.Unix(0,0)`, `Uid/Gid: 0`, `Uname/Gname: ""`, `Format: tar.FormatPAX`
fixed; `gzip.NewWriterLevel(w, gzip.BestSpeed)` with zeroed gzip header (`ModTime`,
`Name`, `OS: 255`). Use `tar.FileInfoHeader` then normalize. Read files with
`O_NOFOLLOW`-safe stat ordering (lstat first; only regular files opened).

**Step 3–4:** Pass with `-race`; commit `feat(workspacestore): deterministic archive`.

### Task A3: Snapshot

**Files:**
- Create: `pkg/workspacestore/store.go` (Open/Options/Store), `pkg/workspacestore/snapshot.go`,
  `pkg/workspacestore/snapshot_test.go`

**Step 1 (failing tests, memstore Blobs):** snapshot of a tree returns a parseable Ref;
same tree → same Ref and (via a counting Blobs wrapper) the second Put is skipped
(content-addressed no-op: check `List` before `Put`, or Put unconditionally — pick
skip-if-present and assert it); root validation: nonexistent root, root outside itself
after `filepath.Clean`, file-not-dir root → `*SnapshotError`.

**Step 2:** Implement:

```go
func Open(b storekit.Blobs, opts ...Option) (*Store, error) // nil b → typed error

// Snapshot: spool writeArchive output to an O_TMPDIR temp file while teeing
// into sha256; derive Ref; if blobKey absent, seek-to-0 and Blobs.Put. The
// spool means the working set never resides in memory and the digest names
// the key before any upload byte is sent (upload-before-append discipline is
// the caller's, but the key must be final before Put begins).
func (s *Store) Snapshot(ctx context.Context, root string) (Ref, error)
```

**Step 3–4:** Pass; commit `feat(workspacestore): Snapshot`.

### Task A4: Guarded extraction

**Files:**
- Create: `pkg/workspacestore/extract.go`, `pkg/workspacestore/extract_test.go`,
  `pkg/workspacestore/extract_fuzz_test.go`

**Step 1 (failing tests — the hostile corpus, table-driven; build archives by hand with
`archive/tar` so each attack is precise):**
- happy path: extract(archive(tree)) reproduces the tree (contents, modes, symlinks) —
  the round-trip property test.
- `../escape` entry name → `*ArchiveEntryError`.
- absolute `/etc/x` entry name → rejected.
- entry name with `..` mid-path → rejected.
- symlink whose *name* escapes → rejected; symlink whose *target* is absolute or escapes
  is **created as-is but never followed during extraction** (later writes go through
  entry-name containment, so a hostile target cannot redirect a write; assert a following
  entry `link/inner` is rejected because its cleaned path resolves through a symlink —
  implement by `O_NOFOLLOW`/`os.Lstat` checks on every path component under dest).
- device/fifo/hardlink entries → rejected.
- decompression bomb: entry count > `MaxEntries` (default 1<<20) or cumulative size >
  `MaxBytes` (default 8 GiB, `Options` knobs) → typed error, partial output removed.

**Step 2:** Implement `extractArchive(ctx, r io.Reader, dest string, limits limits) error`
with per-entry validation before any write; extraction into `dest` which the caller
guarantees empty; on error, best-effort `os.RemoveAll(dest contents)` then return.

**Step 3:** `FuzzArchiveEntryName` (never panics, never writes outside a canary dest).
**Step 4:** Commit `feat(workspacestore): guarded extraction + hostile corpus`.

### Task A5: Materialize with verified reuse + Delete

**Files:**
- Create: `pkg/workspacestore/materialize.go`, `pkg/workspacestore/materialize_test.go`

**Step 1 (failing tests):** empty dest → extracts (round-trip equality); dest holding
exactly the ref's tree → verified no-op (Blobs.Get never called — assert with counting
wrapper); dest with one byte changed → `*DestNotEmptyError`, tree untouched; dest with an
extra file → `*DestNotEmptyError`; absent blob → `*storekit.BlobNotFoundError` surfaced
inside `*MaterializeError`; digest of fetched blob re-verified during extraction (tamper
the stored blob bytes → typed error, nothing left in dest).

**Step 2:** Implement: `Materialize` = if dest missing/empty → fetch (`Blobs.Get`), tee
through sha256 while extracting, compare final digest to ref (fail closed, wipe partial);
if dest non-empty → `writeArchive(dest)` into a digest-only writer (no spool needed),
compare to ref: equal → nil; else `*DestNotEmptyError`. `Delete(ctx, ref)` =
`Blobs.Delete(blobKey)` (idempotent per storekit).

**Step 3–4:** Pass; commit `feat(workspacestore): Materialize (verified reuse) + Delete`.

### Task A6: GC helper + spec amendment

**Files:**
- Create: `pkg/workspacestore/gc.go`, `pkg/workspacestore/gc_test.go`
- Modify: `docs/plans/2026-07-02-workspacestore-design.md` (GC section + future work)

**Steps (TDD):** `GC(ctx context.Context, live map[Ref]struct{}) (deleted []Ref, err error)`
— `List("workspaces/")`, delete keys whose Ref is not in live. Doc comment carries the
concurrency constraint from the execution notes. Amend the spec's GC paragraph: v1 is
live-set-only; age-based sweep requires a future `Blobs.Stat` (add one line under
Non-goals/future). Commit `feat(workspacestore): live-set GC` and
`docs(plans): record v1 GC narrowing`.

### Task A7: WorkspaceCheckpointed event

**Files:**
- Modify: `pkg/event/` — the sealed union: add the event type where its lifecycle
  siblings live (find `RestoreDone` in `pkg/event/event.go` and mirror placement),
  `pkg/event/marshal.go` `MarshalEvent` switch (`journal.go:112` region) and
  `UnmarshalEvent` tag switch (`:316` region)
- Modify/Create: the event package's existing codec test tables (find the table covering
  `RestoreDone` and add rows)

**Step 1 (failing test):** add `WorkspaceCheckpointed{Ref: "v1:sha256:<hex>"}` rows to the
marshal/unmarshal round-trip tables + Class() table (`Enduring`).

**Step 2:** Implement:

```go
// WorkspaceCheckpointed records that the session's workspace was durably
// snapshotted as Ref at this point in the event order. Enduring: it is the
// resume token's pointer to the workspace store.
type WorkspaceCheckpointed struct{ Ref string }
```

Field is `string`, not `workspacestore.Ref` — pkg/event must not import workspacestore
(keep the event package dependency-light; the harness converts). Wire both codec switches.

**Step 3–4:** `GOWORK=off go test -race ./pkg/event/...` green; commit
`feat(event): WorkspaceCheckpointed enduring event`.

---

## Phase B — session integration (suspend/resume path)

### Task B1: Session checkpoint capability

**Files:**
- Read first: `pkg/session/` construction surface (how `newSession`/`Restore` receive
  config; where the event appender lives) — pick the same Option pattern `Restore` uses
- Modify: `pkg/session/` — add an Option wiring `{WS *workspacestore.Store, Root string}`;
  add `Session.CheckpointWorkspace(ctx) (workspacestore.Ref, error)`

**Step 1 (failing test, memstore end-to-end):** configure a session with a workspace
store + temp root; write a file into root; `CheckpointWorkspace` → returns Ref; the
journal's record tail contains a `WorkspaceCheckpointed` with that Ref **after** the blob
exists (counting-wrapper order assertion — snapshot-before-append); unconfigured session →
typed `WorkspaceNotConfiguredError`.

**Step 2:** Implement: `Snapshot(root)` then append the event through the session's
existing enduring-event append path. looprig exposes the capability; *when* to call it
(quiescence) is the composition root's decision — consistent with looprig-as-SDK and the
foreign-loop quiescence model. Document that on the method.

**Step 3–4:** Green; commit `feat(session): CheckpointWorkspace`.

### Task B2: Materialize on restore

**Files:**
- Modify: `pkg/session/restore.go` / `restore_constructor.go` (same Option), tests

**Step 1 (failing test):** full-cycle — session A: write files, `CheckpointWorkspace`,
close. `Restore` with the workspace Option pointing at a fresh empty root: root now equals
the checkpointed tree; a session with **no** `WorkspaceCheckpointed` in its journal
restores with the root untouched; Materialize failure → restore fails closed
(`RestoreErrored` recorded, lease released — same contract as any restore failure);
warm-volume case: pre-populate the root with the exact tree → restore succeeds via
verified reuse.

**Step 2:** Implement inside the restore fold: track the last `WorkspaceCheckpointed`
during replay; after a successful fold and before the session goes live, `Materialize`.

**Step 3–4:** Green; `make secure`; commit `feat(session): workspace materialize on restore`.

---

## Phase C — rclonestore module

### Task C1: Scaffold + exec runner

**Files:**
- Create: `~/code/ciram-co/rclonestore/` — go.mod (module `github.com/ciram-co/rclonestore`,
  go 1.25.0, require storekit + replace), CLAUDE.md (stdlib + storekit only; drives the
  external rclone binary; never librclone), Makefile (with `test-integration`)
- Create: `runner.go`, `runner_test.go`

**Step 1 (failing tests):** the runner executes argv (no shell), always inserts `--`
before positionals, bounds every call with ctx, captures a tail of stderr into typed
errors, streams stdin/stdout. Test with a fake rclone: a helper script installed by the
test into `t.TempDir()` (`#!/bin/sh` echoing args / consuming stdin) — assert exact argv,
`--` present, timeout kill.

**Step 2:** Implement `run(ctx, opts, args []string, stdin io.Reader, stdout io.Writer) error`
over `exec.CommandContext`; `// #nosec` not needed (argv list, no shell). Typed
`RcloneError{Args []string (subcommand + key only — never anything that could carry
secrets), ExitCode int, Stderr string}`.

**Step 3–4:** Pass; commit `feat: exec runner`.

### Task C2: Blobs over rclone

**Files:**
- Create: `blobs.go`, `blobs_test.go`

**Step 1 (failing tests, fake rclone):** mapping table —
`Put(key, r)` → `rclone rcat -- <remote>:<prefix>/<key>` with r on stdin;
`Get` → `rclone cat -- <remote>:<prefix>/<key>` streaming stdout; nonexistent → exit
code/stderr classified to `*storekit.BlobNotFoundError`;
`Delete` → `rclone deletefile --` (absent → nil, idempotent);
`List(prefix)` → `rclone lsf --files-only -R --` output parsed, prefixed keys returned
**sorted deduped** (sort locally — do not trust tool order). Keys validated against the
storekit grammar before ever reaching argv (defense in depth on top of `--`).

**Step 2:** Implement. **Step 3–4:** Pass; commit `feat: Blobs via rclone exec`.

### Task C3: Open probe + conformance

**Files:**
- Create: `rclonestore.go` (`Options{Remote, Prefix, Binary, ConfigPath string, Timeout time.Duration}`;
  `New` validates Remote against `^[A-Za-z0-9_-]+$` or the `:local:`-style connection
  form, resolves Binary via `exec.LookPath`, probes with `rclone lsf --max-depth 0` —
  fail loudly per fail-secure), `README.md`
- Create: `conformance_integration_test.go` (`//go:build integration`)

**Steps:** integration conformance runs `storetest.TestBlobs` against a real rclone with
the **local** backend (`Options{Remote: ":local:" + t.TempDir()}`) — no cloud credentials
needed; skip with a clear message if `rclone` is not on PATH. Unit suite stays green
without rclone installed. Commit; tag `v0.1.0-rc1`.

---

## Phase D — swe wiring + docs

### Task D1: swe workspace wiring

**Files (discover):** swe's session composition (found in companion plan Task E1) and its
existing workspace-root config (`grep -rn "WorkspaceRoot" --include="*.go" ~/code/swe | grep -v vendor`)

**Steps:** laptop profile — `workspacestore.Open(fs)` reusing the same `fsstore` value as
sessions; pass the workspace Option to session construction; call
`CheckpointWorkspace` at swe's quiescence signal (where it emits/handles SessionIdle —
the same place a cloud harness would suspend). Run swe suite. Commit in swe.

### Task D2: End-to-end integration test + wrap-up

**Files:**
- Create: `pkg/workspacestore/e2e_integration_test.go` (`//go:build integration`) in looprig

**Steps:** the spec's suspend/resume test in one function against fsstore (via the dev
replace): tree → Snapshot → `WorkspaceCheckpointed` appended → new sessionstore instance →
replay → Materialize into fresh dir → tree equality (contents, modes, symlinks). Then:
`make secure`, full `-race` suite, `-tags integration` suite. Tag rclonestore `v0.1.0`
alongside the companion plan's Task E2 tags. Update memory index notes.

---

## Verification (whole plan)

- Round-trip property, hostile corpus, and drift-rejection tests all in the default
  (`-race`) suite — no tags needed except the fsstore/rclone/e2e integration runs.
- `make secure` on looprig: gosec must be clean on the extraction code (path handling is
  the hot spot — every finding either fixed or explicitly justified inline).
- Manual smoke (optional, laptop profile): run swe, create files in a session workspace,
  kill the process at quiescence, resume, confirm the workspace and conversation both
  came back.
