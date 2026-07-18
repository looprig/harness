# Session versioning, configuration adoption, and journal migration

**Status:** design accepted 2026-07-17. Phase 1 (configuration manifest,
drift assessment, adoption) is ready for implementation planning. Phase 2
(migration framework) is specified but deliberately deferred until a real
schema break exists.

**Date:** 2026-07-16, revised 2026-07-17.

## Purpose

Harness Sessions must remain reusable as their Rig changes. Adding or removing
tools, changing a model or prompt, tightening confinement, changing permission
policy, or upgrading an application must not automatically make an otherwise
readable Session unrestorable.

The current configuration fingerprint detects behavioral drift but is also used
as a default restore lock. That combines three different questions:

1. Can the current code decode and validate the durable journal?
2. Does the current Rig behave differently from the configuration last adopted
   by this Session?
3. If the journal cannot be reconstructed directly, is there a registered
   migration that can transform its interpretation safely?

This design separates those concerns. Journal compatibility and migration are
driven by durable schema versions. Configuration manifests and fingerprints
describe behavior and support informed restore decisions. Migrations preserve
the Session ID and the original append-only history.

The governing rule: **decodability gates restore; configuration drift gates it
only when policy says so.**

## Current state (verified 2026-07-17)

What exists in the code today, so the delta this design builds is explicit:

- `ConfigFingerprint` (`pkg/event/config_fingerprint.go`) is a JSON struct
  compared field-by-field via `Equal`, not a hash. Individual fields carry
  digests (`SystemPromptRev`, `ToolPolicyRev`, …) but there is no top-level
  canonical encoding and no domain or version tag. Only the hustle-topology
  sub-digest (`pkg/rig/fingerprint.go`, `canonicalHustleTopologyMaterial`)
  uses a canonical length-delimited encoding with an explicit domain string.
- `ToolPolicyRev` hashes **sorted tool names only**. Tool input/output schemas
  are not fingerprinted: a tool whose schema changed while keeping its name
  produces an identical fingerprint.
- Restore compares the live fingerprint permanently against the **first
  `SessionStarted`** (`internal/sessionruntime/restore.go`,
  `firstConfigFingerprint`). There is no adoption baseline.
- `WithAllowConfigMismatch` (`pkg/rig/options.go`) and `ConfigMismatchError`
  (`pkg/session/errors.go`) are live and load-bearing; the single boolean is
  the only drift decision available.
- Two durable versions exist: the sessionstore frame version
  (`pkg/sessionstore/envelope.go`, `envelopeVersion = 1`, fails closed on any
  other value) and the event wire-envelope schema version
  (`pkg/event/marshal.go`, `schemaVersion = 1`). The event `v` field is
  **stamped on write but never read on decode** — decode dispatches purely on
  the `"type"` tag. No other durable record family carries its own version.
- The restore flow is: acquire lease → lease fence → fingerprint and agent
  checks → `RestoreStarted` → rebuild loops/workspace/context →
  `RestoreDone`, with `RestoreErrored` on failure.
- No migration machinery exists: no `pkg/migration`, no `internal/migration`,
  no `ConfigurationAdopted`/`MigrationStarted`/`MigrationApplied`/
  `MigrationFailed` events, no configuration epochs.

## Prior art

Surveyed 2026-07-17 against codex and opencode.

**codex** does not version its rollout JSONL history at all. Compatibility is
pure serde tolerance: optional/defaulted fields, aliases for renames,
catch-all variants on leaf enums, and unparseable lines are logged, counted,
and skipped — never fatal. Resume is replay, not validation; the only drift
check is a non-blocking warning when the recorded model differs from the
current one. Strict, numbered, checksummed migrations exist only for its
SQLite index, which is safe to be strict about because it is a rebuildable
cache over the JSONL.

**opencode** versions its storage engine (two forward-only migration ledgers)
but never rewrites message bodies: JSON documents in columns, absorbed by
tolerant decoders with decoding defaults. Every session records the writing
build's version, but nothing ever checks it. The recorded model is re-resolved
against the current catalog at runtime (typed error if unavailable), and when
replaying under a different model, provider-specific metadata (reasoning
signatures) is stripped rather than replayed.

Neither has a configuration manifest, drift assessment, adoption events, or a
history-migration framework. Two lessons follow:

1. **Additive, tolerant schema evolution carries almost the entire
   compatibility load.** With that discipline, history migrations are
   approximately never needed.
2. **Nobody blocks restore on behavioral drift.** Harness goes further than
   both by making drift *auditable* (typed assessment, durable adoption
   record) — but the default posture for ordinary drift is accept-and-record,
   not block.

Harness's stricter substrate (append-only fenced journal, leases, attribution,
audit) justifies the manifest/adoption layer neither comparator has. It does
not justify building the migration framework speculatively.

## Decisions

- Work is phased: Phase 1 builds configuration manifest, drift assessment,
  and adoption; Phase 2 (migration framework) is deferred until the first
  real schema break. Migration event names and invariants are reserved now.
- Durable record schemas evolve additively by policy (see "Evolution
  policy"); migration is the last resort, not the mechanism of change.
- A migration preserves the existing Session ID.
- Committed journal records are never rewritten or deleted.
- Wire and schema versions determine whether a migration is needed.
- A configuration fingerprint is not a journal version.
- Configuration drift is normally a restore decision, not a hard
  compatibility failure. Default policy: auto-accept and record behavioral
  drift; require an explicit decision only for security-posture drift.
- Headless default is fail-secure: reject on security-posture drift unless an
  explicit deployment policy accepts it.
- Corrupt records, digest mismatches, unsupported schemas with no migration,
  invalid attribution, and unsafe reconstruction remain hard failures.
- Restore compares the live Rig against the latest configuration adopted by
  the Session, not permanently against the original `SessionStarted`
  fingerprint.
- Accepting configuration drift creates a durable adoption event with the
  decision source and an optional user message.
- Applying a migration creates a durable migration overlay or reconstruction
  checkpoint. Original records remain available for audit.
- Incomplete migrations have no effect. A single committed event is the
  migration boundary.

## Evolution policy (additive-first)

This policy is what keeps Phase 2 unbuilt. For every durable record family:

- Fields are only ever **added**, and always optional with a defined default.
  Fields are never removed or repurposed while journals may contain them.
- Decoders **ignore unknown fields** rather than failing on them.
- Renames are handled by dual-read (accept old and new names) for as long as
  old journals exist.
- An event type is never removed while its state contribution may still be
  needed for reconstruction, except through a registered Phase 2 migration.
- Load-time shims (normalizing a legacy shape into the current in-memory
  form) are preferred over rewriting stored bytes.

Under this policy, a journal migration is required only for the changes listed
in "What requires migration" — an event expected to occur rarely, possibly
never.

## Separate identities

The durable model uses five distinct identities:

| Identity | Purpose |
| --- | --- |
| Journal frame version | Decode the storage envelope surrounding a record. |
| Record schema version | Decode an event, command, fence, or private record body. |
| Migration ID and version | Select and audit a transformation between supported schemas. |
| Configuration manifest and fingerprint | Describe the behavior a Session is running under. |
| Configuration epoch | Order the configurations explicitly adopted within one Session. |

Harness already has a session-store envelope version and an event-envelope
schema version. Those are durable wire contracts and remain the basis for
codec selection — with one Phase 1 correction: the event envelope's `v` field
is currently write-only. Decode must start reading it and fail with a typed
`UnsupportedSchemaError` naming the version when it exceeds the supported
range. That typed error is the dispatch hook the Phase 2 migration layer will
catch. Equivalent explicit versions are required for every durable record
family that can evolve incompatibly.

## Fingerprint mismatch versus digest mismatch

These are different in kind and must never share a code path.

**Fingerprint mismatch** means the configuration evolved: the live Rig's
manifest differs from the last adopted one. This is normal life. It flows
through drift assessment and a restore decision, and is recorded durably when
accepted.

**Digest mismatch** means *the bytes are not the bytes that were committed*.
Its only causes are storage corruption (torn writes, bitrot, buggy backends),
a blob store returning content that does not hash to the recorded digest
(overwritten, wrongly deduplicated, or garbage-collected blobs), manual
tampering with journal files, or — in Phase 2 — a migration source range that
changed under the writer lease. None of these are "the config evolved"; all
of them mean the durable record can no longer be trusted. There is no safe
decision a user could make about corrupted history, because Harness cannot
even show them truthfully what the history was. Digest mismatch therefore
stays a hard failure. In a healthy deployment it should fire approximately
never; when it fires, it is surfacing a storage bug or tampering, and failing
loudly is the feature.

## Configuration manifest

`ConfigManifest` is a canonical, bounded, secret-free description of behavior
that may matter when a Session is restored. It replaces a hash-only view as
the primary compatibility artifact. The public shape lives in `pkg/event`
beside the legacy `ConfigFingerprint`; `pkg/rig` assembles it, mirroring how
`fingerprint.go` works today.

The manifest includes:

- manifest schema version;
- application and role identity;
- Loop topology, primers, active primer, delegates, and policy revisions;
- model identities and relevant capability or request-shape identity;
- system-prompt digests, never raw prompts;
- model-facing tool entries as `{name, input-schema digest, output-schema
  digest}` — closing the current names-only gap;
- optional external capability identities, including MCP server and catalog
  identities;
- confinement or sandbox posture;
- permission-policy digests;
- workspace placement identity and trust mode;
- application-defined, secret-free compatibility fields.

Credentials, bearer tokens, environment contents, raw prompts, tool results,
private keys, and other secrets never enter a manifest.

The configuration fingerprint is:

```text
SHA-256(canonical encoding of ConfigManifest)
```

The canonical encoding has an explicit domain tag
(`looprig/config-manifest/v1`), a manifest schema version, stable field
ordering, length-delimited values, and deterministic collection ordering —
extending the pattern the hustle-topology sub-digest already uses. A
fingerprint accelerates equality checks and content addressing; the manifest
supplies the detail needed to explain drift. The legacy `ConfigFingerprint`
struct remains as the read shape for old journals.

Large manifests may be stored as content-addressed blobs. The journal event
then carries the manifest digest, schema version, bounded summary, and blob
reference. Restore must verify the referenced content against the digest
before using it (a failure there is a digest mismatch, and hard).

## Configuration freeze and adoption

A candidate manifest freezes only after the application has fully assembled
the Rig configuration needed by a Session:

- all Loop definitions and modes are known;
- application-owned tools are resolved;
- externally discovered tools required for the initial tool catalog are known;
- tool schemas and policy revisions are known;
- confinement, permission, workspace, and trust posture are resolved;
- secrets have been excluded.

Freezing a candidate does not by itself mutate a Session. It becomes the
Session's adopted baseline at one of these boundaries:

1. **New Session.** `SessionStarted` adopts configuration epoch 1.
2. **Accepted restore drift.** `ConfigurationAdopted` commits the next epoch
   before `RestoreDone`.
3. **Successful journal migration (Phase 2).** `MigrationApplied` commits the
   migrated interpretation, followed by `ConfigurationAdopted` when the live
   configuration also changes.
4. **Explicit live reconfiguration.** A new epoch may be adopted only at a
   defined safe boundary, initially Session idle with no unresolved operation
   that depends on the replaced configuration.

An ordinary restore with no drift appends no new configuration epoch.

The latest committed `SessionStarted` or `ConfigurationAdopted` event is the
baseline for the next restore:

```text
SessionStarted             epoch 1
ConfigurationAdopted       epoch 2
MigrationApplied
ConfigurationAdopted       epoch 3
                                  ^
                         next restore baseline
```

Legacy journals that carry only `ConfigFingerprint` remain readable. When no
complete prior manifest exists, drift assessment is limited to the fields the
legacy fingerprint can distinguish. The first accepted restore under the new
system installs a complete manifest and establishes a current baseline.

## Drift assessment

Restore produces a structured assessment before constructing live Loops. It
compares the latest adopted manifest to the candidate live manifest and
reports typed changes rather than only hash inequality. Each change carries a
category, old and new safe identity, and a severity.

Severity has two levels, classified by one question: **does the change expand
what the session can touch?**

| Severity | Meaning | Changes |
| --- | --- | --- |
| Info | Auto-accept and record | Tool added, removed, or schema changed; prompt digest changed; model changed; MCP/external catalog changed; topology or delegation changed; confinement became **stricter**; permission policy **narrowed**; application-defined fields (default). |
| Warn | Explicit decision required | Workspace root or placement changed; confinement **broadened**; permission policy **broadened**; trust mode changed; agent kind or adapter changed. |

Direction-sensitive fields (confinement, permissions) classify by direction:
tightening is Info, broadening is Warn. Severity is advisory input to
application policy, not authority granted by the manifest; applications may
reclassify categories (for example, a deployment may promote model changes to
Warn).

**Default policy.** Interactive frontends prompt on Warn and auto-accept Info
(recording an adoption event either way when anything drifted). The headless
default is fail-secure: accept when all changes are Info, reject when any
change is Warn.

**Why reject Warn in headless.** Warn is, by construction, "this change
expands what the session can touch." Interactively, a human decides. Headless,
there is nobody to ask, so the default must guess — and guessing "accept"
means an unattended process silently comes up with a more permissive posture
than the session ever ran under. The scenarios this protects against are
mundane: a deployment template change flips confinement off; a session
directory restored on a different host now points its workspace at a different
repo; an edited permission policy hands every dormant session broader grants
on next restore. Each is exactly the case where a human would want to be
asked. The cost is low because the failure is benign and self-describing:
restore fails with a typed `RestoreRejectedError` carrying the full
assessment, and the operator either fixes the configuration or installs an
explicit policy accepting that category. The default refuses to guess; it does
not refuse to be configured. The inverse default gets the worse outcome:
nothing fails, and the durable record claims "policy accepted" when nobody
decided anything.

## Restore decision interface

`WithAllowConfigMismatch` is replaced by an explicit decision interface. A
single boolean cannot distinguish accepted tool drift from an unsafe workspace
move, cannot record who decided, and cannot provide a useful warning.

```go
type RestoreDecider interface {
	DecideRestore(ctx context.Context, a DriftAssessment) (RestoreDecision, error)
}

type RestoreDecision struct {
	Accept  bool
	Source  DecisionSource // user | policy | operator
	Actor   string         // optional stable identity
	Message string         // optional user-authored note
}
```

Harness ships the default policy decider described above (accept all-Info,
reject any-Warn). The TUI, `serve`, and other interactive frontends install a
decider that presents the assessment and asks. The Harness owns the typed
assessment and the durable adoption record; frontends own presentation and the
final policy choice.

Compatibility: `WithAllowConfigMismatch` remains as a deprecated shim that
installs an accept-everything decider with `Source: policy`, so existing
callers keep compiling while the durable record still shows what happened.
`ConfigMismatchError` is superseded by `RestoreRejectedError`, which carries
the full drift assessment.

## Configuration adoption event

`ConfigurationAdopted` is an Enduring, session-scoped event containing:

- Session ID through its header;
- monotonically increasing configuration epoch;
- previous and adopted manifest fingerprints;
- adopted manifest schema and reference or bounded inline form;
- classified drift summary;
- decision source: user, policy, operator, or migration;
- stable actor identity when available;
- application version or build identity;
- optional user-authored message explaining the decision.

The optional user message is durable user-authored data, not an instruction
that automatically gains authority during future prompt construction.

The event is appended after the restore lease is held and after the decision
is validated, but before `RestoreDone`. A failed append means the Session does
not come up under the unrecorded configuration.

## What requires migration

Configuration drift does not itself require a journal migration. Old tool
calls, removed tools, historical sandbox posture, and historical model
identities are data in the journal. Restore does not require the old
implementation to remain installed if current codecs can decode the records
and reconstruct committed conversation state.

A migration is required only when current code cannot safely reconstruct the
Session from the durable schema without transforming its interpretation.
Under the additive evolution policy these are rare, deliberate events:

- an unsupported record-envelope or event schema version;
- a removed event type whose state contribution has no current decoder;
- an incompatible change to identity or routing semantics;
- historical records missing information that must be supplied through an
  explicit, reviewed rule;
- a reconstruction checkpoint format that must be upgraded.

Malformed JSON, digest mismatch, invalid IDs, attribution conflicts,
impossible state transitions, and other corruption are not ordinary migration
triggers. They remain hard failures unless a narrowly defined repair migration
explicitly recognizes and validates that historical condition.

## Phase 2 (deferred): migration framework

Everything in this section is specified so names and invariants are reserved,
but **none of it is built until the first real schema break exists**. Building
it speculatively means designing against imagined migrations; the additive
evolution policy is expected to absorb ordinary change indefinitely.

### Migration registration

Public migration contracts belong in `pkg/migration`. Selection, chaining,
validation, and commit logic belong in `internal/migration`.

The conceptual API is:

```go
type Migrator interface {
	Descriptor() Descriptor
	Plan(context.Context, Source) (Plan, error)
	Apply(context.Context, Source) (Result, error)
}

type Descriptor struct {
	ID         string
	Version    uint32
	FromSchema SchemaSet
	ToSchema   SchemaSet
}
```

`Descriptor.ID` is stable and globally names the semantic migration, for
example `"coderig/session-tools-v1-to-v2"`. `Version` changes only when the
migration implementation's accepted input or produced output changes. A
changed version is a distinct audited program identity and must never silently
replace a previously committed one.

Applications register migrators at composition:

```go
rig.WithMigrations(
	migration.Register(toolHistoryMigration),
	migration.Register(legacyRoutingMigration),
)
```

Registration is immutable for a compiled Rig. Harness validates duplicate IDs,
ambiguous source ranges, invalid chains, and cycles before a Session restore
is attempted.

### Migration input and output

`Source` is a bounded, read-only view over:

- Session ID;
- source record sequence range;
- raw durable envelope bytes;
- record kind and exposed schema versions;
- stable source digest;
- latest committed migration and configuration baselines.

It does not expose mutable storage or internal live runtime objects.

`Plan` explains:

- which records or checkpoint are affected;
- source and target schemas;
- whether the migration is lossless, semantic, or repair-oriented;
- expected output kind and bounds;
- warnings requiring user review.

`Result` contains either:

- validated current-schema overlay records; or
- a validated reconstruction checkpoint plus any required audit records.

Harness, not the migrator, stamps durable IDs and appends the commit records.

Before commit, Harness validates:

- the Session ID is unchanged;
- the source range and digest still match under the held lease;
- the declared migration ID and version match registration;
- output schemas are supported;
- record identity, routing, attribution, and content invariants hold;
- output sizes and record counts are bounded;
- no output attempts to rewrite or delete source records;
- the result is deterministic for the committed source and migration identity;
- reapplying a committed migration is idempotent.

### Command migration adapter

A later optional adapter may let users write migrations as executables:

```go
migration.Command(CommandSpec{
	Path: "/absolute/path/to/migrate-session",
	Args: []string{"--format", "jsonl"},
})
```

The process exchanges bounded JSONL over stdin/stdout. It is launched with an
explicit argv, never a shell string. Its output is untrusted and receives the
same validation as an in-process migrator.

The application owns confinement for migration processes. A migration should
default to no network and read-only access outside explicitly granted
temporary output. Environment variables are allowlisted; credentials are not
forwarded by default.

### Migration events and atomicity

Migration lifecycle is represented by Enduring, session-scoped events:

- `MigrationStarted`
- `MigrationApplied`
- `MigrationFailed`

`MigrationStarted` records intent after the lease and source digest are
established. It is not a commit point.

`MigrationApplied` is the single commit point. It records:

- migration chain and program versions;
- source sequence range and digest;
- target schema set;
- overlay or checkpoint reference and digest;
- actor and application identity;
- optional user-authored message.

Restore ignores an unmatched `MigrationStarted`. It uses only the latest valid
committed migration chain. `MigrationFailed` records a bounded, redacted
failure classification and leaves the prior interpretation baseline unchanged.

Backends continue to use append-only, fenced journal semantics. Migration
commit must occur under the same Session writer lease used by restore.

### Automatic versus approved migrations

- Lossless, first-party, registered codec migrations may be allowed
  automatically by application policy.
- Semantic, repair-oriented, user-authored, or external-command migrations
  require explicit approval unless a deployment has installed an equally
  explicit trusted policy.

Regardless of approval policy, every applied migration is durably recorded.

## Restore order

The target restore sequence is:

1. acquire the Session lease and append the lease fence;
2. inspect frame and record schema versions without constructing live Loops;
3. locate and validate the latest committed migration baseline (Phase 2;
   trivially absent in Phase 1);
4. select a supported decoder or registered migration chain;
5. plan migration and obtain required approval (Phase 2);
6. apply, validate, and commit `MigrationApplied` when needed (Phase 2);
7. replay the effective migrated view and validate reconstructability;
8. load the latest adopted configuration manifest;
9. freeze the candidate live manifest and produce the drift assessment;
10. obtain the configured restore decision;
11. append `RestoreStarted`;
12. append `ConfigurationAdopted` when a new epoch is accepted;
13. reconstruct Loops, gates, workspace, context, and runtime state;
14. append `RestoreDone`.

The exact placement of `RestoreStarted` relative to migration lifecycle events
needs final implementation review. The invariant is that no live Session is
returned until migration, configuration adoption, reconstruction, and
`RestoreDone` have all committed.

## MCP consequence

MCP tool catalogs participate in the configuration manifest, but catalog drift
does not make an old Session unreadable. Historical MCP calls remain journal
data. A changed live MCP catalog is assessed like any other tool change:

- unchanged tools may continue;
- removed tools fail clearly if invoked;
- changed schemas are not silently substituted for the old model-facing
  definition;
- accepting a refreshed catalog adopts a new configuration epoch;
- no journal migration is required merely because an MCP server added,
  removed, or changed tools.

## Future work (neither phase)

- **Cross-model replay accommodation.** When the resume model differs from
  the recording model, provider-specific reasoning signatures and metadata in
  historical assistant turns may not be replayable verbatim (opencode strips
  them and downgrades reasoning parts). This is a drift-layer accommodation
  during context reconstruction, not a journal migration; it needs its own
  design when multi-model resume becomes a supported flow.
- **Old-binary tolerance.** The frame version fails closed on `v != 1`,
  correctly. Whether an older binary should skip-or-surface unknown Enduring
  event types written by a newer binary (codex's `ignore_missing` posture),
  rather than refusing the whole Session, is a deliberate open choice for the
  first frame or event version bump.

## Testing (Phase 1)

- Golden-vector determinism tests for the canonical manifest encoding, plus a
  fuzz target on its decoder.
- A table-driven drift-classification matrix covering every category × both
  directions for direction-sensitive fields.
- Epoch ordering and adoption-baseline selection across multi-epoch journals.
- Legacy fingerprint-only journals: limited assessment, first-restore manifest
  installation.
- Both default decider paths (all-Info accept, any-Warn reject), the
  interactive decider seam, and the `WithAllowConfigMismatch` shim.
- `UnsupportedSchemaError` on a future event `v`, and continued fail-closed
  behavior at the frame version.

## Open questions for the implementation design

1. Exact public shapes of `ConfigManifest` and the drift-classification
   types (the `RestoreDecider` seam above is the accepted direction).
2. Whether manifests are always inline, always blob-backed, or
   size-dependent.
3. Which durable record families need independent schema versions beyond
   their existing envelopes.
4. Overlay records versus reconstruction checkpoints for each migration class
   (Phase 2).
5. How migration program digests are built for in-process Go implementations
   (Phase 2).
6. How the TUI, `serve`, and headless callers present and answer restore
   decisions (mechanism decided; presentation per frontend).
