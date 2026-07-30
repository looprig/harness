package sessionruntime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/tool"
)

// dedupAppender is a controllable eventAppender/eventAppenderResult double: it records
// every event it is asked to append and, per call (in order; the last decision repeats
// once exhausted), reports either a genuinely new append (Appended=true) or a
// deduplicated retry (Appended=false). A non-nil err makes every call fail. Safe for
// concurrent use.
type dedupAppender struct {
	mu        sync.Mutex
	decisions []bool
	appended  []event.Event
	calls     int
	err       error
}

func (a *dedupAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	seq, _, err := a.AppendEventResult(ctx, ev)
	return seq, err
}

func (a *dedupAppender) AppendEventResult(_ context.Context, ev event.Event) (uint64, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return 0, false, a.err
	}
	a.appended = append(a.appended, ev)
	appended := true
	if len(a.decisions) > 0 {
		idx := a.calls - 1
		if idx >= len(a.decisions) {
			idx = len(a.decisions) - 1
		}
		appended = a.decisions[idx]
	}
	return uint64(a.calls), appended, nil
}

func (a *dedupAppender) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *dedupAppender) appendedEvents() []event.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]event.Event, len(a.appended))
	copy(out, a.appended)
	return out
}

// recvProcessEvent reads one event.Delivery within a short timeout, failing the test if
// none arrives.
func recvProcessEvent(t *testing.T, sub *hub.EventSubscription) event.Event {
	t.Helper()
	select {
	case d, ok := <-sub.Events():
		if !ok {
			t.Fatalf("Events() closed unexpectedly")
		}
		return d.Event
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for an event")
		return nil
	}
}

// expectNoProcessEvent asserts no event arrives within a brief window.
func expectNoProcessEvent(t *testing.T, sub *hub.EventSubscription) {
	t.Helper()
	select {
	case d, ok := <-sub.Events():
		if ok {
			t.Fatalf("unexpected event delivered: %T", d.Event)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func processSubscribeAll(t *testing.T, h *hub.Hub) *hub.EventSubscription {
	t.Helper()
	sub, err := h.SubscribeEvents(event.EventFilter{
		Ephemeral: event.LoopScope{All: true},
		Enduring:  event.LoopScope{All: true},
	})
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}
	return sub
}

func mustProcessID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

// validLifecycleMetadata builds Tools-supplied metadata satisfying the closed
// kind/state/reason/timestamp matrix for kind, so tests exercise the publisher's own
// coordinate/owner checks rather than tripping tool.ProcessLifecycleMetadata.Validate.
func validLifecycleMetadata(
	kind tool.ProcessLifecycleKind,
	sessionID, loopID, originID, eventID uuid.UUID,
) tool.ProcessLifecycleMetadata {
	created := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	started := created.Add(time.Second)
	finished := started.Add(time.Second)

	base := tool.ProcessLifecycleMetadata{
		EventID:           eventID,
		Kind:              kind,
		SessionID:         sessionID,
		LoopID:            loopID,
		ProcessHandle:     "proc-handle-1",
		OriginExecutionID: originID,
		ProcessCreatedAt:  created,
	}
	switch kind {
	case tool.ProcessLifecycleStarted, tool.ProcessLifecycleBackgrounded:
		base.State = tool.ProcessLifecycleRunning
		base.ProcessStartedAt = started
	case tool.ProcessLifecycleStopRequested:
		base.State = tool.ProcessLifecycleRunning
		base.ProcessStartedAt = started
		base.Reason = tool.ProcessTerminalInterrupted
	case tool.ProcessLifecycleCompleted:
		base.State = tool.ProcessLifecycleExited
		base.Reason = tool.ProcessTerminalExited
		base.ProcessStartedAt = started
		base.ProcessFinishedAt = finished
		base.HasExitCode = true
		base.ExitCode = 0
	case tool.ProcessLifecycleLost:
		base.State = tool.ProcessLifecycleLostOnRestore
		base.Reason = tool.ProcessTerminalLostOnRestore
		base.ProcessFinishedAt = finished
	}
	return base
}

// TestProcessLifecycleKindsPublishAndBroadcast covers every closed lifecycle kind
// (started/backgrounded/completed/stop-requested/lost): the checked publisher builds
// the matching sealed event, preserves the Tools-supplied Process payload
// byte-for-byte, and the hub delivers it live on the first (genuinely new) append.
func TestProcessLifecycleKindsPublishAndBroadcast(t *testing.T) {
	tests := []struct {
		name   string
		kind   tool.ProcessLifecycleKind
		wantEv func(event.Event) (tool.ProcessLifecycleMetadata, bool)
	}{
		{
			name: "started",
			kind: tool.ProcessLifecycleStarted,
			wantEv: func(ev event.Event) (tool.ProcessLifecycleMetadata, bool) {
				e, ok := ev.(event.ProcessStarted)
				return e.Process, ok
			},
		},
		{
			name: "backgrounded",
			kind: tool.ProcessLifecycleBackgrounded,
			wantEv: func(ev event.Event) (tool.ProcessLifecycleMetadata, bool) {
				e, ok := ev.(event.ProcessBackgrounded)
				return e.Process, ok
			},
		},
		{
			name: "completed",
			kind: tool.ProcessLifecycleCompleted,
			wantEv: func(ev event.Event) (tool.ProcessLifecycleMetadata, bool) {
				e, ok := ev.(event.ProcessCompleted)
				return e.Process, ok
			},
		},
		{
			name: "stop-requested",
			kind: tool.ProcessLifecycleStopRequested,
			wantEv: func(ev event.Event) (tool.ProcessLifecycleMetadata, bool) {
				e, ok := ev.(event.ProcessStopRequested)
				return e.Process, ok
			},
		},
		{
			name: "lost",
			kind: tool.ProcessLifecycleLost,
			wantEv: func(ev event.Event) (tool.ProcessLifecycleMetadata, bool) {
				e, ok := ev.(event.ProcessLost)
				return e.Process, ok
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := mustProcessID(t)
			loopID := mustProcessID(t)
			originID := mustProcessID(t)
			eventID := mustProcessID(t)

			app := &dedupAppender{}
			h := hub.New(sessionID, hub.WithAppender(app))
			sub := processSubscribeAll(t, h)

			pub, err := newCheckedProcessLifecyclePublisher(sessionID, h, func() time.Time {
				return time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
			})
			if err != nil {
				t.Fatalf("newCheckedProcessLifecyclePublisher() error = %v", err)
			}

			meta := validLifecycleMetadata(tt.kind, sessionID, loopID, originID, eventID)
			if err := pub.PublishProcessLifecycle(context.Background(), meta); err != nil {
				t.Fatalf("PublishProcessLifecycle() error = %v, want nil", err)
			}

			ev := recvProcessEvent(t, sub)
			gotProcess, ok := tt.wantEv(ev)
			if !ok {
				t.Fatalf("delivered event = %T, want kind %v's sealed type", ev, tt.kind)
			}
			if !reflect.DeepEqual(gotProcess, meta) {
				t.Errorf("delivered Process payload = %+v, want byte-for-byte %+v", gotProcess, meta)
			}
			header := ev.EventHeader()
			if header.EventID != eventID {
				t.Errorf("header.EventID = %v, want the Tools-supplied %v (never replaced)", header.EventID, eventID)
			}
			if header.SessionID != sessionID || header.LoopID != loopID {
				t.Errorf("header coordinates = %+v, want session=%v loop=%v", header.Coordinates, sessionID, loopID)
			}
			if header.CreatedAt.IsZero() {
				t.Error("header.CreatedAt is zero, want a stamped Harness envelope timestamp")
			}
			if got := app.callCount(); got != 1 {
				t.Errorf("appender calls = %d, want 1", got)
			}
		})
	}
}

// TestProcessLifecycleAppendFailureFaults proves an append failure is never swallowed:
// the publisher returns the hub's fault, and nothing is delivered.
func TestProcessLifecycleAppendFailureFaults(t *testing.T) {
	sessionID := mustProcessID(t)
	loopID := mustProcessID(t)

	wantErr := errors.New("durable append failed")
	app := &dedupAppender{err: wantErr}
	h := hub.New(sessionID, hub.WithAppender(app))
	sub := processSubscribeAll(t, h)

	pub, err := newCheckedProcessLifecyclePublisher(sessionID, h, nil)
	if err != nil {
		t.Fatalf("newCheckedProcessLifecyclePublisher() error = %v", err)
	}

	meta := validLifecycleMetadata(tool.ProcessLifecycleStarted, sessionID, loopID, mustProcessID(t), mustProcessID(t))
	err = pub.PublishProcessLifecycle(context.Background(), meta)
	if err == nil {
		t.Fatal("PublishProcessLifecycle() error = nil, want the append fault")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("PublishProcessLifecycle() error = %v, want it to wrap %v", err, wantErr)
	}
	expectNoProcessEvent(t, sub)
}

// TestProcessLifecycleRejectsOwnerMismatch proves metadata naming a session other than
// the one the publisher is bound to is rejected fail-secure before any durable append.
func TestProcessLifecycleRejectsOwnerMismatch(t *testing.T) {
	owningSession := mustProcessID(t)
	otherSession := mustProcessID(t)
	loopID := mustProcessID(t)

	app := &dedupAppender{}
	h := hub.New(owningSession, hub.WithAppender(app))
	sub := processSubscribeAll(t, h)

	pub, err := newCheckedProcessLifecyclePublisher(owningSession, h, nil)
	if err != nil {
		t.Fatalf("newCheckedProcessLifecyclePublisher() error = %v", err)
	}

	meta := validLifecycleMetadata(tool.ProcessLifecycleStarted, otherSession, loopID, mustProcessID(t), mustProcessID(t))
	err = pub.PublishProcessLifecycle(context.Background(), meta)
	var mismatch *ProcessLifecycleOwnerMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("PublishProcessLifecycle() error = %v, want *ProcessLifecycleOwnerMismatchError", err)
	}
	if mismatch.Want != owningSession || mismatch.Got != otherSession {
		t.Errorf("mismatch = {Want:%v Got:%v}, want {Want:%v Got:%v}", mismatch.Want, mismatch.Got, owningSession, otherSession)
	}
	expectNoProcessEvent(t, sub)
	if got := app.callCount(); got != 0 {
		t.Errorf("appender calls = %d, want 0 (owner mismatch must fail before any durable append)", got)
	}
}

// TestProcessLifecycleRejectsInvalidCoordinatesAndMatrix proves the publisher validates
// the Tools-supplied coordinates and lifecycle matrix before ever appending: a zero
// LoopID, a zero ProcessHandle, a zero pre-persisted EventID, and a kind/state
// combination outside the closed matrix must all fail closed.
func TestProcessLifecycleRejectsInvalidCoordinatesAndMatrix(t *testing.T) {
	sessionID := mustProcessID(t)
	loopID := mustProcessID(t)

	tests := []struct {
		name   string
		mutate func(*tool.ProcessLifecycleMetadata)
	}{
		{
			name:   "zero loop id",
			mutate: func(m *tool.ProcessLifecycleMetadata) { m.LoopID = uuid.UUID{} },
		},
		{
			name:   "empty process handle",
			mutate: func(m *tool.ProcessLifecycleMetadata) { m.ProcessHandle = "" },
		},
		{
			name:   "zero event id",
			mutate: func(m *tool.ProcessLifecycleMetadata) { m.EventID = uuid.UUID{} },
		},
		{
			name: "kind state mismatch outside the closed matrix",
			mutate: func(m *tool.ProcessLifecycleMetadata) {
				m.State = tool.ProcessLifecycleExited // started requires Running
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &dedupAppender{}
			h := hub.New(sessionID, hub.WithAppender(app))
			sub := processSubscribeAll(t, h)

			pub, err := newCheckedProcessLifecyclePublisher(sessionID, h, nil)
			if err != nil {
				t.Fatalf("newCheckedProcessLifecyclePublisher() error = %v", err)
			}

			meta := validLifecycleMetadata(tool.ProcessLifecycleStarted, sessionID, loopID, mustProcessID(t), mustProcessID(t))
			tt.mutate(&meta)

			if err := pub.PublishProcessLifecycle(context.Background(), meta); err == nil {
				t.Fatal("PublishProcessLifecycle() error = nil, want a validation error")
			}
			expectNoProcessEvent(t, sub)
			if got := app.callCount(); got != 0 {
				t.Errorf("appender calls = %d, want 0 (invalid metadata must fail before any durable append)", got)
			}
		})
	}
}

// TestProcessLifecyclePreservesPrePersistedEventID proves the publisher installs the
// Tools-supplied EventID directly onto the durable event's Header and never mints or
// replaces it, for a nonzero id supplied ahead of time.
func TestProcessLifecyclePreservesPrePersistedEventID(t *testing.T) {
	sessionID := mustProcessID(t)
	loopID := mustProcessID(t)
	eventID := mustProcessID(t)

	app := &dedupAppender{}
	h := hub.New(sessionID, hub.WithAppender(app))
	sub := processSubscribeAll(t, h)

	pub, err := newCheckedProcessLifecyclePublisher(sessionID, h, nil)
	if err != nil {
		t.Fatalf("newCheckedProcessLifecyclePublisher() error = %v", err)
	}

	meta := validLifecycleMetadata(tool.ProcessLifecycleStarted, sessionID, loopID, mustProcessID(t), eventID)
	if err := pub.PublishProcessLifecycle(context.Background(), meta); err != nil {
		t.Fatalf("PublishProcessLifecycle() error = %v, want nil", err)
	}

	ev := recvProcessEvent(t, sub)
	if got := ev.EventHeader().EventID; got != eventID {
		t.Errorf("delivered header EventID = %v, want the pre-persisted %v", got, eventID)
	}
	appended := app.appendedEvents()
	if len(appended) != 1 {
		t.Fatalf("appender recorded %d events, want 1", len(appended))
	}
	if got := appended[0].EventHeader().EventID; got != eventID {
		t.Errorf("appended event EventID = %v, want the pre-persisted %v", got, eventID)
	}
}

// TestProcessLifecycleDeduplicatedAppendSkipsBroadcast proves a redelivered publish
// whose append the underlying journal recognizes as already durable (24A's
// AppendResult.Appended=false) durably appends nothing further and broadcasts nothing:
// the publisher still reports success (the record IS durable), but the hub applies and
// delivers only the first, genuinely new append.
func TestProcessLifecycleDeduplicatedAppendSkipsBroadcast(t *testing.T) {
	sessionID := mustProcessID(t)
	loopID := mustProcessID(t)

	app := &dedupAppender{decisions: []bool{true, false}}
	h := hub.New(sessionID, hub.WithAppender(app))
	sub := processSubscribeAll(t, h)

	pub, err := newCheckedProcessLifecyclePublisher(sessionID, h, nil)
	if err != nil {
		t.Fatalf("newCheckedProcessLifecyclePublisher() error = %v", err)
	}

	meta := validLifecycleMetadata(tool.ProcessLifecycleStarted, sessionID, loopID, mustProcessID(t), mustProcessID(t))

	if err := pub.PublishProcessLifecycle(context.Background(), meta); err != nil {
		t.Fatalf("first PublishProcessLifecycle() error = %v, want nil", err)
	}
	recvProcessEvent(t, sub)

	if err := pub.PublishProcessLifecycle(context.Background(), meta); err != nil {
		t.Fatalf("second (deduplicated) PublishProcessLifecycle() error = %v, want nil", err)
	}
	expectNoProcessEvent(t, sub)

	if got := app.callCount(); got != 2 {
		t.Errorf("appender calls = %d, want 2 (both the original and the deduplicated retry reach the journal)", got)
	}
}

// TestProcessLifecycleNewCheckedPublisherRejectsInvalidDeps proves the constructor
// fails loud on a composition-wiring bug rather than deferring to a nil-deref at the
// first publish.
func TestProcessLifecycleNewCheckedPublisherRejectsInvalidDeps(t *testing.T) {
	sessionID := mustProcessID(t)
	h := hub.New(sessionID)

	if _, err := newCheckedProcessLifecyclePublisher(uuid.UUID{}, h, nil); err == nil {
		t.Error("newCheckedProcessLifecyclePublisher() with a zero session id error = nil, want an error")
	}
	if _, err := newCheckedProcessLifecyclePublisher(sessionID, nil, nil); err == nil {
		t.Error("newCheckedProcessLifecyclePublisher() with a nil hub error = nil, want an error")
	}
}

// TestProcessLifecycleBridgeAttachDelegatesToCheckedPublisher proves attaching the
// checked implementation behind sessionProcessServiceBridge (Task 4's stable late-bound
// lifecycle bridge) makes the bridge's PublishProcessLifecycle delegate to it, WITHOUT
// replacing the bridge itself — the same bridge value keeps satisfying
// tool.ProcessLifecyclePublisher before and after attach.
func TestProcessLifecycleBridgeAttachDelegatesToCheckedPublisher(t *testing.T) {
	sessionID := mustProcessID(t)
	loopID := mustProcessID(t)

	bridge, services, err := newSessionProcessServices()
	if err != nil {
		t.Fatalf("newSessionProcessServices() error = %v", err)
	}

	// Before attach: the bridge still reports the explicit unavailable error.
	meta := validLifecycleMetadata(tool.ProcessLifecycleStarted, sessionID, loopID, mustProcessID(t), mustProcessID(t))
	if err := bridge.PublishProcessLifecycle(context.Background(), meta); !errors.Is(err, errSessionProcessServicesUnavailable) {
		t.Fatalf("pre-attach PublishProcessLifecycle() error = %v, want errSessionProcessServicesUnavailable", err)
	}

	app := &dedupAppender{}
	h := hub.New(sessionID, hub.WithAppender(app))
	sub := processSubscribeAll(t, h)
	pub, err := newCheckedProcessLifecyclePublisher(sessionID, h, nil)
	if err != nil {
		t.Fatalf("newCheckedProcessLifecyclePublisher() error = %v", err)
	}
	bridge.attachProcessLifecyclePublisher(pub)

	if services.ProcessLifecyclePublisher() != bridge {
		t.Fatal("SessionResourceServices lifecycle publisher is no longer the stable bridge after attach")
	}

	if err := bridge.PublishProcessLifecycle(context.Background(), meta); err != nil {
		t.Fatalf("post-attach PublishProcessLifecycle() error = %v, want nil", err)
	}
	if _, ok := recvProcessEvent(t, sub).(event.ProcessStarted); !ok {
		t.Fatal("post-attach publish did not deliver a ProcessStarted event")
	}

	// A nil attach never erases an already-attached delegate.
	bridge.attachProcessLifecyclePublisher(nil)
	if err := bridge.PublishProcessLifecycle(context.Background(), meta); err != nil {
		t.Fatalf("PublishProcessLifecycle() after a nil attach error = %v, want nil (delegate must stay attached)", err)
	}
}
