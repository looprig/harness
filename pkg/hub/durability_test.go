package hub

import (
	"context"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// resultAppender is a controllable eventAppenderResult double for hub unit tests. Each
// call consults decisions (in order; the last entry repeats once exhausted) to decide
// whether that append reports Appended=true or Appended=false, and always assigns a
// fresh, strictly increasing sequence regardless of the decision (mirroring a real
// idempotent journal, which still reports the ORIGINAL sequence on a dedup — tests that
// care about the reported sequence set it explicitly via seqOverride). It is safe for
// concurrent use.
type resultAppender struct {
	mu          sync.Mutex
	decisions   []bool
	seqOverride map[int]uint64 // 1-based call index -> sequence to report
	appended    []event.Event  // every event this appender was asked to append, in order
	calls       int
	err         error
}

func (a *resultAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	seq, _, err := a.AppendEventResult(ctx, ev)
	return seq, err
}

func (a *resultAppender) AppendEventResult(_ context.Context, ev event.Event) (uint64, bool, error) {
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
	seq := uint64(a.calls)
	if s, ok := a.seqOverride[a.calls]; ok {
		seq = s
	}
	return seq, appended, nil
}

func (a *resultAppender) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// TestNopAppenderResultAlwaysAppended proves nopEventAppender's optional extension
// always reports Appended=true, so a bare New(sessionID) hub keeps its pre-extension
// behavior: every Enduring publish applies and delivers, never gated.
func TestNopAppenderResultAlwaysAppended(t *testing.T) {
	t.Parallel()
	seq, appended, err := nopEventAppender{}.AppendEventResult(context.Background(), event.StepDone{})
	if err != nil {
		t.Fatalf("AppendEventResult() error = %v, want nil", err)
	}
	if !appended {
		t.Error("AppendEventResult() appended = false, want true")
	}
	if seq != 0 {
		t.Errorf("AppendEventResult() seq = %d, want 0", seq)
	}
}

// TestPublishGatesBroadcastOnDeduplicatedAppend proves a Hub whose injected appender
// implements the optional eventAppenderResult extension applies and delivers a
// genuinely new append (Appended=true) but neither applies nor delivers a deduplicated
// retry (Appended=false) — and reports the SAME nil success both times, since a
// deduplicated retry already durably landed and already delivered via its original
// call.
func TestPublishGatesBroadcastOnDeduplicatedAppend(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	app := &resultAppender{decisions: []bool{true, false}}
	h := New(session, WithAppender(app))

	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}

	ev := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}

	// First publish: a genuinely new append. Applied and delivered.
	if err := h.PublishEventChecked(context.Background(), ev); err != nil {
		t.Fatalf("first PublishEventChecked() error = %v, want nil", err)
	}
	delivery := recvDelivery(t, sub)
	if _, ok := delivery.Event.(event.StepDone); !ok {
		t.Fatalf("first delivery = %T, want event.StepDone", delivery.Event)
	}
	if delivery.JournalSeq != 1 {
		t.Errorf("first delivery.JournalSeq = %d, want 1", delivery.JournalSeq)
	}

	// Second publish of the SAME event: the appender reports a deduplicated retry.
	// PublishEventChecked still succeeds (the record IS durable — just not from this
	// call), but nothing new is applied or broadcast.
	if err := h.PublishEventChecked(context.Background(), ev); err != nil {
		t.Fatalf("second (deduplicated) PublishEventChecked() error = %v, want nil", err)
	}
	expectNone(t, sub)

	if got := app.callCount(); got != 2 {
		t.Fatalf("appender calls = %d, want 2 (both the original and the deduplicated retry reach the appender)", got)
	}
}

// TestPublishAppenderWithoutResultExtensionAlwaysApplies proves an appender that
// implements only the OLD single-method eventAppender surface (no
// AppendEventResult) is treated exactly as before this extension existed: every
// successful append is applied and delivered, never gated.
func TestPublishAppenderWithoutResultExtensionAlwaysApplies(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	app := &fakeAppender{}
	h := New(session, WithAppender(app))

	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}

	ev := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}
	for i := 0; i < 2; i++ {
		if err := h.PublishEventChecked(context.Background(), ev); err != nil {
			t.Fatalf("PublishEventChecked() [%d] error = %v, want nil", i, err)
		}
		if _, ok := recv(t, sub).(event.StepDone); !ok {
			t.Fatalf("publish [%d] was not delivered", i)
		}
	}
	if got := app.callCount(); got != 2 {
		t.Errorf("appender calls = %d, want 2 (a plain eventAppender is never gated)", got)
	}
}

// TestPublishDeduplicatedAppendUncheckedNeverFaults proves the unchecked PublishEvent
// variant also treats a deduplicated retry as a silent, faultless no-op: no state
// mutation, no broadcast, no reported fault.
func TestPublishDeduplicatedAppendUncheckedNeverFaults(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	rep := &recordingReporter{}
	app := &resultAppender{decisions: []bool{true, false}}
	h := New(session, WithAppender(app), WithFaultReporter(rep))

	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}

	ev := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}
	if err := h.PublishEvent(context.Background(), ev); err != nil {
		t.Fatalf("first PublishEvent() error = %v, want nil", err)
	}
	recv(t, sub)

	if err := h.PublishEvent(context.Background(), ev); err != nil {
		t.Fatalf("second (deduplicated) PublishEvent() error = %v, want nil", err)
	}
	expectNone(t, sub)

	if faults := rep.reported(); len(faults) != 0 {
		t.Errorf("reported %d faults for a deduplicated retry, want 0", len(faults))
	}
}
