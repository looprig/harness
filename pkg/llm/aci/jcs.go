package aci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// This file implements the constrained JCS (canonical-JSON) profile used by the
// Dstack "aci/1" confidential-inference protocol. It mirrors the Rust reference
// private_ai_gateway::aci::canonical (pinned commit 1b43f76e) byte-for-byte:
//
//   - Numbers are integers only: an i64, else a u64 (so 0..2^64-1 are valid);
//     anything fractional, exponential, or out of range is rejected as a
//     *FloatNotAllowedError. There are no floats on the wire.
//   - Object keys are sorted by UTF-16 code-unit lexicographic order at every
//     level (NOT raw byte / rune order — they differ for supplementary-plane
//     characters that encode as surrogate pairs).
//   - Strings escape only " and \ and the C0 controls; the two-char short
//     escapes \b \t \n \f \r are used where defined, every other code point
//     below 0x20 becomes a lowercase \u00xx, and all other characters
//     (including non-ASCII) are emitted as raw UTF-8 bytes.
//   - Arrays preserve element order; there is no whitespace anywhere.
//
// The Value union stores objects as ORDERED key/value pairs (insertion order);
// the JCS key sort happens only in the emitter, never in the type. This keeps
// Value reusable by a non-sorting compact serializer (the body-hash profile)
// that preserves insertion order.

// Value is the sealed union of canonicalizable JSON values. The unexported
// marker method keeps the set of concrete types closed to this package.
type Value interface {
	isValue()
}

// String is a JSON string value.
type String string

// Int is a signed-integer JSON number (the i64 arm of the number rule).
type Int int64

// Uint is an unsigned-integer JSON number (the u64 tail above the i64 range).
type Uint uint64

// Number is an integer JSON number carried verbatim as the source decimal
// literal (e.g. produced by the parser, which defers the i64-then-u64 check to
// emit time). Canonicalize validates it: a non-integer or out-of-range literal
// yields a *FloatNotAllowedError, matching the Rust reference's as_i64/as_u64
// fallthrough.
type Number json.Number

// Float is a NON-integer JSON number used ONLY by the compact body serializer
// (CompactJSON), not by the JCS canonical path. Real chat-request bodies carry
// fractional sampling params (temperature/top_p, e.g. 0.7, 0.9), so the body
// hash must serialize floats; the constrained JCS profile forbids them. Float
// therefore lives in the shared Value union but is REJECTED by Canonicalize
// (it surfaces *FloatNotAllowedError there) and accepted only by CompactJSON.
// See body.go for the emission rules and their validated serde_json domain.
type Float float64

// Bool is a JSON boolean.
type Bool bool

// Null is the JSON null literal.
type Null struct{}

// Array is an ordered list of values; order is preserved on emit.
type Array []Value

// objMember is one ordered (key, value) pair inside an Object.
type objMember struct {
	key string
	val Value
}

// Object is a JSON object stored as ordered key/value pairs in insertion order.
// JCS key sorting is applied by the emitter, not by this type, so the same
// Object can be emitted in insertion order by a non-sorting serializer.
type Object struct {
	members []objMember
}

func (String) isValue()  {}
func (Int) isValue()     {}
func (Uint) isValue()    {}
func (Number) isValue()  {}
func (Float) isValue()   {}
func (Bool) isValue()    {}
func (Null) isValue()    {}
func (Array) isValue()   {}
func (*Object) isValue() {}

// NewObject returns an empty Object ready for ordered Set calls.
func NewObject() *Object { return &Object{} }

// Set inserts or updates a (key, value) pair and returns the Object so calls can
// chain. It follows IndexMap / serde_json(preserve_order) semantics: a new key is
// appended (preserving insertion order); an existing key keeps its original
// position and has its value overwritten (last value wins). This guarantees an
// Object never holds duplicate keys, so the emitter can never produce invalid
// JSON — the same parity serde_json yields on the deserialize path. The canonical
// emitter is still responsible for the UTF-16 key SORT; Set only dedups.
func (o *Object) Set(key string, val Value) *Object {
	for i := range o.members {
		if o.members[i].key == key {
			o.members[i].val = val
			return o
		}
	}
	o.members = append(o.members, objMember{key: key, val: val})
	return o
}

// Len reports the number of members.
func (o *Object) Len() int { return len(o.members) }

// KeyAt returns the key of the i-th member in insertion order. It panics on an
// out-of-range index, matching slice-indexing semantics.
func (o *Object) KeyAt(i int) string { return o.members[i].key }

// ValueAt returns the value of the i-th member in insertion order.
func (o *Object) ValueAt(i int) Value { return o.members[i].val }

// FloatNotAllowedError reports a JSON number that is not an acceptable integer
// under the constrained JCS profile: a fraction, an exponent, a value outside
// the i64 ∪ u64 range, OR a literal that is not in canonical JSON-integer form
// (a leading '+' or a leading zero such as "01"). It carries the offending decimal
// literal so callers can inspect the cause; it is the Go analogue of the Rust
// CanonicalError variant FloatNotAllowed. It is an internal canonicalization
// failure, distinct from the protocol-level *llm.AttestationError reasons.
//
// The grammar gate matters off the JSON-parse path: serde_json (and Go's
// json.Decoder) only ever produce grammar-valid number literals, so on the parse
// path the looser cases never occur. But a programmatically-constructed
// Number("+1") / Number("01") must fail closed rather than be silently
// normalized — a digest validator must reject anything it would not have emitted.
type FloatNotAllowedError struct {
	// Literal is the offending JSON number literal (e.g. "1.5", "1e5", "+1",
	// "01"). It is a numeric token only and carries no secret material.
	Literal string
}

func (e *FloatNotAllowedError) Error() string {
	return "aci/jcs: number " + e.Literal + " is not an acceptable integer literal"
}

// nilValueError reports a nil Value handed to the emitter — a programming bug,
// since Value is a sealed union. It is typed (per the no-bare-error rule) so the
// emitter can fail closed instead of producing a wrong canonical encoding.
type nilValueError struct{}

func (e *nilValueError) Error() string { return "aci/jcs: nil Value" }

// InvalidUTF8Error reports a String value or object key that is not valid UTF-8.
// The emitter writes string bytes verbatim while the key sort decodes them via
// []rune (which folds invalid sequences to U+FFFD); those two views diverge on
// malformed input, so a digest validator must reject it rather than risk an
// order/encoding mismatch. On the JSON-parse path this never occurs (json
// guarantees valid UTF-8), but Task 1.3 builds String values programmatically.
//
// It deliberately does NOT echo the offending bytes (they may be arbitrary,
// possibly secret-adjacent payload); it reports only where the fault was found.
type InvalidUTF8Error struct {
	// Where names the location of the fault: "string" for a String value or
	// "object key" for a member key. It is a fixed label, never external data.
	Where string
}

func (e *InvalidUTF8Error) Error() string { return "aci/jcs: invalid UTF-8 in " + e.Where }

// integerLiteral validates a JSON number literal under the integer-only rule and
// returns its canonical decimal form. It first enforces the strict JSON-integer
// grammar (-?(0|[1-9][0-9]*): no leading '+', no leading zeros), then accepts an
// i64, then a u64 (so the full 0..2^64-1 range is valid), matching the Rust
// as_i64/as_u64 fallback. Any fractional, exponential, malformed, or
// out-of-range literal returns a *FloatNotAllowedError. "-0" is grammar-valid
// and normalizes to "0" (as serde_json does).
//
// The grammar pre-gate is load-bearing because strconv.ParseInt/ParseUint are
// looser than JSON: they silently accept and normalize "+1"→"1", "01"→"1". On
// the JSON-parse path json.Decoder already rejects those, but a
// programmatically-built Number("+1") would be normalized instead of rejected —
// the wrong posture for a digest validator and looser than serde_json::Number.
// "-0" is the one case the gate intentionally lets through, because serde_json
// accepts it and emits "0"; rejecting it would diverge from the reference on
// valid JSON input.
func integerLiteral(num json.Number) (string, *FloatNotAllowedError) {
	s := string(num)
	if !isStrictJSONInteger(s) {
		return "", &FloatNotAllowedError{Literal: s}
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return strconv.FormatInt(i, 10), nil
	}
	if u, err := strconv.ParseUint(s, 10, 64); err == nil {
		return strconv.FormatUint(u, 10), nil
	}
	return "", &FloatNotAllowedError{Literal: s}
}

// isStrictJSONInteger reports whether s matches the JSON number integer grammar
// -?(0|[1-9][0-9]*): an optional single leading '-', then either a single '0' or
// a non-zero digit followed by more digits. It rejects '+' signs, leading zeros
// ("01", "00", "-01"), empty/lone-"-" input, and any non-digit byte (so
// fractions/exponents fail here too).
//
// "-0" IS accepted: it is grammar-valid JSON, and the Rust reference
// (serde_json::Number::as_i64) parses it to the integer 0 and emits "0". The
// subsequent ParseInt normalizes "-0"→"0", matching that. The gate's purpose is
// to reject literals that are NOT canonical JSON integers — exactly the forms
// strconv would otherwise silently normalize ("+1", "01") — without diverging
// from Rust on any literal a real serde_json::Number could hold.
func isStrictJSONInteger(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
		if s == "" { // a lone "-"
			return false
		}
	}
	if s == "0" {
		return true // a single '0' (or "-0") is the only zero form allowed
	}
	if s[0] == '0' { // leading zero (covers "00", "01", "-00", "-01", ...)
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Canonicalize emits the constrained-JCS canonical UTF-8 encoding of v. Object
// keys are sorted by UTF-16 code units at every level; numbers must be integers.
// It returns a *FloatNotAllowedError if any number violates the integer rule.
func Canonicalize(v Value) ([]byte, error) {
	var b strings.Builder
	if err := emit(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// emit writes the canonical encoding of v into b. It is the single recursive
// emitter; each Value concrete type has exactly one canonical form.
func emit(b *strings.Builder, v Value) error {
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
		lit, err := integerLiteral(json.Number(t))
		if err != nil {
			return err
		}
		b.WriteString(lit)
		return nil
	case Float:
		// The constrained JCS profile forbids non-integer numbers. Float exists
		// only for the compact body path (CompactJSON); it must NEVER reach the
		// canonical digest. Reject it as the typed *FloatNotAllowedError, carrying
		// the offending value as its shortest decimal literal so the JCS path
		// stays fail-closed and indistinguishable from a fractional Number.
		return &FloatNotAllowedError{Literal: strconv.FormatFloat(float64(t), 'g', -1, 64)}
	case Array:
		return emitArray(b, t)
	case *Object:
		return emitObject(b, t)
	default:
		// Unreachable in practice: Value is a sealed union and every concrete
		// type is handled above. Only a nil Value interface reaches here; we
		// surface it as a typed error rather than silently emitting a literal,
		// so a programming bug fails closed instead of producing a wrong digest.
		return &nilValueError{}
	}
}

// emitArray writes "[e0,e1,…]" with no whitespace, preserving order.
func emitArray(b *strings.Builder, a Array) error {
	b.WriteByte('[')
	for i, e := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := emit(b, e); err != nil {
			return err
		}
	}
	b.WriteByte(']')
	return nil
}

// emitObject writes {"k0":v0,"k1":v1,…} with keys sorted by UTF-16 code units
// and no whitespace. Sorting is done on a copy of the member slice so the
// Object's insertion order is left untouched (the type stays order-preserving).
//
// Keys are validated as UTF-8 BEFORE sorting: lessUTF16 decodes via []rune
// (folding invalid bytes to U+FFFD), so an invalid key must fail closed up front
// rather than be silently reordered to a U+FFFD-based position.
func emitObject(b *strings.Builder, o *Object) error {
	for _, m := range o.members {
		if !utf8.ValidString(m.key) {
			return &InvalidUTF8Error{Where: "object key"}
		}
	}
	sorted := make([]objMember, len(o.members))
	copy(sorted, o.members)
	sort.SliceStable(sorted, func(i, j int) bool {
		return lessUTF16(sorted[i].key, sorted[j].key)
	})
	b.WriteByte('{')
	for i, m := range sorted {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := emitString(b, m.key, "object key"); err != nil {
			return err
		}
		b.WriteByte(':')
		if err := emit(b, m.val); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

// lessUTF16 reports whether a sorts before b in UTF-16 code-unit lexicographic
// order. This is the JCS / RFC 8785 key ordering: compare the UTF-16 code-unit
// sequences element-wise. It differs from byte/rune order for supplementary
// characters, whose surrogate code units (0xD800..0xDFFF) sort below 0xE000.
func lessUTF16(a, b string) bool {
	ua := utf16.Encode([]rune(a))
	ub := utf16.Encode([]rune(b))
	n := len(ua)
	if len(ub) < n {
		n = len(ub)
	}
	for i := 0; i < n; i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// hexDigits is the lowercase hex alphabet for \u00xx control escapes.
const hexDigits = "0123456789abcdef"

// emitString writes a JSON string literal per the Dstack profile: " and \ are
// backslash-escaped; the short C escapes \b \t \n \f \r are used where defined;
// every other code point below 0x20 becomes a lowercase \u00xx; all other bytes
// (including the multi-byte UTF-8 of non-ASCII characters) are written raw.
//
// where labels the call site ("string" or "object key") for the
// *InvalidUTF8Error returned when s is not valid UTF-8 — a fail-closed guard so
// the raw-byte emit can never diverge from the []rune-based key sort.
func emitString(b *strings.Builder, s, where string) error {
	if !utf8.ValidString(s) {
		return &InvalidUTF8Error{Where: where}
	}
	b.WriteByte('"')
	// Iterate by byte: every byte >= 0x20 except " and \ is passed through
	// verbatim, which preserves valid UTF-8 sequences for non-ASCII runes
	// without \u-escaping them.
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return nil
}

// Sha256Raw returns the raw SHA-256 digest of the canonical encoding of v.
func Sha256Raw(v Value) ([32]byte, error) {
	canon, err := Canonicalize(v)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canon), nil
}

// Sha256Hex returns the canonical digest as "sha256:" + lowercase hex of the
// SHA-256 of the canonical encoding of v.
func Sha256Hex(v Value) (string, error) {
	sum, err := Sha256Raw(v)
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// parseError wraps a stdlib json.Decoder failure (malformed JSON, unexpected
// EOF, …) in a typed error so the exported ParseValue contract is uniformly
// typed per CLAUDE.md, while still chaining the underlying cause via Unwrap (so
// errors.Is(err, io.EOF) and errors.As to *json.SyntaxError keep working).
type parseError struct {
	cause error
}

func (e *parseError) Error() string { return "aci/jcs: parse: " + e.cause.Error() }
func (e *parseError) Unwrap() error { return e.cause }

// trailingDataError reports extra tokens after a complete JSON value (e.g.
// "{} {}"). It is a typed parse failure carrying no payload because the only
// fact is "there was more".
type trailingDataError struct{}

func (e *trailingDataError) Error() string {
	return "aci/jcs: trailing data after JSON value"
}

// malformedTokenError reports a json.Token that violates the decoder's own
// contract — a bare close-delimiter where a value was expected, a non-string
// object key, or (with UseNumber) a float64. These are UNREACHABLE on real input
// because json.Decoder rejects such structures before yielding the token; they
// exist purely as fail-closed guards so a future decoder change cannot silently
// produce a wrong Value.
type malformedTokenError struct{}

func (e *malformedTokenError) Error() string {
	return "aci/jcs: malformed JSON token"
}

// ParseValue decodes JSON bytes into the ordered Value union. Objects keep
// insertion order (Go maps would lose it), numbers are validated against the
// integer rule on the way in, and the only any in the package lives here at the
// json.Token boundary, immediately narrowed to a concrete Value. All stdlib
// decoder failures are wrapped in *parseError (Unwrap-able); the integer rule
// yields *FloatNotAllowedError; surplus tokens yield *trailingDataError.
func ParseValue(data []byte) (Value, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	// Reject trailing tokens after a complete value (e.g. "{} {}"); a clean
	// document yields exactly one value then EOF.
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, &trailingDataError{}
		}
		return nil, &parseError{cause: err}
	}
	return v, nil
}

// parseValue reads exactly one JSON value from dec. The first Token() drives the
// dispatch: delimiters open objects/arrays, scalars map straight to the union.
func parseValue(dec *json.Decoder) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, &parseError{cause: err}
	}
	return parseToken(dec, tok)
}

// parseToken narrows a single decoded json.Token (the lone any boundary) into a
// concrete Value, recursing into containers via the decoder.
func parseToken(dec *json.Decoder, tok json.Token) (Value, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return parseObject(dec)
		case '[':
			return parseArray(dec)
		default:
			// Unreachable: json.Decoder never yields a bare '}' or ']' as the
			// start of a value. Fail-closed guard.
			return nil, &malformedTokenError{}
		}
	case string:
		return String(t), nil
	case json.Number:
		if _, ferr := integerLiteral(t); ferr != nil {
			return nil, ferr
		}
		return Number(t), nil
	case bool:
		return Bool(t), nil
	case nil:
		return Null{}, nil
	default:
		// Unreachable: with UseNumber the decoder never yields float64, and the
		// cases above are exhaustive over json.Token. Fail-closed guard.
		return nil, &malformedTokenError{}
	}
}

// parseObject reads members until the closing '}', preserving insertion order.
// The opening '{' has already been consumed by the caller. Duplicate keys follow
// IndexMap semantics via Object.Set (last value wins, first position kept).
func parseObject(dec *json.Decoder) (Value, error) {
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
		val, err := parseValue(dec)
		if err != nil {
			return nil, err
		}
		obj.Set(key, val)
	}
	// Consume the closing '}'.
	if _, err := dec.Token(); err != nil {
		return nil, &parseError{cause: err}
	}
	return obj, nil
}

// parseArray reads elements until the closing ']', preserving order. The opening
// '[' has already been consumed by the caller.
func parseArray(dec *json.Decoder) (Value, error) {
	arr := Array{}
	for dec.More() {
		val, err := parseValue(dec)
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
	// Consume the closing ']'.
	if _, err := dec.Token(); err != nil {
		return nil, &parseError{cause: err}
	}
	return arr, nil
}
