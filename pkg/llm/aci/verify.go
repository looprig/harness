package aci

import (
	"bytes"
	"encoding/hex"
	"strconv"
	"time"

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

// =============================================================================
// Task 2.8: VerifyReport — the assembled attestation chain (steps 1–9)
// =============================================================================
//
// verifyReport runs the full Dstack ACI ("aci/1") report verification — the
// ordered chain from the design's §Attestation steps 1–9 — and returns the FIRST
// failing step's typed *llm.AttestationError, or a *VerifiedReport on success.
// The order is load-bearing: a report that fails an early structural check is
// rejected with that step's reason BEFORE any later (potentially more expensive or
// more permissive) check runs, so the earliest failure always wins.
//
// The chain wires the prior Phase-2 functions in this exact order:
//
//	1. api_version          ParseReport            -> unsupported_api_version / parse error
//	2. identity digests     verifyIdentityDigests  -> keyset_digest_mismatch
//	3. report_data binding  verifyReportDataBinding-> report_data_mismatch
//	4. TDX quote            verifyQuote (seam)     -> quote_invalid
//	5. RTMR3 + app-id +     verifyEventLogAndAppID -> quote_invalid
//	   provenance           checkAppIDPolicy       -> policy_rejected
//	                        checkProvenancePolicy  -> policy_rejected
//	6. keyset endorsement   verifyKeysetEndorsement-> endorsement_invalid
//	7. KMS custody          verifyKMSCustody       -> kms_root_untrusted
//	8. freshness            (this file)            -> stale_report
//	9. workload_id policy   (this file)            -> policy_rejected
//
// The quoteVerifier is an INJECTED SEAM (step 4): offline tests pass a fake that
// returns the fixture's real quote_report_data so the whole chain runs without
// network I/O; the public VerifyReport passes defaultQuoteVerifier (the live DCAP
// verifier), which fails offline because it needs Intel collateral.

// VerifiedReport is the validated output of a passing attestation chain: the
// workload identity (workload_id + keyset digest) and the FULL validated keyset.
// Phase 3 (E2EE sealing) reads Keyset.E2EEPublicKeys; Phase 4 (receipt
// verification) reads Keyset.ReceiptSigningKeys; both get their key ids, algos,
// and public keys from the embedded Keyset, which is the exact value whose digest
// was recomputed and matched in step 2 (so the keys are the attested ones, not
// merely claimed). Returned only on a fully successful chain; nil on any failure.
type VerifiedReport struct {
	// WorkloadID is the validated workload_id ("sha256:<hex>"), recomputed and
	// matched in step 2.
	WorkloadID string
	// WorkloadKeysetDigest is the validated workload_keyset_digest
	// ("sha256:<hex>"), recomputed and matched in step 2.
	WorkloadKeysetDigest string
	// Keyset is the full validated keyset: identity, epoch, and the
	// receipt-signing / E2EE / TLS key lists Phase 3/4/5 consume. It is the value
	// whose digest step 2 verified, so its keys are attestation-backed.
	Keyset Keyset
}

// verifyReport is the chain workhorse: it runs attestation steps 1–9 in order
// against reportJSON, using nonce for the report_data binding (step 3/4), now for
// freshness (step 8), policy for the allow-list checks (steps 5/7/9), and verifyFn
// as the injected quote-verifier seam (step 4). It returns the FIRST failing
// step's typed *llm.AttestationError (or ParseReport's typed parse error for
// malformed JSON), so the earliest failure wins; on full success it returns the
// populated *VerifiedReport. It is unexported so offline tests can inject a fake
// seam; the public VerifyReport wires the live verifier.
func verifyReport(reportJSON []byte, nonce *string, now time.Time, policy Policy, verifyFn quoteVerifier) (*VerifiedReport, error) {
	// Step 1: api_version (and JSON well-formedness).
	rep, err := ParseReport(reportJSON)
	if err != nil {
		return nil, err
	}

	// Step 2: recomputed identity / keyset digests match the claimed values.
	if err := verifyIdentityDigests(rep); err != nil {
		return nil, err
	}

	// Step 3: report_data binds the recomputed statement under this nonce.
	if err := verifyReportDataBinding(rep, nonce); err != nil {
		return nil, err
	}

	// Step 4: the TDX quote verifies and binds binding(32) ‖ zeros(32).
	if err := verifyQuote(rep, nonce, verifyFn); err != nil {
		return nil, err
	}

	// Step 5: event-log replay matches the quote's RTMR3, yielding the app-id;
	// then the app-id and source provenance must pass policy.
	appID, err := verifyEventLogAndAppID(rep)
	if err != nil {
		return nil, err
	}
	if err := checkAppIDPolicy(appID, policy.AcceptedAppIDs); err != nil {
		return nil, err
	}
	if err := checkProvenancePolicy(rep.Attestation.SourceProvenance, policy.AcceptedSourceProvenance); err != nil {
		return nil, err
	}

	// Step 6: the keyset endorsement is a valid signature by the identity key.
	if err := verifyKeysetEndorsement(rep); err != nil {
		return nil, err
	}

	// Step 7: the identity key's custody chain recovers a trusted KMS root.
	if err := verifyKMSCustody(rep, appID, policy.AcceptedKMSRootPubKeys); err != nil {
		return nil, err
	}

	// Step 8: the report is within its freshness window.
	if err := verifyFreshness(rep.Attestation.Freshness, now); err != nil {
		return nil, err
	}

	// Step 9: workload_id is allow-listed WHEN CONFIGURED (empty set skips).
	if err := checkWorkloadIDPolicy(rep.WorkloadID, policy.AcceptedWorkloadIDs); err != nil {
		return nil, err
	}

	return &VerifiedReport{
		WorkloadID:           rep.WorkloadID,
		WorkloadKeysetDigest: rep.WorkloadKeysetDigest,
		Keyset:               rep.Attestation.Keyset,
	}, nil
}

// VerifyReport runs the full Dstack ACI attestation chain (steps 1–9) against
// reportJSON with the LIVE DCAP quote verifier and returns the validated
// *VerifiedReport, or the first failing step's typed *llm.AttestationError. nonce
// is the report_data binding nonce (nil if none was sent), now is the wall clock
// for the freshness check, and policy is the acceptance allow-list (a zero
// Policy{} accepts any genuine report; a provider package supplies a pinned
// preset, e.g. phala.DefaultPolicy()). It delegates to verifyReport with
// defaultQuoteVerifier, so step 4 fetches Intel collateral over the bounded
// HTTPS-only getter — meaning this entry point requires network access and cannot
// run fully offline (offline tests use the unexported verifyReport with a fake
// seam).
func VerifyReport(reportJSON []byte, nonce *string, now time.Time, policy Policy) (*VerifiedReport, error) {
	return verifyReport(reportJSON, nonce, now, policy, defaultQuoteVerifier)
}

// verifyFreshness is attestation step 8: the report is fresh iff its freshness
// window contains now, as the HALF-OPEN interval [fetched_at, stale_after) in Unix
// seconds — fetched_at inclusive, stale_after EXCLUSIVE. So now == fetched_at is
// fresh, now == stale_after is stale, and the last fresh second is stale_after-1.
// It fails closed with reason stale_report (typed *freshnessError cause) on any
// now outside the window; the values are wall-clock seconds, never secrets.
func verifyFreshness(f Freshness, now time.Time) error {
	nowUnix := now.Unix()
	if nowUnix < f.FetchedAt || nowUnix >= f.StaleAfter {
		return attestErr(reasonStaleReport, &freshnessError{
			now:        nowUnix,
			fetchedAt:  f.FetchedAt,
			staleAfter: f.StaleAfter,
		})
	}
	return nil
}

// checkWorkloadIDPolicy is attestation step 9: it enforces the workload_id
// allow-list WHEN CONFIGURED — if accepted is non-empty, the report's workload_id
// must be a member, else policy_rejected. An empty or nil accepted set skips the
// check (returns nil), because the workload_id rotates with the keyset epoch and
// trust is anchored by the stable app-id + provenance + KMS-root checks. The key
// is the workload_id string ("sha256:<hex>"); Policy.AcceptedWorkloadIDs uses that
// same form.
func checkWorkloadIDPolicy(workloadID string, accepted map[string]struct{}) error {
	if len(accepted) == 0 {
		return nil
	}
	if _, ok := accepted[workloadID]; !ok {
		return attestErr(reasonPolicyRejected, &workloadIDRejectedError{workloadID: workloadID})
	}
	return nil
}

// freshnessError is the typed cause wrapped inside the stale_report
// *llm.AttestationError. It carries now, fetched_at, and stale_after (all Unix
// seconds) so the cause keeps type identity (per CLAUDE.md: no bare fmt.Errorf
// from package APIs) and callers can errors.As to inspect the window. The values
// are wall-clock timestamps, never key material or secrets.
type freshnessError struct {
	now        int64
	fetchedAt  int64
	staleAfter int64
}

func (e *freshnessError) Error() string {
	return "aci/verify: report not fresh: now " + strconv.FormatInt(e.now, 10) +
		" outside [" + strconv.FormatInt(e.fetchedAt, 10) +
		", " + strconv.FormatInt(e.staleAfter, 10) + ")"
}

// workloadIDRejectedError is the typed cause wrapped inside the policy_rejected
// *llm.AttestationError when a configured AcceptedWorkloadIDs set does not contain
// the report's workload_id. workloadID is the public "sha256:<hex>" digest string,
// not key material, so logging it leaks no secret.
type workloadIDRejectedError struct {
	workloadID string
}

func (e *workloadIDRejectedError) Error() string {
	return "aci/verify: workload_id " + e.workloadID + " not in accepted set"
}
