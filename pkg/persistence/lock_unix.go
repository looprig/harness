//go:build unix

package persistence

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// flockSessionLock holds an exclusive flock on an open session.lock descriptor. The lock is
// associated with the open file description, so a second open of the same file — even in
// this process — is denied, which is exactly the single-engine-per-session guarantee.
type flockSessionLock struct {
	file *os.File
}

// acquireSessionLock takes a non-blocking exclusive flock on <dir>/session.lock. A lock
// already held elsewhere returns *SessionLockedError; any other failure returns a typed
// *SessionStoreError.
func acquireSessionLock(dir string) (sessionLock, error) {
	path := filepath.Join(dir, sessionLockFileName)
	// #nosec G304 -- path is the confined lock file under a validated session directory.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, sessionLockFilePerm)
	if err != nil {
		return nil, &SessionStoreError{Operation: SessionStoreOpen, Path: path, Cause: err}
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &SessionLockedError{Path: path}
		}
		return nil, &SessionStoreError{Operation: SessionStoreOpen, Path: path, Cause: err}
	}
	return &flockSessionLock{file: file}, nil
}

// Unlock releases the flock and closes the descriptor. It is idempotent.
func (l *flockSessionLock) Unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
