package delegationtool

import (
	"context"
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
		{Name: "worker", Description: "performs delegated work"},
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
		{name: "start", tool: NewStartAgent(&fakeController{}, loop.DelegationManaged, agentCatalog()), args: `{"name":"map","instructions":"inspect","agent_type":"explorer"}`, want: tool.DelegateStart},
		{name: "message", tool: NewMessageAgent(&fakeController{}, loop.DelegationManaged, agentCatalog()), args: `{"agent_id":"` + delegateID + `","message":"continue"}`, want: tool.DelegateSend},
		{name: "list", tool: NewListAgents(&fakeController{}, loop.DelegationManaged, agentCatalog()), args: `{}`, want: tool.DelegateStatus},
		{name: "stop", tool: NewStopAgent(&fakeController{}, loop.DelegationManaged, agentCatalog()), args: `{"agent_id":"` + delegateID + `"}`, want: tool.DelegateInterrupt},
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

func TestAgentToolResultsUseExactPersistentAgentJSONShapes(t *testing.T) {
	t.Parallel()
	agentID := mustParseUUID(t, "55555555-5555-4555-8555-555555555555")
	tests := []struct {
		name     string
		newTool  func(*fakeController) preparedAgentTool
		args     string
		result   tool.DelegateResult
		wantOp   tool.DelegateOperation
		wantJSON string
	}{
		{name: "start", newTool: func(c *fakeController) preparedAgentTool {
			return NewStartAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"name":"map","instructions":"inspect","agent_type":"explorer"}`, result: tool.DelegateResult{AgentID: agentID, Name: "map", State: tool.AgentStateIdle, ResponseStatus: tool.DelegateResponseCompleted, Response: "done"}, wantOp: tool.DelegateStart, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"map","state":"idle","response":"done"}`},
		{name: "message", newTool: func(c *fakeController) preparedAgentTool {
			return NewMessageAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"agent_id":"` + agentID.String() + `","message":"continue"}`, result: tool.DelegateResult{AgentID: agentID, Name: "map", State: tool.AgentStateIdle, ResponseStatus: tool.DelegateResponseCompleted, Response: "next"}, wantOp: tool.DelegateSend, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"map","state":"idle","response":"next"}`},
		{name: "background admission", newTool: func(c *fakeController) preparedAgentTool {
			return NewMessageAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"agent_id":"` + agentID.String() + `","message":"continue","wait_for_response":false}`, result: tool.DelegateResult{AgentID: agentID, Name: "map", State: tool.AgentStateWorking}, wantOp: tool.DelegateSend, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"map","state":"working"}`},
		{name: "list", newTool: func(c *fakeController) preparedAgentTool {
			return NewListAgents(c, loop.DelegationManaged, agentCatalog())
		}, args: `{}`, result: tool.DelegateResult{Agents: []tool.DelegateAgent{{AgentID: agentID, Name: "map", AgentType: "explorer", State: tool.AgentStateWorking, QueuedMessages: 1, Runtime: tool.DelegateRuntime{Harness: "codex", Source: "gateway", Model: "gpt-5.6-sol", Effort: "high"}, AgentMode: "review"}}, Truncated: true}, wantOp: tool.DelegateStatus, wantJSON: `{"agents":[{"agent_id":"55555555-5555-4555-8555-555555555555","name":"map","agent_type":"explorer","state":"working","queued_messages":1,"agent_harness":"codex","agent_source":"gateway","model":"gpt-5.6-sol","effort":"high","agent_mode":"review"}],"truncated":true}`},
		{name: "stop", newTool: func(c *fakeController) preparedAgentTool {
			return NewStopAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"agent_id":"` + agentID.String() + `"}`, result: tool.DelegateResult{AgentID: agentID, PreviousState: tool.AgentStateWorking, State: tool.AgentStateIdle}, wantOp: tool.DelegateInterrupt, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","previous_state":"working","state":"idle"}`},
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
			if got := textOf(t, result); got != tt.wantJSON {
				t.Errorf("result = %q, want %q", got, tt.wantJSON)
			}
		})
	}
}

func TestAgentToolExecutionUsesTrustedPreparedArtifact(t *testing.T) {
	t.Parallel()
	controller := &fakeController{result: tool.DelegateResult{ResponseStatus: tool.DelegateResponseCompleted, Response: "ok"}}
	agentTool := NewStartAgent(controller, loop.DelegationManaged, agentCatalog())
	request, artifact, err := agentTool.PrepareCall(context.Background(), mustParseUUID(t, "11111111-1111-4111-8111-111111111111"), `{"name":"d","instructions":"prepared","agent_type":"explorer"}`)
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

func TestAgentToolExecutionRejectsPreparedArtifactForAnotherOperation(t *testing.T) {
	t.Parallel()
	controller := &fakeController{}
	start := NewStartAgent(controller, loop.DelegationManaged, agentCatalog())
	message := NewMessageAgent(controller, loop.DelegationManaged, agentCatalog())
	request, artifact, err := message.PrepareCall(context.Background(), mustParseUUID(t, "11111111-1111-4111-8111-111111111111"), `{"agent_id":"55555555-5555-4555-8555-555555555555","message":"prepared"}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{Request: request, Artifact: artifact})
	result, err := start.InvokableRun(ctx, `{"agent_type":"explorer","instructions":"untrusted"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, result); got != "error: agent call unavailable" {
		t.Fatalf("result = %q, want unavailable", got)
	}
	if got := controller.last(); got.Operation != 0 {
		t.Fatalf("controller received cross-operation request: %+v", got)
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
