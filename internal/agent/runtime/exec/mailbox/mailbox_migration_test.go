package mailbox

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/stretchr/testify/require"
)

// TestMailbox_Queue_AndDrainOrReleaseFinal_SingleExecution is the core #308
// regression test: a message queued via Mailbox.Queue() (the new
// QueueMessage path) during a busy session must be returned by
// DrainOrReleaseFinal EXACTLY ONCE — never twice. The pre-#308 risk was
// that a message could end up in BOTH mb.Submitted (via the new path) and
// the legacy messageQueue (via the old path), causing DrainOrReleaseFinal's
// submitted branch AND its checkLegacy branch to each return it, running
// the message as two separate turns. With the legacy Queue removed, there
// is only one Queue and one branch.
func TestMailbox_Queue_AndDrainOrReleaseFinal_SingleExecution(t *testing.T) {
	mb := &Mailbox{
		State:            MbOwned,
		Epoch:            1,
		DispatcherCancel: func() {},
		Current:          Generation{ID: 1, Cancel: func() {}},
	}

	queued := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "queued exactly once"}
	mb.Queue(queued)

	// First drain: should find the entry and return it.
	next, hasNext, _, _ := mb.DrainOrReleaseFinal(1, nil)
	require.True(t, hasNext, "the queued entry must be returned on the first drain")
	require.Equal(t, queued, next)

	// Second drain: submitted is now empty — no legacy Queue to check.
	// This MUST return hasNext=false. If the entry had been double-inserted
	// (the exact bug class this migration closes), a second drain would find
	// it again.
	next2, hasNext2, _, _ := mb.DrainOrReleaseFinal(1, nil)
	require.False(t, hasNext2, "the queued entry must NOT be returned a second time — single insertion, single execution")
	require.Equal(t, agentcall.SessionAgentCall{}, next2)
}

// TestMailbox_Queue_AbandonOwnership_SurvivesForNextOwner verifies that
// work queued via Mailbox.Queue() survives an AbandonOwnership call (which
// now leaves entries in submitted instead of draining them to a legacy
// Queue). The next owner that calls Submit() + DrainOrReleaseFinal picks
// them up.
func TestMailbox_Queue_AbandonOwnership_SurvivesForNextOwner(t *testing.T) {
	mb := &Mailbox{
		State:            MbOwned,
		Epoch:            1,
		DispatcherCancel: func() {},
		Current:          Generation{ID: 1, Cancel: func() {}},
	}

	queued := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "survives AbandonOwnership"}
	mb.Queue(queued)

	// Simulate Run's cleanup defer: AbandonOwnership ends the era at MbIdle
	// but leaves submitted populated.
	hadWork := mb.AbandonOwnership(1)
	require.True(t, hadWork)
	require.Equal(t, MbIdle, mb.State)
	require.Len(t, mb.Submitted, 1, "the entry must survive in submitted")

	// A new owner arrives.
	becomeOwner, epoch2 := mb.Submit(agentcall.SessionAgentCall{SessionID: "s1", Prompt: "new owner"}, func() {})
	require.True(t, becomeOwner)
	require.NotEqual(t, uint64(1), epoch2)

	// The new owner's end-of-turn drain must find the old entry.
	next, hasNext, _, _ := mb.DrainOrReleaseFinal(epoch2, nil)
	require.True(t, hasNext, "the surviving entry must be returned to the new owner")
	require.Equal(t, queued, next)
}

// TestSessionAgent_ClearQueue_ClearsMailboxOnly verifies ClearQueue clears
// all queued work from the Mailbox. Since #308 removed the legacy
// messageQueue, ClearQueue's only job is clearing the Mailbox's submitted/
// Replacement/Injects via ClearAll().

// TestSessionAgent_QueuedPrompts_NoDoubleCount verifies that QueuedPrompts
// and QueuedPromptsList count each queued message exactly once. Before #308,
// these methods summed counts across both the legacy messageQueue and the
// Mailbox's submitted Queue — a message that appeared in both would be
// counted twice. With the legacy Queue gone, there is only one source.

// TestMailbox_ReclaimReplacementOrKeep_StillPushesToFrontAfterMigration
// verifies that ReclaimReplacementOrKeep (#307) continues to return the
// displaced call to the FRONT of mb.Submitted after the #308 migration
// removed the legacy messageQueue. The submitted Queue is now the ONLY
// Queue; if ReclaimReplacementOrKeep failed to re-Queue the displaced call,
// it would be silently destroyed — the same #283/P0-2 class of bug.
func TestMailbox_ReclaimReplacementOrKeep_StillPushesToFrontAfterMigration(t *testing.T) {
	callA := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "A - about to be pre-empted"}
	callB := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "B - already queued"}
	callD := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "D - the interrupt's Replacement"}
	mb := &Mailbox{
		Replacement: &callD,
		Submitted:   []agentcall.SessionAgentCall{callB},
	}

	got := mb.ReclaimReplacementOrKeep(callA)

	require.Equal(t, callD.SessionID, got.SessionID, "D (the Replacement) must be returned to run next")
	require.Equal(t, callD.Prompt, got.Prompt)
	require.True(t, got.ReplacementHandoff)
	require.Nil(t, mb.Replacement, "Replacement must be consumed")
	require.Equal(t, []agentcall.SessionAgentCall{callA, callB}, mb.Submitted,
		"A must be pushed to the FRONT of submitted (ahead of B), preserving FIFO order — "+
			"with the legacy Queue gone, submitted is the ONLY Queue; dropping A here would lose it permanently")
}

// TestRun_QueuedMessageDuringBusySession_RunsExactlyOnce is the end-to-end
// integration test for the #308 migration's core promise: a message queued
// via QueueMessage during a busy session executes EXACTLY ONCE (as exactly
// one additional turn), not zero times (stranded) and not twice
// (double-inserted). It replaces the former
// TestRun_LegacyQueueMessageDuringFinalDrain_DoesNotWedgeAcrossTurns's
// direct messageQueue.Append with the public QueueMessage API, proving the
// full public path works after the migration.
