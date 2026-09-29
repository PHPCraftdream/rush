// A11 (docs/reviews/2026-09-29-async-phase4-round1.md): the terminal-
// transition CAS keyed only on (owner_session_id, tool_call_id, state=
// 'running') is vulnerable to ABA -- a row deleted then re-claimed under the
// SAME key lets a stale executor's late result commit onto the NEW claim.
// claim_id closes it: minted once per Claim, carried by the caller, and
// matched by Transition's CAS.
package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTransition_StaleClaimIDLosesAfterDeleteAndReclaim reproduces the ABA
// scenario the migration's own doc describes: E1 claims R1 (claim_id C1), R1
// is deleted (ASYNC-05's abort path -- the "started" write failed), the SAME
// (owner, tool_call_id) is claimed again by E2 (R2, claim_id C2, DIFFERENT
// from C1 since it is a fresh row), and E1 -- still running because context
// cancellation is best-effort, not instantaneous -- finally reports its late
// result, still carrying C1. Without claim_id in the CAS, E1's UPDATE ...
// WHERE state='running' would match R2 (same key, also running) and silently
// overwrite E2's still-live job with E1's stale content.
//
// Revert-check performed: temporarily reverted TransitionAsyncJobTerminalPreserveVoid's
// WHERE clause to drop "AND claim_id = ?" (and Transition to stop passing it)
// -- this test FAILED (E1's stale Transition call returned TransitionWon,
// overwriting R2 with "E1's stale result"). Restored the claim_id predicate;
// re-ran, passed. Diffed internal/db/sql/async_jobs.sql and
// internal/session/async_job_store.go against git HEAD after restoring:
// match the committed fix.
func TestTransition_StaleClaimIDLosesAfterDeleteAndReclaim(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	claim1, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	staleClaimID := claim1.Row.ClaimID
	require.NotEmpty(t, staleClaimID, "Claim must mint a real claim_id")

	// R1 deleted (ASYNC-05's abort path: the "started" write failed).
	require.NoError(t, store.DeleteUnannounced(ctx, "owner-1", "call-1"))

	// A fresh claim for the SAME key: R2, with a DIFFERENT claim_id.
	claim2, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NotEqual(t, staleClaimID, claim2.Row.ClaimID, "a fresh claim of a re-used key must mint its OWN claim_id")
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))

	// E1 -- the stale executor from the FIRST claim -- finally reports its
	// late result, carrying claim_id C1.
	lostResult, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "completed",
		ResultSummary: "E1's stale result", Wake: true, ClaimID: staleClaimID,
	})
	require.NoError(t, err)
	require.Equal(t, TransitionLost, lostResult.Outcome, "a stale claim_id must lose the CAS even though state='running' still matches")

	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "R2 must be untouched by E1's stale commit -- still running, ready for E2's own result")

	// E2's own (current) claim_id must still win normally.
	wonResult, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "completed",
		ResultSummary: "E2's real result", Wake: true, ClaimID: claim2.Row.ClaimID,
	})
	require.NoError(t, err)
	require.Equal(t, TransitionWon, wonResult.Outcome)
	require.Equal(t, "E2's real result", wonResult.Row.ResultSummary.String)
}

// TestTransition_EmptyClaimIDResolvesCurrentRow pins the backward-compat
// half: a caller with no claim_id of its own (recovery, or a test seeding
// state directly, as every OTHER TransitionParams{} literal in this package
// already does) must keep its exact pre-A11 "any running row matches"
// behavior -- Transition resolves "" to the row's own CURRENT claim_id in
// the same transaction rather than requiring every such caller to learn
// about claim_id at all.
func TestTransition_EmptyClaimIDResolvesCurrentRow(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))

	result, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "completed",
		ResultSummary: "ok", Wake: true, // ClaimID left at its zero value ("")
	})
	require.NoError(t, err)
	require.Equal(t, TransitionWon, result.Outcome, "an empty ClaimID must still win against whatever claim_id the row currently has")
}

// TestClaim_MintsDistinctClaimIDsForDistinctKeys pins the mechanism directly:
// two claims for two different keys mint two different, non-empty claim_ids.
func TestClaim_MintsDistinctClaimIDsForDistinctKeys(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	c1, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	c2, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-2", Kind: JobKindCommand, Input: "y", ToolName: "bash"})
	require.NoError(t, err)

	require.NotEmpty(t, c1.Row.ClaimID)
	require.NotEmpty(t, c2.Row.ClaimID)
	require.NotEqual(t, c1.Row.ClaimID, c2.Row.ClaimID)
}
