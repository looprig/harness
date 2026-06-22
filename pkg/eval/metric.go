package eval

import (
	"context"
	"strings"
)

// containsMetricName is the single source of truth for this metric's label, used
// by both Name and the Score it returns so the two can never drift.
const containsMetricName = "contains"

// Contains is a deterministic, case-driven Metric: it passes when ActualOutput
// contains the case's ExpectedOutput as a case-insensitive substring. An empty
// ExpectedOutput is vacuously satisfied (Value 1.0, Passed true) regardless of
// ActualOutput — a case with no expectation always passes this metric, by
// design. Keeping the expectation on the case (not on the metric) lets golden
// files be self-describing.
type Contains struct{}

// Name identifies the metric in Scores and errors.
func (Contains) Name() string { return containsMetricName }

// Measure scores tc.ActualOutput against tc.ExpectedOutput.
func (Contains) Measure(_ context.Context, tc TestCase) (Score, error) {
	value := 1.0
	reason := "no ExpectedOutput to check"
	if tc.ExpectedOutput != "" {
		if strings.Contains(strings.ToLower(tc.ActualOutput), strings.ToLower(tc.ExpectedOutput)) {
			value = 1.0
			reason = "ActualOutput contains ExpectedOutput"
		} else {
			value = 0.0
			reason = "ExpectedOutput not found in ActualOutput"
		}
	}
	return Score{
		Metric:    containsMetricName,
		Value:     value,
		Threshold: 1.0,
		Passed:    value >= 1.0,
		Reason:    reason,
	}, nil
}
