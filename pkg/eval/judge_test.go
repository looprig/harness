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
		name         string
		resp         string
		respErr      error
		threshold    float64
		wantValue    float64
		wantPassed   bool
		wantParseErr bool // expect a *JudgeParseError from parsing
	}{
		{name: "passes above threshold", resp: "SCORE: 0.9\nREASON: solid", threshold: 0.7, wantValue: 0.9, wantPassed: true},
		{name: "fails below threshold", resp: "SCORE: 0.3\nREASON: weak", threshold: 0.7, wantValue: 0.3, wantPassed: false},
		{name: "no score line is parse error", resp: "garbage", threshold: 0.7, wantParseErr: true},
		{name: "out of range is parse error", resp: "SCORE: 2.0\nREASON: nope", threshold: 0.7, wantParseErr: true},
		{name: "completer error propagates", respErr: errors.New("network"), threshold: 0.7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j := Judge{Criteria: "is it correct?", Threshold: tt.threshold, Model: fakeCompleter{resp: tt.resp, err: tt.respErr}}
			got, err := j.Measure(context.Background(), TestCase{Name: tt.name, Input: "q", ActualOutput: "a"})
			if tt.wantParseErr { // expect a JudgeParseError
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

func TestParseJudge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		raw        string
		wantValue  float64
		wantReason string
		wantErr    bool // expect a *JudgeParseError
	}{
		{name: "happy path", raw: "SCORE: 0.8\nREASON: ok", wantValue: 0.8, wantReason: "ok"},
		{name: "whitespace-padded score", raw: "SCORE:  0.5 ", wantValue: 0.5, wantReason: ""},
		{name: "trailing junk after number", raw: "SCORE: 0.5 out of 1", wantErr: true},
		{name: "missing score line", raw: "REASON: no score here", wantErr: true},
		{name: "out of range score", raw: "SCORE: 2.0\nREASON: nope", wantErr: true},
		{name: "duplicate score line", raw: "SCORE: 0.5\nSCORE: 0.6\nREASON: which", wantErr: true},
		{name: "missing reason allowed", raw: "SCORE: 0.5", wantValue: 0.5, wantReason: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			value, reason, err := parseJudge(tt.raw)
			if tt.wantErr {
				var pe *JudgeParseError
				if !errors.As(err, &pe) {
					t.Fatalf("error = %v, want *JudgeParseError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseJudge() error = %v", err)
			}
			if value != tt.wantValue {
				t.Errorf("value = %v, want %v", value, tt.wantValue)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}
