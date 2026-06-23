# JetStream Persistence — Issues Spec

**Date:** 2026-06-23
**Status:** Investigated, not yet fixed (this is a problem spec, not an implementation plan)
**Scope:** The embedded JetStream persistence layer only. TUI rendering issues are tracked separately and are explicitly **out of scope** here.
**Repos:** Core code in `looprig` (`pkg/persistence`, `pkg/journal`, `pkg/session`); composition + lifecycle in `swe` (`cmd/swe/main.go`, `swarms/swe/persistence.go`).

---

## Background / Architecture

- The CLI starts ONE **embedded JetStream engine** over an on-disk **StoreDir** at `~/.looprig/jetstream` (override: `$XDG_DATA_HOME/looprig/jetstream`). It is opened once at process start and closed at exit (`cmd/swe/main.go` `startPersistence` → `pkg/persistence.Open`; `pkg/persistence/embedded.go` `DefaultEngineOptions`).
- Per session there is one **event stream** (`looprig_session_<id>`), one **object bucket** (`OBJ_..._obj`), and a shared **lease KV bucket** (`KV_looprig_session_leases`). Files live under `~/.looprig/jetstream/jetstream/$G/streams/`.
- Writes are governed by a **single-writer lease/epoch model** (`pkg/journal/lease.go`):
  - KV entry per session: `leaseRecord{Epoch uint64, ExpiresAt time.Time}`.
  - **TTL = 30s** (`defaultLeaseTTL`). The holder renews `ExpiresAt` on a background **heartbeat goroutine** via CAS on its own revision.
  - Acquire is CAS: an expired entry (or none) can be taken over with a **monotonically bumped epoch**. A failed renew (higher epoch / different holder won the entry) marks the lease **lost** and closes `Lost()`.
  - The journal stamps the lease's `Epoch` into an opening `LeaseFence` and **refuses every `Append` once `Lost()` fires** (`pkg/journal/journal.go:145,158`).
- The session also keeps an **audit-only command intent-log**: `appendCommand` (`pkg/session/command_journal.go`) writes a `CommandRecord` to the journal **before every command dispatch** (including `Interrupt` and `Shutdown`). On append failure it **logs loudly and proceeds** — a lost audit record must never block a user action.

Diagnostic evidence gathered (2026-06-23):
- **Two `swe` instances running simultaneously** against the same store: pids `27335` and `30709` (both `go run ./cmd/swe`).
- `~/.looprig/looprig.log`: **73** `"looprig starting"` records vs **18** `"construct failed"` / lease-related failures.
- Reported error (ctrl+C): `intent-log command append failed (audit-only, proceeding) … err="journal: session … append refused: lease at epoch 1 lost"` — **epoch 1**, i.e. released/expired, **not** overtaken by a higher epoch.

---

## Issue 1 — No single-instance / cross-process lock on the StoreDir

**Severity:** High (data-integrity risk + cascading failures)

**Symptom:** Multiple `swe` processes can run at once against `~/.looprig/jetstream`. Confirmed live: pids `27335` + `30709`.

**Root cause:** Neither `pkg/persistence.Open` nor the embedded server takes an OS-level (cross-process) lock on the StoreDir. Two embedded JetStream servers pointing at the **same FileStore** risk storage-level conflict (shared `.blk`/index files) and lease-bucket contention. Nothing fails closed at startup.

**Proposed fix:** Acquire an **OS advisory file lock** (e.g. `flock`/`O_EXLOCK` on a `~/.looprig/jetstream/.lock` file) in `persistence.Open`, released on `Engine.Close`. A second instance either (a) **refuses to start** with a clear message ("another swe session is using ~/.looprig; close it first"), or (b) optionally blocks/waits behind a flag. Default: refuse fast.

**Acceptance:**
- Starting a second `swe` while one is running prints a clear single-instance error and exits non-zero, without touching the store.
- The lock is released on clean exit, on crash (OS-released), and on `kill`.
- Test: integration test that opens the engine twice against one StoreDir and asserts the second `Open` returns a typed `single-instance` error.

---

## Issue 2 — Startup `construct failed` / `run failed` under contention

**Severity:** Medium (hard startup failure; surfaced as a broken launch)

**Symptom:** `looprig.log` shows `open agent failed … err="construct failed"` followed by `tui exited with error … err="run failed"` (~18 occurrences). The TUI never comes up.

**Root cause:** When the lease (or store) is already taken, per-session construction (`swarms/swe/persistence.go` `openNew` → lease acquire → `NewSessionJournal`) fails, and `cmd/swe/main.go` `run` returns `exitFailed`. This is the same contention as Issue 1, manifesting at construct time.

**Proposed fix:** Largely **resolved by Issue 1** (single-instance lock prevents the contended construct). Additionally, surface a **human-readable** message instead of the opaque `construct failed` (distinguish "another instance holds the lease" from genuine setup errors).

**Acceptance:**
- With the single-instance lock in place, a normal launch never hits `construct failed` from lease contention.
- A genuinely held session (e.g. `--resume` of a live session) reports a specific, actionable error.

---

## Issue 3 — Lease-lost ERROR on ctrl+C shutdown (audit append races teardown)

**Severity:** Medium (alarming but non-fatal; pollutes the log with ERROR on every clean exit)

**Symptom:** On ctrl+C: `ERROR session: intent-log command append failed (audit-only, proceeding) … lease at epoch 1 lost`. Happens on (apparently) normal shutdown.

**Root cause:** `appendCommand` runs the audit write **before dispatching every command, including `Shutdown`/`Interrupt`** (`command_journal.go`). During teardown the lease is **released (or its heartbeat context cancelled)**, so by the time the final Shutdown command's audit record is appended, `Lost()` has fired and the journal refuses it (`journal.go:145`). The `epoch 1` (not bumped) confirms this is **own-lease release/expiry, not takeover** by another instance. It is deliberately tolerated ("audit-only, proceeding"), but logged at **ERROR**, which reads as a real failure.

**Proposed fix (pick one or combine):**
1. **Order teardown** so the final Shutdown/Interrupt audit append completes **before** `lease.Release()` and before the heartbeat-bearing context is cancelled.
2. **Downgrade the log** to `DEBUG`/`INFO` (not `ERROR`) when the append failure is a `*LeaseLostError`/`*JournalLeaseLostError` during shutdown — it is expected and audit-only.
3. **Skip the audit append** for `Shutdown`/`Interrupt` once teardown has begun or once `lease.Lost()` is observed.

Recommended: **(1) + (2)** — try to record the shutdown intent, but never log it as ERROR when the lease is legitimately gone.

**Acceptance:**
- A clean single-instance ctrl+C produces **no ERROR** about a lost lease.
- The shutdown command record is journaled when the lease is still held; when it isn't, the failure is logged at DEBUG and the exit proceeds.
- Test: simulate shutdown after `Release()` and assert the dispatch proceeds and does not emit an ERROR-level record.

---

## Issue 4 — Lease TTL / heartbeat robustness during long stalls

**Severity:** Low–Medium (can fault a live session mid-turn; needs confirmation)

**Symptom (hypothesized):** If the holder fails to renew within the **30s TTL** (a long blocked turn, a debugger pause, or a stalled process), the lease expires, `Lost()` fires, and **every subsequent `Append` is refused** for the rest of that session — even with no competing instance.

**Root cause (to confirm):** Heartbeat renewal vs. TTL margin. Need to verify the heartbeat runs on a fully **independent** ticker (not coupled to turn/event processing) and renews at a safe fraction of the TTL, and that a transient KV error doesn't prematurely mark the lease lost.

**Proposed fix:** Confirm the heartbeat is independent and resilient to transient KV errors (retry within the TTL before declaring lost). Consider a longer TTL or an explicit grace window. Add a metric/log when a renew is late.

**Acceptance:**
- A session that is idle or busy for >30s does not spuriously lose its lease.
- A single transient KV renew error does not mark the lease lost if a retry succeeds within the TTL.
- Test: clock-injected lease test covering a late-but-within-grace renew and a transient renew error.

---

## Issue 5 — Session stream accumulation (minor / follow-on)

**Severity:** Low

**Symptom:** Many `looprig_session_<id>` streams + object buckets accumulate under the StoreDir (each `/clear` and each launch opens a fresh session).

**Root cause:** Orphan/object GC exists (`pkg/journal/gc.go` `ObjectGC`) but **GC for resumed sessions is a documented follow-on**, and there is no retention/pruning of old session streams.

**Proposed fix:** Define a retention policy (cap count/age of kept sessions) and a prune path; complete resumed-session GC. Lower priority than 1–3.

**Acceptance:** Old sessions beyond the retention policy are pruned; store size stays bounded over many launches.

---

## Recommended priority order

1. **Issue 1** — single-instance OS file lock on the StoreDir (prevents the whole contention class; also resolves Issue 2).
2. **Issue 3** — clean ctrl+C: order the shutdown audit before lease release and stop logging the expected lost-lease at ERROR.
3. **Issue 4** — confirm/strengthen heartbeat robustness.
4. **Issue 5** — session retention/GC follow-on.

## Open questions

- Should a second instance **refuse** or **wait** for the lock by default? (Proposed: refuse, with an opt-in wait flag.)
- Is the heartbeat currently coupled to anything that a long turn can starve? (Issue 4 confirmation.)
- Do we want the shutdown command journaled at all, or is best-effort + DEBUG sufficient? (Issue 3 option 3.)

## Out of scope

TUI rendering issues raised separately — the missing "thinking" tag on some blocks, and orchestrator thinking appearing before a subagent's done line — are **not** part of this spec. (Working hypothesis for those is the live-tail strand, addressed by the `liveTailCap` commit-headroom fix already on `main`; to be re-verified on a fresh single-instance run and, if persistent, specced on their own.)
