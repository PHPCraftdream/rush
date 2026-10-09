//go:build windows

package cahgen

import (
	"os/exec"
	"strings"
	"syscall"
)

// Configure cmd's special quoting for npm.cmd paths containing spaces.
func configureRunnerCommand(cmd *exec.Cmd, executable string, args []string) {
	if executable != "cmd.exe" {
		return
	}
	var words []string
	for _, arg := range args[3:] {
		words = append(words, syscall.EscapeArg(arg))
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Set only CmdLine: replacing the struct would drop the hidden-console flags.
	cmd.SysProcAttr.CmdLine = `cmd.exe /d /s /c "` + strings.Join(words, " ") + `"`
}
