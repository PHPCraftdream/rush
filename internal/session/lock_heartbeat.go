package session

import (
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Verify contention through a read-only handle when the writable open is denied.
func busyIfPermissionDeniedOpenHasActiveLock(path string, openErr error) (*SessionLockBusyError, bool) {
	if !os.IsPermission(openErr) {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if err := tryLockFileShared(f); err == nil {
		_ = unlockFile(f)
		return nil, false
	} else if isLockContentionError(err) {
		return &SessionLockBusyError{Path: path, HolderPID: readLockHolderPID(path)}, true
	}
	return nil, false
}

// touchLockFileFn is a test-only indirection seam over touchLockFile. It
// lets a test in this package make the heartbeat's per-tick touch block on
// command, to deterministically exercise Release's bounded handoff to the
// heartbeat finalizer. The initial touch in
// acquireSessionLockFileWithOptions deliberately calls touchLockFile
// directly (NOT through this seam) because it runs before the heartbeat
// starts, so an override here can never stall acquire. The production
// default is the real touchLockFile; only the heartbeat tick consults it.
var touchLockFileFn = touchLockFile

// heartbeat touches the lock file every lockHeartbeatInterval, but ONLY
// if RecordActivity was called on the owning SessionLock at least once
// since the previous tick. Stops when done is closed.
//
// This is diagnostics only ("something might be wrong if this goes
// stale") — see the package doc on SessionLock and acquireSessionLockFile.
// It must never be treated as the source of truth for whether the lock
// may be reclaimed; only actually winning the OS lock decides that.
//
// Gating on activity is itself a deliberate design requirement, not an
// optimization: a session that is fully wedged (stuck goroutine, no
// forward progress) must NOT keep presenting a live-looking mtime
// forever — that was a diagnostic false positive. See RecordActivity.
// Skipping a tick because there was no activity is a normal, expected
// outcome, not an error condition, so it does not touch failCount or log
// anything.
//
// A timestamp-touch failure is logged, but it does not stop the heartbeat
// loop or mark us as dead. A transient failure while we hold the OS lock
// must never let another process conclude it can steal the session. Logging
// is throttled so a persistently failing filesystem does not spam each tick.
func heartbeat(f *os.File, path string, done <-chan struct{}, active *atomic.Bool, interval time.Duration, exited chan<- struct{}, closeState *atomic.Int32, handoffDone chan struct{}, exitSeam func()) {
	// Registered as the FIRST statement so it runs LAST (defers are LIFO):
	// it executes only after the loop below returns — hence after any
	// in-flight touchLockFile completed — so the handle is never closed
	// mid-use.
	//
	// The CAS races the identical one in Release's handoff branch: we try
	// to claim closeStateReleaseCloses (i.e. "Release closes"). If that
	// succeeds, Release either already took, or will take, the normal
	// unlock+close path itself. If it fails, Release already claimed
	// closeStateHeartbeatCloses first (its bounded wait timed out before we
	// got here) — WE are the sole closer, and it is always safe: we only
	// reach this point after the loop below returned, i.e. after any
	// in-flight touch completed. Exactly one CAS on one variable — not an
	// independent Load/Store pair — makes this decision exactly-once; see
	// the closeState field doc and Release's handoff branch.
	defer func() {
		if !closeState.CompareAndSwap(closeStateUndecided, closeStateReleaseCloses) {
			if err := unlockFile(f); err != nil {
				slog.Warn("session lock: heartbeat handoff unlock failed",
					"path", path, "err", err)
			}
			if err := f.Close(); err != nil {
				slog.Warn("session lock: heartbeat handoff close failed",
					"path", path, "err", err)
			}
			// Release registered handoffDone before winning its CAS; we
			// are the loser, so we close it — only this lock's own channel
			// (task #1031).
			finishPendingHandoff(path, handoffDone)
		}
		if exitSeam != nil {
			exitSeam()
		}
		close(exited)
	}()
	t := time.NewTicker(interval)
	defer t.Stop()
	var failCount atomic.Int64
	for {
		select {
		case <-done:
			return
		case <-t.C:
			// Consume ("swap to false") the activity recorded since the
			// last tick. This is the sole gate: no activity this window
			// means no Chtimes this tick, and the window resets either
			// way for the next tick.
			if !active.Swap(false) {
				continue
			}
			now := time.Now()
			if err := touchLockFileFn(f, now); err != nil {
				n := failCount.Add(1)
				// Log the 1st, 2nd, 4th, 8th, 16th... failure so a
				// persistent failure doesn't flood the log but is still
				// visible quickly and periodically.
				if n == 1 || n&(n-1) == 0 {
					slog.Warn("session lock: heartbeat failed to touch lock file mtime",
						"path", path, "err", err, "consecutive_failures", n)
				}
			} else {
				failCount.Store(0)
			}
		}
	}
}
