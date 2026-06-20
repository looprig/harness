.PHONY: build run test fmt fmt-check lint vuln verify secure fuzz

# Module's own package dirs, excluding vendor/ and the nested .worktrees/ modules
# (go list ./... stops at nested module boundaries and skips vendor).
GO_DIRS := $(shell go list -f '{{.Dir}}' ./...)

# Build from the vendored dependency tree: offline, reproducible, and auditable
# (every dependency's source lives in vendor/ and shows up in review diffs). Go
# auto-selects -mod=vendor when vendor/ is present; we export it explicitly so a
# stray global GOFLAGS (e.g. -mod=mod) can't silently switch the build off the
# vendored tree. Do NOT use -mod=readonly here — it ignores vendor/ entirely.
export GOFLAGS := -mod=vendor

build:
	CGO_ENABLED=0 go build -trimpath -o bin/urvi ./cmd/cli

# Run the TUI directly. Loads .env (if present) so LLM_API_KEY and friends are
# exported for the process. Select an agent with AGENT=<name> (default: coding;
# "personal-assistant" is also available). e.g. make run AGENT=personal-assistant
run:
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/cli $(AGENT)

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
	# gosec is NOT module-aware: its ./... is a filesystem walk that descends into
	# the nested .worktrees/ checkouts (separate modules) and, under -mod=vendor,
	# reports modules.txt desyncs for those foreign trees. Scope it to THIS module's
	# package dirs via GO_DIRS (the same go-list idiom fmt/fmt-check use). go vet and
	# staticcheck are module-aware (go list stops at module boundaries), so they need
	# no scoping.
	go tool gosec $(GO_DIRS)

vuln:
	go mod verify
	go tool govulncheck ./...

secure: lint vuln

fuzz:
	@echo "Usage: go test -fuzz=FuzzXxx ./path/to/pkg -fuzztime=30s"
