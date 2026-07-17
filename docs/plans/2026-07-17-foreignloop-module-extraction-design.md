# Foreign-loop module extraction

**Status:** approved design, not implemented. Created 2026-07-17.

This design extracts the existing foreign-loop backend and its Claude and Codex
drivers from `github.com/looprig/harness` into a dedicated optional module,
`github.com/looprig/foreignloop`.

This is a structural ownership change with one deliberate package-boundary
refinement: the generic stream contract stops exposing Claude's transcript path
and instead exposes provider-neutral committed history. It does not introduce ACP,
redesign the loop engine selector, change event or journal formats, or alter
foreign-loop runtime behavior.

The extraction preserves runtime behavior but is not Go source-compatible for
consumers of `harness/pkg/foreignloop`: concrete backend and driver imports move
to the new module. The release sequence below makes that source boundary explicit.

## Goals

- Remove the concrete foreign-loop backend and process drivers from Harness.
- Keep Harness usable without foreign-agent process support.
- Preserve every existing runtime, event, restore, cancellation, locking, and
  transcript invariant.
- Give the extracted repository cohesive Go packages with no Go files or Go
  package at the repository root.
- Apply the same formatting, race, static-analysis, vulnerability, integration,
  and fuzz-testing standards used by Harness.

## Non-goals

- ACP protocol, client, server, or transport support.
- Replacing the closed `loop.Engine` enum with an open driver registry.
- Reworking `WithForeignBuilders` or adding simultaneous foreign backends to one
  rig.
- Rewriting Claude or Codex around their ACP servers.
- Changing durable event names, event bodies, command-journal folding, foreign
  session IDs, transcript formats, or restore behavior.
- Changing workspace placement, permissions, or confinement policy.

Those changes have different owners and failure modes. In particular, ACP is
specified separately in `2026-07-17-acp-bridge-design.md`.

Harness temporarily retaining `EngineForeignClaude` and `EngineForeignCodex` is
accepted compatibility debt for this extraction. Removing those driver-named
constants or adding an ACP engine remains a separate design.

## Current ownership problem

`harness/pkg/foreignloop` currently owns four distinct responsibilities:

1. The narrow builder contract consumed by Harness session construction.
2. Driver-neutral turn and event contracts.
3. The concrete `loop.Backend` implementation and its event mapper.
4. Claude and Codex subprocess drivers and decoders.

Only the first responsibility belongs in Harness. The remaining responsibilities
are optional process-hosting functionality and should live in an optional module.

## Target repositories and packages

Harness retains one protocol-neutral package:

```text
github.com/looprig/harness
└── pkg/foreign/
    ├── builder.go
    └── restored.go
```

The new module is organized as:

```text
github.com/looprig/foreignloop
├── AGENTS.md
├── CLAUDE.md
├── Makefile
├── go.mod
├── go.sum
├── backend/
│   ├── backend implementation
│   ├── event mapping
│   ├── restoration
│   ├── transcript commit
│   └── foreign-session locking
├── driver/
│   ├── agent, turn, stream, and event contracts
│   ├── claude/
│   │   ├── process invocation
│   │   ├── stream and transcript decoding
│   │   └── configuration adapter
│   └── codex/
│       ├── process invocation
│       ├── stream decoding
│       └── configuration adapter
└── vendor/
```

The repository root contains module and development metadata only. It must not
contain `*.go` files, and therefore is not itself an importable Go package.

Cross-module integration belongs to the existing workspace test module:

```text
github.com/looprig/tests
└── foreignloop_integration_test.go
```

`github.com/looprig/tests` is the only module permitted to import both Harness and
the extracted foreignloop module.

### Package dependency direction

```text
driver/claude ─┐
               ├──▶ driver
driver/codex  ─┘        ▲
                        │
backend ────────────────┘
   │
   ├──▶ harness/pkg/foreign
   ├──▶ harness/pkg/loop and harness/pkg/event
   └──▶ looprig/core

driver ──▶ looprig/core/content

harness ──X──▶ github.com/looprig/foreignloop

tests ──▶ harness
  └────▶ foreignloop/backend + foreignloop/driver
```

Harness must never import the extracted module. The extracted module imports the
public Harness seam and satisfies it at a consumer's composition root.

## Harness-owned seam

`harness/pkg/foreign` owns the types consumed by Harness:

- `EventPublisher`
- `Builder`
- `RestoredBuilder`
- `RestoredForeign`

These types are moved from `harness/pkg/foreignloop` without semantic changes.
They may reference public Harness, Core, and event value types, but they must not
reference any extracted backend or driver type.

`rig.WithForeignBuilders` and the internal session lifecycle continue accepting
one live builder and one restored builder. Their behavior and validation remain
unchanged; only their imported contract package changes.

Command-journal decoding and durable folding remain inside Harness. Restore keeps
constructing `foreign.RestoredForeign{ForeignSID, TurnIndex, Msgs}` and passes it
to the injected restored builder. The external module consumes that folded state
but does not decode Harness journals.

## Driver package

`driver` owns the process-agent abstraction. Its public vocabulary is concise and
package-qualified:

- `Agent`
- `Turn`
- `Stream`
- `Event`
- `Kind`
- `PermissionPosture`
- `History`

`Event` is the normalized driver output consumed by `backend`; drivers never mint
Harness `event.Event` values. This preserves the backend as the sole owner of
Harness coordinates, event IDs, tool-execution correlation, turn terminals, and
transcript commit.

Claude- and Codex-specific executable paths, arguments, environment construction,
wire decoding, and transcript interpretation remain in their respective driver
packages. Shared driver contracts must not accumulate provider-specific fields.

### Authoritative history contract

The current `ForeignStream.TranscriptPath` leaks Claude's on-disk transcript model
into the generic backend, and the backend currently calls a Claude-specific JSONL
decoder. That dependency is removed during extraction without changing commit
behavior.

`driver.Stream` exposes provider-neutral authoritative history:

```go
type History struct {
	Available bool
	Steps     []content.AgenticMessages
}

type Stream interface {
	Events() <-chan Event
	History() (History, error)
	Close() error
}
```

`History` is called after `Close`, preserving the current process-drain then
transcript-read ordering.

The current backend computes `int(cur)-1` and passes it to
`decodeTranscriptTail`, but that decoder explicitly ignores the value and reads
the complete transcript. The new public contract therefore does not expose an
unused `sinceTurn` parameter. `History()` reads the complete authoritative history
in the first release. True tail reads require a later behavioral design and a new
contract justified by an actual consumer.

- Claude owns transcript-path derivation and JSONL decoding in `driver/claude`.
  It returns `Available: true` with the decoded authoritative steps. A read or
  decode failure returns a typed driver error.
- Codex has no separate transcript source in this version and returns
  `History{Available: false}` with no error.
- `Available: false` means the driver deliberately has no separate authoritative
  history. It is not an error and produces no warning.
- The backend owns the unchanged policy: use available authoritative steps;
  otherwise, or on a typed history failure, fall back to complete assistant
  messages observed on the live stream and publish the same synthetic `StepDone`
  events as today.

No provider path, transcript wire type, or transcript decoder is exposed through
`driver` or retained in `backend`.

`History.Steps` maps directly to today's decoder result:
`decodeTranscriptTail` returns `[]content.AgenticMessages`, and `commitTurn`
publishes one `StepDone` per element before appending every element to the committed
snapshot. The move changes the owner of decoding, not the decoded value shape,
grouping, ordering, fallback, or published event bodies.

Before moving the decoder, its existing transcript fixtures are used to commit
golden expectations for:

- the exact grouped `[]content.AgenticMessages` value;
- the ordered `StepDone.Messages` bodies produced from those groups;
- the final flattened committed snapshot; and
- malformed-line, sidechain, missing-file, and empty-history behavior.

The new `driver/claude` tests consume the same input bytes and must match those
goldens exactly. During the migration overlap, a parity test runs the old decoder
and the new driver implementation over the same fixtures and compares the complete
Go values and canonical event JSON. The old decoder is removed only after parity
passes. Transcript input bytes are never rewritten by the extraction.

## Backend package

`backend` owns the concrete foreign `loop.Backend` and the behavior currently in
the top-level `foreignloop` package:

- Actor lifecycle and command handling.
- Managed-input queue behavior.
- Turn and step identity.
- Driver-event to Harness-event mapping.
- Foreign-session binding and per-session process locks.
- Transcript-derived authoritative message commit.
- Snapshot and restore construction.
- Interrupt, shutdown, and terminal-event behavior.

Its configuration accepts a `driver.Agent` and backend-owned runtime values such
as the workspace root and session-ID mode. Executable and environment settings
belong to concrete driver configuration, not to the generic backend when the
backend does not consume them.

`SIDMode` is therefore a `backend` type. `PermissionPosture` remains a `driver`
type because it crosses the turn contract to the external agent.

Concrete driver constructors return agents rather than backend configurations:

```text
driver/claude.NewAgent(...) -> driver.Agent
driver/codex.NewAgent(...)  -> driver.Agent
```

The product composition root constructs `backend.Config` from the returned agent,
workspace root, posture, and SID mode. Concrete driver packages never import
`backend`. The existing `claude.NewSpec` and `codex.NewSpec` conveniences are
retired or replaced by agent-only constructors; their validation, environment
whitelisting, credential handling, and typed errors remain in the driver packages.

Fields currently present on the generic `Spec` but unused by the backend, including
the executable path and copied environment, do not move into `backend.Config`.
Removing them is a source-level cleanup only and must not change child-process
configuration.

The consumer-facing backend API is explicit:

```go
type Config struct {
	Agent   driver.Agent
	Cwd     string
	Posture driver.PermissionPosture
	SIDMode SIDMode
}

func BuildWith(cfg Config) foreign.Builder
func BuildRestoredWith(cfg Config) foreign.RestoredBuilder
```

The composition pattern remains, but `BuildWith` and `BuildRestoredWith` now take
`backend.Config` instead of the old `foreignloop.Spec`. This is the exact product
migration boundary. Validation remains eager and typed at agent/config resolution
where possible, while builder invocation revalidates required runtime wiring
fail-closed as it does today.

The current root `export.go` exists only to expose the package-private Claude
stream decoder to the nested Claude package. It does not move. Once decoding lives
inside `driver/claude`, that package calls its own unexported decoder directly and
`DecodeStream` is removed without a public replacement.

### Error ownership

Errors move with the operation that creates them:

- `driver`: spawn, process exit, stream decode, and authoritative-history errors.
- `driver/claude` and `driver/codex`: provider configuration, arguments, wrapping,
  and provider-wire errors.
- `backend`: backend configuration, foreign-session lock/busy, protocol-terminal,
  and snapshot errors.
- `harness/pkg/foreign`: value contracts only; it does not own implementation
  errors.

Equivalent exported typed error classification through `errors.Is`/`errors.As` is
preserved wherever the current error is externally observable. Moving a type to a
new module necessarily changes its Go type identity and import path; consumers
must migrate their imports as part of the source-breaking release.

## Internal dependency removal

The extracted backend may not import `harness/internal/...`. The shared observable
managed-input limit moves from `harness/internal/runtimecontract` to the public
loop contract:

```go
package loop

const ManagedInputQueueCapacity = 64
```

Native loop runtime and the extracted backend both consume this constant. The old
internal definition is removed after every caller migrates. The value and reject-
before-durable-acceptance behavior remain unchanged.

A dependency-guard test must fail if:

- Harness imports `github.com/looprig/foreignloop`.
- The extracted module imports `github.com/looprig/harness/internal/...`.
- A concrete driver imports Harness event or session packages.
- A module other than `github.com/looprig/tests` imports both Harness and the
  extracted foreignloop module as integration subjects.

## Repository quality and security gates

The new repository writes a focused `CLAUDE.md` from the applicable Harness
principles, with `AGENTS.md` pointing to it. This is not a wholesale copy. It keeps
the interface, dependency-inversion, validation, least-privilege, process, path,
context-bound, testing, vendoring, and secure-build rules relevant to foreignloop.
Harness-only ACI, TEE, storage, provider, and unrelated approved-dependency entries
are omitted.

The initial approved dependency list names only what this module actually uses:

- `github.com/looprig/core` for content and UUID values;
- `github.com/looprig/harness` for the public foreign, loop, command, event, and
  identity contracts; and
- `gosec`, `govulncheck`, and `staticcheck` as development tools.

Any later runtime dependency still requires explicit approval in the foreignloop
repository. All child-process, transcript, environment, path, and wire inputs are
treated as untrusted.

The module `go.mod` declares the same toolchain-managed development tools used by
Harness:

- `gosec`
- `govulncheck`
- `staticcheck`

The root `Makefile` provides at least:

```text
build         CGO_ENABLED=0 go build -trimpath ./...
test          go test -race ./...
fmt           format this module's packages
fmt-check     fail on unformatted Go
root-check    fail if the repository root contains a *.go file
vendor        refresh, scrub, and verify the vendored tree
vendor-check  reject embedded VCS metadata
lint          root-check + fmt-check + vendor-check + vet + staticcheck + gosec
vuln          go mod verify + govulncheck
secure        lint + vuln
fuzz          document/run bounded package fuzz targets
```

As in Harness, `GO_DIRS` is derived with `go list` so formatting and gosec inspect
this module's packages without descending into nested worktrees or vendored code.
Build and checks use the vendored dependency tree for reproducibility and review.

`root-check` is a Makefile/filesystem check rather than a root Go test: creating a
root test would itself create the forbidden root package. CI and `make secure`
both execute it.

Existing `#nosec` suppressions around operator-selected executable paths are moved
with their justifications. No broader suppression is introduced.

The initial extracted release supports the same Unix process model as the current
implementation: macOS and Linux process groups and PID liveness checks. Unsupported
platforms fail clearly through build constraints or typed construction errors;
the extraction does not silently claim Windows support. Adding native Windows
process supervision is a later behavioral feature.

## Test migration

Tests move with the behavior they verify:

- Backend actor, mapper, snapshot, restore, locking, and transcript tests move to
  `backend`.
- Driver argument, environment, decoder, and process tests move to the matching
  `driver/claude` or `driver/codex` package.
- Subprocess tests remain `//go:build integration`, use race detection, and have
  bounded contexts.
- Stream and transcript parsers retain fuzz targets with malformed, truncated,
  oversized, and unknown-message inputs.
- Claude transcript tests retain the pre-move golden value/event snapshots and
  prove `driver.Stream.History()` is exactly equivalent to the prior decoder and
  commit projection.
- Harness session-runtime tests that verify builder selection, missing-builder
  failures, journal folding, and restore seed construction remain in Harness and
  use seam fakes rather than importing the external backend.
- The foreignloop module tests backend actor behavior through direct backend tests
  and tests each driver independently. It never imports Harness internals.
- `github.com/looprig/tests` owns every test that needs both the real Harness rig or
  session and the real extracted backend. Its `go.mod` requires Harness and
  foreignloop, using workspace-local `replace` directives during development and
  a separate `go.release.mod` without local replacements for tagged-release
  verification. Its `Makefile` runs both modes.
- The eight scenarios currently in
  `harness/internal/sessionruntime/foreign_e2e_test.go` are re-expressed in
  `tests/foreignloop_integration_test.go` using public APIs. A checked coverage
  table maps every old test name to its new test before the Harness copy is removed.
- Any other Harness test that currently imports the concrete foreignloop package
  is classified explicitly: seam behavior stays with a fake in Harness; real
  backend/session behavior moves to `github.com/looprig/tests`; backend-only
  behavior moves to the foreignloop module.

No test is deleted merely because its implementation moved.

The initial cross-module coverage map is:

| Existing Harness test | Tests-module owner |
|---|---|
| `TestForeignPrimaryE2E` | `TestForeignloopPrimary` |
| `TestCodexForeignPrimaryLateBoundPublishesBoundAndTurnDone` | `TestForeignloopCodexPrimaryLateBound` |
| `TestForeignSubagentE2E` | `TestForeignloopSubagent` |
| `TestForeignQueuedDelegateInterruptResolvesWithoutWaitTimeout` | `TestForeignloopQueuedDelegateInterrupt` |
| `TestForeignQueuedDelegateTimeoutCancelsOnlyThatRequest` | `TestForeignloopQueuedDelegateTimeout` |
| `TestForeignProviderFailureResolvesQueuedDelegatesFailedLive` | `TestForeignloopProviderFailureWithQueuedDelegates` |
| `TestCodexForeignSubagentLateBoundReturnsFinalText` | `TestForeignloopCodexSubagentLateBound` |
| `TestForeignSubagentQuotaCap` | `TestForeignloopSubagentQuota` |

`TestReplaceExternalToolsRefusedOnForeignLoop` remains in Harness because it
verifies Harness engine policy, but its real backend construction is replaced by a
foreign-builder seam fake.

## Migration sequence

1. Add `harness/pkg/foreign`, move the four Harness-consumed contract types, and
   promote `loop.ManagedInputQueueCapacity`; keep the concrete package temporarily.
2. Update Harness rig and session runtime to use the new public seam and constant.
3. Create the external module metadata, package directories, policy, Makefile,
   tooling, vendor configuration, and `root-check`.
4. Move driver-neutral contracts to `driver`, including the authoritative-history
   contract; move `SIDMode` to `backend`.
5. Move Claude and Codex implementations and tests to their driver packages,
   converting their constructors to return `driver.Agent`.
6. Move the concrete backend, mapper, locks, restore logic, errors, and tests to
   `backend`, preserving authoritative-history fallback behavior.
7. Add foreignloop to `github.com/looprig/tests`, port the eight real
   backend/session scenarios through public APIs, and complete the coverage map.
8. Replace remaining concrete imports in Harness tests with seam fakes, then remove
   `harness/pkg/foreignloop` and prove Harness has no external-module dependency.
9. Run Harness and foreignloop builds, race tests, tagged integration tests,
   relevant fuzz targets, and secure checks; run the tests module's complete suite.
10. Tag the Harness release containing the public seam and no concrete backend.
11. Change foreignloop's released `go.mod` from the temporary local replace to that
    Harness tag, verify its vendor tree, and tag the first foreignloop release.
12. Update `github.com/looprig/tests/go.release.mod` to verify the tagged pair in
    addition to the workspace-local `go.mod`, then migrate product consumers to
    explicit `backend.BuildWith`/`BuildRestoredWith` wiring.

No released `go.mod` contains a local `replace` or a dependency on an untagged
workspace commit. Harness is tagged first because foreignloop depends on Harness;
Harness never waits on or imports the foreignloop release.

## Acceptance criteria

- Harness contains only the public foreign builder/restoration seam and no
  concrete foreign backend or process driver.
- `github.com/looprig/foreignloop` contains no root-level Go files.
- The new package graph matches the dependency direction above.
- `driver/claude` owns Claude transcript decoding; backend contains no provider-
  specific transcript path or wire decoder.
- `History()` returns the same grouped messages, `StepDone` bodies, fallback
  behavior, and committed snapshot as the pre-extraction decoder/commit path on
  the shared golden corpus.
- Driver packages do not import backend, Harness event, or Harness session
  packages.
- `BuildWith(backend.Config)` and `BuildRestoredWith(backend.Config)` are the
  documented consumer migration surface; the old `Spec` and `DecodeStream` export
  do not survive accidentally.
- Native and foreign backends use the same public managed-input capacity constant.
- Claude and Codex behavior, event sequences, transcripts, locks, cancellation,
  and restore behavior remain compatible with the pre-extraction implementation.
- Harness builds and passes its tests without the external module present.
- `github.com/looprig/tests` is the sole cross-module integration owner and covers
  every migrated real backend/session scenario through public APIs.
- The extracted module passes `build`, race tests, tagged integration tests,
  relevant fuzz targets, and `make secure`.
- `root-check` rejects every root-level Go source or test file.
- Harness and foreignloop release metadata use tagged dependencies and contain no
  local replacement directives.
- No durable Harness schema or wire representation changes as part of extraction.
