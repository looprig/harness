package loopruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// TestCaptureSinkCountsEveryOfferedByteAndRetainsTheCeiling enumerates a space of
// (ceiling, payload size) pairs rather than pinning one pair: the property is
// "for all sizes, captured = min(size, ceiling) and offered = size", and a single
// fixture would let a mutant that returned the ceiling, the payload length, or a
// constant agree with it.
func TestCaptureSinkCountsEveryOfferedByteAndRetainsTheCeiling(t *testing.T) {
	t.Parallel()
	for _, ceiling := range []int{1, 2, 7, 16, 64} {
		for _, size := range []int{0, 1, 6, 7, 8, 15, 16, 17, 100} {
			payload := strings.Repeat("a", size)
			sink := newCaptureSink(ceiling)
			n, err := sink.Write([]byte(payload))
			if err != nil || n != size {
				t.Fatalf("ceiling=%d size=%d: Write = (%d, %v), want (%d, nil)", ceiling, size, n, err, size)
			}
			wantCaptured := min(size, ceiling)
			if got := sink.capturedBytes(); got != uint64(wantCaptured) {
				t.Errorf("ceiling=%d size=%d: capturedBytes = %d, want %d", ceiling, size, got, wantCaptured)
			}
			if got := sink.offeredBytes(); got != uint64(size) {
				t.Errorf("ceiling=%d size=%d: offeredBytes = %d, want %d", ceiling, size, got, size)
			}
			if got, want := sink.truncated(), size > ceiling; got != want {
				t.Errorf("ceiling=%d size=%d: truncated = %v, want %v", ceiling, size, got, want)
			}
			if got := string(sink.bytes()); got != payload[:wantCaptured] {
				t.Errorf("ceiling=%d size=%d: bytes = %q, want %q", ceiling, size, got, payload[:wantCaptured])
			}
			sum := sha256.Sum256([]byte(payload[:wantCaptured]))
			if got, want := sink.digestHex(), hex.EncodeToString(sum[:]); got != want {
				t.Errorf("ceiling=%d size=%d: digestHex = %s, want %s", ceiling, size, got, want)
			}
		}
	}
}

// TestCaptureSinkAccumulatesAcrossWrites pins that the ceiling applies to the
// running total, not to each Write: an incremental producer must not be able to
// exceed the ceiling by writing in pieces.
func TestCaptureSinkAccumulatesAcrossWrites(t *testing.T) {
	t.Parallel()
	sink := newCaptureSink(5)
	for i := 0; i < 4; i++ {
		if _, err := sink.Write([]byte("abc")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if got := string(sink.bytes()); got != "abcab" {
		t.Fatalf("bytes = %q, want %q", got, "abcab")
	}
	if got := sink.offeredBytes(); got != 12 {
		t.Fatalf("offeredBytes = %d, want 12", got)
	}
	if !sink.truncated() {
		t.Fatal("truncated = false, want true")
	}
}

// TestCaptureSinkEncodingClassifiesTheRetainedBytes covers both directions,
// including the boundary the doc comment calls out: a valid text result cut
// mid-rune by the ceiling is reported as binary, because that is what the stored
// object contains.
func TestCaptureSinkEncodingClassifiesTheRetainedBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ceiling int
		payload string
		want    event.ToolResultEncoding
	}{
		{"empty is utf-8", 8, "", event.ToolResultEncodingUTF8},
		{"ascii is utf-8", 8, "hello", event.ToolResultEncodingUTF8},
		{"multibyte kept whole is utf-8", 8, "界界", event.ToolResultEncodingUTF8},
		{"multibyte cut on a boundary is utf-8", 3, "界界", event.ToolResultEncodingUTF8},
		{"multibyte cut mid-rune is binary", 4, "界界", event.ToolResultEncodingBinary},
		{"invalid bytes are binary", 8, "\xff\xfe", event.ToolResultEncodingBinary},
		{"invalid bytes past the ceiling do not count", 2, "ab\xff", event.ToolResultEncodingUTF8},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sink := newCaptureSink(tt.ceiling)
			if _, err := sink.Write([]byte(tt.payload)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if got := sink.encoding(); got != tt.want {
				t.Fatalf("encoding = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestToolResultCaptureCeilingTakesTheSmallerBound enumerates both orderings
// and the tie. A fixture that only ever set the two bounds equal could not tell
// min from either projection.
func TestToolResultCaptureCeilingTakesTheSmallerBound(t *testing.T) {
	t.Parallel()
	for _, capture := range []int{256, 1024, 4096} {
		for _, materialized := range []int{256, 1024, 4096} {
			ts := ToolSet{MaxToolResultCaptureBytes: capture, MaxMaterializedToolResultBytes: materialized}
			want := min(capture, materialized)
			if got := materializedCaptureCeiling(ts); got != want {
				t.Errorf("materializedCaptureCeiling(capture=%d, materialized=%d) = %d, want %d", capture, materialized, got, want)
			}
		}
	}
}

// TestToolResultCaptureBoundsAreDefaulted pins that neither capture bound
// can resolve to zero, which would make an oversized result unreachable while a
// shaped preview still committed.
func TestToolResultCaptureBoundsAreDefaulted(t *testing.T) {
	t.Parallel()
	for _, in := range []int{-1, 0} {
		got := resolveToolSetCaps(ToolSet{MaxToolResultCaptureBytes: in, MaxMaterializedToolResultBytes: in})
		if got.MaxToolResultCaptureBytes != loop.DefaultToolResultCaptureBytes {
			t.Errorf("in=%d MaxToolResultCaptureBytes = %d, want %d", in, got.MaxToolResultCaptureBytes, loop.DefaultToolResultCaptureBytes)
		}
		if got.MaxMaterializedToolResultBytes != defaultMaxMaterializedToolResultBytes {
			t.Errorf("in=%d MaxMaterializedToolResultBytes = %d, want %d", in, got.MaxMaterializedToolResultBytes, defaultMaxMaterializedToolResultBytes)
		}
	}
	got := resolveToolSetCaps(ToolSet{MaxToolResultCaptureBytes: 4096, MaxMaterializedToolResultBytes: 8192})
	if got.MaxToolResultCaptureBytes != 4096 || got.MaxMaterializedToolResultBytes != 8192 {
		t.Fatalf("declared bounds were overwritten: %+v", got)
	}
}
