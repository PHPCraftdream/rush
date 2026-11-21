package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBatchRetentionRootsJoinUsesOwnerSessionIndex pins the plan of the
// retention query. Its child join (owner_session_id = delegation.child_session_id)
// has no plain index except idx_batch_nodes_owner_session; without it the
// planner scans batch_nodes once per candidate root (or builds a transient
// index per root), which held the writer for over an hour on a store with
// ~10k expired roots.
func TestBatchRetentionRootsJoinUsesOwnerSessionIndex(t *testing.T) {
	t.Parallel()
	conn, _ := newCallTreeTestDB(t)
	rows, err := conn.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+batchRetentionRoots, time.Now().Unix())
	require.NoError(t, err)
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int64
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	joined := strings.Join(plan, "\n")
	require.Contains(t, joined, "idx_batch_nodes_owner_session", "plan:\n%s", joined)
	for _, line := range plan {
		require.NotContains(t, line, "AUTOMATIC", "plan:\n%s", joined)
		require.NotEqual(t, "SCAN child", strings.TrimSpace(line), "plan:\n%s", joined)
	}
}
