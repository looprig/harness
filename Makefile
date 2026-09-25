.PHONY: test fmt fmt-check staticcheck lint vuln verify secure fuzz compat

GO ?= go

# Module's own package dirs (go list ./... stops at nested module boundaries).
# GO_DIRS scopes gosec, which takes package dirs. Never hand GO_DIRS to gofmt:
# gofmt recurses into directory operands, and for a module with a root package
# GO_DIRS contains the module root, so gofmt would walk the entire tree —
# including the nested .worktrees/ checkouts, which are separate modules. Use
# GO_FILES for gofmt: it expands to each package dir's own .go files (including
# platform-specific ones go list omits for the host) without descending.
GO_DIRS = $(shell go list -f '{{.Dir}}' ./...)
GO_FILES = $(foreach dir,$(GO_DIRS),$(wildcard $(dir)/*.go))

# This module does not vendor. go.mod pins exact versions and go.sum verifies
# their content hashes, which is what makes a build reproducible; a vendor tree
# adds only offline builds and source-level dependency diffs. It also actively
# misleads: a stale vendor/ is ignored under a go.work but silently satisfies a
# GOWORK=off build, so standalone verification tests the vendored copy rather
# than the version go.mod actually pins — which is precisely what standalone
# verification exists to check.

test:
	go test -race ./...

# Format the whole module in place.
fmt:
	gofmt -w $(GO_FILES)

# Fail (non-zero exit) if any tracked Go file is not gofmt-clean. Wired into lint.
fmt-check:
	@unformatted=$$(gofmt -l $(GO_FILES)); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed (run 'make fmt'):"; echo "$$unformatted"; exit 1; \
	fi

lint: fmt-check
	go vet ./...
	$(MAKE) staticcheck
	# gosec is NOT module-aware: its ./... is a filesystem walk that descends into
	# the nested .worktrees/ checkouts, which are separate modules. Scope it to THIS
	# module's package dirs via GO_DIRS. go vet and staticcheck are module-aware
	# (go list stops at module boundaries), so they need no scoping.
	go tool gosec $(GO_DIRS)

staticcheck:
	@GO="$(GO)" ./scripts/run-staticcheck.sh

vuln:
	go mod verify
	go tool govulncheck ./...

secure: lint vuln

fuzz:
	@echo "Usage: go test -fuzz=FuzzXxx ./path/to/pkg -fuzztime=30s"

# Run the released v0.40.2 codecs over old and newly attributed records. This
# resolves a released module, so it is a separate release gate rather than check.
compat:
	cd internal/compat/testdata/v0402probe && GOWORK=off go run . decode ../pre_v0410/*.json ../v0410/*.json

# --- standardized check surface -------------------------------------------
# One target, the same set of checks, in every module. CI calls exactly this,
# so a check can no longer pass locally and be silently absent in CI (or the
# reverse). The lint/security tools are pinned by this module's go.mod tool directives.
#
# CHECK_GO_DIRS scopes gosec: gosec is NOT module-aware, so a bare ./... is a
# filesystem walk that descends into nested .worktrees/ checkouts, which are
# separate modules. go vet and staticcheck are module-aware and need no scope.
CHECK_GO_DIRS = $(shell GOWORK=off go list -f '{{.Dir}}' ./...)
# CHECK_GO_FILES is what gofmt gets. Never hand it CHECK_GO_DIRS: gofmt RECURSES
# into directory operands, so for a module with a root package it would walk the
# whole tree, nested .worktrees/ checkouts included.
CHECK_GO_FILES = $(foreach dir,$(CHECK_GO_DIRS),$(wildcard $(dir)/*.go))

vet:
	GOWORK=off go vet ./...

check-staticcheck:
	GOWORK=off go tool staticcheck ./...

check-gosec:
	GOWORK=off go tool gosec -quiet $(CHECK_GO_DIRS)

check-vuln:
	GOWORK=off go mod verify
	GOWORK=off go tool govulncheck ./...

build:
	GOWORK=off go build ./...

check: fmt-check vet check-staticcheck check-gosec check-vuln test build

.PHONY: check check-staticcheck check-gosec check-vuln fmt fmt-check vet test build
