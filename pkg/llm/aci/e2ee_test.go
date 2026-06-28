package aci

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// e2eeVectorFile is the committed deterministic cross-implementation fixture.
// It fixes the ephemeral private key and the nonce so seal output is
// byte-reproducible; production seal uses crypto/rand for both.
const e2eeVectorFile = "testdata/e2ee_vectors.json"

// e2eeVectorSet mirrors the top-level shape of e2ee_vectors.json. Only the
// vector field is load-bearing; the rest documents provenance.
type e2eeVectorSet struct {
	Description string     `json:"description"`
	Vector      e2eeVector `json:"vector"`
}

// e2eeVector is the single deterministic golden case. Every byte field is
// lowercase hex except plaintext and aad, which are raw UTF-8 strings.
type e2eeVector struct {
	ModelPriv     string `json:"model_priv"`
	EphPriv       string `json:"eph_priv"`
	ModelPub      string `json:"model_pub"`
	Plaintext     string `json:"plaintext"`
	AAD           string `json:"aad"`
	Nonce         string `json:"nonce"`
	SharedX       string `json:"shared_x"`
	CiphertextHex string `json:"ciphertext_hex"`
}

// loadE2EEVector reads and decodes the committed deterministic fixture.
func loadE2EEVector(t *testing.T) e2eeVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(e2eeVectorFile))
	if err != nil {
		t.Fatalf("read %s: %v", e2eeVectorFile, err)
	}
	var set e2eeVectorSet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatalf("decode %s: %v", e2eeVectorFile, err)
	}
	return set.Vector
}

// mustDecodeHex decodes lowercase hex or fails the test.
func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}

// mustPrivFromHex builds a fixed secp256k1 private key from a 32-byte hex scalar.
func mustPrivFromHex(t *testing.T, s string) *secp256k1.PrivateKey {
	t.Helper()
	b := mustDecodeHex(t, s)
	if len(b) != 32 {
		t.Fatalf("priv key %q is %d bytes, want 32", s, len(b))
	}
	return secp256k1.PrivKeyFromBytes(b)
}

// mustPubFromHex parses an uncompressed (65-byte) secp256k1 public key.
func mustPubFromHex(t *testing.T, s string) *secp256k1.PublicKey {
	t.Helper()
	b := mustDecodeHex(t, s)
	pub, err := secp256k1.ParsePubKey(b)
	if err != nil {
		t.Fatalf("parse pub %q: %v", s, err)
	}
	return pub
}

// TestE2EESealWithVector is the blocker-#1 arbiter: with the FIXED ephemeral
// key and nonce from the deterministic Python vector, sealWith must reproduce
// the reference ciphertext_hex byte-for-byte, and open must recover plaintext.
func TestE2EESealWithVector(t *testing.T) {
	t.Parallel()
	v := loadE2EEVector(t)

	ephPriv := mustPrivFromHex(t, v.EphPriv)
	nonce := mustDecodeHex(t, v.Nonce)
	modelPriv := mustPrivFromHex(t, v.ModelPriv)
	modelPub := mustPubFromHex(t, v.ModelPub)
	plaintext := []byte(v.Plaintext)
	aad := []byte(v.AAD)

	// The model public key in the vector must equal model_priv.PubKey().
	if got := hex.EncodeToString(modelPriv.PubKey().SerializeUncompressed()); got != v.ModelPub {
		t.Fatalf("model_pub mismatch:\n got  %s\n want %s", got, v.ModelPub)
	}

	gotHex, err := sealWith(ephPriv, nonce, modelPub, plaintext, aad)
	if err != nil {
		t.Fatalf("sealWith: %v", err)
	}
	if gotHex != v.CiphertextHex {
		t.Fatalf("sealWith ciphertext mismatch:\n got  %s\n want %s", gotHex, v.CiphertextHex)
	}

	got, err := open(modelPriv, v.CiphertextHex, aad)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("open plaintext mismatch:\n got  %q\n want %q", got, plaintext)
	}
}

// TestE2EESharedXVector asserts the ECDH shared X-coordinate matches the
// reference, from both directions (sender ephemeral and recipient private).
func TestE2EESharedXVector(t *testing.T) {
	t.Parallel()
	v := loadE2EEVector(t)

	ephPriv := mustPrivFromHex(t, v.EphPriv)
	modelPriv := mustPrivFromHex(t, v.ModelPriv)
	modelPub := mustPubFromHex(t, v.ModelPub)
	ephPub := ephPriv.PubKey()

	fromEph := hex.EncodeToString(ecdhSharedX(ephPriv, modelPub))
	fromModel := hex.EncodeToString(ecdhSharedX(modelPriv, ephPub))

	if fromEph != v.SharedX {
		t.Fatalf("shared X (eph->modelPub) mismatch:\n got  %s\n want %s", fromEph, v.SharedX)
	}
	if fromModel != v.SharedX {
		t.Fatalf("shared X (model->ephPub) mismatch:\n got  %s\n want %s", fromModel, v.SharedX)
	}
}

// TestE2EEECDHSymmetry confirms ECDH is symmetric for arbitrary keypairs:
// shared(a_priv, b_pub) == shared(b_priv, a_pub).
func TestE2EEECDHSymmetry(t *testing.T) {
	t.Parallel()
	aPriv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("gen a: %v", err)
	}
	bPriv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("gen b: %v", err)
	}
	ab := ecdhSharedX(aPriv, bPriv.PubKey())
	ba := ecdhSharedX(bPriv, aPriv.PubKey())
	if !bytes.Equal(ab, ba) {
		t.Fatalf("ECDH not symmetric:\n ab %x\n ba %x", ab, ba)
	}
	if len(ab) != 32 {
		t.Fatalf("shared X is %d bytes, want 32", len(ab))
	}
}

// TestE2EERoundTrip exercises the random-ephemeral seal against open over a
// table of plaintext/aad sizes including empty.
func TestE2EERoundTrip(t *testing.T) {
	t.Parallel()
	recipientPriv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("gen recipient: %v", err)
	}
	recipientPub := recipientPriv.PubKey()

	tests := []struct {
		name      string
		plaintext []byte
		aad       []byte
	}{
		{name: "typical", plaintext: []byte("hello confidential world"), aad: []byte("v2|req|m=0")},
		{name: "empty plaintext", plaintext: []byte{}, aad: []byte("aad-only")},
		{name: "empty aad", plaintext: []byte("payload"), aad: []byte{}},
		{name: "empty both", plaintext: []byte{}, aad: []byte{}},
		{name: "nil both", plaintext: nil, aad: nil},
		{name: "large plaintext", plaintext: bytes.Repeat([]byte("A"), 8192), aad: []byte("big")},
		{name: "binary plaintext", plaintext: []byte{0x00, 0xff, 0x10, 0x80, 0x7f}, aad: []byte{0x01, 0x02}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blob, err := seal(recipientPub, tt.plaintext, tt.aad)
			if err != nil {
				t.Fatalf("seal: %v", err)
			}
			got, err := open(recipientPriv, blob, tt.aad)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if !bytes.Equal(got, tt.plaintext) {
				t.Fatalf("round-trip mismatch:\n got  %q\n want %q", got, tt.plaintext)
			}
		})
	}
}

// TestE2EESealRandomness confirms seal draws a fresh ephemeral+nonce each call:
// two seals of the same plaintext/aad must differ (and both still open).
func TestE2EESealRandomness(t *testing.T) {
	t.Parallel()
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	pub := priv.PubKey()
	pt := []byte("same plaintext")
	aad := []byte("same aad")

	a, err := seal(pub, pt, aad)
	if err != nil {
		t.Fatalf("seal a: %v", err)
	}
	b, err := seal(pub, pt, aad)
	if err != nil {
		t.Fatalf("seal b: %v", err)
	}
	if a == b {
		t.Fatalf("two seals produced identical blobs; ephemeral/nonce not random")
	}
	for _, blob := range []string{a, b} {
		if _, err := open(priv, blob, aad); err != nil {
			t.Fatalf("open seal %q: %v", blob, err)
		}
	}
}

// TestE2EEOpenHexPrefix confirms open tolerates an optional 0x prefix.
func TestE2EEOpenHexPrefix(t *testing.T) {
	t.Parallel()
	v := loadE2EEVector(t)
	modelPriv := mustPrivFromHex(t, v.ModelPriv)
	aad := []byte(v.AAD)

	got, err := open(modelPriv, "0x"+v.CiphertextHex, aad)
	if err != nil {
		t.Fatalf("open with 0x prefix: %v", err)
	}
	if !bytes.Equal(got, []byte(v.Plaintext)) {
		t.Fatalf("plaintext mismatch:\n got  %q\n want %q", got, v.Plaintext)
	}
}

// TestE2EEOpenTamper covers every tamper / malformation path: each must return
// a typed *e2eeOpenError (open never returns a plaintext on tamper).
func TestE2EEOpenTamper(t *testing.T) {
	t.Parallel()
	v := loadE2EEVector(t)
	modelPriv := mustPrivFromHex(t, v.ModelPriv)
	aad := []byte(v.AAD)
	blob := mustDecodeHex(t, v.CiphertextHex)

	// Helper to flip the last bit of byte at index i within a fresh copy.
	flip := func(i int) string {
		c := append([]byte(nil), blob...)
		c[i] ^= 0x01
		return hex.EncodeToString(c)
	}

	tests := []struct {
		name string
		hex  string
		aad  []byte
	}{
		{name: "tampered tag (last byte)", hex: flip(len(blob) - 1), aad: aad},
		{name: "tampered ciphertext byte", hex: flip(77), aad: aad},
		{name: "tampered nonce byte", hex: flip(65), aad: aad},
		{name: "wrong aad", hex: v.CiphertextHex, aad: []byte("v2|req|algo=secp256k1-aes-256-gcm-hkdf-sha256|m=0|c=X")},
		{name: "empty aad mismatch", hex: v.CiphertextHex, aad: []byte{}},
		{name: "truncated below minimum", hex: hex.EncodeToString(blob[:92]), aad: aad},
		{name: "empty blob", hex: "", aad: aad},
		{name: "odd-length hex", hex: v.CiphertextHex[:len(v.CiphertextHex)-1], aad: aad},
		{name: "non-hex chars", hex: "zzzz", aad: aad},
		{name: "corrupt ephemeral pubkey", hex: flip(1), aad: aad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := open(modelPriv, tt.hex, tt.aad)
			if err == nil {
				t.Fatalf("open(%s) succeeded, want error; got plaintext %q", tt.name, got)
			}
			if got != nil {
				t.Fatalf("open(%s) returned non-nil plaintext %q with error", tt.name, got)
			}
			var oe *e2eeOpenError
			if !errors.As(err, &oe) {
				t.Fatalf("open(%s) error %T (%v) is not *e2eeOpenError", tt.name, err, err)
			}
		})
	}
}

// TestE2EESealWithRejectsBadNonce confirms sealWith validates the nonce length
// (the 12-byte GCM standard nonce) with a typed error.
func TestE2EESealWithRejectsBadNonce(t *testing.T) {
	t.Parallel()
	v := loadE2EEVector(t)
	ephPriv := mustPrivFromHex(t, v.EphPriv)
	modelPub := mustPubFromHex(t, v.ModelPub)

	tests := []struct {
		name  string
		nonce []byte
	}{
		{name: "too short", nonce: bytes.Repeat([]byte{0x00}, 11)},
		{name: "too long", nonce: bytes.Repeat([]byte{0x00}, 13)},
		{name: "empty", nonce: []byte{}},
		{name: "nil", nonce: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sealWith(ephPriv, tt.nonce, modelPub, []byte("x"), []byte("a"))
			if err == nil {
				t.Fatalf("sealWith with %s nonce succeeded, want error; got %q", tt.name, got)
			}
			if got != "" {
				t.Fatalf("sealWith returned non-empty blob %q with error", got)
			}
			var se *e2eeSealError
			if !errors.As(err, &se) {
				t.Fatalf("sealWith error %T (%v) is not *e2eeSealError", err, err)
			}
		})
	}
}
