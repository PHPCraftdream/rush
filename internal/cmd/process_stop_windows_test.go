//go:build windows

package cmd

import (
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"golang.org/x/sys/windows"
)

func configureStopParent(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
}

func configureStopLeaf(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}
func trackStopLeaf(pid int) error { return session.TrackProcessTree(pid) }
func triggerOwnStopConsole() error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0)
}

func processStopScenarios() []processStopScenario {
	return []processStopScenario{{name: "console-break", mode: "console", code: 130, send: func(_ *exec.Cmd, stdin io.Writer) error {
		_, err := fmt.Fprintln(stdin, "break")
		return err
	}}}
}

func TestProcessStopWindowsConsoleEvents(t *testing.T) {
	for _, event := range []uint32{windows.CTRL_C_EVENT, windows.CTRL_BREAK_EVENT, windows.CTRL_CLOSE_EVENT, windows.CTRL_LOGOFF_EVENT, windows.CTRL_SHUTDOWN_EVENT} {
		if code, ok := consoleProcessStopCode(event); !ok || code != 130 {
			t.Errorf("event %d: code=%d handled=%v", event, code, ok)
		}
	}
	if _, ok := consoleProcessStopCode(999); ok {
		t.Fatal("unknown console event was handled")
	}
}
