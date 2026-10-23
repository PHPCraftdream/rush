package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	require.NoError(t, migrateLegacySnapshot(t.Context(), conn))
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
		`INSERT INTO batch_timeout_checkins (root_id,node_id,host_id,token,kind,owner_session_id,created_at) VALUES ('b','b','old-host',0,'pimCheckin','s',1)`,
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
	t.Run("pimJournalModes", func(t *testing.T) {
		t.Parallel()
		pimJournalModes(t)
	})
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	t.Cleanup(func() { _ = legacyConn.Close() })
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
	t.Cleanup(func() { _ = newDB.Close() })
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
	for _, table := range []string{"async_hosts", "session_drivers", "batch_nodes", "batch_control_requests", "batch_runner_checkpoints", "batch_timeout_checkins"} {
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
	t.Run("pimFailureRetry", func(t *testing.T) {
		t.Parallel()
		pimFailureRetry(t)
	})
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	t.Cleanup(func() { _ = legacyConn.Close() })
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
	t.Run("pimExistingDestination", func(t *testing.T) {
		t.Parallel()
		pimExistingDestination(t)
	})
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
	t.Run("pimHostRefusal", func(t *testing.T) {
		t.Parallel()
		pimHostRefusal(t)
	})
	dataDir := t.TempDir()
	legacy, legacyConn := createLegacyFixture(t, dataDir)
	before := legacyImportHashes(t, legacy)
	t.Cleanup(func() { _ = legacyConn.Close() })
	lock, err := filelock.TryAcquireFileLock(filepath.Join(dataDir, "hosts", "old-host.lock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })
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
	pimCrashChild() // re-executed crash child: runs the import and exits
	t.Parallel()
	t.Run("pimPrepublicationCrashRetry", func(t *testing.T) {
		t.Parallel()
		pimPrepublicationCrashRetry(t)
	})
	dataDir := t.TempDir()
	_, legacyConn := createLegacyFixture(t, dataDir)
	t.Cleanup(func() { _ = legacyConn.Close() })
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

// legacyImportFrozenHashes pins the importer-owned frozen copy of the
// pre-store-v1 migration chain, byte for byte. Before H4 the frozen
// directory was compared against the active embedded chain; H4 replaced
// that chain with the single store-v1 migration, so the comparison now
// pins the frozen files directly: any edit to a frozen legacy migration
// (renaming a column, rewriting an index, adding a migration) changes a
// hash and fails this test, because such an edit would migrate legacy
// rush.db snapshots to a schema that never existed when the source data
// was written.
//
// legacyImportFrozenHeadVersion is the last pre-H4 embedded version; the
// frozen chain must end exactly there and must never contain the
// store-v1 migration.
func TestLegacyImportFrozenChainPinned(t *testing.T) {
	t.Parallel()
	legacyFS, err := legacyMigrationFS()
	require.NoError(t, err)
	entries, err := fs.ReadDir(legacyFS, ".")
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
		content, err := fs.ReadFile(legacyFS, entry.Name())
		require.NoError(t, err)
		sum := sha256.Sum256(content)
		require.Equal(t, legacyImportFrozenHashes[entry.Name()], hex.EncodeToString(sum[:]), entry.Name())
	}
	sort.Strings(names)
	require.Equal(t, legacyImportFrozenHeadVersion, names[len(names)-1],
		"frozen chain must end exactly at the last pre-H4 version")
	for name := range legacyImportFrozenHashes {
		require.NotContains(t, name, "store_v1", "frozen chain must never gain a store-v1 migration")
	}
	require.Len(t, names, len(legacyImportFrozenHashes))
}

// legacyImportFrozenHeadVersion is the last version the active embedded
// chain contained before H4 replaced it with the store-v1 schema; the
// frozen importer chain stops exactly at this version.
const legacyImportFrozenHeadVersion = "20261019000001_add_batch_timeout_checkins.sql"

// legacyImportFrozenHashes pins the frozen legacy migration chain
// byte-for-byte (sha256 over the exact embedded bytes).
var legacyImportFrozenHashes = map[string]string{
	"20250424200609_initial.sql":                               "07a62a147089293b128eed108b493de0de96dd2e47cdeedc0397bb8181e90a15",
	"20250515105448_add_summary_message_id.sql":                "1d8597089034d7386d93b5da34741401d4b0ebb9d33580403d8ded1ab768ad27",
	"20250624000000_add_created_at_indexes.sql":                "0c2cd46b4f3c3e29e9bf56705019264213e2069b10b33d62f06460d18568e612",
	"20250627000000_add_provider_to_messages.sql":              "adcb6ee4202b0a360518dc0f03778e5e64558ad22c8ff1e390a8236506749889",
	"20250810000000_add_is_summary_message.sql":                "5dae7e7fd55e2b6dac7e9190dd8759eeeb87b4aadabcefed44d67cb1df1a85f1",
	"20250812000000_add_todos_to_sessions.sql":                 "8cd37232d6862bf4bc6f7d97786a0f5c6dec28c656e87a09e0953645271a2649",
	"20260127000000_add_read_files_table.sql":                  "1b8cdda9ebbd0e9e4f415d2963bceaf33dc8d364915ec163277d4052ba6ed9a3",
	"20260308000001_add_models_to_sessions.sql":                "e4f1df90460ad7ef34263f6644eb5aa4978a953dbaaa9f4110b0d01eda61105a",
	"20260308000002_add_session_permissions.sql":               "95522f72ea6e725210c813d2bde1abb14d5a912b4d384b3a593d4e95b527f866",
	"20260308000003_add_system_prompt_to_sessions.sql":         "2b81e0b97a09dd98384eb783424323ac680ef78fa7a306bf9ed14d2bdca18a50",
	"20260309000001_cleanup_empty_permissions.sql":             "0b85f762d264b63ed3fd00b3ecf69cdbc44eafd1045be01701ffa6afe791a496",
	"20260310000001_add_pinned_to_messages.sql":                "927b0c1d4e8163683f91fd6730de5bdbb797b18654cdc7e74d2e415ce263957f",
	"20260311000001_add_hidden_to_messages.sql":                "2fcf82994da279b1d6adc64c4db2b5f3be53c45be1b7b91fc85ea431242cad5c",
	"20260312000001_add_yolo_to_sessions.sql":                  "44a77c5f73a11ae59c6f53c7bf93419a57abd0436285bd4e98815f78738788a1",
	"20260312000002_add_enabled_to_permissions.sql":            "75f913e3c06e90ab2c1dcb64097393390604ac9a24f5ca388727867ea041f472",
	"20260313000000_add_reasoning_effort_to_sessions.sql":      "0eca46bdf7d4c2c57a4cb6373c3bfb82afbe04a96ea7dda25de270bff31249b4",
	"20260313000001_add_reasoning_effort_to_messages.sql":      "6f1b4181281afaa3ca6623cbb555f09cb8c063ef37ae0122aab3ddc54673f317",
	"20260517000001_permissions_dedup_and_indexes.sql":         "efd8b04f7b45354dc9209db08ca7c1f10523e049bdd57a9f3c7d267a15d6308d",
	"20260520000001_add_cancel_to_sessions.sql":                "84cd6424e41ae1de5cde6169c1f99cb961f7b201764c9358839a09c1e8c3f167",
	"20260520000002_add_queue_tasks.sql":                       "a4265559e15a166eb429d6f41edc484f99b21b62fd465eb2fbfa31d7f76027a3",
	"20260521000001_add_ended_reason_and_budget.sql":           "bd81f2775b37bc85619ea79fcd33509addc873f19daabc08c8f2440f2dbb0f49",
	"20260625000001_add_auto_resumed_to_messages.sql":          "86673256401237f946e9cb3903811142b9b94d59cea8e2e2773d726583e9192e",
	"20260626000001_add_background_job_notice_to_messages.sql": "3c5093bb8191ce624977513f901139c4ec6f473ce4a681e69b51b2912c038fd7",
	"20260627000002_add_deleted_todos_to_sessions.sql":         "7f81acedffde53339ddd89fe1a40d2a7846fee7f661f31eec209d83103bbedb1",
	"20260703000001_add_pending_injects_table.sql":             "cf8270fdddb30e186a6cec3cedf4f199606cdeb0f01ade594c3683b3a6003644",
	"20260728000001_add_parent_cost_accounted_to_sessions.sql": "9ea94a2e7c31837d3bff1bdae3b23c153fb9b65fa53ef66bc2c2ef56f861e06a",
	"20260728000002_add_sessions_parent_session_id_index.sql":  "ab73913843eaf8bcd86bb8f7423db24527193170269639f969666c20e10e15ed",
	"20260809000001_add_session_run_queue.sql":                 "012085e9171d08567f9319cff8551631ce11ce1f2e7d997bb757f5802b6d7f27",
	"20260812000001_add_orphan_call_outbox.sql":                "64630e20bfe04c41447dc9c53c3bec8266f9cd5f91e7dd6399907e771eeffbc5",
	"20260816000001_add_worker_reviewer_to_sessions.sql":       "0622d18aeaccbfa80f46148bba153842c8c6a1c3a6b1a1c823630df10dc6ee17",
	"20260816000002_add_usage_to_messages.sql":                 "160e70bc7190915fab49c997dbaa2afc21da0f8713fcb2c307af5059a5e35ac3",
	"20260818000001_rename_model_slots_to_smart_fast.sql":      "0ffdb517609824af9348dd9759be8e16ca76be5332880e1f71a00d7abac65ba8",
	"20260819000001_add_checkpoint_generation_to_messages.sql": "f6f3bcd3974ce73f57d88dba149203b2b9ab2267b7f04e2d02968cf2f0f40669",
	"20260826000001_add_unfinished_assistant_index.sql":        "ea7102eb3a93f4a679598c855e31fc3f80dca85b18d63e500cd8546427180b48",
	"20260828000001_add_query_covering_indexes.sql":            "c98a8ffcea60674b051d821ead18916e1d079af9e313e87a045e77a4b5e91aa3",
	"20260831000001_add_origin_to_sessions_and_messages.sql":   "420f901f482e853075bf2596d4d98c7b30b6e6e04931e2fd161ba35ad365b7c3",
	"20260928000001_add_notice_kind_to_messages.sql":           "4f36341021b618032539f5c125029c6b8bffa0b4a57053ea0c33e014953e0352",
	"20260928000002_add_async_phase4_core.sql":                 "0dd9be9cecbd48ee47e7f1fadb46fc87dd4823a367bb0ca0a5ae202b8fcf5f3c",
	"20260929000001_add_session_notices_owner_index.sql":       "b667ebfb4de28baaf0612519108e56fd83536344acd84679765f8502296c8fc0",
	"20260929000002_add_claim_id_to_async_jobs.sql":            "6f620059d66d87979f2e5936c37cd8f50aa15307bf3a3bdc112089e7673a11a8",
	"20260929000003_add_announce_message_id_to_async_jobs.sql": "255416a1b4d809b316a44245f2817bb67f731d79aa9874bb26c67ef478a890c4",
	"20260929000004_add_session_drivers.sql":                   "a1d5d0d384342a090439eab41010e96902d942f23d106650eb11c738ee7a194c",
	"20260929000005_add_wake_schedules.sql":                    "7ace189d98b1203892bed0238dbbba1c8afb13bfd5368300415bf1af21208d8b",
	"20261001000001_run_cost_subtree.sql":                      "aa19b7a965b9cea9f7f635b658e36922bc640f9d0bae5443763c8af926e07cf7",
	"20261002000001_add_workspace_to_sessions.sql":             "4df80a84789e39edbe1366ed918bb063afe1b6bc4bc0157ec83e4b5da2af98a5",
	"20261005000001_bg_shell_jobs.sql":                         "8d9f4bc2e0af426e76ee74fa598d8ff8f2c9921b25912c5c2be2e8a64e92eb3e",
	"20261012000001_batch_projection.sql":                      "fd3a47cf2eaa9d8c101474001b54f22fe30552e93e80f6f16e41d69f5d5bcc49",
	"20261012000002_batch_runner_incarnation.sql":              "f6a47b975c286c654dff2d089b215d22a458b60b8b09570d637745ceef678af8",
	"20261019000001_add_batch_timeout_checkins.sql":            "1eff3bc1c5cad7b33fc8215e414ea817004be3887f8c2c0086113f5a03c66d74",
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
