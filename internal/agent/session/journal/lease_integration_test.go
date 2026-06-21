//go:build integration

package journal_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/nats-io/nats.go"
)

// leaseTTL is the short lease TTL used across the lease integration tests. It is
// short enough that an expiry test advancing the injected clock past it does not
// have to wait on wall-clock, yet the bucket-level backstop TTL never fires within
// a test run.
const leaseTTL = 200 * time.Millisecond

// newLeaseManager builds a LeaseManager over the embedded JetStream with an
// injectable clock. now defaults to time.Now when nil. Each test gets a fresh
// bucket name (derived from the test name) so parallel tests never collide.
func newLeaseManager(t *testing.T, js nats.JetStreamContext, now func() time.Time) *journal.LeaseManager {
	t.Helper()
	opts := []journal.LeaseOption{journal.WithLeaseTTL(leaseTTL)}
	if now != nil {
		opts = append(opts, journal.WithLeaseClock(now))
	}
	lm, err := journal.NewLeaseManager(js, opts...)
	if err != nil {
		t.Fatalf("NewLeaseManager: %v", err)
	}
	return lm
}

// TestLeaseEpochMonotonic asserts the epoch strictly increases across successive
// acquire->release cycles for the same session: each fresh acquisition fences a
// higher epoch than the last, so a new owner always out-ranks every prior owner.
func TestLeaseEpochMonotonic(t *testing.T) {
	sid := seedUUID(0x60)
	_, js := newEmbeddedJS(t)
	lm := newLeaseManager(t, js, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var prev uint64
	for i := 0; i < 4; i++ {
		lease, err := lm.Acquire(ctx, sid)
		if err != nil {
			t.Fatalf("Acquire #%d: %v", i, err)
		}
		if lease.SessionID() != sid {
			t.Errorf("lease.SessionID() = %v, want %v", lease.SessionID(), sid)
		}
		if lease.Epoch() <= prev {
			t.Fatalf("Acquire #%d epoch = %d, want > %d (monotonic)", i, lease.Epoch(), prev)
		}
		if !lease.Valid() {
			t.Errorf("Acquire #%d lease not valid immediately after acquire", i)
		}
		prev = lease.Epoch()
		if err := lease.Release(ctx); err != nil {
			t.Fatalf("Release #%d: %v", i, err)
		}
		if lease.Valid() {
			t.Errorf("Acquire #%d lease still valid after Release", i)
		}
	}
	if prev < 4 {
		t.Errorf("final epoch = %d, want >= 4 across four acquire/release cycles", prev)
	}
}

// TestLeaseConcurrentAcquireFails asserts that while one holder holds a live lease,
// a second Acquire for the same session loses the CAS and fails with a typed
// *LeaseHeldError carrying the current holder's epoch — only one writer wins.
func TestLeaseConcurrentAcquireFails(t *testing.T) {
	sid := seedUUID(0x61)
	_, js := newEmbeddedJS(t)
	lm := newLeaseManager(t, js, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := lm.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer func() { _ = first.Release(ctx) }()

	_, err = lm.Acquire(ctx, sid)
	if err == nil {
		t.Fatalf("second Acquire succeeded while first holds; want *LeaseHeldError")
	}
	var held *journal.LeaseHeldError
	if !errors.As(err, &held) {
		t.Fatalf("second Acquire error %v is not *LeaseHeldError", err)
	}
	if held.SessionID != sid {
		t.Errorf("LeaseHeldError.SessionID = %v, want %v", held.SessionID, sid)
	}
	if held.Epoch != first.Epoch() {
		t.Errorf("LeaseHeldError.Epoch = %d, want %d (current holder's epoch)", held.Epoch, first.Epoch())
	}
	if !first.Valid() {
		t.Errorf("first lease should remain valid after a losing concurrent Acquire")
	}
}

// TestLeaseConcurrentAcquireRace runs many goroutines racing to Acquire the same
// session at once and asserts exactly one wins; every loser sees *LeaseHeldError.
func TestLeaseConcurrentAcquireRace(t *testing.T) {
	sid := seedUUID(0x62)
	_, js := newEmbeddedJS(t)
	lm := newLeaseManager(t, js, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		losers  int
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			lease, err := lm.Acquire(ctx, sid)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners++
				_ = lease
				return
			}
			var held *journal.LeaseHeldError
			if errors.As(err, &held) {
				losers++
				return
			}
			t.Errorf("racer got unexpected error %v (not *LeaseHeldError)", err)
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1 (single-holder)", winners)
	}
	if losers != racers-1 {
		t.Errorf("losers = %d, want %d", losers, racers-1)
	}
}

// TestLeaseTTLExpiryAllowsTakeover asserts the injected-clock expiry path: holder A
// acquires (epoch N), then the clock is advanced past A's ExpiresAt. A fresh Acquire
// now succeeds with a higher epoch (it CAS-replaced the stale entry), and A's lease is
// observably lost (Valid()==false and Lost() fires).
func TestLeaseTTLExpiryAllowsTakeover(t *testing.T) {
	sid := seedUUID(0x63)
	_, js := newEmbeddedJS(t)

	var (
		mu  sync.Mutex
		clk = time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clk
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		clk = clk.Add(d)
	}
	lm := newLeaseManager(t, js, now)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, err := lm.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	epochA := a.Epoch()

	// Before expiry, a second Acquire must still lose (A's lease is live).
	if _, err := lm.Acquire(ctx, sid); err == nil {
		t.Fatalf("Acquire before expiry succeeded; A's lease is still live")
	}

	// Advance the clock past A's ExpiresAt (plus heartbeat slack) so the stored entry
	// is, per the injected clock, expired and eligible for CAS takeover.
	advance(leaseTTL * 4)

	b, err := lm.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire(B) after expiry: %v", err)
	}
	if b.Epoch() <= epochA {
		t.Errorf("B epoch = %d, want > A epoch %d after takeover", b.Epoch(), epochA)
	}

	// A's lease must be observably lost: B took the entry to a higher epoch, so A's
	// heartbeat CAS can no longer renew. Wait on Lost() with a bound.
	select {
	case <-a.Lost():
	case <-time.After(5 * time.Second):
		t.Fatalf("A.Lost() never fired after B took over")
	}
	if a.Valid() {
		t.Errorf("A.Valid() = true after B took over; want false (lease lost)")
	}

	_ = b.Release(ctx)
}
