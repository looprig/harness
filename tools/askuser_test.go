package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/tool"
)

// stubInputError is a typed error a stub requestUserInput seam can return to
// exercise AskUser's provider-error path (e.g. a cancelled turn / missing gate
// plumbing in the real loop.RequestUserInput).
type stubInputError struct{ msg string }

func (e *stubInputError) Error() string { return e.msg }

// TestAskUserInfo asserts the self-description: the name MUST be exactly
// "AskUser" (the classifyTool/manifest contract) and the schema/desc are present.
func TestAskUserInfo(t *testing.T) {
	t.Parallel()
	a := NewAskUser()
	info, err := a.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "AskUser" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "AskUser")
	}
	if info.Name != askUserToolName {
		t.Errorf("askUserToolName const = %q, want Info().Name %q", askUserToolName, info.Name)
	}
	if strings.TrimSpace(info.Desc) == "" {
		t.Error("Info().Desc is empty")
	}
	if len(info.Schema) == 0 {
		t.Error("Info().Schema is empty")
	}
}

// TestAskUserAuditSummary asserts the audit summary surfaces the question (which
// the user is shown anyway) and never panics on bad args.
func TestAskUserAuditSummary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		argsJSON string
		want     string
	}{
		{name: "question surfaced", argsJSON: `{"question":"Proceed?"}`, want: "AskUser: Proceed?"},
		{name: "with choices still just question", argsJSON: `{"question":"Pick","choices":["a","b"]}`, want: "AskUser: Pick"},
		{name: "empty question", argsJSON: `{"question":""}`, want: "AskUser (unparsable args)"},
		{name: "unparsable args", argsJSON: `not json`, want: "AskUser (unparsable args)"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := NewAskUser()
			if got := a.AuditSummary(tt.argsJSON); got != tt.want {
				t.Errorf("AuditSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAskUserNotPermissionPrompter asserts AskUser is AutoApprove: it must NOT
// implement PermissionPrompter (a Prompter would route it through the Ask gate).
// It MAY implement Auditable, which it does.
func TestAskUserNotPermissionPrompter(t *testing.T) {
	t.Parallel()
	var ti tool.InvokableTool = NewAskUser()
	if _, ok := ti.(tool.PermissionPrompter); ok {
		t.Error("AskUser must NOT implement PermissionPrompter (it is AutoApprove)")
	}
	if _, ok := ti.(tool.Auditable); !ok {
		t.Error("AskUser should implement Auditable")
	}
}

// TestAskUserInvokableRun drives InvokableRun through the requestUserInput SEAM
// (a documented test seam: the AskUser struct holds an indirect func field
// defaulting to loop.RequestUserInput in NewAskUser; tests override it to exercise
// the answer-validation logic without standing up a real loop gate). The real loop
// wiring is exercised by the gate tests (loop package) + integration later.
func TestAskUserInvokableRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		argsJSON string
		// seam behaviour
		seamAnswer string
		seamErr    error
		// expectations: substring the result text must contain, and whether the
		// result text is an "error:" string.
		wantContains string
		wantErrText  bool
	}{
		{
			name:         "valid answer in choices",
			argsJSON:     `{"question":"Pick one","choices":["yes","no"]}`,
			seamAnswer:   "yes",
			wantContains: "yes",
		},
		{
			name:         "other accepted as escape hatch",
			argsJSON:     `{"question":"Pick one","choices":["yes","no"]}`,
			seamAnswer:   "other",
			wantContains: "other",
		},
		{
			name:         "invalid answer not in choices is error",
			argsJSON:     `{"question":"Pick one","choices":["yes","no"]}`,
			seamAnswer:   "maybe",
			wantContains: "error:",
			wantErrText:  true,
		},
		{
			name:         "free-text accepted when no choices",
			argsJSON:     `{"question":"What is your name?"}`,
			seamAnswer:   "anything goes here",
			wantContains: "anything goes here",
		},
		{
			name:         "empty free-text accepted when no choices",
			argsJSON:     `{"question":"Optional?"}`,
			seamAnswer:   "",
			wantContains: "",
		},
		{
			name:         "answer must exactly match a choice (case sensitive)",
			argsJSON:     `{"question":"Pick one","choices":["Yes","No"]}`,
			seamAnswer:   "yes",
			wantContains: "error:",
			wantErrText:  true,
		},
		{
			name:         "seam provider error becomes tool-result error",
			argsJSON:     `{"question":"Pick one","choices":["yes","no"]}`,
			seamErr:      &stubInputError{msg: "context canceled"},
			wantContains: "error:",
			wantErrText:  true,
		},
		{
			name:         "missing question is error",
			argsJSON:     `{"choices":["yes","no"]}`,
			wantContains: "error:",
			wantErrText:  true,
		},
		{
			name:         "unparsable args is error",
			argsJSON:     `not json`,
			wantContains: "error:",
			wantErrText:  true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := NewAskUser()
			// Override the seam (option b): no real loop gate needed.
			a.requestUserInput = func(_ context.Context, _ string, _ []string) (string, error) {
				return tt.seamAnswer, tt.seamErr
			}
			res, err := a.InvokableRun(context.Background(), tt.argsJSON)
			if err != nil {
				t.Fatalf("InvokableRun() returned a Go error %v; failures must be tool-result strings", err)
			}
			got := textOf(t, res)
			if tt.wantErrText && !strings.HasPrefix(got, "error:") {
				t.Errorf("result = %q, want an error: string", got)
			}
			if !strings.Contains(got, tt.wantContains) {
				t.Errorf("result = %q, want to contain %q", got, tt.wantContains)
			}
			if !tt.wantErrText && tt.wantContains != "" && got != tt.seamAnswer {
				t.Errorf("result = %q, want the validated answer %q verbatim", got, tt.seamAnswer)
			}
		})
	}
}

// TestAskUserSeamDefaultsToLoop asserts NewAskUser wires the seam to the real
// loop.RequestUserInput by default — calling InvokableRun with NO loop ctx values
// yields a tool-result error (the real helper's *GateContextError path), proving
// the default is not a no-op stub.
func TestAskUserSeamDefaultsToLoop(t *testing.T) {
	t.Parallel()
	a := NewAskUser()
	res, err := a.InvokableRun(context.Background(), `{"question":"hi"}`)
	if err != nil {
		t.Fatalf("InvokableRun() Go error = %v", err)
	}
	got := textOf(t, res)
	if !strings.HasPrefix(got, "error:") {
		t.Errorf("default seam (loop.RequestUserInput) without ctx plumbing should error, got %q", got)
	}
}
