package llm_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/llm"
)

// Compile-time assertions that all error types satisfy the error interface.
// These are intentionally outside the table-driven pattern — they carry no runtime behavior.
var (
	_ error = (*llm.NetworkError)(nil)
	_ error = (*llm.APIError)(nil)
	_ error = (*llm.ValidationError)(nil)
	_ error = (*llm.AttestationError)(nil)
)

func TestNetworkError(t *testing.T) {
	t.Parallel()

	inner := errors.New("connection refused")

	tests := []struct {
		name         string
		err          *llm.NetworkError
		wantContains string
		wantUnwrap   error
	}{
		{
			name:         "wraps inner error message in output",
			err:          &llm.NetworkError{Err: inner},
			wantContains: "connection refused",
			wantUnwrap:   inner,
		},
		{
			name:         "error string has llm prefix",
			err:          &llm.NetworkError{Err: inner},
			wantContains: "llm:",
			wantUnwrap:   inner,
		},
		{
			name:         "wraps a wrapped error and unwrap chain works",
			err:          &llm.NetworkError{Err: fmt.Errorf("dial tcp: %w", inner)},
			wantContains: "dial tcp",
			wantUnwrap:   nil, // we check errors.Is separately below
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.err.Error()
			if got == "" {
				t.Fatalf("Error() returned empty string")
			}
			if !strings.Contains(got, tt.wantContains) {
				t.Errorf("Error() = %q, want it to contain %q", got, tt.wantContains)
			}
			if tt.wantUnwrap != nil {
				if !errors.Is(tt.err, tt.wantUnwrap) {
					t.Errorf("errors.Is(%v, %v) = false, want true", tt.err, tt.wantUnwrap)
				}
			}
		})
	}

	// Separate table for Unwrap chain through nested wrapping.
	t.Run("errors.Is traverses full unwrap chain", func(t *testing.T) {
		t.Parallel()

		wrapped := fmt.Errorf("dial tcp: %w", inner)
		ne := &llm.NetworkError{Err: wrapped}
		if !errors.Is(ne, inner) {
			t.Errorf("errors.Is(NetworkError{Err: fmt.Errorf(\"...: inner\")}, inner) = false, want true")
		}
	})
}

func TestNetworkError_NilErrPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on NetworkError{Err: nil}.Error(), got none")
		}
	}()
	e := &llm.NetworkError{Err: nil}
	_ = e.Error() // must panic
}

func TestAPIError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          *llm.APIError
		wantContains []string
	}{
		{
			name:         "status 200 with message",
			err:          &llm.APIError{Status: 200, Message: "ok"},
			wantContains: []string{"200", "ok"},
		},
		{
			name:         "status 429 rate limited",
			err:          &llm.APIError{Status: 429, Message: "rate limited"},
			wantContains: []string{"429", "rate limited"},
		},
		{
			name:         "status 500 server error",
			err:          &llm.APIError{Status: 500, Message: "internal server error"},
			wantContains: []string{"500", "internal server error"},
		},
		{
			name:         "empty message boundary",
			err:          &llm.APIError{Status: 503, Message: ""},
			wantContains: []string{"503"},
		},
		{
			name:         "error string has llm prefix",
			err:          &llm.APIError{Status: 400, Message: "bad request"},
			wantContains: []string{"llm:"},
		},
		{
			name:         "nil body is accepted without panic",
			err:          &llm.APIError{Status: 404, Message: "not found", Body: nil},
			wantContains: []string{"404", "not found"},
		},
		{
			name:         "non-nil body does not affect Error() output",
			err:          &llm.APIError{Status: 401, Message: "unauthorized", Body: []byte(`{"error":"auth"}`)},
			wantContains: []string{"401", "unauthorized"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.err.Error()
			if got == "" {
				t.Fatalf("Error() returned empty string")
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("Error() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestValidationError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          *llm.ValidationError
		wantContains []string
	}{
		{
			name:         "field and reason both present",
			err:          &llm.ValidationError{Field: "temperature", Reason: "must be between 0 and 1"},
			wantContains: []string{"temperature", "must be between 0 and 1"},
		},
		{
			name:         "error string has llm prefix",
			err:          &llm.ValidationError{Field: "model", Reason: "unknown model"},
			wantContains: []string{"llm:"},
		},
		{
			name:         "empty field boundary",
			err:          &llm.ValidationError{Field: "", Reason: "missing required field"},
			wantContains: []string{"missing required field"},
		},
		{
			name:         "empty reason boundary",
			err:          &llm.ValidationError{Field: "max_tokens", Reason: ""},
			wantContains: []string{"max_tokens"},
		},
		{
			name:         "both empty boundary produces non-empty string",
			err:          &llm.ValidationError{Field: "", Reason: ""},
			wantContains: []string{"llm:"},
		},
		{
			name:         "messages field with long reason",
			err:          &llm.ValidationError{Field: "messages", Reason: "must not be empty"},
			wantContains: []string{"messages", "must not be empty"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.err.Error()
			if got == "" {
				t.Fatalf("Error() returned empty string")
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("Error() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestAttestationError(t *testing.T) {
	t.Parallel()

	inner := errors.New("quote verification failed")

	tests := []struct {
		name         string
		err          *llm.AttestationError
		wantContains []string
		wantUnwrap   error // nil means Unwrap() should return nil
		checkUnwrap  bool  // whether to assert on wantUnwrap
	}{
		{
			name:         "nil inner error uses reason only",
			err:          &llm.AttestationError{Reason: "nonce mismatch", Err: nil},
			wantContains: []string{"llm:", "nonce mismatch"},
			wantUnwrap:   nil,
			checkUnwrap:  true,
		},
		{
			name:         "non-nil inner error includes both reason and inner message",
			err:          &llm.AttestationError{Reason: "quote invalid", Err: inner},
			wantContains: []string{"llm:", "quote invalid", "quote verification failed"},
			wantUnwrap:   inner,
			checkUnwrap:  true,
		},
		{
			name:         "errors.Is unwraps to inner when Err is set",
			err:          &llm.AttestationError{Reason: "pcr mismatch", Err: inner},
			wantContains: []string{"pcr mismatch"},
			wantUnwrap:   inner,
			checkUnwrap:  true,
		},
		{
			name:         "error string has llm prefix regardless of Err",
			err:          &llm.AttestationError{Reason: "expired certificate", Err: nil},
			wantContains: []string{"llm:"},
			wantUnwrap:   nil,
			checkUnwrap:  false,
		},
		{
			name:         "attestation prefix present in error string",
			err:          &llm.AttestationError{Reason: "some reason", Err: nil},
			wantContains: []string{"attestation"},
			wantUnwrap:   nil,
			checkUnwrap:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.err.Error()
			if got == "" {
				t.Fatalf("Error() returned empty string")
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("Error() = %q, want it to contain %q", got, want)
				}
			}

			if tt.checkUnwrap {
				unwrapped := tt.err.Unwrap()
				if unwrapped != tt.wantUnwrap {
					t.Errorf("Unwrap() = %v, want %v", unwrapped, tt.wantUnwrap)
				}
				if tt.wantUnwrap != nil && !errors.Is(tt.err, tt.wantUnwrap) {
					t.Errorf("errors.Is(%v, %v) = false, want true", tt.err, tt.wantUnwrap)
				}
			}
		})
	}
}
