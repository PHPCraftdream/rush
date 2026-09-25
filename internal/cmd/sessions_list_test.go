package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolatedListEnvWithConfiguredDataDir mirrors
// isolatedResetEnvWithConfiguredDataDir (sessions_reset_test.go) but wires up
// sessionsListCmd instead: stands up a real app via setupApp against a data
// directory that is deliberately NOT <cwd>/.rush, so a fix that reads
// a.Config().Options.DataDirectory can be told apart from a pre-fix
// cwd-based guess.
func isolatedListEnvWithConfiguredDataDir(t *testing.T) (a *app.App, workDir, dataDir string) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)

	workDir = t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	// Deliberately outside workDir entirely, so filepath.Join(cwd, ".rush")
	// (the pre-fix hardcoded guess) can never accidentally coincide with it.
	dataDir = filepath.Join(tmp, "configured-elsewhere-data")

	ctx, cancel := context.WithCancel(context.Background())

	ensureRootFlagStandIns(sessionsListCmd, dataDir)
	if f := sessionsListCmd.Flags().Lookup("json"); f == nil {
		sessionsListCmd.Flags().Bool("json", false, "")
	}
	require.NoError(t, sessionsListCmd.Flags().Set("json", "false"))
	sessionsListCmd.SetContext(ctx)

	built, err := setupApp(sessionsListCmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, built.Config().Options.DataDirectory,
		"test setup assumption: resolved DataDirectory must equal the --data-dir we configured")

	t.Cleanup(func() {
		builtDataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		_ = os.Chdir(orig)
		cancel()
		_ = db.Release(builtDataDir)
		waitForSQLiteHandleRelease(t, builtDataDir)
		waitForSQLiteHandleRelease(t, workDir)
	})

	return built, workDir, dataDir
}

// TestSessionsListCmdRun_StatusHonorsConfiguredDataDir is the regression test
// for task #233 finding 1: computeSessionStatuses (backing `sessions list`'s
// STATUS column) used to compute locksDir as
// filepath.Join(ResolveCwd(cmd), ".rush", "locks"), completely ignoring
// --data-dir / a configured data_directory, even though sessionsListCmd's
// RunE had already resolved the correct value onto `a` via setupApp. This is
// the same bug class already fixed for `sessions kill` / `sessions reset
// --force` (task #219/#224) and `sessions locks` (task #231).
//
// This creates a real session, writes a lock file at the path the FIX
// computes (<configured-data-dir>/locks/session-<id>.lock) naming this test
// process's own live PID, runs the real sessionsListCmd.RunE, and asserts
// the STATUS column reports "running" for that session — not blank, which is
// what the pre-fix code would show since it was looking in the wrong,
// cwd-based directory that has no lock file at all.
func TestSessionsListCmdRun_StatusHonorsConfiguredDataDir(t *testing.T) {
	a, workDir, dataDir := isolatedListEnvWithConfiguredDataDir(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "list-status-configured-datadir", "regression title")
	require.NoError(t, err)

	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	// Sanity: the WRONG (pre-fix) path must not exist, so a blank STATUS
	// column can never be confused with a real find.
	wrongPath := filepath.Join(workDir, ".rush", "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	_, wrongStatErr := os.Stat(wrongPath)
	require.True(t, os.IsNotExist(wrongStatErr))

	// SessionsListCmd.RunE creates its own full App. Release the seed App's
	// process-wide MCP owner before invoking the command so the lifetimes do
	// not overlap.
	a.Shutdown()

	stdout := captureStdout(t, func() {
		runErr := sessionsListCmd.RunE(sessionsListCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("sessions list stdout:\n%s", stdout)

	require.Contains(t, stdout, sess.ID[:8],
		"listing must include the seeded session")

	// Find the row for our session and assert its STATUS column says
	// "running" (our own PID is alive) rather than being blank/dash, which
	// is what the pre-fix cwd-based lookup would produce since it never
	// finds the lock file seeded at the configured data dir.
	found := false
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, sess.ID[:8]) {
			continue
		}
		found = true
		require.Contains(t, line, "running",
			"STATUS column must show running — fix must look up the lock at the configured data dir, not a cwd-based guess")
	}
	require.True(t, found, "expected to find the seeded session's row in the table output")
}

// TestComputeSessionStatuses_PidReuseBeyondMaxFallbackAgeIsNotRunning is the
// regression test for task #241: computeSessionStatuses trusted a
// CONFIRMED-alive recorded PID unconditionally, with no bound on how old the
// lock file itself could be. A `rush run` killed with SIGKILL leaves its
// PID in the lock file without releasing; hours later the OS can recycle
// that exact PID number for a completely unrelated, currently-running
// process. Before this fix, `sessions list` would report that session's
// STATUS as "running" forever — the crash never self-heals to "crashed"/
// "done" the way task #235 already fixed for session.InspectSessionLock.
//
// A real second process (spawnKillTestLockHolder, the same cross-process
// harness sessions_kill_test.go uses) stands in for "the OS reused this
// exact PID number" — it is genuinely alive throughout, so this proves the
// AGE bound, not merely a dead-PID false negative. The lock file's mtime is
// back-dated past session.MaxPidFallbackAge to simulate a lock abandoned
// long enough ago that its recorded PID can no longer be trusted.
func TestComputeSessionStatuses_PidReuseBeyondMaxFallbackAgeIsNotRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "list-status-pid-reuse", "regression title")
	require.NoError(t, err)

	// reapInBackground=false: this test never kills the holder (it stays
	// alive throughout as a live-PID fixture and is only stopped in the
	// deferred cleanup), so there is no forceKillHolder/probeThenKillHolder
	// poll racing a zombie window here. See spawnKillTestLockHolder's doc
	// comment in sessions_kill_test.go for the cases that actually depend
	// on one mode or the other.
	holder := spawnKillTestLockHolder(t, dataDir, sess.ID, false)
	defer holder.stop()
	require.True(t, session.IsProcessAlive(holder.pid), "helper process must be alive for this test to be meaningful")

	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	staleTime := time.Now().Add(-(session.MaxPidFallbackAge + 5*time.Second))
	require.NoError(t, os.Chtimes(lockPath, staleTime, staleTime),
		"back-dating mtime past MaxPidFallbackAge to simulate a lock abandoned long enough ago that its recorded PID can no longer be trusted, even though it currently resolves to a live process")

	statuses := computeSessionStatuses(a)
	require.NotNil(t, statuses)
	assert.Equal(t, "crashed", statuses[sess.ID],
		"a lock older than MaxPidFallbackAge must not be reported running just because its recorded PID currently belongs to a live (but unrelated) process — this is the core #241 fix")
}

// TestComputeSessionStatuses_PidAliveWithinMaxFallbackAgeIsRunning is the
// non-regression companion: a live PID within MaxPidFallbackAge of the
// lock's mtime must still be reported "running", exactly like before this
// fix — the bound must only kick in once the lock is genuinely old.
func TestComputeSessionStatuses_PidAliveWithinMaxFallbackAgeIsRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "list-status-pid-fresh", "regression title")
	require.NoError(t, err)

	// reapInBackground=false: this test never kills the holder (it stays
	// alive throughout as a live-PID fixture and is only stopped in the
	// deferred cleanup), so there is no forceKillHolder/probeThenKillHolder
	// poll racing a zombie window here. See spawnKillTestLockHolder's doc
	// comment in sessions_kill_test.go for the cases that actually depend
	// on one mode or the other.
	holder := spawnKillTestLockHolder(t, dataDir, sess.ID, false)
	defer holder.stop()
	require.True(t, session.IsProcessAlive(holder.pid), "helper process must be alive for this test to be meaningful")

	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	justUnder := time.Now().Add(-(session.MaxPidFallbackAge - 2*time.Minute))
	require.NoError(t, os.Chtimes(lockPath, justUnder, justUnder))

	statuses := computeSessionStatuses(a)
	require.NotNil(t, statuses)
	assert.Equal(t, "running", statuses[sess.ID],
		"a live PID just under MaxPidFallbackAge must still be trusted as running")
}

// TestSessionsListCmdRun_LiveDescendantIsDelegatingNotDone is the regression
// test for the observed production bug: `rush sessions list` classified the
// parent/root session as done merely because its per-turn session lock was
// released (stale lock) — while an implementation sub-agent session held a
// LIVE lock and the outer `rush run` process was still alive waiting on
// that sub-agent.
//
// The fixture mirrors that sequence exactly:
//   - a parent session whose lock file is stale (names a dead PID, aged
//     past LockStaleDuration) and whose last assistant message finished
//     with end_turn — the shape reclassifyCrashedAsDone turns into "done";
//   - a child session (real parent_session_id linkage) holding a REAL
//     exclusive lock, acquired in-process via TryAcquireSessionLock and
//     released explicitly as soon as phase 1's output is captured (with a
//     deferred Release as a safety net).
//
// Pre-fix the parent's STATUS column read "done". With the cross-process
// descendant walk (markDelegatingLiveDescendants → session.LiveDescendants)
// it must read "delegating" instead — and fall back to "done" once the
// child's lock is gone.
func TestSessionsListCmdRun_LiveDescendantIsDelegatingNotDone(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)

	ctx := context.Background()
	parent, err := a.Sessions.CreateWithID(ctx, "list-desc-parent", "parent waiting on sub-agent")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "list-desc-child", parent.ID, "implementation sub-agent")
	require.NoError(t, err)

	// The parent's per-turn lock: stale (dead PID, aged heartbeat) with a
	// clean end_turn finish — the reclassifyCrashedAsDone "done" shape.
	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	parentLock := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(parent.ID)+".lock")
	require.NoError(t, os.WriteFile(parentLock, []byte("999999\n"), 0o644))
	stale := time.Now().Add(-(session.LockStaleDuration + 5*time.Second))
	require.NoError(t, os.Chtimes(parentLock, stale, stale))

	assistant, err := a.Messages.Create(ctx, parent.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "delegating to a sub-agent"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, assistant))

	// The child's lock is genuinely held (in-process) for the whole first
	// phase, exactly like a sub-agent still working in another process.
	childLock, err := session.TryAcquireSessionLock(dataDir, child.ID)
	require.NoError(t, err)
	// Safety net only: the explicit Release() right after phase 1's output is
	// captured below is what normally frees the lock, well before t.Cleanup's
	// waitForSQLiteHandleRelease runs. This defer covers the window between
	// acquisition and that explicit release — an assertion aborting the test
	// in between would otherwise strand the lock handle into cleanup, which
	// would then burn waitForSQLiteHandleRelease's full 30s budget on Windows.
	// SessionLock.Release is idempotent (sync.Once), so once the explicit call
	// has run this deferred one is a no-op.
	defer func() { _ = childLock.Release() }()

	// SessionsListCmd.RunE creates its own full App. Release the seed App's
	// process-wide MCP owner before invoking the command so the lifetimes
	// do not overlap.
	a.Shutdown()

	runList := func() string {
		stdout := captureStdout(t, func() {
			runErr := sessionsListCmd.RunE(sessionsListCmd, nil)
			require.NoError(t, runErr)
		})
		return stdout
	}
	// statusRowFor returns the table row for the given session ID prefix.
	statusRowFor := func(stdout, idPrefix string) string {
		for _, line := range strings.Split(stdout, "\n") {
			if strings.Contains(line, idPrefix) {
				return line
			}
		}
		return ""
	}

	// Phase 1: child lock live → parent must NOT read done.
	phase1 := runList()
	t.Logf("sessions list (child lock live):\n%s", phase1)

	// Release the child lock the moment phase 1's output exists: every
	// assertion below only inspects the captured string, so the OS lock is
	// no longer needed. Releasing here — not at function end — guarantees
	// the handle is closed before t.Cleanup's waitForSQLiteHandleRelease
	// runs, so cleanup never waits on the still-held lock file.
	require.NoError(t, childLock.Release())

	parentRow := statusRowFor(phase1, parent.ID[:8])
	require.NotEmpty(t, parentRow, "the parent session must be listed")
	require.Contains(t, parentRow, "delegating",
		"a parent with a live descendant lock must show delegating, never done — the observed bug")
	require.NotContains(t, parentRow, "done",
		"nothing may report done while a descendant session holds a live lock")
	require.NotContains(t, parentRow, "crashed",
		"the stale-lock + clean-finish parent is not a crash; it is waiting on its sub-agent")
	// The sub-agent session itself is not a top-level row. Assert the CHILD's
	// full id: its 8-char prefix ("list-desc") is also a prefix of the
	// PARENT's id ("list-desc-parent"), so the parent row would trip a
	// prefix-scoped NotContains even though no child row leaked.
	require.NotContains(t, phase1, child.ID,
		"child sessions are filtered out of sessions list")

	// Phase 2: child lock released (above) and aged out → back to done.
	releasedAgo := time.Now().Add(-(session.LockStaleDuration + 5*time.Second))
	require.NoError(t, os.Chtimes(session.SessionLockPath(dataDir, child.ID), releasedAgo, releasedAgo))

	phase2 := runList()
	t.Logf("sessions list (child lock released):\n%s", phase2)
	parentRow = statusRowFor(phase2, parent.ID[:8])
	require.NotEmpty(t, parentRow)
	require.Contains(t, parentRow, "done",
		"with no live descendant the clean-exit reclassification must apply again")
	require.NotContains(t, parentRow, "delegating",
		"delegating must be driven by live descendant work, not by the mere existence of a child row")
}
