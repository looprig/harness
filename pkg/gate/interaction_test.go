package gate

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

// stubApprover records the combined prompts it is asked to resolve and answers
// each with a fixed action (or error).
type stubApprover struct {
	action  ApprovalAction
	err     error
	prompts []ApprovalPrompt
}

func (a *stubApprover) RequestApproval(_ context.Context, prompt ApprovalPrompt) (ApprovalAction, error) {
	a.prompts = append(a.prompts, prompt)
	if a.err != nil {
		return "", a.err
	}
	return a.action, nil
}

func gatedCommandBindings() []AccessBinding {
	return []AccessBinding{
		{Kind: tool.CapabilityCommandExecute, Source: &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}},
	}
}

func TestNewInteractiveEvaluatorRequiresApproverAndWriter(t *testing.T) {
	writer := &stubRuleWriter{}
	approver := &stubApprover{action: ApprovalApprove}

	var evalErr *EvaluationError
	if _, err := NewInteractiveEvaluator(gatedCommandBindings(), nil, nil, writer, &stubGrantIssuer{}); !errors.As(err, &evalErr) || evalErr.Kind != EvaluationApproverMissing {
		t.Fatalf("NewInteractiveEvaluator(nil approver) error = %v, want %s", err, EvaluationApproverMissing)
	}
	if _, err := NewInteractiveEvaluator(gatedCommandBindings(), nil, approver, nil, &stubGrantIssuer{}); !errors.As(err, &evalErr) || evalErr.Kind != EvaluationWriterMissing {
		t.Fatalf("NewInteractiveEvaluator(nil writer) error = %v, want %s", err, EvaluationWriterMissing)
	}

	evaluator, err := NewInteractiveEvaluator(gatedCommandBindings(), nil, approver, writer, &stubGrantIssuer{})
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator() error = %v", err)
	}
	if !evaluator.Interactive() {
		t.Error("Interactive() = false, want true for interactive construction")
	}
}

func TestNewHeadlessEvaluatorIsNotInteractive(t *testing.T) {
	evaluator, err := NewHeadlessEvaluator(gatedCommandBindings(), nil, &stubGrantIssuer{})
	if err != nil {
		t.Fatalf("NewHeadlessEvaluator() error = %v", err)
	}
	if evaluator.Interactive() {
		t.Error("Interactive() = true, want false for headless construction")
	}
}

func TestHeadlessAuthorizeUnmetIsTypedApprovalRequiredDenial(t *testing.T) {
	issuer := &stubGrantIssuer{}
	evaluator, err := NewHeadlessEvaluator(gatedCommandBindings(), &stubRuleMatcher{}, issuer)
	if err != nil {
		t.Fatalf("NewHeadlessEvaluator() error = %v", err)
	}

	_, err = evaluator.Authorize(context.Background(), validCommandRequest())
	var evalErr *EvaluationError
	if !errors.As(err, &evalErr) || evalErr.Kind != EvaluationApprovalRequired {
		t.Fatalf("Authorize() error = %v, want %s", err, EvaluationApprovalRequired)
	}
	if len(issuer.calls) != 0 {
		t.Errorf("issuer calls = %d, want 0 for an approval-required denial", len(issuer.calls))
	}
}

func TestHeadlessAuthorizeSavedAllowIssuesGrants(t *testing.T) {
	issuer := &stubGrantIssuer{tokens: []string{"tok-1"}}
	matcher := &stubRuleMatcher{allows: map[string]bool{tool.CapabilityCommandExecute: true}}
	evaluator, err := NewHeadlessEvaluator(gatedCommandBindings(), matcher, issuer)
	if err != nil {
		t.Fatalf("NewHeadlessEvaluator() error = %v", err)
	}

	resolution, err := evaluator.Authorize(context.Background(), validCommandRequest())
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if !resolution.Approved {
		t.Fatal("Authorize().Approved = false, want true when a saved allow satisfies the gate")
	}
	if len(resolution.Grants) != 1 || resolution.Grants[0] != "tok-1" {
		t.Fatalf("Authorize().Grants = %v, want [tok-1]", resolution.Grants)
	}
	if len(issuer.calls) != 1 || issuer.calls[0].target != "git status" || issuer.calls[0].class != tool.GrantClassCommandStart {
		t.Fatalf("issuer calls = %+v, want one exact-command grant", issuer.calls)
	}
}

func TestInteractiveAuthorizePromptsOnceAndResolvesApprove(t *testing.T) {
	writer := &stubRuleWriter{}
	approver := &stubApprover{action: ApprovalApprove}
	issuer := &stubGrantIssuer{}
	evaluator, err := NewInteractiveEvaluator(gatedCommandBindings(), &stubRuleMatcher{}, approver, writer, issuer)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator() error = %v", err)
	}

	resolution, err := evaluator.Authorize(context.Background(), validCommandRequest())
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if !resolution.Approved || len(resolution.Grants) != 1 {
		t.Fatalf("Authorize() = %+v, want approved with one grant", resolution)
	}
	if len(approver.prompts) != 1 {
		t.Fatalf("approver prompts = %d, want exactly one combined gate", len(approver.prompts))
	}
	prompt := approver.prompts[0]
	if len(prompt.Unmet) != 1 || prompt.Unmet[0].Kind != tool.CapabilityCommandExecute {
		t.Errorf("prompt.Unmet = %+v, want the gated command requirement", prompt.Unmet)
	}
	if len(prompt.Candidates) != 1 {
		t.Errorf("prompt.Candidates = %+v, want the displayed reusable candidate", prompt.Candidates)
	}
	if len(writer.writes) != 0 {
		t.Errorf("writer writes = %d, want 0 for a once approval", len(writer.writes))
	}
}

func TestInteractiveAuthorizeApproveAlwaysPersistsBeforeGrant(t *testing.T) {
	writer := &stubRuleWriter{}
	approver := &stubApprover{action: ApprovalApproveAlwaysWorkspace}
	issuer := &stubGrantIssuer{}
	evaluator, err := NewInteractiveEvaluator(gatedCommandBindings(), &stubRuleMatcher{}, approver, writer, issuer)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator() error = %v", err)
	}

	resolution, err := evaluator.Authorize(context.Background(), validCommandRequest())
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if !resolution.Approved {
		t.Fatal("Authorize().Approved = false, want true")
	}
	if len(writer.writes) != 1 || len(writer.writes[0]) != 1 || writer.writes[0][0].Match != "Bash(git status)" {
		t.Fatalf("writer.writes = %+v, want one atomic batch with the displayed candidate", writer.writes)
	}
	if len(issuer.calls) != 1 {
		t.Fatalf("issuer calls = %d, want 1", len(issuer.calls))
	}
}

func TestInteractiveAuthorizeDenyWritesAndMintsNothing(t *testing.T) {
	writer := &stubRuleWriter{}
	approver := &stubApprover{action: ApprovalDeny}
	issuer := &stubGrantIssuer{}
	evaluator, err := NewInteractiveEvaluator(gatedCommandBindings(), &stubRuleMatcher{}, approver, writer, issuer)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator() error = %v", err)
	}

	resolution, err := evaluator.Authorize(context.Background(), validCommandRequest())
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if resolution.Approved || len(resolution.Grants) != 0 {
		t.Fatalf("Authorize() = %+v, want unapproved with no grants", resolution)
	}
	if len(writer.writes) != 0 || len(issuer.calls) != 0 {
		t.Errorf("writes = %d, issuer calls = %d, want 0 and 0", len(writer.writes), len(issuer.calls))
	}
}

func TestAuthorizeDeniedAccessNeverPrompts(t *testing.T) {
	bindings := []AccessBinding{
		{Kind: tool.CapabilityCommandExecute, Source: &stubAccessSource{version: CurrentAccessVersion, access: AccessDeny}},
	}
	approver := &stubApprover{action: ApprovalApprove}
	evaluator, err := NewInteractiveEvaluator(bindings, &stubRuleMatcher{}, approver, &stubRuleWriter{}, &stubGrantIssuer{})
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator() error = %v", err)
	}

	resolution, err := evaluator.Authorize(context.Background(), validCommandRequest())
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if resolution.Approved {
		t.Fatal("Authorize().Approved = true, want false for configured deny")
	}
	if len(approver.prompts) != 0 {
		t.Errorf("approver prompts = %d, want 0: a deny rejects without prompting", len(approver.prompts))
	}
}

func TestAuthorizeEmptyRequestApprovedWithoutPrompt(t *testing.T) {
	approver := &stubApprover{action: ApprovalDeny}
	issuer := &stubGrantIssuer{}
	evaluator, err := NewInteractiveEvaluator(nil, nil, approver, &stubRuleWriter{}, issuer)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator() error = %v", err)
	}

	resolution, err := evaluator.Authorize(context.Background(), tool.Request{})
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if !resolution.Approved || len(resolution.Grants) != 0 {
		t.Fatalf("Authorize() = %+v, want approved with no grants", resolution)
	}
	if len(approver.prompts) != 0 || len(issuer.calls) != 0 {
		t.Errorf("prompts = %d, issuer calls = %d, want 0 and 0 for a pure empty request", len(approver.prompts), len(issuer.calls))
	}
}

func TestAuthorizeApproverFailureFailsClosed(t *testing.T) {
	approver := &stubApprover{err: errors.New("approver gone")}
	issuer := &stubGrantIssuer{}
	evaluator, err := NewInteractiveEvaluator(gatedCommandBindings(), &stubRuleMatcher{}, approver, &stubRuleWriter{}, issuer)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator() error = %v", err)
	}

	_, err = evaluator.Authorize(context.Background(), validCommandRequest())
	var evalErr *EvaluationError
	if !errors.As(err, &evalErr) || evalErr.Kind != EvaluationApprovalFailed {
		t.Fatalf("Authorize() error = %v, want %s", err, EvaluationApprovalFailed)
	}
	if len(issuer.calls) != 0 {
		t.Errorf("issuer calls = %d, want 0 after approver failure", len(issuer.calls))
	}
}

func TestResolveApproveAlwaysZeroCandidatesSkipsWrite(t *testing.T) {
	writer := &stubRuleWriter{err: errors.New("writer must not be called")}
	approver := &stubApprover{action: ApprovalApproveAlwaysWorkspace}
	request := validCommandRequest()
	request.Requirements[0].Candidates = nil
	evaluator, err := NewInteractiveEvaluator(gatedCommandBindings(), &stubRuleMatcher{}, approver, writer, &stubGrantIssuer{})
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator() error = %v", err)
	}

	resolution, err := evaluator.Authorize(context.Background(), request)
	if err != nil {
		t.Fatalf("Authorize() error = %v, want empty candidate batch to be a no-op", err)
	}
	if !resolution.Approved {
		t.Fatal("Authorize().Approved = false, want true")
	}
	if len(writer.writes) != 0 {
		t.Errorf("writer.writes = %d, want 0 for an empty candidate batch", len(writer.writes))
	}
}
