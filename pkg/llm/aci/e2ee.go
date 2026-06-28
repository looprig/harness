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
