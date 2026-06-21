# Event Persistence & Checkpoint/Restore — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan
> task-by-task. Use @superpowers:test-driven-development for every task (test → red → impl →
> green → commit).

**Goal:** Durably persist a session's Enduring events + commands to NATS JetStream so a
stopped session can be brought back (primary-loop restore) and the TUI repainted.

**Architecture:** Event sourcing — the Enduring event stream is the source of truth; `msgs`
and `turnIndex` are folds over it. One JetStream **stream per session** (subjects
`…session`, `…loop.<lid>.event`, `…loop.<lid>.cmd`, `…fence`). A single per-session
`SessionJournal` serializer owns a stream-level expected-sequence fence; the **hub** appends
every Enduring event through it (append-before-apply, fail-secure) and the **Session
boundary** appends commands. Restore folds the primary loop's events, rebuilds `msgs` +
`turnIndex`, and brackets itself with `Restore*` events. CLI = embedded `nats-server` (no
TCP). See `docs/plans/2026-06-19-event-persistence-checkpoint-design.md` (the **v1 spec**)
for the full rationale; this plan references its sections by name.

**Tech Stack:** Go 1.26.4, module `github.com/inventivepotter/urvi`. New deps:
`github.com/nats-io/nats.go`, `github.com/nats-io/nats-server/v2` (embedded). Build
`CGO_ENABLED=0 go build -trimpath`; test `go test -race ./...`; integration
`go test -tags integration -race ./...`; `make fmt` + `make secure` before commits.

**Conventions (CLAUDE.md):** table-driven tests with `t.Parallel()`; typed errors only (no
bare `errors.New`/`fmt.Errorf` from package APIs); strict typing (no `any` past
serialization boundaries); interface-first; commit messages **short subject, no co-author
trailer**.

**Repo facts (discovered in Task 0.1):**
- The module is **vendored** (`vendor/`). After any dependency change run `go mod vendor`;
  builds/tests use the vendored tree (`-mod=vendor`). Most of Task 0.1's diff is vendored
  NATS source — expected.
- `go mod tidy` prunes deps with no importer, and a custom build tag (`integration`) does
  **not** anchor a dep for `tidy`. So a **temporary anchor** `internal/agent/session/journal/deps.go`
  blank-imports both NATS packages. Remove the `nats.go` blank-import when Phase 4's journal
  production code imports it for real; remove the `nats-server/v2/server` blank-import (and
  delete the file) when Phase 10 wires the embedded server in `cmd/cli`. **The `journal`
  package must NOT permanently import `nats-server/v2/server`** — the embedded server is a
  composition-root (cmd/cli) concern; journal takes a `nats.JetStreamContext`.

**Scope reminder (v1):** local CLI only; **primary loop restore only**; cloud backend,
multi-loop/subagent restore, at-rest encryption, and snapshots are **out of scope** (seams
only). Do not implement them.

---

## Phase 0 — Dependencies & scaffolding

### Task 0.1: Approve + add NATS deps; amend CLAUDE.md

**Files:**
- Modify: `go.mod`, `go.sum`
- Modify: `CLAUDE.md` (sanctioned-deps list)

**Step 1:** Add the deps:
```bash
go get github.com/nats-io/nats.go@latest
go get github.com/nats-io/nats-server/v2@latest
go mod tidy
```
**Step 2:** Append to the CLAUDE.md "Approved external packages" list:
```
- `github.com/nats-io/nats.go` — JetStream client (pub/sub, JetStream ctx, KV, object store,
  durable consumers); required by internal/agent/session/journal for session persistence.
- `github.com/nats-io/nats-server/v2` — embedded in-process JetStream server (no TCP) for
  the CLI's local durable journal; the client alone persists nothing.
```
**Step 3:** Verify build: `CGO_ENABLED=0 go build -trimpath ./...` → success.
**Step 4:** Commit:
```bash
git add go.mod go.sum CLAUDE.md
git commit -m "build: add nats.go + embedded nats-server (session journal); sanction in CLAUDE.md"
```

### Task 0.2: Embedded-server test harness (integration helper)

Used by every integration test. Embedded server, no TCP, temp StoreDir, in-process conn.

**Files:**
- Create: `internal/agent/session/journal/testserver_test.go` (helper; `//go:build integration`)

**Step 1:** Write the helper + a smoke test:
```go
//go:build integration

package journal_test

import (
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// newEmbeddedJS starts an in-process JetStream server (no TCP) on a temp StoreDir and
// returns a connected client. Everything is torn down via t.Cleanup.
func newEmbeddedJS(t *testing.T) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		JetStream:  true,
		StoreDir:   t.TempDir(),
		DontListen: true, // no TCP socket
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	return nc, js
}

func TestEmbeddedServerSmoke(t *testing.T) {
	_, js := newEmbeddedJS(t)
	if _, err := js.AddStream(&nats.StreamConfig{Name: "S", Subjects: []string{"s.>"}}); err != nil {
		t.Fatalf("AddStream: %v", err)
	}
}
```
**Step 2:** Run: `go test -tags integration -race ./internal/agent/session/journal/ -run TestEmbeddedServerSmoke -v` → PASS.
**Step 3:** Commit: `git commit -am "test(journal): embedded JetStream harness (no TCP, temp StoreDir)"`

---

## Phase 1 — Event factory: `EventID` + `CreatedAt` at creation (spec B2 + Timestamps)

> **Why first:** an event with no stable `EventID` is not persistable. Today only
> `LoopStarted` mints one. Read `internal/agent/loop/event/event.go` (Header) and
> `identity/identifier_types.go` first.

### Task 1.1: Add `Header.CreatedAt`

**Files:**
- Modify: `internal/agent/loop/event/event.go` (Header struct)
- Test: `internal/agent/loop/event/header_test.go`

**Step 1 (test):** add to the header test table a case asserting `Header{CreatedAt: ts}` round-trips through the (existing) header JSON and `EventHeader().CreatedAt == ts`.
**Step 2:** Run → FAIL (no field).
**Step 3:** Add `CreatedAt time.Time \`json:"created_at,omitzero"\`` to `Header`.
**Step 4:** Run → PASS. `make fmt`.
**Step 5:** Commit: `git commit -am "feat(event): add Header.CreatedAt (stamped at creation)"`

### Task 1.2: `Clock` + `Factory` (mint EventID + CreatedAt)

**Files:**
- Create: `internal/agent/loop/event/factory.go`
- Test: `internal/agent/loop/event/factory_test.go`

**Step 1 (test):**
```go
func TestFactoryStamp(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	ids := []uuid.UUID{uuid.MustParse("..."), uuid.MustParse("...")}
	i := 0
	f := event.NewFactory(func() uuid.UUID { id := ids[i]; i++; return id }, func() time.Time { return ts })
	h := f.NewHeader() // mints EventID + CreatedAt
	if h.EventID != ids[0] || !h.CreatedAt.Equal(ts) {
		t.Fatalf("got %+v", h)
	}
}
```
**Step 2:** Run → FAIL.
**Step 3 (impl):**
```go
package event

// Clock and IDGen are injected so tests are deterministic (mirrors session.idGenerator).
type Clock func() time.Time
type IDGen func() uuid.UUID

type Factory struct { newID IDGen; now Clock }

func NewFactory(newID IDGen, now Clock) *Factory { return &Factory{newID: newID, now: now} }

// NewHeader mints a fresh EventID + CreatedAt. Callers fill Coordinates/Cause.
func (f *Factory) NewHeader() Header { return Header{EventID: f.newID(), CreatedAt: f.now()} }
```
**Step 4:** Run → PASS.
**Step 5:** Commit: `git commit -am "feat(event): Factory mints EventID + CreatedAt at creation"`

### Task 1.3: Mint at every Enduring creation site

**Files (read then modify):**
- `internal/agent/session/session.go` (`SessionStarted` @ ~413; `LoopStarted` @ ~330 already mints — unify on Factory)
- `internal/agent/loop/loop.go` (`publish`/`stampLoopHeader` @ ~367; commit arm @ ~954)

**Step 1 (test):** in `session` and `loop` tests, assert every published Enduring event has a
non-zero `EventID` and `CreatedAt` (table over the event types each produces). Inject a
deterministic `Factory`.
**Step 2:** Run → FAIL (SessionStarted/loop events have zero EventID).
**Step 3 (impl):** thread a `*event.Factory` into `Session` (construct in `New`, store on the
struct, replace the ad-hoc `LoopStarted` minting) and into the loop config; have
`stampLoopHeader` and the commit-arm publish set `EventID`+`CreatedAt` from the factory.
Keep `newID` injection points wired to the same generator.
**Step 4:** Run → `go test -race ./internal/agent/...` PASS.
**Step 5:** Commit: `git commit -am "feat(session,loop): mint EventID+CreatedAt for every Enduring event via Factory"`

---

## Phase 2 — New event/record types (spec: Restore*, LeaseFence, config fingerprint)

### Task 2.1: `RestoreStarted` / `RestoreDone` / `RestoreErrored`

**Files:**
- Modify: `internal/agent/loop/event/event.go` (add three session-scoped Enduring types)
- Modify: `internal/agent/loop/event/doc.go` (compile-time `var _ Event = …` assertions)
- Test: `internal/agent/loop/event/header_test.go` (class + scope table)

**Step 1 (test):** extend `TestEventClass`/`TestEventScope` tables: the three are `Enduring`,
session-scoped, not terminal; `RestoreErrored` carries an error projected to a string
(`json:"-"` raw error, see RestoredError in Phase 3).
**Step 2:** Run → FAIL.
**Step 3 (impl):** mirror `SessionStarted` (embed `enduring`, `sessionScoped`, `Header`).
`RestoreErrored{ Header; Err error \`json:"-"\` }`. Add the `var _ Event` lines to `doc.go`.
**Step 4:** Run → PASS.
**Step 5:** Commit: `git commit -m "feat(event): add RestoreStarted/RestoreDone/RestoreErrored (Enduring, session-scoped)"`

### Task 2.2: Config fingerprint on `SessionStarted`

**Files:**
- Modify: `internal/agent/loop/event/event.go` (`SessionStarted` gains `Config ConfigFingerprint`)
- Create: `internal/agent/loop/event/config_fingerprint.go` (`ConfigFingerprint{AgentKind, ModelID, SystemPromptRev, ToolPolicyRev string}`)
- Test: `…/config_fingerprint_test.go`

**Step 1 (test):** `ConfigFingerprint` round-trips JSON; `Equal` returns false on any field
diff (table: same, model differs, prompt differs, tool-policy differs, agent differs).
**Step 2:** Run → FAIL. **Step 3:** implement struct + `Equal`. **Step 4:** PASS.
**Step 5:** Commit: `git commit -m "feat(event): ConfigFingerprint on SessionStarted (restore compatibility)"`

### Task 2.3: `LeaseFence{Epoch}` journal record

> Not an `event.Event` — a `JournalRecord` (Phase 4). Define the type here, near the journal
> package, once that package exists. **Defer the type to Task 4.2** to avoid a forward ref.

---

## Phase 3 — Codecs (spec: Serialization)

> Read `internal/content/block_json.go` (`UnmarshalBlock`/`MarshalBlock`, the existing tagged
> union) and `internal/content/message.go` (Message JSON) first — the event codec **delegates**
> to these. Read `internal/agent/loop/event/marshal_test.go` (notes whole-event round-trip is
> infeasible today — this phase fixes that for the Enduring set).

### Task 3.1: `RestoredError` (typed, round-trippable)

**Files:**
- Create: `internal/agent/loop/event/restored_error.go`
- Test: `…/restored_error_test.go`

**Step 1 (test):**
```go
func TestRestoredError(t *testing.T) {
	t.Parallel()
	e := &event.RestoredError{Kind: "tool_timeout", Message: "deadline exceeded"}
	if e.Error() != "tool_timeout: deadline exceeded" { t.Fatal(e.Error()) }
	b, _ := json.Marshal(e)
	var got event.RestoredError
	if err := json.Unmarshal(b, &got); err != nil || got != *e { t.Fatalf("%v %+v", err, got) }
}
```
**Step 2:** FAIL. **Step 3:**
```go
type RestoredError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}
func (e *RestoredError) Error() string { return e.Kind + ": " + e.Message }
// ErrKind(err) maps a concrete error to a stable kind string (errors.As switch).
```
**Step 4:** PASS. **Step 5:** Commit: `git commit -m "feat(event): RestoredError {kind,message} for TurnFailed.Err persistence"`

### Task 3.2: Sealed `tool.PermissionRequest` codec (spec B4 — persist in full)

**Files (read first):** `internal/tool/permission_request.go` (the sealed interface + concrete
types Bash/FileWrite/Unknown/… and `ToolName()/Description()/AllowedScopes()`).
- Create: `internal/tool/permission_request_json.go` (`MarshalPermissionRequest`/`UnmarshalPermissionRequest`, tagged by a `type` discriminator)
- Test: `…/permission_request_json_test.go` (+ `FuzzUnmarshalPermissionRequest`)

**Step 1 (test):** table over every concrete request type: marshal→unmarshal yields a value
whose `ToolName()/Description()/AllowedScopes()` equal the original; unknown tag → typed
`UnknownPermissionRequestError`; nil → typed error (fail-closed, mirror `blockTag`).
**Step 2:** FAIL. **Step 3:** implement the tagged union (mirror `block_json.go`). **Step 4:**
PASS; add `FuzzUnmarshalPermissionRequest`, run `go test -fuzz=Fuzz -fuzztime=30s`.
**Step 5:** Commit: `git commit -m "feat(tool): sealed PermissionRequest JSON codec (full-fidelity persistence)"`

### Task 3.3: `MarshalEvent`/`UnmarshalEvent` — Enduring-only, rejects Ephemeral (spec M9)

**Files:**
- Create: `internal/agent/loop/event/marshal.go`
- Test: `internal/agent/loop/event/marshal_test.go` (extend) + `FuzzDecodeEvent`

**Step 1 (test):** table over **every Enduring event type** (`StepDone`, `TurnStarted`,
`TurnFoldedInto`, `InputCancelled`, `TurnRejected`, `LoopIdle`, `LoopStarted`, `Session*`,
`Restore*`, `PermissionRequested` (full `Request`), `UserInputRequested`, terminals
`TurnDone`/`TurnFailed`(→`RestoredError`)/`TurnInterrupted`): `UnmarshalEvent(MarshalEvent(e))`
deep-equals `e` (with `TurnFailed.Err` compared as `RestoredError`). Separate table: **each
Ephemeral type** (`TokenDelta`, `ToolCallStarted`, `ToolCallCompleted`, `InputQueued`) →
`MarshalEvent` returns `*EphemeralNotPersistableError`.
**Step 2:** Run → FAIL.
**Step 3 (impl):** a wire envelope `{ "type": <tag>, ...header..., ...payload... }`. Dispatch
on concrete type for the tag; **guard at the top**: `if ev.Class() == Ephemeral { return nil,
&EphemeralNotPersistableError{Type: tag} }`. Delegate message/block payloads to
`content.MarshalBlocks`/`Message` codecs, `PermissionRequested.Request` to Task 3.2,
`TurnFailed.Err` to a `RestoredError` projection (`ErrKind`). `UnmarshalEvent` switches on
`type`. Unknown tag → typed `UnknownEventTypeError`.
**Step 4:** Run → PASS; add `FuzzDecodeEvent`, run 30s.
**Step 5:** Commit: `git commit -m "feat(event): MarshalEvent/UnmarshalEvent (Enduring-only; rejects Ephemeral)"`

### Task 3.4: `MarshalCommand`/`UnmarshalCommand`

**Files (read first):** the `command` package concrete types (`UserInput`, `ApproveToolCall`,
`DenyToolCall`, `ProvideUserInput`, `SubagentResult`, `CancelQueuedInput`, `Interrupt`,
`Shutdown`) — note ack channels are **not** serialized.
- Create: `internal/agent/loop/command/marshal.go`
- Test: `…/marshal_test.go` + `FuzzDecodeCommand`

**Step 1 (test):** table over every concrete command: round-trips its serializable fields
(`UserInput` blocks via the block codec); ack-channel fields are absent from the wire and
nil after unmarshal; unknown tag → typed error.
**Step 2:** FAIL. **Step 3:** tagged union mirroring the event codec. **Step 4:** PASS + fuzz.
**Step 5:** Commit: `git commit -m "feat(command): MarshalCommand/UnmarshalCommand (intent log codec)"`

---

## Phase 4 — `SessionJournal`: the single fenced serializer (spec: durable tap, fencing, ambiguous-ack)

> New package `internal/agent/session/journal`. Everything in this phase is integration-tagged
> (real embedded server). Read the design's *The durable tap* and *Idempotency* sections.

### Task 4.1: Stream + subject layout; `JournalRecord` sum

**Files:**
- Create: `internal/agent/session/journal/subjects.go` (subject builders for `.session`,
  `.loop.<lid>.event`, `.loop.<lid>.cmd`, `.fence`; stream name `urvi_session_<sid>`)
- Create: `internal/agent/session/journal/record.go` (`JournalRecord` sealed sum: event |
  command | `LeaseFence{Epoch uint64}`; each carries its idempotency id + maps to a subject)
- Test: `…/subjects_test.go` (table: each record kind → expected subject; round-trip parse)

**Steps:** TDD the pure subject/record mapping (unit, no server). Commit:
`git commit -m "feat(journal): stream/subject layout + JournalRecord sum (event|command|LeaseFence)"`

### Task 4.2: `LeaseFence{Epoch}` type + codec

**Files:** `…/record.go` (+ `…/record_json.go`), test `…/record_json_test.go`.
TDD round-trip of `LeaseFence{Epoch}`. Commit:
`git commit -m "feat(journal): LeaseFence{epoch} record + codec"`

### Task 4.3: Fenced append (happy path) — `SessionJournal.Append`

**Files:**
- Create: `internal/agent/session/journal/journal.go` (the serializer: one goroutine/mutex;
  holds `expectedSeq`; `Append(ctx, rec) (seq, error)`)
- Create: `internal/agent/session/journal/nats.go` (NATS-backed publish with
  `Nats-Expected-Last-Sequence` + `Nats-Msg-Id`; per-append deadline)
- Test: `…/journal_integration_test.go` (`//go:build integration`)

**Step 1 (test):** create the per-session stream; append a sequence of records (events on
their subjects, a command, the leading `LeaseFence`); assert returned `seq` is strictly
monotonic and `StreamInfo.LastSeq` matches; read them back by sequence and decode.
**Step 2:** FAIL. **Step 3:** implement: serialize through one mutex; set
`Nats-Expected-Last-Sequence = expectedSeq` and `Nats-Msg-Id = rec.id()`; on `PubAck` advance
`expectedSeq = ack.Sequence`; per-append `context.WithTimeout`.
**Step 4:** PASS.
**Step 5:** Commit: `git commit -m "feat(journal): fenced SessionJournal.Append (stream-level expected-seq)"`

### Task 4.4: Stale-writer rejection (fencing)

**Step 1 (test):** two `SessionJournal`s over the same stream; A appends (advances seq); B
(stale `expectedSeq`) appends → expected-sequence error; B does **not** retry-advance.
**Step 2–4:** TDD. **Step 5:** `git commit -m "test(journal): stale writer is fenced by expected-seq"`

### Task 4.5: Bounded append + ambiguous-ack resolution (spec round 5)

**Step 1 (test):** simulate a timed-out/ambiguous ack (inject a publish wrapper that reports
timeout but actually stored). Resolver must: retry same `Nats-Msg-Id`+`expected N` →
(a) `PubAck.Duplicate=true` → verify `N+1` id/hash, advance; (b) `Duplicate=false` → advance;
(c) expected-seq conflict → direct-get `N+1`, compare id+hash → ours advance / not-ours
`SessionPersistenceFault`. Cover all three rows.
**Step 2–4:** implement `resolveAmbiguous(...)` per the design's algorithm.
**Step 5:** Commit: `git commit -m "feat(journal): ambiguous-ack resolution via PubAck.Duplicate + direct-get N+1"`

---

## Phase 5 — Record size + Object-Store offload + orphan-GC (spec: Record size)

### Task 5.1: Server/stream size config + record-size guard

**Files:** `…/nats.go` (set `max_payload` + stream `MaxMsgSize` to the inline ceiling; reject
oversized inline records with a typed `RecordTooLargeError` unless offloaded).
TDD (integration): a record above the inline threshold but not offloaded → typed error.
Commit: `git commit -m "feat(journal): inline record-size ceiling + RecordTooLargeError"`

### Task 5.2: Content-addressed object offload (upload-before-event)

**Files:** `…/objectstore.go` (sha256 object id; `Put` idempotent; reference
`{ObjectID, Length, CodecVersion}` embedded into the record).
**Step 1 (test, integration):** a `StepDone` whose block bodies exceed the threshold → bodies
uploaded to the JetStream Object Store **before** the event append; the stored event carries
references; restore re-hydrates byte-for-byte.
**Steps 2–5:** TDD. Commit: `git commit -m "feat(journal): content-addressed object offload for oversized records"`

### Task 5.3: Missing/corrupt object → fail-secure; orphan-GC under lease

**Step 1 (tests, integration):** (a) delete/corrupt an object → restore returns
`RestoreErrored` (does not come up); (b) an object uploaded then orphaned (event append
fails) is reaped by GC **only under the lease** and **only past the grace period**; a
referenced object is never reaped.
**Steps 2–5:** implement `GC(ctx)` (lease-guarded; grace > max retry interval). Commit:
`git commit -m "feat(journal): fail-secure on missing/corrupt object; lease-guarded orphan-GC"`

---

## Phase 6 — KV lease + `LeaseFence` handover (spec: Lease handover)

### Task 6.1: KV session lease (TTL + monotonic fencing token/epoch)

**Files:** `…/lease.go`, test `…/lease_integration_test.go`.
**Step 1 (test):** `Acquire` returns a monotonically increasing epoch across successive
acquisitions; a second concurrent `Acquire` fails while the first holds; TTL expiry releases.
**Steps 2–5:** implement over a JetStream KV bucket (`urvi_session_leases`). Commit:
`git commit -m "feat(journal): KV session lease with monotonic fencing epoch"`

### Task 6.2: `LeaseFence` as the owner's first append; gate on ack

**Files:** `journal.go` (`Open`/takeover writes `LeaseFence{epoch}` first; refuses further
appends if a later fence with a higher epoch is observed; old owner stops refreshing
`expectedSeq` after lease loss).
**Step 1 (test, integration):** owner B takes the lease (epoch+1), writes `LeaseFence`; a
stale owner A append now fails the fence **on any subject**; B does not start loop work until
the `LeaseFence` ack.
**Steps 2–5:** TDD. Commit: `git commit -m "feat(journal): LeaseFence handover boundary (first append, gates loop start)"`

---

## Phase 7 — Hub durable tap + Session-boundary command append (spec B1/B3 + hub algorithm)

> Read `internal/agent/session/hub/hub.go` (`PublishEvent` @ ~80, the synth of
> `SessionActive/Idle`, `StopSession`) and `subscription.go`.

### Task 7.1: `SessionPersistenceFault` + fault reporter

**Files:** `internal/agent/session/hub/fault.go`, test `fault_test.go`.
TDD a typed `SessionPersistenceFault` and a `FaultReporter` interface (the hub calls it; the
Session implements it to reject new `Submit`/`NewLoop` and wake `WaitIdle` waiters).
Commit: `git commit -m "feat(hub): SessionPersistenceFault + FaultReporter"`

### Task 7.2: Inject `SessionJournal` + `Factory` + reporter into the hub

**Files:** `hub/hub.go` (constructor `New` gains these deps), `session.go` (wire them).
TDD (unit, fake journal): constructing the hub stores the deps; no behavior change yet.
Commit: `git commit -m "refactor(hub): inject SessionJournal, event Factory, FaultReporter"`

### Task 7.3: Append-before-apply for direct Enduring events

**Step 1 (test, unit w/ fake journal):** publishing an Enduring event **appends before**
fan-out; publishing an Ephemeral event **does not** append; an `Append` error → no live
fan-out + `SessionPersistenceFault` raised (reporter called).
**Steps 2–4:** implement in `PublishEvent`: `if ev.Class()==Enduring { append; if err →
fault+return }`; then fan out. Ephemeral → fan out only.
**Step 5:** Commit: `git commit -m "feat(hub): append Enduring before fan-out; fault on append error"`

### Task 7.4: Derived session events — create→append→deliver, ordered

**Step 1 (test):** a trigger that flips Idle→Active appends the **trigger first**, then
**creates (Factory) + appends + delivers** `SessionActive`, in that order; if either append
fails, **neither** is delivered live and a fault is raised.
**Steps 2–5:** implement in the quiescence transition path. Commit:
`git commit -m "feat(hub): derived session events create-append-deliver in causal order"`

### Task 7.5: Session-boundary command append

**Files:** `session.go` (`Submit`/`Interrupt`/`Shutdown`/… build a `CommandRecord` and call
`CommandAppender.Append` **before** dispatch; **audit-only** — log + proceed on error).
**Step 1 (test):** each command path appends a `CommandRecord` with the right
`TargetLoopID`/`Agency`/`CommandID`; an append error is logged but dispatch proceeds (assert
the command still reaches the loop). `Interrupt`/`Shutdown` append **one record per loop**.
**Steps 2–5:** TDD. Commit: `git commit -m "feat(session): append commands to the intent log (audit-only fail handling)"`

---

## Phase 8 — `Restore` constructor (primary loop only) (spec: Restore)

### Task 8.1: Fold primary loop events → `msgs` + `turnIndex`

**Files:** `internal/agent/session/restore.go` (fold helper, pure over a slice of events),
test `restore_fold_test.go` (unit).
**Step 1 (test):** table of event sequences → expected `(msgs, turnIndex)`:
`TurnStarted`+`StepDone`s → user msg + step groups; `turnIndex` == count of `TurnStarted`;
`TurnFoldedInto` folds at the fold point; lifecycle events are no-ops; `SystemMessage`
re-seeded from config (passed in).
**Steps 2–5:** implement the fold. Commit: `git commit -m "feat(session): fold primary-loop events into msgs + turnIndex"`

### Task 8.2: Crash-seam + interrupted-turn visible result

**Step 1 (test):** a history ending with `TurnStarted` and no terminal → restore appends a
`TurnInterrupted`; folded result = committed user message + interruption marker, **no partial
assistant step**. A history ending at a clean `StepDone`/`TurnDone` → no synthetic event.
**Steps 2–5:** TDD. Commit: `git commit -m "feat(session): restore crash-seam (open turn → TurnInterrupted)"`

### Task 8.3: `Restore(ctx, cfg, sessionID, journal, replayer)` end-to-end (integration)

**Step 1 (test, integration):** stream a session, tear down, `Restore`: reuses `SessionID` +
primary `LoopID`; **config-fingerprint mismatch → rejected/confirm path**; in-log order is
`LeaseFence → RestoreStarted → … → TurnInterrupted? → RestoreDone`; loop comes up **idle**;
queued-but-unstarted `InputQueued` is **not** resumed.
**Steps 2–5:** implement the constructor (fingerprint check → lease+`LeaseFence` →
`RestoreStarted` → fold → crash-seam → `RestoreDone`/`RestoreErrored` → idle). Commit:
`git commit -m "feat(session): Restore constructor — primary-loop, fingerprint-checked, bracketed"`

---

## Phase 9 — Catalog (KV) + lazy listing (spec: Session catalog)

### Task 9.1: `SessionMeta` + inline post-append update

**Files:** `…/catalog.go` (KV bucket `urvi_sessions`; `SessionMeta`), wired into the journal
serializer (best-effort update **after** a catalog-relevant append).
**Step 1 (test, integration):** appending `SessionStarted`/first `TurnStarted`/`RestoreDone`/
`SessionStopped` updates the KV record (upsert, Title, LastActiveAt, Status); a KV-write error
does not fail the journal append.
**Steps 2–5:** TDD. Commit: `git commit -m "feat(journal): derived KV session catalog (post-append, best-effort)"`

### Task 9.2: Lazy listing + startup repair

**Step 1 (test, integration):** `ListSessions()` reads only the KV bucket (assert **no
consumer** is created — e.g. `StreamInfo`/consumer count unchanged); deleting the catalog then
`RepairCatalog()` rebuilds it from stream metadata.
**Steps 2–5:** TDD. Commit: `git commit -m "feat(journal): lazy session listing + startup catalog repair"`

---

## Phase 10 — TUI replay-repaint + composition wiring (spec: Restore modes (a))

### Task 10.1: Background fold → render once

**Files (read first):** `tui/screen.go` (subscribe @ ~123, `handleEvent`), `tui/transcript.go`
(`ApplyEvent`), `tui/interaction.go`.
- Create: `tui/restore.go` (a `tea.Cmd` that drains an `EventReplayer` backlog off the update
  loop, folds via `transcript.ApplyEvent`/`interaction.ApplyEvent`, returns one `restoredMsg`)
**Step 1 (test):** a fake replayer yielding N events → the `tea.Cmd` produces one
`restoredMsg` with the final reducer state; `transcript`/`interaction` reducers are applied
exactly once per event; backlog does **not** go through the 256-buffer subscription.
**Steps 2–5:** TDD. Commit: `git commit -m "feat(tui): background backlog fold → single restored render"`

### Task 10.2: Cold-restore handoff (mode a) + repaint into scrollback

**Step 1 (test):** after `restoredMsg`, the screen flushes the rebuilt transcript to
scrollback once, then attaches the live hub subscription; no double-render across the
provisional→committed boundary.
**Steps 2–5:** TDD against the tea harness (`tui/teaharness_test.go`). Commit:
`git commit -m "feat(tui): cold-restore repaint then live hub handoff"`

### Task 10.3: Wire the journal at the composition root

**Files:** `cmd/cli/main.go`, `agents/coding/agent.go` (embedded server lifecycle: start in
`coding.New`/main, `DontListen`, StoreDir under the user data dir `0700`, conservative
`SyncInterval`; build the `SessionJournal`/hub/replayer; pass `Factory`).
**Step 1 (test, integration):** a CLI-shaped wiring test constructs a `Session` backed by the
embedded journal, runs a turn, restarts, and `Restore`s — asserting the headline property
(below) end-to-end.
**Steps 2–5:** wire it. Commit: `git commit -m "feat(cli): wire embedded JetStream journal + Restore into the session"`

---

## Phase 11 — End-to-end property tests (spec: Testing — headline, scoped)

### Task 11.1: Quiescent history → exact equality

**Step 1 (test, integration):** stream a clean multi-turn session → graceful stop → `Restore`
→ `msgs` **and** transcript **exactly identical**.
**Steps 2–5:** make it pass (should already, by construction). Commit:
`git commit -m "test(e2e): quiescent restore is exactly identical"`

### Task 11.2: Mid-step kill → durable projection

**Step 1 (test, integration):** stream a turn, `kill` mid-step (drop the in-flight step), →
`Restore` → restored transcript equals the **durable Enduring projection** (committed user
message + interruption marker, no partial assistant step, no ephemeral) — **not** the
pre-crash live view; restored `msgs` ends at the last committed `StepDone`.
**Steps 2–5:** TDD. Commit: `git commit -m "test(e2e): mid-step kill restores to the durable projection"`

### Task 11.3: Full verification gate

**Steps:**
- `make fmt-check`
- `go test -race ./...`
- `go test -tags integration -race ./...`
- `go test -fuzz=FuzzDecodeEvent -fuzztime=30s ./internal/agent/loop/event/`
- `make secure`

Use @superpowers:verification-before-completion — paste the actual output before claiming
done. Commit any fixups. Then @superpowers:finishing-a-development-branch to decide merge/PR.

---

## Out of scope — do NOT implement (seams only)

Cloud backend / clustering / at-rest encryption (`PayloadCipher`); multi-loop & subagent
restore; snapshots + head-trim; explicit session deletion; cross-worker subagent routing;
live-attach mode (b) (`Follow:true`). Leave the interfaces shaped for them per the spec, but
build none.
