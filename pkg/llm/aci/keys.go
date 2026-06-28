package aci

// This file implements attestation step 6 of the Dstack ACI ("aci/1") report
// verification: checking the keyset endorsement — a signature by the workload's
// identity key over the workload_keyset_digest, proving the keyset the report
// publishes is the one the identity endorsed.
//
// The verification mirrors the Rust reference
// private_ai_gateway::aci::keys::verify_keyset_endorsement:
//
//   - The endorsement's algo MUST equal the identity key's algo; a mismatch is a
//     verification failure, never a silent algorithm downgrade.
//   - The signed message is the canonical-JCS payload
//     {purpose:"aci.keyset.endorsement.v1", workload_keyset_digest}, where the
//     digest is RECOMPUTED from the keyset (identity.go's workloadKeysetDigest),
//     not the value the report merely claims — so a tampered keyset moves the
//     payload and breaks the signature.
//   - ed25519: a 32-byte public key verifies the 64-byte signature over the RAW
//     payload bytes (RFC 8032). (The Rust reference uses verify_strict, which
//     additionally rejects small-order / non-canonical points; Go's stdlib
//     Verify does not. The live fixture is secp256k1, so ed25519 is a synthetic
//     test path and the difference is immaterial here.)
//   - ecdsa-secp256k1: a 33/65-byte SEC1 public key verifies a fixed 64-byte
//     r‖s signature (NO recovery byte) over sha256(payload). r = sig[:32],
//     s = sig[32:64], big-endian; both must be < the curve order N.
//
// Every failure mode — algo mismatch, bad hex, malformed key, wrong-sized or
// out-of-range signature, or a failed verification — funnels to the fail-closed
// *llm.AttestationError with reason endorsement_invalid. The typed cause carries
// only the algorithm string and a short reason label; it never carries a private
// key (this path is verify-only and touches public keys and digests only).

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"

	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// keysetEndorsementPurpose is the fixed purpose tag of the endorsement payload.
// It is part of the signed statement, so it is a protocol constant, not config.
const keysetEndorsementPurpose = "aci.keyset.endorsement.v1"

// Endorsement signature algorithm identifiers. These are the wire algo strings;
// the endorsement and the identity key must agree on one of them.
const (
	algoEd25519   = "ed25519"
	algoSecp256k1 = "ecdsa-secp256k1"
)

// ed25519SignatureSize and secp256k1SignatureSize are the exact byte lengths the
// two signature formats require: ed25519 is 64 bytes (RFC 8032), and the
// secp256k1 endorsement is a fixed 64-byte r‖s pair with no recovery byte.
const (
	ed25519SignatureSize   = ed25519.SignatureSize
	secp256k1SignatureSize = 64
)

// keysetEndorsementPayload builds the raw canonical-JCS bytes the endorsement
// signs: the {purpose, workload_keyset_digest} object over the digest RECOMPUTED
// from ks (identity.go's workloadKeysetDigest), so the signature binds the actual
// keyset, not the report's claimed digest. JCS sorts object keys on emit, so the
// .Set order here does not affect the bytes. It returns the raw payload (NOT a
// hash); the secp256k1 arm hashes it with SHA-256, the ed25519 arm signs it raw.
func keysetEndorsementPayload(ks Keyset) ([]byte, error) {
	digest, err := workloadKeysetDigest(ks)
	if err != nil {
		return nil, err
	}
	payload := NewObject().
		Set("purpose", String(keysetEndorsementPurpose)).
		Set("workload_keyset_digest", String(digest))
	return Canonicalize(payload)
}

// verifyKeysetEndorsement is attestation step 6: it confirms the keyset
// endorsement is a valid signature by the workload's identity key over the
// recomputed workload_keyset_digest.
//
// It enforces algo agreement (endorsement.algo == identity.algo), builds the JCS
// payload, then dispatches by algorithm to verify the signature. It returns nil
// only on a cryptographically valid signature; on any failure it returns the
// fail-closed *llm.AttestationError with reason endorsement_invalid, wrapping a
// typed cause that names the algorithm and the short failure reason and carries
// no secret (public keys and digests only).
func verifyKeysetEndorsement(rep *Report) error {
	end := rep.Attestation.KeysetEndorsement
	id := rep.Attestation.Keyset.Identity.PublicKey

	if end.Algo != id.Algo {
		return endorsementInvalid(end.Algo, "algo mismatch between endorsement and identity key", nil)
	}

	payload, err := keysetEndorsementPayload(rep.Attestation.Keyset)
	if err != nil {
		return endorsementInvalid(end.Algo, "payload canonicalization failed", err)
	}

	sig, err := hex.DecodeString(end.ValueHex)
	if err != nil {
		return endorsementInvalid(end.Algo, "signature hex decode failed", err)
	}
	pub, err := hex.DecodeString(id.PublicKeyHex)
	if err != nil {
		return endorsementInvalid(end.Algo, "public key hex decode failed", err)
	}

	switch end.Algo {
	case algoEd25519:
		return verifyEd25519Endorsement(pub, payload, sig)
	case algoSecp256k1:
		return verifySecp256k1Endorsement(pub, payload, sig)
	default:
		return endorsementInvalid(end.Algo, "unsupported endorsement algorithm", nil)
	}
}

// verifyEd25519Endorsement verifies a 64-byte ed25519 signature over the raw
// payload with a 32-byte public key (RFC 8032). Both sizes are checked before
// the stdlib verify, which would otherwise panic on a wrong-length key.
func verifyEd25519Endorsement(pub, payload, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return endorsementInvalid(algoEd25519, "ed25519 public key wrong length", nil)
	}
	if len(sig) != ed25519SignatureSize {
		return endorsementInvalid(algoEd25519, "ed25519 signature wrong length", nil)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		return endorsementInvalid(algoEd25519, "ed25519 signature verification failed", nil)
	}
	return nil
}

// verifySecp256k1Endorsement verifies a fixed 64-byte r‖s secp256k1 signature
// (no recovery byte) over sha256(payload) with a SEC1 (33/65-byte) public key.
// r = sig[:32], s = sig[32:64], big-endian; a scalar that overflows the curve
// order N is rejected (SetByteSlice reports the overflow).
func verifySecp256k1Endorsement(pub, payload, sig []byte) error {
	if len(sig) != secp256k1SignatureSize {
		return endorsementInvalid(algoSecp256k1, "secp256k1 signature wrong length", nil)
	}
	pubKey, err := secp256k1.ParsePubKey(pub)
	if err != nil {
		return endorsementInvalid(algoSecp256k1, "secp256k1 public key parse failed", err)
	}

	var r, s secp256k1.ModNScalar
	if r.SetByteSlice(sig[:32]) {
		return endorsementInvalid(algoSecp256k1, "secp256k1 signature r overflows curve order", nil)
	}
	if s.SetByteSlice(sig[32:64]) {
		return endorsementInvalid(algoSecp256k1, "secp256k1 signature s overflows curve order", nil)
	}

	digest := sha256.Sum256(payload)
	if !ecdsa.NewSignature(&r, &s).Verify(digest[:], pubKey) {
		return endorsementInvalid(algoSecp256k1, "secp256k1 signature verification failed", nil)
	}
	return nil
}

// endorsementInvalid builds the fail-closed *llm.AttestationError for an
// endorsement failure, wrapping a typed *endorsementError cause (per CLAUDE.md:
// no bare fmt.Errorf from package APIs). cause may be nil for failures that have
// no underlying error to chain (e.g. a wrong-length signature).
func endorsementInvalid(algo, reason string, cause error) error {
	return attestErr(reasonEndorsementInvalid, &endorsementError{
		algo:   algo,
		reason: reason,
		cause:  cause,
	})
}

// endorsementError is the typed cause wrapped inside the endorsement_invalid
// *llm.AttestationError. It names the algorithm and a fixed reason label so the
// cause keeps type identity and callers can errors.As to inspect it. It carries
// no key material — only the public algo string and a static reason — so logging
// it leaks no secret. cause chains any underlying stdlib/library error.
type endorsementError struct {
	// algo is the wire algorithm string (e.g. "ecdsa-secp256k1"); external but
	// a short identifier, never key bytes.
	algo string
	// reason is a fixed, code-defined label, never external data.
	reason string
	// cause is the chained underlying error, or nil.
	cause error
}

func (e *endorsementError) Error() string {
	msg := "aci/keys: endorsement invalid (" + e.algo + "): " + e.reason
	if e.cause != nil {
		return msg + ": " + e.cause.Error()
	}
	return msg
}

func (e *endorsementError) Unwrap() error { return e.cause }
