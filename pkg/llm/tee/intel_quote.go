package tee

import (
	"errors"

	"github.com/google/go-tdx-guest/abi"
	pb "github.com/google/go-tdx-guest/proto/tdx"
	"github.com/google/go-tdx-guest/verify"
)

// VerifyTDXQuote parses raw TDX quote bytes, verifies the ECDSA signature and
// PCK certificate chain against the embedded Intel SGX Root CA, and returns
// the 64-byte report_data field. Callers do their own report_data binding
// check (the binding is provider-specific).
//
// No network I/O: GetCollateral and CheckRevocations are both false, so TCB
// level / QE identity / CRL revocation are NOT checked. This matches the
// chutes v1 scope; if you need full collateral verification, add an Options
// parameter the day a caller needs it.
func VerifyTDXQuote(rawQuote []byte) ([]byte, error) {
	q, err := abi.QuoteToProto(rawQuote)
	if err != nil {
		return nil, &Error{Reason: ReasonEvidenceMalformed, Err: err}
	}
	qv4, ok := q.(*pb.QuoteV4)
	if !ok {
		return nil, &Error{Reason: ReasonEvidenceMalformed, Err: errors.New("unexpected quote type (not QuoteV4)")}
	}
	rd := qv4.GetTdQuoteBody().GetReportData()
	if len(rd) != 64 {
		return nil, &Error{Reason: ReasonEvidenceMalformed, Err: errors.New("report_data is not 64 bytes")}
	}
	if err := verify.TdxQuote(q, &verify.Options{GetCollateral: false, CheckRevocations: false}); err != nil {
		return nil, &Error{Reason: classifyVerifyErr(err), Err: err}
	}
	return rd, nil
}

// classifyVerifyErr maps a go-tdx-guest verification error to a Reason.
// Hash/signature failures map to ReasonQuoteSignatureInvalid; everything else
// from the chain-verification path (untrusted root, expired or invalid certs)
// maps to ReasonRootCAUntrusted.
func classifyVerifyErr(err error) Reason {
	switch {
	case errors.Is(err, verify.ErrHashVerificationFail),
		errors.Is(err, verify.ErrSHA56VerificationFail):
		return ReasonQuoteSignatureInvalid
	default:
		return ReasonRootCAUntrusted
	}
}
