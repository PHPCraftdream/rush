//go:build windows

package session

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFileBlocking takes an exclusive lock that BLOCKS until acquired.
// Same as tryLockFile but without LOCKFILE_FAIL_IMMEDIATELY — the call
// waits for the holder to release.
func lockFileBlocking(f *os.File) error {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	var overlapped windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		flags,
		0,
		^uint32(0),
		^uint32(0),
		&overlapped,
	)
}

// unlinkBeforeUnlock is false on Windows: an open file cannot be deleted
// (Go opens without FILE_SHARE_DELETE), so the lock is closed first and a
// concurrent registrant's open handle turns the remove into a sharing
// violation instead. See RemoveDeadHostFile.
const unlinkBeforeUnlock = false
