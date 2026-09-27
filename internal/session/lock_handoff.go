package session

import (
	"sync"
	"time"
)

// closeState values coordinate, via exactly one atomic CompareAndSwap,
// which of Release or the heartbeat finalizer performs the unlockFile+Close
// when a heartbeat touch is still in flight at Release's bounded handoff
// wait. Both sides attempt a CAS away from closeStateUndecided toward their
// own preferred outcome (Release toward closeStateHeartbeatCloses, the
// heartbeat finalizer toward closeStateReleaseCloses); whichever CAS wins
// is authoritative, and the loser's later CAS necessarily fails and it
// acts accordingly. See SessionLock.closeState and Release's handoff
// branch for the full argument.
const (
	closeStateUndecided int32 = iota
	closeStateHeartbeatCloses
	closeStateReleaseCloses
)

// pendingHandoffWaitBound bounds how long acquireSessionLockFileWithOptions
// waits, in waitForPendingHandoff, for another SessionLock IN THIS SAME
// PROCESS to finish a pending handoff unlock of the exact same path before
// attempting the real OS lock. Waiting is the only way such an acquire can
// succeed at all: the outgoing lock's handle still holds the OS lock until
// the heartbeat finalizer's unlockFile actually runs, so attempting the OS
// lock any earlier would just reproduce the false "already in use" this
// exists to fix (task #1031). 10s is chosen to comfortably exceed any
// realistic stuck-touch drain time (the touch itself is typically
// microseconds; this bound only matters if the touch is genuinely hung,
// e.g. a wedged AV/SMB filesystem) while still being bounded — if it
// elapses, the caller falls through to the normal attempt and gets the
// normal SessionLockBusyError, exactly as if this mechanism didn't exist.
const pendingHandoffWaitBound = 10 * time.Second

// pendingHandoffUnlocks is a process-local registry, keyed by lock file
// path, of in-progress handoff unlocks: Release registers a channel here
// before returning on the handoff path (its closeState CAS succeeded), and
// the heartbeat finalizer closes and removes it once it has actually
// unlocked and closed the handle. acquireSessionLockFileWithOptions
// consults it (via waitForPendingHandoff) so a same-process re-acquire of
// the exact same path waits for the real unlock instead of seeing its own
// outgoing lock as a foreign busy holder.
//
// Deliberately process-local and path-keyed, not per-SessionLock: its
// entire purpose is to let ONE SessionLock instance's re-acquire (a new
// acquireSessionLockFileWithOptions call, which has no handle to the old
// instance) find out about ANOTHER, already-Released instance's pending
// close for the same path.
var pendingHandoffUnlocks sync.Map // path (string) -> chan struct{}

// waitForPendingHandoff blocks, bounded by pendingHandoffWaitBound, until
// any handoff unlock currently in progress for path completes. A no-op if
// no handoff is pending, so it never delays the common case (no prior
// Release, or a Release that never needed to hand off) at all.
func waitForPendingHandoff(path string) {
	v, ok := pendingHandoffUnlocks.Load(path)
	if !ok {
		return
	}
	ch, ok := v.(chan struct{})
	if !ok {
		return
	}
	select {
	case <-ch:
	case <-time.After(pendingHandoffWaitBound):
	}
}

// finishPendingHandoff removes ch from the registry (only if it is still
// the entry for path — a newer lock's entry is never touched) and closes it,
// waking waitForPendingHandoff callers. ch must be the caller's OWN lock's
// handoffDone, and the caller must already have unlocked+closed the handle:
// only the closeState CAS loser calls this, so ch is closed exactly once.
func finishPendingHandoff(path string, ch chan struct{}) {
	pendingHandoffUnlocks.CompareAndDelete(path, ch)
	close(ch)
}

// withHeartbeatExitSeam sets a test-only hook invoked by the heartbeat's
// finalizer after the closeState decision (and any resulting unlock+close)
// but strictly before close(heartbeatExited). It lets a test pause the
// heartbeat goroutine at exactly that point — without needing a real hung
// touch — to deterministically observe or control what happens between the
// closer decision and the heartbeat signalling it has exited. Per-lock
// (an unexported LockOption), not a package global, for the same reason
// WithHeartbeatInterval is: no cross-test coordination needed. Nil in
// every production path (no-op).
func withHeartbeatExitSeam(fn func()) LockOption {
	return func(lk *SessionLock) {
		lk.heartbeatExitSeam = fn
	}
}
