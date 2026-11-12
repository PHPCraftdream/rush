// Tests for the `rush run --worktree` plumbing: slug validation, target
// containment, path comparison, plan decisions, and the git helpers
// against real temporary repositories. Each group names the mutant its
// revert-check kills.
package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

// wtTestGitRun runs one git command in dir and fails the test on error.
// Named distinctly from config's gitRun because package cmd may grow its
// own helpers.
func wtTestGitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := platform.Command(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, out)
	return strings.TrimSpace(string(out))
}

// makeWorktreeTestRepo creates a main checkout with one commit and
// returns its root (symlinks resolved, since Windows hands out 8.3 temp
// paths while git reports long ones).
func makeWorktreeTestRepo(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	root := filepath.Join(base, "main")
	require.NoError(t, os.MkdirAll(root, 0o755))
	wtTestGitRun(t, root, "init", "-q")
	wtTestGitRun(t, root, "config", "user.email", "test@example.com")
	wtTestGitRun(t, root, "config", "user.name", "test")
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("x\n"), 0o644))
	wtTestGitRun(t, root, "add", ".")
	wtTestGitRun(t, root, "commit", "-q", "-m", "init")
	return root
}

func TestValidateWorktreeSlug(t *testing.T) {
	t.Parallel()
	// Revert-check: weaken validateWorktreeSlug and separators or
	// reserved device names would reach the filesystem as paths.
	valid := []string{"task-a", "s46xx", "a1.b_c-d"}
	for _, slug := range valid {
		t.Run("valid/"+slug, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, validateWorktreeSlug(slug))
		})
	}
	invalid := []struct {
		slug    string
		mention string
	}{
		{"", "empty"},
		{"Task", "slug"},
		{".lead", "slug"},
		{"-lead", "slug"},
		{"_lead", "slug"},
		{"a/b", "slug"},
		{`a\b`, "slug"},
		{"a..b", ".."},
		{"a b", "slug"},
		{"con", "reserved"},
		{"AUX", "slug"},
		{"com1", "reserved"},
		{"nul.txt", "reserved"},
		{"a:b", "slug"},
		{"задача", "slug"},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.slug, func(t *testing.T) {
			t.Parallel()
			err := validateWorktreeSlug(tc.slug)
			require.Error(t, err)
			require.True(t,
				strings.Contains(err.Error(), tc.slug) ||
					strings.Contains(err.Error(), tc.mention),
				"error %q must mention the slug or the rule %q", err, tc.mention)
		})
	}
}

func TestWorktreeTargetPath(t *testing.T) {
	t.Parallel()
	// Revert-check: drop the filepath.Rel containment guard and a
	// crafted slug could place worktrees outside the main checkout.
	main := filepath.Join(string(filepath.Separator), "repo")
	got, err := worktreeTargetPath(main, "task-a")
	require.NoError(t, err)
	want := filepath.Join(main, worktreeDirName, "task-a")
	require.Equal(t, want, got)
	require.True(t, strings.HasPrefix(got,
		filepath.Join(main, worktreeDirName)+string(filepath.Separator)))

	_, err = worktreeTargetPath(main, "..")
	require.Error(t, err)

	_, err = worktreeTargetPath(main, "a/b")
	require.Error(t, err)
}

func TestSameWorktreePath(t *testing.T) {
	t.Parallel()
	// Revert-check: drop the Windows case fold or separator cleanup and
	// registered worktrees would be re-created instead of reused.
	require.True(t, sameWorktreePath("D:/x/y", "D:/x/y"))
	require.True(t, sameWorktreePath("D:/x/y", `D:\x\y`))
	require.True(t, sameWorktreePath("D:/x/y", "D:/x/./y"))
	mixedCase := sameWorktreePath("D:/x/y", "D:/x/Y")
	if runtime.GOOS == "windows" {
		require.True(t, mixedCase)
	} else {
		require.False(t, mixedCase)
	}
}

func TestPlanWorktree(t *testing.T) {
	t.Parallel()
	// Revert-check: swap the create/reuse/mismatch branches in
	// planWorktree and existing worktrees get clobbered or duplicated.
	entries := []worktreeEntry{{Path: "/main/worktrees/wt", Branch: "old"}}
	if runtime.GOOS == "windows" {
		entries[0].Path = `D:\main\worktrees\wt`
	}
	main := entries[0].Path[:strings.LastIndex(
		filepath.FromSlash(entries[0].Path), string(filepath.Separator)+worktreeDirName)]

	t.Run("create new branch", func(t *testing.T) {
		t.Parallel()
		plan, err := planWorktree(nil, main, "slug", "feat",
			func(string) bool { return false }, func(string) bool { return false })
		require.NoError(t, err)
		require.Equal(t, worktreePlanCreate, plan.Kind)
		require.Equal(t, "feat", plan.Branch)
	})

	t.Run("create existing branch", func(t *testing.T) {
		t.Parallel()
		plan, err := planWorktree(nil, main, "slug", "feat",
			func(string) bool { return false }, func(string) bool { return true })
		require.NoError(t, err)
		require.Equal(t, worktreePlanCreateExistingBranch, plan.Kind)
	})

	t.Run("reuse", func(t *testing.T) {
		t.Parallel()
		plan, err := planWorktree(entries, main, "wt", "old",
			func(string) bool { return true }, func(string) bool { return true })
		require.NoError(t, err)
		require.Equal(t, worktreePlanReuse, plan.Kind)
		require.Equal(t, entries[0], plan.Entry)
	})

	t.Run("branch mismatch error", func(t *testing.T) {
		t.Parallel()
		_, err := planWorktree(entries, main, "wt", "want",
			func(string) bool { return true }, func(string) bool { return false })
		require.Error(t, err)
		require.Contains(t, err.Error(), "old")
		require.Contains(t, err.Error(), "want")
		require.Contains(t, err.Error(), "--branch")
	})

	t.Run("detached error", func(t *testing.T) {
		t.Parallel()
		detached := []worktreeEntry{{Path: entries[0].Path, Detached: true}}
		_, err := planWorktree(detached, main, "wt", "want",
			func(string) bool { return true }, func(string) bool { return false })
		require.Error(t, err)
		require.Contains(t, err.Error(), "detached")
	})

	t.Run("path occupied error", func(t *testing.T) {
		t.Parallel()
		_, err := planWorktree(nil, main, "slug", "feat",
			func(string) bool { return true }, func(string) bool { return false })
		require.Error(t, err)
		require.Contains(t, err.Error(), "refusing to overwrite")
	})
}

func TestPlanWorktreeRemoval(t *testing.T) {
	t.Parallel()
	// Revert-check: drop the containment re-check in
	// planWorktreeRemoval and a poisoned registration could be deleted
	// outside <main>/worktrees.
	main := "/main"
	entries := []worktreeEntry{
		{Path: "/main/worktrees/wt", Branch: "feat"},
		{Path: "/main/worktrees/dt", Detached: true},
	}
	if runtime.GOOS == "windows" {
		main = `D:\main`
		entries[0].Path = `D:\main\worktrees\wt`
		entries[1].Path = `D:\main\worktrees\dt`
	}

	plan, err := planWorktreeRemoval(entries, main, "wt")
	require.NoError(t, err)
	require.Equal(t, "feat", plan.Branch)

	plan, err = planWorktreeRemoval(entries, main, "dt")
	require.NoError(t, err)
	require.Empty(t, plan.Branch)

	_, err = planWorktreeRemoval(entries, main, "missing")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a rush-managed worktree")
}

// TestListGitWorktreesAndMainRoot exercises the porcelain parser and the
// main-root resolution against a real repository, including a subdir and
// a linked worktree.
func TestListGitWorktreesAndMainRoot(t *testing.T) {
	// Revert-check: break the porcelain record parsing and the worktree
	// registry loses branch attribution or the main root.
	root := makeWorktreeTestRepo(t)
	ctx := context.Background()

	mainRoot, err := mainCheckoutRoot(ctx, root)
	require.NoError(t, err)
	require.Equal(t, root, mainRoot)

	sub := filepath.Join(root, "subdir")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	mainRoot, err = mainCheckoutRoot(ctx, sub)
	require.NoError(t, err)
	require.Equal(t, root, mainRoot)

	wtPath := filepath.Join(filepath.Dir(root), "wt")
	wtTestGitRun(t, root, "worktree", "add", "-q", wtPath, "-b", "feature")

	entries, err := listGitWorktrees(ctx, root)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, root, entries[0].Path)
	require.False(t, entries[0].Detached)
	require.True(t, sameWorktreePath(wtPath, entries[1].Path))
	require.Equal(t, "feature", entries[1].Branch)
}

// TestMainCheckoutRootFromLinkedWorktree verifies that the main checkout
// is found even when rush itself runs inside a linked worktree.
func TestMainCheckoutRootFromLinkedWorktree(t *testing.T) {
	// Revert-check: resolve the main root from GIT_DIR instead of the
	// first worktree record and linked-worktree runs lose the shared
	// worktrees directory.
	root := makeWorktreeTestRepo(t)
	wtPath := filepath.Join(filepath.Dir(root), "wt")
	wtTestGitRun(t, root, "worktree", "add", "-q", wtPath, "-b", "feature")

	got, err := mainCheckoutRoot(context.Background(), wtPath)
	require.NoError(t, err)
	require.Equal(t, root, got)
}

// TestIsGitIgnored covers the exit-code mapping of check-ignore plus the
// not-a-repository error path.
func TestIsGitIgnored(t *testing.T) {
	// Revert-check: treat exit 1 as an error and every unignored path
	// makes worktree planning fail; skip the strip of GIT_DIR and a
	// hook-exported environment redirects the check to another repo.
	root := makeWorktreeTestRepo(t)
	ctx := context.Background()

	ignored, err := isGitIgnored(ctx, root, filepath.Join(root, worktreeDirName, "x"))
	require.NoError(t, err)
	require.False(t, ignored)

	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"),
		[]byte(worktreeDirName+"/\n"), 0o644))
	ignored, err = isGitIgnored(ctx, root, filepath.Join(root, worktreeDirName, "x"))
	require.NoError(t, err)
	require.True(t, ignored)

	_, err = isGitIgnored(ctx, t.TempDir(), filepath.Join(root, "x"))
	require.Error(t, err)
}

// TestLocalBranchExistsAndRemoveBranch covers the show-ref exit codes
// and the -d/-D branch removal distinction.
func TestLocalBranchExistsAndRemoveBranch(t *testing.T) {
	// Revert-check: swap the -d/-D flags in removeGitBranch and
	// unmerged branches are deleted without the force consent.
	root := makeWorktreeTestRepo(t)
	ctx := context.Background()
	wtTestGitRun(t, root, "branch", "feature") // Points at main HEAD, merged.

	exists, err := localBranchExists(ctx, root, "feature")
	require.NoError(t, err)
	require.True(t, exists)

	exists, err = localBranchExists(ctx, root, "missing")
	require.NoError(t, err)
	require.False(t, exists)

	require.NoError(t, removeGitBranch(ctx, root, "feature", false))

	// An unmerged commit refuses a plain delete but yields to force.
	wtTestGitRun(t, root, "branch", "feature")
	wtTestGitRun(t, root, "checkout", "-q", "feature")
	require.NoError(t, os.WriteFile(filepath.Join(root, "unmerged.txt"), []byte("x\n"), 0o644))
	wtTestGitRun(t, root, "add", ".")
	wtTestGitRun(t, root, "commit", "-q", "-m", "unmerged")
	defBranch := wtTestGitRun(t, root, "symbolic-ref", "--short", "HEAD")
	wtTestGitRun(t, root, "checkout", "-q", defBranch)
	require.Error(t, removeGitBranch(ctx, root, "feature", false))
	require.NoError(t, removeGitBranch(ctx, root, "feature", true))
	exists, err = localBranchExists(ctx, root, "feature")
	require.NoError(t, err)
	require.False(t, exists)
}

// TestRemoveGitWorktree covers the happy path, the registry update and
// the honest failure when git has nothing left to remove.
func TestRemoveGitWorktree(t *testing.T) {
	// Revert-check: drop the still-exists guard in removeGitWorktree
	// and a locked directory is reported as removed while it remains.
	root := makeWorktreeTestRepo(t)
	wtPath := filepath.Join(filepath.Dir(root), "wt")
	wtTestGitRun(t, root, "worktree", "add", "-q", wtPath, "-b", "feature")
	ctx := context.Background()
	logf := func(string, ...any) {}

	require.NoError(t, removeGitWorktree(ctx, root, wtPath, false, logf))
	_, err := os.Stat(wtPath)
	require.True(t, os.IsNotExist(err))
	entries, err := listGitWorktrees(ctx, root)
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, sameWorktreePath(entry.Path, wtPath))
	}

	// Removing an already-removed path must fail honestly, not spin.
	err = removeGitWorktree(ctx, root, wtPath, false, logf)
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "worktree")
}

// TestRemoveGitWorktree_RespectsForce pins the force gating: a dirty
// worktree must not be deleted without consent.
func TestRemoveGitWorktree_RespectsForce(t *testing.T) {
	// Revert-check: re-enable the RemoveAll fallback for force=false and
	// a git-refused dirty worktree is deleted without consent.
	root := makeWorktreeTestRepo(t)
	ctx := context.Background()
	logf := func(string, ...any) {}
	wtPath := filepath.Join(filepath.Dir(root), "wt-dirty")
	wtTestGitRun(t, root, "worktree", "add", "-q", wtPath, "-b", "feature")
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "untracked.txt"), []byte("x\n"), 0o644))

	// force=false: git refuses; no prune, no filesystem touch.
	err := removeGitWorktree(ctx, root, wtPath, false, logf)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--force")
	_, statErr := os.Stat(wtPath)
	require.NoError(t, statErr, "dirty worktree directory must still exist")
	entries, err := listGitWorktrees(ctx, root)
	require.NoError(t, err)
	listed := false
	for _, entry := range entries {
		if sameWorktreePath(entry.Path, wtPath) {
			listed = true
		}
	}
	require.True(t, listed, "registration must still be listed in git worktree list")

	// force=true: succeeds and the directory is gone.
	require.NoError(t, removeGitWorktree(ctx, root, wtPath, true, logf))
	_, statErr = os.Stat(wtPath)
	require.True(t, os.IsNotExist(statErr))
}
