package session

// Cross-process shell-id collision: a background shell id is the shell
// manager's per-process counter ("001", "002", ...), unique per process only,
// while the async_jobs row is keyed (owner, tool_call_id) in the SHARED
// database. A second rush process claiming the same shell id for the same
// session must refuse instead of silently adopting the first process's row.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// newTwoHostTestStore builds TWO stores sharing ONE database: db.Connect is
// refcounted per dataDir, so it is called once and both stores are built on
// the returned *sql.DB (different hosts, same rows).
func newTwoHostTestStore(t *testing.T) (a, b *AsyncJobStore, q *db.Queries, ctx context.Context) {
	t.Helper()
	dataDir := t.TempDir()
	ctx = context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	a = NewAsyncJobStore(conn, dataDir, 999, "app-a")
	b = NewAsyncJobStore(conn, dataDir, 999, "app-b")
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a, b, db.New(conn), ctx
}

// REVERT CHECK: temporarily removed ClaimShell's Existing + foreign-host
// branch -- this test FAILED (An error is expected but got nil; store B got
// Existing with host A's row). Restored the branch; re-ran, passed.
func TestClaimShell_CrossProcessShellIDCollisionIsRefused(t *testing.T) {
	t.Parallel()
	a, b, q, ctx := newTwoHostTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	first, err := a.ClaimShell(ctx, "owner-1", "001", true)
	require.NoError(t, err)
	require.False(t, first.Existing)

	_, err = b.ClaimShell(ctx, "owner-1", "001", true)
	require.Error(t, err)
	var collision *ErrBGShellIDCollision
	require.ErrorAs(t, err, &collision)
	require.Equal(t, "001", collision.ShellID)
	require.Equal(t, a.HostID(), collision.HostID)

	row, err := a.Get(ctx, "owner-1", "001")
	require.NoError(t, err)
	require.Equal(t, a.HostID(), row.HostID)
	require.Equal(t, first.Row.ClaimID, row.ClaimID, "host A's row must be byte-for-byte unchanged")
	require.Equal(t, "running", row.State)
	require.Equal(t, int64(1), row.Announced)

	repeat, err := a.ClaimShell(ctx, "owner-1", "001", true)
	require.NoError(t, err)
	require.True(t, repeat.Existing, "a same-host repeat stays idempotent")
}

// REVERT CHECK: temporarily removed the dead-host recovery + single retry --
// this test FAILED (Should be false: Existing with the dead host's still-running
// row). Restored; re-ran, passed. The warm-up claim above is required: on the
// first revert attempt, without it, ensureHost's first-registration sweep
// pre-interrupted the dead host's row and the test passed without the branch.
func TestClaimShell_DeadHostCollisionRecoversAndClaimsFresh(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-warm"))
	// Warm-up claim: register THIS store's host before the dead host's row
	// exists, so ensureHost's first-registration sweep cannot pre-interrupt
	// it and only ClaimShell's recovery+retry branch can.
	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-warm", ToolCallID: "warm-1", Kind: JobKindCommand, Input: "warm"})
	require.NoError(t, err)
	require.NotEmpty(t, store.HostID(), "precondition: the host must already be registered so the first-registration sweep cannot pre-interrupt the dead host's row")
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	fabricateDeadHost(t, ctx, q, store.dataDir, "dead-bg-host")
	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: "owner-1", ToolCallID: "sh-dead-1", Kind: string(JobKindBGShell),
		ToolName: BGShellToolName, InputHash: HashJobInput(bgShellInputPrefix + "sh-dead-1"), HostID: "dead-bg-host",
		ClaimID: "claim-dead-1", CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)
	_, err = q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{UpdatedAt: 1700000001, OwnerSessionID: "owner-1", ToolCallID: "sh-dead-1"})
	require.NoError(t, err)

	res, err := store.ClaimShell(ctx, "owner-1", "sh-dead-1", true)
	require.NoError(t, err)
	require.False(t, res.Existing, "the recovered key must yield a FRESH claim")

	row, err := store.Get(ctx, "owner-1", "sh-dead-1")
	require.NoError(t, err)
	require.Equal(t, store.HostID(), row.HostID)
	require.Equal(t, "running", row.State)
	require.Equal(t, int64(1), row.Announced)
	require.NotEqual(t, "claim-dead-1", row.ClaimID)

	rows, err := q.ListAsyncJobsForOwner(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	running := 0
	for _, r := range rows {
		if r.HostID == "dead-bg-host" {
			require.Equal(t, "interrupted", r.State)
			require.Equal(t, "claim-dead-1", r.ClaimID, "interrupted/archived exactly once")
			continue
		}
		require.Equal(t, "running", r.State)
		require.Equal(t, store.HostID(), r.HostID)
		running++
	}
	require.Equal(t, 1, running, "no second claim-id row left running")
}
