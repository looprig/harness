package serve

import (
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
)

// fixedIdemResponse builds a distinct createResponse for store/lookup assertions.
func fixedIdemResponse(t *testing.T, sid string) createResponse {
	t.Helper()
	return createResponse{SessionID: parseTestUUID(t, sid)}
}

func TestIdempotencyStoreLookup(t *testing.T) {
	t.Parallel()

	const sid = "11111111-1111-1111-1111-111111111111"
	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	hashA := sha256.Sum256([]byte(`{"a":1}`))
	hashB := sha256.Sum256([]byte(`{"b":2}`))
	resp := fixedIdemResponse(t, sid)

	tests := []struct {
		name       string
		seed       bool      // seed an entry (hashA) before lookup
		seedAt     time.Time // when the seed was stored
		lookupHash [32]byte  // hash presented at lookup
		lookupAt   time.Time // when lookup happens
		wantStatus idempotencyStatus
		wantResp   bool // expect the cached response returned
	}{
		{
			name:       "empty store is a miss",
			seed:       false,
			lookupHash: hashA,
			lookupAt:   base,
			wantStatus: idemMiss,
		},
		{
			name:       "fresh same-body entry is a hit",
			seed:       true,
			seedAt:     base,
			lookupHash: hashA,
			lookupAt:   base.Add(time.Minute),
			wantStatus: idemHit,
			wantResp:   true,
		},
		{
			name:       "fresh different-body entry is a conflict",
			seed:       true,
			seedAt:     base,
			lookupHash: hashB,
			lookupAt:   base.Add(time.Minute),
			wantStatus: idemConflict,
		},
		{
			name:       "expired entry is a miss",
			seed:       true,
			seedAt:     base,
			lookupHash: hashA,
			lookupAt:   base.Add(2 * time.Hour),
			wantStatus: idemMiss,
		},
		{
			name:       "entry exactly at expiry is a miss",
			seed:       true,
			seedAt:     base,
			lookupHash: hashA,
			lookupAt:   base.Add(time.Hour), // ttl == 1h below → expiresAt == lookupAt
			wantStatus: idemMiss,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newIdempotencyStore(time.Hour)
			if tt.seed {
				s.store("key", hashA, resp, tt.seedAt)
			}
			got, status := s.lookup("key", tt.lookupHash, tt.lookupAt)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if tt.wantResp && got.SessionID != resp.SessionID {
				t.Errorf("response session_id = %v, want %v", got.SessionID, resp.SessionID)
			}
		})
	}
}

// TestIdempotencyStoreLazyEviction proves an expired lookup drops the entry (so it
// does not linger indefinitely once revisited).
func TestIdempotencyStoreLazyEviction(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	hash := sha256.Sum256([]byte("x"))
	s := newIdempotencyStore(time.Hour)
	s.store("k", hash, fixedIdemResponse(t, "22222222-2222-2222-2222-222222222222"), base)

	if _, status := s.lookup("k", hash, base.Add(2*time.Hour)); status != idemMiss {
		t.Fatalf("expired lookup status = %d, want miss", status)
	}
	s.mu.Lock()
	_, present := s.entries["k"]
	s.mu.Unlock()
	if present {
		t.Errorf("expired entry was not lazily evicted")
	}
}

// TestNewIdempotencyStoreTTLFallback pins the fail-safe default: a non-positive TTL
// falls back to the 24h default rather than zero (which would expire everything).
func TestNewIdempotencyStoreTTLFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{name: "positive ttl kept", ttl: time.Hour, want: time.Hour},
		{name: "zero ttl falls back", ttl: 0, want: defaultIdempotencyTTL},
		{name: "negative ttl falls back", ttl: -time.Second, want: defaultIdempotencyTTL},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := newIdempotencyStore(tt.ttl).ttl; got != tt.want {
				t.Errorf("ttl = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIdempotencyStoreConcurrentAccess is a -race smoke test: concurrent store and
// lookup on one store must be data-race free. It asserts no panic/race, not a
// particular interleaving.
func TestIdempotencyStoreConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := newIdempotencyStore(time.Hour)
	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	hash := sha256.Sum256([]byte("y"))
	resp := createResponse{SessionID: uuid.UUID{}}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.store("k", hash, resp, base) }()
		go func() { defer wg.Done(); s.lookup("k", hash, base) }()
	}
	wg.Wait()
}
