# Design: looprig module boundaries

**Date:** 2026-07-04
**Status:** Draft spec from architecture discussion

## Problem

`github.com/looprig/harness` currently contains several concerns with different audiences
and dependency weights:

- reusable primitives (`uuid`, `content`, small slog setup)
- generic model inference contracts and HTTP/codecs
- provider-specific LLM SDK behavior
- the multi-agent harness runtime
- storage contracts and backend implementations
- app/domain consumers (`flow`, `cli`, `swe`)

The result is workable during early development, but it makes an enterprise "pick the
modules you need" kit harder to consume. A user who wants the multi-agent harness should not
have to import every provider SDK. A user who wants generic OpenAI-compatible inference by
URL should not have to import the harness runtime. A storage backend should not import the
agent runtime or `flow`.

## Goals

- Split by stable ownership boundaries, not by file count.
- Keep the base agent runtime free of provider SDK dependencies. Existing harness runtime
  dependencies from bundled tools, transcript rendering, and test-only storage integrations can
  stay until those surfaces are intentionally split.
- Make `inference` standalone-usable without `harness`, `llm`, or non-stdlib dependencies:
  callers can choose an endpoint, auth, router, API codec, wire framing, model name, and request.
- Let `harness + core + inference` run end-to-end against built-in OpenAI-compatible,
  Anthropic-compatible, and Gemini-compatible wire APIs, and against custom APIs that supply their
  own API codec, router, and, for streaming, stream decoder or stream framer, without importing the
  provider bundle.
- Keep provider batteries in a separate `llm` module.
- Keep tools and harness stores inside `harness` for now. They are part of the runtime surface
  and do not need extra modules yet.
- Rename `storekit` to a clearer storage contracts module name.
- Preserve the current ability to perform most relocation with `git mv`, `cp`, `sed`/`perl`,
  `gofmt`, `go mod tidy`, and tests.

## Non-goals

- No split of `harness/tools` into a separate tools module.
- No split of `harness/journal`, `harness/sessionstore`, or `harness/workspacestore` into a
  separate harness-store module.
- No move of `identity` to `core`. `identity` is harness runtime correlation and attribution
  vocabulary, not generic identity/auth.
- No new external dependencies.
- No provider model-policy catalogue in the base `inference` module.
- No implementation in this spec.

## Target modules

### `github.com/looprig/core`

Small shared primitives. This module should stay stdlib-only.

Packages:

```text
core/uuid
core/content
core/content/streamaccumulator
core/logging
```

Package ownership:

- `uuid`: one shared UUID implementation for `harness`, `flow`, `cli`, stores, and tests.
  This replaces the duplicated `flow/pkg/uuid` and `harness/pkg/uuid`. The stricter parser
  behavior from `flow/pkg/uuid` should be retained.
- `content`: message, block, chunk, media type, and JSON codec vocabulary.
- `content/streamaccumulator`: pure conversion from streaming chunks to complete content
  blocks. It imports only stdlib plus `core/content`.
- `logging`: tiny `log/slog` helpers, such as parse-level and JSON logger construction.
  It must not become observability policy, tracing, sinks, or event persistence.

Rationale:

`content` is the shared language between inference, harness events, tool results, transcript
rendering, and provider codecs. `uuid` is already duplicated. `logging` is application-neutral
when kept to simple slog setup. Keep `logging` in `core` for this split, but keep it narrow: if it
grows beyond parse-level and logger construction into application logging policy, that policy
belongs back in the app modules.

### `github.com/looprig/inference`

Generic model-call layer. This module should remain stdlib-only plus `core`.

Packages:

```text
inference
inference/auth
inference/transport
inference/route
inference/wire/jsonbody
inference/wire/sse
inference/wire/ndjson
inference/codec/openaiapi
inference/codec/anthropicapi
inference/codec/geminiapi
```

Package ownership:

- `inference`: provider-neutral request/response/client types:
  - `Client` interface
  - `Request`
  - `Response`
  - `Endpoint`
  - `Model`
  - `Provider` or `ProviderName` as an opaque label only
  - `APIFormat` as an opaque/helper label, not a validation gate
  - `Codec`
  - `StreamingCodec`
  - `RequestEncoder`
  - `ResponseDecoder`
  - `StreamFrame`
  - `StreamFramer`
  - `StreamDecoder`
  - `Authenticator`
  - `Router`
  - `RequestMode`
  - `Sampling`
  - `Effort`
  - `Capabilities`
  - `Origin`
  - `Usage`
  - `ToolSpec`
  - `StreamReader`
  - generic inference errors such as `NetworkError`, `APIError`, `ValidationError`,
    `MissingCredentialsError`, and `ModelMismatchError`
- `auth`: generic auth mechanisms, for example static header or API-key bearer auth. It must
  not own provider policy.
- `transport`: generic HTTP client assembled from endpoint, router, request/response encoders,
  optional stream decoder, and auth. It owns HTTP mechanics only; request routing comes from the
  injected router.
- `route`: stdlib route builders for wire APIs, for example a static chat-completions route and
  Gemini's model-in-path route. It must also expose a custom router seam so callers can support
  unknown API formats without importing `llm`.
- `wire/*`: low-level body and stream framing helpers. These packages know about byte-level wire
  formats such as JSON HTTP bodies, server-sent events, and newline-delimited JSON. They do not
  know about LLM messages, tools, usage, model names, or provider semantics.
- `codec/*`: semantic API codecs. They translate between `inference.Request` and provider/API
  JSON shapes, and between provider/API responses or stream events and `core/content`.

`inference.Provider` must not carry the current `llm.Provider` policy methods. Provider auth
requirements, allowed wire formats, default endpoint behavior, and known-provider constants belong
in `llm` or in a consumer composition root. `inference` should route by explicit endpoint,
explicit codec/API format, and explicit auth.

`inference.APIFormat` must also be open-ended. Built-in constants are useful names for bundled
codecs and routes, but the generic transport must not reject an unknown `APIFormat` when the caller
has supplied explicit request/response/stream decoders and a `Router`. Validation of known
provider/format pairs belongs in `llm` or in consumer code.

`inference.Model` validation is structural only. It can check that a model name is present, that a
non-empty endpoint URL is syntactically safe, and that request-local fields are internally valid.
It must not reject unknown providers, unknown API formats, provider/API-format pairs, provider
default endpoint behavior, or empty model endpoint identity that is intentionally bound by the
client. Fail-closed known-provider validation belongs in `llm` presets, provider constructors, or
consumer composition roots.

`inference.Codec`, `inference.StreamingCodec`, `inference.RequestEncoder`,
`inference.ResponseDecoder`, `inference.StreamFramer`, `inference.StreamDecoder`, and
`inference.Authenticator` are root interfaces. `inference/codec/*`, `inference/wire/*`, and
`inference/auth` provide stdlib implementations or helpers for those interfaces, but callers can
supply their own implementations without importing `llm`.

The encoder/decoder boundary should separate semantic API mapping from wire framing:

```go
type EncodedRequest struct {
	Header http.Header
	Body   io.Reader
}

type RequestEncoder interface {
	EncodeRequest(req Request, mode RequestMode) (EncodedRequest, error)
}

type ResponseDecoder interface {
	DecodeResponse(body []byte) (*Response, error)
}

type StreamFrame struct {
	Name     string
	Metadata map[string]string
	Data     []byte
}

type StreamFramer interface {
	DecodeStreamFrames(body io.ReadCloser) (*StreamReader[StreamFrame], error)
}

type StreamDecoder interface {
	DecodeStream(resp *http.Response) (*StreamReader[content.Chunk], error)
}

type Codec interface {
	RequestEncoder
	ResponseDecoder
}

type StreamingCodec interface {
	Codec
	StreamDecoder
}
```

`RequestEncoder` handles normal request bodies, including streaming-mode request bodies when the
wire API represents streaming as a JSON flag. Do not add a separate `StreamEncoder` unless a real
supported API needs outbound streaming request bodies. `EncodedRequest.Body` is single-shot. The
generic transport must not implement automatic retries or redirects that replay request bodies;
retry and replay policy belongs in callers or `llm` presets. Add a replayable-body hook later only
when a concrete requirement justifies changing the root contract.

`ResponseDecoder` handles non-streaming responses from an already-drained successful body; the
transport owns HTTP status mapping, body closing, and read errors. `Codec` is only the convenience
composition of `RequestEncoder` and `ResponseDecoder`. Streaming is optional: a non-streaming API
must not stub `DecodeStream`; the streaming path requires an explicit `StreamDecoder` or
`StreamingCodec` and otherwise fails before I/O with a typed unsupported-streaming error.

`StreamFramer` converts a streaming body into raw stream events and owns closing that body through
the returned `StreamReader`. If it returns an error before returning a reader, it must close the
body before returning. `StreamDecoder` owns the full streaming response path from a successful HTTP
response to `content.Chunk`; it can use `wire/sse`, `wire/ndjson`, a future provider-owned framer
such as `llm/providers/bedrock/eventstream`, or a custom caller-supplied framer. Once
`DecodeStream` is called, it owns `resp.Body`; if it returns an error before returning a reader, it
must close `resp.Body` before returning.

`wire/sse` must be a real SSE event framer, not an OpenAI-only `data: ` line helper. It should join
multi-line `data:` fields with newlines, ignore comments, preserve optional event names when useful,
and leave API-specific sentinels such as `data: [DONE]` to the semantic stream decoder. OpenAI,
Anthropic, Gemini, or custom stream decoders decide whether a particular event payload is terminal.
`wire/ndjson` can emit one frame per decoded line.

`inference.Endpoint` is explicit client binding metadata, for example base URL plus optional opaque
provider/API-format labels. It must not contain a hardcoded chat path; route shape belongs to the
injected `Router`. `inference.Model` is secret-free request metadata. The wire-relevant fields are
model name, API format, optional endpoint identity, and any caller-provided opaque provider label.
If a client is bound to an endpoint and a request model declares conflicting non-empty endpoint,
provider, or API-format identity, the transport should fail closed with `ModelMismatchError`.
Empty identity fields are wildcards, not claims.

`Sampling` is request/default tuning. If `Sampling` retains the current reasoning-effort field,
`Effort` moves with it into `inference`; otherwise the field must be explicitly removed during the
split. `Capabilities` and `Origin` are local gating/trust metadata only: they must never imply a
built-in model catalogue, provider truth table, or provider validation policy.

Generic inference errors live in `inference`: network wrapping, HTTP/API status errors, request or
model validation errors, missing-credentials errors for generic auth mechanisms, and model/endpoint
binding mismatches. Generic missing-credentials errors must describe the missing credential or
header without depending on a provider policy table. Provider-specific auth requirement errors,
attestation failures, E2EE failures, and SigV4-specific signing errors live in `llm`.

The router seam is what keeps Gemini standalone. OpenAI-compatible and Anthropic-compatible APIs
can use a static route such as `/chat/completions` or `/messages`. Gemini-compatible APIs need
mode-aware routes such as:

```text
invoke: POST /models/{model}:generateContent
stream: POST /models/{model}:streamGenerateContent?alt=sse
```

Those route shapes are wire API facts and can live in `inference/route` with no external
dependencies. Google-as-a-provider facts, such as the default base URL or provider auth policy,
stay in `llm`.

The router contract must be strong enough that the generic transport does not hardcode OpenAI
assumptions. A router should return the full request method, absolute URL, and route headers for a
given base endpoint, request, and mode:

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

The transport may own common HTTP execution, response-status mapping, and body closing, but it must
not hardcode POST, `/chat/completions`, `Content-Type`, streaming `Accept` headers, or a specific
stream framing such as SSE. For both invoke and stream requests, the transport maps non-2xx
responses to `APIError` before calling the response or stream decoder; error bodies are drained and
closed by the transport because they are usually normal JSON/text, not a stream. Built-in routers
and codecs can provide defaults for their own wire APIs. Headers are applied in this order: route
headers, then encoder headers, then auth headers; later layers override earlier layers.

AWS eventstream stays in Bedrock for now. No current non-Bedrock inference codec needs it, so the
frame reader should live under `llm/providers/bedrock/eventstream` alongside the Bedrock semantic
stream decoder. Extract it later only if another standalone inference codec or non-Bedrock module
needs the same stdlib-only framing logic. Bedrock Converse request/response mapping, region routing,
and SigV4 wiring also belong in `llm/providers/bedrock` unless and until a provider-neutral
Bedrock/Converse codec and router can live in `inference` without AWS SDK dependencies or provider
policy.

Allowed facts in this module:

- Wire formats: OpenAI-compatible, Anthropic-compatible, Gemini-compatible.
- Wire route shapes required by those formats, including mode-aware routes.
- Low-level stdlib wire helpers such as JSON body encode/decode, SSE framing, and NDJSON framing.
- Explicit endpoint URLs supplied by callers.
- Generic HTTP mechanics.
- Generic API key/header auth.
- Custom caller-supplied codecs, stream decoders, stream framers, and routers.

Disallowed facts in this module:

- Provider default endpoints.
- Provider model catalogues.
- Phala/Chutes attestation and E2EE.
- Bedrock SigV4 policy.
- Bedrock Converse unless/until it has a provider-neutral codec, stream decoder, and router that do
  not require provider policy.
- Consumer model choices.

Rationale:

This is the interface-bearing module, but it should be named for the domain, not for the fact
that it contains interfaces. `inference` is understandable to consumers and can include both
interfaces and generic implementations.

### `github.com/looprig/llm`

Batteries-included provider SDK. Depends on `core` and `inference`. It may carry external
dependencies where a provider requires them.

Packages:

```text
llm
llm/auth
llm/auto
llm/providers/gemini
llm/providers/bedrock
llm/providers/chutes
llm/providers/phala
llm/aci
llm/e2e
llm/tee
```

Package ownership:

- provider defaults and canonical endpoints
- provider-specific constructors
- provider-specific auth wiring
- provider-specific auth implementations such as AWS SigV4
- provider-specific attestation/E2EE machinery
- generic HTTP provider presets in `llm/auto` for providers such as LM Studio and OpenRouter,
  unless they later need dedicated packages
- Google/Gemini convenience wiring such as default base URL, provider constants, and API-key
  header selection

`llm` must not reintroduce an SDK-owned model catalogue. The current design is that the SDK owns
mechanism and consumers own model policy. Curated model rows belong in consumers such as `swe`, not
in `inference` or the base `llm` SDK.

Rationale:

This module exists for users who want provider conveniences out of the box. It is deliberately
not required by `harness`.

### `github.com/looprig/harness`

Multi-agent runtime SDK. Depends on `core`, `inference`, and `storage`. It must not depend on
`llm`.

Packages:

```text
harness/identity
harness/event
harness/command
harness/tool
harness/tools
harness/loop
harness/session
harness/hub
harness/foreignloop
harness/foreignloop/claude
harness/journal
harness/sessionstore
harness/workspacestore
harness/transcript
harness/transcript/html
harness/transcript/journalsource
harness/eval
harness/api
```

Package ownership:

- `identity`: harness runtime correlation and attribution:
  - `Coordinates`
  - `Cause`
  - `AgentName`
  - `Agency`
- `event`: public event stream emitted by the harness.
- `command`: command protocol used by the loop/session runtime.
- `tool`: tool execution contracts, permission request contracts, approval scopes, middleware,
  and preparation artifacts.
- `tools`: bundled tool implementations.
- `loop`: single-loop actor.
- `session`: multi-loop/multi-agent orchestration.
- `hub`: event fan-in and subscription behavior.
- `journal`, `sessionstore`, `workspacestore`: harness persistence facades over `storage`.
- `transcript`: reconstruction/export of harness event and command records.
- `eval`: harness runtime evaluation helpers and fixtures; it may exercise harness behavior but
  must not become a provider catalogue or app-specific benchmark suite.
- `api`: optional HTTP API over the harness runtime.

Current test-only backend imports, such as a harness integration test that exercises `fsstore`, are
temporary exceptions to the clean graph. Before enforcing the final dependency graph, either move
those tests to the backend/app module that owns the concrete store or keep them behind explicit
integration-only wiring that does not affect normal harness consumers.

Why `identity` stays in harness:

`identity` is not generic auth identity. It is harness event/command correlation vocabulary.
It answers "where did this happen?", "which agent role produced it?", "was the cause human or
machine?", and "what caused this event?" Those concepts are public because harness emits public
events, but they are still harness runtime concepts, so they stay with harness.

Why `tool` and `tools` stay in harness:

`tool` depends on `core/content` because tool results are content blocks, but the package also
owns harness-specific execution policy: permissions, approval scopes, prepared artifacts,
middleware, and skill/workspace concepts. Splitting it now would add module overhead without a
cleaner boundary.

### `github.com/looprig/storage`

Rename of `storekit`. Leaf storage contracts plus reference/test helpers. Stdlib-only.

Packages:

```text
storage
storage/memstore
storage/storetest
```

Package ownership:

- `Ledger`
- `Leaser`
- `KV`
- `Blobs`
- typed storage errors
- `AppendDefinite`
- `ValidateName`
- `Composite` and `NewComposite`
- in-memory backend for tests and simple local use
- backend conformance suite

Rationale:

`storage` is clearer than `storekit`: it names the domain instead of sounding like a helper kit.
Backends continue to be named by backend technology:

```text
github.com/looprig/fsstore
github.com/looprig/natsstore
github.com/looprig/rclonestore
```

### Consumer modules

Existing app/domain consumers remain separate:

```text
github.com/looprig/flow
github.com/looprig/cli
github.com/looprig/swe
```

Expected dependencies:

- `flow`: depends on `core/uuid` and possibly `core/logging`.
- `cli`: depends on `core`, `harness`, and whichever app modules it presents.
- `swe`: depends on `core`, `harness`, `llm`, and storage backends selected by the app.

## Dependency graph

In this section, `A -> B` means `A` imports or depends on `B`.

Allowed edges:

```text
inference -> core
llm -> core
llm -> inference

harness -> core
harness -> inference
harness -> storage

fsstore -> storage
natsstore -> storage
rclonestore -> storage

flow -> core
cli -> core
cli -> harness
swe -> core
swe -> harness
swe -> llm
swe -> selected storage backends
```

Forbidden dependencies:

```text
harness -> llm
inference -> harness
core -> inference
core -> harness
storage -> harness
storage -> flow
backend stores -> harness
backend stores -> flow
```

## Public API implications

`harness/event` is public because harness emits `event.Event` values and consumers inspect,
filter, serialize, or render them. Any exported type embedded in those events is also public
in practice.

That means `harness/identity` is part of the harness public API, but not part of `core`. This
keeps the public event vocabulary cohesive without broadening `core` into a runtime junk drawer.

`inference` owns the provider-neutral model-call contract. `harness` consumes that contract.
Concrete provider wiring lives in `llm` or in user code that constructs an `inference.Client`.

## Migration approach

Most of the extraction should be mechanical.

Preferred mechanical tools:

```sh
git mv
cp -R
rg
perl -pi -e
sed
gofmt -w
go mod tidy
go test
```

Use mechanical moves and import rewrites for:

- `harness/pkg/uuid` and `flow/pkg/uuid` -> `core/uuid`
- `harness/pkg/content` -> `core/content`
- `harness/pkg/content/streamaccumulator` -> `core/content/streamaccumulator`
- `cli/internal/logging` -> `core/logging`
- `storekit` -> `storage`
- import path rewrites

Use deliberate code edits for:

- separating `pkg/llm` into `inference` and `llm`
- deciding which `llm` structs belong to the generic contract
- keeping current provider policy methods such as `RequiredAuth`, `supportsAPIFormat`, and
  `allowsEmptyBaseURL` out of `inference`
- splitting `pkg/llm/auth`: generic header/API-key/no-auth helpers move to `inference/auth`,
  while SigV4 credentials and signing stay in `llm/auth`
- making `APIFormat` open-ended in `inference`; fail-closed known-provider/API-format validation
  stays in `llm`
- changing `Model.Validate` in `inference` to structural validation only; known provider,
  provider/API-format, provider auth, and provider default endpoint validation stay in `llm`
- replacing the current static `transport.Endpoint.ChatPath` assumption with an injected
  mode-aware `Router`
- defining `inference.Endpoint` binding rules and preserving fail-closed model/endpoint mismatch
  behavior in the generic transport
- splitting the current `llm.Codec` shape into request encoding, non-streaming response decoding,
  and optional streaming response decoding; `Codec` is request/response only, and
  `StreamingCodec` adds `StreamDecoder`
- preserving the single-shot request-body contract: the generic transport does not retry or replay
  request bodies
- defining streaming body ownership: non-2xx streaming responses are drained and closed by
  transport before stream decoding, while `StreamDecoder` and `StreamFramer` close bodies they
  receive if they fail before returning a reader
- applying headers in order route, encoder, auth, with later layers overriding earlier layers
- moving low-level stream framing to `inference/wire/*` and keeping semantic API mapping in
  `inference/codec/*`; `wire/sse` must implement SSE event framing rather than OpenAI-only data
  line parsing
- moving the current Gemini JSON codec to `inference/codec/geminiapi` and the Gemini
  generateContent routes to `inference/route`
- moving generic typed inference errors to `inference`, while keeping provider auth-policy errors
  and provider-security errors in `llm`
- provider-specific default endpoint handling
- treating LM Studio and OpenRouter as `llm/auto` generic HTTP presets unless dedicated provider
  packages are deliberately introduced
- deciding whether `APIFormatBedrockConverse` stays in `llm` for now or moves only after a
  provider-neutral codec, stream decoder, and router exist
- implementing AWS eventstream framing, if needed, as `llm/providers/bedrock/eventstream`;
  extract it to `inference/wire` only if another standalone inference codec or non-Bedrock module
  needs it
- any compile failures that reveal real semantic coupling

Recommended phase order:

1. Extract `core/uuid`.
2. Extract `core/content` and `core/content/streamaccumulator`.
3. Extract `core/logging`.
4. Rename `storekit` to `storage` and rewrite store backend imports.
5. Extract `inference` from the neutral parts of `harness/pkg/llm`.
6. Move provider-specific parts to `llm`.
7. Update `harness` to depend on `inference`, not `llm`.
8. Update `flow`, `cli`, and `swe`.

## Verification gates

Per phase:

```sh
GOWORK=off go test -race ./...
GOWORK=off go mod tidy
```

For modules with lint/security gates:

```sh
make lint
make secure
```

Cross-module smoke checks:

- `harness` builds without importing `github.com/looprig/llm`.
- `harness + inference` can run a fake or local HTTP inference client end-to-end.
- `inference` can call httptest-backed OpenAI-compatible, Anthropic-compatible, and
  Gemini-compatible routes using only stdlib plus `core`.
- `inference` can call a custom httptest API through caller-supplied request encoding,
  response/stream decoding, and routing.
- `inference` can stream from at least one non-SSE custom httptest API, such as NDJSON or a custom
  stream framer, without changing the generic transport.
- `inference` can invoke with request/response encoders that do not implement `StreamDecoder`; a
  stream call without a stream decoder fails before I/O with a typed unsupported-streaming error.
- `inference` maps non-2xx streaming responses to `APIError` before calling `StreamDecoder`, drains
  and closes those error bodies, and requires decoders/framers to close bodies they own on early
  errors.
- `inference` does not retry or replay `EncodedRequest.Body`; retry policy belongs above the
  generic transport.
- header merge tests prove route headers are applied first, encoder headers second, auth headers
  last, and later layers override earlier layers.
- `inference` accepts an unknown/custom `APIFormat` when a caller supplies explicit
  request/response/stream decoders and a router; `llm` still fails closed for unknown
  provider/format presets.
- `inference` preserves typed generic errors for network failures, API status failures, validation,
  missing generic credentials, and model/endpoint mismatches.
- `inference` structural model validation accepts unknown provider/API-format labels when explicit
  routing and codecs are supplied; `llm` presets still fail closed for unknown provider policy.
- `llm` imports `inference`, not the reverse.
- `inference` has no provider default endpoints, provider auth-policy table, or model catalogue.
- `llm` has no SDK-owned model catalogue; consumer-owned catalogues live in app/domain modules.
- `flow` imports `core/uuid`, not a private or duplicated UUID package.
- storage backends import `storage` only, not `harness` or `flow`.

## Open questions

- Whether generic HTTP presets such as LM Studio and OpenRouter should remain only in `llm/auto`
  or later get dedicated provider packages.
- Whether `llm/aci`, `llm/e2e`, and `llm/tee` should stay inside `llm` or later move to an
  optional provider-security module. Keep them in `llm` for this split unless dependency
  weight forces another seam.

## Release Sequencing

Tag modules bottom-up:

```text
core
storage + inference
llm + harness
apps
```

Use `go.work` for local multi-module development, but run release verification with `GOWORK=off`
so each module proves it resolves through tagged dependencies rather than workspace state.

## Decision summary

- Use `core`, `inference`, `llm`, `harness`, and `storage`.
- Rename `storekit` to `storage`.
- Keep `identity`, `event`, `command`, `tool`, `tools`, stores, and transcript in `harness`.
- Keep `harness` independent from `llm`.
- Make `inference`, not `llm`, the generic interface, HTTP, auth, route, wire, and codec layer.
- Keep the first migration mostly mechanical, then make semantic edits only where the split
  exposes real API decisions.
