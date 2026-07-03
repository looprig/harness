package persistence

import (
	"errors"
	"path/filepath"

	"github.com/looprig/harness/pkg/uuid"
	"github.com/nats-io/nats.go"
)

const (
	// sessionLockFileName is the per-session advisory lock file. Holding it marks the
	// session directory as in use by one live engine (this or another process).
	sessionLockFileName = "session.lock"
	// sessionNATSDirName is the StoreDir subdirectory holding the session's embedded
	// JetStream state, kept separate from the lock and manifest files.
	sessionNATSDirName = "nats"
	// sessionLockFilePerm keeps the lock file owner-only.
	sessionLockFilePerm = 0o600
)

// errSessionLockUnsupported is returned by acquireSessionLock on platforms without the
// build-tagged flock implementation. The session never opens unguarded.
var errSessionLockUnsupported = errors.New("persistence: session file locking is unsupported on this platform")

// SessionLockedError reports that a session directory is already locked by a live engine
// (in this or another process). The session is in use; the caller must not open it.
type SessionLockedError struct {
	Path string
}

func (e *SessionLockedError) Error() string {
	if e.Path != "" {
		return "persistence: session already locked: " + e.Path
	}
	return "persistence: session already locked"
}

// sessionLock is a process-exclusive advisory lock on a session directory. The Unix build
// backs it with flock; unsupported platforms fail closed at acquisition time.
type sessionLock interface {
	Unlock() error
}

// engineHandle is the narrow subset of *Engine the session lifecycle depends on, so Close
// ordering can be unit-tested without starting an embedded server.
type engineHandle interface {
	JetStream() nats.JetStreamContext
	Close() error
}

// acquireLock is the lock-acquisition seam. Production points it at the platform flock
// implementation; tests swap it to exercise contention and unsupported-platform paths.
var acquireLock = acquireSessionLock

// openEngine is the engine-construction seam. Production opens a real embedded engine;
// tests swap it to avoid starting NATS.
var openEngine = func(opts EngineOptions) (engineHandle, error) { return Open(opts) }

// SessionEngine is one embedded engine bound to a single session directory and guarded by a
// process-exclusive lock. It is the per-session unit ../swe opens on new/resume and closes
// on teardown; the Engine itself stays session-agnostic.
type SessionEngine struct {
	engine engineHandle
	lock   sessionLock
}

// OpenSessionEngine creates (or validates) the session directory for id, takes its
// exclusive lock, and opens an embedded engine whose StoreDir lives beneath that directory.
// A live session returns *SessionLockedError before any server starts.
func (r *SessionStoreRoot) OpenSessionEngine(id uuid.UUID) (*SessionEngine, error) {
	dir, err := r.CreateSessionDir(id)
	if err != nil {
		return nil, err
	}
	return openSessionEngineAt(dir)
}

// openSessionEngineAt acquires the directory lock first, then opens the engine. It is the
// testable seam beneath OpenSessionEngine: the lock is taken before — and released on every
// failure of — engine construction.
func openSessionEngineAt(dir string) (*SessionEngine, error) {
	lock, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}

	engine, err := openEngine(EngineOptions{DataDir: filepath.Join(dir, sessionNATSDirName)})
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	return &SessionEngine{engine: engine, lock: lock}, nil
}

// JetStream returns the session engine's bound JetStreamContext, valid until Close.
func (e *SessionEngine) JetStream() nats.JetStreamContext {
	if e == nil || e.engine == nil {
		return nil
	}
	return e.engine.JetStream()
}

// Close shuts the embedded engine down and always releases the session lock afterwards,
// even when the engine drain fails. The drain error takes precedence in the return.
func (e *SessionEngine) Close() error {
	if e == nil {
		return nil
	}
	var engineErr error
	if e.engine != nil {
		engineErr = e.engine.Close()
		e.engine = nil
	}
	var lockErr error
	if e.lock != nil {
		lockErr = e.lock.Unlock()
		e.lock = nil
	}
	if engineErr != nil {
		return engineErr
	}
	return lockErr
}
