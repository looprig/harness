# Togo Prompt + Reusable `internal/eval` Framework — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Extract and expand the coding agent's system prompt into an `agents/coding/prompts` subpackage (identity = "Togo"), and add a reusable, deepeval-style `internal/eval` framework (deterministic + LLM-judge metrics) with a golden-set for the agent.

**Architecture:** `internal/eval` is a stdlib-only framework at the bottom of the import graph: typed `TestCase`/`Score`/`Result`, small `Metric`/`Runner`/`Completer` interfaces, a deterministic `Contains` metric, a GEval-style `Judge` metric, and `LoadCases`/`Evaluate`/`RunCases`. The `coding` agent imports it: a golden-set drives an offline data-validity test, and a build-tagged integration test wires the live Togo agent (as a `Runner`) and a model-backed `Completer` (as the judge). The rename is identity-only — package `coding`/type `Coding` stay; only the banner display name and prompt identity become "Togo".

**Tech Stack:** Go 1.26, standard library only (`encoding/json`, `strings`, `strconv`, `os`, `path/filepath`, `context`). Tests are table-driven and run under `-race`; the integration test is `//go:build integration`. Design doc: `docs/plans/2026-06-15-togo-prompt-eval-framework-design.md`.

**Conventions (CLAUDE.md):** SOLID; interfaces first; every error is a typed struct (sentinels only for context-free leaves); table-driven tests covering happy/boundary/error/edge; `go test -race ./...`; `make secure` before committing.

**Working branch:** `feature/togo-eval-framework` (already created off `main`; the design doc is committed there).

---

### Task 1: `internal/eval` core types, typed errors, and the `Contains` metric

**Files:**
- Create: `internal/eval/eval.go` (types + `Metric`/`Runner` interfaces)
- Create: `internal/eval/errors.go` (typed errors)
- Create: `internal/eval/metric.go` (`Contains`)
- Test: `internal/eval/metric_test.go`

**Step 1: Write the failing test**

`internal/eval/metric_test.go`:

```go
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
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/eval/ -run TestContainsMeasure`
Expected: FAIL — `undefined: TestCase` / `undefined: Contains` (package doesn't compile yet).

**Step 3: Write minimal implementation**

`internal/eval/eval.go`:

```go
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
```

`internal/eval/errors.go`:

```go
package eval

import "fmt"

// RunError wraps a Runner failure for a named case.
type RunError struct {
	Case  string
	Cause error
}

func (e *RunError) Error() string { return fmt.Sprintf("eval: run case %q: %v", e.Case, e.Cause) }
func (e *RunError) Unwrap() error { return e.Cause }

// MeasureError wraps a Metric failure for a named case.
type MeasureError struct {
	Metric string
	Case   string
	Cause  error
}

func (e *MeasureError) Error() string {
	return fmt.Sprintf("eval: metric %q on case %q: %v", e.Metric, e.Case, e.Cause)
}
func (e *MeasureError) Unwrap() error { return e.Cause }

// LoadError wraps a golden-set load failure for a path.
type LoadError struct {
	Path  string
	Cause error
}

func (e *LoadError) Error() string { return fmt.Sprintf("eval: load %q: %v", e.Path, e.Cause) }
func (e *LoadError) Unwrap() error { return e.Cause }

// JudgeParseError reports an unparseable judge response.
type JudgeParseError struct {
	Raw   string
	Cause error
}

func (e *JudgeParseError) Error() string {
	return fmt.Sprintf("eval: parse judge response: %v", e.Cause)
}
func (e *JudgeParseError) Unwrap() error { return e.Cause }
```

`internal/eval/metric.go`:

```go
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
```

**Step 4: Run test to verify it passes**

Run: `go test -race ./internal/eval/ -run TestContainsMeasure -v`
Expected: PASS (4 subtests).

**Step 5: Commit**

```bash
git add internal/eval/eval.go internal/eval/errors.go internal/eval/metric.go internal/eval/metric_test.go
git commit -m "feat(eval): add eval framework core types and Contains metric"
```

---

### Task 2: `RunCases` and `Evaluate`

**Files:**
- Modify: `internal/eval/eval.go` (append `RunCases`, `Evaluate`)
- Test: `internal/eval/eval_test.go`

**Step 1: Write the failing test**

`internal/eval/eval_test.go`:

```go
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
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/eval/ -run 'TestRunCases|TestEvaluate'`
Expected: FAIL — `undefined: RunCases` / `undefined: Evaluate`.

**Step 3: Write minimal implementation**

Append to `internal/eval/eval.go`:

```go
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
```

**Step 4: Run test to verify it passes**

Run: `go test -race ./internal/eval/ -run 'TestRunCases|TestEvaluate' -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/eval/eval.go internal/eval/eval_test.go
git commit -m "feat(eval): add RunCases and Evaluate"
```

---

### Task 3: `Judge` LLM-judge metric and `Completer`

**Files:**
- Create: `internal/eval/judge.go` (`Completer`, `Judge`, `parseJudge`)
- Test: `internal/eval/judge_test.go`

**Step 1: Write the failing test**

`internal/eval/judge_test.go`:

```go
package eval

import (
	"context"
	"errors"
	"testing"
)

// fakeCompleter returns resp (or err) for every Complete call.
type fakeCompleter struct {
	resp string
	err  error
}

func (f fakeCompleter) Complete(_ context.Context, _ string) (string, error) { return f.resp, f.err }

func TestJudgeMeasure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		resp       string
		respErr    error
		threshold  float64
		wantValue  float64
		wantPassed bool
		wantErrAs  any // nil, or a **JudgeParseError sentinel-check flag
	}{
		{name: "passes above threshold", resp: "SCORE: 0.9\nREASON: solid", threshold: 0.7, wantValue: 0.9, wantPassed: true},
		{name: "fails below threshold", resp: "SCORE: 0.3\nREASON: weak", threshold: 0.7, wantValue: 0.3, wantPassed: false},
		{name: "no score line is parse error", resp: "garbage", threshold: 0.7, wantErrAs: true},
		{name: "out of range is parse error", resp: "SCORE: 2.0\nREASON: nope", threshold: 0.7, wantErrAs: true},
		{name: "completer error propagates", respErr: errors.New("network"), threshold: 0.7, wantErrAs: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j := Judge{Criteria: "is it correct?", Threshold: tt.threshold, Model: fakeCompleter{resp: tt.resp, err: tt.respErr}}
			got, err := j.Measure(context.Background(), TestCase{Name: tt.name, Input: "q", ActualOutput: "a"})
			if tt.wantErrAs == true { // expect a JudgeParseError
				var pe *JudgeParseError
				if !errors.As(err, &pe) {
					t.Fatalf("error = %v, want *JudgeParseError", err)
				}
				return
			}
			if tt.respErr != nil { // expect the completer error to propagate
				if err == nil {
					t.Fatal("error = nil, want completer error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Measure() error = %v", err)
			}
			if got.Value != tt.wantValue {
				t.Errorf("Value = %v, want %v", got.Value, tt.wantValue)
			}
			if got.Passed != tt.wantPassed {
				t.Errorf("Passed = %v, want %v", got.Passed, tt.wantPassed)
			}
		})
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/eval/ -run TestJudgeMeasure`
Expected: FAIL — `undefined: Judge` / `undefined: Completer`.

**Step 3: Write minimal implementation**

`internal/eval/judge.go`:

```go
package eval

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// Completer is the minimal model surface the LLM-judge metric needs: turn a
// prompt into a completion string. Defining it here keeps eval free of any
// internal/llm import; an agent adapter supplies a real implementation.
type Completer interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// Judge is a GEval-style Metric: it asks a model (via Completer) to score how
// well ActualOutput satisfies Criteria on a 0..1 scale. It is unit-testable with
// a fake Completer and integration-tested with a real model.
type Judge struct {
	Criteria  string
	Threshold float64
	Model     Completer
}

// Name identifies the metric in Scores and errors.
func (Judge) Name() string { return "judge" }

// Measure asks the judge model to score tc.ActualOutput against Criteria.
func (j Judge) Measure(ctx context.Context, tc TestCase) (Score, error) {
	raw, err := j.Model.Complete(ctx, judgePrompt(j.Criteria, tc.Input, tc.ActualOutput))
	if err != nil {
		return Score{}, err
	}
	value, reason, err := parseJudge(raw)
	if err != nil {
		return Score{}, err
	}
	return Score{
		Metric:    "judge",
		Value:     value,
		Threshold: j.Threshold,
		Passed:    value >= j.Threshold,
		Reason:    reason,
	}, nil
}

// judgePrompt builds the instruction sent to the judge model, asking for a
// two-line "SCORE:"/"REASON:" reply that parseJudge extracts.
func judgePrompt(criteria, input, output string) string {
	var b strings.Builder
	b.WriteString("You are an impartial evaluator. Score how well the RESPONSE satisfies the CRITERIA.\n")
	b.WriteString("Reply with exactly two lines:\n")
	b.WriteString("SCORE: <a number from 0.0 to 1.0>\n")
	b.WriteString("REASON: <one sentence>\n\n")
	b.WriteString("CRITERIA:\n")
	b.WriteString(criteria)
	b.WriteString("\n\nINPUT:\n")
	b.WriteString(input)
	b.WriteString("\n\nRESPONSE:\n")
	b.WriteString(output)
	return b.String()
}

var (
	errNoScoreLine = errors.New("judge response has no SCORE line")
	errScoreRange  = errors.New("judge score is outside [0,1]")
)

// parseJudge extracts the 0..1 score and reason from "SCORE: 0.8\nREASON: ...".
// A missing or out-of-range score is a JudgeParseError carrying the raw text.
func parseJudge(raw string) (float64, string, error) {
	var score float64
	var reason string
	gotScore := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "SCORE:"):
			v, err := strconv.ParseFloat(strings.TrimSpace(line[len("SCORE:"):]), 64)
			if err != nil {
				return 0, "", &JudgeParseError{Raw: raw, Cause: err}
			}
			score, gotScore = v, true
		case strings.HasPrefix(upper, "REASON:"):
			reason = strings.TrimSpace(line[len("REASON:"):])
		}
	}
	if !gotScore {
		return 0, "", &JudgeParseError{Raw: raw, Cause: errNoScoreLine}
	}
	if score < 0 || score > 1 {
		return 0, "", &JudgeParseError{Raw: raw, Cause: errScoreRange}
	}
	return score, reason, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test -race ./internal/eval/ -run TestJudgeMeasure -v`
Expected: PASS (5 subtests).

**Step 5: Commit**

```bash
git add internal/eval/judge.go internal/eval/judge_test.go
git commit -m "feat(eval): add GEval-style Judge metric and Completer interface"
```

---

### Task 4: `LoadCases`

**Files:**
- Create: `internal/eval/load.go`
- Test: `internal/eval/load_test.go`

**Step 1: Write the failing test**

`internal/eval/load_test.go`:

```go
package eval

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCases(t *testing.T) {
	t.Parallel()
	t.Run("loads json sorted by filename, ignoring non-json", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "b.json"), `{"name":"second","input":"i2","expectedOutput":"o2"}`)
		mustWrite(t, filepath.Join(dir, "a.json"), `{"name":"first","input":"i1","expectedOutput":"o1"}`)
		mustWrite(t, filepath.Join(dir, "notes.txt"), `ignored`)
		cases, err := LoadCases(dir)
		if err != nil {
			t.Fatalf("LoadCases() error = %v", err)
		}
		if len(cases) != 2 {
			t.Fatalf("len = %d, want 2", len(cases))
		}
		if cases[0].Name != "first" || cases[1].Name != "second" {
			t.Errorf("order = %q,%q, want first,second", cases[0].Name, cases[1].Name)
		}
	})
	t.Run("missing dir is a LoadError", func(t *testing.T) {
		t.Parallel()
		_, err := LoadCases(filepath.Join(t.TempDir(), "nope"))
		var le *LoadError
		if !errors.As(err, &le) {
			t.Fatalf("error = %v, want *LoadError", err)
		}
	})
	t.Run("malformed json is a LoadError", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "bad.json"), `{not json`)
		_, err := LoadCases(dir)
		var le *LoadError
		if !errors.As(err, &le) {
			t.Fatalf("error = %v, want *LoadError", err)
		}
	})
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/eval/ -run TestLoadCases`
Expected: FAIL — `undefined: LoadCases`.

**Step 3: Write minimal implementation**

`internal/eval/load.go`:

```go
package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// goldenCase is the on-disk JSON shape of a TestCase.
type goldenCase struct {
	Name           string   `json:"name"`
	Input          string   `json:"input"`
	ExpectedOutput string   `json:"expectedOutput,omitempty"`
	Context        []string `json:"context,omitempty"`
}

// LoadCases reads every *.json file in dir (non-recursively) as a TestCase.
// os.ReadDir returns entries sorted by filename, so ordering is deterministic.
// A missing dir, an unreadable file, or malformed JSON is a LoadError naming the
// offending path.
func LoadCases(dir string) ([]TestCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, &LoadError{Path: dir, Cause: err}
	}
	var cases []TestCase
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, &LoadError{Path: path, Cause: err}
		}
		var gc goldenCase
		if err := json.Unmarshal(data, &gc); err != nil {
			return nil, &LoadError{Path: path, Cause: err}
		}
		cases = append(cases, TestCase{
			Name:           gc.Name,
			Input:          gc.Input,
			ExpectedOutput: gc.ExpectedOutput,
			Context:        gc.Context,
		})
	}
	return cases, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test -race ./internal/eval/ -v`
Expected: PASS (all `internal/eval` tests).

**Step 5: Commit**

```bash
git add internal/eval/load.go internal/eval/load_test.go
git commit -m "feat(eval): add LoadCases golden-set loader"
```

---

### Task 5: `prompts` subpackage + Togo system prompt, wired into the agent

**Files:**
- Create: `agents/coding/prompts/system.go`
- Test: `agents/coding/prompts/system_test.go`
- Modify: `agents/coding/agent.go` (delete inline const `:33-43`; import prompts; use `prompts.SystemPrompt` at `:80`)
- Modify: `agents/coding/agent_test.go:174` (`codingPersonaPrompt` → `prompts.SystemPrompt`)

**Step 1: Write the failing test**

`agents/coding/prompts/system_test.go`:

```go
package prompts

import (
	"strings"
	"testing"
)

func TestSystemPrompt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
	}{
		{name: "identifies as Togo", want: "Togo"},
		{name: "names an auto-approved tool", want: "ReadFile"},
		{name: "names an approval-gated tool", want: "Bash"},
		{name: "mentions approval", want: "approv"},
		{name: "mentions secrets", want: "secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(SystemPrompt, tt.want) {
				t.Errorf("SystemPrompt is missing %q", tt.want)
			}
		})
	}
}

func TestSystemPromptNonEmpty(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(SystemPrompt) == "" {
		t.Fatal("SystemPrompt is empty")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./agents/coding/prompts/`
Expected: FAIL — package/`SystemPrompt` undefined.

**Step 3: Write minimal implementation**

`agents/coding/prompts/system.go`:

```go
// Package prompts holds Togo's system prompt as a verbatim, exported constant.
// The prompt is never constructed at runtime or interpolated with external data
// (CLAUDE.md: no external data into prompts); it is a single immutable string,
// imported by agents/coding.
package prompts

// SystemPrompt is Togo's identity: a careful software engineer that works through
// tools, plans before mutating the workspace or running a shell, and is explicit
// about which tools require the user's approval.
const SystemPrompt = `You are Togo, an interactive CLI tool that helps users with software engineering tasks. Use the tools available to you to assist the user.

You are highly capable and can help users complete ambitious tasks. Keep going until the user's query is completely resolved before yielding back to them. Only stop when you are sure the problem is solved. Do not guess or make up answers.

# Personality

Your default tone is concise, direct, and friendly. Communicate efficiently, keeping the user informed about what you are doing without unnecessary detail. Prioritize actionable guidance. Unless explicitly asked, avoid verbose explanations.

# Doing tasks

The user will primarily ask you to perform software engineering tasks: solving bugs, adding functionality, refactoring, and explaining code. When given an unclear or generic instruction, consider it in the context of software engineering and the current working directory.

For exploratory questions ("what could we do about X?", "how should we approach this?"), respond in 2-3 sentences with a recommendation and the main trade-off. Present it as something the user can redirect, not a decided plan. Don't implement until the user agrees.

# Communicating while you work

Before making tool calls, send a brief preamble (1-2 sentences) explaining what you are about to do. When you find something relevant, change direction, or hit a blocker, say so in one sentence. Assume the user can't see tool calls — only your text output. State results and decisions directly.

# Writing code

Fix the problem at the root cause rather than applying surface-level patches. Avoid unneeded complexity. Keep changes consistent with the style of the existing codebase and focused on the task. Never guess a file's contents — read it first. Prefer editing existing files to creating new ones.

# Tools and permissions

You work through tools. Some run automatically because they are read-only or otherwise safe: ReadFile, Glob, Grep, Todo, AskUser, and Subagent. Others change the workspace, run a shell, or reach the network, so they require the user's approval before they run: WriteFile, EditFile, Bash, Fetch, and WebSearch. Before any change that writes a file or runs a command, briefly explain your plan so the user can follow and approve it.

# Validating your work

If the codebase has tests or a build system, use them to verify your work. Start with the narrowest test covering your change, then broaden as confidence grows. Do not attempt to fix unrelated test failures; mention them instead.

# Reversibility and risky actions

Consider the reversibility and blast radius of actions. Take local, reversible actions freely. For actions that are hard to reverse or affect shared systems, check with the user before proceeding. The cost of pausing is low; the cost of an unwanted action is high.

# Security and secrets

Do not read, display, or transmit credentials, API keys, secrets, or personally identifiable information. If secrets appear in files, note their presence but do not display their values. Never write a secret into code, logs, or command arguments.`
```

Then edit `agents/coding/agent.go`:
- Delete the `codingPersonaPrompt` const and its doc comment (`:33-43`).
- Add the import `"github.com/inventivepotter/urvi/agents/coding/prompts"`.
- Change `:80` from `spec := model.Spec(apiKey, codingPersonaPrompt)` to `spec := model.Spec(apiKey, prompts.SystemPrompt)`.

Then edit `agents/coding/agent_test.go:174` from `model.Spec("unused-key", codingPersonaPrompt)` to `model.Spec("unused-key", prompts.SystemPrompt)` and add the prompts import.

**Step 4: Run test to verify it passes**

Run: `CGO_ENABLED=0 go build -trimpath ./... && go test -race ./agents/coding/... -v`
Expected: PASS — prompts tests green; `agents/coding` builds and its tests still pass (no `codingPersonaPrompt` references remain).

**Step 5: Commit**

```bash
git add agents/coding/prompts/ agents/coding/agent.go agents/coding/agent_test.go
git commit -m "feat(coding): extract Togo system prompt into prompts subpackage"
```

---

### Task 6: Togo display name in the banner

**Files:**
- Modify: `cmd/cli/main.go` (add `agentDisplayNames` map + `agentDisplayName`; use it at `:154`)
- Test: `cmd/cli/main_test.go` (add `TestAgentDisplayName`)

**Step 1: Write the failing test**

Add to `cmd/cli/main_test.go`:

```go
func TestAgentDisplayName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		agentName string
		want      string
	}{
		{name: "coding agent displays as Togo", agentName: "coding", want: "Togo"},
		{name: "unmapped agent falls back to its name", agentName: "personal-assistant", want: "personal-assistant"},
		{name: "empty name falls back to empty", agentName: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := agentDisplayName(tt.agentName); got != tt.want {
				t.Errorf("agentDisplayName(%q) = %q, want %q", tt.agentName, got, tt.want)
			}
		})
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./cmd/cli/ -run TestAgentDisplayName`
Expected: FAIL — `undefined: agentDisplayName`.

**Step 3: Write minimal implementation**

In `cmd/cli/main.go`, near `agentDescriptions`/`agentDescription` (around `:37-43`), add:

```go
// agentDisplayNames maps an agent's registry name to its user-facing display
// name (shown in the banner). Unmapped agents fall back to the registry name.
var agentDisplayNames = map[string]string{
	"coding": "Togo",
}

// agentDisplayName returns the banner display name for name, falling back to
// name itself when there is no override.
func agentDisplayName(name string) string {
	if d, ok := agentDisplayNames[name]; ok {
		return d
	}
	return name
}
```

Change the banner construction at `:154` from:

```go
screen := tui.New(ctx, agent, open, tui.AgentBanner{Name: name, Description: agentDescription(name)})
```

to:

```go
screen := tui.New(ctx, agent, open, tui.AgentBanner{Name: agentDisplayName(name), Description: agentDescription(name)})
```

**Step 4: Run test to verify it passes**

Run: `go test -race ./cmd/cli/ -run TestAgentDisplayName -v && CGO_ENABLED=0 go build -trimpath ./...`
Expected: PASS; build clean.

**Step 5: Commit**

```bash
git add cmd/cli/main.go cmd/cli/main_test.go
git commit -m "feat(cli): display the coding agent as Togo in the banner"
```

---

### Task 7: Golden-set sample + offline validity test

**Files:**
- Create: `agents/coding/golden-set/cases/greeting.json`
- Create: `agents/coding/golden-set/README.md`
- Test: `agents/coding/golden_set_test.go`

**Step 1: Write the failing test**

`agents/coding/golden_set_test.go`:

```go
package coding

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/eval"
)

// TestGoldenSetLoads proves the checked-in golden cases are valid JSON that
// LoadCases can parse, and that at least one case is present.
func TestGoldenSetLoads(t *testing.T) {
	t.Parallel()
	cases, err := eval.LoadCases("golden-set/cases")
	if err != nil {
		t.Fatalf("LoadCases() error = %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("golden-set/cases has no cases")
	}
	for _, c := range cases {
		if c.Name == "" || c.Input == "" {
			t.Errorf("case %+v missing Name or Input", c)
		}
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./agents/coding/ -run TestGoldenSetLoads`
Expected: FAIL — `LoadError` (the `golden-set/cases` directory does not exist yet).

**Step 3: Write minimal implementation**

`agents/coding/golden-set/cases/greeting.json`:

```json
{
  "name": "greeting",
  "input": "Reply with exactly the single word: hello",
  "expectedOutput": "hello"
}
```

`agents/coding/golden-set/README.md`:

```markdown
# Togo golden-set

Golden input/output pairs for evaluating the Togo coding agent.

Each `cases/*.json` file is one `internal/eval.TestCase`:

| field | meaning |
|---|---|
| `name` | case identifier |
| `input` | the prompt given to the agent |
| `expectedOutput` | substring the answer must contain (the `Contains` metric) |
| `context` | optional grounding strings |

These cases are consumed by the offline validity test (`golden_set_test.go`,
which only checks that they parse) and by the build-tagged integration test
(`eval_integration_test.go`), which runs the live agent against them with the
`Contains` and `Judge` metrics. Keep `input`s answerable without
approval-gated tools (no file writes / shell), so the integration run does not
block on a permission gate.
```

**Step 4: Run test to verify it passes**

Run: `go test -race ./agents/coding/ -run TestGoldenSetLoads -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add agents/coding/golden-set/ agents/coding/golden_set_test.go
git commit -m "feat(coding): add Togo golden-set with offline validity test"
```

---

### Task 8: Build-tagged integration test (live Togo + judge)

> This is the only task that touches the live model. It is excluded from the
> default `go test ./...` by `//go:build integration` and needs `LLM_API_KEY`
> set. **Two spots to confirm against the codebase while implementing** (both
> noted inline): the exact `content.AgenticMessages`/`UserMessage` construction
> for the judge request (mirror how `internal/agent/session` builds a turn), and
> that `StreamReader.Next()` returns `io.EOF` at end of stream.

**Files:**
- Create: `agents/coding/eval_integration_test.go`

**Step 1: Write the test (no prior failing-unit step — it is the deliverable)**

`agents/coding/eval_integration_test.go`:

```go
//go:build integration

package coding

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/eval"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/auto"
)

// togoRunner adapts the live Togo agent to eval.Runner: it streams one turn for
// the input prompt and projects the terminal TurnDone.Message to text (mirroring
// agents/coding/subagent_factory.go's aiMessageText projection).
type togoRunner struct{ agent *Coding }

func (r togoRunner) Run(ctx context.Context, input string) (string, error) {
	sr, err := r.agent.StreamBlocks(ctx, []content.Block{&content.TextBlock{Text: input}})
	if err != nil {
		return "", err
	}
	defer func() { _ = sr.Close() }()
	var out string
	for {
		ev, err := sr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return "", err
		}
		if done, ok := ev.(event.TurnDone); ok {
			out = aiMessageText(done.Message) // reuse the package-internal projection
		}
	}
}

// modelCompleter adapts an llm.LLM to eval.Completer for the Judge metric.
type modelCompleter struct {
	client llm.LLM
	spec   llm.ModelSpec
}

func (m modelCompleter) Complete(ctx context.Context, prompt string) (string, error) {
	// CONFIRM AGAINST CODEBASE: build a single user-message AgenticMessages the
	// same way internal/agent/session does. Sketch:
	//   msgs := content.AgenticMessages{&content.UserMessage{Message: content.Message{
	//       Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: prompt}}}}}
	resp, err := m.client.Invoke(ctx, llm.Request{Model: m.spec, Messages: /* msgs */ nil})
	if err != nil {
		return "", err
	}
	return aiMessageText(resp.Message), nil
}

func TestTogoEvalIntegration(t *testing.T) {
	if os.Getenv("LLM_API_KEY") == "" {
		t.Skip("LLM_API_KEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	agent, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })

	cases, err := eval.LoadCases("golden-set/cases")
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}

	run, err := eval.RunCases(ctx, togoRunner{agent: agent}, cases)
	if err != nil {
		t.Fatalf("RunCases: %v", err)
	}

	// Judge client uses the same production model + key, with a judge system prompt.
	judgeSpec := model.Spec(os.Getenv("LLM_API_KEY"), "You are a strict evaluator.")
	judgeClient, err := auto.New(judgeSpec)
	if err != nil {
		t.Fatalf("auto.New: %v", err)
	}

	results, err := eval.Evaluate(ctx, run, []eval.Metric{
		eval.Contains{},
		eval.Judge{Criteria: "The response directly and correctly answers the input.", Threshold: 0.6,
			Model: modelCompleter{client: judgeClient, spec: judgeSpec}},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	for _, r := range results {
		if !r.Passed {
			var b strings.Builder
			for _, s := range r.Scores {
				b.WriteString(s.Metric + "=" + s.Reason + "; ")
			}
			t.Errorf("case %q failed: %s", r.Case.Name, b.String())
		}
	}
}
```

**Step 2: Verify it compiles under the integration tag**

Run: `go vet -tags integration ./agents/coding/`
Expected: compiles (after filling the `AgenticMessages` construction noted inline). Fix the two confirm-against-codebase spots until `go build -tags integration ./agents/coding/` is clean.

**Step 3: Run the live integration test (optional, needs a key)**

Run: `LLM_API_KEY=… go test -tags integration -race ./agents/coding/ -run TestTogoEvalIntegration -v`
Expected: PASS, or a clear per-case failure report. (Skips cleanly when `LLM_API_KEY` is unset.)

**Step 4: Commit**

```bash
git add agents/coding/eval_integration_test.go
git commit -m "test(coding): add build-tagged Togo eval integration test"
```

---

### Task 9: Full verification sweep

**Step 1: Default suite under race**

Run: `go test -race ./...`
Expected: PASS (integration test excluded by its build tag).

**Step 2: Integration compiles**

Run: `go build -tags integration ./...`
Expected: clean.

**Step 3: Security + lint gate**

Run: `make secure`
Expected: `lint` (vet + staticcheck + gosec) and `vuln` (go mod verify + govulncheck) clean. (`internal/eval` is local + stdlib-only — `go mod verify` is unaffected.)

**Step 4: Final commit if anything was adjusted**

```bash
git add -A
git commit -m "chore(eval): verification sweep — race tests, integration build, make secure"
```

---

## Definition of done

- `internal/eval` exists, stdlib-only, with `TestCase`/`Score`/`Result`, `Metric`/`Runner`/`Completer`, `Contains`, `Judge`, `LoadCases`, `RunCases`, `Evaluate`, and typed errors — all covered by offline table-driven tests under `-race`.
- The coding agent's prompt lives in `agents/coding/prompts.SystemPrompt`, identifies as **Togo**, and accurately names the real tools + approval gates; the inline `codingPersonaPrompt` is gone.
- The banner displays **Togo** for the coding agent; package `coding`/type `Coding` are unchanged.
- `agents/coding/golden-set/` holds a sample case + README, validated offline; a build-tagged integration test runs the live agent through `Contains` + `Judge`.
- `go test -race ./...` and `make secure` are green; `go build -tags integration ./...` is clean.
```
