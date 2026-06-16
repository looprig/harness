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
//
// Judge scores against the free-form Criteria using only the case's Input and
// ActualOutput; it deliberately does NOT consult ExpectedOutput. Reference-string
// checking is the Contains metric's job — pair the two when a case has both a
// gold substring and a qualitative rubric.
type Judge struct {
	Criteria  string
	Threshold float64
	Model     Completer
}

// judgeMetricName is the single source of truth for this metric's label, used by
// both Name and the Score it returns so the two can never drift.
const judgeMetricName = "judge"

// Name identifies the metric in Scores and errors.
func (Judge) Name() string { return judgeMetricName }

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
		Metric:    judgeMetricName,
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
	errNoScoreLine    = errors.New("judge response has no SCORE line")
	errScoreRange     = errors.New("judge score is outside [0,1]")
	errDuplicateScore = errors.New("judge response has more than one SCORE line")
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
			if gotScore {
				return 0, "", &JudgeParseError{Raw: raw, Cause: errDuplicateScore}
			}
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
