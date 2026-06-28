package aci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
)

// This file implements the COMPACT, ORDER-PRESERVING JSON serializer used to
// reproduce the ACI receipt body hash. The gateway computes
//
//	request.received.body_hash = sha256_hex(serde_json::to_vec(cleartext_body))
//
// (src/aggregator/service/e2ee.rs re-serializes the decrypted payload via
// serde_json::to_vec; src/aci/receipt.rs hashes those bytes). serde_json is
// built with the preserve_order feature, so object keys are emitted in the
// INSERTION order the client sent — NOT sorted. CompactJSON must reproduce those
// bytes exactly, or receipt verification fails.
//
// CompactJSON deliberately differs from Canonicalize (jcs.go) in exactly two
// ways, and is identical in every other respect:
//
//  1. Object keys are emitted in insertion order, NOT sorted by UTF-16 code
//     units. (Canonicalize sorts; CompactJSON does not.)
//  2. Non-integer floats are ACCEPTED and emitted (the Float variant), because
//     real bodies carry fractional sampling params (temperature/top_p).
//     Canonicalize still rejects every float as *FloatNotAllowedError.
//
// String escaping and integer formatting are SHARED with the JCS emitter
// (emitString / integerLiteral in jcs.go): serde_json and the Dstack JCS profile
// use byte-identical rules for strings (" \ short C-escapes, lowercase \u00xx
// controls, raw UTF-8 passthrough) and for integers (i64 then u64, no
// normalization surprises). The only profile-specific addition here is float
// emission, documented on emitFloat below.

// NonFiniteFloatError reports a Float value that is NaN or ±Inf. JSON has no
// representation for non-finite numbers (serde_json's Number cannot even hold
// them — Number::from_f64 returns None), so a non-finite Float can never appear
// in a real body and must fail closed rather than emit a non-JSON token. It is
// typed (per CLAUDE.md's no-bare-error rule) and carries the Go textual form of
// the offending value ("NaN", "+Inf", "-Inf") — a numeric label, never secret
// material.
type NonFiniteFloatError struct {
	// Repr is the Go string form of the non-finite value ("NaN", "+Inf",
	// "-Inf"). It is a fixed numeric label and carries no payload bytes.
	Repr string
}

func (e *NonFiniteFloatError) Error() string {
	return "aci/body: non-finite float " + e.Repr + " has no JSON representation"
}

// FloatOutOfDomainError reports a finite Float whose magnitude falls in the
// range where serde_json (ryu) emits EXPONENT form rather than plain decimal —
// |x| >= 1e16 or 0 < |x| < 1e-5. In that range serde's spelling (e.g. "1e+16",
// "1e-6") cannot be reproduced by Go's shortest-float emitter without re-deriving
// ryu's exact exponent rules, so this serializer REFUSES to emit such a value
// rather than risk a wrong body hash. The realistic sampling-parameter domain
// (temperature/top_p ∈ [0,2]) lies entirely inside the decimal-form window, so
// this never fires on a real body; it is the runtime form of Task 1.3's
// "blocker #2: STOP and report" rule — a value here must be re-pinned against a
// fresh Rust vector before its hash can be trusted.
//
// It carries the offending value's shortest decimal form (a numeric label, no
// secret material) so the caller can identify it.
type FloatOutOfDomainError struct {
	// Value is the shortest decimal form of the offending magnitude (e.g.
	// "1e+16"). It is a numeric label and carries no payload bytes.
	Value string
}

func (e *FloatOutOfDomainError) Error() string {
	return "aci/body: float " + e.Value + " is outside the serde decimal-emit domain [1e-5, 1e16); re-pin against a Rust vector"
}

// serdeDecimalLow and serdeDecimalHigh bound the magnitude window in which
// serde_json (ryu) emits a finite float in plain DECIMAL form. Below
// serdeDecimalLow (and above 0) or at/above serdeDecimalHigh, serde switches to
// exponent form, whose spelling Go's emitter does not reproduce — so emitFloat
// fails closed there. These boundaries were pinned against the Rust reference:
// 1e-5 -> "0.00001" (decimal) but 1e-6 -> "1e-6" (exponent); 9.999e15 ->
// "9999000000000000.0" (decimal) but 1e16 -> "1e+16" (exponent).
const (
	serdeDecimalLow  = 1e-5
	serdeDecimalHigh = 1e16
)

// CompactJSON emits the compact, order-preserving JSON encoding of v, matching
// serde_json::to_vec (preserve_order) byte-for-byte for the body-hash profile.
// Object keys are emitted in insertion order; non-integer floats are emitted via
// the Float variant. It returns a *NonFiniteFloatError for a NaN/Inf Float, a
// *FloatNotAllowedError for a non-integer Number (only Float carries
// fractionals), an *InvalidUTF8Error for a malformed string/key, or a
// *nilValueError for a nil Value.
func CompactJSON(v Value) ([]byte, error) {
	var b strings.Builder
	if err := emitCompact(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// emitCompact writes the compact encoding of v into b. It mirrors jcs.go's emit
// but (a) emits object members in insertion order and (b) accepts Float. Every
// other arm delegates to the SAME shared helpers as the JCS emitter so the two
// encoders cannot drift on strings or integers.
func emitCompact(b *strings.Builder, v Value) error {
	switch t := v.(type) {
	case Null:
		b.WriteString("null")
		return nil
	case Bool:
		if bool(t) {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return nil
	case String:
		return emitString(b, string(t), "string")
	case Int:
		b.WriteString(strconv.FormatInt(int64(t), 10))
		return nil
	case Uint:
		b.WriteString(strconv.FormatUint(uint64(t), 10))
		return nil
	case Number:
		// A Number is integer-only on every path; the strict integer gate is
		// shared with the JCS emitter. Fractionals must use the Float variant.
		lit, err := integerLiteral(json.Number(t))
		if err != nil {
			return err
		}
		b.WriteString(lit)
		return nil
	case Float:
		return emitFloat(b, float64(t))
	case Array:
		return emitCompactArray(b, t)
	case *Object:
		return emitCompactObject(b, t)
	default:
		// Unreachable: Value is a sealed union and every concrete type is handled
		// above. Only a nil Value interface reaches here; surface it as the same
		// typed error the JCS emitter uses so a programming bug fails closed.
		return &nilValueError{}
	}
}

// emitCompactArray writes "[e0,e1,…]" with no whitespace, preserving order. It
// is identical to jcs.go's emitArray except it recurses through emitCompact (so
// nested objects keep insertion order too).
func emitCompactArray(b *strings.Builder, a Array) error {
	b.WriteByte('[')
	for i, e := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := emitCompact(b, e); err != nil {
			return err
		}
	}
	b.WriteByte(']')
	return nil
}

// emitCompactObject writes {"k0":v0,"k1":v1,…} in INSERTION order with no
// whitespace — the one structural difference from jcs.go's emitObject, which
// sorts keys by UTF-16 code units. Keys are still validated as UTF-8 via the
// shared emitString guard (serde_json keys are always valid UTF-8; a
// programmatically-built invalid key must fail closed identically on both paths).
func emitCompactObject(b *strings.Builder, o *Object) error {
	b.WriteByte('{')
	for i := 0; i < len(o.members); i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := emitString(b, o.members[i].key, "object key"); err != nil {
			return err
		}
		b.WriteByte(':')
		if err := emitCompact(b, o.members[i].val); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

// emitFloat writes the compact JSON encoding of a float, matching serde_json's
// f64 emission (ryu shortest-round-trip) byte-for-byte over serde's DECIMAL-form
// domain — which is exactly where every realistic sampling parameter lives.
//
// HOW SERDE EMITS f64. serde_json re-parses the wire literal to f64 then
// re-emits via ryu shortest-round-trip — NOT verbatim ("0.70"->"0.7",
// "1e0"->"1.0"). Within the magnitude window [1e-5, 1e16) (and at exactly 0) ryu
// uses plain decimal form and, crucially, gives WHOLE numbers a ".0" suffix
// (1.0, 2.0, -0.0, 1000000000000000.0). Outside that window (|x| >= 1e16 or
// 0 < |x| < 1e-5) ryu switches to exponent form (1e+16, 1e-6).
//
// HOW WE MATCH IT. For the decimal window we emit Go's shortest 'f' form
// (strconv 'f', prec -1), which is the same shortest-round-trip decimal as ryu,
// then append ".0" when the result has no fractional point — reproducing serde's
// whole-number ".0" suffix. This is byte-identical to serde across the entire
// decimal window, not merely the [0,2] sampling range: the golden body vectors
// (chat_float_temperature_top_p, chat_fractional_temperature,
// chat_float_and_int_mixed) pin 0.7->"0.7", 0.9->"0.9", 1.5->"1.5", 0.5->"0.5"
// against the Rust reference; the unit tests additionally pin -0.0->"-0.0",
// 2.0->"2.0", 100.0->"100.0".
//
// WHY WE FAIL CLOSED OUTSIDE THE WINDOW. In serde's exponent-form range the
// spelling ("1e+16", "1e-6") cannot be reproduced from Go's shortest float
// without re-deriving ryu's exact exponent rules (Go's own 'g'/'e' cutoffs
// differ — Go switches to exponent only at 1e21, and pads/strips exponents
// differently). Rather than risk silently emitting a WRONG body hash, emitFloat
// returns *FloatOutOfDomainError there — the runtime form of Task 1.3's
// "blocker #2: STOP and report" rule. The realistic temperature/top_p domain
// ([0,2]) is wholly inside the decimal window, so this never fires on a real
// body; a value that trips it must be re-pinned against a fresh Rust vector.
//
// The decimal-vs-exponent boundary is pinned in the serdeDecimal* constants and
// kept inline so a future Go stdlib change cannot silently shift our output away
// from the pinned vectors.
func emitFloat(b *strings.Builder, f float64) error {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return &NonFiniteFloatError{Repr: strconv.FormatFloat(f, 'g', -1, 64)}
	}
	abs := math.Abs(f)
	if abs != 0 && (abs < serdeDecimalLow || abs >= serdeDecimalHigh) {
		// serde would use exponent form here; we cannot reproduce that spelling
		// faithfully. Fail closed with the shortest decimal as the label.
		return &FloatOutOfDomainError{Value: strconv.FormatFloat(f, 'g', -1, 64)}
	}
	// Decimal window: shortest 'f' form, then append ".0" for whole numbers to
	// match serde/ryu (which never emits a bare integer literal for an f64).
	out := strconv.AppendFloat(nil, f, 'f', -1, 64)
	if !hasFractionPoint(out) {
		out = append(out, '.', '0')
	}
	b.Write(out)
	return nil
}

// hasFractionPoint reports whether the formatted-float bytes already contain a
// decimal point. 'f'-form output never contains an exponent, so a missing '.'
// means the value rendered as a bare integer (e.g. "2", "-0") and needs the
// serde ".0" suffix.
func hasFractionPoint(b []byte) bool {
	for _, c := range b {
		if c == '.' {
			return true
		}
	}
	return false
}

// Sha256HexBytes returns "sha256:" + lowercase hex of the SHA-256 of the given
// bytes. It is the receipt body-hash function over an already-serialized compact
// body (sha256_hex in the Rust reference), distinct from Sha256Hex which hashes
// the CANONICAL (JCS) encoding of a Value. Callers pass CompactJSON(body) here.
func Sha256HexBytes(b []byte) (string, error) {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ParseBodyValue decodes a request-body JSON document into the ordered Value
// union, ACCEPTING non-integer floats (mapped to the Float variant). It is the
// body-hash counterpart to the strict, float-REJECTING ParseValue: bodies carry
// fractional sampling params that the JCS profile forbids, so the body path needs
// its own float-tolerant parse. Objects keep insertion order; integers go to
// Int/Uint (or stay as the verbatim Number where they exceed neither range);
// non-integer numbers become Float (the wire literal parsed to f64, exactly as
// serde_json does before re-emitting via ryu). All decoder failures are wrapped
// in the same typed *parseError; surplus tokens yield *trailingDataError.
//
// Sharing the parse spine with ParseValue (rather than forking it) would couple
// the strict and tolerant policies; instead this is a small, separate parser
// that reuses only the leaf helpers, keeping each policy single-responsibility.
func ParseBodyValue(data []byte) (Value, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	v, err := parseBodyValue(dec)
	if err != nil {
		return nil, err
	}
	// Reject trailing tokens after a complete value, matching ParseValue.
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, &trailingDataError{}
		}
		return nil, &parseError{cause: err}
	}
	return v, nil
}

// parseBodyValue reads exactly one JSON value from dec, accepting floats.
func parseBodyValue(dec *json.Decoder) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, &parseError{cause: err}
	}
	return parseBodyToken(dec, tok)
}

// parseBodyToken narrows a single decoded json.Token into a concrete Value,
// recursing into containers. It differs from parseToken (jcs.go) only in its
// number handling: an integer literal becomes Number (validated, integer-only),
// a non-integer literal becomes Float (parsed to f64) instead of being rejected.
func parseBodyToken(dec *json.Decoder, tok json.Token) (Value, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return parseBodyObject(dec)
		case '[':
			return parseBodyArray(dec)
		default:
			// Unreachable: json.Decoder never yields a bare close-delim as a value.
			return nil, &malformedTokenError{}
		}
	case string:
		return String(t), nil
	case json.Number:
		return numberToBodyValue(t)
	case bool:
		return Bool(t), nil
	case nil:
		return Null{}, nil
	default:
		// Unreachable: with UseNumber the decoder never yields float64.
		return nil, &malformedTokenError{}
	}
}

// numberToBodyValue maps a decoded json.Number to either an integer Number (when
// it satisfies the strict integer rule) or a Float (otherwise). This mirrors how
// serde_json's IndexMap-backed Value stores the wire number: integers stay
// integers; a fractional/exponent literal is parsed to f64. A literal that is
// neither a valid integer NOR a parseable finite float is rejected as a typed
// *parseError (it cannot have come from json.Decoder, which only yields
// grammar-valid numbers, but the guard keeps the mapping total and fail-closed).
func numberToBodyValue(num json.Number) (Value, error) {
	if _, ferr := integerLiteral(num); ferr == nil {
		// A valid JSON integer in i64 ∪ u64 range: keep it as the verbatim
		// Number so CompactJSON emits it through the shared integer path,
		// byte-identical to serde's integer emission.
		return Number(num), nil
	}
	// Not an integer in range: treat as a float, exactly as serde_json does
	// (parse the wire literal to f64; emission re-derives the shortest form).
	f, err := strconv.ParseFloat(string(num), 64)
	if err != nil {
		return nil, &parseError{cause: err}
	}
	return Float(f), nil
}

// parseBodyObject reads members until the closing '}', preserving insertion
// order, accepting float values. Duplicate keys follow IndexMap semantics via
// Object.Set (last value wins, first position kept) — same as parseObject.
func parseBodyObject(dec *json.Decoder) (Value, error) {
	obj := NewObject()
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, &parseError{cause: err}
		}
		key, ok := keyTok.(string)
		if !ok {
			// Unreachable: json.Decoder always yields a string key here.
			return nil, &malformedTokenError{}
		}
		val, err := parseBodyValue(dec)
		if err != nil {
			return nil, err
		}
		obj.Set(key, val)
	}
	if _, err := dec.Token(); err != nil { // consume the closing '}'
		return nil, &parseError{cause: err}
	}
	return obj, nil
}

// parseBodyArray reads elements until the closing ']', preserving order,
// accepting float values.
func parseBodyArray(dec *json.Decoder) (Value, error) {
	arr := Array{}
	for dec.More() {
		val, err := parseBodyValue(dec)
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
	if _, err := dec.Token(); err != nil { // consume the closing ']'
		return nil, &parseError{cause: err}
	}
	return arr, nil
}
