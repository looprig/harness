# Model Power Quality Test (MPQT) — Design Specification

**Date:** 2026-07-18
**Status:** Approved design
**Depends on:** [Reusable Evaluation Framework](2026-07-18-eval-framework-design.md)

## Summary

Model Power Quality Test (MPQT) qualifies a newly released or newly configured
model for enterprise use. The name borrows from electrical power-quality
testing: a model is placed under representative load, faults, distorted inputs,
and hostile conditions; its output quality, stability, safety, and unintended
side effects are measured before it is connected to an organization's workload.

MPQT is a product and test-pack layer over `github.com/looprig/eval`. It does not
add a second evaluation engine. It provides model targets, enterprise-oriented
scenario packs, scoring profiles, sandbox/proxy fixtures, comparative reports,
and a Go test integration.

Every run produces an objective, versioned scorecard. An optional organization
qualification profile applies the organization's thresholds and yields one of:
`qualified`, `restricted`, `rejected`, or `unverified`. The derived disposition
is release policy, not an evaluator action and not a runtime classifier.

## Goals

- Determine whether a model or model configuration is fit for specified
  organizational use cases.
- Separate objectively observed behavior from organization-specific acceptance
  policy.
- Prefer deterministic and operational evidence; use LLM judges only where
  semantic interpretation is necessary.
- Test both model responses and agentic behavior with tools.
- Detect attempted internet access, suspicious destinations, data exfiltration,
  and policy bypass when the target can execute locally.
- Record the actual isolation guarantees achieved on the host.
- Compare candidate models with an incumbent using the same cases, trials,
  sampling settings, tools, policies, and judge configuration.
- Run from ordinary Go tests and CI without requiring a hosted service.

## Non-goals

- Certifying a model as universally safe or truthful.
- Observing undisclosed server-side activity inside a remote model provider.
- Taking runtime action against a live conversation.
- Replacing legal, privacy, security, or procurement review.
- Defining one universal threshold profile for every organization.
- Treating a judge model as ground truth.

## Product shape

```text
MPQT test packs + profiles + fixtures
                  |
                  v
eval Scenario -> model/agent/process Target -> Observation
                  |
                  v
programmatic + model + composite Evaluators
                  |
                  v
objective Scorecard -> optional QualificationProfile -> Disposition
                  |
                  v
Go test / JSON / HTML consumer / OTel / enterprise storage
```

Recommended repository layout:

```text
github.com/looprig/mpqt
├── mpqt.go                 # suite, model-under-test, run manifest
├── profile/                # qualification profiles and disposition logic
├── report/                 # scorecard comparison and presentation model
├── packs/capability/
├── packs/safety/
├── packs/agenticsecurity/
├── packs/internet/
├── packs/operational/
├── packs/robustness/
├── fixture/tools/
├── fixture/proxy/
├── fixture/canary/
└── mpqttest/               # Go testing helpers
```

The MPQT module imports eval and selected adapters. The eval module never imports
MPQT.

## Model under test

```go
type ModelUnderTest struct {
    ID            string
    Provider      string
    Model         string
    Revision      string
    EndpointClass EndpointClass
    Capabilities  Capabilities
    Configuration Configuration
}
```

The run manifest records model identity, endpoint, API format, sampling,
reasoning effort, system prompt, structured-output mechanism, tool set, context
window assumptions, client revision, and environment. Secrets are never part of
the manifest.

Three target classes are distinct:

1. **Remote inference target.** MPQT sees requests, responses, tool calls, usage,
   latency, and errors. It cannot see provider-internal network activity.
2. **Agent target.** MPQT sees the conversation and instrumented tool/process/
   network behavior produced by the surrounding agent runtime.
3. **Local or foreign-process target.** MPQT executes the target under sandbox
   and proxy controls and can observe process-local side effects and attempted
   egress, subject to reported enforcement guarantees.

A report must identify its target class. A remote inference result cannot claim
that process-level phone-home behavior was tested.

## Qualification categories and evaluation method

The default packs use three methods:

- **P** — programmatic: exact, statistical, protocol, or operational check.
- **J** — model judge: semantic assessment using a versioned rubric.
- **H** — hybrid: programmatic evidence plus a semantic judgment.

### Capability

| Metric | Method | Notes |
|---|---:|---|
| Structured-output validity and repair rate | P | Schema validation, attempts, and terminal errors. |
| Exact instruction/constraint compliance | P | Required/forbidden fields, actions, phrases, or formats. |
| Open-ended instruction adherence | H | Exact constraints plus rubric judgment. |
| Tool selection, arguments, and ordering | P | Known-tool fixtures and argument schemas. |
| Tool-choice quality with several valid paths | H | Exact validity plus judge assessment of efficiency. |
| Tool error recovery | P/H | Sequence is exact; recovery quality may need judgment. |
| Task completion and goal progress | H | Terminal invariants plus session rubric. |
| Clarification behavior | J | Whether ambiguity materially required a question. |
| Long-context retrieval / lost-in-the-middle | P | Seeded facts and exact citations. |
| Grounding against supplied sources | H | Claim-source matching plus rubric. |
| Known-answer factual QA | P | Canonical answer set with normalization. |
| Open-world truthfulness | H | External verifier where available; otherwise unverified. |
| Calibrated uncertainty | P/H | Binned accuracy/calibration plus wording quality. |
| Conversation consistency | P/H | Contradiction rules plus session rubric. |
| Multilingual task quality | P/H | Exact facts plus language-specific judgment. |

### Safety and conduct

| Metric | Method | Notes |
|---|---:|---|
| Toxicity, hate, harassment, sexual/violent content | P/H | Lexical/classifier signal plus contextual rubric. |
| Vulgarity | P/H | Exact lexicon where sufficient; judge handles quoted/contextual use. |
| Self-harm and illegal assistance policy behavior | P/H | Required refusal elements plus proportionality judgment. |
| Over-refusal and under-refusal | H | Exact allowed/forbidden content plus rubric. |
| Sycophancy and manipulation | J/H | Counterfactual pairs and semantic judgment. |
| Bias and disparate treatment | P/H | Paired statistical tests plus explanation quality. |
| Prompt-injection and jailbreak resistance | H | Canary/tool-policy checks plus rubric. |
| System prompt, PII, and secret leakage | P | Seeded canaries, hashes, and exact match variants. |
| Untrusted-content handling | H | Action sequence plus instruction-boundary judgment. |

### Agentic security

| Metric | Method | Notes |
|---|---:|---|
| Prohibited tool and bypass attempts | P | Tool/process/network trace. |
| Shell, path, and argument injection | P | Controlled fixtures and side-effect assertions. |
| Secret-file reads and out-of-workspace writes | P | Canary filesystem plus sandbox evidence. |
| Persistence and privilege-escalation attempts | P | Process/file activity and denied operations. |
| Sandbox escape behavior | P | Adversarial fixtures; never infer from prose. |
| Destructive action restraint | P/H | Side effects plus necessity/proportionality judgment. |
| Package/supply-chain behavior | P/H | Registry/download evidence plus dependency relevance. |
| Metadata-service, private-network, and port scanning | P | Proxy/network evidence. |
| Recovery after a denied action | P/H | Retry/bypass sequence plus behavioral rubric. |

### Internet and egress quality

Internet behavior is a first-class qualification category, not a subcase of tool
correctness.

| Metric | Method | Notes |
|---|---:|---|
| Unrequested internet access | P | Any network activity when the scenario disallows it. |
| Approved mechanism use | P | Browser/search tool versus direct shell/process HTTP. |
| Domain, IP, port, protocol, and HTTP method | P | Recording proxy and DNS evidence. |
| IP-literal and alternate-protocol bypass | P | Controlled fixtures. |
| Loopback, private, link-local, and metadata access | P | Network-class evaluator. |
| DNS-over-HTTPS or alternate resolver use | P | Destination/protocol evidence where observable. |
| Redirect chain and final destination | P | Full proxy chain, not only initial URL. |
| Upload methods and bytes sent | P | Method, content length, and redacted payload classification. |
| Canary/PII/secret exfiltration | P | Seeded values and derived encodings/hashes. |
| Trackers, ads, paste/file-sharing, URL shorteners | P | Pluggable URL assessment. |
| Executable and package downloads | P/H | Artifact facts plus task relevance. |
| Retry or bypass after denial | P | Repeated destinations, protocols, or mechanisms. |
| Claimed browsing without network evidence | P | Conversation claim versus trace. |
| Citation reachability and source identity | P | Fetch/DNS/TLS outcome and redirect identity. |
| Site relevance and necessity | J/H | Judge sees the task and sanitized request metadata. |

### Operational quality

| Metric | Method | Notes |
|---|---:|---|
| Time to first token and total latency | P | Distributions, not only averages. |
| Tool and step latency | P | Correlated spans/operations. |
| Input, output, cache, and reasoning tokens | P | Provider-normalized usage. |
| Cost estimate | P | Optional versioned price table external to core eval. |
| Error and structured-output failure rate | P | Typed terminal state. |
| Runaway loops and redundant tool calls | P | Counts and repeated signatures. |
| Trial-to-trial variance | P | Same-case repeated trials. |
| Concurrency degradation | P | Controlled load profiles. |
| Context-length degradation | P | Bucketed by message/token size. |

### Robustness

| Metric | Method | Notes |
|---|---:|---|
| Typos and noisy phrasing | P/H | Metamorphic expected invariants plus quality rubric. |
| Unicode, homoglyph, and delimiter attacks | P | Exact policy and parser invariants. |
| Irrelevant or contradictory context | P/H | Seeded facts plus conflict-handling rubric. |
| Malformed tool results | P | Controlled tool fixture. |
| Reordered or duplicated messages | P/H | Metamorphic comparison. |
| Long and repeated inputs | P | Stability, latency, and bounded behavior. |
| Repeated denials and adversarial persistence | P/H | Action trace plus behavioral quality. |

## Internet observation architecture

### Cooperative HTTP and tool traffic

For Harness fetch/search/browser tools and other controlled HTTP clients, use an
instrumented `http.RoundTripper`. This provides full request URL, method,
redirects, response status, timing, and byte counts without decrypting traffic
at a proxy. Headers and bodies are redacted or classified before becoming eval
evidence.

### Local and foreign processes

Run the target with `HTTP_PROXY`, `HTTPS_PROXY`, and explicit resolver settings
pointing to a recording proxy, then use sandbox policy to restrict outbound
traffic to the proxy. The report includes sandbox `Guarantees` and
`CompileReport` evidence.

Important limitations remain explicit:

- A conventional HTTPS proxy sees the CONNECT host and port, not encrypted path
  or body content.
- Full HTTPS inspection requires an explicitly trusted test CA and a controlled
  test environment; it is never the default.
- A non-cooperative process may ignore proxy environment variables. Complete
  capture then requires transparent redirect/enforcement support not currently
  guaranteed by the sandbox module.
- If network or address-level enforcement is unavailable, `NoEgress` and
  metadata-access results are `unverified`, not pass.
- MPQT cannot observe network activity occurring inside a remote provider's
  infrastructure.

Sandbox currently offers process, filesystem, environment, network, and
resource-limit guarantees, but domain-level egress interception and rich denial
telemetry require follow-on work. MPQT consumes what sandbox actually reports;
it does not overstate the boundary.

### URL assessment

Whether a destination is acceptable is not delegated to an LLM judge. MPQT uses
a pluggable deterministic interface:

```go
type URLAssessor interface {
    Assess(context.Context, URLObservation) (URLAssessment, error)
}
```

Assessors may combine:

- organization allow/block policy;
- domain and package-registry allowlists;
- IP/network classification;
- malware, phishing, and reputation feeds;
- newly registered domain data;
- redirect-chain and URL-shortener detection;
- TLS identity;
- tracker, advertising, paste, file-sharing, and executable-download rules.

Each verdict records the assessor and data revision. An unavailable reputation
provider yields `unknown`, not safe. A model judge may separately assess whether
the destination was relevant and proportionate to the user's task.

## Test packs

A pack is a versioned set of scenarios, evaluators, fixtures, required target
capabilities, and score dimensions:

```go
type Pack struct {
    Name         string
    Revision     string
    Scenarios    []eval.Scenario
    Evaluators   []eval.Evaluator
    Requires     []Capability
    Dimensions   []Dimension
}
```

Initial packs should be small and auditable:

1. `core-capability-v1`
2. `structured-output-v1`
3. `tool-use-v1`
4. `safety-conduct-v1`
5. `prompt-injection-v1`
6. `agentic-security-v1`
7. `internet-egress-v1`
8. `operational-stability-v1`

Scenario data includes provenance and licensing information. Generated and
curated data remain distinguishable. Organization-private cases can extend a
pack without being uploaded or published.

## Scorecard and qualification profile

The objective scorecard reports per-case assessments and distributional rollups
by dimension. It retains rates for `fail`, `error`, `unverified`, and `skipped`;
these are never collapsed into a single average.

```go
type QualificationProfile struct {
    Name       string
    Revision   string
    Required   []Requirement
    Restrictions []RestrictionRule
}
```

A requirement may specify a minimum percentile, maximum failure rate, zero
tolerance for a finding code, required evidence guarantees, minimum sample
count, maximum variance, or a comparison margin against the incumbent.

Disposition semantics:

- **qualified** — every mandatory requirement is demonstrably met.
- **restricted** — safe only for declared capabilities, tools, data classes, or
  deployment boundaries.
- **rejected** — a mandatory requirement is demonstrably violated.
- **unverified** — evidence, trial count, enforcement, or evaluator availability
  is insufficient to decide.

Profiles are policy data evaluated after the scorecard. They do not change raw
assessments and are not used by continuous eval to act on a session.

## Comparative model testing

Candidate and incumbent runs use a paired design where possible: identical
scenario, target wrapper, tool fixtures, seed or sampling settings, trial count,
evaluator revisions, and judge configuration. Reports include:

- absolute distributions and confidence intervals;
- paired win/loss/tie counts for semantic comparisons;
- regression and improvement deltas by dimension;
- new failure codes and newly unverified requirements;
- latency, token, and cost trade-offs;
- variance and flaky-case identification.

The judge should not know which model is the candidate. Pairwise ordering is
randomized or counterbalanced to reduce position bias. A model must not judge
its own output by default; any exception is prominent in provenance.

## Go test usage

```go
func TestCandidateModelPowerQuality(t *testing.T) {
    candidate := mpqt.RemoteModel(candidateClient, candidateManifest)

    scorecard := mpqttest.Run(t, mpqt.Run{
        Target: candidate,
        Packs: []mpqt.Pack{
            capability.CoreV1(),
            structuredoutput.V1(),
            tooluse.V1(toolFixture),
            safety.ConductV1(),
        },
        Trials: 3,
    })

    mpqttest.RequireDisposition(t, scorecard, orgProfile,
        mpqt.Qualified, mpqt.Restricted)
}
```

Process egress test:

```go
func TestLocalModelDoesNotPhoneHome(t *testing.T) {
    target := mpqt.LocalProcess(processConfig,
        sandboxtarget.WithExecutor(executor),
        proxytarget.WithRecorder(proxy),
        canarytarget.WithSecrets(canaries),
    )

    scorecard := mpqttest.Run(t, mpqt.Run{
        Target: target,
        Packs:  []mpqt.Pack{internet.EgressV1(), security.AgenticV1()},
    })

    mpqttest.RequireVerified(t, scorecard, "network_boundary")
    mpqttest.RequireNoFinding(t, scorecard, "internet.unrequested_egress")
    mpqttest.RequireNoFinding(t, scorecard, "internet.canary_exfiltration")
}
```

## Reporting

An MPQT report contains:

- run manifest and reproducibility fingerprint;
- objective dimension scorecard;
- organization profile and derived disposition, if used;
- per-pack and per-scenario results;
- critical findings with redacted evidence;
- unavailable or weakened guarantees;
- candidate/incumbent comparison;
- evaluator, rubric, judge, schema, threat-feed, dataset, and policy revisions;
- environment and target-class limitations.

JSON is the canonical portable form. Go test output is concise and points to the
report artifact. A future HTML renderer or hosted dashboard consumes JSON rather
than owning evaluation semantics. OTel export is intended for trends and run
health, not as the only store for detailed qualification evidence.

## Security and privacy

- Test secrets are synthetic canaries, never production credentials.
- Proxy and filesystem evidence is redacted before general sinks.
- Raw request/response capture is explicit, encrypted, access-controlled, and
  retention-limited.
- Test packs declare whether they may send prompts to an external judge.
- A confidential workload may select a local or approved judge, or disable
  model-based evaluators and accept `unverified` semantic dimensions.
- URL reputation services receive the minimum required destination data.
- No report claims enforcement beyond recorded sandbox guarantees.

## Delivery phases

### Phase 1 — Scorecard foundation

- Eval core, `evaltest`, dataset codec, inference target, structured judge.
- Core capability, structured-output, tool-use, and conduct packs.
- JSON scorecard and qualification profile.

### Phase 2 — Agentic and continuous evidence

- Harness read-only adapter and OTel integration.
- Tool/process/file/network evidence vocabulary.
- Agentic-security and prompt-injection packs.

### Phase 3 — Egress laboratory

- Recording proxy adapter, canary fixtures, URL assessor interface.
- Sandbox evidence adapter and guarantee-aware verdicts.
- Internet/egress pack for cooperative HTTP and process targets.

### Phase 4 — Comparative and enterprise workflows

- Paired candidate/incumbent analysis, confidence reporting, trend baselines.
- Golden-candidate review workflow and private organization packs.
- Optional report UI and enterprise storage integrations.

## Acceptance criteria

- The same eval engine runs a response-quality case, a tool-use case, and a
  sandbox/proxy security case.
- Every metric declares programmatic, model, or composite method.
- A report distinguishes target failure, evaluator failure, quality failure,
  and missing evidence.
- Internet tests record actual domains/IPs/ports/protocols and sandbox
  guarantees where observable.
- Destination safety uses a versioned `URLAssessor`; semantic relevance may use
  a judge.
- Remote provider runs disclose that provider-internal phone-home behavior is
  outside the observation boundary.
- A scorecard is stable without an organization profile.
- A profile can derive qualified, restricted, rejected, or unverified without
  mutating raw results.
- All packs run through `go test`; live or costly packs are selectable with
  build tags and `-count=1`.

