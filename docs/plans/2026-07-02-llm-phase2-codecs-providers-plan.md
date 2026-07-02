# pkg/llm Phase 2 — Gemini + Anthropic codecs, OpenRouter + Bedrock providers — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Stage A fans out (parallel, isolated worktrees); Stages B–C are sequential. Each unit: implementer → spec review → code-quality review.

**Goal:** Add two new wire codecs (`codec/gemini`, `codec/anthropicapi`) and two new providers (OpenRouter as a catalog row over `codec/openaiapi`; Bedrock as a package with an AWS SigV4 signer + region→route + a Converse/Anthropic codec), wired through `auto` so `Model.APIFormat` selection works end-to-end. Builds on merged Phase 1 (`main` @ `15bed86`).

**Architecture:** Onboarding = compose the axes established in Phase 1. A codec is a package implementing `llm.Codec` (`EncodeRequest/DecodeResponse/DecodeEvent`); `codec/openaiapi` is the structural template. A provider is either a **catalog row + `auto` wiring** over an existing codec (OpenRouter, OpenAI-compatible) or a **package** when it has provider-specific code (Bedrock: SigV4 + region routing). The generic `pkg/llm/transport` client (codec × endpoint × auth) already exists; OpenRouter reuses it, Bedrock needs its own transport (AWS Bedrock Runtime is not plain OpenAI-over-HTTP).

**Tech Stack:** Go, stdlib. **SigV4 uses stdlib `crypto/hmac`+`crypto/sha256` — NO new dependency.** Wire-format accuracy: implementers MUST consult authoritative docs via the context7 MCP (`resolve-library-id`→`query-docs`) and, for Anthropic, the `claude-api` skill — do NOT hand-wave the JSON/SSE shapes. Table-driven `-race` tests; typed errors; `GOWORK=off` in worktrees.

**Design source:** `docs/plans/2026-07-01-llm-provider-codec-layout-design.md` (§ five axes, § Auth enforcement, § Migration → Phase 2).

---

## DESIGN DECISION (resolve before Stage B): `auto.New` credential handling for SigV4
`auto.New(model llm.Model, key auth.APIKey)` (Phase 1) can't carry Bedrock's `auth.SigV4Credentials`. Options:
- **(A) Bedrock only via its typed constructor** `bedrock.New(creds auth.SigV4Credentials, region string) (llm.LLM, error)`; `auto.New` returns a typed "provider requires SigV4, use bedrock.New" error for `ProviderBedrock`. Smallest change; keeps `auto.New` key-only. **Recommended for this pass** (matches design's compile-time-typed-constructor path; runtime auto-dispatch for SigV4 is rare).
- **(B) Generalize `auto.New` to a sealed `auth.Credential`** union (`APIKey | SigV4 | None`, each with an `Authenticator()` + typed accessors). Most future-proof; larger blast radius (every `auto.New` caller + Phase-1 signature change).
Default to (A) unless the user picks (B). Either way, `bedrock.New` (typed) is the primary Bedrock entry.

---

## Stage A — parallel, independent new packages (isolated worktrees, off `main`)

### A1: `codec/anthropicapi` — Anthropic Messages API codec
**New package** `pkg/llm/codec/anthropicapi` implementing `llm.Codec` (`var _ llm.Codec = Codec{}`). Template: `pkg/llm/codec/openaiapi` (same file layout: encode/decode/types/stream + Codec adapter). Consult the `claude-api` skill + context7 for the Messages API request/response + streaming event schema.
- `EncodeRequest(req, mode)`: map `llm.Request` → Anthropic `/v1/messages` body (`model`, `system` as top-level `system`, `messages` with role/content blocks incl. tool_use/tool_result, `tools`, `max_tokens` from Sampling, `stream` when mode==Stream, effort→thinking per capability). Map `content.AgenticMessages`/`Tool` accurately.
- `DecodeResponse`: Anthropic message response → `*llm.Response` (text/tool_use blocks → `content.AIMessage`; usage).
- `DecodeEvent`: ONE de-framed SSE event (`message_start`/`content_block_start`/`content_block_delta`/`content_block_stop`/`message_delta`/`message_stop`) → `[]content.Chunk` (text deltas, thinking deltas, tool_use with input_json_delta fragments — emit fragments; `streamaccumulator` assembles). Stateless per-event (like openaiapi); tolerant-skip unknown/irrelevant events.
- Tests: table-driven, `-race`, covering encode (system/tools/images/effort), decode (text+tool_use+usage), DecodeEvent (each event type, multi-block, tolerant skip). Green: `GOWORK=off go test -race ./pkg/llm/codec/anthropicapi/`.
- Do NOT touch shared files (APIFormatAnthropic already exists in `apiformat.go`). Do NOT wire into auto (Stage B).

### A2: `codec/gemini` — Google Gemini generateContent codec
**New package** `pkg/llm/codec/gemini` implementing `llm.Codec` + add `APIFormatGemini APIFormat = "gemini"` to `pkg/llm/apiformat.go` (and its `Valid()` switch). Consult context7 for the Gemini `generateContent`/`streamGenerateContent` request/response + SSE schema.
- `EncodeRequest`: `llm.Request` → Gemini body (`contents` with roles user/model + parts text/inlineData/functionCall/functionResponse; `systemInstruction`; `tools` functionDeclarations; `generationConfig` temperature/maxOutputTokens/topP/stopSequences; thinking per effort).
- `DecodeResponse`: Gemini `candidates[0].content.parts` → `*llm.Response` (text + functionCall → tool_use; `usageMetadata`).
- `DecodeEvent`: one streamed `GenerateContentResponse` chunk → `[]content.Chunk` (text parts, functionCall). Stateless; tolerant-skip.
- Tests as A1. Green: `GOWORK=off go test -race ./pkg/llm/codec/gemini/`. This agent OWNS the `apiformat.go` edit (no other Stage-A agent touches it). Do NOT wire into auto.

### A3: `auth.SigV4` signer + `SigV4Credentials` redaction
**In `pkg/llm/auth`** (new `sigv4.go`): implement AWS Signature Version 4 for HTTP requests using stdlib (`crypto/hmac`, `crypto/sha256`, `encoding/hex`, `time`) — canonical request → string-to-sign → signing key → `Authorization: AWS4-HMAC-SHA256 ...` header. Provide:
- `func SigV4(creds SigV4Credentials, region, service string) llm.Authenticator` — returns an Authenticator whose `Authorize(ctx, r)` signs `r` (reads/hashes the body if present; sets `X-Amz-Date`, `Authorization`, and `X-Amz-Security-Token` when SessionToken set). **Context/time:** derive the timestamp deterministically at sign time.
- Redaction: add `String()`/`LogValue()` to `SigV4Credentials` (mirror `headerAuth`) so `SecretAccessKey`/`SessionToken` never log. (SigV4Credentials type already exists in auth.go — add methods; do NOT change its fields.)
- Tests: table-driven `-race` — a KNOWN AWS SigV4 test vector (AWS publishes canonical examples) to prove the signature is correct; redaction test (`%v`/`%+v`/slog never leak the secret); SessionToken header present/absent. Green: `GOWORK=off go test -race ./pkg/llm/auth/`.
- Do NOT touch AuthKind/provider enums (Stage B). Only auth package.

**Stage A integration:** each A-agent commits to its own isolated branch; controller merges the three (disjoint files; only A2 touches `apiformat.go`) into the Phase-2 branch, reviews each, then Stage B.

## Stage B — sequential wiring (needs Stage A)

### B1: Provider enums + registries
`llm.go`: add `ProviderOpenRouter`, `ProviderBedrock`. `provider.go`: `RequiredAuth` (openrouter→`AuthAPIKey`, bedrock→`AuthSigV4`); `supportsAPIFormat` (openrouter→{openai}; bedrock→{anthropic, bedrock-converse}; also add gemini support to whichever providers offer it — e.g. a future google provider, or leave gemini for custom/catalog). Add `APIFormatBedrockConverse` already exists. Table tests updated.

### B2: `providers/openrouter` (catalog row + auto wiring)
OpenAI-compatible. Likely just: a `catalog` row `OpenRouter(name string) Model` (BaseURL `https://openrouter.ai/api/v1`, APIFormatOpenAI, Bearer) + `auto` dispatch arm `ProviderOpenRouter → transport.New(openaiapi.Codec{}, Endpoint{...}, auth.Key(key))`. No package needed unless it carries specific code.

### B3: `providers/bedrock` (+ region→route, SigV4)
Package `pkg/llm/providers/bedrock`: `New(creds auth.SigV4Credentials, region string) (llm.LLM, error)` composing a Bedrock-runtime transport (endpoint `bedrock-runtime.<region>.amazonaws.com`, path `/model/<modelId>/invoke` or `/converse`), `auth.SigV4(creds, region, "bedrock")`, and a codec by `Model.APIFormat` (anthropic → reuse `codec/anthropicapi` with Bedrock's body variant, or `bedrock-converse` → a Converse codec). **This is the hard unit** — the Bedrock Runtime API differs from OpenAI HTTP (per-model path, AWS eventstream framing for streaming, not plain SSE). Scope carefully; if AWS eventstream framing is too large, land Invoke (non-streaming) first and flag streaming as a follow-up. Consult context7 for Bedrock Runtime InvokeModel/Converse + eventstream.

### B4: `auto` wiring + catalog rows
`auto.New`: extend `codecFor` (add gemini, anthropic, bedrock-converse cases); add dispatch arms (openrouter; bedrock per the DESIGN DECISION above). Catalog: add Gemini + OpenRouter + Claude-on-Bedrock rows. Tests: `auto.New` builds each new provider; fail-closed for missing creds.

## Stage C — verification
`GOWORK=off go build ./...`, `go test -race ./...`, `make secure`. Add an end-to-end `Model.APIFormat`-selection test (same provider, two APIFormats → different codec). Final holistic review; finishing-a-development-branch.

## Done criteria
- `codec/{gemini,anthropicapi}` implement `llm.Codec` with accurate wire mapping + tests; `auth.SigV4` signs correctly (test-vector-verified) + redacts.
- `ProviderOpenRouter`/`ProviderBedrock` wired; `providers/bedrock` builds a working client (at least Invoke); catalog rows added.
- `auto` selects codec by `APIFormat`; fail-closed creds; whole looprig green + `make secure`.
- Deferred if scoped out: Bedrock streaming (AWS eventstream) if landed Invoke-only; the `auth.Credential` union (option B) if (A) chosen.
