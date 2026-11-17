//go:build windows

package cliproc

import (
	"testing"

	"golang.org/x/sys/windows"
)

// Moved from internal/serve/cmd (lane K1): pokes the unexported console
// event classifier in console_ctrl_windows.go.
func TestInstallConsoleProcessStop(t *testing.T) {
	uninstall := installConsoleProcessStop(func(int) { t.Error("unexpected console stop during installation") })
	if uninstall == nil {
		t.Fatal("console handler must return an uninstall function")
	}
	uninstall()
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
