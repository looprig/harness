package e2e_test

import (
	"bytes"
	"crypto/mlkem"
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm/e2e"
)

func TestSealOpen_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		plaintext []byte
		info      []byte
		gzip      bool
	}{
		{
			name:      "happy path: gzip=true",
			plaintext: []byte(`{"model":"test","messages":[]}`),
			info:      []byte("e2e-req-v1"),
			gzip:      true,
		},
		{
			name:      "happy path: gzip=false",
			plaintext: []byte(`{"model":"test"}`),
			info:      []byte("e2e-resp-v1"),
			gzip:      false,
		},
		{
			name:      "boundary: empty plaintext gzip=true",
			plaintext: []byte{},
			info:      []byte("e2e-req-v1"),
			gzip:      true,
		},
		{
			name:      "boundary: empty plaintext gzip=false",
			plaintext: []byte{},
			info:      []byte("e2e-req-v1"),
			gzip:      false,
		},
		{
			name:      "edge case: binary plaintext",
			plaintext: []byte{0x00, 0x01, 0x02, 0xff, 0xfe},
			info:      []byte("e2e-req-v1"),
			gzip:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dk, err := mlkem.GenerateKey768()
			if err != nil {
				t.Fatalf("GenerateKey768: %v", err)
			}
			pub := dk.EncapsulationKey().Bytes()

			mlkemCT, blob, err := e2e.Seal(tt.plaintext, pub, tt.info, tt.gzip)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			if len(mlkemCT) != e2e.MLKEMCTSize {
				t.Errorf("mlkemCT = %d bytes, want %d", len(mlkemCT), e2e.MLKEMCTSize)
			}

			shared, err := dk.Decapsulate(mlkemCT)
			if err != nil {
				t.Fatalf("Decapsulate: %v", err)
			}

			got, err := e2e.Open(shared, mlkemCT, blob, tt.info, tt.gzip)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !bytes.Equal(got, tt.plaintext) {
				t.Errorf("Open = %q, want %q", got, tt.plaintext)
			}
		})
	}
}

func TestOpenFrame_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		plaintext []byte
	}{
		{
			name:      "happy path: json chunk",
			plaintext: []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`),
		},
		{
			name:      "boundary: single byte",
			plaintext: []byte("x"),
		},
		{
			name:      "edge case: binary data",
			plaintext: []byte{0x00, 0xff, 0x7f, 0x80},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Use a random 32-byte key (valid ChaCha20-Poly1305 key size).
			dk, err := mlkem.GenerateKey768()
			if err != nil {
				t.Fatalf("GenerateKey768: %v", err)
			}
			pub := dk.EncapsulationKey().Bytes()

			// Derive a key by sealing then opening — this gives us a real key.
			_, blob, err := e2e.Seal([]byte("seed"), pub, []byte("test-key"), false)
			if err != nil {
				t.Fatalf("Seal for key derivation: %v", err)
			}
			// The key derivation is internal; instead derive a stream key
			// via DeriveKey directly.
			shared, sealCT, err2 := func() ([]byte, []byte, error) {
				mlkemCT2, _, _ := e2e.Seal([]byte("x"), pub, []byte("x"), false)
				s, decErr := dk.Decapsulate(mlkemCT2)
				return s, mlkemCT2, decErr
			}()
			if err2 != nil {
				t.Fatalf("setup key derivation: %v", err2)
			}
			_ = blob

			streamKey, err := e2e.DeriveKey(shared, sealCT, []byte("e2e-stream-v1"))
			if err != nil {
				t.Fatalf("DeriveKey: %v", err)
			}

			frame, err := e2e.SealFrame(streamKey, tt.plaintext)
			if err != nil {
				t.Fatalf("SealFrame: %v", err)
			}
			if len(frame) < e2e.NonceSize+e2e.TagSize {
				t.Fatalf("frame too short: %d bytes", len(frame))
			}

			got, err := e2e.OpenFrame(streamKey, frame)
			if err != nil {
				t.Fatalf("OpenFrame: %v", err)
			}
			if !bytes.Equal(got, tt.plaintext) {
				t.Errorf("OpenFrame = %q, want %q", got, tt.plaintext)
			}
		})
	}
}

func TestOpenFrame_ShortBlob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		blob []byte
	}{
		{name: "empty blob", blob: []byte{}},
		{name: "too short for nonce+tag", blob: make([]byte, e2e.NonceSize+e2e.TagSize-1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			key := make([]byte, 32)
			_, err := e2e.OpenFrame(key, tt.blob)
			if err == nil {
				t.Fatal("OpenFrame() want error for short blob, got nil")
			}
			var ee *e2e.Error
			if !errors.As(err, &ee) {
				t.Errorf("want *e2e.Error, got %T: %v", err, err)
			}
		})
	}
}

func TestDeriveKey_ShortMLKEMCT(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mlkemCT []byte
	}{
		{name: "nil mlkemCT", mlkemCT: nil},
		{name: "too short for salt", mlkemCT: make([]byte, e2e.SaltSize-1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := e2e.DeriveKey(make([]byte, 32), tt.mlkemCT, []byte("info"))
			if err == nil {
				t.Fatal("DeriveKey() want error for short mlkemCT, got nil")
			}
		})
	}
}
