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

func messageAgentErrorText(t *testing.T, result *tool.ToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("MessageAgent result = %#v, want one error block", result)
	}
	outer, ok := result.Content[0].(*content.ToolResultBlock)
	if !ok || !outer.IsError || len(outer.Content) != 1 {
		t.Fatalf("MessageAgent result content = %#v, want one error ToolResultBlock", result.Content)
	}
	text, ok := outer.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("MessageAgent error content = %T, want *content.TextBlock", outer.Content[0])
	}
	return text.Text
}

func nativeListAgentState(t *testing.T, fixture nativeMessageAgentFixture) tool.AgentState {
	t.Helper()
	listAgents := delegationtool.NewListAgents(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `"}`
	request, artifact, err := listAgents.PrepareCall(context.Background(), mustUUID(), args)
	if err != nil {
		t.Fatalf("ListAgents PrepareCall: %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{Request: request, Artifact: artifact})
	result, err := listAgents.InvokableRun(ctx, args)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	var listed struct {
		Agents []struct {
			State tool.AgentState `json:"state"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(messageAgentText(t, result)), &listed); err != nil {
		t.Fatalf("ListAgents result JSON: %v", err)
	}
	if len(listed.Agents) != 1 {
		t.Fatalf("ListAgents agents = %+v, want one child", listed.Agents)
	}
	return listed.Agents[0].State
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
	var delivery struct {
		DeliveryStatus string `json:"delivery_status"`
	}
	block, ok := handBack.Blocks[0].(*content.TextBlock)
	if !ok || json.Unmarshal([]byte(block.Text), &delivery) != nil {
		t.Fatalf("durable hand-back delivery envelope = %#v, want JSON", handBack.Blocks)
	}
	if delivery.DeliveryStatus != string(tool.DelegateDeliveryQueued) {
		t.Fatalf("TurnStarted background delivery_status = %q, want %q", delivery.DeliveryStatus, tool.DelegateDeliveryQueued)
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

func TestMessageAgentNativeForegroundIntentIsDurableAndPhased(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"foreground","wait_for_response":true}`
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
		t.Fatalf("native foreground MessageAgent command = %T, want command.UserInput", cmd)
	}
	initial := fixture.appender.snapshot()
	if len(initial) != 1 {
		t.Fatalf("foreground durable intent records = %d, want 1 before acceptance", len(initial))
	}
	intent, ok := initial[0].Command().(command.UserInput)
	if !ok {
		t.Fatalf("foreground durable intent = %T, want command.UserInput", initial[0].Command())
	}
	if intent.CommandID != cmd.CommandID {
		t.Fatalf("foreground durable intent id = %v, want dispatched id %v", intent.CommandID, cmd.CommandID)
	}
	if intent.DelegateDeliveryPhase != command.DelegateDeliveryPhaseIntent {
		t.Fatalf("foreground native delivery phase = %q, want %q", intent.DelegateDeliveryPhase, command.DelegateDeliveryPhaseIntent)
	}

	cmd.Accepted <- nil
	turnID := mustUUID()
	fixture.sub.feed(event.TurnStarted{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: cmd.CommandID},
	}})
	fixture.sub.feed(event.TurnDone{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID},
	}, Message: aiMessage("foreground answer")})
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("MessageAgent foreground InvokableRun: %v", call.err)
	}
	var got struct {
		DeliveryStatus string `json:"delivery_status"`
	}
	if err := json.Unmarshal([]byte(messageAgentText(t, call.result)), &got); err != nil {
		t.Fatalf("foreground MessageAgent result JSON: %v", err)
	}
	if got.DeliveryStatus != string(tool.DelegateDeliveryQueued) {
		t.Fatalf("foreground TurnStarted delivery_status = %q, want %q", got.DeliveryStatus, tool.DelegateDeliveryQueued)
	}
}

func TestMessageAgentNativeForegroundFoldedReportsInjectedDisposition(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"folded foreground","wait_for_response":true}`
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
		t.Fatalf("native folded foreground command = %T, want command.UserInput", cmd)
	}
	cmd.Accepted <- nil
	turnID := mustUUID()
	fixture.sub.feed(event.TurnFoldedInto{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: cmd.CommandID},
	}})
	fixture.sub.feed(event.TurnDone{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID},
	}, Message: aiMessage("folded foreground answer")})
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("MessageAgent folded foreground InvokableRun: %v", call.err)
	}
	var got struct {
		DeliveryStatus string `json:"delivery_status"`
	}
	if err := json.Unmarshal([]byte(messageAgentText(t, call.result)), &got); err != nil {
		t.Fatalf("folded foreground MessageAgent result JSON: %v", err)
	}
	if got.DeliveryStatus != string(tool.DelegateDeliveryInjected) {
		t.Fatalf("foreground TurnFoldedInto delivery_status = %q, want %q", got.DeliveryStatus, tool.DelegateDeliveryInjected)
	}
}

func TestMessageAgentNativeForegroundTimeoutReturnsStructuredFailure(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"timeout foreground","wait_for_response":true,"timeout_seconds":1}`
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
		t.Fatalf("native timeout foreground command = %T, want command.UserInput", cmd)
	}
	cmd.Accepted <- nil
	turnID := mustUUID()
	fixture.sub.feed(event.TurnStarted{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: cmd.CommandID},
	}})
	select {
	case call := <-resultCh:
		if call.err != nil {
			t.Fatalf("MessageAgent timeout foreground InvokableRun: %v", call.err)
		}
		if got := messageAgentErrorText(t, call.result); got != "MessageAgent failed: agent timed out" {
			t.Fatalf("timeout foreground result = %q, want structured timeout failure", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout foreground MessageAgent did not return")
	}
}

func TestMessageAgentNativeTimeoutBeforeOpeningReturnsStructuredFailure(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"timeout before opening","wait_for_response":true,"timeout_seconds":0}`
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
		t.Fatalf("native timeout-before-opening command = %T, want command.UserInput", cmd)
	}
	cmd.Accepted <- nil
	select {
	case call := <-resultCh:
		if call.err != nil {
			t.Fatalf("MessageAgent timeout-before-opening InvokableRun: %v", call.err)
		}
		if got := messageAgentErrorText(t, call.result); got != "MessageAgent failed: agent timed out" {
			t.Fatalf("timeout-before-opening result = %q, want structured timeout failure", got)
		}
	case <-time.After(time.Second):
		t.Fatal("MessageAgent timeout-before-opening did not return")
	}
}

func TestMessageAgentNativeCancellationBeforeOpeningReturnsStructuredFailure(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"cancel before opening","wait_for_response":true}`
	request, artifact, err := messageAgent.PrepareCall(context.Background(), mustUUID(), args)
	if err != nil {
		t.Fatalf("MessageAgent PrepareCall: %v", err)
	}
	callerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := loop.WithPreparedCall(callerCtx, tool.PreparedCall{Request: request, Artifact: artifact})
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
		t.Fatalf("native cancellation-before-opening command = %T, want command.UserInput", cmd)
	}
	cmd.Accepted <- nil
	cancel()
	select {
	case call := <-resultCh:
		if call.err != nil {
			t.Fatalf("MessageAgent cancellation-before-opening InvokableRun: %v", call.err)
		}
		if got := messageAgentErrorText(t, call.result); got != "MessageAgent failed: agent interrupted" {
			t.Fatalf("cancellation-before-opening result = %q, want structured interruption failure", got)
		}
	case <-time.After(time.Second):
		t.Fatal("MessageAgent cancellation-before-opening did not return")
	}
}

func TestMessageAgentNativeBackgroundFoldedHandbackRetainsInjectedDisposition(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"folded background","wait_for_response":false}`
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
		t.Fatalf("native folded background command = %T, want command.UserInput", cmd)
	}
	cmd.Accepted <- nil
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("MessageAgent folded background InvokableRun: %v", call.err)
	}
	turnID := mustUUID()
	fixture.sub.feed(event.TurnFoldedInto{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: cmd.CommandID},
	}})
	fixture.sub.feed(event.TurnDone{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID},
	}, Message: aiMessage("folded background answer")})

	deadline := time.Now().Add(time.Second)
	var records []journal.CommandRecord
	for len(records) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		records = fixture.appender.snapshot()
	}
	if len(records) < 2 {
		t.Fatalf("folded background records = %d, want intent plus hand-back", len(records))
	}
	handBack, ok := records[1].Command().(command.SubagentResult)
	if !ok {
		t.Fatalf("folded background hand-back = %T, want command.SubagentResult", records[1].Command())
	}
	completion, ok := decodeBackgroundCompletion(handBack.Blocks)
	if !ok || completion.Response != "folded background answer" || completion.DeliveryStatus != tool.DelegateDeliveryInjected {
		t.Fatalf("folded background completion = %+v, %v; want injected answer", completion, ok)
	}
}

func TestMessageAgentNativeBackgroundTimeoutBeforeOpeningPreservesPendingObservation(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"background timeout before opening","wait_for_response":false,"timeout_seconds":0}`
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
		t.Fatalf("native background timeout-before-opening command = %T, want command.UserInput", cmd)
	}
	cmd.Accepted <- nil
	select {
	case call := <-resultCh:
		if call.err != nil {
			t.Fatalf("MessageAgent background timeout-before-opening InvokableRun: %v", call.err)
		}
	case <-time.After(time.Second):
		t.Fatal("MessageAgent background timeout-before-opening did not return")
	}

	select {
	case raw := <-fixture.parent.Commands:
		handBack, ok := raw.(command.SubagentResult)
		if !ok {
			t.Fatalf("background timeout-before-opening command = %T, want command.SubagentResult", raw)
		}
		block, ok := handBack.Blocks[0].(*content.TextBlock)
		if !ok {
			t.Fatalf("background timeout-before-opening block = %T, want *content.TextBlock", handBack.Blocks[0])
		}
		var got struct {
			State          string                      `json:"state"`
			DeliveryStatus string                      `json:"delivery_status"`
			ResponseStatus tool.DelegateResponseStatus `json:"response_status"`
		}
		if err := json.Unmarshal([]byte(block.Text), &got); err != nil {
			t.Fatalf("background timeout-before-opening JSON: %v", err)
		}
		if got.State != string(tool.AgentStateWorking) || got.DeliveryStatus != string(tool.DelegateDeliveryAcceptedPending) || got.ResponseStatus != tool.DelegateResponseTimedOut {
			t.Fatalf("background timeout-before-opening result = %+v, want working/accepted_pending/timed_out", got)
		}
	case <-time.After(time.Second):
		t.Fatal("background timeout-before-opening hand-back did not arrive")
	}
}

func TestMessageAgentForegroundObserverExpiryKeepsListAgentsWorkingUntilTerminal(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	fixture.session.loopsMu.Lock()
	fixture.session.loops[fixture.childID].setMechanicalState(tool.DelegateStatusIdle)
	fixture.session.loopsMu.Unlock()
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"foreground status","wait_for_response":true,"timeout_seconds":0}`
	request, artifact, err := messageAgent.PrepareCall(context.Background(), mustUUID(), args)
	if err != nil {
		t.Fatalf("MessageAgent PrepareCall: %v", err)
	}
	resultCh := make(chan error, 1)
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{Request: request, Artifact: artifact})
	go func() {
		_, runErr := messageAgent.InvokableRun(ctx, args)
		resultCh <- runErr
	}()
	cmd, ok := (<-fixture.child.Commands).(command.UserInput)
	if !ok {
		t.Fatalf("foreground status command = %T, want command.UserInput", cmd)
	}
	cmd.Accepted <- nil
	select {
	case runErr := <-resultCh:
		if runErr != nil {
			t.Fatalf("foreground status MessageAgent: %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("foreground status MessageAgent did not return")
	}
	status, err := fixture.controller.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: fixture.childID})
	if err != nil {
		t.Fatalf("foreground status: %v", err)
	}
	if len(status.Agents) != 1 || status.Agents[0].State != tool.AgentStateWorking {
		t.Fatalf("foreground status after observer expiry = %+v, want working", status.Agents)
	}
	if state := nativeListAgentState(t, fixture); state != tool.AgentStateWorking {
		t.Fatalf("ListAgents after foreground observer expiry = %s, want working", state)
	}
	turnID := mustUUID()
	fixture.sub.feed(event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID}, Cause: identity.Cause{CommandID: cmd.CommandID}}})
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID}}, Message: aiMessage("done")})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status, err = fixture.controller.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: fixture.childID})
		if err == nil && len(status.Agents) == 1 && status.Agents[0].State == tool.AgentStateIdle {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("foreground status after terminal = %+v, want idle", status.Agents)
}

func TestMessageAgentBackgroundObserverExpiryKeepsListAgentsWorkingUntilTerminal(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	fixture.session.loopsMu.Lock()
	fixture.session.loops[fixture.childID].setMechanicalState(tool.DelegateStatusIdle)
	fixture.session.loopsMu.Unlock()
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"background status","wait_for_response":false,"timeout_seconds":0}`
	request, artifact, err := messageAgent.PrepareCall(context.Background(), mustUUID(), args)
	if err != nil {
		t.Fatalf("MessageAgent PrepareCall: %v", err)
	}
	resultCh := make(chan error, 1)
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{Request: request, Artifact: artifact})
	go func() {
		_, runErr := messageAgent.InvokableRun(ctx, args)
		resultCh <- runErr
	}()
	cmd, ok := (<-fixture.child.Commands).(command.UserInput)
	if !ok {
		t.Fatalf("background status command = %T, want command.UserInput", cmd)
	}
	cmd.Accepted <- nil
	select {
	case runErr := <-resultCh:
		if runErr != nil {
			t.Fatalf("background status MessageAgent: %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("background status MessageAgent did not return")
	}
	select {
	case <-fixture.parent.Commands:
	case <-time.After(time.Second):
		t.Fatal("background status timeout hand-back did not arrive")
	}
	status, err := fixture.controller.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: fixture.childID})
	if err != nil {
		t.Fatalf("background status: %v", err)
	}
	if len(status.Agents) != 1 || status.Agents[0].State != tool.AgentStateWorking {
		t.Fatalf("background status after observer expiry = %+v, want working", status.Agents)
	}
	turnID := mustUUID()
	fixture.sub.feed(event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID}, Cause: identity.Cause{CommandID: cmd.CommandID}}})
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: fixture.childID, TurnID: turnID}}, Message: aiMessage("done")})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status, err = fixture.controller.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: fixture.childID})
		if err == nil && len(status.Agents) == 1 && status.Agents[0].State == tool.AgentStateIdle {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("background status after terminal = %+v, want idle", status.Agents)
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

func TestMessageAgentCallerCancellationAfterIntentBeforeDispatchStillDelivers(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	fixture.child.Commands = make(chan command.Command)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"cancel before send","wait_for_response":true}`
	request, artifact, err := messageAgent.PrepareCall(context.Background(), mustUUID(), args)
	if err != nil {
		t.Fatalf("MessageAgent PrepareCall: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	prepared := loop.WithPreparedCall(ctx, tool.PreparedCall{Request: request, Artifact: artifact})
	resultCh := make(chan error, 1)
	go func() {
		_, runErr := messageAgent.InvokableRun(prepared, args)
		resultCh <- runErr
	}()

	deadline := time.Now().Add(time.Second)
	for len(fixture.appender.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(fixture.appender.snapshot()) != 1 {
		t.Fatal("durable intent was not appended before cancellation")
	}
	cancel()
	select {
	case raw := <-fixture.child.Commands:
		cmd, ok := raw.(command.UserInput)
		if !ok {
			t.Fatalf("delivered command = %T, want command.UserInput", raw)
		}
		cmd.Accepted <- nil
	case <-time.After(time.Second):
		t.Fatal("caller cancellation after intent prevented eventual delivery")
	}
	select {
	case <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("MessageAgent did not finish after eventual delivery")
	}
	for _, record := range fixture.appender.snapshot() {
		switch record.Command().(type) {
		case command.CancelDelegateRequest, command.CancelQueuedInput:
			t.Fatalf("caller cancellation emitted control command %T", record.Command())
		}
	}
}

func TestMessageAgentCallerCancellationAfterDispatchBeforeAcceptanceDoesNotRetract(t *testing.T) {
	fixture := newNativeMessageAgentFixture(t)
	messageAgent := delegationtool.NewMessageAgent(fixture.controller, loop.DelegationManaged, nil)
	args := `{"agent_id":"` + fixture.childID.String() + `","message":"cancel before ack","wait_for_response":true}`
	request, artifact, err := messageAgent.PrepareCall(context.Background(), mustUUID(), args)
	if err != nil {
		t.Fatalf("MessageAgent PrepareCall: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	prepared := loop.WithPreparedCall(ctx, tool.PreparedCall{Request: request, Artifact: artifact})
	resultCh := make(chan error, 1)
	go func() {
		_, runErr := messageAgent.InvokableRun(prepared, args)
		resultCh <- runErr
	}()

	raw := <-fixture.child.Commands
	cmd, ok := raw.(command.UserInput)
	if !ok {
		t.Fatalf("delivered command = %T, want command.UserInput", raw)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	cmd.Accepted <- nil
	select {
	case <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("MessageAgent did not finish after post-dispatch cancellation")
	}
	select {
	case extra := <-fixture.child.Commands:
		if extra != nil {
			t.Fatalf("post-dispatch cancellation emitted %T (%+v)", extra, extra)
		}
	case <-time.After(100 * time.Millisecond):
	}
	for _, record := range fixture.appender.snapshot() {
		switch record.Command().(type) {
		case command.CancelDelegateRequest, command.CancelQueuedInput:
			t.Fatalf("post-dispatch cancellation persisted control command %T", record.Command())
		}
	}
}

func TestMessageAgentCallerTimeoutAfterAcceptanceReturnsFailureWithoutInterrupt(t *testing.T) {
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
		if got := messageAgentErrorText(t, call.result); got != "MessageAgent failed: agent timed out" {
			t.Fatalf("timeout result = %q, want structured timeout failure", got)
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
