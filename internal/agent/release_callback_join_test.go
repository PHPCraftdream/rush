package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func newReleaseJoinCoordinator() (*coordinator, *sessionAgent) {
	c := &coordinator{}
	c.asyncJobs = newWorkLedger(c.notifyAsyncCompletion)
	a := NewSessionAgent(SessionAgentOptions{OnSessionIdle: c.onSessionIdleHook}).(*sessionAgent)
	c.currentAgent = a
	return c, a
}

func releaseJoinReservation(t *testing.T, a *sessionAgent) {
	t.Helper()
	_, epoch, cancel, ok := a.ReserveExclusive(context.Background(), "release-join")
	require.True(t, ok)
	a.ReleaseExclusive("release-join", epoch, cancel)
	require.False(t, a.IsSessionBusy("release-join"), "ownership is cleaned before the callback")
}

func awaitReleaseSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle signal was not received")
	}
}

func releaseJoinUnblock(t *testing.T, ch chan struct{}) func() {
	t.Helper()
	var once sync.Once
	unblock := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(unblock)
	return unblock
}

// assertCallbackGracefulJoin uses quiescence, not elapsed time: with the
// callback blocked, an omitted Wait MUST reach the after-waits seam before
// synctest.Wait returns. That seam snapshots exit before accepting an ack.
// With both Waits present it cannot snapshot until we unblock the callback.
func assertCallbackGracefulJoin(t *testing.T, notify bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		c, a := newReleaseJoinCoordinator()
		entered, unblock, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
		release := releaseJoinUnblock(t, unblock)
		ack := make(chan struct{})
		acknowledge := releaseJoinUnblock(t, ack)
		var calls atomic.Int32
		hook := func(context.Context) {
			calls.Add(1)
			close(entered)
			<-unblock
			close(exited)
		}
		if notify {
			c.notifyCallbackHook = hook
			c.notifyAsyncCompletion(AsyncCompletion{SessionID: "completion"})
		} else {
			c.releaseCallbackHook = hook
			releaseJoinReservation(t, a)
		}
		awaitReleaseSignal(t, entered)
		observed := make(chan bool, 1)
		c.callbackAfterWaitHook = func() {
			finished := false
			select {
			case <-exited:
				finished = true
			default:
			}
			observed <- finished
			<-ack
		}
		// Disable expiry: only the join and our explicit seam ack may finish
		// shutdown. Neither done/expiry select nor wall-clock timing is an oracle.
		c.callbackJoinTimeout = func() <-chan time.Time { return nil }
		returned := make(chan bool, 1)
		go func() { returned <- c.CancelAll() }()
		synctest.Wait()
		release()
		finished := <-observed
		acknowledge()
		if notify {
			require.True(t, finished, "notify join reached after-waits seam before callback exit")
		} else {
			require.True(t, finished, "release join reached after-waits seam before callback exit")
		}
		require.False(t, <-returned, "callback completion must allow graceful shutdown")
		if notify {
			c.notifyAsyncCompletion(AsyncCompletion{SessionID: "late-completion"})
			c.notifyWG.Wait()
			require.EqualValues(t, 1, calls.Load(), "late durable completion must not spawn a wake")
		}
	})
}

func TestReleaseCallbackJoinedByCancelAll(t *testing.T) {
	assertCallbackGracefulJoin(t, false)
}

func TestReleaseCallbackJoinExpiryReportsStillBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, a := newReleaseJoinCoordinator()
		entered, unblock := make(chan struct{}), make(chan struct{})
		release := releaseJoinUnblock(t, unblock)
		c.releaseCallbackHook = func(context.Context) {
			close(entered)
			<-unblock
		}
		releaseJoinReservation(t, a)
		awaitReleaseSignal(t, entered)
		expired := make(chan time.Time, 1)
		c.callbackJoinTimeout = func() <-chan time.Time { return expired }
		returned := make(chan bool, 1)
		go func() { returned <- c.CancelAll() }()
		synctest.Wait()
		expired <- time.Now()
		require.True(t, <-returned, "unjoined release callback must report stillBusy")
		release()
		c.releaseWG.Wait()
	})
}

func TestReleaseCallbackRefusedAfterShutdown(t *testing.T) {
	c, a := newReleaseJoinCoordinator()
	var entered atomic.Int32
	c.releaseCallbackHook = func(context.Context) { entered.Add(1) }
	require.False(t, c.CancelAll())
	// Release also fires the idle hook for stale epochs after the agent latch
	// closes. Admission, not the mailbox, must refuse this work.
	a.ReleaseExclusive("late-release", 0, nil)
	c.releaseWG.Wait()
	require.Zero(t, entered.Load(), "refused callback must not enter the seam")
	require.False(t, a.IsSessionBusy("late-release"))
}

func TestReleaseCallbackShutdownContextCancelled(t *testing.T) {
	c, a := newReleaseJoinCoordinator()
	entered, cancelled := make(chan struct{}), make(chan struct{})
	c.releaseCallbackHook = func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
	}
	t.Cleanup(func() {
		c.callbackMu.Lock()
		cancel := c.callbackCancel
		c.callbackMu.Unlock()
		if cancel != nil {
			cancel()
		}
		c.releaseWG.Wait()
	})
	releaseJoinReservation(t, a)
	awaitReleaseSignal(t, entered)
	require.False(t, c.CancelAll(), "cooperative callback should join gracefully")
	select {
	case <-cancelled:
	default:
		t.Fatal("shutdown returned without cancellation and callback exit")
	}
}

func TestNotifyCallbackJoinedAndRefusedByCancelAll(t *testing.T) {
	assertCallbackGracefulJoin(t, true)
}
