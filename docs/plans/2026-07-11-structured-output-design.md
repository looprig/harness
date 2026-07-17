# Structured Output — Provider-Neutral Typed JSON Results

**Date:** 2026-07-11 · **Revised:** 2026-07-17 · **Status:** Draft
**Depends on:**
- `docs/plans/2026-07-11-hustle-mechanism-design.md` — the primary one-shot consumer: classifier
  hustles (command-safety, injection-scan) need typed verdicts, and constrained output is itself a
  security control.
- `docs/plans/2026-07-11-token-usage-context-occupancy-design.md` — compaction does not need typed
  output, so structured output is sequenced before the first classifier, not before compaction.

**New dependencies:** none. The design is stdlib-only (`encoding/json` plus existing
`json.RawMessage` plumbing). It deliberately does not add a JSON-Schema validation package; that
would require explicit approval under `AGENTS.md`.

---

## Motivation

Guardrail hustles must return a typed, constrained answer such as
`{"verdict":"deny","reason":"…"}`, not free-form prose. Agent loops also need a way to use ordinary
tools during a turn and still finish with a typed result.

There are two separate provider capabilities:

1. **Structured output** — the model can produce a direct assistant result constrained by a schema.
2. **Structured output with tools** — the same request may expose ordinary tools while retaining the
   schema as the contract for the eventual direct assistant result.

The second capability must not be inferred from the first plus tool calling. Anthropic and supported
OpenAI models allow both settings in one request. Gemini exposes the native combination only on
Gemini 3-series models and currently labels it preview. OpenAI-compatible endpoints vary even when
they accept the individual fields.

Client-executed tools are still multi-step: an intermediate response requests a tool, Harness runs
it and submits its result, and a later response produces the typed final value. “Both settings are
present” does not mean one response atomically contains a client tool result and the final JSON.

## Scope

**In scope:**
- A provider-neutral `inference.OutputSchema` on `Request`.
- `Capabilities.StructuredOutput` and the independent
  `Capabilities.StructuredOutputWithTools` gate.
- Native Anthropic, OpenAI, and Gemini encodings for both `Invoke` and `Stream`.
- A neutral extraction and typed-decode path for complete responses and completed streamed
  `AIMessage` values.
- A Harness loop strategy that uses native combined output where advertised and a reserved terminal
  tool otherwise.
- Tool-less, one-shot hustle consumption using native structured output.
- Provider-neutral finish-reason handling so truncated, filtered, and tool-use responses are not
  mistaken for valid final output.
- Typed failures for unsupported capabilities, incompatible requests, malformed results, and schema
  validation.

**Out of scope / deferred:**
- Incremental partial-JSON exposure. Loop streams are supported, but validation occurs only after
  clean EOF when the `AIMessage` and finish reason are complete.
- Automatic repair/retry after malformed structured output. v1 fails the hustle or loop turn; a
  later retry design must make attempts, token usage, history, and retry limits explicit.
- Full JSON-Schema validation. Provider constraint plus strict Go decoding plus domain validation is
  the v1 enforcement stack.
- Broad cross-provider schema translation. v1 accepts a deliberately small portable object subset
  and performs only the deterministic Gemini projection described below.
- Provider-executed built-in tools. `Request.Tools` continues to mean Harness/client-executed tools.
- Ordinary tools inside a hustle. A tool-using auxiliary model is a bounded loop, not a hustle.

---

## §1 · Neutral request types

`OutputSchema` lives in `inference`, beside `Request` and `Tool`; message content does not change.
It follows the existing `Tool.Schema json.RawMessage` convention while carrying the name needed by
OpenAI and the reserved terminal-tool fallback.

```go
// OutputSchema demands one typed JSON result matching Schema.
// nil Request.Output preserves today's free-form behavior.
type OutputSchema struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      bool // request strict enforcement where the native API exposes it
}
```

`Request` gains `Output` and a small tool-choice control. Required choice is used only by the
terminal-tool fallback; the zero value preserves today's automatic tool selection.

```go
type ToolChoice uint8

const (
	ToolChoiceAuto ToolChoice = iota
	ToolChoiceRequired
)

type Request struct {
	Model      model.Model
	System     string
	Messages   content.AgenticMessages
	Tools      []Tool
	Output     *OutputSchema
	ToolChoice ToolChoice
	Override   *model.Sampling
}
```

All three canonical codecs map `ToolChoiceRequired`: Anthropic `tool_choice:{"type":"any"}`,
OpenAI `tool_choice:"required"`, and Gemini
`toolConfig.functionCallingConfig.mode:"ANY"`. The codecs omit the field for `ToolChoiceAuto`.
The fallback does not rely on a provider accepting only one call: mixed or duplicate terminal calls
are rejected locally before any ordinary tool executes.

### Request invariants

- `Output == nil` leaves request behavior unchanged.
- `Output != nil` requires `Model.Caps.StructuredOutput`.
- `Output != nil && len(Tools) > 0` additionally requires
  `Model.Caps.StructuredOutputWithTools`.
- `StructuredOutputWithTools` implies both `StructuredOutput` and `Tools`; model validation rejects
  contradictory capability sets.
- A request using `ToolChoiceRequired` must expose at least one tool.
- `Output.Name` is non-empty, bounded, and cannot equal the reserved terminal-tool name.
- `Output.Schema` must be one JSON object in the portable schema subset.

The subset has one shared owner in `inference`:

```go
func ValidateOutputSchema(output OutputSchema) error

// StructuredOutputRevision changes whenever shared schema validation,
// provider projection, extraction, or terminal-tool semantics change.
const StructuredOutputRevision = "structured-output/v1"
```

The shared request guard calls `ValidateOutputSchema` before every provider codec. The validator
parses the serialization boundary, immediately narrows it to typed recursive schema nodes, and
enforces the v1 dialect: an object root; known scalar/object/array types; `properties`; `items`;
type-compatible primitive `enum` values; `required`; optional descriptions; and
`additionalProperties:false` on every object. Every declared property must be required, matching
the common strict subset. Unknown keywords, references/composition, tuple arrays, constraints such
as `minimum`/`maxLength`, duplicate or unknown required names, and non-object roots are rejected
with `SchemaValidationError`.

This is schema-*definition* validation, not full validation of generated instances. Generated data
still follows the layered decode and domain-validation path in §4.
Consumers include `StructuredOutputRevision` in immutable policy fingerprints so restore cannot
silently cross a change in structured-output behavior while retaining the same application schema.

The direct inference API never silently rewrites an unsupported native combined request. It returns
a typed error. Harness performs the explicit terminal-tool transformation described in §5 before it
constructs the inference request.

## §2 · Native provider mappings

The same mapping is used for non-streaming and streaming requests. Streaming changes only the
transport framing; structured output is validated after the stream completes.

### Anthropic — `output_config.format`

Anthropic structured output is native. The existing `outputConfig` already carries reasoning
effort; it gains an independent `Format` member rather than replacing effort or synthesizing a tool.

```go
type outputConfig struct {
	Effort string        `json:"effort,omitempty"`
	Format *outputFormat `json:"format,omitempty"`
}

type outputFormat struct {
	Type   string          `json:"type"` // "json_schema"
	Schema json.RawMessage `json:"schema"`
}
```

When `Request.Output != nil`, the codec sets `output_config.format`. If ordinary tools are present,
it leaves them present only after the combined-capability guard succeeds. An intermediate response
may end with `stop_reason:"tool_use"`; the direct structured JSON is expected only on the later
terminal response. Anthropic returns the native final value as assistant text.

The old synthetic forced-tool mapping is not the native Anthropic implementation. The reserved
terminal tool is solely a Harness portability fallback (§5).

### OpenAI Chat Completions — `response_format.json_schema`

This design intentionally targets Chat Completions because the current `openaiapi` codec and `llm`
provider wrappers already use that request/stream shape. A future Responses API integration is a
separate codec/API-format addition; it can expose the same neutral `OutputSchema` and capability
contracts without changing this design.

`ChatRequest` gains the native response-format and tool-choice fields:

```go
type ChatRequest struct {
	// existing fields
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	ToolChoice      string          `json:"tool_choice,omitempty"`
}

type responseFormat struct {
	Type       string      `json:"type"` // "json_schema"
	JSONSchema *jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}
```

Map `Output.{Name,Strict,Schema}` directly. Tools remain on the same request when
`StructuredOutputWithTools` is true. Tool calls are intermediate; the terminal JSON arrives as
assistant text with a stop finish reason.

`strict:true` requires the supported strict subset: required properties and
`additionalProperties:false`, among other keyword restrictions. Model catalogue rows must advertise
both structured-output flags explicitly. OpenAI-compatible providers default both flags to false
until verified against their actual endpoint.

### Gemini — `responseJsonSchema`

The current Gemini JSON-Schema field is `responseJsonSchema`, not the older OpenAPI-only
`responseSchema` spelling:

```go
type generationConfig struct {
	// existing fields
	ResponseMIMEType   string          `json:"responseMimeType,omitempty"`
	ResponseJSONSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
}
```

When `Request.Output != nil`, set `responseMimeType:"application/json"` and
`responseJsonSchema:projectGeminiSchema(Output.Schema)`. The final JSON is returned in text parts.

Gemini 3-series rows may advertise `StructuredOutputWithTools` while the feature remains preview.
Older Gemini rows may advertise `StructuredOutput` and `Tools` separately but must leave the
combined flag false; Harness then selects the terminal-tool fallback.

The portable v1 schema remains a strict object subset: typed properties, nested object/array shapes,
`enum`, `required`, and `additionalProperties:false`. Shared `ValidateOutputSchema` validates that
subset before codec selection. The Gemini projection therefore does not own common validation: it
removes only the already-validated `additionalProperties:false` keyword where the Gemini dialect
rejects it. Strict local result decoding preserves the no-extra-fields invariant. The same
projection is applied to the reserved terminal tool's input schema, identified by its non-colliding
internal name. Broader dialect translation remains deferred.

## §3 · Capability gating

Capabilities are model/endpoint facts, not provider-name guesses:

```go
type Capabilities struct {
	AcceptsImages             bool
	Tools                     bool
	Thinking                  bool
	StructuredOutput          bool
	StructuredOutputWithTools bool
}

func WithStructuredOutput() ModelOption
func WithStructuredOutputWithTools() ModelOption
```

`WithStructuredOutputWithTools` sets `Tools`, `StructuredOutput`, and
`StructuredOutputWithTools` together. Direct struct construction is still validated so the combined
flag cannot exist without its prerequisites.

Catalogue policy:

| Model/endpoint class | StructuredOutput | StructuredOutputWithTools |
|---|---:|---:|
| Verified Anthropic model with native format | true | true when verified |
| Verified OpenAI model | true | true when verified |
| Gemini 3 model with preview combination enabled | true | true |
| Older Gemini structured-output model | true | false |
| Unverified OpenAI-compatible/custom endpoint | false | false |

The zero value is fail-safe. Neither `inference` nor `llm` derives the combined flag from
`StructuredOutput && Tools`.

At the top of every codec encoder:

- native output without `StructuredOutput` returns `StructuredOutputUnsupportedError`;
- native output plus tools without `StructuredOutputWithTools` returns
  `StructuredOutputWithToolsUnsupportedError`;
- there is never a silent free-form or terminal-tool fallback inside a codec.

## §4 · Result extraction, finish reasons, and validation

Native structured output returns text on all three current mappings. The terminal fallback returns
the JSON as the input of one reserved tool call. A neutral extractor supports both complete response
forms without making callers inspect provider transports.

```go
const StructuredOutputToolName = "_looprig_final_output"

func StructuredResult(resp *Response) (json.RawMessage, error)
func StructuredMessageResult(msg *content.AIMessage) (json.RawMessage, error)
func DecodeOutput(resp *Response, out any) error
func DecodeMessageOutput(msg *content.AIMessage, out any) error
```

The `out any` parameters are the two explicit serialization boundaries; each immediately narrows to
the caller's concrete struct. Decoding uses `json.Decoder.DisallowUnknownFields`, requires exactly
one JSON value, and rejects trailing data.

Extraction accepts exactly one of:

- text-only semantic output whose text fragments concatenate to one JSON value; or
- exactly one `StructuredOutputToolName` call whose input is one JSON object, with no ordinary tool
  calls or semantic text.

Thinking blocks may accompany either representation but are never part of the result. Empty,
duplicate, mixed, unmatched-tool, and multiple-JSON representations fail closed.

`Response` gains provider-neutral `FinishReason stream.FinishReason`, matching the existing stream
result. All non-streaming decoders populate it. `runStep` retains the stream result's finish reason
alongside the assembled `AIMessage` instead of currently reading usage alone.

Finish handling:

- `Length` and `ContentFilter` are terminal failures even if partial JSON happens to parse.
- `ToolUse` must contain at least one complete tool call.
- `Stop` with ordinary tool calls is invalid.
- `Stop` or `Unknown` with no calls may be considered final only after structured extraction and
  local validation succeed. Unknown remains tolerated for compatible endpoints that omit a reason;
  validation still provides the safety boundary.

Validation is layered:

1. The provider constrains native output or terminal-tool arguments.
2. The extractor validates the transport representation and JSON framing.
3. Strict Go decoding rejects type mismatches and unknown fields.
4. The consuming adapter enforces required/non-zero fields and domain rules, such as checking the
   verdict enum. Go decoding alone does not implement JSON-Schema `required`.

The implementation does not claim full local JSON-Schema validation. That remains an explicit
dependency decision.

## §5 · Harness consumer behavior

### Hustles: native and tool-less

A hustle remains a one-shot `Invoke` with no tool registry, permission gate, or continuation loop.
Its immutable output policy sets `Request.Output`; the bound model must advertise
`StructuredOutput`. After invocation, the adapter checks finish reason, calls `DecodeOutput`, applies
domain validation, and only then appends `HustleCompleted`.

A hustle definition that requests ordinary tools, with or without structured output, is invalid.
Trusted Harness code may gather inputs before invoking the hustle. Model-directed tool use requires
a separately designed bounded auxiliary loop.

For untrusted classifier input, native constrained output plus local domain validation is the
containment boundary: injected text can populate allowed fields but cannot authorize an action by
escaping into an unvalidated representation. Any production or validation failure follows the
hustle's fail-closed escalation policy.

### Loops: choose native combined output or a terminal tool

A loop definition may carry an immutable final-output policy. The effective model is selected before
each turn, so the runtime chooses a strategy from that model's capabilities at the turn boundary and
keeps it fixed for the turn.

Decision table:

| Final output requested | Ordinary tools | Effective capability | Strategy |
|---|---:|---|---|
| no | either | n/a | current free-form loop |
| yes | no | `StructuredOutput` | native `Request.Output` |
| yes | no | `Tools` but no native structured output | reserved terminal-output tool |
| yes | yes | `StructuredOutputWithTools` | native `Request.Output` plus tools |
| yes | yes | `Tools` but no combined flag | reserved terminal-output tool |
| yes | either | no usable native or tool path | fail before inference |

For a native combined turn, every inference step carries the same `Output` and ordinary `Tools`.
Ordinary tool calls continue through the existing permission and `RunBatch` path. A no-tool terminal
step is validated as the final structured result before it is committed as `TurnDone`.

If a candidate final step is malformed, truncated, content-filtered, ambiguous, or fails typed/domain
validation, v1 ends the turn immediately with `TurnFailed` carrying the applicable structured-output
error. The invalid final step is never appended or committed; already committed ordinary tool
steps remain in history. Harness does not issue a hidden repair prompt or retry. A future bounded
repair policy must separately specify its maximum attempts, usage accounting, staged-history form,
and durable events before it can be enabled.

For the fallback, Harness transforms the policy before constructing the request:

```text
Request.Output     = nil
Request.Tools      = ordinary tools + _looprig_final_output(Output.Schema)
Request.ToolChoice = ToolChoiceRequired
```

The reserved tool is an internal control frame, not an invokable capability:

- its name is rejected from declared, injected, and external tool registries;
- it is intercepted immediately after `ToolUses()` and before tool limits, permission gates,
  `RunBatch`, or any `ToolStarted` event;
- a sole valid terminal call ends the turn;
- terminal plus ordinary calls, duplicate terminal calls, terminal plus semantic text, or malformed
  arguments fail the turn before any ordinary call executes;
- ordinary calls without the terminal call follow the existing bounded execution path;
- the terminal call does not consume an ordinary-tool call/iteration allowance.

The raw terminal `tool_use` is not committed, because that would create an unpaired tool call in
conversation history. After validation, Harness replaces the control frame with a final
`AIMessage` containing the compact JSON as one `TextBlock`, preserving usage, and commits that
message through the normal final-step handshake. This gives native and fallback turns the same
durable representation and the same `TurnDone.Message` contract.

The fallback may reduce parallelism on providers that choose one required function at a time, but it
does not weaken atomicity: if a provider emits the terminal function alongside action functions,
Harness rejects the entire batch before executing anything.

## §6 · Typed errors

Public failures are concrete and `errors.As`-able:

- `StructuredOutputUnsupportedError{Model string}` — native output was requested without the base
  capability, or a loop had neither a native nor terminal-tool strategy.
- `StructuredOutputWithToolsUnsupportedError{Model string}` — a direct inference request combined
  native output and tools without the explicit combined capability.
- `StructuredOutputConflictError{Feature string}` — invalid required-tool request, reserved-name
  collision, mixed terminal/action calls, or a provider-specific incompatibility.
- `StructuredOutputFinishError{Reason stream.FinishReason}` — length, filtering, or contradictory
  finish metadata.
- `MalformedStructuredOutputError{ReasonCode, Length, SHA256}` — missing, empty, ambiguous, or
  non-JSON output. It never retains raw model output.
- `SchemaValidationError{Field string, ReasonCode ValidationReason}` — strict Go decoding or shared
  structural validation failed. Domain adapters return their own typed value errors.

All diagnostic fields are bounded metadata. Errors neither retain nor wrap raw output or provider
response bodies. Guardrail callers fail closed.

## Testing plan

All Go tests run with `-race`; table-driven cases cover shared shapes.

### `inference`

- Capability invariants and model options, including the combined helper setting both prerequisites.
- Shared `ValidateOutputSchema` and request guard: recursive valid subset, every-property-required,
  enum type compatibility, unknown keyword, unsupported constraint/composition, malformed schema,
  output-only base gate, native combined gate, required choice without tools, and reserved-name
  collision.
- Anthropic encode: native `output_config.format`; preservation of existing `output_config.effort`;
  native format plus ordinary tools; required tool choice; nil output byte-equivalent to today.
- OpenAI encode: `response_format.json_schema`; combined tools; required tool choice; nil output
  unchanged.
- Gemini encode: `responseMimeType` via Go field `ResponseMIMEType`, plus `responseJsonSchema`;
  combined tools on an advertised row;
  required tool choice; deterministic response and reserved-tool schema projection.
- Non-streaming decode maps stop, tool-use, length, and content-filter finish reasons into
  `Response.FinishReason` for every codec.
- Extraction from native text and terminal-tool input; strict concrete decode; all ambiguous, empty,
  unmatched, mixed, trailing-data, type-mismatch, and unknown-field cases fail. Adapter tests prove
  missing required/domain fields also fail.
- Streaming codec tests prove native output fields are sent in stream mode and terminal metadata
  survives clean EOF.
- `FuzzStructuredResult` and `FuzzStructuredMessageResult` cover arbitrary external JSON/block
  combinations.

### `llm`

- Provider client tests capture encoded requests and prove `Output`, ordinary tools, and required
  tool choice survive each wrapper/extension path.
- Invoke and Stream tests preserve the new non-streaming and existing streaming finish metadata.
- OpenAI-compatible catalogue tests leave both capabilities false unless explicitly verified.
- Refresh committed `inference` vendor content after pinning the released inference version.

### `harness`

- Hustle output-only native request, typed decode, domain rejection, and ordinary-tool definition
  rejection.
- Loop decision table for output-only native, native combined, terminal fallback, and unsupported
  model.
- Native loop: ordinary tool calls continue; final valid JSON commits; malformed, length, and
  filtered finals produce typed `TurnFailed`, do not commit the invalid step, retain prior committed
  tool steps, and trigger no repair inference.
- Fallback loop: ordinary call executes; sole terminal call becomes a text-only final message;
  reserved tool never reaches permission/runner events or counters.
- Fail-secure fallback cases: terminal plus action, duplicate terminal, text plus terminal,
  malformed input, reserved-name collision, and cancellation before final commit.
- Restore/fingerprint tests include schema revision and selected output policy; a restored definition
  cannot silently cross output behavior.

## Module blast radius and release

- **`inference`** — request/output types, tool-choice control, both capability flags and model
  options, request validation, finish reason on `Response`, structured extractors, typed errors, and
  all three native codec mappings.
- **`core/content`** — unchanged. Native and fallback final values are `TextBlock`; existing
  `ToolUseBlock` carries the fallback control frame only while the step is staged.
- **`llm`** — wrapper/extension and fail-closed zero-capability tests, inference version pin, and
  vendor refresh. The module has no model catalogue; concrete composition roots opt verified model
  rows into the new flags.
- **`harness`** — immutable hustle output policy; immutable loop final-output policy; per-turn
  strategy selection; terminal-tool construction/interception; finish-aware final validation; policy
  fingerprinting; inference version pin; and vendor refresh.
- **`swe`** — verified per-model capability rows and concrete classifier/final-result schemas.

**Release order:** inference → llm → harness → swe. During sibling development, adjacent worktrees
preserve local `replace ../inference` layouts. Both llm and Harness commit vendored inference trees;
each refreshes its vendor directory after pinning the released inference version (or from the sibling
replace during coordinated pre-release development).

## Open questions

- **Full local JSON-Schema validation** — remain with provider constraint plus strict typed/domain
  validation, or approve a validator dependency later?
- **Broader schema dialects** — when should the portable subset or Gemini projection grow?
- **Provider-native optimization coverage** — once the fallback is stable, which exact model rows
  have enough integration evidence to enable `StructuredOutputWithTools`?
- **Bounded repair** — if fail-hard malformed finals prove too brittle, define a visible one-retry
  policy with explicit budget, usage, transcript, and event semantics rather than silently retrying.
- **Explicit output on events** — v1 keeps compact JSON in `TurnDone.Message`; add a separate typed
  event field only if consumers demonstrate they cannot use the message extractor cleanly.
