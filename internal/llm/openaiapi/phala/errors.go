// Package phala — TEE-attested OpenAI-compatible client for RedPill / Phala
// Cloud. See WIRE.md for the pinned wire format.
package phala

import "github.com/inventivepotter/urvi/internal/llm"

// AttestReason names the phala-specific attestation failure. The shared
// TEE-quote and NRAS reasons live in llm/tee and propagate as *tee.Error;
// the reasons below cover the bindings and receipt checks that ARE
// phala-specific.
type AttestReason string

const (
	// ReasonBindingMismatch: report_data did not match the expected
	// (signing_address || nonce) layout. See WIRE.md section 4.
	ReasonBindingMismatch AttestReason = "binding_mismatch"

	// ReasonSignerMismatch: the receipt's signing_address did not match
	// the address bound into the attested report_data. See WIRE.md section 5.
	ReasonSignerMismatch AttestReason = "signer_mismatch"

	// ReasonReceiptInvalid: the per-response receipt signature did not
	// verify, or the receipt text did not match the expected
	// model:request_hash:response_hash form.
	ReasonReceiptInvalid AttestReason = "receipt_invalid"

	// ReasonUnsupportedSigAlgo: receipt arrived with a signing_algo the
	// client cannot verify. We request signing_algo=ed25519 to stay on
	// stdlib; the only accepted value is "ed25519". This is a hard fail
	// (not a silent skip) so callers can distinguish "did not verify"
	// from "could not verify".
	ReasonUnsupportedSigAlgo AttestReason = "unsupported_sig_algo"

	// ReasonNoInstance: a two-layer attestation response carried no
	// model_attestations entry whose model_name matched the requested
	// model.
	ReasonNoInstance AttestReason = "no_instance"

	// ReasonAttestationMalformed: the /v1/attestation/report body did
	// not match any known response shape (flat, two-layer, or chutes-
	// passthrough — the last is silently rejected since chutes models
	// route through llm/chutes, not llm/phala).
	ReasonAttestationMalformed AttestReason = "attestation_malformed"
)

// attestErr creates an *llm.AttestationError with a phala-specific reason.
func attestErr(reason AttestReason, err error) error {
	return &llm.AttestationError{Reason: string(reason), Err: err}
}
