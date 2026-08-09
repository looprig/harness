package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
)

// restoreBlockedBackend keeps the real actor's DoneChan while diverting the
// session routing path into a buffered sink the actor never reads. This leaves
// the durable native intent and reservation unresolved without manufacturing a
// TurnStarted/TurnInterrupted pair during the crash barrier.
type restoreBlockedBackend struct {
	commands chan command.Command
	done     <-chan struct{}
}

func (b *restoreBlockedBackend) CommandSink() chan<- command.Command { return b.commands }
func (b *restoreBlockedBackend) DoneChan() <-chan struct{}           { return b.done }
func (b *restoreBlockedBackend) Snapshot(context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	return nil, 0, nil
}

func phasedBackgroundCommand(requestID, childID uuid.UUID, phase command.DelegateDeliveryPhase) command.UserInput {
	return command.UserInput{
		Header:                command.Header{CommandID: requestID, Agency: identity.AgencyMachine},
		TargetLoopID:          childID,
		BackgroundHandBack:    true,
		DelegateDeliveryPhase: phase,
	}
}

func TestMessageAgentRestoreRecognizesPhasedIntentAndFallback(t *testing.T) {
	t.Parallel()
	sessionID, childID := mustUUID(), mustUUID()
	intentID, fallbackID := mustUUID(), mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(intentID, childID, command.DelegateDeliveryPhaseIntent)),
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(fallbackID, childID, command.DelegateDeliveryPhaseFallbackQueued)),
	}
	got, err := backgroundDelegateIntents(records)
	if err != nil {
		t.Fatalf("backgroundDelegateIntents: %v", err)
	}
	if got[intentID] != childID {
		t.Fatalf("intent target = %v, want %v", got[intentID], childID)
	}
	if got[fallbackID] != childID {
		t.Fatalf("fallback target = %v, want %v", got[fallbackID], childID)
	}
}

func TestMessageAgentRestoreUnresolvedReservationBecomesUnknownWithoutSteer(t *testing.T) {
	t.Parallel()
	sessionID, childID := mustUUID(), mustUUID()
	requestID := mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
	}
	reservation := event.DelegateDeliveryStateChanged{
		Header:    event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: mustUUID()},
		RequestID: requestID, TargetLoopID: childID,
		State: event.DelegateDeliverySteerAttemptReserved,
	}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, records, []event.Event{reservation}, nil); err != nil {
		t.Fatalf("seedResolvedDelegateRecords: %v", err)
	}
	resolved, ok := durableResolvedRecord(manager, requestID)
	if !ok || resolved.childID != childID || resolved.status != tool.DelegateStatusUnknown {
		t.Fatalf("unresolved reservation = %+v, %v; want child=%v status=%v", resolved, ok, childID, tool.DelegateStatusUnknown)
	}
}

func TestMessageAgentRestoreRejectsPhasedStateRouteContradiction(t *testing.T) {
	t.Parallel()
	sessionID, childID, wrongChild := mustUUID(), mustUUID(), mustUUID()
	requestID := mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseFallbackQueued)),
	}
	contradiction := event.DelegateDeliveryStateChanged{
		Header:    event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: mustUUID()},
		RequestID: requestID, TargetLoopID: wrongChild,
		State: event.DelegateDeliveryResolvedUnknown,
	}
	err := seedResolvedDelegateRecords(newDelegationManager(Topology{}), records, []event.Event{contradiction}, nil)
	var mismatch *journal.CommandRouteMismatchError
	if !errors.As(err, &mismatch) || mismatch.RecordLoopID != wrongChild || mismatch.TargetLoopID != childID {
		t.Fatalf("phased state contradiction error = %T %+v, want typed route mismatch", err, err)
	}
}

func TestMessageAgentRestoreFoldsAllRequestsIntoOneExactTerminal(t *testing.T) {
	t.Parallel()
	childID, turnID := mustUUID(), mustUUID()
	requestA, requestB, requestC, queuedOnly := mustUUID(), mustUUID(), mustUUID(), mustUUID()
	coord := identity.Coordinates{LoopID: childID, TurnID: turnID}
	got := foldDelegateTerminals([]event.Event{
		event.TurnStarted{Header: event.Header{Coordinates: coord, Cause: identity.Cause{CommandID: requestA}}},
		// InputQueued is an ephemeral admission hint, not evidence that a request
		// belongs to this turn and not an idempotence key for restore.
		event.InputQueued{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: queuedOnly}}},
		event.TurnFoldedInto{Header: event.Header{Coordinates: coord, Cause: identity.Cause{CommandID: requestB}}},
		event.TurnFoldedInto{Header: event.Header{Coordinates: coord, Cause: identity.Cause{CommandID: requestC}}},
		event.TurnDone{Header: event.Header{Coordinates: coord}, Message: aiMessage("one answer")},
	})
	for _, requestID := range []uuid.UUID{requestA, requestB, requestC} {
		resolved, ok := got[requestID]
		if !ok || resolved.childID != childID || resolved.status != tool.DelegateStatusCompleted || resolved.text != "one answer" {
			t.Fatalf("request %v resolved = %+v, %v; want one completed answer", requestID, resolved, ok)
		}
	}
	if _, queued := got[queuedOnly]; queued {
		t.Fatalf("InputQueued-only request %v became a terminal resolution: %+v", queuedOnly, got[queuedOnly])
	}
}

func TestMessageAgentRestoreRejectsOutOfOrderTurnLifecycle(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		events func(childID, requestID, turnID uuid.UUID) []event.Event
	}{
		{name: "terminal before opening", events: func(childID, requestID, turnID uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}, Message: aiMessage("done")},
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
			}
		}},
		{name: "duplicate turn started", events: func(childID, requestID, turnID uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
			}
		}},
		{name: "fold before start", events: func(childID, requestID, turnID uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnFoldedInto{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
			}
		}},
		{name: "opening after terminal", events: func(childID, requestID, turnID uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
				event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}, Message: aiMessage("done")},
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: mustUUID()}}},
			}
		}},
		{name: "fold after terminal", events: func(childID, requestID, turnID uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
				event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}, Message: aiMessage("done")},
				event.TurnFoldedInto{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: mustUUID()}}},
			}
		}},
		{name: "duplicate done terminal", events: func(childID, requestID, turnID uuid.UUID) []event.Event {
			started := event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}}
			done := event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}, Message: aiMessage("done")}
			return []event.Event{started, done, done}
		}},
		{name: "duplicate failed terminal", events: func(childID, requestID, turnID uuid.UUID) []event.Event {
			started := event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}}
			failed := event.TurnFailed{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}, Err: errors.New("failed")}
			return []event.Event{started, failed, failed}
		}},
		{name: "duplicate interrupted terminal", events: func(childID, requestID, turnID uuid.UUID) []event.Event {
			started := event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}}
			interrupted := event.TurnInterrupted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}}
			return []event.Event{started, interrupted, interrupted}
		}},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			childID, requestID, turnID := mustUUID(), mustUUID(), mustUUID()
			records := []journal.JournalRecord{
				journal.NewCommandRecord(mustUUID(), childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
			}
			if err := seedResolvedDelegateRecords(newDelegationManager(Topology{}), records, tt.events(childID, requestID, turnID), nil); err == nil {
				t.Fatalf("out-of-order lifecycle %q was accepted", tt.name)
			}
		})
	}
}

func TestMessageAgentRestoreReadmitsUnopenedPhasedCommands(t *testing.T) {
	t.Parallel()
	sessionID, parentID, childID := mustUUID(), mustUUID(), mustUUID()
	intentID, fallbackID := mustUUID(), mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(intentID, childID, command.DelegateDeliveryPhaseIntent)),
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(fallbackID, childID, command.DelegateDeliveryPhaseFallbackQueued)),
	}
	// InputQueued is deliberately the only ephemeral evidence for the fallback;
	// it must not suppress re-admission of the exact durable command.
	replayed := []event.Event{
		event.LoopStarted{Header: event.Header{
			Coordinates: identity.Coordinates{LoopID: childID},
			Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
		}, InitialRequestID: mustUUID()},
		event.InputQueued{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: fallbackID}}},
	}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
		t.Fatalf("seedResolvedDelegateRecords: %v", err)
	}
	plan, err := manager.planRestoredBackgroundRequests(&Session{loops: map[uuid.UUID]*loopHandle{}}, records, replayed, nil)
	if err != nil {
		t.Fatalf("planRestoredBackgroundRequests: %v", err)
	}
	if len(plan) != 2 {
		t.Fatalf("restore re-admission plan = %+v, want two phased commands", plan)
	}
	seen := make(map[uuid.UUID]bool, len(plan))
	for _, entry := range plan {
		if entry.reAdmit == nil || entry.reAdmit.CommandID.IsZero() {
			t.Fatalf("restore plan entry = %+v, want exact re-admission command", entry)
		}
		seen[entry.reAdmit.CommandID] = true
	}
	if !seen[intentID] || !seen[fallbackID] {
		t.Fatalf("restore re-admission ids = %v, want intent=%v fallback=%v", seen, intentID, fallbackID)
	}
}

func TestMessageAgentRestoreReadmitsForegroundPhasedCommandWithoutHandback(t *testing.T) {
	t.Parallel()
	sessionID, parentID, childID := mustUUID(), mustUUID(), mustUUID()
	for _, phase := range []command.DelegateDeliveryPhase{
		command.DelegateDeliveryPhaseIntent,
		command.DelegateDeliveryPhaseFallbackQueued,
	} {
		t.Run(string(phase), func(t *testing.T) {
			requestID := mustUUID()
			foreground := phasedBackgroundCommand(requestID, childID, phase)
			foreground.BackgroundHandBack = false
			records := []journal.JournalRecord{
				journal.NewCommandRecord(sessionID, childID, foreground),
			}
			replayed := []event.Event{
				event.LoopStarted{Header: event.Header{
					Coordinates: identity.Coordinates{LoopID: childID},
					Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
				}, DisplayName: "worker"},
			}
			manager := newDelegationManager(Topology{})
			if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
				t.Fatalf("seedResolvedDelegateRecords: %v", err)
			}
			plan, err := manager.planRestoredBackgroundRequests(&Session{loops: map[uuid.UUID]*loopHandle{}}, records, replayed, nil)
			if err != nil {
				t.Fatalf("planRestoredBackgroundRequests: %v", err)
			}
			if len(plan) != 1 || plan[0].reAdmit == nil {
				t.Fatalf("foreground restore plan = %+v, want one re-admission", plan)
			}
			if plan[0].reAdmit.CommandID != requestID || plan[0].reAdmit.BackgroundHandBack {
				t.Fatalf("foreground restore command = %+v, want same id without hand-back", *plan[0].reAdmit)
			}
			if plan[0].handBack != nil {
				t.Fatalf("foreground restore synthesized hand-back = %+v, want none", plan[0].handBack)
			}
		})
	}
}

func TestMessageAgentRestoreReadmitsForegroundIntentAfterCrashWithoutHandback(t *testing.T) {
	parentLLM := newControlledAgentLLM()
	childLLM := newControlledAgentLLM()
	lifecycle, session, _ := newAgentRestoreLifecycle(t, newRestoreStore(t), parentLLM, childLLM, nil)
	controller := session.delegation.controllerFor(session.ActiveLoopID(), backgroundNode("parent", parentLLM, "child"))
	started, err := controller.Execute(delegateCtx(t), tool.DelegateRequest{
		Operation: tool.DelegateStart, AgentType: "child", Message: "start", WaitForResponse: true,
	})
	if err != nil {
		t.Fatalf("initial child start: %v", err)
	}
	select {
	case message := <-childLLM.started:
		if message != "start" {
			t.Fatalf("initial child message = %q, want start", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial child did not start")
	}
	childLLM.release <- struct{}{}

	session.loopsMu.Lock()
	childHandle := session.loops[started.AgentID]
	if childHandle == nil || childHandle.backend == nil {
		session.loopsMu.Unlock()
		t.Fatalf("initial child %v has no backend", started.AgentID)
	}
	blockedBackend := &restoreBlockedBackend{commands: make(chan command.Command, 1), done: childHandle.backend.DoneChan()}
	childHandle.backend = blockedBackend
	session.loopsMu.Unlock()

	zero := 0
	sendCh := make(chan error, 1)
	go func() {
		_, sendErr := controller.Execute(delegateCtx(t), tool.DelegateRequest{
			Operation: tool.DelegateSend, AgentID: started.AgentID, Message: "foreground after restore", WaitForResponse: true, TimeoutSeconds: &zero,
		})
		sendCh <- sendErr
	}()
	var dispatched command.UserInput
	for {
		raw := <-blockedBackend.commands
		switch typed := raw.(type) {
		case command.UserInput:
			dispatched = typed
		case command.CancelDelegateRequest, command.CancelQueuedInput:
			// A pre-existing child cleanup command is unrelated to the new
			// durable foreground intent; keep draining the diverted route.
			continue
		default:
			t.Fatalf("foreground crash command = %T, want command.UserInput", raw)
		}
		break
	}
	dispatched.Accepted <- nil
	select {
	case sendErr := <-sendCh:
		if sendErr != nil {
			t.Fatalf("foreground send before crash: %v", sendErr)
		}
	case <-time.After(time.Second):
		t.Fatal("foreground send before crash did not observe timeout")
	}
	beforeCrash := messageAgentRestoreRecords(t, lifecycle.store, session.SessionID())
	foundIntent := false
	for _, record := range beforeCrash {
		commandRecord, ok := record.(journal.CommandRecord)
		if !ok {
			continue
		}
		input, ok := commandRecord.Command().(command.UserInput)
		if ok && input.CommandID == dispatched.CommandID {
			foundIntent = true
			if input.BackgroundHandBack || input.NoFold || input.DelegateDeliveryPhase != command.DelegateDeliveryPhaseIntent {
				t.Fatalf("foreground durable command = %+v, want phased foldable intent without hand-back", input)
			}
		}
	}
	if !foundIntent {
		t.Fatalf("foreground durable intent %v missing before crash", dispatched.CommandID)
	}

	sessionID := session.SessionID()
	crashAgentRestoreSession(t, session)
	restored, err := lifecycle.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	defer restored.Shutdown(context.Background())
	select {
	case message := <-childLLM.started:
		if message != "foreground after restore" {
			t.Fatalf("restored foreground message = %q, want exact re-admission", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restored foreground command was not re-admitted")
	}
	restoredController := restored.delegation.controllerFor(restored.ActiveLoopID(), backgroundNode("parent", parentLLM, "child"))
	pending, err := restoredController.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: dispatched.TargetLoopID})
	if err != nil {
		t.Fatalf("restored foreground pending status: %v", err)
	}
	if len(pending.Agents) != 1 || pending.Agents[0].State != tool.AgentStateWorking {
		t.Fatalf("restored foreground pending status = %+v, want working", pending.Agents)
	}
	if got := countMessageAgentHandbacks(messageAgentRestoreRecords(t, lifecycle.store, sessionID), dispatched.CommandID); got != 0 {
		t.Fatalf("restored foreground hand-backs = %d, want none", got)
	}
	childLLM.release <- struct{}{}
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := restored.WaitIdle(waitCtx); err != nil {
		t.Fatalf("restored foreground WaitIdle: %v", err)
	}
	terminal, err := restoredController.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: dispatched.TargetLoopID})
	if err != nil {
		t.Fatalf("restored foreground terminal status: %v", err)
	}
	if len(terminal.Agents) != 1 || terminal.Agents[0].State != tool.AgentStateIdle {
		t.Fatalf("restored foreground terminal status = %+v, want idle", terminal.Agents)
	}
}

func TestMessageAgentRestoreReservationNeverReadmits(t *testing.T) {
	t.Parallel()
	sessionID, parentID, childID := mustUUID(), mustUUID(), mustUUID()
	requestID := mustUUID()
	records := []journal.JournalRecord{
		journal.NewCommandRecord(sessionID, childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
	}
	replayed := []event.Event{
		event.LoopStarted{Header: event.Header{
			Coordinates: identity.Coordinates{LoopID: childID},
			Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
		}, InitialRequestID: mustUUID()},
		event.DelegateDeliveryStateChanged{
			Header:       event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: mustUUID()},
			RequestID:    requestID,
			TargetLoopID: childID,
			State:        event.DelegateDeliverySteerAttemptReserved,
		},
	}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
		t.Fatalf("seedResolvedDelegateRecords: %v", err)
	}
	plan, err := manager.planRestoredBackgroundRequests(&Session{loops: map[uuid.UUID]*loopHandle{}}, records, replayed, nil)
	if err != nil {
		t.Fatalf("planRestoredBackgroundRequests: %v", err)
	}
	if len(plan) != 0 {
		t.Fatalf("unknown delivery plan = %+v, want no delivery action or terminal hand-back", plan)
	}
}

func TestMessageAgentRestorePersistsUnknownReservationIdempotentlyWithoutHandback(t *testing.T) {
	parentLLM := newControlledAgentLLM()
	childLLM := newControlledAgentLLM()
	lifecycle, session, _ := newAgentRestoreLifecycle(t, newRestoreStore(t), parentLLM, childLLM, nil)
	controller := session.delegation.controllerFor(session.ActiveLoopID(), backgroundNode("parent", parentLLM, "child"))
	startCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := controller.Execute(delegateCtx(t), tool.DelegateRequest{
			Operation: tool.DelegateStart, AgentType: "child", Message: "start", WaitForResponse: true,
		})
		startCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	<-childLLM.started
	childLLM.release <- struct{}{}
	started := <-startCh
	if started.err != nil {
		t.Fatalf("initial child start: %v", started.err)
	}
	session.loopsMu.Lock()
	childHandle := session.loops[started.result.AgentID]
	if childHandle == nil || childHandle.backend == nil {
		session.loopsMu.Unlock()
		t.Fatalf("initial child %v has no backend", started.result.AgentID)
	}
	blockedBackend := &restoreBlockedBackend{
		commands: make(chan command.Command, 1),
		done:     childHandle.backend.DoneChan(),
	}
	childHandle.backend = blockedBackend
	session.loopsMu.Unlock()
	sendCh := make(chan struct {
		result tool.DelegateResult
		err    error
	}, 1)
	go func() {
		result, err := controller.Execute(delegateCtx(t), tool.DelegateRequest{
			Operation: tool.DelegateSend, AgentID: started.result.AgentID, Message: "ambiguous", WaitForResponse: false,
		})
		sendCh <- struct {
			result tool.DelegateResult
			err    error
		}{result: result, err: err}
	}()
	dispatched, ok := (<-blockedBackend.commands).(command.UserInput)
	if !ok {
		t.Fatalf("native background command = %T, want command.UserInput", dispatched)
	}
	dispatched.Accepted <- nil
	sentResult := <-sendCh
	sent, err := sentResult.result, sentResult.err
	if err != nil {
		t.Fatalf("native background send: %v", err)
	}
	reservation := event.DelegateDeliveryStateChanged{
		Header:    event.Header{Coordinates: identity.Coordinates{SessionID: session.SessionID()}, EventID: mustUUID()},
		RequestID: sent.CorrelationID, TargetLoopID: sent.AgentID, State: event.DelegateDeliverySteerAttemptReserved,
	}
	if err := session.PublishEvent(context.Background(), reservation); err != nil {
		t.Fatalf("publish reservation: %v", err)
	}
	sessionID := session.SessionID()
	crashAgentRestoreSession(t, session)
	waitForMessageAgentRecordsStable(t, lifecycle.store, sessionID)
	baselineRecords := messageAgentRestoreRecords(t, lifecycle.store, sessionID)
	baselineHandbacks := countMessageAgentHandbacks(baselineRecords, sent.CorrelationID)

	restored, err := lifecycle.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("first RestoreSession: %v", err)
	}
	recordsAfterFirst := messageAgentRestoreRecords(t, lifecycle.store, sessionID)
	assertMessageAgentUnknownRepair(t, recordsAfterFirst, sent.CorrelationID)
	if countMessageAgentHandbacks(recordsAfterFirst, sent.CorrelationID) != baselineHandbacks {
		t.Fatalf("first restore changed SubagentResult count from %d to %d for unresolved delivery", baselineHandbacks, countMessageAgentHandbacks(recordsAfterFirst, sent.CorrelationID))
	}
	if err := restored.Shutdown(context.Background()); err != nil {
		t.Fatalf("first restored Shutdown: %v", err)
	}

	second, err := lifecycle.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("second RestoreSession: %v", err)
	}
	recordsAfterSecond := messageAgentRestoreRecords(t, lifecycle.store, sessionID)
	assertMessageAgentUnknownRepair(t, recordsAfterSecond, sent.CorrelationID)
	if countMessageAgentHandbacks(recordsAfterSecond, sent.CorrelationID) != baselineHandbacks {
		t.Fatalf("second restore changed SubagentResult count from %d to %d for unresolved delivery", baselineHandbacks, countMessageAgentHandbacks(recordsAfterSecond, sent.CorrelationID))
	}
	if err := second.Shutdown(context.Background()); err != nil {
		t.Fatalf("second restored Shutdown: %v", err)
	}
}

func messageAgentRestoreRecords(t *testing.T, store *sessionstore.Store, sessionID uuid.UUID) []journal.JournalRecord {
	t.Helper()
	replayer, err := store.OpenInternalRecordReplayer(sessionID, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		t.Fatalf("OpenInternalRecordReplayer: %v", err)
	}
	records, err := drainRecordReplay(context.Background(), replayer, journal.ReplayRequest{Follow: false})
	if err != nil {
		t.Fatalf("drainRecordReplay: %v", err)
	}
	return records
}

func waitForMessageAgentRecordsStable(t *testing.T, store *sessionstore.Store, sessionID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	previous := -1
	stable := 0
	for time.Now().Before(deadline) {
		length := len(messageAgentRestoreRecords(t, store, sessionID))
		if length == previous {
			stable++
			if stable >= 5 {
				return
			}
		} else {
			previous = length
			stable = 0
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("journal did not stabilize before restore")
}

func assertMessageAgentUnknownRepair(t *testing.T, records []journal.JournalRecord, requestID uuid.UUID) {
	t.Helper()
	unknown := 0
	for _, record := range records {
		eventRecord, ok := record.(journal.EventRecord)
		if !ok {
			continue
		}
		state, ok := eventRecord.Event().(event.DelegateDeliveryStateChanged)
		if ok && state.RequestID == requestID && state.State == event.DelegateDeliveryResolvedUnknown {
			unknown++
		}
	}
	if unknown != 1 {
		t.Fatalf("resolved_unknown repairs = %d, want one durable idempotent repair", unknown)
	}
}

func countMessageAgentHandbacks(records []journal.JournalRecord, requestID uuid.UUID) int {
	count := 0
	for _, record := range records {
		commandRecord, ok := record.(journal.CommandRecord)
		if !ok {
			continue
		}
		handBack, ok := commandRecord.Command().(command.SubagentResult)
		if !ok {
			continue
		}
		envelope, ok := decodeBackgroundCompletion(handBack.Blocks)
		if ok && envelope.CorrelationID == requestID.String() {
			count++
		}
	}
	return count
}

func TestMessageAgentRestoreRejectsContradictoryCorrelation(t *testing.T) {
	t.Parallel()
	const (
		terminalLoopMismatch  = "terminal loop mismatch"
		requestMappedTwice    = "request mapped to multiple turns"
		incompatibleTerminal  = "incompatible terminals"
		stateTerminal         = "delivery state plus terminal"
		stateCancellation     = "delivery state plus cancellation"
		openingLoopMismatch   = "opening loop mismatch"
		foldedLoopMismatch    = "folded loop mismatch"
		cancelledLoopMismatch = "cancelled loop mismatch"
		rejectedLoopMismatch  = "rejected loop mismatch"
		rejectedWithOpening   = "rejected with opening"
		cancelledWithOpening  = "cancelled with opening"
		rejectedWithTerminal  = "rejected with terminal"
		cancelledWithTerminal = "cancelled with terminal"
	)
	for _, tt := range []struct {
		name   string
		events func(childID, wrongLoop, requestID, turnA, turnB uuid.UUID) []event.Event
	}{
		{name: terminalLoopMismatch, events: func(childID, wrongLoop, requestID, turnA, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}, Cause: identity.Cause{CommandID: requestID}}},
				event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: wrongLoop, TurnID: turnA}}, Message: aiMessage("wrong loop")},
			}
		}},
		{name: requestMappedTwice, events: func(childID, _, requestID, turnA, turnB uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}, Cause: identity.Cause{CommandID: requestID}}},
				event.TurnFoldedInto{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnB}, Cause: identity.Cause{CommandID: requestID}}},
			}
		}},
		{name: incompatibleTerminal, events: func(childID, _, requestID, turnA, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}, Cause: identity.Cause{CommandID: requestID}}},
				event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}}, Message: aiMessage("done")},
				event.TurnFailed{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}}, Err: errors.New("failed")},
			}
		}},
		{name: stateTerminal, events: func(childID, _, requestID, turnA, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.DelegateDeliveryStateChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: mustUUID()}, EventID: mustUUID()}, RequestID: requestID, TargetLoopID: childID, State: event.DelegateDeliveryResolvedUnknown},
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}, Cause: identity.Cause{CommandID: requestID}}},
				event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}}, Message: aiMessage("done")},
			}
		}},
		{name: stateCancellation, events: func(childID, _, requestID, _, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.DelegateDeliveryStateChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: mustUUID()}, EventID: mustUUID()}, RequestID: requestID, TargetLoopID: childID, State: event.DelegateDeliveryResolvedUntrackable},
				event.InputCancelled{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}, Reason: event.CancelClientRetracted},
			}
		}},
		{name: openingLoopMismatch, events: func(_, wrongLoop, requestID, turnA, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: wrongLoop, TurnID: turnA}, Cause: identity.Cause{CommandID: requestID}}},
			}
		}},
		{name: foldedLoopMismatch, events: func(_, wrongLoop, requestID, turnA, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnFoldedInto{Header: event.Header{Coordinates: identity.Coordinates{LoopID: wrongLoop, TurnID: turnA}, Cause: identity.Cause{CommandID: requestID}}},
			}
		}},
		{name: cancelledLoopMismatch, events: func(_, wrongLoop, requestID, _, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.InputCancelled{Header: event.Header{Coordinates: identity.Coordinates{LoopID: wrongLoop}, Cause: identity.Cause{CommandID: requestID}}, Reason: event.CancelClientRetracted},
			}
		}},
		{name: rejectedLoopMismatch, events: func(_, wrongLoop, requestID, _, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnRejected{Header: event.Header{Coordinates: identity.Coordinates{LoopID: wrongLoop}, Cause: identity.Cause{CommandID: requestID}}, Reason: event.RejectShuttingDown},
			}
		}},
		{name: rejectedWithOpening, events: func(childID, _, requestID, turnA, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}, Cause: identity.Cause{CommandID: requestID}}},
				event.TurnRejected{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}, Reason: event.RejectShuttingDown},
			}
		}},
		{name: cancelledWithOpening, events: func(childID, _, requestID, turnA, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}, Cause: identity.Cause{CommandID: requestID}}},
				event.InputCancelled{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}, Reason: event.CancelClientRetracted},
			}
		}},
		{name: rejectedWithTerminal, events: func(childID, _, requestID, turnA, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}, Cause: identity.Cause{CommandID: requestID}}},
				event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}}, Message: aiMessage("done")},
				event.TurnRejected{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}, Reason: event.RejectShuttingDown},
			}
		}},
		{name: cancelledWithTerminal, events: func(childID, _, requestID, turnA, _ uuid.UUID) []event.Event {
			return []event.Event{
				event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}, Cause: identity.Cause{CommandID: requestID}}},
				event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnA}}, Message: aiMessage("done")},
				event.InputCancelled{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}, Reason: event.CancelClientRetracted},
			}
		}},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			childID, wrongLoop, requestID, turnA, turnB := mustUUID(), mustUUID(), mustUUID(), mustUUID(), mustUUID()
			records := []journal.JournalRecord{
				journal.NewCommandRecord(mustUUID(), childID, phasedBackgroundCommand(requestID, childID, command.DelegateDeliveryPhaseIntent)),
			}
			err := seedResolvedDelegateRecords(newDelegationManager(Topology{}), records, tt.events(childID, wrongLoop, requestID, turnA, turnB), nil)
			if err == nil {
				t.Fatalf("contradiction %q was accepted", tt.name)
			}
		})
	}
}
