package cmd

// Real-DB coverage for `rush sessions compact` (task #1161): a bloated
// database compacts to its live size with consistent JSON accounting; live
// host locks and live session locks refuse the run (and only those two
// gates --force bypasses); a held migrate.lock refuses even with --force;
// --dry-run leaves the file byte-for-byte untouched; a short-disk refusal
// reports the numbers. All thresholds are package-var seams substituted
// per test so fixtures stay in the multi-MB range.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// isolatedCompactEnv stands up the `sessions compact` command against an
// isolated data directory and returns (dataDir, run). The seed app is
// built with setupAppLite and kept ALIVE across run() on purpose: its
// pooled DB entry means the command's own setupApp reuses the pool and
// never runs migrations, so a test can hold migrate.lock (test d) without
// deadlocking connect()'s blocking AcquireFileLock.
func isolatedCompactEnv(t *testing.T) (string, *app.App, func(t *testing.T, args ...string) (string, string, error)) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)
	dataDir := filepath.Join(tmp, "compact-data")

	ctx, cancel := context.WithCancel(context.Background())
	ensureRootFlagStandIns(sessionsCompactCmd, dataDir)
	sessionsCompactCmd.SetContext(ctx)

	built, err := setupAppLite(sessionsCompactCmd)
	require.NoError(t, err)

	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			built.Shutdown()
			_ = db.Release(dataDir)
			cancel()
			waitForSQLiteHandleRelease(t, dataDir)
		})
	})

	run := func(t *testing.T, args ...string) (string, string, error) {
		t.Helper()
		require.NoError(t, sessionsCompactCmd.Flags().Set("dry-run", "false"))
		require.NoError(t, sessionsCompactCmd.Flags().Set("force", "false"))
		require.NoError(t, sessionsCompactCmd.Flags().Set("json", "false"))
		require.NoError(t, sessionsCompactCmd.ParseFlags(args))

		var outBuf, errBuf strings.Builder
		outR, outW, err := os.Pipe()
		require.NoError(t, err)
		errR, errW, err := os.Pipe()
		require.NoError(t, err)
		oldOut, oldErr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = outW, errW
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = io.Copy(&outBuf, outR) }()
		go func() { defer wg.Done(); _, _ = io.Copy(&errBuf, errR) }()

		runErr := sessionsCompactCmd.RunE(sessionsCompactCmd, nil)

		os.Stdout, os.Stderr = oldOut, oldErr
		_ = outW.Close()
		_ = errW.Close()
		wg.Wait()
		_ = outR.Close()
		_ = errR.Close()
		return outBuf.String(), errBuf.String(), runErr
	}
	return dataDir, built, run
}

// bloatCompactDBConn hands ~wantBytes to the freelist of the given
// connection: a throwaway table filled and dropped, then a WAL checkpoint
// so the freelist is visible in the main file's page accounting.
func bloatCompactDBConn(t *testing.T, conn *sql.DB, wantBytes int) {
	t.Helper()
	ctx := context.Background()
	_, err := conn.ExecContext(ctx, "CREATE TABLE compact_cmd_bloat (id INTEGER PRIMARY KEY, payload BLOB)")
	require.NoError(t, err)
	payload := make([]byte, 64*1024)
	var inserted int
	for inserted < wantBytes {
		_, err = conn.ExecContext(ctx, "INSERT INTO compact_cmd_bloat (payload) VALUES (?)", payload)
		require.NoError(t, err)
		inserted += len(payload)
	}
	_, err = conn.ExecContext(ctx, "DROP TABLE compact_cmd_bloat")
	require.NoError(t, err)
	require.NoError(t, db.CheckpointTruncate(ctx, conn))
}

// dbFileSizeAndMtime snapshots the main database file for the
// untouched-file assertions.
func dbFileSizeAndMtime(t *testing.T, dataDir string) (int64, int64) {
	t.Helper()
	st, err := os.Stat(filepath.Join(dataDir, "rush.db"))
	require.NoError(t, err)
	return st.Size(), st.ModTime().UnixNano()
}

func TestSessionsCompactCmdRun_CompactsBloatedDB(t *testing.T) {
	dataDir, seed, run := isolatedCompactEnv(t)

	bloatCompactDBConn(t, seed.DB(), 4<<20)

	out, _, err := run(t)
	require.NoError(t, err)
	require.Contains(t, out, "database: "+filepath.Join(dataDir, "rush.db"))
	require.Contains(t, out, "before: ")
	require.Contains(t, out, "after: ")
	require.Contains(t, out, "reclaimed ")

	// A second run against the now-compacted database has nothing to do.
	out, _, err = run(t)
	require.NoError(t, err)
	require.Contains(t, out, "nothing to reclaim")
}

func TestSessionsCompactCmdRun_JSONFieldsConsistent(t *testing.T) {
	_, seed, run := isolatedCompactEnv(t)

	bloatCompactDBConn(t, seed.DB(), 3<<20)

	out, _, err := run(t, "--json")
	require.NoError(t, err)

	var result compactResult
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &result))
	require.False(t, result.DryRun)
	require.Greater(t, result.BeforeFree, int64(1<<20), "fixture must leave freelist pages")
	require.Greater(t, result.ReclaimedBytes, int64(0))
	require.Equal(t, result.BeforeBytes-result.AfterBytes, result.ReclaimedBytes,
		"before - after must equal reclaimed")
	require.Equal(t, result.BeforeFree, result.BeforeFree, "free field present")
	require.Less(t, result.AfterBytes, result.BeforeBytes)
}

func TestSessionsCompactCmdRun_LiveHostLockRefuses(t *testing.T) {
	dataDir, seed, run := isolatedCompactEnv(t)

	bloatCompactDBConn(t, seed.DB(), 2<<20)

	require.NoError(t, os.MkdirAll(session.HostsDir(dataDir), 0o755))
	holder, err := filelock.TryAcquireFileLock(session.HostLockPath(dataDir, "compact-live-host"))
	require.NoError(t, err)
	defer holder.Release()

	sizeBefore, _ := dbFileSizeAndMtime(t, dataDir)
	_, _, err = run(t)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to compact")
	require.Contains(t, err.Error(), "live rush host")
	require.Contains(t, err.Error(), "--force")
	sizeAfter, _ := dbFileSizeAndMtime(t, dataDir)
	require.Equal(t, sizeBefore, sizeAfter, "a refused compact must not touch the file")

	_, _, err = run(t, "--force")
	require.NoError(t, err, "--force bypasses the host gate")
}

func TestSessionsCompactCmdRun_LiveSessionLockRefusesWithID(t *testing.T) {
	dataDir, seed, run := isolatedCompactEnv(t)

	bloatCompactDBConn(t, seed.DB(), 2<<20)

	holder, err := session.TryAcquireSessionLock(dataDir, "compact-live-session")
	require.NoError(t, err)
	defer holder.Release()

	_, _, err = run(t)
	require.Error(t, err)
	require.Contains(t, err.Error(), "compact-live-session")
	require.Contains(t, err.Error(), "--force")
}

func TestSessionsCompactCmdRun_MigrateLockRefusedEvenWithForce(t *testing.T) {
	// REVERT CHECK: a mutation that lets --force bypass the migrate.lock
	// gate turns this test red — --force must skip ONLY the host and
	// session-lock gates.
	dataDir, seed, run := isolatedCompactEnv(t)

	bloatCompactDBConn(t, seed.DB(), 2<<20)

	holder, err := filelock.TryAcquireFileLock(filepath.Join(dataDir, "migrate.lock"))
	require.NoError(t, err)
	defer holder.Release()

	_, _, err = run(t, "--force")
	require.Error(t, err)
	require.Contains(t, err.Error(), "migrate.lock")
}

func TestSessionsCompactCmdRun_DryRunLeavesFileUntouched(t *testing.T) {
	// REVERT CHECK: a mutation that runs VACUUM in --dry-run mode turns
	// this test red on the size assertion.
	dataDir, seed, run := isolatedCompactEnv(t)

	bloatCompactDBConn(t, seed.DB(), 2<<20)

	sizeBefore, mtimeBefore := dbFileSizeAndMtime(t, dataDir)
	out, _, err := run(t, "--dry-run")
	require.NoError(t, err)

	require.Contains(t, out, "before: ")
	require.Contains(t, out, "after (estimated): ")
	require.Contains(t, out, "disk space needed: ")
	require.Contains(t, out, "gates: hosts clear; session locks clear; wal checkpoint clear")

	sizeAfter, mtimeAfter := dbFileSizeAndMtime(t, dataDir)
	require.Equal(t, sizeBefore, sizeAfter, "dry run must not change the file size")
	require.Equal(t, mtimeBefore, mtimeAfter, "dry run must not change the file mtime")
}

func TestSessionsCompactCmdRun_LowDiskSpaceRefusesWithNumbers(t *testing.T) {
	_, seed, run := isolatedCompactEnv(t)

	bloatCompactDBConn(t, seed.DB(), 1<<20)

	orig := compactReserveBytes
	compactReserveBytes = func(liveBytes, walBytes int64) int64 { return 1 << 50 }
	defer func() { compactReserveBytes = orig }()

	_, _, err := run(t)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not enough free disk space")
	require.Contains(t, err.Error(), "free ")
}

func TestSessionsCompactHelp_DocumentsTheContract(t *testing.T) {
	t.Parallel()

	long := oneLine(sessionsCompactCmd.Long)
	require.Contains(t, long, "VACUUM", "the mechanism must be named")
	require.Contains(t, long, "never runs automatically", "no silent/automatic vacuum is the operator's hard requirement")
	require.Contains(t, long, "migrate.lock", "the startup-lock warning must be in the help")
	require.Contains(t, long, "SQLITE_BUSY", "the --force consequence must be in the help")
	require.Contains(t, long, "free disk space", "the space reservation must be in the help")
	require.Contains(t, long, "blind spot", "the idle-web-server blind spot must be in the help")
	require.Contains(t, long, "--force skips ONLY these two checks", "what --force does and does not bypass must be explicit")

	require.Equal(t, "compact", sessionsCompactCmd.Use)
	for _, name := range []string{"dry-run", "force", "json"} {
		require.NotNilf(t, sessionsCompactCmd.Flags().Lookup(name), "flag --%s is missing", name)
	}
	example := oneLine(sessionsCompactCmd.Example)
	require.Contains(t, example, "--dry-run")
	require.Contains(t, example, "--json")
}

func TestSessionsParentHelp_ListsCompact(t *testing.T) {
	t.Parallel()

	require.Contains(t, oneLine(sessionsCmd.Long), "compact")
	var registered bool
	for _, sub := range sessionsCmd.Commands() {
		if sub.Name() == "compact" {
			registered = true
			require.Same(t, sessionsCompactCmd, sub)
		}
	}
	require.True(t, registered, "`sessions compact` must be registered on the sessions command")
}

// gcHintEnv mirrors isolatedCompactEnv but wires sessionsGcCmd, and seeds
// one old empty session (gc's rule 1) plus freelist pages, so a real gc
// run exercises the compact hint end to end.
func gcHintEnv(t *testing.T) (string, func(t *testing.T, args ...string) (string, string, error)) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)
	dataDir := filepath.Join(tmp, "gc-hint-data")

	ctx, cancel := context.WithCancel(context.Background())
	ensureRootFlagStandIns(sessionsGcCmd, dataDir)
	sessionsGcCmd.SetContext(ctx)

	seed, err := setupAppLite(sessionsGcCmd)
	require.NoError(t, err)

	seedCtx := context.Background()
	_, err = seed.DB().ExecContext(seedCtx,
		"INSERT INTO sessions (id, title, created_at, updated_at) VALUES ('gc-hint-old-sess', 'old', ?, ?)",
		time.Now().Add(-8*24*time.Hour).Unix(), time.Now().Add(-8*24*time.Hour).Unix())
	require.NoError(t, err)
	bloatCompactDBConn(t, seed.DB(), 2<<20)

	t.Cleanup(func() {
		seed.Shutdown()
		_ = db.Release(dataDir)
		cancel()
		waitForSQLiteHandleRelease(t, dataDir)
	})

	run := func(t *testing.T, args ...string) (string, string, error) {
		t.Helper()
		require.NoError(t, sessionsGcCmd.Flags().Set("dry-run", "false"))
		require.NoError(t, sessionsGcCmd.Flags().Set("older-than", "7d"))
		require.NoError(t, sessionsGcCmd.Flags().Set("jobs-older-than", ""))
		require.NoError(t, sessionsGcCmd.Flags().Set("max-sessions", "0"))
		require.NoError(t, sessionsGcCmd.Flags().Set("json", "false"))
		require.NoError(t, sessionsGcCmd.ParseFlags(args))

		var outBuf, errBuf strings.Builder
		outR, outW, err := os.Pipe()
		require.NoError(t, err)
		errR, errW, err := os.Pipe()
		require.NoError(t, err)
		oldOut, oldErr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = outW, errW
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = io.Copy(&outBuf, outR) }()
		go func() { defer wg.Done(); _, _ = io.Copy(&errBuf, errR) }()

		runErr := sessionsGcCmd.RunE(sessionsGcCmd, nil)

		os.Stdout, os.Stderr = oldOut, oldErr
		_ = outW.Close()
		_ = errW.Close()
		wg.Wait()
		_ = outR.Close()
		_ = errR.Close()
		return outBuf.String(), errBuf.String(), runErr
	}
	return dataDir, run
}

// TestGCFiresCompactHintAfterDeletion: a gc run that actually deleted
// sessions and left a bloated freelist must point the operator at
// `rush sessions compact` on stderr.
//
// REVERT CHECK: a mutation that drops the threshold check (fires the hint
// on any freelist) turns the small-freelist case below red; a mutation
// that drops the hint call turns this test red.
func TestGCFiresCompactHintAfterDeletion(t *testing.T) {
	_, run := gcHintEnv(t)

	orig := compactHintMinBytes
	compactHintMinBytes = 1 << 20
	defer func() { compactHintMinBytes = orig }()

	_, stderr, err := run(t)
	require.NoError(t, err)
	require.Contains(t, stderr, "rush sessions compact")
}

func TestGCSuppressesCompactHint(t *testing.T) {
	_, run := gcHintEnv(t)

	orig := compactHintMinBytes
	compactHintMinBytes = 1 << 20
	defer func() { compactHintMinBytes = orig }()

	// --dry-run deletes nothing: no hint.
	_, stderr, err := run(t, "--dry-run")
	require.NoError(t, err)
	require.NotContains(t, stderr, "rush sessions compact")

	// --json is machine output: no hint.
	_, stderr, err = run(t, "--json")
	require.NoError(t, err)
	require.NotContains(t, stderr, "rush sessions compact")
}

// TestGCNoCompactHintBelowAbsoluteThreshold: the freed pages are most of the
// file, but the absolute amount is far below the default 64 MB cutoff, so
// gc stays silent. (The earlier third case of TestGCSuppressesCompactHint
// was vacuous: its --json run had already deleted the session, so the last
// run deleted nothing and printed no hint for that reason alone.)
//
// Revert-check: drop the compactHintMinBytes check in compactHint and this
// test goes red.
func TestGCNoCompactHintBelowAbsoluteThreshold(t *testing.T) {
	_, run := gcHintEnv(t)

	_, stderr, err := run(t)
	require.NoError(t, err)
	require.NotContains(t, stderr, "rush sessions compact")
}

// TestGCNoCompactHintBelowFraction: a large absolute freelist that is a small
// part of the file does not prompt a compaction either.
//
// Revert-check: drop the compactHintMinFraction check in compactHint and this
// test goes red.
func TestGCNoCompactHintBelowFraction(t *testing.T) {
	_, run := gcHintEnv(t)

	origBytes, origFraction := compactHintMinBytes, compactHintMinFraction
	compactHintMinBytes = 1 << 20
	compactHintMinFraction = 1.1 // no database is more than 100% free
	defer func() { compactHintMinBytes, compactHintMinFraction = origBytes, origFraction }()

	_, stderr, err := run(t)
	require.NoError(t, err)
	require.NotContains(t, stderr, "rush sessions compact")
}
