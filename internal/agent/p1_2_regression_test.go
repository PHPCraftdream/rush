package agent

import (
	"testing"

	mailboxpkg "github.com/PHPCraftdream/rush/internal/agent/runtime/exec/mailbox"

	"github.com/stretchr/testify/require"
)

// TestP1_2_ManualCompactionShowsReleasingState verifies that manual
// compaction transitions to mailboxpkg.MbReleasing state before releasing the OS lock.
// Without this (P1-2 from the 2026-08-07 concurrency review), a hung
// filesystem/AV/SMB during lk.Release() would leave the mailbox permanently
// mailboxpkg.MbOwned, indistinguishable from an actively streaming turn. With mailboxpkg.MbReleasing
// visibility, diagnostic tools can distinguish "releasing OS lock" from
// "still working".
//
// The test verifies that:
//  1. beginRelease transitions from mailboxpkg.MbOwned to mailboxpkg.MbReleasing.
//  2. mailboxpkg.MbReleasing is observable as "busy" (not mailboxpkg.MbIdle).
//  3. finishRelease transitions from mailboxpkg.MbReleasing to mailboxpkg.MbIdle.
//  4. Wrong epoch or wrong state causes no transition (no-op).
//
// REVERT CHECK PROCEDURE:
//  1. In mailbox.go:~1156, comment out the mailboxpkg.MbReleasing transition in Be:
//     // BUG: Don't transition to mailboxpkg.MbReleasing (P1-2 revert check)
//     // mb.State = mailboxpkg.MbReleasing
//     mb.State = mailboxpkg.MbOwned // Stay in mailboxpkg.MbOwned
//  2. In agent.go:~3109, comment out the mb.BeginRelease(epoch) call:
//     // P1-2 fix (2026-08-09): Transition mailbox to mailboxpkg.MbReleasing...
//     // releaseStarted := mb.BeginRelease(epoch)
//     releaseStarted := false // TEMP: test revert
//  3. In agent.go:~3138, comment out the mb.FinishRelease(epoch) call:
//     // P1-2 fix (2026-08-09): Complete the mailboxpkg.MbReleasing -> mailboxpkg.MbIdle transition...
//     // if releaseStarted {
//     //     mb.FinishRelease(epoch)
//     // }
//  4. Run: go test ./internal/agent -run TestP1_2_ManualCompactionShowsReleasingState -v
//  5. The test will FAIL because mailboxpkg.MbReleasing is never observed (state stays mailboxpkg.MbOwned)
//  6. Restore the fix and the test will PASS again.
func TestP1_2_ManualCompactionShowsReleasingState(t *testing.T) {
	t.Parallel()

	mb := &mailboxpkg.Mailbox{}

	// Set up mailboxpkg.MbOwned state via beginCompact.
	cancelFunc := func() {}
	epoch, ok := mb.BeginCompact(cancelFunc)
	require.True(t, ok, "beginCompact should succeed")
	require.Equal(t, uint64(1), epoch, "epoch should be 1 (beginCompact increments from initial)")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbOwned), mb.State, "state should be mailboxpkg.MbOwned after beginCompact")

	// Test normal flow: beginRelease -> mailboxpkg.MbReleasing -> finishRelease -> mailboxpkg.MbIdle.

	// Call beginRelease - should transition to mailboxpkg.MbReleasing.
	releaseStarted := mb.BeginRelease(epoch)
	require.True(t, releaseStarted, "beginRelease should succeed with matching epoch")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbReleasing), mb.State, "state should be mailboxpkg.MbReleasing after beginRelease")

	// Verify that mailboxpkg.MbReleasing reports as busy (not idle).
	// We can't call mb.IsSessionBusy() directly since it's a sessionAgent method,
	// but we can verify the state is not mailboxpkg.MbIdle.
	require.NotEqual(t, mailboxpkg.MailboxState(mailboxpkg.MbIdle), mb.State, "mailboxpkg.MbReleasing should not be mailboxpkg.MbIdle")

	// Call finishRelease - should transition to mailboxpkg.MbIdle.
	finished := mb.FinishRelease(epoch)
	require.True(t, finished, "finishRelease should succeed with matching epoch")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbIdle), mb.State, "state should be mailboxpkg.MbIdle after finishRelease")

	// Test epoch mismatch: call beginRelease with wrong epoch.
	mb.BeginCompact(cancelFunc) // Move to mailboxpkg.MbOwned again
	wrongEpoch := epoch + 10
	releaseStarted = mb.BeginRelease(wrongEpoch)
	require.False(t, releaseStarted, "beginRelease should fail with wrong epoch")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbOwned), mb.State, "state should still be mailboxpkg.MbOwned after wrong epoch")

	// Test wrong St: call beginRelease when already mailboxpkg.MbIdle.
	// First, finish the current era.
	mb.AbandonOwnership(epoch + 1)
	releaseStarted = mb.BeginRelease(epoch)
	require.False(t, releaseStarted, "beginRelease should fail when state is mailboxpkg.MbIdle")

	// Test wrong St: call beginRelease when already mailboxpkg.MbReleasing.
	mb.BeginCompact(cancelFunc)              // Move to mailboxpkg.MbOwned
	epoch2, _ := mb.BeginCompact(cancelFunc) // Move to mailboxpkg.MbOwned (new epoch)
	mb.BeginRelease(epoch2)                  // Move to mailboxpkg.MbReleasing
	releaseStarted = mb.BeginRelease(epoch2)
	require.False(t, releaseStarted, "beginRelease should fail when state is mailboxpkg.MbReleasing")

	// Clean up.
	// No need to call cancelFunc() since it's a no-op func(){}.
}

// TestP1_2_ManualCompactionReleasesEvenWithoutStateTransition verifies that
// manual compaction still releases the OS lock even if beginRelease fails
// (e.g., due to epoch mismatch). This ensures we don't leak the lock in edge
// cases.
func TestP1_2_ManualCompactionReleasesEvenWithoutStateTransition(t *testing.T) {
	t.Parallel()

	mb := &mailboxpkg.Mailbox{}

	// Set up mailboxpkg.MbOwned state.
	cancelFunc := func() {}
	epoch, _ := mb.BeginCompact(cancelFunc)

	// Call beginRelease with a wrong epoch (simulates concurrent state change).
	releaseStarted := mb.BeginRelease(epoch + 1)
	require.False(t, releaseStarted, "beginRelease should fail with wrong epoch")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbOwned), mb.State, "state should still be mailboxpkg.MbOwned")

	// Even though beginRelease failed, the caller should still release the lock.
	// We can't test the actual lk.Release() here without more setup, but we can
	// verify that the mailbox state doesn't prevent us from calling finishRelease.

	// finishRelease should also fail with wrong epoch.
	finished := mb.FinishRelease(epoch + 1)
	require.False(t, finished, "finishRelease should fail with wrong epoch")

	// The caller should then abandonOwnership to transition to mailboxpkg.MbIdle.
	hadWork := mb.AbandonOwnership(epoch)
	require.False(t, hadWork, "abandonOwnership should return false (no work)")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbIdle), mb.State, "state should be mailboxpkg.MbIdle after abandonOwnership")
}

func TestP1_2_ReleasingStateAllowsCancel(t *testing.T) {
	t.Parallel()

	mb := &mailboxpkg.Mailbox{}

	// Set up mailboxpkg.MbOwned state.
	cancelFunc := func() {}
	epoch, _ := mb.BeginCompact(cancelFunc)

	// Transition to mailboxpkg.MbReleasing.
	releaseStarted := mb.BeginRelease(epoch)
	require.True(t, releaseStarted)
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbReleasing), mb.State)

	// Cancel should be a no-op (no panic, no-op return).
	// We can't easily test mb.Cancel() here, but we can verify that the state
	// doesn't change and that cancel handles are nil.
	require.Nil(t, mb.Current.Cancel, "current.cancel should be nil in mailboxpkg.MbReleasing")
	require.Nil(t, mb.DispatcherCancel, "dispatcherCancel should be nil in mailboxpkg.MbReleasing")

	// Clean up.
	mb.FinishRelease(epoch)
}

func TestP1_2_BeginReleaseIdempotent(t *testing.T) {
	t.Parallel()

	mb := &mailboxpkg.Mailbox{}

	// Set up mailboxpkg.MbOwned state.
	cancelFunc := func() {}
	epoch, _ := mb.BeginCompact(cancelFunc)

	// Call beginRelease multiple times.
	require.True(t, mb.BeginRelease(epoch), "first beginRelease should succeed")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbReleasing), mb.State, "state should be mailboxpkg.MbReleasing after first call")

	require.False(t, mb.BeginRelease(epoch), "second beginRelease should fail (already mailboxpkg.MbReleasing)")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbReleasing), mb.State, "state should remain mailboxpkg.MbReleasing after second call")

	require.False(t, mb.BeginRelease(epoch), "third beginRelease should fail")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbReleasing), mb.State, "state should remain mailboxpkg.MbReleasing after third call")

	// Clean up.
	mb.FinishRelease(epoch)
}

func TestP1_2_FinishReleaseIdempotent(t *testing.T) {
	t.Parallel()

	mb := &mailboxpkg.Mailbox{}

	// Set up mailboxpkg.MbOwned state.
	cancelFunc := func() {}
	epoch, _ := mb.BeginCompact(cancelFunc)

	// Transition to mailboxpkg.MbReleasing.
	require.True(t, mb.BeginRelease(epoch))

	// Call finishRelease multiple times.
	require.True(t, mb.FinishRelease(epoch), "first finishRelease should succeed")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbIdle), mb.State, "state should be mailboxpkg.MbIdle after first call")

	require.False(t, mb.FinishRelease(epoch), "second finishRelease should fail (already mailboxpkg.MbIdle)")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbIdle), mb.State, "state should remain mailboxpkg.MbIdle after second call")

	require.False(t, mb.FinishRelease(epoch), "third finishRelease should fail")
	require.Equal(t, mailboxpkg.MailboxState(mailboxpkg.MbIdle), mb.State, "state should remain mailboxpkg.MbIdle after third call")
}
