# Reusable Evaluation Framework Phase 1 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the first independently usable `github.com/looprig/eval` module with typed observations, deterministic and structured-judge evaluators, datasets, reports, inference targets, and native Go test integration.

**Architecture:** The eval root owns application-neutral contracts and orchestration over `core/content.AgenticMessages`. Leaf packages add datasets, deterministic evaluators, an `inference.Client` judge/target, and `testing.TB` presentation. Harness, sandbox/proxy, OTel, continuous eval, and MPQT packs follow after this stable foundation.

**Tech Stack:** Go 1.26, standard library, `github.com/looprig/core/content`, `github.com/looprig/inference`, Go `testing`, JSON/JSONL.

---

Paths beginning with `eval/` are relative to the new sibling repository
`/Users/ipotter/code/looprig/eval`. Provision that repository before execution;
do not place the module inside Harness. Use local `replace` directives only for
workspace development and remove them from release commits if the repository's
release process requires tagged dependencies.

### Task 1: Bootstrap the independent module

**Files:**
- Create: `eval/go.mod`
- Create: `eval/AGENTS.md`
- Create: `eval/doc.go`
- Create: `eval/Makefile`
- Test: `eval/doc_test.go`

**Step 1: Write the failing package contract test**

Create an external-package test that imports `github.com/looprig/eval`, asserts
the package is importable, and checks a public `Version` constant equals
`"eval/v1"`.

**Step 2: Run the focused test and verify failure**

Run: `GOWORK=off go test -race .`

Expected: FAIL because the module/package or `Version` does not exist.

**Step 3: Add the minimal module and package**

Use module path `github.com/looprig/eval`, Go 1.26, and require
`github.com/looprig/core`. Add `const Version = "eval/v1"`. Copy the repository
security, strict-typing, `-race`, integration-test, fuzzing, and dependency
approval rules into `AGENTS.md`; do not copy Harness-specific ownership rules.

**Step 4: Add standard verification targets**

Add `fmt`, `fmt-check`, `test`, and `secure` targets consistent with sibling
modules. Do not introduce a third-party runtime dependency.

**Step 5: Verify and commit**

Run: `GOWORK=off go test -race .`

Expected: PASS.

Commit: `chore: bootstrap eval module`

### Task 2: Add strict identity, scope, method, and status types

**Files:**
- Create: `eval/types.go`
- Create: `eval/validate.go`
- Create: `eval/errors.go`
- Test: `eval/types_test.go`

**Step 1: Write table-driven validation tests**

Cover valid and invalid `Name`, `Revision`, `Scope`, `Method`,
`AssessmentStatus`, `Severity`, and `Unit`. Include empty names, invalid UTF-8,
oversized values, unknown enum values, and valid zero values only where the
design declares them.

**Step 2: Run the focused test and verify failure**

Run: `GOWORK=off go test -race . -run 'Test(Name|Revision|Enums)'`

Expected: FAIL with undefined domain types.

**Step 3: Implement named types and typed validation errors**

Add the enums from the design. Use closed validation switches and bounded UTF-8
strings. Public failures must support `errors.As`; diagnostic strings must not
echo untrusted content.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race .`

Expected: PASS.

Commit: `feat: add eval domain identity types`

### Task 3: Implement observations, traces, and evidence

**Files:**
- Create: `eval/observation.go`
- Create: `eval/evidence.go`
- Test: `eval/observation_test.go`
- Test: `eval/evidence_test.go`

**Step 1: Write tests for conversation preservation**

Construct `content.AgenticMessages` containing user, assistant, tool-use, and
error tool-result content. Assert `Observation.Validate` preserves order and
rejects an invalid subject, invalid time range, invalid message range, duplicate
evidence ID, or an evidence reference outside the conversation.

**Step 2: Write tests for typed evidence variants**

Cover conversation excerpts, timing, usage, tool operation, HTTP request, DNS,
process, file, sandbox guarantee, and diagnostic evidence. Assert each variant
has exactly one payload and sensitive fields use redacted/hash forms.

**Step 3: Run and verify failure**

Run: `GOWORK=off go test -race . -run 'Test(Observation|Evidence)'`

Expected: FAIL with undefined observation/evidence types.

**Step 4: Implement the minimal tagged union and validation**

Keep `Conversation` canonical. `Trace.Operations` contains correlation, timing,
status, safe attributes, and evidence references; do not duplicate message text.

**Step 5: Verify and commit**

Run: `GOWORK=off go test -race .`

Expected: PASS.

Commit: `feat: add eval observations and operational evidence`

### Task 4: Implement scenarios, expectations, targets, and samples

**Files:**
- Create: `eval/scenario.go`
- Create: `eval/expectation.go`
- Create: `eval/target.go`
- Test: `eval/scenario_test.go`

**Step 1: Write validation tests**

Cover stable scenario identity, duplicate labels, empty input, optional
expectations, valid and invalid required facts, forbidden actions, tool-call
expectations, structured-output expectation, and target returning an observation
whose subject does not match the target revision.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race . -run 'Test(Scenario|Expectation|Sample)'`

Expected: FAIL.

**Step 3: Implement the contracts**

Add `Scenario`, `Expectation`, `Target`, and `Sample`. Keep target execution to
one method:

```go
type Target interface {
    Name() string
    Observe(context.Context, Scenario) (Observation, error)
}
```

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race .`

Expected: PASS.

Commit: `feat: add eval scenarios and target contract`

### Task 5: Implement evaluator descriptors and assessments

**Files:**
- Create: `eval/evaluator.go`
- Create: `eval/assessment.go`
- Test: `eval/evaluator_test.go`
- Test: `eval/assessment_test.go`

**Step 1: Write descriptor and assessment validation tests**

Cover duplicate measurement names, non-finite numbers, invalid units, duplicate
finding codes where forbidden, missing required evidence, dangling evidence
references, and status consistency. A missing required evidence kind must yield
an `unverified` assessment helper, not a pass.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race . -run 'Test(Descriptor|Assessment)'`

Expected: FAIL.

**Step 3: Implement the evaluator contract**

```go
type Evaluator interface {
    Descriptor() Descriptor
    Evaluate(context.Context, Sample) (Assessment, error)
}
```

Add constructors/helpers that validate invariants without hiding evaluator
errors as quality failures.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race .`

Expected: PASS.

Commit: `feat: add evaluator and assessment contracts`

### Task 6: Build the non-fail-fast execution engine

**Files:**
- Create: `eval/suite.go`
- Create: `eval/run.go`
- Create: `eval/report.go`
- Test: `eval/run_test.go`
- Test: `eval/run_race_test.go`

**Step 1: Write execution-order and isolation tests**

Use fake targets/evaluators to cover success, target error, one evaluator error
beside one success, timeout, cancellation, duplicate IDs, empty evaluators,
stable output ordering, and bounded concurrency. Assert input scenarios are not
mutated and no failure discards completed sibling assessments.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race . -run 'TestRun'`

Expected: FAIL.

**Step 3: Implement minimal orchestration**

Add `Suite`, `RunConfig`, `Report`, `SampleReport`, and `Run`. Use a semaphore,
`context.WithTimeout`, indexed result slots, and explicit stage errors. Default
to sequential execution; concurrency must be opt-in and bounded.

**Step 4: Verify race behavior and commit**

Run: `GOWORK=off go test -race .`

Expected: PASS with no race report.

Commit: `feat: add eval suite runner and reports`

### Task 7: Add the versioned dataset codec

**Files:**
- Create: `eval/dataset/dataset.go`
- Create: `eval/dataset/json.go`
- Create: `eval/dataset/errors.go`
- Test: `eval/dataset/json_test.go`
- Test: `eval/dataset/json_fuzz_test.go`

**Step 1: Write round-trip and boundary tests**

Cover JSONL ordering, conversation block preservation, unknown versions,
duplicates, malformed input, oversize record/file, symlink escape, invalid UTF-8,
and trailing data. Use `os.OpenRoot` for directory-scoped loading.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./dataset`

Expected: FAIL.

**Step 3: Implement a `dataset/v1` wire envelope**

Use explicit wire discriminators only at JSON boundaries. Decode immediately to
strict domain types. Preserve deterministic ordering and typed path/record
errors.

**Step 4: Add the fuzz target and verify**

Run: `GOWORK=off go test -race ./dataset`

Run: `GOWORK=off go test ./dataset -run '^$' -fuzz=FuzzDecode -fuzztime=30s`

Expected: tests PASS; fuzzing finds no panic.

**Step 5: Commit**

Commit: `feat: add versioned eval dataset codec`

### Task 8: Add foundational programmatic evaluators

**Files:**
- Create: `eval/exact/text.go`
- Create: `eval/exact/tool.go`
- Create: `eval/exact/structured.go`
- Create: `eval/exact/operational.go`
- Test: `eval/exact/text_test.go`
- Test: `eval/exact/tool_test.go`
- Test: `eval/exact/structured_test.go`
- Test: `eval/exact/operational_test.go`

**Step 1: Write table-driven evaluator tests**

Cover required/forbidden text, required/forbidden tool call, JSON-schema result
status, tool error rate, and maximum duration. Include empty expectations,
multiple assistant messages, nested tool results, Unicode, missing evidence, and
malformed tool arguments.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./exact`

Expected: FAIL.

**Step 3: Implement evaluators over typed content/evidence**

Do not flatten a conversation except in the text evaluator's private projection.
Return evidence references for every failure. Vacuous expectations must be a
validation error or `skipped`, never pass.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./exact`

Expected: PASS.

Commit: `feat: add deterministic eval evaluators`

### Task 9: Add native Go test presentation

**Files:**
- Create: `eval/evaltest/run.go`
- Create: `eval/evaltest/assert.go`
- Create: `eval/evaltest/render.go`
- Test: `eval/evaltest/run_test.go`

**Step 1: Write tests with a fake `testing.TB` recorder**

Cover subtest names, pass/fail/error/unverified rendering, concise evidence,
profile-free assertions, deterministic ordering, and no secret/raw content in
failure output.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./evaltest`

Expected: FAIL.

**Step 3: Implement `Run`, `RunScenario`, `RequirePass`, and `RequireVerified`**

Accept `testing.TB`, call `Helper`, and return the complete report. Use subtests
where a concrete `*testing.T` is available; keep core orchestration outside this
package.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./evaltest`

Expected: PASS.

Commit: `feat: integrate eval reports with go test`

### Task 10: Add rubrics and the structured-output judge

**Files:**
- Create: `eval/rubric/rubric.go`
- Create: `eval/rubric/catalog.go`
- Create: `eval/judge/schema.go`
- Create: `eval/judge/judge.go`
- Create: `eval/judge/prompt.go`
- Create: `eval/judge/errors.go`
- Test: `eval/rubric/rubric_test.go`
- Test: `eval/judge/judge_test.go`

**Step 1: Write rubric validation tests**

Cover revision, scope, criteria, anchors, invalid score ranges, duplicate
criteria, and the initial built-ins: answer relevance, groundedness,
instruction adherence, goal adherence, toxicity, vulgarity, and internet-use
appropriateness.

**Step 2: Write judge tests with a fake `inference.Client`**

Assert the request contains the observation conversation, rubric revision,
bounded evidence instructions, and `inference.OutputSchema{Strict:true}`. Cover
valid output, malformed JSON, out-of-range score, invalid message index, quote
not present in the indexed message, inference error, timeout, and unsupported
structured output.

**Step 3: Run and verify failure**

Run: `GOWORK=off go test -race ./rubric ./judge`

Expected: FAIL.

**Step 4: Implement `ScoreSchemaV1` and judge evaluation**

Call `inference.Client.Invoke` directly. Decode the terminal assistant text as
the schema object, validate it again locally, and return `MethodModel` evidence.
Do not parse free-form `SCORE:` lines and do not use Harness Hustles.

**Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./rubric ./judge`

Expected: PASS.

Commit: `feat: add structured-output rubric judge`

### Task 11: Add the inference target

**Files:**
- Create: `eval/target/inference/target.go`
- Create: `eval/target/inference/project.go`
- Test: `eval/target/inference/target_test.go`
- Test: `eval/target/inference/target_integration_test.go`

**Step 1: Write fake-client unit tests**

Cover request-template cloning, scenario message append, response/usage
projection, typed inference error, nil response/message, timeout, and no mutation
of shared request slices.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./target/inference`

Expected: FAIL.

**Step 3: Implement the target**

Use `inference.Client.Invoke`; populate subject/model revisions, inference
operation timing, usage evidence, and returned assistant message. Keep secrets
out of trace metadata.

**Step 4: Add a build-tagged live smoke test**

Guard the real-client test with `//go:build integration`. Fail clearly when the
required test credential is absent; never run it in the default suite.

**Step 5: Verify and commit**

Run: `GOWORK=off go test -race ./target/inference`

Expected: PASS; integration test excluded.

Commit: `feat: add inference evaluation target`

### Task 12: Add JSON reports and baseline comparison

**Files:**
- Create: `eval/reportjson/codec.go`
- Create: `eval/reportjson/sink.go`
- Create: `eval/compare/compare.go`
- Test: `eval/reportjson/codec_test.go`
- Test: `eval/compare/compare_test.go`
- Test: `eval/reportjson/codec_fuzz_test.go`

**Step 1: Write canonical report round-trip tests**

Cover deterministic ordering, finite numeric values, version rejection,
redaction, partial runs, stage errors, and all assessment statuses.

**Step 2: Write compatible/incompatible baseline tests**

Cover added, removed, changed, failed, errored, and unverified cases; compare
distributions only when scenario/evaluator identities are compatible.

**Step 3: Run and verify failure**

Run: `GOWORK=off go test -race ./reportjson ./compare`

Expected: FAIL.

**Step 4: Implement codecs, sink, and comparison**

Use a `report/v1` envelope and atomic file replacement inside an explicitly
provided directory. Do not expose raw conversation content by default.

**Step 5: Verify, fuzz, and commit**

Run: `GOWORK=off go test -race ./reportjson ./compare`

Run: `GOWORK=off go test ./reportjson -run '^$' -fuzz=FuzzDecode -fuzztime=30s`

Expected: PASS and no fuzz panic.

Commit: `feat: add eval reports and baseline comparison`

### Task 13: Migrate the existing Harness eval tests as a compatibility proof

**Files:**
- Create: `harness/pkg/evalmigration/eval_integration_test.go`
- Modify: `harness/go.mod`
- Modify: `harness/go.sum`
- Do not yet remove: `harness/pkg/eval/**`

**Step 1: Write a build-tagged migration test**

Re-express the current contains and judge examples with
`content.AgenticMessages`, `exact.RequiredText`, `judge.New`, and
`evaltest.RunScenario`. The fake judge client must return valid structured
output.

**Step 2: Run and verify dependency failure**

Run: `GOWORK=off go test -race ./pkg/evalmigration -tags evalmigration`

Expected: FAIL until Harness references the new eval module.

**Step 3: Add the approved module dependency**

Add `github.com/looprig/eval` and a local replace during workspace development.
Do not add any unrelated dependency.

**Step 4: Verify old and new paths together**

Run: `GOWORK=off go test -race ./pkg/eval ./pkg/evalmigration -tags evalmigration`

Expected: PASS.

**Step 5: Commit only migration proof changes**

Commit: `test: prove harness eval migration path`

### Task 14: Complete module verification and publish readiness

**Files:**
- Modify: `eval/README.md`
- Modify: `eval/AGENTS.md` only if verification discovers a missing rule

**Step 1: Add focused usage documentation**

Document deterministic qualification, structured judge, `go test` build tags,
`-count=1` for live tests, report JSON, and the explicit non-goals around action
and Harness journals.

**Step 2: Run all offline verification**

Run: `GOWORK=off go test -race ./...`

Run: `CGO_ENABLED=0 GOWORK=off go build -trimpath ./...`

Run: `make secure`

Expected: all commands PASS.

**Step 3: Confirm package boundaries**

Run: `go list -deps ./...`

Expected: eval root has no Harness, sandbox, OTel, or provider-SDK dependency;
only judge/target inference packages import `github.com/looprig/inference`.

**Step 4: Commit documentation and release preparation**

Commit: `docs: document reusable eval framework`

### Deferred implementation plans

Write separate plans after Phase 1 contracts have real usage feedback for:

- continuous Harness adapter and `harness/pkg/telemetry`;
- OpenTelemetry sink;
- sandbox and recording-proxy adapters;
- suite matrices, repeated trials, caching, and rate-limit helpers;
- golden-candidate extraction workflow;
- removal of `harness/pkg/eval`.
