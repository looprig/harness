package aci

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// nan and inf produce non-finite float64 values for the rejection tests without
// importing math at every call site. negZero produces IEEE-754 negative zero
// (distinct bit pattern from +0.0), which serde emits as "-0.0".
func nan() float64         { return math.NaN() }
func inf(sign int) float64 { return math.Inf(sign) }
func negZero() float64     { return math.Copysign(0, -1) }

// bodyVectorFile is the committed golden-vector fixture for the compact
// serde_json body serializer, generated from the Rust reference
// (serde_json::to_vec with preserve_order) at a pinned commit.
const bodyVectorFile = "testdata/body_hash_vectors.json"

// bodyVectorSet mirrors the top-level shape of body_hash_vectors.json. Only the
// vectors array is load-bearing; the metadata fields document provenance.
type bodyVectorSet struct {
	Description string       `json:"description"`
	Vectors     []bodyVector `json:"vectors"`
}

// bodyVector is one golden body-hash case. Body is the raw input JSON (kept as
// json.RawMessage so we parse it through our own order-preserving, float-
// accepting parser — never through a Go map, which would sort keys). BodyText is
// the exact compact serde_json string (authoritative key order); CompactHex is
// its lowercase hex; Sha256 is the "sha256:" digest of those bytes.
type bodyVector struct {
	Name       string          `json:"name"`
	Note       string          `json:"note"`
	Body       json.RawMessage `json:"body"`
	BodyText   string          `json:"body_text"`
	CompactHex string          `json:"compact_hex"`
	Sha256     string          `json:"sha256"`
}

// loadBodyVectors reads and decodes the committed golden-vector fixture.
func loadBodyVectors(t *testing.T) bodyVectorSet {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(bodyVectorFile))
	if err != nil {
		t.Fatalf("read %s: %v", bodyVectorFile, err)
	}
	var set bodyVectorSet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatalf("decode %s: %v", bodyVectorFile, err)
	}
	if len(set.Vectors) == 0 {
		t.Fatalf("no vectors in %s", bodyVectorFile)
	}
	return set
}

// TestCompactJSONVectors is the authoritative byte-for-byte conformance test for
// the compact body serializer: for every golden vector, CompactJSON(parse(body))
// must equal the reference serde_json::to_vec bytes (insertion order, NOT
// sorted), the UTF-8 text must equal body_text, and Sha256Hex(CompactJSON(v))
// must equal the reference digest. The set deliberately mixes integer-only
// bodies, non-integer-float bodies (temperature/top_p), and a key order that is
// not alphabetical (proving insertion order is preserved, not re-sorted).
func TestCompactJSONVectors(t *testing.T) {
	t.Parallel()
	set := loadBodyVectors(t)

	for _, vec := range set.Vectors {
		vec := vec
		t.Run(vec.Name, func(t *testing.T) {
			t.Parallel()

			v, err := ParseBodyValue(vec.Body)
			if err != nil {
				t.Fatalf("ParseBodyValue(%s) error: %v", vec.Name, err)
			}

			got, err := CompactJSON(v)
			if err != nil {
				t.Fatalf("CompactJSON(%s) error: %v", vec.Name, err)
			}

			// 1) Exact bytes against the authoritative compact_hex.
			want, err := hex.DecodeString(vec.CompactHex)
			if err != nil {
				t.Fatalf("bad compact_hex for %s: %v", vec.Name, err)
			}
			if !bytesEqual(got, want) {
				t.Errorf("CompactJSON(%s) bytes mismatch\n got: %s\nwant: %s", vec.Name, got, want)
			}

			// 2) Exact UTF-8 text against body_text (a redundant but readable
			// cross-check that the hex and text fields agree with our output).
			if string(got) != vec.BodyText {
				t.Errorf("CompactJSON(%s) text\n got: %q\nwant: %q", vec.Name, string(got), vec.BodyText)
			}

			// 3) The receipt body-hash itself.
			gotHex, err := Sha256HexBytes(got)
			if err != nil {
				t.Fatalf("Sha256HexBytes(%s) error: %v", vec.Name, err)
			}
			if gotHex != vec.Sha256 {
				t.Errorf("Sha256HexBytes(%s) = %q, want %q", vec.Name, gotHex, vec.Sha256)
			}
		})
	}
}

// TestCompactJSONInsertionOrder proves the compact emitter preserves member
// insertion order and never sorts keys, in direct contrast to Canonicalize
// (which sorts by UTF-16 code units). The same Object emits differently through
// the two paths: insertion order vs sorted order.
func TestCompactJSONInsertionOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		obj          *Object
		wantCompact  string
		wantCanonacl string // canonical (sorted) form, to contrast
	}{
		{
			name:         "reverse-alphabetical keys kept",
			obj:          NewObject().Set("b", Int(1)).Set("a", Int(2)),
			wantCompact:  `{"b":1,"a":2}`,
			wantCanonacl: `{"a":2,"b":1}`,
		},
		{
			name:         "stream before model (insertion)",
			obj:          NewObject().Set("stream", Bool(false)).Set("model", String("m")),
			wantCompact:  `{"stream":false,"model":"m"}`,
			wantCanonacl: `{"model":"m","stream":false}`,
		},
		{
			name:         "mixed case insertion vs utf16 sort",
			obj:          NewObject().Set("Z", Int(1)).Set("a", Int(2)).Set("A", Int(3)),
			wantCompact:  `{"Z":1,"a":2,"A":3}`,
			wantCanonacl: `{"A":3,"Z":1,"a":2}`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotCompact, err := CompactJSON(tt.obj)
			if err != nil {
				t.Fatalf("CompactJSON error: %v", err)
			}
			if string(gotCompact) != tt.wantCompact {
				t.Errorf("CompactJSON = %q, want %q", gotCompact, tt.wantCompact)
			}
			gotCanon, err := Canonicalize(tt.obj)
			if err != nil {
				t.Fatalf("Canonicalize error: %v", err)
			}
			if string(gotCanon) != tt.wantCanonacl {
				t.Errorf("Canonicalize = %q, want %q (compact path must differ)", gotCanon, tt.wantCanonacl)
			}
		})
	}
}

// TestCompactJSONStringEscaping pins the compact emitter's string profile and
// proves it is byte-identical to the JCS emitter's (serde_json and the Dstack
// JCS profile share the same escape rules: " and \, the short C escapes,
// lowercase \u00xx for other controls, raw UTF-8 for non-ASCII).
func TestCompactJSONStringEscaping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
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
			got, err := CompactJSON(String(tt.in))
			if err != nil {
				t.Fatalf("CompactJSON(String(%q)) error: %v", tt.in, err)
			}
			if string(got) != tt.want {
				t.Errorf("CompactJSON(String(%q)) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCompactJSONFloatEmission pins the Float emission against the serde-derived
// expectations for the realistic temperature/top_p domain. These exact strings
// are validated transitively by the golden body vectors; this table exercises
// the Float Value directly so the format choice is asserted in isolation.
func TestCompactJSONFloatEmission(t *testing.T) {
	t.Parallel()
	// wantErr selects which typed error a failing case must surface:
	// "nonfinite" -> *NonFiniteFloatError, "domain" -> *FloatOutOfDomainError,
	// "" -> success.
	tests := []struct {
		name    string
		value   Value
		want    string
		wantErr string
	}{
		// Decimal-window fractionals: byte-identical to serde/ryu (the golden
		// body vectors pin these against the Rust reference).
		{name: "0.7", value: Float(0.7), want: "0.7"},
		{name: "0.9", value: Float(0.9), want: "0.9"},
		{name: "0.5", value: Float(0.5), want: "0.5"},
		{name: "1.5", value: Float(1.5), want: "1.5"},
		{name: "0.1", value: Float(0.1), want: "0.1"},
		{name: "negative fraction", value: Float(-0.25), want: "-0.25"},
		// Whole-number floats get serde's ".0" suffix (ryu never emits a bare
		// integer literal for an f64).
		{name: "2.0 whole -> 2.0", value: Float(2.0), want: "2.0"},
		{name: "0.0 whole -> 0.0", value: Float(0.0), want: "0.0"},
		{name: "negative zero -> -0.0", value: Float(negZero()), want: "-0.0"},
		{name: "100.0 whole -> 100.0", value: Float(100.0), want: "100.0"},
		// serde decimal-window edges (still decimal form).
		{name: "1e-5 decimal edge", value: Float(1e-5), want: "0.00001"},
		{name: "9.999e15 decimal edge", value: Float(9.999e15), want: "9999000000000000.0"},
		// Non-finite: no JSON representation at all.
		{name: "NaN rejected", value: Float(nan()), wantErr: "nonfinite"},
		{name: "+Inf rejected", value: Float(inf(+1)), wantErr: "nonfinite"},
		{name: "-Inf rejected", value: Float(inf(-1)), wantErr: "nonfinite"},
		// Exponent-form domain: serde would emit exponent spelling we do not
		// reproduce, so we fail closed rather than emit a wrong hash.
		{name: "1e16 exponent domain rejected", value: Float(1e16), wantErr: "domain"},
		{name: "1e-6 exponent domain rejected", value: Float(1e-6), wantErr: "domain"},
		{name: "1e20 exponent domain rejected", value: Float(1e20), wantErr: "domain"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := CompactJSON(tt.value)
			if (err != nil) != (tt.wantErr != "") {
				t.Fatalf("CompactJSON() err = %v, wantErr %q", err, tt.wantErr)
			}
			switch tt.wantErr {
			case "nonfinite":
				var ne *NonFiniteFloatError
				if !errors.As(err, &ne) {
					t.Fatalf("error %v (%T) is not *NonFiniteFloatError", err, err)
				}
				return
			case "domain":
				var de *FloatOutOfDomainError
				if !errors.As(err, &de) {
					t.Fatalf("error %v (%T) is not *FloatOutOfDomainError", err, err)
				}
				return
			}
			if string(got) != tt.want {
				t.Errorf("CompactJSON(Float) = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCompactJSONIntegersAndLiterals confirms the compact emitter reuses the
// integer/literal rules unchanged: integers, the u64 tail, bools, null, nested
// arrays/objects. Numbers built via the (integer-only) Number variant still go
// through the strict integer gate; a non-integer Number is rejected (only the
// Float variant carries fractionals on the compact path).
func TestCompactJSONIntegersAndLiterals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   Value
		want    string
		wantErr bool
	}{
		{name: "int zero", value: Int(0), want: "0"},
		{name: "int negative", value: Int(-1), want: "-1"},
		{name: "uint tail", value: Uint(18446744073709551615), want: "18446744073709551615"},
		{name: "bool true", value: Bool(true), want: "true"},
		{name: "bool false", value: Bool(false), want: "false"},
		{name: "null", value: Null{}, want: "null"},
		{name: "array order preserved", value: Array{Int(3), Int(1), Int(2)}, want: "[3,1,2]"},
		{name: "nested mixed", value: Array{Bool(true), Null{}, Float(0.5)}, want: "[true,null,0.5]"},
		{name: "integer Number ok", value: Number(json.Number("256")), want: "256"},
		{name: "non-integer Number rejected", value: Number(json.Number("1.5")), wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := CompactJSON(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CompactJSON() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if string(got) != tt.want {
				t.Errorf("CompactJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParseBodyValueFloats verifies the body parser accepts non-integer floats
// (which strict ParseValue rejects) and preserves object insertion order. The
// parsed Float must round-trip through CompactJSON to serde's form.
func TestParseBodyValueFloats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		in          string
		wantKeys    []string
		wantCompact string
		wantErr     bool
	}{
		{
			name:        "float accepted and order kept",
			in:          `{"temperature":0.7,"top_p":0.9}`,
			wantKeys:    []string{"temperature", "top_p"},
			wantCompact: `{"temperature":0.7,"top_p":0.9}`,
		},
		{
			name:        "fractional single",
			in:          `{"t":1.5}`,
			wantKeys:    []string{"t"},
			wantCompact: `{"t":1.5}`,
		},
		{
			name:        "mixed int and float keeps order",
			in:          `{"temperature":1,"top_p":0.5,"max_tokens":256}`,
			wantKeys:    []string{"temperature", "top_p", "max_tokens"},
			wantCompact: `{"temperature":1,"top_p":0.5,"max_tokens":256}`,
		},
		{
			name:    "malformed rejected",
			in:      `{`,
			wantErr: true,
		},
		{
			name:    "trailing data rejected",
			in:      `{} {}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v, err := ParseBodyValue([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseBodyValue() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			obj, ok := v.(*Object)
			if !ok {
				t.Fatalf("ParseBodyValue() = %T, want *Object", v)
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
			got, err := CompactJSON(v)
			if err != nil {
				t.Fatalf("CompactJSON error: %v", err)
			}
			if string(got) != tt.wantCompact {
				t.Errorf("CompactJSON = %q, want %q", got, tt.wantCompact)
			}
		})
	}
}

// TestCanonicalizeStillRejectsFloats is the JCS-invariant guard: adding Float
// support for the compact body path must NOT weaken the strict JCS path. A Float
// (or a non-integer Number) handed to Canonicalize must still surface the typed
// *FloatNotAllowedError — no floats ever reach the canonical digest.
func TestCanonicalizeStillRejectsFloats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value Value
	}{
		{name: "bare Float", value: Float(0.7)},
		{name: "Float in object", value: NewObject().Set("temperature", Float(0.7))},
		{name: "Float in array", value: Array{Float(1.5)}},
		{name: "non-integer Number", value: Number(json.Number("1.5"))},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Canonicalize(tt.value)
			if err == nil {
				t.Fatalf("Canonicalize(%s) = nil error, want *FloatNotAllowedError", tt.name)
			}
			var fe *FloatNotAllowedError
			if !errors.As(err, &fe) {
				t.Fatalf("error %v (%T) is not *FloatNotAllowedError", err, err)
			}
		})
	}
}

// TestParseValueStillRejectsFloats confirms the strict, float-rejecting
// ParseValue is untouched: a body with a non-integer float still fails at parse
// with *FloatNotAllowedError (the JCS callers depend on this).
func TestParseValueStillRejectsFloats(t *testing.T) {
	t.Parallel()
	_, err := ParseValue([]byte(`{"temperature":0.7}`))
	if err == nil {
		t.Fatalf("ParseValue(float) = nil error, want *FloatNotAllowedError")
	}
	var fe *FloatNotAllowedError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v (%T) is not *FloatNotAllowedError", err, err)
	}
}
