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

// TestAskUserAcceptsTUIChoiceOutputs is Guard B (Task 11): the end-to-end contract
// lock for the tools side of the AskUser answer contract (design §2/§4 finding).
// The TUI's choice-mode router (tui/interaction.go choiceKey) produces a uiAnswer
// whose Text is ALWAYS a listed choice or the literal "other" — never arbitrary
// typed text (locked on the producing side by tui/contract_test.go Guard A). This
// guard pins the CONSUMING side: AskUser.InvokableRun, driven through the real
// requestUserInput seam returning exactly those values, must yield a NON-error tool
// result. The negative row (an unlisted typed answer the TUI can never emit in
// choice mode) pins the contract BOUNDARY: it is the case that WOULD surface as a
// tool-result error, which is exactly why the TUI must never produce it.
//
// This runs in package tools so the unexported requestUserInput seam + validateAnswer
// path are reachable; the choice literal "other" mirrors tui.otherChoice / the
// otherChoice const here (they are asserted equal by value, not import).
func TestAskUserAcceptsTUIChoiceOutputs(t *testing.T) {
	t.Parallel()

	// The choices a with-choices AskUser was asked with; the TUI renders these and
	// emits exactly one of them, or the literal "other", on submit.
	choices := []string{"yes", "no", "maybe"}
	const argsJSON = `{"question":"Pick one","choices":["yes","no","maybe"]}`

	tests := []struct {
		name string
		// seamAnswer is what the TUI would hand back for a with-choices prompt: a
		// listed choice, the literal "other", or (negative row) text it never emits.
		seamAnswer  string
		wantErrText bool // true → the tool-result text begins with "error:"
	}{
		{name: "first listed choice accepted", seamAnswer: "yes"},
		{name: "middle listed choice accepted", seamAnswer: "no"},
		{name: "last listed choice accepted", seamAnswer: "maybe"},
		{name: "literal other escape hatch accepted", seamAnswer: otherChoice},
		// Boundary: an unlisted answer is exactly what choice mode can NEVER produce;
		// the tool rejects it, proving why the TUI invariant (Guard A) matters.
		{name: "unlisted typed answer rejected (boundary)", seamAnswer: "totally unlisted", wantErrText: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Sanity: every non-error row must feed an answer the TUI choice router
			// could actually emit (a listed choice or "other"); every error row must
			// feed one it could not. This keeps the guard honest about what it pins.
			isMember := tt.seamAnswer == otherChoice
			for _, c := range choices {
				if tt.seamAnswer == c {
					isMember = true
				}
			}
			if isMember == tt.wantErrText {
				t.Fatalf("test row inconsistent: seamAnswer %q isMember=%v but wantErrText=%v", tt.seamAnswer, isMember, tt.wantErrText)
			}

			a := NewAskUser()
			a.requestUserInput = func(_ context.Context, _ string, _ []string) (string, error) {
				return tt.seamAnswer, nil
			}
			res, err := a.InvokableRun(context.Background(), argsJSON)
			if err != nil {
				t.Fatalf("InvokableRun() returned a Go error %v; failures must be tool-result strings", err)
			}
			got := textOf(t, res)
			if gotErr := strings.HasPrefix(got, "error:"); gotErr != tt.wantErrText {
				t.Fatalf("result = %q, error=%v, want error=%v", got, gotErr, tt.wantErrText)
			}
			if !tt.wantErrText && got != tt.seamAnswer {
				t.Errorf("accepted result = %q, want the verbatim answer %q", got, tt.seamAnswer)
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
