package db

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/stretchr/testify/require"
)

// orderedByKeyColumns returns a deterministic ORDER BY list over the table's
// declared primary-key columns (in PK ordinal order), falling back to rowid
// for tables without one. The importer copies in rowid order, so comparing in
// primary-key order verifies the same set of rows independently of physical
// rowid renumbering on the destination side.
func orderedByKeyColumns(cols []columnInfo) string {
	type pkCol struct {
		ordinal int
		name    string
	}
	var keys []pkCol
	for _, col := range cols {
		if col.PrimaryKey > 0 {
			keys = append(keys, pkCol{col.PrimaryKey, col.Name})
		}
	}
	if len(keys) == 0 {
		return `rowid`
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ordinal < keys[j].ordinal })
	quoted := make([]string, len(keys))
	for i, key := range keys {
		quoted[i] = quoteIdentifier(key.name)
	}
	return strings.Join(quoted, ",")
}

func legacyImportSchema(t *testing.T) fs.FS {
	t.Helper()
	return storeV1FixtureMigrations()
}

func createLegacyFixture(t *testing.T, dataDir string) (string, *sql.DB) {
	t.Helper()
	path := storepath.DataDirPath(dataDir, storepath.LegacyFilename)
	conn := openTestDB(t, path)
	require.NoError(t, Migrate(t.Context(), conn))
	_, err := conn.ExecContext(t.Context(), `PRAGMA wal_autocheckpoint=0`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions (id,title,updated_at,created_at,cancel_requested) VALUES ('s','s',1700000000,1700000000,1)`)
	require.NoError(t, err)
	for _, stmt := range []string{
		`INSERT INTO messages (id,session_id,role,parts,created_at,updated_at) VALUES ('m','s','user','[]',1700000000,1700000000)`,
		`INSERT INTO files (id,session_id,path,content,version,created_at,updated_at) VALUES ('f','s','f.txt','snapshot',1,1,1)`,
		`INSERT INTO read_files (session_id,path,read_at) VALUES ('s','f.txt',1)`,
		`INSERT INTO session_permissions (id,session_id,tool_name,action,path,created_at,enabled) VALUES ('p','s','bash','allow','/',1,1)`,
		`INSERT INTO queue_tasks (id,prompt,status,created_at) VALUES ('q','queue','done',1)`,
		`INSERT INTO queue_tasks (id,prompt,status,created_at,started_at,cost,tokens,exit_reason,finished_at) VALUES ('q-running','queue-running','running',1,2,3.5,42,'boom',3)`,
		`INSERT INTO pending_injects (id,session_id,message_id,content,created_at) VALUES ('i','s','m','inject',1)`,
		`INSERT INTO session_run_queue (id,session_id,call_data,status,created_at,updated_at) VALUES ('r','s','{}','pending',1,1)`,
		`INSERT INTO session_run_queue (id,session_id,call_data,status,leased_by,leased_at,lease_expires_at,attempts,last_error,created_at,updated_at) VALUES ('r-leased','s','{}','leased','dead-host',1,2,5,'prior',1,1)`,
		`INSERT INTO orphan_call_outbox (id,session_id,call_data,status,created_at,updated_at) VALUES ('o','s','{}','pending',1,1)`,
		`INSERT INTO orphan_call_outbox (id,session_id,call_data,status,attempts,created_at,updated_at) VALUES ('o-claim','s','{"x":1}','processing',1,1,2)`,
		`INSERT INTO async_hosts (id,pid,label,started_at) VALUES ('old-host',1,'cli',1)`,
		`INSERT INTO session_drivers (session_id,host_id,pid,claimed_at) VALUES ('s','old-host',1,1)`,
		`INSERT INTO session_notices (owner,kind,text,wake,delivery,job_tool_call_id,created_at,updated_at) VALUES ('s','wake_failed','notice',1,'pending',NULL,1,1)`,
		`INSERT INTO session_notices (owner,kind,text,wake,delivery,job_tool_call_id,created_at,updated_at) VALUES ('s','supervision','old job notice',1,'pending','job-call',1,1)`,
		`INSERT INTO wake_schedules (id,owner_session_id,kind,message,next_run_at,state,lease_owner,lease_expires_at,created_at,updated_at) VALUES ('w','s','once','wake',2,'active','old-host',9,1,1)`,
		`INSERT INTO async_jobs (owner_session_id,tool_call_id,kind,input_hash,host_id,state,created_at,updated_at) VALUES ('s','job-call','command','h','old-host','running',1,1)`,
		`INSERT INTO batch_nodes (node_id,root_id,owner_session_id,host_id,ordinal,kind,tool_call_id,input_hash,mode,state,created_at,updated_at) VALUES ('b','b','s','old-host',0,'batch','call','hash','parallel','pending',1,1)`,
		`INSERT INTO batch_control_requests (request_id,owner_session_id,root_id,node_id,host_id,created_at) VALUES ('br','s','b','b','old-host',1)`,
		`INSERT INTO batch_runner_checkpoints (host_id,runner_id,sequence,created_at,updated_at) VALUES ('old-host','runner',1,1,1)`,
	} {
		_, err := conn.ExecContext(t.Context(), stmt)
		require.NoError(t, err, stmt)
	}
	var busy, logFrames, checkpointed int
	require.NoError(t, conn.QueryRowContext(t.Context(), `PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &logFrames, &checkpointed))
	require.Zero(t, busy)
	require.Equal(t, logFrames, checkpointed)
	_, err = conn.ExecContext(t.Context(), `PRAGMA wal_autocheckpoint=0`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO messages (id,session_id,role,parts,created_at,updated_at) VALUES ('wal-committed','s','assistant','[]',2,2)`)
	require.NoError(t, err)
	return path, conn
}

func TestLegacyImportPolicyExhaustive(t *testing.T) {
	t.Parallel()
	conn := openTestDB(t, filepath.Join(t.TempDir(), "legacy.db"))
	fsys, err := legacyMigrationFS()
	require.NoError(t, err)
	require.NoError(t, migrateLocked(t.Context(), conn, "", fsys, MigrateOptions{MayMigrate: true}))
	rows, err := conn.QueryContext(t.Context(), `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	names = append(names, "sqlite_sequence")
	policy := make([]string, 0, len(LegacyImportPolicy))
	for _, p := range LegacyImportPolicy {
		require.NotEmpty(t, p.Reason, p.Name)
		policy = append(policy, p.Name)
	}
	sort.Strings(policy)
	sort.Strings(names)
	require.Equal(t, names, policy)
}

func TestLegacyImportCopiesDurableAndDropsRuntimeState(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	defer legacyConn.Close()
	// Independent WAL proof: copy only the main legacy db file (no -wal/-shm);
	// the late committed 'wal-committed' row lives in the -wal, so a main-only
	// copy must not see it while the live connection does.
	mainOnly := filepath.Join(t.TempDir(), "main-only.db")
	srcFile, err := os.Open(legacy)
	require.NoError(t, err)
	dstFile, err := os.Create(mainOnly)
	require.NoError(t, err)
	_, err = io.Copy(dstFile, srcFile)
	require.NoError(t, errors.Join(srcFile.Close(), dstFile.Close(), err))
	probe, err := openReadDB(mainOnly)
	require.NoError(t, err)
	require.Equal(t, 0, len(queryAllRows(t, probe, `SELECT id FROM messages WHERE id='wal-committed'`)), "wal-committed must live in the -wal, not the main file")
	require.NoError(t, probe.Close())
	require.Equal(t, 1, len(queryAllRows(t, legacyConn, `SELECT id FROM messages WHERE id='wal-committed'`)))
	before := legacyImportHashes(t, legacy)
	require.Contains(t, before, "-wal")
	require.Contains(t, before, "-shm")
	result, err := ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
	require.NoError(t, err)
	require.Equal(t, LegacyImportImported, result.Status)
	require.Equal(t, before, legacyImportHashes(t, legacy), "source must be identical while still open")
	newDB := openTestDB(t, storepath.StoreV1Path(dataDir))
	defer newDB.Close()
	newDB.SetMaxOpenConns(1)
	// Full-row comparison: every carried table, all intersecting columns,
	// ordered by rowid, with the production normalization applied to the
	// expected source rows.
	for _, policy := range LegacyImportPolicy {
		if !policy.Carry {
			continue
		}
		sourceCols, sourceExists, err := tableColumnInfo(t.Context(), legacyConn, policy.Name)
		require.NoError(t, err)
		if !sourceExists {
			continue
		}
		targetCols, targetExists, err := tableColumnInfo(t.Context(), newDB, policy.Name)
		require.NoError(t, err)
		require.True(t, targetExists, policy.Name)
		cols := intersectColumns(sourceCols, targetCols)
		require.NotEmpty(t, cols, policy.Name)
		quoted := make([]string, len(cols))
		for i, col := range cols {
			quoted[i] = quoteIdentifier(col.Name)
		}
		order := orderedByKeyColumns(cols)
		query := `SELECT ` + strings.Join(quoted, ",") + ` FROM ` + quoteIdentifier(policy.Name) + ` ORDER BY ` + order
		sourceRows, destRows := queryAllRows(t, legacyConn, query), queryAllRows(t, newDB, query)
		require.Equal(t, len(sourceRows), len(destRows), policy.Name)
		for i := range sourceRows {
			want := normalizeLegacyRow(policy.Name, cols, sourceRows[i])
			for c := range want {
				// SQLite affinity may scan a REAL zero as float64 against an int64 expectation.
				require.True(t, equalSQLiteValue(want[c], destRows[i][c]), "%s row %d column %s: want %#v got %#v", policy.Name, i, cols[c].Name, want[c], destRows[i][c])
			}
		}
	}
	require.Equal(t, 2, len(queryAllRows(t, newDB, `SELECT owner,kind,text,delivery,wake,reacted,job_tool_call_id FROM session_notices ORDER BY rowid`)))
	require.Equal(t, "[]", mustScalar(t, newDB, `SELECT parts FROM messages WHERE id='wal-committed'`))
	for _, table := range []string{"async_hosts", "session_drivers", "batch_nodes", "batch_control_requests", "batch_runner_checkpoints"} {
		var count int
		require.NoError(t, newDB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+quoteIdentifier(table)).Scan(&count), table)
		require.Zero(t, count, table)
	}
	var sequence int
	require.NoError(t, newDB.QueryRowContext(t.Context(), `SELECT seq FROM sqlite_sequence WHERE name='session_notices'`).Scan(&sequence))
	require.Equal(t, 2, sequence)
	require.Equal(t, before, legacyImportHashes(t, legacy))
	require.NoError(t, legacyConn.Close())
	legacyImportNoArtifacts(t, dataDir)
}

func TestLegacyImportMidCopyFailureRollsBack(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	defer legacyConn.Close()
	_, err := legacyConn.ExecContext(t.Context(), `INSERT INTO read_files (session_id,path,read_at) VALUES ('s','boom',3)`)
	require.NoError(t, err)
	expected := legacyImportHashes(t, legacy)
	schemaSQL, err := os.ReadFile(filepath.Join("testdata", "store_v1", "20261015000001_store_v1_initial.sql"))
	require.NoError(t, err)
	require.Contains(t, string(schemaSQL), `path TEXT NOT NULL CHECK (path != '')`)
	broken := strings.Replace(string(schemaSQL), `path TEXT NOT NULL CHECK (path != '')`, `path TEXT NOT NULL CHECK (path != '' AND path <> 'boom')`, 1)
	schema := fstest.MapFS{"20261015000001_store_v1_initial.sql": &fstest.MapFile{Data: []byte(broken)}}
	_, err = ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: schema})
	require.ErrorContains(t, err, "read_files")
	_, err = os.Stat(storepath.StoreV1Path(dataDir))
	require.ErrorIs(t, err, os.ErrNotExist)
	legacyImportNoArtifacts(t, dataDir)
	require.Equal(t, expected, legacyImportHashes(t, legacy))
}

func TestLegacyImportNoopExistingOrMissing(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	result, err := ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{})
	require.NoError(t, err)
	require.Equal(t, LegacyImportNoop, result.Status)
	err = os.WriteFile(storepath.StoreV1Path(dataDir), []byte("not-empty"), 0o600)
	require.NoError(t, err)
	result, err = ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{})
	require.NoError(t, err)
	require.Equal(t, LegacyImportNoop, result.Status)
	stored, err := os.ReadFile(storepath.StoreV1Path(dataDir))
	require.NoError(t, err)
	require.Equal(t, []byte("not-empty"), stored)
}

func TestLegacyImportLiveHostRefuses(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	before := legacyImportHashes(t, legacy)
	defer legacyConn.Close()
	lock, err := filelock.TryAcquireFileLock(filepath.Join(dataDir, "hosts", "old-host.lock"))
	require.NoError(t, err)
	defer lock.Release()
	_, err = ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
	require.ErrorIs(t, err, ErrLegacyImportLiveHosts)
	require.Equal(t, before, legacyImportHashes(t, legacy))
	_, err = os.Stat(storepath.StoreV1Path(dataDir))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestLegacyImportMissingRequiredColumnDoesNotPublish(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	defer legacyConn.Close()
	before := legacyImportHashes(t, legacy)
	broken := fstest.MapFS{"20991010000001_bad.sql": &fstest.MapFile{Data: []byte(`-- +goose Up
CREATE TABLE sessions (id TEXT PRIMARY KEY, required_field TEXT NOT NULL);
-- +goose Down
DROP TABLE sessions;`)}}
	_, err := ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: broken})
	require.ErrorContains(t, err, `required target column "required_field"`)
	require.Equal(t, before, legacyImportHashes(t, legacy))
	_, err = os.Stat(storepath.StoreV1Path(dataDir))
	require.ErrorIs(t, err, os.ErrNotExist)
	legacyImportNoArtifacts(t, dataDir)
}

func TestLegacyImportConcurrentOneWin(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	before := legacyImportHashes(t, legacy)
	var wg sync.WaitGroup
	results := make(chan LegacyImportResult, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	imported, noop := 0, 0
	for result := range results {
		if result.Status == LegacyImportImported {
			imported++
		} else {
			noop++
		}
	}
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, imported)
	require.Equal(t, 1, noop)
	require.Equal(t, before, legacyImportHashes(t, legacy))
	require.NoError(t, legacyConn.Close())
	conn := openTestDB(t, storepath.StoreV1Path(dataDir))
	defer conn.Close()
	var integrity string
	require.NoError(t, conn.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&integrity))
	require.Equal(t, "ok", integrity)
	legacyImportNoArtifacts(t, dataDir)
}

func TestLegacyImportNormalisationLiterals(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	defer legacyConn.Close()
	before := legacyImportHashes(t, legacy)
	result, err := ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
	require.NoError(t, err)
	require.Equal(t, LegacyImportImported, result.Status)
	require.Equal(t, before, legacyImportHashes(t, legacy))
	newDB := openTestDB(t, storepath.StoreV1Path(dataDir))
	defer newDB.Close()
	ctx := t.Context()

	// Queue_tasks running->pending mirrors queue.Service.ReclaimRunning.
	var status, exitReason string
	var cost float64
	var tokens int64
	var startedAt, finishedAt sql.NullInt64
	require.NoError(t, newDB.QueryRowContext(ctx,
		`SELECT status, cost, tokens, exit_reason, started_at, finished_at FROM queue_tasks WHERE id='q-running'`,
	).Scan(&status, &cost, &tokens, &exitReason, &startedAt, &finishedAt))
	require.Equal(t, "pending", status)
	require.Equal(t, float64(0), cost)
	require.Equal(t, int64(0), tokens)
	require.Equal(t, "", exitReason)
	require.False(t, startedAt.Valid)
	require.False(t, finishedAt.Valid)
	require.NoError(t, newDB.QueryRowContext(ctx, `SELECT status FROM queue_tasks WHERE id='q'`).Scan(&status))
	require.Equal(t, "done", status)

	// Session_run_queue lease recovery mirrors CleanupExpiredLeases, including
	// the consumed attempt and the poison-entry marker.
	var leasedBy sql.NullString
	var leasedAt, leaseExpiresAt sql.NullInt64
	var attempts, terminalFailure int64
	var lastError, callData string
	require.NoError(t, newDB.QueryRowContext(ctx,
		`SELECT status, leased_by, leased_at, lease_expires_at, attempts, last_error, terminal_failure, call_data FROM session_run_queue WHERE id='r-leased'`,
	).Scan(&status, &leasedBy, &leasedAt, &leaseExpiresAt, &attempts, &lastError, &terminalFailure, &callData))
	require.Equal(t, "pending", status)
	require.False(t, leasedBy.Valid)
	require.False(t, leasedAt.Valid)
	require.False(t, leaseExpiresAt.Valid)
	require.Equal(t, int64(6), attempts)
	require.Equal(t, "lease_expired", lastError)
	require.Equal(t, int64(0), terminalFailure)
	require.Equal(t, "{}", callData)
	var pendingLastError sql.NullString
	require.NoError(t, newDB.QueryRowContext(ctx,
		`SELECT status, attempts, last_error FROM session_run_queue WHERE id='r'`,
	).Scan(&status, &attempts, &pendingLastError))
	require.Equal(t, "pending", status)
	require.Equal(t, int64(0), attempts)
	require.False(t, pendingLastError.Valid)

	// Job-bound notices are voided with their text preserved as history.
	var delivery, noticeText string
	var wake, reacted int64
	var jobToolCallID sql.NullString
	require.NoError(t, newDB.QueryRowContext(ctx,
		`SELECT delivery, wake, reacted, job_tool_call_id, text FROM session_notices WHERE kind='supervision'`,
	).Scan(&delivery, &wake, &reacted, &jobToolCallID, &noticeText))
	require.Equal(t, "void", delivery)
	require.Equal(t, int64(0), wake)
	require.Equal(t, int64(1), reacted)
	require.False(t, jobToolCallID.Valid)
	require.Equal(t, "old job notice", noticeText)
	require.NoError(t, newDB.QueryRowContext(ctx,
		`SELECT delivery, wake, reacted FROM session_notices WHERE kind='wake_failed'`,
	).Scan(&delivery, &wake, &reacted))
	require.Equal(t, "pending", delivery)
	require.Equal(t, int64(1), wake)
	require.Equal(t, int64(0), reacted)

	// Wake schedule leases are released; the schedule itself is untouched.
	var state string
	var nextRunAt int64
	var leaseOwner sql.NullString
	var leaseExpires sql.NullInt64
	require.NoError(t, newDB.QueryRowContext(ctx,
		`SELECT state, next_run_at, lease_owner, lease_expires_at FROM wake_schedules WHERE id='w'`,
	).Scan(&state, &nextRunAt, &leaseOwner, &leaseExpires))
	require.Equal(t, "active", state)
	require.Equal(t, int64(2), nextRunAt)
	require.False(t, leaseOwner.Valid)
	require.False(t, leaseExpires.Valid)

	// Shutdown cancellation is cleared and the carried outbox call survives.
	var cancelRequested int64
	require.NoError(t, newDB.QueryRowContext(ctx, `SELECT cancel_requested FROM sessions WHERE id='s'`).Scan(&cancelRequested))
	require.Equal(t, int64(0), cancelRequested)
	var outboxCallData string
	require.NoError(t, newDB.QueryRowContext(ctx, `SELECT status, call_data FROM orphan_call_outbox WHERE id='o'`).Scan(&status, &outboxCallData))
	require.Equal(t, "pending", status)
	require.Equal(t, "{}", outboxCallData)
	// The retired pre-P0-4 'processing' claim state must not survive the
	// import: the drain only scans 'pending', so a carried claim would strand
	// its call forever. The payload must be preserved verbatim.
	require.NoError(t, newDB.QueryRowContext(ctx, `SELECT status, call_data FROM orphan_call_outbox WHERE id='o-claim'`).Scan(&status, &outboxCallData))
	require.Equal(t, "pending", status)
	require.Equal(t, `{"x":1}`, outboxCallData)
}

// legacyImportLegacyFSUpTo returns the importer-owned legacy chain trimmed to
// the migrations a binary with the given head had applied.
func legacyImportLegacyFSUpTo(t *testing.T, version int64) fstest.MapFS {
	t.Helper()
	fsys, err := legacyMigrationFS()
	require.NoError(t, err)
	entries, err := fs.ReadDir(fsys, ".")
	require.NoError(t, err)
	out := fstest.MapFS{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(entry.Name(), "_")
		entryVersion, err := strconv.ParseInt(prefix, 10, 64)
		require.NoError(t, err)
		if entryVersion > version {
			continue
		}
		data, err := fs.ReadFile(fsys, entry.Name())
		require.NoError(t, err)
		out[entry.Name()] = &fstest.MapFile{Data: data}
	}
	require.NotEmpty(t, out)
	return out
}

func TestLegacyImportMigratesOldSnapshotToLegacyHead(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	legacy := storepath.DataDirPath(dataDir, storepath.LegacyFilename)
	conn := openTestDB(t, legacy)
	// 20260816000002 is the last version before the smart/fast model-slot
	// rename: sessions still has large_model_*/small_model_*, and neither
	// async_hosts nor the run-cost columns exist yet.
	require.NoError(t, migrateLocked(t.Context(), conn, "", legacyImportLegacyFSUpTo(t, 20260816000002), MigrateOptions{MayMigrate: true}))
	for _, stmt := range []string{
		`INSERT INTO sessions (id,title,updated_at,created_at) VALUES ('old','old',1700000000,1700000000)`,
		`INSERT INTO sessions (id,title,parent_session_id,updated_at,created_at) VALUES ('m$$tc','child','old',1700000000,1700000000)`,
		`INSERT INTO sessions (id,title,parent_session_id,updated_at,created_at) VALUES ('g$$x','grand','m$$tc',1700000000,1700000000)`,
		`UPDATE sessions SET large_model_provider='prov-x', large_model_id='model-x', large_model_reasoning_effort='high' WHERE id='old'`,
		`UPDATE sessions SET small_model_provider='prov-y', small_model_id='model-y' WHERE id='old'`,
		`UPDATE sessions SET cost=10 WHERE id='old'`,
		`UPDATE sessions SET cost=4, parent_cost_accounted=3 WHERE id='m$$tc'`,
		`UPDATE sessions SET cost=9, parent_cost_accounted=5 WHERE id='g$$x'`,
		`INSERT INTO messages (id,session_id,role,parts,created_at,updated_at) VALUES ('m','old','user','[]',1700000000,1700000000)`,
	} {
		_, err := conn.ExecContext(t.Context(), stmt)
		require.NoError(t, err, stmt)
	}
	require.NoError(t, conn.Close())
	before := legacyImportHashes(t, legacy)
	result, err := ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
	// An unmigrated snapshot would be refused forever: the live-host probe
	// queries async_hosts, which this schema version does not have yet.
	require.NoError(t, err)
	require.Equal(t, LegacyImportImported, result.Status)
	require.Equal(t, before, legacyImportHashes(t, legacy))
	newDB := openTestDB(t, storepath.StoreV1Path(dataDir))
	defer newDB.Close()
	ctx := t.Context()
	// Renamed model slots arrive under their new names.
	var smartProvider, smartID, smartEffort, fastProvider, fastID, fastEffort string
	require.NoError(t, newDB.QueryRowContext(ctx,
		`SELECT smart_model_provider, smart_model_id, smart_model_reasoning_effort, fast_model_provider, fast_model_id, fast_model_reasoning_effort FROM sessions WHERE id='old'`,
	).Scan(&smartProvider, &smartID, &smartEffort, &fastProvider, &fastID, &fastEffort))
	require.Equal(t, "prov-x", smartProvider)
	require.Equal(t, "model-x", smartID)
	require.Equal(t, "high", smartEffort)
	require.Equal(t, "prov-y", fastProvider)
	require.Equal(t, "model-y", fastID)
	require.Equal(t, "medium", fastEffort)
	// The run-cost backfill ran on the snapshot during the import: the child
	// transferred parent_cost_accounted=3 of the parent's cost=10, and the
	// grandchild transferred 5 over the child's own cost=4.
	var costSelf, costBase float64
	require.NoError(t, newDB.QueryRowContext(ctx, `SELECT cost_self, cost_base FROM sessions WHERE id='old'`).Scan(&costSelf, &costBase))
	require.InDelta(t, 7.0, costSelf, 0.000001)
	require.InDelta(t, 1.0, costBase, 0.000001)
	var costParentID string
	require.NoError(t, newDB.QueryRowContext(ctx, `SELECT cost_parent_id FROM sessions WHERE id='m$$tc'`).Scan(&costParentID))
	require.Equal(t, "old", costParentID)
}

func TestLegacyImportRefusesFutureVersion(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	defer legacyConn.Close()
	_, err := legacyConn.ExecContext(t.Context(), `INSERT INTO goose_db_version (version_id, is_applied) VALUES (20991010000001, 1)`)
	require.NoError(t, err)
	before := legacyImportHashes(t, legacy)
	_, err = ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
	require.ErrorIs(t, err, ErrLegacyImportFutureVersion)
	require.ErrorContains(t, err, "20991010000001")
	_, err = os.Stat(storepath.StoreV1Path(dataDir))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, before, legacyImportHashes(t, legacy))
	legacyImportNoArtifacts(t, dataDir)
}

func TestLegacyImportRefusesUnknownTables(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	defer legacyConn.Close()
	_, err := legacyConn.ExecContext(t.Context(), `CREATE TABLE legacy_custom (value TEXT)`)
	require.NoError(t, err)
	before := legacyImportHashes(t, legacy)
	_, err = ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
	require.ErrorIs(t, err, ErrLegacyImportUnknownTable)
	require.ErrorContains(t, err, "legacy_custom")
	_, err = os.Stat(storepath.StoreV1Path(dataDir))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, before, legacyImportHashes(t, legacy))
	legacyImportNoArtifacts(t, dataDir)
}

func TestLegacyImportRefusesUnknownColumns(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	defer legacyConn.Close()
	_, err := legacyConn.ExecContext(t.Context(), `ALTER TABLE queue_tasks ADD COLUMN future_col TEXT NOT NULL DEFAULT ''`)
	require.NoError(t, err)
	before := legacyImportHashes(t, legacy)
	_, err = ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
	require.ErrorIs(t, err, ErrLegacyImportUnknownColumns)
	require.ErrorContains(t, err, "queue_tasks")
	require.ErrorContains(t, err, "future_col")
	_, err = os.Stat(storepath.StoreV1Path(dataDir))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, before, legacyImportHashes(t, legacy))
	legacyImportNoArtifacts(t, dataDir)
}

func TestLegacyImportSweepsDebrisFromCrashedRun(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	_, legacyConn := createLegacyFixture(t, dataDir)
	defer legacyConn.Close()
	staleSnapshot := filepath.Join(dataDir, ".rush-import-snapshot-crashed")
	staleProbe := filepath.Join(dataDir, ".rush-host-probe-crashed")
	staleBuilding := filepath.Join(dataDir, ".rush-import-building-crashed.db")
	require.NoError(t, os.Mkdir(staleSnapshot, 0o755))
	require.NoError(t, os.Mkdir(staleProbe, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staleSnapshot, "legacy.db"), []byte("junk"), 0o600))
	require.NoError(t, os.WriteFile(staleBuilding, []byte("junk"), 0o600))
	require.NoError(t, os.WriteFile(staleBuilding+"-wal", []byte("junk"), 0o600))
	result, err := ImportLegacyStore(t.Context(), dataDir, LegacyImportOptions{Schema: legacyImportSchema(t)})
	require.NoError(t, err)
	require.Equal(t, LegacyImportImported, result.Status)
	for _, path := range []string{staleSnapshot, staleProbe, staleBuilding, staleBuilding + "-wal"} {
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist, path)
	}
}

func TestLegacyImportMigrationsMatchEmbedded(t *testing.T) {
	t.Parallel()
	legacyFS, err := legacyMigrationFS()
	require.NoError(t, err)
	activeFS, err := migrationFS()
	require.NoError(t, err)
	legacyEntries, err := fs.ReadDir(legacyFS, ".")
	require.NoError(t, err)
	activeEntries, err := fs.ReadDir(activeFS, ".")
	require.NoError(t, err)
	require.Len(t, legacyEntries, len(activeEntries))
	for i, entry := range legacyEntries {
		require.Equal(t, activeEntries[i].Name(), entry.Name())
		want, err := fs.ReadFile(activeFS, entry.Name())
		require.NoError(t, err)
		got, err := fs.ReadFile(legacyFS, entry.Name())
		require.NoError(t, err)
		require.Equal(t, string(want), string(got), entry.Name())
	}
}

func queryAllRows(t *testing.T, conn *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()
	columns, err := rows.Columns()
	require.NoError(t, err)
	var out [][]any
	for rows.Next() {
		values, ptrs := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		out = append(out, cloneRow(values))
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	return out
}

func cloneRow(row []any) []any {
	out := make([]any, len(row))
	for i, value := range row {
		if b, ok := value.([]byte); ok {
			out[i] = append([]byte(nil), b...)
		} else {
			out[i] = value
		}
	}
	return out
}

func mustScalar(t *testing.T, conn *sql.DB, query string) string {
	t.Helper()
	var value string
	require.NoError(t, conn.QueryRowContext(t.Context(), query).Scan(&value))
	return value
}

func legacyImportHashes(t *testing.T, path string) map[string][32]byte {
	t.Helper()
	out := make(map[string][32]byte)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		require.NoError(t, err)
		out[suffix] = sha256.Sum256(data)
	}
	return out
}

func legacyImportNoArtifacts(t *testing.T, dataDir string) {
	t.Helper()
	entries, err := os.ReadDir(dataDir)
	require.NoError(t, err)
	for _, entry := range entries {
		name := entry.Name()
		require.False(t, strings.HasPrefix(name, ".rush-import-snapshot-"), name)
		require.False(t, strings.HasPrefix(name, ".rush-host-probe-"), name)
		require.False(t, strings.HasPrefix(name, ".rush-import-building-"), name)
	}
}
