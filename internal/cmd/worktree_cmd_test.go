// Command-level tests for `rush worktree remove` and `rush worktree
// list` against real temporary git repositories. Each test names the
// product regression its Revert-check catches.
package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// wtRegisterCwdFlag registers a local "cwd" flag on the command, as the
// sessions tests do, and points it at mainRoot.
func wtRegisterCwdFlag(t *testing.T, cmd *cobra.Command, mainRoot string) {
	t.Helper()
	if cmd.Flags().Lookup("cwd") == nil {
		cmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, cmd.Flags().Set("cwd", mainRoot))
}

// wtAddWorktree creates the rush-managed worktree <main>/worktrees/<slug>
// on branch <slug>, as `rush run --worktree` would.
func wtAddWorktree(t *testing.T, main, slug string) string {
	t.Helper()
	wt := filepath.Join(main, worktreeDirName, slug)
	wtTestGitRun(t, main, "worktree", "add", "-b", slug, wt)
	return wt
}

// wtRunRemove invokes runWorktreeRemove with the given flags and returns
// its error and stdout.
func wtRunRemove(t *testing.T, mainRoot, slug string, flags map[string]string) (error, string) {
	t.Helper()
	wtRegisterCwdFlag(t, worktreeRemoveCmd, mainRoot)
	require.NoError(t, worktreeRemoveCmd.Flags().Set("keep-branch", "false"))
	require.NoError(t, worktreeRemoveCmd.Flags().Set("force", "false"))
	for name, value := range flags {
		require.NoError(t, worktreeRemoveCmd.Flags().Set(name, value))
	}
	var out bytes.Buffer
	worktreeRemoveCmd.SetOut(&out)
	worktreeRemoveCmd.SetErr(&out)
	err := runWorktreeRemove(worktreeRemoveCmd, []string{slug})
	return err, out.String()
}

// wtWorktreeRegistered reports whether `git worktree list` in main still
// names path.
func wtWorktreeRegistered(t *testing.T, main, path string) bool {
	t.Helper()
	out := wtTestGitRun(t, main, "worktree", "list", "--porcelain")
	return strings.Contains(out, path)
}

func TestWorktreeRemoveHappyPath(t *testing.T) {
	// Revert-check: skip the worktree removal or the branch deletion and
	// a removed run would leave stale worktrees and merged branches
	// behind in the main checkout.
	main := makeWorktreeTestRepo(t)
	wt := wtAddWorktree(t, main, "task-a")
	err, out := wtRunRemove(t, main, "task-a", nil)
	require.NoError(t, err)
	require.Contains(t, out, "removed worktree task-a")
	_, statErr := os.Stat(wt)
	require.True(t, os.IsNotExist(statErr), "worktree dir must be gone")
	require.False(t, wtWorktreeRegistered(t, main, wt))
	exists, err := localBranchExists(context.Background(), main, "task-a")
	require.NoError(t, err)
	require.False(t, exists, "merged branch must be deleted")
}

func TestWorktreeRemoveKeepBranch(t *testing.T) {
	// Revert-check: ignore --keep-branch and the branch survives flag
	// disappears — callers keeping a branch across runs lose it.
	main := makeWorktreeTestRepo(t)
	wt := wtAddWorktree(t, main, "task-a")
	err, out := wtRunRemove(t, main, "task-a",
		map[string]string{"keep-branch": "true"})
	require.NoError(t, err)
	require.Contains(t, out, "keeping branch task-a")
	_, statErr := os.Stat(wt)
	require.True(t, os.IsNotExist(statErr))
	exists, err := localBranchExists(context.Background(), main, "task-a")
	require.NoError(t, err)
	require.True(t, exists)
}

// wtCommitInWorktree commits one file inside the worktree so its branch
// diverges (unmerged) from the default branch while the tree stays clean.
func wtCommitInWorktree(t *testing.T, wt string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "extra.txt"), []byte("x\n"), 0o644))
	wtTestGitRun(t, wt, "add", ".")
	wtTestGitRun(t, wt, "commit", "-q", "-m", "extra")
}

func TestWorktreeRemoveUnmergedBranchKeptWithoutForce(t *testing.T) {
	// Revert-check: delete unmerged branches without --force (or fail
	// the command on git's -d refusal) and callers lose commits or get
	// spurious failures after a successful worktree removal.
	main := makeWorktreeTestRepo(t)
	wt := wtAddWorktree(t, main, "task-a")
	wtCommitInWorktree(t, wt)
	err, out := wtRunRemove(t, main, "task-a", nil)
	require.NoError(t, err)
	require.Contains(t, out, "removed worktree task-a")
	_, statErr := os.Stat(wt)
	require.True(t, os.IsNotExist(statErr))
	exists, err := localBranchExists(context.Background(), main, "task-a")
	require.NoError(t, err)
	require.True(t, exists, "unmerged branch must be kept without --force")
}

func TestWorktreeRemoveUnmergedBranchForceDeletes(t *testing.T) {
	// Revert-check: make --force skip the branch deletion and unmerged
	// commits survive a removal the caller explicitly forced.
	main := makeWorktreeTestRepo(t)
	wt := wtAddWorktree(t, main, "task-a")
	wtCommitInWorktree(t, wt)
	err, _ := wtRunRemove(t, main, "task-a", map[string]string{"force": "true"})
	require.NoError(t, err)
	exists, err := localBranchExists(context.Background(), main, "task-a")
	require.NoError(t, err)
	require.False(t, exists, "--force must delete the unmerged branch")
}

func TestWorktreeRemoveDirtyRefusedWithoutForce(t *testing.T) {
	// Revert-check: drop the dirty-tree refusal and uncommitted changes
	// are silently destroyed by a plain removal.
	main := makeWorktreeTestRepo(t)
	wt := wtAddWorktree(t, main, "task-a")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "untracked.txt"), []byte("x\n"), 0o644))
	err, _ := wtRunRemove(t, main, "task-a", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--force")
	require.True(t, wtWorktreeRegistered(t, main, wt))
}

func TestWorktreeRemoveDirtyForceSucceeds(t *testing.T) {
	// Revert-check: make --force still honor the dirty refusal and the
	// documented escape hatch stops working.
	main := makeWorktreeTestRepo(t)
	wt := wtAddWorktree(t, main, "task-a")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "untracked.txt"), []byte("x\n"), 0o644))
	err, _ := wtRunRemove(t, main, "task-a", map[string]string{"force": "true"})
	require.NoError(t, err)
	_, statErr := os.Stat(wt)
	require.True(t, os.IsNotExist(statErr))
}

func TestWorktreeRemoveLiveLockInMainDataDir(t *testing.T) {
	// Revert-check: drop the live-session refusal and removal yanks the
	// worktree out from under a running session, corrupting the run.
	main := makeWorktreeTestRepo(t)
	wtAddWorktree(t, main, "task-a")
	dataDir := filepath.Join(main, ".rush")
	lock, err := session.TryAcquireSessionLock(dataDir, "task-a")
	require.NoError(t, err)
	err, _ = wtRunRemove(t, main, "task-a", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "task-a")
	// --force must NOT bypass the live-run refusal.
	err, _ = wtRunRemove(t, main, "task-a", map[string]string{"force": "true"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "task-a")
	require.NoError(t, lock.Release())
	err, _ = wtRunRemove(t, main, "task-a", nil)
	require.NoError(t, err)
}

func TestWorktreeRemoveLiveLockInLocalStore(t *testing.T) {
	// Revert-check: skip the worktree-local lock store probing and a
	// legacy in-worktree run is removed while it still holds its lock.
	main := makeWorktreeTestRepo(t)
	wt := wtAddWorktree(t, main, "task-a")
	dataDir := filepath.Join(wt, ".rush")
	lock, err := session.TryAcquireSessionLock(dataDir, "live-one")
	require.NoError(t, err)
	err, _ = wtRunRemove(t, main, "task-a", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "live-one")
	require.NoError(t, lock.Release())
	err, _ = wtRunRemove(t, main, "task-a", nil)
	require.NoError(t, err)
}

func TestWorktreeRemoveUnknownSlug(t *testing.T) {
	// Revert-check: remove a slug with no registered worktree (e.g. the
	// empty-plan path) and the command reports success without doing
	// anything, or fails with the wrong message.
	main := makeWorktreeTestRepo(t)
	err, _ := wtRunRemove(t, main, "nope", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a rush-managed worktree")
}

func TestWorktreeRemoveInvalidSlug(t *testing.T) {
	// Revert-check: skip slug validation in the remove path and a path
	// traversal slug could resolve outside the worktrees directory.
	main := makeWorktreeTestRepo(t)
	err, _ := wtRunRemove(t, main, "../evil", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "slug")
}

func TestWorktreeListOutput(t *testing.T) {
	// Revert-check: break the list containment filter and unrelated
	// linked worktrees outside worktrees/ leak into the table; break
	// the empty case and stale output hides the "no worktrees" line.
	main := makeWorktreeTestRepo(t)
	wt := wtAddWorktree(t, main, "task-a")
	defer func() {
		_, statErr := os.Stat(wt)
		require.True(t, os.IsNotExist(statErr),
			"worktree dir must be gone after removal")
	}()

	// A linked worktree OUTSIDE worktrees/ must not be listed.
	other := filepath.Join(filepath.Dir(main), "other-wt")
	wtTestGitRun(t, main, "worktree", "add", "-b", "other", other)

	wtRegisterCwdFlag(t, worktreeListCmd, main)
	var out bytes.Buffer
	worktreeListCmd.SetOut(&out)
	worktreeListCmd.SetErr(&out)
	require.NoError(t, runWorktreeList(worktreeListCmd, nil))
	text := out.String()
	require.Contains(t, text, "task-a")
	require.Contains(t, text, "-")
	require.NotContains(t, text, filepath.Base(other))

	// After removal the list reports the empty case.
	err, _ := wtRunRemove(t, main, "task-a", nil)
	require.NoError(t, err)
	var out2 bytes.Buffer
	worktreeListCmd.SetOut(&out2)
	require.NoError(t, runWorktreeList(worktreeListCmd, nil))
	require.Contains(t, out2.String(), "no rush-managed worktrees")
}

func TestWorktreeListLiveSession(t *testing.T) {
	// Revert-check: break the SESSION column probing and an operator
	// removes a worktree believing no run is live.
	main := makeWorktreeTestRepo(t)
	wtAddWorktree(t, main, "task-a")
	dataDir := filepath.Join(main, ".rush")
	lock, err := session.TryAcquireSessionLock(dataDir, "task-a")
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Release()) }()
	wtRegisterCwdFlag(t, worktreeListCmd, main)
	var out bytes.Buffer
	worktreeListCmd.SetOut(&out)
	require.NoError(t, runWorktreeList(worktreeListCmd, nil))
	require.Contains(t, out.String(), "task-a")
}

func TestWorktreeSlugOf(t *testing.T) {
	// Revert-check: weaken the containment check in worktreeSlugOf and
	// `rush worktree list` shows foreign worktrees as rush-managed.
	main := makeWorktreeTestRepo(t)
	wt := wtAddWorktree(t, main, "task-a")
	slug, ok := worktreeSlugOf(main, wt)
	require.True(t, ok)
	require.Equal(t, "task-a", slug)
	_, ok = worktreeSlugOf(main, filepath.Join(filepath.Dir(main), "elsewhere"))
	require.False(t, ok)
	_, ok = worktreeSlugOf(main, main)
	require.False(t, ok)
}
