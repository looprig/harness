# Session-Isolated Embedded JetStream Design

**Date:** 2026-06-23  
**Status:** Approved design  
**Scope:** Local `swe` persistence, session discovery, and best-effort generated session titles.

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
      jetstream/
```

`<session-id>` is a canonical UUID. A new session mints its UUID before opening
an engine, creates the session directory with owner-only permissions, and opens
the embedded engine on `<session-dir>/jetstream`.

JetStream data in `jetstream/` remains the authoritative durable event and
object log for that session. `session.json` is an atomically written, non-secret
manifest used only for session discovery and display:

- `session_id`
- `title`
- `title_source` (`generated` or `first_user_message`)
- `created_at`
- `last_active_at`
- `status` (`active` or `stopped`)

No `llm.ModelSpec`, API key, prompt, or transcript content is written to the
manifest. Its title is sanitized plain text with a bounded length.

## Process Ownership

Every active session owns one in-process JetStream server and one StoreDir.
Launching or clearing session A never opens, closes, or writes session B's
directory.

An OS-level exclusive `session.lock` is held for the lifetime of the embedded
engine. It rejects a second process attempting to open the *same* session
directory. This protects a resumed session from two embedded servers touching
the same StoreDir; the journal lease remains the single-writer fence within the
server.

This design intentionally isolates local embedded stores. It is not a
distributed NATS topology. Multi-process writers that must share one live
session require a future external JetStream cluster/client mode, where each
NATS node has its own StoreDir.

## Lifecycle

1. A new session mints an ID, creates `<id>/`, locks it, starts the engine on
   `<id>/jetstream`, and atomically writes an `active` manifest.
2. `--resume <id>` validates the UUID, opens and locks that exact directory,
   then restores from its embedded JetStream store.
3. `--list` enumerates only immediate UUID-named children of `sessions/`, reads
   their manifests, and reports corrupt or incomplete entries without starting
   their JetStream engines.
4. On each meaningful session activity, `last_active_at` is atomically updated.
   Clean shutdown writes `stopped` before releasing the session lock.
5. `/clear` fully shuts down the current session and closes its engine before
   minting and opening a separate new session directory. It has no effect on
   another process's session.

## Optional Model Tiers and Titles

The user-facing `swe.Config` gains an optional in-memory model catalog:

```go
type ModelCatalog struct {
	Economy  []llm.ModelSpec
	Standard []llm.ModelSpec
	Premium  []llm.ModelSpec
}
```

The lists are ordered. The first usable model in the selected tier is chosen;
there is no silent mid-session failover to a different model.

- An empty `Economy` list means no title-model call. The title falls back to a
  sanitized, length-bounded first user message.
- A non-empty `Economy` list enables one asynchronous, best-effort title call
  after the first completed turn. It cannot block a turn, session creation,
  shutdown, or `/clear`.
- An empty `Standard` list preserves the existing default primary model.
- An empty `Premium` list means no premium model can be selected.

Title-model output is untrusted text: it is validated as one short plain-text
line before an atomic manifest update. Failure, timeout, empty output, or an
invalid output preserves the first-user-message fallback.

## Testing and Acceptance

- Two different session IDs can be active concurrently, each with a different
  StoreDir, without affecting the other's append or shutdown path.
- A second open of the same session directory returns a typed lock-held error
  before starting a JetStream server.
- New, resume, clear, and list follow the directory contract above.
- Listing does not start JetStream engines and handles missing/corrupt manifests
  deterministically.
- Metadata writes are atomic and never contain secrets.
- Economy title generation succeeds without delaying turns; absent or failed
  Economy configuration produces the first-user-message fallback.
