# Event Persistence & Checkpoint/Restore Design

**Date:** 2026-06-19
**Status:** Draft
**Depends on:** `docs/plans/loop-machine-design.md` — the event model this layer
persists: the `Ephemeral`/`Enduring` classes, the per-step `StepDone` event
(finalized `AIMessage` + its `ToolResultMessage`s), the turn/input lifecycle
events (`TurnStarted`/`TurnFoldedInto`/`InputCancelled`/`TurnRejected`), the
session/loop lifecycle events (`SessionStarted`/`SessionActive`/`SessionIdle`/
`SessionStopped`/`LoopIdle`/`LoopStarted`), producer-identity `Header` fields
(`Coordinates{SessionID,LoopID,TurnID,StepID}`, `EventID`, `Cause`), and the
publish/subscribe hub the loop emits to.
**Composes with:** `docs/plans/2026-06-18-tui-event-adoption-design.md` — the TUI
as a projection of Enduring events via `ApplyEvent`; restore reuses that exact
projection.

> **Reconciliation with landed code (2026-06-21).** Validated against `main`
> (`fc93ee4`); the loop-machine event model has **landed as real code**, so this
> layer is buildable now — no upstream design dependency remains. Deltas from the
> original draft:
>
> - **Confirmed as-designed:** `StepDone` (Enduring, loop-scoped, carries
>   `Messages content.AgenticMessages`); `TurnStarted`/`TurnFoldedInto` carry the
>   `UserMessage`; the commit arm appends `msgs` **then** publishes the event at one
>   actor-owned point (`loop.go` — "never a lie"); the hub
>   (`internal/agent/session/hub`) exposes `SubscribeEvents(EventFilter{Ephemeral,
>   Enduring LoopScope})` and already enforces Ephemeral-drop / Enduring-fail-loud
>   (`SubscriptionLossError`, 256-buffer); `ToolCall*` + `InputQueued` are
>   Ephemeral and `TurnRejected` Enduring (TUI-adoption amendments are in code);
>   the TUI reduces via `transcript.ApplyEvent` + `interaction.ApplyEvent` with
>   per-loop `ClearPromptsForLoop`.
> - **`LoopSpawned` caveat resolved:** `LoopStarted` (Enduring, loop-scoped) is
>   published by `Session.NewLoop`, spawning provenance in
>   `Header.Cause.Coordinates` (zero = primary/root). Session structure folds from
>   it; no new event needed.
> - **Terminology map** (doc → code): `AgentSession`→`Session`; `Header.ID`→
>   `Header.EventID` (the idempotency key → `Nats-Msg-Id`); `Header.ParentLoopID`→
>   `Header.Cause.Coordinates.LoopID`; `CausationID`→`Cause.CommandID`/`Cause.EventID`;
>   submit `InputID`→`CommandID`. Subjects key off `Header.Coordinates`.
> - **Integration is a publish-path decorator, not a reroute** (supersedes *One
>   delivery path per class* below). The hub is already the live bus with the
>   right policy, so the durable layer is a `journalingPublisher` implementing the
>   loop's narrow `eventPublisher` (`PublishEvent`) and wrapping `*hub.Hub`: for
>   Enduring, **Append to JetStream then forward to the hub**; for Ephemeral,
>   **forward only**. The loop is unchanged (DIP); live delivery stays in-memory
>   via the hub; JetStream is the durable tap + restore source.
> - **Append-failure = crash-consistency (fail-secure).** `msgs` commits before
>   publish, so a JetStream `Append` error can't un-commit; treat it as a crash at
>   that instant — surface loudly and stop/degrade the session. The append-only
>   log stays a consistent prefix; restore lands at the last durable step (the
>   crash-seam covers the rest). **Never** forward-on-Append-error (that yields an
>   unrestorable session).
> - **An event-level JSON codec must be built** (supersedes "reuses the existing
>   codec"). Only `content.Block` and `Message` have tagged-union codecs today;
>   whole events don't round-trip. Build a sealed `MarshalEvent`/`UnmarshalEvent`
>   (type discriminator + payload) over the 20 event types atop the block/message
>   codecs, with its own fuzz target. The existing `json:"-"` tags are *in-memory-only*
>   artifacts (events were never serialized before — the hub is pure in-memory), not
>   a limit: **our codec decides what persists.** It serializes `TurnFailed.Err` as a
>   `{kind, message}` projection → reconstructs a leaf `RestoredError` on read (the
>   tombstone needs the text; a typed `error` can't round-trip). `PermissionRequested.Request`
>   is *not needed* for restore — the prompt is always cleared (by its turn's terminal
>   on replay, or the crash-seam if pending) — so persist header-only by default; add
>   the body only for a gate audit trail.
> - **Restore is itself recorded.** Emit `RestoreStarted` then `RestoreDone` (or
>   `RestoreErrored`) — Enduring, session-scoped, appended to the **same** stream —
>   and keep `SessionID` + every `LoopID` **unchanged** across restore (the reason
>   for the dedicated `Restore` constructor; no fresh `SessionStarted`/`LoopStarted`).
>   Three new Enduring session-scoped event types this design owns.
> - **Restore needs a dedicated constructor.** `session.New` mints a fresh id,
>   publishes `SessionStarted`, and spawns an empty primary loop — wrong for
>   resume. Add a `Restore(ctx, cfg, sessionID, replayer)` path that folds the
>   journal to rebuild loops + `msgs` + tree, appends the crash-seam
>   `TurnInterrupted` for any open turn, and comes up idle without a new
>   `SessionStarted`. Live handoff: subscribe to the fresh hub first, replay the
>   JetStream backlog, then drain buffered live events **deduped by
>   `Header.EventID`**.

## Motivation

Today there is **zero persistence**: a loop's committed conversation
(`loopState.msgs`) lives only in memory and dies with the process; `cfg.Sinks`
is `nil`. The loop-machine spec deliberately left this open — it lists "journal
format and restore mechanics" as out of scope, and documents the hub as the
attach point: *"Consumers (TUI/CLI now, a durable journal later) attach here."*

This design builds that durable journal. The goals:

1. **Durably persist every loop's committed history**, so a session survives
   process exit (clean quit or crash) and can be **brought back to continue**.
2. **Repaint the TUI** from the durable record on restore.
3. **One programming model across two topologies** — an embedded, file-backed
   store for the CLI, and a clustered store for cloud/headless workers — so the
   future cross-worker subagent layer drops onto the same substrate without a
   second storage implementation.

The chosen substrate is **NATS JetStream**: embedded `nats-server/v2` (in-process,
FileStore) for the CLI; a JetStream cluster for cloud. JetStream gives us an
append-only, sequenced, replayable log with durable consumers and idempotent
publish — the same client API whether the bytes live in a local file or a
cluster. A hand-rolled stdlib journal would force us to write the cloud half
twice (local format *and* cross-worker coordination) and let them drift.

## Scope

**In scope:** the durable Enduring-event journal; the command (intent) log;
per-record creation timestamps; checkpoint/restore of a loop's `msgs`; TUI
replay-repaint (background fold, no UI hang); the session catalog + lazy restore;
the embedded vs clustered wiring; retention policy.

**Out of scope (named, not built here):**
- **Cross-worker subagent routing/orchestration** — this layer *enables* it (the
  subject namespace is session-rooted so any worker publishes to one logical
  stream), but the routing/hand-back coordination is a separate follow-on.
- **At-rest encryption** of the cloud store — deferred; a `PayloadCipher` seam is
  noted but not implemented this iteration.
- **Snapshots** — a dormant `Save/LoadSnapshot` interface seam only; added when a
  profiler shows replay-from-zero is measurably slow on a pathologically long
  session.
- **Explicit session deletion** ("delete/forget this session") and cloud
  auth/creds/clustering operations — named below, designed elsewhere.

## Dependency approval (required before implementation)

This layer introduces the **first heavy external dependencies** in the tree:

- `github.com/nats-io/nats.go` — the client (publish/subscribe, JetStream
  context, KV, durable consumers).
- `github.com/nats-io/nats-server/v2` — run **in-process** for the CLI/embedded
  mode (this is where JetStream durability lives; the client alone persists
  nothing).

Per `CLAUDE.md`, external deps require explicit approval; this has been given for
both. **Action at implementation time:** amend `CLAUDE.md`'s sanctioned-deps list
with both modules and this rationale, in the same change that adds them to
`go.mod` (so `CLAUDE.md` never lists a dep that isn't yet vendored).

## The spine: route by event class

The whole design follows from the loop-machine's own `Ephemeral`/`Enduring`
split:

- **Enduring events → JetStream.** This is **both** the durable journal **and**
  the transport that reattaching clients and (later) remote workers read.
  `StepDone`, `TurnStarted`/`TurnFoldedInto`, and the session/loop lifecycle
  events land here.
- **Ephemeral events → in-process hub only.** `TokenDelta` and the `ToolCall*`
  spinners (Ephemeral per the TUI-adoption amendment) stay on the fast local
  fan-out for whatever client is attached *now*, and are dropped otherwise. We
  **never** serialize or fsync a per-token delta — exactly what "Ephemeral =
  droppable, reconstructable" already promised. In headless mode with no live
  client, they go nowhere; the Enduring stream is all that matters.

The loop never learns NATS exists. It emits every event to the loop-machine's
`eventPublisher`. A **durable hub** implements that interface and routes by
`event.Class()`. NATS is wired only at the composition root (Dependency
Inversion).

### Stream & subject topology

- **One JetStream stream per session.** Subjects:
  `urvi.session.<sid>.loop.<lid>.event` (events) and
  `urvi.session.<sid>.loop.<lid>.cmd` (commands — see *Commands are persisted*);
  session-level commands use `urvi.session.<sid>.cmd`.
- Per-session means per-session lifecycle and a single ordered log; the per-loop
  subject lets a consumer filter to one loop, to `…loop.<lid>.event` for just its
  events, or to `urvi.session.<sid>.loop.*.event` for all loops' events — which is
  exactly the `EventFilter` per-`LoopID` semantics, and exactly how a future
  cross-worker parent reads its subagents.
- **Idempotent publish:** set `Nats-Msg-Id = Header.EventID` (events) /
  `CommandRecord.CommandID` (commands). JetStream's dedup window makes retries
  effectively-once on the log. (Dedup uses metadata, not payload — unaffected by
  the future at-rest encryption.)

### One delivery path per class (no dedup seam)

> **Superseded by the 2026-06-21 reconciliation.** Now that the hub exists as the
> live bus, the durable layer is a *publish-path decorator* (Append-then-forward),
> so live Enduring delivery stays in-memory via the hub and JetStream is the
> durable tap; the backlog→live seam is deduped by `Header.EventID` at restore.
> The original framing below is retained for rationale.

Even **live** Enduring events round-trip through JetStream before a local client
sees them: `PublishEvent(Enduring)` does `journal.Append` only; a subscriber
receives it back via its JetStream consumer. This is deliberate — each class has
exactly one path, so "replay then live" is literally one cursor, with no
double-delivery to dedupe. The cost is a few ms of embedded-JetStream round-trip
on a coarse `StepDone` (invisible to a human); the latency-sensitive token stream
(Ephemeral) stays in-process and instant.

## Three properties that fall out

1. **One log, not two.** `StepDone` is emitted at the *same* actor-owned instant
   `loopState.msgs` is appended (the spec's "`StepDone` is never a lie"
   invariant). So **folding the Enduring stream reconstructs `msgs`** — the
   conversation context and the TUI transcript are both projections of the one
   log.
2. **Crash-consistency for free, at step granularity.** A crash mid-step means
   that step never emitted its `StepDone`, so it is simply absent from the log;
   restore lands at the last completed step — already the loop's "discard the
   in-flight incomplete step" semantics. No torn turns, no half-run tools.
3. **Restore and live are one subscription.** A durable consumer starting at a
   sequence delivers the backlog (→ repaint via `ApplyEvent`) and continues
   seamlessly into live Enduring events on the same cursor.

## No separate `msgs` snapshot (the event stream is the source of truth)

We considered checkpointing `loopState.msgs` directly (e.g., at `LoopIdle`). We
**reject a separate snapshot as the source of truth**, because:

- **Folding events → `msgs` is cheap and lossless.** Messages aren't *derived*
  from events, they're *carried* by them (`StepDone.Messages` already is the
  finalized group). Folding is a linear append of already-materialized structs —
  milliseconds for thousands of steps, off a sequential FileStore read. The only
  field not in the stream is the optional `SystemMessage`, which comes from the
  agent's config and is re-seeded at construction, not replayed.
- **It doesn't compound across subagents.** Each loop is its own subject, so a
  loop restores from *its* subject alone — more subagents means more small,
  independent (parallelizable) folds, never one larger fold.
- **"Step is the unit of work" argues for the stream, not snapshots.** Both an
  append-per-`StepDone` and a snapshot-per-`StepDone` give step-granular
  recovery, but appending is O(1) per step (**O(n)** total) while re-snapshotting
  the growing `msgs` is **O(n²)** write amplification. The append-only stream
  already *is* a step-granular checkpoint, at linear cost.

**The only honest caveat:** replay-from-sequence-1 is O(history), so a
*pathologically* long single session (a headless run with tens of thousands of
steps) eventually makes cold restore measurably slow. That is the *only* thing a
snapshot ever buys. So we keep `Save/LoadSnapshot` as a **dormant interface
seam** and add a periodic full-`msgs` snapshot *later, at `LoopIdle`* (the cheap
coarse instant), purely as a replay-truncation optimization — never promoting it
to a source of truth. YAGNI until a profiler says otherwise.

## Commands are persisted too (the intent log)

The Enduring **event** stream is the source of truth for *state* (above). On top
of it we also persist **commands** — the inputs/intent (`Submit`, `Interrupt`,
`Shutdown`, `SubagentResult`, `CancelQueuedInput`) — as a parallel **audit +
deterministic-replay** layer. Commands and events are produced at different
points: commands arrive at the **Session boundary** (before the loop actor);
events are emitted **by** the loop actor. So the layer has two write entry points
into the *same* per-session stream, on distinct subjects:

- events → `urvi.session.<sid>.loop.<lid>.event`
- commands → `urvi.session.<sid>.loop.<lid>.cmd` (loop-targeted) or
  `urvi.session.<sid>.cmd` (session-level: `Interrupt`-all, `Shutdown`)

**Commands are not state.** Restore-to-continue folds **events only**. A `Submit`
carries input blocks, but the *committed* user message comes from `TurnStarted`;
folding both would double-add it, and a *rejected* `Submit` (no `TurnStarted`)
must never enter `msgs`. So events stay authoritative; commands are "what was
attempted" — for audit, control-plane history (who interrupted, when), and
deterministic input replay. Correlate to outcomes by
`CommandRecord.CommandID == event.Cause.CommandID`; order against events by the
shared JetStream sequence (one timeline per session).

**Asymmetric fail-secure.** Event append is strict fail-secure — it is the restore
source of truth, so a failed append degrades/stops the session. Command append is
audit-only: losing one cannot corrupt restore, so it surfaces loudly but
**proceeds** with dispatch — never block the user's action on the audit write.

## Timestamps and ordering

Every persisted record — event and command — carries a **creation** timestamp,
stamped when created, not when delivered or stored:

- Events gain `Header.CreatedAt time.Time`; commands carry `CommandRecord.CreatedAt`
  (realizing the loop-machine "stamp `CreatedAt`/`EventID` at creation, not
  delivery" intent). The clock is injected (`now func() time.Time`), mirroring the
  existing `idGenerator` injection, so tests are deterministic.
- **Timestamps are for audit / display / latency only — never ordering.** Wall
  clocks skew and run backwards; the authoritative order is the **JetStream
  sequence**, and since the loop is the sole producer per subject, sequence order
  *is* causal order. JetStream also stamps a server-side store time in message
  metadata; `CreatedAt` is the in-record, replay-stable companion to it.

## Interfaces (the DIP boundary)

Two narrow interfaces (ISP — write and read sides are independent); the NATS
implementation lives behind them and is wired at the composition root.

```go
// Write side (events) — used by the journalingPublisher decorator.
type EventAppender interface {
    Append(ctx context.Context, ev event.Event) error // idempotent by Header.EventID
}

// Write side (commands) — used at the Session boundary (Submit/Interrupt/…).
type CommandAppender interface {
    Append(ctx context.Context, rec CommandRecord) error // idempotent by rec.CommandID
}

type CommandRecord struct {
    CommandID    uuid.UUID
    CreatedAt    time.Time       // stamped at creation (injected clock)
    SessionID    uuid.UUID
    TargetLoopID uuid.UUID       // zero for session-level (Interrupt-all / Shutdown)
    Agency       identity.Agency // user | machine
    Command      command.Command // sealed; serialized via MarshalCommand
}

// Read side — used by loop cold-restore and TUI repaint.
type EventReplayer interface {
    Open(ctx context.Context, req ReplayRequest) (EventCursor, error)
}

type ReplayRequest struct {
    SessionID uuid.UUID
    LoopID    uuid.UUID // zero ⇒ all loops in the session
    From      StartPos  // Beginning | LiveOnly | FromSeq(n)  — FromSeq is the dormant-snapshot hook
    Follow    bool      // after backlog drains, continue into live on the same cursor
}

type EventCursor interface {
    Next(ctx context.Context) (event.Event, uint64 /*seq*/, error) // io.EOF at backlog end when !Follow
    Close() error
}

// Dormant seam — not implemented this iteration.
type SnapshotStore interface {
    Save(ctx context.Context, loopID uuid.UUID, snap LoopSnapshot) error
    Load(ctx context.Context, loopID uuid.UUID) (LoopSnapshot, bool, error)
}
```

The **`journalingPublisher`** implements the loop's narrow `eventPublisher` and
wraps the existing `*hub.Hub`:
- `PublishEvent(Enduring)` → `EventAppender.Append` (durable) **then** forward to
  the hub (live fan-out). Append-failure is fail-secure (see reconciliation).
- `PublishEvent(Ephemeral)` → forward to the hub only; never persisted.

The hub stays the live bus. Backlog on restore comes from `EventReplayer`
(a JetStream fetch), **not** the hub subscription — so the 256-cap live buffer is
never in the backlog path (see *Replay performance*). The NATS impl maps
`LoopID`→subject filter, `From`→JetStream `DeliverPolicy`, `Follow`→a consumer
that stays open vs. closes at `io.EOF`. Commands are appended at the Session
boundary via `CommandAppender`, independent of this publish path.

## Restore flow — folds over one stream

**Session structure first.** Before any loop is seeded, reconstruct
`sessionState` — by folding the session's own stream, exactly like everything
else, not from a side-record. The **set of loops = the `loop.*` subjects** in the
session stream; the **subagent tree = `Header.Cause.Coordinates.LoopID`** on their
`LoopStarted` events; the **primary loop = the root** (no parent). A loop that was
spawned but never
committed any Enduring event has no subject and no state to restore — consistent
with "discard the incomplete." (See *Session structural state* for the one
caveat.)

**Loop cold-restore (per loop):** `Open({sid, lid, From:Beginning, Follow:false})`
→ fold: `TurnStarted`/`TurnFoldedInto` append the user message; `StepDone`
appends the step group; lifecycle events ignored → seed `loopState.msgs`
(+ `SystemMessage` from config) → start `runLoop` **idle**, ready for input. New
events it emits continue the same subject; JetStream assigns the next sequence.

**TUI repaint:** `Open({sid, LoopID:0 /*all loops*/, From:Beginning, Follow:true})`
→ fold every Enduring event through `transcript.ApplyEvent` +
`interaction.ApplyEvent` → transcript repainted; the same cursor then follows
into live. The Ephemeral live tail attaches from the local hub.

**The crash seam (what makes restore correct, not just plausible):** a `kill -9`
mid-step leaves a turn with no terminal event and possibly an orphaned
`PermissionRequested`. So **on restore, before resuming, the loop appends a
`TurnInterrupted` for any turn left open** — exactly what an in-process abort
would have emitted. The stream is then well-formed and *existing* semantics clean
up: the loop's fold already ignores the dead in-flight step (no `StepDone`), and
the TUI's "a terminal event clears that loop's pending gates" rule
(tui-event-adoption §7) drops the orphaned prompt. No restore-only code in either
reducer — recovery is the stream healing itself to the last completed step.

**Restore is recorded; identity is stable.** Restore runs through a dedicated
`Restore(ctx, cfg, sessionID, replayer)` constructor (not `New`, which would mint
a fresh id, publish a new `SessionStarted`, and spawn an empty primary loop).
`Restore` reuses the original `SessionID` and reconstructs every loop under its
**original `LoopID`** (from its `LoopStarted`), so nothing's identity changes and
new events continue the same subjects. It brackets the operation with Enduring,
session-scoped events appended to the same stream — `RestoreStarted` at the start,
then `RestoreDone` on success or `RestoreErrored{Err}` on failure (fail-secure: a
half-restored session does not come up; the failed attempt is durably recorded).
The in-log sequence is `…history… → RestoreStarted → TurnInterrupted×(open turns)
→ RestoreDone → …new live events…`. These persist back, so repeated restores leave
an audit trail (folded as no-ops for `msgs`); the catalog bumps `LastActiveAt` /
sets a "restored" marker off `RestoreDone`. `RestoreStarted`/`RestoreDone`/
`RestoreErrored` are three new Enduring session-scoped event types this design adds
to the event package.

## Replay performance — no UI hang

A big backlog must not freeze the TUI. It won't, because replay is a background
batch fold rendered once — never a per-event repaint on the update loop:

- **Backlog comes from `EventReplayer` (a JetStream fetch), not the hub
  subscription**, so the 256-cap live buffer is never in the backlog path and
  can't backpressure or fail.
- **Fold off the UI loop.** In a background `tea.Cmd`/goroutine, decode and apply
  all N events through the pure `transcript.ApplyEvent` / `interaction.ApplyEvent`
  folds (in-memory, no I/O, no rendering) to build the final reducer state, then
  hand the TUI **one** "restored" message.
- **Render once into scrollback.** The TUI is scrollback-first: the rebuilt
  transcript flushes to native terminal history in one pass (the terminal owns the
  scroll history, so we don't keep N live-rendered entries), then live attaches.
  Rendering is windowed to what's visible.

**Cost of ~10k events** (a stress case — most sessions are tens-to-hundreds):
sequential read + JSON decode (~10–20 MB) + slice-append fold is **well under
~1 s**; the variable cost is *rendering* many entries, bounded by
render-once-to-scrollback + viewport windowing (~1–2 s wall worst case). If cold
restore ever hurts at that scale, the dormant **snapshot seam** turns it into
*load snapshot + replay only the tail* (O(history) → O(since-last-snapshot)). We
don't build it until a profiler says 10k hurts.

## Session structural state (a fold, not a record)

`sessionState` holds the session's structure — the loop map, the subagent
parent/child tree, the primary loop. This is **distinct** from the cross-session
*index* below, and it is **not** persisted as a separate authoritative blob. Like
`loopState.msgs`, it is a **fold over the session's own stream** — the loop set
from the `LoopStarted` events, the tree from each `LoopStarted`'s
`Header.Cause.Coordinates.LoopID`, the primary loop = the root (zero `Cause`).

The governing principle: **any must-survive session state that isn't
re-derivable from config must be an Enduring event, never a side-record** — so it
stays in the one source of truth. This is already satisfied: `LoopStarted` (landed
on `main`) carries the spawning provenance in `Header.Cause.Coordinates`, so the
structure folds with no upstream amendment. (Were some future structural fact not
inferable — e.g. an explicit role — it would be added as a field on `LoopStarted`
rather than stored in a side-record.)

## Session catalog (index) & lazy restore

Opening the CLI must **not** replay every session's history. Listing reads only
metadata; full restore happens on selection.

The index is a **derived, rebuildable cache** — *not* a source of truth and *not*
`sessionState`. If it is lost or stale it is rebuilt by scanning streams; keeping
it non-authoritative is what prevents an index-says-X-but-streams-say-Y
divergence.

- **`SessionMeta` in a `sessions` KV bucket.** One small record per session:
  `{SessionID, Title, CreatedAt, LastActiveAt, Status, AgentKind, LoopCount}`.
  KV is FileStore-backed in CLI, replicated in cloud — same substrate as the
  event streams.
- **Listing is replay-free.** The CLI session picker reads the KV bucket only —
  one tiny record per session, **zero event replay**. (`StreamInfo` additionally
  provides message counts / first/last timestamps for free.)
- **Restore is lazy.** Only on *selecting* a session do we instantiate the
  `Session` via `Restore`, open a replayer per loop subject, fold → seed, repaint.
  Until then nothing is read but metadata.
- **A `catalog` subscriber keeps metadata current** without coupling the loop to
  KV: it watches Enduring lifecycle events — `SessionStarted` upserts the record;
  the first `TurnStarted` sets `Title` (e.g. from the first user message);
  `TurnStarted`/`StepDone` bump `LastActiveAt`; `SessionStopped` flips `Status`.

**Logical vs physical layout.** Logically: session → stream, loop → subject,
session state → KV record. Physically, JetStream's FileStore owns the on-disk
layout — roughly a directory per session-stream under `StoreDir`, plus the KV
buckets — so you do get "a folder per session," just not hand-managed. "File per
loop" becomes "subject per loop" within the session stream: it preserves
session-wide ordering while staying per-loop addressable via subject filter.

```
StoreDir/
  jetstream/$G/
    streams/
      urvi_session_<sid-1>/   ← one stream (one session); loops are subjects within
      urvi_session_<sid-2>/
      ...
    kv/
      urvi_sessions/          ← SessionMeta catalog (the "session state")
```

## Embedded vs cluster — one impl, the connection differs

The `EventAppender`/`EventReplayer` implementation is **identical** in both
topologies; only the composition root differs.

- **CLI/embedded:** start `server.NewServer(&server.Options{JetStream:true,
  StoreDir:<dataDir>, DontListen:true})` (`DontListen` ⇒ **no TCP listener at
  all**); `go srv.Start()`; `srv.ReadyForConnections(...)`; connect via
  `nats.Connect("", nats.InProcessServer(srv))` — an in-memory pipe, no socket.
  `StoreDir` perms `0700`. The embedded server is the full `nats-server/v2`
  JetStream engine compiled in (several in-process goroutines), with no network
  surface — no daemon, no port, no IPC; the only footprint is `StoreDir`.
- **Cloud:** drop the embedded server; `nats.Connect(url, creds)` to the cluster;
  streams created with `Replicas>1`. TLS `MinVersion 1.2`, nkey/creds auth, never
  `InsecureSkipVerify` (per `CLAUDE.md`).

`AddStream`/bucket creation is idempotent, so restore just re-binds to the
existing FileStore-backed stream.

## Retention — keep everything, forever

The Enduring stream **is** the source of truth and the durable history, so it
**never auto-expires**:

- `Limits` retention; **no `MaxAge`, no `MaxBytes` discard, no auto-expiry of any
  kind.** Nothing ages out.
- **Closing a session deletes nothing.** `SessionStopped` is a *runtime*
  lifecycle event (loops shut down, process exits, user quits) and is entirely
  orthogonal to the durable record — which is the whole point: a stopped session
  survives on disk and is brought back by restore.
- **The only deletion is an explicit, deliberate user action** ("delete/forget
  this session"), never a side effect of any lifecycle transition. It is **out of
  scope** here; default behavior is that we never delete.
- **No auto-discard ceiling.** For a source-of-truth stream there is no good
  discard behavior (dropping old data loses history; refusing new writes stalls
  the session). Disk capacity is an **ops/monitoring** concern; the eventual
  snapshot-and-trim seam is the *only* legitimate, *explicit* way to shrink
  history later.

## Security at rest

The journal is **not a sink.** Sinks are redacted because they may leak; the
journal is the *authoritative restore source*, so redacting it would corrupt
restore (you'd lose tool args/results). The loop-machine design made the session
fan-in full-fidelity *precisely* so a durable journal could attach. So nothing is
redacted — confidentiality at rest is handled by topology:

- **CLI/embedded:** same trust domain as the process and the in-memory
  conversation (the user's own machine). **Full-fidelity, filesystem perms only**
  (`StoreDir` `0700`).
- **Cloud/clustered:** crosses a trust boundary. **Deferred this iteration**
  (see Scope). The seam is a `PayloadCipher` on the NATS impl — encrypt the event
  *body* before `Append`, decrypt on `Replay`, reusing the ChaCha20-Poly1305 AEAD
  already sanctioned in `internal/llm/e2e`; routing metadata + `Nats-Msg-Id` stay
  clear so dedup still works. Wire a no-op cipher for CLI now; build the real
  AEAD + key management with the cloud backend.

## Serialization format — JSON

The on-disk payload of each record — event or command — is **JSON**
(`encoding/json`), because it is a **long-lived compatibility surface**: old
sessions are restored by new code, so the format must evolve forward/backward-
compatibly. JSON does this with additive fields + an explicit `schemaVersion` tag.

- **Must build an event-level codec, atop existing ones.** `content.Block` and
  `Message` already have sealed-interface JSON codecs (the ones `FuzzUnmarshalBlock`
  guards), but **whole events do not round-trip through `encoding/json` today**
  (verified 2026-06-21). `Event` is a sealed interface, so build a
  `MarshalEvent`/`UnmarshalEvent` tagged union (type tag + payload) over the 20
  event types, delegating block/message payloads to the existing codecs — one
  codec style, a new `FuzzDecodeEvent` boundary. The current `json:"-"` tags are
  in-memory-only artifacts, not a limit — the codec decides what persists:
  `TurnFailed.Err` serializes as a `{kind, message}` projection (reconstructed as a
  leaf `RestoredError` — the tombstone needs the text; a typed `error` can't
  round-trip). `PermissionRequested.Request` is not needed for restore (the prompt
  is always cleared), so persist header-only by default; add the body only for a
  gate audit trail.
- **Debuggable.** The stream/FileStore can be inspected (`nats stream view`) and
  the events read directly — valuable during restore bring-up.
- **Stdlib**, per `CLAUDE.md`.
- **Headers carry type + version.** `Event-Type` and `Schema-Version` go in the
  NATS message headers (body stays JSON), so the index/catalog and any filtering
  never decode a body.
- **Commands use a parallel codec.** `MarshalCommand`/`UnmarshalCommand` (sealed
  tagged union over the command types), delegating `Submit`'s blocks to the block
  codec; transient fields (ack channels) are not serialized. The `CommandRecord`
  envelope adds `CommandID`, `CreatedAt`, `TargetLoopID`, `Agency`.

**On-disk shape** (short ids; JetStream metadata shown in the comment line, JSON
body below):

```jsonc
// command — subject urvi.session.S1.loop.L1.cmd   (Nats-Msg-Id: C1, seq 8230)
{
  "type": "submit",
  "command_id": "C1",
  "created_at": "2026-06-21T15:04:04.880Z",
  "session_id": "S1",
  "target_loop_id": "L1",
  "agency": "user",
  "blocks": [{"type": "text", "text": "what's the module path?"}]
}

// event it caused — subject urvi.session.S1.loop.L1.event  (Nats-Msg-Id: E7, seq 8231)
{
  "type": "step_done",
  "event_id": "E7",
  "created_at": "2026-06-21T15:04:05.310Z",
  "session_id": "S1", "loop_id": "L1", "turn_id": "T1", "step_id": "P1",
  "cause": {"command_id": "C1"},
  "messages": [
    {"role": "assistant", "blocks": [
      {"type": "text", "text": "It's github.com/…/urvi"}]},
    {"role": "tool", "tool_use_id": "u1", "blocks": [
      {"type": "tool_result", "text": "module github.com/…"}]}
  ]
}
```

`cause.command_id == command_id` links outcome to intent; `seq` orders the two;
`created_at` is audit-only. (Block-type strings are the existing `content.Block`
discriminators.)

**Rejected:** `gob` — Go-native and compact, but type changes silently break old
data, disqualifying for a durable format; `protobuf`/`CBOR` — an external dep +
codegen for no real win on coarse `StepDone`-granularity events.

## Testing

Per `CLAUDE.md`: table-driven, `-race`, integration-tagged for process-boundary
code, fuzz the untrusted boundary.

- **Unit:** `journalingPublisher` class routing (Enduring→append-then-forward;
  Ephemeral→forward-only, never persisted); the event→`msgs` fold round-trips
  (`TurnStarted`/`StepDone` → exact `AgenticMessages`); the event **and** command
  codecs round-trip (incl. `TurnFailed.Err` → `RestoredError`; `Submit` blocks);
  `CreatedAt` is stamped from the injected clock (deterministic); idempotent append
  (same `Header.EventID` / `CommandID` twice → one record); the crash-seam (a turn
  with no terminal event → restore appends `TurnInterrupted` before resume);
  asymmetric fail-secure (event-append error stops/degrades; command-append error
  surfaces but dispatch proceeds); catalog updates (`SessionStarted`/first
  `TurnStarted`/`SessionStopped`/`RestoreDone` → expected `SessionMeta`).
- **Integration (`//go:build integration`)** — NATS is a real engine even
  embedded: append N Enduring events → tear down → **new server, same
  `StoreDir`** → replay reconstructs `msgs` *byte-for-byte* and the TUI transcript
  identically; dedup window; replay-then-live on one cursor; per-loop subject
  filtering; crash mid-step (no `StepDone`) lands restore at the last completed
  step; **session structure (loop set / tree / primary) reconstructs from
  `LoopStarted` + `Header.Cause.Coordinates.LoopID`**; commands replay/correlate to
  their outcome events by `CommandID`; lazy listing reads only the KV bucket (no
  consumer opened until select); restore is bracketed by
  `RestoreStarted`/`RestoreDone` with `SessionID`/`LoopID`s unchanged.
- **Fuzz:** `FuzzDecodeEvent` (and `FuzzDecodeCommand`) over the on-disk codecs —
  restore decodes untrusted bytes off disk, the same untrusted-restore boundary
  `FuzzUnmarshalBlock` already guards.
- **Replay does not hang the UI:** a 10k-event backlog folds in a background
  `tea.Cmd` and renders once; the live 256-buffer is never in the backlog path.
- **Headline property test:** stream a session → `kill` → restore → `msgs`
  identical **and** transcript identical ("displayed == stored == restored").
- Run with `go test -race ./...`; integration with `go test -tags integration
  -race ./...`.

## Sequencing

The loop-machine event model (classes, `StepDone`, the hub, `EventFilter`,
`LoopStarted`) has **landed on `main` as of `fc93ee4`** — so there is no upstream
design dependency left and this layer is buildable now. The `LoopSpawned`
amendment is moot: `LoopStarted` + `Header.Cause.Coordinates` already make
session structure foldable. Build order within this layer: `Header.CreatedAt` +
injected clock → event & command codecs (`MarshalEvent`/`UnmarshalEvent` +
`MarshalCommand`/`UnmarshalCommand` + `FuzzDecodeEvent`) → `EventAppender` /
`CommandAppender` / `EventReplayer` (NATS) → `journalingPublisher` decorator +
Session-boundary command append + composition wiring → `Restore` constructor +
the three `Restore*` events → catalog (KV) → TUI replay-repaint (background fold).
Orthogonal to and composes with the TUI-event-adoption work.

## Open questions / risks

- **Binary size.** `nats-server/v2` is a large dependency; measure the binary-size
  delta and confirm it's acceptable for the CLI.
- **Disk growth.** Keep-everything means unbounded growth for very long-lived
  sessions; the snapshot-and-trim seam is the planned (explicit) mitigation, plus
  ops-level disk monitoring.
- **Live-Enduring round-trip latency.** Routing live Enduring events through the
  embedded JetStream adds a small round-trip; benchmark to confirm it's
  imperceptible at `StepDone` granularity.

## Future seams enabled (not built here)

- **Cross-worker subagent routing** — the session-rooted subject namespace lets
  any worker publish a subagent loop's events to the one session stream, and a
  parent on another worker reads them via `urvi.session.<sid>.loop.*`.
- **Snapshots + head-trim** — the dormant `SnapshotStore` bounds cold-restore
  time and enables explicit history trimming.
- **At-rest encryption** — the `PayloadCipher` seam.
- **Explicit session deletion** — a deliberate, confirmed purge operation.
