package sessionruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

type foreignMessageAgentFixture struct {
	session    *Session
	controller *scopedController
	childID    uuid.UUID
	child      *channelBackend
	parent     *channelBackend
	hook       *foreignDeliveryHook
	commands   *foreignDeliveryCommandAppender
	events     *foreignDeliveryEventAppender
	sub        *fakeSubscription
}

type manualForeignDeliveryTimer struct {
	ch chan time.Time
}

func newManualForeignDeliveryTimer() *manualForeignDeliveryTimer {
	return &manualForeignDeliveryTimer{ch: make(chan time.Time, 1)}
}

func (t *manualForeignDeliveryTimer) Chan() <-chan time.Time { return t.ch }
func (t *manualForeignDeliveryTimer) Stop() bool             { return true }
func (t *manualForeignDeliveryTimer) fire()                  { t.ch <- time.Time{} }

func newForeignMessageAgentFixture(t *testing.T) foreignMessageAgentFixture {
	t.Helper()
	parentID, childID, sessionID := mustUUID(), mustUUID(), mustUUID()
	parent := &channelBackend{Commands: make(chan command.Command, 8), Done: make(chan struct{})}
	child := &channelBackend{Commands: make(chan command.Command, 8), Done: make(chan struct{})}
	commands := &foreignDeliveryCommandAppender{}
	events := &foreignDeliveryEventAppender{}
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	childBound := bindCfg(engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("foreign")}}, loop.EngineForeignClaude, "foreign"), sessionID, childID)
	s := &Session{
		sessionID:     sessionID,
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		hub:           hub.New(sessionID, hub.WithAppender(events)),
		newID:         uuid.New,
		now:           time.Now,
		factory:       event.NewFactory(uuid.New, time.Now),
		cmdAppender:   commands,
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
	sub := newFakeSubscription(16)
	s.delegateSubscribe = func(event.EventFilter) (event.Subscription, error) { return sub, nil }
	hook := newForeignDeliveryHook(s, childID)
	fixture := foreignMessageAgentFixture{
		session: s, controller: &scopedController{manager: manager, parentLoopID: parentID, style: loop.DelegationManaged},
		childID: childID, child: child, parent: parent, hook: hook, commands: commands, events: events, sub: sub,
	}
	t.Cleanup(func() {
		sessionCancel()
		close(child.Done)
		close(parent.Done)
	})
	return fixture
}

func foreignMessageRequest(childID uuid.UUID, wait bool) tool.DelegateRequest {
	return tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: childID, Message: "steer", WaitForResponse: wait}
}

func requestTrackerPresent(manager *delegationManager, requestID uuid.UUID) bool {
	if manager == nil {
		return false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	_, ok := manager.requests[requestID]
	return ok
}

func waitForRequestTracker(t *testing.T, manager *delegationManager, requestID uuid.UUID, want bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if requestTrackerPresent(manager, requestID) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("request tracker present = %v, want %v", requestTrackerPresent(manager, requestID), want)
}

func assertNoForeignCancellationCommands(t *testing.T, fixture foreignMessageAgentFixture) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, record := range fixture.commands.snapshot() {
			switch record.Command().(type) {
			case command.CancelDelegateRequest, command.CancelQueuedInput:
				t.Fatalf("observer timeout persisted %T", record.Command())
			}
		}
		select {
		case raw := <-fixture.child.Commands:
			switch raw.(type) {
			case command.CancelDelegateRequest, command.CancelQueuedInput:
				t.Fatalf("observer timeout dispatched %T", raw)
			default:
				t.Fatalf("unexpected child command after admission: %T", raw)
			}
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func assertNoParentCommand(t *testing.T, parent *channelBackend, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case raw := <-parent.Commands:
		t.Fatalf("unexpected second parent command: %T", raw)
	case <-timer.C:
	}
}

func foreignTurnEvent(fixture foreignMessageAgentFixture, cmdID, turnID uuid.UUID, folded bool) event.Event {
	header := event.Header{Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID}, Cause: identity.Cause{CommandID: cmdID}}
	if folded {
		return event.TurnFoldedInto{Header: header}
	}
	return event.TurnStarted{Header: header}
}

func TestMessageAgentForeignQueuedWaitReportsQueuedDeliveryAndResponse(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(context.Background(), foreignMessageRequest(fixture.childID, true))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	if err := fixture.hook.QueueFallback(context.Background(), foreignFallbackIntent(cmd.CommandID, fixture.childID)); err != nil {
		t.Fatalf("QueueFallback: %v", err)
	}
	cmd.Accepted <- nil
	turnID := mustUUID()
	fixture.sub.feed(foreignTurnEvent(fixture, cmd.CommandID, turnID, false))
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID}}, Message: aiMessage("queued answer")})
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("Execute: %v", call.err)
	}
	if call.result.DeliveryStatus != tool.DelegateDeliveryQueued || call.result.ResponseStatus != tool.DelegateResponseCompleted || call.result.Response != "queued answer" {
		t.Fatalf("result = %+v, want queued/completed/queued answer", call.result)
	}
}

func TestMessageAgentForeignQueuedForegroundOwnsSubscriptionBeforeObserverBound(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	timer := newManualForeignDeliveryTimer()
	fixture.controller.manager.foreignDeliveryTimerFactory = func(time.Duration) foreignDeliveryObserverTimer { return timer }
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(context.Background(), foreignMessageRequest(fixture.childID, true))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	if err := fixture.hook.QueueFallback(context.Background(), foreignFallbackIntent(cmd.CommandID, fixture.childID)); err != nil {
		t.Fatalf("QueueFallback: %v", err)
	}
	cmd.Accepted <- nil
	turnID := mustUUID()
	fixture.sub.feed(foreignTurnEvent(fixture, cmd.CommandID, turnID, false))
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID}}, Message: aiMessage("attached answer")})
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("Execute: %v", call.err)
	}
	if call.result.DeliveryStatus != tool.DelegateDeliveryQueued || call.result.ResponseStatus != tool.DelegateResponseCompleted || call.result.Response != "attached answer" {
		t.Fatalf("attached result = %+v, want queued/completed/attached answer", call.result)
	}
	select {
	case extra := <-fixture.parent.Commands:
		t.Fatalf("foreground queued request created handback: %T", extra)
	default:
	}
	// The observer timer was never allowed to synthesize a second update or
	// consume the target subscription after the foreground drain completed.
	timer.fire()
}

func TestMessageAgentForeignInjectedWaitReportsInjectedDeliveryAndResponse(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(context.Background(), foreignMessageRequest(fixture.childID, true))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	if err := fixture.hook.Reserve(context.Background(), foreign.DeliveryReservation{LoopID: fixture.childID, RequestID: cmd.CommandID}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	turnID := mustUUID()
	fixture.hook.observeFold(event.TurnFoldedInto{Header: event.Header{Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID}, Cause: identity.Cause{CommandID: cmd.CommandID}}})
	if err := fixture.hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: fixture.childID, RequestID: cmd.CommandID, TurnID: turnID, State: foreign.DeliveryResolutionInjected}); err != nil {
		t.Fatalf("Resolve injected: %v", err)
	}
	cmd.Accepted <- nil
	fixture.sub.feed(foreignTurnEvent(fixture, cmd.CommandID, turnID, true))
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID}}, Message: aiMessage("injected answer")})
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("Execute: %v", call.err)
	}
	if call.result.DeliveryStatus != tool.DelegateDeliveryInjected || call.result.ResponseStatus != tool.DelegateResponseCompleted || call.result.Response != "injected answer" {
		t.Fatalf("result = %+v, want injected/completed/injected answer", call.result)
	}
}

func TestMessageAgentForeignIdleAdmissionReportsQueuedDelivery(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(context.Background(), foreignMessageRequest(fixture.childID, false))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	cmd.Accepted <- nil
	turnID := mustUUID()
	fixture.session.recordForeignDeliveryFold(foreignTurnEvent(fixture, cmd.CommandID, turnID, false))
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("Execute: %v", call.err)
	}
	if call.result.DeliveryStatus != tool.DelegateDeliveryQueued || call.result.ResponseStatus != tool.DelegateResponseUnknown {
		t.Fatalf("result = %+v, want queued with unknown response", call.result)
	}
	fixture.sub.feed(foreignTurnEvent(fixture, cmd.CommandID, turnID, false))
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID}}, Message: aiMessage("idle answer")})
	rawParent := <-fixture.parent.Commands
	handBack, ok := rawParent.(command.SubagentResult)
	if !ok {
		t.Fatalf("idle handback = %T, want command.SubagentResult", rawParent)
	}
	completion, ok := decodeBackgroundCompletion(handBack.Blocks)
	if !ok || completion.DeliveryStatus != tool.DelegateDeliveryQueued || completion.ResponseStatus != tool.DelegateResponseCompleted || completion.Response != "idle answer" {
		t.Fatalf("idle completion = %+v/%v, want queued/completed/idle answer", completion, ok)
	}
}

func TestMessageAgentForeignObserverBoundReturnsPendingWithoutUnknown(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	timer := newManualForeignDeliveryTimer()
	fixture.controller.manager.foreignDeliveryTimerFactory = func(time.Duration) foreignDeliveryObserverTimer { return timer }
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(context.Background(), foreignMessageRequest(fixture.childID, false))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	cmd.Accepted <- nil
	timer.fire()
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("Execute: %v", call.err)
	}
	if call.result.DeliveryStatus != tool.DelegateDeliveryAcceptedPending || call.result.ResponseStatus != tool.DelegateResponseUnknown || call.result.CorrelationID != cmd.CommandID {
		t.Fatalf("observer-bound result = %+v, want accepted_pending/unknown correlation=%v", call.result, cmd.CommandID)
	}
	if got := fixture.hook.deliveryStatus(cmd.CommandID); got != "" {
		t.Fatalf("observer bound changed hook status to %q, want no concrete phase", got)
	}
	if err := fixture.hook.QueueFallback(context.Background(), foreignFallbackIntent(cmd.CommandID, fixture.childID)); err != nil {
		t.Fatalf("QueueFallback: %v", err)
	}
	turnID := mustUUID()
	fixture.sub.feed(foreignTurnEvent(fixture, cmd.CommandID, turnID, false))
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID}}, Message: aiMessage("bound answer")})
	rawParent := <-fixture.parent.Commands
	completion, ok := decodeBackgroundCompletion(rawParent.(command.SubagentResult).Blocks)
	if !ok || completion.DeliveryStatus != tool.DelegateDeliveryQueued || completion.ResponseStatus != tool.DelegateResponseCompleted {
		t.Fatalf("observer-bound completion = %+v/%v, want queued/completed", completion, ok)
	}
	waitForRequestTracker(t, fixture.controller.manager, cmd.CommandID, false)
}

func TestMessageAgentForeignBackgroundTimeoutBeforeDecisionHandsBackOnce(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	zero := 0
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		req := foreignMessageRequest(fixture.childID, false)
		req.TimeoutSeconds = &zero
		result, err := fixture.controller.Execute(context.Background(), req)
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	cmd.Accepted <- nil
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("Execute: %v", call.err)
	}
	if call.result.DeliveryStatus != tool.DelegateDeliveryAcceptedPending || call.result.ResponseStatus != tool.DelegateResponseUnknown || call.result.CorrelationID != cmd.CommandID {
		t.Fatalf("predecision timeout result = %+v, want accepted_pending/unknown correlation=%v", call.result, cmd.CommandID)
	}
	rawParent := <-fixture.parent.Commands
	handBack, ok := rawParent.(command.SubagentResult)
	if !ok {
		t.Fatalf("predecision timeout handback = %T, want command.SubagentResult", rawParent)
	}
	completion, ok := decodeBackgroundCompletion(handBack.Blocks)
	if !ok || completion.State != tool.AgentStateWorking || completion.DeliveryStatus != tool.DelegateDeliveryAcceptedPending || completion.ResponseStatus != tool.DelegateResponseTimedOut || completion.Response != "" || completion.CorrelationID != cmd.CommandID.String() {
		t.Fatalf("predecision timeout completion = %+v/%v, want working/pending/timed_out", completion, ok)
	}
	waitForRequestTracker(t, fixture.controller.manager, cmd.CommandID, true)
	if err := fixture.hook.QueueFallback(context.Background(), foreignFallbackIntent(cmd.CommandID, fixture.childID)); err != nil {
		t.Fatalf("QueueFallback: %v", err)
	}
	turnID := mustUUID()
	fixture.sub.feed(foreignTurnEvent(fixture, cmd.CommandID, turnID, false))
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID}}, Message: aiMessage("after timeout")})
	waitForRequestTracker(t, fixture.controller.manager, cmd.CommandID, false)
	select {
	case extra := <-fixture.parent.Commands:
		t.Fatalf("predecision timeout created second handback: %T", extra)
	default:
	}
	for _, record := range fixture.commands.snapshot() {
		switch record.Command().(type) {
		case command.CancelDelegateRequest, command.CancelQueuedInput:
			t.Fatalf("predecision timeout persisted cancellation: %T", record.Command())
		}
	}
	select {
	case raw := <-fixture.child.Commands:
		switch raw.(type) {
		case command.CancelDelegateRequest, command.CancelQueuedInput:
			t.Fatalf("predecision timeout dispatched cancellation: %T", raw)
		default:
			t.Fatalf("unexpected child command after timeout: %T", raw)
		}
	default:
	}
}

func TestMessageAgentForeignObserverBoundLaterCategoricalHandbackOnce(t *testing.T) {
	for _, state := range []foreign.DeliveryResolutionState{foreign.DeliveryResolutionUnknown, foreign.DeliveryResolutionUntrackable} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			fixture := newForeignMessageAgentFixture(t)
			timer := newManualForeignDeliveryTimer()
			fixture.controller.manager.foreignDeliveryTimerFactory = func(time.Duration) foreignDeliveryObserverTimer { return timer }
			resultCh := make(chan struct {
				result tool.DelegateResult
				err    error
			}, 1)
			go func() {
				result, err := fixture.controller.Execute(context.Background(), foreignMessageRequest(fixture.childID, false))
				resultCh <- struct {
					result tool.DelegateResult
					err    error
				}{result: result, err: err}
			}()
			raw := <-fixture.child.Commands
			cmd := raw.(command.UserInput)
			cmd.Accepted <- nil
			timer.fire()
			call := <-resultCh
			if call.err != nil || call.result.DeliveryStatus != tool.DelegateDeliveryAcceptedPending || call.result.CorrelationID != cmd.CommandID {
				t.Fatalf("pending result = %+v/%v, want accepted_pending correlation=%v", call.result, call.err, cmd.CommandID)
			}
			if err := fixture.hook.Reserve(context.Background(), foreign.DeliveryReservation{LoopID: fixture.childID, RequestID: cmd.CommandID}); err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			if err := fixture.hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: fixture.childID, RequestID: cmd.CommandID, State: state}); err != nil {
				t.Fatalf("Resolve %s: %v", state, err)
			}
			rawParent := <-fixture.parent.Commands
			handBack, ok := rawParent.(command.SubagentResult)
			if !ok {
				t.Fatalf("categorical handback = %T, want command.SubagentResult", rawParent)
			}
			completion, ok := decodeBackgroundCompletion(handBack.Blocks)
			want := tool.DelegateDeliveryUnknown
			if state == foreign.DeliveryResolutionUntrackable {
				want = tool.DelegateDeliveryUntrackable
			}
			if !ok || completion.DeliveryStatus != want || completion.ResponseStatus != tool.DelegateResponseUnknown {
				t.Fatalf("categorical completion = %+v/%v, want %s/unknown", completion, ok, want)
			}
			waitForRequestTracker(t, fixture.controller.manager, cmd.CommandID, false)
			select {
			case extra := <-fixture.parent.Commands:
				t.Fatalf("categorical transition created second handback: %T", extra)
			default:
			}
		})
	}
}

func TestMessageAgentForeignTimedOutThenUnknownDoesNotHandBackTwice(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	zero := 0
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		req := foreignMessageRequest(fixture.childID, false)
		req.TimeoutSeconds = &zero
		result, err := fixture.controller.Execute(context.Background(), req)
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	cmd.Accepted <- nil
	call := <-resultCh
	if call.err != nil || call.result.DeliveryStatus != tool.DelegateDeliveryAcceptedPending {
		t.Fatalf("timed-out result = %+v/%v, want accepted_pending", call.result, call.err)
	}
	firstRaw := <-fixture.parent.Commands
	first, ok := firstRaw.(command.SubagentResult)
	if !ok {
		t.Fatalf("timed-out handback = %T, want command.SubagentResult", firstRaw)
	}
	firstCompletion, ok := decodeBackgroundCompletion(first.Blocks)
	if !ok || firstCompletion.DeliveryStatus != tool.DelegateDeliveryAcceptedPending || firstCompletion.ResponseStatus != tool.DelegateResponseTimedOut {
		t.Fatalf("timed-out completion = %+v/%v, want pending/timed_out", firstCompletion, ok)
	}
	waitForRequestTracker(t, fixture.controller.manager, cmd.CommandID, true)
	if err := fixture.hook.Reserve(context.Background(), foreign.DeliveryReservation{LoopID: fixture.childID, RequestID: cmd.CommandID}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := fixture.hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: fixture.childID, RequestID: cmd.CommandID, State: foreign.DeliveryResolutionUnknown}); err != nil {
		t.Fatalf("Resolve unknown: %v", err)
	}
	waitForRequestTracker(t, fixture.controller.manager, cmd.CommandID, false)
	select {
	case extra := <-fixture.parent.Commands:
		t.Fatalf("timed-out then unknown created second handback: %T", extra)
	default:
	}
}

func TestMessageAgentForeignUnknownBackgroundHandbackIsCategoricalAndUnwatched(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(context.Background(), foreignMessageRequest(fixture.childID, false))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	if err := fixture.hook.Reserve(context.Background(), foreign.DeliveryReservation{LoopID: fixture.childID, RequestID: cmd.CommandID}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := fixture.hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: fixture.childID, RequestID: cmd.CommandID, State: foreign.DeliveryResolutionUnknown}); err != nil {
		t.Fatalf("Resolve unknown: %v", err)
	}
	cmd.Accepted <- nil
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("Execute: %v", call.err)
	}
	if call.result.DeliveryStatus != tool.DelegateDeliveryUnknown || call.result.ResponseStatus != tool.DelegateResponseUnknown {
		t.Fatalf("result = %+v, want delivery_unknown with no response status", call.result)
	}
	rawParent := <-fixture.parent.Commands
	handBack, ok := rawParent.(command.SubagentResult)
	if !ok {
		t.Fatalf("unknown handback = %T, want command.SubagentResult", rawParent)
	}
	var envelope struct {
		DeliveryStatus string `json:"delivery_status"`
		ResponseStatus string `json:"response_status"`
	}
	block := handBack.Blocks[0].(*content.TextBlock)
	if err := json.Unmarshal([]byte(block.Text), &envelope); err != nil {
		t.Fatalf("unknown handback JSON: %v", err)
	}
	if envelope.DeliveryStatus != string(tool.DelegateDeliveryUnknown) || envelope.ResponseStatus != "" {
		t.Fatalf("unknown handback = %+v, want categorical delivery only", envelope)
	}
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: mustUUID()}}, Message: aiMessage("late")})
	select {
	case extra := <-fixture.parent.Commands:
		t.Fatalf("late terminal created second handback: %T", extra)
	default:
	}
}

func TestMessageAgentForeignUntrackableBackgroundHandbackIsCategoricalAndUnwatched(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(context.Background(), foreignMessageRequest(fixture.childID, false))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	if err := fixture.hook.Reserve(context.Background(), foreign.DeliveryReservation{LoopID: fixture.childID, RequestID: cmd.CommandID}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := fixture.hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: fixture.childID, RequestID: cmd.CommandID, State: foreign.DeliveryResolutionUntrackable}); err != nil {
		t.Fatalf("Resolve untrackable: %v", err)
	}
	cmd.Accepted <- nil
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("Execute: %v", call.err)
	}
	if call.result.DeliveryStatus != tool.DelegateDeliveryUntrackable || call.result.ResponseStatus != tool.DelegateResponseUnknown {
		t.Fatalf("result = %+v, want delivered_untrackable with no response status", call.result)
	}
	rawParent := <-fixture.parent.Commands
	handBack, ok := rawParent.(command.SubagentResult)
	if !ok {
		t.Fatalf("untrackable handback = %T, want command.SubagentResult", rawParent)
	}
	var envelope struct {
		DeliveryStatus string `json:"delivery_status"`
		ResponseStatus string `json:"response_status"`
	}
	block := handBack.Blocks[0].(*content.TextBlock)
	if err := json.Unmarshal([]byte(block.Text), &envelope); err != nil {
		t.Fatalf("untrackable handback JSON: %v", err)
	}
	if envelope.DeliveryStatus != string(tool.DelegateDeliveryUntrackable) || envelope.ResponseStatus != "" {
		t.Fatalf("untrackable handback = %+v, want categorical delivery only", envelope)
	}
}

func TestMessageAgentForeignQueuedBackgroundTimeoutRetainsTrackerUntilTerminal(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	zero := 0
	go func() {
		req := foreignMessageRequest(fixture.childID, false)
		req.TimeoutSeconds = &zero
		result, err := fixture.controller.Execute(context.Background(), req)
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	if err := fixture.hook.QueueFallback(context.Background(), foreignFallbackIntent(cmd.CommandID, fixture.childID)); err != nil {
		t.Fatalf("QueueFallback: %v", err)
	}
	cmd.Accepted <- nil
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("Execute: %v", call.err)
	}
	if call.result.AgentID != fixture.childID || call.result.State != tool.AgentStateWorking ||
		call.result.DeliveryStatus != tool.DelegateDeliveryQueued ||
		call.result.ResponseStatus != tool.DelegateResponseUnknown ||
		call.result.Response != "" || call.result.CorrelationID != cmd.CommandID {
		t.Fatalf("immediate result = %+v, want working/queued/unknown correlation=%v", call.result, cmd.CommandID)
	}
	var handBack command.SubagentResult
	select {
	case rawParent := <-fixture.parent.Commands:
		var ok bool
		handBack, ok = rawParent.(command.SubagentResult)
		if !ok {
			t.Fatalf("queued background handback = %T, want command.SubagentResult", rawParent)
		}
		completion, ok := decodeBackgroundCompletion(handBack.Blocks)
		if !ok || completion.AgentID != fixture.childID.String() || completion.State != tool.AgentStateWorking ||
			completion.DeliveryStatus != tool.DelegateDeliveryQueued ||
			completion.ResponseStatus != tool.DelegateResponseTimedOut ||
			completion.Response != "" || completion.CorrelationID != cmd.CommandID.String() {
			t.Fatalf("queued background completion = %+v/%v, want working/queued/timed_out empty response correlation=%v", completion, ok, cmd.CommandID)
		}
	case <-time.After(time.Second):
		t.Fatal("trackable queued timeout handback did not arrive")
	}
	waitForRequestTracker(t, fixture.controller.manager, cmd.CommandID, true)
	assertNoForeignCancellationCommands(t, fixture)
	turnID := mustUUID()
	fixture.sub.feed(foreignTurnEvent(fixture, cmd.CommandID, turnID, false))
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID}}, Message: aiMessage("queued background")})
	waitForRequestTracker(t, fixture.controller.manager, cmd.CommandID, false)
	assertNoParentCommand(t, fixture.parent, 100*time.Millisecond)
	assertNoForeignCancellationCommands(t, fixture)
}

func TestMessageAgentForeignRestoreReadmissionUsesNormalPathWithoutSteering(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	requestID := mustUUID()
	cmd := command.UserInput{
		Header: command.Header{CommandID: requestID, Agency: identity.AgencyMachine},
		Blocks: []content.Block{&content.TextBlock{Text: "restored"}}, TargetLoopID: fixture.childID,
		NoFold: true, BackgroundHandBack: true, DelegateDeliveryPhase: command.DelegateDeliveryPhaseIntent,
	}
	entry := restoredBackgroundPlan{
		requestID: requestID, childID: fixture.childID, parentID: fixture.controller.parentLoopID,
		name: "worker", reAdmit: &cmd,
	}
	fixture.controller.manager.readmitRestoredBackgroundRequest(fixture.session, entry)
	raw := <-fixture.child.Commands
	reAdmitted, ok := raw.(command.UserInput)
	if !ok || reAdmitted.CommandID != requestID || reAdmitted.DelegateDeliveryPhase != command.DelegateDeliveryPhaseIntent {
		t.Fatalf("restored command = %#v, want exact intent command", raw)
	}
	for _, ev := range fixture.events.snapshot() {
		if _, reserved := ev.(event.DelegateDeliveryStateChanged); reserved {
			t.Fatalf("restore re-admission attempted steering reservation: %T", ev)
		}
	}
}

func TestMessageAgentForeignObserverCancellationDoesNotRetractAcceptedDelivery(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	callerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(callerCtx, foreignMessageRequest(fixture.childID, true))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	if err := fixture.hook.QueueFallback(context.Background(), foreignFallbackIntent(cmd.CommandID, fixture.childID)); err != nil {
		t.Fatalf("QueueFallback: %v", err)
	}
	cmd.Accepted <- nil
	cancel()
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("Execute: %v", call.err)
	}
	if call.result.DeliveryStatus != tool.DelegateDeliveryQueued || call.result.ResponseStatus != tool.DelegateResponseInterrupted {
		t.Fatalf("result = %+v, want queued/interrupted", call.result)
	}
	select {
	case commandValue := <-fixture.child.Commands:
		t.Fatalf("observer cancellation retracted accepted delivery with %T", commandValue)
	default:
	}
}

func TestMessageAgentForeignSessionCancellationKeepsAdmissionPending(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(context.Background(), foreignMessageRequest(fixture.childID, false))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	cmd.Accepted <- nil
	// Session cancellation ends the observer before the foreign actor emits a
	// terminal delivery phase. It is not evidence of provider Unknown, so the
	// public result must remain accepted-pending and must not issue a cancel.
	fixture.session.sessionCancel()
	select {
	case call := <-resultCh:
		if call.err != nil {
			t.Fatalf("Execute: %v", call.err)
		}
		if call.result.DeliveryStatus != tool.DelegateDeliveryAcceptedPending || call.result.ResponseStatus != tool.DelegateResponseUnknown || call.result.CorrelationID != cmd.CommandID {
			t.Fatalf("result = %+v, want accepted_pending/unknown correlation=%v", call.result, cmd.CommandID)
		}
	case <-time.After(time.Second):
		t.Fatal("session cancellation did not resolve admission observer")
	}
	for _, record := range fixture.commands.snapshot() {
		if _, ok := record.Command().(command.CancelDelegateRequest); ok {
			t.Fatalf("session cancellation retracted accepted delivery: %T", record.Command())
		}
	}
}

func TestMessageAgentForeignForegroundSessionCancellationKeepsAdmissionPending(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(context.Background(), foreignMessageRequest(fixture.childID, true))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	cmd.Accepted <- nil
	fixture.session.sessionCancel()
	select {
	case call := <-resultCh:
		if call.err != nil {
			t.Fatalf("Execute: %v", call.err)
		}
		if call.result.DeliveryStatus != tool.DelegateDeliveryAcceptedPending || call.result.ResponseStatus != tool.DelegateResponseUnknown || call.result.CorrelationID != cmd.CommandID {
			t.Fatalf("result = %+v, want accepted_pending/unknown correlation=%v", call.result, cmd.CommandID)
		}
	case <-time.After(time.Second):
		t.Fatal("foreground session cancellation did not resolve admission observer")
	}
	for _, record := range fixture.commands.snapshot() {
		if _, ok := record.Command().(command.CancelDelegateRequest); ok {
			t.Fatalf("session cancellation retracted accepted delivery: %T", record.Command())
		}
	}
}

func TestMessageAgentForeignCallerTimeoutBeforeDecisionKeepsAdmissionPending(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	callerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(callerCtx, foreignMessageRequest(fixture.childID, true))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	cmd.Accepted <- nil
	cancel()
	select {
	case call := <-resultCh:
		if call.err != nil {
			t.Fatalf("Execute: %v", call.err)
		}
		if call.result.DeliveryStatus != tool.DelegateDeliveryAcceptedPending || call.result.ResponseStatus != tool.DelegateResponseInterrupted || call.result.CorrelationID != cmd.CommandID {
			t.Fatalf("result = %+v, want accepted_pending/interrupted correlation=%v", call.result, cmd.CommandID)
		}
	case <-time.After(time.Second):
		t.Fatal("caller timeout did not resolve admission observer")
	}
	for _, record := range fixture.commands.snapshot() {
		if _, ok := record.Command().(command.CancelDelegateRequest); ok {
			t.Fatalf("caller timeout retracted accepted delivery: %T", record.Command())
		}
	}
}

func TestMessageAgentForeignForegroundTimeoutLaterQueuedCleansWithoutHandback(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	callerCtx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(callerCtx, foreignMessageRequest(fixture.childID, true))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	cmd.Accepted <- nil
	cancel()
	call := <-resultCh
	if call.err != nil {
		t.Fatalf("Execute: %v", call.err)
	}
	if call.result.DeliveryStatus != tool.DelegateDeliveryAcceptedPending || call.result.ResponseStatus != tool.DelegateResponseInterrupted {
		t.Fatalf("timeout result = %+v, want accepted_pending/interrupted", call.result)
	}
	waitForRequestTracker(t, fixture.controller.manager, cmd.CommandID, true)
	if err := fixture.hook.QueueFallback(context.Background(), foreignFallbackIntent(cmd.CommandID, fixture.childID)); err != nil {
		t.Fatalf("QueueFallback: %v", err)
	}
	turnID := mustUUID()
	fixture.sub.feed(foreignTurnEvent(fixture, cmd.CommandID, turnID, false))
	fixture.sub.feed(event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: fixture.session.sessionID, LoopID: fixture.childID, TurnID: turnID}}, Message: aiMessage("late")})
	waitForRequestTracker(t, fixture.controller.manager, cmd.CommandID, false)
	assertNoParentCommand(t, fixture.parent, 100*time.Millisecond)
}

func TestMessageAgentForeignResolvedUnknownIsCategoricalWithoutWatcher(t *testing.T) {
	fixture := newForeignMessageAgentFixture(t)
	resultCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := fixture.controller.Execute(context.Background(), foreignMessageRequest(fixture.childID, false))
		resultCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	raw := <-fixture.child.Commands
	cmd := raw.(command.UserInput)
	if err := fixture.hook.Reserve(context.Background(), foreign.DeliveryReservation{LoopID: fixture.childID, RequestID: cmd.CommandID}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := fixture.hook.Resolve(context.Background(), foreign.DeliveryResolution{LoopID: fixture.childID, RequestID: cmd.CommandID, State: foreign.DeliveryResolutionUnknown}); err != nil {
		t.Fatalf("Resolve unknown: %v", err)
	}
	cmd.Accepted <- nil
	select {
	case call := <-resultCh:
		if call.err != nil {
			t.Fatalf("Execute: %v", call.err)
		}
		if call.result.DeliveryStatus != tool.DelegateDeliveryUnknown || call.result.ResponseStatus != tool.DelegateResponseUnknown {
			t.Fatalf("result = %+v, want delivery_unknown with no response status", call.result)
		}
	case <-time.After(time.Second):
		t.Fatal("foreign internal admission timeout did not resolve")
	}
	select {
	case rawParent := <-fixture.parent.Commands:
		handBack, ok := rawParent.(command.SubagentResult)
		if !ok {
			t.Fatalf("timeout handback = %T, want command.SubagentResult", rawParent)
		}
		completion, ok := decodeBackgroundCompletion(handBack.Blocks)
		if !ok || completion.DeliveryStatus != tool.DelegateDeliveryUnknown || completion.ResponseStatus != tool.DelegateResponseUnknown {
			t.Fatalf("timeout handback = %+v/%v, want categorical unknown", completion, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("foreign internal admission timeout handback did not arrive")
	}
}

func foreignFallbackIntent(requestID, loopID uuid.UUID) foreign.DeliveryFallback {
	return foreign.DeliveryFallback{RequestID: requestID, LoopID: loopID}
}
