package uuid

import (
	"crypto/rand"
	"fmt"
	"io"
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
