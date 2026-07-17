# Structured Output Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** Add provider-neutral structured JSON output to `inference`, preserve it through `llm`, and support native or terminal-tool typed completion in Harness hustles and loops.

**Architecture:** `inference` owns schema-definition validation, capability gates, native provider encodings, finish reasons, and result extraction. `llm` proves its provider-specific wrappers preserve those fields and refreshes its vendored inference tree. Harness stores immutable output policies: hustles always use native one-shot output, while loops select native output (including native tools+output when advertised) or a reserved terminal-output tool at the turn boundary.

**Tech Stack:** Go 1.26, stdlib `encoding/json`, existing `github.com/looprig/core/content`, existing `github.com/looprig/inference` codecs and streams, Harness actor runtimes. No new dependency.

**Source design:** `docs/plans/2026-07-11-structured-output-design.md`

**Repositories/worktrees:**

- `inference`: `.worktrees/structured-output/inference`
- `llm`: `.worktrees/structured-output/llm`
- `harness`: `.worktrees/structured-output/harness`
- Shared unmodified `core` and `storage` are symlinked into `.worktrees/structured-output/` so existing `replace ../core` and `replace ../storage` directives resolve.

**Development rules:**

- Every production change follows red → green → refactor. Each task records the focused failing and passing command in its commit message or handoff.
- Use `GOWORK=off`. Before vendor refreshes, use `GOFLAGS=-mod=mod` in `llm` and `harness` so tests resolve the sibling inference worktree. After refresh, use each repository's default vendored mode.
- Run Go tests with `-race`; build with `CGO_ENABLED=0 go build -trimpath`.
- Do not add dependencies.
- Preserve unrelated changes in the original checkout; all implementation happens in the worktrees above.

**Review cadence (explicit user override):** Do not dispatch spec/code reviewers after each task. After every phase, or after five tasks since the previous review (whichever comes first), dispatch one spec-compliance reviewer followed by one code-quality reviewer over the cumulative phase diff. A repair subagent fixes all findings, then the corresponding reviewer re-checks. Do not start the next phase with open findings.

---

## Phase 1 — Neutral inference contracts

### Task 1: Add structured-output model capabilities

**Repository:** `inference`

**Files:**
- Modify: `model/capabilities.go`
- Modify: `model/model.go`
- Modify: `model/model_test.go`

**Step 1: Write failing tests**

Add table-driven cases proving:

- `WithStructuredOutput()` sets only `Caps.StructuredOutput`.
- `WithStructuredOutputWithTools()` sets `Tools`, `StructuredOutput`, and `StructuredOutputWithTools`.
- `Model.Validate()` rejects a directly constructed capability set with `StructuredOutputWithTools=true` when either prerequisite is false.
- `Clone()` preserves both flags.

Expected public shape:

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

**Step 2: Verify red**

Run: `GOWORK=off go test -race ./model -run 'Test.*StructuredOutput|TestModelValidate'`

Expected: compile failure for missing fields/options, then a behavioral failure for contradictory direct capabilities.

**Step 3: Implement minimally**

Add the fields/options and a typed `model.ValidationError{Field:"Caps.StructuredOutputWithTools"}` invariant in `Model.Validate`.

**Step 4: Verify green**

Run: `GOWORK=off go test -race ./model`

**Step 5: Commit**

```bash
git add model/capabilities.go model/model.go model/model_test.go
git commit -m "feat(model): add structured output capabilities"
```

### Task 2: Define output schema and validate the portable subset

**Repository:** `inference`

**Files:**
- Create: `output.go`
- Create: `output_test.go`
- Create: `output_fuzz_test.go`
- Create: `structured_errors.go`

**Step 1: Write failing tests**

Cover valid nested object/array schemas and these failures: empty/malformed JSON, non-object root, missing/invalid name, oversized name/schema, object without `additionalProperties:false`, property absent from `required`, duplicate/unknown required name, missing array `items`, invalid scalar type, type-incompatible enum, unknown keyword, `$ref`/composition, tuple items, and `minimum`/`maxLength`.

Expected API:

```go
const StructuredOutputToolName = "_looprig_final_output"

type OutputSchema struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      bool
}

func (o OutputSchema) Clone() OutputSchema
func ValidateOutputSchema(output OutputSchema) error
```

Implement recursive parsing with a typed serialization DTO:

```go
type schemaNode struct {
	Type                 string                     `json:"type"`
	Description          string                     `json:"description,omitempty"`
	Properties           map[string]json.RawMessage `json:"properties,omitempty"`
	Items                json.RawMessage            `json:"items,omitempty"`
	Enum                 []json.RawMessage          `json:"enum,omitempty"`
	Required             []string                   `json:"required,omitempty"`
	AdditionalProperties *bool                      `json:"additionalProperties,omitempty"`
}
```

Use `DisallowUnknownFields`, exactly one JSON value, bounded byte size/depth/property counts, ASCII provider-safe names, and typed raw enum validation. Never use `map[string]any`.

**Step 2: Verify red**

Run: `GOWORK=off go test -race . -run 'TestValidateOutputSchema|TestOutputSchemaClone'`

Expected: compile failure because the API does not exist.

**Step 3: Implement minimally**

Return bounded, `errors.As`-able `SchemaValidationError` values that do not retain schema bytes.

**Step 4: Verify green and fuzz seed corpus**

Run: `GOWORK=off go test -race . -run 'TestValidateOutputSchema|TestOutputSchemaClone'`

Run: `GOWORK=off go test -run=^$ -fuzz=FuzzValidateOutputSchema -fuzztime=30s .`

**Step 5: Commit**

```bash
git add output.go output_test.go output_fuzz_test.go structured_errors.go
git commit -m "feat(inference): validate portable output schemas"
```

### Task 3: Add request tool choice and structured capability guards

**Repository:** `inference`

**Files:**
- Modify: `client.go`
- Modify: `client_test.go`
- Modify: `structured_errors.go`

**Step 1: Write failing tests**

Add table-driven tests for:

- nil output leaves the request valid;
- output without `StructuredOutput` returns `StructuredOutputUnsupportedError`;
- output plus ordinary tools without `StructuredOutputWithTools` returns `StructuredOutputWithToolsUnsupportedError`;
- the combined flag accepts both fields;
- `ToolChoiceRequired` without tools returns `StructuredOutputConflictError`;
- invalid schemas and reserved-name collisions fail before codec encoding.

Expected additions:

```go
type ToolChoice uint8

const (
	ToolChoiceAuto ToolChoice = iota
	ToolChoiceRequired
)

func ValidateRequestFeatures(req Request) error
```

`Request` gains `Output *OutputSchema` and `ToolChoice ToolChoice` without changing zero-value behavior.

**Step 2: Verify red**

Run: `GOWORK=off go test -race . -run 'TestValidateRequestFeatures|TestRequestStructuredFields'`

**Step 3: Implement minimally**

`ValidateRequestFeatures` calls `ValidateOutputSchema` exactly once when output is present and performs only provider-neutral capability/request checks.

**Step 4: Verify green**

Run: `GOWORK=off go test -race .`

**Step 5: Commit**

```bash
git add client.go client_test.go structured_errors.go
git commit -m "feat(inference): gate structured request features"
```

### Task 4: Add finish-aware structured result extraction

**Repository:** `inference`

**Files:**
- Modify: `client.go`
- Create: `structured_result.go`
- Create: `structured_result_test.go`
- Extend: `output_fuzz_test.go`
- Modify: `structured_errors.go`

**Step 1: Write failing tests**

Cover:

- text fragments concatenate into one JSON value;
- a sole `_looprig_final_output` call returns its input;
- thinking may accompany either representation;
- empty, malformed, trailing JSON, duplicate terminal calls, terminal+text, terminal+ordinary call, ordinary-only call, nil response/message/block, and typed-nil blocks fail;
- `DecodeOutput`/`DecodeMessageOutput` use `DisallowUnknownFields` and reject trailing values;
- response finish `Length`/`ContentFilter` fails even when JSON parses;
- `ToolUse` without calls and `Stop` with calls fail; `Unknown` plus valid final JSON is accepted.

Expected API:

```go
type Response struct {
	Message      *content.AIMessage
	Usage        *content.Usage
	Model        string
	FinishReason stream.FinishReason
}

func StructuredResult(resp *Response) (json.RawMessage, error)
func StructuredMessageResult(msg *content.AIMessage) (json.RawMessage, error)
func DecodeOutput(resp *Response, out any) error
func DecodeMessageOutput(msg *content.AIMessage, out any) error
```

**Step 2: Verify red**

Run: `GOWORK=off go test -race . -run 'TestStructured|TestDecode.*Output'`

**Step 3: Implement minimally**

Use bounded metadata-only `MalformedStructuredOutputError` and `StructuredOutputFinishError`; hash malformed raw output rather than retaining it.

**Step 4: Verify green and fuzz**

Run: `GOWORK=off go test -race . -run 'TestStructured|TestDecode.*Output'`

Run: `GOWORK=off go test -run=^$ -fuzz=FuzzStructuredMessageResult -fuzztime=30s .`

**Step 5: Commit**

```bash
git add client.go structured_result.go structured_result_test.go output_fuzz_test.go structured_errors.go
git commit -m "feat(inference): extract typed structured results"
```

### Phase 1 review checkpoint

Dispatch cumulative inference spec review, then code-quality/security review. Repair and re-review before Phase 2.

---

## Phase 2 — Native provider codecs

### Task 5: Encode and decode Anthropic native structured output

**Repository:** `inference`

**Files:**
- Modify: `codec/anthropicapi/types.go`
- Modify: `codec/anthropicapi/encode.go`
- Modify: `codec/anthropicapi/decode.go`
- Modify: `codec/anthropicapi/encode_test.go`
- Modify: `codec/anthropicapi/decode_test.go`
- Modify: `codec/anthropicapi/stream_test.go`

**Step 1: Write failing tests**

Assert `output_config.format:{type:"json_schema",schema:...}`, coexistence with existing `output_config.effort`, ordinary tools retained on combined-capable requests, `tool_choice:{type:"any"}` for required choice, capability failures before marshal, identical fields in stream mode, and non-streaming finish mapping for stop/tool_use/max_tokens/refusal.

**Step 2: Verify red**

Run: `GOWORK=off go test -race ./codec/anthropicapi -run 'Test.*Structured|Test.*Finish|Test.*ToolChoice'`

**Step 3: Implement minimally**

Call `inference.ValidateRequestFeatures` at encoder entry. Extend existing `outputConfig` with `Format`; do not synthesize a tool for native output. Add `StopReason` to the response DTO and reuse the package's stream `mapFinishReason`.

**Step 4: Verify green**

Run: `GOWORK=off go test -race ./codec/anthropicapi`

**Step 5: Commit**

```bash
git add codec/anthropicapi
git commit -m "feat(anthropic): add native structured output"
```

### Task 6: Encode and decode OpenAI Chat Completions structured output

**Repository:** `inference`

**Files:**
- Modify: `codec/openaiapi/types.go`
- Modify: `codec/openaiapi/encode.go`
- Modify: `codec/openaiapi/decode.go`
- Modify: `codec/openaiapi/encode_test.go`
- Modify: `codec/openaiapi/decode_test.go`
- Modify: `codec/openaiapi/stream_test.go`

**Step 1: Write failing tests**

Assert `response_format.json_schema` name/strict/schema, coexistence with ordinary tools, `tool_choice:"required"`, capability failures, stream request preservation, nil-output byte-shape compatibility, and non-streaming finish mapping for stop/tool_calls/length/content_filter.

**Step 2: Verify red**

Run: `GOWORK=off go test -race ./codec/openaiapi -run 'Test.*Structured|Test.*Finish|Test.*ToolChoice'`

**Step 3: Implement minimally**

Add typed response-format DTOs and call the shared guard at `BuildChatRequest` entry so direct callers (`aci`, Chutes) receive the same validation.

**Step 4: Verify green**

Run: `GOWORK=off go test -race ./codec/openaiapi`

**Step 5: Commit**

```bash
git add codec/openaiapi
git commit -m "feat(openai): add native structured output"
```

### Task 7: Encode and decode Gemini structured output

**Repository:** `inference`

**Files:**
- Modify: `codec/geminiapi/types.go`
- Modify: `codec/geminiapi/encode.go`
- Modify: `codec/geminiapi/decode.go`
- Modify: `codec/geminiapi/encode_test.go`
- Modify: `codec/geminiapi/decode_test.go`
- Modify: `codec/geminiapi/stream_test.go`

**Step 1: Write failing tests**

Assert `generationConfig.responseMimeType:"application/json"`, `responseJsonSchema`, recursive removal of validated `additionalProperties:false`, unchanged native tools on combined requests, terminal-tool schema projection by reserved name, `toolConfig.functionCallingConfig.mode:"ANY"`, capability failures, and non-streaming finish mapping.

**Step 2: Verify red**

Run: `GOWORK=off go test -race ./codec/geminiapi -run 'Test.*Structured|Test.*Projection|Test.*Finish|Test.*ToolChoice'`

**Step 3: Implement minimally**

Use Go field `ResponseMIMEType`. `BuildGenerateContentRequest` calls the shared guard. Projection accepts only an already validated schema, returns a deep copy, and changes no ordinary tool schema.

**Step 4: Verify green**

Run: `GOWORK=off go test -race ./codec/geminiapi`

**Step 5: Commit**

```bash
git add codec/geminiapi
git commit -m "feat(gemini): add native structured output"
```

### Task 8: Verify inference as one provider-neutral module

**Repository:** `inference`

**Files:**
- Modify only tests or implementation needed to resolve cross-package failures.

**Step 1: Run the full suite**

Run: `GOWORK=off go test -race ./...`

Expected: all packages pass. Treat failures as regressions; do not weaken tests.

**Step 2: Run format/build/security**

Run: `GOWORK=off make fmt`

Run: `GOWORK=off CGO_ENABLED=0 go build -trimpath ./...`

Run: `GOWORK=off make secure`

**Step 3: Run parser fuzz targets**

Run: `GOWORK=off go test -run=^$ -fuzz=FuzzValidateOutputSchema -fuzztime=30s .`

Run: `GOWORK=off go test -run=^$ -fuzz=FuzzStructuredMessageResult -fuzztime=30s .`

**Step 4: Commit any verification-only fixes**

```bash
git add -u
git commit -m "test(inference): verify structured output integration"
```

Skip the commit if verification required no changes.

### Phase 2 review checkpoint

Review cumulative inference Phase 2 changes against the spec, then review codec quality/security and wire compatibility. Repair and re-review.

---

## Phase 3 — `llm` preservation and vendoring

### Task 9: Prove provider wrappers preserve structured fields

**Repository:** `llm`

**Files:**
- Modify: `aci/client_test.go`
- Modify: `providers/chutes/client_test.go` and/or `providers/chutes/encode_test.go` (create if focused coverage is clearer)
- Modify: `providers/bedrock/body_test.go`
- Modify: `providers/gemini/client_test.go`
- Modify: `auto/apiformat_e2e_test.go`

**Step 1: Write failing tests against sibling inference**

Use `GOFLAGS=-mod=mod`. Capture request bodies and prove:

- ACI and Chutes retain OpenAI `response_format`, tools, and required choice through their extension/encryption paths.
- Bedrock retains Anthropic `output_config.format` while rewriting only model/version fields.
- Gemini retains `responseJsonSchema`, tools, and required choice.
- unverified/custom models with zero flags fail before HTTP when output is requested.

**Step 2: Verify red**

Run focused package tests with `GOWORK=off GOFLAGS=-mod=mod go test -race ...`; failures must show missing fields or stale vendored APIs, not fixture errors.

**Step 3: Implement only necessary wrapper fixes**

Embedding `openaiapi.ChatRequest` and existing codec delegation should carry most fields automatically. Do not duplicate codec logic in `llm`.

**Step 4: Verify green**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./aci ./providers/chutes ./providers/bedrock ./providers/gemini ./auto`

**Step 5: Commit**

```bash
git add aci providers auto
git commit -m "test(llm): preserve structured output requests"
```

### Task 10: Refresh llm's vendored inference and verify

**Repository:** `llm`

**Files:**
- Modify: `vendor/github.com/looprig/inference/**`
- Modify: `vendor/modules.txt` if generated content changes.

**Step 1: Refresh**

Run: `GOWORK=off GOFLAGS=-mod=mod make vendor`

Confirm no nested `.git` remains and the vendor diff contains the expected inference files only.

**Step 2: Verify vendored mode**

Run: `GOWORK=off make fmt`

Run: `GOWORK=off make test`

Run: `GOWORK=off CGO_ENABLED=0 go build -trimpath ./...`

Run: `GOWORK=off make secure`

**Step 3: Commit**

```bash
git add vendor
git add -u
git commit -m "chore(llm): vendor structured output inference"
```

### Phase 3 review checkpoint

Review `llm` against the spec and cumulative inference API, then review wrapper/vendor quality. Repair and re-review.

---

## Phase 4 — Harness public policies, hustles, and native loops

During this phase use `GOWORK=off GOFLAGS=-mod=mod` so Harness resolves the sibling inference worktree.

### Task 11: Add immutable hustle output policy

**Repository:** `harness`

**Files:**
- Modify: `pkg/hustle/definition.go`
- Modify: `pkg/hustle/errors.go`
- Modify: `pkg/hustle/definition_test.go`
- Modify: `pkg/hustle/descriptor_test.go`
- Modify: `pkg/rig/hustle_fingerprint_test.go`
- Modify exhaustive event descriptor fixtures affected by the new descriptor fields.

**Step 1: Write failing tests**

Add `WithOutputSchema(inference.OutputSchema)` and prove deep-copy immutability, duplicate/invalid option failures, descriptor schema name/digest/revision identity, policy revision drift, secret-free descriptor JSON, bound accessor cloning, and unchanged definitions when absent.

Store raw schema only in private definition state. The public durable descriptor carries bounded identity fields, not raw schema bytes.

**Step 2: Verify red**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./pkg/hustle ./pkg/rig -run 'Test.*Output|Test.*Descriptor|Test.*Fingerprint'`

**Step 3: Implement minimally**

Expose `OutputSchema() (*inference.OutputSchema, bool)` on `BoundDefinition`; every return is a clone. Fold `inference.StructuredOutputRevision` and schema digest into policy identity.

**Step 4: Verify green**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./pkg/hustle ./pkg/rig ./pkg/event`

**Step 5: Commit**

```bash
git add pkg/hustle pkg/rig pkg/event
git commit -m "feat(hustle): add immutable output schemas"
```

### Task 12: Use native structured output in hustle execution

**Repository:** `harness`

**Files:**
- Modify: `internal/hustleruntime/execution.go`
- Modify: `internal/hustleruntime/errors.go`
- Modify: `internal/hustleruntime/execution_test.go`
- Modify: `internal/hustleruntime/advanced_test.go`
- Modify: `internal/hustleruntime/output_reason_test.go`
- Modify: `internal/hustleruntime/execution_fuzz_test.go`

**Step 1: Write failing tests**

Prove the one-shot request carries `Output` and never tools; unsupported models fail before client I/O; stop/unknown valid JSON succeeds; length/content-filter/tool-use finishes fail at `StageOutput`; `inference.StructuredResult` accepts only canonical text output; usage and domain `ValidateResult` behavior remain intact; no retry occurs.

**Step 2: Verify red**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./internal/hustleruntime -run 'Test.*Structured|Test.*Output|Test.*Finish'`

**Step 3: Implement minimally**

Pass the bound output clone into `invoke`; replace local shape parsing with the inference extractor while retaining Harness byte limits and existing domain validator/finalizer ordering.

**Step 4: Verify green and fuzz**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./internal/hustleruntime`

Run: `GOWORK=off GOFLAGS=-mod=mod go test -run=^$ -fuzz=FuzzProviderOutputBoundary -fuzztime=30s ./internal/hustleruntime`

**Step 5: Commit**

```bash
git add internal/hustleruntime
git commit -m "feat(hustle): request native structured output"
```

### Task 13: Add immutable loop final-output policy

**Repository:** `harness`

**Files:**
- Modify: `pkg/loop/definition.go`
- Modify: `pkg/loop/errors.go`
- Modify: `pkg/loop/definition_test.go`
- Modify: `pkg/rig/fingerprint.go`
- Modify: `pkg/rig/fingerprint_test.go`

**Step 1: Write failing tests**

Add `loop.WithOutputSchema(inference.OutputSchema)` and prove schema validation/deep copy, duplicate option rejection, reserved produced-tool-name rejection, loop `PolicyRevision` drift, frozen/live fingerprint agreement, bound clone access, and mode-independent policy identity.

**Step 2: Verify red**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./pkg/loop ./pkg/rig -run 'Test.*Output|Test.*PolicyRevision|Test.*Fingerprint'`

**Step 3: Implement minimally**

Expose `OutputSchema() (*inference.OutputSchema, bool)` on `BoundDefinition`. Keep the schema loop-wide; the effective mode model determines transport strategy each turn.

**Step 4: Verify green**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./pkg/loop ./pkg/rig`

**Step 5: Commit**

```bash
git add pkg/loop pkg/rig
git commit -m "feat(loop): add immutable final output policy"
```

### Task 14: Select and encode the per-turn output strategy

**Repository:** `harness`

**Files:**
- Modify: `internal/loopruntime/config.go`
- Modify: `internal/loopruntime/turn.go`
- Create: `internal/loopruntime/output.go`
- Create: `internal/loopruntime/output_test.go`
- Modify: `internal/loopruntime/config_test.go` or the nearest existing config tests.

**Step 1: Write failing decision-table tests**

Cover free-form, output-only native, tools+native combined, tools+terminal fallback, tool-only fallback when native output is absent, and unsupported. Prove strategy freezes for the turn and every continuation request has the same shape.

Expected private enum:

```go
type outputStrategy uint8

const (
	outputStrategyNone outputStrategy = iota
	outputStrategyNative
	outputStrategyTerminalTool
)
```

**Step 2: Verify red**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./internal/loopruntime -run 'Test.*OutputStrategy|Test.*InferenceRequest'`

**Step 3: Implement minimally**

Resolve once before the step loop. Native requests carry `Output` plus ordinary tools. Fallback requests carry no `Output`, append the reserved inference tool, and set `ToolChoiceRequired`.

**Step 4: Verify green**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./internal/loopruntime -run 'Test.*OutputStrategy|Test.*InferenceRequest'`

**Step 5: Commit**

```bash
git add internal/loopruntime/config.go internal/loopruntime/turn.go internal/loopruntime/output.go internal/loopruntime/output_test.go internal/loopruntime/*config*test.go
git commit -m "feat(loopruntime): select structured output strategy"
```

### Task 15: Propagate finish reasons and validate native final steps

**Repository:** `harness`

**Files:**
- Modify: `internal/loopruntime/step.go`
- Modify: `internal/loopruntime/step_test.go`
- Modify: `internal/loopruntime/turn.go`
- Modify: `internal/loopruntime/turn_test.go` and focused finish tests.
- Modify: `pkg/event/errors.go`

**Step 1: Write failing tests**

Prove `stepResult` retains terminal `FinishReason`; length/content-filter fail before commit; tool-use without calls and stop with calls fail; native stop/unknown valid JSON commits `TurnDone`; malformed JSON returns typed `TurnFailed`; prior committed tool steps remain; invalid final step and `StepDone` do not commit; no repair call occurs.

**Step 2: Verify red**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./internal/loopruntime -run 'Test.*Finish|Test.*Structured.*Final|Test.*Malformed.*Final'`

**Step 3: Implement minimally**

Replace `terminalUsage` with a helper that clones the full stream terminal result. Validate native final output before appending `ts.msgs` or calling `commitStep`. Surface metadata-only typed event errors.

**Step 4: Verify green**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./internal/loopruntime ./pkg/event`

**Step 5: Commit**

```bash
git add internal/loopruntime pkg/event/errors.go
git commit -m "feat(loopruntime): validate native structured finals"
```

### Phase 4 review checkpoint

This is five tasks since the previous checkpoint. Review Harness public contracts/runtime against the spec, then code quality, actor atomicity, immutability, and security. Repair and re-review.

---

## Phase 5 — Terminal fallback, integration, and release readiness

### Task 16: Reserve and construct the terminal-output tool safely

**Repository:** `harness`

**Files:**
- Modify: `internal/loopruntime/output.go`
- Modify: `internal/loopruntime/output_test.go`
- Modify: `pkg/loop/definition.go`
- Modify: `internal/sessionruntime/loop_tools.go`
- Modify: `internal/sessionruntime/loop_tools_test.go`

**Step 1: Write failing tests**

Prove the fallback tool has the exact reserved name/schema/description; declared, mode, delegated/injected, and external tools cannot claim the name; collision rejection is atomic; the terminal definition is model-facing but absent from executable registries.

**Step 2: Verify red**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./internal/loopruntime ./internal/sessionruntime ./pkg/loop -run 'Test.*StructuredOutputTool|Test.*Reserved.*Tool|Test.*Collision'`

**Step 3: Implement minimally**

Centralize the reserved-name predicate. Do not create an `InvokableTool`, permission requirement, or tool binding for the control frame.

**Step 4: Verify green**

Run the same focused command and require all packages green.

**Step 5: Commit**

```bash
git add internal/loopruntime/output.go internal/loopruntime/output_test.go pkg/loop/definition.go internal/sessionruntime/loop_tools.go internal/sessionruntime/loop_tools_test.go
git commit -m "feat(loopruntime): reserve terminal output tool"
```

### Task 17: Intercept terminal calls before execution

**Repository:** `harness`

**Files:**
- Modify: `internal/loopruntime/turn.go`
- Modify: `internal/loopruntime/output.go`
- Create: `internal/loopruntime/output_terminal_test.go`
- Modify: `internal/loopruntime/runner_test.go` only if needed for negative event assertions.

**Step 1: Write failing tests**

Cover sole valid terminal call, compact JSON text materialization, usage preservation, no `RunBatch`/gate/tool events/counter increment, and no committed raw `ToolUseBlock`. Fail closed before action execution for terminal+ordinary, duplicate terminal, terminal+text, malformed input, wrong finish reason, or typed-nil blocks.

**Step 2: Verify red**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./internal/loopruntime -run 'Test.*TerminalOutput'`

**Step 3: Implement minimally**

Inspect raw `ToolUses()` immediately after step assembly and before tool-limit accounting. On a sole valid terminal call, replace staged state with one assistant `TextBlock` containing compact JSON, then use the existing final commit handshake.

**Step 4: Verify green**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./internal/loopruntime -run 'Test.*TerminalOutput'`

**Step 5: Commit**

```bash
git add internal/loopruntime
git commit -m "feat(loopruntime): intercept terminal structured output"
```

### Task 18: Prove terminal cancellation, restore, and context behavior

**Repository:** `harness`

**Files:**
- Modify/create focused tests under `internal/loopruntime/`
- Modify: `internal/sessionruntime/config_fingerprint_helpers_test.go`
- Modify: `pkg/rig/hustle_fingerprint_test.go`
- Modify: `pkg/rig/fingerprint_test.go`

**Step 1: Write failing integration tests**

Prove cancellation before terminal commit yields `TurnInterrupted`; malformed terminal yields `TurnFailed` with prior steps retained; context measurement sees the selected request shape consistently; restore fingerprints reject schema/policy drift; native and fallback final durable messages have the same text-only form.

**Step 2: Verify red**

Run focused `-race` tests across `internal/loopruntime`, `internal/sessionruntime`, and `pkg/rig`.

**Step 3: Implement only missing integration wiring**

Do not add retry, a new durable block type, or terminal tool execution events.

**Step 4: Verify green**

Run: `GOWORK=off GOFLAGS=-mod=mod go test -race ./internal/loopruntime ./internal/sessionruntime ./pkg/rig`

**Step 5: Commit**

```bash
git add internal/loopruntime internal/sessionruntime pkg/rig
git commit -m "test(harness): cover structured output lifecycle"
```

### Task 19: Refresh Harness vendor and run complete verification

**Repository:** `harness`

**Files:**
- Modify: `vendor/github.com/looprig/inference/**`
- Modify: `vendor/modules.txt` if generated content changes.
- Modify only implementation/tests required by full verification.

**Step 1: Refresh vendor**

Run: `GOWORK=off GOFLAGS=-mod=mod make vendor`

Inspect the vendor diff and confirm `make vendor-check` passes.

**Step 2: Format/test/build/security**

Run: `GOWORK=off make fmt`

Run: `GOWORK=off go test -race ./...`

Run: `GOWORK=off go test -tags integration -race ./...`

Run: `GOWORK=off CGO_ENABLED=0 go build -trimpath ./...`

Run: `GOWORK=off make secure`

**Step 3: Run external-input fuzz targets**

Run the structured inference fuzz targets in the inference worktree and Harness hustle output fuzz target for 30 seconds each.

**Step 4: Commit**

```bash
git add vendor
git add -u
git commit -m "chore(harness): vendor structured output inference"
```

### Phase 5 and final review checkpoint

1. Dispatch a spec reviewer over the complete diffs and commit ranges in all three worktrees.
2. Repair and re-review until the design and plan are fully satisfied.
3. Dispatch a code-quality/security reviewer over the complete implementation, emphasizing schema parser bounds, provider wire compatibility, typed errors, actor commit ordering, reserved-tool non-execution, vendored-source consistency, and no secret/raw-output retention.
4. Repair and re-review.
5. Re-run the complete verification commands from Tasks 8, 10, and 19 after the final repair commit.
6. Use `superpowers:finishing-a-development-branch` to present merge/integration options; do not merge without the user's choice.
