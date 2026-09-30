// A11 (docs/reviews/2026-09-29-async-phase4-round1.md) at the ledger layer:
// Start captures the claim_id store.Claim mints, and commitTransition
// threads it into every store.Transition call. internal/session's own
// TestTransition_StaleClaimIDLosesAfterDeleteAndReclaim proves the store-
// level CAS mechanism; this proves the LEDGER actually wires job.claimID
// into it (not an empty/ignored value) and, just as importantly, that
// losing the CAS this way never mislabels the stale in-memory job as a false
// "failed" completion.
package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestWorkLedger_StartCapturesClaimIDMatchingTheStore pins the wiring: Start
// must record the SAME claim_id the store actually minted for this row, not
// leave it empty (which would silently degrade every future transition for
// this job back to pre-A11's "any running row matches" behavior).
func TestWorkLedger_StartCapturesClaimIDMatchingTheStore(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	store := newTestAsyncJobStore(t)
	l.store = store

	job, _, err := l.Start("owner", "call", "x", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.NotEmpty(t, job.claimID)

	row, err := store.Get(context.Background(), "owner", "call")
	require.NoError(t, err)
	require.Equal(t, row.ClaimID, job.claimID, "Start must capture the SAME claim_id the store minted for this row")
}

// TestWorkLedger_CommitTransitionLosesToFreshClaimWithoutFalseFailure pins
// A11's STORE-level backstop (the in-process half is closed earlier, by
// executor identity: TestAsyncTool_StaleExecutorAfterAckAbortCannotCommitOntoReclaimedKey;
// this row-changed-behind-the-ledger case needs an external writer): when this job's own commitTransition
// call loses because the row was deleted and re-claimed out from under it
// (claim_id mismatch, row still 'running' under the fresh claim -- NOT
// because some other cause already finished it), the stale in-memory job
// must be dropped cleanly (commitGone), never mislabeled phaseFailed with an
// empty result and delivered to the owner as a false "failed" completion.
//
// Revert-check performed: reverted the `outcome.Row.State == "running"`
// special case in commitTransition (falling through to the ordinary
// TransitionWon/TransitionLost branch for every TransitionLost) -- this test
// FAILED: the stale job was delivered with IsError=true and empty Content
// instead of never being delivered at all. Restored the special case;
// re-ran, passed. Diffed work_ledger_transition.go against git HEAD after
// restoring: matches the committed fix.
func TestWorkLedger_CommitTransitionLosesToFreshClaimWithoutFalseFailure(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 1)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	store := newTestAsyncJobStore(t)
	l.store = store

	job, _, err := l.Start("owner", "call", "x", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	staleClaimID := job.claimID
	require.NotEmpty(t, staleClaimID)

	// Simulate the row being deleted (BEFORE it was ever announced --
	// DeleteUnannounced is scoped to announced=0, ASYNC-05's abort path) and
	// re-claimed WITHOUT the ledger's own map ever finding out (the ledger's
	// abort()/Start() aren't called here on purpose -- this models a truly
	// external/detached stale executor whose own in-memory *asyncJob still
	// carries the FIRST claim, exactly like
	// TestTransition_StaleClaimIDLosesAfterDeleteAndReclaim's scenario, but
	// observed from the CURRENT process's ledger's own commitTransition call
	// instead of a raw store.Transition call).
	require.NoError(t, store.DeleteUnannounced(context.Background(), "owner", "call"))
	fresh, err := store.Claim(context.Background(), session.ClaimParams{
		Owner: "owner", ToolCallID: "call", Kind: session.JobKindCommand, Input: "x", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NotEqual(t, staleClaimID, fresh.Row.ClaimID)
	require.NoError(t, store.MarkAnnounced(context.Background(), "owner", "call"))

	outcome := l.commitTransition(job, causeNaturalFinish, jobResult{content: "stale result"})
	require.Equal(t, commitGone, outcome, "a claim_id mismatch against a still-running fresh claim must be treated like a gone row, not a false terminal outcome")

	select {
	case got := <-delivered:
		t.Fatalf("must never deliver a false completion for a stale, superseded claim: %+v", got)
	default:
	}

	row, err := store.Get(context.Background(), "owner", "call")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "the fresh claim's row must be completely untouched by the stale executor's commit attempt")

	l.mu.Lock()
	_, present := l.bySession["owner"].jobs["call"]
	l.mu.Unlock()
	require.False(t, present, "the stale in-memory job must be dropped from the ledger, not linger claiming a key it no longer owns")
}
