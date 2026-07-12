package sessionruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

type checkpointOrder struct {
	mu    sync.Mutex
	steps []string
}

func (o *checkpointOrder) add(step string) {
	o.mu.Lock()
	o.steps = append(o.steps, step)
	o.mu.Unlock()
}

func (o *checkpointOrder) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.steps...)
}

type boundaryBlobs struct {
	storage.Blobs
	order *checkpointOrder
}

type blockingBoundaryBlobs struct {
	storage.Blobs
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingBoundaryBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	blocked := false
	b.once.Do(func() {
		blocked = true
		close(b.entered)
	})
	if blocked {
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return b.Blobs.Put(ctx, key, r)
}

func (b boundaryBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	if err := b.Blobs.Put(ctx, key, r); err != nil {
		return err
	}
	b.order.add("blob")
	return nil
}

type boundaryPermit struct{ order *checkpointOrder }

func (p boundaryPermit) Release() { p.order.add("release") }

type boundaryCoordinator struct{ order *checkpointOrder }

func (c boundaryCoordinator) Acquire(_ context.Context, op tool.WorkspaceOperation, path string) (tool.WorkspacePermit, error) {
	if op != tool.WorkspaceOperationCheckpoint || path != "" {
		panic("unexpected checkpoint permit request")
	}
	c.order.add("acquire")
	return boundaryPermit{order: c.order}, nil
}
func (boundaryCoordinator) Healthy() error { return nil }

type boundaryPublisher struct {
	order        *checkpointOrder
	mu           sync.Mutex
	events       []event.Event
	checkpointed chan event.WorkspaceCheckpointed
}

func (p *boundaryPublisher) PublishEventChecked(_ context.Context, ev event.Event) error {
	p.mu.Lock()
	p.events = append(p.events, ev)
	p.mu.Unlock()
	switch ev.(type) {
	case event.StepDone, event.TurnDone, event.TurnFailed, event.TurnInterrupted, event.SessionIdle:
		p.order.add("trigger")
	case event.WorkspaceCheckpointed:
		p.order.add("checkpoint")
		if p.checkpointed != nil {
			p.checkpointed <- ev.(event.WorkspaceCheckpointed)
		}
	}
	return nil
}

func TestCheckpointBestEffortKeepsOnlyLatestPendingBoundary(t *testing.T) {
	t.Parallel()
	blobs := &blockingBoundaryBlobs{Blobs: memstore.New().Blobs, entered: make(chan struct{}), release: make(chan struct{})}
	ws, err := workspacestore.Open(blobs)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 3)}
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid,
		Policy:    checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointBestEffort, Timeout: time.Second},
		Store:     ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
		Factory: event.NewFactory(uuid.New, time.Now),
	})
	t.Cleanup(c.shutdown)
	step := func(index byte) event.StepDone {
		stepID, _ := uuid.New()
		eventID, _ := uuid.New()
		_ = index
		return event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eventID}}
	}
	first, middle, latest := step(1), step(2), step(3)
	firstDone := make(chan error, 1)
	go func() { firstDone <- c.boundary(context.Background(), first) }()
	select {
	case <-blobs.entered:
	case <-time.After(time.Second):
		t.Fatal("first best-effort walk did not start")
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first boundary acceptance: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("best-effort boundary blocked through blob walk")
	}
	if err := c.boundary(context.Background(), middle); err != nil {
		t.Fatal(err)
	}
	if err := c.boundary(context.Background(), latest); err != nil {
		t.Fatal(err)
	}
	close(blobs.release)
	firstCP := <-publisher.checkpointed
	latestCP := <-publisher.checkpointed
	if firstCP.Cause.EventID != first.EventID {
		t.Fatalf("first checkpoint cause = %v, want %v", firstCP.Cause.EventID, first.EventID)
	}
	if latestCP.Cause.EventID != latest.EventID {
		t.Fatalf("latest checkpoint cause = %v, want %v (middle %v must coalesce)", latestCP.Cause.EventID, latest.EventID, middle.EventID)
	}
	select {
	case extra := <-publisher.checkpointed:
		t.Fatalf("unexpected third checkpoint for coalesced boundary: %+v", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestCheckpointBestEffortActivationCancelsQuiescentWalk(t *testing.T) {
	t.Parallel()
	blobs := &blockingBoundaryBlobs{Blobs: memstore.New().Blobs, entered: make(chan struct{}), release: make(chan struct{})}
	ws, err := workspacestore.Open(blobs)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	stepID, _ := uuid.New()
	eventID, _ := uuid.New()
	publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 1)}
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointBestEffort, Timeout: time.Second},
		Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
		Factory: event.NewFactory(uuid.New, time.Now),
	})
	t.Cleanup(c.shutdown)
	trigger := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eventID}}
	if err := c.boundary(context.Background(), trigger); err != nil {
		t.Fatal(err)
	}
	<-blobs.entered
	c.activated()
	c.wg.Wait()
	select {
	case cp := <-publisher.checkpointed:
		t.Fatalf("activation-canceled walk emitted checkpoint: %+v", cp)
	default:
	}
}

func TestCheckpointRequiredTimeoutLatchesAndManualSuccessRecovers(t *testing.T) {
	t.Parallel()
	blobs := &blockingBoundaryBlobs{Blobs: memstore.New().Blobs, entered: make(chan struct{}), release: make(chan struct{})}
	ws, err := workspacestore.Open(blobs)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	stepID, _ := uuid.New()
	eventID, _ := uuid.New()
	var faultMu sync.Mutex
	var fault error
	publisher := &boundaryPublisher{order: &checkpointOrder{}}
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointRequired, Timeout: 20 * time.Millisecond},
		Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
		Factory: event.NewFactory(uuid.New, time.Now), Idle: func() bool { return true },
		Fault:   func(err error) { faultMu.Lock(); fault = err; faultMu.Unlock() },
		Recover: func() { faultMu.Lock(); fault = nil; faultMu.Unlock() },
	})
	t.Cleanup(c.shutdown)
	trigger := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eventID}}
	err = c.boundary(context.Background(), trigger)
	var timeout *CheckpointError
	if !errors.As(err, &timeout) || timeout.Kind != CheckpointTimeout {
		t.Fatalf("required boundary = %T %v, want timeout", err, err)
	}
	faultMu.Lock()
	latched := fault
	faultMu.Unlock()
	if latched == nil {
		t.Fatal("required timeout did not latch workspace fault")
	}
	close(blobs.release)
	if _, err := c.manual(context.Background()); err != nil {
		t.Fatalf("manual recovery: %v", err)
	}
	faultMu.Lock()
	defer faultMu.Unlock()
	if fault != nil {
		t.Fatalf("successful manual checkpoint left fault latched: %v", fault)
	}
}

func TestCheckpointControllerShutdownPublishesTerminalWithoutSyntheticSnapshot(t *testing.T) {
	t.Parallel()
	ws, root := checkpointFixture(t, nil)
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	eid, _ := uuid.New()
	publisher := &boundaryPublisher{order: &checkpointOrder{}}
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnTurnDone, Priority: checkpointRequired, Timeout: time.Second},
		Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
		Factory: event.NewFactory(uuid.New, time.Now),
	})
	c.shutdown()
	terminal := event.TurnInterrupted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid}, EventID: eid}}
	if err := c.boundary(context.Background(), terminal); err != nil {
		t.Fatalf("terminal after shutdown: %v", err)
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.events) != 1 {
		t.Fatalf("events after shutdown = %d, want terminal only", len(publisher.events))
	}
	if _, ok := publisher.events[0].(event.TurnInterrupted); !ok {
		t.Fatalf("event = %T, want TurnInterrupted", publisher.events[0])
	}
}

func TestCheckpointRequiredFaultLatchPreservesTriggerAndRejectsAutomaticWalk(t *testing.T) {
	t.Parallel()
	ws, root := checkpointFixture(t, nil)
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	stepID, _ := uuid.New()
	eid, _ := uuid.New()
	publisher := &boundaryPublisher{order: &checkpointOrder{}}
	latched := errors.New("workspace persistence latched")
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointRequired, Timeout: time.Second},
		Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
		Factory: event.NewFactory(uuid.New, time.Now), Faulted: func() error { return latched },
	})
	t.Cleanup(c.shutdown)
	trigger := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eid}}
	err := c.boundary(context.Background(), trigger)
	var faulted *CheckpointError
	if !errors.As(err, &faulted) || faulted.Kind != CheckpointFaulted {
		t.Fatalf("boundary = %T %v, want CheckpointFaulted", err, err)
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.events) != 1 {
		t.Fatalf("events = %d, want durable trigger only", len(publisher.events))
	}
	if _, ok := publisher.events[0].(event.StepDone); !ok {
		t.Fatalf("event = %T, want StepDone", publisher.events[0])
	}
}

func TestCheckpointTurnBoundaryCoversEveryTerminal(t *testing.T) {
	t.Parallel()
	terminals := []struct {
		name string
		make func(event.Header) event.Event
	}{
		{name: "done", make: func(h event.Header) event.Event { return event.TurnDone{Header: h} }},
		{name: "failed", make: func(h event.Header) event.Event { return event.TurnFailed{Header: h} }},
		{name: "interrupted", make: func(h event.Header) event.Event { return event.TurnInterrupted{Header: h} }},
	}
	for _, tt := range terminals {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws, root := checkpointFixture(t, nil)
			sid, _ := uuid.New()
			lid, _ := uuid.New()
			tid, _ := uuid.New()
			eid, _ := uuid.New()
			publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 1)}
			c := newCheckpointController(checkpointControllerConfig{
				SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnTurnDone, Priority: checkpointRequired, Timeout: time.Second},
				Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
				Factory: event.NewFactory(uuid.New, time.Now),
			})
			t.Cleanup(c.shutdown)
			terminal := tt.make(event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid}, EventID: eid})
			if err := c.boundary(context.Background(), terminal); err != nil {
				t.Fatal(err)
			}
			cp := <-publisher.checkpointed
			if cp.Trigger != event.SnapshotTriggerTurnDone || cp.Cause.EventID != eid {
				t.Fatalf("checkpoint = %+v, want turn cause %v", cp, eid)
			}
		})
	}
}

func TestCheckpointSharedBestEffortIsFuzzy(t *testing.T) {
	t.Parallel()
	ws, root := checkpointFixture(t, nil)
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	stepID, _ := uuid.New()
	eid, _ := uuid.New()
	publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 1)}
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointBestEffort, Timeout: time.Second},
		Store: ws, Root: root, Mode: PlacementShared, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
		Factory: event.NewFactory(uuid.New, time.Now),
	})
	t.Cleanup(c.shutdown)
	trigger := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eid}}
	if err := c.boundary(context.Background(), trigger); err != nil {
		t.Fatal(err)
	}
	cp := <-publisher.checkpointed
	if cp.Consistency != event.SnapshotFuzzy {
		t.Fatalf("shared consistency = %v, want fuzzy", cp.Consistency)
	}
}

func TestCheckpointRequiredBoundariesCommitFIFOWithoutCoalescing(t *testing.T) {
	t.Parallel()
	blobs := &blockingBoundaryBlobs{Blobs: memstore.New().Blobs, entered: make(chan struct{}), release: make(chan struct{})}
	ws, err := workspacestore.Open(blobs)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 2)}
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointRequired, Timeout: time.Second},
		Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
		Factory: event.NewFactory(uuid.New, time.Now),
	})
	t.Cleanup(c.shutdown)
	step := func() event.StepDone {
		stepID, _ := uuid.New()
		eid, _ := uuid.New()
		return event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eid}}
	}
	first, second := step(), step()
	results := make(chan error, 2)
	go func() { results <- c.boundary(context.Background(), first) }()
	<-blobs.entered
	go func() { results <- c.boundary(context.Background(), second) }()
	close(blobs.release)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	cp1, cp2 := <-publisher.checkpointed, <-publisher.checkpointed
	if cp1.Cause.EventID != first.EventID || cp2.Cause.EventID != second.EventID {
		t.Fatalf("FIFO causes = [%v %v], want [%v %v]", cp1.Cause.EventID, cp2.Cause.EventID, first.EventID, second.EventID)
	}
}

func TestRequiredIdleCheckpointBlocksWaitIdleThroughBlobCommit(t *testing.T) {
	t.Parallel()
	blobs := &blockingBoundaryBlobs{Blobs: memstore.New().Blobs, entered: make(chan struct{}), release: make(chan struct{})}
	ws, err := workspacestore.Open(blobs)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingEventAppender{}
	s, err := newTestSession(context.Background(), cfg(&stubLLM{}),
		WithEventAppender(recorder),
		withResolvedPlacement(&resolvedPlacement{mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil)}),
		WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotOnIdle, Priority: SnapshotRequired, Timeout: time.Second}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	startID, _ := uuid.New()
	if err := s.PublishEventChecked(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.sessionID, LoopID: lid, TurnID: tid}, EventID: startID}}); err != nil {
		t.Fatal(err)
	}
	published := make(chan error, 1)
	go func() {
		idleID, _ := uuid.New()
		published <- s.PublishEventChecked(context.Background(), event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.sessionID, LoopID: lid}, EventID: idleID}})
	}()
	<-blobs.entered
	waited := make(chan error, 1)
	go func() { waited <- s.WaitIdle(context.Background()) }()
	select {
	case err := <-waited:
		t.Fatalf("WaitIdle returned before required blob/checkpoint commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(blobs.release)
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
	var idle event.SessionIdle
	var cp event.WorkspaceCheckpointed
	for _, ev := range recorder.snapshot() {
		switch e := ev.(type) {
		case event.SessionIdle:
			idle = e
		case event.WorkspaceCheckpointed:
			cp = e
		}
	}
	if idle.EventID.IsZero() || cp.Cause.EventID != idle.EventID || cp.Trigger != event.SnapshotTriggerIdle {
		t.Fatalf("idle/cp cause mismatch: idle=%+v checkpoint=%+v", idle.Header, cp)
	}
}

func TestCheckpointBoundaryRequiredOrdersTriggerBlobAndCheckpoint(t *testing.T) {
	t.Parallel()
	order := &checkpointOrder{}
	backend := memstore.New()
	ws, err := workspacestore.Open(boundaryBlobs{Blobs: backend.Blobs, order: order})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	stid, _ := uuid.New()
	triggerID, _ := uuid.New()
	trigger := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stid}, EventID: triggerID}}
	publisher := &boundaryPublisher{order: order}
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid,
		Policy:    checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointRequired, Timeout: time.Second},
		Store:     ws, Root: root, Mode: PlacementSession,
		Coordinator: boundaryCoordinator{order: order}, Publisher: publisher,
		Factory: event.NewFactory(uuid.New, time.Now),
	})
	t.Cleanup(c.shutdown)

	if err := c.boundary(context.Background(), trigger); err != nil {
		t.Fatalf("boundary: %v", err)
	}
	want := []string{"acquire", "trigger", "blob", "checkpoint", "release"}
	got := order.snapshot()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	cp := publisher.events[len(publisher.events)-1].(event.WorkspaceCheckpointed)
	if cp.Trigger != event.SnapshotTriggerStepDone || cp.Consistency != event.SnapshotQuiescent {
		t.Fatalf("checkpoint metadata = %+v", cp)
	}
	if cp.Cause.EventID != triggerID || cp.Cause.Coordinates != trigger.EventHeader().Coordinates || cp.Cause.Agency != identity.AgencyMachine {
		t.Fatalf("checkpoint cause = %+v, want direct trigger %+v", cp.Cause, trigger.EventHeader())
	}
}

func TestSessionCheckpointPolicyWiresControllerOnlyWithWorkspace(t *testing.T) {
	t.Parallel()
	ws, root := checkpointFixture(t, nil)
	s, err := newTestSession(context.Background(), cfg(&stubLLM{}),
		withResolvedPlacement(&resolvedPlacement{mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil)}),
		WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotOnStepDone, Priority: SnapshotRequired, Timeout: time.Second}),
	)
	if err != nil {
		t.Fatalf("newTestSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if s.checkpoints == nil {
		t.Fatal("snapshot policy with workspace did not construct controller")
	}
}

func TestCheckpointWorkspaceRequiresSessionIdle(t *testing.T) {
	t.Parallel()
	ws, root := checkpointFixture(t, nil)
	s, err := newTestSession(context.Background(), cfg(&stubLLM{}),
		withResolvedPlacement(&resolvedPlacement{mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil)}),
		WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotManual, Priority: SnapshotBestEffort, Timeout: time.Second}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	eid, _ := uuid.New()
	if err := s.PublishEventChecked(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.sessionID, LoopID: lid, TurnID: tid}, EventID: eid}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckpointWorkspace(context.Background()); err == nil {
		t.Fatal("CheckpointWorkspace while active = nil, want not-idle")
	} else {
		var target *CheckpointError
		if !errors.As(err, &target) || target.Kind != CheckpointNotIdle {
			t.Fatalf("CheckpointWorkspace while active = %T %v, want CheckpointNotIdle", err, err)
		}
	}
}

func TestShutdownCancelsCheckpointControllerBeforeLeaseRelease(t *testing.T) {
	t.Parallel()
	ws, root := checkpointFixture(t, nil)
	s, err := newTestSession(context.Background(), cfg(&stubLLM{}),
		withResolvedPlacement(&resolvedPlacement{mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil)}),
		WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotOnIdle, Timeout: time.Second}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.checkpoints.ctx.Done():
	default:
		t.Fatal("checkpoint controller remains active after Shutdown")
	}
}
