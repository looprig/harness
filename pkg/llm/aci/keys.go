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
	"golang.org/x/crypto/sha3"
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
// order N is rejected (SetByteSlice reports the overflow), and decred's Verify
// itself rejects r/s == 0 (it requires r, s in [1, N-1]). Low-S is intentionally
// NOT enforced: the k256 reference does not mandate it on the verify path, and a
// high-S (malleable) signature is harmless here because verification binds the
// RECOMPUTED keyset digest — there is no second signed object to forge against.
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

// =============================================================================
// Task 2.7: KMS custody chain (recoverable secp256k1 over Keccak256)
// =============================================================================
//
// verifyKMSCustody establishes that the workload's identity key descends from a
// trusted KMS root, mirroring the Rust reference
// dstack::verify_dstack_kms_identity_custody. It walks a fixed 2-link chain of
// recoverable secp256k1 signatures, each over a Keccak256 digest:
//
//   chain[0]: recover the app key from a signature over the UTF-8 bytes
//             "{purpose}:{compressed_identity_pubkey_hex}".
//   chain[1]: recover the KMS root from a signature over the byte concatenation
//             "dstack-kms-issued" ‖ ":" ‖ app_id ‖ app_pubkey_sec1.
//
// The recovered root's compressed SEC1 hex must be a member of the caller's
// acceptedRoots set; anything else — wrong provider, missing identity entry,
// pubkey mismatch, malformed chain, recovery failure, an unaccepted root, or an
// EMPTY accepted set — fails closed with reason kms_root_untrusted. Every failure
// means the same thing: we could not tie the identity key to a trusted KMS root.
//
// The digest is Keccak256 (sha3.NewLegacyKeccak256), the Ethereum/pre-standard
// Keccak — NOT FIPS-202 SHA3-256. The two differ in their padding byte, so using
// the FIPS variant here would silently recover the wrong key.

// dstackKMSProvider is the only key-custody provider this client trusts. A
// custody record naming any other provider fails closed.
const dstackKMSProvider = "dstack-kms"

// identityCustodyRole is the role string of the identity key's custody entry.
const identityCustodyRole = "identity"

// kmsChainLength is the exact number of links in a Dstack KMS custody chain:
// chain[0] (purpose -> app key) and chain[1] (issued -> KMS root).
const kmsChainLength = 2

// recoverableSigSize is the byte length of a recoverable secp256k1 signature:
// r(32) ‖ s(32) ‖ v(1), the r‖s‖v (v-LAST) layout Dstack emits.
const recoverableSigSize = 65

// kmsIssuedPrefix is the fixed byte prefix of the chain[1] root_message:
// "dstack-kms-issued" ‖ ":" — the app_id and app pubkey are appended to it.
const kmsIssuedPrefix = "dstack-kms-issued:"

// recoverK256 recovers the secp256k1 public key that signed message, from a
// 65-byte recoverable signature in Dstack's r‖s‖v (v-last) layout, over the
// Keccak256 digest of message.
//
// It normalizes the recovery id (a v byte in [27,30] is shifted down by 27, the
// Ethereum convention; a raw 0..3 is used as-is), reorders to decred's v-FIRST
// compact layout [27+recid]‖r‖s, hashes message with Keccak256, and calls
// ecdsa.RecoverCompact. The returned wasCompressed bool is ignored; callers
// serialize the recovered key themselves (SerializeCompressed). Any malformed
// length, out-of-range recid, or recovery failure returns a typed error.
func recoverK256(message, sig65 []byte) (*secp256k1.PublicKey, error) {
	if len(sig65) != recoverableSigSize {
		return nil, &kmsCustodyError{reason: "recoverable signature wrong length"}
	}

	// Normalize the recovery id. Dstack puts v LAST (r‖s‖v); a v in [27,30] is
	// the Ethereum-offset form (27 + recid), so subtract 27 to get the raw recid.
	v := sig65[64]
	if v >= 27 && v <= 30 {
		v -= 27
	}
	if v > 3 {
		return nil, &kmsCustodyError{reason: "recoverable signature recovery id out of range"}
	}

	// decred's RecoverCompact wants the recovery byte FIRST: [27+recid]‖r‖s. The
	// +4 "compressed" flag is omitted (it only signals output preference, which we
	// override by serializing ourselves). r = sig[:32], s = sig[32:64].
	compact := make([]byte, recoverableSigSize)
	compact[0] = secp256k1CompactMagic + v
	copy(compact[1:33], sig65[:32])
	copy(compact[33:65], sig65[32:64])

	// Keccak256 (legacy Keccak, NOT FIPS SHA3-256) of the message is the digest
	// the signature commits to.
	h := sha3.NewLegacyKeccak256()
	h.Write(message)
	digest := h.Sum(nil)

	pub, _, err := ecdsa.RecoverCompact(compact, digest)
	if err != nil {
		return nil, &kmsCustodyError{reason: "secp256k1 recovery failed", cause: err}
	}
	return pub, nil
}

// secp256k1CompactMagic is decred's compact-signature recovery-code offset (27),
// inherited from Bitcoin. recoverK256 adds the raw recid to it to build the
// v-first compact recovery byte RecoverCompact expects.
const secp256k1CompactMagic = 27

// verifyKMSCustody confirms the workload identity key descends from a trusted
// KMS root by recovering and checking the 2-link custody chain. appID is the
// 20-byte workload app-id from the event log (Task 2.5's verifyEventLogAndAppID);
// acceptedRoots is the set of trusted KMS-root compressed-SEC1 hex strings.
//
// It returns nil ONLY when the recovered root's compressed hex is a member of
// acceptedRoots. Every other outcome — including an empty or nil acceptedRoots —
// returns the fail-closed *llm.AttestationError with reason kms_root_untrusted.
func verifyKMSCustody(rep *Report, appID []byte, acceptedRoots map[string]struct{}) error {
	custody := rep.Attestation.Evidence.KeyCustody
	if custody.Provider != dstackKMSProvider {
		return kmsRootUntrusted("key-custody provider is not "+dstackKMSProvider, nil)
	}

	identity, ok := findIdentityCustodyEntry(custody)
	if !ok {
		return kmsRootUntrusted("no identity-role key-custody entry", nil)
	}

	// The identity custody key must equal the report's published identity key:
	// the chain proves custody of THAT key, not some other one.
	reportIdentityHex := rep.Attestation.Keyset.Identity.PublicKey.PublicKeyHex
	if identity.PublicKeyHex != reportIdentityHex {
		return kmsRootUntrusted("identity custody public key does not match report identity", nil)
	}

	if len(identity.SignatureChain) != kmsChainLength {
		return kmsRootUntrusted("identity signature_chain is not a 2-link chain", nil)
	}

	// Compress the identity key to SEC1 (33 bytes); it is the report's
	// uncompressed (65-byte) SEC1 key, used in the chain[0] message.
	identityRaw, err := hex.DecodeString(identity.PublicKeyHex)
	if err != nil {
		return kmsRootUntrusted("identity custody public key is not hex", err)
	}
	identityPub, err := secp256k1.ParsePubKey(identityRaw)
	if err != nil {
		return kmsRootUntrusted("identity custody public key does not parse", err)
	}
	compressedIdentity := hex.EncodeToString(identityPub.SerializeCompressed())

	// chain[0]: recover the app key from "{purpose}:{compressed_identity}".
	purposeSig, err := hex.DecodeString(identity.SignatureChain[0])
	if err != nil {
		return kmsRootUntrusted("chain[0] signature is not hex", err)
	}
	purposeMessage := []byte(identity.Purpose + ":" + compressedIdentity)
	appPub, err := recoverK256(purposeMessage, purposeSig)
	if err != nil {
		return kmsRootUntrusted("chain[0] app-key recovery failed", err)
	}
	appPubSec1 := appPub.SerializeCompressed()

	// chain[1]: recover the KMS root from
	// "dstack-kms-issued:" ‖ app_id ‖ app_pubkey_sec1.
	appSig, err := hex.DecodeString(identity.SignatureChain[1])
	if err != nil {
		return kmsRootUntrusted("chain[1] signature is not hex", err)
	}
	rootMessage := make([]byte, 0, len(kmsIssuedPrefix)+len(appID)+len(appPubSec1))
	rootMessage = append(rootMessage, kmsIssuedPrefix...)
	rootMessage = append(rootMessage, appID...)
	rootMessage = append(rootMessage, appPubSec1...)
	rootPub, err := recoverK256(rootMessage, appSig)
	if err != nil {
		return kmsRootUntrusted("chain[1] KMS-root recovery failed", err)
	}

	rootCompressedHex := hex.EncodeToString(rootPub.SerializeCompressed())
	if _, accepted := acceptedRoots[rootCompressedHex]; !accepted {
		// Fail-secure: an empty/nil acceptedRoots reaches here and rejects.
		return kmsRootUntrusted("recovered KMS root is not in the accepted set", nil)
	}
	return nil
}

// findIdentityCustodyEntry returns the role=="identity" custody entry, or ok
// false when absent. It returns a copy by value; callers only read it.
func findIdentityCustodyEntry(custody KeyCustody) (KeyCustodyEntry, bool) {
	for _, k := range custody.Keys {
		if k.Role == identityCustodyRole {
			return k, true
		}
	}
	return KeyCustodyEntry{}, false
}

// kmsRootUntrusted builds the fail-closed *llm.AttestationError for a custody
// failure, wrapping a typed *kmsCustodyError cause (per CLAUDE.md: no bare
// fmt.Errorf from package APIs). cause may be nil for failures with no underlying
// error to chain (e.g. a provider mismatch or an unaccepted root).
func kmsRootUntrusted(reason string, cause error) error {
	return attestErr(reasonKMSRootUntrusted, &kmsCustodyError{reason: reason, cause: cause})
}

// kmsCustodyError is the typed cause wrapped inside the kms_root_untrusted
// *llm.AttestationError (and returned directly by recoverK256). It carries only a
// fixed, code-defined reason label and an optional chained cause — never key
// material (this path handles only public compressed-SEC1 hexes), so logging it
// leaks no secret.
type kmsCustodyError struct {
	// reason is a fixed, code-defined label, never external data.
	reason string
	// cause is the chained underlying error, or nil.
	cause error
}

func (e *kmsCustodyError) Error() string {
	msg := "aci/keys: kms custody: " + e.reason
	if e.cause != nil {
		return msg + ": " + e.cause.Error()
	}
	return msg
}

func (e *kmsCustodyError) Unwrap() error { return e.cause }
