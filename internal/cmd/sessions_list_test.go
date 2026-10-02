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
// for task #233 finding 1: the status computation (backing `sessions list`'s
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

// NOTE (R-ACT-2): the two TestComputeSessionStatuses_* tests were rewritten
// onto the LockFact enum -- the mtime/PID-fallback machinery they exercised
// is gone (D10: ONE lock reader). CONSCIOUSLY CHANGED EXPECTATION: the
// #241 MaxPidFallbackAge bound no longer exists. A lock whose recorded PID
// is alive reads held ("running") whatever the lock's age; the bound
// protected against OS PID reuse pinning a killed run's lock as "running"
// forever, and that risk is now accepted by the architect's enum decision
// (release wipes the PID, so a recorded PID means "never reached release").
// The dead-PID half of the old behavior is pinned by
// TestSessionLockFact_DeadPIDIsDead (sessions_inject_test.go).
func TestSessionLockFact_AgedLockWithLivePIDIsHeld(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "list-status-pid-reuse", "regression title")
	require.NoError(t, err)

	holder := spawnKillTestLockHolder(t, dataDir, sess.ID, false)
	defer holder.stop(t)
	require.True(t, session.IsProcessAlive(holder.pid), "helper process must be alive for this test to be meaningful")

	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	staleTime := time.Now().Add(-(session.MaxPidFallbackAge + 5*time.Second))
	require.NoError(t, os.Chtimes(lockPath, staleTime, staleTime))

	require.Equal(t, session.LockHeld, session.InspectSessionLockFact(dataDir, sess.ID).Kind,
		"an alive recorded PID is held regardless of the lock's age (D10)")
}

// TestSessionsListCmdRun_LiveDescendantIsDelegatingNotDone is the regression
// test for the observed production bug: `rush sessions list` classified the
// parent/root session as done merely because its per-turn session lock was
// released (stale lock) — while an implementation sub-agent session was
// still working and the outer `rush run` process was still alive waiting on
// it.
//
// The fixture mirrors that sequence exactly:
//   - a parent session whose lock file is stale (names a dead PID, aged
//     past LockStaleDuration) and whose last assistant message finished
//     with end_turn — the shape the pre-classifier reclassification turned into "done";
//   - a REAL running async_jobs delegation row (owner=parent,
//     child_session_id=child), claimed via the seed App's own
//     AsyncJobStore -- step 7 replaced the session-lock-based descendant
//     walk with this durable-state one (child_session_id, not
//     parent_session_id).
//
// Pre-fix the parent's STATUS column read "done". With the cross-process
// descendant walk (the pre-classifier descendant walk →
// session.AsyncJobStore.LiveDescendantJobs) it must read "delegating"
// instead — and fall back to "done" once the delegation row is terminal.
func TestSessionsListCmdRun_LiveDescendantIsDelegatingNotDone(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)

	ctx := context.Background()
	parent, err := a.Sessions.CreateWithID(ctx, "list-desc-parent", "parent waiting on sub-agent")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "list-desc-child", parent.ID, "implementation sub-agent")
	require.NoError(t, err)

	// The parent's per-turn lock: stale (dead PID, aged heartbeat) with a
	// clean end_turn finish — the pre-classifier "done" shape.
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

	// A REAL running delegation row, claimed via the seed App's own
	// AsyncJobStore (built unconditionally by app.New -- see AsyncJobStore's
	// own doc) -- exactly like a sub-agent still working in another process.
	store := a.AsyncJobStore()
	require.NotNil(t, store, "app.New must build an AsyncJobStore for a data-dir'd App")
	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: parent.ID, ToolCallID: "delegate-1", Kind: session.JobKindAgent,
		Input: "delegate to " + child.ID, ChildSessionID: child.ID,
	})
	require.NoError(t, err)

	// Detach the store from the seed App BEFORE Shutdown: Shutdown's
	// releaseResources Close()s app.asyncJobStore, which releases the OS
	// host lock the delegation row's liveness depends on -- that would make
	// the row look dead before sessionsListCmd.RunE (a brand-new App on the
	// same data dir) ever gets to read it. Keep `store` itself alive (and
	// its host lock held) for the rest of this test via an explicit,
	// deferred Close.
	a.SetAsyncJobStoreForTest(nil)
	defer func() { _ = store.Close(context.Background()) }()

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

	// Phase 1: delegation row running (host alive) → parent must NOT read done.
	phase1 := runList()
	t.Logf("sessions list (delegation row running):\n%s", phase1)

	parentRow := statusRowFor(phase1, parent.ID[:8])
	require.NotEmpty(t, parentRow, "the parent session must be listed")
	require.Contains(t, parentRow, "delegating",
		"a parent with a live async_jobs delegation row must show delegating, never done — the observed bug")
	require.NotContains(t, parentRow, "done",
		"nothing may report done while a descendant session has live work")
	require.NotContains(t, parentRow, "crashed",
		"the stale-lock + clean-finish parent is not a crash; it is waiting on its sub-agent")
	// The sub-agent session itself is not a top-level row. Assert the CHILD's
	// full id: its 8-char prefix ("list-desc") is also a prefix of the
	// PARENT's id ("list-desc-parent"), so the parent row would trip a
	// prefix-scoped NotContains even though no child row leaked.
	require.NotContains(t, phase1, child.ID,
		"child sessions are filtered out of sessions list")

	// Phase 2: the delegation row reaches a terminal state → back to done.
	// This can't reuse `store`: a.Shutdown() plus runList()'s own
	// connect/release cycle already dropped the shared pool's refcount for
	// dataDir to zero, closing that *sql.DB handle (sqlite itself, being
	// file-backed, is unaffected on disk -- only the in-process pooled
	// handle dies). A fresh connection does the write; Transition needs no
	// host registration (unlike Claim), so no extra host lock is involved.
	freshConn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	writer := session.NewAsyncJobStore(freshConn, dataDir, 1001, "test-writer")
	_, err = writer.Transition(ctx, session.TransitionParams{
		Owner: parent.ID, ToolCallID: "delegate-1", State: "completed", NoticeKind: "completed", Wake: true,
	})
	require.NoError(t, err)

	phase2 := runList()
	t.Logf("sessions list (delegation row terminal):\n%s", phase2)
	parentRow = statusRowFor(phase2, parent.ID[:8])
	require.NotEmpty(t, parentRow)
	// CHANGED VERDICT (R-ACT-2, D3): with the delegation terminal the parent
	// has a recorded dead lock PID and no live work -- a crash, whatever the
	// end_turn finish of a previous turn. The old expectation ("done", via
	// the finish-based reclassification) is gone.
	require.Contains(t, parentRow, "crashed",
		"with no live descendant the recorded dead PID is the parent's own crash")
	require.NotContains(t, parentRow, "delegating",
		"delegating must be driven by a LIVE row, not by the mere existence of a past delegation")
}
