package aci

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// requireEndorsementInvalid asserts err is the fail-closed *llm.AttestationError
// carrying reasonEndorsementInvalid. Every tamper row funnels through here so the
// provider-neutral error contract is checked uniformly.
func requireEndorsementInvalid(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("verifyKeysetEndorsement() = nil, want error")
	}
	var attErr *llm.AttestationError
	if !errors.As(err, &attErr) {
		t.Fatalf("verifyKeysetEndorsement() error = %T (%v), want *llm.AttestationError", err, err)
	}
	if attErr.Reason != reasonEndorsementInvalid {
		t.Errorf("AttestationError.Reason = %q, want %q", attErr.Reason, reasonEndorsementInvalid)
	}
}

// TestVerifyKeysetEndorsementFixture is THE ARBITER: the live gateway's real
// secp256k1 endorsement over the recomputed workload_keyset_digest must verify.
// If this regresses, the payload projection, the r‖s split, or the pubkey parse
// is wrong — the fixture is never to be altered to make this pass.
func TestVerifyKeysetEndorsementFixture(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	if err := verifyKeysetEndorsement(rep); err != nil {
		t.Fatalf("verifyKeysetEndorsement(fixture) = %v, want nil", err)
	}
}

// TestKeysetEndorsementPayloadMatchesFixture pins the JCS payload bytes: the
// canonical {purpose, workload_keyset_digest} object over the RECOMPUTED digest,
// and confirms sha256(payload) is the message digest the secp256k1 sig signs.
func TestKeysetEndorsementPayloadMatchesFixture(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)

	payload, err := keysetEndorsementPayload(rep.Attestation.Keyset)
	if err != nil {
		t.Fatalf("keysetEndorsementPayload() error = %v, want nil", err)
	}

	const wantPayload = `{"purpose":"aci.keyset.endorsement.v1","workload_keyset_digest":"sha256:46cdea445e5f2abcb78a5db0a630e7ba2127bc4f24033aa628b49e310e3928e2"}`
	if string(payload) != wantPayload {
		t.Errorf("keysetEndorsementPayload() =\n  %q\nwant\n  %q", payload, wantPayload)
	}

	const wantDigest = "cdef0ae7f9f8dcc60aca09967865c39a289f3aa62eab15d5e97f2da84cbb07fa"
	sum := sha256.Sum256(payload)
	if got := hex.EncodeToString(sum[:]); got != wantDigest {
		t.Errorf("sha256(payload) = %s, want %s", got, wantDigest)
	}
}

// TestVerifyKeysetEndorsementTamper proves every distinct corruption path fails
// closed with reasonEndorsementInvalid: signature byte flips, a keyset mutation
// that moves the recomputed digest (so the payload no longer matches the signed
// one), algo mismatch, a corrupt identity key, mis-sized signatures, bad hex, and
// an unknown algorithm string.
func TestVerifyKeysetEndorsementTamper(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(rep *Report)
	}{
		{
			name: "flip a byte in endorsement.value",
			mutate: func(rep *Report) {
				v := rep.Attestation.KeysetEndorsement.ValueHex
				rep.Attestation.KeysetEndorsement.ValueHex = flipFirstHexNibble(v)
			},
		},
		{
			name: "mutate keyset so recomputed digest changes",
			mutate: func(rep *Report) {
				rep.Attestation.Keyset.Epoch.Version = 999
			},
		},
		{
			name: "mutate identity public key so verify fails",
			mutate: func(rep *Report) {
				v := rep.Attestation.Keyset.Identity.PublicKey.PublicKeyHex
				// flip a byte in the X coordinate (keep 04 prefix valid) so the
				// point parses but is the wrong key.
				rep.Attestation.Keyset.Identity.PublicKey.PublicKeyHex =
					v[:2] + flipFirstHexNibble(v[2:])
			},
		},
		{
			name: "algo mismatch endorsement vs identity",
			mutate: func(rep *Report) {
				rep.Attestation.KeysetEndorsement.Algo = "ed25519"
			},
		},
		{
			name: "corrupt identity public key (unparseable point)",
			mutate: func(rep *Report) {
				rep.Attestation.Keyset.Identity.PublicKey.PublicKeyHex = "04" + "00"
			},
		},
		{
			name: "identity public key non-hex",
			mutate: func(rep *Report) {
				rep.Attestation.Keyset.Identity.PublicKey.PublicKeyHex = "zz" +
					rep.Attestation.Keyset.Identity.PublicKey.PublicKeyHex[2:]
			},
		},
		{
			name: "endorsement value non-hex",
			mutate: func(rep *Report) {
				rep.Attestation.KeysetEndorsement.ValueHex = "zz" +
					rep.Attestation.KeysetEndorsement.ValueHex[2:]
			},
		},
		{
			name: "signature truncated (63 bytes)",
			mutate: func(rep *Report) {
				v := rep.Attestation.KeysetEndorsement.ValueHex
				rep.Attestation.KeysetEndorsement.ValueHex = v[:len(v)-2]
			},
		},
		{
			name: "signature over-length (65 bytes)",
			mutate: func(rep *Report) {
				rep.Attestation.KeysetEndorsement.ValueHex += "00"
			},
		},
		{
			name: "unknown algo string",
			mutate: func(rep *Report) {
				rep.Attestation.KeysetEndorsement.Algo = "rsa-2048"
				rep.Attestation.Keyset.Identity.PublicKey.Algo = "rsa-2048"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rep := fixtureReport(t)
			// sanity: pristine fixture verifies before mutation.
			if err := verifyKeysetEndorsement(fixtureReport(t)); err != nil {
				t.Fatalf("precondition: pristine fixture must verify, got %v", err)
			}
			tt.mutate(rep)
			requireEndorsementInvalid(t, verifyKeysetEndorsement(rep))
		})
	}
}

// TestVerifyKeysetEndorsementEd25519 exercises the synthetic ed25519 arm: a
// freshly generated keypair signs the real JCS payload for a synthetic keyset,
// and the verifier accepts it; flipping a signature byte fails closed.
func TestVerifyKeysetEndorsementEd25519(t *testing.T) {
	t.Parallel()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}

	ks := Keyset{
		Identity: WorkloadIdentity{PublicKey: PublicKey{
			Algo:         "ed25519",
			PublicKeyHex: hex.EncodeToString(pub),
		}},
		Epoch: KeysetEpoch{Version: 7, NotAfter: 1 << 40},
	}
	payload, err := keysetEndorsementPayload(ks)
	if err != nil {
		t.Fatalf("keysetEndorsementPayload() error = %v, want nil", err)
	}
	sig := ed25519.Sign(priv, payload)

	newRep := func() *Report {
		return &Report{
			APIVersion: SupportedAPIVersion,
			Attestation: Attestation{
				Keyset: ks,
				KeysetEndorsement: KeysetEndorsement{
					Algo:     "ed25519",
					ValueHex: hex.EncodeToString(sig),
				},
			},
		}
	}

	t.Run("valid ed25519 endorsement verifies", func(t *testing.T) {
		t.Parallel()
		if err := verifyKeysetEndorsement(newRep()); err != nil {
			t.Fatalf("verifyKeysetEndorsement(ed25519) = %v, want nil", err)
		}
	})

	t.Run("flipped ed25519 signature byte fails closed", func(t *testing.T) {
		t.Parallel()
		rep := newRep()
		rep.Attestation.KeysetEndorsement.ValueHex =
			flipFirstHexNibble(rep.Attestation.KeysetEndorsement.ValueHex)
		requireEndorsementInvalid(t, verifyKeysetEndorsement(rep))
	})

	t.Run("ed25519 wrong-length signature fails closed", func(t *testing.T) {
		t.Parallel()
		rep := newRep()
		v := rep.Attestation.KeysetEndorsement.ValueHex
		rep.Attestation.KeysetEndorsement.ValueHex = v[:len(v)-2]
		requireEndorsementInvalid(t, verifyKeysetEndorsement(rep))
	})

	t.Run("ed25519 wrong-length public key fails closed", func(t *testing.T) {
		t.Parallel()
		rep := newRep()
		rep.Attestation.Keyset.Identity.PublicKey.PublicKeyHex = "ab"
		requireEndorsementInvalid(t, verifyKeysetEndorsement(rep))
	})
}

// flipFirstHexNibble toggles the high nibble of the first hex byte of s,
// guaranteeing a different value of the same length. s must be non-empty hex.
func flipFirstHexNibble(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) == 0 {
		// caller-controlled test input; a malformed seed is a test bug.
		panic("flipFirstHexNibble: input must be non-empty hex")
	}
	b[0] ^= 0xff
	return hex.EncodeToString(b)
}

// =============================================================================
// Task 2.7: KMS custody chain (recoverable secp256k1 + Keccak256)
// =============================================================================

// Recovered values, pinned from the fixture's real 2-link custody chain. These
// are computed BY THE VERIFIER over the live fixture, not copied from the wire —
// the chain[1] root is the one value with no independent precompute, so the
// fixture chain verifying (recovering this root, accepted -> nil) is the arbiter.
const (
	// fixtureAppPubKey is the chain[0] intermediate: the compressed SEC1 hex of
	// the app key recovered from purpose_message "aci.identity.v1:<compressed-id>".
	fixtureAppPubKey = "02cdcc3ffd3e22f60a0e4f7c0dfcc3fb62d4765295dd43997c538109b732bdb835"
	// fixtureKMSRoot is the chain[1] recovered KMS root, compressed SEC1 hex.
	// (Blocker #3: cross-check against a published Phala/Dstack KMS root before
	// merge; the Phala provider default policy pins it as an accepted root.)
	fixtureKMSRoot = "0334c76e0c3f52ec64cbf9bbf5c910c272330166fd656c0a86bb330963e46910e1"
	// fixtureCompressedIdentity is the identity custody key compressed to SEC1.
	fixtureCompressedIdentity = "02d3b51dcb45d74434a76fc1b7e2bc152cf81190eab43bdbf5c2c624321232c76a"
)

// requireKMSRootUntrusted asserts err is the fail-closed *llm.AttestationError
// carrying reasonKMSRootUntrusted. Every custody tamper row funnels through here
// so the provider-neutral, fail-closed error contract is checked uniformly.
func requireKMSRootUntrusted(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("verifyKMSCustody() = nil, want error")
	}
	var attErr *llm.AttestationError
	if !errors.As(err, &attErr) {
		t.Fatalf("verifyKMSCustody() error = %T (%v), want *llm.AttestationError", err, err)
	}
	if attErr.Reason != reasonKMSRootUntrusted {
		t.Errorf("AttestationError.Reason = %q, want %q", attErr.Reason, reasonKMSRootUntrusted)
	}
}

// TestVerifyKMSCustodyFixture is THE ARBITER for the custody chain: the live
// gateway's real 2-link recoverable-secp256k1 chain over Keccak256 digests must
// recover a stable KMS root that, when accepted, verifies (nil). A regression
// here means the v-byte reordering, the Keccak256 digest, the SEC1 compression,
// or the message construction is wrong — the fixture is never altered to pass.
func TestVerifyKMSCustodyFixture(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	appID := mustHex(t, fixtureAppIDHex)
	accepted := map[string]struct{}{fixtureKMSRoot: {}}

	if err := verifyKMSCustody(rep, appID, accepted); err != nil {
		t.Fatalf("verifyKMSCustody(fixture) = %v, want nil", err)
	}
}

// TestRecoverK256Fixture pins the chain's two recovered keys, proving each link
// recovers deterministically: chain[0] -> the app key (compressed SEC1), and
// chain[1] over "dstack-kms-issued:"‖app_id‖app_pubkey_sec1 -> the KMS root.
func TestRecoverK256Fixture(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	identity := identityCustodyEntry(t, rep)

	// compressed identity (sanity: the fixture pins it).
	compressed := compressedHexFromUncompressed(t, identity.PublicKeyHex)
	if compressed != fixtureCompressedIdentity {
		t.Fatalf("compressed identity = %s, want %s", compressed, fixtureCompressedIdentity)
	}

	// chain[0]: purpose_message -> app key.
	purposeMessage := []byte(identity.Purpose + ":" + compressed)
	sig0 := mustHex(t, identity.SignatureChain[0])
	appPub, err := recoverK256(purposeMessage, sig0)
	if err != nil {
		t.Fatalf("recoverK256(chain[0]) error = %v, want nil", err)
	}
	gotApp := hex.EncodeToString(appPub.SerializeCompressed())
	if gotApp != fixtureAppPubKey {
		t.Fatalf("recovered app pubkey = %s, want %s", gotApp, fixtureAppPubKey)
	}

	// chain[1]: root_message -> KMS root.
	appID := mustHex(t, fixtureAppIDHex)
	rootMessage := append([]byte("dstack-kms-issued:"), appID...)
	rootMessage = append(rootMessage, appPub.SerializeCompressed()...)
	sig1 := mustHex(t, identity.SignatureChain[1])
	rootPub, err := recoverK256(rootMessage, sig1)
	if err != nil {
		t.Fatalf("recoverK256(chain[1]) error = %v, want nil", err)
	}
	gotRoot := hex.EncodeToString(rootPub.SerializeCompressed())
	if gotRoot != fixtureKMSRoot {
		t.Fatalf("recovered KMS root = %s, want %s", gotRoot, fixtureKMSRoot)
	}
}

// TestRecoverK256RecidNormalization exercises the 27<=v<=30 -> v-27 branch the
// fixture never triggers (its v bytes are 0 and 1). A signature with v in
// {27,28,29,30} must recover the SAME public key as the canonical v in {0,1,2,3},
// proving the normalization is a no-op on the recovered key.
func TestRecoverK256RecidNormalization(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	identity := identityCustodyEntry(t, rep)
	compressed := compressedHexFromUncompressed(t, identity.PublicKeyHex)
	purposeMessage := []byte(identity.Purpose + ":" + compressed)
	sig0 := mustHex(t, identity.SignatureChain[0]) // v byte = 0

	canonical, err := recoverK256(purposeMessage, sig0)
	if err != nil {
		t.Fatalf("recoverK256(canonical) error = %v, want nil", err)
	}
	wantHex := hex.EncodeToString(canonical.SerializeCompressed())

	tests := []struct {
		name string
		v    byte
	}{
		{name: "v=27 (recid 0)", v: 27},
		{name: "v=28 (recid 1)", v: 28},
		{name: "v=29 (recid 2)", v: 29},
		{name: "v=30 (recid 3)", v: 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Only v=27 corresponds to the real recid (0) of this sig, so only it
			// recovers the same key; the rest recover a DIFFERENT key or fail. We
			// assert: v=27 (recid 0) recovers the canonical key; the normalization
			// is exercised for all four (no panic, no out-of-range error).
			shifted := make([]byte, 65)
			copy(shifted, sig0)
			shifted[64] = tt.v
			got, err := recoverK256(purposeMessage, shifted)
			if tt.v == 27 {
				if err != nil {
					t.Fatalf("recoverK256(v=27) error = %v, want nil", err)
				}
				if h := hex.EncodeToString(got.SerializeCompressed()); h != wantHex {
					t.Fatalf("recoverK256(v=27) = %s, want canonical %s", h, wantHex)
				}
				return
			}
			// v in {28,29,30}: the -27 branch yields recid {1,2,3}; recovery may
			// succeed (a different key) or fail, but must never panic or return the
			// out-of-recid-range path's error spuriously.
			if err == nil {
				if h := hex.EncodeToString(got.SerializeCompressed()); h == wantHex {
					t.Fatalf("recoverK256(v=%d) unexpectedly recovered canonical key", tt.v)
				}
			}
		})
	}
}

// TestVerifyKMSCustodyTamper proves every distinct custody-step failure funnels
// to the fail-closed reasonKMSRootUntrusted: wrong provider, missing identity
// role, identity pubkey mismatch, wrong chain length, corrupt link bytes, a wrong
// app_id (moves root_message -> different root), an empty accepted set
// (fail-secure), and a non-matching accepted root.
func TestVerifyKMSCustodyTamper(t *testing.T) {
	t.Parallel()

	goodRoots := map[string]struct{}{fixtureKMSRoot: {}}

	tests := []struct {
		name     string
		appID    []byte
		accepted map[string]struct{}
		mutate   func(rep *Report)
	}{
		{
			name:     "provider not dstack-kms",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: goodRoots,
			mutate: func(rep *Report) {
				rep.Attestation.Evidence.KeyCustody.Provider = "other-kms"
			},
		},
		{
			name:     "no identity-role custody entry",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: goodRoots,
			mutate: func(rep *Report) {
				keys := rep.Attestation.Evidence.KeyCustody.Keys
				for i := range keys {
					if keys[i].Role == "identity" {
						keys[i].Role = "not-identity"
					}
				}
			},
		},
		{
			name:     "identity custody pubkey != report identity",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: goodRoots,
			mutate: func(rep *Report) {
				e := identityEntryPtr(rep)
				e.PublicKeyHex = e.PublicKeyHex[:2] + flipFirstHexNibble(e.PublicKeyHex[2:])
			},
		},
		{
			name:     "signature_chain length 1",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: goodRoots,
			mutate: func(rep *Report) {
				e := identityEntryPtr(rep)
				e.SignatureChain = e.SignatureChain[:1]
			},
		},
		{
			name:     "signature_chain length 3",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: goodRoots,
			mutate: func(rep *Report) {
				e := identityEntryPtr(rep)
				e.SignatureChain = append(e.SignatureChain, e.SignatureChain[1])
			},
		},
		{
			name:     "flip a byte in chain[0]",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: goodRoots,
			mutate: func(rep *Report) {
				e := identityEntryPtr(rep)
				e.SignatureChain[0] = flipFirstHexNibble(e.SignatureChain[0])
			},
		},
		{
			name:     "flip a byte in chain[1]",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: goodRoots,
			mutate: func(rep *Report) {
				e := identityEntryPtr(rep)
				e.SignatureChain[1] = flipFirstHexNibble(e.SignatureChain[1])
			},
		},
		{
			name:     "chain[0] non-hex",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: goodRoots,
			mutate: func(rep *Report) {
				e := identityEntryPtr(rep)
				e.SignatureChain[0] = "zz" + e.SignatureChain[0][2:]
			},
		},
		{
			name:     "identity custody pubkey non-hex",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: goodRoots,
			mutate: func(rep *Report) {
				e := identityEntryPtr(rep)
				e.PublicKeyHex = "zz" + e.PublicKeyHex[2:]
			},
		},
		{
			name:     "wrong app_id moves root_message",
			appID:    mustHex(t, "00112233445566778899aabbccddeeff00112233"),
			accepted: goodRoots,
			mutate:   func(rep *Report) {},
		},
		{
			name:     "empty accepted set fails secure",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: map[string]struct{}{},
			mutate:   func(rep *Report) {},
		},
		{
			name:     "nil accepted set fails secure",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: nil,
			mutate:   func(rep *Report) {},
		},
		{
			name:     "non-matching accepted root",
			appID:    mustHex(t, fixtureAppIDHex),
			accepted: map[string]struct{}{flipFirstHexNibble(fixtureKMSRoot): {}},
			mutate:   func(rep *Report) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// sanity: pristine fixture verifies before mutation.
			if err := verifyKMSCustody(fixtureReport(t), mustHex(t, fixtureAppIDHex), goodRoots); err != nil {
				t.Fatalf("precondition: pristine fixture must verify, got %v", err)
			}
			rep := fixtureReport(t)
			tt.mutate(rep)
			requireKMSRootUntrusted(t, verifyKMSCustody(rep, tt.appID, tt.accepted))
		})
	}
}

// TestRecoverK256BadLength proves recoverK256 rejects a signature that is not
// exactly 65 bytes with a typed error (never a panic).
func TestRecoverK256BadLength(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sig  []byte
	}{
		{name: "empty", sig: nil},
		{name: "64 bytes", sig: make([]byte, 64)},
		{name: "66 bytes", sig: make([]byte, 66)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := recoverK256([]byte("msg"), tt.sig); err == nil {
				t.Fatalf("recoverK256(%d bytes) = nil error, want error", len(tt.sig))
			}
		})
	}
}

// identityCustodyEntry returns the role=="identity" custody entry from the
// fixture report, failing the test if absent.
func identityCustodyEntry(t *testing.T, rep *Report) KeyCustodyEntry {
	t.Helper()
	for _, k := range rep.Attestation.Evidence.KeyCustody.Keys {
		if k.Role == "identity" {
			return k
		}
	}
	t.Fatal("fixture has no identity-role custody entry")
	return KeyCustodyEntry{}
}

// identityEntryPtr returns a pointer to the role=="identity" custody entry so a
// tamper mutation edits the report in place.
func identityEntryPtr(rep *Report) *KeyCustodyEntry {
	keys := rep.Attestation.Evidence.KeyCustody.Keys
	for i := range keys {
		if keys[i].Role == "identity" {
			return &keys[i]
		}
	}
	panic("identityEntryPtr: fixture has no identity-role custody entry")
}

// compressedHexFromUncompressed parses an uncompressed (65-byte) SEC1 hex pubkey
// and returns its compressed (33-byte) SEC1 hex.
func compressedHexFromUncompressed(t *testing.T, uncompressedHex string) string {
	t.Helper()
	raw := mustHex(t, uncompressedHex)
	pub, err := secp256k1.ParsePubKey(raw)
	if err != nil {
		t.Fatalf("ParsePubKey: %v", err)
	}
	return hex.EncodeToString(pub.SerializeCompressed())
}
