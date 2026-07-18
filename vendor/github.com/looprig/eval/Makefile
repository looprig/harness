.PHONY: test fmt fmt-check lint vuln verify secure fuzz

# Module's own package dirs, excluding vendor/ and any nested .worktrees/
# modules (go list ./... stops at nested module boundaries and skips vendor).
GO_DIRS := $(shell go list -f '{{.Dir}}' ./...)

# eval does not vendor (it depends only on core via a local replace and stdlib,
# with inference added later in judge/ and target/inference/). Verification runs
# GOWORK=off so the module proves it resolves through its own require/replace
# graph.

test:
	go test -race ./...

# Format the whole module in place.
fmt:
	gofmt -w $(GO_DIRS)

# Fail (non-zero exit) if any tracked Go file is not gofmt-clean. Wired into lint.
fmt-check:
	@unformatted=$$(gofmt -l $(GO_DIRS)); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed (run 'make fmt'):"; echo "$$unformatted"; exit 1; \
	fi

lint: fmt-check
	go vet ./...
	go tool staticcheck ./...
	# gosec is NOT module-aware: its ./... is a filesystem walk that descends
	# into nested .worktrees/ checkouts (separate modules) and, under
	# -mod=vendor, reports modules.txt desyncs for those foreign trees. Scope it
	# to THIS module's package dirs via GO_DIRS (the same go-list idiom
	# fmt/fmt-check use). go vet and staticcheck are module-aware (go list stops
	# at module boundaries), so they need no scoping.
	go tool gosec $(GO_DIRS)

vuln:
	go mod verify
	go tool govulncheck ./...

secure: lint vuln

fuzz:
	@echo "Usage: go test -fuzz=FuzzXxx ./path/to/pkg -fuzztime=30s"
