package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// newRunCommandToolWithRecordingPerms builds the run_command tool with a
// recording permission service so a test can assert how many permission
// requests a call raised. Local copy on purpose: the bash helper lives in
// bash_test.go and takes bash-specific arguments.
func newRunCommandToolWithRecordingPerms(t *testing.T, workingDir string) (fantasy.AgentTool, *recordingPermissionService) {
	t.Helper()
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
	}
	return NewRunCommandTool(perms, workingDir), perms
}

// TestRunCommandTool_GitWritesBlockedInLinkedWorktree pins A17 for the
// no-shell program runner: a git state-mutating call is refused when the
// run is rooted in a linked worktree, BEFORE any permission request is
// raised; read-only git passes; and the guard keys on the resolved exec
// directory (params.WorkingDir), not on the tool's root.
func TestRunCommandTool_GitWritesBlockedInLinkedWorktree(t *testing.T) {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "gw-run-session")

	// Run rooted in a linked worktree: write refused before permission.
	worktree := linkedWorktreeFixture(t)
	tool, perms := newRunCommandToolWithRecordingPerms(t, worktree)

	resp := runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program: "git",
		Args:    []string{"checkout", "--", "file.txt"},
	})
	require.True(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content,
		"git writes are not allowed for this run; the orchestrator commits")
	require.Zero(t, perms.requestCount, "refusal must happen before the permission flow")

	// Read-only git is allowed: no orchestrator-commits refusal.
	roResp := runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program: "git",
		Args:    []string{"status", "--short"},
	})
	require.NotContains(t, roResp.Content, "git writes are not allowed")

	// Guard keys on the RESOLVED exec directory, not the tool root: the
	// tool is rooted outside the worktree, the call runs inside it.
	base := t.TempDir()
	sub := filepath.Join(base, "nested", "wt")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	gitdir := filepath.Join(base, "main", ".git", "worktrees", "wt")
	require.NoError(t, os.WriteFile(
		filepath.Join(sub, ".git"),
		[]byte("gitdir: "+gitdir+"\n"), 0o644,
	))
	nestedTool, nestedPerms := newRunCommandToolWithRecordingPerms(t, base)
	nestedResp := runRunCommandTool(t, nestedTool, ctx, RunCommandParams{
		Program:    "git",
		Args:       []string{"checkout", "--", "file.txt"},
		WorkingDir: filepath.Join("nested", "wt"),
	})
	require.True(t, nestedResp.IsError, nestedResp.Content)
	require.Contains(t, nestedResp.Content,
		"git writes are not allowed for this run; the orchestrator commits")
	require.Zero(t, nestedPerms.requestCount,
		"refusal must happen before the permission flow")

	// Main-checkout run: guard silent, the permission flow still runs.
	plainTool, plainPerms := newRunCommandToolWithRecordingPerms(t, t.TempDir())
	plainResp := runRunCommandTool(t, plainTool, ctx, RunCommandParams{
		Program: "git",
		Args:    []string{"checkout", "--", "f"},
	})
	require.NotContains(t, plainResp.Content, "git writes are not allowed")
	require.Equal(t, 1, plainPerms.requestCount)
}
