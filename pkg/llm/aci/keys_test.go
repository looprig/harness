package aci

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
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
