// Mailbox generation arming: beginGeneration re-arms the current
// generation each turn; beginCompact atomically claims ownership for a
// compaction. Pure code move from mailbox.go.

package agent

import (
	"context"
)

// beginGeneration implements design §5: called by Run's loop before each
// turn (replacing today's activeRequests.Set(call.SessionID, cancel)
// re-arm). It bumps the per-session generation counter and records cancel
// as the new current generation's cancel func, returning the new
// generation id.
func (mb *mailbox) beginGeneration(cancel context.CancelFunc) (genID uint64) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.current.id++
	mb.current.cancel = cancel
	return mb.current.id
}

// beginCompact atomically claims mailbox ownership for a compaction (manual
// /compact or inline auto-summarize). It is the atomic check-and-reserve
// that replaces the old non-atomic IsSessionBusy + runSummarize pair
// (#268/P0-4, design §6): the idle check and the state flip happen under
// one mb.mu hold, so no concurrent Run or second compaction can slip
// between them.
//
// Returns (epoch, true) when the caller becomes the sole owner. The
// compact's cancel is stored in BOTH current.cancel and dispatcherCancel so
// Cancel(sessionID), CancelAll, and interruptAndReplace reach it through
// the SAME mb.current.cancel field as a turn's generation cancel — no
// synthetic "sessionID-summarize" string key. The caller must present
// epoch to abandonOwnership when the compaction finishes.
//
// Returns (0, false) when the session is already owned (a turn or another
// compaction is in progress) or stopped. The caller queues (manual /compact
// via summarizeQueue) or skips.
//
// beginCompact is NOT in mailbox_invariant_test.go's stale-handle table:
// that table covers operations that END or CONTINUE an era (drain/release/
// abandon), whose postcondition is current.cancel == nil. beginCompact
// STARTS an era with a FRESH cancel — its postcondition is the opposite
// (current.cancel != nil). Its postconditions are covered by a dedicated
// test in the regression test file added alongside this change.
func (mb *mailbox) beginCompact(cancel context.CancelFunc) (epoch uint64, ok bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.state != mbIdle || mb.stopped {
		return 0, false
	}
	mb.state = mbOwned
	mb.epoch++
	mb.dispatcherCancel = cancel
	mb.current.id++
	mb.current.cancel = cancel
	return mb.epoch, true
}
