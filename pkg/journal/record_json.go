package journal

import (
	"bytes"
	"encoding/json"
	"io"
)

// FenceEncodeError wraps a failure to marshal a LeaseFence to JSON. A LeaseFence
// is a single uint64, so this is effectively unreachable, but the codec returns a
// typed error rather than dropping the json.Marshal error to satisfy the
// errors-are-typed contract at the package API.
type FenceEncodeError struct{ Cause error }

func (e *FenceEncodeError) Error() string {
	return "journal: encode lease fence: " + e.Cause.Error()
}
func (e *FenceEncodeError) Unwrap() error { return e.Cause }

// FenceDecodeError wraps a failure to decode LeaseFence bytes at the untrusted
// restore boundary: malformed JSON, a wrong field type, a non-object, or trailing
// data after the object. The codec fails closed with this typed error so callers
// inspect the cause via errors.As rather than guessing an epoch.
type FenceDecodeError struct {
	Reason string
	Cause  error
}

func (e *FenceDecodeError) Error() string {
	if e.Cause == nil {
		return "journal: decode lease fence: " + e.Reason
	}
	return "journal: decode lease fence: " + e.Reason + ": " + e.Cause.Error()
}
func (e *FenceDecodeError) Unwrap() error { return e.Cause }

// MarshalLeaseFence encodes a LeaseFence as its minimal JSON object {"epoch":N}.
func MarshalLeaseFence(f LeaseFence) ([]byte, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return nil, &FenceEncodeError{Cause: err}
	}
	return data, nil
}

// UnmarshalLeaseFence decodes bytes produced by MarshalLeaseFence. It fails closed
// with a *FenceDecodeError on any malformed input — empty bytes, non-object JSON, a
// non-numeric/negative epoch, an unknown field, or trailing bytes after the
// object. A negative or non-integer epoch fails because Epoch is a uint64.
func UnmarshalLeaseFence(data []byte) (LeaseFence, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f LeaseFence
	if err := dec.Decode(&f); err != nil {
		return LeaseFence{}, &FenceDecodeError{Reason: "invalid json", Cause: err}
	}
	// Reject trailing bytes after the object (e.g. `{"epoch":1}trailing`): a single
	// record decodes to exactly one JSON value with nothing after it.
	if _, err := dec.Token(); err != io.EOF {
		return LeaseFence{}, &FenceDecodeError{Reason: "trailing data after object"}
	}
	return f, nil
}
