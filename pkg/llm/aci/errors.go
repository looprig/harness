// Package aci implements a client for the Dstack private-ai-gateway "aci/1"
// confidential-inference protocol. This file defines the attestation failure
// reasons and the typed errors the package returns.
//
// Every attestation failure is reported as the provider-neutral, fail-closed
// *llm.AttestationError (defined in pkg/llm). This package does not introduce a
// new exported error type; it supplies the reason strings and thin constructors.
package aci

import "github.com/ciram-co/looprig/pkg/llm"

// SupportedAPIVersion is the only Dstack ACI wire api_version this client
// speaks. A report declaring any other version is rejected
// (reasonUnsupportedAPIVersion) — the version-drift tripwire.
const SupportedAPIVersion = "aci/1"

// Attestation failure reasons. Each value is the exact spec string from
// docs/plans/2026-06-24-aci-confidential-inference-client-design.md §Errors and
// becomes the Reason field of the returned *llm.AttestationError. They are
// unexported: they are consumed across the aci package, not by external callers.
const (
	reasonUnsupportedAPIVersion = "unsupported_api_version"
	reasonReportDataMismatch    = "report_data_mismatch"
	reasonBindingMismatch       = "binding_mismatch"
	reasonQuoteInvalid          = "quote_invalid"
	reasonTCBRevoked            = "tcb_revoked"
	reasonKeysetDigestMismatch  = "keyset_digest_mismatch"
	reasonEndorsementInvalid    = "endorsement_invalid"
	reasonKMSRootUntrusted      = "kms_root_untrusted"
	reasonPolicyRejected        = "policy_rejected"
	reasonStaleReport           = "stale_report"
	reasonReceiptInvalid        = "receipt_invalid"
	reasonUpstreamUnverified    = "upstream_unverified"
	reasonE2EEFailed            = "e2ee_failed"
)

// attestErr builds the fail-closed *llm.AttestationError returned everywhere in
// this package. reason should be one of the reason* constants; err is the
// underlying cause and may be nil (the AttestationError chains it via Unwrap).
func attestErr(reason string, err error) *llm.AttestationError {
	return &llm.AttestationError{Reason: reason, Err: err}
}

// apiVersionMismatchError is the unexported, typed cause wrapped inside the
// unsupported_api_version AttestationError. It carries the offending version and
// the supported one as distinct fields so the cause keeps type identity (per
// CLAUDE.md: no bare fmt.Errorf from package APIs) and so neither the API key nor
// any plaintext can ever reach the rendered message — only version strings do.
type apiVersionMismatchError struct {
	Got  string
	Want string
}

func (e *apiVersionMismatchError) Error() string {
	return "api_version " + e.Got + " is not supported, want " + e.Want
}

// errUnsupportedAPIVersion reports a report whose api_version is not
// SupportedAPIVersion. The returned *llm.AttestationError has reason
// reasonUnsupportedAPIVersion; its message names both the offending version got
// and the supported version, and carries no secrets.
func errUnsupportedAPIVersion(got string) *llm.AttestationError {
	return attestErr(reasonUnsupportedAPIVersion, &apiVersionMismatchError{
		Got:  got,
		Want: SupportedAPIVersion,
	})
}
