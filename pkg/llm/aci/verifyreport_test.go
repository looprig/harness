package aci

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/tee"
)

// This file tests Task 2.8's assembled chain: verifyReport runs attestation
// steps 1–9 in order, returning the FIRST failure's typed *llm.AttestationError,
// or a populated *VerifiedReport on success. The quote verification (step 4) is
// delegated to the injected quoteVerifier seam so the full chain runs OFFLINE
// against the fixture (the live DCAP verifier needs Intel collateral the fixture
// cannot carry); the public VerifyReport wires the live seam and is exercised
// only for its wiring, never invoked here.
//
// The fixture (testdata/report_aci1.json) is the arbiter. Its freshness window
// is [1782664738, 1782668338) (a 1-hour half-open window); the offline full
// chain uses now = 1782664738 (the inclusive lower bound) with the capture nonce
// and testPolicy.

// fixtureNow is a time inside the fixture's freshness window (== fetched_at, the
// inclusive lower bound of the half-open [fetched_at, stale_after) window).
const (
	fixtureFetchedAt  int64 = 1782664738
	fixtureStaleAfter int64 = 1782668338
)

// testPolicy builds an attestation acceptance Policy whose anchors MATCH THE
// FIXTURE (testdata/report_aci1.json): the fixture app-id, its {repo_url,
// repo_commit} source provenance, and its recovered KMS root, with an empty
// AcceptedWorkloadIDs (step 9 skipped by default). These generic chain tests need
// *a* valid policy for the fixture, not the shipped provider preset — the pinned
// production Phala anchors now live in pkg/llm/providers/phala (phala.DefaultPolicy).
// It draws the values from the same fixture-derived constants the per-step tests
// use (fixtureAppIDHex, fixtureRepoURL/fixtureRepoCommit, fixtureKMSRoot), so a
// drift in the fixture is still caught, and it returns a fresh value each call so
// a test may narrow it without corrupting another.
func testPolicy() Policy {
	return Policy{
		AcceptedWorkloadIDs: map[string]struct{}{},
		AcceptedSourceProvenance: map[ProvenanceKey]struct{}{
			{RepoURL: fixtureRepoURL, RepoCommit: fixtureRepoCommit}: {},
		},
		AcceptedAppIDs: map[string]struct{}{
			fixtureAppIDHex: {},
		},
		AcceptedKMSRootPubKeys: map[string]struct{}{
			fixtureKMSRoot: {},
		},
	}
}

// fakeQuoteVerifier returns the fixture's real 64-byte quote_report_data as the
// "verified" report_data, standing in for the live DCAP verifier offline.
func fakeQuoteVerifier(rep *Report) quoteVerifier {
	rd, err := hex.DecodeString(rep.Attestation.Evidence.QuoteReportData)
	return func(raw []byte, opts tee.Options) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		return rd, nil
	}
}

// requireReason asserts err is an *llm.AttestationError carrying wantReason.
func requireReason(t *testing.T, err error, wantReason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("verifyReport() = nil error, want reason %q", wantReason)
	}
	var ae *llm.AttestationError
	if !errors.As(err, &ae) {
		t.Fatalf("verifyReport() error = %T (%v), want *llm.AttestationError", err, err)
	}
	if ae.Reason != wantReason {
		t.Errorf("AttestationError.Reason = %q, want %q", ae.Reason, wantReason)
	}
}

// remarshalReport mutates a parsed report, re-encodes it to JSON, and returns the
// bytes so verifyReport re-parses from a faithful document (step 1 runs ParseReport
// on the bytes, so per-step mutations must round-trip through JSON).
func remarshalReport(t *testing.T, rep *Report) []byte {
	t.Helper()
	out, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("re-marshal report: %v", err)
	}
	return out
}

// TestVerifyReportOfflineFullChain is THE arbiter for Task 2.8: the whole chain
// (steps 1–9) passes offline against the fixture with the capture nonce, a now
// inside the freshness window, testPolicy, and the fake quote seam. The
// returned *VerifiedReport must carry the fixture's workload_id, keyset digest,
// and the validated e2ee + receipt-signing keys (Phase 3/4 consumers).
func TestVerifyReportOfflineFullChain(t *testing.T) {
	t.Parallel()
	raw := readFixture(t)
	rep := fixtureReport(t)

	vr, err := verifyReport(raw, strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), testPolicy(), fakeQuoteVerifier(rep))
	if err != nil {
		t.Fatalf("verifyReport(full chain) = %v, want nil", err)
	}
	if vr == nil {
		t.Fatalf("verifyReport() = nil *VerifiedReport, want populated")
	}
	if vr.WorkloadID != claimedWorkloadID {
		t.Errorf("VerifiedReport.WorkloadID = %q, want %q", vr.WorkloadID, claimedWorkloadID)
	}
	if vr.WorkloadKeysetDigest != claimedKeysetDigest {
		t.Errorf("VerifiedReport.WorkloadKeysetDigest = %q, want %q", vr.WorkloadKeysetDigest, claimedKeysetDigest)
	}
	if len(vr.Keyset.E2EEPublicKeys) != len(rep.Attestation.Keyset.E2EEPublicKeys) {
		t.Errorf("VerifiedReport.Keyset.E2EEPublicKeys = %d entries, want %d", len(vr.Keyset.E2EEPublicKeys), len(rep.Attestation.Keyset.E2EEPublicKeys))
	}
	if len(vr.Keyset.ReceiptSigningKeys) != len(rep.Attestation.Keyset.ReceiptSigningKeys) {
		t.Errorf("VerifiedReport.Keyset.ReceiptSigningKeys = %d entries, want %d", len(vr.Keyset.ReceiptSigningKeys), len(rep.Attestation.Keyset.ReceiptSigningKeys))
	}
	// Spot-check Phase 3/4 ids survive intact.
	if len(vr.Keyset.E2EEPublicKeys) > 0 && vr.Keyset.E2EEPublicKeys[0].KeyID != rep.Attestation.Keyset.E2EEPublicKeys[0].KeyID {
		t.Errorf("e2ee key_id = %q, want %q", vr.Keyset.E2EEPublicKeys[0].KeyID, rep.Attestation.Keyset.E2EEPublicKeys[0].KeyID)
	}
	if len(vr.Keyset.ReceiptSigningKeys) > 0 && vr.Keyset.ReceiptSigningKeys[0].KeyID != rep.Attestation.Keyset.ReceiptSigningKeys[0].KeyID {
		t.Errorf("receipt key_id = %q, want %q", vr.Keyset.ReceiptSigningKeys[0].KeyID, rep.Attestation.Keyset.ReceiptSigningKeys[0].KeyID)
	}
}

// TestVerifyReportPerStepFailures mutates the fixture (or the nonce/now/policy) so
// EXACTLY ONE step fails, and asserts the FIRST failure's reason surfaces. The fake
// seam returns the fixture's real quote_report_data except where a row overrides it
// to exercise the quote_invalid placement path.
func TestVerifyReportPerStepFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// build returns (reportJSON, nonce, now, policy, verifyFn). Each row mutates
		// exactly one input so a single step fails.
		build      func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier)
		wantReason string
	}{
		{
			name: "step1 bad api_version -> unsupported_api_version",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				vf := fakeQuoteVerifier(rep)
				rep.APIVersion = "aci/2"
				return remarshalReport(t, rep), strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), testPolicy(), vf
			},
			wantReason: reasonUnsupportedAPIVersion,
		},
		{
			name: "step2 mutated keyset -> keyset_digest_mismatch",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				vf := fakeQuoteVerifier(rep)
				// flip the claimed keyset digest so recomputed != claimed.
				rep.WorkloadKeysetDigest = "sha256:" + "00000000000000000000000000000000000000000000000000000000000000ff"
				return remarshalReport(t, rep), strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), testPolicy(), vf
			},
			wantReason: reasonKeysetDigestMismatch,
		},
		{
			name: "step3 wrong nonce -> report_data_mismatch",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				vf := fakeQuoteVerifier(rep)
				return remarshalReport(t, rep), strPtr(wrongNonce), time.Unix(fixtureFetchedAt, 0), testPolicy(), vf
			},
			wantReason: reasonReportDataMismatch,
		},
		{
			name: "step4 tampered report_data -> quote_invalid",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				rd := decodeQuoteReportData(t, rep)
				tampered := append([]byte(nil), rd...)
				tampered[0] ^= 0xFF
				vf := func(raw []byte, opts tee.Options) ([]byte, error) { return tampered, nil }
				return remarshalReport(t, rep), strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), testPolicy(), vf
			},
			wantReason: reasonQuoteInvalid,
		},
		{
			name: "step5 tampered event_log -> quote_invalid",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				vf := fakeQuoteVerifier(rep)
				// drop the first imr==3 event so the IMR3 replay diverges from the
				// quote's attested RTMR3 (integrity failure -> quote_invalid).
				rep.Attestation.Evidence.EventLog = mutateEventLog(t, rep, func(events []EventLogEntry) []EventLogEntry {
					out := make([]EventLogEntry, 0, len(events))
					dropped := false
					for _, e := range events {
						if !dropped && e.IMR == imr3 {
							dropped = true
							continue
						}
						out = append(out, e)
					}
					return out
				})
				return remarshalReport(t, rep), strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), testPolicy(), vf
			},
			wantReason: reasonQuoteInvalid,
		},
		{
			name: "step5 app-id not in policy -> policy_rejected",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				vf := fakeQuoteVerifier(rep)
				p := testPolicy()
				// keep provenance/KMS valid; replace the app-id allow-list with a
				// non-matching one so only the app-id check rejects.
				p.AcceptedAppIDs = map[string]struct{}{"deadbeef": {}}
				return remarshalReport(t, rep), strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), p, vf
			},
			wantReason: reasonPolicyRejected,
		},
		{
			name: "step5 provenance not in policy -> policy_rejected",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				vf := fakeQuoteVerifier(rep)
				p := testPolicy()
				p.AcceptedSourceProvenance = map[ProvenanceKey]struct{}{
					{RepoURL: "https://example.com/evil.git", RepoCommit: "0"}: {},
				}
				return remarshalReport(t, rep), strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), p, vf
			},
			wantReason: reasonPolicyRejected,
		},
		{
			name: "step6 tampered endorsement -> endorsement_invalid",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				vf := fakeQuoteVerifier(rep)
				// flip a byte in the endorsement signature so verification fails.
				sig := rep.Attestation.KeysetEndorsement.ValueHex
				rep.Attestation.KeysetEndorsement.ValueHex = "00" + sig[2:]
				return remarshalReport(t, rep), strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), testPolicy(), vf
			},
			wantReason: reasonEndorsementInvalid,
		},
		{
			name: "step7 KMS root not in policy -> kms_root_untrusted",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				vf := fakeQuoteVerifier(rep)
				p := testPolicy()
				p.AcceptedKMSRootPubKeys = map[string]struct{}{"02ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff": {}}
				return remarshalReport(t, rep), strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), p, vf
			},
			wantReason: reasonKMSRootUntrusted,
		},
		{
			name: "step8 before fetched_at -> stale_report",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				vf := fakeQuoteVerifier(rep)
				return remarshalReport(t, rep), strPtr(captureNonce), time.Unix(fixtureFetchedAt-1, 0), testPolicy(), vf
			},
			wantReason: reasonStaleReport,
		},
		{
			name: "step8 at stale_after -> stale_report",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				vf := fakeQuoteVerifier(rep)
				return remarshalReport(t, rep), strPtr(captureNonce), time.Unix(fixtureStaleAfter, 0), testPolicy(), vf
			},
			wantReason: reasonStaleReport,
		},
		{
			name: "step9 workload_id not in policy -> policy_rejected",
			build: func(t *testing.T) ([]byte, *string, time.Time, Policy, quoteVerifier) {
				rep := fixtureReport(t)
				vf := fakeQuoteVerifier(rep)
				p := testPolicy()
				p.AcceptedWorkloadIDs = map[string]struct{}{"sha256:wrong": {}}
				return remarshalReport(t, rep), strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), p, vf
			},
			wantReason: reasonPolicyRejected,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, nonce, now, policy, vf := tt.build(t)
			_, err := verifyReport(raw, nonce, now, policy, vf)
			requireReason(t, err, tt.wantReason)
		})
	}
}

// TestVerifyReportFreshnessBoundary pins the half-open window [fetched_at,
// stale_after): now == fetched_at is valid (inclusive lower), now ==
// stale_after-1 (last valid second) is valid, now == stale_after is stale
// (exclusive upper), now < fetched_at is stale.
func TestVerifyReportFreshnessBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		now     int64
		wantErr bool
	}{
		{name: "before fetched_at is stale", now: fixtureFetchedAt - 1, wantErr: true},
		{name: "exactly fetched_at is valid (inclusive lower)", now: fixtureFetchedAt, wantErr: false},
		{name: "last valid second is valid", now: fixtureStaleAfter - 1, wantErr: false},
		{name: "exactly stale_after is stale (exclusive upper)", now: fixtureStaleAfter, wantErr: true},
		{name: "after stale_after is stale", now: fixtureStaleAfter + 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := readFixture(t)
			rep := fixtureReport(t)
			_, err := verifyReport(raw, strPtr(captureNonce), time.Unix(tt.now, 0), testPolicy(), fakeQuoteVerifier(rep))
			if tt.wantErr {
				requireReason(t, err, reasonStaleReport)
				return
			}
			if err != nil {
				t.Errorf("verifyReport(now=%d) = %v, want nil", tt.now, err)
			}
		})
	}
}

// TestVerifyReportWorkloadIDPolicy pins step 9's "when configured" semantics: an
// empty AcceptedWorkloadIDs skips the check (passes); a set containing the fixture
// workload_id passes; a set without it rejects with policy_rejected.
func TestVerifyReportWorkloadIDPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		workloadIDs map[string]struct{}
		wantErr     bool
	}{
		{name: "empty set skips check", workloadIDs: map[string]struct{}{}, wantErr: false},
		{name: "nil set skips check", workloadIDs: nil, wantErr: false},
		{name: "fixture workload_id accepted", workloadIDs: map[string]struct{}{claimedWorkloadID: {}}, wantErr: false},
		{name: "wrong workload_id rejected", workloadIDs: map[string]struct{}{"sha256:wrong": {}}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := readFixture(t)
			rep := fixtureReport(t)
			p := testPolicy()
			p.AcceptedWorkloadIDs = tt.workloadIDs
			_, err := verifyReport(raw, strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), p, fakeQuoteVerifier(rep))
			if tt.wantErr {
				requireReason(t, err, reasonPolicyRejected)
				return
			}
			if err != nil {
				t.Errorf("verifyReport(workload_id policy) = %v, want nil", err)
			}
		})
	}
}

// TestVerifyReportStepOrder proves the EARLIEST failing step wins: when both the
// api_version (step 1) and freshness (step 8) would fail, the api_version reason
// surfaces (step 1 short-circuits before step 8).
func TestVerifyReportStepOrder(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	vf := fakeQuoteVerifier(rep)
	rep.APIVersion = "aci/2" // step 1 fails
	raw := remarshalReport(t, rep)
	// now is also out of the freshness window (step 8 would fail), but step 1 wins.
	_, err := verifyReport(raw, strPtr(captureNonce), time.Unix(fixtureFetchedAt-1, 0), testPolicy(), vf)
	requireReason(t, err, reasonUnsupportedAPIVersion)
}

// TestVerifyReportMalformedJSON: a non-JSON body fails at step 1 (ParseReport's
// typed parse error), surfacing as a *reportParseError, not an AttestationError.
func TestVerifyReportMalformedJSON(t *testing.T) {
	t.Parallel()
	rep := fixtureReport(t)
	_, err := verifyReport([]byte("{not json"), strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), testPolicy(), fakeQuoteVerifier(rep))
	if err == nil {
		t.Fatalf("verifyReport(malformed) = nil, want parse error")
	}
	var parseErr *reportParseError
	if !errors.As(err, &parseErr) {
		t.Errorf("verifyReport(malformed) error = %T (%v), want *reportParseError", err, err)
	}
}

// TestVerifyReportPublicWiring confirms the exported VerifyReport exists with the
// documented signature and delegates to verifyReport with the live seam. It is NOT
// invoked end-to-end (the live DCAP verifier needs network collateral); we only
// assert the malformed-body short-circuit (step 1, before any quote work) so the
// public entry point's wiring is exercised without a network call.
func TestVerifyReportPublicWiring(t *testing.T) {
	t.Parallel()
	_, err := VerifyReport([]byte("{not json"), strPtr(captureNonce), time.Unix(fixtureFetchedAt, 0), testPolicy())
	if err == nil {
		t.Fatalf("VerifyReport(malformed) = nil, want parse error")
	}
	var parseErr *reportParseError
	if !errors.As(err, &parseErr) {
		t.Errorf("VerifyReport(malformed) error = %T (%v), want *reportParseError", err, err)
	}
}
