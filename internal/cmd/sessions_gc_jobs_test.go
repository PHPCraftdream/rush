package cmd

// Real-DB coverage for `sessions gc --jobs-older-than` (doc sec.5 step 7):
// purges terminal, delivered-or-voided async_jobs/session_notices rows
// older than the given age; a 'running' row (even on an unknown-liveness
// host) is never purged regardless of age; --dry-run counts without
// deleting.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// isolatedGcEnv mirrors isolatedJobsEnv but wires up sessionsGcCmd. The seed
// app must be shut down (shutdownSeed) before sessionsGcCmdRun runs its own
// setupApp against the same data dir -- the process-wide MCP owner can only
// be held by one App at a time.
func isolatedGcEnv(t *testing.T) (a *app.App, shutdownSeed func()) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)
	dataDir := filepath.Join(tmp, "gc-jobs-data")

	ctx, cancel := context.WithCancel(context.Background())
	ensureRootFlagStandIns(sessionsGcCmd, dataDir)
	sessionsGcCmd.SetContext(ctx)
	require.NoError(t, sessionsGcCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsGcCmd.Flags().Set("older-than", "7d"))
	require.NoError(t, sessionsGcCmd.Flags().Set("jobs-older-than", ""))
	require.NoError(t, sessionsGcCmd.Flags().Set("max-sessions", "0"))
	require.NoError(t, sessionsGcCmd.Flags().Set("json", "false"))

	built, err := setupApp(sessionsGcCmd)
	require.NoError(t, err)
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			builtDataDir := built.Config().Options.DataDirectory
			built.Shutdown()
			_ = db.Release(builtDataDir)
		})
	}
	t.Cleanup(func() {
		shutdown()
		cancel()
		waitForSQLiteHandleRelease(t, dataDir)
	})
	return built, shutdown
}

// backdateAsyncJob rewrites updated_at for one async_jobs row, simulating a
// job that reached its current state long ago -- Transition always stamps
// "now", so a deterministic retention-cutoff test needs a raw rewrite.
func backdateAsyncJob(t *testing.T, conn *sql.DB, owner, toolCallID string, at time.Time) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(),
		`UPDATE async_jobs SET updated_at = ? WHERE owner_session_id = ? AND tool_call_id = ?`,
		at.Unix(), owner, toolCallID)
	require.NoError(t, err)
}

// seedTerminalNotice inserts a session_notices row already in a terminal
// delivery state ('done' or 'void') at the given updated_at -- notices have
// no CAS-based terminal transition to reuse, so this writes the shape
// directly.
func seedTerminalNotice(t *testing.T, q *db.Queries, conn *sql.DB, owner, delivery string, at time.Time) int64 {
	t.Helper()
	row, err := q.InsertSessionNotice(context.Background(), db.InsertSessionNoticeParams{
		Owner: owner, Kind: "supervision", Text: "test notice", Wake: 1,
		CreatedAt: at.Unix(), UpdatedAt: at.Unix(),
	})
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(),
		`UPDATE session_notices SET delivery = ?, updated_at = ? WHERE id = ?`, delivery, at.Unix(), row.ID)
	require.NoError(t, err)
	return row.ID
}

func countAsyncJobRows(t *testing.T, conn *sql.DB, owner, toolCallID string) int {
	t.Helper()
	var count int
	row := conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ?`, owner, toolCallID)
	require.NoError(t, row.Scan(&count))
	return count
}

func countNoticeRows(t *testing.T, conn *sql.DB, id int64) int {
	t.Helper()
	var count int
	row := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM session_notices WHERE id = ?`, id)
	require.NoError(t, row.Scan(&count))
	return count
}

// TestSessionsGcCmdRun_JobsOlderThan_PurgesTerminalOnly is the doc sec.5
// step 7 retention rule: an old terminal, delivered row is purged; a
// recent terminal row survives; an old RUNNING row survives regardless of
// age, including one on a host whose liveness cannot be determined
// ("unknown" -- doc sec.6: an unknown-liveness host's row is never
// deleted). Covers async_jobs and its session_notices twin.
func TestSessionsGcCmdRun_JobsOlderThan_PurgesTerminalOnly(t *testing.T) {
	a, shutdownSeed := isolatedGcEnv(t)
	ctx := context.Background()
	dataDir := a.Config().Options.DataDirectory

	sess, err := a.Sessions.CreateWithID(ctx, "gc-jobs-sess", "gc jobs probe")
	require.NoError(t, err)

	store := a.AsyncJobStore()
	require.NotNil(t, store)
	conn := a.DB()
	q := db.New(conn)

	// Old terminal job: past the cutoff, delivered -- must be purged.
	_, err = store.Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: "old-terminal", Kind: session.JobKindCommand, Input: "a"})
	require.NoError(t, err)
	_, err = store.Transition(ctx, session.TransitionParams{Owner: sess.ID, ToolCallID: "old-terminal", State: "completed", NoticeKind: "completed", Wake: true, Delivery: "done"})
	require.NoError(t, err)

	// Recent terminal job: within the window -- must survive.
	_, err = store.Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: "recent-terminal", Kind: session.JobKindCommand, Input: "b"})
	require.NoError(t, err)
	_, err = store.Transition(ctx, session.TransitionParams{Owner: sess.ID, ToolCallID: "recent-terminal", State: "completed", NoticeKind: "completed", Wake: true, Delivery: "done"})
	require.NoError(t, err)

	// A5: purge/count exclude unreacted debt (wake=1, reacted=0) regardless
	// of age -- mark both rows above already reacted so THIS test's fixture
	// represents ordinary resolved history, not open debt (which has its own
	// dedicated test, TestSessionsGcCmdRun_JobsOlderThan_NeverPurgesUnreactedDebt).
	_, err = q.MarkAsyncJobsReactedForOwner(ctx, db.MarkAsyncJobsReactedForOwnerParams{UpdatedAt: time.Now().Unix(), OwnerSessionID: sess.ID})
	require.NoError(t, err)

	backdateAsyncJob(t, conn, sess.ID, "old-terminal", time.Now().Add(-10*24*time.Hour))

	// Old but STILL RUNNING job (own host, alive) -- must never be purged.
	_, err = store.Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: "old-running", Kind: session.JobKindCommand, Input: "c"})
	require.NoError(t, err)
	backdateAsyncJob(t, conn, sess.ID, "old-running", time.Now().Add(-30*24*time.Hour))

	// Old RUNNING job on an UNKNOWN-liveness host (a directory in place of
	// the lock file -- doc sec.6, DUR-5: unknown is never "dead") -- must
	// never be purged either; the purge predicate itself never even
	// consults host liveness, it excludes every 'running' row outright.
	unknownHostPath := session.HostLockPath(dataDir, "gc-unknown-host")
	require.NoError(t, os.MkdirAll(unknownHostPath, 0o755))
	_, err = q.RegisterAsyncHost(ctx, db.RegisterAsyncHostParams{ID: "gc-unknown-host", Pid: 1, Label: "x", StartedAt: 1})
	require.NoError(t, err)
	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: sess.ID, ToolCallID: "old-running-unknown-host", Kind: "command",
		InputHash: "h", HostID: "gc-unknown-host", CreatedAt: 1, UpdatedAt: 1,
	})
	require.NoError(t, err)

	// Old terminal ('done') and old pending ('void' twin coverage) notices.
	oldNoticeID := seedTerminalNotice(t, q, conn, sess.ID, "done", time.Now().Add(-10*24*time.Hour))
	// A5: already reacted, not open debt (wake=1/reacted=0 on a 'done' row IS
	// debt and must survive retention -- that has its own dedicated test,
	// TestSessionsGcCmdRun_JobsOlderThan_NeverPurgesUnreactedDebt).
	_, err = conn.ExecContext(ctx, `UPDATE session_notices SET reacted = 1 WHERE id = ?`, oldNoticeID)
	require.NoError(t, err)
	recentNoticeID := seedTerminalNotice(t, q, conn, sess.ID, "void", time.Now())

	shutdownSeed()

	require.NoError(t, sessionsGcCmd.Flags().Set("jobs-older-than", "7d"))
	require.NoError(t, sessionsGcCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsGcCmdRun(sessionsGcCmd, nil))

	verifyConn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	require.Equal(t, 0, countAsyncJobRows(t, verifyConn, sess.ID, "old-terminal"),
		"an old, delivered terminal job must be purged")
	require.Equal(t, 1, countAsyncJobRows(t, verifyConn, sess.ID, "recent-terminal"),
		"a terminal job still within the retention window must survive")
	require.Equal(t, 1, countAsyncJobRows(t, verifyConn, sess.ID, "old-running"),
		"a running job must never be purged regardless of age")
	require.Equal(t, 1, countAsyncJobRows(t, verifyConn, sess.ID, "old-running-unknown-host"),
		"a running job on an unknown-liveness host must never be purged")
	require.Equal(t, 0, countNoticeRows(t, verifyConn, oldNoticeID),
		"an old, delivered notice must be purged (the async_jobs twin)")
	require.Equal(t, 1, countNoticeRows(t, verifyConn, recentNoticeID),
		"a recent notice must survive")
}

// TestSessionsGcCmdRun_JobsOlderThan_DryRunCountsOnly proves --dry-run
// counts the purge candidate without deleting anything.
func TestSessionsGcCmdRun_JobsOlderThan_DryRunCountsOnly(t *testing.T) {
	a, shutdownSeed := isolatedGcEnv(t)
	ctx := context.Background()
	dataDir := a.Config().Options.DataDirectory

	sess, err := a.Sessions.CreateWithID(ctx, "gc-jobs-dryrun-sess", "gc dry-run probe")
	require.NoError(t, err)

	store := a.AsyncJobStore()
	require.NotNil(t, store)
	conn := a.DB()
	q := db.New(conn)

	_, err = store.Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: "dry-run-terminal", Kind: session.JobKindCommand, Input: "a"})
	require.NoError(t, err)
	_, err = store.Transition(ctx, session.TransitionParams{Owner: sess.ID, ToolCallID: "dry-run-terminal", State: "completed", NoticeKind: "completed", Wake: true, Delivery: "done"})
	require.NoError(t, err)
	// A5: already-reacted, not open debt -- see the sibling test above.
	_, err = q.MarkAsyncJobsReactedForOwner(ctx, db.MarkAsyncJobsReactedForOwnerParams{UpdatedAt: time.Now().Unix(), OwnerSessionID: sess.ID})
	require.NoError(t, err)
	backdateAsyncJob(t, conn, sess.ID, "dry-run-terminal", time.Now().Add(-10*24*time.Hour))

	shutdownSeed()

	require.NoError(t, sessionsGcCmd.Flags().Set("jobs-older-than", "7d"))
	require.NoError(t, sessionsGcCmd.Flags().Set("dry-run", "true"))
	// A-b test fix: the count message is printed to STDERR (sessionsGcCmdRun's
	// `fmt.Fprintf(os.Stderr, ...)`), not stdout -- the original test captured
	// stdout and discarded it (`_ = stdout`), never actually checking the
	// count was reported. Capture stderr and assert on it.
	stderr := captureStderr(t, func() {
		require.NoError(t, sessionsGcCmdRun(sessionsGcCmd, nil))
	})
	require.Contains(t, stderr, "would purge 1 async job row")

	verifyConn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	require.Equal(t, 1, countAsyncJobRows(t, verifyConn, sess.ID, "dry-run-terminal"),
		"--dry-run must count the purge candidate without deleting it")
}

// TestSessionsGcCmdRun_JobsOlderThan_NeverPurgesUnreactedDebt is A5's CLI-
// level proof (A-b test fix: the sibling PurgesTerminalOnly test above has no
// announced/wake=1/done/unreacted row at all): an old, delivered, ANNOUNCED
// row that is still unreacted debt (wake=1, reacted=0) must survive
// `sessions gc --jobs-older-than`, same as any still-running row.
func TestSessionsGcCmdRun_JobsOlderThan_NeverPurgesUnreactedDebt(t *testing.T) {
	a, shutdownSeed := isolatedGcEnv(t)
	ctx := context.Background()
	dataDir := a.Config().Options.DataDirectory

	sess, err := a.Sessions.CreateWithID(ctx, "gc-jobs-debt-sess", "gc debt probe")
	require.NoError(t, err)

	store := a.AsyncJobStore()
	require.NotNil(t, store)
	conn := a.DB()

	_, err = store.Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: "old-debt", Kind: session.JobKindCommand, Input: "a"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, sess.ID, "old-debt"))
	_, err = store.Transition(ctx, session.TransitionParams{Owner: sess.ID, ToolCallID: "old-debt", State: "completed", NoticeKind: "completed", Wake: true, Delivery: "done"})
	require.NoError(t, err)
	backdateAsyncJob(t, conn, sess.ID, "old-debt", time.Now().Add(-30*24*time.Hour))

	shutdownSeed()

	require.NoError(t, sessionsGcCmd.Flags().Set("jobs-older-than", "7d"))
	require.NoError(t, sessionsGcCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsGcCmdRun(sessionsGcCmd, nil))

	verifyConn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	require.Equal(t, 1, countAsyncJobRows(t, verifyConn, sess.ID, "old-debt"),
		"an old, announced, delivered-but-unreacted row must never be purged")
}
