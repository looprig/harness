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
)

type shutdownHustleClient struct{ invoked chan struct{} }

func (c *shutdownHustleClient) Invoke(ctx context.Context, _ inference.Request) (*inference.Response, error) {
	close(c.invoked)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*shutdownHustleClient) Stream(context.Context, inference.Request) (*inference.StreamReader[content.Chunk], error) {
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

func (*ignoringShutdownHustleClient) Stream(context.Context, inference.Request) (*inference.StreamReader[content.Chunk], error) {
	return nil, &shutdownHustleStreamError{}
}

func (c *gatedShutdownHustleClient) Invoke(ctx context.Context, _ inference.Request) (*inference.Response, error) {
	close(c.invoked)
	<-ctx.Done()
	<-c.release
	return nil, ctx.Err()
}

func (*gatedShutdownHustleClient) Stream(context.Context, inference.Request) (*inference.StreamReader[content.Chunk], error) {
	return nil, &shutdownHustleStreamError{}
}

type shutdownOrderRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *shutdownOrderRecorder) add(step string) {
	r.mu.Lock()
	r.steps = append(r.steps, step)
	r.mu.Unlock()
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
