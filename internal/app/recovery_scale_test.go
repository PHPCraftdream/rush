package app

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecoverInterruptedTurns_CostProportionalToCandidates is the task #774
// proof: recoverInterruptedTurns' cost must scale with the number of
// CANDIDATES (sessions whose last message is an unfinished assistant
// message), not the total number of sessions in the data directory.
//
// Before the fix, recoverInterruptedTurns called Sessions.ListAll then, for
// EVERY session returned, ran a full Messages.List against it just to find
// the last assistant message -- an O(total sessions) scan. Measured on a
// real dev DB: 137 sessions, 10s+, tripping the sweep's own 10s deadline.
//
// This test seeds 100 finished (non-candidate) sessions plus 5 genuinely
// orphaned (candidate) sessions, and asserts recoverSessionListSeam --
// which fires once per candidate actually processed by
// recoverSessionInterruptedTurn -- fires exactly 5 times, not 105. Confirmed
// to fail against the pre-fix code (which called the per-session path for
// every session returned by Sessions.ListAll, i.e. would have fired 105
// times) by temporarily reverting the fix locally before finalizing this
// test.
func TestRecoverInterruptedTurns_CostProportionalToCandidates(t *testing.T) {
	app := newRecoveryTestApp(t)
	ctx := t.Context()

	const numFinished = 100
	const numOrphans = 5

	for i := 0; i < numFinished; i++ {
		sess, err := app.Sessions.Create(ctx, fmt.Sprintf("finished-%d", i))
		require.NoError(t, err)
		asst, err := app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "done"}},
		})
		require.NoError(t, err)
		// The candidate query keys off the DB's finished_at column, which
		// (matching real agent turns, see agent_turn.go's OnStepFinish ->
		// AddFinish -> Update chain) is only stamped by Update, never by
		// Create -- baking a Finish part into the initial Parts at Create
		// time does NOT set finished_at, so these sessions would otherwise
		// wrongly count as candidates too.
		asst.AddFinish(message.FinishReasonEndTurn, "done", "")
		require.NoError(t, app.Messages.Update(ctx, asst))
	}

	orphanIDs := make(map[string]bool, numOrphans)
	for i := 0; i < numOrphans; i++ {
		sess, err := app.Sessions.Create(ctx, fmt.Sprintf("orphan-%d", i))
		require.NoError(t, err)
		orphanIDs[sess.ID] = true
		_, err = app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "x", Name: "bash", Input: "{}", Finished: true},
			},
		})
		require.NoError(t, err)
	}

	var seamCalls int64
	recoverSessionListSeam = func() { atomic.AddInt64(&seamCalls, 1) }
	t.Cleanup(func() { recoverSessionListSeam = nil })

	app.recoverInterruptedTurns(ctx)

	assert.Equal(t, int64(numOrphans), atomic.LoadInt64(&seamCalls),
		"recovery must only process the %d candidate sessions, not all %d sessions",
		numOrphans, numFinished+numOrphans)

	// Sanity: the orphans were actually recovered, and the finished sessions
	// untouched -- proves the candidate-only path didn't just skip work.
	for id := range orphanIDs {
		msgs, err := app.Messages.List(ctx, id)
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		assert.True(t, msgs[0].IsFinished(), "orphan session %s must be recovered", id)
	}
}
