# Foreign-loop module extraction

**Status:** approved design, not implemented. Created 2026-07-17.

This design extracts the existing foreign-loop backend and its Claude and Codex
drivers from `github.com/looprig/harness` into a dedicated optional module,
`github.com/looprig/foreignloop`.

This is deliberately a mechanical ownership change. It does not introduce ACP,
redesign the loop engine selector, change event or journal formats, or alter
foreign-loop behavior.

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

harness ──X──▶ github.com/looprig/foreignloop
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
- `SIDMode`

`Event` is the normalized driver output consumed by `backend`; drivers never mint
Harness `event.Event` values. This preserves the backend as the sole owner of
Harness coordinates, event IDs, tool-execution correlation, turn terminals, and
transcript commit.

Claude- and Codex-specific executable paths, arguments, environment construction,
wire decoding, and transcript interpretation remain in their respective driver
packages. Shared driver contracts must not accumulate provider-specific fields.

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

The existing `BuildWith` and `BuildRestoredWith` composition pattern remains,
returning `foreign.Builder` and `foreign.RestoredBuilder` respectively.

## Internal dependency removal

The extracted backend may not import `harness/internal/...`. The current managed
input queue capacity must therefore become a small public Harness runtime contract
or be supplied through the public foreign seam. The extraction must preserve its
existing value and behavior; it must not silently duplicate a magic number in the
new module.

A dependency-guard test must fail if:

- Harness imports `github.com/looprig/foreignloop`.
- The extracted module imports `github.com/looprig/harness/internal/...`.
- A concrete driver imports Harness event or session packages.
- The repository root gains a Go source file.

## Repository quality and security gates

The new repository copies the applicable Harness development policy into its own
`CLAUDE.md`, with `AGENTS.md` pointing to it. In particular, external dependencies
still require explicit approval and all external process and wire inputs are
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
vendor        refresh, scrub, and verify the vendored tree
vendor-check  reject embedded VCS metadata
lint          fmt-check + vendor-check + vet + staticcheck + gosec
vuln          go mod verify + govulncheck
secure        lint + vuln
fuzz          document/run bounded package fuzz targets
```

As in Harness, `GO_DIRS` is derived with `go list` so formatting and gosec inspect
this module's packages without descending into nested worktrees or vendored code.
Build and checks use the vendored dependency tree for reproducibility and review.

Existing `#nosec` suppressions around operator-selected executable paths are moved
with their justifications. No broader suppression is introduced.

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
- Harness session-runtime tests that verify its use of the builder seam remain in
  Harness and use seam fakes rather than importing the external backend.
- A small cross-module integration suite in the extracted module proves that its
  live and restored builders satisfy the current Harness seam.

No test is deleted merely because its implementation moved.

## Migration sequence

1. Add `harness/pkg/foreign` and move the four Harness-consumed contract types.
2. Update Harness rig and session runtime to use the new public seam; keep the
   existing concrete package temporarily until dependency guards pass.
3. Create the external module metadata, package directories, policy, Makefile,
   tooling, vendor configuration, and root-package guard.
4. Move driver-neutral contracts to `driver`.
5. Move Claude and Codex implementations and tests to their driver packages.
6. Move the concrete backend, mapper, locks, restore logic, and tests to `backend`.
7. Remove the extracted implementation from Harness and prove Harness has no
   dependency on the external module.
8. Run both modules' race, integration, fuzz, build, and secure checks.
9. Release the external module and update consumers to wire
   `backend.BuildWith`/`BuildRestoredWith` explicitly.

The migration may use a temporary local `replace` while both repositories are
developed together. Released module metadata must use tagged module versions.

## Acceptance criteria

- Harness contains only the public foreign builder/restoration seam and no
  concrete foreign backend or process driver.
- `github.com/looprig/foreignloop` contains no root-level Go files.
- The new package graph matches the dependency direction above.
- Claude and Codex behavior, event sequences, transcripts, locks, cancellation,
  and restore behavior remain compatible with the pre-extraction implementation.
- Harness builds and passes its tests without the external module present.
- The extracted module passes `build`, race tests, tagged integration tests,
  relevant fuzz targets, and `make secure`.
- No durable Harness schema or wire representation changes as part of extraction.
