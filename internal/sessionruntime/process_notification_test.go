package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// idempotentFakeCommandAppender is a commandAppenderResult double that
// reproduces the real journal's per-CommandID idempotency semantics (24A) in
// memory over a genuine journal.IdempotencyIndex: an identical retry of the
// SAME CommandID reports AppendResult{Appended:false} at its original
// sequence without being counted as a new frame, and a retry with a
// DIFFERENT persisted payload fails closed with the real
// *journal.IdempotencyCollisionError. Fingerprinting marshals the command
// exactly as a real backend would (command.MarshalCommand drops the
// transient Result channel via its json:"-" tag), so two calls carrying
// distinct Result channels for otherwise-identical payloads still dedupe.
type idempotentFakeCommandAppender struct {
	mu      sync.Mutex
	index   *journal.IdempotencyIndex
	seq     uint64
	calls   int
	records []journal.CommandRecord
}

func newIdempotentFakeCommandAppender() *idempotentFakeCommandAppender {
	return &idempotentFakeCommandAppender{index: journal.NewIdempotencyIndex()}
}

func (f *idempotentFakeCommandAppender) AppendCommand(ctx context.Context, rec journal.CommandRecord) error {
	_, err := f.AppendCommandResult(ctx, rec)
	return err
}

func (f *idempotentFakeCommandAppender) AppendCommandResult(_ context.Context, rec journal.CommandRecord) (journal.AppendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	data, err := command.MarshalCommand(rec.Command())
	if err != nil {
		return journal.AppendResult{}, err
	}
	fp := journal.NewFingerprint("command", data)
	id := rec.IdempotencyID()
	seq, duplicate, err := f.index.Check(id, fp)
	if err != nil {
		return journal.AppendResult{}, err
	}
	if duplicate {
		return journal.AppendResult{Sequence: seq, Appended: false}, nil
	}
	f.seq++
	f.index.Observe(id, f.seq, fp)
	f.records = append(f.records, rec)
	return journal.AppendResult{Sequence: f.seq, Appended: true}, nil
}

var _ commandAppenderResult = (*idempotentFakeCommandAppender)(nil)

// notifyNativeSession builds a single-loop native session (a real loopruntime
// actor) and returns it alongside its root loop id. A nil app leaves the
// session's default nop appender installed (headless mode — no idempotency
// memory of its own); a non-nil app is wired as the intent-log appender.
func notifyNativeSession(t *testing.T, app *idempotentFakeCommandAppender) (*Session, uuid.UUID) {
	t.Helper()
	opts := []Option{}
	if app != nil {
		opts = append(opts, WithCommandAppender(app))
	}
	s, err := newTestSession(context.Background(), cfg(&stubLLM{}), opts...)
	if err != nil {
		t.Fatalf("newTestSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s, s.ActiveLoopID()
}

func processCompletionNotification(id, sessionID, loopID uuid.UUID, handle string) tool.ProcessCompletionNotification {
	return tool.ProcessCompletionNotification{
		CommandID:     id,
		SessionID:     sessionID,
		LoopID:        loopID,
		ProcessHandle: handle,
		State:         tool.ProcessLifecycleExited,
		Reason:        tool.ProcessTerminalExited,
	}
}

// TestProcessNotificationLiveDuplicateIgnored proves the loop's own live
// de-dup guard — not the journal — drops an at-least-once redelivery of the
// SAME CommandID: with the default headless (nop) appender, which has no
// memory of its own, the FIRST notifyProcessCompletion call is accepted and
// the SECOND, identical call is reported as a duplicate purely because the
// loop already holds that CommandID.
func TestProcessNotificationLiveDuplicateIgnored(t *testing.T) {
	t.Parallel()
	s, loopID := notifyNativeSession(t, nil) // nop appender: no journal-side memory at all
	id := mustUUID()
	notification := processCompletionNotification(id, s.SessionID(), loopID, "proc-live-dup")

	first, err := s.notifyProcessCompletion(context.Background(), notification)
	if err != nil {
		t.Fatalf("first notifyProcessCompletion() error = %v, want nil", err)
	}
	if first != command.ProcessNotificationAccepted {
		t.Fatalf("first notifyProcessCompletion() = %v, want Accepted", first)
	}

	second, err := s.notifyProcessCompletion(context.Background(), notification)
	if err != nil {
		t.Fatalf("second notifyProcessCompletion() error = %v, want nil", err)
	}
	if second != command.ProcessNotificationDuplicate {
		t.Fatalf("second notifyProcessCompletion() = %v, want Duplicate", second)
	}
}

// TestProcessNotificationCollisionRejected proves a same-CommandID retry
// carrying a DIFFERENT persisted payload fails closed as a typed
// *journal.IdempotencyCollisionError and is never dispatched to the loop
// (the loop's live set gains no second entry).
func TestProcessNotificationCollisionRejected(t *testing.T) {
	t.Parallel()
	app := newIdempotentFakeCommandAppender()
	s, loopID := notifyNativeSession(t, app)
	id := mustUUID()
	first := processCompletionNotification(id, s.SessionID(), loopID, "proc-collide-1")

	disposition, err := s.notifyProcessCompletion(context.Background(), first)
	if err != nil || disposition != command.ProcessNotificationAccepted {
		t.Fatalf("first notifyProcessCompletion() = (%v, %v), want (Accepted, nil)", disposition, err)
	}

	second := first
	second.ProcessHandle = "proc-collide-2" // same CommandID, different persisted payload
	disposition, err = s.notifyProcessCompletion(context.Background(), second)
	var collision *journal.IdempotencyCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("second notifyProcessCompletion() error = %T %v, want *journal.IdempotencyCollisionError", err, err)
	}
	if disposition != command.ProcessNotificationCollision {
		t.Fatalf("second notifyProcessCompletion() disposition = %v, want Collision", disposition)
	}
}

// TestProcessNotificationInboxFullRetryAppendsOnce fills a loop's bounded
// live notification set to capacity, then proves that retrying dispatch for
// one further CommandID — exactly what the supervisor does after a Stopped
// disposition — appends that CommandID's durable command exactly ONCE
// despite two notifyProcessCompletion calls: the second call's append
// deduplicates (24A) rather than writing a second frame, even though the
// loop remains full and reports Stopped both times.
func TestProcessNotificationInboxFullRetryAppendsOnce(t *testing.T) {
	t.Parallel()
	app := newIdempotentFakeCommandAppender()
	s, loopID := notifyNativeSession(t, app)

	for i := 0; i < loop.ManagedInputQueueCapacity; i++ {
		n := processCompletionNotification(mustUUID(), s.SessionID(), loopID, "proc-fill")
		disposition, err := s.notifyProcessCompletion(context.Background(), n)
		if err != nil || disposition != command.ProcessNotificationAccepted {
			t.Fatalf("fill notifyProcessCompletion(%d) = (%v, %v), want (Accepted, nil)", i, disposition, err)
		}
	}

	overflow := processCompletionNotification(mustUUID(), s.SessionID(), loopID, "proc-overflow")

	first, err := s.notifyProcessCompletion(context.Background(), overflow)
	if first != command.ProcessNotificationStopped {
		t.Fatalf("first overflow notifyProcessCompletion() = (%v, %v), want (Stopped, non-nil)", first, err)
	}
	if err == nil {
		t.Fatal("first overflow notifyProcessCompletion() error = nil, want non-nil")
	}

	before := app.calls
	second, err := s.notifyProcessCompletion(context.Background(), overflow)
	if second != command.ProcessNotificationStopped {
		t.Fatalf("retried overflow notifyProcessCompletion() = (%v, %v), want (Stopped, non-nil)", second, err)
	}
	if err == nil {
		t.Fatal("retried overflow notifyProcessCompletion() error = nil, want non-nil")
	}
	if app.calls != before+1 {
		t.Errorf("appender calls after retry = %d, want %d (one more append call, deduplicated to no new frame)", app.calls, before+1)
	}

	app.mu.Lock()
	distinct := len(app.records)
	app.mu.Unlock()
	if distinct != loop.ManagedInputQueueCapacity+1 {
		t.Errorf("durable records = %d, want %d (fill + exactly one NEW frame for the overflow CommandID)", distinct, loop.ManagedInputQueueCapacity+1)
	}
}

// TestForeignLoopRejectsProcessNotification proves a process completion
// notification addressed to a foreign-engine loop is refused structurally,
// on the ENGINE (mirroring loopHandle.ReplaceExternalTools' identical
// guard — see TestReplaceExternalToolsRefusedOnForeignLoop), rather than
// silently dropped or hung waiting on a backend arm the foreign loop does
// not have.
func TestForeignLoopRejectsProcessNotification(t *testing.T) {
	t.Parallel()
	builder := &fakeForeignBuilder{sid: fixedForeignSID, backend: newFakeBackend()}
	definition := engineCfg(&stubLLM{}, loop.EngineForeignClaude, "sys")
	s, err := newTestSession(context.Background(), definition, WithForeignBuilders(builder.build, builder.buildRestored))
	if err != nil {
		t.Fatalf("newTestSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	loopID := s.ActiveLoopID()
	h := s.loops[loopID]
	if h.bound.Engine() == loop.EngineNative {
		t.Fatal("test setup: want a foreign-engine loop")
	}

	notification := processCompletionNotification(mustUUID(), s.SessionID(), loopID, "proc-foreign")
	disposition, err := s.notifyProcessCompletion(context.Background(), notification)
	var unsupported *ProcessNotificationUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("notifyProcessCompletion() error = %T %v, want *ProcessNotificationUnsupportedError", err, err)
	}
	if unsupported.LoopID != loopID {
		t.Errorf("ProcessNotificationUnsupportedError.LoopID = %v, want %v", unsupported.LoopID, loopID)
	}
	if disposition != command.ProcessNotificationStopped {
		t.Errorf("notifyProcessCompletion() disposition = %v, want Stopped", disposition)
	}
}

// TestProcessCompletionBridgeAttachDelegatesToCheckedNotifier proves attaching
// the checked implementation (*Session.NotifyProcessCompletion) behind
// sessionProcessServiceBridge (Task 4's stable late-bound bridge, the SAME
// object 24B's attachProcessLifecyclePublisher already extends) makes the
// bridge's NotifyProcessCompletion delegate to it WITHOUT replacing the
// bridge itself — the same bridge value keeps satisfying
// tool.ProcessCompletionNotifier before and after attach, mirroring
// TestProcessLifecycleBridgeAttachDelegatesToCheckedPublisher exactly.
func TestProcessCompletionBridgeAttachDelegatesToCheckedNotifier(t *testing.T) {
	t.Parallel()
	bridge, services, err := newSessionProcessServices()
	if err != nil {
		t.Fatalf("newSessionProcessServices() error = %v", err)
	}

	s, loopID := notifyNativeSession(t, nil)
	notification := processCompletionNotification(mustUUID(), s.SessionID(), loopID, "proc-bridge")

	// Before attach: the bridge still reports the explicit unavailable error.
	if err := bridge.NotifyProcessCompletion(context.Background(), notification); !errors.Is(err, errSessionProcessServicesUnavailable) {
		t.Fatalf("pre-attach NotifyProcessCompletion() error = %v, want errSessionProcessServicesUnavailable", err)
	}

	bridge.attachProcessCompletionNotifier(s)
	if services.ProcessCompletionNotifier() != bridge {
		t.Fatal("SessionResourceServices completion notifier is no longer the stable bridge after attach")
	}

	if err := bridge.NotifyProcessCompletion(context.Background(), notification); err != nil {
		t.Fatalf("post-attach NotifyProcessCompletion() error = %v, want nil", err)
	}

	// A nil attach never erases an already-attached delegate.
	bridge.attachProcessCompletionNotifier(nil)
	second := processCompletionNotification(mustUUID(), s.SessionID(), loopID, "proc-bridge-2")
	if err := bridge.NotifyProcessCompletion(context.Background(), second); err != nil {
		t.Fatalf("NotifyProcessCompletion() after a nil attach error = %v, want nil (delegate must stay attached)", err)
	}
}

// TestProcessNotificationAppendBeforeDispatchRestoresDelivery proves
// undeliveredProcessNotifications recovers a durably-appended
// ProcessNotification command whose loop never reached a causality commit
// (a crash between the supervisor's dispatch and the loop's causing durable
// event): with no durable event carrying Cause.CommandID == the
// notification's CommandID, it is returned as pending — exactly the
// "restores delivery" outcome restore's fold feeds into RestoredState.
func TestProcessNotificationAppendBeforeDispatchRestoresDelivery(t *testing.T) {
	t.Parallel()
	loopID := mustUUID()
	sessionID := mustUUID()
	id := mustUUID()
	notification := processCompletionNotification(id, sessionID, loopID, "proc-undelivered")
	cmd := command.ProcessNotification{Header: command.Header{CommandID: id}, Notification: notification}
	// A CommandRecord reconstructed from replay carries a ZERO dispatch loopID
	// (the transient route is never persisted — see journal/record_replay.go);
	// undeliveredProcessNotifications must key off Notification.LoopID, not
	// CommandRecord.LoopID(), to find it at all.
	records := []journal.JournalRecord{journal.NewCommandRecord(sessionID, uuid.UUID{}, cmd)}

	pending := undeliveredProcessNotifications(records, loopID, causedCommandIDs(nil))
	if len(pending) != 1 || pending[0].CommandID != id {
		t.Fatalf("undeliveredProcessNotifications() = %+v, want [%+v]", pending, notification)
	}
}

// TestProcessNotificationConsumedCommandNotRedelivered proves the mirror
// image: when a durable loop event's Cause.CommandID already names the
// notification's CommandID (the causality commit landed before the crash),
// undeliveredProcessNotifications excludes it — restore never redelivers an
// already-consumed notification.
func TestProcessNotificationConsumedCommandNotRedelivered(t *testing.T) {
	t.Parallel()
	loopID := mustUUID()
	sessionID := mustUUID()
	id := mustUUID()
	notification := processCompletionNotification(id, sessionID, loopID, "proc-consumed")
	cmd := command.ProcessNotification{Header: command.Header{CommandID: id}, Notification: notification}
	records := []journal.JournalRecord{journal.NewCommandRecord(sessionID, uuid.UUID{}, cmd)}

	consumedBy := event.LoopIdle{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID},
		Cause:       identity.Cause{CommandID: id},
	}}
	pending := undeliveredProcessNotifications(records, loopID, causedCommandIDs([]event.Event{consumedBy}))
	if len(pending) != 0 {
		t.Fatalf("undeliveredProcessNotifications() = %+v, want none (already consumed)", pending)
	}
}
