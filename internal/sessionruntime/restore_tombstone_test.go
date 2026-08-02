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
