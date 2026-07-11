package sessionruntime

import (
	"context"
	"sync"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// recordingEventAppender is an eventAppender double for the hub's REQUIRED durable
// tap: it records every Enduring event the hub appends before fan-out. It is the
// composition-seam counterpart to fakeCommandAppender (the audit-only intent log).
type recordingEventAppender struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recordingEventAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return uint64(len(r.events)), nil
}

func (r *recordingEventAppender) snapshot() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]event.Event, len(r.events))
	copy(out, r.events)
	return out
}

// TestWithSessionID proves the composition-root seam that resolves the journal
// chicken-and-egg: New mints its own sessionID by default, but an injected
// WithSessionID(id) makes New adopt the externally-minted id verbatim (so the
// composition root can build the journal from the same id BEFORE New). A zero id is
// ignored (New mints one) so a wiring slip can never produce a zero-id session.
func TestWithSessionID(t *testing.T) {
	t.Parallel()

	injected := mustUUID()
	tests := []struct {
		name      string
		opt       Option
		wantEqual bool // want SessionID == injected
	}{
		{name: "injected id adopted", opt: WithSessionID(injected), wantEqual: true},
		{name: "zero id ignored (New mints)", opt: WithSessionID(uuid.UUID{}), wantEqual: false},
		{name: "no option (New mints)", opt: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var opts []Option
			if tt.opt != nil {
				opts = append(opts, tt.opt)
			}
			s, err := New(ctx, cfg(&stubLLM{}), opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			if tt.wantEqual && s.SessionID() != injected {
				t.Errorf("SessionID = %v, want injected %v", s.SessionID(), injected)
			}
			if !tt.wantEqual && s.SessionID() == injected {
				t.Errorf("SessionID = injected %v, want a freshly-minted id", injected)
			}
			if s.SessionID().IsZero() {
				t.Error("SessionID is zero — New must never produce a zero-id session")
			}
		})
	}
}

// TestWithLeaseRelease proves the lease-release-on-teardown seam: a session built with a
// release hook calls it EXACTLY ONCE at the end of Shutdown (so a clean exit relinquishes
// single-writer ownership and a successor can re-acquire without waiting out the TTL). A
// second Shutdown does not call it again (idempotent). A session built WITHOUT the option
// never references a releaser (the default is nil — a no-op), so headless mode is
// unchanged.
func TestWithLeaseRelease(t *testing.T) {
	t.Parallel()

	var calls int
	var mu sync.Mutex
	release := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := New(ctx, cfg(&stubLLM{}), WithLeaseRelease(release))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// A second Shutdown must not double-release (StopSession is idempotent; the releaser
	// must be too).
	_ = s.Shutdown(context.Background())

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("lease release called %d times, want exactly 1", got)
	}
}

// TestWithEventAppender proves the hub's required durable tap is injectable at the
// session boundary: a new session built WithEventAppender(rec) appends its Enduring
// session-scoped events (SessionStarted, LoopStarted) through rec BEFORE fan-out. The
// default (no option) installs the hub's nop appender, so a bare New persists nothing —
// the headless behavior is unchanged.
func TestWithEventAppender(t *testing.T) {
	t.Parallel()

	rec := &recordingEventAppender{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := New(ctx, cfg(&stubLLM{}), WithEventAppender(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	got := rec.snapshot()
	// New publishes SessionStarted then the root loop's LoopStarted; both are Enduring,
	// so both must have been appended through the injected tap.
	if len(got) < 2 {
		t.Fatalf("appended %d events, want >= 2 (SessionStarted + LoopStarted)", len(got))
	}
	if _, ok := got[0].(event.SessionStarted); !ok {
		t.Errorf("first appended event = %T, want event.SessionStarted", got[0])
	}
	var sawLoopStarted bool
	for _, ev := range got {
		if _, ok := ev.(event.LoopStarted); ok {
			sawLoopStarted = true
		}
		if ev.Class() != event.Enduring {
			t.Errorf("appended a non-Enduring event %T (the tap is Enduring-only)", ev)
		}
	}
	if !sawLoopStarted {
		t.Error("no LoopStarted appended — the root loop's start was not persisted")
	}
}
