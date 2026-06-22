package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
)

// fakePutter is a test double for objectPutter. It records the (name, data) of the
// single PutBytes call and returns errPut so the offload path can be driven down both
// its success (errPut == nil) and fail-closed (errPut != nil) arms without a live
// object store. called proves we attempted the upload (so the fail-closed assertion is
// that we tried then failed, not that we skipped the put).
type fakePutter struct {
	errPut  error
	called  bool
	gotName string
	gotData []byte
}

func (f *fakePutter) PutBytes(name string, data []byte, _ ...nats.ObjectOpt) (*nats.ObjectInfo, error) {
	f.called = true
	f.gotName = name
	f.gotData = data
	if f.errPut != nil {
		return nil, f.errPut
	}
	return &nats.ObjectInfo{}, nil
}

// TestBuildOffloadMessage covers the offload path's two outcomes against a fake
// objectPutter: a successful, content-addressed upload that yields a pointer message,
// and an upload failure that fails closed with a typed *RecordTooLargeError (NO pointer
// built or appendable). This is the only coverage of the PutBytes failure arm — the
// whole reason the narrow objectPutter interface and RecordTooLargeError exist.
func TestBuildOffloadMessage(t *testing.T) {
	t.Parallel()

	errPutFailed := errors.New("object store unavailable")

	tests := []struct {
		name    string
		putErr  error // nil ⇒ success row; non-nil ⇒ fail-closed row
		payload []byte
		wantErr bool
	}{
		{
			name:    "success uploads content-addressed and builds pointer",
			payload: bytes.Repeat([]byte{0xab}, inlineThreshold+1),
		},
		{
			name:    "put failure fails closed with RecordTooLargeError",
			putErr:  errPutFailed,
			payload: bytes.Repeat([]byte{0xcd}, inlineThreshold+1),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			putter := &fakePutter{errPut: tt.putErr}
			rec := foreignRecord{subject: "looprig.test.subject", id: "test-msg-id"}

			msg, err := buildOffloadMessage(context.Background(), putter, rec, tt.payload)

			if (err != nil) != tt.wantErr {
				t.Fatalf("buildOffloadMessage() err = %v, wantErr %v", err, tt.wantErr)
			}

			// Both arms must attempt the upload before deciding — upload-before-append.
			if !putter.called {
				t.Fatalf("PutBytes was not called; offload never attempted the upload")
			}

			if tt.wantErr {
				// Fail-closed: nil message, typed *RecordTooLargeError unwrapping to cause.
				if msg != nil {
					t.Errorf("buildOffloadMessage() msg = %v, want nil on failure", msg)
				}
				var tooLarge *RecordTooLargeError
				if !errors.As(err, &tooLarge) {
					t.Fatalf("error %v is not *RecordTooLargeError", err)
				}
				if !errors.Is(err, tt.putErr) {
					t.Errorf("error %v does not unwrap to put sentinel %v", err, tt.putErr)
				}
				if tooLarge.Unwrap() != tt.putErr {
					t.Errorf("RecordTooLargeError.Unwrap() = %v, want %v", tooLarge.Unwrap(), tt.putErr)
				}
				if tooLarge.Subject != rec.Subject() {
					t.Errorf("RecordTooLargeError.Subject = %q, want %q", tooLarge.Subject, rec.Subject())
				}
				if tooLarge.MsgID != rec.IdempotencyID() {
					t.Errorf("RecordTooLargeError.MsgID = %q, want %q", tooLarge.MsgID, rec.IdempotencyID())
				}
				if tooLarge.Length != len(tt.payload) {
					t.Errorf("RecordTooLargeError.Length = %d, want %d", tooLarge.Length, len(tt.payload))
				}
				return
			}

			// Success: the upload is content-addressed (name == hex(sha256(payload))) and
			// carries the exact payload bytes.
			wantID := objectID(tt.payload)
			if putter.gotName != wantID {
				t.Errorf("PutBytes name = %q, want content address %q", putter.gotName, wantID)
			}
			sum := sha256.Sum256(tt.payload)
			if putter.gotName != hex.EncodeToString(sum[:]) {
				t.Errorf("PutBytes name = %q, want hex(sha256(payload))", putter.gotName)
			}
			if !bytes.Equal(putter.gotData, tt.payload) {
				t.Errorf("PutBytes data (%d bytes) != payload (%d bytes)", len(putter.gotData), len(tt.payload))
			}

			// Success: the returned pointer message has the content-id header and a body
			// that decodes to a pointer matching the upload.
			if msg == nil {
				t.Fatalf("buildOffloadMessage() msg = nil, want a pointer message")
			}
			if got := msg.Header.Get(objectIDHeader); got != wantID {
				t.Errorf("%s header = %q, want %q", objectIDHeader, got, wantID)
			}
			if got := msg.Header.Get(nats.MsgIdHdr); got != rec.IdempotencyID() {
				t.Errorf("%s header = %q, want %q", nats.MsgIdHdr, got, rec.IdempotencyID())
			}
			if msg.Subject != rec.Subject() {
				t.Errorf("msg.Subject = %q, want %q", msg.Subject, rec.Subject())
			}
			ptr, decErr := unmarshalPointer(msg.Data)
			if decErr != nil {
				t.Fatalf("unmarshalPointer(msg.Data): %v", decErr)
			}
			want := pointerRecord{ObjectID: wantID, Length: uint64(len(tt.payload)), CodecVersion: pointerCodecVersion}
			if ptr != want {
				t.Errorf("pointer = %#v, want %#v", ptr, want)
			}
		})
	}
}

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
