//go:build !windows

package session

import (
	"os/exec"
	"syscall"
)

func configureLateStopChild(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
func registerLateStopChild(pid int) error { return RegisterSpawnedGroup(pid) }
