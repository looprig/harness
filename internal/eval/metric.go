package eval

import (
	"context"
	"strings"
)

// Contains is a deterministic, case-driven Metric: it passes when ActualOutput
// contains the case's ExpectedOutput as a case-insensitive substring. An empty
// ExpectedOutput is vacuously satisfied (Value 1.0). Keeping the expectation on
// the case (not on the metric) lets golden files be self-describing.
type Contains struct{}

// Name identifies the metric in Scores and errors.
func (Contains) Name() string { return "contains" }

// Measure scores tc.ActualOutput against tc.ExpectedOutput.
func (Contains) Measure(_ context.Context, tc TestCase) (Score, error) {
	value := 1.0
	if tc.ExpectedOutput != "" {
		if strings.Contains(strings.ToLower(tc.ActualOutput), strings.ToLower(tc.ExpectedOutput)) {
			value = 1.0
		} else {
			value = 0.0
		}
	}
	return Score{
		Metric:    "contains",
		Value:     value,
		Threshold: 1.0,
		Passed:    value >= 1.0,
		Reason:    "ActualOutput contains ExpectedOutput",
	}, nil
}
