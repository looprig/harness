package serve

import (
	"sync"
	"time"
)

// defaultIdempotencyTTL is the lifetime of a recorded create outcome under an
// Idempotency-Key when the store is built with its secure default: 24h. After it
// elapses the entry is treated as absent (a repeat of the same key mints a fresh
// session), so a client's retry window is generous but bounded — the TTL is what
// keeps the per-pod map from growing without limit. Tests override the (unexported)
// idempotencyStore.ttl / .now fields directly for deterministic expiry, mirroring
// the heartbeat-interval injection convention.
const defaultIdempotencyTTL = 24 * time.Hour

// maxIdempotencyKeyLen bounds an accepted Idempotency-Key at 255 bytes. A key over
// the bound is rejected at the HTTP boundary (400) before it can be used as a map
// key, so a caller cannot pin unbounded memory with a single oversized key. 255 is
// comfortably above any sane client-generated token (a UUID is 36 bytes).
const maxIdempotencyKeyLen = 255

// idempotencyStatus is the outcome of an idempotencyStore.lookup: the three-way
// decision the create handler switches on. It is a typed enum, never a bare int at
// the call site.
type idempotencyStatus int

const (
	// idemMiss means no fresh entry exists for the key (absent, or expired and
	// lazily evicted): the caller proceeds with a normal create.
	idemMiss idempotencyStatus = iota
	// idemHit means a fresh entry exists for the key AND the request body is
	// byte-identical: the caller replays the cached response and does NOT re-run.
	idemHit
	// idemConflict means a fresh entry exists for the key but the request body
	// differs: the caller rejects the request with 409 (the key was reused for a
	// different request).
	idemConflict
)

// idempotencyEntry is one recorded create outcome: the sha256 of the exact request
// body that produced it (so "same request" is byte-identical), the cached success
// response to replay, and the wall-clock instant the entry stops being fresh.
type idempotencyEntry struct {
	bodyHash  [32]byte
	response  createResponse
	expiresAt time.Time
}

// idempotencyStore is the per-pod (NOT distributed — SPEC §6, Decision #18)
// in-memory record of completed POST /v1/sessions outcomes keyed by Idempotency-Key.
// One instance is built per Handler and shared across every request; all access is
// guarded by mu. It is deliberately richer than flow's equivalent: entries carry a
// TTL (so the map is bounded) and a body hash (so a reused key with a different body
// is a 409 rather than a silent wrong replay).
//
// Eviction is LAZY: an expired entry is dropped when its key is next looked up.
// There is no background sweeper — a key that is written once and never revisited
// lingers until the process exits. That is acceptable because keys are
// client-supplied per create and the working set is small; if a workload ever
// accumulates many write-once keys, a periodic sweep is the follow-up (the TTL field
// already carries the information a sweeper would need). now is injected so expiry is
// deterministic in tests.
type idempotencyStore struct {
	mu      sync.Mutex
	entries map[string]idempotencyEntry
	ttl     time.Duration
	now     func() time.Time
}

// newIdempotencyStore builds an empty store with the given TTL and the real clock. A
// non-positive ttl falls back to defaultIdempotencyTTL (fail-safe: an entry always
// has a bounded, positive lifetime).
func newIdempotencyStore(ttl time.Duration) *idempotencyStore {
	if ttl <= 0 {
		ttl = defaultIdempotencyTTL
	}
	return &idempotencyStore{
		entries: make(map[string]idempotencyEntry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// lookup reports what the caller should do for (key, bodyHash) as of now. It never
// calls the rig or registry — it only consults the guarded map — so it is safe to
// hold the lock for its whole (short) duration. An entry whose expiresAt is at or
// before now is expired: it is lazily evicted and reported as a miss.
func (s *idempotencyStore) lookup(key string, bodyHash [32]byte, now time.Time) (createResponse, idempotencyStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok {
		return createResponse{}, idemMiss
	}
	if !now.Before(e.expiresAt) {
		delete(s.entries, key)
		return createResponse{}, idemMiss
	}
	if e.bodyHash != bodyHash {
		return createResponse{}, idemConflict
	}
	return e.response, idemHit
}

// store records a completed create outcome under key with expiresAt = now + ttl,
// overwriting any prior entry for the key. The caller invokes it AFTER the create has
// fully succeeded and NEVER while holding any other lock; the store takes only its
// own mu.
func (s *idempotencyStore) store(key string, bodyHash [32]byte, resp createResponse, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = idempotencyEntry{
		bodyHash:  bodyHash,
		response:  resp,
		expiresAt: now.Add(s.ttl),
	}
}
