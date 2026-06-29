package aci

// This file implements Dstack ACI ("aci/1") §9 receipt verification: parsing a
// signed receipt off the wire, projecting it into the canonical (JCS) bytes the
// gateway signed, and verifying that signature against an attested
// receipt-signing key.
//
// The canonical projection mirrors the Rust reference
// private_ai_gateway::aci::types::Receipt::to_canonical_value(false) byte-for-byte
// (the signature won't verify otherwise):
//
//   - Each ReceiptEvent flattens into ONE object: {seq, type, ...fields}, the
//     event's `fields` object members merged alongside `seq` and `type`. JCS sorts
//     keys on emit, so the merge order does not affect the bytes.
//   - The receipt projects to {api_version, receipt_id, chat_id, workload_id,
//     workload_keyset_digest, endpoint, method, served_at, event_log, signature},
//     where signature carries ONLY {algo, key_id} — signature.value is OMITTED (it
//     is the thing being signed). chat_id is JSON null when absent, a string when
//     present (the Rust Option<String> arm).
//
// The signature verify mirrors private_ai_gateway::aci::keys::verify_receipt_signature,
// dispatching on the RECEIPT KEY's algo (the key found by signature.key_id in the
// attested keyset's receipt_signing_keys):
//
//   - ed25519: a 32-byte public key verifies a 64-byte signature over the RAW
//     canonical bytes (RFC 8032).
//   - ecdsa-secp256k1: a 65-byte recoverable signature in Dstack r‖s‖v (v-last)
//     layout, recovered over sha256(canonical bytes) and required to EQUAL the
//     receipt key's public key (recover-and-equal). The bare 64-byte r‖s JOSE form
//     is rejected (the length check fires first).
//
// EVERY failure — unknown key_id, algo mismatch, bad hex, wrong length, a failed
// verify — funnels to the fail-closed *llm.AttestationError with reason
// receipt_invalid. The typed cause carries only the algorithm string and a short
// reason label; it never carries key material (this path is verify-only and
// touches public keys and canonical bytes only).

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// Receipt is a parsed ACI §9 receipt: the per-request signed event log plus its
// metadata and the signature over the canonical projection. ChatID is a pointer
// because the wire field is nullable (Rust Option<String>): nil when absent/null,
// a string when present — the two project to different canonical bytes. ServedAt
// and ReceiptEvent.Seq are uint64 (the Rust u64 arm).
type Receipt struct {
	APIVersion           string           `json:"api_version"`
	ReceiptID            string           `json:"receipt_id"`
	ChatID               *string          `json:"chat_id"`
	WorkloadID           string           `json:"workload_id"`
	WorkloadKeysetDigest string           `json:"workload_keyset_digest"`
	Endpoint             string           `json:"endpoint"`
	Method               string           `json:"method"`
	ServedAt             uint64           `json:"served_at"`
	EventLog             []ReceiptEvent   `json:"event_log"`
	Signature            ReceiptSignature `json:"signature"`
}

// ReceiptEvent is one event in the receipt's event log. The wire shape flattens
// seq and type to the top of the object alongside any type-specific fields; this
// struct keeps Fields as RAW JSON (an object), parsed into a jcs.Value only when
// building the canonical projection. EventType reads the wire key "type".
type ReceiptEvent struct {
	Seq       uint64          `json:"seq"`
	EventType string          `json:"type"`
	Fields    json.RawMessage `json:"-"`
}

// ReceiptSignature is the receipt's signature block: the algorithm, the key id
// naming the attested receipt-signing key, and the hex signature value. ValueHex
// reads the wire key "value"; it is OMITTED from the canonical projection (it is
// the signed thing).
type ReceiptSignature struct {
	Algo     string `json:"algo"`
	KeyID    string `json:"key_id"`
	ValueHex string `json:"value"`
}

// reservedEventKeys are the flattened event keys that are NOT type-specific
// fields; they are projected explicitly, so they are stripped before the fields
// object is merged (mirroring the Rust ReceiptEvent::fields, which excludes them).
var reservedEventKeys = map[string]struct{}{
	"seq":  {},
	"type": {},
}

// receiptParseError is the typed error wrapping a stdlib json failure on the
// ParseReceipt path, so no bare stdlib error escapes the exported API (per
// CLAUDE.md). It chains the underlying cause via Unwrap so callers can errors.As
// to *json.SyntaxError / *json.UnmarshalTypeError while keeping the uniform typed
// contract. It carries no payload beyond the chained cause.
type receiptParseError struct {
	cause error
}

func (e *receiptParseError) Error() string { return "aci/receipt: parse: " + e.cause.Error() }
func (e *receiptParseError) Unwrap() error { return e.cause }

// ParseReceipt decodes a §9 receipt from its wire JSON. It first decodes the
// fixed receipt fields, then re-decodes each event-log entry to split the fixed
// seq/type members from the free-form (type-specific) fields, which are retained
// as raw JSON for the canonical projection. Any stdlib decode failure is wrapped
// in a typed *receiptParseError.
func ParseReceipt(data []byte) (*Receipt, error) {
	var r Receipt
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, &receiptParseError{cause: err}
	}

	// Re-decode the raw event-log entries to capture each event's free-form
	// fields (every wire member except seq/type) as raw JSON. json.Unmarshal into
	// Receipt cannot do this because Fields is tagged json:"-".
	wrapper := struct {
		EventLog []json.RawMessage `json:"event_log"`
	}{}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, &receiptParseError{cause: err}
	}
	rawEvents := wrapper.EventLog

	for i := range r.EventLog {
		if i >= len(rawEvents) {
			break
		}
		fields, err := eventFields(rawEvents[i])
		if err != nil {
			return nil, &receiptParseError{cause: err}
		}
		r.EventLog[i].Fields = fields
	}
	return &r, nil
}

// eventFields extracts the type-specific fields of one flat event object: every
// member except the reserved seq/type keys, re-marshaled as a raw JSON object.
// It returns "{}" when the event has no extra fields, so the canonical projection
// always merges a (possibly empty) object.
func eventFields(rawEvent json.RawMessage) (json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(rawEvent, &all); err != nil {
		return nil, err
	}
	for k := range reservedEventKeys {
		delete(all, k)
	}
	if len(all) == 0 {
		return json.RawMessage("{}"), nil
	}
	out, err := json.Marshal(all)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// canonicalReceipt builds the canonical (JCS) bytes the gateway signed: the
// Rust Receipt::to_canonical_value(false) projection (signature.value OMITTED,
// events flattened), canonicalized via the shared jcs Canonicalize. It returns
// the JCS bytes — the exact preimage ed25519 signs and the sha256-prehash the
// secp256k1 recover commits to.
func canonicalReceipt(r *Receipt) ([]byte, error) {
	events := make(Array, 0, len(r.EventLog))
	for i := range r.EventLog {
		ev, err := canonicalEvent(&r.EventLog[i])
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}

	// chat_id: null when absent (Rust Option<String>::None), a string when present.
	var chatID Value = Null{}
	if r.ChatID != nil {
		chatID = String(*r.ChatID)
	}

	signature := NewObject().
		Set("algo", String(r.Signature.Algo)).
		Set("key_id", String(r.Signature.KeyID))

	projection := NewObject().
		Set("api_version", String(r.APIVersion)).
		Set("receipt_id", String(r.ReceiptID)).
		Set("chat_id", chatID).
		Set("workload_id", String(r.WorkloadID)).
		Set("workload_keyset_digest", String(r.WorkloadKeysetDigest)).
		Set("endpoint", String(r.Endpoint)).
		Set("method", String(r.Method)).
		Set("served_at", Uint(r.ServedAt)).
		Set("event_log", events).
		Set("signature", signature)

	return Canonicalize(projection)
}

// canonicalEvent flattens one event into a single JCS object: {seq, type} merged
// with the event's fields object members. The fields raw JSON is parsed into a
// jcs.Value; it must be an object (or absent/empty) so its members merge cleanly
// alongside seq and type. JCS sorts keys on emit, so the merge order is moot.
func canonicalEvent(ev *ReceiptEvent) (Value, error) {
	obj := NewObject().
		Set("seq", Uint(ev.Seq)).
		Set("type", String(ev.EventType))

	if len(ev.Fields) == 0 {
		return obj, nil
	}
	parsed, err := ParseValue(ev.Fields)
	if err != nil {
		return nil, err
	}
	fieldsObj, ok := parsed.(*Object)
	if !ok {
		// The wire contract says fields is an object; a non-object is a malformed
		// event. Surface it as the typed parse error so the canonical path fails
		// closed rather than silently dropping fields.
		return nil, &receiptEventFieldsError{}
	}
	for i := 0; i < fieldsObj.Len(); i++ {
		obj.Set(fieldsObj.KeyAt(i), fieldsObj.ValueAt(i))
	}
	return obj, nil
}

// receiptEventFieldsError reports an event whose fields are not a JSON object —
// a malformed receipt. It is typed (no bare error) and carries no payload because
// the only fact is "fields was not an object".
type receiptEventFieldsError struct{}

func (e *receiptEventFieldsError) Error() string {
	return "aci/receipt: event fields are not a JSON object"
}

// receiptSignatureSizeEd25519 / receiptSignatureSizeSecp256k1 are the exact byte
// lengths the two receipt-signature formats require: ed25519 is 64 bytes (RFC
// 8032); the secp256k1 receipt signature is the 65-byte recoverable r‖s‖v form
// (the bare 64-byte r‖s JOSE form is rejected).
const (
	receiptSignatureSizeEd25519   = ed25519.SignatureSize
	receiptSignatureSizeSecp256k1 = recoverableSigSize
)

// verifyReceiptSig verifies a receipt signature per ACI §9.4. It finds the
// signing key by keyID in vr.Keyset.ReceiptSigningKeys, then dispatches on THAT
// key's algo (never the receipt's claimed signature.algo) to verify sigHex over
// canonical:
//
//   - ed25519: ed25519.Verify(pub, canonical, sig) over the raw canonical bytes.
//   - ecdsa-secp256k1: recover the public key from (sha256(canonical), r‖s, recid)
//     and require it equals the receipt key's public key (recover-and-equal).
//
// It returns nil ONLY on a cryptographically valid signature; every failure —
// unknown key_id, algo mismatch, bad hex, wrong length, failed verify — returns
// the fail-closed *llm.AttestationError with reason receipt_invalid, wrapping a
// typed cause that names the algo and a short reason and carries no secret.
func verifyReceiptSig(vr VerifiedReport, keyID string, canonical []byte, sigHex string) error {
	key, ok := findReceiptKey(vr.Keyset.ReceiptSigningKeys, keyID)
	if !ok {
		return receiptInvalid("", "no receipt-signing key with the receipt's key_id", nil)
	}

	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return receiptInvalid(key.Algo, "signature hex decode failed", err)
	}
	pub, err := hex.DecodeString(key.PublicKeyHex)
	if err != nil {
		return receiptInvalid(key.Algo, "public key hex decode failed", err)
	}

	switch key.Algo {
	case algoEd25519:
		return verifyReceiptEd25519(pub, canonical, sig)
	case algoSecp256k1:
		return verifyReceiptSecp256k1(pub, canonical, sig)
	default:
		return receiptInvalid(key.Algo, "unsupported receipt signature algorithm", nil)
	}
}

// findReceiptKey returns the receipt-signing key whose key_id equals keyID, or
// ok false when none matches. It returns a copy by value; callers only read it.
func findReceiptKey(keys []KeyEntry, keyID string) (KeyEntry, bool) {
	for _, k := range keys {
		if k.KeyID == keyID {
			return k, true
		}
	}
	return KeyEntry{}, false
}

// verifyReceiptEd25519 verifies a 64-byte ed25519 signature over the raw
// canonical bytes with a 32-byte public key (RFC 8032). Both lengths are checked
// before the stdlib verify, which would otherwise panic on a wrong-length key.
func verifyReceiptEd25519(pub, canonical, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return receiptInvalid(algoEd25519, "ed25519 public key wrong length", nil)
	}
	if len(sig) != receiptSignatureSizeEd25519 {
		return receiptInvalid(algoEd25519, "ed25519 signature wrong length", nil)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), canonical, sig) {
		return receiptInvalid(algoEd25519, "ed25519 signature verification failed", nil)
	}
	return nil
}

// verifyReceiptSecp256k1 verifies a 65-byte recoverable secp256k1 signature in
// Dstack r‖s‖v (v-last) layout: recover the public key from sha256(canonical) and
// require it EQUALS the receipt key's public key (recover-and-equal). The receipt
// key's public key is parsed as SEC1 (33/65-byte) and compared on its serialized
// compressed form, so an uncompressed and compressed encoding of the same key
// match. The bare 64-byte r‖s JOSE form is rejected (length check first).
func verifyReceiptSecp256k1(pub, canonical, sig []byte) error {
	if len(sig) != receiptSignatureSizeSecp256k1 {
		return receiptInvalid(algoSecp256k1, "secp256k1 receipt signature must be 65 bytes (r||s||v)", nil)
	}
	expected, err := secp256k1.ParsePubKey(pub)
	if err != nil {
		return receiptInvalid(algoSecp256k1, "secp256k1 receipt public key parse failed", err)
	}

	digest := sha256.Sum256(canonical)
	recovered, err := recoverCompactFromDigest(digest[:], sig)
	if err != nil {
		return receiptInvalid(algoSecp256k1, "secp256k1 receipt signature recovery failed", err)
	}
	if !recovered.IsEqual(expected) {
		return receiptInvalid(algoSecp256k1, "recovered key does not match the receipt signing key", nil)
	}
	return nil
}

// receiptInvalid builds the fail-closed *llm.AttestationError for a receipt
// verification failure, wrapping a typed *receiptVerifyError cause (per CLAUDE.md:
// no bare fmt.Errorf from package APIs). cause may be nil for failures with no
// underlying error to chain (e.g. a wrong-length signature or a missing key_id).
func receiptInvalid(algo, reason string, cause error) error {
	return attestErr(reasonReceiptInvalid, &receiptVerifyError{
		algo:   algo,
		reason: reason,
		cause:  cause,
	})
}

// receiptVerifyError is the typed cause wrapped inside the receipt_invalid
// *llm.AttestationError. It names the algorithm (a short wire string, empty when
// the failure precedes key lookup) and a fixed, code-defined reason label so the
// cause keeps type identity and callers can errors.As to inspect it. It carries
// no key material — only the public algo string and a static reason — so logging
// it leaks no secret. cause chains any underlying stdlib/library error.
type receiptVerifyError struct {
	// algo is the wire algorithm string (e.g. "ecdsa-secp256k1"), or empty when
	// the failure occurred before a key was found; a short identifier, never key
	// bytes.
	algo string
	// reason is a fixed, code-defined label, never external data.
	reason string
	// cause is the chained underlying error, or nil.
	cause error
}

func (e *receiptVerifyError) Error() string {
	msg := "aci/receipt: receipt invalid"
	if e.algo != "" {
		msg += " (" + e.algo + ")"
	}
	msg += ": " + e.reason
	if e.cause != nil {
		return msg + ": " + e.cause.Error()
	}
	return msg
}

func (e *receiptVerifyError) Unwrap() error { return e.cause }
