# Model Power Quality Test Phase 1 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build MPQT's first enterprise model scorecard on the reusable eval framework, covering capability, structured output, tool use, safety conduct, qualification profiles, and candidate/incumbent comparison.

**Architecture:** `github.com/looprig/mpqt` is a product/test-pack module above `github.com/looprig/eval`. It contributes versioned packs, run manifests, rollups, and organization policy profiles; it does not fork the eval runner or take runtime action. Sandbox/proxy egress laboratories are Phase 2 because they require adapter and enforcement work identified in the design.

**Tech Stack:** Go 1.26, `github.com/looprig/eval`, `github.com/looprig/core/content`, `github.com/looprig/inference`, standard-library statistics and JSON, Go `testing`.

---

Paths beginning with `mpqt/` are relative to a new sibling repository
`/Users/ipotter/code/looprig/mpqt`. Execute this plan only after the eval Phase 1
acceptance criteria pass.

### Task 1: Bootstrap the MPQT module and manifest

**Files:**
- Create: `mpqt/go.mod`
- Create: `mpqt/AGENTS.md`
- Create: `mpqt/doc.go`
- Create: `mpqt/manifest.go`
- Test: `mpqt/manifest_test.go`

**Step 1: Write manifest validation tests**

Cover required target/model/revision identity, endpoint class, capabilities,
sampling/configuration revisions, forbidden secrets, bounded strings, and a
stable reproducibility fingerprint.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race .`

Expected: FAIL because the module and manifest do not exist.

**Step 3: Implement the module and secret-free manifest**

Require the eval/core/inference modules only. Fingerprint canonical public
configuration with SHA-256. Never include credentials or raw authorization
headers.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race .`

Expected: PASS.

Commit: `chore: bootstrap mpqt module`

### Task 2: Implement pack contracts and capability filtering

**Files:**
- Create: `mpqt/pack.go`
- Create: `mpqt/capability.go`
- Test: `mpqt/pack_test.go`

**Step 1: Write tests**

Cover duplicate pack/scenario/evaluator IDs, missing required target capability,
stable pack revision, optional unsupported scenarios, and the distinction
between `skipped` and `unverified`.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race . -run 'Test(Pack|Capability)'`

Expected: FAIL.

**Step 3: Implement `Pack` and expansion into `eval.Suite`**

Do not add a second runner. Capability filtering produces explicit preflight
assessments that remain in the final scorecard.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race .`

Expected: PASS.

Commit: `feat: add mpqt pack contracts`

### Task 3: Implement scorecard dimensions and rollups

**Files:**
- Create: `mpqt/scorecard.go`
- Create: `mpqt/stats.go`
- Test: `mpqt/scorecard_test.go`

**Step 1: Write statistical rollup tests**

Cover count, pass/fail/error/unverified/skipped rates, mean, median, configured
quantiles, variance, empty sets, one sample, repeated trials, and non-finite
input rejection. Never average status rates into a quality score.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race . -run 'Test(Scorecard|Rollup|Quantile)'`

Expected: FAIL.

**Step 3: Implement deterministic rollups**

Use sorted copies and documented quantile interpolation. Preserve every raw
`eval.Assessment` behind the rollup.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race .`

Expected: PASS.

Commit: `feat: add mpqt scorecard rollups`

### Task 4: Implement qualification profiles and dispositions

**Files:**
- Create: `mpqt/profile/profile.go`
- Create: `mpqt/profile/requirement.go`
- Create: `mpqt/profile/evaluate.go`
- Test: `mpqt/profile/evaluate_test.go`

**Step 1: Write disposition tests**

Cover minimum percentile, maximum failure/error/unverified rate, zero-tolerance
finding code, minimum sample count, required evidence guarantee, restriction
rules, and conflicting rules. Assert precedence: demonstrated mandatory
violation is rejected; insufficient proof is unverified; restrictions apply
only after mandatory requirements pass.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./profile`

Expected: FAIL.

**Step 3: Implement profile evaluation as a pure function**

Return a derived result containing requirement evidence. Do not mutate the
scorecard and do not call a model or target.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./profile`

Expected: PASS.

Commit: `feat: add organization qualification profiles`

### Task 5: Build the structured-output pack

**Files:**
- Create: `mpqt/packs/structuredoutput/v1.go`
- Create: `mpqt/packs/structuredoutput/testdata/v1/*.json`
- Test: `mpqt/packs/structuredoutput/v1_test.go`

**Step 1: Write pack construction and fixture tests**

Include valid nested objects/arrays, enum selection, required fields, forbidden
additional properties, Unicode, large-but-valid output, schema conflict with
tools, and malformed/partial terminal output.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./packs/structuredoutput`

Expected: FAIL.

**Step 3: Implement scenarios using only programmatic evaluators**

Measure first-attempt validity, terminal validity, repair count, latency, and
failure rate. Record the inference structured-output revision in provenance.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./packs/structuredoutput`

Expected: PASS with a fake target.

Commit: `feat: add mpqt structured output pack`

### Task 6: Build the tool-use pack and fixtures

**Files:**
- Create: `mpqt/fixture/tools/tools.go`
- Create: `mpqt/fixture/tools/recorder.go`
- Create: `mpqt/packs/tooluse/v1.go`
- Create: `mpqt/packs/tooluse/testdata/v1/*.json`
- Test: `mpqt/packs/tooluse/v1_test.go`

**Step 1: Write deterministic tool fixture tests**

Cover correct selection, schema-valid arguments, required order, parallel-safe
calls, malformed result, explicit tool error, retry/recovery, forbidden tool,
and no-tool-needed cases.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./fixture/tools ./packs/tooluse`

Expected: FAIL.

**Step 3: Implement fixture recording and pack scenarios**

Emit typed eval tool-operation evidence. Use programmatic evaluators for exact
behavior; include an optional tool-choice-quality rubric only for scenarios with
several valid paths.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./fixture/tools ./packs/tooluse`

Expected: PASS.

Commit: `feat: add mpqt tool use pack`

### Task 7: Build the core capability pack

**Files:**
- Create: `mpqt/packs/capability/v1.go`
- Create: `mpqt/packs/capability/testdata/v1/*.json`
- Test: `mpqt/packs/capability/v1_test.go`

**Step 1: Write fixture validation tests**

Start with multi-constraint instruction following, clarification, long-context
retrieval, known-answer QA, calibrated uncertainty, conversation consistency,
and one multilingual subset. Every scenario must identify which expectation is
programmatic and which rubric is model-based.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./packs/capability`

Expected: FAIL.

**Step 3: Implement the smallest auditable v1 pack**

Prefer exact evaluators. Add composite evaluators only where deterministic facts
cannot decide the intended semantic property. Do not label closed-book known
answer tests as open-world truthfulness.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./packs/capability`

Expected: PASS with fake targets/judges.

Commit: `feat: add mpqt core capability pack`

### Task 8: Build the safety-conduct pack

**Files:**
- Create: `mpqt/packs/safety/conduct_v1.go`
- Create: `mpqt/packs/safety/testdata/conduct_v1/*.json`
- Test: `mpqt/packs/safety/conduct_v1_test.go`

**Step 1: Write paired and contextual scenario tests**

Cover toxicity, vulgarity in direct versus quoted context, harassment, safe
refusal, allowed benign request, over-refusal, under-refusal, sycophancy, prompt
injection, and seeded prompt/PII canary leakage.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./packs/safety`

Expected: FAIL.

**Step 3: Implement hybrid evaluation**

Use exact canary and required/forbidden-content checks first. Apply contextual
rubrics only to ambiguity. Preserve paired-case identities for bias and
over-refusal analysis.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./packs/safety`

Expected: PASS with fake targets/judges.

Commit: `feat: add mpqt safety conduct pack`

### Task 9: Implement candidate/incumbent comparison

**Files:**
- Create: `mpqt/compare/paired.go`
- Create: `mpqt/compare/report.go`
- Test: `mpqt/compare/paired_test.go`

**Step 1: Write paired comparison tests**

Cover wins/losses/ties, missing cases, incompatible revisions, candidate
regression, newly unverified dimensions, latency/token trade-offs, trial
variance, randomized pairwise ordering metadata, and judge self-evaluation
disclosure.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./compare`

Expected: FAIL.

**Step 3: Implement comparison over immutable scorecards**

Require compatible run manifests and expose incompatibilities instead of
silently dropping cases.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./compare`

Expected: PASS.

Commit: `feat: add paired model quality comparison`

### Task 10: Add Go test integration and canonical JSON reports

**Files:**
- Create: `mpqt/mpqttest/run.go`
- Create: `mpqt/mpqttest/assert.go`
- Create: `mpqt/reportjson/codec.go`
- Test: `mpqt/mpqttest/run_test.go`
- Test: `mpqt/reportjson/codec_test.go`

**Step 1: Write presentation and round-trip tests**

Cover concise pack/dimension subtests, accepted dispositions, critical finding
output, unverified guarantees, report versioning, deterministic JSON, and
redaction.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./mpqttest ./reportjson`

Expected: FAIL.

**Step 3: Implement `Run`, `RequireDisposition`, and JSON output**

Delegate scenario execution to eval. MPQT only expands packs, builds the
scorecard, applies a profile when supplied, and renders the result.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./mpqttest ./reportjson`

Expected: PASS.

Commit: `feat: integrate mpqt with go test`

### Task 11: Add a build-tagged remote-model qualification example

**Files:**
- Create: `mpqt/examples/qualification/model_quality_test.go`
- Create: `mpqt/examples/qualification/testdata/profile.json`
- Create: `mpqt/README.md`

**Step 1: Write the example with a fake default and live build tag**

The offline example must run with a fake target. A separate
`//go:build qualification` test may construct a real inference client from
caller-provided configuration without embedding credentials.

**Step 2: Run offline example**

Run: `GOWORK=off go test -race ./examples/qualification`

Expected: PASS.

**Step 3: Document live execution**

Document `go test -tags qualification -count=1`, trial count, artifact path,
judge selection, target-class limitations, and profile semantics.

**Step 4: Verify all Phase 1 packages**

Run: `GOWORK=off go test -race ./...`

Run: `CGO_ENABLED=0 GOWORK=off go build -trimpath ./...`

Run: `make secure`

Expected: all commands PASS.

**Step 5: Commit**

Commit: `docs: add model power quality qualification example`

### Deferred Phase 2 plan: internet and agentic-security laboratory

Do not implement the egress pack until a separate approved plan covers:

- an eval sandbox evidence adapter mapping `Guarantees` and `CompileReport`;
- cooperative `http.RoundTripper` recording with redaction;
- recording proxy lifecycle and HTTPS visibility limits;
- enforcement that restricts a process to the proxy or reports `unverified`;
- canary encoding variants and leak detection;
- `URLAssessor` policy/reputation interfaces and revision provenance;
- metadata/private-network/DNS/redirect/download test fixtures;
- cross-platform sandbox conformance tests.
