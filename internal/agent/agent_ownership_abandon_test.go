package agent

// Deterministic unit coverage for the abandon-handoff path behind the log
// line "calls were pending when ownership was abandoned" — the branch
// TestRunNonInteractive_ChildFailedDrainRetriedByTick hit as a 1/30 flake:
// a call (typically a completion Drain) submitted into a mailbox whose owner
// is already exiting is popped by abandonOwnershipAndPopSubmitted and handed
// to restartOrphanedWithRetry.
//
// Invariants proven here (no sleeps; the interleaving is forced by direct
// calls, which is exactly the interleaving submit/abandon's shared mb.mu
// serializes in production):
//   - a queued non-Drain call is durably enqueued EXACTLY once, and a stale
//     second abandon of the same era cannot re-enqueue it (no duplicates);
//   - a queued Drain is dropped outright — by design (phase-4 step 3, doc
//     sec.3.4): the next driver's turn-start pull re-derives it, so it must
//     not leave a durable row that would buy a redundant turn.

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func newAbandonHandoffAgent(t *testing.T) (*sessionAgent, string) {
	t.Helper()
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "abandon-handoff-test")
	require.NoError(t, err)
	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	return sa, sess.ID
}

func pendingRunQueueRowsForSession(t *testing.T, sa *sessionAgent, sessionID string) int {
	t.Helper()
	rows, err := sa.sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	n := 0
	for _, r := range rows {
		if r.SessionID == sessionID {
			n++
		}
	}
	return n
}

func TestAbandonHandoff_NonDrainOrphanEnqueuedExactlyOnce(t *testing.T) {
	sa, sessionID := newAbandonHandoffAgent(t)
	mb := sa.getMailbox(sessionID)

	became, epoch := mb.submit(SessionAgentCall{SessionID: sessionID, Prompt: "owner turn"}, nil)
	require.True(t, became)

	// A call queued by a concurrent submit while this Run still owned the
	// mailbox — the flake's exact shape, forced instead of raced.
	orphan := SessionAgentCall{
		SessionID:     sessionID,
		Prompt:        "queued follow-up",
		LogicalCallID: uuid.NewString(),
	}
	became, _ = mb.submit(orphan, nil)
	require.False(t, became, "the submit must queue behind the owner")

	sa.abandonOwnershipWithHandoff(sessionID, epoch)
	require.Equal(t, mbIdle, mb.state)
	require.Equal(t, 1, pendingRunQueueRowsForSession(t, sa, sessionID),
		"the orphaned call must be durably enqueued exactly once")

	// A stale second abandon of the same era (Run's defer firing after the
	// era legitimately ended) must be a no-op, not a second enqueue.
	sa.abandonOwnershipWithHandoff(sessionID, epoch)
	require.Equal(t, 1, pendingRunQueueRowsForSession(t, sa, sessionID),
		"a stale abandon must not create a duplicate durable row")
}

func TestAbandonHandoff_QueuedDrainDroppedWithoutDurableRow(t *testing.T) {
	sa, sessionID := newAbandonHandoffAgent(t)
	mb := sa.getMailbox(sessionID)

	became, epoch := mb.submit(SessionAgentCall{SessionID: sessionID, Prompt: "owner turn"}, nil)
	require.True(t, became)

	became, _ = mb.submit(SessionAgentCall{
		SessionID:     sessionID,
		IsDrain:       true,
		LogicalCallID: uuid.NewString(),
	}, nil)
	require.False(t, became)

	sa.abandonOwnershipWithHandoff(sessionID, epoch)
	require.Equal(t, mbIdle, mb.state)
	require.Equal(t, 0, pendingRunQueueRowsForSession(t, sa, sessionID),
		"an orphaned Drain must be dropped, never durably enqueued: the next turn-start pull re-derives it")
}
