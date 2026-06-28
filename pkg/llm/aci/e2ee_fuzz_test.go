package aci

import (
	"testing"
	"time"

	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// FuzzOpenResponse feeds arbitrary bytes as the sealed response body and asserts
// the one safety property openResponse must hold: it NEVER panics on hostile
// input. A bad parse, a non-hex/short ciphertext, an AAD mismatch, or a replay
// skew are all clean, typed (*llm.AttestationError) returns — never a crash and
// never a partial cleartext. The fuzzer drives ParseBodyValue (the float-tolerant
// parser), the per-field open path, and the field reconstruction all from one
// untrusted blob.
//
// The seed corpus mixes valid sealed-ish shapes (so the dispatch + open path is
// exercised, not just the parse-reject path) with structural edge cases: an empty
// body, a chat body whose content holds non-hex junk, an embedding body, a
// completion body, and an explicit-index multi-choice body.
func FuzzOpenResponse(f *testing.F) {
	// A fixed client key: the fuzzer mutates only the sealed body, so a stable key
	// keeps the open path deterministic for a given input.
	clientPriv := secp256k1.PrivKeyFromBytes(mustHex32(f,
		"2222222222222222222222222222222222222222222222222222222222222222"))

	for _, seed := range openResponseFuzzSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// openResponse must not panic on ANY input; it may return a typed error.
		// now is fixed so the replay window is deterministic across inputs.
		_, _ = openResponse(data, clientPriv, "fuzz-model", "fuzz-nonce", 1750000000,
			time.Unix(1750000000, 0))
	})
}

// mustHex32 decodes a 32-byte hex scalar for the fuzz client key or fails setup.
func mustHex32(f *testing.F, s string) []byte {
	f.Helper()
	b := make([]byte, 32)
	for i := 0; i < 32; i++ {
		var hi, lo byte
		var err error
		if hi, err = hexNibble(s[2*i]); err != nil {
			f.Fatalf("bad hex scalar: %v", err)
		}
		if lo, err = hexNibble(s[2*i+1]); err != nil {
			f.Fatalf("bad hex scalar: %v", err)
		}
		b[i] = hi<<4 | lo
	}
	return b
}

// hexNibble maps a single lowercase-hex byte to its nibble value.
func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	default:
		return 0, &e2eeLengthError{Field: "hex nibble", Got: int(c), Want: 0}
	}
}

// openResponseFuzzSeeds returns the seed corpus: valid-shaped sealed bodies and
// structural edge cases so the fuzzer starts from inputs that reach the dispatch
// and open paths, not only the parse-reject path.
func openResponseFuzzSeeds() [][]byte {
	return [][]byte{
		[]byte(`{}`),
		[]byte(`{"id":"x"}`),
		[]byte(`{"choices":[{"index":0,"message":{"content":"deadbeef"}}]}`),
		[]byte(`{"choices":[{"index":0,"message":{"content":"00","reasoning_content":"zz"}}]}`),
		[]byte(`{"choices":[{"text":"abcd"}]}`),
		[]byte(`{"data":[{"index":0,"embedding":"00112233"}]}`),
		[]byte(`{"data":[{"embedding":"nothex"}]}`),
		[]byte(`{"id":"y","choices":[{"index":2,"message":{"content":"ff"}},{"index":5,"message":{"content":"ee"}}]}`),
		[]byte(`{"id":"z","object":"chat.completion","choices":[],"usage":{"total_tokens":0}}`),
		[]byte(`{"choices":[{"message":{"content":null}}]}`),
		[]byte(`{"data":[{"embedding":[0.1,0.2]}]}`),
		[]byte(`not json`),
	}
}
