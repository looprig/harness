package aci

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzCanonicalize feeds arbitrary bytes through ParseValue + Canonicalize and
// asserts two safety properties:
//
//  1. No panic — neither the parser nor the emitter may crash on hostile input.
//  2. Idempotence — when Canonicalize succeeds, re-parsing its output and
//     re-canonicalizing yields byte-identical output. The canonical form is a
//     fixed point: canonicalizing it again changes nothing.
//
// The seed corpus is drawn from the committed golden vectors plus a handful of
// edge cases (whitespace, nesting, unicode, the float reject) so the fuzzer
// starts from valid, structurally varied inputs.
func FuzzCanonicalize(f *testing.F) {
	for _, seed := range fuzzSeeds(f) {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Property 1: ParseValue must not panic; it may reject the input.
		v, err := ParseValue(data)
		if err != nil {
			return // not valid JSON / not an integer profile — nothing to check.
		}

		// Property 1 (cont.): Canonicalize must not panic. A valid parse of a
		// valid integer profile should not surface a float error, but if it
		// somehow does, that is still a clean (non-panicking) return.
		out, err := Canonicalize(v)
		if err != nil {
			return
		}

		// Property 2: idempotence. Re-parse the canonical bytes and canonicalize
		// again; the result must be identical.
		v2, err := ParseValue(out)
		if err != nil {
			t.Fatalf("canonical output failed to re-parse: %v\noutput: %q", err, out)
		}
		out2, err := Canonicalize(v2)
		if err != nil {
			t.Fatalf("re-canonicalize failed: %v\noutput: %q", err, out)
		}
		if !bytes.Equal(out, out2) {
			t.Fatalf("Canonicalize not idempotent\n first: %q\nsecond: %q", out, out2)
		}
	})
}

// fuzzSeeds returns the seed corpus: every golden-vector input value plus a few
// hand-picked structural edge cases.
func fuzzSeeds(f *testing.F) [][]byte {
	f.Helper()
	seeds := [][]byte{
		[]byte(`{}`),
		[]byte(`[]`),
		[]byte(`{"b":1,"a":2}`),
		[]byte(`[3,1,2]`),
		[]byte(`{"outer":{"b":1,"a":[{"y":2,"x":1}]}}`),
		[]byte(`{"s":"héllo"}`),
		[]byte(`{"s":"a\nb\tc"}`),
		[]byte(`{"𐀀":1,"":2}`),
		[]byte(`  {  "k" : "v" }  `),
		[]byte(`{"n":null,"t":true,"f":false}`),
		[]byte(`18446744073709551615`),
		[]byte(`-9223372036854775808`),
		[]byte(`{"x":1.5}`), // float reject — exercises the error path
	}

	// Add the raw value bytes of each committed golden vector, when readable.
	raw, err := os.ReadFile(filepath.Clean(jcsVectorFile))
	if err != nil {
		return seeds
	}
	var set jcsVectorSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return seeds
	}
	for _, vec := range set.Vectors {
		if len(vec.Value) > 0 {
			seeds = append(seeds, []byte(vec.Value))
		}
	}
	return seeds
}
