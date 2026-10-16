package session

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

func batchStoreTestMigrationFS(t *testing.T, includeProjection bool) fstest.MapFS {
	t.Helper()
	entries, err := fs.ReadDir(db.FS, "migrations")
	require.NoError(t, err)
	files := fstest.MapFS{}
	for _, entry := range entries {
		if !includeProjection && (entry.Name() == "20261012000001_batch_projection.sql" ||
			entry.Name() == "20261012000002_batch_runner_incarnation.sql") {
			continue
		}
		data, e := fs.ReadFile(db.FS, "migrations/"+entry.Name())
		require.NoError(t, e)
		files[entry.Name()] = &fstest.MapFile{Data: data}
	}
	return files
}

func TestBatchMigrationUpgradeAndDownPreserveLegacy(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, batchStoreTestMigrationFS(t, true), goose.WithLogger(goose.NopLogger()))
	require.NoError(t, err)
	_, err = provider.Down(ctx)
	require.NoError(t, err)
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))
	before, err := db.New(conn).ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{OwnerSessionID: "owner", ToolCallID: "legacy", Kind: "command", InputHash: "hash", HostID: "legacy-host", ClaimID: "claim", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	old, err := goose.NewProvider(goose.DialectSQLite3, conn, batchStoreTestMigrationFS(t, false), goose.WithLogger(goose.NopLogger()))
	require.NoError(t, err)
	_, err = old.Up(ctx)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	after, err := db.New(conn).GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: "owner", ToolCallID: "legacy"})
	require.NoError(t, err)
	require.Equal(t, before, after)
	// Populate the new subtree to exercise Down with its self-referencing FKs.
	_, err = NewBatchStore(conn).Reserve(ctx, batchStoreTestReservation())
	require.NoError(t, err)
	_, err = provider.Down(ctx) // 20261012000002_batch_runner_incarnation (R11B additive)
	require.NoError(t, err)
	_, err = provider.Down(ctx) // 20261012000001_batch_projection
	require.NoError(t, err)
	for _, table := range []string{"batch_nodes", "batch_control_requests", "batch_runner_checkpoints"} {
		var count int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count))
		require.Zero(t, count)
	}
	for _, table := range []string{"async_jobs", "async_hosts", "session_notices", "sessions"} {
		var count int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count))
		require.Equal(t, 1, count)
	}
	after, err = db.New(conn).GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: "owner", ToolCallID: "legacy"})
	require.NoError(t, err)
	require.Equal(t, before, after)
}
