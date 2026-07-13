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

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/inference"
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

type failingPutBlobs struct {
	storage.Blobs
	err error
}

func (b failingPutBlobs) Put(context.Context, string, io.Reader) error { return b.err }

type failFirstPutBlobs struct {
	storage.Blobs
	mu     sync.Mutex
	err    error
	failed bool
}

type panickingRuntimeContext struct{}

func (panickingRuntimeContext) Blocks(context.Context) []content.Block {
	panic("runtime context panic")
}

type panickingInference struct{}

func (panickingInference) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	panic("inference invoke panic")
}

func (panickingInference) Stream(context.Context, inference.Request) (*inference.StreamReader[content.Chunk], error) {
	panic("inference stream panic")
}

func (b *failFirstPutBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	b.mu.Lock()
	if !b.failed {
		b.failed = true
		b.mu.Unlock()
		return b.err
	}
	b.mu.Unlock()
	return b.Blobs.Put(ctx, key, r)
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

func TestInterruptCheckpointOutcomeFollowsPolicyBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		trigger         checkpointTrigger
		priority        checkpointPriority
		wantDisposition interruptCheckpointDisposition
		wantCheckpoints int
		wantTrigger     event.SnapshotTriggerKind
	}{
		{name: "required idle commits interrupt checkpoint", trigger: checkpointOnIdle, priority: checkpointRequired, wantDisposition: interruptCheckpointCommitted, wantCheckpoints: 1, wantTrigger: event.SnapshotTriggerInterrupt},
		{name: "required turn commits turn checkpoint", trigger: checkpointOnTurnDone, priority: checkpointRequired, wantDisposition: interruptCheckpointCommitted, wantCheckpoints: 1, wantTrigger: event.SnapshotTriggerTurnDone},
		{name: "required step manufactures no step", trigger: checkpointOnStepDone, priority: checkpointRequired, wantDisposition: interruptCheckpointCommitted},
		{name: "best effort idle accepts interrupt checkpoint", trigger: checkpointOnIdle, priority: checkpointBestEffort, wantDisposition: interruptCheckpointAccepted, wantCheckpoints: 1, wantTrigger: event.SnapshotTriggerInterrupt},
		{name: "best effort turn releases after idle", trigger: checkpointOnTurnDone, priority: checkpointBestEffort, wantDisposition: interruptCheckpointAccepted, wantCheckpoints: 1, wantTrigger: event.SnapshotTriggerTurnDone},
		{name: "best effort step manufactures no step", trigger: checkpointOnStepDone, priority: checkpointBestEffort, wantDisposition: interruptCheckpointAccepted},
		{name: "manual releases after idle without checkpoint", trigger: checkpointManual, priority: checkpointBestEffort, wantDisposition: interruptCheckpointAccepted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws, err := workspacestore.Open(memstore.New().Blobs)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "work.txt"), []byte("work"), 0o600); err != nil {
				t.Fatal(err)
			}
			sid, lid, tid := mustUUID(), mustUUID(), mustUUID()
			publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 2)}
			c := newCheckpointController(checkpointControllerConfig{
				SessionID: sid,
				Policy:    checkpointPolicy{Trigger: tt.trigger, Priority: tt.priority, Timeout: time.Second},
				Store:     ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil),
				Publisher: publisher, Factory: event.NewFactory(uuid.New, time.Now),
			})
			t.Cleanup(c.shutdown)

			sweep := c.beginInterruptSweep()
			terminal := event.TurnInterrupted{Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid},
				EventID:     mustUUID(),
			}}
			if err := c.boundary(context.Background(), terminal); err != nil {
				t.Fatalf("interrupt terminal boundary: %v", err)
			}
			idle := event.SessionIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: mustUUID()}}
			if err := c.sessionIdle(context.Background(), idle, func() error {
				return publisher.PublishEventChecked(context.Background(), idle)
			}); err != nil {
				t.Fatalf("session idle: %v", err)
			}
			outcome, err := sweep.await(context.Background())
			if err != nil {
				t.Fatalf("await interrupt checkpoint outcome: %v", err)
			}
			if outcome.Disposition != tt.wantDisposition {
				t.Fatalf("disposition = %v, want %v", outcome.Disposition, tt.wantDisposition)
			}
			for i := 0; i < tt.wantCheckpoints; i++ {
				select {
				case <-publisher.checkpointed:
				case <-time.After(time.Second):
					t.Fatalf("checkpoint %d did not commit", i+1)
				}
			}

			var checkpoints []event.WorkspaceCheckpointed
			publisher.mu.Lock()
			for _, ev := range publisher.events {
				if cp, ok := ev.(event.WorkspaceCheckpointed); ok {
					checkpoints = append(checkpoints, cp)
				}
			}
			publisher.mu.Unlock()
			if len(checkpoints) != tt.wantCheckpoints {
				t.Fatalf("checkpoint count = %d, want %d", len(checkpoints), tt.wantCheckpoints)
			}
			if tt.wantCheckpoints > 0 && checkpoints[0].Trigger != tt.wantTrigger {
				t.Fatalf("checkpoint trigger = %v, want %v", checkpoints[0].Trigger, tt.wantTrigger)
			}
		})
	}
}

func TestInterruptCheckpointOutcomeReportsRequiredFault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "required idle fault is explicit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws, err := workspacestore.Open(failingPutBlobs{Blobs: memstore.New().Blobs, err: errors.New("put failed")})
			if err != nil {
				t.Fatal(err)
			}
			sid, lid, tid := mustUUID(), mustUUID(), mustUUID()
			publisher := &boundaryPublisher{order: &checkpointOrder{}}
			c := newCheckpointController(checkpointControllerConfig{
				SessionID: sid,
				Policy:    checkpointPolicy{Trigger: checkpointOnIdle, Priority: checkpointRequired, Timeout: time.Second},
				Store:     ws, Root: t.TempDir(), Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil),
				Publisher: publisher, Factory: event.NewFactory(uuid.New, time.Now),
			})
			t.Cleanup(c.shutdown)
			sweep := c.beginInterruptSweep()
			terminal := event.TurnInterrupted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid}, EventID: mustUUID()}}
			if err := c.boundary(context.Background(), terminal); err != nil {
				t.Fatal(err)
			}
			idle := event.SessionIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: mustUUID()}}
			if err := c.sessionIdle(context.Background(), idle, func() error { return publisher.PublishEventChecked(context.Background(), idle) }); err == nil {
				t.Fatal("session idle checkpoint error = nil, want failure")
			}
			outcome, err := sweep.await(context.Background())
			if err != nil {
				t.Fatalf("await outcome: %v", err)
			}
			if outcome.Disposition != interruptCheckpointFaulted || outcome.Err == nil {
				t.Fatalf("outcome = %+v, want explicit fault", outcome)
			}
		})
	}
}

func TestInterruptCheckpointRequiredTurnWaitsForNestedCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "session idle nested in terminal publish cannot release before blob commit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blobs := &blockingBoundaryBlobs{Blobs: memstore.New().Blobs, entered: make(chan struct{}), release: make(chan struct{})}
			ws, err := workspacestore.Open(blobs)
			if err != nil {
				t.Fatal(err)
			}
			sid, lid, tid := mustUUID(), mustUUID(), mustUUID()
			queued := make(chan struct{}, 2)
			base := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 1)}
			publisher := &nestedIdlePublisher{
				base:     base,
				idle:     event.SessionIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: mustUUID()}},
				idleDone: make(chan error, 1),
			}
			c := newCheckpointController(checkpointControllerConfig{
				SessionID: sid,
				Policy:    checkpointPolicy{Trigger: checkpointOnTurnDone, Priority: checkpointRequired, Timeout: time.Second},
				Store:     ws, Root: t.TempDir(), Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil),
				Publisher: publisher, Factory: event.NewFactory(uuid.New, time.Now),
				RequiredQueued: func(event.Event) { queued <- struct{}{} },
			})
			publisher.controller = c
			t.Cleanup(c.shutdown)
			sweep := c.beginInterruptSweep()
			terminal := event.TurnInterrupted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid}, EventID: mustUUID()}}
			boundaryDone := make(chan error, 1)
			go func() { boundaryDone <- c.boundary(context.Background(), terminal) }()
			select {
			case <-blobs.entered:
			case <-time.After(time.Second):
				t.Fatal("required checkpoint did not reach blob commit")
			}
			<-queued // terminal request, whose nested idle fixed this sweep's cutoff
			laterStarted := make(chan struct{})
			laterRelease := make(chan struct{})
			laterDone := make(chan error, 1)
			go func() {
				_, laterErr := c.runRequired(context.Background(), event.TurnDone{}, func(context.Context) (workspacestore.Ref, error) {
					close(laterStarted)
					<-laterRelease
					return "", nil
				})
				laterDone <- laterErr
			}()
			<-queued // later request is ordered after the idle cutoff
			select {
			case outcome := <-sweep.result:
				t.Fatalf("interrupt outcome resolved before required blob commit: %+v", outcome)
			default:
			}
			close(blobs.release)
			if err := <-boundaryDone; err != nil {
				t.Fatalf("terminal boundary: %v", err)
			}
			select {
			case <-laterStarted:
			case <-time.After(time.Second):
				t.Fatal("later required boundary did not start")
			}
			if err := <-publisher.idleDone; err != nil {
				t.Fatalf("nested session idle: %v", err)
			}
			outcome, err := sweep.await(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Disposition != interruptCheckpointCommitted {
				t.Fatalf("outcome = %+v, want committed", outcome)
			}
			close(laterRelease)
			if err := <-laterDone; err != nil {
				t.Fatalf("later boundary: %v", err)
			}
		})
	}
}

func TestInterruptCheckpointSweepCancellationIsGenerationLocal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "canceled stale sweep cannot consume newer idle outcome"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			publisher := &boundaryPublisher{order: &checkpointOrder{}}
			c := newCheckpointController(checkpointControllerConfig{
				Policy:    checkpointPolicy{Trigger: checkpointManual, Priority: checkpointBestEffort},
				Publisher: publisher,
			})
			t.Cleanup(c.shutdown)
			stale := c.beginInterruptSweep()
			stale.cancel()
			current := c.beginInterruptSweep()
			idle := event.SessionIdle{Header: event.Header{EventID: mustUUID()}}
			if err := c.sessionIdle(context.Background(), idle, func() error {
				return publisher.PublishEventChecked(context.Background(), idle)
			}); err != nil {
				t.Fatal(err)
			}
			outcome, err := current.await(context.Background())
			if err != nil || outcome.Disposition != interruptCheckpointAccepted {
				t.Fatalf("current outcome = %+v, err %v", outcome, err)
			}
			select {
			case outcome := <-stale.result:
				t.Fatalf("canceled stale sweep resolved from newer idle: %+v", outcome)
			default:
			}
		})
	}
}

func TestInterruptCheckpointSweepAfterShutdownFaultsImmediately(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "closed controller cannot strand a sweep"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCheckpointController(checkpointControllerConfig{})
			c.shutdown()
			sweep := c.beginInterruptSweep()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			outcome, err := sweep.await(ctx)
			if err != nil {
				t.Fatalf("await closed controller: %v", err)
			}
			if outcome.Disposition != interruptCheckpointFaulted || outcome.Err == nil {
				t.Fatalf("outcome = %+v, want immediate fault", outcome)
			}
		})
	}
}

func TestInterruptBestEffortIdleStartsWhenActiveFinishesDuringPublish(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "accepted interrupt idle is scheduled after active walk finishes in publish callback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws, err := workspacestore.Open(memstore.New().Blobs)
			if err != nil {
				t.Fatal(err)
			}
			sid, lid, tid := mustUUID(), mustUUID(), mustUUID()
			publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 1)}
			c := newCheckpointController(checkpointControllerConfig{
				SessionID: sid,
				Policy:    checkpointPolicy{Trigger: checkpointOnIdle, Priority: checkpointBestEffort, Timeout: time.Second},
				Store:     ws, Root: t.TempDir(), Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil),
				Publisher: publisher, Factory: event.NewFactory(uuid.New, time.Now),
			})
			t.Cleanup(c.shutdown)
			c.mu.Lock()
			c.active = true // an older best-effort walk exists at the first active check
			c.mu.Unlock()
			sweep := c.beginInterruptSweep()
			terminal := event.TurnInterrupted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid}, EventID: mustUUID()}}
			if err := c.boundary(context.Background(), terminal); err != nil {
				t.Fatal(err)
			}
			idle := event.SessionIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: mustUUID()}}
			if err := c.sessionIdle(context.Background(), idle, func() error {
				c.finishBestEffort() // deterministically finish the older walk during publish
				return publisher.PublishEventChecked(context.Background(), idle)
			}); err != nil {
				t.Fatal(err)
			}
			outcome, err := sweep.await(context.Background())
			if err != nil || outcome.Disposition != interruptCheckpointAccepted {
				t.Fatalf("outcome = %+v, err %v", outcome, err)
			}
			select {
			case checkpoint := <-publisher.checkpointed:
				if checkpoint.Trigger != event.SnapshotTriggerInterrupt {
					t.Fatalf("trigger = %v, want interrupt", checkpoint.Trigger)
				}
			case <-time.After(time.Second):
				t.Fatal("accepted interrupt checkpoint was left pending with no active runner")
			}
		})
	}
}

func TestInterruptCheckpointShutdownResolvesLiveAndDeferredSweeps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		deferred bool
	}{
		{name: "live sweep", deferred: false},
		{name: "deferred required sweep", deferred: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			queued := make(chan struct{}, 1)
			publisher := &boundaryPublisher{order: &checkpointOrder{}}
			c := newCheckpointController(checkpointControllerConfig{
				Policy:         checkpointPolicy{Trigger: checkpointOnTurnDone, Priority: checkpointRequired, Timeout: time.Second},
				Publisher:      publisher,
				RequiredQueued: func(event.Event) { queued <- struct{}{} },
			})
			sweep := c.beginInterruptSweep()
			requiredDone := make(chan error, 1)
			if tt.deferred {
				go func() {
					_, err := c.runRequired(context.Background(), event.TurnInterrupted{}, func(ctx context.Context) (workspacestore.Ref, error) {
						<-ctx.Done()
						return "", ctx.Err()
					})
					requiredDone <- err
				}()
				<-queued
				idle := event.SessionIdle{Header: event.Header{EventID: mustUUID()}}
				if err := c.sessionIdle(context.Background(), idle, func() error {
					return publisher.PublishEventChecked(context.Background(), idle)
				}); err != nil {
					t.Fatal(err)
				}
				c.mu.Lock()
				deferred := len(c.interruptDeferred)
				c.mu.Unlock()
				if deferred != 1 {
					t.Fatalf("deferred sweeps = %d, want 1", deferred)
				}
			}
			shutdownDone := make(chan struct{})
			go func() { c.shutdown(); close(shutdownDone) }()
			outcome, err := sweep.await(context.Background())
			if err != nil || outcome.Disposition != interruptCheckpointFaulted || outcome.Err == nil {
				t.Fatalf("shutdown outcome = %+v, err %v", outcome, err)
			}
			select {
			case <-shutdownDone:
			case <-time.After(time.Second):
				t.Fatal("checkpoint controller shutdown hung")
			}
			if tt.deferred {
				if err := <-requiredDone; err == nil {
					t.Fatal("required request survived shutdown")
				}
			}
			c.mu.Lock()
			live, deferred := len(c.interruptSweeps), len(c.interruptDeferred)
			c.mu.Unlock()
			if live != 0 || deferred != 0 {
				t.Fatalf("sweep leaks after shutdown: live=%d deferred=%d", live, deferred)
			}
		})
	}
}

func TestInterruptRequiredHandoffCapturesFailureAndExactCutoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "completion during handoff faults sweep and later request cannot delay it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			queued := make(chan struct{}, 2)
			firstRelease := make(chan struct{})
			secondRelease := make(chan struct{})
			firstDone := make(chan error, 1)
			secondDone := make(chan error, 1)
			publisher := &boundaryPublisher{order: &checkpointOrder{}}
			var c *checkpointController
			cfg := checkpointControllerConfig{
				Policy:         checkpointPolicy{Trigger: checkpointOnTurnDone, Priority: checkpointRequired, Timeout: time.Second},
				Publisher:      publisher,
				RequiredQueued: func(event.Event) { queued <- struct{}{} },
			}
			c = newCheckpointController(cfg)
			c.interruptTransferred = func() {
				close(firstRelease)
				if err := <-firstDone; err == nil {
					t.Error("first required request error = nil")
				}
				go func() {
					_, err := c.runRequired(context.Background(), event.TurnDone{}, func(context.Context) (workspacestore.Ref, error) {
						<-secondRelease
						return "", nil
					})
					secondDone <- err
				}()
				<-queued
			}
			t.Cleanup(c.shutdown)
			sweep := c.beginInterruptSweep()
			go func() {
				_, err := c.runRequired(context.Background(), event.TurnInterrupted{}, func(context.Context) (workspacestore.Ref, error) {
					<-firstRelease
					return "", errors.New("first required failed")
				})
				firstDone <- err
			}()
			<-queued
			idle := event.SessionIdle{Header: event.Header{EventID: mustUUID()}}
			if err := c.sessionIdle(context.Background(), idle, func() error {
				return publisher.PublishEventChecked(context.Background(), idle)
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			outcome, err := sweep.await(ctx)
			cancel()
			close(secondRelease)
			if secondErr := <-secondDone; secondErr != nil {
				t.Fatalf("later required request: %v", secondErr)
			}
			if err != nil {
				t.Fatalf("sweep was delayed by request queued after handoff: %v", err)
			}
			if outcome.Disposition != interruptCheckpointFaulted || outcome.Err == nil {
				t.Fatalf("outcome = %+v, want first required failure", outcome)
			}
		})
	}
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

type nestedIdlePublisher struct {
	base       *boundaryPublisher
	controller *checkpointController
	idle       event.SessionIdle
	idleDone   chan error
	once       sync.Once
}

func (p *nestedIdlePublisher) PublishEventChecked(ctx context.Context, ev event.Event) error {
	if err := p.base.PublishEventChecked(ctx, ev); err != nil {
		return err
	}
	if _, ok := ev.(event.TurnInterrupted); ok {
		p.once.Do(func() {
			p.idleDone <- p.controller.sessionIdle(ctx, p.idle, func() error {
				return p.base.PublishEventChecked(ctx, p.idle)
			})
		})
	}
	return nil
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

func TestCheckpointManualPrecedesPendingBestEffortAndNeverCoalesces(t *testing.T) {
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
	manualQueued := make(chan struct{}, 2)
	publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 4)}
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointBestEffort, Timeout: time.Second},
		Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
		Factory: event.NewFactory(uuid.New, time.Now), Idle: func() bool { return true }, ManualQueued: func() { manualQueued <- struct{}{} },
	})
	t.Cleanup(c.shutdown)
	step := func() event.StepDone {
		stepID, _ := uuid.New()
		eid, _ := uuid.New()
		return event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eid}}
	}
	first, pending := step(), step()
	if err := c.boundary(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	<-blobs.entered
	if err := c.boundary(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	manualResults := make(chan error, 2)
	for range 2 {
		go func() { _, err := c.manual(context.Background()); manualResults <- err }()
		<-manualQueued
	}
	close(blobs.release)
	for range 2 {
		if err := <-manualResults; err != nil {
			t.Fatal(err)
		}
	}
	checkpoints := []event.WorkspaceCheckpointed{<-publisher.checkpointed, <-publisher.checkpointed, <-publisher.checkpointed, <-publisher.checkpointed}
	if checkpoints[0].Cause.EventID != first.EventID || checkpoints[1].Trigger != event.SnapshotTriggerManual || checkpoints[2].Trigger != event.SnapshotTriggerManual || checkpoints[3].Cause.EventID != pending.EventID {
		t.Fatalf("checkpoint precedence = [%v/%v %v/%v %v/%v %v/%v]", checkpoints[0].Trigger, checkpoints[0].Cause.EventID, checkpoints[1].Trigger, checkpoints[1].Cause.EventID, checkpoints[2].Trigger, checkpoints[2].Cause.EventID, checkpoints[3].Trigger, checkpoints[3].Cause.EventID)
	}
}

func TestCheckpointCanceledUnstartedManualDoesNotCommit(t *testing.T) {
	t.Parallel()
	blobs := &blockingBoundaryBlobs{Blobs: memstore.New().Blobs, entered: make(chan struct{}), release: make(chan struct{})}
	ws, err := workspacestore.Open(blobs)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "work.txt"), []byte("work"), 0o600)
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	stepID, _ := uuid.New()
	eid, _ := uuid.New()
	manualQueued := make(chan struct{}, 1)
	publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 2)}
	c := newCheckpointController(checkpointControllerConfig{SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointBestEffort, Timeout: time.Second}, Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher, Factory: event.NewFactory(uuid.New, time.Now), Idle: func() bool { return true }, ManualQueued: func() { close(manualQueued) }})
	t.Cleanup(c.shutdown)
	trigger := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eid}}
	if err := c.boundary(context.Background(), trigger); err != nil {
		t.Fatal(err)
	}
	<-blobs.entered
	manualCtx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := c.manual(manualCtx); result <- err }()
	<-manualQueued
	cancel()
	var canceled *CheckpointError
	if err := <-result; !errors.As(err, &canceled) || canceled.Kind != CheckpointCanceled {
		t.Fatalf("manual = %T %v", err, err)
	}
	close(blobs.release)
	first := <-publisher.checkpointed
	if first.Cause.EventID != eid {
		t.Fatalf("checkpoint cause = %v", first.Cause.EventID)
	}
	select {
	case extra := <-publisher.checkpointed:
		t.Fatalf("canceled manual committed %+v", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRequiredManualNotIdlePreservesExistingFaultLatch(t *testing.T) {
	t.Parallel()
	ws, root := checkpointFixture(t, nil)
	gate := newCheckpointAdmissionGate()
	latched := errors.New("existing required checkpoint fault")
	gate.latch(latched)
	recoveries := 0
	c := newCheckpointController(checkpointControllerConfig{
		Policy: checkpointPolicy{Trigger: checkpointManual, Priority: checkpointRequired, Timeout: time.Second},
		Store:  ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil),
		Publisher: &boundaryPublisher{order: &checkpointOrder{}}, Factory: event.NewFactory(uuid.New, time.Now),
		Idle: func() bool { return false }, Admission: gate.enterCheckpoint,
		Recover: func() { recoveries++; gate.recover() },
	})
	t.Cleanup(c.shutdown)

	_, err := c.manual(context.Background())
	var notIdle *CheckpointError
	if !errors.As(err, &notIdle) || notIdle.Kind != CheckpointNotIdle {
		t.Fatalf("manual = %T %v, want CheckpointNotIdle", err, err)
	}
	if recoveries != 0 {
		t.Fatalf("recoveries = %d, want 0", recoveries)
	}
	if _, err := gate.enterExecution(context.Background()); !errors.Is(err, latched) {
		t.Fatalf("execution admission after failed manual = %v, want existing latch %v", err, latched)
	}
}

func TestRequiredManualCanceledBeforePermitPreservesExistingFaultLatch(t *testing.T) {
	t.Parallel()
	ws, root := checkpointFixture(t, nil)
	gate := newCheckpointAdmissionGate()
	latched := errors.New("existing required checkpoint fault")
	gate.latch(latched)
	recoveries := 0
	c := newCheckpointController(checkpointControllerConfig{
		Policy: checkpointPolicy{Trigger: checkpointManual, Priority: checkpointRequired, Timeout: time.Second},
		Store:  ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil),
		Publisher: &boundaryPublisher{order: &checkpointOrder{}}, Factory: event.NewFactory(uuid.New, time.Now),
		Idle: func() bool { return true }, Admission: gate.enterCheckpoint,
		Recover: func() { recoveries++; gate.recover() },
	})
	t.Cleanup(c.shutdown)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.commit(ctx, nil, event.SnapshotTriggerManual, nil, nil)
	var canceled *CheckpointError
	if !errors.As(err, &canceled) || canceled.Kind != CheckpointCanceled {
		t.Fatalf("manual commit = %T %v, want CheckpointCanceled", err, err)
	}
	if recoveries != 0 {
		t.Fatalf("recoveries = %d, want 0", recoveries)
	}
	if _, err := gate.enterExecution(context.Background()); !errors.Is(err, latched) {
		t.Fatalf("execution admission after canceled manual = %v, want existing latch %v", err, latched)
	}
}

func TestCheckpointBestEffortObservesAsyncFailureAndRetriesNextEdge(t *testing.T) {
	t.Parallel()
	rootCause := errors.New("snapshot write failed")
	blobs := &failFirstPutBlobs{Blobs: memstore.New().Blobs, err: rootCause}
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
	publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 1)}
	observed := make(chan error, 2)
	faulted := make(chan error, 1)
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointBestEffort, Timeout: time.Second},
		Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
		Factory: event.NewFactory(uuid.New, time.Now), ObserveError: func(err error) { observed <- err },
		Fault: func(err error) { faulted <- err },
	})
	t.Cleanup(c.shutdown)
	step := func() event.StepDone {
		stepID, _ := uuid.New()
		eid, _ := uuid.New()
		return event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eid}}
	}
	first := step()
	if err := c.boundary(context.Background(), first); err != nil {
		t.Fatalf("first boundary acceptance: %v", err)
	}
	select {
	case err := <-observed:
		if !errors.Is(err, rootCause) {
			t.Fatalf("observed error = %v, want root cause %v", err, rootCause)
		}
	case <-time.After(time.Second):
		t.Fatal("async best-effort snapshot failure was not observed")
	}
	select {
	case err := <-faulted:
		t.Fatalf("best-effort failure latched execution fault: %v", err)
	default:
	}

	second := step()
	if err := c.boundary(context.Background(), second); err != nil {
		t.Fatalf("retry boundary acceptance: %v", err)
	}
	select {
	case cp := <-publisher.checkpointed:
		if cp.Cause.EventID != second.EventID {
			t.Fatalf("retry checkpoint cause = %v, want %v", cp.Cause.EventID, second.EventID)
		}
	case <-time.After(time.Second):
		t.Fatal("next eligible edge did not retry best-effort checkpoint")
	}
	select {
	case err := <-observed:
		t.Fatalf("successful retry produced extra observed error: %v", err)
	default:
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
	observed := make(chan error, 1)
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointBestEffort, Timeout: time.Second},
		Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
		Factory: event.NewFactory(uuid.New, time.Now), ObserveError: func(err error) { observed <- err },
	})
	t.Cleanup(c.shutdown)
	trigger := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eventID}}
	if err := c.boundary(context.Background(), trigger); err != nil {
		t.Fatal(err)
	}
	<-blobs.entered
	c.activated()
	c.waitDrained()
	select {
	case cp := <-publisher.checkpointed:
		t.Fatalf("activation-canceled walk emitted checkpoint: %+v", cp)
	default:
	}
	select {
	case err := <-observed:
		t.Fatalf("expected activation cancellation was reported as checkpoint failure: %v", err)
	default:
	}
}

func TestCheckpointBestEffortShutdownCancellationIsNotObservedAsFailure(t *testing.T) {
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
	observed := make(chan error, 1)
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointBestEffort, Timeout: time.Second},
		Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil),
		Publisher: &boundaryPublisher{order: &checkpointOrder{}}, Factory: event.NewFactory(uuid.New, time.Now),
		ObserveError: func(err error) { observed <- err },
	})
	trigger := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eventID}}
	if err := c.boundary(context.Background(), trigger); err != nil {
		t.Fatal(err)
	}
	<-blobs.entered
	c.shutdown()
	select {
	case err := <-observed:
		t.Fatalf("expected shutdown cancellation was reported as checkpoint failure: %v", err)
	default:
	}
}

func TestCheckpointBestEffortGenuineFailureRacingExpectedCancellationIsObserved(t *testing.T) {
	for _, tt := range []struct {
		name   string
		cancel func(*checkpointController, chan struct{})
	}{
		{
			name: "activation after failure",
			cancel: func(c *checkpointController, done chan struct{}) {
				c.activated()
				close(done)
			},
		},
		{
			name: "shutdown after failure",
			cancel: func(c *checkpointController, done chan struct{}) {
				go func() {
					c.shutdown()
					close(done)
				}()
				<-c.ctx.Done()
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rootCause := errors.New("genuine snapshot failure")
			ws, err := workspacestore.Open(failingPutBlobs{Blobs: memstore.New().Blobs, err: rootCause})
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
			observePending := make(chan struct{})
			releaseObserve := make(chan struct{})
			observed := make(chan error, 1)
			c := newCheckpointController(checkpointControllerConfig{
				SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointBestEffort, Timeout: time.Second},
				Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil),
				Publisher: &boundaryPublisher{order: &checkpointOrder{}}, Factory: event.NewFactory(uuid.New, time.Now),
				ObservePending: func() { close(observePending); <-releaseObserve },
				ObserveError:   func(err error) { observed <- err },
			})
			t.Cleanup(c.shutdown)
			trigger := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eventID}}
			if err := c.boundary(context.Background(), trigger); err != nil {
				t.Fatal(err)
			}
			select {
			case <-observePending:
			case <-time.After(time.Second):
				t.Fatal("genuine failure did not reach observation barrier")
			}
			cancelDone := make(chan struct{})
			tt.cancel(c, cancelDone)
			close(releaseObserve)
			select {
			case err := <-observed:
				if !errors.Is(err, rootCause) {
					t.Fatalf("observed error = %v, want genuine cause %v", err, rootCause)
				}
			case <-time.After(time.Second):
				t.Fatal("genuine failure was suppressed by later cancellation")
			}
			select {
			case <-cancelDone:
			case <-time.After(time.Second):
				t.Fatal("cancellation did not finish")
			}
		})
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

func TestCheckpointSharedActivationAllowsFuzzyWalkToComplete(t *testing.T) {
	t.Parallel()
	blobs := &blockingBoundaryBlobs{Blobs: memstore.New().Blobs, entered: make(chan struct{}), release: make(chan struct{})}
	ws, _ := workspacestore.Open(blobs)
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "work.txt"), []byte("work"), 0o600)
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	stepID, _ := uuid.New()
	eid, _ := uuid.New()
	publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 1)}
	c := newCheckpointController(checkpointControllerConfig{SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointBestEffort, Timeout: time.Second}, Store: ws, Root: root, Mode: PlacementShared, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher, Factory: event.NewFactory(uuid.New, time.Now)})
	t.Cleanup(c.shutdown)
	trigger := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eid}}
	if err := c.boundary(context.Background(), trigger); err != nil {
		t.Fatal(err)
	}
	<-blobs.entered
	c.activated()
	select {
	case cp := <-publisher.checkpointed:
		t.Fatalf("shared walk completed before release: %+v", cp)
	default:
	}
	close(blobs.release)
	cp := <-publisher.checkpointed
	if cp.Consistency != event.SnapshotFuzzy || cp.Cause.EventID != eid {
		t.Fatalf("shared checkpoint = %+v", cp)
	}
}

func TestCheckpointSnapshotFailureLeavesDurableTriggerWithoutDanglingRef(t *testing.T) {
	t.Parallel()
	snapshotErr := errors.New("blob put failed")
	ws, err := workspacestore.Open(failingPutBlobs{Blobs: memstore.New().Blobs, err: snapshotErr})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "work.txt"), []byte("work"), 0o600)
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	tid, _ := uuid.New()
	stepID, _ := uuid.New()
	eid, _ := uuid.New()
	publisher := &boundaryPublisher{order: &checkpointOrder{}}
	c := newCheckpointController(checkpointControllerConfig{SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointRequired, Timeout: time.Second}, Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher, Factory: event.NewFactory(uuid.New, time.Now)})
	t.Cleanup(c.shutdown)
	trigger := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eid}}
	if err := c.boundary(context.Background(), trigger); err == nil {
		t.Fatal("snapshot failure = nil")
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.events) != 1 {
		t.Fatalf("events = %d, want trigger only", len(publisher.events))
	}
	if got := publisher.events[0].EventHeader().EventID; got != eid {
		t.Fatalf("trigger id = %v, want %v", got, eid)
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
	publisher := &boundaryPublisher{order: &checkpointOrder{}, checkpointed: make(chan event.WorkspaceCheckpointed, 3)}
	queued := make(chan uuid.UUID, 3)
	c := newCheckpointController(checkpointControllerConfig{
		SessionID: sid, Policy: checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointRequired, Timeout: time.Second},
		Store: ws, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil), Publisher: publisher,
		Factory:        event.NewFactory(uuid.New, time.Now),
		RequiredQueued: func(ev event.Event) { queued <- ev.EventHeader().EventID },
	})
	t.Cleanup(c.shutdown)
	step := func() event.StepDone {
		stepID, _ := uuid.New()
		eid, _ := uuid.New()
		return event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID}, EventID: eid}}
	}
	first, second, third := step(), step(), step()
	results := make(chan error, 3)
	go func() { results <- c.boundary(context.Background(), first) }()
	if got := <-queued; got != first.EventID {
		t.Fatalf("first queued = %v", got)
	}
	<-blobs.entered
	go func() { results <- c.boundary(context.Background(), second) }()
	if got := <-queued; got != second.EventID {
		t.Fatalf("second queued = %v", got)
	}
	go func() { results <- c.boundary(context.Background(), third) }()
	if got := <-queued; got != third.EventID {
		t.Fatalf("third queued = %v", got)
	}
	close(blobs.release)
	for range 3 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	cp1, cp2, cp3 := <-publisher.checkpointed, <-publisher.checkpointed, <-publisher.checkpointed
	if cp1.Cause.EventID != first.EventID || cp2.Cause.EventID != second.EventID || cp3.Cause.EventID != third.EventID {
		t.Fatalf("FIFO causes = [%v %v %v], want [%v %v %v]", cp1.Cause.EventID, cp2.Cause.EventID, cp3.Cause.EventID, first.EventID, second.EventID, third.EventID)
	}
}

func TestCheckpointRequiredQueueRemovesCanceledMiddleRequest(t *testing.T) {
	t.Parallel()
	queued := make(chan uuid.UUID, 3)
	c := newCheckpointController(checkpointControllerConfig{RequiredQueued: func(ev event.Event) { queued <- ev.EventHeader().EventID }})
	t.Cleanup(c.shutdown)
	trigger := func() event.StepDone {
		id, _ := uuid.New()
		return event.StepDone{Header: event.Header{EventID: id}}
	}
	first, middle, third := trigger(), trigger(), trigger()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var orderMu sync.Mutex
	var order []uuid.UUID
	run := func(id uuid.UUID, started chan struct{}, release <-chan struct{}) func(context.Context) (workspacestore.Ref, error) {
		return func(context.Context) (workspacestore.Ref, error) {
			orderMu.Lock()
			order = append(order, id)
			orderMu.Unlock()
			if started != nil {
				close(started)
			}
			if release != nil {
				<-release
			}
			return "", nil
		}
	}
	results := make(chan error, 3)
	go func() {
		_, err := c.runRequired(context.Background(), first, run(first.EventID, firstStarted, releaseFirst))
		results <- err
	}()
	if got := <-queued; got != first.EventID {
		t.Fatalf("first queued = %v", got)
	}
	<-firstStarted
	middleCtx, cancelMiddle := context.WithCancel(context.Background())
	go func() { _, err := c.runRequired(middleCtx, middle, run(middle.EventID, nil, nil)); results <- err }()
	if got := <-queued; got != middle.EventID {
		t.Fatalf("middle queued = %v", got)
	}
	go func() {
		_, err := c.runRequired(context.Background(), third, run(third.EventID, nil, nil))
		results <- err
	}()
	if got := <-queued; got != third.EventID {
		t.Fatalf("third queued = %v", got)
	}
	cancelMiddle()
	var canceled *CheckpointError
	if err := <-results; !errors.As(err, &canceled) || canceled.Kind != CheckpointCanceled {
		t.Fatalf("middle result = %T %v, want canceled", err, err)
	}
	close(releaseFirst)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 2 || order[0] != first.EventID || order[1] != third.EventID {
		t.Fatalf("run order = %v, want [%v %v]", order, first.EventID, third.EventID)
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

func TestShutdownDrainsRequiredTurnCheckpointBeforeControllerAndLeases(t *testing.T) {
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
	var orderMu sync.Mutex
	var releaseOrder []string
	var s *Session
	assertControllerClosed := func(label string) {
		select {
		case <-s.checkpoints.ctx.Done():
		default:
			t.Errorf("%s lease released before checkpoint controller closed", label)
		}
		orderMu.Lock()
		releaseOrder = append(releaseOrder, label)
		orderMu.Unlock()
	}
	s, err = newTestSession(context.Background(), cfg(&stubLLM{blockUntilCancel: true}),
		WithEventAppender(recorder),
		WithLeaseRelease(func(context.Context) error { assertControllerClosed("session"); return nil }),
		withResolvedPlacement(&resolvedPlacement{
			mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil),
			rootRelease: func(context.Context) error { assertControllerClosed("root"); return nil },
		}),
		WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotOnTurnDone, Priority: SnapshotRequired, Timeout: time.Second}),
	)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if _, err := s.Submit(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case delivery := <-sub.Events():
			if _, ok := delivery.Event.(event.TurnStarted); ok {
				goto started
			}
		case <-time.After(time.Second):
			t.Fatal("turn did not start")
		}
	}

started:
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	select {
	case <-blobs.entered:
	case <-time.After(time.Second):
		t.Fatal("shutdown terminal never entered required checkpoint")
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before checkpoint blob committed: %v", err)
	default:
	}
	select {
	case <-s.checkpoints.ctx.Done():
		t.Fatal("checkpoint controller closed before active terminal checkpoint completed")
	default:
	}
	close(blobs.release)
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}

	var terminal event.TurnInterrupted
	var checkpoint event.WorkspaceCheckpointed
	for _, ev := range recorder.snapshot() {
		switch e := ev.(type) {
		case event.TurnInterrupted:
			terminal = e
		case event.WorkspaceCheckpointed:
			checkpoint = e
		}
	}
	if terminal.EventID.IsZero() || checkpoint.Cause.EventID != terminal.EventID || checkpoint.Trigger != event.SnapshotTriggerTurnDone {
		t.Fatalf("terminal/checkpoint mismatch: terminal=%+v checkpoint=%+v", terminal.Header, checkpoint)
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	if len(releaseOrder) != 2 || releaseOrder[0] != "root" || releaseOrder[1] != "session" {
		t.Fatalf("lease release order = %v, want [root session]", releaseOrder)
	}
}

func TestShutdownBestEffortPreservesRealTerminalAndCancelsActiveWalk(t *testing.T) {
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
	s, err := newTestSession(context.Background(), cfg(&stubLLM{blockUntilCancel: true}),
		WithEventAppender(recorder),
		withResolvedPlacement(&resolvedPlacement{mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil)}),
		WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotOnTurnDone, Priority: SnapshotBestEffort, Timeout: time.Second}),
	)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if _, err := s.Submit(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case delivery := <-sub.Events():
			if _, ok := delivery.Event.(event.TurnStarted); ok {
				goto startedBestEffort
			}
		case <-time.After(time.Second):
			t.Fatal("turn did not start")
		}
	}

startedBestEffort:
	done := make(chan error, 1)
	go func() { done <- s.Shutdown(context.Background()) }()
	select {
	case <-blobs.entered:
	case <-time.After(time.Second):
		t.Fatal("best-effort shutdown terminal did not start snapshot walk")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var terminals, checkpoints int
	for _, ev := range recorder.snapshot() {
		switch ev.(type) {
		case event.TurnInterrupted:
			terminals++
		case event.WorkspaceCheckpointed:
			checkpoints++
		}
	}
	if terminals != 1 || checkpoints != 0 {
		t.Fatalf("best-effort shutdown terminal/checkpoints = %d/%d, want 1/0 cancellation", terminals, checkpoints)
	}
}

func TestShutdownIdleSessionCreatesNoSnapshotTrigger(t *testing.T) {
	t.Parallel()
	ws, root := checkpointFixture(t, nil)
	recorder := &recordingEventAppender{}
	s, err := newTestSession(context.Background(), cfg(&stubLLM{}),
		WithEventAppender(recorder),
		withResolvedPlacement(&resolvedPlacement{mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil)}),
		WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotOnTurnDone, Priority: SnapshotRequired, Timeout: time.Second}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, ev := range recorder.snapshot() {
		switch ev.(type) {
		case event.TurnDone, event.TurnFailed, event.TurnInterrupted, event.WorkspaceCheckpointed:
			t.Fatalf("idle shutdown synthesized boundary event %T", ev)
		}
	}
}

func TestRequiredCheckpointFaultRejectsQueuedWorkAcrossLoopsUntilManualRecovery(t *testing.T) {
	t.Parallel()
	ws, root := checkpointFixture(t, nil)
	recorder := &recordingEventAppender{}
	s, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}),
		WithEventAppender(recorder),
		withResolvedPlacement(&resolvedPlacement{mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil)}),
		WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotManual, Priority: SnapshotRequired, Timeout: time.Second}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	childID, err := s.NewLoop(loop.Provenance{}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("child")}}))
	if err != nil {
		t.Fatal(err)
	}

	releaseWriter, err := s.checkpointAdmission.enterCheckpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	primaryCommand, err := s.Submit(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	childCommand, err := s.SubmitToLoop(context.Background(), childID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A second send to each unbuffered actor command channel cannot be received
	// until the first command's case has requested execution admission. This makes
	// both first commands deterministically parked behind the writer before faulting.
	if _, err := s.Submit(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitToLoop(context.Background(), childID, nil); err != nil {
		t.Fatal(err)
	}
	fault := errors.New("required checkpoint failed")
	s.latchWorkspaceCheckpointFault(fault)
	releaseWriter()
	if err := s.WaitIdle(context.Background()); !errors.Is(err, fault) {
		t.Fatalf("WaitIdle after required checkpoint fault = %v, want fault", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		cancelled := map[uuid.UUID]bool{}
		for _, ev := range recorder.snapshot() {
			switch e := ev.(type) {
			case event.TurnStarted:
				if e.Cause.CommandID == primaryCommand || e.Cause.CommandID == childCommand {
					t.Fatalf("faulted queued command %v emitted TurnStarted", e.Cause.CommandID)
				}
			case event.InputCancelled:
				cancelled[e.Cause.CommandID] = true
			}
		}
		if cancelled[primaryCommand] && cancelled[childCommand] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued commands were not both rejected: cancelled=%v", cancelled)
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := s.SubmitToLoop(context.Background(), childID, nil); err == nil {
		t.Fatal("submit while workspace checkpoint faulted = nil, want SessionFaulted")
	}
	if _, err := s.CheckpointWorkspace(context.Background()); err != nil {
		t.Fatalf("manual recovery checkpoint: %v", err)
	}
	if err := s.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after successful manual recovery = %v, want nil", err)
	}
	recoveredCommand, err := s.SubmitToLoop(context.Background(), childID, nil)
	if err != nil {
		t.Fatalf("submit after manual recovery: %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		starts := 0
		for _, ev := range recorder.snapshot() {
			if started, ok := ev.(event.TurnStarted); ok && started.Cause.CommandID == recoveredCommand {
				starts++
			}
		}
		if starts == 1 {
			return
		}
		if starts > 1 {
			t.Fatalf("post-recovery command emitted %d TurnStarted events, want 1", starts)
		}
		if time.Now().After(deadline) {
			t.Fatal("post-recovery command did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTurnPanicReleasesFirstAdmissionForRequiredManualCheckpoint(t *testing.T) {
	for _, tt := range []struct {
		name    string
		client  inference.Client
		runtime loop.RuntimeContextProvider
	}{
		{name: "runtime context provider", client: &stubLLM{chunks: []content.Chunk{textChunk("unused")}}, runtime: panickingRuntimeContext{}},
		{name: "inference client", client: panickingInference{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ws, root := checkpointFixture(t, nil)
			recorder := &recordingEventAppender{}
			opts := []loop.Option{
				loop.WithName("panic-agent"),
				loop.WithInference(tt.client, validModel("panic-model")),
				loop.WithDrainTimeout(100 * time.Millisecond),
			}
			if tt.runtime != nil {
				opts = append(opts, loop.WithRuntimeContext(tt.runtime), loop.WithPolicyRevision("panic-runtime-context"))
			}
			definition := mustDefine(opts...)
			s, err := newTestSession(context.Background(), definition,
				WithEventAppender(recorder),
				withResolvedPlacement(&resolvedPlacement{mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil)}),
				WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotManual, Priority: SnapshotRequired, Timeout: 50 * time.Millisecond}),
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			if _, err := s.Submit(context.Background(), nil); err != nil {
				t.Fatal(err)
			}

			deadline := time.Now().Add(time.Second)
			for {
				failed := false
				for _, ev := range recorder.snapshot() {
					if _, ok := ev.(event.TurnFailed); ok {
						failed = true
						break
					}
				}
				if failed {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("turn panic did not emit TurnFailed")
				}
				time.Sleep(time.Millisecond)
			}

			checkpointCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := s.CheckpointWorkspace(checkpointCtx); err != nil {
				t.Fatalf("required manual checkpoint after TurnFailed: %v", err)
			}
		})
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
	if err := s.faultIfFaulted(); err != nil {
		t.Fatalf("manual not-idle precondition poisoned session: %v", err)
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
