package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

// laggingMigration is the migration the "one ALTER behind" fixtures drop: a
// plain ALTER, so removing it leaves a database this binary is happy to bring
// forward, one whose maximum applied version still exceeds the missing one.
const laggingMigration = "20260929000003_add_announce_message_id_to_async_jobs.sql"

const announceColumn = "announce_message_id"

// embeddedMigrationsFS copies the embedded migration set into a MapFS,
// optionally dropping files skipFn rejects. It is how a database that does
// not match this binary's set is produced.
func embeddedMigrationsFS(t *testing.T, skipFn func(name string) bool) fstest.MapFS {
	t.Helper()
	fsys, err := migrationFS()
	require.NoError(t, err)
	entries, err := fs.ReadDir(fsys, ".")
	require.NoError(t, err)
	out := fstest.MapFS{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if skipFn != nil && skipFn(entry.Name()) {
			continue
		}
		data, err := fs.ReadFile(fsys, entry.Name())
		require.NoError(t, err)
		out[entry.Name()] = &fstest.MapFile{Data: data}
	}
	require.NotEmpty(t, out)
	return out
}

// applyFS runs an arbitrary migration set against conn, which is how the
// fixtures build databases that do not match the embedded set.
func applyFS(t *testing.T, conn *sql.DB, fsys fs.FS) {
	t.Helper()
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, fsys,
		goose.WithLogger(goose.NopLogger()))
	require.NoError(t, err)
	_, err = provider.Up(context.Background())
	require.NoError(t, err)
}

// openTestDB opens an independent connection to dbPath with a single pooled
// connection, mirroring what connect hands to migrate.
func openTestDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	conn, err := openDB(dbPath)
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// newLaggingDB builds a database holding every embedded migration except
// laggingMigration, i.e. one ALTER behind this binary with a maximum applied
// version higher than the missing one.
func newLaggingDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "rush.db")
	conn := openTestDB(t, dbPath)
	applyFS(t, conn, embeddedMigrationsFS(t, func(name string) bool {
		return name == laggingMigration
	}))
	return dbPath, conn
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

// hasColumn reports whether table has the named column.
func hasColumn(t *testing.T, conn *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notNull int
			dflt    any
			pk      int
		)
		require.NoError(t, rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk))
		if name == column {
			return true
		}
	}
	require.NoError(t, rows.Err())
	return false
}

// recordUnknownVersion marks version as applied in the database without any
// file backing it, which is exactly what an older/newer binary leaves behind.
func recordUnknownVersion(t *testing.T, conn *sql.DB, version int64) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(),
		"INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)", version)
	require.NoError(t, err)
}

// TestMigrate_LockSerializesConcurrentMigrations proves the contract that
// makes a shared data directory safe after a deploy: while one process holds
// <dataDir>/migrate.lock, another cannot read the version table, so it cannot
// decide the same pending migration is still unapplied.
//
// The revert check is checked in manually: with the lock acquisition removed,
// both processes see the same pending ALTER, both apply it, and the second
// fails with "duplicate column name".
func TestMigrate_LockSerializesConcurrentMigrations(t *testing.T) {
	dbPath, first := newLaggingDB(t)
	fsys := embeddedMigrationsFS(t, nil)
	lockPath := filepath.Join(filepath.Dir(dbPath), "migrate.lock")
	second := openTestDB(t, dbPath)
	opts := MigrateOptions{MayMigrate: true, MigrationLockPath: lockPath}

	versionsRead := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	var once sync.Once
	prev := onVersionsRead
	onVersionsRead = func(path string) {
		if path != dbPath {
			return
		}
		once.Do(func() {
			close(versionsRead)
			select {
			case <-release:
			case <-time.After(30 * time.Second):
			}
		})
	}
	t.Cleanup(func() { onVersionsRead = prev })

	// Process A takes migrate.lock, reads the versions, and parks at the
	// seam while still holding it.
	go func() { firstDone <- migrateLocked(context.Background(), first, dbPath, fsys, opts) }()

	select {
	case <-versionsRead:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the first migrate to read versions")
	}

	// Process B must not be able to get anywhere: it is serialized behind
	// the lock A still holds.
	go func() { secondDone <- migrateLocked(context.Background(), second, dbPath, fsys, opts) }()
	select {
	case err := <-secondDone:
		t.Fatalf("second migrate completed while the first still held migrate.lock: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-firstDone:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the first migrate")
	}
	select {
	case err := <-secondDone:
		require.NoError(t, err, "the second migrate must succeed once the lock is free")
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the second migrate")
	}

	require.True(t, hasColumn(t, first, "async_jobs", announceColumn),
		"the pending ALTER must have been applied exactly once")
}

// TestMigrate_VersionSetRules covers the decision matrix: which combination
// of unknown (applied but not embedded) and pending (embedded but not
// applied) versions opens the database, applies migrations, or refuses.
func TestMigrate_VersionSetRules(t *testing.T) {
	full := embeddedMigrationsFS(t, nil)

	t.Run("unknown_and_pending_diverges", func(t *testing.T) {
		dbPath, conn := newLaggingDB(t)
		recordUnknownVersion(t, conn, 29990101000000)

		before := dbVersionSet(t, conn)

		var diverged *SchemaDivergedError
		err := migrate(context.Background(), conn, dbPath, full, MigrateOptions{MayMigrate: true})
		require.ErrorAs(t, err, &diverged)
		require.Contains(t, diverged.Unknown, int64(29990101000000))
		require.Contains(t, diverged.Pending, laggingVersion())

		require.Equal(t, before, dbVersionSet(t, conn), "a diverged schema must not be touched")
		require.False(t, hasColumn(t, conn, "async_jobs", announceColumn),
			"a diverged schema must not be migrated")
	})

	t.Run("unknown_only_opens_without_migrating", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "rush.db")
		conn := openTestDB(t, dbPath)
		applyFS(t, conn, full)
		recordUnknownVersion(t, conn, 29990101000000)

		before := dbVersionSet(t, conn)

		require.NoError(t, migrate(context.Background(), conn, dbPath, full,
			MigrateOptions{MayMigrate: true}))
		require.Equal(t, before, dbVersionSet(t, conn),
			"a database ahead of this binary must keep its versions")
	})

	t.Run("pending_below_db_max_applies_out_of_order", func(t *testing.T) {
		dbPath, conn := newLaggingDB(t)

		require.NoError(t, migrate(context.Background(), conn, dbPath, full,
			MigrateOptions{MayMigrate: true}))

		require.True(t, hasColumn(t, conn, "async_jobs", announceColumn),
			"the out-of-order pending migration must be applied")
		emitted, err := embeddedVersions(full)
		require.NoError(t, err)
		for _, v := range emitted {
			require.True(t, dbVersionSet(t, conn)[v], "version %d must be applied", v)
		}
	})

	t.Run("up_to_date_is_a_noop", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "rush.db")
		conn := openTestDB(t, dbPath)
		applyFS(t, conn, full)

		before := dbVersionSet(t, conn)

		require.NoError(t, migrate(context.Background(), conn, dbPath, full,
			MigrateOptions{MayMigrate: true}))
		require.Equal(t, before, dbVersionSet(t, conn))
	})
}

// laggingVersion parses the numeric prefix of the dropped migration file.
func laggingVersion() int64 {
	prefix := strings.SplitN(laggingMigration, "_", 2)[0]
	v, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		panic(err)
	}
	return v
}

// TestMigrate_NotAllowedRefusesPending proves a caller that opts out of
// migration gets a typed error and an untouched schema, so a dev build can be
// pointed at a shared data directory without writing to it.
func TestMigrate_NotAllowedRefusesPending(t *testing.T) {
	dbPath, conn := newLaggingDB(t)

	var notAllowed *SchemaMigrationNotAllowedError
	err := migrate(context.Background(), conn, dbPath, embeddedMigrationsFS(t, nil),
		MigrateOptions{MayMigrate: false})
	require.ErrorAs(t, err, &notAllowed)
	require.Contains(t, notAllowed.Pending, laggingVersion())

	require.False(t, hasColumn(t, conn, "async_jobs", announceColumn),
		"refusing to migrate must leave the schema untouched")
	require.Len(t, dbVersionSet(t, conn), len(mustEmbeddedVersions(t))-1)
}

func mustEmbeddedVersions(t *testing.T) []int64 {
	t.Helper()
	v, err := embeddedVersions(embeddedMigrationsFS(t, nil))
	require.NoError(t, err)
	return v
}

// TestConnect_MigratesLaggingDatabase is the integration check: the default
// Connect applies the pending migration and creates the cross-process lock
// file next to the database.
func TestConnect_MigratesLaggingDatabase(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "rush.db")
	seed := openTestDB(t, dbPath)
	applyFS(t, seed, embeddedMigrationsFS(t, func(name string) bool {
		return name == laggingMigration
	}))

	conn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ReleaseConn(conn) })

	require.True(t, hasColumn(t, conn, "async_jobs", announceColumn),
		"Connect must apply the pending migration")

	lockPath := filepath.Join(dataDir, "migrate.lock")
	require.FileExists(t, lockPath, "the lock file must live next to the database")
	_ = seed
}
