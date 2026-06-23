package persistence

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
)

// fakeEngine is a stand-in engineHandle so the SessionEngine lifecycle (especially Close
// ordering) can be exercised without starting an embedded NATS server.
type fakeEngine struct {
	closeErr error
	closed   bool
}

func (f *fakeEngine) JetStream() nats.JetStreamContext { return nil }

func (f *fakeEngine) Close() error {
	f.closed = true
	return f.closeErr
}

// fakeLock records release so tests can assert the lock is always freed.
type fakeLock struct {
	unlockErr error
	unlocked  bool
}

func (f *fakeLock) Unlock() error {
	f.unlocked = true
	return f.unlockErr
}

func swapAcquireLock(t *testing.T, fn func(string) (sessionLock, error)) {
	t.Helper()
	prev := acquireLock
	acquireLock = fn
	t.Cleanup(func() { acquireLock = prev })
}

func swapOpenEngine(t *testing.T, fn func(EngineOptions) (engineHandle, error)) {
	t.Helper()
	prev := openEngine
	openEngine = fn
	t.Cleanup(func() { openEngine = prev })
}

// TestSessionEngineLockedBeforeStartup proves a second open of a locked session directory
// returns *SessionLockedError before any engine (server) construction is attempted.
func TestSessionEngineLockedBeforeStartup(t *testing.T) {
	dir := t.TempDir()
	swapOpenEngine(t, func(EngineOptions) (engineHandle, error) {
		t.Fatalf("openEngine called despite a held session lock")
		return nil, nil
	})

	held, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("acquireLock(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = held.Unlock() })

	_, err = openSessionEngineAt(dir)
	var locked *SessionLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("openSessionEngineAt() error = %T %v, want *SessionLockedError", err, err)
	}
}

// TestSessionLockReleaseAllowsReacquire proves releasing a lock lets it be retaken — the
// property a session reopen after a clean close depends on.
func TestSessionLockReleaseAllowsReacquire(t *testing.T) {
	dir := t.TempDir()

	first, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("first acquireLock: %v", err)
	}
	if err := first.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	second, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("second acquireLock after release: %v", err)
	}
	if err := second.Unlock(); err != nil {
		t.Fatalf("second Unlock: %v", err)
	}
}

// TestSessionEngineCloseReleasesLockOnDrainError proves Close frees the lock even when the
// engine drain fails, and surfaces the drain error.
func TestSessionEngineCloseReleasesLockOnDrainError(t *testing.T) {
	drainErr := errors.New("drain failed")
	lock := &fakeLock{}
	engine := &fakeEngine{closeErr: drainErr}
	se := &SessionEngine{engine: engine, lock: lock}

	err := se.Close()
	if !errors.Is(err, drainErr) {
		t.Fatalf("Close() = %v, want drain error %v", err, drainErr)
	}
	if !engine.closed {
		t.Error("engine.Close was not called")
	}
	if !lock.unlocked {
		t.Error("lock was not released after a drain error")
	}
}

// TestSessionEngineUnsupportedPlatform proves an unsupported locking platform surfaces a
// typed error rather than opening an unguarded engine.
func TestSessionEngineUnsupportedPlatform(t *testing.T) {
	swapAcquireLock(t, func(string) (sessionLock, error) {
		return nil, errSessionLockUnsupported
	})
	swapOpenEngine(t, func(EngineOptions) (engineHandle, error) {
		t.Fatalf("openEngine called on an unsupported locking platform")
		return nil, nil
	})

	if _, err := openSessionEngineAt(t.TempDir()); !errors.Is(err, errSessionLockUnsupported) {
		t.Fatalf("openSessionEngineAt() error = %v, want errSessionLockUnsupported", err)
	}
}

// TestSessionEngineTwoDistinctIDsLockIndependently proves two different session
// directories lock without contending with each other.
func TestSessionEngineTwoDistinctIDsLockIndependently(t *testing.T) {
	a, err := acquireLock(t.TempDir())
	if err != nil {
		t.Fatalf("acquireLock(a): %v", err)
	}
	t.Cleanup(func() { _ = a.Unlock() })

	b, err := acquireLock(t.TempDir())
	if err != nil {
		t.Fatalf("acquireLock(b): %v", err)
	}
	t.Cleanup(func() { _ = b.Unlock() })
}

// TestSessionEngineCloseReleasesLockOnSuccess proves the ordinary close path frees the lock
// and reports no error.
func TestSessionEngineCloseReleasesLockOnSuccess(t *testing.T) {
	lock := &fakeLock{}
	engine := &fakeEngine{}
	se := &SessionEngine{engine: engine, lock: lock}

	if err := se.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if !lock.unlocked {
		t.Error("lock was not released on a clean close")
	}
}
