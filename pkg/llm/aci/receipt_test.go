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

// ---------------------------------------------------------------------------
// Task 4.2 — VerifyReceipt mandatory-checks tests.
//
// These build SYNTHETIC flattened receipts (no live fixture yet, Task 6.1) whose
// identity matches a synthetic *VerifiedReport, whose request.received/
// response.returned/upstream.verified events carry the correct hashes/fields, and
// which are then ed25519-signed over canonicalReceipt so the signature verifies.
// Each table case mutates exactly one binding to trigger exactly one reason.
// ---------------------------------------------------------------------------

// synthReceiptParams are the knobs the synthetic-receipt builder threads onto the
// wire receipt. The zero value is not meaningful; validReceiptParams seeds a
// consistent happy-path set that individual tests then mutate by one field. The
// *Present flags drop an event entirely (to exercise the "absent event" misses).
type synthReceiptParams struct {
	apiVersion           string
	workloadID           string
	workloadKeysetDigest string
	endpoint             string
	method               string

	reqBodyCompact   []byte // compact serde_json of the cleartext request body
	bodyHashOverride string // when non-empty, used verbatim instead of hashing reqBodyCompact
	includeReqEvent  bool
	includeRespEvent bool
	cleartextHash    string // verbatim cleartext_hash field on response.returned
	wireHash         string // verbatim wire_hash field on response.returned
	includeUpstream  bool
	upstreamResult   string // "verified" / "failed"
	upstreamProvider string // the provider TYPE (design-doc "vendor")
	upstreamModelID  string
}

// validReceiptParams returns a consistent happy-path parameter set whose hashes
// are computed from the given request/response/wire bytes and whose identity
// matches vr. reqBody is the COMPACT serde_json bytes of the cleartext request
// body (what ReceiptExpect.ReqBody carries — the caller computes CompactJSON),
// so the body_hash event binds to Sha256HexBytes(reqBody) directly. Tests
// clone-and-mutate one field to exercise a single miss.
func validReceiptParams(t *testing.T, vr VerifiedReport, reqBody, respCleartext, respWire []byte, provider, modelID string) synthReceiptParams {
	t.Helper()
	bodyHash, err := Sha256HexBytes(reqBody)
	if err != nil {
		t.Fatalf("Sha256HexBytes(reqBody): %v", err)
	}
	cleartextHash, err := Sha256HexBytes(respCleartext)
	if err != nil {
		t.Fatalf("Sha256HexBytes(cleartext): %v", err)
	}
	wireHash, err := Sha256HexBytes(respWire)
	if err != nil {
		t.Fatalf("Sha256HexBytes(wire): %v", err)
	}
	return synthReceiptParams{
		apiVersion:           SupportedAPIVersion,
		workloadID:           vr.WorkloadID,
		workloadKeysetDigest: vr.WorkloadKeysetDigest,
		endpoint:             "/v1/chat/completions",
		method:               "POST",
		reqBodyCompact:       reqBody,
		bodyHashOverride:     bodyHash,
		includeReqEvent:      true,
		includeRespEvent:     true,
		cleartextHash:        cleartextHash,
		wireHash:             wireHash,
		includeUpstream:      true,
		upstreamResult:       "verified",
		upstreamProvider:     provider,
		upstreamModelID:      modelID,
	}
}

// synthReqBody is a fixed compact serde_json request body (already CompactJSON'd,
// matching how the caller — Task 5.2 — supplies ReceiptExpect.ReqBody). Its
// Sha256HexBytes is the request.received.body_hash.
var synthReqBody = []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

// buildSignedReceipt assembles the flattened wire receipt from p, then ed25519-
// signs canonicalReceipt and injects the hex signature value, returning the wire
// JSON. keyID/priv must correspond to a receipt-signing key registered in the
// caller's *VerifiedReport so VerifyReceipt's signature step passes.
func buildSignedReceipt(t *testing.T, p synthReceiptParams, keyID string, priv ed25519.PrivateKey) []byte {
	t.Helper()

	events := make([]json.RawMessage, 0, 3)
	seq := uint64(0)
	if p.includeReqEvent {
		bh := p.bodyHashOverride
		if bh == "" {
			var err error
			bh, err = Sha256HexBytes(p.reqBodyCompact)
			if err != nil {
				t.Fatalf("Sha256HexBytes(reqBodyCompact): %v", err)
			}
		}
		events = append(events, flatEvent(t, seq, "request.received", map[string]string{"body_hash": bh}))
		seq++
	}
	if p.includeRespEvent {
		fields := map[string]string{"cleartext_hash": p.cleartextHash}
		if p.wireHash != "" {
			fields["wire_hash"] = p.wireHash
		}
		events = append(events, flatEvent(t, seq, "response.returned", fields))
		seq++
	}
	if p.includeUpstream {
		events = append(events, flatEvent(t, seq, "upstream.verified", map[string]string{
			"result":   p.upstreamResult,
			"provider": p.upstreamProvider,
			"model_id": p.upstreamModelID,
		}))
		seq++
	}

	eventLog, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal event_log: %v", err)
	}

	// Build the receipt with a placeholder signature value, parse + canonicalize,
	// sign, then re-inject the hex signature value.
	receiptObj := map[string]json.RawMessage{
		"api_version":            mustJSON(t, p.apiVersion),
		"receipt_id":             mustJSON(t, "rcpt-4dot2-synth"),
		"chat_id":                json.RawMessage("null"),
		"workload_id":            mustJSON(t, p.workloadID),
		"workload_keyset_digest": mustJSON(t, p.workloadKeysetDigest),
		"endpoint":               mustJSON(t, p.endpoint),
		"method":                 mustJSON(t, p.method),
		"served_at":              json.RawMessage("1700000500"),
		"event_log":              eventLog,
		"signature":              mustJSON(t, ReceiptSignature{Algo: algoEd25519, KeyID: keyID, ValueHex: ""}),
	}
	unsigned, err := json.Marshal(receiptObj)
	if err != nil {
		t.Fatalf("marshal unsigned receipt: %v", err)
	}

	r, err := ParseReceipt(unsigned)
	if err != nil {
		t.Fatalf("ParseReceipt(unsigned): %v", err)
	}
	canonical, err := canonicalReceipt(r)
	if err != nil {
		t.Fatalf("canonicalReceipt: %v", err)
	}
	sig := ed25519.Sign(priv, canonical)

	receiptObj["signature"] = mustJSON(t, ReceiptSignature{
		Algo:     algoEd25519,
		KeyID:    keyID,
		ValueHex: hex.EncodeToString(sig),
	})
	signed, err := json.Marshal(receiptObj)
	if err != nil {
		t.Fatalf("marshal signed receipt: %v", err)
	}
	return signed
}

// flatEvent marshals one flattened event object: {seq, type, ...fields}. The
// extra fields are string-valued (the receipt's hash/result/provider/model_id
// fields all are), which keeps the helper small and explicit.
func flatEvent(t *testing.T, seq uint64, eventType string, fields map[string]string) json.RawMessage {
	t.Helper()
	obj := map[string]json.RawMessage{
		"seq":  mustJSON(t, seq),
		"type": mustJSON(t, eventType),
	}
	for k, v := range fields {
		obj[k] = mustJSON(t, v)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal event %q: %v", eventType, err)
	}
	return out
}

// mustJSON marshals v to JSON or fails the test; a thin helper to keep the
// builders readable.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	return out
}

// synthVerifiedReport builds a *VerifiedReport with the given identity and a
// single ed25519 receipt-signing key, returning it alongside the private key and
// key id so the builder can sign matching receipts.
func synthVerifiedReport(t *testing.T) (VerifiedReport, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	const keyID = "rcpt-4dot2"
	vr := VerifiedReport{
		WorkloadID:           "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		WorkloadKeysetDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		Keyset: Keyset{
			ReceiptSigningKeys: []KeyEntry{
				{KeyID: keyID, Algo: algoEd25519, PublicKeyHex: hex.EncodeToString(pub)},
			},
		},
	}
	return vr, priv, keyID
}

// requireUpstreamUnverified asserts err is the fail-closed *llm.AttestationError
// with reason upstream_unverified.
func requireUpstreamUnverified(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("VerifyReceipt() error = nil, want *llm.AttestationError(upstream_unverified)")
	}
	var attErr *llm.AttestationError
	if !errors.As(err, &attErr) {
		t.Fatalf("VerifyReceipt() error = %T (%v), want *llm.AttestationError", err, err)
	}
	if attErr.Reason != reasonUpstreamUnverified {
		t.Errorf("AttestationError.Reason = %q, want %q", attErr.Reason, reasonUpstreamUnverified)
	}
}

const (
	synthProvider = "openai"
	synthModelID  = "gpt-4o"
)

// synthExpect returns the ReceiptExpect that matches a happy-path synthetic
// receipt built from validReceiptParams. respWire may be nil to exercise the
// "wire_hash not checked" path.
func synthExpect(reqCompact, respCleartext, respWire []byte) ReceiptExpect {
	return ReceiptExpect{
		Endpoint:          "/v1/chat/completions",
		Method:            "POST",
		Vendor:            synthProvider,
		ModelID:           synthModelID,
		ReqBody:           reqCompact,
		RespBodyCleartext: respCleartext,
		RespWireBytes:     respWire,
	}
}

// TestVerifyReceiptHappyPath proves a well-formed, correctly-signed receipt with
// matching identity, hashes, and a verified upstream event passes.
func TestVerifyReceiptHappyPath(t *testing.T) {
	t.Parallel()
	vr, priv, keyID := synthVerifiedReport(t)
	respCleartext := []byte(`{"choices":[{"message":{"content":"hi"}}]}`)
	respWire := []byte("wire-bytes-opaque")
	p := validReceiptParams(t, vr, synthReqBody, respCleartext, respWire, synthProvider, synthModelID)
	receiptJSON := buildSignedReceipt(t, p, keyID, priv)

	expect := synthExpect(p.reqBodyCompact, respCleartext, respWire)
	if err := VerifyReceipt(receiptJSON, &vr, expect); err != nil {
		t.Fatalf("VerifyReceipt() error = %v, want nil", err)
	}
}

// TestVerifyReceiptWrappedUnwraps proves the {"receipt": {...}} envelope verifies
// identically to the bare {...} form.
func TestVerifyReceiptWrappedUnwraps(t *testing.T) {
	t.Parallel()
	vr, priv, keyID := synthVerifiedReport(t)
	respCleartext := []byte("resp-cleartext")
	respWire := []byte("resp-wire")
	p := validReceiptParams(t, vr, synthReqBody, respCleartext, respWire, synthProvider, synthModelID)
	receiptJSON := buildSignedReceipt(t, p, keyID, priv)

	wrapped, err := json.Marshal(map[string]json.RawMessage{"receipt": receiptJSON})
	if err != nil {
		t.Fatalf("marshal wrapped: %v", err)
	}
	expect := synthExpect(p.reqBodyCompact, respCleartext, respWire)
	if err := VerifyReceipt(wrapped, &vr, expect); err != nil {
		t.Fatalf("VerifyReceipt(wrapped) error = %v, want nil", err)
	}
}

// TestVerifyReceiptWireHashSkippedWhenNil proves wire_hash is NOT checked when
// RespWireBytes is nil: a receipt whose wire_hash would mismatch still passes.
func TestVerifyReceiptWireHashSkippedWhenNil(t *testing.T) {
	t.Parallel()
	vr, priv, keyID := synthVerifiedReport(t)
	respCleartext := []byte("resp-cleartext")
	p := validReceiptParams(t, vr, synthReqBody, respCleartext, []byte("ignored-wire"), synthProvider, synthModelID)
	// Force a wire_hash that would NOT match any RespWireBytes, then skip the check.
	p.wireHash = "sha256:" + strings.Repeat("9", 64)
	receiptJSON := buildSignedReceipt(t, p, keyID, priv)

	expect := synthExpect(p.reqBodyCompact, respCleartext, nil) // RespWireBytes nil -> skip wire_hash
	if err := VerifyReceipt(receiptJSON, &vr, expect); err != nil {
		t.Fatalf("VerifyReceipt(wire skip) error = %v, want nil", err)
	}
}

// TestVerifyReceiptVendorEmptySkipsProvider proves an empty expect.Vendor skips
// the provider check: a receipt whose provider differs still passes on model_id
// + result alone.
func TestVerifyReceiptVendorEmptySkipsProvider(t *testing.T) {
	t.Parallel()
	vr, priv, keyID := synthVerifiedReport(t)
	respCleartext := []byte("resp-cleartext")
	respWire := []byte("resp-wire")
	p := validReceiptParams(t, vr, synthReqBody, respCleartext, respWire, "some-other-provider", synthModelID)
	receiptJSON := buildSignedReceipt(t, p, keyID, priv)

	expect := synthExpect(p.reqBodyCompact, respCleartext, respWire)
	expect.Vendor = "" // skip provider check
	if err := VerifyReceipt(receiptJSON, &vr, expect); err != nil {
		t.Fatalf("VerifyReceipt(vendor empty) error = %v, want nil", err)
	}
}

// TestVerifyReceiptInvalidChecks proves each identity / hash / signature miss
// fails closed with reason receipt_invalid.
func TestVerifyReceiptInvalidChecks(t *testing.T) {
	t.Parallel()
	respCleartext := []byte(`{"resp":"body"}`)
	respWire := []byte("wire-opaque-bytes")

	tests := []struct {
		name string
		// mutate adjusts the happy-path params before signing; expectMut adjusts the
		// caller's ReceiptExpect; tamperSigned mutates the signed wire bytes.
		mutate       func(p *synthReceiptParams)
		expectMut    func(e *ReceiptExpect)
		tamperSigned func(t *testing.T, b []byte) []byte
	}{
		{
			name:   "wrong workload_id",
			mutate: func(p *synthReceiptParams) { p.workloadID = "sha256:" + strings.Repeat("f", 64) },
		},
		{
			name:   "wrong workload_keyset_digest",
			mutate: func(p *synthReceiptParams) { p.workloadKeysetDigest = "sha256:" + strings.Repeat("e", 64) },
		},
		{
			name:   "wrong api_version",
			mutate: func(p *synthReceiptParams) { p.apiVersion = "aci/2" },
		},
		{
			name:   "wrong endpoint",
			mutate: func(p *synthReceiptParams) { p.endpoint = "/v1/embeddings" },
		},
		{
			name:   "wrong method",
			mutate: func(p *synthReceiptParams) { p.method = "GET" },
		},
		{
			name:   "tampered request body (body_hash mismatch)",
			mutate: func(p *synthReceiptParams) { p.bodyHashOverride = "sha256:" + strings.Repeat("0", 64) },
		},
		{
			name:   "missing request.received event",
			mutate: func(p *synthReceiptParams) { p.includeReqEvent = false },
		},
		{
			name:   "wrong cleartext_hash",
			mutate: func(p *synthReceiptParams) { p.cleartextHash = "sha256:" + strings.Repeat("1", 64) },
		},
		{
			name:   "missing response.returned event",
			mutate: func(p *synthReceiptParams) { p.includeRespEvent = false },
		},
		{
			name:   "wrong wire_hash (RespWireBytes provided)",
			mutate: func(p *synthReceiptParams) { p.wireHash = "sha256:" + strings.Repeat("2", 64) },
		},
		{
			name: "bad signature (tampered after signing)",
			tamperSigned: func(t *testing.T, b []byte) []byte {
				t.Helper()
				// Flip a byte in the served_at value so the canonical bytes change but
				// the signature does not, breaking the signature check.
				var m map[string]json.RawMessage
				if err := json.Unmarshal(b, &m); err != nil {
					t.Fatalf("unmarshal for tamper: %v", err)
				}
				m["served_at"] = json.RawMessage("1700000999")
				out, err := json.Marshal(m)
				if err != nil {
					t.Fatalf("marshal tampered: %v", err)
				}
				return out
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			vr, priv, keyID := synthVerifiedReport(t)
			p := validReceiptParams(t, vr, synthReqBody, respCleartext, respWire, synthProvider, synthModelID)
			if tt.mutate != nil {
				tt.mutate(&p)
			}
			receiptJSON := buildSignedReceipt(t, p, keyID, priv)
			if tt.tamperSigned != nil {
				receiptJSON = tt.tamperSigned(t, receiptJSON)
			}
			expect := synthExpect(p.reqBodyCompact, respCleartext, respWire)
			if tt.expectMut != nil {
				tt.expectMut(&expect)
			}
			requireReceiptInvalid(t, VerifyReceipt(receiptJSON, &vr, expect))
		})
	}
}

// TestVerifyReceiptUpstreamUnverified proves every upstream miss fails closed with
// reason upstream_unverified.
func TestVerifyReceiptUpstreamUnverified(t *testing.T) {
	t.Parallel()
	respCleartext := []byte("resp-cleartext")
	respWire := []byte("resp-wire")

	tests := []struct {
		name      string
		mutate    func(p *synthReceiptParams)
		expectMut func(e *ReceiptExpect)
	}{
		{
			name:   "upstream result failed",
			mutate: func(p *synthReceiptParams) { p.upstreamResult = "failed" },
		},
		{
			name:   "no upstream.verified event",
			mutate: func(p *synthReceiptParams) { p.includeUpstream = false },
		},
		{
			name:   "model_id mismatch",
			mutate: func(p *synthReceiptParams) { p.upstreamModelID = "gpt-3.5" },
		},
		{
			name:   "provider mismatch (Vendor set)",
			mutate: func(p *synthReceiptParams) { p.upstreamProvider = "anthropic" },
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			vr, priv, keyID := synthVerifiedReport(t)
			p := validReceiptParams(t, vr, synthReqBody, respCleartext, respWire, synthProvider, synthModelID)
			if tt.mutate != nil {
				tt.mutate(&p)
			}
			receiptJSON := buildSignedReceipt(t, p, keyID, priv)
			expect := synthExpect(p.reqBodyCompact, respCleartext, respWire)
			if tt.expectMut != nil {
				tt.expectMut(&expect)
			}
			requireUpstreamUnverified(t, VerifyReceipt(receiptJSON, &vr, expect))
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
