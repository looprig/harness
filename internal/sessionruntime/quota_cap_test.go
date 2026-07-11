package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
)

// readSpawned reads s.spawned under loopsMu (the counter's guard).
func readSpawned(t *testing.T, s *Session) int {
	t.Helper()
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	return s.spawned
}

// TestNewLoopQuotaCap proves that once M sub-loops have been spawned, the next NewLoop
// is refused with a typed *SessionError{SessionLoopQuotaExceeded}, publishes NO
// LoopStarted, and leaves spawned == M (no over-count). The primary (built by New) does
// not count toward the quota, so a Quota of M permits exactly M successful spawns.
func TestNewLoopQuotaCap(t *testing.T) {
	t.Parallel()
	const quota = 3
	// Depth is set high so the quota — not the depth cap — is the limiting factor (every
	// spawn here is a direct child of the primary, chain length 1).
	s, err := New(context.Background(),
		cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}),
		WithLimits(Limits{Depth: 10, Quota: quota}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	for i := 0; i < quota; i++ {
		if _, err := s.NewLoop(loop.Provenance{LoopID: s.PrimaryLoopID()}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}})); err != nil {
			t.Fatalf("NewLoop #%d within quota: %v", i+1, err)
		}
	}
	if got := readSpawned(t, s); got != quota {
		t.Fatalf("spawned after %d successful spawns = %d, want %d", quota, got, quota)
	}

	// The (quota+1)th spawn is refused.
	_, err = s.NewLoop(loop.Provenance{LoopID: s.PrimaryLoopID()}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("over")}}))
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopQuotaExceeded {
		t.Fatalf("over-quota NewLoop err = %v, want *SessionError{SessionLoopQuotaExceeded}", err)
	}
	// No over-count: spawned stays at the quota (the refused spawn reserved nothing).
	if got := readSpawned(t, s); got != quota {
		t.Errorf("spawned after refused spawn = %d, want %d (no over-count)", got, quota)
	}
	// The refused spawn published no LoopStarted: we saw exactly `quota` of them.
	if n := countLoopStarted(sub, 250*time.Millisecond); n != quota {
		t.Errorf("observed %d LoopStarted, want exactly %d (refused spawn publishes none)", n, quota)
	}
}

// TestNewLoopQuotaConcurrent proves N concurrent NewLoop calls against a Quota of M
// never push spawned over M: exactly M succeed, the rest are refused with
// SessionLoopQuotaExceeded, and the final spawned == M. Run under -race, this proves the
// reservation (spawned++ under loopsMu) is atomic with the quota check.
func TestNewLoopQuotaConcurrent(t *testing.T) {
	t.Parallel()
	const quota = 8
	const goroutines = 40
	s, err := New(context.Background(),
		cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}),
		WithLimits(Limits{Depth: 10, Quota: quota}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	var success, quotaRejected, otherErr atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all at once to maximize contention
			_, err := s.NewLoop(loop.Provenance{LoopID: s.PrimaryLoopID()}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}))
			switch {
			case err == nil:
				success.Add(1)
			case isQuotaExceeded(err):
				quotaRejected.Add(1)
			default:
				otherErr.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := success.Load(); got != quota {
		t.Errorf("successful spawns = %d, want exactly %d", got, quota)
	}
	if got := quotaRejected.Load(); got != goroutines-quota {
		t.Errorf("quota-rejected spawns = %d, want %d", got, goroutines-quota)
	}
	if got := otherErr.Load(); got != 0 {
		t.Errorf("unexpected non-quota errors = %d, want 0", got)
	}
	if got := readSpawned(t, s); got != quota {
		t.Errorf("final spawned = %d, want exactly %d (never exceeded under contention)", got, quota)
	}
}

// TestNewLoopQuotaRollback proves a rolled-back spawn releases its quota reservation: a
// forced loop.New failure (a nil-Client cfg fails loop.New's validation AFTER the
// reservation) decrements spawned, so a later valid spawn still succeeds within the
// quota. Without rollback, the failed spawn would permanently consume a slot.
func TestNewLoopQuotaRollback(t *testing.T) {
	t.Parallel()
	const quota = 2
	s, err := New(context.Background(),
		cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}),
		WithLimits(Limits{Depth: 10, Quota: quota}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// A nil-Client cfg fails loop.New's Config validation synchronously (after the
	// reservation), forcing the rollback path. Repeat MORE than `quota` times: without
	// rollback the quota would be exhausted by these failures and the valid spawns below
	// would be wrongly refused.
	badCfg := loop.Definition{}
	for i := 0; i < quota+2; i++ {
		_, err := s.NewLoop(loop.Provenance{LoopID: s.PrimaryLoopID()}, badCfg)
		var be *loop.BindError
		if !errors.As(err, &be) || be.Kind != loop.BindInvalidDefinition {
			t.Fatalf("forced loop bind failure #%d err = %v, want *loop.BindError{BindInvalidDefinition}", i+1, err)
		}
	}
	// Every failure rolled back, so spawned is back to 0.
	if got := readSpawned(t, s); got != 0 {
		t.Fatalf("spawned after %d rolled-back spawns = %d, want 0 (each released its reservation)", quota+2, got)
	}

	// A full quota of valid spawns now succeeds — proving the slots were released.
	for i := 0; i < quota; i++ {
		if _, err := s.NewLoop(loop.Provenance{LoopID: s.PrimaryLoopID()}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}})); err != nil {
			t.Fatalf("valid spawn #%d after rollbacks: %v, want success", i+1, err)
		}
	}
	if got := readSpawned(t, s); got != quota {
		t.Errorf("spawned after %d valid spawns = %d, want %d", quota, got, quota)
	}
}

// isQuotaExceeded reports whether err is a *SessionError{SessionLoopQuotaExceeded}.
func isQuotaExceeded(err error) bool {
	var se *SessionError
	return errors.As(err, &se) && se.Kind == SessionLoopQuotaExceeded
}
