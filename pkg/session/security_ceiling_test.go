package session

import (
	"context"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
)

// collectSecurityCeilingChanged drains sub for up to d, returning every
// SecurityCeilingChanged it saw (ignoring the other session/loop events that flow). It
// lets a test assert BOTH the level AND that EXACTLY ONE change event was emitted.
func collectSecurityCeilingChanged(t *testing.T, sub *hub.EventSubscription, d time.Duration) []event.SecurityCeilingChanged {
	t.Helper()
	var out []event.SecurityCeilingChanged
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return out
			}
			if sc, is := ev.(event.SecurityCeilingChanged); is {
				out = append(out, sc)
			}
		case <-deadline:
			return out
		}
	}
}

// TestSetSecurityCeilingAppliesAndEmits proves applying SetSecurityCeiling updates the
// session's live ceiling ordinal (Current()) and emits EXACTLY ONE SecurityCeilingChanged
// carrying the applied level, session-scoped.
func TestSetSecurityCeilingAppliesAndEmits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := New(ctx, cfg(&stubLLM{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(event.EventFilter{})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer func() { _ = sub.Close() }()

	if err := s.SetSecurityCeiling(ctx, 2); err != nil {
		t.Fatalf("SetSecurityCeiling: %v", err)
	}
	if got := s.CeilingSource().Current(); got != 2 {
		t.Fatalf("Current() = %d, want 2", got)
	}

	got := collectSecurityCeilingChanged(t, sub, 500*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("emitted %d SecurityCeilingChanged, want exactly 1", len(got))
	}
	if got[0].Level != 2 {
		t.Errorf("SecurityCeilingChanged.Level = %d, want 2", got[0].Level)
	}
	if got[0].SessionID != s.SessionID {
		t.Errorf("event SessionID = %v, want %v", got[0].SessionID, s.SessionID)
	}
}

// TestSetSecurityCeilingLastWriteWins proves the live apply sequence {1},{2},{0} leaves
// the ceiling at 0 — last write wins, the same determinism replay relies on.
func TestSetSecurityCeilingLastWriteWins(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := New(ctx, cfg(&stubLLM{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	for _, lv := range []uint8{1, 2, 0} {
		if err := s.SetSecurityCeiling(ctx, lv); err != nil {
			t.Fatalf("SetSecurityCeiling(%d): %v", lv, err)
		}
	}
	if got := s.CeilingSource().Current(); got != 0 {
		t.Fatalf("Current() after {1,2,0} = %d, want 0 (last write wins)", got)
	}
}

// TestWithCeilingSharesSource proves the composition root's injected source (the SAME one
// it wires into the checker via tools.WithCeilingPostures) sees the change, so posture
// selection and this session's ceiling never disagree.
func TestWithCeilingSharesSource(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	shared := ceiling.New()
	s, err := New(ctx, cfg(&stubLLM{}), WithCeiling(shared))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if err := s.SetSecurityCeiling(ctx, 3); err != nil {
		t.Fatalf("SetSecurityCeiling: %v", err)
	}
	if got := shared.Current(); got != 3 {
		t.Fatalf("shared source Current() = %d, want 3 (session and checker share one state)", got)
	}
	if got := s.CeilingSource().Current(); got != 3 {
		t.Fatalf("CeilingSource().Current() = %d, want 3", got)
	}
}

// TestLastSecurityCeiling is the restore discovery-scanner unit: fold the durable
// SecurityCeilingChanged events, last write wins, and report absence so a never-changed
// session resumes at the fail-secure default.
func TestLastSecurityCeiling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		events []event.Event
		want   uint8
		wantOK bool
	}{
		{"none", []event.Event{event.SessionStarted{}, event.LoopStarted{}}, 0, false},
		{"single", []event.Event{event.SecurityCeilingChanged{Level: 2}}, 2, true},
		{"last wins", []event.Event{
			event.SecurityCeilingChanged{Level: 1},
			event.SecurityCeilingChanged{Level: 2},
			event.SecurityCeilingChanged{Level: 0},
		}, 0, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := lastSecurityCeiling(tt.events)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("lastSecurityCeiling() = (%d,%v), want (%d,%v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestSecurityCeilingRestoreRoundTrip is the gold-standard replay proof: a session whose
// durable stream carries two SecurityCeilingChanged events restores with the LAST one's
// ordinal live on its ceiling source — deterministic replay, last write wins.
func TestSecurityCeilingRestoreRoundTrip(t *testing.T) {
	store := newRestoreStore(t)
	fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

	h, sessionID, _, lease, es := newOriginalHub(t, store, fp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The operator changed the ceiling twice during the original run; last write wins.
	es.stamp(t, ctx, h, event.SecurityCeilingChanged{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}, Level: 1,
	})
	es.stamp(t, ctx, h, event.SecurityCeilingChanged{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}, Level: 2,
	})
	handOver(t, lease)

	s, err := Restore(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"), sessionID, store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if got := s.CeilingSource().Current(); got != 2 {
		t.Fatalf("restored Current() = %d, want 2 (last SecurityCeilingChanged wins)", got)
	}
}
