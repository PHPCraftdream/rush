package session

// R-BG-1 (#1126, docs/plans/2026-10-01-bg-shell-ledger.md): a background shell
// born outside the async-wrapped branch claims a kind=bg_shell async_jobs row
// keyed (owner, tool_call_id = shell id) at its start, and its terminal
// transition commits state + result in one CAS (DUR-1).

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// REVERT CHECK: dropping the MarkAnnounced call from ClaimShell leaves the row
// announced=0 -- this test FAILED (Announced 0, want 1: an unannounced row can
// never produce a notice, DUR-7, and a terminal one would be silently deleted
// by a dead-host sweep). Restored the call; re-ran, passed.
func TestClaimShell_RunningAnnouncedAndIdempotent(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	first, err := store.ClaimShell(ctx, "owner-1", "sh-1", true)
	require.NoError(t, err)
	require.False(t, first.Existing)
	row, err := store.Get(ctx, "owner-1", "sh-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State)
	require.Equal(t, string(JobKindBGShell), row.Kind)
	require.Equal(t, BGShellToolName, row.ToolName)
	require.Equal(t, int64(1), row.Announced, "the tool response that reported the shell id IS the start announcement")

	second, err := store.ClaimShell(ctx, "owner-1", "sh-1", true)
	require.NoError(t, err)
	require.True(t, second.Existing, "re-claiming the same shell id must be an idempotent repeat, never a second row")

	rows, err := q.ListAsyncJobsForOwner(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, rows, 1, "exactly one row per shell")
}

// The dead-host sweep (DUR-6) must treat a running bg_shell row like any
// other running row: interrupted, never silently forgotten. The row is seeded
// through the REAL claim path (kind=bg_shell, tool_call_id = shell id).
//
// REVERT CHECK: no mutant applies -- the sweep reads async_jobs without a
// kind filter; this test pins that the bg_shell KIND survives the migration's
// CHECK and the generic recovery (a CHECK regression would fail on insert).
func TestBGShellRow_DeadHostSweepInterrupts(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-bg-host")
	hostID := "dead-bg-host"
	seeded, err := q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: "owner-1", ToolCallID: "sh-dead", Kind: string(JobKindBGShell),
		ToolName: BGShellToolName, InputHash: "bg-shell:sh-dead", HostID: hostID,
		ClaimID: "claim-sh-dead", CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)
	require.Equal(t, string(JobKindBGShell), seeded.Kind, "the rebuilt table's CHECK must accept kind=bg_shell")
	_, err = q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{UpdatedAt: 1700000001, OwnerSessionID: "owner-1", ToolCallID: "sh-dead"})
	require.NoError(t, err)

	outcomes, err := store.SweepDeadHosts(ctx, nil)
	require.NoError(t, err)
	require.Contains(t, outcomes, hostID, "the sweep must recover the bg_shell row's dead host")

	row, err := store.Get(ctx, "owner-1", "sh-dead")
	require.NoError(t, err)
	require.Equal(t, "interrupted", row.State)
}
