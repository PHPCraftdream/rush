package session

import (
	"os"

	"github.com/PHPCraftdream/rush/internal/filelock"
)

// Bridge to the leaf filelock package (moved 2026-10): keeps the session
// host-lock module and its tests calling the same unqualified names.

type FileLock = filelock.FileLock

type ErrLockContended = filelock.ErrLockContended

func TryAcquireFileLock(lockPath string) (*filelock.FileLock, error) {
	return filelock.TryAcquireFileLock(lockPath)
}

func openLockFile(lockPath string) (*os.File, error) { return filelock.OpenLockFile(lockPath) }

func classifyAndLock(f *os.File, lockPath string) (*filelock.FileLock, error) {
	return filelock.AcquireFromFile(f, lockPath)
}

func isContentionError(err error) bool { return filelock.IsContentionError(err) }
