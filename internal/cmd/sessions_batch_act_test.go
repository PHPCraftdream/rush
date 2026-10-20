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
	// The seed app's shutdown released its host lock, which makes the host
	// DEAD and its nonterminal roots recoverable: the reset recovers a dead
	// host's roots BEFORE its refusal. Hold the host lock for the test's
	// duration so the root stays work a LIVE host holds.
	hostLock, lockErr := session.TryAcquireFileLock(session.HostLockPath(dataDir, f.host))
	require.NoError(t, lockErr)
	t.Cleanup(func() { _ = hostLock.Release() })
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
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, dataDir)

	runBatchResetErr(t, f.owner,
		"still being worked on",
		f.rootID,
		"pending",
		"nothing was reset")
	require.Equal(t, 1, countActMessages(t, dataDir, f.owner), "a refused reset deletes nothing")
}

// TestSessionsReset_BatchCancellingRootRefusesFlagOn: cancelling counts as
// held, nonterminal work, surfaced as "stopped but awaiting termination".
func TestSessionsReset_BatchCancellingRootRefusesFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	dataDir, f := resetBatchFixture(t, "reset-batch-cancelling", "cancelling")
	dropLegacyAsyncJobs(t, dataDir)

	runBatchResetErr(t, f.owner,
		"still being worked on",
		"stopped but awaiting termination",
		"nothing was reset")
	require.Equal(t, 1, countActMessages(t, dataDir, f.owner), "a refused reset deletes nothing")
}

// TestSessionsReset_BatchTerminalRootResetsFlagOn: with an all-terminal
// root, the R10c batch reset runs and reports its outcome; the legacy reset
// line survives. NodesDeleted is BatchResetDeleteOwnerNodes's RowsAffected --
// the DIRECT DELETE's rows (root rows only; the two task children vanish by
// FK cascade and are not counted; precedent
// internal/session/batch_history_reset_test.go:149-152: two root rows ⇒ 2).
func TestSessionsReset_BatchTerminalRootResetsFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	dataDir, f := resetBatchFixture(t, "reset-batch-done", "completed")
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, dataDir)

	stderr := runBatchReset(t, f.owner)
	require.Contains(t, stderr, "reset session "+f.owner)
	require.Contains(t, stderr, "batch reset: deleted 1 message(s)")
	require.Contains(t, stderr, "1 batch node(s)", "RowsAffected counts the direct DELETE's root row only")
	require.Equal(t, 0, countActMessages(t, dataDir, f.owner), "the wipe removed the history")
}

// TestSessionsReset_BatchPendingRootFlagOffRegression: with the flag unset,
// the same pending-root fixture behaves exactly as today -- the legacy reset
// ignores batch rows and wipes the history.
func TestSessionsReset_BatchPendingRootFlagOffRegression(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "off")
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
	// The seed shutdown released its host lock, which makes the host DEAD and
	// its pending root recoverable by the gc app's startup recovery. Hold the
	// lock so the root stays work a LIVE host holds.
	hostLock, lockErr := session.TryAcquireFileLock(session.HostLockPath(dataDir, f.host))
	require.NoError(t, lockErr)
	t.Cleanup(func() { _ = hostLock.Release() })
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, dataDir)

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
	a, shutdownSeed := isolatedGcEnv(t)
	dataDir := a.Config().Options.DataDirectory
	shutdownSeed()
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, dataDir)

	require.NoError(t, sessionsGcCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsGcCmd.Flags().Set("jobs-older-than", "1d"))
	require.NoError(t, sessionsGcCmd.Flags().Set("json", "true"))
	stdout, _ := captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsGcCmd.RunE(sessionsGcCmd, nil))
	})
	lines := splitNonEmpty(stdout)
	require.Len(t, lines, 1, "flag ON emits only the batch_retention line: %q", stdout)
	require.Contains(t, lines[0], `"kind":"batch_retention"`)
	require.Contains(t, lines[0], `"batch_nodes":0`, "a fresh database has no expired batch roots")
	require.NotContains(t, stdout, `"kind":"async_jobs"`)
}

// TestSessionsGc_BatchRetentionHonoursAgeFlagOn: the flag-ON --jobs-older-than
// cutoff honours the caller-chosen age through BatchRetentionRoots. An 8-day-
// old debt-free terminal root is NOT expired at a 30d cutoff (this is what
// fails against the old fixed-7d behavior) and IS purged at a 1d cutoff.
//
// Static trace: db.Connect ran the real migrations; the fixture satisfies the
// batch_nodes CHECKs/FKs (Reserve). BatchRetentionRoots (batch_recovery.sql)
// qualifies the root: node_id = root_id, updated_at < cutoff (backdated to
// -8d), no pending control requests, no live/debt row in the root (all three
// rows completed, delivery 'none', wake 0), no delegation links, no
// child-session notices. The session is kept fresh so gc's old-empty rule
// cannot collect it (batch_nodes.owner_session_id cascades on session
// delete, which would silently remove the rows under test).
func TestSessionsGc_BatchRetentionHonoursAgeFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	a, shutdownSeed := isolatedGcEnv(t)
	ctx := context.Background()
	f := seedGcBatchSession(t, a, "gc-batch-age")
	dataDir := a.Config().Options.DataDirectory
	projectBatchRoot(t, f, "completed")
	_, err := a.DB().ExecContext(ctx,
		`UPDATE sessions SET created_at = ? WHERE id = ?`, time.Now().Unix(), f.owner)
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx,
		`UPDATE batch_nodes SET updated_at = ? WHERE root_id = ?`,
		time.Now().Add(-8*24*time.Hour).Unix(), f.rootID)
	require.NoError(t, err)
	shutdownSeed()
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, dataDir)

	// 30d cutoff: the 8-day-old rows are retained.
	require.NoError(t, sessionsGcCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsGcCmd.Flags().Set("jobs-older-than", "30d"))
	require.NoError(t, sessionsGcCmd.Flags().Set("json", "true"))
	stdout, _ := captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsGcCmd.RunE(sessionsGcCmd, nil))
	})
	lines := splitNonEmpty(stdout)
	require.Len(t, lines, 1, "only the batch_retention line is expected: %q", stdout)
	require.Contains(t, lines[0], `"kind":"batch_retention"`)
	require.Contains(t, lines[0], `"batch_nodes":0`, "8-day-old rows survive a 30d cutoff")
	require.Equal(t, 3, countBatchNodes(t, dataDir, f.rootID), "nothing was purged at the 30d cutoff")

	// 1d cutoff: the same rows are now expired; the real run purges the root.
	require.NoError(t, sessionsGcCmd.Flags().Set("jobs-older-than", "1d"))
	stdout, _ = captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsGcCmd.RunE(sessionsGcCmd, nil))
	})
	require.Contains(t, stdout, `"batch_nodes":1`, "the root count is purged at the 1d cutoff")
	require.Equal(t, 0, countBatchNodes(t, dataDir, f.rootID), "the rows are gone")
}

// TestSessionsGc_BatchHeldRootFlagOffRegression: with the flag unset, the
// same pending-root fixture is collected exactly as today.
func TestSessionsGc_BatchHeldRootFlagOffRegression(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "off")
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
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, built.Config().Options.DataDirectory)
	bloatCompactDBConn(t, built.DB(), 2<<20)

	// Offender strings render as "<session hash prefix> (batch root <short
	// root> (tool call ...) is pending)" inside the refusal error.
	_, stderr, runErr := run(t, "--force")
	require.ErrorContains(t, runErr, "refusing to compact")
	require.ErrorContains(t, runErr, "live or unknown batch work")
	require.ErrorContains(t, runErr, short(session.HashID(sess.ID)), "the refusal names the session's hash prefix")
	require.ErrorContains(t, runErr, f.rootID[:8], "the reason names the offending root's short id")
	require.NotContains(t, stderr, "batch gate: LIVE", "a real run refuses before printing gate lines")

	projectBatchRoot(t, f, "completed")
	// Release the fixture's host lock so the host gate cannot mask the
	// (now cleared) batch gate.
	if f.closeAsync != nil {
		f.closeAsync()
	}
	_, _, runErr = run(t, "--force")
	require.NoError(t, runErr, "a terminal batch root clears the gate")
}

// TestSessionsCompact_BatchLiveRootFlagOffRegression: with the flag unset,
// the same live-root database compacts exactly as today (no batch gate).
func TestSessionsCompact_BatchLiveRootFlagOffRegression(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "off")
	_, built, run := isolatedCompactEnv(t)
	ctx := context.Background()
	sess, err := built.Sessions.CreateWithID(ctx, "compact-batch-off", "compact batch fixture")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, built, sess.ID)
	// The seed's fallback host lock must not trip the host gate.
	if f.closeAsync != nil {
		f.closeAsync()
	}
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
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, dataDir)

	stderr := captureStderr(t, func() {
		require.NoError(t, sessionsKillCmd.RunE(sessionsKillCmd, []string{sessID}))
	})
	require.Contains(t, stderr, "no lock file at", "the legacy branch output is unchanged")
	require.Contains(t, stderr, "batch work for session "+sessID, "the note must report the held batch work")
	require.Contains(t, stderr, "pending", "and the root's state")
}

// TestSessionsKill_BatchHeldRootFlagOffRegression: with the flag unset, the
// same fixture produces today's output exactly -- no batch note.
func TestSessionsKill_BatchHeldRootFlagOffRegression(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "off")
	dataDir, sessID, closeFixture := seedKillBatchFixture(t)
	defer closeFixture()
	isolateKillCmdEnv(t, dataDir)

	stderr := captureStderr(t, func() {
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

// TestSessionsGc_TerminalRootDebtProtectsSessionFlagOn: terminal completion
// debt -- a completed root's selected, unreacted notice -- must protect the
// session from gc exactly like live work, and the protection is the debt,
// not the terminal root: once the notice stops being debt the session is
// collected.
//
// Static trace: the fixture satisfies the batch_nodes CHECKs (root kind=
// 'batch' with tool_call_id/input_hash; children opposite). The debt-seeding
// UPDATEs satisfy the raw CHECKs: the root gets announced=1 WITH
// announce_message_id NOT NULL; the task gets notice_selected=1 with
// notice_kind/state/cause NOT NULL, notice_sequence > 0, notice_ordinal >= 0
// and delivery='pending' (not 'none'), wake=1 -- exactly what
// BatchOutboxDebtNodes selects (it joins r.announced=1 and has NO state
// filter, so the TERMINAL root's node debt counts). NO message is seeded:
// the old-empty gc rule needs MessageCount == 0, so the session IS a gc
// candidate and the protection proof is the debt alone.
func TestSessionsGc_TerminalRootDebtProtectsSessionFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	a, shutdownSeed := isolatedGcEnv(t)
	ctx := context.Background()
	f := seedGcBatchSession(t, a, "gc-batch-debt")
	dataDir := a.Config().Options.DataDirectory
	projectBatchRoot(t, f, "completed")
	_, err := a.DB().ExecContext(ctx,
		`UPDATE batch_nodes SET announced = 1, announce_message_id = 'seed-msg' WHERE node_id = ? AND root_id = ?`,
		f.rootID, f.rootID)
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx,
		`UPDATE batch_nodes SET notice_selected = 1, notice_kind = 'task', notice_state = 'completed',
		 notice_cause = '', notice_sequence = 1, notice_ordinal = 0, delivery = 'pending', wake = 1
		 WHERE node_id = ? AND parent_id = ?`, f.nodeA, f.rootID)
	require.NoError(t, err)
	shutdownSeed()
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, dataDir)

	_, stderr := runBatchGc(t, true)
	require.Contains(t, stderr, "kept session", "dry run must report the debt keep")
	require.Contains(t, stderr, "batch delivery debt", "the keep names the debt, not the terminal root")
	_, stderr = runBatchGc(t, false)
	require.Contains(t, stderr, "kept session", "the real run keeps it too")
	require.Equal(t, 1, countSessions(t, dataDir, f.owner), "terminal completion debt protects from gc")
	// Protection means nothing was wiped: every batch_nodes row survives.
	require.Equal(t, 3, countBatchNodes(t, dataDir, f.rootID), "no batch row was deleted while protected")

	// Project the debt away (wake=0): the real run now collects the old,
	// empty session -- the guard was the debt.
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET wake = 0 WHERE node_id = ?`, f.nodeA)
	require.NoError(t, err)
	require.NoError(t, db.ReleaseConn(conn))

	_, stderr = runBatchGc(t, false)
	require.Contains(t, stderr, "deleted 1 session(s)", "without the debt the terminal root no longer protects")
	require.Equal(t, 0, countSessions(t, dataDir, f.owner), "the session was collected")
}

// countBatchNodes returns the batch_nodes row count for one root id.
func countBatchNodes(t *testing.T, dataDir, rootID string) int {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	defer func() { _ = db.ReleaseConn(conn) }()
	var count int
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM batch_nodes WHERE root_id = ?`, rootID).Scan(&count))
	return count
}

// countSessions returns the sessions-row count for one id.
func countSessions(t *testing.T, dataDir, sessionID string) int {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	defer func() { _ = db.ReleaseConn(conn) }()
	var count int
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE id = ?`, sessionID).Scan(&count))
	return count
}
