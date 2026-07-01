package aci

import "testing"

// FuzzParseReceipt feeds arbitrary bytes into ParseReceipt and, when parsing
// succeeds, runs canonicalReceipt on the parsed result. Neither must ever panic:
// ParseReceipt and canonicalReceipt both ingest untrusted, attacker-controlled
// receipt JSON off the wire, so they must fail (return a typed error) rather than
// crash. The corpus seeds a few well-formed and malformed shapes; the fuzzer
// explores the rest.
func FuzzParseReceipt(f *testing.F) {
	seeds := [][]byte{
		[]byte(""),
		[]byte("{}"),
		[]byte("not json"),
		[]byte(`{"api_version":"aci/1"}`),
		[]byte(`{"chat_id":null,"served_at":1,"event_log":[]}`),
		[]byte(`{"event_log":[{"seq":0,"type":"x","f":1}],"signature":{"algo":"ed25519","key_id":"k","value":"00"}}`),
		[]byte(`{"served_at":18446744073709551615,"event_log":[{"seq":1,"type":"t"}]}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := ParseReceipt(data)
		if err != nil {
			return
		}
		// A successfully parsed receipt must canonicalize without panicking; a
		// canonicalization error (e.g. a non-integer event field) is acceptable,
		// a panic is not.
		_, _ = canonicalReceipt(r)
	})
}
