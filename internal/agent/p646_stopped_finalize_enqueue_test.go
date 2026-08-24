package agent

// Task #646 (N-3 outcome (a)): prove the AGENT-level behavior that
// drainOrReleaseMerged durably enqueues work that raced into the
// release window on a hard-stopped mailbox. This mirrors p641_mbstopped_rebind_test.go's
// machinery (testEnv, enqueueRecordingSessions wrapper pattern, countingModel) to test
// the full agent path from drainOrReleaseFinal's stopped branch through restartOrphaned
// to the durable session_run_queue.

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAgent_DrainOrReleaseMerged_StoppedFinalizeEnqueuesOrphanedWork(t *testing.T) {
	env := testEnv(t)
	rec := &enqueueRecordingSessions{Service: env.sessions}
	lm := &countingModel{}
	agentIface := NewSessionAgent(SessionAgentOptions{
		SmartModel:     Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		FastModel:      Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SystemPrompt:   "test",
		IsYolo:         true,
		Sessions:       rec,
		Messages:       env.messages,
		CancelAllGrace: 200 * time.Millisecond,
	})
	sa := agentIface.(*sessionAgent)

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "p646 stopped finalize enqueue")
	require.NoError(t, err)
	sessionID := sess.ID

	// Acquire ownership of the mailbox as if we're in a turn loop
	becomeOwner, epoch := sa.getMailbox(sessionID).submit(SessionAgentCall{
		SessionID:       sessionID,
		Prompt:          "first turn",
		MaxOutputTokens: 1000,
	}, func() {})
	require.True(t, becomeOwner)

	mb := sa.getMailbox(sessionID)

	// Verify the mailbox is in the expected state
	mb.mu.Lock()
	require.Equal(t, mbOwned, mb.state, "mailbox must be owned after submit")
	require.Equal(t, uint64(1), epoch, "epoch must be 1 after submit")
	mb.mu.Unlock()

	// Simulate work racing in after hardStop is latched but before the
	// finalize step runs. We latch stopped first, then queue work, to
	// test the "stopped already latched at entry" path through drainOrReleaseFinal.
	mb.mu.Lock()
	mb.stopped = true
	// Queue a call with a unique LogicalCallID we can verify in the durable enqueue
	queuedCall := SessionAgentCall{
		SessionID:       sessionID,
		Prompt:          "queued during shutdown",
		LogicalCallID:   "p646-queued-12345",
		MaxOutputTokens: 1000,
	}
	mb.submitted = append(mb.submitted, queuedCall)
	mb.replacement = nil
	mb.mu.Unlock()

	// Call drainOrReleaseMerged with a nil lock (no real OS lock, so
	// release() is a no-op). This is the agent-level path that handles
	// the orphaned work returned by drainOrReleaseFinal's finalize step.
	//
	// Because stopped was already latched at entry, drainOrReleaseFinal
	// will skip the live replacement/submitted branches (they're inside
	// `if !mb.stopped`), fall through to the releasing path, and then
	// in the finalize step hand out the queued work as orphaned (our
	// #646 fix). drainOrReleaseMerged then calls restartOrphaned, which
	// durably enqueues via the Sessions service.
	next, hasNext := sa.drainOrReleaseMerged(sessionID, epoch, nil, func() {})

	// Assertions: no next turn, hasNext false (shutdown must not run
	// another turn in-process)
	require.Equal(t, SessionAgentCall{}, next, "no call should be returned for the next turn")
	require.False(t, hasNext, "hasNext must be false — a shutdown must not start a fresh turn in-process")

	// Verify the mailbox state after the drain
	mb.mu.Lock()
	state := mb.state
	stopped := mb.stopped
	queued := len(mb.submitted)
	mb.mu.Unlock()
	require.Equal(t, mbIdle, state, "mailbox must end at mbIdle after drain")
	require.True(t, stopped, "the stopped latch must remain set")
	require.Equal(t, 0, queued, "the queued work must have been drained out as orphaned")

	// THE #646 assertions: the recording service must have seen exactly
	// one EnqueueRunQueueEntry call with the queued call's LogicalCallID,
	// proving that drainOrReleaseMerged durably enqueued the orphaned work
	// rather than discarding it.
	rec.mu.Lock()
	keys := append([]string(nil), rec.keys...)
	calls := append([]session.SessionAgentCallData(nil), rec.calls...)
	rec.mu.Unlock()

	require.Len(t, keys, 1, "exactly one durable enqueue must have occurred")
	require.Contains(t, keys[0], "p646-queued-12345", "the idempotency key must contain the queued call's LogicalCallID")

	// Verify the call data was correctly marshaled and contains our LogicalCallID
	require.Len(t, calls, 1)
	require.Equal(t, "queued during shutdown", calls[0].Prompt)
	require.Equal(t, "p646-queued-12345", calls[0].LogicalCallID)

	// Prove no provider turn started: countingModel should be at zero
	require.Zero(t, lm.calls.Load(), "no provider turn may start on this path — only a durable enqueue")
}
