// Helpers for the SessionLock tests in lock_test.go (same package), extracted
// with the Windows TempDir-cleanup flake fix (#1142 SD-C follow-up): Release's
// bounded handoff can leave the final file-handle Close to the heartbeat's
// finalizer, which runs "a few syscalls later" — a t.TempDir RemoveAll that
// races it fails on Windows with "file in use". Awaiting heartbeatExited
// closes that race, and wrapping the release as cleanup covers failure paths.

package session

import (
	"testing"
	"time"
)

// awaitHeartbeatExit waits until the lock's heartbeat goroutine has fully
// exited, i.e. any handed-off unlock+Close of the file handle has completed.
// Release returns as soon as its bounded handoff window (50ms) expires,
// leaving the final unlock+Close to the heartbeat's finalizer "a few syscalls
// later"; on Windows a t.TempDir cleanup that races that close fails with
// "The process cannot access the file because it is being used by another
// process" — the observed TestHeartbeatTouchesFile flake. Waiting on
// heartbeatExited (the finalizer closes it strictly after its unlock+Close)
// closes that race. heartbeatExited is created for every lock, so a nil
// check is only a nil-lock guard.
func awaitHeartbeatExit(t *testing.T, lk *SessionLock) {
	t.Helper()
	if lk == nil || lk.heartbeatExited == nil {
		return
	}
	select {
	case <-lk.heartbeatExited:
	case <-time.After(10 * time.Second):
		t.Fatal("heartbeat goroutine did not exit after Release")
	}
}

// releaseLockFully registers the lock's release as test cleanup AND makes
// that release wait for the heartbeat's handle close (see
// awaitHeartbeatExit), so t.TempDir's RemoveAll — whose own cleanup was
// registered earlier and therefore runs after this one — never races an
// in-flight Close on Windows. The explicit Release calls in the test bodies
// stay: they assert release success; this cleanup only covers failure paths
// (an assert stops neither the test nor the deferred release, but a
// require.Fatal or panic would otherwise skip both) and is a no-op then —
// Release is sync.Once-guarded.
func releaseLockFully(t *testing.T, lk *SessionLock) {
	t.Helper()
	t.Cleanup(func() {
		_ = lk.Release()
		awaitHeartbeatExit(t, lk)
	})
}
