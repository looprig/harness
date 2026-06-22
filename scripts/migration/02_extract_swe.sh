#!/usr/bin/env bash
#
# Phase 2 helper — run INSIDE the new swe checkout, AFTER copying agents/ swarms/
# cmd/ out of the post-Phase-1 looprig tree and BEFORE `go mod tidy`.
#
# After Phase 1, swe's own files import its sibling packages under the looprig
# module path (e.g. github.com/ciram-co/looprig/swarms/swe, .../agents/explorer)
# because Phase 1 lived in one module. Once swe is its own module those must point
# at github.com/ciram-co/swe/*, while genuine framework imports
# (github.com/ciram-co/looprig/pkg/...) stay on looprig. This rewrites ONLY the
# consumer-owned trees: agents, swarms, cmd.
#
# Usage (this file is archived to scripts/migration/ alongside agents/ swarms/ cmd/):
#   git -C <looprig> archive <presplit> agents swarms cmd scripts/migration/02_extract_swe.sh \
#     | tar -x -C ~/code/swe
#   cd ~/code/swe && git init
#   bash scripts/migration/02_extract_swe.sh        # MUST run before `go mod tidy`
#   go mod init github.com/ciram-co/swe && go mod edit -go=1.26.4 \
#     && go mod edit -require=github.com/ciram-co/looprig@v0.1.0 && go mod tidy
#   CGO_ENABLED=0 go build -trimpath ./... && go test -race ./...

set -euo pipefail

LOOP='github.com/ciram-co/looprig'
SWE='github.com/ciram-co/swe'
export LOOP SWE

command -v perl >/dev/null  || { echo "perl required"; exit 1; }
command -v find >/dev/null  || { echo "find required"; exit 1; }
for d in agents swarms cmd; do
  [ -d "$d" ] || { echo "missing consumer tree: $d/ (copy it from looprig first)"; exit 1; }
done

# The trees are UNTRACKED here (fresh `git init`, nothing staged), so `git grep`
# would silently match nothing. Discover with `find` over the consumer trees and
# require a non-empty result so a no-op can never pass unnoticed.
FILE_LIST="$(mktemp)"; trap 'rm -f "$FILE_LIST"' EXIT
find agents swarms cmd -type f -name '*.go' -print0 > "$FILE_LIST"
[ -s "$FILE_LIST" ] || { echo "no .go files under agents/ swarms/ cmd/"; exit 1; }

# Boundary-guarded: only looprig/{agents,swarms,cmd}<boundary> -> swe/...; the
# alternation captures the consumer segment, (?=["/`]) prevents matching e.g.
# .../agentstore. Framework imports (looprig/pkg/...) are untouched. `--` guards
# against a future leading-dash filename.
xargs -0 perl -pi -e '
  my ($l, $s) = ($ENV{LOOP}, $ENV{SWE});
  s{\Q$l\E/(agents|swarms|cmd)(?=["/`])}{$s/$1}g;
' -- < "$FILE_LIST"

xargs -0 gofmt -w -- < "$FILE_LIST"
echo "DONE. Now: go mod init/edit/tidy, then CGO_ENABLED=0 go build -trimpath ./... && go test -race ./..."
