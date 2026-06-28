package aci

import "encoding/hex"

// This file implements attestation step 3 of the Dstack ACI ("aci/1") report
// verification: binding the report_data. It projects the canonical attestation
// statement, hashes it, and checks that hash against the value the quote binds to
// (attestation.report_data).
//
// The statement is
//
//	{ "purpose":                "aci.report_data.v1",
//	  "workload_id":            <recomputed workload_id "sha256:…">,
//	  "workload_keyset_digest": <recomputed workload_keyset_digest "sha256:…">,
//	  "nonce":                  <the URL-decoded UTF-8 nonce STRING, or JSON null> }
//
// and report_data = sha256(JCS(statement)). The projection SHAPE is authoritative
// — it must reproduce the Rust reference (src/aci/types.rs ::
// AttestationStatement::to_canonical_value) field set and null rules byte-for-
// byte, or the recomputed report_data will not equal the quote's. binding_test.go
// asserts equality against the live fixture's attestation.report_data.
//
// Two facts pin the encoding. First, the statement binds the RECOMPUTED identity
// digests (workloadID / workloadKeysetDigest from identity.go), NOT the report's
// claimed strings: report_data then binds to the identity WE independently
// computed. verifyIdentityDigests (step 2, run earlier in the chain) already
// guarantees recomputed == claimed, so on the verified path they coincide; using
// the recomputed values keeps this step honest even in isolation. Second, the
// nonce is the UTF-8 string that was sent (here the 64-hex query-param value used
// LITERALLY as a string, never decoded to bytes); when no nonce was sent it is
// the JSON null literal — never the string "null", never an omitted key — exactly
// like subjectValue's null handling in identity.go.

// reportDataPurpose is the statement's fixed "purpose" tag. It is the domain-
// separation constant binding this JCS projection to the report_data role
// (distinct from the endorsement payload's purpose in Task 2.6); it MUST match
// the Rust reference's AttestationStatement purpose literal exactly.
const reportDataPurpose = "aci.report_data.v1"

// nonceValue projects the optional nonce: JSON null when nil (no nonce was sent),
// else the present UTF-8 string. Emitting the JSON null literal — never the
// string "null", never an omitted key — is required for the null-branch digest to
// match the reference. The empty string is a present "" distinct from null.
func nonceValue(nonce *string) Value {
	if nonce == nil {
		return Null{}
	}
	return String(*nonce)
}

// reportDataStatement projects the canonical attestation statement from the
// RECOMPUTED identity digests and the supplied nonce. JCS sorts keys on emit, so
// the .Set order here is irrelevant to the resulting bytes; the canonical key
// order is nonce < purpose < workload_id < workload_keyset_digest. The error path
// surfaces a JCS canonicalization failure from recomputing the digests (e.g.
// invalid UTF-8 in a field); on the live report path the inputs are JSON-parsed
// strings, so it does not occur.
func reportDataStatement(rep *Report, nonce *string) (*Object, error) {
	wid, err := workloadID(rep.Attestation.Keyset.Identity)
	if err != nil {
		return nil, err
	}
	kd, err := workloadKeysetDigest(rep.Attestation.Keyset)
	if err != nil {
		return nil, err
	}
	return NewObject().
		Set("purpose", String(reportDataPurpose)).
		Set("workload_id", String(wid)).
		Set("workload_keyset_digest", String(kd)).
		Set("nonce", nonceValue(nonce)), nil
}

// reportDataBinding recomputes the report_data binding: the raw 32-byte SHA-256 of
// the canonical (JCS) attestation statement. This [32]byte is the value Task 2.4
// consumes — the quote's 64-byte report_data must equal binding(32) ‖ zero(32).
// The error path fails closed on a canonicalization failure. It is a pure
// function so Tasks 2.4/2.8 can reuse it.
func reportDataBinding(rep *Report, nonce *string) ([32]byte, error) {
	stmt, err := reportDataStatement(rep, nonce)
	if err != nil {
		return [32]byte{}, err
	}
	return Sha256Raw(stmt)
}

// verifyReportDataBinding is attestation step 3 of VerifyReport (the chain lands
// in Task 2.8): it recomputes the report_data binding from the recomputed
// identity digests and the supplied nonce, and compares its lowercase hex to the
// report's claimed attestation.report_data. On any mismatch — or on a
// canonicalization failure, which fails closed — it returns the provider-neutral
// *llm.AttestationError with reason report_data_mismatch. It returns nil only when
// the recomputed binding equals the claimed report_data, binding the quote's
// report_data to the statement (and through it, the recomputed identity).
func verifyReportDataBinding(rep *Report, nonce *string) error {
	binding, err := reportDataBinding(rep, nonce)
	if err != nil {
		return attestErr(reasonReportDataMismatch, err)
	}
	got := hex.EncodeToString(binding[:])
	if got != rep.Attestation.ReportDataHex {
		return attestErr(reasonReportDataMismatch, &reportDataMismatchError{
			claimed: rep.Attestation.ReportDataHex,
			actual:  got,
		})
	}
	return nil
}

// reportDataMismatchError is the typed cause wrapped inside the
// report_data_mismatch *llm.AttestationError. It carries the claimed (the quote's
// report_data) and the recomputed report_data hex so the cause keeps type
// identity (per CLAUDE.md: no bare fmt.Errorf from package APIs) and callers can
// errors.As to inspect it. Both values are SHA-256 digest hex strings, not key
// material or API keys, so logging them leaks no secret.
type reportDataMismatchError struct {
	// claimed is the report's attestation.report_data; actual is the report_data
	// recomputed from the canonical statement.
	claimed string
	actual  string
}

func (e *reportDataMismatchError) Error() string {
	return "aci/binding: report_data mismatch: claimed " + e.claimed + ", recomputed " + e.actual
}
