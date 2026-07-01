package aci

// This file implements attestation step 2 of the Dstack ACI ("aci/1") report
// verification: recomputing the workload identity and keyset digests from the
// report's own key material and comparing them to the values the report claims.
//
// The two digests are SHA-256 over the constrained-JCS canonicalization of a
// fixed projection of the keyset (jcs.go's Sha256Hex / Value union). The
// projection SHAPE is authoritative — it must reproduce the Rust reference
// (private_ai_gateway::aci::types::to_canonical_value, commit 1b43f76e) field
// set, nesting, and null/omission rules byte-for-byte, or the recomputed digests
// will not equal the gateway's claimed workload_id / workload_keyset_digest. The
// live fixture's claimed digests (testdata/report_aci1.json) are the arbiter;
// identity_test.go asserts equality against them.
//
// Two facts pin the number handling: keyset_epoch.not_after carries the
// 2^64-1 "never expires" sentinel, so the epoch fields project as Uint (u64),
// never Int; and JCS forbids floats, so an integer Value is the only correct
// encoding.

// publicKeyValue projects an algorithm-tagged public key to its canonical
// {algo, public_key} object. JCS sorts keys on emit, so the .Set order here is
// irrelevant to the resulting bytes. This is the leaf shared by the identity-key
// projection (workload_id) and the keyset projection's workload_identity. Note:
// there is NO "purpose" field — purpose appears only in the report_data
// statement and the endorsement payload (Tasks 2.2/2.6), never in this digest.
func publicKeyValue(pk PublicKey) *Object {
	return NewObject().
		Set("algo", String(pk.Algo)).
		Set("public_key", String(pk.PublicKeyHex))
}

// keyEntryValue projects one receipt-signing or E2EE key entry to its canonical
// {key_id, algo, public_key} object. The two key lists share this shape.
func keyEntryValue(e KeyEntry) *Object {
	return NewObject().
		Set("key_id", String(e.KeyID)).
		Set("algo", String(e.Algo)).
		Set("public_key", String(e.PublicKeyHex))
}

// tlsBindingValue projects one TLS binding. It always carries "spki_sha256" and
// includes "domain" ONLY when non-empty, mirroring the Rust reference's
// serde skip_serializing_if = Option::is_none on the domain field. Task 1.4
// modeled Domain as a plain string, so an empty string is treated as "absent"
// (the key is omitted). See the package report's note on the empty-vs-absent
// conflation; the live fixture always carries a non-empty domain, so this branch
// is exercised only by the synthetic "drop a tls domain" mutation test.
func tlsBindingValue(b TLSBinding) *Object {
	o := NewObject().Set("spki_sha256", String(b.SPKISHA256Hex))
	if b.Domain != "" {
		o.Set("domain", String(b.Domain))
	}
	return o
}

// subjectValue projects the optional workload-identity subject: JSON null when
// absent (Subject == nil), else the present string (including the empty string,
// which is a present "" distinct from null). Emitting the JSON null literal —
// never the string "null", never an omitted key — is required for the digest to
// match the reference.
func subjectValue(subject *string) Value {
	if subject == nil {
		return Null{}
	}
	return String(*subject)
}

// workloadIdentityValue projects the full workload identity (its public key plus
// the optional subject) for the keyset digest. The standalone workload_id digest
// uses only publicKeyValue (no subject).
func workloadIdentityValue(id WorkloadIdentity) *Object {
	return NewObject().
		Set("public_key", publicKeyValue(id.PublicKey)).
		Set("subject", subjectValue(id.Subject))
}

// keysetEpochValue projects the epoch as {version, not_after}, both u64. Uint
// (not Int) is mandatory: not_after carries 2^64-1 in the live fixture, which
// overflows i64.
func keysetEpochValue(e KeysetEpoch) *Object {
	return NewObject().
		Set("version", Uint(e.Version)).
		Set("not_after", Uint(e.NotAfter))
}

// keyEntryArray projects a slice of key entries to an ordered JSON array,
// preserving the keyset's entry order (JCS sorts object keys, not array
// elements). A nil or empty slice projects to an empty array [], so an empty key
// list still digests deterministically.
func keyEntryArray(entries []KeyEntry) Array {
	arr := make(Array, 0, len(entries))
	for _, e := range entries {
		arr = append(arr, keyEntryValue(e))
	}
	return arr
}

// tlsBindingArray projects a slice of TLS bindings to an ordered JSON array,
// preserving order. A nil or empty slice projects to an empty array [].
func tlsBindingArray(bindings []TLSBinding) Array {
	arr := make(Array, 0, len(bindings))
	for _, b := range bindings {
		arr = append(arr, tlsBindingValue(b))
	}
	return arr
}

// keysetValue projects the full keyset to its canonical Value: the workload
// identity (public key + subject), the epoch, and the three ordered key/binding
// lists. This is the exact field set, nesting, and ordering of the Rust
// reference's keyset canonical value; its Sha256Hex is workload_keyset_digest.
func keysetValue(ks Keyset) *Object {
	return NewObject().
		Set("workload_identity", workloadIdentityValue(ks.Identity)).
		Set("keyset_epoch", keysetEpochValue(ks.Epoch)).
		Set("receipt_signing_keys", keyEntryArray(ks.ReceiptSigningKeys)).
		Set("e2ee_public_keys", keyEntryArray(ks.E2EEPublicKeys)).
		Set("tls_public_keys", tlsBindingArray(ks.TLSPublicKeys))
}

// workloadID recomputes the report's workload_id: Sha256Hex over the canonical
// {algo, public_key} projection of the identity public key. The error path
// surfaces a JCS canonicalization failure (e.g. invalid UTF-8 in a field) as the
// typed jcs error; on the live report path the inputs are JSON-parsed strings,
// so it does not occur. It is a pure function so Tasks 2.2/2.8 can reuse it.
func workloadID(id WorkloadIdentity) (string, error) {
	return Sha256Hex(publicKeyValue(id.PublicKey))
}

// workloadKeysetDigest recomputes the report's workload_keyset_digest: Sha256Hex
// over the canonical projection of the full keyset. Pure, for reuse by the
// verification chain (Task 2.8).
func workloadKeysetDigest(ks Keyset) (string, error) {
	return Sha256Hex(keysetValue(ks))
}

// verifyIdentityDigests is attestation step 2 of VerifyReport (the chain lands in
// Task 2.8): it recomputes workload_id and workload_keyset_digest from the
// report's own key material and compares each to the value the report claims. On
// any mismatch — or on a canonicalization failure, which fails closed — it
// returns the provider-neutral *llm.AttestationError with reason
// keyset_digest_mismatch. It returns nil only when BOTH recomputed digests equal
// their claimed values, binding the report's claimed identity to its actual keys.
func verifyIdentityDigests(rep *Report) error {
	gotID, err := workloadID(rep.Attestation.Keyset.Identity)
	if err != nil {
		return attestErr(reasonKeysetDigestMismatch, err)
	}
	if gotID != rep.WorkloadID {
		return attestErr(reasonKeysetDigestMismatch, &digestMismatchError{
			field:   "workload_id",
			claimed: rep.WorkloadID,
			actual:  gotID,
		})
	}

	gotKeyset, err := workloadKeysetDigest(rep.Attestation.Keyset)
	if err != nil {
		return attestErr(reasonKeysetDigestMismatch, err)
	}
	if gotKeyset != rep.WorkloadKeysetDigest {
		return attestErr(reasonKeysetDigestMismatch, &digestMismatchError{
			field:   "workload_keyset_digest",
			claimed: rep.WorkloadKeysetDigest,
			actual:  gotKeyset,
		})
	}
	return nil
}

// digestMismatchError is the typed cause wrapped inside the
// keyset_digest_mismatch *llm.AttestationError. It names which digest diverged
// and carries the claimed vs recomputed values so the cause keeps type identity
// (per CLAUDE.md: no bare fmt.Errorf from package APIs) and callers can inspect
// it. The values are SHA-256 digest strings ("sha256:<hex>"), not key material,
// so logging them leaks no secret.
type digestMismatchError struct {
	// field is the digest that diverged: "workload_id" or
	// "workload_keyset_digest". A fixed label, never external data.
	field string
	// claimed is the value the report asserts; actual is the value recomputed
	// from the report's own key material.
	claimed string
	actual  string
}

func (e *digestMismatchError) Error() string {
	return "aci/identity: " + e.field + " digest mismatch: claimed " + e.claimed + ", recomputed " + e.actual
}
