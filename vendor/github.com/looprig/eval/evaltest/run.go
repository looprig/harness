// Package evaltest integrates eval reports with Go's testing package. It presents
// a report through a *testing.T as subtests and provides report-level assertions
// (RequirePass, RequireVerified) for use in ordinary Go tests. All orchestration
// stays in github.com/looprig/eval: evaltest only runs the suite through eval.Run
// and presents or asserts on the resulting report — it never re-implements the
// runner.
//
// # The TB subset
//
// testing.TB carries an unexported marker method, so no type outside the testing
// package can implement it. evaltest therefore accepts a minimal exported subset,
// TB, containing only the methods it uses. Both *testing.T and testing.TB satisfy
// TB, so callers pass a *testing.T exactly as the design intends; a test can also
// supply a fake recorder that implements TB to capture the presented output
// without failing a real test.
//
// # Subtest presentation vs. flat fallback
//
// Subtests require Run(string, func(*testing.T)) bool, which is a *testing.T
// method and is not part of TB. Run type-asserts the passed value to that method
// set: when present (a real *testing.T) it emits a subtest per scenario and per
// evaluator; when absent (a fake recorder, or a *testing.B, whose Run signature
// differs) it falls back to flat, line-per-assessment rendering. Presentation is
// informational only — it renders the verdicts but does not itself fail the test
// on a failing verdict. Use RequirePass or RequireVerified to gate on the report.
package evaltest

import (
	"context"
	"testing"

	"github.com/looprig/eval"
)

// TB is the minimal subset of testing.TB that evaltest uses. Both *testing.T and
// testing.TB satisfy it, so a caller passes a *testing.T unchanged; a test may
// supply a fake implementation to capture presented output. The variadic any is
// the standard printf logging boundary and is narrowed immediately by the format
// verbs.
type TB interface {
	// Helper marks the calling function as a test helper so failures are
	// attributed to the caller's line.
	Helper()
	// Logf records informational, non-failing output.
	Logf(format string, args ...any)
	// Errorf records a failure and continues (it never calls runtime.Goexit), so
	// the caller can still return the complete report.
	Errorf(format string, args ...any)
}

// subtestRunner is the *testing.T subset that lets evaltest emit subtests. Only a
// concrete *testing.T satisfies it; a *testing.B has a different Run signature and
// a fake recorder has none, so both fall back to flat rendering.
type subtestRunner interface {
	Run(name string, f func(*testing.T)) bool
}

// contextProvider is the optional Context accessor exposed by *testing.T on Go
// 1.24+. When the passed TB provides it, evaltest runs under the test's context
// so cancellation and deadlines propagate; otherwise it uses context.Background.
type contextProvider interface {
	Context() context.Context
}

// Run executes suite against target with evaluators via eval.Run under a default
// RunConfig (one trial, sequential), presents the result through tb, and returns
// the complete report. It never fails tb on a failing or errored verdict —
// presentation is informational; gate the report with RequirePass or
// RequireVerified. It calls tb.Helper so any failure it does signal is attributed
// to the caller.
//
// When eval.Run returns a non-nil error — a preflight rejection (an ill-formed
// suite or config, a nil target or evaluator) or a context cancellation — Run
// surfaces it through tb.Errorf with a safe message and still returns whatever
// report exists (the zero report for a preflight failure, or the partial report
// for a cancellation). The eval package guarantees its error strings never echo
// untrusted content, so the surfaced message is safe.
func Run(tb TB, suite eval.Suite, target eval.Target, evaluators ...eval.Evaluator) eval.Report {
	tb.Helper()
	ctx := contextFrom(tb)
	report, err := eval.Run(ctx, eval.RunConfig{}, suite, target, evaluators...)
	if err != nil {
		// eval's errors are safe to render: they draw on a fixed vocabulary and
		// never embed conversation, tool, or judge content.
		tb.Errorf("evaltest: run failed: %s", err.Error())
	}
	present(tb, report)
	return report
}

// RunScenario is the single-scenario convenience: it wraps scenario in a
// one-scenario suite (reusing the scenario's own Name and Revision for suite
// identity) and delegates to Run. An ill-formed scenario is rejected by eval.Run's
// preflight and surfaced through tb exactly as in Run.
func RunScenario(tb TB, scenario eval.Scenario, target eval.Target, evaluators ...eval.Evaluator) eval.Report {
	tb.Helper()
	suite := eval.Suite{
		Name:      scenario.Name,
		Revision:  scenario.Revision,
		Scenarios: []eval.Scenario{scenario},
	}
	return Run(tb, suite, target, evaluators...)
}

// contextFrom returns the test's context when tb exposes one (Go 1.24+
// *testing.T), else context.Background.
func contextFrom(tb TB) context.Context {
	if cp, ok := tb.(contextProvider); ok {
		if ctx := cp.Context(); ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

// present renders the report through tb: a summary line, then a subtest tree when
// tb is a real *testing.T, or flat per-assessment lines otherwise. It renders
// nothing for an empty report (no samples), so a preflight failure does not emit a
// spurious summary.
func present(tb TB, report eval.Report) {
	tb.Helper()
	if len(report.Samples) == 0 {
		return
	}
	tb.Logf("eval report %s: %s", report.ID, renderSummary(report.Summary))
	if sr, ok := tb.(subtestRunner); ok {
		presentSubtests(sr, report)
		return
	}
	presentFlat(tb, report)
}

// presentFlat renders each sample as flat log lines: a target-stage error, or one
// line per assessment. It is the fallback when no *testing.T is available.
func presentFlat(tb TB, report eval.Report) {
	tb.Helper()
	for i := range report.Samples {
		s := report.Samples[i]
		name := scenarioName(s)
		if s.TargetErr != nil {
			// TargetError.Error() is a fixed, safe string; the underlying cause is
			// never echoed.
			tb.Logf("%s: %s", name, s.TargetErr.Error())
			continue
		}
		for _, a := range s.Assessments {
			tb.Logf("%s: %s", name, renderAssessment(a))
		}
	}
}

// presentSubtests renders each sample as a subtest named by scenario, with a
// nested subtest per evaluator. The subtests are informational: they log the
// rendered verdict and do not fail, so evaltest.Run does not gate on the report.
func presentSubtests(sr subtestRunner, report eval.Report) {
	for i := range report.Samples {
		s := report.Samples[i]
		sr.Run(scenarioName(s), func(t *testing.T) {
			if s.TargetErr != nil {
				t.Logf("target: %s", s.TargetErr.Error())
				return
			}
			for _, a := range s.Assessments {
				a := a
				t.Run(string(a.Evaluator), func(t *testing.T) {
					t.Logf("%s", renderAssessment(a))
				})
			}
		})
	}
}
