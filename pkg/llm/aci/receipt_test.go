package aci

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// receiptCanonicalVector mirrors testdata/receipt_canonical_vector.json: the
// Rust-emitted authoritative golden vector. receipt is the on-wire receipt JSON
// (kept raw so the test parses an honest wire receipt); canonical_hex / sha256
// are the byte-exact arbiter produced by the Rust reference.
type receiptCanonicalVector struct {
	Receipt      json.RawMessage `json:"receipt"`
	CanonicalHex string          `json:"canonical_hex"`
	Sha256       string          `json:"sha256"`
}

// loadReceiptVector loads the authoritative Rust-emitted canonical vector.
func loadReceiptVector(t *testing.T) receiptCanonicalVector {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "receipt_canonical_vector.json"))
	if err != nil {
		t.Fatalf("read receipt_canonical_vector.json: %v", err)
	}
	var v receiptCanonicalVector
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal receipt vector: %v", err)
	}
	return v
}

// requireReceiptInvalid asserts err is the fail-closed *llm.AttestationError
// with reason receipt_invalid.
func requireReceiptInvalid(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("verifyReceiptSig() error = nil, want *llm.AttestationError(receipt_invalid)")
	}
	var attErr *llm.AttestationError
	if !errors.As(err, &attErr) {
		t.Fatalf("verifyReceiptSig() error = %T (%v), want *llm.AttestationError", err, err)
	}
	if attErr.Reason != reasonReceiptInvalid {
		t.Errorf("AttestationError.Reason = %q, want %q", attErr.Reason, reasonReceiptInvalid)
	}
}

// TestCanonicalReceiptMatchesRustVector is the projection arbiter: the Go
// canonicalReceipt of the parsed wire receipt must equal the Rust-emitted
// canonical bytes byte-for-byte (and the sha256 of those bytes).
func TestCanonicalReceiptMatchesRustVector(t *testing.T) {
	t.Parallel()
	v := loadReceiptVector(t)

	r, err := ParseReceipt(v.Receipt)
	if err != nil {
		t.Fatalf("ParseReceipt() error = %v, want nil", err)
	}
	got, err := canonicalReceipt(r)
	if err != nil {
		t.Fatalf("canonicalReceipt() error = %v, want nil", err)
	}
	gotHex := hex.EncodeToString(got)
	if gotHex != v.CanonicalHex {
		t.Fatalf("canonicalReceipt() hex mismatch:\n got = %s\nwant = %s", gotHex, v.CanonicalHex)
	}
	sum := sha256.Sum256(got)
	gotSha := "sha256:" + hex.EncodeToString(sum[:])
	if gotSha != v.Sha256 {
		t.Fatalf("canonicalReceipt() sha256 = %s, want %s", gotSha, v.Sha256)
	}
}

// TestCanonicalReceiptChatIDNullVsPresent proves chat_id absent (JSON null) and
// chat_id present produce DIFFERENT canonical bytes (the Option<String> arm).
func TestCanonicalReceiptChatIDNullVsPresent(t *testing.T) {
	t.Parallel()
	v := loadReceiptVector(t)

	present, err := ParseReceipt(v.Receipt)
	if err != nil {
		t.Fatalf("ParseReceipt(present) error = %v", err)
	}
	canonPresent, err := canonicalReceipt(present)
	if err != nil {
		t.Fatalf("canonicalReceipt(present) error = %v", err)
	}

	// Drop chat_id from the wire JSON -> absent -> canonical null.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(v.Receipt, &raw); err != nil {
		t.Fatalf("unmarshal receipt to map: %v", err)
	}
	delete(raw, "chat_id")
	absentJSON, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal absent receipt: %v", err)
	}
	absent, err := ParseReceipt(absentJSON)
	if err != nil {
		t.Fatalf("ParseReceipt(absent) error = %v", err)
	}
	if absent.ChatID != nil {
		t.Fatalf("ChatID = %v, want nil when absent", *absent.ChatID)
	}
	canonAbsent, err := canonicalReceipt(absent)
	if err != nil {
		t.Fatalf("canonicalReceipt(absent) error = %v", err)
	}

	if hex.EncodeToString(canonPresent) == hex.EncodeToString(canonAbsent) {
		t.Fatalf("canonical bytes for chat_id present and absent must differ")
	}
}

// TestCanonicalReceiptEventFlatten proves the event flatten merges seq+type with
// the fields object members in one flat, key-sorted object (per the vector).
func TestCanonicalReceiptEventFlatten(t *testing.T) {
	t.Parallel()
	v := loadReceiptVector(t)
	r, err := ParseReceipt(v.Receipt)
	if err != nil {
		t.Fatalf("ParseReceipt() error = %v", err)
	}
	got, err := canonicalReceipt(r)
	if err != nil {
		t.Fatalf("canonicalReceipt() error = %v", err)
	}
	canon := string(got)
	// The first event flattens seq+type+body_hash; in canonical (sorted) form the
	// object opens with body_hash, then seq, then type.
	wantFragment := `[{"body_hash":"sha256:` +
		`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
		`"seq":0,"type":"request.received"}`
	if !strings.Contains(canon, wantFragment) {
		t.Fatalf("canonical event flatten fragment not found.\n got = %s\nwant fragment = %s", canon, wantFragment)
	}
}

// receiptSigningKeyset builds a synthetic VerifiedReport carrying a single
// receipt-signing key (key_id, algo, public_key hex) so verifyReceiptSig can
// look the key up.
func receiptSigningKeyset(keyID, algo, pubHex string) VerifiedReport {
	return VerifiedReport{
		Keyset: Keyset{
			ReceiptSigningKeys: []KeyEntry{
				{KeyID: keyID, Algo: algo, PublicKeyHex: pubHex},
			},
		},
	}
}

// TestVerifyReceiptSigEd25519RoundTrip signs the canonical bytes with ed25519
// and verifies; tampering the canonical bytes or the sig flips to invalid.
func TestVerifyReceiptSigEd25519RoundTrip(t *testing.T) {
	t.Parallel()
	v := loadReceiptVector(t)
	r, err := ParseReceipt(v.Receipt)
	if err != nil {
		t.Fatalf("ParseReceipt() error = %v", err)
	}
	canonical, err := canonicalReceipt(r)
	if err != nil {
		t.Fatalf("canonicalReceipt() error = %v", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	sig := ed25519.Sign(priv, canonical)
	const keyID = "rcpt-ed25519"
	vr := receiptSigningKeyset(keyID, algoEd25519, hex.EncodeToString(pub))

	// Happy path: valid signature verifies.
	if err := verifyReceiptSig(vr, keyID, canonical, hex.EncodeToString(sig)); err != nil {
		t.Fatalf("verifyReceiptSig(ed25519, valid) error = %v, want nil", err)
	}

	// Tamper the canonical bytes -> invalid.
	badCanonical := append([]byte(nil), canonical...)
	badCanonical[0] ^= 0xff
	requireReceiptInvalid(t, verifyReceiptSig(vr, keyID, badCanonical, hex.EncodeToString(sig)))

	// Tamper a sig byte -> invalid.
	badSig := append([]byte(nil), sig...)
	badSig[0] ^= 0xff
	requireReceiptInvalid(t, verifyReceiptSig(vr, keyID, canonical, hex.EncodeToString(badSig)))
}

// TestVerifyReceiptSigSecp256k1RoundTrip signs sha256(canonical) recoverably and
// builds the Dstack r‖s‖v (v-last, recid 0-3) signature; verify recovers and
// matches. Tampering any of canonical / sig / key flips to invalid.
func TestVerifyReceiptSigSecp256k1RoundTrip(t *testing.T) {
	t.Parallel()
	v := loadReceiptVector(t)
	r, err := ParseReceipt(v.Receipt)
	if err != nil {
		t.Fatalf("ParseReceipt() error = %v", err)
	}
	canonical, err := canonicalReceipt(r)
	if err != nil {
		t.Fatalf("canonicalReceipt() error = %v", err)
	}

	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().SerializeUncompressed())
	const keyID = "rcpt-secp256k1"
	vr := receiptSigningKeyset(keyID, algoSecp256k1, pubHex)

	dstackSig := dstackRSV(t, priv, canonical)

	if err := verifyReceiptSig(vr, keyID, canonical, hex.EncodeToString(dstackSig)); err != nil {
		t.Fatalf("verifyReceiptSig(secp256k1, valid) error = %v, want nil", err)
	}

	// Tamper canonical -> recovers a different key -> invalid.
	badCanonical := append([]byte(nil), canonical...)
	badCanonical[0] ^= 0xff
	requireReceiptInvalid(t, verifyReceiptSig(vr, keyID, badCanonical, hex.EncodeToString(dstackSig)))

	// Tamper a sig (r) byte -> invalid.
	badSig := append([]byte(nil), dstackSig...)
	badSig[0] ^= 0xff
	requireReceiptInvalid(t, verifyReceiptSig(vr, keyID, canonical, hex.EncodeToString(badSig)))

	// Tamper the key (a different keypair's pubkey) -> invalid.
	other, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey(other): %v", err)
	}
	otherVR := receiptSigningKeyset(keyID, algoSecp256k1, hex.EncodeToString(other.PubKey().SerializeUncompressed()))
	requireReceiptInvalid(t, verifyReceiptSig(otherVR, keyID, canonical, hex.EncodeToString(dstackSig)))
}

// TestVerifyReceiptSigSecp256k1VOffsetVariant proves a v in {27..30} recovers the
// same key as the raw recid 0-3 (the Ethereum-offset normalization).
func TestVerifyReceiptSigSecp256k1VOffsetVariant(t *testing.T) {
	t.Parallel()
	v := loadReceiptVector(t)
	r, err := ParseReceipt(v.Receipt)
	if err != nil {
		t.Fatalf("ParseReceipt() error = %v", err)
	}
	canonical, err := canonicalReceipt(r)
	if err != nil {
		t.Fatalf("canonicalReceipt() error = %v", err)
	}

	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey: %v", err)
	}
	pubHex := hex.EncodeToString(priv.PubKey().SerializeUncompressed())
	const keyID = "rcpt-secp256k1"
	vr := receiptSigningKeyset(keyID, algoSecp256k1, pubHex)

	dstackSig := dstackRSV(t, priv, canonical) // v = recid (0-3)
	offset := append([]byte(nil), dstackSig...)
	offset[64] += 27 // shift to the Ethereum-offset form (27 + recid)

	if err := verifyReceiptSig(vr, keyID, canonical, hex.EncodeToString(offset)); err != nil {
		t.Fatalf("verifyReceiptSig(secp256k1, v-offset) error = %v, want nil", err)
	}
}

// TestVerifyReceiptSigKeyNotFound proves an unknown key_id fails closed.
func TestVerifyReceiptSigKeyNotFound(t *testing.T) {
	t.Parallel()
	v := loadReceiptVector(t)
	r, err := ParseReceipt(v.Receipt)
	if err != nil {
		t.Fatalf("ParseReceipt() error = %v", err)
	}
	canonical, err := canonicalReceipt(r)
	if err != nil {
		t.Fatalf("canonicalReceipt() error = %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	sig := ed25519.Sign(priv, canonical)
	vr := receiptSigningKeyset("present-key", algoEd25519, hex.EncodeToString(pub))

	requireReceiptInvalid(t, verifyReceiptSig(vr, "missing-key", canonical, hex.EncodeToString(sig)))
}

// TestVerifyReceiptSigAlgoAndLengthFailures proves algo mismatch and wrong-length
// signatures fail closed.
func TestVerifyReceiptSigAlgoAndLengthFailures(t *testing.T) {
	t.Parallel()
	v := loadReceiptVector(t)
	r, err := ParseReceipt(v.Receipt)
	if err != nil {
		t.Fatalf("ParseReceipt() error = %v", err)
	}
	canonical, err := canonicalReceipt(r)
	if err != nil {
		t.Fatalf("canonicalReceipt() error = %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	ed25519Sig := ed25519.Sign(priv, canonical)
	const keyID = "k"

	tests := []struct {
		name   string
		algo   string
		pubHex string
		sigHex string
	}{
		{
			name:   "ed25519 wrong-length signature",
			algo:   algoEd25519,
			pubHex: hex.EncodeToString(pub),
			sigHex: hex.EncodeToString(ed25519Sig[:63]),
		},
		{
			name:   "ed25519 wrong-length public key",
			algo:   algoEd25519,
			pubHex: hex.EncodeToString(pub[:31]),
			sigHex: hex.EncodeToString(ed25519Sig),
		},
		{
			name:   "secp256k1 wrong-length signature (64B JOSE form rejected)",
			algo:   algoSecp256k1,
			pubHex: hex.EncodeToString(pub), // shape irrelevant, length check fires first
			sigHex: hex.EncodeToString(make([]byte, 64)),
		},
		{
			name:   "unsupported algo",
			algo:   "rsa-pkcs1",
			pubHex: hex.EncodeToString(pub),
			sigHex: hex.EncodeToString(ed25519Sig),
		},
		{
			name:   "signature not hex",
			algo:   algoEd25519,
			pubHex: hex.EncodeToString(pub),
			sigHex: "nothex!!",
		},
		{
			name:   "public key not hex",
			algo:   algoEd25519,
			pubHex: "nothex!!",
			sigHex: hex.EncodeToString(ed25519Sig),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			vr := receiptSigningKeyset(keyID, tt.algo, tt.pubHex)
			requireReceiptInvalid(t, verifyReceiptSig(vr, keyID, canonical, tt.sigHex))
		})
	}
}

// TestParseReceiptErrors proves malformed JSON yields a typed *receiptParseError
// (no bare stdlib error escapes the exported API).
func TestParseReceiptErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []byte
	}{
		{name: "empty", in: []byte("")},
		{name: "not json", in: []byte("not json")},
		{name: "truncated", in: []byte(`{"api_version":`)},
		{name: "wrong type for served_at", in: []byte(`{"served_at":"x"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseReceipt(tt.in)
			if err == nil {
				t.Fatalf("ParseReceipt(%q) error = nil, want error", tt.in)
			}
			var pe *receiptParseError
			if !errors.As(err, &pe) {
				t.Fatalf("ParseReceipt(%q) error = %T (%v), want *receiptParseError", tt.in, err, err)
			}
		})
	}
}

// dstackRSV signs sha256(message) recoverably and reorders decred's v-first
// compact [27+recid]‖r‖s into the Dstack v-last r‖s‖v (v = recid 0-3) layout.
func dstackRSV(t *testing.T, priv *secp256k1.PrivateKey, message []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(message)
	compact := ecdsa.SignCompact(priv, digest[:], false) // [27+recid][r][s]
	if len(compact) != 65 {
		t.Fatalf("SignCompact len = %d, want 65", len(compact))
	}
	out := make([]byte, 65)
	copy(out[:32], compact[1:33])    // r
	copy(out[32:64], compact[33:65]) // s
	out[64] = compact[0] - 27        // v = recid (0-3)
	return out
}
