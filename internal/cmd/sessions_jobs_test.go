package cmd

// Real-DB, real-lock-file coverage for `rush sessions jobs`: table and
// --json output, a foreign-live-host row's kill hint (naming its PID), and
// a dead-host row showing as dead. Mirrors the isolatedListEnvWithConfigured
// DataDir pattern (sessions_list_test.go): a SEED App claims/inserts rows,
// then sessionsJobsCmd.RunE builds its OWN fresh App on the same data dir to
// read them back, exactly like a genuinely separate process would.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// isolatedJobsEnv mirrors isolatedListEnvWithConfiguredDataDir but wires up
// sessionsJobsCmd instead.
func isolatedJobsEnv(t *testing.T) (a *app.App, dataDir string) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	dataDir = filepath.Join(tmp, "jobs-data")

	ctx, cancel := context.WithCancel(context.Background())
	ensureRootFlagStandIns(sessionsJobsCmd, dataDir)
	if f := sessionsJobsCmd.Flags().Lookup("json"); f == nil {
		sessionsJobsCmd.Flags().Bool("json", false, "")
	}
	require.NoError(t, sessionsJobsCmd.Flags().Set("json", "false"))
	sessionsJobsCmd.SetContext(ctx)

	built, err := setupApp(sessionsJobsCmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, built.Config().Options.DataDirectory)

	t.Cleanup(func() {
		builtDataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		_ = os.Chdir(orig)
		cancel()
		_ = db.Release(builtDataDir)
		waitForSQLiteHandleRelease(t, builtDataDir)
		waitForSQLiteHandleRelease(t, workDir)
	})

	return built, dataDir
}

// claimForeignRunning inserts a running async_jobs row directly via
// db.Queries, attributed to hostID -- a host this test process never
// registers via session.RegisterHost/AsyncJobStore.Claim (which would mark
// it "own" in the process-wide registry every AsyncJobStore in THIS test
// binary shares -- see host_lock.go's own doc). Genuinely foreign from the
// reading App's point of view, exactly like a row written by a real,
// separate process.
func claimForeignRunning(ctx context.Context, q *db.Queries, owner, toolCallID, hostID string) error {
	_, err := q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: owner, ToolCallID: toolCallID, Kind: "command", ToolName: "bash",
		InputHash: "h", HostID: hostID, CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	return err
}

func runJobsCmd(t *testing.T, args ...string) string {
	t.Helper()
	return captureStdout(t, func() {
		runErr := sessionsJobsCmd.RunE(sessionsJobsCmd, args)
		require.NoError(t, runErr)
	})
}

// TestSessionsJobsCmdRun_TableAndForeignLiveHostHint covers: a table listing
// with an own-process running row (alive without probing), a foreign LIVE
// host's running row (prints the kill hint naming its PID), and a foreign
// DEAD host's row (shows dead, no hint).
func TestSessionsJobsCmdRun_TableAndForeignLiveHostHint(t *testing.T) {
	batchOn := config.BatchRuntimeEnabled()
	a, dataDir := isolatedJobsEnv(t)
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "jobs-table-sess", "session with jobs")
	require.NoError(t, err)

	// Dual mode: ON the table reports batch roots and no host hint; OFF keeps
	// the legacy async-job liveness and host-hint contract below.
	if batchOn {
		foreignID := "foreign-live-host"
		foreignLock, err := filelock.TryAcquireFileLock(session.HostLockPath(dataDir, foreignID))
		require.NoError(t, err)
		t.Cleanup(func() { _ = foreignLock.Release() })
		q := db.New(a.DB())
		_, err = q.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{ID: foreignID, Pid: 4242, Label: "other-rush", StartedAt: 1700000000})
		require.NoError(t, err)
		reserved, err := session.NewBatchStore(a.DB()).Reserve(ctx, session.BatchReservation{
			Owner: sess.ID, Host: foreignID, RunnerID: "foreign-batch-runner",
			ToolCallID: "foreign-batch-call", InputHash: "foreign-batch-hash", Sequence: 1,
			Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none"}},
		})
		require.NoError(t, err)
		require.NotEmpty(t, reserved.Snapshot.Root.RootID)
		a.Shutdown()
		stdout := runJobsCmd(t, sess.ID)
		lines := strings.Split(stdout, "\n")
		var foreignLine string
		for _, line := range lines {
			if strings.Contains(line, "foreign-batch-call") {
				foreignLine = line
			}
		}
		require.NotEmpty(t, foreignLine)
		require.NotContains(t, strings.Fields(foreignLine), "4242")
		require.Contains(t, stdout, "TOOL_CALL_ID")
		require.Contains(t, stdout, "foreign-batch-call")
		require.Contains(t, foreignLine, "running")
		require.Contains(t, foreignLine, "true")
		require.NotContains(t, stdout, "taskkill")
		require.NotContains(t, stdout, "kill -INT")
		return
	}
	// Own-process row: claimed via this seed App's own AsyncJobStore, so its
	// host is "own" -- alive by definition, without probing.
	store := a.AsyncJobStore()
	require.NotNil(t, store)
	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: sess.ID, ToolCallID: "own-call-1", Kind: session.JobKindCommand, Input: "echo hi", ToolName: "bash",
	})
	require.NoError(t, err)

	// Foreign LIVE host: a real held lock file this test process itself
	// controls (via the generic FileLock primitive, which does NOT mark
	// ownership), plus a display-only async_hosts row naming its PID.
	q := db.New(a.DB())
	foreignLock, err := filelock.TryAcquireFileLock(session.HostLockPath(dataDir, "foreign-live-host"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreignLock.Release() })
	_, err = q.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{ID: "foreign-live-host", Pid: 4242, Label: "other-rush", StartedAt: 1700000000})
	require.NoError(t, err)
	require.NoError(t, claimForeignRunning(ctx, q, sess.ID, "foreign-call-1", "foreign-live-host"))

	// Foreign DEAD host: lock file exists on disk but nobody holds it.
	deadSeed, err := filelock.TryAcquireFileLock(session.HostLockPath(dataDir, "foreign-dead-host"))
	require.NoError(t, err)
	require.NoError(t, deadSeed.Release())
	require.NoError(t, claimForeignRunning(ctx, q, sess.ID, "foreign-call-2", "foreign-dead-host"))

	// Detach + keep the seed store's OWN host lock alive across Shutdown
	// (mirrors sessions_list_test.go's pattern): Shutdown would otherwise
	// Close() it, releasing the "own-call-1" row's host lock.
	a.SetAsyncJobStoreForTest(nil)
	defer func() { _ = store.Close(context.Background()) }()
	a.Shutdown()

	stdout := runJobsCmd(t, sess.ID)
	t.Logf("sessions jobs:\n%s", stdout)

	require.Contains(t, stdout, "own-call-1")
	require.Contains(t, stdout, "foreign-call-1")
	require.Contains(t, stdout, "foreign-call-2")

	lines := strings.Split(stdout, "\n")
	var ownLine, liveLine, deadLine string
	for _, l := range lines {
		switch {
		case strings.Contains(l, "own-call-1"):
			ownLine = l
		case strings.Contains(l, "foreign-call-1"):
			liveLine = l
		case strings.Contains(l, "foreign-call-2"):
			deadLine = l
		}
	}
	require.Contains(t, ownLine, "alive", "this process's own row must show alive")
	require.Contains(t, liveLine, "alive", "a genuinely held foreign lock must show alive")
	require.Contains(t, deadLine, "dead", "a released foreign lock must show dead")

	// The foreign LIVE row's hint (on the line right after it) must name its
	// PID and the sessions-kill escape hatch.
	hintIdx := -1
	for i, l := range lines {
		if strings.Contains(l, "foreign-call-1") {
			hintIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, hintIdx, 0)
	require.Contains(t, lines[hintIdx+1], "4242", "the hint must name the foreign host's PID")
	require.Contains(t, lines[hintIdx+1], killCommandFor(4242), "the hint must name the command that stops the host process")
	require.Contains(t, lines[hintIdx+1], "whole process", "the hint must say the whole host stops, not just the job")
	require.NotContains(t, lines[hintIdx+1], "sessions kill", "`rush sessions kill` kills the session-lock holder and nothing holds it between turns, so it must not be suggested")

	// The dead row must NOT get a hint line.
	for i, l := range lines {
		if strings.Contains(l, "foreign-call-2") {
			require.NotContains(t, lines[i+1], killCommandFor(4242), "a dead row must never print the live-host hint")
			require.NotContains(t, lines[i+1], "owned by a live host", "a dead row must never print the live-host hint")
		}
	}
}

// TestSessionsJobsCmdRun_JSON covers --json: one object per line, and the
// derived liveness/hint fields.
func TestSessionsJobsCmdRun_JSON(t *testing.T) {
	batchOn := config.BatchRuntimeEnabled()
	a, dataDir := isolatedJobsEnv(t)
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "jobs-json-sess", "session with jobs")
	require.NoError(t, err)

	if batchOn {
		f := seedBatchReadRoot(t, a, sess.ID)
		require.NoError(t, sessionsJobsCmd.Flags().Set("json", "true"))
		t.Cleanup(func() { _ = sessionsJobsCmd.Flags().Set("json", "false") })
		a.Shutdown()
		holdSeedHost(t, dataDir, f.host)
		stdout := runJobsCmd(t, sess.ID)
		var item jobsJSONItem
		require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &item), stdout)
		require.Equal(t, f.rootID, item.BatchRootID)
		require.Equal(t, "batch-read-call", item.ToolCallID)
		require.Equal(t, "batch", item.Kind)
		require.Equal(t, "pending", item.State)
		require.True(t, item.BatchHeld)
		require.Equal(t, "held", item.Liveness)
		require.Empty(t, item.Hint)
		require.Zero(t, item.HostPID)
		return
	}
	q := db.New(a.DB())
	foreignLock, err := filelock.TryAcquireFileLock(session.HostLockPath(dataDir, "json-live-host"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreignLock.Release() })
	_, err = q.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{ID: "json-live-host", Pid: 9911, Label: "peer", StartedAt: 1700000000})
	require.NoError(t, err)
	require.NoError(t, claimForeignRunning(ctx, q, sess.ID, "json-call-1", "json-live-host"))

	a.Shutdown()

	require.NoError(t, sessionsJobsCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsJobsCmd.Flags().Set("json", "false") })
	stdout := runJobsCmd(t, sess.ID)

	var item jobsJSONItem
	found := false
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		var it jobsJSONItem
		require.NoError(t, json.Unmarshal([]byte(line), &it))
		if it.ToolCallID == "json-call-1" {
			item = it
			found = true
		}
	}
	require.True(t, found, "the seeded job must appear in --json output")
	require.Equal(t, "alive", item.Liveness)
	require.EqualValues(t, 9911, item.HostPID)
	require.Equal(t, "peer", item.HostLabel)
	require.Contains(t, item.Hint, killCommandFor(9911))
	require.NotContains(t, item.Hint, "sessions kill")
}

func TestSessionsJobsCmdRun_JSONDeadHostDeletesUnannouncedSubtree(t *testing.T) {
	if !config.BatchRuntimeEnabled() {
		t.Skip("batch runtime is disabled")
	}
	a, dataDir := isolatedJobsEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "jobs-json-dead-sess", "dead batch host")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, a, sess.ID)
	var announced int
	require.NoError(t, a.DB().QueryRowContext(ctx, `SELECT announced FROM batch_nodes WHERE node_id = ?`, f.rootID).Scan(&announced))
	require.Zero(t, announced)
	var beforeCount int
	require.NoError(t, a.DB().QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE root_id = ?`, f.rootID).Scan(&beforeCount))
	require.Equal(t, 3, beforeCount)
	a.Shutdown()
	status, probeErr := session.ProbeHostShared(dataDir, f.host)
	require.Equal(t, session.HostStatusDead, status)
	require.NoError(t, probeErr)
	require.NoError(t, sessionsJobsCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsJobsCmd.Flags().Set("json", "false") })
	stdout := runJobsCmd(t, sess.ID)
	require.Empty(t, stdout)
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.ReleaseConn(conn)) }()
	var afterCount int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE root_id = ?`, f.rootID).Scan(&afterCount))
	require.Zero(t, afterCount)
}

// TestSessionsJobsCmdRun_NoJobs covers the empty case.
func TestSessionsJobsCmdRun_NoJobs(t *testing.T) {
	a, _ := isolatedJobsEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "jobs-empty-sess", "no jobs here")
	require.NoError(t, err)
	a.Shutdown()

	stdout := runJobsCmd(t, sess.ID)
	require.Contains(t, stdout, "no jobs")
}

// killCommandFor is the literal process-kill command the hint must carry for
// pid on this OS -- an independent oracle, not a call into the production
// helper.
func killCommandFor(pid int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("taskkill /F /T /PID %d", pid)
	}
	return fmt.Sprintf("kill -INT %d", pid)
}

// TestForeignHostKillHintFor_PerPlatformCommand pins both platform forms on
// any OS (R2C-9): POSIX must be `kill -INT` -- rush catches os.Interrupt, not
// SIGTERM, and job children live in their own process groups, so a plain
// `kill <pid>` ends the host with no cleanup and leaves them running -- and
// both forms must say the whole host stops. Windows keeps the forced
// process-tree kill.
func TestForeignHostKillHintFor_PerPlatformCommand(t *testing.T) {
	posix := foreignHostKillHintFor("linux", 4242)
	require.Contains(t, posix, "`kill -INT 4242`")
	require.NotContains(t, posix, "`kill 4242`", "a plain SIGTERM kill is not caught by rush and leaves job children behind")
	require.Contains(t, posix, "whole process")
	require.Contains(t, posix, "graceful")

	win := foreignHostKillHintFor("windows", 4242)
	require.Contains(t, win, "`taskkill /F /T /PID 4242`")
	require.Contains(t, win, "whole process")
	require.Contains(t, win, "forced")

	unknown := foreignHostKillHintFor("linux", 0)
	require.Contains(t, unknown, "PID unknown")
	require.Contains(t, unknown, "whole process")
}
