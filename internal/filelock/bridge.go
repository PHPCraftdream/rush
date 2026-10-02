package filelock

import "os"

// OpenLockFile exposes openLockFile to callers that need the seam between
// opening the lock file and taking the lock (session host-lock registration).
func OpenLockFile(lockPath string) (*os.File, error) { return openLockFile(lockPath) }

// AcquireFromFile takes the exclusive non-blocking lock on an already-open f,
// applying TryAcquireFileLock's error classification.
func AcquireFromFile(f *os.File, lockPath string) (*FileLock, error) {
	return classifyAndLock(f, lockPath)
}

// IsContentionError reports whether err reports genuine lock contention.
func IsContentionError(err error) bool { return isContentionError(err) }

// File returns the underlying lock file handle.
func (l *FileLock) File() *os.File { return l.f }

// WrapLockedFile wraps an already-open, already-locked file handle in a
// FileLock WITHOUT taking the lock again: the caller has already won it
// (see session.ProbeHostLock, which locks read-only itself before returning).
func WrapLockedFile(f *os.File, lockPath string) *FileLock {
	return &FileLock{Path: lockPath, f: f}
}
