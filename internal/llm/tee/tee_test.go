package tee

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestExtractNRASToken(t *testing.T) {
	t.Parallel()

	bareJWT := `"eyJhbGciOiJFUzM4NCJ9.eyJzdWIiOiJ0ZXN0In0.c2lnbmF0dXJl"`
	nestedEAT := func(jwt string) []byte {
		inner, _ := json.Marshal([]interface{}{"JWT", jwt})
		outer, _ := json.Marshal([]json.RawMessage{inner, json.RawMessage(`{"claims":"here"}`)})
		return outer
	}

	cases := []struct {
		name     string
		body     []byte
		want     string
		wantErr  bool
		errReason Reason
	}{
		{
			name: "bare JSON string",
			body: []byte(bareJWT),
			want: "eyJhbGciOiJFUzM4NCJ9.eyJzdWIiOiJ0ZXN0In0.c2lnbmF0dXJl",
		},
		{
			name: "nested EAT array",
			body: nestedEAT("eyJhbGciOiJFUzM4NCJ9.eyJzdWIiOiJ0ZXN0In0.c2lnbmF0dXJl"),
			want: "eyJhbGciOiJFUzM4NCJ9.eyJzdWIiOiJ0ZXN0In0.c2lnbmF0dXJl",
		},
		{
			name:     "empty body",
			body:     []byte{},
			wantErr:  true,
			errReason: ReasonNvidiaVerdictInvalid,
		},
		{
			name:     "whitespace only",
			body:     []byte("   \n\t"),
			wantErr:  true,
			errReason: ReasonNvidiaVerdictInvalid,
		},
		{
			name:     "invalid bare string not valid JSON",
			body:     []byte(`not-a-json-string`),
			wantErr:  true,
			errReason: ReasonNvidiaVerdictInvalid,
		},
		{
			name:     "unexpected leading byte (object)",
			body:     []byte(`{"token":"x"}`),
			wantErr:  true,
			errReason: ReasonNvidiaVerdictInvalid,
		},
		{
			name:     "empty EAT array",
			body:     []byte(`[]`),
			wantErr:  true,
			errReason: ReasonNvidiaVerdictInvalid,
		},
		{
			name:     "EAT array overall token has no JWT element",
			body:     []byte(`[["JWT"]]`),
			wantErr:  true,
			errReason: ReasonNvidiaVerdictInvalid,
		},
		{
			name:     "outer EAT array is not valid JSON",
			body:     []byte(`[not json`),
			wantErr:  true,
			errReason: ReasonNvidiaVerdictInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := extractNRASToken(tc.body)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				var teeErr *Error
				if !asError(err, &teeErr) {
					t.Fatalf("expected *tee.Error, got %T: %v", err, err)
				}
				if teeErr.Reason != tc.errReason {
					t.Errorf("reason: got %q, want %q", teeErr.Reason, tc.errReason)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("token: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGPUEvidenceNonce(t *testing.T) {
	t.Parallel()

	// Build a well-formed fake SPDM MEASUREMENTS blob: 4-byte header + 32-byte nonce.
	makeEvidence := func(nonce [32]byte) string {
		var buf [4 + 32]byte
		binary.BigEndian.PutUint32(buf[:4], 0x11223344) // fake SPDM header
		copy(buf[4:], nonce[:])
		return base64.StdEncoding.EncodeToString(buf[:])
	}

	knownNonce := [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	wantHex := hex.EncodeToString(knownNonce[:])

	cases := []struct {
		name      string
		evidence  string
		want      string
		wantErr   bool
		errReason Reason
	}{
		{
			name:     "valid evidence extracts nonce",
			evidence: makeEvidence(knownNonce),
			want:     wantHex,
		},
		{
			name:      "invalid base64",
			evidence:  "not-valid-base64!!!",
			wantErr:   true,
			errReason: ReasonNvidiaVerdictInvalid,
		},
		{
			name:      "too short after decode — only header, no nonce",
			evidence:  base64.StdEncoding.EncodeToString([]byte{0x11, 0x22, 0x33, 0x44}),
			wantErr:   true,
			errReason: ReasonNvidiaVerdictInvalid,
		},
		{
			name:      "empty string",
			evidence:  "",
			wantErr:   true,
			errReason: ReasonNvidiaVerdictInvalid,
		},
		{
			name:     "minimum valid length (exactly 36 bytes)",
			evidence: makeEvidence([32]byte{}),
			want:     strings.Repeat("00", 32),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := gpuEvidenceNonce(tc.evidence)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				var teeErr *Error
				if !asError(err, &teeErr) {
					t.Fatalf("expected *tee.Error, got %T: %v", err, err)
				}
				if teeErr.Reason != tc.errReason {
					t.Errorf("reason: got %q, want %q", teeErr.Reason, tc.errReason)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("nonce hex: got %q, want %q", got, tc.want)
			}
		})
	}
}

func asError(err error, target **Error) bool {
	return errors.As(err, target)
}

func FuzzExtractNRASToken(f *testing.F) {
	// Bare JSON string.
	f.Add([]byte(`"eyJhbGciOiJFUzM4NCJ9.payload.sig"`))
	// Nested EAT array.
	f.Add([]byte(`[["JWT","eyJhbGciOiJFUzM4NCJ9.payload.sig"],{"sub":"test"}]`))
	// Empty.
	f.Add([]byte(``))
	// Object (unsupported leading byte).
	f.Add([]byte(`{"token":"x"}`))

	f.Fuzz(func(t *testing.T, body []byte) {
		// Must not panic. Error return is fine.
		_, _ = extractNRASToken(body)
	})
}

func FuzzGPUEvidenceNonce(f *testing.F) {
	// Valid base64 of a 36-byte blob.
	valid := make([]byte, 36)
	f.Add(base64.StdEncoding.EncodeToString(valid))
	// Empty.
	f.Add("")
	// Not base64.
	f.Add("not-base64!!!")
	// Base64 of a 3-byte blob (too short).
	f.Add(base64.StdEncoding.EncodeToString([]byte{1, 2, 3}))

	f.Fuzz(func(t *testing.T, evidence string) {
		// Must not panic. Error return is fine.
		_, _ = gpuEvidenceNonce(evidence)
	})
}
