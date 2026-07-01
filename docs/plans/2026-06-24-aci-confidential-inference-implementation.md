# ACI Confidential-Inference Client (`pkg/llm/aci`) — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan
> task-by-task.

**Goal:** Add a reusable, provider-agnostic Go client for the Dstack `private-ai-gateway`
(`aci/1`): full-DCAP attestation + ACI **E2EE v2** request/response sealing + mandatory
signed-receipt verification, wired in as the looprig `ProviderPhala` backend.

**Architecture:** New self-contained `pkg/llm/aci` implementing `llm.LLM` end-to-end; builds on
`pkg/llm/tee` (TDX quote) and `pkg/llm/openaiapi` (OpenAI codec). `auto.New(ProviderPhala)`
routes to it; old `pkg/llm/openaiapi/phala` removed. **The spec is the contract — read it:**
`docs/plans/2026-06-24-aci-confidential-inference-client-design.md`. This plan implements that
spec; where they disagree, the spec wins.

**Tech Stack:** Go 1.26; `crypto/{sha256,sha512,ed25519,aes,cipher}`,
`golang.org/x/crypto/{sha3,hkdf}`, `github.com/decred/dcrd/dcrec/secp256k1/v4` (+`/ecdsa`,
+`/ecdh`); DCAP via `pkg/llm/tee` (`github.com/google/go-tdx-guest`).

**Spec pin:** Dstack `private-ai-gateway` @ `1b43f76e43c2459856faebe9cd97d8e01cb0df0c`. Local
reference clone for cross-checks: `/private/tmp/private-ai-gateway-review/`.

---

## Pre-flight (do once, before Phase 0)

- **Rebase first.** This branch is ~33 commits behind `main`. Before executing, rebase onto
  current `main` (or recreate the branch off `main`) so the implementation targets current
  code: `git rebase main` in the worktree; resolve any `CLAUDE.md` conflict by keeping main's
  file and re-adding the secp256k1 entry (Task 0.1). Re-verify the referenced files still exist:
  `pkg/llm/tee/intel_quote.go`, `pkg/llm/auto/auto.go`, `pkg/llm/errors.go`,
  `pkg/llm/openaiapi/{encode.go,decode.go}`, `pkg/llm/openaiapi/phala/**`, `pkg/loop/step.go`.

## Conventions for EVERY task

- Run all `go`/`make` from the looprig worktree root.
- **TDD loop:** write failing test → `go test -race ./pkg/llm/aci/... -run <Name> -v` (see it
  fail) → minimal impl → re-run (pass) → `make secure` → commit.
- **Build check:** `CGO_ENABLED=0 go build -trimpath ./...` (repo rule, CLAUDE.md §Build).
- **Live tests** are tagged `//go:build integration` and run with
  `go test -tags integration -race ./pkg/llm/aci/... -run Live -v`; they `t.Skip` without
  `PHALA_API_KEY` even under the tag.
- **Reference files by symbol** (function/type), not line number (main has moved).
- Reasons are `llm.AttestationError` string constants (spec §Errors) — never a new error type.

---

## Phase 0 — dependency & fixtures & golden vectors

### Task 0.1: Add secp256k1 (pinned) + record approval

**Files:** Modify `go.mod`, `go.sum`, `CLAUDE.md`

**Step 1:** Resolve a concrete version (not `@latest`):
`GOWORK=off go get github.com/decred/dcrd/dcrec/secp256k1/v4@v4.4.0` (verify the exact tag with
`go list -m -versions github.com/decred/dcrd/dcrec/secp256k1/v4`; pin the chosen one).
**Step 2:** `go mod verify && make vuln` → no new vulns.
**Step 3:** Add the `CLAUDE.md` approved-packages entry for `decred/secp256k1/v4` (+ note
`x/crypto/sha3` Keccak + `x/crypto/hkdf` usage). If the rebase already carries it, skip.
**Step 4:** `CGO_ENABLED=0 go build -trimpath ./...`.
**Step 5:** Commit: `build(aci): pin decred secp256k1/v4 (approved in CLAUDE.md)`.

### Task 0.2: Capture real fixtures + golden vectors

**Files:** Create `pkg/llm/aci/testdata/{report_aci1.json, receipt_aci1.json, jcs_vectors.json,
body_hash_vectors.json, e2ee_vectors.json}`

**Step 1 — report** (known nonce so binding tests are deterministic):
```bash
KEY=$(grep -E '^PHALA_API_KEY=' /Users/ipotter/code/swe/.env | cut -d= -f2-)
N=0000000000000000000000000000000000000000000000000000000000000001
curl -s -H "Authorization: Bearer $KEY" \
  "https://inference.phala.com/v1/aci/attestation?model=z-ai/glm-5.2&nonce=$N" \
  > pkg/llm/aci/testdata/report_aci1.json
```
**Step 2 — receipt:** do one real E2EE round-trip (after Phase 3 exists, or with a throwaway
script), grab `x-receipt-id`, `GET /v1/aci/receipts/{id}` → `receipt_aci1.json`. Until then,
stub this file from a captured live receipt.
**Step 3 — JCS vectors** (`jcs_vectors.json`): generate from the Rust reference so they are
authoritative. In `/private/tmp/private-ai-gateway-review`, drive
`src/aci/canonical.rs::canonicalize` over fixed inputs (a tiny Rust test or the
`verify_aci_artifacts` path) and record `{value, canonical_bytes, sha256}`. Include: empty
object/array, integer, nested object with keys that differ under UTF-16 vs byte order, unicode
escapes, control chars.
**Step 4 — body-hash vectors** (`body_hash_vectors.json`): the compact `serde_json::to_vec`
(preserve-order) bytes for representative request bodies (string content, array content with a
text + an image part, with `temperature`/`stream` fields), each with its `sha256:` digest,
captured from Rust `serde_json::to_vec` with `preserve_order`. **Critical** — Go's
`encoding/json` sorts map keys; we must match Rust's insertion order (Task 1.3).
**Step 5 — e2ee vectors** (`e2ee_vectors.json`): if extractable from
`tests/aci_service_surface.rs` seal fixtures, record `{model_priv, client_eph, plaintext, aad,
ciphertext_hex}`; otherwise leave a `README` noting E2EE is validated by live round-trip
(Task 6.1) and mark Task 3.1's vector step accordingly.
**Step 6:** Commit: `test(aci): real report/receipt fixtures + JCS/body-hash/e2ee golden vectors`.

> Fixtures are time-bound (`freshness`); freshness-touching tests inject a fixed `now` in
> `[fetched_at, stale_after)`.

---

## Phase 1 — foundation

### Task 1.1: Errors + version constant

**Files:** Create `pkg/llm/aci/errors.go`, `errors_test.go`

**Step 1 (test):** `SupportedAPIVersion == "aci/1"`; reason consts equal their spec strings;
`attestErr(reason, err)` returns `*llm.AttestationError` with that `Reason` and wrapped `Err`;
`errUnsupportedAPIVersion("aci/2")` → `*llm.AttestationError{Reason:"unsupported_api_version"}`
whose message contains `"aci/2"` and `"aci/1"` but no secrets.
**Steps 2-5:** fail → implement (`const SupportedAPIVersion`; `Reason*` consts; helpers wrapping
`&llm.AttestationError{}`) → pass → `make secure` → commit.

### Task 1.2: Constrained JCS canonicalizer (+ fuzz)

**Files:** Create `pkg/llm/aci/jcs.go`, `jcs_test.go`, `jcs_fuzz_test.go`

**Step 1 (test):** table-driven from `testdata/jcs_vectors.json`: `Canonicalize(v)` == expected
bytes; `Sha256Hex(v)` == expected `sha256:…`. Explicit cases: float / non-integer number →
error; object keys sorted by **UTF-16 code units** (vector that differs from byte order);
string escaping per Dstack profile; arrays keep order; no whitespace.
**Step 2:** `go test -race ./pkg/llm/aci -run JCS -v` → FAIL.
**Step 3 (impl):** ordered `Value` union (object = ordered pairs, array, string, int64/uint64,
bool, null); reject floats at construction + emit; UTF-16 key sort (`utf16.Encode` compare);
`Sha256Hex`/`Sha256Raw`.
**Step 4:** PASS. **Step 5 (fuzz):** `FuzzCanonicalize` — feed arbitrary bytes through a JSON→
Value parse + `Canonicalize`; assert no panic and idempotence (`Canonicalize(parse(out))==out`).
Run `go test -fuzz=FuzzCanonicalize ./pkg/llm/aci -fuzztime=30s`. **Step 6:** `make secure`; commit.

### Task 1.3: Compact `serde_json` body serializer (+ golden, + fuzz)

**Files:** Create `pkg/llm/aci/body.go`, `body_test.go`, `body_fuzz_test.go`

This serializes the **request body** for the receipt hash — it must match Rust
`serde_json::to_vec` with `preserve_order`. **Go's `encoding/json` sorts map keys, so we cannot
use `map[string]any`.** Build/keep an order-preserving JSON value.

**Step 1 (test):** from `testdata/body_hash_vectors.json`: `CompactJSON(orderedValue)` == the
exact Rust bytes; `Sha256Hex(CompactJSON(v))` == the vector digest. Cover number formatting,
string escaping, nested arrays/objects, and a body whose key order ≠ alphabetical (proves order
preservation).
**Step 2-4:** FAIL → implement an order-preserving compact encoder (reuse the JCS `Value` type
but **without** key sorting; same string/number rules as serde_json — verify escaping matches)
→ PASS.
**Step 5 (fuzz):** `FuzzCompactJSON` round-trip no-panic. **Step 6:** `make secure`; commit.

> If serde_json escaping/number-formatting differs from the JCS emitter in any case, keep the
> two encoders separate and document the difference inline. Golden vectors are the arbiter.

### Task 1.4: Wire types + parse + api_version guard (+ fuzz)

**Files:** Create `pkg/llm/aci/report.go`, `report_test.go`, `report_fuzz_test.go`

**Step 1 (test):** `ParseReport(testdata/report_aci1.json)` populates `Report{APIVersion,
WorkloadID, WorkloadKeysetDigest, Attestation{ReportDataHex, Evidence, Keyset,
KeysetEndorsement, SourceProvenance, Freshness}}` and `Keyset{Identity, Epoch,
ReceiptSigningKeys, E2EEPublicKeys, TLSPublicKeys}`. `api_version:"aci/2"` →
`unsupported_api_version`. Serde renames: `report_data`→`ReportDataHex`, `value`→`ValueHex`,
`public_key`→`PublicKeyHex`, `spki_sha256`→`SPKISHA256Hex`, event `type`→`EventType`.
**Step 2-4:** FAIL → structs + `ParseReport` → PASS.
**Step 5 (fuzz):** `FuzzParseReport` no-panic on arbitrary bytes. **Step 6:** `make secure`; commit.

---

## Phase 2 — attestation (full DCAP)

### Task 2.1: Identity digests

**Files:** `pkg/llm/aci/identity.go`, `identity_test.go`
**Test:** recomputed `workload_id`/`workload_keyset_digest` from the fixture == report values;
mutated keyset → mismatch. (Builds the JCS canonical-value projections for identity + keyset.)
TDD → commit.

### Task 2.2: report_data binding

**Files:** `pkg/llm/aci/binding.go`, `binding_test.go`
**Test:** `Sha256Raw(JCS(statement{purpose:"aci.report_data.v1", workload_id, keyset_digest,
nonce}))` hex == `attestation.report_data` for nonce `0…01`; wrong nonce → `report_data_mismatch`;
nil nonce → JSON null branch. TDD → commit.

### Task 2.3: `tee.VerifyTDXQuote` Options (bounded, injectable)

**Files:** Modify `pkg/llm/tee/intel_quote.go`, `intel_quote_test.go`
**Test:** new `VerifyTDXQuoteWithOptions(raw, Options{GetCollateral, CheckRevocations bool,
Getter getter, Now func() time.Time})`; `VerifyTDXQuote(raw)` stays a wrapper (collateral off) —
existing tee tests unchanged. `Getter` defaults to a **timeout-bounded** `http.Client`
(context-aware), never the package default unbounded `http.Get`. Thread all four into
`verify.Options`. Add a test asserting the bounded getter is used (inject a fake getter, assert
called) and a back-compat test (wrapper behavior unchanged).
TDD → `make secure` → commit.

### Task 2.4: Quote verification + report_data placement

**Files:** `pkg/llm/aci/verify.go`, `verify_test.go`
**Test (offline, injected verifier seam):** `verifyQuote` calls the injected
`func([]byte, tee.Options)([]byte,error)`; requires verified 64B report_data ==
`binding(32) ‖ zeros(32)` and `tee_type=="tdx"`; tampered placement → `quote_invalid`. Live path
uses `tee.VerifyTDXQuoteWithOptions(... GetCollateral:true, CheckRevocations:true, bounded
getter)`. TDD → commit.

### Task 2.5: RTMR3 replay + app-id + provenance (+ fuzz)

**Files:** `pkg/llm/aci/rtmr.go`, `rtmr_test.go`, `rtmr_fuzz_test.go`
**Test:** parse `evidence.event_log` (JSON string → events); IMR3 replay
`mr=zeros(48); imr==3: mr=SHA384(mr‖pad48(hexdigest))` == quote RTMR3 (fixture); extract app-id
(first `imr==3 && event=="app-id"`, stop at `system-ready`); `app-id ∈ policy.AcceptedAppIDs`;
`source_provenance{repo_url,repo_commit} ∈ policy`. `FuzzEventLog` no-panic. If go-tdx-guest
lacks an RTMR3 accessor, add a tiny `tee` getter (separate commit). TDD → commit.

### Task 2.6: keyset endorsement

**Files:** `pkg/llm/aci/keys.go`, `keys_test.go`
**Test:** `endorsement.algo == identity.algo`; payload `JCS({purpose:"aci.keyset.endorsement.v1",
workload_keyset_digest})`; verify ed25519 raw-64B over payload, or secp256k1 **64B r‖s** over
`sha256(payload)`; tampered digest → `endorsement_invalid`. Use the fixture's real endorsement.
TDD → commit.

### Task 2.7: KMS custody chain

**Files:** add to `keys.go`, `keys_test.go`
**Test:** `key_custody.provider=="dstack-kms"`; `keys[]` `role=="identity"` `public_key==identity`;
chain[0] recoverable secp256k1 (65B, **Keccak256** digest) over `"{purpose}:{compressed_identity
_pubkey_hex}"` → app key; chain[1] over `"dstack-kms-issued"‖":"‖app_id‖app_pubkey_sec1` → KMS
root; root ∈ `policy.AcceptedKMSRootPubKeys` else `kms_root_untrusted`. Fixture's real custody.
TDD → commit.

### Task 2.8: Policy + freshness + `VerifyReport`

**Files:** `pkg/llm/aci/policy.go`, extend `verify.go`, tests
**Test:** `Policy{AcceptedWorkloadIDs, AcceptedSourceProvenance, AcceptedAppIDs,
AcceptedKMSRootPubKeys}`; `DefaultPhalaPolicy()` with pinned values (annotated "verify
independently"). `VerifyReport(reportJSON, nonce, now, policy) (*VerifiedReport, error)` runs the
ordered chain (spec §Attestation 1–9) via the injected quote-verifier seam; each sub-failure →
its `Reason`. Offline full-chain test against the fixture (collateral disabled via seam) +
freshness boundary (`fetched_at<=now<stale_after`). `VerifiedReport` exposes validated keyset +
ids. TDD → `make secure` → commit.

---

## Phase 3 — ACI E2EE v2

### Task 3.1: secp256k1 ECDH + HKDF + AES-256-GCM primitive

**Files:** `pkg/llm/aci/e2ee.go`, `e2ee_test.go`
**Test:** `seal(modelPub, plaintext, aad) → hex(ephem65 ‖ nonce12 ‖ ct+tag)` and
`open(clientPriv, blob, aad)` round-trip; AAD/tag tamper → error. KDF: ECDH (decred
`secp256k1`/`ecdh` shared secret) → `HKDF-SHA256(salt=nil, info="aci.e2ee.v2.secp256k1")` → 32B
→ AES-256-GCM (12B nonce). If `e2ee_vectors.json` exists, assert byte-equality against it.
TDD → commit.

### Task 3.2: Request field sealing (text-only) + ordered body + headers

**Files:** add to `e2ee.go`, `e2ee_test.go`
**Test:** `sealRequest(orderedBody, verified, model) → (sealedBody, headers)` encrypts only:
`messages[].content` string (AAD `c=-`) OR array `{"type":"text"}` parts (AAD `c=<idx>`, images
untouched); `prompt.{i}`/`input.{i}`. AAD `v2|req|algo=…|model=…|m=<i>|c=<sel>|n=<nonce>|ts=<ts>`.
Headers `X-E2EE-Version:2, X-Client-Pub-Key, X-Model-Pub-Key, X-E2EE-Nonce, X-E2EE-Timestamp`.
Picks the `e2ee_public_keys` entry by algo `secp256k1-aes-256-gcm-hkdf-sha256`. The sealed body
must preserve field order (Task 1.3 encoder). TDD → commit.

### Task 3.3: Response open (+ fuzz)

**Files:** add to `e2ee.go`, `e2ee_test.go`, `e2ee_fuzz_test.go`
**Test:** `openResponse(sealedResp, clientPriv, model, nonce, ts) → plainBody` decrypts
`content`/`reasoning_content`/`embedding` with `resp` AAD (both `data=`/`field=` and
`choice=`/`field=` forms); 5-min replay window; AAD/nonce mismatch → `e2ee_failed`.
`FuzzOpenResponse` (arbitrary ciphertext blob) no-panic. TDD → commit.

---

## Phase 4 — receipt

### Task 4.1: Receipt canonical (JCS) + signature (+ fuzz)

**Files:** `pkg/llm/aci/receipt.go`, `receipt_test.go`, `receipt_fuzz_test.go`
**Test:** `canonicalReceipt(r)` = `JCS(receipt minus signature.value)` (keep
`signature.{algo,key_id}`; flatten event `seq`+`type`+fields); `verifyReceiptSig(key, canonical,
sig)` — ed25519 raw-64B over bytes; secp256k1 **65B r‖s‖v** over `sha256(bytes)` (v∈{0,1}/27–30),
recover-and-equal. Key by `signature.key_id` in `verified.ReceiptSigningKeys`. Fixture-driven;
tamper → invalid. `FuzzParseReceipt` no-panic. TDD → commit.

### Task 4.2: Mandatory receipt checks

**Files:** add to `receipt.go`, `receipt_test.go`
**Test:** `VerifyReceipt(receiptJSON, verified, ReceiptExpect{Endpoint, Method, Vendor, ModelID,
ReqBody, RespBodyCleartext, RespWireBytes}) error` requires ALL:
- identity match (`workload_id`, `workload_keyset_digest`); `api_version=="aci/1"`;
  `endpoint`/`method` == expected;
- `request.received.body_hash == Sha256Hex(CompactJSON(ReqBody))` (Task 1.3 — decrypted body,
  compact serde_json);
- `response.returned.cleartext_hash == sha256:hex(RespBodyCleartext)` AND (if provided)
  `wire_hash == sha256:hex(RespWireBytes)`;
- an `upstream.verified` event with `result=="verified"` for `Vendor`/`ModelID` (else
  `upstream_unverified`).
Any miss → `receipt_invalid`/`upstream_unverified`. TDD → commit.

---

## Phase 5 — client + wiring

### Task 5.1: Attested-session cache

**Files:** `pkg/llm/aci/session.go`, `session_test.go`
**Test:** per-model cache, TTL (~50s, margin below `stale_after`); hit until TTL; expiry
re-attests; failures NOT cached; concurrent-safe (run under `-race`). TDD → commit.

### Task 5.2: Client — `Invoke` (buffer-until-verified)

**Files:** `pkg/llm/aci/client.go`, `client_test.go`
**Test (fake httpDoer + fake gateway honoring the seal/receipt contract):**
`New(baseURL, apiKey, policy, opts...) llm.LLM`; `Invoke` = attest(cached) → `openaiapi` encode →
ordered body → `sealRequest` → `POST /v1/chat/completions` (+headers) → read full body +
`x-receipt-id` → `openResponse` → `VerifyReceipt` (req body = the decrypted ordered body; resp
cleartext = opened body; wire = raw sealed bytes) → only then `openaiapi` decode + return. ANY
failure → typed error, **no** `*llm.Response`. Inject `httpDoer` + `now`. TDD → commit.

### Task 5.3: Client — `Stream` (buffer-until-verified)

**Files:** add to `client.go`, `client_test.go`
**Test:** `Stream` buffers the full SSE response, verifies (receipt+hashes+upstream), then returns
a `*llm.StreamReader[content.Chunk]` replaying the verified opened deltas; verification failure →
error + **zero** chunks observed. (Rationale: spec §Streaming — `loop/step.go` emits live deltas.)
TDD → commit.

### Task 5.4: Wire `auto.New`; remove old phala

**Files:** Modify `pkg/llm/auto/auto.go`, `auto_test.go`; `git rm -r pkg/llm/openaiapi/phala`
**Test:** `auto.New(spec)` with `spec.Provider==ProviderPhala` → `*aci.Client`
(`aci.New(spec.BaseURL, spec.APIKey, aci.DefaultPhalaPolicy())`). Remove the phala case + package.
`CGO_ENABLED=0 go build -trimpath ./... && go test -race ./pkg/llm/...`. `make secure`; commit.

> Custom-policy seam (per-spec policy override) is YAGNI now; `DefaultPhalaPolicy()` only.

---

## Phase 6 — end-to-end verification (live, tagged)

### Task 6.1: Live integration test

**Files:** `pkg/llm/aci/live_integration_test.go` (`//go:build integration`)
**Test:** gated on `PHALA_API_KEY`; `aci.New("https://inference.phala.com", key,
DefaultPhalaPolicy())`; `Invoke` + `Stream` a 1-line prompt to `z-ai/glm-5.2`; assert non-empty
verified output; **full DCAP on** (collateral+revocation, bounded getter). Run:
`PHALA_API_KEY=… go test -tags integration -race ./pkg/llm/aci -run Live -v`. This is the
authoritative validation of the E2EE seal/open bytes + receipt model. If it fails on seal/open,
STOP and re-extract from `src/aci/e2ee.rs` + `src/aggregator/service/e2ee_crypto.rs` (do not
guess). Use this run to capture `receipt_aci1.json` (Task 0.2 step 2) + `e2ee_vectors.json`.
`make secure`; commit.

### Task 6.2: swe end-to-end

**Step 1:** in swe: `CGO_ENABLED=0 go build -trimpath ./... && go test -race ./...`.
**Step 2:** run swe with Phala config (`ProviderPhala`/`inference.phala.com`/`z-ai/glm-5.2`/
`PHALA_API_KEY`) and confirm a verified streamed reply (manual).
**Step 3:** any GLM `reasoning_content` decode gap → fix in shared `openaiapi`, never `aci`.
**Step 4:** commit any swe changes separately.

---

## Open blockers — flag, don't guess

1. **E2EE v2 exact bytes** (KDF info, per-field AAD, content-array selectors, ordered-body
   serialization). Validate via `e2ee_vectors.json` (Rust) or the Task 6.1 live round-trip. If
   neither reproduces, STOP and re-extract from the Rust source.
2. **Compact serde_json parity** (Task 1.3) — escaping/number formatting must match Rust byte
   -for-byte or receipt request-hash verification fails. Golden vectors are the arbiter.
3. **Policy anchors** — confirm `DefaultPhalaPolicy()` (workload_id, source repo+commit, KMS root
   pubkeys) against an independent Phala/Dstack source before merge, not just our fetched report.
4. **RTMR3 accessor** — may require a small `tee` helper (Task 2.5).

## Done criteria

`CGO_ENABLED=0 go build -trimpath ./...` + `go test -race ./...` green (looprig + swe);
`make secure` clean; fuzz targets present (JCS, body, report, receipt, event-log, e2ee);
`go test -tags integration -race ./pkg/llm/aci -run Live` passes with `PHALA_API_KEY`; swe streams
a verified GLM-5.2 reply via Phala; `pkg/llm/openaiapi/phala` removed; `CLAUDE.md` records the
secp256k1 dep.

---

## Execution handoff

Plan saved. Two options:
1. **Subagent-driven (this session)** — fresh subagent per task + code review between tasks
   (recommended: crypto/security tasks want review at each step, and blocker #1/#2 need early
   detection).
2. **Parallel session** — open a new session in this worktree running superpowers:executing-plans.
