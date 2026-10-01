package cliprovider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// linkedMCPWorktree makes a directory that looks like a linked git worktree
// (a `.git` FILE pointing into a worktrees registry) — the run-root shape
// where the A17 git-write guard is active. Local copy of the tools-package
// fixture so this package needs no cross-package import.
func linkedMCPWorktree(t *testing.T) string {
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

func firstMCPText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, result.Content, 1)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected text content, got %T", result.Content[0])
	return text.Text
}

// TestMCPBash_GitWritesBlockedInLinkedWorktree pins A17 for the MCP bash
// surface: with the server rooted in a linked worktree, a state-mutating git
// command is refused before the permission flow runs, while read-only git
// still reaches the shell (and a main-checkout run is unaffected).
func TestMCPBash_GitWritesBlockedInLinkedWorktree(t *testing.T) {
	worktree := linkedMCPWorktree(t)

	base := newMCPTestPermissionService(t, worktree)
	perms := &recordingMCPPermissionService{Service: base}
	perms.AutoApproveSession("owner-session")
	client := connectMCPTestClient(t, newMCPTestServer(t, perms, "owner-session", worktree, nil))

	blocked := callMCPTool(t, client, "Bash", mcpBashInput{
		Command:     "git checkout -- f",
		Description: "mutate git state",
	})
	require.True(t, blocked.IsError)
	require.Contains(t, firstMCPText(t, blocked),
		"git writes are not allowed for this run; the orchestrator commits")
	require.Empty(t, perms.Requests(),
		"refusal must happen before the permission flow")

	// Read-only git passes the guard (it then fails normally in the
	// throwaway worktree, which has no real repository behind it).
	readOnly := callMCPTool(t, client, "Bash", mcpBashInput{
		Command:     "git status --short",
		Description: "read git state",
	})
	require.NotContains(t, firstMCPText(t, readOnly), "git writes are not allowed")

	// Main-checkout run: guard silent, the permission flow still decides.
	plain := t.TempDir()
	plainPerms := &recordingMCPPermissionService{Service: newMCPTestPermissionService(t, plain)}
	plainPerms.AutoApproveSession("owner-session")
	plainClient := connectMCPTestClient(t, newMCPTestServer(t, plainPerms, "owner-session", plain, nil))
	allowed := callMCPTool(t, plainClient, "Bash", mcpBashInput{
		Command:     "git checkout -- f",
		Description: "mutate git state outside a worktree",
	})
	require.NotContains(t, firstMCPText(t, allowed), "git writes are not allowed")
	require.Len(t, plainPerms.Requests(), 1)
}
