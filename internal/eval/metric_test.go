package eval

import (
	"context"
	"testing"
)

func TestContainsMeasure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		actual     string
		expected   string
		wantValue  float64
		wantPassed bool
	}{
		{name: "contains expected", actual: "the answer is 4", expected: "4", wantValue: 1, wantPassed: true},
		{name: "missing expected", actual: "the answer is five", expected: "4", wantValue: 0, wantPassed: false},
		{name: "case-insensitive", actual: "HELLO World", expected: "hello", wantValue: 1, wantPassed: true},
		{name: "empty expected is vacuous", actual: "anything", expected: "", wantValue: 1, wantPassed: true},
		{name: "empty actual and empty expected is vacuous", actual: "", expected: "", wantValue: 1, wantPassed: true},
		{name: "empty actual with expectation fails", actual: "", expected: "x", wantValue: 0, wantPassed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := TestCase{Name: tt.name, ActualOutput: tt.actual, ExpectedOutput: tt.expected}
			got, err := Contains{}.Measure(context.Background(), tc)
			if err != nil {
				t.Fatalf("Measure() error = %v", err)
			}
			if got.Value != tt.wantValue {
				t.Errorf("Value = %v, want %v", got.Value, tt.wantValue)
			}
			if got.Passed != tt.wantPassed {
				t.Errorf("Passed = %v, want %v", got.Passed, tt.wantPassed)
			}
			if got.Metric != "contains" {
				t.Errorf("Metric = %q, want %q", got.Metric, "contains")
			}
		})
	}
}
