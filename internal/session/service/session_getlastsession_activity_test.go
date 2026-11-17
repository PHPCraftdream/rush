// T13 (#1130, design sec.8): delegation-subtree activity drives `--continue`
// (GetLastSession) — a busy child keeps its root the continuation target —
// while the PARENT's updated_at in the database stays untouched: the child
// never writes it, the activity is a query.

package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetLastSession_SubtreeActivity(t *testing.T) {
	ctx := context.Background()
	conn, q := newTestDB(t)
	svc := NewService(q, conn)

	quiet, err := svc.Create(ctx, "quiet root")
	require.NoError(t, err)
	active, err := svc.Create(ctx, "active root")
	require.NoError(t, err)
	child, err := svc.CreateTaskSession(ctx, "t13$$c1", active.ID, "child")
	require.NoError(t, err)

	// Own-row timestamps: the QUIET root's row is the newest of the two
	// roots; only the ACTIVE root's CHILD is newer than everything.
	pin := func(id string, ts int64) {
		_, err := conn.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, ts, id)
		require.NoError(t, err)
	}
	const tQuiet, tActive, tChild = int64(1_000_100), int64(1_000_000), int64(1_000_200)
	pin(quiet.ID, tQuiet)
	pin(active.ID, tActive)
	pin(child.ID, tChild)

	// The busy child makes ITS root the continuation target, even though the
	// quiet root's own row is fresher.
	last, err := svc.GetLast(ctx)
	require.NoError(t, err)
	require.Equal(t, active.ID, last.ID, "the root with the active child wins --continue")

	// T13's second half: the parent's updated_at in the DB is UNCHANGED by
	// the child's activity (the activity was only ever a query).
	var parentUpdatedAt int64
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT updated_at FROM sessions WHERE id = ?`, active.ID).Scan(&parentUpdatedAt))
	assert.Equal(t, tActive, parentUpdatedAt, "the parent's updated_at must not be written by the child's activity")

	// And a child-only root never becomes --continue's answer: the query
	// only ever returns top-level sessions.
	var topLevel int
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE parent_session_id IS NULL`).Scan(&topLevel))
	require.Equal(t, 2, topLevel)
}
