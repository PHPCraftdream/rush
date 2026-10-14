package cmd

// R12-B part 2 coverage: with RUSH_BATCH_RUNTIME on, the session action
// commands guard on batch facts (real SQLite fixtures seeded through
// session.BatchStore.Reserve/Project, never :memory:): reset refuses or wipes
// through the R10c batch reset, gc never collects a session with live or
// unknown batch work, compact refuses while batch work is live, and kill
// never reports a held-root session at rest. Flag off, behavior is unchanged.

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// seedActMessage puts one user message into the session so the reset tests
// can assert deletion (or refusal keeps it) by message count.
func seedActMessage(t *testing.T, a *app.App, owner, text string) {
	t.Helper()
	_, err := a.Messages.Create(context.Background(), owner, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
}

// countActMessages returns the session's message row count through a fresh
// connection (callers may have shut their seed app down).
func countActMessages(t *testing.T, dataDir, sessionID string) int {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	defer func() { _ = db.ReleaseConn(conn) }()
	var count int
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&count))
	return count
}

// resetBatchFixture stands up the reset command env with one seeded batch
// root (optionally projected to a state) and the seed app shut down, the way
// RunE needs it.
func resetBatchFixture(t *testing.T, id, state string) (string, batchReadFixture) {
	t.Helper()
	a, _ := isolatedResetEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, id, "batch reset fixture")
	require.NoError(t, err)
	seedActMessage(t, a, sess.ID, "history to wipe")
	f := seedBatchReadRoot(t, a, sess.ID)
	if state != "" {
		projectBatchRoot(t, f, state)
	}
	dataDir := a.Config().Options.DataDirectory
	a.Shutdown()
	return dataDir, f
}

// runBatchReset runs the real reset command expecting success, returning
// stderr.
func runBatchReset(t *testing.T, sessID string) string {
	t.Helper()
	require.NoError(t, resetSessionCmdFlags().Flags().Set("force", "false"))
	var runErr error
	stderr := captureStderr(t, func() {
		runErr = sessionsResetCmd.RunE(sessionsResetCmd, []string{sessID})
	})
	require.NoError(t, runErr)
	return stderr
}

// runBatchResetErr runs the real reset command expecting the given refusal.
func runBatchResetErr(t *testing.T, sessID string, fragments ...string) {
	t.Helper()
	require.NoError(t, resetSessionCmdFlags().Flags().Set("force", "false"))
	var runErr error
	captureStderr(t, func() {
		runErr = sessionsResetCmd.RunE(sessionsResetCmd, []string{sessID})
	})
	for _, want := range fragments {
		require.ErrorContains(t, runErr, want)
	}
}

// TestSessionsReset_BatchPendingRootRefusesFlagOn: with the flag on, a
// pending batch root refuses the reset; the refusal names the root, and
// nothing is deleted.
//
// Static trace: migrations create batch_nodes with the root CHECK (kind
// ='batch', tool_call_id, input_hash NOT NULL) and FKs ON -- satisfied by
// Reserve, which seedBatchReadRoot drives; ctx is non-nil (command context,
// Background inside the reader); the fixture's own registered host resolves
// as live like in the part-1 tests.
func TestSessionsReset_BatchPendingRootRefusesFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	dataDir, f := resetBatchFixture(t, "reset-batch-pending", "")

	runBatchResetErr(t, f.owner,
		"still being worked on",
		f.rootID, "the refusal must name the offending root",
		"pending",
		"nothing was reset")
	require.Equal(t, 1, countActMessages(t, dataDir, f.owner), "a refused reset deletes nothing")
}

// TestSessionsReset_BatchCancellingRootRefusesFlagOn: cancelling counts as
// held, nonterminal work, surfaced as "stopped but awaiting termination".
func TestSessionsReset_BatchCancellingRootRefusesFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	dataDir, f := resetBatchFixture(t, "reset-batch-cancelling", "cancelling")

	runBatchResetErr(t, f.owner,
		"still being worked on",
		"stopped but awaiting termination",
		"nothing was reset")
	require.Equal(t, 1, countActMessages(t, dataDir, f.owner), "a refused reset deletes nothing")
}

// TestSessionsReset_BatchTerminalRootResetsFlagOn: with an all-terminal
// root, the R10c batch reset runs and reports its outcome; the legacy reset
// line survives.
func TestSessionsReset_BatchTerminalRootResetsFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	dataDir, f := resetBatchFixture(t, "reset-batch-done", "completed")

	stderr := runBatchReset(t, f.owner)
	require.Contains(t, stderr, "reset session "+f.owner)
	require.Contains(t, stderr, "batch reset: deleted 1 message(s)")
	require.Contains(t, stderr, "2 batch node(s)", "root plus one task child")
	require.Equal(t, 0, countActMessages(t, dataDir, f.owner), "the wipe removed the history")
}

// TestSessionsReset_BatchPendingRootFlagOffRegression: with the flag unset,
// the same pending-root fixture behaves exactly as today -- the legacy reset
// ignores batch rows and wipes the history.
func TestSessionsReset_BatchPendingRootFlagOffRegression(t *testing.T) {
	require.False(t, batchRuntimeFlagOnForTest(), "precondition: flag must be unset")
	dataDir, f := resetBatchFixture(t, "reset-batch-flagoff", "")

	stderr := runBatchReset(t, f.owner)
	require.Contains(t, stderr, "reset session "+f.owner)
	require.NotContains(t, stderr, "batch reset:")
	require.Equal(t, 0, countActMessages(t, dataDir, f.owner), "legacy reset wipes the history")
}

// seedGcBatchSession creates an old empty session with a pending batch root
// on the given app and returns its fixture.
func seedGcBatchSession(t *testing.T, a *app.App, id string) batchReadFixture {
	t.Helper()
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, id, "gc batch fixture")
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx,
		`UPDATE sessions SET created_at = ? WHERE id = ?`,
		time.Now().Add(-8*24*time.Hour).Unix(), sess.ID)
	require.NoError(t, err)
	return seedBatchReadRoot(t, a, sess.ID)
}

// runBatchGc runs the real gc command with the given flags, returning
// (stdout, stderr).
func runBatchGc(t *testing.T, dryRun bool) (string, string) {
	t.Helper()
	require.NoError(t, sessionsGcCmd.Flags().Set("dry-run", boolStr(dryRun)))
	require.NoError(t, sessionsGcCmd.Flags().Set("jobs-older-than", ""))
	require.NoError(t, sessionsGcCmd.Flags().Set("json", "false"))
	var err error
	stdout, stderr := captureStdoutAndStderr(t, func() {
		err = sessionsGcCmd.RunE(sessionsGcCmd, nil)
	})
	require.NoError(t, err)
	return stdout, stderr
}

// TestSessionsGc_BatchHeldRootProtectsSessionFlagOn: with the flag on, gc
// refuses to collect a session whose batch root is pending; dry-run reports
// the keep, the real run purges nothing.
//
// Static trace: same Reserve-satisfied batch_nodes CHECK/FKs and live own
// host as the reset fixture; ctx non-nil throughout.
func TestSessionsGc_BatchHeldRootProtectsSessionFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	a, shutdownSeed := isolatedGcEnv(t)
	f := seedGcBatchSession(t, a, "gc-batch-protected")
	dataDir := a.Config().Options.DataDirectory
	shutdownSeed()

	_, stderr := runBatchGc(t, true)
	require.Contains(t, stderr, "kept session", "dry run must report the protected keep")
	_, stderr = runBatchGc(t, false)
	require.Contains(t, stderr, "kept session", "the real run keeps it too")

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	defer func() { _ = db.ReleaseConn(conn) }()
	var count int
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE id = ?`, f.owner).Scan(&count))
	require.Equal(t, 1, count, "the session with a held batch root was never collected")
	require.Equal(t, 0, countActMessages(t, dataDir, f.owner), "and its data is untouched")
}

// TestSessionsGc_BatchRetentionSummaryFlagOn: with the flag on, the
// --jobs-older-than run reports the batch-side numbers through the R10c
// retention API as an additive JSON line.
func TestSessionsGc_BatchRetentionSummaryFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	_, shutdownSeed := isolatedGcEnv(t)
	shutdownSeed()

	require.NoError(t, sessionsGcCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsGcCmd.Flags().Set("jobs-older-than", "1d"))
	require.NoError(t, sessionsGcCmd.Flags().Set("json", "true"))
	stdout, _ := captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsGcCmd.RunE(sessionsGcCmd, nil))
	})
	lines := splitNonEmpty(stdout)
	require.Len(t, lines, 2, "legacy async_jobs summary line plus the additive batch line: %q", stdout)
	require.Contains(t, lines[0], `"kind":"async_jobs"`)
	require.Contains(t, lines[1], `"kind":"batch_retention"`)
	require.Contains(t, lines[1], `"batch_nodes":0`, "a fresh database has no expired batch roots")
}

// TestSessionsGc_BatchHeldRootFlagOffRegression: with the flag unset, the
// same pending-root fixture is collected exactly as today.
func TestSessionsGc_BatchHeldRootFlagOffRegression(t *testing.T) {
	require.False(t, batchRuntimeFlagOnForTest(), "precondition: flag must be unset")
	a, shutdownSeed := isolatedGcEnv(t)
	f := seedGcBatchSession(t, a, "gc-batch-flagoff")
	dataDir := a.Config().Options.DataDirectory
	shutdownSeed()

	_, stderr := runBatchGc(t, false)
	require.Contains(t, stderr, "deleted 1 session(s)")

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	defer func() { _ = db.ReleaseConn(conn) }()
	var count int
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE id = ?`, f.owner).Scan(&count))
	require.Zero(t, count, "flag-off gc collects the session exactly as before")
}

// TestSessionsCompact_BatchLiveRootRefusesFlagOn: with the flag on, a
// database holding a live batch root refuses compaction even with --force;
// once the root is terminal the compaction proceeds.
//
// Static trace: same Reserve-satisfied root CHECK/FKs; host probing resolves
// the fixture's own live host; the terminal projection makes every root
// state batchRootHeld=false, clearing the gate.
func TestSessionsCompact_BatchLiveRootRefusesFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	_, built, run := isolatedCompactEnv(t)
	ctx := context.Background()
	sess, err := built.Sessions.CreateWithID(ctx, "compact-batch-sess", "compact batch fixture")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, built, sess.ID)
	bloatCompactDBConn(t, built.DB(), 2<<20)

	_, stderr, runErr := run(t, "--force")
	require.ErrorContains(t, runErr, "refusing to compact")
	require.Contains(t, stderr, "batch work")
	require.Contains(t, stderr, f.rootID[:8], "the refusal names the offending root")

	projectBatchRoot(t, f, "completed")
	_, _, runErr = run(t)
	require.NoError(t, runErr, "a terminal batch root clears the gate")
}

// TestSessionsCompact_BatchLiveRootFlagOffRegression: with the flag unset,
// the same live-root database compacts exactly as today (no batch gate).
func TestSessionsCompact_BatchLiveRootFlagOffRegression(t *testing.T) {
	require.False(t, batchRuntimeFlagOnForTest(), "precondition: flag must be unset")
	_, built, run := isolatedCompactEnv(t)
	ctx := context.Background()
	sess, err := built.Sessions.CreateWithID(ctx, "compact-batch-off", "compact batch fixture")
	require.NoError(t, err)
	seedBatchReadRoot(t, built, sess.ID)
	bloatCompactDBConn(t, built.DB(), 2<<20)

	stdout, stderr, runErr := run(t)
	require.NoError(t, runErr)
	require.Contains(t, stdout+stderr, "reclaimed", "flag-off compaction runs despite the live batch root")
}

// TestSessionsKill_BatchHeldRootNoteFlagOn: with the flag on, killing a
// session whose batch root is pending must not read as "nothing running" --
// an additive note names the live batch work even when no lock file exists.
//
// Static trace: the fixture's migrations create batch_nodes with FKs ON and
// the root CHECK (kind='batch', tool_call_id, input_hash) which Reserve
// satisfies; ctx is Background (non-nil); the host lock is held by this live
// test process, so host probing resolves it like the part-1 EnsureHost
// tests.
func TestSessionsKill_BatchHeldRootNoteFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	dataDir, sessID, closeFixture := seedKillBatchFixture(t)
	defer closeFixture()
	isolateKillCmdEnv(t, dataDir)

	var stderr string
	captureStderr(t, func() {
		require.NoError(t, sessionsKillCmd.RunE(sessionsKillCmd, []string{sessID}))
	})
	require.Contains(t, stderr, "no lock file at", "the legacy branch output is unchanged")
	require.Contains(t, stderr, "batch work for session "+sessID, "the note must report the held batch work")
	require.Contains(t, stderr, "pending", "and the root's state")
}

// TestSessionsKill_BatchHeldRootFlagOffRegression: with the flag unset, the
// same fixture produces today's output exactly -- no batch note.
func TestSessionsKill_BatchHeldRootFlagOffRegression(t *testing.T) {
	require.False(t, batchRuntimeFlagOnForTest(), "precondition: flag must be unset")
	dataDir, sessID, closeFixture := seedKillBatchFixture(t)
	defer closeFixture()
	isolateKillCmdEnv(t, dataDir)

	var stderr string
	captureStderr(t, func() {
		require.NoError(t, sessionsKillCmd.RunE(sessionsKillCmd, []string{sessID}))
	})
	require.Contains(t, stderr, "no lock file at")
	require.NotContains(t, stderr, "batch work", "flag off adds nothing")
}

// isolateKillCmdEnv points sessionsKillCmd at the fixture data dir in an
// isolated config env, mirroring TestSessionsKillCmdRun_HonorsConfiguredDataDir.
func isolateKillCmdEnv(t *testing.T, dataDir string) {
	t.Helper()
	isolateConfigEnvForTests(t)
	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })
	ensureRootFlagStandIns(sessionsKillCmd, dataDir)
	if f := sessionsKillCmd.Flags().Lookup("cwd"); f == nil {
		sessionsKillCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsKillCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsKillCmd.Flags().Set("keep-lock", "false"))
	require.NoError(t, sessionsKillCmd.Flags().Set("wait", "2s"))
	sessionsKillCmd.SetContext(context.Background())
}

// seedKillBatchFixture builds a standalone data directory (no app) whose
// database holds a session with a pending batch root hosted by this live
// test process, and returns its path, the session id, and a closer.
func seedKillBatchFixture(t *testing.T) (string, string, func()) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	store := session.NewAsyncJobStore(conn, dir, os.Getpid(), "kill-batch-fixture")
	_, err = db.New(conn).CreateSession(ctx, db.CreateSessionParams{ID: "kill-batch-sess", Title: "kill batch fixture", Origin: "cli"})
	require.NoError(t, err)
	host, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	seedKillBatchRoot(t, conn, host, "kill-batch-sess")
	closed := false
	return dir, "kill-batch-sess", func() {
		if closed {
			return
		}
		closed = true
		_ = store.Close(context.Background())
		_ = db.ReleaseConn(conn)
	}
}

// seedKillBatchRoot reserves one minimal pending batch root (kind=batch with
// tool call and input hash, no children) for owner on the given host.
func seedKillBatchRoot(t *testing.T, conn *sql.DB, host, owner string) {
	t.Helper()
	_, err := session.NewBatchStore(conn).Reserve(context.Background(), session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "kill-batch-runner",
		ToolCallID: "kill-batch-call", InputHash: "kill-batch-hash", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}},
	})
	require.NoError(t, err)
}

// splitNonEmpty splits command output into non-empty lines.
func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// boolStr is a tiny literal helper for flag setting.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
