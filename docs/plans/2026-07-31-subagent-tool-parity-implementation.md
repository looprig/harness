# Subagent Tool Parity Upgrade Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Upgrade Harness's injected `Subagent` tool to the approved Claude-style launch vocabulary, background-first managed behavior, strict prepared-call execution, structured JSON results, and model-safe error boundary while retaining the existing parent-scoped five-action controller.

**Architecture:** `internal/delegationtool` remains the sole model-facing adapter. It decodes untrusted JSON once in `PrepareCall`, stores one translated `tool.DelegateRequest` in a private artifact, and executes only that artifact. `internal/sessionruntime` remains the authorization and child-lifecycle owner. `pkg/tool` gains only a narrow delegation failure-code contract so the adapter can classify controller refusals without importing the runtime. The long-running-command process/resource subsystem stays separate.

**Tech Stack:** Go 1.26, `encoding/json`, Looprig `tool.CallPreparer`, `tool.PreparedArtifact`, `tool.PreparedCall`, `loop.PreparedCallFromContext`, `tool.DelegateController`, and `core/uuid`.

---

## Preconditions

Implement on a branch containing the long-running-command prepared-call
contracts reviewed at `.worktrees/long-running-commands/harness` commit
`aaefa01c`, or rebase this plan's paths and assertions onto their merged
equivalents first.

Preserve unrelated local changes, especially existing `go.mod` and `go.sum`
edits. Do not add a dependency. Run every Go test with `GOWORK=off` and `-race`.

The approved design is:

- `docs/plans/2026-07-31-subagent-tool-parity-design.md`

The model-facing hard cut is:

```text
agent   -> subagent_type
message -> prompt
wait    -> !run_in_background
```

Do not implement `model` or `isolation` in this plan. Strict decoding must reject
both until their runtime contracts exist.

### Task 1: Add the stable delegation failure-code seam

**Files:**

- Modify: `pkg/tool/definition.go`
- Modify: `pkg/tool/definition_test.go`
- Modify: `internal/sessionruntime/delegation.go`
- Modify: `internal/sessionruntime/delegation_test.go`

**Step 1: Write the failing public-contract tests**

In `pkg/tool/definition_test.go`, add table tests that pin a small string-backed
code type and classifier interface:

```go
type DelegateFailureCode string

type DelegateFailure interface {
	error
	DelegateFailureCode() DelegateFailureCode
}
```

Cover exact stable codes for the controller failures the adapter may safely
distinguish:

```go
const (
	DelegateFailureActionUnavailable DelegateFailureCode = "action_unavailable"
	DelegateFailureUnauthorizedAgent DelegateFailureCode = "unauthorized_agent"
	DelegateFailureUnknownAgent      DelegateFailureCode = "unknown_agent"
	DelegateFailureUnknownMode       DelegateFailureCode = "unknown_mode"
	DelegateFailureNotOwned          DelegateFailureCode = "not_owned"
	DelegateFailureUnknownRequest    DelegateFailureCode = "unknown_request"
	DelegateFailureInterruptPending  DelegateFailureCode = "interrupt_pending"
	DelegateFailureUnavailable       DelegateFailureCode = "unavailable"
)
```

Test `Valid()` for every declared value and invalid values. The contract carries
classification only; it must not expose agent names, UUIDs, causes, or raw
messages.

**Step 2: Run the focused tests to verify they fail**

Run:

```bash
GOWORK=off go test -race ./pkg/tool -run 'TestDelegateFailureCode'
```

Expected: FAIL because the code and interface do not exist.

**Step 3: Implement the minimal contract**

Add the type, constants, interface, and `Valid()` method beside
`DelegateController`. Keep it independent of `sessionruntime` types.

Do not add a generic public error struct. The controller implementation owns
its detailed error and implements the narrow interface structurally.

**Step 4: Pin the runtime mapping**

In `internal/sessionruntime/delegation_test.go`, add a table mapping every
`DelegateErrorKind` to the intended public code. Expected mappings:

| Runtime kind | Public code |
|---|---|
| action unavailable | `action_unavailable` |
| unauthorized agent | `unauthorized_agent` |
| unknown agent | `unknown_agent` |
| unknown mode | `unknown_mode` |
| not owned/missing delegate ID | `not_owned` |
| unknown/missing request ID | `unknown_request` |
| interrupt pending | `interrupt_pending` |
| session unavailable/unknown operation | `unavailable` |

Then add:

```go
func (e *DelegateError) DelegateFailureCode() tool.DelegateFailureCode
```

Keep `Error()` and `Unwrap()` behavior available for internal diagnostics.

**Step 5: Run the focused tests**

Run:

```bash
GOWORK=off go test -race ./pkg/tool ./internal/sessionruntime -run 'TestDelegate(FailureCode|Error)'
```

Expected: PASS.

**Step 6: Commit**

```bash
git add pkg/tool/definition.go pkg/tool/definition_test.go internal/sessionruntime/delegation.go internal/sessionruntime/delegation_test.go
git commit -m "feat(tool): classify delegation failures"
```

### Task 2: Pin the new Subagent schema and hard-cut envelope

**Files:**

- Modify: `internal/delegationtool/subagent.go`
- Modify: `internal/delegationtool/subagent_test.go`

**Step 1: Replace the schema tests first**

Rewrite `TestSubagentInfoSchemaPerStyle` to inspect the decoded schema rather
than searching strings. Pin:

- exact model-facing name `Subagent`;
- `type: object` and `additionalProperties: false`;
- managed actions `start`, `send`, `wait`, `interrupt`, `status`;
- sync-only action `start` only;
- start required fields `description`, `prompt`, `subagent_type`;
- catalog-derived `subagent_type` choices;
- catalog-derived mode choices for each subagent type;
- managed `run_in_background` boolean;
- sync-only `run_in_background` constrained to `false`;
- action-specific forbidden fields;
- absence of legacy `agent`, `message`, and `wait` properties;
- absence of deferred `model` and `isolation` properties.

Keep the concurrent deterministic-schema test.

Add a description test proving the `<available_agents>` block remains complete
and does not mutate when the caller mutates its source catalog after
construction.

**Step 2: Run the schema tests to verify they fail**

Run:

```bash
GOWORK=off go test -race ./internal/delegationtool -run 'TestSubagentInfo|TestSubagentCatalog'
```

Expected: FAIL on the old field names and sync/background contract.

**Step 3: Define the canonical argument type and bounds**

Replace the model-facing fields in `SubagentArgs`:

```go
type SubagentArgs struct {
	Action          SubagentAction     `json:"action,omitempty"`
	Description     string             `json:"description,omitempty"`
	Prompt          string             `json:"prompt,omitempty"`
	SubagentType    identity.AgentName `json:"subagent_type,omitempty"`
	Mode            loop.ModeName      `json:"mode,omitempty"`
	DelegateID      *uuid.UUID         `json:"delegate_id,omitempty"`
	RequestID       *uuid.UUID         `json:"request_id,omitempty"`
	RunInBackground *bool              `json:"run_in_background,omitempty"`
	TimeoutSeconds  *int               `json:"timeout_seconds,omitempty"`
}
```

Add the exact private V1 limits from the design:

```go
const (
	maxSubagentArgsBytes = 256 << 10
	maxDescriptionBytes  = 256
	maxPromptBytes       = 192 << 10
	maxTimeoutSeconds    = 24 * 60 * 60
)
```

Generate the schema from this field vocabulary. Keep action-specific branches
and the existing catalog-specific `oneOf` behavior. Do not introduce a JSON
schema library.

**Step 4: Run the schema tests to verify they pass**

Run:

```bash
GOWORK=off go test -race ./internal/delegationtool -run 'TestSubagentInfo|TestSubagentCatalog'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/delegationtool/subagent.go internal/delegationtool/subagent_test.go
git commit -m "feat(subagent): adopt Claude-style envelope"
```

### Task 3: Move all argument validation into preparation

**Files:**

- Modify: `internal/delegationtool/subagent.go`
- Modify: `internal/delegationtool/subagent_test.go`

**Step 1: Write failing preparation tests**

Replace `TestSubagentPrepareCallIsPure` with table-driven tests that call
`PrepareCall` and inspect the private artifact from the same package.

Cover happy paths:

- omitted action becomes managed background `start`;
- explicit foreground `start` maps to `DelegateStart` and `Wait=true`;
- mode is preserved;
- background and foreground `send` map correctly;
- wait preserves both IDs and timeout;
- interrupt and one/all-child status map correctly;
- sync-only omitted background becomes foreground.

Cover boundary failures:

- empty, malformed, trailing, top-level null/array/scalar JSON;
- argument bytes just over `maxSubagentArgsBytes`;
- unknown fields, including `agent`, `message`, `wait`, `model`, and
  `isolation`;
- unknown action;
- action-specific forbidden fields;
- missing/blank `description`, `prompt`, or `subagent_type` on start;
- description and prompt at and above their byte limits;
- missing/zero/malformed UUIDs;
- negative and over-limit timeout;
- timeout on a background start/send;
- background start under sync-only style;
- selected subagent type absent from the catalog;
- selected mode absent from that catalog entry.

Each rejection must return:

- a non-nil typed preparation error;
- an empty `tool.Request`;
- a nil artifact;
- bounded text that does not contain the prompt, description, UUID, raw JSON,
  or decoder internals.

**Step 2: Run the preparation tests to verify they fail**

Run:

```bash
GOWORK=off go test -race ./internal/delegationtool -run 'TestSubagentPrepare'
```

Expected: FAIL because preparation still returns a nil artifact and validates
nothing.

**Step 3: Add the private artifact and safe preparation error**

Implement:

```go
type subagentArtifact struct {
	tool.TokenArtifact
	request     tool.DelegateRequest
	description string
}

type subagentPrepareError struct {
	reason string
}

func (e *subagentPrepareError) Error() string {
	return subagentToolName + ": " + e.reason
}
```

Reasons are fixed constants or bounded field categories. Never put supplied
values into the error.

`PrepareCall` must:

1. reject oversized raw bytes before allocating decoder state;
2. strictly decode exactly one object;
3. resolve the default action;
4. validate allowed/present fields;
5. validate style and catalog choices;
6. resolve `run_in_background` defaults;
7. build one `tool.DelegateRequest` with internal `Agent`, `Message`, and `Wait`;
8. return `tool.Request{}` and a `subagentArtifact` whose embedded token is
   `executionID.String()`.

Use explicit presence tracking so omitted booleans remain distinguishable from
false and action branches can reject irrelevant fields. Copy pointer values
into artifact-owned storage; no artifact field may point into mutable decode
scratch.

**Step 4: Verify preparation and decoder behavior**

Run:

```bash
GOWORK=off go test -race ./internal/delegationtool -run 'TestSubagentPrepare|TestSubagentInfo'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/delegationtool/subagent.go internal/delegationtool/subagent_test.go
git commit -m "feat(subagent): prepare typed delegation calls"
```

### Task 4: Execute only the prepared artifact

**Files:**

- Modify: `internal/delegationtool/subagent.go`
- Modify: `internal/delegationtool/subagent_test.go`

**Step 1: Write failing prepared-execution tests**

Add a helper that performs the real sequence:

```go
request, artifact, err := subagent.PrepareCall(ctx, executionID, argsJSON)
preparedCtx := loop.WithPreparedCall(ctx, tool.PreparedCall{
	ExecutionID: executionID,
	Request:     request,
	Artifact:    artifact,
})
result, err := subagent.InvokableRun(preparedCtx, argsJSON)
```

Rewrite envelope-to-controller tests through that helper. Add focused tests
proving:

- raw arguments changed after preparation are ignored;
- malformed raw arguments at execution are ignored after valid preparation;
- absent prepared call fails closed;
- nil artifact fails closed;
- wrong artifact type fails closed;
- a token/execution-ID mismatch fails closed;
- every fail-closed case makes zero controller calls;
- the provider tool-use ID is copied from execution context into a fresh
  request copy;
- adding the provider tool-use ID does not mutate the stored artifact;
- repeated execution cannot mutate artifact-owned UUID/timeout values.

Use a tiny test artifact embedding `tool.TokenArtifact` to exercise the
wrong-type case without changing the sealed artifact contract.

**Step 2: Run the tests to verify they fail**

Run:

```bash
GOWORK=off go test -race ./internal/delegationtool -run 'TestSubagent(Prepared|Start|Send|Wait|Interrupt|Status)'
```

Expected: FAIL because `InvokableRun` still re-decodes raw JSON.

**Step 3: Replace execution-time parsing**

Make `InvokableRun`:

1. read `loop.PreparedCallFromContext`;
2. type-assert `*subagentArtifact` or `subagentArtifact`, choosing one exact
   representation and pinning it in tests;
3. require non-zero/matching execution ID and artifact token;
4. deep-copy the prepared `tool.DelegateRequest`;
5. set `ParentToolUseID` from `loop.ToolUseIDFrom(ctx)`;
6. call `DelegateController.Execute` once;
7. format the typed result.

Delete execution-only decoder paths. `InvokableRun` must not consult
`argsJSON`, even for audit or error text.

Keep missing-artifact failures as model-visible tool-result errors with nil Go
errors. Preparation failures never reach `InvokableRun`; the loop runner already
turns them into model-visible failures.

**Step 4: Run the package tests**

Run:

```bash
GOWORK=off go test -race ./internal/delegationtool
```

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/delegationtool/subagent.go internal/delegationtool/subagent_test.go
git commit -m "fix(subagent): execute only prepared artifacts"
```

### Task 5: Return typed JSON and sanitize controller failures

**Files:**

- Modify: `internal/delegationtool/subagent.go`
- Modify: `internal/delegationtool/subagent_test.go`

**Step 1: Write failing result-shape tests**

Replace substring assertions with JSON decoding into test structs/maps. Pin the
exact keys for:

- queued start/send;
- completed foreground start/send;
- completed wait;
- timed-out, interrupted, and failed terminal results;
- interrupt;
- one-child status;
- all-child status, including deterministic child ordering supplied by the
  controller.

Pin that a completed child output containing quotes, newlines, Unicode, and
JSON-looking text remains one correctly escaped string field. Pin that empty
completed output is still represented as `"output":""`.

Every response must be valid JSON in exactly one text block. No response may be
a bare child string.

**Step 2: Write failing error-boundary tests**

Program the fake controller with:

- each stable `tool.DelegateFailure` code;
- an ordinary error containing a fake path, token, prompt, and UUID;
- a wrapped classified error;
- a malicious error string containing newlines and JSON.

Assert stable model-visible messages and prove none of the raw strings reach the
result. Also assert `AuditSummary` is exactly `Subagent` for valid, invalid, and
sensitive inputs.

**Step 3: Run the focused tests to verify they fail**

Run:

```bash
GOWORK=off go test -race ./internal/delegationtool -run 'TestSubagent(Result|ControllerError|Audit)'
```

Expected: FAIL because completed output is bare text and controller errors are
currently concatenated with `err.Error()`.

**Step 4: Implement typed result values**

Define private response structs with explicit JSON tags. Marshal through one
helper:

```go
func subagentJSONResult(value subagentResponse) *tool.ToolResult
```

Handle the impossible marshal error fail-closed with a fixed generic result;
do not panic. Avoid `map[string]any` and hand-built JSON.

Use exact status labels:

```text
running idle completed interrupted failed timed_out queued unknown
```

Use `failed`, not the current `faulted`, so operation-terminal and status labels
share one vocabulary.

**Step 5: Implement safe controller-error mapping**

Use `errors.As` against `tool.DelegateFailure`. Map stable codes to bounded
messages that disclose no supplied value. Any invalid or unknown code, ordinary
error, or malicious wrapper maps to:

```text
error: subagent operation failed
```

Never append `err.Error()` to a model result. Internal logging, if added, must
use the existing bounded diagnostic helper and must not log prompt/description.

**Step 6: Run the package tests**

Run:

```bash
GOWORK=off go test -race ./internal/delegationtool
```

Expected: PASS.

**Step 7: Commit**

```bash
git add internal/delegationtool/subagent.go internal/delegationtool/subagent_test.go
git commit -m "feat(subagent): structure results and sanitize failures"
```

### Task 6: Re-pin the scoped controller against the new defaults

**Files:**

- Modify: `internal/sessionruntime/delegation_test.go`
- Modify if required: `internal/sessionruntime/delegation.go`

**Step 1: Add controller-bypass tests**

The schema and adapter are guidance/boundary validation, not the authorization
boundary. Add or strengthen direct `scopedController.Execute` tests proving:

- sync-only rejects `DelegateStart` with `Wait=false`;
- sync-only rejects every non-start operation;
- managed accepts start/send with either wait value;
- unauthorized and unknown agents fail before quota reservation;
- unknown mode fails before quota reservation;
- child ownership is parent-exact for send/wait/interrupt/status;
- zero/missing IDs fail closed;
- timeout values are already bounded by the adapter, while a direct controller
  caller still cannot bypass ownership or style;
- controller errors satisfy the new public classification interface.

Do not duplicate JSON-envelope tests here. These are typed controller security
tests.

**Step 2: Run the controller tests**

Run:

```bash
GOWORK=off go test -race ./internal/sessionruntime -run 'TestDelegate|TestScopedController'
```

Expected: PASS unless the new tests reveal a controller bypass.

**Step 3: Make only required controller fixes**

If a test fails, fix the narrow typed request boundary. Do not move parsing,
schema logic, or model-facing defaults into `sessionruntime`.

Keep these existing behaviors unchanged:

- background drains use `sessionCtx`;
- queued turns are non-folding;
- wait identifies one request ID;
- restored resolved requests remain collectible;
- interrupt affects the current child turn, not the parent/subtree;
- cumulative spawn quota does not decrement for idle children.

**Step 4: Run the complete sessionruntime package**

Run:

```bash
GOWORK=off go test -race ./internal/sessionruntime
```

Expected: PASS.

**Step 5: Commit**

```bash
git add internal/sessionruntime/delegation.go internal/sessionruntime/delegation_test.go
git commit -m "test(session): pin upgraded Subagent controller boundary"
```

If `delegation.go` did not change, omit it from `git add`.

### Task 7: Update Harness integration and model-call fixtures

**Files:**

- Modify: `pkg/rig/subagent_injection_test.go`
- Modify if matched: any Harness Go fixture found by the searches below
- Modify: `pkg/loop/README.md`
- Modify: `pkg/tool/README.md`

**Step 1: Find every current model-facing call shape**

Run:

```bash
rg -n '"(agent|message|wait)"\s*:' --glob '*.go' --glob '*.json' --glob '*.md' .
rg -n 'Subagent' --glob '*.go' --glob '*.json' --glob '*.md' .
```

Classify matches before editing:

- executable tests/examples must use the new envelope;
- internal `tool.DelegateRequest.Agent/Message/Wait` remains unchanged;
- historical design documents stay historical;
- the new approved design/plan must not contain legacy fields except migration
  tables and current-state descriptions.

Do not edit the separate `github.com/looprig/tools` Task implementation branch.
Its consumer fixtures should update when that work rebases onto this Harness
change.

**Step 2: Strengthen injection tests**

In `pkg/rig/subagent_injection_test.go`, retain proof that:

- a delegate-bearing loop receives exactly one injected `Subagent`;
- a leaf receives none;
- the catalog lists only configured children.

Add schema assertions for `description`, `prompt`, `subagent_type`, and
`run_in_background`, and negative assertions for the removed fields.

**Step 3: Update public documentation**

In `pkg/loop/README.md`, document:

- Subagent is automatically injected from `WithDelegates`;
- managed calls default to background;
- sync-only calls are foreground;
- `mode` is the supported configured variant selector;
- `model` and `isolation` are not accepted in this version.

In `pkg/tool/README.md`, document the prepared-artifact invariant for pure
control tools: an empty access request does not imply a nil artifact or deferred
argument validation.

**Step 4: Run integration-focused tests**

Run:

```bash
GOWORK=off go test -race ./pkg/rig ./pkg/loop ./pkg/tool -run 'Test.*Subagent|TestDelegate|TestPrepared'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/rig/subagent_injection_test.go pkg/loop/README.md pkg/tool/README.md
git commit -m "docs: publish upgraded Subagent contract"
```

Include any additional executable fixtures found in Step 1 in the same commit.

### Task 8: Move fuzzing to the actual untrusted boundary

**Files:**

- Modify: `internal/delegationtool/subagent_test.go`

**Step 1: Replace the execution fuzz target**

Replace `FuzzSubagentArgs`, which currently fuzzes `InvokableRun`, with
`FuzzSubagentPrepareCall`.

Seed at least:

```json
{"description":"Explore code","prompt":"Map the repo","subagent_type":"explorer"}
{"action":"send","delegate_id":"55555555-5555-4555-8555-555555555555","prompt":"continue"}
{"action":"wait","delegate_id":"55555555-5555-4555-8555-555555555555","request_id":"66666666-6666-4666-8666-666666666666"}
{"action":"status"}
{"model":"sonnet"}
{"isolation":"worktree"}
{}
null
[]
```

For arbitrary bytes, assert `PrepareCall` never panics. On error, assert artifact
is nil. On success, assert:

- request is a valid empty `tool.Request`;
- artifact has the exact private type;
- its token matches the supplied execution ID;
- its translated request has a known operation;
- a real prepare-to-execute sequence never returns a Go error or nil result.

Do not assert every arbitrary input succeeds.

**Step 2: Run the ordinary fuzz seed corpus**

Run:

```bash
GOWORK=off go test -race ./internal/delegationtool -run '^$' -fuzz FuzzSubagentPrepareCall -fuzztime=1x
```

Expected: PASS.

**Step 3: Run the required 30-second fuzz campaign**

Run:

```bash
GOWORK=off go test ./internal/delegationtool -run '^$' -fuzz FuzzSubagentPrepareCall -fuzztime=30s
```

Expected: PASS with no panic or artifact invariant failure. The Go race detector
is covered by the complete package and suite runs; do not combine `-race` with a
long fuzz campaign if the environment makes it impractical.

**Step 4: Commit**

```bash
git add internal/delegationtool/subagent_test.go
git commit -m "test(subagent): fuzz prepared argument boundary"
```

### Task 9: Verify long-running-command compatibility and the full module

**Files:**

- Modify only if a failure proves necessary: files implicated by that failure

**Step 1: Run focused packages together**

Run:

```bash
GOWORK=off go test -race ./internal/delegationtool ./internal/sessionruntime ./internal/loopruntime ./pkg/tool ./pkg/loop ./pkg/rig
```

Expected: PASS.

This specifically exercises the prepared-call handoff, session/controller
behavior, injected tool schema, and the long-running branch's runner/resource
integration in one command.

**Step 2: Run formatting checks**

Run:

```bash
make fmt
make fmt-check
```

Expected: PASS with no unexpected changes outside files touched by the plan.

Inspect:

```bash
git status --short
git diff --check
```

Preserve unrelated `go.mod` and `go.sum` changes.

**Step 3: Run the full race suite**

Run:

```bash
GOWORK=off go test -race ./...
```

Expected: PASS.

**Step 4: Run the required build**

Run:

```bash
CGO_ENABLED=0 GOWORK=off go build -trimpath ./...
```

Expected: PASS.

**Step 5: Run security verification**

Run:

```bash
GOWORK=off make secure
```

Expected: PASS. If network-backed vulnerability metadata is unavailable, record
that environment failure separately; do not describe it as a code pass.

**Step 6: Review the final diff against the design**

Confirm all acceptance criteria from
`docs/plans/2026-07-31-subagent-tool-parity-design.md` and explicitly verify:

- no `err.Error()` is appended to a Subagent result;
- no execution-time argument decoder remains;
- no `model`/`isolation` parameter is advertised;
- no Subagent code imports or uses process-resource contracts;
- no Task store or Task tool implementation was changed;
- every new success result is valid JSON;
- every affected test ran with `-race` at least once.

**Step 7: Commit any verification-only correction**

If verification required a correction:

```bash
git add <exact corrected files>
git commit -m "fix(subagent): satisfy parity verification"
```

If no correction was needed, do not create an empty commit.

## Completion Evidence

The implementation is complete only when the handoff includes:

- commit hashes for Tasks 1-8 and any Task 9 correction;
- the exact focused and full test commands run;
- the final `go test -race ./...` result;
- the final `CGO_ENABLED=0 GOWORK=off go build -trimpath ./...` result;
- the final `make secure` result or a precise external blocker;
- confirmation that unrelated worktree changes were preserved;
- confirmation that the separate Task implementation was not modified.
