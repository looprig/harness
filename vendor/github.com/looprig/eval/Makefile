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
	# staticcheck: analyze THIS module's concrete package dirs via GO_DIRS (the
	# same go-list idiom gosec/fmt use) rather than a bare `./...`. A bare pattern
	# that resolves to no packages makes staticcheck print a "matched no packages"
	# warning and STILL EXIT 0 — a vacuous run that reads as success while nothing
	# was analyzed. GO_DIRS is the explicit package set, and the guard below FAILS
	# the target if go-list resolved nothing or staticcheck ever matches none, so a
	# vacuous "clean" can never masquerade as a pass again.
	@if [ -z "$(GO_DIRS)" ]; then \
		echo "staticcheck: go list resolved no packages (GO_DIRS empty)"; exit 1; \
	fi
	@echo "go tool staticcheck $(words $(GO_DIRS)) packages"
	@out=$$(go tool staticcheck $(GO_DIRS) 2>&1); rc=$$?; \
	if [ -n "$$out" ]; then printf '%s\n' "$$out"; fi; \
	if printf '%s\n' "$$out" | grep -q "matched no packages"; then \
		echo "staticcheck matched no packages (vacuous success); failing"; exit 1; \
	fi; \
	if [ $$rc -ne 0 ]; then exit $$rc; fi
	# gosec is NOT module-aware: its ./... is a filesystem walk that descends
	# into nested .worktrees/ checkouts (separate modules) and, under
	# -mod=vendor, reports modules.txt desyncs for those foreign trees. Scope it
	# to THIS module's package dirs via GO_DIRS (the same go-list idiom
	# fmt/fmt-check use). go vet is module-aware (go list stops at module
	# boundaries), so it needs no scoping.
	go tool gosec $(GO_DIRS)

vuln:
	go mod verify
	go tool govulncheck ./...

secure: lint vuln

fuzz:
	@echo "Usage: go test -fuzz=FuzzXxx ./path/to/pkg -fuzztime=30s"
