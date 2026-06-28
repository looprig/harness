package aci

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// =============================================================================
// Task 3.2: sealRequest — request field sealing + ordered body + headers
// =============================================================================

// e2eeAlgo is the algorithm string the gateway uses to pick the model E2EE key
// and to bind every per-field AAD. The tests assert the exact AAD bytes against
// it, so it is pinned here too rather than referenced from production (a
// production typo would then be caught by the AAD-string assertions).
const testE2EEAlgo = "secp256k1-aes-256-gcm-hkdf-sha256"

// newModelKeypair returns a fresh secp256k1 keypair plus its uncompressed-hex
// public key. The tests hold the PRIVATE key so they can open() each sealed
// field and assert the recovered plaintext + AAD.
func newModelKeypair(t *testing.T) (*secp256k1.PrivateKey, string) {
	t.Helper()
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate model key: %v", err)
	}
	return priv, hex.EncodeToString(priv.PubKey().SerializeUncompressed())
}

// verifiedWithE2EEKeys builds a *VerifiedReport whose keyset carries the given
// E2EE key entries (and nothing else load-bearing for sealing). It is the seam
// sealRequest reads the model pubkey from.
func verifiedWithE2EEKeys(entries ...KeyEntry) *VerifiedReport {
	return &VerifiedReport{
		Keyset: Keyset{E2EEPublicKeys: entries},
	}
}

// mustParseBody parses a body JSON string into the ordered *Object the sealer
// mutates in place.
func mustParseBody(t *testing.T, s string) *Object {
	t.Helper()
	v, err := ParseBodyValue([]byte(s))
	if err != nil {
		t.Fatalf("parse body %q: %v", s, err)
	}
	obj, ok := v.(*Object)
	if !ok {
		t.Fatalf("body is %T, want *Object", v)
	}
	return obj
}

// chatAAD builds the EXACT expected chat-message AAD string for the given model,
// message index, content selector (sel is "-" for a string content or the
// decimal item index for an array text part), nonce, and timestamp.
func chatAAD(model string, m int, sel, nonce string, ts uint64) string {
	return "v2|req|algo=" + testE2EEAlgo + "|model=" + model +
		"|m=" + strconv.Itoa(m) + "|c=" + sel +
		"|n=" + nonce + "|ts=" + strconv.FormatUint(ts, 10)
}

// completionAAD builds the EXACT expected completion/embedding AAD string for the
// given model, field name (prompt / prompt.{i} / input / input.{i}), nonce, ts.
func completionAAD(model, field, nonce string, ts uint64) string {
	return "v2|req|algo=" + testE2EEAlgo + "|model=" + model +
		"|field=" + field +
		"|n=" + nonce + "|ts=" + strconv.FormatUint(ts, 10)
}

// findString walks an ordered *Object by a path of keys/indices and returns the
// terminal String value's hex (the sealed ciphertext). Path elements are either
// a string (object key) or an int (array index).
func sealedHexAt(t *testing.T, body *Object, path ...any) string {
	t.Helper()
	v := walkPath(t, body, path...)
	s, ok := v.(String)
	if !ok {
		t.Fatalf("value at %v is %T, want String", path, v)
	}
	return string(s)
}

func walkPath(t *testing.T, root Value, path ...any) Value {
	t.Helper()
	cur := root
	for _, step := range path {
		switch key := step.(type) {
		case string:
			obj, ok := cur.(*Object)
			if !ok {
				t.Fatalf("path step %q: current is %T, want *Object", key, cur)
			}
			found := false
			for i := 0; i < obj.Len(); i++ {
				if obj.KeyAt(i) == key {
					cur = obj.ValueAt(i)
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("path step %q not found", key)
			}
		case int:
			arr, ok := cur.(Array)
			if !ok {
				t.Fatalf("path step %d: current is %T, want Array", key, cur)
			}
			if key < 0 || key >= len(arr) {
				t.Fatalf("path step %d out of range (len %d)", key, len(arr))
			}
			cur = arr[key]
		default:
			t.Fatalf("unsupported path step %T", step)
		}
	}
	return cur
}

// fixedClientPriv is a deterministic client private key for the header/AAD
// tests (sealRequestWith injects it so the headers are reproducible).
func fixedClientPriv(t *testing.T) *secp256k1.PrivateKey {
	t.Helper()
	return mustPrivFromHex(t, "1111111111111111111111111111111111111111111111111111111111111111")
}

// TestSealRequestChatStringContent seals a chat body whose message content is a
// plain string. It must seal under AAD c=- and recover the plaintext on open.
func TestSealRequestChatStringContent(t *testing.T) {
	t.Parallel()
	modelPriv, modelPubHex := newModelKeypair(t)
	verified := verifiedWithE2EEKeys(KeyEntry{KeyID: "k1", Algo: testE2EEAlgo, PublicKeyHex: modelPubHex})

	const model = "z-ai/glm-5.2"
	body := mustParseBody(t, `{"model":"`+model+`","messages":[{"role":"user","content":"hello there"}]}`)

	const nonce = "0011223344556677889900aabbccddee"
	const ts = uint64(1750000000)
	clientPriv := fixedClientPriv(t)

	sr, err := sealRequestWith(body, verified, clientPriv, nonce, ts)
	if err != nil {
		t.Fatalf("sealRequestWith: %v", err)
	}

	// The sealed body must round-trip-parse and carry hex at messages[0].content.
	out := mustParseBody(t, string(sr.Body))
	ctHex := sealedHexAt(t, out, "messages", 0, "content")

	wantAAD := chatAAD(model, 0, "-", nonce, ts)
	got, err := open(modelPriv, ctHex, []byte(wantAAD))
	if err != nil {
		t.Fatalf("open sealed content under expected AAD: %v", err)
	}
	if string(got) != "hello there" {
		t.Fatalf("recovered %q, want %q", got, "hello there")
	}

	// A wrong AAD (wrong content selector) must FAIL to open.
	if _, err := open(modelPriv, ctHex, []byte(chatAAD(model, 0, "0", nonce, ts))); err == nil {
		t.Fatalf("open succeeded under wrong AAD (c=0), want failure")
	}
	// Wrong message index must also fail.
	if _, err := open(modelPriv, ctHex, []byte(chatAAD(model, 1, "-", nonce, ts))); err == nil {
		t.Fatalf("open succeeded under wrong AAD (m=1), want failure")
	}

	if sr.Model != model {
		t.Fatalf("sr.Model = %q, want %q", sr.Model, model)
	}
}

// TestSealRequestChatArrayContent seals an array content with a text part at
// index 1 (an image at index 0). The text's AAD must use c=1, the image must be
// left byte-identical, and field order must be preserved.
func TestSealRequestChatArrayContent(t *testing.T) {
	t.Parallel()
	modelPriv, modelPubHex := newModelKeypair(t)
	verified := verifiedWithE2EEKeys(KeyEntry{KeyID: "k1", Algo: testE2EEAlgo, PublicKeyHex: modelPubHex})

	const model = "z-ai/glm-5.2"
	bodyJSON := `{"model":"` + model + `","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://example.com/x.png"}},` +
		`{"type":"text","text":"describe this"}]}]}`
	body := mustParseBody(t, bodyJSON)

	const nonce = "abcdef0011223344556677889900aabb"
	const ts = uint64(1750000001)

	sr, err := sealRequestWith(body, verified, fixedClientPriv(t), nonce, ts)
	if err != nil {
		t.Fatalf("sealRequestWith: %v", err)
	}
	out := mustParseBody(t, string(sr.Body))

	// The image item (index 0) must be untouched (still has url, no ciphertext).
	imgURL := walkPath(t, out, "messages", 0, "content", 0, "image_url", "url")
	if s, ok := imgURL.(String); !ok || string(s) != "https://example.com/x.png" {
		t.Fatalf("image url mutated: got %#v", imgURL)
	}
	// The image's "type" stays "image_url".
	if s, ok := walkPath(t, out, "messages", 0, "content", 0, "type").(String); !ok || string(s) != "image_url" {
		t.Fatalf("image type mutated: %#v", s)
	}

	// The text item (index 1) text must be sealed under c=1.
	ctHex := sealedHexAt(t, out, "messages", 0, "content", 1, "text")
	wantAAD := chatAAD(model, 0, "1", nonce, ts)
	got, err := open(modelPriv, ctHex, []byte(wantAAD))
	if err != nil {
		t.Fatalf("open array text under c=1 AAD: %v", err)
	}
	if string(got) != "describe this" {
		t.Fatalf("recovered %q, want %q", got, "describe this")
	}
	// c=0 (the image index) must NOT open the text.
	if _, err := open(modelPriv, ctHex, []byte(chatAAD(model, 0, "0", nonce, ts))); err == nil {
		t.Fatalf("open succeeded under wrong AAD (c=0), want failure")
	}
}

// TestSealRequestCompletionPrompt covers the completion prompt field in both
// string and array shapes (only string elements seal; field names prompt /
// prompt.{i}).
func TestSealRequestCompletionPrompt(t *testing.T) {
	t.Parallel()
	modelPriv, modelPubHex := newModelKeypair(t)
	verified := verifiedWithE2EEKeys(KeyEntry{KeyID: "k1", Algo: testE2EEAlgo, PublicKeyHex: modelPubHex})
	const model = "openai/gpt-x"
	const nonce = "00ff00ff00ff00ff00ff00ff00ff00ff"
	const ts = uint64(1750000002)

	t.Run("string prompt", func(t *testing.T) {
		t.Parallel()
		body := mustParseBody(t, `{"model":"`+model+`","prompt":"complete me"}`)
		sr, err := sealRequestWith(body, verified, fixedClientPriv(t), nonce, ts)
		if err != nil {
			t.Fatalf("sealRequestWith: %v", err)
		}
		out := mustParseBody(t, string(sr.Body))
		ctHex := sealedHexAt(t, out, "prompt")
		got, err := open(modelPriv, ctHex, []byte(completionAAD(model, "prompt", nonce, ts)))
		if err != nil {
			t.Fatalf("open prompt: %v", err)
		}
		if string(got) != "complete me" {
			t.Fatalf("recovered %q, want %q", got, "complete me")
		}
	})

	t.Run("array prompt", func(t *testing.T) {
		t.Parallel()
		// index 1 is a non-string (number) and must be left untouched.
		body := mustParseBody(t, `{"model":"`+model+`","prompt":["a",42,"b"]}`)
		sr, err := sealRequestWith(body, verified, fixedClientPriv(t), nonce, ts)
		if err != nil {
			t.Fatalf("sealRequestWith: %v", err)
		}
		out := mustParseBody(t, string(sr.Body))

		ct0 := sealedHexAt(t, out, "prompt", 0)
		got0, err := open(modelPriv, ct0, []byte(completionAAD(model, "prompt.0", nonce, ts)))
		if err != nil {
			t.Fatalf("open prompt.0: %v", err)
		}
		if string(got0) != "a" {
			t.Fatalf("prompt.0 recovered %q, want %q", got0, "a")
		}
		ct2 := sealedHexAt(t, out, "prompt", 2)
		got2, err := open(modelPriv, ct2, []byte(completionAAD(model, "prompt.2", nonce, ts)))
		if err != nil {
			t.Fatalf("open prompt.2: %v", err)
		}
		if string(got2) != "b" {
			t.Fatalf("prompt.2 recovered %q, want %q", got2, "b")
		}
		// index 1 (the number) is untouched.
		if n := walkPath(t, out, "prompt", 1); n != Number("42") {
			t.Fatalf("prompt.1 mutated: %#v", n)
		}
	})
}

// TestSealRequestEmbeddingInput covers the embedding input field (string +
// array, field names input / input.{i}). It also asserts prompt takes precedence
// over input is NOT exercised here (no prompt present), so input is selected.
func TestSealRequestEmbeddingInput(t *testing.T) {
	t.Parallel()
	modelPriv, modelPubHex := newModelKeypair(t)
	verified := verifiedWithE2EEKeys(KeyEntry{KeyID: "k1", Algo: testE2EEAlgo, PublicKeyHex: modelPubHex})
	const model = "text-embedding-3"
	const nonce = "12341234123412341234123412341234"
	const ts = uint64(1750000003)

	t.Run("string input", func(t *testing.T) {
		t.Parallel()
		body := mustParseBody(t, `{"model":"`+model+`","input":"embed me"}`)
		sr, err := sealRequestWith(body, verified, fixedClientPriv(t), nonce, ts)
		if err != nil {
			t.Fatalf("sealRequestWith: %v", err)
		}
		out := mustParseBody(t, string(sr.Body))
		ctHex := sealedHexAt(t, out, "input")
		got, err := open(modelPriv, ctHex, []byte(completionAAD(model, "input", nonce, ts)))
		if err != nil {
			t.Fatalf("open input: %v", err)
		}
		if string(got) != "embed me" {
			t.Fatalf("recovered %q, want %q", got, "embed me")
		}
	})

	t.Run("array input", func(t *testing.T) {
		t.Parallel()
		body := mustParseBody(t, `{"model":"`+model+`","input":["one","two"]}`)
		sr, err := sealRequestWith(body, verified, fixedClientPriv(t), nonce, ts)
		if err != nil {
			t.Fatalf("sealRequestWith: %v", err)
		}
		out := mustParseBody(t, string(sr.Body))
		ct0 := sealedHexAt(t, out, "input", 0)
		got0, err := open(modelPriv, ct0, []byte(completionAAD(model, "input.0", nonce, ts)))
		if err != nil {
			t.Fatalf("open input.0: %v", err)
		}
		if string(got0) != "one" {
			t.Fatalf("input.0 recovered %q, want %q", got0, "one")
		}
		ct1 := sealedHexAt(t, out, "input", 1)
		got1, err := open(modelPriv, ct1, []byte(completionAAD(model, "input.1", nonce, ts)))
		if err != nil {
			t.Fatalf("open input.1: %v", err)
		}
		if string(got1) != "two" {
			t.Fatalf("input.1 recovered %q, want %q", got1, "two")
		}
	})
}

// TestSealRequestPrecedence asserts that when BOTH prompt and messages are
// present, prompt wins (completion dispatch) and messages is left untouched.
func TestSealRequestPrecedence(t *testing.T) {
	t.Parallel()
	modelPriv, modelPubHex := newModelKeypair(t)
	verified := verifiedWithE2EEKeys(KeyEntry{KeyID: "k1", Algo: testE2EEAlgo, PublicKeyHex: modelPubHex})
	const model = "m"
	const nonce = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const ts = uint64(1750000004)

	body := mustParseBody(t, `{"model":"`+model+`","prompt":"p","messages":[{"role":"user","content":"c"}]}`)
	sr, err := sealRequestWith(body, verified, fixedClientPriv(t), nonce, ts)
	if err != nil {
		t.Fatalf("sealRequestWith: %v", err)
	}
	out := mustParseBody(t, string(sr.Body))

	// prompt sealed under field=prompt.
	ctHex := sealedHexAt(t, out, "prompt")
	if _, err := open(modelPriv, ctHex, []byte(completionAAD(model, "prompt", nonce, ts))); err != nil {
		t.Fatalf("open prompt: %v", err)
	}
	// messages[0].content must be the untouched plaintext "c".
	if s, ok := walkPath(t, out, "messages", 0, "content").(String); !ok || string(s) != "c" {
		t.Fatalf("messages content mutated despite prompt precedence: %#v", s)
	}
}

// TestSealRequestHeaders asserts the exact header set and values.
func TestSealRequestHeaders(t *testing.T) {
	t.Parallel()
	_, modelPubHex := newModelKeypair(t)
	verified := verifiedWithE2EEKeys(KeyEntry{KeyID: "k1", Algo: testE2EEAlgo, PublicKeyHex: modelPubHex})
	const nonce = "0123456789abcdef0123456789abcdef"
	const ts = uint64(1750000005)
	clientPriv := fixedClientPriv(t)

	body := mustParseBody(t, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	sr, err := sealRequestWith(body, verified, clientPriv, nonce, ts)
	if err != nil {
		t.Fatalf("sealRequestWith: %v", err)
	}

	if got := sr.Headers["X-E2EE-Version"]; got != "2" {
		t.Fatalf("X-E2EE-Version = %q, want 2", got)
	}
	if got := sr.Headers["X-Model-Pub-Key"]; got != modelPubHex {
		t.Fatalf("X-Model-Pub-Key = %q, want %q", got, modelPubHex)
	}
	wantClientPub := hex.EncodeToString(clientPriv.PubKey().SerializeUncompressed())
	if got := sr.Headers["X-Client-Pub-Key"]; got != wantClientPub {
		t.Fatalf("X-Client-Pub-Key = %q, want %q", got, wantClientPub)
	}
	// X-Client-Pub-Key must be valid uncompressed hex (parses back).
	if _, err := mustPubFromHexErr(sr.Headers["X-Client-Pub-Key"]); err != nil {
		t.Fatalf("X-Client-Pub-Key not valid uncompressed pubkey: %v", err)
	}
	if got := sr.Headers["X-E2EE-Nonce"]; got != nonce {
		t.Fatalf("X-E2EE-Nonce = %q, want %q", got, nonce)
	}
	if got := sr.Headers["X-E2EE-Timestamp"]; got != strconv.FormatUint(ts, 10) {
		t.Fatalf("X-E2EE-Timestamp = %q, want %q", got, strconv.FormatUint(ts, 10))
	}
	// surfaced for 3.3
	if sr.ClientPriv != clientPriv {
		t.Fatalf("ClientPriv not surfaced")
	}
	if sr.Nonce != nonce {
		t.Fatalf("Nonce = %q, want %q", sr.Nonce, nonce)
	}
	if sr.Timestamp != ts {
		t.Fatalf("Timestamp = %d, want %d", sr.Timestamp, ts)
	}
}

// mustPubFromHexErr parses an uncompressed pubkey hex, returning the error.
func mustPubFromHexErr(s string) (*secp256k1.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return secp256k1.ParsePubKey(b)
}

// TestSealRequestOrderPreserved confirms CompactJSON of the sealed body keeps
// insertion order (non-alphabetical keys stay put) and non-string elements are
// byte-identical to the input.
func TestSealRequestOrderPreserved(t *testing.T) {
	t.Parallel()
	_, modelPubHex := newModelKeypair(t)
	verified := verifiedWithE2EEKeys(KeyEntry{KeyID: "k1", Algo: testE2EEAlgo, PublicKeyHex: modelPubHex})
	const nonce = "deadbeefdeadbeefdeadbeefdeadbeef"
	const ts = uint64(1750000006)

	// Keys deliberately NOT alphabetical: "z_last","model","messages","a_first".
	body := mustParseBody(t, `{"z_last":1,"model":"m","messages":[{"role":"user","content":"hi"}],"a_first":true}`)
	sr, err := sealRequestWith(body, verified, fixedClientPriv(t), nonce, ts)
	if err != nil {
		t.Fatalf("sealRequestWith: %v", err)
	}
	out := mustParseBody(t, string(sr.Body))
	wantOrder := []string{"z_last", "model", "messages", "a_first"}
	if out.Len() != len(wantOrder) {
		t.Fatalf("top-level key count = %d, want %d", out.Len(), len(wantOrder))
	}
	for i, k := range wantOrder {
		if out.KeyAt(i) != k {
			t.Fatalf("key[%d] = %q, want %q (order not preserved)", i, out.KeyAt(i), k)
		}
	}
	// The serialized bytes must START with the first (non-alphabetical) key.
	if !strings.HasPrefix(string(sr.Body), `{"z_last":1,`) {
		t.Fatalf("sealed body does not preserve leading key order: %s", sr.Body)
	}
}

// TestSealRequestNoOp confirms a body with no messages/prompt/input is returned
// unchanged (round-trips byte-equal) with headers still set.
func TestSealRequestNoOp(t *testing.T) {
	t.Parallel()
	_, modelPubHex := newModelKeypair(t)
	verified := verifiedWithE2EEKeys(KeyEntry{KeyID: "k1", Algo: testE2EEAlgo, PublicKeyHex: modelPubHex})
	const nonce = "cccccccccccccccccccccccccccccccc"
	const ts = uint64(1750000007)

	const in = `{"model":"m","temperature":0.7}`
	body := mustParseBody(t, in)
	sr, err := sealRequestWith(body, verified, fixedClientPriv(t), nonce, ts)
	if err != nil {
		t.Fatalf("sealRequestWith: %v", err)
	}
	// Re-serialize the original to compare against the same compact form.
	wantBody, err := CompactJSON(body)
	if err != nil {
		t.Fatalf("CompactJSON: %v", err)
	}
	if !bytes.Equal(sr.Body, wantBody) {
		t.Fatalf("no-op body changed:\n got  %s\n want %s", sr.Body, wantBody)
	}
	if sr.Headers["X-E2EE-Version"] != "2" {
		t.Fatalf("headers not set on no-op body")
	}
}

// TestSealRequestMissingE2EEKey asserts a typed error when the keyset has no
// E2EE key with the supported algo.
func TestSealRequestMissingE2EEKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries []KeyEntry
	}{
		{name: "empty keyset", entries: nil},
		{name: "wrong algo only", entries: []KeyEntry{{KeyID: "k", Algo: "x25519-something", PublicKeyHex: "00"}}},
	}
	const nonce = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const ts = uint64(1750000008)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			verified := verifiedWithE2EEKeys(tt.entries...)
			body := mustParseBody(t, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
			sr, err := sealRequestWith(body, verified, fixedClientPriv(t), nonce, ts)
			if err == nil {
				t.Fatalf("sealRequestWith succeeded, want typed error; sr=%v", sr)
			}
			var me *e2eeNoModelKeyError
			if !errors.As(err, &me) {
				t.Fatalf("error %T (%v) is not *e2eeNoModelKeyError", err, err)
			}
		})
	}
}

// TestSealRequestAmbiguityGuard asserts a model or nonce carrying a separator
// byte (| \r \n) is rejected with a typed error (the gateway rejects them).
func TestSealRequestAmbiguityGuard(t *testing.T) {
	t.Parallel()
	_, modelPubHex := newModelKeypair(t)
	verified := verifiedWithE2EEKeys(KeyEntry{KeyID: "k1", Algo: testE2EEAlgo, PublicKeyHex: modelPubHex})
	const ts = uint64(1750000009)

	tests := []struct {
		name  string
		model string
		nonce string
	}{
		{name: "model has pipe", model: "a|b", nonce: "00112233445566778899aabbccddeeff"},
		{name: "model has CR", model: "a\rb", nonce: "00112233445566778899aabbccddeeff"},
		{name: "model has LF", model: "a\nb", nonce: "00112233445566778899aabbccddeeff"},
		{name: "nonce has pipe", model: "ok", nonce: "00|11"},
		{name: "nonce has CR", model: "ok", nonce: "00\r11"},
		{name: "nonce has LF", model: "ok", nonce: "00\n11"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := NewObject()
			body.Set("model", String(tt.model))
			msg := NewObject()
			msg.Set("content", String("x"))
			body.Set("messages", Array{msg})
			sr, err := sealRequestWith(body, verified, fixedClientPriv(t), tt.nonce, ts)
			if err == nil {
				t.Fatalf("sealRequestWith succeeded with ambiguous input, want error; sr=%v", sr)
			}
			var ae *e2eeAmbiguityError
			if !errors.As(err, &ae) {
				t.Fatalf("error %T (%v) is not *e2eeAmbiguityError", err, err)
			}
		})
	}
}

// TestSealRequestProductionWrapper exercises sealRequest (the production
// wrapper): it must generate a fresh client key + random nonce + now()-seconds
// ts, seal the field, and surface everything for 3.3.
func TestSealRequestProductionWrapper(t *testing.T) {
	t.Parallel()
	modelPriv, modelPubHex := newModelKeypair(t)
	verified := verifiedWithE2EEKeys(KeyEntry{KeyID: "k1", Algo: testE2EEAlgo, PublicKeyHex: modelPubHex})
	const model = "z-ai/glm-5.2"
	body := mustParseBody(t, `{"model":"`+model+`","messages":[{"role":"user","content":"prod path"}]}`)

	fixed := time.Unix(1750001234, 0)
	sr, err := sealRequest(body, verified, func() time.Time { return fixed })
	if err != nil {
		t.Fatalf("sealRequest: %v", err)
	}
	if sr.Timestamp != uint64(fixed.Unix()) {
		t.Fatalf("Timestamp = %d, want %d", sr.Timestamp, fixed.Unix())
	}
	if sr.Headers["X-E2EE-Timestamp"] != strconv.FormatUint(uint64(fixed.Unix()), 10) {
		t.Fatalf("X-E2EE-Timestamp header = %q", sr.Headers["X-E2EE-Timestamp"])
	}
	if sr.ClientPriv == nil {
		t.Fatalf("ClientPriv nil, want a generated key")
	}
	if len(sr.Nonce) == 0 || strings.ContainsAny(sr.Nonce, "|\r\n") {
		t.Fatalf("Nonce %q is empty or ambiguous", sr.Nonce)
	}
	// The sealed field opens under the AAD built from the surfaced nonce/ts.
	out := mustParseBody(t, string(sr.Body))
	ctHex := sealedHexAt(t, out, "messages", 0, "content")
	wantAAD := chatAAD(model, 0, "-", sr.Nonce, sr.Timestamp)
	got, err := open(modelPriv, ctHex, []byte(wantAAD))
	if err != nil {
		t.Fatalf("open prod-sealed content: %v", err)
	}
	if string(got) != "prod path" {
		t.Fatalf("recovered %q, want %q", got, "prod path")
	}

	// Two production calls draw different nonces (randomness).
	sr2, err := sealRequest(body, verified, func() time.Time { return fixed })
	if err != nil {
		t.Fatalf("sealRequest 2: %v", err)
	}
	if sr.Nonce == sr2.Nonce {
		t.Fatalf("two production nonces identical: %q", sr.Nonce)
	}
}
