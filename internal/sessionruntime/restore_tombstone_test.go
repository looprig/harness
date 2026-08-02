package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

func TestAttachRestoredTombstonedLoopPublishesOnceAndExposesClosedStatus(t *testing.T) {
	t.Parallel()
	sessionID, parentID, childID := mustUUID(), mustUUID(), mustUUID()
	s := &Session{
		sessionID:  sessionID,
		sessionCtx: context.Background(),
		factory:    event.NewFactory(uuid.New, time.Now),
		hub:        hub.New(sessionID),
		loops:      make(map[uuid.UUID]*loopHandle),
	}
	manager := newDelegationManager(Topology{})
	manager.attach(s)
	s.delegation = manager

	sub, err := s.hub.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), sessionID, childID)
	plan := loopPlan{
		started: event.LoopStarted{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
			Cause:       identity.Cause{Coordinates: identity.Coordinates{LoopID: parentID}},
		}},
		bound:    bound,
		bindings: tool.Bindings{SessionID: sessionID, LoopID: childID},
	}
	parent := loop.Provenance{LoopID: parentID}
	if err := s.attachRestoredTombstonedLoop(plan, parent); err != nil {
		t.Fatalf("attachRestoredTombstonedLoop: %v", err)
	}
	select {
	case delivery := <-sub.Events():
		tombstone, ok := delivery.Event.(event.LoopRestoreTombstoned)
		if !ok {
			t.Fatalf("event = %T, want LoopRestoreTombstoned", delivery.Event)
		}
		if tombstone.LoopID != childID || tombstone.Category != event.LoopRestoreTombstoneRuntimeMismatch {
			t.Fatalf("tombstone = %+v, want child %v/runtime_mismatch", tombstone, childID)
		}
	case <-time.After(time.Second):
		t.Fatal("tombstone event not published")
	}

	plan.tombstoned = true
	if err := s.attachRestoredTombstonedLoop(plan, parent); err != nil {
		t.Fatalf("second attachRestoredTombstonedLoop: %v", err)
	}
	select {
	case delivery := <-sub.Events():
		t.Fatalf("second attach published unexpected event %T", delivery.Event)
	case <-time.After(20 * time.Millisecond):
	}

	controller := &scopedController{manager: manager, parentLoopID: parentID, style: loop.DelegationManaged}
	status, err := controller.Execute(context.Background(), tool.DelegateRequest{Operation: tool.DelegateStatus, DelegateID: childID})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Status != tool.DelegateStatusFailed {
		t.Fatalf("status = %v, want failed", status.Status)
	}

	for _, operation := range []tool.DelegateOperation{tool.DelegateSend, tool.DelegateWait, tool.DelegateInterrupt} {
		req := tool.DelegateRequest{Operation: operation, DelegateID: childID}
		if operation == tool.DelegateWait {
			requestID := mustUUID()
			req.RequestID = &requestID
		}
		_, err := controller.Execute(context.Background(), req)
		var delegateErr *DelegateError
		if !errors.As(err, &delegateErr) || delegateErr.Kind != DelegateClosed {
			t.Errorf("operation %v error = %v, want DelegateClosed", operation, err)
		}
	}
}

func TestSeedResolvedDelegateRecordsMarksTombstonedRequestsFailed(t *testing.T) {
	t.Parallel()
	requestID, childID := mustUUID(), mustUUID()
	manager := newDelegationManager(Topology{})
	cmd := command.UserInput{
		Header:       command.Header{CommandID: requestID, Agency: identity.AgencyMachine},
		NoFold:       true,
		TargetLoopID: childID,
	}
	accepted := event.LoopStarted{
		Header:           event.Header{Coordinates: identity.Coordinates{LoopID: childID}},
		InitialRequestID: requestID,
	}
	if err := seedResolvedDelegateRecords(
		manager,
		[]journal.JournalRecord{journal.NewCommandRecord(mustUUID(), childID, cmd)},
		[]event.Event{accepted},
		nil,
		map[uuid.UUID]struct{}{childID: {}},
	); err != nil {
		t.Fatal(err)
	}
	resolved, ok := manager.getResolved(requestID)
	if !ok || resolved.childID != childID || resolved.status != tool.DelegateStatusFailed {
		t.Fatalf("resolved = %+v, %v; want failed child %v", resolved, ok, childID)
	}
}

func TestAttachAndActivateTombstonesRuntimeMismatchFromRestoredBuilder(t *testing.T) {
	t.Parallel()
	sessionID, rootID, childID := mustUUID(), mustUUID(), mustUUID()
	started := restoreRuntimeStarted(model.ModelKey{Provider: "provider", Model: "luna-target"})
	started.Header = event.Header{
		AgentName:   "worker",
		Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
		Cause:       identity.Cause{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: rootID}},
	}
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), sessionID, childID)
	var err error
	bound, err = restoreRuntimeBinding(started, bound, foldLoopInference([]event.Event{started}), restoreRuntimeCatalog(t), true, false)
	if err != nil {
		t.Fatalf("restoreRuntimeBinding: %v", err)
	}

	builder := &fakeForeignBuilder{err: &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}}
	var registry foreign.BuilderRegistry
	if err := registry.Register("acp/codex", builder.build, builder.buildRestored); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	s := &Session{
		sessionID:       sessionID,
		sessionCtx:      context.Background(),
		factory:         event.NewFactory(uuid.New, time.Now),
		hub:             hub.New(sessionID),
		loops:           make(map[uuid.UUID]*loopHandle),
		foreignRegistry: &registry,
	}
	rootBackend := newFakeBackend()
	s.loops[rootID] = &loopHandle{id: rootID, owner: s, backend: rootBackend, bound: bound, state: tool.DelegateStatusIdle}
	t.Cleanup(func() {
		rootBackend.CommandSink() <- command.Shutdown{Ack: make(chan error, 1)}
	})

	sub, err := s.hub.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	plan := loopPlan{
		started:  started,
		bound:    bound,
		bindings: tool.Bindings{SessionID: sessionID, LoopID: childID},
		folded:   foldLoop([]event.Event{started}),
		events:   []event.Event{started},
	}
	if err := attachAndActivate(s, []event.Event{started}, []loopPlan{plan}, rootID); err != nil {
		t.Fatalf("attachAndActivate: %v", err)
	}
	if got := s.ActiveLoopID(); got != rootID {
		t.Fatalf("active loop = %v, want root %v", got, rootID)
	}
	if got := s.spawnedCount(); got != 0 {
		t.Fatalf("spawned count = %d, want unchanged 0", got)
	}
	s.loopsMu.RLock()
	handle, ok := s.loops[childID]
	s.loopsMu.RUnlock()
	if !ok || !handle.tombstoned || handle.backend != nil {
		t.Fatalf("child handle = %#v, %v; want tombstoned with nil backend", handle, ok)
	}
	select {
	case delivery := <-sub.Events():
		tombstone, ok := delivery.Event.(event.LoopRestoreTombstoned)
		if !ok {
			t.Fatalf("event = %T, want LoopRestoreTombstoned", delivery.Event)
		}
		if tombstone.Category != event.LoopRestoreTombstoneRuntimeUnavailable {
			t.Fatalf("tombstone category = %q, want %q", tombstone.Category, event.LoopRestoreTombstoneRuntimeUnavailable)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime mismatch tombstone was not published")
	}
}

func TestAttachAndActivateActiveChildRuntimeFailureIsFatal(t *testing.T) {
	t.Parallel()
	s, rootID, childID, plan := runtimeFailureRestoreFixture(t,
		&RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}, nil)

	err := attachAndActivate(s, []event.Event{
		plan.started,
		event.ActiveLoopChanged{ActiveLoopID: childID},
	}, []loopPlan{plan}, rootID)
	var restoreErr *RestoreError
	var sessionErr *SessionError
	if !errors.As(err, &restoreErr) || restoreErr.Kind != RestoreLoopFailed ||
		!errors.As(err, &sessionErr) || sessionErr.Kind != SessionLoopExited {
		t.Fatalf("attachAndActivate error = %v, want RestoreLoopFailed wrapping SessionLoopExited", err)
	}
	if got := s.ActiveLoopID(); got != rootID {
		t.Fatalf("active loop = %v, want root %v after failed activation", got, rootID)
	}
}

func TestAttachAndActivateUnrelatedChildRestoreErrorIsFatalWithoutTombstone(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("unrelated restored builder failure")
	s, rootID, childID, plan := runtimeFailureRestoreFixture(t,
		&RestoreError{Kind: RestoreLoopFailed, Cause: sentinel}, nil)

	err := attachAndActivate(s, []event.Event{plan.started}, []loopPlan{plan}, rootID)
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) || restoreErr.Kind != RestoreLoopFailed || !errors.Is(err, sentinel) {
		t.Fatalf("attachAndActivate error = %v, want RestoreLoopFailed retaining unrelated cause", err)
	}
	if _, ok := s.Loop(childID); ok {
		t.Fatal("unrelated child restore failure registered a tombstoned handle")
	}
	if got := s.ActiveLoopID(); got != rootID {
		t.Fatalf("active loop = %v, want root %v", got, rootID)
	}
}

func TestBuildRestoredSessionRootRuntimeFailureIsFatal(t *testing.T) {
	t.Parallel()
	sessionID, rootID := mustUUID(), mustUUID()
	started := restoreRuntimeStarted(model.ModelKey{Provider: "provider", Model: "luna-target"})
	started.Header = event.Header{
		AgentName:   "worker",
		Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: rootID},
	}
	ri := foldLoopInference([]event.Event{started})
	ri.AgentSessionID = "root-agent-session"
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), sessionID, rootID)
	var err error
	bound, err = restoreRuntimeBinding(started, bound, ri, restoreRuntimeCatalog(t), true, false)
	if err != nil {
		t.Fatalf("restoreRuntimeBinding: %v", err)
	}

	runtimeFailure := &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}
	builder := &fakeForeignBuilder{err: runtimeFailure}
	var registry foreign.BuilderRegistry
	if err := registry.Register("acp/codex", builder.build, builder.buildRestored); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	restoreCtx, restoreCancel := context.WithCancel(context.Background())
	t.Cleanup(restoreCancel)
	s, err := buildRestoredSession(
		restoreCtx, restoreCancel, bound,
		tool.Bindings{SessionID: sessionID, LoopID: rootID},
		sessionID, rootID, "", 0, foldLoop([]event.Event{started}), ri, nil,
		fakeSessionJournal{}, event.NewFactory(uuid.New, time.Now), uuid.New, time.Now,
		WithForeignBuilderRegistry(&registry),
	)
	if s != nil {
		t.Fatal("buildRestoredSession returned a session after root runtime failure")
	}
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) || restoreErr.Kind != RestoreLoopFailed || !errors.Is(err, runtimeFailure) {
		t.Fatalf("buildRestoredSession error = %v, want RestoreLoopFailed retaining runtime failure", err)
	}
}

func TestTombstoneCommitObserverCanLookupFailedChild(t *testing.T) {
	t.Parallel()
	var observed, found, failed bool
	s, _, _, plan := runtimeFailureRestoreFixture(t,
		&RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable},
		func(s *Session, childID uuid.UUID, ev event.Event) {
			if _, ok := ev.(event.LoopRestoreTombstoned); !ok {
				return
			}
			observed = true
			handle, ok := s.Loop(childID)
			found = ok
			if loopHandle, ok := handle.(*loopHandle); ok {
				failed = loopHandle.tombstoned && loopHandle.mechanicalState() == tool.DelegateStatusFailed
			}
		})

	if err := s.attachRestoredTombstonedLoop(plan, loop.Provenance{LoopID: plan.started.Cause.Coordinates.LoopID}); err != nil {
		t.Fatalf("attachRestoredTombstonedLoop: %v", err)
	}
	if !observed || !found || !failed {
		t.Fatalf("commit observer state: observed=%v found=%v failed=%v; want all true", observed, found, failed)
	}
}

func runtimeFailureRestoreFixture(
	t *testing.T,
	builderErr error,
	observer func(*Session, uuid.UUID, event.Event),
) (*Session, uuid.UUID, uuid.UUID, loopPlan) {
	t.Helper()
	sessionID, rootID, childID := mustUUID(), mustUUID(), mustUUID()
	started := restoreRuntimeStarted(model.ModelKey{Provider: "provider", Model: "luna-target"})
	started.Header = event.Header{
		AgentName:   "worker",
		Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: childID},
		Cause:       identity.Cause{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: rootID}},
	}
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), sessionID, childID)
	var err error
	bound, err = restoreRuntimeBinding(started, bound, foldLoopInference([]event.Event{started}), restoreRuntimeCatalog(t), true, false)
	if err != nil {
		t.Fatalf("restoreRuntimeBinding: %v", err)
	}
	events := []event.Event{
		started,
		event.LoopAgentSessionBound{Header: started.Header, ACPSessionID: "child-agent-session"},
	}

	builder := &fakeForeignBuilder{err: builderErr}
	var registry foreign.BuilderRegistry
	if err := registry.Register("acp/codex", builder.build, builder.buildRestored); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	s := &Session{
		sessionID:       sessionID,
		sessionCtx:      context.Background(),
		factory:         event.NewFactory(uuid.New, time.Now),
		loops:           make(map[uuid.UUID]*loopHandle),
		foreignRegistry: &registry,
	}
	if observer == nil {
		s.hub = hub.New(sessionID)
	} else {
		s.hub = hub.New(sessionID, hub.WithCommitObserver(func(ev event.Event) { observer(s, childID, ev) }))
	}
	rootBackend := newFakeBackend()
	s.loops[rootID] = &loopHandle{id: rootID, owner: s, backend: rootBackend, bound: bound, state: tool.DelegateStatusIdle}
	s.activeLoopID = rootID
	t.Cleanup(func() {
		rootBackend.CommandSink() <- command.Shutdown{Ack: make(chan error, 1)}
	})

	return s, rootID, childID, loopPlan{
		started:  started,
		bound:    bound,
		bindings: tool.Bindings{SessionID: sessionID, LoopID: childID},
		folded:   foldLoop(events),
		events:   events,
	}
}
