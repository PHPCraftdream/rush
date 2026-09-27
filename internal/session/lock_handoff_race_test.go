package session

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReleaseHandoffNeverLeavesLockUnclosed proves defect #1017 is fixed:
// the old Load/Store *atomic.Bool pair between Release and the heartbeat
// finalizer admitted an interleaving where BOTH sides skipped closing the
// handle. Deterministic, no real hung touch needed — the heartbeat exits
// its loop immediately on close(stop) (no RecordActivity/tick involved),
// reaches its finalizer's closer decision, and then blocks in the
// heartbeatExitSeam test hook (installed via withHeartbeatExitSeam) BEFORE
// closing heartbeatExited. That forces Release's bounded wait to time out
// and race its own CAS against the heartbeat's already-made decision,
// exactly the way a genuinely hung touch would.
//
// Revert check: in Release's timeout branch (internal/session/lock.go),
// change `if l.closeState.CompareAndSwap(closeStateUndecided,
// closeStateHeartbeatCloses) {` to `if true {` (always take the handoff
// branch regardless of the CAS outcome) — reproducing the old code's
// unconditional Store(true). Combined with the heartbeat having already
// (correctly) decided closeStateReleaseCloses in this interleaving, nobody
// closes the file, and the require.Eventually below times out and fails.
func TestReleaseHandoffNeverLeavesLockUnclosed(t *testing.T) {
	dir := t.TempDir()

	seamEntered := make(chan struct{})
	unblockSeam := make(chan struct{})
	var seamOnce sync.Once

	lk, err := TryAcquireSessionLockWithOptions(dir, "handoff-close",
		withHeartbeatExitSeam(func() {
			seamOnce.Do(func() { close(seamEntered) })
			<-unblockSeam
		}))
	require.NoError(t, err)

	// No RecordActivity, no tick: the heartbeat's ticker never fires a
	// touch before Release stops it, so it exits its select loop
	// immediately on close(stop) and goes straight into its finalizer.
	released := make(chan error, 1)
	go func() { released <- lk.Release() }()

	select {
	case <-seamEntered:
		// The heartbeat has made its closer decision (CAS'd to
		// closeStateReleaseCloses, since nothing raced it yet) and is
		// now parked in the seam, strictly before close(heartbeatExited).
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat never reached the exit seam")
	}

	// heartbeatExited is still open (blocked in the seam), so Release
	// cannot take its fast path. It must still return within its bounded
	// handoff window: its own CAS attempt loses (heartbeat already
	// claimed closeStateReleaseCloses), so it falls through and closes
	// the handle itself.
	select {
	case err := <-released:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Release did not return within its bounded handoff window")
	}

	// Release lost the CAS after registering its own handoffDone: it must
	// have closed it (after its own unlock) and dropped the entry, or a
	// same-process waiter would sit out the full pendingHandoffWaitBound.
	select {
	case <-lk.handoffDone:
	default:
		t.Fatal("the losing Release must close its own pending-handoff channel")
	}
	_, pending := pendingHandoffUnlocks.Load(lk.Path)
	require.False(t, pending, "the losing Release must drop its pending-handoff entry")

	close(unblockSeam)

	require.Eventually(t, func() bool {
		lk2, err := TryAcquireSessionLock(dir, "handoff-close")
		if err != nil {
			return false
		}
		return lk2.Release() == nil
	}, 2*time.Second, 20*time.Millisecond,
		"the lock file must become reacquirable: on the old Load/Store code, this interleaving leaves nobody having closed it")
}

// TestFinishPendingHandoffLeavesNewerLocksEntry: finishing one lock's
// handoff must never close or remove a channel registered by a newer lock
// on the same path — closing it would make that newer lock's own finish
// panic on a double close.
//
// Revert check: make finishPendingHandoff close whatever channel is
// registered for path (Load, then close) — chB is closed and removed.
func TestFinishPendingHandoffLeavesNewerLocksEntry(t *testing.T) {
	path := t.TempDir() + "/newer.lock"
	chA := make(chan struct{})
	chB := make(chan struct{})
	pendingHandoffUnlocks.Store(path, chB)
	defer pendingHandoffUnlocks.Delete(path)

	finishPendingHandoff(path, chA)

	select {
	case <-chA:
	default:
		t.Fatal("finishPendingHandoff must close the channel it was given")
	}
	select {
	case <-chB:
		t.Fatal("finishPendingHandoff closed a newer lock's channel")
	default:
	}
	v, ok := pendingHandoffUnlocks.Load(path)
	require.True(t, ok, "the newer lock's entry must stay registered")
	require.Equal(t, chB, v)
}

// TestReacquireWaitsForOwnHandoffUnlock proves defect #1031 is fixed: a
// same-process re-acquire of the exact same session path, arriving while
// this process's own prior Release() handoff is still pending (heartbeat
// finalizer hasn't yet drained a hung touch and run the real unlockFile),
// must wait for that unlock instead of bouncing off its own outgoing lock
// as though it were a foreign holder.
//
// Revert check: comment out (or no-op) the `waitForPendingHandoff(path)`
// call at the top of acquireSessionLockFileWithOptions in
// internal/session/lock.go. The single TryAcquireSessionLock call below
// then races the real OS lock immediately, before the delayed unblock(),
// and fails with a *SessionLockBusyError instead of returning nil.
func TestReacquireWaitsForOwnHandoffUnlock(t *testing.T) {
	dir := t.TempDir()

	entered := make(chan struct{}, 1)
	unblock := make(chan struct{})
	original := touchLockFileFn
	touchLockFileFn = func(f *os.File, now time.Time) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-unblock
		return original(f, now)
	}
	defer func() { touchLockFileFn = original }()

	lk, err := TryAcquireSessionLockWithOptions(dir, "handoff-reacquire", WithHeartbeatInterval(1*time.Millisecond))
	require.NoError(t, err)

	lk.RecordActivity()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat never entered the blocked touch")
	}

	// Release must return via the bounded handoff: the heartbeat is
	// genuinely stuck inside touchLockFileFn on the handle, so Release's
	// own CAS wins and it returns without unlocking/closing itself,
	// having registered the pending-handoff entry first.
	released := make(chan error, 1)
	go func() { released <- lk.Release() }()
	select {
	case err := <-released:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Release did not return via bounded handoff")
	}

	go func() {
		time.Sleep(300 * time.Millisecond)
		close(unblock)
	}()

	// A single, immediate, no-retry re-acquire of the exact path this
	// process just released. At this instant the OS lock is still held
	// by our own now-orphaned handle (the heartbeat's touch hasn't
	// drained yet, so unlockFile hasn't run). Without waitForPendingHandoff
	// this fails immediately with SessionLockBusyError against our OWN
	// prior lock; with it, this call blocks until the handoff completes
	// (~300ms later) and then succeeds.
	lk2, err := TryAcquireSessionLock(dir, "handoff-reacquire")
	require.NoError(t, err, "same-process re-acquire must wait for its own pending handoff unlock, not bounce off it")
	require.NoError(t, lk2.Release())
}
