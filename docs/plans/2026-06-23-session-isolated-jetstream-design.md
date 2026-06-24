# Session-Isolated Embedded JetStream Design

**Date:** 2026-06-23  
**Status:** Implemented (2026-06-23) — see the implementation plan and the
`feature/session-isolated-jetstream` branches in `looprig` and `../swe`.  
**Scope:** Local `swe` persistence, session discovery, best-effort generated session titles, and explicit clean-slate removal of the legacy shared StoreDir.

## Implementation Notes

- The single-instance guard is a **Unix-only** `syscall.Flock` on
  `<session-dir>/session.lock`, acquired before the embedded server starts and
  released on `Close`. Non-Unix platforms fail closed
  (`errSessionLockUnsupported`) rather than run an unguarded engine.
- The persistence session API is keyed on `looprig/pkg/uuid` (not `google/uuid`)
  for internal consistency with the journal/session packages and to avoid adding a
  direct dependency to `../swe`.
- The session manifest is created (active) when a session is opened, so a new
  session is immediately listable; `Init` repairs a missing **or corrupt**
  manifest from the authoritative store on resume.
- The destructive legacy purge is exposed as `swe --purge-legacy-sessions`
  (mutually exclusive with `--list`/`--resume`); it derives and
  containment-checks the `~/.looprig/jetstream` path internally and never accepts
  a deletion path.

## Goal

Give every durable session its own embedded JetStream StoreDir so independently
running CLI processes never share JetStream files. Sessions remain resumable and
listable without starting every embedded server.

## Directory Contract

```text
<looprig-data-root>/                 # $XDG_DATA_HOME/looprig or ~/.looprig
  sessions/
    <session-id>/
      session.json
      session.lock
      nats/
```

`<looprig-data-root>` follows the existing XDG rule: `$XDG_DATA_HOME/looprig`
when `XDG_DATA_HOME` is set, otherwise `~/.looprig`. `<session-id>` is a
canonical UUID. A new session mints its UUID before opening
an engine, creates the session directory with owner-only permissions, and opens
the embedded engine on `<session-dir>/nats`. NATS creates its own `jetstream/`
child below that configured StoreDir, so naming the configured directory `nats`
avoids a misleading `jetstream/jetstream/` contract.

JetStream data in `jetstream/` remains the authoritative durable event and
object log for that session. `session.json` is an atomically written, non-secret
manifest used only for session discovery and display:

- `session_id`
- `title`
- `title_source` (`none`, `generated`, or `first_user_message`)
- `created_at`
- `last_active_at`
- `status` (`active` or `stopped`)

No raw `llm.ModelSpec`, API key, prompt, or transcript field is written to the
manifest. A deliberately selected title is allowed: either a sanitized,
length-bounded snippet of the first accepted user message or a generated summary
of the first exchange. The session directory is owner-only because both title
forms and the JetStream transcript are private user data.

## Process Ownership

Every active session owns one in-process JetStream server and one StoreDir.
Launching or clearing session A never opens, closes, or writes session B's
directory.

An OS-level exclusive `session.lock` is held for the lifetime of the embedded
engine. It is acquired before the server starts and rejects a second process
attempting to open the *same* session directory. This protects a resumed session
from two embedded servers touching the same StoreDir; the journal lease remains
the single-writer fence within the server.

Every engine-construction failure closes the lock before returning. Engine close
releases the lock even when the NATS connection drain reports an error.

The initial local-embedded implementation supports Unix-like hosts through a
small build-tagged `syscall.Flock` adapter and returns a typed unsupported-platform
error elsewhere. It does not add a dependency. Creation and traversal reject
symlinks, clean every derived path, and verify each path stays under the resolved
sessions root before opening it.

This design intentionally isolates local embedded stores. It is not a
distributed NATS topology. Multi-process writers that must share one live
session require a future external JetStream cluster/client mode, where each
NATS node has its own StoreDir.

## Lifecycle

1. A new session mints an ID, creates `<id>/`, locks it, starts the engine on
   `<id>/nats`, and atomically writes an `active` manifest.
2. `--resume <id>` validates the UUID, opens and locks that exact directory,
   then restores from its embedded JetStream store. A missing or corrupt
   manifest never prevents resume: JetStream is authoritative and the manifest
   is rebuilt after a successful restore.
3. `--list` enumerates only immediate UUID-named children of `sessions/`, reads
   their manifests, and reports a `metadata-invalid` row for a missing or
   corrupt manifest without starting a JetStream engine. It never silently
   omits a UUID-named session directory.
4. One metadata writer serializes every manifest update, writes a temporary
   owner-only file, fsyncs it, renames it atomically, and fsyncs the parent
   directory. It updates `last_active_at` on creation, successful resume,
   accepted user input, a terminal turn, and clean shutdown.
   Creation and restore treat the first manifest write as required: failure
   closes the just-opened engine and returns a typed setup error. Later metadata
   refresh failures are logged without interrupting an already-running session.
5. `status` is the last persisted lifecycle state, not a liveness probe. An
   `active` manifest after a process crash is displayed as `active/unclean`;
   only a clean shutdown changes it to `stopped` before releasing the lock.
6. `/clear` creates and validates the replacement session first, then swaps the
   TUI to it and asynchronously closes the old session and its engine. The two
   session engines may overlap briefly because their directories are isolated.
   A failed replacement leaves the old session open. Neither path affects a
   session owned by another CLI process.

## Composition and Legacy Cleanup

The existing process-wide `startPersistence` / `swe.Persistence` composition is
replaced by a session-store factory. It mints or receives a session ID, resolves
that session's directory, acquires its lock, starts its engine, and constructs
the journal dependencies for that engine only. The global JetStream catalog is
not used in isolated-store mode; directory manifests provide listing.

`--list` runs before any engine is opened. It enumerates manifests directly.

There is no legacy compatibility or migration mode. The user has explicitly
accepted permanent removal of all pre-existing sessions in the shared legacy
StoreDir. An explicit `--purge-legacy-sessions` command performs that one-way
cleanup before the isolated-store workflow is used.

The purge command never starts a JetStream engine. It resolves the exact legacy
StoreDir through the same containment checks as persistence startup, rejects a
symlinked path, and removes only that verified directory; it must never remove
`~/.looprig`, its logs, or `sessions/`. The operator must stop every old `swe`
process before running the purge, because the older process has no lock that can
reliably prove it is not actively using the legacy files. A successful purge
prints the removed path. A missing legacy StoreDir is a successful no-op.

## Optional Model Tiers and Titles

The user-facing `swe.Config` gains an optional in-memory model catalog:

```go
type ModelCatalog struct {
	Economy  []llm.ModelSpec
	Standard []llm.ModelSpec
	Premium  []llm.ModelSpec
}
```

The lists are ordered. Configuration validates every supplied spec: it requires
a non-empty model and known provider, then applies `ModelSpec.Validate`. The
tier resolver materializes a provider client for the selected Standard model and,
only when a title is due, the selected Economy model; the two may use different
providers. The first model in each selected tier is used with no silent
mid-session failover. Specs, including their API keys, remain in memory and are
never logged or serialized.

- An empty `Economy` list means no title-model call. On the first accepted user
  input, the title immediately falls back to a sanitized, length-bounded first
  non-empty text block so a crashed first turn is still listable. A multimodal
  input with no text keeps `title_source:none` until a generated title exists.
- A non-empty `Economy` list enables one asynchronous, best-effort title call
  after the first completed turn. A bounded background context and the metadata
  writer replace the fallback only on valid success; it cannot block a turn,
  session creation, shutdown, or `/clear`.
- A non-empty `Standard` list selects its first model for normal orchestrator
  and subagent turns. An empty `Standard` list preserves the existing default
  primary model.
- `Premium` is intentionally catalog-only in this change. An empty list means
  no premium model is available; a future explicit tier-selection feature may
  consume it. There is no implicit escalation based on task content or errors.

The title request has no tools, a fixed title-only system instruction, a hard
timeout, and bounded excerpts of the first user message and first final assistant
reply. Title-model output is untrusted text: it is validated as one short
plain-text line before an atomic manifest update. Failure, timeout, empty output,
or an invalid output preserves the first-user-message fallback.

## Testing and Acceptance

- Two different session IDs can be active concurrently, each with a different
  StoreDir, without affecting the other's append or shutdown path.
- A second open of the same session directory returns a typed lock-held error
  before starting a JetStream server.
- New, resume, clear, and list follow the directory contract above; a failed
  clear replacement leaves the original session usable.
- Listing does not start JetStream engines, displays every UUID-named directory,
  and reports missing/corrupt manifests as `metadata-invalid`.
- Resume repairs a missing/corrupt manifest from the authoritative store.
- Metadata writes are serialized, crash-safe, and never contain model specs or
  secrets.
- Economy title generation succeeds without delaying turns; absent or failed
  Economy configuration produces the first-user-message fallback.
- `--purge-legacy-sessions` deletes only the verified legacy StoreDir, is a
  no-op when it is absent, and never deletes the broader Looprig data directory.
- Clean single-instance shutdown still has no unexpected lease-lost error-level
  log. Directory isolation removes shared-StoreDir corruption; it does not
  replace the independent shutdown/lease-lifecycle fix.
