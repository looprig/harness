# pkg/llm Phase 1 — ModelSpec Split & Connection-Bound Client — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task (fresh subagent per task; spec + code-quality review between tasks).
>
> **⚠ EXECUTION GATE — DO NOT START until the user confirms the cross-module strategy (see § Decision required).** Phase 1 is a BREAKING public-API change that also breaks the `swe` consumer module. Phases 0 and 0.5 are already merged to `main`.

**Goal:** Retire `ModelSpec`; make the client **connection-bound** (Provider + endpoint + auth bind at construction) and put a secret-free `Model` descriptor on every `Request`; fold sampling into `Sampling`, add `APIFormat`/`Origin`/`Caps` to `Model`; migrate all first-party consumers (`loop`, `session`/`event` fingerprint, `foreignloop`, `auto`) and the `swe` module; extract `providers/`.

**Architecture:** Wire the Phase-0 types (`APIFormat`, `Origin`, `Capabilities`, `Sampling`, `Effort`, `Codec`, `Authenticator`, `AuthKind`, `ModelMismatchError`, `pkg/llm/auth`) into the domain. The client binds `Provider`+endpoint+`Authenticator` once; each `Request` carries a secret-free `Model` whose connection fields must match the bound client (else `*ModelMismatchError`, pre-I/O). Codec selected per turn from `req.Model.APIFormat`. Source of truth: `docs/plans/2026-07-01-llm-provider-codec-layout-design.md` (§ ModelSpec redesign → After, § Auth enforcement, § Consumer impact, § Migration → Phase 1).

**Tech Stack:** Go (`github.com/looprig/harness`), stdlib + already-approved deps. Table-driven `-race` tests; typed errors. Worktree + `GOWORK=off` on every `go`/`make` command (worktree is outside the parent `go.work`).

---

## Decision required (blocks execution)

Phase 1 breaks the `swe` consumer's public-API usage:
- `swe/swarms/swe/registry.go`: `type ModelFactory func(systemPrompt string) llm.ModelSpec` (ModelSpec retired).
- `swe/swarms/swe/{session_title.go, swarm.go, agent.go, spawner.go, operator_eval_integration_test.go, session_title_test.go}`: construct `llm.ModelSpec` and `loop.Config{...}` (both reshaped).

**Recommended: coordinated (looprig + swe together in one worktree).** Both modules are members of `/Users/ipotter/code/go.work`, so a single worktree can reshape both and only merge when the **entire workspace** builds and tests green under `-race`. This honors the design's "no transitional duplicates" and never leaves a broken tree. Alternatives (compat shim / looprig-only-defer-swe / re-scope smaller) are viable but respectively reintroduce duplication, break the consumer, or postpone the payoff. **This plan assumes the coordinated approach**; Phase 1D (swe migration) is cleanly separable if a different strategy is chosen.

---

## Sub-phase map (task groups)

- **1A — llm domain reshape** (Tasks 1–7): `Sampling` on `Model`; `Model` gains `APIFormat`/`Origin`/`Caps`; `Request` gains `Model`/`System`/`Override`; retire `ModelSpec`; `Model.Validate`; `CustomModel`+options; catalog constructors.
- **1B — client & codec wiring** (Tasks 8–11): connection-bound `httpLLM` (codec × endpoint × auth); `ModelMismatchError` guard; `auto.New(model, auth)` rewrite; codec adapters implement `llm.Codec`.
- **1C — providers extraction** (Tasks 12–14): `providers/phala` (move `DefaultPhalaPolicy` out of `aci`); `openaiapi/chutes` → `providers/chutes`; `openaiapi/lmstudio` → catalog row; delete the now-empty `pkg/llm/openaiapi/` dir; fix the 4 deferred `lmstudio` stale header comments during the move.
- **1D — consumer migration** (Tasks 15–19): `loop.Config` split + `loop/turn.go`; `FingerprintFrom` moves (`event` + `session` config_fingerprint, restore-stable); `foreignloop`; **`swe` module** (ModelFactory, loop.Config sites, session_title, operator_eval).
- **1E — verification** (Task 20): whole-workspace build + `-race` + `make secure` for BOTH modules; restore-fingerprint stability proof.

Each task follows TDD (write/adjust failing test → implement → green → commit with the `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>` trailer). The controller curates exact per-task context at dispatch (as in Phases 0/0.5).

---

## 1A — Domain reshape

### Task 1: Fold sampling into `Model.Sampling`; add `APIFormat`/`Origin`/`Caps`
**Files:** `pkg/llm/model.go` (+ `model_test.go`). Depends on Phase-0 `Sampling`/`Capabilities`/`APIFormat`/`Origin`.
Reshape `Model` to the design's "After" shape:
```go
type Model struct {
    Provider  Provider
    APIFormat APIFormat        // NEW
    BaseURL   string
    Name      string
    Origin    Origin           // NEW (zero = OriginCustom, fail-safe)
    Caps      Capabilities     // NEW (replaces AcceptsImages bool)
    Sampling  Sampling         // NEW (folds Temperature/MaxTokens/… ; Effort replaces ReasoningEffort/ThinkingBudget)
}
```
Remove the old `Temperature`/`MaxTokens`/`AcceptsImages` fields and the `Spec(...)` method (materialization is gone — creds bind via `Authenticator`). Update `model_test.go`. **This is the breaking edit Phase 0 deferred.**

### Task 2: `Request` carries `Model`/`System`/`Override`; retire `ModelSpec`
**Files:** `pkg/llm/llm.go` (`Request`, `Response`, delete `ModelSpec` + its `redactKey`/`String`/`LogValue`), tests.
```go
type Request struct {
    Model    Model
    System   string
    Messages content.AgenticMessages
    Tools    []Tool
    Override *Sampling            // nil = use Model.Sampling
}
```
`Response.Model` echoes the resolved model. Delete `ModelSpec` entirely (grep proves zero references remain after 1B/1C/1D).

### Task 3: `Model.Validate()` (typed `*ValidationError`)
Provider known; `APIFormat` in provider's supported set (fail-closed on unsupported pair); `Name` non-empty; provider-specific URL rule (`https://` required; `http://` only for `127.0.0.1`/`localhost`; provider-routed clients may leave `BaseURL` empty). Per design § Model.Validate.

### Task 4: `CustomModel` + `ModelOption`s
`func CustomModel(p Provider, f APIFormat, baseURL, name string, opts ...ModelOption) Model` (Origin defaults custom; caps fail-safe/opt-in via `WithMaxContext`/`WithTools`/`WithImages`/`WithSampling`…). Per design § Custom models.

### Task 5: Catalog constructors (`Origin: OriginCatalog`)
Rewrite `catalog.go` rows as `Model` constructors with filled `Caps` (e.g. `GLM46Phala()`). Hand-authored for phala/chutes; discovery-generated rows deferred.

### Tasks 6–7: `APIFormat`↔provider support map + `Effort` mapping seam
Provider→supported-`APIFormat` set (used by `Validate` + `auto`); document the codec-side `Effort`→wire mapping contract (implemented in 1B codecs).

## 1B — Client & codec wiring

### Task 8: `Codec` adapters implement `llm.Codec`
Wrap `codec/openaiapi` (`EncodeRequest`/`DecodeResponse`/`NewStream`→`DecodeEvent`) behind the Phase-0 `llm.Codec` interface, mapping `RequestMode` (drop the `stream bool`). Map `Sampling.Effort`→`reasoning_effort` (capability-driven).

### Task 9: connection-bound `httpLLM` (codec × endpoint × auth)
`func newHTTP(c Codec, ep Endpoint, a Authenticator) *httpLLM` — `a` REQUIRED (no zero-value fall-through; "no auth" = `auth.None()`). Binds connection; serves any model on it.

### Task 10: `ModelMismatchError` enforcement (pre-I/O)
`Invoke`/`Stream` reject `req.Model` whose provider/endpoint ≠ bound client, returning `*ModelMismatchError` before encode/network. Uses the Phase-0 error type.

### Task 11: `auto.New(model, auth)` rewrite
Dispatch on `model.Provider`; `RequiredAuth()` fail-closed (`*AuthRequiredError` on empty cred); pick codec from `model.APIFormat`; add confidential wiring (aci) for phala. Replaces the current `ModelSpec`-based `auto.go`.

## 1C — providers extraction

### Task 12: `providers/phala`
Move `DefaultPhalaPolicy()` + pinned trust anchors out of `pkg/llm/aci/policy.go` into `pkg/llm/providers/phala`; `phala.New(baseURL, key auth.APIKey, policy)` composes `aci` + `codec/openaiapi`. `aci` becomes provider-agnostic. `auto` wires `ProviderPhala → phala.New`.

### Task 13: `openaiapi/chutes` → `providers/chutes`
Relocate (git mv) the chutes package to `pkg/llm/providers/chutes`; update `auto` import. Keep its bespoke ML-KEM protocol + its own `sseEventReader` (do NOT dedup into `codec/sse` — different contract: `errSSEDone` vs `io.EOF`, multi-line accumulation, 1 MiB buffer). Fix its stale `// internal/...` headers during the move.

### Task 14: `lmstudio` → catalog row; remove empty `openaiapi/` dir
Dissolve `openaiapi/lmstudio` into an `auto`/catalog row over `codec/openaiapi` (no package). Fix the 4 deferred `lmstudio` stale header comments (or delete the files as they dissolve). After this, `pkg/llm/openaiapi/` is gone entirely.

## 1D — consumer migration

### Task 15: `loop.Config` split + `loop/turn.go`
```go
type Config struct {
    Client llm.LLM
    Model  llm.Model   // secret-free; on every Request; NO System, NO secret
    System string      // per-agent prompt; on every Request AND hashed into fingerprint
    // …unchanged fields…
}
```
`turn.go` assembles `llm.Request{Model: cfg.Model, System: cfg.System, Messages: …}` each turn.

### Task 16: `FingerprintFrom` moves (restore-STABLE)
`event/config_fingerprint.go` + `session/config_fingerprint.go`: `ModelID = cfg.Model.Name`, `SystemPromptRev = hexSHA256(cfg.System)` (was `cfg.Model.System`). **Same fingerprint inputs, new field homes** — add a test proving a pre-refactor fingerprint value still matches post-refactor for equivalent config (restore stability).

### Task 17: `foreignloop`
Update `pkg/foreignloop` model/system reads to the new field homes (same values).

### Task 18–19: `swe` module migration (only under the coordinated strategy)
`ModelFactory func(systemPrompt string) llm.Model` (was `ModelSpec`); update `session_title.go`/`registry.go`/`swarm.go`/`agent.go`/`spawner.go` + tests to build `loop.Config{Client, Model, System}` and call `auto.New(model, auth)` where they materialized specs. Land in the SAME worktree so `GOWORK` builds both modules green.

## 1E — verification (Task 20)
- BOTH modules: `GOWORK=off go build ./...` + `go test -race ./...` (run from each module dir), `make secure`.
- `grep -rn "ModelSpec" .` → only historical/plan docs, zero code refs.
- Restore-fingerprint stability test green (Task 16).
- Final holistic review over the whole branch; then `finishing-a-development-branch` (coordinated merge to `main`).

---

## Done criteria
- `ModelSpec` deleted; `Model` reshaped (APIFormat/Origin/Caps/Sampling); `Request` carries `Model`/`System`/`Override`; client connection-bound with `*ModelMismatchError` guard; `auto.New(model, auth)`; `providers/{phala,chutes}` exist; `lmstudio`+`openaiapi/` dir gone.
- `loop.Config` split; `FingerprintFrom` restore-stable; `foreignloop` + `swe` migrated; whole workspace green under `-race` + `make secure`.
- Deferred to Phase 2: `providers/bedrock` + `codec/bedrockconverse`/`anthropicapi`, the `auth.SigV4` signer + `SigV4Credentials` redaction, `Capabilities.Thinking` granularity, discovery-generated catalog rows.
