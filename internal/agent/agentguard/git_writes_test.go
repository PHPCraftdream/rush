package agentguard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckGitWrites(t *testing.T) {
	t.Parallel()

	blocked := []string{
		"git checkout -- file.txt",
		"git checkout main",
		"git restore --staged .",
		"git reset --hard HEAD~1",
		"git stash",
		"git stash pop",
		"git commit -m x",
		"git add .",
		"git rm -rf src",
		"git mv a b",
		"git rebase main",
		"git merge feature",
		"git cherry-pick abc123",
		"git pull",
		"git push origin main",
		"git worktree add ../other",
		"git clean -fd",
		"git apply patch.diff",
		"git am 0001-x.mbox",
		"git branch -d feature",
		"git branch -D feature",
		"git branch --delete feature",
		"env git add .",
		"git -C ../elsewhere checkout main",
		"git -c user.email=x@y checkout main",
		"ls && git reset --hard",
		"bash -c \"git reset --hard\"",
	}
	for _, command := range blocked {
		t.Run("blocked/"+command, func(t *testing.T) {
			t.Parallel()
			err := CheckGitWrites(command)
			require.Error(t, err, command)
			var gitErr *GitWriteError
			require.ErrorAs(t, err, &gitErr)
			require.Contains(t, err.Error(), "git writes are not allowed for this run; the orchestrator commits")
		})
	}

	allowed := []string{
		"git status",
		"git diff",
		"git log --oneline -5",
		"git show HEAD",
		"git rev-parse HEAD",
		"git ls-files",
		"git blame file.go",
		"git grep pattern",
		"git branch",
		"git branch feature",
		"git branch --list x",
		"git -C .. status",
		"echo hello",
		"git status && git diff",
	}
	for _, command := range allowed {
		t.Run("allowed/"+command, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, CheckGitWrites(command), command)
		})
	}
}

// TestBashToolFixture makes a directory tree that looks exactly like a
// linked worktree from git's point of view: a `.git` FILE pointing into
// the main repo's worktrees registry.
func makeLinkedWorktree(t *testing.T) string {
	t.Helper()
	mainRepo := t.TempDir()
	worktree := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	gitFile := filepath.Join(worktree, ".git")
	gitdir := filepath.Join(mainRepo, ".git", "worktrees", "wt")
	require.NoError(t, os.WriteFile(gitFile, []byte("gitdir: "+gitdir+"\n"), 0o644))
	return worktree
}

func TestIsLinkedWorktree(t *testing.T) {
	t.Parallel()

	worktree := makeLinkedWorktree(t)
	require.True(t, IsLinkedWorktree(worktree))
	// Subdirectories of the worktree are still inside the run root.
	sub := filepath.Join(worktree, "internal", "agent")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.True(t, IsLinkedWorktree(sub))

	// A main checkout: .git is a directory.
	mainRepo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(mainRepo, ".git"), 0o755))
	require.False(t, IsLinkedWorktree(mainRepo))

	// Not a repository at all.
	require.False(t, IsLinkedWorktree(t.TempDir()))
}

// TestCheckGitWritesArgs pins the argv counterpart of CheckGitWrites:
// same denylist and same shell-runner/wrapper recursion, but the head is
// resolved on the already-split arguments so no token boundary is lost.
func TestCheckGitWritesArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		argv []string
		sub  string // expected GitWriteError.Subcommand, "" = must pass
	}{
		{
			name: "checkout is a write",
			argv: []string{"git", "checkout", "--", "f"},
			sub:  "checkout",
		},
		{
			name: "status is read-only",
			argv: []string{"git", "status", "--short"},
		},
		{
			name: "global option before subcommand",
			argv: []string{"git", "-C", "some/dir", "reset", "--hard"},
			sub:  "reset",
		},
		{
			name: "branch -D deletes",
			argv: []string{"git", "branch", "-D", "x"},
			sub:  "branch",
		},
		{
			name: "branch create is read-only",
			argv: []string{"git", "branch", "x"},
		},
		{
			name: "shell runner recursion",
			argv: []string{"bash", "-c", "git reset --hard"},
			sub:  "reset",
		},
		{
			name: "wrapper recursion",
			argv: []string{"iex", "git checkout -- f"},
			sub:  "checkout",
		},
		{
			name: "empty argv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := CheckGitWritesArgs(tt.argv)
			if tt.sub == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			var gitErr *GitWriteError
			require.ErrorAs(t, err, &gitErr)
			require.Equal(t, tt.sub, gitErr.Subcommand)
			require.Contains(t, err.Error(), "git writes are not allowed for this run; the orchestrator commits")
			require.Contains(t, err.Error(), tt.sub)
		})
	}
}
