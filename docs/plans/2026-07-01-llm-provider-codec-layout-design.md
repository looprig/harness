# Design: `pkg/llm` — codec × provider layout & the `ModelSpec` split

Date: 2026-07-01
Status: **Proposed.** Staged rollout; Phase 0 is additive. ACI has landed in `main`
(merge `7272b94`); this design builds on it (not alongside an in-flight worktree) — see
§ Migration / rollout.

## Problem

`pkg/llm` conflates two independent things and can't express a third:

1. **APIFormat and provider are fused.** `openaiapi/{chutes,lmstudio}` nests providers
   *under* a codec, which reads as "chutes is a kind of OpenAI codec." It isn't — chutes
   *consumes* the OpenAI codec. Phala now routes through `aci`, but still composes an
   OpenAI-shaped codec internally. Adding an Anthropic-format provider has no clean home.
2. **`ModelSpec` mashes four concerns** into one flat struct passed inside every `Request`:
   connection (`Provider`/`BaseURL`/`APIKey`), model identity (`Model`/`AcceptsImages`),
   per-agent state (`System`), and sampling (`Temperature`/…). The connection and secret
   ride along on every turn even though they're fixed for the life of a client.
3. **There is no codec axis**, so a provider that offers *more than one* wire format
   (Bedrock: Converse + Anthropic-body; an upgraded LMStudio: OpenAI + Anthropic on
   different paths) cannot be modelled. Today the codec is implied by `Provider`, which is
   only true for single-codec providers.

## The five axes

A "provider" is not one thing. It decomposes into orthogonal axes; onboarding = compose them.

| Axis | What it is | Examples |
|---|---|---|
| **Codec (dialect)** | canonical `Request`/`Response` ↔ wire bytes (+ stream event schema) | `openaiapi`, `anthropicapi`, `bedrockconverse`, `gemini` |
| **Transport** | base URL, invoke/stream routes, stream *framing* | plain SSE, AWS eventstream |
| **Auth** | credential signing | API key header, Bearer, AWS SigV4, GCP ADC |
| **Confidential protocol** | TEE attest + E2EE seal/open, fail-closed | `aci` (Dstack), chutes ML-KEM |
| **Catalog / capabilities** | model id mapping, context window, images, tools, caching semantics | per-model rows |

**The load-bearing rule: the codec is *data*, not structure.** A provider offers a *set* of
codecs; a model/catalog row picks one via a `APIFormat` field. A provider earns a *package* only
when it has provider-specific *code* (SigV4, eventstream framing, attestation) — supporting N
codecs is N values of a field, not N packages.

Three words for three axes, kept distinct on purpose:
`openaiapi`/`anthropicapi` are **protocols**; `gpt`/`claude` are **models**;
`openai`/`bedrock`/`lmstudio`/`phala` are **providers**.

## Package layout

```
pkg/llm/                 domain types + interfaces + generic httpLLM (LLM, Codec, Authenticator, APIFormat, errors, catalog, stream)
pkg/llm/codec/
        ├── sse/         shared SSE framing (both dialects; hoisted from openaiapi/sse.go)
        ├── openaiapi/   OpenAI Chat Completions codec           (moved from pkg/llm/openaiapi)
        └── anthropicapi/ Anthropic Messages codec               (NEW, when first needed)
pkg/llm/aci/  tee/  e2e/  reusable confidential protocol + attestation primitives (building blocks)
pkg/llm/providers/
        ├── phala/       openaiapi + aci protocol + pinned Phala trust policy
        ├── chutes/      openaiapi + ML-KEM protocol
        └── bedrock/     {anthropicapi, bedrockconverse} + SigV4 + region→route map
pkg/llm/auto/            composition root: Provider/APIFormat → client; config-only providers = catalog rows
```

**Import direction — the discipline that prevents cycles** (honors CLAUDE.md "wire at the
composition root"):

```
codec/*      → imports llm             (never imports providers, never each other)
providers/*  → imports llm, codec/*    (may import several codecs; never each other)
aci          → imports llm, codec/openaiapi, tee, e2e
auto         → imports llm, codec/*, providers/*, aci     ← the ONLY sink
```

**Provider = package vs catalog row.** Package when it carries provider-specific *code or
pinned data*: `phala` (trust policy + aci wiring), `chutes` (bespoke protocol), `bedrock`
(SigV4 + dialect→route). Catalog row when it's base URL + creds + model list over an existing
codec: openrouter, together, groq, **lmstudio**. A package for those is empty boilerplate.

## Interfaces

`LLM` is unchanged — it stays the consumer-facing port:

```go
type LLM interface {
    Invoke(ctx context.Context, req Request) (*Response, error)
    Stream(ctx context.Context, req Request) (*StreamReader[content.Chunk], error)
}
```

New seams (defined in `pkg/llm`; concrete codecs satisfy `Codec` structurally, importing only
`llm` for the domain types):

```go
// APIFormat names a wire codec — the new axis. Carried as data on a Model.
type APIFormat string
const (
    APIFormatOpenAI          APIFormat = "openai"
    APIFormatAnthropic       APIFormat = "anthropic"
    APIFormatBedrockConverse APIFormat = "bedrock-converse"
)

// RequestMode is the typed encode mode (not a bool): the wire body differs because
// streaming sets "stream": true.
type RequestMode uint8
const ( RequestModeInvoke RequestMode = iota; RequestModeStream )

// Codec owns one wire dialect's JSON + stream-event *semantics*. It does NOT own wire
// *framing* — the transport de-frames the response (SSE lines / AWS eventstream, via the
// shared codec/sse helper) and hands the codec one event payload at a time.
type Codec interface {
    EncodeRequest(Request, RequestMode) ([]byte, error)   // typed mode, not a bool
    DecodeResponse([]byte) (*Response, error)             // non-streaming body → Response
    DecodeEvent([]byte) ([]content.Chunk, error)          // one de-framed stream event → chunks
}

// Authenticator mutates an outbound request to carry credentials. Orthogonal to dialect.
type Authenticator interface {
    Authorize(ctx context.Context, r *http.Request) error
}
// impls from pkg/llm/auth (all return llm.Authenticator):
//   auth.Key(auth.APIKey)            — Authorization: Bearer <key>
//   auth.Header(auth.APIKey, name)   — custom header (e.g. "x-api-key")
//   auth.None()
//   auth.SigV4(auth.SigV4Credentials, region, service)
// auth.APIKey / auth.SigV4Credentials are the named secret TYPES (see § Auth enforcement); the
// constructors wrap them — so the package has ONE `APIKey` (a type), not a type-and-func clash.
```

The non-confidential atom is a generic client = **codec × transport × auth** (illustrative;
unexported, constructed by `auto` and by provider packages):

```go
type httpLLM struct {
    codec Codec
    ep    Endpoint      // base URL + invoke/stream paths + framing
    auth  Authenticator
    hc    *http.Client
}
```

Confidential providers (`aci`, `chutes`) are **wire-level confidential transports**, not generic
decorators. They interpose *inside* the pipeline: encode via the codec, then re-parse the JSON to
seal specific fields in place, snapshot the cleartext for the receipt, POST, open the sealed
response, verify the receipt, and **only then** decode. They need byte-exact access to the codec's
*output*, so they compose with the codec at the wire level — they do not wrap a finished `llm.LLM`.

## What each provider is

| Provider | Codec set | Package? | Auth | Protocol |
|---|---|---|---|---|
| lmstudio | {`openaiapi`, `anthropicapi`} | **row** | none | — |
| openrouter / together / groq | {`openaiapi`} | **row** | Bearer key | — |
| phala | {`openaiapi`} | pkg | key | `aci` (reusable) |
| chutes | {`openaiapi`} | pkg | key | ML-KEM (its own) |
| bedrock | {`anthropicapi`, `bedrockconverse`} | pkg | SigV4 | — |

A single-codec provider makes the codec pick look invisible; Bedrock makes it explicit — same
machinery, `Model.APIFormat` selects.

## `ModelSpec` redesign

### Before (today) — one flat struct rides inside every `Request`

```go
type ModelSpec struct {
    Provider Provider; BaseURL string; APIKey string   // connection + secret
    Model string; System string; AcceptsImages bool    // model + agent
    Temperature *float64; TopP *float64; MaxTokens *int; Stop []string
    ThinkingBudget int; ReasoningEffort ReasoningEffort // two mechanisms, one intent
}
```

Problems: connection + secret are per-*client* yet travel per-*request*; no `APIFormat`;
`ThinkingBudget` (Anthropic `budget_tokens`, now deprecated) and `ReasoningEffort` (OpenAI) are
two dialect mechanisms for the same intent; `ThinkingBudget requires Temperature==1.0` is an
Anthropic rule baked into the neutral `Validate`.

### After — three concerns split by lifetime

1. **`Model`** — secret-free catalog entry. Gains **`APIFormat`** + a `Capabilities` block;
   sampling becomes intent-shaped.

```go
type Model struct {
    Provider Provider
    APIFormat  APIFormat          // NEW: which codec speaks to this model on this provider
    BaseURL  string
    Name     string           // provider-specific model id
    Origin   Origin           // NEW: provenance — catalog (verified caps) vs custom (user-asserted). Zero value = custom (fail-safe).
    Caps     Capabilities     // NEW: gating data, never sent on the wire
    Sampling Sampling         // defaults; per-call overrides live on Request
}

type Capabilities struct {
    AcceptsImages bool
    MaxContext    int
    Tools         bool
    Thinking      bool
}

// Sampling is dialect-neutral intent. Each Codec maps it to its wire mechanism.
type Sampling struct {
    Temperature *float64
    TopP        *float64
    MaxTokens   *int
    Stop        []string
    Effort      Effort        // replaces ThinkingBudget + ReasoningEffort
}

type Effort string
const (
    EffortNone   Effort = ""       // disabled
    EffortLow    Effort = "low"
    EffortMedium Effort = "medium"
    EffortHigh   Effort = "high"
    EffortMax    Effort = "max"
)
```

`Effort` normalization is **capability-driven, not a blanket codec rule.** `openaiapi` maps it to
`reasoning_effort`; `anthropicapi` maps it per the *model's* capability/version — adaptive thinking
+ `effort` on current models, `budget_tokens` on older ones that still accept it, or nothing when
the model has no thinking — so the codec consults `Capabilities` (whose `Thinking` may need to be
richer than a bool — mechanism/version), not a fixed rule. The `Temperature==1.0` coupling and
other dialect-specific validity rules likewise move **into the codec** that owns them; the neutral
layer only validates dialect-neutral contradictions.

2. **Credentials** bind at client construction — through `auto.New(model, auth)` or the typed
   provider constructor — never on the `Model`, the catalog, or the `Request`. The old
   `Model.Spec(apiKey,…)` materialization is gone; the secret reaches the client only as an
   `Authenticator` (`auth.Key` / `auth.SigV4` / `auth.None`).

3. **`Request`** carries the per-turn model selection + messages; only the *connection + secret*
   bind at construction:

```go
type Request struct {
    Model    Model                     // secret-free descriptor for THIS turn: APIFormat, Name, Sampling, Caps
    System   string                    // per-agent prompt
    Messages content.AgenticMessages
    Tools    []Tool
    Override *Sampling                  // optional per-call sampling overrides; nil = use Model.Sampling
}
```

The client is **connection-bound**, not model-bound: it binds `Provider` + connection endpoint
(`BaseURL` for plain HTTP providers, provider-specific endpoint config for non-URL providers) + auth
once, and serves any model on that same connection. `req.Model.Provider` and the request's endpoint
identity must match the bound client; a mismatch returns a typed `*ModelMismatchError` before
encoding or network I/O. The codec is selected per turn from `req.Model.APIFormat`.

The `Model` rides on `Request` because the codebase already works this way — one ACI client keys its
per-model attested-session cache on the requested model (`aci/client.go`), and `loop.Config` holds
the model beside the client for fingerprint/restore (`config.go:22`, `config_fingerprint.go:63`).
Only the **secret never rides the request** (it binds at construction); the `Model`'s connection
fields are redundant-with-the-client and secret-free, and exist for validation/fingerprint/catalog
continuity. `Response.Model` echoes the resolved model.

The connection-mismatch guard is a typed, `errors.As`-able error carrying both sides:

```go
type ModelMismatchError struct {   // req.Model's connection ≠ the bound client's — fail closed, pre-I/O
    BoundProvider, RequestProvider Provider
    BoundEndpoint, RequestEndpoint string
}
```

### `Model.Validate()`

Validate at the boundary before constructing a client or encoding a request; returns a typed
`*ValidationError`:
- `Provider` is a known value, and `APIFormat` is in that provider's **supported set** — fail
  closed on an unsupported pair (`bedrock` ∈ {anthropic, converse}, `phala` ∈ {openai}, …).
- `Name` is non-empty.
- URL validation is provider-specific:
  - HTTP BaseURL providers (`lmstudio`, `phala`, `chutes`, OpenRouter/Together/Groq rows) require a
    non-empty `BaseURL` that parses as `https://`, with **one explicit exception**: `http://` is
    allowed only for `127.0.0.1`/`localhost` (local LM Studio) and rejected for any other host — no
    plaintext to a remote gateway.
  - Provider-routed clients such as Bedrock may leave `BaseURL` empty; their constructors validate
    the provider-specific endpoint inputs (region/service/route) instead.
- `OriginCustom` models validate identically; only their `Caps` are trusted less (asserted, not
  verified).

### Fields needed to make a call

| Bound once, at client construction | Per turn, on `Request` |
|---|---|
| `Provider` + endpoint (`BaseURL` or provider-specific config), from the seed `Model` | `Model` descriptor; its connection fields must match the bound client |
| `Authenticator` (`auth.Key` / `auth.SigV4` / `auth.None`) | `System` |
| trust policy (confidential providers) | `Messages` |
| | `Tools` (optional) |
| | `Override *Sampling` (optional) |

`Capabilities` are read locally for gating (e.g. a TUI deciding whether to allow image
attachments) — never serialized.

### Consumer impact (loop / session)

The split changes what first-party consumers hold. `loop.Config` today carries one
`Model llm.ModelSpec` (connection + model + `System`, sent every turn) beside the constructed
`Client`. Post-split it separates the three lifetimes:

```go
type Config struct {
    Client llm.LLM   // connection-bound, built via auto.New at the composition root
    Model  llm.Model // secret-free descriptor put on every Request — NO System, NO secret
    System string    // per-agent prompt; put on every Request AND hashed into the fingerprint
    // …other fields unchanged…
}
```

`FingerprintFrom` moves off `ModelSpec`: `ModelID = cfg.Model.Name`,
`SystemPromptRev = hexSHA256(cfg.System)` (was `cfg.Model.System`). Each turn the loop assembles
`llm.Request{Model: cfg.Model, System: cfg.System, Messages: …}`. Foreign loops
(`pkg/foreignloop`) that read the model/system follow the same field moves. Same fingerprint
inputs, new field homes — restore stays stable across the refactor.

## Using it going forward

```go
// 1. Declare a model — secret-free catalog entry. APIFormat + caps are data on the row.
glm := llm.Model{
    Provider: llm.ProviderPhala,
    APIFormat:  llm.APIFormatOpenAI,          // the Phala gateway speaks the OpenAI wire
    BaseURL:  "https://api.phala.network/v1",
    Name:     "zai-org/GLM-4.6",
    Caps:     llm.Capabilities{MaxContext: 200_000, Tools: true, Thinking: true},
    Sampling: llm.Sampling{MaxTokens: ptr(4096), Effort: llm.EffortHigh},
}

// 2. Build a connection-bound client at the composition root. auto binds the connection
//    (glm.Provider + glm.BaseURL + auth), validates the auth kind vs the provider, and adds
//    attestation for confidential providers. The client serves any model on that same connection.
client, err := auto.New(glm, auth.Key(key))   // connection-bound; codec picked per turn

// 3. Make a call — the Model rides on the Request; no secret does.
resp, err := client.Invoke(ctx, llm.Request{
    Model:    glm,
    System:   "You are a helpful agent.",
    Messages: msgs,
    Tools:    tools,
})

// streaming — identical shape
sr, err := client.Stream(ctx, llm.Request{Model: glm, System: sys, Messages: msgs})
```

**Multi-codec provider — the codec is data, chosen per model:**

```go
claudeOnBedrock := llm.Model{
    Provider: llm.ProviderBedrock, APIFormat: llm.APIFormatAnthropic,
    Name: "anthropic.claude-...",         // region carried by SigV4 creds
}
llamaOnBedrock := llm.Model{
    Provider: llm.ProviderBedrock, APIFormat: llm.APIFormatBedrockConverse,
    Name: "meta.llama-...",
}
// The connection-bound bedrock client picks the codec per turn from req.Model.APIFormat;
// providers/bedrock maps APIFormat → (route + framing).
```

**One provider, two dialects — upgraded LMStudio (no new package, two rows):**

```go
localOAI := llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatOpenAI,
    BaseURL: "http://localhost:1234/v1",  Name: "..."}                 // /v1/chat/completions
localAnthropic := llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatAnthropic,
    BaseURL: "http://localhost:1234",     Name: "..."}                 // /v1/messages
```

**Confidential providers — same call site, stronger guarantees:**

```go
client, _ := auto.New(phalaModel, auth.Key(key))  // fail-closed, attested llm.LLM (aci protocol)
resp, _ := client.Invoke(ctx, llm.Request{Model: phalaModel, System: sys, Messages: msgs}) // buffered-until-verified
// swapping phala→chutes is a different connection: rebuild the client via auto.New (a
// req.Model.Provider mismatch against the bound client fails closed with *ModelMismatchError).
// Only the Request shape is unchanged.
```

## Custom (unlisted) models & generating the catalog

### Curated rows — generated offline where possible

A catalog row is a plain `Model` constructor with `Origin: OriginCatalog` and filled caps:

```go
func GLM46Phala() Model {
    return Model{
        Provider: ProviderPhala, APIFormat: APIFormatOpenAI,
        BaseURL:  "https://api.phala.network/v1", Name: "zai-org/GLM-4.6",
        Origin:   OriginCatalog,
        Caps:     Capabilities{MaxContext: 200_000, Tools: true, Thinking: true},
        Sampling: Sampling{MaxTokens: ptr(4096), Effort: EffortHigh},
    }
}
```

For discovery-capable providers these rows are **generated offline**, not hand-typed. A
`//go:generate` step calls the provider's `Discovery.Models(ctx)` (OpenRouter / Anthropic /
LM Studio native) and emits reviewed Go source; phala/chutes rows stay hand-authored. The
generator fills informational caps (context window, pricing) from the wire but leaves **gating
caps (`AcceptsImages`) for a human to confirm** — the endpoint may not report them, and we never
let the wire widen a permission. The inference request path never calls `/models`.

### Custom models — the escape hatch for anything not in the catalog

Users must be able to run a model we haven't curated yet, without waiting on a catalog PR.
`CustomModel` builds the same `Model` type: `Origin` defaults to custom, capabilities default
**fail-safe** and are opt-in.

```go
// CustomModel forces the core things we cannot guess (provider, format, endpoint, name);
// endpoint is a BaseURL for HTTP providers and may be "" for provider-routed clients
// such as Bedrock. Everything else defaults safe/unknown and is opted into via options.
func CustomModel(p Provider, f APIFormat, baseURL, name string, opts ...ModelOption) Model

m := llm.CustomModel(
    llm.ProviderPhala, llm.APIFormatOpenAI,
    "https://api.phala.network/v1", "some-org/Brand-New-70B",
    llm.WithMaxContext(128_000),   // optional; unknown → conservative default
    llm.WithTools(),               // opt in to what you know it supports
    // images NOT opted in → AcceptsImages stays false (fail-closed)
)
client, err := auto.New(m, auth.Key(key))   // identical path & auth enforcement; only the caps are user-asserted
```

- **Because `auto.New` takes a `Model`, not a name lookup, custom models need zero special
  plumbing** — a `CustomModel` value flows through the exact same construction and auth path as a
  curated row.
- **`APIFormat` is the one field the user MUST supply** — we can't infer whether an unknown
  endpoint speaks OpenAI or Anthropic. Provider + endpoint string + name are the other required
  positional args; provider-specific validation decides whether the endpoint string may be empty
  (Bedrock) or must be a valid BaseURL (HTTP providers).
- **`Origin` provenance, fail-safe zero value.** A raw `Model{}` literal or `CustomModel(...)` is
  `OriginCustom`; only the curated constructors set `OriginCatalog`. Downstream can trust catalog
  caps and treat custom caps as asserted — a TUI warns "capabilities unverified," and gating stays
  conservative.
  ```go
  type Origin uint8
  const ( OriginCustom Origin = iota; OriginCatalog )   // zero = custom → conservative by default
  ```
- **Naming:** **`CustomModel`** (recommended — "bring your own / user-defined," widely understood).
  Alternative: `UnlistedModel`, if you prefer the contrast with the curated *catalog listing*.
  Avoid `Unknown`/`Dynamic` — the user *does* know the model; it just isn't in our list, so those
  names describe our ignorance rather than the user's intent.

## Auth enforcement — compile-time where the provider is known, fail-closed otherwise

"Is auth present for this provider?" is answerable at two different times, and *when* is decided
by *when the provider is chosen* — not by a quality tradeoff.

**Provider known at compile time → the constructor signature is the contract (compile-time).**
Each provider's `New` demands exactly the credential type it needs, using named credential
types from `pkg/llm/auth`. Omit it, or pass the wrong kind, and it does not compile:

```go
// package auth
type APIKey string   // named type — a base URL can't be passed where a key belongs
type SigV4Credentials struct { AccessKeyID, SecretAccessKey, SessionToken string }

func phala.New(baseURL string, key auth.APIKey, p aci.Policy) (llm.LLM, error)             // key REQUIRED
func chutes.New(baseURL string, key auth.APIKey) (llm.LLM, error)                          // key REQUIRED
func lmstudio.New(baseURL string) (llm.LLM, error)                                         // NO credential param — the type says "no auth"
func bedrock.New(creds auth.SigV4Credentials, region string) (llm.LLM, error)              // SigV4 creds, not an APIKey
```

This is the strongest guarantee Go offers: the signature encodes the requirement, and the
credential *type* is provider-specific, so you can't hand an `APIKey` to Bedrock either. Most
consumers know their provider at compile time (you write code that talks to Phala) — they get
this for free by calling the typed constructor directly. Constructors still empty-check
(`key == ""` → typed error) to close the "passed an empty string" gap.

**Provider chosen at runtime → the factory validates and fails closed (runtime).**
`auto.New` dispatches on a runtime `Provider` value, so compile-time is impossible *by
definition* — the provider isn't known until the program runs. It consults the provider's
declared auth requirement and errors before any network call:

```go
type AuthKind string
const ( AuthNone AuthKind = "none"; AuthAPIKey AuthKind = "api_key"; AuthSigV4 AuthKind = "sigv4" )

// generalizes today's Provider.RequiresKey() from bool to an auth kind (multi-auth-ready)
func (p Provider) RequiredAuth() (AuthKind, error)

type AuthRequiredError struct { Provider Provider; Kind AuthKind }  // typed, errors.As-able
```

`auto.New` returns `*AuthRequiredError` when a key-requiring provider gets an empty credential —
the same fail-closed posture as the existing `RequiresKey`, whose doc-comment already notes "a
bare default-false would fail open — the bug this method exists to prevent."

**Kill the fail-open path structurally.** The generic client takes a **required**
`Authenticator`; there is no zero-value fall-through to "no auth." "No auth" is `auth.None()` —
a visible, deliberate value, never a default:

```go
func newHTTP(c Codec, ep Endpoint, a Authenticator) *httpLLM   // `a` required; nil is a programmer error, not "open"
```

Net: compile-time safety exactly when it's achievable (provider known statically), fail-closed
runtime validation when the provider is dynamic, and no silent "no auth" default anywhere.

## Migration / rollout

ACI has landed in `main` (merge `7272b94`), so this builds on it rather than coexisting with an
in-flight worktree. Phase 0 is additive; the structural moves follow in Phase 1.

**Phase 0 — additive, non-breaking (do now): new symbols only, no edits to existing structs.**
- Introduce the new types/consts as fresh declarations: `APIFormat`, `Effort`, `Sampling`,
  `Capabilities`, `Origin`, `RequestMode`, the `Codec` and `Authenticator` interfaces,
  `AuthRequiredError`, `ModelMismatchError`, and the `pkg/llm/auth` package (`APIKey` /
  `SigV4Credentials` types + `Key` / `Header` / `None` / `SigV4` constructors).
- Add `Provider.RequiredAuth()` alongside the existing `RequiresKey()` (additive method; don't
  remove `RequiresKey` yet).
- **Do NOT touch `ModelSpec`, `Model`, or `Request`.** No fields are added to any existing struct,
  so nothing breaks and there are **no transitional duplicate fields** (e.g. `Temperature` *and*
  `Sampling.Temperature`). The reshape — folding `Temperature`/`MaxTokens` into `Sampling`, adding
  `APIFormat`/`Origin`/`Caps` to `Model`, moving `Model` onto `Request`, retiring `ModelSpec`, and
  the `loop.Config`/`FingerprintFrom` moves — all happens together in Phase 1.

**Phase 0.5 — relocate the codec (its own step; mechanical but breaking):**
- Move `pkg/llm/openaiapi` → `pkg/llm/codec/openaiapi` and hoist SSE framing to `codec/sse`. It
  touches every importer of the codec (~13 files today): `aci/client.go`, the `openaiapi/chutes`
  and `openaiapi/lmstudio` sub-packages, and the codec's own in-package tests. The in-dir tests
  relocate *with* the package, so the real external edits are `aci` + `chutes` + `lmstudio` (the
  `auto` factory imports those sub-packages, not the codec directly). Land it as one mechanical
  rename commit, or ship a thin `pkg/llm/openaiapi` shim re-exporting `codec/openaiapi` and migrate
  callers incrementally.
- Add `codec/anthropicapi` when the first Anthropic-format provider is actually needed.

**Phase 1 — structural refactor (on `main`):**
- Introduce `pkg/llm/providers/`; dissolve `openaiapi/lmstudio` into a catalog row and
  `openaiapi/chutes` into `providers/chutes`; move `aci` (and optionally `chutes`) under a
  `confidential/` grouping if that reads better with two siblings.
- Split `ModelSpec` per this doc: the connection + secret bind at construction; the secret-free
  `Model` descriptor rides on `Request` (connection-bound client — see § ModelSpec redesign → After).
- **Extract `providers/phala` (decided — option B).** ACI landed as `auto` →
  `aci.New(spec.BaseURL, spec.APIKey, aci.DefaultPhalaPolicy())`, with `DefaultPhalaPolicy()`
  inside `pkg/llm/aci/policy.go`. This phase moves those pinned trust anchors **out of `aci`**
  into a new `providers/phala` package that composes `aci` + `openaiapi` and owns the policy, so
  the reusable `aci` protocol becomes truly provider-agnostic; `auto` then wires
  `ProviderPhala → phala.New(...)`. Forcing trigger regardless: a second Dstack gateway using `aci`.

**Phase 2 — exercise the multi-codec path:** add `providers/bedrock` + `codec/bedrockconverse`
(or reuse `anthropicapi`) to prove `Model.APIFormat` selection end-to-end.

## Open questions

1. **Where config-only providers live** — catalog rows in `pkg/llm` (default, extends the
   existing `catalog.go`) vs a declarative registry `auto` reads.
2. **`bedrockconverse` vs `anthropicapi` for Claude-on-Bedrock** — whether to add a Converse
   codec at all, or standardize on the Anthropic-Messages-compatible Bedrock surface and skip
   Converse until a non-Anthropic Bedrock model is actually needed (YAGNI).
3. **`Capabilities.Thinking` granularity** — a bool may be too coarse for the capability-driven
   `Effort`→thinking mapping (§ ModelSpec redesign → After); it may need a mechanism/version enum.
   Decide when `codec/anthropicapi` lands.

**Resolved:** *Model-bound vs connection-bound* — the client is **connection-bound**; the `Model`
descriptor rides on `Request`, and mismatched request connection fields fail closed before network
I/O (matches ACI's per-model session cache and `loop.Config`). See § ModelSpec redesign → After.
