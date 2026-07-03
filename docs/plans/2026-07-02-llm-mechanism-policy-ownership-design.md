# Design — LLM mechanism/policy ownership: retire the catalogue, own base URLs in clients, fail-close attestation

- **Date:** 2026-07-02
- **Status:** Implemented on branch `feat/llm-mechanism-policy-ownership` (Tasks 1–12 of the
  companion plan; every task spec- and quality-reviewed, `-race` suite + `make secure` green).
  Pre-merge follow-ups: ops confirmation of the Phala production host (`inference.phala.com`,
  §4.2) and the coordinated `swe` migration (§5, breaking — after this tags).
- **Scope:** `pkg/llm` and its sub-packages in the `looprig` SDK, plus a coordinated migration in the `swe` consumer.

## 1. Context

`looprig` is a reusable SDK: it ships the *mechanism* for talking to and verifying LLM
providers. The concrete agents and their model choices live in the downstream `swe`
repo — the SDK has **no in-repo consumers** of the catalogue constructors (verified: a
grep for `ChutesKimiK2`/`GeminiFlash`/`GLM46Phala`/`ClaudeOnBedrock`/`LMStudioLocal`
outside `catalog.go` and tests returns nothing).

Two places in `pkg/llm` conflate **mechanism** (how to reach/verify a provider — the
SDK's job) with **policy** (which models, which trust anchors — the consumer's job):

- **A — attestation policy.** `pkg/llm/providers/phala` bakes in fixture-derived,
  externally-unverified, rot-prone trust pins (app-id, two git commits, KMS root) as a
  shipped `DefaultPolicy()`. Worse, the underlying `aci.Policy` **fails open**: a
  zero-value `Policy{}` "accepts any genuine, quote-backed report" (`aci/policy.go:29-33`).
- **B — model catalogue.** `pkg/llm/catalog.go` ships opinionated model rows
  (`ChutesKimiK2()`, `GeminiFlash()`, …). Those rows are the **only** home for four
  provider base URLs (chutes `apiBase`, phala, openrouter, lmstudio), so the file is
  smuggling connection *mechanism* inside a bundle of model *policy*.

## 2. The decision rule

> **The SDK owns mechanism; the consumer owns policy. Every default the SDK keeps must
> be overridable, and must fail *secure* on ambiguity.**

Distinguishing test for a "provider fact":

| fact | trust/risk decision for the consumer? | owner |
|---|---|---|
| base URL (canonical host) | no — identical for every consumer of that gateway | **SDK** (in the client) |
| attestation trust pins | yes — the consumer bears the risk of trusting a wrong enclave, and pins rotate on redeploy | **consumer** |
| default `max_tokens`, chat path, timeouts | no — wire-required fallbacks, already overridable | **SDK** (already correct) |

This is the same "default-with-override, fail-secure" pattern the package already applies
correctly to `anthropicapi.defaultMaxTokens` (overridable via `Sampling.MaxTokens`),
`transport.DefaultChatPath` (overridable via `Endpoint`), and the aci/gemini timeouts.
The catalogue is the only place that broke it — by baking policy **and** by being the
sole, un-overridable home for mechanism.

## 3. Problem A — attestation policy

### 3.1 What's wrong

1. **Fails open (primary, security).** `aci.Policy` checks are "when configured": an empty
   accepted-set skips its step, so `Policy{}` allow-lists nothing and accepts any genuine
   report. `phala.New(base, key, aci.Policy{})` therefore silently disables all
   workload allow-listing. This violates CLAUDE.md *"Fail secure — on error or ambiguity,
   deny by default."*
2. **Rot-prone, unverified pins in the SDK.** `phala.go:22-54` documents the pins as
   recovered from a test fixture and **not** cross-checked against a published
   Phala/Dstack source ("BLOCKER #3"), and instructs future maintainers to *"prune stale
   commits as the gateway upgrades."* The SDK is carrying unverified trust anchors that
   go stale on every gateway redeploy — the same anti-pattern as the model rows.

### 3.2 Fix

- **A1 (security — fail closed at the aci boundary).** The guard must live at
  `aci.New`, not only `phala.New` — otherwise a direct `aci.New(base, key, aci.Policy{})`
  stays fail-open. Concretely:
  - Add `Policy.IsPinned() bool` — true when **any** of the four accepted sets
    (`AcceptedWorkloadIDs`, `AcceptedSourceProvenance`, `AcceptedAppIDs`,
    `AcceptedKMSRootPubKeys`) is non-empty. Rationale: the guard's job is to reject the
    *fail-open* case, and `verifyReport` runs a membership check for every configured set
    (steps 5/7/9, `verify.go:258,268`), so any non-empty set means **at least one
    allow-list gate runs** → not fail-open. `AcceptedWorkloadIDs` is therefore **included**
    — a workload-ID-only policy is the *strictest* pin (one exact keyset digest), so
    rejecting it as "unpinned" would be backwards. (Named `IsPinned`, not `HasAnchor`,
    precisely because a workload_id is a rotating leaf digest, not a stable trust
    "anchor"; it is a valid but brittle pin — it rejects a legitimately rotated keyset, so
    it fails *closed*, never open. Callers are steered toward the stable
    app-id/provenance/KMS anchors in docs, but the guard accepts any non-empty pin.)
  - Add an explicit, **detectable** opt-out for the legitimate "genuineness-only, no
    allow-list" case via a named `aci.UnpinnedPolicy()` constructor backed by an
    **unexported** `allowUnpinned bool` field. The field is unexported on purpose: a caller
    outside `aci` cannot set it with a struct literal, so `aci.UnpinnedPolicy()` is the
    *only* way to select unpinned mode — which is what makes the choice greppable and
    keeps an accidental `Policy{}` from ever opting out. (The accepted-set fields stay
    exported so provider/consumer packages still populate pins directly; only the danger
    flag is gated behind the constructor.)
  - Add one shared `Policy` gate — `func (p Policy) requireAcceptable() error` returning
    `*UnpinnedPolicyError` when `!p.IsPinned() && !p.allowUnpinned` — and apply it at **both
    public entry points**, so neither can be used genuineness-only by accident:
    - **`aci.New` signature → `(llm.LLM, error)`**, calling the gate before any network
      object exists. Blast radius is one production caller (`phala.go:96`), which already
      returns `(llm.LLM, error)`, so it just propagates; `phala.New` keeps no separate
      guard.
    - **`aci.VerifyReport`** (the one-shot verification entry, `verify.go:290`) calls the
      gate before running the chain, returning `*UnpinnedPolicyError`. Its doc comment is
      corrected — the current *"a zero `Policy{}` accepts any genuine report"* line and its
      `phala.DefaultPolicy()` reference are both now false and must be rewritten.
  - The **unexported `verifyReport`** (`verify.go:217`) stays **unguarded** — it is the
    pure low-level mechanism, reached only by the two guarded public entries and by test
    seams that legitimately exercise the chain with `Policy{}`. Guarding the public
    boundaries (not the internal runner) keeps the mechanism composable while closing every
    *public* fail-open path.
- **A2 (ownership — move the pins to swe) — DECIDED: full removal.** Delete the exported `DefaultPolicy()` and its
  four pinned consts from `pkg/llm/providers/phala`. `swe` constructs its own
  `aci.Policy` from its **own, verified** pins. `looprig` keeps the *mechanism*: the
  `aci.Policy` type, the verification chain (`aci/verify.go`), `phala.New`, the base-URL
  default (§4), and the A1 guard.
  - The fixture-derived pin *values* remain in `looprig`'s **test fixtures**
    (`aci/testdata/`, phala tests) to exercise the verification mechanism — they are test
    inputs, not shipped trust anchors. `swe` may copy them as a starting point but must
    verify them against a published Phala/Dstack source before production (BLOCKER #3
    ownership moves to the party that actually bears the risk).

Gentler alternative (**considered and rejected**): keep `DefaultPolicy()` but add only the
A1 guard and rename it to signal its unverified provenance. Rejected because a guarded
`DefaultPolicy()` still ships unverified, rot-prone trust anchors in the SDK, and callers
read *"Default"* as *blessed* — the exact false assurance this change removes. The only
reason to keep-and-guard would be short-term external API compatibility; that is not a
constraint here, so: remove, version-bump, and let `auto.New(ProviderPhala)` return the
typed "construct directly with a policy" error (A3).

- **A3 (`auto.New` can no longer construct Phala).** `auto.New` currently calls
  `phala.DefaultPolicy()` directly (`auto.go:63`). With A2 there is no SDK-side policy to
  supply, and `auto.New`'s only inputs are `(model, key)` — it cannot carry an attestation
  policy any more than it can carry SigV4 credentials. So **Phala joins Bedrock as a
  "construct directly" provider**: `auto.New` dispatches `ProviderPhala` to a typed
  `*PolicyNotConstructibleError` (modeled on the existing `*SigV4NotConstructibleError`)
  directing the caller to `phala.New(baseURL, key, policy)` with their own verified policy,
  and `auto` **drops its `phala` import** — matching the stated rule that `auto` imports
  only providers it can fully feed. This keeps the common path fail-closed and honest:
  there is no way to get an attested Phala client without consciously supplying a policy.
  (Rejected alternative: add a policy option/factory to `auto.New` — it re-couples the
  composition root to phala's policy type and re-introduces a defaultable policy, which is
  what A1/A2 exist to prevent.)

## 4. Problem B — model catalogue and base URLs

### 4.1 What's wrong

- `catalog.go` bundles model **policy** (name, caps, sampling) that is `swe`'s call.
- It is the **only** home for four base URLs, so deleting it orphans them. `gemini`
  already self-defaults its base (`gemini/client.go:34`) and even ignores
  `model.BaseURL`; `chutes` self-defaults its inference host (`defaultLLMBase`) but **not**
  its evidence host (`apiBase`); `phala`/`openrouter`/`lmstudio` have no client-side
  default at all. The result is a live **base-URL drift**: `catalog.go` says the phala base
  is `https://api.phala.network/v1` while `phala.New`'s doc says `https://inference.phala.com`.

### 4.2 Fix

- **B1 — delete `catalog.go` + `catalog_test.go`.** `swe` owns its model rows, built from
  the existing primitives (`llm.CustomModel` + `WithTools/WithImages/WithThinking/WithMaxContext/WithSampling`).
  No per-provider preset functions in `looprig` — those would re-create the per-provider
  surface this change removes.
- **B2 — base-URL defaults move into the clients (mechanism, closest to the connection).**
  The base-URL *values* live with the provider that connects there, **not** in core `llm`
  (core `llm` holds no endpoint URLs — that would reintroduce the smell):
  - `chutes.New`: default `apiBase` to the canonical chutes evidence host when `""`
    (const in the chutes package; it already defaults `llmBase`/NRAS/JWKS — this finishes
    the job).
  - `phala.New`: default `baseURL` to `https://inference.phala.com` when `""` (const in
    the phala package). This value is a **provisional, in-repo-evidence-based** choice — the
    phala tests (`policy_test.go:109-110`) and `phala.New`'s own doc both use it, and only
    the (deleted) `catalog.go` row said `api.phala.network/v1`, so the catalogue was the
    outlier. It has **not** been independently confirmed against a public authoritative
    Phala source and is **not** treated as authoritative here; **ops must confirm the
    production host before merge** (the drift is resolved *structurally* — one home for the
    value — regardless of which string wins).
  - `auto.genericHTTP`: default the base for `openrouter` (`https://openrouter.ai/api/v1`)
    and `lmstudio` (`http://localhost:1234/v1`) when `model.BaseURL == ""` (consts local to
    `auto`). These two share the generic transport and have no dedicated client, so the
    composition root is their natural default home. The default is written onto the
    **`Endpoint`** so `transport.buildRequest` always has a concrete, valid URL.
  - `gemini`: already self-defaults — no change.
- **B4 — generic-transport binding must tolerate an empty request base.**
  `transport.Client.checkBinding` (`transport/client.go:162`) rejects a request whose
  `Model.BaseURL != c.ep.BaseURL`. Once `auto.genericHTTP` defaults the *endpoint* to a
  concrete host but the caller's request `Model` still carries `BaseURL == ""`, that
  comparison would raise a spurious `*ModelMismatchError`. Fix: `checkBinding` compares
  `Provider` always, but compares `BaseURL` **only when the request base is non-empty** —
  an empty request base means "use the bound endpoint" (exactly the §4.3 contract), while a
  non-empty base that disagrees with the binding still fails closed (the real
  cross-wiring guard is preserved). No change needed to the gemini/bedrock clients: their
  `checkBinding` already compares provider only and ignores `BaseURL`.
- **B3 — `Model.Validate` carve-out generalized.** Replace the bedrock-only
  `if m.Provider == ProviderBedrock && m.BaseURL == ""` special case with a fail-closed
  predicate `func (p Provider) allowsEmptyBaseURL() bool` (in `provider.go`): true for
  every provider whose client self-defaults or is region-routed (all current providers),
  **false by default** so a future provider with no canonical endpoint still requires an
  explicit base. A non-empty base is always validated as https/loopback as today.

### 4.3 Resulting `Model.BaseURL` contract

- `""` → "use this provider's canonical endpoint" (the client fills it).
- non-empty → explicit override (proxy, self-hosted mirror, test server), still
  https/loopback-validated.

Symmetric with the existing `Sampling` override-vs-default. Not a safety regression: an
empty base resolves to a hard-coded canonical host, never an attacker-controlled one —
the same posture Bedrock already has. Binding (B4) honors the same contract: `""` binds to
the client's resolved endpoint; a non-empty override must match the binding or fail closed.

### 4.4 `Origin` after the change (DECIDED: keep, clarify docs)

With the catalogue gone, nothing in `looprig` sets `OriginCatalog`; every `swe`-built
model is `OriginCustom` (the fail-safe zero value — honest, since `swe` is now asserting
the caps). **Keep** the `OriginCatalog` constant and its `String()` case — removing an
exported enum value `swe` may reference is a breaking change for near-zero safety gain.
Only the doc comment changes, to decouple the meaning from the SDK:

> `OriginCatalog` — capabilities came from a curated catalogue maintained by the consumer
> or integration layer (not necessarily this SDK); capabilities are trusted.

`swe` stamps `OriginCatalog` on its own curated rows to mark trusted caps; a raw
`Model{}`/`CustomModel` stays `OriginCustom`.

## 5. File-level change list

### looprig
- **Delete** `pkg/llm/catalog.go`, `pkg/llm/catalog_test.go`.
- `pkg/llm/provider.go`: add `allowsEmptyBaseURL()` predicate (fail-closed default).
- `pkg/llm/model.go`: `Validate` uses the predicate instead of the bedrock special case.
- `pkg/llm/transport/client.go`: `checkBinding` compares `BaseURL` only when the request
  base is non-empty (B4).
- `pkg/llm/providers/chutes/client.go`: default `apiBase` when `""`.
- `pkg/llm/providers/phala/phala.go`: default `baseURL` to `https://inference.phala.com`
  when `""`; **remove** the four pinned consts and `DefaultPolicy()`; `New` forwards
  `aci.New`'s error (no separate guard).
- `pkg/llm/aci/policy.go`: add `IsPinned()` (any accepted set non-empty), the unexported
  `allowUnpinned` field, the `UnpinnedPolicy()` constructor, and the shared
  `requireAcceptable()` gate + `*UnpinnedPolicyError` type. Update the type doc: `Policy{}`
  is no longer a silently-accepted "accepts any genuine report" value at the public
  boundaries.
- `pkg/llm/aci/client.go`: **`New` signature → `(llm.LLM, error)`**; call
  `requireAcceptable()` and fail closed. Update the one caller (`phala.New`) and aci tests.
- `pkg/llm/aci/verify.go`: **`VerifyReport`** calls `requireAcceptable()` before running the
  chain; rewrite its doc comment (drop the "zero `Policy{}` accepts any genuine report" line
  and the `phala.DefaultPolicy()` reference). The unexported `verifyReport` stays unguarded.
- `pkg/llm/origin.go`: reword the `OriginCatalog` comment to decouple it from the SDK
  (§4.4) — no code change to the enum.
- `pkg/llm/auto/auto.go`: dispatch `ProviderPhala` to a typed `*PolicyNotConstructibleError`
  and **drop the `phala` import** (A3); `genericHTTP` writes the openrouter/lmstudio default
  base onto the `Endpoint` (B2/B4).
- Tests: table-driven coverage for the predicate, each client's self-default, the
  `Validate` empty-vs-override matrix, `checkBinding` empty-vs-override, `IsPinned`
  (including the **workload-ID-only → pinned** case and the all-empty → unpinned case), the
  fail-closed `aci.New`/`phala.New` path, the `UnpinnedPolicy()` opt-out, and the
  `auto.New` Phala error dispatch.

### swe (coordinated, breaking — separate PR in the swe repo)
- Add an internal catalogue (`CustomModel` rows) replacing the deleted constructors; base
  omitted → provider default.
- Construct its own phala `aci.Policy` from its own verified pins (starting from the
  looprig fixture values, verified against a published source).

## 6. Security considerations

- Attestation now **fails closed at every public aci entry** — both `aci.New` (client
  construction; covers `phala.New` and any future provider) and `aci.VerifyReport` (one-shot
  verification). An unpinned/under-specified policy is rejected before any chain runs or
  network object exists; the only way to accept an unallow-listed genuine enclave is the
  explicit, greppable `aci.UnpinnedPolicy()` opt-in — its backing `allowUnpinned` flag is
  unexported, so no struct literal (accidental `Policy{}` or otherwise) can reach it from
  outside `aci`. The internal `verifyReport` stays composable for tests and the
  already-guarded client path.
- BLOCKER #3 (verify pins against a published Phala/Dstack source) moves to `swe`, the
  party that runs against a specific gateway and bears the trust risk.
- Base-URL defaulting cannot fail open to an attacker-controlled host: `""` resolves to a
  compile-time constant canonical host; any override is https/loopback-validated.

## 7. Blockers / open decisions

### Resolved in this revision (were review blockers)
- **`auto.New` Phala policy source** → A3: dispatch to a typed
  `*PolicyNotConstructibleError` and drop the `phala` import (mirrors Bedrock).
- **Fail-closed guard must cover `aci.New`, not just `phala.New`** → A1: `aci.New` becomes
  `(llm.LLM, error)` and is the single fail-closed boundary; one caller updated.
- **Generic-HTTP binding vs defaulted endpoint** → B4: `transport.checkBinding` compares
  `BaseURL` only when the request base is non-empty; `genericHTTP` writes the default onto
  the `Endpoint`.
- **`IsPinned` must include `AcceptedWorkloadIDs`** → included: `verifyReport` enforces
  workload_id when configured (`verify.go:268`), so a workload-ID-only policy is a valid
  (strict, brittle) pin, not fail-open. Renamed `HasAnchor`→`IsPinned` to say so.
- **Opt-out API shape** → `aci.UnpinnedPolicy()` constructor backed by an **unexported**
  `allowUnpinned` flag (only the constructor can set it; no struct literal can).
- **`aci.VerifyReport` is a second public fail-open surface** → guarded too: the shared
  `requireAcceptable()` gate is applied at both public entries (`New` + `VerifyReport`);
  the unexported `verifyReport` stays pure.
- **Phala base-URL** → `https://inference.phala.com` chosen from in-repo evidence but
  **not** treated as authoritative; ops must confirm the production host before merge. The
  drift is resolved structurally regardless.

### Decided (owner confirmed)
- **Move the catalogue to swe** — yes; looprig ships mechanism only.
- **A2 → full removal** of `DefaultPolicy()` + the four pins (best security posture; no API
  compatibility constraint forcing keep-and-guard).
- **`OriginCatalog` → keep**, with the comment reworded to mean "curated by the
  consumer/integration layer, not necessarily the SDK" (§4.4).

### Remaining (process, not design)
1. **Ops confirmation** of the Phala production host before merge.
2. **swe migration sequencing** — land the looprig change behind a version bump; the swe PR
   follows. (`go.work`: build/verify looprig with `GOWORK=off` per repo convention.)

## 8. Out of scope

- Actually re-deriving/verifying the phala pins against a published source (a manual
  security task now owned by `swe`).
- Unrelated tracked LLM work (bedrock streaming/Converse, chutes fail-closed discovery).
