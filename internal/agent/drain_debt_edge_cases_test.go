// DUR-4 edge cases (Ф4-4 review, doc sec.6): compaction must not clear
// unreacted debt, an independent pull by another lock holder must not erase
// it either, a step-finish write failure must leave it intact for exactly
// one more attempt, and an operator interrupt landing on a Drain must still
// reach the provider. Real SQLite + a real *sessionAgent throughout.
package agent

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestCompaction_DoesNotClearUnreactedDebt complements TestCompaction_
// DoesNotPullNotices (which pins "never pulls a pending row"): here the
// notice is ALREADY pulled (delivery='done', reacted=0) before compaction
// runs, proving compaction's own summary write does not go through
// persistStepFinish/MarkReactedWithMessageUpdate and so can never
// accidentally mark it reacted.
//
// REVERT CHECK: temporarily made runSummarizeBody's final summary-message
// write (agent_compaction.go) call store.MarkReactedWithMessageUpdate
// instead of the plain messages.Update -- this test's second
// `require.True(t, f.debtVisible(...))` FAILED (the notice was marked
// reacted by the compaction summary). Restored the plain write path
// (byte-identical diff confirmed); re-ran, passed.
func TestCompaction_DoesNotClearUnreactedDebt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWakeDebtFixture(t, "compaction-debt")
	// Compaction needs SOMETHING to summarize, or runSummarizeBody no-ops
	// before ever reaching its own message write.
	_, err := f.messages.Create(ctx, f.sessID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "earlier work"}},
	})
	require.NoError(t, err)
	f.claimAndFinish(t, ctx, "call-1")
	_, err = f.store.PullJobNotices(ctx, f.messages, f.sessID, buildJobNoticeMessageParams)
	require.NoError(t, err)
	require.True(t, f.debtVisible(t, ctx))

	require.NoError(t, f.sa.runSummarizeBody(ctx, f.sessID, nil, f.model, ""))

	require.True(t, f.debtVisible(t, ctx), "a compaction summary must never clear a notice's own debt")
	job, err := f.store.Get(ctx, f.sessID, "call-1")
	require.NoError(t, err)
	require.EqualValues(t, 0, job.Reacted)
}

// TestAnotherHolderPulling_DoesNotEraseDebt_RootStillReacts pins doc
// sec.3.4: "moving notices into history by another lock holder does not
// erase debt" -- a web tab merely pulling a `rush run` session's notices
// (any session-lock holder can call the pull) leaves the debt exactly as
// open as it was; the legitimate leader (here, a later wakeSession call)
// still reacts to it normally.
//
// REVERT CHECK: temporarily made wakeSession skip the debt-then-policy path
// entirely for an already-visible-debt session (returning nil right after
// bumpHint, mirroring the external-driver early-return) -- this test's
// `require.NotZero(t, f.requests.Load())` FAILED (no turn ran despite real
// debt). Restored the normal path; re-ran, passed. The precondition itself
// (`require.True(t, f.debtVisible...)` right after the independent pull) is
// exactly what proves the pull alone never sets reacted -- see
// PullPendingAsyncJobNotice's own SQL (sql/async_jobs.sql), which only ever
// touches `delivery`, never `reacted`.
func TestAnotherHolderPulling_DoesNotEraseDebt_RootStillReacts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWakeDebtFixture(t, "another-holder-pulls")
	f.claimAndFinish(t, ctx, "call-1")

	// "A web tab" pulls the notice into history directly -- NOT through
	// wakeSession/the Drain machinery at all, just the raw pull primitive
	// any session-lock holder can call.
	pulled, err := f.store.PullJobNotices(ctx, f.messages, f.sessID, buildJobNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	require.True(t, f.debtVisible(t, ctx), "an independent pull must not erase the debt it just made visible")

	// The legitimate leader (a wakeSession call, standing in for `rush run`'s
	// own loop reacting to its hint) still runs a real turn over it.
	err = f.coord.wakeSession(ctx, jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.NoError(t, err)
	require.NotZero(t, f.requests.Load(), "the root must still get a real turn despite another holder's pull")
	require.False(t, f.debtVisible(t, ctx), "the root's own turn must react and clear the debt")
}

// failingThenOKUpdateTxMessages fails UpdateTx exactly once, then delegates
// to the real service -- simulates fantasy discarding/surfacing an
// OnStepFinish write failure (doc sec.6: "the debt stays, exactly one more
// Drain, no loop").
type failingThenOKUpdateTxMessages struct {
	message.Service
	remaining atomic.Int32
}

func (f *failingThenOKUpdateTxMessages) UpdateTx(ctx context.Context, tx *sql.Tx, msg message.Message) (func(), error) {
	if f.remaining.Add(-1) >= 0 {
		return nil, errors.New("simulated OnStepFinish write failure")
	}
	return f.Service.UpdateTx(ctx, tx, msg)
}

// TestStepFinishWriteFailure_DebtStaysThenOneMoreDrainClearsIt pins doc
// sec.3.4/6 verbatim: "if the step finish is not written (fantasy discards
// the OnStepFinish error, ...) the debt remains and there will be one more
// Drain call -- acceptable". fantasy does NOT surface a bare OnStepFinish
// error as a Run()-level failure (agent_turn_step.go's own onStepFinish doc:
// "returning an error from OnStepFinish alone does NOT break fantasy's
// loop") -- so wakeSession's call still reports success even though the
// reacted marker was never durably written. What DOES have to hold, and
// what this test actually pins, is that the debt itself survives that
// silent failure intact (MarkReactedWithMessageUpdate's one-transaction
// property: a failed write there rolls back the message update too, so
// nothing about this step is half-committed), and exactly one more Drain
// (not a chain) resolves it once the write succeeds.
//
// REVERT CHECK: changed MarkReactedWithMessageUpdate (async_job_reaction.go)
// to commit the reacted-marking queries in their OWN transaction, separate
// from messages.UpdateTx's -- this test's `require.True(t, f.debtVisible)`
// right after the first (failing) wakeSession call FAILED (the debt was
// gone: the reacted-marking half committed independently even though the
// message write it was supposed to be atomic with had failed). Restored the
// single shared transaction; re-ran, passed.
func TestStepFinishWriteFailure_DebtStaysThenOneMoreDrainClearsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWakeDebtFixture(t, "step-finish-write-failure")
	failing := &failingThenOKUpdateTxMessages{Service: f.messages.Service}
	failing.remaining.Store(1) // fail exactly the first UpdateTx call
	f.messages.Service = failing
	f.claimAndFinish(t, ctx, "call-1")
	_, err := f.store.PullJobNotices(ctx, f.messages, f.sessID, buildJobNoticeMessageParams)
	require.NoError(t, err)
	require.True(t, f.debtVisible(t, ctx))

	err = f.coord.wakeSession(ctx, jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.NoError(t, err, "fantasy discards a bare OnStepFinish error -- wakeSession must not surface one either")
	require.True(t, f.debtVisible(t, ctx), "the debt must survive an unwritten step-finish")

	// Exactly one more Drain, now that the write succeeds, resolves it.
	err = f.coord.wakeSession(ctx, jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.NoError(t, err)
	require.False(t, f.debtVisible(t, ctx), "the very next attempt must clear the debt -- no further chain needed")
}

// TestInterruptDuringDrain_OperatorMessageReachesProvider complements
// drain_interrupt_test.go's construction-only proofs
// (TestHandleInterruptTick_DrainActiveCallNotInherited et al: the
// replacement call is never itself a Drain) with an end-to-end run: the
// operator's interrupt message, built via callFromActive from an ACTIVE
// Drain call exactly like handleInterruptTick does, actually reaches the
// provider when run.
//
// REVERT CHECK: changed callFromActive to `return active` unmodified
// (inheriting IsDrain/AutoResumed/BackgroundJobNotice) -- this test FAILED
// (`result` was nil: with IsDrain still true and no debt/pull to react to,
// runTurn took the no-provider-call branch instead of running the
// operator's message as a real turn). Restored callFromActive's reset
// (byte-identical diff confirmed); re-ran, passed.
func TestInterruptDuringDrain_OperatorMessageReachesProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var mu sync.Mutex
	var sawOperatorPrompt bool
	f := newWakeDebtFixtureWithHandler(t, "interrupt-during-drain", func(body []byte) {
		mu.Lock()
		defer mu.Unlock()
		if bytes.Contains(body, []byte("operator interrupt: please check on this")) {
			sawOperatorPrompt = true
		}
	})

	activeDrain := newDrainCall(SessionAgentCall{SessionID: f.sessID, NoticeKind: "supervision"})
	replacement := callFromActive(activeDrain)
	replacement.Prompt = "operator interrupt: please check on this"

	result, err := f.sa.Run(ctx, replacement)
	require.NoError(t, err)
	require.NotNil(t, result)
	mu.Lock()
	defer mu.Unlock()
	require.True(t, sawOperatorPrompt, "the operator's message must reach the provider as a real turn, not be dropped as a Drain")
}
