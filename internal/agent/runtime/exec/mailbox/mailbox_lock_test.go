package mailbox

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	"github.com/stretchr/testify/require"
)

// TestMailbox_DrainOrReleaseFinal_OSLockReleasedBeforeStateFlipsIdle is the
// deterministic regression test for round 11 review, HIGH-1.
//
// Before the fix, the end-of-turn drain was two SEPARATE steps with no
// shared critical section: (1) Mailbox.DrainOrRelease flips mb.State to
// MbIdle under mb.Mu and returns, then (2) — only much later, after
// runTurn's own deferred wg.Wait() (title Generation) and the rest of the
// call stack unwound back up into Run() — Run's own `defer lk.Release()`
// finally released the OS-level session.SessionLock. Any same-process
// observer of IsSessionBusy(sessionID) (a plain `mb.State != MbIdle` read
// under mb.Mu) could see "not busy" strictly BEFORE step 2 ran, legitimately
// try to become the new owner, and hit a spurious SessionLockBusyError from
// its OWN PROCESS's prior, not-yet-unwound turn — silently dropping the
// message (tryReserveSession's owner-branch does not requeue on that error).
//
// This test proves the fix removes that two-step shape entirely by
// reconstructing BOTH shapes against the exact same real OS-level
// session.SessionLock and observing the difference directly:
//
//   - "old shape": call mb.DrainOrRelease(Epoch) (the Mailbox-only primitive,
//     still present for direct unit testing) and, WITHOUT holding mb.Mu,
//     probe the real OS lock before calling lk.Release() separately/later —
//     proving the old two-step API allows exactly the observable gap the
//     bug report describes: state already idle, lock still held.
//   - "new shape": call mb.DrainOrReleaseFinal(Epoch, ...) with the SAME
//     lk.Release as its release callback — proving that by the time it
//     returns hasNext=false, the OS lock is unconditionally already free,
//     because release() runs inside the same mb.Mu critical section that
//     flips the state, with no separate later step for a same-process
//     caller to race against.
func TestMailbox_DrainOrReleaseFinal_OSLockReleasedBeforeStateFlipsIdle(t *testing.T) {
	dataDir := t.TempDir()

	t.Run("old two-step shape reproduces the gap", func(t *testing.T) {
		const sessionID = "high-1-lock-test-old-shape"
		lk, err := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
		require.NoError(t, err)

		mb := &Mailbox{
			State:            MbOwned,
			Epoch:            1,
			DispatcherCancel: func() {},
			Current:          Generation{ID: 1, Cancel: func() {}},
		}

		// Step 1 (old shape): Mailbox-only release. Nothing about this call
		// touches the OS lock at all.
		_, hasNext := mb.DrainOrRelease(1)
		require.False(t, hasNext)
		require.Equal(t, MbIdle, mb.State, "Mailbox reports idle immediately after step 1")

		// The observable bug: state is idle, but the OS lock this "turn" was
		// holding has NOT been released yet (step 2 hasn't run). A
		// same-process caller relying on IsSessionBusy==false would wrongly
		// believe the lock is free here.
		_, lockErr := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
		require.Error(t, lockErr, "reproduction: with the old two-step shape, the OS lock is still held even though "+
			"the Mailbox already reports idle — this IS the HIGH-1 gap")

		// Step 2 (old shape, "later"): release finally happens.
		require.NoError(t, lk.Release())
		lk2, err := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
		require.NoError(t, err)
		require.NoError(t, lk2.Release())
	})

	t.Run("new atomic shape has no gap", func(t *testing.T) {
		const sessionID = "high-1-lock-test-new-shape"
		lk, err := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
		require.NoError(t, err)

		mb := &Mailbox{
			State:            MbOwned,
			Epoch:            1,
			DispatcherCancel: func() {},
			Current:          Generation{ID: 1, Cancel: func() {}},
		}

		_, hasNext, orphaned, releaseErr := mb.DrainOrReleaseFinal(1, lk.Release)
		require.False(t, hasNext)
		require.NoError(t, releaseErr)
		require.Empty(t, orphaned)
		require.Equal(t, MbIdle, mb.State)

		// THE core HIGH-1 assertion: the instant DrainOrReleaseFinal reports
		// idle, the OS lock MUST already be free — no separate later step
		// remains for a same-process caller to race against.
		lk2, lockErr := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
		require.NoError(t, lockErr, "the OS lock must be acquirable immediately once DrainOrReleaseFinal reports "+
			"idle — IsSessionBusy()==false must mean the OS lock is really free, not just that the in-memory "+
			"state flipped first (round 11 review, HIGH-1)")
		require.NoError(t, lk2.Release())
	})
}

// TestMailbox_DrainOrReleaseFinal_MuNotHeldDuringRelease_ButNoPrematureIdle
// is the #296/P1-C rewrite of the round-11 structural test
// (TestMailbox_DrainOrReleaseFinal_MuHeldForWholeOfReleaseCallback, which
// asserted the OPPOSITE of the current contract and has been deleted), then
// corrected again per the #297 review: an earlier version of this test
// asserted that work landing during the release() window is handed BACK to
// the original turn loop (state == MbOwned). That is wrong — release() has
// already run by the time the racing Submit() is noticed, so the OS lock is
// gone; handing the turn loop another turn to run would mean it runs
// unprotected by any inter-process lock, letting a second process acquire
// the same session lock concurrently. The corrected contract: such work is
// drained into `orphaned` and the era still ends at MbIdle. See
// TestMailbox_DrainOrReleaseFinal_OrphanedWorkNeverRunsWithoutFreshOSLock
// below for the end-to-end proof that a second lock acquisition succeeds
// the instant this call returns.
//
// Before #296, mb.Mu was held for the whole of release()'s disk I/O
// (Truncate/Seek/Sync/sidecar-unlink/unlock/Close — no context, no timeout),
// so a slow/hung filesystem, antivirus, or SMB share blocked every other
// in-process reader of the Mailbox (Submit, Cancel, InterruptAndReplace,
// IsSessionBusy, IsBusy, CancelAll) for that session, unboundedly. #296
// removes that coupling: release() now runs with mb.Mu dropped, and
// mb.State == MbReleasing stands in for the mutex as far as HIGH-1's
// atomicity goes.
//
// This test proves the full contract against the exact same paused-release
// setup the old test used:
//  1. mb.Mu is NOT held while release() runs — a concurrent Submit()
//     completes promptly instead of blocking for the duration of release().
//  2. HIGH-1 still holds — that prompt Submit() does NOT become the new
//     owner (MbReleasing reads as busy), and a same-process busy-read
//     observes busy for the whole window.
//  3. The work that landed during the window is neither lost NOR handed
//     back to the (now lock-less) turn loop: it comes back as `orphaned`,
//     and the era ends at MbIdle.
func TestMailbox_DrainOrReleaseFinal_MuNotHeldDuringRelease_ButNoPrematureIdle(t *testing.T) {
	mb := &Mailbox{
		State:            MbOwned,
		Epoch:            1,
		DispatcherCancel: func() {},
		Current:          Generation{ID: 1, Cancel: func() {}},
	}

	releaseEntered := make(chan struct{})
	releaseMayReturn := make(chan struct{})
	release := func() error {
		close(releaseEntered)
		<-releaseMayReturn
		return nil
	}

	type drainResult struct {
		hasNext    bool
		releaseErr error
		orphaned   []agentcall.SessionAgentCall
	}
	drainDone := make(chan drainResult, 1)
	go func() {
		_, hasNext, orphaned, releaseErr := mb.DrainOrReleaseFinal(1, release)
		drainDone <- drainResult{hasNext: hasNext, releaseErr: releaseErr, orphaned: orphaned}
	}()

	select {
	case <-releaseEntered:
	case <-drainDone:
		t.Fatal("DrainOrReleaseFinal returned before release() was even entered")
	}

	// (1) release() is now paused. mb.Mu must NOT be held: a concurrent
	// Submit() must complete promptly, not block until release() returns.
	submitDone := make(chan struct {
		becomeOwner bool
		Epoch       uint64
	}, 1)
	go func() {
		becomeOwner, Epoch := mb.Submit(agentcall.SessionAgentCall{SessionID: "s1", Prompt: "concurrent"}, func() {})
		submitDone <- struct {
			becomeOwner bool
			Epoch       uint64
		}{becomeOwner, Epoch}
	}()

	select {
	case res := <-submitDone:
		// (2) HIGH-1: even though Submit() was not blocked, it must not have
		// become the new owner — the OS lock release is still in flight.
		require.False(t, res.becomeOwner, "HIGH-1: while release() is still in flight, a concurrent Submit() must "+
			"NOT become the new owner — MbReleasing must read as busy and Queue instead")
		require.Equal(t, uint64(0), res.Epoch, "a non-owner Submit must return Epoch 0")
	case <-time.After(2 * time.Second):
		t.Fatal("Submit() did not complete while release() was paused — mb.Mu is still held during release(), " +
			"reintroducing the unbounded control-plane stall #296 exists to remove")
	}

	// A same-process busy read must also not block, AND must report busy —
	// the actual HIGH-1 property CancelAll/IsBusy/IsSessionBusy depend on.
	busyDone := make(chan bool, 1)
	go func() {
		mb.Mu.Lock()
		s := mb.State
		mb.Mu.Unlock()
		busyDone <- s != MbIdle
	}()
	select {
	case busy := <-busyDone:
		require.True(t, busy, "the Mailbox must read as busy (state != MbIdle) while release() is in flight")
	case <-time.After(2 * time.Second):
		t.Fatal("a Mailbox-state read did not complete while release() was paused — mb.Mu is still held")
	}

	close(releaseMayReturn)

	// (3) The call submitted during the release window must come back as
	// orphaned — NOT handed back to the turn loop (the OS lock is already
	// gone) and NOT lost.
	select {
	case res := <-drainDone:
		require.False(t, res.hasNext, "work that lands during the release() window must NOT be handed back to "+
			"the turn loop — the OS lock is already released by the time it is noticed")
		require.Equal(t, []agentcall.SessionAgentCall{{SessionID: "s1", Prompt: "concurrent"}}, res.orphaned,
			"the racing call must be returned as orphaned so the caller can restart it under a fresh OS lock")
		require.NoError(t, res.releaseErr)
	case <-time.After(2 * time.Second):
		t.Fatal("DrainOrReleaseFinal did not return after release() completed")
	}

	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	require.Equal(t, MbIdle, mb.State, "the era must end at MbIdle — there is no OS lock left to keep a turn loop "+
		"running under")
	require.Nil(t, mb.DispatcherCancel)
	require.Nil(t, mb.Current.Cancel)
}

// TestMailbox_DrainOrReleaseFinal_OrphanedWorkNeverRunsWithoutFreshOSLock is
// the direct regression test for the #297 finding: it proves the actual
// cross-process invariant, not just the in-memory Mailbox postcondition.
// While a separate goroutine holds a call that DrainOrReleaseFinal reported
// as orphaned, a DIRECT session.TryAcquireSessionLock (standing in for a
// second `rush` process) must succeed immediately — proving no code path
// is still running "as if" it held that lock. Reverting the finalize step to
// the rejected hand-back-to-MbOwned shape must make this test fail: the
// prior draft returned hasNext=true/MbOwned for this exact scenario, and a
// caller trusting that (as drainOrReleaseMerged's turn loop does) would
// start another turn while a concurrent TryAcquireSessionLock now succeeds
// too — two owners of one session.
func TestMailbox_DrainOrReleaseFinal_OrphanedWorkNeverRunsWithoutFreshOSLock(t *testing.T) {
	dataDir := t.TempDir()
	const sessionID = "p1-c-orphan-no-double-lock-test"

	lk, err := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)

	mb := &Mailbox{
		State:            MbOwned,
		Epoch:            1,
		DispatcherCancel: func() {},
		Current:          Generation{ID: 1, Cancel: func() {}},
	}

	// release() simulates a concurrent Submit() landing during the window —
	// exactly like a real Submit() would, it just appends under mb.Mu.
	release := func() error {
		mb.Mu.Lock()
		mb.Submitted = append(mb.Submitted, agentcall.SessionAgentCall{SessionID: sessionID, Prompt: "raced in"})
		mb.Mu.Unlock()
		return lk.Release()
	}

	_, hasNext, orphaned, releaseErr := mb.DrainOrReleaseFinal(1, release)
	require.NoError(t, releaseErr)
	require.False(t, hasNext, "orphaned work must not be reported as hasNext — that would tell the caller's turn "+
		"loop to keep running without a lock")
	require.Len(t, orphaned, 1)

	// THE core #297 assertion: the instant this call returns, the OS lock is
	// genuinely free for a second acquirer — proving nothing is still
	// running "on behalf of" the released lock.
	lk2, lockErr := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, lockErr, "the OS lock must be immediately acquirable by a second holder once "+
		"DrainOrReleaseFinal returns — if orphaned work were instead handed back as hasNext=true/MbOwned, a real "+
		"caller would now be running a turn for this session WHILE this second acquisition also succeeds: two "+
		"owners of one session, exactly the bug #297 reported")
	require.NoError(t, lk2.Release())
}

// TestMailbox_DrainOrReleaseFinal_ReleaseErrorStillReachesIdle is the #296/
// P1-C guarantee that a release() returning an error cannot wedge the
// Mailbox in MbReleasing forever: it must still finalize to MbIdle (the
// error is surfaced for logging only, never fatal — mirroring how a failed
// Run() defer lk.Release() today only logs).
func TestMailbox_DrainOrReleaseFinal_ReleaseErrorStillReachesIdle(t *testing.T) {
	mb := &Mailbox{
		State:            MbOwned,
		Epoch:            1,
		DispatcherCancel: func() {},
		Current:          Generation{ID: 1, Cancel: func() {}},
	}

	release := func() error { return errors.New("disk full") }

	next, hasNext, orphaned, releaseErr := mb.DrainOrReleaseFinal(1, release)

	require.False(t, hasNext)
	require.Equal(t, agentcall.SessionAgentCall{}, next)
	require.Empty(t, orphaned)
	require.Error(t, releaseErr, "the release error must be surfaced to the caller for logging")
	require.ErrorContains(t, releaseErr, "disk full")

	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	require.Equal(t, MbIdle, mb.State, "a failed release must still finalize the Mailbox to MbIdle, not wedge it "+
		"in MbReleasing forever")
	require.Nil(t, mb.DispatcherCancel)
	require.Nil(t, mb.Current.Cancel)
}

// TestMailbox_DrainOrReleaseFinal_ReleasePanicStillReachesIdle is the #296/
// P1-C guarantee that a PANICKING release() also cannot wedge the Mailbox:
// the panic is recovered into an error and the Mailbox still finalizes to
// MbIdle. Without an internal recover, a panicking release() would propagate
// up through DrainOrReleaseFinal on a goroutine with no mb.Mu held at the
// panic site to clean up under, leaving the Mailbox stuck in MbReleasing —
// permanently "busy" with nobody left to ever finish the transition. The
// test goroutine carries its own recover so an unfixed regression fails at
// the MbIdle assertion below instead of crashing the whole test binary.
func TestMailbox_DrainOrReleaseFinal_ReleasePanicStillReachesIdle(t *testing.T) {
	mb := &Mailbox{
		State:            MbOwned,
		Epoch:            1,
		DispatcherCancel: func() {},
		Current:          Generation{ID: 1, Cancel: func() {}},
	}

	release := func() error { panic("release boom") }

	type drainResult struct {
		hasNext    bool
		releaseErr error
		orphaned   []agentcall.SessionAgentCall
	}
	drainDone := make(chan drainResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				drainDone <- drainResult{
					hasNext:    false,
					releaseErr: fmt.Errorf("panic escaped DrainOrReleaseFinal — internal recover missing: %v", r),
				}
			}
		}()
		_, hasNext, orphaned, releaseErr := mb.DrainOrReleaseFinal(1, release)
		drainDone <- drainResult{hasNext: hasNext, releaseErr: releaseErr, orphaned: orphaned}
	}()

	select {
	case res := <-drainDone:
		require.False(t, res.hasNext)
		require.Empty(t, res.orphaned)
		require.Error(t, res.releaseErr, "the recovered panic must be surfaced as an error")
		require.ErrorContains(t, res.releaseErr, "release boom")
	case <-time.After(2 * time.Second):
		t.Fatal("DrainOrReleaseFinal did not return — the panic was not recovered and the Mailbox is stuck in MbReleasing")
	}

	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	require.Equal(t, MbIdle, mb.State, "a panicking release must still finalize the Mailbox to MbIdle")
	require.Nil(t, mb.DispatcherCancel)
	require.Nil(t, mb.Current.Cancel)
}

// TestMailbox_DrainOrReleaseFinal_HardStopDuringReleaseWindow_HandsWorkToDurableEnqueue
// proves the #646 fix: HardStop (CancelAll/shutdown) landing WHILE release()
// is in flight (state == MbReleasing) must still hand out any work that
// raced in as `orphaned` so drainOrReleaseMerged can durably enqueue it via
// restartOrphaned. Before the fix, this branch discarded the work; after the
// fix, it's handed out in the same order as the live branches (Replacement
// first, then submitted), and the caller (drainOrReleaseMerged) durably
// enqueues it as a session_run_queue row rather than starting a provider turn.
func TestMailbox_DrainOrReleaseFinal_HardStopDuringReleaseWindow_HandsWorkToDurableEnqueue(t *testing.T) {
	mb := &Mailbox{
		State:            MbOwned,
		Epoch:            1,
		DispatcherCancel: func() {},
		Current:          Generation{ID: 1, Cancel: func() {}},
	}

	// Simulate HardStop AND concurrent Submit()/InterruptAndReplace both
	// landing during the release() window: the release callback itself
	// (running with mb.Mu free) does both, exactly as two separate real
	// goroutines would.
	replacedCall := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "Replacement raced in during shutdown", LogicalCallID: "Replacement-123"}
	submittedCall := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "raced in during shutdown", LogicalCallID: "submitted-456"}
	release := func() error {
		mb.HardStop()
		mb.Mu.Lock()
		mb.Replacement = &replacedCall
		mb.Submitted = append(mb.Submitted, submittedCall)
		mb.Mu.Unlock()
		return nil
	}

	next, hasNext, orphaned, releaseErr := mb.DrainOrReleaseFinal(1, release)

	require.NoError(t, releaseErr)
	require.False(t, hasNext, "work that landed during the release window must NOT be handed back once HardStop "+
		"has latched — a shutdown must not start a fresh turn")
	require.Equal(t, agentcall.SessionAgentCall{}, next)
	require.Len(t, orphaned, 2, "both Replacement and submitted must be handed out as orphaned for durable enqueue")
	require.Equal(t, "Replacement-123", orphaned[0].LogicalCallID, "Replacement must come first, matching live-branch priority")
	require.Equal(t, "submitted-456", orphaned[1].LogicalCallID, "submitted must come after Replacement")

	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	require.Equal(t, MbIdle, mb.State, "the era must still end at MbIdle even though HardStop landed mid-release")
	require.Nil(t, mb.DispatcherCancel)
	require.Nil(t, mb.Current.Cancel)
	require.True(t, mb.Stopped, "the Stopped latch must remain set")
}

// TestSessionAgent_IsBusyAndIsSessionBusy_TreatMbReleasingAsBusy is the
// regression test for the #296/P1-C half of HIGH-1 that lives on
// sessionAgent rather than Mailbox: IsBusy() (CancelAll's 5-second shutdown
// drain loop) and IsSessionBusy() (the same-process "can I become owner"
// check) must both report BUSY while a Mailbox sits in MbReleasing, not just
// MbOwned. Before this fix, IsBusy() read `mb.State == MbOwned` — MbReleasing
// didn't exist yet when that line was written, but once it does exist,
// MbReleasing fails that equality check and IsBusy() would have reported
// "not busy" for a session whose release() disk I/O (OS session-lock
// teardown) is still genuinely in flight on another goroutine. CancelAll's
// drain loop calling IsBusy() in that window would return early, and
// App.Shutdown proceeds to tear the process's DB down right after CancelAll
// returns — the exact class of race HIGH-1 exists to prevent, reachable
// through the shutdown path instead of the "becomes new owner" path.
// IsSessionBusy() already used `!= MbIdle` before this fix and is included
// here as the non-regression companion proving it stays correct.
//
// Constructs a bare sessionAgent with just the mailboxes map (no provider,
// no DB) since this is testing pure Mailbox-state plumbing, not a live turn.

// TestMailbox_DrainOrReleaseFinal_SubmittedReclaimNeverReleasesLock is the
// companion proving the OTHER half of HIGH-1's fix: when mb.Submitted
// has something, the OS lock must NOT be released at all — ownership (and
// the lock) is handed straight to the reclaimed turn without ever exposing a
// gap where a different process could steal it (see DrainOrReleaseFinal's
// own doc for why release-then-reacquire is rejected).
func TestMailbox_DrainOrReleaseFinal_SubmittedReclaimNeverReleasesLock(t *testing.T) {
	dataDir := t.TempDir()
	const sessionID = "high-1-lock-reclaim-test"

	lk, err := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lk.Release() })

	mb := &Mailbox{
		State:   MbOwned,
		Epoch:   1,
		Current: Generation{ID: 1, Cancel: func() {}},
	}

	reclaimed := agentcall.SessionAgentCall{SessionID: sessionID, Prompt: "reclaimed from submitted"}
	releaseCalled := false
	mb.Submitted = []agentcall.SessionAgentCall{reclaimed}
	release := func() error {
		releaseCalled = true
		return lk.Release()
	}

	next, hasNext, orphaned, releaseErr := mb.DrainOrReleaseFinal(1, release)

	require.True(t, hasNext)
	require.Equal(t, reclaimed, next)
	require.NoError(t, releaseErr)
	require.Empty(t, orphaned)
	require.False(t, releaseCalled, "release() must NOT be invoked when mb.Submitted reclaims ownership — "+
		"the OS lock stays held for the reclaimed turn, never released-then-reacquired")
	require.Equal(t, MbOwned, mb.State, "state must stay owned across a mb.Submitted reclaim")
	require.Equal(t, uint64(1), mb.Epoch, "Epoch must NOT bump on a same-era reclaim")

	// The OS lock must still be held by THIS process — a second acquisition
	// attempt must fail.
	_, lockErr := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
	require.Error(t, lockErr, "the OS lock must remain held across a mb.Submitted reclaim")
}

// TestMailbox_DrainOrReleaseFinal_SubmittedBranchClearsStaleCancelHandle is
// the regression test for round 12 review, finding A: the SAME MEDIUM-1
// shape as TestMailbox_DrainOrReleaseFinal_SubmittedReclaimNeverReleasesLock
// above, but explicitly checking the stale-cancel-handle postcondition.
// The mb.Submitted Queue is populated by the CURRENT Submit() path and
// is now the only "keep running" branch in DrainOrReleaseFinal (the former
// checkLegacy branch was removed in #308).
//
// By the time this branch runs, mb.Current.Cancel still holds the
// just-finished turn's own genCtx cancel func — already invoked once via
// runTurn's unconditional `cancel()` call immediately before the drain
// (agent.go, right before drainOrReleaseMerged) — inert but NOT nil. Left
// untouched, it defeats Cancel()/InterruptAndReplace()'s
// `current.cancel == nil` fallback gate exactly like the original MEDIUM-1
// defect, for the whole window until the NEXT turn's own BeginGeneration
// (which can be as long as title Generation takes — several seconds).
//
// Caught by round 12's independent review; confirmed by this session via
// direct code read (mb.Submitted branch returned without touching
// mb.Current.Cancel at all) before landing the one-line fix.
func TestMailbox_DrainOrReleaseFinal_SubmittedBranchClearsStaleCancelHandle(t *testing.T) {
	queued := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "queued via Submit()"}
	mb := &Mailbox{
		State:            MbOwned,
		Epoch:            1,
		DispatcherCancel: func() {},
		Current:          Generation{ID: 1, Cancel: func() {}},
		Submitted:        []agentcall.SessionAgentCall{queued},
	}

	next, hasNext, orphaned, releaseErr := mb.DrainOrReleaseFinal(1, nil)

	require.True(t, hasNext)
	require.Equal(t, queued, next)
	require.NoError(t, releaseErr)
	require.Empty(t, orphaned)
	require.Equal(t, MbOwned, mb.State, "state must stay owned when mb.Submitted has a queued call")
	require.Empty(t, mb.Submitted, "the popped call must be removed from the Queue")

	// THE core finding-A assertion: current.cancel must be cleared so
	// Cancel()'s fallback to dispatcherCancel actually fires, instead of
	// silently invoking the just-finished turn's spent, harmless cancel.
	require.Nil(t, mb.Current.Cancel, "current.cancel must be cleared on the mb.Submitted branch too — otherwise "+
		"Cancel() calls the stale, already-spent PRIOR turn's cancel func instead of ever reaching the "+
		"dispatcherCancel fallback (round 12 review, finding A)")
}

// TestMailbox_DrainAfterCancel_ClearsStaleCancelHandle is the regression
// test for round 13 review's fourth instance of the same MEDIUM-1 shape:
// DrainAfterCancel — the cancel-branch drain, called from agent.go's
// isCancelErr block right before its own `cancel()` call — left
// mb.Current.Cancel holding the just-cancelled Generation's own spent
// (but non-nil) genCtx cancel func on BOTH of its hit branches
// (Replacement and submitted), defeating Cancel()/InterruptAndReplace()'s
// current.cancel==nil fallback gate for the window until the Replacement
// turn's own BeginGeneration — arguably the worst instance of the four,
// since this is exactly the "user already cancelled once and wants the
// Replacement instead" path.
func TestMailbox_DrainAfterCancel_ClearsStaleCancelHandle(t *testing.T) {
	t.Run("Replacement branch", func(t *testing.T) {
		Replacement := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "Replacement"}
		mb := &Mailbox{
			State:       MbOwned,
			Replacement: &Replacement,
			Current:     Generation{ID: 1, Cancel: func() {}},
		}

		next, ok := mb.DrainAfterCancel()

		require.True(t, ok)
		require.Equal(t, Replacement.SessionID, next.SessionID)
		require.Equal(t, Replacement.Prompt, next.Prompt)
		require.True(t, next.ReplacementHandoff, "Replacement extraction must preserve callback provenance")
		require.Nil(t, mb.Current.Cancel, "current.cancel must be cleared on the Replacement branch — otherwise "+
			"Cancel()/InterruptAndReplace() call the stale, already-cancelled PRIOR Generation's cancel func "+
			"instead of ever reaching the dispatcherCancel fallback (round 13 review, fourth instance)")
	})

	t.Run("submitted branch", func(t *testing.T) {
		queued := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "queued"}
		mb := &Mailbox{
			State:     MbOwned,
			Submitted: []agentcall.SessionAgentCall{queued},
			Current:   Generation{ID: 1, Cancel: func() {}},
		}

		next, ok := mb.DrainAfterCancel()

		require.True(t, ok)
		require.Equal(t, queued, next)
		require.Nil(t, mb.Current.Cancel, "current.cancel must be cleared on the submitted branch too — same "+
			"defect, same fix, mirroring the Replacement branch above")
	})
}
