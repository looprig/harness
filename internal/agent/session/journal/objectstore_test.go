package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// TestObjectID asserts the content-addressed id is the lowercase hex sha256 of the
// payload, so identical bytes always map to the same object name (idempotent put) and
// different bytes (almost) never collide.
func TestObjectID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "empty", payload: []byte{}},
		{name: "single byte", payload: []byte{0x00}},
		{name: "ascii", payload: []byte("the quick brown fox")},
		{name: "binary", payload: bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 1024)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := objectID(tt.payload)
			sum := sha256.Sum256(tt.payload)
			want := hex.EncodeToString(sum[:])
			if got != want {
				t.Fatalf("objectID = %q, want %q", got, want)
			}
			if len(got) != sha256.Size*2 {
				t.Errorf("objectID length = %d, want %d hex chars", len(got), sha256.Size*2)
			}
			// Lowercase hex only — a valid object-store name and a stable address.
			if got != strings.ToLower(got) {
				t.Errorf("objectID = %q is not lowercase hex", got)
			}
			// Determinism: the same bytes hash to the same id.
			if again := objectID(tt.payload); again != got {
				t.Errorf("objectID not deterministic: %q != %q", again, got)
			}
		})
	}
}

// TestPointerRecordRoundTrip asserts the pointer body codec round-trips a pointer and
// fails closed on malformed or contractually-invalid input (empty bytes, non-object,
// unknown field, trailing data, empty object-id, non-hex object-id, zero length).
func TestPointerRecordRoundTrip(t *testing.T) {
	t.Parallel()
	valid := pointerRecord{ObjectID: strings.Repeat("a", sha256.Size*2), Length: 1234, CodecVersion: pointerCodecVersion}

	tests := []struct {
		name    string
		ptr     pointerRecord // for the encode-then-decode rows
		raw     []byte        // for decode-only rows (nil ⇒ encode ptr first)
		wantErr bool
	}{
		{name: "happy path", ptr: valid},
		{name: "max length", ptr: pointerRecord{ObjectID: strings.Repeat("f", sha256.Size*2), Length: 1 << 40, CodecVersion: pointerCodecVersion}},
		{name: "empty bytes", raw: []byte{}, wantErr: true},
		{name: "not an object", raw: []byte(`"string"`), wantErr: true},
		{name: "unknown field", raw: []byte(`{"object_id":"` + valid.ObjectID + `","length":1,"codec_version":1,"extra":2}`), wantErr: true},
		{name: "trailing data", raw: []byte(`{"object_id":"` + valid.ObjectID + `","length":1,"codec_version":1}junk`), wantErr: true},
		{name: "empty object id", raw: []byte(`{"object_id":"","length":1,"codec_version":1}`), wantErr: true},
		{name: "non-hex object id", raw: []byte(`{"object_id":"` + strings.Repeat("g", sha256.Size*2) + `","length":1,"codec_version":1}`), wantErr: true},
		{name: "short object id", raw: []byte(`{"object_id":"abcd","length":1,"codec_version":1}`), wantErr: true},
		{name: "zero length", raw: []byte(`{"object_id":"` + valid.ObjectID + `","length":0,"codec_version":1}`), wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := tt.raw
			if raw == nil {
				var err error
				raw, err = marshalPointer(tt.ptr)
				if err != nil {
					t.Fatalf("marshalPointer: %v", err)
				}
			}
			got, err := unmarshalPointer(raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("unmarshalPointer() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var decErr *PointerDecodeError
				if !errors.As(err, &decErr) {
					t.Errorf("error %v is not *PointerDecodeError", err)
				}
				return
			}
			if got != tt.ptr {
				t.Errorf("round-trip = %#v, want %#v", got, tt.ptr)
			}
		})
	}
}

// TestInlineThresholdBelowMaxPayload guards the load-bearing invariant: the inline
// threshold must sit strictly below the NATS default max_payload (1 MiB) so any record
// that cannot be inlined is always offloaded, and below the stream inline ceiling.
func TestInlineThresholdBelowMaxPayload(t *testing.T) {
	t.Parallel()
	const natsDefaultMaxPayload = 1 << 20 // 1 MiB, the NATS server default.
	if inlineThreshold >= natsDefaultMaxPayload {
		t.Fatalf("inlineThreshold %d must be < NATS default max_payload %d", inlineThreshold, natsDefaultMaxPayload)
	}
	if inlineThreshold >= streamInlineCeiling {
		t.Errorf("inlineThreshold %d must be < streamInlineCeiling %d", inlineThreshold, streamInlineCeiling)
	}
	if streamInlineCeiling > natsDefaultMaxPayload {
		t.Errorf("streamInlineCeiling %d must be <= NATS default max_payload %d", streamInlineCeiling, natsDefaultMaxPayload)
	}
}

// TestSessionObjectBucket asserts the per-session object bucket name is scoped to the
// session id, is a valid object-store bucket name (NATS bucket regex), and is distinct
// from the session's stream name (so the two never collide).
func TestSessionObjectBucket(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seed byte
	}{
		{name: "zero seed", seed: 0x00},
		{name: "mid seed", seed: 0x7f},
		{name: "high seed", seed: 0xff},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sid := fixedUUID(tt.seed)
			bucket := SessionObjectBucket(sid)
			// Scoped to the session and self-describing.
			if !strings.Contains(bucket, sid.String()) {
				t.Errorf("bucket %q does not contain session id %q", bucket, sid.String())
			}
			// Valid NATS object-store bucket name: ^[a-zA-Z0-9_-]+$.
			for _, r := range bucket {
				ok := r == '_' || r == '-' ||
					(r >= '0' && r <= '9') ||
					(r >= 'a' && r <= 'z') ||
					(r >= 'A' && r <= 'Z')
				if !ok {
					t.Fatalf("bucket %q contains illegal rune %q", bucket, r)
				}
			}
			// Distinct from the stream name.
			if bucket == StreamName(sid) {
				t.Errorf("object bucket %q collides with stream name", bucket)
			}
		})
	}
}
