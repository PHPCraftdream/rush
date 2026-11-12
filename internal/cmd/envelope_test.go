// Fork-only tests for envelope resolution and loading (envelope.go).
// Revert-check comments state which product change each test catches.

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMainRootFromCommonDirOutput pins the git-common-dir -> main-root
// mapping that session-id lookup depends on. Revert-check: breaking
// base-.git detection breaks `<main>/.rush/stdin/<id>.out` resolution
// from a linked worktree.
func TestMainRootFromCommonDirOutput(t *testing.T) {
	t.Parallel()
	t.Run("main checkout relative dot git", func(t *testing.T) {
		t.Parallel()
		root, err := mainRootFromCommonDirOutput("D:/tmp/repo", ".git\n")
		require.NoError(t, err)
		assert.Equal(t, filepath.Clean("D:/tmp/repo"), root)
	})
	t.Run("linked worktree absolute", func(t *testing.T) {
		t.Parallel()
		main := t.TempDir()
		// Build a native-absolute "<main>/.git" like a linked worktree
		// sees, so the case is portable across platforms.
		root, err := mainRootFromCommonDirOutput("D:/wt", filepath.Join(main, ".git")+"\n")
		require.NoError(t, err)
		assert.Equal(t, filepath.Clean(main), root)
	})
	t.Run("relative subpath", func(t *testing.T) {
		t.Parallel()
		root, err := mainRootFromCommonDirOutput("D:/tmp/repo", "sub/.git\n")
		require.NoError(t, err)
		assert.Equal(t, filepath.Clean("D:/tmp/repo/sub"), root)
	})
	t.Run("not a dot git dir", func(t *testing.T) {
		t.Parallel()
		_, err := mainRootFromCommonDirOutput("D:/tmp", "D:/tmp/somewhere\n")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot derive the main checkout root")
	})
	t.Run("empty output", func(t *testing.T) {
		t.Parallel()
		_, err := mainRootFromCommonDirOutput("D:/tmp", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty git rev-parse output")
	})
}

// Revert-check: a direct file path must resolve to itself, and
// non-ASCII content must not interfere with resolution.
func TestResolveEnvelopePathDirectFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "env.json")
	err := os.WriteFile(path, []byte(`{"final_text":"Готово: всё проверено"}`), 0o644)
	require.NoError(t, err)
	got, err := resolveEnvelopePath(path)
	require.NoError(t, err)
	assert.Equal(t, path, got)
}

// Revert-check: a missing session id must fail loudly, telling the
// user both that the main checkout could not be located and that a
// direct path works — not fall back to some wrong default.
func TestResolveEnvelopePathMissingEverything(t *testing.T) {
	// t.Chdir is sequential-only; do not parallelise this test.
	tmp := t.TempDir()
	t.Chdir(tmp)
	// Not a git repo, so the envelopeMainCheckoutRoot probe fails here.
	_, err := resolveEnvelopePath("nope-123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no envelope for session")
}

// Revert-check: catches regressions in git-common-dir -> main-root
// resolution, i.e. `rush result s1` failing from a linked worktree.
func TestResolveEnvelopePathSessionID(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init", "-q")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644))
	run("add", "f.txt")
	run("-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-qm", "init")
	run("worktree", "add", "-q", "wt", "-b", "side")
	wt := filepath.Join(repo, "wt")

	stdinDir := filepath.Join(repo, ".rush", "stdin")
	require.NoError(t, os.MkdirAll(stdinDir, 0o755))
	envPath := filepath.Join(stdinDir, "s1-review.out")
	require.NoError(t, os.WriteFile(envPath,
		[]byte(`{"session_id":"s1-review","final_text":"Готово"}`), 0o644))

	t.Chdir(wt)
	got, err := resolveEnvelopePath("s1-review")
	require.NoError(t, err)
	want, err := filepath.Abs(envPath)
	require.NoError(t, err)
	gotEval, gotErr := filepath.EvalSymlinks(got)
	require.NoError(t, gotErr)
	wantEval, wantErr := filepath.EvalSymlinks(want)
	require.NoError(t, wantErr)
	if runtime.GOOS == "darwin" {
		// EvalSymlinks resolves the /private prefix on macOS, but does
		// not make the two strings agree when /var is itself a link;
		// accept either representation.
		assert.True(t, gotEval == wantEval || got == want,
			"got %s (%s), want %s (%s)", got, gotEval, want, wantEval)
		return
	}
	assert.Equal(t, wantEval, gotEval)
}
