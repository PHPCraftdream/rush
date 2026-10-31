package agentguard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckGitWritesNewlineSeparators(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{name: "LF separates commands", command: "git status\ngit replace HEAD HEAD~1", blocked: true},
		{name: "CRLF separates commands", command: "git status\r\ngit replace HEAD HEAD~1", blocked: true},
		{name: "bare CR does not split commands", command: "git status\rgit replace HEAD HEAD~1", blocked: false},
		{name: "newline inside quotes does not split", command: "echo 'git status\ngit revert HEAD'", blocked: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := CheckGitWrites(tt.command)
			if tt.blocked {
				require.Error(t, err)
				var gitErr *GitWriteError
				require.ErrorAs(t, err, &gitErr)
				require.Equal(t, "replace", gitErr.Subcommand)
				return
			}
			require.NoError(t, err)
		})
	}
}

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
		"git pack-refs --all",
		"git filter-branch --tree-filter true HEAD",
		"git fast-import",
		"git replace HEAD HEAD~1",
		"git update-ref refs/heads/main HEAD",
		"git symbolic-ref HEAD refs/heads/main",
		"git tag -d release-1",
		"git tag --delete release-1",
		"git tag -f release-1",
		"git tag --force release-1",
		"git branch -m old new",
		"git branch -M old new",
		"git branch --move old new",
		"git branch -c old new",
		"git branch -C old new",
		"git branch --copy old new",
		"git branch -f main HEAD",
		"git branch --force main HEAD",
		"git branch -d feature",
		"git branch -D feature",
		"git branch --delete feature",
		"env git add .",
		"git -C ../elsewhere checkout main",
		"git -c user.email=x@y checkout main",
		"ls && git reset --hard",
		"bash -c \"git reset --hard\"",
		"cd sub\ngit reset --hard",
		"git status\ngit reset --hard",
		"git status\r\ngit reset --hard",
		"git -C dir reset --hard",
		"git -c k=v reset --hard",
		"git --git-dir=repo/.git reset --hard",
		"git --work-tree=repo reset --hard",
		"git --no-pager reset --hard",
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
		"git tag v1",
		"git tag",
		"git log",
		"git revert HEAD",
		"git fetch origin",
		"git notes add -m note HEAD",
		"git branch --edit-description main",
		"git -C .. status",
		"git --git-dir=repo/.git --work-tree=repo status",
		"echo hello",
		"git status && git diff",
		"git status\ngit diff",
		"git status\r\ngit diff",
		"echo 'not a command\ngit status'",
		"echo ok\ngit status",
		"echo 'hello\nworld'",
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
			name: "pack-refs writer",
			argv: []string{"git", "pack-refs", "--all"},
			sub:  "pack-refs",
		},
		{
			name: "filter-branch writer",
			argv: []string{"git", "filter-branch", "HEAD"},
			sub:  "filter-branch",
		},
		{
			name: "fast-import writer",
			argv: []string{"git", "fast-import"},
			sub:  "fast-import",
		},
		{
			name: "replace writer",
			argv: []string{"git", "replace", "HEAD", "HEAD~1"},
			sub:  "replace",
		},
		{
			name: "update-ref writer",
			argv: []string{"git", "update-ref", "refs/heads/main", "HEAD"},
			sub:  "update-ref",
		},
		{
			name: "symbolic-ref writer",
			argv: []string{"git", "symbolic-ref", "HEAD", "refs/heads/main"},
			sub:  "symbolic-ref",
		},
		{
			name: "branch move flag",
			argv: []string{"git", "branch", "-m", "old", "new"},
			sub:  "branch",
		},
		{
			name: "tag plain create is allowed",
			argv: []string{"git", "tag", "v1"},
		},
		{
			name: "tag delete",
			argv: []string{"git", "tag", "-d", "v1"},
			sub:  "tag",
		},
		{
			name: "tag force delete",
			argv: []string{"git", "tag", "--delete", "v1"},
			sub:  "tag",
		},
		{
			name: "tag force create",
			argv: []string{"git", "tag", "-f", "v1"},
			sub:  "tag",
		},
		{
			name: "tag long force create",
			argv: []string{"git", "tag", "--force", "v1"},
			sub:  "tag",
		},
		{
			name: "branch move uppercase flag",
			argv: []string{"git", "branch", "-M", "old", "new"},
			sub:  "branch",
		},
		{
			name: "branch --move flag",
			argv: []string{"git", "branch", "--move", "old", "new"},
			sub:  "branch",
		},
		{
			name: "branch copy uppercase flag",
			argv: []string{"git", "branch", "-C", "old", "new"},
			sub:  "branch",
		},
		{
			name: "branch --copy flag",
			argv: []string{"git", "branch", "--copy", "old", "new"},
			sub:  "branch",
		},
		{
			name: "branch force flag",
			argv: []string{"git", "branch", "-f", "main", "HEAD"},
			sub:  "branch",
		},
		{
			name: "branch force long flag",
			argv: []string{"git", "branch", "--force", "main", "HEAD"},
			sub:  "branch",
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
