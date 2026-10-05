package session

// B9-1 (ox round 9): ClaimShell inserts the row (Claim) and only then marks it
// announced. When the second step fails the caller refuses the background start
// and kills the shell, so no observer is registered and nothing else closes an
// unannounced running row of a LIVE host: it would keep the session scope open
// for the whole life of the process.

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

const blockBGShellAnnounce = "UPDATE OF announced ON async_jobs WHEN NEW.kind = 'bg_shell'"

// REVERT CHECK: removing the abandonShellClaim call from ClaimShell leaves the
// claimed row behind -- this test FAILED (store.Get found a running,
// announced=0 row after ClaimShell returned its error).
func TestClaimShell_AnnounceFailureLeavesNoRow(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	abortingTrigger(t, store, "fx_block_bgshell_announce", blockBGShellAnnounce, "fx: announce blocked")

	_, err := store.ClaimShell(ctx, "owner-1", "sh-1", false)
	require.Error(t, err, "the caller must be told to refuse the background start")
	require.Contains(t, err.Error(), "mark announced")

	_, getErr := store.Get(ctx, "owner-1", "sh-1")
	require.ErrorIs(t, getErr, sql.ErrNoRows, "a refused shell must not leave an open row behind")
	rows, err := q.ListAsyncJobsForOwner(ctx, "owner-1")
	require.NoError(t, err)
	require.Empty(t, rows)
}

// The cleanup is scoped to unannounced rows: a repeat claim of a shell whose row
// already exists and is announced (Existing) and whose announce fails must leave
// that earlier row alone. No Go-level mutant applies -- the scope is the
// DeleteUnannouncedAsyncJobForClaim query's announced=0 predicate; this test
// pins it (a hard delete by claim id would make it FAIL with ErrNoRows).
func TestClaimShell_AnnounceFailureOnExistingClaimKeepsTheRow(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	first, err := store.ClaimShell(ctx, "owner-1", "sh-1", false)
	require.NoError(t, err)
	require.False(t, first.Existing)
	abortingTrigger(t, store, "fx_block_bgshell_announce", blockBGShellAnnounce, "fx: announce blocked")

	_, err = store.ClaimShell(ctx, "owner-1", "sh-1", false)
	require.Error(t, err)

	row, getErr := store.Get(ctx, "owner-1", "sh-1")
	require.NoError(t, getErr, "the earlier claim's row is not this call's to delete")
	require.Equal(t, "running", row.State)
	require.Equal(t, first.Row.ClaimID, row.ClaimID)
}
