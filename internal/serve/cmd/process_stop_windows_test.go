//go:build windows

package cmd

import (
	"fmt"
	"io"
	"os/exec"

	"github.com/PHPCraftdream/rush/internal/kit/sys/proctrack"
	"golang.org/x/sys/windows"
)

func configureStopParent(command *exec.Cmd) {
	// Реальному console event нужна своя консоль; окно остаётся скрытым.
	command.SysProcAttr.CreationFlags &^= windows.CREATE_NO_WINDOW
	command.SysProcAttr.CreationFlags |= windows.CREATE_NEW_CONSOLE
}

func configureStopLeaf(command *exec.Cmd) {
	command.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}
func trackStopLeaf(pid int) error { return proctrack.TrackProcessTree(pid) }
func triggerOwnStopConsole() error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0)
}

func processStopScenarios() []processStopScenario {
	return []processStopScenario{{name: "console-break", mode: "console", code: 130, send: func(_ *exec.Cmd, stdin io.Writer) error {
		_, err := fmt.Fprintln(stdin, "break")
		return err
	}}}
}
