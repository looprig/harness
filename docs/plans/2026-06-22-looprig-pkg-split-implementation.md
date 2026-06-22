# looprig / swe pkg-split + rename — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Turn this repo into an importable framework (`github.com/ciram-co/looprig`) and split the SWE swarm into its own module (`github.com/ciram-co/swe`) that depends on it — with zero logic changes, only relocation + rename.

**Architecture:** Two ordered phases driven by reviewed, fail-closed scripts. **Phase 1** (one module, in place): rename `urvi→looprig`, promote the engine `internal/→pkg/` (flattened), promote `tools/`+`tui/` → `pkg/`. **Phase 2** (split): tag looprig as a library, extract `agents/ swarms/ cmd/` into a new swe module that requires the published looprig tag. The dependency graph is already a clean DAG (no framework→consumer back-edge), so this is pure relocation.

**Tech Stack:** Go 1.26 (`-mod=vendor`), Bash 3.2-safe scripts, `git archive`/`git mv`, NATS-backed persistence, Bubble Tea v2 TUI. Codex-reviewed to GO/GO (9 rounds, see the runbook §7).

**Source of truth:** the runbook `docs/plans/2026-06-22-looprig-pkg-split-runbook.md` and the four scripts under `scripts/migration/`. This plan is the **ordered execution + verification gates**; it does not duplicate command bodies (DRY) — it runs the scripts and checks their output.

**Deviation from defaults (intentional):** run on the **main checkout on a feature branch, not a git worktree** (rename-heavy refactor); land the kit on `main` first.

---

## Task 0: Land the migration kit on `main` (pre-flight)

**Files:**
- Add: `docs/plans/2026-06-22-looprig-pkg-split-runbook.md`, `docs/plans/2026-06-22-looprig-pkg-split-implementation.md`
- Add: `scripts/migration/0{1,2,3,4}_*.sh`, `scripts/migration/02_extract_swe.sh`
- (Exclude: `scripts/migration/codex_review_*.{md,txt}` — review provenance, optional)

**Step 1 — confirm baseline is green and tree is clean of code changes**

Run: `git status --porcelain | grep -vE '^\?\? (docs/plans/2026-06-22|scripts/)' ; CGO_ENABLED=0 go build ./... && echo BUILD_OK`
Expected: no stray modified tracked files; `BUILD_OK`.

**Step 2 — stage and commit only the kit**

```bash
git add docs/plans/2026-06-22-looprig-pkg-split-runbook.md \
        docs/plans/2026-06-22-looprig-pkg-split-implementation.md \
        scripts/migration/01_pkg_split_phase1.sh \
        scripts/migration/02_extract_swe.sh \
        scripts/migration/03_prune_looprig.sh \
        scripts/migration/04_swe_module.sh
git commit -m "docs: looprig/swe pkg-split runbook + migration scripts"
```

**Step 3 — verify clean**

Run: `git status --porcelain -- scripts/migration docs/plans/2026-06-22-looprig-pkg-split-implementation.md`
Expected: empty (kit committed). Untracked `codex_review_*` may remain — leave or `git clean` them per preference.

**CHECKPOINT:** kit is on `main`. Nothing migrated yet.

---

## Task 1: Phase 1 — rename + promote `internal/→pkg/` (on a branch)

**Files:** moves ~247 files (`internal/agent/*`, `internal/{content,llm,tool,uuid,persistence,eval,cli}`, `tools/`, `tui/`) → `pkg/*`; rewrites ~650 import lines; `go.mod` module path. Driven entirely by `scripts/migration/01_pkg_split_phase1.sh`.

**Step 1 — branch off main (main checkout, NOT a worktree)**

Run: `git checkout -b refactor/pkg-split-looprig && git status -sb`
Expected: `## refactor/pkg-split-looprig`, clean tree.

**Step 2 — run the Phase 1 script**

Run: `bash scripts/migration/01_pkg_split_phase1.sh`
Expected: prints steps 1/5…5/5, ends with `CGO_ENABLED=0 go build -trimpath ./...` and `go vet ./...` succeeding, then `DONE.` It aborts (fail-closed) on any untracked `.go`/`.mod`, pre-existing `pkg/`, or build/vet failure.

**Step 3 — full verification gate**

Run: `go test -race ./... && make secure`
Expected: all tests pass under `-race`; `make secure` (gofmt-check + vet + staticcheck + gosec + govulncheck) exits 0.
> If anything fails here, STOP — do not commit. The script's rewrite is deterministic; a failure means an un-handled edge (report it, don't patch blindly).

**Step 4 — spot-check the result**

Run: `head -1 go.mod ; ls -d pkg/* ; git grep -c 'inventivepotter/urvi' -- '*.go' '*.mod' ':(exclude)vendor' | head`
Expected: `module github.com/ciram-co/looprig`; flat `pkg/{loop,command,event,identity,session,hub,journal,content,llm,tool,uuid,persistence,eval,cli,tools,tui}`; **zero** old-path matches.

**Step 5 — commit**

```bash
git add -A
git commit -m "refactor: promote engine internal/->pkg/, rename module urvi->looprig"
```

**CHECKPOINT:** review the diff (`git show --stat`). Build + tests + secure all green on the branch.

---

## Task 2: Phase 1b — `urvi→looprig` runtime strings (DECISION required)

> **Decision gate:** this renames the **stateful** runtime identifiers (`~/.urvi` approval store, `urvi.session.*` NATS subjects, `URVI_LOG_LEVEL`) — a clean break that abandons any existing local approvals/sessions. Recommended (pre-release), but confirm before running. To **keep** the `.urvi` runtime names, skip this whole task.

**Files:** `pkg/cli/run.go`, `pkg/tools/{permission,store,writefile,duckduckgo}.go`, `pkg/journal/*`. Commands: runbook §3.

**Step 1 — run the clean-break rewrite** (commands in runbook §3, scoped `git grep -lzF` + perl).

**Step 2 — verify**

Run: `CGO_ENABLED=0 go build -trimpath ./... && go test -race ./...`
Expected: green (these are string changes; NATS subject generation+parsing shift together, verified by the journal tests).

**Step 3 — commit (separate from the mechanical move)**

```bash
git add -A
git commit -m "refactor: rename runtime identifiers urvi->looprig (clean break)"
```

**CHECKPOINT:** Phase 1 (+1b) complete on the branch.

---

## Task 3: Merge Phase 1 to `main`

**Step 1 — fast-forward merge**

Run: `git checkout main && git merge --ff-only refactor/pkg-split-looprig`
Expected: fast-forward (no merge commit).

**Step 2 — re-verify on main**

Run: `CGO_ENABLED=0 go build -trimpath ./... && go test -race ./... && make secure`
Expected: all green.

**CHECKPOINT:** `main` is now the renamed, promoted single module. Phase 1 done.

---

## Task 4: Phase 2a — prune looprig to a library + tag (`03_prune_looprig.sh`)

**Files:** removes `agents/ swarms/ cmd/`; edits `Makefile` (drops `build`/`run`); regenerates `go.mod`/`go.sum`/`vendor/`. Captures the swe trees to `$SWE_DIR` first. Driven by `scripts/migration/03_prune_looprig.sh` (fail-closed: asserts branch==main, Phase-1-complete, `GOWORK=off`, capture-before-prune, verify-before-tag).

**Step 1 — run it (from looprig root, on main)**

Run: `SWE_DIR=~/code/swe bash scripts/migration/03_prune_looprig.sh`
Expected: prints 0/5…5/5; ends `DONE. looprig verified, committed, tagged v0.1.0 (not pushed)` and reports the capture dir. Aborts on any precondition failure.

**Step 2 — review before publishing**

Run: `git show --stat HEAD ; git tag --list v0.1.0 ; ls ~/code/swe`
Expected: commit removes consumer trees + edits Makefile/vendor; tag `v0.1.0` exists; `~/code/swe` holds `agents swarms cmd scripts`.

**Step 3 — push (human-reviewed; outward-facing)**

Run: `git push looprig main --tags`
Expected: `main` + `v0.1.0` pushed to `git@github.com:ciram-co/looprig.git`.

**CHECKPOINT:** looprig is a published library at `v0.1.0`.

---

## Task 5: Phase 2b — build the swe module (`04_swe_module.sh`)

> **Prereqs (human):** `gh auth login`; an SSH key that can read private `ciram-co/looprig`; network/module-cache for public deps. This step is **not** offline.

**Files:** in `$SWE_DIR`: new `go.mod`/`go.sum`/`vendor/`; rewrites `agents/ swarms/ cmd/` imports `looprig/{agents,swarms,cmd}→swe/*`. Driven by `scripts/migration/04_swe_module.sh` (fail-closed: `GOWORK=off`, process-scoped Git auth, ls-remote tag preflight, verify-before-tag).

**Step 1 — run it**

Run: `cd ~/code/swe && LOOPRIG_TAG=v0.1.0 bash scripts/migration/04_swe_module.sh`
Expected: prints 1/4…4/4; ends `DONE. swe verified, committed, tagged v0.1.0 (not pushed)`. Aborts if the looprig tag isn't reachable or build/test fail.

**Step 2 — create the remote + push (human-reviewed; outward-facing)**

```bash
gh repo create ciram-co/swe --private --source=. --remote=origin --push
git push origin --tags
```

**CHECKPOINT:** two repos exist; swe `v0.1.0` builds against looprig `v0.1.0`.

---

## Task 6: Post-split sanity + optional co-dev

**Step 1 — each repo builds independently**

Run (looprig): `cd ~/code/urvi && CGO_ENABLED=0 go build -trimpath ./... && go test -race ./...`
Run (swe): `cd ~/code/swe && CGO_ENABLED=0 go build -trimpath ./... && go test -race ./...`
Expected: both green.

**Step 2 — optional local workspace (uncommitted)** — runbook §4c: `cd ~/code && go work init ./urvi ./swe`. Never commit `go.work`.

**DONE.** Framework + swarm split, renamed, versioned.

---

## Rollback

- Phase 1 (pre-merge): `git checkout main && git branch -D refactor/pkg-split-looprig` — main untouched.
- Phase 1 (post-merge, pre-push): `git reset --hard <pre-merge-sha>` on local main.
- Phase 2a (pre-push): `git reset --hard HEAD~1 && git tag -d v0.1.0`; `rm -rf ~/code/swe`.
- After any push: revert commits / delete the remote tag; swe repo deletion is manual.
