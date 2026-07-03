# Design: Extract the presentation layer into `looprig-console`

**Date:** 2026-07-01
**Status:** Approved (design) — ready for implementation planning
**Author:** brainstorming session

## 1. Problem & Goal

`looprig` is a reusable SDK (agents and `cmd/swe` live in the separate `ciram-co/swe`
consumer repo). Today `looprig`'s root `go.mod` is polluted by the entire terminal-UI
dependency stack — `charm.land/{bubbletea,bubbles,lipgloss,glamour}` plus ~15
transitives (chroma, ultraviolet, bluemonday, colorprofile, x/term, …) — even though
that stack is only reachable through the TUI/CLI packages.

**Goal:** move the TUI + CLI presentation layer into a new module so `looprig`'s root
`go.mod` no longer carries the `charm.land` stack.

**Explicitly deferred (acceptable for now, per the request):**

- **`nats`** stays in `looprig` — it is pulled by `journal`/`persistence`/`session`,
  none of which the TUI/CLI touch. Moving the UI does not affect it.
- **`goldmark`** stays in `looprig` — it is pulled by `pkg/transcript/html`, which the
  TUI *imports* (transcript rendering). This is the "export deps ok for now."

## 2. Decisions (locked)

- **Module path / name:** `github.com/looprig/cli`.
  Chosen over `looprig-tui` because the module also holds the CLI runner, and over
  thematic names (`loom`, `shuttle`) for discoverability as an obvious `looprig` sibling.
- **Repo strategy:** a **new, separate repository** (not a nested submodule), matching
  the existing `looprig` (SDK) / `swe` (consumer) shape.
- **Migration mechanism:** plain file relocation with **`mv` / `cp` / `sed`** — copy the
  packages, rewrite import paths with `sed`. **Git history is NOT preserved** (this
  supersedes any `git filter-repo`/`subtree` approach).
- **Security / build parity:** the new repo mirrors looprig's posture exactly — vendored
  (`-mod=vendor`, `vendor/` committed), a `Makefile` with the same `make secure`
  pipeline, matching `.gitignore`, and its own adapted `CLAUDE.md`.

## 3. Dependency analysis (why this is a clean leaf-cut)

The presentation packages are consumed by **nothing** in `looprig` core. The dependency
graph flows strictly one way:

```
pkg/cli ──▶ pkg/tui ──▶ { content, event, hub, identity, tool,
                          transcript, transcript/html,
                          transcript/journalsource, uuid }
pkg/tui/components, pkg/tui/styles  (internal to the tui subtree)
```

- `pkg/tui` does **not** import `pkg/cli`.
- Nothing in `looprig` core imports `pkg/tui` or `pkg/cli` — **except one test file**
  (see §5), which is the only real complication.
- `pkg/tui`/`pkg/cli` do **not** import any nats-touching package
  (`journal`/`persistence`/`session`). Confirms moving the UI does not evict nats.

After extraction the module relationship is one-directional:

```
looprig-console ──▶ looprig        (never the reverse)
swe            ──▶ looprig, looprig-console
```

## 4. What moves

| From (`looprig`)        | To (`looprig-console`) | New import path                                  |
|-------------------------|------------------------|--------------------------------------------------|
| `pkg/tui`               | `tui`                  | `github.com/looprig/cli/tui`        |
| `pkg/tui/components`    | `tui/components`       | `github.com/looprig/cli/tui/components` |
| `pkg/tui/styles`        | `tui/styles`           | `github.com/looprig/cli/tui/styles` |
| `pkg/cli`               | `cli`                  | `github.com/looprig/cli/cli`        |

`sed` rewrites, applied only under the copied tree:

```
s|github.com/looprig/harness/pkg/tui|github.com/looprig/cli/tui|g
s|github.com/looprig/harness/pkg/cli|github.com/looprig/cli/cli|g
```

(The `tui` rule also fixes `tui/components` and `tui/styles`.) All other
`ciram-co/looprig/pkg/...` imports are left intact — they stay in looprig.

The new module keeps depending on `looprig` for: `content`, `event`, `hub`, `identity`,
`tool`, `transcript`, `transcript/html`, `transcript/journalsource`, `uuid`. That set is
the stable contract between the two modules.

## 5. The one cross-boundary test — **larger than a `cp` (needs a decision)**

`pkg/session/headline_integration_test.go` (`//go:build integration`, declared
`package session`) is the **only** `looprig`-core file that imports `pkg/tui`. It holds
the two headline-property tests (Task 11.1 / 11.2 of the event-persistence design):
`TestHeadlineQuiescentDisplayedStoredRestored` and
`TestHeadlineCrashRestoredEqualsDurableProjection`.

Investigation shows this **cannot be a simple move**:

- **It must leave looprig.** `go mod tidy` considers *all* build tags, so even a
  `//go:build integration` file importing `pkg/tui` would drag `looprig-console` + the
  charm stack back into looprig's `go.mod`. Keeping it in looprig defeats the goal.
- **The `tui` symbol it needs can't be pushed down to core.** It uses only
  `tui.FoldDisplay(...)` and three methods on the result (`EqualTranscript`,
  `CommittedLen`, `PendingPrompts`). But `FoldDisplay`/`DisplayProjection` live in
  `pkg/tui/restore.go`, which imports `charm.land/bubbletea/v2` — relocating them into
  looprig-core would drag charm *into* core, the opposite of the goal.
- **Its fixtures are shared, session-internal test infra.** It depends on
  `buildComplexShapesRun`, `buildCrashedRun`, `restoreCfg`, `restoredSnapshot`,
  `newEmbeddedJS`, `handOver`, `mustObjectStore`, `mustLeaseManager`, `submitAndDrain`
  (all in `pkg/session/restore_integration_test.go`, and reused by looprig's own restore
  tests) plus `stubLLM`/`textChunk` (`session_test.go`). `FingerprintFrom` is already
  exported. These cannot simply travel with the test.

### Options (to be chosen before the looprig cleanup step)

- **(A) Drop the two headline tests from looprig (fast path).** They are
  integration-tagged (excluded from default `go test ./...`). Removing them unblocks the
  cleanup immediately; re-add cross-module coverage later. **Cost:** temporarily loses a
  high-value headline property test. **YAGNI-friendly, reversible.**
- **(B) Extract a shared `sessiontest` fixture package (correct path).** Promote the
  needed builders from `pkg/session/*_test.go` into an exported, importable support
  package (e.g. `pkg/session/sessiontest`), then move the two headline tests to
  `looprig-console` importing `sessiontest` + `tui`. **Cost:** a real refactor of
  `restore_integration_test.go` + `session_test.go`; must confirm the promoted helpers
  touch no unexported `session` internals (else widen `session`'s API — avoid if
  possible). **SOLID, keeps coverage, preserves the module boundary.**

**Recommendation:** (A) now to complete the split cleanly, tracked as a follow-up to do
(B) — unless keeping the headline coverage green through the split is a hard requirement,
in which case do (B) as part of this work.

Either way, `looprig` core ends with **zero** references to `tui`/`cli`.

## 6. New repo scaffolding & security parity

The new repo is seeded to match looprig:

- **`go.mod`** — `module github.com/looprig/cli`, `go 1.26.4`, the same
  `tool (...)` block (gosec / govulncheck / staticcheck) so `make secure` works, and two
  `replace` directives:
  - `replace github.com/looprig/harness => ../looprig` — **local, unpublished**
    dependency. looprig-console is extracted from the current looprig *working tree*, so
    it must build against that exact tree (published tags lag: HEAD is past `v0.3.3`,
    while `swe` still pins `v0.3.0`). **At release:** drop this replace and pin a real
    looprig tag.
  - `replace charm.land/bubbletea/v2 => github.com/looprig/bubbletea/v2 <pseudo>` —
    copied verbatim from looprig; the TUI regresses (Shift+Enter / Kitty protocol)
    without it. Must stay in sync with looprig's pin.
  - `require`s for the charm stack + transitives are produced by `go mod tidy`.
- **`Makefile`** — mirror of looprig's (`test`/`fmt`/`fmt-check`/`lint`/`vuln`/`secure`/
  `fuzz`, `export GOFLAGS := -mod=vendor`, `GO_DIRS` scoping for gosec). Add a note that
  `-mod=vendor` is incompatible with the active workspace, so targets run as
  `GOWORK=off make secure` (see §7).
- **`.gitignore`** — mirror of looprig's, keeping the trailing `!/vendor/**` negation so
  the committed vendor tree survives the global ignores.
- **`CLAUDE.md`** — adapted from looprig's: keep the SOLID, Security, Secure Coding
  Patterns, and Build/Testing sections verbatim; the **Dependencies** approved-list
  carries over only the entries this module owns — `charm.land/bubbletea` (v2),
  `charm.land/bubbles` (v2), `charm.land/lipgloss` (v2), `charm.land/glamour`,
  `github.com/atotto/clipboard` — plus the dev-tool entries (gosec / govulncheck /
  staticcheck) it also runs.
- **`vendor/`** — populated with `GOWORK=off go mod vendor` and committed, mirroring
  looprig's offline/auditable build.

## 7. Local development workflow (`go.work` already exists)

A workspace already lives at `/Users/ipotter/code/go.work`:

```
go 1.26.4
use ( ./looprig  ./swe )
replace charm.land/bubbletea/v2 => github.com/looprig/bubbletea/v2 <pseudo>
```

- Add `./looprig-console` to its `use (...)` block.
- **Constraint discovered:** an active `go.work` is **incompatible with `-mod=vendor`**
  (Go looks for a *workspace*-level vendor dir and errors "inconsistent vendoring in
  /Users/ipotter/code"). looprig already lives with this — so vendored `make` targets and
  `go mod vendor`/`go mod tidy` must run with **`GOWORK=off`**. The workspace is for
  IDE / cross-module editing; audited builds run per-module with the workspace off.
- Release order: tag `looprig` → swap looprig-console's `replace` for that tagged
  `require` → tag `looprig-console` → bump `swe`.

## 8. Consumer (`swe`) rewiring

`swe` (`github.com/looprig/swe`, currently `require github.com/looprig/harness
v0.3.0`) imports the presentation layer in **6 spots**:

- `cmd/swe/main.go` — `looprig/pkg/cli` **and** `looprig/pkg/tui`
- `swarms/swe/swarm.go`, `swarm_test.go`, `agent_test.go`, `acceptance_test.go` —
  `looprig/pkg/tui`

Rewiring (via `sed`):
```
s|github.com/looprig/harness/pkg/cli|github.com/looprig/cli/cli|g
s|github.com/looprig/harness/pkg/tui|github.com/looprig/cli/tui|g
```
Then add `require github.com/looprig/cli` to `swe/go.mod` (resolved locally
via the workspace `use`; `swe` does not vendor). The public API is unchanged:
`cli.Run(ctx, newAgent func(context.Context) (tui.Agent, error), banner cli.Banner) int`
plus the `tui.Agent` interface and `cli.Banner` struct.

## 9. looprig cleanup + CLAUDE.md update

- Delete `pkg/tui`, `pkg/tui/components`, `pkg/tui/styles`, `pkg/cli` (via `rm -rf` /
  `git rm`); resolve the headline test per §5.
- `GOWORK=off go mod tidy` and re-`vendor`; **assert** `go mod graph | grep charm.land`
  returns nothing and the four `charm.land/*` requires are gone from `go.mod`.
- **`looprig/CLAUDE.md`:** remove the `charm.land/*` + `atotto/clipboard` entries from
  the Dependencies approved-list (they now live in looprig-console's CLAUDE.md), and add
  a short pointer noting the TUI/CLI presentation layer — and its charm stack — now lives
  in `github.com/looprig/cli`. Keep `goldmark` (transcript/html stays),
  `nats`, `go-tdx-guest`, `x/crypto`, `secp256k1`, `x/net` entries as-is.

## 10. Migration sequence (keeps every module building at each checkpoint)

1. **Create `looprig-console`** (`git init`, `go.mod` + `replace`s + `tool` block, copy
   `tui`/`cli` via `cp -R`, `sed` import rewrite, add `Makefile`/`.gitignore`/`CLAUDE.md`).
2. `GOWORK=off go mod tidy` → `go mod vendor`; **verify** `GOWORK=off make secure` and
   `GOWORK=off go test -race ./...` pass. *(looprig still has its copies — nothing broken.)*
3. Add `./looprig-console` to `go.work`.
4. **Rewire `swe`** imports + `go.mod`; verify `swe` builds/tests (workspace mode).
5. **Clean `looprig`:** delete the four packages, resolve the headline test (§5),
   `GOWORK=off go mod tidy` + re-vendor; verify build/test and the charm eviction.
6. Update `looprig/CLAUDE.md` (§9).

## 11. Verification

- **`looprig`:** `GOWORK=off make secure`, `GOWORK=off go test -race ./...`, and assert
  `go mod graph | grep charm.land` is empty (bubbletea/bubbles/lipgloss/glamour absent
  from `go.mod`).
- **`looprig-console`:** `GOWORK=off make secure`, `GOWORK=off go test -race ./...`; if
  §5 option (B), run `-tags integration` so the moved headline tests pass.
- **`swe`:** builds and runs the TUI/CLI against both modules via the workspace.

## 12. Risks & mitigations

- **Headline-test entanglement (§5)** — the main complication; resolved by the (A)/(B)
  decision before cleanup.
- **`go.work` + `-mod=vendor` conflict (§7)** — mitigated by running audited/module
  commands with `GOWORK=off`.
- **Version skew (§6)** — mitigated by the local `replace => ../looprig`; becomes a real
  tagged `require` at release.
- **`bubbletea` replace drift** — the fork pin must stay identical across looprig,
  looprig-console, and the workspace `go.work`.

## 13. Out of scope (future, per the request)

- Extracting `nats` out of `looprig` core.
- Extracting `goldmark`/`transcript/html` out of `looprig` core.
  (When tackled: `transcript/html` could move into `looprig-console` too, since the TUI
  is its only in-tree consumer — revisit then.)
