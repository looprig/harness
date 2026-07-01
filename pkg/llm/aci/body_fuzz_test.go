package aci

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzCompactJSON feeds arbitrary bytes through ParseBodyValue (the float-
// accepting body parser) + CompactJSON and asserts two safety properties:
//
//  1. No panic — neither the float-tolerant parser nor the order-preserving
//     emitter may crash on hostile input.
//  2. Idempotence — when CompactJSON succeeds, re-parsing its output and
//     re-emitting yields byte-identical output. The compact form is a fixed
//     point: serializing it again changes nothing. This is the round-trip
//     guarantee the body-hash relies on (a stored cleartext body re-serializes
//     to the same bytes the receipt was hashed over).
//
// The seed corpus is the committed golden body vectors plus structural edge
// cases (floats, mixed int/float, nesting, unicode, deep arrays) so the fuzzer
// starts from valid, varied inputs — including the fractional-number path that
// strict ParseValue rejects but the body parser must accept.
func FuzzCompactJSON(f *testing.F) {
	for _, seed := range bodyFuzzSeeds(f) {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Property 1: ParseBodyValue must not panic; it may reject the input.
		v, err := ParseBodyValue(data)
		if err != nil {
			return // not valid body JSON — nothing to check.
		}

		// Property 1 (cont.): CompactJSON must not panic. A clean body parse can
		// still surface a typed error (e.g. invalid UTF-8 built into a String, or
		// a non-finite Float) — that is a clean, non-panicking return.
		out, err := CompactJSON(v)
		if err != nil {
			return
		}

		// Property 2: idempotence. Re-parse the compact bytes and re-emit; the
		// result must be byte-identical.
		v2, err := ParseBodyValue(out)
		if err != nil {
			t.Fatalf("compact output failed to re-parse: %v\noutput: %q", err, out)
		}
		out2, err := CompactJSON(v2)
		if err != nil {
			t.Fatalf("re-emit failed: %v\noutput: %q", err, out)
		}
		if !bytes.Equal(out, out2) {
			t.Fatalf("CompactJSON not idempotent\n first: %q\nsecond: %q", out, out2)
		}
	})
}

// bodyFuzzSeeds returns the seed corpus: every golden body-vector input plus a
// few hand-picked structural and float edge cases.
func bodyFuzzSeeds(f *testing.F) [][]byte {
	f.Helper()
	seeds := [][]byte{
		[]byte(`{}`),
		[]byte(`[]`),
		[]byte(`{"temperature":0.7,"top_p":0.9}`),
		[]byte(`{"t":1.5}`),
		[]byte(`{"temperature":1,"top_p":0.5,"max_tokens":256}`),
		[]byte(`{"stream":false,"model":"m"}`),
		[]byte(`{"n":null,"t":true,"f":false}`),
		[]byte(`[0.1,0.2,0.3]`),
		[]byte(`{"nested":{"b":1,"a":[{"y":0.5,"x":1}]}}`),
		[]byte(`{"s":"héllo"}`),
		[]byte(`{"s":"a\nb\tc"}`),
		[]byte(`-0.25`),
		[]byte(`18446744073709551615`),
		[]byte(`-9223372036854775808`),
		[]byte(`  {  "k" : 0.7 }  `),
	}

	// Add the raw body bytes of each committed golden vector, when readable.
	raw, err := os.ReadFile(filepath.Clean(bodyVectorFile))
	if err != nil {
		return seeds
	}
	var set bodyVectorSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return seeds
	}
	for _, vec := range set.Vectors {
		if len(vec.Body) > 0 {
			seeds = append(seeds, []byte(vec.Body))
		}
	}
	return seeds
}
