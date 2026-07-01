package aci

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
)

// The arbiter values for attestation step 3 (report_data binding), independently
// verified against the live fixture (testdata/report_aci1.json) and the Rust
// reference's AttestationStatement::to_canonical_value projection. The statement
// is {purpose, workload_id, workload_keyset_digest, nonce}; report_data is the
// raw lowercase hex of sha256(JCS(statement)) — note: NO "sha256:" prefix, unlike
// the workload_id / keyset_digest digests.
const (
	// captureNonce is the 64-char hex query-param value the fixture was captured
	// with, used LITERALLY as a UTF-8 string (not decoded to bytes).
	captureNonce = "0000000000000000000000000000000000000000000000000000000000000001"
	// reportDataWithNonce is sha256(JCS(statement{...,nonce:captureNonce})), the
	// value the fixture's attestation.report_data carries.
	reportDataWithNonce = "659810686f7ef071d736034d8131f13258cad714dcf812df6a9894a82ff98b63"
	// reportDataNullNonce is sha256(JCS(statement{...,nonce:null})), the null
	// branch — distinct from reportDataWithNonce, proving the nonce binds.
	reportDataNullNonce = "91694ee7d0ab0e3d5c6666cf9a547dd48506088da4000d01ed7535d633e167a7"
	// wrongNonce is any nonce other than the captured one; it must NOT reproduce
	// the fixture's report_data.
	wrongNonce = "0000000000000000000000000000000000000000000000000000000000000002"
)

// TestReportDataBindingMatchesFixture proves the statement projection is
// byte-exact: with the capture nonce, the recomputed report_data binding
// hex-equals the live fixture's attestation.report_data (and the verified
// constant), and the output is exactly 32 bytes (the value Task 2.4 consumes as
// the low 32 bytes of the quote's 64-byte report_data).
func TestReportDataBindingMatchesFixture(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	nonce := strPtr(captureNonce)

	binding, err := reportDataBinding(rep, nonce)
	if err != nil {
		t.Fatalf("reportDataBinding() error = %v, want nil", err)
	}
	// [32]byte is a fixed-size array; len is a compile-time 32. Assert the
	// hex-encoding equals the fixture's claimed value and the verified constant.
	got := hex.EncodeToString(binding[:])
	if got != rep.Attestation.ReportDataHex {
		t.Errorf("reportDataBinding() = %q, want fixture report_data %q", got, rep.Attestation.ReportDataHex)
	}
	if got != reportDataWithNonce {
		t.Errorf("reportDataBinding() = %q, want constant %q", got, reportDataWithNonce)
	}
	if len(binding) != 32 {
		t.Errorf("reportDataBinding() length = %d, want 32", len(binding))
	}
}

// TestReportDataBindingNullNonce exercises the JSON null branch: a nil nonce
// projects "nonce":null (not the string "null", not an omitted key), yielding the
// distinct null-branch digest. This binding does NOT match the fixture (captured
// with a nonce), so it is also the wrong-nonce sentinel for verification.
func TestReportDataBindingNullNonce(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)

	binding, err := reportDataBinding(rep, nil)
	if err != nil {
		t.Fatalf("reportDataBinding(nil) error = %v, want nil", err)
	}
	got := hex.EncodeToString(binding[:])
	if got != reportDataNullNonce {
		t.Errorf("reportDataBinding(nil) = %q, want null-branch constant %q", got, reportDataNullNonce)
	}
	if got == reportDataWithNonce {
		t.Errorf("null-nonce binding must differ from with-nonce binding, both = %q", got)
	}
}

// TestReportDataBindingDistinctNonces confirms the nonce participates in the
// binding: a different nonce string yields a different binding.
func TestReportDataBindingDistinctNonces(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)

	a, err := reportDataBinding(rep, strPtr(captureNonce))
	if err != nil {
		t.Fatalf("reportDataBinding(capture) error = %v, want nil", err)
	}
	b, err := reportDataBinding(rep, strPtr(wrongNonce))
	if err != nil {
		t.Fatalf("reportDataBinding(wrong) error = %v, want nil", err)
	}
	if a == b {
		t.Errorf("distinct nonces must yield distinct bindings, both = %x", a)
	}
}

// TestVerifyReportDataBinding exercises the chain helper Task 2.8 will call: nil
// on the matching capture nonce, *llm.AttestationError{Reason: report_data_mismatch}
// for a wrong nonce or the null branch (the fixture was captured with a nonce).
func TestVerifyReportDataBinding(t *testing.T) {
	t.Parallel()

	t.Run("capture nonce verifies", func(t *testing.T) {
		t.Parallel()
		if err := verifyReportDataBinding(fixtureReport(t), strPtr(captureNonce)); err != nil {
			t.Errorf("verifyReportDataBinding(capture) = %v, want nil", err)
		}
	})

	tests := []struct {
		name  string
		nonce *string
	}{
		{name: "wrong nonce", nonce: strPtr(wrongNonce)},
		{name: "nil nonce (null branch, fixture used a nonce)", nonce: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := verifyReportDataBinding(fixtureReport(t), tt.nonce)
			if err == nil {
				t.Fatalf("verifyReportDataBinding(%s) = nil, want error", tt.name)
			}
			var attErr *llm.AttestationError
			if !errors.As(err, &attErr) {
				t.Fatalf("verifyReportDataBinding() error = %T (%v), want *llm.AttestationError", err, err)
			}
			if attErr.Reason != reasonReportDataMismatch {
				t.Errorf("AttestationError.Reason = %q, want %q", attErr.Reason, reasonReportDataMismatch)
			}
		})
	}
}

// TestReportDataBindingTamperedReport confirms the binding tracks the recomputed
// identity digests: tampering the keyset (which changes the recomputed
// workload_keyset_digest used in the statement) changes the binding, so it no
// longer matches the fixture's report_data even with the correct nonce.
func TestReportDataBindingTamperedReport(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	rep.Attestation.Keyset.Epoch.Version = 2

	binding, err := reportDataBinding(rep, strPtr(captureNonce))
	if err != nil {
		t.Fatalf("reportDataBinding(tampered) error = %v, want nil", err)
	}
	if got := hex.EncodeToString(binding[:]); got == reportDataWithNonce {
		t.Errorf("tampered keyset must change the binding, got fixture value %q", got)
	}
}
