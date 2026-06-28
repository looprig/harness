package aci

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzVerifyKeysetEndorsement asserts the endorsement verification path never
// panics on arbitrary external input. verifyKeysetEndorsement parses two
// untrusted hex fields (endorsement.value, identity.public_key) and dispatches on
// two untrusted algo strings (endorsement.algo, identity.algo), so it must be
// total: any combination of bytes — valid, malformed, non-hex, wrong-length,
// unknown algo — must yield nil or a typed error, never a crash. The corpus is
// seeded from the real fixture's endorsement plus mutations exercising the hex
// decode, the secp256k1 point parse, the r/s overflow check, the length guards,
// and the algo dispatch (incl. the algo-mismatch and unknown-algo branches).
//
// keysetEndorsementPayload is fuzzed alongside on the same fixture-derived keyset
// (its inputs are the keyset's already-typed fields, not raw bytes, but the digest
// recomputation it drives must also stay panic-free under the mutated identity).
func FuzzVerifyKeysetEndorsement(f *testing.F) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "report_aci1.json"))
	if err != nil {
		f.Fatalf("read fixture: %v", err)
	}
	base, err := ParseReport(fixture)
	if err != nil {
		f.Fatalf("parse fixture: %v", err)
	}
	end := base.Attestation.KeysetEndorsement
	id := base.Attestation.Keyset.Identity.PublicKey

	// Each seed is (endorsementValueHex, endorsementAlgo, identityPubKeyHex,
	// identityAlgo) — the four external fields verifyKeysetEndorsement reads.
	seeds := []struct {
		value, endAlgo, pubKey, idAlgo string
	}{
		{end.ValueHex, end.Algo, id.PublicKeyHex, id.Algo},      // the real, verifying endorsement
		{"", end.Algo, id.PublicKeyHex, id.Algo},                // empty signature hex
		{"zz", end.Algo, id.PublicKeyHex, id.Algo},              // non-hex signature
		{end.ValueHex, "ed25519", id.PublicKeyHex, id.Algo},     // algo mismatch
		{end.ValueHex, end.Algo, "", id.Algo},                   // empty pubkey hex
		{end.ValueHex, end.Algo, "zz", id.Algo},                 // non-hex pubkey
		{end.ValueHex, end.Algo, "04" + "00", id.Algo},          // unparseable point
		{end.ValueHex, "ed25519", id.PublicKeyHex, "ed25519"},   // ed25519 path, wrong-size key/sig
		{end.ValueHex, "rsa-2048", id.PublicKeyHex, "rsa-2048"}, // unknown algo
		{"00", "ed25519", "ab", "ed25519"},                      // ed25519 short sig + short key
	}
	for _, s := range seeds {
		f.Add(s.value, s.endAlgo, s.pubKey, s.idAlgo)
	}

	f.Fuzz(func(t *testing.T, value, endAlgo, pubKey, idAlgo string) {
		// Build a fixture-derived report and overwrite only the fuzzed external
		// fields, so the keyset shape (and thus the recomputed digest path) stays
		// realistic while the signature/key/algo inputs are arbitrary.
		rep, err := ParseReport(fixture)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		rep.Attestation.KeysetEndorsement.ValueHex = value
		rep.Attestation.KeysetEndorsement.Algo = endAlgo
		rep.Attestation.Keyset.Identity.PublicKey.PublicKeyHex = pubKey
		rep.Attestation.Keyset.Identity.PublicKey.Algo = idAlgo

		// The only contract under fuzz is no panic; any error is acceptable.
		_ = verifyKeysetEndorsement(rep)
		_, _ = keysetEndorsementPayload(rep.Attestation.Keyset)
	})
}
