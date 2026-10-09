//go:build windows

package cahgen

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/PHPCraftdream/rush/internal/platform"
)

// Revert-check: cmd.exe preserves a spaced npm shim path as one command.
func TestRunnerWindowsQuoting(t *testing.T) {
	cmd := platform.Command(context.Background(), "cmd.exe")
	configureRunnerCommand(cmd, "cmd.exe", []string{"/d", "/s", "/c", "C:/Program Files/nodejs/npm.cmd", "pack", "--ignore-scripts"})
	require.Equal(t, `cmd.exe /d /s /c ""C:/Program Files/nodejs/npm.cmd" pack --ignore-scripts"`, cmd.SysProcAttr.CmdLine)
	direct := platform.Command(context.Background(), "node")
	configureRunnerCommand(direct, "node", []string{"script.js"})
	require.Empty(t, direct.SysProcAttr.CmdLine)
}

// Revert-check: assigning a fresh SysProcAttr{CmdLine} in configureRunnerCommand drops the flags.
func TestRunnerWindowsKeepsHiddenConsole(t *testing.T) {
	cmd := platform.Command(context.Background(), "cmd.exe")
	configureRunnerCommand(cmd, "cmd.exe", []string{"/d", "/s", "/c", "npm.cmd", "pack"})
	require.NotZero(t, cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW)
	require.True(t, cmd.SysProcAttr.HideWindow)
}
