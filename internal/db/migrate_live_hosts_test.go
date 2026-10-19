package db

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"testing/fstest"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the R12-A pre-migration live-host gate and the additive
// PreMigrationChecks hook against the synthetic (TEST DATA ONLY) destructive
// migration in testdata/r12a_drop_async_jobs_testonly. That migration is
// never embedded and never applied outside these tests; the real embedded
// set is untouched.

const (
	r12aSyntheticDir      = "testdata/r12a_drop_async_jobs_testonly" // fs paths use forward slashes on every OS
	r12aSyntheticFile     = "20991231000001_r12a_drop_async_jobs_testonly.sql"
	r12aSyntheticVersion  = int64(20991231000001)
	r12aHelperEnv         = "RUSH_DB_HOSTLOCK_HELPER"
	r12aHelperLockPathEnv = "RUSH_DB_HOSTLOCK_PATH"
)

// r12aEmbeddedFS returns a MapFS holding exactly the embedded migration set
// (same pattern as internal/session's batchStoreTestMigrationFS).
func r12aEmbeddedFS(t *testing.T) fstest.MapFS {
	t.Helper()
	embedded, err := migrationFS()
	require.NoError(t, err)
	entries, err := fs.ReadDir(embedded, ".")
	require.NoError(t, err)
	files := fstest.MapFS{}
	for _, entry := range entries {
		data, e := fs.ReadFile(embedded, entry.Name())
		require.NoError(t, e)
		files[entry.Name()] = &fstest.MapFile{Data: data}
	}
	return files
}

// r12aSyntheticFS returns the full embedded set PLUS the synthetic test-only
// migration, so a fully-migrated test DB sees pending={synthetic}, unknown=∅.
// The synthetic content is read from testdata so the FS cannot drift from the
// DDL fixture.
func r12aSyntheticFS(t *testing.T) fs.FS {
	t.Helper()
	files := r12aEmbeddedFS(t)
	data, err := os.ReadFile(filepath.Join(r12aSyntheticDir, r12aSyntheticFile))
	require.NoError(t, err)
	files[r12aSyntheticFile] = &fstest.MapFile{Data: data}
	return files
}

// r12aNewEmbeddedDB builds a temp SQLite database fully migrated through the
// embedded set.
func r12aNewEmbeddedDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "rush.db")
	conn := openTestDB(t, dbPath)
	require.NoError(t, Migrate(context.Background(), conn))
	return dbPath, conn
}

// r12aApplySynthetic applies the synthetic destructive migration via the real
// goose runner and reports how many versions it applied.
func r12aApplySynthetic(t *testing.T, conn *sql.DB) int {
	t.Helper()
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, r12aSyntheticFS(t),
		goose.WithLogger(goose.NopLogger()))
	require.NoError(t, err)
	applied, err := provider.Up(context.Background())
	require.NoError(t, err)
	return len(applied)
}

// r12aSeedRichDB populates conn with rows across every table family the
// cutover could plausibly touch: async_hosts (one with jobs, one idle),
// async_jobs, job-bound and unrelated session_notices, batch tables, control
// requests, messages, session_drivers, and wake_schedules.
func r12aSeedRichDB(t *testing.T, ctx context.Context, conn *sql.DB) {
	t.Helper()
	const ts = int64(1700000000)
	seed := []string{
		`INSERT INTO sessions (id, title, updated_at, created_at) VALUES ('sess-1', 'sess-1', 1700000000, 1700000000)`,
		`INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('host-a', 1, 'cli', 1700000000)`,
		`INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('host-b', 2, 'cli', 1700000000)`,
		`INSERT INTO async_jobs (owner_session_id, tool_call_id, kind, input_hash, host_id, state, created_at, updated_at)
			VALUES ('sess-1', 'call-1', 'command', 'h1', 'host-a', 'running', 1700000000, 1700000000)`, // state NOT NULL, 20261005000001
		`INSERT INTO async_jobs (owner_session_id, tool_call_id, kind, input_hash, host_id, state, created_at, updated_at)
			VALUES ('sess-1', 'call-2', 'agent', 'h2', 'host-a', 'running', 1700000000, 1700000000)`,
		`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at)
			VALUES ('msg-1', 'sess-1', 'user', '[]', 1700000000, 1700000000)`,
		// Job-bound notice: pairs with ('sess-1', 'call-1').
		`INSERT INTO session_notices (owner, kind, text, wake, delivery, job_tool_call_id, created_at, updated_at)
			VALUES ('sess-1', 'supervision', 'job-bound', 1, 'pending', 'call-1', 1700000000, 1700000000)`,
		// Second job-bound notice: pairs with ('sess-1', 'call-2').
		`INSERT INTO session_notices (owner, kind, text, wake, delivery, job_tool_call_id, created_at, updated_at)
			VALUES ('sess-1', 'supervision', 'job-bound-2', 1, 'pending', 'call-2', 1700000000, 1700000000)`,
		// Third job pair, for the reacted historical notice below.
		`INSERT INTO async_jobs (owner_session_id, tool_call_id, kind, input_hash, host_id, state, created_at, updated_at)
			VALUES ('sess-1', 'call-3', 'command', 'h3', 'host-a', 'completed', 1700000000, 1700000000)`,
		// Job-bound REACTED historical notice: already consumed by a turn
		// (reacted = 1, delivery 'done', wake already drained). The rehearsal
		// voids unreacted notices only, so this row must stay byte-identical.
		`INSERT INTO session_notices (owner, kind, text, wake, delivery, job_tool_call_id, reacted, created_at, updated_at)
			VALUES ('sess-1', 'supervision', 'job-bound-reacted', 0, 'done', 'call-3', 1, 1700000000, 1700000000)`,
		// Unrelated notice: no async_jobs row references it.
		`INSERT INTO session_notices (owner, kind, text, wake, delivery, job_tool_call_id, created_at, updated_at)
			VALUES ('sess-1', 'wake_failed', 'unrelated', 1, 'pending', NULL, 1700000000, 1700000000)`,
		// batch_nodes root node (the schema's CHECKs force this exact shape).
		`INSERT INTO batch_nodes (node_id, root_id, owner_session_id, host_id, parent_id, ordinal, kind,
			tool_call_id, input_hash, mode, state, created_at, updated_at)
			VALUES ('root-1', 'root-1', 'sess-1', 'host-a', NULL, 0, 'batch', 'call-r', 'hash-r',
			'parallel', 'pending', 1700000000, 1700000000)`, // state NOT NULL, no default, 20261012000001
		`INSERT INTO batch_control_requests (request_id, owner_session_id, root_id, node_id, host_id, created_at)
			VALUES ('req-1', 'sess-1', 'root-1', 'root-1', 'host-a', 1700000000)`,
		`INSERT INTO session_drivers (session_id, host_id, pid, claimed_at)
			VALUES ('sess-1', 'host-a', 1, 1700000000)`,
		`INSERT INTO wake_schedules (id, owner_session_id, kind, message, next_run_at, state, created_at, updated_at)
			VALUES ('wake-1', 'sess-1', 'once', 'hi', 1700000001, 'active', 1700000000, 1700000000)`,
	}
	for _, stmt := range seed {
		_, err := conn.ExecContext(ctx, stmt)
		require.NoError(t, err, "seed: %s", stmt)
	}
}

func r12aCount(t *testing.T, ctx context.Context, conn *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	err := conn.QueryRowContext(ctx, query, args...).Scan(&n)
	require.NoError(t, err)
	return n
}

// r12aNotice returns a notice's delivery, wake flag, reacted flag, and
// updated_at.
func r12aNotice(t *testing.T, ctx context.Context, conn *sql.DB, text string) (delivery string, wake int, reacted int, updatedAt int64) {
	t.Helper()
	err := conn.QueryRowContext(ctx,
		`SELECT delivery, wake, reacted, updated_at FROM session_notices WHERE text = ?`, text,
	).Scan(&delivery, &wake, &reacted, &updatedAt)
	require.NoError(t, err)
	return delivery, wake, reacted, updatedAt
}

// TestHelperHostLockHold is the re-exec child entry point (same idiom as
// internal/session's TestHelperLockHold): under the sentinel env var it
// acquires an EXCLUSIVE filelock on the exact lock path named by the second
// env var, prints LOCKED, and blocks on stdin until the parent closes it.
func TestHelperHostLockHold(t *testing.T) {
	if os.Getenv(r12aHelperEnv) != "1" {
		return
	}
	lk, err := filelock.TryAcquireFileLock(os.Getenv(r12aHelperLockPathEnv))
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		os.Exit(1)
	}
	fmt.Println("LOCKED")
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	_ = lk.Release()
	os.Exit(0)
}

// r12aLockHolder mirrors session's lockHolder: a real child OS process
// holding the host lock until killed or its stdin closes.
type r12aLockHolder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

func r12aSpawnHostLockHolder(t *testing.T, lockPath string) *r12aLockHolder {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := platform.Command(context.Background(), exe, "-test.run=^TestHelperHostLockHold$")
	cmd.Env = append(os.Environ(),
		r12aHelperEnv+"=1",
		r12aHelperLockPathEnv+"="+lockPath,
	)
	stdinW, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdoutR, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	lineCh := make(chan string, 1)
	sc := bufio.NewScanner(stdoutR)
	go func() {
		for sc.Scan() {
			if sc.Text() != "" {
				lineCh <- sc.Text()
				return
			}
		}
		lineCh <- ""
	}()
	select {
	case line := <-lineCh:
		require.Equal(t, "LOCKED", line, "helper child failed to acquire %s", lockPath)
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("timed out waiting for host-lock helper on %s", lockPath)
	}
	return &r12aLockHolder{cmd: cmd, stdin: stdinW}
}

func (h *r12aLockHolder) stop(t *testing.T) {
	t.Helper()
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	_ = h.stdin.Close()
	_ = h.cmd.Wait()
}

// Case 1: the synthetic upgrade applies with ZERO row conversion.
func TestR12ASyntheticUpgrade_NoConversion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, conn := r12aNewEmbeddedDB(t)
	r12aSeedRichDB(t, ctx, conn)

	require.Equal(t, 1, r12aApplySynthetic(t, conn))

	requireTableExists(t, ctx, conn, "async_jobs", false)
	requireTableExists(t, ctx, conn, "async_hosts", true)
	assert.Equal(t, 2, r12aCount(t, ctx, conn, `SELECT COUNT(*) FROM async_hosts`))
	assert.Equal(t, 1, r12aCount(t, ctx, conn, `SELECT COUNT(*) FROM batch_nodes`))
	assert.Equal(t, 1, r12aCount(t, ctx, conn, `SELECT COUNT(*) FROM batch_control_requests`))
	assert.Equal(t, 1, r12aCount(t, ctx, conn, `SELECT COUNT(*) FROM messages`))
	assert.Equal(t, 1, r12aCount(t, ctx, conn, `SELECT COUNT(*) FROM session_drivers`))
	assert.Equal(t, 1, r12aCount(t, ctx, conn, `SELECT COUNT(*) FROM wake_schedules`))
	assert.Equal(t, 4, r12aCount(t, ctx, conn, `SELECT COUNT(*) FROM session_notices`))

	delivery, wake, reacted, updated := r12aNotice(t, ctx, conn, "job-bound")
	assert.Equal(t, "void", delivery)
	assert.Equal(t, 0, wake)
	assert.Equal(t, 0, reacted)
	assert.Greater(t, updated, int64(1700000000), "voided notice's updated_at must be rewritten")
	delivery2, wake2, reacted2, updated2 := r12aNotice(t, ctx, conn, "job-bound-2")
	assert.Equal(t, "void", delivery2)
	assert.Equal(t, 0, wake2)
	assert.Equal(t, 0, reacted2)
	assert.Greater(t, updated2, int64(1700000000))
	// Reacted historical notice: byte-unchanged by the rehearsal.
	deliveryR, wakeR, reactedR, updatedR := r12aNotice(t, ctx, conn, "job-bound-reacted")
	assert.Equal(t, "done", deliveryR, "reacted notice's delivery must not be voided")
	assert.Equal(t, 0, wakeR, "reacted notice was seeded with wake=0")
	assert.Equal(t, 1, reactedR, "reacted notice's reacted flag must not be rewritten")
	assert.Equal(t, int64(1700000000), updatedR, "reacted notice's updated_at must not change")
	delivery, wake, _, unrelatedUpdated := r12aNotice(t, ctx, conn, "unrelated")
	assert.Equal(t, "pending", delivery)
	assert.Equal(t, 1, wake)
	assert.Equal(t, int64(1700000000), unrelatedUpdated, "untouched notice's updated_at must not change")

	assert.True(t, dbVersionSet(t, conn)[r12aSyntheticVersion])
}

// Case 2 + negative half of case 4: an alive host (real child OS process
// holding the exclusive lock, even with zero async_jobs rows) refuses the
// gate; schema and goose version table are unchanged.
func TestR12AAliveHostRefuses(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "rush.db")
	conn := openTestDB(t, dbPath)
	require.NoError(t, Migrate(ctx, conn))
	_, err := conn.ExecContext(ctx,
		`INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('host-live', 1, 'cli', 1700000000)`)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "hosts"), 0o755))
	h := r12aSpawnHostLockHolder(t, hostLockPath(dataDir, "host-live"))
	defer h.stop(t)

	before := dbVersionSet(t, conn)
	err = CheckLiveHosts(ctx, conn, dataDir, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host-live")
	// Gate refused BEFORE any migration: schema and versions untouched.
	requireTableExists(t, ctx, conn, "async_jobs", true)
	assert.Equal(t, before, dbVersionSet(t, conn))
}

// Case 11: CheckLiveHosts called directly on an alive host names the host and
// instructs the operator to stop other Rush processes.
func TestR12ACheckLiveHosts_InstructsOperator(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	conn := openTestDB(t, filepath.Join(dataDir, "rush.db"))
	require.NoError(t, Migrate(ctx, conn))
	_, err := conn.ExecContext(ctx,
		`INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('host-direct', 1, 'cli', 1700000000)`)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "hosts"), 0o755))

	h := r12aSpawnHostLockHolder(t, hostLockPath(dataDir, "host-direct"))
	defer h.stop(t)

	err = CheckLiveHosts(ctx, conn, dataDir, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host-direct")
	assert.Contains(t, err.Error(), "stop other Rush processes")
}

// Case 3: a DIRECTORY at the lock path makes the probe return Unknown, which
// refuses; versions unchanged.
func TestR12AUnknownHostRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn := openTestDB(t, filepath.Join(dataDir, "rush.db"))
	require.NoError(t, Migrate(ctx, conn))
	_, err := conn.ExecContext(ctx,
		`INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('host-unk', 1, 'cli', 1700000000)`)
	require.NoError(t, err)
	// A directory where the lock file should be: deterministic on every OS.
	require.NoError(t, os.MkdirAll(hostLockPath(dataDir, "host-unk"), 0o755))

	before := dbVersionSet(t, conn)
	err = CheckLiveHosts(ctx, conn, dataDir, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host-unk")
	assert.Equal(t, before, dbVersionSet(t, conn))
}

// Case 4, proceed half: a zero-job host whose lock file is missing is still
// probed (enumerate-all) and reads Dead, so the gate proceeds.
func TestR12AZeroJobHostDeadProceeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn := openTestDB(t, filepath.Join(dataDir, "rush.db"))
	require.NoError(t, Migrate(ctx, conn))
	_, err := conn.ExecContext(ctx,
		`INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('host-idle', 2, 'cli', 1700000000)`)
	require.NoError(t, err)
	assert.NoError(t, CheckLiveHosts(ctx, conn, dataDir, nil))
}

// Case 4, refuse half: the SAME zero-job host refuses when a live child holds
// its lock -- jobs are irrelevant to the gate.
func TestR12AZeroJobHostAliveRefuses(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	conn := openTestDB(t, filepath.Join(dataDir, "rush.db"))
	require.NoError(t, Migrate(ctx, conn))
	_, err := conn.ExecContext(ctx,
		`INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('host-idle-live', 2, 'cli', 1700000000)`)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "hosts"), 0o755))

	h := r12aSpawnHostLockHolder(t, hostLockPath(dataDir, "host-idle-live"))
	defer h.stop(t)

	err = CheckLiveHosts(ctx, conn, dataDir, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host-idle-live")
}

// Case 5: isOwnHost refuses the named host even with no lock file; nil
// isOwnHost over the same (dead) set proceeds.
func TestR12AOwnHostRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn := openTestDB(t, filepath.Join(dataDir, "rush.db"))
	require.NoError(t, Migrate(ctx, conn))
	_, err := conn.ExecContext(ctx,
		`INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('host-own', 3, 'cli', 1700000000)`)
	require.NoError(t, err)

	err = CheckLiveHosts(ctx, conn, dataDir, func(hostID string) bool { return hostID == "host-own" })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host-own")
	assert.NoError(t, CheckLiveHosts(ctx, conn, dataDir, nil))
}

// Case 6: an error during async_hosts ENUMERATION refuses the gate. Closing
// the connection makes conn.QueryContext fail deterministically on every OS,
// exercising the "failed to enumerate async_hosts" branch.
func TestR12AEnumerateErrorRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, conn := r12aNewEmbeddedDB(t)
	_, err := conn.ExecContext(ctx,
		`INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('host-broken', 4, 'cli', 1700000000)`)
	require.NoError(t, err)
	// openTestDB registers Cleanup(conn.Close); sql.DB.Close is idempotent.
	require.NoError(t, conn.Close())

	err = CheckLiveHosts(ctx, conn, t.TempDir(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to enumerate async_hosts")
}

// Case 6b (unix-only): <dataDir>/hosts exists as a FILE, so the probe open
// fails with ENOTDIR (Unknown) and the gate refuses. On Windows opening a
// path under a file component fails with ERROR_PATH_NOT_FOUND/
// ERROR_DIRECTORY, which the shared probe classifies as not-exist, i.e. Dead
// per the legacy session contract. That is the intended, legacy-identical
// outcome there (the gate correctly proceeding is fine), so the ENOTDIR-
// Unknown refusal is only asserted on unix.
func TestR12AHostsAsFileProbeErrorRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows classifies a missing path under a file component as not-exist, i.e. Dead by the legacy probe contract; the Unix ENOTDIR-Unknown behaviour is covered here")
	}
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn := openTestDB(t, filepath.Join(dataDir, "rush.db"))
	require.NoError(t, Migrate(ctx, conn))
	_, err := conn.ExecContext(ctx,
		`INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('host-broken', 4, 'cli', 1700000000)`)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "hosts"), []byte("x"), 0o644))

	err = CheckLiveHosts(ctx, conn, dataDir, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host-broken")
}

// Case 7: a fresh (embedded-migrated, zero-host) DB lets the gate proceed and
// takes the synthetic Up cleanly.
func TestR12AFreshDBGateProceedsAndUpplies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, conn := r12aNewEmbeddedDB(t)
	assert.NoError(t, CheckLiveHosts(ctx, conn, t.TempDir(), nil))
	require.Equal(t, 1, r12aApplySynthetic(t, conn))
	requireTableExists(t, ctx, conn, "async_jobs", false)
}

// Case 8: running the synthetic migration twice is a no-op the second time.
func TestR12ASyntheticIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, conn := r12aNewEmbeddedDB(t)
	require.Equal(t, 1, r12aApplySynthetic(t, conn))
	require.Equal(t, 0, r12aApplySynthetic(t, conn), "second pass must apply nothing")
	requireTableExists(t, ctx, conn, "async_jobs", false)
}

// Case 9: pending synthetic migration with MayMigrate=false yields
// SchemaMigrationNotAllowedError and touches nothing.
func TestR12AMayMigrateFalseRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath, conn := r12aNewEmbeddedDB(t)

	err := migrate(ctx, conn, dbPath, r12aSyntheticFS(t),
		newMigrateOptions([]MigrateOption{WithMayMigrate(false)}, dbPath))
	require.Error(t, err)
	var notAllowed *SchemaMigrationNotAllowedError
	require.True(t, errors.As(err, &notAllowed))
	assert.Equal(t, []int64{r12aSyntheticVersion}, notAllowed.Pending)
	requireTableExists(t, ctx, conn, "async_jobs", true)
	assert.False(t, dbVersionSet(t, conn)[r12aSyntheticVersion])
}

// Case 10: an unknown-only version table opens without migrating (warn-only,
// nothing applied). This is NOT an old-binary fence: an older binary treats
// this binary's versions the same way; operational quiescence, not this
// policy, is what makes a rolling upgrade safe.
func TestR12AUnknownOnlyPolicyUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, conn := r12aNewEmbeddedDB(t)
	_, err := conn.ExecContext(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (20981231000001, 1, CURRENT_TIMESTAMP)`)
	require.NoError(t, err)
	before := dbVersionSet(t, conn)

	embeddedFS := r12aEmbeddedFS(t)
	require.NoError(t, migrate(ctx, conn, "unused.db", embeddedFS,
		newMigrateOptions(nil, "unused.db")))
	assert.Equal(t, before, dbVersionSet(t, conn), "nothing may be applied on unknown-only")
}

// Case 12: the gate registered as a PreMigrationCheck runs while the decision
// lock would be held: benign (all-dead) hosts let the cutover apply; an alive
// host makes the hook fire BEFORE the Up, so async_jobs survives.
func TestR12AGateHookIntegration(t *testing.T) {
	ctx := context.Background()

	newHookedDB := func(t *testing.T) (string, *sql.DB) {
		t.Helper()
		dataDir := t.TempDir()
		dbPath := filepath.Join(dataDir, "rush.db")
		conn := openTestDB(t, dbPath)
		require.NoError(t, Migrate(ctx, conn))
		r12aSeedRichDB(t, ctx, conn)
		require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "hosts"), 0o755))
		return dataDir, conn
	}

	t.Run("benign applies", func(t *testing.T) {
		dataDir, conn := newHookedDB(t)
		err := migrate(ctx, conn, dataDir, r12aSyntheticFS(t),
			newMigrateOptions([]MigrateOption{
				WithPreMigrationChecks(func(ctx context.Context, conn *sql.DB, dbPath string) error {
					return CheckLiveHosts(ctx, conn, dataDir, nil)
				}),
			}, dataDir))
		require.NoError(t, err)
		requireTableExists(t, ctx, conn, "async_jobs", false)
	})

	t.Run("alive host blocks before Up", func(t *testing.T) {
		dataDir, conn := newHookedDB(t)
		h := r12aSpawnHostLockHolder(t, hostLockPath(dataDir, "host-a"))
		defer h.stop(t)

		before := dbVersionSet(t, conn)
		err := migrate(ctx, conn, dataDir, r12aSyntheticFS(t),
			newMigrateOptions([]MigrateOption{
				WithPreMigrationChecks(func(ctx context.Context, conn *sql.DB, dbPath string) error {
					return CheckLiveHosts(ctx, conn, dataDir, nil)
				}),
			}, dataDir))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "host-a")
		// The hook fired BEFORE the Up: async_jobs still present.
		requireTableExists(t, ctx, conn, "async_jobs", true)
		assert.Equal(t, before, dbVersionSet(t, conn))
	})
}
