package delegationtool

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

type fakeController struct {
	mu       sync.Mutex
	result   tool.DelegateResult
	execErr  error
	requests []tool.DelegateRequest
}

func (f *fakeController) Execute(_ context.Context, request tool.DelegateRequest) (tool.DelegateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	return f.result, f.execErr
}

func (f *fakeController) last() tool.DelegateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return tool.DelegateRequest{}
	}
	return f.requests[len(f.requests)-1]
}

func agentCatalog() []AgentCatalogEntry {
	return []AgentCatalogEntry{
		{Name: "operator", Description: "edits files and runs commands", Modes: []loop.ModeName{"", "build"}},
		{Name: "explorer", Description: "searches the workspace", Modes: []loop.ModeName{"", "review"}},
	}
}

type preparedAgentTool interface {
	tool.InvokableTool
	tool.CallPreparer
}

func invokePrepared(t *testing.T, agentTool preparedAgentTool, args string) (*tool.ToolResult, error) {
	t.Helper()
	executionID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	request, artifact, err := agentTool.PrepareCall(context.Background(), executionID, args)
	if err != nil {
		return tool.TextResult("error: agent call unavailable"), nil
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{Request: request, Artifact: artifact})
	return agentTool.InvokableRun(ctx, args)
}

func textOf(t *testing.T, result *tool.ToolResult) string {
	t.Helper()
	if result == nil {
		t.Fatal("nil ToolResult")
	}
	if len(result.Content) != 1 {
		t.Fatalf("ToolResult content length = %d, want 1", len(result.Content))
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("ToolResult content = %T, want *content.TextBlock", result.Content[0])
	}
	return block.Text
}

func mustParseUUID(t *testing.T, value string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAgentToolsPrepareFixedControllerOperations(t *testing.T) {
	t.Parallel()
	delegateID := "55555555-5555-4555-8555-555555555555"
	tests := []struct {
		name string
		tool preparedAgentTool
		args string
		want tool.DelegateOperation
	}{
		{name: "start", tool: NewStartAgent(&fakeController{}, loop.DelegationManaged, agentCatalog()), args: `{"description":"map","prompt":"inspect","subagent_type":"explorer"}`, want: tool.DelegateStart},
		{name: "message", tool: NewMessageAgent(&fakeController{}, loop.DelegationManaged, agentCatalog()), args: `{"action":"send","delegate_id":"` + delegateID + `","prompt":"continue"}`, want: tool.DelegateSend},
		{name: "list", tool: NewListAgents(&fakeController{}, loop.DelegationManaged, agentCatalog()), args: `{"action":"status"}`, want: tool.DelegateStatus},
		{name: "stop", tool: NewStopAgent(&fakeController{}, loop.DelegationManaged, agentCatalog()), args: `{"action":"interrupt","delegate_id":"` + delegateID + `"}`, want: tool.DelegateInterrupt},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, artifact, err := tt.tool.PrepareCall(context.Background(), mustParseUUID(t, "11111111-1111-4111-8111-111111111111"), tt.args)
			if err != nil {
				t.Fatalf("PrepareCall() error = %v", err)
			}
			got := artifact.(tool.DelegateArtifact).Request.Operation
			if got != tt.want {
				t.Fatalf("prepared operation = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAgentToolsInvokePreparedRequests(t *testing.T) {
	t.Parallel()
	delegateID := mustParseUUID(t, "55555555-5555-4555-8555-555555555555")
	requestID := mustParseUUID(t, "66666666-6666-4666-8666-666666666666")
	tests := []struct {
		name       string
		newTool    func(*fakeController) preparedAgentTool
		args       string
		result     tool.DelegateResult
		wantOp     tool.DelegateOperation
		wantOutput string
	}{
		{name: "start", newTool: func(c *fakeController) preparedAgentTool {
			return NewStartAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"description":"map","prompt":"inspect","subagent_type":"explorer","run_in_background":false}`, result: tool.DelegateResult{Status: tool.DelegateStatusCompleted, Output: "done"}, wantOp: tool.DelegateStart, wantOutput: "done"},
		{name: "message", newTool: func(c *fakeController) preparedAgentTool {
			return NewMessageAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"action":"send","delegate_id":"` + delegateID.String() + `","prompt":"continue"}`, result: tool.DelegateResult{DelegateID: delegateID, RequestID: requestID, Status: tool.DelegateStatusQueued}, wantOp: tool.DelegateSend, wantOutput: `"status":"queued"`},
		{name: "list", newTool: func(c *fakeController) preparedAgentTool {
			return NewListAgents(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"action":"status"}`, result: tool.DelegateResult{Children: []tool.DelegateChildStatus{{DelegateID: delegateID, Status: tool.DelegateStatusIdle}}}, wantOp: tool.DelegateStatus, wantOutput: `"children"`},
		{name: "stop", newTool: func(c *fakeController) preparedAgentTool {
			return NewStopAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"action":"interrupt","delegate_id":"` + delegateID.String() + `"}`, result: tool.DelegateResult{DelegateID: delegateID, Status: tool.DelegateStatusInterrupted}, wantOp: tool.DelegateInterrupt, wantOutput: `"status":"interrupted"`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			controller := &fakeController{result: tt.result}
			result, err := invokePrepared(t, tt.newTool(controller), tt.args)
			if err != nil {
				t.Fatalf("InvokableRun() error = %v", err)
			}
			if got := controller.last().Operation; got != tt.wantOp {
				t.Errorf("controller operation = %v, want %v", got, tt.wantOp)
			}
			if got := textOf(t, result); !strings.Contains(got, tt.wantOutput) {
				t.Errorf("result = %q, want substring %q", got, tt.wantOutput)
			}
		})
	}
}

func TestAgentToolExecutionUsesTrustedPreparedArtifact(t *testing.T) {
	t.Parallel()
	controller := &fakeController{result: tool.DelegateResult{Status: tool.DelegateStatusCompleted, Output: "ok"}}
	agentTool := NewStartAgent(controller, loop.DelegationManaged, agentCatalog())
	request, artifact, err := agentTool.PrepareCall(context.Background(), mustParseUUID(t, "11111111-1111-4111-8111-111111111111"), `{"description":"d","prompt":"prepared","subagent_type":"explorer","run_in_background":false}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{Request: request, Artifact: artifact})
	ctx = loop.WithToolUseID(ctx, "trusted-tool-use-id")
	if _, err := agentTool.InvokableRun(ctx, `{"prompt":"untrusted"}`); err != nil {
		t.Fatal(err)
	}
	got := controller.last()
	if got.Message != "prepared" || got.ParentToolUseID != "trusted-tool-use-id" {
		t.Fatalf("controller request = %+v, want prepared message and trusted tool-use id", got)
	}
}

func TestAgentToolCapabilities(t *testing.T) {
	t.Parallel()
	tools := []preparedAgentTool{
		NewListAgents(&fakeController{}, loop.DelegationManaged, agentCatalog()),
		NewMessageAgent(&fakeController{}, loop.DelegationManaged, agentCatalog()),
		NewStartAgent(&fakeController{}, loop.DelegationManaged, agentCatalog()),
		NewStopAgent(&fakeController{}, loop.DelegationManaged, agentCatalog()),
	}
	for _, agentTool := range tools {
		if _, ok := any(agentTool).(tool.WriteTarget); ok {
			t.Errorf("%T unexpectedly implements WriteTarget", agentTool)
		}
		if _, ok := any(agentTool).(tool.Auditable); !ok {
			t.Errorf("%T does not implement Auditable", agentTool)
		}
	}
}
