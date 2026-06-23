# Session-Isolated Embedded JetStream Design

**Date:** 2026-06-23  
**Status:** Approved design  
**Scope:** Local `swe` persistence, session discovery, best-effort generated session titles, and safe transition away from the legacy shared StoreDir.

## Goal

Give every durable session its own embedded JetStream StoreDir so independently
running CLI processes never share JetStream files. Sessions remain resumable and
listable without starting every embedded server.

## Directory Contract

```text
~/.looprig/
  sessions/
    <session-id>/
      session.json
      session.lock
      nats/
```

`<session-id>` is a canonical UUID. A new session mints its UUID before opening
an engine, creates the session directory with owner-only permissions, and opens
the embedded engine on `<session-dir>/nats`. NATS creates its own `jetstream/`
child below that configured StoreDir, so naming the configured directory `nats`
avoids a misleading `jetstream/jetstream/` contract.

JetStream data in `jetstream/` remains the authoritative durable event and
object log for that session. `session.json` is an atomically written, non-secret
manifest used only for session discovery and display:

- `session_id`
- `title`
- `title_source` (`generated` or `first_user_message`)
- `created_at`
- `last_active_at`
- `status` (`active` or `stopped`)

No `llm.ModelSpec`, API key, prompt, or transcript is written to the manifest,
except for the deliberately selected title fallback: a sanitized, length-bounded
snippet of the first accepted user message. The session directory is owner-only
because both this title and the JetStream transcript are private user data.

## Process Ownership

Every active session owns one in-process JetStream server and one StoreDir.
Launching or clearing session A never opens, closes, or writes session B's
directory.

An OS-level exclusive `session.lock` is held for the lifetime of the embedded
engine. It is acquired before the server starts and rejects a second process
attempting to open the *same* session directory. This protects a resumed session
from two embedded servers touching the same StoreDir; the journal lease remains
the single-writer fence within the server.

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
5. `status` is the last persisted lifecycle state, not a liveness probe. An
   `active` manifest after a process crash is displayed as `active/unclean`;
   only a clean shutdown changes it to `stopped` before releasing the lock.
6. `/clear` creates and validates the replacement session first, then swaps the
   TUI to it and asynchronously closes the old session and its engine. The two
   session engines may overlap briefly because their directories are isolated.
   A failed replacement leaves the old session open. Neither path affects a
   session owned by another CLI process.

## Composition and Legacy Compatibility

The existing process-wide `startPersistence` / `swe.Persistence` composition is
replaced by a session-store factory. It mints or receives a session ID, resolves
that session's directory, acquires its lock, starts its engine, and constructs
the journal dependencies for that engine only. The global JetStream catalog is
not used in isolated-store mode; directory manifests provide listing.

`--list` runs before any engine is opened. It enumerates manifests directly.

The legacy `~/.looprig/jetstream` StoreDir is never overwritten or silently
discarded. Moving old sessions into individual stores requires an explicit,
user-approved migration policy because it must export each stream and its object
bucket into a distinct new server. Until that policy is approved and implemented,
the isolated-store switch must retain a readable legacy mode rather than hiding
existing resumable sessions.

## Optional Model Tiers and Titles

The user-facing `swe.Config` gains an optional in-memory model catalog:

```go
type ModelCatalog struct {
	Economy  []llm.ModelSpec
	Standard []llm.ModelSpec
	Premium  []llm.ModelSpec
}
```

The lists are ordered. Construction validates every supplied spec, including its
provider, and initializes only the client for the selected spec. The first model
in the selected tier is chosen; there is no silent mid-session failover to a
different model. Specs, including their API keys, remain in memory and are never
logged or serialized.

- An empty `Economy` list means no title-model call. On the first accepted user
  input, the title immediately falls back to a sanitized, length-bounded message
  snippet so a crashed first turn is still listable.
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

Title-model output is untrusted text: it is validated as one short plain-text
line before an atomic manifest update. Failure, timeout, empty output, or an
invalid output preserves the first-user-message fallback.

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
- The legacy shared StoreDir remains discoverable and readable until an explicit
  migration policy is approved and implemented.
- Clean single-instance shutdown still has no unexpected lease-lost error-level
  log. Directory isolation removes shared-StoreDir corruption; it does not
  replace the independent shutdown/lease-lifecycle fix.
