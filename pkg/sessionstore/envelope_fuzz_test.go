package sessionstore

import (
	"bytes"
	"testing"
)

// FuzzEnvelopeDecode drives decodeEnvelope with arbitrary bytes. It asserts two
// invariants: decode never panics on any input (fail closed, not crash), and any
// input that decodes successfully re-encodes to bytes that decode again to an
// identical frame (round-trip stability — the codec has no lossy accepting path).
func FuzzEnvelopeDecode(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"v":1,"kind":"event","id":"e1","body":"aGVsbG8="}`),
		[]byte(`{"v":1,"kind":"command","id":"c1","body":null}`),
		[]byte(`{"v":1,"kind":"fence","id":"7","body":""}`),
		[]byte(`{"v":1,"kind":"blobptr","id":"b1","body":"e30="}`),
		[]byte(`{"v":1,"kind":"event","id":"","body":null}`),
		[]byte(`{"v":2,"kind":"event","id":"x","body":null}`),
		[]byte(`{"v":0,"kind":"event","id":"x","body":null}`),
		[]byte(`{"v":1,"kind":"bogus","id":"x","body":null}`),
		[]byte(`{"v":1,"kind":"","id":"x","body":null}`),
		[]byte(`{"v":"1","kind":"event","id":"x","body":null}`),
		[]byte(`{"v":1,"kind":"event"`),
		[]byte(`not json at all`),
		[]byte(`{`),
		[]byte(``),
		{0x00, 0x01, 0x02, 0xff},
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		env, err := decodeEnvelope(data)
		if err != nil {
			return // rejection is an acceptable outcome; the invariant is "no panic"
		}

		// A successfully decoded frame must re-encode (encode validates, and a
		// decoded frame is valid by construction) and decode again identically.
		reframed, err := encodeEnvelope(env)
		if err != nil {
			t.Fatalf("re-encode of decoded envelope failed: %v (env=%+v)", err, env)
		}
		env2, err := decodeEnvelope(reframed)
		if err != nil {
			t.Fatalf("re-decode of re-encoded envelope failed: %v (env=%+v)", err, env)
		}
		if env2.V != env.V || env2.Kind != env.Kind || env2.ID != env.ID || !bytes.Equal(env2.Body, env.Body) {
			t.Fatalf("round-trip not stable:\n first = %+v\nsecond = %+v", env, env2)
		}
	})
}
