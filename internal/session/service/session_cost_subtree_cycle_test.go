package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// T9 (#1130): a corrupted cost_parent_id cycle (a node its own ancestor) must
// terminate the CTE and count every node of the cycle exactly once -- the
// UNION (not UNION ALL) is what makes that hold. The spend still reads.
//
// Revert-check: UNION ALL in GetSubtreeSpent loops the two-node cycle forever
// (the test binary times out); dropping the recursion entirely under-reports.
func TestSubtreeSpent_CycleTerminatesAndCountsOnce(t *testing.T) {
	ctx := context.Background()
	conn, q := newTestDB(t)
	svc := NewService(q, conn)

	r, err := svc.Create(ctx, "root")
	require.NoError(t, err)
	a, err := svc.CreateTaskSession(ctx, "cyc-a", r.ID, "A")
	require.NoError(t, err)
	b, err := svc.CreateTaskSession(ctx, "cyc-b", a.ID, "B")
	require.NoError(t, err)

	_, err = svc.IncrementCost(ctx, a.ID, 0.10)
	require.NoError(t, err)
	_, err = svc.IncrementCost(ctx, b.ID, 0.20)
	require.NoError(t, err)

	// Corrupt the edges into a two-node cycle below r.
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET cost_parent_id = 'cyc-b' WHERE id = 'cyc-a'`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET cost_parent_id = 'cyc-a' WHERE id = 'cyc-b'`)
	require.NoError(t, err)

	// The corruption re-parented a out of r's subtree: r now reads alone.
	spent, err := svc.SubtreeSpent(ctx, r.ID)
	require.NoError(t, err)
	require.InDelta(t, 0.0, spent, 1e-9, "r's own ledger only � a left the subtree with its edge")

	// The cycle reads finite and counts each node exactly once: 0.10 + 0.20.
	cycleSpent, err := svc.SubtreeSpent(ctx, a.ID)
	require.NoError(t, err)
	require.InDelta(t, 0.30, cycleSpent, 1e-9, "a->b->a: both cycle nodes, each counted exactly once")
	cycleSpentB, err := svc.SubtreeSpent(ctx, b.ID)
	require.NoError(t, err)
	require.InDelta(t, 0.30, cycleSpentB, 1e-9, "b reads the same cycle, same total")
}
