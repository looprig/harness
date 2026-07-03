package aci

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/tee"
)

// This file is attestation step 4's test (verifyQuote): the quote signature/chain
// verification is delegated to an injected quoteVerifier seam so the offline tests
// use the fixture's REAL quote bytes without network I/O (the real DCAP verifier
// needs live Intel collateral the fixture cannot carry). The seam returns the
// verified 64-byte report_data; verifyQuote then enforces the report_data
// PLACEMENT — binding(32) ‖ zeros(32) — and tee_type == "tdx". Every failure in
// this step maps to quote_invalid (spec §Attestation step 4). The fixture
// (testdata/report_aci1.json) is the arbiter: its evidence.quote_report_data is
// exactly the binding from Task 2.2 (nonce captureNonce) followed by 32 zero
// bytes, so a fake verifier returning that value makes verifyQuote pass and any
// tamper fails.

// decodeQuoteReportData decodes the fixture's evidence.quote_report_data (the real
// 64-byte value the quote covers) so fakes can return it as the "verified"
// report_data. It is the offline stand-in for what the live DCAP verifier would
// extract from the quote.
func decodeQuoteReportData(t *testing.T, rep *Report) []byte {
	t.Helper()
	rd, err := hex.DecodeString(rep.Attestation.Evidence.QuoteReportData)
	if err != nil {
		t.Fatalf("decode quote_report_data: %v", err)
	}
	if len(rd) != 64 {
		t.Fatalf("fixture quote_report_data = %d bytes, want 64", len(rd))
	}
	return rd
}

// assertQuoteInvalid asserts err is a fail-closed *llm.AttestationError with reason
// quote_invalid. Every verifyQuote failure row uses this.
func assertQuoteInvalid(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("verifyQuote() = nil, want error")
	}
	var attErr *llm.AttestationError
	if !errors.As(err, &attErr) {
		t.Fatalf("verifyQuote() error = %T (%v), want *llm.AttestationError", err, err)
	}
	if attErr.Reason != reasonQuoteInvalid {
		t.Errorf("AttestationError.Reason = %q, want %q", attErr.Reason, reasonQuoteInvalid)
	}
}

// TestVerifyQuoteHappyPath is the offline happy path: the injected seam returns the
// fixture's real 64-byte quote_report_data and asserts it was called with the live
// DCAP options (GetCollateral && CheckRevocations) and the decoded quote bytes.
// With the capture nonce — which reproduces the binding the fixture's quote covers
// — verifyQuote returns nil.
func TestVerifyQuoteHappyPath(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)

	wantRaw, err := hex.DecodeString(rep.Attestation.Evidence.Quote)
	if err != nil {
		t.Fatalf("decode fixture quote: %v", err)
	}
	wantRD := decodeQuoteReportData(t, rep)

	var called bool
	fake := func(raw []byte, opts tee.Options) ([]byte, error) {
		called = true
		if !opts.GetCollateral {
			t.Errorf("opts.GetCollateral = false, want true")
		}
		if !opts.CheckRevocations {
			t.Errorf("opts.CheckRevocations = false, want true")
		}
		if !bytes.Equal(raw, wantRaw) {
			t.Errorf("verifier raw quote = %d bytes, want the decoded fixture quote (%d bytes)", len(raw), len(wantRaw))
		}
		return wantRD, nil
	}

	if err := verifyQuote(rep, strPtr(captureNonce), fake); err != nil {
		t.Errorf("verifyQuote(capture) = %v, want nil", err)
	}
	if !called {
		t.Errorf("verifier was not called on the happy path")
	}
}

// TestVerifyQuotePlacement covers report_data PLACEMENT tampering: the seam returns
// a syntactically valid 64-byte rd whose layout is wrong (flipped binding byte,
// non-zero upper half) or whose length is wrong. All map to quote_invalid.
func TestVerifyQuotePlacement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// mutate transforms the fixture's real 64-byte report_data into the
		// tampered value the fake returns.
		mutate func(rd []byte) []byte
	}{
		{
			name: "flipped byte in binding half",
			mutate: func(rd []byte) []byte {
				out := append([]byte(nil), rd...)
				out[0] ^= 0xFF
				return out
			},
		},
		{
			name: "non-zero byte in upper (zeros) half",
			mutate: func(rd []byte) []byte {
				out := append([]byte(nil), rd...)
				out[32] = 0x01
				return out
			},
		},
		{
			name: "non-zero byte at last (zeros) position",
			mutate: func(rd []byte) []byte {
				out := append([]byte(nil), rd...)
				out[63] = 0x01
				return out
			},
		},
		{
			name: "wrong length (32 bytes)",
			mutate: func(rd []byte) []byte {
				return rd[:32]
			},
		},
		{
			name: "wrong length (empty)",
			mutate: func(rd []byte) []byte {
				return []byte{}
			},
		},
		{
			name: "wrong length (65 bytes)",
			mutate: func(rd []byte) []byte {
				return append(append([]byte(nil), rd...), 0x00)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rep := fixtureReport(t)
			tampered := tt.mutate(decodeQuoteReportData(t, rep))
			fake := func(raw []byte, opts tee.Options) ([]byte, error) {
				return tampered, nil
			}
			err := verifyQuote(rep, strPtr(captureNonce), fake)
			assertQuoteInvalid(t, err)
		})
	}
}

// TestVerifyQuoteWrongNonce: the seam returns the fixture's real report_data, but
// verifyQuote recomputes the binding from a DIFFERENT nonce, so the binding half no
// longer matches the quote's report_data -> quote_invalid.
func TestVerifyQuoteWrongNonce(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	rd := decodeQuoteReportData(t, rep)
	fake := func(raw []byte, opts tee.Options) ([]byte, error) {
		return rd, nil
	}
	err := verifyQuote(rep, strPtr(wrongNonce), fake)
	assertQuoteInvalid(t, err)
}

// TestVerifyQuoteNilNonce: the null-nonce branch recomputes a different binding
// (the fixture was captured with a nonce), so even the real report_data fails
// placement -> quote_invalid.
func TestVerifyQuoteNilNonce(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	rd := decodeQuoteReportData(t, rep)
	fake := func(raw []byte, opts tee.Options) ([]byte, error) {
		return rd, nil
	}
	err := verifyQuote(rep, nil, fake)
	assertQuoteInvalid(t, err)
}

// TestVerifyQuoteWrongTEEType: a non-tdx tee_type is rejected as quote_invalid and
// the verifier is NOT called (short-circuit before any quote work).
func TestVerifyQuoteWrongTEEType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		teeType string
	}{
		{name: "sev", teeType: "sev"},
		{name: "empty", teeType: ""},
		{name: "uppercase TDX", teeType: "TDX"},
		{name: "snp", teeType: "snp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rep := fixtureReport(t)
			rep.Attestation.TEEType = tt.teeType
			var called bool
			fake := func(raw []byte, opts tee.Options) ([]byte, error) {
				called = true
				return decodeQuoteReportData(t, rep), nil
			}
			err := verifyQuote(rep, strPtr(captureNonce), fake)
			assertQuoteInvalid(t, err)
			if called {
				t.Errorf("verifier must NOT be called when tee_type=%q (short-circuit)", tt.teeType)
			}
		})
	}
}

// errFakeVerify is a sentinel cause a fake verifier returns to exercise the
// verifier-error path; it stands in for a real tee.Error from the DCAP verifier.
var errFakeVerify = errors.New("fake verifier failure")

// TestVerifyQuoteVerifierError: when the seam returns (nil, err), verifyQuote fails
// closed with quote_invalid and chains the cause (errors.Is reaches it).
func TestVerifyQuoteVerifierError(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)

	cause := &tee.Error{Reason: tee.ReasonRootCAUntrusted, Err: errFakeVerify}
	fake := func(raw []byte, opts tee.Options) ([]byte, error) {
		return nil, cause
	}
	err := verifyQuote(rep, strPtr(captureNonce), fake)
	assertQuoteInvalid(t, err)

	var teeErr *tee.Error
	if !errors.As(err, &teeErr) {
		t.Fatalf("verifyQuote() error chain missing *tee.Error, got %v", err)
	}
	if !errors.Is(err, errFakeVerify) {
		t.Errorf("verifyQuote() error chain missing the verifier cause")
	}
}

// TestVerifyQuoteMalformedQuoteHex: a non-hex evidence.quote fails decoding before
// the verifier is reached -> quote_invalid, verifier NOT called.
func TestVerifyQuoteMalformedQuoteHex(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	rep.Attestation.Evidence.Quote = "zzzz-not-hex"
	var called bool
	fake := func(raw []byte, opts tee.Options) ([]byte, error) {
		called = true
		return decodeQuoteReportData(t, rep), nil
	}
	err := verifyQuote(rep, strPtr(captureNonce), fake)
	assertQuoteInvalid(t, err)
	if called {
		t.Errorf("verifier must NOT be called when evidence.quote is non-hex")
	}
}

// TestVerifyQuotePlacementErrorCause confirms the placement failure carries the
// typed *reportDataPlacementError cause (errors.As), so callers can inspect the
// expected vs got report_data hex (non-secret digests).
func TestVerifyQuotePlacementErrorCause(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	rd := decodeQuoteReportData(t, rep)
	tampered := append([]byte(nil), rd...)
	tampered[0] ^= 0xFF
	fake := func(raw []byte, opts tee.Options) ([]byte, error) {
		return tampered, nil
	}
	err := verifyQuote(rep, strPtr(captureNonce), fake)
	assertQuoteInvalid(t, err)

	var placeErr *reportDataPlacementError
	if !errors.As(err, &placeErr) {
		t.Fatalf("verifyQuote() error chain missing *reportDataPlacementError, got %v", err)
	}
	if placeErr.actual != hex.EncodeToString(tampered) {
		t.Errorf("placement error actual = %q, want %q", placeErr.actual, hex.EncodeToString(tampered))
	}
	// expected is binding(32) ‖ zeros(32) for the capture nonce.
	binding, err := reportDataBinding(rep, strPtr(captureNonce))
	if err != nil {
		t.Fatalf("reportDataBinding: %v", err)
	}
	wantExpected := hex.EncodeToString(append(binding[:], make([]byte, 32)...))
	if placeErr.expected != wantExpected {
		t.Errorf("placement error expected = %q, want %q", placeErr.expected, wantExpected)
	}
}

// TestDefaultQuoteVerifier confirms the package exposes the live DCAP verifier as a
// quoteVerifier value (Task 2.8 wires it into the chain). It is non-nil and IS
// tee.VerifyTDXQuoteWithOptions; we do NOT invoke it here (it needs network).
func TestDefaultQuoteVerifier(t *testing.T) {
	t.Parallel()
	if defaultQuoteVerifier == nil {
		t.Fatalf("defaultQuoteVerifier is nil, want the live tee verifier")
	}
}
