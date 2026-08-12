.PHONY: test fmt fmt-check vendor vendor-scrub vendor-check staticcheck lint vuln verify secure fuzz

GO ?= go

# Module's own package dirs, excluding vendor/ and the nested .worktrees/ modules
# (go list ./... stops at nested module boundaries and skips vendor).
GO_DIRS = $(shell go list -f '{{.Dir}}' ./...)

# Build from the vendored dependency tree: offline, reproducible, and auditable
# (every dependency's source lives in vendor/ and shows up in review diffs). Go
# auto-selects -mod=vendor when vendor/ is present; we export it explicitly so a
# stray global GOFLAGS (e.g. -mod=mod) can't silently switch the build off the
# vendored tree. Do NOT use -mod=readonly here — it ignores vendor/ entirely.
export GOFLAGS := -mod=vendor

VENDOR_DIR ?= vendor
LOCAL_REPLACE_VENDOR_DIRS := \
	$(VENDOR_DIR)/github.com/looprig/core \
	$(VENDOR_DIR)/github.com/looprig/inference \
	$(VENDOR_DIR)/github.com/looprig/storage

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

# Refresh the auditable dependency tree, then remove only VCS metadata donated
# by the three declared local replace targets. A final whole-tree check catches
# metadata from any other source instead of broadening the scrub silently.
vendor:
	go mod vendor
	$(MAKE) vendor-scrub
	$(MAKE) vendor-check

vendor-scrub:
	rm -rf $(addsuffix /.git,$(LOCAL_REPLACE_VENDOR_DIRS))

vendor-check:
	@metadata=$$(find "$(VENDOR_DIR)" -name .git -print); \
	if [ -n "$$metadata" ]; then \
		echo "forbidden VCS metadata in $(VENDOR_DIR):"; echo "$$metadata"; exit 1; \
	fi

lint: fmt-check vendor-check
	go vet ./...
	$(MAKE) staticcheck
	# gosec is NOT module-aware: its ./... is a filesystem walk that descends into
	# the nested .worktrees/ checkouts (separate modules) and, under -mod=vendor,
	# reports modules.txt desyncs for those foreign trees. Scope it to THIS module's
	# package dirs via GO_DIRS (the same go-list idiom fmt/fmt-check use). go vet and
	# staticcheck are module-aware (go list stops at module boundaries), so they need
	# no scoping.
	go tool gosec $(GO_DIRS)

staticcheck:
	@GO="$(GO)" ./scripts/run-staticcheck.sh

vuln:
	go mod verify
	go tool govulncheck ./...

secure: lint vuln

fuzz:
	@echo "Usage: go test -fuzz=FuzzXxx ./path/to/pkg -fuzztime=30s"
