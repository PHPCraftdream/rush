// Tests for the index-served live-node read: whole-result row/order
// equivalence with the join query it replaces (the production helper is driven
// directly, not a copy), a snapshot-consistency interleaving test for both
// public transactional entries, and EXPLAIN QUERY PLAN pins on real captured
// production SQL (the driver-level shim in batch_sqllog_test.go).
package session

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// batchLiveNodesForOwnersJoin is the join query the served two-step read
// replaces (internal/db/sql/batch_read.sql), duplicated here because the
// generated statement is unexported. It is the equivalence oracle below.
const batchLiveNodesForOwnersJoin = `SELECT n.root_id, n.node_id FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE r.owner_session_id IN (?1, ?2)
  AND r.state IN ('pending', 'starting', 'running', 'cancelling')
ORDER BY r.created_at, r.root_id, n.ordinal, n.node_id`

func batchServedTestEnv(t *testing.T) (*BatchStore, *sql.DB, *sql.DB, context.Context, string) {
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
	raw, err := sql.Open("sqlite", filepath.Join(dataDir, "rush.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	return NewBatchStore(conn), conn, raw, ctx, dataDir
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

func batchLivePairs(t *testing.T, conn *sql.DB, query, owner1, owner2 string) [][2]string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), query, owner1, owner2)
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

func dbProjectForRoot(owner, root string, sequence int64, update db.ProjectBatchNodeParams) BatchProjection {
	return BatchProjection{Host: "host", Sequence: sequence, Roots: []BatchRootProjection{{Owner: owner, Root: root, Updates: []db.ProjectBatchNodeParams{update}}}}
}

// TestBatchLiveNodesForOwnersServedMatchesJoin pins the served two-step read
// to the join query's rows on a real migrated schema: two owners, two live
// roots each with deterministic interleaved created_at, a settled root
// excluded, a settled task inside a live root kept. WHOLE-RESULT assertion:
// exact rows in exact order — the helper visits roots in GLOBAL
// (created_at, root_id) order across owners, which the public map API (it
// groups results per owner) cannot express, hence the direct helper call.
func TestBatchLiveNodesForOwnersServedMatchesJoin(t *testing.T) {
	t.Parallel()
	s, conn, _, ctx, _ := batchServedTestEnv(t)
	liveA1 := batchReserveForOwner(t, s, ctx, "owner", "a1")
	liveB1 := batchReserveForOwner(t, s, ctx, "owner2", "b1")
	rootA2 := batchReserveForOwner(t, s, ctx, "owner", "a2")
	liveB2 := batchReserveForOwner(t, s, ctx, "owner2", "b2")
	doneRoot := batchReserveForOwner(t, s, ctx, "owner2", "b-done")
	require.NoError(t, s.Project(ctx, dbProjectForRoot("owner2", doneRoot, 2, batchStoreTestUpdate(doneRoot, "completed"))))
	require.NoError(t, s.Project(ctx, dbProjectForRoot("owner", liveA1, 3, batchStoreTestUpdate(liveA1+".1", "completed"))))
	for _, tc := range []struct {
		id string
		at int64
	}{{liveA1, 100}, {liveB1, 200}, {rootA2, 300}, {liveB2, 400}} {
		_, err := conn.ExecContext(ctx, "UPDATE batch_nodes SET created_at = ? WHERE node_id = ?", tc.at, tc.id)
		require.NoError(t, err)
	}

	// Reversed input: the global sort must not depend on the owners' order.
	served, err := batchLiveNodesForOwnersServed(ctx, db.New(conn), []string{"owner2", "owner"})
	require.NoError(t, err)
	joinPairs := batchLivePairs(t, conn, batchLiveNodesForOwnersJoin, "owner2", "owner")
	require.Equal(t, joinPairs, servedPairs(served))
	for _, n := range served {
		require.NotEqual(t, doneRoot, n.RootID, "the settled root stays out")
	}
}

// batchServedInterleave runs one snapshot-consistency interleaving round:
// reserve the root, arm the seam (guarded so it fires exactly once — each
// public entry invokes the served helper once), drive the reader, and assert
// the pre-interleaving root is still reported complete and live.
func batchServedInterleave(t *testing.T, s *BatchStore, conn, raw *sql.DB, ctx context.Context, owner, call string, drive func(*BatchLiveReader) map[string]BatchLiveResult) {
	t.Helper()
	root := batchReserveForOwner(t, s, ctx, owner, call)
	reader := batchReadTestReader(t, conn)
	reader.TxStarter = conn
	var fired bool
	hook := func() {
		if fired {
			return
		}
		fired = true
		rawStore := NewBatchStore(raw)
		batchReserveForOwner(t, rawStore, ctx, owner, call+"-late")
		require.NoError(t, rawStore.DeleteRoot(ctx, owner, "host", root))
	}
	batchLiveServedSeam.Store(&hook)
	t.Cleanup(func() { batchLiveServedSeam.Store(nil) })
	live := drive(reader)
	require.True(t, live[owner].Complete)
	require.True(t, live[owner].HasLive(), "the snapshot still shows the live root")
	require.Len(t, live[owner].Own, 1)
	require.Equal(t, root, live[owner].Own[0].Ref.RootID)
}

// TestBatchLiveServedReadIsSnapshotConsistent proves the transactional read
// keeps the old single-statement semantics under the reviewer's exact
// interleaving: root A live when the root list is read, root B submitted and
// A settled+purged between the root list and the node reads. The seam sits
// exactly between those two statements. Each public entry gets its own fresh
// root pair and seam (owner "owner" via BatchLiveForOwner, owner "owner2" via
// BatchLiveForSessions). MUTATION IT CATCHES: running the fetch WITHOUT the
// read transaction (the pre-fix two-step shape) — the purge lands between the
// statements, A's node read returns nothing, B was never listed, and the
// reader reports a complete empty result.
func TestBatchLiveServedReadIsSnapshotConsistent(t *testing.T) {
	s, conn, raw, ctx, _ := batchServedTestEnv(t)
	batchServedInterleave(t, s, conn, raw, ctx, "owner", "live-a", func(reader *BatchLiveReader) map[string]BatchLiveResult {
		return map[string]BatchLiveResult{"owner": reader.BatchLiveForOwner(ctx, "owner")}
	})
	batchServedInterleave(t, s, conn, raw, ctx, "owner2", "live-b", func(reader *BatchLiveReader) map[string]BatchLiveResult {
		return reader.BatchLiveForSessions(ctx, []string{"owner2"})
	})
}

// TestBatchHotStatementsPlanWithoutScans EXPLAIN QUERY PLANs every batch_nodes
// statement captured from REAL production code paths at the driver level (the
// shim in batch_sqllog_test.go): the transactional live read,
// BatchReactionMarkReactedWithMessageUpdate (react UPDATE, ClearFailure roots
// SELECT and its chunked UPDATE), the pending-inclusive debt read and the
// FailedNodes listing — no copied SQL anywhere. Per-statement assertion: every
// SELECT/UPDATE on these paths is owner/root-keyed, so its plan must contain a
// SEARCH under its production alias (n/r) and NEVER any SCAN line under any
// alias (SQLite EXPLAIN prints the alias, so a bare "SCAN batch_nodes" needle
// would evade "SCAN n").
// MUTATION TRACES:
//   - dropping the debt SELECT's verbatim `delivery != 'void'` term, or the
//     react UPDATE's, changes the captured statement for THAT statement's own
//     plan assertion — the debt and react statements are captured and
//     EXPLAINed individually here;
//   - reverting BatchLiveForSessions' dispatch to the nontransactional served
//     helper removes the BeginTx — no plan change; that revert is caught by
//     TestBatchLiveServedReadIsSnapshotConsistent's identity/Complete asserts;
//   - reverting the dispatch to the Source JOIN captures the join statement —
//     its plan scans batch_nodes and fails THIS test;
//   - reverting the reaction tx's ClearFailure call to the generated
//     owner-wide UPDATE captures that UPDATE — its plan scans batch_nodes and
//     fails THIS test;
//   - reverting the FailedNodes rewrite is NOT caught here: that statement's
//     plan is asserted only as SCAN-free, so a scan-avoiding revert would pass.
func TestBatchHotStatementsPlanWithoutScans(t *testing.T) {
	t.Parallel()
	s, conn, _, ctx, dataDir := batchServedTestEnv(t)
	logDB, log := newBatchSQLLogDB(t, filepath.Join(dataDir, "rush.db"))
	// Live-read capture FIRST: a production reader over the logging DB,
	// dispatched through the transactional path (TxStarter set), over a
	// SEEDED live root — so both ListLiveBatchRootsForOwner and the per-root
	// ListBatchNodes are captured with real rows.
	batchReserveForOwner(t, s, ctx, "owner", "plan-root")
	reader := NewBatchLiveReader(db.New(logDB), dataDir)
	reader.IsOwnHost = func(string) bool { return false }
	reader.ProbeHost = func(string, string) (HostLockStatus, error) { return HostStatusAlive, nil }
	reader.TxStarter = logDB
	live := reader.BatchLiveForOwner(ctx, "owner")
	require.True(t, live.Complete)
	require.True(t, live.HasLive())
	// Reaction capture: the exact seeding of the "clear failed" scenario in
	// TestBatchOutboxFailureMarkerRollbackAndReactionRollback — a root
	// reserved, settled with a notice, and announced (no injected failure, so
	// the reaction tx commits and its full statement set is captured).
	m := message.NewService(db.New(conn))
	snap := batchOutboxReserve(t, s, ctx, 1)
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	msg, err := m.Create(ctx, "owner", message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "done"}}})
	require.NoError(t, err)
	countingStore := NewBatchStore(logDB)
	require.NoError(t, countingStore.BatchReactionMarkReactedWithMessageUpdate(ctx, m, "owner", msg))
	// Failed-row seed so the FailedNodes rewrite has a hit, then drive BOTH
	// public reads: the pending-inclusive debt SELECT (batchDebtNodesVoidAwareSQL)
	// and the FailedNodes listing.
	_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET reacted_failed = 1 WHERE owner_session_id = 'owner'")
	require.NoError(t, err)
	debt, err := countingStore.BatchReactionPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.NotEmpty(t, debt.Nodes, "the debt statement must run with rows")
	failed, err := countingStore.BatchReactionListReactedFailedText(ctx, "owner")
	require.NoError(t, err)
	require.NotEmpty(t, failed)
	found := false
	for _, f := range failed {
		if f.RootID == snap.Root.RootID {
			found = true
		}
	}
	// The listing selects only noticed, announced roots: that is the settled
	// "snap" root, not the pending "plan-root" seeded for the live read.
	require.True(t, found, "failed list must contain the settled root %s", snap.Root.RootID)

	plans := ""
	for _, entry := range batchSQLLogBatchNodes(log) {
		// EXPLAIN only SELECT/UPDATE batch_nodes statements — still ALL of
		// them, not a sample; nothing else touches batch_nodes here.
		if !strings.Contains(entry.Query, "SELECT") && !strings.Contains(entry.Query, "UPDATE") {
			continue
		}
		plan := explainPlanArgs(t, conn, ctx, entry.Query, entry.Args)
		require.NotEmpty(t, plan, "empty EXPLAIN plan for %q", entry.Query)
		// Reject ANY SCAN line under ANY alias: production aliases the table
		// as n/r, so "SCAN n" would evade a "SCAN batch_nodes" needle. Every
		// statement on these paths is owner/root-keyed and must plan as a
		// SEARCH.
		require.NotContains(t, plan, "SCAN", "statement %q has a full-table SCAN under some alias; plan:\n%s", entry.Query, plan)
		plans += plan + "\n"
	}
	// The union of plans must serve from the three hot indexes; joined so the
	// assertion does not depend on capture order.
	require.Contains(t, plans, "idx_batch_nodes_owner_roots")
	require.Contains(t, plans, "idx_batch_nodes_parent")
	require.Contains(t, plans, "idx_batch_nodes_debt")
}

func explainPlanArgs(t *testing.T, conn *sql.DB, ctx context.Context, query string, args []driver.NamedValue) string {
	t.Helper()
	values := make([]any, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	rows, err := conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, values...)
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

// The per-root node read's SQL (generated ListBatchNodes) contains this
// exact substring (internal/db/batch_nodes.sql.go).
const batchListNodeReadNeedle = "WHERE root_id = ?1 AND owner_session_id = ?2"

// The roots-list SQL (generated ListLiveBatchRootsForOwner) is identified by
// its state filter, unique among the captured statements in these tests.
const batchRootListNeedle = "state IN ('pending', 'starting', 'running', 'cancelling')"

// TestBatchLiveRootVanishRetriesOnFreshSnapshot: the first node read of the
// listed root returns no rows (errBatchLiveRootVanished); the dispatch's
// fresh-snapshot retry must recover on a NEW transaction that SEES changes
// committed after the first attempt. Between attempt 1's rollback and
// attempt 2's roots list the test commits a second root (B); attempt 2 must
// list both roots — B's presence proves the retry re-listed on a fresh
// snapshot. Transaction observables: the driver-level tx event log must read
// begin, rollback, begin, rollback (one rollback per attempt, the retry in a
// NEW tx). MUTATIONS caught — retry inside the SAME transaction (B invisible
// -> the Len(Own, 2) assertion fails); no rollback between attempts (the
// event sequence differs); unbounded retry (the begin count/event count
// differ).
func TestBatchLiveRootVanishRetriesOnFreshSnapshot(t *testing.T) {
	s, _, _, ctx, dataDir := batchServedTestEnv(t)
	root := batchReserveForOwner(t, s, ctx, "owner", "vanish-retry")
	logDB, log := newBatchSQLLogDB(t, filepath.Join(dataDir, "rush.db"))
	raw, err := sql.Open("sqlite", filepath.Join(dataDir, "rush.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	reader := batchReadTestReader(t, logDB)
	reader.TxStarter = logDB
	log.Intercept = func(query string, invocation int) bool {
		// Suppress the FIRST node read -> the vanish -> the retry.
		if strings.Contains(query, batchListNodeReadNeedle) {
			return invocation == 1
		}
		// The SECOND roots-list execution is attempt 2's first read: its tx
		// snapshot begins with this query. Commit root B right before it: a
		// fresh snapshot lists both roots; a reused attempt-1 snapshot (fixed
		// before B existed) lists only A.
		if strings.Contains(query, batchRootListNeedle) && invocation == 2 {
			batchReserveForOwner(t, NewBatchStore(raw), ctx, "owner", "vanish-retry-b")
		}
		return false
	}
	t.Cleanup(func() { log.Intercept = nil })
	res := reader.BatchLiveForOwner(ctx, "owner")
	require.NoError(t, res.Err)
	require.True(t, res.Complete)
	require.Len(t, res.Own, 2, "attempt 2 must see the fresh snapshot: both roots")
	rootIDs := make(map[string]bool, len(res.Own))
	for _, n := range res.Own {
		rootIDs[n.Ref.RootID] = true
	}
	require.True(t, rootIDs[root], "the original root stays in the fresh snapshot")
	// Transaction observables: exactly one begin+rollback per attempt, in
	// that order — the first attempt's tx is rolled back before the retry
	// begins.
	require.Equal(t, []string{"begin", "rollback", "begin", "rollback"}, log.txEventsSnapshot())
	// Node reads are per listed root: attempt 1 lists one root (its read is
	// suppressed), attempt 2 lists both roots and reads each - three in total.
	require.Equal(t, 3, batchSQLLogCountContaining(log, batchListNodeReadNeedle), "one suppressed read in attempt 1, one per root in attempt 2")
}

// TestBatchLiveRootVanishNeverCompletesEmpty: EVERY node read is suppressed;
// after the one retry the result must stay INCOMPLETE with the vanish error —
// never a silently complete empty scope. Transaction observables pin the
// bounded retry at the driver level: exactly two begin/rollback pairs and
// exactly TWO node reads — never a third attempt, never a complete-empty
// scope. MUTATIONS — deleting the len(nodes)==0 guard in
// batchLiveNodesForOwnersServed silently completes an empty scope ->
// the Err/Complete/HasLive asserts fail; an unbounded retry (or deleting the
// rollback between attempts) fails the event/node-read count asserts.
func TestBatchLiveRootVanishNeverCompletesEmpty(t *testing.T) {
	s, _, _, ctx, dataDir := batchServedTestEnv(t)
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	batchReserveForOwner(t, s, ctx, "owner", "vanish-empty")
	logDB, log := newBatchSQLLogDB(t, filepath.Join(dataDir, "rush.db"))
	reader := batchReadTestReader(t, logDB)
	reader.TxStarter = logDB
	reads := 0
	log.Intercept = func(query string, _ int) bool {
		if !strings.Contains(query, batchListNodeReadNeedle) {
			return false
		}
		// An unbounded retry would loop forever: cancel on the third read so the
		// mutation fails the count assertions instead of hanging the test.
		reads++
		if reads > 2 {
			cancel()
		}
		return true
	}
	t.Cleanup(func() { log.Intercept = nil })
	res := reader.BatchLiveForOwner(ctx, "owner")
	require.NotNil(t, res.Err)
	require.ErrorIs(t, res.Err, errBatchLiveRootVanished)
	require.False(t, res.Complete, "a vanished root must never read as complete")
	require.True(t, res.HasLive(), "incomplete stays conservatively live")
	require.Equal(t, []string{"begin", "rollback", "begin", "rollback"}, log.txEventsSnapshot())
	require.Equal(t, 2, batchSQLLogCountContaining(log, batchListNodeReadNeedle), "exactly TWO attempts, then stop: never a third attempt, never a complete-empty scope")
}
