# ACI Confidential-Inference Client (`pkg/llm/aci`) Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a reusable, provider-agnostic Go client for the Dstack `private-ai-gateway`
(`aci/1`) that does full-DCAP attestation, ACI **E2EE v2** request/response sealing, and
mandatory signed-receipt verification — wired in as the looprig `ProviderPhala` backend.

**Architecture:** New self-contained `pkg/llm/aci` package implementing `llm.LLM`
end-to-end (it owns the confidential round-trip; a "provider" is just `aci.New(baseURL,
apiKey, policy)`). Builds on existing `pkg/llm/tee` (TDX quote) and `pkg/llm/openaiapi`
(OpenAI-compatible codec). `auto.New(ProviderPhala)` routes to it; the obsolete
`pkg/llm/openaiapi/phala` is removed. Companion design:
`docs/plans/2026-06-24-aci-confidential-inference-client-design.md` (READ IT — it has the
exact constants this plan references).

**Tech Stack:** Go 1.26; `crypto/sha256`, `crypto/sha512`, `crypto/ed25519`,
`golang.org/x/crypto/sha3` (Keccak), `golang.org/x/crypto/hkdf`, `crypto/aes`+`cipher`
(AES-256-GCM), `github.com/decred/dcrd/dcrec/secp256k1/v4` (+`/ecdsa`) — approved in
`CLAUDE.md`; DCAP via `pkg/llm/tee` (`github.com/google/go-tdx-guest`).

**Spec provenance:** Dstack `private-ai-gateway` pinned at commit
`1b43f76e43c2459856faebe9cd97d8e01cb0df0c` (== the deployed gateway's `source_provenance.
repo_commit`). All wire constants below are from `src/aci/*` at that commit.

**Conventions for every task:** branch is `aci-confidential-inference` (looprig). Run all
`go`/`make` from `/Users/ipotter/code/looprig`. TDD: write failing test → run (see it fail)
→ implement minimal → run (pass) → `make secure` → commit. Live tests skip without
`PHALA_API_KEY`.

---

## Phase 0 — Fixtures & dependency

### Task 0.1: Vendor the secp256k1 dependency

**Files:** Modify: `go.mod`, `go.sum`

**Step 1:** `cd /Users/ipotter/code/looprig && GOWORK=off go get github.com/decred/dcrd/dcrec/secp256k1/v4@latest`
**Step 2:** Verify it resolves: `go list -m github.com/decred/dcrd/dcrec/secp256k1/v4`
**Step 3:** `go mod verify && make vuln` — expect no new vulns.
**Step 4:** Commit: `git add go.mod go.sum && git commit -m "build(aci): add decred secp256k1/v4 (approved in CLAUDE.md)"`

### Task 0.2: Capture real fixtures

**Files:** Create: `pkg/llm/aci/testdata/report_aci1.json`, `pkg/llm/aci/testdata/receipt_aci1.json`, `pkg/llm/aci/testdata/jcs_vectors.json`

**Step 1:** Copy the already-captured report: the scratchpad has a real `aci/1` report; save a fresh one bound to a known nonce:
```bash
KEY=$(grep -E '^PHALA_API_KEY=' /Users/ipotter/code/swe/.env | cut -d= -f2-)
NONCE=0000000000000000000000000000000000000000000000000000000000000001
curl -s -H "Authorization: Bearer $KEY" \
  "https://inference.phala.com/v1/aci/attestation?model=z-ai/glm-5.2&nonce=$NONCE" \
  > pkg/llm/aci/testdata/report_aci1.json
```
**Step 2:** Capture a real receipt via a round-trip (small chat → grab `x-receipt-id` → fetch receipt). Script it in `testdata/` capture notes; save `receipt_aci1.json`.
**Step 3:** Author `jcs_vectors.json` — fixed input→output canonicalization vectors. Seed with hand-computed cases (empty object, key-order, integer, unicode-escape, UTF-16 key ordering above BMP) AND cross-check against the Rust `examples/verify_aci_artifacts` JCS where derivable. Each entry `{ "value": <json>, "canonical": "<exact bytes>", "sha256": "sha256:<hex>" }`.
**Step 4:** Commit: `git add pkg/llm/aci/testdata && git commit -m "test(aci): real aci/1 report+receipt + fixed JCS vectors"`

> NOTE: the report/receipt fixtures are real and time-bound (`freshness`); tests that touch
> freshness must inject a fixed `now` inside `[fetched_at, stale_after)`.

---

## Phase 1 — Foundation: errors, JCS, types

### Task 1.1: Errors + version constant

**Files:** Create `pkg/llm/aci/errors.go`, `pkg/llm/aci/errors_test.go`

**Step 1 (test):** assert `SupportedAPIVersion == "aci/1"`; `ErrUnsupportedAPIVersion` is an `error` carrying the offending version; a typed `*AttestError{Reason, Err}` formats without leaking secrets.
**Step 2:** run `go test ./pkg/llm/aci/ -run Errors -v` → FAIL (undefined).
**Step 3 (impl):** `const SupportedAPIVersion = "aci/1"`; `type Reason string` with the set from the design (`unsupported_api_version`, `attestation_malformed`, `report_data_mismatch`, `binding_mismatch`, `quote_invalid`, `tcb_revoked`, `keyset_digest_mismatch`, `endorsement_invalid`, `kms_root_untrusted`, `policy_rejected`, `stale_report`, `receipt_invalid`, `upstream_unverified`, `e2ee_failed`); `AttestError`; `ErrUnsupportedAPIVersion(got string) error`.
**Step 4:** run → PASS. **Step 5:** `make secure` then commit.

### Task 1.2: Constrained JCS canonicalizer

**Files:** Create `pkg/llm/aci/jcs.go`, `pkg/llm/aci/jcs_test.go`

This is interop-critical — port the Dstack constrained profile (NOT generic RFC 8785).

**Step 1 (test):** table test driven by `testdata/jcs_vectors.json`: for each vector,
`Canonicalize(value)` equals the expected bytes and `Sha256Hex(value)` equals the expected
`sha256:`-prefixed digest. Add explicit cases: floats and non-integer numbers **error**;
object keys sorted by **UTF-16 code units** (include a key pair that differs under UTF-16 vs
byte ordering); strings escape `\" \\ \b \t \n \f \r` and `\u00xx` for other <0x20, non-ASCII
UTF-8 emitted verbatim; arrays preserve order; no whitespace.
**Step 2:** run → FAIL.
**Step 3 (impl):** recursive emitter over a `Value` union (object as ordered key/val, array,
string, integer (int64/uint64), bool, null). Reject float/non-integer at parse and emit.
Sort object keys by re-encoding to `[]uint16` and lexicographic compare. `Sha256Hex(v) =
"sha256:"+hex(sha256(Canonicalize(v)))`. Provide `Sha256Raw(v) [32]byte`.
**Step 4:** run → PASS. **Step 5:** `make secure` then commit.

> The canonical-value projections (what fields, in what nesting) for statement / keyset /
> receipt are defined in the design doc; build them as explicit `Value` constructors in the
> respective files, never from raw `json.Unmarshal` of the wire struct (extra wire fields
> must not change a digest).

### Task 1.3: Wire types + parse + api_version guard

**Files:** Create `pkg/llm/aci/report.go`, `pkg/llm/aci/report_test.go`

**Step 1 (test):** `ParseReport(testdata/report_aci1.json)` succeeds; fields populate
(`APIVersion`, `WorkloadID`, `WorkloadKeysetDigest`, `Attestation{ReportDataHex, Evidence,
Keyset, KeysetEndorsement, SourceProvenance, Freshness}`, `Keyset.{Identity, Epoch,
ReceiptSigningKeys, E2EEPublicKeys, TLSPublicKeys}`). A report with `api_version:"aci/2"`
→ `ErrUnsupportedAPIVersion`. Note serde renames: `report_data`↔`ReportDataHex`,
`value`↔`ValueHex`, `public_key`↔`PublicKeyHex`, `spki_sha256`↔`SPKISHA256Hex`, event
`type`↔`EventType`.
**Step 2:** run → FAIL. **Step 3 (impl):** structs + `ParseReport`. **Step 4:** PASS.
**Step 5:** `make secure`; commit.

---

## Phase 2 — Attestation verification

### Task 2.1: Identity digests

**Files:** Create `pkg/llm/aci/identity.go`, `identity_test.go`

**Step 1 (test):** from the fixture, recomputed `workload_id` (= `Sha256Hex(JCS({algo,
public_key}))`) equals `report.WorkloadID`; recomputed `workload_keyset_digest` (=
`Sha256Hex(JCS(keyset canonical value)))`) equals `report.WorkloadKeysetDigest`. Mutate a
keyset byte → mismatch error.
**Steps 2-5:** FAIL → implement canonical-value builders + compare → PASS → `make secure` → commit.

### Task 2.2: report_data binding

**Files:** `pkg/llm/aci/binding.go`, `binding_test.go`

**Step 1 (test):** `statement = {purpose:"aci.report_data.v1", workload_id,
workload_keyset_digest, nonce}`; `Sha256Raw(JCS(statement))` (hex) == `attestation.
report_data`; using the fixture's known nonce (`0…01`). Wrong nonce → `report_data_mismatch`.
nonce may be the string or JSON null.
**Steps 2-5:** TDD → commit.

### Task 2.3: `tee.VerifyTDXQuote` gains Options (full DCAP)

**Files:** Modify `pkg/llm/tee/intel_quote.go`, `pkg/llm/tee/intel_quote_test.go`

**Step 1 (test):** new `VerifyTDXQuoteOpts(raw []byte, opts Options) ([]byte, error)` with
`Options{GetCollateral, CheckRevocations bool}`; keep `VerifyTDXQuote(raw)` as a wrapper
passing `Options{false,false}` (no behavior change — existing tests stay green). Add a test
that `Options{true,true}` is threaded to `verify.Options` (use a fixture quote; gated/live if
collateral needs network — mark `t.Skip` without network).
**Steps 2-5:** implement (thread Options into `verify.TdxQuote`), run existing tee tests green,
`make secure`, commit.

### Task 2.4: Quote verification + report_data placement

**Files:** `pkg/llm/aci/verify.go`, `verify_test.go`

**Step 1 (test, gated live):** `verifyQuote(report, opts)` calls
`tee.VerifyTDXQuoteOpts(hex(evidence.quote), {true,true})` → 64B; require `[0:32]==binding`
and `[32:64]==0`; `tee_type=="tdx"`. Skip without network. Add an offline test that the
`[0:32]/[32:64]` placement check rejects a tampered report_data.
**Steps 2-5:** TDD → commit.

### Task 2.5: RTMR3 replay + app-id + source provenance

**Files:** `pkg/llm/aci/rtmr.go`, `rtmr_test.go`

**Step 1 (test):** parse `evidence.event_log` (JSON string → `[]{imr,event_type/type,digest,
event,event_payload}`); replay IMR3: `mr=zeros(48); for imr==3: mr=SHA384(mr || pad48(hex
digest))`; equals quote RTMR3 (from a fixture quote body). Extract `app-id` (first `imr==3 &&
event=="app-id"`, stop at `system-ready`). `checkSourceProvenance(report, policy)` accepts
matching `repo_url`+`repo_commit`, rejects otherwise.
**Steps 2-5:** TDD → commit. (RTMR3 vs quote uses the fixture; if quote RTMR3 extraction
needs go-tdx-guest accessors, add a small `tee` helper.)

### Task 2.6: keyset endorsement

**Files:** `pkg/llm/aci/keys.go`, `keys_test.go`

**Step 1 (test):** `verifyKeysetEndorsement(identityKey, endorsement, keysetDigest)`:
`endorsement.algo == identity.algo`; payload = `JCS({purpose:"aci.keyset.endorsement.v1",
workload_keyset_digest})`; verify — `ed25519` raw-64B over payload; `ecdsa-secp256k1`
**64-byte r‖s** over `sha256(payload)`. Use a vector from the fixture (its real endorsement).
Tampered digest → `endorsement_invalid`.
**Steps 2-5:** TDD → commit.

### Task 2.7: KMS custody chain

**Files:** add to `keys.go`, `keys_test.go`

**Step 1 (test):** `evidence.key_custody` with `provider=="dstack-kms"`, a `keys[]` `role==
"identity"` whose `public_key==workload_identity.public_key`, 2-element `signature_chain`:
chain[0] = recoverable secp256k1 (65B, **Keccak256** digest) over UTF-8
`"{purpose}:{compressed_identity_pubkey_hex}"` → recovers app key; chain[1] = recoverable
secp256k1 over `"dstack-kms-issued" ‖ ":" ‖ app_id ‖ app_pubkey_sec1` → recovers KMS root;
require compressed-hex root ∈ `policy.AcceptedKMSRootPubKeys`. Use the fixture's real custody
block; unknown root → `kms_root_untrusted`.
**Steps 2-5:** TDD → commit.

### Task 2.8: Policy + freshness + top-level VerifyReport

**Files:** `pkg/llm/aci/policy.go`, `pkg/llm/aci/verify.go` (extend), tests

**Step 1 (test):** `Policy{AcceptedWorkloadIDs, AcceptedSourceProvenance{Repo,Commit},
AcceptedAppIDs, AcceptedKMSRootPubKeys}` with documented **pinned Phala/RedPill defaults**
(`DefaultPhalaPolicy()`), each value annotated "verify independently". `VerifyReport(reportJSON,
nonce, now, policy) (*VerifiedReport, error)` runs, in order: api_version guard → identity
digests → binding → quote(+placement) → RTMR3/app-id/provenance → endorsement → KMS custody →
freshness (`fetched_at <= now < stale_after`). `VerifiedReport` exposes the validated keyset
(e2ee + receipt-signing keys), workload_id, keyset_digest. Each failing sub-check yields its
typed `Reason`. Add a full offline `VerifyReport` test against the fixture with collateral
disabled via an injected quote-verifier seam, plus a gated live test with full DCAP.
**Steps 2-5:** TDD → `make secure` → commit.

> Inject the quote verifier (`func([]byte, tee.Options) ([]byte,error)`) into `VerifyReport`
> so offline tests use the fixture's real quote without network, and the live path uses
> `tee.VerifyTDXQuoteOpts`.

---

## Phase 3 — ACI E2EE v2 (seal/open)

### Task 3.1: secp256k1 ECDH + HKDF + AES-GCM primitive

**Files:** `pkg/llm/aci/e2ee.go`, `e2ee_test.go`

**Step 1 (test):** `seal(modelPub, plaintext, aad) -> hex(ephem65 ‖ nonce12 ‖ ct+tag)` and
`open(clientPriv, hexBlob, aad) -> plaintext` round-trip; AAD mismatch → error; tampered tag
→ error. Cross-check the exact KDF: ECDH(`k256` shared secret = X coord) → `HKDF-SHA256(salt=
nil, info="aci.e2ee.v2.secp256k1")` → 32B AES key; `AES-256-GCM`, 12B nonce, 16B tag;
ephemeral key is fresh uncompressed secp256k1 (65B `04…`).
**Step 2:** FAIL. **Step 3:** implement with decred secp256k1 (ECDH via shared-secret),
`x/crypto/hkdf`, `crypto/aes`+`cipher`. **Step 4:** PASS. **Step 5:** `make secure`; commit.

> Validate against a **fixed vector** captured from the Rust `tests/aci_service_surface.rs`
> seal fixture if extractable; otherwise rely on the live round-trip (Task 5.x). Record any
> ambiguity in the AAD/info string as a blocker before shipping.

### Task 3.2: Field-level request sealing + headers

**Files:** add to `e2ee.go`, `e2ee_test.go`

**Step 1 (test):** `sealRequest(openaiBody, verified, model) -> (sealedBody, headers)`:
encrypts each `messages[].content` (and `prompt`/`input` when present) with per-field AAD
`v2|req|algo=<algo>|model=<model>|m=<i>|c=<sel>|n=<nonce>|ts=<ts>`; sets headers
`X-E2EE-Version:2`, `X-Client-Pub-Key`(our ephemeral/static client pub), `X-Model-Pub-Key`
(chosen keyset e2ee key), `X-E2EE-Nonce`, `X-E2EE-Timestamp`. Selects the `e2ee_public_keys`
entry by algo `secp256k1-aes-256-gcm-hkdf-sha256`.
**Steps 2-5:** TDD → commit.

### Task 3.3: Response field opening

**Files:** add to `e2ee.go`, `e2ee_test.go`

**Step 1 (test):** `openResponse(sealedRespBody, clientPriv, model, nonce, ts) -> plainBody`
decrypts `content`/`reasoning_content`/`embedding` fields with `resp`-direction AAD; enforce
5-min freshness on the response markers; reject on AAD/nonce mismatch.
**Steps 2-5:** TDD → commit.

---

## Phase 4 — Receipt verification

### Task 4.1: Receipt canonical bytes + signature

**Files:** `pkg/llm/aci/receipt.go`, `receipt_test.go`

**Step 1 (test):** from `testdata/receipt_aci1.json`: `canonicalReceipt(r)` =
`JCS(receipt with signature.value omitted)` (keep `signature.{algo,key_id}`; flatten event
`seq`+`type`+fields). `verifyReceiptSig(key, canonical, sig)`: `ed25519` raw-64B over bytes;
`ecdsa-secp256k1` **65B r‖s‖v** over `sha256(bytes)` (v∈{0,1} or 27–30), recover pubkey and
require equality with the listed key. Key chosen from `verified.Keyset.ReceiptSigningKeys` by
`signature.key_id`. Tamper a field → invalid.
**Steps 2-5:** TDD → commit.

### Task 4.2: Mandatory receipt policy checks

**Files:** add to `receipt.go`, `receipt_test.go`

**Step 1 (test):** `VerifyReceipt(receiptJSON, verified, want ReceiptExpect) error` requires:
identity match (`workload_id`, `workload_keyset_digest` == verified); `api_version=="aci/1"`;
`endpoint`/`method` == expected; events include `request.received`, `response.returned`;
`request.received.body_hash == sha256:hex(reqBody)`; `response.returned.{cleartext_hash|
wire_hash} == sha256:hex(respBody)` (**both mandatory**, not optional); an `upstream.verified`
event with `result=="verified"` for the expected `vendor/model_id`. Any miss →
`receipt_invalid` / `upstream_unverified`.
**Steps 2-5:** TDD → commit.

---

## Phase 5 — Client + wiring

### Task 5.1: Attested-session cache

**Files:** `pkg/llm/aci/session.go`, `session_test.go`

**Step 1 (test):** per-model cache keyed by model name with TTL (~50s, < `stale_after`
margin); `get` returns a cached `*VerifiedReport` until TTL; expiry forces re-attest;
failures are NOT cached.
**Steps 2-5:** TDD → commit.

### Task 5.2: Client (llm.LLM) — Invoke (buffer-until-verified)

**Files:** `pkg/llm/aci/client.go`, `client_test.go`

**Step 1 (test, fake transport):** `New(baseURL, apiKey, policy, opts...)` returns `llm.LLM`.
`Invoke`: attest (cached) → `openaiapi` encode → `sealRequest` → `POST {base}/v1/chat/
completions` (+ headers) → read full body + `x-receipt-id` → `openResponse` →
`VerifyReceipt` (req/resp wire hashes computed over the actual sealed bytes per the receipt
contract) → only then `openaiapi` decode + return. Any verification failure → typed error
and **no** `*llm.Response`. Use an injected `httpDoer` + fake gateway implementing the seal
contract for the unit test.
**Steps 2-5:** TDD → commit.

### Task 5.3: Client — Stream (buffer-until-verified)

**Files:** add to `client.go`, `client_test.go`

**Step 1 (test):** `Stream` must NOT release chunks pre-verification. Implement by buffering
the full SSE response, verifying receipt+hashes+upstream, then returning a
`*llm.StreamReader[content.Chunk]` that replays the already-verified, opened deltas; if
verification fails, `Stream` returns an error and emits nothing. Test: a fake gateway whose
receipt fails ⇒ `Stream` returns error, zero chunks observed.
**Steps 2-5:** TDD → commit.

> Rationale captured in design resolution #4 — `loop/step.go` emits live deltas, so the
> client must finish verification before any delta is observable.

### Task 5.4: Wire into `auto.New`; remove old phala

**Files:** Modify `pkg/llm/auto/auto.go`, `auto_test.go`; Delete `pkg/llm/openaiapi/phala/**`

**Step 1 (test):** `auto.New(spec)` with `spec.Provider==ProviderPhala` returns an
`*aci.Client` (assert via a behavior, e.g. non-nil + Invoke validates). Update the case to
`aci.New(spec.BaseURL, spec.APIKey, aci.DefaultPhalaPolicy())`.
**Step 2:** FAIL/compile error referencing removed phala. **Step 3:** rewrite the case;
`git rm -r pkg/llm/openaiapi/phala`. **Step 4:** `go build ./... && go test ./pkg/llm/...`.
**Step 5:** `make secure`; commit.

> Provider key handling: `aci.New` takes `apiKey` (Bearer). `Policy` is constructed by the
> caller; `auto.New` uses `DefaultPhalaPolicy()`. If callers need a custom policy, add a
> spec/option seam in a follow-up (YAGNI for now).

---

## Phase 6 — End-to-end verification (gated, live)

### Task 6.1: Live integration test (looprig)

**Files:** `pkg/llm/aci/live_integration_test.go`

**Step 1 (test):** gated on `PHALA_API_KEY`; builds `aci.New("https://inference.phala.com",
key, DefaultPhalaPolicy())`, `Invoke` a 1-line prompt to `z-ai/glm-5.2`, assert a non-empty
verified response, full DCAP on. Also a `Stream` variant. Skips cleanly without the key.
**Step 2:** run `PHALA_API_KEY=… go test ./pkg/llm/aci -run Live -v` → exercises the real
seal→attest→chat→open→receipt path. **Step 3:** `make secure`; commit.

### Task 6.2: swe end-to-end

**Files:** (swe) re-verify; no code change expected.

**Step 1:** in swe, `go build ./... && go test ./...`. **Step 2:** run swe with the Phala
config (`/run` or `go run ./cmd/swe`) and confirm GLM-5.2 streams a verified reply (manual).
**Step 3:** if any GLM `reasoning_content` decode gap surfaces, fix in `openaiapi` (shared
codec), not in `aci`. **Step 4:** commit any swe changes separately.

---

## Open blockers to resolve DURING execution (flag, don't guess)

- **E2EE v2 exact KDF/AAD bytes.** Tasks 3.1–3.3 must be validated against a *fixed vector*
  from the Rust `tests/aci_service_surface.rs` fixtures OR a live round-trip. If the live
  round-trip fails and no vector is extractable, STOP and re-extract the exact AAD/info/field
  -selector encoding from `src/aci/e2ee.rs` + `src/aggregator/service/e2ee_crypto.rs` before
  shipping seal/open.
- **Policy trust anchors.** `DefaultPhalaPolicy()` values (workload_id, source repo+commit,
  KMS root pubkeys) are pinned from a known-good attestation; before merge, confirm them
  against an independent Phala/Dstack source, not just the report we fetched.
- **RTMR3 accessor.** If go-tdx-guest doesn't expose RTMR3 directly, add a small `tee` getter
  (Task 2.5) rather than re-parsing the quote in `aci`.

---

## Done criteria

`go build ./...` + `go test ./...` green in looprig and swe; `make secure` clean; live
integration test passes with `PHALA_API_KEY`; swe streams a verified GLM-5.2 reply via Phala;
`pkg/llm/openaiapi/phala` removed; `CLAUDE.md` records the secp256k1 dep.
