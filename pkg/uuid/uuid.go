package uuid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
)

type UUID [16]byte

// GenerateError wraps failures from the randomness source.
type GenerateError struct{ Cause error }

func (e *GenerateError) Error() string {
	if e.Cause == nil {
		return "uuid: generate"
	}
	return "uuid: generate: " + e.Cause.Error()
}
func (e *GenerateError) Unwrap() error { return e.Cause }

// New returns a version-4 UUID sourced from crypto/rand.
func New() (UUID, error) { return generate(rand.Reader) }

// generate is the testable seam: it reads 16 bytes from r and stamps the v4
// version and variant bits.
func generate(r io.Reader) (UUID, error) {
	var u UUID
	if _, err := io.ReadFull(r, u[:]); err != nil {
		return UUID{}, &GenerateError{Cause: err}
	}
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant 10
	return u, nil
}

func (u UUID) String() string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// IsZero reports whether u is the zero UUID (absent / root).
func (u UUID) IsZero() bool { return u == UUID{} }

// ParseError reports a malformed UUID text encoding.
type ParseError struct {
	Input string
	Cause error
}

func (e *ParseError) Error() string {
	if e.Cause == nil {
		return "uuid: parse " + strconv.Quote(e.Input)
	}
	return "uuid: parse " + strconv.Quote(e.Input) + ": " + e.Cause.Error()
}
func (e *ParseError) Unwrap() error { return e.Cause }

// MarshalText encodes u as its canonical 8-4-4-4-12 hyphenated form, so JSON
// (and any other encoding.TextMarshaler consumer) emits a readable string
// rather than a 16-int byte array.
func (u UUID) MarshalText() ([]byte, error) { return []byte(u.String()), nil }

// UnmarshalText parses the canonical 8-4-4-4-12 hyphenated form produced by
// MarshalText/String. Any other shape yields a *ParseError.
func (u *UUID) UnmarshalText(text []byte) error {
	s := string(text)
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return &ParseError{Input: s}
	}
	var hexBuf [32]byte
	copy(hexBuf[0:8], s[0:8])
	copy(hexBuf[8:12], s[9:13])
	copy(hexBuf[12:16], s[14:18])
	copy(hexBuf[16:20], s[19:23])
	copy(hexBuf[20:32], s[24:36])
	var out UUID
	if _, err := hex.Decode(out[:], hexBuf[:]); err != nil {
		return &ParseError{Input: s, Cause: err}
	}
	*u = out
	return nil
}
