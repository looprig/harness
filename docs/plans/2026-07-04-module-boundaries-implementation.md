# looprig Module Boundaries — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Split `github.com/looprig/harness` (and adjacent modules) into the ownership-scoped modules `core`, `inference`, `llm`, `harness`, and `storage` (rename of `storekit`), per `docs/plans/2026-07-04-module-boundaries-design.md`, without regressing any test.

**Architecture:** Bottom-up extraction. Move stdlib-only primitives into `core`; rename `storekit` → `storage`; carve the provider-neutral half of `pkg/llm` into a standalone `inference` module and the provider batteries into `llm`; repoint `harness` at `inference` (never `llm`); update consumers (`flow`, `cli`, `swe`). Most moves are mechanical (`git mv` + import rewrite + `go mod tidy`); the `inference`/`llm` carve is the one genuinely semantic phase and is done test-first.

**Tech Stack:** Go (multi-module: leaf modules `go 1.25.0`, harness `go 1.26.4`), separate git repos per module, `replace` directives + selective vendoring for local wiring, `make`/`go test -race` gates. No new external dependencies.

---

## Ground truth (verified against the working tree — read before starting)

These repo-specific facts are the reason several tasks below are not "just `git mv`". Confirm each still holds before Phase 0; if any drifted, stop and re-plan.

1. **Each module dir is its own git repo.** `/Users/ipotter/code/looprig` is **not** a git repo — it is an umbrella working dir holding sibling repos (`harness/.git`, `flow/.git`, `cli/.git`, `swe/.git`, `storekit/.git`, `fsstore/.git`, `natsstore/.git`, `rclonestore/.git`). So "create module `core`" = create a **new repo** `core/` with its own `.git`. Tags are per-repo.
2. **There is no `go.work` anywhere.** Cross-module wiring today is `replace` directives + vendoring. Some replaces are **stale** relative to this checkout (`harness → ../ciram-co/storekit`, `cli → ../looprig`, `swe → ../looprig-console`) and only work because the consumers that need them **vendor** their deps. Phase 0 fixes this.
3. **Vendoring is mixed:** `harness` vendors `storekit`+`fsstore`; `cli` vendors `harness`; `flow` vendors (no looprig deps); `swe`, `storekit`, `fsstore`, `natsstore`, `rclonestore` do **not** vendor. Any phase that adds a new-module dependency to a **vendoring** consumer (`harness`, `cli`) must run `go mod vendor` after `go mod tidy`.
4. **Two Makefile templates.** Leaf (`storekit/Makefile`): `test`=`GOWORK=off go test -race ./...`, `fmt-check`=`gofmt -l .`, `vet`, `check: fmt-check vet test`; `go.mod` is module line + `go 1.25.0`, no requires, no `tool`. Heavy (`harness/Makefile`): `GO_DIRS := $(shell go list -f '{{.Dir}}' ./...)`, `fmt`/`fmt-check`/`lint`(vet+staticcheck+gosec)/`vuln`(mod verify+govulncheck)/`secure`, `export GOFLAGS := -mod=vendor`, `tool (...)` block. `core`/`storage` use the leaf template; `inference`/`llm` use the heavy template (minus `-mod=vendor` unless they vendor).
5. **Baseline is green via vendor.** `GOWORK=off go build ./...` passes in `storekit` and `harness` today. Establish and record a green baseline per module before touching anything (Phase 0, Task 0.2).
6. **The `pkg/llm` neutral boundary is clean on the harness-core side.** `pkg/loop` and `pkg/session` import **only** the neutral root `pkg/llm` package (types like `Model`, `LLM`, `Request`), never `transport`/`codec/*`/`providers/*`. The only harness-core→llm edge to cut is `loop`/`session` → root `pkg/llm`. `swe` additionally imports provider-specific `auth`+`auto`.

### Neutral vs provider classification of `pkg/llm` (from the code map)

**→ `inference` (provider-neutral):** root `pkg/llm` minus the `Provider` enum — `codec.go`, `authenticator.go`, `stream.go`, `capabilities.go`, `effort.go`, `origin.go`, `requestmode.go`, `sampling.go`, `apiformat.go` (type only), `model.go` (struct + `validateHTTPBaseURL`, minus provider-policy calls), `errors.go` (`NetworkError`, `APIError`, `ValidationError`, `ModelMismatchError`, `AuthKind` type); `transport/*`; `codec/openaiapi`, `codec/anthropicapi`, `codec/gemini`, `codec/sse`; generic half of `auth/auth.go` (`Key`, `Header`, `None`, `APIKey`, `headerAuth`).

**→ `llm` (provider batteries):** `provider.go` (all four policy methods) + the `Provider` enum + provider-named `APIFormat` values + `AttestationError`/`AuthRequiredError`/`AuthSigV4` from `errors.go`; `auto/*`; `auth/sigv4.go` + `SigV4Credentials`; `providers/*` (bedrock, chutes, gemini, phala); `aci/*`; `e2e/*`; `tee/*`.

**Three coupling breaks the carve must make (each has its own task in Phase 5):**
- `transport.Endpoint`/`transport.Client` embed `llm.Provider` and hardcode the OpenAI `/chat/completions` path → replace with an injected `Router`.
- `model.Validate()` calls `Provider.RequiredAuth()`/`supportsAPIFormat()`/`allowsEmptyBaseURL()` → make `inference.Model.Validate` structural-only; provider policy moves to `llm`.
- `errors.go` mixes neutral transport errors with the TEE `AttestationError` and SigV4-aware `AuthKind` → split the file.

---

## Reusable recipes (referenced by task steps below — DRY)

**RECIPE-NEWMODULE `<name>` `<goversion>` `<template>`:**
1. `mkdir /Users/ipotter/code/looprig/<name> && cd /Users/ipotter/code/looprig/<name> && git init`
2. `go mod init github.com/looprig/<name>` then set `go <goversion>`.
3. Copy the chosen Makefile template (leaf or heavy) and a `.gitignore` (copy `storekit/.gitignore`).
4. For the heavy template only: copy the `tool (...)` block from `harness/go.mod` and run `go mod tidy` to pull the tool deps.

**RECIPE-WIRE `<consumer>` `<newmodule>`** (make an untagged new module resolvable from a consumer):
1. In `<consumer>/go.mod`: add `require github.com/looprig/<newmodule> v0.0.0` and `replace github.com/looprig/<newmodule> => ../<newmodule>`.
2. `cd <consumer> && GOWORK=off go mod tidy`.
3. **If `<consumer>` vendors** (`harness`, `cli`): `GOWORK=off go mod vendor`.

**RECIPE-REWRITE `<oldimport>` `<newimport>` `<moduledir>`** (rewrite every import site in a module):
1. `cd /Users/ipotter/code/looprig/<moduledir>`
2. `rg -l '<oldimport>' --glob '!vendor' | xargs perl -pi -e 's{\Q<oldimport>\E}{<newimport>}g'`
3. `gofmt -w $(git diff --name-only -- '*.go')`
4. Handle aliased/single-line imports (`import "..."`) — `perl` catches both since it matches the path string, not the line shape.

**RECIPE-GATE `<moduledir>`** (per-phase verification, run in the module that changed):
1. `cd /Users/ipotter/code/looprig/<moduledir>`
2. `GOWORK=off go build ./... && GOWORK=off go test -race ./...`
3. `GOWORK=off go mod tidy` (must produce no unexpected diff)
4. Heavy modules only: `make lint && make secure`.
5. Vendoring modules only: confirm `go mod vendor` left `vendor/modules.txt` consistent (`git diff --stat vendor/`).

---

## Phase 0 — Local multi-module dev baseline (prerequisite)

**Why:** The design's per-phase gate is `GOWORK=off go test`, but cross-module builds currently rely on stale replaces that only vendored consumers survive. New untagged modules must resolve locally. Establish a coherent, recorded baseline first.

### Task 0.1: Create the umbrella dev workspace

**Files:**
- Create: `/Users/ipotter/code/looprig/go.work` (untracked — the umbrella dir is not a repo)

**Step 1:** `cd /Users/ipotter/code/looprig && go work init ./harness ./flow ./cli ./swe ./storekit ./fsstore ./natsstore ./rclonestore`

**Step 2:** Confirm `go work sync` succeeds. Note in the file's top comment that `go.work` is for editor/navigation only and **all verification runs `GOWORK=off`** so each module proves it resolves through its own `require`/`replace` graph (matching every Makefile).

**Step 3:** Do **not** commit anything (no repo owns this file). This task is environment setup, not a code change.

### Task 0.2: Record the green baseline

**Step 1:** For each module dir run `RECIPE-GATE <dir>` (build + `-race` test). Capture pass/fail per module into `/private/tmp/.../scratchpad/baseline.txt`.

**Step 2:** If any module is red at baseline, STOP — fix or document the pre-existing failure before migrating, so later phase gates have a trustworthy comparison point.

**Step 3:** Confirm the stale replaces are the only cross-module surprise: `rg -n 'replace github.com/looprig' */go.mod`. Leave them for now; each is corrected in the phase that touches its target.

**Commit:** none (no source changed).

---

## Phase 1 — Extract `core/uuid`

**Decision (verified):** `core/uuid` is **flow's** implementation verbatim (has `Parse`/`MustParse` + `errors.Is`-able `errInvalidText` sentinel + single parse path). No `harness`/`cli`/`swe` caller references the `.Cause`/`.Err` error fields, so repointing imports is safe; only field-name-constructing code would break, and there is none outside flow.

### Task 1.1: Stand up the `core` module with `uuid`

**Files:**
- Create module: `github.com/looprig/core` (RECIPE-NEWMODULE `core` `1.25.0` leaf)
- Create: `core/uuid/uuid.go`, `core/uuid/doc.go`, `core/uuid/uuid_test.go`

**Step 1:** RECIPE-NEWMODULE `core` `1.25.0` leaf.

**Step 2:** Copy flow's files into place:
`cp flow/pkg/uuid/uuid.go flow/pkg/uuid/doc.go flow/pkg/uuid/uuid_test.go core/uuid/`
(Use flow's, not harness's — flow's is the stricter parser the design retains.)

**Step 3:** In `core/uuid/doc.go`, drop the flow-design-doc reference in the package comment; keep it generic.

**Step 4:** RECIPE-GATE `core` — `go test -race ./...` must pass (the copied test is self-contained, stdlib-only).

**Step 5:** `cd core && git add -A && git commit -m "feat(uuid): stdlib-only v4 UUID with strict Parse/MustParse"` (see message footer convention at end of plan).

### Task 1.2: Repoint `flow` at `core/uuid`, delete `flow/pkg/uuid`

**Step 1:** RECIPE-WIRE `flow` `core` (flow does not vendor → require + replace + tidy only).

**Step 2:** RECIPE-REWRITE `github.com/looprig/flow/pkg/uuid` `github.com/looprig/core/uuid` `flow`. (12 sites: `flow/pkg/flow/*.go`, `flow/cmd/*/main.go`, and the nested `flow/pkg/nats` submodule — the submodule needs its own RECIPE-WIRE + tidy because it is a separate module `github.com/looprig/flow/pkg/nats`.)

**Step 3:** `git rm -r flow/pkg/uuid`.

**Step 4:** RECIPE-GATE `flow` and RECIPE-GATE `flow/pkg/nats`.

**Step 5:** Commit in `flow` (and `flow/pkg/nats` if it changed).

### Task 1.3: Repoint `harness`, `cli`, `swe` at `core/uuid`, delete `harness/pkg/uuid`

**Step 1:** For each of `harness`, `cli`, `swe`: RECIPE-WIRE `<consumer>` `core`. (`harness` and `cli` vendor → include `go mod vendor`; `swe` does not.)

**Step 2:** RECIPE-REWRITE `github.com/looprig/core/uuid` `github.com/looprig/core/uuid` for each of `harness`, `cli`, `swe`.

**Step 3:** `git rm -r harness/pkg/uuid`.

**Step 4:** RECIPE-GATE for `harness`, `cli`, `swe` each. Harness has the largest surface (~100 sites across `pkg/{api,command,event,foreignloop,hub,identity,journal,loop,session,sessionstore,tool,tools,transcript}`) — the perl rewrite handles all; the gate's `-race` test is the safety net.

**Step 5:** Commit per repo.

**Phase 1 gate:** design verification "`flow` imports `core/uuid`, not a private or duplicated UUID package" — confirm with `rg -n 'pkg/uuid' flow harness cli swe --glob '!vendor'` returning nothing.

---

## Phase 2 — Extract `core/content` (+ `streamaccumulator`)

`content` is the shared vocabulary of nearly every module (harness loop/session/codecs, cli tui, swe). `streamaccumulator` has only 2 importers.

### Task 2.1: Move `content` and `streamaccumulator` into `core`

**Files:**
- Create: `core/content/*` (from `harness/pkg/content/*`), `core/content/streamaccumulator/*`

**Step 1:** `cp -R harness/pkg/content/* core/content/` (this brings `streamaccumulator/` along as `core/content/streamaccumulator/`).

**Step 2:** Rewrite intra-package self-imports inside the copied tree: RECIPE-REWRITE `github.com/looprig/harness/pkg/content` `github.com/looprig/core/content` `core` (catches `streamaccumulator.go`'s import of `content`).

**Step 3:** RECIPE-GATE `core` (content is stdlib-only; `streamaccumulator` imports only stdlib + `core/content`). Confirm `core` stays stdlib-only: `go list -deps ./content/... | rg -v '^github.com/looprig/core' | rg looprig` must be empty.

**Step 4:** Commit in `core`.

### Task 2.2: Repoint all consumers, delete `harness/pkg/content`

**Step 1:** `harness` and `cli` already require `core` (Phase 1) — `swe` too. No new RECIPE-WIRE needed except confirming `core` version pin is current; re-vendor harness/cli after the rewrite.

**Step 2:** RECIPE-REWRITE `github.com/looprig/harness/pkg/content/streamaccumulator` `github.com/looprig/core/content/streamaccumulator` **first** (more specific path), then RECIPE-REWRITE `github.com/looprig/harness/pkg/content` `github.com/looprig/core/content` — for each of `harness`, `cli`, `swe`. Order matters so the longer path isn't partially rewritten by the shorter one.

**Step 3:** `git rm -r harness/pkg/content`.

**Step 4:** For `harness`, `cli`: `go mod tidy` + `go mod vendor`. RECIPE-GATE `harness`, `cli`, `swe`.

**Step 5:** Note: `harness/pkg/llm/codec/*` and `transport/*` import `content` — they get rewritten here too, which is correct; they move to `inference` in Phase 5 already pointing at `core/content`.

**Step 6:** Commit per repo.

**Phase 2 gate:** `rg -n 'harness/pkg/content' --glob '!vendor'` returns nothing across all modules.

---

## Phase 3 — Extract `core/logging`

`cli/internal/logging` has exactly **one** importer (`cli/cli/run.go:22`). Trivial move; the design keeps it narrow (parse-level + JSON logger only).

### Task 3.1: Move logging into `core`, repoint cli

**Files:**
- Create: `core/logging/logging.go`, `core/logging/logging_test.go` (from `cli/internal/logging/*`)

**Step 1:** `cp cli/internal/logging/logging.go cli/internal/logging/logging_test.go core/logging/` and set `package logging`.

**Step 2:** RECIPE-GATE `core` (must remain stdlib-only — logging uses only `log/slog`).

**Step 3:** In `cli`: RECIPE-REWRITE `github.com/looprig/cli/internal/logging` `github.com/looprig/core/logging` `cli`; `git rm -r cli/internal/logging`; `go mod tidy` + `go mod vendor`.

**Step 4:** RECIPE-GATE `cli`. Commit in `core` and `cli`.

**Phase 3 gate:** `core` still stdlib-only (re-run the `go list -deps` check from Task 2.1 across all of `core`).

---

## Phase 4 — Rename `storekit` → `storage`

`storekit` is its own repo. Rename means: new module path `github.com/looprig/storage`, then repoint 4 dependents (`harness`, `fsstore`, `natsstore`, `rclonestore`) that import root + `memstore` + `storetest`.

### Task 4.1: Rename the module in place

**Step 1:** `cd storekit && git mv` is not needed for the module path itself — edit `go.mod` module line to `github.com/looprig/storage`. Decide repo dir: rename `storekit/` → `storage/` (`mv storekit storage`; the `.git` moves with it). Update the umbrella `go.work` path.

**Step 2:** RECIPE-REWRITE `github.com/looprig/storekit` `github.com/looprig/storage` `storage` (rewrites intra-module imports in `memstore/*`, `storetest/*`).

**Step 3:** Update `storage/CLAUDE.md`, `README.md` name references. Keep package names (`memstore`, `storetest`) — only the module path changes; the design does **not** rename `storage`'s subpackages.

**Step 4:** RECIPE-GATE `storage`. Commit in the `storage` repo.

### Task 4.2: Repoint the three backend stores

**Step 1:** For each of `fsstore`, `natsstore`, `rclonestore`: edit `go.mod` `require` + fix the `replace` to `=> ../storage`; RECIPE-REWRITE `github.com/looprig/storekit` `github.com/looprig/storage` `<store>`; `go mod tidy`.

**Step 2:** RECIPE-GATE each (natsstore/rclonestore conformance tests are `//go:build integration` — run `go build ./...` + unit tests; note the integration suites in the commit message as not run locally if no NATS/rclone backend).

**Step 3:** Commit per repo.

### Task 4.3: Repoint `harness` (root + `memstore`) and fix its stale replace

**Step 1:** In `harness/go.mod`: replace the `storekit` require/replace with `storage`, and **fix** the stale path (`../ciram-co/storekit` → `../storage`). Also fix the stale `fsstore` replace (`../ciram-co/fsstore` → `../fsstore`) while here.

**Step 2:** RECIPE-REWRITE `github.com/looprig/storekit` `github.com/looprig/storage` `harness` (sites in `pkg/sessionstore/*`, `pkg/workspacestore/*`, `pkg/session/*_test.go` — root + `memstore`).

**Step 3:** `go mod tidy` + `go mod vendor` (harness vendors `storekit` today → the vendored dir becomes `vendor/github.com/looprig/storage`).

**Step 4:** RECIPE-GATE `harness`. Commit.

**Phase 4 gate:** design checks "storage backends import `storage` only, not `harness` or `flow`" — `rg -n 'looprig/(harness|flow)' fsstore natsstore rclonestore --glob '!vendor'` returns nothing; and `rg -n 'looprig/storekit' --glob '!vendor'` is empty everywhere.

---

## Phase 5 — Extract `inference` (the semantic carve — test-first)

This is the only non-mechanical phase. Build the `inference` module by moving neutral code out of `pkg/llm` **and** making the three coupling breaks. Provider code stays in `harness/pkg/llm` temporarily and moves to the `llm` module in Phase 6. Work test-first for each new contract.

### Task 5.1: Scaffold `inference`, move neutral leaf types

**Step 1:** RECIPE-NEWMODULE `inference` `1.26.4` heavy (it will grow lint/security gates; match harness's Go version since it shares `core/content` semantics). RECIPE-WIRE `inference` `core`.

**Step 2:** `git mv` (within harness first, to preserve history, then `cp` across repos — the modules are separate repos, so use `cp` + note provenance in the commit) the neutral leaf files from `harness/pkg/llm/` into `inference/`: `authenticator.go`, `stream.go`, `capabilities.go`, `effort.go`, `origin.go`, `requestmode.go`, `sampling.go`, and their `_test.go` siblings. Set `package inference`.

**Step 3:** Split `llm.go`: the `LLM` interface + `Request`/`Response`/`Tool`/`Usage` value types move to `inference` (rename interface to `inference.Client` per design, keeping `LLM` as an alias in `llm` for Phase 6 compatibility). The `Provider` enum + its constants **stay** in `harness/pkg/llm` for now (moves to `llm` module in Phase 6).

**Step 4:** RECIPE-GATE `inference` on what's moved so far. Commit.

### Task 5.2: Split `errors.go`

**Step 1:** Move `NetworkError`, `APIError`, `ValidationError`, `ModelMismatchError`, and the `AuthKind` **type** to `inference/errors.go`. Add the generic `MissingCredentialsError` the design names (describes missing header/credential without a provider table).

**Step 2:** Leave `AttestationError`, `AuthRequiredError`, and the `AuthSigV4` constant in `harness/pkg/llm` (→ `llm` module Phase 6).

**Step 3:** Write/port table-driven tests for each moved error's `Error()`/`Unwrap()` into `inference`. RECIPE-GATE `inference`. Commit.

### Task 5.3: `inference.Model` structural validation (TDD)

**Files:** Create `inference/model.go`, `inference/model_test.go`.

**Step 1 (failing test):** Port `harness/pkg/llm/model_test.go`, then **add** cases asserting structural-only behavior: unknown `Provider` label accepts; unknown `APIFormat` accepts; empty model endpoint accepts (wildcard); a syntactically unsafe base URL (userinfo, non-loopback http) rejects with `*inference.ValidationError`; empty model name rejects. Remove/````t.Skip```` the old cases that asserted provider-policy rejection.

**Step 2:** Run — fails to compile (`Provider.RequiredAuth` etc. absent in `inference`).

**Step 3 (impl):** Port `Model` struct + `validateHTTPBaseURL` (keep the https-or-loopback + no-userinfo checks). `Model.Validate` calls only `validateHTTPBaseURL` + name-present + request-local field checks. Drop all `Provider.RequiredAuth()/supportsAPIFormat()/allowsEmptyBaseURL()` calls. Keep `ModelOption`/`CustomModel`/`With*` builders.

**Step 4:** Run — green.

**Step 5:** Commit.

### Task 5.4: `APIFormat` open-ended (TDD)

**Step 1 (failing test):** In `inference/apiformat_test.go`, assert `APIFormat` is an open string label: an unknown value is **not** rejected by any `inference`-level gate (no `Valid()` that fails-closed). Keep built-in constant names (`APIFormatOpenAI`, `APIFormatAnthropic`, `APIFormatGemini`) as convenience labels.

**Step 2:** Impl `inference/apiformat.go` with the type + built-in constants but no fail-closed `Valid()`; the `APIFormatBedrockConverse` and provider-named validation stay out of `inference` (Phase 6 decides Bedrock).

**Step 3:** Commit.

### Task 5.5: `Router` seam replacing `ChatPath` (TDD)

**Files:** Create `inference/route.go` (interface + `Route`), `inference/route/*` (builders), tests.

**Step 1 (failing test):** In `inference/route/route_test.go`, table-drive two builders against `httptest`-style expectations:
- static chat route → `POST {base}/chat/completions` for both invoke and stream modes;
- Gemini model-in-path → invoke `POST {base}/models/{model}:generateContent`, stream `POST {base}/models/{model}:streamGenerateContent?alt=sse`.
Assert the returned `Route{Method, URL, Header}`.

**Step 2:** Define in `inference/route.go`:
```go
type Route struct {
	Method string
	URL    string
	Header http.Header
}

type Router interface {
	BuildRoute(baseURL string, req Request, mode RequestMode) (Route, error)
}
```
Implement `route.StaticChat("/chat/completions")` and `route.GeminiGenerateContent()` in `inference/route`. Expose a custom-router seam (callers pass any `Router`).

**Step 3:** Run — green. Commit.

### Task 5.6: `wire/*` framing — real SSE, NDJSON, JSON body (TDD)

**Files:** Create `inference/wire/sse/*`, `inference/wire/ndjson/*`, `inference/wire/jsonbody/*`, tests.

**Step 1 (failing test) — `wire/sse`:** Replace the OpenAI-only `data: `-stripper. Table cases: multi-line `data:` fields joined with `\n`; `:`-comment lines ignored; `event:` name preserved on the frame; **no** `[DONE]` handling (that's the semantic decoder's job — assert `[DONE]` is returned as an ordinary frame, not swallowed). Frame type is `inference.StreamFrame{Name, Metadata, Data}`.

**Step 2:** Implement the real SSE event framer per RFC (accumulate `data:` fields, dispatch on blank line, carry `event:`). Implement `wire/ndjson` (one frame per decoded line) and `wire/jsonbody` (JSON encode/decode helpers).

**Step 3:** Run — green. Commit.

### Task 5.7: Codec split — `RequestEncoder`/`ResponseDecoder`/`StreamDecoder` (TDD)

**Files:** Create `inference/codec.go` (root interfaces + `EncodedRequest`/`StreamFrame`), tests.

**Step 1 (failing test):** Assert the interface shapes compile and that `Codec` does **not** embed `StreamDecoder`:
```go
type EncodedRequest struct { Header http.Header; Body io.Reader }
type RequestEncoder interface { EncodeRequest(req Request, mode RequestMode) (EncodedRequest, error) }
type ResponseDecoder interface { DecodeResponse(body []byte) (*Response, error) }
type StreamFrame struct { Name string; Metadata map[string]string; Data []byte }
type StreamFramer interface { DecodeStreamFrames(body io.ReadCloser) (*StreamReader[StreamFrame], error) }
type StreamDecoder interface { DecodeStream(resp *http.Response) (*StreamReader[content.Chunk], error) }
type Codec interface { RequestEncoder; ResponseDecoder }
type StreamingCodec interface { Codec; StreamDecoder }
```
Include a compile-time `var _ Codec = nonStreamingFake{}` proving a non-streaming codec need **not** implement `DecodeStream`.

**Step 2:** Define the interfaces in `inference/codec.go`.

**Step 3:** Run — green. Commit.

### Task 5.8: Move the semantic codecs → `inference/codec/*` (TDD-preserving)

**Step 1:** `cp -R` `harness/pkg/llm/codec/openaiapi` → `inference/codec/openaiapi`, `anthropicapi` → `inference/codec/anthropicapi`, `gemini` → `inference/codec/geminiapi` (rename package dir `gemini`→`geminiapi` per design). Rewrite their imports: `harness/pkg/llm` → `inference`, `harness/pkg/content` → `core/content`, `harness/pkg/llm/codec/sse` → `inference/wire/sse`.

**Step 2:** Adapt each codec to the new split: `EncodeRequest` returns `EncodedRequest` (header+body) instead of `[]byte`; add a `DecodeStream` on each (making them `StreamingCodec`) that composes `wire/sse` + the existing `decodeEvent`. The OpenAI/Anthropic/Gemini decoders own their terminal-sentinel decisions (e.g. OpenAI's `[DONE]`).

**Step 3:** Port each codec's test package. RECIPE-GATE `inference` (codecs' fuzz tests included). Commit.

### Task 5.9: Split `auth` — generic → `inference/auth` (TDD)

**Step 1:** `cp` the generic helpers from `harness/pkg/llm/auth/auth.go` (`APIKey`, `headerAuth`, `Key`, `Header`, `None`, `noneAuth`) into `inference/auth/auth.go`; rewrite import `harness/pkg/llm` → `inference`. Leave `SigV4Credentials` + `sigv4.go` in `harness/pkg/llm/auth` (→ `llm` Phase 6).

**Step 2:** Define `inference.Authenticator` as the root interface (moved in 5.1). Port `auth_test.go` for the generic helpers. RECIPE-GATE `inference`. Commit.

### Task 5.10: Transport — Router-driven, streaming ownership, header precedence (TDD)

This replaces `transport/endpoint.go`'s `DefaultChatPath` and `transport/client.go`'s hardcoded POST/JSON/SSE/`Accept`.

**Files:** Create `inference/transport/*`, tests using `httptest`.

**Step 1 (failing tests):** Table-drive against `httptest.Server`:
- **Routing:** transport calls the injected `Router` for method+URL — no hardcoded `/chat/completions`. An OpenAI static router and a Gemini model-in-path router both work through the same transport.
- **Header precedence:** a case where route, encoder, and auth all set the same header proves order route→encoder→auth with later overriding earlier.
- **Non-2xx before decode:** a 500 with a JSON error body maps to `*inference.APIError` **before** `ResponseDecoder`/`StreamDecoder` is called, for **both** invoke and stream; the error body is drained and closed by the transport.
- **Optional streaming:** a `Codec` with no `StreamDecoder`, called in stream mode, fails **before I/O** with a typed unsupported-streaming error.
- **No replay:** transport never retries/replays `EncodedRequest.Body` (assert the body reader is consumed exactly once).
- **Binding mismatch:** a request Model with a conflicting non-empty endpoint/provider/API-format vs the bound `Endpoint` fails closed with `*inference.ModelMismatchError`; empty fields are wildcards.
- **Custom API end-to-end:** a caller-supplied encoder+decoder+router against a custom `httptest` route works with no `inference` change; and a non-SSE stream (NDJSON or custom framer) streams through.

**Step 2 (impl):** Port `transport.Client` to: build request via `Router.BuildRoute`; apply headers route→encoder→auth; execute; map non-2xx→`APIError` (drain+close); on success invoke `ResponseDecoder` (invoke) or `StreamDecoder` (stream, requiring the codec implement it else typed error). `Endpoint` becomes `{BaseURL string, Provider ProviderName /*opaque label*/, APIFormat APIFormat}` — no `ChatPath`, no dependence on the provider policy enum. `StreamDecoder`/`StreamFramer` own closing the body on early error.

**Step 3:** Run — green. Commit.

**Phase 5 gate (design cross-module smoke checks):**
- `inference` calls httptest OpenAI/Anthropic/Gemini routes using only stdlib + `core`.
- `inference` calls a custom httptest API via caller-supplied encoder/decoder/router.
- `inference` streams from a non-SSE custom API without transport changes.
- unknown `APIFormat` accepted when explicit decoders+router supplied.
- typed generic errors preserved (network/API/validation/missing-creds/mismatch).
- `inference` has **no** provider default endpoints, auth-policy table, or model catalogue: `rg -n 'chutes|phala|bedrock|openrouter|generativelanguage|SigV4|Attestation' inference --glob '!*_test.go'` returns nothing.

---

## Phase 6 — Build the `llm` provider batteries module

Move everything provider-specific out of `harness/pkg/llm` into a new `llm` module that depends on `core` + `inference`.

### Task 6.1: Scaffold `llm`, move provider policy + enums

**Step 1:** RECIPE-NEWMODULE `llm` `1.26.4` heavy; RECIPE-WIRE `llm` `core` and RECIPE-WIRE `llm` `inference`.

**Step 2:** Move into `llm`: `provider.go` (all four policy methods) + the `Provider` enum/constants + `apiformat.go`'s provider-named values (`APIFormatBedrockConverse`) + `AttestationError`/`AuthRequiredError`/`AuthSigV4` from the old `errors.go` + `auth/sigv4.go` + `SigV4Credentials` (→ `llm/auth`). Provider policy fail-closed validation (known provider/API-format pairs, `RequiredAuth`) now lives here as presets that wrap `inference.Model`.

**Step 3:** RECIPE-GATE `llm`. Commit.

### Task 6.2: Move providers, attestation, composition root

**Step 1:** `cp -R` `providers/{bedrock,chutes,gemini,phala}`, `aci`, `e2e`, `tee`, `auto` into `llm/`. Rewrite imports: `harness/pkg/llm` → `inference` (for neutral types) or `llm` (for `Provider`/policy); `harness/pkg/llm/codec/*` → `inference/codec/*`; `harness/pkg/llm/auth` generic → `inference/auth`, SigV4 → `llm/auth`; `harness/pkg/content` → `core/content`.

**Step 2:** `llm/auto` (composition root) keeps default base URLs + provider→codec/auth wiring, now constructing `inference.Client` via `inference/transport` + `inference/route` + `inference/codec/*`. Bedrock's `eventstream` (if implemented) lives at `llm/providers/bedrock/eventstream`.

**Step 3:** Enforce the design's "no SDK-owned model catalogue": `rg -n 'LMStudioLocal|ChutesKimiK2|Kimi|GPT-|claude-' llm --glob '!*_test.go'` — curated rows must **not** appear; if found, they move to `swe` in Phase 8.

**Step 4:** RECIPE-GATE `llm` (incl. `make secure` — `llm` carries go-tdx-guest/secp256k1/x/crypto). Commit.

**Phase 6 gate:** `llm` imports `inference`, not the reverse (`rg -n 'looprig/inference' llm` present; `rg -n 'looprig/llm' inference` empty). `go list -deps` on `inference` shows no `looprig/llm`.

---

## Phase 7 — Repoint `harness` at `inference`; drop `llm`

Now `harness/pkg/llm` is empty of both neutral (→`inference`) and provider (→`llm`) code. Cut the harness-core edge.

### Task 7.1: Repoint `pkg/loop` and `pkg/session` to `inference`

**Step 1:** RECIPE-WIRE `harness` `inference`. (harness vendors → include `go mod vendor`.)

**Step 2:** RECIPE-REWRITE `github.com/looprig/harness/pkg/llm` `github.com/looprig/inference` `harness` — the sites are `pkg/loop/{config,step,turn,fake_test,...}.go` and `pkg/session/*_test.go` (all reference root types `Model`/`LLM`/`Request` now in `inference`). Adjust the interface name if `LLM`→`Client` (rename the harness-side references).

**Step 3:** `git rm -r harness/pkg/llm` (now empty). Remove the `llm`-related requires from `harness/go.mod`; `go mod tidy` + `go mod vendor`.

**Step 4:** RECIPE-GATE `harness`.

**Step 5:** **Assert independence:** `go list -deps ./... | rg 'looprig/llm'` in `harness` returns **nothing** (design's headline invariant: `harness` builds without importing `github.com/looprig/llm`). Also confirm harness no longer vendors go-tdx-guest/secp256k1 unless another path needs them: `ls harness/vendor/github.com/google/go-tdx-guest 2>/dev/null` should be gone.

**Step 6:** Handle the test-only `fsstore` integration import (design's noted exception): confirm it's still behind `//go:build integration` and its replace path is fixed (Phase 4). Commit.

**Phase 7 gate:** design checks "`harness` builds without importing `github.com/looprig/llm`" and "`harness + inference` can run a fake/local HTTP inference client end-to-end" — add a small `harness` integration test (or reuse an existing loop test) that drives a loop against an `httptest` OpenAI-compatible server via `inference`.

---

## Phase 8 — Update consumers `flow`, `cli`, `swe`

`flow` and `cli` are largely done (Phases 1–3). `swe` is the real work: it spans `core` + `harness` + `inference` + `llm` + storage backends.

### Task 8.1: `swe` — repoint neutral vs provider imports

**Step 1:** RECIPE-WIRE `swe` `inference` and RECIPE-WIRE `swe` `llm` (swe does not vendor → require+replace+tidy). Confirm `swe` already requires `core`, `harness`.

**Step 2:** Rewrite `swe/swarms/swe/*` imports by symbol class:
- neutral types (`Model`, `LLM`/`Client`, `Request`, `Response`, `StreamReader`, `NewStreamReader`) → `inference`;
- provider policy (`Provider`, `ProviderLMStudio`, `ProviderChutes`) → `llm`;
- provider factory (`auth`, `auto`) → `llm/auth`, `llm/auto`.
Use targeted RECIPE-REWRITE per old path; where a single file mixes both, split the import block by hand.

**Step 3:** **Relocate residual catalogue constants.** `swe` references `llm.LMStudioLocal` and `llm.ChutesKimiK2` (curated model rows). Per the mechanism-vs-policy rule these belong in `swe`, not `llm`. Move their definitions into `swe/swarms/swe/model_catalog.go` (swe already owns `model_catalog.go`) and drop them from `llm`. Update references to the swe-local names.

**Step 4:** RECIPE-GATE `swe` (incl. `make secure` if swe has it; note any `//go:build integration` suites not run).

**Step 5:** Commit.

### Task 8.2: `cli` and `flow` final gate

**Step 1:** Confirm `cli` compiles against `core` (uuid/content/logging) + `harness`; it does not import `inference`/`llm` directly. RECIPE-GATE `cli`.

**Step 2:** `flow` depends only on `core` (uuid, maybe logging). RECIPE-GATE `flow`.

**Step 3:** Commit any remaining stragglers.

---

## Final cross-cutting verification (design's full gate list)

Run all, record results:

1. `harness` builds without `github.com/looprig/llm` (Phase 7 assertion).
2. `harness + inference` runs a fake/local HTTP client end-to-end.
3. `inference` calls httptest OpenAI/Anthropic/Gemini routes with only stdlib + `core`.
4. `inference` calls a custom httptest API via caller-supplied encoder/decoder/router.
5. `inference` streams a non-SSE custom API (NDJSON/custom framer) without transport change.
6. `inference` invoke works with a codec lacking `StreamDecoder`; stream without one fails pre-I/O with a typed error.
7. `inference` maps non-2xx streaming to `APIError` before decoding; drains/closes error bodies; decoders/framers close bodies on early error.
8. `inference` never retries/replays `EncodedRequest.Body`.
9. Header merge order proven route→encoder→auth (later overrides earlier).
10. Unknown/custom `APIFormat` accepted with explicit decoders+router; `llm` still fails closed for unknown provider/format presets.
11. Typed generic errors preserved (network/API/validation/missing-creds/mismatch).
12. `inference` structural model validation accepts unknown provider/API-format labels; `llm` presets fail closed.
13. `llm` imports `inference`, not the reverse.
14. `inference` has no provider default endpoints, auth-policy table, or model catalogue.
15. `llm` has no SDK-owned model catalogue (rows live in `swe`).
16. `flow` imports `core/uuid`, not a duplicated UUID package.
17. storage backends import `storage` only, not `harness`/`flow`.
18. Forbidden edges absent: `harness→llm`, `inference→harness`, `core→inference/harness`, `storage→harness/flow`, `backend stores→harness/flow` — verify each with `go list -deps`.

---

## Release sequencing (after all gates green)

Tag bottom-up, per the design's Release Sequencing, each in its own repo:
1. `core`
2. `storage` + `inference`
3. `llm` + `harness`
4. apps (`flow`, `cli`, `swe`)

For each tagged tier, drop the local `replace` in downstream consumers and pin the real tag, then re-run `GOWORK=off` verification so each module proves it resolves through tagged deps rather than workspace state. Keep the umbrella `go.work` for ongoing local dev only.

---

## Risks & rollback

- **Per-repo commits, no umbrella repo:** there is no single "revert the whole phase" commit. Each phase touches multiple repos; tag a pre-migration commit in every repo you touch (`git tag pre-modsplit`) so any repo can roll back independently.
- **Vendored consumers hide resolution errors:** always run the vendoring consumers' gates with `GOWORK=off` and re-vendor after every dep change, or a stale `vendor/` will mask a broken `go.mod`.
- **Interface rename `LLM`→`Client`:** if the churn is large, keep `type LLM = Client` aliases in `inference` through Phase 8, then remove in a cleanup commit.
- **Bedrock/streaming edge cases:** Bedrock Converse + AWS eventstream stay in `llm` (design non-goal to genericize now); don't block Phase 5 on them.
- **Field-name reconciliation (uuid):** verified no external caller uses `.Cause`/`.Err` — but if a later `git` merge reintroduces harness's `Cause`-field variant, the `core/uuid` (flow `Err`-field) version is canonical.

## Commit message footer

End every commit body with:
```
Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
```
