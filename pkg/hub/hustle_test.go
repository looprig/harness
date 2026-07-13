package hub

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

type recordingHustleIdleBoundary struct {
	mu          sync.Mutex
	activations int
	commits     int
}

type transitionGateAppender struct {
	mu sync.Mutex

	appended []event.Event

	activeCalls       int
	blockActiveCall   int
	failBlockedActive bool
	activeEntered     chan struct{}
	activeRelease     chan struct{}

	idleCalls     int
	blockIdleCall int
	idleEntered   chan struct{}
	idleRelease   chan struct{}
}

func (a *transitionGateAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	var entered chan struct{}
	var release chan struct{}
	var fail bool
	a.mu.Lock()
	switch ev.(type) {
	case event.SessionActive:
		a.activeCalls++
		if a.activeCalls == a.blockActiveCall {
			entered, release, fail = a.activeEntered, a.activeRelease, a.failBlockedActive
		}
	case event.SessionIdle:
		a.idleCalls++
		if a.idleCalls == a.blockIdleCall {
			entered, release = a.idleEntered, a.idleRelease
		}
	}
	if entered != nil {
		close(entered)
	}
	a.mu.Unlock()

	if release != nil {
		<-release
	}
	if fail {
		return 0, errAppend
	}
	a.mu.Lock()
	a.appended = append(a.appended, ev)
	seq := uint64(len(a.appended))
	a.mu.Unlock()
	return seq, nil
}

func (a *transitionGateAppender) events() []event.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]event.Event, len(a.appended))
	copy(result, a.appended)
	return result
}

type activityOperationResult struct {
	lease *HustleActivityLease
	err   error
}

func expectActivityOperationBlocked(t *testing.T, result <-chan activityOperationResult, operation string) {
	t.Helper()
	select {
	case got := <-result:
		t.Fatalf("%s returned before the earlier activity transition completed: lease=%v error=%v", operation, got.lease, got.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func receiveActivityOperation(t *testing.T, result <-chan activityOperationResult, operation string) activityOperationResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", operation)
		return activityOperationResult{}
	}
}

func requireEventTypes(t *testing.T, got []event.Event, want ...event.Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("appended count = %d, want %d: %#v", len(got), len(want), got)
	}
	for index := range want {
		if eventTypeName(got[index]) != eventTypeName(want[index]) {
			t.Fatalf("appended[%d] = %T, want %T", index, got[index], want[index])
		}
	}
}

func (b *recordingHustleIdleBoundary) SessionActivated() {
	b.mu.Lock()
	b.activations++
	b.mu.Unlock()
}

func (b *recordingHustleIdleBoundary) CommitSessionIdle(_ context.Context, _ event.SessionIdle, commit func() error) error {
	b.mu.Lock()
	b.commits++
	b.mu.Unlock()
	return commit()
}

func (b *recordingHustleIdleBoundary) calls() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activations, b.commits
}

func testHustleStarted(t *testing.T, sessionID uuid.UUID, runID hustle.RunID) event.HustleStarted {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName("conversation.compact"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1, OutputBytes: 1}),
		hustle.WithCurrentLoopModel(),
		hustle.WithSystemPrompt("private prompt", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
	)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	return event.HustleStarted{
		Header: event.Header{
			Coordinates:     identity.Coordinates{SessionID: sessionID},
			EventID:         mustID(t),
			CreatedAt:       time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
			EventVisibility: event.Internal,
		},
		Run: event.HustleRunDescriptor{Definition: definition.Descriptor(), RunID: runID},
	}
}

func testRunID(t *testing.T) hustle.RunID {
	t.Helper()
	return hustle.RunID(mustID(t))
}

func TestPublishInternalEventCheckedBoundary(t *testing.T) {
	t.Parallel()
	sessionID := mustID(t)
	valid := testHustleStarted(t, sessionID, testRunID(t))
	public := valid
	public.EventVisibility = event.Public
	unknownVisibility := valid
	unknownVisibility.EventVisibility = event.EventVisibility(99)
	wrongSession := valid
	wrongSession.SessionID = mustID(t)
	invalid := valid
	invalid.EventID = uuid.UUID{}
	completed := event.HustleCompleted{
		Header: valid.Header,
		Run:    valid.Run,
	}
	completed.EventID = mustID(t)
	completed.Run.Runtime = event.ModelRuntime{Key: inference.ModelKey{Provider: "test", Model: "model"}}
	failed := event.HustleFailed{
		Header:     valid.Header,
		Run:        valid.Run,
		Stage:      hustle.StageQueue,
		ReasonCode: hustle.ReasonCanceled,
	}
	failed.EventID = mustID(t)
	var nilLifecycle *event.HustleStarted
	tests := []struct {
		name       string
		ev         event.Event
		wantReason PublishBoundaryReason
		wantAppend bool
	}{
		{name: "valid started", ev: valid, wantAppend: true},
		{name: "valid completed", ev: completed, wantAppend: true},
		{name: "valid failed", ev: failed, wantAppend: true},
		{name: "typed nil denied", ev: nilLifecycle, wantReason: PublishBoundaryNilEvent},
		{name: "public denied", ev: public, wantReason: PublishBoundaryVisibility},
		{name: "unknown visibility denied", ev: unknownVisibility, wantReason: PublishBoundaryVisibility},
		{name: "wrong session denied", ev: wrongSession, wantReason: PublishBoundarySession},
		{name: "ephemeral denied", ev: event.TokenDelta{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventVisibility: event.Internal}}, wantReason: PublishBoundaryClass},
		{name: "non hustle enduring denied", ev: event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventVisibility: event.Internal}}, wantReason: PublishBoundaryType},
		{name: "invalid lifecycle denied", ev: invalid, wantReason: PublishBoundaryInvalid},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			appender := &fakeAppender{}
			idleBoundary := &recordingHustleIdleBoundary{}
			h := New(sessionID, WithAppender(appender), WithFactory(testFactory()), withSessionIdleBoundary(idleBoundary))
			sub, err := h.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents() error = %v", err)
			}

			err = h.PublishInternalEventChecked(context.Background(), testCase.ev)
			if testCase.wantAppend {
				if err != nil {
					t.Fatalf("PublishInternalEventChecked() error = %v", err)
				}
				appended := appender.events()
				if len(appended) != 1 || appended[0] != testCase.ev {
					t.Fatalf("appended = %#v, want only triggering event", appended)
				}
				expectNone(t, sub)
				h.mu.RLock()
				phase, active := h.state.phase, len(h.state.active)
				h.mu.RUnlock()
				if phase != SessionIdle || active != 0 {
					t.Fatalf("state after private audit = (%v,%d), want idle/0", phase, active)
				}
				if activations, commits := idleBoundary.calls(); activations != 0 || commits != 0 {
					t.Fatalf("workspace boundary calls = activation:%d idle:%d, want 0/0", activations, commits)
				}
				return
			}

			var boundary *PublishBoundaryError
			if !errors.As(err, &boundary) || boundary.Reason != testCase.wantReason {
				t.Fatalf("error = %T %v, want PublishBoundaryError reason %q", err, err, testCase.wantReason)
			}
			if appender.callCount() != 0 {
				t.Fatalf("append calls = %d, want 0", appender.callCount())
			}
			expectNone(t, sub)
		})
	}
}

func TestPublishInternalEventCheckedPersistenceFault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "append failure returned and reported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessionID := mustID(t)
			appender := &fakeAppender{failAll: true}
			reporter := &recordingReporter{}
			h := New(sessionID, WithAppender(appender), WithFaultReporter(reporter))
			err := h.PublishInternalEventChecked(context.Background(), testHustleStarted(t, sessionID, testRunID(t)))
			var fault *SessionPersistenceFault
			if !errors.As(err, &fault) {
				t.Fatalf("error = %T %v, want SessionPersistenceFault", err, err)
			}
			if got := reporter.reported(); len(got) != 1 || got[0] != fault {
				t.Fatalf("reported = %#v, want returned fault", got)
			}
		})
	}
}

func TestOrdinaryPublicationRejectsNonPublic(t *testing.T) {
	t.Parallel()
	sessionID := mustID(t)
	internal := event.SessionStarted{Header: event.Header{
		Coordinates:     identity.Coordinates{SessionID: sessionID},
		EventVisibility: event.Internal,
	}}
	unknown := internal
	unknown.EventVisibility = event.EventVisibility(99)
	internalTurn := event.TurnStarted{Header: event.Header{
		Coordinates:     identity.Coordinates{SessionID: sessionID, LoopID: mustID(t)},
		EventVisibility: event.Internal,
	}}
	var nilPublic *event.SessionStarted
	tests := []struct {
		name    string
		ev      event.Event
		publish func(*Hub, context.Context, event.Event) error
	}{
		{name: "unchecked rejects internal", ev: internal, publish: (*Hub).PublishEvent},
		{name: "checked rejects internal", ev: internal, publish: (*Hub).PublishEventChecked},
		{name: "unchecked rejects unknown", ev: unknown, publish: (*Hub).PublishEvent},
		{name: "checked rejects unknown", ev: unknown, publish: (*Hub).PublishEventChecked},
		{name: "unchecked rejects mutating internal", ev: internalTurn, publish: (*Hub).PublishEvent},
		{name: "checked rejects mutating internal", ev: internalTurn, publish: (*Hub).PublishEventChecked},
		{name: "unchecked rejects typed nil", ev: nilPublic, publish: (*Hub).PublishEvent},
		{name: "checked rejects typed nil", ev: nilPublic, publish: (*Hub).PublishEventChecked},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			appender := &fakeAppender{}
			h := New(sessionID, WithAppender(appender))
			sub, err := h.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents() error = %v", err)
			}
			err = testCase.publish(h, context.Background(), testCase.ev)
			var boundary *PublishBoundaryError
			if !errors.As(err, &boundary) {
				t.Fatalf("error = %T %v, want PublishBoundaryError", err, err)
			}
			if testCase.ev == nilPublic && boundary.Reason != PublishBoundaryNilEvent {
				t.Fatalf("reason = %q, want nil event", boundary.Reason)
			}
			if testCase.ev != nilPublic && boundary.Reason != PublishBoundaryVisibility {
				t.Fatalf("reason = %q, want visibility", boundary.Reason)
			}
			if appender.callCount() != 0 {
				t.Fatalf("append calls = %d, want 0", appender.callCount())
			}
			h.mu.RLock()
			phase, active := h.state.phase, len(h.state.active)
			h.mu.RUnlock()
			if phase != SessionIdle || active != 0 {
				t.Fatalf("state after denied ordinary publish = (%v,%d), want idle/0", phase, active)
			}
			expectNone(t, sub)
		})
	}
}

func TestOrdinaryPublicationRejectsPublicHustleLifecycle(t *testing.T) {
	t.Parallel()
	sessionID := mustID(t)
	started := testHustleStarted(t, sessionID, testRunID(t))
	started.EventVisibility = event.Public
	completed := event.HustleCompleted{Header: started.Header, Run: started.Run}
	completed.EventID = mustID(t)
	completed.Run.Runtime = event.ModelRuntime{Key: inference.ModelKey{Provider: "test", Model: "model"}}
	failed := event.HustleFailed{
		Header:     started.Header,
		Run:        started.Run,
		Stage:      hustle.StageQueue,
		ReasonCode: hustle.ReasonCanceled,
	}
	failed.EventID = mustID(t)
	tests := []struct {
		name    string
		ev      event.Event
		publish func(*Hub, context.Context, event.Event) error
	}{
		{name: "unchecked rejects public started", ev: started, publish: (*Hub).PublishEvent},
		{name: "checked rejects public started", ev: started, publish: (*Hub).PublishEventChecked},
		{name: "unchecked rejects public completed", ev: completed, publish: (*Hub).PublishEvent},
		{name: "checked rejects public completed", ev: completed, publish: (*Hub).PublishEventChecked},
		{name: "unchecked rejects public failed", ev: failed, publish: (*Hub).PublishEvent},
		{name: "checked rejects public failed", ev: failed, publish: (*Hub).PublishEventChecked},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			appender := &fakeAppender{}
			h := New(sessionID, WithAppender(appender))
			sub, err := h.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents() error = %v", err)
			}

			err = testCase.publish(h, context.Background(), testCase.ev)
			var boundary *PublishBoundaryError
			if !errors.As(err, &boundary) || boundary.Reason != PublishBoundaryType {
				t.Fatalf("error = %T %v, want type PublishBoundaryError", err, err)
			}
			if appender.callCount() != 0 {
				t.Fatalf("append calls = %d, want 0", appender.callCount())
			}
			h.mu.RLock()
			phase, active := h.state.phase, len(h.state.active)
			h.mu.RUnlock()
			if phase != SessionIdle || active != 0 {
				t.Fatalf("state after denied lifecycle = (%v,%d), want idle/0", phase, active)
			}
			expectNone(t, sub)
		})
	}
}

func TestOrdinaryPublicationRejectsHustleLifecyclePointers(t *testing.T) {
	t.Parallel()
	sessionID := mustID(t)
	started := testHustleStarted(t, sessionID, testRunID(t))
	started.EventVisibility = event.Public
	completed := event.HustleCompleted{Header: started.Header, Run: started.Run}
	completed.EventID = mustID(t)
	completed.Run.Runtime = event.ModelRuntime{Key: inference.ModelKey{Provider: "test", Model: "model"}}
	failed := event.HustleFailed{
		Header:     started.Header,
		Run:        started.Run,
		Stage:      hustle.StageQueue,
		ReasonCode: hustle.ReasonCanceled,
	}
	failed.EventID = mustID(t)
	var nilStarted *event.HustleStarted
	var nilCompleted *event.HustleCompleted
	var nilFailed *event.HustleFailed
	tests := []struct {
		name       string
		ev         event.Event
		publish    func(*Hub, context.Context, event.Event) error
		wantReason PublishBoundaryReason
	}{
		{name: "unchecked rejects started pointer", ev: &started, publish: (*Hub).PublishEvent, wantReason: PublishBoundaryType},
		{name: "checked rejects started pointer", ev: &started, publish: (*Hub).PublishEventChecked, wantReason: PublishBoundaryType},
		{name: "unchecked rejects completed pointer", ev: &completed, publish: (*Hub).PublishEvent, wantReason: PublishBoundaryType},
		{name: "checked rejects completed pointer", ev: &completed, publish: (*Hub).PublishEventChecked, wantReason: PublishBoundaryType},
		{name: "unchecked rejects failed pointer", ev: &failed, publish: (*Hub).PublishEvent, wantReason: PublishBoundaryType},
		{name: "checked rejects failed pointer", ev: &failed, publish: (*Hub).PublishEventChecked, wantReason: PublishBoundaryType},
		{name: "unchecked rejects nil started pointer", ev: nilStarted, publish: (*Hub).PublishEvent, wantReason: PublishBoundaryNilEvent},
		{name: "checked rejects nil started pointer", ev: nilStarted, publish: (*Hub).PublishEventChecked, wantReason: PublishBoundaryNilEvent},
		{name: "unchecked rejects nil completed pointer", ev: nilCompleted, publish: (*Hub).PublishEvent, wantReason: PublishBoundaryNilEvent},
		{name: "checked rejects nil completed pointer", ev: nilCompleted, publish: (*Hub).PublishEventChecked, wantReason: PublishBoundaryNilEvent},
		{name: "unchecked rejects nil failed pointer", ev: nilFailed, publish: (*Hub).PublishEvent, wantReason: PublishBoundaryNilEvent},
		{name: "checked rejects nil failed pointer", ev: nilFailed, publish: (*Hub).PublishEventChecked, wantReason: PublishBoundaryNilEvent},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			appender := &fakeAppender{}
			h := New(sessionID, WithAppender(appender))
			sub, err := h.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents() error = %v", err)
			}

			err = testCase.publish(h, context.Background(), testCase.ev)
			var boundary *PublishBoundaryError
			if !errors.As(err, &boundary) || boundary.Reason != testCase.wantReason {
				t.Fatalf("error = %T %v, want PublishBoundaryError reason %q", err, err, testCase.wantReason)
			}
			if appender.callCount() != 0 {
				t.Fatalf("append calls = %d, want 0", appender.callCount())
			}
			h.mu.RLock()
			phase, active := h.state.phase, len(h.state.active)
			h.mu.RUnlock()
			if phase != SessionIdle || active != 0 {
				t.Fatalf("state after denied lifecycle pointer = (%v,%d), want idle/0", phase, active)
			}
			expectNone(t, sub)
		})
	}
}

func TestAcquireHustleActivityLifecycle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "idle active idle durable edges"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessionID := mustID(t)
			appender := &fakeAppender{}
			h := New(sessionID, WithAppender(appender), WithFactory(testFactory()))
			sub, err := h.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents() error = %v", err)
			}
			runID := testRunID(t)
			lease, err := h.AcquireHustleActivity(context.Background(), runID)
			if err != nil || lease == nil {
				t.Fatalf("AcquireHustleActivity() = (%v,%v), want nonnil,nil", lease, err)
			}
			if _, ok := recv(t, sub).(event.SessionActive); !ok {
				t.Fatalf("first delivery is not SessionActive")
			}
			h.mu.RLock()
			_, tracked := h.state.active[activityKey{kind: kindHustle, id: uuid.UUID(runID)}]
			phase := h.state.phase
			h.mu.RUnlock()
			if !tracked || phase != SessionActive {
				t.Fatalf("tracked/phase = %v/%v, want true/active", tracked, phase)
			}

			waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			waited := make(chan error, 1)
			go func() { waited <- h.WaitIdle(waitCtx) }()
			select {
			case waitErr := <-waited:
				t.Fatalf("WaitIdle() returned early: %v", waitErr)
			case <-time.After(25 * time.Millisecond):
			}

			if err := lease.Release(context.Background()); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
			if _, ok := recv(t, sub).(event.SessionIdle); !ok {
				t.Fatalf("second delivery is not SessionIdle")
			}
			if err := <-waited; err != nil {
				t.Fatalf("WaitIdle() error = %v", err)
			}
			appended := appender.events()
			if len(appended) != 2 {
				t.Fatalf("appended count = %d, want 2", len(appended))
			}
			if _, ok := appended[0].(event.SessionActive); !ok {
				t.Fatalf("appended[0] = %T, want SessionActive", appended[0])
			}
			if _, ok := appended[1].(event.SessionIdle); !ok {
				t.Fatalf("appended[1] = %T, want SessionIdle", appended[1])
			}
		})
	}
}

func TestAcquireHustleActivityValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		prepare    func(*Hub, hustle.RunID)
		runID      func(*testing.T) hustle.RunID
		wantReason HustleActivityReason
		wantAbort  bool
	}{
		{name: "zero run id", runID: func(*testing.T) hustle.RunID { return hustle.RunID{} }, wantReason: HustleActivityInvalidRunID},
		{name: "duplicate run id", runID: testRunID, prepare: func(h *Hub, id hustle.RunID) { _, _ = h.AcquireHustleActivity(context.Background(), id) }, wantReason: HustleActivityDuplicate},
		{name: "stopped session", runID: testRunID, prepare: func(h *Hub, _ hustle.RunID) { h.StopSession(context.Background()) }, wantReason: HustleActivityStopped},
		{name: "aborted session", runID: testRunID, prepare: func(h *Hub, _ hustle.RunID) { h.AbortSession(errors.New("construction failed")) }, wantAbort: true},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			h := New(mustID(t), WithFactory(testFactory()))
			runID := testCase.runID(t)
			if testCase.prepare != nil {
				testCase.prepare(h, runID)
			}
			lease, err := h.AcquireHustleActivity(context.Background(), runID)
			if lease != nil {
				t.Fatalf("lease = %v, want nil", lease)
			}
			if testCase.wantAbort {
				var aborted *SessionAbortedError
				if !errors.As(err, &aborted) {
					t.Fatalf("error = %T %v, want SessionAbortedError", err, err)
				}
				return
			}
			var activityErr *HustleActivityError
			if !errors.As(err, &activityErr) || activityErr.Reason != testCase.wantReason {
				t.Fatalf("error = %T %v, want HustleActivityError reason %q", err, err, testCase.wantReason)
			}
		})
	}
}

func TestHustleActivityLeaseReleaseIdempotent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "committed lease releases once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			appender := &fakeAppender{}
			h := New(mustID(t), WithAppender(appender), WithFactory(testFactory()))
			lease, err := h.AcquireHustleActivity(context.Background(), testRunID(t))
			if err != nil {
				t.Fatalf("AcquireHustleActivity() error = %v", err)
			}
			const callers = 16
			releaseErrors := make(chan error, callers)
			var releases sync.WaitGroup
			for range callers {
				releases.Add(1)
				go func() {
					defer releases.Done()
					releaseErrors <- lease.Release(context.Background())
				}()
			}
			releases.Wait()
			close(releaseErrors)
			for err := range releaseErrors {
				if err != nil {
					t.Fatalf("concurrent Release() error = %v", err)
				}
			}
			if appender.callCount() != 2 {
				t.Fatalf("append calls = %d, want active+idle exactly once", appender.callCount())
			}
		})
	}
}

func TestHustleActivityPartialLeaseRollback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "failed active append returns cleanup lease"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			appender := failOnType(event.SessionActive{})
			reporter := &recordingReporter{}
			h := New(mustID(t), WithAppender(appender), WithFactory(testFactory()), WithFaultReporter(reporter))
			sub, err := h.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents() error = %v", err)
			}
			runID := testRunID(t)
			lease, acquireErr := h.AcquireHustleActivity(context.Background(), runID)
			var fault *SessionPersistenceFault
			if lease == nil || !errors.As(acquireErr, &fault) {
				t.Fatalf("AcquireHustleActivity() = (%v,%T %v), want partial lease and persistence fault", lease, acquireErr, acquireErr)
			}
			expectNone(t, sub)
			if releaseErr := lease.Release(context.Background()); !errors.Is(releaseErr, acquireErr) {
				t.Fatalf("Release() error = %v, want cached acquisition error %v", releaseErr, acquireErr)
			}
			if releaseErr := lease.Release(context.Background()); !errors.Is(releaseErr, acquireErr) {
				t.Fatalf("second Release() error = %v, want cached acquisition error %v", releaseErr, acquireErr)
			}
			h.mu.RLock()
			_, tracked := h.state.active[activityKey{kind: kindHustle, id: uuid.UUID(runID)}]
			phase, active := h.state.phase, len(h.state.active)
			h.mu.RUnlock()
			if tracked || phase != SessionIdle || active != 0 {
				t.Fatalf("state after rollback = tracked:%v phase:%v active:%d, want false/idle/0", tracked, phase, active)
			}
			if len(appender.events()) != 0 {
				t.Fatalf("partial release appended an idle edge: %#v", appender.events())
			}
			if got := reporter.reported(); len(got) != 1 || got[0] != fault {
				t.Fatalf("reported = %#v, want acquisition fault once", got)
			}
		})
	}
}

func TestHustleActivityPartialLeaseMintRollback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "failed active mint returns cleanup lease"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			appender := &fakeAppender{}
			reporter := &recordingReporter{}
			factory := event.NewFactory(func() (uuid.UUID, error) {
				return uuid.UUID{}, errAppend
			}, time.Now)
			h := New(mustID(t), WithAppender(appender), WithFactory(factory), WithFaultReporter(reporter))
			runID := testRunID(t)
			lease, acquireErr := h.AcquireHustleActivity(context.Background(), runID)
			var fault *SessionPersistenceFault
			if lease == nil || !errors.As(acquireErr, &fault) {
				t.Fatalf("AcquireHustleActivity() = (%v,%T %v), want partial lease and persistence fault", lease, acquireErr, acquireErr)
			}
			if releaseErr := lease.Release(context.Background()); !errors.Is(releaseErr, acquireErr) {
				t.Fatalf("Release() error = %v, want cached acquisition error %v", releaseErr, acquireErr)
			}
			h.mu.RLock()
			phase, active := h.state.phase, len(h.state.active)
			h.mu.RUnlock()
			if phase != SessionIdle || active != 0 {
				t.Fatalf("state after rollback = (%v,%d), want idle/0", phase, active)
			}
			if appender.callCount() != 0 {
				t.Fatalf("append calls = %d, want 0", appender.callCount())
			}
			if got := reporter.reported(); len(got) != 1 || got[0] != fault {
				t.Fatalf("reported = %#v, want mint fault once", got)
			}
		})
	}
}

func TestHustleActivityLeaseCachesReleaseFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "idle append failure is cached"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			appender := failOnType(event.SessionIdle{})
			reporter := &recordingReporter{}
			h := New(mustID(t), WithAppender(appender), WithFactory(testFactory()), WithFaultReporter(reporter))
			lease, err := h.AcquireHustleActivity(context.Background(), testRunID(t))
			if err != nil {
				t.Fatalf("AcquireHustleActivity() error = %v", err)
			}
			firstErr := lease.Release(context.Background())
			var fault *SessionPersistenceFault
			if !errors.As(firstErr, &fault) {
				t.Fatalf("first Release() error = %T %v, want SessionPersistenceFault", firstErr, firstErr)
			}
			if secondErr := lease.Release(context.Background()); secondErr != firstErr {
				t.Fatalf("second Release() error = %v, want cached %v", secondErr, firstErr)
			}
			appended := appender.events()
			if len(appended) != 1 {
				t.Fatalf("appended = %#v, want only SessionActive", appended)
			}
			if _, ok := appended[0].(event.SessionActive); !ok {
				t.Fatalf("appended[0] = %T, want SessionActive", appended[0])
			}
			if got := reporter.reported(); len(got) != 1 || got[0] != fault {
				t.Fatalf("reported = %#v, want idle append fault once", got)
			}
		})
	}
}

func TestHustleActivityCoexistsWithOtherActivity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "hustle release does not idle busy loop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sessionID, loopID := mustID(t), mustID(t)
			appender := &fakeAppender{}
			h := New(sessionID, WithAppender(appender), WithFactory(testFactory()))
			if err := h.PublishEvent(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}}); err != nil {
				t.Fatalf("PublishEvent(TurnStarted) error = %v", err)
			}
			callsBefore := appender.callCount()
			lease, err := h.AcquireHustleActivity(context.Background(), testRunID(t))
			if err != nil {
				t.Fatalf("AcquireHustleActivity() error = %v", err)
			}
			if err := lease.Release(context.Background()); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
			if appender.callCount() != callsBefore {
				t.Fatalf("hustle on busy session appended phase edge: before=%d after=%d", callsBefore, appender.callCount())
			}
			h.mu.RLock()
			phase, active := h.state.phase, len(h.state.active)
			h.mu.RUnlock()
			if phase != SessionActive || active != 1 {
				t.Fatalf("state = (%v,%d), want active loop retained", phase, active)
			}
		})
	}
}

func TestFailedHustleActiveEdgeSerializesNextAcquireUntilPartialRelease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "next acquire cannot inherit uncommitted active phase"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			firstRunID, secondRunID := testRunID(t), testRunID(t)
			appender := &transitionGateAppender{
				blockActiveCall:   1,
				failBlockedActive: true,
				activeEntered:     make(chan struct{}),
				activeRelease:     make(chan struct{}),
			}
			h := New(mustID(t), WithAppender(appender), WithFaultReporter(&recordingReporter{}))
			firstResult := make(chan activityOperationResult, 1)
			go func() {
				lease, err := h.AcquireHustleActivity(context.Background(), firstRunID)
				firstResult <- activityOperationResult{lease: lease, err: err}
			}()
			<-appender.activeEntered

			secondResult := make(chan activityOperationResult, 1)
			go func() {
				lease, err := h.AcquireHustleActivity(context.Background(), secondRunID)
				secondResult <- activityOperationResult{lease: lease, err: err}
			}()
			expectActivityOperationBlocked(t, secondResult, "second AcquireHustleActivity")

			close(appender.activeRelease)
			first := receiveActivityOperation(t, firstResult, "first AcquireHustleActivity")
			var fault *SessionPersistenceFault
			if first.lease == nil || !errors.As(first.err, &fault) {
				t.Fatalf("first acquire = (%v,%T %v), want partial lease and persistence fault", first.lease, first.err, first.err)
			}
			expectActivityOperationBlocked(t, secondResult, "second AcquireHustleActivity before partial release")
			if err := first.lease.Release(context.Background()); !errors.Is(err, first.err) {
				t.Fatalf("partial Release() error = %v, want %v", err, first.err)
			}

			second := receiveActivityOperation(t, secondResult, "second AcquireHustleActivity after partial release")
			if second.err != nil || second.lease == nil {
				t.Fatalf("second acquire = (%v,%v), want committed lease", second.lease, second.err)
			}
			if err := second.lease.Release(context.Background()); err != nil {
				t.Fatalf("second Release() error = %v", err)
			}
			requireEventTypes(t, appender.events(), event.SessionActive{}, event.SessionIdle{})
		})
	}
}

func TestHustleReleaseSerializesFollowingAcquireEdge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "new active edge cannot commit ahead of older idle edge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			firstRunID, secondRunID := testRunID(t), testRunID(t)
			appender := &transitionGateAppender{
				blockIdleCall: 1,
				idleEntered:   make(chan struct{}),
				idleRelease:   make(chan struct{}),
			}
			h := New(mustID(t), WithAppender(appender))
			firstLease, err := h.AcquireHustleActivity(context.Background(), firstRunID)
			if err != nil {
				t.Fatalf("first AcquireHustleActivity() error = %v", err)
			}
			releaseResult := make(chan activityOperationResult, 1)
			go func() {
				releaseResult <- activityOperationResult{err: firstLease.Release(context.Background())}
			}()
			<-appender.idleEntered

			secondResult := make(chan activityOperationResult, 1)
			go func() {
				lease, acquireErr := h.AcquireHustleActivity(context.Background(), secondRunID)
				secondResult <- activityOperationResult{lease: lease, err: acquireErr}
			}()
			expectActivityOperationBlocked(t, secondResult, "AcquireHustleActivity behind SessionIdle")

			close(appender.idleRelease)
			if released := receiveActivityOperation(t, releaseResult, "first Release"); released.err != nil {
				t.Fatalf("first Release() error = %v", released.err)
			}
			second := receiveActivityOperation(t, secondResult, "second AcquireHustleActivity")
			if second.err != nil || second.lease == nil {
				t.Fatalf("second acquire = (%v,%v), want committed lease", second.lease, second.err)
			}
			if err := second.lease.Release(context.Background()); err != nil {
				t.Fatalf("second Release() error = %v", err)
			}
			requireEventTypes(t, appender.events(),
				event.SessionActive{}, event.SessionIdle{}, event.SessionActive{}, event.SessionIdle{})
		})
	}
}

func TestFailedHustleActiveEdgeSerializesLoopAndWakeActivity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		competitor string
	}{
		{name: "loop activity waits for partial cleanup", competitor: "loop"},
		{name: "wake activity waits for partial cleanup", competitor: "wake"},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			sessionID, competitorID, hustleRunID := mustID(t), mustID(t), testRunID(t)
			appender := &transitionGateAppender{
				blockActiveCall:   1,
				failBlockedActive: true,
				activeEntered:     make(chan struct{}),
				activeRelease:     make(chan struct{}),
			}
			h := New(sessionID, WithAppender(appender), WithFaultReporter(&recordingReporter{}))
			firstResult := make(chan activityOperationResult, 1)
			go func() {
				lease, err := h.AcquireHustleActivity(context.Background(), hustleRunID)
				firstResult <- activityOperationResult{lease: lease, err: err}
			}()
			<-appender.activeEntered

			competitorResult := make(chan activityOperationResult, 1)
			go func() {
				switch testCase.competitor {
				case "loop":
					err := h.PublishEvent(context.Background(), event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: competitorID}}})
					competitorResult <- activityOperationResult{err: err}
				case "wake":
					h.ExpectTurn(context.Background(), competitorID)
					competitorResult <- activityOperationResult{}
				}
			}()
			expectActivityOperationBlocked(t, competitorResult, testCase.competitor+" activity")

			close(appender.activeRelease)
			first := receiveActivityOperation(t, firstResult, "failed hustle acquire")
			if first.lease == nil || first.err == nil {
				t.Fatalf("first acquire = (%v,%v), want partial lease and error", first.lease, first.err)
			}
			expectActivityOperationBlocked(t, competitorResult, testCase.competitor+" activity before partial release")
			if err := first.lease.Release(context.Background()); !errors.Is(err, first.err) {
				t.Fatalf("partial Release() error = %v, want %v", err, first.err)
			}
			if competed := receiveActivityOperation(t, competitorResult, testCase.competitor+" activity after cleanup"); competed.err != nil {
				t.Fatalf("competitor error = %v", competed.err)
			}

			switch testCase.competitor {
			case "loop":
				if err := h.PublishEvent(context.Background(), event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: competitorID}}}); err != nil {
					t.Fatalf("PublishEvent(LoopIdle) error = %v", err)
				}
				requireEventTypes(t, appender.events(),
					event.TurnStarted{}, event.SessionActive{}, event.LoopIdle{}, event.SessionIdle{})
			case "wake":
				h.CancelExpectTurn(context.Background(), competitorID)
				requireEventTypes(t, appender.events(), event.SessionActive{}, event.SessionIdle{})
			}
		})
	}
}

func TestStopBoundaryWaitsForHustleActivityTransition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		stop string
	}{
		{name: "graceful stop follows active edge", stop: "graceful"},
		{name: "construction abort follows active edge", stop: "abort"},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			runID := testRunID(t)
			appender := &transitionGateAppender{
				blockActiveCall: 1,
				activeEntered:   make(chan struct{}),
				activeRelease:   make(chan struct{}),
			}
			h := New(mustID(t), WithAppender(appender))
			acquireResult := make(chan activityOperationResult, 1)
			go func() {
				lease, err := h.AcquireHustleActivity(context.Background(), runID)
				acquireResult <- activityOperationResult{lease: lease, err: err}
			}()
			<-appender.activeEntered

			stopResult := make(chan activityOperationResult, 1)
			go func() {
				switch testCase.stop {
				case "graceful":
					h.StopSession(context.Background())
				case "abort":
					h.AbortSession(errors.New("construction failed"))
				}
				stopResult <- activityOperationResult{}
			}()
			expectActivityOperationBlocked(t, stopResult, testCase.stop+" stop")

			close(appender.activeRelease)
			acquired := receiveActivityOperation(t, acquireResult, "AcquireHustleActivity")
			if acquired.err != nil || acquired.lease == nil {
				t.Fatalf("acquire = (%v,%v), want committed lease", acquired.lease, acquired.err)
			}
			receiveActivityOperation(t, stopResult, testCase.stop+" stop after active edge")
			h.mu.RLock()
			phase, active := h.state.phase, len(h.state.active)
			h.mu.RUnlock()
			if phase != SessionStopped || active != 0 {
				t.Fatalf("state after stop = (%v,%d), want stopped/0", phase, active)
			}
			if testCase.stop == "graceful" {
				requireEventTypes(t, appender.events(), event.SessionActive{}, event.SessionStopped{})
			} else {
				requireEventTypes(t, appender.events(), event.SessionActive{})
			}
		})
	}
}
