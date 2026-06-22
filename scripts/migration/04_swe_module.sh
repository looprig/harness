#!/usr/bin/env bash
#
# Phase 2b — run from the fresh swe checkout ($SWE_DIR) that 03_prune_looprig.sh populated.
# Builds the swe module against the PUBLISHED looprig tag (no committed replace), VERIFIES,
# then commits + tags v0.1.0. Runs under `set -euo pipefail` so a failed auth/resolve/verify
# can NEVER fall through to commit/tag. Does NOT create or push the GitHub repo (needs gh auth);
# prints those commands at the end.
#
# Prereqs: looprig v0.1.0 already pushed (Phase 2a); an SSH key that can read ciram-co/looprig;
# network or a populated module cache for swe's PUBLIC deps (this step is NOT offline).
#
#   LOOPRIG_TAG=v0.1.0 bash scripts/migration/04_swe_module.sh

set -euo pipefail

LOOPRIG_TAG="${LOOPRIG_TAG:-v0.1.0}"

command -v git >/dev/null || { echo "git required"; exit 1; }
command -v go  >/dev/null || { echo "go required"; exit 1; }
for d in agents swarms cmd; do [ -d "$d" ] || { echo "run from the extracted swe root (missing $d/)"; exit 1; }; done
[ -f scripts/migration/02_extract_swe.sh ] || { echo "missing extraction helper"; exit 1; }
if [ -e go.mod ]; then echo "go.mod already exists; run on a fresh extraction"; exit 1; fi

echo "==> 1/4  repoint consumer->consumer imports (looprig/{agents,swarms,cmd} -> swe/*)"
# fail-closed: these trees must be POST-Phase-1 (module already renamed urvi->looprig); a stale
# old-module path here means a wrong extraction and would mis-resolve against looprig@tag.
if grep -rqF 'github.com/inventivepotter/urvi' agents swarms cmd 2>/dev/null; then
  echo "old module path 'inventivepotter/urvi' present — re-extract from a post-Phase-1 looprig"; exit 1
fi
bash scripts/migration/02_extract_swe.sh

echo "==> 2/4  init module + require the PUBLISHED looprig tag (no replace in the tag)"
git init -q
go mod init github.com/ciram-co/swe
go mod edit -go=1.26.4
go mod edit -require="github.com/ciram-co/looprig@${LOOPRIG_TAG}"

echo "==> 3/4  Git auth preflight, then resolve + vendor + VERIFY (against the PUBLISHED tag)"
export GOWORK=off                          # never resolve looprig via an ancestor go.work — prove the tag builds
export GOPRIVATE='github.com/ciram-co/*'   # GOPRIVATE only skips proxy/sumdb — not Git auth
# process-scoped insteadOf (does NOT mutate ~/.gitconfig): force SSH for go's HTTPS module fetch
export GIT_CONFIG_COUNT=1
export GIT_CONFIG_KEY_0='url.git@github.com:.insteadOf'
export GIT_CONFIG_VALUE_0='https://github.com/'
rc=0; git ls-remote --tags git@github.com:ciram-co/looprig "refs/tags/${LOOPRIG_TAG}" 2>/dev/null | grep -q "${LOOPRIG_TAG}" || rc=$?
[ "$rc" -eq 0 ] || { echo "looprig ${LOOPRIG_TAG} not reachable — push Phase 2a first and check SSH auth (rc=$rc)"; exit 1; }
go mod tidy
go mod vendor                              # optional; keeps swe on the auditable -mod=vendor convention
CGO_ENABLED=0 go build -trimpath ./...
go test -race ./...

echo "==> 4/4  commit + tag (NOT pushed)"
git add -A
git commit -m "feat: swe swarm on looprig ${LOOPRIG_TAG}"
git tag v0.1.0

echo "DONE. swe verified, committed, tagged v0.1.0 (not pushed)."
echo "To publish (after 'gh auth login'):"
echo "  gh repo create ciram-co/swe --private --source=. --remote=origin --push"
echo "  git push origin --tags"
