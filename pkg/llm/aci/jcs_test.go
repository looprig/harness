package aci

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// jcsVectorFile is the committed golden-vector fixture generated from the Rust
// reference (private_ai_gateway::aci::canonical) at a pinned commit.
const jcsVectorFile = "testdata/jcs_vectors.json"

// jcsVectorSet mirrors the top-level shape of jcs_vectors.json. Only the vectors
// array is load-bearing for the test; the metadata fields document provenance.
type jcsVectorSet struct {
	Description string      `json:"description"`
	Profile     string      `json:"profile"`
	Vectors     []jcsVector `json:"vectors"`
}

// jcsVector is one golden case. Value is the raw input JSON (kept as
// json.RawMessage so we parse it through our own order-preserving parser, never
// through a Go map). When WantErr is false, CanonicalHex/Sha256 hold the
// expected canonical bytes (lowercase hex) and the "sha256:" digest.
type jcsVector struct {
	Name         string          `json:"name"`
	Value        json.RawMessage `json:"value"`
	WantErr      bool            `json:"wantErr"`
	ErrKind      string          `json:"errKind"`
	CanonicalHex string          `json:"canonical_hex"`
	Sha256       string          `json:"sha256"`
}

// loadJCSVectors reads and decodes the committed golden-vector fixture.
func loadJCSVectors(t *testing.T) jcsVectorSet {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(jcsVectorFile))
	if err != nil {
		t.Fatalf("read %s: %v", jcsVectorFile, err)
	}
	var set jcsVectorSet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatalf("decode %s: %v", jcsVectorFile, err)
	}
	if len(set.Vectors) == 0 {
		t.Fatalf("no vectors in %s", jcsVectorFile)
	}
	return set
}

// TestCanonicalizeVectors is the authoritative byte-for-byte conformance test:
// for every golden vector, Canonicalize(parse(value)) must equal the reference
// canonical bytes, and Sha256Hex must equal the reference digest. Float (and
// other non-integer) numbers must surface the typed *FloatNotAllowedError.
func TestJCSCanonicalizeVectors(t *testing.T) {
	t.Parallel()
	set := loadJCSVectors(t)

	for _, vec := range set.Vectors {
		vec := vec
		t.Run(vec.Name, func(t *testing.T) {
			t.Parallel()

			v, parseErr := ParseValue(vec.Value)

			if vec.WantErr {
				// The reject can surface either at parse (the parser is strict
				// about numbers too) or at canonicalize; either way the typed
				// *FloatNotAllowedError must be reachable via errors.As, and no
				// canonical bytes are produced.
				var got []byte
				err := parseErr
				if err == nil {
					got, err = Canonicalize(v)
				}
				if err == nil {
					t.Fatalf("Canonicalize(%s) = %q, want error", vec.Name, got)
				}
				var fe *FloatNotAllowedError
				if !errors.As(err, &fe) {
					t.Fatalf("error %v (%T) is not *FloatNotAllowedError", err, err)
				}
				return
			}

			if parseErr != nil {
				t.Fatalf("ParseValue(%s) error: %v", vec.Name, parseErr)
			}

			got, err := Canonicalize(v)
			if err != nil {
				t.Fatalf("Canonicalize(%s) error: %v", vec.Name, err)
			}
			want, err := hex.DecodeString(vec.CanonicalHex)
			if err != nil {
				t.Fatalf("bad canonical_hex for %s: %v", vec.Name, err)
			}
			if !bytesEqual(got, want) {
				t.Errorf("Canonicalize(%s) bytes mismatch\n got: %x\nwant: %x", vec.Name, got, want)
			}

			gotHex, err := Sha256Hex(v)
			if err != nil {
				t.Fatalf("Sha256Hex(%s) error: %v", vec.Name, err)
			}
			if gotHex != vec.Sha256 {
				t.Errorf("Sha256Hex(%s) = %q, want %q", vec.Name, gotHex, vec.Sha256)
			}
		})
	}
}

// bytesEqual is a tiny helper to keep the comparison explicit (bytes.Equal would
// do, but this avoids importing bytes solely for one call).
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCanonicalizeNumbers covers the integer-only number rule directly against
// the Value constructors: i64 range, the u64 tail above i64-max, and the
// non-integer rejects (fraction, exponent), independent of the JSON parser.
func TestJCSCanonicalizeNumbers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   Value
		want    string // expected canonical bytes (string form)
		wantErr bool
	}{
		{name: "zero", value: Int(0), want: "0"},
		{name: "negative", value: Int(-1), want: "-1"},
		{name: "int64 min", value: Int(-9223372036854775808), want: "-9223372036854775808"},
		{name: "int64 max", value: Int(9223372036854775807), want: "9223372036854775807"},
		{name: "uint64 above int64 max", value: Uint(9223372036854775808), want: "9223372036854775808"},
		{name: "uint64 max", value: Uint(18446744073709551615), want: "18446744073709551615"},
		// "-0" is grammar-valid JSON; like serde_json it normalizes to "0".
		{name: "negative zero normalizes", value: rawNumber("-0"), want: "0"},
		{name: "fraction rejected", value: rawNumber("1.5"), wantErr: true},
		{name: "exponent rejected", value: rawNumber("1e5"), wantErr: true},
		{name: "leading-dot-ish rejected", value: rawNumber("0.0"), wantErr: true},
		{name: "out of range rejected", value: rawNumber("99999999999999999999999"), wantErr: true},
		// Strict JSON-integer grammar gate (off the parse path): strconv would
		// silently normalize these, but a digest validator must fail closed.
		{name: "leading plus rejected", value: rawNumber("+1"), wantErr: true},
		{name: "leading zero rejected", value: rawNumber("01"), wantErr: true},
		{name: "double zero rejected", value: rawNumber("00"), wantErr: true},
		{name: "negative leading zero rejected", value: rawNumber("-01"), wantErr: true},
		{name: "negative double zero rejected", value: rawNumber("-00"), wantErr: true},
		{name: "empty literal rejected", value: rawNumber(""), wantErr: true},
		{name: "lone minus rejected", value: rawNumber("-"), wantErr: true},
		{name: "whitespace literal rejected", value: rawNumber(" 1"), wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Canonicalize(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Canonicalize() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var fe *FloatNotAllowedError
				if !errors.As(err, &fe) {
					t.Fatalf("error %v (%T) is not *FloatNotAllowedError", err, err)
				}
				return
			}
			if string(got) != tt.want {
				t.Errorf("Canonicalize() = %q, want %q", got, tt.want)
			}
		})
	}
}

// rawNumber builds a Number Value from its decimal string without going through
// the parser, so the number rule is exercised at the emitter boundary.
func rawNumber(s string) Value { return Number(json.Number(s)) }

// TestCanonicalizeStringEscaping pins the Dstack string profile: the two
// mandatory escapes (" and \), the short C escapes, lowercase \u00xx for other
// control chars, and raw UTF-8 passthrough for non-ASCII.
func TestJCSCanonicalizeStringEscaping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string // canonical bytes of the bare string value
	}{
		{name: "empty", in: "", want: `""`},
		{name: "plain ascii", in: "abc", want: `"abc"`},
		{name: "quote", in: `a"b`, want: `"a\"b"`},
		{name: "backslash", in: `a\b`, want: `"a\\b"`},
		{name: "backspace short escape", in: "\b", want: `"\b"`},
		{name: "tab short escape", in: "\t", want: `"\t"`},
		{name: "newline short escape", in: "\n", want: `"\n"`},
		{name: "formfeed short escape", in: "\f", want: `"\f"`},
		{name: "carriage return short escape", in: "\r", want: `"\r"`},
		{name: "control U+0001 lowercase hex", in: "\x01", want: `"\u0001"`},
		{name: "control U+001f lowercase hex", in: "\x1f", want: `"\u001f"`},
		{name: "non-ascii raw utf8", in: "héllo", want: "\"héllo\""},
		{name: "emoji raw utf8 (supplementary)", in: "😀", want: "\"😀\""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Canonicalize(String(tt.in))
			if err != nil {
				t.Fatalf("Canonicalize(String(%q)) error: %v", tt.in, err)
			}
			if string(got) != tt.want {
				t.Errorf("Canonicalize(String(%q)) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCanonicalizeKeySortUTF16 proves object keys sort by UTF-16 code units, not
// by raw UTF-8 byte order. The surrogate-pair case (U+10000 'A') differs between
// the two orderings: in UTF-8 byte order U+10000 (0xF0…) sorts AFTER U+E000
// (0xEE…); in UTF-16 code-unit order U+10000 (high surrogate 0xD800) sorts
// BEFORE U+E000, so the surrogate key comes first.
func TestJCSCanonicalizeKeySortUTF16(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		obj  *Object
		want string
	}{
		{
			name: "ascii keys sorted",
			obj:  NewObject().Set("b", Int(1)).Set("a", Int(2)),
			want: `{"a":2,"b":1}`,
		},
		{
			name: "mixed case by code unit",
			obj:  NewObject().Set("Z", Int(1)).Set("a", Int(2)).Set("A", Int(3)),
			want: `{"A":3,"Z":1,"a":2}`,
		},
		{
			name: "surrogate sorts before private-use BMP char",
			// "" (U+E000, BMP) vs "\U00010000" (U+10000, supplementary).
			obj:  NewObject().Set("", Int(2)).Set("\U00010000", Int(1)),
			want: "{\"\U00010000\":1,\"\":2}",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Canonicalize(tt.obj)
			if err != nil {
				t.Fatalf("Canonicalize() error: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Canonicalize() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCanonicalizeArrayOrder confirms arrays preserve element order and emit no
// whitespace, including nested objects whose keys are still sorted.
func TestJCSCanonicalizeArrayOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Value
		want string
	}{
		{name: "empty array", in: Array{}, want: "[]"},
		{name: "scalar order preserved", in: Array{Int(3), Int(1), Int(2)}, want: "[3,1,2]"},
		{
			name: "nested object keys sorted inside array",
			in:   Array{NewObject().Set("y", Int(2)).Set("x", Int(1))},
			want: `[{"x":1,"y":2}]`,
		},
		{
			name: "literals",
			in:   Array{Bool(true), Bool(false), Null{}},
			want: "[true,false,null]",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Canonicalize(tt.in)
			if err != nil {
				t.Fatalf("Canonicalize() error: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Canonicalize() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParseValuePreservesObjectOrder verifies the order-preserving parser keeps
// object members in insertion order (so Task 1.3's compact body serializer can
// reuse Value without re-sorting). The canonical emitter still re-sorts; this
// test inspects the parsed structure directly.
func TestJCSParseValuePreservesObjectOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		in       string
		wantKeys []string
		wantErr  bool
	}{
		{name: "object keeps insertion order", in: `{"b":1,"a":2,"c":3}`, wantKeys: []string{"b", "a", "c"}},
		{name: "empty object", in: `{}`, wantKeys: []string{}},
		{name: "nested keeps order", in: `{"z":{"y":1,"x":2}}`, wantKeys: []string{"z"}},
		{name: "float rejected at parse", in: `{"x":1.5}`, wantErr: true},
		{name: "malformed rejected", in: `{`, wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v, err := ParseValue([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseValue() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			obj, ok := v.(*Object)
			if !ok {
				t.Fatalf("ParseValue() = %T, want *Object", v)
			}
			gotKeys := make([]string, 0, obj.Len())
			for i := 0; i < obj.Len(); i++ {
				gotKeys = append(gotKeys, obj.KeyAt(i))
			}
			if len(gotKeys) != len(tt.wantKeys) {
				t.Fatalf("keys = %v, want %v", gotKeys, tt.wantKeys)
			}
			for i := range gotKeys {
				if gotKeys[i] != tt.wantKeys[i] {
					t.Errorf("key[%d] = %q, want %q", i, gotKeys[i], tt.wantKeys[i])
				}
			}
		})
	}
}

// TestSha256Raw verifies Sha256Raw returns the 32-byte digest whose lowercase
// hex (prefixed) equals Sha256Hex, and matches a known vector.
func TestJCSSha256Raw(t *testing.T) {
	t.Parallel()
	// empty_object: sha256 of "{}" is a fixed, well-known value.
	v, err := ParseValue([]byte(`{}`))
	if err != nil {
		t.Fatalf("ParseValue: %v", err)
	}
	raw, err := Sha256Raw(v)
	if err != nil {
		t.Fatalf("Sha256Raw: %v", err)
	}
	wantHex := "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	if got := hex.EncodeToString(raw[:]); got != wantHex {
		t.Errorf("Sha256Raw hex = %q, want %q", got, wantHex)
	}
	hexStr, err := Sha256Hex(v)
	if err != nil {
		t.Fatalf("Sha256Hex: %v", err)
	}
	if hexStr != "sha256:"+wantHex {
		t.Errorf("Sha256Hex = %q, want %q", hexStr, "sha256:"+wantHex)
	}
}

// TestFloatNotAllowedError exercises the typed error's surface: it carries the
// offending number literal and renders a message that names it (no secrets).
func TestJCSFloatNotAllowedError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		literal string
	}{
		{name: "fraction", literal: "1.5"},
		{name: "exponent", literal: "1e5"},
		{name: "overflow", literal: "99999999999999999999999"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Canonicalize(rawNumber(tt.literal))
			if err == nil {
				t.Fatalf("Canonicalize(%q) = nil error, want *FloatNotAllowedError", tt.literal)
			}
			var fe *FloatNotAllowedError
			if !errors.As(err, &fe) {
				t.Fatalf("error %v (%T) is not *FloatNotAllowedError", err, err)
			}
			if fe.Literal != tt.literal {
				t.Errorf("Literal = %q, want %q", fe.Literal, tt.literal)
			}
			if fe.Error() == "" {
				t.Errorf("Error() is empty")
			}
		})
	}
}

// TestJCSObjectDuplicateKeys verifies IndexMap / serde_json(preserve_order)
// duplicate-key semantics: the last value wins and the key keeps its first
// position. This holds both for the programmatic Set API and for the ParseValue
// path, and the canonical emit must therefore contain the key exactly once.
func TestJCSObjectDuplicateKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		build    func() Value // produce the Value under test
		wantKeys []string     // expected keys in INSERTION order (pre-sort)
		wantVal  string       // canonical bytes (post UTF-16 sort)
	}{
		{
			name:     "Set last value wins keeps position",
			build:    func() Value { return NewObject().Set("a", Int(1)).Set("b", Int(2)).Set("a", Int(9)) },
			wantKeys: []string{"a", "b"},
			wantVal:  `{"a":9,"b":2}`,
		},
		{
			name:     "parse duplicate key last wins",
			build:    func() Value { v, _ := ParseValue([]byte(`{"a":1,"b":2,"a":9}`)); return v },
			wantKeys: []string{"a", "b"},
			wantVal:  `{"a":9,"b":2}`,
		},
		{
			name:     "parse triple duplicate",
			build:    func() Value { v, _ := ParseValue([]byte(`{"k":1,"k":2,"k":3}`)); return v },
			wantKeys: []string{"k"},
			wantVal:  `{"k":3}`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := tt.build()
			obj, ok := v.(*Object)
			if !ok {
				t.Fatalf("built value is %T, want *Object", v)
			}
			if obj.Len() != len(tt.wantKeys) {
				t.Fatalf("Len() = %d, want %d (keys %v)", obj.Len(), len(tt.wantKeys), tt.wantKeys)
			}
			for i, want := range tt.wantKeys {
				if got := obj.KeyAt(i); got != want {
					t.Errorf("KeyAt(%d) = %q, want %q", i, got, want)
				}
			}
			got, err := Canonicalize(v)
			if err != nil {
				t.Fatalf("Canonicalize: %v", err)
			}
			if string(got) != tt.wantVal {
				t.Errorf("Canonicalize = %q, want %q", got, tt.wantVal)
			}
		})
	}
}

// TestJCSInvalidUTF8 verifies the fail-closed UTF-8 guard: a String value or an
// object key containing invalid UTF-8 must yield a typed *InvalidUTF8Error
// rather than emit bytes that diverge from the []rune-based key sort. Valid
// UTF-8 (including multi-byte and supplementary) must still canonicalize.
func TestJCSInvalidUTF8(t *testing.T) {
	t.Parallel()
	const badUTF8 = "\xff\xfe" // a lone continuation/invalid lead — never valid UTF-8
	tests := []struct {
		name      string
		value     Value
		wantErr   bool
		wantWhere string
	}{
		{name: "valid ascii string", value: String("ok")},
		{name: "valid multibyte string", value: String("héllo")},
		{name: "valid supplementary string", value: String("😀")},
		{name: "invalid utf8 string value", value: String(badUTF8), wantErr: true, wantWhere: "string"},
		{
			name:      "invalid utf8 object key",
			value:     NewObject().Set(badUTF8, Int(1)),
			wantErr:   true,
			wantWhere: "object key",
		},
		{
			name:      "invalid utf8 nested in array",
			value:     Array{String("ok"), String(badUTF8)},
			wantErr:   true,
			wantWhere: "string",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Canonicalize(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Canonicalize() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var ue *InvalidUTF8Error
			if !errors.As(err, &ue) {
				t.Fatalf("error %v (%T) is not *InvalidUTF8Error", err, err)
			}
			if ue.Where != tt.wantWhere {
				t.Errorf("Where = %q, want %q", ue.Where, tt.wantWhere)
			}
			// Security: the error must not echo the offending raw bytes.
			if got := ue.Error(); got == "" {
				t.Errorf("Error() is empty")
			}
		})
	}
}

// TestJCSParseErrorTyped verifies that ParseValue wraps stdlib json.Decoder
// failures in a typed *parseError whose Unwrap chains the underlying cause, so
// errors.As(*parseError) and errors.Is(io.EOF) both hold — a uniform typed-error
// contract on the exported API.
func TestJCSParseErrorTyped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		wantEOF  bool // expect the chained cause to be io.EOF / unexpected-EOF family
		wantKind string
	}{
		{name: "empty input is EOF", input: "", wantEOF: true, wantKind: "parse"},
		{name: "unterminated object", input: `{"a":1`, wantKind: "parse"},
		{name: "garbage", input: `@@@`, wantKind: "parse"},
		{name: "trailing data", input: `{} {}`, wantKind: "trailing"},
		{name: "float still float error", input: `{"x":1.5}`, wantKind: "float"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseValue([]byte(tt.input))
			if err == nil {
				t.Fatalf("ParseValue(%q) = nil error, want error", tt.input)
			}
			switch tt.wantKind {
			case "parse":
				var pe *parseError
				if !errors.As(err, &pe) {
					t.Fatalf("error %v (%T) is not *parseError", err, err)
				}
				if errors.Unwrap(pe) == nil {
					t.Errorf("parseError.Unwrap() = nil, want a chained cause")
				}
			case "trailing":
				var te *trailingDataError
				if !errors.As(err, &te) {
					t.Fatalf("error %v (%T) is not *trailingDataError", err, err)
				}
			case "float":
				var fe *FloatNotAllowedError
				if !errors.As(err, &fe) {
					t.Fatalf("error %v (%T) is not *FloatNotAllowedError", err, err)
				}
			}
			if tt.wantEOF && !errors.Is(err, io.EOF) {
				t.Errorf("errors.Is(err, io.EOF) = false, want true (err=%v)", err)
			}
		})
	}
}
