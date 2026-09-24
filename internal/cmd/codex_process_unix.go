//go:build !windows

package cmd

import (
	"os/exec"
	"syscall"
)

func configureCodexChild(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
