package hustleruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const evidenceReadKind = "filesystem.read"

type evidenceAccessStub struct {
	access uint8
	err    error
	panic  bool
	calls  int
}

func (s *evidenceAccessStub) AccessFor(requirement tool.Requirement) (uint8, error) {
	s.calls++
	if s.panic {
		panic("access-secret")
	}
	return s.access, s.err
}

type preparedEvidenceTool struct {
	info       tool.ToolInfo
	request    func(uuid.UUID) tool.Request
	prepareErr error
	runErr     error
	result     *tool.ToolResult
	panicPrep  bool
	panicRun   bool

	mu          sync.Mutex
	prepares    int
	runs        int
	args        []string
	executionID []uuid.UUID
	seen        []tool.PreparedCall
}

func (t *preparedEvidenceTool) Info(context.Context) (*tool.ToolInfo, error) {
	info := t.info.Clone()
	return &info, nil
}

func (t *preparedEvidenceTool) PrepareCall(_ context.Context, executionID uuid.UUID, args string) (tool.Request, tool.PreparedArtifact, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prepares++
	t.args = append(t.args, args)
	t.executionID = append(t.executionID, executionID)
	if t.panicPrep {
		panic("prepare-secret")
	}
	if t.prepareErr != nil {
		return tool.Request{}, nil, t.prepareErr
	}
	request := tool.Request{
		ToolName:    t.info.Name,
		ExecutionID: executionID.String(),
		Requirements: []tool.Requirement{{
			Kind: evidenceReadKind, Match: "/workspace/file", Description: "read evidence",
		}},
	}
	if t.request != nil {
		request = t.request(executionID)
	}
	return request, tool.TokenArtifact{Token: "prepared-token"}, nil
}

func (t *preparedEvidenceTool) InvokableRun(ctx context.Context, args string) (*tool.ToolResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.runs++
	t.args = append(t.args, args)
	if prepared, ok := loop.PreparedCallFromContext(ctx); ok {
		t.seen = append(t.seen, prepared)
	}
	if t.panicRun {
		panic("run-secret")
	}
	return t.result, t.runErr
}

func (t *preparedEvidenceTool) counts() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.prepares, t.runs
}

type unpreparedEvidenceTool struct{ info tool.ToolInfo }

func (t *unpreparedEvidenceTool) Info(context.Context) (*tool.ToolInfo, error) {
	info := t.info.Clone()
	return &info, nil
}
func (*unpreparedEvidenceTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	panic("must not execute")
}

type typedNilPreparerEvidenceTool struct{ info tool.ToolInfo }

func (t *typedNilPreparerEvidenceTool) Info(context.Context) (*tool.ToolInfo, error) {
	info := t.info.Clone()
	return &info, nil
}
func (*typedNilPreparerEvidenceTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	panic("must not execute")
}
func (*typedNilPreparerEvidenceTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	panic("must not prepare")
}

func TestBindEvidenceInvocationUsesExplicitRequestOrigin(t *testing.T) {
	t.Parallel()

	sessionID := mustRuntimeTestID(t)
	rootLoopID := mustRuntimeTestID(t)
	changedActiveLoopID := mustRuntimeTestID(t)
	childLoopID := mustRuntimeTestID(t)
	var got []tool.EvidenceFactoryBindings
	definition := evidenceRuntimeDefinition(t, func(_ context.Context, bindings tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		got = append(got, bindings)
		return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
	})
	root := &tool.ReadWorkspaceBinding{Root: "/workspace"}

	for _, loopID := range []uuid.UUID{rootLoopID, changedActiveLoopID, childLoopID} {
		request := hustle.Request{Cause: identity.Cause{Coordinates: identity.Coordinates{
			SessionID: sessionID,
			LoopID:    loopID,
		}}}
		catalog, err := bindEvidenceInvocation(context.Background(), definition, request, root)
		if err != nil || len(catalog) != 1 {
			t.Fatalf("bindEvidenceInvocation() = (%d,%v), want one tool", len(catalog), err)
		}
	}
	if len(got) != 3 {
		t.Fatalf("factory calls = %d, want 3", len(got))
	}
	for i, wantLoopID := range []uuid.UUID{rootLoopID, changedActiveLoopID, childLoopID} {
		if got[i].SessionID != sessionID || got[i].LoopID != wantLoopID {
			t.Fatalf("factory origin[%d] = (%s,%s), want (%s,%s)", i, got[i].SessionID, got[i].LoopID, sessionID, wantLoopID)
		}
		if got[i].ReadWorkspace == root || got[i].ReadWorkspace == nil || got[i].ReadWorkspace.Root != root.Root {
			t.Fatalf("factory read workspace[%d] = %#v, want defensive root-only binding", i, got[i].ReadWorkspace)
		}
	}
}

func TestBindEvidenceInvocationRejectsMissingExplicitOrigin(t *testing.T) {
	t.Parallel()
	definition := evidenceRuntimeDefinition(t, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		t.Fatal("factory called")
		return nil, nil
	})
	valid := identity.Coordinates{SessionID: mustRuntimeTestID(t), LoopID: mustRuntimeTestID(t)}
	for _, coordinates := range []identity.Coordinates{
		{LoopID: valid.LoopID},
		{SessionID: valid.SessionID},
	} {
		_, err := bindEvidenceInvocation(context.Background(), definition, hustle.Request{
			Cause: identity.Cause{Coordinates: coordinates},
		}, &tool.ReadWorkspaceBinding{Root: "/workspace"})
		assertEvidenceFailure(t, err, EvidenceFailureInvalidBinding)
	}
}

func TestEvidenceRunnerPreparesOnceExecutesSequentiallyAndPreservesPreparedArtifact(t *testing.T) {
	t.Parallel()
	order := make([]string, 0, 2)
	first := newPreparedEvidenceTool("workspace_first", "first")
	second := newPreparedEvidenceTool("workspace_second", "second")
	first.result = tool.TextResult("first")
	second.result = tool.TextResult("second")
	firstRun := first.InvokableRun
	first.runErr = nil
	secondRun := second.InvokableRun
	firstWrapper := &orderingEvidenceTool{preparedEvidenceTool: first, order: &order, name: "first", run: firstRun}
	secondWrapper := &orderingEvidenceTool{preparedEvidenceTool: second, order: &order, name: "second", run: secondRun}
	catalog := []hustle.BoundEvidenceTool{
		boundEvidenceRuntimeTool(t, firstWrapper),
		boundEvidenceRuntimeTool(t, secondWrapper),
	}
	runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, []uuid.UUID{
		mustRuntimeTestID(t), mustRuntimeTestID(t),
	})
	calls := []evidenceToolCall{
		{id: "provider-1", name: "workspace_first", input: json.RawMessage(`{"path":"a"}`)},
		{id: "provider-2", name: "workspace_second", input: json.RawMessage(`{"path":"b"}`)},
	}

	results, err := runner.run(context.Background(), catalog, calls, hustle.ToolLoopLimits{
		MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("execution order = %v, want provider order", order)
	}
	if len(results) != 2 || results[0].callID != "provider-1" || results[0].name != "workspace_first" ||
		results[1].callID != "provider-2" || results[1].name != "workspace_second" {
		t.Fatalf("results = %#v, want exact call ID/name pairs", results)
	}
	for _, candidate := range []*preparedEvidenceTool{first, second} {
		prepares, runs := candidate.counts()
		if prepares != 1 || runs != 1 {
			t.Fatalf("%s counts = prepare:%d run:%d, want 1,1", candidate.info.Name, prepares, runs)
		}
		if len(candidate.seen) != 1 {
			t.Fatalf("%s prepared contexts = %d, want 1", candidate.info.Name, len(candidate.seen))
		}
		prepared := candidate.seen[0]
		if prepared.ExecutionID != candidate.executionID[0] ||
			prepared.Request.ExecutionID != candidate.executionID[0].String() ||
			prepared.Request.ToolName != candidate.info.Name ||
			prepared.Artifact != (tool.TokenArtifact{Token: "prepared-token"}) ||
			prepared.Grants != nil {
			t.Fatalf("%s prepared call = %#v, want original grant-free artifact", candidate.info.Name, prepared)
		}
	}
	first.result.Content[0].(*content.TextBlock).Text = "mutated"
	if got := results[0].content[0].(*content.TextBlock).Text; got != "first" {
		t.Fatalf("owned result = %q after tool mutation, want first", got)
	}
	results[0].content[0].(*content.TextBlock).Text = "caller-mutated"
	if got := first.seen[0].Request.Requirements[0].Match; got != "/workspace/file" {
		t.Fatalf("prepared request mutated through result: %q", got)
	}
}

type orderingEvidenceTool struct {
	*preparedEvidenceTool
	order *[]string
	name  string
	run   func(context.Context, string) (*tool.ToolResult, error)
}

func (t *orderingEvidenceTool) InvokableRun(ctx context.Context, args string) (*tool.ToolResult, error) {
	*t.order = append(*t.order, t.name)
	return t.run(ctx, args)
}

func TestEvidenceRunnerRejectsUnpreparedUnknownAndAmbiguousCalls(t *testing.T) {
	t.Parallel()
	valid := newPreparedEvidenceTool("workspace_read", "ok")
	tests := []struct {
		name    string
		catalog []hustle.BoundEvidenceTool
		calls   []evidenceToolCall
		ids     []uuid.UUID
		want    EvidenceFailureReason
	}{
		{
			name: "missing preparer",
			catalog: []hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, &unpreparedEvidenceTool{
				info: evidenceToolInfo("workspace_read"),
			})},
			calls: []evidenceToolCall{{id: "call", name: "workspace_read", input: json.RawMessage(`{}`)}},
			ids:   []uuid.UUID{mustRuntimeTestID(t)}, want: EvidenceFailureUnprepared,
		},
		{
			name:    "unknown tool",
			catalog: []hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, valid)},
			calls:   []evidenceToolCall{{id: "call", name: "workspace_unknown", input: json.RawMessage(`{}`)}},
			ids:     []uuid.UUID{mustRuntimeTestID(t)}, want: EvidenceFailureUnknownTool,
		},
		{
			name:    "duplicate provider id",
			catalog: []hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, valid)},
			calls: []evidenceToolCall{
				{id: "same", name: "workspace_read", input: json.RawMessage(`{}`)},
				{id: "same", name: "workspace_read", input: json.RawMessage(`{}`)},
			},
			ids: []uuid.UUID{mustRuntimeTestID(t), mustRuntimeTestID(t)}, want: EvidenceFailureAmbiguousIdentity,
		},
		{
			name:    "zero minted id",
			catalog: []hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, valid)},
			calls:   []evidenceToolCall{{id: "call", name: "workspace_read", input: json.RawMessage(`{}`)}},
			ids:     []uuid.UUID{{}}, want: EvidenceFailureAmbiguousIdentity,
		},
		{
			name:    "duplicate minted id",
			catalog: []hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, valid)},
			calls: []evidenceToolCall{
				{id: "first", name: "workspace_read", input: json.RawMessage(`{}`)},
				{id: "second", name: "workspace_read", input: json.RawMessage(`{}`)},
			},
			ids: func() []uuid.UUID {
				id := mustRuntimeTestID(t)
				return []uuid.UUID{id, id}
			}(),
			want: EvidenceFailureAmbiguousIdentity,
		},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, testCase.ids)
			_, err := runner.run(context.Background(), testCase.catalog, testCase.calls, hustle.ToolLoopLimits{
				MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
			})
			assertEvidenceFailure(t, err, testCase.want)
		})
	}
}

func TestBindEvidenceInvocationRejectsTypedNilPreparerTool(t *testing.T) {
	t.Parallel()
	typedNil := (*typedNilPreparerEvidenceTool)(nil)
	definition := evidenceRuntimeDefinition(t, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{typedNil}, nil
	})
	request := hustle.Request{Cause: identity.Cause{Coordinates: identity.Coordinates{
		SessionID: mustRuntimeTestID(t), LoopID: mustRuntimeTestID(t),
	}}}
	_, err := bindEvidenceInvocation(context.Background(), definition, request, &tool.ReadWorkspaceBinding{Root: "/workspace"})
	assertEvidenceFailure(t, err, EvidenceFailureInvalidBinding)
}

func TestEvidenceRunnerRejectsDuplicateMintedIdentityBeforePreparation(t *testing.T) {
	t.Parallel()
	candidate := newPreparedEvidenceTool("workspace_read", "ok")
	id := mustRuntimeTestID(t)
	runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, []uuid.UUID{id, id})
	_, err := runner.run(context.Background(),
		[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, candidate)},
		[]evidenceToolCall{
			{id: "first", name: "workspace_read", input: json.RawMessage(`{}`)},
			{id: "second", name: "workspace_read", input: json.RawMessage(`{}`)},
		},
		hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
	)
	assertEvidenceFailure(t, err, EvidenceFailureAmbiguousIdentity)
	if prepares, runs := candidate.counts(); prepares != 0 || runs != 0 {
		t.Fatalf("duplicate identity performed work: prepare=%d run=%d", prepares, runs)
	}
}

func TestEvidenceRunnerRequiresExactPreparedExecutionIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request func(uuid.UUID) tool.Request
	}{
		{name: "missing execution id", request: func(id uuid.UUID) tool.Request {
			return validEvidenceRequest("workspace_read", id, evidenceReadKind, "")
		}},
		{name: "mismatched execution id", request: func(id uuid.UUID) tool.Request {
			return validEvidenceRequest("workspace_read", id, evidenceReadKind, mustRuntimeTestID(t).String())
		}},
		{name: "mismatched tool name", request: func(id uuid.UUID) tool.Request {
			request := validEvidenceRequest("workspace_other", id, evidenceReadKind, id.String())
			return request
		}},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			candidate := newPreparedEvidenceTool("workspace_read", "ok")
			candidate.request = testCase.request
			runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, []uuid.UUID{mustRuntimeTestID(t)})
			_, err := runner.run(context.Background(),
				[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, candidate)},
				[]evidenceToolCall{{id: "call", name: "workspace_read", input: json.RawMessage(`{}`)}},
				hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
			)
			assertEvidenceFailure(t, err, EvidenceFailureAmbiguousIdentity)
			if _, runs := candidate.counts(); runs != 0 {
				t.Fatal("identity mismatch executed")
			}
		})
	}
}

func TestEvidenceRunnerFailsClosedForAccessAndForbiddenCapabilities(t *testing.T) {
	t.Parallel()
	secret := "never-echo-access-secret"
	tests := []struct {
		name        string
		kind        string
		access      uint8
		accessErr   error
		panicAccess bool
		mutate      func(*tool.Request)
		want        EvidenceFailureReason
	}{
		{name: "allowed read", kind: evidenceReadKind, access: gate.AccessAllow},
		{name: "gated", kind: evidenceReadKind, access: gate.AccessGated, want: EvidenceFailureAccessRefused},
		{name: "denied", kind: evidenceReadKind, access: gate.AccessDeny, want: EvidenceFailureAccessRefused},
		{name: "unknown state", kind: evidenceReadKind, access: 99, want: EvidenceFailureAccessRefused},
		{name: "source error", kind: evidenceReadKind, access: gate.AccessAllow, accessErr: errors.New(secret), want: EvidenceFailureAccessRefused},
		{name: "source panic", kind: evidenceReadKind, access: gate.AccessAllow, panicAccess: true, want: EvidenceFailureInternal},
		{name: "invalid prepared request", kind: evidenceReadKind, access: gate.AccessAllow, mutate: func(request *tool.Request) {
			request.Requirements[0].Match = secret + "\x00"
		}, want: EvidenceFailureInvalidRequest},
		{name: "mutation kind", kind: tool.CapabilityCommandExecute, access: gate.AccessAllow, want: EvidenceFailureForbiddenCapability},
		{name: "unknown kind", kind: "future.read", access: gate.AccessAllow, want: EvidenceFailureForbiddenCapability},
		{name: "grant class", kind: evidenceReadKind, access: gate.AccessAllow, mutate: func(request *tool.Request) {
			request.Requirements[0].GrantClass = "filesystem.read.v1"
			request.Requirements[0].GrantTarget = "/workspace/file"
		}, want: EvidenceFailureForbiddenCapability},
		{name: "reusable candidate", kind: evidenceReadKind, access: gate.AccessAllow, mutate: func(request *tool.Request) {
			request.Requirements[0].Candidates = []tool.RuleCandidate{{
				Kind: evidenceReadKind, Match: "/workspace/*", Description: "workspace reads",
			}}
		}, want: EvidenceFailureForbiddenCapability},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			candidate := newPreparedEvidenceTool("workspace_read", "ok")
			candidate.request = func(id uuid.UUID) tool.Request {
				request := validEvidenceRequest("workspace_read", id, testCase.kind, id.String())
				if testCase.mutate != nil {
					testCase.mutate(&request)
				}
				return request
			}
			access := &evidenceAccessStub{access: testCase.access, err: testCase.accessErr, panic: testCase.panicAccess}
			runner := newTestEvidenceRunner(t, access, []uuid.UUID{mustRuntimeTestID(t)})
			_, err := runner.run(context.Background(),
				[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, candidate)},
				[]evidenceToolCall{{id: "call", name: "workspace_read", input: json.RawMessage(`{}`)}},
				hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
			)
			if testCase.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				assertEvidenceFailure(t, err, testCase.want)
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error %q leaked source content", err)
				}
			}
			if testCase.want != "" {
				if _, runs := candidate.counts(); runs != 0 {
					t.Fatal("refused request executed")
				}
			}
		})
	}
}

func TestEvidenceRunnerEnforcesPerResultAndAggregateEncodedByteBounds(t *testing.T) {
	t.Parallel()
	result := tool.TextResult("bounded evidence")
	encoded, err := content.MarshalBlocks(result.Content)
	if err != nil {
		t.Fatal(err)
	}
	run := func(maxResult, maxEvidence int, calls int) error {
		candidate := newPreparedEvidenceTool("workspace_read", "ignored")
		candidate.result = result
		ids := make([]uuid.UUID, calls)
		evidenceCalls := make([]evidenceToolCall, calls)
		for i := range calls {
			ids[i] = mustRuntimeTestID(t)
			evidenceCalls[i] = evidenceToolCall{id: string(rune('a' + i)), name: "workspace_read", input: json.RawMessage(`{}`)}
		}
		runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, ids)
		_, runErr := runner.run(context.Background(),
			[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, candidate)},
			evidenceCalls,
			hustle.ToolLoopLimits{MaxResultBytes: maxResult, MaxEvidenceBytes: maxEvidence},
		)
		return runErr
	}
	if err := run(len(encoded), len(encoded)*2, 1); err != nil {
		t.Fatalf("exact per-result bound: %v", err)
	}
	assertEvidenceFailure(t, run(len(encoded)-1, len(encoded)*2, 1), EvidenceFailureResultTooLarge)
	if err := run(len(encoded), len(encoded)*2, 2); err != nil {
		t.Fatalf("exact aggregate bound: %v", err)
	}
	assertEvidenceFailure(t, run(len(encoded), len(encoded)*2-1, 2), EvidenceFailureEvidenceTooLarge)
}

func TestEvidenceRunnerCancellationAndPanicsAreRedacted(t *testing.T) {
	t.Parallel()
	const secret = "never-echo-panic-or-error"
	tests := []struct {
		name   string
		setup  func(*preparedEvidenceTool)
		ctx    func() context.Context
		reason EvidenceFailureReason
	}{
		{name: "prepare error", setup: func(tool *preparedEvidenceTool) { tool.prepareErr = errors.New(secret) }, ctx: context.Background, reason: EvidenceFailurePreparation},
		{name: "prepare panic", setup: func(tool *preparedEvidenceTool) { tool.panicPrep = true }, ctx: context.Background, reason: EvidenceFailureInternal},
		{name: "tool error", setup: func(tool *preparedEvidenceTool) { tool.runErr = errors.New(secret) }, ctx: context.Background, reason: EvidenceFailureExecution},
		{name: "tool panic", setup: func(tool *preparedEvidenceTool) { tool.panicRun = true }, ctx: context.Background, reason: EvidenceFailureInternal},
		{name: "canceled", ctx: func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, reason: EvidenceFailureCanceled},
		{name: "deadline", ctx: func() context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return ctx
		}, reason: EvidenceFailureDeadline},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			candidate := newPreparedEvidenceTool("workspace_read", "ok")
			if testCase.setup != nil {
				testCase.setup(candidate)
			}
			runner := newTestEvidenceRunner(t, &evidenceAccessStub{access: gate.AccessAllow}, []uuid.UUID{mustRuntimeTestID(t)})
			_, err := runner.run(testCase.ctx(),
				[]hustle.BoundEvidenceTool{boundEvidenceRuntimeTool(t, candidate)},
				[]evidenceToolCall{{id: "call", name: "workspace_read", input: json.RawMessage(`{"secret":"` + secret + `"}`)}},
				hustle.ToolLoopLimits{MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
			)
			assertEvidenceFailure(t, err, testCase.reason)
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q leaked sensitive content", err)
			}
		})
	}
}

func newPreparedEvidenceTool(name, result string) *preparedEvidenceTool {
	return &preparedEvidenceTool{
		info:   evidenceToolInfo(name),
		result: tool.TextResult(result),
	}
}

func evidenceToolInfo(name string) tool.ToolInfo {
	return tool.ToolInfo{
		Name: name, Desc: "Read bounded workspace evidence.",
		Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}
}

func validEvidenceRequest(name string, id uuid.UUID, kind, executionID string) tool.Request {
	return tool.Request{
		ToolName: name, ExecutionID: executionID,
		Requirements: []tool.Requirement{{
			Kind: kind, Match: "/workspace/file", Description: "read evidence",
		}},
	}
}

func evidenceRuntimeDefinition(t *testing.T, factory tool.EvidenceFactory) hustle.BoundDefinition {
	t.Helper()
	return evidenceRuntimeDefinitionWithInfo(t, evidenceToolInfo("workspace_read"), factory)
}

func evidenceRuntimeDefinitionWithInfo(t *testing.T, info tool.ToolInfo, factory tool.EvidenceFactory) hustle.BoundDefinition {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName("test.evidence"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 1024}),
		hustle.WithSystemPrompt("Review only.", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
		hustle.WithNamedInference(successfulRuntimeClient(nil), runtimeStructuredTestModel()),
		hustle.WithOutputSchema(runtimeTestOutputSchema()),
		hustle.WithEvidenceTools(hustle.EvidenceToolPolicy{
			Revision: "evidence-v1",
			Limits: hustle.ToolLoopLimits{
				MaxRounds: 2, MaxCalls: 4, MaxCallsPerRound: 2,
				MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
			},
			Definitions: []tool.Definition{tool.NewEvidenceDefinition(
				"workspace-read", tool.RequiresWorkspaceRead, []tool.ToolInfo{info}, factory,
			)},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := definition.Bind(context.Background(), hustle.Bindings{})
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func boundEvidenceRuntimeTool(t *testing.T, concrete tool.InvokableTool) hustle.BoundEvidenceTool {
	t.Helper()
	info, err := concrete.Info(context.Background())
	if err != nil || info == nil {
		t.Fatalf("Info() = (%#v,%v)", info, err)
	}
	return bindSingleRuntimeEvidenceTool(t, concrete, *info)
}

func bindSingleRuntimeEvidenceTool(t *testing.T, concrete tool.InvokableTool, info tool.ToolInfo) hustle.BoundEvidenceTool {
	t.Helper()
	definition := evidenceRuntimeDefinitionWithInfo(t, info, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{concrete}, nil
	})
	request := hustle.Request{Cause: identity.Cause{Coordinates: identity.Coordinates{
		SessionID: mustRuntimeTestID(t), LoopID: mustRuntimeTestID(t),
	}}}
	catalog, err := bindEvidenceInvocation(context.Background(), definition, request, &tool.ReadWorkspaceBinding{Root: "/workspace"})
	if err != nil || len(catalog) != 1 {
		t.Fatalf("bindEvidenceInvocation() = (%#v,%v)", catalog, err)
	}
	return catalog[0]
}

func newTestEvidenceRunner(t *testing.T, access evidenceAccessEvaluator, ids []uuid.UUID) *evidenceRunner {
	t.Helper()
	index := 0
	runner, err := newEvidenceRunner(access, []string{evidenceReadKind}, func() (uuid.UUID, error) {
		if index >= len(ids) {
			t.Fatal("unexpected execution id request")
		}
		id := ids[index]
		index++
		return id, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func assertEvidenceFailure(t *testing.T, err error, want EvidenceFailureReason) {
	t.Helper()
	var evidenceErr *EvidenceError
	if !errors.As(err, &evidenceErr) || !evidenceErr.Valid() || evidenceErr.Reason != want {
		t.Fatalf("error = %T %v, want valid EvidenceError reason %q", err, err, want)
	}
}
