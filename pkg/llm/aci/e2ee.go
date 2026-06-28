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
	"math"
	"strconv"
	"strings"
	"time"

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

// =============================================================================
// Task 3.2: sealRequest — request field sealing (text-only) + ordered body +
// headers.
// =============================================================================
//
// sealRequest encrypts the user-supplied TEXT fields of an OpenAI-shaped request
// body for the verified model's E2EE public key, in place over the ORDERED body
// (Task 1.3 *Object), then serializes the result with CompactJSON so object key
// insertion order is preserved byte-for-byte (the gateway re-serializes with
// serde_json preserve_order, so a reordering here would change the body the
// gateway hashes and decrypts).
//
// Per-field AAD (reproduced byte-exact from the Rust reference
// src/aggregator/service/e2ee_crypto.rs so the gateway recomputes the SAME AAD
// to decrypt):
//
//   - chat messages content:
//     "v2|req|algo={algo}|model={model}|m={m}|c={sel}|n={nonce}|ts={ts}"
//     where sel is "-" for a STRING content, or the ITEM INDEX (over ALL items,
//     images included) for an array {"type":"text"} part.
//   - completion prompt / embedding input:
//     "v2|req|algo={algo}|model={model}|field={field}|n={nonce}|ts={ts}"
//     where field is one of prompt, prompt.{i}, input, input.{i}.
//
// {algo} is e2eeRequestAlgo; {model} is the body's own model string field;
// {nonce} is the per-request nonce (also the X-E2EE-Nonce header); {ts} is the
// canonical uint64 decimal unix-seconds (strconv.FormatUint, no padding) used in
// BOTH the AAD and the X-E2EE-Timestamp header.
//
// Field selection mirrors the gateway's decrypt dispatch — EXACTLY ONE applies,
// in this precedence: prompt (completion) > input (embedding) > messages (chat).
// A body with none of the three is a NO-OP: it is returned unchanged (only the
// headers are produced).
//
// Only side-effect-free reads happen before sealing; the seal itself draws a
// fresh ephemeral key + nonce per field from crypto/rand (Task 3.1's seal).

// e2eeRequestAlgo is the E2EE v2 algorithm string. It selects the model's
// e2ee_public_keys entry and is bound into every per-field AAD. It is
// protocol-fixed (the gateway pins the same string); changing it breaks interop.
const e2eeRequestAlgo = "secp256k1-aes-256-gcm-hkdf-sha256"

// E2EE v2 request header names. Go's textproto canonicalization maps
// "x-client-pub-key" -> "X-Client-Pub-Key"; we emit the canonical forms directly
// so a raw map lookup by the canonical key always hits.
const (
	hdrE2EEVersion   = "X-E2EE-Version"
	hdrClientPubKey  = "X-Client-Pub-Key"
	hdrModelPubKey   = "X-Model-Pub-Key"
	hdrE2EENonce     = "X-E2EE-Nonce"
	hdrE2EETimestamp = "X-E2EE-Timestamp"
	// e2eeVersionValue is the wire value of X-E2EE-Version for this scheme.
	e2eeVersionValue = "2"
)

// Body field keys read during dispatch. They are the OpenAI request shape the
// gateway decrypts; pinned as constants so a typo cannot silently desync from
// the gateway.
const (
	bodyKeyModel    = "model"
	bodyKeyMessages = "messages"
	bodyKeyPrompt   = "prompt"
	bodyKeyInput    = "input"
	bodyKeyContent  = "content"
	bodyKeyType     = "type"
	bodyKeyText     = "text"
	// contentTypeText is the array-part type whose text field is sealed; every
	// other item type (image_url, etc.) is left untouched.
	contentTypeText = "text"
	// chatContentString is the content-index selector for a STRING content
	// (Rust content_index = None renders as "-").
	chatContentString = "-"
)

// sealedRequest is the output of sealRequest: the order-preserving sealed body,
// the E2EE v2 headers, and the material Task 3.3 needs to OPEN the gateway's
// response (which the gateway seals to ClientPriv's public key under a response
// AAD bound by the same Nonce/Timestamp/Model). ClientPriv is the fresh
// per-request client private key; it never leaves the process.
type sealedRequest struct {
	// Body is the CompactJSON of the sealed, order-preserving body.
	Body []byte
	// Headers are the E2EE v2 request headers (canonical X-… keys).
	Headers map[string]string
	// ClientPriv is the fresh client private key the gateway's response is
	// sealed to; Task 3.3 opens the response with it. Never logged, never sent.
	ClientPriv *secp256k1.PrivateKey
	// Nonce is the per-request nonce (X-E2EE-Nonce); bound into every AAD.
	Nonce string
	// Timestamp is the canonical unix-seconds bound into every AAD and the
	// X-E2EE-Timestamp header.
	Timestamp uint64
	// Model is the body's model string, surfaced for the response AAD (3.3).
	Model string
}

// e2eeNoModelKeyError is the typed failure returned when the verified keyset
// carries no e2ee_public_keys entry with algo e2eeRequestAlgo. Sealing cannot
// proceed without the recipient key, so this fails closed. It carries the
// algorithm we looked for (a fixed protocol label, never a secret).
type e2eeNoModelKeyError struct {
	Algo string
}

func (e *e2eeNoModelKeyError) Error() string {
	return "aci e2ee: no e2ee_public_keys entry with algo " + e.Algo
}

// e2eeAmbiguityError is the typed failure returned when model or nonce contains a
// byte the AAD grammar reserves as a field separator ('|', '\r', '\n'). The
// gateway rejects such values because they would let an attacker forge a
// different AAD parse, so we validate and fail closed BEFORE sealing. Field names
// the offending input (model/nonce); Value is omitted to avoid echoing
// caller-controlled bytes into logs.
type e2eeAmbiguityError struct {
	Field string
}

func (e *e2eeAmbiguityError) Error() string {
	return "aci e2ee: " + e.Field + " contains a reserved separator byte (| CR or LF)"
}

// e2eeModelFieldError is the typed failure returned when the body's model field
// is missing or not a JSON string. The AAD binds the model, so a body without a
// string model cannot be sealed deterministically against the gateway.
type e2eeModelFieldError struct{}

func (e *e2eeModelFieldError) Error() string {
	return "aci e2ee: body has no string \"model\" field"
}

// e2eePubKeyParseError is the typed failure returned when the selected E2EE
// public key hex does not parse as an uncompressed secp256k1 point. It wraps the
// parse cause; the key is public material, but we surface only the wrapped error.
type e2eePubKeyParseError struct {
	Err error
}

func (e *e2eePubKeyParseError) Error() string {
	return "aci e2ee: model e2ee public key parse: " + e.Err.Error()
}

func (e *e2eePubKeyParseError) Unwrap() error { return e.Err }

// modelE2EEKey returns the keyset's e2ee_public_keys entry whose Algo equals
// e2eeRequestAlgo, parsed to a secp256k1 public key, plus its uncompressed hex
// (the X-Model-Pub-Key value). It fails closed with *e2eeNoModelKeyError when no
// such entry exists and *e2eePubKeyParseError when the hex is malformed.
func modelE2EEKey(verified *VerifiedReport) (*secp256k1.PublicKey, string, error) {
	for _, k := range verified.Keyset.E2EEPublicKeys {
		if k.Algo != e2eeRequestAlgo {
			continue
		}
		raw, err := hex.DecodeString(k.PublicKeyHex)
		if err != nil {
			return nil, "", &e2eePubKeyParseError{Err: err}
		}
		pub, err := secp256k1.ParsePubKey(raw)
		if err != nil {
			return nil, "", &e2eePubKeyParseError{Err: err}
		}
		return pub, k.PublicKeyHex, nil
	}
	return nil, "", &e2eeNoModelKeyError{Algo: e2eeRequestAlgo}
}

// hasSeparator reports whether s contains a byte the AAD grammar reserves as a
// field separator ('|', '\r', '\n'). model and nonce must not, or the gateway
// rejects the request.
func hasSeparator(s string) bool {
	return strings.ContainsAny(s, "|\r\n")
}

// chatRequestAAD builds the chat-message AAD: sel is chatContentString ("-") for
// a string content, or the decimal item index for an array text part.
func chatRequestAAD(algo, model string, m int, sel, nonce string, ts uint64) []byte {
	return []byte("v2|req|algo=" + algo + "|model=" + model +
		"|m=" + strconv.Itoa(m) + "|c=" + sel +
		"|n=" + nonce + "|ts=" + strconv.FormatUint(ts, 10))
}

// completionRequestAAD builds the completion/embedding AAD for the given field
// name (prompt, prompt.{i}, input, input.{i}).
func completionRequestAAD(algo, model, field, nonce string, ts uint64) []byte {
	return []byte("v2|req|algo=" + algo + "|model=" + model +
		"|field=" + field +
		"|n=" + nonce + "|ts=" + strconv.FormatUint(ts, 10))
}

// objectGet returns the value for key in the ordered object and whether it was
// present, preserving the read-only nature of the lookup.
func objectGet(o *Object, key string) (Value, bool) {
	for i := 0; i < o.Len(); i++ {
		if o.KeyAt(i) == key {
			return o.ValueAt(i), true
		}
	}
	return nil, false
}

// objectSet overwrites (or appends) key in the ordered object, preserving
// insertion order (Object.Set semantics).
func objectSet(o *Object, key string, val Value) {
	o.Set(key, val)
}

// sealRequest is the production entry point: it generates a fresh client keypair,
// a random 16-byte hex nonce, and a now()-derived unix-seconds timestamp, then
// delegates to sealRequestWith. now is injected so callers (and tests) control
// the clock; it must return a non-nil time. The returned *sealedRequest carries
// the sealed body, headers, and the ClientPriv/Nonce/Timestamp/Model for 3.3.
func sealRequest(orderedBody *Object, verified *VerifiedReport, now func() time.Time) (*sealedRequest, error) {
	clientPriv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, &e2eeSealError{Stage: "generate client key", Err: err}
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, &e2eeSealError{Stage: "generate nonce", Err: err}
	}
	nonce := hex.EncodeToString(nonceBytes)
	ts := now().Unix()
	if ts < 0 {
		ts = 0
	}
	return sealRequestWith(orderedBody, verified, clientPriv, nonce, uint64(ts))
}

// sealRequestWith is the deterministic core of sealRequest: it seals the selected
// text fields of orderedBody for the verified model's E2EE key using the
// CALLER-SUPPLIED clientPriv, nonce, and ts (so tests can assert the exact AAD,
// headers, and ordered body). It mutates orderedBody IN PLACE then emits
// CompactJSON. It fails closed before any sealing on a missing model key, a
// non-string model field, or an ambiguous model/nonce.
func sealRequestWith(orderedBody *Object, verified *VerifiedReport, clientPriv *secp256k1.PrivateKey, nonce string, ts uint64) (*sealedRequest, error) {
	if orderedBody == nil {
		return nil, &e2eeSealError{Stage: "nil body"}
	}
	if clientPriv == nil {
		return nil, &e2eeSealError{Stage: "nil client key"}
	}

	modelPub, modelPubHex, err := modelE2EEKey(verified)
	if err != nil {
		return nil, err
	}

	model, err := bodyModel(orderedBody)
	if err != nil {
		return nil, err
	}
	if hasSeparator(model) {
		return nil, &e2eeAmbiguityError{Field: "model"}
	}
	if hasSeparator(nonce) {
		return nil, &e2eeAmbiguityError{Field: "nonce"}
	}

	if err := sealBodyFields(orderedBody, modelPub, model, nonce, ts); err != nil {
		return nil, err
	}

	sealedBody, err := CompactJSON(orderedBody)
	if err != nil {
		return nil, &e2eeSealError{Stage: "serialize sealed body", Err: err}
	}

	headers := map[string]string{
		hdrE2EEVersion:   e2eeVersionValue,
		hdrClientPubKey:  hex.EncodeToString(clientPriv.PubKey().SerializeUncompressed()),
		hdrModelPubKey:   modelPubHex,
		hdrE2EENonce:     nonce,
		hdrE2EETimestamp: strconv.FormatUint(ts, 10),
	}

	return &sealedRequest{
		Body:       sealedBody,
		Headers:    headers,
		ClientPriv: clientPriv,
		Nonce:      nonce,
		Timestamp:  ts,
		Model:      model,
	}, nil
}

// bodyModel reads the body's required string "model" field for the AAD. A
// missing or non-string model fails closed with *e2eeModelFieldError.
func bodyModel(body *Object) (string, error) {
	v, ok := objectGet(body, bodyKeyModel)
	if !ok {
		return "", &e2eeModelFieldError{}
	}
	s, ok := v.(String)
	if !ok {
		return "", &e2eeModelFieldError{}
	}
	return string(s), nil
}

// sealBodyFields dispatches to EXACTLY ONE family in the precedence prompt >
// input > messages (mirroring the gateway's decrypt dispatch) and seals the
// selected text fields in place. A body with none of the three is a no-op.
func sealBodyFields(body *Object, modelPub *secp256k1.PublicKey, model, nonce string, ts uint64) error {
	if v, ok := objectGet(body, bodyKeyPrompt); ok {
		return sealCompletionField(body, bodyKeyPrompt, v, modelPub, model, nonce, ts)
	}
	if v, ok := objectGet(body, bodyKeyInput); ok {
		return sealCompletionField(body, bodyKeyInput, v, modelPub, model, nonce, ts)
	}
	if v, ok := objectGet(body, bodyKeyMessages); ok {
		return sealChatMessages(v, modelPub, model, nonce, ts)
	}
	return nil
}

// sealCompletionField seals a completion prompt / embedding input field: a string
// value seals under field={name}; an array seals each STRING element under
// field={name}.{i} (non-string elements untouched). It writes the sealed string
// back into body (string case) or mutates the Array in place (array case).
func sealCompletionField(body *Object, name string, v Value, modelPub *secp256k1.PublicKey, model, nonce string, ts uint64) error {
	switch t := v.(type) {
	case String:
		ctHex, err := seal(modelPub, []byte(string(t)), completionRequestAAD(e2eeRequestAlgo, model, name, nonce, ts))
		if err != nil {
			return err
		}
		objectSet(body, name, String(ctHex))
		return nil
	case Array:
		for i := range t {
			s, ok := t[i].(String)
			if !ok {
				continue // non-string element: untouched
			}
			field := name + "." + strconv.Itoa(i)
			ctHex, err := seal(modelPub, []byte(string(s)), completionRequestAAD(e2eeRequestAlgo, model, field, nonce, ts))
			if err != nil {
				return err
			}
			t[i] = String(ctHex)
		}
		return nil
	default:
		// Neither string nor array (e.g. null): nothing to seal, leave as-is.
		return nil
	}
}

// sealChatMessages seals each message's content: a string content seals under
// c=- and replaces the string; an array content seals ONLY {"type":"text"}
// parts whose "text" is a string, under c={item-index over ALL items}, replacing
// that item's text. Images / non-text items are left untouched and still consume
// an index (so an image at 0 then text at 1 uses c=1). A non-array messages value
// or a non-object message is left untouched.
func sealChatMessages(v Value, modelPub *secp256k1.PublicKey, model, nonce string, ts uint64) error {
	msgs, ok := v.(Array)
	if !ok {
		return nil
	}
	for m := range msgs {
		msg, ok := msgs[m].(*Object)
		if !ok {
			continue
		}
		content, ok := objectGet(msg, bodyKeyContent)
		if !ok {
			continue
		}
		if err := sealMessageContent(msg, content, modelPub, model, m, nonce, ts); err != nil {
			return err
		}
	}
	return nil
}

// sealMessageContent seals one message's content value: a string seals under c=-
// (written back into msg), an array seals each text part under c={index}. Any
// other content shape is left untouched.
func sealMessageContent(msg *Object, content Value, modelPub *secp256k1.PublicKey, model string, m int, nonce string, ts uint64) error {
	switch t := content.(type) {
	case String:
		ctHex, err := seal(modelPub, []byte(string(t)), chatRequestAAD(e2eeRequestAlgo, model, m, chatContentString, nonce, ts))
		if err != nil {
			return err
		}
		objectSet(msg, bodyKeyContent, String(ctHex))
		return nil
	case Array:
		for c := range t {
			item, ok := t[c].(*Object)
			if !ok {
				continue // non-object item: untouched, still consumes index c
			}
			typeVal, ok := objectGet(item, bodyKeyType)
			if !ok {
				continue
			}
			typeStr, ok := typeVal.(String)
			if !ok || string(typeStr) != contentTypeText {
				continue // not a text part (e.g. image_url): untouched
			}
			textVal, ok := objectGet(item, bodyKeyText)
			if !ok {
				continue
			}
			textStr, ok := textVal.(String)
			if !ok {
				continue // text part with non-string text: untouched
			}
			ctHex, err := seal(modelPub, []byte(string(textStr)), chatRequestAAD(e2eeRequestAlgo, model, m, strconv.Itoa(c), nonce, ts))
			if err != nil {
				return err
			}
			objectSet(item, bodyKeyText, String(ctHex))
		}
		return nil
	default:
		return nil
	}
}

// =============================================================================
// Task 3.3: openResponse — response open (content / reasoning_content /
// embedding / text) + 5-minute replay window.
// =============================================================================
//
// The gateway SEALS the response to the CLIENT's public key (the X-Client-Pub-Key
// from 3.2) under a response AAD bound by the request's model/nonce/timestamp;
// openResponse INVERTS that, in place over the ORDERED body (Task 1.3 *Object),
// then re-serializes with CompactJSON so the opened cleartext body's bytes match
// the gateway's serde_json::to_vec exactly (Task 5.2 hashes those bytes for the
// receipt cleartext_hash — blocker #1).
//
// Per-field response AAD (reproduced byte-exact from the Rust reference
// src/aggregator/service/e2ee_crypto.rs so the bytes match the gateway's seal):
//
//   - chat (choices[i]): for each choice's message.content and
//     message.reasoning_content (and the legacy choices[i].text):
//     "v2|resp|algo={algo}|model={model}|id={id}|choice={ci}|field={field}|n={nonce}|ts={ts}"
//   - embedding (data[i].embedding):
//     "v2|resp|algo={algo}|model={model}|id={id}|data={di}|field={field}|n={nonce}|ts={ts}"
//
// {algo} is e2eeRequestAlgo; {model} is the REQUEST model (the param — for AciV2
// the gateway binds ctx.request_model, NOT the response body's model); {id} is the
// response body's "id" string (default ""); {ci}/{di} is the item's explicit
// "index" (u64) when present, else its array position; {nonce}/{ts} are the
// request's nonce/timestamp (the same values 3.2 surfaced).
//
// Dispatch mirrors the gateway: a body with "data" is an embedding response;
// otherwise "choices" is a chat/completion response.
//
// Field reconstruction is byte-critical for the cleartext hash:
//   - content / reasoning_content / text: the gateway sealed the RAW string bytes,
//     so the opened plaintext is set back as String(plaintext) — NOT JSON-parsed.
//   - embedding: the gateway sealed serde_json::to_vec(embedding), so the opened
//     plaintext is PARSED with ParseBodyValue and set back as that Value (it
//     round-trips a float array or base64 string to its original type).
//
// Fail-secure: any parse failure, any open failure, or a replay-window violation
// returns the fail-closed *llm.AttestationError (reason e2ee_failed) with a typed,
// secret-free cause and a nil body — no partial cleartext ever escapes.

// e2eeReplayWindowSeconds is the maximum tolerated skew (in seconds) between now
// and the request timestamp bound into the response AAD. It is a defensive client
// freshness guard: the live path uses a fresh per-request ts, so a larger skew
// means the response (or the caller's clock) is stale and the open is refused.
const e2eeReplayWindowSeconds = 300

// Response body keys read during dispatch, and the per-field names bound into
// each response AAD. They are the OpenAI response shape the gateway seals; pinned
// as constants so a typo cannot silently desync from the gateway. The respField*
// values double as the {field} token in the AAD AND the object key to open, so
// reconstruction and AAD construction can never drift.
const (
	bodyKeyID      = "id"
	bodyKeyChoices = "choices"
	bodyKeyData    = "data"
	bodyKeyMessage = "message"
	bodyKeyIndex   = "index"

	respFieldContent          = "content"
	respFieldReasoningContent = "reasoning_content"
	respFieldText             = "text"
	respFieldEmbedding        = "embedding"
)

// e2eeReplayError is the typed cause wrapped inside the e2ee_failed
// AttestationError when the response timestamp is outside the replay window. It
// carries the skew (in seconds) — a numeric label, never a secret — so the caller
// can inspect how stale the response was without parsing the message.
type e2eeReplayError struct {
	SkewSeconds   int64
	WindowSeconds int64
}

func (e *e2eeReplayError) Error() string {
	return "aci e2ee response: timestamp skew " + strconv.FormatInt(e.SkewSeconds, 10) +
		"s exceeds replay window " + strconv.FormatInt(e.WindowSeconds, 10) + "s"
}

// e2eeBodyShapeError is the typed cause wrapped inside the e2ee_failed
// AttestationError when the sealed response is not a JSON object (the only shape
// the gateway emits). It names the actual top-level kind so the failure is
// inspectable; the kind label is structural, never payload bytes.
type e2eeBodyShapeError struct {
	Kind string
}

func (e *e2eeBodyShapeError) Error() string {
	return "aci e2ee response: body is " + e.Kind + ", want a JSON object"
}

// chatResponseAAD builds the chat-response AAD for one choice field
// (content / reasoning_content / text).
func chatResponseAAD(algo, model, id string, choiceIndex uint64, field, nonce string, ts uint64) []byte {
	return []byte("v2|resp|algo=" + algo + "|model=" + model +
		"|id=" + id + "|choice=" + strconv.FormatUint(choiceIndex, 10) +
		"|field=" + field + "|n=" + nonce + "|ts=" + strconv.FormatUint(ts, 10))
}

// embeddingResponseAAD builds the embedding-response AAD for one data item's
// embedding field.
func embeddingResponseAAD(algo, model, id string, dataIndex uint64, field, nonce string, ts uint64) []byte {
	return []byte("v2|resp|algo=" + algo + "|model=" + model +
		"|id=" + id + "|data=" + strconv.FormatUint(dataIndex, 10) +
		"|field=" + field + "|n=" + nonce + "|ts=" + strconv.FormatUint(ts, 10))
}

// valueAsUint64 returns the u64 value of v when it is an integer JSON number
// (Number / Int / Uint), and false otherwise. The gateway's index fields are u64;
// a missing or non-integer index falls back to the array position at the call
// site, so this never errors — it only reports presence.
func valueAsUint64(v Value) (uint64, bool) {
	switch t := v.(type) {
	case Uint:
		return uint64(t), true
	case Int:
		if t < 0 {
			return 0, false
		}
		return uint64(t), true
	case Number:
		n, err := strconv.ParseUint(string(t), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// itemIndex returns the item's explicit "index" (u64) when present and integral,
// else the array position pos. It mirrors the gateway: choices[i]["index"] /
// data[i]["index"] when set, otherwise the positional index. pos is a slice index
// (always >= 0 from range); the guard makes that bound explicit so the unsigned
// conversion cannot wrap.
func itemIndex(item *Object, pos int) uint64 {
	if v, ok := objectGet(item, bodyKeyIndex); ok {
		if n, ok := valueAsUint64(v); ok {
			return n
		}
	}
	if pos < 0 {
		return 0
	}
	return uint64(pos)
}

// openResponse opens a gateway-sealed E2EE v2 response with the client private
// key and returns the cleartext body (CompactJSON of the order-preserving
// *Object). model/nonce/ts are the REQUEST values bound into every response AAD
// (surfaced by 3.2's sealRequest); now is injected so callers control the replay
// clock. Every failure — a bad parse, a non-object body, a stale timestamp, or
// any field open failure — returns the fail-closed *llm.AttestationError (reason
// e2ee_failed) with a typed, secret-free cause and a nil body.
func openResponse(sealedResp []byte, clientPriv *secp256k1.PrivateKey, model, nonce string, ts uint64, now time.Time) ([]byte, error) {
	if clientPriv == nil {
		return nil, attestErr(reasonE2EEFailed, &e2eeOpenError{Stage: "nil client key"})
	}

	// Replay window: refuse a response whose bound timestamp is more than the
	// window away from now, in either clock direction. A ts above MaxInt64 is not
	// a real unix-seconds value (it cannot fit a signed clock) — fail closed rather
	// than wrap on the int64 conversion.
	if ts > math.MaxInt64 {
		return nil, attestErr(reasonE2EEFailed, &e2eeReplayError{SkewSeconds: math.MaxInt64, WindowSeconds: e2eeReplayWindowSeconds})
	}
	skew := now.Unix() - int64(ts)
	if skew < 0 {
		skew = -skew
	}
	if skew > e2eeReplayWindowSeconds {
		return nil, attestErr(reasonE2EEFailed, &e2eeReplayError{SkewSeconds: skew, WindowSeconds: e2eeReplayWindowSeconds})
	}

	parsed, err := ParseBodyValue(sealedResp)
	if err != nil {
		return nil, attestErr(reasonE2EEFailed, err)
	}
	body, ok := parsed.(*Object)
	if !ok {
		return nil, attestErr(reasonE2EEFailed, &e2eeBodyShapeError{Kind: valueKind(parsed)})
	}

	responseID := responseBodyID(body)

	if v, ok := objectGet(body, bodyKeyData); ok {
		if err := openEmbeddingData(v, clientPriv, model, responseID, nonce, ts); err != nil {
			return nil, attestErr(reasonE2EEFailed, err)
		}
	} else if v, ok := objectGet(body, bodyKeyChoices); ok {
		if err := openChatChoices(v, clientPriv, model, responseID, nonce, ts); err != nil {
			return nil, attestErr(reasonE2EEFailed, err)
		}
	}

	out, err := CompactJSON(body)
	if err != nil {
		return nil, attestErr(reasonE2EEFailed, err)
	}
	return out, nil
}

// responseBodyID returns the body's "id" string (the AAD {id}), defaulting to ""
// when absent or not a string — matching the gateway's id.unwrap_or_default().
func responseBodyID(body *Object) string {
	v, ok := objectGet(body, bodyKeyID)
	if !ok {
		return ""
	}
	s, ok := v.(String)
	if !ok {
		return ""
	}
	return string(s)
}

// openChatChoices opens each choice's sealed content / reasoning_content / text
// fields in place. A non-array choices value or a non-object choice is left
// untouched; only present STRING fields are sealed blobs and get opened.
func openChatChoices(v Value, clientPriv *secp256k1.PrivateKey, model, id, nonce string, ts uint64) error {
	choices, ok := v.(Array)
	if !ok {
		return nil
	}
	for i := range choices {
		choice, ok := choices[i].(*Object)
		if !ok {
			continue
		}
		ci := itemIndex(choice, i)

		// Legacy completion: a choices[i].text string is a sealed blob.
		if err := openStringField(choice, respFieldText, clientPriv,
			chatResponseAAD(e2eeRequestAlgo, model, id, ci, respFieldText, nonce, ts)); err != nil {
			return err
		}

		msg, ok := objectGet(choice, bodyKeyMessage)
		if !ok {
			continue
		}
		msgObj, ok := msg.(*Object)
		if !ok {
			continue
		}
		if err := openStringField(msgObj, respFieldContent, clientPriv,
			chatResponseAAD(e2eeRequestAlgo, model, id, ci, respFieldContent, nonce, ts)); err != nil {
			return err
		}
		if err := openStringField(msgObj, respFieldReasoningContent, clientPriv,
			chatResponseAAD(e2eeRequestAlgo, model, id, ci, respFieldReasoningContent, nonce, ts)); err != nil {
			return err
		}
	}
	return nil
}

// openEmbeddingData opens each data item's sealed embedding field in place. A
// non-array data value or a non-object item is left untouched.
func openEmbeddingData(v Value, clientPriv *secp256k1.PrivateKey, model, id, nonce string, ts uint64) error {
	data, ok := v.(Array)
	if !ok {
		return nil
	}
	for i := range data {
		item, ok := data[i].(*Object)
		if !ok {
			continue
		}
		di := itemIndex(item, i)
		if err := openEmbeddingField(item, clientPriv,
			embeddingResponseAAD(e2eeRequestAlgo, model, id, di, respFieldEmbedding, nonce, ts)); err != nil {
			return err
		}
	}
	return nil
}

// openStringField opens a sealed STRING field (content / reasoning_content /
// text) in place: it decrypts the hex blob under aad and sets the field back to
// the RAW recovered string (no JSON parse). A missing field or a non-string value
// is a no-op (the field is not in the E2EE flow). An open failure propagates.
func openStringField(obj *Object, field string, clientPriv *secp256k1.PrivateKey, aad []byte) error {
	v, ok := objectGet(obj, field)
	if !ok {
		return nil
	}
	s, ok := v.(String)
	if !ok {
		return nil // e.g. null content: not a sealed blob.
	}
	plaintext, err := open(clientPriv, string(s), aad)
	if err != nil {
		return err
	}
	objectSet(obj, field, String(string(plaintext)))
	return nil
}

// openEmbeddingField opens a sealed embedding field in place: it decrypts the hex
// blob under aad, then PARSES the recovered plaintext with ParseBodyValue and sets
// the field to that Value (the gateway sealed serde_json::to_vec(embedding), so
// the plaintext is JSON — a float array or a base64 string). A missing field or a
// non-string value is a no-op. An open or parse failure propagates.
func openEmbeddingField(obj *Object, clientPriv *secp256k1.PrivateKey, aad []byte) error {
	v, ok := objectGet(obj, respFieldEmbedding)
	if !ok {
		return nil
	}
	s, ok := v.(String)
	if !ok {
		return nil
	}
	plaintext, err := open(clientPriv, string(s), aad)
	if err != nil {
		return err
	}
	parsed, err := ParseBodyValue(plaintext)
	if err != nil {
		return err
	}
	objectSet(obj, respFieldEmbedding, parsed)
	return nil
}

// valueKind returns a short structural label for a Value, used only in the
// non-object body-shape error. It never renders payload bytes.
func valueKind(v Value) string {
	switch v.(type) {
	case String:
		return "string"
	case Int, Uint, Number:
		return "number"
	case Float:
		return "float"
	case Bool:
		return "bool"
	case Null:
		return "null"
	case Array:
		return "array"
	case *Object:
		return "object"
	default:
		return "unknown"
	}
}
