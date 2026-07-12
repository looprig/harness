package sessionruntime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

type livePermissionGate struct{ effect atomic.Uint32 }

type ceilingPermissionGate struct{ source ceiling.Source }

type ceilingCapture struct {
	mu      sync.Mutex
	sources []ceiling.Source
}

func (c *ceilingCapture) add(source ceiling.Source) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sources = append(c.sources, source)
}
func (c *ceilingCapture) snapshot() []ceiling.Source {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ceiling.Source(nil), c.sources...)
}

func (g ceilingPermissionGate) Check(context.Context, tool.InvokableTool, string, string) loop.Effect {
	if g.source.Current() == 1 {
		return loop.EffectAutoApprove
	}
	return loop.EffectAsk
}
func (ceilingPermissionGate) Grant(context.Context, string, string, tool.ApprovalScope) error {
	return nil
}

type failChildStartAppender struct {
	enabled atomic.Bool
	err     error
}

type failDelegateAcceptanceAppender struct {
	enabled atomic.Bool
	err     error
}

func (a *failDelegateAcceptanceAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	if _, ok := ev.(event.DelegateRequestAccepted); ok && a.enabled.Load() {
		return 0, a.err
	}
	return 1, nil
}

func (a *failChildStartAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	if _, ok := ev.(event.LoopStarted); ok && a.enabled.Load() {
		return 0, a.err
	}
	return 1, nil
}

func newLivePermissionGate(effect loop.Effect) *livePermissionGate {
	g := &livePermissionGate{}
	g.effect.Store(uint32(effect))
	return g
}
func (g *livePermissionGate) Check(context.Context, tool.InvokableTool, string, string) loop.Effect {
	return loop.Effect(g.effect.Load())
}
func (*livePermissionGate) Grant(context.Context, string, string, tool.ApprovalScope) error {
	return nil
}

// delegation_test.go drives the parent-scoped tool.DelegateController end-to-end against
// REAL child loops (a stub LLM emitting one final message). It exercises the
// security-critical invariants: agent authorization + resolution, mode validation
// before quota, ownership (registry-derived) rejection of siblings/ancestors/unrelated
// loops, the action set per delegation style, quota reservation before construction, and
// the wait:true / wait:false→wait request correlation.

func delegateParent(style loop.DelegationStyle, delegates ...identity.AgentName) loop.Definition {
	return mustDefine(
		loop.WithName("parent"),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk("parent")}}, validModel("parent")),
		loop.WithDelegates(delegates...),
		loop.WithDelegation(loop.Delegation{Style: style}),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

func delegateChild(name, finalText string) loop.Definition {
	return mustDefine(
		loop.WithName(identity.AgentName(name)),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk(finalText)}}, validModel(name)),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

func delegateBlockingChild(name string) loop.Definition {
	return mustDefine(
		loop.WithName(identity.AgentName(name)),
		loop.WithInference(&stubLLM{blockUntilCancel: true}, validModel(name)),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

type releasedFailureLLM struct {
	started chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
}

func (l *releasedFailureLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("releasedFailureLLM.Invoke not used")
}
func (l *releasedFailureLLM) Stream(context.Context, inference.Request) (*inference.StreamReader[content.Chunk], error) {
	return inference.NewStreamReader(func() (content.Chunk, error) {
		l.once.Do(func() { close(l.started) })
		<-l.release
		return nil, l.err
	}, nil), nil
}

func delegateChildWithModes(name, finalText string) loop.Definition {
	return mustDefine(
		loop.WithName(identity.AgentName(name)),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk(finalText)}}, validModel(name)),
		loop.WithModes(
			loop.Mode{Name: "build", Effort: inference.EffortHigh, Instructions: "build-i"},
			loop.Mode{Name: "review", Effort: inference.EffortLow, Instructions: "review-i"},
		),
		loop.WithInitialMode("build"),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

func newDelegationSession(t *testing.T, parent loop.Definition, options []Option, children ...loop.Definition) *Session {
	t.Helper()
	defs := append([]loop.Definition{parent}, children...)
	topo := Topology{Definitions: defs, Primers: []identity.AgentName{parent.Name()}, ActivePrimer: parent.Name()}
	opts := append([]Option{WithFingerprintProvider(testFingerprintProvider)}, options...)
	s, err := newSessionTopology(context.Background(), topo, uuid.New, time.Now, opts...)
	if err != nil {
		t.Fatalf("newSessionTopology: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

func delegateCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func requestIDPtr(id uuid.UUID) *uuid.UUID { return &id }

// TestDelegateStartSyncReturnsChildText proves the synchronous start path: the scoped
// controller spawns the authorized child, drives one turn, and returns its final text.
// The child is registered as owned by the parent.
func TestDelegateStartSyncReturnsChildText(t *testing.T) {
	t.Parallel()
	s := newDelegationSession(t, delegateParent(loop.DelegationManaged, "child"), nil, delegateChild("child", "child final"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), delegateParent(loop.DelegationManaged, "child"))

	res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: true})
	if err != nil {
		t.Fatalf("Execute(start) error = %v", err)
	}
	if res.Status != tool.DelegateStatusCompleted {
		t.Errorf("status = %v, want Completed", res.Status)
	}
	if res.Output != "child final" {
		t.Errorf("output = %q, want %q", res.Output, "child final")
	}
	if res.DelegateID.IsZero() {
		t.Fatal("delegate id is zero")
	}
	s.loopsMu.RLock()
	handle, ok := s.loops[res.DelegateID]
	s.loopsMu.RUnlock()
	if !ok || handle.parent.LoopID != s.PrimaryLoopID() {
		t.Errorf("child not registered as owned by parent %v", s.PrimaryLoopID())
	}
}

// TestDelegateStartValidation covers the boundary refusals that must NOT spawn: an
// unauthorized agent, an agent not in the topology, and an undeclared mode.
func TestDelegateStartValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  tool.DelegateRequest
		kind DelegateErrorKind
	}{
		{name: "unauthorized agent", req: tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "stranger", Message: "m", Wait: true}, kind: DelegateUnauthorizedAgent},
		{name: "unknown agent not in topology", req: tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "ghost", Message: "m", Wait: true}, kind: DelegateUnknownAgent},
		{name: "undeclared mode", req: tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Mode: "nope", Message: "m", Wait: true}, kind: DelegateUnknownMode},
	}
	// The parent authorizes "child" and "ghost", but only "child" resolves in the topology.
	parent := delegateParent(loop.DelegationManaged, "child", "ghost")
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newDelegationSession(t, parent, nil, delegateChild("child", "final"))
			ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
			before := s.spawnedCount()
			_, err := ctrl.Execute(delegateCtx(t), tt.req)
			var de *DelegateError
			if !errors.As(err, &de) || de.Kind != tt.kind {
				t.Fatalf("error = %v, want DelegateError kind %d", err, tt.kind)
			}
			if got := s.spawnedCount(); got != before {
				t.Errorf("spawned count = %d, want unchanged %d (no spawn on refusal)", got, before)
			}
		})
	}
}

// TestDelegateActionSetEnforcement proves the parent-scoped controller re-enforces the
// action set independent of crafted JSON: a sync-only parent's controller rejects every
// managed action, while a managed controller admits them.
func TestDelegateActionSetEnforcement(t *testing.T) {
	t.Parallel()
	s := newDelegationSession(t, delegateParent(loop.DelegationManaged, "child"), nil, delegateChild("child", "final"))
	del := requestIDPtr(mustUUID())
	managedOnly := []tool.DelegateOperation{tool.DelegateSend, tool.DelegateWait, tool.DelegateInterrupt, tool.DelegateStatus}
	for _, op := range managedOnly {
		op := op
		syncCtrl := s.delegation.controllerFor(s.PrimaryLoopID(), delegateParent(loop.DelegationSyncOnly, "child"))
		_, err := syncCtrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: op, DelegateID: *del, RequestID: del})
		var de *DelegateError
		if !errors.As(err, &de) || de.Kind != DelegateActionUnavailable {
			t.Fatalf("sync-only op %v error = %v, want DelegateActionUnavailable", op, err)
		}
	}

	syncCtrl := s.delegation.controllerFor(s.PrimaryLoopID(), delegateParent(loop.DelegationSyncOnly, "child"))
	_, err := syncCtrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "m", Wait: false})
	var de *DelegateError
	if !errors.As(err, &de) || de.Kind != DelegateActionUnavailable {
		t.Fatalf("sync-only wait:false start error = %v, want DelegateActionUnavailable", err)
	}
}

// TestDelegateOwnershipRejection proves a scoped controller addresses ONLY children of
// its bound parent: an owned child is addressable, but a controller bound to a different
// parent rejects it as not owned.
func TestDelegateOwnershipRejection(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "final"))
	owner := s.delegation.controllerFor(s.PrimaryLoopID(), parent)

	res, err := owner.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	childID := res.DelegateID

	// A controller bound to an unrelated parent loop id owns nothing here.
	stranger := s.delegation.controllerFor(mustUUID(), parent)
	tests := []struct {
		name string
		req  tool.DelegateRequest
	}{
		{name: "send", req: tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: childID, Message: "m"}},
		{name: "interrupt", req: tool.DelegateRequest{Operation: tool.DelegateInterrupt, DelegateID: childID}},
		{name: "status", req: tool.DelegateRequest{Operation: tool.DelegateStatus, DelegateID: childID}},
		{name: "wait", req: tool.DelegateRequest{Operation: tool.DelegateWait, DelegateID: childID, RequestID: requestIDPtr(mustUUID())}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := stranger.Execute(delegateCtx(t), tt.req)
			var de *DelegateError
			if !errors.As(err, &de) || de.Kind != DelegateNotOwned {
				t.Fatalf("error = %v, want DelegateNotOwned", err)
			}
		})
	}

	// The real owner CAN interrupt its child.
	if _, err := owner.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateInterrupt, DelegateID: childID}); err != nil {
		t.Fatalf("owner interrupt error = %v", err)
	}
}

// TestDelegateModeSelectiveStart proves a supplied valid mode starts the child DIRECTLY
// in that mode (the child's live mode is the selected one), without a synthetic mode
// change.
func TestDelegateModeSelectiveStart(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChildWithModes("child", "final"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)

	res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Mode: "review", Message: "go", Wait: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	handle, ok := s.Loop(res.DelegateID)
	if !ok {
		t.Fatal("child not registered")
	}
	if handle.Mode() != "review" {
		t.Errorf("child live mode = %q, want review (started directly in the selected mode)", handle.Mode())
	}
}

func TestDelegateCatalogDerivesAllowedModes(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	topology := Topology{Definitions: []loop.Definition{parent, delegateChildWithModes("child", "final")}}
	manager := newDelegationManager(topology)
	defs := delegateExtraTools(parent, manager)
	if len(defs) != 1 {
		t.Fatalf("delegateExtraTools length = %d, want 1", len(defs))
	}
	built, err := defs[0].Build(context.Background(), tool.Bindings{
		SessionID: mustUUID(), LoopID: mustUUID(), Delegate: manager.controllerFor(mustUUID(), parent),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	info, err := built[0].Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	for _, want := range []string{`"child"`, `"build"`, `"review"`} {
		if !strings.Contains(string(info.Schema), want) {
			t.Errorf("schema missing %s: %s", want, info.Schema)
		}
	}
}

func TestFoldDelegateTerminalUsesOnlyTurnDoneMessage(t *testing.T) {
	t.Parallel()
	requestID, turnID, childID := mustUUID(), mustUUID(), mustUUID()
	index := foldDelegateTerminals([]event.Event{
		event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
		event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}, Messages: content.AgenticMessages{aiMessage("progress")}},
		event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}},
	})
	got, ok := index[requestID]
	if !ok {
		t.Fatal("correlated terminal missing")
	}
	if got.text != "" || got.status != tool.DelegateStatusCompleted {
		t.Fatalf("terminal = %+v, want empty completed answer", got)
	}
}

func TestCrashClosureReseedsInterruptedDelegateRequest(t *testing.T) {
	t.Parallel()
	requestID, turnID, childID := mustUUID(), mustUUID(), mustUUID()
	original := []event.Event{
		event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}}, InitialRequestID: requestID},
		event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
	}
	closure := event.TurnInterrupted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}}
	manager := newDelegationManager(Topology{})
	cmd := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyMachine}, NoFold: true, TargetLoopID: childID}
	if err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), childID, cmd)}, original, []event.Event{closure}); err != nil {
		t.Fatal(err)
	}
	got, ok := manager.getResolved(requestID)
	if !ok || got.childID != childID || got.status != tool.DelegateStatusInterrupted {
		t.Fatalf("resolved = %+v, %v; want interrupted child %v", got, ok, childID)
	}
}

func TestRestoreIgnoresUnacceptedDelegateIntent(t *testing.T) {
	t.Parallel()
	requestID, childID := mustUUID(), mustUUID()
	cmd := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyMachine}, NoFold: true, TargetLoopID: childID}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), uuid.UUID{}, cmd)}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := manager.getResolved(requestID); ok {
		t.Fatalf("unaccepted intent admitted: %+v", got)
	}
}

func TestRestoreDoesNotAdmitOrdinaryTurnTerminalAsDelegateRequest(t *testing.T) {
	t.Parallel()
	requestID, turnID, childID := mustUUID(), mustUUID(), mustUUID()
	ordinary := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyUser}}
	events := []event.Event{
		event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}, Cause: identity.Cause{CommandID: requestID}}},
		event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID, TurnID: turnID}}, Message: aiMessage("ordinary answer")},
	}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), childID, ordinary)}, events, nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := manager.getResolved(requestID); ok {
		t.Fatalf("ordinary user request was admitted as delegate result: %+v", got)
	}
}

func TestRestoreOverlaysAdmittedQueuedCancellationReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		reason event.CancelReason
		status tool.DelegateStatusValue
	}{
		{name: "interrupted", reason: event.CancelTurnInterrupted, status: tool.DelegateStatusInterrupted},
		{name: "failed", reason: event.CancelTurnFailed, status: tool.DelegateStatusFailed},
		{name: "client retracted", reason: event.CancelClientRetracted, status: tool.DelegateStatusInterrupted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestID, childID := mustUUID(), mustUUID()
			cmd := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyMachine}, NoFold: true, TargetLoopID: childID}
			events := []event.Event{
				event.DelegateRequestAccepted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}},
				event.InputCancelled{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}, Reason: tt.reason},
			}
			manager := newDelegationManager(Topology{})
			if err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), childID, cmd)}, events, nil); err != nil {
				t.Fatal(err)
			}
			got, ok := manager.getResolved(requestID)
			if !ok || got.childID != childID || got.status != tt.status {
				t.Fatalf("resolved = %+v, %v; want child=%v status=%v", got, ok, childID, tt.status)
			}
		})
	}
}

func TestRestoreIgnoresUnadmittedQueuedCancellation(t *testing.T) {
	t.Parallel()
	requestID, childID := mustUUID(), mustUUID()
	ordinary := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyUser}}
	events := []event.Event{event.InputCancelled{Header: event.Header{Coordinates: identity.Coordinates{LoopID: childID}, Cause: identity.Cause{CommandID: requestID}}, Reason: event.CancelTurnFailed}}
	manager := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), childID, ordinary)}, events, nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := manager.getResolved(requestID); ok {
		t.Fatalf("ordinary cancellation entered delegate index: %+v", got)
	}
}

func TestRestoreRejectsDelegateIntentRouteMismatch(t *testing.T) {
	t.Parallel()
	requestID, target, wrong := mustUUID(), mustUUID(), mustUUID()
	cmd := command.UserInput{Header: command.Header{CommandID: requestID, Agency: identity.AgencyMachine}, NoFold: true, TargetLoopID: target}
	manager := newDelegationManager(Topology{})
	err := seedResolvedDelegateRecords(manager, []journal.JournalRecord{journal.NewCommandRecord(mustUUID(), wrong, cmd)}, nil, nil)
	var mismatch *journal.CommandRouteMismatchError
	if !errors.As(err, &mismatch) || mismatch.RecordLoopID != wrong || mismatch.TargetLoopID != target {
		t.Fatalf("error = %T %+v, want typed route mismatch", err, err)
	}
}

// TestDelegateWaitFalseThenWaitResolves proves the request correlation: a wait:false
// start returns a queued request id, and a later wait for that id resolves the SAME
// request's answer.
func TestDelegateWaitFalseThenWaitResolves(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "async final"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)

	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: false})
	if err != nil {
		t.Fatalf("start wait:false: %v", err)
	}
	if queued.Status != tool.DelegateStatusQueued {
		t.Fatalf("status = %v, want Queued", queued.Status)
	}
	if queued.RequestID.IsZero() || queued.DelegateID.IsZero() {
		t.Fatalf("queued handle missing ids: %+v", queued)
	}
	pending, ok := s.delegation.getPending(queued.RequestID)
	if !ok {
		t.Fatal("queued request was not registered")
	}
	select {
	case <-pending.done:
	case <-time.After(2 * time.Second):
		t.Fatal("queued request did not resolve")
	}
	status, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStatus, DelegateID: queued.DelegateID})
	if err != nil {
		t.Fatalf("status before collection: %v", err)
	}
	if status.Status != tool.DelegateStatusIdle || status.PendingRequests != 1 {
		t.Fatalf("resolved-uncollected status = %v pending=%d, want Idle pending=1", status.Status, status.PendingRequests)
	}

	resolved, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{
		Operation:  tool.DelegateWait,
		DelegateID: queued.DelegateID,
		RequestID:  requestIDPtr(queued.RequestID),
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if resolved.Status != tool.DelegateStatusCompleted {
		t.Errorf("status = %v, want Completed", resolved.Status)
	}
	if resolved.Output != "async final" {
		t.Errorf("output = %q, want %q", resolved.Output, "async final")
	}

	// An unknown request id for the same owned child is rejected.
	_, err = ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateWait, DelegateID: queued.DelegateID, RequestID: requestIDPtr(mustUUID())})
	var de *DelegateError
	if !errors.As(err, &de) || de.Kind != DelegateUnknownRequest {
		t.Fatalf("wait unknown request error = %v, want DelegateUnknownRequest", err)
	}
}

func TestDelegateWaitTimeoutInterruptsRunningChild(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	result, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateWait, DelegateID: queued.DelegateID, RequestID: requestIDPtr(queued.RequestID), TimeoutSeconds: &zero})
	if err != nil || result.Status != tool.DelegateStatusTimedOut {
		t.Fatalf("timed wait = %+v, %v; want TimedOut", result, err)
	}
	pending, ok := s.delegation.getPending(queued.RequestID)
	if !ok {
		t.Fatal("pending request disappeared after timeout")
	}
	select {
	case <-pending.done:
		_, status := pending.result()
		if status != tool.DelegateStatusInterrupted {
			t.Fatalf("post-timeout terminal = %v, want Interrupted", status)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timed-out wait did not interrupt the child")
	}
}

func TestDelegateQueuedRequestTimeoutCancelsOnlyThatRequest(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	active, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "A", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	waitDelegateMechanicalStatus(t, ctrl, active.DelegateID, tool.DelegateStatusRunning)
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: active.DelegateID, Message: "B", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	timed, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateWait, DelegateID: active.DelegateID, RequestID: requestIDPtr(queued.RequestID), TimeoutSeconds: &zero})
	if err != nil || timed.Status != tool.DelegateStatusTimedOut {
		t.Fatalf("timed wait = %+v, %v; want TimedOut", timed, err)
	}
	pending, ok := s.delegation.getPending(queued.RequestID)
	if !ok {
		t.Fatal("timed request was not left collectable")
	}
	select {
	case <-pending.done:
		_, status := pending.result()
		if status != tool.DelegateStatusInterrupted {
			t.Fatalf("queued cancellation status = %v, want Interrupted", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued request-specific timeout did not resolve")
	}
	status, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, DelegateID: active.DelegateID})
	if err != nil || status.Status != tool.DelegateStatusRunning {
		t.Fatalf("A status after B timeout = %+v, %v; want Running", status, err)
	}
	_, _ = ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateInterrupt, DelegateID: active.DelegateID})
}

func TestDelegateQueuedRequestCallerCancellationTargetsOnlyThatRequest(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	active, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "A", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	waitDelegateMechanicalStatus(t, ctrl, active.DelegateID, tool.DelegateStatusRunning)
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: active.DelegateID, Message: "B", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := ctrl.Execute(cancelledCtx, tool.DelegateRequest{Operation: tool.DelegateWait, DelegateID: active.DelegateID, RequestID: requestIDPtr(queued.RequestID)})
	if err != nil || result.Status != tool.DelegateStatusInterrupted {
		t.Fatalf("cancelled wait = %+v, %v; want Interrupted", result, err)
	}
	pending, _ := s.delegation.getPending(queued.RequestID)
	select {
	case <-pending.done:
	case <-time.After(2 * time.Second):
		t.Fatal("caller cancellation did not resolve targeted queued request")
	}
	status, _ := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, DelegateID: active.DelegateID})
	if status.Status != tool.DelegateStatusRunning {
		t.Fatalf("A status after B caller cancellation = %v, want Running", status.Status)
	}
	_, _ = ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateInterrupt, DelegateID: active.DelegateID})
}

func TestDelegateRunningRequestTimeoutStartsNextWithoutCancellingIt(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	running, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "B", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	waitDelegateMechanicalStatus(t, ctrl, running.DelegateID, tool.DelegateStatusRunning)
	next, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: running.DelegateID, Message: "C", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	result, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateWait, DelegateID: running.DelegateID, RequestID: requestIDPtr(running.RequestID), TimeoutSeconds: &zero})
	if err != nil || result.Status != tool.DelegateStatusTimedOut {
		t.Fatalf("running timeout = %+v, %v; want TimedOut", result, err)
	}
	runningPending, _ := s.delegation.getPending(running.RequestID)
	select {
	case <-runningPending.done:
		_, status := runningPending.result()
		if status != tool.DelegateStatusInterrupted {
			t.Fatalf("running request terminal = %v, want Interrupted", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("running request did not resolve after targeted timeout")
	}
	waitDelegateMechanicalStatus(t, ctrl, running.DelegateID, tool.DelegateStatusRunning)
	nextPending, ok := s.delegation.getPending(next.RequestID)
	if !ok {
		t.Fatal("next request handle missing")
	}
	select {
	case <-nextPending.done:
		t.Fatal("targeting running B also resolved/cancelled C")
	default:
	}
	_, _ = s.cancelDelegateRequest(running.DelegateID, next.RequestID)
}

func TestDelegateTerminalWinsCancelledWaitBeforeControlDispatch(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "B done"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	request, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "B", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := s.delegation.getPending(request.RequestID)
	select {
	case <-pending.done:
	case <-time.After(2 * time.Second):
		t.Fatal("B did not reach terminal barrier")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := ctrl.Execute(cancelled, tool.DelegateRequest{Operation: tool.DelegateWait, DelegateID: request.DelegateID, RequestID: requestIDPtr(request.RequestID)})
	if err != nil || result.Status != tool.DelegateStatusCompleted || result.Output != "B done" {
		t.Fatalf("terminal/deadline race = %+v, %v; want completed B", result, err)
	}
	next, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: request.DelegateID, Message: "C", Wait: true})
	if err != nil || next.Status != tool.DelegateStatusCompleted {
		t.Fatalf("C after terminal race = %+v, %v", next, err)
	}
}

func TestNativeQueuedDelegateInterruptResolvesWithoutWaitTimeout(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	active, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "A", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	waitDelegateMechanicalStatus(t, ctrl, active.DelegateID, tool.DelegateStatusRunning)
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: active.DelegateID, Message: "B", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateInterrupt, DelegateID: active.DelegateID}); err != nil {
		t.Fatal(err)
	}
	result := make(chan tool.DelegateResult, 1)
	errCh := make(chan error, 1)
	go func() {
		got, waitErr := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateWait, DelegateID: active.DelegateID, RequestID: requestIDPtr(queued.RequestID)})
		result <- got
		errCh <- waitErr
	}()
	select {
	case got := <-result:
		if waitErr := <-errCh; waitErr != nil || got.Status != tool.DelegateStatusInterrupted {
			t.Fatalf("wait = %+v, %v; want Interrupted", got, waitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background wait depended on a context timeout after queued cancellation")
	}
}

func TestNativeQueuedDelegateFailedTurnResolvesFailedLiveAndRestore(t *testing.T) {
	t.Parallel()
	providerErr := errors.New("injected active-turn failure")
	client := &releasedFailureLLM{started: make(chan struct{}), release: make(chan struct{}), err: providerErr}
	parent := delegateParent(loop.DelegationManaged, "child")
	child := mustDefine(
		loop.WithName("child"),
		loop.WithInference(client, validModel("child")),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
	events := &recordingEventAppender{}
	commands := &fakeCommandAppender{}
	s := newDelegationSession(t, parent, []Option{WithEventAppender(events), WithCommandAppender(commands)}, child)
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	active, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "A", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.started:
	case <-time.After(2 * time.Second):
		t.Fatal("active turn never reached injected provider")
	}
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: active.DelegateID, Message: "B", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	close(client.release)

	live, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateWait, DelegateID: active.DelegateID, RequestID: requestIDPtr(queued.RequestID)})
	if err != nil || live.Status != tool.DelegateStatusFailed {
		t.Fatalf("live wait = %+v, %v; want Failed", live, err)
	}

	records := commands.snapshot()
	journalRecords := make([]journal.JournalRecord, len(records))
	for i := range records {
		journalRecords[i] = records[i]
	}
	restored := newDelegationManager(Topology{})
	if err := seedResolvedDelegateRecords(restored, journalRecords, events.snapshot(), nil); err != nil {
		t.Fatal(err)
	}
	resolved, ok := restored.getResolved(queued.RequestID)
	if !ok || resolved.childID != active.DelegateID || resolved.status != tool.DelegateStatusFailed {
		t.Fatalf("restored cancellation = %+v, %v; want Failed child %v", resolved, ok, active.DelegateID)
	}
}

func waitDelegateMechanicalStatus(t *testing.T, ctrl tool.DelegateController, delegateID uuid.UUID, want tool.DelegateStatusValue) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		status, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, DelegateID: delegateID})
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("delegate status = %v, want %v", status.Status, want)
		case <-time.After(time.Millisecond):
		}
	}
}

// TestDelegateStatusReportsMechanicalState proves status returns bounded mechanical
// state + pending counts for a single owned child and for all owned children.
func TestDelegateStatusReportsMechanicalState(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "final"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)

	res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	single, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStatus, DelegateID: res.DelegateID})
	if err != nil {
		t.Fatalf("status one: %v", err)
	}
	if single.Status != tool.DelegateStatusIdle {
		t.Errorf("single status = %v, want Idle (child finished its turn, no pending request)", single.Status)
	}
	if single.PendingRequests != 0 {
		t.Errorf("pending = %d, want 0", single.PendingRequests)
	}

	all, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStatus})
	if err != nil {
		t.Fatalf("status all: %v", err)
	}
	if len(all.Children) != 1 || all.Children[0].DelegateID != res.DelegateID {
		t.Errorf("children = %+v, want exactly the one owned child", all.Children)
	}
}

func TestDelegateStatusReportsWaitTrueChildRunning(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateBlockingChild("child"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	startCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ctrl.Execute(startCtx, tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: true})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if len(status.Children) == 1 {
			if status.Children[0].Status != tool.DelegateStatusRunning {
				t.Fatalf("active wait:true child status = %v, want Running", status.Children[0].Status)
			}
			cancel()
			<-done
			return
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("child was never registered")
}

func TestDelegateChildPermissionIsAttenuatedByLiveParent(t *testing.T) {
	t.Parallel()
	parentGate := newLivePermissionGate(loop.EffectAsk)
	childGate := newLivePermissionGate(loop.EffectAutoApprove)
	parent := mustDefine(
		loop.WithName("parent"),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk("parent")}}, validModel("parent")),
		loop.WithDelegates("child"), loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
		loop.WithPolicyRevision("parent-permission"),
		loop.WithPermissionFactory(func(context.Context, tool.Bindings) (loop.PermissionGate, error) { return parentGate, nil }),
	)
	child := mustDefine(
		loop.WithName("child"),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk("done")}}, validModel("child")),
		loop.WithPolicyRevision("child-permission"),
		loop.WithPermissionFactory(func(context.Context, tool.Bindings) (loop.PermissionGate, error) { return childGate, nil }),
	)
	s := newDelegationSession(t, parent, nil, child)
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	s.loopsMu.RLock()
	permission := s.loops[res.DelegateID].bound.Permission()
	s.loopsMu.RUnlock()
	if got := permission.Check(context.Background(), nil, "Bash", `{}`); got != loop.EffectAsk {
		t.Fatalf("permissive child under Ask parent = %v, want Ask", got)
	}
	parentGate.effect.Store(uint32(loop.EffectDeny))
	if got := permission.Check(context.Background(), nil, "Bash", `{}`); got != loop.EffectDeny {
		t.Fatalf("live parent tightening = %v, want Deny", got)
	}
}

func TestDelegatePermissionFactoriesShareLiveSessionCeiling(t *testing.T) {
	t.Parallel()
	var parentSource, childSource ceiling.Source
	parent := mustDefine(
		loop.WithName("parent"), loop.WithInference(&stubLLM{}, validModel("parent")),
		loop.WithDelegates("child"), loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}),
		loop.WithPolicyRevision("parent-ceiling"),
		loop.WithPermissionFactory(func(_ context.Context, bindings tool.Bindings) (loop.PermissionGate, error) {
			parentSource = bindings.Ceiling
			return ceilingPermissionGate{source: bindings.Ceiling}, nil
		}),
	)
	child := mustDefine(
		loop.WithName("child"), loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk("done")}}, validModel("child")),
		loop.WithPolicyRevision("child-ceiling"),
		loop.WithPermissionFactory(func(_ context.Context, bindings tool.Bindings) (loop.PermissionGate, error) {
			childSource = bindings.Ceiling
			return ceilingPermissionGate{source: bindings.Ceiling}, nil
		}),
	)
	s := newDelegationSession(t, parent, nil, child)
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if parentSource == nil || parentSource != childSource || parentSource != s.CeilingSource() {
		t.Fatalf("ceiling sources parent=%p child=%p session=%p, want exact same source", parentSource, childSource, s.CeilingSource())
	}
	s.loopsMu.RLock()
	permission := s.loops[res.DelegateID].bound.Permission()
	s.loopsMu.RUnlock()
	if got := permission.Check(context.Background(), nil, "Bash", `{}`); got != loop.EffectAsk {
		t.Fatalf("level0 = %v, want Ask", got)
	}
	if err := s.SetSecurityCeiling(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := permission.Check(context.Background(), nil, "Bash", `{}`); got != loop.EffectAutoApprove {
		t.Fatalf("level1 = %v, want AutoApprove", got)
	}
}

func TestPermissionCeilingIsSharedOnRestoreAndIsolatedAcrossSessions(t *testing.T) {
	t.Parallel()
	parents, children := &ceilingCapture{}, &ceilingCapture{}
	parent := mustDefine(
		loop.WithName("parent"), loop.WithInference(&stubLLM{}, validModel("parent")), loop.WithDelegates("child"),
		loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}), loop.WithPolicyRevision("p-ceiling"),
		loop.WithPermissionFactory(func(_ context.Context, b tool.Bindings) (loop.PermissionGate, error) {
			parents.add(b.Ceiling)
			return ceilingPermissionGate{b.Ceiling}, nil
		}),
	)
	child := mustDefine(
		loop.WithName("child"), loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk("done")}}, validModel("child")), loop.WithPolicyRevision("c-ceiling"),
		loop.WithPermissionFactory(func(_ context.Context, b tool.Bindings) (loop.PermissionGate, error) {
			children.add(b.Ceiling)
			return ceilingPermissionGate{b.Ceiling}, nil
		}),
	)
	store := newRestoreStore(t)
	topo := Topology{Definitions: []loop.Definition{parent, child}, Primers: []identity.AgentName{"parent"}, ActivePrimer: "parent"}
	lc, err := NewTopologyLifecycle(topo, store, WithLifecycleFingerprintProvider(testFingerprintProvider))
	if err != nil {
		t.Fatal(err)
	}
	original, err := lc.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctrl := original.delegation.controllerFor(original.PrimaryLoopID(), parent)
	if _, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: true}); err != nil {
		t.Fatal(err)
	}
	if err := original.SetSecurityCeiling(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	sid := original.SessionID()
	if err := original.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := lc.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Shutdown(context.Background()) }()
	p, c := parents.snapshot(), children.snapshot()
	if len(p) != 2 || len(c) != 2 || p[0] != c[0] || p[1] != c[1] || p[0] == p[1] || p[1] != restored.CeilingSource() || p[1].Current() != 1 {
		t.Fatalf("sources parent=%v child=%v restored=%p level=%d", p, c, restored.CeilingSource(), restored.CeilingSource().Current())
	}
	separate, err := lc.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = separate.Shutdown(context.Background()) }()
	p = parents.snapshot()
	if len(p) != 3 || p[2] == p[0] || p[2] == p[1] || p[2] != separate.CeilingSource() {
		t.Fatalf("separate session reused ceiling: %v", p)
	}
}

func TestDelegateStartSetupFailuresLeaveNoChildQuotaOrDurablePhantom(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("injected delegate setup failure")
	tests := []struct {
		name   string
		inject func(*Session)
	}{
		{name: "subscribe failure", inject: func(s *Session) {
			s.delegateSubscribe = func(event.EventFilter) (event.Subscription, error) { return nil, sentinel }
		}},
		{name: "initial enqueue failure", inject: func(s *Session) {
			s.delegateEnqueue = func(context.Context, loop.Backend, command.UserInput) error { return sentinel }
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recordingEventAppender{}
			parent := delegateParent(loop.DelegationManaged, "child")
			s := newDelegationSession(t, parent, []Option{WithEventAppender(rec)}, delegateChild("child", "answer"))
			tt.inject(s)
			beforeQuota := s.spawnedCount()
			ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent).(*scopedController)
			beforeLoops := len(ctrl.ownedChildren(s))
			_, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: false})
			if !errors.Is(err, sentinel) {
				t.Fatalf("start error = %v, want injected sentinel", err)
			}
			if got := s.spawnedCount(); got != beforeQuota {
				t.Fatalf("spawned quota = %d, want rolled back %d", got, beforeQuota)
			}
			if got := len(ctrl.ownedChildren(s)); got != beforeLoops {
				t.Fatalf("owned children = %d, want %d", got, beforeLoops)
			}
			for _, ev := range rec.snapshot() {
				if started, ok := ev.(event.LoopStarted); ok && started.Cause.Coordinates.LoopID == s.PrimaryLoopID() {
					t.Fatalf("failed spawn durably published child LoopStarted: %+v", started)
				}
			}
		})
	}
}

func TestDelegateStartCommitsLoopStartedBeforeTurnEvents(t *testing.T) {
	t.Parallel()
	rec := &recordingEventAppender{}
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, []Option{WithEventAppender(rec)}, delegateChild("child", "answer"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	queued, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events := rec.snapshot()
		started, turn := -1, -1
		for i, ev := range events {
			switch e := ev.(type) {
			case event.LoopStarted:
				if e.LoopID == queued.DelegateID {
					started = i
				}
			case event.TurnStarted:
				if e.LoopID == queued.DelegateID {
					turn = i
				}
			}
		}
		if turn >= 0 {
			if started < 0 || started >= turn {
				t.Fatalf("event order started=%d turn=%d: %#v", started, turn, events)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("child TurnStarted not observed")
}

func TestDelegateStartAppendFailureRollsBackPreparedChild(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("child LoopStarted append failed")
	parent := delegateParent(loop.DelegationManaged, "child")
	appender := &failChildStartAppender{err: sentinel}
	s := newDelegationSession(t, parent, []Option{WithEventAppender(appender)}, delegateChild("child", "answer"))
	// The root LoopStarted has already committed. Fail exactly the next child creation
	// commit without replacing the live session hub beneath running loop publishers.
	appender.enabled.Store(true)
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent).(*scopedController)
	beforeQuota := s.spawnedCount()
	_, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: false})
	if !errors.Is(err, sentinel) {
		t.Fatalf("start error = %v, want append sentinel", err)
	}
	if s.spawnedCount() != beforeQuota || len(ctrl.ownedChildren(s)) != 0 {
		t.Fatalf("failed durable commit left quota=%d children=%d", s.spawnedCount(), len(ctrl.ownedChildren(s)))
	}
}

func TestDelegateRequiredIntentAppendFailureDoesNotDispatch(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("delegate intent append failed")
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "answer"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent).(*scopedController)
	failing := &fakeCommandAppender{err: sentinel}
	s.cmdAppender = failing
	before := s.spawnedCount()
	_, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: false})
	var sessionErr *SessionError
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionDelegateIntentAppendFailed || !errors.Is(err, sentinel) {
		t.Fatalf("start error = %T %v, want typed required-intent failure", err, err)
	}
	if s.spawnedCount() != before || len(ctrl.ownedChildren(s)) != 0 {
		t.Fatalf("failed start left quota=%d children=%d", s.spawnedCount(), len(ctrl.ownedChildren(s)))
	}

	s.cmdAppender = &fakeCommandAppender{}
	started, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	s.cmdAppender = failing
	_, err = ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: started.DelegateID, Message: "queued", Wait: false})
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionDelegateIntentAppendFailed || !errors.Is(err, sentinel) {
		t.Fatalf("send error = %T %v, want typed required-intent failure", err, err)
	}
	if got := s.delegation.pendingCount(started.DelegateID); got != 0 {
		t.Fatalf("failed send pending count = %d, want 0", got)
	}
}

func TestDelegateAcceptanceAppendFailureReturnsNoHandle(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("delegate acceptance append failed")
	parent := delegateParent(loop.DelegationManaged, "child")
	appender := &failDelegateAcceptanceAppender{err: sentinel}
	s := newDelegationSession(t, parent, []Option{WithEventAppender(appender)}, delegateChild("child", "answer"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	started, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "A", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	appender.enabled.Store(true)
	result, err := ctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: started.DelegateID, Message: "B", Wait: false})
	var sessionErr *SessionError
	if err == nil || !result.RequestID.IsZero() || !errors.Is(err, sentinel) || !errors.As(err, &sessionErr) || sessionErr.Kind != SessionDelegateAdmissionCommitFailed {
		t.Fatalf("send = %+v, %v; want no handle and acceptance failure", result, err)
	}
	if got := s.delegation.pendingCount(started.DelegateID); got != 0 {
		t.Fatalf("pending=%d, want 0", got)
	}
}

// TestDelegateQuotaReservedBeforeConstruction proves the cumulative spawn quota is
// enforced by the shared NewLoop reservation (before the child is constructed), and that
// a pre-spawn refusal (invalid mode) does not consume a quota slot.
func TestDelegateQuotaReservedBeforeConstruction(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, []Option{WithLimits(Limits{Depth: 3, Quota: 1})}, delegateChildWithModes("child", "final"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)

	// An invalid mode is refused BEFORE reserving quota.
	if _, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Mode: "ghost", Message: "m", Wait: true}); err == nil {
		t.Fatal("expected an invalid-mode refusal")
	}

	// The first real spawn consumes the sole quota slot.
	if _, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "m", Wait: true}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	// The second exceeds the quota.
	_, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "m", Wait: true})
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopQuotaExceeded {
		t.Fatalf("second start error = %v, want SessionLoopQuotaExceeded", err)
	}
}

func (s *Session) spawnedCount() int {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	return s.spawned
}

// waitTurnDoneOnLoop reads the observer until a TurnDone for loopID arrives (the child's
// turn completed and durably persisted) or the deadline elapses.
func waitTurnDoneOnLoop(t *testing.T, sub interface {
	Events() <-chan event.Delivery
}, loopID [16]byte) bool {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case d, ok := <-sub.Events():
			if !ok {
				return false
			}
			if td, ok := d.Event.(event.TurnDone); ok && td.Coordinates.LoopID == loopID {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// TestDelegateSendResolvesDistinctTurns proves each `send` produces its OWN distinct,
// request-correlated turn on an owned child (never a fold): two sequential sends each
// resolve their own answer with a distinct request id. The non-folding guarantee at a
// live tool-continuation boundary is proven at the loop-actor level by
// TestNonFoldingInputStartsOwnTurn.
func TestDelegateSendResolvesDistinctTurns(t *testing.T) {
	t.Parallel()
	parent := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parent, nil, delegateChild("child", "answer"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)

	start, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	childID := start.DelegateID

	seen := map[uuid.UUID]struct{}{start.RequestID: {}}
	for i := 0; i < 2; i++ {
		res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: childID, Message: "again", Wait: true})
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if res.Status != tool.DelegateStatusCompleted || res.Output != "answer" {
			t.Fatalf("send %d = %v/%q, want Completed/answer", i, res.Status, res.Output)
		}
		if _, dup := seen[res.RequestID]; dup || res.RequestID.IsZero() {
			t.Fatalf("send %d request id %v not distinct", i, res.RequestID)
		}
		seen[res.RequestID] = struct{}{}
	}
}

// TestDelegateWaitResolvesAfterRestore proves the plan Step-1 requirement that a
// wait:false request resolves via a later wait INCLUDING after restore: the in-memory
// pending handle does not survive the restart, but restore reconstructs the durable
// request→terminal index so the same request id resolves the same answer.
func TestDelegateWaitResolvesAfterRestore(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	parent := delegateParent(loop.DelegationManaged, "child")
	child := delegateChild("child", "durable answer")
	topo := Topology{Definitions: []loop.Definition{parent, child}, Primers: []identity.AgentName{parent.Name()}, ActivePrimer: parent.Name()}
	lc, err := NewTopologyLifecycle(topo, store, WithLifecycleFingerprintProvider(testFingerprintProvider))
	if err != nil {
		t.Fatalf("NewTopologyLifecycle: %v", err)
	}

	ctx := delegateCtx(t)
	s, err := lc.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Observe all loops BEFORE the spawn so the child's TurnDone (no hub replay) is caught.
	obs, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer func() { _ = obs.Close() }()

	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	queued, err := ctrl.Execute(ctx, tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: false})
	if err != nil {
		t.Fatalf("start wait:false: %v", err)
	}
	childID, reqID := queued.DelegateID, queued.RequestID

	// Wait until the child's turn is durably done before shutdown (so its terminal is on
	// the durable stream the restore reads).
	if !waitTurnDoneOnLoop(t, obs, childID) {
		t.Fatal("child turn never completed before shutdown")
	}
	sid := s.SessionID()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Restore: the in-memory pending map is empty; wait must resolve from the durable index.
	r, err := lc.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })

	rctrl := r.delegation.controllerFor(r.PrimaryLoopID(), parent)
	res, err := rctrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateWait, DelegateID: childID, RequestID: requestIDPtr(reqID)})
	if err != nil {
		t.Fatalf("wait after restore: %v", err)
	}
	if res.Status != tool.DelegateStatusCompleted || res.Output != "durable answer" {
		t.Fatalf("post-restore wait = %v/%q, want Completed/durable answer", res.Status, res.Output)
	}
}

func TestDelegateQueuedRequestRestoresInterruptedWithoutReplay(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	parent := delegateParent(loop.DelegationManaged, "child")
	child := delegateBlockingChild("child")
	topo := Topology{Definitions: []loop.Definition{parent, child}, Primers: []identity.AgentName{parent.Name()}, ActivePrimer: parent.Name()}
	lc, err := NewTopologyLifecycle(topo, store, WithLifecycleFingerprintProvider(testFingerprintProvider))
	if err != nil {
		t.Fatal(err)
	}
	s, err := lc.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	obs, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatal(err)
	}
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parent)
	a, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "A", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	if !waitTurnStartedRequest(t, obs, a.RequestID) {
		t.Fatal("turn A never started")
	}
	b, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: a.DelegateID, Message: "B", Wait: false})
	if err != nil {
		t.Fatal(err)
	}
	if !waitInputQueuedRequest(t, obs, b.RequestID) {
		t.Fatal("request B never durably queued")
	}
	sid := s.SessionID()
	s.sessionCancel() // crash: no graceful queue flush or shutdown command
	s.releaseLease(context.Background())
	_ = obs.Close()

	restored, err := lc.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	restoredCtrl := restored.delegation.controllerFor(restored.PrimaryLoopID(), parent)
	result, err := restoredCtrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateWait, DelegateID: a.DelegateID, RequestID: requestIDPtr(b.RequestID)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != tool.DelegateStatusInterrupted {
		t.Fatalf("restored queued request B status = %v, want Interrupted", result.Status)
	}
	status, err := restoredCtrl.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, DelegateID: a.DelegateID})
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != tool.DelegateStatusIdle {
		t.Fatalf("restored child status = %v, want idle (B not replayed)", status.Status)
	}
}

func waitTurnStartedRequest(t *testing.T, sub interface{ Events() <-chan event.Delivery }, requestID uuid.UUID) bool {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case delivery, ok := <-sub.Events():
			if !ok {
				return false
			}
			if started, ok := delivery.Event.(event.TurnStarted); ok && started.Cause.CommandID == requestID {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func waitInputQueuedRequest(t *testing.T, sub interface{ Events() <-chan event.Delivery }, requestID uuid.UUID) bool {
	t.Helper()
	deadline := time.After(5 * time.Second)
	accepted := false
	for {
		select {
		case delivery, ok := <-sub.Events():
			if !ok {
				return false
			}
			if eventAccepted, ok := delivery.Event.(event.DelegateRequestAccepted); ok && eventAccepted.Cause.CommandID == requestID {
				accepted = true
			}
			if queued, ok := delivery.Event.(event.InputQueued); ok && queued.Cause.CommandID == requestID {
				return accepted
			}
		case <-deadline:
			return false
		}
	}
}
