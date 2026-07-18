package evaltest

import "github.com/looprig/eval"

// This file holds the report-level assertions. Each takes the TB subset, calls
// Helper, and signals failures through Errorf (never a panic or Fatalf), so the
// caller keeps control and can make further custom assertions on the same report.
// Failure lines are concise and safe: they name the scenario and render the
// offending assessment through renderAssessment, which never echoes untrusted
// content.

// RequirePass asserts that every evaluation in report reached a passing verdict.
// It fails (via Errorf) when:
//
//   - the report has no samples (nothing was verified — fail secure: an empty
//     report is not a pass);
//   - any sample failed at the target stage (its evaluators never ran); or
//   - any assessment has a status other than pass or skipped.
//
// StatusSkipped is permitted: a deliberately skipped evaluator is an intentional
// non-run, not a failure. Every other non-pass status — fail, unverified, error —
// fails the assertion.
func RequirePass(tb TB, report eval.Report) {
	tb.Helper()
	requireStatuses(tb, report, "RequirePass", passIsFailure)
}

// RequireVerified asserts that every evaluation in report reached a definite
// disposition — that nothing was left unverified or errored. It fails (via
// Errorf) when:
//
//   - the report has no samples;
//   - any sample failed at the target stage; or
//   - any assessment has status unverified or error.
//
// It ACCEPTS pass, fail, and skipped: a fail is a definite quality verdict and a
// skipped evaluator was intentionally not run, so neither is "unverified". This is
// the deliberate distinction from RequirePass — RequireVerified asserts coverage
// (everything ran and decided), not that the subject passed.
func RequireVerified(tb TB, report eval.Report) {
	tb.Helper()
	requireStatuses(tb, report, "RequireVerified", verifiedIsFailure)
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
// every target-stage error, and fails every assessment whose status isFailure
// reports as failing. It reports each failure through a single concise Errorf.
func requireStatuses(tb TB, report eval.Report, assertion string, isFailure func(eval.AssessmentStatus) bool) {
	tb.Helper()
	if len(report.Samples) == 0 {
		tb.Errorf("evaltest: %s: report has no samples", assertion)
		return
	}
	for i := range report.Samples {
		s := report.Samples[i]
		name := scenarioName(s)
		if s.TargetErr != nil {
			// TargetError.Error() is a fixed, safe string; the cause is not echoed.
			tb.Errorf("evaltest: %s: %s: %s", assertion, name, s.TargetErr.Error())
			continue
		}
		for _, a := range s.Assessments {
			if isFailure(a.Status) {
				tb.Errorf("evaltest: %s: %s: %s", assertion, name, renderAssessment(a))
			}
		}
	}
}
