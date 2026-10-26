// Batch-fixture helpers (port round A3) for the reaction-chain and wake-debt
// dual-mode tests. Prefixed a3 so sibling packages' porting agents cannot
// collide with these names.
package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// a3DebtRowIdentity is the chain identity of one seeded debt row: the root
// tool_call_id (batch chains key on the launched root's call id, never on
// claims).
func a3DebtRowIdentity(row fixtureAsyncJobRow) string {
	return row.ToolCallID
}

// a3MarkRowsReacted marks the debt rows seeded under callIDs reacted through
// the selected authority (the batch update covers both the root row itself
// and its child rows via the root_id subquery).
func a3MarkRowsReacted(t *testing.T, ctx context.Context, conn *sql.DB, owner string, calls ...string) {
	t.Helper()
	for _, call := range calls {
		var err error
		_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET reacted=1 WHERE owner_session_id=? AND (tool_call_id=? OR root_id IN (SELECT node_id FROM batch_nodes WHERE owner_session_id=? AND tool_call_id=?))`, owner, call, owner, call)
		require.NoError(t, err)
	}
}

// a3BatchDebtSnapshotForCalls builds the acted-on set: the pending-inclusive
// debt rows whose ROOT tool_call_id is one of calls -- the batch twin of the
// legacy tests' crafted claim snapshots (the rows of PREVIOUS legs' launches).
func a3BatchDebtSnapshotForCalls(t *testing.T, ctx context.Context, conn *sql.DB, owner string, calls []string) session.BatchDebtSnapshot {
	t.Helper()
	snap := session.BatchDebtSnapshot{Owner: owner}
	if len(calls) == 0 {
		return snap
	}
	rows, err := session.NewBatchStore(conn).BatchReactionPendingInclusiveDebtRows(ctx, owner)
	require.NoError(t, err)
	want := make(map[string]struct{}, len(calls))
	for _, c := range calls {
		want[c] = struct{}{}
	}
	rootCall := func(rootID string) string {
		var callID string
		_ = conn.QueryRowContext(ctx, `SELECT tool_call_id FROM batch_nodes WHERE node_id=?`, rootID).Scan(&callID)
		return callID
	}
	for _, ref := range rows.Nodes {
		if _, ok := want[rootCall(ref.RootID)]; ok {
			snap.Nodes = append(snap.Nodes, session.BatchDebtNodeRef{RootID: ref.RootID, NodeID: ref.NodeID, NoticeMessageID: ref.NoticeMessageID})
		}
	}
	return snap
}

// a3SeedChainBatchRoots seeds the guard's state in the batch identity
// vocabulary (the twin of setReactionChain's claim map).
func a3SeedChainBatchRoots(c *coordinator, sessionID string, links int, roots ...string) {
	c.seedArbiterState(sessionID, func(s *arbiterState) {
		s.chainLinks = links
		s.chainBatchRoots = make(map[string]struct{}, len(roots))
		for _, r := range roots {
			s.chainBatchRoots[r] = struct{}{}
		}
	})
}

// a3ChainHasIdentity is the has-identity read over the chain's batch roots.
func a3ChainHasIdentity(c *coordinator, sessionID, identity string) bool {
	_, ok := c.arb.snapshot(sessionID).ChainBatchRoots[identity]
	return ok
}

// a3DebtOpen is the open-reaction-debt predicate over the batch store.
func a3DebtOpen(t *testing.T, ctx context.Context, conn *sql.DB, owner string) bool {
	t.Helper()
	open, err := session.NewBatchStore(conn).BatchReactionDebtExists(ctx, owner)
	require.NoError(t, err)
	return open
}
