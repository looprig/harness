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
