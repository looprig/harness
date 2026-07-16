package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/hustleruntime"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
)

type shutdownHustleClient struct{ invoked chan struct{} }

func (c *shutdownHustleClient) Invoke(ctx context.Context, _ inference.Request) (*inference.Response, error) {
	close(c.invoked)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*shutdownHustleClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, &shutdownHustleStreamError{}
}

type shutdownHustleStreamError struct{}

func (*shutdownHustleStreamError) Error() string { return "shutdown test: unexpected stream" }

type gatedShutdownHustleClient struct {
	invoked chan struct{}
	release chan struct{}
}

type ignoringShutdownHustleClient struct {
	invoked chan struct{}
	release chan struct{}
}

func (c *ignoringShutdownHustleClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	close(c.invoked)
	<-c.release
	return nil, context.Canceled
}

func (*ignoringShutdownHustleClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, &shutdownHustleStreamError{}
}

func (c *gatedShutdownHustleClient) Invoke(ctx context.Context, _ inference.Request) (*inference.Response, error) {
	close(c.invoked)
	<-ctx.Done()
	<-c.release
	return nil, ctx.Err()
}

func (*gatedShutdownHustleClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, &shutdownHustleStreamError{}
}

type shutdownOrderRecorder struct {
	mu    sync.Mutex
	steps []string
	added chan struct{}
}

func (r *shutdownOrderRecorder) add(step string) {
	r.mu.Lock()
	r.steps = append(r.steps, step)
	r.mu.Unlock()
	if r.added != nil {
		r.added <- struct{}{}
	}
}

func (r *shutdownOrderRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steps...)
}

type shutdownOrderingAppender struct{ order *shutdownOrderRecorder }

func (a shutdownOrderingAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	switch ev.(type) {
	case event.HustleStarted:
		a.order.add("hustle-started")
	case event.HustleFailed:
		a.order.add("hustle-failed")
	case event.SessionIdle:
		a.order.add("session-idle")
	case event.SessionStopped:
		a.order.add("session-stopped")
	}
	return 1, nil
}

type shutdownPhaseAppender struct {
	stopped chan error
}

func (a shutdownPhaseAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	if _, ok := ev.(event.SessionStopped); ok {
		a.stopped <- ctx.Err()
	}
	return 1, nil
}

type shutdownBlockingAppender struct{ started chan struct{} }

func (a shutdownBlockingAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	if _, ok := ev.(event.SessionStopped); !ok {
		return 1, nil
	}
	close(a.started)
	<-ctx.Done()
	return 0, ctx.Err()
}

func newShutdownHustleSession(t *testing.T, client inference.Client, order *shutdownOrderRecorder) *Session {
	t.Helper()
	sessionID := mustUUID()
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	factory := event.NewFactory(uuid.New, time.Now)
	definition, err := hustle.Define(
		hustle.WithName("shutdown-order"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Minute),
		hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 1024}),
		hustle.WithNamedInference(client, validModel("shutdown-order")),
		hustle.WithSystemPrompt("return json", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
	)
	if err != nil {
		t.Fatalf("hustle.Define: %v", err)
	}
	s := &Session{
		sessionID: sessionID, sessionCtx: sessionCtx, sessionCancel: sessionCancel,
		loops: map[uuid.UUID]*loopHandle{}, newID: uuid.New, now: time.Now, factory: factory,
		hustleDefinitions: []hustle.Definition{definition}, hustleLimits: testHustleLimits(),
		wsRootRelease: func(context.Context) error { order.add("root-lease-release"); return nil },
		leaseRelease:  func(context.Context) error { order.add("session-lease-release"); return nil },
	}
	s.hub = hub.New(sessionID, hub.WithFactory(factory), hub.WithAppender(shutdownOrderingAppender{order: order}), hub.WithFaultReporter(s))
	if err := s.bindSessionHustles(); err != nil {
		t.Fatalf("bindSessionHustles: %v", err)
	}
	return s
}

func TestShutdownDrainsOwnedHustleBeforeStoppingSessionOrReleasingLeases(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "owned finalizer and activity drain before terminal teardown"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			order := &shutdownOrderRecorder{}
			client := &shutdownHustleClient{invoked: make(chan struct{})}
			s := newShutdownHustleSession(t, client, order)
			finalizerEntered := make(chan struct{})
			releaseFinalizer := make(chan struct{})
			defer func() {
				select {
				case <-releaseFinalizer:
				default:
					close(releaseFinalizer)
				}
			}()
			runDone := make(chan error, 1)
			go func() {
				runDone <- s.hustleController.RunAndFinalize(
					context.Background(),
					hustle.Request{
						Name:  "shutdown-order",
						Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: mustUUID()}},
						Input: []byte(`{"version":1}`),
					},
					func(context.Context, hustle.Result) error { return nil },
					func(ctx context.Context, outcome hustle.Outcome) error {
						if outcome.Err == nil || !errors.Is(outcome.Err, context.Canceled) {
							t.Errorf("shutdown finalizer outcome = %v, want cancellation", outcome.Err)
						}
						order.add("finalizer-entered")
						close(finalizerEntered)
						select {
						case <-releaseFinalizer:
							order.add("finalizer-done")
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					},
				)
			}()
			<-client.invoked

			shutdownDone := make(chan error, 1)
			go func() { shutdownDone <- s.Shutdown(context.Background()) }()

			select {
			case <-finalizerEntered:
			case err := <-shutdownDone:
				t.Fatalf("Shutdown returned %v before the owned finalizer entered", err)
			}
			select {
			case err := <-shutdownDone:
				t.Fatalf("Shutdown returned %v before the owned finalizer completed", err)
			default:
			}
			select {
			case <-s.sessionCtx.Done():
				t.Fatal("session context canceled before hustle drain")
			default:
			}
			for _, step := range order.snapshot() {
				if step == "session-stopped" || step == "root-lease-release" || step == "session-lease-release" {
					t.Fatalf("terminal teardown step %q occurred before hustle drain: %v", step, order.snapshot())
				}
			}

			close(releaseFinalizer)
			if err := <-runDone; err == nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("RunAndFinalize error = %v, want cancellation", err)
			}
			if err := <-shutdownDone; err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			want := []string{
				"hustle-started", "hustle-failed", "finalizer-entered", "finalizer-done",
				"session-idle", "session-stopped", "root-lease-release", "session-lease-release",
			}
			if got := order.snapshot(); !equalShutdownOrder(got, want) {
				t.Fatalf("shutdown order = %v, want %v", got, want)
			}
			select {
			case <-s.sessionCtx.Done():
			default:
				t.Fatal("session context remains live after teardown")
			}
		})
	}
}

func TestShutdownCallerCancellationIsReportedAfterOwnedHustleDrain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		newCtx  func() (context.Context, context.CancelFunc)
		wantErr error
	}{
		{name: "caller cancellation", newCtx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, wantErr: context.Canceled},
		{name: "caller deadline", newCtx: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}, wantErr: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			order := &shutdownOrderRecorder{}
			client := &shutdownHustleClient{invoked: make(chan struct{})}
			s := newShutdownHustleSession(t, client, order)
			finalizerEntered := make(chan struct{})
			releaseFinalizer := make(chan struct{})
			defer func() {
				select {
				case <-releaseFinalizer:
				default:
					close(releaseFinalizer)
				}
			}()
			runDone := make(chan error, 1)
			go func() {
				runDone <- s.hustleController.RunAndFinalize(
					context.Background(),
					hustle.Request{
						Name:  "shutdown-order",
						Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: mustUUID()}},
						Input: []byte(`{"version":1}`),
					},
					func(context.Context, hustle.Result) error { return nil },
					func(context.Context, hustle.Outcome) error {
						close(finalizerEntered)
						<-releaseFinalizer
						return nil
					},
				)
			}()
			<-client.invoked
			callerCtx, cancelCaller := tt.newCtx()
			defer cancelCaller()
			shutdownDone := make(chan error, 1)
			go func() { shutdownDone <- s.Shutdown(callerCtx) }()
			select {
			case <-finalizerEntered:
			case err := <-shutdownDone:
				t.Fatalf("Shutdown returned %v before owned cleanup despite caller cancellation", err)
			}
			select {
			case err := <-shutdownDone:
				t.Fatalf("Shutdown abandoned owned cleanup with %v", err)
			default:
			}
			close(releaseFinalizer)
			if err := <-runDone; err == nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("RunAndFinalize error = %v, want cancellation", err)
			}
			err := <-shutdownDone
			var sessionErr *SessionError
			if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionContextDone || !errors.Is(err, tt.wantErr) {
				t.Fatalf("Shutdown error = %T %v, want SessionContextDone wrapping %v", err, err, tt.wantErr)
			}
			steps := order.snapshot()
			if len(steps) < 3 || steps[len(steps)-3] != "session-stopped" || steps[len(steps)-2] != "root-lease-release" || steps[len(steps)-1] != "session-lease-release" {
				t.Fatalf("caller cancellation skipped terminal teardown: %v", steps)
			}
		})
	}
}

func TestConcurrentShutdownCallersJoinOneTeardown(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "one loop shutdown command and one cleanup result"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessionID := mustUUID()
			loopID := mustUUID()
			commands := make(chan command.Command)
			done := make(chan struct{})
			sessionCtx, sessionCancel := context.WithCancel(context.Background())
			s := &Session{
				sessionID: sessionID, sessionCtx: sessionCtx, sessionCancel: sessionCancel,
				hub: hub.New(sessionID), newID: uuid.New, now: time.Now,
				loops: map[uuid.UUID]*loopHandle{
					loopID: {backend: &channelBackend{Commands: commands, Done: done}},
				},
				activeLoopID: loopID,
			}
			results := make(chan error, 2)
			go func() { results <- s.Shutdown(context.Background()) }()
			first := <-commands
			shutdown, ok := first.(command.Shutdown)
			if !ok {
				t.Fatalf("first command = %T, want command.Shutdown", first)
			}
			go func() { results <- s.Shutdown(context.Background()) }()
			shutdown.Ack <- nil

			for received := 0; received < 2; received++ {
				select {
				case err := <-results:
					if err != nil {
						t.Fatalf("Shutdown caller %d: %v", received, err)
					}
				case duplicate := <-commands:
					if duplicateShutdown, duplicateOK := duplicate.(command.Shutdown); duplicateOK {
						duplicateShutdown.Ack <- nil
					}
					t.Fatalf("concurrent Shutdown sent duplicate loop command %T", duplicate)
				case <-time.After(2 * time.Second):
					t.Fatal("concurrent Shutdown caller did not join completed teardown")
				}
			}
		})
	}
}

func TestShutdownPreservesCallerAndLoopFailures(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "typed multi-cause shutdown result"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessionID := mustUUID()
			loopID := mustUUID()
			commands := make(chan command.Command)
			done := make(chan struct{})
			sessionCtx, sessionCancel := context.WithCancel(context.Background())
			s := &Session{
				sessionID: sessionID, sessionCtx: sessionCtx, sessionCancel: sessionCancel,
				hub: hub.New(sessionID), newID: uuid.New, now: time.Now,
				loops: map[uuid.UUID]*loopHandle{
					loopID: {backend: &channelBackend{Commands: commands, Done: done}},
				},
				activeLoopID: loopID,
			}
			callerCtx, cancelCaller := context.WithCancel(context.Background())
			cancelCaller()
			result := make(chan error, 1)
			go func() { result <- s.Shutdown(callerCtx) }()
			raw := <-commands
			shutdown, ok := raw.(command.Shutdown)
			if !ok {
				t.Fatalf("command = %T, want command.Shutdown", raw)
			}
			loopErr := &command.LoopTerminatedError{Cause: &shutdownHustleStreamError{}}
			shutdown.Ack <- loopErr
			err := <-result
			var sessionErr *SessionError
			var terminated *command.LoopTerminatedError
			if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionContextDone ||
				!errors.As(err, &terminated) || !errors.Is(err, context.Canceled) {
				t.Fatalf("Shutdown error = %T %v, want caller cancellation and loop termination", err, err)
			}
		})
	}
}

func TestShutdownClosesHustleAdmissionThenStopsLoopsBeforeJoiningHustleDrain(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "loop shutdown runs with hub and session context live"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			order := &shutdownOrderRecorder{}
			client := &gatedShutdownHustleClient{invoked: make(chan struct{}), release: make(chan struct{})}
			s := newShutdownHustleSession(t, client, order)
			loopID := mustUUID()
			commands := make(chan command.Command)
			done := make(chan struct{})
			s.loops[loopID] = &loopHandle{backend: &channelBackend{Commands: commands, Done: done}}
			s.activeLoopID = loopID
			finalizerEntered := make(chan struct{})
			releaseFinalizer := make(chan struct{})
			defer func() {
				select {
				case <-client.release:
				default:
					close(client.release)
				}
				select {
				case <-releaseFinalizer:
				default:
					close(releaseFinalizer)
				}
			}()
			runDone := make(chan error, 1)
			go func() {
				runDone <- s.hustleController.RunAndFinalize(
					context.Background(),
					hustle.Request{
						Name:  "shutdown-order",
						Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: loopID}},
						Input: []byte(`{"version":1}`),
					},
					func(context.Context, hustle.Result) error { return nil },
					func(context.Context, hustle.Outcome) error {
						order.add("finalizer-entered")
						close(finalizerEntered)
						<-releaseFinalizer
						order.add("finalizer-done")
						return nil
					},
				)
			}()
			<-client.invoked
			shutdownDone := make(chan error, 1)
			go func() { shutdownDone <- s.Shutdown(context.Background()) }()

			raw := <-commands
			shutdown, ok := raw.(command.Shutdown)
			if !ok {
				t.Fatalf("loop command = %T, want command.Shutdown", raw)
			}
			order.add("loop-shutdown")
			admissionErr := s.hustleController.RunAndFinalize(
				context.Background(),
				hustle.Request{
					Name:  "shutdown-order",
					Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: loopID}},
					Input: []byte(`{"version":1}`),
				},
				func(context.Context, hustle.Result) error { return nil },
				func(context.Context, hustle.Outcome) error { return nil },
			)
			var closed *hustleruntime.AdmissionError
			if !errors.As(admissionErr, &closed) || closed.Reason != hustleruntime.AdmissionClosed {
				t.Fatalf("post-closing admission error = %T %v, want AdmissionClosed", admissionErr, admissionErr)
			}
			select {
			case <-s.sessionCtx.Done():
				t.Fatal("session context canceled while graceful loop shutdown is in progress")
			default:
			}
			close(client.release)
			<-finalizerEntered
			shutdown.Ack <- nil
			order.add("loop-joined")
			select {
			case err := <-shutdownDone:
				t.Fatalf("Shutdown returned %v before hustle finalizer drain", err)
			default:
			}
			for _, step := range order.snapshot() {
				if step == "session-stopped" || step == "root-lease-release" || step == "session-lease-release" {
					t.Fatalf("terminal teardown %q raced hustle drain: %v", step, order.snapshot())
				}
			}
			close(releaseFinalizer)
			if err := <-runDone; err == nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("RunAndFinalize error = %v, want cancellation", err)
			}
			if err := <-shutdownDone; err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			steps := order.snapshot()
			if indexShutdownStep(steps, "loop-shutdown") > indexShutdownStep(steps, "session-stopped") ||
				indexShutdownStep(steps, "finalizer-done") > indexShutdownStep(steps, "session-stopped") ||
				indexShutdownStep(steps, "session-idle") > indexShutdownStep(steps, "session-stopped") {
				t.Fatalf("unsafe shutdown order: %v", steps)
			}
		})
	}
}

func TestShutdownDrainsPoisonedHustleOwnershipBeforeAbandonedWorkerTeardown(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "capability-free ignored-cancellation worker is the sole abandonment"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			order := &shutdownOrderRecorder{}
			client := &ignoringShutdownHustleClient{invoked: make(chan struct{}), release: make(chan struct{})}
			defer close(client.release)
			s := newShutdownHustleSession(t, client, order)
			finalized := make(chan struct{})
			runDone := make(chan error, 1)
			go func() {
				runDone <- s.hustleController.RunAndFinalize(
					context.Background(),
					hustle.Request{
						Name:  "shutdown-order",
						Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: mustUUID()}},
						Input: []byte(`{"version":1}`),
					},
					func(context.Context, hustle.Result) error { return nil },
					func(context.Context, hustle.Outcome) error {
						order.add("finalizer-done")
						close(finalized)
						return nil
					},
				)
			}()
			<-client.invoked
			shutdownDone := make(chan error, 1)
			go func() { shutdownDone <- s.Shutdown(context.Background()) }()
			select {
			case <-finalized:
			case err := <-shutdownDone:
				t.Fatalf("Shutdown returned %v before poisoned ownership finalized", err)
			case <-time.After(2 * time.Second):
				t.Fatal("ignored-cancellation worker did not reach bounded poison cleanup")
			}
			if err := <-runDone; err == nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("RunAndFinalize error = %v, want cancellation", err)
			}
			if err := <-shutdownDone; err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			s.loopsMu.RLock()
			faulted := s.faulted
			s.loopsMu.RUnlock()
			if !faulted {
				t.Fatal("ignored-cancellation worker did not poison and fault the session")
			}
			steps := order.snapshot()
			if indexShutdownStep(steps, "finalizer-done") >= indexShutdownStep(steps, "session-stopped") {
				t.Fatalf("SessionStopped preceded poisoned ownership finalizer: %v", steps)
			}
		})
	}
}

func TestHustleFinalizerShutdownReentryIsRefusedWithoutBlockingOwner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		queued bool
	}{
		{name: "executing finalizer reentry"},
		{name: "queued finalizer reentry", queued: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			order := &shutdownOrderRecorder{added: make(chan struct{}, 16)}
			client := &gatedShutdownHustleClient{invoked: make(chan struct{}), release: make(chan struct{})}
			s := newShutdownHustleSession(t, client, order)
			reentrant := make(chan error, 1)
			run := func(finalizer func(context.Context, hustle.Outcome) error) <-chan error {
				result := make(chan error, 1)
				go func() {
					result <- s.hustleController.RunAndFinalize(
						context.Background(),
						hustle.Request{
							Name:  "shutdown-order",
							Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: mustUUID()}},
							Input: []byte(`{"version":1}`),
						},
						func(context.Context, hustle.Result) error { return nil },
						finalizer,
					)
				}()
				return result
			}
			reentrantFinalizer := func(ctx context.Context, _ hustle.Outcome) error {
				err := s.Shutdown(ctx)
				reentrant <- err
				return err
			}
			var activeDone <-chan error
			var reentrantDone <-chan error
			if tt.queued {
				activeDone = run(func(context.Context, hustle.Outcome) error { return nil })
				<-client.invoked
				reentrantDone = run(reentrantFinalizer)
				waitForShutdownSteps(t, order, "hustle-started", 2)
			} else {
				reentrantDone = run(reentrantFinalizer)
				<-client.invoked
			}
			shutdownDone := make(chan error, 1)
			go func() { shutdownDone <- s.Shutdown(context.Background()) }()
			close(client.release)
			select {
			case err := <-reentrant:
				var reentry *HustleShutdownReentryError
				if !errors.As(err, &reentry) {
					t.Fatalf("reentrant Shutdown error = %T %v, want HustleShutdownReentryError", err, err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("reentrant Shutdown deadlocked with its owning finalizer")
			}
			if err := <-reentrantDone; err == nil {
				t.Fatal("reentrant finalizer run returned nil, want refusal")
			}
			if activeDone != nil {
				if err := <-activeDone; err == nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("active RunAndFinalize error = %v, want cancellation", err)
				}
			}
			outerErr := <-shutdownDone
			if tt.queued {
				var closeErr *hustleruntime.CloseError
				var reentry *HustleShutdownReentryError
				if !errors.As(outerErr, &closeErr) || !errors.As(outerErr, &reentry) {
					t.Fatalf("outer Shutdown error = %T %v, want queued CloseError retaining reentry refusal", outerErr, outerErr)
				}
			} else if outerErr != nil {
				t.Fatalf("outer Shutdown: %v", outerErr)
			}
		})
	}
}

func TestShutdownInternalCleanupTimeoutEscapesWedgedLoopSend(t *testing.T) {
	tests := []struct{ name string }{{name: "session-owned deadline hard-cancels wedged loop send"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := mustUUID()
			loopID := mustUUID()
			commands := make(chan command.Command)
			done := make(chan struct{})
			loopCanceled := make(chan struct{})
			leaseReleased := make(chan struct{})
			sessionCtx, sessionCancel := context.WithCancel(context.Background())
			s := &Session{
				sessionID: sessionID, sessionCtx: sessionCtx, sessionCancel: sessionCancel,
				constructionAbortTimeout: 20 * time.Millisecond,
				hub:                      hub.New(sessionID), newID: uuid.New, now: time.Now,
				loops: map[uuid.UUID]*loopHandle{
					loopID: {
						backend: &channelBackend{Commands: commands, Done: done},
						cancel:  func() { close(loopCanceled) },
					},
				},
				activeLoopID: loopID,
				leaseRelease: func(context.Context) error {
					close(leaseReleased)
					return nil
				},
			}
			result := make(chan error, 1)
			go func() { result <- s.Shutdown(context.Background()) }()
			select {
			case err := <-result:
				var timeoutErr *ShutdownCleanupTimeoutError
				if !errors.As(err, &timeoutErr) || timeoutErr.Phase != ShutdownCleanupLoopSend ||
					!errors.Is(timeoutErr, context.DeadlineExceeded) {
					t.Fatalf("Shutdown error = %T %v, want typed loop-send deadline", err, err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("Shutdown remained blocked on wedged loop send past internal cleanup bound")
			}
			select {
			case <-loopCanceled:
			default:
				t.Fatal("internal cleanup timeout did not hard-cancel wedged loop")
			}
			select {
			case <-leaseReleased:
			default:
				t.Fatal("internal cleanup timeout did not continue safe lease teardown")
			}
		})
	}
}

func TestShutdownLoopDrainTimeoutHardCancelsAndContinuesFreshCleanup(t *testing.T) {
	tests := []struct{ name string }{{name: "ack deadline cancels loop then stops hub and releases lease"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := mustUUID()
			loopID := mustUUID()
			commands := make(chan command.Command, 1)
			done := make(chan struct{})
			loopCanceled := make(chan struct{})
			leaseReleased := make(chan struct{})
			hubStopped := make(chan error, 1)
			sessionCtx, sessionCancel := context.WithCancel(context.Background())
			s := &Session{
				sessionID: sessionID, sessionCtx: sessionCtx, sessionCancel: sessionCancel,
				constructionAbortTimeout: 20 * time.Millisecond,
				hub:                      hub.New(sessionID, hub.WithAppender(shutdownPhaseAppender{stopped: hubStopped})),
				newID:                    uuid.New, now: time.Now,
				loops: map[uuid.UUID]*loopHandle{
					loopID: {
						backend: &channelBackend{Commands: commands, Done: done},
						cancel:  func() { close(loopCanceled) },
					},
				},
				activeLoopID: loopID,
				leaseRelease: func(context.Context) error {
					close(leaseReleased)
					return nil
				},
			}
			err := s.Shutdown(context.Background())
			var timeoutErr *ShutdownCleanupTimeoutError
			if !errors.As(err, &timeoutErr) || timeoutErr.Phase != ShutdownCleanupLoopDrain ||
				!errors.Is(timeoutErr, context.DeadlineExceeded) {
				t.Fatalf("Shutdown error = %T %v, want typed loop-drain deadline", err, err)
			}
			select {
			case <-loopCanceled:
			default:
				t.Fatal("loop-drain timeout did not hard-cancel unresponsive loop")
			}
			select {
			case ctxErr := <-hubStopped:
				if ctxErr != nil {
					t.Fatalf("SessionStopped context error = %v, want fresh live cleanup context", ctxErr)
				}
			default:
				t.Fatal("loop-drain timeout skipped hub stop")
			}
			select {
			case <-leaseReleased:
			default:
				t.Fatal("loop-drain timeout skipped lease release")
			}
		})
	}
}

func TestShutdownCheckpointTimeoutContinuesFreshHubAndLeaseCleanup(t *testing.T) {
	tests := []struct{ name string }{{name: "checkpoint drain deadline cannot suppress later teardown"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := mustUUID()
			hubStopped := make(chan error, 1)
			leaseReleased := make(chan struct{})
			sessionCtx, sessionCancel := context.WithCancel(context.Background())
			checkpointCtx, checkpointCancel := context.WithCancel(context.Background())
			s := &Session{
				sessionID: sessionID, sessionCtx: sessionCtx, sessionCancel: sessionCancel,
				constructionAbortTimeout: 20 * time.Millisecond,
				hub:                      hub.New(sessionID, hub.WithAppender(shutdownPhaseAppender{stopped: hubStopped})),
				newID:                    uuid.New, now: time.Now, loops: map[uuid.UUID]*loopHandle{},
				checkpoints: &checkpointController{ctx: checkpointCtx, cancel: checkpointCancel, drained: make(chan struct{})},
				leaseRelease: func(context.Context) error {
					close(leaseReleased)
					return nil
				},
			}
			err := s.Shutdown(context.Background())
			var timeoutErr *ShutdownCleanupTimeoutError
			if !errors.As(err, &timeoutErr) || timeoutErr.Phase != ShutdownCleanupCheckpointDrain ||
				!errors.Is(timeoutErr, context.DeadlineExceeded) {
				t.Fatalf("Shutdown error = %T %v, want typed checkpoint-drain deadline", err, err)
			}
			if !errors.Is(checkpointCtx.Err(), context.Canceled) {
				t.Fatalf("checkpoint context error = %v, want cancellation", checkpointCtx.Err())
			}
			select {
			case ctxErr := <-hubStopped:
				if ctxErr != nil {
					t.Fatalf("SessionStopped context error = %v, want fresh live cleanup context", ctxErr)
				}
			default:
				t.Fatal("checkpoint timeout skipped hub stop")
			}
			select {
			case <-leaseReleased:
			default:
				t.Fatal("checkpoint timeout skipped lease release")
			}
		})
	}
}

func TestShutdownHubStopUsesFiniteSessionOwnedDeadline(t *testing.T) {
	tests := []struct{ name string }{{name: "blocked durable stop returns typed timeout before lease release"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := mustUUID()
			appendStarted := make(chan struct{})
			leaseReleased := make(chan struct{})
			sessionCtx, sessionCancel := context.WithCancel(context.Background())
			s := &Session{
				sessionID: sessionID, sessionCtx: sessionCtx, sessionCancel: sessionCancel,
				constructionAbortTimeout: 20 * time.Millisecond,
				hub:                      hub.New(sessionID, hub.WithAppender(shutdownBlockingAppender{started: appendStarted})),
				newID:                    uuid.New, now: time.Now, loops: map[uuid.UUID]*loopHandle{},
				leaseRelease: func(context.Context) error {
					close(leaseReleased)
					return nil
				},
			}
			result := make(chan error, 1)
			go func() { result <- s.Shutdown(context.Background()) }()
			select {
			case <-appendStarted:
			case <-time.After(500 * time.Millisecond):
				t.Fatal("SessionStopped append did not begin")
			}
			select {
			case err := <-result:
				var timeoutErr *ShutdownCleanupTimeoutError
				if !errors.As(err, &timeoutErr) || timeoutErr.Phase != ShutdownCleanupHubStop ||
					!errors.Is(timeoutErr, context.DeadlineExceeded) {
					t.Fatalf("Shutdown error = %T %v, want typed hub-stop deadline", err, err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("Shutdown remained blocked past hub-stop deadline")
			}
			select {
			case <-leaseReleased:
			default:
				t.Fatal("hub-stop timeout skipped lease release")
			}
		})
	}
}

func TestShutdownNeverDetachesOwnedHustleCleanupAtOuterBudget(t *testing.T) {
	tests := []struct {
		name   string
		queued bool
	}{
		{name: "executing finalizer remains owned"},
		{name: "queued finalizer remains owned", queued: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			order := &shutdownOrderRecorder{added: make(chan struct{}, 32)}
			client := &shutdownHustleClient{invoked: make(chan struct{})}
			s := newShutdownHustleSession(t, client, order)
			// Shrink only Shutdown's computed outer budget. The already-bound
			// controller retains its trusted one-second callback bounds.
			s.constructionAbortTimeout = 15 * time.Millisecond
			s.hustleLimits = HustleLimits{}
			finalizerEntered := make(chan struct{})
			releaseFinalizer := make(chan struct{})
			defer closeIfOpen(releaseFinalizer)

			run := func(finalizer hustleruntime.Finalizer) <-chan error {
				result := make(chan error, 1)
				go func() {
					result <- s.hustleController.RunAndFinalize(
						context.Background(),
						hustle.Request{
							Name:  "shutdown-order",
							Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: mustUUID()}},
							Input: []byte(`{"version":1}`),
						},
						func(context.Context, hustle.Result) error { return nil },
						finalizer,
					)
				}()
				return result
			}
			ownedFinalizer := func(context.Context, hustle.Outcome) error {
				order.add("owned-finalizer-entered")
				close(finalizerEntered)
				<-releaseFinalizer
				order.add("owned-finalizer-released")
				return nil
			}
			var activeDone <-chan error
			var ownedDone <-chan error
			if tt.queued {
				activeDone = run(func(context.Context, hustle.Outcome) error { return nil })
				<-client.invoked
				ownedDone = run(ownedFinalizer)
				waitForShutdownSteps(t, order, "hustle-started", 2)
			} else {
				ownedDone = run(ownedFinalizer)
				<-client.invoked
			}

			shutdownDone := make(chan error, 1)
			go func() { shutdownDone <- s.Shutdown(context.Background()) }()
			select {
			case <-finalizerEntered:
			case err := <-shutdownDone:
				t.Fatalf("Shutdown returned %v before owned finalizer entered", err)
			}
			select {
			case err := <-shutdownDone:
				t.Fatalf("Shutdown detached owned finalizer after outer budget: %v", err)
			case <-time.After(60 * time.Millisecond):
			}
			for _, step := range order.snapshot() {
				if step == "session-stopped" || step == "root-lease-release" || step == "session-lease-release" {
					t.Fatalf("terminal teardown %q preceded owned finalizer release: %v", step, order.snapshot())
				}
			}

			close(releaseFinalizer)
			if err := <-ownedDone; err == nil {
				t.Fatal("owned run returned nil after shutdown cancellation")
			}
			if activeDone != nil {
				if err := <-activeDone; err == nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("active run error = %v, want cancellation", err)
				}
			}
			if err := <-shutdownDone; err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			steps := order.snapshot()
			if indexShutdownStep(steps, "owned-finalizer-released") >= indexShutdownStep(steps, "session-idle") ||
				indexShutdownStep(steps, "session-idle") >= indexShutdownStep(steps, "session-stopped") ||
				indexShutdownStep(steps, "session-stopped") >= indexShutdownStep(steps, "root-lease-release") ||
				indexShutdownStep(steps, "root-lease-release") >= indexShutdownStep(steps, "session-lease-release") {
				t.Fatalf("unsafe owned-cleanup teardown order: %v", steps)
			}
		})
	}
}

func closeIfOpen(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func TestHustleCleanupTimeoutCoversEveryOwnedBlockingPhase(t *testing.T) {
	const maximumDuration = time.Duration(1<<63 - 1)
	tests := []struct {
		name   string
		limits HustleLimits
		want   time.Duration
	}{
		{
			name: "four audit phases plus finalization and worker drain per ownership slot",
			limits: HustleLimits{
				BlockingConcurrent: 2, BlockingQueued: 3, BackgroundConcurrent: 5, BackgroundQueued: 7,
				AuditTimeout: 2 * time.Millisecond, FinalizationTimeout: 3 * time.Millisecond, WorkerDrainTimeout: 5 * time.Millisecond,
			},
			want: 272 * time.Millisecond,
		},
		{name: "zero and negative bounds contribute nothing", limits: HustleLimits{BlockingConcurrent: -1}, want: 0},
		{
			name: "duration overflow saturates",
			limits: HustleLimits{
				BlockingConcurrent: 2,
				AuditTimeout:       maximumDuration, FinalizationTimeout: maximumDuration, WorkerDrainTimeout: maximumDuration,
			},
			want: maximumDuration,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session{hustleLimits: tt.limits}
			if got := s.hustleCleanupTimeout(); got != tt.want {
				t.Fatalf("hustleCleanupTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func waitForShutdownSteps(t *testing.T, order *shutdownOrderRecorder, target string, count int) {
	t.Helper()
	for {
		seen := 0
		for _, step := range order.snapshot() {
			if step == target {
				seen++
			}
		}
		if seen >= count {
			return
		}
		select {
		case <-order.added:
		case <-time.After(2 * time.Second):
			t.Fatalf("observed %d %q steps, want %d: %v", seen, target, count, order.snapshot())
		}
	}
}

func indexShutdownStep(steps []string, target string) int {
	for index, step := range steps {
		if step == target {
			return index
		}
	}
	return len(steps)
}

func equalShutdownOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
