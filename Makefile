.PHONY: test fmt fmt-check staticcheck lint vuln verify secure fuzz

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
