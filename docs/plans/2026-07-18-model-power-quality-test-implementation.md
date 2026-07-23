# Model Power Quality Test Phase 1 Implementation Plan

> **SUPERSEDED (2026-07-22):** execute
> `2026-07-22-mpqt-phase1-detailed-implementation.md` instead — the same scope
> expanded to verified-API, complete-code detail and validated by compiling its
> code against the live modules. This file remains as the approved outline.

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
- Create: `mpqt/CLAUDE.md`
- Create symlink: `mpqt/AGENTS.md` -> `CLAUDE.md`
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

### Task 7: Build instruction-following and known-answer capability families

**Files:**
- Create: `mpqt/packs/capability/instruction_v1.go`
- Create: `mpqt/packs/capability/knowledge_v1.go`
- Create: `mpqt/packs/capability/testdata/instruction_v1/*.json`
- Create: `mpqt/packs/capability/testdata/knowledge_v1/*.json`
- Test: `mpqt/packs/capability/instruction_v1_test.go`
- Test: `mpqt/packs/capability/knowledge_v1_test.go`

**Step 1: Write fixture validation tests**

Start with 5–10 reviewed scenarios per family. Instruction cases cover explicit
single and multi-constraint requests, conflicting lower-priority text, required
format, and forbidden action. Knowledge cases use closed-book answers with
versioned authoritative references. Every scenario identifies its programmatic
expectations and any model rubric.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./packs/capability`

Expected: FAIL.

**Step 3: Implement the two auditable families**

Prefer exact evaluators. Add composite evaluators only where deterministic facts
cannot decide the intended semantic property. Label known-answer QA as factual
consistency against the supplied reference, not open-world truthfulness.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./packs/capability`

Expected: PASS with fake targets/judges.

Commit: `feat: add instruction and known answer capability families`

### Task 8: Build long-context and conversation-consistency families

**Files:**
- Create: `mpqt/packs/capability/context_v1.go`
- Create: `mpqt/packs/capability/consistency_v1.go`
- Create: `mpqt/packs/capability/testdata/context_v1/*.json`
- Create: `mpqt/packs/capability/testdata/consistency_v1/*.json`
- Test: `mpqt/packs/capability/context_v1_test.go`
- Test: `mpqt/packs/capability/consistency_v1_test.go`

**Step 1: Write fixture and metamorphic-pair tests**

Author 5–10 reviewed scenarios per family. Context cases seed exact facts near
the beginning, middle, and end and include relevant distractors. Consistency
cases use paired multi-turn conversations that preserve or deliberately change
one prior fact, commitment, or user constraint.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./packs/capability -run 'Test(Context|Consistency)'`

Expected: FAIL.

**Step 3: Implement exact retrieval and hybrid consistency evaluation**

Measure seeded-fact retrieval programmatically. Use a contradiction/consistency
rubric only after attaching the exact prior-turn evidence to the sample.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./packs/capability -run 'Test(Context|Consistency)'`

Expected: PASS with fake targets/judges.

Commit: `feat: add context and consistency capability families`

### Task 9: Build clarification, uncertainty, and multilingual families

**Files:**
- Create: `mpqt/packs/capability/interaction_v1.go`
- Create: `mpqt/packs/capability/multilingual_v1.go`
- Create: `mpqt/packs/capability/testdata/interaction_v1/*.json`
- Create: `mpqt/packs/capability/testdata/multilingual_v1/*.json`
- Test: `mpqt/packs/capability/interaction_v1_test.go`
- Test: `mpqt/packs/capability/multilingual_v1_test.go`

**Step 1: Write scenario and calibration tests**

Author 5–10 reviewed cases for clarification and a small, explicitly disclosed
language subset. Add known-answer confidence cases distributed across difficulty
buckets so calibration is computed over repeated outcomes rather than judged
from confident wording alone.

**Step 2: Run and verify failure**

Run: `GOWORK=off go test -race ./packs/capability -run 'Test(Interaction|Calibration|Multilingual)'`

Expected: FAIL.

**Step 3: Implement hybrid interaction evaluation**

Use a judge for whether ambiguity warranted clarification, programmatic binned
accuracy for calibration, and exact facts plus language-specific rubrics for the
multilingual subset. The pack metadata must disclose case and language counts;
v1 is a seed pack, not a comprehensive benchmark.

**Step 4: Assemble `capability.CoreV1`, verify, and commit**

Create `mpqt/packs/capability/v1.go` to compose Tasks 7–9 without duplicating
scenarios or evaluators.

Run: `GOWORK=off go test -race ./packs/capability`

Expected: PASS with fake targets/judges.

Commit: `feat: assemble mpqt core capability pack`

### Task 10: Build the safety-conduct pack

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

### Task 11: Implement candidate/incumbent comparison

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

### Task 12: Add Go test integration and canonical JSON reports

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
scorecard, applies a profile when supplied, and renders the result. Pass
`mpqt.Run.Trials` directly to `eval.RunConfig.Trials`; MPQT must not implement a
second scenario/trial loop.

**Step 4: Verify and commit**

Run: `GOWORK=off go test -race ./mpqttest ./reportjson`

Expected: PASS.

Commit: `feat: integrate mpqt with go test`

### Task 13: Add a build-tagged remote-model qualification example

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
