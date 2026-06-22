package eval

import (
	"context"
	"errors"
	"testing"
)

// fakeRunner returns out (or err) for every Run call.
type fakeRunner struct {
	out string
	err error
}

func (f fakeRunner) Run(_ context.Context, _ string) (string, error) { return f.out, f.err }

// erroringMetric always fails Measure, to exercise the Evaluate error path.
type erroringMetric struct{}

func (erroringMetric) Name() string { return "boom" }
func (erroringMetric) Measure(_ context.Context, _ TestCase) (Score, error) {
	return Score{}, errors.New("kaboom")
}

func TestRunCases(t *testing.T) {
	t.Parallel()
	t.Run("fills actual output", func(t *testing.T) {
		t.Parallel()
		cases := []TestCase{{Name: "a", Input: "x"}}
		got, err := RunCases(context.Background(), fakeRunner{out: "y"}, cases)
		if err != nil {
			t.Fatalf("RunCases() error = %v", err)
		}
		if got[0].ActualOutput != "y" {
			t.Errorf("ActualOutput = %q, want %q", got[0].ActualOutput, "y")
		}
		if cases[0].ActualOutput != "" {
			t.Error("RunCases mutated the input slice")
		}
	})
	t.Run("runner error is a RunError", func(t *testing.T) {
		t.Parallel()
		_, err := RunCases(context.Background(), fakeRunner{err: errors.New("down")}, []TestCase{{Name: "a"}})
		var re *RunError
		if !errors.As(err, &re) {
			t.Fatalf("error = %v, want *RunError", err)
		}
		if re.Case != "a" {
			t.Errorf("RunError.Case = %q, want %q", re.Case, "a")
		}
	})
}

func TestEvaluate(t *testing.T) {
	t.Parallel()
	t.Run("passes when metric passes", func(t *testing.T) {
		t.Parallel()
		cases := []TestCase{{Name: "a", ActualOutput: "the answer is 4", ExpectedOutput: "4"}}
		res, err := Evaluate(context.Background(), cases, []Metric{Contains{}})
		if err != nil {
			t.Fatalf("Evaluate() error = %v", err)
		}
		if !res[0].Passed {
			t.Error("Passed = false, want true")
		}
	})
	t.Run("fails when metric fails", func(t *testing.T) {
		t.Parallel()
		cases := []TestCase{{Name: "a", ActualOutput: "nope", ExpectedOutput: "4"}}
		res, err := Evaluate(context.Background(), cases, []Metric{Contains{}})
		if err != nil {
			t.Fatalf("Evaluate() error = %v", err)
		}
		if res[0].Passed {
			t.Error("Passed = true, want false")
		}
	})
	t.Run("no metrics is vacuously passed", func(t *testing.T) {
		t.Parallel()
		res, err := Evaluate(context.Background(), []TestCase{{Name: "a"}}, nil)
		if err != nil {
			t.Fatalf("Evaluate() error = %v", err)
		}
		if !res[0].Passed {
			t.Error("Passed = false, want true (vacuous)")
		}
	})
	t.Run("metric error is a MeasureError", func(t *testing.T) {
		t.Parallel()
		_, err := Evaluate(context.Background(), []TestCase{{Name: "a"}}, []Metric{erroringMetric{}})
		var me *MeasureError
		if !errors.As(err, &me) {
			t.Fatalf("error = %v, want *MeasureError", err)
		}
		if me.Metric != "boom" || me.Case != "a" {
			t.Errorf("MeasureError = %+v, want Metric=boom Case=a", me)
		}
	})
}
