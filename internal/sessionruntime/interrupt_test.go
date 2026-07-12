package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// interrupt_test.go proves the Task 11 hierarchical-interruption + queue-policy
// contract at the SESSION layer: session-wide / subtree / owned-child interrupt
// scopes, mark-before-fanout, the user-queued vs machine-flushed admission
// distinction, fail-quiet idle interrupts, and the pluggable admission-barrier
// release seam. Concurrency is proven with channels/barriers, never sleeps.

// blockingRelease is a test InterruptReleasePolicy that never releases on its own — it
// parks until the session ctx ends. Used where the test does not care when the barrier
// releases (it only observes fan-out/marking), so the barrier goroutine reaps on cleanup.
type blockingRelease struct{}

func (blockingRelease) AwaitRelease(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

// controllableRelease releases exactly when the test closes release, so barrier release is
// deterministic.
type controllableRelease struct{ release chan struct{} }

func (c controllableRelease) AwaitRelease(ctx context.Context) error {
	select {
	case <-c.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type blockingSessionIdleAppender struct {
	base    recordingEventAppender
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockingSessionIdleAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	if _, ok := ev.(event.SessionIdle); ok {
		a.once.Do(func() { close(a.entered) })
		select {
		case <-a.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return a.base.AppendEvent(ctx, ev)
}

// treeLoop is one node in a fake loop tree: a name and its parent's name ("" = root).
type treeLoop struct {
	name   string
	parent string
}

// fakeTreeSession builds a struct-literal Session whose registry is the given loop tree,
// each backed by an unbuffered channel backend (so a send is observable and blocks until
// received). The first spec is the primary loop. A real hub is wired so the default idle
// release policy is safe; tests pass an explicit policy to control the barrier.
func fakeTreeSession(t *testing.T, policy InterruptReleasePolicy, loops ...treeLoop) (*Session, map[string]uuid.UUID, map[string]chan command.Command) {
	t.Helper()
	ids := make(map[string]uuid.UUID, len(loops))
	for _, l := range loops {
		ids[l.name] = mustUUID()
	}
	cmds := make(map[string]chan command.Command, len(loops))
	registry := make(map[uuid.UUID]*loopHandle, len(loops))
	var primary uuid.UUID
	for i, l := range loops {
		c := make(chan command.Command)
		cmds[l.name] = c
		var parentID uuid.UUID
		if l.parent != "" {
			parentID = ids[l.parent]
		}
		registry[ids[l.name]] = &loopHandle{
			id:      ids[l.name],
			backend: &channelBackend{Commands: c, Done: make(chan struct{})},
			parent:  loop.Provenance{LoopID: parentID},
		}
		if i == 0 {
			primary = ids[l.name]
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Session{
		sessionID:        mustUUID(),
		sessionCtx:       ctx,
		sessionCancel:    cancel,
		loops:            registry,
		primaryLoopID:    primary,
		newID:            uuid.New,
		now:              time.Now,
		hub:              hub.New(mustUUID()),
		interruptRelease: policy,
	}
	for _, h := range registry {
		h.owner = s
	}
	return s, ids, cmds
}

// ackTrue reads one command.Interrupt from cmds and replies the ack with true (a cancelled
// turn). It fails the test if the command is the wrong type or never arrives.
func ackInterrupt(t *testing.T, cmds chan command.Command, cancelled bool) {
	t.Helper()
	select {
	case cmd := <-cmds:
		ic, ok := cmd.(command.Interrupt)
		if !ok {
			t.Errorf("received %T, want command.Interrupt", cmd)
			return
		}
		ic.Ack <- cancelled
	case <-time.After(2 * time.Second):
		t.Error("interrupt never delivered")
	}
}

// TestSessionInterruptConcurrentFanout proves (behavior 1) that a session interrupt reaches
// every live loop CONCURRENTLY. Both loop readers must RECEIVE their interrupt before either
// is allowed to ack (a shared gate): a sequential sender would block on the first loop's ack
// (which never comes until the second loop has also received) and deadlock, so the barrier
// only opens when both sends were simultaneously in flight.
func TestSessionInterruptConcurrentFanout(t *testing.T) {
	t.Parallel()
	s, _, cmds := fakeTreeSession(t, blockingRelease{}, treeLoop{name: "A"}, treeLoop{name: "B", parent: "A"})

	var received sync.WaitGroup
	received.Add(2)
	gate := make(chan struct{})
	reader := func(name string) {
		cmd := <-cmds[name]
		ic := cmd.(command.Interrupt)
		received.Done()
		<-gate // hold the ack until BOTH interrupts have been received
		ic.Ack <- true
	}
	go reader("A")
	go reader("B")

	done := make(chan struct{})
	go func() { _, _ = s.Interrupt(context.Background()); close(done) }()

	bothReceived := make(chan struct{})
	go func() { received.Wait(); close(bothReceived) }()
	select {
	case <-bothReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("session interrupt did not deliver to both loops concurrently (fan-out is sequential)")
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Interrupt never returned after both loops acked")
	}
}

// TestControllerInterruptCoversSubtree proves (behavior 2) that loop.Controller.Interrupt
// covers the loop AND its delegate subtree (root P, child C, grandchild G) but never an
// unrelated loop E.
func TestControllerInterruptCoversSubtree(t *testing.T) {
	t.Parallel()
	s, ids, cmds := fakeTreeSession(t, blockingRelease{},
		treeLoop{name: "P"}, treeLoop{name: "C", parent: "P"}, treeLoop{name: "G", parent: "C"}, treeLoop{name: "E"})

	for _, n := range []string{"P", "C", "G"} {
		go ackInterrupt(t, cmds[n], true)
	}
	// E must never receive an interrupt; a reader records over-reach.
	eGot := make(chan struct{}, 1)
	go func() {
		select {
		case <-cmds["E"]:
			eGot <- struct{}{}
		case <-s.sessionCtx.Done():
		}
	}()

	controller := s.loops[ids["P"]]
	errCh := make(chan error, 1)
	go func() { errCh <- controller.Interrupt(context.Background()) }()

	select {
	case <-eGot:
		t.Fatal("unrelated loop E received an interrupt: subtree selection over-reached")
	case err := <-errCh:
		if err != nil {
			t.Fatalf("controller Interrupt returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("controller Interrupt never completed (a target was not delivered)")
	}
}

// TestControllerInterruptUnknownRoot proves the fail-secure miss: a controller interrupt for
// a loop not in the registry returns SessionLoopNotFound and sends nothing.
func TestControllerInterruptUnknownRoot(t *testing.T) {
	t.Parallel()
	s, _, _ := fakeTreeSession(t, blockingRelease{}, treeLoop{name: "P"})
	err := s.interruptSubtree(context.Background(), mustUUID())
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopNotFound {
		t.Fatalf("interruptSubtree(unknown) = %v, want *SessionError{SessionLoopNotFound}", err)
	}
}

// TestSubagentInterruptAffectsOnlyOwnedChild proves (behavior 3) that a Subagent `interrupt`
// affects ONLY the owned child's current turn — never the parent above it, never a grandchild
// below it. It goes through the parent-scoped controller, which does NOT cascade to the subtree.
func TestSubagentInterruptAffectsOnlyOwnedChild(t *testing.T) {
	t.Parallel()
	s, ids, cmds := fakeTreeSession(t, blockingRelease{},
		treeLoop{name: "P"}, treeLoop{name: "C", parent: "P"}, treeLoop{name: "G", parent: "C"})

	got := make(chan command.Command, 1)
	go func() { got <- <-cmds["C"] }()

	ctrl := &scopedController{parentLoopID: ids["P"]}
	res, err := ctrl.interrupt(s, tool.DelegateRequest{DelegateID: ids["C"]})
	if err != nil {
		t.Fatalf("Subagent interrupt returned %v, want nil", err)
	}
	if res.Status != tool.DelegateStatusInterrupted {
		t.Errorf("status = %v, want Interrupted", res.Status)
	}

	select {
	case cmd := <-got:
		if _, ok := cmd.(command.Interrupt); !ok {
			t.Fatalf("child C received %T, want command.Interrupt", cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owned child C never received its interrupt")
	}
	// The parent and the grandchild must be untouched (single-hop, no subtree cascade).
	select {
	case cmd := <-cmds["P"]:
		t.Fatalf("parent P received %T; a Subagent interrupt must not reach the parent", cmd)
	default:
	}
	select {
	case cmd := <-cmds["G"]:
		t.Fatalf("grandchild G received %T; a Subagent interrupt must not cascade to the subtree", cmd)
	default:
	}
}

// TestInterruptMarksBeforeFanout proves (behavior 4) the mark-before-fanout ordering: at the
// instant the child C's interrupt is DELIVERED, the parent P is ALREADY interrupt-pending. C's
// interrupt is what unblocks a parent's interrupted delegate wait; because P was marked under
// the session lock BEFORE any interrupt was sent, a parent that resumes on C's cancellation can
// no longer open a new delegate step. A "mark after fan-out" implementation fails this.
func TestInterruptMarksBeforeFanout(t *testing.T) {
	t.Parallel()
	s, ids, cmds := fakeTreeSession(t, blockingRelease{}, treeLoop{name: "P"}, treeLoop{name: "C", parent: "P"})

	observed := make(chan bool, 1)
	go func() {
		cmd := <-cmds["C"]
		observed <- s.loopInterruptPending(ids["P"])
		cmd.(command.Interrupt).Ack <- true
	}()
	go ackInterrupt(t, cmds["P"], true)

	if _, err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt returned %v, want nil", err)
	}
	select {
	case pending := <-observed:
		if !pending {
			t.Fatal("parent P was NOT interrupt-pending when child C's interrupt was delivered: marks were not set before fan-out")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("child C never received its interrupt")
	}
}

// TestInterruptQueuePolicy proves the queue policy (behaviors 5 + 6): while a loop is
// interrupt-pending, pending USER input still admits (survives, stays queued), but a
// machine-created delegate request (start / send) is FLUSHED — refused with a typed
// DelegateInterruptPending until the admission barrier releases.
func TestInterruptQueuePolicy(t *testing.T) {
	t.Parallel()
	parentDef := delegateParent(loop.DelegationManaged, "child")
	s := newDelegationSession(t, parentDef, nil, delegateChild("child", "final"))
	ctrl := s.delegation.controllerFor(s.PrimaryLoopID(), parentDef)

	// Start one owned child while NOT pending, so `send` has a real target below.
	res, err := ctrl.Execute(delegateCtx(t), tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "go", Wait: true})
	if err != nil {
		t.Fatalf("initial start: %v", err)
	}
	childID := res.DelegateID

	// Mark the parent (primary) loop interrupt-pending, as a fan-out would.
	primary := s.PrimaryLoopID()
	s.loopsMu.Lock()
	s.markInterruptPendingLocked([]loopSnapshot{{loopID: primary, handle: s.loops[primary]}})
	s.loopsMu.Unlock()

	// Machine delegate admission is flushed (refused) while pending.
	machine := []struct {
		name string
		req  tool.DelegateRequest
	}{
		{name: "start", req: tool.DelegateRequest{Operation: tool.DelegateStart, Agent: "child", Message: "again", Wait: true}},
		{name: "send", req: tool.DelegateRequest{Operation: tool.DelegateSend, DelegateID: childID, Message: "more", Wait: true}},
	}
	for _, tt := range machine {
		t.Run("machine_"+tt.name+"_flushed", func(t *testing.T) {
			_, err := ctrl.Execute(delegateCtx(t), tt.req)
			var de *DelegateError
			if !errors.As(err, &de) || de.Kind != DelegateInterruptPending {
				t.Fatalf("machine %s during interrupt = %v, want DelegateInterruptPending", tt.name, err)
			}
		})
	}

	// User input survives: a human Submit is still admitted (non-zero id, no error).
	id, err := s.Submit(delegateCtx(t), []content.Block{&content.TextBlock{Text: "still mine"}})
	if err != nil {
		t.Fatalf("user Submit refused while interrupt-pending: %v (user input must remain queued)", err)
	}
	if id.IsZero() {
		t.Fatal("user Submit returned a zero id while interrupt-pending")
	}
}

// TestInterruptPendingRefcountedAcrossOverlappingScopes proves that the interrupt-pending marks
// are REFCOUNTED: two overlapping interrupt scopes sharing a target (X+Y and Y+Z) each hold the
// shared target Y independently. Releasing the FIRST barrier clears only its exclusive target (X)
// and decrements Y — Y stays pending because the SECOND barrier still holds it. Releasing the
// second barrier clears Y and Z. Without refcounting the first release would delete Y outright,
// admitting work while the second barrier still logically holds it (a real bug once Task 16's
// workspace policy staggers release). Determinism comes from two controllable release policies.
func TestInterruptPendingRefcountedAcrossOverlappingScopes(t *testing.T) {
	t.Parallel()
	s, ids, cmds := fakeTreeSession(t, nil, treeLoop{name: "X"}, treeLoop{name: "Y"}, treeLoop{name: "Z"})

	// Persistent ackers: Y receives an interrupt from BOTH scopes, so each loop acks every
	// command it gets until the session ctx ends.
	for _, name := range []string{"X", "Y", "Z"} {
		go func() {
			for {
				select {
				case cmd := <-cmds[name]:
					if ic, ok := cmd.(command.Interrupt); ok {
						ic.Ack <- true
					}
				case <-s.sessionCtx.Done():
					return
				}
			}
		}()
	}

	selector := func(names ...string) func() ([]loopSnapshot, bool) {
		return func() ([]loopSnapshot, bool) {
			out := make([]loopSnapshot, 0, len(names))
			for _, n := range names {
				out = append(out, loopSnapshot{loopID: ids[n], handle: s.loops[ids[n]]})
			}
			return out, true
		}
	}
	relA := controllableRelease{release: make(chan struct{})}
	relB := controllableRelease{release: make(chan struct{})}

	// Scope A over {X, Y}: armInterruptBarrier captures relA synchronously, so switching the
	// session policy before scope B is race-free (the field is read only on the test goroutine).
	s.interruptRelease = relA
	anyA, doneA, errA := s.runInterrupt(context.Background(), selector("X", "Y"), identity.AgencyMachine)
	if errA != nil || !anyA || doneA == nil {
		t.Fatalf("scope A: any=%v done=%v err=%v, want cancelled + armed barrier", anyA, doneA, errA)
	}
	// Scope B over {Y, Z}: Y is now held by BOTH scopes (refcount 2).
	s.interruptRelease = relB
	anyB, doneB, errB := s.runInterrupt(context.Background(), selector("Y", "Z"), identity.AgencyMachine)
	if errB != nil || !anyB || doneB == nil {
		t.Fatalf("scope B: any=%v done=%v err=%v, want cancelled + armed barrier", anyB, doneB, errB)
	}
	if !s.loopInterruptPending(ids["Y"]) {
		t.Fatal("shared target Y not pending after both scopes marked it")
	}

	// Release scope A: X (A-exclusive) clears, but Y is STILL held by scope B.
	close(relA.release)
	<-doneA
	if s.loopInterruptPending(ids["X"]) {
		t.Fatal("X still pending after scope A released (X was held only by A)")
	}
	if !s.loopInterruptPending(ids["Y"]) {
		t.Fatal("Y cleared after only scope A released: marks are not refcounted (scope B still holds Y)")
	}

	// Release scope B: Y and Z clear.
	close(relB.release)
	<-doneB
	if s.loopInterruptPending(ids["Y"]) {
		t.Fatal("Y still pending after both scopes released")
	}
	if s.loopInterruptPending(ids["Z"]) {
		t.Fatal("Z still pending after scope B released")
	}
}

// TestIdleInterruptFailQuiet proves (behavior 7) that a fully-idle session interrupt returns
// false and appends NO events: every loop acks false (no turn cancelled) and the session stays
// idle with nothing published.
func TestIdleInterruptFailQuiet(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("hi")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	any, err := s.Interrupt(context.Background())
	if err != nil {
		t.Fatalf("idle Interrupt returned error %v, want nil", err)
	}
	if any {
		t.Fatal("idle Interrupt returned true, want false (no turn was running)")
	}
	// No event may have been published by the idle interrupt: a non-blocking read misses.
	select {
	case ev := <-sub.Events():
		t.Fatalf("idle interrupt published an event %T, want none (fail-quiet)", ev)
	default:
	}
}

// TestInterruptBarrierReleasePolicy proves the pluggable admission-barrier seam: after a
// fan-out that cancelled a running turn, the interrupt-pending marks are HELD until the
// injected release policy fires, then cleared. Task 16 supplies the workspace-aware policy;
// here a controllable policy stands in for it.
func TestInterruptBarrierReleasePolicy(t *testing.T) {
	t.Parallel()
	rel := controllableRelease{release: make(chan struct{})}
	s, ids, cmds := fakeTreeSession(t, rel, treeLoop{name: "A"}, treeLoop{name: "B", parent: "A"})

	for _, n := range []string{"A", "B"} {
		go ackInterrupt(t, cmds[n], true)
	}

	any, done, err := s.runInterrupt(context.Background(), func() ([]loopSnapshot, bool) {
		return s.liveLoopSnapshotLocked(), true
	}, identity.AgencyUser)
	if err != nil {
		t.Fatalf("runInterrupt returned %v, want nil", err)
	}
	if !any {
		t.Fatal("runInterrupt returned any=false, want true (both loops cancelled a turn)")
	}
	if done == nil {
		t.Fatal("no admission barrier was armed after a cancelled turn")
	}
	// Held before release.
	if !s.loopInterruptPending(ids["A"]) || !s.loopInterruptPending(ids["B"]) {
		t.Fatal("interrupt-pending marks were not held before the barrier released")
	}
	// Release, then the marks clear.
	close(rel.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("barrier never released after the policy fired")
	}
	if s.loopInterruptPending(ids["A"]) || s.loopInterruptPending(ids["B"]) {
		t.Fatal("interrupt-pending marks were not cleared after the barrier released")
	}
}

func TestInterruptCheckpointBarrierUsesNativeIdleOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "checkpoint sweep holds marks through session idle acceptance"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, ids, cmds := fakeTreeSession(t, nil, treeLoop{name: "A"}, treeLoop{name: "B", parent: "A"})
			publisher := &boundaryPublisher{order: &checkpointOrder{}}
			s.checkpoints = newCheckpointController(checkpointControllerConfig{
				SessionID: s.sessionID,
				Policy:    checkpointPolicy{Trigger: checkpointManual, Priority: checkpointBestEffort, Timeout: time.Second},
				Publisher: publisher,
			})
			t.Cleanup(s.checkpoints.shutdown)
			go ackInterrupt(t, cmds["A"], true)
			go ackInterrupt(t, cmds["B"], true)

			any, done, err := s.runInterrupt(context.Background(), func() ([]loopSnapshot, bool) {
				return s.liveLoopSnapshotLocked(), true
			}, identity.AgencyUser)
			if err != nil || !any || done == nil {
				t.Fatalf("runInterrupt = any %v, done %v, err %v", any, done, err)
			}
			s.checkpoints.mu.Lock()
			registered := len(s.checkpoints.interruptSweeps)
			s.checkpoints.mu.Unlock()
			if registered != 1 {
				t.Fatalf("registered checkpoint sweeps = %d, want 1 before fan-out idle can release", registered)
			}
			select {
			case <-done:
				t.Fatal("barrier released before checkpoint controller observed SessionIdle")
			default:
			}
			if !s.loopInterruptPending(ids["A"]) || !s.loopInterruptPending(ids["B"]) {
				t.Fatal("a target interrupt mark cleared before every target was idle and SessionIdle committed")
			}

			idle := event.SessionIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: s.sessionID}, EventID: mustUUID()}}
			if err := s.checkpoints.sessionIdle(context.Background(), idle, func() error {
				return publisher.PublishEventChecked(context.Background(), idle)
			}); err != nil {
				t.Fatalf("sessionIdle: %v", err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("barrier did not release from explicit checkpoint outcome")
			}
			if s.loopInterruptPending(ids["A"]) || s.loopInterruptPending(ids["B"]) {
				t.Fatal("target interrupt marks remain after SessionIdle outcome")
			}
		})
	}
}

func TestInterruptPreservedInputDispatchesOnlyAfterSessionIdleAppend(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		workspace bool
	}{
		{name: "manual workspace", workspace: true},
		{name: "no workspace", workspace: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			appender := &blockingSessionIdleAppender{entered: make(chan struct{}), release: make(chan struct{})}
			opts := []Option{WithEventAppender(appender)}
			if tt.workspace {
				ws, root := checkpointFixture(t, nil)
				opts = append(opts,
					withResolvedPlacement(&resolvedPlacement{mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil)}),
					WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotManual, Priority: SnapshotBestEffort, Timeout: time.Second}),
				)
			}
			s, err := newTestSession(context.Background(), cfg(&stubLLM{blockUntilCancel: true}), opts...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			sub, err := s.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sub.Close() })
			if _, err := s.Submit(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			for {
				select {
				case delivery := <-sub.Events():
					if _, ok := delivery.Event.(event.TurnStarted); ok {
						goto firstStarted
					}
				case <-time.After(time.Second):
					t.Fatal("first turn did not start")
				}
			}

		firstStarted:
			any, err := s.Interrupt(context.Background())
			if err != nil || !any {
				t.Fatalf("Interrupt = %v, %v", any, err)
			}
			select {
			case <-appender.entered:
			case <-time.After(time.Second):
				t.Fatal("interrupted terminal did not reach SessionIdle append")
			}
			submitDone := make(chan error, 1)
			go func() {
				_, submitErr := s.Submit(context.Background(), nil)
				submitDone <- submitErr
			}()
			select {
			case err := <-submitDone:
				t.Fatalf("next input dispatched before SessionIdle append completed: %v", err)
			default:
			}
			close(appender.release)
			select {
			case err := <-submitDone:
				if err != nil {
					t.Fatalf("preserved input after idle: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("preserved input did not dispatch after SessionIdle")
			}
			for {
				select {
				case delivery := <-sub.Events():
					if _, ok := delivery.Event.(event.TurnStarted); ok {
						goto secondStarted
					}
				case <-time.After(time.Second):
					t.Fatal("preserved input never started a turn after SessionIdle")
				}
			}

		secondStarted:
			events := appender.base.snapshot()
			idleAt, secondStartAt := -1, -1
			starts := 0
			for i, ev := range events {
				switch ev.(type) {
				case event.SessionIdle:
					idleAt = i
				case event.TurnStarted:
					starts++
					if starts == 2 {
						secondStartAt = i
					}
				}
			}
			if idleAt < 0 || secondStartAt < 0 || secondStartAt <= idleAt {
				t.Fatalf("event order idle=%d second-start=%d events=%T", idleAt, secondStartAt, events)
			}
		})
	}
}
