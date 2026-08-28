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
	sessions map[uuid.UUID]registration
	// nextToken is the source of registration tokens. It is plain (not sync/atomic)
	// because it is only ever read and incremented under mu, alongside the map write
	// it belongs to; an atomic here would buy nothing and would let the mint drift
	// out of the critical section that makes it meaningful.
	nextToken uint64
}

// registration is what the table stores: the session plus the token identifying THIS
// entry of it. The token is what an eviction is checked against, so the guard never
// compares LiveSession values — see deleteMatching.
type registration struct {
	sess  LiveSession
	token uint64
}

// newRegistry builds an empty live-session table ready for use.
func newRegistry() *registry {
	return &registry{sessions: make(map[uuid.UUID]registration)}
}

// get returns the live session registered under id, and whether one existed. It
// takes only a read lock; the caller drives the returned session after get
// returns (never under the lock).
func (r *registry) get(id uuid.UUID) (LiveSession, bool) {
	s, _, ok := r.lookup(id)
	return s, ok
}

// lookup is get plus the entry's registration token, for the one caller that may later
// need to evict what it just read (the liveness probe). Every other caller wants get:
// a token is meaningful only to code that will hand it back to deleteMatching.
func (r *registry) lookup(id uuid.UUID) (LiveSession, uint64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.sessions[id]
	return entry.sess, entry.token, ok
}

// put registers s under id, overwriting any prior entry, and returns the token
// identifying this registration. The token is monotonically increasing and never
// reissued, so it names one specific entry of one specific session: a caller that
// later observes that session's death hands the token back to deleteMatching, which
// then evicts only if this registration is still the live one.
//
// Tokens start at 1, so the zero value is never a live registration and a caller that
// forgot to capture one can never accidentally evict.
//
// Callers with nothing to evict — the lifecycle routes — ignore the return value.
func (r *registry) put(id uuid.UUID, s LiveSession) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextToken++
	token := r.nextToken
	r.sessions[id] = registration{sess: s, token: token}
	return token
}

// putIfAbsent registers s under id only if no session is already live for id, returning
// the token identifying this registration and whether it stored. It is the fail-secure
// guard against an id silently overwriting (and orphaning) a live session: a collision is
// rejected rather than clobbering the existing entry.
//
// The token is returned for the same reason put returns one — a caller that stores may
// then watch for that session's death and hand the token back to deleteMatching. On a
// COLLISION the returned token is 0, which names no registration (tokens start at 1), so
// even a caller that ignores the bool and evicts on the returned token removes nothing:
// the incumbent it lost to cannot be evicted on the loser's behalf.
func (r *registry) putIfAbsent(id uuid.UUID, s LiveSession) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sessions[id]; exists {
		return 0, false
	}
	r.nextToken++
	r.sessions[id] = registration{sess: s, token: r.nextToken}
	return r.nextToken, true
}

// deleteMatching removes id ONLY if it is still held by the registration named by
// token, reporting whether it removed anything. It is the eviction counterpart to
// putIfAbsent's fail-secure no-overwrite: an eviction is driven by an observation of
// one specific session's death, and that observation can be stale by the time it
// reaches the map, because a restore may already have re-registered a NEW live session
// under the same sid. An unguarded delete would then remove the live replacement on the
// corpse's behalf, 404-ing a running session. Checking the token first makes a stale
// observation a no-op.
//
// The guard is the token rather than the LiveSession for two reasons. Interface == is
// a run-time panic ("comparing uncomparable type") when the dynamic type is a struct
// held by value with a slice, map or func field. LiveSession is exported and harness is
// a library, so those dynamic types are chosen by external implementors, and the
// comparison would run on register's watcher GOROUTINE — an unrecoverable crash of the
// whole process, with a stack naming registry internals rather than the implementor
// that supplied the type. Every implementation is a pointer today, so the panic is
// latent, but it is not ours to keep latent.
//
// A token is also the stronger test. It distinguishes two DISTINCT registrations even
// of the same session value, which identity cannot: re-register a session under its own
// sid and == would call the first registration's stale eviction a match.
//
// Like every registry method it touches the map and returns, calling nothing on the
// session under the lock.
func (r *registry) deleteMatching(id uuid.UUID, token uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.sessions[id]; !ok || current.token != token {
		return false
	}
	delete(r.sessions, id)
	return true
}

// delete removes id from the table and returns the entry it removed (and whether
// one existed), so the caller can tear the session down OUTSIDE the lock. delete
// performs no session call itself, precisely so nothing blocks under the lock.
func (r *registry) delete(id uuid.UUID) (LiveSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[id]
	if ok {
		delete(r.sessions, id)
	}
	return entry.sess, ok
}
