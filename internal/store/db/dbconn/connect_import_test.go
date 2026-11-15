package dbconn

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/store/db/dbtest"
	"github.com/PHPCraftdream/rush/internal/store/db/migrater"
	"github.com/PHPCraftdream/rush/internal/store/db/storepath"
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
	conn := dbtest.OpenSingle(t, path)
	fsys, err := migrater.LegacyFS()
	require.NoError(t, err)
	require.NoError(t, migrater.ApplyLocked(t.Context(), conn, "", fsys, migrater.MigrateOptions{MayMigrate: true}))
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

// Scenario 3: with MayMigrate(false) the permission gate suppresses legacy
// import and refuses a missing store-v1 destination before SQLite can create
// it. Connect fails with SchemaMigrationNotAllowedError and leaves the legacy
// source and destination untouched.
func TestConnectImport_MayMigrateFalseRefuses(t *testing.T) {
	dataDir := t.TempDir()
	buildLegacySource(t, dataDir)

	_, err := Connect(t.Context(), dataDir, WithMigrateOptions(migrater.WithMayMigrate(false)))
	require.Error(t, err)
	var notAllowed *migrater.SchemaMigrationNotAllowedError
	require.ErrorAs(t, err, &notAllowed)
	require.NotEmpty(t, notAllowed.Pending)
	require.Equal(t, (&migrater.SchemaMigrationNotAllowedError{Pending: notAllowed.Pending}).Error(), notAllowed.Error())
	require.Equal(t, "failed to apply migrations: "+notAllowed.Error(), err.Error())
	_, err = os.Stat(storepath.StoreV1Path(dataDir))
	require.ErrorIs(t, err, os.ErrNotExist, "refusing a missing destination must not create it")
	require.FileExists(t, storepath.DataDirPath(dataDir, storepath.LegacyFilename), "the legacy source must remain untouched")
}

// Scenario 4: after the migration gate refuses the import, a later Connect
// with migration allowed imports the still-present legacy source.
func TestConnectImport_MayMigrateFalseThenAllowed(t *testing.T) {
	dataDir := t.TempDir()
	buildLegacySource(t, dataDir)

	_, err := Connect(t.Context(), dataDir, WithMigrateOptions(migrater.WithMayMigrate(false)))
	require.Error(t, err)
	var notAllowed *migrater.SchemaMigrationNotAllowedError
	require.ErrorAs(t, err, &notAllowed)
	_, err = os.Stat(storepath.StoreV1Path(dataDir))
	require.True(t, errors.Is(err, os.ErrNotExist), "refusing a missing destination must not create it; stat error: %v", err)

	conn, err := Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ReleaseConn(conn) })

	var id string
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT id FROM sessions WHERE id = 'legacy-sess'`).Scan(&id))
	require.Equal(t, "legacy-sess", id)
}

// Scenario 5: a destination already migrated to the embedded head remains
// openable with MayMigrate(false); no legacy import is attempted or required.
func TestConnectImport_MayMigrateFalseAllowsAlreadyMigratedDestination(t *testing.T) {
	dataDir := t.TempDir()
	dest := storepath.StoreV1Path(dataDir)
	seed := dbtest.OpenSingle(t, dest)
	require.NoError(t, migrater.Migrate(t.Context(), seed))
	require.NoError(t, seed.Close())

	conn, err := Connect(t.Context(), dataDir, WithMigrateOptions(migrater.WithMayMigrate(false)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ReleaseConn(conn) })
	require.NoError(t, conn.PingContext(t.Context()))
	require.FileExists(t, dest)
}

// Scenario 6: an existing store-v1 destination wins -- the import must not
// touch it (ImportLegacyStore no-ops when the destination exists).
func TestConnectImport_DestinationWins(t *testing.T) {
	dataDir := t.TempDir()
	dest := storepath.StoreV1Path(dataDir)

	seeded := dbtest.OpenSingle(t, dest)
	fsys, err := migrater.ActiveFS()
	require.NoError(t, err)
	require.NoError(t, migrater.ApplyLocked(t.Context(), seeded, "", fsys, migrater.MigrateOptions{MayMigrate: true}))
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

// TestConnect_OpenUpToDateDatabase is the integration check: the default
// Connect opens an already fully migrated database without touching it
// (applied=set(initial), no pending version) and creates the cross-process
// lock file next to the database. The pending-migration path itself is
// covered by TestMigrate_LockSerializesConcurrentMigrations; Connect cannot
// see the test-only probe because it migrates with the embedded set.
func TestConnect_OpenUpToDateDatabase(t *testing.T) {
	dataDir := t.TempDir()
	// Seed the ACTIVE store file: Connect opens storepath.StoreV1Path, and
	// seeding the legacy name instead would silently exercise the legacy
	// import path, not the up-to-date open this test pins.
	dbPath := storepath.StoreV1Path(dataDir)
	seed := dbtest.OpenSingle(t, dbPath)
	require.NoError(t, migrater.Migrate(context.Background(), seed))

	conn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ReleaseConn(conn) })

	before := dbVersionSet(t, seed)
	require.Equal(t, before, dbVersionSet(t, conn), "Connect must not record versions on an up-to-date database")

	lockPath := filepath.Join(dataDir, "migrate.lock")
	require.FileExists(t, lockPath, "the lock file must live next to the database")
}

// dbVersionSet returns the real migration versions recorded in the database,
// excluding goose's version-0 sentinel row.
func dbVersionSet(t *testing.T, conn *sql.DB) map[int64]bool {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(),
		"SELECT version_id FROM goose_db_version WHERE is_applied = 1 AND version_id > 0")
	require.NoError(t, err)
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var v int64
		require.NoError(t, rows.Scan(&v))
		out[v] = true
	}
	require.NoError(t, rows.Err())
	return out
}
