package sessionruntime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
)

func TestPlanRestoredBackgroundRequestReplaysExistingHandBack(t *testing.T) {
	t.Parallel()
	requestID, childID, parentID, handBackID := mustUUID(), mustUUID(), mustUUID(), mustUUID()
	manager := newDelegationManager(Topology{})
	background := command.UserInput{
		Header:             command.Header{CommandID: requestID, Agency: identity.AgencyMachine},
		NoFold:             true,
		TargetLoopID:       childID,
		BackgroundHandBack: true,
	}
	completion := backgroundCompletionBlocks(childID, "worker", requestID, tool.DelegateStatusCompleted, "done")
	handBack := command.SubagentResult{
		Header:      command.Header{CommandID: handBackID, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: childID}}},
		Coordinates: identity.Coordinates{LoopID: parentID},
		Blocks:      completion,
	}
	replayed := []event.Event{
		event.LoopStarted{
			Header:           event.Header{AgentName: "worker", Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}}},
			DisplayName:      "worker",
			InitialRequestID: requestID,
		},
		event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: mustUUID()}, Cause: identity.Cause{CommandID: requestID}}},
		event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: mustUUID()}}, Message: aiMessage("done")},
	}
	// Use the same turn id for the terminal correlation; the helper above deliberately
	// keeps the event construction compact, so replace it before folding.
	turnID := replayed[1].(event.TurnStarted).TurnID
	done := replayed[2].(event.TurnDone)
	done.TurnID = turnID
	replayed[2] = done
	records := []journal.JournalRecord{
		journal.NewCommandRecord(mustUUID(), childID, background),
		journal.NewCommandRecord(mustUUID(), parentID, handBack),
	}
	if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
		t.Fatal(err)
	}
	s := &Session{loops: map[uuid.UUID]*loopHandle{}}
	plan, err := manager.planRestoredBackgroundRequests(s, records, replayed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].handBack == nil || plan[0].handBack.CommandID != handBackID {
		t.Fatalf("plan = %+v, want one replay of hand-back %v", plan, handBackID)
	}
}

func TestPlanRestoredBackgroundRequestSkipsProcessedParentInput(t *testing.T) {
	t.Parallel()
	requestID, childID, parentID, handBackID := mustUUID(), mustUUID(), mustUUID(), mustUUID()
	manager := newDelegationManager(Topology{})
	background := command.UserInput{
		Header:             command.Header{CommandID: requestID, Agency: identity.AgencyMachine},
		NoFold:             true,
		TargetLoopID:       childID,
		BackgroundHandBack: true,
	}
	completion := backgroundCompletionBlocks(childID, "worker", requestID, tool.DelegateStatusCompleted, "done")
	handBack := command.SubagentResult{
		Header:      command.Header{CommandID: handBackID, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: childID}}},
		Coordinates: identity.Coordinates{LoopID: parentID},
		Blocks:      completion,
	}
	turnID := mustUUID()
	childStart := event.LoopStarted{
		Header:           event.Header{AgentName: "worker", Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}}},
		DisplayName:      "worker",
		InitialRequestID: requestID,
	}
	replayed := []event.Event{
		childStart,
		event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
		event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}, Message: aiMessage("done")},
		event.TurnStarted{
			Header:  event.Header{Coordinates: identity.Coordinates{LoopID: parentID, TurnID: mustUUID()}, Cause: identity.Cause{CommandID: handBackID, Coordinates: identity.Coordinates{LoopID: childID}}},
			Message: &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: completion}},
		},
	}
	records := []journal.JournalRecord{
		journal.NewCommandRecord(mustUUID(), childID, background),
		journal.NewCommandRecord(mustUUID(), parentID, handBack),
	}
	if err := seedResolvedDelegateRecords(manager, records, replayed, nil); err != nil {
		t.Fatal(err)
	}
	s := &Session{loops: map[uuid.UUID]*loopHandle{}}
	plan, err := manager.planRestoredBackgroundRequests(s, records, replayed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Fatalf("processed parent input was scheduled again: %+v", plan)
	}
}

// restoreCommandGate creates the durable snapshot where the child terminal is
// committed but the SubagentResult intent has not yet reached the journal. The
// initial delegate command is allow-listed after admission; every later command
// append is held until the simulated crash cancels the predecessor session.
type restoreCommandGate struct {
	enabled    atomic.Bool
	allowedID  string
	reached    chan struct{}
	release    chan struct{}
	reachedOne sync.Once
	releaseOne sync.Once
}

func newRestoreCommandGate() *restoreCommandGate {
	return &restoreCommandGate{reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *restoreCommandGate) allowInitial(requestID uuid.UUID) {
	g.allowedID = requestID.String()
	g.enabled.Store(true)
}

func (g *restoreCommandGate) open() {
	g.releaseOne.Do(func() { close(g.release) })
}

func (g *restoreCommandGate) begin(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
	if !g.enabled.Load() || call.JournalAppend == nil || call.JournalAppend.Family != hook.RecordCommand || call.JournalAppend.RecordID == g.allowedID {
		return ctx, nil
	}
	g.reachedOne.Do(func() { close(g.reached) })
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return ctx, nil
}

// restoreTurnGate stops the parent actor after the durable acceptance event but
// before TurnStarted. It produces the "hand-back admitted but not processed"
// snapshot without relying on timing or sleeps.
type restoreTurnGate struct {
	reached    chan struct{}
	release    chan struct{}
	reachedOne sync.Once
	releaseOne sync.Once
}

func newRestoreTurnGate() *restoreTurnGate {
	return &restoreTurnGate{reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *restoreTurnGate) open() {
	g.releaseOne.Do(func() { close(g.release) })
}

func (g *restoreTurnGate) begin(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
	if call.Turn == nil || call.Turn.Input == nil {
		return ctx, nil
	}
	if _, ok := decodeBackgroundCompletion(call.Turn.Input.Blocks); !ok {
		return ctx, nil
	}
	g.reachedOne.Do(func() { close(g.reached) })
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return ctx, nil
}

func restoreHookRunner(t *testing.T, operation hook.Operation, begin hook.BeginFunc) *hook.Runner {
	t.Helper()
	runner, err := hook.Compile(hook.Set{Around: []hook.Around{{Operation: operation, Begin: begin}}})
	if err != nil {
		t.Fatalf("compile restore hook: %v", err)
	}
	return runner
}

func newAgentRestoreLifecycle(t *testing.T, store *sessionstore.Store, parentLLM, childLLM *controlledAgentLLM, runner *hook.Runner) (*Lifecycle, *Session, loop.Definition) {
	t.Helper()
	parent := backgroundNode("parent", parentLLM, "child")
	child := backgroundNode("child", childLLM)
	topology := Topology{
		Definitions:  []loop.Definition{parent, child},
		Primers:      []identity.AgentName{parent.Name()},
		ActivePrimer: parent.Name(),
	}
	options := []LifecycleOption{WithLifecycleFingerprintProvider(testFingerprintProvider)}
	if runner != nil {
		options = append(options, WithLifecycleHooks(runner))
	}
	lifecycle, err := NewTopologyLifecycle(topology, store, options...)
	if err != nil {
		t.Fatalf("NewTopologyLifecycle: %v", err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return lifecycle, s, child
}

func crashAgentRestoreSession(t *testing.T, s *Session) {
	t.Helper()
	sid := s.SessionID()
	s.sessionCancel()
	waitLoopsExited(t, s)
	s.releaseLease(context.Background())
	if sid.IsZero() {
		t.Fatal("crashed session has zero session id")
	}
}

func countBackgroundInputs(t *testing.T, store *sessionstore.Store, sessionID, requestID uuid.UUID) int {
	t.Helper()
	count := 0
	for _, ev := range replayAllSessionEvents(t, store, sessionID) {
		var message *content.UserMessage
		switch typed := ev.(type) {
		case event.TurnStarted:
			message = typed.Message
		case event.TurnFoldedInto:
			message = typed.Message
		}
		if message == nil {
			continue
		}
		envelope, ok := decodeBackgroundCompletion(message.Blocks)
		if !ok {
			continue
		}
		correlation, err := uuid.Parse(envelope.CorrelationID)
		if err == nil && correlation == requestID {
			count++
		}
	}
	return count
}

func restoreListAgentRuntime(t *testing.T, controller tool.DelegateController, agentID uuid.UUID) tool.DelegateRuntime {
	t.Helper()
	status, err := controller.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: agentID})
	if err != nil || len(status.Agents) != 1 {
		t.Fatalf("pre-restore agent status = %+v, %v", status.Agents, err)
	}
	return status.Agents[0].Runtime
}

func restoreAndAssertOneBackgroundCompletion(t *testing.T, lifecycle *Lifecycle, store *sessionstore.Store, parentLLM *controlledAgentLLM, child loop.Definition, wantRuntime tool.DelegateRuntime, sessionID, requestID, childID uuid.UUID, wantStatus tool.DelegateResponseStatus) *Session {
	t.Helper()
	restored, err := lifecycle.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })

	completion := receiveBackgroundCompletion(t, parentLLM)
	if completion.AgentID != childID.String() || completion.Name != "worker" || completion.CorrelationID != requestID.String() || completion.ResponseStatus != wantStatus {
		t.Fatalf("restored completion = %+v, want child=%v request=%v status=%v", completion, childID, requestID, wantStatus)
	}
	parentLLM.release <- struct{}{}
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := restored.WaitIdle(waitCtx); err != nil {
		t.Fatalf("restored WaitIdle: %v", err)
	}
	if got := countBackgroundInputs(t, store, sessionID, requestID); got != 1 {
		t.Fatalf("durable completion count = %d, want exactly one", got)
	}

	controller := restored.delegation.controllerFor(restored.ActiveLoopID(), backgroundNode("parent", parentLLM, "child"))
	status, err := controller.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, AgentID: childID})
	if err != nil || len(status.Agents) != 1 {
		t.Fatalf("restored agent status = %+v, %v", status.Agents, err)
	}
	agent := status.Agents[0]
	if agent.Name != "worker" || agent.AgentType != string(child.Name()) || agent.AgentMode != string(child.InitialMode()) || agent.State != tool.AgentStateIdle || agent.Runtime != wantRuntime {
		t.Fatalf("restored agent identity/state = %+v, want worker/%s/%s/idle", agent, child.Name(), child.InitialMode())
	}
	select {
	case duplicate := <-parentLLM.started:
		t.Fatalf("duplicate restored completion: %q", duplicate)
	default:
	}
	return restored
}

// TestAgentRestoreReconcilesDurableEdges exercises the four Task 8 snapshots
// through the real Lifecycle journal and RestoreSession path. Each case proves
// one completion, no wake-token leak (WaitIdle), and stable direct-child identity.
func TestAgentRestoreReconcilesDurableEdges(t *testing.T) {
	tests := []struct {
		name      string
		processed bool
		setup     func(*testing.T) (*Lifecycle, *sessionstore.Store, *Session, *controlledAgentLLM, loop.Definition, uuid.UUID, uuid.UUID, uuid.UUID, tool.DelegateRuntime, func())
		want      tool.DelegateResponseStatus
	}{
		{
			name: "admitted child still running",
			setup: func(t *testing.T) (*Lifecycle, *sessionstore.Store, *Session, *controlledAgentLLM, loop.Definition, uuid.UUID, uuid.UUID, uuid.UUID, tool.DelegateRuntime, func()) {
				store := newRestoreStore(t)
				parentLLM, childLLM := newControlledAgentLLM(), newControlledAgentLLM()
				lifecycle, session, child := newAgentRestoreLifecycle(t, store, parentLLM, childLLM, nil)
				obs, err := session.SubscribeEvents(allFilter())
				if err != nil {
					t.Fatalf("SubscribeEvents: %v", err)
				}
				controller := session.delegation.controllerFor(session.ActiveLoopID(), backgroundNode("parent", parentLLM, "child"))
				started, err := controller.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Name: "worker", Message: "go", WaitForResponse: false})
				if err != nil {
					t.Fatalf("StartAgent: %v", err)
				}
				select {
				case <-childLLM.started:
				case <-time.After(5 * time.Second):
					t.Fatal("child did not start")
				}
				runtime := restoreListAgentRuntime(t, controller, started.AgentID)
				return lifecycle, store, session, parentLLM, child, session.SessionID(), started.CorrelationID, started.AgentID, runtime, func() { _ = obs.Close() }
			},
			want: tool.DelegateResponseInterrupted,
		},
		{
			name: "child terminal before hand-back intent",
			setup: func(t *testing.T) (*Lifecycle, *sessionstore.Store, *Session, *controlledAgentLLM, loop.Definition, uuid.UUID, uuid.UUID, uuid.UUID, tool.DelegateRuntime, func()) {
				store := newRestoreStore(t)
				parentLLM, childLLM := newControlledAgentLLM(), newControlledAgentLLM()
				gate := newRestoreCommandGate()
				runner := restoreHookRunner(t, hook.OperationJournalAppend, gate.begin)
				lifecycle, session, child := newAgentRestoreLifecycle(t, store, parentLLM, childLLM, runner)
				obs, err := session.SubscribeEvents(allFilter())
				if err != nil {
					t.Fatalf("SubscribeEvents: %v", err)
				}
				controller := session.delegation.controllerFor(session.ActiveLoopID(), backgroundNode("parent", parentLLM, "child"))
				started, err := controller.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Name: "worker", Message: "go", WaitForResponse: false})
				if err != nil {
					t.Fatalf("StartAgent: %v", err)
				}
				gate.allowInitial(started.CorrelationID)
				childLLM.release <- struct{}{}
				if !waitTurnDoneOnLoop(t, obs, started.AgentID) {
					t.Fatal("child terminal was not durable")
				}
				select {
				case <-gate.reached:
				case <-time.After(5 * time.Second):
					t.Fatal("hand-back command append was not held")
				}
				runtime := restoreListAgentRuntime(t, controller, started.AgentID)
				return lifecycle, store, session, parentLLM, child, session.SessionID(), started.CorrelationID, started.AgentID, runtime, func() { gate.open(); _ = obs.Close() }
			},
			want: tool.DelegateResponseCompleted,
		},
		{
			name: "hand-back admitted before parent processing",
			setup: func(t *testing.T) (*Lifecycle, *sessionstore.Store, *Session, *controlledAgentLLM, loop.Definition, uuid.UUID, uuid.UUID, uuid.UUID, tool.DelegateRuntime, func()) {
				store := newRestoreStore(t)
				parentLLM, childLLM := newControlledAgentLLM(), newControlledAgentLLM()
				gate := newRestoreTurnGate()
				runner := restoreHookRunner(t, hook.OperationTurn, gate.begin)
				lifecycle, session, child := newAgentRestoreLifecycle(t, store, parentLLM, childLLM, runner)
				controller := session.delegation.controllerFor(session.ActiveLoopID(), backgroundNode("parent", parentLLM, "child"))
				started, err := controller.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Name: "worker", Message: "go", WaitForResponse: false})
				if err != nil {
					t.Fatalf("StartAgent: %v", err)
				}
				childLLM.release <- struct{}{}
				select {
				case <-gate.reached:
				case <-time.After(5 * time.Second):
					t.Fatal("parent hand-back did not reach pre-TurnStarted gate")
				}
				runtime := restoreListAgentRuntime(t, controller, started.AgentID)
				return lifecycle, store, session, parentLLM, child, session.SessionID(), started.CorrelationID, started.AgentID, runtime, func() { gate.open() }
			},
			want: tool.DelegateResponseCompleted,
		},
		{
			name:      "hand-back already processed",
			processed: true,
			setup: func(t *testing.T) (*Lifecycle, *sessionstore.Store, *Session, *controlledAgentLLM, loop.Definition, uuid.UUID, uuid.UUID, uuid.UUID, tool.DelegateRuntime, func()) {
				store := newRestoreStore(t)
				parentLLM, childLLM := newControlledAgentLLM(), newControlledAgentLLM()
				lifecycle, session, child := newAgentRestoreLifecycle(t, store, parentLLM, childLLM, nil)
				obs, err := session.SubscribeEvents(allFilter())
				if err != nil {
					t.Fatalf("SubscribeEvents: %v", err)
				}
				controller := session.delegation.controllerFor(session.ActiveLoopID(), backgroundNode("parent", parentLLM, "child"))
				started, err := controller.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: "child", Name: "worker", Message: "go", WaitForResponse: false})
				if err != nil {
					t.Fatalf("StartAgent: %v", err)
				}
				childLLM.release <- struct{}{}
				if !waitTurnDoneOnLoop(t, obs, started.AgentID) {
					t.Fatal("child terminal was not durable")
				}
				_ = receiveBackgroundCompletion(t, parentLLM)
				parentLLM.release <- struct{}{}
				if !waitTurnDoneOnLoop(t, obs, session.ActiveLoopID()) {
					t.Fatal("parent hand-back turn was not durable")
				}
				runtime := restoreListAgentRuntime(t, controller, started.AgentID)
				return lifecycle, store, session, parentLLM, child, session.SessionID(), started.CorrelationID, started.AgentID, runtime, func() { _ = obs.Close() }
			},
			want: tool.DelegateResponseCompleted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lifecycle, store, session, parentLLM, child, sessionID, requestID, childID, runtime, cleanup := tt.setup(t)
			if tt.processed {
				if got := countBackgroundInputs(t, store, sessionID, requestID); got != 1 {
					t.Fatalf("pre-crash completion count = %d, want one", got)
				}
				crashAgentRestoreSession(t, session)
				cleanup()
				restored, err := lifecycle.RestoreSession(context.Background(), sessionID)
				if err != nil {
					t.Fatalf("RestoreSession processed edge: %v", err)
				}
				t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
				waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := restored.WaitIdle(waitCtx); err != nil {
					cancel()
					t.Fatalf("processed restore WaitIdle: %v", err)
				}
				cancel()
				select {
				case duplicate := <-parentLLM.started:
					t.Fatalf("processed completion replayed: %q", duplicate)
				default:
				}
				if got := countBackgroundInputs(t, store, sessionID, requestID); got != 1 {
					t.Fatalf("processed completion count after restore = %d, want one", got)
				}
				restoredController := restored.delegation.controllerFor(restored.ActiveLoopID(), backgroundNode("parent", parentLLM, "child"))
				if got := restoreListAgentRuntime(t, restoredController, childID); got != runtime {
					t.Fatalf("processed restored runtime = %+v, want %+v", got, runtime)
				}
				return
			}
			crashAgentRestoreSession(t, session)
			cleanup()
			restoreAndAssertOneBackgroundCompletion(t, lifecycle, store, parentLLM, child, runtime, sessionID, requestID, childID, tt.want)
		})
	}
}
