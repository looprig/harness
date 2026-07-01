package aci

import (
	"encoding/hex"
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

// FuzzKMSCustody asserts the KMS custody chain never panics on arbitrary external
// input. verifyKMSCustody and recoverK256 parse untrusted hex (the two chain
// links, the identity custody pubkey) and an untrusted provider string, recover
// secp256k1 keys over Keccak256 digests, and dispatch on an untrusted app-id, so
// both must be total: any combination of bytes must yield nil or a typed error,
// never a crash. The corpus is seeded from the fixture's real identity custody
// chain plus mutations exercising the length guards, the hex decode, the recid
// normalization (incl. raw v bytes the fixture never carries), the secp256k1
// recovery, and the provider/identity-mismatch branches.
func FuzzKMSCustody(f *testing.F) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "report_aci1.json"))
	if err != nil {
		f.Fatalf("read fixture: %v", err)
	}
	base, err := ParseReport(fixture)
	if err != nil {
		f.Fatalf("parse fixture: %v", err)
	}

	var identity KeyCustodyEntry
	for _, k := range base.Attestation.Evidence.KeyCustody.Keys {
		if k.Role == "identity" {
			identity = k
			break
		}
	}

	// Each seed is (provider, identityPubKeyHex, chain0Hex, chain1Hex, appIDHex)
	// — the five external fields verifyKMSCustody reads (the app-id arrives as a
	// hex string here and is decoded into the []byte the function takes).
	seeds := []struct {
		provider, pubKey, chain0, chain1, appID string
	}{
		{ // the real, verifying custody chain
			"dstack-kms", identity.PublicKeyHex,
			identity.SignatureChain[0], identity.SignatureChain[1],
			"fdb7a14e5a6675f752e2cb69c9067a98ca402918",
		},
		{"other-kms", identity.PublicKeyHex, identity.SignatureChain[0], identity.SignatureChain[1], "fdb7a14e"}, // wrong provider
		{"dstack-kms", "", identity.SignatureChain[0], identity.SignatureChain[1], "fdb7a14e"},                   // empty pubkey
		{"dstack-kms", "zz", identity.SignatureChain[0], identity.SignatureChain[1], "fdb7a14e"},                 // non-hex pubkey
		{"dstack-kms", "04" + "00", identity.SignatureChain[0], identity.SignatureChain[1], "fdb7a14e"},          // unparseable point
		{"dstack-kms", identity.PublicKeyHex, "", identity.SignatureChain[1], "fdb7a14e"},                        // empty chain[0]
		{"dstack-kms", identity.PublicKeyHex, "zz", identity.SignatureChain[1], "fdb7a14e"},                      // non-hex chain[0]
		{"dstack-kms", identity.PublicKeyHex, "00", identity.SignatureChain[1], "fdb7a14e"},                      // short chain[0]
		{"dstack-kms", identity.PublicKeyHex, identity.SignatureChain[0], "", "fdb7a14e"},                        // empty chain[1]
		{"dstack-kms", identity.PublicKeyHex, identity.SignatureChain[0], identity.SignatureChain[1], ""},        // empty app-id
		{"dstack-kms", identity.PublicKeyHex, identity.SignatureChain[0], identity.SignatureChain[1], "zz"},      // non-hex app-id
	}
	for _, s := range seeds {
		f.Add(s.provider, s.pubKey, s.chain0, s.chain1, s.appID)
	}

	f.Fuzz(func(t *testing.T, provider, pubKey, chain0, chain1, appIDHex string) {
		// recoverK256 directly on arbitrary bytes: the chain-link decode feeds it
		// untrusted hex, so it must be total on any byte slice.
		if sig, decErr := hex.DecodeString(chain0); decErr == nil {
			_, _ = recoverK256([]byte("aci.identity.v1:"+pubKey), sig)
		}

		rep, err := ParseReport(fixture)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		rep.Attestation.Evidence.KeyCustody.Provider = provider
		// Overwrite the identity custody entry's external fields in place.
		for i := range rep.Attestation.Evidence.KeyCustody.Keys {
			if rep.Attestation.Evidence.KeyCustody.Keys[i].Role == "identity" {
				rep.Attestation.Evidence.KeyCustody.Keys[i].PublicKeyHex = pubKey
				rep.Attestation.Evidence.KeyCustody.Keys[i].SignatureChain = []string{chain0, chain1}
			}
		}
		// Mirror the identity onto the report's published identity key so the
		// pubkey-match branch can pass for the realistic seed and exercise the
		// recovery path beyond the early mismatch return.
		rep.Attestation.Keyset.Identity.PublicKey.PublicKeyHex = pubKey

		// app-id is decoded from hex; a non-hex string yields a nil slice, which
		// is a valid (empty) app-id input — the function must stay total on it.
		appID, _ := hex.DecodeString(appIDHex)

		// The only contract under fuzz is no panic; any error is acceptable.
		_ = verifyKMSCustody(rep, appID, map[string]struct{}{fixtureKMSRoot: {}})
	})
}
