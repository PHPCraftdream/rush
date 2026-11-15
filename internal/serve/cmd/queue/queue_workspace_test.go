package queue

// WS-1 ownership tests (task #1142 step C, P1 of
// docs/plans/2026-10-01-shared-data-dir.md): with one shared data dir, a
// queue_tasks row belongs to exactly one workspace, and only that workspace's
// runner may claim or reclaim it.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueue_Add_StampsWorkspaceRoot proves Add writes the service's
// workspace_root on the row: a home service stamps ” (the pre-WS-1 shape),
// a workspace-bound service stamps its own root -- this column is what the
// claim/reclaim ownership filters read.
//
// REVERT-CHECK: stop Add from writing workspace_root (drop the column from
// the INSERT) -> tasks[0].WorkspaceRoot is "" for the bound service and
// TestQueue_Add_StampsWorkspaceRoot fails at
// require.Equal(t, wsA, tasks[0].WorkspaceRoot).
func TestQueue_Add_StampsWorkspaceRoot(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	home := NewService(db)
	homeID, err := home.Add(ctx, "", "legacy", "", 0, 0, 0)
	require.NoError(t, err)

	wsA := NewServiceWithWorkspace(db, "/ws/a", false)
	aID, err := wsA.Add(ctx, "", "mine", "", 0, 0, 0)
	require.NoError(t, err)

	tasks, err := home.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, tasks, 2)

	byID := map[string]Task{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	require.Equal(t, "", byID[homeID].WorkspaceRoot, "a home service stamps the legacy '' row")
	require.Equal(t, "/ws/a", byID[aID].WorkspaceRoot, "a workspace-bound service stamps its own root")

	got, err := wsA.Get(ctx, aID)
	require.NoError(t, err)
	require.Equal(t, "/ws/a", got.WorkspaceRoot)
}

// TestQueue_ClaimPending_SkipsForeignRows is the P1 revert-check of step C's
// queue filter: a runner must only claim its own workspace's pending rows. A
// foreign workspace's row (another checkout, or a legacy ” row for a
// non-home service) must not even be read by the claim -- it stays 'pending'
// with started_at NULL, attempts untouched, owned by its own checkout's
// runner.
//
// REVERT-CHECK: drop ownershipFilter from ClaimPending's SELECT ->
// the unfiltered scan returns the legacy AND foreign rows (observed:
// "Should be empty, but was [... /ws/b]") and
// require.Empty(t, claimed, ...) at queue_workspace_test.go:82 fails.
func TestQueue_ClaimPending_SkipsForeignRows(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	home := NewService(db) // workspace "" (home process)
	wsA := NewServiceWithWorkspace(db, "/ws/a", false)
	wsB := NewServiceWithWorkspace(db, "/ws/b", false)

	legacyID, err := home.Add(ctx, "", "legacy", "", 0, 0, 0)
	require.NoError(t, err)
	bID, err := wsB.Add(ctx, "", "foreign", "", 0, 0, 0)
	require.NoError(t, err)

	// A linked worktree owns only its own rows: the legacy '' row belongs to
	// the home process, not to it.
	claimed, err := wsA.ClaimPending(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, claimed, "a workspace service must not claim legacy or foreign rows")

	for _, id := range []string{legacyID, bID} {
		row, err := wsA.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, StatusPending, row.Status, "row %s must stay pending for the non-owner", id)
		assert.False(t, row.StartedAt.Valid, "row %s must not be started by a foreign runner", id)
	}

	aID, err := wsA.Add(ctx, "", "mine", "", 0, 0, 0)
	require.NoError(t, err)

	claimed, err = wsA.ClaimPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, aID, claimed[0].ID)
	require.Equal(t, "/ws/a", claimed[0].WorkspaceRoot)

	for _, id := range []string{legacyID, bID} {
		row, err := wsA.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, StatusPending, row.Status, "row %s must still be pending after the owner claimed its own", id)
		assert.False(t, row.StartedAt.Valid)
	}

	// The home process still owns the legacy row.
	claimed, err = home.ClaimPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, legacyID, claimed[0].ID)
}

// TestQueue_ReclaimRunning_SkipsForeignRows is the reclaim half of the same
// P1 rule: an orphaned 'running' row of ANOTHER workspace is none of this
// runner's business -- its live runner, if any, holds it under its own OS
// lock, and an orphaned one is that checkout's backlog.
//
// REVERT-CHECK: drop ownershipFilter from ReclaimRunning's UPDATE ->
// reclaimed is 2, the foreign row flips to 'pending' with started_at cleared,
// and both queue_workspace_test.go:145 (EqualValues 1) and
// queue_workspace_test.go:154/155 (foreign still running, started_at valid)
// fail.
func TestQueue_ReclaimRunning_SkipsForeignRows(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	wsA := NewServiceWithWorkspace(db, "/ws/a", false)
	wsB := NewServiceWithWorkspace(db, "/ws/b", false)

	aID, err := wsA.Add(ctx, "", "mine", "", 0, 0, 0)
	require.NoError(t, err)
	bID, err := wsB.Add(ctx, "", "foreign", "", 0, 0, 0)
	require.NoError(t, err)

	// Each workspace's runner claims its own row (running + started_at set).
	claimedA, err := wsA.ClaimPending(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimedA, 1)
	claimedB, err := wsB.ClaimPending(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimedB, 1)

	reclaimed, err := wsA.ReclaimRunning(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, reclaimed, "only this workspace's orphaned running row may be reclaimed")

	mine, err := wsA.Get(ctx, aID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, mine.Status, "own orphaned running row must return to pending")
	assert.False(t, mine.StartedAt.Valid, "started_at must be cleared on reclaim")

	foreign, err := wsB.Get(ctx, bID)
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, foreign.Status, "a foreign running row must not be touched by another workspace's reclaim")
	assert.True(t, foreign.StartedAt.Valid, "the foreign row's started_at must survive another workspace's reclaim")

	// The home predicate in Go agrees with the SQL filters above.
	require.True(t, wsA.ownsRow("/ws/a"))
	require.False(t, wsA.ownsRow("/ws/b"))
	require.False(t, wsA.ownsRow(""))
}

// TestQueue_GitCheckoutHomeRunnerKeepsLegacyRows is the regression guard for
// the home/legacy confusion: a runner inside a git checkout has a NON-empty
// workspace root and is still a home process (every process owns its own data
// directory until SD-D, #1143), so it must keep claiming and reclaiming the
// legacy ” rows of its own history -- and still never touch another
// checkout's rows. The tests above only pair home with the empty root and a
// bound root with home=false, so they cannot tell "home" from "root is empty".
//
// REVERT-CHECK: derive the flag from the root in ownershipArgs
// (homeFlag(s.workspaceRoot == "")) -> the legacy row is no longer claimed and
// the first require.ElementsMatch below fails.
func TestQueue_GitCheckoutHomeRunnerKeepsLegacyRows(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	legacy := NewService(db) // stamps '' like every pre-WS-1 writer
	checkout := NewServiceWithWorkspace(db, "/ws/a", true)
	other := NewServiceWithWorkspace(db, "/ws/b", true)

	legacyID, err := legacy.Add(ctx, "", "legacy", "", 0, 0, 0)
	require.NoError(t, err)
	ownID, err := checkout.Add(ctx, "", "mine", "", 0, 0, 0)
	require.NoError(t, err)
	foreignID, err := other.Add(ctx, "", "foreign", "", 0, 0, 0)
	require.NoError(t, err)

	claimed, err := checkout.ClaimPending(ctx, 10)
	require.NoError(t, err)
	var got []string
	for _, task := range claimed {
		got = append(got, task.ID)
	}
	require.ElementsMatch(t, []string{legacyID, ownID}, got,
		"a git-checkout home runner claims its own and the legacy rows, never another checkout's")

	foreign, err := checkout.Get(ctx, foreignID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, foreign.Status, "another checkout's row stays pending")

	reclaimed, err := checkout.ReclaimRunning(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 2, reclaimed, "the claimed legacy row is reclaimable by the home runner too")

	require.True(t, checkout.ownsRow(""), "a home runner owns legacy rows whatever its root")
	require.True(t, checkout.ownsRow("/ws/a"))
	require.False(t, checkout.ownsRow("/ws/b"))
}
