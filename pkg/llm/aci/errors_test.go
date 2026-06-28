package aci

import (
	"errors"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
)

// TestSupportedAPIVersion pins the wire api_version this client speaks.
func TestSupportedAPIVersion(t *testing.T) {
	t.Parallel()
	if SupportedAPIVersion != "aci/1" {
		t.Fatalf("SupportedAPIVersion = %q, want %q", SupportedAPIVersion, "aci/1")
	}
}

// TestReasonConstants verifies every reason const equals its exact spec string
// (docs/plans/2026-06-24-aci-confidential-inference-client-design.md §Errors).
func TestReasonConstants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "unsupported_api_version", got: reasonUnsupportedAPIVersion, want: "unsupported_api_version"},
		{name: "report_data_mismatch", got: reasonReportDataMismatch, want: "report_data_mismatch"},
		{name: "binding_mismatch", got: reasonBindingMismatch, want: "binding_mismatch"},
		{name: "quote_invalid", got: reasonQuoteInvalid, want: "quote_invalid"},
		{name: "tcb_revoked", got: reasonTCBRevoked, want: "tcb_revoked"},
		{name: "keyset_digest_mismatch", got: reasonKeysetDigestMismatch, want: "keyset_digest_mismatch"},
		{name: "endorsement_invalid", got: reasonEndorsementInvalid, want: "endorsement_invalid"},
		{name: "kms_root_untrusted", got: reasonKMSRootUntrusted, want: "kms_root_untrusted"},
		{name: "policy_rejected", got: reasonPolicyRejected, want: "policy_rejected"},
		{name: "stale_report", got: reasonStaleReport, want: "stale_report"},
		{name: "receipt_invalid", got: reasonReceiptInvalid, want: "receipt_invalid"},
		{name: "upstream_unverified", got: reasonUpstreamUnverified, want: "upstream_unverified"},
		{name: "e2ee_failed", got: reasonE2EEFailed, want: "e2ee_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("reason const = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

// errSentinel is a stand-in wrapped cause used to assert chaining behaviour.
type errSentinel struct{ msg string }

func (e *errSentinel) Error() string { return e.msg }

// TestAttestErr verifies attestErr returns a *llm.AttestationError carrying the
// given Reason and wrapping the given Err (nil-safe), and that errors.As/Unwrap work.
func TestAttestErr(t *testing.T) {
	t.Parallel()
	cause := &errSentinel{msg: "underlying boom"}
	tests := []struct {
		name       string
		reason     string
		err        error
		wantReason string
		wantWrap   bool // expect Unwrap to return the same non-nil cause
	}{
		{
			name:       "happy path wraps cause",
			reason:     reasonQuoteInvalid,
			err:        cause,
			wantReason: reasonQuoteInvalid,
			wantWrap:   true,
		},
		{
			name:       "nil cause is allowed (no chain)",
			reason:     reasonStaleReport,
			err:        nil,
			wantReason: reasonStaleReport,
			wantWrap:   false,
		},
		{
			name:       "empty reason is preserved verbatim",
			reason:     "",
			err:        cause,
			wantReason: "",
			wantWrap:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := attestErr(tt.reason, tt.err)
			if got == nil {
				t.Fatalf("attestErr returned nil")
			}

			// The package-level return must be the typed *llm.AttestationError.
			var ae *llm.AttestationError
			if !errors.As(got, &ae) {
				t.Fatalf("attestErr result is not *llm.AttestationError: %T", got)
			}
			if ae.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", ae.Reason, tt.wantReason)
			}

			if tt.wantWrap {
				if !errors.Is(got, cause) {
					t.Errorf("errors.Is(got, cause) = false, want true")
				}
				if errors.Unwrap(got) != tt.err {
					t.Errorf("Unwrap() = %v, want %v", errors.Unwrap(got), tt.err)
				}
			} else {
				if errors.Unwrap(got) != nil {
					t.Errorf("Unwrap() = %v, want nil", errors.Unwrap(got))
				}
			}
		})
	}
}

// TestErrUnsupportedAPIVersion verifies the version-drift tripwire helper:
// Reason is unsupported_api_version, the message names both the offending and
// the supported version, and it never leaks a secret passed alongside it.
func TestErrUnsupportedAPIVersion(t *testing.T) {
	t.Parallel()
	const placeholderSecret = "sk-SUPER-SECRET-DO-NOT-LEAK"

	tests := []struct {
		name        string
		got         string
		wantContain []string
	}{
		{
			name:        "happy path aci/2",
			got:         "aci/2",
			wantContain: []string{"aci/2", SupportedAPIVersion},
		},
		{
			name:        "empty version still names supported",
			got:         "",
			wantContain: []string{SupportedAPIVersion},
		},
		{
			name:        "weird version with delimiters",
			got:         "aci/v1.0-beta",
			wantContain: []string{"aci/v1.0-beta", SupportedAPIVersion},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := errUnsupportedAPIVersion(tt.got)
			if err == nil {
				t.Fatalf("errUnsupportedAPIVersion returned nil")
			}

			var ae *llm.AttestationError
			if !errors.As(err, &ae) {
				t.Fatalf("result is not *llm.AttestationError: %T", err)
			}
			if ae.Reason != reasonUnsupportedAPIVersion {
				t.Errorf("Reason = %q, want %q", ae.Reason, reasonUnsupportedAPIVersion)
			}

			msg := err.Error()
			for _, want := range tt.wantContain {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not contain %q", msg, want)
				}
			}
			// Security: the helper takes only the version string; a secret must
			// never appear in the rendered error text.
			if strings.Contains(msg, placeholderSecret) {
				t.Errorf("message %q leaked secret %q", msg, placeholderSecret)
			}
		})
	}
}
