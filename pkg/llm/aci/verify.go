package aci

import (
	"bytes"
	"encoding/hex"

	"github.com/ciram-co/looprig/pkg/llm/tee"
)

// This file implements attestation step 4 of the Dstack ACI ("aci/1") report
// verification: TDX quote verification + report_data placement. It verifies the
// quote's signature and PCK certificate chain (delegated to the injected
// quoteVerifier seam, which on the live path is the real DCAP verifier with Intel
// collateral and revocation checks ON), then enforces that the verified 64-byte
// report_data the quote covers is laid out as binding(32) ‖ zeros(32) and that
// tee_type == "tdx". The binding is recomputed by reportDataBinding (Task 2.2)
// from the recomputed identity digests and the supplied nonce.
//
// FAIL CLOSED. Per the spec (§Attestation step 4), EVERY failure in this step —
// wrong tee_type, malformed quote hex, a verifier error, or a report_data
// placement mismatch — maps to the single fail-closed reason quote_invalid. A
// nil return means: the quote verified against the Intel root, and the value it
// binds is exactly the binding we independently recomputed (followed by 32 zero
// bytes), so the quote vouches for OUR statement and identity.
//
// The verifier is an INJECTED SEAM so offline tests use the fixture's real quote
// bytes without network I/O (the real DCAP verifier needs live Intel collateral
// the fixture cannot carry, and fails offline on it). The live path passes the
// real tee.VerifyTDXQuoteWithOptions, exposed below as defaultQuoteVerifier for
// Task 2.8 to wire into the chain.

// teeTypeTDX is the only tee_type this quote step accepts. The fixture carries
// "tdx"; any other value (sev, snp, empty, differently-cased) is rejected as
// quote_invalid before any quote work. It is a domain constant, not a magic
// string.
const teeTypeTDX = "tdx"

// reportDataSize is the byte length of the verified TDX report_data the quote
// covers: a 32-byte binding followed by 32 zero bytes.
const reportDataSize = 64

// quoteVerifier verifies raw TDX quote bytes against the Intel SGX Root CA under
// the given options and returns the verified 64-byte report_data the quote
// covers. It is exactly the signature of tee.VerifyTDXQuoteWithOptions (Task
// 2.3), so the live verifier IS that function; the named type makes the
// dependency an injectable seam so verifyQuote is testable offline with the
// fixture's real quote bytes and a fake that returns the known report_data.
type quoteVerifier func(raw []byte, opts tee.Options) ([]byte, error)

// defaultQuoteVerifier is the live DCAP verifier wired into the chain by Task
// 2.8: the real tee.VerifyTDXQuoteWithOptions. verifyQuote calls it with
// GetCollateral and CheckRevocations ON and a nil Getter, so the bounded,
// HTTPS-only, context-deadline'd default getter (newBoundedGetter) is used to
// fetch Intel PCS collateral / CRL — never the library's unbounded default.
var defaultQuoteVerifier quoteVerifier = tee.VerifyTDXQuoteWithOptions

// expectedReportData returns the 64-byte report_data the quote MUST cover for the
// given binding: the 32-byte binding followed by 32 zero bytes. It is the value
// verifyQuote compares the verifier's output against, and the value the typed
// placement error reports as "expected".
func expectedReportData(binding [32]byte) []byte {
	return append(binding[:], make([]byte, 32)...)
}

// verifyQuote is attestation step 4 of VerifyReport (the chain lands in Task 2.8).
// It requires tee_type == "tdx", recomputes the report_data binding from the
// recomputed identity digests and the supplied nonce (Task 2.2), decodes the hex
// quote, verifies it via the injected seam (live: collateral + revocation ON,
// bounded getter), and requires the verified 64-byte report_data to equal
// binding(32) ‖ zeros(32). It fails closed: every failure returns the
// provider-neutral *llm.AttestationError with reason quote_invalid, chaining a
// typed cause. It returns nil only when the quote verified AND the value it binds
// is exactly the independently recomputed binding (followed by 32 zero bytes).
func verifyQuote(rep *Report, nonce *string, verify quoteVerifier) error {
	if rep.Attestation.TEEType != teeTypeTDX {
		return attestErr(reasonQuoteInvalid, &teeTypeError{
			Got:  rep.Attestation.TEEType,
			Want: teeTypeTDX,
		})
	}

	binding, err := reportDataBinding(rep, nonce)
	if err != nil {
		return attestErr(reasonQuoteInvalid, err)
	}

	raw, err := hex.DecodeString(rep.Attestation.Evidence.Quote)
	if err != nil {
		return attestErr(reasonQuoteInvalid, &quoteDecodeError{cause: err})
	}

	rd, err := verify(raw, tee.Options{GetCollateral: true, CheckRevocations: true})
	if err != nil {
		return attestErr(reasonQuoteInvalid, err)
	}

	want := expectedReportData(binding)
	if len(rd) != reportDataSize || !bytes.Equal(rd, want) {
		return attestErr(reasonQuoteInvalid, &reportDataPlacementError{
			expected: hex.EncodeToString(want),
			actual:   hex.EncodeToString(rd),
		})
	}
	return nil
}

// teeTypeError is the typed cause wrapped inside the quote_invalid
// *llm.AttestationError when tee_type != "tdx". It carries the offending and
// expected tee_type so the cause keeps type identity (per CLAUDE.md: no bare
// fmt.Errorf from package APIs); both are short wire labels, never secrets.
type teeTypeError struct {
	Got  string
	Want string
}

func (e *teeTypeError) Error() string {
	return "aci/verify: tee_type " + e.Got + " is not supported, want " + e.Want
}

// quoteDecodeError is the typed cause wrapped inside the quote_invalid
// *llm.AttestationError when evidence.quote is not valid hex. It chains the
// stdlib hex error via Unwrap so callers can errors.As to it while keeping the
// uniform typed-cause contract (per CLAUDE.md: no bare hex error escaping the
// package API).
type quoteDecodeError struct {
	cause error
}

func (e *quoteDecodeError) Error() string {
	return "aci/verify: quote hex decode: " + e.cause.Error()
}

func (e *quoteDecodeError) Unwrap() error { return e.cause }

// reportDataPlacementError is the typed cause wrapped inside the quote_invalid
// *llm.AttestationError when the verified report_data is the wrong length or does
// not equal binding(32) ‖ zeros(32). It carries the expected and actual
// report_data as lowercase hex so the cause keeps type identity (per CLAUDE.md)
// and callers can errors.As to inspect the mismatch. Both values are 64-byte
// report_data digests (a SHA-256 binding plus zero padding), NOT key material or
// API keys, so logging them leaks no secret.
type reportDataPlacementError struct {
	// expected is binding(32) ‖ zeros(32) recomputed from the statement; actual
	// is the verifier's returned report_data (hex of whatever length it had).
	expected string
	actual   string
}

func (e *reportDataPlacementError) Error() string {
	return "aci/verify: report_data placement mismatch: expected " + e.expected + ", got " + e.actual
}

// compile-time assertion: the live verifier satisfies the seam type exactly, so
// no adapter is needed and Task 2.8 can pass defaultQuoteVerifier directly.
var _ quoteVerifier = tee.VerifyTDXQuoteWithOptions
