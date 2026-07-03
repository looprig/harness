package content_test

import (
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/content"
)

// FuzzUnmarshalBlock exercises the untrusted restore boundary with arbitrary
// bytes. It must never panic. When UnmarshalBlock succeeds, re-marshaling and
// re-unmarshaling must be a stable fixed point (deep-equal), proving the codec
// is idempotent on the values it accepts.
func FuzzUnmarshalBlock(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"type":"text","Text":"hello"}`),
		[]byte(`{"type":"thinking","Thinking":"t","Signature":"s"}`),
		[]byte(`{"type":"tool_result","tool_use_id":"tu","content":[{"type":"text","Text":"x"}]}`),
		[]byte(`{"type":"image","MediaType":"image/png","Source":{"URL":"u"}}`),
		[]byte(`{"type":"video"}`),
		[]byte(`not json`),
		[]byte(``),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := content.UnmarshalBlock(data)
		if err != nil {
			return // rejected input is fine; only crashes fail the fuzz
		}

		out, err := content.MarshalBlock(b)
		if err != nil {
			t.Fatalf("re-MarshalBlock of accepted block failed: %v", err)
		}
		b2, err := content.UnmarshalBlock(out)
		if err != nil {
			t.Fatalf("re-UnmarshalBlock of re-marshaled bytes failed: %v", err)
		}
		if !reflect.DeepEqual(b, b2) {
			t.Fatalf("codec not stable: first = %#v, second = %#v", b, b2)
		}
	})
}
