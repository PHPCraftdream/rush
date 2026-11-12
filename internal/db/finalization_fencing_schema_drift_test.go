package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db/dbtest"
	"github.com/stretchr/testify/require"
)

// fencingSchemaObjects returns name -> DDL for the tables of the finalization
// fence and every index on them, as stored in sqlite_master.
func fencingSchemaObjects(t *testing.T, ctx context.Context, conn *sql.DB) map[string]string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, `
SELECT name, sql FROM sqlite_master
WHERE sql IS NOT NULL AND (
  (type = 'table' AND name IN ('batch_nodes', 'session_agent_finalizations',
      'session_agent_finalization_tickets', 'session_agent_finalization_continuations', 'orphan_call_outbox'))
  OR (type = 'index' AND tbl_name IN ('batch_nodes', 'session_agent_finalizations',
      'session_agent_finalization_tickets', 'session_agent_finalization_continuations', 'orphan_call_outbox')))`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, ddl string
		require.NoError(t, rows.Scan(&name, &ddl))
		out[name] = ddl
	}
	require.NoError(t, rows.Err())
	return out
}

// TestFinalizationFencingSchemaMatchesMigrations pins dbtest's copy of the
// fence tables to the migrated production schema, so a migration that changes
// them cannot leave hand-written fixtures testing a stale shape.
func TestFinalizationFencingSchemaMatchesMigrations(t *testing.T) {
	ctx := context.Background()

	prod, err := openDB(t.TempDir() + "/prod.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = prod.Close() })
	require.NoError(t, Migrate(ctx, prod))

	fixture, err := openDB(t.TempDir() + "/fixture.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = fixture.Close() })
	_, err = fixture.ExecContext(ctx, `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	require.NoError(t, dbtest.ApplyFinalizationFencingSchema(ctx, fixture))
	// Idempotent: fixtures that already created some of the tables stay intact.
	require.NoError(t, dbtest.ApplyFinalizationFencingSchema(ctx, fixture))

	want := fencingSchemaObjects(t, ctx, prod)
	require.NotEmpty(t, want)
	require.Equal(t, want, fencingSchemaObjects(t, ctx, fixture))
}
