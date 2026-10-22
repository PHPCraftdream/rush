package db

import (
	"errors"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/stretchr/testify/require"
)

// buildLegacySource creates the legacy-store source fixture in dataDir: a
// rush.db (storepath.LegacyFilename, the legacy import source since H4 made
// storepath.StoreV1Filename the active store) fully migrated through the
// legacy chain and seeded with one marker session. The connection is closed before returning
// so the file state is durable for the import.
func buildLegacySource(t *testing.T, dataDir string) {
	t.Helper()
	path := storepath.DataDirPath(dataDir, storepath.LegacyFilename)
	conn := openTestDB(t, path)
	fsys, err := legacyMigrationFS()
	require.NoError(t, err)
	require.NoError(t, migrateLocked(t.Context(), conn, "", fsys, MigrateOptions{MayMigrate: true}))
	_, err = conn.ExecContext(t.Context(),
		`INSERT INTO sessions (id, title, updated_at, created_at) VALUES ('legacy-sess', 'legacy-sess', 1700000000, 1700000000)`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

// Scenario 1: a fresh data dir has no legacy source, so the import is a
// no-op and Connect opens and migrates the store normally.
func TestConnectImport_FreshDir(t *testing.T) {
	dataDir := t.TempDir()

	conn, err := Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ReleaseConn(conn) })

	require.FileExists(t, storepath.StoreV1Path(dataDir))
	require.NoFileExists(t, storepath.DataDirPath(dataDir, storepath.LegacyFilename))
}

// Scenario 2: a data dir holding only the legacy rush.db gets it imported
// into the store-v1 destination, marker row included.
//
// storepath.ActiveFilename is storepath.StoreV1Filename (H4): after the import
// Connect opens the store-v1 file as its destination and succeeds; the legacy
// rush.db is only the import source.
func TestConnectImport_LegacySourceImported(t *testing.T) {
	dataDir := t.TempDir()
	buildLegacySource(t, dataDir)

	conn, err := Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ReleaseConn(conn) })

	require.FileExists(t, storepath.StoreV1Path(dataDir))

	var id string
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT id FROM sessions WHERE id = 'legacy-sess'`).Scan(&id))
	require.Equal(t, "legacy-sess", id)
}

// Scenario 3: with MayMigrate(false) the permission gate also suppresses the
// legacy import and refuses to apply the pending store-v1 migration: Connect
// fails with SchemaMigrationNotAllowedError and nothing of the legacy source
// is imported (the destination, if sqlite created the empty file on open, has
// no sessions table).
func TestConnectImport_MayMigrateFalseRefuses(t *testing.T) {
	dataDir := t.TempDir()
	buildLegacySource(t, dataDir)

	_, err := Connect(t.Context(), dataDir, WithMayMigrate(false))
	require.Error(t, err)
	var notAllowed *SchemaMigrationNotAllowedError
	require.True(t, errors.As(err, &notAllowed),
		"a pending store-v1 migration with MayMigrate(false) must be refused, got: %v", err)

	if _, statErr := os.Stat(storepath.StoreV1Path(dataDir)); statErr == nil {
		dest := openTestDB(t, storepath.StoreV1Path(dataDir))
		var n int
		require.NoError(t, dest.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`).Scan(&n))
		require.Zero(t, n, "the suppressed import must leave no legacy tables in the destination")
	}
}

// Scenario 4: an existing store-v1 destination wins -- the import must not
// touch it (ImportLegacyStore no-ops when the destination exists).
func TestConnectImport_DestinationWins(t *testing.T) {
	dataDir := t.TempDir()
	dest := storepath.StoreV1Path(dataDir)

	seeded := openTestDB(t, dest)
	fsys, err := migrationFS()
	require.NoError(t, err)
	require.NoError(t, migrateLocked(t.Context(), seeded, "", fsys, MigrateOptions{MayMigrate: true}))
	require.NoError(t, seeded.Close())

	buildLegacySource(t, dataDir)

	conn, err := Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ReleaseConn(conn) })

	var n int
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sessions WHERE id = 'legacy-sess'`).Scan(&n))
	require.Zero(t, n, "an existing destination must not receive the legacy source's rows")
}
