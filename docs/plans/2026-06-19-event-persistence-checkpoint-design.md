# Event Persistence & Checkpoint/Restore Design

**Date:** 2026-06-19 · **Last revised:** 2026-06-21 (review round 2) · **Status:** Draft
**Depends on:** `docs/plans/loop-machine-design.md` — the landed event model this layer
persists: the `Ephemeral`/`Enduring` classes, `StepDone` (finalized `AIMessage` + its
`ToolResultMessage`s), the turn/input lifecycle (`TurnStarted`/`TurnFoldedInto`/
`InputCancelled`/`TurnRejected`), session/loop lifecycle (`SessionStarted`/`SessionActive`/
`SessionIdle`/`SessionStopped`/`LoopIdle`/`LoopStarted`), the `Header`
(`Coordinates{SessionID,LoopID,TurnID,StepID}`, `EventID`, `Cause`), and the session hub.
**Composes with:** `docs/plans/2026-06-18-tui-event-adoption-design.md` — the TUI as a
projection of Enduring events via `ApplyEvent`; restore reuses that projection.

> **Provenance.** This is a clean rewrite (2026-06-21) after validating against `main`
> @ `fc93ee4` and two review rounds; earlier per-amendment history is in git
> (`design/event-persistence-checkpoint`). It states the settled **v1** design once.
> v1 = local CLI, **primary-loop restore**; cloud and multi-loop restore are gated/deferred.

## Motivation

There is **zero persistence** today: a loop's committed conversation (`loopState.msgs`)
lives only in memory and dies with the process. The loop-machine spec left this open and
documents the hub as the attach point (*"Consumers (TUI/CLI now, a durable journal
later) attach here."*). This builds that journal so a session survives process exit and
can be **brought back to continue**, and so the TUI can **repaint** from the durable
record on restore.

Substrate: **NATS JetStream** — embedded `nats-server/v2` (in-process, FileStore) for the
CLI; a JetStream cluster for cloud. One client API whether bytes live in a local file or a
cluster; durable, sequenced, replayable log with idempotent append. (Sizes measured
2026-06-21: embedding adds ~19 MB to the binary under `-trimpath` and ~18 MB idle RSS —
accepted.)

## Scope

**In scope (v1):** the durable Enduring-event journal + the command (intent) log; per-record
creation timestamps; **primary-loop** checkpoint/restore of `loopState.msgs`; TUI
replay-repaint (background fold, no UI hang); the session catalog + lazy listing; the
single-writer lease + fencing; retention policy; **local CLI** wiring.

**Out of scope / gated (named, not built):**
- **Cloud backend is gated**, not merely deferred: it stays off until **at-rest encryption**
  (the `PayloadCipher` seam) **and per-session authorization** land. The journal holds
  prompts, tool inputs, and results; shared-infra storage without both is unacceptable.
- **Multi-loop / subagent restore** — v1 reconstructs the **primary loop only** (rationale
  below). Live subagent resumption needs a persisted loop-definition + permission snapshot.
- **Snapshots** — dormant `SnapshotStore` seam; added only when replay-from-zero is
  measurably slow.
- **Explicit session deletion**, cross-worker subagent routing, deep subagent transcript
  nesting — separate follow-ons (the subject namespace is chosen to enable them).

## Dependency approval

First heavy external deps (approved; **amend `CLAUDE.md` at implementation time**, in the
same change that adds them to `go.mod`):
- `github.com/nats-io/nats.go` — client (pub/sub, JetStream, KV, durable consumers).
- `github.com/nats-io/nats-server/v2` — embedded in-process for the CLI (JetStream
  durability lives here; the client alone persists nothing).

## The spine — route by event class

- **Enduring → durable + live.** Every Enduring event is appended to JetStream (the
  durable journal and the cross-process transport) and fanned out live.
- **Ephemeral → live only.** `TokenDelta`, `ToolCall*`, `InputQueued` stay on the
  in-memory hub fan-out for whatever client is attached now; **never persisted**. Dropped
  ones self-heal at the next `StepDone`. In headless mode they go nowhere.

The loop never learns NATS exists; it emits through the hub. The hub owns the durable tap
(below). NATS is wired only at the composition root (Dependency Inversion).

### Stream & subject topology

One JetStream **stream per session**. Subjects:
- `urvi.session.<sid>.loop.<lid>.event` — a loop's Enduring events.
- `urvi.session.<sid>.session` — session-scoped Enduring events (`SessionStarted`,
  `SessionActive`/`Idle`/`Stopped`, `Restore*`).
- `urvi.session.<sid>.loop.<lid>.cmd` — commands targeting that loop (the intent log).
  **All commands are loop-targeted** — `Interrupt`/`Shutdown` fan out per loop
  (`session.go:626`), so there is no session-level command subject.

Per-session means per-session lifecycle and a single ordered log. The loop goroutine is
the **sole producer** per `…loop.<lid>` subject, so publish order is causal order.

## Single source of truth — event sourcing

`StepDone` is emitted at the *same* actor-owned instant `loopState.msgs` is appended (the
commit arm, `loop.go:954` — "never a lie"). So **the Enduring stream is the source of
truth**, and `msgs` is a *fold* over it (`TurnStarted`/`TurnFoldedInto` carry the
`UserMessage`; `StepDone` carries the step group). Consequences:

- **Crash-consistency at step granularity.** A crash mid-step emits no `StepDone`, so that
  step is absent — restore lands at the last committed step (the loop's existing
  "discard the in-flight step" semantics).
- **No separate `msgs` snapshot.** Folding is a linear append of already-materialized
  structs (ms for thousands of steps); the only non-stream field is the optional
  `SystemMessage`, re-seeded from config. `Save/LoadSnapshot` stays a dormant seam,
  added later (at `LoopIdle`) only to bound replay time — never a source of truth.

## Commands persisted — the intent log

Commands are persisted as a parallel **audit + deterministic-replay** layer, appended at
the **Session boundary** to the target loop's `.cmd` subject. They are **not state** —
restore folds **events only** (a `UserInput`'s blocks become a committed message via
`TurnStarted`; folding the command too would double-add it, and a rejected `UserInput`
must never enter `msgs`). Correlate intent→outcome by `CommandRecord.CommandID ==
event.Cause.CommandID`; order against events by the shared JetStream sequence.

**Concrete command → record mapping** (every concrete `command.Command`; all loop-targeted):

| Command | Subject | Notes |
|---|---|---|
| `UserInput` | `…loop.<lid>.cmd` | user turn input (Agency=user) |
| `ApproveToolCall` / `DenyToolCall` | `…loop.<lid>.cmd` | gate decision (control-plane audit) |
| `ProvideUserInput` | `…loop.<lid>.cmd` | AskUser answer |
| `SubagentResult` | `…loop.<lid>.cmd` | hand-back to parent (Agency=machine) |
| `CancelQueuedInput` | `…loop.<lid>.cmd` | retract a queued input |
| `Interrupt` | one record per loop on each `…loop.<lid>.cmd` | fans out per loop |
| `Shutdown` | one record per loop on each `…loop.<lid>.cmd` | fans out per loop |

`CommandRecord = {CommandID, CreatedAt, SessionID, TargetLoopID, Agency, Command}`. Transient
fields (ack channels) are not serialized.

**Asymmetric fail-secure.** Event append is strict (it is the restore source of truth);
command append is audit-only — losing one cannot corrupt restore — so it surfaces loudly
but **proceeds** with dispatch. Never block the user's action on the audit write.

## Timestamps, identity, ordering

- **`EventID` + `CreatedAt` are minted at creation for every Enduring event**, via an
  injected **event factory** (so the hub, loop, and session share one minting path).
  Today only `LoopStarted` carries an `EventID` (`session.go:413`/`loop.go:367` mint
  none) — minting these is the prerequisite build step; an event without a stable
  `EventID` is not persistable.
- **Ordering is the JetStream stream sequence**, never the timestamp. `CreatedAt` is an
  in-record, replay-stable value for audit/display/latency only; JetStream's server store
  time is its companion. Clock injected (`now func() time.Time`) for deterministic tests.

## The durable tap — one per-session serializer

All persistence for a session funnels through **one journal serializer** (a single
goroutine/mutex) that the hub owns. It is the only writer and it owns the fence.

**Fencing (single-writer correctness).** The serializer appends with the **stream-level**
`Nats-Expected-Last-Sequence` header (NOT subject-level `ExpectedLastSubjSeq`, which only
fences one subject and would let a stale writer append to a different loop/session
subject). It holds the expected stream sequence, appends, and on success advances expected
:= the acknowledged sequence. A KV **lease** (TTL + monotonic fencing token) decides which
process *is* the serializer, but the hard fence is the stream-level expected-sequence: a
stale serializer's append fails because the sequence has advanced. Lease alone is not a
fence.

**Hub tap algorithm** (Enduring events; precise ordering & failure):
1. Serialize all hub persistence through the one serializer.
2. For an incoming Enduring event: **append it before applying it** to hub state.
3. If applying it derives a session event (e.g. quiescence flips Idle→Active →
   `SessionActive`): **create it (factory), append it, then deliver it**, in causal order
   after its trigger.
4. **Deliver no live event whose required durable append failed.**
5. On any append failure raise a typed **`SessionPersistenceFault`** via the injected
   **fault reporter**: the session rejects every new command and `NewLoop`, and wakes
   `WaitIdle` waiters with the fault. The in-memory event already committed by the loop is
   lost from the log = crash-consistency (next restore lands at the last durable step).

This lives in the hub — not the loop — because the loop deliberately swallows publish
errors (`loop.go:367`); fail-secure must be enforced where the append happens. Inject into
the hub: the serializer (`EventAppender`), the event factory, and the fault reporter.

**Idempotency.** `Nats-Msg-Id = EventID` (events) / `CommandID` (commands) gives
**at-least-once with a ~2-min dedup window** for fast retries — **not exactly-once**.
Permanent idempotency comes from the fencing expected-sequence (re-publishing an
already-stored event fails the seq check) plus **read-side `EventID` dedupe** on replay.
On an ambiguous publish ack, retry within the window; rely on fencing + read-dedupe beyond
it.

## Record size & large payloads

Two independent limits make a fixed cap unsafe: the server-wide `max_payload` (default
**1 MiB**) and the stream `MaxMsgSize` are *separate*, and the content codec permits **8
MiB per block** with up to 10 000 blocks per slice (`block_json.go:8`), so a `StepDone`
record is effectively unbounded.

Policy: define a **journal record size limit independent of block limits**. Inline records
stay under a conservative threshold (well below `max_payload`). Above it, **offload block
bodies to a JetStream Object Store** and store a **reference** in the event:
`{object-id, content-hash (sha256), length, codec-version}`. **Upload the object before
appending the referencing event** (no dangling reference). On restore, a missing or
hash-mismatched object is **fail-secure**: treat as corrupt → `RestoreErrored`, do not come
up. Raise `max_payload` + `MaxMsgSize` to a sane inline ceiling; the object store covers
the rest.

## Interfaces (DIP boundary)

```go
// Write side — events. Used by the hub's journal serializer; idempotent by Header.EventID;
// fenced by stream-level expected-sequence (returns the acknowledged sequence).
type EventAppender interface {
    Append(ctx context.Context, ev event.Event) (seq uint64, err error)
}

// Write side — commands (intent log), at the Session boundary; idempotent by CommandID.
type CommandAppender interface {
    Append(ctx context.Context, rec CommandRecord) error
}

// Read side — backlog (and optionally live) for restore / TUI repaint.
type EventReplayer interface {
    Open(ctx context.Context, req ReplayRequest) (EventCursor, error)
}

type ReplayRequest struct {
    SessionID uuid.UUID
    LoopID    uuid.UUID // v1: the primary loop
    From      StartPos  // Beginning | FromSeq(n)  (FromSeq is the dormant-snapshot hook)
    Follow    bool      // true only for "attach to a live session" (see Restore modes)
}

type EventCursor interface {
    Next(ctx context.Context) (event.Event, uint64 /*seq*/, error) // io.EOF at backlog end when !Follow
    Close() error
}

type CommandRecord struct {
    CommandID    uuid.UUID
    CreatedAt    time.Time
    SessionID    uuid.UUID
    TargetLoopID uuid.UUID
    Agency       identity.Agency
    Command      command.Command // serialized via MarshalCommand
}

// Dormant — not implemented this iteration.
type SnapshotStore interface {
    Save(ctx context.Context, loopID uuid.UUID, snap LoopSnapshot) error
    Load(ctx context.Context, loopID uuid.UUID) (LoopSnapshot, bool, error)
}
```

The NATS impl is identical CLI vs cloud; only the connection differs (composition root).

## Serialization — JSON

JSON (`encoding/json`) for each record — a long-lived compatibility surface (old sessions
restored by new code), evolving via additive fields + a `schemaVersion` tag. Build:
- `MarshalEvent`/`UnmarshalEvent` — sealed tagged union over the **Enduring** set;
  **rejects Ephemeral** events with a typed error (`TokenDelta.Chunk` has no durable codec
  by design). New `FuzzDecodeEvent` boundary.
- `MarshalCommand`/`UnmarshalCommand` — sealed tagged union over the command types,
  delegating `UserInput` blocks to the existing block codec.
- A sealed **`tool.PermissionRequest` codec** — `PermissionRequested.Request` is persisted
  **in full** (header-only would panic on replay: `interaction.ApplyEvent` →
  `promptFromPermission` dereferences it, `prompt.go:54`).
- `TurnFailed.Err` → a `{kind, message}` projection, reconstructed as a leaf
  `RestoredError` (a typed `error` can't round-trip).

Reuses the existing `content.Block`/`Message` codecs (the `FuzzUnmarshalBlock` boundary).
`Event-Type` + `Schema-Version` go in NATS headers so listing/filtering never decode a
body. **Rejected:** `gob` (type changes silently break old data); `protobuf`/`CBOR` (an
external dep for no win on coarse events).

On-disk example (short ids; metadata in the comment, JSON body below):

```jsonc
// command — urvi.session.S1.loop.L1.cmd   (Nats-Msg-Id: C1, seq 8230)
{ "type":"user_input", "command_id":"C1", "created_at":"2026-06-21T15:04:04.880Z",
  "session_id":"S1", "target_loop_id":"L1", "agency":"user",
  "blocks":[{"type":"text","text":"what's the module path?"}] }

// event — urvi.session.S1.loop.L1.event   (Nats-Msg-Id: E7, seq 8231)
{ "type":"step_done", "event_id":"E7", "created_at":"2026-06-21T15:04:05.310Z",
  "session_id":"S1","loop_id":"L1","turn_id":"T1","step_id":"P1",
  "cause":{"command_id":"C1"},
  "messages":[{"role":"assistant","blocks":[{"type":"text","text":"It's github.com/…/urvi"}]}] }
```

## Restore (v1 — primary loop only)

Restore runs through a dedicated `Restore(ctx, cfg, sessionID, replayer)` constructor (not
`New`, which mints a fresh id, publishes a new `SessionStarted`, and spawns an empty
primary loop). It **reuses the original `SessionID`** and reconstructs the **primary loop
under its original `LoopID`** (the root `LoopStarted`, zero `Cause`); identity is stable
and new events continue the same subjects.

Steps:
1. **Config-fingerprint check.** `SessionStarted` carries a fingerprint —
   `{agentKind, modelID, systemPromptRev, toolPolicyRev}`. Compare to the current config;
   on mismatch **reject or require explicit user confirmation** (re-seeding silently from
   today's config would change the resumed conversation).
2. **Acquire the single-writer lease + fence** (above) before any append.
3. **Fold the primary loop's `…loop.<lid>.event` subject** (`Open{From:Beginning,
   Follow:false}`): `TurnStarted`/`TurnFoldedInto` → user message; `StepDone` → step group
   → `loopState.msgs`; **rebuild `turnIndex`** from the count of `TurnStarted` so turn
   numbering continues; re-seed `SystemMessage` from config.
4. **Crash seam.** For any turn left open (no terminal event), append a `TurnInterrupted`
   before resuming. Precise visible result of an interrupted turn: the **committed user
   message** (from its `TurnStarted`) **plus an interruption marker**, and **no partial
   assistant step** (the in-flight step had no `StepDone`).
5. **Bracket with `Restore*`.** Append `RestoreStarted`, then `RestoreDone` (success) or
   `RestoreErrored{Err}` (failure → session does not come up). These persist back; repeated
   restores leave an audit trail (folded as no-ops for `msgs`); the catalog bumps
   `LastActiveAt` off `RestoreDone`.
6. Bring the primary loop up **idle**, ready for input.

**Queued inputs** accepted-but-unstarted exist only as Ephemeral `InputQueued` (not
durable): **not auto-resumed** (they never became a turn); they remain in the command log
for audit, optionally re-surfaced later for user re-confirm.

**Why primary-only.** A subagent loop's config (fresh `ToolSet` + permission checker per
spawn, `spawner.go:57`) is not in the journal, and its *result* is already folded into the
parent (`SubagentResult` → `TurnFoldedInto`/`StepDone`), so the conversation is intact
without re-spawning. Session-scoped permission approvals are **deliberately not persisted**
(in-memory, `permission_request.go:25`): restore **re-prompts** rather than resurrecting
authorization from disk — fail-secure.

**Two consumption modes (no UI hang).**
- **(a) Cold restore** of a stopped session: `EventReplayer` backlog (`Follow:false`) →
  fold → bring the loop up idle → take live from the hub. No concurrent producer during
  restore, so no buffer race.
- **(b) Attach to a live session** (future cloud remote / second client): one JetStream
  durable consumer with `Follow:true` (backlog→live, single cursor) — no hub-buffer race.

Either way, the TUI folds the **primary loop's** Enduring events through
`transcript.ApplyEvent` + `interaction.ApplyEvent`. The fold runs **off the Bubble Tea
update loop** (background `tea.Cmd`), builds the final reducer state, then renders **once**
into scrollback (windowed to the viewport). Backlog comes from `EventReplayer`, never the
256-cap hub buffer. ~10 000 events ≈ sub-second in the data layer.

## Session structure & catalog

**Structure (v1).** The runnable structure is the **primary loop** (root `LoopStarted`).
Folding the *full* subagent tree (from each `LoopStarted`'s `Header.Cause.Coordinates`) for
display/restore is the future multi-loop feature; v1 does not reconstruct subloops.

**Catalog (index) — lazy listing.** A `sessions` KV bucket holds one small `SessionMeta`
per session — `{SessionID, Title, CreatedAt, LastActiveAt, Status, AgentKind, LoopCount,
ConfigFingerprint}`. It is a **derived, rebuildable cache**, *not* a source of truth: lost
or stale, it is rebuilt by scanning streams. The CLI picker reads the KV bucket only
(**zero event replay**); a session is restored only on selection. A `catalog` subscriber
keeps it current off the Enduring lifecycle events (`SessionStarted` upserts; first
`TurnStarted` sets `Title`; `RestoreDone`/`TurnStarted`/`StepDone` bump `LastActiveAt`;
`SessionStopped` flips `Status`).

Physically the FileStore lays a stream down as ~a directory per session under `StoreDir`,
plus the KV buckets.

## Retention — keep everything

`Limits` retention, **no `MaxAge`/`MaxBytes` discard, no auto-expiry**. Closing a session
(`SessionStopped`) is a runtime event that **deletes nothing** — a stopped session survives
on disk and is brought back by restore. The **only** deletion is an explicit, deliberate
user action (out of scope). No auto-discard ceiling; disk capacity is ops/monitoring, and
the dormant snapshot-and-trim seam is the only explicit way to shrink history later.

## Security at rest

The journal is **not a sink** — it is the authoritative restore source, so it is
full-fidelity, never redacted. Confidentiality is by topology:
- **CLI/local:** same trust domain as the process; full-fidelity, `StoreDir` perms `0700`.
- **Cloud:** **gated** (see Scope). The seam is a `PayloadCipher` (encrypt the event body
  before append, decrypt on replay, reusing the `internal/llm/e2e` ChaCha20-Poly1305 AEAD;
  metadata + `Nats-Msg-Id` stay clear so dedup works). Not enabled until encryption +
  per-session authorization land.

## Embedded vs cluster wiring

Identical impl; the composition root differs.
- **CLI/embedded:** `server.NewServer(&server.Options{JetStream:true, StoreDir:<dir>,
  DontListen:true})` (no TCP listener); `go srv.Start()`; `nats.Connect("",
  nats.InProcessServer(srv))` (in-memory pipe). `StoreDir` `0700`. Set `max_payload` +
  stream `MaxMsgSize` for the inline ceiling.
- **Cloud (gated):** `nats.Connect(url, creds)`, `Replicas>1`, TLS `MinVersion 1.2`,
  nkey/creds auth, never `InsecureSkipVerify`.

`AddStream`/bucket creation is idempotent; restore re-binds to the existing stream.

## Testing

Per `CLAUDE.md`: table-driven, `-race`, integration-tagged, fuzz the untrusted boundary.

- **Unit:** hub durable-tap routing (Enduring→append-then-fan-out; Ephemeral→fan-out only,
  never persisted); **append-before-apply ordering** for a derived `SessionActive` (trigger
  appended first, derived created+appended+delivered, neither delivered live if its append
  fails); `SessionPersistenceFault` rejects new commands/`NewLoop` and wakes `WaitIdle`;
  event & command codecs round-trip (incl. `tool.PermissionRequest` in full, `TurnFailed.Err`
  → `RestoredError`, `UserInput` blocks); `MarshalEvent` **rejects Ephemeral**; `CreatedAt`/
  `EventID` minted from the injected factory/clock; fold → exact `msgs` + correct `turnIndex`;
  the crash-seam interrupted-turn result (user msg + marker, no partial step); config-
  fingerprint mismatch is rejected/confirmed.
- **Integration (`//go:build integration`):** append N events → tear down → new server, same
  `StoreDir` → primary-loop restore reconstructs `msgs` byte-for-byte and the transcript;
  **stream-level fencing** rejects a stale second writer (lease lost) appending to *any*
  subject; object-store offload + restore, and missing/corrupt-object → `RestoreErrored`;
  dedup window; the two consumption modes; lazy listing reads only the KV bucket;
  `Restore*` bracketing with `SessionID`/`LoopID` unchanged; commands correlate to outcomes
  by `CommandID`.
- **Fuzz:** `FuzzDecodeEvent` + `FuzzDecodeCommand` over the on-disk codecs.
- **No UI hang:** a 10 000-event backlog folds in a background `tea.Cmd` and renders once.
- **Headline property:** stream a session → `kill` → restore → `msgs` identical **and**
  transcript identical.
- `go test -race ./...`; integration with `-tags integration`.

## Build order

1. **Event factory** — mint `EventID` + `CreatedAt` at creation for every Enduring event,
   injected clock (prerequisite; nothing is persistable without a stable `EventID`).
2. **Codecs** — `MarshalEvent`/`UnmarshalEvent` (Enduring-only, rejects Ephemeral) +
   sealed `tool.PermissionRequest` codec + `MarshalCommand`/`UnmarshalCommand` +
   `FuzzDecodeEvent`/`FuzzDecodeCommand`.
3. **NATS impl** — `EventAppender` (stream-fenced, returns acked seq) / `CommandAppender` /
   `EventReplayer`; record-size limit + Object-Store offload; KV lease.
4. **Hub durable tap** — the per-session serializer + append-before-apply algorithm +
   `SessionPersistenceFault` + injected factory/fault-reporter; Session-boundary command
   append; composition wiring.
5. **`Restore` constructor** — primary loop only: config-fingerprint check, fold `msgs` +
   `turnIndex`, crash-seam, `Restore*` events, lease+fence.
6. **Catalog (KV)** + lazy listing → **TUI replay-repaint** (background fold, mode (a)).

CLI (local) is the supported target; the cloud backend is gated on encryption + per-session
authz. Composes with the TUI-event-adoption work.

## Open questions / risks

- **Binary +~19 MB / RAM +~18 MB idle** (measured) — accepted; watch RSS on a cloud worker
  holding many concurrent sessions (memory scales with live streams/consumers).
- **Disk growth** — keep-everything is unbounded for long-lived sessions; the dormant
  snapshot-and-trim seam + ops monitoring are the mitigation.
- **Serializer throughput** — one serializer per session is the correctness boundary;
  confirm it isn't a latency bottleneck at `StepDone` granularity (coarse, so expected fine).

## Future seams enabled

Cross-worker subagent routing (session-rooted subjects); multi-loop / subagent restore
(persisted loop-definition + permission snapshot); snapshots + head-trim; at-rest
encryption (`PayloadCipher`); explicit session deletion.
