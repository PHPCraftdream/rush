// The run-cost migration's backfill, proven against a real goose Up on the
// real migration file (#1130, design sec.3): the per-node invariant
//
//	subtree_sum(cost_self) − cost_base(X)  ==  X.cost + Σ_{D∈desc(X)} owed(D)
//
// holds for every CONSISTENT node (cost ≥ parent_cost_accounted on each row).
// A deliberately corrupt row (cost < accounted) takes the s=0 branch: its own
// ledger keeps its full cost and no budget double-counts the phantom
// transfer — attribution there is approximate by design (documented P3).

package db

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCostFixture seeds a pre-migration tree through legacy columns only, on
// a database migrated to the version BEFORE the run-cost migration:
//
//	R  root                 cost 0.30, accounted 0.00   (own 0.10 + received 0.20)
//	C  delegation child     cost 0.20, accounted 0.20   (fully transferred out)
//	G  grandchild           cost 0.05, accounted 0.02   (partial: owed 0.03)
//	F  root w/ corrupt row  cost 0.01, accounted 0.00
//	FC corrupt child of F   cost 0.04, accounted 0.06   (cost < accounted: s=0)
func setupRunCostFixture(t *testing.T) (context.Context, *goose.Provider, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := openDB(t.TempDir() + "/runcost.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	migrationFS, err := fs.Sub(FS, "migrations")
	require.NoError(t, err)
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, migrationFS, goose.WithLogger(goose.NopLogger()))
	require.NoError(t, err)

	// Up to just BEFORE the run-cost migration, so the fixture writes
	// legacy-shaped rows (no cost_self/cost_base/cost_parent_id yet).
	_, err = provider.UpTo(ctx, 20260929000005)
	require.NoError(t, err)

	type row struct {
		id, parent, cost, accounted string
	}
	for _, r := range []row{
		{`'r'`, `NULL`, `0.30`, `0.00`},
		{`'msg1$$call1'`, `'r'`, `0.20`, `0.20`},
		{`'msg2$$call2'`, `'msg1$$call1'`, `0.05`, `0.02`},
		{`'f'`, `NULL`, `0.01`, `0.00`},
		{`'msg3$$call3'`, `'f'`, `0.04`, `0.06`},
	} {
		_, err = conn.ExecContext(ctx, `INSERT INTO sessions (id, parent_session_id, title, cost, parent_cost_accounted, updated_at, created_at)
			VALUES (`+r.id+`, `+r.parent+`, 'x', `+r.cost+`, `+r.accounted+`, 1700000000, 1700000000)`)
		require.NoError(t, err)
	}

	// THE migration, for real: edge detection by the id scheme and both
	// backfill statements run exactly as production will.
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	return ctx, provider, conn
}

// scalar reads one float out of the fixture database.
func scalar(t *testing.T, conn *sql.DB, query string, args ...any) float64 {
	t.Helper()
	var v float64
	require.NoError(t, conn.QueryRowContext(context.Background(), query, args...).Scan(&v))
	return v
}

// subtreeSelf sums cost_self over X's delegation subtree (the same shape the
// GetSubtreeSpent query uses).
const subtreeSelfSQL = `
WITH RECURSIVE sub(node) AS (
    SELECT id FROM sessions WHERE id = ?
    UNION
    SELECT s.id FROM sessions s JOIN sub ON s.cost_parent_id = sub.node
) SELECT COALESCE(SUM(n.cost_self), 0) FROM sub JOIN sessions n ON n.id = sub.node`

const subtreeBaseSQL = `
WITH RECURSIVE sub(node) AS (
    SELECT id FROM sessions WHERE id = ?
    UNION
    SELECT s.id FROM sessions s JOIN sub ON s.cost_parent_id = sub.node
)
SELECT COALESCE(SUM(max(COALESCE((
    SELECT SUM(CASE WHEN C.cost >= C.parent_cost_accounted THEN C.parent_cost_accounted ELSE 0 END)
    FROM sessions C WHERE C.cost_parent_id = sub.node), 0) - N.cost, 0)), 0)
FROM sub JOIN sessions n ON n.id = sub.node`

const subtreeOwedDescSQL = `
-- Owed over the STRICT descendants of X: the invariant's Σ_{D∈desc(X)}
-- owed(D); X's own cost is added separately as X.cost.
WITH RECURSIVE sub(node) AS (
    SELECT s.id FROM sessions s WHERE s.cost_parent_id = ?
    UNION
    SELECT s.id FROM sessions s JOIN sub ON s.cost_parent_id = sub.node
)
SELECT COALESCE(SUM(max(n.cost - n.parent_cost_accounted, 0)), 0)
FROM sub JOIN sessions n ON n.id = sub.node`

func TestRunCostMigration_BackfillInvariant(t *testing.T) {
	ctx, _, conn := setupRunCostFixture(t)
	_ = ctx

	invariant := func(t *testing.T, id string) {
		t.Helper()
		lhs := scalar(t, conn, subtreeSelfSQL, id) - scalar(t, conn, subtreeBaseSQL, id)
		cost := scalar(t, conn, `SELECT cost FROM sessions WHERE id = ?`, id)
		owed := scalar(t, conn, subtreeOwedDescSQL, id)
		assert.InDelta(t, cost+owed, lhs, 1e-9, "backfill invariant at node %s", id)
	}

	rows, err := conn.QueryContext(context.Background(), `SELECT id, cost, parent_cost_accounted, cost_self, cost_base, cost_parent_id FROM sessions`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id, cp string
		var cost, acc, self, base float64
		require.NoError(t, rows.Scan(&id, &cost, &acc, &self, &base, &cp))
		t.Logf("DUMP id=%s cost=%v acc=%v self=%v base=%v cp=%q", id, cost, acc, self, base, cp)
	}
	require.NoError(t, rows.Err())

	// The consistent nodes: root, mid, grandchild — the partial transfer
	// telescopes exactly (design fixture cases 1–2).
	invariant(t, "r")
	invariant(t, "msg1$$call1")
	invariant(t, "msg2$$call2")

	// The corrupt row (cost < accounted) takes the s=0 branch: attribution
	// there is approximate BY DESIGN (documented P3) — the strict equality
	// above holds only for consistent rows. What must still hold is the
	// no-double-count budget: f's ledger keeps its full cost, its base is 0.
	assert.InDelta(t, 0.01, scalar(t, conn, `SELECT cost_self FROM sessions WHERE id = 'f'`), 1e-9)
	assert.InDelta(t, 0.0, scalar(t, conn, `SELECT cost_base FROM sessions WHERE id = 'f'`), 1e-9)
	assert.InDelta(t, 0.05, scalar(t, conn, subtreeSelfSQL, "f"), 1e-9,
		"f's subtree sums its own 0.01 and the corrupt child's full 0.04, no phantom transfer stripped twice")

	// Budget semantics on the migrated tree: the root's budget equals the
	// old displayed "cost including children" (0.30 + the child's residual
	// 0.03 owed); the mid node keeps its own 0.18 (its row held 0.20, of
	// which 0.02 was G's transferred-up spend), and the corrupt row keeps its
	// full cost in its own ledger (s=0 branch).
	rootBudget := scalar(t, conn, subtreeSelfSQL, "r") - scalar(t, conn, `SELECT cost_base FROM sessions WHERE id = 'r'`)
	assert.InDelta(t, 0.33, rootBudget, 1e-9, "root budget = legacy display (0.30) + child's residual (0.03)")
	costSelfC := scalar(t, conn, `SELECT cost_self FROM sessions WHERE id = 'msg1$$call1'`)
	assert.InDelta(t, 0.18, costSelfC, 1e-9,
		"C keeps its own 0.18: its row held 0.20, of which 0.02 was G's transferred-up spend")
	costSelfCorrupt := scalar(t, conn, `SELECT cost_self FROM sessions WHERE id = 'msg3$$call3'`)
	assert.InDelta(t, 0.04, costSelfCorrupt, 1e-9, "corrupt row (cost < accounted): s=0, its full cost is its own")

	// Idempotency (design T4): re-running BOTH backfill statements changes
	// nothing — the formulas read the original `cost`, which the backfill
	// never modifies.
	beforeSelf := scalar(t, conn, `SELECT COALESCE(SUM(cost_self), 0) FROM sessions`)
	beforeBase := scalar(t, conn, `SELECT COALESCE(SUM(cost_base), 0) FROM sessions`)
	_, err = conn.ExecContext(context.Background(), `
UPDATE sessions SET
  cost_self = max(cost - COALESCE((
    SELECT SUM(CASE WHEN C.cost >= C.parent_cost_accounted THEN C.parent_cost_accounted ELSE 0 END)
    FROM sessions C WHERE C.cost_parent_id = sessions.id), 0), 0)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `
WITH RECURSIVE sub(root, node) AS (
  SELECT id, id FROM sessions
  UNION
  SELECT sub.root, s.id FROM sessions s JOIN sub ON s.cost_parent_id = sub.node
)
UPDATE sessions SET cost_base = COALESCE((
  SELECT SUM(max(COALESCE((
    SELECT SUM(CASE WHEN C.cost >= C.parent_cost_accounted THEN C.parent_cost_accounted ELSE 0 END)
    FROM sessions C WHERE C.cost_parent_id = sub.node), 0) - N.cost, 0))
  FROM sub JOIN sessions N ON N.id = sub.node
  WHERE sub.root = sessions.id
), 0)`)
	require.NoError(t, err)
	assert.InDelta(t, beforeSelf, scalar(t, conn, `SELECT COALESCE(SUM(cost_self), 0) FROM sessions`), 1e-9)
	assert.InDelta(t, beforeBase, scalar(t, conn, `SELECT COALESCE(SUM(cost_base), 0) FROM sessions`), 1e-9)
}

// TestRunCostMigration_DownAndUp keeps the rollback path honest for the
// run-cost migration specifically: Down drops the three columns (and their
// index) without breaking later-migration objects, and Up re-creates them.
func TestRunCostMigration_DownAndUp(t *testing.T) {
	ctx := context.Background()
	conn, err := openDB(t.TempDir() + "/runcost_updown.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	migrationFS, err := fs.Sub(FS, "migrations")
	require.NoError(t, err)
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, migrationFS, goose.WithLogger(goose.NopLogger()))
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)

	_, err = provider.DownTo(ctx, 20260929000005)
	require.NoError(t, err)
	for _, col := range []string{"cost_self", "cost_base", "cost_parent_id"} {
		var n int
		require.NoError(t, conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = ?`, col).Scan(&n))
		assert.Zero(t, n, "column %s must be gone after Down", col)
	}

	_, err = provider.Up(ctx)
	require.NoError(t, err)
	for _, col := range []string{"cost_self", "cost_base", "cost_parent_id"} {
		var n int
		require.NoError(t, conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = ?`, col).Scan(&n))
		assert.Equal(t, 1, n, "column %s must be back after Up", col)
	}
}
