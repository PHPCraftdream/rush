//go:build !windows

package session

import (
	"os"
	"syscall"
)

// lockFileBlocking takes an exclusive lock that BLOCKS until acquired.
// Counterpart to tryLockFile (which is non-blocking via LOCK_NB).
func lockFileBlocking(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlinkBeforeUnlock: a dead host's lock file is unlinked while the lock is
// still held (flock is keyed off the inode, so the held fd stays valid), so
// no registrant can lock and verify the path in the gap between unlock and
// unlink. See RemoveDeadHostFile.
const unlinkBeforeUnlock = true
