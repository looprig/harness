package foreignloop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// LockError is the typed cause for an I/O failure while acquiring the per-(sid,cwd)
// foreign liveness lock (directory create, exclusive create, write, or close). Busy
// contention is reported separately as *ForeignSessionBusyError, never as a LockError.
type LockError struct {
	Op    string // "mkdir" | "create" | "write" | "close"
	Path  string
	Cause error
}

func (e *LockError) Error() string {
	return "foreignloop: lock " + e.Op + " " + e.Path + ": " + e.Cause.Error()
}
func (e *LockError) Unwrap() error { return e.Cause }

// foreignLock is a held per-(sid,cwd) liveness lock: a lockfile recording the holder's
// pid. It serializes foreign spawns so two looprig processes never drive the same
// Claude session in the same working directory and corrupt its transcript.
type foreignLock struct {
	path string
}

// foreignLockPath is the DETERMINISTIC per-(sid,cwd) lockfile path. The cwd is cleaned
// and hashed (first 12 hex of sha256) so the path is stable, filesystem-safe, and lives
// under a dedicated tempdir subtree that never collides with Claude's own session
// files. Identical (sid,cwd) inputs always map to the same path; a different cwd or sid
// maps elsewhere.
func foreignLockPath(sid, cwd string) string {
	return filepath.Join(os.TempDir(), "looprig-foreign", hash12(cwd)+"-"+sid+".lock")
}

// hash12 returns the first 12 hex characters of sha256(filepath.Clean(cwd)) — a stable,
// path-safe short digest keying the lock on the working directory.
func hash12(cwd string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
	return hex.EncodeToString(sum[:])[:12]
}

// acquireForeignLock takes the exclusive (sid,cwd) liveness lock. On success it returns
// a held *foreignLock recording our pid. If a LIVE process already holds the lock it
// fails with *ForeignSessionBusyError. A STALE lock (recorded pid is dead, malformed,
// or empty) is reclaimed and the exclusive create is retried ONCE; if that retry still
// loses the race it is treated as busy (fail-secure: never loop, never steal a live
// holder's lock).
func acquireForeignLock(sid, cwd string) (*foreignLock, error) {
	path := foreignLockPath(sid, cwd)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, &LockError{Op: "mkdir", Path: dir, Cause: err}
	}
	lk, err := tryCreateLock(path)
	if err == nil {
		return lk, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err // already a typed *LockError from tryCreateLock
	}
	// The lock exists: reclaim it only if the recorded holder is provably gone.
	if pid := readLockPID(path); pid > 0 && processAlive(pid) {
		return nil, &ForeignSessionBusyError{SID: sid, Cwd: cwd, PID: pid}
	}
	_ = os.Remove(path) // stale: drop it and retry the exclusive create exactly once.
	lk, err = tryCreateLock(path)
	if err == nil {
		return lk, nil
	}
	if errors.Is(err, os.ErrExist) {
		// Lost the reclaim race to another process; surface it as busy with its pid.
		return nil, &ForeignSessionBusyError{SID: sid, Cwd: cwd, PID: readLockPID(path)}
	}
	return nil, err // typed *LockError
}

// tryCreateLock atomically creates the lockfile (O_CREATE|O_EXCL) and records our pid.
// It returns os.ErrExist VERBATIM when another lock already holds the path (so the
// caller can errors.Is it), or a typed *LockError for any other I/O failure.
func tryCreateLock(path string) (*foreignLock, error) {
	// #nosec G304 -- path is an app-controlled, deterministic lock path under TempDir,
	// derived from a sha256 hash of the cleaned cwd; it is never attacker-influenced.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, err
		}
		return nil, &LockError{Op: "create", Path: path, Cause: err}
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, &LockError{Op: "write", Path: path, Cause: err}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, &LockError{Op: "close", Path: path, Cause: err}
	}
	return &foreignLock{path: path}, nil
}

// readLockPID reads the pid recorded in an existing lockfile. A missing, empty, or
// malformed lockfile yields 0, which the caller treats as a reclaimable stale lock.
func readLockPID(path string) int {
	// #nosec G304 -- deterministic, app-owned lock path (see tryCreateLock).
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

// release removes the lockfile. It ignores a missing file and is safe to call more than
// once, so callers can defer it unconditionally.
func (l *foreignLock) release() {
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Best-effort: a stuck lockfile is reclaimed by the next acquire's liveness check.
		return
	}
}

// processAlive reports whether pid names a live process on this Unix host. A
// non-positive pid is never alive. syscall.Kill(pid, 0) sends no signal but runs the
// kernel's existence/permission check: nil or EPERM means the process exists (EPERM =
// it exists but is owned by another user); ESRCH means there is no such process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
