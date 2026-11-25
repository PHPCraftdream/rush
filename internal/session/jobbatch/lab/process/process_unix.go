//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package process

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

type ProcessTree struct{ pid int }

func ProcessSupported() bool { return true }
func ProcessPrepare(cmd *exec.Cmd) (*ProcessTree, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return &ProcessTree{}, nil
}
func (t *ProcessTree) attach(cmd *exec.Cmd) error { t.pid = cmd.Process.Pid; return nil }
func (t *ProcessTree) signal(signal syscall.Signal) error {
	err := syscall.Kill(-t.pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
func (t *ProcessTree) term() error { return t.signal(syscall.SIGTERM) }
func (t *ProcessTree) kill() error { return t.signal(syscall.SIGKILL) }
func (t *ProcessTree) awaitGone() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if errors.Is(syscall.Kill(-t.pid, 0), syscall.ESRCH) {
			return
		}
		<-ticker.C
	}
}

func ProcessLeaderExited(cmd *exec.Cmd) bool {
	return errors.Is(syscall.Kill(cmd.Process.Pid, 0), syscall.ESRCH)
}

func (t *ProcessTree) dispose() error { return nil }
