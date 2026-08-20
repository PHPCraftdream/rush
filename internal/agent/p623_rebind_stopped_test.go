package agent

// Task #614 (F-3): rebindDispatcher refuses when mb.stopped is latched.
//
// Before this fix, rebindDispatcher only checked mb.epoch != epoch ||
// mb.state != mbOwned, ignoring the stopped latch. This meant rebinding onto
// a hard-stopped mailbox would wire a fresh dispatcher CancelFunc into a
// process that is exiting. beginCompact already refuses stopped; this fix
// makes rebindDispatcher match it.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRebindDispatcher_RefusesStoppedMailbox verifies that rebindDispatcher
// returns false when the mailbox is hard-stopped, even if epoch and state
// match.
//
// Sequence:
//  1. Create a mailbox and claim ownership via beginCompact.
//  2. Call hardStop() on the mailbox.
//  3. Attempt rebindDispatcher with the same epoch — it must fail.
//
// Revert-check: remove the `mb.stopped` condition from rebindDispatcher.
// The test passes (rebind returns true) when it should fail.
func TestRebindDispatcher_RefusesStoppedMailbox(t *testing.T) {
	mb := &mailbox{}

	// Claim ownership via beginCompact (same as ReserveExclusive does).
	epoch, ok := mb.beginCompact(func() {})
	require.True(t, ok, "beginCompact must succeed")
	require.Equal(t, uint64(1), epoch, "epoch must be 1")

	// Hard-stop the mailbox.
	mb.hardStop()

	// Attempt to rebind the dispatcher with the same epoch.
	// This must fail because the mailbox is stopped.
	rebound := mb.rebindDispatcher(epoch, func() {})
	require.False(t, rebound, "rebindDispatcher must refuse when mailbox is stopped")

	// Verify the mailbox is indeed stopped.
	mb.mu.Lock()
	stopped := mb.stopped
	mb.mu.Unlock()
	require.True(t, stopped, "mailbox must be stopped")
}
