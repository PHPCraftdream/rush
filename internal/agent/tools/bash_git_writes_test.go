package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// linkedWorktreeFixture creates a directory that looks like a linked git
// worktree (a `.git` FILE pointing into a worktrees registry) — the run
// root shape where the A17 git-write guard is active.
func linkedWorktreeFixture(t *testing.T) string {
	t.Helper()
	mainRepo := t.TempDir()
	worktree := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	gitdir := filepath.Join(mainRepo, ".git", "worktrees", "wt")
	require.NoError(t, os.WriteFile(
		filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+gitdir+"\n"), 0o644,
	))
	return worktree
}

// TestBashTool_GitWritesBlockedInLinkedWorktree pins A17: in a run rooted
// in a linked worktree the bash tool refuses git state-mutating commands
// with the orchestrator-commits message BEFORE starting any shell (no
// background shell may appear), while read-only git passes through to
// execution. Outside a linked worktree the guard must not fire at all —
// `git add .` reaches the (denying) permission flow instead.
func TestBashTool_GitWritesBlockedInLinkedWorktree(t *testing.T) {
	worktree := linkedWorktreeFixture(t)

	tool, perms := newBashToolWithRecordingPerms(worktree, false)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "gw-session")

	before := len(testBackgroundManager.List())
	response := runBashTool(t, tool, ctx, BashParams{Command: "git checkout -- file.txt"})
	require.Contains(t, response.Content, "git writes are not allowed for this run; the orchestrator commits")
	require.Len(t, testBackgroundManager.List(), before, "blocked command must not start a shell")
	require.Zero(t, perms.requestCount, "refusal must happen before the permission flow")

	// Read-only git is allowed: no orchestrator-commits refusal.
	roResponse := runBashTool(t, tool, ctx, BashParams{Command: "git status --short"})
	require.NotContains(t, roResponse.Content, "git writes are not allowed")

	// Same command in a NON-worktree directory: guard silent, permission
	// flow decides as before (existing scenarios unchanged).
	plainTool, plainPerms := newBashToolWithRecordingPerms(t.TempDir(), false)
	plainResponse := runBashTool(t, plainTool, ctx, BashParams{Command: "git checkout -- file.txt"})
	require.NotContains(t, plainResponse.Content, "git writes are not allowed")
	require.Equal(t, 1, plainPerms.requestCount)
}
