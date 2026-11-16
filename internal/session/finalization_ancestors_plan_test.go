package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFinalizationParentEdgesQueryUsesChildSessionIndex pins the plan of the
// per-ancestor finalization read. It runs on the single writer connection and
// inside the commit transaction; as a full scan of batch_nodes it held the
// writer long enough to exhaust the 45s jobbatch commit budget (exit code 70).
func TestFinalizationParentEdgesQueryUsesChildSessionIndex(t *testing.T) {
	_, conn, _, ctx, _ := batchServedTestEnv(t)
	var plan string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("query:\n%s\nplan:\n%s", finalizationParentEdgesQuery, plan)
		}
	})
	rows, err := conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+finalizationParentEdgesQuery, "child-session")
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id, parent, notused int64
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan += detail + "\n"
	}
	require.NoError(t, rows.Err())
	require.Contains(t, plan, "idx_batch_nodes_child_session", "the ancestry read must search the child-session index")
	require.NotContains(t, plan, "SCAN", "the ancestry read must not scan batch_nodes")
}
