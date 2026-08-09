package sessionruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/delegationtool"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

type nativeMessageAgentFixture struct {
	session    *Session
	controller *scopedController
	childID    uuid.UUID
	child      *channelBackend
	parent     *channelBackend
	appender   *fakeCommandAppender
	sub        *fakeSubscription
}

func newNativeMessageAgentFixture(t *testing.T) nativeMessageAgentFixture {
	t.Helper()
	parentID, childID := mustUUID(), mustUUID()
	parent := &channelBackend{Commands: make(chan command.Command, 4), Done: make(chan struct{})}
	child := &channelBackend{Commands: make(chan command.Command, 4), Done: make(chan struct{})}
	appender := &fakeCommandAppender{}

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	childBound := bindCfg(delegateChild("worker", "native answer"), mustUUID(), childID)
	sessionID := mustUUID()
	s := &Session{
		sessionID:     sessionID,
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		hub:           hub.New(sessionID),
		newID:         uuid.New,
		cmdAppender:   appender,
		loops: map[uuid.UUID]*loopHandle{
			parentID: {id: parentID, backend: parent},
			childID: {
				id:        childID,
				backend:   child,
				bound:     childBound,
				parent:    loop.Provenance{LoopID: parentID},
				agentName: "worker",
				state:     tool.DelegateStatusRunning,
			},
		},
	}
	manager := newDelegationManager(Topology{})
	manager.attach(s)
	sub := newFakeSubscription(8)
	fixture := nativeMessageAgentFixture{
		session:    s,
		controller: &scopedController{manager: manager, parentLoopID: parentID, style: loop.DelegationManaged},
		childID:    childID,
		child:      child,
		parent:     parent,
		appender:   appender,
		sub:        sub,
	}
	s.delegateSubscribe = func(event.EventFilter) (event.Subscription, error) {
		return sub, nil
	}
	t.Cleanup(func() {
		sessionCancel()
		close(fixture.child.Done)
		close(fixture.parent.Done)
	})
	return fixture
}

func messageAgentText(t *testing.T, result *tool.ToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("MessageAgent result = %#v, want one text block", result)
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("MessageAgent result content = %T, want *content.TextBlock", result.Content[0])
	}
	return block.Text
}

func TestMessageAgentNativeBusyNonWaitingReturnsAcceptedPendingAndDurableHandback(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"steer","wait_for_response":false}`
	request, artifact, err := messageAgent.PrepareCall(context.Background(), mustUUID(), args)
	if err != nil {
		t.Fatalf("MessageAgent PrepareCall: %v", err)
	}
	resultCh := make(chan struct {
		result *tool.ToolResult
		err    error
	}, 1)
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{Request: request, Artifact: artifact})
	go func() {
		result, runErr := messageAgent.InvokableRun(ctx, args)
		resultCh <- struct {
			result *tool.ToolResult
			err    error
		}{result: result, err: runErr}
	}()

	cmd, ok := (<-fixture.child.Commands).(command.UserInput)
	if !ok {
		t.Fatalf("native MessageAgent command = %T, want command.UserInput", cmd)
	}
	cmd.Accepted <- nil
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("MessageAgent InvokableRun: %v", call.err)
	}
	initial := fixture.appender.snapshot()
	if len(initial) != 1 {
		t.Fatalf("delegate intent records = %d, want 1 before hand-back", len(initial))
	}

	// Complete the session-owned hand-back so this test observes the full path,
	// not merely the immediate tool envelope.
	turnID := mustUUID()
	fixture.sub.feed(event.TurnStarted{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: cmd.CommandID},
	}})
	fixture.sub.feed(event.TurnDone{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID},
	}, Message: aiMessage("native answer")})

	deadline := time.Now().Add(time.Second)
	var records []journal.CommandRecord
	for len(records) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		records = fixture.appender.snapshot()
	}
	if len(records) < 2 {
		t.Fatalf("durable hand-back records = %d, want intent plus SubagentResult", len(records))
	}
	intent, ok := initial[0].Command().(command.UserInput)
	if !ok {
		t.Fatalf("durable delegate intent = %T, want command.UserInput", records[0].Command())
	}
	if intent.NoFold {
		t.Fatalf("native MessageAgent intent has NoFold=true, want foldable NoFold=false")
	}
	if !intent.BackgroundHandBack {
		t.Fatal("native MessageAgent intent lost BackgroundHandBack durable hand-back marker")
	}
	if intent.DelegateDeliveryPhase != command.DelegateDeliveryPhaseIntent {
		t.Fatalf("native MessageAgent delivery phase = %q, want %q", intent.DelegateDeliveryPhase, command.DelegateDeliveryPhaseIntent)
	}
	handBack, ok := records[1].Command().(command.SubagentResult)
	if !ok {
		t.Fatalf("durable hand-back = %T, want command.SubagentResult", records[1].Command())
	}
	completion, ok := decodeBackgroundCompletion(handBack.Blocks)
	if !ok || completion.CorrelationID != cmd.CommandID.String() || completion.Response != "native answer" {
		t.Fatalf("durable hand-back envelope = %+v, %v; want request=%v answer=native answer", completion, ok, cmd.CommandID)
	}
	var got struct {
		DeliveryStatus string `json:"delivery_status"`
	}
	if err := json.Unmarshal([]byte(messageAgentText(t, call.result)), &got); err != nil {
		t.Fatalf("MessageAgent result JSON: %v", err)
	}
	if got.DeliveryStatus != string(tool.DelegateDeliveryAcceptedPending) {
		t.Fatalf("MessageAgent non-waiting delivery_status = %q, want %q", got.DeliveryStatus, tool.DelegateDeliveryAcceptedPending)
	}
}

func TestMessageAgentCallerCancellationAfterAcceptanceEmitsNoRetraction(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"steer","wait_for_response":true}`
	request, artifact, err := messageAgent.PrepareCall(context.Background(), mustUUID(), args)
	if err != nil {
		t.Fatalf("MessageAgent PrepareCall: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prepared := loop.WithPreparedCall(ctx, tool.PreparedCall{Request: request, Artifact: artifact})
	resultCh := make(chan error, 1)
	go func() {
		_, runErr := messageAgent.InvokableRun(prepared, args)
		resultCh <- runErr
	}()

	cmd, ok := (<-fixture.child.Commands).(command.UserInput)
	if !ok {
		t.Fatalf("native MessageAgent command = %T, want command.UserInput", cmd)
	}
	cmd.Accepted <- nil
	cancel()
	select {
	case runErr := <-resultCh:
		if runErr != nil {
			t.Fatalf("MessageAgent InvokableRun after caller cancel: %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("MessageAgent did not return after caller cancellation")
	}

	select {
	case extra := <-fixture.child.Commands:
		if extra != nil {
			t.Fatalf("caller cancellation after acceptance emitted %T (%+v), want no cancel/interrupt/retraction", extra, extra)
		}
	case <-time.After(100 * time.Millisecond):
	}
	for _, record := range fixture.appender.snapshot() {
		if _, retract := record.Command().(command.CancelDelegateRequest); retract {
			t.Fatal("caller cancellation after acceptance persisted CancelDelegateRequest")
		}
		if _, retract := record.Command().(command.CancelQueuedInput); retract {
			t.Fatal("caller cancellation after acceptance persisted CancelQueuedInput")
		}
	}
}

func TestMessageAgentCallerTimeoutAfterAcceptanceEmitsNoInterrupt(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"steer","wait_for_response":true,"timeout_seconds":0}`
	request, artifact, err := messageAgent.PrepareCall(context.Background(), mustUUID(), args)
	if err != nil {
		t.Fatalf("MessageAgent PrepareCall: %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{Request: request, Artifact: artifact})
	resultCh := make(chan struct {
		result *tool.ToolResult
		err    error
	}, 1)
	go func() {
		result, runErr := messageAgent.InvokableRun(ctx, args)
		resultCh <- struct {
			result *tool.ToolResult
			err    error
		}{result: result, err: runErr}
	}()

	cmd, ok := (<-fixture.child.Commands).(command.UserInput)
	if !ok {
		t.Fatalf("native MessageAgent command = %T, want command.UserInput", cmd)
	}
	cmd.Accepted <- nil
	select {
	case call := <-resultCh:
		if call.err != nil {
			t.Fatalf("MessageAgent InvokableRun after timeout: %v", call.err)
		}
		if got := messageAgentText(t, call.result); got != "error: agent timed out" && got != "error: agent interrupted" {
			t.Fatalf("timeout result = %q, want timed-out/interrupted result", got)
		}
	case <-time.After(time.Second):
		t.Fatal("MessageAgent did not return after caller timeout")
	}

	select {
	case extra := <-fixture.child.Commands:
		if extra != nil {
			t.Fatalf("caller timeout after acceptance emitted %T (%+v), want no interrupt/retraction", extra, extra)
		}
	case <-time.After(100 * time.Millisecond):
	}
	for _, record := range fixture.appender.snapshot() {
		if _, interrupt := record.Command().(command.CancelDelegateRequest); interrupt {
			t.Fatal("caller timeout after acceptance persisted CancelDelegateRequest")
		}
		if _, interrupt := record.Command().(command.CancelQueuedInput); interrupt {
			t.Fatal("caller timeout after acceptance persisted CancelQueuedInput")
		}
	}
}

func TestMessageAgentWaitAcceptsFoldedOpeningAndExactTerminal(t *testing.T) {
	requestID, turnID, loopID, otherLoop := mustUUID(), mustUUID(), mustUUID(), mustUUID()
	sub := newFakeSubscription(8)
	sub.feed(event.TurnFoldedInto{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: loopID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: requestID},
	}})
	// A terminal from another loop or another turn is not the terminal of this
	// request, even when it appears immediately after the folded opening.
	sub.feed(event.TurnDone{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: otherLoop, TurnID: turnID},
	}, Message: aiMessage("wrong loop")})
	sub.feed(event.TurnDone{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: loopID, TurnID: mustUUID()},
	}, Message: aiMessage("wrong turn")})
	sub.feed(event.TurnDone{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: loopID, TurnID: turnID},
	}, Message: aiMessage("folded answer")})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	text, err := drainDelegateAnswerObserved(ctx, sub, requestID, func() {}, nil)
	if err != nil {
		t.Fatalf("drainDelegateAnswerObserved: %v", err)
	}
	if text != "folded answer" {
		t.Fatalf("folded MessageAgent answer = %q, want %q", text, "folded answer")
	}
}
