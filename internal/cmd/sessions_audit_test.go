package cmd

// Fixture and read-only guarantees of `rush sessions audit`.
//
// The read-only contract is the whole point of the command, so it is proved
// the way an operator would notice the difference: nothing in the data
// directory changes, byte for byte and name for name, not even while a
// WAL is pending.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// auditSchemaFixture is a minimal Rush-shaped schema: the columns audit
// requires, plus the optional ones it detects. Deliberately rollback-journal
// (no WAL), so a read leaves no sidecar behind at all.
const auditSchemaFixture = `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	title TEXT,
	created_at INTEGER,
	updated_at INTEGER,
	message_count INTEGER
);
CREATE TABLE messages (
	id TEXT PRIMARY KEY,
	session_id TEXT,
	role TEXT,
	parts TEXT,
	created_at INTEGER,
	updated_at INTEGER,
	notice_kind TEXT DEFAULT ''
);
CREATE TABLE async_jobs (
	owner_session_id TEXT,
	tool_call_id TEXT,
	kind TEXT,
	state TEXT,
	child_session_id TEXT,
	created_at INTEGER,
	updated_at INTEGER,
	PRIMARY KEY (owner_session_id, tool_call_id)
);
`

// auditBatchProjectionDDL carries the batch projection tables the audit's
// flag-ON paths touch: batch_nodes verbatim from the production migration
// (internal/db/migrations/20261012000001_batch_projection.sql, the CREATE
// TABLE and its five indexes) and session_notices verbatim from
// internal/db/migrations/20260928000002_add_async_phase4_core.sql. The DDL
// is GATED on config.BatchRuntimeEnabled(): with RUSH_BATCH_RUNTIME=1 the
// audit refuses a rush.db without batch_nodes (sessions_audit_db.go) and
// reads batch facts through reduceBatchSessionFacts, whose bound-notice and
// debt queries also read session_notices (sessions_batch_read.go), so the
// flag-ON fixtures carry both tables. Flag OFF no audit query touches either
// table — auditInspectSchema only runs PRAGMA table_info on batch_nodes and
// auditFindings reads messages/async_jobs — so the flag-OFF fixtures are
// byte-identical to HEAD in schema AND output.
const auditBatchProjectionDDL = `
CREATE TABLE batch_nodes (
    node_id TEXT PRIMARY KEY,
    root_id TEXT NOT NULL,
    owner_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    host_id TEXT NOT NULL,
    parent_id TEXT,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    kind TEXT NOT NULL CHECK (kind IN ('task', 'batch')),
    tool_call_id TEXT,
    input_hash TEXT,
    announced INTEGER NOT NULL DEFAULT 0 CHECK (announced IN (0, 1)),
    announce_message_id TEXT,
    origin_cli INTEGER NOT NULL DEFAULT 0 CHECK (origin_cli IN (0, 1)),
    mode TEXT CHECK (mode IN ('parallel', 'sequential')),
    on_fail TEXT CHECK (on_fail IN ('', 'stop', 'continue')),
    notify TEXT CHECK (notify IN ('', 'all', 'each')),
    max_parallel INTEGER NOT NULL DEFAULT 0 CHECK (max_parallel >= 0),
    task_kind TEXT,
    task_group TEXT NOT NULL DEFAULT '',
    task_payload TEXT,
    timeout_ns INTEGER NOT NULL DEFAULT 0 CHECK (timeout_ns >= 0),
    child_session_id TEXT,
    state TEXT NOT NULL CHECK (state IN (
        'pending', 'starting', 'running', 'cancelling', 'completed',
        'failed', 'cancelled', 'timed_out', 'interrupted', 'skipped'
    )),
    cause TEXT NOT NULL DEFAULT '' CHECK (cause IN ('', 'stop', 'timeout', 'host_closing', 'stop_on_fail')),
    token INTEGER NOT NULL DEFAULT 0 CHECK (token >= 0),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    control_state TEXT NOT NULL DEFAULT 'none' CHECK (control_state IN ('none', 'pending', 'acked', 'failed')),
    control_error TEXT NOT NULL DEFAULT '',
    control_pending INTEGER NOT NULL DEFAULT 0 CHECK (control_pending IN (0, 1)),
    outcome_state TEXT NOT NULL DEFAULT '' CHECK (outcome_state IN ('', 'completed', 'failed', 'cancelled', 'timed_out', 'interrupted')),
    outcome_code TEXT NOT NULL DEFAULT '',
    outcome_summary TEXT NOT NULL DEFAULT '',
    response_envelope_ref TEXT,
    root_sequence INTEGER NOT NULL DEFAULT 0 CHECK (root_sequence >= 0),
    projection_sequence INTEGER NOT NULL DEFAULT 0 CHECK (projection_sequence >= 0),
    notice_selected INTEGER NOT NULL DEFAULT 0 CHECK (notice_selected IN (0, 1)),
    notice_kind TEXT CHECK (notice_kind IN ('task', 'batch', 'root')),
    notice_state TEXT CHECK (notice_state IN ('completed', 'failed', 'cancelled', 'timed_out', 'interrupted', 'skipped')),
    notice_cause TEXT CHECK (notice_cause IN ('', 'stop', 'timeout', 'host_closing', 'stop_on_fail')),
    notice_sequence INTEGER,
    notice_ordinal INTEGER,
    notice_pending INTEGER NOT NULL DEFAULT 0 CHECK (notice_pending >= 0),
    notice_active INTEGER NOT NULL DEFAULT 0 CHECK (notice_active >= 0),
    notice_completed INTEGER NOT NULL DEFAULT 0 CHECK (notice_completed >= 0),
    notice_failed INTEGER NOT NULL DEFAULT 0 CHECK (notice_failed >= 0),
    notice_cancelled INTEGER NOT NULL DEFAULT 0 CHECK (notice_cancelled >= 0),
    notice_timed_out INTEGER NOT NULL DEFAULT 0 CHECK (notice_timed_out >= 0),
    notice_interrupted INTEGER NOT NULL DEFAULT 0 CHECK (notice_interrupted >= 0),
    notice_skipped INTEGER NOT NULL DEFAULT 0 CHECK (notice_skipped >= 0),
    delivery TEXT NOT NULL DEFAULT 'none' CHECK (delivery IN ('none', 'pending', 'done', 'void')),
    notice_message_id TEXT,
    wake INTEGER NOT NULL DEFAULT 0 CHECK (wake IN (0, 1)),
    reacted INTEGER NOT NULL DEFAULT 0 CHECK (reacted IN (0, 1)),
    wake_attempts INTEGER NOT NULL DEFAULT 0 CHECK (wake_attempts >= 0),
    reacted_failed INTEGER NOT NULL DEFAULT 0 CHECK (reacted_failed IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (root_id, node_id, owner_session_id, host_id),
    UNIQUE (root_id, parent_id, ordinal),
    FOREIGN KEY (root_id, root_id, owner_session_id, host_id)
        REFERENCES batch_nodes (root_id, node_id, owner_session_id, host_id) ON DELETE CASCADE,
    FOREIGN KEY (root_id, parent_id, owner_session_id, host_id)
        REFERENCES batch_nodes (root_id, node_id, owner_session_id, host_id) ON DELETE CASCADE,
    CHECK ((node_id = root_id AND parent_id IS NULL AND kind = 'batch' AND ordinal = 0
            AND tool_call_id IS NOT NULL AND input_hash IS NOT NULL)
        OR (node_id != root_id AND parent_id IS NOT NULL AND tool_call_id IS NULL
            AND input_hash IS NULL AND announced = 0 AND announce_message_id IS NULL
            AND root_sequence = 0)),
    CHECK ((kind = 'batch' AND mode IS NOT NULL AND task_kind IS NULL)
        OR (kind = 'task' AND task_kind IS NOT NULL AND mode IS NULL AND on_fail IS NULL AND notify IS NULL)),
    CHECK (announced = 0 OR announce_message_id IS NOT NULL),
    CHECK ((notice_selected = 0 AND notice_kind IS NULL AND notice_state IS NULL
            AND notice_cause IS NULL AND notice_sequence IS NULL AND notice_ordinal IS NULL
            AND delivery = 'none' AND wake = 0)
        OR (notice_selected = 1 AND notice_kind IS NOT NULL AND notice_state IS NOT NULL
            AND notice_cause IS NOT NULL AND notice_sequence IS NOT NULL AND notice_sequence > 0
            AND notice_ordinal IS NOT NULL AND notice_ordinal >= 0 AND delivery != 'none'))
);
CREATE UNIQUE INDEX idx_batch_nodes_live_key ON batch_nodes (owner_session_id, tool_call_id)
    WHERE node_id = root_id AND state IN ('pending', 'starting', 'running', 'cancelling');
CREATE INDEX idx_batch_nodes_host_live ON batch_nodes (host_id, root_id)
    WHERE node_id = root_id AND state IN ('pending', 'starting', 'running', 'cancelling');
CREATE INDEX idx_batch_nodes_owner_roots ON batch_nodes (owner_session_id, created_at, root_id)
    WHERE node_id = root_id;
CREATE INDEX idx_batch_nodes_parent ON batch_nodes (root_id, parent_id, ordinal);
CREATE INDEX idx_batch_nodes_pending_notice ON batch_nodes (owner_session_id, root_id, notice_sequence, notice_ordinal)
    WHERE notice_selected = 1 AND delivery = 'pending';
CREATE INDEX idx_batch_nodes_debt ON batch_nodes (owner_session_id, root_id)
    WHERE notice_selected = 1 AND wake = 1 AND reacted = 0 AND delivery != 'void';

CREATE TABLE session_notices (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    owner             TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    kind              TEXT NOT NULL,
    text              TEXT NOT NULL,
    wake              INTEGER NOT NULL DEFAULT 0,
    delivery          TEXT NOT NULL DEFAULT 'pending' CHECK (delivery IN ('none', 'pending', 'done', 'void')),
    notice_message_id TEXT,
    reacted           INTEGER NOT NULL DEFAULT 0,
    wake_attempts     INTEGER NOT NULL DEFAULT 0,
    reacted_failed    INTEGER NOT NULL DEFAULT 0,
    job_tool_call_id  TEXT,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);
`

// auditDDLForTest appends the batch projection DDL only when the batch
// runtime flag is on: flag-OFF fixtures stay schema-identical to the legacy
// ones, flag-ON fixtures carry the tables the schema guard and the batch
// scan need.
func auditDDLForTest() string {
	schema := auditSchemaFixture
	if config.BatchRuntimeEnabled() {
		schema += auditBatchProjectionDDL
	}
	return schema
}

// seedAuditFixtureDB writes a Rush-shaped rush.db into dataDir and returns
// its path. dataDir must be a t.TempDir(): the audit command is pointed at
// it, and the whole test asserts nothing else touches it.
func seedAuditFixtureDB(t *testing.T, dataDir string) string {
	t.Helper()

	dbPath := filepath.Join(dataDir, "rush.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	// Close exactly once: a leaked handle would keep Windows from cleaning
	// the temp dir up, which later tests would see as a spurious failure.
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = db.Close()
		}
	})

	_, err = db.ExecContext(context.Background(), auditDDLForTest())
	require.NoError(t, err)

	now := timeNowUnix()
	_, err = db.ExecContext(context.Background(), `INSERT INTO sessions (id, title, created_at, updated_at, message_count) VALUES
		('audit-alpha-1', 'alpha session', ?, ?, 4),
		('audit-beta-2', 'beta session', ?, ?, 1)`,
		now, now, now, now)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `INSERT INTO messages (id, session_id, role, parts, created_at, updated_at, notice_kind) VALUES
		('m1', 'audit-alpha-1', 'user', '[]', ?, ?, ''),
		('m2', 'audit-alpha-1', 'assistant', '[]', ?, ?, '')`,
		now, now, now, now)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `INSERT INTO async_jobs
		(owner_session_id, tool_call_id, kind, state, child_session_id, created_at, updated_at)
		VALUES ('audit-alpha-1', 'call-1', 'command', 'running', NULL, ?, ?)`, now, now)
	require.NoError(t, err)

	// A clean close is what leaves the database with no journal file at all.
	require.NoError(t, db.Close())
	closed = true
	return dbPath
}

// runAuditCmd executes the real `rush sessions audit` through the root
// command, exactly as the CLI does, with both output streams captured.
func runAuditCmd(t *testing.T, args ...string) (stdout, stderr string, runErr error) {
	t.Helper()

	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	resetAuditCmdFlags(t)
	rootCmd.SetArgs(append([]string{"sessions", "audit"}, args...))
	// rootCmd is a package-level var shared with every other test in the
	// package; restore it so no later test sees this one's arguments.
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		resetAuditCmdFlags(t)
	})

	runErr = rootCmd.Execute()
	return out.String(), errOut.String(), runErr
}

// resetAuditCmdFlags zeroes every flag the audit reads. Cobra keeps the
// previous value of any flag absent from an invocation's argument list, and
// these commands are package-level vars shared by the whole test binary —
// without this, a `--all` left set by one test silently applies to the next.
func resetAuditCmdFlags(t *testing.T) {
	t.Helper()

	for name, zero := range map[string]string{
		"data-dir":  "",
		"all":       "false",
		"json":      "false",
		"since":     "",
		"worktrees": "false",
	} {
		_ = rootCmd.PersistentFlags().Set(name, zero)
		_ = sessionsAuditCmd.Flags().Set(name, zero)
	}
}

// auditFileSHA256 is the before/after fingerprint of one file.
func auditFileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// auditDirNames lists a directory's entry names, sorted, so two listings can
// be compared for "nothing appeared, nothing vanished".
func auditDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// TestSessionsAudit_IsStrictlyReadOnly is the command's core guarantee:
// running it against a data directory changes NOTHING in it — not a byte of
// rush.db, not the file list (no -wal/-shm/journal/migrate.lock appears).
//
// Revert-check: with the fixture above the audit only ever reads, so the
// assertions are also what makes a regression visible — the run is repeated
// once to catch a write that a first pass merely left open, and the output
// must still contain the seeded session (otherwise an empty run would pass
// vacuously).
func TestSessionsAudit_IsStrictlyReadOnly(t *testing.T) {
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	dbPath := seedAuditFixtureDB(t, dataDir)

	require.Equal(t, []string{"rush.db"}, auditDirNames(t, dataDir),
		"precondition: the fixture must leave a clean, sidecar-free database")
	beforeHash := auditFileSHA256(t, dbPath)

	for pass := 1; pass <= 2; pass++ {
		stdout, stderr, err := runAuditCmd(t, "--data-dir", dataDir, "--all")
		require.NoError(t, err, "audit pass %d stderr: %s", pass, stderr)

		require.Equal(t, beforeHash, auditFileSHA256(t, dbPath),
			"audit pass %d modified rush.db", pass)
		require.Equal(t, []string{"rush.db"}, auditDirNames(t, dataDir),
			"audit pass %d created or removed a file in the data directory", pass)
		require.Contains(t, stdout, "audit-alpha-1",
			"audit pass %d must actually report the seeded session", pass)
		require.Contains(t, stdout, "db health:",
			"audit pass %d must report DB health", pass)
	}
}

// TestSessionsAudit_ReadOnlyWithPendingWAL is the revert-resistant version
// of the read-only guarantee. A live Rush data directory is in WAL mode with
// a pending -wal, which is exactly the case that distinguishes mode=ro from
// a plain open: a read-only connection cannot checkpoint, so the sidecars
// stay exactly as they were, while a writable one checkpoints and removes
// them on close.
//
// Revert-check (performed): dropping mode=ro from openAuditRO's DSN — i.e.
// "file:<path>?_txlock=deferred" — made this test fail with the directory
// listing going from three entries to one:
//
//	audit created or removed a file in the data directory
//	    expected: [rush.db rush.db-shm rush.db-wal]
//	    actual  : [rush.db]
//
// which is precisely the damage the read-only contract forbids.
func TestSessionsAudit_ReadOnlyWithPendingWAL(t *testing.T) {
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	dbPath := seedAuditWALFixtureDB(t, dataDir)

	require.FileExists(t, dbPath+"-wal", "precondition: the fixture must leave a pending WAL")
	beforeFiles := auditDirNames(t, dataDir)
	require.Len(t, beforeFiles, 3, "precondition: rush.db with WAL sidecars")
	beforeHash := auditFileSHA256(t, dbPath)

	stdout, stderr, err := runAuditCmd(t, "--data-dir", dataDir, "--all")
	require.NoError(t, err, "audit stderr: %s", stderr)

	require.Equal(t, beforeHash, auditFileSHA256(t, dbPath),
		"audit must not checkpoint a pending WAL into rush.db")
	require.Equal(t, beforeFiles, auditDirNames(t, dataDir),
		"a read-only connection must not checkpoint and remove the WAL sidecars")
	require.Contains(t, stdout, "wal", "the health block must report the WAL size")
	require.Contains(t, stdout, "audit-alpha-1", "the WAL reader must still see the seeded session")
}

// seedAuditWALFixtureDB seeds the same schema but leaves the database the way
// a live Rush leaves it: WAL mode with a pending -wal/-shm pair. Making the
// sidecars undeletable for the duration of the writer's final close is what
// preserves them, because that close would otherwise checkpoint and remove
// them (which is also what a writable audit connection would do).
func seedAuditWALFixtureDB(t *testing.T, dataDir string) string {
	t.Helper()

	dbPath := filepath.Join(dataDir, "rush.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = db.Close()
		}
	})

	_, err = db.ExecContext(context.Background(), auditDDLForTest())
	require.NoError(t, err)
	// journal_mode must be set before the inserts for the data to land in
	// the -wal rather than being checkpointed straight into rush.db.
	_, err = db.ExecContext(context.Background(), `PRAGMA journal_mode=WAL;`)
	require.NoError(t, err)

	now := timeNowUnix()
	_, err = db.ExecContext(context.Background(), `INSERT INTO sessions (id, title, created_at, updated_at, message_count) VALUES
		('audit-alpha-1', 'alpha session', ?, ?, 4)`, now, now)
	require.NoError(t, err)

	releaseSidecars := holdAuditSidecars(t, dataDir)
	require.NoError(t, db.Close())
	closed = true
	releaseSidecars()

	if _, err := os.Stat(dbPath + "-wal"); err != nil {
		t.Skipf("could not preserve a pending WAL on %s (%v); the sidecar-blocking mechanism is platform-specific", runtime.GOOS, err)
	}
	return dbPath
}

// holdAuditSidecars makes it impossible to delete the WAL sidecars for as
// long as the returned release function is not called, so a closing writer
// has to leave them behind.
func holdAuditSidecars(t *testing.T, dataDir string) func() {
	t.Helper()

	var release []func()
	if runtime.GOOS == "windows" {
		// Windows refuses to delete a file that still has an open handle
		// without FILE_SHARE_DELETE, which os.OpenFile does not request.
		for _, name := range []string{"rush.db-wal", "rush.db-shm"} {
			file, err := os.OpenFile(filepath.Join(dataDir, name), os.O_RDONLY, 0)
			if err != nil {
				continue
			}
			held := file
			release = append(release, func() { _ = held.Close() })
		}
	} else {
		// POSIX: a directory without the write bit cannot lose entries.
		if err := os.Chmod(dataDir, 0o500); err == nil {
			release = append(release, func() { _ = os.Chmod(dataDir, 0o700) })
		}
	}
	return func() {
		for _, undo := range release {
			undo()
		}
	}
}

// TestOpenAuditRO_RefusesWrites pins the driver layer's half of the
// read-only contract: the handle the audit uses cannot write through, so a
// later phase that adds a rule cannot quietly start mutating a live
// database.
func TestOpenAuditRO_RefusesWrites(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	dbPath := seedAuditFixtureDB(t, dataDir)
	before := auditFileSHA256(t, dbPath)

	db, err := openAuditRO(dbPath)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.ExecContext(context.Background(),
		`INSERT INTO sessions (id, title, created_at, updated_at, message_count) VALUES ('nope', 'x', 1, 2, 0)`)
	require.Error(t, err, "the audit handle must refuse writes")
	require.Equal(t, before, auditFileSHA256(t, dbPath), "a refused write must not modify the file")
}

// TestSessionsAudit_UnrecognizedSchemaIsReportedNotFatal: a rush.db that is
// not a Rush session database (a foreign one, or one far too old) is
// reported and left completely alone — audit exits 0 and writes nothing.
func TestSessionsAudit_UnrecognizedSchemaIsReportedNotFatal(t *testing.T) {
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "rush.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(context.Background(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, note TEXT);
CREATE TABLE something_else (x INTEGER);`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	beforeHash := auditFileSHA256(t, dbPath)
	files := auditDirNames(t, dataDir)

	stdout, stderr, err := runAuditCmd(t, "--data-dir", dataDir, "--all")
	require.NoError(t, err, "a foreign schema is reported, not failed on: %s", stderr)

	require.Contains(t, stdout, "unrecognized schema")
	require.Contains(t, stdout, "missing sessions.title")
	require.Contains(t, stdout, "changed nothing")
	require.Equal(t, beforeHash, auditFileSHA256(t, dbPath))
	require.Equal(t, files, auditDirNames(t, dataDir))
}

// TestSessionsAudit_MissingDatabaseSaysSoAndCreatesNothing: a data
// directory without rush.db is a message, not an empty directory the command
// goes on to create.
func TestSessionsAudit_MissingDatabaseSaysSoAndCreatesNothing(t *testing.T) {
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()

	stdout, stderr, err := runAuditCmd(t, "--data-dir", dataDir, "--all")
	require.NoError(t, err, "stderr: %s", stderr)

	require.Contains(t, stdout, "no rush.db found in "+dataDir)
	require.Contains(t, stdout, "does not create anything")
	require.Empty(t, auditDirNames(t, dataDir), "audit must not create the data directory's contents")
}

// TestSessionsAudit_RequiresSelection: without <id> and without --all there
// is nothing to audit, and the usage error says exactly that.
func TestSessionsAudit_RequiresSelection(t *testing.T) {
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	seedAuditFixtureDB(t, dataDir)

	_, _, err := runAuditCmd(t, "--data-dir", dataDir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--all")
	require.Contains(t, err.Error(), "session id")
}

// TestSessionsAudit_PrefixSelection: an exact id is audited on its own, and
// an ambiguous prefix is refused with a list to choose from.
func TestSessionsAudit_PrefixSelection(t *testing.T) {
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	seedAuditFixtureDB(t, dataDir)

	stdout, stderr, err := runAuditCmd(t, "--data-dir", dataDir, "audit-alpha-1")
	require.NoError(t, err, "stderr: %s", stderr)
	require.Contains(t, stdout, "audit-alpha-1")
	require.NotContains(t, stdout, "audit-beta-2")

	stdout, _, err = runAuditCmd(t, "--data-dir", dataDir, "audit-")
	require.NoError(t, err)
	require.Contains(t, stdout, "matches 2 sessions")
	require.Contains(t, stdout, "audit-alpha-1")
	require.Contains(t, stdout, "audit-beta-2")
	require.NotContains(t, stdout, "sessions (", "an ambiguous prefix must not audit anything")
}

// timeNowUnix keeps the fixture's timestamps in Unix seconds, the unit the
// real schema stores (a millisecond fixture is how #1162 shipped with 1970
// dates and an empty --since window).
func timeNowUnix() int64 {
	return time.Now().Unix()
}

// TestSessionsAudit_InvalidSinceIsRefused: --since reuses the shared
// duration parser, so a bad value must fail before anything is opened.
func TestSessionsAudit_InvalidSinceIsRefused(t *testing.T) {
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	seedAuditFixtureDB(t, dataDir)

	_, _, err := runAuditCmd(t, "--data-dir", dataDir, "--all", "--since", "nonsense")
	require.Error(t, err)
	require.Contains(t, err.Error(), "--since")
}

// TestSessionsAudit_JSONOutput: --json emits one object per scanned
// database, carrying the same health and summary numbers the text output
// shows.
func TestSessionsAudit_JSONOutput(t *testing.T) {
	isolateConfigEnvForTests(t)

	dataDir := t.TempDir()
	seedAuditFixtureDB(t, dataDir)

	stdout, stderr, err := runAuditCmd(t, "--data-dir", dataDir, "--all", "--json")
	require.NoError(t, err, "stderr: %s", stderr)

	var reports []auditDBReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &reports))
	require.Len(t, reports, 1)
	require.Equal(t, dataDir, reports[0].DataDir)
	require.Equal(t, filepath.Join(dataDir, "rush.db"), reports[0].DBPath)
	require.NotNil(t, reports[0].Health, "the health block must be in the JSON output")
	require.Greater(t, reports[0].Health.PageSize, int64(0))
	require.Greater(t, reports[0].Health.PageCount, int64(0))
	require.GreaterOrEqual(t, reports[0].Health.FreePercent, float64(0))
	require.Len(t, reports[0].Sessions, 2)
	require.Equal(t, "audit-alpha-1", reports[0].Sessions[0].ID)
}
