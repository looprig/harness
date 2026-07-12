package sessionruntime

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

type abortBackend struct {
	commands chan command.Command
	done     chan struct{}
}

func (b *abortBackend) CommandSink() chan<- command.Command { return b.commands }
func (b *abortBackend) DoneChan() <-chan struct{}           { return b.done }
func (b *abortBackend) Snapshot(context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	return nil, 0, nil
}

func TestAbortConstructionStopsCollaboratorsWithoutSessionStopped(t *testing.T) {
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	backend := &abortBackend{commands: make(chan command.Command), done: make(chan struct{})}
	appender := &recordingEventAppender{}
	h := hub.New(sid, hub.WithAppender(appender))
	sub, err := h.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	var timerFired atomic.Bool
	var releaseMu sync.Mutex
	var releases []string
	release := func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				t.Fatalf("%s release received canceled context: %v", name, err)
			}
			releaseMu.Lock()
			releases = append(releases, name)
			releaseMu.Unlock()
			return nil
		}
	}
	timer := time.AfterFunc(100*time.Millisecond, func() { timerFired.Store(true) })
	s := &Session{
		sessionID: sid, sessionCtx: ctx, sessionCancel: cancel, hub: h,
		checkpointAdmission: newCheckpointAdmissionGate(),
		loops:               map[uuid.UUID]*loopHandle{lid: {id: lid, backend: backend, cancel: cancel}},
		gates:               map[gate.ID]gateEntry{}, gateTimers: map[gate.ID]*time.Timer{gate.ID(lid): timer},
		wsRootRelease: release("root"), leaseRelease: release("session"),
	}
	latePublish := make(chan error, 1)
	go func() {
		<-ctx.Done()
		latePublish <- s.PublishEventChecked(context.Background(), event.StepDone{})
		close(backend.done)
	}()
	s.abortConstruction(errors.New("construction failed"))

	select {
	case <-backend.done:
	default:
		t.Fatal("abort returned before loop joined")
	}
	if len(s.gateTimers) != 0 {
		t.Fatalf("gate timers remain: %d", len(s.gateTimers))
	}
	time.Sleep(120 * time.Millisecond)
	if timerFired.Load() {
		t.Fatal("gate timer callback survived abort")
	}
	if _, ok := <-sub.Events(); ok {
		t.Fatal("hub subscription remained open")
	}
	for _, appended := range appender.snapshot() {
		if _, stopped := appended.(event.SessionStopped); stopped {
			t.Fatal("construction abort appended SessionStopped")
		}
	}
	var aborted *hub.SessionAbortedError
	if err := <-latePublish; !errors.As(err, &aborted) {
		t.Fatalf("late backend publish error = %T %v, want *hub.SessionAbortedError", err, err)
	}
	if got := len(appender.snapshot()); got != 0 {
		t.Fatalf("late backend touched appender %d times after abort seal", got)
	}
	releaseMu.Lock()
	defer releaseMu.Unlock()
	if got := releases; len(got) != 2 || got[0] != "root" || got[1] != "session" {
		t.Fatalf("cooperative construction releases = %v, want [root session] before return", got)
	}
}

func TestAbortConstructionTracksBlockingRestoreErroredPrelude(t *testing.T) {
	sid, _ := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	preludeEntered := make(chan struct{})
	preludeRelease := make(chan struct{})
	released := make(chan struct{})
	s := &Session{
		sessionID: sid, sessionCtx: ctx, sessionCancel: cancel, hub: hub.New(sid),
		checkpointAdmission: newCheckpointAdmissionGate(), loops: map[uuid.UUID]*loopHandle{},
		gates: map[gate.ID]gateEntry{}, gateTimers: map[gate.ID]*time.Timer{},
		leaseRelease: func(context.Context) error { close(released); return nil },
	}
	withConstructionAbortTimeout(25 * time.Millisecond)(s)
	start := time.Now()
	s.abortConstructionAfter(errors.New("restore failed"), func(context.Context) {
		close(preludeEntered)
		<-preludeRelease
	})
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond || elapsed > 100*time.Millisecond {
		t.Fatalf("abort with blocked RestoreErrored elapsed = %v, want one bounded deadline", elapsed)
	}
	select {
	case <-preludeEntered:
	default:
		t.Fatal("RestoreErrored prelude never started")
	}
	select {
	case <-released:
		t.Fatal("session lease released while RestoreErrored append remained in flight")
	default:
	}
	close(preludeRelease)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("cleanup owner did not release after RestoreErrored prelude drained")
	}
}

func TestAbortConstructionUsesOneOverallDeadline(t *testing.T) {
	sid, _ := uuid.New()
	first, _ := uuid.New()
	second, _ := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	uncooperative := func() *abortBackend {
		return &abortBackend{commands: make(chan command.Command), done: make(chan struct{})}
	}
	s := &Session{
		sessionID: sid, sessionCtx: ctx, sessionCancel: cancel, hub: hub.New(sid),
		checkpointAdmission: newCheckpointAdmissionGate(),
		loops: map[uuid.UUID]*loopHandle{
			first:  {id: first, backend: uncooperative(), cancel: func() {}},
			second: {id: second, backend: uncooperative(), cancel: func() {}},
		},
		gates: map[gate.ID]gateEntry{}, gateTimers: map[gate.ID]*time.Timer{},
	}
	withConstructionAbortTimeout(25 * time.Millisecond)(s)
	sub, err := s.hub.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	s.abortConstruction(errors.New("failed"))
	elapsed := time.Since(started)
	if elapsed < 20*time.Millisecond || elapsed > 55*time.Millisecond {
		t.Fatalf("abort elapsed = %v, want one shared ~25ms deadline", elapsed)
	}
	if _, open := <-sub.Events(); open {
		t.Fatal("hub cleanup did not continue after abort deadline")
	}
}

func TestAbortConstructionBoundsContextIgnoringCheckpointWorker(t *testing.T) {
	blobs := &ignoringPutBlobs{Blobs: memstore.New().Blobs, entered: make(chan struct{}), release: make(chan struct{})}
	workspace, err := workspacestore.Open(blobs)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	sid, _ := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		sessionID: sid, sessionCtx: ctx, sessionCancel: cancel, hub: hub.New(sid),
		checkpointAdmission: newCheckpointAdmissionGate(), loops: map[uuid.UUID]*loopHandle{},
		gates: map[gate.ID]gateEntry{}, gateTimers: map[gate.ID]*time.Timer{},
	}
	var releaseMu sync.Mutex
	var releases []string
	released := make(chan struct{})
	s.wsRootRelease = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			t.Errorf("root release context = %v, want live", err)
		}
		releaseMu.Lock()
		releases = append(releases, "root")
		releaseMu.Unlock()
		return nil
	}
	s.leaseRelease = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			t.Errorf("session release context = %v, want live", err)
		}
		releaseMu.Lock()
		releases = append(releases, "session")
		releaseMu.Unlock()
		close(released)
		return nil
	}
	withConstructionAbortTimeout(25 * time.Millisecond)(s)
	s.checkpoints = newCheckpointController(checkpointControllerConfig{
		SessionID: sid,
		Policy:    checkpointPolicy{Trigger: checkpointOnStepDone, Priority: checkpointBestEffort, Timeout: time.Second},
		Store:     workspace, Root: root, Mode: PlacementSession, Coordinator: newWorkspaceCoordinator(nil),
		Publisher: checkedPublisherFunc(func(context.Context, event.Event) error { return nil }),
		Factory:   event.NewFactory(uuid.New, time.Now), Idle: func() bool { return true },
	})
	triggerID, _ := uuid.New()
	trigger := event.StepDone{Header: event.Header{EventID: triggerID, Coordinates: identity.Coordinates{SessionID: sid}}}
	if err := s.checkpoints.bestEffortBoundary(context.Background(), trigger); err != nil {
		t.Fatal(err)
	}
	<-blobs.entered
	aborted := make(chan struct{})
	go func() {
		s.abortConstruction(errors.New("construction failed"))
		close(aborted)
	}()
	select {
	case <-aborted:
	case <-time.After(100 * time.Millisecond):
		close(blobs.release)
		t.Fatal("construction abort blocked on context-ignoring checkpoint worker")
	}
	releaseMu.Lock()
	if len(releases) != 0 {
		t.Fatalf("leases released before checkpoint drain: %v", releases)
	}
	releaseMu.Unlock()
	close(blobs.release)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("background cleanup owner did not release after checkpoint drained")
	}
	releaseMu.Lock()
	defer releaseMu.Unlock()
	if got := releases; len(got) != 2 || got[0] != "root" || got[1] != "session" {
		t.Fatalf("background construction releases = %v, want [root session] exactly once", got)
	}
}

type ignoringPutBlobs struct {
	storage.Blobs
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *ignoringPutBlobs) Put(_ context.Context, key string, r io.Reader) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.Blobs.Put(context.Background(), key, r)
}

type checkedPublisherFunc func(context.Context, event.Event) error

func (f checkedPublisherFunc) PublishEventChecked(ctx context.Context, ev event.Event) error {
	return f(ctx, ev)
}
