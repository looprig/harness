#!/usr/bin/env bash
#
# Phase 2a — run from the post-Phase-1 looprig root (agents/ swarms/ cmd/ still present;
# Phase 1 already committed + merged to main). Captures the swe trees into a fresh dir,
# prunes looprig to a library, reconciles vendor, VERIFIES, then commits + tags v0.1.0.
#
# Runs under `set -euo pipefail` so a failed precondition/capture/build/verify can NEVER fall
# through to the commit/tag. Does NOT push (outward-facing — do that by hand after review).
#
#   SWE_DIR=~/code/swe bash scripts/migration/03_prune_looprig.sh

set -euo pipefail

SWE_DIR="${SWE_DIR:-$HOME/code/swe}"
EXPECT_MOD='github.com/ciram-co/looprig'
OLD_MOD='github.com/inventivepotter/urvi'

command -v git  >/dev/null || { echo "git required"; exit 1; }
command -v go   >/dev/null || { echo "go required"; exit 1; }
command -v perl >/dev/null || { echo "perl required"; exit 1; }
[ -f go.mod ] || { echo "run from the looprig root"; exit 1; }

echo "==> 0/5  prove Phase 1 completed on THIS tree (fail-closed)"
export GOWORK=off   # never let an ancestor go.work skew verification toward a local/workspace module
br="$(git branch --show-current)"
[ "$br" = "main" ] || { echo "on branch '$br'; merge Phase 1 to main and run from main (the tag must be on main)"; exit 1; }
[ -z "$(git status --porcelain --untracked-files=no)" ] || { echo "tracked changes present; commit first"; exit 1; }
# reject untracked *.go/*.mod: they'd be in the VERIFIED working tree but absent from the scoped commit
untracked="$(git ls-files --others --exclude-standard -- '*.go' '*.mod')"
[ -z "$untracked" ] || { echo "untracked .go/.mod present (verified-but-uncommitted):"; echo "$untracked"; exit 1; }
got_mod="$(go list -m 2>/dev/null || true)"
[ "$got_mod" = "$EXPECT_MOD" ] || { echo "module is '$got_mod', expected '$EXPECT_MOD' — run Phase 1 first"; exit 1; }
for p in pkg/loop pkg/session pkg/llm pkg/tool pkg/content pkg/identity pkg/event; do
  [ -d "$p" ] || { echo "missing $p/ — Phase 1 (internal/->pkg/) not complete"; exit 1; }
done
# fail-closed: rc 0 = found (incomplete), rc 1 = clean, rc>1 = git grep error
rc=0; git grep -qF "$OLD_MOD" -- '*.go' '*.mod' ':(exclude)vendor' || rc=$?
if [ "$rc" -eq 0 ]; then echo "old module path '$OLD_MOD' still in build inputs — Phase 1 incomplete"; exit 1
elif [ "$rc" -ne 1 ]; then echo "git grep failed (rc=$rc)"; exit 1; fi
for d in agents swarms cmd; do [ -d "$d" ] || { echo "missing $d/ — nothing to extract"; exit 1; }; done
[ -f scripts/migration/02_extract_swe.sh ] || { echo "missing scripts/migration/02_extract_swe.sh"; exit 1; }

echo "==> 1/5  resolve a capture dir OUTSIDE the looprig worktree (fresh, no symlink)"
REPO_ROOT="$(cd "$(git rev-parse --show-toplevel)" && pwd -P)"
if [ -e "$SWE_DIR" ] || [ -L "$SWE_DIR" ]; then echo "$SWE_DIR already exists; move it aside"; exit 1; fi
parent="$(dirname "$SWE_DIR")"
[ -d "$parent" ] || { echo "parent dir '$parent' does not exist"; exit 1; }
SWE_CANON="$(cd "$parent" && pwd -P)/$(basename "$SWE_DIR")"
case "$SWE_CANON/" in
  "$REPO_ROOT/"*) echo "$SWE_DIR resolves inside the looprig repo ($REPO_ROOT); pick a path OUTSIDE it"; exit 1;;
esac

echo "==> 2/5  capture swe trees + helper into $SWE_CANON (before any removal)"
PRESPLIT="$(git rev-parse HEAD)"
mkdir -p "$SWE_CANON"
git archive "$PRESPLIT" agents swarms cmd scripts/migration/02_extract_swe.sh | tar -x -C "$SWE_CANON"
for p in agents swarms cmd scripts/migration/02_extract_swe.sh; do
  [ -e "$SWE_CANON/$p" ] || { echo "capture incomplete: missing $SWE_CANON/$p"; exit 1; }
done

echo "==> 3/5  prune looprig to a library (remove consumer trees + cmd/swe Makefile targets)"
git rm -r -q agents swarms cmd
perl -0777 -pi -e '
  s/^\.PHONY: build run /.PHONY: /m;
  s/^build:\n\tCGO_ENABLED=0 go build -trimpath -o bin\/swe \.\/cmd\/swe\n\n//m;
  s/^# Run the TUI directly\..*?\nrun:\n\tset -a;.*?go run \.\/cmd\/swe\n\n//ms;
' Makefile
# fail-closed: assert the build:/run: targets AND any cmd/swe reference are gone (grep -c errors
# yield empty -> the [0] test fails -> exit). Covers reformatted targets, not just literal cmd/swe.
left="$(grep -cE '^(build|run):|cmd/swe' Makefile || true)"
[ "$left" = "0" ] || { echo "Makefile still has build/run/cmd-swe after edit ($left); edit by hand"; exit 1; }

echo "==> 4/5  reconcile vendored deps + VERIFY (errexit gates the commit below)"
go mod tidy
go mod vendor
CGO_ENABLED=0 go build -trimpath ./...
make secure
make test

echo "==> 5/5  stage ONLY the intended paths, commit + tag (NOT pushed)"
# git rm already staged the consumer-tree deletions; add only the generated/edited artifacts so a
# stray untracked file (incl. a mis-placed capture dir) can never enter the looprig tag.
git add -A -- go.mod go.sum vendor Makefile
git commit -m "refactor: extract swe swarm; looprig is now a library"
git tag v0.1.0

echo "DONE. looprig verified, committed, tagged v0.1.0 (not pushed)."
echo "  review, then:  git push looprig main --tags"
echo "  swe trees captured at: $SWE_CANON  → run 04_swe_module.sh there"
