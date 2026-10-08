//go:build windows

package cahgen

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// Revert-check: cmd.exe preserves a spaced npm shim path as one command.
func TestRunnerWindowsQuoting(t *testing.T) {
	cmd := &exec.Cmd{}
	configureRunnerCommand(cmd, "cmd.exe", []string{"/d", "/s", "/c", "C:/Program Files/nodejs/npm.cmd", "pack", "--ignore-scripts"})
	require.Equal(t, `cmd.exe /d /s /c ""C:/Program Files/nodejs/npm.cmd" pack --ignore-scripts"`, cmd.SysProcAttr.CmdLine)
	direct := &exec.Cmd{}
	configureRunnerCommand(direct, "node", []string{"script.js"})
	require.Nil(t, direct.SysProcAttr)
}
