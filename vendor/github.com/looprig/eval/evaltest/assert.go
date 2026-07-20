package evaltest

import "github.com/looprig/eval"

// This file holds the report-level assertions. Each takes the TB subset, calls
// Helper, and signals failures through Errorf (never a panic or Fatalf), so the
// caller keeps control and can make further custom assertions on the same report.
// Failure lines are concise and safe: they name the scenario and render the
// offending assessment through renderAssessment, which never echoes untrusted
// content.

// RequirePass asserts that every evaluation in report reached a passing verdict
// and that the report actually verified something. It fails (via Errorf) when:
//
//   - the report has no samples (nothing was verified — fail secure: an empty
//     report is not a pass);
//   - any sample failed at the target stage (its evaluators never ran);
//   - any sample succeeded at the target stage but carries zero assessments (no
//     evaluator ran, so nothing about that sample was verified);
//   - any assessment has a status other than pass or skipped; or
//   - the report contains no passing assessment at all (an all-skipped or
//     otherwise pass-free report verified nothing and is not a pass).
//
// StatusSkipped is permitted on an individual assessment: a deliberately skipped
// evaluator is an intentional non-run, not a failure. But at least one passing
// assessment must be present for the report as a whole to pass. Every other
// non-pass status — fail, unverified, error — fails the assertion.
func RequirePass(tb TB, report eval.Report) {
	tb.Helper()
	requireStatuses(tb, report, "RequirePass", passIsFailure, true)
}

// RequireVerified asserts that every evaluation in report reached a definite
// disposition — that nothing was left unverified or errored, and that every
// sample was actually covered by at least one evaluator. It fails (via Errorf)
// when:
//
//   - the report has no samples;
//   - any sample failed at the target stage;
//   - any sample succeeded at the target stage but carries zero assessments (no
//     evaluator ran, so the sample was not verified); or
//   - any assessment has status unverified or error.
//
// It ACCEPTS pass, fail, and skipped: a fail is a definite quality verdict and a
// skipped evaluator was intentionally not run (and, being an assessment, still
// counts as coverage), so neither is "unverified". Unlike RequirePass it does not
// require a passing assessment — it asserts coverage (everything ran and decided),
// not that the subject passed.
func RequireVerified(tb TB, report eval.Report) {
	tb.Helper()
	requireStatuses(tb, report, "RequireVerified", verifiedIsFailure, false)
}

// passIsFailure reports whether a status fails RequirePass: anything that is not
// a pass and not an intentional skip.
func passIsFailure(s eval.AssessmentStatus) bool {
	return s != eval.StatusPass && s != eval.StatusSkipped
}

// verifiedIsFailure reports whether a status fails RequireVerified: an unverified
// or errored assessment.
func verifiedIsFailure(s eval.AssessmentStatus) bool {
	return s == eval.StatusUnverified || s == eval.StatusError
}

// requireStatuses is the shared assertion walk. It fails an empty report, fails
// every target-stage error, fails every sample that succeeded but ran no
// evaluator (zero assessments — nothing verified), and fails every assessment
// whose status isFailure reports as failing. When requireAtLeastOnePass is set
// (RequirePass) it additionally fails a report that reached no passing assessment
// at all, so an all-skipped or otherwise pass-free report cannot read as a pass.
// It reports each failure through a single concise Errorf.
func requireStatuses(tb TB, report eval.Report, assertion string, isFailure func(eval.AssessmentStatus) bool, requireAtLeastOnePass bool) {
	tb.Helper()
	if len(report.Samples) == 0 {
		tb.Errorf("evaltest: %s: report has no samples", assertion)
		return
	}
	passSeen := false
	for i := range report.Samples {
		s := report.Samples[i]
		name := scenarioName(s)
		if s.TargetErr != nil {
			// TargetError.Error() is a fixed, safe string; the cause is not echoed.
			tb.Errorf("evaltest: %s: %s: %s", assertion, name, s.TargetErr.Error())
			continue
		}
		if len(s.Assessments) == 0 {
			// The target succeeded but no evaluator produced a verdict: nothing about
			// this sample was verified. Fail secure — a vacuous non-run is neither a
			// pass nor "verified".
			tb.Errorf("evaltest: %s: %s: no evaluators ran; nothing was verified", assertion, name)
			continue
		}
		for _, a := range s.Assessments {
			if a.Status == eval.StatusPass {
				passSeen = true
			}
			if isFailure(a.Status) {
				tb.Errorf("evaltest: %s: %s: %s", assertion, name, renderAssessment(a))
			}
		}
	}
	if requireAtLeastOnePass && !passSeen {
		tb.Errorf("evaltest: %s: report has no passing assessments; nothing was verified", assertion)
	}
}
