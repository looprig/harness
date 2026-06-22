# looprig / swe — pkg split + repo split + rename (runbook)

- **Date:** 2026-06-22
- **Status:** Runbook — adversarial review (Codex), **9 rounds to convergence (see §7):
  Round 9 = GO / GO, zero BLOCKER/MAJOR/MINOR/NIT for both phases.** **Not yet executed** —
  awaiting explicit go-ahead.
- **Goal:** make this repository an **importable framework** (`looprig`) that a separate
  **`swe`** swarm repo depends on. Rename module `github.com/inventivepotter/urvi` →
  `github.com/ciram-co/looprig` (matches the `looprig` remote at
  `git@github.com:ciram-co/looprig.git`). Promote the engine out of `internal/` into a flat
  `pkg/`. Extract the swe swarm (`agents/`, `swarms/`, `cmd/`) into its own module/repo.
- **Why this is small:** the dependency graph is **already a clean DAG** — no framework code
  (`tools/`, `tui/`, `internal/`) imports the consumer side (`agents/`, `swarms/`, `cmd/`),
  not even in tests. So the split is pure relocation: no DIP inversion, no logic edits.

---

## 0. Two repos (and why not more)

| Repo | Module | Contents |
|---|---|---|
| **looprig** (this repo, after pruning) | `github.com/ciram-co/looprig` | engine in `pkg/*`, reusable `pkg/tools` + `pkg/tui`, framework-private `internal/*`. A **library** (no `main`). |
| **swe** (new) | `github.com/ciram-co/swe` | `agents/{5 roles}`, `swarms/swe`, `cmd/swe`. Depends on `looprig`. |

**Stop at two.** The only natural *future* seams (defer — YAGNI) are `pkg/llm/openaiapi`
(pulls `go-tdx-guest`, `x/crypto`) and `pkg/journal` (pulls embedded `nats-server`) as
optional modules, *if* dependency weight on minimal consumers ever bites.

## 1. The move map (`internal/ → pkg/`, wholesale + flattened)

Promote exactly the packages the consumer side names (traced). Flatten the `agent/` wrapper.

| Today | → | Package decl unchanged? |
|---|---|---|
| `internal/agent/loop` | `pkg/loop` | yes (`package loop`) |
| `internal/agent/loop/command` | `pkg/command` | yes |
| `internal/agent/loop/event` | `pkg/event` | yes |
| `internal/agent/loop/identity` | `pkg/identity` | yes |
| `internal/agent/session` | `pkg/session` | yes |
| `internal/agent/session/hub` | `pkg/hub` | yes |
| `internal/agent/session/journal` | `pkg/journal` | yes |
| `internal/content` (+`streamaccumulator`, `testdata`) | `pkg/content` | yes |
| `internal/llm` (+`auto`,`openaiapi/*`,`tee`,`e2e`) | `pkg/llm` | yes |
| `internal/tool` | `pkg/tool` | yes |
| `internal/uuid` | `pkg/uuid` | yes |
| `internal/persistence` | `pkg/persistence` | yes |
| `internal/eval` | `pkg/eval` | yes |
| `internal/cli` | `pkg/cli` | yes |
| `tools/` | `pkg/tools` | yes |
| `tui/` (+`components`,`styles`) | `pkg/tui` | yes |

**Stays in `internal/`** (framework-private; `looprig`'s own `pkg/*` may still import them):
`internal/registry`, `internal/hashcache`, `internal/logging`, `internal/ttylog`.

Because every package keeps its basename, **no `package` declarations change** — only import
paths and directory locations.

## 2. Phase 1 — in-place rename + promote (ONE module, stays green)

Driver script: [`scripts/migration/01_pkg_split_phase1.sh`](../../scripts/migration/01_pkg_split_phase1.sh).
It does, in order: (1) `git mv` directory moves — children before parents so they flatten;
(2) an **ordered** `perl -pi` import rewrite (most-specific child paths first, the bare module
rename last); (3) `gofmt -w`; (4) `go build ./... && go vet ./...`.

```bash
git checkout -b refactor/pkg-split-looprig
bash scripts/migration/01_pkg_split_phase1.sh
go test -race ./...          # full gate
make secure                  # gofmt-check + vet + staticcheck + gosec + govulncheck
git add -A && git commit -m "refactor: promote engine internal/->pkg/, rename module urvi->looprig"
```

### The rewrite rules (also embedded in the script)

`OLD=github.com/inventivepotter/urvi`, `NEW=github.com/ciram-co/looprig`. The file set is
discovered with **`git grep -lzF "$OLD" -- '*.go' '*.mod' ':(exclude)vendor'`** — tracked files
in **this worktree only**, so it never descends into the gitignored `.worktrees/` (which holds
~500 unrelated Go files that must NOT be rewritten) and excludes `vendor/`. The list is piped
NUL-delimited to `xargs -0 perl` (no `mapfile` — macOS `/bin/bash` is 3.2). Each rule is applied
**in this exact order** (specific → general) and **boundary-guarded** with a ``(?=["/`])``
lookahead (double-quote, slash, or backtick — covering both interpreted and raw-string Go
imports) so `.../loop` can't corrupt `.../loopback` and `.../tools` can't corrupt `.../toolsmith`.
The final bare rule is guarded with ``(?=["/`]|$)`` so it also catches `go.mod`'s `module <OLD>`
line (perl `$` matches before the trailing newline), plus the in-module `agents/`/`swarms/`/`cmd/`
and stay-internal `registry`/`hashcache`/`logging`/`ttylog` refs:

```
OLD/internal/agent/loop/command   -> NEW/pkg/command
OLD/internal/agent/loop/event     -> NEW/pkg/event
OLD/internal/agent/loop/identity  -> NEW/pkg/identity
OLD/internal/agent/loop           -> NEW/pkg/loop
OLD/internal/agent/session/hub    -> NEW/pkg/hub
OLD/internal/agent/session/journal-> NEW/pkg/journal
OLD/internal/agent/session        -> NEW/pkg/session
OLD/internal/content              -> NEW/pkg/content
OLD/internal/llm                  -> NEW/pkg/llm
OLD/internal/tool                 -> NEW/pkg/tool
OLD/internal/uuid                 -> NEW/pkg/uuid
OLD/internal/persistence          -> NEW/pkg/persistence
OLD/internal/eval                 -> NEW/pkg/eval
OLD/internal/cli                  -> NEW/pkg/cli
OLD/tools                         -> NEW/pkg/tools
OLD/tui                           -> NEW/pkg/tui
OLD                               -> NEW            (module line; agents/swarms/cmd; stay-internal)
```

## 3. Phase 1b — `urvi` string literals (NOT module paths — decide explicitly)

The rename above touches only the **module import path**. These runtime identifiers still say
`urvi` and split into two classes:

**Cosmetic — safe to rename now** (no state, no migration):
- `pkg/tools/duckduckgo.go` — User-Agent `urvi-websearch/1.0`.
- `pkg/tools/writefile.go` — temp prefix `.urvi-write-`.
- `pkg/cli/run.go` — log dir `.urvi` / file `urvi.log` (new log location; harmless), and the
  startup log string `logger.Info("urvi starting", …)`.

**User-facing identifier — rename but know it's a contract** (decide explicitly):
- `pkg/cli/run.go` — the **`URVI_LOG_LEVEL`** env var. Renaming to `LOOPRIG_LOG_LEVEL` changes
  the name users/scripts set. Recommended (clean break, pre-release), but it is a surface
  change, not invisible — call it out in the changelog.

**Stateful — a behavioral break; do NOT fold into the mechanical pass:**
- `pkg/tools/permission.go`, `pkg/tools/store.go` — the **`~/.urvi` approval/policy store**
  (`urviDirName = ".urvi"`, hard-deny globs `~/.urvi/**`). Renaming ⇒ existing persisted
  approvals are not found.
- `pkg/journal/*` — NATS subjects/streams (`urvi.session`, `urvi_session_`, `urvi_sessions`).
  Renaming ⇒ existing persisted sessions don't resume.

**Recommendation:** since the repo is pre-release (177 commits ahead of the remote, unpushed),
take a **clean break** — rename all of them to `looprig` and accept that any local `~/.urvi`
store / NATS state is abandoned. But do it as its **own commit, after** Phase 1 is green, so a
behavioral change is never entangled with the mechanical move. Sed for the clean break:

```bash
# run only after Phase 1 verifies green; review the diff before committing.
# git grep -lz keeps it scoped to tracked files in this worktree (no .worktrees/, no vendor/).
git grep -lzF urvi -- '*.go' ':(exclude)vendor' | xargs -0 perl -pi -e '
  s/\.urvi\b/.looprig/g;
  s/urvi\.log/looprig.log/g;
  s/URVI_LOG_LEVEL/LOOPRIG_LOG_LEVEL/g;   # user-facing env var (changelog it)
  s/urvi starting/looprig starting/g;     # startup log string
  s/urvi-websearch/looprig-websearch/g;
  s/\.urvi-write-/.looprig-write-/g;
  s/urvi\.session/looprig.session/g;      # NATS subject root (generation + parsing both shift)
  s/urvi_session/looprig_session/g;       # NATS stream prefix / catalog bucket
  s/"urvi"/"looprig"/g;                   # journal subject token guard
'
git grep -lzF looprig -- '*.go' ':(exclude)vendor' | xargs -0 gofmt -w --
CGO_ENABLED=0 go build -trimpath ./... && go test -race ./...
```

## 4. Phase 2 — physical split into two repos + idiomatic versioning

After Phase 1 (+1b) is committed and green on `refactor/pkg-split-looprig`, merge to `main`,
then split. **Go-idiomatic versioning:** both modules start at **`v0.x`** (unstable API, so
**no `/vN` path suffix**); tag `v0.1.0`. Promote to `v1.0.0` when the API stabilizes; a future
breaking change *after* v1 requires a `/v2` module-path suffix (semantic import versioning).
Co-develop with a **`go.work`** (dev-only, git-ignored); ship with a real `require`.

Phase 2 is split into two **guarded scripts** ([`03_prune_looprig.sh`](../../scripts/migration/03_prune_looprig.sh),
[`04_swe_module.sh`](../../scripts/migration/04_swe_module.sh)) — both run under `set -euo
pipefail` with every `&&` verify-then-publish gate split, so a failed capture / build / auth /
test can **never** fall through to `commit`/`tag` (no unbuildable published tag). Neither script
pushes (outward-facing); they stop at a local tag and print the exact push command.

### 4a. Capture swe FIRST, then prune looprig into a library (`03_prune_looprig.sh`)

Run from the post-Phase-1 looprig root. The script: captures `agents/ swarms/ cmd/` + the helper
via `git archive "$PRESPLIT"` into a **fresh** `$SWE_DIR` (rejects an existing path or dangling
symlink, asserts the capture landed) **before** `git rm`; automates the Makefile edit (removes the
`build:`/`run:` `./cmd/swe` targets + their `.PHONY` entry, asserts no `cmd/swe` remains); then
`go mod tidy && go mod vendor` (the repo is `-mod=vendor`, `Makefile:12`), verifies with
`go build -trimpath ./... && make secure && make test`, and only then stages **only** the intended
paths (`git add -A -- go.mod go.sum vendor Makefile`; the `git rm` deletions are already staged) +
commit + tag. It also fail-closed-gates Phase-1 completion (`go list -m`, `pkg/*`, no old-module
refs, untracked `.go`/`.mod`), requires the branch to be `main`, sets `GOWORK=off`, and rejects a
`$SWE_DIR` resolving inside the repo.

```bash
git checkout main && git merge --ff-only refactor/pkg-split-looprig
SWE_DIR=~/code/swe bash scripts/migration/03_prune_looprig.sh
# review the diff, then push the verified library tag:
git push looprig main --tags             # remote already exists (ciram-co/looprig)
```

### 4b. Build the swe module against the PUBLISHED looprig (`04_swe_module.sh`)

After §4a is pushed, run from the captured `$SWE_DIR`. swe depends on the **published** looprig
tag via a real `require` — **no committed `replace`** (a replace in the tag would pin a
machine-local path / desync `vendor/modules.txt`). The script repoints swe's consumer→consumer
imports (`…/looprig/{agents,swarms,cmd}` → `…/swe/*`; framework `…/looprig/pkg/…` stays), inits
the module, runs a **Git-auth preflight** (`GOPRIVATE` + `url.insteadOf` SSH rewrite + a
`git ls-remote --tags` check for `v0.1.0`), then `tidy`/`vendor`/build/test, and commits + tags.

> **Prerequisites (NOT offline):** `gh auth login`; an SSH key that can read the private
> `ciram-co/looprig`; network or a populated module cache for swe's *public* deps (NATS et al.).

```bash
cd ~/code/swe
LOOPRIG_TAG=v0.1.0 bash scripts/migration/04_swe_module.sh
# then publish (after `gh auth login`):
gh repo create ciram-co/swe --private --source=. --remote=origin --push
git push origin --tags
```

### 4c. Optional: co-iterate on looprig + swe locally (uncommitted)

To edit both repos together without re-publishing looprig per change, overlay a Go workspace
**outside** the tagged history. The looprig checkout is your current clone (`~/code/urvi`, or
rename the dir to `~/code/looprig` to match the module):

```bash
cd ~/code                    # parent of both checkouts — not itself a git repo
go work init ./urvi ./swe    # use ./looprig instead if you renamed the dir
```
`~/code/go.work` sits above both repos so neither tracks it; `go build`/`go test` then use the
local looprig. Note `go mod tidy` **ignores** the workspace (it tidies a single module), so for
anything you tag, publish looprig and rely on the real `require`. A single-module equivalent is an
**uncommitted** `go mod edit -replace=github.com/ciram-co/looprig=/abs/path/to/urvi` — never commit
it (it would pin a machine-local path into the tag).

## 5. Sizing (mechanical, low-complexity)

- 422 `.go` files; 309 reference the module path.
- Module/org rename: 1 prefix replace across 309 files.
- `internal/→pkg/` move: ~247 files change directory; ~650+ import lines rewritten — all
  deterministic prefix substitutions (≈16 ordered rules), **zero logic edits**.
- swe extraction: 45 files (agents 10 + swarms 33 + cmd 2) + a new `go.mod`.
- No back-edges to invert (clean DAG).

## 6. Verification gates

Phase 1: `go build ./...`, `go vet ./...`, `go test -race ./...`, `make secure`.
Phase 1b: same, after the literal rename, reviewing the diff first.
Phase 2: each repo independently `CGO_ENABLED=0 go build -trimpath ./... && go test -race ./...`;
`swe` additionally resolves `looprig@v0.1.0` (or via `go.work` locally).

## 7. Review log (Codex adversarial review, read-only)

**Round 1 → fixed in this revision:**
- BLOCKER: `mapfile` absent in macOS Bash 3.2 → replaced with `git grep -lzF | xargs -0` (no mapfile).
- BLOCKER: `grep -rl .` would rewrite the gitignored `.worktrees/` (~500 unrelated files) →
  scoped to `git grep` (tracked, current worktree only) + `:(exclude)vendor`.
- BLOCKER: Phase 2 never repointed swe's consumer→consumer imports → added
  `02_extract_swe.sh` (`looprig/{agents,swarms,cmd}` → `swe/*`, boundary-guarded, before tidy).
- MINOR: unbounded replacements (`loop`↔`loopback`, `tools`↔`toolsmith`) → `(?=["/])` guards.
- MINOR: clean-tree check missed staged files → `git status --porcelain --untracked-files=no`.
- MINOR: missing `-trimpath` → added to all build commands.
- NIT: `gofmt ''` empty-arg → gofmt re-lists `*.go` only, never `go.mod`.
- NIT: go.work `.gitignore` placement clarified (parent file, not child-tracked).
- NIT: extra branding (`"urvi starting"`, `URVI_LOG_LEVEL`) folded into §3.
- Confirmed OK by Codex: all 35 import paths covered; child-before-parent ordering; `internal/agent`
  empties cleanly; no framework→consumer back-edge; consumers don't touch retained internal pkgs;
  package decls valid; `swarms/swe/skills` embed stays relative.

**Round 2 → fixed in this revision** (Round-1 fixes all re-confirmed correct):
- BLOCKER: `02_extract_swe.sh` ran `git grep` on freshly-`git init`'d (untracked) trees → found
  nothing, masked by `|| true`, so swe imports were never repointed → now discovers via
  `find … -print0` into a `mktemp` list, requires it non-empty, no `|| true`.
- MAJOR: Phase 1 allowed an untracked `pkg/` to nest `git mv` destinations → added a preflight
  refusing a pre-existing `pkg/`.
- MAJOR: `02` used a predictable `/tmp/swe_rewrite_files` (race/symlink) → `mktemp`+`trap`.
- NIT: boundary classes now include a backtick (raw-string imports); gofmt re-list uses `-F`;
  `--` added before perl/gofmt file operands.
- New: `GOPRIVATE=github.com/ciram-co/*` added before swe's `go mod tidy` (looprig is `--private`).
- Confirmed OK by Codex: bare rule still rewrites the `module` line (perl `$` before final
  newline); Phase 2 leaves `looprig/pkg/...` untouched; tag-before-tidy ordering not racy;
  `$ENV{OLD}` regex-escaped via `\Q\E`; xargs NUL-safe and under ARG_MAX.

**Round 3 → fixed in this revision** (core mechanics all re-confirmed correct):
- BLOCKER: §4 removed the swe trees (`git rm`) *before* §4b copied them → reordered to **capture
  first** via `git archive "$PRESPLIT" …` then prune.
- BLOCKER: `GOPRIVATE` comment was wrong (it bypasses proxy/sumdb, not Git auth) and tidy ran
  before auth existed → switched dev to a **local `replace`** (offline, no auth); the published
  path now sets `GOPRIVATE` + `insteadOf` SSH rewrite + a `git ls-remote` auth preflight.
- MAJOR: Phase 1 permitted untracked `.go`/`.mod` that `git mv` would carry but the rewrite
  wouldn't touch → added a `git ls-files --others --exclude-standard -- '*.go' '*.mod'` reject.
- MAJOR: the `Makefile` `build`/`run` targets point at `./cmd/swe` → §4a now gives the
  deterministic Makefile edit (delete both targets + `.PHONY` entry) and re-runs `make secure`.
- Also folded in: `-mod=vendor` reality (`go mod vendor` after the prune; swe vendors via the
  local replace); NIT prose boundary updated to the backtick form; `pkg` preflight now `-L`-aware.
- MINOR (accepted, not changed): ~11 **historical** `docs/plans/*.md` still contain the old
  module string. **Intentionally exempt** — they are point-in-time records, not build inputs
  (Codex confirmed no Makefile/workflow/template/testdata/generator embeds the old path).
- Confirmed OK by Codex: end-to-end `go build`/`go test` import coverage; the `find`-based
  Phase-2 discovery handles untracked copies; eval/persistence/journal all reachable; no
  retained `internal/` import remains in swe.

**Round 4 → Phase 1 reached GO; Phase 2 fixes applied here:**
- Phase 1: **GO** — Codex found no remaining BLOCKER/MAJOR (untracked gate, `$PRESPLIT` capture,
  Makefile edit, post-prune deps all verified correct).
- BLOCKER: §4b invoked `bash 02_extract_swe.sh` but `git archive` extracts it to
  `scripts/migration/02_extract_swe.sh` → fixed the invocation path (+ helper usage comment).
- BLOCKER: the dev `replace=../looprig` was committed into the tagged `v0.1.0` (machine-local
  pin; vendor inconsistency) → **removed the committed replace**; §4b now depends on the
  already-published looprig tag with a Git-auth preflight; the `replace`/`go.work` is demoted to
  an **uncommitted** §4c co-dev convenience.
- BLOCKER: `../looprig` never exists (the checkout is `~/code/urvi`) → §4c uses the real dir.
- MAJOR: "fully offline" was false (public deps like NATS still fetch) → stated the
  network/module-cache prerequisite honestly; the step is explicitly not offline.

**Round 5 → Phase 2 fixes applied here** (Codex found **zero BLOCKERs**; all round-4 Phase-2
fixes verified correct):
- MAJOR: §4a `mkdir -p ~/code/swe` allowed a pre-existing/contaminated checkout → added a
  fresh-destination preflight (`[ ! -e "$SWE_DIR" ] || exit`).
- MINOR: `go mod vendor` then `git commit -am` wouldn't stage newly-created vendor files →
  switched to `git add -A && git commit` so the looprig tag can't omit generated vendor/go.sum.
- Confirmed OK by Codex: capture precedes prune; archive path ↔ helper invocation agree; looprig
  tagged/pushed before swe requires it; auth checked before tidy; the swe tag is replace-free;
  `git init`→commit→`gh repo create --source=. --push` ordering valid.

**Round 6 → Phase 2 hardened into guarded scripts** (Phase 1 GO re-confirmed: all build-input
imports covered):
- BLOCKER: §4a/§4b were loose command blocks → a failed capture/build/verify could fall through
  to `commit`/`tag`/`push` (publishing an unbuildable tag). Replaced the inline blocks with two
  **`set -euo pipefail`** scripts — `03_prune_looprig.sh` and `04_swe_module.sh` — that split
  every `&&` gate, reject existing/dangling-symlink `$SWE_DIR`, assert the capture landed and the
  Makefile no longer references `cmd/swe`, run a `git ls-remote` tag/auth preflight before tidy,
  and stop at a local tag (push stays a reviewed manual step). The Makefile edit is now automated
  + asserted (validated on a copy: `.PHONY: test …`, no `cmd/swe` remains).
- MINOR: §3 clean-break missed `URVI_LOG_LEVEL` and `"urvi starting"` → added; also scoped that
  recipe to `git grep -lzF` for consistency.

**Round 7 → `03` hardened** (Phase 1 GO re-confirmed; all of `03`/`04` otherwise fail-closed):
- BLOCKER: `03` didn't prove Phase 1 ran (could prune+tag a `looprig v0.1.0` still on the old
  module path) → added a step 0 gate: `go list -m == github.com/ciram-co/looprig`, required
  `pkg/*` layout, and `git grep` reject of any remaining `inventivepotter/urvi` build input.
- MAJOR: `$SWE_DIR` could resolve *inside* the looprig worktree and be staged into the tag via
  `git add -A` → canonicalize (`pwd -P`) + reject any path under `REPO_ROOT`, and **scope the
  staging** to `-- go.mod go.sum vendor Makefile` (deletions already staged by `git rm`).
- MINOR/NIT: Makefile assertion now rejects any leftover `^(build|run):` target *and* `cmd/swe`
  via `grep -cE … || true` (errors → empty → fail-closed), not just the literal string.
- Added a symmetric guard to `04`: reject trees that still contain `inventivepotter/urvi`.
- Unit-tested: Makefile assertion 4→0 on edit; containment rejects `testdata/swe`, allows
  `code/swe`, no `urvitools` false-positive.

**Round 8 → verification-integrity holes closed** (Phase 1 GO re-confirmed; R7 fixes verified):
- BLOCKER: `04` (and `03`) could resolve looprig via an ancestor `go.work` (the §4c workspace) →
  added **`export GOWORK=off`** so build/test prove compilation against the *published* tag.
- BLOCKER: `03` ignored untracked `.go`/`.mod` (verified-but-uncommitted) → added the same
  untracked reject Phase 1 uses.
- MAJOR: `03` didn't enforce branch → now requires `git branch --show-current == main` before any
  mutation (so the tag lands on main, matching `git push looprig main --tags`).
- MINOR: `git grep` gate now distinguishes rc 1 (clean) from rc>1 (error → fail-closed).
- MINOR: `04` no longer mutates global git config — process-scoped `GIT_CONFIG_COUNT`/`_KEY_0`/
  `_VALUE_0` insteadOf (verified: works, leaves `~/.gitconfig` untouched).
- NIT: §4a prose updated to the scoped `git add`.

**Round 9 → CONVERGED. GO / GO.** Codex: "no BLOCKER or MAJOR … no MINOR/NIT." It re-verified the
import graph (consumers reference only moved `pkg/*`, never retained `internal/*`), the helper
repoints only consumer paths, Phase-2a captures-before-prune + verifies-before-commit + scopes
staging, Phase-2b auth is process-scoped + rc-gated, `GOWORK=off` precedes all resolution/verify
in both scripts, and `bash -n` passes on all four scripts. No migration step executed.
