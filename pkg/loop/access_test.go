package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// fakeAccessGate is a minimal AccessGate for wiring tests.
type fakeAccessGate struct{ calls int }

func (g *fakeAccessGate) Authorize(context.Context, tool.Request) (gate.Resolution, error) {
	g.calls++
	return gate.Resolution{Approved: true}, nil
}

func TestWithAccessGateBinds(t *testing.T) {
	t.Parallel()
	access := &fakeAccessGate{}
	d, err := Define(
		WithName("agent"),
		WithInference(&fakeLLM{}, testModel()),
		WithAccessGate(access),
		WithPolicyRevision("rev-1"),
	)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	bound, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if bound.Access() != AccessGate(access) {
		t.Fatalf("bound.Access() = %v, want the configured gate", bound.Access())
	}
}

func TestDefineAccessGateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
		kind DefinitionErrorKind
	}{
		{
			name: "nil access gate",
			opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithAccessGate(nil), WithPolicyRevision("rev-1")},
			kind: DefinitionInvalidAccessGate,
		},
		{
			name: "opaque access gate lacks revision",
			opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithAccessGate(&fakeAccessGate{})},
			kind: DefinitionMissingPolicyRevision,
		},
		{
			name: "duplicate access gate",
			opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithAccessGate(&fakeAccessGate{}), WithAccessGate(&fakeAccessGate{}), WithPolicyRevision("rev-1")},
			kind: DefinitionDuplicateOption,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Define(tt.opts...)
			var definitionErr *DefinitionError
			if !errors.As(err, &definitionErr) || definitionErr.Kind != tt.kind {
				t.Fatalf("Define() error = %T %v, want *DefinitionError kind %q", err, err, tt.kind)
			}
		})
	}
}

func TestPreparedCallContextRoundTrip(t *testing.T) {
	t.Parallel()
	id := mustUUID(t)
	prepared := tool.PreparedCall{
		ExecutionID: id,
		Request:     tool.Request{ToolName: "Bash", Summary: "run git status"},
		Artifact:    tool.TokenArtifact{Token: "artifact"},
		Grants:      []string{"grant-token"},
	}

	if _, ok := PreparedCallFromContext(context.Background()); ok {
		t.Fatal("PreparedCallFromContext(background) ok = true, want false")
	}
	ctx := WithPreparedCall(context.Background(), prepared)
	got, ok := PreparedCallFromContext(ctx)
	if !ok {
		t.Fatal("PreparedCallFromContext ok = false, want true")
	}
	if got.ExecutionID != id || got.Request.ToolName != "Bash" || len(got.Grants) != 1 || got.Grants[0] != "grant-token" {
		t.Fatalf("PreparedCallFromContext = %+v, want the stored contract", got)
	}
	if artifact, ok := got.Artifact.(tool.TokenArtifact); !ok || artifact.Token != "artifact" {
		t.Fatalf("Artifact = %#v, want the stored TokenArtifact", got.Artifact)
	}
}

func TestGateApproverFailsClosedWithoutRequester(t *testing.T) {
	t.Parallel()
	_, err := GateApprover().RequestApproval(context.Background(), gate.ApprovalPrompt{})
	var ctxErr *ApprovalContextError
	if !errors.As(err, &ctxErr) {
		t.Fatalf("RequestApproval() error = %T %v, want *ApprovalContextError", err, err)
	}
}

func TestGateApproverRoutesToInstalledRequester(t *testing.T) {
	t.Parallel()
	var got gate.ApprovalPrompt
	requester := func(_ context.Context, prompt gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		got = prompt
		return gate.ApprovalApproveAlwaysWorkspace, nil
	}
	ctx := WithApprovalRequester(context.Background(), requester)
	prompt := gate.ApprovalPrompt{Request: tool.Request{ToolName: "Bash", ExecutionID: uuid.UUID{}.String()}}
	action, err := GateApprover().RequestApproval(ctx, prompt)
	if err != nil {
		t.Fatalf("RequestApproval() error = %v", err)
	}
	if action != gate.ApprovalApproveAlwaysWorkspace {
		t.Fatalf("action = %q, want %q", action, gate.ApprovalApproveAlwaysWorkspace)
	}
	if got.Request.ToolName != "Bash" {
		t.Fatalf("routed prompt = %+v, want the caller's prompt", got)
	}
}
