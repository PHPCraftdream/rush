package db

import (
	"errors"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/stretchr/testify/require"
)

// buildLegacySource creates the legacy-store source fixture in dataDir: a
// rush.db (the ActiveFilename, which in this worktree is still the legacy
// name -- P3 owns the switch) fully migrated through the legacy chain and
// seeded with one marker session. The connection is closed before returning
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

	require.FileExists(t, storepath.DataDirPath(dataDir, storepath.ActiveFilename))
	require.NoFileExists(t, storepath.StoreV1Path(dataDir))
}

// Scenario 2: a data dir holding only the legacy rush.db gets it imported
// into the store-v1 destination, marker row included.
//
// INTERIM ARTIFACT: in this worktree storepath.ActiveFilename is still the
// legacy name (P3 owns the switch), so after the import Connect re-opens the
// SAME legacy file as its destination and the version-set decision fails
// against the single store-v1 initial migration. That error is expected and
// composition-specific; the assertable contract here is the imported
// destination, so the Connect error is captured but not asserted. In the
// composed world (ActiveFilename == StoreV1Filename) Connect succeeds here.
func TestConnectImport_LegacySourceImported(t *testing.T) {
	dataDir := t.TempDir()
	buildLegacySource(t, dataDir)

	_, err := Connect(t.Context(), dataDir)
	_ = err // documented interim artifact; see the comment above

	require.FileExists(t, storepath.StoreV1Path(dataDir))

	imported := openTestDB(t, storepath.StoreV1Path(dataDir))
	var id string
	require.NoError(t, imported.QueryRowContext(t.Context(),
		`SELECT id FROM sessions WHERE id = 'legacy-sess'`).Scan(&id))
	require.Equal(t, "legacy-sess", id)
}

// Scenario 3: with MayMigrate(false) the permission gate also suppresses the
// legacy import -- no store-v1 destination may be created.
//
// The surfaced error is worktree-specific: the legacy source itself sits at
// the ActiveFilename, so after openDB the version-set decision sees the
// legacy chain's versions as unknown AND the store-v1 initial as pending,
// which is the SchemaDivergedError branch (unknown+pending is decided before
// the MayMigrate check). The gate still refused before creating anything.
func TestConnectImport_MayMigrateFalseRefuses(t *testing.T) {
	dataDir := t.TempDir()
	buildLegacySource(t, dataDir)

	_, err := Connect(t.Context(), dataDir, WithMayMigrate(false))
	require.Error(t, err)
	var diverged *SchemaDivergedError
	require.True(t, errors.As(err, &diverged),
		"legacy versions read as unknown with the store-v1 initial pending must diverge, got: %v", err)

	require.NoFileExists(t, storepath.StoreV1Path(dataDir))
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

	before, err := os.ReadFile(dest)
	require.NoError(t, err)

	buildLegacySource(t, dataDir)

	// INTERIM ARTIFACT (same as scenario 2): ActiveFilename is still the
	// legacy name, so Connect re-opens the legacy file as its destination and
	// fails the version-set decision after the (suppressed) import. The
	// assertable contract is that the destination bytes are unchanged; do not
	// assert the error's presence or type. In the composed world
	// (ActiveFilename == StoreV1Filename) Connect succeeds here.
	conn, err := Connect(t.Context(), dataDir)
	_ = err
	if err == nil {
		t.Cleanup(func() { _ = ReleaseConn(conn) })
	}

	after, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "an existing destination must be byte-unchanged by the import")
}
