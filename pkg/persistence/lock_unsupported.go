//go:build !unix

package persistence

// acquireSessionLock fails closed on platforms without flock support: rather than open a
// session without an exclusive guard, it returns errSessionLockUnsupported so the caller
// never runs two engines over one StoreDir.
func acquireSessionLock(dir string) (sessionLock, error) {
	return nil, errSessionLockUnsupported
}
