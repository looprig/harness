package serve

import (
	"sync"

	"github.com/looprig/core/uuid"
)

// registry is the in-process table mapping a session id to its live LiveSession,
// so the live/control HTTP routes can resolve {sid} to the session an incoming
// request targets. Its single responsibility is that membership bookkeeping: it
// guards the map, nothing more.
//
// Concurrency contract: the mutex protects ONLY the map (which ids are live).
// A registry method never calls a LiveSession method while holding the lock —
// each method touches the map and returns, and the caller invokes session methods
// after the value is handed back. This keeps a slow or blocking session call from
// stalling every other route that just needs to look up a different id.
type registry struct {
	mu       sync.RWMutex
	sessions map[uuid.UUID]LiveSession
}

// newRegistry builds an empty live-session table ready for use.
func newRegistry() *registry {
	return &registry{sessions: make(map[uuid.UUID]LiveSession)}
}

// get returns the live session registered under id, and whether one existed. It
// takes only a read lock; the caller drives the returned session after get
// returns (never under the lock).
func (r *registry) get(id uuid.UUID) (LiveSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[id]
	return s, ok
}

// put registers s under id, overwriting any prior entry.
func (r *registry) put(id uuid.UUID, s LiveSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[id] = s
}

// putIfAbsent registers s under id only if no session is already live for id,
// reporting whether it stored. It is the fail-secure guard against a client-
// controlled id silently overwriting (and orphaning) a live session: a collision
// is rejected rather than clobbering the existing entry.
func (r *registry) putIfAbsent(id uuid.UUID, s LiveSession) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sessions[id]; exists {
		return false
	}
	r.sessions[id] = s
	return true
}

// delete removes id from the table and returns the entry it removed (and whether
// one existed), so the caller can tear the session down OUTSIDE the lock. delete
// performs no session call itself, precisely so nothing blocks under the lock.
func (r *registry) delete(id uuid.UUID) (LiveSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if ok {
		delete(r.sessions, id)
	}
	return s, ok
}
