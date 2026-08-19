package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
)

type backgroundCompletion struct {
	AgentID        string                      `json:"agent_id"`
	Name           string                      `json:"name"`
	State          tool.AgentState             `json:"state"`
	ResponseStatus tool.DelegateResponseStatus `json:"response_status"`
	CorrelationID  string                      `json:"correlation_id"`
	Response       string                      `json:"response"`
}

type controlledAgentLLM struct {
	started chan string
	release chan struct{}
}

func newControlledAgentLLM() *controlledAgentLLM {
	return &controlledAgentLLM{started: make(chan string, 8), release: make(chan struct{}, 8)}
}

func (*controlledAgentLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("controlledAgentLLM.Invoke not used")
}

func (l *controlledAgentLLM) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	message := latestUserText(req.Messages)
	l.started <- message
	released := false
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if !released {
			select {
			case <-l.release:
				released = true
				return textChunk("reply " + message), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, io.EOF
	}, nil), nil
}

func backgroundNode(name string, client inference.Client, delegates ...identity.AgentName) loop.Definition {
	return mustDefine(
		loop.WithName(identity.AgentName(name)),
		loop.WithInference(client, validModel(name)),
		loop.WithDelegates(delegates...),
		loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

func receiveBackgroundCompletion(t *testing.T, parent *controlledAgentLLM) backgroundCompletion {
	t.Helper()
	select {
	case raw := <-parent.started:
		var got backgroundCompletion
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("background completion = %q: %v", raw, err)
		}
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("background completion did not reach parent")
		return backgroundCompletion{}
	}
}

func TestBackgroundAgentReturnsImmediatelyAndHandsBackExactlyOnce(t *testing.T) {
	parentLLM := newControlledAgentLLM()
	childLLM := newControlledAgentLLM()
	parent := backgroundNode("parent", parentLLM, "child")
	child := backgroundNode("child", childLLM)
	s := newDelegationSession(t, parent, nil, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	result, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{
		Operation: tool.DelegateStart, AgentType: "child", Name: "worker", Message: "go", WaitForResponse: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AgentID.IsZero() || result.State != tool.AgentStateWorking {
		t.Fatalf("background admission = %+v, want agent id and working", result)
	}
	if result.Response != "" || result.ResponseStatus != tool.DelegateResponseUnknown {
		t.Fatalf("background admission leaked response as tool result: %+v", result)
	}
	select {
	case got := <-childLLM.started:
		if got != "go" {
			t.Fatalf("child input = %q, want go", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("child did not start")
	}

	childLLM.release <- struct{}{}
	completion := receiveBackgroundCompletion(t, parentLLM)
	if completion.AgentID != result.AgentID.String() || completion.Name != "worker" || completion.State != tool.AgentStateIdle || completion.ResponseStatus != tool.DelegateResponseCompleted || completion.CorrelationID == "" || completion.Response != "reply go" {
		t.Fatalf("completion = %+v", completion)
	}
	parentLLM.release <- struct{}{}
	if err := s.WaitIdle(delegateCtx(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case duplicate := <-parentLLM.started:
		t.Fatalf("duplicate background completion: %q", duplicate)
	default:
	}
}

func TestBackgroundAgentFailureDetailIsDurable(t *testing.T) {
	parentLLM := newControlledAgentLLM()
	providerErr := errors.New("provider rejected model alias")
	childLLM := &releasedFailureLLM{started: make(chan struct{}), release: make(chan struct{}), err: providerErr}
	appender := &blockingSubagentResultAppender{reached: make(chan struct{}), release: make(chan struct{})}
	parent := backgroundNode("parent", parentLLM, "child")
	child := backgroundNode("child", childLLM)
	s := newDelegationSession(t, parent, []Option{WithCommandAppender(appender)}, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	started, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{
		Operation: tool.DelegateStart, AgentType: "child", Name: "worker", Message: "go", WaitForResponse: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-childLLM.started:
	case <-time.After(2 * time.Second):
		t.Fatal("child did not start")
	}
	close(childLLM.release)
	select {
	case <-appender.reached:
	case <-time.After(2 * time.Second):
		t.Fatal("failed child did not append durable SubagentResult")
	}

	appender.mu.Lock()
	records := append([]journal.CommandRecord(nil), appender.records...)
	appender.mu.Unlock()
	var handBack command.SubagentResult
	var found bool
	for _, record := range records {
		if result, ok := record.Command().(command.SubagentResult); ok {
			handBack, found = result, true
			break
		}
	}
	if !found {
		t.Fatalf("durable records = %+v, want command.SubagentResult", records)
	}
	completion, ok := decodeBackgroundCompletion(handBack.Blocks)
	if !ok || completion.ResponseStatus != tool.DelegateResponseFailed || completion.Response != providerErr.Error() || completion.CorrelationID != started.CorrelationID.String() {
		t.Fatalf("durable failure completion = %+v, %v; want failed response %q correlated to %v", completion, ok, providerErr, started.CorrelationID)
	}

	close(appender.release)
	parentCompletion := receiveBackgroundCompletion(t, parentLLM)
	if parentCompletion.ResponseStatus != tool.DelegateResponseFailed || parentCompletion.Response != providerErr.Error() {
		t.Fatalf("parent failure completion = %+v, want failed response %q", parentCompletion, providerErr)
	}
	parentLLM.release <- struct{}{}
}

func TestBackgroundAgentQueuesHandBackWhileParentWorking(t *testing.T) {
	parentLLM := newControlledAgentLLM()
	parent := backgroundNode("parent", parentLLM, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "child done"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	if _, err := s.Submit(context.Background(), textBlocks("occupy")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-parentLLM.started:
		if got != "occupy" {
			t.Fatalf("parent input = %q, want occupy", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parent did not start occupying turn")
	}
	started, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case unexpected := <-parentLLM.started:
		t.Fatalf("hand-back bypassed working parent turn: %q", unexpected)
	default:
	}

	parentLLM.release <- struct{}{}
	completion := receiveBackgroundCompletion(t, parentLLM)
	if completion.AgentID != started.AgentID.String() || completion.Response != "child done" {
		t.Fatalf("queued completion = %+v", completion)
	}
	parentLLM.release <- struct{}{}
}

func TestBackgroundAgentTimeoutHandsBackOnce(t *testing.T) {
	parentLLM := newControlledAgentLLM()
	parent := backgroundNode("parent", parentLLM, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)
	zero := 0

	started, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{
		Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false, TimeoutSeconds: &zero,
	})
	if err != nil {
		t.Fatal(err)
	}
	completion := receiveBackgroundCompletion(t, parentLLM)
	if completion.AgentID != started.AgentID.String() || completion.ResponseStatus != tool.DelegateResponseTimedOut || completion.Response != "" {
		t.Fatalf("timed completion = %+v", completion)
	}
	parentLLM.release <- struct{}{}
	if err := s.WaitIdle(delegateCtx(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case duplicate := <-parentLLM.started:
		t.Fatalf("duplicate timeout completion: %q", duplicate)
	default:
	}
}

func TestBackgroundAgentOmittedTimeoutWaitsForExplicitControl(t *testing.T) {
	parentLLM := newControlledAgentLLM()
	childLLM := newControlledAgentLLM()
	parent := backgroundNode("parent", parentLLM, "child")
	child := backgroundNode("child", childLLM)
	s := newDelegationSession(t, parent, nil, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	started, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-childLLM.started:
	case <-time.After(2 * time.Second):
		t.Fatal("child did not start")
	}
	select {
	case unexpected := <-parentLLM.started:
		t.Fatalf("omitted timeout completed without terminal control: %q", unexpected)
	default:
	}
	if _, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateInterrupt, AgentID: started.AgentID}); err != nil {
		t.Fatal(err)
	}
	completion := receiveBackgroundCompletion(t, parentLLM)
	if completion.ResponseStatus != tool.DelegateResponseInterrupted {
		t.Fatalf("controlled completion = %+v, want interrupted", completion)
	}
	parentLLM.release <- struct{}{}
}

type blockingSubagentResultAppender struct {
	mu      sync.Mutex
	records []journal.CommandRecord
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockingSubagentResultAppender) AppendCommand(ctx context.Context, rec journal.CommandRecord) error {
	a.mu.Lock()
	a.records = append(a.records, rec)
	a.mu.Unlock()
	if _, ok := rec.Command().(command.SubagentResult); !ok {
		return nil
	}
	a.once.Do(func() { close(a.reached) })
	select {
	case <-a.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestBackgroundAgentWaitIdleBridgesTerminalToParentAdmission(t *testing.T) {
	parentLLM := newControlledAgentLLM()
	childLLM := newControlledAgentLLM()
	appender := &blockingSubagentResultAppender{reached: make(chan struct{}), release: make(chan struct{})}
	parent := backgroundNode("parent", parentLLM, "child")
	child := backgroundNode("child", childLLM)
	s := newDelegationSession(t, parent, []Option{WithCommandAppender(appender)}, child)
	ctrl := s.delegation.controllerFor(s.ActiveLoopID(), parent)

	if _, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Message: "go", WaitForResponse: false}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-childLLM.started:
	case <-time.After(2 * time.Second):
		t.Fatal("child did not start")
	}
	childLLM.release <- struct{}{}
	select {
	case <-appender.reached:
	case <-time.After(2 * time.Second):
		t.Fatal("child terminal did not reach parent admission barrier")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.WaitIdle(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitIdle in terminal/admission gap = %v, want deadline", err)
	}
	close(appender.release)
	completion := receiveBackgroundCompletion(t, parentLLM)
	if completion.Response != "reply go" {
		t.Fatalf("completion = %+v", completion)
	}
	parentLLM.release <- struct{}{}
	if err := s.WaitIdle(delegateCtx(t)); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundAgentCompletionIsBoundedJSON(t *testing.T) {
	completion := backgroundCompletionBlocks(mustUUID(), strings.Repeat("n", 1024), mustUUID(), tool.DelegateStatusCompleted, strings.Repeat("\"\n", maxDelegateOutputBytes))
	if len(completion) != 1 {
		t.Fatalf("blocks = %d, want 1", len(completion))
	}
	text, ok := completion[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block = %T, want TextBlock", completion[0])
	}
	if len(text.Text) > maxDelegateOutputBytes {
		t.Fatalf("encoded completion size = %d, max %d", len(text.Text), maxDelegateOutputBytes)
	}
	var decoded backgroundCompletion
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatalf("bounded completion is invalid JSON: %v", err)
	}
}
