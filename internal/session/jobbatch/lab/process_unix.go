//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package lab

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

type processTree struct{ pid int }

func processSupported() bool { return true }
func processPrepare(cmd *exec.Cmd) (*processTree, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return &processTree{}, nil
}
func (t *processTree) attach(cmd *exec.Cmd) error { t.pid = cmd.Process.Pid; return nil }
func (t *processTree) signal(signal syscall.Signal) error {
	err := syscall.Kill(-t.pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
func (t *processTree) term() error { return t.signal(syscall.SIGTERM) }
func (t *processTree) kill() error { return t.signal(syscall.SIGKILL) }
func (t *processTree) awaitGone() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if errors.Is(syscall.Kill(-t.pid, 0), syscall.ESRCH) {
			return
		}
		<-ticker.C
	}
}

func processLeaderExited(cmd *exec.Cmd) bool {
	return errors.Is(syscall.Kill(cmd.Process.Pid, 0), syscall.ESRCH)
}

func (t *processTree) dispose() error { return nil }
