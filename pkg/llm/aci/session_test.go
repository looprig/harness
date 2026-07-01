package aci

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file tests the attested-session cache (Task 5.1): a per-model cache of the
// expensive *VerifiedReport with a short TTL, so the full DCAP attestation isn't
// re-run on every request. The attestation is injected as the attestFunc seam, so
// these tests drive a fake (an atomic counter + a fixed report) and a controllable
// now() — no network, no real DCAP. They assert the four cache behaviors (hit
// until TTL, expiry re-attests, failures NOT cached, per-model isolation) plus
// concurrency safety; the concurrency case is meaningful only under `-race`.

// fakeReport returns a distinct, non-nil *VerifiedReport tagged by model so a test
// can assert it got back the SAME report instance the cache stored (pointer
// identity) and that distinct models get distinct reports.
func fakeReport(model string) *VerifiedReport {
	return &VerifiedReport{WorkloadID: "sha256:" + model}
}

// countingAttest is a fake attestFunc backed by an atomic call counter. Every call
// increments calls and returns fakeReport(model). It is the happy-path seam: the
// counter lets a test assert attest was (or was NOT) invoked the expected number
// of times across cache hits/misses.
type countingAttest struct {
	calls atomic.Int64
}

func (c *countingAttest) attest(_ context.Context, model string) (*VerifiedReport, error) {
	c.calls.Add(1)
	return fakeReport(model), nil
}

// errAttestSentinel is the canned failure an always-failing attestFunc returns, so
// a test can assert get() surfaces the underlying attest error verbatim and does
// NOT cache it.
var errAttestSentinel = errors.New("attest failed")

func TestSessionCacheHitUntilTTL(t *testing.T) {
	t.Parallel()
	fake := &countingAttest{}
	clock := time.Unix(1_700_000_000, 0)
	c := newSessionCache(fake.attest, defaultSessionTTL, func() time.Time { return clock })

	// First get: a MISS, so attest runs exactly once.
	got1, err := c.get(context.Background(), "modelA")
	if err != nil {
		t.Fatalf("first get() error = %v, want nil", err)
	}
	if got1 == nil || got1.WorkloadID != "sha256:modelA" {
		t.Fatalf("first get() report = %+v, want fakeReport(modelA)", got1)
	}
	if want := int64(1); fake.calls.Load() != want {
		t.Fatalf("after first get(), attest calls = %d, want %d", fake.calls.Load(), want)
	}

	// Second get within TTL (clock unchanged): a HIT, attest NOT called again, and
	// the SAME cached pointer comes back.
	got2, err := c.get(context.Background(), "modelA")
	if err != nil {
		t.Fatalf("second get() error = %v, want nil", err)
	}
	if got2 != got1 {
		t.Errorf("second get() returned a different report instance; want the cached one")
	}
	if want := int64(1); fake.calls.Load() != want {
		t.Errorf("after second get() within TTL, attest calls = %d, want %d (no re-attest)", fake.calls.Load(), want)
	}

	// A get one second before the TTL boundary is still a HIT.
	clock = clock.Add(defaultSessionTTL - time.Second)
	if _, err := c.get(context.Background(), "modelA"); err != nil {
		t.Fatalf("get() just below TTL error = %v, want nil", err)
	}
	if want := int64(1); fake.calls.Load() != want {
		t.Errorf("get() just below TTL: attest calls = %d, want %d (still cached)", fake.calls.Load(), want)
	}
}

func TestSessionCacheExpiryReAttests(t *testing.T) {
	t.Parallel()
	fake := &countingAttest{}
	clock := time.Unix(1_700_000_000, 0)
	c := newSessionCache(fake.attest, defaultSessionTTL, func() time.Time { return clock })

	if _, err := c.get(context.Background(), "modelA"); err != nil {
		t.Fatalf("first get() error = %v, want nil", err)
	}
	if want := int64(1); fake.calls.Load() != want {
		t.Fatalf("after first get(), attest calls = %d, want %d", fake.calls.Load(), want)
	}

	// Advance now to exactly the TTL boundary: age == ttl is EXPIRED (the freshness
	// window is the half-open [cachedAt, cachedAt+ttl)), so this re-attests.
	clock = clock.Add(defaultSessionTTL)
	if _, err := c.get(context.Background(), "modelA"); err != nil {
		t.Fatalf("get() at TTL boundary error = %v, want nil", err)
	}
	if want := int64(2); fake.calls.Load() != want {
		t.Errorf("after expiry, attest calls = %d, want %d (re-attest)", fake.calls.Load(), want)
	}
}

func TestSessionCacheFailureNotCached(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	// Fail the first two calls, then succeed: proves each failed get re-attests
	// (nothing cached) and that a later success populates the cache normally.
	attest := func(_ context.Context, model string) (*VerifiedReport, error) {
		n := calls.Add(1)
		if n <= 2 {
			return nil, errAttestSentinel
		}
		return fakeReport(model), nil
	}
	clock := time.Unix(1_700_000_000, 0)
	c := newSessionCache(attest, defaultSessionTTL, func() time.Time { return clock })

	// First get: attest fails; get returns that error and caches NOTHING.
	_, err := c.get(context.Background(), "modelA")
	if !errors.Is(err, errAttestSentinel) {
		t.Fatalf("first get() error = %v, want errAttestSentinel", err)
	}
	if want := int64(1); calls.Load() != want {
		t.Fatalf("after first failed get(), attest calls = %d, want %d", calls.Load(), want)
	}

	// Second get: a failure was NOT cached, so attest is called again (still fails).
	_, err = c.get(context.Background(), "modelA")
	if !errors.Is(err, errAttestSentinel) {
		t.Fatalf("second get() error = %v, want errAttestSentinel", err)
	}
	if want := int64(2); calls.Load() != want {
		t.Errorf("after second failed get(), attest calls = %d, want %d (failure not cached)", calls.Load(), want)
	}

	// Third get: attest now succeeds and the report IS cached.
	got, err := c.get(context.Background(), "modelA")
	if err != nil {
		t.Fatalf("third get() error = %v, want nil", err)
	}
	if got == nil || got.WorkloadID != "sha256:modelA" {
		t.Fatalf("third get() report = %+v, want fakeReport(modelA)", got)
	}
	if want := int64(3); calls.Load() != want {
		t.Fatalf("after third (success) get(), attest calls = %d, want %d", calls.Load(), want)
	}

	// Fourth get within TTL: a HIT off the now-cached success, no re-attest.
	if _, err := c.get(context.Background(), "modelA"); err != nil {
		t.Fatalf("fourth get() error = %v, want nil", err)
	}
	if want := int64(3); calls.Load() != want {
		t.Errorf("after fourth get() within TTL, attest calls = %d, want %d (success cached)", calls.Load(), want)
	}
}

func TestSessionCachePerModelIsolation(t *testing.T) {
	t.Parallel()
	fake := &countingAttest{}
	clock := time.Unix(1_700_000_000, 0)
	c := newSessionCache(fake.attest, defaultSessionTTL, func() time.Time { return clock })

	gotA, err := c.get(context.Background(), "modelA")
	if err != nil {
		t.Fatalf("get(modelA) error = %v, want nil", err)
	}
	gotB, err := c.get(context.Background(), "modelB")
	if err != nil {
		t.Fatalf("get(modelB) error = %v, want nil", err)
	}

	// Two distinct models → two attests, and the entries are independent reports.
	if want := int64(2); fake.calls.Load() != want {
		t.Fatalf("after get(modelA)+get(modelB), attest calls = %d, want %d", fake.calls.Load(), want)
	}
	if gotA == gotB {
		t.Errorf("get(modelA) and get(modelB) returned the same report instance; entries must be per-model")
	}
	if gotA.WorkloadID != "sha256:modelA" || gotB.WorkloadID != "sha256:modelB" {
		t.Errorf("per-model reports crossed wires: A=%q B=%q", gotA.WorkloadID, gotB.WorkloadID)
	}

	// get(modelA) again within TTL is a HIT — modelB's miss did not disturb it.
	gotA2, err := c.get(context.Background(), "modelA")
	if err != nil {
		t.Fatalf("second get(modelA) error = %v, want nil", err)
	}
	if gotA2 != gotA {
		t.Errorf("second get(modelA) returned a different instance; want the cached one")
	}
	if want := int64(2); fake.calls.Load() != want {
		t.Errorf("second get(modelA) within TTL: attest calls = %d, want %d (cached)", fake.calls.Load(), want)
	}
}

func TestSessionCacheConcurrent(t *testing.T) {
	t.Parallel()
	fake := &countingAttest{}
	clock := time.Unix(1_700_000_000, 0)
	c := newSessionCache(fake.attest, defaultSessionTTL, func() time.Time { return clock })

	models := []string{"modelA", "modelB", "modelC"}

	// Launch a burst of concurrent get()s across a mix of models. Under `-race`
	// this asserts there is no data race on the cache map; a burst of simultaneous
	// misses MAY attest a model more than once (last-writer-wins is acceptable), so
	// we assert correctness — every get succeeds and returns a sane report — not an
	// exactly-once attest count.
	const goroutines = 64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		model := models[i%len(models)]
		go func() {
			defer wg.Done()
			got, err := c.get(context.Background(), model)
			if err != nil {
				errs <- err
				return
			}
			if got == nil || got.WorkloadID != "sha256:"+model {
				errs <- errors.New("unexpected report for " + model)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent get() error: %v", err)
	}

	// After the burst settles, every model is cached: a serial get within TTL is a
	// HIT (no further attest), proving the cache reached a consistent state.
	before := fake.calls.Load()
	for _, model := range models {
		got, err := c.get(context.Background(), model)
		if err != nil {
			t.Fatalf("post-burst get(%s) error = %v, want nil", model, err)
		}
		if got.WorkloadID != "sha256:"+model {
			t.Errorf("post-burst get(%s) report = %q, want cached", model, got.WorkloadID)
		}
	}
	if after := fake.calls.Load(); after != before {
		t.Errorf("post-burst serial gets re-attested: calls went %d -> %d, want stable (all cached)", before, after)
	}
}
