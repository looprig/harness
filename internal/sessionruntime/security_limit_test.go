package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/security"
)

// ceilingEmit records, for one SecurityLimitChanged durably appended, the Level it
// carried and the value of the shared security limit source AT THE INSTANT of the append — the
// probe that pins the apply/emit ORDER (the hub's durable tap runs synchronously before
// fan-out, so this read happens exactly when the emit commits).
type ceilingEmit struct {
	level         security.Level
	currentAtEmit security.Level
}

// ceilingTapSpy is a fake hub durable tap (eventAppender) that both PROBES the emit order
// (recording ceilingEmit for every SecurityLimitChanged) and can INJECT a durable-append
// fault selectively on those events — the seam that drives the fault-ordering cases.
// Non-security limit events (SessionStarted/LoopStarted/LoopIdle/...) always succeed, so the
// session comes up and the loop runs; only the security limit emit is faulted when failSC is set.
type ceilingTapSpy struct {
	state *security.Limit
	mu    sync.Mutex
	seen  []ceilingEmit
	fail  bool
}

func (r *ceilingTapSpy) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	sc, ok := ev.(event.SecurityLimitChanged)
	if !ok {
		return 0, nil
	}
	r.mu.Lock()
	r.seen = append(r.seen, ceilingEmit{level: sc.Level, currentAtEmit: r.state.Current()})
	fail := r.fail
	n := len(r.seen)
	r.mu.Unlock()
	if fail {
		return 0, errors.New("injected durable append failure (security securityLimit)")
	}
	return uint64(n), nil
}

func (r *ceilingTapSpy) setFail(v bool) {
	r.mu.Lock()
	r.fail = v
	r.mu.Unlock()
}

func (r *ceilingTapSpy) emits() []ceilingEmit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ceilingEmit(nil), r.seen...)
}

// collectSecurityLimitChanged drains sub for up to d, returning every
// SecurityLimitChanged it saw (ignoring the other session/loop events that flow). It
// lets a test assert BOTH the level AND that EXACTLY ONE change event was emitted.
func collectSecurityLimitChanged(t *testing.T, sub event.Subscription, d time.Duration) []event.SecurityLimitChanged {
	t.Helper()
	var out []event.SecurityLimitChanged
	deadline := time.After(d)
	for {
		select {
		case d, ok := <-sub.Events():
			if !ok {
				return out
			}
			if sc, is := d.Event.(event.SecurityLimitChanged); is {
				out = append(out, sc)
			}
		case <-deadline:
			return out
		}
	}
}

// TestSetSecurityLimitAppliesAndEmits proves applying SetSecurityLimit updates the
// session's live security limit ordinal (Current()) and emits EXACTLY ONE SecurityLimitChanged
// carrying the applied level, session-scoped.
func TestSetSecurityLimitAppliesAndEmits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := newTestSession(ctx, cfg(&stubLLM{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(event.EventFilter{})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer func() { _ = sub.Close() }()

	if err := s.SetSecurityLimit(ctx, 2); err != nil {
		t.Fatalf("SetSecurityLimit: %v", err)
	}
	if got := s.SecurityLimitSource().Current(); got != 2 {
		t.Fatalf("Current() = %d, want 2", got)
	}

	got := collectSecurityLimitChanged(t, sub, 500*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("emitted %d SecurityLimitChanged, want exactly 1", len(got))
	}
	if got[0].Level != 2 {
		t.Errorf("SecurityLimitChanged.Level = %d, want 2", got[0].Level)
	}
	if got[0].SessionID != s.SessionID() {
		t.Errorf("event SessionID = %v, want %v", got[0].SessionID, s.SessionID())
	}
}

// TestSetSecurityLimitLastWriteWins proves the live apply sequence {1},{2},{0} leaves
// the security limit at 0 — last write wins, the same determinism replay relies on.
func TestSetSecurityLimitLastWriteWins(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := newTestSession(ctx, cfg(&stubLLM{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	for _, lv := range []security.Level{1, 2, 0} {
		if err := s.SetSecurityLimit(ctx, lv); err != nil {
			t.Fatalf("SetSecurityLimit(%d): %v", lv, err)
		}
	}
	if got := s.SecurityLimitSource().Current(); got != 0 {
		t.Fatalf("Current() after {1,2,0} = %d, want 0 (last write wins)", got)
	}
}

// TestWithSecurityLimitSharesSource proves the composition root's injected source (the SAME one
// it wires into the checker via tools.WithSecurityLimitPostures) sees the change, so posture
// selection and this session's security limit never disagree.
func TestWithSecurityLimitSharesSource(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	shared := security.New()
	s, err := newTestSession(ctx, cfg(&stubLLM{}), WithSecurityLimit(shared))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if err := s.SetSecurityLimit(ctx, 3); err != nil {
		t.Fatalf("SetSecurityLimit: %v", err)
	}
	if got := shared.Current(); got != 3 {
		t.Fatalf("shared source Current() = %d, want 3 (session and checker share one state)", got)
	}
	if got := s.SecurityLimitSource().Current(); got != 3 {
		t.Fatalf("SecurityLimitSource().Current() = %d, want 3", got)
	}
}

// TestSetSecurityLimitDirectionOrdering pins the direction-dependent apply/emit order
// via the durable tap: a RAISE emits BEFORE it mutates Current() (so the emit sees the OLD
// ordinal), while a LOWER mutates Current() BEFORE it emits (so the emit sees the NEW,
// lower ordinal). This is the ordering guarantee that forbids an un-persisted loosening.
func TestSetSecurityLimitDirectionOrdering(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	shared := security.New()
	spy := &ceilingTapSpy{state: shared}
	s, err := newTestSession(ctx, cfg(&stubLLM{}), WithSecurityLimit(shared), WithEventAppender(spy))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Raise 0 -> 2: emit-before-apply, so the emit observes the OLD ordinal (0).
	if err := s.SetSecurityLimit(ctx, 2); err != nil {
		t.Fatalf("SetSecurityLimit(2): %v", err)
	}
	if got := shared.Current(); got != 2 {
		t.Fatalf("Current() after raise = %d, want 2", got)
	}
	// Lower 2 -> 1: apply-before-emit, so the emit observes the NEW ordinal (1).
	if err := s.SetSecurityLimit(ctx, 1); err != nil {
		t.Fatalf("SetSecurityLimit(1): %v", err)
	}
	if got := shared.Current(); got != 1 {
		t.Fatalf("Current() after lower = %d, want 1", got)
	}

	emits := spy.emits()
	if len(emits) != 2 {
		t.Fatalf("recorded %d securityLimit emits, want 2: %+v", len(emits), emits)
	}
	if emits[0].level != 2 || emits[0].currentAtEmit != 0 {
		t.Errorf("raise emit = %+v, want {level:2, currentAtEmit:0} (emit BEFORE the raise is applied)", emits[0])
	}
	if emits[1].level != 1 || emits[1].currentAtEmit != 1 {
		t.Errorf("lower emit = %+v, want {level:1, currentAtEmit:1} (apply BEFORE the emit)", emits[1])
	}
}

// TestSetSecurityLimitLooseningFaultKeepsOld proves a LOOSENING whose durable emit faults
// leaves Current() at the OLD (lower) ordinal — never raised. The permissiveness increase
// is not live in-memory because it has no durable record, closing the audit/permissiveness
// gap even for an in-flight turn's checker sharing this source.
func TestSetSecurityLimitLooseningFaultKeepsOld(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	shared := security.New() // starts at 0
	spy := &ceilingTapSpy{state: shared, fail: true}
	s, err := newTestSession(ctx, cfg(&stubLLM{}), WithSecurityLimit(shared), WithEventAppender(spy))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Raise 0 -> 2 whose emit faults: PublishEvent returns nil (hub contract), and the raise
	// is NOT applied. Current() stays 0.
	if err := s.SetSecurityLimit(ctx, 2); err != nil {
		t.Fatalf("SetSecurityLimit(2) returned %v, want nil (fault surfaced out-of-band)", err)
	}
	if got := shared.Current(); got != 0 {
		t.Fatalf("loosening whose emit faulted left Current() = %d, want 0 (not raised ahead of the journal)", got)
	}
	if s.faultIfFaulted() == nil {
		t.Errorf("session not faulted after an injected securityLimit-append failure")
	}
	// The emit DID happen (the durable record was attempted) before the raise was declined.
	if emits := spy.emits(); len(emits) != 1 || emits[0].level != 2 || emits[0].currentAtEmit != 0 {
		t.Errorf("emits = %+v, want one {level:2, currentAtEmit:0} (emit-before-apply on a raise)", emits)
	}
}

// TestSetSecurityLimitTighteningFaultStillApplies proves a TIGHTENING whose durable emit
// faults STILL lowers Current() immediately (§8): more-restrictive-in-memory is the safe
// direction, so a tightening applies ahead of its durable record.
func TestSetSecurityLimitTighteningFaultStillApplies(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	shared := security.New()
	spy := &ceilingTapSpy{state: shared}
	s, err := newTestSession(ctx, cfg(&stubLLM{}), WithSecurityLimit(shared), WithEventAppender(spy))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Set up a non-zero security limit first (a raise that succeeds).
	if err := s.SetSecurityLimit(ctx, 2); err != nil {
		t.Fatalf("SetSecurityLimit(2): %v", err)
	}
	if got := shared.Current(); got != 2 {
		t.Fatalf("setup Current() = %d, want 2", got)
	}

	// Now fault the next security limit emit and TIGHTEN 2 -> 1: apply-before-emit means Current()
	// is already 1 when the emit faults.
	spy.setFail(true)
	if err := s.SetSecurityLimit(ctx, 1); err != nil {
		t.Fatalf("SetSecurityLimit(1) returned %v, want nil", err)
	}
	if got := shared.Current(); got != 1 {
		t.Fatalf("tightening whose emit faulted left Current() = %d, want 1 (still lowered immediately)", got)
	}
	if s.faultIfFaulted() == nil {
		t.Errorf("session not faulted after an injected securityLimit-append failure")
	}
}

// TestLastSecurityLimit is the restore discovery-scanner unit: fold the durable
// SecurityLimitChanged events, last write wins, and report absence so a never-changed
// session resumes at the fail-secure default.
func TestLastSecurityLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		events []event.Event
		want   security.Level
		wantOK bool
	}{
		{"none", []event.Event{event.SessionStarted{}, event.LoopStarted{}}, 0, false},
		{"single", []event.Event{event.SecurityLimitChanged{Level: 2}}, 2, true},
		{"last wins", []event.Event{
			event.SecurityLimitChanged{Level: 1},
			event.SecurityLimitChanged{Level: 2},
			event.SecurityLimitChanged{Level: 0},
		}, 0, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := lastSecurityLimit(tt.events)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("lastSecurityLimit() = (%d,%v), want (%d,%v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestSecurityLimitRestoreRoundTrip is the gold-standard replay proof: a session whose
// durable stream carries two SecurityLimitChanged events restores with the LAST one's
// ordinal live on its security limit source — deterministic replay, last write wins.
func TestSecurityLimitRestoreRoundTrip(t *testing.T) {
	store := newRestoreStore(t)
	fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

	h, sessionID, _, lease, es := newOriginalHub(t, store, fp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The operator changed the security limit twice during the original run; last write wins.
	es.stamp(t, ctx, h, event.SecurityLimitChanged{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}, Level: 1,
	})
	es.stamp(t, ctx, h, event.SecurityLimitChanged{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}, Level: 2,
	})
	handOver(t, lease)

	s, err := restoreTestSession(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"), sessionID, store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if got := s.SecurityLimitSource().Current(); got != 2 {
		t.Fatalf("restored Current() = %d, want 2 (last SecurityLimitChanged wins)", got)
	}
}
