# Design: `pkg/llm/aci` — Dstack ACI confidential-inference client

Date: 2026-06-24
Status: Approved (architecture); Phase 2 (E2EE v2) pending protocol extraction.

## Problem

Phala / RedPill migrated their inference gateway to the **Dstack `private-ai-gateway`**,
which serves a new **versioned attestation format, `api_version: "aci/1"`**. looprig's
`pkg/llm/openaiapi/phala` client was built against the *old* RedPill format (top-level
`request_nonce`, `signing_address ‖ nonce` report-data binding) and breaks on `aci/1`
with `attestation_malformed: request_nonce is missing`. The gateway self-declares the new
format in its own (TEE-bound) report: `vendor: private-ai-gateway-dev`,
`source_provenance.repo_url: github.com/Dstack-TEE/private-ai-gateway`. This is provider
API drift, not a looprig bug. Chutes is unaffected — it runs a different gateway with its
own (ML-KEM-sealed) protocol, so `pkg/llm/openaiapi/chutes` still matches it.

## Goal

A **reusable, provider-agnostic** Go client for any Dstack `private-ai-gateway`, with
**maximum confidentiality** (target: E2EE v2). Make swe's GLM-5.2-on-Phala path work
end-to-end with proper attestation, and make the next Dstack-gateway provider ~config-only.

## Key decision: no provider specifics → end-to-end package

The `aci/1` contract (report shape, binding, endpoints, OpenAI-compatible body, receipts)
is defined by the **gateway software**, so it is identical across every deployment
(Phala, RedPill, future). The only differences are **base URL, API key, model names** —
constructor parameters, not code paths. Therefore `aci` is an **end-to-end `llm.LLM`
implementation**; a "provider" collapses to `aci.New(baseURL, apiKey)`.

## Architecture

```
pkg/llm/tee        (exists)  TDX quote verification (go-tdx-guest), GPU evidence
pkg/llm/openaiapi  (exists)  OpenAI-compatible request/response codec (GLM reasoning_content)
pkg/llm/aci        (NEW)     Dstack ACI protocol, end-to-end llm.LLM client
   ├─ jcs.go        RFC 8785 canonical JSON (interop-critical)
   ├─ report.go     aci/1 wire types, parse, api_version guard
   ├─ verify.go     attestation verification (binding, quote, keyset endorsement, freshness)
   ├─ receipt.go    receipt fetch + canonical-bytes + signature verification
   ├─ seal.go       Phase 2: E2EE v2 seal/open
   ├─ client.go     llm.LLM (Invoke/Stream) + per-model attested-session cache
   └─ errors.go     typed errors incl. ErrUnsupportedAPIVersion
```

`auto.New` maps `ProviderPhala → aci.New(spec.BaseURL, spec.APIKey)`. The obsolete
`pkg/llm/openaiapi/phala` package is **removed**. swe needs no code change — its config
(`ProviderPhala` / `inference.phala.com` / `z-ai/glm-5.2` / `PHALA_API_KEY`) already
routes here; we re-verify end-to-end after.

## Confidentiality / channel model

Target **E2EE v2** (user decision: "the max we can go"). Staged:

- **Phase 1 — verified attestation + signed receipt** (the secure working floor):
  prove the enclave is genuine and bound to our fresh nonce, and that the response is
  signed by a key the attested keyset endorses. Channel is TLS to the gateway.
- **Phase 2 — E2EE v2 sealed channel**: app-layer seal request/response to the keyset's
  `e2ee_public_keys`, so confidentiality holds even if TLS terminates upstream.

**Never fall back to plaintext silently.** If a required step can't be established, error.

## Verification algorithm (Phase 1) — per Dstack Rust reference

Source of truth: `Dstack-TEE/private-ai-gateway@main` (`src/aci/{canonical,identity,keys,
receipt,verifier/*}.rs`, `examples/verify_aci_artifacts.rs`).

All digests are the string `"sha256:" + hex(sha256(JCS(value)))`.

1. **api_version guard**: require exactly `"aci/1"`; else typed `ErrUnsupportedAPIVersion`
   ("client supports aci/1, gateway sent %q"). This is the version tracking.
2. **Identity digests**: recompute and compare to the report's top-level fields:
   - `workload_id = sha256:hex(sha256(JCS(workload_identity.public_key)))` (the
     `{algo, public_key}` object only).
   - `workload_keyset_digest = sha256:hex(sha256(JCS(workload_keyset)))`.
3. **report_data binding**: build `statement = {purpose:"aci.report_data.v1", workload_id,
   workload_keyset_digest, nonce}` (nonce = the UTF-8 string we sent, or JSON null);
   `expected = sha256(JCS(statement))`; compare to `attestation.report_data` (32 bytes hex).
4. **TDX quote (DCAP)**: `tee.VerifyTDXQuote(hex(attestation.evidence.quote))` → 64-byte
   report_data; require `[0:32] == expected` and `[32:64] == 0`. Verify `tee_type` matches.
   (Optional hardening, later: RTMR3 replay via SHA-384 over zeroed-48 accumulator, app-id
   extraction, dstack-kms custody chain.)
5. **keyset endorsement**: `keyset_endorsement.algo` must equal the identity key algo;
   payload = `JCS({purpose:"aci.keyset.endorsement.v1", workload_keyset_digest})`; verify
   with the identity public key — ed25519 over payload, or **secp256k1 64-byte `r‖s`** over
   `sha256(payload)`.
6. **freshness**: require `freshness.fetched_at <= now < freshness.stale_after`.

GPU/NVIDIA evidence: **out of scope** (legacy-only, never a gate in `aci/1`).

Trust chain: TDX quote (DCAP) commits to report_data → report_data embeds workload_id +
keyset_digest + our nonce → digests recomputed from the keyset must match → endorsement
proves the identity key owns the keyset → `receipt_signing_keys[]` are members of that
keyset.

## Receipt verification (Phase 1)

- Fetch `GET /v1/aci/receipts/{id}` (id from `x-receipt-id` header; legacy alias
  `/v1/signature/{id}`). Accept both `{...}` and `{ "receipt": {...} }`.
- **Identity match**: `receipt.workload_id == report.workload_id` and
  `receipt.workload_keyset_digest == report.workload_keyset_digest`.
- **Key selection**: the `receipt_signing_keys[]` entry with `key_id == signature.key_id`.
- **Canonical bytes**: `JCS(receipt with signature.value omitted)` (keep `algo`+`key_id`).
- **Signature**: ed25519 raw 64-byte over the canonical bytes; or **secp256k1 65-byte
  `r‖s‖v`** over `sha256(canonical bytes)`, recover-and-equal the listed pubkey (bare
  64-byte JOSE form rejected).
- **Hash tie-in (optional)**: `sha256:hex(sha256(body))` vs event `request.received.body_hash`
  and `response.returned.{cleartext_hash|wire_hash}`.

## Error handling

Typed errors throughout (extend the existing `AttestationError` reason set); fail closed on
every check; **loud, actionable** version-drift error. No silent downgrade of the security
posture.

## Crypto dependencies (Go)

`crypto/sha256`, `crypto/sha512` (SHA-384), `golang.org/x/crypto/sha3` (Keccak-256),
`crypto/ed25519`, and secp256k1 (`github.com/decred/dcrd/dcrec/secp256k1/v4` (+`/ecdsa`)
or go-ethereum `crypto`) — two modes: 65-byte recoverable (receipts) and 64-byte verify
(endorsement). TDX DCAP reuses `pkg/llm/tee` (go-tdx-guest).

## Testing (TDD)

- **Offline unit tests** against the captured real report (`aci1_report.json`):
  JCS canonicalization (with vectors generated from the Rust example to assert
  byte-identical output), digest recomputation, report_data binding, api_version guard,
  keyset endorsement, freshness, and receipt canonical-bytes + signature verification.
- **Gated live integration test**: full attest → chat → (Phase 2: seal/open) → receipt
  round-trip against `inference.phala.com` (skips without `PHALA_API_KEY`).

## Out of scope / follow-ups

- **E2EE v2 (Phase 2)** — needs a dedicated protocol-extraction pass (seal/open wire format,
  KEM/AEAD) from the Dstack source before implementation.
- Optional DCAP hardening (RTMR3 replay, app-id, KMS custody chain).
- GPU evidence (legacy profile only).

## swe impact

None beyond the `auto.New` provider mapping. This is an SDK capability, not a GLM-5.2 fix —
SDK users choose the provider. Re-verify end-to-end (any model) after it lands.

---

## FINAL SCOPE (2026-06-24) — supersedes the phasing above

This is a **max-security SDK feature**: looprig exposes a confidential client for the Dstack
`private-ai-gateway` (Phala/RedPill). **Chutes** already provides the post-quantum
(ML-KEM-768) confidential path; **Phala** gets its ceiling, **ACI E2EE v2**. SDK users pick.
The "Phase 1 / Phase 2" split is **dropped** — the single deliverable is
**full-DCAP attestation + ACI E2EE v2 sealing + mandatory receipt verification**. Plaintext
mode is never a shipped posture. (Spec sources: Dstack `private-ai-gateway@1b43f76`,
`src/aci/*` and `examples/verify_aci_artifacts.rs`.)

### Confidentiality facts (verified against the source)
- The gateway **serves plain HTTP**; TLS is terminated by an **external, un-attested**
  component (`tls_spkis()` is empty in-TEE). TLS-SPKI pinning is therefore **not** a valid
  confidentiality anchor here — operator confidentiality requires **E2EE v2**.
- E2EE v2 ships **no client** (server + Rust lib + tests only); we implement the client.
- E2EE v2 is **classical** (secp256k1 ECDH). ML-KEM (PQ) exists only on the gateway→Chutes
  hop, not user→gateway. Quantum-safe user confidentiality ⇒ use the Chutes provider.

### Security model & review resolutions (all six accepted)
1. **Trust policy (required) + no false TLS pin.** `aci.New(baseURL, apiKey, Policy)` where
   `Policy{ AcceptedWorkloadIDs, AcceptedSourceProvenance{repo,commit}, AcceptedAppIDs/RTMRs,
   AcceptedKMSRootPubKeys }`. Anchors are **caller-supplied out-of-band** (never trusted from
   the report itself — that would be circular). Ship documented **pinned Phala/RedPill
   defaults** derived from a known-good attestation + commit `1b43f76`, flagged "verify
   independently." No TLS-SPKI pin (not anchorable on this gateway).
2. **Full DCAP.** Add `Options{GetCollateral,CheckRevocations}` to `tee.VerifyTDXQuote`
   (today both false — confirmed `intel_quote.go`); enable collateral + revocation/TCB; add
   RTMR3 event-log replay (SHA-384 over zeroed-48 accumulator vs quote RTMR3), app-id
   extraction, `source_provenance` repo+commit vs policy, and the dstack-KMS custody chain
   (two recoverable secp256k1 sigs → root ∈ policy).
3. **Mandatory receipt checks** — exact request/response **wire-hash** match,
   `endpoint`/`method`/`api_version`, and `upstream.verified.result=="verified"` for the
   expected provider/model. Signature-valid-but-mismatched ⇒ reject.
4. **Streaming = buffer until verified.** Accumulate the full sealed response, open + verify
   receipt/hashes/upstream, then emit. No pre-verification output.
5. **Canonical endpoints + commit pin** — `GET /v1/aci/attestation`, `/v1/aci/receipts/{id}`
   (both live, 200). Pin protocol to commit `1b43f76` (== deployed `repo_commit`); old
   `/v1/attestation/report`, `/v1/signature/{id}` are compat-only.
6. **Constrained JCS** (rejects floats/non-integers; UTF-16-ordered keys) with **fixed
   cross-language vectors** captured from the Rust `verify_aci_artifacts`.

### Verification algorithm (full DCAP) — condensed
All digests = `"sha256:"+hex(sha256(JCS(v)))`.
1. `api_version == "aci/1"` else `ErrUnsupportedAPIVersion`.
2. recompute `workload_id` (JCS of identity `{algo,public_key}`) and `workload_keyset_digest`
   (JCS of full keyset); compare to report.
3. `report_data = sha256(JCS({purpose:"aci.report_data.v1", workload_id,
   workload_keyset_digest, nonce}))`; compare to `attestation.report_data` (32B hex).
4. `tee.VerifyTDXQuote(hex(evidence.quote), Options{collateral,revocations})` → 64B
   report_data; require `[0:32]==report_data && [32:64]==0`; check `tee_type`; RTMR3 replay;
   app-id; `source_provenance` ∈ policy.
5. `keyset_endorsement`: algo == identity algo; verify identity key over
   `JCS({purpose:"aci.keyset.endorsement.v1", workload_keyset_digest})` (ed25519 raw, or
   **secp256k1 64B** over sha256(payload)).
6. KMS custody chain → root ∈ `policy.AcceptedKMSRootPubKeys`.
7. freshness: `fetched_at <= now < stale_after`.

### E2EE v2 seal/open — condensed
- Algo `secp256k1-aes-256-gcm-hkdf-sha256`; key = keyset `e2ee_public_keys` entry.
- Per message: ephemeral secp256k1 keypair → ECDH(model pubkey) → HKDF-SHA256
  (info `aci.e2ee.v2.secp256k1`) → AES-256-GCM key. **Field-level**: encrypt
  `messages[].content` / `prompt` / `input`; ciphertext = `hex(ephem_uncompressed(65) ‖
  nonce(12) ‖ ct+tag)`. **AAD** = `v2|req|algo=…|model=…|m=<i>|c=<sel>|n=<nonce>|ts=<ts>`.
  **Headers**: `X-E2EE-Version:2`, `X-Client-Pub-Key`, `X-Model-Pub-Key`, `X-E2EE-Nonce`,
  `X-E2EE-Timestamp`.
- Response: gateway re-encrypts `content`/`reasoning_content`/`embedding` to the client
  pubkey with `resp` AAD; client opens with its ephemeral secret. 5-min replay window.

### Receipt — condensed
`GET /v1/aci/receipts/{id}` (id from `x-receipt-id`); identity match (`workload_id`,
`workload_keyset_digest`); key = `receipt_signing_keys[]` by `signature.key_id`; canonical =
`JCS(receipt minus signature.value)`; verify ed25519 raw (64B over bytes) or **secp256k1
65B `r‖s‖v`** (over sha256(bytes), recover-and-equal). Mandatory checks per resolution 3.

### Dependencies (approved)
`github.com/decred/dcrd/dcrec/secp256k1/v4` (+`/ecdsa`) — secp256k1 verify(64B)+recover(65B)
+ ECDH for E2EE; `x/crypto/sha3` (Keccak-256); `crypto/ed25519`; DCAP via `pkg/llm/tee`
(go-tdx-guest). Recorded in `CLAUDE.md`.
