package serve

import (
	"testing"
	"time"

	"github.com/looprig/core/uuid"
)

// doneFakeSession is a LiveSession that ALSO satisfies the optional SessionDone
// extension, standing in for *sessionruntime.Session. Its Done channel is open until
// the test calls shutdown, which is exactly the transition the real session publishes
// when Shutdown begins.
type doneFakeSession struct {
	fakeLiveSession
	done chan struct{}
}

func newDoneFakeSession(id int) *doneFakeSession {
	return &doneFakeSession{fakeLiveSession: fakeLiveSession{id: id}, done: make(chan struct{})}
}

func (d *doneFakeSession) Done() <-chan struct{} { return d.done }

// shutdown publishes death the way the real session does at the start of teardown.
func (d *doneFakeSession) shutdown() { close(d.done) }

// testServer builds a server with no rig or reader: the registry helpers under test
// touch neither.
func testServer(t *testing.T) *server[*fakeSession, fakeSessionOption] {
	t.Helper()
	return newServer[*fakeSession, fakeSessionOption](nil, nil, newConfig())
}

// waitAbsent blocks until id is no longer registered, failing the test if it is still
// there after a generous deadline. Eviction is driven by a watcher goroutine, so the
// test must wait for it rather than assume it has already run.
func waitAbsent(t *testing.T, r *registry, id uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := r.get(id); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("session still registered after its Done channel closed; the watcher never evicted it, so its routes keep resolving a corpse")
		}
		time.Sleep(time.Millisecond)
	}
}

// assertRegistered checks that id currently resolves to want.
func assertRegistered(t *testing.T, r *registry, id uuid.UUID, want LiveSession) {
	t.Helper()
	got, ok := r.get(id)
	if !ok {
		t.Fatalf("get(%v): entry missing, want %v", id, want)
	}
	if got != want {
		t.Fatalf("get(%v) = %v, want %v", id, got, want)
	}
}

// TestServerRegisterEvictsOnShutdown is the eager half of the fix. A client holding an
// open SSE stream may never issue another request, so no amount of probing on next use
// can free it: something must be watching. Once the session publishes death the
// registry entry has to go, or the dead session, its hub subscription and its handler
// goroutine stay pinned for the life of the process.
func TestServerRegisterEvictsOnShutdown(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	id := mustUUID(t)
	sess := newDoneFakeSession(1)

	srv.register(id, sess)
	assertRegistered(t, srv.registry, id, sess)

	sess.shutdown()
	waitAbsent(t, srv.registry, id)
}

// TestServerLiveSessionProbesADeadEntry is the lazy half: the deterministic backstop
// for the window between close(done) and the watcher goroutine being scheduled. The
// entry is seeded with put, NOT register, so no watcher exists and only the probe can
// find the corpse — a request landing in that window must still get a clean miss, not
// a handle to a dead session.
func TestServerLiveSessionProbesADeadEntry(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	id := mustUUID(t)
	sess := newDoneFakeSession(1)

	srv.registry.put(id, sess)
	sess.shutdown()

	if got, ok := srv.liveSession(id); ok {
		t.Fatalf("liveSession() = %v, true for a shut-down session; the route would drive a corpse", got)
	}
	if got, ok := srv.registry.get(id); ok {
		t.Errorf("get() after a probed miss = %v, want the dead entry evicted", got)
	}
}

// TestServerLiveSession covers the ordinary resolve paths: a live session is handed
// back, and an id that was never registered misses. A miss must be indistinguishable
// between "never existed" and "shut down" — that is what keeps the route from
// answering as a liveness oracle.
func TestServerLiveSession(t *testing.T) {
	t.Parallel()

	t.Run("live session resolves", func(t *testing.T) {
		t.Parallel()
		srv := testServer(t)
		id := mustUUID(t)
		sess := newDoneFakeSession(1)
		srv.register(id, sess)

		got, ok := srv.liveSession(id)
		if !ok {
			t.Fatal("liveSession() missed a live session")
		}
		if got != LiveSession(sess) {
			t.Errorf("liveSession() = %v, want %v", got, sess)
		}
	})

	t.Run("unknown id misses", func(t *testing.T) {
		t.Parallel()
		srv := testServer(t)
		if got, ok := srv.liveSession(mustUUID(t)); ok {
			t.Fatalf("liveSession() = %v, true for an unregistered id", got)
		}
	})
}

// TestServerRegisterToleratesSessionsWithoutDone pins the optionality of SessionDone.
// LiveSession deliberately does not carry Done, so tui, acp and consumer fakes that
// implement only the five required methods must keep working exactly as they do today:
// registered, resolvable, and never evicted behind the caller's back.
func TestServerRegisterToleratesSessionsWithoutDone(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	id := mustUUID(t)
	sess := fakeLiveSession{id: 1}
	if _, isDone := LiveSession(sess).(SessionDone); isDone {
		t.Fatal("fakeLiveSession satisfies SessionDone; this test needs a session that does not")
	}

	srv.register(id, sess)

	// Repeated resolution must keep hitting: with no Done channel there is nothing to
	// probe, so the probe must not treat "cannot ask" as "dead" (fail-open is correct
	// here — the alternative evicts every session a consumer registers).
	for i := 0; i < 3; i++ {
		got, ok := srv.liveSession(id)
		if !ok {
			t.Fatalf("liveSession() call %d missed a session that never reports death", i)
		}
		if got != LiveSession(sess) {
			t.Fatalf("liveSession() call %d = %v, want %v", i, got, sess)
		}
	}
	assertRegistered(t, srv.registry, id, sess)
}

// TestServerWatcherDeclinesToEvictAReRegisteredSession is the identity guard seen from
// the watcher. A restore can re-register a fresh live session under the same sid while
// the old session is still tearing down; the old session's watcher then fires with a
// stale observation. It must remove nothing. The window is observed rather than sampled
// once: with an unguarded delete the watcher wins this race essentially always, so the
// replacement disappears well inside it.
func TestServerWatcherDeclinesToEvictAReRegisteredSession(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	id := mustUUID(t)
	dying := newDoneFakeSession(1)
	replacement := newDoneFakeSession(2)

	srv.register(id, dying)
	// The restore re-registers under the same sid, replacing the map entry.
	srv.register(id, replacement)

	dying.shutdown()

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		got, ok := srv.registry.get(id)
		if !ok {
			t.Fatal("the dying session's watcher evicted the sid; a restore's live replacement was removed on a corpse's behalf")
		}
		if got != LiveSession(replacement) {
			t.Fatalf("get() = %v, want the replacement %v", got, replacement)
		}
		time.Sleep(time.Millisecond)
	}

	// The replacement's own watcher must still work: the guard rejects stale
	// observations, not every observation.
	replacement.shutdown()
	waitAbsent(t, srv.registry, id)
}
