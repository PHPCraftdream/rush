//go:build windows

package session

// unlinkBeforeUnlock is false on Windows: an open file cannot be deleted
// (Go opens without FILE_SHARE_DELETE), so the lock is closed first and a
// concurrent registrant's open handle turns the remove into a sharing
// violation instead. See RemoveDeadHostFile.
const unlinkBeforeUnlock = false
