package delegationtool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func TestFormatForegroundBoundsMultibyteResponseAtRuneBoundary(t *testing.T) {
	t.Parallel()

	result := formatForeground(tool.DelegateResult{
		Name:           "map",
		State:          tool.AgentStateIdle,
		ResponseStatus: tool.DelegateResponseCompleted,
		Response:       strings.Repeat("界", maxAgentResultBytes),
	})
	if len(result) > maxAgentResultBytes {
		t.Fatalf("encoded result length = %d, want <= %d", len(result), maxAgentResultBytes)
	}
	if !utf8.ValidString(result) {
		t.Fatal("encoded result is not valid UTF-8")
	}
	var decoded foregroundResult
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded.Response == nil {
		t.Fatal("decoded response is nil")
	}
	if !utf8.ValidString(*decoded.Response) {
		t.Fatal("decoded response is not valid UTF-8")
	}
	if strings.ContainsRune(*decoded.Response, utf8.RuneError) {
		t.Fatal("decoded response contains a replacement rune from partial truncation")
	}
	if *decoded.Response == "" || strings.Trim(*decoded.Response, "界") != "" {
		t.Fatal("decoded response did not preserve a complete-rune prefix")
	}
}

func TestMessageAgentFormatForegroundIncludesDeliveryStatus(t *testing.T) {
	t.Parallel()

	agentID := mustParseUUID(t, "55555555-5555-4555-8555-555555555555")
	got := formatForeground(tool.DelegateResult{
		AgentID:        agentID,
		Name:           "worker",
		State:          tool.AgentStateIdle,
		DeliveryStatus: tool.DelegateDeliveryInjected,
		ResponseStatus: tool.DelegateResponseCompleted,
		Response:       "done",
	})
	want := `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"idle","delivery_status":"injected","response_status":"completed","response":"done"}`
	if got != want {
		t.Fatalf("formatForeground() = %q, want %q", got, want)
	}
}

func TestFormatForegroundPreservesEmptyLegacyCompletedResponse(t *testing.T) {
	t.Parallel()

	agentID := mustParseUUID(t, "55555555-5555-4555-8555-555555555555")
	got := formatForeground(tool.DelegateResult{
		AgentID:        agentID,
		Name:           "worker",
		State:          tool.AgentStateIdle,
		ResponseStatus: tool.DelegateResponseCompleted,
	})
	want := `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"idle","response":""}`
	if got != want {
		t.Fatalf("formatForeground() = %q, want %q", got, want)
	}
}

func TestFormatForegroundPreservesEmptyDeliveryAwareCompletedResponse(t *testing.T) {
	t.Parallel()

	agentID := mustParseUUID(t, "55555555-5555-4555-8555-555555555555")
	got := formatForeground(tool.DelegateResult{
		AgentID:        agentID,
		Name:           "worker",
		State:          tool.AgentStateIdle,
		DeliveryStatus: tool.DelegateDeliveryInjected,
		ResponseStatus: tool.DelegateResponseCompleted,
	})
	want := `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"idle","delivery_status":"injected","response_status":"completed","response":""}`
	if got != want {
		t.Fatalf("formatForeground() = %q, want %q", got, want)
	}
}

func TestMessageAgentDeliveryStatusFormatting(t *testing.T) {
	t.Parallel()

	agentID := mustParseUUID(t, "55555555-5555-4555-8555-555555555555")
	tests := []struct {
		name     string
		delivery tool.DelegateDeliveryStatus
		wantJSON string
	}{
		{name: "accepted pending", delivery: tool.DelegateDeliveryAcceptedPending, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"working","delivery_status":"accepted_pending"}`},
		{name: "injected", delivery: tool.DelegateDeliveryInjected, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"working","delivery_status":"injected"}`},
		{name: "queued", delivery: tool.DelegateDeliveryQueued, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"working","delivery_status":"queued"}`},
		{name: "rejected", delivery: tool.DelegateDeliveryRejected, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"working","delivery_status":"rejected"}`},
		{name: "delivery unknown", delivery: tool.DelegateDeliveryUnknown, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"working","delivery_status":"delivery_unknown"}`},
		{name: "delivered untrackable", delivery: tool.DelegateDeliveryUntrackable, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"working","delivery_status":"delivered_untrackable"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatMessageAgentResult(tool.DelegateRequest{}, tool.DelegateResult{
				AgentID:        agentID,
				Name:           "worker",
				State:          tool.AgentStateWorking,
				DeliveryStatus: tt.delivery,
				ResponseStatus: tool.DelegateResponseCompleted,
				Response:       "not returned immediately",
			})
			if got != tt.wantJSON {
				t.Fatalf("formatMessageAgentResult() = %q, want %q", got, tt.wantJSON)
			}
		})
	}
}

func TestMessageAgentResponseStatusFormatting(t *testing.T) {
	t.Parallel()

	agentID := mustParseUUID(t, "55555555-5555-4555-8555-555555555555")
	tests := []struct {
		name           string
		responseStatus tool.DelegateResponseStatus
		wantJSON       string
	}{
		{name: "completed", responseStatus: tool.DelegateResponseCompleted, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"idle","delivery_status":"injected","response_status":"completed","response":"reply"}`},
		{name: "failed", responseStatus: tool.DelegateResponseFailed, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"idle","delivery_status":"injected","response_status":"failed"}`},
		{name: "interrupted", responseStatus: tool.DelegateResponseInterrupted, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"idle","delivery_status":"injected","response_status":"interrupted"}`},
		{name: "timed out", responseStatus: tool.DelegateResponseTimedOut, wantJSON: `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"idle","delivery_status":"injected","response_status":"timed_out"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			response := ""
			if tt.responseStatus == tool.DelegateResponseCompleted {
				response = "reply"
			}
			got := formatMessageAgentResult(tool.DelegateRequest{WaitForResponse: true}, tool.DelegateResult{
				AgentID:        agentID,
				Name:           "worker",
				State:          tool.AgentStateIdle,
				DeliveryStatus: tool.DelegateDeliveryInjected,
				ResponseStatus: tt.responseStatus,
				Response:       response,
				CorrelationID:  mustParseUUID(t, "66666666-6666-4666-8666-666666666666"),
			})
			if got != tt.wantJSON {
				t.Fatalf("formatMessageAgentResult() = %q, want %q", got, tt.wantJSON)
			}
		})
	}
}

func TestMessageAgentUnknownDeliveryOmitsResponseStatus(t *testing.T) {
	t.Parallel()

	agentID := mustParseUUID(t, "55555555-5555-4555-8555-555555555555")
	for _, delivery := range []tool.DelegateDeliveryStatus{tool.DelegateDeliveryUnknown, tool.DelegateDeliveryUntrackable} {
		delivery := delivery
		t.Run(string(delivery), func(t *testing.T) {
			t.Parallel()
			got := formatMessageAgentResult(tool.DelegateRequest{WaitForResponse: true}, tool.DelegateResult{
				AgentID:        agentID,
				Name:           "worker",
				State:          tool.AgentStateWorking,
				DeliveryStatus: delivery,
				ResponseStatus: tool.DelegateResponseCompleted,
				Response:       "must not be correlated",
			})
			want := `{"agent_id":"55555555-5555-4555-8555-555555555555","name":"worker","state":"working","delivery_status":"` + string(delivery) + `"}`
			if got != want {
				t.Fatalf("formatMessageAgentResult() = %q, want %q", got, want)
			}
		})
	}
}

func TestFormatForegroundBoundsEncodedEscapedResponse(t *testing.T) {
	t.Parallel()

	result := formatForeground(tool.DelegateResult{
		Name:           "map",
		State:          tool.AgentStateIdle,
		ResponseStatus: tool.DelegateResponseCompleted,
		Response:       strings.Repeat("\x01", maxAgentResultBytes),
	})
	if len(result) > maxAgentResultBytes {
		t.Fatalf("encoded result length = %d, want <= %d", len(result), maxAgentResultBytes)
	}
	if !utf8.ValidString(result) {
		t.Fatal("encoded result is not valid UTF-8")
	}
	var decoded foregroundResult
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded.Response == nil || *decoded.Response == "" || strings.Trim(*decoded.Response, "\x01") != "" {
		t.Fatal("decoded response did not preserve the escaped response prefix")
	}
}

func TestFailedStartAgentReturnsRawStructuredFailureDetail(t *testing.T) {
	t.Parallel()
	const detail = "ACP error 429: retry later"
	controller := &fakeController{result: tool.DelegateResult{
		ResponseStatus: tool.DelegateResponseFailed,
		Response:       detail,
	}}
	start := NewStartAgent(controller, loop.DelegationManaged, agentCatalog())
	result, err := invokePrepared(t, start, `{"name":"map","instructions":"inspect","agent_type":"explorer"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if got := errorTextOf(t, result); got != "StartAgent failed: "+detail {
		t.Fatalf("result = %q, want raw failure detail", got)
	}
}

func TestFailedStartAgentBoundsMalformedFailureDetail(t *testing.T) {
	t.Parallel()
	detail := strings.Repeat("界", maxAgentResultBytes) + "\xff"
	controller := &fakeController{result: tool.DelegateResult{
		ResponseStatus: tool.DelegateResponseFailed,
		Response:       detail,
	}}
	start := NewStartAgent(controller, loop.DelegationManaged, agentCatalog())
	result, err := invokePrepared(t, start, `{"name":"map","instructions":"inspect","agent_type":"explorer"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	got := errorTextOf(t, result)
	if len(got) > maxAgentResultBytes {
		t.Fatalf("result bytes = %d, want <= %d", len(got), maxAgentResultBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("result is not valid UTF-8")
	}
	if !strings.HasPrefix(got, "StartAgent failed: ") {
		t.Fatalf("result = %q, want bounded failure detail", got)
	}
}

func TestStartAgentImmediateErrorReturnsRawStructuredFailure(t *testing.T) {
	t.Parallel()
	controller := &fakeController{execErr: errors.New("dial failed: raw provider response")}
	start := NewStartAgent(controller, loop.DelegationManaged, agentCatalog())
	result, err := invokePrepared(t, start, `{"name":"map","instructions":"inspect","agent_type":"explorer"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if got := errorTextOf(t, result); got != "StartAgent failed: dial failed: raw provider response" {
		t.Fatalf("result = %q, want exact raw failure", got)
	}
}

func TestForegroundAgentTerminalStatusesReturnStructuredFailures(t *testing.T) {
	t.Parallel()
	agentID := "55555555-5555-4555-8555-555555555555"
	tests := []struct {
		name   string
		tool   func(*fakeController) preparedAgentTool
		args   string
		result tool.DelegateResult
		want   string
	}{
		{name: "start failed with detail", tool: func(c *fakeController) preparedAgentTool {
			return NewStartAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"name":"map","instructions":"inspect","agent_type":"explorer"}`, result: tool.DelegateResult{ResponseStatus: tool.DelegateResponseFailed, Response: "child bootstrap failed"}, want: "StartAgent failed: child bootstrap failed"},
		{name: "start interrupted", tool: func(c *fakeController) preparedAgentTool {
			return NewStartAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"name":"map","instructions":"inspect","agent_type":"explorer"}`, result: tool.DelegateResult{ResponseStatus: tool.DelegateResponseInterrupted}, want: "StartAgent failed: agent interrupted"},
		{name: "start timed out", tool: func(c *fakeController) preparedAgentTool {
			return NewStartAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"name":"map","instructions":"inspect","agent_type":"explorer"}`, result: tool.DelegateResult{ResponseStatus: tool.DelegateResponseTimedOut}, want: "StartAgent failed: agent timed out"},
		{name: "start invalid status", tool: func(c *fakeController) preparedAgentTool {
			return NewStartAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"name":"map","instructions":"inspect","agent_type":"explorer"}`, result: tool.DelegateResult{}, want: "StartAgent failed: agent returned invalid response status"},
		{name: "message failed", tool: func(c *fakeController) preparedAgentTool {
			return NewMessageAgent(c, loop.DelegationManaged, agentCatalog())
		}, args: `{"agent_id":"` + agentID + `","message":"continue"}`, result: tool.DelegateResult{ResponseStatus: tool.DelegateResponseFailed, Response: "child rejected message"}, want: "MessageAgent failed: child rejected message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			controller := &fakeController{result: tt.result}
			result, err := invokePrepared(t, tt.tool(controller), tt.args)
			if err != nil {
				t.Fatalf("InvokableRun() error = %v", err)
			}
			if got := errorTextOf(t, result); got != tt.want {
				t.Fatalf("result = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMessageAgentAcceptedPendingRemainsSuccessfulDeliveryEnvelope(t *testing.T) {
	t.Parallel()
	const agentID = "55555555-5555-4555-8555-555555555555"
	controller := &fakeController{result: tool.DelegateResult{
		AgentID:        mustParseUUID(t, agentID),
		Name:           "map",
		State:          tool.AgentStateWorking,
		DeliveryStatus: tool.DelegateDeliveryAcceptedPending,
		ResponseStatus: tool.DelegateResponseUnknown,
	}}
	message := NewMessageAgent(controller, loop.DelegationManaged, agentCatalog())
	result, err := invokePrepared(t, message, `{"agent_id":"`+agentID+`","message":"continue"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	want := `{"agent_id":"` + agentID + `","name":"map","state":"working","delivery_status":"accepted_pending"}`
	if got := textOf(t, result); got != want {
		t.Fatalf("result = %q, want successful delivery envelope %q", got, want)
	}
}

func TestAgentToolImmediateErrorsReturnOperationSpecificRawStructuredFailures(t *testing.T) {
	t.Parallel()
	agentID := "55555555-5555-4555-8555-555555555555"
	tests := []struct {
		name string
		tool preparedAgentTool
		args string
		want string
	}{
		{name: "StartAgent", tool: NewStartAgent(&fakeController{execErr: errors.New("dial failed: raw provider response")}, loop.DelegationManaged, agentCatalog()), args: `{"name":"map","instructions":"inspect","agent_type":"explorer"}`, want: "StartAgent failed: dial failed: raw provider response"},
		{name: "MessageAgent", tool: NewMessageAgent(&fakeController{execErr: errors.New("dial failed: raw provider response")}, loop.DelegationManaged, agentCatalog()), args: `{"agent_id":"` + agentID + `","message":"continue"}`, want: "MessageAgent failed: dial failed: raw provider response"},
		{name: "ListAgents", tool: NewListAgents(&fakeController{execErr: errors.New("dial failed: raw provider response")}, loop.DelegationManaged, agentCatalog()), args: `{}`, want: "ListAgents failed: dial failed: raw provider response"},
		{name: "StopAgent", tool: NewStopAgent(&fakeController{execErr: errors.New("dial failed: raw provider response")}, loop.DelegationManaged, agentCatalog()), args: `{"agent_id":"` + agentID + `"}`, want: "StopAgent failed: dial failed: raw provider response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := invokePrepared(t, tt.tool, tt.args)
			if err != nil {
				t.Fatalf("InvokableRun() error = %v", err)
			}
			if got := errorTextOf(t, result); got != tt.want {
				t.Fatalf("result = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentToolUnavailablePreparedCallIsStructuredFailure(t *testing.T) {
	t.Parallel()
	start := NewStartAgent(&fakeController{}, loop.DelegationManaged, agentCatalog())
	result, err := start.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if got := errorTextOf(t, result); got != "StartAgent failed: agent call unavailable" {
		t.Fatalf("result = %q, want unavailable failure", got)
	}
}

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

func errorTextOf(t *testing.T, result *tool.ToolResult) string {
	t.Helper()
	if result == nil {
		t.Fatal("nil ToolResult")
	}
	if len(result.Content) != 1 {
		t.Fatalf("ToolResult content length = %d, want 1", len(result.Content))
	}
	outer, ok := result.Content[0].(*content.ToolResultBlock)
	if !ok {
		t.Fatalf("ToolResult content = %T, want *content.ToolResultBlock", result.Content[0])
	}
	if !outer.IsError {
		t.Fatal("ToolResultBlock.IsError = false, want true")
	}
	if len(outer.Content) != 1 {
		t.Fatalf("ToolResultBlock content length = %d, want 1", len(outer.Content))
	}
	text, ok := outer.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("ToolResultBlock content = %T, want *content.TextBlock", outer.Content[0])
	}
	return text.Text
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
	if got := errorTextOf(t, result); got != "StartAgent failed: agent call unavailable" {
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
