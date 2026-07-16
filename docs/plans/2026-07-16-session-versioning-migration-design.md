# Session versioning, configuration adoption, and journal migration

**Status:** design direction captured; implementation planning deferred.

**Date:** 2026-07-16.

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

## Decisions

- A migration preserves the existing Session ID.
- Committed journal records are never rewritten or deleted.
- Wire and schema versions determine whether a migration is needed.
- A configuration fingerprint is not a journal version.
- Configuration drift is normally a restore decision, not a hard compatibility
  failure.
- Corrupt records, unsupported schemas with no migration, invalid attribution,
  and unsafe reconstruction remain hard failures.
- Restore compares the live Rig against the latest configuration adopted by the
  Session, not permanently against the original `SessionStarted` fingerprint.
- Accepting configuration drift creates a durable adoption event with the
  decision source and an optional user message.
- Applying a migration creates a durable migration overlay or reconstruction
  checkpoint. Original records remain available for audit.
- Incomplete migrations have no effect. A single committed event is the
  migration boundary.

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
schema version. Those are durable wire contracts and remain the basis for codec
selection. Equivalent explicit versions are required for every durable record
family that can evolve incompatibly.

## Configuration manifest

`ConfigManifest` is a canonical, bounded, secret-free description of behavior
that may matter when a Session is restored. It replaces a hash-only view as the
primary compatibility artifact.

The manifest should include:

- manifest schema version;
- application and role identity;
- Loop topology, primers, active primer, delegates, and policy revisions;
- model identities and relevant capability or request-shape identity;
- system-prompt digests, never raw prompts;
- model-facing tool names and input/output schema digests;
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

The canonical encoding must have an explicit domain and manifest schema version,
stable field ordering, length-delimited values, and deterministic collection
ordering. A fingerprint accelerates equality checks and content addressing; the
manifest supplies the detail needed to explain drift.

Large manifests may be stored as content-addressed blobs. The journal event then
carries the manifest digest, schema version, bounded summary, and blob reference.
Restore must verify the referenced content against the digest before using it.

## Configuration freeze and adoption

A candidate manifest freezes only after the application has fully assembled the
Rig configuration needed by a Session:

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
3. **Successful journal migration.** `MigrationApplied` commits the migrated
   interpretation, followed by `ConfigurationAdopted` when the live
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
complete prior manifest exists, drift assessment may be limited to the fields
the legacy fingerprint can distinguish. The first accepted restore under the
new system installs a complete manifest and establishes a current baseline.

## Drift assessment

Restore produces a structured assessment before constructing live Loops. It
compares the latest adopted manifest to the candidate live manifest and reports
typed changes rather than only hash inequality.

Example changes include:

- tool added, removed, or schema changed;
- model changed;
- prompt digest changed;
- confinement became stricter or broader;
- permission policy changed;
- topology or delegation changed;
- workspace identity changed;
- external capability catalog changed;
- application-defined field changed.

Each change has a category, old and new safe identity, and a severity hint.
Severity is advisory input to application policy, not authority granted by the
manifest.

The application chooses a restore policy:

- accept and report informational drift;
- require an interactive confirmation;
- reject under a headless or deployment policy;
- require a registered migration;
- apply a stricter application-specific rule for selected fields.

The Harness owns the typed assessment and durable adoption record. A TUI, HTTP
client, or headless application owns presentation and the final policy choice.

`WithAllowConfigMismatch` should be replaced by an explicit restore decision
interface. A single boolean cannot distinguish accepted tool drift from an
unsafe workspace move, cannot record who decided, and cannot provide a useful
warning.

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

The optional user message is durable user-authored data, not an instruction that
automatically gains authority during future prompt construction.

The event is appended after the restore lease is held and after the decision is
validated, but before `RestoreDone`. A failed append means the Session does not
come up under the unrecorded configuration.

## What requires migration

Configuration drift does not itself require a journal migration. Old tool calls,
removed tools, historical sandbox posture, and historical model identities are
data in the journal. Restore does not require the old implementation to remain
installed if current codecs can decode the records and reconstruct committed
conversation state.

A migration is required when current code cannot safely reconstruct the Session
from the durable schema without transforming its interpretation. Examples:

- an unsupported record-envelope or event schema version;
- a removed event type whose state contribution has no current decoder;
- an incompatible change to identity or routing semantics;
- historical records missing information that must be supplied through an
  explicit, reviewed rule;
- a reconstruction checkpoint format that must be upgraded.

Malformed JSON, digest mismatch, invalid IDs, attribution conflicts, impossible
state transitions, and other corruption are not ordinary migration triggers.
They remain hard failures unless a narrowly defined repair migration explicitly
recognizes and validates that historical condition.

## Migration registration

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

`Descriptor.ID` is stable and globally names the semantic migration, for example
`"coderig/session-tools-v1-to-v2"`. `Version` changes only when the migration
implementation's accepted input or produced output changes. A changed version
is a distinct audited program identity and must never silently replace a
previously committed one.

Applications register migrators at composition:

```go
rig.WithMigrations(
	migration.Register(toolHistoryMigration),
	migration.Register(legacyRoutingMigration),
)
```

Registration is immutable for a compiled Rig. Harness validates duplicate IDs,
ambiguous source ranges, invalid chains, and cycles before a Session restore is
attempted.

## Migration input and output

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

## Command migration adapter

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
default to no network and read-only access outside explicitly granted temporary
output. Environment variables are allowlisted; credentials are not forwarded by
default.

## Migration events and atomicity

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
committed migration chain. `MigrationFailed` records a bounded, redacted failure
classification and leaves the prior interpretation baseline unchanged.

Backends continue to use append-only, fenced journal semantics. Migration commit
must occur under the same Session writer lease used by restore.

## Restore order

The target restore sequence is:

1. acquire the Session lease and append the lease fence;
2. inspect frame and record schema versions without constructing live Loops;
3. locate and validate the latest committed migration baseline;
4. select a supported decoder or registered migration chain;
5. plan migration and obtain required approval;
6. apply, validate, and commit `MigrationApplied` when needed;
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

## Automatic versus approved migrations

The policy is intentionally left open for the implementation design. The likely
split is:

- lossless, first-party, registered codec migrations may be allowed
  automatically by application policy;
- semantic, repair-oriented, user-authored, or external-command migrations
  require explicit approval unless a deployment has installed an equally
  explicit trusted policy.

Regardless of approval policy, every applied migration is durably recorded.

## MCP consequence

MCP tool catalogs participate in the configuration manifest, but catalog drift
does not make an old Session unreadable. Historical MCP calls remain journal
data. A changed live MCP catalog is assessed like any other tool change:

- unchanged tools may continue;
- removed tools fail clearly if invoked;
- changed schemas are not silently substituted for the old model-facing
  definition;
- accepting a refreshed catalog adopts a new configuration epoch;
- no journal migration is required merely because an MCP server added, removed,
  or changed tools.

## Open questions for the implementation design

1. Exact public shapes of `ConfigManifest`, drift classification, restore
   decision, and migration contracts.
2. Whether manifests are always inline, always blob-backed, or size-dependent.
3. The canonical manifest encoding and domain version.
4. Which durable record families need independent schema versions beyond their
   existing envelopes.
5. Overlay records versus reconstruction checkpoints for each migration class.
6. How migration program digests are built for in-process Go implementations.
7. Default approval policy for first-party lossless migrations.
8. How the TUI, `serve`, and headless callers present and answer restore
   decisions.
9. Compatibility and deprecation path for `ConfigFingerprint`,
   `ConfigMismatchError`, and `WithAllowConfigMismatch`.
