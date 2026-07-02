# looprig-console Extraction Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: execute with superpowers:subagent-driven-development (chosen), one fresh subagent per task, code review between tasks.

**Goal:** Move the TUI + CLI presentation layer out of `looprig` into a new sibling module `github.com/ciram-co/looprig-console`, so `looprig`'s root `go.mod` no longer carries the `charm.land` stack — and rewire the `swe` consumer.

**Architecture:** Clean one-way leaf-cut. `looprig-console → looprig` (never reverse). Packages relocate via `cp`/`sed` (no git-history preservation). New repo mirrors looprig's security posture (vendored, `make secure`, adapted `CLAUDE.md`). Local `replace => ../looprig` avoids version skew (looprig HEAD is past the published `v0.3.0`). The `looprig` cleanup lands on a branch in a git worktree.

**Tech Stack:** Go 1.26.4, go modules + vendoring (`-mod=vendor`), `go.work` workspace, charm.land Bubble Tea v2 stack.

**Design reference:** `docs/plans/2026-07-01-looprig-console-extraction-design.md`

## Key constraints (read before executing)

- **`GOWORK=off` on every module command.** An active `go.work` is incompatible with `-mod=vendor` (Go errors "inconsistent vendoring in /Users/ipotter/code"). All `go build`/`go test`/`go mod tidy`/`go mod vendor`/`make` for the vendored modules run with `GOWORK=off`.
- **Three working locations:**
  - `/Users/ipotter/code/looprig-console` — NEW sibling repo (Task 1). Not a worktree.
  - `<WORKTREE>` — a worktree of `looprig` off `main` (Task 2). Path provided by the orchestrator.
  - `/Users/ipotter/code/swe` — existing consumer repo (Task 3).
- **Task 1 copies from the MAIN looprig checkout** (`/Users/ipotter/code/looprig`), which retains `pkg/tui`/`pkg/cli` throughout (Task 2 deletes them only on the worktree branch).
- **Headline test = option A:** delete `pkg/session/headline_integration_test.go` outright (self-contained; verified its helpers are used nowhere else). Restoring cross-module headline coverage via a shared `sessiontest` package is a tracked **follow-up (Task 4, not executed now).**

---

### Task 1: Scaffold and build `looprig-console`

**Location:** `/Users/ipotter/code/looprig-console` (new repo). Source of truth for copy: `/Users/ipotter/code/looprig`.

**Files:**
- Create dir + `git init`
- Copy: `looprig/pkg/tui` → `tui`, `looprig/pkg/cli` → `cli`
- Create: `go.mod`, `Makefile`, `.gitignore`, `CLAUDE.md`, `AGENTS.md` (symlink), `README.md`, `vendor/`

**Step 1: Fresh repo + copy (idempotent)**

```bash
rm -rf /Users/ipotter/code/looprig-console
mkdir -p /Users/ipotter/code/looprig-console
cd /Users/ipotter/code/looprig-console && git init -q
cp -R /Users/ipotter/code/looprig/pkg/tui /Users/ipotter/code/looprig-console/tui
cp -R /Users/ipotter/code/looprig/pkg/cli /Users/ipotter/code/looprig-console/cli
```

**Step 2: Rewrite import paths with `sed`**

```bash
find /Users/ipotter/code/looprig-console/tui /Users/ipotter/code/looprig-console/cli -name '*.go' -type f -exec sed -i '' \
  -e 's|github.com/ciram-co/looprig/pkg/tui|github.com/ciram-co/looprig-console/tui|g' \
  -e 's|github.com/ciram-co/looprig/pkg/cli|github.com/ciram-co/looprig-console/cli|g' \
  {} +
```
Verify (expect NO output): `grep -rn "ciram-co/looprig/pkg/\(tui\|cli\)" /Users/ipotter/code/looprig-console`

**Step 3: Write `go.mod`** (`/Users/ipotter/code/looprig-console/go.mod`) — minimal; `go mod tidy` fills requires.

```
module github.com/ciram-co/looprig-console

go 1.26.4

tool (
	github.com/securego/gosec/v2/cmd/gosec
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)

require github.com/ciram-co/looprig v0.0.0

// Local, unpublished dependency on the looprig SDK. looprig-console is extracted
// FROM the current looprig working tree, so it must build against that exact tree
// (published tags lag). At release: drop this replace and pin a real looprig tag.
replace github.com/ciram-co/looprig => ../looprig

// The TUI requires the ciram-co bubbletea fork for the Kitty keyboard protocol
// (true Shift+Enter). Copied verbatim from looprig/go.mod — MUST stay in sync.
replace charm.land/bubbletea/v2 => github.com/ciram-co/bubbletea/v2 v2.0.0-20260623210731-9571e88971cd
```

**Step 4: Write `Makefile`** — copy `/Users/ipotter/code/looprig/Makefile` verbatim, then add this note above `export GOFLAGS := -mod=vendor`:

```
# NOTE: -mod=vendor is incompatible with an active go.work workspace. This module
# participates in ../go.work for local editing, so run targets with the workspace
# disabled: `GOWORK=off make secure`.
```

**Step 5: Write `.gitignore`** — copy `/Users/ipotter/code/looprig/.gitignore` verbatim EXCEPT drop the `/scripts/migration/...` line (looprig-specific). Keep the trailing `!/vendor/**` negation.

**Step 6: Write `CLAUDE.md`** — copy `/Users/ipotter/code/looprig/CLAUDE.md`, then edit ONLY the `## Dependencies` approved-list to keep the entries this module owns and drop the rest:
- **KEEP:** `gosec`, `govulncheck`, `staticcheck` (dev tools), `charm.land/bubbletea` (v2), `charm.land/bubbles` (v2), `charm.land/lipgloss` (v2), `charm.land/glamour`, `github.com/atotto/clipboard`.
- **REMOVE:** `go-tdx-guest`, `secp256k1`, `golang.org/x/crypto`, `golang.org/x/net/*`, `nats.go`, `nats-server`, `goldmark` (these belong to looprig-core packages this module does not directly import).
- Add one line at the top of `## Dependencies`: `This module is the presentation layer extracted from github.com/ciram-co/looprig; it depends on that SDK for core types (content, event, transcript, …).`
Then symlink AGENTS.md: `ln -s CLAUDE.md /Users/ipotter/code/looprig-console/AGENTS.md`

**Step 7: Write `README.md`** — short: what the module is (looprig's TUI + CLI presentation layer), the `looprig-console → looprig` relationship, and the `GOWORK=off make secure` build note.

**Step 8: Tidy + vendor (workspace OFF)**

```bash
cd /Users/ipotter/code/looprig-console
GOWORK=off GOFLAGS= go mod tidy
GOWORK=off GOFLAGS= go mod vendor
```
Expected: no errors; `vendor/` populated; `go.mod` gains the four `charm.land/*` requires.

**Step 9: Verify build + tests + secure**

```bash
cd /Users/ipotter/code/looprig-console
GOWORK=off go build ./...                 # expect: clean
GOWORK=off go test -race ./...            # expect: PASS
GOWORK=off make secure                    # expect: lint + vuln clean
```
If any charm-dependent test needs a TTY and fails in headless mode, note it — do NOT delete tests to make them pass; report it for review.

**Step 10: Commit**

```bash
cd /Users/ipotter/code/looprig-console
git add -A
git commit -m "feat: extract looprig TUI + CLI presentation layer into looprig-console"
```
End with: end-of-commit trailer `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

---

### Task 2: Clean `looprig` (in the worktree) + update CLAUDE.md

**Location:** `<WORKTREE>` (a worktree of looprig off `main`, branch `feature/console-extraction`). Provided by orchestrator. All paths below are relative to `<WORKTREE>`.

**Step 1: Delete the presentation packages + the entangled test**

```bash
cd <WORKTREE>
git rm -r pkg/tui pkg/cli
git rm pkg/session/headline_integration_test.go   # option A (self-contained; see plan header)
```

**Step 2: Tidy + re-vendor (workspace OFF)**

```bash
cd <WORKTREE>
GOWORK=off GOFLAGS= go mod tidy
GOWORK=off GOFLAGS= go mod vendor
```

**Step 3: Assert the charm stack is evicted**

```bash
cd <WORKTREE>
GOWORK=off go mod graph | grep -i 'charm.land' && echo "FAIL: charm still present" || echo "OK: no charm.land"
grep -E 'charm.land/(bubbletea|bubbles|lipgloss|glamour)' go.mod && echo "FAIL: charm still in go.mod" || echo "OK: go.mod clean"
```
Expected: both "OK".

**Step 4: Update `<WORKTREE>/CLAUDE.md`** — in `## Dependencies`, REMOVE the four `charm.land/*` entries and the `github.com/atotto/clipboard` entry (they now live in looprig-console). KEEP all others (`gosec`/`govulncheck`/`staticcheck`, `go-tdx-guest`, `x/crypto`, `secp256k1`, `x/net/*`, `nats.go`, `nats-server`, `goldmark`). Add a pointer bullet:
`- The TUI + CLI presentation layer (and its charm.land stack) now lives in the sibling module github.com/ciram-co/looprig-console.`

**Step 5: Verify build + tests + secure**

```bash
cd <WORKTREE>
GOWORK=off go build ./...                 # expect: clean
GOWORK=off go test -race ./...            # expect: PASS (default, non-integration)
GOWORK=off make secure                    # expect: clean
```

**Step 6: Commit**

```bash
cd <WORKTREE>
git add -A
git commit -m "refactor: remove TUI + CLI (extracted to looprig-console); drop charm.land deps"
```
(with the Co-Authored-By trailer.)

---

### Task 3: Wire the workspace + fix `swe`

**Location:** `/Users/ipotter/code/swe` and `/Users/ipotter/code/go.work`.

**Step 1: Add looprig-console to the workspace** — edit `/Users/ipotter/code/go.work` `use (...)` to add `./looprig-console`:
```
use (
	./looprig
	./looprig-console
	./swe
)
```

**Step 2: Rewire swe imports (5 files) via `sed`**

```bash
find /Users/ipotter/code/swe/cmd/swe /Users/ipotter/code/swe/swarms/swe -name '*.go' -type f -exec sed -i '' \
  -e 's|github.com/ciram-co/looprig/pkg/cli|github.com/ciram-co/looprig-console/cli|g' \
  -e 's|github.com/ciram-co/looprig/pkg/tui|github.com/ciram-co/looprig-console/tui|g' \
  {} +
```
Verify (expect NO output): `grep -rn "ciram-co/looprig/pkg/\(tui\|cli\)" /Users/ipotter/code/swe --include='*.go' | grep -v /vendor/`

**Step 3: Add the looprig-console dependency to `swe/go.mod`** — add a `require github.com/ciram-co/looprig-console v0.0.0` and a local replace (swe does NOT vendor; unpublished dep needs the replace for module-mode commands):
```
replace github.com/ciram-co/looprig-console => ../looprig-console
```
(Note in a comment: drop the replace + pin a tag at release.)

**Step 4: Tidy + verify swe (workspace mode; swe is not vendored)**

```bash
cd /Users/ipotter/code/swe
go mod tidy
go build ./...                            # expect: clean
go test -race ./...                       # expect: PASS
```
If swe has its own `make`/lint, run it too. Report any swe-owned test that fails for reasons unrelated to the import swap — do not paper over it.

**Step 5: Commit swe**

```bash
cd /Users/ipotter/code/swe
git add -A
git commit -m "refactor: import TUI + CLI from looprig-console instead of looprig"
```
(with the Co-Authored-By trailer.) The `go.work` change is developer-local (not committed).

---

### Task 4 (FOLLOW-UP — NOT executed now): restore headline coverage

Re-establish the two headline-property tests (`TestHeadlineQuiescentDisplayedStoredRestored`, `TestHeadlineCrashRestoredEqualsDurableProjection`) in `looprig-console` by promoting the shared session fixtures (`buildComplexShapesRun`, `buildCrashedRun`, `restoreCfg`, `restoredSnapshot`, `newEmbeddedJS`, `handOver`, `mustObjectStore`, `mustLeaseManager`, `submitAndDrain`, `stubLLM`, `textChunk`) from `pkg/session/*_test.go` into an exported `pkg/session/sessiontest` support package, then move the tests to `looprig-console` importing `sessiontest` + `tui`. Prereq audit: confirm the promoted helpers touch no unexported `session` internals (else the surface must widen — avoid if possible). Track as a separate issue/plan.

---

## Final verification (orchestrator, after Tasks 1–3)

- `looprig` worktree: `GOWORK=off make secure` + `GOWORK=off go test -race ./...` green; `go mod graph | grep charm.land` empty.
- `looprig-console`: `GOWORK=off make secure` + `GOWORK=off go test -race ./...` green.
- `swe`: `go build ./...` + `go test -race ./...` green (workspace mode).
- Report the three commits (one per repo) and the go.work edit.
