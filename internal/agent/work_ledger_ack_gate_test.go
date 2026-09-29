// The ack gate's fused-transaction coverage (DUR-7, docs/plans/2026-09-28-
// async-phase4-durable-core.md sec.3.8, step 6): acknowledgeWithMessageTx
// commits the "started" tool-result message and announced=1 together, aborts
// cleanly on failure, and still delivers exactly once for a job that races
// to terminal before its own ack -- the same wake-hint path
// TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce already pins for
// delegations, exercised here for the ack gate's OWN fused write.
package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// startedParams is a minimal stand-in for onToolResult's real
// message.CreateMessageParams -- content does not matter to these tests,
// only that it is the fused transaction's own insert.
func startedParams(text string) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	}
}

// TestWorkLedger_AckGate_FusesMessageAndAnnouncedInOneTransaction pins the
// happy path: the message lands in history AND announced=1 commits together.
func TestWorkLedger_AckGate_FusesMessageAndAnnouncedInOneTransaction(t *testing.T) {
	t.Parallel()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	messages := message.NewService(db.New(conn))
	l := newWorkLedger(nil)
	l.store = store

	_, _, err := l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)

	msg, handled, err := l.acknowledgeWithMessageTx(context.Background(), "owner-1", "call-1", messages, startedParams("started"))
	require.NoError(t, err)
	require.True(t, handled)
	require.NotEmpty(t, msg.ID)

	row, err := store.Get(context.Background(), "owner-1", "call-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, row.Announced)

	got, err := messages.Get(context.Background(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "started", got.Content().Text)
}

// TestWorkLedger_AckGate_ErrAsyncJobGoneRollsBackAndFallsBackToPlainCreate
// pins B15 two ways at once: (1) genuine atomicity of the fused write -- the
// happy-path test above only checks the END state (message exists AND
// announced=1), which would ALSO pass for two separate, non-transactional
// writes; this test fails the SECOND half (MarkAsyncJobAnnounced, via a row
// deleted out from under the in-memory job -- the real "a Rerun truncation
// raced this call" shape) and proves the FIRST half (the message insert)
// rolled back too, which only a single shared transaction guarantees.
// (2) the caller-visible contract: handled must be false (not true-with-
// swallowed-error) so onToolResult falls through to its own plain Create --
// otherwise the tool_use this result answers gets no tool_result at all,
// which every provider rejects on the next turn.
//
// Revert-check performed: reverted acknowledgeWithMessageTx's ErrAsyncJobGone
// branch to `return message.Message{}, true, nil` -- this test FAILED
// (handled was true, so the fallback Create below never ran and msgs stayed
// empty). Restored the false-return version; re-ran, passed. Diffed
// work_ledger_announce.go against git HEAD after restoring: matches the
// committed fix.
func TestWorkLedger_AckGate_ErrAsyncJobGoneRollsBackAndFallsBackToPlainCreate(t *testing.T) {
	t.Parallel()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	messages := message.NewService(db.New(conn))
	l := newWorkLedger(nil)
	l.store = store

	_, _, err := l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)

	// Simulate "a Rerun truncation raced this call": the DURABLE row is gone
	// (deleted directly at the store level, bypassing the ledger's own
	// abort() so the IN-MEMORY job entry survives, exactly as a concurrent
	// Rerun would leave it -- see acknowledgeWithMessageTx's own doc).
	require.NoError(t, store.DeleteUnannounced(context.Background(), "owner-1", "call-1"))

	msg, handled, err := l.acknowledgeWithMessageTx(context.Background(), "owner-1", "call-1", messages, startedParams("started"))
	require.NoError(t, err)
	require.False(t, handled, "ErrAsyncJobGone must fall back to the caller's own plain Create, not swallow the result")
	require.Empty(t, msg.ID)

	// Atomicity: the message the fused tx staged must NOT have survived --
	// only a single shared transaction with MarkAsyncJobAnnounced guarantees
	// this; two independent writes would have left it behind.
	msgs, err := messages.List(context.Background(), "owner-1")
	require.NoError(t, err)
	require.Empty(t, msgs, "the fused transaction's own message insert must roll back with the failed announce write")

	// The caller's own fallback (onToolResult's unchanged plain path):
	// exactly one message must end up persisted.
	created, err := messages.Create(context.Background(), "owner-1", startedParams("started"))
	require.NoError(t, err)
	l.acknowledged("owner-1", "call-1") // tolerates the still-gone row (MarkAnnounced's own ErrAsyncJobGone handling)

	msgs, err = messages.List(context.Background(), "owner-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1, "exactly one tool-result message must exist after the fallback -- never zero (orphaned tool_use), never two")
	require.Equal(t, created.ID, msgs[0].ID)
}

// TestWorkLedger_AckGate_SyncAndUntrackedCallsAreNotHandled pins the
// fallback contract: a sync job (announced=true already, no DB row) and an
// ordinary tool call with no ledger entry at all must both come back
// handled=false, so onToolResult's plain messages.Create path runs
// unchanged.
func TestWorkLedger_AckGate_SyncAndUntrackedCallsAreNotHandled(t *testing.T) {
	t.Parallel()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	messages := message.NewService(db.New(conn))
	l := newWorkLedger(nil)
	l.store = store

	_, handled, err := l.acknowledgeWithMessageTx(context.Background(), "owner-1", "no-such-call", messages, startedParams("x"))
	require.NoError(t, err)
	require.False(t, handled, "an ordinary tool call with no ledger entry must not be handled by the fused path")

	_, _, err = l.Start("owner-1", "sync-call", "", "bash", "", false, true, nil, func() {})
	require.NoError(t, err)
	_, handled, err = l.acknowledgeWithMessageTx(context.Background(), "owner-1", "sync-call", messages, startedParams("x"))
	require.NoError(t, err)
	require.False(t, handled, "a sync job has no durable row to fuse with")
}

// TestWorkLedger_AckGate_FailedTransactionDeletesRowAndNeverProducesANotice
// pins DUR-7's failure half: if the fused transaction fails, NEITHER the
// message NOR announced=1 persists, and the row is deleted (ASYNC-05) via
// the same abort path a failed plain Create used before this gate existed --
// so an unannounced job never produces a notice.
//
// Revert-check performed: changed acknowledgeWithMessageTx to swallow the
// AnnounceStarted error instead of calling l.abort on it. This test FAILED
// (the row survived in the store, announced=0, running forever). Restored
// the abort call; re-ran, passed. Diffed work_ledger_announce.go against
// git HEAD after restoring: matches the committed step-6 code.
func TestWorkLedger_AckGate_FailedTransactionDeletesRowAndNeverProducesANotice(t *testing.T) {
	t.Parallel()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	real := message.NewService(db.New(conn))
	failing := &failingCreateTxMessages{Service: real}
	failing.fail.Store(true)
	l := newWorkLedger(nil)
	l.store = store

	var cancelled bool
	_, _, err := l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() { cancelled = true })
	require.NoError(t, err)

	_, handled, err := l.acknowledgeWithMessageTx(context.Background(), "owner-1", "call-1", failing, startedParams("started"))
	require.True(t, handled)
	require.Error(t, err)

	_, err = store.Get(context.Background(), "owner-1", "call-1")
	require.ErrorIs(t, err, sql.ErrNoRows, "the row must be deleted, never left announced=0 forever")
	require.False(t, l.running("owner-1"), "the ledger must drop the job too")
	require.True(t, cancelled, "abort must cancel the job's executor")

	msgs, err := real.List(context.Background(), "owner-1")
	require.NoError(t, err)
	require.Empty(t, msgs, "no message must survive a failed fused transaction")
}

// TestWorkLedger_AckGate_FastJobFinishingBeforeAckDeliversExactlyOnceAtAck
// pins doc sec.3.8's Ack gate paragraph: a job whose terminal transition
// commits BEFORE its own "started" result is announced must not deliver
// (and must not wake anyone) until the ack itself runs -- the SAME
// deliverLocked guard TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce
// pins for delegations, exercised here for a PLAIN job through the fused
// ack-gate path. Exactly one delivery, and it happens at acknowledgment.
func TestWorkLedger_AckGate_FastJobFinishingBeforeAckDeliversExactlyOnceAtAck(t *testing.T) {
	t.Parallel()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	messages := message.NewService(db.New(conn))
	delivered := make(chan AsyncCompletion, 2)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	l.store = store

	_, _, err := l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)

	// The job finishes naturally BEFORE its own "started" result is ever
	// announced -- deliverLocked's `!job.announced` guard must withhold
	// delivery here.
	l.finish("owner-1", "call-1", jobResult{content: "done"})
	require.Empty(t, drainCompletions(delivered), "must not deliver before the started result is announced")
	require.True(t, l.running("owner-1"), "the terminal-but-unannounced job must still be present")

	msg, handled, err := l.acknowledgeWithMessageTx(context.Background(), "owner-1", "call-1", messages, startedParams("Async bash job call-1 started."))
	require.NoError(t, err)
	require.True(t, handled)
	require.NotEmpty(t, msg.ID)

	got := drainCompletions(delivered)
	require.Len(t, got, 1, "exactly one delivery, at the ack, not before and not twice")
	require.Equal(t, "done", got[0].Content)

	// The "started" message must exist and, being the only message written
	// so far, necessarily precede whatever notice a later driver pull would
	// insert for this same row's outcome.
	msgs, err := messages.List(context.Background(), "owner-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "Async bash job call-1 started.", msgs[0].Content().Text)
}
