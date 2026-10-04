package cmd

// WS-1 per-workspace queue lock (#1142 step C, P1 of
// docs/plans/2026-10-01-shared-data-dir.md): `queue run` now takes
// queue-<hash(ws)>.lock instead of one process-wide queue.lock, so two linked
// worktrees sharing one data directory no longer block each other's runners.
// A home process (workspaceRoot "") keeps the historical "queue.lock" name, so
// an existing installation's behaviour is unchanged.
//
// The ClaimPending/ReclaimRunning ownership filter these runners use is covered
// by internal/queue/queue_workspace_test.go (step C); this file pins only the
// lock-name derivation and the runtime "different workspaces do not conflict"
// fact the design doc calls out.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

// TestQueueLockName_HomeUnchangedWorkspacesDistinct: the name is "queue.lock"
// for a home process (existing installs unchanged), deterministic for the same
// root, and distinct across different roots (a short fixed-length hashed suffix,
// not the raw path).
//
// Revert-check: returning the constant "queue.lock" (the pre-WS-1 single-lock
// shape) makes nameA == nameB and nameA == "queue.lock"; require.Equal("queue.lock",
// nameA) at the home case still holds, but require.NotEqual(nameA, nameB) fails.
func TestQueueLockName_HomeUnchangedWorkspacesDistinct(t *testing.T) {
	require.Equal(t, "queue.lock", queueLockName(""), "a home process keeps the historical name")

	wsA := string(filepath.Separator) + filepath.Join("repos", "wt-a")
	wsB := string(filepath.Separator) + filepath.Join("repos", "wt-b")
	nameA, nameB := queueLockName(wsA), queueLockName(wsB)

	require.True(t, strings.HasPrefix(nameA, "queue-") && strings.HasSuffix(nameA, ".lock") && nameA != "queue.lock",
		"a linked worktree gets its own hashed lock file, not the shared queue.lock")
	require.Equal(t, nameA, queueLockName(wsA), "the name is deterministic for the same root")
	require.NotEqual(t, nameA, nameB, "different workspaces get different lock files")
	require.Less(t, len(nameA), 40, "the hashed suffix keeps the name short")
}

// TestQueueRun_PerWorkspaceLocksDoNotConflict: two `queue run` runners from two
// different workspaces over the SAME data directory each take their own lock and
// both hold it at the same time — a shared queue.lock would block the second
// until the first released.
//
// Revert-check: switching the lock path back to the one shared "queue.lock"
// (dropping queueLockName from queueRunCmd) makes pathA == pathB; the second
// acquireSpawnLock then blocks on the first's held lock, so the goroutine never
// signals and this test fails at "second per-workspace lock never acquired".
func TestQueueRun_PerWorkspaceLocksDoNotConflict(t *testing.T) {
	dataDir := t.TempDir()
	pathA := filepath.Join(dataDir, queueLockName("/ws/a"))
	pathB := filepath.Join(dataDir, queueLockName("/ws/b"))
	require.NotEqual(t, pathA, pathB, "precondition: different workspaces map to different lock files")

	releaseA, err := acquireSpawnLock(pathA)
	require.NoError(t, err)

	acquired := make(chan struct{})
	go func() {
		releaseB, aerr := acquireSpawnLock(pathB)
		if aerr == nil {
			releaseB()
		}
		close(acquired)
	}()

	select {
	case <-acquired:
		// The second workspace's lock was acquired while the first's was still
		// held: the per-workspace locks do not collide.
	case <-time.After(2 * time.Second):
		// releaseA's defer runs on return; the leaked goroutine is blocked in
		// the (failing) shared-lock case only, which is fine for a revert-check.
		releaseA()
		t.Fatal("second per-workspace lock never acquired while the first workspace's lock was held — the locks collided (a shared queue.lock?)")
	}

	releaseA()
}

// TestQueueRunCmd_UsesPerWorkspaceLockFileForLinkedWorktree: end-to-end proof
// that `queue run` in a linked worktree creates its OWN queue-<hash(root)>.lock
// in the data directory and never touches the home queue.lock. Exercises the
// real command (setupApp -> claim -> per-workspace lock), pinning the wiring
// between config.WorkspaceRoot and queueLockName in queue.go.
//
// Revert-check: reverting lockPath to filepath.Join(dataDir, "queue.lock") makes
// the queue.lock assertion ("a linked worktree must not touch the home
// queue.lock") fail, and the per-workspace Stat fail.
func TestQueueRunCmd_UsesPerWorkspaceLockFileForLinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := isolateConfigEnvForTests(t)

	origCwd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	// A real git repo, so config.WorkspaceRoot resolves a non-home root and the
	// command treats this cwd as a linked worktree.
	projectDir := filepath.Join(tmp, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	init := platform.Command(t.Context(), "git", "init", "-q")
	init.Dir = projectDir
	require.NoError(t, init.Run())

	dataDir := filepath.Clean(filepath.Join(tmp, "datadir"))

	// Seed one pending task via `queue add` (run from the linked worktree, so
	// Add stamps workspace_root with this checkout).
	ensureRootFlagStandIns(queueAddCmd, dataDir)
	if f := queueAddCmd.Flags().Lookup("cwd"); f == nil {
		queueAddCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, queueAddCmd.Flags().Set("cwd", projectDir))
	t.Cleanup(func() { _ = queueAddCmd.Flags().Set("cwd", "") })
	queueAddCmd.SetContext(context.Background())

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString("run in a linked worktree")
	require.NoError(t, err)
	require.NoError(t, w.Close())
	os.Stdin = r
	addErr := queueAddCmd.RunE(queueAddCmd, nil)
	os.Stdin = oldStdin
	require.NoError(t, addErr)

	// Spawned child "succeeds" with valid JSON, so the task is consumed.
	queueTaskExecOverride = func(args []string, cwd, prompt string) ([]byte, error) {
		out, _ := json.Marshal(map[string]any{"cost_usd": 0.0, "tokens": int64(0), "exit_reason": "stop"})
		return out, nil
	}
	t.Cleanup(func() { queueTaskExecOverride = nil })

	ensureRootFlagStandIns(queueRunCmd, dataDir)
	if f := queueRunCmd.Flags().Lookup("cwd"); f == nil {
		queueRunCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, queueRunCmd.Flags().Set("cwd", projectDir))
	t.Cleanup(func() { _ = queueRunCmd.Flags().Set("cwd", "") })
	for name, val := range map[string]string{"concurrent": "1", "stop-on-fail": "false", "max-tasks": "0"} {
		if f := queueRunCmd.Flags().Lookup(name); f == nil {
			switch name {
			case "concurrent":
				queueRunCmd.Flags().Int("concurrent", 0, "")
			case "stop-on-fail":
				queueRunCmd.Flags().Bool("stop-on-fail", false, "")
			default:
				queueRunCmd.Flags().Int("max-tasks", 0, "")
			}
		}
		require.NoError(t, queueRunCmd.Flags().Set(name, val))
	}
	queueRunCmd.SetContext(context.Background())

	stderr := captureStderr(t, func() {
		runErr := queueRunCmd.RunE(queueRunCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("queue run stderr:\n%s", stderr)
	require.Contains(t, stderr, "processed 1 task(s)",
		"the linked worktree's own runner must claim and process its own task")

	wsRoot := config.WorkspaceRoot(projectDir)
	require.NotEmpty(t, wsRoot, "a real git repo is not a home process")

	_, statErr := os.Stat(filepath.Join(dataDir, queueLockName(wsRoot)))
	require.NoError(t, statErr, "queue run must create the per-workspace lock file queue-<hash>.lock")
	_, legacyErr := os.Stat(filepath.Join(dataDir, "queue.lock"))
	require.True(t, os.IsNotExist(legacyErr), "a linked worktree must not use the home queue.lock")

	waitForSQLiteHandleRelease(t, dataDir)
}
