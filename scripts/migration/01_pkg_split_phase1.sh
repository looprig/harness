#!/usr/bin/env bash
#
# Phase 1 (in-place, single module) of the urvi -> looprig pkg split.
#
# Does NOT split repos. After this script the tree is still ONE Go module,
# but: (a) the module path is github.com/ciram-co/looprig, and (b) the engine
# is promoted out of internal/ into a flat pkg/ namespace. The build stays green.
#
# Run from the repo root, on a dedicated branch, with a clean tracked tree:
#   git checkout -b refactor/pkg-split-looprig
#   bash scripts/migration/01_pkg_split_phase1.sh
#
# Bash 3.2 compatible (macOS /bin/bash has no `mapfile`). Idempotency: NOT
# idempotent (git mv fails on a second run). Run once on a clean tree.

set -euo pipefail

OLD='github.com/inventivepotter/urvi'
NEW='github.com/ciram-co/looprig'
export OLD NEW

command -v perl >/dev/null     || { echo "perl required"; exit 1; }
command -v git  >/dev/null     || { echo "git required"; exit 1; }
[ -f go.mod ]                  || { echo "run from repo root"; exit 1; }
# Reject any change to TRACKED files (staged or unstaged). Untracked files are
# ignored on purpose: .worktrees/ is gitignored and git grep (below) only ever
# touches TRACKED files, so untracked noise cannot be rewritten or mis-moved.
[ -z "$(git status --porcelain --untracked-files=no)" ] \
  || { echo "tracked changes present; commit or stash first"; exit 1; }
# Reject untracked *.go/*.mod: `git mv` of a parent dir physically carries untracked
# files to the new path, but the rewrite below (git grep) only touches TRACKED files,
# so an untracked .go would compile with stale imports and break the build.
# --exclude-standard still skips the gitignored .worktrees/.
untracked="$(git ls-files --others --exclude-standard -- '*.go' '*.mod')"
[ -z "$untracked" ] || { echo "untracked .go/.mod present (would move unrewritten):"; echo "$untracked"; exit 1; }

echo "==> 1/5  directory moves (git mv): flatten agent/ wrapper, internal/->pkg/, tools+tui->pkg/"
# Refuse a pre-existing pkg/ tree. The clean-tree gate above ignores UNTRACKED files
# (so .worktrees/ noise doesn't block us), but an untracked pkg/command/ would make
# `git mv internal/agent/loop/command pkg/command` nest into pkg/command/command.
if [ -e pkg ] || [ -L pkg ]; then   # -L also catches a dangling symlink (-e is false for it)
  echo "pkg/ already exists; refusing (would corrupt git mv destinations)"; exit 1
fi
mkdir pkg

# --- loop subtree: move children OUT first so they flatten to pkg/<name>, then the parent ---
git mv internal/agent/loop/command   pkg/command
git mv internal/agent/loop/event     pkg/event
git mv internal/agent/loop/identity  pkg/identity
git mv internal/agent/loop           pkg/loop

# --- session subtree: same, children before parent ---
git mv internal/agent/session/hub      pkg/hub
git mv internal/agent/session/journal  pkg/journal
git mv internal/agent/session          pkg/session
rmdir internal/agent 2>/dev/null || { echo "internal/agent not empty after move:"; ls -la internal/agent; exit 1; }

# --- remaining engine packages (whole-subtree moves) ---
git mv internal/content      pkg/content       # carries streamaccumulator/ + testdata/
git mv internal/llm          pkg/llm           # carries auto/ openaiapi/ tee/ e2e/
git mv internal/tool         pkg/tool
git mv internal/uuid         pkg/uuid
git mv internal/persistence  pkg/persistence
git mv internal/eval         pkg/eval
git mv internal/cli          pkg/cli

# --- promote the already-public runtime for a uniform pkg/ surface ---
git mv tools  pkg/tools
git mv tui    pkg/tui

# Stays in internal/ (framework-private, no external consumer names them):
#   internal/registry internal/hashcache internal/logging internal/ttylog

echo "==> 2/5  collect tracked files referencing the OLD module path (current worktree only)"
# git grep: TRACKED files in THIS worktree only -> never descends into the
# gitignored .worktrees/, and we exclude vendor/. NUL-delimited for safe paths.
FILE_LIST="$(mktemp)"; trap 'rm -f "$FILE_LIST"' EXIT
git grep -lzF "$OLD" -- '*.go' '*.mod' ':(exclude)vendor' > "$FILE_LIST"

echo "==> 3/5  import-path rewrite (ordered: child paths before parents; bounded; module rename last)"
# Each path rule is boundary-guarded with (?=["/`]) so e.g. .../loop never matches
# .../loopback and .../tools never matches .../toolsmith. The final bare rule is
# guarded with (?=["/]|$) so it also catches the go.mod `module <OLD>` line.
xargs -0 perl -pi -e '
  my ($o, $n) = ($ENV{OLD}, $ENV{NEW});
  s{\Q$o\E/internal/agent/loop/command(?=["/`])}{$n/pkg/command}g;
  s{\Q$o\E/internal/agent/loop/event(?=["/`])}{$n/pkg/event}g;
  s{\Q$o\E/internal/agent/loop/identity(?=["/`])}{$n/pkg/identity}g;
  s{\Q$o\E/internal/agent/loop(?=["/`])}{$n/pkg/loop}g;
  s{\Q$o\E/internal/agent/session/hub(?=["/`])}{$n/pkg/hub}g;
  s{\Q$o\E/internal/agent/session/journal(?=["/`])}{$n/pkg/journal}g;
  s{\Q$o\E/internal/agent/session(?=["/`])}{$n/pkg/session}g;
  s{\Q$o\E/internal/content(?=["/`])}{$n/pkg/content}g;
  s{\Q$o\E/internal/llm(?=["/`])}{$n/pkg/llm}g;
  s{\Q$o\E/internal/tool(?=["/`])}{$n/pkg/tool}g;
  s{\Q$o\E/internal/uuid(?=["/`])}{$n/pkg/uuid}g;
  s{\Q$o\E/internal/persistence(?=["/`])}{$n/pkg/persistence}g;
  s{\Q$o\E/internal/eval(?=["/`])}{$n/pkg/eval}g;
  s{\Q$o\E/internal/cli(?=["/`])}{$n/pkg/cli}g;
  s{\Q$o\E/tools(?=["/`])}{$n/pkg/tools}g;
  s{\Q$o\E/tui(?=["/`])}{$n/pkg/tui}g;
  s{\Q$o\E(?=["/`]|$)}{$n}g;
' -- < "$FILE_LIST"

echo "==> 4/5  gofmt the rewritten .go files (CLAUDE.md: gofmt-clean; skip go.mod)"
# Re-list .go files (NUL-safe) and gofmt only those; never pass go.mod to gofmt.
git grep -lzF "$NEW" -- '*.go' ':(exclude)vendor' | xargs -0 gofmt -w --

echo "==> 5/5  verify: build (-trimpath, CGO off) + vet"
CGO_ENABLED=0 go build -trimpath ./...
go vet ./...

echo "DONE. Next: go test -race ./...  &&  make secure  then commit."
