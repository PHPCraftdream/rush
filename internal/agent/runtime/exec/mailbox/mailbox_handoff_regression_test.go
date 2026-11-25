package mailbox

import (
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

	"github.com/stretchr/testify/require"
)

// TestMailbox_AbandonOwnershipAndPopSubmitted_EpochMismatch verifies that
// AbandonOwnershipAndPopSubmitted returns nil and touches nothing when the
// Epoch has already moved on (a later caller became owner).
func TestMailbox_AbandonOwnershipAndPopSubmitted_EpochMismatch(t *testing.T) {
	t.Parallel()

	mb := &Mailbox{
		State: MbOwned,
		Epoch: 2,
		Current: Generation{
			ID:     1,
			Cancel: func() {},
		},
		DispatcherCancel: func() {},
		Submitted: []agentcall.SessionAgentCall{
			{SessionID: "s1", Prompt: "queued for new era"},
		},
	}

	// Call with stale Epoch (1) when current Epoch is 2.
	result := mb.AbandonOwnershipAndPopSubmitted(1)

	require.Nil(t, result, "must return nil for stale Epoch")
	require.Equal(t, MbOwned, mb.State, "state must not change")
	require.Equal(t, uint64(2), mb.Epoch, "Epoch must not change")
	require.NotNil(t, mb.Current.Cancel, "current.cancel must not be cleared")
	require.NotNil(t, mb.DispatcherCancel, "dispatcherCancel must not be cleared")
	require.Len(t, mb.Submitted, 1, "submitted must not be touched")
	require.Equal(t, "queued for new era", mb.Submitted[0].Prompt)
}

// TestMailbox_AbandonOwnershipAndPopSubmitted_NoWork verifies that
// AbandonOwnershipAndPopSubmitted correctly transitions to MbIdle and returns
// nil (not an empty slice) when there is no queued work.
func TestMailbox_AbandonOwnershipAndPopSubmitted_NoWork(t *testing.T) {
	t.Parallel()

	mb := &Mailbox{
		State: MbOwned,
		Epoch: 1,
		Current: Generation{
			ID:     1,
			Cancel: func() {},
		},
		DispatcherCancel: func() {},
		Submitted:        []agentcall.SessionAgentCall{},
	}

	result := mb.AbandonOwnershipAndPopSubmitted(1)

	require.Nil(t, result, "must return nil for empty Queue")
	require.Equal(t, MbIdle, mb.State, "state must be MbIdle")
	require.Equal(t, uint64(1), mb.Epoch, "Epoch must be preserved")
	require.Nil(t, mb.Current.Cancel, "current.cancel must be cleared")
	require.Nil(t, mb.DispatcherCancel, "dispatcherCancel must be cleared")
	require.Len(t, mb.Submitted, 0, "submitted must be empty")
}

// TestMailbox_AbandonOwnershipAndPopSubmitted_HasWork verifies that
// AbandonOwnershipAndPopSubmitted returns all queued entries, clears the Queue,
// and transitions to MbIdle.
func TestMailbox_AbandonOwnershipAndPopSubmitted_HasWork(t *testing.T) {
	t.Parallel()

	replacementCall := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "Replacement"}
	mb := &Mailbox{
		State: MbOwned,
		Epoch: 1,
		Current: Generation{
			ID:     1,
			Cancel: func() {},
		},
		DispatcherCancel: func() {},
		Submitted: []agentcall.SessionAgentCall{
			{SessionID: "s1", Prompt: "first"},
			{SessionID: "s1", Prompt: "second"},
			{SessionID: "s1", Prompt: "third"},
		},
		Replacement: &replacementCall,
	}

	result := mb.AbandonOwnershipAndPopSubmitted(1)

	require.NotNil(t, result, "must return non-nil for non-empty Queue")
	require.Len(t, result, 4, "must return all entries including Replacement")
	require.Equal(t, "first", result[0].Prompt)
	require.Equal(t, "second", result[1].Prompt)
	require.Equal(t, "third", result[2].Prompt)
	require.Equal(t, "Replacement", result[3].Prompt, "Replacement must be folded into submitted")

	require.Equal(t, MbIdle, mb.State, "state must be MbIdle")
	require.Equal(t, uint64(1), mb.Epoch, "Epoch must be preserved")
	require.Nil(t, mb.Current.Cancel, "current.cancel must be cleared")
	require.Nil(t, mb.DispatcherCancel, "dispatcherCancel must be cleared")
	require.Len(t, mb.Submitted, 0, "submitted must be cleared")
	require.Nil(t, mb.Replacement, "Replacement must be cleared")
}

// TestMailbox_SplitCallSequence_EraBoundaryReorderingReproduces is a
// deterministic repro of the PRE-FIX bug using the still-existing, separate
// AbandonOwnership/PopAllSubmitted primitives (AbandonOwnershipAndPopSubmitted
// does not use this two-call sequence, but the old primitives themselves are
// intentionally left in place for other callers — see their doc comments).
//
// The bug: AbandonOwnership and PopAllSubmitted are two independent lock
// acquisitions. Anything that runs strictly between the two calls (a new
// Submit() becoming the owner of a fresh era, followed by a Queue() for that
// era) is invisible to PopAllSubmitted's Epoch-blind pop, so it gets scooped
// up and handed to the old era's caller instead of being left for the new
// owner's own drain. This test constructs that window explicitly (no
// goroutines needed — the "race" is just "call AbandonOwnership, then do
// other things, then call PopAllSubmitted", which is exactly the shape
// abandonOwnershipWithHandoff used to have) and confirms the leak happens,
// as documented in PopAllSubmitted's doc comment before the P2-5 fix.
func TestMailbox_SplitCallSequence_EraBoundaryReorderingReproduces(t *testing.T) {
	t.Parallel()

	mb := &Mailbox{
		State: MbOwned,
		Epoch: 1,
		Current: Generation{
			ID:     1,
			Cancel: func() {},
		},
		DispatcherCancel: func() {},
		Submitted: []agentcall.SessionAgentCall{
			{SessionID: "s1", Prompt: "old era queued work"},
		},
	}

	hadWork := mb.AbandonOwnership(1)
	require.True(t, hadWork, "old era had queued work")
	require.Equal(t, MbIdle, mb.State, "AbandonOwnership must leave MbIdle")

	// This is the exact window the bug lived in: AbandonOwnership already
	// returned, PopAllSubmitted has not run yet. A new owner lands here.
	became, newEpoch := mb.Submit(agentcall.SessionAgentCall{SessionID: "s1", Prompt: "new era first"}, func() {})
	require.True(t, became, "Submit on MbIdle must become the new owner")
	require.Equal(t, uint64(2), newEpoch)
	mb.Queue(agentcall.SessionAgentCall{SessionID: "s1", Prompt: "new era second (via Queue)"})

	popped := mb.PopAllSubmitted()

	var sawNewEraWork bool
	for _, call := range popped {
		if call.Prompt == "new era second (via Queue)" {
			sawNewEraWork = true
		}
	}
	require.True(t, sawNewEraWork,
		"documents the pre-fix bug: PopAllSubmitted grabs new-era work when "+
			"called as a separate step after AbandonOwnership — this is exactly "+
			"why abandonOwnershipWithHandoff must use the atomic "+
			"AbandonOwnershipAndPopSubmitted instead of this two-call sequence")
}

// TestMailbox_AbandonOwnershipAndPopSubmitted_EraBoundaryReorderingClosed
// stress-tests the atomic AbandonOwnershipAndPopSubmitted method with two
// genuinely concurrent goroutines (no ordering constraint between them, unlike
// a naive test that would serialize "abandon first, then Submit" and thereby
// never actually attempt to land inside any window). Because
// AbandonOwnershipAndPopSubmitted holds mb.Mu for its entire body, mb.Submit
// and mb.Queue can only ever run strictly before or strictly after it — never
// interleaved — so this test cannot deterministically force the old bug's
// window to be hit (that window no longer exists by construction). What it
// verifies is the invariant that must hold in EITHER legal interleaving:
// whichever call the mutex serializes first determines whether the new-era
// work exists yet — and it is never possible to observe a new era's own
// Queue() output smuggled into the old era's pop, across many iterations
// under -race. The real guarantee is architectural (single critical section,
// verified by code review), not statistical; this test is a sanity check
// against a future accidental re-introduction of an early unlock.
func TestMailbox_AbandonOwnershipAndPopSubmitted_EraBoundaryReorderingClosed(t *testing.T) {
	t.Parallel()

	const iterations = 500

	for i := 0; i < iterations; i++ {
		mb := &Mailbox{
			State: MbOwned,
			Epoch: 1,
			Current: Generation{
				ID:     1,
				Cancel: func() {},
			},
			DispatcherCancel: func() {},
			Submitted: []agentcall.SessionAgentCall{
				{SessionID: "s1", Prompt: "old era queued work"},
			},
		}

		var wg sync.WaitGroup
		wg.Add(2)

		var popped []agentcall.SessionAgentCall
		go func() {
			defer wg.Done()
			popped = mb.AbandonOwnershipAndPopSubmitted(1)
		}()
		go func() {
			defer wg.Done()
			// No synchronization with goroutine A: let the scheduler race
			// this against the atomic call for real. Submit() itself is a
			// no-op Queue-append (not a new owner) if it loses the race and
			// lands while mb.State is still MbOwned — either outcome is
			// legal, both are asserted below.
			mb.Submit(agentcall.SessionAgentCall{SessionID: "s1", Prompt: "new era first"}, func() {})
			mb.Queue(agentcall.SessionAgentCall{SessionID: "s1", Prompt: "new era second (via Queue)"})
		}()

		wg.Wait()

		// Whatever the interleaving, the old era's pop must always contain
		// its own pre-existing work.
		require.NotEmpty(t, popped, "old era's own queued work must be popped")
		require.Equal(t, "old era queued work", popped[0].Prompt)

		// The new-era Submit()/Queue() calls only append to mb.Submitted
		// AFTER mb.Mu serializes them relative to the atomic call — so if
		// they landed after AbandonOwnershipAndPopSubmitted's critical
		// section, their entries remain in mb.Submitted for the new owner
		// and must never appear in what the old era already popped. If they
		// landed before it (mb still MbOwned when Submit() ran), their
		// entries become part of the OLD era's own Queue (Submit() appends
		// unconditionally when not MbIdle) and legitimately belong in
		// popped — that is correct handoff behavior, not a leak, since the
		// call was genuinely submitted while the old era still owned the
		// Mailbox.
		for _, call := range popped {
			if call.Prompt == "new era second (via Queue)" {
				require.Equal(t, uint64(1), mb.Epoch,
					"new era second can only legitimately appear in the old "+
						"era's pop if Submit() never got a chance to bump the "+
						"Epoch before the atomic call ran")
			}
		}
	}
}

// TestMailbox_AbandonOwnershipAndPopFirstSubmitted_EpochMismatch mirrors
// TestMailbox_AbandonOwnershipAndPopSubmitted_EpochMismatch for the
// PopFirstSubmitted counterpart (runSummarize's manual-compaction success
// path, agent.go) added as a P2-5 follow-up: a stale Epoch must be a safe
// no-op.
func TestMailbox_AbandonOwnershipAndPopFirstSubmitted_EpochMismatch(t *testing.T) {
	t.Parallel()

	mb := &Mailbox{
		State: MbOwned,
		Epoch: 2,
		Current: Generation{
			ID:     1,
			Cancel: func() {},
		},
		DispatcherCancel: func() {},
		Submitted: []agentcall.SessionAgentCall{
			{SessionID: "s1", Prompt: "queued for new era"},
		},
	}

	call, hasNext := mb.AbandonOwnershipAndPopFirstSubmitted(1)

	require.False(t, hasNext, "must report no work for a stale Epoch")
	require.Equal(t, agentcall.SessionAgentCall{}, call)
	require.Equal(t, MbOwned, mb.State, "state must not change")
	require.Equal(t, uint64(2), mb.Epoch, "Epoch must not change")
	require.NotNil(t, mb.Current.Cancel, "current.cancel must not be cleared")
	require.Len(t, mb.Submitted, 1, "submitted must not be touched")
}

// TestMailbox_AbandonOwnershipAndPopFirstSubmitted_HasWork verifies that
// only the FIRST entry is popped, folded Replacement included when it sorts
// first by append order, and the remaining entries stay queued for the next
// owner's own drain — matching PopFirstSubmitted's existing "pop one, leave
// the rest" contract, now made Epoch-atomic.
func TestMailbox_AbandonOwnershipAndPopFirstSubmitted_HasWork(t *testing.T) {
	t.Parallel()

	mb := &Mailbox{
		State: MbOwned,
		Epoch: 1,
		Current: Generation{
			ID:     1,
			Cancel: func() {},
		},
		DispatcherCancel: func() {},
		Submitted: []agentcall.SessionAgentCall{
			{SessionID: "s1", Prompt: "first"},
			{SessionID: "s1", Prompt: "second"},
		},
	}

	call, hasNext := mb.AbandonOwnershipAndPopFirstSubmitted(1)

	require.True(t, hasNext)
	require.Equal(t, "first", call.Prompt)
	require.Equal(t, MbIdle, mb.State, "state must be MbIdle")
	require.Nil(t, mb.Current.Cancel, "current.cancel must be cleared")
	require.Nil(t, mb.DispatcherCancel, "dispatcherCancel must be cleared")
	require.Len(t, mb.Submitted, 1, "only the first entry is popped")
	require.Equal(t, "second", mb.Submitted[0].Prompt, "the rest stays queued for the next owner")
}

// TestMailbox_AbandonOwnershipAndPopFirstSubmitted_EraBoundaryReorderingClosed
// is the PopFirstSubmitted-counterpart stress test to
// TestMailbox_AbandonOwnershipAndPopSubmitted_EraBoundaryReorderingClosed
// above — see that test's doc for the reasoning on why this cannot force a
// deterministic repro (no window exists to force it into) and instead
// checks the invariant holds under real, unsynchronized concurrent racing.
func TestMailbox_AbandonOwnershipAndPopFirstSubmitted_EraBoundaryReorderingClosed(t *testing.T) {
	t.Parallel()

	const iterations = 500

	for i := 0; i < iterations; i++ {
		mb := &Mailbox{
			State: MbOwned,
			Epoch: 1,
			Current: Generation{
				ID:     1,
				Cancel: func() {},
			},
			DispatcherCancel: func() {},
			Submitted: []agentcall.SessionAgentCall{
				{SessionID: "s1", Prompt: "old era queued work"},
			},
		}

		var wg sync.WaitGroup
		wg.Add(2)

		var call agentcall.SessionAgentCall
		var hasNext bool
		go func() {
			defer wg.Done()
			call, hasNext = mb.AbandonOwnershipAndPopFirstSubmitted(1)
		}()
		go func() {
			defer wg.Done()
			mb.Submit(agentcall.SessionAgentCall{SessionID: "s1", Prompt: "new era first"}, func() {})
			mb.Queue(agentcall.SessionAgentCall{SessionID: "s1", Prompt: "new era second (via Queue)"})
		}()

		wg.Wait()

		require.True(t, hasNext, "old era's own queued work must be popped")
		require.Equal(t, "old era queued work", call.Prompt)

		if call.Prompt == "new era second (via Queue)" {
			require.Equal(t, uint64(1), mb.Epoch,
				"new era second can only legitimately appear in the old "+
					"era's pop if Submit() never got a chance to bump the "+
					"Epoch before the atomic call ran")
		}
	}
}
