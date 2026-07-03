package sessionstore

import (
	"bytes"
	"errors"
	"testing"
)

// envelopesEqual reports whether two envelopes carry the same frame fields. Body
// is compared byte-wise (bytes.Equal treats nil and empty as equal, which is the
// intended "same bytes" semantics for a body that may be empty).
func envelopesEqual(a, b envelope) bool {
	return a.V == b.V && a.Kind == b.Kind && a.ID == b.ID && bytes.Equal(a.Body, b.Body)
}

func TestEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  envelope
	}{
		{
			name: "event kind",
			env:  envelope{V: envelopeVersion, Kind: string(kindEvent), ID: "e1", Body: []byte("event-body")},
		},
		{
			name: "command kind",
			env:  envelope{V: envelopeVersion, Kind: string(kindCommand), ID: "c1", Body: []byte(`{"cmd":"interrupt"}`)},
		},
		{
			name: "fence kind",
			env:  envelope{V: envelopeVersion, Kind: string(kindFence), ID: "7", Body: []byte(`{"epoch":7}`)},
		},
		{
			name: "blobptr kind",
			env:  envelope{V: envelopeVersion, Kind: string(kindBlobPtr), ID: "b1", Body: []byte(`{"key":"k","size":3,"sha256":"abc"}`)},
		},
		{
			name: "empty body (nil)",
			env:  envelope{V: envelopeVersion, Kind: string(kindEvent), ID: "e2", Body: nil},
		},
		{
			name: "empty body (non-nil zero length)",
			env:  envelope{V: envelopeVersion, Kind: string(kindEvent), ID: "e3", Body: []byte{}},
		},
		{
			name: "body with arbitrary binary bytes",
			env:  envelope{V: envelopeVersion, Kind: string(kindCommand), ID: "c2", Body: []byte{0x00, 0x01, 0xff, 0x7f, 0x80}},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, err := encodeEnvelope(tt.env)
			if err != nil {
				t.Fatalf("encodeEnvelope() error = %v, want nil", err)
			}

			got, err := decodeEnvelope(b)
			if err != nil {
				t.Fatalf("decodeEnvelope() error = %v, want nil", err)
			}

			if !envelopesEqual(got, tt.env) {
				t.Errorf("round-trip mismatch:\n got = %+v\nwant = %+v", got, tt.env)
			}
			if !bytes.Equal(got.Body, tt.env.Body) {
				t.Errorf("Body not preserved: got %v, want %v", got.Body, tt.env.Body)
			}
		})
	}
}

func TestEnvelopeDecodeRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     []byte
		wantCause bool // true when the rejection wraps an underlying JSON error
	}{
		{
			name:      "unknown kind",
			input:     []byte(`{"v":1,"kind":"bogus","id":"x","body":null}`),
			wantCause: false,
		},
		{
			name:      "empty kind",
			input:     []byte(`{"v":1,"kind":"","id":"x","body":null}`),
			wantCause: false,
		},
		{
			name:      "future version",
			input:     []byte(`{"v":2,"kind":"event","id":"x","body":null}`),
			wantCause: false,
		},
		{
			name:      "zero version (missing v)",
			input:     []byte(`{"kind":"event","id":"x","body":null}`),
			wantCause: false,
		},
		{
			name:      "negative version",
			input:     []byte(`{"v":-1,"kind":"event","id":"x","body":null}`),
			wantCause: false,
		},
		{
			name:      "truncated frame",
			input:     []byte(`{"v":1,"kind":"event","id":"e1","bod`),
			wantCause: true,
		},
		{
			name:      "non-JSON garbage",
			input:     []byte{0x00, 0x01, 0x02, 'n', 'o', 'p', 'e'},
			wantCause: true,
		},
		{
			name:      "empty input",
			input:     []byte(``),
			wantCause: true,
		},
		{
			name:      "wrong type for v",
			input:     []byte(`{"v":"1","kind":"event","id":"x","body":null}`),
			wantCause: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := decodeEnvelope(tt.input)
			if err == nil {
				t.Fatalf("decodeEnvelope() error = nil, want *EnvelopeError")
			}

			var envErr *EnvelopeError
			if !errors.As(err, &envErr) {
				t.Fatalf("decodeEnvelope() error = %T (%v), want *EnvelopeError", err, err)
			}

			cause := errors.Unwrap(err)
			if tt.wantCause && cause == nil {
				t.Errorf("EnvelopeError.Unwrap() = nil, want non-nil JSON cause")
			}
			if !tt.wantCause && cause != nil {
				t.Errorf("EnvelopeError.Unwrap() = %v, want nil (semantic rejection has no cause)", cause)
			}
		})
	}
}

func TestEncodeEnvelopeRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  envelope
	}{
		{
			name: "unknown kind fails closed on encode",
			env:  envelope{V: envelopeVersion, Kind: "bogus", ID: "x", Body: nil},
		},
		{
			name: "unset version fails closed on encode",
			env:  envelope{Kind: string(kindEvent), ID: "x", Body: nil},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := encodeEnvelope(tt.env)
			if err == nil {
				t.Fatalf("encodeEnvelope() error = nil, want *EnvelopeError")
			}
			var envErr *EnvelopeError
			if !errors.As(err, &envErr) {
				t.Fatalf("encodeEnvelope() error = %T (%v), want *EnvelopeError", err, err)
			}
		})
	}
}

func TestBlobPointerRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ptr  blobPointer
	}{
		{
			name: "populated pointer",
			ptr:  blobPointer{Key: "blobs/session/abc", Size: 4096, SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		},
		{
			name: "zero-size pointer",
			ptr:  blobPointer{Key: "blobs/empty", Size: 0, SHA256: ""},
		},
		{
			name: "empty pointer",
			ptr:  blobPointer{},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, err := encodeBlobPointer(tt.ptr)
			if err != nil {
				t.Fatalf("encodeBlobPointer() error = %v, want nil", err)
			}
			got, err := decodeBlobPointer(b)
			if err != nil {
				t.Fatalf("decodeBlobPointer() error = %v, want nil", err)
			}
			if got != tt.ptr {
				t.Errorf("round-trip mismatch: got %+v, want %+v", got, tt.ptr)
			}
		})
	}
}

// TestBlobPtrEnvelopeRoundTrip verifies a blobptr envelope carries the blobPointer
// JSON as its Body and survives a full envelope round-trip.
func TestBlobPtrEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()

	ptr := blobPointer{Key: "blobs/session/xyz", Size: 1 << 20, SHA256: "deadbeef"}

	body, err := encodeBlobPointer(ptr)
	if err != nil {
		t.Fatalf("encodeBlobPointer() error = %v", err)
	}

	env := envelope{V: envelopeVersion, Kind: string(kindBlobPtr), ID: "blob-1", Body: body}
	frame, err := encodeEnvelope(env)
	if err != nil {
		t.Fatalf("encodeEnvelope() error = %v", err)
	}

	gotEnv, err := decodeEnvelope(frame)
	if err != nil {
		t.Fatalf("decodeEnvelope() error = %v", err)
	}
	if gotEnv.Kind != string(kindBlobPtr) {
		t.Errorf("Kind = %q, want %q", gotEnv.Kind, kindBlobPtr)
	}

	gotPtr, err := decodeBlobPointer(gotEnv.Body)
	if err != nil {
		t.Fatalf("decodeBlobPointer(env.Body) error = %v", err)
	}
	if gotPtr != ptr {
		t.Errorf("blobPointer round-trip mismatch: got %+v, want %+v", gotPtr, ptr)
	}
}

func TestBlobPointerDecodeRejectsGarbage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []byte
	}{
		{name: "truncated", input: []byte(`{"key":"k","siz`)},
		{name: "non-JSON", input: []byte{0xff, 0xfe, 0x00}},
		{name: "empty", input: []byte(``)},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := decodeBlobPointer(tt.input)
			if err == nil {
				t.Fatalf("decodeBlobPointer() error = nil, want *EnvelopeError")
			}
			var envErr *EnvelopeError
			if !errors.As(err, &envErr) {
				t.Fatalf("decodeBlobPointer() error = %T (%v), want *EnvelopeError", err, err)
			}
			if errors.Unwrap(err) == nil {
				t.Errorf("EnvelopeError.Unwrap() = nil, want non-nil JSON cause")
			}
		})
	}
}
