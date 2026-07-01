package aci

// This file implements the attested-session cache (Task 5.1): a per-model cache of
// the validated *VerifiedReport (verify.go, Task 2.8) with a short TTL, so the
// expensive full DCAP attestation (fetch report + VerifyReport) isn't re-run on
// every request. The attestation itself is injected as the attestFunc seam — tests
// drive a fake, and Task 5.2 wires it to the real attest-then-VerifyReport flow.
//
// TTL vs. the report's stale_after. The cache TTL (defaultSessionTTL, ~50s) is a
// FIXED sub-window deliberately chosen to sit well below the gateway's attestation
// freshness window [fetched_at, stale_after) (verify.go step 8), which is far
// larger (~1h in the fixture). Re-attesting every ~50s therefore always keeps the
// cached report comfortably inside its freshness window, so we do NOT need to read
// stale_after off the report to size the cache: a fixed sub-window is simpler and
// strictly safe. (A future refinement could clamp TTL to min(ttl, stale_after-now),
// but it is not needed for correctness here.)

import (
	"context"
	"sync"
	"time"
)

// defaultSessionTTL is the per-model cache lifetime: how long an attested
// *VerifiedReport is served before a fresh attestation is forced. ~50s is the
// "margin below stale_after" — short enough that the cached report always stays
// inside the gateway's much larger freshness window (see file header), yet long
// enough to amortize the expensive DCAP attestation across a burst of requests.
const defaultSessionTTL = 50 * time.Second

// attestFunc performs a fresh attestation for one model: it fetches the model's
// report and runs the full VerifyReport chain, returning the validated
// *VerifiedReport on success or a typed *llm.AttestationError on any failure. It
// is the injected seam the sessionCache calls on a miss/expiry; the live wiring
// (Task 5.2) supplies the real fetch+verify, while tests supply a fake. It takes a
// context so the live attestation's network I/O is cancelable/deadline-bounded.
type attestFunc func(ctx context.Context, model string) (*VerifiedReport, error)

// sessionEntry is one cached attestation: the validated report and the wall-clock
// instant it was cached. An entry is fresh while now()-cachedAt < ttl.
type sessionEntry struct {
	report   *VerifiedReport
	cachedAt time.Time
}

// sessionCache is a concurrent, per-model cache of validated *VerifiedReports with
// a fixed TTL. It calls attest only on a miss or after a TTL expiry; successful
// reports are cached, failures are NOT (so the next get retries). It is safe for
// concurrent use: the map is guarded by mu, and — crucially — mu is NOT held
// across the attest call, so a slow attestation for one model never serializes
// gets for other models. A burst of simultaneous misses for the same model may
// attest more than once (last-writer-wins); that is acceptable (correctness over
// dedup — singleflight is intentionally out of scope here).
type sessionCache struct {
	mu      sync.Mutex
	entries map[string]sessionEntry
	attest  attestFunc
	ttl     time.Duration
	now     func() time.Time
}

// newSessionCache builds a sessionCache that re-attests via attest, treats a
// cached report as fresh for ttl, and reads the wall clock via now (injected so
// tests drive TTL deterministically). attest and now must be non-nil; ttl should
// be positive (a non-positive ttl makes every get a miss).
func newSessionCache(attest attestFunc, ttl time.Duration, now func() time.Time) *sessionCache {
	return &sessionCache{
		entries: make(map[string]sessionEntry),
		attest:  attest,
		ttl:     ttl,
		now:     now,
	}
}

// get returns a validated *VerifiedReport for model, serving a cached one while it
// is within ttl and otherwise running a fresh attestation. On a HIT (an entry
// exists and now()-cachedAt < ttl) it returns the cached report WITHOUT calling
// attest. On a MISS or EXPIRY it calls attest(ctx, model); on success it stores
// {report, cachedAt: now()} and returns it; on FAILURE it caches nothing and
// returns the error, so the next get retries.
//
// Locking: the cache lookup and the store each happen under mu, but mu is RELEASED
// across the attest call. So concurrent gets for different models proceed in
// parallel, and concurrent misses for the SAME model may each attest (the last
// store wins) — accepted by design.
func (c *sessionCache) get(ctx context.Context, model string) (*VerifiedReport, error) {
	if report, ok := c.lookup(model); ok {
		return report, nil
	}

	// Miss or expiry: attest WITHOUT holding the lock so other models proceed.
	report, err := c.attest(ctx, model)
	if err != nil {
		// Fail-open for caching: do NOT store a failure, so the next get retries.
		return nil, err
	}

	c.store(model, report)
	return report, nil
}

// lookup returns the cached report for model if an entry exists and is still
// within ttl, else (nil, false). It holds mu only for the map read so the caller
// can release it before the network-bound attest.
func (c *sessionCache) lookup(model string) (*VerifiedReport, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[model]
	if !ok {
		return nil, false
	}
	if c.now().Sub(entry.cachedAt) >= c.ttl {
		return nil, false
	}
	return entry.report, true
}

// store records report as the cached entry for model, stamped at now(). It is
// called only after a successful attest; concurrent stores for the same model are
// last-writer-wins.
func (c *sessionCache) store(model string, report *VerifiedReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[model] = sessionEntry{report: report, cachedAt: c.now()}
}
