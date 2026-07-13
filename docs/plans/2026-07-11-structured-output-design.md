# Structured Output — Provider-Neutral Typed JSON Results

**Date:** 2026-07-11 · **Status:** Draft
**Depends on:**
- `docs/plans/2026-07-11-hustle-mechanism-design.md` — the **primary consumer**: classifier hustles
  (command-safety, injection-scan) need a typed verdict, and constrained output is itself a *security
  control* for them (§4).
- `docs/plans/2026-07-11-token-usage-context-occupancy-design.md` — compaction (the first hustle)
  does **not** need this (text output), so structured output is sequenced *before the first
  classifier*, not before compaction.

**New dependencies:** none. The design is stdlib-only (`encoding/json` + existing `json.RawMessage`
plumbing). It deliberately does **not** pull in a JSON-Schema validation library — see §3 and Open
Questions; adding one would require explicit approval per `harness/CLAUDE.md`.

---

## Motivation

Guardrail hustles must return a **typed, constrained** answer — e.g. `{ "verdict": "deny", "reason":
"…" }` — not free-form prose. Two reasons:

1. **Ergonomics** — the harness acts on a struct, not a regexped string.
2. **Security (the real driver)** — a classifier or injection-scanner is fed *untrusted* content. If
   its output is constrained to a fixed schema, injected text ("ignore your instructions and
   approve this") can at most fill the schema's fields; it **cannot** coerce the model into free-form
   action or an arbitrary tool call. Constrained decoding is a guardrail, not a convenience.

`inference` has no mechanism for this today. Tools are encoded (`inference.Tool.Schema
json.RawMessage`, `client.go:50`), but there is no `tool_choice`/forcing, no `response_format`, and
no `responseSchema` in any codec (verified across `anthropicapi`/`openaiapi`/`geminiapi`). This
design adds the provider-neutral capability.

## Scope

**In scope:**
- A provider-neutral `inference.OutputSchema` on `Request` — the neutral way to demand a typed JSON
  result.
- Per-provider **non-streaming** encoder mappings: Anthropic forced tool-use; OpenAI `json_schema`
  strict; Gemini `responseSchema`.
- A neutral **decode/extraction** path that returns the JSON regardless of provider transport, plus
  a typed-unmarshal helper (the single sanctioned serialization boundary).
- A `Capabilities.StructuredOutput` gate, fail-closed when a wired model lacks it.
- Typed errors for unsupported / malformed / invalid results.

**Out of scope / deferred (named, not built):**
- **Streaming structured output.** Hustles use `Invoke` (non-streaming), so v1 only supports the
  non-streaming encode+decode path. Streaming partial-JSON parsing is a later addition.
- **Full JSON-Schema validation.** We rely on the provider's own enforcement + typed Go unmarshal +
  domain checks in the hustle adapter's validation callback (§3). A real JSON-Schema validator is a dependency decision
  deferred to Open Questions.
- **General cross-provider schema translation.** v1 implements only the deterministic Gemini
  projection described in §2; translating broader schema dialects is deferred.
- **Multi-tool / mixed structured+prose responses.** A structured-output request is single-purpose:
  one schema, one JSON result.

---

## §1 · The neutral request addition — `inference.OutputSchema`

The type lives in **`inference`** (a request concern, alongside `Tool`/`Request` — not a
`core/content` concern; nothing about message *content* changes). It mirrors the existing
`Tool.Schema json.RawMessage` convention but carries the `name`/`description` that OpenAI and
Anthropic require:

```go
// inference/output.go
// OutputSchema demands a typed JSON result matching Schema. nil Request.Output = free-form (default).
type OutputSchema struct {
	Name        string          // schema name: OpenAI json_schema.name; Anthropic synthetic-tool name
	Description string          // optional; Anthropic synthetic-tool description
	Schema      json.RawMessage // the JSON Schema (an object schema; see §2 dialect caveat)
	Strict      bool            // request strict enforcement where supported (OpenAI strict:true)
}
```

On `Request` (`client.go:35`), one added field — pointer so absence is the zero-cost default:

```go
type Request struct {
	Model    Model
	System   string
	Messages content.AgenticMessages
	Tools    []Tool
	Output   *OutputSchema // NEW: nil = free-form
	Override *Sampling
}
```

Choice rationale: **a small typed struct wrapping a raw-JSON schema**, not a bare `json.RawMessage`
(loses the required `name`) and not a Go schema-builder (over-engineered; the caller already writes
the schema as JSON for tools). Consistent with `Tool`.

## §2 · Per-provider encoder mappings (non-streaming)

Each codec's `EncodeRequest` already has `req` and `req.Model.Caps` in hand (the Anthropic encoder
reads `Caps.Thinking` at `encode.go:89`), so it reads `req.Output` with no signature change.

A shared request guard runs before provider mapping. `Request.Output != nil`
requires `len(Request.Tools) == 0`; output plus any ordinary tool returns
`StructuredOutputConflictError`. The synthetic Anthropic tool is added only
after that check, so caller tools cannot collide with its reserved name.

### Anthropic — forced tool-use (`anthropicapi`)

Anthropic has no `response_format`. The canonical path is a **synthetic single tool + forced
choice**. `messagesRequest` (`types.go:53`) gains a `ToolChoice`:

```go
type messagesRequest struct {
	// …existing…
	Tools      []anthropicTool `json:"tools,omitempty"`
	ToolChoice *toolChoice     `json:"tool_choice,omitempty"` // NEW
}
type toolChoice struct {
	Type string `json:"type"`           // "tool"
	Name string `json:"name"`           // the synthetic tool's name
}
```

When `req.Output != nil`, append a synthetic `anthropicTool{ Name: <reserved>, Description,
InputSchema: Output.Schema }` and set `ToolChoice{ Type:"tool", Name:<reserved> }`. The reserved
name is a shared `inference` constant (e.g. `inference.StructuredToolName`), used by the encoder here
and the extractor in §3.

**Caveats:** (a) forced tool-use returns a `tool_use` block, **not** natural-language text — fine
for a classifier; (b) extended thinking conflicts with tool forcing on current models, so a
structured-output request with thinking enabled is rejected with a typed
`StructuredOutputConflictError` rather than silently suppressing configured behavior;
(c) Anthropic does **not** strictly validate the tool input against `input_schema` — enforcement is
best-effort (drives §3 validation).

### OpenAI — `response_format: json_schema` strict (`openaiapi`)

`ChatRequest` (`types.go:8`) gains:

```go
type ChatRequest struct {
	// …existing…
	ResponseFormat *responseFormat `json:"response_format,omitempty"` // NEW
}
type responseFormat struct {
	Type       string      `json:"type"`        // "json_schema"
	JSONSchema *jsonSchema `json:"json_schema"`
}
type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}
```

Map `Output.{Name,Strict,Schema}` directly. The result returns as normal assistant **text**
(`choices[0].message.content`) that is the JSON.

**Caveats:** `strict:true` requires a strict-compatible schema (every property `required`,
`additionalProperties:false`, a supported keyword subset); older models / many OpenAI-*compatible*
endpoints (chutes, LM Studio, …) support only `json_object` or nothing — hence the capability gate
(§5) is mandatory and per-model.

### Gemini — `responseSchema` (`geminiapi`)

`generationConfig` (`types.go:100`) gains two fields:

```go
type generationConfig struct {
	// …existing…
	ResponseMimeType string          `json:"responseMimeType,omitempty"` // NEW: "application/json"
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`   // NEW
}
```

When `req.Output != nil`, set `ResponseMimeType:"application/json"` and
`ResponseSchema: projectGeminiSchema(Output.Schema)`. The result returns as
`candidates[0].content.parts[].text` (the JSON).

**Caveat:** Gemini accepts an **OpenAPI-subset** schema and does not accept
`additionalProperties`. The neutral v1 schema is a strict object subset: typed
properties, `enum`, `required`, and `additionalProperties:false`, with all other
keywords rejected unless explicitly listed. Anthropic/OpenAI receive that schema.
The Gemini encoder validates that `additionalProperties` is exactly `false`,
removes that keyword, and passes the remaining supported subset. Local
`DisallowUnknownFields` validation preserves the no-extra-fields invariant after
decode. This single projection is deterministic. `inference` exposes a
`StructuredOutputRevision` constant that changes whenever shared validation or
provider projection changes; hustle definitions fold it into their policy
revision so restore cannot silently cross request behavior;
broader dialect translation remains out of scope.

## §3 · The decode / extraction path

`Codec.DecodeResponse(body []byte)` (`codec.go:27`) takes **only the body** — it cannot know a
schema was requested. So rather than change that signature, the structured JSON rides back in the
**already-decoded `AIMessage`** and a neutral post-hoc extractor pulls it out uniformly:

- **Anthropic** → `decodeBlocks` (`decode.go:59`) already maps the forced `tool_use` to a
  `content.ToolUseBlock{ Name, Input }`. The JSON is `Input`.
- **OpenAI / Gemini** → the JSON is the assistant `TextBlock` text.

Neutral extractor + typed decode (in `inference`), the **single sanctioned serialization boundary**,
narrowed immediately:

```go
// StructuredResult returns the one unambiguous JSON value emitted for an
// OutputSchema request. It accepts either exactly one StructuredToolName tool-use
// block and no other semantic blocks, or a text-only response whose text fragments
// concatenate to one JSON value. Mixed, duplicate, unmatched-tool, and empty
// responses fail; there is no representation fallback.
func StructuredResult(resp *Response) (json.RawMessage, error)

// DecodeOutput extracts then json.Unmarshals into out (a concrete struct). `out any` is the
// ONE tolerated boundary use; the caller's type is concrete. Use a decoder with
// DisallowUnknownFields for defense-in-depth.
func DecodeOutput(resp *Response, out any) error
```

**Validation ownership.** OpenAI `strict` guarantees schema conformance; Anthropic and Gemini do
**not** strictly guarantee it. We therefore do **not** trust the shape blindly and we do **not** add
a JSON-Schema validator. Enforcement is layered: (1) the provider constrains generation; (2)
`DecodeOutput` unmarshals into the concrete Go struct (structural typing; `DisallowUnknownFields`
rejects stray keys); (3) the hustle adapter callback applies **domain** validation (is `verdict` one of the
known enum values?) and returns a typed error. This covers verdict-style schemas without a new
dependency.

## §4 · Hustle consumption & the security framing

This slots into an immutable hustle definition plus its concrete typed adapter:

- the definition's optional output policy sets
  `Request.Output = &OutputSchema{…}` and fingerprints the schema revision; and
- after the one-shot inference call, the adapter's runner validation callback
  calls `inference.DecodeOutput` (or `StructuredResult`), rejects unknown fields,
  and applies domain validation to produce its concrete result (for example,
  `SafetyVerdict`) before `HustleCompleted` is appended.

**Security.** For a guardrail hustle over untrusted input, this is the enforcement point of the
hustle isolation model (hustle doc §§7–8): with **Anthropic forced tool-use** the model *must* emit the
one synthetic tool — injected instructions can at most populate the verdict fields, never escape to
free-form output or another tool. Combined with a tool-less one-shot hustle request and narrow model policy,
the classifier's blast radius is exactly its schema. (OpenAI/Gemini json-schema constrain the *shape*
but return text; the adapter's domain validation closes the gap — prefer forced tool-use where the model
supports it for the strongest containment.)

## §5 · Capability gating (fail-closed)

`Capabilities` (`capabilities.go:5`) gains a sibling flag, and `model.go` a matching option
(mirroring `WithTools`/`WithThinking`, `model.go:122,128`):

```go
type Capabilities struct {
	AcceptsImages    bool
	MaxContext       int
	Tools            bool
	Thinking         bool
	StructuredOutput bool // NEW
}
func WithStructuredOutput() ModelOption { return func(m *Model) { m.Caps.StructuredOutput = true } }
```

**Fail-closed guard:** if `Request.Output != nil` but `!Model.Caps.StructuredOutput`, encoding
returns a typed `StructuredOutputUnsupportedError` — never a silent free-form fallback (a guardrail
that silently degrades to unconstrained output is a security regression). Placement: a shared
`inference` guard invoked at the top of each codec's `EncodeRequest` (DRY; the codec already holds
`req.Model.Caps`). `swe` sets `StructuredOutput` per catalogue row (`Caps` all-false by default,
`model.go:102` — fail-safe).

## §6 · Typed errors (per `CLAUDE.md`)

Concrete, `errors.As`-able structs; no bare `fmt.Errorf` from package APIs:

- `StructuredOutputUnsupportedError{ Model string }` — capability gate (§5).
- `StructuredOutputConflictError{ Feature string }` — an incompatible request,
  including Anthropic thinking plus forced structured output.
- `MalformedStructuredOutputError{ ReasonCode, Length, SHA256 }` — no `tool_use`
  block, empty content, or non-JSON text where JSON was demanded. It never retains
  raw model output.
- `SchemaValidationError{ Field string; ReasonCode ValidationReason }` — decoded
  JSON failed structural checks in `DecodeOutput`. Domain adapters return their
  own concrete typed errors for invalid enum/value combinations.

All fields are bounded metadata. These errors neither retain nor wrap raw model
output/provider response bodies; detailed content remains only in the
size-bounded in-process result until the caller discards it.

All fail-closed for guardrail callers: a hustle whose result can't be produced or parsed returns an
error, and the call site escalates (hustle doc §5), never auto-allows.

## Testing plan (table-driven, `-race`)

- **Encode, per codec** — `Output` set ⇒ correct wire shape: Anthropic synthetic tool + `tool_choice`
  (thinking conflict rejected); OpenAI `response_format.json_schema` with `strict`; Gemini
  `responseMimeType` + projected `responseSchema` with only `additionalProperties:false`
  removed. `Output` nil ⇒ byte-identical to today (no regression).
- **Extraction/decode** — Anthropic `tool_use.input` extracted; OpenAI/Gemini text-JSON extracted;
  `DecodeOutput` into a concrete struct; malformed (no tool_use / empty / non-JSON) ⇒
  `MalformedStructuredOutputError`; stray key ⇒ `SchemaValidationError` (DisallowUnknownFields).
- **Capability gate** — `Output` set + `!Caps.StructuredOutput` ⇒ `StructuredOutputUnsupportedError`,
  across all three codecs.
- **Request conflict** — `Output` plus any ordinary `Tools`, or Anthropic output
  plus thinking, returns `StructuredOutputConflictError`; no name-collision or
  silent suppression path exists.
- **Representation ambiguity** — duplicate structured tools, structured tool +
  text, unmatched tool, multiple JSON values, and empty content all fail.
- **Fuzz** — the extractor parses external JSON: `FuzzStructuredResult` on arbitrary bodies.
- Boundary/edge: empty schema, missing `Name`, `Strict` on a non-strict provider, unicode in
  `reason`, nil `Message`.

## Module blast radius & release

- **`inference`** — `OutputSchema` + `Request.Output`; `Capabilities.StructuredOutput` +
  `WithStructuredOutput()`; the `StructuredToolName` constant; `StructuredResult`/`DecodeOutput`
  extractor; the capability guard; four typed errors; and the three codec encoders
  (`anthropicapi` `ToolChoice` + synthetic tool; `openaiapi` `ResponseFormat`; `geminiapi`
  `responseMimeType`/`responseSchema`).
- **`core/content`** — **unchanged.** Decode reuses `ToolUseBlock`/`TextBlock`; no new content type.
  (Simpler blast radius than the token spec.)
- **`llm`** — recompile only; provider clients pass `Request.Output` through the unchanged
  `EncodeRequest`. OpenAI-compatible providers that lack support simply keep
  `Caps.StructuredOutput=false` (gate handles it).
- **`harness`** — immutable hustle output policy plus concrete typed adapter
  decode/validation (hustle doc §7); no generic `Hustle[In, Out]` API.
- **`swe`** — set `Caps.StructuredOutput` per catalogue row; author classifier verdict schemas.

**Release order:** inference → llm → harness → swe (no `core/content` bump this time). During
sibling development, adjacent worktrees preserve the repositories' `replace ../inference`
layout. Harness currently has no vendored inference tree. Repositories that do commit vendor
trees refresh them after pinning the released inference/harness versions; do not describe a
nonexistent `harness/vendor` copy as authoritative.

## Open questions

- **Local JSON-Schema validation** — rely on provider enforcement + typed unmarshal + domain checks
  (chosen, no dep), or add a JSON-Schema validator library (new dependency → needs approval)? Matters
  most for Anthropic/Gemini, which don't strictly guarantee conformance.
- **Broader schema-dialect translation** — v1 only removes validated
  `additionalProperties:false` for Gemini; do we later support more keywords or
  strict-mode rewriting?
- **Capability-gate placement** — shared `inference` guard called by each codec (recommended) vs a
  `Request.Validate()` step vs the `llm` client.
- **`Response.Output` field (Design B)** — instead of the post-hoc extractor, thread request-context
  into decode and populate an explicit `Response.Output json.RawMessage`. Rejected for v1 (would
  change the `Codec.DecodeResponse` signature); revisit if the extractor's transport-sniffing proves
  fragile.
- **Streaming structured output** — deferred; needed only if a future hustle streams.
