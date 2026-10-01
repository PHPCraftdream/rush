// The one lock-fact reader for the activity classifier (plan R-ACT, architect
// decision 10): an enum state derived from the on-disk record, not a pair of
// independent flags. Release truncates the lock file and removes the .pid
// sidecar (clearHolderMetadata), so "reads back empty" IS a released lock
// whatever the mtime -- the previous flag pair classified a fresh empty file
// as in turn for the 20s heartbeat window after every release.
// InspectSessionLock's Live field remains for the server/recovery readers;
// the cmd readers use this fact through the classifier.

package session

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SessionLockStem is the lock-FILE stem of a real session id: ids sanitise
// into file names lossily, so the stem maps to real ids only forward
// (R7C-2/R8C-8). The batched activity reader returns the reverse map for
// the requested ids so locks/reap/prune never rebuild their own.
func SessionLockStem(sessionID string) string {
	return sanitiseSessionID(sessionID)
}

// InspectSessionLockFact reads one session's lock state without acquiring
// it. No mtime is consulted: a held lock always carries a fresh .pid
// sidecar (writePIDSidecar at acquire), so the record's content decides.
func InspectSessionLockFact(dataDir, sessionID string) LockFact {
	path := SessionLockPath(dataDir, sessionID)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return LockFact{Kind: LockAbsent}
		}
		return LockFact{Kind: LockUnknown}
	}
	pid, ok := readLockRecord(path)
	if !ok {
		// Unreadable record: on Windows a held lock's primary file cannot
		// be read at all while the holder lives, so this shape is
		// "possibly held", never "released" (fail-open).
		return LockFact{Kind: LockUnknown}
	}
	if pid <= 0 {
		return LockFact{Kind: LockReleased}
	}
	if IsProcessAlive(pid) {
		return LockFact{Kind: LockHeld, PID: pid}
	}
	return LockFact{Kind: LockDead, PID: pid}
}

// readLockRecord parses the holder PID out of a lock's record: the never-
// locked .pid sidecar first, then the (Windows: mandatorily locked)
// primary file. ok is false only when NEITHER file could be read -- empty
// but readable records parse to 0 (the released shape).
func readLockRecord(path string) (pid int, ok bool) {
	if bts, err := os.ReadFile(pidSidecarPath(path)); err == nil {
		return parseLockRecord(string(bts)), true
	}
	bts, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	return parseLockRecord(string(bts)), true
}

func parseLockRecord(content string) int {
	line := content
	if i := strings.IndexByte(content, '\n'); i >= 0 {
		line = content[:i]
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(line))
	return pid
}

// writeFile is the test helper for hand-shaped lock records.
func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func itoa2(n int) string {
	return strconv.Itoa(n)
}
