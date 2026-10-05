// WorkspaceRoot fallback tests: walking up the directory tree for a
// .git marker (directory or file) when the git binary cannot launch,
// git's own answer winning over the walk, and launch failures not
// being cached while genuine git failures stay cached.

package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

// Revert-check: removing WorkspaceRoot's .git-walk fallback makes this
// return "" (git unavailable, launch failure uncached).
func TestWorkspaceRoot_WalkFindsDotGitDirWithoutGit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub", "dir"), 0o755))

	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	dir := filepath.Join(root, "sub", "dir")
	got := WorkspaceRoot(dir)

	want, _ := filepath.Abs(root)
	require.Equal(t, want, got)

	dirAbs, _ := filepath.Abs(dir)
	require.NotEqual(t, dirAbs, got)

	t.Cleanup(func() { worktreeRootCache.Delete(dir) })
}

// Revert-check: removing WorkspaceRoot's .git-walk fallback makes this
// return "" (git unavailable, launch failure uncached).
func TestWorkspaceRoot_WalkFindsDotGitFileWithoutGit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: elsewhere\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub", "dir"), 0o755))

	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	dir := filepath.Join(root, "sub", "dir")
	got := WorkspaceRoot(dir)

	want, _ := filepath.Abs(root)
	require.Equal(t, want, got)

	dirAbs, _ := filepath.Abs(dir)
	require.NotEqual(t, dirAbs, got)

	t.Cleanup(func() { worktreeRootCache.Delete(dir) })
}

// Unchanged-behaviour guard: no .git marker anywhere and git cannot
// launch, so WorkspaceRoot must still return "".
func TestWorkspaceRoot_NoDotGitAndNoGitReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub", "dir"), 0o755))

	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	require.Equal(t, "", WorkspaceRoot(filepath.Join(root, "sub", "dir")))

	t.Cleanup(func() { worktreeRootCache.Delete(filepath.Join(root, "sub", "dir")) })
}

// Git must win over the naive .git-walk: root/sub contains an invalid
// empty .git marker dir, but git discovery reports root itself.
func TestWorkspaceRoot_GitStillWinsInRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	gitInit := platform.Command(t.Context(), "git", "init", "-q")
	gitInit.Dir = root
	require.NoError(t, gitInit.Run())
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub", ".git"), 0o755))

	dir := filepath.Join(root, "sub")

	rev := platform.Command(t.Context(), "git", "rev-parse", "--show-toplevel")
	rev.Dir = dir
	out, err := rev.Output()
	require.NoError(t, err)
	want, _ := filepath.Abs(strings.TrimSpace(string(out)))

	got := WorkspaceRoot(dir)
	require.Equal(t, want, got)

	walkWrong, _ := filepath.Abs(dir)
	require.NotEqual(t, walkWrong, got)

	t.Cleanup(func() { worktreeRootCache.Delete(dir) })
}

// Revert-check: reverting the launch-failure no-cache rule + walk
// fallback makes step 1 return "" / cached=true and step 2 return the
// stale "".
func TestWorkspaceRoot_LaunchFailureNotCached(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	realPATH := os.Getenv("PATH")

	root := t.TempDir()
	gitInit := platform.Command(t.Context(), "git", "init", "-q")
	gitInit.Dir = root
	require.NoError(t, gitInit.Run())
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))

	dir := filepath.Join(root, "sub")
	wantRoot, _ := filepath.Abs(root)

	// Step 1: without git on PATH the walk answers and nothing is cached.
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)
	got := WorkspaceRoot(dir)
	require.Equal(t, wantRoot, got)
	_, cached := worktreeRootCache.Load(dir)
	require.False(t, cached, "a process-launch failure must not be cached")

	// Step 2: restored PATH, git genuinely resolves the real root.
	t.Setenv("PATH", realPATH)
	require.Equal(t, wantRoot, worktreeRoot(dir))

	// Step 3: a genuine non-zero git exit stays cached.
	plain := t.TempDir()
	require.Equal(t, "", worktreeRoot(plain))
	_, cached = worktreeRootCache.Load(plain)
	require.True(t, cached, "a genuine non-zero git exit must stay cached")

	t.Cleanup(func() {
		worktreeRootCache.Delete(dir)
		worktreeRootCache.Delete(plain)
	})
}
