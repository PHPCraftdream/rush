// Shared host-lock probe for the phase-4 durable async core. A pure,
// DB-independent primitive: open the lock file read-only, never create
// it, take a SHARED non-blocking lock, and reduce the outcome to a
// three-way verdict. internal/session's ProbeHostLockShared delegates
// here (internal/db will later need the same probe and cannot import
// internal/session).
package filelock

import (
	"errors"
	"fmt"
	"os"
)

// LockStatus is the three-way verdict a probe reduces every OS outcome
// to: "unknown" is a real, distinct outcome, not an error case folded
// into "alive" or "dead".
type LockStatus int

const (
	// LockStatusUnknown means the probe could not determine liveness
	// (permission error, IO error, a filesystem without lock support,
	// ...). Callers MUST NOT treat a host with this status as reapable.
	LockStatusUnknown LockStatus = iota
	// LockStatusAlive means another process currently holds the lock
	// exclusively (EWOULDBLOCK/EAGAIN on POSIX,
	// ERROR_LOCK_VIOLATION/ERROR_SHARING_VIOLATION on Windows --
	// isLockContentionError).
	LockStatusAlive
	// LockStatusDead means either the lock file does not exist (ENOENT)
	// or this probe itself just won the shared lock, which is
	// kernel-attested proof no exclusive holder exists right now.
	LockStatusDead
)

func (s LockStatus) String() string {
	switch s {
	case LockStatusAlive:
		return "alive"
	case LockStatusDead:
		return "dead"
	default:
		return "unknown"
	}
}

// ProbeShared answers "is the process behind lockPath alive?" via the
// SHARED, non-acquiring probe: unlike TryAcquireFileLock it never takes
// the EXCLUSIVE lock itself, so it can never win the lock a live holder
// is about to hold, nor make a live host look dead. Winning the SHARED
// lock proves no exclusive holder exists (dead); contention means alive;
// anything else is unknown. The probe does not create the file, and
// releases its own shared lock before returning in every case.
func ProbeShared(lockPath string) (LockStatus, error) {
	// Open read-only only: the probe is a pure reader of lock state.
	f, err := os.OpenFile(lockPath, os.O_RDONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LockStatusDead, nil
		}
		return LockStatusUnknown, fmt.Errorf("file lock probe (shared): open %s: %w", lockPath, err)
	}
	defer f.Close()
	// Locking a directory succeeds on some platforms with meaningless
	// semantics, so refuse it explicitly: a directory at the lock path
	// is never a live host.
	if info, statErr := f.Stat(); statErr == nil && info.IsDir() {
		return LockStatusUnknown, fmt.Errorf("file lock probe (shared): %s is a directory", lockPath)
	}
	if err := tryLockFileShared(f); err != nil {
		if isLockContentionError(err) {
			return LockStatusAlive, nil
		}
		return LockStatusUnknown, fmt.Errorf("file lock probe (shared): lock %s: %w", lockPath, err)
	}
	// Won the shared lock: release it immediately -- this probe never
	// holds anything past its own return, and a genuine holder is never
	// disturbed.
	if err := unlockFile(f); err != nil {
		return LockStatusUnknown, fmt.Errorf("file lock probe (shared): unlock %s: %w", lockPath, err)
	}
	return LockStatusDead, nil
}
