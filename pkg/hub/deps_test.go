package hub

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/core/uuid"
)

// fakeAppender is a controllable eventAppender for hub unit tests. It records every
// appended event in order and can be set to fail (returning errAppend) so a test can
// drive the fail-secure branch. It is safe for concurrent use.
type fakeAppender struct {
	mu       sync.Mutex
	appended []event.Event
	failAt   int  // 1-based index of the call that should fail; 0 = never fail
	failAll  bool // every call fails
	calls    int
}

func (f *fakeAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failAll || (f.failAt != 0 && f.calls == f.failAt) {
		return 0, errAppend
	}
	f.appended = append(f.appended, ev)
	return uint64(len(f.appended)), nil
}

func (f *fakeAppender) events() []event.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]event.Event, len(f.appended))
	copy(out, f.appended)
	return out
}

func (f *fakeAppender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// typeFailAppender records every appended event in order and fails the append of any
// event whose concrete type matches a registered fail set. It is the ordering-robust
// counterpart to fakeAppender.failAt: a test that needs "succeed on every append
// EXCEPT the derived SessionIdle" cannot use a brittle 1-based call index (the index
// shifts if the publish path appends a different number of events), so it names the
// type to fail instead. Safe for concurrent use.
type typeFailAppender struct {
	mu       sync.Mutex
	appended []event.Event
	failOn   map[string]struct{} // fmt.Sprintf("%T", ev) values to fail
	calls    int
}

// failOnType returns a typeFailAppender that fails AppendEvent for any event whose
// %T matches one of evs, and succeeds (recording the event) for every other type.
func failOnType(evs ...event.Event) *typeFailAppender {
	set := make(map[string]struct{}, len(evs))
	for _, ev := range evs {
		set[fmt.Sprintf("%T", ev)] = struct{}{}
	}
	return &typeFailAppender{failOn: set}
}

func (f *typeFailAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if _, fail := f.failOn[fmt.Sprintf("%T", ev)]; fail {
		return 0, errAppend
	}
	f.appended = append(f.appended, ev)
	return uint64(len(f.appended)), nil
}

func (f *typeFailAppender) events() []event.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]event.Event, len(f.appended))
	copy(out, f.appended)
	return out
}

// recordingReporter captures the faults the hub reports so a test can assert the
// fail-secure escalation fired with the right concrete error. Safe for concurrent use.
type recordingReporter struct {
	mu     sync.Mutex
	faults []*SessionPersistenceFault
}

func (r *recordingReporter) ReportFault(_ context.Context, f *SessionPersistenceFault) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.faults = append(r.faults, f)
}

func (r *recordingReporter) reported() []*SessionPersistenceFault {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*SessionPersistenceFault, len(r.faults))
	copy(out, r.faults)
	return out
}

// testFactory mints deterministic, monotonically increasing EventIDs and a fixed
// CreatedAt so a test can assert a synthesized session event was stamped.
func testFactory() *event.Factory {
	var n byte
	ts := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	return event.NewFactory(func() (uuid.UUID, error) {
		n++
		return uuid.UUID{n}, nil
	}, func() time.Time { return ts })
}

// TestNopAppenderReturnsZeroSeq proves the default (no-persistence) appender commits
// nothing, never fails, and reports sequence 0 — so a headless hub delivers every
// Enduring event with JournalSeq 0 (nothing was durably sequenced).
func TestNopAppenderReturnsZeroSeq(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ev   event.Event
	}{
		{name: "enduring session event", ev: event.SessionStarted{}},
		{name: "enduring step event", ev: event.StepDone{}},
		{name: "zero-value event", ev: event.SessionStopped{}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			seq, err := nopEventAppender{}.AppendEvent(context.Background(), tt.ev)
			if err != nil {
				t.Fatalf("AppendEvent() error = %v, want nil", err)
			}
			if seq != 0 {
				t.Errorf("AppendEvent() seq = %d, want 0", seq)
			}
		})
	}
}

// TestNewDefaultsAreNop proves the bare New(sessionID) constructor still works and
// installs the nop appender + nop reporter + a usable factory, so existing callers
// and headless mode are unchanged: an Enduring publish neither errors nor invokes a
// reporter (the nop appender never fails).
func TestNewDefaultsAreNop(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	h := New(session)

	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents = %v", err)
	}

	// An Enduring publish under the default deps: delivered, no error, no fault.
	if err := h.PublishEvent(context.Background(), event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}); err != nil {
		t.Fatalf("PublishEvent = %v", err)
	}
	if _, ok := recv(t, sub).(event.StepDone); !ok {
		t.Fatalf("default-deps hub did not deliver StepDone")
	}
}

// TestNewWithDepsStores proves the option constructor stores the injected appender,
// factory, and reporter (and that omitting any falls back to the nop default).
func TestNewWithDepsStores(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	app := &fakeAppender{}
	rep := &recordingReporter{}
	fac := testFactory()

	h := New(session, WithAppender(app), WithFactory(fac), WithFaultReporter(rep))

	if h.appender != app {
		t.Errorf("appender not stored")
	}
	if h.factory != fac {
		t.Errorf("factory not stored")
	}
	if h.reporter != rep {
		t.Errorf("reporter not stored")
	}

	// Omitting options leaves the nop defaults non-nil.
	h2 := New(session)
	if h2.appender == nil || h2.factory == nil || h2.reporter == nil {
		t.Fatalf("default-deps hub left a nil dep: appender=%v factory=%v reporter=%v", h2.appender, h2.factory, h2.reporter)
	}
}
