//go:build !windows

package session

// unlinkBeforeUnlock: a dead host's lock file is unlinked while the lock is
// still held (flock is keyed off the inode, so the held fd stays valid), so
// no registrant can lock and verify the path in the gap between unlock and
// unlink. See RemoveDeadHostFile.
const unlinkBeforeUnlock = true
