package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/stretchr/testify/require"
)

func TestStoreV1QueriesOnFixture(t *testing.T) {
	ctx := context.Background()
	conn := openFixture(t, true)
	q := New(conn)
	session, err := q.CreateSession(ctx, CreateSessionParams{ID: "s1", Title: "query fixture", Cost: 0, WorkspaceRoot: "/workspace", GitBranch: "main"})
	require.NoError(t, err)
	require.Equal(t, "s1", session.ID)
	message, err := q.CreateMessage(ctx, CreateMessageParams{ID: "m1", SessionID: "s1", Role: "user", Parts: "[]", Origin: "test"})
	require.NoError(t, err)
	require.Equal(t, "m1", message.ID)
	messages, err := q.ListMessagesBySession(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	var messageCount int64
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT message_count FROM sessions WHERE id='s1'`).Scan(&messageCount))
	require.EqualValues(t, 1, messageCount)
	file, err := q.CreateFile(ctx, CreateFileParams{ID: "f1", SessionID: "s1", Path: "a.txt", Content: "snapshot", Version: 1})
	require.NoError(t, err)
	require.Equal(t, "snapshot", file.Content)
	latestFile, err := q.GetFileByPathAndSession(ctx, GetFileByPathAndSessionParams{Path: "a.txt", SessionID: "s1"})
	require.NoError(t, err)
	require.Equal(t, "f1", latestFile.ID)
	files, err := q.ListFilesBySession(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "f1", files[0].ID)
	require.NoError(t, q.RecordFileRead(ctx, RecordFileReadParams{SessionID: "s1", Path: "a.txt"}))
	readFile, err := q.GetFileRead(ctx, GetFileReadParams{SessionID: "s1", Path: "a.txt"})
	require.NoError(t, err)
	require.Equal(t, "a.txt", readFile.Path)
	require.NoError(t, q.CreateSessionPermission(ctx, CreateSessionPermissionParams{ID: "p1", SessionID: "s1", ToolName: "bash", Action: "allow", Path: "/workspace"}))
	permissions, err := q.ListSessionPermissions(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, permissions, 1)

	// queue_tasks has no generated sqlc queries; mirror internal/queue/queue.go Add/List SQL.
	_, err = conn.ExecContext(ctx, `INSERT INTO queue_tasks (id,session_id,prompt,status,created_at,workspace_root) VALUES ('q1','s1','task','pending',1,'/workspace')`)
	require.NoError(t, err)
	var queuePrompt string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT prompt FROM queue_tasks WHERE id=?`, "q1").Scan(&queuePrompt))
	require.Equal(t, "task", queuePrompt)
	// pending_injects is currently consumed through raw SQL in session_inject.go.
	require.NoError(t, q.CreatePendingInject(ctx, CreatePendingInjectParams{ID: "i1", SessionID: "s1", MessageID: "m1", Content: "inject", CreatedAt: 1}))
	injects, err := q.ListPendingInjectsBySession(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, injects, 1)
	entry, err := q.EnqueueRunQueueEntry(ctx, EnqueueRunQueueEntryParams{ID: "r1", SessionID: "s1", CallData: "{}", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	require.Equal(t, "r1", entry.ID)
	pendingRun, err := q.GetRunQueueEntry(ctx, "r1")
	require.NoError(t, err)
	require.Equal(t, "pending", pendingRun.Status)
	outbox, err := q.WriteToOrphanOutbox(ctx, WriteToOrphanOutboxParams{ID: "o1", SessionID: "s1", CallData: "{}", CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	require.Equal(t, "o1", outbox.ID)
	host, err := q.RegisterAsyncHostRow(ctx, RegisterAsyncHostRowParams{ID: "h1", Pid: 1, Label: "test", StartedAt: 1})
	require.NoError(t, err)
	require.Equal(t, "h1", host.ID)
	hostRows, err := q.ListAsyncHostRows(ctx)
	require.NoError(t, err)
	require.Len(t, hostRows, 1)
	_, err = q.InsertSessionDriver(ctx, InsertSessionDriverParams{SessionID: "s1", HostID: "h1", Pid: 1, ClaimedAt: 1})
	require.NoError(t, err)
	driver, err := q.GetSessionDriver(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, "h1", driver.HostID)
	notice, err := q.InsertSessionNotice(ctx, InsertSessionNoticeParams{Owner: "s1", Kind: "test", Text: "notice", Wake: 0, JobToolCallID: sql.NullString{String: "call1", Valid: true}, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	require.Equal(t, "call1", notice.JobToolCallID.String)
	pendingNotices, err := q.ListPendingSessionNoticesForOwner(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, pendingNotices, 1)
	wake, err := q.InsertWakeSchedule(ctx, InsertWakeScheduleParams{ID: "w1", OwnerSessionID: "s1", Kind: "once", Message: "wake", NextRunAt: 1, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	require.Equal(t, "w1", wake.ID)

	root, err := q.ReserveBatchNode(ctx, ReserveBatchNodeParams{
		NodeID: "root", RootID: "root", OwnerSessionID: "s1", HostID: "h1", RunnerID: "run1", Ordinal: 0,
		Kind: "batch", ToolCallID: sql.NullString{String: "tool1", Valid: true}, InputHash: sql.NullString{String: "hash", Valid: true},
		Mode: sql.NullString{String: "sequential", Valid: true}, TaskGroup: "", State: "pending", Cause: "", ControlState: "none",
		OutcomeState: "", CreatedAt: 1, UpdatedAt: 1,
	})
	require.NoError(t, err)
	require.Equal(t, "root", root.NodeID)
	control, err := q.CreateBatchControlRequest(ctx, CreateBatchControlRequestParams{RequestID: "c1", OwnerSessionID: "s1", RootID: "root", NodeID: "root", HostID: "h1", CreatedAt: 1})
	require.NoError(t, err)
	require.Equal(t, "run1", control.RunnerID)
	changed, err := q.UpdateBatchRunnerCheckpoint(ctx, UpdateBatchRunnerCheckpointParams{HostID: "h1", RunnerID: "run1", Sequence: 1, EventType: "started", UpdatedAt: 1})
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
	var checkpointSequence int64
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT sequence FROM batch_runner_checkpoints WHERE host_id='h1' AND runner_id='run1'`).Scan(&checkpointSequence))
	require.EqualValues(t, 1, checkpointSequence)
}

type storeFileSnapshot struct {
	path string
	size int64
	mode os.FileMode
	time int64
	hash [32]byte
}

func snapshotStoreFile(t *testing.T, path string) storeFileSnapshot {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return storeFileSnapshot{path: path, size: info.Size(), mode: info.Mode(), time: info.ModTime().UnixNano(), hash: sha256.Sum256(contents)}
}

func requireStoreFileUnchanged(t *testing.T, want storeFileSnapshot) {
	t.Helper()
	got := snapshotStoreFile(t, want.path)
	require.Equal(t, want.size, got.size, want.path)
	require.Equal(t, want.mode, got.mode, want.path)
	require.Equal(t, want.time, got.time, want.path)
	require.Equal(t, want.hash, got.hash, want.path)
}

func storeV1FixtureMigrations() fs.FS {
	return os.DirFS("testdata/store_v1")
}

func TestStoreV1CreationLeavesLegacyDatabaseAndSidecarsUntouched(t *testing.T) {
	dataDir := t.TempDir()
	legacyPath := storepath.DataDirPath(dataDir, storepath.LegacyFilename)
	legacy, err := openDB(legacyPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, legacy.Close()) })
	_, err = legacy.ExecContext(t.Context(), `CREATE TABLE legacy_marker (value TEXT)`)
	require.NoError(t, err)
	_, err = legacy.ExecContext(t.Context(), `INSERT INTO legacy_marker VALUES ('old-store')`)
	require.NoError(t, err)
	_, err = legacy.ExecContext(t.Context(), `CREATE TABLE async_hosts (id TEXT PRIMARY KEY,pid INTEGER NOT NULL,label TEXT NOT NULL DEFAULT '',started_at INTEGER NOT NULL)`)
	require.NoError(t, err)
	_, err = legacy.ExecContext(t.Context(), `INSERT INTO async_hosts VALUES ('old-host',42,'legacy',1)`)
	require.NoError(t, err)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	// Seed stable sidecar bytes only after closing SQLite; the new-store path
	// must not inspect or open any legacy artifact, including old host rows.
	for _, suffix := range []string{"-wal", "-shm"} {
		require.NoError(t, os.WriteFile(legacyPath+suffix, []byte("legacy-sidecar"+suffix), 0o600))
	}
	oldFiles := []storeFileSnapshot{snapshotStoreFile(t, legacyPath), snapshotStoreFile(t, legacyPath+"-wal"), snapshotStoreFile(t, legacyPath+"-shm")}

	newPath := storepath.StoreV1Path(dataDir)
	newDB, err := openDB(newPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, newDB.Close()) })
	fixture := storeV1FixtureMigrations()
	require.NoError(t, migrateLocked(context.Background(), newDB, newPath, fixture, MigrateOptions{MayMigrate: true, MigrationLockPath: filepath.Join(dataDir, storepath.StoreV1MigrateLockFilename)}))
	var hosts int
	require.NoError(t, newDB.QueryRowContext(t.Context(), `SELECT count(*) FROM async_hosts`).Scan(&hosts))
	require.Zero(t, hosts)
	for _, file := range oldFiles {
		requireStoreFileUnchanged(t, file)
	}
	require.NoError(t, newDB.Close())
	for _, file := range oldFiles {
		requireStoreFileUnchanged(t, file)
	}
}

func TestStorePathLifecycleUsesLegacyFilename(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() { require.NoError(t, ReleaseAll(dataDir)) })
	conn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)
	_, err = os.Stat(storepath.DataDirPath(dataDir, storepath.LegacyFilename))
	require.NoError(t, err)
	require.NoError(t, ReleaseAll(dataDir))
	_, err = conn.ExecContext(t.Context(), `SELECT 1`)
	require.Error(t, err)
	if _, err := os.Stat(storepath.DataDirPath(dataDir, storepath.StoreV1Filename)); !os.IsNotExist(err) {
		t.Fatalf("inactive store unexpectedly created: %v", err)
	}
}
