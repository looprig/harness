// Package eval is a small, dependency-free evaluation framework for LLM agents,
// modeled on deepeval. A TestCase carries an input and the agent's actual
// output; a Metric scores a TestCase; Evaluate runs cases against metrics.
//
// The package imports only the standard library. The LLM-judge metric depends
// on the local Completer interface (judge.go), not internal/llm, so the
// framework stays at the bottom of the import graph and any agent can use it.
package eval

import "context"

// TestCase is one evaluation case: a golden input and (after a run) the agent's
// actual output, plus an optional reference output and grounding context.
type TestCase struct {
	Name           string
	Input          string
	ActualOutput   string
	ExpectedOutput string
	Context        []string
}

// Score is one metric's verdict on one TestCase. Value is normalized to [0,1].
type Score struct {
	Metric    string
	Value     float64
	Threshold float64
	Passed    bool
	Reason    string
}

// Result groups a TestCase with every metric's Score. Passed is true only when
// every metric passed (vacuously true with no metrics).
type Result struct {
	Case   TestCase
	Scores []Score
	Passed bool
}

// Metric scores a single TestCase. Implementations must be safe to call
// concurrently for distinct cases.
type Metric interface {
	Name() string
	Measure(ctx context.Context, tc TestCase) (Score, error)
}

// Runner produces a TestCase's ActualOutput from its Input. An agent adapter
// implements this; the framework never constructs agents itself.
type Runner interface {
	Run(ctx context.Context, input string) (string, error)
}

// RunCases returns a copy of cases with ActualOutput filled by r. The first
// Runner error aborts and is returned wrapped in a RunError. The input slice is
// never mutated.
func RunCases(ctx context.Context, r Runner, cases []TestCase) ([]TestCase, error) {
	out := make([]TestCase, len(cases))
	copy(out, cases)
	for i := range out {
		got, err := r.Run(ctx, out[i].Input)
		if err != nil {
			return nil, &RunError{Case: out[i].Name, Cause: err}
		}
		out[i].ActualOutput = got
	}
	return out, nil
}

// Evaluate scores every case against every metric. A metric error aborts and is
// returned wrapped in a MeasureError. A Result's Passed is the AND of its metric
// Passed flags (vacuously true with no metrics).
func Evaluate(ctx context.Context, cases []TestCase, metrics []Metric) ([]Result, error) {
	results := make([]Result, 0, len(cases))
	for _, tc := range cases {
		r := Result{Case: tc, Passed: true}
		for _, m := range metrics {
			s, err := m.Measure(ctx, tc)
			if err != nil {
				return nil, &MeasureError{Metric: m.Name(), Case: tc.Name, Cause: err}
			}
			r.Scores = append(r.Scores, s)
			if !s.Passed {
				r.Passed = false
			}
		}
		results = append(results, r)
	}
	return results, nil
}
