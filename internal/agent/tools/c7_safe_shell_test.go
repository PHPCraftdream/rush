package tools

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestBashTool_UnsafeReadOnlyLookalikesRequestPermission(t *testing.T) {
	commands := []string{
		"echo > overwritten.txt",
		"echo $(touch created.txt)",
		"VALUE=unsafe echo hi",
		"date --set=tomorrow",
		"date -s tomorrow",
		"hostname NEWNAME",
		"hostname -F hostname.txt",
		"ipconfig /release",
		"ipconfig /renew",
		"ipconfig /flushdns",
		"git branch -d feature",
		"git branch feature",
		"git tag -a release",
		"git tag release",
		"git config --global user.name attacker",
		"git config --unset user.name",
		"git remote add origin https://example.invalid/repo.git",
		"git remote set-url origin https://example.invalid/repo.git",
		"git grep --open-files-in-pager needle",
		"git grep -O needle",
		"git ls-remote origin",
		"git ls-remote helper::origin",
		"git ls-remote -u origin",
		"git ls-remote --upload-pack=evil origin",
		"git remote show origin",
		"git remote show origin -n",
		"git remote -n show origin",
		"git status --unknown-option",
		"git diff --unknown-option",
		"git diff --output=changed.txt",
		"kill -TERM 1",
		"killall agent",
	}

	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			tool, permissions := newBashToolWithRecordingPerms(t.TempDir(), false)
			ctx := context.WithValue(context.Background(), SessionIDContextKey, "unsafe-session")

			response := runBashTool(t, tool, ctx, BashParams{Command: command})

			require.Equal(t, 1, permissions.requestCount)
			require.Contains(t, response.Content, "User denied permission")
		})
	}
}

func TestBashTool_RestrictedRunChecksSafeCommandBeforeShortcut(t *testing.T) {
	workingDir := t.TempDir()
	dbDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dbDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dbDir) })

	permissions := permission.NewPermissionService(t.Context(), workingDir, false, nil, db.New(conn))
	permissions.AutoApproveSession("restricted-session")
	allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
		Restrict:  true,
		AllowBash: []string{"echo"},
	})
	require.NoError(t, err)
	permissions.SetRunAllowlist(allowlist)

	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	tool := NewBashTool(permissions, workingDir, attribution, "test-model", nil, testBackgroundManager)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "restricted-session")

	denied := runBashTool(t, tool, ctx, BashParams{Command: "echo > restricted.txt"})
	require.Contains(t, denied.Content, "User denied permission")

	deniedUnlisted := runBashTool(t, tool, ctx, BashParams{Command: "date"})
	require.Contains(t, deniedUnlisted.Content, "User denied permission")

	allowed := runBashTool(t, tool, ctx, BashParams{Command: "echo allowed"})
	require.False(t, allowed.IsError)
	require.Contains(t, allowed.Content, "allowed")
}

func TestSafeReadOnlyCommandRejectsDynamicShellSyntax(t *testing.T) {
	for _, command := range []string{
		"echo 'literal'",
		"echo \"literal\"",
		"echo $VALUE",
		"echo ${VALUE}",
		"echo $((1 + 1))",
		"ls *.go",
		"ls ?.go",
		"echo hi;",
		"echo hi 2>&1",
	} {
		require.False(t, isSafeReadOnlyCommand(command), command)
	}
}
