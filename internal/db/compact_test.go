package db

// Coverage for Compact/Stats/CheckpointTruncate (task #1161): a bloated
// temporary database compacts down to its live bytes with a zero freelist,
// row counts survive, integrity_check stays clean; an open reader
// transaction elsewhere makes Compact refuse with a busy error and leave
// the file untouched.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// compactTestDB opens a fresh temporary database and returns the writer
// connection plus the resolved dataDir (ReleaseAll is registered as
// cleanup so the -wal/-shm handles don't outlive the test).
func compactTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ReleaseAll(dataDir)) })
	return conn, dataDir
}

// bloatTable fills a throwaway table with roughly wantBytes of payload and
// then drops it, handing all those pages to the freelist - the shape
// `sessions gc`/`purge` leave behind. A throwaway table (not production
// schema) keeps the fixture honest about what is being measured: freelist
// pages that VACUUM must reclaim.
func bloatTable(t *testing.T, conn *sql.DB, wantBytes int) {
	t.Helper()
	ctx := context.Background()
	_, err := conn.ExecContext(ctx, "CREATE TABLE compact_bloat_probe (id INTEGER PRIMARY KEY, payload BLOB)")
	require.NoError(t, err)
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	var inserted int
	for inserted < wantBytes {
		_, err = conn.ExecContext(ctx, "INSERT INTO compact_bloat_probe (payload) VALUES (?)", payload)
		require.NoError(t, err)
		inserted += len(payload)
	}
	_, err = conn.ExecContext(ctx, "DROP TABLE compact_bloat_probe")
	require.NoError(t, err)
	require.NoError(t, CheckpointTruncate(ctx, conn))
}

func TestCompactReclaimsFreelist(t *testing.T) {
	conn, _ := compactTestDB(t)
	ctx := context.Background()

	// A live table whose rows must survive the compaction byte-for-byte.
	_, err := conn.ExecContext(ctx, "CREATE TABLE compact_keep_probe (id INTEGER PRIMARY KEY, payload TEXT)")
	require.NoError(t, err)
	for i := range 10 {
		_, err = conn.ExecContext(ctx, "INSERT INTO compact_keep_probe (payload) VALUES (?)", string(rune('a'+i)))
		require.NoError(t, err)
	}

	bloatTable(t, conn, 4<<20)

	before, err := Stats(ctx, conn)
	require.NoError(t, err)
	require.Greater(t, before.FreeBytes, int64(1<<20), "fixture must leave freelist pages")
	require.Greater(t, before.PageCount, before.FreelistCount)

	after, err := Compact(ctx, conn)
	require.NoError(t, err)

	require.Equal(t, int64(0), after.FreelistCount, "compact must drain the freelist")
	require.Less(t, after.Bytes, before.Bytes, "file must shrink")
	require.Less(t, after.Bytes, before.FreeBytes, "file must be smaller than the freed space alone")
	require.Less(t, after.LiveBytes(), int64(2<<20), "live bytes must survive bloat removal")

	var kept int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM compact_keep_probe").Scan(&kept))
	require.Equal(t, 10, kept, "live rows must survive compaction")

	verdict, err := IntegrityCheck(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, "ok", verdict)
}

// dbFileSize stats the main database file on disk. The on-disk size, not
// PRAGMA page_count, is the observable a compaction moves: after a VACUUM
// that has not been checkpointed, page_count already reports the new
// (smaller) page layout while the main file on disk is unchanged - the
// rewritten pages sit in the WAL. Observed on the modernc driver; the
// tests below therefore assert on os.Stat.
func dbFileSize(t *testing.T, dataDir string) int64 {
	t.Helper()
	st, err := os.Stat(filepath.Join(dataDir, "rush.db"))
	require.NoError(t, err)
	return st.Size()
}

func TestCompactWithoutFinalCheckpointLeavesFileBig(t *testing.T) {
	// REVERT CHECK for Compact's second checkpoint: a mutation that drops
	// the final wal_checkpoint(TRUNCATE) leaves the VACUUM's output in the
	// WAL sidecar, so the main file never shrinks on disk and this test
	// goes red. (PRAGMA page_count cannot catch this - it reports the new
	// layout immediately; see dbFileSize's doc.)
	conn, dataDir := compactTestDB(t)
	ctx := context.Background()

	bloatTable(t, conn, 2<<20)
	sizeBefore := dbFileSize(t, dataDir)

	require.NoError(t, CheckpointTruncate(ctx, conn))
	_, err := conn.ExecContext(ctx, "PRAGMA temp_store=FILE")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "VACUUM")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "PRAGMA temp_store=MEMORY")
	require.NoError(t, err)

	sizeAfter := dbFileSize(t, dataDir)
	require.GreaterOrEqual(t, sizeAfter, sizeBefore-int64(4096),
		"without the final checkpoint the main file must not have shrunk on disk")
}

func TestCompactRefusesWhileReaderHoldsTransaction(t *testing.T) {
	// REVERT CHECK for CheckpointTruncate's busy check: a mutation that
	// drops the busy probe lets Compact finish without ever reporting the
	// contention; this test goes red on the refusal assertion.
	//
	// Observed driver behavior (modernc): an open read transaction on a
	// second connection does not stop the VACUUM itself from committing -
	// the reader keeps its snapshot through the WAL - but the final
	// TRUNCATE checkpoint cannot proceed and busy-times out, so Compact
	// returns an error and the main file on disk keeps its pre-compact
	// size. Those two observables are what this test pins.
	conn, dataDir := compactTestDB(t)
	ctx := context.Background()

	bloatTable(t, conn, 1<<20)
	sizeBefore := dbFileSize(t, dataDir)

	reader, err := sql.Open("sqlite", filepath.Join(dataDir, "rush.db"))
	require.NoError(t, err)
	defer reader.Close()
	tx, err := reader.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	var probe int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema").Scan(&probe))

	_, err = Compact(ctx, conn)
	require.Error(t, err, "compact must refuse while a reader transaction is open")

	require.Equal(t, sizeBefore, dbFileSize(t, dataDir),
		"main file on disk must be untouched by a refused compact")
}

func TestStatsWALBytes(t *testing.T) {
	conn, dataDir := compactTestDB(t)
	ctx := context.Background()

	stats, err := Stats(ctx, conn)
	require.NoError(t, err)
	require.Positive(t, stats.PageSize)
	require.Greater(t, stats.Bytes, int64(0))
	require.Equal(t, stats.PageSize*stats.PageCount, stats.Bytes)

	// Touch the WAL so Stats must report a non-zero sidecar size.
	_, err = conn.ExecContext(ctx, "CREATE TABLE compact_wal_probe (id INTEGER PRIMARY KEY)")
	require.NoError(t, err)
	walSt, err := os.Stat(filepath.Join(dataDir, "rush.db-wal"))
	require.NoError(t, err)
	require.Greater(t, walSt.Size(), int64(0))

	stats, err = Stats(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, walSt.Size(), stats.WALBytes)
}

func TestDiskFreeBytes(t *testing.T) {
	available, total, err := DiskFreeBytes(t.TempDir())
	require.NoError(t, err)
	require.Greater(t, available, uint64(0))
	require.Greater(t, total, uint64(0))
	require.LessOrEqual(t, available, total)
}
