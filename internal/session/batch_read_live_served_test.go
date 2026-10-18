// Tests for the index-served live-node read: row/order equivalence with the
// join query it replaces (same production helper), EXPLAIN QUERY PLAN pins
// on the production SQL constants, and a snapshot-consistency interleaving
// test for the transactional read.
package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// batchLiveNodesForOwnersJoin is the join query the served two-step read
// replaces (internal/db/sql/batch_read.sql), duplicated here because the
// generated statement is unexported. It is the equivalence oracle below.
const batchLiveNodesForOwnersJoin = `SELECT n.root_id, n.node_id FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE r.owner_session_id = ?1
  AND r.state IN ('pending', 'starting', 'running', 'cancelling')
ORDER BY r.created_at, r.root_id, n.ordinal, n.node_id`

func batchServedTestEnv(t *testing.T) (*BatchStore, *sql.DB, *sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))
	require.NoError(t, seedSession(ctx, db.New(conn), "owner2"))
	// An independent handle: it can commit between the served read's
	// statements even while the writer connection holds the read transaction.
	raw, err := sql.Open("sqlite", filepath.Join(dataDir, "rush.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	return NewBatchStore(conn), conn, raw, ctx
}

func batchReserveForOwner(t *testing.T, s *BatchStore, ctx context.Context, owner, call string) string {
	t.Helper()
	p := batchStoreTestReservation()
	p.Owner = owner
	p.ToolCallID = call
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	return r.Snapshot.Root.RootID
}

func batchLivePairs(t *testing.T, conn *sql.DB, query, owner string) [][2]string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), query, owner)
	require.NoError(t, err)
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		require.NoError(t, rows.Scan(&pair[0], &pair[1]))
		out = append(out, pair)
	}
	require.NoError(t, rows.Err())
	return out
}

func servedPairs(rows []db.BatchNode) [][2]string {
	out := make([][2]string, 0, len(rows))
	for _, n := range rows {
		out = append(out, [2]string{n.RootID, n.NodeID})
	}
	return out
}

func servedByOwner(rows []db.BatchNode, owner string) []db.BatchNode {
	out := make([]db.BatchNode, 0, len(rows))
	for _, n := range rows {
		if n.OwnerSessionID == owner {
			out = append(out, n)
		}
	}
	return out
}

func dbProjectForRoot(owner, root string, sequence int64, update db.ProjectBatchNodeParams) BatchProjection {
	return BatchProjection{Host: "host", Sequence: sequence, Roots: []BatchRootProjection{{Owner: owner, Root: root, Updates: []db.ProjectBatchNodeParams{update}}}}
}

// TestBatchLiveNodesForOwnersServedMatchesJoin pins the served two-step read
// to the join query's rows on a real migrated schema: several owners, several
// live roots per owner, a settled root excluded, a settled task kept. Whole
// result: exact row SET. Per owner: exact order (the join's ORDER BY
// restricted to the owner) — this is the order the caller observes after
// grouping rows by owner.
func TestBatchLiveNodesForOwnersServedMatchesJoin(t *testing.T) {
	t.Parallel()
	s, conn, _, ctx := batchServedTestEnv(t)
	q := db.New(conn)
	liveA1 := batchReserveForOwner(t, s, ctx, "owner", "a1")
	liveA2 := batchReserveForOwner(t, s, ctx, "owner", "a2")
	batchReserveForOwner(t, s, ctx, "owner2", "b1")
	batchReserveForOwner(t, s, ctx, "owner2", "b2")
	doneRoot := batchReserveForOwner(t, s, ctx, "owner2", "b-done")
	require.NoError(t, s.Project(ctx, dbProjectForRoot("owner2", doneRoot, 2, batchStoreTestUpdate(doneRoot, "completed"))))
	require.NoError(t, s.Project(ctx, dbProjectForRoot("owner", liveA1, 3, batchStoreTestUpdate(liveA1+".1", "completed"))))

	served, err := batchLiveNodesForOwnersServed(ctx, q, []string{"owner", "owner2"})
	require.NoError(t, err)

	want := map[[2]string]bool{}
	for _, owner := range []string{"owner", "owner2"} {
		for _, pair := range batchLivePairs(t, conn, batchLiveNodesForOwnersJoin, owner) {
			want[pair] = true
		}
	}
	got := map[[2]string]bool{}
	for _, pair := range servedPairs(served) {
		require.True(t, want[pair], "unexpected served row %v", pair)
		got[pair] = true
	}
	require.Len(t, got, len(want), "served rows miss join rows")
	// Exact per-owner order.
	require.Equal(t, batchLivePairs(t, conn, batchLiveNodesForOwnersJoin, "owner"), servedPairs(servedByOwner(served, "owner")))
	require.Equal(t, batchLivePairs(t, conn, batchLiveNodesForOwnersJoin, "owner2"), servedPairs(servedByOwner(served, "owner2")))
	for _, n := range served {
		require.NotEqual(t, doneRoot, n.RootID, "the settled root stays out")
	}
}

// TestBatchLiveServedReadIsSnapshotConsistent proves the transactional read
// keeps the old single-statement semantics under the reviewer's exact
// interleaving: root A live when the root list is read, root B submitted and
// A settled+purged between the root list and the node reads. The seam sits
// exactly between those two statements. MUTATION IT CATCHES: running the
// fetch WITHOUT the read transaction (the pre-fix two-step shape) — the
// purge lands between the statements, A's node read returns nothing, B was
// never listed, and the reader reports a complete empty result.
func TestBatchLiveServedReadIsSnapshotConsistent(t *testing.T) {
	s, conn, raw, ctx := batchServedTestEnv(t)
	rootA := batchReserveForOwner(t, s, ctx, "owner", "live-a")
	reader := batchReadTestReader(t, conn)
	batchLiveServedSeam.Store(func() {
		rawStore := NewBatchStore(raw)
		batchReserveForOwner(t, rawStore, ctx, "owner", "live-b")
		require.NoError(t, rawStore.DeleteRoot(ctx, "owner", "host", rootA))
	})
	t.Cleanup(func() { batchLiveServedSeam.Store(nil) })

	live := s.BatchLiveForSessionsTx(ctx, reader, []string{"owner"})
	require.True(t, live["owner"].Complete)
	require.True(t, live["owner"].HasLive(), "the snapshot still shows the live root")
	require.Len(t, live["owner"].Own, 1)
	require.Equal(t, rootA, live["owner"].Own[0].Ref.RootID)
}

// TestBatchHotStatementsPlanWithoutScans runs EXPLAIN QUERY PLAN against the
// real migrated schema. The debt and react statements are the PRODUCTION
// constants (batchDebtNodesVoidAwareSQL / batchOutboxReactNodesVoidAware), so
// dropping either verbatim `delivery != 'void'` term fails the SCAN
// assertion here. The two live-read statements mirror the generated
// ListLiveBatchRootsForOwner / ListBatchNodes text (unexportable); they pin
// the schema's index contract for the served read.
func TestBatchHotStatementsPlanWithoutScans(t *testing.T) {
	t.Parallel()
	_, conn, _, ctx := batchServedTestEnv(t)
	// Mirrors of the generated consts (internal/db/batch_nodes.sql.go).
	liveRoots := `SELECT node_id FROM batch_nodes
WHERE owner_session_id = ?1 AND node_id = root_id
  AND state IN ('pending', 'starting', 'running', 'cancelling')
ORDER BY created_at, root_id`
	rootNodes := `SELECT node_id FROM batch_nodes
WHERE root_id = ?1 AND owner_session_id = ?2
ORDER BY node_id`
	planLiveRoots := explainPlan(t, conn, ctx, liveRoots, "owner")
	planRootNodes := explainPlan(t, conn, ctx, rootNodes, "owner", "owner2")
	planDebt := explainPlan(t, conn, ctx, batchDebtNodesVoidAwareSQL, "owner")
	planReact := explainPlan(t, conn, ctx, batchOutboxReactNodesVoidAware, "owner", "owner")
	for name, plan := range map[string]string{
		"live roots by owner": planLiveRoots,
		"nodes of one root":   planRootNodes,
		"debt nodes":          planDebt,
		"react nodes":         planReact,
	} {
		require.NotContains(t, plan, "SCAN", "%s: full scan returned", name)
	}
	require.Contains(t, planLiveRoots, "idx_batch_nodes_owner_roots")
	require.Contains(t, planRootNodes, "idx_batch_nodes_parent")
	require.Contains(t, planDebt, "idx_batch_nodes_debt")
	require.Contains(t, planReact, "idx_batch_nodes_debt")
}

func explainPlan(t *testing.T, conn *sql.DB, ctx context.Context, query string, args ...any) string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notused int64
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	return strings.Join(details, "\n")
}
