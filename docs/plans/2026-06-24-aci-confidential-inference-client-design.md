# Design: `pkg/llm/aci` — Dstack ACI confidential-inference client

Date: 2026-06-24
Status: **Approved spec.** Single deliverable (no phasing): full-DCAP attestation + ACI
**E2EE v2** sealing + mandatory signed-receipt verification.

> Spec provenance: Dstack `private-ai-gateway` pinned at commit
> `1b43f76e43c2459856faebe9cd97d8e01cb0df0c` (== the deployed gateway's
> `source_provenance.repo_commit`). All wire constants below are from `src/aci/*` and
> `src/aggregator/service/*` at that commit. ACI is an upstream developer preview — pin the
> commit, never `@main`.

## Problem

Phala / RedPill migrated their gateway to the Dstack `private-ai-gateway`, which serves a
versioned attestation format `api_version: "aci/1"`. looprig's `pkg/llm/openaiapi/phala`
client targets the *old* RedPill format and fails on `aci/1`
(`attestation_malformed: request_nonce is missing`). The gateway self-declares the new format
in its TEE-bound report (`vendor: private-ai-gateway-dev`, `source_provenance.repo_url:
github.com/Dstack-TEE/private-ai-gateway`). Chutes is unaffected — different gateway, its own
ML-KEM protocol (`pkg/llm/openaiapi/chutes`).

## Goal & scope

A **reusable, provider-agnostic** Go client for any Dstack `private-ai-gateway`, at **maximum
security**. This is an SDK capability, not a single-model fix — SDK users choose the provider.
"Max security per provider": **Chutes** already gives post-quantum (ML-KEM-768) confidentiality;
**Phala** gets its ceiling, **ACI E2EE v2** (classical secp256k1 — the most the gateway offers
on the user→gateway hop; ML-KEM exists only on the gateway→Chutes hop). Quantum-safe user
confidentiality ⇒ use the Chutes provider.

## Architecture

```
pkg/llm/tee        (exists)  TDX quote verification (go-tdx-guest)
pkg/llm/openaiapi  (exists)  OpenAI-compatible request/response codec (GLM reasoning_content)
pkg/llm/aci        (NEW)     Dstack ACI protocol, end-to-end llm.LLM client
   ├─ jcs.go        constrained canonical JSON (for digests + receipt signature only)
   ├─ report.go     aci/1 wire types, parse, api_version guard
   ├─ identity.go   workload_id / workload_keyset_digest recompute
   ├─ binding.go    report_data = sha256(JCS(statement))
   ├─ verify.go     full-DCAP attestation orchestration + VerifyReport
   ├─ rtmr.go       RTMR3 event-log replay, app-id, source-provenance
   ├─ keys.go       keyset endorsement + KMS custody chain + signature primitives
   ├─ policy.go     trust Policy + pinned defaults
   ├─ e2ee.go       E2EE v2 seal/open (secp256k1 ECDH + HKDF + AES-256-GCM)
   ├─ receipt.go    receipt fetch + canonical + signature + mandatory checks
   ├─ session.go    per-model attested-session cache (TTL)
   └─ client.go     llm.LLM (Invoke/Stream), buffer-until-verified
```

`auto.New(spec)` maps `ProviderPhala → aci.New(spec.BaseURL, spec.APIKey, aci.DefaultPhalaPolicy())`.
The obsolete `pkg/llm/openaiapi/phala` is **removed**. A "provider" is just config (base URL,
key, model names) — no provider-specific code paths. swe needs no code change beyond the
`auto.New` mapping.

## Confidentiality facts (verified against the source)

- The gateway **serves plain HTTP**; TLS is terminated by an **external, un-attested**
  component (`src/dstack.rs` `tls_spkis()` returns empty in-TEE; `src/main.rs` plain
  `axum::serve`). **TLS-SPKI pinning is NOT a valid confidentiality anchor here** — the
  published `tls_public_keys` bind a deployment-mounted cert the TEE does not hold. We do
  **not** pin TLS. Operator confidentiality comes from **E2EE v2 only**.
- E2EE v2 ships **no client** (server + Rust lib + in-process tests only). We implement the
  client from the extracted protocol.
- E2EE v2 is **classical** (secp256k1 ECDH + AES-256-GCM). Not post-quantum.

## Trust policy

`aci.New(baseURL, apiKey, Policy)` where:
```
Policy{
  AcceptedWorkloadIDs        []string  // sha256:… identity digests
  AcceptedSourceProvenance   []struct{ Repo, Commit string }
  AcceptedAppIDs             []string  // from RTMR3 event-log
  AcceptedKMSRootPubKeys     []string  // compressed secp256k1 hex
}
```
Trust anchors are **caller-supplied, out-of-band** — never trusted from the report itself
(circular). Ship `DefaultPhalaPolicy()` with **pinned Phala/RedPill values** derived from a
known-good attestation + commit `1b43f76`, each annotated "verify independently before
relying on it." There is no TLS-SPKI pin (not anchorable on this gateway).

## Endpoints (canonical, pinned)

- `GET /v1/aci/attestation?model=&nonce=` — attestation report.
- `POST /v1/chat/completions` — inference (sealed body + E2EE headers).
- `GET /v1/aci/receipts/{id}` — receipt (id from the `x-receipt-id` response header).

`/v1/attestation/report` and `/v1/signature/{id}` are **compat-only**; do not use.

## Two distinct serializations (do not conflate)

This is the single most error-prone point. There are **two** byte-exact serializations:

1. **Constrained JCS** (`jcs.go`) — used for **attestation digests** (`workload_id`,
   `workload_keyset_digest`, the `report_data` statement, the keyset-endorsement payload) and
   for the **receipt signature** canonical bytes. It rejects floats/non-integer numbers,
   sorts object keys by **UTF-16 code units**, escapes per the Dstack profile, no whitespace.
   Validate with **fixed cross-language vectors** captured from the Rust `verify_aci_artifacts`.
   *This is NOT generic RFC 8785 — it is the constrained Dstack subset.*
2. **Compact `serde_json`** — used for the **request/response body hashes** in the receipt
   (`request.received.body_hash`, `response.returned.{cleartext_hash,wire_hash}`). These are
   `sha256(serde_json::to_vec(payload))`: compact (no whitespace), **object order preserved as
   sent** (the gateway uses `preserve_order`). The client must reproduce serde_json's exact
   compact bytes (escaping, number formatting, key order) → requires a **golden byte-for-byte
   vector test**, or receipt verification fails despite correct crypto. *This is NOT JCS.*

All digest *strings* are `"sha256:" + hex(sha256(bytes))`.

## Attestation verification (full DCAP) — `VerifyReport(reportJSON, nonce, now, policy)`

In order, each failure returning a typed `*llm.AttestationError` (see Errors):
1. **api_version guard** — must equal `"aci/1"` (else reason `unsupported_api_version`,
   message names the offending version). This is the version-drift tripwire.
2. **Identity digests** — recompute `workload_id = Sha256Hex(JCS(identity.public_key {algo,
   public_key}))` and `workload_keyset_digest = Sha256Hex(JCS(keyset))`; compare to the report.
3. **report_data binding** — `statement = {purpose:"aci.report_data.v1", workload_id,
   workload_keyset_digest, nonce}` (nonce = the UTF-8 string sent, or JSON null);
   `sha256(JCS(statement))` must equal `attestation.report_data` (32 bytes hex).
4. **TDX quote (DCAP)** — `tee.VerifyTDXQuote(hex(evidence.quote), Options{...})` (see tee
   change). Require the verified 64-byte quote report_data to be `binding(32) ‖ zero(32)` and
   `tee_type == "tdx"`.
5. **RTMR3 + app-id + provenance** — parse `evidence.event_log` (JSON string → events); replay
   IMR3: `mr = zeros(48); for imr==3: mr = SHA384(mr ‖ pad48(hex digest))`; compare to the
   quote's RTMR3. Extract `app-id` (first `imr==3 && event=="app-id"`, stop at `system-ready`);
   require `app-id ∈ policy.AcceptedAppIDs` (when configured). Require
   `source_provenance.{repo_url,repo_commit}` ∈ `policy.AcceptedSourceProvenance`.
6. **keyset endorsement** — `keyset_endorsement.algo == identity.algo`; payload =
   `JCS({purpose:"aci.keyset.endorsement.v1", workload_keyset_digest})`; verify with the
   identity key: `ed25519` raw-64B over payload, or `ecdsa-secp256k1` **64-byte r‖s** over
   `sha256(payload)`.
7. **KMS custody** — `evidence.key_custody` (`provider=="dstack-kms"`), a `keys[]` entry
   `role=="identity"` whose `public_key == identity.public_key`, with a 2-element
   `signature_chain`: chain[0] = recoverable secp256k1 (65B, **Keccak256** digest) over UTF-8
   `"{purpose}:{compressed_identity_pubkey_hex}"` → recovers the app key; chain[1] =
   recoverable secp256k1 over `"dstack-kms-issued" ‖ ":" ‖ app_id ‖ app_pubkey_sec1` →
   recovers the KMS root; require compressed-hex root ∈ `policy.AcceptedKMSRootPubKeys`.
8. **freshness** — `freshness.fetched_at <= now < freshness.stale_after`.
9. **policy** — `workload_id ∈ policy.AcceptedWorkloadIDs` (when configured).

Returns a `*VerifiedReport` exposing the validated keyset (`e2ee_public_keys`,
`receipt_signing_keys`), `workload_id`, `workload_keyset_digest`. GPU/NVIDIA evidence is
legacy-only and **out of scope**.

> Inject the quote verifier as a seam (`func([]byte, tee.Options) ([]byte, error)`) so offline
> tests use the fixture's real quote without network and the live path uses
> `tee.VerifyTDXQuote`.

## `pkg/llm/tee` change — bounded, injectable DCAP

`tee.VerifyTDXQuote` today runs `verify.Options{GetCollateral:false, CheckRevocations:false}`
(`intel_quote.go`). Full DCAP needs collateral + TCB/QE/CRL revocation, but go-tdx-guest's
default collateral `Getter` uses an **unbounded `http.Get`**, which violates the repo timeout
rule. Add an options type that threads **all** of: `GetCollateral`, `CheckRevocations`, a
**bounded `Getter`** (an `http.Client` with a timeout, or a caller-injected getter), and a
`Now` clock. Keep `VerifyTDXQuote(raw)` as a back-compat wrapper (collateral off) so existing
callers/tests are unaffected; `aci` calls the options form with collateral+revocation on and a
bounded getter.

## E2EE v2 — seal/open (exact)

- **Key:** the `e2ee_public_keys` keyset entry with algo `secp256k1-aes-256-gcm-hkdf-sha256`.
- **Per field:** fresh ephemeral secp256k1 keypair → ECDH(model pubkey) → **HKDF-SHA256**
  (`salt=nil`, `info="aci.e2ee.v2.secp256k1"`) → 32-byte key → **AES-256-GCM** (12B nonce, 16B
  tag). Ciphertext = `hex(ephemeral_uncompressed_pubkey(65) ‖ gcm_nonce(12) ‖ ct+tag)`.
- **Request field rules (chat):** `messages[].content` —
  - if a **string**: encrypt the whole string; AAD content-selector `c=-`.
  - if an **array**: encrypt only `{"type":"text","text":…}` parts (the `text` value), AAD
    `c=<content index>`; **leave non-text parts (e.g. `image_url`) untouched**.
  For completions/embeddings: `prompt.{index}` / `input.{index}` array elements.
- **Request AAD:** `v2|req|algo={algo}|model={model}|m={message_index}|c={content_index}|n={nonce}|ts={timestamp}`.
- **Headers:** `X-E2EE-Version: 2`, `X-Client-Pub-Key`, `X-Model-Pub-Key`, `X-E2EE-Nonce`,
  `X-E2EE-Timestamp`.
- **Response open:** the gateway re-encrypts response fields to the client pubkey; decrypt
  `content`/`reasoning_content`/`embedding` with the client secret. **Response AAD** is one of
  `v2|resp|algo=…|model=…|id={response_id}|data={data_index}|field={field_name}|n=…|ts=…` or
  `v2|resp|…|id={response_id}|choice={choice_index}|field={field_name}|n=…|ts=…`. 5-minute
  replay window on the markers.
- **Post-quantum?** No (secp256k1 ECDH). PQ is Chutes-only.

> The exact KDF/AAD/field-selector bytes must be validated against a fixed vector from the Rust
> `tests/aci_service_surface.rs` fixtures or a live round-trip before shipping; if neither is
> reproducible, STOP and re-extract from `src/aci/e2ee.rs` + `src/aggregator/service/
> e2ee_crypto.rs`.

## Receipt verification (mandatory) — `VerifyReceipt(receiptJSON, verified, expect)`

- **Fetch** `GET /v1/aci/receipts/{id}` (id from `x-receipt-id`). Accept `{...}` or
  `{ "receipt": {...} }`.
- **Identity match:** `receipt.workload_id == verified.workload_id` and
  `receipt.workload_keyset_digest == verified.workload_keyset_digest`; `api_version == "aci/1"`;
  `endpoint`/`method` == expected.
- **Body hashes (compact serde_json, NOT JCS):**
  - `request.received.body_hash == sha256:hex(decrypted request body)` — for E2EE the gateway
    substitutes the **decrypted** body as the received body (`handlers.rs` →
    `prepared.decrypted_body`; hashed in `forward.rs add_request_received`). The client
    reproduces this by compact-`serde_json`-serializing **its own plaintext request body**
    (same field order it sent). MANDATORY.
  - `response.returned` carries **two** fields: `cleartext_hash` (sha256 of the **decrypted**
    response) and `wire_hash` (sha256 of the **sealed wire** response) —
    `forward.rs add_response_returned(cleartext, wire)`. The client verifies `cleartext_hash`
    against its opened response (and may also check `wire_hash` against the raw sealed bytes).
    MANDATORY (at least `cleartext_hash`).
- **Upstream:** an `upstream.verified` event with `result == "verified"` for the expected
  `vendor`/`model_id`. MANDATORY (reason `upstream_unverified` otherwise).
- **Signature (JCS):** key = `verified.Keyset.ReceiptSigningKeys` entry with
  `key_id == signature.key_id`; canonical bytes = `JCS(receipt with signature.value omitted)`
  (keep `signature.{algo,key_id}`; flatten event `seq`+`type`+fields). Verify `ed25519`
  raw-64B over the canonical bytes, or `ecdsa-secp256k1` **65-byte r‖s‖v** over
  `sha256(canonical bytes)` (v ∈ {0,1} or 27–30), recover the pubkey and require equality
  with the listed key.

## Streaming — buffer until verified

`loop/step.go` emits live `TokenDelta`s as chunks arrive and only `Close()`s on `defer`, so a
"verify at Close" model would release **unverified** output. The aci client therefore
**buffers the full sealed response**, opens it, verifies receipt + body hashes + `upstream.
verified`, and only then returns content (Invoke) or a `StreamReader` replaying the
already-verified deltas (Stream). On any failure it returns an error and emits nothing.
**Tradeoff (accepted):** no live token streaming and the full response is held in memory
per turn — acceptable for fail-closed confidentiality + integrity.

## Errors

Reuse the existing provider-neutral **`llm.AttestationError{Reason string, Err error}`**
(fail-closed; `Unwrap` chains). `aci` defines its reason **string constants**
(`unsupported_api_version`, `report_data_mismatch`, `binding_mismatch`, `quote_invalid`,
`tcb_revoked`, `keyset_digest_mismatch`, `endorsement_invalid`, `kms_root_untrusted`,
`policy_rejected`, `stale_report`, `receipt_invalid`, `upstream_unverified`, `e2ee_failed`) and
returns `&llm.AttestationError{Reason: …, Err: …}`. **No new error type.** Never leak the
API key or plaintext in error text.

## Dependencies (approved in CLAUDE.md)

`github.com/decred/dcrd/dcrec/secp256k1/v4` (+`/ecdsa`) — secp256k1 verify(64B) + recover(65B)
+ ECDH; `x/crypto/sha3` (Keccak-256); `x/crypto/hkdf`; `crypto/ed25519`, `crypto/aes`+`cipher`
(stdlib); DCAP via `pkg/llm/tee` (go-tdx-guest). Pin a specific secp256k1 version (not
`@latest`) at `go get` time.

## swe impact

None beyond the `auto.New` mapping. SDK users choose the provider. Re-verify end-to-end (any
model) after it lands. Any GLM `reasoning_content` decode gap is fixed in the shared
`openaiapi` codec, never in `aci`.
