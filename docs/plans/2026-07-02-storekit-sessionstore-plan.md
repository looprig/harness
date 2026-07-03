# storekit + sessionstore Extraction Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Extract looprig's session persistence behind neutral storekit contracts so NATS becomes one pluggable backend module and looprig core carries zero NATS dependency.

**Architecture:** A new stdlib-only leaf module `storekit` defines the primitives (`Ledger`, `Leaser`, `KV`, `Blobs`), typed errors, the `AppendDefinite` ambiguity resolver, an in-memory reference backend, and a conformance suite. looprig gains `pkg/sessionstore` (domain facade over any backend) and loses `pkg/journal`'s NATS files plus `pkg/persistence`. Two new backend modules: `fsstore` (fresh, local disk) and `natsstore` (port of the existing JetStream machinery + embedded server).

**Tech Stack:** Go stdlib throughout except natsstore (`nats-io/nats.go` + `nats-io/nats-server/v2`, already sanctioned — approvals move to that module's own CLAUDE.md).

**Design spec:** `docs/plans/2026-07-02-storekit-sessionstore-design.md` — read it first; it pins every contract semantic referenced below.

---

## Execution notes (read before Task A1)

- **Repos:** looprig lives at `~/code/looprig`. New repos go under `~/code/ciram-co/`:
  `storekit`, `fsstore`, `natsstore` (sibling of the existing `~/code/ciram-co/flow`).
- **Every Go command in every repo runs with `GOWORK=off`** — there is a `go.work` at
  `~/code` that must not capture these modules. Standard invocations:
  - build: `GOWORK=off CGO_ENABLED=0 go build -trimpath ./...`
  - test: `GOWORK=off go test -race ./...`
- **Cross-module wiring during development:** the new modules are unpublished. Use
  `replace` directives (e.g. in looprig's go.mod:
  `replace github.com/looprig/storekit => ../ciram-co/storekit`) and re-vendor
  (`GOWORK=off go mod vendor`) after every go.mod change in looprig. Task E2 removes the
  replaces and pins tags. Set `GOPRIVATE=github.com/ciram-co` when fetching tags.
- **Ordering constraint:** natsstore (Phase D) ports code out of looprig's `pkg/journal`
  and `pkg/persistence`. Phase B deletes those files. Phase D therefore copies from the
  git tag created in Task B1 (`pre-storekit-extraction`), not from looprig's HEAD.
- **looprig conventions apply everywhere** (looprig `CLAUDE.md`): table-driven tests with
  `t.Parallel()`, typed errors only, `-race` always, `make secure` before looprig commits.
  New repos get a minimal Makefile with `fmt-check`/`vet`/`test` and the same rules.
- Deviation from spec, deliberate: the sessionstore offload threshold default stays at the
  current **512 KiB** (`pkg/journal/objectstore.go:23`), not the spec's provisional
  256 KiB. It is an `Options` knob either way.

---

## Phase A — storekit module

### Task A1: Scaffold the storekit repo

**Files:**
- Create: `~/code/ciram-co/storekit/go.mod`, `README.md`, `CLAUDE.md`, `Makefile`, `.gitignore`

**Step 1:** `mkdir -p ~/code/ciram-co/storekit && cd ~/code/ciram-co/storekit && git init -b main`

**Step 2:** Write `go.mod`:

```
module github.com/looprig/storekit

go 1.25.0
```

(1.25.0, not looprig's 1.26.4 — flow is on 1.25.0 and must be able to import storekit.)

**Step 3:** Write `CLAUDE.md` (short): stdlib-only module, no third-party deps ever; typed
errors; table-driven `t.Parallel()` tests; `-race` always. Write `README.md` one-paragraph
stub naming the four contracts. Write `Makefile`:

```make
test:
	GOWORK=off go test -race ./...
fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
vet:
	GOWORK=off go vet ./...
check: fmt-check vet test
```

**Step 4:** `git add -A && git commit -m "chore: scaffold storekit module"`

### Task A2: Name grammar (`ValidateName`)

**Files:**
- Create: `names.go`, `names_test.go`

**Step 1:** Write the failing table test. Cases (all must be in the table):
valid: `a`, `sessions/0b0e...` (uuid-shaped), `a/b_c.d-e`, exactly-512-byte name.
invalid: `""`, `A/b` (uppercase), `a//b`, `a/./b`, `a/../b`, `/a`, `a/`, `.a`, `-a`,
`_a` (segment must start `[a-z0-9]`), 513-byte name, `a b`, `a\x00b`.

**Step 2:** `GOWORK=off go test -race ./... -run TestValidateName` → FAIL (undefined).

**Step 3:** Implement:

```go
package storekit

// InvalidNameError reports a name that violates the storekit grammar: one or
// more segments joined by single '/', each segment matching
// [a-z0-9][a-z0-9_.-]*, no leading/trailing '/', at most 512 bytes total.
type InvalidNameError struct {
	Name string
	Rule string
}

func (e *InvalidNameError) Error() string {
	return "storekit: invalid name " + strconv.Quote(e.Name) + ": " + e.Rule
}

// ValidateName reports whether name conforms to the storekit name grammar.
// The grammar is canonical by construction: empty, ".", and ".." segments are
// unrepresentable, so no two valid names alias one backend location.
func ValidateName(name string) error { ... } // iterate segments; no regexp needed
```

**Step 4:** Test passes. **Step 5:** Commit `feat: name grammar validation`.

### Task A3: Typed errors

**Files:**
- Create: `errors.go`, `errors_test.go`

**Step 1–4 (TDD):** Define exactly the spec's taxonomy, each a concrete struct with an
`Error()` implementation and an `Unwrap()` where it carries a cause:

```go
type ConflictError struct{ Name string; Expected uint64 }
type AmbiguousError struct{ Name string; Expected uint64; Cause error } // Unwrap → Cause
type RecordNotFoundError struct{ Name string; Seq uint64 }
type KeyNotFoundError struct{ Key string }
type BlobNotFoundError struct{ Key string }
type LeaseHeldError struct{ Name string; HolderEpoch uint64 }
type LeaseLostError struct{ Name string; Epoch uint64 }
```

Table test: each error's `Error()` is non-empty and names its subject; `errors.As`
round-trips through `fmt.Errorf("wrap: %w", err)`; `AmbiguousError` unwraps to its cause.
There is deliberately **no** ledger-not-found error (absent == empty).

**Step 5:** Commit `feat: typed error taxonomy`.

### Task A4: Contracts

**Files:**
- Create: `storekit.go` (package doc + `Ledger`, `Record`, `Cursor`, `Leaser`, `Lease`,
  `KV`, `Blobs`, `Composite`)

**Step 1:** Copy the interfaces **verbatim from the design spec** (sections "Ledger",
"Leaser", "KV and Blobs", "Composite"). Signatures, exactly:

```go
type Ledger interface {
	Append(ctx context.Context, name string, expected uint64, payload []byte) error
	Read(ctx context.Context, name string, from uint64) (Cursor, error)
	Tip(ctx context.Context, name string) (uint64, error)
	Delete(ctx context.Context, name string) error
}
type Record struct{ Seq uint64; Payload []byte }
type Cursor interface {
	Next(ctx context.Context) (Record, error) // io.EOF when drained
	Close() error
}
type Leaser interface {
	Acquire(ctx context.Context, name string) (Lease, error)
}
type Lease interface {
	Epoch() uint64
	Lost() <-chan struct{}
	Release(ctx context.Context) error
}
type KV interface {
	Get(ctx context.Context, key string) (val []byte, rev uint64, err error)
	Put(ctx context.Context, key string, expectedRev uint64, val []byte) (rev uint64, err error)
	Keys(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}
type Blobs interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}
type Composite struct{ Ledger; Leaser; KV; Blobs }
```

Doc comments come from the spec too — the edge-semantics block (absent == empty; reads
beyond tip drained; caller-owned payloads; sorted listings) goes on `Ledger` verbatim.

**Step 2:** `GOWORK=off go vet ./...` clean. **Step 3:** Commit `feat: storekit contracts`.

### Task A5: memstore — Ledger

**Files:**
- Create: `memstore/memstore.go`, `memstore/ledger_test.go`

**Step 1 (failing tests first):** table cases: append then read round-trips; sequences are
1-based contiguous; CAS conflict on wrong expected returns `*ConflictError`; expected 0 on
non-empty conflicts; absent ledger: `Tip` 0, `Read` drained, `Delete` no-op nil; `from >
tip` drained; cursor bounded (append after `Read` not observed); payload isolation (mutate
the appended slice after Append and the returned slice after Next — stored data unchanged;
memstore must copy in and copy out); zero-length payload legal; invalid name →
`*InvalidNameError` from every method.

**Step 2:** Implement `memstore.Store` (`New() *Store`): `map[string][][]byte` under
`sync.RWMutex`; every method validates names first. Never returns `AmbiguousError`.

**Step 3:** Tests pass with `-race`. **Step 4:** Commit `feat(memstore): ledger`.

### Task A6: memstore — Leaser, KV, Blobs

**Files:**
- Modify: `memstore/memstore.go`; Create: `memstore/lease_test.go`, `memstore/kv_test.go`, `memstore/blobs_test.go`

**Steps (TDD per primitive, commit each):**
- Leaser: `Acquire` while held → `*LeaseHeldError`; epochs strictly increase across
  grant→release→grant; `Release` closes `Lost()`; double-`Release` is a no-op nil.
- KV: `Put` rev-CAS (expectedRev 0 = create-only), `Get` on absent → `*KeyNotFoundError`,
  `Keys` sorted deduped, `Delete` idempotent.
- Blobs: `Put` reads the reader fully and copies; `Get` returns an independent
  `io.ReadCloser`; absent → `*BlobNotFoundError`; `List` sorted.
- Final assertion in `memstore_test.go`:
  `var _ interface { storekit.Ledger; storekit.Leaser; storekit.KV; storekit.Blobs } = (*Store)(nil)`
  and `storekit.Composite{Ledger: s, Leaser: s, KV: s, Blobs: s}` compiles.

### Task A7: AppendDefinite

**Files:**
- Create: `appenddefinite.go`, `appenddefinite_test.go`

**Step 1 (failing tests):** use an in-file `fakeLedger` whose `Append`/`Read` responses are
scripted per call. Table (mirrors the branch analysis in looprig's
`pkg/journal/journal.go` `resolveAmbiguous`/`reconcileTip`, which this replaces):

| case | script | want |
|---|---|---|
| clean success | append→nil | nil |
| definite conflict, foreign at tip | append→Conflict; read(expected+1)→foreign bytes | `*ConflictError` |
| definite conflict, ours at tip | append→Conflict; read→same bytes | nil (original landed) |
| ambiguous then retry lands | append→Ambiguous; append→nil | nil |
| ambiguous then conflict, ours | append→Ambiguous; append→Conflict; read→same bytes | nil |
| ambiguous then conflict, foreign | append→Ambiguous; append→Conflict; read→foreign | `*ConflictError` |
| ambiguous twice | append→Ambiguous; append→Ambiguous | `*AmbiguousError` |
| conflict but nothing at expected+1 | append→Conflict; read→drained | `*ConflictError` |
| read error during verify | append→Conflict; read→err | that error, wrapped |

**Step 2:** Implement per the spec doc comment: retry the identical append once on
ambiguous; on conflict from either attempt, read the record at expected+1 and
byte-compare (`bytes.Equal`); equal → nil; unequal/absent → `*ConflictError`; second
ambiguous → `*AmbiguousError` (cause = first ambiguous error).

**Step 3–4:** Pass; commit `feat: AppendDefinite ambiguity resolver`.

### Task A8: storetest conformance suite

**Files:**
- Create: `storetest/ledger.go`, `storetest/leaser.go`, `storetest/kv.go`,
  `storetest/blobs.go`, `storetest/doc.go`
- Create: `memstore/conformance_test.go`

**Step 1:** Each suite is an exported func a backend's tests call:

```go
package storetest

// TestLedger runs the Ledger conformance suite. newBackend must return a fresh,
// empty backend; cleanup (may be nil) runs after each subtest.
func TestLedger(t *testing.T, newBackend func(t *testing.T) storekit.Ledger)
func TestLeaser(t *testing.T, newBackend func(t *testing.T) storekit.Leaser)
func TestKV(t *testing.T, newBackend func(t *testing.T) storekit.KV)
func TestBlobs(t *testing.T, newBackend func(t *testing.T) storekit.Blobs)
```

Assertions: everything from memstore's tables (Tasks A5–A6 — lift them into the suite so
they run against every backend) **plus** the spec's storetest list: concurrent-appender
linearization (N goroutines race `AppendDefinite` at the observed tip; final ledger is
gap-free and every committed payload unique), name-grammar rejection, 1 MiB payload-floor
acceptance, sorted duplicate-free listings, reads-beyond-tip, idempotent delete.

**Step 2:** `memstore/conformance_test.go` calls all four suites with `memstore.New()`.
The memstore-specific test files from A5/A6 keep only memstore-specific cases (payload
copy-in/copy-out); shared cases live in storetest only (DRY).

**Step 3:** `GOWORK=off go test -race ./...` all green. **Step 4:** `make check`.
**Step 5:** Commit `feat: storetest conformance suite`, then `git tag v0.1.0-rc1`.

---

## Phase B — looprig: pkg/sessionstore + NATS removal

### Task B1: Branch, tag, and wire storekit

**Files:**
- Modify: `~/code/looprig/go.mod`

**Step 1:** In looprig: `git tag pre-storekit-extraction && git checkout -b feat/storekit-extraction`
(the tag is what Phase D ports from).

**Step 2:** Add to go.mod: `require github.com/looprig/storekit v0.0.0` +
`replace github.com/looprig/storekit => ../ciram-co/storekit`. Run
`GOWORK=off go mod tidy && GOWORK=off go mod vendor`.

**Step 3:** `GOWORK=off go build ./...` clean. Commit `chore: depend on storekit (dev replace)`.

### Task B2: Record envelope

**Files:**
- Create: `pkg/sessionstore/envelope.go`, `pkg/sessionstore/envelope_test.go`,
  `pkg/sessionstore/envelope_fuzz_test.go`

**Step 1 (failing tests):** round-trip each kind; unknown kind fails closed; unknown
version fails closed; `blobptr` body round-trips; truncated/garbage input → typed error.

**Step 2:** Implement:

```go
// envelope is the versioned wire frame for one ledger record. Body is the
// record's codec bytes (event/command/fence payload, or the blobptr JSON).
// encoding/json base64s []byte, so the frame is codec-agnostic.
type envelope struct {
	V    int    `json:"v"`    // 1
	Kind string `json:"kind"` // "event" | "command" | "fence" | "blobptr"
	ID   string `json:"id"`   // domain idempotency id (ex-Nats-Msg-Id)
	Body []byte `json:"body"`
}

type blobPointer struct {
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type EnvelopeError struct{ Reason string; Cause error } // typed decode/encode failure
```

Kinds map 1:1 from `journal.EventRecord`/`CommandRecord`/`FenceRecord` (see
`pkg/journal/record.go`); marshalling of Body delegates to the existing codecs exactly as
`marshalRecord` does today (`pkg/journal/journal.go:364`).

**Step 3:** Add `FuzzEnvelopeDecode` (30s in CI is enough: `go test -fuzz=FuzzEnvelopeDecode
-fuzztime=30s ./pkg/sessionstore`). **Step 4:** Commit `feat(sessionstore): record envelope`.

### Task B3: Facade skeleton — Backend, Options, Open, lease adapter

**Files:**
- Create: `pkg/sessionstore/sessionstore.go`, `pkg/sessionstore/lease.go`,
  `pkg/sessionstore/lease_test.go`
- Read first: `pkg/journal/lease.go` (the `journal.Lease` interface it must adapt to)

**Step 1:** Define exactly the spec's facade:

```go
type Backend interface {
	storekit.Ledger
	storekit.Leaser
	storekit.KV
	storekit.Blobs
}

type Options struct{ OffloadThreshold int } // default 512 KiB
func Open(b Backend, opts ...Option) (*Store, error) // nil b / nil Composite fields → typed error
```

Ledger name derivation: `ledgerName(id uuid.UUID) = "sessions/" + id.String()`; KV catalog
key the same; blob keys `sessions/<id>/blobs/<sha256-hex>`.

**Step 2 (TDD):** lease adapter — wraps `storekit.Lease` into the `journal.Lease`
interface: `Valid()` = non-blocking `Lost()` select; `Epoch()`/`Lost()` passthrough;
`Release(ctx)` passthrough. Port the relevant cases from `pkg/journal/lease_test.go`.

**Step 3:** `Store.AcquireLease(ctx, id)` = `b.Acquire(ctx, ledgerName(id))` + adapt.
Tests against memstore. **Step 4:** Commit `feat(sessionstore): facade skeleton + lease adapter`.

### Task B4: Journal writer

**Files:**
- Create: `pkg/sessionstore/journal.go`, `pkg/sessionstore/journal_test.go`
- Read first: `pkg/journal/journal.go` (semantics being ported), `pkg/journal/objectstore.go`

**Step 1 (failing tests, ported from `pkg/journal/journal_test.go` onto memstore):**
append before opening fence → `journal.JournalNotReadyError`; opening fence carries the
lease epoch as the first record; appends serialized and strictly ordered under concurrent
callers; append after `Lost()` fires → `journal.JournalLeaseLostError`; over-threshold
record: blob exists **before** the pointer record (assert via a Blobs wrapper that records
call order); stale second writer (fresh journal on same ledger at old tip) → conflict
surfaces as `journal.AppendError`; per-append timeout honored (wrap memstore with a
blocking Ledger).

**Step 2:** Implement `sessionJournal` satisfying `journal.SessionJournal`
(`Append(ctx, journal.JournalRecord) (uint64, error)`):
mutex-serialized; `ready` set by the opening fence (written in `Store.OpenJournal` via the
same code path); fast-path lease check; envelope-encode; if `len(payload) >
OffloadThreshold`, `Blobs.Put` the envelope bytes content-addressed, then append a
`blobptr` envelope (same ID); `storekit.AppendDefinite` at the tracked tip; advance tip on
success. Per-append deadline: 5s child context (constant, same as today's
`defaultAppendTimeout`). Map `*storekit.ConflictError` → `journal.AppendError`,
`*storekit.AmbiguousError` → `journal.AmbiguousAckError` (keep the existing exported error
types in pkg/journal — callers already handle them).

**Step 3–4:** Green with `-race`; commit `feat(sessionstore): fenced journal writer`.

### Task B5: Replayers

**Files:**
- Create: `pkg/sessionstore/replay.go`, `pkg/sessionstore/replay_test.go`
- Read first: `pkg/journal/replay.go`, `pkg/journal/record_replay.go` (interfaces +
  semantics), `pkg/transcript/journalsource/source.go` (a consumer to keep working)

**Step 1 (failing tests):** replay-equals-appended (append N mixed records through the
Task-B4 writer, replay, compare); `FromSeq` starts inclusive; fences surface to
`RecordReplayer` but are skipped by `EventReplayer` (match current behavior — verify in
`pkg/journal/replay.go` before coding); `blobptr` records are resolved transparently via
`Blobs.Get` with SHA256 verified (mismatch → typed error, fail closed); missing blob →
typed error.

**Step 2:** Implement `Store.OpenEventReplayer` / `Store.OpenRecordReplayer` returning
implementations of the **unchanged** `journal.EventReplayer`/`journal.RecordReplayer`
interfaces, reading `storekit.Cursor` + envelope decode + codec unmarshal.

**Step 3–4:** Green; commit `feat(sessionstore): replayers over storekit`.

### Task B6: Catalog

**Files:**
- Create: `pkg/sessionstore/catalog.go`, `pkg/sessionstore/catalog_test.go`
- Read first: `pkg/journal/catalog.go`, `pkg/journal/catalog_list.go` + their tests

**Steps (TDD):** Preserve the exported surface of the existing catalog (entry type,
list/upsert semantics — read first, port the API verbatim) reimplemented over
`storekit.KV` with rev-CAS updates. Port `catalog_test.go`/`catalog_list_test.go` cases to
memstore. Commit `feat(sessionstore): catalog over KV`.

### Task B7: Offload-blob GC

**Files:**
- Create: `pkg/sessionstore/gc.go`, `pkg/sessionstore/gc_test.go`
- Read first: `pkg/journal/gc.go` + `gc_test.go`

**Steps (TDD):** Port ObjectGC semantics: scan the session's ledger for live `blobptr`
keys, `Blobs.List` the session's blob prefix, delete unreferenced. Same policy surface as
the current `NewObjectGC`. Commit `feat(sessionstore): offload GC`.

### Task B8: Swap session.Restore to the facade

**Files:**
- Modify: `pkg/session/restore_constructor.go` (signature at `:106` and core at `:121`),
  `pkg/session/restore.go` internals as needed, associated tests

**Step 1:** New signature:

```go
func Restore(ctx context.Context, cfg loop.Config, sessionID uuid.UUID,
	store *sessionstore.Store, opts ...Option) (*Session, error)
```

Internals swap `journal.NewSessionJournal/NewEventReplayer/leases.Acquire` construction
for `store.AcquireLease` / `store.OpenJournal` / `store.OpenEventReplayer`. Behavior
contract unchanged: failed restore records `RestoreErrored` and releases the lease;
success holds it.

**Step 2:** Re-point restore tests at memstore-backed `sessionstore.Open`
(`restore_integration_test.go`'s NATS server usage is deleted here; equivalent coverage
now runs un-tagged against memstore — faster and in the default suite).

**Step 3:** `GOWORK=off go test -race ./pkg/session/...` green. Commit
`refactor(session)!: Restore takes *sessionstore.Store`.

### Task B9: Strip pkg/journal to its neutral core

**Files:**
- Delete: `pkg/journal/{nats.go,objectstore.go,catalog.go,catalog_list.go,gc.go,subjects.go}`,
  the NATS impl portions of `replay.go`/`record_replay.go`/`lease.go`/`journal.go`, all
  `*_integration_test.go` in pkg/journal, `testserver_test.go`, and the now-superseded
  unit tests of deleted code (`journal_test.go` publish-seam cases, `objectstore_test.go`,
  `lease_test.go` manager cases, `catalog*_test.go`, `gc_test.go`, `subjects_test.go`)
- Keep (trim in place): `record.go`, `record_json.go`, `appender.go`, the interface
  declarations (`SessionJournal`, `EventReplayer`/`EventCursor`,
  `RecordReplayer`/`RecordCursor`, `ReplayRequest`/`StartPos`, `Lease`), the exported
  error types callers rely on (`JournalNotReadyError`, `JournalLeaseLostError`,
  `AppendError`, `AmbiguousAckError`, `MarshalRecordError`, `RecordKindError`)
- Modify: `pkg/journal/record.go` — drop `Subject()` from `JournalRecord` (replaced by the
  envelope's Kind; the only external user is `pkg/session/command_journal_test.go` — fix it)

**Step 1:** Delete/trim per above. **Step 2:** `GOWORK=off go build ./...` — chase every
compile error; nothing outside pkg/journal, pkg/session (test), and pkg/sessionstore
should need edits (verify: `grep -rn "nats" --include="*.go" pkg/ | grep -v vendor` →
only pkg/persistence remains).

**Step 3:** Full suite: `GOWORK=off go test -race ./...` green. Commit
`refactor(journal)!: remove NATS implementation; keep neutral contracts`.

### Task B10: Delete pkg/persistence, drop NATS deps, amend CLAUDE.md

**Files:**
- Delete: `pkg/persistence/` (entire package — it moves to natsstore in Phase D)
- Modify: `go.mod`, `CLAUDE.md`

**Step 1:** Delete the package. `GOWORK=off go mod tidy && GOWORK=off go mod vendor` —
verify `nats-io` is gone from go.mod and `vendor/github.com/nats-io` no longer exists.

**Step 2:** CLAUDE.md approved-deps list: remove both `nats-io` entries; add:
`github.com/looprig/storekit — leaf storage contracts (Ledger/Leaser/KV/Blobs) +
memstore + conformance suite; stdlib-only. The NATS deps moved to the ciram-co/natsstore
backend module.`

**Step 3:** `make secure` clean; `GOWORK=off go test -race ./...` green. Commit
`refactor!: drop NATS from looprig core; persistence moves to natsstore`.

---

## Phase C — fsstore module

### Task C1: Scaffold

Same shape as Task A1 at `~/code/ciram-co/fsstore`: module `github.com/looprig/fsstore`,
go 1.25.0, `require github.com/looprig/storekit` + `replace => ../storekit`, CLAUDE.md
(stdlib + storekit only), Makefile. Commit.

### Task C2: Ledger frame codec

**Files:**
- Create: `frame.go`, `frame_test.go`, `frame_fuzz_test.go`

**Step 1 (failing tests):** encode/decode round-trip; CRC flip detected; truncated header
detected; truncated payload detected; zero-length payload legal; max-size guard
(reject frames > 16 MiB — floor is 1 MiB, headroom deliberate).

**Step 2:** Implement frame `[len uint32][crc32c(payload) uint32][seq uint64][payload]`,
little-endian, `hash/crc32` Castagnoli. Decode returns a typed `FrameError` distinguishing
`Torn` (clean truncation at any byte — recoverable) from `Corrupt` (CRC/length violation
with full bytes present).

**Step 3:** `FuzzFrameDecode` — never panics, never over-reads. **Step 4:** Commit.

### Task C3: Ledger

**Files:**
- Create: `ledger.go`, `ledger_test.go`, `conformance_test.go`

**Step 1 (failing test):** `storetest.TestLedger(t, factory)` with a `t.TempDir()` root —
plus fsstore-specific cases: torn-tail truncation (append 3, truncate the file mid-frame-3,
reopen, Tip == 2, next append succeeds at 3); reopen preserves tip; name → path mapping
puts `a/b` under `streams/a/b.log` and never escapes root.

**Step 2:** Implement: one file per ledger under `<root>/streams/<name>.log` (0600, dirs
0700); per-ledger `sync.Mutex` in-process + `flock` on the file for cross-process; Append
validates expected == cached tip, writes frame, `fsync`s (file + parent dir on create);
Read streams frames up to the tip observed at open (bounded cursor); Delete removes the
file (idempotent). Never `AmbiguousError`.

**Step 3:** Conformance green with `-race`. **Step 4:** Commit `feat: fs ledger`.

### Task C4: Leaser

**Files:**
- Create: `lease.go`, `lease_unix.go`, `lease_unsupported.go`, `lease_test.go`

**Steps (TDD):** `<root>/leases/<name>.lock`: `syscall.Flock(LOCK_EX|LOCK_NB)` (build tag
`unix`, mirroring the flock pattern looprig's `pkg/persistence` used — see the
`pre-storekit-extraction` tag); epoch = counter persisted in the lock file, incremented on
each grant; held → `*LeaseHeldError`; `Release` unlocks + closes `Lost()`; unsupported
platforms fail closed at `Acquire` (typed error). OS releases the flock on process death —
that is the documented per-host scope. Cross-process contention test: spawn a helper
process (`go run ./internal/locktest`) or use `os/exec` on the test binary with a flag.
`storetest.TestLeaser` green. Commit.

### Task C5: KV + Blobs

**Files:**
- Create: `kv.go`, `kv_test.go`, `blobs.go`, `blobs_test.go`

**Steps (TDD):** KV: `<root>/kv/<key>` files, value prefixed by a rev header; CAS by
read-check-write under a per-key mutex + write-temp-then-`os.Rename`. Blobs:
`<root>/blobs/<key>`, temp+rename, `List` via sorted `filepath.WalkDir`. Both:
`storetest.TestKV` / `storetest.TestBlobs` green. Commit each.

### Task C6: Open/Options/Close + module check

**Files:**
- Create: `fsstore.go` (`Options{Root string}` required; `Open` creates the root 0700,
  validates it; `Close` releases in-process state), `README.md`

**Steps:** compile-time assertion `var _ interface { storekit.Ledger; storekit.Leaser;
storekit.KV; storekit.Blobs } = (*Store)(nil)`; full `make check`; `gosec` if available
(file-permission findings must be clean or explicitly annotated). Commit; tag `v0.1.0-rc1`.

---

## Phase D — natsstore module (port from looprig tag `pre-storekit-extraction`)

### Task D1: Scaffold + embedded engine port

**Files:**
- Create: `~/code/ciram-co/natsstore/` — go.mod (module `github.com/looprig/natsstore`,
  go 1.25.0; require storekit (replace), `nats-io/nats.go v1.52.0`,
  `nats-io/nats-server/v2 v2.14.2`), CLAUDE.md (NATS deps sanctioned HERE, moved from
  looprig), Makefile with `test-integration: GOWORK=off go test -tags integration -race ./...`
- Create: `embedded.go` — port of looprig `pkg/persistence/embedded.go` at the tag
  (Engine, EngineOptions, DontListen server, StoreDir, SyncInterval) and the flock'd
  session-dir guard from `session_engine.go` where applicable

**Steps:** copy, rename package to `natsstore`, keep the ported unit tests. Commit
`feat: embedded JetStream engine (ported from looprig pkg/persistence)`.

### Task D2: Name escaping

**Files:**
- Create: `subject.go`, `subject_test.go`

**Steps (TDD):** storekit names → JetStream-safe tokens: `/` → `.` (subject hierarchy);
`.` within a segment → `%2E` (percent-escape; `%` itself is grammar-illegal so the
escaping is unambiguous and reversible). Stream names likewise (dots illegal in stream
names → substitute `_` after escaping collisions are ruled out by the grammar). Round-trip
property test over valid names. Commit.

### Task D3: Ledger over JetStream

**Files:**
- Create: `ledger.go`, `ledger_test.go` (unit, seam-injected), `conformance_integration_test.go`
  (`//go:build integration`)

**Steps (TDD):** one stream per ledger (created on first append; `MaxMsgSize` ≥ 1 MiB
floor + envelope headroom); `Append` = `PublishMsg` with `ExpectLastSequence(expected)`;
classify: `JSErrCodeStreamWrongLastSequence` → `*storekit.ConflictError`;
`nats.ErrTimeout`/`ErrNoStreamResponse`/ctx deadline → `*storekit.AmbiguousError`; all
else definite failure. (No retry logic here — `AppendDefinite` owns that now; the old
`resolveAmbiguous`/`reconcileTip` from `pkg/journal/journal.go` is **not** ported.)
`Read` = `GetMsg` walk from `from` to the tip captured at open; `Tip` = StreamInfo last
seq; `Delete` = DeleteStream (absent → nil). Unit tests script the publish seam (port the
pattern from the tag's `journal_test.go`); integration runs
`storetest.TestLedger` + the ported ambiguous-ack integration cases against the embedded
engine. Commit.

### Task D4: Leaser, KV, Blobs

**Files:**
- Create: `lease.go` (port `pkg/journal/lease.go` LeaseManager at the tag: KV bucket,
  TTL + heartbeat, epoch bump, watch → `Lost()`), `kv.go` (JetStream KV: `Create` for
  expectedRev 0, `Update` for CAS; revision mapping is direct), `blobs.go`
  (ObjectStore: Put/Get/Delete/List — sort List output), each with unit tests +
  integration conformance

**Steps:** port, adapt to storekit signatures + error taxonomy, run
`storetest.TestLeaser/TestKV/TestBlobs` under the integration tag. Commit each primitive.

### Task D5: Options/Open + full integration pass

**Files:**
- Create: `natsstore.go` — `Options{URL string, EmbeddedDir string}` (exactly one set;
  embedded mode owns an `Engine`), `Open`, `Close` (drain + shutdown, port ordering from
  `SessionEngine.Close`), compile-time all-four assertion; `README.md`

**Steps:** `GOWORK=off go test -race ./...` (unit) and
`GOWORK=off go test -tags integration -race ./...` (embedded server) both green. Commit;
tag `v0.1.0-rc1`.

---

## Phase E — swe wiring + release

### Task E1: swe composition root

**Files (discover first):** in `~/code/swe`, run
`grep -rn "persistence\.\|journal\.NewSessionJournal\|session.Restore\|OpenSessionEngine" --include="*.go" . | grep -v vendor`

**Steps:** add `fsstore` + `storekit` deps (workspace-on for swe per repo convention —
check `~/code/go.work`); replace the `persistence.SessionStoreRoot`/engine wiring with
`fsstore.Open(fsstore.Options{Root: <swe's existing data dir>/store})` +
`sessionstore.Open(fs)`; update every `session.Restore` call site to the new signature.
The data-dir default (`~/.looprig/...` previously) now lives HERE — pick swe's existing
config surface. Run swe's full suite. Commit in swe.

### Task E2: Release + de-replace

**Steps:**
1. Tag `storekit v0.1.0`, `fsstore v0.1.0`, `natsstore v0.1.0` (push if remotes exist;
   `GOPRIVATE=github.com/ciram-co`).
2. looprig: remove the `replace`, `require github.com/looprig/storekit v0.1.0`,
   `GOWORK=off go mod tidy && go mod vendor`, `make secure`, full `-race` suite.
3. fsstore/natsstore: same de-replace against storekit v0.1.0.
4. Merge `feat/storekit-extraction`; tag looprig `v0.5.0`.
5. Update the memory/plan docs checklist: extraction complete.

---

## Verification (whole plan)

- looprig: `make secure && GOWORK=off go test -race ./...` — zero NATS in `go.mod`.
- storekit/fsstore: `make check`; natsstore: unit + `-tags integration` suites.
- Cross-backend sanity: one looprig test (`pkg/sessionstore/crossbackend_test.go`,
  integration-tagged) running the B4/B5 journal+replay round-trip against fsstore via its
  replace path — proves the facade is backend-agnostic in practice, not just via memstore.
