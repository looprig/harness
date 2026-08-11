# Client Module Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Ship `github.com/looprig/client` — a framework-neutral TypeScript session SDK plus a Svelte 5 reference web app, served by a Go backend-for-frontend that browses all session history and drives live sessions.

**Architecture:** A Go BFF embeds a static SPA and exposes a same-origin `/api/v1` surface. Its **read plane** is a composition-time seam — either harness `serve.ReadHandler` mounted in-process over a storage backend, or a reverse proxy to a remote `serve`. Its **live/control planes** are always reverse-proxied to a session host, with the bearer token held server-side. The browser boundary is `@looprig/client` (`sdk/core`): protocol types, ajv validation over serve's shipped JSON Schema, transports, and a framework-neutral session state machine. `@looprig/svelte` is a thin reactivity adapter; the Svelte app is the reference implementation, not the public boundary.

**Tech Stack:** Go 1.26 (stdlib-only on the server), harness `pkg/serve` + `pkg/serve/catalogreader` + `pkg/sessionstore`, `fsstore`/`natsstore` backends, TypeScript, `ajv`, `json-schema-to-ts`, Svelte 5 + SvelteKit `adapter-static`, vitest.

**Design doc:** `harness/docs/plans/2026-07-02-client-web-app-design.md` (read it before Task 1; this plan implements it and does not restate its rationale).

---

## Preconditions and conventions

**Read these before starting.** Getting them wrong will cost you a rewrite.

1. **Two repos.** Phase 0 (Tasks 1–4) lands in `github.com/looprig/harness`, which already exists at `/Users/ipotter/code/looprig/harness`. Phases 1a–1d land in a **new** `github.com/looprig/client` repo. Do not start Phase 1 until Phase 0 is tagged.
2. **Harness is stdlib-only in `pkg/serve`.** A dependency guard (`pkg/serve/deps_test.go`, `TestProductionImportsAreAllowed`) parses every non-test file in the package and fails on any import that is not stdlib or one of four allowed paths. Adding an import there will fail the build. This is deliberate.
3. **A second guard forbids the identifier `Runner`** at package scope in `pkg/serve` (`forbiddenServeSurface`). The old `serve.Handler[S](runner Runner[S], …)` signature is gone; the current one is `Handler[S LiveSession, O any](rig Rig[S, O], reads Reader, opts ...Option) http.Handler`. Do not reintroduce the name.
4. **Every Go test runs with `-race`.** `make test` is `go test -race ./...`. A test that passes without `-race` and fails with it is not passing.
5. **`make secure` before every commit** in the harness repo — it runs `fmt-check`, `vendor-check`, `vet`, `staticcheck`, `gosec`, then `go mod verify` + `govulncheck`.
6. **Commit messages omit `Co-Authored-By` trailers.** This is a looprig convention.
7. **No new external dependency without asking the user first**, in either repo. The client repo's approved npm list is seeded in Task 5; anything beyond it needs a fresh "yes".
8. **TDD is not optional here.** Every task below is written as: failing test → run it and watch it fail → minimal implementation → run it and watch it pass → commit. The "watch it fail" step is not ceremony; it is how you learn the test actually exercises the thing you think it does.

### Verification gate between phases

At the end of each phase, all of these must pass before the next phase starts:

```bash
# harness repo
make test && make secure

# client repo (from Phase 1a onward)
make test && make secure && npm run -w sdk/core test && npm run -w sdk/core typecheck
```

---

# Phase 0 — harness: `serve.ReadHandler`

**Why this exists:** the BFF hosts no agent, so it has no `Rig` to pass. As built, the three read handlers are methods on the generic `server[S, O]`, so the read plane is type-coupled to a session factory that a browse-only process cannot supply. This phase carves them onto a rig-free receiver and exposes a read-only handler.

**Good news, verified against the source:** `handleListSessions`, `handleStatus`, and `handleJournal` touch **only** `s.reader`. They never read `s.rig`, `s.registry`, `s.cfg`, or `s.idem`. The carve-out is therefore a pure receiver change with no logic moved.

**Branch:** `git checkout -b feat/serve-read-handler`

---

### Task 1: Carve the read handlers onto a rig-free receiver

This is a **pure refactor**. Every existing test must still pass, unchanged. If you find yourself editing an existing test, stop — you have changed behavior and should not have.

**Files:**
- Create: `pkg/serve/read_server.go`
- Modify: `pkg/serve/server_core.go:22-49`
- Modify: `pkg/serve/handlers_read.go:24,52,84` (receiver only)
- Modify: `pkg/serve/handlers_capabilities.go:33-39`

**Step 1: Write the failing test**

Create `pkg/serve/read_server_test.go`:

```go
package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/looprig/core/uuid"
)

// stubReader is a Reader that returns canned values, so the read handlers can be
// exercised with no store, no rig, and no live session.
type stubReader struct {
	list    SessionList
	status  SessionStatus
	journal EventJournalPage
	err     error
}

func (s stubReader) ListSessions(context.Context, Page) (SessionList, error) {
	return s.list, s.err
}

func (s stubReader) ReadStatus(context.Context, uuid.UUID) (SessionStatus, error) {
	return s.status, s.err
}

func (s stubReader) ReadJournal(context.Context, uuid.UUID, JournalPage) (EventJournalPage, error) {
	return s.journal, s.err
}

// TestReadServerServesListWithoutRig proves the read plane is reachable from a
// receiver that holds NO session factory — the property the BFF depends on.
func TestReadServerServesListWithoutRig(t *testing.T) {
	t.Parallel()

	rs := &readServer{reader: stubReader{list: SessionList{Done: true}}}

	rec := httptest.NewRecorder()
	rs.handleListSessions(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got SessionList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !got.Done {
		t.Errorf("Done = false, want true")
	}
}
```

**Step 2: Run it and watch it fail**

```bash
go test -race ./pkg/serve/ -run TestReadServerServesListWithoutRig
```

Expected: **FAIL**, `undefined: readServer`.

**Step 3: Write the minimal implementation**

Create `pkg/serve/read_server.go`:

```go
package serve

// readServer is the rig-free holder for the stateless read plane. It owns only the
// dependencies a pure read needs — the Reader and the capability document's feature
// list — so the read routes can be served by a process that hosts no agent and
// therefore has no session factory to supply (the BFF case).
//
// The full server[S, O] embeds this holder, so the read handlers are promoted onto it
// and the live/control server keeps serving all ten routes from one value. Splitting
// the receiver, not the logic, is the whole point: there is exactly one
// implementation of each read route.
//
// It carries no request state — one readServer serves every request.
type readServer struct {
	reader   Reader
	features []string
}
```

Now change the three receivers in `pkg/serve/handlers_read.go` — `(s *server[S, O])` becomes `(s *readServer)` on lines 24, 52, and 84. Change nothing else in that file.

In `pkg/serve/handlers_capabilities.go`, make the feature list a field rather than a literal:

```go
// handleCapabilities serves GET /v1/capabilities: the static protocol-discovery
// document (SPEC §6). It reads no request state — the document is fixed at
// construction from the feature set this server actually serves, so a read-only
// server honestly advertises less than a full one. The Features order is part of
// the contract.
func (s *readServer) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, capabilities{
		Protocol: protocolName,
		Version:  protocolVersion,
		Features: s.features,
	})
}
```

Add the canonical full set next to the feature constants in the same file:

```go
// fullFeatures is the capability set a complete server (live + control + read)
// advertises, in the canonical contract order. readOnlyFeatures (read_server.go) is
// the honest subset for a handler with no live plane.
var fullFeatures = []string{featureJournal, featureLiveSSE, featureEphemeralSSE, featureGateResponse}
```

Finally, embed the holder in `pkg/serve/server_core.go`. Replace the `reader` field with the embedded pointer and update the constructor:

```go
type server[S LiveSession, O any] struct {
	*readServer
	rig      Rig[S, O]
	registry *registry
	cfg      *config
	idem     *idempotencyStore
}

func newServer[S LiveSession, O any](rig Rig[S, O], reader Reader, cfg *config) *server[S, O] {
	return &server[S, O]{
		readServer: &readServer{reader: reader, features: fullFeatures},
		rig:        rig,
		registry:   newRegistry(),
		cfg:        cfg,
		idem:       newIdempotencyStore(defaultIdempotencyTTL),
	}
}
```

Keep the existing doc comments on `server` and `newServer`; just add a sentence to `server`'s noting that the read plane is embedded.

**Step 4: Run the tests and watch them pass**

```bash
go test -race ./pkg/serve/...
```

Expected: **PASS**, all of it. `srv.handleCapabilities` and `srv.handleListSessions` still resolve at every existing call site through embedding promotion, so `mux.go` and the existing tests need no edits. If you had to touch either, you did something other than a pure refactor.

**Step 5: Commit**

```bash
git add pkg/serve/read_server.go pkg/serve/read_server_test.go pkg/serve/server_core.go pkg/serve/handlers_read.go pkg/serve/handlers_capabilities.go
git commit -m "refactor(serve): carve read handlers onto a rig-free receiver"
```

---

### Task 2: Reduced capability document for a read-only server

**Files:**
- Modify: `pkg/serve/read_server.go`
- Test: `pkg/serve/read_server_test.go`

**Step 1: Write the failing test**

Append to `pkg/serve/read_server_test.go`:

```go
// TestReadServerCapabilitiesAdvertiseJournalOnly proves a read-only server does not
// claim live/control planes it cannot serve. A client that trusts the document and
// then opens an SSE stream against a read-only host would hang; honest advertisement
// is the contract that prevents it.
func TestReadServerCapabilitiesAdvertiseJournalOnly(t *testing.T) {
	t.Parallel()

	rs := &readServer{reader: stubReader{}, features: readOnlyFeatures}

	rec := httptest.NewRecorder()
	rs.handleCapabilities(rec, httptest.NewRequest(http.MethodGet, "/v1/capabilities", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got capabilities
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Protocol != protocolName || got.Version != protocolVersion {
		t.Errorf("protocol/version = %q/%d, want %q/%d", got.Protocol, got.Version, protocolName, protocolVersion)
	}
	if len(got.Features) != 1 || got.Features[0] != featureJournal {
		t.Errorf("features = %v, want exactly [%q]", got.Features, featureJournal)
	}
}
```

**Step 2: Run it and watch it fail**

```bash
go test -race ./pkg/serve/ -run TestReadServerCapabilitiesAdvertiseJournalOnly
```

Expected: **FAIL**, `undefined: readOnlyFeatures`.

**Step 3: Write the minimal implementation**

Add to `pkg/serve/read_server.go`:

```go
// readOnlyFeatures is the capability set a ReadHandler advertises: the journal plane
// and nothing else. A read-only server has no live session and no control routes, so
// claiming live_sse/ephemeral_sse/gate_response would be a lie a client acts on.
var readOnlyFeatures = []string{featureJournal}
```

**Step 4: Run it and watch it pass**

```bash
go test -race ./pkg/serve/...
```

Expected: **PASS**.

**Step 5: Commit**

```bash
git add pkg/serve/read_server.go pkg/serve/read_server_test.go
git commit -m "feat(serve): honest reduced capability set for read-only servers"
```

---

### Task 3: Export `ReadHandler`

**Files:**
- Modify: `pkg/serve/mux.go`
- Test: `pkg/serve/read_server_test.go`

**Step 1: Write the failing tests**

Append to `pkg/serve/read_server_test.go`:

```go
// TestReadHandlerRoutes proves ReadHandler serves exactly the three stateless read
// routes plus capabilities, and that every live/control route is absent. The absence
// assertions are the security-relevant half: a browse-only deployment must not expose
// a control surface at all, rather than exposing one that 403s.
func TestReadHandlerRoutes(t *testing.T) {
	t.Parallel()

	// core/uuid exposes New() (UUID, error) and MustParse(string) — there is no
	// Must(NewV7()) helper. Existing serve tests use a literal via MustParse.
	sid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	h := ReadHandler(stubReader{status: SessionStatus{SessionID: sid}})

	tests := []struct {
		name       string
		method     string
		target     string
		wantAbsent bool
	}{
		{name: "capabilities", method: http.MethodGet, target: "/v1/capabilities"},
		{name: "list", method: http.MethodGet, target: "/v1/sessions"},
		{name: "status", method: http.MethodGet, target: "/v1/sessions/" + sid.String() + "/status"},
		{name: "journal", method: http.MethodGet, target: "/v1/sessions/" + sid.String() + "/journal"},

		{name: "create absent", method: http.MethodPost, target: "/v1/sessions", wantAbsent: true},
		{name: "events absent", method: http.MethodGet, target: "/v1/sessions/" + sid.String() + "/events", wantAbsent: true},
		{name: "input absent", method: http.MethodPost, target: "/v1/sessions/" + sid.String() + "/input", wantAbsent: true},
		{name: "interrupt absent", method: http.MethodPost, target: "/v1/sessions/" + sid.String() + "/interrupt", wantAbsent: true},
		{name: "restore absent", method: http.MethodPost, target: "/v1/sessions/" + sid.String() + "/restore", wantAbsent: true},
		{name: "gate absent", method: http.MethodPost, target: "/v1/sessions/" + sid.String() + "/gates/" + sid.String(), wantAbsent: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.target, http.NoBody))

			switch {
			case tt.wantAbsent && rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed:
				t.Errorf("%s %s = %d, want 404/405 (route must not be registered)", tt.method, tt.target, rec.Code)
			case !tt.wantAbsent && rec.Code != http.StatusOK:
				t.Errorf("%s %s = %d, want %d", tt.method, tt.target, rec.Code, http.StatusOK)
			}
		})
	}
}

// TestReadHandlerRespectsOptions proves ReadHandler shares the Handler middleware
// chain, so an authenticator installed by the caller actually runs.
func TestReadHandlerRespectsOptions(t *testing.T) {
	t.Parallel()

	denied := errors.New("denied")
	h := ReadHandler(stubReader{}, WithAuth(func(*http.Request) error { return denied }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions", http.NoBody))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
```

Add `"errors"` to the test file's imports.

**Step 2: Run them and watch them fail**

```bash
go test -race ./pkg/serve/ -run 'TestReadHandler'
```

Expected: **FAIL**, `undefined: ReadHandler`.

**Step 3: Write the minimal implementation**

Add to `pkg/serve/mux.go`, below `Handler`:

```go
// ReadHandler builds the stateless READ-ONLY session HTTP surface: capabilities plus
// the list/status/journal routes, and nothing else. It exists for a process that
// serves history but hosts no agent — a browse-only BFF or a read-plane pod — which
// therefore has no Rig to hand to Handler.
//
// It reuses the identical handlers Handler registers (they hang off the shared
// rig-free readServer), so there is exactly ONE implementation of each read route and
// no second wire contract to drift. The live and control routes are NOT registered:
// a control request 404s from the mux before any handler runs, so browse-only mode is
// a property of the type system rather than a runtime authorization check that could
// fall through (fail secure).
//
// The capability document it serves advertises `journal` only — a read-only server
// must not claim planes it cannot serve.
//
// Like Handler it returns a *boundHandler carrying the has-auth bit, so a downstream
// Server bind stays fail-secure for a public address.
func ReadHandler(reads Reader, opts ...Option) http.Handler {
	cfg := newConfig(opts...)
	srv := &readServer{reader: reads, features: readOnlyFeatures}

	mux := http.NewServeMux()
	mux.HandleFunc(routeCapabilities, srv.handleCapabilities)
	mux.HandleFunc(routeList, srv.handleListSessions)
	mux.HandleFunc(routeStatus, srv.handleStatus)
	mux.HandleFunc(routeJournal, srv.handleJournal)

	return &boundHandler{Handler: cfg.wrap(mux), hasAuth: cfg.hasAuth()}
}
```

**Step 4: Run everything and watch it pass**

```bash
go test -race ./pkg/serve/... && make secure
```

Expected: **PASS**, including `TestProductionImportsAreAllowed` (no new imports were added) and `TestProductionHasNoLegacyDeclarations` (nothing named `Runner`).

**Step 5: Commit**

```bash
git add pkg/serve/mux.go pkg/serve/read_server_test.go
git commit -m "feat(serve): add ReadHandler for rig-free read-only deployments"
```

---

### Task 4: Contract artifacts and release

The client pins harness by version and copies its schema + fixtures. Give it something to pin.

**Files:**
- Create: `pkg/serve/testdata/fixtures/capabilities_read_only.json`
- Modify: `pkg/serve/testdata/openapi.yaml`
- Modify: `pkg/serve/README.md`

**Step 1: Write the failing test**

Append to `pkg/serve/read_server_test.go`:

```go
// TestReadOnlyCapabilitiesMatchesFixture pins the read-only discovery document to a
// golden fixture the client repo copies verbatim. A wire change here breaks this test
// AND the client's ajv conformance suite — the shared-fixture drift mechanism.
func TestReadOnlyCapabilitiesMatchesFixture(t *testing.T) {
	t.Parallel()

	want, err := os.ReadFile("testdata/fixtures/capabilities_read_only.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	rec := httptest.NewRecorder()
	rs := &readServer{reader: stubReader{}, features: readOnlyFeatures}
	rs.handleCapabilities(rec, httptest.NewRequest(http.MethodGet, "/v1/capabilities", http.NoBody))

	var got, wantDoc capabilities
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if err := json.Unmarshal(want, &wantDoc); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if got.Protocol != wantDoc.Protocol || got.Version != wantDoc.Version {
		t.Fatalf("protocol/version = %q/%d, want %q/%d", got.Protocol, got.Version, wantDoc.Protocol, wantDoc.Version)
	}
	if len(got.Features) != len(wantDoc.Features) {
		t.Fatalf("features = %v, want %v", got.Features, wantDoc.Features)
	}
	for i := range got.Features {
		if got.Features[i] != wantDoc.Features[i] {
			t.Fatalf("features = %v, want %v", got.Features, wantDoc.Features)
		}
	}
}
```

Add `"os"` to the test imports.

**Step 2: Run it and watch it fail**

```bash
go test -race ./pkg/serve/ -run TestReadOnlyCapabilitiesMatchesFixture
```

Expected: **FAIL**, no such file.

**Step 3: Create the fixture**

`pkg/serve/testdata/fixtures/capabilities_read_only.json`:

```json
{
  "protocol": "looprig.serve",
  "version": 1,
  "features": ["journal"]
}
```

Then add a short paragraph to `pkg/serve/README.md` documenting `ReadHandler` next to `Handler`, and add the read-only capabilities response as a documented variant in `testdata/openapi.yaml`. Keep the OpenAPI edit minimal — it is a hand-maintained doc artifact, and the only assertion on it is that it exists and is non-empty.

**Step 4: Run it and watch it pass**

```bash
make test && make secure
```

Expected: **PASS**.

**Step 5: Commit and tag**

```bash
git add pkg/serve/testdata pkg/serve/README.md pkg/serve/read_server_test.go
git commit -m "docs(serve): document ReadHandler and pin read-only capabilities fixture"
```

Then merge to `main` and tag. Check the current tag first (`git tag --list 'v0.*' | tail -5` — `v0.22.0` at time of writing) and cut the next **minor**, since this adds exported API:

```bash
git checkout main && git merge --no-ff feat/serve-read-handler
make test && make secure
git tag v0.23.0 && git push origin main --tags
```

**Record the tag you actually cut — Phase 1a pins it.**

---

# Phase 1a — client module, read plane, security

**Repo:** new `github.com/looprig/client`. Create it at `/Users/ipotter/code/looprig/client`.

---

### Task 5: Module scaffold

**Files:**
- Create: `go.mod`, `Makefile`, `CLAUDE.md`, `.gitignore`, `README.md`, `.github/workflows/ci.yml`

**Step 1: Initialize**

```bash
mkdir -p /Users/ipotter/code/looprig/client && cd /Users/ipotter/code/looprig/client
git init && go mod init github.com/looprig/client
go get github.com/looprig/harness@v0.23.0   # the tag from Task 4
```

**Step 2: Write `CLAUDE.md`**

Copy harness's `CLAUDE.md` verbatim, then replace the "Dependencies" approved-list section with:

```markdown
## Dependencies

Inherits looprig's rules: prefer stdlib; external packages require explicit user
approval in the current conversation; record approvals here.

**Go (approved):**
- `github.com/looprig/harness` — `pkg/serve` (wire contract, `ReadHandler`),
  `pkg/serve/catalogreader`, `pkg/sessionstore`, `pkg/event`, `pkg/journal`
- `github.com/looprig/core` — `content`, `uuid`
- `github.com/looprig/fsstore` — laptop storage backend (mounted-read mode only)
- `github.com/looprig/natsstore` — cloud storage backend (mounted-read mode only)
- `github.com/looprig/storage` — `memstore` for tests

**npm (approved):**
- `ajv` — runtime validation against serve's shipped JSON Schema
- `json-schema-to-ts` — type-level `FromSchema<>`; generates no files
- `svelte`, `@sveltejs/kit`, `@sveltejs/adapter-static`, `vite`, `vitest`
- `shadcn-svelte` (on Bits UI), Svelte AI Elements — presentational only
- `virtua` — transcript virtualization
- `shiki` — code highlighting
- `svelte-exmarkdown` — markdown rendering

**Explicitly NOT approved:** `invopop/jsonschema` (serve owns its hand-authored
schema), `json-schema-to-zod`, `@ai-sdk/svelte`'s transport, anything pulling the
charm/TUI stack (`github.com/looprig/tui`), and `swe` or any agent implementation.
```

**Step 3: Write the `Makefile`**

```makefile
GO_DIRS := $(shell go list -f '{{.Dir}}' ./...)

.PHONY: test fmt fmt-check lint vet staticcheck gosec vuln secure contract sdk app build

test:
	go test -race ./...

fmt:
	gofmt -w $(GO_DIRS)

fmt-check:
	@unformatted=$$(gofmt -l $(GO_DIRS)); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed (run 'make fmt'):"; echo "$$unformatted"; exit 1; \
	fi

vet:
	go vet ./...

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

gosec:
	go run github.com/securego/gosec/v2/cmd/gosec@latest -quiet ./...

lint: fmt-check vet staticcheck gosec

vuln:
	go mod verify
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

secure: lint vuln

build:
	CGO_ENABLED=0 go build -trimpath ./cmd/...
```

`contract`, `sdk`, and `app` targets land in Tasks 6, 14, and 17.

**Step 4: Write `.gitignore`**

```gitignore
node_modules/
pkg/webui/dist/*
!pkg/webui/dist/index.html
app/.svelte-kit/
app/build/
*.test
```

**Step 5: Verify and commit**

```bash
go build ./... && git add -A && git commit -m "chore: scaffold client module"
```

---

### Task 6: Vendor the wire contract

The client copies serve's `testdata/{schema,fixtures}` per harness version. Both repos then parse the same golden bytes, so a harness wire change fails in both.

**Files:**
- Create: `contract/README.md`, `contract/schema/`, `contract/fixtures/`
- Modify: `Makefile`
- Test: `contract/contract_test.go`

**Step 1: Add the `contract` target**

```makefile
HARNESS_VERSION := v0.23.0
HARNESS_DIR := $(shell go list -m -f '{{.Dir}}' github.com/looprig/harness)

contract:
	rm -rf contract/schema contract/fixtures
	mkdir -p contract/schema contract/fixtures
	cp $(HARNESS_DIR)/pkg/serve/testdata/schema/*.json contract/schema/
	cp $(HARNESS_DIR)/pkg/serve/testdata/fixtures/* contract/fixtures/
	@echo "$(HARNESS_VERSION)" > contract/VERSION
```

**Step 2: Run it**

```bash
make contract && ls contract/schema contract/fixtures
```

Expected: 18 schema files and 18 fixtures (17 originals plus `capabilities_read_only.json` from Task 4).

**Step 3: Write the drift test**

`contract/contract_test.go` — assert the copy is byte-identical to the pinned harness module, so a `go get -u` that moves the contract without re-running `make contract` fails loudly:

```go
package contract_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// harnessTestdata resolves the pinned harness module's serve testdata directory from
// the module cache, so the assertion is against the exact version go.mod names.
func harnessTestdata(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/looprig/harness").Output()
	if err != nil {
		t.Fatalf("locate harness module: %v", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "pkg", "serve", "testdata")
}

// TestContractMatchesPinnedHarness proves contract/ is a verbatim copy of the pinned
// harness version's wire artifacts. This is the drift guard: bumping harness without
// re-running `make contract` fails here, and a genuine wire change surfaces as a
// reviewable fixture diff rather than a silent protocol mismatch at runtime.
func TestContractMatchesPinnedHarness(t *testing.T) {
	t.Parallel()

	upstream := harnessTestdata(t)
	for _, dir := range []string{"schema", "fixtures"} {
		entries, err := os.ReadDir(filepath.Join(upstream, dir))
		if err != nil {
			t.Fatalf("read upstream %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			want, err := os.ReadFile(filepath.Join(upstream, dir, e.Name()))
			if err != nil {
				t.Fatalf("read upstream %s/%s: %v", dir, e.Name(), err)
			}
			got, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Errorf("missing vendored %s/%s (run `make contract`): %v", dir, e.Name(), err)
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s/%s differs from pinned harness (run `make contract`)", dir, e.Name())
			}
		}
	}
}
```

**Step 4: Run it and watch it pass**

```bash
go test -race ./contract/
```

Then deliberately break it to prove the guard works: edit one byte of a vendored fixture, re-run, confirm **FAIL**, then `make contract` to restore.

**Step 5: Commit**

```bash
git add contract Makefile && git commit -m "feat(contract): vendor serve wire schema and fixtures with a drift guard"
```

---

### Task 7: Typed configuration at the composition root

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Step 1: Write the failing test**

Cover, at minimum: loopback default; explicit addr accepted; `CLIENT_HOST_TOKEN` required **iff** `CLIENT_HOST_URL` is set (fail loud, and the error must not contain the token value); `CLIENT_STORE` and `CLIENT_HOST_URL` both empty ⇒ valid browse-only-with-no-source is **rejected** (nothing to read); a non-`https` remote host URL rejected unless it is loopback.

```go
func TestLoadRejectsHostWithoutToken(t *testing.T) {
	t.Parallel()
	_, err := config.Load(map[string]string{
		"CLIENT_STORE":    "fs:/tmp/x",
		"CLIENT_HOST_URL": "https://host.example",
	})
	if err == nil {
		t.Fatal("Load() err = nil, want a missing-token error")
	}
	var missing *config.MissingSecretError
	if !errors.As(err, &missing) {
		t.Fatalf("Load() err = %T, want *config.MissingSecretError", err)
	}
}

func TestLoadErrorNeverContainsSecret(t *testing.T) {
	t.Parallel()
	const secret = "super-secret-token-value"
	_, err := config.Load(map[string]string{
		"CLIENT_HOST_URL":   "http://not-loopback.example",
		"CLIENT_HOST_TOKEN": secret,
	})
	if err == nil {
		t.Fatal("Load() err = nil, want a scheme rejection")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("Load() error text leaked the token")
	}
}
```

Take `Load` a `map[string]string` rather than reading `os.Getenv` directly — it makes the tests parallel-safe and keeps the env read at `cmd/`.

**Step 2–5:** implement `Config`, `MissingSecretError`, and the validation; run; commit as `feat(config): typed fail-loud configuration`.

---

### Task 8: `ReadSource` — mounted mode

**Files:**
- Create: `internal/bff/readsource.go`, `internal/bff/mounted.go`
- Test: `internal/bff/mounted_test.go`

The read plane is an `http.Handler` chosen at composition. Declare the seam:

```go
// ReadSource is the BFF's read plane: an http.Handler serving serve's stateless read
// routes. It is chosen at the composition root — either serve.ReadHandler mounted
// in-process over a local store, or a reverse proxy to a remote serve. The BFF and
// the SDK are blind to which is wired, because both speak the identical wire contract.
type ReadSource interface {
	http.Handler
}
```

Mounted mode is `serve.ReadHandler(catalogreader.New(catalog, store))`. Test it against a `memstore`-backed `sessionstore` using the same helper shape harness uses (`sessionstore.Open(memstore.New())`, `st.OpenCatalog(...)`) — fast, deterministic, no NATS. Assert a seeded session appears in `GET /v1/sessions` and that its status resolves.

Commit: `feat(bff): mounted read source over serve.ReadHandler`.

---

### Task 9: `ReadSource` — proxied mode

**Files:**
- Create: `internal/bff/proxied.go`
- Test: `internal/bff/proxied_test.go`

A `net/http/httputil.ReverseProxy` to a remote serve read plane, with:
- an **allowlist** of forwarded paths/methods (never an arbitrary upstream path);
- `Authorization` injected server-side, any inbound `Authorization` **stripped**;
- `tls.Config{MinVersion: tls.VersionTLS12}`, never `InsecureSkipVerify`;
- explicit transport timeouts; every call context-bounded.

Test against an `httptest` stub serve. Assert: an allowlisted path reaches upstream with the injected token; a non-allowlisted path is refused **without** contacting upstream (assert the stub recorded zero requests); an inbound `Authorization` from the SPA never reaches upstream.

Commit: `feat(bff): proxied read source with path allowlist and token custody`.

---

### Task 10: Host/Origin + DNS-rebind guard

Loopback binding alone does not stop DNS rebinding: a malicious page can rebind a name to `127.0.0.1` and drive a token-holding BFF, and no CORS preflight fires for simple same-origin-shaped requests.

**Files:**
- Create: `internal/bff/guard.go`
- Test: `internal/bff/guard_test.go`

**Step 1: Write the failing table test**

```go
func TestHostGuard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		host     string
		origin   string
		wantCode int
	}{
		{name: "loopback v4", host: "127.0.0.1:7777", wantCode: http.StatusOK},
		{name: "localhost", host: "localhost:7777", wantCode: http.StatusOK},
		{name: "loopback v6", host: "[::1]:7777", wantCode: http.StatusOK},
		{name: "rebound dns name", host: "evil.example:7777", wantCode: http.StatusForbidden},
		{name: "bare ip not loopback", host: "10.0.0.5:7777", wantCode: http.StatusForbidden},
		{name: "cross origin", host: "127.0.0.1:7777", origin: "https://evil.example", wantCode: http.StatusForbidden},
		{name: "same origin", host: "127.0.0.1:7777", origin: "http://127.0.0.1:7777", wantCode: http.StatusOK},
		{name: "empty host", host: "", wantCode: http.StatusForbidden},
	}
	// ... run each through the guard wrapping a 200 handler
}
```

The guard runs **before** auth. Reject any `Host` outside `{127.0.0.1, localhost, [::1]}` or the configured public host, and any cross-origin `Origin`. Fail secure: an unparseable `Host` is a rejection, not a pass.

Commit: `feat(bff): host/origin guard against DNS rebinding`.

---

### Task 11: CSRF tokens for control POSTs

Per-page-load token minted into the SPA, verified on every control POST. Use `crypto/rand`, constant-time comparison (`crypto/subtle.ConstantTimeCompare`), and a bounded TTL.

Test: a POST with no token is 403; with a stale/unknown token is 403; with a valid token passes; the comparison is constant-time (assert by construction — call `subtle.ConstantTimeCompare`, do not try to time it).

Commit: `feat(bff): CSRF tokens for control POSTs`.

---

### Task 12: BFF mux and synthesized capabilities

**Files:**
- Create: `internal/bff/mux.go`, `internal/bff/capabilities.go`
- Test: `internal/bff/mux_test.go`

Mount `ReadSource` under `/api/v1`, stripping the prefix so serve's own routes match. `GET /api/v1/capabilities` is **BFF-synthesized**, not proxied verbatim: it advertises the BFF's own feature set, so a browse-only deployment (`Host == nil`) advertises `journal` only and never `live_sse`/`gate_response`.

Test: with a host wired, capabilities include the live/control features; with `Host == nil`, exactly `["journal"]`, **and** the control routes are absent from the mux entirely (404, not 403).

Commit: `feat(bff): api mux with synthesized capability negotiation`.

---

### Task 13: `pkg/webui` embed

**Files:**
- Create: `pkg/webui/webui.go`, `pkg/webui/dist/index.html` (committed placeholder)
- Test: `pkg/webui/webui_test.go`

`pkg/webui` is the **only exported package** — swe reuses the SPA embed, not the proxying BFF. Export `FS` plus an SPA-fallback handler that serves `index.html` for unmatched non-asset paths. Every served path must be `filepath.Clean`ed and confined to the embedded FS.

The committed one-line `dist/index.html` placeholder keeps `go build`/`vet`/`test` green with no Node installed. Test path traversal explicitly: `../../etc/passwd` and encoded variants must not escape.

Commit: `feat(webui): embedded SPA host with traversal-safe fallback`.

---

### Task 14: npm workspace and `sdk/core` scaffold

**Files:**
- Create: `package.json` (workspace root), `sdk/core/package.json`, `sdk/core/tsconfig.json`, `sdk/core/vitest.config.ts`
- Modify: `Makefile` (add `sdk`)

Root `package.json` declares `"workspaces": ["sdk/core", "sdk/svelte", "app"]`. `sdk/core` is `@looprig/client`, dependencies `ajv` and `json-schema-to-ts` only, `"type": "module"`.

```makefile
sdk:
	npm ci && npm run build -w sdk/core && npm run test -w sdk/core
```

Commit: `chore(sdk): scaffold npm workspace and @looprig/client package`.

---

### Task 15: `sdk/core` — types and ajv validation

**Files:**
- Create: `sdk/core/src/schema.ts`, `sdk/core/src/validate.ts`, `sdk/core/src/types.ts`
- Test: `sdk/core/test/contract.test.ts`

Types are **type-level only** — `FromSchema<typeof sessionListSchema>` from `json-schema-to-ts`. Nothing is generated to disk, so nothing can rot. Runtime validation compiles the same schema with ajv and **parses** at the SDK boundary; it does not cast.

The conformance test parses every fixture in `contract/fixtures/` through ajv against its schema. This is the other half of the drift mechanism: harness changes the wire → the golden fixture changes → this test fails here and in harness's own Go tests.

```ts
// A fixture that fails its schema means the vendored contract is internally
// inconsistent — the copy is corrupt or harness shipped a mismatch. Either way it
// must fail loudly here rather than at runtime in a browser.
test.each(fixtureCases)("fixture %s validates against %s", (fixture, schema) => {
  const validate = ajv.compile(schema);
  expect(validate(fixture)).toBe(true);
});
```

Commit: `feat(sdk): ajv validation and type-level DTOs over the vendored contract`.

---

### Task 16: `sdk/core` — transport and cold reads

**Files:**
- Create: `sdk/core/src/transport.ts`, `sdk/core/src/client.ts`, `sdk/core/src/errors.ts`
- Test: `sdk/core/test/transport.test.ts`, `sdk/core/test/errors.test.ts`

`LooprigTransport` interface with `BFFTransport` (same-origin `/api/...`) as the first implementation; `ServeTransport` (direct to `pkg/serve`) follows in Task 29. Implement `listSessions`, `readStatus`, `readHistory`. Every response is ajv-parsed before it reaches a caller.

Typed errors from serve's stable error envelope, carrying the machine-readable `code` and any retry metadata — callers switch on the code, never on message text. Cover each envelope fixture (`error_400/404/409/500/503.json`) and assert abort handling via `AbortController`.

Commit: `feat(sdk): BFF transport, cold reads, and typed error envelope`.

---

**PHASE 1a GATE** — run the full verification gate. Do not proceed until green.

---

# Phase 1b — Svelte reference shell

### Task 17: SvelteKit `adapter-static` scaffold

SvelteKit with `adapter-static` and `export const ssr = false` in the root layout → pure static assets, no Node at serve time, embeds cleanly into `embed.FS` and (Phase 5) the Wails shell. SvelteKit is the router/build host only; no SSR/server features are in play.

`make app` builds into `pkg/webui/dist/`. Dev loop is `vite dev` proxying `/api → 127.0.0.1:<bff>`, so CORS never exists in dev or prod.

Commit: `chore(app): sveltekit adapter-static scaffold`.

---

### Task 18: `@looprig/svelte` adapter

**Files:** `sdk/svelte/src/session.svelte.ts`, `sdk/svelte/test/session.test.ts`

A thin wrapper turning `sdk/core`'s state machine into Svelte 5 runes. **The adapter must not parse raw SSE, know looprig event internals, or implement its own history join.** Its tests cover reactivity lifecycle, cleanup, and unsubscribe-on-destroy — not protocol behavior, which is `sdk/core`'s job.

Commit: `feat(svelte): reactivity adapter over @looprig/client`.

---

### Task 19: Session list route

Route + `shadcn-svelte` data table over `listSessions`. Loading, empty, and error states are all required — an empty catalog is a normal state, not an error.

Commit: `feat(app): session list route`.

---

### Task 20: Cold transcript route

Renders a session's journal by folding the DTO through `sdk/core`. No HTML shortcut exists — `pkg/transcript` was archived out of harness and the client must not depend on the charm/TUI stack. Component tests fold a recorded DTO stream; `virtua` virtualizes long transcripts; `shiki` renders code blocks; `svelte-exmarkdown` renders markdown with fenced code routed to shiki.

Commit: `feat(app): cold transcript rendering`.

---

**PHASE 1b GATE.**

---

# Phase 1c — live plane and the exact seam join

### Task 21: SSE reverse-proxy

**Files:** `internal/bff/sse.go`, `internal/bff/sse_test.go` (`//go:build integration`)

Proxy `GET /api/v1/sessions/{sid}/events` to the host. It **must forward `Last-Event-ID`** on reconnect and pass downstream `id:` stamps through untouched, or lossless resume breaks through the BFF. Use a flush loop with an idle deadline — never an unbounded `io.Copy`.

Test against a stub serve: assert `Last-Event-ID` reaches upstream, `id:` stamps survive, the stream flushes incrementally rather than buffering to completion, and teardown is clean when the client disconnects mid-stream.

Commit: `feat(bff): SSE reverse-proxy preserving the resume seam`.

---

### Task 22: SSE frame parsing

**Files:** `sdk/core/src/sse.ts`, `sdk/core/test/sse.test.ts`

Parse `event: enduring` (always carries `id: <journal_seq>`) and `event: ephemeral` (never sequenced). Frame bodies are `{"v":1,"event":<envelope>}`. Drive the tests from the golden `.sse` fixtures. Cover chunk boundaries splitting a frame mid-line — a real network will do this and a naive line parser will corrupt the stream.

Commit: `feat(sdk): SSE frame parser over golden fixtures`.

---

### Task 23: Event folding — every delta kind

**Files:** `sdk/core/src/fold.ts`, `sdk/core/test/fold.test.ts`

Fold both a replayed journal record and a live frame into **one** shape, so history and live render through a single path.

The `ephemeral` delta kinds are enumerated by `contract/schema/ephemeral_frame.schema.json` and **all** must be handled:

| Kind | Payload |
|---|---|
| `token_delta` | tagged chunk DTO — text / thinking / tool-use |
| `tool_call_started` | tool-call delta |
| `tool_call_completed` | tool-call delta |
| `input_queued` | *(absent)* |
| `compaction_started` | attempt id, reason, basis |

`input_queued` and `compaction_started` shipped after the design doc's July revision. **An unhandled kind must produce a typed, surfaced fold error — never a silent drop.** Silent drops are how the next wire addition gets discovered in production instead of in CI. Test one case per kind plus an unknown-kind case asserting the typed error.

Commit: `feat(sdk): fold every ephemeral delta kind with loud unknown-kind handling`.

---

### Task 24: The exact history→live join

**Files:** `sdk/core/src/join.ts`, `sdk/core/test/join.test.ts`

The seam between history and live is the journal sequence, and the join is **exact** because serve already stamps `id: <journal_seq>` on every `enduring` frame:

1. subscribe to `…/events`, buffering everything;
2. page `…/journal` to tip `T`;
3. drop buffered frames with `journal_seq <= T`;
4. follow live.

Only `ephemeral` frames are best-effort — they are unsequenced by design and may be lost across a reconnect.

**The test that matters most:** an event that lands *inside the join window* — after the journal page is taken but before the buffer is drained — must appear exactly once. Also cover: reconnect mid-stream resumes from `Last-Event-ID` with no gap and no duplicate; an empty journal (brand-new session) joins cleanly; a session that ends during the join terminates cleanly.

Commit: `feat(sdk): exact lossless history-to-live join`.

---

### Task 25: Live transcript in the app

Wire the Svelte transcript to the live session state. Streaming bubbles, tool panels, autoscroll with stick-to-bottom via `virtua`.

Commit: `feat(app): live streaming transcript`.

---

**PHASE 1c GATE.**

---

# Phase 1d — control plane

### Task 26: Control reverse-proxy with token custody

Proxy input / gates / interrupt / create / restore. Forward `Idempotency-Key` on create and restore. Inject the server-side bearer token; **strip any inbound `Authorization`** so a compromised SPA cannot smuggle credentials upstream. TLS `MinVersion: tls.VersionTLS12`, never `InsecureSkipVerify`.

Audit auth failures and denied gates by **route + sid + decision only** — never the body. Gate values and input blocks are PII-ish. Add a test asserting the audit log never contains a body byte.

Commit: `feat(bff): control-plane proxy with token custody and safe auditing`.

---

### Task 27: Fail-secure browse-only

`Host == nil` ⇒ control routes are **never registered**. Browse-only falls out of the type system rather than a runtime check that could fall through. Test that every control route 404s (not 403) when no host is configured.

Commit: `feat(bff): fail-secure browse-only mode`.

---

### Task 28: `sdk/core` control methods

`createSession`, `restoreSession`, `submit`, `respondGate`, `interrupt`, with typed errors and retry metadata. Gate ids are **opaque** — never parse or construct one. Add `ServeTransport` here so trusted server-side callers can target `pkg/serve` directly, and run the shared conformance suite against both transports.

Commit: `feat(sdk): control methods and direct serve transport`.

---

### Task 29: Composer and gate-approval UI

The interactive chat composer plus the gate prompt. Serve's status projection carries **one** `WaitingGateID`, so v1 surfaces at most one open gate; the `/gates` view is BFF-synthesized from status plus a journal fold, not a serve route. Multiple concurrent gates are explicitly out of scope.

The gate UI offers exactly the three responses harness defines: Approve / Approve always for this workspace / Deny.

Commit: `feat(app): composer and gate approval`.

---

### Task 30: Composition roots

**Files:** `cmd/looprig-client/main.go`, `cmd/looprig-client-local/main.go`

**Only `cmd/` imports a storage backend, and only in mounted-read mode.** `looprig-client-local` links `fsstore` only — the no-NATS laptop binary. `looprig-client` is the dual-mode convenience binary. The cloud/thin path proxies read and links **no** storage backend at all.

Explicit `http.Server` timeouts (Read/Write/Idle, `MaxHeaderBytes`). Loopback bind default; public bind opt-in and gated.

Commit: `feat(cmd): laptop and dual-mode composition roots`.

---

### Task 31: End-to-end integration pass

`//go:build integration` tests driving a real binary against a stub serve host: browse-only history, live tail with a mid-stream reconnect, a gate round-trip, and upstream-down producing a typed error rather than a hang.

```bash
go test -tags integration -race ./...
```

Commit: `test: end-to-end integration coverage`.

---

**PHASE 1d GATE — this is v1.**

---

# Beyond v1 (outline only — do not build without a fresh design pass)

- **Phase 2 — workspaces.** `pkg/workspacestore` now exists in harness (`ref.go`, `snapshot.go`, `archive.go`, `extract.go`), so this gate is open. Add a workspaces/snapshots view listing `WorkspaceCheckpointed` refs from the journal.
- **Phase 3 — more framework adapters.** `@looprig/react` first if demand is real, then Vue/Angular/Solid. Each is a small wrapper over `sdk/core` and **must pass the same fixture-driven conformance suite**; none may implement its own transport.
- **Phase 5 — desktop/mobile.** Wails v3 (Decision #19, superseding Tauri v2). Because Wails' backend is Go, the shell links `internal/bff` in-process: one binary, no sidecar, no Rust toolchain, no loopback listener in the packaged app. **Re-verify Wails v3's release channel and mobile maturity before starting** — that decision was taken from documentation, not from shipping it.

## Known future work, recorded not assumed

- **v1 is single-host.** The BFF proxies live/control to one configured host URL. A fleet of scale-to-zero sandboxes (one host per session) needs a session→host routing map that nothing owns yet.
- **Multiple concurrent gates** (parallel tools/subagents) need an O(journal) fold or a new serve open-gates list.
- **No `DELETE` in v1.** Serve ships no destroy endpoint; the UI's "stop" maps to interrupt. Store deletion stays retention/GC policy.
- **`EventReplayer.Follow` is cold-replay only**, so the live tail comes from session events, not journal follow. Host-independent store-follow is future work.
