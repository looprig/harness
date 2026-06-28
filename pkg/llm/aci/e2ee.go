package aci

// This file implements the Dstack ACI "aci/1" E2EE v2 field encryption
// primitive: the seal/open pair that wraps a request/response field for the
// confidential-inference gateway. It reproduces the Rust reference
// private_ai_gateway::aci::e2ee::{encrypt_for_public_key, decrypt_with_secret_key}
// exactly so the bytes match the gateway on the wire.
//
// Scheme (ECIES-style, version "v2"):
//
//   - ECDH over secp256k1: the shared secret is the AFFINE X-COORDINATE of the
//     scalar-mult point (RFC 5903 §9 — return only x), 32 big-endian bytes.
//     decred's secp256k1.GenerateSharedSecret computes exactly this (it does
//     ScalarMultNonConst -> ToAffine -> X.Bytes()), matching k256's
//     raw_secret_bytes() and the Python reference.
//   - KDF: HKDF-SHA256 with salt=nil and info="aci.e2ee.v2.secp256k1", expanded
//     to a 32-byte AES-256 key.
//   - AEAD: AES-256-GCM with a 12-byte nonce; Go's GCM appends the 16-byte tag
//     to the ciphertext. The caller's aad is authenticated, not encrypted.
//   - Wire blob: ephemeral pubkey UNCOMPRESSED (65 bytes, starts 0x04)
//     ‖ nonce (12 bytes) ‖ ciphertext+tag. seal/open exchange it as lowercase
//     hex; open tolerates an optional 0x prefix.
//
// Security posture: the ephemeral private key and the GCM nonce are drawn from
// crypto/rand on every seal (never math/rand). open is fail-secure — any
// malformation, truncation, bad ephemeral point, or AEAD tag/AAD mismatch
// returns a typed *e2eeOpenError and never a partial plaintext. The 32-byte
// HKDF output and the ECDH secret never appear in any error message.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
	"strings"

	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/crypto/hkdf"
)

// E2EE v2 wire constants. These are protocol-fixed and define the blob layout
// and the KDF binding; they are not configurable.
const (
	// e2eePublicKeyLen is the length of the uncompressed secp256k1 ephemeral
	// public key prefixed to every blob (0x04 ‖ X(32) ‖ Y(32)).
	e2eePublicKeyLen = 65
	// e2eeNonceLen is the AES-GCM standard nonce length (96 bits).
	e2eeNonceLen = 12
	// e2eeTagLen is the AES-GCM authentication tag length (128 bits).
	e2eeTagLen = 16
	// e2eeKeyLen is the AES-256 key length the HKDF expands to.
	e2eeKeyLen = 32
	// e2eeHKDFInfo binds the derived key to this exact scheme and curve. It is
	// part of the KDF contract with the gateway; changing it breaks interop.
	e2eeHKDFInfo = "aci.e2ee.v2.secp256k1"
	// e2eeHexPrefix is the optional prefix open strips from a ciphertext hex.
	e2eeHexPrefix = "0x"
)

// e2eeMinBlobLen is the smallest valid blob: ephemeral key + nonce + bare tag
// (an empty plaintext still carries the 16-byte GCM tag). Anything shorter
// cannot hold a complete framed ciphertext.
const e2eeMinBlobLen = e2eePublicKeyLen + e2eeNonceLen + e2eeTagLen

// e2eeSealError is the typed failure returned by sealWith/seal. It carries a
// short stage label and the wrapped cause (which may be nil). It never carries
// plaintext, the ECDH secret, or the derived key.
type e2eeSealError struct {
	Stage string
	Err   error
}

func (e *e2eeSealError) Error() string {
	if e.Err != nil {
		return "aci e2ee seal: " + e.Stage + ": " + e.Err.Error()
	}
	return "aci e2ee seal: " + e.Stage
}

func (e *e2eeSealError) Unwrap() error { return e.Err }

// e2eeOpenError is the typed failure returned by open. Like e2eeSealError it
// carries only a stage label and the wrapped cause; on any failure open returns
// a nil plaintext, so no partial decryption ever escapes.
type e2eeOpenError struct {
	Stage string
	Err   error
}

func (e *e2eeOpenError) Error() string {
	if e.Err != nil {
		return "aci e2ee open: " + e.Stage + ": " + e.Err.Error()
	}
	return "aci e2ee open: " + e.Stage
}

func (e *e2eeOpenError) Unwrap() error { return e.Err }

// ecdhSharedX returns the secp256k1 ECDH shared secret: the affine
// X-coordinate of priv·pub, 32 big-endian bytes (RFC 5903 §9). This is the IKM
// fed to HKDF; it is exposed within the package for the vector cross-check.
func ecdhSharedX(priv *secp256k1.PrivateKey, pub *secp256k1.PublicKey) []byte {
	return secp256k1.GenerateSharedSecret(priv, pub)
}

// deriveKey runs HKDF-SHA256(salt=nil, IKM=shared, info=e2eeHKDFInfo) and reads
// out the 32-byte AES-256 key. It returns the typed errStage cause on any read
// failure so callers can wrap it without leaking the secret.
func deriveKey(shared []byte) ([e2eeKeyLen]byte, error) {
	var key [e2eeKeyLen]byte
	r := hkdf.New(sha256.New, shared, nil, []byte(e2eeHKDFInfo))
	if _, err := io.ReadFull(r, key[:]); err != nil {
		return key, err
	}
	return key, nil
}

// newGCM builds the AES-256-GCM AEAD for the derived key with the standard
// 12-byte nonce size. It is shared by sealWith and open so both sides frame the
// tag identically.
func newGCM(key [e2eeKeyLen]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealWith is the deterministic core of seal: it encrypts plaintext for
// recipientPub using the CALLER-SUPPLIED ephemeral key and nonce, returning the
// lowercase-hex blob (eph65 ‖ nonce12 ‖ ct+tag). It is unexported and exists so
// tests can reproduce the deterministic reference vector byte-for-byte; all
// production callers go through seal, which supplies crypto/rand material.
//
// nonce must be exactly e2eeNonceLen bytes (a wrong size is a programming error,
// reported as a typed *e2eeSealError rather than silently re-sized by GCM).
func sealWith(ephPriv *secp256k1.PrivateKey, nonce []byte, recipientPub *secp256k1.PublicKey, plaintext, aad []byte) (string, error) {
	if ephPriv == nil {
		return "", &e2eeSealError{Stage: "nil ephemeral key"}
	}
	if recipientPub == nil {
		return "", &e2eeSealError{Stage: "nil recipient key"}
	}
	if len(nonce) != e2eeNonceLen {
		return "", &e2eeSealError{Stage: "nonce length", Err: &e2eeLengthError{Field: "nonce", Got: len(nonce), Want: e2eeNonceLen}}
	}

	shared := ecdhSharedX(ephPriv, recipientPub)
	key, err := deriveKey(shared)
	if err != nil {
		return "", &e2eeSealError{Stage: "derive key", Err: err}
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", &e2eeSealError{Stage: "init gcm", Err: err}
	}

	ephPub := ephPriv.PubKey().SerializeUncompressed() // 65 bytes, starts 0x04
	ct := gcm.Seal(nil, nonce, plaintext, aad)         // appends 16-byte tag

	blob := make([]byte, 0, len(ephPub)+len(nonce)+len(ct))
	blob = append(blob, ephPub...)
	blob = append(blob, nonce...)
	blob = append(blob, ct...)
	return hex.EncodeToString(blob), nil
}

// seal encrypts plaintext for recipientPub under the E2EE v2 scheme, drawing a
// FRESH ephemeral key and 12-byte nonce from crypto/rand on every call, and
// returns the lowercase-hex wire blob. aad is authenticated, not encrypted.
func seal(recipientPub *secp256k1.PublicKey, plaintext, aad []byte) (string, error) {
	if recipientPub == nil {
		return "", &e2eeSealError{Stage: "nil recipient key"}
	}
	ephPriv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return "", &e2eeSealError{Stage: "generate ephemeral key", Err: err}
	}
	nonce := make([]byte, e2eeNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", &e2eeSealError{Stage: "generate nonce", Err: err}
	}
	return sealWith(ephPriv, nonce, recipientPub, plaintext, aad)
}

// open decrypts a hex-encoded E2EE v2 blob with recipientPriv and verifies aad.
// It strips an optional 0x prefix, parses the framed blob (eph65 ‖ nonce12 ‖
// ct+tag), re-derives the AES key via ECDH+HKDF, and runs AES-256-GCM Open.
// Every failure — bad hex, short blob, malformed ephemeral point, or AEAD
// tag/AAD mismatch — returns a typed *e2eeOpenError and a nil plaintext.
func open(recipientPriv *secp256k1.PrivateKey, ciphertextHex string, aad []byte) ([]byte, error) {
	if recipientPriv == nil {
		return nil, &e2eeOpenError{Stage: "nil recipient key"}
	}

	cleanHex := strings.TrimPrefix(ciphertextHex, e2eeHexPrefix)
	blob, err := hex.DecodeString(cleanHex)
	if err != nil {
		return nil, &e2eeOpenError{Stage: "decode hex", Err: err}
	}
	if len(blob) < e2eeMinBlobLen {
		return nil, &e2eeOpenError{Stage: "short blob", Err: &e2eeLengthError{Field: "blob", Got: len(blob), Want: e2eeMinBlobLen}}
	}

	ephBytes := blob[:e2eePublicKeyLen]
	nonce := blob[e2eePublicKeyLen : e2eePublicKeyLen+e2eeNonceLen]
	ct := blob[e2eePublicKeyLen+e2eeNonceLen:]

	ephPub, err := secp256k1.ParsePubKey(ephBytes)
	if err != nil {
		return nil, &e2eeOpenError{Stage: "parse ephemeral key", Err: err}
	}

	shared := ecdhSharedX(recipientPriv, ephPub)
	key, err := deriveKey(shared)
	if err != nil {
		return nil, &e2eeOpenError{Stage: "derive key", Err: err}
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, &e2eeOpenError{Stage: "init gcm", Err: err}
	}

	plaintext, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		// AEAD failure: wrong key, tampered ciphertext/tag, or AAD mismatch.
		return nil, &e2eeOpenError{Stage: "aead open", Err: err}
	}
	return plaintext, nil
}

// e2eeLengthError is the typed cause for a wrong-sized nonce or an undersized
// blob. It names the field and the got/want lengths so callers can inspect the
// failure without parsing a message; it carries no secret material.
type e2eeLengthError struct {
	Field string
	Got   int
	Want  int
}

func (e *e2eeLengthError) Error() string {
	return e.Field + " length " + strconv.Itoa(e.Got) + ", want at least " + strconv.Itoa(e.Want)
}
