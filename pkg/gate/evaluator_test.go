package gate

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

type stubRuleMatcher struct {
	denies map[string]bool
	allows map[string]bool
	err    error
	calls  []string
}

type stubRuleWriter struct {
	writes [][]tool.RuleCandidate
	err    error
}

func (w *stubRuleWriter) WriteRules(_ context.Context, candidates []tool.RuleCandidate) error {
	w.writes = append(w.writes, cloneCandidates(candidates))
	return w.err
}

type grantCall struct {
	executionID, command, cwd, kind, scope, class, target string
	expiryUnixMilli                                       int64
}

type stubGrantIssuer struct {
	version uint16
	calls   []grantCall
	tokens  []string
	err     error
}

func (i *stubGrantIssuer) GrantVersion() uint16 {
	if i.version == 0 {
		return CurrentGrantVersion
	}
	return i.version
}

func (i *stubGrantIssuer) IssueGrant(_ context.Context, executionID, command, cwd, kind, scope, class, target string, expiryUnixMilli int64) (string, error) {
	i.calls = append(i.calls, grantCall{
		executionID: executionID, command: command, cwd: cwd, kind: kind,
		scope: scope, class: class, target: target, expiryUnixMilli: expiryUnixMilli,
	})
	if i.err != nil {
		return "", i.err
	}
	token := "grant-token"
	if len(i.tokens) > len(i.calls)-1 {
		token = i.tokens[len(i.calls)-1]
	}
	return token, nil
}

func (m *stubRuleMatcher) MatchesDeny(_ context.Context, requirement tool.Requirement) (bool, error) {
	m.calls = append(m.calls, "deny:"+requirement.Kind)
	return m.denies[requirement.Kind], m.err
}

func (m *stubRuleMatcher) MatchesAllow(_ context.Context, requirement tool.Requirement) (bool, error) {
	m.calls = append(m.calls, "allow:"+requirement.Kind)
	return m.allows[requirement.Kind], m.err
}

func validCommandRequest() tool.Request {
	return tool.Request{
		ToolName:           "Bash",
		Summary:            "run git status",
		ExecutionID:        "exec-1",
		Command:            "git status",
		WorkingDirectory:   "/workspace",
		ExpiresAtUnixMilli: 1_800_000_000_000,
		Requirements: []tool.Requirement{{
			Kind:        tool.CapabilityCommandExecute,
			Scope:       "",
			Match:       "git status",
			Description: "run command: git status",
			GrantClass:  tool.GrantClassCommandStart,
			GrantTarget: "git status",
			Candidates: []tool.RuleCandidate{{
				Kind:        tool.CapabilityCommandExecute,
				Match:       "Bash(git status)",
				Description: "Bash(git status)",
				GrantClass:  tool.GrantClassCommandStart,
				GrantTarget: "git status",
			}},
		}},
	}
}

func validCombinedRequest() tool.Request {
	request := validCommandRequest()
	request.Command = "git push"
	request.Summary = "run git push"
	request.Requirements[0].Match = "git push"
	request.Requirements[0].Description = "run command: git push"
	request.Requirements[0].GrantTarget = "git push"
	request.Requirements[0].Candidates = []tool.RuleCandidate{{
		Kind:        tool.CapabilityCommandExecute,
		Match:       "Bash(git push)",
		Description: "Bash(git push)",
		GrantClass:  tool.GrantClassCommandStart,
		GrantTarget: "git push",
	}}
	request.Requirements = append(request.Requirements, tool.Requirement{
		Kind:        "network",
		Scope:       "",
		Match:       "tcp:github.com:443",
		Description: "connect to github.com:443",
		GrantClass:  "network.proxy-target.v1",
		GrantTarget: "tcp:github.com:443",
		Candidates: []tool.RuleCandidate{{
			Kind:        "network",
			Match:       "Network(tcp:github.com:443)",
			Description: "Network(tcp:github.com:443)",
			GrantClass:  "network.proxy-target.v1",
			GrantTarget: "tcp:github.com:443",
		}},
	})
	return request
}

func TestGrantABIVersionPinned(t *testing.T) {
	if CurrentGrantVersion != 1 {
		t.Fatalf("CurrentGrantVersion = %d, want 1", CurrentGrantVersion)
	}
}

func TestNewEvaluatorRejectsUnsupportedGrantVersion(t *testing.T) {
	_, err := newEvaluatorForTest(nil, nil, nil, &stubGrantIssuer{version: 2})
	var evalErr *EvaluationError
	if !errors.As(err, &evalErr) || evalErr.Kind != EvaluationGrantVersionUnsupported {
		t.Fatalf("newEvaluatorForTest(v2 issuer) error = %v, want grant_version_unsupported", err)
	}
}

func TestDecodeRequestStrictValidation(t *testing.T) {
	valid := `{"tool_name":"Bash","summary":"run git status","execution_id":"exec-1","command":"git status","working_directory":"/workspace","expires_at_unix_milli":1800000000000,"requirements":[{"kind":"command.execute","scope":"","match":"git status","description":"run command: git status","grant_class":"command.start.v1","grant_target":"git status","candidates":[{"kind":"command.execute","match":"Bash(git status)","description":"Bash(git status)","grant_class":"command.start.v1","grant_target":"git status"}]}]}`
	request, err := DecodeRequest([]byte(valid))
	if err != nil {
		t.Fatalf("DecodeRequest(valid) error = %v", err)
	}
	if request.Command != "git status" || len(request.Requirements) != 1 {
		t.Fatalf("DecodeRequest(valid) = %#v", request)
	}

	tests := []struct {
		name string
		data string
	}{
		{name: "unknown field", data: strings.Replace(valid, `"tool_name":"Bash"`, `"tool_name":"Bash","unknown":true`, 1)},
		{name: "duplicate top-level field", data: strings.Replace(valid, `"tool_name":"Bash"`, `"tool_name":"Bash","tool_name":"Other"`, 1)},
		{name: "duplicate nested field", data: strings.Replace(valid, `"kind":"command.execute"`, `"kind":"command.execute","kind":"network"`, 1)},
		{name: "trailing json", data: valid + `{}`},
		{name: "null", data: `null`},
		{name: "malformed grant pair", data: strings.Replace(valid, `,"grant_target":"git status"`, `,"grant_target":""`, 1)},
		{name: "non-normalized match", data: strings.Replace(valid, `"match":"git status"`, `"match":" git status"`, 1)},
		{name: "excessive nesting", data: strings.Repeat("[", maxScanJSONDepth+2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeRequest([]byte(tt.data))
			var decodeErr *RequestDecodeError
			if !errors.As(err, &decodeErr) {
				t.Fatalf("DecodeRequest() error = %T %v, want *RequestDecodeError", err, err)
			}
		})
	}
}

func TestEvaluatorAppliesAccessDenyBeforeStoredRules(t *testing.T) {
	command := &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}
	network := &stubAccessSource{version: CurrentAccessVersion, access: AccessDeny}
	matcher := &stubRuleMatcher{allows: map[string]bool{"command.execute": true, "network": true}}
	evaluator, err := newEvaluatorForTest([]AccessBinding{
		{Kind: "command.execute", Source: command},
		{Kind: "network", Source: network},
	}, matcher, nil, nil)
	if err != nil {
		t.Fatalf("newEvaluatorForTest() error = %v", err)
	}

	result, err := evaluator.Evaluate(context.Background(), validCombinedRequest())
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(result.Denied) != 1 || result.Denied[0].Kind != "network" {
		t.Fatalf("Denied = %#v, want network", result.Denied)
	}
	if len(matcher.calls) != 0 {
		t.Fatalf("matcher calls = %#v, want none after access deny", matcher.calls)
	}
}

func TestEvaluatorFailsClosedOnAccessRoutingError(t *testing.T) {
	evaluator, err := newEvaluatorForTest([]AccessBinding{
		{Kind: "command.execute", Source: &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}},
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("newEvaluatorForTest() error = %v", err)
	}

	// The combined request contains a network requirement with no bound source.
	_, err = evaluator.Evaluate(context.Background(), validCombinedRequest())
	var accessErr *AccessError
	if !errors.As(err, &accessErr) || accessErr.Kind != AccessSourceMissing {
		t.Fatalf("Evaluate() error = %v, want source_missing AccessError", err)
	}
}

func TestEvaluatorChecksEveryStoredDenyBeforeAnyAllow(t *testing.T) {
	command := &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}
	network := &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}
	matcher := &stubRuleMatcher{
		denies: map[string]bool{"network": true},
		allows: map[string]bool{"command.execute": true, "network": true},
	}
	evaluator, err := newEvaluatorForTest([]AccessBinding{
		{Kind: "command.execute", Source: command},
		{Kind: "network", Source: network},
	}, matcher, nil, nil)
	if err != nil {
		t.Fatalf("newEvaluatorForTest() error = %v", err)
	}

	result, err := evaluator.Evaluate(context.Background(), validCombinedRequest())
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(result.Denied) != 1 || result.Denied[0].Kind != "network" {
		t.Fatalf("Denied = %#v, want network", result.Denied)
	}
	wantCalls := []string{"deny:command.execute", "deny:network"}
	if !reflect.DeepEqual(matcher.calls, wantCalls) {
		t.Fatalf("matcher calls = %#v, want %#v", matcher.calls, wantCalls)
	}
}

func TestEvaluatorReturnsOneCombinedUnmetSet(t *testing.T) {
	command := &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}
	network := &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}
	matcher := &stubRuleMatcher{}
	evaluator, err := newEvaluatorForTest([]AccessBinding{
		{Kind: "command.execute", Source: command},
		{Kind: "network", Source: network},
	}, matcher, nil, nil)
	if err != nil {
		t.Fatalf("newEvaluatorForTest() error = %v", err)
	}

	result, err := evaluator.Evaluate(context.Background(), validCombinedRequest())
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(result.Unmet) != 2 || result.Unmet[0].Kind != "command.execute" || result.Unmet[1].Kind != "network" {
		t.Fatalf("Unmet = %#v, want command and network combined", result.Unmet)
	}
	if len(result.Candidates) != 2 {
		t.Fatalf("Candidates = %#v, want both displayed reusable candidates", result.Candidates)
	}
	wantCalls := []string{"deny:command.execute", "deny:network", "allow:command.execute", "allow:network"}
	if !reflect.DeepEqual(matcher.calls, wantCalls) {
		t.Fatalf("matcher calls = %#v, want %#v", matcher.calls, wantCalls)
	}
}

func TestEvaluatorPartialSavedAllowLeavesOnlyUnmetCapability(t *testing.T) {
	command := &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}
	network := &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}
	matcher := &stubRuleMatcher{allows: map[string]bool{"command.execute": true}}
	evaluator, err := newEvaluatorForTest([]AccessBinding{
		{Kind: "command.execute", Source: command},
		{Kind: "network", Source: network},
	}, matcher, nil, nil)
	if err != nil {
		t.Fatalf("newEvaluatorForTest() error = %v", err)
	}

	result, err := evaluator.Evaluate(context.Background(), validCombinedRequest())
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(result.Unmet) != 1 || result.Unmet[0].Kind != "network" {
		t.Fatalf("Unmet = %#v, want only network", result.Unmet)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].Kind != "network" {
		t.Fatalf("Candidates = %#v, want only the network candidate", result.Candidates)
	}
}

func TestResolveApproveWritesNothingAndMintsExactGrants(t *testing.T) {
	command := &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}
	network := &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}
	writer := &stubRuleWriter{}
	issuer := &stubGrantIssuer{tokens: []string{"secret-command-token", "secret-network-token"}}
	evaluator, err := newEvaluatorForTest([]AccessBinding{
		{Kind: "command.execute", Source: command},
		{Kind: "network", Source: network},
	}, &stubRuleMatcher{}, writer, issuer)
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := evaluator.Evaluate(context.Background(), validCombinedRequest())
	if err != nil {
		t.Fatal(err)
	}

	resolution, err := evaluator.Resolve(context.Background(), evaluation, ApprovalApprove)
	if err != nil {
		t.Fatalf("Resolve(Approve) error = %v", err)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("writer writes = %#v, want none", writer.writes)
	}
	if !reflect.DeepEqual(resolution.Grants, []string{"secret-command-token", "secret-network-token"}) {
		t.Fatalf("grants = %#v", resolution.Grants)
	}
	if len(issuer.calls) != 2 {
		t.Fatalf("issuer calls = %#v, want two", issuer.calls)
	}
	commandGrant := issuer.calls[0]
	if commandGrant.class != "command.start.v1" || commandGrant.target != "git push" || commandGrant.command != "git push" {
		t.Fatalf("command grant = %#v, want exact command.start.v1/git push", commandGrant)
	}
}

func TestResolveSavedFamilyAllowStillMintsExactCommandGrant(t *testing.T) {
	// A saved family/wildcard rule satisfies the DECISION for command.execute,
	// but issuance stays exact-command, single-spawn: the grant carries the
	// exact normalized command target, not the matched rule.
	matcher := &stubRuleMatcher{allows: map[string]bool{"command.execute": true, "network": true}}
	issuer := &stubGrantIssuer{}
	evaluator, err := newEvaluatorForTest([]AccessBinding{
		{Kind: "command.execute", Source: &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}},
		{Kind: "network", Source: &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}},
	}, matcher, nil, issuer)
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := evaluator.Evaluate(context.Background(), validCombinedRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Unmet) != 0 {
		t.Fatalf("Unmet = %#v, want none when saved rules match", evaluation.Unmet)
	}

	resolution, err := evaluator.Resolve(context.Background(), evaluation, ApprovalApprove)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !resolution.Approved || len(resolution.Grants) != 2 {
		t.Fatalf("resolution = %#v, want approved with both grants", resolution)
	}
	if issuer.calls[0].class != "command.start.v1" || issuer.calls[0].target != "git push" {
		t.Fatalf("command grant = %#v, want exact command target", issuer.calls[0])
	}
}

func TestResolveApproveAlwaysPersistsEveryCandidateBeforeGrantMinting(t *testing.T) {
	order := make([]string, 0, 2)
	writer := &orderedRuleWriter{order: &order}
	issuer := &orderedGrantIssuer{order: &order}
	evaluator := newCombinedEvaluator(t, writer, issuer)
	evaluation, err := evaluator.Evaluate(context.Background(), validCombinedRequest())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := evaluator.Resolve(context.Background(), evaluation, ApprovalApproveAlwaysWorkspace); err != nil {
		t.Fatalf("Resolve(ApproveAlways) error = %v", err)
	}
	if !reflect.DeepEqual(order, []string{"write:2", "grant", "grant"}) {
		t.Fatalf("operation order = %#v, want atomic batch write before grants", order)
	}
}

func TestResolveApproveAlwaysPersistenceFailureMintsNoGrants(t *testing.T) {
	persistErr := errors.New("persistence failed")
	writer := &stubRuleWriter{err: persistErr}
	issuer := &stubGrantIssuer{}
	evaluator := newCombinedEvaluator(t, writer, issuer)
	evaluation, err := evaluator.Evaluate(context.Background(), validCombinedRequest())
	if err != nil {
		t.Fatal(err)
	}

	_, err = evaluator.Resolve(context.Background(), evaluation, ApprovalApproveAlwaysWorkspace)
	if !errors.Is(err, persistErr) {
		t.Fatalf("Resolve() error = %v, want persistence failure", err)
	}
	if len(writer.writes) != 1 || len(writer.writes[0]) != 2 {
		t.Fatalf("writer writes = %#v, want one atomic two-candidate batch", writer.writes)
	}
	if len(issuer.calls) != 0 {
		t.Fatalf("issuer calls = %#v, want none", issuer.calls)
	}
}

func TestResolveFailsClosedWithoutConfiguredWriterOrIssuer(t *testing.T) {
	evaluator, err := newEvaluatorForTest([]AccessBinding{
		{Kind: "command.execute", Source: &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}},
		{Kind: "network", Source: &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}},
	}, &stubRuleMatcher{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := evaluator.Evaluate(context.Background(), validCombinedRequest())
	if err != nil {
		t.Fatal(err)
	}

	var evalErr *EvaluationError
	_, err = evaluator.Resolve(context.Background(), evaluation, ApprovalApproveAlwaysWorkspace)
	if !errors.As(err, &evalErr) || evalErr.Kind != EvaluationWriterMissing {
		t.Fatalf("Resolve(ApproveAlways) error = %v, want writer_missing", err)
	}
	_, err = evaluator.Resolve(context.Background(), evaluation, ApprovalApprove)
	if !errors.As(err, &evalErr) || evalErr.Kind != EvaluationIssuerMissing {
		t.Fatalf("Resolve(Approve) error = %v, want issuer_missing", err)
	}
	_, err = evaluator.Resolve(context.Background(), evaluation, ApprovalAction("Approve always"))
	if !errors.As(err, &evalErr) || evalErr.Kind != EvaluationActionInvalid {
		t.Fatalf("Resolve(non-exact action) error = %v, want action_invalid", err)
	}
}

func TestResolveDeniedEvaluationRejectsApprovalAndMintsNothing(t *testing.T) {
	issuer := &stubGrantIssuer{}
	evaluator, err := newEvaluatorForTest([]AccessBinding{
		{Kind: "command.execute", Source: &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}},
		{Kind: "network", Source: &stubAccessSource{version: CurrentAccessVersion, access: AccessDeny}},
	}, &stubRuleMatcher{}, &stubRuleWriter{}, issuer)
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := evaluator.Evaluate(context.Background(), validCombinedRequest())
	if err != nil {
		t.Fatal(err)
	}

	var evalErr *EvaluationError
	if _, err := evaluator.Resolve(context.Background(), evaluation, ApprovalApprove); !errors.As(err, &evalErr) || evalErr.Kind != EvaluationDenied {
		t.Fatalf("Resolve(Approve) on denied evaluation error = %v, want denied", err)
	}
	resolution, err := evaluator.Resolve(context.Background(), evaluation, ApprovalDeny)
	if err != nil || resolution.Approved {
		t.Fatalf("Resolve(Deny) = %#v, %v, want unapproved without error", resolution, err)
	}
	if len(issuer.calls) != 0 {
		t.Fatalf("issuer calls = %#v, want none", issuer.calls)
	}
}

type orderedRuleWriter struct{ order *[]string }

func (w *orderedRuleWriter) WriteRules(_ context.Context, candidates []tool.RuleCandidate) error {
	*w.order = append(*w.order, "write:"+string(rune('0'+len(candidates))))
	return nil
}

type orderedGrantIssuer struct{ order *[]string }

func (i *orderedGrantIssuer) GrantVersion() uint16 { return CurrentGrantVersion }

func (i *orderedGrantIssuer) IssueGrant(context.Context, string, string, string, string, string, string, string, int64) (string, error) {
	*i.order = append(*i.order, "grant")
	return "secret-token", nil
}

func newCombinedEvaluator(t *testing.T, writer RuleWriter, issuer GrantIssuer) *Evaluator {
	t.Helper()
	evaluator, err := newEvaluatorForTest([]AccessBinding{
		{Kind: "command.execute", Source: &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}},
		{Kind: "network", Source: &stubAccessSource{version: CurrentAccessVersion, access: AccessGated}},
	}, &stubRuleMatcher{}, writer, issuer)
	if err != nil {
		t.Fatal(err)
	}
	return evaluator
}

// newEvaluatorForTest preserves the pre-interaction constructor shape for the
// evaluation tests: a nil writer builds a headless evaluator, a non-nil writer
// an interactive one with a stub approver (the interaction seam itself is
// covered in interaction_test.go).
func newEvaluatorForTest(bindings []AccessBinding, matcher RuleMatcher, writer RuleWriter, issuer GrantIssuer) (*Evaluator, error) {
	if writer == nil {
		return NewHeadlessEvaluator(bindings, matcher, issuer)
	}
	return NewInteractiveEvaluator(bindings, matcher, &stubApprover{action: ApprovalApprove}, writer, issuer)
}
