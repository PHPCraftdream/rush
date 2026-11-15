//go:build !windows

package cmd

import (
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/PHPCraftdream/rush/internal/session"
)

func configureStopParent(*exec.Cmd)       {}
func configureStopLeaf(command *exec.Cmd) { command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func trackStopLeaf(pid int) error         { return session.RegisterSpawnedGroup(pid) }
func triggerOwnStopConsole() error        { return nil }

func processStopScenarios() []processStopScenario {
	return []processStopScenario{
		{name: "interrupt", mode: "signal", code: 130, send: func(c *exec.Cmd, _ io.Writer) error { return c.Process.Signal(os.Interrupt) }},
		{name: "terminate", mode: "signal", code: 143, send: func(c *exec.Cmd, _ io.Writer) error { return c.Process.Signal(syscall.SIGTERM) }},
	}
}
